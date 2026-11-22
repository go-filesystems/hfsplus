// Copyright (c) 2026, go-filesystems
// SPDX-License-Identifier: BSD-3-Clause

//go:build darwin

package hfsplus

import (
	"bytes"
	"crypto/sha256"
	"encoding/binary"
	"fmt"
	"os"
	"path/filepath"
	"slices"
	"sort"
	"strings"
	"testing"
)

// dOnly keeps the names this test created (macOS adds .fseventsd and friends at
// mount).
func dOnly(names []string) []string {
	var out []string
	for _, n := range names {
		if strings.HasPrefix(n, "d") {
			out = append(out, n)
		}
	}
	return out
}

// TestDarwinJournalWriterKernelReplay is the darwin half of the oracle of design
// section 9: the commits of a journal-writing session on a Journaled HFS+ image
// macOS made, every distinct crash image of their log (event prefixes, write
// subsets, torn sectors), each handed to macOS. Per image:
//
//  1. the kernel replay: hdiutil attach with mounting runs journal_open and
//     replay_journal; the mount lists exactly the volume after j commits, for a j
//     between the commits that had returned and the commits that had begun;
//  2. fsck_hfs -fn on the result exits 0;
//  3. the durability walk: this driver opens the result and lists the same;
//  4. lastMountedVersion is not 'FSK!' (the mount did not judge the journal bad);
//  5. on every 8th image, on a second copy, the writable fsck_hfs -fy path
//     (journal_replay by sysctl, SVerify1.c:4514-4538), then -fn exits 0. Where
//     this sandbox cannot open the raw device for writing, that is counted and
//     reported, not asserted.
//
// Opt-in (HFSPLUS_JOURNAL_ORACLE=1), like TestDarwinReplayMatchesKernel.
func TestDarwinJournalWriterKernelReplay(t *testing.T) {
	darwinKernelReplay(t, "JWORACLE", mkdirOps(4), nil, nil)
}

// mountView is what the mounted volume shows the oracle: names, and for files
// their size and checksum, in observeFiles' format.
func mountView(mnt string) []string {
	ents, _ := os.ReadDir(mnt)
	var out []string
	for _, e := range ents {
		if e.IsDir() {
			out = append(out, e.Name()+"/")
			continue
		}
		b, err := os.ReadFile(filepath.Join(mnt, e.Name()))
		if err != nil {
			out = append(out, e.Name()+":unreadable")
			continue
		}
		out = append(out, fmt.Sprintf("%s:%d:%x", e.Name(), len(b), sha256.Sum256(b)))
	}
	sort.Strings(out)
	return out
}

// TestDarwinJournalWriterDataKernelReplay is the same oracle with file contents
// (PR 4 of hfsplus#45): a small file through the ring, a large file in place
// before its barrier, an overwrite that reuses the freed blocks (the revoke
// case), each crash image handed to macOS, whose mount must show the bytes of a
// state between the commits that returned and the commits that had begun.
func TestDarwinJournalWriterDataKernelReplay(t *testing.T) {
	ops := []jwOp{
		wf("/d000", pat(3000, 1)),
		wf("/d001", pat(70000, 2)),
		rm("/d000"),
		wf("/d002", pat(1500, 3)),
	}
	darwinKernelReplay(t, "JWDATA", ops, observeFiles, mountView)
}

// TestDarwinJournalWriterFullKernelReplay (PR 5): journal-full commits, then
// Close's T_c, every crash image handed to macOS.
func TestDarwinJournalWriterFullKernelReplay(t *testing.T) {
	ops := []jwOp{mkdirOps(1)[0], fullOp("/d001"), mkdirOps(3)[2], fullOp("/d003")}
	darwinKernelReplayTail(t, "JWFULL", ops, func(v *Volume) error { return v.Close() }, nil, nil)
}

func darwinKernelReplay(t *testing.T, vol string, ops []jwOp, observe jwObserver, view func(string) []string) {
	darwinKernelReplayTail(t, vol, ops, nil, observe, view)
}

