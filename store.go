// Copyright (c) 2026, go-filesystems
// SPDX-License-Identifier: BSD-3-Clause

package hfsplus

import (
	"bytes"
	"errors"
	"fmt"
	"io"
	"sort"
)

// store.go is the writable volume's I/O layer. A writable volume holds NO copy
// of the image: it reads the headers, B-tree nodes, bitmap blocks and file data
// on demand from the underlying io.ReaderAt (through a small bounded read
// cache), and keeps the structures a transaction edits in "regions" -- the
// B-tree nodes, bitmap blocks and the two volume headers it touches, each a
// private copy loaded on first use. At commit only the 512-byte sectors of a
// region that differ from what was loaded are written back, in the order
// Apple TN1150 asks for (see flush). File data is written straight to the
// store at the time it is produced, into blocks no metadata references yet.
//
// Memory is therefore bounded by what one transaction touches
// (maxRegionBytes) plus the read cache (readCachePages * readCachePage), never
// by the image size.

const (
	// sectorSize is the unit a region is diffed in at flush.
	sectorSize = 512
	// readCachePage / readCachePages / readCacheMaxRead bound the read cache: 2048
	// pages of 4 KiB (8 MiB), used only for reads of at most 16 KiB (B-tree nodes,
	// headers, bitmap blocks); file data bypasses it.
	readCachePage    = 4096
	readCachePages   = 2048
	readCacheMaxRead = 16 << 10
	// maxRegionBytes bounds the metadata one transaction may hold (a bound on
	// the work, not on anything an image field claims): 64 MiB.
	maxRegionBytes = 64 << 20
	// maxRegions bounds how many regions one transaction may hold.
	maxRegions = 1 << 16
)

// errTxnTooLarge is returned when a transaction would hold more metadata than
// maxRegionBytes / maxRegions.
var errTxnTooLarge = errors.New("hfsplus: transaction touches too much metadata")

// region is a transaction-private copy of [off, off+len(buf)) of the image. orig
// is the content it was loaded with, so flush can tell which sectors changed.
type region struct {
	off  int64
	buf  []byte
	orig []byte
}

// writeRun is one contiguous range to write at flush.
type writeRun struct {
	off  int64
	data []byte
}

// overlayReader is the io.ReaderAt a writable volume reads through: the
// underlying image with the transaction's regions laid over it.
type overlayReader struct{ v *Volume }

func (o overlayReader) ReadAt(p []byte, off int64) (int, error) {
	v := o.v
	n, err := v.readUnder(p, off)
	if err != nil && err != io.EOF {
		return n, err
	}
	end := off + int64(len(p))
	for _, r := range v.regions {
		rEnd := r.off + int64(len(r.buf))
		if r.off >= end || rEnd <= off {
			continue
		}
		lo, hi := max(r.off, off), min(rEnd, end)
		copy(p[lo-off:hi-off], r.buf[lo-r.off:hi-r.off])
		if int(hi-off) > n {
			n = int(hi - off)
		}
	}
	if n < len(p) {
		return n, io.EOF
	}
	return n, nil
}

// readUnder reads from the underlying image, through the bounded read cache for
// small reads.
func (v *Volume) readUnder(p []byte, off int64) (int, error) {
	if len(p) == 0 {
		return 0, nil
	}
	if !v.cacheOn || len(p) > readCacheMaxRead {
		return v.under.ReadAt(p, off)
	}
	v.rcMu.Lock()
	defer v.rcMu.Unlock()
	total := 0
	for total < len(p) {
		pos := off + int64(total)
		pi := pos / readCachePage
		page, ok := v.rcache[pi]
		if !ok {
			buf := make([]byte, readCachePage)
			n, err := v.under.ReadAt(buf, pi*readCachePage)
			if err != nil && err != io.EOF {
				return total, err
			}
			page = buf[:n]
			if n > 0 {
				if len(v.rcache) >= readCachePages {
					for k := range v.rcache {
						delete(v.rcache, k)
						break
					}
				}
				v.rcache[pi] = page
			}
		}
		in := int(pos - pi*readCachePage)
		if in >= len(page) {
			return total, io.EOF
		}
		total += copy(p[total:], page[in:])
	}
	return total, nil
}

// dropReadCache forgets every cached page (after anything was written).
func (v *Volume) dropReadCache() {
	if !v.cacheOn {
		return
	}
	v.rcMu.Lock()
	clear(v.rcache)
	v.rcMu.Unlock()
}

