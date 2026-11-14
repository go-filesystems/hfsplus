// Copyright (c) 2026, go-filesystems
// SPDX-License-Identifier: BSD-3-Clause

package hfsplus

import (
	"bytes"
	"errors"
	"os"
	"path/filepath"
	"testing"
)

var allDurabilities = []Durability{DurabilityStrict, DurabilityMounted, DurabilityLazy}

// durOp is a crashOp run at one durability level.
type durOp struct {
	crashOp
	dur Durability
}

// crashOpsPerDurability is crashOps at every level, named after it.
func crashOpsPerDurability() []durOp {
	var out []durOp
	for _, d := range allDurabilities {
		for _, op := range crashOps {
			out = append(out, durOp{crashOp{d.String() + "-" + op.name, op.run}, d})
		}
	}
	return out
}

// TestDurabilityCrashModel replays every crash point of every level under the two
// crash models of TestInPlaceCrashModel: a volume still marked cleanly unmounted
// is consistent. For the levels that make each commit durable (Strict, Mounted) it
// also requires that, when the mutator has returned, the writes an fsync made
// durable already hold its result.
func TestDurabilityCrashModel(t *testing.T) {
	p := sparseImage(t, 8<<20)
	orig, err := os.ReadFile(p)
	if err != nil {
		t.Fatal(err)
	}
	base := baseline(t, p)
	for _, op := range crashOpsPerDurability() {
		t.Run(op.name, func(t *testing.T) {
			if err := os.WriteFile(p, orig, 0o644); err != nil {
				t.Fatal(err)
			}
			v, err := OpenFileWritable(p)
			if err != nil {
				t.Fatal(err)
			}
			if err := v.SetDurability(op.dur); err != nil {
				t.Fatal(err)
			}
			c := instrument(v)
			if err := op.run(v); err != nil {
				t.Fatal(err)
			}
			returned := len(c.events)
			if err := v.Close(); err != nil {
				t.Fatal(err)
			}
			events := c.events
			crash := filepath.Join(t.TempDir(), "crash.img")
			image := func(upto int) []byte {
				img := bytes.Clone(orig)
				for _, e := range events[:upto] {
					if !e.sync {
						copy(img[e.off:], e.data)
					}
				}
				return img
			}
			durable := func(k int) int { // events kept by the fsynced-only model
				upto := 0
				for i := 0; i < k; i++ {
					if events[i].sync {
						upto = i + 1
					}
				}
				return upto
			}
			for _, model := range []string{"fsynced-only", "in-order"} {
				clean, dirty := 0, 0
				for k := 0; k <= len(events); k++ {
					upto := k
					if model == "fsynced-only" {
						upto = durable(k)
					}
					if err := os.WriteFile(crash, image(upto), 0o644); err != nil {
						t.Fatal(err)
					}
					if attrsOf(t, crash)&kHFSVolumeUnmountedBit == 0 {
						dirty++
						continue
					}
					clean++
					if msg := base.check(crash); msg != "" {
						t.Errorf("%s, crash after event %d of %d: marked cleanly unmounted but %s", model, k, len(events), msg)
					}
				}
				if clean < 2 || dirty < 1 {
					t.Errorf("%s: %d crash points marked clean, %d marked dirty", model, clean, dirty)
				}
			}
			// Nothing orders the writes between two fsyncs on the disk: for each such
			// group, the durable prefix plus any ONE write of it, and plus all but
			// one, must be dirty or consistent.
			reordered := 0
			for s := 0; s <= len(events); {
				e := s
				for e < len(events) && !events[e].sync {
					e++
				}
				for i := s; i < e; i++ {
					for _, only := range []bool{true, false} {
						img := image(s)
						for j := s; j < e; j++ {
							if (j == i) == only {
								copy(img[events[j].off:], events[j].data)
							}
						}
						if err := os.WriteFile(crash, img, 0o644); err != nil {
							t.Fatal(err)
						}
						reordered++
						if attrsOf(t, crash)&kHFSVolumeUnmountedBit == 0 {
							continue
						}
						if msg := base.check(crash); msg != "" {
							t.Errorf("group [%d,%d), write %d (only=%v): marked cleanly unmounted but %s", s, e, i, only, msg)
						}
					}
				}
				s = e + 1
			}
			if reordered == 0 {
				t.Error("no reordered image")
			}
			if op.dur == DurabilityLazy {
				return
			}
			// Durable when the mutator returned: the fsynced writes alone hold it.
			if err := os.WriteFile(crash, image(durable(returned)), 0o644); err != nil {
				t.Fatal(err)
			}
			r, err := OpenFile(crash)
			if err != nil {
				t.Fatal(err)
			}
			defer r.Close()
			if f, d, _, err := walkVolume(r); err != nil || f+d != 1 {
				t.Errorf("not durable at return: %d files, %d folders, %v", f, d, err)
			}
		})
	}
}

