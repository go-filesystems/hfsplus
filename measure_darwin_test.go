// Copyright (c) 2026, go-filesystems
// SPDX-License-Identifier: BSD-3-Clause

//go:build darwin

package hfsplus

import (
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"syscall"
	"testing"
	"time"
)

// plainFsync is an *os.File whose Sync is fsync(2), not Go's F_FULLFSYNC: it
// asks the drive for nothing. man 2 fsync: the drive "may be written in an
// out-of-order sequence ... later writes may be present, while earlier writes are
// not"; man 2 fcntl, F_FULLFSYNC: it "acts as a barrier". So the ordering the
// commit relies on is only a guarantee over F_FULLFSYNC: this variant is measured
// to price the primitive, it is not offered as a mode.
type plainFsync struct{ *os.File }

func (p plainFsync) Sync() error {
	rc, err := p.SyscallConn()
	if err != nil {
		return err
	}
	var serr error
	if err := rc.Control(func(fd uintptr) { serr = syscall.Fsync(int(fd)) }); err != nil {
		return err
	}
	return serr
}

// TestDarwinWriteLatencyVsMacOS times a small durable write (create a file of one
// byte, fsync it) and a Chtimes on a volume mounted by macOS's own HFS+ driver
// against this driver at every durability level (and Strict over fsync(2)), each
// on an identical image, interleaved, and logs the medians and the exact fsync
// count. Opt-in (HFSPLUS_MEASURE=1): a timing is only meaningful on a quiet
// machine (1-minute load <= 8), which the caller checks.
func TestDarwinWriteLatencyVsMacOS(t *testing.T) {
	if os.Getenv("HFSPLUS_MEASURE") != "1" {
		t.Skip("set HFSPLUS_MEASURE=1 on a quiet machine")
	}
	requireTools(t)
	macImg := appleImage(t, "HFS+", "MACSIDE")
	dev, mnt := mountRaw(t, macImg)
	defer detach(dev)

	type side struct {
		name         string
		v            *Volume
		c            *countingWriter
		write, times []time.Duration
		syncs        int
	}
	var sides []*side
	for _, cfg := range []struct {
		name  string
		d     Durability
		plain bool
	}{
		{"strict (F_FULLFSYNC)", DurabilityStrict, false},
		{"strict (fsync(2))", DurabilityStrict, true},
		{"mounted (F_FULLFSYNC)", DurabilityMounted, false},
		{"lazy (no barrier per commit)", DurabilityLazy, false},
	} {
		img := appleImage(t, "HFS+", "OURSIDE")
		var v *Volume
		if cfg.plain {
			f, err := os.OpenFile(img, os.O_RDWR, 0)
			if err != nil {
				t.Fatal(err)
			}
			defer f.Close()
			st, _ := f.Stat()
			if v, err = OpenWritableAt(plainFsync{f}, st.Size()); err != nil {
				t.Fatal(err)
			}
		} else {
			var err error
			if v, err = OpenFileWritable(img); err != nil {
				t.Fatal(err)
			}
		}
		defer v.Close()
		if err := v.SetDurability(cfg.d); err != nil {
			t.Fatal(err)
		}
		sides = append(sides, &side{name: cfg.name, v: v, c: instrument(v)})
	}

	const rounds = 30
	var macWrite, macTimes []time.Duration
	for i := 0; i < rounds; i++ {
		name := fmt.Sprintf("f%03d.txt", i)
		now := time.Now()

		t0 := time.Now()
		f, err := os.Create(filepath.Join(mnt, name))
		if err != nil {
			t.Fatal(err)
		}
		if _, err := f.Write([]byte{'x'}); err != nil {
			t.Fatal(err)
		}
		if err := f.Sync(); err != nil {
			t.Fatal(err)
		}
		if err := f.Close(); err != nil {
			t.Fatal(err)
		}
		macWrite = append(macWrite, time.Since(t0))
		t0 = time.Now()
		if err := os.Chtimes(filepath.Join(mnt, name), now, now); err != nil {
			t.Fatal(err)
		}
		macTimes = append(macTimes, time.Since(t0))

		for _, s := range sides {
			before := s.c.syncs
			t0 = time.Now()
			if err := s.v.WriteFile("/"+name, []byte{'x'}, 0o644); err != nil {
				t.Fatal(err)
			}
			s.write = append(s.write, time.Since(t0))
			t0 = time.Now()
			if err := s.v.Chtimes("/"+name, now, now); err != nil {
				t.Fatal(err)
			}
			s.times = append(s.times, time.Since(t0))
			s.syncs += s.c.syncs - before
		}
	}
	med := func(d []time.Duration) time.Duration {
		s := append([]time.Duration(nil), d...)
		sort.Slice(s, func(i, j int) bool { return s[i] < s[j] })
		return s[len(s)/2]
	}
	t.Logf("macOS driver (F_FULLFSYNC from Go's File.Sync): create+1 byte+fsync %v, Chtimes (not durable) %v; medians of %d interleaved rounds", med(macWrite), med(macTimes), rounds)
	for _, s := range sides {
		t.Logf("this driver, %-30s create+1 byte %v, Chtimes %v, %.1f fsyncs per round (WriteFile+Chtimes)",
			s.name+":", med(s.write), med(s.times), float64(s.syncs)/rounds)
	}
}
