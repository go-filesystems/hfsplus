// Copyright (c) 2026, go-filesystems
// SPDX-License-Identifier: BSD-3-Clause

package hfsplus

import (
	"testing"
	"time"
)

// Regressions for the three read-path defects the fuzzer found. Each one is a
// small patch spliced into an otherwise valid volume, so the corruption is
// reached by code that has already accepted the volume header.

// A ONE-BYTE change to a catalog record was enough. A fork's logical size is a
// uint64 off the disk and was handed straight to
// make(). int64 of a hostile value is either enormous or negative, and both
// panic with "makeslice: len out of range".
func TestReadFileRejectsAForkLargerThanItsExtents(t *testing.T) {
	base := fuzzSeed(t)
	v, err := Open(&patched{base: base, patch: []byte("0"), off: 20831}, int64(len(base)))
	if err != nil {
		return // refused at Open is a fine answer; a panic is not
	}
	defer func() { _ = v.Close() }()
	walk(v, "/", 0)
}

// Covering the claimed size on paper is not enough: the extents can point past
// the end of the image. This input allocated a slice big enough that zeroing it
// took 17 seconds for a 4 MiB volume.
func TestReadFileDoesNotAllocatePastTheImage(t *testing.T) {
	base := fuzzSeed(t)
	done := make(chan struct{})
	go func() {
		defer close(done)
		v, err := Open(&patched{base: base, patch: []byte("000000000@00000000000"), off: 20833}, int64(len(base)))
		if err != nil {
			return
		}
		defer func() { _ = v.Close() }()
		walk(v, "/", 0)
	}()
	select {
	case <-done:
	case <-time.After(10 * time.Second):
		t.Fatal("walking a corrupted volume took more than 10s: the allocation is still sized from the image")
	}
}

// FLink comes off the disk, so a link pointing backwards makes the leaf chain a
// cycle. Label and the catalog searches followed it with no bound at all.
func TestLeafChainCycleTerminates(t *testing.T) {
	base := fuzzSeed(t)
	done := make(chan struct{})
	go func() {
		defer close(done)
		v, err := Open(&patched{base: base, patch: []byte("0000000000\x00\x00\x00\x010000\xff0\x00\x01000"), off: 20470}, int64(len(base)))
		if err != nil {
			return
		}
		defer func() { _ = v.Close() }()
		_ = v.Label()
		walk(v, "/", 0)
	}()
	select {
	case <-done:
	case <-time.After(10 * time.Second):
		t.Fatal("a cyclic leaf chain did not terminate in 10s")
	}
}

// The guards must not be refusing everything: a valid volume still reads.
func TestValidVolumeStillWalks(t *testing.T) {
	base := fuzzSeed(t)
	v, err := Open(&patched{base: base, patch: []byte{}, off: 0}, int64(len(base)))
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	defer func() { _ = v.Close() }()
	if got := v.Label(); got != "FUZZ" {
		t.Errorf("Label() = %q, want %q", got, "FUZZ")
	}
	data, err := v.ReadFile("/dir/big")
	if err != nil {
		t.Fatalf("ReadFile: %v", err)
	}
	if len(data) != 12000 {
		t.Errorf("ReadFile returned %d bytes, want 12000", len(data))
	}
	if tgt, err := v.ReadLink("/link"); err != nil || tgt != "/dir/big" {
		t.Errorf("ReadLink = %q, %v; want %q, nil", tgt, err, "/dir/big")
	}
}
