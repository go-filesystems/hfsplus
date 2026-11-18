// Copyright (c) 2026, go-filesystems
// SPDX-License-Identifier: BSD-3-Clause

package hfsplus

import (
	"bytes"
	"encoding/binary"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"runtime"
	"runtime/debug"
	"strconv"
	"testing"
)

// jbuilder builds a journal by hand, in the layout of design section 4: a header
// sector, then a circular buffer [jh, size) of block lists and blocks.
type jbuilder struct {
	bo           binary.ByteOrder
	size, jh, bl uint64
	buf          []byte
}

type blk struct {
	bnum int64
	data []byte
}

func newJB(size, jh, bl uint64) *jbuilder {
	return &jbuilder{bo: binary.LittleEndian, size: size, jh: jh, bl: bl, buf: make([]byte, size)}
}

func fill(n int, v byte) []byte { return bytes.Repeat([]byte{v}, n) }

// put writes d at off, continuing at jh after size, and returns the offset after
// it (not wrapped when it is exactly size, as do_journal_io leaves it).
func (b *jbuilder) put(off uint64, d []byte) uint64 {
	for len(d) > 0 {
		if off >= b.size {
			off = b.jh
		}
		n := min(uint64(len(d)), b.size-off)
		copy(b.buf[off:off+n], d[:n])
		d = d[n:]
		off += n
	}
	return off
}

// txn writes one block list with its blocks at off and returns where the next
// list starts: off + bytes_used, wrapped (hfs_journal.c:1442-1446).
func (b *jbuilder) txn(off uint64, seq, flags uint32, blks ...blk) uint64 {
	hdr := fill(int(b.bl), 0x5a)
	used := b.bl
	for _, k := range blks {
		used += uint64(len(k.data))
	}
	b.bo.PutUint16(hdr[0:], uint16(b.bl/16-1))
	b.bo.PutUint16(hdr[2:], uint16(1+len(blks)))
	b.bo.PutUint32(hdr[4:], uint32(used))
	b.bo.PutUint32(hdr[8:], 0)
	b.bo.PutUint32(hdr[12:], flags)
	clear(hdr[16:32])
	b.bo.PutUint32(hdr[28:], seq)
	for i, k := range blks {
		e := hdr[16+16*(i+1):]
		b.bo.PutUint64(e[0:], uint64(k.bnum))
		b.bo.PutUint32(e[8:], uint32(len(k.data)))
		b.bo.PutUint32(e[12:], journalChecksum(k.data))
	}
	b.bo.PutUint32(hdr[8:], journalChecksum(hdr[:blhdrChecksumSize]))
	pos := b.put(off, hdr)
	for _, k := range blks {
		pos = b.put(pos, k.data)
	}
	next := off + used
	if next >= b.size {
		next = next%b.size + b.jh
	}
	return next
}

func (b *jbuilder) header(start, end uint64) journalHeader {
	return journalHeader{bo: b.bo, start: start, end: end, size: b.size,
		blhdrSize: uint32(b.bl), jhdrSize: uint32(b.jh)}
}

func (b *jbuilder) replay(start, end, volSize uint64) (*replayOverlay, error) {
	return replayJournal(bytesReaderAt(b.buf), 0, b.header(start, end), volSize)
}

// view reads n bytes of the replayed volume at off over an all-zero volume.
func view(ov *replayOverlay, off, n int64) []byte {
	rr := &replayReader{base: bytesReaderAt(make([]byte, 1<<20)), size: 1 << 20, ov: ov}
	p := make([]byte, n)
	if _, err := rr.ReadAt(p, off); err != nil && err != io.EOF {
		panic(err)
	}
	return p
}

const (
	tSize   = 16384 // the smallest size jhdr_size*(jhdr_size/16) allows at 512
	tVolume = 1 << 20
	first   = blhdrFirstHeader | blhdrCheckChecksums
)

func wantBlocks(t *testing.T, ov *replayOverlay, want map[int64]byte) {
	t.Helper()
	for bnum, v := range want {
		if got := view(ov, bnum*512, 512); !bytes.Equal(got, fill(512, v)) {
			t.Errorf("block %d = %#x..., want %#x", bnum, got[0], v)
		}
	}
	if len(ov.units) != len(want) {
		t.Errorf("overlay holds %d units, want %d", len(ov.units), len(want))
	}
}

