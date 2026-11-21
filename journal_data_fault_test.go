// Copyright (c) 2026, go-filesystems
// SPDX-License-Identifier: BSD-3-Clause

package hfsplus

import (
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
			v, c, _ := jwFile(t, img, true)
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
	t.Run("direct fallback with data", func(t *testing.T) {
		faultSweep(t, "fallback", func(v *Volume) {
			setup(v)
			v.jw.prevSize = 0x40000 - 512 - 4096
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
