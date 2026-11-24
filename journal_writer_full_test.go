// Copyright (c) 2026, go-filesystems
// SPDX-License-Identifier: BSD-3-Clause

package hfsplus

import (
	"encoding/binary"
	"fmt"
	"os"
	"slices"
	"testing"
)

// forceFull makes the next commit find the live journal (T_{k-2} + T_{k-1}) too
// full for any transaction, while an empty journal still holds it: the state a
// run of large transactions produces (design section 7). It does not touch the
// medium.
func forceFull(v *Volume) {
	hdr := v.jw.loc.hdr
	v.jw.prevSize = hdr.size - uint64(hdr.jhdrSize) - 512
}

func fullOp(name string) jwOp {
	return func(v *Volume) error {
		forceFull(v)
		return v.MkDir(name, 0o755)
	}
}

// Journal full (design section 7): a commit that fits an empty journal but not the
// live one empties the journal (2 barriers) and commits as a first commit (2 more),
// numbered seq(T_{k-1}) + 1 with no session-start bump, and the ring scrub does not
// run again.
func TestJournalFullPath(t *testing.T) {
	const size = 0x40000
	img, jbase := journalImage(t, size, 1000, 0x4400)
	v, c, path := jwFile(t, img, true)
	defer v.Close()
	for _, n := range []string{"/a", "/b"} {
		if err := v.MkDir(n, 0o755); err != nil {
			t.Fatal(err)
		}
	}
	seq0, end0 := v.jw.lastSeq, v.jw.prevEnd
	// A forged header in the ring, far from the transactions: a second scrub would
	// zero it.
	f, err := os.OpenFile(path, os.O_RDWR, 0)
	if err != nil {
		t.Fatal(err)
	}
	sector := make([]byte, 512)
	copy(sector, forgedHeader(binary.LittleEndian, 0))
	if _, err := f.WriteAt(sector, jbase+0x30000); err != nil {
		t.Fatal(err)
	}
	f.Close()

	forceFull(v)
	before := c.syncs
	if err := v.MkDir("/c", 0o755); err != nil {
		t.Fatal(err)
	}
	if got := c.syncs - before; got != 4 {
		t.Errorf("a journal-full commit paid %d barriers, want 4", got)
	}
	if v.jw.fullCommits != 1 || !v.journaling() {
		t.Errorf("fullCommits %d, journaling %v", v.jw.fullCommits, v.journaling())
	}
	if v.jw.lastSeq != seq0+1 {
		t.Errorf("sequence %d, want %d (previous + 1, no bump)", v.jw.lastSeq, seq0+1)
	}
	onDisk := mustRead(t, path)
	if h := journalHeaderOf(t, onDisk, jbase); h.start != end0 || h.sequenceNum != seq0+1 {
		t.Errorf("header after the journal-full commit: start %d seq %d, want start %d seq %d", h.start, h.sequenceNum, end0, seq0+1)
	}
	if !slices.Equal(onDisk[jbase+0x30000:jbase+0x30000+512], sector) {
		t.Error("the ring was scrubbed again")
	}
	// The next commit is a plain later commit.
	before = c.syncs
	if err := v.MkDir("/d", 0o755); err != nil {
		t.Fatal(err)
	}
	if got := c.syncs - before; got != 1 || v.jw.lastSeq != seq0+2 {
		t.Errorf("commit after journal-full: %d barriers, seq %d", got, v.jw.lastSeq)
	}
	// Replay from the medium alone gives all four.
	names, _ := rootNames(mustOpen(t, mustRead(t, path), true))
	if !slices.Equal(names, []string{"a", "b", "c", "d"}) {
		t.Errorf("replay shows %v", names)
	}
}