func darwinKernelReplayTail(t *testing.T, vol string, ops []jwOp, tail jwOp, observe jwObserver, view func(string) []string) {
	darwinKernelReplayAt(t, vol, DurabilityStrict, nil, ops, tail, observe, view)
}

// TestDarwinJournalWriterMountedKernelReplay: the same oracle at DurabilityMounted
// (a Mounted commit is the journal commit since PR 6), with a commit that falls
// back in the middle (a forged sector: journal emptied, written in place, bracketed,
// never held) followed by journal commits again, then Close. Every crash image goes
// to macOS: mount, fsck_hfs -fn, the durability walk.
func TestDarwinJournalWriterMountedKernelReplay(t *testing.T) {
	ops := []jwOp{
		mkdirOps(1)[0],
		func(v *Volume) error { return plantForgedMkDir(v, "/d001", binary.LittleEndian) },
		func(v *Volume) error { return v.MkDir("/d002", 0o755) },
		func(v *Volume) error { return v.MkDir("/d003", 0o755) },
	}
	// Op 1 is the direct commit: a crash inside it leaves the volume dirty by design
	// (fsck_hfs repairs it; TestDarwinCrashImagesFsck covers that path), so macOS may
	// refuse to mount those images. The oracle checks every other crash point, in
	// particular the journal commits before and after it.
	darwinKernelReplayAt(t, "JWMOUNT", DurabilityMounted, []int{1}, ops, func(v *Volume) error { return v.Close() }, nil, nil)
}

