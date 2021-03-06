// Copyright 2013 The LevelDB-Go and Pebble Authors. All rights reserved. Use
// of this source code is governed by a BSD-style license that can be found in
// the LICENSE file.

package pebble

import (
	"fmt"
	"path/filepath"

	"github.com/petermattis/pebble/db"
	"github.com/petermattis/pebble/sstable"
)

// expandedCompactionByteSizeLimit is the maximum number of bytes in all
// compacted files. We avoid expanding the lower level file set of a compaction
// if it would make the total compaction cover more than this many bytes.
func expandedCompactionByteSizeLimit(opts *db.Options, level int) uint64 {
	return uint64(25 * opts.Level(level).TargetFileSize)
}

// maxGrandparentOverlapBytes is the maximum bytes of overlap with level+2
// before we stop building a single file in a level to level+1 compaction.
func maxGrandparentOverlapBytes(opts *db.Options, level int) uint64 {
	return uint64(10 * opts.Level(level).TargetFileSize)
}

// compaction is a table compaction from one level to the next, starting from a
// given version.
type compaction struct {
	version *version

	// level is the level that is being compacted. Inputs from level and
	// level+1 will be merged to produce a set of level+1 files.
	level int

	// inputs are the tables to be compacted.
	inputs [3][]fileMetadata
}

// pickCompaction picks the best compaction, if any, for vs' current version.
func pickCompaction(vs *versionSet) (c *compaction) {
	cur := vs.currentVersion()

	// Pick a compaction based on size. If none exist, pick one based on seeks.
	if cur.compactionScore >= 1 {
		c = &compaction{
			version: cur,
			level:   cur.compactionLevel,
		}
		// TODO(peter): Pick the first file that comes after the compaction pointer
		// for c.level.
		c.inputs[0] = []fileMetadata{cur.files[c.level][0]}
	} else {
		return nil
	}

	// Files in level 0 may overlap each other, so pick up all overlapping ones.
	if c.level == 0 {
		smallest, largest := ikeyRange(vs.cmp, c.inputs[0], nil)
		c.inputs[0] = cur.overlaps(0, vs.cmp, smallest.UserKey, largest.UserKey)
		if len(c.inputs) == 0 {
			panic("pebble: empty compaction")
		}
	}

	c.setupOtherInputs(vs)
	return c
}

// TODO(peter): user initiated compactions.

// setupOtherInputs fills in the rest of the compaction inputs, regardless of
// whether the compaction was automatically scheduled or user initiated.
func (c *compaction) setupOtherInputs(vs *versionSet) {
	smallest0, largest0 := ikeyRange(vs.cmp, c.inputs[0], nil)
	c.inputs[1] = c.version.overlaps(c.level+1, vs.cmp, smallest0.UserKey, largest0.UserKey)
	smallest01, largest01 := ikeyRange(vs.cmp, c.inputs[0], c.inputs[1])

	// Grow the inputs if it doesn't affect the number of level+1 files.
	if c.grow(vs, smallest01, largest01) {
		smallest01, largest01 = ikeyRange(vs.cmp, c.inputs[0], c.inputs[1])
	}

	// Compute the set of level+2 files that overlap this compaction.
	if c.level+2 < numLevels {
		c.inputs[2] = c.version.overlaps(c.level+2, vs.cmp, smallest01.UserKey, largest01.UserKey)
	}

	// TODO(peter): update the compaction pointer for c.level.
}

// grow grows the number of inputs at c.level without changing the number of
// c.level+1 files in the compaction, and returns whether the inputs grew. sm
// and la are the smallest and largest InternalKeys in all of the inputs.
func (c *compaction) grow(vs *versionSet, sm, la db.InternalKey) bool {
	if len(c.inputs[1]) == 0 {
		return false
	}
	grow0 := c.version.overlaps(c.level, vs.cmp, sm.UserKey, la.UserKey)
	if len(grow0) <= len(c.inputs[0]) {
		return false
	}
	if totalSize(grow0)+totalSize(c.inputs[1]) >=
		expandedCompactionByteSizeLimit(vs.opts, c.level+1) {
		return false
	}
	sm1, la1 := ikeyRange(vs.cmp, grow0, nil)
	grow1 := c.version.overlaps(c.level+1, vs.cmp, sm1.UserKey, la1.UserKey)
	if len(grow1) != len(c.inputs[1]) {
		return false
	}
	c.inputs[0] = grow0
	c.inputs[1] = grow1
	return true
}