// A commit that cannot fit even an empty journal is a bypass: the journal is
// emptied, today's strict path runs (3 barriers) and writes 'GoHF'. The next
// journaled commit is a first commit with the session-start bump (a deviation
// from the design text, which says "+1": the bump is the safer of the two).
func TestJournalBypassBarriersAndNumbering(t *testing.T) {
	const size = 0x40000
	img, jbase := journalImage(t, size, 1000, 0x4400)
	v, c, path := jwFile(t, img, true)
	defer v.Close()
	if err := v.MkDir("/a", 0o755); err != nil {
		t.Fatal(err)
	}
	seq := v.jw.lastSeq
	before := c.syncs
	if err := plantForgedMkDir(v, "/b", binary.LittleEndian); err != nil {
		t.Fatal(err)
	}
	if got := c.syncs - before; got != 5 {
		t.Errorf("bypass after a journal commit paid %d barriers, want 2 + 3", got)
	}
	onDisk := mustRead(t, path)
	if got := string(onDisk[vhLastMountedVersionOff : vhLastMountedVersionOff+4]); got != "GoHF" {
		t.Errorf("lastMountedVersion %q after a bypass, want GoHF", got)
	}
	if h := journalHeaderOf(t, onDisk, jbase); h.start != h.end || h.sequenceNum != seq {
		t.Errorf("header after a bypass: %+v, want empty with seq %d", h, seq)
	}
	// A bypass with nothing live pays only the strict path.
	before = c.syncs
	if err := plantForgedMkDir(v, "/c", binary.BigEndian); err != nil {
		t.Fatal(err)
	}
	if got := c.syncs - before; got != 3 {
		t.Errorf("bypass with an empty journal paid %d barriers, want 3", got)
	}
	if err := v.MkDir("/d", 0o755); err != nil {
		t.Fatal(err)
	}
	hdr := v.jw.loc.hdr
	if want := skipZeroSeq(seq + uint32(hdr.size/uint64(hdr.jhdrSize)) + 1); v.jw.lastSeq != want {
		t.Errorf("first commit after a bypass numbered %d, want %d (header seq + size/jhdr + 1)", v.jw.lastSeq, want)
	}
	if !v.journaling() {
		t.Error("the journal did not resume")
	}
}

// Wraparound and journal-full many times: every full commit is counted, the ring
// wraps, and the volume replays to everything written, closed or not.
func TestJournalChurnFullAndWrap(t *testing.T) {
	const size = 0x40000
	img, _ := journalImage(t, size, 7, 0x3f000)
	v, _, path := jwFile(t, img, true)
	var want []string
	forced, wraps := 0, 0
	for i := 0; i < 120; i++ {
		name := fmt.Sprintf("d%03d", i)
		want = append(want, name)
		if i%7 == 3 {
			forceFull(v)
			forced++
		}
		if err := v.MkDir("/"+name, 0o755); err != nil {
			t.Fatalf("commit %d: %v", i, err)
		}
		if v.jw.prevEnd <= v.jw.prevStart {
			wraps++
		}
	}
	if v.jw.fullCommits != forced {
		t.Errorf("%d journal-full commits, forced %d", v.jw.fullCommits, forced)
	}
	if wraps == 0 {
		t.Error("the ring never wrapped")
	}
	t.Logf("120 commits, %d journal-full, %d straddling the end of the ring", v.jw.fullCommits, wraps)
	if names, _ := rootNames(mustOpen(t, mustRead(t, path), true)); !slices.Equal(names, want) {
		t.Errorf("replay without Close shows %d names, want %d", len(names), len(want))
	}
	if err := v.Close(); err != nil {
		t.Fatal(err)
	}
	if names, _ := rootNames(mustOpen(t, mustRead(t, path), false)); !slices.Equal(names, want) {
		t.Errorf("after Close: %d names, want %d", len(names), len(want))
	}
}