func TestReplayTwoTransactions(t *testing.T) {
	b := newJB(tSize, 512, 512)
	n := b.txn(512, 7, first, blk{10, fill(512, 1)}, blk{11, fill(1024, 2)})
	end := b.txn(n, 8, first, blk{10, fill(512, 3)}, blk{-1, fill(512, 9)})
	ov, err := b.replay(512, end, tVolume)
	if err != nil {
		t.Fatal(err)
	}
	// Block 10 is written twice: the later list wins. Block 12 is the second sector
	// of the 1024-byte entry at 11. The killed entry (bnum -1) writes nothing.
	wantBlocks(t, ov, map[int64]byte{10: 3, 11: 2, 12: 2})
}

func TestReplayEmptyJournal(t *testing.T) {
	b := newJB(tSize, 512, 512)
	ov, err := b.replay(1024, 1024, tVolume)
	if err != nil || len(ov.units) != 0 {
		t.Fatalf("empty: %v, %d units", err, len(ov.units))
	}
	// start == size wraps to jhdr_size, as end == size does (:1177-1182).
	ov, err = b.replay(tSize, 512, tVolume)
	if err != nil || len(ov.units) != 0 {
		t.Fatalf("start == size, end == jhdr: %v", err)
	}
}

// A block list header split by the end of the buffer (hfs_journal.c:341-347,
// :1443-1446), a block split by it, and a journal that ends exactly at size.
func TestReplayWraparound(t *testing.T) {
	t.Run("split block list header", func(t *testing.T) {
		// Two-sector lists: max_blocks (63) must not exceed size/jhdr_size (:1302).
		b := newJB(4*tSize, 512, 1024)
		start := uint64(4*tSize - 512)
		end := b.txn(start, 5, first, blk{20, fill(1024, 0xa1)})
		if end != 2048 {
			t.Fatalf("end = %d, want 2048", end)
		}
		// The list header's second sector is at the start of the buffer.
		if b.buf[512] != 0x5a {
			t.Fatal("test did not wrap the header")
		}
		ov, err := b.replay(start, end, tVolume)
		if err != nil {
			t.Fatal(err)
		}
		wantBlocks(t, ov, map[int64]byte{20: 0xa1, 21: 0xa1})
	})
	t.Run("split block", func(t *testing.T) {
		b := newJB(tSize, 512, 512)
		start := uint64(tSize - 1024)
		end := b.txn(start, 5, first, blk{30, fill(1024, 0xb2)})
		if end != 1024 {
			t.Fatalf("end = %d, want 1024", end)
		}
		ov, err := b.replay(start, end, tVolume)
		if err != nil {
			t.Fatal(err)
		}
		wantBlocks(t, ov, map[int64]byte{30: 0xb2, 31: 0xb2})
	})
	t.Run("end == size", func(t *testing.T) {
		b := newJB(tSize, 512, 512)
		start := uint64(tSize - 1024)
		b.txn(start, 5, first, blk{40, fill(512, 0xc3)})
		// The header may state end == size; the kernel reads it as jhdr_size (:1180).
		ov, err := b.replay(start, tSize, tVolume)
		if err != nil {
			t.Fatal(err)
		}
		wantBlocks(t, ov, map[int64]byte{40: 0xc3})
	})
	t.Run("start == size", func(t *testing.T) {
		b := newJB(tSize, 512, 512)
		end := b.txn(512, 5, first, blk{41, fill(512, 0xd4)})
		ov, err := b.replay(tSize, end, tVolume)
		if err != nil {
			t.Fatal(err)
		}
		wantBlocks(t, ov, map[int64]byte{41: 0xd4})
	})
}

