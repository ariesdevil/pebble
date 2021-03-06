// Copyright 2011 The LevelDB-Go and Pebble Authors. All rights reserved. Use
// of this source code is governed by a BSD-style license that can be found in
// the LICENSE file.

package sstable

import (
	"bytes"
	"encoding/binary"
	"errors"
	"fmt"
	"io"

	"github.com/golang/snappy"
	"github.com/petermattis/pebble/cache"
	"github.com/petermattis/pebble/crc"
	"github.com/petermattis/pebble/db"
	"github.com/petermattis/pebble/storage"
)

// blockHandle is the file offset and length of a block.
type blockHandle struct {
	offset, length uint64
}

// decodeBlockHandle returns the block handle encoded at the start of src, as
// well as the number of bytes it occupies. It returns zero if given invalid
// input.
func decodeBlockHandle(src []byte) (blockHandle, int) {
	offset, n := binary.Uvarint(src)
	length, m := binary.Uvarint(src[n:])
	if n == 0 || m == 0 {
		return blockHandle{}, 0
	}
	return blockHandle{offset, length}, n + m
}

func encodeBlockHandle(dst []byte, b blockHandle) int {
	n := binary.PutUvarint(dst, b.offset)
	m := binary.PutUvarint(dst[n:], b.length)
	return n + m
}

// block is a []byte that holds a sequence of key/value pairs plus an index
// over those pairs.
type block []byte

// Iter is an iterator over an entire table of data. It is a two-level
// iterator: to seek for a given key, it first looks in the index for the
// block that contains that key, and then looks inside that block.
type Iter struct {
	reader *Reader
	index  blockIter
	data   blockIter
	err    error
}

// Iter implements the db.InternalIterator interface.
var _ db.InternalIterator = (*Iter)(nil)

func (i *Iter) init(r *Reader) error {
	i.reader = r
	i.err = i.index.init(r.compare, r.index, r.Properties.GlobalSeqNum)
	return i.err
}

// loadBlock loads the block at the current index position and leaves i.data
// unpositioned. If unsuccessful, it sets i.err to any error encountered, which
// may be nil if we have simply exhausted the entire table.
func (i *Iter) loadBlock() bool {
	if !i.index.Valid() {
		i.err = i.index.err
		return false
	}
	// Load the next block.
	v := i.index.Value()
	h, n := decodeBlockHandle(v)
	if n == 0 || n != len(v) {
		i.err = errors.New("pebble/table: corrupt index entry")
		return false
	}
	block, err := i.reader.readBlock(h)
	if err != nil {
		i.err = err
		return false
	}
	i.err = i.data.init(i.reader.compare, block, i.reader.Properties.GlobalSeqNum)
	if i.err != nil {
		return false
	}
	return true
}

// seekBlock loads the block at the current index position and positions i.data
// at the first key in that block which is >= the given key. If unsuccessful,
// it sets i.err to any error encountered, which may be nil if we have simply
// exhausted the entire table.
//
// If f is non-nil, the caller is presumably looking for one specific key, as
// opposed to iterating over a range of keys (where the minimum of that range
// isn't necessarily in the table). In that case, i.err will be set to
// db.ErrNotFound if f does not contain the key.
func (i *Iter) seekBlock(key []byte, f *blockFilterReader) bool {
	if !i.index.Valid() {
		i.err = i.index.err
		return false
	}
	// Load the next block.
	v := i.index.Value()
	h, n := decodeBlockHandle(v)
	if n == 0 || n != len(v) {
		i.err = errors.New("pebble/table: corrupt index entry")
		return false
	}
	if f != nil && !f.mayContain(h.offset, key) {
		i.err = db.ErrNotFound
		return false
	}
	block, err := i.reader.readBlock(h)
	if err != nil {
		i.err = err
		return false
	}
	i.err = i.data.init(i.reader.compare, block, i.reader.Properties.GlobalSeqNum)
	if i.err != nil {
		return false
	}
	// Look for the key inside that block.
	i.data.SeekGE(key)
	return true
}

