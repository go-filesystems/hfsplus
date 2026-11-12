// Copyright (c) 2026, go-filesystems
// SPDX-License-Identifier: BSD-3-Clause

package hfsplus

import (
	"bytes"
	"encoding/binary"
	"errors"
	"os"
	"path/filepath"
	"runtime"
	"testing"
	"time"
)

// Tests for go-filesystems/hfsplus#33: a writable volume reads on demand and
// writes only what a mutation changed.

// countingWriter records every WriteAt (offset, bytes) and fsync, and can be
// told to fail from the n-th write on.
type countingWriter struct {
	inner interface {
		WriteAt([]byte, int64) (int, error)
	}
	syncer interface{ Sync() error }
	writes []writeRec
	syncs  int
	failAt int // fail the write with this index (0-based); -1 never
}

type writeRec struct {
	off  int64
	data []byte
}

var errInjected = errors.New("hfsplus: injected failure")

func (c *countingWriter) WriteAt(p []byte, off int64) (int, error) {
	if c.failAt >= 0 && len(c.writes) == c.failAt {
		c.writes = append(c.writes, writeRec{off: -1})
		return 0, errInjected
	}
	c.writes = append(c.writes, writeRec{off: off, data: bytes.Clone(p)})
	return c.inner.WriteAt(p, off)
}

func (c *countingWriter) Sync() error { c.syncs++; return c.syncer.Sync() }

func (c *countingWriter) bytes() int {
	n := 0
	for _, w := range c.writes {
		n += len(w.data)
	}
	return n
}

// instrument replaces the volume's writer with a countingWriter.
func instrument(v *Volume) *countingWriter {
	c := &countingWriter{inner: v.wa, syncer: v.wa.(interface{ Sync() error }), failAt: -1}
	v.wa = c
	return c
}

// sparseImage formats a size-byte volume at a path and stores it sparsely: only
// the non-zero 64 KiB chunks are written, so a 1 GiB image costs a few hundred
// KiB of disk.
func sparseImage(t testing.TB, size int64) string {
	t.Helper()
	img, err := Mkfs(size, FormatConfig{Label: "BIG"})
	if err != nil {
		t.Fatal(err)
	}
	p := filepath.Join(t.TempDir(), "sparse.img")
	f, err := os.Create(p)
	if err != nil {
		t.Fatal(err)
	}
	defer f.Close()
	if err := f.Truncate(int64(len(img))); err != nil {
		t.Fatal(err)
	}
	const chunk = 64 << 10
	zero := make([]byte, chunk)
	for off := 0; off < len(img); off += chunk {
		end := min(off+chunk, len(img))
		if bytes.Equal(img[off:end], zero[:end-off]) {
			continue
		}
		if _, err := f.WriteAt(img[off:end], int64(off)); err != nil {
			t.Fatal(err)
		}
	}
	return p
}

func attrsOf(t testing.TB, path string) uint32 {
	t.Helper()
	f, err := os.Open(path)
	if err != nil {
		t.Fatal(err)
	}
	defer f.Close()
	var b [8]byte
	if _, err := f.ReadAt(b[:], volumeHeaderOffset); err != nil {
		t.Fatal(err)
	}
	return binary.BigEndian.Uint32(b[4:8])
}

