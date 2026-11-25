// Copyright (c) 2026, go-filesystems
// SPDX-License-Identifier: BSD-3-Clause

package hfsplus

import (
	"encoding/binary"
	"errors"
	"fmt"
	"io"
)

// The journal, from Apple TN1150 "HFS Plus Volume Format", sections "Journal",
// "Journal Info Block" and "Journal Checksums".
//
// This driver does not write transactions. A volume whose journal is empty
// (start == end) is safe to write directly: macOS's replay at mount finds
// nothing to replay, so it cannot overwrite what was written. A volume whose
// journal holds transactions is not: replay would copy the journalled blocks
// over the driver's. OpenWritable therefore refuses such a volume with
// ErrJournaled; Open (read-only) is unaffected, and Open with WithJournalReplay
// serves the volume as macOS would see it after replaying the journal (replay.go).

const (
	kHFSVolumeJournaledBit = 1 << 13 // volume header attributes, bit 13

	kJIJournalInFSMask          = 0x00000001
	kJIJournalOnOtherDeviceMask = 0x00000002
	kJIJournalNeedInitMask      = 0x00000004

	journalHeaderMagic = 0x4a4e4c78
	journalEndianMagic = 0x12345678
	// journalOldHeaderMagic is 'JHDR', the magic of the journal header before 'JNLx'.
	journalOldHeaderMagic = 0x4a484452

	// journalInfoBlockLen is the 4 + 32 + 8 + 8 + 128 bytes of JournalInfoBlock.
	journalInfoBlockLen = 180
	// journalHeaderLen is the bytes of journal_header the checks read (through
	// jhdr_size); the on-disk header fills one sector.
	journalHeaderLen = 44
	// maxJournalHeaderBytes is how much of the journal is read to decode the
	// header (one sector; TN1150: "2048 on many types of optical media"). It does
	// not bound jhdr_size: the decoder reads only the first journalHeaderLen bytes.
	maxJournalHeaderBytes = 4096
)

// ErrJournaled is returned by OpenWritable / OpenFileWritable for a journaled
// volume whose journal is not provably empty: it holds transactions macOS would
// replay over the driver's writes, or its journal cannot be read, is on another
// device, or fails its checksum. Open it read-only, or let macOS replay it
// (mount and unmount the volume) first.
var ErrJournaled = errors.New("hfsplus: journaled volume with a journal that is not empty")

// journalState is what checkJournal learned about a volume.
type journalState struct {
	journaled bool
	empty     bool // journaled and start == end, or never initialised
}

// readJournalHeader decodes the journal_header at h (TN1150 lays it out; the
// fields are in the byte order of the host that wrote it, which the endian
// field tells). It returns the start, end, size and jhdr_size fields after
// verifying magic, endian and checksum.
func readJournalHeader(h []byte) (start, end, size uint64, jhdrSize uint32, err error) {
	jh, err := decodeJournalHeader(h)
	if err != nil {
		return 0, 0, 0, 0, err
	}
	return jh.start, jh.end, jh.size, jh.jhdrSize, nil
}

// journalHeader is every field of journal_header (hfs_journal.h:111-124),
// decoded in the byte order the header states.
type journalHeader struct {
	bo          binary.ByteOrder
	start, end  uint64
	size        uint64
	blhdrSize   uint32
	jhdrSize    uint32
	sequenceNum uint32 // 0 when h held fewer than 48 bytes
}

