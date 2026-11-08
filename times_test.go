package hfsplus

import (
	"compress/gzip"
	"errors"
	"io"
	iofs "io/fs"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	filesystem "github.com/go-filesystems/interface"
)

// timesFixture is an HFS+ image written by Apple's reference implementation:
// newfs_hfs on a raw disk image attached with hdiutil, mounted by diskutil,
// populated with files whose atime/mtime were set by os.Chtimes to the instants
// below. createDate and attributeModDate are stamped by the kernel with the
// wall clock at generation, so only their plausibility is asserted.
// Regenerate on macOS with HFSPLUS_REGEN_TIMES=1.
const timesFixture = "testdata/times-hfsplus.img.gz"

var timesSources = []struct {
	name  string
	mtime time.Time
	atime time.Time
}{
	{"A.TXT", time.Date(2024, 5, 17, 13, 45, 30, 0, time.UTC), time.Date(2024, 5, 18, 8, 0, 10, 0, time.UTC)},
	{"OLD.TXT", time.Date(1960, 6, 15, 1, 2, 3, 0, time.UTC), time.Date(1990, 1, 1, 0, 0, 0, 0, time.UTC)},
	{"LATE.TXT", time.Date(2039, 12, 31, 23, 59, 59, 0, time.UTC), time.Date(2039, 12, 31, 23, 59, 58, 0, time.UTC)},
}

func TestTimesRegenerateFixture(t *testing.T) {
	if os.Getenv("HFSPLUS_REGEN_TIMES") != "1" {
		t.Skip("set HFSPLUS_REGEN_TIMES=1 (macOS) to rebuild " + timesFixture)
	}
	img := filepath.Join(t.TempDir(), "t.img")
	if err := os.WriteFile(img, nil, 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.Truncate(img, 16<<20); err != nil {
		t.Fatal(err)
	}
	run := func(name string, args ...string) string {
		t.Helper()
		cmd := exec.Command(name, args...)
		var stderr strings.Builder
		cmd.Stderr = &stderr
		out, err := cmd.Output()
		if err != nil {
			t.Fatalf("%s %v: %v\n%s%s", name, args, err, out, stderr.String())
		}
		return string(out)
	}
	disk := strings.TrimSpace(run("hdiutil", "attach", "-imagekey", "diskimage-class=CRawDiskImage", "-nomount", img))
	detach := func() { _ = exec.Command("hdiutil", "detach", disk).Run() }
	defer detach()
	run("newfs_hfs", "-v", "TIMES", disk)
	run("diskutil", "mount", disk)
	mnt := "/Volumes/TIMES"
	if err := os.Mkdir(filepath.Join(mnt, "DIR"), 0o755); err != nil {
		t.Fatal(err)
	}
	dirTime := time.Date(2022, 2, 3, 4, 5, 6, 0, time.UTC)
	if err := os.Chtimes(filepath.Join(mnt, "DIR"), dirTime, dirTime); err != nil {
		t.Fatal(err)
	}
	for _, s := range timesSources {
		p := filepath.Join(mnt, s.name)
		if err := os.WriteFile(p, []byte("x"), 0o644); err != nil {
			t.Fatal(err)
		}
		if err := os.Chtimes(p, s.atime, s.mtime); err != nil {
			t.Fatal(err)
		}
	}
	run("diskutil", "unmount", mnt)
	detach()
	in, err := os.Open(img)
	if err != nil {
		t.Fatal(err)
	}
	defer in.Close()
	out, err := os.Create(timesFixture)
	if err != nil {
		t.Fatal(err)
	}
	zw, _ := gzip.NewWriterLevel(out, gzip.BestCompression)
	if _, err := io.Copy(zw, in); err != nil {
		t.Fatal(err)
	}
	if err := zw.Close(); err != nil {
		t.Fatal(err)
	}
	out.Close()
}

func openTimesFixture(t *testing.T) filesystem.TimesReader {
	t.Helper()
	gz, err := os.Open(timesFixture)
	if err != nil {
		t.Fatal(err)
	}
	defer gz.Close()
	zr, err := gzip.NewReader(gz)
	if err != nil {
		t.Fatal(err)
	}
	img := filepath.Join(t.TempDir(), "t.img")
	out, err := os.Create(img)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := io.Copy(out, zr); err != nil {
		t.Fatal(err)
	}
	out.Close()
	v, err := OpenFile(img)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { v.Close() })
	var tr filesystem.TimesReader = v
	return tr
}