// TestInPlaceWriteOrder: one small WriteFile plus one Chtimes write a few KiB,
// in the order TN1150 relies on -- file data, the unmounted bit cleared, B-tree
// and bitmap sectors, the alternate header, the primary header last, with the bit
// set again -- and an fsync after each stage.
func TestInPlaceWriteOrder(t *testing.T) {
	p := sparseImage(t, 8<<20)
	v, err := OpenFileWritable(p)
	if err != nil {
		t.Fatal(err)
	}
	defer v.Close()
	if attrsOf(t, p)&kHFSVolumeUnmountedBit == 0 {
		t.Fatal("fixture is not marked cleanly unmounted")
	}
	c := instrument(v)
	if err := v.WriteFile("/a.txt", []byte{'x'}, 0o644); err != nil {
		t.Fatal(err)
	}
	if len(c.writes) < 5 {
		t.Fatalf("only %d writes", len(c.writes))
	}
	size := int64(8 << 20)
	// The file's data (the byte, then the zero fill of its block) comes first.
	k := 0
	for k < len(c.writes) && c.writes[k].off != volumeHeaderOffset {
		k++
	}
	if k == 0 || k >= len(c.writes) {
		t.Fatalf("data writes before the header: %d of %d", k, len(c.writes))
	}
	var dataBytes int
	for _, w := range c.writes[:k] {
		dataBytes += len(w.data)
	}
	if dataBytes != 4096 {
		t.Errorf("%d data bytes before the header, want one 4096-byte block", dataBytes)
	}
	if w := c.writes[k]; len(w.data) != 512 || binary.BigEndian.Uint32(w.data[4:8])&kHFSVolumeUnmountedBit != 0 {
		t.Errorf("write [%d,+%d) is not the primary header with the unmounted bit cleared", w.off, len(w.data))
	}
	n := len(c.writes)
	if w := c.writes[n-1]; w.off != volumeHeaderOffset || len(w.data) != 512 ||
		binary.BigEndian.Uint32(w.data[4:8])&kHFSVolumeUnmountedBit == 0 {
		t.Errorf("last write [%d,+%d) is not the primary header with the bit set", w.off, len(w.data))
	}
	if w := c.writes[n-2]; w.off != size-1024 || len(w.data) != 512 {
		t.Errorf("last but one write [%d,+%d) is not the alternate header", w.off, len(w.data))
	}
	for _, w := range c.writes[k+1 : n-2] {
		if w.off == volumeHeaderOffset || w.off == size-1024 {
			t.Errorf("a header written before the B-tree and bitmap sectors: %d", w.off)
		}
	}
	if c.syncs != 3 {
		t.Errorf("%d fsyncs, want 3 (data+bit, B-trees+bitmap, alternate+primary header)", c.syncs)
	}
	if got := c.bytes(); got > 64<<10 {
		t.Errorf("a 1-byte WriteFile wrote %d bytes", got)
	}
	before := c.bytes()
	now := time.Now()
	if err := v.Chtimes("/a.txt", now, now); err != nil {
		t.Fatal(err)
	}
	if got := c.bytes() - before; got > 32<<10 {
		t.Errorf("Chtimes wrote %d bytes", got)
	}
	v.Close()
	if attrsOf(t, p)&kHFSVolumeUnmountedBit == 0 {
		t.Error("the volume is left marked as not cleanly unmounted")
	}
	r, err := OpenFile(p)
	if err != nil {
		t.Fatal(err)
	}
	defer r.Close()
	if b, err := r.ReadFile("/a.txt"); err != nil || string(b) != "x" {
		t.Errorf("ReadFile = %q, %v", b, err)
	}
}

// TestInPlaceCrashIsVisible: whatever write a crash interrupts, once a B-tree or
// bitmap sector may have changed the volume is marked as not cleanly unmounted,
// and a crash before any write leaves the volume untouched.
func TestInPlaceCrashIsVisible(t *testing.T) {
	p := sparseImage(t, 8<<20)
	orig, err := os.ReadFile(p)
	if err != nil {
		t.Fatal(err)
	}
	probe, err := OpenFileWritable(p)
	if err != nil {
		t.Fatal(err)
	}
	c := instrument(probe)
	if err := probe.MkDir("/d", 0o755); err != nil {
		t.Fatal(err)
	}
	total := len(c.writes)
	probe.Close()
	for i := 0; i < total; i++ {
		if err := os.WriteFile(p, orig, 0o644); err != nil {
			t.Fatal(err)
		}
		v, err := OpenFileWritable(p)
		if err != nil {
			t.Fatal(err)
		}
		cw := instrument(v)
		cw.failAt = i
		if err := v.MkDir("/d", 0o755); !errors.Is(err, errInjected) {
			t.Fatalf("failure at write %d: err = %v", i, err)
		}
		v.Close()
		got, err := os.ReadFile(p)
		if err != nil {
			t.Fatal(err)
		}
		if i == 0 && !bytes.Equal(got, orig) {
			t.Errorf("a failure at the first write changed the image")
		}
		if i >= 1 && attrsOf(t, p)&kHFSVolumeUnmountedBit != 0 {
			t.Errorf("crash at write %d of %d: the volume still says it was cleanly unmounted", i, total)
		}
	}
}

