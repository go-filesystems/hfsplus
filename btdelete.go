// Copyright (c) 2026, go-filesystems
// SPDX-License-Identifier: BSD-3-Clause

package hfsplus

import (
	"fmt"
)

// btdelete.go implements HFS+ B-tree deletion with node-underflow handling:
// after removing the record, an underflowing leaf or index node rebalances by
// rotating a record from a sibling, or merges with a sibling when neither can
// spare one. Index keys are kept consistent at every level, emptied nodes are
// freed back to the node bitmap (freeNodes++), and the tree height shrinks when
// the root collapses to a single child. This keeps the tree fsck-clean (no
// "out of order", "invalid node", or "overlapped" complaints) and storage-tight
// under heavy create/delete churn.

// deleteByKey removes the leaf record for which match returns true. probeKey is
// an encoded key used only to descend to the right leaf. Returns nil whether or
// not the record was present.
func (bw *btreeWriter) deleteByKey(probeKey []byte, match func(rec []byte) bool) error {
	if bw.rootNode() == 0 {
		return nil
	}
	probe := assembleProbe(probeKey)
	path, childIdx, err := bw.descend(probe)
	if err != nil {
		return err
	}
	leafNum := path[len(path)-1]
	en, err := bw.loadNode(leafNum)
	if err != nil {
		return err
	}
	idx := -1
	for i, r := range en.records {
		if match(r) {
			idx = i
			break
		}
	}
	if idx < 0 {
		return nil
	}
	en.records = append(en.records[:idx], en.records[idx+1:]...)
	bw.setLeafRecords(bw.leafRecords() - 1)
	if err := bw.storeNode(en); err != nil {
		return err
	}
	// If the leaf is the root, handle the empty-root case and stop.
	if len(path) == 1 {
		if len(en.records) == 0 {
			if err := bw.freeNode(leafNum); err != nil {
				return err
			}
			bw.setRootNode(0)
			bw.setFirstLeaf(0)
			bw.setLastLeaf(0)
			bw.setTreeDepth(0)
		}
		return nil
	}
	// Fix the parent index key if we removed the first record (the key that
	// represents this node in its parent changed).
	if idx == 0 && len(en.records) > 0 {
		if err := bw.updateParentKey(path, childIdx, len(path)-1); err != nil {
			return err
		}
	}
	// Rebalance if the leaf underflows.
	return bw.rebalance(path, childIdx, len(path)-1)
}

// assembleProbe wraps an encoded key into a record-shaped buffer (key only) so
// compareRecords can read its key. The data portion is irrelevant for ordering.
func assembleProbe(key []byte) []byte {
	rec := append([]byte(nil), key...)
	if len(rec)%2 != 0 {
		rec = append(rec, 0)
	}
	return rec
}

// minFill is the underflow threshold: a node must keep at least this many bytes
// used (excluding the header-node overheads). HFS+ does not mandate a specific
// fill factor for correctness, but rebalancing toward half-full keeps the tree
// compact and avoids long chains of nearly-empty nodes that slow fsck.
func (bw *btreeWriter) minFill() int { return bw.nodeSize / 2 }

// underflows reports whether en is below the fill threshold.
func (bw *btreeWriter) underflows(en *editNode) bool {
	return bw.usedBytes(en) < bw.minFill()
}

