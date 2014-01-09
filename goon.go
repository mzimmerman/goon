/*
 * Copyright (c) 2012 Matt Jibson <matt.jibson@gmail.com>
 *
 * Permission to use, copy, modify, and distribute this software for any
 * purpose with or without fee is hereby granted, provided that the above
 * copyright notice and this permission notice appear in all copies.
 *
 * THE SOFTWARE IS PROVIDED "AS IS" AND THE AUTHOR DISCLAIMS ALL WARRANTIES
 * WITH REGARD TO THIS SOFTWARE INCLUDING ALL IMPLIED WARRANTIES OF
 * MERCHANTABILITY AND FITNESS. IN NO EVENT SHALL THE AUTHOR BE LIABLE FOR
 * ANY SPECIAL, DIRECT, INDIRECT, OR CONSEQUENTIAL DAMAGES OR ANY DAMAGES
 * WHATSOEVER RESULTING FROM LOSS OF USE, DATA OR PROFITS, WHETHER IN AN
 * ACTION OF CONTRACT, NEGLIGENCE OR OTHER TORTIOUS ACTION, ARISING OUT OF
 * OR IN CONNECTION WITH THE USE OR PERFORMANCE OF THIS SOFTWARE.
 */

package goon

import (
	"errors"
	"fmt"
	"math/rand"
	"net/http"
	"reflect"
	"time"

	"appengine"
	"appengine/datastore"
	"appengine/memcache"
)

var (
	// LogErrors issues appengine.Context.Errorf on any error.
	LogErrors bool = true
)

// Goon holds the app engine context and request memory cache.
type Goon struct {
	testing       bool // if Goon should simulate leggy responses on RPCs
	context       appengine.Context
	cache         map[string]interface{}
	inTransaction bool
	toSet         map[string]interface{}
	toDelete      []string
}

// Used for testing to simulate laggy responses to RPCs
func (g *Goon) fakeDelay(max time.Duration) {
	if !g.testing { // if we're in production, just move along
		return
	}
	time.Sleep(time.Duration(rand.Int63n(int64(max))))
}

// Returns the internal appengine.Context in use
func (g *Goon) C() appengine.Context {
	return g.context
}

func memkey(k *datastore.Key) string {
	return k.Encode()
}

// NewGoon creates a new Goon object from the given request.
func NewGoon(r *http.Request) *Goon {
	return FromContext(appengine.NewContext(r))
}

// FromContext creates a new Goon object from the given appengine Context.
func FromContext(c appengine.Context) *Goon {
	return &Goon{
		context: c,
		cache:   make(map[string]interface{}),
	}
}

func (g *Goon) error(err error) {
	if LogErrors {
		g.context.Errorf("goon: %v", err.Error())
	}
}

func (g *Goon) extractKeys(src interface{}, allowIncomplete bool) ([]*datastore.Key, error) {
	v := reflect.Indirect(reflect.ValueOf(src))
	if v.Kind() != reflect.Slice {
		return nil, errors.New("goon: value must be a slice or pointer-to-slice")
	}
	l := v.Len()

	keys := make([]*datastore.Key, l)
	for i := 0; i < l; i++ {
		vi := v.Index(i)
		key, err := g.getStructKey(vi.Interface())
		if err != nil {
			return nil, err
		}
		if !allowIncomplete && key.Incomplete() {
			return nil, errors.New("goon: cannot find a key for struct")
		}
		keys[i] = key
	}
	return keys, nil
}

// Key is the same as KeyError, except nil is returned on error.
func (g *Goon) Key(src interface{}) *datastore.Key {
	if k, err := g.KeyError(src); err == nil {
		if !k.Incomplete() {
			return k
		}
	}
	return nil
}

// Key returns the key of src based on its properties.
func (g *Goon) KeyError(src interface{}) (*datastore.Key, error) {
	return g.getStructKey(src)
}

// RunInTransaction runs f in a transaction. It calls f with a transaction
// context tg that f should use for all App Engine operations. Neither cache nor
// memcache are used or set during a transaction.
//
// Otherwise similar to appengine/datastore.RunInTransaction:
// https://developers.google.com/appengine/docs/go/datastore/reference#RunInTransaction
func (g *Goon) RunInTransaction(f func(tg *Goon) error, opts *datastore.TransactionOptions) error {
	var ng *Goon
	err := datastore.RunInTransaction(g.context, func(tc appengine.Context) error {
		ng = &Goon{
			context:       tc,
			inTransaction: true,
			toSet:         make(map[string]interface{}),
		}
		return f(ng)
	}, opts)

	if err == nil {
		for k, v := range ng.toSet {
			g.cache[k] = v
		}

		for _, k := range ng.toDelete {
			delete(g.cache, k)
		}
	} else {
		g.error(err)
	}

	return err
}

// Put saves the entity src into the datastore based on src's key k. If k
// is an incomplete key, the returned key will be a unique key generated by
// the datastore.
func (g *Goon) Put(src interface{}) (*datastore.Key, error) {
	ks, err := g.PutMulti(&[]interface{}{src})
	if len(ks) == 1 {
		return ks[0], err
	}
	return nil, err
}

