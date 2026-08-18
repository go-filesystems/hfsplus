// Copyright (c) 2026, go-filesystems
// SPDX-License-Identifier: BSD-3-Clause

package hfsplus

import (
	"errors"
	"testing"
)

// The formatter reserved a fixed 256-node catalog without ever checking that
// the volume could hold it. Below roughly 1 MiB the catalog fork was laid out
// past the end of the image and the volume header's freeBlocks -- totalBlocks
// minus usedBlocks, both uint32 -- wrapped around.

func TestMkfsDoesNotUnderflowFreeBlocks(t *testing.T) {
	for _, size := range []int64{32 << 10, 64 << 10, 256 << 10, 1 << 20, 2 << 20, 16 << 20} {
		img, err := Mkfs(size, FormatConfig{Label: "SMALL"})
		if err != nil {
			if errors.Is(err, ErrCorrupt) {
				continue // refused outright is a fine answer; silent corruption is not
			}
			t.Fatalf("Mkfs(%d): %v", size, err)
		}
		v, err := OpenWritable(img, nil)
		if err != nil {
			t.Fatalf("OpenWritable(%d KiB): %v", size>>10, err)
		}
		vh := v.VolumeHeader()
		if vh.FreeBlocks > vh.TotalBlocks {
			t.Errorf("%d KiB volume: freeBlocks=%d exceeds totalBlocks=%d (uint32 underflow)",
				size>>10, vh.FreeBlocks, vh.TotalBlocks)
		}
	}
}

// The catalog fork must lie inside the image. A fork whose extents run past the
// end of the volume is corruption the formatter wrote itself.
func TestMkfsKeepsSpecialFilesInsideTheImage(t *testing.T) {
	for _, size := range []int64{32 << 10, 256 << 10, 1 << 20, 4 << 20} {
		img, err := Mkfs(size, FormatConfig{Label: "FIT"})
		if err != nil {
			if errors.Is(err, ErrCorrupt) {
				continue
			}
			t.Fatalf("Mkfs(%d): %v", size, err)
		}
		v, err := OpenWritable(img, nil)
		if err != nil {
			t.Fatalf("OpenWritable(%d KiB): %v", size>>10, err)
		}
		vh := v.VolumeHeader()
		for name, fd := range map[string]forkData{
			"catalog":    vh.CatalogFile,
			"extents":    vh.ExtentsFile,
			"allocation": vh.AllocationFile,
		} {
			for _, e := range fd.Extents {
				if e.BlockCount == 0 {
					continue
				}
				if end := uint64(e.StartBlock) + uint64(e.BlockCount); end > uint64(vh.TotalBlocks) {
					t.Errorf("%d KiB volume: %s fork extent covers blocks %d..%d, past the %d blocks of the image",
						size>>10, name, e.StartBlock, end, vh.TotalBlocks)
				}
			}
		}
	}
}

// A volume too small for even a minimal catalog must be refused, not emitted.
func TestMkfsRefusesAnImpossiblySmallVolume(t *testing.T) {
	if _, err := Mkfs(1024, FormatConfig{}); !errors.Is(err, ErrCorrupt) {
		t.Fatalf("Mkfs(1024) = %v, want ErrCorrupt", err)
	}
}

// The clamp must not change volumes large enough to afford the full
// reservation: those are the ones validated against fsck_hfs.
func TestMkfsLeavesLargeVolumesUnchanged(t *testing.T) {
	img, err := Mkfs(4<<20, FormatConfig{Label: "GOTEST"})
	if err != nil {
		t.Fatalf("Mkfs: %v", err)
	}
	v, err := OpenWritable(img, nil)
	if err != nil {
		t.Fatalf("OpenWritable: %v", err)
	}
	if got := v.VolumeHeader().FreeBlocks; got != 763 {
		t.Errorf("4 MiB volume freeBlocks = %d, want 763 (the pre-existing layout)", got)
	}
}
