// Copyright (c) 2026, go-filesystems
// SPDX-License-Identifier: BSD-3-Clause

package hfsplus

import (
	"encoding/binary"
	"fmt"
	"sort"
)

// extwrite.go is the extents-overflow B-tree write engine. It inserts
// HFSPlusExtentRecord continuations (keyed by forkType+fileID+startBlock, data
// = up to eight extent descriptors) for forks whose extents spill past the
// eight inline descriptors, and removes them when a fork is deleted/truncated.
// It is driven by the shared generic btreeWriter (btwrite.go / btgrow.go).

// extentsOps adapts the generic engine to extents-overflow keys.
type extentsOps struct{}

func (extentsOps) compareRecords(a, b []byte) int {
	ka, oka := extentsKeyOfRecord(a)
	kb, okb := extentsKeyOfRecord(b)
	if !oka || !okb {
		return 0
	}
	return compareExtentsKey(ka, kb)
}

func (extentsOps) indexRecord(firstKey []byte, child uint32) []byte {
	return indexRecordFor(firstKey, child)
}

// extentsKeyOfRecord decodes the extents key from a leaf/index record.
func extentsKeyOfRecord(rec []byte) (extentsKey, bool) {
	kl, ks, ok := recordKeyLen(rec)
	if !ok {
		return extentsKey{}, false
	}
	return parseExtentsKey(rec[ks : ks+kl])
}

// encodeExtentsKey encodes an HFSPlusExtentKey: keyLength(2)=10, forkType(1),
// pad(1), fileID(4), startBlock(4).
func encodeExtentsKey(forkType uint8, fileID, startBlock uint32) []byte {
	buf := make([]byte, 12)
	binary.BigEndian.PutUint16(buf[0:2], 10)
	buf[2] = forkType
	buf[3] = 0
	binary.BigEndian.PutUint32(buf[4:8], fileID)
	binary.BigEndian.PutUint32(buf[8:12], startBlock)
	return buf
}

// encodeExtentRecord encodes an HFSPlusExtentRecord: eight (startBlock,
// blockCount) descriptors (64 bytes), zero-padded past the supplied extents.
func encodeExtentRecord(exts []extentDescriptor) []byte {
	buf := make([]byte, numInlineExtents*8)
	for i := 0; i < numInlineExtents && i < len(exts); i++ {
		binary.BigEndian.PutUint32(buf[i*8:i*8+4], exts[i].StartBlock)
		binary.BigEndian.PutUint32(buf[i*8+4:i*8+8], exts[i].BlockCount)
	}
	return buf
}

// extentsWriter wraps the generic B-tree writer for the extents-overflow tree.
type extentsWriter struct {
	bw *btreeWriter
}

// newExtentsWriter builds an extents-overflow tree writer over the volume's
// extents file, sharing the allocation bitmap.
func (v *Volume) newExtentsWriter(alc *allocator) (*extentsWriter, error) {
	if v.extentsTree == nil {
		return nil, fmt.Errorf("%w: extents tree not opened", ErrCorrupt)
	}
	bw, err := v.newBTreeWriter(v.extentsTree, forkExtentsFile, extentsOps{}, alc)
	if err != nil {
		return nil, err
	}
	return &extentsWriter{bw: bw}, nil
}

// insertExtentRecords inserts the continuation extents for (fileID, forkType)
// starting at fork-relative allocation block startBlock, splitting them into
// HFSPlusExtentRecord groups of eight and keying each group by its starting
// block. Existing records for the same keys are replaced.
func (ew *extentsWriter) insertExtentRecords(fileID uint32, forkType uint8, startBlock uint32, exts []extentDescriptor) error {
	block := startBlock
	for i := 0; i < len(exts); i += numInlineExtents {
		end := i + numInlineExtents
		if end > len(exts) {
			end = len(exts)
		}
		group := exts[i:end]
		key := encodeExtentsKey(forkType, fileID, block)
		rec := assembleRecord(key, encodeExtentRecord(group))
		if err := ew.insertOrReplace(rec); err != nil {
			return err
		}
		for _, e := range group {
			block += e.BlockCount
		}
	}
	return nil
}

// assembleRecord concatenates an encoded key (with length prefix) and data,
// padding to a 2-byte boundary.
func assembleRecord(key, data []byte) []byte {
	rec := make([]byte, 0, len(key)+len(data)+1)
	rec = append(rec, key...)
	if len(rec)%2 != 0 {
		rec = append(rec, 0)
	}
	rec = append(rec, data...)
	return rec
}

// insertOrReplace inserts rec, replacing any existing record with the same key.
func (ew *extentsWriter) insertOrReplace(rec []byte) error {
	key, ok := extentsKeyOfRecord(rec)
	if !ok {
		return fmt.Errorf("%w: bad extents record", ErrCorrupt)
	}
	if err := ew.deleteKey(key); err != nil {
		return err
	}
	return ew.bw.insert(rec)
}

// deleteAllForFork removes every extents-overflow record belonging to
// (fileID, forkType). Returns the extents that were recorded (for block
// reclamation by the caller).
func (ew *extentsWriter) deleteAllForFork(fileID uint32, forkType uint8) ([]extentDescriptor, error) {
	bw := ew.bw
	if bw.rootNode() == 0 {
		return nil, nil
	}
	var freed []extentDescriptor
	var keys []extentsKey
	// Collect all matching keys + their extents by scanning leaves.
	leaf := bw.firstLeaf()
	for leaf != 0 {
		en, err := bw.loadNode(leaf)
		if err != nil {
			return nil, err
		}
		next := en.desc.FLink
		for _, rec := range en.records {
			k, ok := extentsKeyOfRecord(rec)
			if !ok {
				continue
			}
			if k.fileID != fileID || k.forkType != forkType {
				continue
			}
			keys = append(keys, k)
			data, ok := recordData(rec)
			if ok {
				for i := 0; i+8 <= len(data); i += 8 {
					ed := extentDescriptor{
						StartBlock: binary.BigEndian.Uint32(data[i : i+4]),
						BlockCount: binary.BigEndian.Uint32(data[i+4 : i+8]),
					}
					if ed.BlockCount == 0 {
						break
					}
					freed = append(freed, ed)
				}
			}
		}
		leaf = next
	}
	// Order keys ascending and delete (delete may restructure the tree).
	sort.Slice(keys, func(i, j int) bool { return compareExtentsKey(keys[i], keys[j]) < 0 })
	for _, k := range keys {
		if err := ew.deleteKey(k); err != nil {
			return nil, err
		}
	}
	return freed, nil
}

// deleteKey removes the extents record matching key exactly, if present.
func (ew *extentsWriter) deleteKey(key extentsKey) error {
	return ew.bw.deleteByKey(
		encodeExtentsKey(key.forkType, key.fileID, key.startBlock),
		func(rec []byte) bool {
			k, ok := extentsKeyOfRecord(rec)
			return ok && compareExtentsKey(k, key) == 0
		},
	)
}
