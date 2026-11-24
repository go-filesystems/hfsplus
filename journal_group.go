// Copyright (c) 2026, go-filesystems
// SPDX-License-Identifier: BSD-3-Clause

package hfsplus

import (
	"fmt"
	"slices"
)

// Group commit (docs/design/journal-write.md section 8; hfsplus#45, PR 7).
//
// With journal writes on, DurabilityLazy does not write per commit. A commit is
// absorbed into the group: the sectors it changed (metadata, both volume headers,
// small data) are copied into jgroup.sectors, and reads see them through
// overlayGroup. Nothing reaches the medium until the group is flushed, as ONE
// journal transaction (emitTxn, the same commit protocol as Strict: one barrier,
// two for a session's first), by Sync, Close, SetDurability away from Lazy (so by
// Batch's end), or because the next commit would not fit.
//
// The group is bounded, which is what bounds the memory of a long Lazy session:
//
//   - at most jw.dataBound() bytes of sectors (the smaller of 1 MiB and
//     (journal size - jhdr_size)/8: three consecutive transactions stay under the
//     journal-full threshold) = at most 2048 sectors, so at most about 1 MiB of
//     sector copies plus the map;
//   - at most jwMaxRuns runs (one block list header);
//   - at most maxGroupFreed spans of freed blocks.
//
// A commit that would pass a bound flushes the group first (a transaction of whole
// commits), then starts a new group with itself; one that cannot be a group alone
// takes the Strict path (a transaction of its own, or the direct path with the
// journal emptied first). The volume is never held dirty while it journals.
//
// File data: writeData stages at Lazy exactly as at Strict, and a commit's staged
// data is absorbed into the group (journaled with the metadata). Large data goes in
// place at once, so it must land only in blocks nothing durable references: the
// allocator's zones therefore also cover the blocks the group freed (they are
// still referenced by the durable volume until the group is flushed), and a zone
// block is taken only as journaled data. A barrier orders the in-place data before
// the group's transaction.

// maxGroupFreed bounds the freed-block spans a group remembers.
const maxGroupFreed = 1 << 12

type jgroup struct {
	// sectors: the state of every sector the group changed, as of the last absorbed
	// commit (raw: the header sectors without the journal stamp).
	sectors map[int64][]byte
	// freed: allocation-block spans freed by the group's commits, sorted, merged.
	freed []span
	// dirty: data was written in place since the last barrier.
	dirty bool
}

// groupMode reports whether commits are absorbed instead of written.
func (v *Volume) groupMode() bool { return v.jw != nil && v.durability == DurabilityLazy }

// noteFreed records block n as freed by the commit in progress.
func (v *Volume) noteFreed(n uint32, bs int64) {
	lo := int64(n) * bs
	if k := len(v.jfreed); k > 0 && v.jfreed[k-1].hi == lo {
		v.jfreed[k-1].hi = lo + bs
		return
	}
	v.jfreed = append(v.jfreed, span{lo, lo + bs})
}

