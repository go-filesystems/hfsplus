// Copyright (c) 2026, go-filesystems
// SPDX-License-Identifier: BSD-3-Clause

package hfsplus

import (
	"bytes"
	"encoding/binary"
	"fmt"
	"os"
	"path/filepath"
	"strconv"
	"testing"
)

// Risk 5 of the design (section 11), hfsplus#56: replay follows, past end, a sector
// that verifies as a block list header, and the journal writer puts bytes a user
// chose (file names in catalog nodes) into the ring. The guard is forgedSector;
// these tests are its proof, its mutation targets, and its agreement with the
// replayer.

// forgedHeader builds the sector an attacker wants in the ring: a block list header
// of byte order bo that verifies, sequence number seq (0 is never rejected on
// sequence), one block aimed at the primary volume header (bnum 2, 512 bytes, no
// block checksum so the data after the sector is accepted whatever it is), and
// 0x77 for the rest of the sector, which a name could hold.
func forgedHeader(bo binary.ByteOrder, seq uint32) []byte {
	s := fill(512, 0x77)
	bo.PutUint16(s[0:], 31)
	bo.PutUint16(s[2:], 2)
	bo.PutUint32(s[4:], 1024)
	bo.PutUint32(s[8:], 0)
	bo.PutUint32(s[12:], blhdrCheckChecksums|blhdrFirstHeader)
	clear(s[16:28])
	bo.PutUint32(s[28:], seq)
	bo.PutUint64(s[32:], 2)
	bo.PutUint32(s[40:], 512)
	bo.PutUint32(s[44:], 0)
	bo.PutUint32(s[8:], blhdrSum(s))
	return s
}

func TestVerifiesAsBlockListHeader(t *testing.T) {
	for _, c := range []struct {
		name string
		bo   binary.ByteOrder
	}{{"little-endian", binary.LittleEndian}, {"big-endian", binary.BigEndian}} {
		t.Run(c.name, func(t *testing.T) {
			if !verifiesAsBlockListHeader(forgedHeader(c.bo, 0)) {
				t.Errorf("a %s header that verifies was not recognised", c.name)
			}
			// The same bytes with one byte of the covered 32 changed no longer verify.
			s := forgedHeader(c.bo, 0)
			s[17] ^= 1
			if verifiesAsBlockListHeader(s) {
				t.Error("a header with a covered byte changed still verifies")
			}
			// Bytes after the 32 covered ones are not part of the checksum.
			s = forgedHeader(c.bo, 0)
			s[200] ^= 0xff
			if !verifiesAsBlockListHeader(s) {
				t.Error("a byte after BLHDR_CHECKSUM_SIZE changed the verdict")
			}
		})
	}
	for name, s := range map[string][]byte{
		"zeros": make([]byte, 512), "0x5a": fill(512, 0x5a), "0xff": fill(512, 0xff), "short": make([]byte, 31),
	} {
		if verifiesAsBlockListHeader(s) {
			t.Errorf("%s verifies", name)
		}
	}
	// A slice shorter than the 32 covered bytes is not a header, even when its
	// zero-padded form would verify (seq 0 makes byte 31 zero) and when it is too
	// short to hold the checksum field at all.
	for _, n := range []int{0, 10, 31} {
		if verifiesAsBlockListHeader(forgedHeader(binary.LittleEndian, 0)[:n]) {
			t.Errorf("a %d-byte slice verifies", n)
		}
	}
	// Our own block list header verifies, which is why the guard looks at the
	// blocks and not at the header the encoder writes.
	enc := encodeTxn(binary.LittleEndian, 5, []writeRun{{off: 512, data: fill(512, 1)}})
	if !verifiesAsBlockListHeader(enc[:512]) {
		t.Error("the encoder's own header does not verify")
	}
	if forgedSector([]writeRun{{off: 512, data: fill(512, 1)}, {off: 4096, data: fill(1024, 2)}}) {
		t.Error("ordinary sectors reported forged")
	}
	if !forgedSector([]writeRun{{off: 512, data: fill(512, 1)}, {off: 4096, data: append(fill(512, 2), forgedHeader(binary.BigEndian, 0)...)}}) {
		t.Error("a forged second sector of a run was missed")
	}
}