// isBaseLevelForUkey reports whether it is guaranteed that there are no
// key/value pairs at c.level+2 or higher that have the user key ukey.
func (c *compaction) isBaseLevelForUkey(userCmp db.Compare, ukey []byte) bool {
	// TODO(peter): this can be faster if ukey is always increasing between
	// successive isBaseLevelForUkey calls and we can keep some state in between
	// calls.
	for level := c.level + 2; level < numLevels; level++ {
		for _, f := range c.version.files[level] {
			if userCmp(ukey, f.largest.UserKey) <= 0 {
				if userCmp(ukey, f.smallest.UserKey) >= 0 {
					return false
				}
				// For levels above level 0, the files within a level are in
				// increasing ikey order, so we can break early.
				break
			}
		}
	}
	return true
}

// maybeScheduleFlush schedules a flush if necessary.
//
// d.mu must be held when calling this.
func (d *DB) maybeScheduleFlush() {
	if d.mu.compact.flushing || d.mu.closed {
		return
	}
	if len(d.mu.mem.queue) <= 1 {
		return
	}
	if !d.mu.mem.queue[0].readyForFlush() {
		return
	}

	d.mu.compact.flushing = true
	go d.flush()
}

func (d *DB) flush() {
	d.mu.Lock()
	defer d.mu.Unlock()
	if err := d.flush1(); err != nil {
		// TODO(peter): count consecutive compaction errors and backoff.
	}
	d.mu.compact.flushing = false
	// More flush work may have arrived while we were flushing, so schedule
	// another flush if needed.
	d.maybeScheduleFlush()
	// The flush may have produced too many files in a level, so schedule a
	// compaction if needed.
	d.maybeScheduleCompaction()
	d.mu.compact.cond.Broadcast()
}

// flush runs a compaction that copies the immutable memtables from memory to
// disk.
//
// d.mu must be held when calling this, but the mutex may be dropped and
// re-acquired during the course of this method.
func (d *DB) flush1() error {
	// var dirty int
	// for _, mem := range d.mu.mem.queue {
	// 	dirty += mem.ApproximateMemoryUsage()
	// }

	var n int
	for ; n < len(d.mu.mem.queue)-1; n++ {
		if !d.mu.mem.queue[n].readyForFlush() {
			break
		}
	}
	if n == 0 {
		// None of the immutable memtables are ready for flushing.
		return nil
	}

	var iter db.InternalIterator
	if n == 1 {
		iter = d.mu.mem.queue[0].NewIter(nil)
	} else {
		iters := make([]db.InternalIterator, n)
		for i := range iters {
			iters[i] = d.mu.mem.queue[i].NewIter(nil)
		}
		iter = newMergingIter(d.cmp, iters...)
	}

	meta, err := d.writeLevel0Table(d.opts.Storage, iter)
	if err != nil {
		return err
	}

	err = d.mu.versions.logAndApply(d.opts, d.dirname, &versionEdit{
		logNumber: d.mu.log.number,
		newFiles: []newFileEntry{
			{level: 0, meta: meta},
		},
	})
	delete(d.mu.compact.pendingOutputs, meta.fileNum)
	if err != nil {
		return err
	}

	// Mark all the memtables we flushed as flushed.
	for i := 0; i < n; i++ {
		close(d.mu.mem.queue[i].flushed)
	}
	d.mu.mem.queue = d.mu.mem.queue[n:]

	// var newDirty int
	// for _, mem := range d.mu.mem.queue {
	// 	newDirty += mem.ApproximateMemoryUsage()
	// }
	// fmt.Printf("flushed %d: %.1f MB -> %.1f MB\n",
	// 	n, float64(dirty)/(1<<20), float64(newDirty)/(1<<20))

	d.deleteObsoleteFiles()
	return nil
}

// maybeScheduleCompaction schedules a compaction if necessary.
//
// d.mu must be held when calling this.
func (d *DB) maybeScheduleCompaction() {
	if d.mu.compact.compacting || d.mu.closed {
		return
	}

	// TODO(peter): check for manual compactions.

	v := d.mu.versions.currentVersion()
	// TODO(peter): check v.fileToCompact.
	if v.compactionScore < 1 {
		// There is no work to be done.
		return
	}

	d.mu.compact.compacting = true
	go d.compact()
}

