// Copyright (c) 2026, go-filesystems
// SPDX-License-Identifier: BSD-3-Clause

package hfsplus

import (
	"bytes"
	"errors"
	"slices"
	"testing"
)

// hfsplus#45, PR 6: journal writes are on by default on a journaled volume, at
// Strict and at Mounted, with no call to EnableJournalWrites.

// noJournal turns the default journal path off for a test that asserts the direct
// (in-place) path on a journaled volume.
func noJournal(v *Volume) {
	v.jw = nil
	v.jwReason = errors.New("test: journal writes off")
}

func TestJournalWritesDefaultOn(t *testing.T) {
	for _, d := range []Durability{DurabilityStrict, DurabilityMounted} {
		t.Run(d.String(), func(t *testing.T) {
			img, jbase := journalImage(t, 0x80000, 1000, 0x4400)
			v, c, path := jwFile(t, img, false) // no EnableJournalWrites
			if v.jw == nil || v.JournalWritesReason() != nil {
				t.Fatalf("not on after open: jw=%v reason=%v", v.jw, v.JournalWritesReason())
			}
			if err := v.SetDurability(d); err != nil {
				t.Fatal(err)
			}
			var counts []int
			for _, n := range []string{"/a", "/b", "/c"} {
				before := c.syncs
				if err := v.MkDir(n, 0o755); err != nil {
					t.Fatal(err)
				}
				counts = append(counts, c.syncs-before)
			}
			// The first commit of a session is 2 barriers, then 1 each, at both levels.
			if !slices.Equal(counts, []int{2, 1, 1}) {
				t.Errorf("barriers per commit at %v: %v, want [2 1 1]", d, counts)
			}
			if v.held || v.unsynced {
				t.Errorf("at %v the volume was held dirty (held=%v unsynced=%v): the journal needs no dirty span", d, v.held, v.unsynced)
			}
			// A second EnableJournalWrites is a no-op.
			first := v.jw
			if err := v.EnableJournalWrites(); err != nil || v.jw != first || !v.journaling() {
				t.Errorf("EnableJournalWrites on a volume already on: %v", err)
			}
			if h := journalHeaderOf(t, mustRead(t, path), jbase); h.start == h.end {
				t.Error("the journal is empty after three commits")
			}
			if err := v.Close(); err != nil {
				t.Fatal(err)
			}
			if names, _ := rootNames(mustOpen(t, mustRead(t, path), false)); !slices.Equal(names, []string{"a", "b", "c"}) {
				t.Errorf("after Close: %v", names)
			}
		})
	}
}

// Strict <-> Mounted is a switch between two journal-path levels: no emptying, the
// transaction chain goes on; Lazy still empties.
func TestJournalWritesStrictMountedSwitch(t *testing.T) {
	img, jbase := journalImage(t, 0x80000, 1000, 0x4400)
	v, c, path := jwFile(t, img, false)
	defer v.Close()
	must := func(err error) {
		t.Helper()
		if err != nil {
			t.Fatal(err)
		}
	}
	must(v.MkDir("/a", 0o755))
	seq := v.jw.lastSeq
	before := c.syncs
	must(v.SetDurability(DurabilityMounted))
	if c.syncs != before || !v.journaling() {
		t.Errorf("Strict -> Mounted: %d barriers, journaling %v; want none, still live", c.syncs-before, v.journaling())
	}
	before = c.syncs
	must(v.MkDir("/b", 0o755))
	if got := c.syncs - before; got != 1 || v.jw.lastSeq != seq+1 {
		t.Errorf("a Mounted commit after a Strict one: %d barriers, seq %d, want 1, %d", got, v.jw.lastSeq, seq+1)
	}
	before = c.syncs
	must(v.SetDurability(DurabilityStrict))
	if c.syncs != before || !v.journaling() {
		t.Errorf("Mounted -> Strict: %d barriers, journaling %v; want none, still live", c.syncs-before, v.journaling())
	}
	must(v.SetDurability(DurabilityLazy))
	if v.journaling() {
		t.Error("SetDurability(Lazy) left the journal live")
	}
	if h := journalHeaderOf(t, mustRead(t, path), jbase); h.start != h.end {
		t.Errorf("journal not empty after leaving the journal levels: %d %d", h.start, h.end)
	}
	must(v.MkDir("/c", 0o755)) // Lazy: in place, held dirty
	if !v.held {
		t.Error("a Lazy commit on a journaled volume did not hold the volume dirty")
	}
	must(v.SetDurability(DurabilityMounted)) // settles the held span, back on the journal
	must(v.MkDir("/d", 0o755))
	if !v.journaling() || v.held {
		t.Errorf("Mounted after Lazy: journaling %v held %v", v.journaling(), v.held)
	}
}

