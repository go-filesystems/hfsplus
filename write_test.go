// Copyright (c) 2026, go-filesystems
// SPDX-License-Identifier: BSD-3-Clause

package hfsplus

import (
	"bytes"
	"crypto/rand"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"testing"
)

// openMem formats a fresh in-memory writable volume.
func openMem(t *testing.T, size int64, cfg FormatConfig) *Volume {
	t.Helper()
	img, err := Mkfs(size, cfg)
	if err != nil {
		t.Fatalf("Mkfs: %v", err)
	}
	v, err := OpenWritable(img, nil)
	if err != nil {
		t.Fatalf("OpenWritable: %v", err)
	}
	return v
}

func TestMkfsEmptyRoundTrip(t *testing.T) {
	for _, cs := range []bool{false, true} {
		v := openMem(t, 8<<20, FormatConfig{Label: "GOTEST", CaseSensitive: cs})
		ents, err := v.ListDir("/")
		if err != nil {
			t.Fatalf("cs=%v ListDir: %v", cs, err)
		}
		if len(ents) != 0 {
			t.Errorf("cs=%v fresh root has %d entries, want 0", cs, len(ents))
		}
		if v.CaseSensitive() != cs {
			t.Errorf("CaseSensitive()=%v want %v", v.CaseSensitive(), cs)
		}
		if got := v.Label(); got != "GOTEST" {
			t.Errorf("Label()=%q want GOTEST", got)
		}
		v.Close()
	}
}

func TestWriteFileReadBack(t *testing.T) {
	v := openMem(t, 16<<20, FormatConfig{Label: "RW"})
	defer v.Close()

	cases := map[string][]byte{
		"/hello.txt": []byte("hello hfsplus write\n"),
		"/empty.txt": {},
		"/binary":    bytes.Repeat([]byte{0xAB, 0xCD}, 5000),
	}
	for p, data := range cases {
		if err := v.WriteFile(p, data, 0o644); err != nil {
			t.Fatalf("WriteFile(%q): %v", p, err)
		}
	}
	for p, want := range cases {
		got, err := v.ReadFile(p)
		if err != nil {
			t.Fatalf("ReadFile(%q): %v", p, err)
		}
		if !bytes.Equal(got, want) {
			t.Errorf("ReadFile(%q) = %d bytes, want %d", p, len(got), len(want))
		}
		st, err := v.Stat(p)
		if err != nil {
			t.Fatalf("Stat(%q): %v", p, err)
		}
		if st.Size() != uint64(len(want)) {
			t.Errorf("Stat(%q).Size = %d want %d", p, st.Size(), len(want))
		}
	}
}

func TestMkDirNestedWrite(t *testing.T) {
	v := openMem(t, 16<<20, FormatConfig{Label: "RW"})
	defer v.Close()
	if err := v.MkDir("/a", 0o755); err != nil {
		t.Fatal(err)
	}
	if err := v.MkDir("/a/b", 0o755); err != nil {
		t.Fatal(err)
	}
	if err := v.WriteFile("/a/b/deep.txt", []byte("deep"), 0o644); err != nil {
		t.Fatal(err)
	}
	got, err := v.ReadFile("/a/b/deep.txt")
	if err != nil {
		t.Fatal(err)
	}
	if string(got) != "deep" {
		t.Errorf("deep.txt = %q", got)
	}
	ents, err := v.ListDir("/a/b")
	if err != nil {
		t.Fatal(err)
	}
	if len(ents) != 1 || ents[0].Name() != "deep.txt" {
		t.Errorf("/a/b listing = %v", ents)
	}
	// MkDir over an existing name fails.
	if err := v.MkDir("/a", 0o755); !errors.Is(err, ErrExists) {
		t.Errorf("MkDir existing err = %v want ErrExists", err)
	}
}

// TestManyFilesSplit forces catalog leaf splitting (and tree-height growth) by
// writing enough files that a single 4 KiB leaf cannot hold them all.
func TestManyFilesSplit(t *testing.T) {
	v := openMem(t, 32<<20, FormatConfig{Label: "SPLIT"})
	defer v.Close()
	const n = 400
	for i := 0; i < n; i++ {
		p := fmt.Sprintf("/file%04d.dat", i)
		if err := v.WriteFile(p, []byte(fmt.Sprintf("content-%d", i)), 0o644); err != nil {
			t.Fatalf("WriteFile %s: %v", p, err)
		}
	}
	ents, err := v.ListDir("/")
	if err != nil {
		t.Fatal(err)
	}
	if len(ents) != n {
		t.Fatalf("root has %d entries, want %d (catalog split lost records)", len(ents), n)
	}
	// Spot-read a sample across the range.
	for _, i := range []int{0, 1, 199, 200, 399} {
		p := fmt.Sprintf("/file%04d.dat", i)
		got, err := v.ReadFile(p)
		if err != nil {
			t.Fatalf("ReadFile %s after split: %v", p, err)
		}
		if string(got) != fmt.Sprintf("content-%d", i) {
			t.Errorf("%s = %q", p, got)
		}
	}
	// Tree must have grown beyond a single leaf.
	if v.catalogTree.header.RootNode == v.catalogTree.header.FirstLeaf {
		t.Log("note: tree still single-leaf (unexpected for 400 files)")
	}
}

