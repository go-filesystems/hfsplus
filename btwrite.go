// Copyright (c) 2026, go-filesystems
// SPDX-License-Identifier: BSD-3-Clause

package hfsplus

import (
	"encoding/binary"
	"fmt"
)

// btwrite.go is the generic HFS+ B-tree write engine shared by the catalog and
// extents-overflow trees. It addresses B-tree nodes through the backing
// special file's resolved extent list (so the fork may be fragmented, with
// inline extents spilling into the extents-overflow tree), grows that fork when
// the node reservation is exhausted, and maintains the B-tree header node's
// node-allocation bitmap — adding map nodes when the bitmap overflows the
// header node.
//
// Both trees are driven through this engine; the catalog-key and extents-key
// specifics (ordering, record encoding) are supplied by a small treeOps adapter
// so the split/merge/insert/delete machinery is written exactly once.

// specialForkIndex identifies which volume-header special-file fork descriptor
// backs a B-tree, so fork growth can rewrite the right descriptor in the header.
type specialForkIndex int

const (
	forkExtentsFile specialForkIndex = 1 // vh special-file slot for the extents tree
	forkCatalogFile specialForkIndex = 2 // vh special-file slot for the catalog tree
)

// specialBaseOffset is the byte offset of the first special-file fork
// descriptor within the 512-byte volume header (see encodeVolumeHeader).
const specialBaseOffset = 0x70

// treeOps abstracts the per-tree key handling the generic engine needs: how to
// order two records by key, and how to build an index record (firstKey+child).
type treeOps interface {
	// compareRecords orders record a before/after b by their B-tree key.
	compareRecords(a, b []byte) int
	// indexRecord builds an index record from a child's first key and number.
	indexRecord(firstKey []byte, child uint32) []byte
}

// btreeWriter is the generic mutable view of one B-tree (catalog or extents).
type btreeWriter struct {
	v        *Volume
	tree     *btree
	ops      treeOps
	nodeSize int
	forkIdx  specialForkIndex
	alc      *allocator // shared allocation bitmap for fork growth

	// nodeOff maps node number -> absolute image byte offset, derived from the
	// backing fork's resolved extent list. Rebuilt after the fork grows.
	extents []extentDescriptor
	bs      int64 // block size
}

// newBTreeWriter builds a generic writer over an opened B-tree backed by the
// special file forkIdx. The fork's full extent list (inline + extents-overflow)
// is resolved up front so fragmented B-tree files are addressable.
func (v *Volume) newBTreeWriter(tree *btree, forkIdx specialForkIndex, ops treeOps, alc *allocator) (*btreeWriter, error) {
	fd := v.specialFork(forkIdx)
	if fd.Extents[0].BlockCount == 0 {
		return nil, fmt.Errorf("%w: special file %d has no extents", ErrCorrupt, forkIdx)
	}
	bw := &btreeWriter{
		v:        v,
		tree:     tree,
		ops:      ops,
		nodeSize: int(tree.header.NodeSize),
		forkIdx:  forkIdx,
		alc:      alc,
		bs:       int64(v.vh.BlockSize),
	}
	exts, err := v.resolveForkExtents(specialFileID(forkIdx), forkTypeData, fd)
	if err != nil {
		return nil, err
	}
	bw.extents = exts
	return bw, nil
}

// specialFork returns the volume header fork descriptor for a special file.
func (v *Volume) specialFork(idx specialForkIndex) forkData {
	switch idx {
	case forkExtentsFile:
		return v.vh.ExtentsFile
	case forkCatalogFile:
		return v.vh.CatalogFile
	default:
		return forkData{}
	}
}

// specialFileID returns the well-known CNID of a special file, used as the key
// fileID when its own fork spills into the extents-overflow tree.
func specialFileID(idx specialForkIndex) uint32 {
	switch idx {
	case forkExtentsFile:
		return kHFSExtentsFileID
	case forkCatalogFile:
		return kHFSCatalogFileID
	default:
		return 0
	}
}

// Well-known CNIDs of the HFS+ special files (TN1150).
const (
	kHFSExtentsFileID = 3
	kHFSCatalogFileID = 4
)

// nodeBytes returns the live image slice for node n, mapping it through the
// fork's extent list. Returns nil if n is out of range (caller must have grown
// the fork first).
func (bw *btreeWriter) nodeBytes(n uint32) []byte {
	logical := int64(n) * int64(bw.nodeSize)
	var base int64
	for _, e := range bw.extents {
		span := int64(e.BlockCount) * bw.bs
		if logical < base+span {
			abs := int64(e.StartBlock)*bw.bs + (logical - base)
			return bw.v.img[abs : abs+int64(bw.nodeSize)]
		}
		base += span
	}
	return nil
}

// forkBlocks returns the total allocation blocks the backing fork currently
// covers.
func (bw *btreeWriter) forkBlocks() uint32 {
	var n uint32
	for _, e := range bw.extents {
		n += e.BlockCount
	}
	return n
}

// --- header-node accessors (generic over both trees) ---

// hdr returns the live BTHeaderRec bytes (record 0 of the header node).
func (bw *btreeWriter) hdrRec() []byte {
	b := bw.nodeBytes(0)
	return b[nodeDescriptorLen:]
}