// TestDurabilityBarriers: the barrier counts the README states. A 1-byte WriteFile
// then a Chtimes, on a volume that was clean when opened.
func TestDurabilityBarriers(t *testing.T) {
	for _, tc := range []struct {
		d     Durability
		first int // fsyncs of the first mutation
		next  int // of the second (no file data)
		data  int // of a third that writes file data
		sync  int // of the Sync that follows
	}{
		{DurabilityStrict, 3, 3, 3, 1},
		{DurabilityMounted, 2, 1, 2, 1},
		{DurabilityLazy, 1, 0, 0, 2},
	} {
		t.Run(tc.d.String(), func(t *testing.T) {
			p := sparseImage(t, 8<<20)
			v, err := OpenFileWritable(p)
			if err != nil {
				t.Fatal(err)
			}
			defer v.Close()
			if err := v.SetDurability(tc.d); err != nil {
				t.Fatal(err)
			}
			c := instrument(v)
			if err := v.WriteFile("/a.txt", []byte{'x'}, 0o644); err != nil {
				t.Fatal(err)
			}
			if c.syncs != tc.first {
				t.Errorf("first mutation: %d fsyncs, want %d", c.syncs, tc.first)
			}
			at := c.syncs
			if err := v.Chtimes("/a.txt", timeNow(), timeNow()); err != nil {
				t.Fatal(err)
			}
			if got := c.syncs - at; got != tc.next {
				t.Errorf("second mutation: %d fsyncs, want %d", got, tc.next)
			}
			at = c.syncs
			// A mutation that writes file data: Mounted orders it before the metadata.
			if err := v.WriteFile("/c.txt", []byte{'z'}, 0o644); err != nil {
				t.Fatal(err)
			}
			if got := c.syncs - at; got != tc.data {
				t.Errorf("third mutation (with data): %d fsyncs, want %d", got, tc.data)
			}
			at = c.syncs
			if err := v.Sync(); err != nil {
				t.Fatal(err)
			}
			if got := c.syncs - at; got != tc.sync {
				t.Errorf("Sync: %d fsyncs, want %d", got, tc.sync)
			}
			// After Sync the volume is marked cleanly unmounted again, at every level.
			if attrsOf(t, p)&kHFSVolumeUnmountedBit == 0 {
				t.Error("Sync left the volume marked as not cleanly unmounted")
			}
			if tc.d != DurabilityStrict {
				if err := v.WriteFile("/b.txt", []byte{'y'}, 0o644); err != nil {
					t.Fatal(err)
				}
				if attrsOf(t, p)&kHFSVolumeUnmountedBit != 0 {
					t.Error("a held volume is marked cleanly unmounted between Syncs")
				}
			}
		})
	}
}

// TestDurabilityCloseSettles: Close restores the clean mark and makes lazy writes
// visible to a fresh open.
func TestDurabilityCloseSettles(t *testing.T) {
	for _, d := range []Durability{DurabilityMounted, DurabilityLazy} {
		p := sparseImage(t, 8<<20)
		v, err := OpenFileWritable(p)
		if err != nil {
			t.Fatal(err)
		}
		if err := v.SetDurability(d); err != nil {
			t.Fatal(err)
		}
		if err := v.WriteFile("/a.txt", []byte("hello"), 0o644); err != nil {
			t.Fatal(err)
		}
		if err := v.Close(); err != nil {
			t.Fatal(err)
		}
		if attrsOf(t, p)&kHFSVolumeUnmountedBit == 0 {
			t.Errorf("%v: Close left the volume dirty", d)
		}
		if msg := baseline2(t, p); msg != "" {
			t.Errorf("%v: %s", d, msg)
		}
	}
}

// baseline2 checks a closed image the way the crash tests do.
func baseline2(t *testing.T, p string) string {
	t.Helper()
	r, err := OpenFile(p)
	if err != nil {
		return err.Error()
	}
	defer r.Close()
	b, err := r.ReadFile("/a.txt")
	if err != nil || string(b) != "hello" {
		return "ReadFile: " + string(b)
	}
	return ""
}

