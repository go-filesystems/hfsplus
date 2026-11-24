// Copyright (c) 2026, go-filesystems
// SPDX-License-Identifier: BSD-3-Clause

package hfsplus

import (
	"bytes"
	"encoding/binary"
	"errors"
	"os"
	"path/filepath"
	"slices"
	"testing"
)

// jwFile writes img to a temp file and opens it writable with the writer's log.
func jwFile(t *testing.T, img []byte, enable bool) (*Volume, *countingWriter, string) {
	t.Helper()
	path := filepath.Join(t.TempDir(), "w.img")
	if err := os.WriteFile(path, img, 0o644); err != nil {
		t.Fatal(err)
	}
	v, err := OpenFileWritable(path)
	if err != nil {
		t.Fatal(err)
	}
	if enable {
		if err := v.EnableJournalWrites(); err != nil {
			t.Fatal(err)
		}
	}
	return v, instrument(v), path
}

// memWriter is an io.WriterAt over a slice; syncWriter adds a Sync.
type memWriter struct{ b []byte }

func (m *memWriter) WriteAt(p []byte, off int64) (int, error) {
	copy(m.b[off:], p)
	return len(p), nil
}

type syncWriter struct{ memWriter }

func (s *syncWriter) Sync() error { return nil }

func journalHeaderOf(t testing.TB, img []byte, jbase int64) journalHeader {
	t.Helper()
	h, err := decodeJournalHeader(img[jbase : jbase+512])
	if err != nil {
		t.Fatalf("journal header: %v", err)
	}
	return h
}

// The encoder's bytes are the layout of the test builder (design section 4), and
// the replayer reads them back.
func TestEncodeTxn(t *testing.T) {
	runs := []writeRun{{off: 5 * 512, data: fill(512, 1)}, {off: 40 * 512, data: fill(1536, 2)}}
	enc := encodeTxn(binary.LittleEndian, 77, runs)
	b := newJB(tSize, 512, 512)
	b.txn(512, 77, first, blk{5, fill(512, 1)}, blk{40, fill(1536, 2)})
	if !bytes.Equal(enc, b.buf[512:512+len(enc)]) {
		t.Fatal("the encoder differs from the test builder's layout")
	}
	b2 := newJB(tSize, 512, 512)
	copy(b2.buf[512:], enc)
	ov, err := b2.replay(512, uint64(512+len(enc)), tVolume)
	if err != nil {
		t.Fatal(err)
	}
	wantBlocks(t, ov, map[int64]byte{5: 1, 40: 2, 41: 2, 42: 2})
	if hdr := enc[:512]; binary.LittleEndian.Uint32(hdr[12:]) != 3 || binary.LittleEndian.Uint32(hdr[28:]) != 77 ||
		binary.LittleEndian.Uint32(hdr[4:]) != uint32(len(enc)) {
		t.Errorf("flags/seq/bytes_used: %x", hdr[:32])
	}
	for i := 16 + 16*3; i < 512; i++ {
		if enc[i] != 0x5a {
			t.Fatalf("byte %d of the block list header is %#x, want 0x5a padding", i, enc[i])
		}
	}
}

