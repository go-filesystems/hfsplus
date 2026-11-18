// Copyright (c) 2026, go-filesystems
// SPDX-License-Identifier: BSD-3-Clause

package hfsplus

import (
	"bytes"
	"crypto/sha256"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"slices"
	"sort"
	"strings"
	"testing"
)

// Tests for hfsplus#45 PR 2: a writable open replays a non-empty journal.
// The fixture is the macOS-written image of PR 1 (testdata/macos-nonempty-
// journal.*): a 16 MiB Journaled HFS+ volume whose journal holds transactions, and
// the listing macOS itself showed after replaying it.

func goldenList(t testing.TB) []string {
	t.Helper()
	b, err := os.ReadFile(filepath.Join("testdata", "macos-nonempty-journal.golden"))
	if err != nil {
		t.Fatal(err)
	}
	return strings.Split(strings.TrimSpace(string(b)), "\n")
}

// withNew returns want plus the digest line of a file named name holding body.
func withNew(want []string, name string, body []byte) []string {
	out := append(slices.Clone(want), fmt.Sprintf("%s %d %x", name, len(body), sha256.Sum256(body)))
	sort.Strings(out)
	return out
}

func listOf(t testing.TB, img []byte) []string {
	t.Helper()
	v, err := Open(bytesReaderAt(img), int64(len(img)))
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	return fileDigests(t, v)
}

func TestWritableOpenReplaysJournal(t *testing.T) {
	img0, _, _ := macNonEmptyFixture(t)
	golden := goldenList(t)
	body := []byte("written after the replay\n")
	if slices.Equal(listOf(t, img0), golden) {
		t.Fatal("the fixture shows every file without replay: it proves nothing")
	}
	want := withNew(golden, "fnew.txt", body)

	check := func(t *testing.T, img []byte) {
		t.Helper()
		if got := listOf(t, img); !slices.Equal(got, want) {
			t.Errorf("golden + new file expected; first difference: %s", firstDiff(got, want))
		}
		vh, err := readVolumeHeader(bytesReaderAt(img))
		if err != nil {
			t.Fatal(err)
		}
		// The journal is empty again (and stays so, as the write path leaves it).
		if _, err := checkJournal(img, vh); err != nil {
			t.Errorf("journal after replay and write: %v", err)
		}
	}

	t.Run("OpenWritable", func(t *testing.T) {
		img := bytes.Clone(img0)
		v, err := OpenWritable(img, nil)
		if err != nil {
			t.Fatal(err)
		}
		if err := v.WriteFile("/fnew.txt", body, 0o644); err != nil {
			t.Fatal(err)
		}
		v.Close()
		check(t, img)
	})
	t.Run("OpenWritable mirrored to wa", func(t *testing.T) {
		img := bytes.Clone(img0)
		path := filepath.Join(t.TempDir(), "w.img")
		if err := os.WriteFile(path, img0, 0o644); err != nil {
			t.Fatal(err)
		}
		f, err := os.OpenFile(path, os.O_RDWR, 0)
		if err != nil {
			t.Fatal(err)
		}
		defer f.Close()
		v, err := OpenWritable(img, f)
		if err != nil {
			t.Fatal(err)
		}
		if err := v.WriteFile("/fnew.txt", body, 0o644); err != nil {
			t.Fatal(err)
		}
		v.Close()
		onDisk, _ := os.ReadFile(path)
		check(t, onDisk)
	})
	t.Run("OpenFileWritable", func(t *testing.T) {
		path := filepath.Join(t.TempDir(), "w.img")
		if err := os.WriteFile(path, img0, 0o644); err != nil {
			t.Fatal(err)
		}
		v, err := OpenFileWritable(path)
		if err != nil {
			t.Fatal(err)
		}
		if err := v.WriteFile("/fnew.txt", body, 0o644); err != nil {
			t.Fatal(err)
		}
		v.Close()
		onDisk, _ := os.ReadFile(path)
		check(t, onDisk)
	})
	t.Run("OpenWritableAt", func(t *testing.T) {
		path := filepath.Join(t.TempDir(), "w.img")
		if err := os.WriteFile(path, img0, 0o644); err != nil {
			t.Fatal(err)
		}
		f, err := os.OpenFile(path, os.O_RDWR, 0)
		if err != nil {
			t.Fatal(err)
		}
		defer f.Close()
		v, err := OpenWritableAt(f, int64(len(img0)))
		if err != nil {
			t.Fatal(err)
		}
		if err := v.WriteFile("/fnew.txt", body, 0o644); err != nil {
			t.Fatal(err)
		}
		v.Close()
		onDisk, _ := os.ReadFile(path)
		check(t, onDisk)
	})
	t.Run("replay without a write", func(t *testing.T) {
		img := bytes.Clone(img0)
		v, err := OpenWritable(img, nil)
		if err != nil {
			t.Fatal(err)
		}
		v.Close()
		if got := listOf(t, img); !slices.Equal(got, golden) {
			t.Errorf("replayed volume: first difference: %s", firstDiff(got, golden))
		}
	})
}