// A torn block (checksum mismatch, :1373-1382) drops its transaction and keeps
// the ones before it (:1413-1439); a bad block list header drops the previous
// transaction too, because the kernel restarts at the start of the last list
// that set BLHDR_FIRST_HEADER (the divergence is design section 11, risk 2).
func TestReplayTornAndBad(t *testing.T) {
	t.Run("torn block in range", func(t *testing.T) {
		b := newJB(tSize, 512, 512)
		n := b.txn(512, 1, first, blk{10, fill(512, 1)})
		end := b.txn(n, 2, first, blk{11, fill(512, 2)})
		b.buf[n+512+7] ^= 0xff // a byte of the second transaction's block
		ov, err := b.replay(512, end, tVolume)
		if err != nil {
			t.Fatal(err)
		}
		wantBlocks(t, ov, map[int64]byte{10: 1})
	})
	t.Run("torn block past end", func(t *testing.T) {
		b := newJB(tSize, 512, 512)
		n := b.txn(512, 1, first, blk{10, fill(512, 1)})
		b.txn(n, 2, first, blk{11, fill(512, 2)})
		b.buf[n+512+7] ^= 0xff
		ov, err := b.replay(512, n, tVolume) // end = n: the second list is past end
		if err != nil {
			t.Fatal(err)
		}
		wantBlocks(t, ov, map[int64]byte{10: 1})
	})
	t.Run("valid list past end is replayed", func(t *testing.T) {
		b := newJB(tSize, 512, 512)
		n := b.txn(512, 1, first, blk{10, fill(512, 1)})
		b.txn(n, 2, first, blk{11, fill(512, 2)})
		ov, err := b.replay(512, n, tVolume)
		if err != nil {
			t.Fatal(err)
		}
		wantBlocks(t, ov, map[int64]byte{10: 1, 11: 2})
	})
	t.Run("bad list header in range drops the previous transaction", func(t *testing.T) {
		b := newJB(tSize, 512, 512)
		n := b.txn(512, 1, first, blk{10, fill(512, 1)})
		end := b.txn(n, 2, first, blk{11, fill(512, 2)})
		b.buf[n+9] ^= 0xff // its checksum field
		ov, err := b.replay(512, end, tVolume)
		if err != nil {
			t.Fatal(err)
		}
		wantBlocks(t, ov, map[int64]byte{})
	})
	t.Run("bad first list header aborts", func(t *testing.T) {
		b := newJB(tSize, 512, 512)
		end := b.txn(512, 1, first, blk{10, fill(512, 1)})
		b.buf[512+9] ^= 0xff
		if _, err := b.replay(512, end, tVolume); !errors.Is(err, ErrJournalReplay) {
			t.Fatalf("err = %v, want ErrJournalReplay", err)
		}
	})
	t.Run("continuation list does not start a transaction", func(t *testing.T) {
		// Two lists of one transaction: only the first has BLHDR_FIRST_HEADER. A torn
		// second list restarts at the first, so the whole transaction is dropped.
		b := newJB(tSize, 512, 512)
		n := b.txn(512, 1, first, blk{10, fill(512, 1)})
		m := b.txn(n, 2, first, blk{11, fill(512, 2)}) // transaction 2, list 1
		end := b.txn(m, 2, blhdrCheckChecksums, blk{12, fill(512, 3)})
		b.buf[m+512+3] ^= 0xff // a byte of the continuation's block
		ov, err := b.replay(512, end, tVolume)
		if err != nil {
			t.Fatal(err)
		}
		wantBlocks(t, ov, map[int64]byte{10: 1})
	})
}

// Past end, a list is accepted when its sequence number equals the previous one
// or is one greater (:1268-1271; fsck the same, fsck_journal.c:590-593), and
// the read stops at any other: a list a previous lap left at that offset.
func TestReplaySequenceNumbers(t *testing.T) {
	build := func(seqs ...uint32) (*jbuilder, uint64, uint64) {
		b := newJB(tSize, 512, 512)
		off := uint64(512)
		var end uint64
		for i, s := range seqs {
			off = b.txn(off, s, first, blk{int64(10 + i), fill(512, byte(i+1))})
			if i == 0 {
				end = off // the header covers only the first list
			}
		}
		return b, 512, end
	}
	for _, tc := range []struct {
		name string
		seqs []uint32
		want map[int64]byte
	}{
		{"equal", []uint32{10, 10}, map[int64]byte{10: 1, 11: 2}},
		{"plus one", []uint32{10, 11}, map[int64]byte{10: 1, 11: 2}},
		{"equal then plus one", []uint32{10, 10, 11}, map[int64]byte{10: 1, 11: 2, 12: 3}},
		{"plus two is stale", []uint32{10, 12}, map[int64]byte{10: 1}},
		{"lower is a previous lap", []uint32{10, 4}, map[int64]byte{10: 1}},
		{"stale after good", []uint32{10, 11, 4, 12}, map[int64]byte{10: 1, 11: 2}},
		// :1289-1298: a list at end with sequence 0 is a pre-sequence-number journal's,
		// and the read stops there.
		{"zero at end stops", []uint32{10, 0}, map[int64]byte{10: 1}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			b, start, end := build(tc.seqs...)
			ov, err := b.replay(start, end, tVolume)
			if err != nil {
				t.Fatal(err)
			}
			wantBlocks(t, ov, tc.want)
		})
	}
	t.Run("out of order inside end stops the read", func(t *testing.T) {
		b := newJB(tSize, 512, 512)
		n := b.txn(512, 10, first, blk{10, fill(512, 1)})
		end := b.txn(n, 40, first, blk{11, fill(512, 2)})
		ov, err := b.replay(512, end, tVolume)
		if err != nil {
			t.Fatal(err)
		}
		// :1273-1280: check_past_jnl_end is still set, so the kernel treats the list as
		// the end of the journal and keeps what came before it.
		wantBlocks(t, ov, map[int64]byte{10: 1})
	})
}