// ringOf places a forged sector f at ring offset 4096 behind two honest
// transactions and returns the journal builder and the header's start and end.
// T_{k-1} (seq 7) is [512, 1536), T_k (seq 8) is [1536, 4096) and ends exactly on
// f, which replay reads past end; the sector after f is 0xEE, the data of f's one
// block when f is followed.
func ringOf(bo binary.ByteOrder, f []byte) (b *jbuilder, start, end uint64) {
	b = newJB(tSize, 512, 512)
	b.bo = bo
	end = b.txn(512, 7, first, blk{10, fill(512, 1)})
	if next := b.txn(end, 8, first, blk{11, fill(2048, 2)}); next != 4096 {
		panic(fmt.Sprintf("T_k ends at %d", next))
	}
	b.put(4096, f)
	b.put(4608, fill(512, 0xEE))
	return b, 512, end
}

// The proof of risk 5, without the guard: a crash image whose last transaction ends
// on the forged sector makes this driver's replayer apply the forged entries. The
// control, the same ring with an unremarkable sector there, replays the two
// honest transactions only.
func TestForgedHeaderPastEndIsFollowed(t *testing.T) {
	for _, bo := range []binary.ByteOrder{binary.LittleEndian, binary.BigEndian} {
		b, start, end := ringOf(bo, fill(512, 0x77))
		ov, err := b.replay(start, end, tVolume)
		if err != nil {
			t.Fatal(err)
		}
		wantBlocks(t, ov, map[int64]byte{10: 1, 11: 2, 12: 2, 13: 2, 14: 2})

		b, start, end = ringOf(bo, forgedHeader(bo, 0))
		ov, err = b.replay(start, end, tVolume)
		if err != nil {
			t.Fatal(err)
		}
		if got := view(ov, 1024, 512); !bytes.Equal(got, fill(512, 0xEE)) {
			t.Fatalf("%v: replay did not follow the forged list: block 2 = %#x...", bo, got[0])
		}
		if ov.end == 4096 {
			t.Errorf("%v: replay stopped at the end of T_k, it read the forged list", bo)
		}
	}
}

// plantForgedMkDir is MkDir with one difference: before the commit it writes a
// verifying block list header into a sector of the catalog node that is free space
// (what a file name positioned on a sector boundary would put there; the name
// itself cannot be placed through the public API, whose names are UTF-16 and
// ordered by the catalog, so the node bytes are built directly, here). The node
// still holds the same records.
func plantForgedMkDir(v *Volume, p string, bo binary.ByteOrder) error {
	parent, name, err := v.resolveParent(p)
	if err != nil {
		return err
	}
	t, err := v.begin()
	if err != nil {
		return err
	}
	cnid := v.nextCNID()
	folderRec := encodeFolderRecord(cnid, timeNow(), 0)
	binary.BigEndian.PutUint16(folderRec[42:44], uint16(sIFDIR)|0o755)
	if v.CaseSensitive() {
		setHasFolderCount(folderRec, 0)
	}
	if err := t.cw.insertRecord(assembleCatalogRecord(encodeCatalogKey(parent, name), folderRec)); err != nil {
		return err
	}
	if err := t.cw.insertRecord(assembleCatalogRecord(encodeCatalogKey(cnid, ""), encodeFolderThreadRecord(parent, name))); err != nil {
		return err
	}
	v.incFolderCount(1)
	if err := t.adjustParentValence(parent, +1, +1); err != nil {
		return err
	}
	v.bumpWriteCount()
	bw := t.cw.bw
	node := bw.nodeBytes(bw.rootNode())
	n := int(binary.BigEndian.Uint16(node[10:12]))
	freeStart := int(binary.BigEndian.Uint16(node[len(node)-2*(n+1):]))
	tableStart := len(node) - 2*(n+1)
	s := (freeStart + 511) / 512 // the first sector wholly inside the free space
	if (s+1)*512 > tableStart {
		return fmt.Errorf("no free sector in the catalog node (free %d..%d)", freeStart, tableStart)
	}
	copy(node[s*512:], forgedHeader(bo, 0))
	return t.commit()
}

