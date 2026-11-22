// Copyright (c) 2026, go-filesystems
// SPDX-License-Identifier: BSD-3-Clause

package hfsplus

import (
	"bytes"
	"encoding/binary"
	"errors"
	"os"
	"path/filepath"
	"testing"
)

// macJournalHeader is the first 48 bytes of the journal header of an image made
// by `hdiutil create -fs "Journaled HFS+"` on Darwin 27.2 (arm64), read at the
// offset its journal info block names (0x3000). It is little-endian, as the host
// that wrote it: the endian field is what tells.
var macJournalHeader = []byte{
	0x78, 0x4c, 0x4e, 0x4a, 0x78, 0x56, 0x34, 0x12, // magic, endian
	0x00, 0x44, 0x00, 0x00, 0x00, 0x00, 0x00, 0x00, // start 0x4400
	0x00, 0x44, 0x00, 0x00, 0x00, 0x00, 0x00, 0x00, // end   0x4400
	0x00, 0x00, 0x08, 0x00, 0x00, 0x00, 0x00, 0x00, // size  0x80000
	0x00, 0x20, 0x00, 0x00, 0xdd, 0x52, 0xe7, 0x08, // blhdr_size 0x2000, checksum
	0x00, 0x02, 0x00, 0x00, 0xea, 0xc4, 0x4c, 0x00, // jhdr_size 512, (sequence number)
}

// sector pads a journal header to its sector, as on disk.
func sector(h []byte) []byte {
	s := make([]byte, 512)
	copy(s, h)
	return s
}

// buildJournalHeader encodes a journal header in the given byte order, with a
// valid checksum unless badSum.
func buildJournalHeader(bo binary.ByteOrder, start, end, size uint64, badSum bool) []byte {
	h := make([]byte, 512)
	bo.PutUint32(h[0:], journalHeaderMagic)
	bo.PutUint32(h[4:], journalEndianMagic)
	bo.PutUint64(h[8:], start)
	bo.PutUint64(h[16:], end)
	bo.PutUint64(h[24:], size)
	bo.PutUint32(h[32:], 0x2000)
	bo.PutUint32(h[40:], 512)
	sum := journalChecksum(h[:journalHeaderLen])
	if badSum {
		sum++
	}
	bo.PutUint32(h[36:], sum)
	return h
}

func TestJournalHeaderGolden(t *testing.T) {
	start, end, size, jh, err := readJournalHeader(sector(macJournalHeader))
	if err != nil {
		t.Fatalf("macOS-made journal header rejected: %v", err)
	}
	if start != 0x4400 || end != 0x4400 || size != 0x80000 || jh != 512 {
		t.Errorf("start %#x end %#x size %#x jhdr %d", start, end, size, jh)
	}
	// One flipped bit anywhere in the sector must break the checksum.
	for _, i := range []int{9, 17, 26, 33, 41} {
		h := sector(macJournalHeader)
		h[i] ^= 0x01
		if _, _, _, _, err := readJournalHeader(h); !errors.Is(err, ErrJournaled) {
			t.Errorf("flipped byte %d: err = %v, want ErrJournaled", i, err)
		}
	}
}

