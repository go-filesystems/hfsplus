//go:build darwin

package hfsplus

import (
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"time"
)

// TestDarwinChtimes has Apple's HFS+ implementation read the result: fsck_hfs
// -n reports the volume clean after the catalog records were patched, and the
// mounted files' stat shows the times that were set, including both ends of the
// range and a pre-1970 time.
func TestDarwinChtimes(t *testing.T) {
	requireTools(t)
	p := filepath.Join(t.TempDir(), "ct.img")
	v := formatFileVolume(t, p, 16<<20, FormatConfig{Label: "GOCT"})
	if err := v.MkDir("/sub", 0o755); err != nil {
		t.Fatal(err)
	}
	cases := map[string][2]time.Time{ // atime, mtime
		"/a.txt":     {time.Date(2024, 5, 18, 8, 0, 10, 0, time.UTC), time.Date(2024, 5, 17, 13, 45, 30, 0, time.UTC)},
		"/old.txt":   {time.Date(1904, 1, 1, 0, 0, 1, 0, time.UTC), time.Date(1960, 6, 15, 1, 2, 3, 0, time.UTC)},
		"/late.txt":  {time.Date(2040, 2, 6, 6, 28, 15, 0, time.UTC), time.Date(2039, 12, 31, 23, 59, 59, 0, time.UTC)},
		"/sub/n.txt": {time.Date(2001, 2, 3, 4, 5, 6, 0, time.UTC), time.Date(2002, 3, 4, 5, 6, 7, 0, time.UTC)},
	}
	for name, v2 := range cases {
		if err := v.WriteFile(name, []byte("x"), 0o644); err != nil {
			t.Fatal(err)
		}
		if err := v.Chtimes(name, v2[0], v2[1]); err != nil {
			t.Fatalf("%s: %v", name, err)
		}
	}
	dirT := time.Date(2022, 2, 3, 4, 5, 6, 0, time.UTC)
	if err := v.Chtimes("/sub", dirT, dirT); err != nil {
		t.Fatal(err)
	}
	if err := v.Chmod("/a.txt", 0o640); err != nil {
		t.Fatal(err)
	}
	if err := v.Chmod("/sub", 0o750); err != nil {
		t.Fatal(err)
	}
	v.Close()

	// hdiutil warns on stderr on some hosts, so only its stdout names the device
	// (attachRaw reads both streams and takes the first word of the first line).
	attach := func(args ...string) string {
		cmd := exec.Command("hdiutil", append([]string{"attach", "-imagekey", "diskimage-class=CRawDiskImage"}, args...)...)
		var stderr strings.Builder
		cmd.Stderr = &stderr
		out, err := cmd.Output()
		if err != nil {
			t.Fatalf("hdiutil attach %v: %v: %s%s", args, err, out, stderr.String())
		}
		return strings.Fields(strings.SplitN(string(out), "\n", 2)[0])[0]
	}
	dev := attach("-nomount", "-readonly", p)
	fsckClean(t, dev)
	detach(dev)

	dev, mnt := mountRaw(t, p)
	defer detach(dev)
	waitMount(mnt)
	stat := func(p string) (a, m int64) {
		// stat -f %a/%m: seconds since the epoch (negative before 1970).
		o, err := exec.Command("stat", "-f", "%a %m", filepath.Join(mnt, p)).CombinedOutput()
		if err != nil {
			t.Fatalf("stat %s: %v: %s", p, err, o)
		}
		f := strings.Fields(string(o))
		a, _ = strconv.ParseInt(f[0], 10, 64)
		m, _ = strconv.ParseInt(f[1], 10, 64)
		return a, m
	}
	// Darwin's BSD layer reports a catalog date before 1970 as 0 (the date is on
	// disk, as fsck_hfs and Times show; stat cannot say it), so that is what it
	// is expected to print for those.
	unix := func(t time.Time) int64 { return max(t.Unix(), 0) }
	for name, v2 := range cases {
		a, m := stat(name)
		if m != unix(v2[1]) {
			t.Errorf("%s: macOS mtime %d, want %d", name, m, unix(v2[1]))
		}
		if a != unix(v2[0]) {
			t.Errorf("%s: macOS atime %d, want %d", name, a, unix(v2[0]))
		}
	}
	// Chtimes ran before Chmod on /sub and /a.txt: the mtime survives Chmod.
	if a, m := stat("/sub"); m != dirT.Unix() || a != dirT.Unix() {
		t.Errorf("/sub: macOS atime %d mtime %d, want %d", a, m, dirT.Unix())
	}
	for name, want := range map[string]string{"/a.txt": "640", "/sub": "750"} {
		o, err := exec.Command("stat", "-f", "%Lp", filepath.Join(mnt, name)).CombinedOutput()
		if err != nil || strings.TrimSpace(string(o)) != want {
			t.Errorf("%s: macOS mode %q (%v), want %s", name, o, err, want)
		}
	}
}