// TestTimesReferenceImage checks the decoder against records the reference
// implementation wrote for known source times.
func TestTimesReferenceImage(t *testing.T) {
	tr := openTimesFixture(t)
	for _, s := range timesSources {
		ft, err := tr.Times("/" + s.name)
		if err != nil {
			t.Fatalf("%s: %v", s.name, err)
		}
		t.Logf("%s: %+v", s.name, ft)
		if m, ok := ft.Mtime.Get(); !ok || !m.Equal(s.mtime) {
			t.Errorf("%s mtime = %v, want %v", s.name, m, s.mtime)
		}
		if a, ok := ft.Atime.Get(); !ok || !a.Equal(s.atime) {
			t.Errorf("%s atime = %v, want %v", s.name, a, s.atime)
		}
		for n, ts := range map[string]filesystem.Timestamp{"btime": ft.Btime, "mtime": ft.Mtime, "ctime": ft.Ctime, "atime": ft.Atime} {
			if ts.Recorded() && ts.Resolution() != time.Second {
				t.Errorf("%s %s resolution %v", s.name, n, ts.Resolution())
			}
		}
		// The kernel stamped attributeModDate with the wall clock at generation
		// and createDate likewise, then lowered createDate to an earlier
		// mtime (Darwin does that), so Btime <= Ctime.
		c, cok := ft.Ctime.Get()
		b, bok := ft.Btime.Get()
		if !cok || c.Year() < 2026 || c.After(time.Now().Add(24*time.Hour)) || !bok || b.After(c) {
			t.Errorf("%s btime = %v ctime = %v", s.name, b, c)
		}
	}
	d, err := tr.Times("/DIR")
	if err != nil {
		t.Fatal(err)
	}
	if m, _ := d.Mtime.Get(); !m.Equal(time.Date(2022, 2, 3, 4, 5, 6, 0, time.UTC)) {
		t.Errorf("DIR mtime = %v", m)
	}
}

func TestTimesRootAndMissing(t *testing.T) {
	tr := openTimesFixture(t)
	if ft, err := tr.Times("/"); err != nil || ft != (filesystem.FileTimes{}) {
		t.Fatalf("root: %+v %v", ft, err)
	}
	if _, err := tr.Times("/NOPE.TXT"); !errors.Is(err, iofs.ErrNotExist) && !errors.Is(err, ErrNotFound) {
		t.Fatalf("err = %v", err)
	}
	if _, err := tr.Times("/A.TXT/x"); err == nil {
		t.Fatal("a file is not a directory")
	}
}

func datesBytes(c, m, a, ac, b uint32) []byte {
	out := make([]byte, 20)
	for i, v := range []uint32{c, m, a, ac, b} {
		out[i*4], out[i*4+1], out[i*4+2], out[i*4+3] = byte(v>>24), byte(v>>16), byte(v>>8), byte(v)
	}
	return out
}

// TestDecodeHFSTimes covers the encoding exactly with hand-built words.
func TestDecodeHFSTimes(t *testing.T) {
	// 1970-01-01 is 2082844800 s after 1904-01-01.
	ft := decodeHFSTimes(datesBytes(2082844800, 2082844801, 0, 0xFFFFFFFF, 7))
	if b, ok := ft.Btime.Get(); !ok || b.Unix() != 0 {
		t.Errorf("btime = %v", b)
	}
	if m, _ := ft.Mtime.Get(); m.Unix() != 1 {
		t.Errorf("mtime = %v", m)
	}
	if ft.Ctime.Recorded() {
		t.Error("attributeModDate 0 must be unrecorded")
	}
	// The maximum date, TN1150: 2040-02-06 06:28:15 GMT.
	if a, _ := ft.Atime.Get(); !a.Equal(time.Date(2040, 2, 6, 6, 28, 15, 0, time.UTC)) {
		t.Errorf("max date = %v", a)
	}
	// Second 1 is 1904-01-01 00:00:01.
	if g, _ := decodeHFSDate(1).Get(); !g.Equal(time.Date(1904, 1, 1, 0, 0, 1, 0, time.UTC)) {
		t.Errorf("date 1 = %v", g)
	}
	if decodeHFSDate(0).Recorded() {
		t.Error("date 0")
	}
	if decodeHFSDate(5).Resolution() != time.Second {
		t.Error("resolution")
	}
}

// TestCatalogRecordKeepsDates: folder and file records carry bytes 12..32.
func TestCatalogRecordKeepsDates(t *testing.T) {
	d := datesBytes(1, 2, 3, 4, 5)
	fol := make([]byte, 88)
	fol[1] = byte(recordFolder)
	copy(fol[12:], d)
	r, ok := parseCatalogRecord(fol)
	if !ok || r.folder == nil || r.folder.dates != [20]byte(d) {
		t.Fatalf("folder dates: %+v", r.folder)
	}
	// A short folder record has no dates: zero, not a panic.
	short := make([]byte, 20)
	short[1] = byte(recordFolder)
	if r, ok := parseCatalogRecord(short); !ok || r.folder.dates != [20]byte{} {
		t.Fatal("short folder")
	}
	file := make([]byte, 168+forkDataLen)
	file[1] = byte(recordFile)
	copy(file[12:], d)
	r, ok = parseCatalogRecord(file)
	if !ok || r.file == nil || r.file.dates != [20]byte(d) {
		t.Fatal("file dates")
	}
}

func FuzzDecodeHFSTimes(f *testing.F) {
	f.Add(make([]byte, 20))
	f.Add(datesBytes(0xFFFFFFFF, 0xFFFFFFFF, 0xFFFFFFFF, 0xFFFFFFFF, 0xFFFFFFFF))
	f.Add(datesBytes(2082844800, 1, 0, 0xE0E0E0E0, 3))
	f.Fuzz(func(t *testing.T, b []byte) {
		if len(b) < 20 {
			return
		}
		ft := decodeHFSTimes(b[:20])
		for _, ts := range []filesystem.Timestamp{ft.Btime, ft.Mtime, ft.Ctime, ft.Atime} {
			if g, ok := ts.Get(); ok {
				if ts.Resolution() != time.Second || g.Year() < 1904 || g.Year() > 2040 {
					t.Fatalf("recorded %v from %x", g, b[:20])
				}
			}
		}
	})
}
