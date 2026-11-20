// Copyright (c) 2026, go-filesystems
// SPDX-License-Identifier: BSD-3-Clause

package hfsplus

import (
	"encoding/binary"
	"errors"
	"fmt"
	"io"
)

// The journal replayer (docs/design/journal-write.md, section 4 "Replayer" and
// section 12 item 1). It reads a journal that holds transactions and builds, in
// memory, the blocks the kernel would write to the volume; nothing is written.
//
// It is a port of Apple's replay_journal (core/hfs_journal.c:1162-1514, tag
// hfs-715.100.10), keeping its control flow so the two agree on what a torn or
// stale journal means. fsck_hfs's replay (lib_fsck_hfs/fsck_journal.c:517-610)
// is the second reference; the differences are listed at each decision below
// and in docs/design/journal-write.md section 11, risk 2.
//
// It decodes untrusted bytes, so every size it takes from the image is bounded
// before it is used (see replayJournal).

const (
	// blhdrCheckChecksums and blhdrFirstHeader are the block list header flags
	// (hfs_journal.h:77-78).
	blhdrCheckChecksums = 0x0001
	blhdrFirstHeader    = 0x0002
	// blhdrChecksumSize is the bytes of a block list header its checksum covers:
	// the 16-byte header and binfo[0] (BLHDR_CHECKSUM_SIZE, hfs_journal.c:130).
	blhdrChecksumSize = 32
	// maxReplayRetries is the kernel's limit on restarts after a bad block list
	// (hfs_journal.c:1423).
	maxReplayRetries = 3
	// maxBlhdrSize bounds blhdr_size. macOS writes at most 96 KiB (design section
	// 4, "Sizes"); the bound only keeps one buffer small.
	maxBlhdrSize = 1 << 20
	// maxReplayBytes bounds the overlay, whatever the journal claims: 256 MiB.
	maxReplayBytes = 256 << 20
	// minJhdrSize and maxJhdrSize bound jhdr_size for the replayer: it is the unit
	// of bnum and of every block (design section 4). Apple refuses only zero.
	minJhdrSize = 512
	maxJhdrSize = 1 << 16
)

// ErrJournalReplay is returned when a journal that holds transactions cannot be
// replayed: the kernel would abandon it (hfs_journal.c, bad_replay), or a size
// in it is outside the bounds the replayer accepts. It wraps ErrJournaled.
var ErrJournalReplay = fmt.Errorf("%w: journal cannot be replayed", ErrJournaled)

// ErrReplayBudget is returned when the blocks a journal would replay exceed this
// driver's own resource bound, min(journal size, 256 MiB). The kernel has no such
// bound (it streams blocks to the device); it protects the memory of a process
// that replays an untrusted journal into an overlay. It wraps ErrJournalReplay.
var ErrReplayBudget = fmt.Errorf("%w: replayed blocks exceed the replay memory budget", ErrJournalReplay)

// blhdrSum is calc_checksum of a block list header as replay_journal computes it
// (hfs_journal.c:1225-1235): the checksum field, bytes 8..12, zeroed, over the
// first BLHDR_CHECKSUM_SIZE bytes. hb is not modified. It is the one
// implementation: the replayer compares it with the stored field, and the writer's
// guard (verifiesAsBlockListHeader) with the stored field in both byte orders.
func blhdrSum(hb []byte) uint32 {
	var f [blhdrChecksumSize]byte
	copy(f[:], hb)
	clear(f[8:12])
	return journalChecksum(f[:])
}

// verifiesAsBlockListHeader reports whether a sector would pass the replayer's
// checksum test as a block list header in either byte order of the journal (the
// order is the journal's, unknown to whoever planted the bytes). Past end, replay
// follows such a sector (design section 11, risk 5).
func verifiesAsBlockListHeader(sector []byte) bool {
	if len(sector) < blhdrChecksumSize { // also keeps sector[8:12] in range
		return false
	}
	sum := blhdrSum(sector)
	return binary.BigEndian.Uint32(sector[8:12]) == sum || binary.LittleEndian.Uint32(sector[8:12]) == sum
}

func replayErrorf(format string, a ...any) error {
	return fmt.Errorf("%w: "+format, append([]any{ErrJournalReplay}, a...)...)
}

// replayOverlay holds the replayed blocks, one jhdr_size unit each.
type replayOverlay struct {
	unit  uint64
	units map[uint64][]byte // unit index (volume offset / unit) -> its bytes
	// end is where the replay stopped: the kernel's jhdr->start == jhdr->end after
	// replay_journal (hfs_journal.c:1453-1456), the offset the journal header is
	// rewritten with when the replay is written back (replay_write.go).
	end uint64
}

