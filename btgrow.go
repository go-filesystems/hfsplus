// Copyright (c) 2026, go-filesystems
// SPDX-License-Identifier: BSD-3-Clause

package hfsplus

import (
	"encoding/binary"
	"fmt"
)

// btgrow.go grows a B-tree file: when the node reservation is exhausted it
// allocates more allocation blocks, extends the fork's extent records (inline,
// then the extents-overflow tree when more than eight extents are needed),
// updates the BTHeaderRec totalNodes/freeNodes, grows the node-allocation
// bitmap (adding a map node when the bitmap overflows the header node), and
// rewrites the volume-header special-file fork descriptor and freeBlocks.

// allocNode returns a free B-tree node, growing the backing fork if necessary.
// The returned node is zeroed and marked used; freeNodes is decremented.
func (bw *btreeWriter) allocNode() (uint32, error) {
	n, ok, err := bw.firstFreeNode()
	if err != nil {
		return 0, err
	}
	if !ok {
		if err := bw.growFork(); err != nil {
			return 0, err
		}
		n, ok, err = bw.firstFreeNode()
		if err != nil {
			return 0, err
		}
		if !ok {
			return 0, fmt.Errorf("%w: btree still full after grow", ErrNoSpace)
		}
	}
	if err := bw.setNodeBit(n, true); err != nil {
		return 0, err
	}
	bw.setFreeNodes(bw.freeNodes() - 1)
	b := bw.nodeBytes(n)
	if b == nil {
		return 0, fmt.Errorf("%w: alloc node %d unmapped", ErrCorrupt, n)
	}
	for i := range b {
		b[i] = 0
	}
	return n, nil
}

func (bw *btreeWriter) freeNode(n uint32) error {
	if err := bw.setNodeBit(n, false); err != nil {
		return err
	}
	// Erase the node's content: fsck_hfs reports "Unused node is not erased" if a
	// node marked free in the bitmap still carries a non-zero node descriptor.
	if b := bw.nodeBytes(n); b != nil {
		for i := range b {
			b[i] = 0
		}
	}
	bw.setFreeNodes(bw.freeNodes() + 1)
	return nil
}

// growFork enlarges the B-tree file by one clump (a batch of nodes), so the
// next allocNode succeeds. It allocates allocation blocks, extends the fork's
// extents, bumps totalNodes/freeNodes, and grows the node bitmap.
func (bw *btreeWriter) growFork() error {
	// Grow by a clump sized to add a useful batch of nodes (here: 8 nodes worth,
	// rounded up to whole allocation blocks), so growth amortises across many
	// inserts rather than reallocating on every node.
	const growNodes = 8
	addBytes := int64(growNodes) * int64(bw.nodeSize)
	addBlocks := uint32((addBytes + bw.bs - 1) / bw.bs)
	if addBlocks == 0 {
		addBlocks = 1
	}
	start, err := bw.allocFragments(addBlocks)
	if err != nil {
		return err
	}
	// Extend the fork's extent records with the freshly allocated runs.
	if err := bw.appendForkExtents(start); err != nil {
		return err
	}
	// Recompute the node map mapping over the now-larger fork.
	newForkBlocks := bw.forkBlocks()
	newTotal := uint32(int64(newForkBlocks) * bw.bs / int64(bw.nodeSize))
	added := newTotal - bw.totalNodes()
	if added == 0 {
		return fmt.Errorf("%w: btree grow added no nodes", ErrNoSpace)
	}
	// Ensure the node bitmap can address the new node count, adding map nodes if
	// the header (and existing map-node chain) cannot. Each added map node
	// consumes one of the new node slots and is marked used, so it must not be
	// counted as a free node.
	mapNodesAdded, err := bw.ensureMapCapacity(newTotal)
	if err != nil {
		return err
	}
	bw.setTotalNodes(newTotal)
	bw.setFreeNodes(bw.freeNodes() + added - uint32(mapNodesAdded))
	return nil
}

