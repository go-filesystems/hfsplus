// Copyright (c) 2026, go-filesystems
// SPDX-License-Identifier: BSD-3-Clause

package hfsplus

import (
	"bytes"
	"compress/gzip"
	"crypto/md5"
	"encoding/hex"
	"errors"
	"io"
	"os"
	"path/filepath"
	"sort"
	"testing"

	filesystem "github.com/go-filesystems/interface"
)

// loadFixture decompresses the committed gzipped raw HFS+ image into memory and
// opens it. This runs on every architecture (no macOS tools needed), so it is
// the cross-arch / big-endian (s390x) on-disk decode test.
func loadFixture(t *testing.T) *Volume {
	t.Helper()
	f, err := os.Open(filepath.Join("testdata", "hfsplus.dmg.gz"))
	if err != nil {
		t.Fatalf("open fixture: %v", err)
	}
	defer f.Close()
	gz, err := gzip.NewReader(f)
	if err != nil {
		t.Fatalf("gunzip fixture: %v", err)
	}
	raw, err := io.ReadAll(gz)
	if err != nil {
		t.Fatalf("read fixture: %v", err)
	}
	v, err := Open(bytes.NewReader(raw), int64(len(raw)))
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	return v
}

func md5hex(b []byte) string {
	s := md5.Sum(b)
	return hex.EncodeToString(s[:])
}

// fixtureFiles are the known files in the committed image with the MD5
// checksums computed by macOS at creation time.
var fixtureFiles = map[string]struct {
	size int
	md5  string
}{
	"/hello.txt":              {14, "79e6ac83c1117d9eba802a88ae08720a"},
	"/fox.txt":                {45, "0d7006cd055e94cf614587e1d2ae0c8e"},
	"/subdir/a.txt":           {14, "87addbacda001524a1be6b76110a034f"},
	"/subdir/nested/deep.txt": {25, "a5a762efb4fd1637f1e77e5a5722b419"},
	"/big.bin":                {262144, "5b1baa02cd1e35fbc9c2a80dd759ee7c"},
	"/empty.txt":              {0, "d41d8cd98f00b204e9800998ecf8427e"},
}

func TestVolumeHeader(t *testing.T) {
	v := loadFixture(t)
	defer v.Close()
	vh := v.VolumeHeader()
	if vh.Signature != sigHFSPlus {
		t.Errorf("signature = %#x, want %#x", vh.Signature, sigHFSPlus)
	}
	if vh.BlockSize != 4096 {
		t.Errorf("block size = %d, want 4096", vh.BlockSize)
	}
	if vh.TotalBlocks == 0 {
		t.Error("total blocks = 0")
	}
	if v.CaseSensitive() {
		t.Error("fixture is plain HFS+ (case-insensitive), got case-sensitive")
	}
}

func TestReadFileExactBytes(t *testing.T) {
	v := loadFixture(t)
	defer v.Close()
	for path, want := range fixtureFiles {
		data, err := v.ReadFile(path)
		if err != nil {
			t.Errorf("ReadFile(%q): %v", path, err)
			continue
		}
		if len(data) != want.size {
			t.Errorf("ReadFile(%q) size = %d, want %d", path, len(data), want.size)
		}
		if got := md5hex(data); got != want.md5 {
			t.Errorf("ReadFile(%q) md5 = %s, want %s", path, got, want.md5)
		}
	}
}

func TestReadFileContent(t *testing.T) {
	v := loadFixture(t)
	defer v.Close()
	got, err := v.ReadFile("/hello.txt")
	if err != nil {
		t.Fatal(err)
	}
	if string(got) != "hello hfsplus\n" {
		t.Errorf("hello.txt = %q", got)
	}
}

func TestListDirRoot(t *testing.T) {
	v := loadFixture(t)
	defer v.Close()
	ents, err := v.ListDir("/")
	if err != nil {
		t.Fatal(err)
	}
	names := map[string]uint8{}
	for _, e := range ents {
		names[e.Name()] = e.FileType()
		if e.Inode() == 0 {
			t.Errorf("entry %q has zero inode", e.Name())
		}
	}
	for _, want := range []string{"hello.txt", "fox.txt", "empty.txt", "subdir", "big.bin", "link.txt"} {
		if _, ok := names[want]; !ok {
			t.Errorf("root listing missing %q (got %v)", want, names)
		}
	}
	if names["subdir"] != ftDir {
		t.Errorf("subdir type = %d, want %d", names["subdir"], ftDir)
	}
	if names["hello.txt"] != ftRegular {
		t.Errorf("hello.txt type = %d, want %d", names["hello.txt"], ftRegular)
	}
	if names["link.txt"] != ftSymlink {
		t.Errorf("link.txt type = %d, want %d", names["link.txt"], ftSymlink)
	}
}

func TestListDirNested(t *testing.T) {
	v := loadFixture(t)
	defer v.Close()
	ents, err := v.ListDir("/subdir")
	if err != nil {
		t.Fatal(err)
	}
	var got []string
	for _, e := range ents {
		got = append(got, e.Name())
	}
	sort.Strings(got)
	want := []string{"a.txt", "nested"}
	if len(got) != len(want) {
		t.Fatalf("subdir = %v, want %v", got, want)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Errorf("subdir[%d] = %q, want %q", i, got[i], want[i])
		}
	}
}

