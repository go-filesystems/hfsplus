// Copyright (c) 2026, go-filesystems
// SPDX-License-Identifier: BSD-3-Clause

package hfsplus

import (
	"errors"
	"os"
	"path/filepath"
	"slices"
	"sync/atomic"
	"testing"
)

// failingReaderAt serves img and fails the n-th ReadAt (1-based, 0: never), once.
type failingReaderAt struct {
	img   []byte
	n     int64
	reads atomic.Int64
}

var errInjectedRead = errors.New("injected read failure")

func (f *failingReaderAt) ReadAt(p []byte, off int64) (int, error) {
	if k := f.reads.Add(1); k == f.n {
		return 0, errInjectedRead
	}
	return bytesReaderAt(f.img).ReadAt(p, off)
}

// Every read of Open fails once, in turn (mutsweep, hfsplus#55: the error returns of
// openTrees and Open survived because no test failed a read there). Open either
// returns an error or a volume that serves the same root as the pristine one:
// never a volume with a tree it failed to open.
func TestOpenReadFaults(t *testing.T) {
	img, err := Mkfs(8<<20, FormatConfig{Label: "FAULT"})
	if err != nil {
		t.Fatal(err)
	}
	w, err := OpenWritable(img, nil)
	if err != nil {
		t.Fatal(err)
	}
	for _, n := range []string{"/a", "/b"} {
		if err := w.MkDir(n, 0o755); err != nil {
			t.Fatal(err)
		}
	}
	if err := w.Close(); err != nil {
		t.Fatal(err)
	}
	want, err := rootNames(mustOpen(t, img, false))
	if err != nil {
		t.Fatal(err)
	}
	count := &failingReaderAt{img: img}
	if _, err := Open(count, int64(len(img))); err != nil {
		t.Fatal(err)
	}
	total := count.reads.Load()
	if total < 3 {
		t.Fatalf("Open made %d reads: the sweep would be vacuous", total)
	}
	refused := 0
	for n := int64(1); n <= total; n++ {
		f := &failingReaderAt{img: img, n: n}
		v, err := Open(f, int64(len(img)))
		if err != nil {
			refused++
			if v != nil {
				t.Errorf("read %d fails: Open returned an error and a volume", n)
			}
			continue
		}
		got, err := rootNames(v)
		if err != nil || !slices.Equal(got, want) {
			t.Errorf("read %d fails: Open succeeded but the root is %v, %v; want %v", n, got, err, want)
		}
	}
	if refused == 0 {
		t.Error("no failed read made Open fail")
	}
}

// OpenWritable refuses an image that is no volume, and one cut short of its alternate
// volume header, without opening it (hfsplus#55: both returns survived mutsweep).
func TestOpenWritableRefusals(t *testing.T) {
	if v, err := OpenWritable(make([]byte, 1<<20), nil); err == nil || v != nil {
		t.Errorf("an image of zeros: %v, %v", v, err)
	}
	img, err := Mkfs(8<<20, FormatConfig{Label: "CUT"})
	if err != nil {
		t.Fatal(err)
	}
	v, err := OpenWritable(img[:len(img)-8192], nil)
	if !errors.Is(err, ErrVolumeSize) || v != nil {
		t.Errorf("an image cut before the alternate header: %v, %v; want ErrVolumeSize", v, err)
	}
}

// OpenFile refuses a path that does not exist and a file that is not a volume,
// and does not leave the file open (hfsplus#55).
func TestOpenFilePathRefusals(t *testing.T) {
	dir := t.TempDir()
	if v, err := OpenFile(filepath.Join(dir, "missing.img")); err == nil || v != nil {
		t.Errorf("a missing file: %v, %v", v, err)
	}
	junk := filepath.Join(dir, "junk.img")
	if err := os.WriteFile(junk, make([]byte, 8192), 0o644); err != nil {
		t.Fatal(err)
	}
	if v, err := OpenFile(junk); err == nil || v != nil {
		t.Errorf("a file of zeros: %v, %v", v, err)
	}
	if v, err := OpenFile(dir); err == nil || v != nil {
		t.Errorf("a directory: %v, %v", v, err)
	}
}
