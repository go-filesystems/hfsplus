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
// byte, fsync it), a durable mkdir (a commit that carries no file data, the one the
// journal writer journals) and a Chtimes on volumes mounted by macOS's own HFS+
// driver (plain and journaled) against this driver at every durability level (and
// Strict over fsync(2), and with journal writes on a Journaled HFS+ image), each on
// an identical image, interleaved, and logs the medians, the first round separately
// and the exact fsync count per operation. Opt-in (HFSPLUS_MEASURE=1): a timing is
// only meaningful on a quiet machine (1-minute load <= 8), which the caller checks.
func TestDarwinWriteLatencyVsMacOS(t *testing.T) {
	if os.Getenv("HFSPLUS_MEASURE") != "1" {
		t.Skip("set HFSPLUS_MEASURE=1 on a quiet machine")
	}
	requireTools(t)
	type macSide struct {
		name        string
		mnt         string
		write, mkdr []time.Duration
	}
	var macs []*macSide
	for _, cfg := range []struct{ name, fs string }{{"macOS driver (HFS+)", "HFS+"}, {"macOS driver (Journaled HFS+)", "Journaled HFS+"}} {
		dev, mnt := mountRaw(t, appleImage(t, cfg.fs, "MACSIDE"))
		defer detach(dev)
		macs = append(macs, &macSide{name: cfg.name, mnt: mnt})
	}

	type side struct {
		name              string
		v                 *Volume
		c                 *countingWriter
		write, mkdr, tms  []time.Duration
		wSync, mSync, tSy []int
	}
	var sides []*side
	for _, cfg := range []struct {
		name    string
		fs      string
		d       Durability
		plain   bool
		journal bool
	}{
		{"strict (F_FULLFSYNC)", "HFS+", DurabilityStrict, false, false},
		{"strict (fsync(2))", "HFS+", DurabilityStrict, true, false},
		{"mounted (F_FULLFSYNC)", "HFS+", DurabilityMounted, false, false},
		{"lazy (no barrier per commit)", "HFS+", DurabilityLazy, false, false},
		{"strict, Journaled HFS+ image", "Journaled HFS+", DurabilityStrict, false, false},
		{"journal writes, Journaled HFS+", "Journaled HFS+", DurabilityStrict, false, true},
	} {
		img := appleImage(t, cfg.fs, "OURSIDE")
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
		if cfg.journal {
			if err := v.EnableJournalWrites(); err != nil {
				t.Fatal(err)
			}
		}
		sides = append(sides, &side{name: cfg.name, v: v, c: instrument(v)})
	}

	const rounds = 30
	for i := 0; i < rounds; i++ {
		name := fmt.Sprintf("f%03d.txt", i)
		dir := fmt.Sprintf("m%03d", i)
		now := time.Now()
		for _, m := range macs {
			t0 := time.Now()
			f, err := os.Create(filepath.Join(m.mnt, name))
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
			m.write = append(m.write, time.Since(t0))
			t0 = time.Now()
			if err := os.Mkdir(filepath.Join(m.mnt, dir), 0o755); err != nil {
				t.Fatal(err)
			}
			df, err := os.Open(filepath.Join(m.mnt, dir))
			if err != nil {
				t.Fatal(err)
			}
			df.Sync()
			df.Close()
			m.mkdr = append(m.mkdr, time.Since(t0))
		}
		for _, s := range sides {
			run := func(op func() error, durs *[]time.Duration, syncs *[]int) {
				before := s.c.syncs
				t0 := time.Now()
				if err := op(); err != nil {
					t.Fatal(err)
				}
				*durs = append(*durs, time.Since(t0))
				*syncs = append(*syncs, s.c.syncs-before)
			}
			run(func() error { return s.v.WriteFile("/"+name, []byte{'x'}, 0o644) }, &s.write, &s.wSync)
			run(func() error { return s.v.MkDir("/"+dir, 0o755) }, &s.mkdr, &s.mSync)
			run(func() error { return s.v.Chtimes("/"+name, now, now) }, &s.tms, &s.tSy)
		}
	}
	med := func(d []time.Duration) time.Duration {
		s := append([]time.Duration(nil), d[1:]...) // rounds 2..: the first is reported apart
		sort.Slice(s, func(i, j int) bool { return s[i] < s[j] })
		return s[len(s)/2]
	}
	medI := func(d []int) float64 {
		s := append([]int(nil), d[1:]...)
		sort.Ints(s)
		return float64(s[len(s)/2])
	}
	t.Logf("%d interleaved rounds; medians over rounds 2-%d, round 1 apart", rounds, rounds)
	for _, m := range macs {
		t.Logf("%-34s create+1 byte+fsync %v (round 1 %v), mkdir+fsync %v (round 1 %v)", m.name+":", med(m.write), m.write[0], med(m.mkdr), m.mkdr[0])
	}
	for _, s := range sides {
		t.Logf("%-34s create+1 byte %v (r1 %v, %v/%v fsyncs), mkdir %v (r1 %v, %v/%v fsyncs), Chtimes %v (r1 %v, %v/%v fsyncs)",
			s.name+":", med(s.write), s.write[0], medI(s.wSync), s.wSync[0],
			med(s.mkdr), s.mkdr[0], medI(s.mSync), s.mSync[0],
			med(s.tms), s.tms[0], medI(s.tSy), s.tSy[0])
	}
}
