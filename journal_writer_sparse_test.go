// Copyright (c) 2026, go-filesystems
// SPDX-License-Identifier: BSD-3-Clause

package hfsplus

import (
	"bytes"
	"encoding/binary"
	"fmt"
	"hash/crc32"
	"math/bits"
	"math/rand"
	"os"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"testing"
	"time"
)

// The crash oracle on a volume of 9.2 GiB, journal-full and the size bypass reached
// through the public API (hfsplus#64).
//
// Why a new oracle. The writer reaches journal-full only when three consecutive
// transactions together hold more than the ring (journal_writer.go plan: live +
// tsize >= size - jhdr_size; the smallest journal is 256 KiB, journal.go), and the
// size bypass only when one transaction is larger than the ring on its own
// (journal_writer.go plan, same test with live = 0; flushJournal returns done =
// false and store.go flush empties the journal and writes in place). A commit's
// metadata is a few catalog nodes (tens of KiB at most) and file data is held to
// (size - jhdr_size)/8 per commit (journal_data.go dataBound, journal_group.go
// absorb), so no ordinary commit gets there. The one natural commit that does is
// one that changes a long run of the allocation bitmap: freeing a file of N blocks
// rewrites N/8 bytes of bitmap, contiguous, one block-list entry. With 4096-byte
// blocks a transaction past 256 KiB needs about 8 GiB of blocks freed at once, and
// three journal-full commits in a row need about 2.7 GiB each.
//
// So the scenarios run on a 2.4 million block (9.2 GiB) volume held in a SPARSE file:
// Truncate to the size, and writes only where the formatter and the driver write.
// The big files are planted into the catalog and the bitmap without their data
// (a file of zeros is a file of holes), so the setup costs the catalog and bitmap
// and nothing else; the deleted files are then freed through DeleteFile.
//
// Disk and memory are bounded, not proportional to the volume. Every byte of the
// image that is not a hole lies in sparseBase.ranges: the head of the volume
// (header, bitmap, extents and catalog trees, about 1.3 MiB), the first 1 MiB after
// it (where later file data lands), the journal info block, the 256 KiB ring and the
// last block. A crash image is a file made by copying those ranges from a base file
// and applying a prefix of the write log to it, so each costs about 2.5 MiB of copy
// and no more than that of disk; one base file, one working copy, the durable and
// in-order images and one scratch image are alive at once (about 15 MiB of disk in
// all), and the memory is the write log (a few MiB) plus one copy buffer.

const (
	sparseBS       = 4096
	sparseBlocks   = 2_400_000 // 9.2 GiB
	sparseJournal  = 256 << 10 // the smallest journal there is
	sparseFullFile = 720_000   // blocks of each of the three files of the journal-full scenario (2.75 GiB)
	sparseBigFile  = 2_150_000 // blocks of the file of the bypass scenario (8.2 GiB)
	sparseArea     = 1 << 20   // the file data area after the head that is part of the ranges
)

// sparseRange is [off, off+n) of the image.
type sparseRange struct{ off, n int64 }

// sparseBase is a journaled volume held in a sparse file whose non-hole bytes all
// lie in ranges.
type sparseBase struct {
	path   string
	size   int64
	jbase  int64
	ranges []sparseRange
}

// plantBigFile adds a file of blocks allocation blocks to the root, in one commit,
// the way createFile does but without writing the data: the blocks are allocated and
// the file reads as zeros.
func plantBigFile(v *Volume, name string, blocks uint32) error {
	parent, nm, err := v.resolveParent("/" + name)
	if err != nil {
		return err
	}
	t, err := v.begin()
	if err != nil {
		return err
	}
	cnid := v.nextCNID()
	runs, err := t.alc.allocData(blocks)
	if err != nil {
		return err
	}
	if len(runs) > numInlineExtents {
		return fmt.Errorf("the planted file is in %d runs", len(runs))
	}
	var df forkData
	df.LogicalSize = uint64(blocks) * uint64(v.vh.BlockSize)
	df.ClumpSize = v.vh.BlockSize
	df.TotalBlocks = blocks
	copy(df.Extents[:], runs)
	rec := encodeFileRecord(cnid, timeNow(), modeFileDefault, df)
	if err := t.cw.insertRecord(assembleCatalogRecord(encodeCatalogKey(parent, nm), rec)); err != nil {
		return err
	}
	if err := t.cw.insertRecord(assembleCatalogRecord(encodeCatalogKey(cnid, ""), encodeFileThreadRecord(parent, nm))); err != nil {
		return err
	}
	v.incFileCount(1)
	if err := t.adjustParentValence(parent, +1, 0); err != nil {
		return err
	}
	v.bumpWriteCount()
	return t.commit()
}

