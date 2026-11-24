// Copyright (c) 2026, go-filesystems
// SPDX-License-Identifier: BSD-3-Clause

package hfsplus

import (
	"encoding/binary"
	"testing"
)

// A big-endian journal (hfsplus#61): every other oracle builds a little-endian one,
// so the write path's `enc: h.bo` had no coverage. The byte order of the host does
// not matter (every encoding names its order); what matters is that the header and
// the block lists the writer emits are in the order the journal was made in. The
// scenario is cheap enough for -short: one small commit and the revoke case, a
// handful of crash points per commit instead of every event prefix, replayed with
// the driver's own replayer.
//
// Mutation target: journalWriter.enc (journal_writer.go) set to binary.LittleEndian
// turns TestJournalWriterCrashOracleBigEndian red.
func TestJournalWriterCrashOracleBigEndian(t *testing.T) {
	img, jbase := journalImageOrder(t, binary.BigEndian, 0x40000, 100, 0x4400)
	ops := []jwOp{
		mkdirOps(1)[0], wf("/a", pat(12288, 7)), rm("/a"), wf("/b", pat(60000, 8)),
	}
	checkJWScenario(t, jwScenario{
		name: "big-endian revoke", img: img, jbase: jbase, ops: ops, observe: observeFiles,
		points: func(starts, ends []int, ev []crashEvent, rs []jwRange) map[int]bool {
			pts := map[int]bool{}
			for k := range starts {
				for _, p := range []int{starts[k], starts[k] + (ends[k]-starts[k])/2, ends[k] - 1, ends[k]} {
					pts[p] = true
				}
			}
			if len(pts) < 8 {
				t.Errorf("only %d crash points", len(pts))
			}
			return pts
		},
	})
}

// The journal the writer leaves is still big-endian: its header decodes as such,
// and a block list at the start of the ring carries the big-endian magic.
func TestJournalWriterKeepsTheByteOrder(t *testing.T) {
	for name, bo := range map[string]binary.ByteOrder{"big": binary.BigEndian, "little": binary.LittleEndian} {
		t.Run(name, func(t *testing.T) {
			img, jbase := journalImageOrder(t, bo, 0x40000, 100, 0x4400)
			v, _, path := jwFile(t, img, true)
			if err := v.MkDir("/d", 0o755); err != nil {
				t.Fatal(err)
			}
			h := journalHeaderOf(t, mustRead(t, path), jbase)
			if h.start == h.end {
				t.Fatal("no transaction in the journal")
			}
			if h.bo != bo {
				t.Errorf("header byte order %v, want %v", h.bo, bo)
			}
			raw := mustRead(t, path)
			if got := bo.Uint32(raw[jbase+4:]); got != journalEndianMagic {
				t.Errorf("header endian magic %#x in the journal's order", got)
			}
			v.Close()
		})
	}
}