func TestJournalWriterBarriersAndState(t *testing.T) {
	const size = 0x80000
	img, jbase := journalImage(t, size, 1000, 0x4400)
	copy(img[vhLastMountedVersionOff:], "GoHF") // as a volume this driver wrote
	v, c, path := jwFile(t, img, true)
	var counts []int
	var seqs []uint32
	for _, n := range []string{"/a", "/b", "/c"} {
		before := c.syncs
		if err := v.MkDir(n, 0o755); err != nil {
			t.Fatal(err)
		}
		counts = append(counts, c.syncs-before)
		seqs = append(seqs, v.jw.lastSeq)
	}
	if !slices.Equal(counts, []int{2, 1, 1}) {
		t.Errorf("barriers per commit %v, want [2 1 1]", counts)
	}
	// The session-start bump, then +1 (design section 4).
	if want := uint32(1000 + size/512 + 1); seqs[0] != want || seqs[1] != want+1 || seqs[2] != want+2 {
		t.Errorf("sequence numbers %v, want %d, +1, +2", seqs, want)
	}
	// The in-place writes happened: the volume reads without replay.
	onDisk, _ := os.ReadFile(path)
	if names, _ := rootNames(mustOpen(t, onDisk, false)); !slices.Equal(names, []string{"a", "b", "c"}) {
		t.Errorf("volume in place shows %v", names)
	}
	// 'HFSJ' in both headers, unmounted bit clear, journal non-empty (not closed).
	for _, off := range []int{volumeHeaderOffset, len(onDisk) - 1024} {
		if got := string(onDisk[off+8 : off+12]); got != "HFSJ" {
			t.Errorf("header at %d: lastMountedVersion %q, want HFSJ", off, got)
		}
		if onDisk[off+vhAttributes+2]&(kHFSVolumeUnmountedBit>>8) != 0 {
			t.Errorf("header at %d: marked cleanly unmounted while the journal is live", off)
		}
	}
	if h := journalHeaderOf(t, onDisk, jbase); h.start == h.end || h.blhdrSize != 512 {
		t.Errorf("journal header after 3 commits: start %d end %d blhdr_size %d", h.start, h.end, h.blhdrSize)
	}
	if err := v.Close(); err != nil {
		t.Fatal(err)
	}
	// Close leaves what macOS leaves: an empty journal, 'HFSJ', the clean mark.
	onDisk, _ = os.ReadFile(path)
	if h := journalHeaderOf(t, onDisk, jbase); h.start != h.end || h.sequenceNum != seqs[2]+1 {
		t.Errorf("journal header after Close: start %d end %d seq %d, want empty, seq(T_c) %d", h.start, h.end, h.sequenceNum, seqs[2]+1)
	}
	for _, off := range []int{volumeHeaderOffset, len(onDisk) - 1024} {
		if string(onDisk[off+8:off+12]) != "HFSJ" || onDisk[off+vhAttributes+2]&(kHFSVolumeUnmountedBit>>8) == 0 {
			t.Errorf("header at %d after Close: %q, attributes %x", off, onDisk[off+8:off+12], onDisk[off+4:off+8])
		}
	}
	if names, _ := rootNames(mustOpen(t, onDisk, false)); !slices.Equal(names, []string{"a", "b", "c"}) {
		t.Errorf("after Close: %v", names)
	}
	if _, err := OpenWritable(bytes.Clone(onDisk), nil); err != nil {
		t.Errorf("a closed journal-mode volume does not reopen for writing: %v", err)
	}
}

func mustOpen(t testing.TB, img []byte, replay bool) *Volume {
	t.Helper()
	var opts []OpenOption
	if replay {
		opts = append(opts, WithJournalReplay())
	}
	v, err := Open(bytesReaderAt(img), int64(len(img)), opts...)
	if err != nil {
		t.Fatal(err)
	}
	return v
}

// At Lazy a commit writes nothing (PR 7: it joins the group in memory, no barrier, no
// journal write); Close flushes the group as one transaction and empties the journal.
func TestJournalWritesAtLazyAreGrouped(t *testing.T) {
	img, jbase := journalImage(t, 0x80000, 1000, 0x4400)
	jh := bytes.Clone(img[jbase : jbase+512])
	v, c, path := jwFile(t, img, false)
	if err := v.SetDurability(DurabilityLazy); err != nil {
		t.Fatal(err)
	}
	if err := v.MkDir("/a", 0o755); err != nil {
		t.Fatal(err)
	}
	if c.syncs != 0 || len(c.events) != 0 {
		t.Errorf("%d barriers and %d writes, want none (Lazy has no per-commit cost)", c.syncs, len(c.events))
	}
	if onDisk, _ := os.ReadFile(path); !bytes.Equal(onDisk[jbase:jbase+512], jh) {
		t.Error("the journal header changed before any flush")
	}
	if err := v.Close(); err != nil {
		t.Fatal(err)
	}
	onDisk, _ := os.ReadFile(path)
	if h := journalHeaderOf(t, onDisk, jbase); h.start != h.end {
		t.Errorf("journal not empty after Close: %d %d", h.start, h.end)
	}
	if got := string(onDisk[vhLastMountedVersionOff : vhLastMountedVersionOff+4]); got != "HFSJ" {
		t.Errorf("lastMountedVersion %q, want HFSJ", got)
	}
	if names, _ := rootNames(mustOpen(t, onDisk, false)); !slices.Equal(names, []string{"a"}) {
		t.Errorf("the closed image shows %v", names)
	}
}

