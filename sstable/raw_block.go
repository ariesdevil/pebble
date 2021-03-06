// Copyright 2018 The LevelDB-Go and Pebble Authors. All rights reserved. Use
// of this source code is governed by a BSD-style license that can be found in
// the LICENSE file.

package sstable

import (
	"encoding/binary"
	"errors"
	"sort"
	"unsafe"

	"github.com/petermattis/pebble/db"
)

type rawBlockWriter struct {
	blockWriter
}

func (w *rawBlockWriter) add(key db.InternalKey, value []byte) {
	w.curKey, w.prevKey = w.prevKey, w.curKey

	size := len(key.UserKey)
	if cap(w.curKey) < size {
		w.curKey = make([]byte, 0, size*2)
	}
	w.curKey = w.curKey[:size]
	copy(w.curKey, key.UserKey)

	w.store(size, value)
}

// rawBlockIter is an iterator over a single block of data. Unlike blockIter,
// keys are stored in "raw" format (i.e. not as internal keys). Note that there
// is significant similarity between this code and the code in blockIter. Yet
// reducing duplication is difficult due to the blockIter being performance
// critical.
type rawBlockIter struct {
	cmp         db.Compare
	offset      int
	nextOffset  int
	restarts    int
	numRestarts int
	ptr         unsafe.Pointer
	data        []byte
	key, val    []byte
	ikey        db.InternalKey
	cached      []blockEntry
	cachedBuf   []byte
	err         error
}

// rawBlockIter implements the db.InternalIterator interface.
var _ db.InternalIterator = (*rawBlockIter)(nil)

func newRawBlockIter(cmp db.Compare, block block) (*rawBlockIter, error) {
	i := &rawBlockIter{}
	return i, i.init(cmp, block)
}

func (i *rawBlockIter) init(cmp db.Compare, block block) error {
	numRestarts := int(binary.LittleEndian.Uint32(block[len(block)-4:]))
	if numRestarts == 0 {
		return errors.New("pebble/table: invalid table (block has no restart points)")
	}
	i.cmp = cmp
	i.restarts = len(block) - 4*(1+numRestarts)
	i.numRestarts = numRestarts
	i.ptr = unsafe.Pointer(&block[0])
	i.data = block
	if i.key == nil {
		i.key = make([]byte, 0, 256)
	} else {
		i.key = i.key[:0]
	}
	i.val = nil
	i.clearCache()
	return nil
}

func (i *rawBlockIter) readEntry() {
	ptr := unsafe.Pointer(uintptr(i.ptr) + uintptr(i.offset))
	shared, ptr := decodeVarint(ptr)
	unshared, ptr := decodeVarint(ptr)
	value, ptr := decodeVarint(ptr)
	i.key = append(i.key[:shared], getBytes(ptr, int(unshared))...)
	i.key = i.key[:len(i.key):len(i.key)]
	ptr = unsafe.Pointer(uintptr(ptr) + uintptr(unshared))
	i.val = getBytes(ptr, int(value))
	i.nextOffset = int(uintptr(ptr)-uintptr(i.ptr)) + int(value)
}

func (i *rawBlockIter) loadEntry() {
	i.readEntry()
	i.ikey.UserKey = i.key
}

func (i *rawBlockIter) clearCache() {
	i.cached = i.cached[:0]
	i.cachedBuf = i.cachedBuf[:0]
}

func (i *rawBlockIter) cacheEntry() {
	i.cachedBuf = append(i.cachedBuf, i.key...)
	i.cached = append(i.cached, blockEntry{
		offset: i.offset,
		key:    i.cachedBuf[len(i.cachedBuf)-len(i.key) : len(i.cachedBuf) : len(i.cachedBuf)],
		val:    i.val,
	})
}

// SeekGE implements InternalIterator.SeekGE, as documented in the pebble/db
// package.
func (i *rawBlockIter) SeekGE(key []byte) {
	// Find the index of the smallest restart point whose key is > the key
	// sought; index will be numRestarts if there is no such restart point.
	i.offset = 0
	index := sort.Search(i.numRestarts, func(j int) bool {
		offset := int(binary.LittleEndian.Uint32(i.data[i.restarts+4*j:]))
		// For a restart point, there are 0 bytes shared with the previous key.
		// The varint encoding of 0 occupies 1 byte.
		ptr := unsafe.Pointer(uintptr(i.ptr) + uintptr(offset+1))
		// Decode the key at that restart point, and compare it to the key sought.
		v1, ptr := decodeVarint(ptr)
		_, ptr = decodeVarint(ptr)
		s := getBytes(ptr, int(v1))
		return i.cmp(key, s) < 0
	})

	// Since keys are strictly increasing, if index > 0 then the restart point at
	// index-1 will be the largest whose key is <= the key sought.  If index ==
	// 0, then all keys in this block are larger than the key sought, and offset
	// remains at zero.
	if index > 0 {
		i.offset = int(binary.LittleEndian.Uint32(i.data[i.restarts+4*(index-1):]))
	}
	i.loadEntry()

	// Iterate from that restart point to somewhere >= the key sought.
	for ; i.Valid(); i.Next() {
		if i.cmp(key, i.key) <= 0 {
			break
		}
	}
}

