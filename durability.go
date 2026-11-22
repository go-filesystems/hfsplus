// Copyright (c) 2026, go-filesystems
// SPDX-License-Identifier: BSD-3-Clause

package hfsplus

import (
	"errors"
	"fmt"
)

// Durability says when a mutation of a writable volume is on the medium and what
// the volume looks like to fsck if the machine dies meanwhile (hfsplus#35).
//
// Every level keeps the one guarantee the format needs: a volume whose header
// still says "cleanly unmounted" (kHFSVolumeUnmountedBit, and not journaled) is
// consistent. They differ in how many barriers (fsync, which on macOS is
// F_FULLFSYNC: Go's File.Sync) a commit pays and in when the header says so again.
type Durability int

const (
	// DurabilityStrict is the default and the strongest: every commit is durable
	// when its mutator returns, and the volume is marked cleanly unmounted again
	// between commits, so a crash at ANY point outside a commit leaves a volume
	// fsck_hfs -q passes. Cost: three barriers per commit on a volume that was
	// clean when opened (data and the cleared bit; B-tree and bitmap sectors;
	// alternate and primary header). On a journaled volume whose journal passes the
	// checks (the default, see JournalWritesReason) a commit is a journal
	// transaction instead: one barrier, two for a session's first.
	DurabilityStrict Durability = iota
	// DurabilityMounted behaves like a mounted HFS+ volume: the first commit
	// marks the volume as not cleanly unmounted (one barrier), later commits write
	// everything and take ONE barrier, and the clean mark is restored by Sync,
	// Close or SetDurability(DurabilityStrict). Each commit is durable when its
	// mutator returns, but a crash between Syncs leaves the volume marked dirty
	// (the next mount runs fsck_hfs, which finds nothing to repair unless a commit
	// was torn). Nothing inside the metadata of a commit is ordered: the dirty
	// mark is what protects it. Data is ordered (like ext4 data=ordered): a
	// commit that wrote file data takes a barrier between the data and the
	// metadata that points at it, so a crash never shows a file holding bytes it
	// was not written with; a commit without data takes one barrier. The
	// journaled bit stays cleared for the whole held span, so after a crash in it
	// the journal stays off until diskutil enableJournal. None of this applies on a
	// journaled volume written through its journal (the default, see
	// JournalWritesReason): there Mounted is the journal commit, the same as Strict,
	// and the volume is never held dirty. A commit that cannot be journaled (see the
	// list in journal_writer.go) takes the Strict direct path at Mounted too, bracketed
	// and three barriers, so the volume is not held dirty while it journals.
	DurabilityMounted
	// DurabilityLazy is DurabilityMounted without the per-commit barrier: a
	// mutation is durable only after Sync, Close or SetDurability(DurabilityStrict)
	// (Batch does this for a group of mutations). A crash before that loses the
	// mutations made since the last Sync, in whole or in part, and leaves the
	// volume marked dirty. It is data-writeback: nothing orders file data before
	// the metadata that references it, so after a crash a file may contain stale
	// bytes of a previously deleted file. Do not use it where different users
	// share a volume.
	DurabilityLazy
)

// String names the level.
func (d Durability) String() string {
	switch d {
	case DurabilityStrict:
		return "strict"
	case DurabilityMounted:
		return "mounted"
	case DurabilityLazy:
		return "lazy"
	}
	return fmt.Sprintf("Durability(%d)", int(d))
}

// SetDurability selects the level for the following mutations. Leaving a level
// that holds the volume dirty first settles it: everything written is made durable
// and, for DurabilityStrict, the clean mark is restored. It returns the error of
// that settling, and the level is unchanged when it fails. Like every mutator,
// it must not run concurrently with another mutator, Sync, Batch or Close.
func (v *Volume) SetDurability(d Durability) error {
	if d < DurabilityStrict || d > DurabilityLazy {
		return fmt.Errorf("hfsplus: unknown durability %d", int(d))
	}
	if !v.wr {
		return ErrReadOnly
	}
	if d == v.durability {
		return nil
	}
	if v.held || v.unsynced {
		if err := v.Sync(); err != nil {
			return err
		}
	}
	if v.journaling() && d != DurabilityStrict && d != DurabilityMounted {
		// Journal writes apply at Strict and Mounted; Strict <-> Mounted needs no
		// emptying. Leave the journal empty before a level that writes in place.
		if err := v.emptyJournal(); err != nil {
			v.failed = err
			return err
		}
	}
	v.durability = d
	return nil
}

// Durability returns the current level.
func (v *Volume) Durability() Durability { return v.durability }

