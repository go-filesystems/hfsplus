// Copyright (c) 2026, go-filesystems
// SPDX-License-Identifier: BSD-3-Clause

package hfsplus

import (
	"bytes"
	"fmt"
	"math/rand"
	"testing"
)

// TestExtentsOverflowReplace exercises the extents-overflow writer's
// insert/replace/delete paths: writing a fragmented file, overwriting it with a
// differently-fragmented one (forcing replacement of overflow records), and
// deleting it (reclaiming the overflow extents).
func TestExtentsOverflowReplace(t *testing.T) {
	v := openMem(t, 32<<20, FormatConfig{Label: "EOV"})
	defer v.Close()

	// Fragment the low region so writes spill into the overflow tree.
	holes := fragmentFreeSpace(t, v, int(v.vh.TotalBlocks)+10)
	if holes < 128 {
		t.Fatalf("holes=%d", holes)
	}
	bs := int(v.vh.BlockSize)

	mk := func(nblk int) []byte {
		b := make([]byte, nblk*bs)
		rng := rand.New(rand.NewSource(int64(nblk)))
		rng.Read(b)
		return b
	}

	first := mk(numInlineExtents + 20)
	if err := v.WriteFile("/eov.bin", first, 0o644); err != nil {
		t.Fatalf("first write: %v", err)
	}
	if got, err := v.ReadFile("/eov.bin"); err != nil || !bytes.Equal(got, first) {
		t.Fatalf("first read mismatch: err=%v", err)
	}

	// Overwrite with a smaller-but-still-overflowing file (replace overflow recs).
	second := mk(numInlineExtents + 12)
	if err := v.WriteFile("/eov.bin", second, 0o644); err != nil {
		t.Fatalf("overwrite: %v", err)
	}
	if got, err := v.ReadFile("/eov.bin"); err != nil || !bytes.Equal(got, second) {
		t.Fatalf("overwrite read mismatch: err=%v len(got)=%d want=%d", err, len(got), len(second))
	}

	// Confirm it still has >8 extents (overflow path engaged).
	r, err := v.lookupPath("/eov.bin")
	if err != nil {
		t.Fatal(err)
	}
	exts, err := v.resolveForkExtents(r.rec.file.fileID, forkTypeData, r.rec.file.dataFork)
	if err != nil {
		t.Fatal(err)
	}
	if len(exts) <= numInlineExtents {
		t.Fatalf("expected overflow extents, got %d", len(exts))
	}

	// Delete: overflow records and their blocks must be reclaimed.
	freeBefore := v.VolumeHeader().FreeBlocks
	if err := v.DeleteFile("/eov.bin"); err != nil {
		t.Fatalf("delete: %v", err)
	}
	if v.VolumeHeader().FreeBlocks <= freeBefore {
		t.Errorf("blocks not reclaimed after delete of overflow file")
	}
}

// TestRotateBalancing drives delete patterns that force sibling rotation (rather
// than merge) by deleting in an order that underflows nodes whose siblings are
// near-full, then verifies the surviving set reads back and fsck-style chain
// stays intact.
func TestRotateBalancing(t *testing.T) {
	v := openMem(t, 64<<20, FormatConfig{Label: "ROT"})
	defer v.Close()

	const n = 3000
	for i := 0; i < n; i++ {
		if err := v.WriteFile(fmt.Sprintf("/r%06d", i), []byte("payloadpayloadpayload"), 0o644); err != nil {
			t.Fatalf("write %d: %v", i, err)
		}
	}
	// Delete a contiguous middle band: this underflows interior leaves whose
	// neighbours are full, exercising rotation before any merge.
	for i := 1000; i < 1500; i++ {
		if err := v.DeleteFile(fmt.Sprintf("/r%06d", i)); err != nil {
			t.Fatalf("delete %d: %v", i, err)
		}
	}
	// Survivors intact.
	for _, i := range []int{0, 999, 1500, 1501, n - 1} {
		if _, err := v.Stat(fmt.Sprintf("/r%06d", i)); err != nil {
			t.Errorf("survivor r%06d: %v", i, err)
		}
	}
	// Deleted gone.
	for _, i := range []int{1000, 1250, 1499} {
		if _, err := v.Stat(fmt.Sprintf("/r%06d", i)); err == nil {
			t.Errorf("r%06d should be deleted", i)
		}
	}
	ents, err := v.ListDir("/")
	if err != nil {
		t.Fatal(err)
	}
	if len(ents) != n-500 {
		t.Fatalf("got %d entries, want %d", len(ents), n-500)
	}
}

// TestLastLeafAccessor reaches the lastLeaf accessor and single-node tree paths
// by growing then fully draining a small tree.
func TestLastLeafAccessor(t *testing.T) {
	v := openMem(t, 16<<20, FormatConfig{Label: "LL"})
	defer v.Close()
	for i := 0; i < 200; i++ {
		if err := v.WriteFile(fmt.Sprintf("/x%04d", i), []byte("z"), 0o644); err != nil {
			t.Fatalf("write %d: %v", i, err)
		}
	}
	for i := 0; i < 200; i++ {
		if err := v.DeleteFile(fmt.Sprintf("/x%04d", i)); err != nil {
			t.Fatalf("delete %d: %v", i, err)
		}
	}
	ents, err := v.ListDir("/")
	if err != nil {
		t.Fatal(err)
	}
	if len(ents) != 0 {
		t.Fatalf("tree not empty: %d", len(ents))
	}
	// Reinsert after full drain (root rebuilt from empty).
	if err := v.WriteFile("/after", []byte("ok"), 0o644); err != nil {
		t.Fatalf("reinsert: %v", err)
	}
	if got, err := v.ReadFile("/after"); err != nil || string(got) != "ok" {
		t.Fatalf("reinsert read: %v %q", err, got)
	}
}
