// Copyright (c) 2026, go-filesystems
// SPDX-License-Identifier: BSD-3-Clause

package hfsplus

import (
	"bytes"
	"encoding/binary"
	"slices"
	"strconv"
	"testing"
)

// The ring scrub (hfsplus#58, design section 11 risk 5): the guard of #57 keeps new
// forged block list headers out of the ring, scrubRing removes the ones already
// there, once per session, before the first journaled commit writes anything.

const scrubRingSize = 0x40000

// scrubTxnStart is where journalImage's empty journal ends, so where T_1 goes.
const scrubTxnStart = 0x4400

// plantRing writes sector s at ring offset off of img (the ring is at jbase).
func plantRing(img []byte, jbase int64, off int, s []byte) {
	copy(img[int(jbase)+off:], s)
}

// scrubEnds runs n MkDir commits on a copy of img with the scrub as it is and
// returns end(T_1..T_n), ring-relative: where a later transaction ends, so where a
// sector must lie to be read past end.
func scrubEnds(t *testing.T, img []byte, n int) []int {
	t.Helper()
	v, _, _ := jwFile(t, bytes.Clone(img), true)
	defer v.Close()
	var ends []int
	for _, op := range mkdirOps(n) {
		if err := op(v); err != nil {
			t.Fatal(err)
		}
		ends = append(ends, int(v.jw.prevEnd))
	}
	return ends
}

// Sectors that verify, in either byte order, wherever they lie in the ring (first
// sector, a chunk edge, the last sector), are zeroed before the first commit, and
// that costs one barrier. Nothing else in the ring changes but T_1. Mutation
// targets: the scrub removed; its barrier removed; verifiesAsBlockListHeader
// reading one byte order.
func TestRingScrubZeroesForgedSectors(t *testing.T) {
	img, jbase := journalImage(t, scrubRingSize, 100, scrubTxnStart)
	planted := []int{512, scrubChunk - 512, scrubChunk, 0x9000, scrubRingSize - 512}
	for i, off := range planted {
		bo := binary.ByteOrder(binary.LittleEndian)
		if i%2 == 1 {
			bo = binary.BigEndian
		}
		plantRing(img, jbase, off, forgedHeader(bo, uint32(i)))
	}
	// A sector that does not verify stays as it is.
	plantRing(img, jbase, 0xA000, fill(512, 0x5a))
	orig := bytes.Clone(img)
	v, c, path := jwFile(t, img, true)
	defer v.Close()
	var counts []int
	for _, n := range []string{"/a", "/b", "/c"} {
		before := c.syncs
		if err := v.MkDir(n, 0o755); err != nil {
			t.Fatal(err)
		}
		counts = append(counts, c.syncs-before)
	}
	if !slices.Equal(counts, []int{3, 1, 1}) {
		t.Errorf("barriers per commit %v, want [3 1 1] (the scrub's barrier, then 2, 1, 1)", counts)
	}
	disk := mustRead(t, path)
	for _, off := range planted {
		if got := disk[int(jbase)+off : int(jbase)+off+512]; !bytes.Equal(got, make([]byte, 512)) {
			t.Errorf("the sector at ring offset %#x was not zeroed", off)
		}
	}
	if got := disk[int(jbase)+0xA000 : int(jbase)+0xA200]; !bytes.Equal(got, fill(512, 0x5a)) {
		t.Error("a sector that does not verify was changed")
	}
	// Outside the header sector and T_1..T_3, the ring is what it was, but for the
	// zeroed sectors.
	end := int(v.jw.prevEnd)
	for off := 512; off < scrubRingSize; off += 512 {
		if off >= scrubTxnStart && off < end {
			continue
		}
		want := orig[int(jbase)+off : int(jbase)+off+512]
		if slices.Contains(planted, off) {
			want = make([]byte, 512)
		}
		if !bytes.Equal(disk[int(jbase)+off:int(jbase)+off+512], want) {
			t.Fatalf("the ring sector at %#x changed", off)
		}
	}
}

