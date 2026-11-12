// Copyright (c) 2026, go-filesystems
// SPDX-License-Identifier: BSD-3-Clause

//go:build darwin

package hfsplus

import (
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"testing"
	"time"
)

// TestDarwinWriteLatencyVsMacOS times a small durable write (create a file of one
// byte, fsync it) and a Chtimes on a volume mounted by macOS's own HFS+ driver
// against this driver on an identical image, interleaved, and logs the medians
// and the bytes this driver wrote. Opt-in (HFSPLUS_MEASURE=1): a timing is only
// meaningful on a quiet machine (1-minute load <= 8), which the caller checks.
func TestDarwinWriteLatencyVsMacOS(t *testing.T) {
	if os.Getenv("HFSPLUS_MEASURE") != "1" {
		t.Skip("set HFSPLUS_MEASURE=1 on a quiet machine")
	}
	requireTools(t)
	macImg := appleImage(t, "HFS+", "MACSIDE")
	ourImg := appleImage(t, "HFS+", "OURSIDE")
	dev, mnt := mountRaw(t, macImg)
	defer detach(dev)

	v, err := OpenFileWritable(ourImg)
	if err != nil {
		t.Fatal(err)
	}
	defer v.Close()
	c := instrument(v)

	const rounds = 30
	var macWrite, macTimes, ourWrite, ourTimes []time.Duration
	var ourBytesWrite, ourBytesTimes int
	for i := 0; i < rounds; i++ {
		name := fmt.Sprintf("f%03d.txt", i)
		now := time.Now()

		t0 := time.Now()
		f, err := os.Create(filepath.Join(mnt, name))
		if err != nil {
			t.Fatal(err)
		}
		f.Write([]byte{'x'})
		f.Sync()
		f.Close()
		macWrite = append(macWrite, time.Since(t0))
		t0 = time.Now()
		if err := os.Chtimes(filepath.Join(mnt, name), now, now); err != nil {
			t.Fatal(err)
		}
		macTimes = append(macTimes, time.Since(t0))

		b0 := c.bytes()
		t0 = time.Now()
		if err := v.WriteFile("/"+name, []byte{'x'}, 0o644); err != nil {
			t.Fatal(err)
		}
		ourWrite = append(ourWrite, time.Since(t0))
		b1 := c.bytes()
		t0 = time.Now()
		if err := v.Chtimes("/"+name, now, now); err != nil {
			t.Fatal(err)
		}
		ourTimes = append(ourTimes, time.Since(t0))
		ourBytesWrite += b1 - b0
		ourBytesTimes += c.bytes() - b1
	}
	med := func(d []time.Duration) time.Duration {
		s := append([]time.Duration(nil), d...)
		sort.Slice(s, func(i, j int) bool { return s[i] < s[j] })
		return s[len(s)/2]
	}
	t.Logf("create+1 byte+fsync: macOS driver %v, this driver %v (median of %d interleaved rounds)", med(macWrite), med(ourWrite), rounds)
	t.Logf("Chtimes:             macOS driver %v, this driver %v", med(macTimes), med(ourTimes))
	t.Logf("bytes written by this driver per round: WriteFile %d, Chtimes %d (%d WriteAt calls in all)", ourBytesWrite/rounds, ourBytesTimes/rounds, len(c.writes))
}
