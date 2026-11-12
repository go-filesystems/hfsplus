// Copyright (c) 2026, go-filesystems
// SPDX-License-Identifier: BSD-3-Clause

package hfsplus

import (
	"bytes"
	"fmt"
	"io"
	"testing"
	"time"
)

// memStore is a readerWriterAt over a byte slice with an fsync that does nothing:
// it drives the file-backed path (read cache, regions, flush order) with no file.
type memStore struct{ buf []byte }

func (m *memStore) ReadAt(p []byte, off int64) (int, error) {
	if off < 0 || off >= int64(len(m.buf)) {
		return 0, io.EOF
	}
	n := copy(p, m.buf[off:])
	if n < len(p) {
		return n, io.EOF
	}
	return n, nil
}

func (m *memStore) WriteAt(p []byte, off int64) (int, error) {
	if off < 0 || off+int64(len(p)) > int64(len(m.buf)) {
		return 0, fmt.Errorf("write [%d,+%d) outside the store", off, len(p))
	}
	return copy(m.buf[off:], p), nil
}

func (m *memStore) Sync() error { return nil }

// fuzzOps drives the mutators over v and returns, per call, whether it failed.
func fuzzOps(v *Volume) []bool {
	var failed []bool
	rec := func(err error) { failed = append(failed, err != nil) }
	rec(v.MkDir("/q", 0o755))
	rec(v.WriteFile("/q/z", make([]byte, 9000), 0o644))
	for i := 0; i < 40; i++ {
		rec(v.MkDir("/q/d"+string(rune('a'+i%26))+string(rune('a'+i/26)), 0o755))
	}
	rec(v.Truncate("/q/z", 20000))
	rec(v.DeleteFile("/q/z"))
	rec(v.Rename("/q", "/r"))
	rec(v.DeleteDir("/r"))
	return failed
}

// FuzzWritable splices fuzzer bytes into a valid image, opens it writable and
// drives the mutators over it, twice: over the in-memory image (OpenWritable) and
// in place over a readerWriterAt (the OpenFileWritable path, with its read cache
// and its regions). A hostile image may make a mutator fail; it must never
// panic, and the two paths must agree on which calls fail and on the image they
// leave. (Added after the review of go-filesystems/hfsplus#34 found a panic in
// adjustParentValence on a patched image.)
func FuzzWritable(f *testing.F) {
	base := fuzzSeed(f)
	// The two runs must stamp the same time on what they create.
	fixed := time.Date(2026, 1, 2, 3, 4, 5, 0, time.UTC)
	timeNow = func() time.Time { return fixed }
	f.Cleanup(func() { timeNow = time.Now })
	f.Add(int64(volumeHeaderOffset+112), []byte{0xff, 0xff, 0xff, 0xff})
	f.Add(int64(5*4096), []byte{0, 0, 0, 9})
	f.Add(int64(6*4096), []byte{0xff, 0xff})
	f.Fuzz(func(t *testing.T, off int64, patch []byte) {
		if len(patch) == 0 || len(patch) > 4096 || off < 0 || off >= int64(len(base)) {
			return
		}
		img := append([]byte(nil), base...)
		copy(img[off:], patch)
		inMem := append([]byte(nil), img...)
		v, err := OpenWritable(inMem, nil)
		st := &memStore{buf: append([]byte(nil), img...)}
		w, err2 := openInPlace(st, int64(len(img)))
		if (err == nil) != (err2 == nil) {
			t.Fatalf("OpenWritable err=%v, in-place open err=%v", err, err2)
		}
		if err != nil {
			return
		}
		a, b := fuzzOps(v), fuzzOps(w)
		for i := range a {
			if a[i] != b[i] {
				t.Fatalf("call %d: failed in memory=%v, in place=%v", i, a[i], b[i])
			}
		}
		// A volume left latched by a failed flush is not compared: the in-place
		// path's disk is then in an unknown state by design.
		if w.failed == nil && !bytes.Equal(v.Bytes(), st.buf) {
			t.Fatal("the in-place image differs from the in-memory one")
		}
	})
}
