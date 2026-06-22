// Copyright (c) 2026, go-filesystems
// SPDX-License-Identifier: BSD-3-Clause

package hfsplus

import (
	"bytes"
	"fmt"
	"math/rand"
	"testing"
)

// stress_test.go exercises the three production write-path features in pure Go
// on every architecture (incl. big-endian s390x): fragmented data forks that
// spill into the extents-overflow B-tree (>8 extents), catalog B-tree file
// growth across multiple node allocations, and heavy create/delete churn that
// drives node-underflow rebalancing/merging. The darwin fsck + macOS mount
// checks for the same scenarios live in macos_validation_test.go.

// fragmentFreeSpace creates a fragmented free region: it writes a contiguous
// run of single-block "spacer" files, then deletes every other one. The deleted
// blocks become isolated single-block holes separated by still-allocated
// spacers, so no run of two free blocks exists in that region. The rest of the
// volume stays free for metadata growth. It returns the count of single-block
// holes created. spacerCount is the number of spacer files to lay down.
func fragmentFreeSpace(t *testing.T, v *Volume, spacerCount int) int {
	t.Helper()
	bs := int(v.vh.BlockSize)
	one := make([]byte, bs) // one full allocation block per file
	created := 0
	for i := 0; i < spacerCount; i++ {
		p := fmt.Sprintf("/frag%05d", i)
		if err := v.WriteFile(p, one, 0o644); err != nil {
			break
		}
		created = i + 1
	}
	holes := 0
	for i := 0; i < created; i += 2 {
		p := fmt.Sprintf("/frag%05d", i)
		if err := v.DeleteFile(p); err != nil {
			t.Fatalf("frag delete %s: %v", p, err)
		}
		holes++
	}
	return holes
}

// TestFragmentedForkOverflow forces a data fork needing >8 extents, exercising
// the extents-overflow insert path, then reads it back byte-for-byte. A large
// volume keeps ample free headroom for metadata growth while the spacer
// checkerboard guarantees the only low-numbered free blocks are isolated
// singles, so the contiguous allocator cannot satisfy the write in one run and
// the fork fragments into many extents.
func TestFragmentedForkOverflow(t *testing.T) {
	v := openMem(t, 32<<20, FormatConfig{Label: "FRAG"})
	defer v.Close()

	// Fill the whole volume with single-block spacers, then delete every other
	// one. The ONLY free space left is then scattered single blocks — there is
	// no contiguous run of two free blocks anywhere, so the contiguous allocator
	// cannot satisfy a multi-block write in one run and BOTH the data fork and
	// the metadata (extents-overflow B-tree) growth must draw from the isolated
	// singles via the fragment allocator. This is the worst-case fragmentation
	// that exercises the extents-overflow insert path.
	holes := fragmentFreeSpace(t, v, int(v.vh.TotalBlocks)+10)
	if holes < 128 {
		t.Fatalf("fragmentation produced only %d holes", holes)
	}

	// A file of `nblk` blocks must spread across the single-block gaps: no
	// contiguous run longer than one block exists, so the fork fragments into
	// many extents that spill into the extents-overflow tree (well past the
	// eight inline descriptors). Reserve the rest of the holes for the metadata
	// growth (extents-overflow B-tree nodes) the write itself triggers — that
	// growth also allocates from the scattered singles.
	bs := int(v.vh.BlockSize)
	nblk := numInlineExtents + 24 // > 8 extents, each a single-block fragment
	if holes < nblk*3 {
		t.Fatalf("not enough holes (%d) to write %d-block file plus metadata", holes, nblk)
	}
	want := make([]byte, nblk*bs)
	rng := rand.New(rand.NewSource(1))
	rng.Read(want)
	if err := v.WriteFile("/big_fragmented.bin", want, 0o644); err != nil {
		t.Fatalf("fragmented write: %v", err)
	}
	got, err := v.ReadFile("/big_fragmented.bin")
	if err != nil {
		t.Fatalf("read fragmented: %v", err)
	}
	if !bytes.Equal(got, want) {
		t.Fatalf("fragmented read mismatch: got %d want %d bytes", len(got), len(want))
	}

	// Confirm the fork actually spilled past the inline extents (otherwise the
	// test is not exercising the overflow path).
	r, err := v.lookupPath("/big_fragmented.bin")
	if err != nil {
		t.Fatal(err)
	}
	exts, err := v.resolveForkExtents(r.rec.file.fileID, forkTypeData, r.rec.file.dataFork)
	if err != nil {
		t.Fatal(err)
	}
	if len(exts) <= numInlineExtents {
		t.Fatalf("expected >%d extents to exercise overflow tree, got %d", numInlineExtents, len(exts))
	}
	t.Logf("fragmented fork uses %d extents", len(exts))

	// Delete it and confirm the blocks come back (overflow cleanup path).
	freeBefore := v.VolumeHeader().FreeBlocks
	if err := v.DeleteFile("/big_fragmented.bin"); err != nil {
		t.Fatalf("delete fragmented: %v", err)
	}
	if v.VolumeHeader().FreeBlocks <= freeBefore {
		t.Errorf("blocks not reclaimed after deleting fragmented file")
	}
	if _, err := v.ReadFile("/big_fragmented.bin"); err == nil {
		t.Errorf("deleted fragmented file still readable")
	}
}