// A commit whose catalog node holds a sector that verifies falls back to the direct
// path: the forged header never reaches the ring, the volume is right, and the
// journal goes on working. Mutation targets: forgedSector's call in flushJournal
// removed, and verifiesAsBlockListHeader reading one byte order only.
func TestJournalWriterForgedSectorFallsBack(t *testing.T) {
	for _, bo := range []binary.ByteOrder{binary.LittleEndian, binary.BigEndian} {
		t.Run(fmt.Sprint(bo), func(t *testing.T) {
			img, jbase := journalImage(t, 0x40000, 100, 0x4400)
			v, c, path := jwFile(t, img, true)
			defer v.Close()
			if err := v.MkDir("/a", 0o755); err != nil {
				t.Fatal(err)
			}
			if !v.journaling() {
				t.Fatal("the first commit was not journaled")
			}
			before := c.syncs
			if err := plantForgedMkDir(v, "/b", bo); err != nil {
				t.Fatal(err)
			}
			if v.journaling() {
				t.Error("a commit with a forged sector stayed on the journal")
			}
			if got := c.syncs - before; got != 5 {
				t.Errorf("the fallback paid %d barriers, want 2 (empty the journal) + 3 (direct)", got)
			}
			onDisk := mustRead(t, path)
			f := forgedHeader(bo, 0)[:blhdrChecksumSize]
			if ring := onDisk[jbase+512 : jbase+0x40000]; bytes.Contains(ring, f) {
				t.Fatal("the forged header is in the ring")
			}
			if !bytes.Contains(onDisk, f) {
				t.Fatal("the commit did not carry the forged sector at all: the test proves nothing")
			}
			if h := journalHeaderOf(t, onDisk, jbase); h.start != h.end {
				t.Errorf("journal not empty after the fallback: %d %d", h.start, h.end)
			}
			// The volume is right and journal commits resume.
			if names, _ := rootNames(mustOpen(t, onDisk, false)); fmt.Sprint(names) != "[a b]" {
				t.Errorf("volume shows %v", names)
			}
			if err := v.MkDir("/c", 0o755); err != nil {
				t.Fatal(err)
			}
			if !v.journaling() {
				t.Error("the journal did not resume after the fallback")
			}
		})
	}
}

// The crash oracle with the forged commit in the middle of a journal sequence:
// every crash image replays to a state between the commits that had returned and
// those that had begun, and the forged sector is never read as a list.
func TestJournalWriterCrashOracleForgedSector(t *testing.T) {
	img, jbase := journalImage(t, 0x40000, 100, 0x4400)
	ops := []jwOp{
		func(v *Volume) error { return v.MkDir("/d000", 0o755) },
		func(v *Volume) error { return plantForgedMkDir(v, "/d001", binary.LittleEndian) },
		func(v *Volume) error { return v.MkDir("/d002", 0o755) },
		func(v *Volume) error { return plantForgedMkDir(v, "/d003", binary.BigEndian) },
		func(v *Volume) error { return v.MkDir("/d004", 0o755) },
	}
	checkJWScenario(t, jwScenario{name: "forged", img: img, jbase: jbase, direct: true, ops: ops})
}

// The writer never emits sequence_num 0, at the start of a session and in the
// middle of one: a sequence number that wraps skips it.
func TestSkipZeroSeq(t *testing.T) {
	if skipZeroSeq(0) != 1 || skipZeroSeq(1) != 1 || skipZeroSeq(0xffffffff) != 0xffffffff {
		t.Error("skipZeroSeq")
	}
	const size = 0x40000
	// Session start: header seq + size/jhdr_size + 1 wraps to exactly 0.
	img, _ := journalImage(t, size, 1<<32-size/512-1, 0x4400)
	v, _, _ := jwFile(t, img, true)
	defer v.Close()
	if err := v.MkDir("/a", 0o755); err != nil {
		t.Fatal(err)
	}
	if v.jw.lastSeq != 1 {
		t.Errorf("session start sequence %d, want 1 (0 skipped)", v.jw.lastSeq)
	}
	// Mid-session: the next one after 0xffffffff.
	v.jw.lastSeq = 0xffffffff
	if err := v.MkDir("/b", 0o755); err != nil {
		t.Fatal(err)
	}
	if v.jw.lastSeq != 1 {
		t.Errorf("mid-session sequence %d, want 1 (0 skipped)", v.jw.lastSeq)
	}
	if got := binary.LittleEndian.Uint32(mustReadRing(t, v)[28:]); got != 1 {
		t.Errorf("the transaction on the medium carries sequence %d", got)
	}
}

// mustReadRing returns the block list header of the last transaction written.
func mustReadRing(t *testing.T, v *Volume) []byte {
	t.Helper()
	b := make([]byte, 512)
	if _, err := v.readUnder(b, int64(v.jw.loc.off+v.jw.prevStart)); err != nil {
		t.Fatal(err)
	}
	return b
}

