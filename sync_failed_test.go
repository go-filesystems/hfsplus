package hfsplus

import (
	"errors"
	"os"
	"testing"
)

// failOnce is a file whose next Sync fails once: an EIO the kernel reports once
// and then forgets (fsyncgate), so a retried fsync succeeds without the data
// that failed being on the medium.
type failOnce struct {
	*os.File
	fail bool
}

func (f *failOnce) Sync() error {
	if f.fail {
		f.fail = false
		return errors.New("EIO")
	}
	return f.File.Sync()
}

// A Sync that failed is latched: every later Sync (and Close) returns
// ErrVolumeFailed wrapping the cause, and never nil. A caller retrying after an
// error, a server answering a client's second COMMIT or FLUSH, must not be told
// its data is durable by an fsync that only succeeded because the kernel
// forgot the error.
func TestSyncAfterFailedSyncStaysFailed(t *testing.T) {
	p := sparseImage(t, 8<<20)
	f, err := os.OpenFile(p, os.O_RDWR, 0)
	if err != nil {
		t.Fatal(err)
	}
	defer f.Close()
	st, _ := f.Stat()
	s := &failOnce{File: f}
	v, err := OpenWritableAt(s, st.Size())
	if err != nil {
		t.Fatal(err)
	}
	if err := v.SetDurability(DurabilityLazy); err != nil {
		t.Fatal(err)
	}
	for _, name := range []string{"/a.bin", "/b.bin"} {
		if err := v.WriteFile(name, []byte("x"), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	s.fail = true
	if err := v.Sync(); err == nil {
		t.Fatal("control: the first Sync did not fail")
	}
	for i := 0; i < 2; i++ {
		err := v.Sync()
		if !errors.Is(err, ErrVolumeFailed) {
			t.Fatalf("Sync #%d after a failed Sync = %v, want ErrVolumeFailed", i+2, err)
		}
	}
	if err := v.WriteFile("/c.bin", []byte("c"), 0o644); !errors.Is(err, ErrVolumeFailed) {
		t.Errorf("a write after a failed Sync = %v, want ErrVolumeFailed", err)
	}
	if err := v.Close(); !errors.Is(err, ErrVolumeFailed) {
		t.Errorf("Close after a failed Sync = %v, want ErrVolumeFailed", err)
	}
}
