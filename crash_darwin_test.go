// Copyright (c) 2026, go-filesystems
// SPDX-License-Identifier: BSD-3-Clause

//go:build darwin

package hfsplus

import (
	"bytes"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

// fsckExit runs fsck_hfs with args on dev and returns its exit status and output.
func fsckExit(dev string, args ...string) (int, string) {
	out, err := exec.Command("fsck_hfs", append(args, dev)...).CombinedOutput()
	code := 0
	var ee *exec.ExitError
	if errors.As(err, &ee) {
		code = ee.ExitCode()
	}
	return code, string(out)
}

// TestDarwinCrashImagesFsck replays the log of a MkDir and of a WriteFile, in
// order and cut at every point (plus "everything but the alternate header"), on
// three kinds of image -- made by this driver, by hdiutil as HFS+, and by hdiutil
// as Journaled HFS+ -- and asks macOS's own fsck_hfs about each:
//
//   - an image still marked cleanly unmounted must pass `fsck_hfs -fn`;
//   - an image the crash left marked dirty must make the quick check macOS runs
//     before mounting (`fsck_hfs -q`) exit non-zero, i.e. get a full check. On a
//     journaled volume the unmounted bit alone does not do that (-q exits 0 and
//     trusts the journal), which is why the commit also clears the journaled bit
//     for its duration.
func TestDarwinCrashImagesFsck(t *testing.T) {
	requireTools(t)
	kinds := []struct{ name, path string }{
		{"go-mkfs", sparseImage(t, 8<<20)},
		{"apple-hfs", appleImage(t, "HFS+", "CRASHPLAIN")},
		{"apple-jnl", appleImage(t, "Journaled HFS+", "CRASHJNL")},
	}
	dir := t.TempDir()
	for _, kind := range kinds {
		orig, err := os.ReadFile(kind.path)
		if err != nil {
			t.Fatal(err)
		}
		journaled := orig[volumeHeaderOffset+6]&(kHFSVolumeJournaledBit>>8) != 0
		for _, op := range crashOps {
			work := filepath.Join(dir, kind.name+"-"+op.name+".img")
			if err := os.WriteFile(work, orig, 0o644); err != nil {
				t.Fatal(err)
			}
			v, err := OpenFileWritable(work)
			if err != nil {
				t.Fatalf("%s: %v", kind.name, err)
			}
			c := instrument(v)
			if err := op.run(v); err != nil {
				t.Fatalf("%s %s: %v", kind.name, op.name, err)
			}
			v.Close()
			ev := c.events

			type cand struct {
				name string
				img  []byte
			}
			var cands []cand
			for k := 0; k <= len(ev); k++ {
				img := bytes.Clone(orig)
				for _, e := range ev[:k] {
					if !e.sync {
						copy(img[e.off:], e.data)
					}
				}
				cands = append(cands, cand{fmt.Sprintf("prefix-%d/%d", k, len(ev)), img})
			}
			// The last group is not ordered on the disk: the primary header may
			// land without the alternate.
			for i := len(ev) - 1; i >= 0; i-- {
				if !ev[i].sync && ev[i].off == int64(len(orig))-1024 {
					img := bytes.Clone(orig)
					for j, e := range ev {
						if !e.sync && j != i {
							copy(img[e.off:], e.data)
						}
					}
					cands = append(cands, cand{"primary-without-alt", img})
					break
				}
			}
			clean, dirty := 0, 0
			for _, cd := range cands {
				f := filepath.Join(dir, "crash.img")
				if err := os.WriteFile(f, cd.img, 0o644); err != nil {
					t.Fatal(err)
				}
				bit := attrsOf(t, f)&kHFSVolumeUnmountedBit != 0
				dev := attachRaw(t, f, true)
				q, _ := fsckExit(dev, "-q")
				fn, out := fsckExit(dev, "-fn")
				detach(dev)
				name := fmt.Sprintf("%s %s %s", kind.name, op.name, cd.name)
				if bit {
					clean++
					if !strings.Contains(out, "appears to be OK") {
						t.Errorf("%s: marked cleanly unmounted but fsck_hfs -fn (exit %d) disagrees:\n%s", name, fn, out)
					}
				} else {
					dirty++
					if q == 0 {
						t.Errorf("%s: marked dirty (journaled volume: %v) but fsck_hfs -q exits 0, so macOS would not check it at mount", name, journaled)
					}
				}
			}
			if clean < 2 || dirty < 1 {
				t.Errorf("%s %s: %d clean and %d dirty images", kind.name, op.name, clean, dirty)
			}
		}
	}
}
