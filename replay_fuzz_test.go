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
	"strconv"
	"testing"
)

// snapOffset maps any value to a multiple of unit in [unit, size].
func snapOffset(v, size, unit uint64) uint64 {
	n := size / unit
	if n == 0 {
		return unit
	}
	return unit * (1 + v%n)
}

// resignLists makes a mutated journal reach the checks behind the checksums. It
// walks the lists from h.start the way the replayer does (but only along lists
// that do not wrap, and never more than size/jhdr_size of them) and, per mode,
// recomputes the block checksums of each list (bit 1) and then the list header's
// own (bit 0). A fuzzer that mutates bytes would otherwise die at the first
// list-header checksum (a 32-bit value over 32 bytes) and never exercise
// bytes_used, num_blocks, bnum or bsize.
func resignLists(data []byte, h journalHeader, mode uint8) {
	jh, size, bl := uint64(h.jhdrSize), h.size, uint64(h.blhdrSize)
	if mode&3 == 0 || jh == 0 || bl < 32 || bl%jh != 0 || bl > size || uint64(len(data)) < size {
		return
	}
	bo := h.bo
	off := h.start
	if off == size {
		off = jh
	}
	for n := uint64(0); n < size/jh; n++ {
		if off < jh || off+bl > size {
			return
		}
		hb := data[off : off+bl]
		num := uint64(bo.Uint16(hb[2:4]))
		used := int32(bo.Uint32(hb[4:8]))
		if mode&2 != 0 {
			blockOff := off + bl
			for k := uint64(1); k < num && 16+16*k+16 <= bl; k++ {
				e := hb[16+16*k:]
				bn, bs := int64(bo.Uint64(e[0:8])), uint64(bo.Uint32(e[8:12]))
				if bs > 0 && blockOff+bs <= size {
					if bn != -1 {
						bo.PutUint32(e[12:], journalChecksum(data[blockOff:blockOff+bs]))
					}
					blockOff += bs
				}
			}
		}
		if mode&1 != 0 {
			clear(hb[8:12])
			bo.PutUint32(hb[8:], journalChecksum(hb[:blhdrChecksumSize]))
		}
		if used < int32(bl) {
			return
		}
		off += uint64(used)
		if off >= size {
			return
		}
	}
}

// The Open path: a journal inside a real journaled image, replayed, then the
// volume read (ReadAt over the overlay, ListDir, ReadFile). The journal bytes,
// header fields and fix mode are fuzzed; the rest of the image is fixed.
func FuzzOpenJournalReplay(f *testing.F) {
	const jsize = 0x80000
	img, _ := journalWithTxn(f)
	vh, err := readVolumeHeader(bytesReaderAt(img))
	if err != nil {
		f.Fatal(err)
	}
	jbase := int64(vh.TotalBlocks-140) * int64(vh.BlockSize)
	base := bytes.Clone(img)
	clear(base[jbase : jbase+jsize])

	for _, s := range openFuzzSeeds(f, img[jbase:jbase+jsize]) {
		f.Add(s.data, s.start, s.end, s.blhdr, s.big, s.fix)
	}
	f.Fuzz(func(t *testing.T, data []byte, start, end uint64, blhdr uint32, big bool, fix uint8) {
		var bo binary.ByteOrder = binary.LittleEndian
		if big {
			bo = binary.BigEndian
		}
		jrnl := make([]byte, jsize)
		copy(jrnl, data[:min(len(data), jsize)])
		if fix&4 != 0 {
			start, end = snapOffset(start, jsize, 512), snapOffset(end, jsize, 512)
		}
		// start == end is an empty journal: nothing to replay, nothing to look at.
		hdr := buildJournalHeader(bo, start, end, jsize, false)
		bo.PutUint32(hdr[32:], blhdr)
		resignJournalHeader(hdr)
		copy(jrnl, hdr)
		resignLists(jrnl, journalHeader{bo: bo, start: start, end: end, size: jsize, blhdrSize: blhdr, jhdrSize: 512}, fix)

		r := &patchedReader{base: bytesReaderAt(base), off: jbase, patch: jrnl}
		v, err := Open(r, int64(len(base)), WithJournalReplay())
		if err != nil {
			return
		}
		buf := make([]byte, 64<<10)
		for _, off := range []int64{0, 1024, 4096, int64(len(base)) - 1024} {
			if _, err := v.rs.ReadAt(buf, off); err != nil && !errors.Is(err, io.EOF) {
				t.Fatalf("ReadAt(%d): %v", off, err)
			}
		}
		if ents, err := v.ListDir("/"); err == nil {
			for _, e := range ents[:min(len(ents), 8)] {
				_, _ = v.ReadFile("/" + e.Name())
			}
		}
	})
}

