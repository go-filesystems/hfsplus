// Copyright (c) 2026, go-filesystems
// SPDX-License-Identifier: BSD-3-Clause

package hfsplus

import (
	"encoding/binary"
	"fmt"
)

// alloc.go manages the HFS+ allocation bitmap (the allocation file, TN1150
// "Allocation File"). Each bit maps one allocation block: the most-significant
// bit of byte 0 is block 0. The bitmap is not loaded: it is read in chunks of at
// most 4 KiB, on demand, and a chunk becomes an editable region of the
// transaction only when a bit in it changes, so allocating a file writes only the
// bitmap sectors that hold its blocks. The volume header's freeBlocks is kept in
// step as bits change. All access goes through allocator, which resolves the
// allocation file's extents from the volume header.

// allocChunk is the most bitmap bytes read or edited as one unit.
const allocChunk = 4096

// allocator provides block allocate/free over a writable volume's bitmap.
type allocator struct {
	v     *Volume
	bits  uint32             // total allocation blocks
	size  int64              // byte length of the allocation file's logical contents
	exts  []extentDescriptor // its inline extents
	bs    int64              // allocation block size
	chunk int64              // bytes per chunk: min(block size, allocChunk)
	free  uint32             // free blocks, kept in step with the bitmap
	err   error              // first read/edit error; allocation fails with it

	cur    []byte // the last chunk read (a scan cache)
	curIdx int64  // its index, -1 when none
}

// newAllocator prepares the allocation bitmap of the volume for use. It reads
// nothing; the size and extents it will address are checked against the image
// first, so no later access is aimed outside it.
func (v *Volume) newAllocator() (*allocator, error) {
	fd := v.vh.AllocationFile
	f := newSpecialFork(v, fd)
	capacity := v.capacity()
	if f.size < 0 || f.size > maxForkBytes || f.size > f.extentBytes() || (capacity >= 0 && f.size > capacity) {
		return nil, fmt.Errorf("%w: allocation bitmap claims %d bytes, its extents cover %d", ErrCorrupt, f.size, f.extentBytes())
	}
	if capacity >= 0 {
		if err := f.checkExtentsInside(capacity); err != nil {
			return nil, fmt.Errorf("hfsplus: allocation bitmap: %w", err)
		}
	}
	bs := int64(v.vh.BlockSize)
	return &allocator{
		v:      v,
		bits:   v.vh.TotalBlocks,
		size:   f.size,
		exts:   f.extents,
		bs:     bs,
		chunk:  min(bs, allocChunk),
		free:   v.vh.FreeBlocks,
		curIdx: -1,
	}, nil
}

// chunkAt maps bitmap chunk idx to its absolute image offset and length.
func (a *allocator) chunkAt(idx int64) (abs int64, n int, ok bool) {
	logical := idx * a.chunk
	if logical >= a.size {
		return 0, 0, false
	}
	n = int(min(a.chunk, a.size-logical))
	var base int64
	for _, e := range a.exts {
		span := int64(e.BlockCount) * a.bs
		if logical < base+span {
			return int64(e.StartBlock)*a.bs + (logical - base), n, true
		}
		base += span
	}
	return 0, 0, false
}

// byteAt returns bitmap byte i, or false if it lies outside the bitmap or cannot
// be read (then a.err says why, if it was a read error).
func (a *allocator) byteAt(i int64) (byte, bool) {
	idx := i / a.chunk
	if a.curIdx != idx {
		abs, n, ok := a.chunkAt(idx)
		if !ok {
			return 0, false
		}
		buf := make([]byte, n)
		if got, err := a.v.rs.ReadAt(buf, abs); got < n {
			if a.err == nil {
				if err == nil {
					err = fmt.Errorf("short read")
				}
				a.err = fmt.Errorf("hfsplus: read allocation bitmap: %w", err)
			}
			return 0, false
		}
		a.cur, a.curIdx = buf, idx
	}
	in := i - idx*a.chunk
	if in >= int64(len(a.cur)) {
		return 0, false
	}
	return a.cur[in], true
}

// edit applies f to bitmap byte i inside the transaction's private copy of its
// chunk.
func (a *allocator) edit(i int64, f func(b byte) byte) {
	idx := i / a.chunk
	abs, n, ok := a.chunkAt(idx)
	if !ok {
		return
	}
	buf, err := a.v.region(abs, n)
	if err != nil {
		if a.err == nil {
			a.err = fmt.Errorf("hfsplus: allocation bitmap: %w", err)
		}
		return
	}
	buf[i-idx*a.chunk] = f(buf[i-idx*a.chunk])
	// The region is now the freshest copy of the chunk: scan from it.
	a.cur, a.curIdx = buf, idx
}

