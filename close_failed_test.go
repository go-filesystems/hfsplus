package hfsplus

import (
	"errors"
	"os"
	"testing"
)

// failWrites is a store whose WriteAt, once armed, lets one write through and
// fails the rest: a commit that failed MID-write, with part of it on the disk.
type failWrites struct {
	*os.File
	armed bool
	n     int
	pass  int // writes let through once armed
}

func (f *failWrites) WriteAt(p []byte, off int64) (int, error) {
	if f.armed {
		f.n++
		if f.n > f.pass {
			return 0, errors.New("EIO")
		}
	}
	return f.File.WriteAt(p, off)
}

// A strict volume whose commit failed mid-write holds nothing back (no held
// dirty mark, nothing unsynced), but it is not consistent: Close must say so,
// at every durability level, and not return nil.
func TestCloseAfterAFailedCommitReturnsTheLatchedFailure(t *testing.T) {
	for _, d := range []Durability{DurabilityStrict, DurabilityMounted, DurabilityLazy} {
		t.Run(d.String(), func(t *testing.T) {
			p := sparseImage(t, 8<<20)
			f, err := os.OpenFile(p, os.O_RDWR, 0)
			if err != nil {
				t.Fatal(err)
			}
			defer f.Close()
			st, _ := f.Stat()
			s := &failWrites{File: f, pass: 3}
			v, err := OpenWritableAt(s, st.Size())
			if err != nil {
				t.Fatal(err)
			}
			if err := v.SetDurability(d); err != nil {
				t.Fatal(err)
			}
			s.armed = true
			if err := v.WriteFile("/a.bin", []byte("x"), 0o644); err == nil {
				t.Fatal("control: the commit did not fail")
			}
			s.armed = false
			if err := v.Close(); !errors.Is(err, ErrVolumeFailed) {
				t.Errorf("Close after a failed commit = %v, want ErrVolumeFailed", err)
			}
		})
	}
}