func (o *replayOverlay) add(fsOff uint64, data []byte) {
	for i := uint64(0); i*o.unit < uint64(len(data)); i++ {
		o.units[fsOff/o.unit+i] = data[i*o.unit : (i+1)*o.unit : (i+1)*o.unit]
	}
}

// bytes is the memory the overlay holds.
func (o *replayOverlay) bytes() uint64 { return uint64(len(o.units)) * o.unit }

// replayReader is the replayed view of a volume: base with the overlay on top.
type replayReader struct {
	base io.ReaderAt
	size int64
	ov   *replayOverlay
}

func (rr *replayReader) ReadAt(p []byte, off int64) (int, error) {
	if off < 0 {
		return 0, errors.New("hfsplus: negative offset")
	}
	n, err := rr.base.ReadAt(p, off)
	if err != nil && err != io.EOF {
		return n, err
	}
	if len(p) == 0 {
		return n, err
	}
	unit := int64(rr.ov.unit)
	end := off + int64(len(p))
	for u := off / unit; u*unit < end; u++ {
		d, ok := rr.ov.units[uint64(u)]
		if !ok {
			continue
		}
		lo, hi := max(u*unit, off), min((u+1)*unit, end)
		copy(p[lo-off:hi-off], d[lo-u*unit:hi-u*unit])
		n = max(n, int(hi-off))
	}
	if n < len(p) {
		return n, io.EOF
	}
	return n, nil
}

type replayEntry struct {
	fsOff uint64
	data  []byte
}

type attemptResult int

const (
	attemptDone attemptResult = iota
	attemptRestart
	attemptAbort
)

// replayer is the state of replay_journal (its locals, hfs_journal.c:1164-1174).
type replayer struct {
	checkBlocks bool // check_block_checksums, sticky (:1323-1325)
	r           io.ReaderAt
	base        uint64 // offset of the journal in the image
	h           journalHeader
	volSize     uint64
	ring        uint64 // bytes of the circular buffer [jhdr_size, size)
	budget      uint64 // most bytes of block data one replay may hold

	start, end uint64 // jhdr->start and jhdr->end, which replay_journal rewrites
	origStart  uint64
	txnStart   uint64 // txn_start_offset
	checkPast  bool   // check_past_jnl_end
	uncharted  bool   // in_uncharted_territory
	retries    int
}

// replayJournal replays the journal at base (its size and header in h) over a
// volume of volSize bytes, into an overlay.
//
// Bounds, all checked before use: jhdr_size in [512, 64 KiB] and size a multiple
// of it; blhdr_size a positive multiple of jhdr_size, at most 1 MiB and at most
// the buffer; start and end inside [jhdr_size, size]; per block list,
// max_blocks <= size/jhdr_size, 0 < num_blocks <= max_blocks, num_blocks
// entries inside blhdr_size, blhdr_size <= bytes_used <= the buffer; per
// block, bsize a non-zero multiple of jhdr_size within the buffer and
// bnum*jhdr_size + bsize inside the volume (bnum is checked against
// volSize/jhdr_size first, so the product cannot overflow on 32 or 64 bits);
// the bytes_used of all lists replayed in one pass at most the buffer (a pass
// that would lap the buffer stops, or is refused if still before end); at most
// size/jhdr_size lists per pass; and the block data held at most
// min(buffer, 256 MiB). So the overlay never exceeds the journal's size, and
// every loop ends.
func replayJournal(r io.ReaderAt, base uint64, h journalHeader, volSize uint64) (*replayOverlay, error) {
	jh := uint64(h.jhdrSize)
	if jh < minJhdrSize || jh > maxJhdrSize || h.size%jh != 0 || h.size < 2*jh {
		return nil, replayErrorf("jhdr_size %d, size %d", h.jhdrSize, h.size)
	}
	// journal_open (hfs_journal.c:1910-1922) requires size >= phys_blksz *
	// (phys_blksz / sizeof(block_info)) and size % phys_blksz == 0, where
	// phys_blksz is the DEVICE's sector size (:1900). The replayer does not know
	// the device, so it uses jhdr_size as phys_blksz (design section 4: they are
	// equal on a journal macOS wrote, and do_journal_io addresses the journal in
	// jhdr_size units, :386). A stand-in, not the kernel's test.
	if h.size < jh*(jh/16) {
		return nil, replayErrorf("size %d below jhdr_size*(jhdr_size/16)", h.size)
	}
	bl := uint64(h.blhdrSize)
	ring := h.size - jh
	if bl == 0 || bl%jh != 0 || bl > maxBlhdrSize || bl > ring {
		return nil, replayErrorf("blhdr_size %d", h.blhdrSize)
	}
	p := &replayer{r: r, base: base, h: h, volSize: volSize, ring: ring,
		budget: min(ring, maxReplayBytes), start: h.start, end: h.end, checkPast: true}
	// hfs_journal.c:1177-1182: a start or end at the very end wraps to the first
	// block after the header.
	if p.start == h.size {
		p.start = jh
	}
	if p.end == h.size {
		p.end = jh
	}
	if p.start < jh || p.start > h.size || p.end < jh || p.end > h.size {
		return nil, replayErrorf("start %d, end %d outside the journal buffer", h.start, h.end)
	}
	ov := &replayOverlay{unit: jh, units: map[uint64][]byte{}}
	if p.start == p.end { // :1184-1186
		ov.end = p.start
		return ov, nil
	}
	p.origStart = p.start
	for {
		entries, res, err := p.attempt()
		switch res {
		case attemptAbort:
			return nil, err
		case attemptRestart:
			continue
		}
		// The coalescing of add_block (:1134-1159) keeps, for each block, the data of
		// the latest list that wrote it; applying the entries in order does the same.
		for _, e := range entries {
			ov.add(e.fsOff, e.data)
		}
		ov.end = p.start // == p.end, :1453-1456
		return ov, nil
	}
}

