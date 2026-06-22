// Copyright (c) 2026, go-filesystems
// SPDX-License-Identifier: BSD-3-Clause

package hfsplus

import (
	"encoding/binary"
)

// Fork types used in extents-overflow keys.
const (
	forkTypeData = 0x00
	forkTypeRsrc = 0xFF
)

// extentsKey is a decoded HFSPlusExtentKey.
type extentsKey struct {
	forkType   uint8
	fileID     uint32
	startBlock uint32
}

// parseExtentsKey decodes the key bytes (excluding the length prefix).
func parseExtentsKey(keyBytes []byte) (extentsKey, bool) {
	if len(keyBytes) < 10 {
		return extentsKey{}, false
	}
	return extentsKey{
		forkType:   keyBytes[0],
		fileID:     binary.BigEndian.Uint32(keyBytes[2:6]),
		startBlock: binary.BigEndian.Uint32(keyBytes[6:10]),
	}, true
}

// compareExtentsKey orders extents-overflow keys: fileID, then forkType, then
// startBlock — matching the on-disk B-tree ordering.
func compareExtentsKey(a, b extentsKey) int {
	if a.fileID != b.fileID {
		if a.fileID < b.fileID {
			return -1
		}
		return 1
	}
	if a.forkType != b.forkType {
		if a.forkType < b.forkType {
			return -1
		}
		return 1
	}
	if a.startBlock != b.startBlock {
		if a.startBlock < b.startBlock {
			return -1
		}
		return 1
	}
	return 0
}

// findExtentsLeaf descends the extents-overflow tree to the leaf that would
// contain key.
func (t *btree) findExtentsLeaf(key extentsKey) (uint32, error) {
	cur := t.header.RootNode
	if cur == 0 {
		return t.header.FirstLeaf, nil
	}
	for depth := 0; depth < 64; depth++ {
		nd, err := t.readNode(cur)
		if err != nil {
			return 0, err
		}
		if nd.desc.Kind == kindLeafNode {
			return cur, nil
		}
		if nd.desc.Kind != kindIndexNode {
			return 0, ErrCorrupt
		}
		var child uint32
		chosen := false
		for _, rec := range nd.records {
			kl, ks, ok := recordKeyLen(rec)
			if !ok {
				continue
			}
			k, ok := parseExtentsKey(rec[ks : ks+kl])
			if !ok {
				continue
			}
			if compareExtentsKey(k, key) <= 0 {
				if c, ok := indexChild(rec); ok {
					child = c
					chosen = true
				}
			} else {
				break
			}
		}
		if !chosen {
			if len(nd.records) == 0 {
				return 0, ErrCorrupt
			}
			c, ok := indexChild(nd.records[0])
			if !ok {
				return 0, ErrCorrupt
			}
			child = c
		}
		cur = child
	}
	return 0, ErrCorrupt
}

// overflowExtents returns the extents recorded in the extents-overflow tree for
// (fileID, forkType) starting at allocation block startBlock, in order. An
// empty result means the fork is wholly described by its inline extents.
func (t *btree) overflowExtents(fileID uint32, forkType uint8, startBlock uint32) ([]extentDescriptor, error) {
	key := extentsKey{forkType: forkType, fileID: fileID, startBlock: startBlock}
	leaf, err := t.findExtentsLeaf(key)
	if err != nil {
		return nil, err
	}
	var out []extentDescriptor
	for leaf != 0 {
		nd, err := t.readNode(leaf)
		if err != nil {
			return nil, err
		}
		for _, rec := range nd.records {
			kl, ks, ok := recordKeyLen(rec)
			if !ok {
				continue
			}
			k, ok := parseExtentsKey(rec[ks : ks+kl])
			if !ok {
				continue
			}
			if k.fileID != fileID || k.forkType != forkType {
				if k.fileID > fileID {
					return out, nil
				}
				continue
			}
			if k.startBlock < startBlock {
				continue
			}
			data, ok := recordData(rec)
			if !ok {
				continue
			}
			for i := 0; i+8 <= len(data) && i < numInlineExtents*8; i += 8 {
				ed := extentDescriptor{
					StartBlock: binary.BigEndian.Uint32(data[i : i+4]),
					BlockCount: binary.BigEndian.Uint32(data[i+4 : i+8]),
				}
				if ed.BlockCount == 0 {
					break
				}
				out = append(out, ed)
			}
		}
		leaf = nd.desc.FLink
	}
	return out, nil
}

// resolveForkExtents assembles the full ordered extent list for a fork: the up
// to eight inline extents, then any continuation runs from the
// extents-overflow tree. fileID and forkType identify the fork in the overflow
// tree.
func (v *Volume) resolveForkExtents(fileID uint32, forkType uint8, fd forkData) ([]extentDescriptor, error) {
	exts := make([]extentDescriptor, 0, numInlineExtents)
	var blocksCovered uint32
	for _, e := range fd.Extents {
		if e.BlockCount == 0 {
			break
		}
		exts = append(exts, e)
		blocksCovered += e.BlockCount
	}
	// If the inline extents already cover the fork's allocation blocks, done.
	if blocksCovered >= fd.TotalBlocks || v.extentsTree == nil {
		return exts, nil
	}
	more, err := v.extentsTree.overflowExtents(fileID, forkType, blocksCovered)
	if err != nil {
		return nil, err
	}
	for _, e := range more {
		exts = append(exts, e)
		blocksCovered += e.BlockCount
		if blocksCovered >= fd.TotalBlocks {
			break
		}
	}
	return exts, nil
}