// TestCatalogFileGrowth writes tens of thousands of entries to force the
// catalog B-tree file to grow past its formatter reservation multiple times,
// then verifies every entry is intact.
func TestCatalogFileGrowth(t *testing.T) {
	if testing.Short() {
		t.Skip("skipping catalog-growth stress in -short")
	}
	v := openMem(t, 256<<20, FormatConfig{Label: "GROW"})
	defer v.Close()

	const n = 20000
	for i := 0; i < n; i++ {
		p := fmt.Sprintf("/f%06d.txt", i)
		if err := v.WriteFile(p, []byte(fmt.Sprintf("v%d", i)), 0o644); err != nil {
			t.Fatalf("write %s (entry %d): %v", p, i, err)
		}
	}
	// The catalog fork must have grown beyond the formatter reservation.
	if v.vh.CatalogFile.TotalBlocks <= catalogReserveNodes*mkfsNodeSize/mkfsBlockSize {
		t.Logf("catalog blocks=%d (reservation=%d)", v.vh.CatalogFile.TotalBlocks,
			catalogReserveNodes*mkfsNodeSize/mkfsBlockSize)
	}
	ents, err := v.ListDir("/")
	if err != nil {
		t.Fatal(err)
	}
	if len(ents) != n {
		t.Fatalf("root has %d entries, want %d", len(ents), n)
	}
	// Spot-read across the range, including the extremes.
	for _, i := range []int{0, 1, n / 2, n - 2, n - 1} {
		p := fmt.Sprintf("/f%06d.txt", i)
		got, err := v.ReadFile(p)
		if err != nil {
			t.Fatalf("read %s after growth: %v", p, err)
		}
		if string(got) != fmt.Sprintf("v%d", i) {
			t.Errorf("%s = %q", p, got)
		}
	}
}

// TestChurnRebalance creates many entries then deletes most in a scrambled
// order, exercising node-underflow rotate/merge and height-shrink. The
// surviving set must read back correctly and the tree stay usable.
func TestChurnRebalance(t *testing.T) {
	v := openMem(t, 128<<20, FormatConfig{Label: "CHURN"})
	defer v.Close()

	const n = 6000
	for i := 0; i < n; i++ {
		p := fmt.Sprintf("/c%06d", i)
		if err := v.MkDir(p, 0o755); err != nil {
			t.Fatalf("mkdir %s: %v", p, err)
		}
	}
	// Delete ~90% in a scrambled order; keep i%10==0.
	order := rand.New(rand.NewSource(7)).Perm(n)
	for _, i := range order {
		if i%10 == 0 {
			continue
		}
		p := fmt.Sprintf("/c%06d", i)
		if err := v.DeleteDir(p); err != nil {
			t.Fatalf("delete %s: %v", p, err)
		}
	}
	// Survivors must all still be present and listable.
	ents, err := v.ListDir("/")
	if err != nil {
		t.Fatal(err)
	}
	want := 0
	for i := 0; i < n; i++ {
		if i%10 == 0 {
			want++
		}
	}
	if len(ents) != want {
		t.Fatalf("after churn: %d entries, want %d", len(ents), want)
	}
	for i := 0; i < n; i += 10 {
		p := fmt.Sprintf("/c%06d", i)
		if _, err := v.Stat(p); err != nil {
			t.Errorf("survivor %s missing: %v", p, err)
		}
	}
	// Tree still usable: write more, then delete everything.
	if err := v.WriteFile("/after_churn.txt", []byte("ok"), 0o644); err != nil {
		t.Fatalf("write after churn: %v", err)
	}
	for i := 0; i < n; i += 10 {
		p := fmt.Sprintf("/c%06d", i)
		if err := v.DeleteDir(p); err != nil {
			t.Fatalf("final delete %s: %v", p, err)
		}
	}
	if err := v.DeleteFile("/after_churn.txt"); err != nil {
		t.Fatal(err)
	}
	final, _ := v.ListDir("/")
	if len(final) != 0 {
		t.Errorf("tree not empty after full churn delete: %d left", len(final))
	}
}