// region returns the live, editable bytes of [off, off+n) of the image, loading
// them on first use. The same (off, n) returns the same slice for the rest of the
// transaction, and reads through v.rs see edits made to it. A range that
// overlaps a different region is refused: two structures of an image that claim
// the same bytes are a corrupt image, not something to merge.
func (v *Volume) region(off int64, n int) ([]byte, error) {
	if r, ok := v.regions[off]; ok {
		if len(r.buf) != n {
			return nil, fmt.Errorf("%w: bytes at %d used as two different structures", ErrCorrupt, off)
		}
		return r.buf, nil
	}
	if n <= 0 || off < 0 || off > v.size-int64(n) {
		return nil, fmt.Errorf("%w: range [%d,+%d) is outside the %d-byte image", ErrCorrupt, off, n, v.size)
	}
	for _, r := range v.regions {
		if r.off < off+int64(n) && off < r.off+int64(len(r.buf)) {
			return nil, fmt.Errorf("%w: bytes at %d overlap another structure", ErrCorrupt, off)
		}
	}
	if len(v.regions) >= maxRegions || v.regionBytes+int64(n) > maxRegionBytes {
		return nil, errTxnTooLarge
	}
	buf := make([]byte, n)
	if got, err := v.readUnder(buf, off); got < n {
		if err == nil || err == io.EOF {
			err = io.ErrUnexpectedEOF
		}
		return nil, fmt.Errorf("hfsplus: read [%d,+%d): %w", off, n, err)
	}
	if v.regions == nil {
		v.regions = make(map[int64]*region)
	}
	v.regions[off] = &region{off: off, buf: buf, orig: bytes.Clone(buf)}
	v.regionBytes += int64(n)
	return buf, nil
}

// noteErr records the first error of an accessor that cannot return one, so
// commit fails with it. A range outside the image (ErrCorrupt) is not recorded:
// the accessors that return nil for it let their callers report it.
func (v *Volume) noteErr(err error) {
	if v.werr == nil && !errors.Is(err, ErrCorrupt) {
		v.werr = err
	}
}

// hdr returns the live bytes of the primary (alt false) or alternate volume
// header. On failure the error is recorded and a scratch buffer comes back, so a
// setter never panics; commit then refuses.
func (v *Volume) hdr(alt bool) []byte {
	off := int64(volumeHeaderOffset)
	if alt {
		off = v.altOff
	}
	b, err := v.region(off, 512)
	if err != nil {
		if v.werr == nil {
			v.werr = err
		}
		return make([]byte, 512)
	}
	return b
}

// discard drops every region and the read cache: the transaction is abandoned.
func (v *Volume) discard() {
	v.regions = nil
	v.regionBytes = 0
	v.werr = nil
	v.dropReadCache()
}

// writeData writes file data (or the zero tail of its last block) straight to the
// store. It is only ever aimed at allocation blocks the transaction just took
// from the bitmap, so nothing on disk references them until the metadata that
// does is flushed -- which is why it may go first, and need not wait for commit.
func (v *Volume) writeData(p []byte, off int64) error {
	if len(p) == 0 {
		return nil
	}
	if off < 0 || off > v.size-int64(len(p)) {
		return fmt.Errorf("%w: data range [%d,+%d) is outside the %d-byte image", ErrCorrupt, off, len(p), v.size)
	}
	if err := v.putAt(p, off); err != nil {
		return err
	}
	v.dataDirty = true
	return nil
}

// putAt writes p at off to the in-memory mirror (if any) and the backing writer
// (if any), and forgets what was cached.
func (v *Volume) putAt(p []byte, off int64) error {
	if v.img != nil {
		copy(v.img[off:], p)
	}
	v.dropReadCache()
	if v.wa != nil {
		if _, err := v.wa.WriteAt(p, off); err != nil {
			return fmt.Errorf("hfsplus: sync: %w", err)
		}
	}
	return nil
}

// fsync makes what was written durable, when the backing writer can.
func (v *Volume) fsync() error {
	if v.wa == nil {
		return nil
	}
	if s, ok := v.wa.(interface{ Sync() error }); ok {
		if err := s.Sync(); err != nil {
			return fmt.Errorf("hfsplus: sync: %w", err)
		}
	}
	return nil
}

// vhAttributes is the offset of the attributes word in the volume header.
// kHFSVolumeUnmountedBit (bit 8, TN1150 "Volume Attributes") is set when the
// volume was cleanly unmounted.
const vhAttributes = 4

// changedRuns returns the sectors of r that differ from what it was loaded with,
// merged into contiguous runs.
func changedRuns(r *region) []writeRun {
	var runs []writeRun
	n := len(r.buf)
	for s := 0; s < n; s += sectorSize {
		e := min(s+sectorSize, n)
		if bytes.Equal(r.buf[s:e], r.orig[s:e]) {
			continue
		}
		if k := len(runs); k > 0 && runs[k-1].off+int64(len(runs[k-1].data)) == r.off+int64(s) {
			runs[k-1].data = r.buf[int(runs[k-1].off-r.off):e]
			continue
		}
		runs = append(runs, writeRun{off: r.off + int64(s), data: r.buf[s:e]})
	}
	return runs
}