// TestBatch: a group of mutations pays one set of barriers, and the level is
// restored.
func TestBatch(t *testing.T) {
	p := sparseImage(t, 8<<20)
	v, err := OpenFileWritable(p)
	if err != nil {
		t.Fatal(err)
	}
	defer v.Close()
	c := instrument(v)
	err = v.Batch(func() error {
		for _, n := range []string{"/a", "/b", "/c", "/d"} {
			if err := v.MkDir(n, 0o755); err != nil {
				return err
			}
		}
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	if v.Durability() != DurabilityStrict {
		t.Errorf("level %v after Batch", v.Durability())
	}
	if c.syncs != 3 {
		t.Errorf("4 mutations in a Batch: %d fsyncs, want 3 (dirty mark; sectors; clean mark)", c.syncs)
	}
	if attrsOf(t, p)&kHFSVolumeUnmountedBit == 0 {
		t.Error("Batch left the volume marked dirty")
	}
	boom := errors.New("boom")
	if err := v.Batch(func() error { return boom }); !errors.Is(err, boom) {
		t.Errorf("Batch error = %v", err)
	}
	if err := v.SetDurability(Durability(9)); err == nil {
		t.Error("an unknown level was accepted")
	}
}

// TestDurabilityFailureStaysDirty: after a failed write or fsync in a held level
// the volume refuses writes and Close does not mark it clean.
func TestDurabilityFailureStaysDirty(t *testing.T) {
	for _, d := range []Durability{DurabilityMounted, DurabilityLazy} {
		for failAt := 0; failAt < 24; failAt++ {
			p := sparseImage(t, 8<<20)
			v, err := OpenFileWritable(p)
			if err != nil {
				t.Fatal(err)
			}
			if err := v.SetDurability(d); err != nil {
				t.Fatal(err)
			}
			c := instrument(v)
			c.failAt = failAt
			err = v.WriteFile("/a.txt", []byte("hello"), 0o644)
			if err == nil {
				v.Close()
				continue
			}
			c.failAt = -1
			if err := v.MkDir("/z", 0o755); !errors.Is(err, ErrVolumeFailed) && v.failed != nil {
				t.Errorf("%v failAt %d: next mutation = %v, want ErrVolumeFailed", d, failAt, err)
			}
			held := v.held
			v.Close()
			if v.failed != nil && held && attrsOf(t, p)&kHFSVolumeUnmountedBit != 0 {
				t.Errorf("%v failAt %d: Close marked a failed volume cleanly unmounted", d, failAt)
			}
		}
	}
}

// TestDurabilityNoStaleData: a later commit writes a file into the blocks a
// deleted file held. Under Strict and Mounted no crash image (fsynced-only,
// in-order, and every write of every fsync group landing alone or missing alone)
// may show /b with a byte it was never written with. Lazy is data-writeback: the
// stale bytes are allowed, and must be observed (the test is not vacuous).
func TestDurabilityNoStaleData(t *testing.T) {
	for _, d := range allDurabilities {
		t.Run(d.String(), func(t *testing.T) {
			p := sparseImage(t, 8<<20)
			v, err := OpenFileWritable(p)
			if err != nil {
				t.Fatal(err)
			}
			if err := v.SetDurability(d); err != nil {
				t.Fatal(err)
			}
			if err := v.WriteFile("/a", bytes.Repeat([]byte{0xAA}, 9000), 0o644); err != nil {
				t.Fatal(err)
			}
			if err := v.DeleteFile("/a"); err != nil {
				t.Fatal(err)
			}
			orig, err := os.ReadFile(p)
			if err != nil {
				t.Fatal(err)
			}
			c := instrument(v)
			if err := v.WriteFile("/b", bytes.Repeat([]byte{0xBB}, 9000), 0o644); err != nil {
				t.Fatal(err)
			}
			v.Close()
			events := c.events
			crash := filepath.Join(t.TempDir(), "crash.img")
			stale, images := 0, 0
			look := func(img []byte, what string) {
				images++
				if err := os.WriteFile(crash, img, 0o644); err != nil {
					t.Fatal(err)
				}
				r, err := OpenFile(crash)
				if err != nil {
					return
				}
				defer r.Close()
				b, err := r.ReadFile("/b")
				if err != nil {
					return
				}
				if bytes.Contains(b, []byte{0xAA}) {
					stale++
					if d != DurabilityLazy {
						t.Errorf("%s: /b holds bytes of the deleted file", what)
					}
				}
			}
			apply := func(upto int, skip, only int) []byte {
				img := bytes.Clone(orig)
				for j, e := range events[:upto] {
					if e.sync || (skip >= 0 && j == skip) || (only >= 0 && j != only) {
						continue
					}
					copy(img[e.off:], e.data)
				}
				return img
			}
			for k := 0; k <= len(events); k++ {
				look(apply(k, -1, -1), "in-order")
				dur := 0
				for i := 0; i < k; i++ {
					if events[i].sync {
						dur = i + 1
					}
				}
				look(apply(dur, -1, -1), "fsynced-only")
			}
			for s := 0; s <= len(events); {
				e := s
				for e < len(events) && !events[e].sync {
					e++
				}
				base := apply(s, -1, -1)
				for i := s; i < e; i++ {
					only := bytes.Clone(base)
					copy(only[events[i].off:], events[i].data)
					look(only, "group write alone")
					rest := bytes.Clone(base)
					for j := s; j < e; j++ {
						if j != i {
							copy(rest[events[j].off:], events[j].data)
						}
					}
					look(rest, "group write missing")
				}
				s = e + 1
			}
			if images == 0 {
				t.Fatal("no image examined")
			}
			if d == DurabilityLazy && stale == 0 {
				t.Error("lazy: the documented stale-data exposure was never observed, so the test proves nothing")
			}
			t.Logf("%v: %d images, %d with stale bytes", d, images, stale)
		})
	}
}
