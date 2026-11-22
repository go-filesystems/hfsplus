// Copyright (c) 2026, go-filesystems
// SPDX-License-Identifier: BSD-3-Clause

package hfsplus

import (
	"bytes"
	"encoding/binary"
	"fmt"
	"math/rand"
	"os"
	"path/filepath"
	"slices"
	"sort"
	"testing"
)

// The crash oracle of design section 9, run against this driver's own replayer
// (PR 1) on every platform, and against the kernel's on darwin
// (journal_writer_darwin_test.go).
//
// A scenario is a base image and a list of mutations, each one commit. The log of
// writes and barriers of the whole sequence is recorded; every crash image built
// from it must replay to the volume after j commits, for some j between the
// commits that had returned before the crash point and the commits that had begun.
// Never a half state, never a journal the kernel would discard.

// jwOp is one commit.
type jwOp func(v *Volume) error

// jwObserver reads the state of a volume that the oracle compares.
type jwObserver func(v *Volume) ([]string, error)

type jwScenario struct {
	name string
	img  []byte // base image: a journaled volume with an empty journal
	// jbase is where the journal lies; direct allows direct-path commits (mid-commit
	// images with the journaled bit cleared are then only checked for an empty journal).
	jbase  int64
	direct bool
	ops    []jwOp
	// tail, when set, runs after the commits as one more step that must not change
	// what the volume shows (Close, SetDurability away from Strict): a crash anywhere
	// inside it replays to the last state. tailSyncs, when not 0, is its barrier count.
	tail      jwOp
	tailSyncs int
	// noScrub skips the ring scrub, for a scenario that tests what protects a ring
	// the scrub would empty (the sequence bump) and would be 500 sector writes longer.
	noScrub bool
	// reverse: a medium that lands each barrier group in reverse order (jwCrashImagesOrder).
	reverse bool
	// observe reads what a volume shows, compared between states (nil: rootNames).
	// observeFiles adds every file's size and checksum: the byte checks of PR 4.
	observe jwObserver
	// wantSyncs, when set, is the barrier count of each commit.
	wantSyncs []int
	// points selects the crash points to check (nil: every event prefix).
	points func(starts, ends []int, ev []crashEvent, jw []jwRange) map[int]bool
}

// jwRange is where commit k's transaction lies in the ring, from the writer's
// state after the commit.
type jwRange struct{ start, end uint64 }

func mkdirOps(n int) []jwOp {
	var ops []jwOp
	for i := 0; i < n; i++ {
		name := fmt.Sprintf("/d%03d", i)
		ops = append(ops, func(v *Volume) error { return v.MkDir(name, 0o755) })
	}
	return ops
}

// rootNames lists the names in the root of v, sorted.
func rootNames(v *Volume) ([]string, error) {
	ents, err := v.ListDir("/")
	if err != nil {
		return nil, err
	}
	var out []string
	for _, e := range ents {
		out = append(out, e.Name())
	}
	sort.Strings(out)
	return out, nil
}

// consistencyOf returns "" when the volume is consistent in the sense of
// crash_test.go's check: its counts agree with its catalog (against the offsets
// the pristine image had) and next CNID is above every CNID in use.
func jwConsistent(v *Volume, base consistency) string {
	f, d, maxCNID, err := walkVolume(v)
	if err != nil {
		return "catalog walk: " + err.Error()
	}
	raw := make([]byte, volumeHeaderOffset+512)
	if _, err := v.rs.ReadAt(raw[volumeHeaderOffset:], volumeHeaderOffset); err != nil {
		return "header: " + err.Error()
	}
	hf, hd, next, _ := headerCounts(raw)
	switch {
	case int(hf)-base.fileOff != f:
		return fmt.Sprintf("fileCount %d, catalog holds %d", int(hf)-base.fileOff, f)
	case int(hd)-base.folderOff != d:
		return fmt.Sprintf("folderCount %d, catalog holds %d", int(hd)-base.folderOff, d)
	case next <= maxCNID:
		return fmt.Sprintf("nextCatalogID %d, but CNID %d is in use", next, maxCNID)
	}
	return ""
}

