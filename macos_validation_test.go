// Copyright (c) 2026, go-filesystems
// SPDX-License-Identifier: BSD-3-Clause

//go:build darwin

package hfsplus

import (
	"bytes"
	"fmt"
	"math/rand"
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

// formatFileVolume formats a real on-disk image and returns the concrete
// *Volume backing the Filesystem so the darwin stress tests can drive the same
// helpers the cross-arch pure-Go stress tests use.
func formatFileVolume(t *testing.T, path string, size int64, cfg FormatConfig) *Volume {
	t.Helper()
	fs, err := Format(path, size, cfg)
	if err != nil {
		t.Fatalf("Format %s: %v", path, err)
	}
	v, ok := fs.(*Volume)
	if !ok {
		t.Fatalf("Format returned %T, want *Volume", fs)
	}
	return v
}

// waitMount blocks (briefly) until the mount point appears.
func waitMount(mnt string) {
	for i := 0; i < 30; i++ {
		if _, err := os.Stat(mnt); err == nil {
			return
		}
		time.Sleep(100 * time.Millisecond)
	}
}

// TestDarwinFragmentedForkFsck proves the extents-overflow *insert* path on
// macOS: it builds a worst-case fragmented free space, writes a file that must
// spill into >8 extents, confirms the fork really used the overflow tree, then
// fsck_hfs -n reports the image clean and macOS reads the exact bytes back.
func TestDarwinFragmentedForkFsck(t *testing.T) {
	requireTools(t)
	p := filepath.Join(t.TempDir(), "frag.img")
	v := formatFileVolume(t, p, 32<<20, FormatConfig{Label: "FRAG"})

	holes := fragmentFreeSpace(t, v, int(v.vh.TotalBlocks)+10)
	if holes < 128 {
		t.Fatalf("fragmentation produced only %d holes", holes)
	}
	bs := int(v.vh.BlockSize)
	nblk := numInlineExtents + 24
	if holes < nblk*3 {
		t.Fatalf("not enough holes (%d) for a >8-extent file plus metadata", holes)
	}
	want := bytes.Repeat([]byte{0xDE, 0xAD, 0xBE, 0xEF}, nblk*bs/4)
	if err := v.WriteFile("/frag_big.bin", want, 0o644); err != nil {
		t.Fatalf("fragmented write: %v", err)
	}
	r, err := v.lookupPath("/frag_big.bin")
	if err != nil {
		t.Fatal(err)
	}
	exts, err := v.resolveForkExtents(r.rec.file.fileID, forkTypeData, r.rec.file.dataFork)
	if err != nil {
		t.Fatal(err)
	}
	if len(exts) <= numInlineExtents {
		t.Fatalf("fork used %d extents, expected >%d (overflow path not exercised)", len(exts), numInlineExtents)
	}
	t.Logf("fragmented fork uses %d extents", len(exts))
	v.Close()

	dev := attachRaw(t, p, true)
	fsckClean(t, dev)
	detach(dev)

	out, err := exec.Command("hdiutil", "attach", "-imagekey", "diskimage-class=CRawDiskImage", p).CombinedOutput()
	if err != nil {
		t.Fatalf("hdiutil mount: %v: %s", err, out)
	}
	dev = strings.Fields(strings.SplitN(string(out), "\n", 2)[0])[0]
	defer detach(dev)
	mnt := "/Volumes/FRAG"
	waitMount(mnt)
	got, err := os.ReadFile(filepath.Join(mnt, "frag_big.bin"))
	if err != nil {
		t.Fatalf("macOS read frag_big.bin: %v", err)
	}
	if !bytes.Equal(got, want) {
		t.Fatalf("macOS read frag_big.bin: %d bytes, want %d", len(got), len(want))
	}
}

// TestDarwinCatalogGrowthFsck writes enough entries to force the catalog B-tree
// fork to grow past its formatter reservation several times (across multiple
// node allocations + fork extensions), then fsck_hfs -n must be clean and macOS
// must list/read the grown catalog. The thousands of inserts are done against an
// in-memory image and the result is flushed to disk ONCE (a per-write full-image
// Sync would make the disk-backed path pathologically slow); the on-disk bytes
// fsck and mount are identical either way.
func TestDarwinCatalogGrowthFsck(t *testing.T) {
	requireTools(t)
	if testing.Short() {
		t.Skip("skipping catalog-growth darwin stress in -short")
	}
	const size = 48 << 20
	img, err := Mkfs(size, FormatConfig{Label: "GROW"})
	if err != nil {
		t.Fatalf("Mkfs: %v", err)
	}
	v, err := OpenWritable(img, nil)
	if err != nil {
		t.Fatalf("OpenWritable: %v", err)
	}
	startBlocks := v.vh.CatalogFile.TotalBlocks
	const n = 5000 // well past the ~1900-entry first-growth threshold: multiple grows
	for i := 0; i < n; i++ {
		fp := fmt.Sprintf("/f%06d.txt", i)
		if err := v.WriteFile(fp, []byte(fmt.Sprintf("v%d", i)), 0o644); err != nil {
			t.Fatalf("write entry %d: %v", i, err)
		}
	}
	if v.vh.CatalogFile.TotalBlocks <= startBlocks {
		t.Fatalf("catalog fork did not grow: still %d blocks", v.vh.CatalogFile.TotalBlocks)
	}
	t.Logf("catalog fork grew from %d to %d blocks across %d entries", startBlocks, v.vh.CatalogFile.TotalBlocks, n)
	p := filepath.Join(t.TempDir(), "grow.img")
	if err := os.WriteFile(p, v.Bytes(), 0o644); err != nil {
		t.Fatalf("flush image: %v", err)
	}
	v.Close()

	dev := attachRaw(t, p, true)
	fsckClean(t, dev)
	detach(dev)

	out, err := exec.Command("hdiutil", "attach", "-imagekey", "diskimage-class=CRawDiskImage", p).CombinedOutput()
	if err != nil {
		t.Fatalf("hdiutil mount: %v: %s", err, out)
	}
	dev = strings.Fields(strings.SplitN(string(out), "\n", 2)[0])[0]
	defer detach(dev)
	mnt := "/Volumes/GROW"
	waitMount(mnt)
	ents, err := os.ReadDir(mnt)
	if err != nil {
		t.Fatalf("macOS readdir: %v", err)
	}
	if len(ents) != n {
		t.Fatalf("macOS sees %d entries, want %d", len(ents), n)
	}
	for _, i := range []int{0, 1, n / 2, n - 2, n - 1} {
		fp := filepath.Join(mnt, fmt.Sprintf("f%06d.txt", i))
		got, err := os.ReadFile(fp)
		if err != nil {
			t.Errorf("macOS read %s: %v", fp, err)
			continue
		}
		if string(got) != fmt.Sprintf("v%d", i) {
			t.Errorf("macOS read %s = %q", fp, got)
		}
	}
}

// TestDarwinChurnFsck drives heavy create/delete churn (forcing node-underflow
// rotate/merge and tree-height shrink), then fsck_hfs -n must report the image
// clean — no "out of order", "invalid node", or "overlapped" complaints — and
// macOS must see exactly the survivors.
func TestDarwinChurnFsck(t *testing.T) {
	requireTools(t)
	// Build in-memory (thousands of mkdir/delete ops) and flush to disk once;
	// a per-op full-image Sync on the file-backed path is pathologically slow.
	const size = 128 << 20
	img, err := Mkfs(size, FormatConfig{Label: "CHURN"})
	if err != nil {
		t.Fatalf("Mkfs: %v", err)
	}
	v, err := OpenWritable(img, nil)
	if err != nil {
		t.Fatalf("OpenWritable: %v", err)
	}

	const n = 6000
	for i := 0; i < n; i++ {
		if err := v.MkDir(fmt.Sprintf("/c%06d", i), 0o755); err != nil {
			t.Fatalf("mkdir %d: %v", i, err)
		}
	}
	order := rand.New(rand.NewSource(7)).Perm(n)
	for _, i := range order {
		if i%10 == 0 {
			continue
		}
		if err := v.DeleteDir(fmt.Sprintf("/c%06d", i)); err != nil {
			t.Fatalf("delete %d: %v", i, err)
		}
	}
	p := filepath.Join(t.TempDir(), "churn.img")
	if err := os.WriteFile(p, v.Bytes(), 0o644); err != nil {
		t.Fatalf("flush image: %v", err)
	}
	v.Close()

	dev := attachRaw(t, p, true)
	fsckClean(t, dev)
	detach(dev)

	out, err := exec.Command("hdiutil", "attach", "-imagekey", "diskimage-class=CRawDiskImage", p).CombinedOutput()
	if err != nil {
		t.Fatalf("hdiutil mount: %v: %s", err, out)
	}
	dev = strings.Fields(strings.SplitN(string(out), "\n", 2)[0])[0]
	defer detach(dev)
	mnt := "/Volumes/CHURN"
	waitMount(mnt)
	ents, err := os.ReadDir(mnt)
	if err != nil {
		t.Fatalf("macOS readdir: %v", err)
	}
	want := 0
	for i := 0; i < n; i++ {
		if i%10 == 0 {
			want++
		}
	}
	if len(ents) != want {
		t.Fatalf("macOS sees %d survivors, want %d", len(ents), want)
	}
}
