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

// sizedImage returns the path of a 8 MiB sparse volume followed by extra trailing
// bytes (0xEE), and the volume's length.
func sizedImage(t *testing.T, extra int) (path string, volLen int64) {
	t.Helper()
	p := sparseImage(t, 8<<20)
	if extra > 0 {
		f, err := os.OpenFile(p, os.O_RDWR|os.O_APPEND, 0)
		if err != nil {
			t.Fatal(err)
		}
		if _, err := f.Write(bytes.Repeat([]byte{0xEE}, extra)); err != nil {
			t.Fatal(err)
		}
		f.Close()
	}
	return p, 8 << 20
}

// openAt opens path through OpenWritableAt with the given size.
func openAt(t *testing.T, path string, size int64) (*Volume, *os.File, error) {
	t.Helper()
	f, err := os.OpenFile(path, os.O_RDWR, 0)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { f.Close() })
	v, err := OpenWritableAt(f, size)
	return v, f, err
}

// TestWritableOpenSizeMustFitTheVolume: the alternate header sits 1024 bytes before
// the end of the VOLUME (TN1150), not of the size the caller names or of the file.
// A size smaller than the volume is refused with nothing written; a size larger
// than the file, or a file with trailing bytes, writes the alternate header at the
// volume's end and touches nothing after it.
func TestWritableOpenSizeMustFitTheVolume(t *testing.T) {
	// Too small, through OpenWritableAt: refused, file unchanged.
	p, vol := sizedImage(t, 0)
	orig, _ := os.ReadFile(p)
	for _, size := range []int64{vol - 1, vol - 4096, 7 << 20, 1 << 20, 2048} {
		if _, _, err := openAt(t, p, size); !errors.Is(err, ErrVolumeSize) {
			t.Errorf("size %d: err = %v, want ErrVolumeSize", size, err)
		}
	}
	// Too small, through OpenFileWritable: a truncated file.
	if err := os.Truncate(p, vol-4096); err != nil {
		t.Fatal(err)
	}
	if _, err := OpenFileWritable(p); !errors.Is(err, ErrVolumeSize) {
		t.Errorf("a truncated file: err = %v, want ErrVolumeSize", err)
	}
	if err := os.WriteFile(p, orig, 0o644); err != nil {
		t.Fatal(err)
	}
	// No volume header where the alternate must be.
	z := bytes.Clone(orig)
	clear(z[vol-1024 : vol-1024+512])
	zp := filepath.Join(t.TempDir(), "noalt.img")
	if err := os.WriteFile(zp, z, 0o644); err != nil {
		t.Fatal(err)
	}
	if _, _, err := openAt(t, zp, vol); !errors.Is(err, ErrVolumeSize) {
		t.Errorf("no alternate header: err = %v, want ErrVolumeSize", err)
	}
	if _, err := OpenFileWritable(zp); !errors.Is(err, ErrVolumeSize) {
		t.Errorf("no alternate header (file): err = %v, want ErrVolumeSize", err)
	}
	if after, _ := os.ReadFile(zp); !bytes.Equal(after, z) {
		t.Error("a refused open changed the image")
	}

	// Larger size than the file, and trailing bytes, through both entry points.
	const extra = 4096
	for _, tc := range []struct {
		name string
		open func(path string) (*Volume, error)
	}{
		{"OpenFileWritable", OpenFileWritable},
		{"OpenWritableAt, size = file", func(path string) (*Volume, error) {
			st, _ := os.Stat(path)
			v, _, err := openAt(t, path, st.Size())
			return v, err
		}},
		{"OpenWritableAt, size beyond the file", func(path string) (*Volume, error) {
			v, _, err := openAt(t, path, vol+(1<<20))
			return v, err
		}},
	} {
		tp, _ := sizedImage(t, extra)
		before, _ := os.ReadFile(tp)
		v, err := tc.open(tp)
		if err != nil {
			t.Fatalf("%s: %v", tc.name, err)
		}
		if err := v.MkDir("/t", 0o755); err != nil {
			t.Fatalf("%s: %v", tc.name, err)
		}
		v.Close()
		after, _ := os.ReadFile(tp)
		if len(after) != len(before) {
			t.Errorf("%s: the file grew from %d to %d bytes", tc.name, len(before), len(after))
		}
		if !bytes.Equal(after[vol:], before[vol:]) {
			t.Errorf("%s: bytes after the volume were written", tc.name)
		}
		alt, pri := after[vol-1024:vol-512], after[volumeHeaderOffset:volumeHeaderOffset+512]
		if !bytes.Equal(alt, pri) {
			t.Errorf("%s: the alternate header at the volume's end is not the primary's copy", tc.name)
		}
		r, err := OpenFile(tp)
		if err != nil {
			t.Fatalf("%s: %v", tc.name, err)
		}
		if _, err := r.Stat("/t"); err != nil {
			t.Errorf("%s: %v", tc.name, err)
		}
		r.Close()
	}
}

// TestClosedVolumeRefusesMutations: after Close a writable volume returns
// ErrClosed and keeps no cache or edit state.
func TestClosedVolumeRefusesMutations(t *testing.T) {
	p, _ := sizedImage(t, 0)
	for name, open := range map[string]func() (*Volume, error){
		"OpenFileWritable": func() (*Volume, error) { return OpenFileWritable(p) },
		"OpenWritableAt": func() (*Volume, error) {
			v, _, err := openAt(t, p, 8<<20)
			return v, err
		},
	} {
		v, err := open()
		if err != nil {
			t.Fatal(err)
		}
		if err := v.MkDir("/k"+name[len(name)-2:], 0o755); err != nil {
			t.Fatal(err)
		}
		before, _ := os.ReadFile(p)
		v.Close()
		for _, err := range []error{
			v.MkDir("/z", 0o755),
			v.WriteFile("/z", []byte("x"), 0o644),
			v.DeleteDir("/k" + name[len(name)-2:]),
			v.Chtimes("/k"+name[len(name)-2:], time.Now(), time.Now()),
		} {
			if !errors.Is(err, ErrClosed) {
				t.Errorf("%s: a mutation after Close returned %v, want ErrClosed", name, err)
			}
		}
		if v.regions != nil || v.rcache != nil {
			t.Errorf("%s: Close keeps its cache or regions", name)
		}
		if after, _ := os.ReadFile(p); !bytes.Equal(after, before) {
			t.Errorf("%s: a mutation after Close changed the image", name)
		}
	}
}