// buildSparseBase formats a sparseBlocks-block volume into a sparse file without
// ever holding the volume: the formatter's builder runs over a slice that holds the
// head and the last block only (every other block of a fresh volume is zero), plants
// the files, then makes the volume journaled with an empty journal of sparseJournal
// bytes at the end of the volume.
func buildSparseBase(t *testing.T, planted map[string]uint32) *sparseBase {
	t.Helper()
	b := &mkfsBuilder{
		blockSize: sparseBS, totalBlocks: sparseBlocks, nodeSize: mkfsNodeSize,
		label: "SPARSE", now: time.Now(),
	}
	if err := b.layout(); err != nil {
		t.Fatal(err)
	}
	head := int64(b.catStart+b.catBlocks) * sparseBS
	b.img = make([]byte, head+sparseBS)
	b.writeAllocationBitmap()
	b.writeExtentsTree()
	b.writeCatalogTree()
	b.writeVolumeHeaders() // the alternate header lands in the last block of b.img

	path := filepath.Join(t.TempDir(), "base.img")
	f, err := os.Create(path)
	if err != nil {
		t.Fatal(err)
	}
	size := int64(sparseBlocks) * sparseBS
	if err := f.Truncate(size); err != nil {
		t.Fatal(err)
	}
	if _, err := f.WriteAt(b.img[:head], 0); err != nil {
		t.Fatal(err)
	}
	if _, err := f.WriteAt(b.img[head:], size-sparseBS); err != nil {
		t.Fatal(err)
	}
	if err := f.Close(); err != nil {
		t.Fatal(err)
	}
	base := &sparseBase{path: path, size: size, ranges: []sparseRange{
		{0, head}, {head, sparseArea}, {size - sparseBS, sparseBS},
	}}

	names := make([]string, 0, len(planted))
	for n := range planted {
		names = append(names, n)
	}
	slices.Sort(names)
	v, err := OpenFileWritable(path)
	if err != nil {
		t.Fatal(err)
	}
	for _, n := range names {
		if err := plantBigFile(v, n, planted[n]); err != nil {
			t.Fatal(err)
		}
	}
	if err := v.Close(); err != nil {
		t.Fatal(err)
	}
	base.journalize(t)
	return base
}

// journalize turns the volume into a journaled one with an empty journal of
// sparseJournal bytes in free blocks at its end (the layout journaledImage gives an
// in-memory image, and the layout of a real volume: both are blocks the bitmap
// owns), sequence number 100, ring start 0x4400.
func (base *sparseBase) journalize(t *testing.T) {
	t.Helper()
	f, err := os.OpenFile(base.path, os.O_RDWR, 0)
	if err != nil {
		t.Fatal(err)
	}
	defer f.Close()
	vh, err := readVolumeHeader(f)
	if err != nil {
		t.Fatal(err)
	}
	be, le := binary.BigEndian, binary.LittleEndian
	jBlocks := uint32(sparseJournal / sparseBS)
	jibBlock := vh.TotalBlocks - 2 - jBlocks - 4
	jBlock := jibBlock + 1
	jOff := int64(jBlock) * sparseBS
	base.jbase = jOff

	h := buildJournalHeader(le, 0x4400, 0x4400, sparseJournal, false)
	le.PutUint32(h[32:], 0x2000)
	le.PutUint32(h[44:], 100)
	resignJournalHeader(h)
	jib := make([]byte, 52)
	be.PutUint32(jib[0:], kJIJournalInFSMask)
	be.PutUint64(jib[36:], uint64(jOff))
	be.PutUint64(jib[44:], sparseJournal)
	for _, w := range []struct {
		p   []byte
		off int64
	}{{h, jOff}, {jib, int64(jibBlock) * sparseBS}} {
		if _, err := f.WriteAt(w.p, w.off); err != nil {
			t.Fatal(err)
		}
	}
	bmOff := int64(vh.AllocationFile.Extents[0].StartBlock) * sparseBS
	used := uint32(0)
	for _, r := range [][2]uint32{{jibBlock, 1}, {jBlock, jBlocks}} {
		for blk := r[0]; blk < r[0]+r[1]; blk++ {
			var c [1]byte
			if _, err := f.ReadAt(c[:], bmOff+int64(blk/8)); err != nil {
				t.Fatal(err)
			}
			if c[0]&(0x80>>(blk%8)) != 0 {
				t.Fatalf("block %d is already allocated", blk)
			}
			c[0] |= 0x80 >> (blk % 8)
			if _, err := f.WriteAt(c[:], bmOff+int64(blk/8)); err != nil {
				t.Fatal(err)
			}
			used++
		}
	}
	for _, off := range []int64{volumeHeaderOffset, base.size - 1024} {
		vhb := make([]byte, 512)
		if _, err := f.ReadAt(vhb, off); err != nil {
			t.Fatal(err)
		}
		be.PutUint32(vhb[4:], be.Uint32(vhb[4:])|kHFSVolumeJournaledBit)
		copy(vhb[8:12], "HFSJ")
		be.PutUint32(vhb[12:], jibBlock)
		be.PutUint32(vhb[0x30:], be.Uint32(vhb[0x30:])-used)
		if _, err := f.WriteAt(vhb, off); err != nil {
			t.Fatal(err)
		}
	}
	base.ranges = append(base.ranges, sparseRange{int64(jibBlock) * sparseBS, sparseBS}, sparseRange{jOff, sparseJournal})
}