// The structural setters (rootNode, firstLeaf, lastLeaf) update BOTH the
// on-image header-node bytes AND the reader's cached bw.tree.header, because the
// write path performs reader lookups mid-transaction (e.g. adjustParentValence
// reads a thread record after the tree may have been restructured). Without
// keeping the cached header in sync, the reader would descend from a stale root
// — into a just-freed/erased node — and report a bogus corruption.

func (bw *btreeWriter) treeDepth() uint16     { return binary.BigEndian.Uint16(bw.hdrRec()[0:2]) }
func (bw *btreeWriter) setTreeDepth(d uint16) { binary.BigEndian.PutUint16(bw.hdrRec()[0:2], d) }
func (bw *btreeWriter) rootNode() uint32      { return binary.BigEndian.Uint32(bw.hdrRec()[2:6]) }
func (bw *btreeWriter) setRootNode(n uint32) {
	binary.BigEndian.PutUint32(bw.hdrRec()[2:6], n)
	if bw.tree != nil {
		bw.tree.header.RootNode = n
	}
}
func (bw *btreeWriter) leafRecords() uint32 { return binary.BigEndian.Uint32(bw.hdrRec()[6:10]) }
func (bw *btreeWriter) setLeafRecords(n uint32) {
	binary.BigEndian.PutUint32(bw.hdrRec()[6:10], n)
}
func (bw *btreeWriter) setFirstLeaf(n uint32) {
	binary.BigEndian.PutUint32(bw.hdrRec()[10:14], n)
	if bw.tree != nil {
		bw.tree.header.FirstLeaf = n
	}
}
func (bw *btreeWriter) firstLeaf() uint32 { return binary.BigEndian.Uint32(bw.hdrRec()[10:14]) }
func (bw *btreeWriter) setLastLeaf(n uint32) {
	binary.BigEndian.PutUint32(bw.hdrRec()[14:18], n)
	if bw.tree != nil {
		bw.tree.header.LastLeaf = n
	}
}
func (bw *btreeWriter) lastLeaf() uint32   { return binary.BigEndian.Uint32(bw.hdrRec()[14:18]) }
func (bw *btreeWriter) totalNodes() uint32 { return binary.BigEndian.Uint32(bw.hdrRec()[22:26]) }
func (bw *btreeWriter) setTotalNodes(n uint32) {
	binary.BigEndian.PutUint32(bw.hdrRec()[22:26], n)
}
func (bw *btreeWriter) freeNodes() uint32 { return binary.BigEndian.Uint32(bw.hdrRec()[26:30]) }
func (bw *btreeWriter) setFreeNodes(n uint32) {
	binary.BigEndian.PutUint32(bw.hdrRec()[26:30], n)
}

// --- node load / store ---

func (bw *btreeWriter) loadNode(n uint32) (*editNode, error) {
	b := bw.nodeBytes(n)
	if b == nil {
		return nil, fmt.Errorf("%w: node %d out of fork range", ErrCorrupt, n)
	}
	desc := parseNodeDescriptor(b)
	nrec := int(desc.NumRecords)
	if nrec < 0 || (nrec+1)*2 > bw.nodeSize {
		return nil, fmt.Errorf("%w: node %d bad reccount", ErrCorrupt, n)
	}
	offs := make([]int, nrec+1)
	for i := 0; i <= nrec; i++ {
		pos := bw.nodeSize - 2*(i+1)
		offs[i] = int(binary.BigEndian.Uint16(b[pos : pos+2]))
	}
	recs := make([][]byte, nrec)
	for i := 0; i < nrec; i++ {
		s, e := offs[i], offs[i+1]
		if s < nodeDescriptorLen || e > bw.nodeSize || s > e {
			return nil, fmt.Errorf("%w: node %d rec %d bounds", ErrCorrupt, n, i)
		}
		recs[i] = append([]byte(nil), b[s:e]...)
	}
	return &editNode{num: n, desc: desc, records: recs}, nil
}

func (bw *btreeWriter) storeNode(en *editNode) error {
	b := bw.nodeBytes(en.num)
	if b == nil {
		return fmt.Errorf("%w: store node %d out of range", ErrCorrupt, en.num)
	}
	for i := range b {
		b[i] = 0
	}
	off := nodeDescriptorLen
	offsets := []uint16{uint16(off)}
	for _, r := range en.records {
		if off+len(r) > bw.nodeSize-2*(len(en.records)+1) {
			return fmt.Errorf("%w: node %d overflow", errNodeFull, en.num)
		}
		copy(b[off:], r)
		off += len(r)
		offsets = append(offsets, uint16(off))
	}
	en.desc.NumRecords = uint16(len(en.records))
	writeNodeDescriptor(b, en.desc)
	writeOffsetTable(b, offsets)
	return nil
}

// recordsFit reports whether en's records fit in a node.
func (bw *btreeWriter) recordsFit(en *editNode) bool {
	used := nodeDescriptorLen + 2*(len(en.records)+1)
	for _, r := range en.records {
		used += len(r)
	}
	return used <= bw.nodeSize
}

// usedBytes returns the bytes en's records occupy including the offset table.
func (bw *btreeWriter) usedBytes(en *editNode) int {
	used := nodeDescriptorLen + 2*(len(en.records)+1)
	for _, r := range en.records {
		used += len(r)
	}
	return used
}
