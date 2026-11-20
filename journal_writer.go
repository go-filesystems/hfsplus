// Copyright (c) 2026, go-filesystems
// SPDX-License-Identifier: BSD-3-Clause

package hfsplus

import (
	"encoding/binary"
	"errors"
	"fmt"
	"slices"
)

// The journal writer (docs/design/journal-write.md sections 4, 5 and 7;
// hfsplus#45, PR 3). Behind an explicit opt-in, EnableJournalWrites, a Strict
// commit on a journaled volume writes its metadata as a journal transaction
// instead of in place first: one barrier per commit instead of three.
//
// What it journals, and what falls back to the direct path (the one the driver
// has always used, emptying the journal first when it is not empty):
//
//   - journaled: a commit that changes only metadata (B-tree nodes, bitmap
//     sectors, the two volume headers), at DurabilityStrict, whose changed
//     sectors make at most 30 runs and whose transaction fits (invariant D);
//   - direct path: a commit that carries file data (user data is not journaled
//     in this PR: design section 11, risk 5); a commit with a sector that
//     verifies as a block list header (forgedSector); a commit with more than 30
//     runs or one that does not fit the journal; every durability level but Strict; and a
//     volume for which EnableJournalWrites refused (not journaled, journal not
//     inside the volume or never initialised, jhdr_size other than 512, or any
//     check of section 4 failing).
//
// Order of a session's first commit (2 barriers): group 1 is T_1 at `end` and the
// primary header in place with lastMountedVersion 'HFSJ', unmounted bit clear,
// journaled bit set (hfs_vfsutils.c:706-710, hfs_vfsops.c:3728-3729); barrier;
// group 2 is H_1 = [start(T_1), end(T_1)) with seq(T_1) (journal.go's header
// layout; write_journal_header, hfs_journal.c:457-542); barrier; then T_1's blocks
// in place, with no barrier. Every later commit k (1 barrier): T_k at end(T_{k-1})
// and H_k = [start(T_{k-1}), end(T_{k-1})) with seq(T_{k-1}), then one barrier,
// then T_k's blocks in place. H_k never names T_k: replay reaches it by reading
// past end (hfs_journal.c:1211-1300), the way macOS reaches the transaction its
// last F_FULLFSYNC wrote.
//
// The first journaled commit of a session starts with scrubRing (hfsplus#58): it
// zeroes any sector of the ring that verifies as a block list header, then one
// barrier if it zeroed any, before T_1 is written; a ring with none costs a read.
//
// The block list header is one sector (blhdr_size = jhdr_size, set in H_1: a
// header that fits one sector cannot be torn between sectors, design section 4),
// so a transaction has at most 30 blocks; each block is one contiguous run of
// changed 512-byte sectors, adjacent runs merged.
//
// Sequence numbers: the first transaction after the journal was empty gets
// header.sequence_num + size/jhdr_size + 1 (0 skipped), as macOS does after a
// block-size change (hfs_journal.c:2097-2099), so no stale block list of an
// earlier lap can follow a new one; each later transaction gets the previous
// number + 1.

// ErrJournalWritesUnavailable is returned by EnableJournalWrites when the volume
// cannot be written through its journal. The volume stays usable and keeps
// writing directly.
var ErrJournalWritesUnavailable = errors.New("hfsplus: this volume cannot be written through its journal")

const (
	// jwBlockSize is the only jhdr_size the writer supports: the driver writes in
	// 512-byte sectors (changedRuns).
	jwBlockSize = 512
	// jwMaxRuns is the most blocks one one-sector block list header holds:
	// blhdr_size/16 - 1 entries, one of them binfo[0].
	jwMaxRuns = jwBlockSize/16 - 2
)

