// Copyright (c) 2026, go-filesystems
// SPDX-License-Identifier: BSD-3-Clause

package hfsplus

import (
	"bytes"
	"compress/gzip"
	"crypto/sha256"
	"encoding/hex"
	"io"
	"os"
	"slices"
	"testing"
	"time"
)

// What the volume looks like after the same operations as v0.17.0 wrote it. The
// digests in goldenV017 were produced by a clean clone of the v0.17.0 tag running
// this file over the base images in testdata (golden values empty, -v). Mkfs stamps
// the wall clock, so the bases are committed, not rebuilt: they were written once
// by this test with HFSPLUS_GOLDEN_WRITE_BASE=1. A volume that cannot journal (not
// journaled, or a writer without Sync) must keep writing exactly as v0.17.0 did, at
// every level, so PR 6's default changes nothing for it.
//
// Only the public API is used, so the same file compiles in the v0.17.0 clone.

var goldenV017 = map[string]string{
	"plain/strict":              "3ea3445bce967691d2631962413d2dcd0454e261a7ddf19c4193a18e5c934d44",
	"plain/mounted":             "3ea3445bce967691d2631962413d2dcd0454e261a7ddf19c4193a18e5c934d44",
	"plain/lazy":                "3ea3445bce967691d2631962413d2dcd0454e261a7ddf19c4193a18e5c934d44",
	"journaled-no-sync/strict":  "b2a1279713afe3aa029966dec7819ae907bf18fdb15a5211fd26828849648633",
	"journaled-no-sync/mounted": "22d71b5f256c7a13ce1f68416c6bf707eb65765a1d262272b2a8889d9c94e9d5",
	"journaled-no-sync/lazy":    "22d71b5f256c7a13ce1f68416c6bf707eb65765a1d262272b2a8889d9c94e9d5",

	// Batch, digests of a clean checkout of v0.18.0 (origin/main 814dd8d).
	"plain/strict-batch":              "3ea3445bce967691d2631962413d2dcd0454e261a7ddf19c4193a18e5c934d44",
	"plain/mounted-batch":             "3ea3445bce967691d2631962413d2dcd0454e261a7ddf19c4193a18e5c934d44",
	"plain/lazy-batch":                "3ea3445bce967691d2631962413d2dcd0454e261a7ddf19c4193a18e5c934d44",
	"journaled-no-sync/strict-batch":  "22d71b5f256c7a13ce1f68416c6bf707eb65765a1d262272b2a8889d9c94e9d5",
	"journaled-no-sync/mounted-batch": "22d71b5f256c7a13ce1f68416c6bf707eb65765a1d262272b2a8889d9c94e9d5",
	"journaled-no-sync/lazy-batch":    "22d71b5f256c7a13ce1f68416c6bf707eb65765a1d262272b2a8889d9c94e9d5",
}

// goldenBase returns the committed base image of a kind.
func goldenBase(t *testing.T, kind string) []byte {
	t.Helper()
	path := "testdata/golden-v017-" + kind + ".img.gz"
	if os.Getenv("HFSPLUS_GOLDEN_WRITE_BASE") == "1" {
		var img []byte
		switch kind {
		case "plain":
			var err error
			if img, err = Mkfs(8<<20, FormatConfig{Label: "PLAIN"}); err != nil {
				t.Fatal(err)
			}
		default:
			img, _ = journalImage(t, 0x80000, 1000, 0x4400)
		}
		var buf bytes.Buffer
		zw := gzip.NewWriter(&buf)
		zw.Write(img)
		zw.Close()
		if err := os.MkdirAll("testdata", 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(path, buf.Bytes(), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	f, err := os.Open(path)
	if err != nil {
		t.Fatal(err)
	}
	defer f.Close()
	zr, err := gzip.NewReader(f)
	if err != nil {
		t.Fatal(err)
	}
	img, err := io.ReadAll(zr)
	if err != nil {
		t.Fatal(err)
	}
	return img
}

func goldenRun(t *testing.T, kind string, d Durability, batch bool) string {
	t.Helper()
	fixed := time.Date(2026, 1, 2, 3, 4, 5, 0, time.UTC)
	timeNow = func() time.Time { return fixed }
	t.Cleanup(func() { timeNow = time.Now })
	img := goldenBase(t, kind)
	var v *Volume
	var out func() []byte
	var err error
	switch kind {
	case "plain":
		if v, err = OpenWritable(img, nil); err != nil {
			t.Fatal(err)
		}
		out = v.Bytes
	default:
		mem := &memWriter{b: slices.Clone(img)}
		if v, err = OpenWritable(img, onlyWriter{w: mem}); err != nil {
			t.Fatal(err)
		}
		out = func() []byte { return mem.b }
	}
	if err := v.SetDurability(d); err != nil {
		t.Fatal(err)
	}
	ops := func() error {
		for _, n := range []string{"/a", "/b"} {
			if err := v.MkDir(n, 0o755); err != nil {
				return err
			}
		}
		return v.WriteFile("/f", fill(9000, 3), 0o644)
	}
	if batch {
		err = v.Batch(ops)
	} else {
		err = ops()
	}
	if err != nil {
		t.Fatal(err)
	}
	if err := v.Close(); err != nil {
		t.Fatal(err)
	}
	sum := sha256.Sum256(out())
	return hex.EncodeToString(sum[:])
}

func TestWritesMatchV017Golden(t *testing.T) {
	for _, kind := range []string{"plain", "journaled-no-sync"} {
		for _, d := range allDurabilities {
			for _, batch := range []bool{false, true} {
				key := kind + "/" + d.String()
				if batch {
					key += "-batch" // Batch from level d: the same writes inside Lazy, then a Sync
				}
				t.Run(key, func(t *testing.T) {
					got := goldenRun(t, kind, d, batch)
					t.Logf("GOLDEN %q: %q,", key, got)
					if want := goldenV017[key]; got != want {
						t.Errorf("image digest %s, v0.18.0 wrote %q", got, want)
					}
				})
			}
		}
	}
}
