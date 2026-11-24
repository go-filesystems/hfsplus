// Copyright (c) 2026, go-filesystems
// SPDX-License-Identifier: BSD-3-Clause

package hfsplus

import (
	"errors"
	"slices"
	"testing"
)

// A public mutation that commits twice (an overwrite is a delete then a create) is
// never split across two transactions by the group's size bound: after a crash a Batch
// shows the volume before some whole mutation or after it, never a file deleted and
// not yet recreated. The filler makes the second commit not fit beside the first
// group; every case also requires that a flush DID fall inside the Batch (the bound
// was reached), or the case would prove nothing.
func TestGroupBatchNeverSplitsAMutation(t *testing.T) {
	cases := []struct {
		name       string
		setup, ops []jwOp
		both       bool // both write orders (the others run in order only: the oracle is slow)
	}{
		{"WriteFile overwrite", []jwOp{wf("/a", pat(3000, 1))}, []jwOp{wf("/s", pat(7*4096, 2)), wf("/a", pat(3000, 3))}, true},
		{"Truncate", []jwOp{wf("/a", pat(3000, 1))}, []jwOp{wf("/s", pat(7*4096, 2)), func(v *Volume) error { return v.Truncate("/a", 2000) }}, false},
		{"Truncate grow", []jwOp{wf("/a", pat(3000, 1))}, []jwOp{wf("/s", pat(7*4096, 2)), func(v *Volume) error { return v.Truncate("/a", 4000) }}, false},
	}
	for _, tc := range cases {
		for _, reverse := range []bool{false, true} {
			if reverse && !tc.both {
				continue
			}
			name := tc.name
			if reverse {
				name += "/reverse"
			}
			t.Run(name, func(t *testing.T) {
				// First session barriers: 2 for one group. A flush inside the Batch
				// makes at least 3.
				if syncs := batchOracle(t, tc.name, tc.setup, tc.ops, true, reverse); syncs < 3 {
					t.Fatalf("%d barriers: no group was flushed inside the Batch, the case does not reach the bound", syncs)
				}
			})
		}
	}
}

