// Copyright (c) 2026, go-filesystems
// SPDX-License-Identifier: BSD-3-Clause

package hfsplus

import (
	"bytes"
	"encoding/binary"
	"errors"
	"fmt"
	"slices"
	"testing"
)

// PR 7: DurabilityLazy and Batch on a journaled volume are an in-memory group commit.
// What these tests pin down (the design is in journal_group.go):
//
//   - a Lazy commit writes nothing and takes no barrier;
//   - Sync, Close, a level switch away from Lazy and the size bound each flush ONE
//     journal transaction (one barrier, two for the session's first);
//   - the volume is never held dirty while it journals;
//   - a Batch that fits one group is atomic on the medium, data included; one that
//     does not is a sequence of whole mutations (a crash leaves a prefix);
//   - the group stays within the data bound of the journal, in memory too.

// neverHeld is the invariant of TestMountedFallbackNeverHeldWhileJournaling, for the
// group commit: journal writes on, the volume not held dirty, the journaled bit set on
// the medium.
func neverHeld(t *testing.T, v *Volume, path, step string) {
	t.Helper()
	if v.jw == nil {
		t.Fatalf("%s: journal writes went off", step)
	}
	if v.held {
		t.Errorf("%s: the volume is held dirty while it journals", step)
	}
	raw := mustRead(t, path)
	if a := binary.BigEndian.Uint32(raw[volumeHeaderOffset+4:]); a&kHFSVolumeJournaledBit == 0 {
		t.Errorf("%s: journaled bit cleared on the medium (attributes %#x)", step, a)
	}
}

// replayedNames is what a mount of the medium as it is now (journal replayed) lists.
func replayedNames(t *testing.T, path string) []string {
	t.Helper()
	names, err := rootNames(mustOpen(t, mustRead(t, path), true))
	if err != nil {
		t.Fatal(err)
	}
	return names
}

func TestGroupLazyCommitsWriteNothing(t *testing.T) {
	img, _ := journalImage(t, 0x40000, 100, 0x4400)
	v, c, path := jwFile(t, img, true)
	defer v.Close()
	if err := v.SetDurability(DurabilityLazy); err != nil {
		t.Fatal(err)
	}
	before := mustRead(t, path)
	for _, op := range mkdirOps(5) {
		if err := op(v); err != nil {
			t.Fatal(err)
		}
		neverHeld(t, v, path, "lazy commit")
	}
	if err := wf("/f", pat(3000, 4))(v); err != nil {
		t.Fatal(err)
	}
	if len(c.events) != 0 || c.syncs != 0 {
		t.Errorf("Lazy commits wrote %d events and took %d barriers, want none", len(c.events), c.syncs)
	}
	if !bytes.Equal(before, mustRead(t, path)) {
		t.Error("the medium changed under Lazy commits")
	}
	if got := replayedNames(t, path); len(got) != 0 {
		t.Errorf("the medium shows %v before any Sync", got)
	}
}

// One transaction per flush: two barriers for the session's first, one after, none
// when nothing is pending.
func TestGroupBarrierCounts(t *testing.T) {
	img, _ := journalImage(t, 0x40000, 100, 0x4400)
	v, c, path := jwFile(t, img, true)
	defer v.Close()
	if err := v.SetDurability(DurabilityLazy); err != nil {
		t.Fatal(err)
	}
	step := func(name string, want int, do func() error) {
		t.Helper()
		before := c.syncs
		if err := do(); err != nil {
			t.Fatal(err)
		}
		if got := c.syncs - before; got != want {
			t.Errorf("%s: %d barriers, want %d", name, got, want)
		}
		neverHeld(t, v, path, name)
	}
	step("first session flush of 4 commits", 2, func() error {
		for _, op := range mkdirOps(4) {
			if err := op(v); err != nil {
				return err
			}
		}
		return v.Sync()
	})
	step("second flush of 4 commits", 1, func() error {
		for _, op := range []jwOp{wf("/a", pat(2000, 1)), wf("/b", pat(100, 2)), rm("/a"), func(v *Volume) error { return v.MkDir("/g", 0o755) }} {
			if err := op(v); err != nil {
				return err
			}
		}
		return v.Sync()
	})
	// The flush's in-place writes are made durable by the next barrier (jw.pending,
	// as for Strict): the first Sync after a flush pays it, the next pays nothing.
	step("Sync after a flush (its in-place writes)", 1, v.Sync)
	step("Sync with nothing pending", 0, v.Sync)
	step("Batch", 1, func() error { return v.Batch(func() error { return v.MkDir("/e", 0o755) }) })
	step("Sync after the Batch", 1, v.Sync)
	step("Batch of nothing", 0, func() error { return v.Batch(func() error { return nil }) })
	names := replayedNames(t, path)
	if want := []string{"b", "d000", "d001", "d002", "d003", "e", "g"}; !slices.Equal(names, want) {
		t.Errorf("the medium shows %v, want %v", names, want)
	}
	// The first flush of a fresh session also pays two.
	v2, c2, _ := jwFile(t, img, true)
	defer v2.Close()
	before := c2.syncs
	if err := v2.Batch(func() error { return mkdirOps(3)[2](v2) }); err != nil {
		t.Fatal(err)
	}
	if got := c2.syncs - before; got != 2 {
		t.Errorf("first Batch of a session: %d barriers, want 2", got)
	}
}

