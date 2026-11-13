// Copyright (c) 2026, go-filesystems
// SPDX-License-Identifier: BSD-3-Clause

package hfsplus

import (
	"bytes"
	"encoding/binary"
	"errors"
	"os"
	"path/filepath"
	"testing"
	"time"
)

// Tests for OpenWritableAt: the in-place writable open over a store the caller
// already holds (go-fileshare/fileshare#130 needs it to avoid reopening by name).

// TestOpenWritableAtRoundTrip: mutations through a descriptor the caller opened
// reach the file, and Close leaves the caller's descriptor open and usable.
func TestOpenWritableAtRoundTrip(t *testing.T) {
	p := sparseImage(t, 8<<20)
	f, err := os.OpenFile(p, os.O_RDWR, 0)
	if err != nil {
		t.Fatal(err)
	}
	st, err := f.Stat()
	if err != nil {
		t.Fatal(err)
	}
	v, err := OpenWritableAt(f, st.Size())
	if err != nil {
		t.Fatal(err)
	}
	payload := bytes.Repeat([]byte("at"), 3000)
	if err := v.WriteFile("/a.bin", payload, 0o644); err != nil {
		t.Fatal(err)
	}
	if err := v.MkDir("/d", 0o755); err != nil {
		t.Fatal(err)
	}
	if err := v.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}
	// The descriptor is still the caller's.
	var b [4]byte
	if _, err := f.ReadAt(b[:], 0); err != nil {
		t.Fatalf("the caller's file is closed by Volume.Close: %v", err)
	}
	if err := f.Sync(); err != nil {
		t.Fatalf("the caller's file is closed by Volume.Close: %v", err)
	}
	if err := f.Close(); err != nil {
		t.Fatalf("closing the caller's file: %v", err)
	}
	r, err := OpenFile(p)
	if err != nil {
		t.Fatal(err)
	}
	defer r.Close()
	if got, err := r.ReadFile("/a.bin"); err != nil || !bytes.Equal(got, payload) {
		t.Errorf("ReadFile: %d bytes, %v", len(got), err)
	}
	if _, err := r.Stat("/d"); err != nil {
		t.Errorf("Stat /d: %v", err)
	}
}

// TestOpenWritableAtSameAsFileWritable: the same operations through the new entry
// point and through OpenFileWritable leave byte-identical images, and the latter
// still closes its own file.
func TestOpenWritableAtSameAsFileWritable(t *testing.T) {
	base := sparseImage(t, 8<<20)
	orig, err := os.ReadFile(base)
	if err != nil {
		t.Fatal(err)
	}
	fixed := func(path string, open func(string) (*Volume, error)) []byte {
		if err := os.WriteFile(path, orig, 0o644); err != nil {
			t.Fatal(err)
		}
		v, err := open(path)
		if err != nil {
			t.Fatal(err)
		}
		if err := v.MkDir("/x", 0o755); err != nil {
			t.Fatal(err)
		}
		v.Close()
		out, _ := os.ReadFile(path)
		return out
	}
	dir := t.TempDir()
	pinned := time.Date(2026, 1, 2, 3, 4, 5, 0, time.UTC)
	timeNow = func() time.Time { return pinned }
	defer func() { timeNow = time.Now }()
	a := fixed(filepath.Join(dir, "a.img"), OpenFileWritable)
	b := fixed(filepath.Join(dir, "b.img"), func(path string) (*Volume, error) {
		f, err := os.OpenFile(path, os.O_RDWR, 0)
		if err != nil {
			return nil, err
		}
		t.Cleanup(func() { f.Close() })
		return OpenWritableAt(f, int64(len(orig)))
	})
	if !bytes.Equal(a, b) {
		t.Error("OpenWritableAt and OpenFileWritable leave different images")
	}
	// OpenFileWritable owns its file: Close releases it, a second Close reports it.
	v, err := OpenFileWritable(filepath.Join(dir, "a.img"))
	if err != nil {
		t.Fatal(err)
	}
	if err := v.Close(); err != nil {
		t.Fatal(err)
	}
	if err := v.Close(); err == nil {
		t.Error("OpenFileWritable's Close did not close its file")
	}
}

// TestOpenWritableAtRefusesPendingJournal: a journal holding transactions is
// refused through the new entry point, and nothing is written.
func TestOpenWritableAtRefusesPendingJournal(t *testing.T) {
	pending := buildJournalHeader(binary.LittleEndian, 0x4400, 0x6000, 0x80000, false)
	img, _, _ := journaledImage(t, kJIJournalInFSMask, pending, 0x80000)
	p := filepath.Join(t.TempDir(), "j.img")
	if err := os.WriteFile(p, img, 0o600); err != nil {
		t.Fatal(err)
	}
	f, err := os.OpenFile(p, os.O_RDWR, 0)
	if err != nil {
		t.Fatal(err)
	}
	defer f.Close()
	if _, err := OpenWritableAt(f, int64(len(img))); !errors.Is(err, ErrJournaled) {
		t.Fatalf("OpenWritableAt err = %v, want ErrJournaled", err)
	}
	if after, err := os.ReadFile(p); err != nil || !bytes.Equal(after, img) {
		t.Errorf("image changed by a refused open (read err %v)", err)
	}
	if _, err := f.Stat(); err != nil {
		t.Errorf("the caller's file was closed by the refused open: %v", err)
	}
}

// TestOpenWritableAtBadArguments: no store, or no size, is an error, not a panic.
func TestOpenWritableAtBadArguments(t *testing.T) {
	if _, err := OpenWritableAt(nil, 1<<20); err == nil {
		t.Error("nil store accepted")
	}
	f, err := os.CreateTemp(t.TempDir(), "x")
	if err != nil {
		t.Fatal(err)
	}
	defer f.Close()
	for _, size := range []int64{0, -1} {
		if _, err := OpenWritableAt(f, size); err == nil {
			t.Errorf("size %d accepted", size)
		}
	}
}
