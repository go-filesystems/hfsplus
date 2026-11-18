// Copyright (c) 2026, go-filesystems
// SPDX-License-Identifier: BSD-3-Clause

//go:build darwin

package hfsplus

import (
	"bytes"
	"compress/gzip"
	"crypto/sha256"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
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

// kernelListing mounts a copy of snap, which makes macOS replay its journal, and
// returns the "name size sha256" lines of its f* files.
func kernelListing(t *testing.T, snap []byte) ([]string, error) {
	work := filepath.Join(t.TempDir(), "k.img")
	if err := os.WriteFile(work, snap, 0o644); err != nil {
		t.Fatal(err)
	}
	kdev, kmnt, err := tryMountRaw(work)
	if err != nil {
		return nil, err
	}
	defer detach(kdev)
	ents, err := os.ReadDir(kmnt)
	if err != nil {
		t.Fatal(err)
	}
	var out []string
	for _, e := range ents {
		if strings.HasPrefix(e.Name(), "f") {
			b, err := os.ReadFile(filepath.Join(kmnt, e.Name()))
			if err != nil {
				t.Fatal(err)
			}
			out = append(out, fmt.Sprintf("%s %d %x", e.Name(), len(b), sha256.Sum256(b)))
		}
	}
	sort.Strings(out)
	return out, nil
}

// TestDarwinReplayMatchesKernel compares this driver's replay with the kernel's.
// It needs a NON-EMPTY journal written by macOS, which design section 10 marks
// UNVERIFIED to obtain; macJournalSnapshot tries to catch one. The clone is
// opened with WithJournalReplay and, separately, mounted by macOS (whose mount
// replays the journal); the two listings and contents must agree. The CONTROL is
// asserted: the clone read without replay must differ from the kernel's listing,
// else the comparison would pass without the replay doing anything. A clone
// where it does not differ is not a result; the test tries again, and skips
// (never passes) when no attempt gave a clone with something pending.
//
// Opt-in (HFSPLUS_JOURNAL_ORACLE=1). HFSPLUS_SAVE_JOURNAL=<dir> writes the first
// good clone there as testdata would hold it: macos-nonempty-journal.img.gz and
// macos-nonempty-journal.golden.
func TestDarwinReplayMatchesKernel(t *testing.T) {
	if os.Getenv("HFSPLUS_JOURNAL_ORACLE") != "1" {
		t.Skip("set HFSPLUS_JOURNAL_ORACLE=1")
	}
	requireTools(t)
	const attempts = 6
	for attempt := 0; attempt < attempts; attempt++ {
		clone := macJournalSnapshot(t)
		if clone == "" {
			t.Logf("attempt %d: no clone caught a non-empty journal", attempt)
			continue
		}
		snap, err := os.ReadFile(clone)
		if err != nil {
			t.Fatal(err)
		}
		vh, _ := readVolumeHeader(bytesReaderAt(snap))
		_, loc, _ := loadJournalAt(bytesReaderAt(snap), int64(len(snap)), vh)
		t.Logf("attempt %d: macOS-written journal: start %#x, end %#x, size %#x, blhdr_size %#x, jhdr_size %d, sequence %d",
			attempt, loc.hdr.start, loc.hdr.end, loc.hdr.size, loc.hdr.blhdrSize, loc.hdr.jhdrSize, loc.hdr.sequenceNum)

		want, err := kernelListing(t, snap)
		if err != nil {
			t.Logf("attempt %d: macOS cannot mount the clone: %v", attempt, err)
			continue
		}
		plain, err := Open(bytesReaderAt(snap), int64(len(snap)))
		if err != nil {
			t.Fatal(err)
		}
		plainList := fileDigests(t, plain)
		if slices.Equal(plainList, want) {
			t.Logf("attempt %d: the clone shows all %d files without replay; no control, trying again", attempt, len(want))
			continue
		}
		v, err := Open(bytesReaderAt(snap), int64(len(snap)), WithJournalReplay())
		if err != nil {
			t.Fatalf("replay of a journal macOS wrote: %v", err)
		}
		got := fileDigests(t, v)
		t.Logf("kernel sees %d files, this driver %d with replay, %d without", len(want), len(got), len(plainList))
		if !slices.Equal(got, want) {
			t.Errorf("replay differs from the kernel; first difference: %s", firstDiff(got, want))
		}
		if dir := os.Getenv("HFSPLUS_SAVE_JOURNAL"); dir != "" {
			var zb bytes.Buffer
			zw, _ := gzip.NewWriterLevel(&zb, gzip.BestCompression)
			zw.Write(snap)
			zw.Close()
			if err := os.WriteFile(filepath.Join(dir, "macos-nonempty-journal.img.gz"), zb.Bytes(), 0o644); err != nil {
				t.Fatal(err)
			}
			if err := os.WriteFile(filepath.Join(dir, "macos-nonempty-journal.golden"), []byte(strings.Join(want, "\n")+"\n"), 0o644); err != nil {
				t.Fatal(err)
			}
		}
		return
	}
	t.Skipf("none of %d attempts gave a clone with a non-empty journal that the kernel's replay changed: UNVERIFIED", attempts)
}
