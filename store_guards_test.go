// Copyright (c) 2026, go-filesystems
// SPDX-License-Identifier: BSD-3-Clause

package hfsplus

import (
	"bytes"
	"encoding/binary"
	"errors"
	"io"
	"testing"
	"time"
	"unicode/utf16"
)

// within runs f and fails the test when it has not returned after d: a guard
// whose loss makes the code spin is an assertion, not a hung test binary.
func within(t *testing.T, d time.Duration, what string, f func()) {
	t.Helper()
	done := make(chan struct{})
	go func() {
		defer close(done)
		f()
	}()
	select {
	case <-done:
	case <-time.After(d):
		t.Fatalf("%s did not return within %v", what, d)
	}
}

// erringReaderAt fails every read.
type erringReaderAt struct{}

func (erringReaderAt) ReadAt([]byte, int64) (int, error) { return 0, errInjectedRead }

// bareVolume is a Volume with only what the store reads: the image bytes behind it.
func bareVolume(img []byte, cache bool) *Volume {
	v := &Volume{under: bytesReaderAt(img), size: int64(len(img))}
	if cache {
		v.cacheOn = true
		v.rcache = map[int64][]byte{}
	}
	v.rs = overlayReader{v}
	return v
}

// The overlay reader returns a failed read as the error, and io.EOF with the bytes
// read when the image ends inside the range (hfsplus#55: store.go's error and
// short-read returns survived).
func TestOverlayReaderErrorsAndShortReads(t *testing.T) {
	img := bytes.Repeat([]byte{0xAB}, 6000)
	for _, cache := range []bool{false, true} {
		for _, size := range []int{100, 32 << 10} { // cached page read, and a read past the cache's limit
			v := &Volume{under: erringReaderAt{}, size: 1 << 20, cacheOn: cache}
			if cache {
				v.rcache = map[int64][]byte{}
			}
			if _, err := (overlayReader{v}).ReadAt(make([]byte, size), 0); !errors.Is(err, errInjectedRead) {
				t.Errorf("cache=%v size=%d: a failed read answered %v, want the read's error", cache, size, err)
			}
		}
		v := bareVolume(img, cache)
		for _, tc := range []struct {
			off  int64
			size int
			n    int
			err  error
		}{
			{0, 6000, 6000, nil},    // exactly the image
			{0, 6001, 6000, io.EOF}, // one byte too many
			{5900, 100, 100, nil},   // ends at the end
			{5900, 200, 100, io.EOF},
			{6000, 10, 0, io.EOF}, // starts at the end
		} {
			p := make([]byte, tc.size)
			var n int
			var err error
			within(t, 5*time.Second, "overlayReader.ReadAt", func() { n, err = (overlayReader{v}).ReadAt(p, tc.off) })
			if n != tc.n || err != tc.err {
				t.Errorf("cache=%v ReadAt(%d bytes at %d) = %d, %v; want %d, %v", cache, tc.size, tc.off, n, err, tc.n, tc.err)
			}
		}
	}
}

// readUnder of nothing reads nothing and succeeds, wherever it is aimed.
func TestReadUnderOfNothing(t *testing.T) {
	v := bareVolume(make([]byte, 100), false)
	if n, err := v.readUnder(nil, 1<<30); n != 0 || err != nil {
		t.Errorf("readUnder(nil, past the end) = %d, %v; want 0, nil", n, err)
	}
}

// A cached read that starts exactly where the last page ends is at the end of the
// image: io.EOF, with nothing copied, and it returns.
func TestReadUnderCachedAtPageEnd(t *testing.T) {
	v := bareVolume(make([]byte, readCachePage+904), true)
	p := make([]byte, 10)
	var n int
	var err error
	within(t, 5*time.Second, "readUnder at the end of a short page", func() { n, err = v.readUnder(p, readCachePage+904) })
	if n != 0 || err != io.EOF {
		t.Errorf("readUnder at the end = %d, %v; want 0, io.EOF", n, err)
	}
}

// A write drops the cached pages: a read after it sees the new bytes, not the page
// loaded before.
func TestWriteDropsTheReadCache(t *testing.T) {
	v := bareVolume(make([]byte, 8192), true)
	p := make([]byte, 4)
	if _, err := v.readUnder(p, 100); err != nil {
		t.Fatal(err)
	}
	if len(v.rcache) == 0 {
		t.Fatal("the read did not fill the cache")
	}
	v.dropReadCache()
	if len(v.rcache) != 0 {
		t.Errorf("dropReadCache left %d pages", len(v.rcache))
	}
	// And with the cache off it is a no-op that does not touch a nil map.
	off := bareVolume(make([]byte, 100), false)
	off.dropReadCache()
}

