// Copyright (c) 2026, go-filesystems
// SPDX-License-Identifier: BSD-3-Clause

package hfsplus

import (
	"errors"
	"strings"
	"testing"
)

// The guards of EnableJournalWrites and of the journal's entry points, one by one
// (mutsweep): each refusal is asserted by the reason it gives, not just its class.
func TestEnableJournalWritesReasons(t *testing.T) {
	const size = 0x80000
	reason := func(err error, want string) {
		t.Helper()
		if !errors.Is(err, ErrJournalWritesUnavailable) || !strings.Contains(err.Error(), want) {
			t.Errorf("err = %v, want ErrJournalWritesUnavailable mentioning %q", err, want)
		}
	}
	plain, err := Mkfs(8<<20, FormatConfig{Label: "PLAIN"})
	if err != nil {
		t.Fatal(err)
	}
	v, err := OpenWritable(plain, nil)
	if err != nil {
		t.Fatal(err)
	}
	reason(v.EnableJournalWrites(), "not journaled")

	// A journal info block that says "needs init": the header holds nothing yet.
	img, _, _ := journaledImage(t, kJIJournalInFSMask|kJIJournalNeedInitMask, make([]byte, 512), size)
	if v, err = OpenWritable(img, nil); err == nil {
		reason(v.EnableJournalWrites(), "never initialised")
	}

	// Calling it twice keeps the first writer (its state is the session's).
	img, _ = journalImage(t, size, 1000, 0x4400)
	v, err = OpenWritable(img, nil)
	if err != nil {
		t.Fatal(err)
	}
	if err := v.EnableJournalWrites(); err != nil {
		t.Fatal(err)
	}
	first := v.jw
	if err := v.MkDir("/a", 0o755); err != nil {
		t.Fatal(err)
	}
	if err := v.EnableJournalWrites(); err != nil || v.jw != first || !v.journaling() {
		t.Errorf("second call: %v, writer replaced %v, journaling %v", err, v.jw != first, v.journaling())
	}
	// A closed volume.
	if err := v.Close(); err != nil {
		t.Fatal(err)
	}
	if err := v.EnableJournalWrites(); !errors.Is(err, ErrClosed) {
		t.Errorf("closed: %v, want ErrClosed", err)
	}
}

// A region that cannot be read stops the commit before anything is written.
func TestJournalWriterRegionErrors(t *testing.T) {
	for _, which := range []string{"primary", "alternate"} {
		img, _ := journalImage(t, 0x80000, 1000, 0x4400)
		v, c, _ := jwFile(t, img, true)
		off := int64(volumeHeaderOffset)
		if which == "alternate" {
			if _, err := v.region(volumeHeaderOffset, 512); err != nil {
				t.Fatal(err)
			}
			off = v.altOff
		}
		if v.regions == nil {
			v.regions = map[int64]*region{}
		}
		v.regions[off] = &region{off: off, buf: make([]byte, 100), orig: make([]byte, 100)}
		n := len(c.events)
		if done, err := v.flushJournal(nil); done || err == nil || len(c.events) != n {
			t.Errorf("%s header region unreadable: done=%v err=%v, %d events", which, done, err, len(c.events)-n)
		}
		delete(v.regions, off)
		v.Close()
	}
}

// A commit with nothing to write (both headers already say what the journal path
// would write, no run changed) writes nothing and does not claim to have committed.
func TestJournalWriterNothingToWrite(t *testing.T) {
	img, _ := journalImage(t, 0x80000, 1000, 0x4400)
	v, c, _ := jwFile(t, img, true)
	defer v.Close()
	if v.regions == nil {
		v.regions = map[int64]*region{}
	}
	for _, off := range []int64{volumeHeaderOffset, v.altOff} {
		b := make([]byte, 512)
		if _, err := v.readUnder(b, off); err != nil {
			t.Fatal(err)
		}
		copy(b[vhLastMountedVersion:], "HFSJ")
		b[vhAttributes+2] &^= byte(kHFSVolumeUnmountedBit >> 8)
		b[vhAttributes+2] |= byte(kHFSVolumeJournaledBit >> 8)
		v.regions[off] = &region{off: off, buf: b, orig: append([]byte(nil), b...)}
	}
	n := len(c.events)
	if done, err := v.flushJournal(nil); done || err != nil || len(c.events) != n || v.journaling() {
		t.Errorf("done=%v err=%v, %d events, journaling %v", done, err, len(c.events)-n, v.journaling())
	}
}