// allocFragments allocates count allocation blocks, contiguously if possible,
// otherwise as multiple fragments. It returns the runs. Allocation comes from
// the shared bitmap.
func (bw *btreeWriter) allocFragments(count uint32) ([]extentDescriptor, error) {
	return bw.alc.allocFragments(count)
}

// appendForkExtents adds the runs to the B-tree's backing fork: it fills the
// remaining inline extents in the volume-header fork descriptor, then spills
// into the extents-overflow tree, and updates the fork descriptor's totalBlocks
// and logicalSize. It also refreshes bw.extents so node addressing reflects the
// growth immediately.
func (bw *btreeWriter) appendForkExtents(runs []extentDescriptor) error {
	v := bw.v
	off := specialBaseOffset + int(bw.forkIdx)*forkDataLen
	vh := v.img[volumeHeaderOffset : volumeHeaderOffset+512]
	fdBytes := vh[off : off+forkDataLen]
	fd := parseForkData(fdBytes)

	// Count inline extents in use and total blocks.
	inlineUsed := 0
	for i := 0; i < numInlineExtents; i++ {
		if fd.Extents[i].BlockCount == 0 {
			break
		}
		inlineUsed = i + 1
	}

	// Append runs to the live extent list (inline first, then overflow).
	for _, r := range runs {
		bw.extents = append(bw.extents, r)
		fd.TotalBlocks += r.BlockCount
	}
	fd.LogicalSize = uint64(fd.TotalBlocks) * uint64(bw.bs)

	// Re-pack the full extent list into inline + overflow. Inline gets the first
	// up-to-8 extents; the rest go into the extents-overflow tree keyed by the
	// special file's CNID.
	all := bw.extents
	for i := 0; i < numInlineExtents; i++ {
		if i < len(all) {
			fd.Extents[i] = all[i]
		} else {
			fd.Extents[i] = extentDescriptor{}
		}
	}
	_ = inlineUsed
	// Write the fork descriptor back into both volume headers.
	bw.encodeForkDescriptor(fdBytes, fd)
	altOff := int64(len(v.img)) - 1024
	altFD := v.img[altOff+int64(off) : altOff+int64(off)+forkDataLen]
	bw.encodeForkDescriptor(altFD, fd)

	// Spill extents beyond the 8 inline into the extents-overflow tree.
	if len(all) > numInlineExtents {
		if err := bw.spillForkOverflow(all); err != nil {
			return err
		}
	}
	// Refresh the parsed volume header so subsequent lookups see the new size.
	if err := bw.refreshHeader(); err != nil {
		return err
	}
	return nil
}

// encodeForkDescriptor writes a forkData into an 80-byte descriptor.
func (bw *btreeWriter) encodeForkDescriptor(dst []byte, fd forkData) {
	binary.BigEndian.PutUint64(dst[0:8], fd.LogicalSize)
	binary.BigEndian.PutUint32(dst[8:12], fd.ClumpSize)
	binary.BigEndian.PutUint32(dst[12:16], fd.TotalBlocks)
	off := 16
	for i := 0; i < numInlineExtents; i++ {
		binary.BigEndian.PutUint32(dst[off:off+4], fd.Extents[i].StartBlock)
		binary.BigEndian.PutUint32(dst[off+4:off+8], fd.Extents[i].BlockCount)
		off += 8
	}
}

// spillForkOverflow records the extents beyond the inline eight into the
// extents-overflow tree for the special file backing this B-tree. The overflow
// tree itself is a B-tree file; inserting into it may grow it (it prefers its
// own inline extents). To avoid unbounded recursion the extents tree's own
// growth never spills past its inline extents in practice (a B-tree file rarely
// needs >8 fragments), and if it did the spill would recurse here for the
// extents file's own CNID, which is acceptable as the recursion terminates once
// a contiguous run is found.
func (bw *btreeWriter) spillForkOverflow(all []extentDescriptor) error {
	fileID := specialFileID(bw.forkIdx)
	// Compute the startBlock (fork-relative allocation block) at which the
	// inline extents end — that is the key for the first overflow record.
	var startBlock uint32
	for i := 0; i < numInlineExtents; i++ {
		startBlock += all[i].BlockCount
	}
	overflow := all[numInlineExtents:]
	return bw.v.extentsWriter.insertExtentRecords(fileID, forkTypeData, startBlock, overflow)
}