// journalWriter is the writer's state for one volume.
type journalWriter struct {
	loc journalLoc // where the journal is and its header as it was found

	// active: a transaction of this session is in the journal (the header on the
	// medium is not necessarily empty, and the next commit is a "later" commit).
	active bool
	// Transactions T_{k-1} (prev) and T_{k-2} (prev2): offsets and sizes in bytes.
	prevStart, prevEnd uint64
	prevSeq            uint32
	prevSize, prev2    uint64
	// hs, he, hseq: the journal header last written (H_k).
	hs, he uint64
	hseq   uint32
	// end is where the next transaction goes (end of T_{k-1}); valid when active.
	end uint64
	// lastSeq is the newest sequence number used.
	lastSeq uint32
	// enc is the byte order of the journal header.
	enc binary.ByteOrder
	// scrubbed: this session's ring scrub has run (scrubRing); it runs once, before
	// the first journaled commit writes anything.
	scrubbed bool
}

// EnableJournalWrites opts the volume in to journal writes (see the comment at
// the top of journal_writer.go). It returns ErrJournalWritesUnavailable, with the
// reason, when the volume is not journaled or its journal fails the checks of
// section 4; the volume then keeps writing directly, unchanged. It must be called
// on a writable volume, between mutations, and the journal must be empty (a
// non-empty one is replayed at open, see replay_write.go).
func (v *Volume) EnableJournalWrites() error {
	if !v.wr {
		return ErrReadOnly
	}
	if v.closed {
		return ErrClosed
	}
	if v.jw != nil {
		return nil
	}
	// The commit protocol is its two kinds of barrier; a writer that cannot Sync makes
	// fsync a no-op (store.go), so the journal would run with none and a crash could
	// leave an emptied or advanced header over blocks that never landed. The same
	// refusal as the replay at open (replay_write.go). A nil writer is the in-memory
	// image or a file the volume owns (OpenFileWritable gives it a Sync).
	if v.wa != nil {
		if _, ok := v.wa.(interface{ Sync() error }); !ok {
			return fmt.Errorf("%w: the writer cannot Sync, so the commits could not be ordered", ErrJournalWritesUnavailable)
		}
	}
	if v.vh.Attributes&kHFSVolumeJournaledBit == 0 {
		return fmt.Errorf("%w: the volume is not journaled", ErrJournalWritesUnavailable)
	}
	st, loc, err := loadJournalAt(v.under, v.size, v.vh)
	if err != nil {
		return fmt.Errorf("%w: %v", ErrJournalWritesUnavailable, err)
	}
	if loc.hdr.size == 0 {
		return fmt.Errorf("%w: the journal was never initialised", ErrJournalWritesUnavailable)
	}
	if !st.empty {
		return fmt.Errorf("%w: the journal holds transactions", ErrJournalWritesUnavailable)
	}
	h := loc.hdr
	switch {
	case h.jhdrSize != jwBlockSize:
		return fmt.Errorf("%w: jhdr_size is %d, the writer supports %d", ErrJournalWritesUnavailable, h.jhdrSize, jwBlockSize)
	case h.size%uint64(h.jhdrSize) != 0 || h.size < uint64(h.jhdrSize)*uint64(h.jhdrSize/16):
		// hfs_journal.c:1910-1922, with jhdr_size for the device's sector size.
		return fmt.Errorf("%w: journal size %d", ErrJournalWritesUnavailable, h.size)
	case h.blhdrSize == 0 || h.blhdrSize%h.jhdrSize != 0:
		return fmt.Errorf("%w: blhdr_size %d", ErrJournalWritesUnavailable, h.blhdrSize)
	case h.start%uint64(h.jhdrSize) != 0 || h.end%uint64(h.jhdrSize) != 0:
		// The writer puts T_1 at end and every block on the sector grid forgedSector
		// checks. loadJournalAt already refuses this for jhdr_size 512, the only size
		// accepted above; the check stays here because the writer's placement rests on it.
		return fmt.Errorf("%w: start %d or end %d is not a multiple of jhdr_size", ErrJournalWritesUnavailable, h.start, h.end)
	case loc.off%uint64(h.jhdrSize) != 0:
		return fmt.Errorf("%w: the journal is at %d, not sector-aligned", ErrJournalWritesUnavailable, loc.off)
	}
	v.jw = &journalWriter{loc: loc, enc: h.bo}
	return nil
}

// journaling reports whether a transaction of this session is in the journal.
func (v *Volume) journaling() bool { return v.jw != nil && v.jw.active }