// SetDurability: a read-only volume refuses, and asking for the level it already
// has changes nothing (the journal stays live, nothing is written).
func TestSetDurabilityEdges(t *testing.T) {
	img, _ := journalImage(t, 0x80000, 1000, 0x4400)
	ro := mustOpen(t, img, false)
	if err := ro.SetDurability(DurabilityMounted); !errors.Is(err, ErrReadOnly) {
		t.Errorf("read-only: %v, want ErrReadOnly", err)
	}
	v, c, _ := jwFile(t, img, true)
	defer v.Close()
	if err := v.MkDir("/a", 0o755); err != nil {
		t.Fatal(err)
	}
	n := len(c.events)
	if err := v.SetDurability(v.Durability()); err != nil || !v.journaling() || len(c.events) != n {
		t.Errorf("same level: %v, journaling %v, %d events", err, v.journaling(), len(c.events)-n)
	}
	// At a level that holds writes back, asking for the same level does not flush them.
	if err := v.SetDurability(DurabilityLazy); err != nil {
		t.Fatal(err)
	}
	if err := v.MkDir("/b", 0o755); err != nil {
		t.Fatal(err)
	}
	n, s := len(c.events), c.syncs
	if err := v.SetDurability(DurabilityLazy); err != nil || len(c.events) != n || c.syncs != s || v.grp == nil {
		t.Errorf("same level, group pending: %v, %d events, %d barriers, group %v", err, len(c.events)-n, c.syncs-s, v.grp != nil)
	}
}

// A journal that holds transactions this writer did not write is not appended to.
func TestJournalWriterRefusesForeignTransactions(t *testing.T) {
	img, jbase := journalImage(t, 0x80000, 1000, 0x4400)
	v, c, _ := jwFile(t, img, true)
	defer v.Close()
	hs, err := v.jw.headerSector(v, 0x4400, 0x5400, 1001)
	if err != nil {
		t.Fatal(err)
	}
	if err := v.putAt(hs, jbase); err != nil {
		t.Fatal(err)
	}
	n := len(c.events)
	if done, err := v.flushJournal([]writeRun{{off: 2 << 20, data: fill(512, 3)}}); done || !errors.Is(err, ErrJournaled) || len(c.events) != n {
		t.Errorf("done=%v err=%v, %d events written", done, err, len(c.events)-n)
	}
}

// With no transaction live, emptying the journal and closing it write nothing; Close
// leaves the clean mark only when the volume was clean at open.
func TestJournalWriterCloseEdges(t *testing.T) {
	img, _ := journalImage(t, 0x80000, 1000, 0x4400)
	v, c, _ := jwFile(t, img, true)
	if err := v.emptyJournal(); err != nil || len(c.events) != 0 {
		t.Errorf("emptyJournal with nothing live: %v, %d events", err, len(c.events))
	}
	if err := v.closeJournal(); err != nil || len(c.events) != 0 || c.syncs != 0 {
		t.Errorf("closeJournal with nothing live: %v, %d events, %d barriers", err, len(c.events), c.syncs)
	}
	if err := v.Close(); err != nil {
		t.Fatal(err)
	}

	// A volume that was not clean at open: Close empties the journal (2 barriers)
	// and leaves the unmounted bit clear.
	dirty, _ := journalImage(t, 0x80000, 1000, 0x4400)
	for _, off := range []int{volumeHeaderOffset, len(dirty) - 1024} {
		dirty[off+vhAttributes+2] &^= byte(kHFSVolumeUnmountedBit >> 8)
	}
	v, c, path := jwFile(t, dirty, true)
	if v.cleanOnDisk {
		t.Fatal("the fixture is clean")
	}
	if err := v.MkDir("/a", 0o755); err != nil {
		t.Fatal(err)
	}
	before := c.syncs
	if err := v.Close(); err != nil {
		t.Fatal(err)
	}
	if got := c.syncs - before; got != 2 {
		t.Errorf("Close of a volume not clean at open paid %d barriers, want 2", got)
	}
	b := mustRead(t, path)
	if b[volumeHeaderOffset+vhAttributes+2]&byte(kHFSVolumeUnmountedBit>>8) != 0 {
		t.Error("Close marked a volume clean that was not clean at open")
	}
}