// Close commits T_c carrying the unmounted bit: 3 barriers, an empty journal with
// seq(T_c). A volume that was not clean at open pays only the emptying; a Close
// whose T_c finds the journal full empties first.
func TestJournalCloseTc(t *testing.T) {
	const size = 0x40000
	attrs := func(b []byte, off int) bool { return b[off+vhAttributes+2]&(kHFSVolumeUnmountedBit>>8) != 0 }
	altOff := func(b []byte) int { return len(b) - 1024 }

	t.Run("clean", func(t *testing.T) {
		img, jbase := journalImage(t, size, 100, 0x4400)
		v, c, path := jwFile(t, img, true)
		for _, n := range []string{"/a", "/b"} {
			if err := v.MkDir(n, 0o755); err != nil {
				t.Fatal(err)
			}
		}
		seq := v.jw.lastSeq
		before := c.syncs
		if err := v.Close(); err != nil {
			t.Fatal(err)
		}
		if got := c.syncs - before; got != 3 {
			t.Errorf("Close paid %d barriers, want 3", got)
		}
		b := mustRead(t, path)
		if h := journalHeaderOf(t, b, jbase); h.start != h.end || h.sequenceNum != seq+1 {
			t.Errorf("header after Close %+v, want empty with seq(T_c) = %d", h, seq+1)
		}
		for _, off := range []int{volumeHeaderOffset, altOff(b)} {
			if !attrs(b, off) || string(b[off+8:off+12]) != "HFSJ" {
				t.Errorf("header at %d after Close: not clean or not HFSJ", off)
			}
		}
	})
	t.Run("not clean at open", func(t *testing.T) {
		img, jbase := journalImage(t, size, 100, 0x4400)
		for _, off := range []int{volumeHeaderOffset, altOff(img)} {
			img[off+vhAttributes+2] &^= kHFSVolumeUnmountedBit >> 8
		}
		v, c, path := jwFile(t, img, true)
		if err := v.MkDir("/a", 0o755); err != nil {
			t.Fatal(err)
		}
		seq := v.jw.lastSeq
		before := c.syncs
		if err := v.Close(); err != nil {
			t.Fatal(err)
		}
		if got := c.syncs - before; got != 2 {
			t.Errorf("Close of a volume that was not clean paid %d barriers, want 2", got)
		}
		b := mustRead(t, path)
		if h := journalHeaderOf(t, b, jbase); h.start != h.end || h.sequenceNum != seq {
			t.Errorf("header %+v, want empty with seq %d", h, seq)
		}
		if attrs(b, volumeHeaderOffset) {
			t.Error("Close marked a volume clean that was not")
		}
	})
	t.Run("T_c finds the journal full", func(t *testing.T) {
		img, jbase := journalImage(t, size, 100, 0x4400)
		v, c, path := jwFile(t, img, true)
		if err := v.MkDir("/a", 0o755); err != nil {
			t.Fatal(err)
		}
		seq := v.jw.lastSeq
		forceFull(v)
		before := c.syncs
		if err := v.Close(); err != nil {
			t.Fatal(err)
		}
		if got := c.syncs - before; got != 6 {
			t.Errorf("Close with a full journal paid %d barriers, want 4 (journal-full commit) + 2 (empty header)", got)
		}
		b := mustRead(t, path)
		if h := journalHeaderOf(t, b, jbase); h.start != h.end || h.sequenceNum != seq+1 || !attrs(b, volumeHeaderOffset) {
			t.Errorf("after Close: header %+v, clean %v", h, attrs(b, volumeHeaderOffset))
		}
	})
}

// Sync in journal mode: one barrier when in-place writes are pending since the last
// barrier, nothing otherwise.
func TestJournalSyncPending(t *testing.T) {
	img, _ := journalImage(t, 0x40000, 100, 0x4400)
	v, c, _ := jwFile(t, img, true)
	defer v.Close()
	syncs := func() int {
		t.Helper()
		before := c.syncs
		if err := v.Sync(); err != nil {
			t.Fatal(err)
		}
		return c.syncs - before
	}
	if got := syncs(); got != 0 {
		t.Errorf("Sync with nothing written paid %d barriers", got)
	}
	if err := v.MkDir("/a", 0o755); err != nil {
		t.Fatal(err)
	}
	if got := syncs(); got != 1 {
		t.Errorf("Sync after a commit paid %d barriers, want 1", got)
	}
	if got := syncs(); got != 0 {
		t.Errorf("a second Sync paid %d barriers, want 0", got)
	}
	// The next commit's barrier makes the previous in-place writes durable, but its
	// own are pending again.
	if err := v.MkDir("/b", 0o755); err != nil {
		t.Fatal(err)
	}
	if got := syncs(); got != 1 {
		t.Errorf("Sync after a later commit paid %d barriers, want 1", got)
	}
	// A bypass ends on a barrier: nothing pending.
	if err := plantForgedMkDir(v, "/c", binary.LittleEndian); err != nil {
		t.Fatal(err)
	}
	if got := syncs(); got != 0 {
		t.Errorf("Sync after a bypass paid %d barriers, want 0", got)
	}
}