// SeekGE implements InternalIterator.SeekGE, as documented in the pebble/db
// package.
func (i *Iter) SeekGE(key []byte) {
	if i.err != nil {
		return
	}

	i.index.SeekGE(key)
	if i.loadBlock() {
		i.data.SeekGE(key)
	}
}

// SeekLT implements InternalIterator.SeekLT, as documented in the pebble/db
// package.
func (i *Iter) SeekLT(key []byte) {
	if i.err != nil {
		return
	}

	i.index.SeekGE(key)
	if !i.index.Valid() {
		i.index.Last()
	}
	if i.loadBlock() {
		i.data.SeekLT(key)
		if !i.data.Valid() {
			// The index contains separator keys which may between
			// user-keys. Consider the user-keys:
			//
			//   complete
			// ---- new block ---
			//   complexion
			//
			// If these two keys end one block and start the next, the index key may
			// be chosen as "compleu". The SeekGE in the index block will then point
			// us to the block containing "complexion". If this happens, we want the
			// last key from the previous data block.
			i.index.Prev()
			if i.loadBlock() {
				i.data.Last()
			}
		}
	}
}

// First implements InternalIterator.First, as documented in the pebble/db
// package.
func (i *Iter) First() {
	if i.err != nil {
		return
	}

	i.index.First()
	if i.loadBlock() {
		i.data.First()
	}
}

// Last implements InternalIterator.Last, as documented in the pebble/db
// package.
func (i *Iter) Last() {
	if i.err != nil {
		return
	}

	i.index.Last()
	if i.loadBlock() {
		i.data.Last()
	}
}

// Next implements InternalIterator.Next, as documented in the pebble/db
// package.
func (i *Iter) Next() bool {
	if i.err != nil {
		return false
	}
	if i.data.Next() {
		return true
	}
	for {
		if i.data.err != nil {
			i.err = i.data.err
			break
		}
		if !i.index.Next() {
			break
		}
		if i.loadBlock() {
			i.data.First()
			return true
		}
	}
	return false
}

// NextUserKey implements InternalIterator.NextUserKey, as documented in the
// pebble/db package.
func (i *Iter) NextUserKey() bool {
	return i.Next()
}

// Prev implements InternalIterator.Prev, as documented in the pebble/db
// package.
func (i *Iter) Prev() bool {
	if i.err != nil {
		return false
	}
	if i.data.Prev() {
		return true
	}
	for {
		if i.data.err != nil {
			i.err = i.data.err
			break
		}
		if !i.index.Prev() {
			break
		}
		if i.loadBlock() {
			i.data.Last()
			return true
		}
	}
	return false
}

// PrevUserKey implements InternalIterator.PrevUserKey, as documented in the
// pebble/db package.
func (i *Iter) PrevUserKey() bool {
	return i.Prev()
}

// Key implements InternalIterator.Key, as documented in the pebble/db package.
func (i *Iter) Key() db.InternalKey {
	return i.data.Key()
}

// Value implements InternalIterator.Value, as documented in the pebble/db
// package.
func (i *Iter) Value() []byte {
	return i.data.Value()
}

// Valid implements InternalIterator.Valid, as documented in the pebble/db
// package.
func (i *Iter) Valid() bool {
	return i.data.Valid()
}

// Error implements InternalIterator.Error, as documented in the pebble/db
// package.
func (i *Iter) Error() error {
	if err := i.data.Error(); err != nil {
		return err
	}
	return i.err
}

// Close implements InternalIterator.Close, as documented in the pebble/db
// package.
func (i *Iter) Close() error {
	if err := i.data.Close(); err != nil {
		return err
	}
	return i.err
}

// Reader is a table reader. It implements the DB interface, as documented
// in the pebble/db package.
type Reader struct {
	file        storage.File
	fileNum     uint64
	err         error
	index       block
	opts        *db.Options
	cache       *cache.Cache
	compare     db.Compare
	blockFilter *blockFilterReader
	tableFilter *tableFilterReader
	Properties  Properties
}