// copyRanges makes dst (created, truncated to size) hold src's bytes in ranges and
// holes elsewhere. dst is overwritten range by range when it exists, so a scratch
// file is reused without growing: every write of a crash image lies in ranges.
func copyRanges(dst, src *os.File, ranges []sparseRange, buf []byte) error {
	for _, r := range ranges {
		for o := int64(0); o < r.n; {
			n := min(int64(len(buf)), r.n-o)
			if _, err := src.ReadAt(buf[:n], r.off+o); err != nil {
				return err
			}
			if _, err := dst.WriteAt(buf[:n], r.off+o); err != nil {
				return err
			}
			o += n
		}
	}
	return nil
}

func (base *sparseBase) inRanges(off, n int64) bool {
	for _, r := range base.ranges {
		if off >= r.off && off+n <= r.off+r.n {
			return true
		}
	}
	return false
}

// newImage creates a sparse file of the volume's size at path holding the base's
// ranges.
func (base *sparseBase) newImage(t *testing.T, path string) *os.File {
	t.Helper()
	src, err := os.Open(base.path)
	if err != nil {
		t.Fatal(err)
	}
	defer src.Close()
	dst, err := os.OpenFile(path, os.O_RDWR|os.O_CREATE|os.O_TRUNC, 0o644)
	if err != nil {
		t.Fatal(err)
	}
	if err := dst.Truncate(base.size); err != nil {
		t.Fatal(err)
	}
	if err := copyRanges(dst, src, base.ranges, make([]byte, 256<<10)); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { dst.Close() })
	return dst
}

// sparseObserve is what the oracle compares between states: the names in the root
// (a small file with its checksum, "c*": the data), the free block count of the
// header. The bitmap must agree with that count: a transaction that freed the
// blocks of a deleted file in the header but not in the bitmap, or the other way
// round, is a half transaction.
func sparseObserve(v *Volume) ([]string, error) {
	ents, err := v.ListDir("/")
	if err != nil {
		return nil, err
	}
	var out []string
	for _, e := range ents {
		n := e.Name()
		if strings.HasPrefix(n, "c") {
			data, err := v.ReadFile("/" + n)
			if err != nil {
				return nil, err
			}
			n += fmt.Sprintf(":%d:%08x", len(data), crc32.ChecksumIEEE(data))
		}
		out = append(out, n)
	}
	slices.Sort(out)
	raw := make([]byte, volumeHeaderOffset+512)
	if _, err := v.rs.ReadAt(raw[volumeHeaderOffset:], volumeHeaderOffset); err != nil {
		return nil, err
	}
	_, _, _, free := headerCounts(raw)
	bs := int64(v.vh.BlockSize)
	bm := make([]byte, (int64(v.vh.TotalBlocks)+7)/8)
	if _, err := v.rs.ReadAt(bm, int64(v.vh.AllocationFile.Extents[0].StartBlock)*bs); err != nil {
		return nil, err
	}
	used := 0
	for _, c := range bm {
		used += bits.OnesCount8(c)
	}
	if got := int(v.vh.TotalBlocks) - used; got != int(free) {
		return nil, fmt.Errorf("the header says %d free blocks, the bitmap %d", free, got)
	}
	return append(out, "free="+strconv.Itoa(int(free))), nil
}