// batchOracle runs fn inside one Batch on a journaled volume that already went through
// setup, replays every crash image of the Batch and requires each to be the volume
// before the Batch, after some whole mutation of it, or after all of it, and the end
// of the Batch to show all of it. With perOp false only the two ends are allowed:
// atomicity. Data is checked byte for byte (observeFiles).
func batchOracle(t *testing.T, name string, setup, ops []jwOp, perOp, reverse bool) (syncs int) {
	t.Helper()
	if testing.Short() {
		t.Skip("crash oracle: native runs only")
	}
	img, jbase := journalImage(t, 0x40000, 100, 0x4400)
	if len(setup) > 0 {
		v, _, path := jwFile(t, img, true)
		for _, op := range setup {
			if err := op(v); err != nil {
				t.Fatal(err)
			}
		}
		if err := v.Close(); err != nil {
			t.Fatal(err)
		}
		img = mustRead(t, path)
	}
	v, c, _ := jwFile(t, img, true)
	defer v.Close()
	states := [][]string{}
	snap := func() {
		t.Helper()
		names, err := observeFiles(v)
		if err != nil {
			t.Fatal(err)
		}
		states = append(states, names)
	}
	snap()
	err := v.Batch(func() error {
		for i, op := range ops {
			if err := op(v); err != nil {
				return err
			}
			if perOp && i < len(ops)-1 {
				snap()
			}
		}
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	snap()
	last := len(states) - 1
	if !perOp && last != 1 {
		t.Fatalf("%d states", len(states))
	}
	ev, starts, ends := splitSectors(c.events, []int{0}, []int{len(c.events)})
	base := jwBaseline(t, img)
	n, bad := 0, []string{}
	jwCrashImagesOrder(img, ev, starts, ends, nil, reverse, func(label string, im []byte, lo, hi int) {
		n++
		// Before the end of the Batch any state is allowed; at its end, the last.
		if lo > 0 {
			lo = last
		}
		hi = last
		if msg := jwCheckImage(im, lo, hi, states, base, jbase, false, observeFiles); msg != "" {
			bad = append(bad, label+": "+msg)
		}
	})
	// The last crash point is the end of the Batch: all of it.
	end := mustOpenState(t, ev, img)
	if got, err := observeFiles(end); err != nil || !slices.Equal(got, states[last]) {
		t.Errorf("%s: after the Batch the medium shows %v (%v), want %v", name, got, err, states[last])
	}
	t.Logf("%s: %d mutations, %d states, %d barriers, %d events, %d crash images", name, len(ops), len(states), c.syncs, len(ev), n)
	if len(bad) > 0 {
		for _, b := range bad[:min(len(bad), 8)] {
			t.Errorf("%s", b)
		}
		t.Fatalf("%d of %d crash images are wrong", len(bad), n)
	}
	return c.syncs
}

// mustOpenState applies every event of the log to img (all durable) and opens it.
func mustOpenState(t *testing.T, ev []crashEvent, img []byte) *Volume {
	t.Helper()
	im := bytes.Clone(img)
	for _, e := range ev {
		if !e.sync {
			copy(im[e.off:], e.data)
		}
	}
	return mustOpen(t, im, true)
}

// A Batch that fits one group is atomic: every crash image shows the volume before it
// or after all of it, never part, and the files' bytes are right.
func TestGroupBatchAtomicCrashOracle(t *testing.T) {
	cases := []struct {
		name       string
		setup, ops []jwOp
	}{
		{"metadata", nil, mkdirOps(6)},
		{"small-files", nil, []jwOp{wf("/f1", pat(1, 1)), wf("/f2", pat(3000, 2)), mkdirOps(1)[0], wf("/f3", pat(5000, 3)), rm("/f1")}},
		{"large-data", nil, []jwOp{wf("/big", pat(60000, 4)), wf("/s", pat(100, 5)), mkdirOps(1)[0]}},
		// The stale-bytes case: /a is deleted and its blocks are reused by /b in the same
		// Batch. After a crash /b is never seen holding a's bytes and /a is never seen
		// holding b's.
		{"delete-then-reuse", []jwOp{wf("/a", pat(60000, 7))}, []jwOp{rm("/a"), wf("/b", pat(60000, 8))}},
		{"overwrite", []jwOp{wf("/a", pat(20000, 7))}, []jwOp{wf("/a", pat(20000, 9)), wf("/b", pat(300, 1))}},
	}
	for _, tc := range cases {
		for _, reverse := range []bool{false, true} {
			t.Run(fmt.Sprintf("%s/reverse=%v", tc.name, reverse), func(t *testing.T) {
				batchOracle(t, tc.name, tc.setup, tc.ops, false, reverse)
			})
		}
	}
}

// A Batch larger than one group is a sequence of whole-mutation groups: a crash leaves
// some prefix of the mutations, each whole, and the Batch still pays one barrier per
// group flushed, not one per mutation.
func TestGroupBatchLargerThanOneGroupLeavesAPrefix(t *testing.T) {
	var ops []jwOp
	for i := 0; i < 5; i++ {
		ops = append(ops, wf(fmt.Sprintf("/f%d", i), pat(20000, byte(i+1))), mkdirOps(i + 1)[i])
	}
	syncs := batchOracle(t, "prefix", nil, ops, true, false)
	if syncs <= 2 {
		t.Errorf("%d barriers: the Batch never exceeded one group, the test covers nothing", syncs)
	}
	if syncs >= len(ops) {
		t.Errorf("%d barriers for %d mutations: no grouping", syncs, len(ops))
	}
}

// The size bound through the public API: however many commits are made, the group
// held in memory stays within the journal's data bound, a flush happens on its own, and
// everything is on the medium after the Sync.
func TestGroupSizeBound(t *testing.T) {
	img, _ := journalImage(t, 0x40000, 100, 0x4400)
	v, c, path := jwFile(t, img, true)
	defer v.Close()
	if err := v.SetDurability(DurabilityLazy); err != nil {
		t.Fatal(err)
	}
	const n = 60
	bound := int(v.jw.dataBound())
	flushedMidway := false
	for i := 0; i < n; i++ {
		if err := v.WriteFile(fmt.Sprintf("/f%03d", i), pat(3000, byte(i)), 0o644); err != nil {
			t.Fatal(err)
		}
		if v.grp != nil && len(v.grp.sectors)*512 > bound {
			t.Fatalf("after commit %d the group holds %d bytes, over the journal's data bound %d", i, len(v.grp.sectors)*512, bound)
		}
		if c.syncs > 0 {
			flushedMidway = true
		}
		if i%25 == 0 {
			neverHeld(t, v, path, fmt.Sprint("commit ", i))
		}
	}
	if !flushedMidway {
		t.Error("the commits never overflowed the group: the bound was not exercised")
	}
	if err := v.Sync(); err != nil {
		t.Fatal(err)
	}
	w := mustOpen(t, mustRead(t, path), true)
	for i := 0; i < n; i++ {
		name := fmt.Sprintf("/f%03d", i)
		if b, err := w.ReadFile(name); err != nil || !bytes.Equal(b, pat(3000, byte(i))) {
			t.Fatalf("%s after Sync: %d bytes, %v", name, len(b), err)
		}
	}
}

// Level switches in every direction: the commits made at the level left are on the
// medium when the switch returns (a Lazy group is flushed), nothing is lost, and the
// volume is never held dirty.
func TestGroupLevelSwitches(t *testing.T) {
	levels := []Durability{DurabilityStrict, DurabilityMounted, DurabilityLazy}
	for _, from := range levels {
		for _, to := range levels {
			if from == to {
				continue
			}
			t.Run(from.String()+"->"+to.String(), func(t *testing.T) {
				img, _ := journalImage(t, 0x40000, 100, 0x4400)
				v, _, path := jwFile(t, img, true)
				defer v.Close()
				must := func(err error) {
					t.Helper()
					if err != nil {
						t.Fatal(err)
					}
				}
				must(v.SetDurability(from))
				neverHeld(t, v, path, "set from")
				must(v.MkDir("/a", 0o755))
				must(v.WriteFile("/f", pat(2500, 3), 0o644))
				neverHeld(t, v, path, "commits at the level left")
				must(v.SetDurability(to))
				neverHeld(t, v, path, "switch")
				if v.grp != nil {
					t.Error("a group is still pending after the switch away from Lazy")
				}
				if from == DurabilityLazy && !slices.Equal(replayedNames(t, path), []string{"a", "f"}) {
					t.Errorf("the switch left the group off the medium: %v", replayedNames(t, path))
				}
				must(v.MkDir("/b", 0o755))
				neverHeld(t, v, path, "commit at the level entered")
				must(v.Sync())
				neverHeld(t, v, path, "Sync")
				if got := replayedNames(t, path); !slices.Equal(got, []string{"a", "b", "f"}) {
					t.Errorf("the medium shows %v", got)
				}
				got, err := mustOpen(t, mustRead(t, path), true).ReadFile("/f")
				if err != nil || !bytes.Equal(got, pat(2500, 3)) {
					t.Errorf("/f reads %d bytes, %v", len(got), err)
				}
			})
		}
	}
}

// A commit the journal cannot take (a forged sector) at Lazy: the group is flushed,
// then the commit takes the bracketed direct path, and the volume is never held.
func TestGroupLazyFallbackNeverHeld(t *testing.T) {
	img, _ := journalImage(t, 0x40000, 100, 0x4400)
	v, _, path := jwFile(t, img, true)
	defer v.Close()
	if err := v.SetDurability(DurabilityLazy); err != nil {
		t.Fatal(err)
	}
	neverHeld(t, v, path, "open")
	if err := v.MkDir("/a", 0o755); err != nil {
		t.Fatal(err)
	}
	neverHeld(t, v, path, "group commit")
	if err := plantForgedMkDir(v, "/f", binary.LittleEndian); err != nil {
		t.Fatal(err)
	}
	neverHeld(t, v, path, "fallback commit")
	if v.grp != nil {
		t.Error("the fallback commit left a group pending")
	}
	if err := v.MkDir("/b", 0o755); err != nil {
		t.Fatal(err)
	}
	neverHeld(t, v, path, "commit after the fallback")
	if err := v.SetDurability(DurabilityStrict); err != nil {
		t.Fatal(err)
	}
	neverHeld(t, v, path, "SetDurability(Strict)")
	if got := replayedNames(t, path); !slices.Equal(got, []string{"a", "b", "f"}) {
		t.Errorf("the medium shows %v", got)
	}
}

// Close flushes the group: the image is clean (nothing to replay) and complete.
func TestGroupCloseFlushes(t *testing.T) {
	img, _ := journalImage(t, 0x40000, 100, 0x4400)
	v, _, path := jwFile(t, img, true)
	if err := v.SetDurability(DurabilityLazy); err != nil {
		t.Fatal(err)
	}
	for _, op := range append(mkdirOps(3), wf("/f", pat(4000, 2))) {
		if err := op(v); err != nil {
			t.Fatal(err)
		}
	}
	if err := v.Close(); err != nil {
		t.Fatal(err)
	}
	w := mustOpen(t, mustRead(t, path), false)
	got, err := rootNames(w)
	if err != nil || !slices.Equal(got, []string{"d000", "d001", "d002", "f"}) {
		t.Errorf("the closed image shows %v, %v", got, err)
	}
	if b, err := w.ReadFile("/f"); err != nil || !bytes.Equal(b, pat(4000, 2)) {
		t.Errorf("/f reads %d bytes, %v", len(b), err)
	}
}

// Reads inside a group see the group's writes, before they are on the medium.
func TestGroupReadYourWrites(t *testing.T) {
	img, _ := journalImage(t, 0x40000, 100, 0x4400)
	v, _, path := jwFile(t, img, true)
	defer v.Close()
	if err := v.Batch(func() error {
		if err := v.MkDir("/d", 0o755); err != nil {
			return err
		}
		if err := v.WriteFile("/d/f", pat(9000, 6), 0o644); err != nil {
			return err
		}
		b, err := v.ReadFile("/d/f")
		if err != nil || !bytes.Equal(b, pat(9000, 6)) {
			return fmt.Errorf("read back %d bytes, %v", len(b), err)
		}
		if got := replayedNames(t, path); len(got) != 0 {
			return fmt.Errorf("the medium shows %v inside the Batch", got)
		}
		return v.Rename("/d/f", "/d/g")
	}); err != nil {
		t.Fatal(err)
	}
	b, err := mustOpen(t, mustRead(t, path), true).ReadFile("/d/g")
	if err != nil || !bytes.Equal(b, pat(9000, 6)) {
		t.Errorf("after the Batch /d/g reads %d bytes, %v", len(b), err)
	}
}

// fn's error: the mutations made stay and are committed by the Batch's Sync, the
// error is returned, the level is restored.
func TestGroupBatchFnError(t *testing.T) {
	img, _ := journalImage(t, 0x40000, 100, 0x4400)
	v, _, path := jwFile(t, img, true)
	defer v.Close()
	boom := errors.New("boom")
	err := v.Batch(func() error {
		if err := v.MkDir("/kept", 0o755); err != nil {
			return err
		}
		return boom
	})
	if !errors.Is(err, boom) {
		t.Fatalf("Batch returned %v", err)
	}
	if v.Durability() != DurabilityStrict || v.grp != nil {
		t.Errorf("level %v, group pending %v", v.Durability(), v.grp != nil)
	}
	neverHeld(t, v, path, "after a failed Batch")
	if got := replayedNames(t, path); !slices.Equal(got, []string{"kept"}) {
		t.Errorf("the medium shows %v: the mutations made before the error must stay", got)
	}
}