// compact runs one compaction and maybe schedules another call to compact.
func (d *DB) compact() {
	d.mu.Lock()
	defer d.mu.Unlock()
	if err := d.compact1(); err != nil {
		// TODO(peter): count consecutive compaction errors and backoff.
	}
	d.mu.compact.compacting = false
	// The previous compaction may have produced too many files in a
	// level, so reschedule another compaction if needed.
	d.maybeScheduleCompaction()
	d.mu.compact.cond.Broadcast()
}

// compact1 runs one compaction.
//
// d.mu must be held when calling this, but the mutex may be dropped and
// re-acquired during the course of this method.
func (d *DB) compact1() error {
	// TODO(peter): support manual compactions.

	c := pickCompaction(&d.mu.versions)
	if c == nil {
		return nil
	}

	// Check for a trivial move of one table from one level to the next.
	// We avoid such a move if there is lots of overlapping grandparent data.
	// Otherwise, the move could create a parent file that will require
	// a very expensive merge later on.
	//
	if len(c.inputs[0]) == 1 && len(c.inputs[1]) == 0 &&
		totalSize(c.inputs[2]) <= maxGrandparentOverlapBytes(d.opts, c.level+1) {

		meta := &c.inputs[0][0]
		return d.mu.versions.logAndApply(d.opts, d.dirname, &versionEdit{
			deletedFiles: map[deletedFileEntry]bool{
				deletedFileEntry{level: c.level, fileNum: meta.fileNum}: true,
			},
			newFiles: []newFileEntry{
				{level: c.level + 1, meta: *meta},
			},
		})
	}

	ve, pendingOutputs, err := d.compactDiskTables(c)
	if err != nil {
		return err
	}
	err = d.mu.versions.logAndApply(d.opts, d.dirname, ve)
	for _, fileNum := range pendingOutputs {
		delete(d.mu.compact.pendingOutputs, fileNum)
	}
	if err != nil {
		return err
	}
	d.deleteObsoleteFiles()
	return nil
}

// compactDiskTables runs a compaction that produces new on-disk tables from
// old on-disk tables.
//
// d.mu must be held when calling this, but the mutex may be dropped and
// re-acquired during the course of this method.
func (d *DB) compactDiskTables(c *compaction) (ve *versionEdit, pendingOutputs []uint64, retErr error) {
	defer func() {
		if retErr != nil {
			for _, fileNum := range pendingOutputs {
				delete(d.mu.compact.pendingOutputs, fileNum)
			}
			pendingOutputs = nil
		}
	}()

	// Release the d.mu lock while doing I/O.
	// Note the unusual order: Unlock and then Lock.
	d.mu.Unlock()
	defer d.mu.Lock()

	iiter, err := compactionIterator(d.cmp, d.newIter, c)
	if err != nil {
		return nil, pendingOutputs, err
	}
	iter := &compactionIter{
		cmp:   d.cmp,
		merge: d.merge,
		iter:  iiter,
	}

	// TODO(peter): output to more than one table, if it would otherwise be too large.
	var (
		fileNum  uint64
		filename string
		tw       *sstable.Writer
	)
	defer func() {
		if iter != nil {
			retErr = firstError(retErr, iter.Close())
		}
		if tw != nil {
			retErr = firstError(retErr, tw.Close())
		}
		if retErr != nil {
			d.opts.Storage.Remove(filename)
		}
	}()

	var smallest, largest db.InternalKey
	for iter.First(); iter.Valid(); iter.Next() {
		// TODO(peter): support c.shouldStopBefore.

		ikey := iter.Key()
		if ikey.Kind() == db.InternalKeyKindDelete &&
			c.isBaseLevelForUkey(d.opts.Comparer.Compare, ikey.UserKey) {
			continue
		}

		if tw == nil {
			d.mu.Lock()
			fileNum = d.mu.versions.nextFileNum()
			d.mu.compact.pendingOutputs[fileNum] = struct{}{}
			pendingOutputs = append(pendingOutputs, fileNum)
			d.mu.Unlock()

			filename = dbFilename(d.dirname, fileTypeTable, fileNum)
			file, err := d.opts.Storage.Create(filename)
			if err != nil {
				return nil, pendingOutputs, err
			}
			tw = sstable.NewWriter(file, d.opts, d.opts.Level(c.level+1))
			smallest = ikey.Clone()
		}

		// Avoid the memory allocation in InternalKey.Clone() by reusing the buffer
		// in largest.
		//
		// TODO(peter): sstable.Writer internally keeps track of the last key
		// added. Rather than making our own copy here, we should expose that one.
		largest.UserKey = append(largest.UserKey[:0], ikey.UserKey...)
		largest.Trailer = ikey.Trailer
		if err := tw.Add(ikey, iter.Value()); err != nil {
			return nil, pendingOutputs, err
		}
	}

	if err := tw.Close(); err != nil {
		tw = nil
		return nil, pendingOutputs, err
	}
	stat, err := tw.Stat()
	if err != nil {
		tw = nil
		return nil, pendingOutputs, err
	}
	tw = nil

	ve = &versionEdit{
		deletedFiles: map[deletedFileEntry]bool{},
		newFiles: []newFileEntry{
			{
				level: c.level + 1,
				meta: fileMetadata{
					fileNum:  fileNum,
					size:     uint64(stat.Size()),
					smallest: smallest,
					largest:  largest,
				},
			},
		},
	}
	for i := 0; i < 2; i++ {
		for _, f := range c.inputs[i] {
			ve.deletedFiles[deletedFileEntry{
				level:   c.level + i,
				fileNum: f.fileNum,
			}] = true
		}
	}
	return ve, pendingOutputs, nil
}