func jwBaseline(t testing.TB, img []byte) consistency {
	t.Helper()
	v, err := Open(bytesReaderAt(img), int64(len(img)))
	if err != nil {
		t.Fatal(err)
	}
	f, d, _, err := walkVolume(v)
	if err != nil {
		t.Fatal(err)
	}
	raw := make([]byte, volumeHeaderOffset+512)
	v.rs.ReadAt(raw[volumeHeaderOffset:], volumeHeaderOffset)
	hf, hd, _, _ := headerCounts(raw)
	return consistency{fileOff: int(hf) - f, folderOff: int(hd) - d}
}

// jwCheckImage returns "" when img, as a crash image, replays to the volume after
// j commits for some j in [lo, hi]; otherwise what is wrong.
func jwCheckImage(img []byte, lo, hi int, states [][]string, base consistency, jbase int64, direct bool, observe jwObserver) string {
	if observe == nil {
		observe = rootNames
	}
	vh, err := readVolumeHeader(bytesReaderAt(img))
	if err != nil {
		return "volume header: " + err.Error()
	}
	if direct && vh.Attributes&kHFSVolumeJournaledBit == 0 {
		// Mid-commit on the direct path: the journaled bit is cleared for the span of
		// the commit, so macOS treats the volume as not journaled and checks it. What
		// must hold: the journal is empty, or its transactions would be lost.
		h, err := decodeJournalHeader(img[jbase : jbase+512])
		if err != nil || h.start != h.end {
			return fmt.Sprintf("direct-path commit under way with a journal that is not empty: %+v, %v", h, err)
		}
		return ""
	}
	st, _, err := loadJournalAt(bytesReaderAt(img), int64(len(img)), vh)
	if err != nil {
		return "journal: " + err.Error()
	}
	if !st.empty {
		// macOS replays a journal only when the volume says 'HFSJ' (hfs_vfsops.c:1583);
		// any other value and it throws the transactions away.
		if got := string(img[vhLastMountedVersionOff : vhLastMountedVersionOff+4]); got != "HFSJ" {
			return fmt.Sprintf("transactions pending but lastMountedVersion is %q", got)
		}
	}
	v, err := Open(bytesReaderAt(img), int64(len(img)), WithJournalReplay())
	if err != nil {
		return "does not open with replay: " + err.Error()
	}
	names, err := observe(v)
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
	// The writable replay (PR 2) agrees with the read path.
	w := bytes.Clone(img)
	wv, err := OpenWritable(w, nil)
	if err != nil {
		return "writable open (replay) refused: " + err.Error()
	}
	wnames, err := observe(wv)
	if err != nil || !slices.Equal(wnames, names) {
		return fmt.Sprintf("writable replay shows %v (%v), read replay %v", wnames, err, names)
	}
	return ""
}

// splitSectors turns the write events into 512-byte events (a medium tears and
// reorders sector by sector) and returns the new log with the commit boundaries
// mapped into it.
func splitSectors(ev []crashEvent, starts, ends []int) ([]crashEvent, []int, []int) {
	var out []crashEvent
	idx := make([]int, len(ev)+1)
	for i, e := range ev {
		idx[i] = len(out)
		if e.sync {
			out = append(out, e)
			continue
		}
		for o := 0; o < len(e.data); o += 512 {
			out = append(out, crashEvent{off: e.off + int64(o), data: e.data[o:min(o+512, len(e.data))]})
		}
	}
	idx[len(ev)] = len(out)
	ns, ne := make([]int, len(starts)), make([]int, len(ends))
	for i := range starts {
		ns[i], ne[i] = idx[starts[i]], idx[ends[i]]
	}
	return out, ns, ne
}

// jwCrashImages enumerates crash images and calls visit with each: for every
// selected crash point p, the images of "fsynced-only" and "in-order", the open
// group's write subsets (each write alone, all but each write, and random subsets
// from a fixed seed), and each sector write of the group torn (its first half
// reaches the medium).
func jwCrashImages(orig []byte, ev []crashEvent, starts, ends []int, points map[int]bool, visit func(name string, img []byte, lo, hi int)) {
	jwCrashImagesOrder(orig, ev, starts, ends, points, false, visit)
}

