// Copyright (c) 2026, go-filesystems
// SPDX-License-Identifier: BSD-3-Clause

package hfsplus

import (
	"bytes"
	"crypto/sha256"
	"encoding/binary"
	"fmt"
	"slices"
	"sort"
	"testing"
)

// File data through the journal (design section 6; hfsplus#45, PR 4): small data
// journaled, large data ordered, the revoke rule, the forged-sector guard for data,
// and the crash oracle with byte checks. Mutation targets (each must turn a named
// test red; see the PR):
//
//   - no revoke check (zones never built): TestJournalDataCrashOracleRevoke*,
//     TestJournalDataRevokeForcesJournaling;
//   - large data written without its barrier: TestJournalDataCrashOracleLarge*,
//     TestJournalDataBarriers;
//   - the forged-sector guard skipped for data: TestJournalDataForgedSectorStaysOutOfTheRing.

// pat is n bytes that differ from every other pattern and shift every sector, so
// stale bytes of another file never read as this one.
func pat(n int, seed byte) []byte {
	b := make([]byte, n)
	for i := range b {
		b[i] = byte(i*7+i/512*13) + seed
	}
	return b
}

// observeFiles is the oracle's view of a volume with file contents: every root
// entry, a file with its size and the checksum of its bytes.
func observeFiles(v *Volume) ([]string, error) {
	ents, err := v.ListDir("/")
	if err != nil {
		return nil, err
	}
	var out []string
	for _, e := range ents {
		if e.FileType() == ftDir {
			out = append(out, e.Name()+"/")
			continue
		}
		b, err := v.ReadFile("/" + e.Name())
		if err != nil {
			return nil, fmt.Errorf("%s: %w", e.Name(), err)
		}
		out = append(out, fmt.Sprintf("%s:%d:%x", e.Name(), len(b), sha256.Sum256(b)))
	}
	sort.Strings(out)
	return out, nil
}

func wf(name string, data []byte) jwOp {
	return func(v *Volume) error { return v.WriteFile(name, data, 0o644) }
}

func rm(name string) jwOp { return func(v *Volume) error { return v.DeleteFile(name) } }

// ringHoldsSector reports whether any sector of the ring is this one. (The ring
// holds the driver's own block list headers, which verify: only the planted
// sector is looked for.)
func ringHoldsSector(img []byte, jbase int64, size int, sector []byte) bool {
	for o := 512; o+512 <= size; o += 512 {
		if bytes.Equal(img[int(jbase)+o:int(jbase)+o+512], sector) {
			return true
		}
	}
	return false
}

// A 1-byte create is one barrier; large data is two (three at the first commit); the
// data is in place and reads back.
func TestJournalDataBarriers(t *testing.T) {
	img, _ := journalImage(t, 0x80000, 1000, 0x4400)
	v, c, path := jwFile(t, img, true)
	defer v.Close()
	small, big := pat(1, 3), pat(100000, 5) // the bound is (0x80000-512)/8 = 65472
	if v.jw.dataBound() != 65472 {
		t.Fatalf("bound %d", v.jw.dataBound())
	}
	var counts []int
	for _, op := range []jwOp{wf("/s1", small), wf("/s2", pat(3000, 9)), wf("/big", big), wf("/s3", small), wf("/big2", pat(70000, 1))} {
		before := c.syncs
		if err := op(v); err != nil {
			t.Fatal(err)
		}
		counts = append(counts, c.syncs-before)
		if !v.journaling() {
			t.Fatalf("commit %d left the journal", len(counts))
		}
	}
	if want := []int{2, 1, 2, 1, 2}; !slices.Equal(counts, want) {
		t.Errorf("barriers per commit %v, want %v (first commit 2, small data 1, large data 2)", counts, want)
	}
	// In place, readable with no replay.
	onDisk := mustRead(t, path)
	w := mustOpen(t, onDisk, false)
	for name, want := range map[string][]byte{"/s1": small, "/s2": pat(3000, 9), "/big": big, "/s3": small, "/big2": pat(70000, 1)} {
		if got, err := w.ReadFile(name); err != nil || !bytes.Equal(got, want) {
			t.Errorf("%s: %d bytes, %v", name, len(got), err)
		}
	}
}

