// Copyright (c) 2026, go-filesystems
// SPDX-License-Identifier: BSD-3-Clause

package hfsplus

import (
	"bytes"
	"encoding/binary"
	"errors"
	"io"
	"testing"
	"unicode/utf16"
)

// memVolume builds a Volume whose backing bytes are b, with a synthetic volume
// header carrying the given block size. Used to unit-test the fork reader in
// isolation from a full on-disk image.
func memVolume(b []byte, blockSize uint32) *Volume {
	return &Volume{
		rs:   bytes.NewReader(b),
		size: int64(len(b)),
		vh:   &volumeHeader{BlockSize: blockSize},
	}
}

func TestForkReadMultiExtent(t *testing.T) {
	// Block size 16. Lay out three blocks of distinct content and address them
	// out of order via two extents so reads must hop between extents.
	const bs = 16
	img := make([]byte, bs*4)
	for i := 0; i < bs; i++ {
		img[0*bs+i] = 'A' // block 0
		img[1*bs+i] = 'B' // block 1
		img[2*bs+i] = 'C' // block 2
	}
	v := memVolume(img, bs)
	// Fork = block 2 then block 0 then block 1: "C"*16 "A"*16 "B"*16.
	f := &fork{vol: v, size: bs * 3, extents: []extentDescriptor{
		{StartBlock: 2, BlockCount: 1},
		{StartBlock: 0, BlockCount: 1},
		{StartBlock: 1, BlockCount: 1},
	}}
	got, err := f.readAll()
	if err != nil {
		t.Fatal(err)
	}
	want := append(append(bytes.Repeat([]byte("C"), bs), bytes.Repeat([]byte("A"), bs)...), bytes.Repeat([]byte("B"), bs)...)
	if !bytes.Equal(got, want) {
		t.Errorf("multi-extent read mismatch:\n got %q\nwant %q", got, want)
	}

	// Partial read straddling the extent boundary (last byte of block 2 region
	// + first byte of block 0 region).
	p := make([]byte, 2)
	n, err := f.readAt(p, bs-1)
	if err != nil || n != 2 {
		t.Fatalf("straddle read n=%d err=%v", n, err)
	}
	if p[0] != 'C' || p[1] != 'A' {
		t.Errorf("straddle bytes = %q, want CA", p)
	}
}

func TestForkReadEdges(t *testing.T) {
	v := memVolume(make([]byte, 64), 16)
	f := &fork{vol: v, size: 32, extents: []extentDescriptor{{StartBlock: 0, BlockCount: 2}}}
	if _, err := f.readAt(make([]byte, 1), -1); err == nil {
		t.Error("negative offset: want error")
	}
	if _, err := f.readAt(make([]byte, 1), 32); !errors.Is(err, io.EOF) {
		t.Errorf("read at size: err=%v, want EOF", err)
	}
	// Read past end is short-read clamped to size.
	p := make([]byte, 100)
	n, err := f.readAt(p, 30)
	if err != nil {
		t.Fatalf("clamped read err=%v", err)
	}
	if n != 2 {
		t.Errorf("clamped read n=%d, want 2", n)
	}
	// Empty fork.
	empty := &fork{vol: v, size: 0}
	b, err := empty.readAll()
	if err != nil || len(b) != 0 {
		t.Errorf("empty fork: b=%v err=%v", b, err)
	}
}

func TestForkSizeExceedsExtents(t *testing.T) {
	v := memVolume(make([]byte, 64), 16)
	// Claim 32 bytes but only provide one 16-byte block.
	f := &fork{vol: v, size: 32, extents: []extentDescriptor{{StartBlock: 0, BlockCount: 1}}}
	if _, err := f.readAt(make([]byte, 32), 0); !errors.Is(err, io.ErrUnexpectedEOF) {
		t.Errorf("under-covered fork err=%v, want ErrUnexpectedEOF", err)
	}
}

func TestParseForkData(t *testing.T) {
	b := make([]byte, forkDataLen)
	binary.BigEndian.PutUint64(b[0:8], 4096)
	binary.BigEndian.PutUint32(b[12:16], 1)
	binary.BigEndian.PutUint32(b[16:20], 7) // extent[0].startBlock
	binary.BigEndian.PutUint32(b[20:24], 1) // extent[0].blockCount
	fd := parseForkData(b)
	if fd.LogicalSize != 4096 || fd.TotalBlocks != 1 {
		t.Errorf("fork data = %+v", fd)
	}
	if fd.Extents[0].StartBlock != 7 || fd.Extents[0].BlockCount != 1 {
		t.Errorf("extent[0] = %+v", fd.Extents[0])
	}
}

func TestCompareU16(t *testing.T) {
	cases := []struct {
		a, b []uint16
		want int
	}{
		{[]uint16{1, 2}, []uint16{1, 2}, 0},
		{[]uint16{1}, []uint16{1, 2}, -1},
		{[]uint16{1, 3}, []uint16{1, 2}, 1},
		{[]uint16{1, 2}, []uint16{1}, 1},
		{[]uint16{0}, []uint16{1}, -1},
	}
	for _, c := range cases {
		if got := compareU16(c.a, c.b); got != c.want {
			t.Errorf("compareU16(%v,%v) = %d, want %d", c.a, c.b, got, c.want)
		}
	}
}