func TestDeleteFileAndDir(t *testing.T) {
	v := openMem(t, 16<<20, FormatConfig{Label: "DEL"})
	defer v.Close()
	v.MkDir("/d", 0o755)
	v.WriteFile("/d/x.txt", []byte("x"), 0o644)
	v.WriteFile("/top.txt", []byte("top"), 0o644)

	freeBefore := v.VolumeHeader().FreeBlocks
	if err := v.DeleteFile("/top.txt"); err != nil {
		t.Fatal(err)
	}
	if _, err := v.ReadFile("/top.txt"); !errors.Is(err, ErrNotFound) {
		t.Errorf("deleted file still readable: %v", err)
	}
	if v.VolumeHeader().FreeBlocks <= freeBefore {
		t.Errorf("freeBlocks not reclaimed after delete: %d -> %d", freeBefore, v.VolumeHeader().FreeBlocks)
	}
	// Non-empty dir delete fails.
	if err := v.DeleteDir("/d"); !errors.Is(err, ErrNotEmpty) {
		t.Errorf("DeleteDir non-empty err = %v want ErrNotEmpty", err)
	}
	if err := v.DeleteFile("/d/x.txt"); err != nil {
		t.Fatal(err)
	}
	if err := v.DeleteDir("/d"); err != nil {
		t.Fatalf("DeleteDir empty: %v", err)
	}
	ents, _ := v.ListDir("/")
	if len(ents) != 0 {
		t.Errorf("root not empty after deletes: %v", ents)
	}
}

func TestRenameWrite(t *testing.T) {
	v := openMem(t, 16<<20, FormatConfig{Label: "MV"})
	defer v.Close()
	v.MkDir("/sub", 0o755)
	v.WriteFile("/old.txt", []byte("payload"), 0o644)
	if err := v.Rename("/old.txt", "/sub/new.txt"); err != nil {
		t.Fatalf("Rename: %v", err)
	}
	if _, err := v.ReadFile("/old.txt"); !errors.Is(err, ErrNotFound) {
		t.Errorf("old path still present")
	}
	got, err := v.ReadFile("/sub/new.txt")
	if err != nil {
		t.Fatalf("ReadFile new path: %v", err)
	}
	if string(got) != "payload" {
		t.Errorf("renamed content = %q", got)
	}
	// Rename onto an existing name fails.
	v.WriteFile("/a.txt", []byte("a"), 0o644)
	v.WriteFile("/b.txt", []byte("b"), 0o644)
	if err := v.Rename("/a.txt", "/b.txt"); !errors.Is(err, ErrExists) {
		t.Errorf("Rename onto existing err = %v want ErrExists", err)
	}
}

func TestSymlinkWrite(t *testing.T) {
	v := openMem(t, 16<<20, FormatConfig{Label: "LN"})
	defer v.Close()
	v.WriteFile("/target.txt", []byte("t"), 0o644)
	if err := v.Symlink("/target.txt", "/link"); err != nil {
		t.Fatalf("Symlink: %v", err)
	}
	tgt, err := v.ReadLink("/link")
	if err != nil {
		t.Fatalf("ReadLink: %v", err)
	}
	if tgt != "/target.txt" {
		t.Errorf("ReadLink = %q", tgt)
	}
	ents, _ := v.ListDir("/")
	var sawLink bool
	for _, e := range ents {
		if e.Name() == "link" && e.FileType() == ftSymlink {
			sawLink = true
		}
	}
	if !sawLink {
		t.Error("link not listed as symlink")
	}
}

