package hfsplus

import (
	"errors"
	"fmt"
	iofs "io/fs"
	"path/filepath"
	"testing"
	"time"
)

func formatChtimes(t *testing.T) (*Volume, string) {
	t.Helper()
	p := filepath.Join(t.TempDir(), "ct.img")
	fs, err := Format(p, 16<<20, FormatConfig{Label: "CT"})
	if err != nil {
		t.Fatal(err)
	}
	v := fs.(*Volume)
	for _, f := range []string{"/a.txt", "/b.txt"} {
		if err := v.WriteFile(f, []byte("x"), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	if err := v.MkDir("/sub", 0o755); err != nil {
		t.Fatal(err)
	}
	if err := v.WriteFile("/sub/c.txt", []byte("x"), 0o644); err != nil {
		t.Fatal(err)
	}
	return v, p
}

func timesAt(t *testing.T, v *Volume, p string) (b, m, c, a time.Time) {
	t.Helper()
	ft, err := v.Times(p)
	if err != nil {
		t.Fatalf("Times(%s): %v", p, err)
	}
	b, _ = ft.Btime.Get()
	m, _ = ft.Mtime.Get()
	c, _ = ft.Ctime.Get()
	a, _ = ft.Atime.Get()
	return
}

func pinNow(t *testing.T, now time.Time) {
	old := hfsNow
	hfsNow = func() time.Time { return now }
	t.Cleanup(func() { hfsNow = old })
}

// TestChtimesRoundTrip: Times reports the whole seconds that were set, for a
// file, a nested file and a directory, attributeModDate (Ctime) is refreshed to
// the clock and createDate (Btime) is untouched; it survives a reopen.
func TestChtimesRoundTrip(t *testing.T) {
	now := time.Date(2031, 4, 5, 6, 7, 8, 0, time.UTC)
	pinNow(t, now)
	v, path := formatChtimes(t)
	at := time.Date(2024, 5, 18, 8, 0, 10, 999_000_000, time.FixedZone("x", 7200))
	mt := time.Date(1960, 6, 15, 1, 2, 3, 5, time.UTC)
	for _, p := range []string{"/a.txt", "/sub", "/sub/c.txt"} {
		b0, _, _, _ := timesAt(t, v, p)
		if err := v.Chtimes(p, at, mt); err != nil {
			t.Fatalf("%s: %v", p, err)
		}
		b, m, c, a := timesAt(t, v, p)
		if !a.Equal(at.Truncate(time.Second)) || !m.Equal(mt.Truncate(time.Second)) {
			t.Errorf("%s: atime %v mtime %v", p, a, m)
		}
		if !c.Equal(now) {
			t.Errorf("%s: ctime %v, want the clock %v", p, c, now)
		}
		if !b.Equal(b0) {
			t.Errorf("%s: createDate changed %v -> %v", p, b0, b)
		}
	}
	v.Close()
	re, err := OpenFile(path)
	if err != nil {
		t.Fatal(err)
	}
	defer re.Close()
	if _, m, _, _ := timesAt(t, re, "/sub/c.txt"); !m.Equal(mt.Truncate(time.Second)) {
		t.Errorf("after reopen: %v", m)
	}
	// The neighbours still read and list.
	if got, err := re.ReadFile("/b.txt"); err != nil || string(got) != "x" {
		t.Errorf("neighbour: %q %v", got, err)
	}
}

func TestChtimesRangeEdges(t *testing.T) {
	v, _ := formatChtimes(t)
	defer v.Close()
	first := time.Date(1904, 1, 1, 0, 0, 1, 0, time.UTC)
	last := time.Date(2040, 2, 6, 6, 28, 15, 999_999_999, time.UTC)
	for _, tm := range []time.Time{first, last, time.Date(2000, 2, 29, 12, 0, 1, 0, time.UTC)} {
		if err := v.Chtimes("/a.txt", tm, tm); err != nil {
			t.Fatalf("%v: %v", tm, err)
		}
		_, m, _, a := timesAt(t, v, "/a.txt")
		if !m.Equal(tm.Truncate(time.Second)) || !a.Equal(tm.Truncate(time.Second)) {
			t.Errorf("%v: got atime %v mtime %v", tm, a, m)
		}
	}
	_, m0, _, a0 := timesAt(t, v, "/a.txt")
	for _, tm := range []time.Time{
		time.Date(1904, 1, 1, 0, 0, 0, 0, time.UTC), // 0 reads back as "not recorded"
		time.Date(1904, 1, 1, 0, 0, 0, 999_999_999, time.UTC),
		time.Date(1903, 12, 31, 23, 59, 59, 0, time.UTC),
		time.Date(2040, 2, 6, 6, 28, 16, 0, time.UTC),
		time.Date(2107, 1, 1, 0, 0, 0, 0, time.UTC),
		time.Unix(-1<<40, 0),
		time.Unix(1<<40, 0),
	} {
		for _, which := range []string{"atime", "mtime"} {
			good := time.Date(2001, 1, 1, 0, 0, 0, 0, time.UTC)
			var err error
			if which == "atime" {
				err = v.Chtimes("/a.txt", tm, good)
			} else {
				err = v.Chtimes("/a.txt", good, tm)
			}
			var re *TimeRangeError
			if !errors.Is(err, ErrTimeOutOfRange) || !errors.As(err, &re) || re.Field != which {
				t.Errorf("%v as %s: err = %v", tm, which, err)
			}
			if _, m, _, a := timesAt(t, v, "/a.txt"); !a.Equal(a0) || !m.Equal(m0) {
				t.Errorf("%v as %s: a refused call changed the times", tm, which)
			}
		}
	}
}

func TestChtimesZeroLeavesUnchanged(t *testing.T) {
	pinNow(t, time.Date(2031, 4, 5, 6, 7, 8, 0, time.UTC))
	v, _ := formatChtimes(t)
	defer v.Close()
	at := time.Date(2020, 3, 4, 5, 6, 7, 0, time.UTC)
	mt := time.Date(2022, 9, 10, 11, 12, 13, 0, time.UTC)
	if err := v.Chtimes("/a.txt", at, mt); err != nil {
		t.Fatal(err)
	}
	mt2 := time.Date(2023, 1, 2, 3, 4, 5, 0, time.UTC)
	if err := v.Chtimes("/a.txt", time.Time{}, mt2); err != nil {
		t.Fatal(err)
	}
	if _, m, _, a := timesAt(t, v, "/a.txt"); !a.Equal(at) || !m.Equal(mt2) {
		t.Errorf("zero atime: %v %v", a, m)
	}
	at2 := time.Date(2024, 6, 7, 8, 9, 10, 0, time.UTC)
	if err := v.Chtimes("/a.txt", at2, time.Time{}); err != nil {
		t.Fatal(err)
	}
	if _, m, _, a := timesAt(t, v, "/a.txt"); !a.Equal(at2) || !m.Equal(mt2) {
		t.Errorf("zero mtime: %v %v", a, m)
	}
	// Both zero touches nothing, ctime included.
	pinNow(t, time.Date(2035, 1, 1, 0, 0, 0, 0, time.UTC))
	_, _, c0, _ := timesAt(t, v, "/a.txt")
	if err := v.Chtimes("/a.txt", time.Time{}, time.Time{}); err != nil {
		t.Fatal(err)
	}
	if _, m, c, a := timesAt(t, v, "/a.txt"); !a.Equal(at2) || !m.Equal(mt2) || !c.Equal(c0) {
		t.Errorf("both zero: %v %v %v", a, m, c)
	}
}

func TestChtimesPathsAndClock(t *testing.T) {
	v, _ := formatChtimes(t)
	defer v.Close()
	now := time.Now()
	for _, p := range []string{"/nope.txt", "/nope/x.txt"} {
		if err := v.Chtimes(p, now, now); !errors.Is(err, iofs.ErrNotExist) {
			t.Errorf("%s: %v, want ErrNotExist", p, err)
		}
	}
	if err := v.Chtimes("/nope.txt", time.Time{}, time.Time{}); !errors.Is(err, iofs.ErrNotExist) {
		t.Errorf("zero times on a missing path: %v", err)
	}
	if err := v.Chtimes("/", now, now); err == nil {
		t.Error("the root is not served: want an error")
	}
	if err := v.Chtimes("/a.txt/x", now, now); err == nil {
		t.Error("a file is not a directory")
	}
	// A clock outside the format's range is an error, not a clamped ctime.
	pinNow(t, time.Date(2100, 1, 1, 0, 0, 0, 0, time.UTC))
	var re *TimeRangeError
	if err := v.Chtimes("/a.txt", now, now); !errors.As(err, &re) || re.Field != "now" {
		t.Errorf("clock out of range: %v", err)
	}
}

func TestChtimesReadOnly(t *testing.T) {
	v, path := formatChtimes(t)
	v.Close()
	ro, err := OpenFile(path)
	if err != nil {
		t.Fatal(err)
	}
	defer ro.Close()
	now := time.Now()
	if err := ro.Chtimes("/a.txt", now, now); !errors.Is(err, ErrReadOnly) {
		t.Errorf("Chtimes on a read-only volume = %v, want ErrReadOnly", err)
	}
}

type failWriter struct {
	inner interface {
		WriteAt([]byte, int64) (int, error)
	}
	fail bool
}

var errWrite = errors.New("hfsplus: injected write error")

func (f *failWriter) WriteAt(p []byte, off int64) (int, error) {
	if f.fail {
		return 0, errWrite
	}
	return f.inner.WriteAt(p, off)
}

// TestChtimesWriteFailure: the image reaches the disk in one Sync, so a failed
// write returns the error and the file on disk keeps its old times and is
// still a valid volume that takes the change on the next try.
func TestChtimesWriteFailure(t *testing.T) {
	v, path := formatChtimes(t)
	old := time.Date(2010, 1, 2, 3, 4, 5, 0, time.UTC)
	if err := v.Chtimes("/sub/c.txt", old, old); err != nil {
		t.Fatal(err)
	}
	fw := &failWriter{inner: v.wa, fail: true}
	v.wa = fw
	nw := time.Date(2030, 7, 8, 9, 10, 11, 0, time.UTC)
	if err := v.Chtimes("/sub/c.txt", nw, nw); !errors.Is(err, errWrite) {
		t.Fatalf("err = %v, want the injected write error", err)
	}
	v.Close()
	re, err := OpenFile(path)
	if err != nil {
		t.Fatalf("the volume no longer opens: %v", err)
	}
	if _, m, _, a := timesAt(t, re, "/sub/c.txt"); !m.Equal(old) || !a.Equal(old) {
		t.Errorf("on disk after the failed write: %v %v", a, m)
	}
	re.Close()
	w, err := OpenFileWritable(path)
	if err != nil {
		t.Fatal(err)
	}
	defer w.Close()
	if err := w.Chtimes("/sub/c.txt", nw, nw); err != nil {
		t.Fatalf("next try: %v", err)
	}
	if _, m, _, _ := timesAt(t, w, "/sub/c.txt"); !m.Equal(nw) {
		t.Errorf("mtime %v", m)
	}
}

// TestChtimesManyRecords sets times on every file of a catalog large enough to
// span several leaf nodes: every record is patched in place, nothing else moves,
// and the whole tree still lists and reads.
func TestChtimesManyRecords(t *testing.T) {
	p := filepath.Join(t.TempDir(), "many.img")
	fs, err := Format(p, 16<<20, FormatConfig{Label: "MANY"})
	if err != nil {
		t.Fatal(err)
	}
	v := fs.(*Volume)
	defer v.Close()
	const n = 300
	for i := 0; i < n; i++ {
		if err := v.WriteFile(fmt.Sprintf("/file-%03d.txt", i), []byte{byte(i)}, 0o644); err != nil {
			t.Fatal(err)
		}
	}
	base := time.Date(2001, 1, 1, 0, 0, 0, 0, time.UTC)
	for i := 0; i < n; i++ {
		tm := base.Add(time.Duration(i) * time.Hour)
		if err := v.Chtimes(fmt.Sprintf("/file-%03d.txt", i), tm, tm); err != nil {
			t.Fatalf("file %d: %v", i, err)
		}
	}
	ents, err := v.ListDir("/")
	if err != nil || len(ents) != n {
		t.Fatalf("ListDir: %d entries, %v", len(ents), err)
	}
	for i := 0; i < n; i++ {
		name := fmt.Sprintf("/file-%03d.txt", i)
		_, m, _, a := timesAt(t, v, name)
		tm := base.Add(time.Duration(i) * time.Hour)
		if !m.Equal(tm) || !a.Equal(tm) {
			t.Fatalf("%s: %v %v, want %v", name, a, m, tm)
		}
		if got, err := v.ReadFile(name); err != nil || len(got) != 1 || got[0] != byte(i) {
			t.Fatalf("%s: content %v %v", name, got, err)
		}
	}
}

// TestEncodeHFSDate: the encoder is the inverse of decodeHFSDate over the whole
// range, at its ends.
func TestEncodeHFSDate(t *testing.T) {
	for _, d := range []uint32{1, 2, 0x7FFFFFFF, 0xFFFFFFFE, 0xFFFFFFFF} {
		got, ok := decodeHFSDate(d).Get()
		if !ok {
			t.Fatalf("%d: not recorded", d)
		}
		back, err := encodeHFSDate("x", got)
		if err != nil || back != d {
			t.Errorf("%d: encode(decode) = %d, %v", d, back, err)
		}
	}
}

func FuzzEncodeHFSDate(f *testing.F) {
	f.Add(int64(0), int64(0))
	f.Add(int64(-2082844799), int64(0))   // 1904-01-01 00:00:01
	f.Add(int64(-2082844800), int64(999)) // 1904-01-01 00:00:00.999: refused
	f.Add(int64(2212122495), int64(999_999_999))
	f.Add(int64(2212122496), int64(0)) // one past the end
	f.Fuzz(func(t *testing.T, sec, nsec int64) {
		if sec < -1<<40 || sec > 1<<40 {
			t.Skip()
		}
		tm := time.Unix(sec, nsec%1_000_000_000)
		d, err := encodeHFSDate("f", tm)
		if err != nil {
			var re *TimeRangeError
			if !errors.As(err, &re) || !errors.Is(err, ErrTimeOutOfRange) {
				t.Fatalf("untyped error %v", err)
			}
			// Refused exactly when the whole second is outside 1..2^32-1.
			if s := tm.Unix() - hfsEpochToUnix; s >= 1 && s <= 0xFFFFFFFF {
				t.Fatalf("%v refused although it encodes", tm)
			}
			return
		}
		got, ok := decodeHFSDate(d).Get()
		if !ok || !got.Equal(tm.Truncate(time.Second)) {
			t.Fatalf("%v encoded to %d decodes to %v (%v)", tm, d, got, ok)
		}
	})
}