// patchedReader serves patch at [off, off+len(patch)) over base.
type patchedReader struct {
	base  io.ReaderAt
	off   int64
	patch []byte
}

func (p *patchedReader) ReadAt(b []byte, off int64) (int, error) {
	n, err := p.base.ReadAt(b, off)
	lo, hi := max(off, p.off), min(off+int64(len(b)), p.off+int64(len(p.patch)))
	if lo < hi {
		copy(b[lo-off:hi-off], p.patch[lo-p.off:hi-p.off])
	}
	return n, err
}

type openFuzzSeed struct {
	name       string
	data       []byte
	start, end uint64
	blhdr      uint32
	big        bool
	fix        uint8
}

// openFuzzSeeds: the journal journalWithTxn builds (a valid transaction that
// rewrites the volume header), the same with a torn block, the same with the
// checksums re-signed after mutation (fix 7), and the journal macOS wrote.
func openFuzzSeeds(t testing.TB, jrnl []byte) []openFuzzSeed {
	good := bytes.Clone(jrnl[:512+0x2000+512+512])
	h, err := decodeJournalHeader(jrnl[:512])
	if err != nil {
		t.Fatal(err)
	}
	seeds := []openFuzzSeed{{"txn", good, h.start, h.end, 0x2000, false, 0}}
	torn := bytes.Clone(good)
	torn[512+0x2000+9] ^= 0xff
	seeds = append(seeds, openFuzzSeed{"txn-torn", torn, h.start, h.end, 0x2000, false, 0})
	seeds = append(seeds, openFuzzSeed{"txn-fix", good, h.start, h.end, 0x2000, false, 7})
	_, mj, mh := macNonEmptyFixture(t)
	seeds = append(seeds, openFuzzSeed{"macos", bytes.Clone(mj[:0x25000]), mh.start, mh.end, mh.blhdrSize, false, 0})
	return seeds
}

// TestWriteOpenReplayCorpus is TestWriteReplayCorpus for FuzzOpenJournalReplay.
func TestWriteOpenReplayCorpus(t *testing.T) {
	img, _ := journalWithTxn(t)
	vh, _ := readVolumeHeader(bytesReaderAt(img))
	jbase := int64(vh.TotalBlocks-140) * int64(vh.BlockSize)
	dir := filepath.Join("testdata", "fuzz", "FuzzOpenJournalReplay")
	regen := os.Getenv("HFSPLUS_REGEN_CORPUS") == "1"
	if regen {
		if err := os.MkdirAll(dir, 0o755); err != nil {
			t.Fatal(err)
		}
	}
	for _, s := range openFuzzSeeds(t, img[jbase:jbase+0x80000]) {
		if s.name == "macos" {
			continue // the 150 KiB fixture is added from code, not committed twice
		}
		body := fmt.Sprintf("go test fuzz v1\n[]byte(%s)\nuint64(%d)\nuint64(%d)\nuint32(%d)\nbool(%v)\nbyte(%d)\n",
			strconv.Quote(string(s.data)), s.start, s.end, s.blhdr, s.big, s.fix)
		path := filepath.Join(dir, s.name)
		if regen {
			if err := os.WriteFile(path, []byte(body), 0o644); err != nil {
				t.Fatal(err)
			}
			continue
		}
		// Only presence: mkfs stamps dates into the volume header the journal carries,
		// so the bytes differ on every run.
		if _, err := os.Stat(path); err != nil {
			t.Errorf("%s: corpus file is missing (run with HFSPLUS_REGEN_CORPUS=1): %v", path, err)
		}
		_ = body
	}
}
