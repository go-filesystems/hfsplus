// Copyright (c) 2026, go-filesystems
// SPDX-License-Identifier: BSD-3-Clause

//go:build darwin

package hfsplus

import (
	"bytes"
	"encoding/binary"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

// TestDarwinMountOfCrashedJournaled mounts, read/write, a Journaled HFS+ image
// left in each state a crash of a MkDir can leave, and logs what macOS does: does
// it check the volume, does the volume come up journaled, what do the header
// attributes say after the unmount. Opt-in (HFSPLUS_MOUNT_EXPERIMENT=1): it is an
// experiment whose result is documented in the README, not a gate.
func TestDarwinMountOfCrashedJournaled(t *testing.T) {
	if os.Getenv("HFSPLUS_MOUNT_EXPERIMENT") != "1" {
		t.Skip("set HFSPLUS_MOUNT_EXPERIMENT=1")
	}
	requireTools(t)
	src := appleImage(t, "Journaled HFS+", "MNTJNL")
	orig, err := os.ReadFile(src)
	if err != nil {
		t.Fatal(err)
	}
	work := filepath.Join(t.TempDir(), "w.img")
	if err := os.WriteFile(work, orig, 0o644); err != nil {
		t.Fatal(err)
	}
	v, err := OpenFileWritable(work)
	if err != nil {
		t.Fatal(err)
	}
	c := instrument(v)
	if err := v.MkDir("/d", 0o755); err != nil {
		t.Fatal(err)
	}
	v.Close()
	ev := c.events
	// The states: after the bracket write only; after the B-tree and bitmap
	// sectors (headers not yet written); complete.
	var firstHdr, lastSync int
	var syncs []int
	for i, e := range ev {
		if e.sync {
			lastSync = i
			syncs = append(syncs, i)
		}
	}
	_ = lastSync
	metaSync := syncs[len(syncs)-2] // the fsync after the B-tree and bitmap sectors
	for i, e := range ev {
		if !e.sync && e.off == volumeHeaderOffset {
			firstHdr = i
			break
		}
	}
	states := map[string]int{"bracket-only": firstHdr + 1, "metadata-written": metaSync + 1, "alternate-header-written": metaSync + 2, "complete": len(ev)}
	for name, k := range states {
		img := bytes.Clone(orig)
		for _, e := range ev[:k] {
			if !e.sync {
				copy(img[e.off:], e.data)
			}
		}
		f := filepath.Join(t.TempDir(), name+".img")
		if err := os.WriteFile(f, img, 0o644); err != nil {
			t.Fatal(err)
		}
		before := binary.BigEndian.Uint32(img[volumeHeaderOffset+4:])
		dev, mnt := mountRaw(t, f)
		info, _ := exec.Command("diskutil", "info", mnt).CombinedOutput()
		var keep []string
		for _, l := range strings.Split(string(info), "\n") {
			if strings.Contains(l, "Journal") || strings.Contains(l, "File System Personality") || strings.Contains(l, "Read-Only Volume") {
				keep = append(keep, strings.TrimSpace(l))
			}
		}
		detach(dev)
		after, _ := os.ReadFile(f)
		a := binary.BigEndian.Uint32(after[volumeHeaderOffset+4:])
		rdev := attachRaw(t, f, true)
		_, fn := fsckExit(rdev, "-fn")
		detach(rdev)
		t.Logf("%s: attributes before mount %#x (journaled %v, unmounted %v); mounted rw as %q; after unmount %#x (journaled %v, unmounted %v); fsck -fn after: %q",
			name, before, before&kHFSVolumeJournaledBit != 0, before&kHFSVolumeUnmountedBit != 0, keep,
			a, a&kHFSVolumeJournaledBit != 0, a&kHFSVolumeUnmountedBit != 0, lastLine(fn))
	}
}

// lastLine is the last non-empty line of s.
func lastLine(s string) string {
	lines := strings.Split(strings.TrimSpace(s), "\n")
	return lines[len(lines)-1]
}
