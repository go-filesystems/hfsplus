// Copyright (c) 2026, go-filesystems
// SPDX-License-Identifier: BSD-3-Clause

package hfsplus

import (
	"fmt"
	"io"
	"slices"
)

// fork is a logical byte stream addressed by a list of allocation-block
// extents over the volume. It models the catalog/extents special files and
// regular-file data forks uniformly. Extents are read in order; the logical
// size bounds reads.
type fork struct {
	vol     *Volume
	size    int64
	extents []extentDescriptor // resolved extents (inline + overflow), in order
}

// readAt reads len(p) bytes starting at logical offset off within the fork.
// It walks the extent list, mapping logical offsets to absolute byte offsets
// on the backing image. Reads past the logical size are short.
func (f *fork) readAt(p []byte, off int64) (int, error) {
	if off < 0 {
		return 0, fmt.Errorf("hfsplus: negative fork offset")
	}
	if off >= f.size {
		return 0, io.EOF
	}
	if int64(len(p)) > f.size-off {
		p = p[:f.size-off]
	}
	bs := int64(f.vol.vh.BlockSize)
	total := 0
	// blockBase tracks the logical byte offset at which the current extent
	// begins.
	var blockBase int64
	for _, ext := range f.extents {
		extLen := int64(ext.BlockCount) * bs
		if extLen == 0 {
			continue
		}
		extEnd := blockBase + extLen
		if off < extEnd {
			// Some of [off, off+len(p)) lies in this extent.
			within := off - blockBase
			abs := int64(ext.StartBlock)*bs + within
			n := extLen - within
			if n > int64(len(p)-total) {
				n = int64(len(p) - total)
			}
			if _, err := f.vol.rs.ReadAt(p[total:total+int(n)], abs); err != nil {
				return total, fmt.Errorf("hfsplus: fork read: %w", err)
			}
			total += int(n)
			off += n
			if total == len(p) {
				return total, nil
			}
		}
		blockBase = extEnd
	}
	if total < len(p) {
		// Logical size claimed more bytes than the extents cover.
		return total, io.ErrUnexpectedEOF
	}
	return total, nil
}

// maxForkBytes caps a single fork at 1 PiB. HFS+ logical sizes are uint64 and
// come straight off the disk; this keeps the int64 arithmetic below in range
// whatever the image claims.
const maxForkBytes = 1 << 50

// extentBytes is the number of bytes the fork's extents actually cover. It is
// the one bound on a fork's size that is not itself a number read off the disk,
// so it is what the claimed logical size gets checked against. The running
// total saturates rather than wrapping: BlockCount and BlockSize are both
// uint32 and their product over several extents can overflow an int64.
func (f *fork) extentBytes() int64 {
	bs := int64(f.vol.vh.BlockSize)
	var n int64
	for _, e := range f.extents {
		if int64(e.BlockCount) > (maxForkBytes-n)/bs {
			return maxForkBytes
		}
		n += int64(e.BlockCount) * bs
	}
	return n
}

// readAll returns the entire fork contents.
func (f *fork) readAll() ([]byte, error) {
	if f.size == 0 {
		return []byte{}, nil
	}
	// Allocate from the extents, never from the claimed size. A catalog record
	// saying a 12-byte file is 2^63 bytes long used to reach make() directly:
	// int64(uint64) of a hostile logicalSize is either enormous or negative,
	// and both panic with "makeslice: len out of range".
	if f.size < 0 || f.size > maxForkBytes || f.size > int64(^uint(0)>>1) {
		return nil, fmt.Errorf("%w: fork claims %d bytes", ErrCorrupt, f.size)
	}
	if covered := f.extentBytes(); f.size > covered {
		return nil, fmt.Errorf("%w: fork claims %d bytes but its extents cover only %d", ErrCorrupt, f.size, covered)
	}
	// The extents can themselves point past the end of the image, so covering
	// the size on paper is not enough: the last byte the fork claims has to be
	// readable before a single byte is allocated. One ReadAt -- the image
	// either has it or the fork is lying. Without this a 21-byte corruption of
	// the extent list made the driver spend 17 seconds zeroing a slice for a
	// 4 MiB image.
	var probe [1]byte
	if _, err := f.readAt(probe[:], f.size-1); err != nil {
		return nil, fmt.Errorf("%w: fork claims %d bytes, past the end of the image", ErrCorrupt, f.size)
	}
	capacity := f.vol.capacity()
	if capacity < 0 {
		// The image size is unknown, so nothing says the extents before the last
		// one exist: grow with the bytes the reader delivers.
		return f.readGrow()
	}
	// The last byte being readable proves only the LAST extent. An earlier
	// extent may lie outside the image (an 8-extent fork whose first extent
	// starts at block 2^30 passed the probe above and reserved its whole claimed
	// size), and extents may alias, so a fork can claim more bytes than the
	// volume holds. Both are refused before the buffer is made: the fork must
	// fit in the image and every extent it uses must lie inside it.
	if f.size > capacity {
		return nil, fmt.Errorf("%w: fork claims %d bytes, the image holds %d", ErrCorrupt, f.size, capacity)
	}
	if err := f.checkExtentsInside(capacity); err != nil {
		return nil, err
	}
	buf := make([]byte, f.size)
	n, err := f.readAt(buf, 0)
	if err != nil && err != io.EOF {
		return nil, err
	}
	return buf[:n], nil
}

