// Copyright (c) 2026, go-filesystems
// SPDX-License-Identifier: BSD-3-Clause

package hfsplus

import (
	"encoding/binary"
	"testing"
)

// buildMapWriter constructs a minimal btreeWriter over a hand-built image whose
// header node carries a deliberately tiny node-bitmap record (record 2), with a
// single linked map node continuing the bitmap. This drives the map-node chain
// code (walkMap / mapNodeSlice / setNodeBit across the chain / ensureMapCapacity
// / addMapNode) that only triggers on enormous real trees.
func buildMapWriter(t *testing.T) *btreeWriter {
	t.Helper()
	const ns = 512   // small node so the header map record is tiny
	const nodes = 64 // fork holds 64 nodes
	bs := int64(ns)  // one node per block for simple addressing
	img := make([]byte, ns*nodes)

	// Header node (node 0): 3 records [BTHeaderRec, userData, map]. We size the
	// map record to a single byte (8 bits) so the bitmap overflows almost
	// immediately and a map node is required.
	hdr := img[:ns]
	hdr[8] = byte(kindHeaderNode)
	binary.BigEndian.PutUint16(hdr[10:12], 3) // 3 records
	rec0 := nodeDescriptorLen
	rec1 := rec0 + 106
	rec2 := rec1 + 16  // tiny userData
	mapEnd := rec2 + 1 // 1-byte map record => 8 bits in the header
	// BTHeaderRec fields the accessors read: nodeSize, totalNodes, freeNodes.
	h := hdr[nodeDescriptorLen:]
	binary.BigEndian.PutUint16(h[18-14:18-14+2], ns) // nodeSize at off 18
	binary.BigEndian.PutUint32(h[22-14:22-14+4], 3)  // totalNodes
	binary.BigEndian.PutUint32(h[26-14:26-14+4], 0)  // freeNodes
	// Offset table: 4 entries (3 records + terminator), last-first.
	offs := []uint16{uint16(rec0), uint16(rec1), uint16(rec2), uint16(mapEnd)}
	for i, o := range offs {
		p := ns - 2*(i+1)
		binary.BigEndian.PutUint16(hdr[p:p+2], o)
	}
	// Mark nodes 0 (header) used in the 8-bit header bitmap.
	hdr[rec2] = 0x80 // node 0 used

	v := &Volume{img: img, vh: &volumeHeader{BlockSize: uint32(bs)}}
	bw := &btreeWriter{
		v:        v,
		nodeSize: ns,
		bs:       bs,
		extents:  []extentDescriptor{{StartBlock: 0, BlockCount: nodes}},
	}
	return bw
}

func TestMapNodeChainSynth(t *testing.T) {
	bw := buildMapWriter(t)

	// Initially the bitmap (header record) addresses only 8 bits.
	cap0, err := bw.mapCapacityBits()
	if err != nil {
		t.Fatal(err)
	}
	if cap0 != 8 {
		t.Fatalf("initial map capacity = %d bits, want 8", cap0)
	}

	// ensureMapCapacity for 40 nodes must append map node(s) to extend the
	// bitmap, exercising addMapNode + mapNodeSlice + walkMap chain traversal.
	added, err := bw.ensureMapCapacity(40)
	if err != nil {
		t.Fatalf("ensureMapCapacity: %v", err)
	}
	if added == 0 {
		t.Fatalf("expected at least one map node added")
	}
	cap1, err := bw.mapCapacityBits()
	if err != nil {
		t.Fatal(err)
	}
	if cap1 < 40 {
		t.Fatalf("map capacity after grow = %d, want >= 40", cap1)
	}

	// setNodeBit must locate and set a bit in the appended map-node stretch
	// (beyond the 8-bit header record), then read back consistently.
	if err := bw.setNodeBit(20, true); err != nil {
		t.Fatalf("setNodeBit(20): %v", err)
	}
	// firstFreeNode (bounded by totalNodes) must skip the set bits.
	bw.setTotalNodes(40)
	n, ok, err := bw.firstFreeNode()
	if err != nil || !ok {
		t.Fatalf("firstFreeNode: ok=%v err=%v", ok, err)
	}
	if n == 0 || n == 20 {
		t.Fatalf("firstFreeNode returned an occupied node %d", n)
	}
}
