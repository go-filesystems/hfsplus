// Copyright (c) 2026, go-filesystems
// SPDX-License-Identifier: BSD-3-Clause

package hfsplus

import (
	"bytes"
	"encoding/binary"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// Crash and failure tests for go-filesystems/hfsplus#33/#34: the harness can fail
// a write or an fsync at any index, and replay the log of what reached the disk
// under two crash models.

// walkVolume walks the catalog from the root and returns the number of files and
// folders (the root excluded) and the greatest CNID, or an error on a duplicate
// CNID or an unreadable directory.
func walkVolume(v *Volume) (files, folders int, maxCNID uint32, err error) {
	seen := map[uint32]bool{cnidRootFolder: true}
	var visit func(id uint32) error
	visit = func(id uint32) error {
		kids, err := v.catalogTree.listChildren(id)
		if err != nil {
			return err
		}
		for _, k := range kids {
			var cnid uint32
			switch {
			case k.rec.folder != nil:
				cnid = k.rec.folder.folderID
				folders++
			case k.rec.file != nil:
				cnid = k.rec.file.fileID
				files++
			default:
				continue
			}
			if seen[cnid] {
				return fmt.Errorf("CNID %d appears twice", cnid)
			}
			seen[cnid] = true
			maxCNID = max(maxCNID, cnid)
			if k.rec.folder != nil {
				if err := visit(cnid); err != nil {
					return err
				}
			}
		}
		return nil
	}
	err = visit(cnidRootFolder)
	return
}

// volumeCounts reads the counters a consistent volume must keep in step with its
// catalog: the header's file and folder counts, next CNID and freeBlocks.
func headerCounts(raw []byte) (files, folders, next, free uint32) {
	h := raw[volumeHeaderOffset:]
	return binary.BigEndian.Uint32(h[0x20:]), binary.BigEndian.Uint32(h[0x24:]),
		binary.BigEndian.Uint32(h[0x40:]), binary.BigEndian.Uint32(h[0x30:])
}

// consistency is what a volume that claims to be cleanly unmounted must satisfy.
type consistency struct{ fileOff, folderOff int }

// baseline records how the header counts relate to the walk on a pristine image.
func baseline(t *testing.T, path string) consistency {
	t.Helper()
	raw, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	r, err := OpenFile(path)
	if err != nil {
		t.Fatal(err)
	}
	defer r.Close()
	f, d, _, err := walkVolume(r)
	if err != nil {
		t.Fatal(err)
	}
	hf, hd, _, _ := headerCounts(raw)
	return consistency{fileOff: int(hf) - f, folderOff: int(hd) - d}
}

// check returns "" when the volume at path is consistent: it opens, its
// catalog walks without a repeated CNID, the header counts match the walk, nextCNID
// is above every CNID in use and freeBlocks equals the bitmap's free bits.
func (c consistency) check(path string) string {
	raw, err := os.ReadFile(path)
	if err != nil {
		return err.Error()
	}
	r, err := OpenFile(path)
	if err != nil {
		return "does not open: " + err.Error()
	}
	defer r.Close()
	f, d, maxCNID, err := walkVolume(r)
	if err != nil {
		return "catalog walk: " + err.Error()
	}
	hf, hd, next, free := headerCounts(raw)
	switch {
	case int(hf)-c.fileOff != f:
		return fmt.Sprintf("fileCount %d, catalog holds %d", int(hf)-c.fileOff, f)
	case int(hd)-c.folderOff != d:
		return fmt.Sprintf("folderCount %d, catalog holds %d", int(hd)-c.folderOff, d)
	case next <= maxCNID:
		return fmt.Sprintf("nextCatalogID %d, but CNID %d is in use", next, maxCNID)
	}
	w, err := OpenFileWritable(path)
	if err != nil {
		return "does not open for writing: " + err.Error()
	}
	defer w.Close()
	w.reconcileFreeBlocks()
	if w.vh.FreeBlocks != free {
		return fmt.Sprintf("freeBlocks %d, bitmap has %d free", free, w.vh.FreeBlocks)
	}
	return ""
}

type crashOp struct {
	name string
	run  func(v *Volume) error
}

var crashOps = []crashOp{
	{"MkDir", func(v *Volume) error { return v.MkDir("/d", 0o755) }},
	{"WriteFile", func(v *Volume) error { return v.WriteFile("/f.bin", bytes.Repeat([]byte{7}, 5000), 0o644) }},
}

// TestInPlaceCrashModel replays the log of a mutation under two crash models --
// only the writes an fsync has made durable survive, and every write up to the
// crash point survives, in order -- and requires that at every crash point a
// volume still marked cleanly unmounted is consistent.
func TestInPlaceCrashModel(t *testing.T) {
	p := sparseImage(t, 8<<20)
	orig, err := os.ReadFile(p)
	if err != nil {
		t.Fatal(err)
	}
	base := baseline(t, p)
	if msg := base.check(p); msg != "" {
		t.Fatalf("the pristine image is not consistent: %s", msg)
	}
	for _, op := range crashOps {
		t.Run(op.name, func(t *testing.T) {
			if err := os.WriteFile(p, orig, 0o644); err != nil {
				t.Fatal(err)
			}
			v, err := OpenFileWritable(p)
			if err != nil {
				t.Fatal(err)
			}
			c := instrument(v)
			if err := op.run(v); err != nil {
				t.Fatal(err)
			}
			v.Close()
			events := c.events
			crash := filepath.Join(t.TempDir(), "crash.img")
			for _, model := range []string{"fsynced-only", "in-order"} {
				clean, dirty := 0, 0
				for k := 0; k <= len(events); k++ {
					upto := k
					if model == "fsynced-only" {
						upto = 0
						for i := 0; i < k; i++ {
							if events[i].sync {
								upto = i + 1
							}
						}
					}
					img := bytes.Clone(orig)
					for _, e := range events[:upto] {
						if !e.sync {
							copy(img[e.off:], e.data)
						}
					}
					if err := os.WriteFile(crash, img, 0o644); err != nil {
						t.Fatal(err)
					}
					if attrsOf(t, crash)&kHFSVolumeUnmountedBit == 0 {
						dirty++
						continue
					}
					clean++
					if msg := base.check(crash); msg != "" {
						t.Errorf("%s, crash after event %d of %d: marked cleanly unmounted but %s", model, k, len(events), msg)
					}
				}
				// Not vacuous: the start and the end are clean, the middle is not.
				if clean < 2 || dirty < 1 {
					t.Errorf("%s: %d crash points marked clean, %d marked dirty", model, clean, dirty)
				}
			}
		})
	}
}

// TestInPlaceFailureThenMutate: after a write or an fsync fails, at every index
// of the sequence, a further mutation is refused with ErrVolumeFailed and writes
// nothing -- it must not mark a half-written volume cleanly unmounted -- and the
// volume on disk is marked as not cleanly unmounted once anything but the first
// write failed.
func TestInPlaceFailureThenMutate(t *testing.T) {
	p := sparseImage(t, 8<<20)
	orig, err := os.ReadFile(p)
	if err != nil {
		t.Fatal(err)
	}
	base := baseline(t, p)
	for _, op := range crashOps {
		probe, err := OpenFileWritable(p)
		if err != nil {
			t.Fatal(err)
		}
		pc := instrument(probe)
		if err := op.run(probe); err != nil {
			t.Fatal(err)
		}
		writes, syncs := len(pc.writes), pc.syncs
		// The file's data comes before the first header write.
		dataWrites := 0
		for dataWrites < writes && pc.writes[dataWrites].off != volumeHeaderOffset {
			dataWrites++
		}
		probe.Close()
		for _, kind := range []string{"write", "fsync"} {
			n := writes
			if kind == "fsync" {
				n = syncs
			}
			for i := 0; i < n; i++ {
				name := fmt.Sprintf("%s/%s-%d", op.name, kind, i)
				if err := os.WriteFile(p, orig, 0o644); err != nil {
					t.Fatal(err)
				}
				v, err := OpenFileWritable(p)
				if err != nil {
					t.Fatal(err)
				}
				c := instrument(v)
				if kind == "write" {
					c.failAt = i
				} else {
					c.syncFailAt = i
				}
				if err := op.run(v); !errors.Is(err, errInjected) {
					t.Fatalf("%s: err = %v", name, err)
				}
				c.failAt, c.syncFailAt = -1, -1
				if kind == "write" && i < dataWrites {
					// The failure hit file data, before anything of the volume was
					// touched: nothing is latched, the next mutation works and
					// leaves a consistent volume.
					if err := v.MkDir("/e", 0o755); err != nil {
						t.Errorf("%s: a mutation after a failed data write: %v", name, err)
					}
					v.Close()
					if msg := base.check(p); msg != "" && !strings.Contains(msg, "folderCount") {
						t.Errorf("%s: %s", name, msg)
					}
					continue
				}
				before, _ := os.ReadFile(p)
				for _, again := range []func() error{
					func() error { return v.MkDir("/e", 0o755) },
					func() error { return v.WriteFile("/g", []byte("y"), 0o644) },
					func() error { return v.DeleteDir("/e") },
					func() error { return v.Chtimes("/", time.Now(), time.Now()) },
				} {
					if err := again(); !errors.Is(err, ErrVolumeFailed) {
						t.Errorf("%s: a mutation after the failure returned %v, want ErrVolumeFailed", name, err)
					}
				}
				v.Close()
				after, _ := os.ReadFile(p)
				if !bytes.Equal(before, after) {
					t.Errorf("%s: a refused mutation changed the image", name)
				}
				// The bit is clear unless nothing was written yet (the write that clears
				// it failed) or the primary header, bit set, was the last thing written
				// (its fsync failed).
				wrote := !(kind == "write" && i == dataWrites) && !(kind == "fsync" && i == syncs-1)
				if wrote && attrsOf(t, p)&kHFSVolumeUnmountedBit != 0 {
					t.Errorf("%s: the volume is marked cleanly unmounted", name)
				}
			}
		}
	}
}

// TestInPlaceFreeBlocksReconciled: a wrong freeBlocks in the header is put right
// by the first write, because the allocator counts from the bitmap at the first
// write instead of trusting the header.
func TestInPlaceFreeBlocksReconciled(t *testing.T) {
	p := sparseImage(t, 8<<20)
	f, err := os.OpenFile(p, os.O_RDWR, 0)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := f.WriteAt([]byte{0, 0, 0, 1}, volumeHeaderOffset+0x30); err != nil {
		t.Fatal(err)
	}
	f.Close()
	v, err := OpenFileWritable(p)
	if err != nil {
		t.Fatal(err)
	}
	if err := v.MkDir("/z", 0o755); err != nil {
		t.Fatal(err)
	}
	v.Close()
	w, err := OpenFileWritable(p)
	if err != nil {
		t.Fatal(err)
	}
	defer w.Close()
	have := w.vh.FreeBlocks
	w.reconcileFreeBlocks()
	if have != w.vh.FreeBlocks || have == 1 {
		t.Errorf("freeBlocks = %d after the write, the bitmap has %d free", have, w.vh.FreeBlocks)
	}
}
