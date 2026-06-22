// Copyright (c) 2026, go-filesystems
// SPDX-License-Identifier: BSD-3-Clause

package hfsplus

import (
	"bytes"
	"encoding/binary"
	"testing"
)

// synthNode encodes one B-tree node of size ns: a node descriptor with the
// given kind/flink, followed by the records and the trailing offset table.
func synthNode(ns int, kind int8, flink uint32, records [][]byte) []byte {
	buf := make([]byte, ns)
	binary.BigEndian.PutUint32(buf[0:4], flink)
	buf[8] = byte(kind)
	binary.BigEndian.PutUint16(buf[10:12], uint16(len(records)))
	// Lay records out sequentially after the 14-byte descriptor.
	offs := make([]int, len(records)+1)
	pos := nodeDescriptorLen
	for i, r := range records {
		offs[i] = pos
		copy(buf[pos:], r)
		pos += len(r)
	}
	offs[len(records)] = pos
	// Offset table at the end, last-record-first.
	for i := 0; i <= len(records); i++ {
		p := ns - 2*(i+1)
		binary.BigEndian.PutUint16(buf[p:p+2], uint16(offs[i]))
	}
	return buf
}

// extentsKeyRec builds an extents-overflow index/leaf record. For leaf records
// pass extents; for index records pass a child node number (extents nil).
func extentsKeyRec(k extentsKey, child uint32, extents []extentDescriptor) []byte {
	// Key: keyLength(2) forkType(1) pad(1) fileID(4) startBlock(4) = 10 bytes
	// of key payload, so keyLength = 10.
	key := make([]byte, 2+10)
	binary.BigEndian.PutUint16(key[0:2], 10)
	key[2] = k.forkType
	binary.BigEndian.PutUint32(key[4:8], k.fileID)
	binary.BigEndian.PutUint32(key[8:12], k.startBlock)
	if extents == nil {
		data := make([]byte, 4)
		binary.BigEndian.PutUint32(data, child)
		return append(key, data...)
	}
	data := make([]byte, numInlineExtents*8)
	for i, e := range extents {
		if i >= numInlineExtents {
			break
		}
		binary.BigEndian.PutUint32(data[i*8:], e.StartBlock)
		binary.BigEndian.PutUint32(data[i*8+4:], e.BlockCount)
	}
	return append(key, data...)
}

// buildExtentsTree assembles a 2-level extents-overflow B-tree (header, one
// index node, two leaf nodes) into a flat byte image, returning a *btree whose
// fork addresses that image contiguously. Node size is ns.
func buildExtentsTree(t *testing.T, ns int) *btree {
	t.Helper()
	// Node layout in the flat fork: 0=header, 1=index(root), 2=leaf0, 3=leaf1.
	header := make([]byte, ns)
	header[8] = byte(kindHeaderNode)
	binary.BigEndian.PutUint16(header[10:12], 3) // header,map,user records (we fake count; only header rec is read)
	hr := header[nodeDescriptorLen:]
	binary.BigEndian.PutUint16(hr[0:2], 1)   // treeDepth
	binary.BigEndian.PutUint32(hr[2:6], 1)   // rootNode = index node 1
	binary.BigEndian.PutUint32(hr[10:14], 2) // firstLeaf = 2
	binary.BigEndian.PutUint32(hr[14:18], 3) // lastLeaf = 3
	binary.BigEndian.PutUint16(hr[18:20], uint16(ns))

	// Leaf 0: fileID 10 data fork, continuation starting at block 8.
	leaf0 := synthNode(ns, kindLeafNode, 3, [][]byte{
		extentsKeyRec(extentsKey{forkType: forkTypeData, fileID: 10, startBlock: 8}, 0,
			[]extentDescriptor{{StartBlock: 100, BlockCount: 4}, {StartBlock: 200, BlockCount: 2}}),
	})
	// Leaf 1: fileID 20.
	leaf1 := synthNode(ns, kindLeafNode, 0, [][]byte{
		extentsKeyRec(extentsKey{forkType: forkTypeData, fileID: 20, startBlock: 0}, 0,
			[]extentDescriptor{{StartBlock: 300, BlockCount: 1}}),
	})
	// Index node: two children. Key = smallest key reachable in each child.
	index := synthNode(ns, kindIndexNode, 0, [][]byte{
		extentsKeyRec(extentsKey{forkType: forkTypeData, fileID: 10, startBlock: 8}, 2, nil),
		extentsKeyRec(extentsKey{forkType: forkTypeData, fileID: 20, startBlock: 0}, 3, nil),
	})

	img := bytes.Join([][]byte{header, index, leaf0, leaf1}, nil)
	v := memVolume(img, uint32(ns))
	f := &fork{vol: v, size: int64(len(img)), extents: []extentDescriptor{{StartBlock: 0, BlockCount: uint32(len(img) / ns)}}}
	bt, err := openBTree(f)
	if err != nil {
		t.Fatalf("openBTree synth: %v", err)
	}
	return bt
}