// Batch runs fn with DurabilityLazy, then makes everything fn wrote durable with
// one Sync and restores the previous level: a group of mutations pays one set of
// barriers instead of one per mutation (an SMB request that writes and sets times,
// say). It is not a transaction: if fn fails or the machine dies, the mutations
// made so far stay (or are partly lost) and the volume is left marked dirty; fn's
// error is returned in preference to Sync's. Like SetDurability, it must not run
// concurrently with mutators; it has DurabilityLazy's data-writeback exposure.
func (v *Volume) Batch(fn func() error) error {
	prev := v.durability
	if err := v.SetDurability(DurabilityLazy); err != nil {
		return err
	}
	ferr := fn()
	serr := v.Sync()
	if prev != DurabilityLazy {
		if err := v.SetDurability(prev); err != nil && serr == nil {
			serr = err
		}
	}
	return errors.Join(ferr, serr)
}

// flushHeld is flush for DurabilityMounted and DurabilityLazy: the volume is made
// dirty on the medium once (a barrier), every commit then writes its sectors in
// place, and a commit is made durable with one barrier (Mounted) or left to Sync
// (Lazy). Headers are written with the clean marks off; settle puts them back.
func (v *Volume) flushHeld(meta, hdrP, hdrA []writeRun, old []byte, journaled bool) error {
	pOff, aOff := int64(volumeHeaderOffset), v.altOff
	if !v.held && (v.cleanOnDisk || journaled) {
		v.restoreUnmounted, v.restoreJournaled = v.cleanOnDisk, journaled
		old[vhAttributes+2] &^= kHFSVolumeUnmountedBit >> 8
		old[vhAttributes+2] &^= kHFSVolumeJournaledBit >> 8
		v.touched = true
		if err := v.putAt(old, pOff); err != nil {
			return err
		}
		// The dirty mark (and the data written so far) before any sector moves.
		if err := v.fsync(); err != nil {
			return err
		}
		v.held = true
		v.dataDirty = false // that fsync made the data durable too
	}
	v.touched = true
	// Data-ordered (like ext4 data=ordered): file data is durable before the
	// metadata that points at it, or a crash can show a new file holding the bytes
	// of a deleted one. A commit without data pays no extra barrier. Lazy skips it
	// by design (data-writeback).
	if v.dataDirty && v.durability == DurabilityMounted {
		if err := v.fsync(); err != nil {
			return err
		}
	}
	for _, h := range []struct {
		off int64
		dst *[]writeRun
	}{{pOff, &hdrP}, {aOff, &hdrA}} {
		// Both headers are written whole every commit: the alternate has never
		// seen the dirty mark, and neither has anything to wait for.
		b, err := v.region(h.off, 512)
		if err != nil {
			return err
		}
		b[vhAttributes+2] &^= (kHFSVolumeUnmountedBit | kHFSVolumeJournaledBit) >> 8
		*h.dst = []writeRun{{off: h.off, data: b}}
	}
	for _, phase := range [][]writeRun{meta, hdrA, hdrP} {
		for _, run := range phase {
			if err := v.putAt(run.data, run.off); err != nil {
				return err
			}
		}
	}
	v.unsynced = true
	if v.durability == DurabilityMounted {
		if err := v.fsync(); err != nil {
			return err
		}
		v.unsynced = false
	}
	v.commitDone()
	return nil
}

// settle makes everything written durable and, if the volume was held dirty,
// restores the clean marks: both headers are rewritten with them (alternate, then
// primary) after a barrier that makes every sector durable first. A volume that
// failed is left as it is, dirty.
func (v *Volume) settle() error {
	if v.jw != nil && !v.held && !v.unsynced {
		// Journal mode (design section 7): every commit is already durable; one barrier
		// if in-place writes are pending since the last one, nothing otherwise.
		if v.jw.pending {
			return v.fsync()
		}
		return nil
	}
	if v.unsynced || !v.held {
		if err := v.fsync(); err != nil {
			return err
		}
		v.unsynced = false
	}
	if !v.held || v.failed != nil {
		return nil
	}
	for _, off := range []int64{v.altOff, volumeHeaderOffset} {
		b := make([]byte, 512)
		if _, err := v.readUnder(b, off); err != nil {
			return err
		}
		if v.restoreUnmounted {
			b[vhAttributes+2] |= kHFSVolumeUnmountedBit >> 8
		}
		if v.restoreJournaled {
			b[vhAttributes+2] |= kHFSVolumeJournaledBit >> 8
		}
		if err := v.putAt(b, off); err != nil {
			return err
		}
	}
	if err := v.fsync(); err != nil {
		return err
	}
	v.held = false
	return nil
}