// FuzzGuardAgreesWithReplayer: a sector the guard lets through (it does not verify
// as a block list header in either byte order) is never accepted as a header by
// the replayer when it lies past end, behind a transaction that ends on it. data
// is the sector then the sector after it.
func FuzzGuardAgreesWithReplayer(f *testing.F) {
	for _, s := range guardFuzzSeeds() {
		f.Add(s.data, s.big)
	}
	f.Fuzz(func(t *testing.T, data []byte, big bool) {
		var bo binary.ByteOrder = binary.LittleEndian
		if big {
			bo = binary.BigEndian
		}
		sector := make([]byte, 512)
		copy(sector, data[:min(len(data), 512)])
		after := make([]byte, 512)
		if len(data) > 512 {
			copy(after, data[512:])
		}
		b, start, end := ringOf(bo, sector)
		b.put(4608, after)
		ov, err := b.replay(start, end, tVolume)
		if verifiesAsBlockListHeader(sector) {
			return // the guard refuses this one: nothing to agree on
		}
		if err != nil {
			t.Fatalf("a sector the guard accepts made replay fail: %v", err)
		}
		if ov.end != 4096 {
			t.Fatalf("the guard accepts the sector, replay read it as a header (stopped at %d)", ov.end)
		}
		for u := range ov.units {
			if u != 10 && (u < 11 || u > 14) {
				t.Fatalf("replay applied unit %d", u)
			}
		}
	})
}

type guardFuzzSeed struct {
	name string
	data []byte
	big  bool
}

// guardFuzzSeeds: forged sectors in both orders (the guard refuses, replay follows),
// the same with a checksum off by one (the guard lets it through, replay refuses),
// a sector of zeros, of 0x5a, and a legitimate header.
func guardFuzzSeeds() []guardFuzzSeed {
	after := fill(512, 0xEE)
	join := func(s []byte) []byte { return append(bytes.Clone(s), after...) }
	bad := forgedHeader(binary.LittleEndian, 0)
	bad[8]++
	enc := encodeTxn(binary.LittleEndian, 9, []writeRun{{off: 512, data: fill(512, 1)}})
	return []guardFuzzSeed{
		{"forged-le", join(forgedHeader(binary.LittleEndian, 0)), false},
		{"forged-be", join(forgedHeader(binary.BigEndian, 0)), true},
		{"forged-le-in-be-ring", join(forgedHeader(binary.LittleEndian, 0)), true},
		{"forged-seq8", join(forgedHeader(binary.LittleEndian, 8)), false},
		{"forged-bad-sum", join(bad), false},
		{"zeros", join(make([]byte, 512)), false},
		{"pad-5a", join(fill(512, 0x5a)), false},
		{"legit-header", join(enc[:512]), false},
	}
}

// TestWriteGuardFuzzCorpus keeps testdata/fuzz/FuzzGuardAgreesWithReplayer in step
// with guardFuzzSeeds (HFSPLUS_REGEN_CORPUS=1 rewrites it).
func TestWriteGuardFuzzCorpus(t *testing.T) {
	dir := filepath.Join("testdata", "fuzz", "FuzzGuardAgreesWithReplayer")
	regen := os.Getenv("HFSPLUS_REGEN_CORPUS") == "1"
	if regen {
		if err := os.MkdirAll(dir, 0o755); err != nil {
			t.Fatal(err)
		}
	}
	for _, s := range guardFuzzSeeds() {
		body := fmt.Sprintf("go test fuzz v1\n[]byte(%s)\nbool(%v)\n", strconv.Quote(string(s.data)), s.big)
		path := filepath.Join(dir, s.name)
		if regen {
			if err := os.WriteFile(path, []byte(body), 0o644); err != nil {
				t.Fatal(err)
			}
			continue
		}
		got, err := os.ReadFile(path)
		if err != nil {
			t.Errorf("%s: %v (run with HFSPLUS_REGEN_CORPUS=1)", path, err)
		} else if string(got) != body {
			t.Errorf("%s differs from its seed (run with HFSPLUS_REGEN_CORPUS=1)", path)
		}
	}
}

// BenchmarkForgedSector is the guard's cost for a commit of n journaled sectors
// (a MkDir commit journals about 5 to 8: two headers, the bitmap, a catalog node
// of 8 sectors at 4 KiB nodes).
func BenchmarkForgedSector(b *testing.B) {
	for _, n := range []int{8, 30, 240} {
		b.Run(strconv.Itoa(n)+"-sectors", func(b *testing.B) {
			var runs []writeRun
			for i := 0; i < n; i += 8 {
				runs = append(runs, writeRun{off: int64(i) * 4096, data: fill(8*512, byte(i+1))})
			}
			b.ReportAllocs()
			b.SetBytes(int64(n) * 512)
			for i := 0; i < b.N; i++ {
				if forgedSector(runs) {
					b.Fatal("forged")
				}
			}
		})
	}
}