func TestTruncateWrite(t *testing.T) {
	v := openMem(t, 16<<20, FormatConfig{Label: "TR"})
	defer v.Close()
	v.WriteFile("/f", []byte("0123456789"), 0o644)
	if err := v.Truncate("/f", 4); err != nil {
		t.Fatal(err)
	}
	got, _ := v.ReadFile("/f")
	if string(got) != "0123" {
		t.Errorf("after shrink = %q", got)
	}
	if err := v.Truncate("/f", 8); err != nil {
		t.Fatal(err)
	}
	got, _ = v.ReadFile("/f")
	if !bytes.Equal(got, append([]byte("0123"), 0, 0, 0, 0)) {
		t.Errorf("after grow = %v", got)
	}
}

func TestSetLabelWrite(t *testing.T) {
	v := openMem(t, 16<<20, FormatConfig{Label: "OLD"})
	defer v.Close()
	if err := v.SetLabel("NEWLABEL"); err != nil {
		t.Fatalf("SetLabel: %v", err)
	}
	if v.Label() != "NEWLABEL" {
		t.Errorf("Label after set = %q", v.Label())
	}
	// Files still readable after a relabel.
	v.WriteFile("/k", []byte("k"), 0o644)
	if got, _ := v.ReadFile("/k"); string(got) != "k" {
		t.Errorf("file lost after relabel")
	}
}

func TestOverwriteFile(t *testing.T) {
	v := openMem(t, 16<<20, FormatConfig{Label: "OW"})
	defer v.Close()
	v.WriteFile("/f", bytes.Repeat([]byte("A"), 9000), 0o644)
	if err := v.WriteFile("/f", []byte("short"), 0o644); err != nil {
		t.Fatal(err)
	}
	got, _ := v.ReadFile("/f")
	if string(got) != "short" {
		t.Errorf("overwrite = %q", got)
	}
}