// jwCrashImagesOrder is jwCrashImages; reverse makes a medium that lands the writes
// of one barrier group in the reverse of the order they were issued, both when the
// group is made durable by its barrier and when a crash cuts it open. Two writes to
// the same sector in one group then end in the older one: the order a barrier is
// there to fix.
func jwCrashImagesOrder(orig []byte, ev []crashEvent, starts, ends []int, points map[int]bool, reverse bool, visit func(name string, img []byte, lo, hi int)) {
	rng := rand.New(rand.NewSource(1))
	dur, cur := bytes.Clone(orig), bytes.Clone(orig)
	durIdx := 0
	apply := func(img []byte, e crashEvent) { copy(img[e.off:], e.data) }
	for p := 0; p <= len(ev); p++ {
		if p > 0 {
			e := ev[p-1]
			if e.sync {
				grp := ev[durIdx : p-1]
				for i := range grp {
					w := grp[i]
					if reverse {
						w = grp[len(grp)-1-i]
					}
					if !w.sync {
						apply(dur, w)
					}
				}
				durIdx = p
			} else {
				apply(cur, e)
			}
		}
		if points != nil && !points[p] {
			continue
		}
		done, started := 0, 0
		for k := range starts {
			if ends[k] <= p {
				done++
			}
			if starts[k] < p {
				started++
			}
		}
		lo, hi := done, started
		visit(fmt.Sprintf("fsynced-only@%d", p), bytes.Clone(dur), lo, hi)
		visit(fmt.Sprintf("in-order@%d", p), bytes.Clone(cur), lo, hi)
		var group []crashEvent
		for _, w := range ev[durIdx:p] {
			if !w.sync {
				group = append(group, w)
			}
		}
		if len(group) == 0 {
			continue
		}
		with := func(keep func(i int) bool, torn int) []byte {
			img := bytes.Clone(dur)
			for gi := range group {
				i := gi
				if reverse {
					i = len(group) - 1 - gi
				}
				w := group[i]
				switch {
				case i == torn:
					copy(img[w.off:], w.data[:len(w.data)/2])
				case keep(i):
					apply(img, w)
				}
			}
			return img
		}
		for i := range group {
			visit(fmt.Sprintf("subset-one@%d.%d", p, i), with(func(j int) bool { return j == i }, -1), lo, hi)
			visit(fmt.Sprintf("subset-allbut@%d.%d", p, i), with(func(j int) bool { return j != i }, -1), lo, hi)
			visit(fmt.Sprintf("torn@%d.%d", p, i), with(func(j int) bool { return j != i }, i), lo, hi)
		}
		for r := 0; r < 4; r++ {
			mask := make([]bool, len(group))
			for i := range mask {
				mask[i] = rng.Intn(2) == 0
			}
			visit(fmt.Sprintf("subset-rand@%d.%d", p, r), with(func(j int) bool { return mask[j] }, -1), lo, hi)
		}
	}
}

// runJWScenario runs the scenario's commits with journal writes on, then returns
// the log, the commit boundaries, the expected state after each commit, and the
// ranges the writer used.
func runJWScenario(t *testing.T, s jwScenario) (ev []crashEvent, starts, ends []int, states [][]string, ranges []jwRange, syncs []int) {
	t.Helper()
	path := filepath.Join(t.TempDir(), "w.img")
	if err := os.WriteFile(path, s.img, 0o644); err != nil {
		t.Fatal(err)
	}
	base, err := Open(bytesReaderAt(s.img), int64(len(s.img)))
	if err != nil {
		t.Fatal(err)
	}
	observe := s.observe
	if observe == nil {
		observe = rootNames
	}
	n0, err := observe(base)
	if err != nil {
		t.Fatal(err)
	}
	states = [][]string{n0}
	v, err := OpenFileWritable(path)
	if err != nil {
		t.Fatal(err)
	}
	defer v.Close()
	if err := v.EnableJournalWrites(); err != nil {
		t.Fatal(err)
	}
	v.jw.scrubbed = s.noScrub
	c := instrument(v)
	for k, op := range s.ops {
		starts = append(starts, len(c.events))
		before := c.syncs
		if err := op(v); err != nil {
			t.Fatalf("commit %d: %v", k+1, err)
		}
		ends = append(ends, len(c.events))
		syncs = append(syncs, c.syncs-before)
		names, err := observe(v)
		if err != nil {
			t.Fatal(err)
		}
		states = append(states, names)
		ranges = append(ranges, jwRange{v.jw.prevStart, v.jw.prevEnd})
	}
	if s.tail != nil {
		starts = append(starts, len(c.events))
		before := c.syncs
		if err := s.tail(v); err != nil {
			t.Fatalf("tail: %v", err)
		}
		ends = append(ends, len(c.events))
		syncs = append(syncs, c.syncs-before)
		states = append(states, states[len(states)-1])
		if s.tailSyncs != 0 && syncs[len(syncs)-1] != s.tailSyncs {
			t.Errorf("the tail paid %d barriers, want %d", syncs[len(syncs)-1], s.tailSyncs)
		}
		if ends[len(ends)-1] == starts[len(starts)-1] {
			t.Error("the tail wrote nothing: the oracle covers no crash inside it")
		}
	}
	return c.events, starts, ends, states, ranges, syncs
}

