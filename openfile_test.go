// Copyright (c) 2026, go-filesystems
// SPDX-License-Identifier: BSD-3-Clause

package hfsplus

import (
	"bytes"
	"errors"
	"io"
	"path/filepath"
	"testing"

	filesystem "github.com/go-filesystems/interface"
)

func offsetPattern(n int) []byte {
	b := make([]byte, n)
	for i := range b {
		b[i] = byte(i*7 + i>>12)
	}
	return b
}

func bigVolume(t testing.TB, fileSize int) (filesystem.Filesystem, []byte) {
	t.Helper()
	path := filepath.Join(t.TempDir(), "big.hfs")
	fs, err := Format(path, int64(fileSize)*2+16<<20, FormatConfig{Label: "BIG"})
	if err != nil {
		t.Fatalf("Format: %v", err)
	}
	data := offsetPattern(fileSize)
	if err := fs.WriteFile("/big", data, 0o644); err != nil {
		t.Fatalf("WriteFile: %v", err)
	}
	t.Cleanup(func() { fs.Close() })
	return fs, data
}

// A READ at an offset past 64 MiB must not need the file whole.
func TestOpenFileReadsARangePast64MiB(t *testing.T) {
	const fileSize = 72 << 20
	fs, want := bigVolume(t, fileSize)
	o, ok := fs.(filesystem.Opener)
	if !ok {
		t.Fatal("the driver is not a filesystem.Opener")
	}
	f, err := o.OpenFile("/big")
	if err != nil {
		t.Fatal(err)
	}
	defer f.Close()
	if f.Size() != fileSize {
		t.Fatalf("Size = %d", f.Size())
	}
	for _, off := range []int64{0, 4095, 64 << 20, 66<<20 + 123, fileSize - 10} {
		buf := make([]byte, 4096)
		n, err := f.ReadAt(buf, off)
		wantN := min(int64(len(buf)), fileSize-off)
		if int64(n) != wantN || (n < len(buf)) != errors.Is(err, io.EOF) || (n == len(buf) && err != nil) {
			t.Fatalf("ReadAt(%d) = %d, %v; want %d", off, n, err, wantN)
		}
		if !bytes.Equal(buf[:n], want[off:off+int64(n)]) {
			t.Fatalf("ReadAt(%d): wrong bytes", off)
		}
	}
	if n, err := f.ReadAt(make([]byte, 8), fileSize); n != 0 || !errors.Is(err, io.EOF) {
		t.Errorf("ReadAt at the end = %d, %v", n, err)
	}
	if _, err := f.ReadAt(make([]byte, 8), -1); err == nil {
		t.Error("a negative offset was read")
	}
	if err := f.Close(); err != nil {
		t.Error(err)
	}
}

func TestOpenFileRefusals(t *testing.T) {
	fs, _ := bigVolume(t, 1<<20)
	o := fs.(filesystem.Opener)
	if _, err := o.OpenFile("/nope"); err == nil {
		t.Error("a missing file was opened")
	}
	if _, err := o.OpenFile("/"); !errors.Is(err, ErrNotRegular) {
		t.Errorf("a directory: %v", err)
	}
}

// BenchmarkReadRangeOfBigFile: a 1 MiB read at 66 MiB of a 72 MiB file, through
// OpenFile/ReadAt and through ReadFile. Allocations are exact; times are not
// under load.
func BenchmarkReadRangeOfBigFile(b *testing.B) {
	fs, _ := bigVolume(b, 72<<20)
	b.Run("OpenFile-ReadAt", func(b *testing.B) {
		f, err := fs.(filesystem.Opener).OpenFile("/big")
		if err != nil {
			b.Fatal(err)
		}
		buf := make([]byte, 1<<20)
		b.SetBytes(int64(len(buf)))
		b.ReportAllocs()
		for b.Loop() {
			if _, err := f.ReadAt(buf, 66<<20); err != nil {
				b.Fatal(err)
			}
		}
	})
	b.Run("ReadFile-whole", func(b *testing.B) {
		b.SetBytes(1 << 20)
		b.ReportAllocs()
		for b.Loop() {
			data, err := fs.ReadFile("/big")
			if err != nil {
				b.Fatal(err)
			}
			_ = data[66<<20 : 67<<20]
		}
	})
}