func TestParseCatalogKey(t *testing.T) {
	name := utf16.Encode([]rune("héllo"))
	b := make([]byte, 6+len(name)*2)
	binary.BigEndian.PutUint32(b[0:4], 42)
	binary.BigEndian.PutUint16(b[4:6], uint16(len(name)))
	for i, u := range name {
		binary.BigEndian.PutUint16(b[6+i*2:], u)
	}
	k, ok := parseCatalogKey(b)
	if !ok {
		t.Fatal("parseCatalogKey failed")
	}
	if k.parentID != 42 || k.name != "héllo" {
		t.Errorf("key = %+v", k)
	}
	// Truncated key.
	if _, ok := parseCatalogKey(b[:5]); ok {
		t.Error("short key parsed")
	}
	if _, ok := parseCatalogKey(b[:7]); ok {
		t.Error("truncated name parsed")
	}
}

func TestCompareExtentsKey(t *testing.T) {
	a := extentsKey{fileID: 5, forkType: forkTypeData, startBlock: 0}
	if compareExtentsKey(a, a) != 0 {
		t.Error("equal keys not 0")
	}
	if compareExtentsKey(a, extentsKey{fileID: 6}) >= 0 {
		t.Error("fileID order")
	}
	if compareExtentsKey(extentsKey{fileID: 5, forkType: forkTypeRsrc}, a) <= 0 {
		t.Error("forkType order")
	}
	if compareExtentsKey(a, extentsKey{fileID: 5, startBlock: 1}) >= 0 {
		t.Error("startBlock order")
	}
}

func TestParseExtentsKey(t *testing.T) {
	b := make([]byte, 10)
	b[0] = forkTypeData
	binary.BigEndian.PutUint32(b[2:6], 99)
	binary.BigEndian.PutUint32(b[6:10], 8)
	k, ok := parseExtentsKey(b)
	if !ok || k.fileID != 99 || k.startBlock != 8 {
		t.Errorf("extents key = %+v ok=%v", k, ok)
	}
	if _, ok := parseExtentsKey(b[:9]); ok {
		t.Error("short extents key parsed")
	}
}

func TestRecordHelpers(t *testing.T) {
	// Build a record: keyLen=4, key="ABCD", then data "XY".
	rec := []byte{0, 4, 'A', 'B', 'C', 'D', 'X', 'Y'}
	kl, ks, ok := recordKeyLen(rec)
	if !ok || kl != 4 || ks != 2 {
		t.Fatalf("recordKeyLen = %d,%d,%v", kl, ks, ok)
	}
	data, ok := recordData(rec)
	if !ok || string(data) != "XY" {
		t.Errorf("recordData = %q ok=%v", data, ok)
	}
	// Odd key length forces data alignment.
	rec2 := []byte{0, 3, 'A', 'B', 'C', 0, 'Z'}
	d2, ok := recordData(rec2)
	if !ok || string(d2) != "Z" {
		t.Errorf("aligned recordData = %q ok=%v", d2, ok)
	}
	// indexChild reads the trailing uint32.
	rec3 := []byte{0, 2, 'P', 'Q', 0, 0, 0, 9}
	c, ok := indexChild(rec3)
	if !ok || c != 9 {
		t.Errorf("indexChild = %d ok=%v", c, ok)
	}
	// Malformed.
	if _, _, ok := recordKeyLen([]byte{0}); ok {
		t.Error("short rec parsed")
	}
	if _, ok := recordData([]byte{0, 9, 'A'}); ok {
		t.Error("over-long key parsed")
	}
}

func TestParseCatalogRecordTypes(t *testing.T) {
	// Folder.
	fb := make([]byte, 14)
	binary.BigEndian.PutUint16(fb[0:2], recordFolder)
	binary.BigEndian.PutUint32(fb[8:12], 100)
	cr, ok := parseCatalogRecord(fb)
	if !ok || cr.folder == nil || cr.folder.folderID != 100 {
		t.Errorf("folder rec = %+v ok=%v", cr, ok)
	}
	// Thread.
	tb := make([]byte, 12)
	binary.BigEndian.PutUint16(tb[0:2], recordFolderThread)
	binary.BigEndian.PutUint32(tb[4:8], 2)
	binary.BigEndian.PutUint16(tb[8:10], 1)
	binary.BigEndian.PutUint16(tb[10:12], uint16('x'))
	cr, ok = parseCatalogRecord(tb)
	if !ok || cr.threadName != "x" || cr.threadParent != 2 {
		t.Errorf("thread rec = %+v ok=%v", cr, ok)
	}
	// Too-short data.
	if _, ok := parseCatalogRecord([]byte{0}); ok {
		t.Error("1-byte record parsed")
	}
}

func TestSplitPath(t *testing.T) {
	got := splitPath("/a//b/./c/")
	want := []string{"a", "b", "c"}
	if len(got) != len(want) {
		t.Fatalf("splitPath = %v", got)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Errorf("splitPath[%d] = %q", i, got[i])
		}
	}
	if len(splitPath("/")) != 0 {
		t.Error("root split nonempty")
	}
}