func checkJWScenario(t *testing.T, s jwScenario) {
	t.Helper()
	ev, starts, ends, states, ranges, syncs := runJWScenario(t, s)
	base := jwBaseline(t, s.img)
	if s.wantSyncs != nil && !slices.Equal(syncs[:len(s.ops)], s.wantSyncs) {
		t.Errorf("barriers per commit %v, want %v", syncs[:len(s.ops)], s.wantSyncs)
	}
	sev, sstarts, sends := splitSectors(ev, starts, ends)
	var points map[int]bool
	if s.points != nil {
		points = s.points(sstarts, sends, sev, ranges)
	}
	n, bad := 0, []string{}
	jwCrashImagesOrder(s.img, sev, sstarts, sends, points, s.reverse, func(name string, img []byte, lo, hi int) {
		n++
		if msg := jwCheckImage(img, lo, hi, states, base, s.jbase, s.direct, s.observe); msg != "" {
			bad = append(bad, name+": "+msg)
		}
	})
	t.Logf("%s: %d commits, barriers per commit %v, %d events, %d crash images checked", s.name, len(s.ops), syncs, len(sev), n)
	if n < 100 && points == nil {
		t.Fatalf("only %d crash images: the oracle is vacuous", n)
	}
	if len(bad) > 0 {
		for _, b := range bad[:min(len(bad), 8)] {
			t.Errorf("%s", b)
		}
		t.Fatalf("%d of %d crash images are wrong", len(bad), n)
	}
}

// journalImage returns a journaled image (HFSJ, an empty journal of size bytes,
// header sequence number seq) for the scenarios, and the ring's absolute offset.
func journalImage(t testing.TB, size, seq uint64, end uint64) ([]byte, int64) {
	t.Helper()
	h := buildJournalHeader(binary.LittleEndian, end, end, size, false)
	binary.LittleEndian.PutUint32(h[32:], 0x2000)
	binary.LittleEndian.PutUint32(h[44:], uint32(seq))
	resignJournalHeader(h)
	img, _, jBlock := journaledImage(t, kJIJournalInFSMask, h, size)
	return img, int64(jBlock) * 4096
}

// TestJournalWriterCrashOracle is the merge gate of PR 3: the multi-commit sequence
// k = 1..4, every event prefix, the open group's write subsets and torn sectors.
func TestJournalWriterCrashOracle(t *testing.T) {
	img, _ := journalImage(t, 0x40000, 100, 0x4400)
	checkJWScenario(t, jwScenario{name: "k=1..4", img: img, ops: mkdirOps(4)})
}

// Close and SetDurability away from Strict, after journal commits: the journal is
// emptied (barrier, header start = end, barrier), then Close writes the clean mark,
// alternate header first. A crash between the emptying and the clean mark leaves an
// empty journal and 'HFSJ': nothing to replay, the volume as after the last commit.
func TestJournalWriterCrashOracleClose(t *testing.T) {
	img, _ := journalImage(t, 0x40000, 100, 0x4400)
	checkJWScenario(t, jwScenario{
		name: "close", img: img, ops: mkdirOps(4), tailSyncs: 3,
		tail: func(v *Volume) error { return v.Close() },
	})
	img, _ = journalImage(t, 0x40000, 100, 0x4400)
	checkJWScenario(t, jwScenario{
		name: "set-durability", img: img, ops: mkdirOps(4), tailSyncs: 2,
		tail: func(v *Volume) error { return v.SetDurability(DurabilityMounted) },
	})
}

