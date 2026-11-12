// Copyright (c) 2026, go-filesystems
// SPDX-License-Identifier: BSD-3-Clause

package hfsplus

import (
	"encoding/binary"
	"errors"
	"fmt"
)

// The journal, from Apple TN1150 "HFS Plus Volume Format", sections "Journal",
// "Journal Info Block" and "Journal Checksums".
//
// This driver does not write transactions. A volume whose journal is empty
// (start == end) is safe to write directly: macOS's replay at mount finds
// nothing to replay, so it cannot overwrite what was written. A volume whose
// journal holds transactions is not: replay would copy the journalled blocks
// over the driver's. OpenWritable therefore refuses such a volume with
// ErrJournaled; Open (read-only) is unaffected.

const (
	kHFSVolumeJournaledBit = 1 << 13 // volume header attributes, bit 13

	kJIJournalInFSMask          = 0x00000001
	kJIJournalOnOtherDeviceMask = 0x00000002
	kJIJournalNeedInitMask      = 0x00000004

	journalHeaderMagic = 0x4a4e4c78
	journalEndianMagic = 0x12345678

	// journalInfoBlockLen is the 4 + 32 + 8 + 8 + 128 bytes of JournalInfoBlock.
	journalInfoBlockLen = 180
	// journalHeaderLen is the bytes of journal_header the checks read (through
	// jhdr_size); the on-disk header fills one sector.
	journalHeaderLen = 44
	// maxJournalHeaderBytes bounds jhdr_size, one sector (TN1150: "2048 on many
	// types of optical media"); a larger one is not a journal header.
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
	if len(h) < journalHeaderLen {
		return 0, 0, 0, 0, fmt.Errorf("%w: journal header is truncated", ErrJournaled)
	}
	var bo binary.ByteOrder
	switch {
	case binary.BigEndian.Uint32(h[0:4]) == journalHeaderMagic && binary.BigEndian.Uint32(h[4:8]) == journalEndianMagic:
		bo = binary.BigEndian
	case binary.LittleEndian.Uint32(h[0:4]) == journalHeaderMagic && binary.LittleEndian.Uint32(h[4:8]) == journalEndianMagic:
		bo = binary.LittleEndian
	default:
		return 0, 0, 0, 0, fmt.Errorf("%w: journal header magic is not valid", ErrJournaled)
	}
	jhdrSize = bo.Uint32(h[40:44])
	if jhdrSize < journalHeaderLen || jhdrSize > maxJournalHeaderBytes {
		return 0, 0, 0, 0, fmt.Errorf("%w: journal header size %d", ErrJournaled, jhdrSize)
	}
	// TN1150: zero the checksum field, then checksum the header. The header it
	// means is the journal_header struct (44 bytes, through jhdr_size), not the
	// whole jhdr_size-byte sector: a header written by macOS 27.2 verifies only
	// that way (see macJournalHeader in journal_test.go).
	var buf [journalHeaderLen]byte
	copy(buf[:], h)
	copy(buf[36:40], []byte{0, 0, 0, 0})
	if got, want := journalChecksum(buf[:]), bo.Uint32(h[36:40]); got != want {
		return 0, 0, 0, 0, fmt.Errorf("%w: journal header checksum %#x, want %#x", ErrJournaled, want, got)
	}
	return bo.Uint64(h[8:16]), bo.Uint64(h[16:24]), bo.Uint64(h[24:32]), jhdrSize, nil
}

// journalChecksum is TN1150's calc_checksum.
func journalChecksum(b []byte) uint32 {
	var c int32
	for _, x := range b {
		c = (c << 8) ^ (c + int32(x))
	}
	return uint32(^c)
}

// checkJournal inspects the journal of the volume held in img. A volume that is
// not journaled, or whose journal is empty, may be written; any other outcome is
// an error wrapping ErrJournaled. It only reads img.
func checkJournal(img []byte, vh *volumeHeader) (journalState, error) {
	if vh.Attributes&kHFSVolumeJournaledBit == 0 {
		return journalState{}, nil
	}
	st := journalState{journaled: true}
	jibOff := uint64(vh.JournalInfoBlock) * uint64(vh.BlockSize)
	if vh.JournalInfoBlock == 0 || jibOff+journalInfoBlockLen > uint64(len(img)) {
		return st, fmt.Errorf("%w: journal info block %d is outside the volume", ErrJournaled, vh.JournalInfoBlock)
	}
	jib := img[jibOff : jibOff+journalInfoBlockLen]
	flags := binary.BigEndian.Uint32(jib[0:4])
	if flags&kJIJournalNeedInitMask != 0 {
		st.empty = true // TN1150: never initialised, no valid transactions
		return st, nil
	}
	if flags&kJIJournalInFSMask == 0 || flags&kJIJournalOnOtherDeviceMask != 0 {
		return st, fmt.Errorf("%w: journal is not inside the volume (flags %#x)", ErrJournaled, flags)
	}
	off, size := binary.BigEndian.Uint64(jib[36:44]), binary.BigEndian.Uint64(jib[44:52])
	if off > uint64(len(img)) || size > uint64(len(img))-off || size < journalHeaderLen {
		return st, fmt.Errorf("%w: journal at %d, size %d, is outside the volume", ErrJournaled, off, size)
	}
	hdr := img[off:]
	if len(hdr) > maxJournalHeaderBytes {
		hdr = hdr[:maxJournalHeaderBytes]
	}
	start, end, jsize, _, err := readJournalHeader(hdr)
	if err != nil {
		return st, err
	}
	if jsize != size {
		return st, fmt.Errorf("%w: journal header size %d differs from the journal info block's %d", ErrJournaled, jsize, size)
	}
	if start != end {
		return st, fmt.Errorf("%w: transactions pending (start %d, end %d)", ErrJournaled, start, end)
	}
	st.empty = true
	return st, nil
}
