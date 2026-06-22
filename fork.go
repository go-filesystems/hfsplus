// Copyright (c) 2026, go-filesystems
// SPDX-License-Identifier: BSD-3-Clause

package hfsplus

import (
	"fmt"
	"io"
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

// readAll returns the entire fork contents.
func (f *fork) readAll() ([]byte, error) {
	if f.size == 0 {
		return []byte{}, nil
	}
	buf := make([]byte, f.size)
	n, err := f.readAt(buf, 0)
	if err != nil && err != io.EOF {
		return nil, err
	}
	return buf[:n], nil
}

// newSpecialFork builds a fork for a special file (catalog/extents/allocation)
// from the volume header's inline fork descriptor. Special files in practice
// fit within their eight inline extents for the small images this reader
// targets; if a special file were fragmented beyond that the extents-overflow
// would also be needed, which we note but do not chase for special files.
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
