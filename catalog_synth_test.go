// Copyright (c) 2026, go-filesystems
// SPDX-License-Identifier: BSD-3-Clause

package hfsplus

import (
	"bytes"
	"encoding/binary"
	"testing"
	"unicode/utf16"
)

// catKeyBytes encodes an HFSPlusCatalogKey (with the 2-byte key-length prefix).
func catKeyBytes(parent uint32, name string) []byte {
	u := utf16.Encode([]rune(name))
	key := make([]byte, 2+6+len(u)*2)
	binary.BigEndian.PutUint16(key[0:2], uint16(6+len(u)*2)) // keyLength
	binary.BigEndian.PutUint32(key[2:6], parent)
	binary.BigEndian.PutUint16(key[6:8], uint16(len(u)))
	for i, c := range u {
		binary.BigEndian.PutUint16(key[8+i*2:], c)
	}
	return key
}

// catFolderRec builds a leaf folder record for (parent,name)->folderID.
func catFolderRec(parent uint32, name string, folderID uint32) []byte {
	key := catKeyBytes(parent, name)
	data := make([]byte, 14)
	binary.BigEndian.PutUint16(data[0:2], recordFolder)
	binary.BigEndian.PutUint32(data[8:12], folderID)
	return alignedRec(key, data)
}

// catFileRec builds a leaf file record for (parent,name)->fileID with mode.
func catFileRec(parent uint32, name string, fileID uint32, mode uint16) []byte {
	key := catKeyBytes(parent, name)
	data := make([]byte, 88+forkDataLen)
	binary.BigEndian.PutUint16(data[0:2], recordFile)
	binary.BigEndian.PutUint32(data[8:12], fileID)
	binary.BigEndian.PutUint16(data[42:44], mode)
	return alignedRec(key, data)
}

// catIndexRec builds an index record: key + child node number.
func catIndexRec(parent uint32, name string, child uint32) []byte {
	key := catKeyBytes(parent, name)
	data := make([]byte, 4)
	binary.BigEndian.PutUint32(data, child)
	return alignedRec(key, data)
}

// alignedRec concatenates key + data, padding the key to an even length so the
// data starts on a 2-byte boundary (matching recordData's alignment rule).
func alignedRec(key, data []byte) []byte {
	if len(key)%2 != 0 {
		key = append(key, 0)
	}
	return append(append([]byte(nil), key...), data...)
}

// buildCatalogTree assembles a 2-level catalog B-tree (header, index root, two
// leaves) using case-fold key comparison. keyCompare selects kCF/kBC.
func buildCatalogTree(t *testing.T, ns int, keyCompare uint8) *Volume {
	t.Helper()
	header := make([]byte, ns)
	header[8] = byte(kindHeaderNode)
	binary.BigEndian.PutUint16(header[10:12], 3)
	hr := header[nodeDescriptorLen:]
	binary.BigEndian.PutUint16(hr[0:2], 2)   // treeDepth
	binary.BigEndian.PutUint32(hr[2:6], 1)   // rootNode = 1 (index)
	binary.BigEndian.PutUint32(hr[10:14], 2) // firstLeaf = 2
	binary.BigEndian.PutUint32(hr[14:18], 3) // lastLeaf = 3
	binary.BigEndian.PutUint16(hr[18:20], uint16(ns))
	hr[37] = keyCompare // keyCompareType in BTHeaderRec

	// Leaf 2: root (CNID 2) children "alpha"(folder 100) and "bravo"(file 18).
	leaf0 := synthNode(ns, kindLeafNode, 3, [][]byte{
		catFolderRec(cnidRootFolder, "alpha", 100),
		catFileRec(cnidRootFolder, "bravo", 18, sIFREG|0o644),
	})
	// Leaf 3: children of folder 100: "child.txt"(file 200).
	leaf1 := synthNode(ns, kindLeafNode, 0, [][]byte{
		catFileRec(100, "child.txt", 200, sIFREG|0o600),
	})
	// Index root: first child covers parent 2, second covers parent 100.
	index := synthNode(ns, kindIndexNode, 0, [][]byte{
		catIndexRec(cnidRootFolder, "alpha", 2),
		catIndexRec(100, "child.txt", 3),
	})

	img := bytes.Join([][]byte{header, index, leaf0, leaf1}, nil)
	v := memVolume(img, uint32(ns))
	f := &fork{vol: v, size: int64(len(img)), extents: []extentDescriptor{{StartBlock: 0, BlockCount: uint32(len(img) / ns)}}}
	bt, err := openBTree(f)
	if err != nil {
		t.Fatalf("openBTree catalog: %v", err)
	}
	v.catalogTree = bt
	return v
}