// ensureMapCapacity makes sure the node bitmap can address at least total node
// bits, appending a map node when the existing bitmap (header record + chain)
// falls short. A new map node is itself a node in the fork, so it must be
// allocated from the just-grown space.
func (bw *btreeWriter) ensureMapCapacity(total uint32) (int, error) {
	added := 0
	for {
		cap, err := bw.mapCapacityBits()
		if err != nil {
			return added, err
		}
		if cap >= total {
			return added, nil
		}
		if err := bw.addMapNode(); err != nil {
			return added, err
		}
		added++
	}
}

// addMapNode appends a new map node (kind=2) to the B-tree, linking it after
// the last map node (or the header node), and marks it used in the bitmap. Its
// single record is the continuation of the node bitmap (initially all-zero =
// all-free). The map node consumes one node from the fork.
func (bw *btreeWriter) addMapNode() error {
	// The map node must be addressable; find a free node within the current
	// total without growing (growFork enlarged totalNodes-to-be but we set it
	// after this; here we scan the raw fork capacity).
	forkNodeCap := uint32(int64(bw.forkBlocks()) * bw.bs / int64(bw.nodeSize))
	// Find the last node in the header/map chain to link from.
	last := uint32(0) // header node
	cur := parseNodeDescriptor(bw.nodeBytes(0)).FLink
	for cur != 0 {
		last = cur
		cur = parseNodeDescriptor(bw.nodeBytes(cur)).FLink
	}
	// Pick a free node slot for the new map node: scan the bitmap up to the
	// fork capacity.
	var slot uint32
	found := false
	scanErr := bw.walkMap(func(bitBase uint32, m []byte) (bool, error) {
		for i := 0; i < len(m)*8; i++ {
			n := bitBase + uint32(i)
			if n >= forkNodeCap {
				return true, nil
			}
			if m[i/8]&(0x80>>(i%8)) == 0 {
				slot = n
				found = true
				return true, nil
			}
		}
		return false, nil
	})
	if scanErr != nil {
		return scanErr
	}
	if !found {
		return fmt.Errorf("%w: no slot for map node", ErrNoSpace)
	}
	// Initialise the map node: one record of bitmap, the rest free space.
	b := bw.nodeBytes(slot)
	if b == nil {
		return fmt.Errorf("%w: map node slot %d unmapped", ErrCorrupt, slot)
	}
	for i := range b {
		b[i] = 0
	}
	// The single map record spans from just after the descriptor to the start of
	// the offset table (2 offsets: record start + free-space terminator).
	recStart := nodeDescriptorLen
	recEnd := bw.nodeSize - 2*2
	writeNodeDescriptor(b, nodeDescriptor{Kind: kindMapNode, Height: 0, NumRecords: 1})
	writeOffsetTable(b, []uint16{uint16(recStart), uint16(recEnd)})
	// Link: previous last node -> slot.
	if last == 0 {
		hb := bw.nodeBytes(0)
		hd := parseNodeDescriptor(hb)
		hd.FLink = slot
		writeNodeDescriptor(hb, hd)
	} else {
		lb := bw.nodeBytes(last)
		ld := parseNodeDescriptor(lb)
		ld.FLink = slot
		writeNodeDescriptor(lb, ld)
		// keep records intact: writeNodeDescriptor only touches the first 12
		// bytes, leaving the map record + offset table untouched.
	}
	// Mark the map node used in the bitmap (it now exists).
	if err := bw.setNodeBit(slot, true); err != nil {
		return err
	}
	// Note: totalNodes/freeNodes are reconciled by the caller (growFork); the
	// map node itself is "used" so it does not inflate freeNodes.
	return nil
}

// refreshHeader reparses the in-memory volume header from the image so fork
// descriptors and free-block counts read back the just-written values.
func (bw *btreeWriter) refreshHeader() error {
	vh, err := readVolumeHeader(bw.v.rs)
	if err != nil {
		return err
	}
	bw.v.vh = vh
	return nil
}
