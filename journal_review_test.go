// Copyright (c) 2026, go-filesystems
// SPDX-License-Identifier: BSD-3-Clause

package hfsplus

import (
	"bytes"
	"encoding/binary"
	"errors"
	"testing"
)

// secTxnImage is a journaled image whose journal holds one transaction that sets
// the volume's free-block count to 7777 (v.vh.FreeBlocks == 7777 after a replay).
// legacy stamps the header with the old magic 'JHDR' in bo's order.
func secTxnImage(t *testing.T, bo binary.ByteOrder, legacy bool, jibSize uint64) (img []byte, jbase uint64) {
	t.Helper()
	const size = 0x80000
	h := buildJournalHeader(bo, 512, 512, size, false)
	img, jibBlock, jBlock := journaledImage(t, kJIJournalInFSMask, h, size)
	jbase = uint64(jBlock) * 4096
	vhSector := bytes.Clone(img[volumeHeaderOffset : volumeHeaderOffset+512])
	copy(vhSector[vhLastMountedVersion:], "HFSJ")
	binary.BigEndian.PutUint32(vhSector[0x30:], 7777)
	b := &jbuilder{bo: bo, size: size, jh: 512, bl: 0x2000, buf: img[jbase : jbase+size]}
	end := b.txn(512, 1, first, blk{volumeHeaderOffset / 512, vhSector})
	hdr := buildJournalHeader(bo, 512, end, size, false)
	if legacy {
		bo.PutUint32(hdr[0:], journalOldHeaderMagic)
		bo.PutUint32(hdr[4:], 0)
		bo.PutUint32(hdr[36:], 0)
	}
	copy(img[jbase:], hdr)
	copy(img[vhLastMountedVersionOff:], "HFSJ")
	binary.BigEndian.PutUint64(img[int64(jibBlock)*4096+44:], jibSize)
	return img, jbase
}

// Apple swaps a header for a swapped 'JNLx' only (hfs_journal.c:1969); a swapped
// 'JHDR' fails at :1979. On a little-endian Mac the bytes "JHDR" are that swapped
// magic. The native-order header is the control: it replays (security review of
// v0.19.0..main, finding 3).
func TestJournalSwappedOldMagicIsRefused(t *testing.T) {
	img, jbase := secTxnImage(t, binary.BigEndian, true, 0x80000)
	if string(img[jbase:jbase+4]) != "JHDR" {
		t.Fatalf("the fixture has magic %q, want the bytes JHDR", img[jbase:jbase+4])
	}
	before := bytes.Clone(img)
	if v, err := OpenWritable(img, nil); !errors.Is(err, ErrJournaled) || v != nil {
		t.Errorf("a swapped JHDR opened writable (volume %t): %v; want ErrJournaled", v != nil, err)
	}
	if !bytes.Equal(img, before) {
		t.Error("the refused open changed the image")
	}
	if _, err := decodeJournalHeader(img[jbase : jbase+512]); err == nil {
		t.Error("decodeJournalHeader accepted a swapped JHDR")
	}
}

func TestJournalNativeOldMagicStillReplays(t *testing.T) {
	img, _ := secTxnImage(t, binary.LittleEndian, true, 0x80000)
	v, err := OpenWritable(img, nil)
	if err != nil {
		t.Fatal(err)
	}
	if v.vh.FreeBlocks != 7777 {
		t.Errorf("FreeBlocks %d: the transaction was not replayed", v.vh.FreeBlocks)
	}
}

// The journal info block's size is held to Apple's rules: 256 KiB to 1 GiB
// (hfs_journal.c:1905) and a multiple of the sector size (:1918). Before the fix
// 4096, 44 and 0x80007 opened writable and replayed; with 44 the replay rewrote
// 512 bytes past the declared extent (finding 2).
func TestJournalInfoBlockSizeIsHeld(t *testing.T) {
	for _, tc := range []struct {
		name string
		size uint64
		ok   bool
	}{
		{"the 512 KiB the header says", 0x80000, true},
		{"4096", 4096, false},
		{"44", 44, false},
		{"zero", 0, false},
		{"not a sector multiple", 0x80000 + 7, false},
		{"one sector under 256 KiB", 256*1024 - 512, false},
		{"over 1 GiB", 1<<30 + 512, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			img, _ := secTxnImage(t, binary.LittleEndian, false, tc.size)
			before := bytes.Clone(img)
			v, err := OpenWritable(img, nil)
			if tc.ok {
				if err != nil || v.vh.FreeBlocks != 7777 {
					t.Fatalf("a valid size: %v", err)
				}
				return
			}
			if !errors.Is(err, ErrJournaled) || v != nil {
				t.Fatalf("info block size %d opened writable (volume %t): %v; want ErrJournaled", tc.size, v != nil, err)
			}
			if !bytes.Equal(img, before) {
				t.Error("the refused open changed the image")
			}
		})
	}
}

// A journal at offset 0 puts the primary volume header and the journal info block
// inside the ring: the journal writer would overwrite them (finding 4, which
// predates #70). The refusal is at the writable open; a read-only open is unaffected.
func TestJournalOverTheVolumeHeaderIsRefusedWritable(t *testing.T) {
	const size = 0x80000
	h := buildJournalHeader(binary.LittleEndian, 512, 512, size, false)
	for _, tc := range []struct {
		name string
		at   func(jibOff int64) int64
	}{
		{"offset 0, over the volume header", func(int64) int64 { return 0 }},
		{"starting just before the info block", func(jib int64) int64 { return jib - 512 }},
	} {
		t.Run(tc.name, func(t *testing.T) {
			img, jibBlock, _ := journaledImage(t, kJIJournalInFSMask, h, size)
			jibOff := int64(jibBlock) * 4096
			at := tc.at(jibOff)
			binary.BigEndian.PutUint64(img[jibOff+36:], uint64(at))
			if at != jibOff {
				copy(img[at:at+512], h)
			}
			before := bytes.Clone(img)
			if v, err := OpenWritable(img, nil); !errors.Is(err, ErrJournaled) || v != nil {
				t.Fatalf("opened writable (volume %t): %v; want ErrJournaled", v != nil, err)
			}
			if !bytes.Equal(img, before) {
				t.Error("the refused open changed the image")
			}
			if _, err := Open(bytesReaderAt(bytes.Clone(img)), int64(len(img))); err != nil {
				t.Errorf("the read-only open is refused too: %v", err)
			}
		})
	}
}