// sparseCheckImage is jwCheckImage over a file: nil when the image at path, as a
// crash image, replays to the volume after j commits for some j in [lo, hi], with
// its tree and data readable and consistent. It rewrites path (the writable replay).
func sparseCheckImage(path string, lo, hi int, states [][]string, base consistency, jbase int64, direct bool) string {
	f, err := os.Open(path)
	if err != nil {
		return err.Error()
	}
	defer f.Close()
	st, err := f.Stat()
	if err != nil {
		return err.Error()
	}
	size := st.Size()
	vh, err := readVolumeHeader(f)
	if err != nil {
		return "volume header: " + err.Error()
	}
	if direct && vh.Attributes&kHFSVolumeJournaledBit == 0 {
		var hb [512]byte
		if _, err := f.ReadAt(hb[:], jbase); err != nil {
			return err.Error()
		}
		h, err := decodeJournalHeader(hb[:])
		if err != nil || h.start != h.end {
			return fmt.Sprintf("direct-path commit under way with a journal that is not empty: %+v, %v", h, err)
		}
		return ""
	}
	js, _, err := loadJournalAt(f, size, vh)
	if err != nil {
		return "journal: " + err.Error()
	}
	if !js.empty {
		var lm [4]byte
		if _, err := f.ReadAt(lm[:], vhLastMountedVersionOff); err != nil {
			return err.Error()
		}
		if string(lm[:]) != "HFSJ" {
			return fmt.Sprintf("transactions pending but lastMountedVersion is %q", lm[:])
		}
	}
	v, err := Open(f, size, WithJournalReplay())
	if err != nil {
		return "does not open with replay: " + err.Error()
	}
	names, err := sparseObserve(v)
	if err != nil {
		return "replayed volume unreadable: " + err.Error()
	}
	match := -1
	for j := lo; j <= hi && j < len(states); j++ {
		if slices.Equal(names, states[j]) {
			match = j
			break
		}
	}
	if match < 0 {
		return fmt.Sprintf("replays to %v, allowed states %d..%d (%v .. %v)", names, lo, hi, states[min(lo, len(states)-1)], states[min(hi, len(states)-1)])
	}
	if msg := jwConsistent(v, base); msg != "" {
		return "replayed volume inconsistent: " + msg
	}
	wv, err := OpenFileWritable(path)
	if err != nil {
		return "writable open (replay) refused: " + err.Error()
	}
	defer wv.Close()
	wnames, err := sparseObserve(wv)
	if err != nil || !slices.Equal(wnames, names) {
		return fmt.Sprintf("writable replay shows %v (%v), read replay %v", wnames, err, names)
	}
	return ""
}

// sparseOp is one commit of a scenario and what must hold right after it.
type sparseOp struct {
	do    jwOp
	after func(t *testing.T, v *Volume)
}

// sparseScenario is jwScenario over a sparse base.
type sparseScenario struct {
	name   string
	base   *sparseBase
	direct bool
	ops    []sparseOp
	// every is the stride of the crash points (an event prefix of the sector-split
	// log); a point next to a barrier, a header sector or a volume header is always
	// taken, and gets the open group's subsets and torn sectors too.
	every int
}

// sparseStats is what a run reports.
type sparseStats struct {
	events, points, images int
	commits                int
	elapsed                time.Duration
}