// A journal whose volume does not say 'HFSJ' is not replayed (TN1150, Journal;
// hfs_vfsutils.c:2886-2895): the open is refused with ErrJournaled and writes
// nothing.
func TestWritableOpenRefusesNonHFSJ(t *testing.T) {
	img0, _, _ := macNonEmptyFixture(t)
	if got := string(img0[vhLastMountedVersionOff : vhLastMountedVersionOff+4]); got != "HFSJ" {
		t.Fatalf("the fixture carries %q, the test assumes 'HFSJ'", got)
	}
	for _, sig := range []string{"GoHF", "FSK!", "10.0", "\x00\x00\x00\x00"} {
		img := bytes.Clone(img0)
		copy(img[vhLastMountedVersionOff:], sig)
		before := bytes.Clone(img)
		_, err := OpenWritable(img, nil)
		if !errors.Is(err, ErrJournaled) {
			t.Errorf("lastMountedVersion %q: err = %v, want ErrJournaled", sig, err)
		}
		if !bytes.Equal(img, before) {
			t.Errorf("lastMountedVersion %q: the refused open changed the image", sig)
		}
	}
}

// A replay that fails keeps its typed error and leaves the volume untouched.
func TestWritableOpenReplayErrorsLeaveVolumeUntouched(t *testing.T) {
	img0, _, h := macNonEmptyFixture(t)
	vh, _ := readVolumeHeader(bytesReaderAt(img0))
	_, loc, _ := loadJournalAt(bytesReaderAt(img0), int64(len(img0)), vh)
	_ = h

	t.Run("bad first list header", func(t *testing.T) {
		img := bytes.Clone(img0)
		img[loc.off+loc.hdr.start+9] ^= 0xff
		before := bytes.Clone(img)
		_, err := OpenWritable(img, nil)
		if !errors.Is(err, ErrJournalReplay) || !errors.Is(err, ErrJournaled) {
			t.Errorf("err = %v, want ErrJournalReplay (wrapping ErrJournaled)", err)
		}
		if !bytes.Equal(img, before) {
			t.Error("a failed replay changed the image")
		}
	})
	t.Run("OpenWritableAt on a file", func(t *testing.T) {
		img := bytes.Clone(img0)
		img[loc.off+loc.hdr.start+9] ^= 0xff
		path := filepath.Join(t.TempDir(), "x.img")
		if err := os.WriteFile(path, img, 0o644); err != nil {
			t.Fatal(err)
		}
		if _, err := OpenFileWritable(path); !errors.Is(err, ErrJournalReplay) {
			t.Errorf("err = %v, want ErrJournalReplay", err)
		}
		after, _ := os.ReadFile(path)
		if !bytes.Equal(after, img) {
			t.Error("a failed replay changed the file")
		}
	})
	t.Run("a block on the journal", func(t *testing.T) {
		// A block list whose first entry names a block inside the journal itself.
		img := bytes.Clone(img0)
		base := loc.off + loc.hdr.start
		first := img[base : base+uint64(loc.hdr.blhdrSize)]
		bo := loc.hdr.bo
		bo.PutUint64(first[32:], loc.off/uint64(loc.hdr.jhdrSize)+4) // binfo[1].bnum
		bo.PutUint32(first[8:], 0)
		bo.PutUint32(first[8:], journalChecksum(first[:blhdrChecksumSize]))
		before := bytes.Clone(img)
		if _, err := OpenWritable(img, nil); !errors.Is(err, ErrJournalReplay) {
			t.Errorf("err = %v, want ErrJournalReplay", err)
		}
		if !bytes.Equal(img, before) {
			t.Error("a failed replay changed the image")
		}
	})
}

// crashStore is a replayStore over an image that logs its writes and barriers.
type crashStore struct {
	img    []byte
	events []crashEvent
}

func (c *crashStore) ReadAt(p []byte, off int64) (int, error) {
	return bytesReaderAt(c.img).ReadAt(p, off)
}

func (c *crashStore) WriteAt(p []byte, off int64) (int, error) {
	c.events = append(c.events, crashEvent{off: off, data: bytes.Clone(p)})
	copy(c.img[off:], p)
	return len(p), nil
}

func (c *crashStore) Sync() error {
	c.events = append(c.events, crashEvent{sync: true})
	return nil
}