// Everything the header and the lists claim is bounded (security).
func TestReplayBounds(t *testing.T) {
	good := func() (*jbuilder, uint64) {
		b := newJB(tSize, 512, 512)
		return b, b.txn(512, 1, first, blk{10, fill(512, 1)})
	}
	poke := func(name string, mut func(b *jbuilder), h func(*journalHeader)) {
		t.Run(name, func(t *testing.T) {
			b, end := good()
			if mut != nil {
				mut(b)
				b.bo.PutUint32(b.buf[512+8:], 0)
				b.bo.PutUint32(b.buf[512+8:], journalChecksum(b.buf[512:512+blhdrChecksumSize]))
			}
			jh := b.header(512, end)
			if h != nil {
				h(&jh)
			}
			ov, err := replayJournal(bytesReaderAt(b.buf), 0, jh, tVolume)
			if !errors.Is(err, ErrJournaled) && !errors.Is(err, ErrJournalReplay) {
				t.Fatalf("err = %v (overlay %v), want a refusal", err, ov != nil)
			}
		})
	}
	le := binary.LittleEndian
	poke("bytes_used below blhdr_size", func(b *jbuilder) { le.PutUint32(b.buf[512+4:], 100) }, nil)
	poke("bytes_used negative", func(b *jbuilder) { le.PutUint32(b.buf[512+4:], 0xffffff00) }, nil)
	poke("bytes_used beyond the buffer", func(b *jbuilder) { le.PutUint32(b.buf[512+4:], tSize) }, nil)
	poke("num_blocks > max_blocks", func(b *jbuilder) { le.PutUint16(b.buf[512+2:], 40) }, nil)
	poke("num_blocks zero", func(b *jbuilder) { le.PutUint16(b.buf[512+2:], 0) }, nil)
	poke("max_blocks zero", func(b *jbuilder) { le.PutUint16(b.buf[512+0:], 0) }, nil)
	poke("max_blocks beyond size/jhdr_size", func(b *jbuilder) { le.PutUint16(b.buf[512+0:], 33) }, nil)
	poke("entries beyond blhdr_size", func(b *jbuilder) { le.PutUint16(b.buf[512+0:], 32); le.PutUint16(b.buf[512+2:], 32) }, nil)
	poke("bnum negative but not -1", func(b *jbuilder) { le.PutUint64(b.buf[512+32:], ^uint64(1)) }, nil)
	poke("bnum * jhdr_size overflows 64 bits", func(b *jbuilder) { le.PutUint64(b.buf[512+32:], 1<<56) }, nil)
	poke("bnum * jhdr_size overflows 32 bits", func(b *jbuilder) { le.PutUint64(b.buf[512+32:], 1<<23) }, nil)
	poke("bnum + bsize beyond the volume", func(b *jbuilder) { le.PutUint64(b.buf[512+32:], tVolume/512) }, nil)
	poke("bsize not a multiple of jhdr_size", func(b *jbuilder) { le.PutUint32(b.buf[512+40:], 100) }, nil)
	poke("bsize beyond the buffer", func(b *jbuilder) { le.PutUint32(b.buf[512+40:], 1<<31) }, nil)
	poke("blhdr_size zero", nil, func(h *journalHeader) { h.blhdrSize = 0 })
	poke("blhdr_size not a multiple", nil, func(h *journalHeader) { h.blhdrSize = 700 })
	poke("blhdr_size beyond the buffer", nil, func(h *journalHeader) { h.blhdrSize = 1 << 30 })
	poke("jhdr_size zero", nil, func(h *journalHeader) { h.jhdrSize = 0 })
	poke("jhdr_size huge", nil, func(h *journalHeader) { h.jhdrSize = 1 << 31 })
	poke("size not a multiple of jhdr_size", nil, func(h *journalHeader) { h.size = tSize + 8 })
	poke("start below jhdr_size", nil, func(h *journalHeader) { h.start = 8 })
	poke("end beyond size", nil, func(h *journalHeader) { h.end = tSize + 512 })
}