// A wraparound sequence on the smallest journal macOS accepts (256 KiB): enough
// commits to wrap the ring, with the crash points around the commits whose
// transaction straddles the end of the buffer, lands right behind the previous
// ones, or follows a wrap.
func TestJournalWriterCrashOracleWraparound(t *testing.T) {
	if testing.Short() {
		t.Skip("short")
	}
	img, _ := journalImage(t, 0x40000, 7, 0x3f000)
	checkJWScenario(t, jwScenario{
		name: "wrap", img: img, ops: mkdirOps(80),
		points: func(starts, ends []int, ev []crashEvent, rs []jwRange) map[int]bool {
			pts := map[int]bool{}
			wraps := 0
			for k := range rs {
				w := rs[k].end <= rs[k].start // the transaction straddles the end
				if w {
					wraps++
				}
				if w || k < 2 || k >= len(rs)-1 {
					for p := starts[k]; p <= ends[k]+1 && p <= len(ev); p++ {
						pts[p] = true
					}
					if k+1 < len(rs) {
						for p := starts[k+1]; p <= ends[k+1]; p++ {
							pts[p] = true
						}
					}
				}
			}
			if wraps < 1 {
				t.Errorf("no transaction straddled the end of the ring: the sequence does not wrap")
			}
			return pts
		},
	})
}

// poisonRing fills the ring of the journal at jbase (size bytes) with valid-looking
// stale block lists of an earlier lap, all numbered seq: each is a one-sector list
// with one block, the next sector, aimed at the primary volume header. A replay
// that read one past a new transaction's end would write garbage over the volume
// header. Only the session-start sequence bump keeps them out.
func poisonRing(img []byte, jbase int64, size uint64, seq uint32) {
	bo := binary.LittleEndian
	ring := img[uint64(jbase)+512 : uint64(jbase)+size]
	n := len(ring) / 512
	for j := n - 2; j >= 0; j-- {
		s := ring[j*512 : (j+1)*512]
		for i := range s {
			s[i] = 0x5a
		}
		bo.PutUint16(s[0:], 31)
		bo.PutUint16(s[2:], 2)
		bo.PutUint32(s[4:], 1024)
		bo.PutUint32(s[12:], blhdrCheckChecksums|blhdrFirstHeader)
		clear(s[16:32])
		bo.PutUint32(s[28:], seq)
		bo.PutUint64(s[32:], 2) // bnum 2: the sector at 1024
		bo.PutUint32(s[40:], 512)
		bo.PutUint32(s[44:], journalChecksum(ring[(j+1)*512:(j+2)*512]))
		bo.PutUint32(s[8:], 0)
		bo.PutUint32(s[8:], journalChecksum(s[:blhdrChecksumSize]))
	}
}

// A journal a kernel replay left with a lagging sequence number: its header says
// seq s while the ring still holds block lists numbered s (and s+1...). The first
// transaction of a session must not be followed by one of them (design section 4).
func TestJournalWriterCrashOracleStaleRing(t *testing.T) {
	const size = 0x40000
	img, jbase := journalImage(t, size, 5000, 0x1000)
	poisonRing(img, jbase, size, 5001)
	checkJWScenario(t, jwScenario{name: "stale-ring", img: img, ops: mkdirOps(3), noScrub: true})
}

// A commit with file data on a journal whose live part is full: journal-full
// (journal_writer_full_test.go has the bypass scenarios, which run the direct
// path right after a journal is emptied and write 'GoHF'), then journal commits
// again.
func TestJournalWriterCrashOracleFallback(t *testing.T) {
	img, jbase := journalImage(t, 0x40000, 100, 0x4400)
	ops := []jwOp{
		func(v *Volume) error { return v.MkDir("/d000", 0o755) },
		func(v *Volume) error { return v.MkDir("/d001", 0o755) },
		func(v *Volume) error {
			// Data that does not fit the journal takes the direct path (data that fits is
			// journaled: journal_data_test.go).
			v.jw.prevSize = 0x40000 - 512 - 4096
			return v.WriteFile("/d002", fill(5000, 9), 0o644)
		},
		func(v *Volume) error { return v.MkDir("/d003", 0o755) },
		func(v *Volume) error { return v.MkDir("/d004", 0o755) },
	}
	checkJWScenario(t, jwScenario{name: "fallback", img: img, jbase: jbase, direct: true, ops: ops})
}