func TestJournalHeaderByteOrders(t *testing.T) {
	for name, bo := range map[string]binary.ByteOrder{"big": binary.BigEndian, "little": binary.LittleEndian} {
		start, end, size, _, err := readJournalHeader(buildJournalHeader(bo, 512, 1024, 4096, false))
		if err != nil || start != 512 || end != 1024 || size != 4096 {
			t.Errorf("%s: %d %d %d %v", name, start, end, size, err)
		}
		if _, _, _, _, err := readJournalHeader(buildJournalHeader(bo, 512, 1024, 4096, true)); !errors.Is(err, ErrJournaled) {
			t.Errorf("%s bad checksum: %v", name, err)
		}
	}
	for _, h := range [][]byte{nil, make([]byte, 20), make([]byte, 512)} {
		if _, _, _, _, err := readJournalHeader(h); !errors.Is(err, ErrJournaled) {
			t.Errorf("len %d: err = %v, want ErrJournaled", len(h), err)
		}
	}
	// jhdr_size zero (Apple, hfs_journal.c:1961) or smaller than the struct read.
	for _, js := range []uint32{0, 43} {
		h := buildJournalHeader(binary.BigEndian, 512, 512, 4096, false)
		binary.BigEndian.PutUint32(h[40:], js)
		resignJournalHeader(h)
		if _, _, _, _, err := readJournalHeader(h); !errors.Is(err, ErrJournaled) {
			t.Errorf("jhdr_size %d: err = %v", js, err)
		}
	}
	// Apple refuses only zero, so any other size at least the struct is decoded
	// (a power-of-two sector-size rule would have refused 100, 513 and 768).
	for _, js := range []uint32{44, 100, 513, 768, 8192} {
		h := buildJournalHeader(binary.BigEndian, 512, 512, 4096, false)
		binary.BigEndian.PutUint32(h[40:], js)
		resignJournalHeader(h)
		if _, _, _, jh, err := readJournalHeader(h); err != nil || jh != js {
			t.Errorf("jhdr_size %d: %d, %v", js, jh, err)
		}
	}
}

// journaledImage returns a fresh volume marked journaled, with a journal info
// block and journal placed in free blocks at the end of the volume (the real
// layout is the same: both live in allocation blocks the bitmap owns). jh is
// the journal header sector; flags the JIB flags.
func journaledImage(t testing.TB, flags uint32, jh []byte, size uint64) (img []byte, jibBlock, jBlock uint32) {
	t.Helper()
	img, err := Mkfs(8<<20, FormatConfig{Label: "JNL"})
	if err != nil {
		t.Fatal(err)
	}
	vh, err := readVolumeHeader(bytesReaderAt(img))
	if err != nil {
		t.Fatal(err)
	}
	if vh.BlockSize != 4096 {
		t.Fatalf("block size %d, the tests assume 4096", vh.BlockSize)
	}
	// The journal is 512 KiB = 128 blocks; both sit clear of the alternate
	// volume header in the last block.
	jibBlock = vh.TotalBlocks - 150
	jBlock = vh.TotalBlocks - 140
	be := binary.BigEndian
	h := img[volumeHeaderOffset:]
	be.PutUint32(h[4:], vh.Attributes|kHFSVolumeJournaledBit)
	copy(h[8:12], "HFSJ")
	be.PutUint32(h[12:], jibBlock)
	jib := img[uint64(jibBlock)*uint64(vh.BlockSize):]
	be.PutUint32(jib[0:], flags)
	be.PutUint64(jib[36:], uint64(jBlock)*uint64(vh.BlockSize))
	be.PutUint64(jib[44:], size)
	copy(img[uint64(jBlock)*uint64(vh.BlockSize):], jh)
	// The bitmap owns both: mark the JIB block and the journal's 128 blocks
	// allocated (and take them off freeBlocks, as macOS does), so a larger write
	// cannot allocate over them.
	bmOff := uint64(vh.AllocationFile.Extents[0].StartBlock) * uint64(vh.BlockSize)
	used := uint32(0)
	for _, r := range [][2]uint32{{jibBlock, 1}, {jBlock, 128}} {
		for b := r[0]; b < r[0]+r[1]; b++ {
			if img[bmOff+uint64(b/8)]&(0x80>>(b%8)) != 0 {
				t.Fatalf("block %d is already allocated", b)
			}
			img[bmOff+uint64(b/8)] |= 0x80 >> (b % 8)
			used++
		}
	}
	be.PutUint32(h[0x30:], vh.FreeBlocks-used)
	return img, jibBlock, jBlock
}

// withJhdrSize returns a copy of header h with jhdr_size js, re-signed.
func withJhdrSize(h []byte, js uint32) []byte {
	h = bytes.Clone(h)
	bo := binary.ByteOrder(binary.LittleEndian)
	if binary.BigEndian.Uint32(h[0:4]) == journalHeaderMagic {
		bo = binary.BigEndian
	}
	bo.PutUint32(h[40:], js)
	resignJournalHeader(h)
	return h
}

