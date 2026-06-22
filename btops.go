// Copyright (c) 2026, go-filesystems
// SPDX-License-Identifier: BSD-3-Clause

package hfsplus

import (
	"fmt"
	"sort"
)

// btops.go holds the generic B-tree insert/delete/split/merge machinery shared
// by the catalog and extents-overflow trees, operating through btreeWriter and
// its treeOps adapter. Insertion splits overflowing leaves/index nodes and
// grows a new root; deletion removes the record and, when a node falls below
// the fill threshold, rebalances (rotates) with a sibling or merges nodes,
// propagating index-key updates upward, freeing emptied nodes, and shrinking
// the tree height when the root keeps a single child.

// descend returns the node numbers from the root down to the leaf that would
// contain a record ordered like probe (by ops.compareRecords against index
// keys). It also returns, for each level, the index of the chosen child record
// within its parent (childIdx[k] is the record index in path[k] that points to
// path[k+1]); childIdx has len(path)-1 entries.
func (bw *btreeWriter) descend(probe []byte) (path []uint32, childIdx []int, err error) {
	cur := bw.rootNode()
	if cur == 0 {
		return nil, nil, fmt.Errorf("%w: descend on empty tree", ErrCorrupt)
	}
	for depth := 0; depth < 64; depth++ {
		en, err := bw.loadNode(cur)
		if err != nil {
			return nil, nil, err
		}
		path = append(path, cur)
		if en.desc.Kind == kindLeafNode {
			return path, childIdx, nil
		}
		if en.desc.Kind != kindIndexNode {
			return nil, nil, fmt.Errorf("%w: descend hit kind %d", ErrCorrupt, en.desc.Kind)
		}
		if len(en.records) == 0 {
			return nil, nil, fmt.Errorf("%w: descend empty index node %d", ErrCorrupt, cur)
		}
		chosen := -1
		for i, r := range en.records {
			if bw.ops.compareRecords(r, probe) <= 0 {
				chosen = i
			} else {
				break
			}
		}
		if chosen < 0 {
			chosen = 0
		}
		c, ok := indexChild(en.records[chosen])
		if !ok {
			return nil, nil, fmt.Errorf("%w: descend bad index child node %d rec %d", ErrCorrupt, cur, chosen)
		}
		childIdx = append(childIdx, chosen)
		cur = c
	}
	return nil, nil, ErrCorrupt
}

// insert adds a leaf record (caller guarantees the key is absent), splitting and
// growing the tree as needed.
func (bw *btreeWriter) insert(rec []byte) error {
	if bw.rootNode() == 0 {
		leaf, err := bw.allocNode()
		if err != nil {
			return err
		}
		en := &editNode{num: leaf, desc: nodeDescriptor{Kind: kindLeafNode, Height: 1}, records: [][]byte{rec}}
		if err := bw.storeNode(en); err != nil {
			return err
		}
		bw.setRootNode(leaf)
		bw.setFirstLeaf(leaf)
		bw.setLastLeaf(leaf)
		bw.setTreeDepth(1)
		bw.setLeafRecords(1)
		return nil
	}
	path, _, err := bw.descend(rec)
	if err != nil {
		return err
	}
	splitKey, splitChild, err := bw.insertIntoLeaf(path[len(path)-1], rec)
	if err != nil {
		return err
	}
	bw.setLeafRecords(bw.leafRecords() + 1)
	if splitChild == 0 {
		return nil
	}
	if len(path) == 1 {
		return bw.growRoot(path[0], splitKey, splitChild)
	}
	return bw.propagateSplit(path, splitKey, splitChild)
}

func (bw *btreeWriter) insertIntoLeaf(n uint32, rec []byte) ([]byte, uint32, error) {
	en, err := bw.loadNode(n)
	if err != nil {
		return nil, 0, err
	}
	bw.insertSorted(en, rec)
	if bw.recordsFit(en) {
		return nil, 0, bw.storeNode(en)
	}
	return bw.splitLeaf(en)
}

// insertSorted inserts rec keeping key order (by ops.compareRecords).
func (bw *btreeWriter) insertSorted(en *editNode, rec []byte) {
	idx := sort.Search(len(en.records), func(i int) bool {
		return bw.ops.compareRecords(en.records[i], rec) >= 0
	})
	en.records = append(en.records, nil)
	copy(en.records[idx+1:], en.records[idx:])
	en.records[idx] = rec
}

