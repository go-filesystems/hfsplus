// Copyright (c) 2026, go-filesystems
// SPDX-License-Identifier: BSD-3-Clause

package hfsplus

import (
	"encoding/binary"
	"errors"
	"testing"
)

// These exercise the error-return branches of the high-level methods on the
// synthetic catalog tree, so they run on every architecture (no real image,
// no macOS tooling).

func TestStatErrors(t *testing.T) {
	v := buildCatalogTree(t, 512, keyCompareCaseFold)
	if _, err := v.Stat("/nope"); !errors.Is(err, ErrNotFound) {
		t.Errorf("Stat missing err = %v", err)
	}
	// Stat a file.
	st, err := v.Stat("/bravo")
	if err != nil {
		t.Fatal(err)
	}
	if st.Mode()&0xF000 != sIFREG {
		t.Errorf("bravo mode = %#o", st.Mode())
	}
}

func TestReadFileErrorsSynth(t *testing.T) {
	v := buildCatalogTree(t, 512, keyCompareCaseFold)
	// Reading a folder is ErrNotRegular.
	if _, err := v.ReadFile("/alpha"); !errors.Is(err, ErrNotRegular) {
		t.Errorf("ReadFile folder err = %v", err)
	}
	// Missing path.
	if _, err := v.ReadFile("/missing"); !errors.Is(err, ErrNotFound) {
		t.Errorf("ReadFile missing err = %v", err)
	}
}

func TestReadLinkErrorsSynth(t *testing.T) {
	v := buildCatalogTree(t, 512, keyCompareCaseFold)
	// bravo is a plain file, not a symlink.
	if _, err := v.ReadLink("/bravo"); !errors.Is(err, ErrNotSymlink) {
		t.Errorf("ReadLink non-symlink err = %v", err)
	}
	if _, err := v.ReadLink("/missing"); !errors.Is(err, ErrNotFound) {
		t.Errorf("ReadLink missing err = %v", err)
	}
}

func TestListDirErrorsSynth(t *testing.T) {
	v := buildCatalogTree(t, 512, keyCompareCaseFold)
	// bravo is a file, not a directory.
	if _, err := v.ListDir("/bravo"); !errors.Is(err, ErrNotDirectory) {
		t.Errorf("ListDir file err = %v", err)
	}
	if _, err := v.ListDir("/missing"); !errors.Is(err, ErrNotFound) {
		t.Errorf("ListDir missing err = %v", err)
	}
}

func TestLookupRootSynth(t *testing.T) {
	v := buildCatalogTree(t, 512, keyCompareCaseFold)
	r, err := v.lookupPath("/")
	if err != nil {
		t.Fatal(err)
	}
	if r.cnid != cnidRootFolder {
		t.Errorf("root cnid = %d, want %d", r.cnid, cnidRootFolder)
	}
}

func TestListDirSkipsThreads(t *testing.T) {
	// listChildren must skip thread records. Build a leaf containing a thread
	// record for the parent plus a real file.
	const ns = 512
	header := make([]byte, ns)
	header[8] = byte(kindHeaderNode)
	hr := header[nodeDescriptorLen:]
	binary.BigEndian.PutUint32(hr[2:6], 1)   // rootNode = leaf node 1
	binary.BigEndian.PutUint32(hr[10:14], 1) // firstLeaf = 1
	binary.BigEndian.PutUint16(hr[18:20], uint16(ns))
	hr[37] = keyCompareCaseFold

	// Thread record (parent 2's own thread) + a file child.
	threadData := make([]byte, 12)
	binary.BigEndian.PutUint16(threadData[0:2], recordFolderThread)
	thread := alignedRec(catKeyBytes(cnidRootFolder, ""), threadData)
	file := catFileRec(cnidRootFolder, "real.txt", 50, sIFREG|0o644)
	leaf := synthNode(ns, kindLeafNode, 0, [][]byte{thread, file})

	img := append(append([]byte(nil), header...), leaf...)
	v := memVolume(img, ns)
	f := &fork{vol: v, size: int64(len(img)), extents: []extentDescriptor{{StartBlock: 0, BlockCount: 2}}}
	// Point root at the single leaf (node 1).
	v.catalogTree = &btree{f: f, header: btHeader{RootNode: 1, FirstLeaf: 1, NodeSize: ns, KeyCompare: keyCompareCaseFold}}

	ents, err := v.ListDir("/")
	if err != nil {
		t.Fatal(err)
	}
	if len(ents) != 1 || ents[0].Name() != "real.txt" {
		t.Errorf("listing (thread should be skipped) = %v", ents)
	}
}