// A ring with nothing to zero: no scrub barrier, and the scan writes nothing.
func TestRingScrubNothingToZero(t *testing.T) {
	img, jbase := journalImage(t, scrubRingSize, 100, scrubTxnStart)
	v, c, _ := jwFile(t, img, true)
	defer v.Close()
	writes := 0
	var counts []int
	for _, n := range []string{"/a", "/b", "/c"} {
		before, ev := c.syncs, len(c.events)
		if err := v.MkDir(n, 0o755); err != nil {
			t.Fatal(err)
		}
		counts = append(counts, c.syncs-before)
		for _, e := range c.events[ev:] {
			if !e.sync && e.off >= jbase+512 && e.off < jbase+scrubRingSize && e.off < jbase+scrubTxnStart {
				writes++ // below T_1: only a scrub could write there
			}
		}
	}
	if !slices.Equal(counts, []int{2, 1, 1}) {
		t.Errorf("barriers per commit %v, want [2 1 1]", counts)
	}
	if writes != 0 {
		t.Errorf("%d writes below T_1 in a ring with nothing to zero", writes)
	}
}

// The scan is lazy: a session whose commits all take the direct path (here, one with
// file data) never reads the ring and leaves a planted sector alone, at no cost.
func TestRingScrubIsLazy(t *testing.T) {
	img, jbase := journalImage(t, scrubRingSize, 100, scrubTxnStart)
	plantRing(img, jbase, 0x9000, forgedHeader(binary.LittleEndian, 0))
	v, _, path := jwFile(t, img, true)
	defer v.Close()
	// A commit the journal cannot take (a forged sector) goes the direct path.
	if err := plantForgedMkDir(v, "/f", binary.LittleEndian); err != nil {
		t.Fatal(err)
	}
	if v.jw.scrubbed {
		t.Error("the scrub ran on a commit that took the direct path")
	}
	if got := mustRead(t, path)[int(jbase)+0x9000 : int(jbase)+0x9200]; !bytes.Equal(got, forgedHeader(binary.LittleEndian, 0)) {
		t.Error("the planted sector was touched before any journaled commit")
	}
	if err := v.MkDir("/a", 0o755); err != nil {
		t.Fatal(err)
	}
	if !v.jw.scrubbed {
		t.Error("the first journaled commit did not scrub")
	}
	if got := mustRead(t, path)[int(jbase)+0x9000 : int(jbase)+0x9200]; !bytes.Equal(got, make([]byte, 512)) {
		t.Error("the planted sector survived the first journaled commit")
	}
}

// The proof: a forged sector where T_2 ends is followed by the replayer after a crash
// when the scrub does not run (the seam skips it), and is not when it runs. The image
// is the one a crash after the second commit leaves (journal header H_2 = T_1, T_2
// read past end, the forged list right behind it).
func TestRingScrubClosesTheGap(t *testing.T) {
	base, jbase := journalImage(t, scrubRingSize, 100, scrubTxnStart)
	ends := scrubEnds(t, base, 2)
	for _, scrub := range []bool{false, true} {
		t.Run("scrub="+strconv.FormatBool(scrub), func(t *testing.T) {
			img := bytes.Clone(base)
			plantRing(img, jbase, ends[1], forgedHeader(binary.LittleEndian, 0))
			plantRing(img, jbase, ends[1]+512, fill(512, 0xEE))
			v, _, path := jwFile(t, img, true)
			defer v.Close()
			v.jw.scrubbed = !scrub
			for _, op := range mkdirOps(2) {
				if err := op(v); err != nil {
					t.Fatal(err)
				}
			}
			disk := mustRead(t, path)
			followed := false
			rv, err := Open(bytesReaderAt(disk), int64(len(disk)), WithJournalReplay())
			if err != nil {
				followed = true // the forged entry overwrote the volume header
			} else {
				hdr := make([]byte, 512)
				if _, err := rv.rs.ReadAt(hdr, volumeHeaderOffset); err != nil {
					t.Fatal(err)
				}
				followed = !bytes.Equal(hdr, disk[volumeHeaderOffset:volumeHeaderOffset+512])
			}
			if scrub {
				if followed {
					t.Fatal("replay followed the forged list although the ring was scrubbed")
				}
				if names, _ := rootNames(rv); !slices.Equal(names, []string{"d000", "d001"}) {
					t.Errorf("replayed volume shows %v", names)
				}
			} else if !followed {
				t.Fatal("without the scrub replay did not follow the forged list: the test proves nothing")
			}
		})
	}
}