func runSparseScenario(t *testing.T, s sparseScenario) sparseStats {
	t.Helper()
	start := time.Now()
	dir := t.TempDir()
	work := filepath.Join(dir, "work.img")
	wf := s.base.newImage(t, work)
	wf.Close()

	bf, err := os.Open(s.base.path)
	if err != nil {
		t.Fatal(err)
	}
	defer bf.Close()
	bv, err := Open(bf, s.base.size)
	if err != nil {
		t.Fatal(err)
	}
	n0, err := sparseObserve(bv)
	if err != nil {
		t.Fatalf("the base volume: %v", err)
	}
	f0, d0, _, err := walkVolume(bv)
	if err != nil {
		t.Fatal(err)
	}
	raw := make([]byte, volumeHeaderOffset+512)
	bv.rs.ReadAt(raw[volumeHeaderOffset:], volumeHeaderOffset)
	hf, hd, _, _ := headerCounts(raw)
	cons := consistency{fileOff: int(hf) - f0, folderOff: int(hd) - d0}
	states := [][]string{n0}

	v, err := OpenFileWritable(work)
	if err != nil {
		t.Fatal(err)
	}
	defer v.Close()
	if v.jw == nil {
		t.Fatalf("journal writes are not on after open: %v", v.JournalWritesReason())
	}
	c := instrument(v)
	var starts, ends []int
	for k, op := range s.ops {
		starts = append(starts, len(c.events))
		if err := op.do(v); err != nil {
			t.Fatalf("commit %d: %v", k+1, err)
		}
		ends = append(ends, len(c.events))
		if op.after != nil {
			op.after(t, v)
		}
		names, err := sparseObserve(v)
		if err != nil {
			t.Fatalf("after commit %d: %v", k+1, err)
		}
		states = append(states, names)
	}
	sev, sstarts, sends := splitSectors(c.events, starts, ends)
	for _, e := range sev {
		if !e.sync && !s.base.inRanges(e.off, int64(len(e.data))) {
			t.Fatalf("a write at %d (+%d) lies outside the ranges the images hold", e.off, len(e.data))
		}
	}

	dur := s.base.newImage(t, filepath.Join(dir, "dur.img"))
	cur := s.base.newImage(t, filepath.Join(dir, "cur.img"))
	scratch := s.base.newImage(t, filepath.Join(dir, "scratch.img"))
	buf := make([]byte, 256<<10)
	rng := rand.New(rand.NewSource(1))
	st := sparseStats{events: len(sev), commits: len(s.ops)}
	var bad []string
	check := func(name string, src *os.File, lo, hi int, mutate func(img *os.File)) {
		if err := copyRanges(scratch, src, s.base.ranges, buf); err != nil {
			t.Fatal(err)
		}
		if mutate != nil {
			mutate(scratch)
		}
		st.images++
		if msg := sparseCheckImage(scratch.Name(), lo, hi, states, cons, s.base.jbase, s.direct); msg != "" {
			bad = append(bad, name+": "+msg)
		}
	}
	// near: a point next to a barrier, a journal header sector or a volume header.
	hdrSector := func(e crashEvent) bool {
		return !e.sync && (e.off/512 == s.base.jbase/512 || e.off/512 == volumeHeaderOffset/512 || e.off/512 == (s.base.size-1024)/512)
	}
	near := func(p int) bool {
		for _, q := range []int{p - 2, p - 1, p} {
			if q >= 0 && q < len(sev) && (sev[q].sync || hdrSector(sev[q])) {
				return true
			}
		}
		return false
	}
	durIdx := 0
	every := max(s.every, 1)
	for p := 0; p <= len(sev); p++ {
		if p > 0 {
			e := sev[p-1]
			if e.sync {
				for _, w := range sev[durIdx : p-1] {
					if !w.sync {
						dur.WriteAt(w.data, w.off)
					}
				}
				durIdx = p
			} else {
				cur.WriteAt(e.data, e.off)
			}
		}
		take := near(p) || p%every == 0
		if !take {
			continue
		}
		st.points++
		done, started := 0, 0
		for k := range sstarts {
			if sends[k] <= p {
				done++
			}
			if sstarts[k] < p {
				started++
			}
		}
		lo, hi := done, started
		check(fmt.Sprintf("fsynced-only@%d", p), dur, lo, hi, nil)
		check(fmt.Sprintf("in-order@%d", p), cur, lo, hi, nil)
		if !near(p) {
			continue
		}
		var group []crashEvent
		for _, w := range sev[durIdx:p] {
			if !w.sync {
				group = append(group, w)
			}
		}
		if len(group) == 0 {
			continue
		}
		// The writes of the open group that get their own subsets: the first and last
		// few, the header sectors, and every 64th.
		var pick []int
		for i := range group {
			if i < 3 || i >= len(group)-3 || hdrSector(group[i]) || i%64 == 0 {
				pick = append(pick, i)
			}
		}
		with := func(keep func(i int) bool, torn int) func(*os.File) {
			return func(img *os.File) {
				for i, w := range group {
					switch {
					case i == torn:
						img.WriteAt(w.data[:len(w.data)/2], w.off)
					case keep(i):
						img.WriteAt(w.data, w.off)
					}
				}
			}
		}
		for _, i := range pick {
			check(fmt.Sprintf("subset-one@%d.%d", p, i), dur, lo, hi, with(func(j int) bool { return j == i }, -1))
			check(fmt.Sprintf("subset-allbut@%d.%d", p, i), dur, lo, hi, with(func(j int) bool { return j != i }, -1))
			check(fmt.Sprintf("torn@%d.%d", p, i), dur, lo, hi, with(func(j int) bool { return j != i }, i))
		}
		for r := 0; r < 4; r++ {
			mask := make([]bool, len(group))
			for i := range mask {
				mask[i] = rng.Intn(2) == 0
			}
			check(fmt.Sprintf("subset-rand@%d.%d", p, r), dur, lo, hi, with(func(j int) bool { return mask[j] }, -1))
		}
	}
	st.elapsed = time.Since(start)
	t.Logf("%s: %d commits, %d sector events, %d crash points, %d crash images checked in %v", s.name, st.commits, st.events, st.points, st.images, st.elapsed.Round(time.Millisecond))
	if st.images < 500 {
		t.Fatalf("only %d crash images: the oracle is vacuous", st.images)
	}
	if len(bad) > 0 {
		for _, b := range bad[:min(len(bad), 8)] {
			t.Errorf("%s", b)
		}
		t.Fatalf("%d of %d crash images are wrong", len(bad), st.images)
	}
	return st
}

