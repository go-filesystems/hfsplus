// Copyright (c) 2026, go-filesystems
// SPDX-License-Identifier: BSD-3-Clause

//go:build !linux

package hfsplus

import "testing"

// limitAddressSpace: only Linux enforces RLIMIT_AS (`ulimit -v` is accepted and
// ignored on macOS), so elsewhere the child runs under GOMEMLIMIT alone.
func limitAddressSpace(t *testing.T, slack uint64) string {
	t.Helper()
	return "not enforced on this OS (GOMEMLIMIT only)"
}