// SeekLT implements InternalIterator.SeekLT, as documented in the pebble/db
// package.
func (i *rawBlockIter) SeekLT(key []byte) {
	panic("pebble/table: SeekLT unimplemented")
}

// First implements InternalIterator.First, as documented in the pebble/db
// package.
func (i *rawBlockIter) First() {
	i.offset = 0
	i.loadEntry()
}

// Last implements InternalIterator.Last, as documented in the pebble/db package.
func (i *rawBlockIter) Last() {
	// Seek forward from the last restart point.
	i.offset = int(binary.LittleEndian.Uint32(i.data[i.restarts+4*(i.numRestarts-1):]))

	i.readEntry()
	i.clearCache()
	i.cacheEntry()

	for i.nextOffset < i.restarts {
		i.offset = i.nextOffset
		i.readEntry()
		i.cacheEntry()
	}

	i.ikey.UserKey = i.key
}

// Next implements InternalIterator.Next, as documented in the pebble/db
// package.
func (i *rawBlockIter) Next() bool {
	i.offset = i.nextOffset
	if !i.Valid() {
		return false
	}
	i.loadEntry()
	return true
}

// NextUserKey implements InternalIterator.NextUserKey, as documented in the
// pebble/db package.
func (i *rawBlockIter) NextUserKey() bool {
	return i.Next()
}

// Prev implements InternalIterator.Prev, as documented in the pebble/db
// package.
func (i *rawBlockIter) Prev() bool {
	if n := len(i.cached) - 1; n > 0 && i.cached[n].offset == i.offset {
		i.nextOffset = i.offset
		e := &i.cached[n-1]
		i.offset = e.offset
		i.val = e.val
		i.ikey.UserKey = e.key
		i.cached = i.cached[:n]
		return true
	}

	if i.offset == 0 {
		i.offset = -1
		i.nextOffset = 0
		return false
	}

	targetOffset := i.offset
	index := sort.Search(i.numRestarts, func(j int) bool {
		offset := int(binary.LittleEndian.Uint32(i.data[i.restarts+4*j:]))
		return offset >= targetOffset
	})
	i.offset = 0
	if index > 0 {
		i.offset = int(binary.LittleEndian.Uint32(i.data[i.restarts+4*(index-1):]))
	}

	i.readEntry()
	i.clearCache()
	i.cacheEntry()

	for i.nextOffset < targetOffset {
		i.offset = i.nextOffset
		i.readEntry()
		i.cacheEntry()
	}

	i.ikey.UserKey = i.key
	return true
}

// PrevUserKey implements InternalIterator.PrevUserKey, as documented in the
// pebble/db package.
func (i *rawBlockIter) PrevUserKey() bool {
	return i.Prev()
}

// Key implements InternalIterator.Key, as documented in the pebble/db package.
func (i *rawBlockIter) Key() db.InternalKey {
	return i.ikey
}

// Value implements InternalIterator.Value, as documented in the pebble/db
// package.
func (i *rawBlockIter) Value() []byte {
	return i.val
}

func (i *rawBlockIter) valueOffset() uint64 {
	ptr := unsafe.Pointer(uintptr(i.ptr) + uintptr(i.offset))
	shared, ptr := decodeVarint(ptr)
	unshared, _ := decodeVarint(ptr)
	return uint64(i.offset) + uint64(shared+unshared)
}

// Valid implements InternalIterator.Valid, as documented in the pebble/db
// package.
func (i *rawBlockIter) Valid() bool {
	return i.offset >= 0 && i.offset < i.restarts
}

// Error implements InternalIterator.Error, as documented in the pebble/db
// package.
func (i *rawBlockIter) Error() error {
	return i.err
}

// Close implements InternalIterator.Close, as documented in the pebble/db
// package.
func (i *rawBlockIter) Close() error {
	i.val = nil
	return i.err
}