// read is read_journal_data (do_journal_io, :313-434) over the circular buffer:
// n bytes from *off, continuing at jhdr_size after size. It reports whether the
// bytes could be read.
func (p *replayer) read(off *uint64, n uint64) ([]byte, bool) {
	buf := make([]byte, n)
	jh, size := uint64(p.h.jhdrSize), p.h.size
	for done := uint64(0); done < n; {
		if *off >= size { // :341-343, :427-429
			*off = jh
		}
		chunk := min(n-done, size-*off)
		if got, _ := p.r.ReadAt(buf[done:done+chunk], int64(p.base+*off)); uint64(got) < chunk {
			return nil, false
		}
		done += chunk
		*off += chunk
	}
	return buf, true
}

// attempt is one pass of replay_journal from restart_replay (:1199) to the
// replay of the collected blocks, which it returns instead of applying.
func (p *replayer) attempt() ([]replayEntry, attemptResult, error) {
	h := p.h
	jh, size := uint64(h.jhdrSize), h.size
	bl := uint64(h.blhdrSize)
	var (
		entries      []replayEntry
		lastSeq      uint32
		consumed     uint64
		pendingBytes uint64
		lists        uint64
	)
	maxLists := size / jh
	// bad is bad_txn_handling (:1414-1439): drop what the pass collected and
	// replay again only up to the start of the transaction that was being read.
	bad := func() ([]replayEntry, attemptResult, error) {
		if p.txnStart == 0 { // :1417-1420
			return nil, attemptAbort, replayErrorf("no known good transaction start")
		}
		if p.retries == maxReplayRetries { // :1423-1426
			return nil, attemptAbort, replayErrorf("repeated errors")
		}
		p.retries++
		p.start, p.end, p.checkPast = p.origStart, p.txnStart, false
		return nil, attemptRestart, nil
	}
	// stopPast ends the pass quietly where the kernel's checks would.
	stopPast := func() { p.checkPast = false; p.end = p.start }

	for p.checkPast || p.start != p.end { // :1211
		lists++
		if lists > maxLists {
			if p.uncharted {
				stopPast()
				continue
			}
			return bad()
		}
		blOff := p.start
		off := p.start
		hb, ok := p.read(&off, bl) // :1215-1220
		if !ok {
			return bad()
		}
		bo := h.bo
		orig := bo.Uint32(hb[8:12]) // :1225-1235
		checksum := blhdrSum(hb)
		if checksum != orig {
			if p.checkPast && p.uncharted { // :1245-1254: a torn list past end
				p.checkPast = false
				p.end = blOff
				continue
			}
			if blOff == p.origStart { // :1259-1262: nothing in the journal at all
				return nil, attemptAbort, replayErrorf("bad block list header at the start of the journal")
			}
			return bad()
		}
		maxBlocks, numBlocks := uint64(bo.Uint16(hb[0:2])), uint64(bo.Uint16(hb[2:4]))
		bytesUsed := int64(int32(bo.Uint32(hb[4:8])))
		flags := bo.Uint32(hb[12:16])
		seq := bo.Uint32(hb[28:32]) // binfo[0] third word, "next" in TN1150 (:4140)

		// :1268-1287. fsck applies this only past end (fsck_journal.c:581-602); the
		// kernel applies it to every list, as here.
		if lastSeq != 0 && seq != 0 && seq != lastSeq && seq != lastSeq+1 {
			p.txnStart, p.end = blOff, blOff
			if p.checkPast {
				p.checkPast = false
				continue
			}
			return bad()
		}
		lastSeq = seq

		if blOff >= p.end && p.start <= p.end && lastSeq == 0 { // :1289-1298
			p.checkPast = false
			p.start = p.end
			continue
		}

		// :1302-1308, plus the bounds this reader needs: the entries lie inside
		// blhdr_size, and bytes_used covers the header and stays inside the buffer.
		if maxBlocks == 0 || maxBlocks > size/jh || numBlocks == 0 || numBlocks > maxBlocks {
			return bad()
		}
		// Ours, not the kernel's: the entries must lie inside the list header we
		// read, and bytes_used must cover it and fit the buffer (the kernel adds
		// bytes_used to start unchecked, :1442). Past end the list is not part of
		// the journal, so, like a torn list there (:1245-1254), it just ends the read.
		if numBlocks > bl/16-1 || bytesUsed < int64(bl) || uint64(bytesUsed) > p.ring {
			if p.uncharted {
				stopPast()
				continue
			}
			return bad()
		}
		if consumed+uint64(bytesUsed) > p.ring { // the pass would lap the buffer
			if p.uncharted {
				stopPast()
				continue
			}
			return bad()
		}
		for i := uint64(1); i < numBlocks; i++ { // :1311-1321
			bnum := int64(bo.Uint64(hb[16+16*i:]))
			if bnum < 0 && bnum != -1 {
				return bad()
			}
		}
		if flags&blhdrFirstHeader != 0 { // :1330-1332
			p.txnStart = blOff
		}
		// :1323-1325: once any list sets BLHDR_CHECK_CHECKSUMS the kernel checks the
		// blocks of every later list too (check_block_checksums is never cleared,
		// not even at restart_replay).
		if flags&blhdrCheckChecksums != 0 {
			p.checkBlocks = true
		}
		check := p.checkBlocks
		for i := uint64(1); i < numBlocks; i++ { // :1337-1406
			e := hb[16+16*i:]
			bnum := int64(bo.Uint64(e[0:8]))
			bsize := uint64(bo.Uint32(e[8:12]))
			cksum := bo.Uint32(e[12:16])
			if bsize == 0 { // :1344-1348
				return bad()
			}
			// The kernel has no such bounds: it would fail at update_fs_block (:826,
			// bad_replay) or read garbage. Here they refuse the whole journal.
			if bsize%jh != 0 || bsize > p.ring {
				return nil, attemptAbort, replayErrorf("block size %d", bsize)
			}
			if bnum != -1 { // a killed block is skipped (:1350-1352)
				if uint64(bnum) > p.volSize/jh || uint64(bnum)*jh+bsize > p.volSize {
					return nil, attemptAbort, replayErrorf("block %d (%d bytes) outside the volume", bnum, bsize)
				}
				if pendingBytes+bsize > p.budget {
					// Our own bound, not the kernel's (see ErrReplayBudget).
					return nil, attemptAbort, fmt.Errorf("%w (%d bytes)", ErrReplayBudget, p.budget)
				}
				boff := off
				data, ok := p.read(&boff, bsize) // :1362-1367
				if !ok {
					return bad()
				}
				if check && cksum != 0 && journalChecksum(data) != cksum { // :1373-1382
					return bad()
				}
				pendingBytes += bsize
				entries = append(entries, replayEntry{fsOff: uint64(bnum) * jh, data: data})
			}
			off += bsize // :1397-1405
			if off >= size {
				off = jh + (off - size)
			}
		}
		p.start += uint64(bytesUsed) // :1442-1446
		if p.start >= size {
			p.start = p.start%size + jh
		}
		consumed += uint64(bytesUsed)
		if p.start == p.end { // :1448-1450
			p.uncharted = true
		}
	}
	if p.start != p.end { // :1453-1456
		p.end = p.start
	}
	return entries, attemptDone, nil
}

// replayedView returns the view of the volume with its journal replayed, or rs
// itself when the volume is not journaled or its journal is empty. replayed
// reports whether a journal was replayed.
func replayedView(rs io.ReaderAt, size int64, vh *volumeHeader) (view io.ReaderAt, replayed bool, err error) {
	if vh.Attributes&kHFSVolumeJournaledBit == 0 {
		return rs, false, nil
	}
	if size < 0 {
		return nil, false, fmt.Errorf("%w: replaying a journal needs the volume size", ErrJournaled)
	}
	st, loc, err := loadJournalAt(rs, size, vh)
	if err != nil {
		return nil, false, err
	}
	if st.empty {
		return rs, false, nil
	}
	ov, err := replayJournal(rs, loc.off, loc.hdr, uint64(size))
	if err != nil {
		return nil, false, err
	}
	return &replayReader{base: rs, size: size, ov: ov}, true, nil
}