func TestEnableJournalWritesUnavailable(t *testing.T) {
	const size = 0x80000
	empty := buildJournalHeader(binary.LittleEndian, 0x4400, 0x4400, size, false)
	cases := []struct {
		name  string
		flags uint32
		jh    []byte
	}{
		{"jhdr_size 4096", kJIJournalInFSMask, withJhdrSize(empty, 4096)},
		{"never initialised", kJIJournalInFSMask | kJIJournalNeedInitMask, make([]byte, 512)},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			img, _, _ := journaledImage(t, c.flags, c.jh, size)
			v, err := OpenWritable(img, nil)
			if err != nil {
				t.Skipf("the open refuses it already: %v", err)
			}
			if err := v.EnableJournalWrites(); !errors.Is(err, ErrJournalWritesUnavailable) {
				t.Errorf("err = %v, want ErrJournalWritesUnavailable", err)
			}
			// The volume keeps writing directly.
			if err := v.MkDir("/a", 0o755); err != nil {
				t.Errorf("MkDir after the refusal: %v", err)
			}
		})
	}
	// A writer that cannot Sync would make every barrier a no-op: refused, the same
	// way the replay at open refuses it. A writer that can is accepted.
	{
		img, _ := journalImage(t, size, 1000, 0x4400)
		mem := &memWriter{b: slices.Clone(img)}
		v, err := OpenWritable(img, onlyWriter{w: mem})
		if err != nil {
			t.Fatal(err)
		}
		if err := v.EnableJournalWrites(); !errors.Is(err, ErrJournalWritesUnavailable) {
			t.Errorf("a writer without Sync: %v, want ErrJournalWritesUnavailable", err)
		}
		if v.jw != nil {
			t.Error("the writer is enabled over a store that cannot Sync")
		}
		img, _ = journalImage(t, size, 1000, 0x4400)
		v, err = OpenWritable(img, &syncWriter{memWriter{b: slices.Clone(img)}})
		if err != nil {
			t.Fatal(err)
		}
		if err := v.EnableJournalWrites(); err != nil {
			t.Errorf("a writer with Sync: %v", err)
		}
		img, _ = journalImage(t, size, 1000, 0x4400)
		if v, err = OpenWritable(img, nil); err != nil {
			t.Fatal(err)
		}
		if err := v.EnableJournalWrites(); err != nil {
			t.Errorf("no writer (in memory): %v", err)
		}
	}
	// Not journaled.
	plain, err := Mkfs(8<<20, FormatConfig{Label: "PLAIN"})
	if err != nil {
		t.Fatal(err)
	}
	v, err := OpenWritable(plain, nil)
	if err != nil {
		t.Fatal(err)
	}
	if err := v.EnableJournalWrites(); !errors.Is(err, ErrJournalWritesUnavailable) {
		t.Errorf("not journaled: %v, want ErrJournalWritesUnavailable", err)
	}
	if err := v.MkDir("/a", 0o755); err != nil {
		t.Fatal(err)
	}
	// The read-only volume.
	ro := mustOpen(t, plain, false)
	if err := ro.EnableJournalWrites(); !errors.Is(err, ErrReadOnly) {
		t.Errorf("read-only: %v", err)
	}
}