// flush writes what the transaction changed, in the order TN1150 relies on: file
// data (already written by writeData, into blocks nothing references yet) is made
// durable first, then the B-tree and bitmap sectors, then the alternate volume
// header and the primary header last. The guarantee is exactly this: three
// fsyncs separate (data + cleared bit), (B-tree and bitmap sectors) and (both
// headers), so no later group reaches the disk before an earlier one is durable;
// inside the last group the alternate is written before the primary but nothing
// orders them on the disk. The B-tree and bitmap sectors are edited in place, so
// there is no atomic commit point: a crash after the first of them leaves a
// volume that may be inconsistent, which is what the cleared unmounted bit
// declares. Only changed 512-byte sectors are written: O(change).
//
// When the volume was cleanly unmounted at open, the primary header is first
// rewritten with kHFSVolumeUnmountedBit cleared and fsynced, and the final
// primary header sets it again: a crash anywhere between leaves the bit clear, so
// the next mount runs fsck instead of trusting a half-written volume.
func (v *Volume) flush() error {
	if v.werr != nil {
		return v.werr
	}
	var meta []writeRun
	var hdrP, hdrA []writeRun
	pOff, aOff := int64(volumeHeaderOffset), v.altOff
	anyChange := false
	offs := make([]int64, 0, len(v.regions))
	for off := range v.regions {
		offs = append(offs, off)
	}
	sort.Slice(offs, func(i, j int) bool { return offs[i] < offs[j] })
	for _, off := range offs {
		r := v.regions[off]
		runs := changedRuns(r)
		if len(runs) > 0 {
			anyChange = true
		}
		switch {
		case r.off == pOff && len(r.buf) == 512:
			hdrP = runs
		case r.off == aOff && len(r.buf) == 512:
			hdrA = runs
		default:
			meta = append(meta, runs...)
		}
	}
	if !anyChange && !v.dataDirty {
		return nil
	}
	old := make([]byte, 512)
	if _, err := v.readUnder(old, pOff); err != nil && err != io.EOF {
		return err
	}
	// A journaled volume is not checked on the strength of the unmounted bit
	// alone: macOS's quick check (fsck_hfs -q) reports a journaled volume clean
	// whatever that bit says, trusting the journal to bring it back. This driver
	// does not journal, so for the span of the commit the volume says it is not
	// journaled, whether or not it was cleanly unmounted at open (a journaled
	// volume left dirty with an empty journal is a real state); the final header,
	// written last, restores the bit. (The journal was empty at open, see
	// checkJournal, and is never written.)
	journaled := old[vhAttributes+2]&(kHFSVolumeJournaledBit>>8) != 0
	if v.cleanOnDisk || journaled {
		// The headers are rewritten whole, whatever changed: the final ones carry
		// the unmounted bit (if the volume had it) and the journaled bit (if it had
		// that), which the bracket write below takes away.
		for _, h := range []struct {
			off int64
			dst *[]writeRun
		}{{pOff, &hdrP}, {aOff, &hdrA}} {
			b, err := v.region(h.off, 512)
			if err != nil {
				return err
			}
			if v.cleanOnDisk {
				b[vhAttributes+2] |= kHFSVolumeUnmountedBit >> 8
			}
			*h.dst = []writeRun{{off: h.off, data: b}}
		}
		// Make the crash visible before anything the old header references moves.
		old[vhAttributes+2] &^= kHFSVolumeUnmountedBit >> 8
		old[vhAttributes+2] &^= kHFSVolumeJournaledBit >> 8
		v.touched = true
		if err := v.putAt(old, pOff); err != nil {
			return err
		}
		v.dataDirty = true // fsync below covers it
	}
	v.touched = true // from here on a failure leaves the disk in an unknown state
	if v.dataDirty {
		if err := v.fsync(); err != nil {
			return err
		}
	}
	// The alternate header is only a fallback copy the OS reads when the primary is
	// unusable, so it is written just before the primary, with no barrier between
	// them: a crash may leave the two copies apart (either one new), and fsck
	// repairs that. Everything else is ordered by an fsync: data and the cleared
	// bit are durable before any B-tree or bitmap sector moves, and those are
	// durable before either header is rewritten. (A fourth fsync between the
	// alternate and the primary would order those two as well; it is not taken,
	// for its latency -- see hfsplus#35.)
	for _, phase := range [][]writeRun{meta, append(hdrA, hdrP...)} {
		if len(phase) == 0 {
			continue
		}
		for _, run := range phase {
			if err := v.putAt(run.data, run.off); err != nil {
				return err
			}
		}
		if err := v.fsync(); err != nil {
			return err
		}
	}
	v.regions = nil
	v.regionBytes = 0
	v.touched = false
	v.dataDirty = false
	v.dropReadCache()
	return nil
}