func (bw *btreeWriter) splitLeaf(en *editNode) ([]byte, uint32, error) {
	right, err := bw.allocNode()
	if err != nil {
		return nil, 0, err
	}
	mid := len(en.records) / 2
	rightRecs := append([][]byte(nil), en.records[mid:]...)
	en.records = en.records[:mid]
	oldF := en.desc.FLink
	rightEN := &editNode{
		num:     right,
		desc:    nodeDescriptor{Kind: kindLeafNode, Height: 1, FLink: oldF, BLink: en.num},
		records: rightRecs,
	}
	en.desc.FLink = right
	if oldF != 0 {
		of, err := bw.loadNode(oldF)
		if err != nil {
			return nil, 0, fmt.Errorf("splitLeaf: load oldF %d: %w", oldF, err)
		}
		of.desc.BLink = right
		if err := bw.storeNode(of); err != nil {
			return nil, 0, err
		}
	} else {
		bw.setLastLeaf(right)
	}
	if err := bw.storeNode(en); err != nil {
		return nil, 0, err
	}
	if err := bw.storeNode(rightEN); err != nil {
		return nil, 0, err
	}
	return keyBytesOf(rightRecs[0]), right, nil
}

func (bw *btreeWriter) propagateSplit(path []uint32, splitKey []byte, splitChild uint32) error {
	for level := len(path) - 2; level >= 0; level-- {
		parent := path[level]
		en, err := bw.loadNode(parent)
		if err != nil {
			return err
		}
		idxRec := bw.ops.indexRecord(splitKey, splitChild)
		bw.insertSorted(en, idxRec)
		if bw.recordsFit(en) {
			return bw.storeNode(en)
		}
		nk, nc, err := bw.splitIndex(en)
		if err != nil {
			return err
		}
		splitKey, splitChild = nk, nc
		if level == 0 {
			return bw.growRoot(path[0], splitKey, splitChild)
		}
	}
	return nil
}

func (bw *btreeWriter) splitIndex(en *editNode) ([]byte, uint32, error) {
	right, err := bw.allocNode()
	if err != nil {
		return nil, 0, err
	}
	mid := len(en.records) / 2
	rightRecs := append([][]byte(nil), en.records[mid:]...)
	en.records = en.records[:mid]
	oldF := en.desc.FLink
	rightEN := &editNode{
		num:     right,
		desc:    nodeDescriptor{Kind: kindIndexNode, Height: en.desc.Height, FLink: oldF, BLink: en.num},
		records: rightRecs,
	}
	en.desc.FLink = right
	// Splice `right` into the index-node sibling chain: the node that used to
	// follow `en` must now point back to `right`, otherwise fsck_hfs reports an
	// "Invalid sibling link" for that index level (the same chain leaf nodes
	// maintain).
	if oldF != 0 {
		of, err := bw.loadNode(oldF)
		if err != nil {
			return nil, 0, fmt.Errorf("splitIndex: load oldF %d: %w", oldF, err)
		}
		of.desc.BLink = right
		if err := bw.storeNode(of); err != nil {
			return nil, 0, err
		}
	}
	if err := bw.storeNode(en); err != nil {
		return nil, 0, err
	}
	if err := bw.storeNode(rightEN); err != nil {
		return nil, 0, err
	}
	return keyBytesOf(rightRecs[0]), right, nil
}

func (bw *btreeWriter) growRoot(oldRoot uint32, splitKey []byte, splitChild uint32) error {
	newRoot, err := bw.allocNode()
	if err != nil {
		return err
	}
	oldEN, err := bw.loadNode(oldRoot)
	if err != nil {
		return err
	}
	oldFirstKey := keyBytesOf(oldEN.records[0])
	rec0 := bw.ops.indexRecord(oldFirstKey, oldRoot)
	rec1 := bw.ops.indexRecord(splitKey, splitChild)
	en := &editNode{
		num:     newRoot,
		desc:    nodeDescriptor{Kind: kindIndexNode, Height: uint8(bw.treeDepth() + 1)},
		records: [][]byte{rec0, rec1},
	}
	if err := bw.storeNode(en); err != nil {
		return err
	}
	bw.setRootNode(newRoot)
	bw.setTreeDepth(bw.treeDepth() + 1)
	return nil
}