func skipSparse(t *testing.T) {
	t.Helper()
	if testing.Short() {
		t.Skip("short: the sparse-volume oracle takes tens of seconds")
	}
	if strconv.IntSize < 64 {
		t.Skip("the volume is 9.2 GiB, past a 32-bit address space")
	}
}

func rmOp(name string) jwOp {
	return func(v *Volume) error { return v.DeleteFile("/" + name) }
}

// Journal-full, reached naturally: three consecutive commits that each free a
// 2.75 GiB file rewrite 90000 bytes of bitmap, so each transaction is about 92 KiB,
// two fit the 256 KiB ring together and the third does not. The third empties the
// journal (2 barriers) and commits as a first commit (2 more).
func TestJournalWriterCrashOracleSparseFull(t *testing.T) {
	skipSparse(t)
	base := buildSparseBase(t, map[string]uint32{"big1": sparseFullFile, "big2": sparseFullFile, "big3": sparseFullFile})
	fulls := func(want int) func(t *testing.T, v *Volume) {
		return func(t *testing.T, v *Volume) {
			t.Helper()
			t.Logf("tsize %d, prev2 %d, ring %d", v.jw.prevSize, v.jw.prev2, v.jw.loc.hdr.size)
			if v.jw.fullCommits != want || !v.journaling() {
				t.Fatalf("fullCommits %d (want %d), journaling %v", v.jw.fullCommits, want, v.journaling())
			}
		}
	}
	runSparseScenario(t, sparseScenario{
		name: "sparse-full", base: base, every: 4,
		ops: []sparseOp{
			{do: rmOp("big1"), after: fulls(0)},
			{do: rmOp("big2"), after: fulls(0)},
			{do: rmOp("big3"), after: fulls(1)}, // T_3 found T_1 + T_2 in the ring
			{do: func(v *Volume) error { return v.WriteFile("/c1", bytes.Repeat([]byte("hfs+"), 3000), 0o644) }, after: fulls(1)},
			{do: func(v *Volume) error { return v.MkDir("/d1", 0o755) }, after: fulls(1)},
		},
	})
}

// The size bypass, reached naturally: freeing a 8.2 GiB file rewrites 268750 bytes
// of bitmap, a transaction past the ring on its own. The commit empties the journal
// and takes the direct path (store.go flush); the commits after it are first
// commits again.
func TestJournalWriterCrashOracleSparseBypass(t *testing.T) {
	skipSparse(t)
	base := buildSparseBase(t, map[string]uint32{"big": sparseBigFile})
	runSparseScenario(t, sparseScenario{
		name: "sparse-bypass", base: base, direct: true, every: 4,
		ops: []sparseOp{
			{do: func(v *Volume) error { return v.MkDir("/a", 0o755) }, after: func(t *testing.T, v *Volume) {
				if !v.journaling() {
					t.Fatal("the first commit was not journaled")
				}
			}},
			{do: rmOp("big"), after: func(t *testing.T, v *Volume) {
				if v.journaling() || v.jw.fullCommits != 0 {
					t.Fatalf("the big free was not a bypass: journaling %v, fullCommits %d", v.journaling(), v.jw.fullCommits)
				}
			}},
			{do: func(v *Volume) error { return v.WriteFile("/c1", bytes.Repeat([]byte("hfs+"), 3000), 0o644) }, after: func(t *testing.T, v *Volume) {
				if !v.journaling() {
					t.Fatal("the commit after the bypass was not journaled")
				}
			}},
			{do: func(v *Volume) error { return v.MkDir("/b", 0o755) }},
		},
	})
}