// When the checks fail the open still succeeds, the volume writes directly exactly
// as before, and JournalWritesReason says why.
func TestJournalWritesFallbackReason(t *testing.T) {
	const size = 0x80000
	t.Run("not journaled", func(t *testing.T) {
		plain, err := Mkfs(8<<20, FormatConfig{Label: "PLAIN"})
		if err != nil {
			t.Fatal(err)
		}
		v, err := OpenWritable(plain, nil)
		if err != nil {
			t.Fatal(err)
		}
		if v.jw != nil || !errors.Is(v.JournalWritesReason(), ErrJournalWritesUnavailable) {
			t.Errorf("jw=%v reason=%v", v.jw, v.JournalWritesReason())
		}
	})
	t.Run("writer without Sync", func(t *testing.T) {
		img, _ := journalImage(t, size, 1000, 0x4400)
		v, err := OpenWritable(img, onlyWriter{w: &memWriter{b: slices.Clone(img)}})
		if err != nil {
			t.Fatal(err)
		}
		if v.jw != nil || !errors.Is(v.JournalWritesReason(), ErrJournalWritesUnavailable) {
			t.Fatalf("jw=%v reason=%v", v.jw, v.JournalWritesReason())
		}
		for _, d := range []Durability{DurabilityStrict, DurabilityMounted} {
			if err := v.SetDurability(d); err != nil {
				t.Fatal(err)
			}
			if err := v.MkDir("/"+d.String(), 0o755); err != nil {
				t.Errorf("direct write at %v after the fallback: %v", d, err)
			}
		}
	})
	t.Run("journal never initialised", func(t *testing.T) {
		img, _, _ := journaledImage(t, kJIJournalInFSMask|kJIJournalNeedInitMask, make([]byte, 512), size)
		v, err := OpenWritable(img, nil)
		if err != nil {
			t.Skipf("the open refuses it already: %v", err)
		}
		if v.jw != nil || !errors.Is(v.JournalWritesReason(), ErrJournalWritesUnavailable) {
			t.Errorf("jw=%v reason=%v", v.jw, v.JournalWritesReason())
		}
	})
	t.Run("read-only", func(t *testing.T) {
		img, _ := journalImage(t, size, 1000, 0x4400)
		if err := mustOpen(t, img, false).JournalWritesReason(); !errors.Is(err, ErrReadOnly) {
			t.Errorf("read-only: %v", err)
		}
	})
	t.Run("writer with Sync is on", func(t *testing.T) {
		img, _ := journalImage(t, size, 1000, 0x4400)
		mem := &syncWriter{memWriter{b: slices.Clone(img)}}
		v, err := OpenWritable(img, mem)
		if err != nil {
			t.Fatal(err)
		}
		if v.JournalWritesReason() != nil {
			t.Fatalf("reason %v on a volume that is on", v.JournalWritesReason())
		}
	})
}

// openDirect is OpenWritable as it was before PR 6: no automatic journal writes.
func openDirect(t testing.TB, img []byte) *Volume {
	t.Helper()
	v, err := Open(bytesReaderAt(img), int64(len(img)))
	if err != nil {
		t.Fatal(err)
	}
	if err := v.enableWrites(bytesReaderAt(img), int64(len(img)), false); err != nil {
		t.Fatal(err)
	}
	v.img = img
	return v
}

// A non-journaled volume is written byte for byte as before, at every level.
func TestNonJournaledVolumeUnchangedByDefault(t *testing.T) {
	plain, err := Mkfs(8<<20, FormatConfig{Label: "PLAIN"})
	if err != nil {
		t.Fatal(err)
	}
	for _, d := range allDurabilities {
		t.Run(d.String(), func(t *testing.T) {
			run := func(v *Volume) []byte {
				if err := v.SetDurability(d); err != nil {
					t.Fatal(err)
				}
				for _, n := range []string{"/a", "/b"} {
					if err := v.MkDir(n, 0o755); err != nil {
						t.Fatal(err)
					}
				}
				if err := v.WriteFile("/f", fill(9000, 3), 0o644); err != nil {
					t.Fatal(err)
				}
				if err := v.Close(); err != nil {
					t.Fatal(err)
				}
				return v.Bytes()
			}
			got, err := OpenWritable(bytes.Clone(plain), nil)
			if err != nil {
				t.Fatal(err)
			}
			if got.jw != nil {
				t.Fatal("journal writes on a non-journaled volume")
			}
			if a, b := run(got), run(openDirect(t, bytes.Clone(plain))); !bytes.Equal(a, b) {
				t.Error("the image differs from the one the pre-PR-6 open produces")
			}
		})
	}
}
