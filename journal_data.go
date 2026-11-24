// Copyright (c) 2026, go-filesystems
// SPDX-License-Identifier: BSD-3-Clause

package hfsplus

import (
	"cmp"
	"slices"
	"sort"
)

// File data through the journal (docs/design/journal-write.md section 6;
// hfsplus#45, PR 4). With journal writes on and DurabilityStrict, writeData does
// not touch the medium: it stages the data, and flushJournal decides at commit
// time.
//
//   - Small data (everything staged, at most jw.dataBound() bytes): the blocks go
//     into T_k as ordinary blocks and in place after the barrier. One barrier.
//   - Large data: the part over the bound is written in place, then a barrier,
//     then the metadata commit: two barriers. The bytes are in blocks nothing
//     durable references, so the order is all that matters.
//   - The revoke rule: a data block that a transaction replay can still reach
//     (T_{k-2}, T_{k-1}) journaled would be overwritten by that replay if the new
//     data went in place unjournaled. Such a block ("zone") is journaled in T_k
//     whatever its size, and the allocator skips it once the bound is spent.
//   - Risk 5: a data sector that verifies as a block list header never enters the
//     ring. Staged data with one takes the ordered path; a revoke-forced block
//     with one (which can be neither journaled nor written in place) sends the
//     commit to the direct path, which first empties the journal.

// span is a byte range [lo, hi) of the image.
type span struct{ lo, hi int64 }

// dataBound is the most data one commit journals: the smaller of 1 MiB and
// (size - jhdr_size)/8, which keeps three consecutive transactions under the
// journal-full threshold (design section 6).
func (jw *journalWriter) dataBound() int64 {
	return min(1<<20, int64(jw.loc.hdr.size-uint64(jw.loc.hdr.jhdrSize))/8)
}

// stagesData reports whether writeData stages instead of writing.
func (v *Volume) stagesData() bool {
	return v.jw != nil
}

// hasStaged reports whether data waits for the commit.
func (v *Volume) hasStaged() bool { return len(v.jdata) > 0 || len(v.jforced) > 0 }

// appendRun adds p at off to runs, joining it to the last run when contiguous. The
// bytes are copied: the caller's slice may be reused.
func appendRun(runs []writeRun, p []byte, off int64) []writeRun {
	if k := len(runs); k > 0 && runs[k-1].off+int64(len(runs[k-1].data)) == off {
		runs[k-1].data = append(runs[k-1].data, p...)
		return runs
	}
	return append(runs, writeRun{off: off, data: slices.Clone(p)})
}

// inZones reports whether off lies in one of the sorted, disjoint zones.
func inZones(zones []span, off int64) bool {
	i := sort.Search(len(zones), func(i int) bool { return zones[i].hi > off })
	return i < len(zones) && zones[i].lo <= off
}

// stageData is writeData for a volume that journals. p is cut at the zone
// boundaries: the parts inside a zone are forced into the transaction, the rest
// is a candidate for small data, and is written in place (the large path) when
// the staged bytes would pass the bound.
func (v *Volume) stageData(p []byte, off int64) error {
	// The zones of the ring plus the blocks the open group freed: still durably
	// referenced until the group's transaction lands, so data over them is journaled.
	zones := v.allocZones()
	for len(p) > 0 {
		n, forced := int64(len(p)), false
		i := sort.Search(len(zones), func(i int) bool { return zones[i].hi > off })
		switch {
		case i < len(zones) && zones[i].lo <= off:
			forced = true
			n = min(n, zones[i].hi-off)
		case i < len(zones):
			n = min(n, zones[i].lo-off)
		}
		piece := p[:n]
		switch {
		case forced && v.jforcedBytes+n > v.jw.dataBound():
			// Past the bound of its own (hfsplus#61): the forced blocks do not all fit one
			// transaction, so this takes the direct path, as a plan that does not fit does
			// (store.go flush): empty the journal, and everything staged goes in place with
			// the piece. Nothing replays over it then, and the staging stays bounded.
			if err := v.settleForced(); err != nil {
				return err
			}
			if err := v.putAt(piece, off); err != nil {
				return err
			}
			v.dataDirty = true
			zones = v.allocZones()
		case forced:
			v.jforced = appendRun(v.jforced, piece, off)
			v.jforcedBytes += n
		case !v.dataSpilled && v.jdataBytes+v.jforcedBytes+n <= v.jw.dataBound():
			v.jdata = appendRun(v.jdata, piece, off)
			v.jdataBytes += n
		default:
			if err := v.spillData(); err != nil {
				return err
			}
			if err := v.putAt(piece, off); err != nil {
				return err
			}
			v.dataDirty = true
		}
		p, off = p[n:], off+n
	}
	return nil
}

// spillData ends small data for this commit: what was staged as a candidate goes in
// place now (it is in blocks nothing durable references), and later data follows.
// The forced blocks stay staged.
func (v *Volume) spillData() error {
	v.dataSpilled = true
	return v.writeOrdered()
}

// settleForced is the direct path of flush, taken early: the open group (Lazy) is
// flushed, the journal emptied, and every staged byte goes in place.
func (v *Volume) settleForced() error {
	if v.groupMode() {
		if err := v.flushGroup(); err != nil {
			return err
		}
	}
	if err := v.emptyJournal(); err != nil {
		return err
	}
	return v.writeStaged()
}

// writeOrdered writes the staged candidates in place and forgets them.
func (v *Volume) writeOrdered() error {
	for _, r := range v.jdata {
		if err := v.putAt(r.data, r.off); err != nil {
			return err
		}
		v.dataDirty = true
	}
	v.jdata, v.jdataBytes = nil, 0
	return nil
}

// writeStaged writes every staged byte in place: the direct path's data, after the
// journal was emptied (so nothing replays over it).
func (v *Volume) writeStaged() error {
	if err := v.writeOrdered(); err != nil {
		return err
	}
	for _, r := range v.jforced {
		if err := v.putAt(r.data, r.off); err != nil {
			return err
		}
		v.dataDirty = true
	}
	v.jforced, v.jforcedBytes = nil, 0
	return nil
}

// dropStaged forgets the staged data (the transaction was abandoned or committed).
func (v *Volume) dropStaged() {
	v.jdata, v.jforced = nil, nil
	v.jdataBytes, v.jforcedBytes = 0, 0
	v.dataSpilled = false
}

// zonesOf returns the allocation-block-aligned, sorted, merged cover of the runs of
// the given transactions.
func zonesOf(bs int64, sets ...[]span) []span {
	var all []span
	for _, s := range sets {
		for _, r := range s {
			all = append(all, span{r.lo / bs * bs, (r.hi + bs - 1) / bs * bs})
		}
	}
	slices.SortFunc(all, func(a, b span) int { return cmp.Compare(a.lo, b.lo) })
	var out []span
	for _, s := range all {
		if k := len(out); k > 0 && s.lo <= out[k-1].hi {
			out[k-1].hi = max(out[k-1].hi, s.hi)
			continue
		}
		out = append(out, s)
	}
	return out
}

// spansOf lists the byte ranges of a transaction's blocks.
func spansOf(runs []writeRun) []span {
	out := make([]span, len(runs))
	for i, r := range runs {
		out[i] = span{r.off, r.off + int64(len(r.data))}
	}
	return out
}