// test reports whether block n is marked used.
func (a *allocator) test(n uint32) bool {
	b, ok := a.byteAt(int64(n / 8))
	if !ok {
		return true // out of range or unreadable: treat as used (cannot allocate)
	}
	return b&(0x80>>(n%8)) != 0
}

func (a *allocator) set(n uint32) {
	if a.test(n) {
		return
	}
	a.edit(int64(n/8), func(b byte) byte { return b | 0x80>>(n%8) })
	if a.free > 0 {
		a.free--
	}
}

func (a *allocator) clear(n uint32) {
	if n >= a.bits || !a.test(n) {
		return
	}
	a.edit(int64(n/8), func(b byte) byte { return b &^ (0x80 >> (n % 8)) })
	a.free++
}

// skipUsed returns n advanced past the whole used bytes starting at n (n must be
// a multiple of 8), or n unchanged when the byte at n is not all ones.
func (a *allocator) skipUsed(n uint32) uint32 {
	if b, ok := a.byteAt(int64(n / 8)); ok && b == 0xFF {
		return n + 8
	}
	return n
}

// allocContiguous finds and marks a contiguous run of count free blocks,
// returning its start block. It searches from block 0; HFS+ does not require
// any particular placement for correctness. Returns an error if no run fits.
func (a *allocator) allocContiguous(count uint32) (uint32, error) {
	if count == 0 {
		return 0, nil
	}
	var run uint32
	var start uint32
	for n := uint32(0); n < a.bits; n++ {
		if n%8 == 0 && run == 0 {
			if m := a.skipUsed(n); m != n {
				n = m - 1
				continue
			}
		}
		if a.test(n) {
			run = 0
			continue
		}
		if run == 0 {
			start = n
		}
		run++
		if run == count {
			for i := start; i < start+count; i++ {
				a.set(i)
			}
			return start, a.err
		}
	}
	if a.err != nil {
		return 0, a.err
	}
	return 0, fmt.Errorf("%w: no contiguous run of %d blocks", ErrNoSpace, count)
}

// allocFragments allocates count free blocks, contiguously when a single run
// fits, otherwise as multiple fragments (in ascending block order). It marks
// every allocated block used and returns the runs. Returns ErrNoSpace if the
// volume lacks count free blocks in total.
func (a *allocator) allocFragments(count uint32) ([]extentDescriptor, error) {
	if count == 0 {
		return nil, nil
	}
	if start, err := a.allocContiguous(count); err == nil {
		return []extentDescriptor{{StartBlock: start, BlockCount: count}}, nil
	} else if a.err != nil {
		return nil, err
	}
	// Fall back to multiple fragments: walk free runs, taking each in turn.
	var runs []extentDescriptor
	remaining := count
	var runStart uint32
	var runLen uint32
	flush := func() {
		if runLen > 0 {
			for i := runStart; i < runStart+runLen; i++ {
				a.set(i)
			}
			runs = append(runs, extentDescriptor{StartBlock: runStart, BlockCount: runLen})
			remaining -= runLen
			runLen = 0
		}
	}
	for n := uint32(0); n < a.bits && remaining > 0; n++ {
		if n%8 == 0 && runLen == 0 {
			if m := a.skipUsed(n); m != n {
				n = m - 1
				continue
			}
		}
		if a.test(n) {
			flush()
			continue
		}
		if runLen == 0 {
			runStart = n
		}
		runLen++
		if runLen == remaining {
			flush()
			break
		}
	}
	flush()
	if a.err != nil {
		return nil, a.err
	}
	if remaining > 0 {
		// Not enough total free space: roll back what we took.
		for _, r := range runs {
			a.freeRun(r.StartBlock, r.BlockCount)
		}
		return nil, fmt.Errorf("%w: need %d blocks, short by %d", ErrNoSpace, count, remaining)
	}
	return runs, nil
}

// freeRun clears count blocks starting at start.
func (a *allocator) freeRun(start, count uint32) {
	for i := start; i < start+count; i++ {
		a.clear(i)
	}
}

// flush records the bitmap's free-block count in the volume header (primary and
// alternate, in the transaction's copies and in the parsed vh). The bitmap
// sectors themselves were edited in place as bits changed.
func (a *allocator) flush() error {
	if a.err != nil {
		return a.err
	}
	a.v.vh.FreeBlocks = a.free
	a.v.setHeaderFreeBlocks(a.free)
	return nil
}

// setHeaderFreeBlocks patches freeBlocks at offset 0x30 in both the primary and
// alternate volume headers.
func (v *Volume) setHeaderFreeBlocks(free uint32) {
	binary.BigEndian.PutUint32(v.hdr(false)[0x30:0x34], free)
	binary.BigEndian.PutUint32(v.hdr(true)[0x30:0x34], free)
}