// ringEnd wraps an offset the way replay does (hfs_journal.c:1443-1446): reaching
// size continues at jhdr_size.
func (jw *journalWriter) wrap(off uint64) uint64 {
	if off >= jw.loc.hdr.size {
		return off%jw.loc.hdr.size + uint64(jw.loc.hdr.jhdrSize)
	}
	return off
}

// ringWrite writes data at the journal-relative offset off, continuing at
// jhdr_size after size (do_journal_io, hfs_journal.c:341-347, :423-431).
func (jw *journalWriter) ringWrite(v *Volume, off uint64, data []byte) error {
	size, jh := jw.loc.hdr.size, uint64(jw.loc.hdr.jhdrSize)
	for len(data) > 0 {
		if off >= size {
			off = jh
		}
		n := min(uint64(len(data)), size-off)
		if err := v.putAt(data[:n], int64(jw.loc.off+off)); err != nil {
			return err
		}
		data = data[n:]
		off += n
	}
	return nil
}

// encodeTxn encodes one transaction: the one-sector block list header and the
// blocks, in the order of runs. Layout, hfs_journal.h:47-124 and design section 4:
// max_blocks = blhdr_size/16 - 1, num_blocks = 1 + blocks, bytes_used = blhdr_size
// + the blocks' bytes, flags = BLHDR_CHECK_CHECKSUMS | BLHDR_FIRST_HEADER,
// binfo[0] carrying the sequence number in its last word (hfs_journal.c:4140),
// binfo[i] = (bnum = offset / jhdr_size, bsize, calc_checksum of the block), the
// unused bytes 0x5a (:2552), the header checksum over its first 32 bytes with the
// field zero (BLHDR_CHECKSUM_SIZE).
func encodeTxn(bo binary.ByteOrder, seq uint32, runs []writeRun) []byte {
	total := jwBlockSize
	for _, r := range runs {
		total += len(r.data)
	}
	out := make([]byte, total)
	hdr := out[:jwBlockSize]
	for i := range hdr {
		hdr[i] = 0x5a
	}
	bo.PutUint16(hdr[0:], uint16(jwBlockSize/16-1))
	bo.PutUint16(hdr[2:], uint16(1+len(runs)))
	bo.PutUint32(hdr[4:], uint32(total))
	bo.PutUint32(hdr[8:], 0)
	bo.PutUint32(hdr[12:], blhdrCheckChecksums|blhdrFirstHeader)
	clear(hdr[16:32])
	bo.PutUint32(hdr[28:], seq)
	pos := jwBlockSize
	for i, r := range runs {
		e := hdr[16+16*(i+1):]
		bo.PutUint64(e[0:], uint64(r.off/jwBlockSize))
		bo.PutUint32(e[8:], uint32(len(r.data)))
		bo.PutUint32(e[12:], journalChecksum(r.data))
		copy(out[pos:], r.data)
		pos += len(r.data)
	}
	bo.PutUint32(hdr[8:], journalChecksum(hdr[:blhdrChecksumSize]))
	return out
}

// headerSector returns the journal header sector as it is on the medium with
// start, end, blhdr_size and sequence_num replaced and the checksum redone
// (write_journal_header, hfs_journal.c:505-509: the checksum covers the first 44
// bytes, the sequence number is after them).
func (jw *journalWriter) headerSector(v *Volume, start, end uint64, seq uint32) ([]byte, error) {
	b := make([]byte, jw.loc.hdr.jhdrSize)
	if n, err := v.readUnder(b, int64(jw.loc.off)); n < len(b) {
		return nil, fmt.Errorf("hfsplus: reading the journal header: %v", err)
	}
	bo := jw.enc
	bo.PutUint64(b[8:], start)
	bo.PutUint64(b[16:], end)
	bo.PutUint32(b[32:], jwBlockSize)
	bo.PutUint32(b[44:], seq)
	clear(b[36:40])
	bo.PutUint32(b[36:], journalChecksum(b[:journalHeaderLen]))
	return b, nil
}

// writeHeader writes the journal header sector.
func (jw *journalWriter) writeHeader(v *Volume, start, end uint64, seq uint32) error {
	b, err := jw.headerSector(v, start, end, seq)
	if err != nil {
		return err
	}
	return v.putAt(b, int64(jw.loc.off))
}