// Every switch between the journal path and a level that writes in place goes
// through emptyJournal (2 barriers, an empty header) and journaling resumes with a
// first commit.
func TestJournalSwitchingLevels(t *testing.T) {
	const size = 0x40000
	for _, lvl := range []Durability{DurabilityLazy} {
		t.Run(lvl.String(), func(t *testing.T) {
			img, _ := journalImage(t, size, 100, 0x4400)
			v, c, path := jwFile(t, img, true)
			defer v.Close()
			if err := v.MkDir("/a", 0o755); err != nil {
				t.Fatal(err)
			}
			before := c.syncs
			if err := v.SetDurability(lvl); err != nil {
				t.Fatal(err)
			}
			if got := c.syncs - before; got != 0 || !v.journaling() {
				t.Errorf("entering Lazy paid %d barriers, journaling %v: the journal stays live, no emptying", got, v.journaling())
			}
			if err := v.MkDir("/b", 0o755); err != nil {
				t.Fatal(err)
			}
			if !v.journaling() || v.held || v.grp == nil || c.syncs != before {
				t.Errorf("a Lazy commit: journaling %v, held %v, group %v, %d barriers", v.journaling(), v.held, v.grp != nil, c.syncs-before)
			}
			if err := v.SetDurability(DurabilityStrict); err != nil {
				t.Fatal(err)
			}
			if got := c.syncs - before; got != 1 || v.grp != nil {
				t.Errorf("leaving Lazy paid %d barriers, group pending %v, want 1 and none", got, v.grp != nil)
			}
			before = c.syncs
			if err := v.MkDir("/c", 0o755); err != nil {
				t.Fatal(err)
			}
			if got := c.syncs - before; got != 1 || !v.journaling() {
				t.Errorf("commit after the switch back: %d barriers, journaling %v", got, v.journaling())
			}
			names, _ := rootNames(mustOpen(t, mustRead(t, path), true))
			if !slices.Equal(names, []string{"a", "b", "c"}) {
				t.Errorf("replay shows %v", names)
			}
		})
	}
	t.Run("Batch", func(t *testing.T) {
		img, _ := journalImage(t, size, 100, 0x4400)
		v, c, _ := jwFile(t, img, true)
		defer v.Close()
		if err := v.MkDir("/a", 0o755); err != nil {
			t.Fatal(err)
		}
		before := c.syncs
		err := v.Batch(func() error {
			if !v.journaling() || c.syncs != before {
				t.Errorf("entering a Batch: journaling %v, %d barriers; want live, none", v.journaling(), c.syncs-before)
			}
			return v.MkDir("/b", 0o755)
		})
		if got := c.syncs - before; got != 1 {
			t.Errorf("the Batch paid %d barriers, want 1", got)
		}
		if err != nil {
			t.Fatal(err)
		}
		if err := v.MkDir("/c", 0o755); err != nil || !v.journaling() {
			t.Errorf("after Batch: %v, journaling %v", err, v.journaling())
		}
	})
}

// The crash oracle across journal-full, with the journal wrapping.
func TestJournalWriterCrashOracleFull(t *testing.T) {
	img, _ := journalImage(t, 0x40000, 100, 0x4400)
	checkJWScenario(t, jwScenario{
		name: "full", img: img,
		ops:       []jwOp{mkdirOps(1)[0], fullOp("/d001"), fullOp("/d002"), mkdirOps(4)[3], fullOp("/d004"), mkdirOps(6)[5]},
		wantSyncs: []int{2, 4, 4, 1, 4, 1},
	})
}

func TestJournalWriterCrashOracleFullWrap(t *testing.T) {
	if testing.Short() {
		t.Skip("short")
	}
	img, _ := journalImage(t, 0x40000, 7, 0x3f000)
	ops := mkdirOps(12)
	ops[4], ops[8] = fullOp("/d004"), fullOp("/d008")
	checkJWScenario(t, jwScenario{
		name: "full-wrap", img: img, ops: ops,
		points: func(starts, ends []int, ev []crashEvent, rs []jwRange) map[int]bool {
			pts := map[int]bool{}
			for k := range rs {
				if k < 2 || (k >= 3 && k <= 5) || (k >= 7 && k <= 9) {
					for p := starts[k]; p <= ends[k]+1 && p <= len(ev); p++ {
						pts[p] = true
					}
				}
			}
			return pts
		},
	})
}

// Close's T_c, after journal-full, and when T_c itself finds the journal full.
func TestJournalWriterCrashOracleCloseTc(t *testing.T) {
	img, _ := journalImage(t, 0x40000, 100, 0x4400)
	checkJWScenario(t, jwScenario{
		name: "close-after-full", img: img, ops: []jwOp{mkdirOps(1)[0], fullOp("/d001"), mkdirOps(3)[2]}, tailSyncs: 3,
		tail: func(v *Volume) error { return v.Close() },
	})
	img, _ = journalImage(t, 0x40000, 100, 0x4400)
	checkJWScenario(t, jwScenario{
		name: "close-full", img: img, ops: mkdirOps(2), tailSyncs: 6,
		tail: func(v *Volume) error { forceFull(v); return v.Close() },
	})
}