// The first commit of a session with large data pays 3: the data barrier, then the
// two of a first commit.
func TestJournalDataFirstCommitLarge(t *testing.T) {
	img, _ := journalImage(t, 0x80000, 1000, 0x4400)
	v, c, _ := jwFile(t, img, true)
	defer v.Close()
	if err := v.WriteFile("/big", pat(100000, 5), 0o644); err != nil {
		t.Fatal(err)
	}
	if c.syncs != 3 {
		t.Errorf("first commit with large data paid %d barriers, want 3", c.syncs)
	}
}

// Small data is in the transaction: the ring holds the bytes. Large data is not
// (only its first zone blocks, none here).
func TestJournalDataSmallIsInTheRing(t *testing.T) {
	img, jbase := journalImage(t, 0x80000, 1000, 0x4400)
	v, _, path := jwFile(t, img, true)
	defer v.Close()
	marker := pat(2048, 77)
	if err := v.WriteFile("/m", marker, 0o644); err != nil {
		t.Fatal(err)
	}
	ring := mustRead(t, path)[jbase+512 : jbase+0x80000]
	if !bytes.Contains(ring, marker) {
		t.Error("small data is not in the journal transaction")
	}
	big := pat(100000, 78)
	if err := v.WriteFile("/b", big, 0o644); err != nil {
		t.Fatal(err)
	}
	ring = mustRead(t, path)[jbase+512 : jbase+0x80000]
	if bytes.Contains(ring, big[:4096]) {
		t.Error("large data went into the ring")
	}
}

// fileOracle runs the crash oracle with byte checks on a 256 KiB journal (the smallest
// macOS accepts: data bound (0x40000-512)/8 = 32704): every crash image replays to the
// volume after some j commits, names AND the checksum of every file's bytes, so a
// file that reads stale bytes (zeros, another file's old contents) matches no state.
func fileOracle(t *testing.T, name string, ops []jwOp, reverse bool, syncs []int) {
	t.Helper()
	img, jbase := journalImage(t, 0x40000, 100, 0x4400)
	checkJWScenario(t, jwScenario{name: name, img: img, jbase: jbase, ops: ops, reverse: reverse, observe: observeFiles, wantSyncs: syncs})
}

func TestJournalDataCrashOracleSmall(t *testing.T) {
	for _, reverse := range []bool{false, true} {
		t.Run(fmt.Sprint("reverse=", reverse), func(t *testing.T) {
			fileOracle(t, "small", []jwOp{
				wf("/f1", pat(1, 1)), wf("/f2", pat(3000, 2)), mkdirOps(1)[0],
				wf("/f3", pat(20000, 3)), rm("/f1"),
			}, reverse, []int{2, 1, 1, 1, 1})
		})
	}
}

// Large data: ordered, one more barrier, and the data never shows stale.
func TestJournalDataCrashOracleLarge(t *testing.T) {
	for _, reverse := range []bool{false, true} {
		t.Run(fmt.Sprint("reverse=", reverse), func(t *testing.T) {
			fileOracle(t, "large", []jwOp{
				wf("/big", pat(60000, 4)), wf("/s", pat(100, 5)), wf("/big2", pat(70000, 6)),
			}, reverse, []int{3, 1, 2})
		})
	}
}

// The revoke case of design section 6: blocks journaled in T_1 (a's data), freed in T_2,
// reused as large data in T_3. A replay of T_1 (the durable header is still H_2 = [T_1]
// while group 3 is in flight) would put a's bytes over b's. The reused blocks are
// journaled in T_3 so the replay of T_3 comes last; the rest of b is ordered.
func TestJournalDataCrashOracleRevoke(t *testing.T) {
	for _, reverse := range []bool{false, true} {
		t.Run(fmt.Sprint("reverse=", reverse), func(t *testing.T) {
			fileOracle(t, "revoke", []jwOp{
				wf("/a", pat(12288, 7)), rm("/a"), wf("/b", pat(60000, 8)), mkdirOps(1)[0],
			}, reverse, []int{2, 1, 2, 1})
		})
	}
}

