// Copyright (c) 2026, go-filesystems
// SPDX-License-Identifier: BSD-3-Clause

package hfsplus

import (
	"fmt"
	"io"
	"sort"
)

// Replaying a journal on a writable open (docs/design/journal-write.md, section
// 12 item 2; hfsplus#45, PR 2).
//
// OpenWritable, OpenWritableAt and OpenFileWritable used to refuse a journal that
// holds transactions with ErrJournaled. They now replay it into the volume, as
// macOS does at mount (hfs_journal.c:1162-1514), and empty it, then carry on with
// the usual write path. The replay is the one of replay.go, with its bounds.
//
// Order of the writes (TN1150, "Replaying the Journal": "You must ensure that
// previous block writes complete before updating the journal header's start
// field on disk"; design section 4, lines 151-158):
//
//  1. every replayed block, written in place;
//  2. a barrier (Sync): the blocks are durable;
//  3. the journal header with start = end (write_journal_header(jnl, 1, ...),
//     hfs_journal.c:1510, :457-542: the whole jhdr_size sector, checksum over
//     the first 44 bytes);
//  4. a barrier: the empty header is durable before the write path moves
//     anything.
//
// That is 2 barriers at open, at every durability level (SetDurability can only
// be called on the opened volume, and a replay that skipped step 2 could leave an
// emptied journal beside blocks that never landed). The kernel does not take
// step 2: write_journal_header(jnl, 1, ...) flushes only AFTER the header
// (:515-539, updating_start), and relies on update_fs_block's completion. That
// completion is not durability, so this is stricter than Apple, as the design
// says.
//
// A crash at any point leaves either the volume as it was, with the journal still
// non-empty (replay is idempotent: it writes the same blocks again), or the
// replayed volume with an empty journal. Never an empty header over blocks that
// were not written. A replay that cannot be done (ErrJournalReplay) writes nothing:
// nothing is written before the replay has succeeded and its end is checked. A
// WriteAt or Sync error part-way is crash-safe, not "writes nothing": the journal
// stays non-empty and the next open replays to the same result. The open's own
// checks after the replay (checkJournal in hfsplus.go) can still refuse a volume
// the replay rewrote.
//
// Only a volume whose lastMountedVersion is 'HFSJ' is replayed. TN1150,
// "Journal": "If the lastMountedVersion field of the volume header does not match
// the signature of an implementation known to properly use and update the
// journal, then the journal must not be replayed". macOS does not replay it
// either: early journal init needs 'HFSJ' (hfs_vfsops.c:1583-1585); any other
// value takes late init, which opens the journal with JOURNAL_RESET and throws
// the transactions away (hfs_vfsutils.c:723-727, :2886-2895). This driver does not
// port that reset: it keeps refusing such a volume with ErrJournaled. Throwing
// away transactions is a decision about someone else's data (the journal may hold
// the only copy of metadata whose in-place write was torn, and the writer that
// last mounted the volume is by definition not one we can vouch for), and macOS
// itself does it only to avoid replaying over a third party's changes, a risk
// that does not arise when we do not replay. Mount and unmount the volume on
// macOS to have it reset.

const vhLastMountedVersionOff = volumeHeaderOffset + vhLastMountedVersion

// replayStore is what a replay writes to.
type replayStore interface {
	io.ReaderAt
	io.WriterAt
	Sync() error
}

// journalMemStore is the store of OpenWritable: the image in memory, mirrored to
// wa when there is one.
type journalMemStore struct {
	img []byte
	wa  io.WriterAt
}

func (m *journalMemStore) ReadAt(p []byte, off int64) (int, error) {
	return bytesReaderAt(m.img).ReadAt(p, off)
}

func (m *journalMemStore) WriteAt(p []byte, off int64) (int, error) {
	if off < 0 || off > int64(len(m.img)) || int64(len(p)) > int64(len(m.img))-off {
		return 0, fmt.Errorf("hfsplus: write [%d,+%d) outside the %d-byte image", off, len(p), len(m.img))
	}
	copy(m.img[off:], p)
	if m.wa != nil {
		return m.wa.WriteAt(p, off)
	}
	return len(p), nil
}

// canBarrier: a purely in-memory volume (wa == nil) is its own store and ordered;
// a mirror must be able to Sync.
func (m *journalMemStore) canBarrier() bool {
	if m.wa == nil {
		return true
	}
	_, ok := m.wa.(interface{ Sync() error })
	return ok
}

func (m *journalMemStore) Sync() error {
	if s, ok := m.wa.(interface{ Sync() error }); ok {
		return s.Sync()
	}
	return nil
}

// maxReplayRun bounds one WriteAt of the replay.
const maxReplayRun = 1 << 20