func TestCatalogIndexDescent(t *testing.T) {
	v := buildCatalogTree(t, 512, keyCompareCaseFold)

	// Lookup in the first leaf.
	r, err := v.lookupPath("/alpha")
	if err != nil {
		t.Fatal(err)
	}
	if r.rec.folder == nil || r.rec.folder.folderID != 100 {
		t.Errorf("/alpha = %+v", r.rec)
	}
	// Lookup descending into folder 100 (second leaf via index).
	r, err = v.lookupPath("/alpha/child.txt")
	if err != nil {
		t.Fatal(err)
	}
	if r.rec.file == nil || r.rec.file.fileID != 200 {
		t.Errorf("/alpha/child.txt = %+v", r.rec)
	}
	// Case-insensitive: ALPHA resolves.
	if _, err := v.lookupPath("/ALPHA"); err != nil {
		t.Errorf("case-insensitive /ALPHA: %v", err)
	}
	// Missing.
	if _, err := v.lookupPath("/zzz"); err == nil {
		t.Error("missing /zzz: want error")
	}

	// ListDir root via the index/leaf path.
	ents, err := v.ListDir("/")
	if err != nil {
		t.Fatal(err)
	}
	names := map[string]bool{}
	for _, e := range ents {
		names[e.Name()] = true
	}
	if !names["alpha"] || !names["bravo"] {
		t.Errorf("root listing = %v", names)
	}
	// ListDir of folder 100.
	ents, err = v.ListDir("/alpha")
	if err != nil {
		t.Fatal(err)
	}
	if len(ents) != 1 || ents[0].Name() != "child.txt" {
		t.Errorf("/alpha listing = %v", ents)
	}
}

func TestCatalogCaseSensitive(t *testing.T) {
	v := buildCatalogTree(t, 512, keyCompareBinary)
	if !v.CaseSensitive() {
		t.Error("expected case-sensitive volume")
	}
	if _, err := v.lookupPath("/alpha"); err != nil {
		t.Errorf("exact-case lookup: %v", err)
	}
	// Binary compare: wrong case must NOT match.
	if _, err := v.lookupPath("/ALPHA"); err == nil {
		t.Error("case-sensitive /ALPHA matched, want not found")
	}
}

func TestCompareCatalogKeyDirect(t *testing.T) {
	bt := &btree{header: btHeader{KeyCompare: keyCompareCaseFold}}
	a := catalogKey{parentID: 1, name: "Foo"}
	b := catalogKey{parentID: 1, name: "foo"}
	if bt.compareCatalogKey(a, b) != 0 {
		t.Error("case-fold compare: Foo vs foo should be equal")
	}
	if bt.compareCatalogKey(catalogKey{parentID: 1}, catalogKey{parentID: 2}) >= 0 {
		t.Error("parent ordering")
	}
	if bt.compareCatalogKey(catalogKey{parentID: 2}, catalogKey{parentID: 1}) <= 0 {
		t.Error("parent ordering rev")
	}
	if bt.compareCatalogKey(catalogKey{parentID: 1, name: "a"}, catalogKey{parentID: 1, name: "b"}) >= 0 {
		t.Error("name ordering")
	}
	if bt.compareCatalogKey(catalogKey{parentID: 1, name: "b"}, catalogKey{parentID: 1, name: "a"}) <= 0 {
		t.Error("name ordering rev")
	}

	// Binary key compare path.
	btb := &btree{header: btHeader{KeyCompare: keyCompareBinary}}
	ka := catalogKey{parentID: 1, nameU16: utf16.Encode([]rune("a"))}
	kb := catalogKey{parentID: 1, nameU16: utf16.Encode([]rune("b"))}
	if btb.compareCatalogKey(ka, kb) >= 0 {
		t.Error("binary name ordering")
	}
}