// mergeSpans returns the sorted, merged union of the sets.
func mergeSpans(sets ...[]span) []span {
	var all []span
	for _, s := range sets {
		all = append(all, s...)
	}
	slices.SortFunc(all, func(a, b span) int { return int(a.lo - b.lo) })
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

// allocZones is the zone set the allocator uses: the transactions' zones, plus the
// blocks the group freed.
func (v *Volume) allocZones() []span {
	if v.grp == nil || len(v.grp.freed) == 0 {
		return v.jw.zones
	}
	return mergeSpans(v.jw.zones, v.grp.freed)
}

// groupTweak is what a journaled header sector is in the transaction: the journal
// stamp, mounted, journaled (journalHeaderRuns).
func groupTweak(b []byte) []byte {
	c := slices.Clone(b)
	copy(c[vhLastMountedVersion:], "HFSJ")
	c[vhAttributes+2] &^= kHFSVolumeUnmountedBit >> 8
	c[vhAttributes+2] |= kHFSVolumeJournaledBit >> 8
	return c
}

func (v *Volume) isHeaderOff(off int64) bool { return off == volumeHeaderOffset || off == v.altOff }

// absorb adds the commit in progress to the group. It returns false, having
// changed nothing, when the group plus the commit would pass a bound or cannot be a
// transaction; the caller then flushes the group and retries or takes the Strict path.
func (v *Volume) absorb(meta, hdrP, hdrA []writeRun) bool {
	jw := v.jw
	add := make(map[int64][]byte)
	for _, p := range [][]writeRun{meta, hdrP, hdrA, v.jdata, v.jforced} {
		for _, r := range p {
			if r.off%jwBlockSize != 0 || len(r.data)%jwBlockSize != 0 {
				return false
			}
			for o := 0; o < len(r.data); o += jwBlockSize {
				add[r.off+int64(o)] = r.data[o : o+jwBlockSize]
			}
		}
	}
	for off, b := range add {
		if v.isHeaderOff(off) {
			b = groupTweak(b)
		}
		if verifiesAsBlockListHeader(b) {
			return false
		}
	}
	g := v.grp
	var have map[int64][]byte
	var freed []span
	if g != nil {
		have, freed = g.sectors, g.freed
	}
	offs := make([]int64, 0, len(have)+len(add))
	for k := range have {
		offs = append(offs, k)
	}
	for k := range add {
		if _, dup := have[k]; !dup {
			offs = append(offs, k)
		}
	}
	slices.Sort(offs)
	runs := 0
	for i, o := range offs {
		if i == 0 || offs[i-1]+jwBlockSize != o {
			runs++
		}
	}
	tsize := uint64(jwBlockSize) + uint64(len(offs))*jwBlockSize
	switch {
	case len(offs) > 0 && runs > jwMaxRuns,
		int64(len(offs))*jwBlockSize > jw.dataBound(),
		tsize >= jw.loc.hdr.size-uint64(jw.loc.hdr.jhdrSize):
		return false
	}
	merged := freed
	if len(v.jfreed) > 0 {
		merged = mergeSpans(freed, v.jfreed)
		if len(merged) > maxGroupFreed {
			return false
		}
	}
	if g == nil {
		g = &jgroup{sectors: make(map[int64][]byte, len(add))}
	}
	for off, b := range add {
		g.sectors[off] = slices.Clone(b)
	}
	g.freed = merged
	if v.dataDirty {
		g.dirty = true
	}
	v.grp = g
	return true
}

// overlayGroup lays the group's sectors over p, which was read at off, and returns
// how many bytes of p they cover from its start (0 when none).
func (v *Volume) overlayGroup(p []byte, off int64) int {
	g := v.grp
	if g == nil || len(g.sectors) == 0 {
		return 0
	}
	end := off + int64(len(p))
	hiMax := 0
	apply := func(s int64, b []byte) {
		lo, hi := max(s, off), min(s+jwBlockSize, end)
		if lo >= hi {
			return
		}
		copy(p[lo-off:hi-off], b[lo-s:hi-s])
		hiMax = max(hiMax, int(hi-off))
	}
	for s := off / jwBlockSize * jwBlockSize; s < end; s += jwBlockSize {
		if b, ok := g.sectors[s]; ok {
			apply(s, b)
		}
	}
	return hiMax
}

// flushGroup makes the group one journal transaction. Nothing happens when there is
// no group.
func (v *Volume) flushGroup() error {
	g := v.grp
	if g == nil {
		return nil
	}
	if len(g.sectors) == 0 {
		v.grp = nil
		if g.dirty {
			return v.fsync()
		}
		return nil
	}
	jw := v.jw
	offs := make([]int64, 0, len(g.sectors))
	for k := range g.sectors {
		offs = append(offs, k)
	}
	slices.Sort(offs)
	var runs []writeRun
	for _, o := range offs {
		b := g.sectors[o]
		if v.isHeaderOff(o) {
			b = groupTweak(b)
		}
		if k := len(runs); k > 0 && runs[k-1].off+int64(len(runs[k-1].data)) == o {
			runs[k-1].data = append(runs[k-1].data, b...)
			continue
		}
		runs = append(runs, writeRun{off: o, data: slices.Clone(b)})
	}
	planned, tsize, ok := jw.plan(runs, nil, nil, nil, nil, false)
	full := false
	if !ok && jw.active {
		planned, tsize, ok = jw.plan(runs, nil, nil, nil, nil, true)
		full = ok
	}
	if !ok {
		return fmt.Errorf("hfsplus: the accumulated commits no longer fit one journal transaction")
	}
	if _, err := v.emitTxn(planned, tsize, full, false, g.dirty); err != nil {
		return err
	}
	v.grp = nil
	v.touched = false
	return nil
}