// decodeJournalHeader is readJournalHeader with all the fields: magic, endian,
// start, end, size, blhdr_size, checksum, jhdr_size and sequence_num.
func decodeJournalHeader(h []byte) (journalHeader, error) {
	if len(h) < journalHeaderLen {
		return journalHeader{}, fmt.Errorf("%w: journal header is truncated", ErrJournaled)
	}
	// The magic alone says the byte order and the era of the header, and the endian
	// field is never read (hfsplus#48). Apple's journal_open takes 'JNLx' as found
	// or byte-swapped and swaps the header for the swapped one only (core/hfs_journal.c
	// :1969); 'JHDR' is accepted only as found (a swapped 'JHDR' fails at :1979).
	// The host that wrote the volume is a little-endian Mac, so 'JHDR' is read
	// little-endian only.
	var bo binary.ByteOrder
	var legacy bool
	switch m := h[0:4]; {
	case binary.BigEndian.Uint32(m) == journalHeaderMagic:
		bo = binary.BigEndian
	case binary.LittleEndian.Uint32(m) == journalHeaderMagic:
		bo = binary.LittleEndian
	case binary.LittleEndian.Uint32(m) == journalOldHeaderMagic:
		bo, legacy = binary.LittleEndian, true
	default:
		return journalHeader{}, fmt.Errorf("%w: journal header magic is not valid", ErrJournaled)
	}
	jhdrSize := bo.Uint32(h[40:44])
	// Apple's journal_open refuses only a zero jhdr_size (core/hfs_journal.c:1961,
	// hfs-715.100.10), so no sector-size rule is applied here. A jhdr_size below
	// the 44-byte struct this reader decodes is refused for that reason alone (it
	// cannot be a header we can read); the upper bound (jhdr_size <= size) is
	// checked in checkJournalAt, where the size is known.
	if jhdrSize < journalHeaderLen {
		return journalHeader{}, fmt.Errorf("%w: journal header size %d", ErrJournaled, jhdrSize)
	}
	// TN1150: zero the checksum field, then checksum the header. The header it
	// means is the journal_header struct (44 bytes, through jhdr_size), not the
	// whole jhdr_size-byte sector: a header written by macOS 27.2 verifies only
	// that way (see macJournalHeader in journal_test.go).
	// Apple does not enforce the 'JNLx' checksum either: the code that compares it
	// is there, but the goto that would refuse is commented out
	// (hfs_journal.c:1988-1993). This reader keeps the check for 'JNLx', which is
	// stricter than Apple's; a 'JHDR' header predates the checksum, so none is
	// checked for it.
	if !legacy {
		var buf [journalHeaderLen]byte
		copy(buf[:], h)
		copy(buf[36:40], []byte{0, 0, 0, 0})
		if got, want := journalChecksum(buf[:]), bo.Uint32(h[36:40]); got != want {
			return journalHeader{}, fmt.Errorf("%w: journal header checksum %#x, want %#x", ErrJournaled, want, got)
		}
	}
	jh := journalHeader{
		bo: bo, start: bo.Uint64(h[8:16]), end: bo.Uint64(h[16:24]), size: bo.Uint64(h[24:32]),
		blhdrSize: bo.Uint32(h[32:36]), jhdrSize: jhdrSize,
	}
	if len(h) >= journalHeaderLen+4 {
		jh.sequenceNum = bo.Uint32(h[44:48])
	}
	return jh, nil
}

// journalChecksum is TN1150's calc_checksum.
func journalChecksum(b []byte) uint32 {
	var c int32
	for _, x := range b {
		c = (c << 8) ^ (c + int32(x))
	}
	return uint32(^c)
}

// journalPlacement refuses a journal whose ring [off, off+size) covers the primary
// volume header, the alternate one, or the journal info block: the writes of a
// replay or of the journal writer would overwrite them (hfsplus#48 review). Apple
// does not check it, and a reader never writes the ring, so only the writable
// opens call it.
func journalPlacement(loc journalLoc, vh *volumeHeader) error {
	lo, hi := loc.off, loc.off+loc.hdr.size
	bs := uint64(vh.BlockSize)
	volEnd := uint64(vh.TotalBlocks) * bs
	jib := uint64(vh.JournalInfoBlock) * bs
	for _, p := range []struct {
		name   string
		lo, hi uint64
	}{
		{"primary volume header", volumeHeaderOffset, volumeHeaderOffset + 512},
		{"alternate volume header", volEnd - 1024, volEnd - 512},
		{"journal info block", jib, jib + bs},
	} {
		if lo < p.hi && p.lo < hi {
			return fmt.Errorf("%w: the journal [%d, %d) covers the %s at %d", ErrJournaled, lo, hi, p.name, p.lo)
		}
	}
	return nil
}

// checkJournalPlacement is journalPlacement for a volume still to be opened for
// writing; a volume with no journal, or a journal never initialised, passes.
func checkJournalPlacement(r io.ReaderAt, imgSize int64, vh *volumeHeader) error {
	// Whatever else is wrong with the journal is reported by checkJournal, which every
	// writable open runs first; loadJournalAt leaves loc empty for no journal, a journal
	// never initialised, and every error.
	_, loc, _ := loadJournalAt(r, imgSize, vh)
	if loc.hdr.size == 0 {
		return nil
	}
	return journalPlacement(loc, vh)
}

// checkJournal inspects the journal of the volume held in img. A volume that is
// not journaled, or whose journal is empty, may be written; any other outcome is
// an error wrapping ErrJournaled. It only reads img.
func checkJournal(img []byte, vh *volumeHeader) (journalState, error) {
	return checkJournalAt(bytesReaderAt(img), int64(len(img)), vh)
}

// checkJournalAt is checkJournal over a reader of size bytes: it reads only the
// journal info block and the journal header, never the image.
func checkJournalAt(r io.ReaderAt, imgSize int64, vh *volumeHeader) (journalState, error) {
	return inspectJournalAt(r, imgSize, vh, nil)
}

// journalLoc is where a journal lies and what its header says; loadJournalAt
// fills it in when the journal has transactions (start != end).
type journalLoc struct {
	off uint64
	hdr journalHeader
	// jibSize is the journal size the journal info block records, which need not
	// equal hdr.size (hfsplus#48).
	jibSize uint64
}

// loadJournalAt runs every check of checkJournalAt except the last one (it does
// not refuse start != end): it returns st.empty == false, a nil error and the
// journal's location for a journal that holds transactions, which is what the
// replayer reads. checkJournalAt turns that case into ErrJournaled.
func loadJournalAt(r io.ReaderAt, imgSize int64, vh *volumeHeader) (journalState, journalLoc, error) {
	var loc journalLoc
	st, err := inspectJournalAt(r, imgSize, vh, &loc)
	return st, loc, err
}