// emptyHeaderState reads the header as it is, for the first commit of a session:
// where the (empty) journal ends and its sequence number.
func (jw *journalWriter) readHeader(v *Volume) (journalHeader, error) {
	b := make([]byte, jw.loc.hdr.jhdrSize)
	if n, err := v.readUnder(b, int64(jw.loc.off)); n < len(b) {
		return journalHeader{}, fmt.Errorf("hfsplus: reading the journal header: %v", err)
	}
	return decodeJournalHeader(b)
}

// journalRuns merges the changed runs of a commit into the blocks of a
// transaction: sorted by offset, contiguous runs joined.
func journalRuns(parts ...[]writeRun) []writeRun {
	var all []writeRun
	for _, p := range parts {
		all = append(all, p...)
	}
	slices.SortFunc(all, func(a, b writeRun) int { return int(a.off - b.off) })
	var out []writeRun
	for _, r := range all {
		if k := len(out); k > 0 && out[k-1].off+int64(len(out[k-1].data)) == r.off {
			out[k-1].data = append(slices.Clone(out[k-1].data), r.data...)
			continue
		}
		out = append(out, r)
	}
	return out
}

// journalHeaderRuns returns the changed runs of the volume header region at off as
// they will be in the transaction: lastMountedVersion 'HFSJ', unmounted bit clear
// (a mounted volume), journaled bit set. The region itself is not edited, so a
// fallback to the direct path sees it as it was.
func (v *Volume) journalHeaderRuns(off int64) ([]writeRun, error) {
	buf, err := v.region(off, 512)
	if err != nil {
		return nil, err
	}
	b := slices.Clone(buf)
	copy(b[vhLastMountedVersion:], "HFSJ")
	b[vhAttributes+2] &^= kHFSVolumeUnmountedBit >> 8
	b[vhAttributes+2] |= kHFSVolumeJournaledBit >> 8
	return changedRuns(&region{off: off, buf: b, orig: v.regions[off].orig}), nil
}

// skipZeroSeq is the writer's only way to a sequence number: 0 is the number of a
// journal from before sequence numbers existed, which replay_journal never
// rejects on sequence (hfs_journal.c:1268-1271), so a wrap skips it.
func skipZeroSeq(seq uint32) uint32 {
	if seq == 0 {
		return 1
	}
	return seq
}

// forgedSector reports whether any jhdr_size-aligned sector of any block of runs
// verifies as a block list header (verifiesAsBlockListHeader). runs are whole
// sectors (checked by the caller). The transaction's own block list header is not
// among them. Cost: one 32-byte checksum per journaled sector, no allocation.
func forgedSector(runs []writeRun) bool {
	for _, r := range runs {
		for o := 0; o+jwBlockSize <= len(r.data); o += jwBlockSize {
			if verifiesAsBlockListHeader(r.data[o : o+jwBlockSize]) {
				return true
			}
		}
	}
	return false
}

// scrubChunk is how much of the ring scrubRing reads at a time: the scan's whole
// memory, whatever the journal's size (at most 1 GiB, loadJournalAt).
const scrubChunk = 64 << 10

