// Copyright (c) 2026, go-filesystems
// SPDX-License-Identifier: BSD-3-Clause

package hfsplus

import (
	"bytes"
	"testing"
)

// Revoke-forced staging has a bound of its own (hfsplus#61): a caller that writes
// into a zone without going through allocData (which keeps the forced blocks within
// the small-data bound) must not grow jforced without limit. Past the bound the
// stage takes the direct path: the journal is emptied and the bytes go in place.
func TestJournalDataForcedStagingIsBounded(t *testing.T) {
	img, jbase := journalImage(t, 0x40000, 100, 0x4400)
	v, _, path := jwFile(t, img, true)
	defer v.Close()
	if err := v.MkDir("/d", 0o755); err != nil {
		t.Fatal(err)
	}
	bound := v.jw.dataBound()
	// A zone over free blocks, several times the bound.
	const off = 2 << 20
	size := int(bound) * 3
	v.jw.zones = []span{{off, off + int64(size)}}
	want := pat(size, 11)
	if err := v.stageData(want, off); err != nil {
		t.Fatal(err)
	}
	var staged int
	for _, r := range v.jforced {
		staged += len(r.data)
	}
	if int64(staged) > bound || v.jforcedBytes > bound {
		t.Errorf("jforced holds %d bytes (counted %d), the bound is %d", staged, v.jforcedBytes, bound)
	}
	// The journal was emptied before the bytes went in place: nothing replays over them.
	if h := journalHeaderOf(t, mustRead(t, path), jbase); h.start != h.end {
		t.Errorf("journal not empty after the early direct path: start %d end %d", h.start, h.end)
	}
	if err := v.flush(); err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(mustRead(t, path)[off:off+size], want) {
		t.Error("the data is not on the medium")
	}
}
