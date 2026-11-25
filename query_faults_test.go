// Copyright (c) 2026, go-filesystems
// SPDX-License-Identifier: BSD-3-Clause

package hfsplus

import (
	"bytes"
	"encoding/binary"
	"fmt"
	"slices"
	"testing"
)

// fragmentedImage is a volume holding /d, /d/f ("payload"), the symlink /s and
// /frag.bin, a file whose data fork spilled into the extents-overflow tree (so that
// tree is not empty, and reading the file resolves extents through it).
func fragmentedImage(t *testing.T) (img, frag []byte) {
	t.Helper()
	v := openMem(t, 4<<20, FormatConfig{Label: "QFAULT"})
	if err := v.MkDir("/d", 0o755); err != nil {
		t.Fatal(err)
	}
	if err := v.WriteFile("/d/f", []byte("payload"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := v.Symlink("/d/f", "/s"); err != nil {
		t.Fatal(err)
	}
	holes := fragmentFreeSpace(t, v, int(v.vh.TotalBlocks)+10)
	nblk := numInlineExtents + 8
	if holes < nblk*3 {
		t.Fatalf("not enough holes (%d) for a %d-block file", holes, nblk)
	}
	frag = bytes.Repeat([]byte("fragment"), nblk*int(v.vh.BlockSize)/8)
	if err := v.WriteFile("/frag.bin", frag, 0o644); err != nil {
		t.Fatal(err)
	}
	r, err := v.lookupPath("/frag.bin")
	if err != nil {
		t.Fatal(err)
	}
	if r.rec.file.dataFork.TotalBlocks <= 0 || !extentsForkNeedsOverflow(r.rec.file.dataFork) {
		t.Fatal("/frag.bin did not spill into the extents-overflow tree")
	}
	img = bytes.Clone(v.Bytes())
	if err := v.Close(); err != nil {
		t.Fatal(err)
	}
	return img, frag
}

// openNoPanic is Open with a panic turned into an assertion: a guard that is gone
// would otherwise end the whole test binary, which a mutation run reads as weak.
func openNoPanic(t *testing.T, r *failingReaderAt, size int64) (v *Volume, err error) {
	t.Helper()
	defer func() {
		if p := recover(); p != nil {
			t.Errorf("Open panicked at read %d: %v", r.n, p)
			v, err = nil, fmt.Errorf("panic: %v", p)
		}
	}()
	return Open(r, size)
}

// A special file whose volume header claims more blocks than its inline extents
// cover is resolved through the extents-overflow tree. A read that fails there is an
// error of Open (hfsplus#55: openTrees' returns after newSpecialForkResolved survived).
func TestOpenOverflowResolveFaults(t *testing.T) {
	for _, tc := range []struct {
		name string
		off  int64 // of the fork's totalBlocks inside the volume header
	}{
		{"catalog", 272 + 12},
		{"extents", 192 + 12},
	} {
		t.Run(tc.name, func(t *testing.T) {
			img, _ := fragmentedImage(t)
			for _, h := range []int64{volumeHeaderOffset, int64(len(img)) - 1024} {
				p := img[h+tc.off:]
				binary.BigEndian.PutUint32(p, binary.BigEndian.Uint32(p)+1)
			}
			count := &failingReaderAt{img: img}
			pristine, err := openNoPanic(t, count, int64(len(img)))
			if err != nil {
				t.Fatalf("the unfaulted open: %v", err)
			}
			want, err := rootNames(pristine)
			if err != nil {
				t.Fatal(err)
			}
			total := count.reads.Load()
			refused := 0
			for n := int64(1); n <= total; n++ {
				f := &failingReaderAt{img: img, n: n}
				v, err := openNoPanic(t, f, int64(len(img)))
				if err != nil {
					refused++
					continue
				}
				// The fault may fall in the listing, after Open: an error there is fine.
				if got, err := rootNames(v); err == nil && !slices.Equal(got, want) {
					t.Errorf("read %d fails: Open succeeded but the root is %v; want %v", n, got, want)
				}
			}
			if refused == 0 {
				t.Error("no failed read made Open fail")
			}
		})
	}
}

// The read-only queries each fail once per read, in turn (mutsweep, hfsplus#55: the
// error returns of lookupPath, ListDir, ReadFile and ReadLink survived because no test
// failed a read there). A query returns an error or the answer the pristine volume
// gives: never a different answer built from a read that failed.
func TestQueryReadFaults(t *testing.T) {
	img, frag := fragmentedImage(t)

	ops := map[string]func(v *Volume) (string, error){
		"ListDir": func(v *Volume) (string, error) {
			es, err := v.ListDir("/")
			return fmt.Sprint(len(es)), err
		},
		"Stat": func(v *Volume) (string, error) {
			st, err := v.Stat("/d/f")
			if err != nil {
				return "", err
			}
			return fmt.Sprint(st.Size(), st.Mode()), nil
		},
		"ReadFile": func(v *Volume) (string, error) {
			b, err := v.ReadFile("/d/f")
			return string(b), err
		},
		"ReadFragmented": func(v *Volume) (string, error) {
			b, err := v.ReadFile("/frag.bin")
			if err == nil && !bytes.Equal(b, frag) {
				return "", fmt.Errorf("wrong bytes: %d, want %d", len(b), len(frag))
			}
			return fmt.Sprint(len(b)), err
		},
		"ReadLink": func(v *Volume) (string, error) {
			return v.ReadLink("/s")
		},
	}
	for name, op := range ops {
		t.Run(name, func(t *testing.T) {
			pristine, err := Open(bytesReaderAt(img), int64(len(img)))
			if err != nil {
				t.Fatal(err)
			}
			want, err := op(pristine)
			if err != nil {
				t.Fatal(err)
			}
			// Count the reads the op makes after Open.
			c := &failingReaderAt{img: img}
			v, err := Open(c, int64(len(img)))
			if err != nil {
				t.Fatal(err)
			}
			base := c.reads.Load()
			if got, err := op(v); err != nil || got != want {
				t.Fatalf("the op on the counting reader: %q, %v; want %q", got, err, want)
			}
			total := c.reads.Load() - base
			if total == 0 {
				t.Fatal("the op made no reads: the sweep would be vacuous")
			}
			failed := 0
			for n := int64(1); n <= total; n++ {
				f := &failingReaderAt{img: img}
				v, err := Open(f, int64(len(img)))
				if err != nil {
					t.Fatal(err)
				}
				f.n = f.reads.Load() + n
				got, err := op(v)
				if err != nil {
					failed++
					continue
				}
				if got != want {
					t.Errorf("read %d of %s fails: answered %q without an error; want %q", n, name, got, want)
				}
			}
			if failed == 0 {
				t.Error("no failed read made the op fail")
			}
		})
	}
}