// scrubRing closes the gap #56/#57 left (design section 11, risk 5; hfsplus#58):
// forgedSector keeps new forged block list headers out of the ring, but a sector
// written by v0.15.0 before the guard, or by another implementation, may be there,
// and a clean unmount leaves the ring's stale sectors in place. A later
// transaction that ends right before one makes replay follow it past end.
//
// It reads the ring [jhdr_size, size) once, in scrubChunk pieces, and overwrites
// with zeroes every jhdr_size sector that verifiesAsBlockListHeader accepts (either
// byte order). When it zeroed anything, one barrier makes the zeroes durable before
// it returns, so before T_1 or any later write to the ring is issued: otherwise a
// zero write and a T_1 write to the same sector, in the same group, could land in
// either order and leave T_1 with a zeroed sector. It writes and syncs nothing when
// the ring holds no such sector.
//
// Why it is crash-safe: it runs on an empty journal (start = end), which replay
// does not read, and the zeroed sectors are stale (no live transaction exists to
// own them), so every crash image, whichever zero writes reached the medium,
// replays to the volume as it was. A sector torn by the crash holds some forged
// bytes and some zeroes and verifies only with probability 2^-32; the next session
// scrubs it again if it does.
func (jw *journalWriter) scrubRing(v *Volume) error {
	size, jh := jw.loc.hdr.size, uint64(jw.loc.hdr.jhdrSize)
	buf := make([]byte, min(scrubChunk, size-jh))
	zero := make([]byte, jh)
	zeroed := 0
	for off := jh; off < size; {
		n := min(uint64(len(buf)), size-off)
		if got, err := v.readUnder(buf[:n], int64(jw.loc.off+off)); uint64(got) < n {
			return fmt.Errorf("hfsplus: scrubbing the journal ring at %d: %v", off, err)
		}
		for o := uint64(0); o+jh <= n; o += jh {
			if !verifiesAsBlockListHeader(buf[o : o+jh]) {
				continue
			}
			if err := v.putAt(zero, int64(jw.loc.off+off+o)); err != nil {
				return err
			}
			zeroed++
		}
		off += n
	}
	if zeroed == 0 {
		return nil
	}
	return v.fsync()
}

// flushJournal is flush's journal path for a metadata-only commit at
// DurabilityStrict. It returns done = false, having written nothing, when the
// commit has to take the direct path (more than 30 runs, or it does not fit).
func (v *Volume) flushJournal(meta []writeRun) (done bool, err error) {
	jw := v.jw
	hp, err := v.journalHeaderRuns(volumeHeaderOffset)
	if err != nil {
		return false, err
	}
	ha, err := v.journalHeaderRuns(v.altOff)
	if err != nil {
		return false, err
	}
	runs := journalRuns(meta, hp, ha)
	if len(runs) == 0 || len(runs) > jwMaxRuns {
		return false, nil
	}
	// A block of the journal is whole sectors (binfo.bnum counts jhdr_size units); a
	// run that is not (a region whose logical size comes from the image, alloc.go)
	// takes the direct path.
	for _, r := range runs {
		if r.off%jwBlockSize != 0 || len(r.data)%jwBlockSize != 0 {
			return false, nil
		}
	}
	// Risk 5 (design section 11; hfsplus#56): replay follows, past end, any sector
	// that verifies as a block list header, and the blocks of this transaction are
	// bytes a user chose (file names in catalog nodes). A transaction that carries
	// such a sector takes the direct path, so a forged list never reaches the ring.
	if forgedSector(runs) {
		return false, nil
	}
	hdr := jw.loc.hdr
	size, jh := hdr.size, uint64(hdr.jhdrSize)
	tsize := uint64(jwBlockSize)
	for _, r := range runs {
		tsize += uint64(len(r.data))
	}
	// Invariant D: the live region during commit k is T_{k-2}, T_{k-1} and T_k, and
	// the journal is never perfectly full (TN1150, Journal; free_space,
	// hfs_journal.c:2382-2396).
	var live uint64
	if jw.active {
		live = jw.prevSize + jw.prev2
	}
	if live+tsize >= size-jh {
		return false, nil
	}

	pOff := int64(volumeHeaderOffset)
	var seq uint32
	var at uint64
	first := !jw.active
	if first {
		h, err := jw.readHeader(v)
		if err != nil {
			return false, err
		}
		if h.start != h.end {
			return false, fmt.Errorf("%w: the journal holds transactions the writer did not write (start %d, end %d)", ErrJournaled, h.start, h.end)
		}
		seq = skipZeroSeq(h.sequenceNum + uint32(size/jh) + 1) // hfs_journal.c:2097-2099
		at = jw.wrap(h.end)
	} else {
		seq = skipZeroSeq(jw.lastSeq + 1)
		at = jw.end
	}
	v.touched = true
	if first && !jw.scrubbed {
		// The journal is empty here (checked above), so nothing replays while the
		// scrub runs and a crash anywhere inside it leaves a volume that opens as it was.
		if err := jw.scrubRing(v); err != nil {
			return false, err
		}
		jw.scrubbed = true
	}
	txn := encodeTxn(jw.enc, seq, runs)
	if err := jw.ringWrite(v, at, txn); err != nil {
		return false, err
	}
	end := jw.wrap(at + tsize)
	if first {
		// Group 1: T_1 and the primary header in place, saying 'HFSJ' and mounted.
		old := make([]byte, 512)
		if _, err := v.readUnder(old, pOff); err != nil {
			return false, err
		}
		copy(old[vhLastMountedVersion:], "HFSJ")
		old[vhAttributes+2] &^= kHFSVolumeUnmountedBit >> 8
		old[vhAttributes+2] |= kHFSVolumeJournaledBit >> 8
		if err := v.putAt(old, pOff); err != nil {
			return false, err
		}
		if err := v.fsync(); err != nil { // barrier 1
			return false, err
		}
		if err := jw.writeHeader(v, at, end, seq); err != nil { // H_1
			return false, err
		}
		jw.hs, jw.he, jw.hseq = at, end, seq
		if err := v.fsync(); err != nil { // barrier 2
			return false, err
		}
	} else {
		// Group k: T_k (written above) and H_k = [T_{k-1}], one barrier. H_2 equals
		// H_1 and is not written again.
		if jw.hs != jw.prevStart || jw.he != jw.prevEnd || jw.hseq != jw.prevSeq {
			if err := jw.writeHeader(v, jw.prevStart, jw.prevEnd, jw.prevSeq); err != nil {
				return false, err
			}
			jw.hs, jw.he, jw.hseq = jw.prevStart, jw.prevEnd, jw.prevSeq
		}
		if err := v.fsync(); err != nil {
			return false, err
		}
	}
	// T_k's blocks in place, no barrier: the next barrier makes them durable.
	for _, r := range runs {
		if err := v.putAt(r.data, r.off); err != nil {
			return false, err
		}
	}
	if first {
		jw.prev2, jw.prevSize = 0, 0
	} else {
		jw.prev2 = jw.prevSize
	}
	jw.prevSize = tsize
	jw.prevStart, jw.prevEnd, jw.prevSeq = at, end, seq
	jw.lastSeq, jw.end, jw.active = seq, end, true
	return true, nil
}

