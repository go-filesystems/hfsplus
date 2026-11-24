// Copyright (c) 2026, go-filesystems
// SPDX-License-Identifier: BSD-3-Clause

package hfsplus

import (
	"bytes"
	"encoding/binary"
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
	img, _ := journalImage(t, 0x80000, 1000, 0x4400)
	v, c, _ := jwFile(t, img, false)
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
	before = c.syncs
	must(v.SetDurability(DurabilityLazy))
	if !v.journaling() || c.syncs != before {
		t.Errorf("Strict -> Lazy: %d barriers, journaling %v; want none, still live", c.syncs-before, v.journaling())
	}
	must(v.MkDir("/c", 0o755)) // Lazy: accumulated in memory, never held dirty
	if v.held || v.grp == nil || c.syncs != before {
		t.Errorf("a Lazy commit on a journaled volume: held %v, group %v, %d barriers", v.held, v.grp != nil, c.syncs-before)
	}
	must(v.SetDurability(DurabilityMounted)) // flushes the group: one transaction
	if v.grp != nil || c.syncs != before+1 {
		t.Errorf("leaving Lazy: group %v, %d barriers, want none pending and 1", v.grp != nil, c.syncs-before)
	}
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
		mem := &memWriter{b: slices.Clone(img)}
		v, err := OpenWritable(img, onlyWriter{w: mem})
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
		if err := v.Close(); err != nil {
			t.Fatal(err)
		}
		// Written in place, as v0.17.0 did (the bytes are pinned by
		// TestWritesMatchV017Golden): the volume header keeps the journaled and
		// unmounted bits it had.
		attr := func(b []byte) uint32 { return binary.BigEndian.Uint32(b[volumeHeaderOffset+4:]) }
		if a, w := attr(mem.b), attr(img); a != w {
			t.Errorf("volume attributes %#x after the writes, %#x before", a, w)
		}
		if names, _ := rootNames(mustOpen(t, mem.b, false)); !slices.Equal(names, []string{"mounted", "strict"}) {
			t.Errorf("directories %v", names)
		}
	})
	t.Run("journal never initialised", func(t *testing.T) {
		img, _, _ := journaledImage(t, kJIJournalInFSMask|kJIJournalNeedInitMask, make([]byte, 512), size)
		v, err := OpenWritable(img, nil)
		if err != nil {
			t.Fatalf("the open refuses the fixture: %v", err)
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
	t.Run("closed", func(t *testing.T) {
		img, _ := journalImage(t, size, 1000, 0x4400)
		v, _, _ := jwFile(t, img, false)
		if v.JournalWritesReason() != nil {
			t.Fatalf("reason %v before Close", v.JournalWritesReason())
		}
		if err := v.Close(); err != nil {
			t.Fatal(err)
		}
		if err := v.JournalWritesReason(); !errors.Is(err, ErrClosed) {
			t.Errorf("after Close: %v, want ErrClosed", err)
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

// A commit that falls back at Mounted (here a forged sector, which the journal
// refuses) used to leave the volume held dirty while the journal stayed on: the next
// commit journaled inside the dirty span and settle wrote the clean mark over a live
// journal. The invariant, checked after every step: with journal writes on, the
// volume is never held, and the journaled bit on the medium is never cleared outside
// a commit.
func TestMountedFallbackNeverHeldWhileJournaling(t *testing.T) {
	img, _ := journalImage(t, 0x40000, 100, 0x4400)
	v, _, path := jwFile(t, img, false)
	defer v.Close()
	if err := v.SetDurability(DurabilityMounted); err != nil {
		t.Fatal(err)
	}
	check := func(step string) []byte {
		t.Helper()
		raw := mustRead(t, path)
		if v.jw == nil {
			t.Fatalf("%s: journal writes went off", step)
		}
		if v.held {
			t.Errorf("%s: the volume is held dirty while it journals", step)
		}
		if a := binary.BigEndian.Uint32(raw[volumeHeaderOffset+4:]); a&kHFSVolumeJournaledBit == 0 {
			t.Errorf("%s: journaled bit cleared on the medium (attributes %#x)", step, a)
		}
		return raw
	}
	check("open")
	if err := v.MkDir("/a", 0o755); err != nil {
		t.Fatal(err)
	}
	check("journal commit")
	if err := plantForgedMkDir(v, "/f", binary.LittleEndian); err != nil {
		t.Fatal(err)
	}
	check("fallback commit")
	if err := v.MkDir("/b", 0o755); err != nil {
		t.Fatal(err)
	}
	check("commit after the fallback")
	if err := v.SetDurability(DurabilityStrict); err != nil {
		t.Fatal(err)
	}
	raw := check("SetDurability(Strict)")
	names, err := rootNames(mustOpen(t, raw, true))
	if err != nil || !slices.Equal(names, []string{"a", "b", "f"}) {
		t.Errorf("the image shows %v, %v", names, err)
	}
}