// TestInPlaceLargeSparseImage: on a 1 GiB sparse image a small WriteFile and a
// Chtimes write O(change) bytes -- KiB, not the GiB the old whole-image Sync
// wrote -- and opening, writing and reading allocate megabytes, not the image.
func TestInPlaceLargeSparseImage(t *testing.T) {
	if testing.Short() {
		t.Skip("builds a 1 GiB image")
	}
	const size = 1 << 30
	p := sparseImage(t, size)
	runtime.GC()
	var m0, m1 runtime.MemStats
	runtime.ReadMemStats(&m0)

	v, err := OpenFileWritable(p)
	if err != nil {
		t.Fatal(err)
	}
	defer v.Close()
	c := instrument(v)
	payload := bytes.Repeat([]byte{0xAB}, 5000)
	if err := v.WriteFile("/f.bin", payload, 0o644); err != nil {
		t.Fatal(err)
	}
	now := time.Now()
	if err := v.Chtimes("/f.bin", now, now); err != nil {
		t.Fatal(err)
	}
	if err := v.MkDir("/dir", 0o755); err != nil {
		t.Fatal(err)
	}
	got, err := v.ReadFile("/f.bin")
	if err != nil || !bytes.Equal(got, payload) {
		t.Fatalf("ReadFile: %d bytes, %v", len(got), err)
	}
	runtime.ReadMemStats(&m1)

	if w := c.bytes(); w > 256<<10 {
		t.Errorf("three small operations on a %d-byte image wrote %d bytes", int64(size), w)
	}
	t.Logf("wrote %d bytes in %d WriteAt calls (%d fsyncs); allocated %d KiB", c.bytes(), len(c.writes), c.syncs, (m1.TotalAlloc-m0.TotalAlloc)>>10)
	if alloc := m1.TotalAlloc - m0.TotalAlloc; alloc > 48<<20 {
		t.Errorf("opening and writing a %d-byte image allocated %d bytes", int64(size), alloc)
	}
	v.Close()
	r, err := OpenFile(p)
	if err != nil {
		t.Fatal(err)
	}
	defer r.Close()
	if b, err := r.ReadFile("/f.bin"); err != nil || !bytes.Equal(b, payload) {
		t.Errorf("after reopen: %d bytes, %v", len(b), err)
	}
}

// TestInPlaceOpenAllocatesLittle: opening a large image for writing reads its
// headers and nothing proportional to its size.
func TestInPlaceOpenAllocatesLittle(t *testing.T) {
	p := sparseImage(t, 64<<20)
	runtime.GC()
	var m0, m1 runtime.MemStats
	runtime.ReadMemStats(&m0)
	v, err := OpenFileWritable(p)
	if err != nil {
		t.Fatal(err)
	}
	defer v.Close()
	runtime.ReadMemStats(&m1)
	if alloc := m1.TotalAlloc - m0.TotalAlloc; alloc > 1<<20 {
		t.Errorf("OpenFileWritable of a 64 MiB image allocated %d bytes", alloc)
	}
	if v.Bytes() != nil {
		t.Error("a file-backed volume holds a copy of the image")
	}
}

// TestInPlaceRegionBounds: the transaction-private copies refuse a range outside
// the image and two structures over the same bytes.
func TestInPlaceRegionBounds(t *testing.T) {
	p := sparseImage(t, 1<<20)
	v, err := OpenFileWritable(p)
	if err != nil {
		t.Fatal(err)
	}
	defer v.Close()
	if _, err := v.region(1<<20-100, 4096); !errors.Is(err, ErrCorrupt) {
		t.Errorf("region past the end: %v", err)
	}
	if _, err := v.region(-1, 512); !errors.Is(err, ErrCorrupt) {
		t.Errorf("negative region: %v", err)
	}
	if _, err := v.region(8192, 4096); err != nil {
		t.Fatal(err)
	}
	if _, err := v.region(8192+512, 512); !errors.Is(err, ErrCorrupt) {
		t.Errorf("overlapping region: %v", err)
	}
	if _, err := v.region(8192, 512); !errors.Is(err, ErrCorrupt) {
		t.Errorf("same offset, other length: %v", err)
	}
	// An edit that never reaches commit is dropped by the next mutation.
	b, _ := v.region(8192, 4096)
	b[0] = 0xEE
	if err := v.MkDir("/x", 0o755); err != nil {
		t.Fatal(err)
	}
	if _, err := v.Stat("/x"); err != nil {
		t.Error(err)
	}
}
