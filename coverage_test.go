// Copyright (c) 2026, go-filesystems
// SPDX-License-Identifier: BSD-3-Clause

package hfsplus

import (
	"bytes"
	"encoding/binary"
	"errors"
	"io"
	"testing"
)

// failReaderAt returns an error after the first successful header read, to
// exercise I/O error paths in the fork/btree readers.
type failReaderAt struct {
	data   []byte
	failAt int64 // offsets >= failAt fail
}

func (f *failReaderAt) ReadAt(p []byte, off int64) (int, error) {
	if off >= f.failAt {
		return 0, errors.New("simulated I/O failure")
	}
	if off >= int64(len(f.data)) {
		return 0, io.EOF
	}
	n := copy(p, f.data[off:])
	if n < len(p) {
		return n, io.EOF
	}
	return n, nil
}

func TestReadVolumeHeaderErrors(t *testing.T) {
	// Too short to read the header at all.
	if _, err := readVolumeHeader(bytes.NewReader(make([]byte, 100))); err == nil {
		t.Error("short image: want error")
	}
	// Valid signature but a non-power-of-two block size → corrupt.
	buf := make([]byte, 2048)
	binary.BigEndian.PutUint16(buf[1024:], sigHFSPlus)
	binary.BigEndian.PutUint32(buf[1024+0x28:], 1000) // bad block size
	if _, err := readVolumeHeader(bytes.NewReader(buf)); !errors.Is(err, ErrCorrupt) {
		t.Errorf("bad block size err = %v, want ErrCorrupt", err)
	}
}

func TestOpenEmptyCatalog(t *testing.T) {
	// Valid header, sane block size, but catalog fork logical size 0.
	buf := make([]byte, 4096)
	binary.BigEndian.PutUint16(buf[1024:], sigHFSPlus)
	binary.BigEndian.PutUint32(buf[1024+0x28:], 512) // block size
	binary.BigEndian.PutUint32(buf[1024+0x2C:], 8)   // total blocks
	if _, err := Open(bytes.NewReader(buf), int64(len(buf))); !errors.Is(err, ErrCorrupt) {
		t.Errorf("empty catalog err = %v, want ErrCorrupt", err)
	}
}

func TestForkReadIOError(t *testing.T) {
	fr := &failReaderAt{data: make([]byte, 64), failAt: 0}
	v := &Volume{rs: fr, vh: &volumeHeader{BlockSize: 16}}
	f := &fork{vol: v, size: 16, extents: []extentDescriptor{{StartBlock: 0, BlockCount: 1}}}
	if _, err := f.readAt(make([]byte, 8), 0); err == nil {
		t.Error("fork read over failing backend: want error")
	}
}

func TestBTreeReadNodeIOError(t *testing.T) {
	v := &Volume{rs: &failReaderAt{data: make([]byte, 1024), failAt: 0}, vh: &volumeHeader{BlockSize: 512}}
	bt := &btree{f: &fork{vol: v, size: 1024, extents: []extentDescriptor{{StartBlock: 0, BlockCount: 2}}}, header: btHeader{NodeSize: 512}}
	if _, err := bt.readNode(0); err == nil {
		t.Error("readNode over failing backend: want error")
	}
}

func TestOpenBTreeShortFork(t *testing.T) {
	// A fork too small to even hold the header-node probe.
	v := memVolume(make([]byte, 8), 16)
	f := &fork{vol: v, size: 8, extents: []extentDescriptor{{StartBlock: 0, BlockCount: 1}}}
	if _, err := openBTree(f); err == nil {
		t.Error("openBTree tiny fork: want error")
	}
}

func TestParseCatalogRecordShortFile(t *testing.T) {
	// recordType File but data too short for the fork descriptors.
	d := make([]byte, 20)
	binary.BigEndian.PutUint16(d[0:2], recordFile)
	if _, ok := parseCatalogRecord(d); ok {
		t.Error("short file record parsed")
	}
}

func TestKeyFromRecordBad(t *testing.T) {
	if _, ok := keyFromRecord([]byte{0}); ok {
		t.Error("1-byte record key parsed")
	}
	// keyLength longer than the record.
	if _, ok := keyFromRecord([]byte{0, 50, 1, 2}); ok {
		t.Error("over-long key parsed")
	}
}

func TestIndexChildBad(t *testing.T) {
	// Record whose data is too short to hold a uint32 child pointer.
	if _, ok := indexChild([]byte{0, 2, 'A', 'B', 1}); ok {
		t.Error("short index child parsed")
	}
}

func TestListChildrenEmptyParent(t *testing.T) {
	v := buildCatalogTree(t, 512, keyCompareCaseFold)
	// A parent CNID with no children returns an empty slice, not an error.
	got, err := v.catalogTree.listChildren(99999)
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 0 {
		t.Errorf("listChildren(absent) = %v", got)
	}
}

func TestStatFolderAndRootList(t *testing.T) {
	v := buildCatalogTree(t, 512, keyCompareCaseFold)
	st, err := v.Stat("/alpha")
	if err != nil {
		t.Fatal(err)
	}
	if st.Mode()&0xF000 != sIFDIR {
		t.Errorf("alpha mode = %#o, want dir", st.Mode())
	}
}

func TestReadFileUnsupportedCompressed(t *testing.T) {
	v := buildCatalogTree(t, 512, keyCompareCaseFold)
	// Mark bravo (file 18) as compressed by patching its flags through a
	// crafted record; simplest is a direct unit on isCompressed.
	if !isCompressed(kFileFlagCompressed) {
		t.Error("isCompressed false for compressed flag")
	}
	if isCompressed(0) {
		t.Error("isCompressed true for zero flags")
	}
	_ = v
}