// The same chain of lists, read to the end of a large pass, never holds more
// than the journal's size and terminates: a journal in which every list is
// valid and points back at itself.
func TestReplayCyclicJournalTerminates(t *testing.T) {
	b := newJB(tSize, 512, 512)
	// One list whose bytes_used is exactly one sector: its successor is at the
	// next sector, and the buffer is full of copies with the same sequence number.
	for off := uint64(512); off < tSize; off += 512 {
		b.txn(off, 3, first)
	}
	ov, err := b.replay(512, 1024, tVolume)
	if err != nil {
		t.Fatal(err)
	}
	if ov.bytes() > tSize {
		t.Errorf("overlay %d bytes, journal %d", ov.bytes(), tSize)
	}
}

// journalWithTxn returns a journaled image whose journal holds one transaction
// that rewrites the volume header's free block count, and the count it writes.
func journalWithTxn(t testing.TB) (img []byte, want uint32) {
	t.Helper()
	const size = 0x80000
	h := buildJournalHeader(binary.LittleEndian, 512, 512, size, false)
	img, _, jBlock := journaledImage(t, kJIJournalInFSMask, h, size)
	jbase := uint64(jBlock) * 4096
	vhSector := bytes.Clone(img[volumeHeaderOffset : volumeHeaderOffset+512])
	want = 7777
	binary.BigEndian.PutUint32(vhSector[0x30:], want) // freeBlocks
	b := &jbuilder{bo: binary.LittleEndian, size: size, jh: 512, bl: 0x2000, buf: img[jbase : jbase+size]}
	end := b.txn(512, 1, first, blk{volumeHeaderOffset / 512, vhSector})
	copy(img[jbase:], buildJournalHeader(binary.LittleEndian, 512, end, size, false))
	return img, want
}

func TestOpenWithJournalReplay(t *testing.T) {
	img, want := journalWithTxn(t)
	before := bytes.Clone(img)

	plain, err := Open(bytesReaderAt(img), int64(len(img)))
	if err != nil {
		t.Fatal(err)
	}
	if plain.vh.FreeBlocks == want {
		t.Fatalf("test image already has file count %d", want)
	}
	v, err := Open(bytesReaderAt(img), int64(len(img)), WithJournalReplay())
	if err != nil {
		t.Fatal(err)
	}
	if v.vh.FreeBlocks != want {
		t.Errorf("replayed file count = %d, want %d", v.vh.FreeBlocks, want)
	}
	if err := v.WriteFile("/x", []byte("y"), 0o644); !errors.Is(err, ErrReadOnly) {
		t.Errorf("write on a replayed volume: %v, want ErrReadOnly", err)
	}
	if !bytes.Equal(img, before) {
		t.Error("Open with replay modified the image")
	}
	// The write path is unchanged: still refused.
	if _, err := OpenWritable(img, nil); !errors.Is(err, ErrJournaled) {
		t.Errorf("OpenWritable: %v, want ErrJournaled", err)
	}
	// Size unknown: replay cannot bound its reads.
	if _, err := Open(bytesReaderAt(img), -1, WithJournalReplay()); !errors.Is(err, ErrJournaled) {
		t.Errorf("size -1: %v, want ErrJournaled", err)
	}
	// An empty journal, or none: the option changes nothing.
	empty, _, _ := journaledImage(t, kJIJournalInFSMask, buildJournalHeader(binary.LittleEndian, 512, 512, 0x80000, false), 0x80000)
	if _, err := Open(bytesReaderAt(empty), int64(len(empty)), WithJournalReplay()); err != nil {
		t.Errorf("empty journal: %v", err)
	}
}

func TestOpenFileWithJournalReplay(t *testing.T) {
	img, want := journalWithTxn(t)
	path := filepath.Join(t.TempDir(), "j.img")
	if err := os.WriteFile(path, img, 0o600); err != nil {
		t.Fatal(err)
	}
	v, err := OpenFile(path, WithJournalReplay())
	if err != nil {
		t.Fatal(err)
	}
	defer v.Close()
	if v.vh.FreeBlocks != want {
		t.Errorf("file count %d, want %d", v.vh.FreeBlocks, want)
	}
}