func TestStat(t *testing.T) {
	v := loadFixture(t)
	defer v.Close()
	st, err := v.Stat("/big.bin")
	if err != nil {
		t.Fatal(err)
	}
	if st.Size() != 262144 {
		t.Errorf("big.bin size = %d, want 262144", st.Size())
	}
	if st.Mode()&0xF000 != sIFREG {
		t.Errorf("big.bin mode = %#o, want regular file", st.Mode())
	}
	if st.Inode() == 0 {
		t.Error("big.bin inode = 0")
	}

	dst, err := v.Stat("/subdir")
	if err != nil {
		t.Fatal(err)
	}
	if dst.Mode()&0xF000 != sIFDIR {
		t.Errorf("subdir mode = %#o, want dir", dst.Mode())
	}

	rootSt, err := v.Stat("/")
	if err != nil {
		t.Fatal(err)
	}
	if rootSt.Inode() != cnidRootFolder {
		t.Errorf("root inode = %d, want %d", rootSt.Inode(), cnidRootFolder)
	}
}

func TestReadLink(t *testing.T) {
	v := loadFixture(t)
	defer v.Close()
	target, err := v.ReadLink("/link.txt")
	if err != nil {
		t.Fatal(err)
	}
	if target != "/subdir/a.txt" {
		t.Errorf("link.txt -> %q, want /subdir/a.txt", target)
	}
	if _, err := v.ReadLink("/hello.txt"); !errors.Is(err, ErrNotSymlink) {
		t.Errorf("ReadLink on regular file err = %v, want ErrNotSymlink", err)
	}
}

func TestLookupErrors(t *testing.T) {
	v := loadFixture(t)
	defer v.Close()
	if _, err := v.ReadFile("/nope.txt"); !errors.Is(err, ErrNotFound) {
		t.Errorf("missing file err = %v, want ErrNotFound", err)
	}
	if _, err := v.ListDir("/hello.txt"); !errors.Is(err, ErrNotDirectory) {
		t.Errorf("ListDir on file err = %v, want ErrNotDirectory", err)
	}
	if _, err := v.ReadFile("/subdir"); !errors.Is(err, ErrNotRegular) {
		t.Errorf("ReadFile on dir err = %v, want ErrNotRegular", err)
	}
	if _, err := v.ReadFile("/subdir/missing/x"); !errors.Is(err, ErrNotFound) {
		t.Errorf("missing intermediate err = %v", err)
	}
	if _, err := v.ReadFile("/hello.txt/x"); !errors.Is(err, ErrNotDirectory) {
		t.Errorf("file-as-dir-component err = %v, want ErrNotDirectory", err)
	}
}

func TestCaseInsensitiveLookup(t *testing.T) {
	v := loadFixture(t)
	defer v.Close()
	// Plain HFS+ is case-insensitive: HELLO.TXT must resolve to hello.txt.
	data, err := v.ReadFile("/HELLO.TXT")
	if err != nil {
		t.Fatalf("case-insensitive ReadFile: %v", err)
	}
	if string(data) != "hello hfsplus\n" {
		t.Errorf("HELLO.TXT = %q", data)
	}
}

func TestMutatorsReadOnly(t *testing.T) {
	v := loadFixture(t)
	defer v.Close()
	if err := v.WriteFile("/x", nil, 0o644); !errors.Is(err, ErrReadOnly) {
		t.Errorf("WriteFile err = %v", err)
	}
	if err := v.MkDir("/x", 0o755); !errors.Is(err, ErrReadOnly) {
		t.Errorf("MkDir err = %v", err)
	}
	if err := v.DeleteFile("/x"); !errors.Is(err, ErrReadOnly) {
		t.Errorf("DeleteFile err = %v", err)
	}
	if err := v.DeleteDir("/x"); !errors.Is(err, ErrReadOnly) {
		t.Errorf("DeleteDir err = %v", err)
	}
	if err := v.Rename("/x", "/y"); !errors.Is(err, ErrReadOnly) {
		t.Errorf("Rename err = %v", err)
	}
}

func TestImplementsInterface(t *testing.T) {
	v := loadFixture(t)
	defer v.Close()
	var _ filesystem.Filesystem = v
}

func TestOpenFile(t *testing.T) {
	// Materialise the decompressed fixture to a temp file and open by path.
	f, err := os.Open(filepath.Join("testdata", "hfsplus.dmg.gz"))
	if err != nil {
		t.Fatal(err)
	}
	defer f.Close()
	gz, err := gzip.NewReader(f)
	if err != nil {
		t.Fatal(err)
	}
	raw, err := io.ReadAll(gz)
	if err != nil {
		t.Fatal(err)
	}
	tmp := filepath.Join(t.TempDir(), "img.hfs")
	if err := os.WriteFile(tmp, raw, 0o644); err != nil {
		t.Fatal(err)
	}
	v, err := OpenFile(tmp)
	if err != nil {
		t.Fatal(err)
	}
	defer v.Close()
	if _, err := v.ReadFile("/hello.txt"); err != nil {
		t.Errorf("ReadFile via OpenFile: %v", err)
	}
}

func TestOpenBadImage(t *testing.T) {
	if _, err := Open(bytes.NewReader(make([]byte, 2048)), 2048); !errors.Is(err, ErrBadHeader) {
		t.Errorf("Open zero image err = %v, want ErrBadHeader", err)
	}
	if _, err := OpenFile(filepath.Join(t.TempDir(), "missing")); err == nil {
		t.Error("OpenFile missing path: want error")
	}
}