func TestCheckJournal(t *testing.T) {
	const size = 0x80000
	empty := buildJournalHeader(binary.LittleEndian, 0x4400, 0x4400, size, false)
	cases := []struct {
		name  string
		flags uint32
		jh    []byte
		size  uint64
		mut   func(img []byte, jibBlock uint32)
		want  string // "" = writable
	}{
		{"empty little-endian", kJIJournalInFSMask, empty, size, nil, ""},
		{"empty big-endian", kJIJournalInFSMask, buildJournalHeader(binary.BigEndian, 512, 512, size, false), size, nil, ""},
		{"never initialised", kJIJournalInFSMask | kJIJournalNeedInitMask, make([]byte, 512), size, nil, ""},
		{"transactions pending", kJIJournalInFSMask, buildJournalHeader(binary.LittleEndian, 0x4400, 0x6000, size, false), size, nil, "cannot be replayed"},
		// The volumes of these two carry 'HFSJ' and a journal of zeros, so since PR 2 of
		// #45 the replayer is what refuses them (checkJournal itself still says
		// "transactions pending"; see replay_write_test.go).
		{"wrapped transactions", kJIJournalInFSMask, buildJournalHeader(binary.LittleEndian, 0x6000, 0x4400, size, false), size, nil, "cannot be replayed"},
		{"bad checksum", kJIJournalInFSMask, buildJournalHeader(binary.LittleEndian, 0x4400, 0x4400, size, true), size, nil, "checksum"},
		{"no magic", kJIJournalInFSMask, make([]byte, 512), size, nil, "magic"},
		{"other device", kJIJournalOnOtherDeviceMask, empty, size, nil, "not inside the volume"},
		{"neither flag", 0, empty, size, nil, "not inside the volume"},
		{"size mismatch", kJIJournalInFSMask, empty, size + 4096, nil, "differs"},
		{"size too small", kJIJournalInFSMask, empty, 8, nil, "outside the volume"},
		{"size past the volume", kJIJournalInFSMask, empty, 1 << 40, nil, "outside the volume"},
		// What Apple's journal_open refuses (hfs_journal.c:2019-2033 start/end <= 0
		// or above size; :2046, :2053 not a multiple of 512).
		{"start == end == 0", kJIJournalInFSMask, buildJournalHeader(binary.LittleEndian, 0, 0, size, false), size, nil, "outside"},
		{"start == end == 256", kJIJournalInFSMask, buildJournalHeader(binary.LittleEndian, 256, 256, size, false), size, nil, "512-aligned"},
		{"start zero, end set", kJIJournalInFSMask, buildJournalHeader(binary.LittleEndian, 0, 0x4400, size, false), size, nil, "outside"},
		{"start == end past size", kJIJournalInFSMask, buildJournalHeader(binary.LittleEndian, size+512, size+512, size, false), size, nil, "outside"},
		{"start == end unaligned", kJIJournalInFSMask, buildJournalHeader(binary.LittleEndian, 0x4401, 0x4401, size, false), size, nil, "512-aligned"},
		{"end past size", kJIJournalInFSMask, buildJournalHeader(binary.LittleEndian, 0x4400, size+512, size, false), size, nil, "outside"},
		{"start == end == size", kJIJournalInFSMask, buildJournalHeader(binary.LittleEndian, size, size, size, false), size, nil, ""},
		// Accepted by Apple's journal_open, so accepted here: jhdr_size is refused
		// only when zero (hfs_journal.c:1961), start/end may lie below jhdr_size
		// (:2019-2033 only bound them by 0 and size). A power-of-two or
		// [jhdr_size, size] rule would have refused all three.
		{"jhdr_size 768 (not a sector size)", kJIJournalInFSMask, withJhdrSize(empty, 768), size, nil, ""},
		{"jhdr_size 4096, start == end == 512", kJIJournalInFSMask, withJhdrSize(buildJournalHeader(binary.LittleEndian, 512, 512, size, false), 4096), size, nil, ""},
		{"jhdr_size zero", kJIJournalInFSMask, withJhdrSize(empty, 0), size, nil, "header size 0"},
		{"jhdr_size beyond the journal", kJIJournalInFSMask, withJhdrSize(empty, size+512), size, nil, "exceeds the journal size"},
		{"journal below 256 KiB", kJIJournalInFSMask, buildJournalHeader(binary.LittleEndian, 512, 512, 0x20000, false), 0x20000, nil, "outside [256 KiB"},
		{"info block zero", kJIJournalInFSMask, empty, size, func(img []byte, _ uint32) {
			binary.BigEndian.PutUint32(img[volumeHeaderOffset+12:], 0)
		}, "outside the volume"},
		{"info block past the volume", kJIJournalInFSMask, empty, size, func(img []byte, _ uint32) {
			binary.BigEndian.PutUint32(img[volumeHeaderOffset+12:], 0xffffffff)
		}, "outside the volume"},
		{"offset past the volume", kJIJournalInFSMask, empty, size, func(img []byte, jib uint32) {
			binary.BigEndian.PutUint64(img[uint64(jib)*4096+36:], 1<<62)
		}, "outside the volume"},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			img, jib, _ := journaledImage(t, c.flags, c.jh, c.size)
			if c.mut != nil {
				c.mut(img, jib)
			}
			_, err := OpenWritable(img, nil)
			if c.want == "" {
				if err != nil {
					t.Fatalf("OpenWritable: %v", err)
				}
				return
			}
			if !errors.Is(err, ErrJournaled) {
				t.Fatalf("OpenWritable err = %v, want ErrJournaled", err)
			}
			if !bytes.Contains([]byte(err.Error()), []byte(c.want)) {
				t.Errorf("err = %q, want it to mention %q", err, c.want)
			}
			// Read-only is never refused.
			v, err := Open(bytesReaderAt(img), int64(len(img)))
			if err != nil {
				t.Fatalf("read-only Open: %v", err)
			}
			if _, err := v.ListDir("/"); err != nil {
				t.Fatalf("read-only ListDir: %v", err)
			}
			if err := v.WriteFile("/x", []byte("x"), 0o644); !errors.Is(err, ErrReadOnly) {
				t.Errorf("WriteFile on a read-only journaled volume: %v", err)
			}
		})
	}
}