// A journal that claims a gigabyte block in a small image is refused, and
// refused cheaply: under a 64 MiB memory limit (GOMEMLIMIT) it allocates a few
// KiB, not what the list claims.
func TestBombReplay(t *testing.T) {
	defer debug.SetMemoryLimit(debug.SetMemoryLimit(64 << 20))
	b := newJB(0x80000, 512, 0x2000)
	end := b.txn(512, 1, first, blk{10, fill(512, 1)})
	// Entry 1 claims 1 GiB.
	b.bo.PutUint32(b.buf[512+40:], 1<<30)
	b.bo.PutUint32(b.buf[512+8:], 0)
	b.bo.PutUint32(b.buf[512+8:], journalChecksum(b.buf[512:512+blhdrChecksumSize]))
	n := allocated(func() {
		if _, err := b.replay(512, end, tVolume); !errors.Is(err, ErrJournalReplay) {
			t.Errorf("err = %v, want ErrJournalReplay", err)
		}
	})
	if n > 1<<20 {
		t.Errorf("refusal allocated %d bytes, want < 1 MiB", n)
	}

	// Many entries, each within bounds: the replay holds what the journal holds.
	b = newJB(0x80000, 512, 0x2000)
	var blks []blk
	for i := 0; i < 500; i++ {
		blks = append(blks, blk{int64(i), fill(512, 1)})
	}
	b.txn(512, 1, first, blks...)
	var m1, m2 runtime.MemStats
	runtime.ReadMemStats(&m1)
	ov, err := b.replay(512, 512+0x2000+500*512, tVolume)
	runtime.ReadMemStats(&m2)
	if err != nil {
		t.Fatal(err)
	}
	if ov.bytes() > 0x80000 {
		t.Errorf("overlay %d bytes, journal 0x80000", ov.bytes())
	}
	if m2.TotalAlloc-m1.TotalAlloc > 4<<20 {
		t.Errorf("allocated %d bytes for a 512 KiB journal", m2.TotalAlloc-m1.TotalAlloc)
	}
}

func FuzzJournalReplay(f *testing.F) {
	for _, s := range replaySeeds() {
		f.Add(s.data, s.start, s.end, s.blhdr, s.sel, s.big)
	}
	f.Fuzz(func(t *testing.T, data []byte, start, end uint64, blhdr uint32, sel uint8, big bool) {
		jh := uint32(512) << (sel % 3)
		size := uint64(len(data)) / uint64(jh) * uint64(jh)
		if size < 2*uint64(jh) {
			return
		}
		var bo binary.ByteOrder = binary.LittleEndian
		if big {
			bo = binary.BigEndian
		}
		h := journalHeader{bo: bo, start: start, end: end, size: size, blhdrSize: blhdr, jhdrSize: jh}
		vol := size * uint64(1+sel/3%4)
		ov, err := replayJournal(bytesReaderAt(data[:size]), 0, h, vol)
		if err != nil {
			if !errors.Is(err, ErrJournalReplay) {
				t.Fatalf("unexpected error type: %v", err)
			}
			return
		}
		if ov.bytes() > size {
			t.Fatalf("overlay %d bytes, journal %d", ov.bytes(), size)
		}
		for u := range ov.units {
			if (u+1)*ov.unit > vol {
				t.Fatalf("unit %d outside the volume of %d", u, vol)
			}
		}
		ov2, err := replayJournal(bytesReaderAt(data[:size]), 0, h, vol)
		if err != nil || len(ov2.units) != len(ov.units) {
			t.Fatalf("replay is not deterministic: %v", err)
		}
	})
}

type replaySeed struct {
	name       string
	data       []byte
	start, end uint64
	blhdr      uint32
	sel        uint8
	big        bool
}