func TestExtentsOverflowTraversal(t *testing.T) {
	const ns = 512
	bt := buildExtentsTree(t, ns)
	if bt.header.RootNode != 1 {
		t.Fatalf("root = %d, want 1", bt.header.RootNode)
	}
	// File 10 continues at block 8 with two runs.
	got, err := bt.overflowExtents(10, forkTypeData, 8)
	if err != nil {
		t.Fatal(err)
	}
	want := []extentDescriptor{{StartBlock: 100, BlockCount: 4}, {StartBlock: 200, BlockCount: 2}}
	if len(got) != len(want) {
		t.Fatalf("file10 extents = %v, want %v", got, want)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Errorf("file10 extent[%d] = %+v, want %+v", i, got[i], want[i])
		}
	}
	// File 20 lives in the second leaf, reached via the index node.
	got, err = bt.overflowExtents(20, forkTypeData, 0)
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 1 || got[0].StartBlock != 300 {
		t.Errorf("file20 extents = %v", got)
	}
	// A fileID with no overflow records yields nothing.
	got, err = bt.overflowExtents(999, forkTypeData, 0)
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 0 {
		t.Errorf("missing file extents = %v, want empty", got)
	}
}

func TestResolveForkExtentsWithOverflow(t *testing.T) {
	const ns = 512
	bt := buildExtentsTree(t, ns)
	v := &Volume{vh: &volumeHeader{BlockSize: ns}, extentsTree: bt}
	// File 10: 8 inline blocks (so TotalBlocks=14 forces overflow lookup at
	// startBlock 8 for the remaining 6).
	fd := forkData{TotalBlocks: 14}
	for i := 0; i < numInlineExtents; i++ {
		fd.Extents[i] = extentDescriptor{StartBlock: uint32(10 + i), BlockCount: 1}
	}
	exts, err := v.resolveForkExtents(10, forkTypeData, fd)
	if err != nil {
		t.Fatal(err)
	}
	// 8 inline + 2 overflow = 10 extents; total blocks 8 + 4 + 2 = 14.
	if len(exts) != 10 {
		t.Fatalf("resolved extents = %d, want 10", len(exts))
	}
	var blocks uint32
	for _, e := range exts {
		blocks += e.BlockCount
	}
	if blocks != 14 {
		t.Errorf("resolved blocks = %d, want 14", blocks)
	}
}

func TestResolveForkExtentsInlineOnly(t *testing.T) {
	v := &Volume{vh: &volumeHeader{BlockSize: 512}}
	fd := forkData{TotalBlocks: 2}
	fd.Extents[0] = extentDescriptor{StartBlock: 5, BlockCount: 2}
	exts, err := v.resolveForkExtents(1, forkTypeData, fd)
	if err != nil {
		t.Fatal(err)
	}
	if len(exts) != 1 || exts[0].BlockCount != 2 {
		t.Errorf("inline-only extents = %v", exts)
	}
}

func TestReadNodeBadOffsets(t *testing.T) {
	const ns = 512
	// Node claiming a record that runs past the node end.
	buf := make([]byte, ns)
	k := kindLeafNode
	buf[8] = byte(k)
	binary.BigEndian.PutUint16(buf[10:12], 1)
	// offset table: record 0 starts at 14, ends at ns+10 (invalid).
	binary.BigEndian.PutUint16(buf[ns-2:], nodeDescriptorLen)
	binary.BigEndian.PutUint16(buf[ns-4:], uint16(ns+10))
	v := memVolume(buf, ns)
	bt := &btree{f: &fork{vol: v, size: ns, extents: []extentDescriptor{{StartBlock: 0, BlockCount: 1}}}, header: btHeader{NodeSize: ns}}
	if _, err := bt.readNode(0); err == nil {
		t.Error("readNode with bad offsets: want error")
	}
}

func TestOpenBTreeBadKind(t *testing.T) {
	const ns = 512
	buf := make([]byte, ns)
	k := kindLeafNode
	buf[8] = byte(k) // node 0 must be a header node
	v := memVolume(buf, ns)
	f := &fork{vol: v, size: ns, extents: []extentDescriptor{{StartBlock: 0, BlockCount: 1}}}
	if _, err := openBTree(f); err == nil {
		t.Error("openBTree non-header node 0: want error")
	}
}