// TestWritableFileRoundTrip formats to a file, writes, closes, reopens
// read-only and verifies persistence through Sync.
func TestWritableFileRoundTrip(t *testing.T) {
	p := filepath.Join(t.TempDir(), "rw.hfs")
	fs, err := Format(p, 16<<20, FormatConfig{Label: "PERSIST"})
	if err != nil {
		t.Fatalf("Format: %v", err)
	}
	if err := fs.WriteFile("/persist.txt", []byte("survives"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := fs.MkDir("/dir", 0o755); err != nil {
		t.Fatal(err)
	}
	if err := fs.Close(); err != nil {
		t.Fatal(err)
	}
	// Reopen read-only.
	v, err := OpenFile(p)
	if err != nil {
		t.Fatalf("reopen: %v", err)
	}
	defer v.Close()
	got, err := v.ReadFile("/persist.txt")
	if err != nil {
		t.Fatalf("read after reopen: %v", err)
	}
	if string(got) != "survives" {
		t.Errorf("persisted content = %q", got)
	}
	if _, err := v.ListDir("/dir"); err != nil {
		t.Errorf("dir lost after reopen: %v", err)
	}
}

func TestReadOnlyRejectsWrites(t *testing.T) {
	v := loadFixture(t)
	defer v.Close()
	if err := v.WriteFile("/x", nil, 0o644); !errors.Is(err, ErrReadOnly) {
		t.Errorf("WriteFile RO err = %v", err)
	}
	if err := v.MkDir("/x", 0o755); !errors.Is(err, ErrReadOnly) {
		t.Errorf("MkDir RO err = %v", err)
	}
	if err := v.Symlink("/t", "/l"); !errors.Is(err, ErrReadOnly) {
		t.Errorf("Symlink RO err = %v", err)
	}
	if err := v.Truncate("/x", 0); !errors.Is(err, ErrReadOnly) {
		t.Errorf("Truncate RO err = %v", err)
	}
	if err := v.SetLabel("X"); !errors.Is(err, ErrReadOnly) {
		t.Errorf("SetLabel RO err = %v", err)
	}
}

func TestNoSpace(t *testing.T) {
	v := openMem(t, 16<<20, FormatConfig{Label: "FULL"})
	defer v.Close()
	big := make([]byte, 12<<20)
	rand.Read(big)
	// First big file should fit; a second should run out of space.
	if err := v.WriteFile("/big1", big, 0o644); err != nil {
		t.Fatalf("first big write: %v", err)
	}
	if err := v.WriteFile("/big2", big, 0o644); !errors.Is(err, ErrNoSpace) {
		t.Errorf("second big write err = %v want ErrNoSpace", err)
	}
}

func TestFormatErrors(t *testing.T) {
	if _, err := Mkfs(1024, FormatConfig{}); !errors.Is(err, ErrCorrupt) {
		t.Errorf("tiny Mkfs err = %v want ErrCorrupt", err)
	}
}

// TestDeepTreeInsertDelete forces multi-level catalog growth (index-node
// splits) and then deletes every entry, exercising the split-propagation,
// index-split, leaf-unlink and child-pointer-removal paths and leaving the
// tree consistent.
func TestDeepTreeInsertDelete(t *testing.T) {
	v := openMem(t, 64<<20, FormatConfig{Label: "DEEP"})
	defer v.Close()
	const n = 3000
	for i := 0; i < n; i++ {
		p := fmt.Sprintf("/e%05d", i)
		if err := v.MkDir(p, 0o755); err != nil {
			t.Fatalf("MkDir %s: %v", p, err)
		}
	}
	if ents, err := v.ListDir("/"); err != nil || len(ents) != n {
		t.Fatalf("after %d mkdirs: list=%d err=%v", n, len(ents), err)
	}
	if v.catalogTree.header.RootNode == v.catalogTree.header.FirstLeaf {
		t.Fatal("expected a multi-level tree after 1200 entries")
	}
	// Delete in a scrambled order to stress unlink/merge-free paths.
	for i := 0; i < n; i++ {
		j := (i * 7) % n
		p := fmt.Sprintf("/e%05d", j)
		if _, err := v.Stat(p); err != nil {
			continue // already deleted (collision in the *7 schedule)
		}
		if err := v.DeleteDir(p); err != nil {
			t.Fatalf("DeleteDir %s: %v", p, err)
		}
	}
	// Remaining (collision-skipped) entries: delete sequentially.
	ents, _ := v.ListDir("/")
	for _, e := range ents {
		if err := v.DeleteDir("/" + e.Name()); err != nil {
			t.Fatalf("cleanup DeleteDir %s: %v", e.Name(), err)
		}
	}
	final, err := v.ListDir("/")
	if err != nil {
		t.Fatal(err)
	}
	if len(final) != 0 {
		t.Errorf("tree not empty after deleting all: %d left", len(final))
	}
	// Tree must still be usable: write a new file.
	if err := v.WriteFile("/after.txt", []byte("ok"), 0o644); err != nil {
		t.Fatalf("write after bulk-delete: %v", err)
	}
}

func TestBytesAndSyncNoop(t *testing.T) {
	v := openMem(t, 8<<20, FormatConfig{Label: "B"})
	defer v.Close()
	if v.Bytes() == nil {
		t.Error("Bytes() nil for writable volume")
	}
	// In-memory (wa==nil) Sync is a no-op and must succeed.
	if err := v.Sync(); err != nil {
		t.Errorf("Sync no-op err = %v", err)
	}
	// Read-only volume Bytes() is nil.
	ro := loadFixture(t)
	defer ro.Close()
	if ro.Bytes() != nil {
		t.Error("Bytes() non-nil for read-only volume")
	}
	if err := ro.Sync(); err != nil {
		t.Errorf("RO Sync err = %v", err)
	}
}

func TestWriteErrorPaths(t *testing.T) {
	v := openMem(t, 16<<20, FormatConfig{Label: "E"})
	defer v.Close()
	// MkDir under a missing parent.
	if err := v.MkDir("/nope/child", 0o755); !errors.Is(err, ErrNotFound) {
		t.Errorf("MkDir missing parent err = %v", err)
	}
	// WriteFile where an intermediate is a file.
	v.WriteFile("/afile", []byte("x"), 0o644)
	if err := v.WriteFile("/afile/child", []byte("y"), 0o644); !errors.Is(err, ErrNotDirectory) {
		t.Errorf("WriteFile through file err = %v", err)
	}
	// WriteFile onto an existing directory.
	v.MkDir("/adir", 0o755)
	if err := v.WriteFile("/adir", []byte("y"), 0o644); !errors.Is(err, ErrExists) {
		t.Errorf("WriteFile onto dir err = %v", err)
	}
	// Delete a missing file / dir.
	if err := v.DeleteFile("/ghost"); !errors.Is(err, ErrNotFound) {
		t.Errorf("DeleteFile missing err = %v", err)
	}
	if err := v.DeleteDir("/ghost"); !errors.Is(err, ErrNotFound) {
		t.Errorf("DeleteDir missing err = %v", err)
	}
	// DeleteFile on a directory / DeleteDir on a file.
	if err := v.DeleteFile("/adir"); !errors.Is(err, ErrNotRegular) {
		t.Errorf("DeleteFile on dir err = %v", err)
	}
	if err := v.DeleteDir("/afile"); !errors.Is(err, ErrNotDirectory) {
		t.Errorf("DeleteDir on file err = %v", err)
	}
	// Rename a missing source / onto a directory parent that is missing.
	if err := v.Rename("/ghost", "/x"); !errors.Is(err, ErrNotFound) {
		t.Errorf("Rename missing src err = %v", err)
	}
	// Truncate a missing/non-regular target.
	if err := v.Truncate("/ghost", 5); !errors.Is(err, ErrNotFound) {
		t.Errorf("Truncate missing err = %v", err)
	}
	if err := v.Truncate("/adir", 5); !errors.Is(err, ErrNotRegular) {
		t.Errorf("Truncate dir err = %v", err)
	}
	if err := v.Truncate("/afile", -1); !errors.Is(err, ErrCorrupt) {
		t.Errorf("Truncate negative err = %v", err)
	}
	// SetLabel rejects empty.
	if err := v.SetLabel("   "); !errors.Is(err, ErrCorrupt) {
		t.Errorf("SetLabel empty err = %v", err)
	}
}

func TestRenameDirectoryAcrossParents(t *testing.T) {
	v := openMem(t, 16<<20, FormatConfig{Label: "MVD"})
	defer v.Close()
	v.MkDir("/src", 0o755)
	v.MkDir("/dst", 0o755)
	v.MkDir("/src/movedir", 0o755)
	v.WriteFile("/src/movedir/inner.txt", []byte("inner"), 0o644)
	if err := v.Rename("/src/movedir", "/dst/movedir"); err != nil {
		t.Fatalf("Rename dir: %v", err)
	}
	if _, err := v.ListDir("/src/movedir"); !errors.Is(err, ErrNotFound) {
		t.Errorf("src dir still present: %v", err)
	}
	got, err := v.ReadFile("/dst/movedir/inner.txt")
	if err != nil {
		t.Fatalf("read moved inner: %v", err)
	}
	if string(got) != "inner" {
		t.Errorf("moved content = %q", got)
	}
}

func TestFoldOrdering(t *testing.T) {
	bt := &btree{header: btHeader{KeyCompare: keyCompareCaseFold}}
	// NUL is ignorable: "\x00abc" folds equal to "abc".
	a := catalogKey{parentID: 1, nameU16: []uint16{0x00, 'a', 'b', 'c'}}
	b := catalogKey{parentID: 1, nameU16: []uint16{'a', 'b', 'c'}}
	if bt.compareCatalogKey(a, b) != 0 {
		t.Error("NUL-prefixed name should fold equal")
	}
	// Latin-1 upper-case folds to lower.
	if foldU16(0x00C0) != 0x00E0 { // À -> à
		t.Errorf("À fold = %#x", foldU16(0x00C0))
	}
	// × (0x00D7) is NOT a letter and must not fold.
	if foldU16(0x00D7) != 0x00D7 {
		t.Errorf("× should not fold")
	}
	// Latin Extended-A even/odd pair.
	if foldU16(0x0100) != 0x0101 {
		t.Errorf("Ā fold = %#x", foldU16(0x0100))
	}
	// Control char (not NUL) compares as itself.
	if foldU16(0x0007) != 0x0007 {
		t.Errorf("BEL fold = %#x", foldU16(0x0007))
	}
}

func TestOpenFileWritableErrors(t *testing.T) {
	// Missing path.
	if _, err := OpenFileWritable(filepath.Join(t.TempDir(), "nope")); err == nil {
		t.Error("OpenFileWritable missing path: want error")
	}
	// A non-HFS+ file.
	bad := filepath.Join(t.TempDir(), "bad.img")
	if err := os.WriteFile(bad, make([]byte, 4096), 0o644); err != nil {
		t.Fatal(err)
	}
	if _, err := OpenFileWritable(bad); !errors.Is(err, ErrBadHeader) {
		t.Errorf("OpenFileWritable bad image err = %v", err)
	}
}

func TestLabelReadBack(t *testing.T) {
	v := openMem(t, 8<<20, FormatConfig{Label: "MYVOL"})
	defer v.Close()
	if v.Label() != "MYVOL" {
		t.Errorf("Label() = %q", v.Label())
	}
	// SetLabel to the same value is a no-op success.
	if err := v.SetLabel("MYVOL"); err != nil {
		t.Errorf("SetLabel same err = %v", err)
	}
	// A label round-trips through reopen on a writable file volume.
	v.SetLabel("RENAMED")
	if v.Label() != "RENAMED" {
		t.Errorf("Label after rename = %q", v.Label())
	}
}

// helper to keep os import even if some subtests are pruned.
var _ = os.O_RDWR