// The same sequence with one more commit between a and its reuse: a's blocks are
// out of the replay window at b, so nothing is forced, and b is plain ordered data.
func TestJournalDataCrashOracleRevokeOutOfWindow(t *testing.T) {
	fileOracle(t, "revoke-out", []jwOp{
		wf("/a", pat(12288, 7)), mkdirOps(1)[0], rm("/a"), wf("/b", pat(60000, 8)),
	}, false, []int{2, 1, 1, 2})
}

// The revoke rule forces the reused blocks into the transaction (zones) and the
// allocator keeps them within the bound.
func TestJournalDataRevokeForcesJournaling(t *testing.T) {
	img, _ := journalImage(t, 0x40000, 100, 0x4400)
	v, _, _ := jwFile(t, img, true)
	defer v.Close()
	for _, op := range []jwOp{wf("/a", pat(12288, 7)), rm("/a")} {
		if err := op(v); err != nil {
			t.Fatal(err)
		}
	}
	if len(v.jw.zones) == 0 {
		t.Fatal("no revoke zones after two commits")
	}
	// Stage b without committing: its first blocks are a's, forced.
	tx, err := v.begin()
	if err != nil {
		t.Fatal(err)
	}
	runs, err := tx.alc.allocData(15)
	if err != nil {
		t.Fatal(err)
	}
	bs := int64(v.vh.BlockSize)
	first := int64(runs[0].StartBlock) * bs
	if !inZones(v.jw.zones, first) {
		t.Fatalf("b starts at %d, not in a's zone %v: the scenario does not reuse a's blocks", first, v.jw.zones)
	}
	if err := tx.writeForkData(runs, pat(15*int(bs), 8)); err != nil {
		t.Fatal(err)
	}
	if v.jforcedBytes == 0 {
		t.Error("no data was forced into the transaction")
	}
	if v.jforcedBytes > v.jw.dataBound() {
		t.Errorf("%d forced bytes, bound %d", v.jforcedBytes, v.jw.dataBound())
	}
	v.discard()
}

// The allocator skips a block of a zone once the bytes of zone blocks it took reach the
// bound, and counts what it took.
func TestJournalDataAllocatorSkipsPastBound(t *testing.T) {
	img, _ := journalImage(t, 0x40000, 100, 0x4400)
	v, _, _ := jwFile(t, img, true)
	defer v.Close()
	tx, err := v.begin()
	if err != nil {
		t.Fatal(err)
	}
	a := tx.alc
	bs := a.bs
	var f uint32
	for a.test(f) {
		f++
	}
	a.zones = []span{{int64(f) * bs, int64(f+20) * bs}}
	a.revBound = 3 * bs
	runs, err := a.allocData(5)
	if err != nil {
		t.Fatal(err)
	}
	if len(runs) != 1 || runs[0].StartBlock < f+20 || a.revUsed != 0 {
		t.Errorf("5 blocks: %v, zone bytes %d: a run through the zone passed the bound", runs, a.revUsed)
	}
	runs, err = a.allocData(3)
	if err != nil {
		t.Fatal(err)
	}
	if runs[0].StartBlock != f || a.revUsed != 3*bs {
		t.Errorf("3 blocks: %v, zone bytes %d, want the first 3 zone blocks (%d), %d", runs, a.revUsed, f, 3*bs)
	}
	// Fragments: the 4th zone block is skipped, the allocation continues past the zone.
	runs, err = a.allocData(2)
	if err != nil {
		t.Fatal(err)
	}
	for _, r := range runs {
		if inZones(a.zones, int64(r.StartBlock)*bs) {
			t.Errorf("a block of the zone was taken past the bound: %v", runs)
		}
	}
	v.discard()
}