// emptyJournal makes the journal empty again after a session's transactions:
// a barrier makes their in-place writes durable, then the header start = end =
// end(T_k) with sequence_num = seq(T_k) (journal_close, hfs_journal.c:2317-2320),
// then a barrier. The next journal commit is a first commit again. It is what the
// direct path does before it writes in place, and what Close and leaving
// DurabilityStrict do: a direct write with transactions pending would be
// overwritten by their replay.
func (v *Volume) emptyJournal() error {
	jw := v.jw
	if jw == nil || !jw.active {
		return nil
	}
	v.touched = true
	if err := v.fsync(); err != nil {
		return err
	}
	if err := jw.writeHeader(v, jw.end, jw.end, jw.lastSeq); err != nil {
		return err
	}
	if err := v.fsync(); err != nil {
		return err
	}
	jw.active = false
	v.touched = false
	return nil
}

// closeJournal is Close for a volume with transactions in its journal: empty the
// journal, then put the clean mark back in both headers (alternate first), so the
// volume is left as macOS leaves it after unmount: an empty journal, 'HFSJ', and
// the unmounted bit if it was set at open. (Design section 7 commits a T_c for
// this; PR 5 does, this PR writes the headers in place once the journal is
// empty, which is crash-safe for the same reason: nothing replays.)
func (v *Volume) closeJournal() error {
	if !v.journaling() {
		return nil
	}
	if err := v.emptyJournal(); err != nil {
		return err
	}
	if !v.cleanOnDisk {
		return nil
	}
	v.touched = true
	for _, off := range []int64{v.altOff, volumeHeaderOffset} {
		b := make([]byte, 512)
		if _, err := v.readUnder(b, off); err != nil {
			return err
		}
		b[vhAttributes+2] |= kHFSVolumeUnmountedBit >> 8
		if err := v.putAt(b, off); err != nil {
			return err
		}
	}
	err := v.fsync()
	v.touched = false
	return err
}