// replaySeeds are the fuzz seeds: hand-built journals (wrapped, torn, stale), and
// the journal header macOS wrote (macJournalHeader, an empty journal at
// start == end) over an otherwise empty buffer.
func replaySeeds() []replaySeed {
	var out []replaySeed
	add := func(name string, b *jbuilder, start, end uint64) {
		out = append(out, replaySeed{name, bytes.Clone(b.buf), start, end, uint32(b.bl), 0, false})
	}
	b := newJB(tSize, 512, 512)
	n := b.txn(512, 7, first, blk{10, fill(512, 1)}, blk{11, fill(1024, 2)})
	end := b.txn(n, 8, first, blk{10, fill(512, 3)}, blk{-1, fill(512, 9)})
	add("two-transactions", b, 512, end)

	b = newJB(4*tSize, 512, 1024)
	end = b.txn(4*tSize-512, 5, first, blk{20, fill(1024, 0xa1)})
	add("split-header", b, 4*tSize-512, end)

	b = newJB(tSize, 512, 512)
	end = b.txn(tSize-1024, 5, first, blk{30, fill(1024, 0xb2)})
	add("split-block", b, tSize-1024, end)

	b = newJB(tSize, 512, 512)
	b.txn(tSize-1024, 5, first, blk{40, fill(512, 0xc3)})
	add("end-is-size", b, tSize-1024, tSize)

	b = newJB(tSize, 512, 512)
	n = b.txn(512, 1, first, blk{10, fill(512, 1)})
	b.txn(n, 2, first, blk{11, fill(512, 2)})
	b.buf[n+512+7] ^= 0xff
	add("torn-block-past-end", b, 512, n)

	b = newJB(tSize, 512, 512)
	n = b.txn(512, 10, first, blk{10, fill(512, 1)})
	b.txn(n, 4, first, blk{11, fill(512, 2)})
	add("stale-previous-lap", b, 512, n)

	b = newJB(tSize, 512, 512)
	n = b.txn(512, 10, first, blk{10, fill(512, 1)})
	n = b.txn(n, 10, first, blk{11, fill(512, 2)})
	b.txn(n, 11, first, blk{12, fill(512, 3)})
	add("sequence-equal-and-plus-one", b, 512, 1024)

	mac := make([]byte, tSize)
	copy(mac, macJournalHeader)
	out = append(out, replaySeed{"macos-empty-header", mac, 0x4400 % tSize, 0x4400 % tSize, 512, 0, false})
	return out
}

// TestWriteReplayCorpus writes the seeds as the committed corpus of
// FuzzJournalReplay when HFSPLUS_REGEN_CORPUS=1; otherwise it checks that every
// seed is on disk and still replays.
func TestWriteReplayCorpus(t *testing.T) {
	dir := filepath.Join("testdata", "fuzz", "FuzzJournalReplay")
	regen := os.Getenv("HFSPLUS_REGEN_CORPUS") == "1"
	if regen {
		if err := os.MkdirAll(dir, 0o755); err != nil {
			t.Fatal(err)
		}
	}
	for _, s := range replaySeeds() {
		body := fmt.Sprintf("go test fuzz v1\n[]byte(%s)\nuint64(%d)\nuint64(%d)\nuint32(%d)\nbyte(%d)\nbool(%v)\n",
			strconv.Quote(string(s.data)), s.start, s.end, s.blhdr, s.sel, s.big)
		path := filepath.Join(dir, s.name)
		if regen {
			if err := os.WriteFile(path, []byte(body), 0o644); err != nil {
				t.Fatal(err)
			}
			continue
		}
		got, err := os.ReadFile(path)
		if err != nil || string(got) != body {
			t.Errorf("%s: corpus file is missing or stale (run with HFSPLUS_REGEN_CORPUS=1): %v", path, err)
		}
	}
}

// BenchmarkReplayThroughput replays a journal of 4 KiB-block lists (blhdr_size
// 8 KiB, 4 KiB blocks, 100 blocks per list) filling a 64 MiB buffer.
func BenchmarkReplayThroughput(b *testing.B) {
	const size, jh, bl = 64 << 20, 512, 0x2000
	jb := newJB(size, jh, bl)
	off, seq := uint64(jh), uint32(1)
	var vol uint64 = 256 << 20
	blkData := fill(4096, 0x77)
	for {
		var blks []blk
		for i := 0; i < 100; i++ {
			blks = append(blks, blk{int64((uint64(seq)*100 + uint64(i)) % (vol / 4096) * 8), blkData})
		}
		if off+bl+100*4096 > size-jh {
			break
		}
		off = jb.txn(off, seq, first, blks...)
		seq++
	}
	b.SetBytes(int64(off))
	b.ReportAllocs()
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		if _, err := jb.replay(512, off, vol); err != nil {
			b.Fatal(err)
		}
	}
}
