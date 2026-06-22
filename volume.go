// Copyright (c) 2026, go-filesystems
// SPDX-License-Identifier: BSD-3-Clause

package hfsplus

import (
	"encoding/binary"
	"fmt"
	"io"
)

// volumeHeaderOffset is the fixed byte offset of the HFS+ volume header. The
// first 1024 bytes are reserved (boot blocks).
const volumeHeaderOffset = 1024

// HFS+/HFSX volume signatures (big-endian on disk).
const (
	sigHFSPlus = 0x482B // "H+"
	sigHFSX    = 0x4858 // "HX"
)

// extentDescriptor is one (startBlock, blockCount) pair addressing a run of
// allocation blocks. HFS+ stores eight inline per fork.
type extentDescriptor struct {
	StartBlock uint32
	BlockCount uint32
}

// numInlineExtents is the count of extent descriptors stored inline in a fork.
const numInlineExtents = 8

// forkData describes one fork (data or resource) of a special or catalog file:
// its logical size plus the first eight extents. Beyond these, fragmented
// forks continue in the extents-overflow B-tree.
type forkData struct {
	LogicalSize uint64
	ClumpSize   uint32
	TotalBlocks uint32
	Extents     [numInlineExtents]extentDescriptor
}

// forkDataLen is the on-disk size of a HFSPlusForkData record.
const forkDataLen = 8 + 4 + 4 + numInlineExtents*8

// parseForkData decodes a HFSPlusForkData from b (must be >= forkDataLen).
func parseForkData(b []byte) forkData {
	var f forkData
	f.LogicalSize = binary.BigEndian.Uint64(b[0:8])
	f.ClumpSize = binary.BigEndian.Uint32(b[8:12])
	f.TotalBlocks = binary.BigEndian.Uint32(b[12:16])
	off := 16
	for i := 0; i < numInlineExtents; i++ {
		f.Extents[i].StartBlock = binary.BigEndian.Uint32(b[off : off+4])
		f.Extents[i].BlockCount = binary.BigEndian.Uint32(b[off+4 : off+8])
		off += 8
	}
	return f
}

// volumeHeader is the decoded HFS+ volume header (a subset of the full
// structure: the fields the reader needs).
type volumeHeader struct {
	Signature   uint16
	Version     uint16
	Attributes  uint32
	BlockSize   uint32
	TotalBlocks uint32
	FreeBlocks  uint32

	AllocationFile forkData
	ExtentsFile    forkData
	CatalogFile    forkData
	AttributesFile forkData
	StartupFile    forkData

	caseSensitive bool // HFSX with a case-sensitive key-compare type
}

// readVolumeHeader reads and validates the volume header at offset 1024.
func readVolumeHeader(rs io.ReaderAt) (*volumeHeader, error) {
	buf := make([]byte, 512)
	if _, err := rs.ReadAt(buf, volumeHeaderOffset); err != nil {
		return nil, fmt.Errorf("hfsplus: read volume header: %w", err)
	}
	vh := &volumeHeader{
		Signature:  binary.BigEndian.Uint16(buf[0:2]),
		Version:    binary.BigEndian.Uint16(buf[2:4]),
		Attributes: binary.BigEndian.Uint32(buf[4:8]),
	}
	if vh.Signature != sigHFSPlus && vh.Signature != sigHFSX {
		return nil, ErrBadHeader
	}
	// Layout (offsets within the 512-byte header):
	//   0x28 blockSize, 0x2C totalBlocks, 0x30 freeBlocks
	//   special files start at 0x70: allocation, extents, catalog,
	//   attributes, startup — each a 80-byte HFSPlusForkData.
	vh.BlockSize = binary.BigEndian.Uint32(buf[0x28:0x2C])
	vh.TotalBlocks = binary.BigEndian.Uint32(buf[0x2C:0x30])
	vh.FreeBlocks = binary.BigEndian.Uint32(buf[0x30:0x34])

	if vh.BlockSize < 512 || vh.BlockSize&(vh.BlockSize-1) != 0 {
		return nil, fmt.Errorf("%w: block size %d", ErrCorrupt, vh.BlockSize)
	}

	const specialBase = 0x70
	vh.AllocationFile = parseForkData(buf[specialBase+0*forkDataLen:])
	vh.ExtentsFile = parseForkData(buf[specialBase+1*forkDataLen:])
	vh.CatalogFile = parseForkData(buf[specialBase+2*forkDataLen:])
	vh.AttributesFile = parseForkData(buf[specialBase+3*forkDataLen:])
	vh.StartupFile = parseForkData(buf[specialBase+4*forkDataLen:])

	return vh, nil
}
