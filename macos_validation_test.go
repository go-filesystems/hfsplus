// Copyright (c) 2026, go-filesystems
// SPDX-License-Identifier: BSD-3-Clause

//go:build darwin

package hfsplus

import (
	"bytes"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// macos_validation_test.go drives the native macOS tooling (hdiutil + fsck_hfs)
// to prove that the pure-Go formatter and write path produce on-disk images
// macOS accepts: fsck_hfs -n must report the volume clean, and macOS must mount
// the image read/write and see the exact files/bytes the Go side wrote. It also
// round-trips the other direction (hdiutil-created image → Go writes → fsck +
// macOS read). These tests are darwin-only; the cross-arch pure-Go round-trip
// lives in write_test.go and runs everywhere.

// attachRaw attaches a raw HFS+ image without mounting and returns its /dev
// node. The caller must detach.
func attachRaw(t *testing.T, path string, readonly bool) string {
	t.Helper()
	args := []string{"attach", "-imagekey", "diskimage-class=CRawDiskImage", "-nomount", path}
	if readonly {
		args = append(args, "-readonly")
	}
	out, err := exec.Command("hdiutil", args...).CombinedOutput()
	if err != nil {
		t.Fatalf("hdiutil attach %s: %v: %s", path, err, out)
	}
	dev := strings.Fields(strings.SplitN(string(out), "\n", 2)[0])[0]
	return dev
}

func detach(dev string) { _ = exec.Command("hdiutil", "detach", dev).Run() }

// fsckClean runs fsck_hfs -n on a /dev node and fails unless the volume is
// reported OK. fsck_hfs cannot open the raw character device for *write* in the
// unprivileged sandbox ("NO WRITE"), but the read-only verification still runs
// and prints its verdict; we assert on that verdict line.
func fsckClean(t *testing.T, dev string) {
	t.Helper()
	out, _ := exec.Command("fsck_hfs", "-n", dev).CombinedOutput()
	s := string(out)
	if !strings.Contains(s, "appears to be OK") {
		t.Fatalf("fsck_hfs not clean:\n%s", s)
	}
}

func requireTools(t *testing.T) {
	t.Helper()
	for _, tool := range []string{"hdiutil", "fsck_hfs"} {
		if _, err := exec.LookPath(tool); err != nil {
			t.Skipf("%s unavailable", tool)
		}
	}
}

// TestDarwinMkfsFsckClean: pure-Go Format → fsck_hfs -n clean for both HFS+
// and HFSX.
func TestDarwinMkfsFsckClean(t *testing.T) {
	requireTools(t)
	for _, cs := range []bool{false, true} {
		name := "hfsplus"
		if cs {
			name = "hfsx"
		}
		p := filepath.Join(t.TempDir(), name+".img")
		fs, err := Format(p, 16<<20, FormatConfig{Label: "GOTEST", CaseSensitive: cs})
		if err != nil {
			t.Fatalf("Format cs=%v: %v", cs, err)
		}
		fs.Close()
		dev := attachRaw(t, p, true)
		fsckClean(t, dev)
		detach(dev)
	}
}

// TestDarwinWriteRoundTrip: pure-Go Format + writes → fsck clean → macOS mounts
// RW and reads the exact bytes.
func TestDarwinWriteRoundTrip(t *testing.T) {
	requireTools(t)
	p := filepath.Join(t.TempDir(), "rw.img")
	fs, err := Format(p, 24<<20, FormatConfig{Label: "GORW"})
	if err != nil {
		t.Fatalf("Format: %v", err)
	}
	files := map[string][]byte{
		"/hello.txt":      []byte("hello from pure-go hfsplus\n"),
		"/sub/nested.txt": []byte("nested content\n"),
		"/big.bin":        bytes.Repeat([]byte{0x5A, 0xA5}, 60000),
	}
	if err := fs.MkDir("/sub", 0o755); err != nil {
		t.Fatal(err)
	}
	for p2, data := range files {
		if err := fs.WriteFile(p2, data, 0o644); err != nil {
			t.Fatalf("WriteFile %s: %v", p2, err)
		}
	}
	if s, ok := fs.(interface {
		Symlink(string, string) error
	}); ok {
		if err := s.Symlink("/hello.txt", "/link.txt"); err != nil {
			t.Fatalf("Symlink: %v", err)
		}
	}
	fs.Close()

	// fsck clean.
	dev := attachRaw(t, p, true)
	fsckClean(t, dev)
	detach(dev)

	// macOS mounts RW and reads back.
	out, err := exec.Command("hdiutil", "attach", "-imagekey", "diskimage-class=CRawDiskImage", p).CombinedOutput()
	if err != nil {
		t.Fatalf("hdiutil mount: %v: %s", err, out)
	}
	dev = strings.Fields(strings.SplitN(string(out), "\n", 2)[0])[0]
	defer detach(dev)
	mnt := "/Volumes/GORW"
	// Wait briefly for the mount to settle.
	for i := 0; i < 20; i++ {
		if _, err := os.Stat(mnt); err == nil {
			break
		}
		time.Sleep(100 * time.Millisecond)
	}
	for p2, want := range files {
		got, err := os.ReadFile(filepath.Join(mnt, strings.TrimPrefix(p2, "/")))
		if err != nil {
			t.Errorf("macOS read %s: %v", p2, err)
			continue
		}
		if !bytes.Equal(got, want) {
			t.Errorf("macOS read %s: %d bytes, want %d", p2, len(got), len(want))
		}
	}
	if tgt, err := os.Readlink(filepath.Join(mnt, "link.txt")); err != nil || tgt != "/hello.txt" {
		t.Errorf("macOS readlink link.txt = %q, %v", tgt, err)
	}
}

// TestDarwinWriteIntoAppleImage: hdiutil-created HFS+ → Go writes into it →
// fsck clean + macOS reads the Go-added files alongside the macOS-created one.
func TestDarwinWriteIntoAppleImage(t *testing.T) {
	requireTools(t)
	dir := t.TempDir()
	dmg := filepath.Join(dir, "apple.dmg")
	if out, err := exec.Command("hdiutil", "create", "-size", "16m", "-fs", "HFS+",
		"-volname", "APPLEMADE", "-layout", "NONE", "-ov", dmg).CombinedOutput(); err != nil {
		t.Skipf("hdiutil create unavailable in sandbox: %v: %s", err, out)
	}
	img := filepath.Join(dir, "apple.img")
	if err := os.Rename(dmg, img); err != nil {
		// hdiutil may have already produced apple.img if extension stripped.
		if _, e2 := os.Stat(img); e2 != nil {
			t.Fatalf("locate created image: %v", err)
		}
	}
	// macOS writes one file.
	out, err := exec.Command("hdiutil", "attach", "-imagekey", "diskimage-class=CRawDiskImage", img).CombinedOutput()
	if err != nil {
		t.Fatalf("attach apple: %v: %s", err, out)
	}
	dev := strings.Fields(strings.SplitN(string(out), "\n", 2)[0])[0]
	mnt := "/Volumes/APPLEMADE"
	time.Sleep(300 * time.Millisecond)
	if err := os.WriteFile(filepath.Join(mnt, "orig.txt"), []byte("orig from apple\n"), 0o644); err != nil {
		detach(dev)
		t.Fatalf("macOS write orig: %v", err)
	}
	detach(dev)

	// Go writes into the same image.
	v, err := OpenFileWritable(img)
	if err != nil {
		t.Fatalf("OpenFileWritable apple: %v", err)
	}
	if err := v.WriteFile("/go_added.txt", []byte("written by pure-go\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := v.MkDir("/godir", 0o755); err != nil {
		t.Fatal(err)
	}
	if err := v.WriteFile("/godir/inner.txt", []byte("inner go\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	v.Close()

	// fsck clean.
	dev = attachRaw(t, img, true)
	fsckClean(t, dev)
	detach(dev)

	// macOS reads the Go additions and the original.
	out, err = exec.Command("hdiutil", "attach", "-imagekey", "diskimage-class=CRawDiskImage", img).CombinedOutput()
	if err != nil {
		t.Fatalf("remount apple: %v: %s", err, out)
	}
	dev = strings.Fields(strings.SplitN(string(out), "\n", 2)[0])[0]
	defer detach(dev)
	time.Sleep(300 * time.Millisecond)
	for name, want := range map[string]string{
		"orig.txt":        "orig from apple\n",
		"go_added.txt":    "written by pure-go\n",
		"godir/inner.txt": "inner go\n",
	} {
		got, err := os.ReadFile(filepath.Join(mnt, name))
		if err != nil {
			t.Errorf("macOS read %s: %v", name, err)
			continue
		}
		if string(got) != want {
			t.Errorf("macOS read %s = %q, want %q", name, got, want)
		}
	}
}