// PutMany is a wrapper around PutMulti.
func (g *Goon) PutMany(srcs ...interface{}) ([]*datastore.Key, error) {
	return g.PutMulti(srcs)
}

const putMultiLimit = 500

// PutMulti is a batch version of Put.
//
// src must satisfy the same conditions as the dst argument to GetMulti.
func (g *Goon) PutMulti(src interface{}) ([]*datastore.Key, error) {
	item := reflect.ValueOf(src)
	if item.Kind() != reflect.Ptr || item.Elem().Kind() != reflect.Slice {
		return nil, errors.New(fmt.Sprintf("goon: must provide pointer to slice of pointer to struct, supplied - %#v", src))
	}

	keys, err := g.extractKeys(src, true) // allow incompletes on a Put request as the datastore will create the key
	if err != nil {
		return nil, err
	}

	var memkeys []string
	for _, key := range keys {
		if !key.Incomplete() {
			memkeys = append(memkeys, memkey(key))
		}
	}

	// Memcache needs to be updated after the datastore to prevent a common race condition
	defer func() {
		memcache.DeleteMulti(g.context, memkeys)
		g.fakeDelay(time.Millisecond * 2)
	}()
	v := reflect.Indirect(reflect.ValueOf(src))
	for i := 0; i <= len(keys)/putMultiLimit; i++ {
		lo := i * putMultiLimit
		hi := (i + 1) * putMultiLimit
		if hi > len(keys) {
			hi = len(keys)
		}
		rkeys, err := datastore.PutMulti(g.context, keys[lo:hi], v.Slice(lo, hi).Interface())
		g.fakeDelay(time.Millisecond * 15)
		if err != nil {
			g.error(err)
			return nil, err
		}

		for i, key := range keys[lo:hi] {
			vi := v.Index(lo + i).Interface()
			if key.Incomplete() {
				setStructKey(vi, rkeys[i])
				keys[i] = rkeys[i]
			}
			if g.inTransaction {
				g.toSet[memkey(rkeys[i])] = vi
			}
		}
	}

	if !g.inTransaction {
		g.putMemoryMulti(src)
	}

	return keys, nil
}

// PutComplete is like Put, but errors if a key is incomplete.
func (g *Goon) PutComplete(src interface{}) (*datastore.Key, error) {
	k, err := g.getStructKey(src)
	if err != nil {
		return nil, err
	}
	if k.Incomplete() {
		err := fmt.Errorf("goon: incomplete key: %v", k)
		g.error(err)
		return nil, err
	}
	return g.Put(src)
}

// PutMultiComplete is like PutMulti, but errors if a key is incomplete.
func (g *Goon) PutMultiComplete(src interface{}) ([]*datastore.Key, error) {
	keys, err := g.extractKeys(src, false)
	if err != nil {
		return nil, err
	}
	for i, k := range keys {
		if k.Incomplete() {
			err := fmt.Errorf("goon: incomplete key (%dth index): %v", i, k)
			g.error(err)
			return nil, err
		}
	}
	return g.PutMulti(src)
}

func (g *Goon) putMemoryMulti(src interface{}) {
	v := reflect.Indirect(reflect.ValueOf(src))
	size := v.Len()
	for i := 0; i < size; i++ {
		g.putMemory(v.Index(i).Interface())
	}
}

func (g *Goon) putMemory(src interface{}) {
	key, _ := g.getStructKey(src)
	if reflect.ValueOf(src).Kind() == reflect.Ptr && reflect.ValueOf(src).Elem().Kind() == reflect.Struct {
		g.cache[memkey(key)] = reflect.ValueOf(src).Interface()
	} else if reflect.ValueOf(src).Kind() == reflect.Struct {
		g.cache[memkey(key)] = reflect.ValueOf(src).Addr().Interface()
	}
}

func (g *Goon) putMemcache(srcs []interface{}) error {
	items := make([]*memcache.Item, len(srcs))

	for i, src := range srcs {
		gob, err := toGob(src)
		if err != nil {
			g.error(err)
			return err
		}
		key, err := g.getStructKey(src)

		items[i] = &memcache.Item{
			Key:   memkey(key),
			Value: gob,
		}
	}
	err := memcache.AddMulti(g.context, items)
	g.fakeDelay(time.Millisecond * 3)
	if err != nil {
		g.error(fmt.Errorf("Race condition detected, two concurrent requests did a Get/GetMulti over the same entity/entities"))
		return err
	}
	g.putMemoryMulti(srcs)
	return nil
}

// Get loads the entity based on dst's key into dst
// If there is no such entity for the key, Get returns
// datastore.ErrNoSuchEntity.
func (g *Goon) Get(dst interface{}) error {
	set := reflect.ValueOf(dst)
	if set.Kind() != reflect.Ptr {
		return errors.New(fmt.Sprintf("goon: expected pointer to a struct, got %#v", dst))
	}
	set = set.Elem()
	if !set.CanSet() {
		return errors.New(fmt.Sprintf("goon: provided %#v, which cannot be changed", dst))
	}
	dsts := []interface{}{dst}
	if err := g.GetMulti(&dsts); err != nil {
		// Look for an embedded error if it's multi
		if me, ok := err.(appengine.MultiError); ok {
			for i, merr := range me {
				if i == 0 {
					return merr
				}
			}
		}
		// Not multi, normal error
		return err
	}
	return nil
}