// rebalance fixes an underflowing node at path[level], rotating from or merging
// with a sibling, then recurses upward if the parent underflows or the root
// collapses. childIdx[level-1] is en's index within its parent.
func (bw *btreeWriter) rebalance(path []uint32, childIdx []int, level int) error {
	if level == 0 {
		return bw.maybeShrinkRoot()
	}
	en, err := bw.loadNode(path[level])
	if err != nil {
		return err
	}
	if !bw.underflows(en) || len(en.records) == 0 && level == 0 {
		// No underflow (or root): nothing to do here, but still check height.
		if !bw.underflows(en) {
			return nil
		}
	}
	parentNum := path[level-1]
	myIdx := childIdx[level-1]
	parent, err := bw.loadNode(parentNum)
	if err != nil {
		return err
	}
	var left, right *editNode
	if myIdx > 0 {
		ln, _ := indexChild(parent.records[myIdx-1])
		if left, err = bw.loadNode(ln); err != nil {
			return fmt.Errorf("rebalance load left sibling %d (parent %d idx %d): %w", ln, parentNum, myIdx-1, err)
		}
	}
	if myIdx < len(parent.records)-1 {
		rn, _ := indexChild(parent.records[myIdx+1])
		if right, err = bw.loadNode(rn); err != nil {
			return fmt.Errorf("rebalance load right sibling %d (parent %d idx %d): %w", rn, parentNum, myIdx+1, err)
		}
	}
	// Prefer merging when it fits (keeps the tree compact); otherwise rotate a
	// record from a sibling that can spare one. Merge is always safe when the
	// combined records fit a node.
	if left != nil && bw.combinedFits(left, en) {
		return bw.merge(path, childIdx, level, parent, left, en, myIdx-1)
	}
	if right != nil && bw.combinedFits(en, right) {
		return bw.merge(path, childIdx, level, parent, en, right, myIdx)
	}
	// Cannot merge: rotate from whichever sibling can spare a record.
	if left != nil && bw.canSpare(left, en) {
		return bw.rotateFromLeft(path, childIdx, level, parent, left, en)
	}
	if right != nil && bw.canSpare(right, en) {
		return bw.rotateFromRight(path, childIdx, level, parent, en, right)
	}
	// No sibling can help (parent has a single child); root-shrink handles it.
	return bw.maybeShrinkRoot()
}

// combinedFits reports whether all of a's and b's records fit in one node.
func (bw *btreeWriter) combinedFits(a, b *editNode) bool {
	used := nodeDescriptorLen + 2*(len(a.records)+len(b.records)+1)
	for _, r := range a.records {
		used += len(r)
	}
	for _, r := range b.records {
		used += len(r)
	}
	return used <= bw.nodeSize
}

// canSpare reports whether donor can give its boundary record to recv and still
// leave both nodes within the node size.
func (bw *btreeWriter) canSpare(donor, recv *editNode) bool {
	if len(donor.records) <= 1 {
		return false
	}
	// recv gains the boundary record; donor loses it. Check recv still fits.
	boundary := donor.records[0]
	if len(donor.records) > 0 {
		boundary = donor.records[len(donor.records)-1]
	}
	used := nodeDescriptorLen + 2*(len(recv.records)+1+1) + len(boundary)
	for _, r := range recv.records {
		used += len(r)
	}
	return used <= bw.nodeSize
}

// rotateFromLeft moves the left sibling's last record into en (through the
// parent for index nodes; directly for leaves) and updates the separating key.
func (bw *btreeWriter) rotateFromLeft(path []uint32, childIdx []int, level int, parent, left, en *editNode) error {
	moved := left.records[len(left.records)-1]
	left.records = left.records[:len(left.records)-1]
	if en.desc.Kind == kindLeafNode {
		en.records = append([][]byte{moved}, en.records...)
	} else {
		en.records = append([][]byte{moved}, en.records...)
	}
	if err := bw.storeNode(left); err != nil {
		return err
	}
	if err := bw.storeNode(en); err != nil {
		return err
	}
	// The separating key for en (parent.records[myIdx]) becomes en's new first
	// key.
	myIdx := childIdx[level-1]
	parent.records[myIdx] = bw.ops.indexRecord(keyBytesOf(en.records[0]), en.num)
	if err := bw.storeNode(parent); err != nil {
		return err
	}
	// Parent key for en changed; if myIdx==0, propagate up.
	if myIdx == 0 {
		return bw.updateParentKey(path, childIdx, level-1)
	}
	return nil
}

// rotateFromRight moves the right sibling's first record into en and updates the
// separating key.
func (bw *btreeWriter) rotateFromRight(path []uint32, childIdx []int, level int, parent, en, right *editNode) error {
	moved := right.records[0]
	right.records = right.records[1:]
	en.records = append(en.records, moved)
	if err := bw.storeNode(en); err != nil {
		return err
	}
	if err := bw.storeNode(right); err != nil {
		return err
	}
	rightIdx := childIdx[level-1] + 1
	parent.records[rightIdx] = bw.ops.indexRecord(keyBytesOf(right.records[0]), right.num)
	if err := bw.storeNode(parent); err != nil {
		return err
	}
	return nil
}