// Every case that is not a metadata-only Strict commit that fits writes directly,
// after emptying the journal; the next journal commit starts a new session.
func TestJournalWriterFallbacks(t *testing.T) {
	img, jbase := journalImage(t, 0x80000, 1000, 0x4400)
	v, c, path := jwFile(t, img, true)
	defer v.Close()
	must := func(err error) {
		t.Helper()
		if err != nil {
			t.Fatal(err)
		}
	}
	must(v.MkDir("/a", 0o755))
	must(v.MkDir("/b", 0o755))
	seq := v.jw.lastSeq

	// File data when the live journal is full (invariant D, forced live size) but an
	// empty one holds the commit: journal-full, 2 + 2 barriers, still journaled
	// (design section 7). (Data that fits is journaled: journal_data_test.go.)
	v.jw.prevSize = 0x80000 - 512 - 4096
	before := c.syncs
	must(v.WriteFile("/f.bin", fill(5000, 7), 0o644))
	if got := c.syncs - before; got != 4 {
		t.Errorf("a data commit on a full journal paid %d barriers, want 2 (empty the journal) + 2 (first commit)", got)
	}
	onDisk, _ := os.ReadFile(path)
	if h := journalHeaderOf(t, onDisk, jbase); h.start == h.end || h.sequenceNum != seq+1 {
		t.Errorf("journal after the journal-full commit: start %d end %d seq %d, want live, seq %d", h.start, h.end, h.sequenceNum, seq+1)
	}
	if !v.journaling() {
		t.Error("the journal-full commit left the journal not live")
	}
	must(v.MkDir("/c", 0o755))

	// Too many runs: nothing is written, the caller falls back.
	var many []writeRun
	for i := 0; i < jwMaxRuns+1; i++ {
		many = append(many, writeRun{off: int64(2<<20 + i*1024), data: fill(512, 1)})
	}
	n := len(c.events)
	if done, err := v.flushJournal(many); done || err != nil || len(c.events) != n {
		t.Errorf("%d runs: done=%v err=%v, %d events written", len(many), done, err, len(c.events)-n)
	}

	// A run that is not whole sectors (a region's logical size comes from the image):
	// nothing is written, the caller falls back.
	for _, r := range []writeRun{
		{off: 2 << 20, data: fill(100, 1)},
		{off: 2<<20 + 100, data: fill(512, 1)},
	} {
		n = len(c.events)
		if done, err := v.flushJournal([]writeRun{r}); done || err != nil || len(c.events) != n {
			t.Errorf("run off %d len %d: done=%v err=%v, %d events written", r.off, len(r.data), done, err, len(c.events)-n)
		}
	}

	// A transaction that fits no journal, even an empty one: the bypass (a forged
	// sector stands for it; the size bound itself is flushJournal's plan).
	must(v.MkDir("/d", 0o755))
	before = c.syncs
	must(plantForgedMkDir(v, "/e", binary.LittleEndian))
	if v.journaling() {
		t.Error("a bypass left the journal live")
	}
	if got := c.syncs - before; got != 5 {
		t.Errorf("a bypass paid %d barriers, want 2 + 3", got)
	}
	if got := string(mustRead(t, path)[vhLastMountedVersionOff : vhLastMountedVersionOff+4]); got != "GoHF" {
		t.Errorf("after a bypass lastMountedVersion = %q, want GoHF", got)
	}
	// A transaction bigger than the whole journal cannot be planned, empty or not.
	hdr := v.jw.loc.hdr
	big := []writeRun{{off: 2 << 20, data: fill(int(hdr.size), 1)}}
	if _, _, ok := v.jw.plan(big, nil, nil, nil, nil, true); ok {
		t.Error("a transaction larger than the journal was planned")
	}
	if got, err := v.ReadFile("/f.bin"); err != nil || !bytes.Equal(got, fill(5000, 7)) {
		t.Errorf("f.bin: %d bytes, %v", len(got), err)
	}
	// Every level journals since PR 7 (Lazy accumulates a group): no switch empties the
	// journal, and a switch away from Lazy flushes the group.
	must(v.MkDir("/g", 0o755)) // journal live again
	if !v.journaling() {
		t.Fatal("expected a live journal")
	}
	must(v.SetDurability(DurabilityLazy))
	if !v.journaling() {
		t.Error("SetDurability(Lazy) took the journal off")
	}
	must(v.MkDir("/h", 0o755))
	if v.held || v.grp == nil {
		t.Errorf("a Lazy commit: held %v, group %v", v.held, v.grp != nil)
	}
	must(v.SetDurability(DurabilityStrict))
	if v.grp != nil {
		t.Error("SetDurability(Strict) left the group pending")
	}
	if names, _ := rootNames(mustOpen(t, mustRead(t, path), true)); !slices.Contains(names, "h") {
		t.Errorf("the group was not flushed by the switch: %v", names)
	}
	must(v.MkDir("/i", 0o755))
	if !v.journaling() {
		t.Error("Strict after Lazy did not stay on the journal")
	}
}

func mustRead(t testing.TB, path string) []byte {
	t.Helper()
	b, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	return b
}

// OpenWritable over bytes and OpenWritableAt over a store take the same path.
func TestJournalWriterEntryPoints(t *testing.T) {
	img, jbase := journalImage(t, 0x80000, 1000, 0x4400)
	t.Run("OpenWritable", func(t *testing.T) {
		b := bytes.Clone(img)
		v, err := OpenWritable(b, nil)
		if err != nil {
			t.Fatal(err)
		}
		if err := v.EnableJournalWrites(); err != nil {
			t.Fatal(err)
		}
		for _, n := range []string{"/a", "/b"} {
			if err := v.MkDir(n, 0o755); err != nil {
				t.Fatal(err)
			}
		}
		if h := journalHeaderOf(t, b, jbase); h.start == h.end {
			t.Error("journal still empty after two commits")
		}
		names, _ := rootNames(mustOpen(t, b, true))
		if !slices.Equal(names, []string{"a", "b"}) {
			t.Errorf("replayed %v", names)
		}
		v.Close()
	})
	t.Run("OpenWritableAt", func(t *testing.T) {
		path := filepath.Join(t.TempDir(), "x.img")
		os.WriteFile(path, img, 0o644)
		f, err := os.OpenFile(path, os.O_RDWR, 0)
		if err != nil {
			t.Fatal(err)
		}
		defer f.Close()
		v, err := OpenWritableAt(f, int64(len(img)))
		if err != nil {
			t.Fatal(err)
		}
		if err := v.EnableJournalWrites(); err != nil {
			t.Fatal(err)
		}
		if err := v.MkDir("/a", 0o755); err != nil {
			t.Fatal(err)
		}
		if names, _ := rootNames(mustOpen(t, mustRead(t, path), true)); !slices.Equal(names, []string{"a"}) {
			t.Errorf("replayed %v", names)
		}
		v.Close()
	})
}