// Close implements DB.Close, as documented in the pebble/db package.
func (r *Reader) Close() error {
	if r.err != nil {
		if r.file != nil {
			r.file.Close()
			r.file = nil
		}
		return r.err
	}
	if r.file != nil {
		r.err = r.file.Close()
		r.file = nil
		if r.err != nil {
			return r.err
		}
	}
	// Make any future calls to Get, NewIter or Close return an error.
	r.err = errors.New("pebble/table: reader is closed")
	return nil
}

func (r *Reader) get(key []byte, o *db.IterOptions) (value []byte, err error) {
	if r.err != nil {
		return nil, r.err
	}

	if r.tableFilter != nil {
		if !r.tableFilter.mayContain(key) {
			return nil, db.ErrNotFound
		}
	}

	i := &Iter{}
	if err := i.init(r); err == nil {
		i.index.SeekGE(key)
		i.seekBlock(key, r.blockFilter)
	}

	if !i.Valid() || r.compare(key, i.Key().UserKey) != 0 {
		err := i.Close()
		if err == nil {
			err = db.ErrNotFound
		}
		return nil, err
	}
	return i.Value(), i.Close()
}

// NewIter implements DB.NewIter, as documented in the pebble/db package.
func (r *Reader) NewIter(o *db.IterOptions) db.InternalIterator {
	// NB: pebble.tableCache wraps the returned iterator with one which performs
	// reference counting on the Reader, preventing the Reader from being closed
	// until the final iterator closes.
	if r.err != nil {
		return &Iter{err: r.err}
	}
	i := &Iter{}
	_ = i.init(r)
	return i
}

// readBlock reads and decompresses a block from disk into memory.
func (r *Reader) readBlock(bh blockHandle) (block, error) {
	if b := r.cache.Get(r.fileNum, bh.offset); b != nil {
		return b, nil
	}

	b := make([]byte, bh.length+blockTrailerLen)
	if _, err := r.file.ReadAt(b, int64(bh.offset)); err != nil {
		return nil, err
	}
	checksum0 := binary.LittleEndian.Uint32(b[bh.length+1:])
	checksum1 := crc.New(b[:bh.length+1]).Value()
	if checksum0 != checksum1 {
		return nil, errors.New("pebble/table: invalid table (checksum mismatch)")
	}
	switch b[bh.length] {
	case noCompressionBlockType:
		b = b[:bh.length]
		r.cache.Set(r.fileNum, bh.offset, b)
		return b, nil
	case snappyCompressionBlockType:
		b, err := snappy.Decode(nil, b[:bh.length])
		if err != nil {
			return nil, err
		}
		r.cache.Set(r.fileNum, bh.offset, b)
		return b, nil
	}
	return nil, fmt.Errorf("pebble/table: unknown block compression: %d", b[bh.length])
}

func (r *Reader) readMetaindex(metaindexBH blockHandle, o *db.Options) error {
	b, err := r.readBlock(metaindexBH)
	if err != nil {
		return err
	}
	i, err := newRawBlockIter(bytes.Compare, b)
	if err != nil {
		return err
	}

	meta := map[string]blockHandle{}
	for i.First(); i.Valid(); i.Next() {
		bh, n := decodeBlockHandle(i.Value())
		if n == 0 {
			return errors.New("pebble/table: invalid table (bad filter block handle)")
		}
		meta[string(i.Key().UserKey)] = bh
	}
	if err := i.Close(); err != nil {
		return err
	}

	if bh, ok := meta["rocksdb.properties"]; ok {
		b, err = r.readBlock(bh)
		if err != nil {
			return err
		}
		if err := r.Properties.load(b, bh.offset); err != nil {
			return err
		}
	}

	for level := range r.opts.Levels {
		fp := r.opts.Levels[level].FilterPolicy
		if fp == nil {
			continue
		}
		types := []struct {
			ftype  db.FilterType
			prefix string
		}{
			{db.BlockFilter, "filter."},
			{db.TableFilter, "fullfilter."},
		}
		var done bool
		for _, t := range types {
			if bh, ok := meta[t.prefix+fp.Name()]; ok {
				b, err = r.readBlock(bh)
				if err != nil {
					return err
				}

				switch t.ftype {
				case db.BlockFilter:
					r.blockFilter = newBlockFilterReader(b, fp)
					if r.blockFilter == nil {
						return errors.New("pebble/table: invalid table (bad filter block)")
					}
				case db.TableFilter:
					r.tableFilter = newTableFilterReader(b, fp)
					if r.tableFilter == nil {
						return errors.New("pebble/table: invalid table (bad filter block)")
					}
				default:
					panic(fmt.Sprintf("unknown filter type: %v", t.ftype))
				}

				done = true
				break
			}
		}
		if done {
			break
		}
	}
	return nil
}