// merge folds right into left (both children of parent; sepIdx is left's index
// in parent), removes parent's pointer to right, frees right, fixes the leaf
// link chain, and recurses to rebalance the parent.
func (bw *btreeWriter) merge(path []uint32, childIdx []int, level int, parent, left, right *editNode, sepIdx int) error {
	left.records = append(left.records, right.records...)
	// Fix the doubly-linked sibling chain at this level (both leaf AND index
	// nodes are chained, and fsck_hfs validates the chain at every level): left
	// inherits right's FLink, and right's old forward neighbour points back at
	// left. setLastLeaf tracks the catalog's last LEAF specifically, so it only
	// applies when these are leaf nodes.
	left.desc.FLink = right.desc.FLink
	if right.desc.FLink != 0 {
		nf, err := bw.loadNode(right.desc.FLink)
		if err != nil {
			return fmt.Errorf("merge: load right neighbour %d: %w", right.desc.FLink, err)
		}
		nf.desc.BLink = left.num
		if err := bw.storeNode(nf); err != nil {
			return err
		}
	} else if left.desc.Kind == kindLeafNode {
		bw.setLastLeaf(left.num)
	}
	if err := bw.storeNode(left); err != nil {
		return err
	}
	if err := bw.freeNode(right.num); err != nil {
		return err
	}
	// Remove parent's pointer to right (the record at sepIdx+1).
	parent.records = append(parent.records[:sepIdx+1], parent.records[sepIdx+2:]...)
	// Ensure parent's pointer to left has the right (possibly changed) first key.
	parent.records[sepIdx] = bw.ops.indexRecord(keyBytesOf(left.records[0]), left.num)
	if err := bw.storeNode(parent); err != nil {
		return err
	}
	// If parent is the root and now has a single child, shrink height.
	if level-1 == 0 {
		return bw.maybeShrinkRoot()
	}
	// Otherwise, the parent may now underflow: recurse.
	if bw.underflows(parent) {
		return bw.rebalance(path, childIdx, level-1)
	}
	return nil
}

// updateParentKey refreshes the index record that points at path[level] in its
// parent, to reflect path[level]'s current first key. Recurses up while the
// updated record is itself the first in its parent.
func (bw *btreeWriter) updateParentKey(path []uint32, childIdx []int, level int) error {
	for level > 0 {
		child, err := bw.loadNode(path[level])
		if err != nil {
			return err
		}
		if len(child.records) == 0 {
			return nil
		}
		parentNum := path[level-1]
		parent, err := bw.loadNode(parentNum)
		if err != nil {
			return err
		}
		myIdx := childIdx[level-1]
		if myIdx >= len(parent.records) {
			return fmt.Errorf("%w: parent idx %d out of range", ErrCorrupt, myIdx)
		}
		parent.records[myIdx] = bw.ops.indexRecord(keyBytesOf(child.records[0]), path[level])
		if err := bw.storeNode(parent); err != nil {
			return err
		}
		if myIdx != 0 {
			return nil
		}
		level--
	}
	return nil
}

// maybeShrinkRoot collapses the root when it is an index node with a single
// child, making that child the new root and decrementing tree depth. Repeats
// while applicable.
func (bw *btreeWriter) maybeShrinkRoot() error {
	for {
		root := bw.rootNode()
		if root == 0 {
			return nil
		}
		en, err := bw.loadNode(root)
		if err != nil {
			return err
		}
		if en.desc.Kind != kindIndexNode || len(en.records) != 1 {
			return nil
		}
		only, ok := indexChild(en.records[0])
		if !ok {
			return fmt.Errorf("%w: shrinkRoot bad child in root %d", ErrCorrupt, root)
		}
		bw.setRootNode(only)
		bw.setTreeDepth(bw.treeDepth() - 1)
		if err := bw.freeNode(root); err != nil {
			return err
		}
	}
}