// The crash oracle with forged sectors in the ring: where T_1 starts (the zero write
// and T_1's header write go to the same sector, so only the scrub's barrier orders
// them: the oracle also runs a medium that lands each barrier group in reverse), where
// T_1, T_2 and T_3 end, and a big-endian one. A crash anywhere, inside the scrub and
// after it, replays to a state between the commits that returned and those that began.
func TestJournalWriterCrashOracleRingScrub(t *testing.T) {
	base, jbase := journalImage(t, scrubRingSize, 100, scrubTxnStart)
	ends := scrubEnds(t, base, 3)
	img := bytes.Clone(base)
	plantRing(img, jbase, scrubTxnStart, forgedHeader(binary.LittleEndian, 0))
	plantRing(img, jbase, ends[0], forgedHeader(binary.LittleEndian, 0))
	plantRing(img, jbase, ends[1], forgedHeader(binary.LittleEndian, 0))
	plantRing(img, jbase, ends[2], forgedHeader(binary.BigEndian, 0))
	plantRing(img, jbase, 0x20000, forgedHeader(binary.LittleEndian, 0))
	for _, reverse := range []bool{false, true} {
		checkJWScenario(t, jwScenario{name: "ring-scrub reverse=" + strconv.FormatBool(reverse), img: bytes.Clone(img), jbase: jbase, ops: mkdirOps(4), reverse: reverse})
	}
}

// The scrub starts at jhdr_size: the journal header sector, offset 0 of the journal,
// is never written, even when its bytes verify as a block list header (a journal
// header whose start field happens to equal the sum). A short read is an error that
// names the cause. scrubRing is driven directly: the ring sits at the end of a
// 1 MiB volume's free space, a 256 KiB journal with its header forged.
func TestRingScrubNeverWritesTheJournalHeader(t *testing.T) {
	img, err := Mkfs(8<<20, FormatConfig{Label: "SCRUB"})
	if err != nil {
		t.Fatal(err)
	}
	const size, off = 0x40000, 8<<20 - 0x40000 - 8192
	plantRing(img, off, 0, forgedHeader(binary.LittleEndian, 0))
	plantRing(img, off, 512, forgedHeader(binary.LittleEndian, 0))
	v, err := OpenWritable(img, nil)
	if err != nil {
		t.Fatal(err)
	}
	jw := &journalWriter{loc: journalLoc{off: off, hdr: journalHeader{size: size, jhdrSize: jwBlockSize}}}
	if err := jw.scrubRing(v); err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(img[off:off+512], forgedHeader(binary.LittleEndian, 0)) {
		t.Error("the scrub wrote the journal header sector")
	}
	if !bytes.Equal(img[off+512:off+1024], make([]byte, 512)) {
		t.Error("the sector at ring offset 512 was not zeroed")
	}
	// A ring that runs past the end of the volume: the read is short, with an error.
	jw.loc.off = uint64(len(img)) - 4096
	if err := jw.scrubRing(v); err == nil {
		t.Error("a ring past the end of the volume scrubbed without error")
	}
}

// The scan's memory is one chunk, whatever the ring's size: no allocation grows with
// it (the benchmark reports allocs/op on an 8 MiB ring).
func BenchmarkRingScrub(b *testing.B) {
	const size = 8 << 20
	// An 8 MiB ring does not fit the 8 MiB test volume: scrubRing needs only a volume
	// to read and a journal location, so the ring sits in a 16 MiB one, filled with
	// 0x5a (no sector verifies, the common case: the scan reads and writes nothing).
	img, err := Mkfs(16<<20, FormatConfig{Label: "BENCH"})
	if err != nil {
		b.Fatal(err)
	}
	const off = 16<<20 - size - 8192 // clear of the catalog at the start and the alternate header
	copy(img[off:off+size], fill(size, 0x5a))
	v, err := OpenWritable(img, nil)
	if err != nil {
		b.Fatal(err)
	}
	jw := &journalWriter{loc: journalLoc{off: off, hdr: journalHeader{size: size, jhdrSize: jwBlockSize}}}
	b.ReportAllocs()
	b.SetBytes(size)
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		if err := jw.scrubRing(v); err != nil {
			b.Fatal(err)
		}
	}
}