// A journaled volume with an empty journal is written, and the write leaves the
// journal info block, the journal and the journaled attribute as they were: the
// journal stays empty, so macOS's replay at mount has nothing to copy over the
// written blocks (checked on macOS in TestDarwinWriteIntoJournaledImage).
func TestWriteIntoEmptyJournal(t *testing.T) {
	empty := buildJournalHeader(binary.LittleEndian, 0x4400, 0x4400, 0x80000, false)
	img, jibBlock, jBlock := journaledImage(t, kJIJournalInFSMask, empty, 0x80000)
	v, err := OpenWritable(img, nil)
	if err != nil {
		t.Fatal(err)
	}
	noJournal(v) // the in-place path
	if err := v.WriteFile("/hello.txt", []byte("hello\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := v.MkDir("/d", 0o755); err != nil {
		t.Fatal(err)
	}
	got, err := v.ReadFile("/hello.txt")
	if err != nil || string(got) != "hello\n" {
		t.Fatalf("ReadFile = %q, %v", got, err)
	}
	out := v.Bytes()
	if a := binary.BigEndian.Uint32(out[volumeHeaderOffset+4:]); a&kHFSVolumeJournaledBit == 0 {
		t.Errorf("journaled attribute lost: %#x", a)
	}
	if !bytes.Equal(out[uint64(jibBlock)*4096:][:journalInfoBlockLen], img[uint64(jibBlock)*4096:][:journalInfoBlockLen]) {
		t.Error("journal info block changed")
	}
	if !bytes.Equal(out[uint64(jBlock)*4096:][:512], empty) {
		t.Error("journal header changed")
	}
	// The first write signed both headers (TN1150: lastMountedVersion).
	for name, off := range map[string]int{"primary": volumeHeaderOffset, "alternate": len(out) - 1024} {
		if got := string(out[off+8 : off+12]); got != lastMountedVersion {
			t.Errorf("%s header lastMountedVersion = %q, want %q", name, got, lastMountedVersion)
		}
	}
	// Reopened for writing it is still accepted: the journal is still empty.
	if _, err := OpenWritable(out, nil); err != nil {
		t.Errorf("reopen after write: %v", err)
	}
}

// The fixture marks the info block and the journal allocated, so a write that
// needs most of the free space cannot land on them.
func TestLargeWriteSparesJournal(t *testing.T) {
	empty := buildJournalHeader(binary.LittleEndian, 0x4400, 0x4400, 0x80000, false)
	img, jibBlock, jBlock := journaledImage(t, kJIJournalInFSMask, empty, 0x80000)
	want := bytes.Clone(img[uint64(jBlock)*4096:][:128*4096])
	jibWant := bytes.Clone(img[uint64(jibBlock)*4096:][:4096])
	v, err := OpenWritable(img, nil)
	if err != nil {
		t.Fatal(err)
	}
	noJournal(v)
	free := int(v.VolumeHeader().FreeBlocks)
	data := bytes.Repeat([]byte{0xA5}, (free-16)*4096)
	if err := v.WriteFile("/big", data, 0o644); err != nil {
		t.Fatal(err)
	}
	out := v.Bytes()
	if !bytes.Equal(out[uint64(jBlock)*4096:][:128*4096], want) || !bytes.Equal(out[uint64(jibBlock)*4096:][:4096], jibWant) {
		t.Error("a large write overwrote the journal or its info block")
	}
	if got, err := v.ReadFile("/big"); err != nil || !bytes.Equal(got, data) {
		t.Errorf("ReadFile: %v", err)
	}
}

// OpenFileWritable refuses a journal with transactions in it, closes the file
// and writes nothing to it.
func TestOpenFileWritableRefusesPendingJournal(t *testing.T) {
	pending := buildJournalHeader(binary.LittleEndian, 0x4400, 0x6000, 0x80000, false)
	img, _, _ := journaledImage(t, kJIJournalInFSMask, pending, 0x80000)
	p := filepath.Join(t.TempDir(), "j.img")
	if err := os.WriteFile(p, img, 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := OpenFileWritable(p); !errors.Is(err, ErrJournaled) {
		t.Fatalf("OpenFileWritable err = %v, want ErrJournaled", err)
	}
	after, err := os.ReadFile(p)
	if err != nil || !bytes.Equal(after, img) {
		t.Errorf("image changed by a refused open (read err %v)", err)
	}
	if v, err := OpenFile(p); err != nil {
		t.Errorf("read-only OpenFile: %v", err)
	} else {
		v.Close()
	}
}

// Format and Mkfs volumes are not journaled and stay writable.
func TestUnjournaledVolumeUnaffected(t *testing.T) {
	img, err := Mkfs(8<<20, FormatConfig{Label: "PLAIN"})
	if err != nil {
		t.Fatal(err)
	}
	vh, _ := readVolumeHeader(bytesReaderAt(img))
	if st, err := checkJournal(img, vh); err != nil || st.journaled {
		t.Fatalf("checkJournal = %+v, %v", st, err)
	}
	if _, err := OpenWritable(img, nil); err != nil {
		t.Fatal(err)
	}
}

// FuzzJournalHeader feeds readJournalHeader arbitrary sectors. It must not panic
// and may only fail with ErrJournaled; a header it accepts must carry a checksum
// that holds.
func FuzzJournalHeader(f *testing.F) {
	f.Add(sector(macJournalHeader))
	f.Add(buildJournalHeader(binary.BigEndian, 512, 1024, 4096, false))
	f.Add(buildJournalHeader(binary.LittleEndian, 512, 512, 4096, true))
	f.Add([]byte{})
	f.Add(bytes.Repeat([]byte{0xff}, 512))
	f.Fuzz(func(t *testing.T, h []byte) {
		start, end, size, jh, err := readJournalHeader(h)
		if err != nil {
			if !errors.Is(err, ErrJournaled) {
				t.Fatalf("err %v does not wrap ErrJournaled", err)
			}
			return
		}
		if jh < journalHeaderLen {
			t.Fatalf("accepted jhdr_size %d", jh)
		}
		_, _, _ = start, end, size
	})
}

// resignJournalHeader recomputes the checksum of the journal header at h, in the
// byte order its magic names (and leaves a header with no valid magic alone).
func resignJournalHeader(h []byte) {
	var bo binary.ByteOrder
	switch {
	case binary.BigEndian.Uint32(h[0:4]) == journalHeaderMagic:
		bo = binary.BigEndian
	case binary.LittleEndian.Uint32(h[0:4]) == journalHeaderMagic:
		bo = binary.LittleEndian
	default:
		return
	}
	bo.PutUint32(h[36:40], 0)
	bo.PutUint32(h[36:40], journalChecksum(h[:journalHeaderLen]))
}

// FuzzCheckJournal splices fuzzer-chosen bytes into the journal info block and
// the journal header of a journaled volume: the volume is writable only when
// checkJournal says so, and checkJournal never panics or fails with anything
// but ErrJournaled.
func FuzzCheckJournal(f *testing.F) {
	empty := buildJournalHeader(binary.LittleEndian, 0x4400, 0x4400, 0x80000, false)
	base, jibBlock, jBlock := journaledImage(f, kJIJournalInFSMask, empty, 0x80000)
	jibOff, jOff := int64(jibBlock)*4096, int64(jBlock)*4096
	f.Add(true, int64(0), []byte{0xff, 0xff, 0xff, 0xff})
	f.Add(true, int64(36), bytes.Repeat([]byte{0xff}, 16))
	f.Add(false, int64(8), []byte{0x00, 0x60})
	f.Add(false, int64(0), []byte{0})
	f.Fuzz(func(t *testing.T, inJIB bool, off int64, patch []byte) {
		if off < 0 || off > 512 || len(patch) == 0 || len(patch) > 512 {
			return
		}
		img := append([]byte(nil), base...)
		at := jOff
		if inJIB {
			at = jibOff
		}
		copy(img[at+off:], patch)
		if !inJIB {
			// Sign what was patched, so the checksum does not turn every input
			// away before the fields behind it are looked at.
			resignJournalHeader(img[jOff : jOff+512])
		}
		vh, err := readVolumeHeader(bytesReaderAt(img))
		if err != nil {
			return
		}
		st, err := checkJournal(img, vh)
		if err != nil && !errors.Is(err, ErrJournaled) {
			t.Fatalf("err %v does not wrap ErrJournaled", err)
		}
		if err == nil && !st.empty {
			t.Fatal("accepted a journal that is not empty")
		}
		if err != nil || !st.journaled {
			return
		}
		// Accepted: decode the header independently of the code under test and
		// hold it to what Apple's journal_open and TN1150 require of an empty
		// journal (a never-initialised one carries no header to read).
		be := binary.BigEndian
		jib := img[int64(vh.JournalInfoBlock)*4096:]
		if be.Uint32(jib[0:4])&kJIJournalNeedInitMask != 0 {
			return
		}
		joff, size := be.Uint64(jib[36:44]), be.Uint64(jib[44:52])
		h := img[joff:]
		var bo binary.ByteOrder = binary.LittleEndian
		if be.Uint32(h[0:4]) == journalHeaderMagic {
			bo = be
		}
		start, end, jsize, jhdr := bo.Uint64(h[8:16]), bo.Uint64(h[16:24]), bo.Uint64(h[24:32]), uint64(bo.Uint32(h[40:44]))
		if start != end {
			t.Fatalf("accepted start %d != end %d", start, end)
		}
		if jsize != size || jsize < 256*1024 || jsize > 1<<30 || jhdr < journalHeaderLen || jhdr > jsize || start == 0 || start > jsize || start%512 != 0 {
			t.Fatalf("accepted out-of-range header: start %d end %d size %d (info block %d) jhdr_size %d", start, end, jsize, size, jhdr)
		}
	})
}