// replayPending replays the journal of the volume in st if it holds transactions
// and the volume is one whose journal may be replayed, and empties it. It does
// nothing, and returns nil, for a volume that is not journaled, whose journal is
// empty, or whose journal the checks of checkJournalAt refuse for another reason
// (the caller's checkJournalAt then reports it, unchanged).
func replayPending(st replayStore, size int64) error {
	vh, err := readVolumeHeader(st)
	if err != nil || vh.Attributes&kHFSVolumeJournaledBit == 0 {
		return nil // Open reports the first; the second has no journal
	}
	jst, loc, err := loadJournalAt(st, size, vh)
	if err != nil || jst.empty {
		return nil
	}
	// The same size and alternate-header checks the open makes, before anything is
	// written: a replay into a volume the open then refuses would have written. They
	// use the volume header as it is now; the replayed blocks are bounded by the
	// image below, not by this header's totalBlocks*blockSize, because an
	// interrupted hfs_extendfs journals the new alternate header past the old end
	// (hfs_resize.c:372, :798; hfs_vfsops.c:3897, :3938) and the kernel and the read
	// path (replay.go) bound a block by the device or image, not by the old header.
	if _, err := checkWritableExtent(st, size, vh); err != nil {
		return err
	}
	var lmv [4]byte
	if _, err := st.ReadAt(lmv[:], vhLastMountedVersionOff); err != nil {
		return fmt.Errorf("%w: lastMountedVersion cannot be read: %v", ErrJournaled, err)
	}
	if string(lmv[:]) != "HFSJ" {
		return fmt.Errorf("%w: transactions pending (start %d, end %d) and lastMountedVersion is %q, not 'HFSJ': "+
			"macOS discards such a journal at mount (hfs_vfsutils.c:2886-2895) and TN1150 says it must not be replayed; "+
			"mount and unmount the volume on macOS first (read-only Open without WithJournalReplay shows the blocks in place; "+
			"WithJournalReplay shows the journal's view, which macOS would discard)", ErrJournaled, loc.hdr.start, loc.hdr.end, lmv[:])
	}
	// The two barriers are what keeps an emptied header from sitting over blocks that
	// never landed. A writer given to OpenWritable that cannot Sync gives none, so
	// the journal is not replayed through it. (The write path itself tolerates such a
	// writer, but a torn commit there is declared by the cleared unmounted bit; an
	// emptied journal over missing blocks is declared by nothing.)
	if b, ok := st.(interface{ canBarrier() bool }); ok && !b.canBarrier() {
		return fmt.Errorf("%w: transactions pending and the writer cannot Sync, so the replay could not be ordered "+
			"(blocks durable before the journal header is emptied)", ErrJournaled)
	}
	ov, err := replayJournal(st, loc.off, loc.hdr, uint64(size))
	if err != nil {
		return err
	}
	// journal_open refuses a start or end that is not a multiple of 512 (:2046,
	// :2053), so a header with such an end would not be opened again.
	if ov.end%512 != 0 || ov.end == 0 {
		return replayErrorf("the replay ends at %d, not a multiple of 512", ov.end)
	}

	// 1. The blocks, in runs of adjacent units. A block that lands on the journal
	// itself would change the header this replay then rewrites (the kernel has the
	// same hazard); such a journal is refused.
	units := make([]uint64, 0, len(ov.units))
	for u := range ov.units {
		if off := u * ov.unit; off < loc.off+loc.hdr.size && off+ov.unit > loc.off {
			return replayErrorf("a replayed block at %d lies inside the journal [%d, %d)", off, loc.off, loc.off+loc.hdr.size)
		}
		units = append(units, u)
	}
	sort.Slice(units, func(i, j int) bool { return units[i] < units[j] })
	var run []byte
	var runOff int64
	flushRun := func() error {
		if len(run) == 0 {
			return nil
		}
		if _, err := st.WriteAt(run, runOff); err != nil {
			return fmt.Errorf("hfsplus: writing the replayed journal: %w", err)
		}
		run = run[:0]
		return nil
	}
	for i, u := range units {
		off := int64(u * ov.unit)
		if len(run) > 0 && (i == 0 || units[i-1]+1 != u || len(run)+int(ov.unit) > maxReplayRun) {
			if err := flushRun(); err != nil {
				return err
			}
		}
		if len(run) == 0 {
			runOff = off
		}
		run = append(run, ov.units[u]...)
	}
	if err := flushRun(); err != nil {
		return err
	}
	// 2. Barrier: the blocks are durable before the header says they are not needed.
	if err := st.Sync(); err != nil {
		return fmt.Errorf("hfsplus: syncing the replayed journal: %w", err)
	}
	// 3. The header, start = end (:1453-1456, :1510).
	hdr := make([]byte, loc.hdr.jhdrSize)
	if _, err := st.ReadAt(hdr, int64(loc.off)); err != nil {
		return fmt.Errorf("hfsplus: reading the journal header: %w", err)
	}
	bo := loc.hdr.bo
	bo.PutUint64(hdr[8:], ov.end)
	bo.PutUint64(hdr[16:], ov.end)
	clear(hdr[36:40])
	bo.PutUint32(hdr[36:], journalChecksum(hdr[:journalHeaderLen])) // :505-507
	if _, err := st.WriteAt(hdr, int64(loc.off)); err != nil {
		return fmt.Errorf("hfsplus: writing the journal header: %w", err)
	}
	// 4. Barrier: the empty header is durable before the write path starts.
	if err := st.Sync(); err != nil {
		return fmt.Errorf("hfsplus: syncing the journal header: %w", err)
	}
	return nil
}