// The scope that keeps a mutation whole must not outlive it.
func TestGroupScopeEnds(t *testing.T) {
	v, _, _ := groupVolume(t)
	if err := v.WriteFile("/a", pat(100, 1), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := v.WriteFile("/a", pat(100, 2), 0o644); err != nil {
		t.Fatal(err)
	}
	if v.scopeBase != nil {
		t.Error("a scope stayed open after WriteFile")
	}
	if err := v.Truncate("/a", 10); err != nil {
		t.Fatal(err)
	}
	if v.scopeBase != nil {
		t.Error("a scope stayed open after Truncate")
	}
	if err := v.SetDurability(DurabilityStrict); err != nil {
		t.Fatal(err)
	}
	end := v.beginScope()
	if v.scopeBase != nil {
		t.Error("a scope opened outside group mode")
	}
	end()
}

// splitGroup keeps what the mutation changed and flushes what was there before.
func TestGroupSplitKeepsTheMutationsSectors(t *testing.T) {
	v, c, _ := groupVolume(t)
	sec := func(b byte) []byte { return pat(512, b) }
	v.grp = &jgroup{sectors: map[int64][]byte{0x30000: sec(1), 0x30200: sec(2)}}
	end := v.beginScope()
	defer end()
	// The mutation rewrites 0x30200, adds 0x30400, and frees a block.
	v.grp.sectors[0x30200] = sec(9)
	v.grp.sectors[0x30400] = sec(4)
	v.grp.freed = []span{{0x38000, 0x38200}}
	w := len(c.writes)
	if err := v.splitGroup(); err != nil {
		t.Fatal(err)
	}
	if len(c.writes) == w {
		t.Fatal("the earlier group was not written")
	}
	if got := len(v.grp.sectors); got != 2 || !slices.Equal(v.grp.sectors[0x30200], sec(9)) || !slices.Equal(v.grp.sectors[0x30400], sec(4)) {
		t.Fatalf("the mutation's sectors were not kept: %d sectors", got)
	}
	if len(v.grp.freed) != 1 {
		t.Errorf("the freed span was lost: %v", v.grp.freed)
	}
	v.grp = nil
}

// splitGroup and flushGroup outside a scope, or with nothing open.
func TestGroupSplitEdges(t *testing.T) {
	v, c, _ := groupVolume(t)
	if err := v.splitGroup(); err != nil {
		t.Fatalf("no group: %v", err)
	}
	// No scope: the whole group is written and gone.
	v.grp = &jgroup{sectors: map[int64][]byte{0x30000: pat(512, 1)}}
	w := len(c.writes)
	if err := v.splitGroup(); err != nil || v.grp != nil || len(c.writes) == w {
		t.Fatalf("no scope: err %v, grp %v, %d writes", err, v.grp, len(c.writes)-w)
	}
	// A scope with no group left (the mutation changed nothing yet).
	v.scopeBase = &jgroup{sectors: map[int64][]byte{}}
	if err := v.splitGroup(); err != nil {
		t.Fatalf("scope, no group: %v", err)
	}
	v.scopeBase = nil
	// A group identical to the scope's base leaves nothing for the next group.
	v.grp = &jgroup{sectors: map[int64][]byte{0x30000: pat(512, 1)}}
	end := v.beginScope()
	w = len(c.writes)
	if err := v.splitGroup(); err != nil || v.grp != nil || len(c.writes) == w {
		t.Fatalf("unchanged group: err %v, grp %v, %d writes", err, v.grp, len(c.writes)-w)
	}
	end()
}

// A mutation whose commits do not fit even an empty group goes alone, and the flush
// of what it had already staged reports its failure.
func TestGroupMutationTooBigForAGroupReportsTheFlush(t *testing.T) {
	v, c, _ := groupVolume(t)
	end := v.beginScope()
	defer end()
	if err := v.MkDir("/a", 0o755); err != nil {
		t.Fatal(err)
	}
	if v.grp == nil {
		t.Fatal("the first commit was not absorbed")
	}
	for i := 0; i <= maxGroupFreed; i++ {
		v.jfreed = append(v.jfreed, span{int64(i) * 1024, int64(i)*1024 + 512})
	}
	c.failAt = len(c.writes)
	err := v.MkDir("/b", 0o755)
	c.failAt = -1
	v.jfreed = nil
	if !errors.Is(err, errInjected) {
		t.Fatalf("MkDir = %v, want the flush failure", err)
	}
}

// Bytes is the current image: the open group is flushed into it first.
func TestGroupBytesIsCurrent(t *testing.T) {
	img, _ := journalImage(t, 0x40000, 100, 0x4400)
	v, err := OpenWritable(img, nil)
	if err != nil {
		t.Fatal(err)
	}
	defer v.Close()
	if v.jw == nil {
		t.Fatal("journal writes are off on an in-memory journaled volume")
	}
	if err := v.SetDurability(DurabilityLazy); err != nil {
		t.Fatal(err)
	}
	if err := v.MkDir("/lazy", 0o755); err != nil {
		t.Fatal(err)
	}
	if v.grp == nil {
		t.Fatal("the commit was not absorbed into a group")
	}
	got := v.Bytes()
	if v.grp != nil {
		t.Error("Bytes left the group open")
	}
	r := mustOpen(t, slices.Clone(got), false)
	names, err := observeFiles(r)
	if err != nil {
		t.Fatal(err)
	}
	if !slices.Contains(names, "lazy/") {
		t.Errorf("Bytes() does not show the Lazy mkdir: %v", names)
	}
}

// Spans more than 2^31 apart sort by value whatever the width of int (a subtraction
// converted to int wraps on 32 bits).
func TestMergeSpansFarApart(t *testing.T) {
	far := int64(3) << 30
	got := mergeSpans([]span{{far, far + 512}}, []span{{0, 512}, {far + 4096, far + 4608}})
	want := []span{{0, 512}, {far, far + 512}, {far + 4096, far + 4608}}
	if !slices.Equal(got, want) {
		t.Fatalf("mergeSpans = %v, want %v", got, want)
	}
}
