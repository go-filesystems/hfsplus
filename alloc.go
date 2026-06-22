// Copyright (c) 2026, go-filesystems
// SPDX-License-Identifier: BSD-3-Clause

package hfsplus

import (
	"encoding/binary"
	"fmt"

	"github.com/go-volumes/safeio"
)

// alloc.go manages the HFS+ allocation bitmap (the allocation file). Each bit
// maps one allocation block: the most-significant bit of byte 0 is block 0.
// Allocation/free operations edit the bitmap bytes inside the in-memory image
// and keep the volume header's freeBlocks count in sync. All access goes
// through allocator, which resolves the allocation file's extents from the
// volume header.

// allocator provides block allocate/free over a writable volume's bitmap.
type allocator struct {
	v        *Volume
	bitmap   []byte // a copy of the allocation file's logical contents
	bits     uint32 // total allocation blocks
	forkSpan int64  // byte span of the allocation file on disk (its extents)
}

// newAllocator loads the allocation bitmap for the volume into memory.
func (v *Volume) newAllocator() (*allocator, error) {
	fd := v.vh.AllocationFile
	f := newSpecialFork(v, fd)
	// Bound the bitmap allocation against the image size (safeio class A): the
	// bitmap can never exceed the image it describes.
	if _, err := safeio.MakeBytes(f.size, int64(len(v.img))+int64(v.vh.BlockSize)); err != nil {
		return nil, fmt.Errorf("hfsplus: allocation bitmap size %d: %w", f.size, err)
	}
	raw, err := f.readAll()
	if err != nil {
		return nil, fmt.Errorf("hfsplus: read allocation bitmap: %w", err)
	}
	a := &allocator{v: v, bitmap: raw, bits: v.vh.TotalBlocks}
	return a, nil
}

// test reports whether block n is marked used.
func (a *allocator) test(n uint32) bool {
	if n/8 >= uint32(len(a.bitmap)) {
		return true // out of range: treat as used (cannot allocate)
	}
	return a.bitmap[n/8]&(0x80>>(n%8)) != 0
}

func (a *allocator) set(n uint32) { a.bitmap[n/8] |= 0x80 >> (n % 8) }
func (a *allocator) clear(n uint32) {
	if n/8 < uint32(len(a.bitmap)) {
		a.bitmap[n/8] &^= 0x80 >> (n % 8)
	}
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
			return start, nil
		}
	}
	return 0, fmt.Errorf("%w: no contiguous run of %d blocks", ErrNoSpace, count)
}

// freeRun clears count blocks starting at start.
func (a *allocator) freeRun(start, count uint32) {
	for i := start; i < start+count; i++ {
		a.clear(i)
	}
}

// freeCount returns the number of unused blocks per the in-memory bitmap.
func (a *allocator) freeCount() uint32 {
	var free uint32
	for n := uint32(0); n < a.bits; n++ {
		if !a.test(n) {
			free++
		}
	}
	return free
}

// flush writes the bitmap back into the image (over the allocation file's
// extents) and updates the volume header's freeBlocks field both in the image
// and in the in-memory vh.
func (a *allocator) flush() error {
	fd := a.v.vh.AllocationFile
	// Write the bitmap bytes across the allocation file's extents.
	bs := int64(a.v.vh.BlockSize)
	var written int
	for _, e := range fd.Extents {
		if e.BlockCount == 0 {
			break
		}
		dst := int64(e.StartBlock) * bs
		n := int(int64(e.BlockCount) * bs)
		if written+n > len(a.bitmap) {
			n = len(a.bitmap) - written
		}
		copy(a.v.img[dst:dst+int64(n)], a.bitmap[written:written+n])
		written += n
		if written >= len(a.bitmap) {
			break
		}
	}
	free := a.freeCount()
	a.v.vh.FreeBlocks = free
	a.v.setHeaderFreeBlocks(free)
	return nil
}

// setHeaderFreeBlocks patches freeBlocks at offset 0x30 in both the primary and
// alternate volume headers in the image.
func (v *Volume) setHeaderFreeBlocks(free uint32) {
	binary.BigEndian.PutUint32(v.img[volumeHeaderOffset+0x30:volumeHeaderOffset+0x34], free)
	alt := int64(len(v.img)) - 1024
	binary.BigEndian.PutUint32(v.img[alt+0x30:alt+0x34], free)
}
