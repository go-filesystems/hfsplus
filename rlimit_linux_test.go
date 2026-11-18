// Copyright (c) 2026, go-filesystems
// SPDX-License-Identifier: BSD-3-Clause

//go:build linux

package hfsplus

import (
	"fmt"
	"os"
	"strconv"
	"strings"
	"syscall"
	"testing"
)

// limitAddressSpace sets RLIMIT_AS (what `ulimit -v` sets) to the process's
// current virtual size plus slack bytes, so the child dies if it tries to map a
// large block, and returns a description for the log.
func limitAddressSpace(t *testing.T, slack uint64) string {
	t.Helper()
	b, err := os.ReadFile("/proc/self/statm")
	if err != nil {
		return "not set: " + err.Error()
	}
	pages, err := strconv.ParseUint(strings.Fields(string(b))[0], 10, 64)
	if err != nil {
		return "not set: " + err.Error()
	}
	limit := pages*uint64(os.Getpagesize()) + slack
	if err := syscall.Setrlimit(syscall.RLIMIT_AS, &syscall.Rlimit{Cur: limit, Max: limit}); err != nil {
		t.Fatalf("setrlimit(RLIMIT_AS, %d): %v", limit, err)
	}
	return fmt.Sprintf("RLIMIT_AS %d bytes (virtual size %d + %d)", limit, pages*uint64(os.Getpagesize()), slack)
}