// NewReader returns a new table reader for the file. Closing the reader will
// close the file.
func NewReader(f storage.File, fileNum uint64, o *db.Options) *Reader {
	o = o.EnsureDefaults()
	r := &Reader{
		file:    f,
		fileNum: fileNum,
		opts:    o,
		cache:   o.Cache,
		compare: o.Comparer.Compare,
	}
	if f == nil {
		r.err = errors.New("pebble/table: nil file")
		return r
	}
	stat, err := f.Stat()
	if err != nil {
		r.err = fmt.Errorf("pebble/table: invalid table (could not stat file): %v", err)
		return r
	}

	// legacy footer format:
	//    metaindex handle (varint64 offset, varint64 size)
	//    index handle     (varint64 offset, varint64 size)
	//    <padding> to make the total size 2 * BlockHandle::kMaxEncodedLength
	//    table_magic_number (8 bytes)
	// new footer format:
	//    checksum type (char, 1 byte)
	//    metaindex handle (varint64 offset, varint64 size)
	//    index handle     (varint64 offset, varint64 size)
	//    <padding> to make the total size 2 * BlockHandle::kMaxEncodedLength + 1
	//    footer version (4 bytes)
	//    table_magic_number (8 bytes)
	footer := make([]byte, footerLen)
	if stat.Size() < int64(len(footer)) {
		r.err = errors.New("pebble/table: invalid table (file size is too small)")
		return r
	}
	_, err = f.ReadAt(footer, stat.Size()-int64(len(footer)))
	if err != nil && err != io.EOF {
		r.err = fmt.Errorf("pebble/table: invalid table (could not read footer): %v", err)
		return r
	}
	if string(footer[magicOffset:footerLen]) != magic {
		r.err = errors.New("pebble/table: invalid table (bad magic number)")
		return r
	}

	version := binary.LittleEndian.Uint32(footer[versionOffset:magicOffset])
	if version != formatVersion {
		r.err = fmt.Errorf("pebble/table: unsupported format version %d", version)
		return r
	}

	if footer[0] != checksumCRC32c {
		r.err = fmt.Errorf("pebble/table: unsupported checksum type %d", footer[0])
		return r
	}
	footer = footer[1:]

	// Read the metaindex.
	metaindexBH, n := decodeBlockHandle(footer)
	if n == 0 {
		r.err = errors.New("pebble/table: invalid table (bad metaindex block handle)")
		return r
	}
	footer = footer[n:]
	if err := r.readMetaindex(metaindexBH, o); err != nil {
		r.err = err
		return r
	}

	// Read the index into memory.
	//
	// TODO(peter): Allow the index block to be placed in the block cache.
	indexBH, n := decodeBlockHandle(footer)
	if n == 0 {
		r.err = errors.New("pebble/table: invalid table (bad index block handle)")
		return r
	}

	footer = footer[n:]
	r.index, r.err = r.readBlock(indexBH)

	// iter, _ := newBlockIter(r.compare, r.index)
	// for iter.First(); iter.Valid(); iter.Next() {
	// 	fmt.Printf("%s#%d\n", iter.Key().UserKey, iter.Key().SeqNum())
	// }
	return r
}