// region refuses a transaction past either bound, and takes one at the bound.
func TestRegionBounds(t *testing.T) {
	fill := func(v *Volume, count int, bytesHeld int64) {
		v.regions = make(map[int64]*region, count)
		for i := 0; i < count; i++ {
			off := v.size + int64(i)*4 // outside the image, so no overlap with a real range
			v.regions[off] = &region{off: off, buf: make([]byte, 1)}
		}
		v.regionBytes = bytesHeld
	}
	img := make([]byte, 1<<16)

	v := bareVolume(img, false)
	fill(v, maxRegions-1, 0)
	if _, err := v.region(0, 512); err != nil {
		t.Errorf("one region below the limit: %v", err)
	}
	v = bareVolume(img, false)
	fill(v, maxRegions, 0)
	if _, err := v.region(0, 512); !errors.Is(err, errTxnTooLarge) {
		t.Errorf("at maxRegions regions: %v, want errTxnTooLarge", err)
	}

	v = bareVolume(img, false)
	fill(v, 1, maxRegionBytes-512)
	if _, err := v.region(0, 512); err != nil {
		t.Errorf("exactly maxRegionBytes: %v", err)
	}
	v = bareVolume(img, false)
	fill(v, 1, maxRegionBytes-511)
	if _, err := v.region(0, 512); !errors.Is(err, errTxnTooLarge) {
		t.Errorf("one byte past maxRegionBytes: %v, want errTxnTooLarge", err)
	}
}

// A region whose read comes back short is an error, with the read's own error kept.
func TestRegionShortAndFailedReads(t *testing.T) {
	v := &Volume{under: bytesReaderAt(make([]byte, 100)), size: 4096}
	if _, err := v.region(0, 512); !errors.Is(err, io.ErrUnexpectedEOF) {
		t.Errorf("short read: %v, want io.ErrUnexpectedEOF", err)
	}
	v = &Volume{under: erringReaderAt{}, size: 4096}
	if _, err := v.region(0, 512); !errors.Is(err, errInjectedRead) {
		t.Errorf("failed read: %v, want the read's error", err)
	}
	v = &Volume{under: bytesReaderAt(make([]byte, 4096)), size: 4096}
	if b, err := v.region(0, 512); err != nil || len(b) != 512 {
		t.Errorf("full read: %d bytes, %v", len(b), err)
	}
}

// hdr hands back a scratch header and records the error when the header cannot be
// loaded, so a setter never writes through nil.
func TestHdrOnAnUnreadableHeader(t *testing.T) {
	v := &Volume{under: bytesReaderAt(make([]byte, 4096)), size: 4096, altOff: 1 << 40}
	b := v.hdr(true)
	if len(b) != 512 {
		t.Errorf("hdr returned %d bytes, want a 512-byte scratch", len(b))
	}
	if v.werr == nil {
		t.Error("hdr did not record the error")
	}
}

// writeData: nothing to write is nil wherever it is aimed; a range outside the
// image is refused; a range ending exactly at the end of the image is written.
func TestWriteDataRange(t *testing.T) {
	img := make([]byte, 4096)
	v := &Volume{under: bytesReaderAt(img), size: 4096, img: img}
	if err := v.writeData(nil, -5); err != nil {
		t.Errorf("empty write at -5: %v", err)
	}
	if v.dataDirty {
		t.Error("an empty write marked the volume dirty")
	}
	for _, tc := range []struct {
		off int64
		n   int
		ok  bool
	}{
		{-1, 4, false},
		{4093, 4, false},
		{4092, 4, true},
		{0, 4096, true},
		{0, 4097, false},
	} {
		err := v.writeData(bytes.Repeat([]byte{7}, tc.n), tc.off)
		if tc.ok && err != nil {
			t.Errorf("write of %d at %d: %v", tc.n, tc.off, err)
		}
		if !tc.ok && !errors.Is(err, ErrCorrupt) {
			t.Errorf("write of %d at %d: %v, want ErrCorrupt", tc.n, tc.off, err)
		}
	}
	if img[4095] != 7 {
		t.Error("the write ending at the end of the image did not land")
	}
}

// fsync with no backing writer still ends the journal's pending state.
func TestFsyncWithoutAWriter(t *testing.T) {
	v := &Volume{jw: &journalWriter{pending: true}}
	if err := v.fsync(); err != nil {
		t.Fatal(err)
	}
	if v.jw.pending {
		t.Error("fsync without a writer left the journal pending")
	}
}