// darwinKernelReplayAt runs the kernel-replay oracle at a durability level; direct
// lists the ops that take the direct path, whose inner crash points are not offered
// to the mount.
func darwinKernelReplayAt(t *testing.T, vol string, dur Durability, direct []int, ops []jwOp, tail jwOp, observe jwObserver, view func(string) []string) {
	if os.Getenv("HFSPLUS_JOURNAL_ORACLE") != "1" {
		t.Skip("set HFSPLUS_JOURNAL_ORACLE=1")
	}
	requireTools(t)
	src := appleImage(t, "Journaled HFS+", vol)
	base, err := os.ReadFile(src)
	if err != nil {
		t.Fatal(err)
	}
	if observe == nil {
		observe = rootNames
	}
	if view == nil {
		view = func(mnt string) []string {
			ents, _ := os.ReadDir(mnt)
			var kn []string
			for _, e := range ents {
				kn = append(kn, e.Name())
			}
			sort.Strings(kn)
			return kn
		}
	}
	s := jwScenario{name: "apple-jnl", img: base, ops: ops, tail: tail, observe: observe, durability: dur}
	ev, starts, ends, states, _, syncs := runJWScenario(t, s)
	for i := range states {
		states[i] = dOnly(states[i])
	}
	sev, sstarts, sends := splitSectors(ev, starts, ends)

	type cand struct {
		name   string
		img    []byte
		lo, hi int
	}
	uniq := map[[32]byte]*cand{}
	var order [][32]byte
	var points map[int]bool
	if len(direct) > 0 {
		points = map[int]bool{}
		for p := 0; p <= len(sev); p++ {
			points[p] = true
		}
		for _, k := range direct {
			for p := sstarts[k] + 1; p < sends[k]; p++ {
				delete(points, p)
			}
		}
	}
	jwCrashImages(base, sev, sstarts, sends, points, func(name string, img []byte, lo, hi int) {
		k := sha256.Sum256(img)
		if c, ok := uniq[k]; ok {
			c.lo, c.hi = max(c.lo, lo), min(c.hi, hi)
			return
		}
		uniq[k] = &cand{name, img, lo, hi}
		order = append(order, k)
	})
	t.Logf("barriers per commit %v; %d events; %d distinct crash images", syncs, len(sev), len(order))
	step := 1
	if len(order) > 160 && os.Getenv("HFSPLUS_JOURNAL_ALL") != "1" { // ALL=1: every image, minutes
		step = (len(order) + 159) / 160
	}

	dir := t.TempDir()
	checked, fyRan, fyDenied, preOK, preBad := 0, 0, 0, 0, 0
	for i := 0; i < len(order); i += step {
		c := uniq[order[i]]
		if c.lo > c.hi {
			t.Errorf("%s: no commit count is allowed (%d..%d)", c.name, c.lo, c.hi)
			continue
		}
		checked++
		f := filepath.Join(dir, "crash.img")
		if err := os.WriteFile(f, c.img, 0o644); err != nil {
			t.Fatal(err)
		}
		// 6. Recorded, not asserted (design section 9, check 6): fsck_hfs -fn on the raw
		// image BEFORE the mount. It replays a non-empty journal into its own cache
		// and does not check block checksums, so it may disagree with the kernel on a
		// torn T_k.
		{
			pdev := attachRaw(t, f, true)
			if code, _ := fsckExit(pdev, "-fn"); code == 0 {
				preOK++
			} else {
				preBad++
			}
			detach(pdev)
		}
		// 1. The kernel replay, by mounting.
		dev, mnt, err := tryMountRaw(f)
		if err != nil {
			t.Errorf("%s: macOS cannot mount the crash image: %v", c.name, err)
			continue
		}
		kn := dOnly(view(mnt))
		detach(dev)
		match := false
		for j := c.lo; j <= c.hi && j < len(states); j++ {
			match = match || slices.Equal(kn, states[j])
		}
		if !match {
			t.Errorf("%s: the kernel's replay lists %v, allowed %d..%d (%v .. %v)", c.name, kn, c.lo, c.hi, states[c.lo], states[min(c.hi, len(states)-1)])
		}
		// 4. No 'FSK!'.
		after, _ := os.ReadFile(f)
		if got := string(after[vhLastMountedVersionOff : vhLastMountedVersionOff+4]); got == "FSK!" {
			t.Errorf("%s: the mount judged the journal bad (lastMountedVersion FSK!)", c.name)
		}
		// 2. fsck_hfs -fn.
		rdev := attachRaw(t, f, true)
		code, out := fsckExit(rdev, "-fn")
		detach(rdev)
		if code != 0 {
			t.Errorf("%s: fsck_hfs -fn exit %d after the kernel replay:\n%s", c.name, code, out)
		}
		// 3. The durability walk with this driver.
		if v, err := Open(bytesReaderAt(after), int64(len(after))); err != nil {
			t.Errorf("%s: this driver cannot open the result: %v", c.name, err)
		} else if names, err := observe(v); err != nil || !slices.Equal(dOnly(names), kn) {
			t.Errorf("%s: this driver lists %v (%v) after the kernel replay, the mount listed %v", c.name, dOnly(names), err, kn)
		}
		// 5. The writable fsck path, on a second copy of the crash image.
		if checked%8 == 1 {
			g := filepath.Join(dir, "fsck.img")
			if err := os.WriteFile(g, c.img, 0o644); err != nil {
				t.Fatal(err)
			}
			wdev := attachRaw(t, g, false)
			fy, fyOut := fsckExit(wdev, "-fy")
			detach(wdev)
			if strings.Contains(fyOut, "Permission denied") || strings.Contains(fyOut, "NO WRITE") {
				fyDenied++
			} else {
				fyRan++
				rdev := attachRaw(t, g, true)
				code, out := fsckExit(rdev, "-fn")
				detach(rdev)
				gb, _ := os.ReadFile(g)
				gv, err := Open(bytesReaderAt(gb), int64(len(gb)))
				var gn []string
				if err == nil {
					gn, _ = observe(gv)
				}
				if fy != 0 || code != 0 || !slices.Equal(dOnly(gn), kn) {
					t.Errorf("%s: writable fsck -fy exit %d, then -fn exit %d, lists %v, kernel mount listed %v\n%s\n%s", c.name, fy, code, dOnly(gn), kn, fyOut, out)
				}
			}
		}
	}
	t.Logf("%d images through the kernel replay, fsck -fn and the durability walk; writable fsck -fy ran on %d, was denied raw write access on %d; fsck -fn BEFORE the mount (recorded, not asserted): %d exit 0, %d not", checked, fyRan, fyDenied, preOK, preBad)
	if checked < 20 {
		t.Fatalf("only %d images checked", checked)
	}
	_ = bytes.Equal
	_ = fmt.Sprint
}