// deleteObsoleteFiles deletes those files that are no longer needed.
//
// d.mu must be held when calling this, but the mutex may be dropped and
// re-acquired during the course of this method.
func (d *DB) deleteObsoleteFiles() {
	liveFileNums := map[uint64]struct{}{}
	for fileNum := range d.mu.compact.pendingOutputs {
		liveFileNums[fileNum] = struct{}{}
	}
	d.mu.versions.addLiveFileNums(liveFileNums)
	logNumber := d.mu.versions.logNumber
	manifestFileNumber := d.mu.versions.manifestFileNumber

	// Release the d.mu lock while doing I/O.
	// Note the unusual order: Unlock and then Lock.
	d.mu.Unlock()
	defer d.mu.Lock()

	fs := d.opts.Storage
	list, err := fs.List(d.dirname)
	if err != nil {
		// Ignore any filesystem errors.
		return
	}
	for _, filename := range list {
		fileType, fileNum, ok := parseDBFilename(filename)
		if !ok {
			return
		}
		keep := true
		switch fileType {
		case fileTypeLog:
			// TODO(peter): also look at prevLogNumber?
			keep = fileNum >= logNumber
		case fileTypeManifest:
			keep = fileNum >= manifestFileNumber
		case fileTypeTable:
			_, keep = liveFileNums[fileNum]
		}
		if keep {
			continue
		}
		if fileType == fileTypeTable {
			d.tableCache.evict(fileNum)
		}
		// Ignore any file system errors.
		fs.Remove(filepath.Join(d.dirname, filename))
	}
}

// compactionIterator returns an iterator over all the tables in a compaction.
func compactionIterator(
	cmp db.Compare, newIter tableNewIter, c *compaction,
) (cIter db.InternalIterator, retErr error) {
	iters := make([]db.InternalIterator, 0, len(c.inputs[0])+1)
	defer func() {
		if retErr != nil {
			for _, iter := range iters {
				if iter != nil {
					iter.Close()
				}
			}
		}
	}()

	if c.level != 0 {
		iter := newLevelIter(cmp, newIter, c.inputs[0])
		iters = append(iters, iter)
	} else {
		for i := range c.inputs[0] {
			f := &c.inputs[0][i]
			iter, err := newIter(f)
			if err != nil {
				return nil, fmt.Errorf("pebble: could not open table %d: %v", f.fileNum, err)
			}
			iter.First()
			iters = append(iters, iter)
		}
	}

	iter := newLevelIter(cmp, newIter, c.inputs[1])
	iters = append(iters, iter)
	return newMergingIter(cmp, iters...), nil
}