// flush returns the error an earlier accessor recorded, and writes nothing.
func TestFlushReturnsTheRecordedError(t *testing.T) {
	v := openMem(t, 4<<20, FormatConfig{Label: "WERR"})
	v.dataDirty = true // otherwise there is nothing to flush and the guard is not what answers
	v.werr = errInjectedIO
	if err := v.flush(); !errors.Is(err, errInjectedIO) {
		t.Errorf("flush = %v, want the recorded error", err)
	}
}

// stampLastMountedVersion refuses when a header cannot be loaded.
func TestStampOnAnUnreadableHeader(t *testing.T) {
	v := openMem(t, 4<<20, FormatConfig{Label: "STAMP"})
	v.altOff = 1 << 40
	var p, a []writeRun
	if err := v.stampLastMountedVersion(&p, &a); !errors.Is(err, ErrCorrupt) {
		t.Errorf("stamp with the alternate header outside the image: %v, want ErrCorrupt", err)
	}
}

// Every read a commit makes can fail, and the commit then fails with that error
// (the cache is off so each read reaches the store).
func TestFlushReadFaults(t *testing.T) {
	img, err := Mkfs(4<<20, FormatConfig{Label: "FLUSHR"})
	if err != nil {
		t.Fatal(err)
	}
	open := func(failAfter int64) (*Volume, *faultRW) {
		rw := &faultRW{img: bytes.Clone(img)}
		v, err := OpenWritableAt(rw, int64(len(img)))
		if err != nil {
			t.Fatal(err)
		}
		v.cacheOn = false
		v.dataDirty = true
		if failAfter > 0 {
			rw.failRead = rw.reads.Load() + failAfter
		}
		return v, rw
	}
	v, rw := open(0)
	base := rw.reads.Load()
	if err := v.flush(); err != nil {
		t.Fatalf("the unfaulted flush: %v", err)
	}
	total := rw.reads.Load() - base
	if total < 3 {
		t.Fatalf("the flush made %d reads; the sweep would be vacuous", total)
	}
	for n := int64(1); n <= total; n++ {
		v, _ := open(n)
		if err := v.flush(); !errors.Is(err, errInjectedIO) {
			t.Errorf("read %d of the flush fails: flush = %v, want the injected error", n, err)
		}
	}
}

// ReadFile and OpenFile refuse a compressed file (flag 0x0020 in its catalog
// record) instead of serving the bytes of its data fork.
func TestCompressedFileIsRefused(t *testing.T) {
	v := openMem(t, 4<<20, FormatConfig{Label: "COMPR"})
	if err := v.WriteFile("/zz.txt", []byte("hello"), 0o644); err != nil {
		t.Fatal(err)
	}
	img := bytes.Clone(v.Bytes())
	if err := v.Close(); err != nil {
		t.Fatal(err)
	}
	// The catalog key is keyLength(2) parentID(4) nameLength(2) name(UTF-16BE); the
	// record follows it: recordType(2) flags(2) ...
	var name []byte
	for _, u := range utf16.Encode([]rune("zz.txt")) {
		name = binary.BigEndian.AppendUint16(name, u)
	}
	pat := binary.BigEndian.AppendUint16(nil, 6)
	pat = append(pat, name...)
	i := bytes.Index(img, pat)
	if i < 8 {
		t.Fatal("the file's catalog key is not in the image")
	}
	keyLen := int(binary.BigEndian.Uint16(img[i-6:]))
	rec := i - 6 + 2 + keyLen
	if rec%2 == 1 {
		rec++
	}
	if binary.BigEndian.Uint16(img[rec:]) != recordFile {
		t.Fatalf("record type %d at %d, want a file record", binary.BigEndian.Uint16(img[rec:]), rec)
	}
	binary.BigEndian.PutUint16(img[rec+2:], binary.BigEndian.Uint16(img[rec+2:])|kFileFlagCompressed)

	ro, err := Open(bytesReaderAt(img), int64(len(img)))
	if err != nil {
		t.Fatal(err)
	}
	if _, err := ro.ReadFile("/zz.txt"); !errors.Is(err, ErrUnsupported) {
		t.Errorf("ReadFile of a compressed file: %v, want ErrUnsupported", err)
	}
}

// reopen returns the error of the header read it starts with, and does not go on to
// parse trees from a header it never read.
func TestReopenOnAnUnreadableImage(t *testing.T) {
	v := openMem(t, 4<<20, FormatConfig{Label: "REOPEN"})
	v.under = erringReaderAt{}
	v.cacheOn = false
	var err error
	func() {
		defer func() {
			if p := recover(); p != nil {
				t.Errorf("reopen panicked: %v", p)
			}
		}()
		err = v.reopen()
	}()
	if !errors.Is(err, errInjectedRead) {
		t.Errorf("reopen = %v, want the read's error", err)
	}
}