// crashViolations replays the log of a replay under three crash models and returns
// what is wrong with the images: "fsynced-only" (only what a barrier made durable
// survives), "in-order" (every write up to the crash point survives) and "subset"
// (what a barrier made durable, plus any one write after it, or all but one: the
// writes of an open group reach the medium in any order). Every image must show
// the golden listing through the replay (the pre-replay volume with its journal
// still replaying, or the post-replay one), and an image whose journal header is
// empty must show it WITHOUT the replay: never an empty header over blocks that
// did not land.
func crashViolations(t *testing.T, orig []byte, events []crashEvent, golden []string) []string {
	t.Helper()
	var bad []string
	checked := 0
	// A write reaches the medium sector by sector, in any order within a group: the
	// log is split into 512-byte events (a barrier stays where it is).
	var sectors []crashEvent
	for _, e := range events {
		if e.sync {
			sectors = append(sectors, e)
			continue
		}
		for o := 0; o < len(e.data); o += 512 {
			sectors = append(sectors, crashEvent{off: e.off + int64(o), data: e.data[o:min(o+512, len(e.data))]})
		}
	}
	events = sectors
	check := func(name string, img []byte) {
		checked++
		v, err := Open(bytesReaderAt(img), int64(len(img)), WithJournalReplay())
		if err != nil {
			bad = append(bad, name+": does not open with replay: "+err.Error())
			return
		}
		got, err := safeDigests(v)
		if err != nil {
			bad = append(bad, name+": replayed view unreadable: "+err.Error())
			return
		}
		if !slices.Equal(got, golden) {
			bad = append(bad, name+": replayed view differs from golden: "+firstDiff(got, golden))
			return
		}
		vh, err := readVolumeHeader(bytesReaderAt(img))
		if err != nil {
			bad = append(bad, name+": "+err.Error())
			return
		}
		st, _, err := loadJournalAt(bytesReaderAt(img), int64(len(img)), vh)
		if err != nil {
			bad = append(bad, name+": journal: "+err.Error())
			return
		}
		if st.empty {
			pv, err := Open(bytesReaderAt(img), int64(len(img)))
			if err != nil {
				bad = append(bad, name+": EMPTY journal header over a volume that does not open: "+err.Error())
				return
			}
			got, err := safeDigests(pv)
			if err != nil || !slices.Equal(got, golden) {
				bad = append(bad, fmt.Sprintf("%s: EMPTY journal header over a volume that is not replayed (%v)", name, err))
			}
		}
	}
	apply := func(evs []crashEvent) []byte {
		img := bytes.Clone(orig)
		for _, e := range evs {
			if !e.sync {
				copy(img[e.off:], e.data)
			}
		}
		return img
	}
	// Every crash point near a barrier or either end, and a stride between.
	crashPoints := map[int]bool{0: true, len(events): true}
	for k := 0; k <= len(events); k += max(1, len(events)/24) {
		crashPoints[k] = true
	}
	for i, e := range events {
		if e.sync {
			for d := -3; d <= 3; d++ {
				if k := i + d; k >= 0 && k <= len(events) {
					crashPoints[k] = true
				}
			}
		}
	}
	for k := 0; k <= len(events); k++ {
		if !crashPoints[k] {
			continue
		}
		lastSync := 0
		for i := 0; i < k; i++ {
			if events[i].sync {
				lastSync = i + 1
			}
		}
		check(fmt.Sprintf("fsynced-only@%d", k), apply(events[:lastSync]))
		check(fmt.Sprintf("in-order@%d", k), apply(events[:k]))
		// The open group: the writes after the last barrier before the crash point.
		var group []crashEvent
		for _, e := range events[lastSync:k] {
			if !e.sync {
				group = append(group, e)
			}
		}
		durable := events[:lastSync]
		if len(group) > 12 { // the group of blocks is large; sample its ends
			group = append(slices.Clone(group[:6]), group[len(group)-6:]...)
		}
		for i := range group {
			only := append(slices.Clone(durable), group[i])
			check(fmt.Sprintf("subset-one@%d.%d", k, i), apply(only))
			var allBut []crashEvent
			allBut = append(allBut, durable...)
			for j, e := range group {
				if j != i {
					allBut = append(allBut, e)
				}
			}
			check(fmt.Sprintf("subset-allbut@%d.%d", k, i), apply(allBut))
		}
	}
	if checked < 50 {
		t.Fatalf("the crash model checked only %d images", checked)
	}
	return bad
}

