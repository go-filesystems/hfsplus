// Copyright (c) 2026, go-filesystems
// SPDX-License-Identifier: BSD-3-Clause

package hfsplus

import (
	"encoding/binary"
	"errors"
	"testing"
)

// Every write and every barrier of a commit that carries file data fails once, in
// turn: the mutator must return the injected error, never swallow it.
//
// prelude runs unfaulted, op is the mutator under test. Each pass uses a fresh volume.
func faultSweep(t *testing.T, name string, prelude func(*Volume), op func(*Volume) error) {
	t.Helper()
	for _, syncs := range []bool{false, true} {
		hit := 0
		for i := 0; i < 400; i++ {
			img, _ := journalImage(t, 0x40000, 100, 0x4400)
			v, c, path := jwFile(t, img, true)
			if prelude != nil {
				prelude(v)
			}
			if syncs {
				c.syncFailAt = c.syncs + i
			} else {
				c.failAt = len(c.writes) + i
			}
			err := op(v)
			injected := c.syncFailAt == -2
			for _, w := range c.writes {
				injected = injected || w.off == -1
			}
			if !injected {
				break // the commit has fewer than i writes: every one was failed in turn
			}
			hit++
			if !errors.Is(err, errInjected) {
				t.Errorf("%s: failing %s %d returned %v, want the injected error", name, map[bool]string{false: "write", true: "barrier"}[syncs], i, err)
			}
			// After the failure nothing else reaches the device: the volume is failed,
			// and a retried write or barrier would land on a half-written commit.
			if c.failedEvents == 0 || len(c.events)+1 != c.failedEvents {
				t.Errorf("%s: failing %s %d: %d events after the failure, want 0", name, map[bool]string{false: "write", true: "barrier"}[syncs], i, len(c.events)+1-c.failedEvents)
			}
			// What the device holds is a crash image: it must open and replay.
			left := mustRead(t, path)
			if _, err := Open(bytesReaderAt(left), int64(len(left)), WithJournalReplay()); err != nil {
				t.Errorf("%s: failing %s %d: the image left on the device does not replay: %v", name, map[bool]string{false: "write", true: "barrier"}[syncs], i, err)
			}
			c.failAt, c.syncFailAt = -1, -1
		}
		if hit == 0 {
			t.Errorf("%s: no failure was injected (syncs=%v): the sweep proves nothing", name, syncs)
		}
	}
}

