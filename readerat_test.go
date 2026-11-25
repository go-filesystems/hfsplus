// Copyright (c) 2026, go-filesystems
// SPDX-License-Identifier: BSD-3-Clause

package hfsplus

import (
	"io"
	"testing"
)

// bytesReaderAt follows the io.ReaderAt contract at its edges (hfsplus#55: the
// range check and the short-read return survived mutsweep): nothing before the
// start or at and past the end, and a read that runs off the end returns what is
// there with io.EOF.
func TestBytesReaderAtEdges(t *testing.T) {
	b := bytesReaderAt("0123456789")
	for _, tc := range []struct {
		off  int64
		size int
		n    int
		err  error
		want string
	}{
		{-1, 4, 0, io.EOF, ""},
		{10, 4, 0, io.EOF, ""},
		{11, 4, 0, io.EOF, ""},
		{1 << 40, 4, 0, io.EOF, ""},
		{8, 4, 2, io.EOF, "89"},
		{0, 10, 10, nil, "0123456789"},
		{6, 4, 4, nil, "6789"},
	} {
		p := make([]byte, tc.size)
		n, err := b.ReadAt(p, tc.off)
		if n != tc.n || err != tc.err || string(p[:n]) != tc.want {
			t.Errorf("ReadAt(%d bytes, %d) = %d, %v, %q; want %d, %v, %q", tc.size, tc.off, n, err, p[:n], tc.n, tc.err, tc.want)
		}
	}
}

// Sync on a volume opened read-only does nothing and says so (hfsplus#55): the
// answer is nil, whatever the store is.
func TestSyncReadOnlyIsANoOp(t *testing.T) {
	img, err := Mkfs(8<<20, FormatConfig{Label: "RO"})
	if err != nil {
		t.Fatal(err)
	}
	v, err := Open(bytesReaderAt(img), int64(len(img)))
	if err != nil {
		t.Fatal(err)
	}
	if err := v.Sync(); err != nil {
		t.Errorf("Sync on a read-only volume: %v", err)
	}
	if err := v.Close(); err != nil {
		t.Errorf("Close on a read-only volume: %v", err)
	}
}