// safeDigests is fileDigests returning an error instead of failing the test.
func safeDigests(v *Volume) (out []string, err error) {
	ents, err := v.ListDir("/")
	if err != nil {
		return nil, err
	}
	for _, e := range ents {
		if !strings.HasPrefix(e.Name(), "f") {
			continue
		}
		b, err := v.ReadFile("/" + e.Name())
		if err != nil {
			return nil, err
		}
		out = append(out, fmt.Sprintf("%s %d %x", e.Name(), len(b), sha256.Sum256(b)))
	}
	sort.Strings(out)
	return out, nil
}

func fixtureReplayEvents(t *testing.T) (orig []byte, events []crashEvent, loc journalLoc) {
	t.Helper()
	orig, _, _ = macNonEmptyFixture(t)
	vh, _ := readVolumeHeader(bytesReaderAt(orig))
	_, loc, _ = loadJournalAt(bytesReaderAt(orig), int64(len(orig)), vh)
	cs := &crashStore{img: bytes.Clone(orig)}
	if err := replayPending(cs, int64(len(orig))); err != nil {
		t.Fatal(err)
	}
	return orig, cs.events, loc
}

// The order of the replay's writes: blocks, barrier, header, barrier.
func TestReplayWriteOrder(t *testing.T) {
	_, events, loc := fixtureReplayEvents(t)
	var syncs []int
	for i, e := range events {
		if e.sync {
			syncs = append(syncs, i)
		}
	}
	if len(syncs) != 2 || syncs[1] != len(events)-1 {
		t.Fatalf("barriers at %v of %d events, want exactly two, the second last", syncs, len(events))
	}
	if syncs[0] < 1 || syncs[1] != syncs[0]+2 {
		t.Fatalf("barriers at %v: want blocks, barrier, header, barrier", syncs)
	}
	for i := 0; i < syncs[0]; i++ {
		if events[i].off >= int64(loc.off) && events[i].off < int64(loc.off+loc.hdr.size) {
			t.Errorf("event %d writes inside the journal before the first barrier", i)
		}
	}
	h := events[syncs[0]+1]
	if h.off != int64(loc.off) || len(h.data) != int(loc.hdr.jhdrSize) {
		t.Fatalf("event after the first barrier writes %d bytes at %d, want the %d-byte journal header at %d", len(h.data), h.off, loc.hdr.jhdrSize, loc.off)
	}
	nh, err := decodeJournalHeader(h.data)
	if err != nil {
		t.Fatalf("the rewritten header does not verify: %v", err)
	}
	if nh.start != nh.end || nh.start%512 != 0 || nh.size != loc.hdr.size || nh.blhdrSize != loc.hdr.blhdrSize || nh.sequenceNum != loc.hdr.sequenceNum {
		t.Errorf("rewritten header %+v from %+v", nh, loc.hdr)
	}
}

// The crash model over the replay's writes, and its mutation: without the barrier
// between the blocks and the header the model must find an empty header over
// blocks that did not land.
func TestReplayCrashModel(t *testing.T) {
	orig, events, _ := fixtureReplayEvents(t)
	golden := goldenList(t)
	if bad := crashViolations(t, orig, events, golden); len(bad) > 0 {
		t.Fatalf("%d violations, first: %s", len(bad), bad[0])
	}
	t.Run("mutation: no barrier between the blocks and the header", func(t *testing.T) {
		var mut []crashEvent
		dropped := false
		for _, e := range events {
			if e.sync && !dropped {
				dropped = true
				continue
			}
			mut = append(mut, e)
		}
		bad := crashViolations(t, orig, mut, golden)
		if len(bad) == 0 {
			t.Fatal("the crash model stayed green with the barrier dropped")
		}
		// The state it must find: the header alone landed, so the journal is empty and
		// the volume shows the pre-replay listing (the golden files are lost).
		found := false
		for _, b := range bad {
			if strings.Contains(b, "subset-one") && strings.Contains(b, "differs from golden") {
				found = true
			}
		}
		if !found {
			t.Errorf("violations found, but none is a header that landed alone: %s", bad[0])
		}
		t.Logf("mutation: %d violations, e.g. %s", len(bad), bad[0])
	})
	t.Run("mutation: header before the blocks", func(t *testing.T) {
		// The header written first, then the barrier, then the blocks.
		n := len(events)
		mut := slices.Clone(events)
		// events: blocks..., sync, header, sync -> header, sync, blocks..., sync
		hdr := events[n-2]
		blocks := events[:n-3]
		mut = append([]crashEvent{hdr, {sync: true}}, blocks...)
		mut = append(mut, crashEvent{sync: true})
		if bad := crashViolations(t, orig, mut, golden); len(bad) == 0 {
			t.Fatal("the crash model stayed green with the header written before the blocks")
		}
	})
}