// TestDarwinJournalWriterFallbackFsck: journal commits, a commit with file data
// (the direct path entered right after the journal is emptied, writing 'GoHF'),
// journal commits again, then Close. After every commit, and after Close, the
// image must pass fsck_hfs -fn, mount, and list what was written (design
// section 7: UNVERIFIED until a run like this). Opt-in like the kernel replay test.
func TestDarwinJournalWriterFallbackFsck(t *testing.T) {
	if os.Getenv("HFSPLUS_JOURNAL_ORACLE") != "1" {
		t.Skip("set HFSPLUS_JOURNAL_ORACLE=1")
	}
	requireTools(t)
	src := appleImage(t, "Journaled HFS+", "JWFALL")
	work := filepath.Join(t.TempDir(), "w.img")
	b, _ := os.ReadFile(src)
	if err := os.WriteFile(work, b, 0o644); err != nil {
		t.Fatal(err)
	}
	v, err := OpenFileWritable(work)
	if err != nil {
		t.Fatal(err)
	}
	if err := v.EnableJournalWrites(); err != nil {
		t.Fatal(err)
	}
	ops := []jwOp{
		func(v *Volume) error { return v.MkDir("/d000", 0o755) },
		func(v *Volume) error { return v.MkDir("/d001", 0o755) },
		func(v *Volume) error { return v.WriteFile("/d002", fill(5000, 9), 0o644) },
		func(v *Volume) error { return v.MkDir("/d003", 0o755) },
		func(v *Volume) error { return v.MkDir("/d004", 0o755) },
		// PR 5: journal-full, then a bypass right after the journal is emptied (a forged
		// sector, 'GoHF' written), then journal commits again.
		fullOp("/d005"),
		func(v *Volume) error { return plantForgedMkDir(v, "/d006", binary.LittleEndian) },
		func(v *Volume) error { return v.MkDir("/d007", 0o755) },
	}
	check := func(label string, want []string) {
		t.Helper()
		snap, _ := os.ReadFile(work)
		f := filepath.Join(t.TempDir(), "snap.img")
		os.WriteFile(f, snap, 0o644)
		pre := attachRaw(t, f, true)
		code, out := fsckExit(pre, "-fn")
		detach(pre)
		if code != 0 {
			t.Errorf("%s: fsck_hfs -fn before the mount exit %d:\n%s", label, code, out)
		}
		dev, mnt, err := tryMountRaw(f)
		if err != nil {
			t.Errorf("%s: cannot mount: %v", label, err)
			return
		}
		ents, _ := os.ReadDir(mnt)
		var got []string
		for _, e := range ents {
			got = append(got, e.Name())
		}
		sort.Strings(got)
		detach(dev)
		if !slices.Equal(dOnly(got), want) {
			t.Errorf("%s: macOS lists %v, want %v", label, dOnly(got), want)
		}
		rdev := attachRaw(t, f, true)
		code, out = fsckExit(rdev, "-fn")
		detach(rdev)
		if code != 0 {
			t.Errorf("%s: fsck_hfs -fn after the mount exit %d:\n%s", label, code, out)
		}
	}
	names := []string{}
	for i, op := range ops {
		if err := op(v); err != nil {
			t.Fatal(err)
		}
		names = append(names, fmt.Sprintf("d%03d", i))
		check(fmt.Sprintf("after commit %d", i+1), slices.Clone(names))
	}
	if err := v.Close(); err != nil {
		t.Fatal(err)
	}
	check("after Close", names)
}