// checkExtentsInside reports ErrCorrupt unless every extent that holds part of
// the fork's first f.size bytes lies inside an image of capacity bytes. No I/O.
func (f *fork) checkExtentsInside(capacity int64) error {
	bs := int64(f.vol.vh.BlockSize)
	var logical int64
	for _, e := range f.extents {
		if logical >= f.size {
			break
		}
		if e.BlockCount == 0 {
			continue
		}
		start, length := int64(e.StartBlock)*bs, int64(e.BlockCount)*bs
		if start > capacity-length {
			return fmt.Errorf("%w: extent [%d,+%d) lies outside the %d-byte image", ErrCorrupt, start, length, capacity)
		}
		logical += length
	}
	return nil
}

// growChunk is the first step of a read that cannot be checked against an image
// size; the buffer then doubles as the reader delivers.
const growChunk = 1 << 20

// readGrow reads the whole fork into a buffer that grows with the bytes
// actually delivered rather than being sized from the claimed length.
func (f *fork) readGrow() ([]byte, error) {
	buf := make([]byte, 0, min(f.size, growChunk))
	for int64(len(buf)) < f.size {
		step := min(f.size-int64(len(buf)), max(growChunk, int64(len(buf))))
		old := len(buf)
		buf = slices.Grow(buf, int(step))[:old+int(step)]
		n, err := f.readAt(buf[old:], int64(old))
		if err != nil && err != io.EOF {
			return nil, err
		}
		if int64(n) < step {
			return buf[:old+n], nil
		}
	}
	return buf, nil
}

// capacity returns the number of bytes the image holds, or -1 when unknown (Open
// with size <= 0 over a reader).
func (v *Volume) capacity() int64 {
	if v.img != nil {
		return int64(len(v.img))
	}
	if v.size > 0 {
		return v.size
	}
	return -1
}

// newSpecialFork builds a fork for a special file (catalog/extents/allocation)
// from the volume header's inline fork descriptor's eight inline extents only.
// Used during bootstrap (the extents-overflow tree is not yet open) and for
// special files that fit inline.
func newSpecialFork(vol *Volume, fd forkData) *fork {
	exts := make([]extentDescriptor, 0, numInlineExtents)
	for _, e := range fd.Extents {
		if e.BlockCount == 0 {
			continue
		}
		exts = append(exts, e)
	}
	return &fork{vol: vol, size: int64(fd.LogicalSize), extents: exts}
}

// newSpecialForkResolved builds a fork for a special file resolving its full
// extent list — the eight inline extents plus any continuation recorded in the
// extents-overflow B-tree (keyed by the special file's CNID). This is required
// once a special file (notably the catalog) grows past eight extents.
func newSpecialForkResolved(vol *Volume, fileID uint32, fd forkData) (*fork, error) {
	exts, err := vol.resolveForkExtents(fileID, forkTypeData, fd)
	if err != nil {
		return nil, err
	}
	return &fork{vol: vol, size: int64(fd.LogicalSize), extents: exts}, nil
}
