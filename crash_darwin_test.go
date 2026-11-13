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
// as Journaled HFS+ -- and asks macOS's own fsck_hfs about each.
//
// An image is mid-commit by its POSITION in the crash model: taken after the
// commit's first header write and before its final primary-header write. It is
// never classified by a header bit, because those are what the code under test
// sets, and a regression that restored a bit too early would then leave its
// images unpicked and unasserted.
//
//   - every mid-commit image must make the quick check macOS runs before
//     mounting (`fsck_hfs -q`) exit non-zero, i.e. get a full check, and their
//     number must equal the number of mid-commit images. On a journaled volume
//     the unmounted bit alone does not do that (-q exits 0 and trusts the
//     journal), which is why the commit also clears the journaled bit for its
//     duration;
//   - every other image still marked cleanly unmounted must pass `fsck_hfs -fn`.
func TestDarwinCrashImagesFsck(t *testing.T) {
	requireTools(t)
	// apple-jnl-dirty is a Journaled HFS+ volume with an empty journal and the
	// unmounted bit already clear (a macOS crash or an unplugged disk after a
	// journal checkpoint): fsck_hfs -fn passes, and a commit must still get a
	// half-written volume checked.
	dirtyJnl := appleImage(t, "Journaled HFS+", "CRASHDIRTY")
	{
		raw, err := os.ReadFile(dirtyJnl)
		if err != nil {
			t.Fatal(err)
		}
		raw[volumeHeaderOffset+6] &^= kHFSVolumeUnmountedBit >> 8
		raw[len(raw)-1024+6] &^= kHFSVolumeUnmountedBit >> 8
		if err := os.WriteFile(dirtyJnl, raw, 0o644); err != nil {
			t.Fatal(err)
		}
	}
	kinds := []struct {
		name, path  string
		dirtyAtOpen bool
	}{
		{"go-mkfs", sparseImage(t, 8<<20), false},
		{"apple-hfs", appleImage(t, "HFS+", "CRASHPLAIN"), false},
		{"apple-jnl", appleImage(t, "Journaled HFS+", "CRASHJNL"), false},
		{"apple-jnl-dirty", dirtyJnl, true},
	}
	dir := t.TempDir()
	for _, kind := range kinds {
		orig, err := os.ReadFile(kind.path)
		if err != nil {
			t.Fatal(err)
		}
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

			// The commit's first and final primary-header writes in the log.
			firstHdr, lastPrimary := -1, -1
			for i, e := range ev {
				if e.sync || e.off != volumeHeaderOffset {
					continue
				}
				if firstHdr < 0 {
					firstHdr = i
				}
				lastPrimary = i
			}
			if firstHdr < 0 {
				t.Fatalf("%s %s: no header write in the log", kind.name, op.name)
			}

			type cand struct {
				name string
				img  []byte
				mid  bool
			}
			var cands []cand
			mids := 0
			for k := 0; k <= len(ev); k++ {
				img := bytes.Clone(orig)
				for _, e := range ev[:k] {
					if !e.sync {
						copy(img[e.off:], e.data)
					}
				}
				// prefix-k applies events [0,k): it holds the first header write
				// when k > firstHdr and lacks the final primary one when
				// k <= lastPrimary.
				mid := k > firstHdr && k <= lastPrimary
				if mid {
					mids++
				}
				cands = append(cands, cand{name: fmt.Sprintf("prefix-%d/%d", k, len(ev)), img: img, mid: mid})
			}
			// The last group is not ordered on the disk: the primary header may
			// land without the alternate. It holds the final primary write, so it
			// is not mid-commit.
			for i := len(ev) - 1; i >= 0; i-- {
				if !ev[i].sync && ev[i].off == int64(len(orig))-1024 {
					img := bytes.Clone(orig)
					for j, e := range ev {
						if !e.sync && j != i {
							copy(img[e.off:], e.data)
						}
					}
					cands = append(cands, cand{name: "primary-without-alt", img: img})
					break
				}
			}
			if mids == 0 {
				t.Fatalf("%s %s: no mid-commit image (first header write %d, final primary %d of %d events)", kind.name, op.name, firstHdr, lastPrimary, len(ev))
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
				switch {
				case cd.mid:
					if q == 0 {
						t.Errorf("%s: mid-commit image but fsck_hfs -q exits 0, so macOS would not check it at mount", name)
					} else {
						dirty++
					}
				case kind.dirtyAtOpen:
					// Dirty before and after: the original and the finished
					// commit are the images outside the commit.
					if q != 0 && !bytes.Equal(cd.img, orig) {
						t.Logf("%s: fsck_hfs -q exits %d", name, q)
					}
				case bit:
					clean++
					if !strings.Contains(out, "appears to be OK") {
						t.Errorf("%s: marked cleanly unmounted but fsck_hfs -fn (exit %d) disagrees:\n%s", name, fn, out)
					}
				default:
					t.Errorf("%s: outside the commit but not marked cleanly unmounted", name)
				}
				if kind.dirtyAtOpen && cd.name == fmt.Sprintf("prefix-%d/%d", len(ev), len(ev)) &&
					!strings.Contains(out, "appears to be OK") {
					t.Errorf("%s: the finished commit does not pass fsck_hfs -fn:\n%s", name, out)
				}
			}
			if dirty != mids {
				t.Errorf("%s %s: %d of %d mid-commit images make fsck_hfs -q exit non-zero", kind.name, op.name, dirty, mids)
			}
			if !kind.dirtyAtOpen && clean < 2 {
				t.Errorf("%s %s: %d clean images", kind.name, op.name, clean)
			}
		}
	}
}
