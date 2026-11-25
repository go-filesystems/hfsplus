// Copyright (c) 2026, go-filesystems
// SPDX-License-Identifier: BSD-3-Clause

package hfsplus

import (
	"bytes"
	"errors"
	"fmt"
	"sync/atomic"
	"testing"
)

// faultRW is a ReadWriteSyncerAt over img that fails the failRead-th ReadAt, the
// failWrite-th WriteAt and the failSync-th Sync (1-based, 0: never), once each.
type faultRW struct {
	img                           []byte
	failRead, failWrite, failSync int64
	reads, writes, syncs          atomic.Int64
}

var errInjectedIO = errors.New("injected io failure")

func (f *faultRW) ReadAt(p []byte, off int64) (int, error) {
	if f.reads.Add(1) == f.failRead {
		return 0, errInjectedIO
	}
	return bytesReaderAt(f.img).ReadAt(p, off)
}

func (f *faultRW) WriteAt(p []byte, off int64) (int, error) {
	if f.writes.Add(1) == f.failWrite {
		return 0, errInjectedIO
	}
	if off < 0 || off+int64(len(p)) > int64(len(f.img)) {
		return 0, errors.New("write out of range")
	}
	return copy(f.img[off:], p), nil
}

func (f *faultRW) Sync() error {
	if f.syncs.Add(1) == f.failSync {
		return errInjectedIO
	}
	return nil
}

// mutation is one writable operation and the check that its effect is there.
type mutation struct {
	name   string
	setup  func(v *Volume) error
	op     func(v *Volume) error
	effect func(v *Volume) error
}

func mutationsUnderTest() []mutation {
	big := bytes.Repeat([]byte("0123456789abcdef"), 4096) // several allocation blocks
	has := func(path string, want []byte) func(v *Volume) error {
		return func(v *Volume) error {
			got, err := v.ReadFile(path)
			if err != nil {
				return err
			}
			if !bytes.Equal(got, want) {
				return fmt.Errorf("%s holds %d bytes, want %d", path, len(got), len(want))
			}
			return nil
		}
	}
	return []mutation{
		{
			name: "MkDir",
			op:   func(v *Volume) error { return v.MkDir("/new", 0o755) },
			effect: func(v *Volume) error {
				_, err := v.Stat("/new")
				return err
			},
		},
		{
			name:   "WriteFile",
			op:     func(v *Volume) error { return v.WriteFile("/big", big, 0o644) },
			effect: has("/big", big),
		},
		{
			name:   "Symlink",
			op:     func(v *Volume) error { return v.Symlink("/target", "/lnk") },
			effect: func(v *Volume) error { _, err := v.ReadLink("/lnk"); return err },
		},
		{
			name:  "DeleteFile",
			setup: func(v *Volume) error { return v.WriteFile("/gone", big, 0o644) },
			op:    func(v *Volume) error { return v.DeleteFile("/gone") },
			effect: func(v *Volume) error {
				if _, err := v.Stat("/gone"); !errors.Is(err, ErrNotFound) {
					return fmt.Errorf("/gone after DeleteFile: %v", err)
				}
				return nil
			},
		},
		{
			name:   "Rename",
			setup:  func(v *Volume) error { return v.WriteFile("/old", big, 0o644) },
			op:     func(v *Volume) error { return v.Rename("/old", "/renamed") },
			effect: has("/renamed", big),
		},
		{
			name:   "Truncate",
			setup:  func(v *Volume) error { return v.WriteFile("/t", big, 0o644) },
			op:     func(v *Volume) error { return v.Truncate("/t", 100) },
			effect: has("/t", big[:100]),
		},
	}
}

// mutateNoPanic runs f and turns a panic into an error: a guard that is gone would
// otherwise end the test binary, which a mutation run reads as weak.
func mutateNoPanic(f func() error) (err error) {
	defer func() {
		if p := recover(); p != nil {
			err = fmt.Errorf("panic: %v", p)
		}
	}()
	return f()
}

// Every read, write and fsync of a mutation fails once, in turn (mutsweep,
// hfsplus#55: the error returns of hfsplus.go and store.go survived because no test
// failed a store call there). The mutation either returns an error or has its effect,
// and the failure never panics.
func TestMutationStoreFaults(t *testing.T) {
	for _, m := range mutationsUnderTest() {
		t.Run(m.name, func(t *testing.T) {
			base, err := Mkfs(8<<20, FormatConfig{Label: "MFAULT"})
			if err != nil {
				t.Fatal(err)
			}
			// run opens a fresh copy, does the setup, then runs the op with the
			// n-th call of kind failing (n == 0: none fails). It returns the number of
			// calls of each kind the op made, and what the op and the effect check
			// returned.
			run := func(kind string, n int64) (calls map[string]int64, opErr, effErr error) {
				f := &faultRW{img: bytes.Clone(base)}
				v, err := OpenWritableAt(f, int64(len(f.img)))
				if err != nil {
					t.Fatalf("OpenWritableAt: %v", err)
				}
				if m.setup != nil {
					if err := m.setup(v); err != nil {
						t.Fatalf("setup: %v", err)
					}
				}
				switch kind {
				case "read":
					f.failRead = f.reads.Load() + n
				case "write":
					f.failWrite = f.writes.Load() + n
				case "sync":
					f.failSync = f.syncs.Load() + n
				}
				if n == 0 {
					f.failRead, f.failWrite, f.failSync = 0, 0, 0
				}
				r0, w0, s0 := f.reads.Load(), f.writes.Load(), f.syncs.Load()
				opErr = mutateNoPanic(func() error { return m.op(v) })
				calls = map[string]int64{
					"read":  f.reads.Load() - r0,
					"write": f.writes.Load() - w0,
					"sync":  f.syncs.Load() - s0,
				}
				// The effect check runs with no fault armed.
				f.failRead, f.failWrite, f.failSync = 0, 0, 0
				if opErr == nil {
					effErr = mutateNoPanic(func() error { return m.effect(v) })
				}
				return calls, opErr, effErr
			}
			counts, opErr, effErr := run("", 0)
			if opErr != nil || effErr != nil {
				t.Fatalf("unfaulted: %v, %v", opErr, effErr)
			}
			for kind, total := range counts {
				if total == 0 {
					t.Fatalf("the op made no %s calls: the sweep would be vacuous", kind)
				}
				for n := int64(1); n <= total; n++ {
					_, opErr, effErr := run(kind, n)
					if opErr != nil {
						continue
					}
					if effErr != nil {
						t.Errorf("%s %d of %d fails: the op returned nil but its effect is missing: %v", kind, n, total, effErr)
					}
				}
			}
		})
	}
}
