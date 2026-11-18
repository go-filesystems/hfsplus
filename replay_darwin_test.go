// Copyright (c) 2026, go-filesystems
// SPDX-License-Identifier: BSD-3-Clause

//go:build darwin

package hfsplus

import (
	"bytes"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"testing"
	"time"
)

// macJournalSnapshot mounts a Journaled HFS+ image, creates files without syncing
// while cloning the backing image (an atomic clonefile, `cp -c`) again and again,
// and returns the first clone whose journal holds transactions (start != end):
// macOS logs a transaction before it checkpoints it, and the window is short. It
// returns "" when none of the clones caught the window.
func macJournalSnapshot(t *testing.T) string {
	t.Helper()
	src := appleImage(t, "Journaled HFS+", "RPLYORC")
	dev, mnt := mountRaw(t, src)
	defer detach(dev)

	stop := make(chan struct{})
	var wg sync.WaitGroup
	wg.Add(1)
	go func() {
		defer wg.Done()
		for i := 0; ; i++ {
			select {
			case <-stop:
				return
			default:
			}
			name := filepath.Join(mnt, fmt.Sprintf("f%03d.txt", i%200))
			body := []byte(strings.Repeat(fmt.Sprintf("%d,", i), 50+i%97))
			if err := os.WriteFile(name, body, 0o644); err != nil {
				return
			}
		}
	}()
	defer func() { close(stop); wg.Wait() }()

	dir := t.TempDir()
	deadline := time.Now().Add(40 * time.Second)
	for i := 0; time.Now().Before(deadline); i++ {
		clone := filepath.Join(dir, fmt.Sprintf("clone%03d.img", i))
		if out, err := exec.Command("cp", "-c", src, clone).CombinedOutput(); err != nil {
			t.Logf("cp -c unavailable: %v: %s", err, out)
			return ""
		}
		f, err := os.Open(clone)
		if err != nil {
			t.Fatal(err)
		}
		st, _ := f.Stat()
		vh, err := readVolumeHeader(f)
		var jst journalState
		if err == nil {
			jst, _, err = loadJournalAt(f, st.Size(), vh)
		}
		f.Close()
		if err == nil && !jst.empty {
			t.Logf("clone %d caught a non-empty journal", i)
			return clone
		}
		if err != nil {
			t.Logf("clone %d: %v", i, err)
		}
		os.Remove(clone)
	}
	return ""
}

// tryMountRaw is mountRaw returning its error.
func tryMountRaw(path string) (dev, mnt string, err error) {
	cmd := exec.Command("hdiutil", "attach", "-imagekey", "diskimage-class=CRawDiskImage", path)
	var stderr strings.Builder
	cmd.Stderr = &stderr
	out, err := cmd.Output()
	if err != nil {
		return "", "", fmt.Errorf("%v: %s%s", err, out, stderr.String())
	}
	return parseAttach(string(out))
}

// TestDarwinReplayMatchesKernel compares this driver's replay with the kernel's.
// It needs a NON-EMPTY journal written by macOS, which design section 10 marks
// UNVERIFIED to obtain; macJournalSnapshot tries to catch one. The clone is
// opened with WithJournalReplay and, separately, mounted by macOS (whose mount
// replays the journal); the two listings and contents must agree.
//
// Opt-in (HFSPLUS_JOURNAL_ORACLE=1). If no clone has a non-empty journal the test
// skips and says so: it never builds the journal itself.
func TestDarwinReplayMatchesKernel(t *testing.T) {
	if os.Getenv("HFSPLUS_JOURNAL_ORACLE") != "1" {
		t.Skip("set HFSPLUS_JOURNAL_ORACLE=1")
	}
	requireTools(t)
	clone := macJournalSnapshot(t)
	if clone == "" {
		t.Skip("no clone caught macOS with a non-empty journal: UNVERIFIED")
	}
	snap, err := os.ReadFile(clone)
	if err != nil {
		t.Fatal(err)
	}
	vh, _ := readVolumeHeader(bytesReaderAt(snap))
	_, loc, _ := loadJournalAt(bytesReaderAt(snap), int64(len(snap)), vh)
	t.Logf("macOS-written journal: start %#x, end %#x, size %#x, blhdr_size %#x, jhdr_size %d, sequence %d",
		loc.hdr.start, loc.hdr.end, loc.hdr.size, loc.hdr.blhdrSize, loc.hdr.jhdrSize, loc.hdr.sequenceNum)
	// The fixture, for the committed corpus and a regression test.
	if dir := os.Getenv("HFSPLUS_SAVE_JOURNAL"); dir != "" {
		jb := snap[loc.off : loc.off+loc.hdr.size]
		if err := os.WriteFile(filepath.Join(dir, "mac-nonempty-journal.bin"), jb, 0o644); err != nil {
			t.Fatal(err)
		}
	}

	// Ours.
	v, err := Open(bytesReaderAt(snap), int64(len(snap)), WithJournalReplay())
	if err != nil {
		t.Fatalf("replay of a journal macOS wrote: %v", err)
	}
	got := map[string][]byte{}
	ents, err := v.ListDir("/")
	if err != nil {
		t.Fatal(err)
	}
	for _, e := range ents {
		if strings.HasPrefix(e.Name(), "f") {
			b, err := v.ReadFile("/" + e.Name())
			if err != nil {
				t.Fatal(err)
			}
			got[e.Name()] = b
		}
	}

	// The kernel's: mount a copy, which replays.
	work := filepath.Join(t.TempDir(), "k.img")
	if err := os.WriteFile(work, snap, 0o644); err != nil {
		t.Fatal(err)
	}
	// fsck_hfs -n on the raw device first: what it says about the journal.
	{
		rdev := attachRaw(t, work, true)
		code, out := fsckExit(rdev, "-fn")
		t.Logf("fsck_hfs -fn on the clone: exit %d\n%s", code, out)
		detach(rdev)
	}
	kdev, kmnt, merr := tryMountRaw(work)
	if merr != nil {
		t.Fatalf("macOS cannot mount the clone: %v", merr)
	}
	want := map[string][]byte{}
	kents, err := os.ReadDir(kmnt)
	if err != nil {
		detach(kdev)
		t.Fatal(err)
	}
	for _, e := range kents {
		if strings.HasPrefix(e.Name(), "f") {
			b, err := os.ReadFile(filepath.Join(kmnt, e.Name()))
			if err != nil {
				detach(kdev)
				t.Fatal(err)
			}
			want[e.Name()] = b
		}
	}
	detach(kdev)

	keys := func(m map[string][]byte) string {
		var k []string
		for n := range m {
			k = append(k, n)
		}
		sort.Strings(k)
		return strings.Join(k, ",")
	}
	// Control: the same clone read WITHOUT replay. If it already showed every file,
	// the comparison below would not need the replay.
	plain, err := Open(bytesReaderAt(snap), int64(len(snap)))
	if err != nil {
		t.Fatal(err)
	}
	pents, _ := plain.ListDir("/")
	nplain := 0
	for _, e := range pents {
		if strings.HasPrefix(e.Name(), "f") {
			nplain++
		}
	}
	t.Logf("kernel sees %d files, this driver %d with replay, %d without", len(want), len(got), nplain)
	if keys(got) != keys(want) {
		t.Errorf("listing differs:\n replay %s\n kernel %s", keys(got), keys(want))
	}
	for n, b := range want {
		if !bytes.Equal(got[n], b) {
			t.Errorf("%s: replay has %d bytes, kernel %d", n, len(got[n]), len(b))
		}
	}
}
