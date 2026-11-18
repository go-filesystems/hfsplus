// Copyright (c) 2026, go-filesystems
// SPDX-License-Identifier: BSD-3-Clause

//go:build darwin

package hfsplus

import (
	"os"
	"path/filepath"
	"slices"
	"testing"
)

// TestDarwinWriteIntoJournaledImageWithTransactions: OpenFileWritable on the
// macOS-written fixture (a journal with transactions), write a file, Close; then
// macOS attaches it and must list golden + the new file, and fsck_hfs -fn must
// exit 0. The control: the same write WITHOUT our replay step (the journal
// emptied by hand, as an open that skipped the replay would leave it) mounts to a
// listing that lacks the golden files, so the comparison is not vacuous.
//
// Opt-in (HFSPLUS_JOURNAL_ORACLE=1), like TestDarwinReplayMatchesKernel.
func TestDarwinWriteIntoJournaledImageWithTransactions(t *testing.T) {
	if os.Getenv("HFSPLUS_JOURNAL_ORACLE") != "1" {
		t.Skip("set HFSPLUS_JOURNAL_ORACLE=1")
	}
	requireTools(t)
	img0, _, _ := macNonEmptyFixture(t)
	golden := goldenList(t)
	body := []byte("written by the Go side after the replay\n")
	want := withNew(golden, "fnew.txt", body)

	kernelAfter := func(t *testing.T, path string, assertFsck bool) []string {
		t.Helper()
		dev := attachRaw(t, path, true)
		code, out := fsckExit(dev, "-fn")
		detach(dev)
		if code != 0 && assertFsck {
			t.Errorf("fsck_hfs -fn exit %d:\n%s", code, out)
		}
		t.Logf("fsck_hfs -fn exit %d (asserted: %v)", code, assertFsck)
		snap, err := os.ReadFile(path)
		if err != nil {
			t.Fatal(err)
		}
		got, err := kernelListing(t, snap)
		if err != nil {
			t.Fatalf("macOS cannot mount the result: %v", err)
		}
		return got
	}

	// With our replay.
	path := filepath.Join(t.TempDir(), "w.img")
	if err := os.WriteFile(path, img0, 0o644); err != nil {
		t.Fatal(err)
	}
	v, err := OpenFileWritable(path)
	if err != nil {
		t.Fatal(err)
	}
	if err := v.WriteFile("/fnew.txt", body, 0o644); err != nil {
		t.Fatal(err)
	}
	if err := v.Close(); err != nil {
		t.Fatal(err)
	}
	got := kernelAfter(t, path, true)
	if !slices.Equal(got, want) {
		t.Errorf("macOS lists %d files, want golden + new = %d; first difference: %s", len(got), len(want), firstDiff(got, want))
	}

	// Control: no replay step. The journal is emptied without writing its blocks.
	ctl := filepath.Join(t.TempDir(), "c.img")
	img := append([]byte(nil), img0...)
	vh, _ := readVolumeHeader(bytesReaderAt(img))
	_, loc, _ := loadJournalAt(bytesReaderAt(img), int64(len(img)), vh)
	hdr := img[loc.off : loc.off+uint64(loc.hdr.jhdrSize)]
	loc.hdr.bo.PutUint64(hdr[8:], loc.hdr.end) // start = end
	clear(hdr[36:40])
	loc.hdr.bo.PutUint32(hdr[36:], journalChecksum(hdr[:journalHeaderLen]))
	if err := os.WriteFile(ctl, img, 0o644); err != nil {
		t.Fatal(err)
	}
	cv, err := OpenFileWritable(ctl)
	if err != nil {
		t.Fatal(err)
	}
	if err := cv.WriteFile("/fnew.txt", body, 0o644); err != nil {
		t.Fatal(err)
	}
	cv.Close()
	cgot := kernelAfter(t, ctl, false) // the control volume is torn: its fsck is not the point
	if slices.Equal(cgot, want) {
		t.Error("the control (no replay) lists golden + new as well: the comparison proves nothing")
	}
	t.Logf("with replay: %d files; without: %d; expected %d", len(got), len(cgot), len(want))
}
