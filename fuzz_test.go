// Copyright (c) 2026, go-filesystems
// SPDX-License-Identifier: BSD-3-Clause

package hfsplus

import (
	"bytes"
	"io"
	"sync"
	"testing"
)

// The read path takes bytes from an untrusted disk and walks three interlocking
// on-disk structures -- the volume header, two B-trees, and the extent lists
// they point at. Every count and offset in there is attacker-controlled, and a
// hostile image must produce an error rather than a panic or an allocation
// sized from a number that was read off the disk.
//
// This was the one driver in the family with no fuzz target at all.
//
// TWO targets, because one is not enough and the reason matters. Fuzzing whole
// images (FuzzOpenHeader) never gets past the volume header: measured over a
// 60-second run, the only input that ever reached the B-tree code was the
// pristine seed itself, out of 24 million executions. The signature and
// geometry checks reject everything else at once. So the tree code gets its own
// target (FuzzOpenPatched), which splices fuzzer bytes into a *valid* image and
// therefore starts where the header parser leaves off.

var (
	seedOnce  sync.Once
	seedImage []byte
)

// fuzzSeed returns a genuinely valid volume -- with a directory, a file
// spanning several blocks, and a symlink -- so the walk has something to walk.
// Built once: it is read-only for the fuzz targets.
func fuzzSeed(t testing.TB) []byte {
	t.Helper()
	seedOnce.Do(func() {
		img, err := Mkfs(4<<20, FormatConfig{Label: "FUZZ"})
		if err != nil {
			return
		}
		v, err := OpenWritable(img, nil)
		if err != nil {
			return
		}
		if err := v.MkDir("/dir", 0o755); err != nil {
			return
		}
		if err := v.WriteFile("/dir/big", bytes.Repeat([]byte("hfs+"), 3000), 0o644); err != nil {
			return
		}
		if err := v.Symlink("/dir/big", "/link"); err != nil {
			return
		}
		seedImage = v.Bytes()
	})
	if seedImage == nil {
		t.Fatal("could not build the seed volume")
	}
	return seedImage
}

// patched is a read-only overlay of patch onto base at off. It exists so a fuzz
// iteration costs no copy of the multi-megabyte seed image.
type patched struct {
	base, patch []byte
	off         int64
}

func (p *patched) ReadAt(b []byte, off int64) (int, error) {
	if off < 0 {
		return 0, io.EOF
	}
	if off >= int64(len(p.base)) {
		return 0, io.EOF
	}
	n := copy(b, p.base[off:])
	// Splice in whatever part of the patch falls inside [off, off+n).
	lo, hi := p.off, p.off+int64(len(p.patch))
	if s, e := max64(off, lo), min64(off+int64(n), hi); s < e {
		copy(b[s-off:e-off], p.patch[s-p.off:e-p.off])
	}
	if n < len(b) {
		return n, io.EOF
	}
	return n, nil
}

func max64(a, b int64) int64 {
	if a > b {
		return a
	}
	return b
}

func min64(a, b int64) int64 {
	if a < b {
		return a
	}
	return b
}

// walk exercises the whole read path: the catalog B-tree, the extents overflow
// tree, and the fork reader. Errors are expected and ignored -- the property
// under test is that the driver returns rather than panicking, hanging, or
// allocating from a corrupted count.
func walk(v *Volume, dir string, depth int) {
	if depth > 8 {
		return
	}
	ents, err := v.ListDir(dir)
	if err != nil {
		return
	}
	for i, e := range ents {
		if i >= 64 {
			return
		}
		p := dir + "/" + e.Name()
		if dir == "/" {
			p = "/" + e.Name()
		}
		_, _ = v.Stat(p)
		if e.FileType() == ftDir {
			walk(v, p, depth+1)
			continue
		}
		_, _ = v.ReadFile(p)
		_, _ = v.ReadLink(p)
	}
}

func exercise(v *Volume) {
	defer func() { _ = v.Close() }()
	_ = v.Label()
	_ = v.CaseSensitive()
	walk(v, "/", 0)
}

// FuzzOpenHeader covers readVolumeHeader: the signature, version, block size
// and geometry checks that decide whether an image is an HFS+ volume at all.
// It is not expected to reach the B-tree code -- see FuzzOpenPatched for that.
func FuzzOpenHeader(f *testing.F) {
	seed := fuzzSeed(f)
	if len(seed) > 2048 {
		f.Add(append([]byte(nil), seed[:2048]...))
	}
	f.Add([]byte{})
	f.Add(bytes.Repeat([]byte{0xff}, 1024))
	f.Add(bytes.Repeat([]byte{0x00}, 2048))

	f.Fuzz(func(t *testing.T, img []byte) {
		v, err := Open(bytes.NewReader(img), int64(len(img)))
		if err != nil {
			return
		}
		exercise(v)
	})
}

// FuzzOpenPatched splices fuzzer-chosen bytes into a valid volume, so the
// corrupted fields are reached by code that has already accepted the header:
// the B-tree node headers, record offsets, key lengths, and fork extents.
func FuzzOpenPatched(f *testing.F) {
	base := fuzzSeed(f)

	// Seeds aimed at the structures worth corrupting: the volume header at
	// 1024, and the first nodes of the extents and catalog trees.
	vh := int64(1024)
	f.Add(vh+44, []byte{0xff, 0xff, 0xff, 0xff})         // totalBlocks
	f.Add(vh+48, []byte{0xff, 0xff, 0xff, 0xff})         // freeBlocks
	f.Add(vh+112, bytes.Repeat([]byte{0xff}, 80))        // the special-file fork descriptors
	f.Add(int64(3*4096), bytes.Repeat([]byte{0xff}, 32)) // extents tree header node
	f.Add(int64(5*4096), bytes.Repeat([]byte{0xff}, 32)) // catalog tree header node
	f.Add(int64(6*4096), bytes.Repeat([]byte{0xff}, 64)) // catalog leaf node

	f.Fuzz(func(t *testing.T, off int64, patch []byte) {
		if len(patch) == 0 || len(patch) > 8192 {
			return
		}
		if off < 0 || off >= int64(len(base)) {
			return
		}
		v, err := Open(&patched{base: base, patch: patch, off: off}, int64(len(base)))
		if err != nil {
			return
		}
		exercise(v)
	})
}
