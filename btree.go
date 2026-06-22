// Copyright (c) 2026, go-filesystems
// SPDX-License-Identifier: BSD-3-Clause

package hfsplus

import (
	"encoding/binary"
	"fmt"
)

// B-tree node kinds (BTNodeDescriptor.kind). Typed int8 so they round-trip
// through the single signed descriptor byte without untyped-constant overflow.
const (
	kindLeafNode   int8 = -1 // 0xFF
	kindIndexNode  int8 = 0
	kindHeaderNode int8 = 1
	kindMapNode    int8 = 2
)

// nodeDescriptor is the 14-byte BTNodeDescriptor at the start of every node.
type nodeDescriptor struct {
	FLink      uint32 // forward link to next node of same kind
	BLink      uint32
	Kind       int8
	Height     uint8
	NumRecords uint16
}

const nodeDescriptorLen = 14

func parseNodeDescriptor(b []byte) nodeDescriptor {
	return nodeDescriptor{
		FLink:      binary.BigEndian.Uint32(b[0:4]),
		BLink:      binary.BigEndian.Uint32(b[4:8]),
		Kind:       int8(b[8]),
		Height:     b[9],
		NumRecords: binary.BigEndian.Uint16(b[10:12]),
	}
}

// btHeader holds the fields the reader needs from the BTHeaderRec (the first
// record of the header node, node 0).
type btHeader struct {
	RootNode   uint32
	FirstLeaf  uint32
	LastLeaf   uint32
	NodeSize   uint16
	KeyCompare uint8 // for catalog: 0xCF case-fold, 0xBC binary (case-sensitive)
	Attributes uint32
}

// btree is an opened HFS+ B-tree (catalog or extents-overflow) backed by a
// fork.
type btree struct {
	f      *fork
	header btHeader
}

// keyCompareCaseFold / keyCompareBinary are the BTHeaderRec.keyCompareType
// values used by the catalog file.
const (
	keyCompareCaseFold = 0xCF
	keyCompareBinary   = 0xBC
)

// openBTree reads the header node of the B-tree backing fork f.
func openBTree(f *fork) (*btree, error) {
	// The header node is node 0. Its size is not yet known, but the
	// BTHeaderRec lives at a fixed offset (14-byte descriptor) and contains
	// nodeSize. Read enough to cover the descriptor + header record.
	const probe = nodeDescriptorLen + 106
	buf := make([]byte, probe)
	if _, err := f.readAt(buf, 0); err != nil {
		return nil, fmt.Errorf("hfsplus: read btree header node: %w", err)
	}
	nd := parseNodeDescriptor(buf)
	if nd.Kind != kindHeaderNode {
		return nil, fmt.Errorf("%w: btree node 0 kind=%d", ErrCorrupt, nd.Kind)
	}
	h := buf[nodeDescriptorLen:]
	hdr := btHeader{
		RootNode:  binary.BigEndian.Uint32(h[2:6]),
		FirstLeaf: binary.BigEndian.Uint32(h[10:14]),
		LastLeaf:  binary.BigEndian.Uint32(h[14:18]),
		NodeSize:  binary.BigEndian.Uint16(h[18:20]),
		// BTHeaderRec: ... nodeSize(18,2) maxKeyLength(20,2) totalNodes(22,4)
		// freeNodes(26,4) reserved1(30,2) clumpSize(32,4) btreeType(36,1)
		// keyCompareType(37,1) attributes(38,4).
		KeyCompare: h[37],
		Attributes: binary.BigEndian.Uint32(h[38:42]),
	}
	if hdr.NodeSize < 512 || hdr.NodeSize&(hdr.NodeSize-1) != 0 {
		return nil, fmt.Errorf("%w: btree node size %d", ErrCorrupt, hdr.NodeSize)
	}
	return &btree{f: f, header: hdr}, nil
}

// node is a fully-read B-tree node with its records sliced out.
type node struct {
	desc    nodeDescriptor
	records [][]byte // each record's raw bytes (key + data)
}

// readNode reads node number n and slices out its records using the trailing
// record-offset table (uint16 big-endian offsets, last-to-first from the end
// of the node).
func (t *btree) readNode(n uint32) (*node, error) {
	ns := int(t.header.NodeSize)
	buf := make([]byte, ns)
	if _, err := t.f.readAt(buf, int64(n)*int64(ns)); err != nil {
		return nil, fmt.Errorf("hfsplus: read node %d: %w", n, err)
	}
	desc := parseNodeDescriptor(buf)
	nrec := int(desc.NumRecords)
	// The offset table holds nrec+1 uint16 offsets at the end of the node.
	// offsets[i] is the start of record i; offsets[nrec] points just past
	// the last record (start of free space).
	if nrec < 0 || (nrec+1)*2 > ns {
		return nil, fmt.Errorf("%w: node %d record count %d", ErrCorrupt, n, nrec)
	}
	offs := make([]int, nrec+1)
	for i := 0; i <= nrec; i++ {
		// offset table is stored last-record-first: entry i lives at
		// ns - 2*(i+1).
		pos := ns - 2*(i+1)
		offs[i] = int(binary.BigEndian.Uint16(buf[pos : pos+2]))
	}
	recs := make([][]byte, 0, nrec)
	for i := 0; i < nrec; i++ {
		start, end := offs[i], offs[i+1]
		if start < nodeDescriptorLen || end > ns || start > end {
			return nil, fmt.Errorf("%w: node %d record %d bounds [%d,%d]", ErrCorrupt, n, i, start, end)
		}
		recs = append(recs, buf[start:end])
	}
	return &node{desc: desc, records: recs}, nil
}

// recordKeyLen returns the key length of a leaf/index record and the byte
// offset at which the key bytes begin. HFS+ catalog and extents trees use a
// 16-bit key length prefix (BTHeaderRec attribute kBTBigKeysMask is set for
// HFS+).
func recordKeyLen(rec []byte) (keyLen int, keyStart int, ok bool) {
	if len(rec) < 2 {
		return 0, 0, false
	}
	keyLen = int(binary.BigEndian.Uint16(rec[0:2]))
	keyStart = 2
	if keyStart+keyLen > len(rec) {
		return 0, 0, false
	}
	return keyLen, keyStart, true
}

// recordData returns the data portion of a record (after the key). For HFS+
// big keys the data begins on the even-byte boundary following the key.
func recordData(rec []byte) ([]byte, bool) {
	keyLen, keyStart, ok := recordKeyLen(rec)
	if !ok {
		return nil, false
	}
	dataStart := keyStart + keyLen
	// Align the data to a 2-byte boundary relative to record start: the key
	// length field plus key bytes is padded so the data is word-aligned.
	if dataStart%2 != 0 {
		dataStart++
	}
	if dataStart > len(rec) {
		return nil, false
	}
	return rec[dataStart:], true
}

// indexChild returns the child node number stored in an index record (the
// trailing uint32 after the key).
func indexChild(rec []byte) (uint32, bool) {
	data, ok := recordData(rec)
	if !ok || len(data) < 4 {
		return 0, false
	}
	return binary.BigEndian.Uint32(data[0:4]), true
}