// A bypass right after a journal emptying, after journal-full, and as the first
// commit (design section 7, marked UNVERIFIED there: the oracle runs it).
func TestJournalWriterCrashOracleBypass(t *testing.T) {
	le := binary.LittleEndian
	forged := func(n string) jwOp { return func(v *Volume) error { return plantForgedMkDir(v, n, le) } }
	img, jbase := journalImage(t, 0x40000, 100, 0x4400)
	checkJWScenario(t, jwScenario{
		name: "bypass-after-full", img: img, jbase: jbase, direct: true,
		ops: []jwOp{mkdirOps(1)[0], fullOp("/d001"), forged("/d002"), mkdirOps(4)[3], forged("/d004"), forged("/d005"), mkdirOps(7)[6]},
	})
	img, jbase = journalImage(t, 0x40000, 100, 0x4400)
	checkJWScenario(t, jwScenario{
		name: "bypass-first", img: img, jbase: jbase, direct: true,
		ops: []jwOp{forged("/d000"), mkdirOps(2)[1], mkdirOps(3)[2]},
	})
}

// Every write and barrier of the journal-full commit and of Close as T_c fails once,
// in turn: the error comes back, never swallowed.
func TestJournalFullAndCloseFaults(t *testing.T) {
	live := func(v *Volume) {
		for _, n := range []string{"/a", "/b"} {
			if err := v.MkDir(n, 0o755); err != nil {
				t.Fatal(err)
			}
		}
	}
	t.Run("journal full", func(t *testing.T) {
		faultSweep(t, "full", live, fullOp("/c"))
	})
	t.Run("close as T_c", func(t *testing.T) {
		faultSweep(t, "close", live, func(v *Volume) error {
			err := v.MkDir("/c", 0o755)
			if err != nil {
				return err
			}
			return v.Close()
		})
	})
	t.Run("close finds the journal full", func(t *testing.T) {
		faultSweep(t, "close-full", live, func(v *Volume) error {
			if err := v.MkDir("/c", 0o755); err != nil {
				return err
			}
			forceFull(v)
			return v.Close()
		})
	})
}

// What the public API reaches on the smallest journal the driver accepts (256 KiB,
// journal.go: Apple's journal_open range; EnableJournalWrites' own 16 KiB floor is
// never reached because the loader refuses below 256 KiB first). Measured: a mkdir
// or a delete is a transaction of 3.5 KiB, a file of up to size/8 = 32 KiB of data
// one of about 24 KiB beside its metadata, and revoke-forced data is bounded by the
// same size/8 (the allocator skips the zone once it is spent). Three such
// transactions stay far under the journal-full threshold, so on the volumes this
// suite can build only the forced states (forceFull, a forged sector, a shrunk
// loc.hdr.size) reach journal-full and the bypass; this test pins that bound, so a
// change that makes ordinary commits larger fails here and gets a natural scenario.
func TestJournalOrdinaryCommitsStayUnderFull(t *testing.T) {
	img, _ := journalImage(t, 0x40000, 100, 0x4400)
	v, _, _ := jwFile(t, img, true)
	defer v.Close()
	hdr := v.jw.loc.hdr
	limit := hdr.size - uint64(hdr.jhdrSize)
	var largest uint64
	step := func(err error) {
		t.Helper()
		if err != nil {
			t.Fatal(err)
		}
		largest = max(largest, v.jw.prevSize)
	}
	for r := 0; r < 4; r++ {
		step(v.MkDir(fmt.Sprintf("/d%d", r), 0o755))
		step(v.WriteFile("/a", pat(int(v.jw.dataBound()), byte(r)), 0o644))
		step(v.DeleteFile("/a"))
	}
	if 3*largest >= limit {
		t.Errorf("three transactions of %d bytes reach the journal-full threshold %d: add a natural scenario", largest, limit)
	}
	if v.jw.fullCommits != 0 {
		t.Errorf("%d journal-full commits through ordinary operations", v.jw.fullCommits)
	}
	t.Logf("largest ordinary transaction %d bytes, threshold %d", largest, limit)
}