func TestJournalDataFaults(t *testing.T) {
	setup := func(v *Volume) {
		for _, op := range []jwOp{wf("/a", pat(8192, 7)), rm("/a")} {
			if err := op(v); err != nil {
				t.Fatal(err)
			}
		}
	}
	t.Run("small", func(t *testing.T) {
		faultSweep(t, "small", nil, func(v *Volume) error { return v.WriteFile("/s", pat(300, 1), 0o644) })
	})
	t.Run("large", func(t *testing.T) {
		faultSweep(t, "large", nil, func(v *Volume) error { return v.WriteFile("/l", pat(100000, 2), 0o644) })
	})
	t.Run("candidates then spill", func(t *testing.T) {
		faultSweep(t, "spill", nil, func(v *Volume) error {
			if err := v.writeData(pat(512, 1), 1<<20); err != nil {
				return err
			}
			if err := v.writeData(pat(40000, 2), 2<<20); err != nil {
				return err
			}
			return v.flush()
		})
	})
	t.Run("revoke forced", func(t *testing.T) {
		faultSweep(t, "forced", setup, func(v *Volume) error { return v.WriteFile("/b", pat(8192, 8), 0o644) })
	})
	t.Run("forged candidate takes the ordered path", func(t *testing.T) {
		data := append(pat(512, 2), forgedHeader(binary.LittleEndian, 0)...)
		faultSweep(t, "ordered", nil, func(v *Volume) error { return v.WriteFile("/f", data, 0o644) })
	})
	t.Run("direct fallback with candidates", func(t *testing.T) {
		faultSweep(t, "fallback-small", func(v *Volume) {
			if err := v.MkDir("/m", 0o755); err != nil { // makes the journal live
				t.Fatal(err)
			}
			// A journal in which nothing fits, empty or not (a full one would take the
			// journal-full path, design section 7, not the direct one swept here).
			v.jw.loc.hdr.size = 1024
		}, func(v *Volume) error {
			err := v.WriteFile("/b", pat(300, 8), 0o644)
			if err == nil && v.journaling() {
				t.Error("the commit did not fall back: the sweep reaches no writeStaged")
			}
			return err
		})
	})
	t.Run("forced past the bound (hfsplus#61)", func(t *testing.T) {
		// A candidate, a small forced piece, then a forced piece past the bound: the early
		// direct path writes all three in place, and a failure of any write is returned.
		faultSweep(t, "forced-bound", func(v *Volume) {
			if err := v.MkDir("/m", 0o755); err != nil { // makes the journal live
				t.Fatal(err)
			}
			big := v.jw.dataBound() * 3
			v.jw.zones = []span{{2 << 20, 2<<20 + big}, {3 << 20, 3<<20 + 8192}}
		}, func(v *Volume) error {
			big := int(v.jw.dataBound()) * 3
			for _, w := range []struct {
				p   []byte
				off int64
			}{{pat(512, 1), 1 << 20}, {pat(8192, 2), 3 << 20}, {pat(big, 3), 2 << 20}} {
				if err := v.stageData(w.p, w.off); err != nil {
					return err
				}
			}
			return v.flush()
		})
	})
	t.Run("forced past the bound, Lazy group (hfsplus#61)", func(t *testing.T) {
		// The open group is flushed first: a failure of its write is returned.
		faultSweep(t, "forced-bound-lazy", func(v *Volume) {
			if err := v.SetDurability(DurabilityLazy); err != nil {
				t.Fatal(err)
			}
			if err := v.MkDir("/m", 0o755); err != nil { // joins the group
				t.Fatal(err)
			}
			v.jw.zones = []span{{2 << 20, 2<<20 + v.jw.dataBound()*3}}
		}, func(v *Volume) error {
			return v.stageData(pat(int(v.jw.dataBound())*3, 3), 2<<20)
		})
	})
	t.Run("forged forced block reaches flush's writeStaged (hfsplus#55)", func(t *testing.T) {
		// A revoke-forced block (one a replay could reach) that verifies as a block list
		// header can be neither journaled nor written in place under the journal: the
		// commit takes the direct path, empties the journal, and flush's own writeStaged
		// puts the staged block in place. A failure of that write is returned and nothing
		// follows it.
		data := forgedHeader(binary.LittleEndian, 0)
		faultSweep(t, "forced-forged", setup, func(v *Volume) error {
			err := v.WriteFile("/b", data, 0o644)
			if err == nil && v.journaling() && v.jw.active {
				t.Error("the commit did not fall back: the sweep reaches no flush writeStaged")
			}
			return err
		})
	})
	t.Run("direct fallback with data", func(t *testing.T) {
		faultSweep(t, "fallback", func(v *Volume) {
			setup(v)
			v.jw.loc.hdr.size = 1024
		}, func(v *Volume) error { return v.WriteFile("/b", pat(8192, 8), 0o644) })
	})
}

// Adjacent pieces of one transaction's data are one run; others are not.
func TestAppendRunCoalesces(t *testing.T) {
	var runs []writeRun
	runs = appendRun(runs, []byte{1, 2}, 100)
	runs = appendRun(runs, []byte{3}, 102)
	if len(runs) != 1 || len(runs[0].data) != 3 || runs[0].off != 100 {
		t.Fatalf("adjacent: %v", runs)
	}
	runs = appendRun(runs, []byte{4}, 200)
	if len(runs) != 2 {
		t.Fatalf("apart: %v", runs)
	}
	runs = appendRun(runs, []byte{5}, 150)
	if len(runs) != 3 {
		t.Fatalf("overlapping-gap: %v", runs)
	}
}