// Risk 5 for data (design section 11): file bytes that form a valid block list
// header never enter the ring. The data takes the ordered path (one barrier more).
func TestJournalDataForgedSectorStaysOutOfTheRing(t *testing.T) {
	for _, bo := range []binary.ByteOrder{binary.LittleEndian, binary.BigEndian} {
		t.Run(fmt.Sprint(bo), func(t *testing.T) {
			img, jbase := journalImage(t, 0x40000, 100, 0x4400)
			v, c, path := jwFile(t, img, true)
			defer v.Close()
			if err := v.MkDir("/d", 0o755); err != nil {
				t.Fatal(err)
			}
			data := append(append(pat(512, 1), forgedHeader(bo, 0)...), pat(700, 2)...)
			if len(data)%512 == 0 || forgedSector([]writeRun{{off: 0, data: data[512:1024]}}) == false {
				t.Fatal("the test data carries no valid header")
			}
			before := c.syncs
			if err := v.WriteFile("/f", data, 0o644); err != nil {
				t.Fatal(err)
			}
			if got := c.syncs - before; got != 2 {
				t.Errorf("a commit with a forged data sector paid %d barriers, want 2 (ordered data, then the commit)", got)
			}
			if !v.journaling() {
				t.Error("the ordered path left the journal: forged data is not a reason for the direct path")
			}
			onDisk := mustRead(t, path)
			if ringHoldsSector(onDisk, jbase, 0x40000, forgedHeader(bo, 0)) {
				t.Fatal("a sector that verifies as a block list header is in the ring")
			}
			f := forgedHeader(bo, 0)[:blhdrChecksumSize]
			if !bytes.Contains(onDisk[:jbase], f) && !bytes.Contains(onDisk[jbase+0x40000:], f) {
				t.Fatal("the forged sector is nowhere in place: the test proves nothing")
			}
			got, err := mustOpen(t, onDisk, false).ReadFile("/f")
			if err != nil || !bytes.Equal(got, data) {
				t.Errorf("/f reads %d bytes (%v), want %d", len(got), err, len(data))
			}
		})
	}
}

// The crash oracle across forged data, small beside it.
func TestJournalDataCrashOracleForged(t *testing.T) {
	fileOracle(t, "forged-data", []jwOp{
		wf("/s", pat(300, 1)),
		wf("/f", append(pat(512, 2), forgedHeader(binary.LittleEndian, 0)...)),
		wf("/g", append(forgedHeader(binary.BigEndian, 0), pat(100, 3)...)),
		wf("/h", pat(100, 4)),
	}, false, []int{2, 2, 2, 1})
}

// A block the revoke rule forces into the transaction whose data verifies as a block
// list header can be neither journaled nor written in place: the commit takes the
// direct path (the journal is emptied first, so nothing replays over the data).
func TestJournalDataForcedForgedFallsBack(t *testing.T) {
	img, jbase := journalImage(t, 0x40000, 100, 0x4400)
	v, _, path := jwFile(t, img, true)
	defer v.Close()
	for _, op := range []jwOp{wf("/a", pat(8192, 7)), rm("/a")} {
		if err := op(v); err != nil {
			t.Fatal(err)
		}
	}
	data := append(forgedHeader(binary.LittleEndian, 0), pat(3000, 2)...)
	if err := v.WriteFile("/b", data, 0o644); err != nil {
		t.Fatal(err)
	}
	if v.journaling() {
		t.Error("the commit stayed on the journal")
	}
	onDisk := mustRead(t, path)
	if h := journalHeaderOf(t, onDisk, jbase); h.start != h.end {
		t.Errorf("journal not empty after the fallback: %d %d", h.start, h.end)
	}
	if ringHoldsSector(onDisk, jbase, 0x40000, forgedHeader(binary.LittleEndian, 0)) {
		t.Error("a forged sector is in the ring")
	}
	if got, err := mustOpen(t, onDisk, false).ReadFile("/b"); err != nil || !bytes.Equal(got, data) {
		t.Errorf("/b reads %d bytes (%v)", len(got), err)
	}
	if err := v.MkDir("/c", 0o755); err != nil || !v.journaling() {
		t.Errorf("the journal did not resume: %v", err)
	}
}

// Staged data of an abandoned transaction is forgotten, and a failed commit leaves
// nothing staged for the next one.
func TestJournalDataDiscardForgets(t *testing.T) {
	img, _ := journalImage(t, 0x40000, 100, 0x4400)
	v, _, _ := jwFile(t, img, true)
	defer v.Close()
	if err := v.writeData(pat(512, 1), 1<<20); err != nil {
		t.Fatal(err)
	}
	if !v.hasStaged() {
		t.Fatal("nothing staged")
	}
	v.discard()
	if v.hasStaged() || v.jdataBytes != 0 || v.dataSpilled {
		t.Error("discard left data staged")
	}
}
