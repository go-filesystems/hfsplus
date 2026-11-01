// Copyright (c) 2026, go-filesystems
// SPDX-License-Identifier: BSD-3-Clause

// bombs_test.go — memory-bomb regression tests. Each road is a fork or B-tree
// claim read off the disk; each test asserts the road is refused (or, with the
// image size unknown, grows only with the data that exists) with
// runtime.MemStats.TotalAlloc growing by a small budget. "Before" figures were
// measured on v0.4.0.
package hfsplus

import (
	"bytes"
	"errors"
	"runtime"
	"testing"
)

func allocated(f func()) uint64 {
	var a, b runtime.MemStats
	runtime.ReadMemStats(&a)
	f()
	runtime.ReadMemStats(&b)
	return b.TotalAlloc - a.TotalAlloc
}

const miB = 1 << 20

// forkOn returns a fork over v with the given claimed size and extents.
func forkOn(v *Volume, size int64, exts ...extentDescriptor) *fork {
	return &fork{vol: v, size: size, extents: exts}
}

// The fork reader proved only that the LAST byte of the fork was readable before
// allocating its whole claimed size. A fork whose first extent lies far outside
// the image and whose last extent is real passed that probe: a 512 MiB claim over
// a 4 MiB volume allocated 536 875 008 bytes (measured) before the read failed.
func TestBombForkEarlyExtentOutsideImage(t *testing.T) {
	img := fuzzSeed(t)
	bs := uint32(4096)
	{
		v, err := Open(bytes.NewReader(img), int64(len(img)))
		if err != nil {
			t.Fatal(err)
		}
		bs = v.vh.BlockSize
		f := forkOn(v, 512*miB+int64(bs),
			extentDescriptor{StartBlock: 1 << 19, BlockCount: 512 * miB / bs},
			extentDescriptor{StartBlock: 0, BlockCount: 1})
		n := allocated(func() {
			if _, err := f.readAll(); !errors.Is(err, ErrCorrupt) {
				t.Errorf("known size: err = %v, want ErrCorrupt", err)
			}
		})
		if n > miB {
			t.Errorf("known size: allocated %d bytes, want < 1 MiB", n)
		}
	}
	{
		// The same fork with the image size unknown: nothing to compare the extents
		// with, so the buffer must grow with what the reader delivers.
		v, err := Open(bytes.NewReader(img), -1)
		if err != nil {
			t.Fatal(err)
		}
		f := forkOn(v, 512*miB+int64(bs),
			extentDescriptor{StartBlock: 1 << 19, BlockCount: 512 * miB / bs},
			extentDescriptor{StartBlock: 0, BlockCount: 1})
		n := allocated(func() {
			if _, err := f.readAll(); err == nil {
				t.Error("unknown size: accepted a fork whose first extent is outside the image")
			}
		})
		if n > 4*miB {
			t.Errorf("unknown size: allocated %d bytes, want < 4 MiB", n)
		}
	}
}

// Extents may alias, so a fork can claim more bytes than the volume holds while
// every extent lies inside the image: eight extents over the whole 4 MiB image
// claimed 32 MiB (allocated, measured). A fork cannot be larger than the
// volume; exactly the volume's size is still allowed.
func TestBombForkAliasedExtents(t *testing.T) {
	img := fuzzSeed(t)
	v, err := Open(bytes.NewReader(img), int64(len(img)))
	if err != nil {
		t.Fatal(err)
	}
	blocks := uint32(int64(len(img)) / int64(v.vh.BlockSize))
	whole := extentDescriptor{StartBlock: 0, BlockCount: blocks}

	f := forkOn(v, 8*int64(len(img)), whole, whole, whole, whole, whole, whole, whole, whole)
	n := allocated(func() {
		if _, err := f.readAll(); !errors.Is(err, ErrCorrupt) {
			t.Errorf("err = %v, want ErrCorrupt", err)
		}
	})
	if n > miB {
		t.Errorf("allocated %d bytes for an aliased fork, want < 1 MiB", n)
	}

	// Exactness: the whole volume as one fork reads; one byte more is refused.
	if b, err := forkOn(v, int64(len(img)), whole).readAll(); err != nil || len(b) != len(img) {
		t.Errorf("fork of exactly the image: len %d, err %v", len(b), err)
	}
	if _, err := forkOn(v, int64(len(img))+1, whole, whole).readAll(); !errors.Is(err, ErrCorrupt) {
		t.Errorf("fork one byte over the image: err = %v, want ErrCorrupt", err)
	}
}

// With the size unknown a legitimate file still reads exactly, across the
// growth steps.
func TestUnknownSizeReadIsExact(t *testing.T) {
	img := fuzzSeed(t)
	for _, size := range []int64{int64(len(img)), -1} {
		v, err := Open(bytes.NewReader(img), size)
		if err != nil {
			t.Fatal(err)
		}
		b, err := v.ReadFile("/dir/big")
		if err != nil || !bytes.Equal(b, bytes.Repeat([]byte("hfs+"), 3000)) {
			t.Errorf("size %d: len %d, err %v", size, len(b), err)
		}
	}
}

// The leaf-chain step bound came from the extents, which are claims: a tree whose
// fork claims 2^30 blocks was allowed 2^24 steps, so a two-node cycle was walked
// sixteen million times appending the same children each lap. The bound is now
// the smaller of the extents and the image.
func TestBombLeafStepBoundedByImage(t *testing.T) {
	img := fuzzSeed(t)
	v, err := Open(bytes.NewReader(img), int64(len(img)))
	if err != nil {
		t.Fatal(err)
	}
	f := forkOn(v, 1<<40, extentDescriptor{StartBlock: 0, BlockCount: 1 << 28})
	tr := &btree{f: f, header: btHeader{NodeSize: 4096}}
	if got, want := tr.maxNodes(), len(img)/4096; got != want {
		t.Errorf("maxNodes() = %d, want %d (image size / node size)", got, want)
	}
	// With the size unknown the extents are all there is.
	v.size = -1
	if got := tr.maxNodes(); got != 1<<24 {
		t.Errorf("maxNodes() with unknown size = %d, want the %d ceiling", got, 1<<24)
	}
}

// FuzzBombs splices fuzzer bytes into a valid volume (as FuzzOpenPatched does),
// opens it with the size known and unknown, walks every read path, and asserts a
// per-input allocation budget: the seed is 4 MiB, so no road may need more than
// a small multiple of it.
func FuzzBombs(f *testing.F) {
	base := fuzzSeed(f)
	vh := int64(1024)
	f.Add(vh+112, bytes.Repeat([]byte{0xff}, 80)) // the special-file fork descriptors
	f.Add(vh+112+16, []byte{0, 0, 0x10, 0, 0, 0x04, 0, 0, 0, 0, 0x08, 0})
	f.Add(int64(5*4096), bytes.Repeat([]byte{0xff}, 32))
	f.Add(int64(6*4096), bytes.Repeat([]byte{0xff}, 64))
	f.Add(int64(20831), []byte("0"))
	f.Add(int64(20833), []byte("000000000@00000000000"))

	f.Fuzz(func(t *testing.T, off int64, patch []byte) {
		if len(patch) == 0 || len(patch) > 8192 || off < 0 || off >= int64(len(base)) {
			return
		}
		for _, size := range []int64{int64(len(base)), -1} {
			n := allocated(func() {
				v, err := Open(&patched{base: base, patch: patch, off: off}, size)
				if err != nil {
					return
				}
				exercise(v)
			})
			if n > 512*miB {
				t.Fatalf("size %d: %d bytes allocated walking a 4 MiB volume patched at %d", size, n, off)
			}
		}
	})
}
