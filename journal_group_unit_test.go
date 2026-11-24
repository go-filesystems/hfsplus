// Copyright (c) 2026, go-filesystems
// SPDX-License-Identifier: BSD-3-Clause

package hfsplus

import (
	"errors"
	"fmt"
	"slices"
	"testing"
)

func groupVolume(t *testing.T) (*Volume, *countingWriter, string) {
	t.Helper()
	img, _ := journalImage(t, 0x40000, 100, 0x4400)
	v, c, path := jwFile(t, img, true)
	if err := v.SetDurability(DurabilityLazy); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		c.failAt, c.syncFailAt = -1, -1
		v.Close()
	})
	return v, c, path
}

// A commit the group cannot hold is refused untouched; a group that remembers more
// freed spans than its bound is refused.
func TestGroupAbsorbRefusals(t *testing.T) {
	v, _, _ := groupVolume(t)
	for name, runs := range map[string][]writeRun{
		"unaligned offset": {{off: 0x30000 + 1, data: make([]byte, 512)}},
		"unaligned length": {{off: 0x30000, data: make([]byte, 513)}},
	} {
		if v.absorb(runs, nil, nil) {
			t.Errorf("%s: absorbed", name)
		}
		if v.grp != nil {
			t.Errorf("%s: left a group behind", name)
		}
	}
	ok := []writeRun{{off: 0x30000, data: make([]byte, 512)}}
	if !v.absorb(ok, nil, nil) {
		t.Fatal("an aligned sector was refused")
	}
	for i := 0; i <= maxGroupFreed; i++ {
		v.jfreed = append(v.jfreed, span{int64(i) * 1024, int64(i)*1024 + 512})
	}
	if v.absorb([]writeRun{{off: 0x30200, data: make([]byte, 512)}}, nil, nil) {
		t.Error("more than maxGroupFreed freed spans were absorbed")
	}
	if len(v.grp.sectors) != 1 {
		t.Errorf("a refused commit changed the group: %d sectors", len(v.grp.sectors))
	}
	v.jfreed = nil
}

func TestGroupNoteFreedMergesNeighbours(t *testing.T) {
	v, _, _ := groupVolume(t)
	v.noteFreed(5, 512)
	v.noteFreed(6, 512)
	if len(v.jfreed) != 1 || v.jfreed[0] != (span{5 * 512, 7 * 512}) {
		t.Fatalf("adjacent blocks did not merge: %v", v.jfreed)
	}
	v.noteFreed(8, 512)
	if len(v.jfreed) != 2 {
		t.Fatalf("a gap merged: %v", v.jfreed)
	}
	v.jfreed = nil
}

// flushGroup on the edges: no group, an empty group, an empty group that owes a
// barrier for in-place data, a group no transaction can hold, a failing medium.
func TestGroupFlushEdges(t *testing.T) {
	v, c, _ := groupVolume(t)
	if err := v.flushGroup(); err != nil {
		t.Fatalf("no group: %v", err)
	}

	before, w := c.syncs, len(c.writes)
	v.grp = &jgroup{sectors: map[int64][]byte{}}
	if err := v.flushGroup(); err != nil || v.grp != nil || c.syncs != before || len(c.writes) != w {
		t.Fatalf("empty group: err %v, grp %v, %d barriers, %d writes", err, v.grp, c.syncs-before, len(c.writes)-w)
	}

	v.grp = &jgroup{sectors: map[int64][]byte{}, dirty: true}
	if err := v.flushGroup(); err != nil || v.grp != nil || c.syncs != before+1 || len(c.writes) != w {
		t.Fatalf("empty dirty group: err %v, grp %v, %d barriers, %d writes", err, v.grp, c.syncs-before, len(c.writes)-w)
	}

	many := &jgroup{sectors: map[int64][]byte{}}
	for i := 0; i < jwMaxRuns+10; i++ {
		many.sectors[0x30000+int64(i)*1024] = make([]byte, 512)
	}
	v.grp = many
	w = len(c.writes)
	if err := v.flushGroup(); err == nil || v.grp != many || len(c.writes) != w {
		t.Fatalf("a group of %d runs: err %v, kept %v, %d writes", jwMaxRuns+10, err, v.grp == many, len(c.writes)-w)
	}

	v.grp = &jgroup{sectors: map[int64][]byte{0x30000: make([]byte, 512)}}
	c.failAt = len(c.writes)
	if err := v.flushGroup(); !errors.Is(err, errInjected) {
		t.Fatalf("a failing medium: %v", err)
	}
	if v.grp == nil {
		t.Error("a failed flush dropped the group")
	}
	c.failAt = -1
	v.grp = nil
}

// A commit that does not fit beside the group flushes it, and a failure of that flush
// is the commit's error; the commit that starts the next group is not written.
func TestGroupOverflowFlushFailsAndStartsTheNextGroup(t *testing.T) {
	v, c, path := groupVolume(t)
	var failed bool
	for i := 0; i < 200 && !failed; i++ {
		c.failAt = len(c.writes)
		err := v.WriteFile(fmt.Sprintf("/f%03d", i), pat(3000, byte(i)), 0o644)
		c.failAt = -1
		if errors.Is(err, errInjected) {
			failed = true
		} else if err != nil {
			t.Fatal(err)
		}
	}
	if !failed {
		t.Fatal("no commit ever flushed the group")
	}

	v2, _, path2 := groupVolume(t)
	for i := 0; i < 60; i++ {
		name := fmt.Sprintf("f%03d", i)
		if err := v2.WriteFile("/"+name, pat(3000, byte(i)), 0o644); err != nil {
			t.Fatal(err)
		}
		if v2.grp == nil || len(v2.grp.sectors) == 0 {
			t.Fatalf("commit %d is not in the group", i)
		}
		if slices.Contains(replayedNames(t, path2), name) {
			t.Fatalf("commit %d reached the medium before any flush", i)
		}
	}
	_ = path
}