// inspectJournalAt is the body of both: it stores the location in loc once the
// header is decoded and the bounds hold, and refuses start != end only when
// loc is nil.
func inspectJournalAt(r io.ReaderAt, imgSize int64, vh *volumeHeader, loc *journalLoc) (journalState, error) {
	if vh.Attributes&kHFSVolumeJournaledBit == 0 {
		return journalState{}, nil
	}
	st := journalState{journaled: true}
	jibOff := uint64(vh.JournalInfoBlock) * uint64(vh.BlockSize)
	if vh.JournalInfoBlock == 0 || imgSize < 0 || jibOff+journalInfoBlockLen > uint64(imgSize) {
		return st, fmt.Errorf("%w: journal info block %d is outside the volume", ErrJournaled, vh.JournalInfoBlock)
	}
	jib := make([]byte, journalInfoBlockLen)
	if _, err := r.ReadAt(jib, int64(jibOff)); err != nil {
		return st, fmt.Errorf("%w: journal info block cannot be read: %v", ErrJournaled, err)
	}
	flags := binary.BigEndian.Uint32(jib[0:4])
	if flags&kJIJournalNeedInitMask != 0 {
		st.empty = true // TN1150: never initialised, no valid transactions
		return st, nil
	}
	if flags&kJIJournalInFSMask == 0 || flags&kJIJournalOnOtherDeviceMask != 0 {
		return st, fmt.Errorf("%w: journal is not inside the volume (flags %#x)", ErrJournaled, flags)
	}
	off, size := binary.BigEndian.Uint64(jib[36:44]), binary.BigEndian.Uint64(jib[44:52])
	if off > uint64(imgSize) || size > uint64(imgSize)-off {
		return st, fmt.Errorf("%w: journal at %d, size %d, is outside the volume", ErrJournaled, off, size)
	}
	// The journal info block's size is held to what Apple's journal_open holds the
	// size it is given: 256 KiB to 1 GiB (core/hfs_journal.c:1905) and a multiple of
	// the sector size (:1918), 512 here; hfs_vfsutils.c:2628 passes the info block's
	// jib_size. Without it the replay rewrites a 512-byte header past a declared
	// extent of 44 bytes.
	if size < 256*1024 || size > 1<<30 || size%512 != 0 {
		return st, fmt.Errorf("%w: journal info block size %d is outside [256 KiB, 1 GiB] or not a multiple of 512", ErrJournaled, size)
	}
	hdr := make([]byte, min(uint64(imgSize)-off, maxJournalHeaderBytes))
	if n, err := r.ReadAt(hdr, int64(off)); n < len(hdr) {
		return st, fmt.Errorf("%w: journal header cannot be read: %v", ErrJournaled, err)
	}
	jh, err := decodeJournalHeader(hdr)
	if err != nil {
		return st, err
	}
	start, end, jsize, jhdrSize := jh.start, jh.end, jh.size, jh.jhdrSize
	// Apple's journal_open never compares the header's size with the journal info
	// block's (hfsplus#48): the header's is the one the ring uses. What the reader
	// needs is that the ring lies inside the volume; the writer, which must not
	// write past the extent the info block declares, compares the two itself
	// (enableJournalWrites, via journalLoc.jibSize).
	if jsize > uint64(imgSize)-off {
		return st, fmt.Errorf("%w: journal at %d, header size %d, is outside the volume", ErrJournaled, off, jsize)
	}
	// Exactly what Apple's journal_open refuses (core/hfs_journal.c, tag
	// hfs-715.100.10), no more: size below 256 KiB or above 1 GiB (:2035); start
	// (:2019-2021) or end (:2027-2029) <= 0, above size, or above 1 GiB; start
	// (:2046) or end (:2053) not a multiple of 512. A journal macOS opens is not
	// refused here. The one addition is jhdr_size > size, which our reader needs
	// so the header cannot claim to be larger than its journal.
	if jsize < 256*1024 || jsize > 1<<30 {
		return st, fmt.Errorf("%w: journal size %d is outside [256 KiB, 1 GiB]", ErrJournaled, jsize)
	}
	if uint64(jhdrSize) > jsize {
		return st, fmt.Errorf("%w: journal header size %d exceeds the journal size %d", ErrJournaled, jhdrSize, jsize)
	}
	for _, f := range []struct {
		name string
		v    uint64
	}{{"start", start}, {"end", end}} {
		if f.v == 0 || f.v > jsize || f.v > 1<<30 || f.v%512 != 0 {
			return st, fmt.Errorf("%w: journal header %s %d is outside (0, %d] or not 512-aligned", ErrJournaled, f.name, f.v, jsize)
		}
	}
	if loc != nil {
		*loc = journalLoc{off: off, hdr: jh, jibSize: size}
		if start != end {
			return st, nil // transactions pending: the caller replays them
		}
	}
	if start != end {
		return st, fmt.Errorf("%w: transactions pending (start %d, end %d)", ErrJournaled, start, end)
	}
	st.empty = true
	return st, nil
}