const getMultiLimit = 1000

// GetMulti is a batch version of Get.
//
// dst has similar constraints as datastore.GetMulti.
func (g *Goon) GetMulti(dst interface{}) error {
	item := reflect.ValueOf(dst)
	if item.Kind() != reflect.Ptr || item.Elem().Kind() != reflect.Slice {
		return errors.New(fmt.Sprintf("goon: must provide pointer to slice of pointer to struct, supplied - %#v", dst))
	}

	keys, err := g.extractKeys(dst, false) // don't allow incomplete keys on a Get request
	if err != nil {
		return err
	}

	if g.inTransaction {
		// todo: support getMultiLimit in transactions
		res := datastore.GetMulti(g.context, keys, dst)
		g.fakeDelay(time.Millisecond * 10)
		return res
	}

	var dskeys []*datastore.Key
	var dsdst []interface{}
	var dixs []int

	var memkeys []string
	var mixs []int

	v := reflect.Indirect(reflect.ValueOf(dst))
	for i, key := range keys {
		m := memkey(key)
		if s, present := g.cache[m]; present && false {
			vi := v.Index(i)
			vi.Set(reflect.ValueOf(s))
		} else {
			memkeys = append(memkeys, m)
			mixs = append(mixs, i)
		}
	}
	if len(memkeys) == 0 {
		return nil
	}

	memvalues, _ := memcache.GetMulti(g.context, memkeys)
	g.fakeDelay(time.Millisecond * 2)
	for i, m := range memkeys {
		d := v.Index(mixs[i]).Interface()
		if s, present := memvalues[m]; present {
			err := fromGob(d, s.Value)
			if err != nil {
				g.error(err)
				return err
			}
			g.putMemory(d)
		} else {
			key, err := g.getStructKey(d)
			if err != nil {
				g.error(err)
				return err
			}
			dskeys = append(dskeys, key)
			dsdst = append(dsdst, d)
			dixs = append(dixs, mixs[i])
		}
	}

	multiErr := make(appengine.MultiError, len(keys))
	var toCache []interface{}
	var ret error
	for i := 0; i <= len(dskeys)/getMultiLimit; i++ {
		lo := i * getMultiLimit
		hi := (i + 1) * getMultiLimit
		if hi > len(dskeys) {
			hi = len(dskeys)
		}
		gmerr := datastore.GetMulti(g.context, dskeys[lo:hi], dsdst[lo:hi])
		g.fakeDelay(time.Millisecond * 10)
		if gmerr != nil {
			merr, ok := gmerr.(appengine.MultiError)
			if !ok {
				g.error(gmerr)
				return gmerr
			}
			for i, idx := range dixs[lo:hi] {
				multiErr[idx] = merr[i]
				if merr[i] == nil {
					toCache = append(toCache, dsdst[lo+i])
				}
			}
			ret = multiErr
		} else {
			toCache = append(toCache, dsdst[lo:hi]...)
		}
	}

	if len(toCache) > 0 {
		if err := g.putMemcache(toCache); err != nil {
			g.error(err)
			return err
		}
	}

	return ret
}

// Delete deletes the entity for the given key.
func (g *Goon) Delete(key *datastore.Key) error {
	keys := []*datastore.Key{key}
	return g.DeleteMulti(keys)
}

const deleteMultiLimit = 500

// DeleteMulti is a batch version of Delete.
func (g *Goon) DeleteMulti(keys []*datastore.Key) error {
	memkeys := make([]string, len(keys))
	for i, k := range keys {
		mk := memkey(k)
		memkeys[i] = mk

		if g.inTransaction {
			g.toDelete = append(g.toDelete, mk)
		} else {
			delete(g.cache, mk)
		}
	}

	// Memcache needs to be updated after the datastore to prevent a common race condition
	defer func() {
		memcache.DeleteMulti(g.context, memkeys)
		g.fakeDelay(time.Millisecond * 2)
	}()

	for i := 0; i <= len(keys)/deleteMultiLimit; i++ {
		lo := i * deleteMultiLimit
		hi := (i + 1) * deleteMultiLimit
		if hi > len(keys) {
			hi = len(keys)
		}
		if err := datastore.DeleteMulti(g.context, keys[lo:hi]); err != nil {
			g.fakeDelay(time.Millisecond * 3)
			return err
		}
		g.fakeDelay(time.Millisecond * 5)
	}
	return nil
}

// NotFound returns true if err is an appengine.MultiError and err[idx] is a datastore.ErrNoSuchEntity.
func NotFound(err error, idx int) bool {
	if merr, ok := err.(appengine.MultiError); ok {
		return idx < len(merr) && merr[idx] == datastore.ErrNoSuchEntity
	}
	return false
}
