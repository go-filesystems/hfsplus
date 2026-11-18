// Copyright (c) 2026, go-filesystems
// SPDX-License-Identifier: BSD-3-Clause

package hfsplus

import (
	"bytes"
	"encoding/binary"
	"errors"
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// journalImageWith returns a journaled image (journal in the HFSJ state, like
// journalWithTxn) whose journal holds one transaction made of the blocks blks(img)
// returns, extra zero bytes appended after the volume.
func journalImageWith(t testing.TB, extra int, blks func(img []byte, jibBlock uint32) []blk) []byte {
	t.Helper()
	const size = 0x80000
	h := buildJournalHeader(binary.LittleEndian, 512, 512, size, false)
	img, jibBlock, jBlock := journaledImage(t, kJIJournalInFSMask, h, size)
	img = append(img, make([]byte, extra)...)
	jbase := uint64(jBlock) * 4096
	b := &jbuilder{bo: binary.LittleEndian, size: size, jh: 512, bl: 0x2000, buf: img[jbase : jbase+size]}
	end := b.txn(512, 1, first, blks(img, jibBlock)...)
	copy(img[jbase:], buildJournalHeader(binary.LittleEndian, 512, end, size, false))
	return img
}

// A block past totalBlocks*blockSize but inside the image is replayed: an
// interrupted hfs_extendfs journals the new alternate header there, and the kernel
// and the read path bound blocks by the image, not by the old volume header.
func TestWritableOpenReplaysBlockPastVolumeEnd(t *testing.T) {
	var at int64
	data := fill(512, 0x6b)
	img := journalImageWith(t, 1<<20, func(img []byte, _ uint32) []blk {
		vh, _ := readVolumeHeader(bytesReaderAt(img))
		at = int64(vh.TotalBlocks)*int64(vh.BlockSize) + 4096
		return []blk{{at / 512, data}}
	})
	// The read path agrees.
	if _, err := Open(bytesReaderAt(img), int64(len(img)), WithJournalReplay()); err != nil {
		t.Fatalf("read path: %v", err)
	}
	v, err := OpenWritable(img, nil)
	if err != nil {
		t.Fatalf("OpenWritable: %v", err)
	}
	v.Close()
	if !bytes.Equal(img[at:at+512], data) {
		t.Error("the block past the old volume end was not replayed")
	}
	// Still bounded by the image: a block past ITS end is refused, nothing written.
	img = journalImageWith(t, 1<<20, func(img []byte, _ uint32) []blk {
		return []blk{{int64(len(img)) / 512, data}}
	})
	before := bytes.Clone(img)
	if _, err := OpenWritable(img, nil); !errors.Is(err, ErrJournalReplay) {
		t.Errorf("block past the image: %v, want ErrJournalReplay", err)
	}
	if !bytes.Equal(img, before) {
		t.Error("a refused replay changed the image")
	}
}

// onlyWriter has WriteAt and no Sync.
type onlyWriter struct{ w io.WriterAt }

func (o onlyWriter) WriteAt(p []byte, off int64) (int, error) { return o.w.WriteAt(p, off) }

// Without a barrier the order cannot be kept, so a journal is not replayed through
// a mirror that cannot Sync; with no mirror, or one that can, it is.
func TestWritableOpenReplayNeedsABarrier(t *testing.T) {
	img0, _ := journalWithTxn(t)
	var sink bytes.Buffer
	_ = sink
	f, err := os.CreateTemp(t.TempDir(), "m")
	if err != nil {
		t.Fatal(err)
	}
	defer f.Close()
	img := bytes.Clone(img0)
	if _, err := OpenWritable(img, onlyWriter{f}); !errors.Is(err, ErrJournaled) || !strings.Contains(err.Error(), "cannot Sync") {
		t.Errorf("writer without Sync: %v, want ErrJournaled naming Sync", err)
	}
	if !bytes.Equal(img, img0) {
		t.Error("the refused open changed the image")
	}
	if fi, _ := f.Stat(); fi.Size() != 0 {
		t.Error("the refused open wrote to the mirror")
	}
	for name, wa := range map[string]io.WriterAt{"no mirror": nil, "file": f} {
		img := bytes.Clone(img0)
		if v, err := OpenWritable(img, wa); err != nil {
			t.Errorf("%s: %v", name, err)
		} else {
			v.Close()
		}
	}
}

// The checks after the replay (OpenWritable and OpenWritableAt re-run checkJournal
// on what the replay left): a transaction that rewrites the journal info block so
// the journal no longer verifies is refused, with the blocks replayed and the
// journal header emptied (crash-safe order), not opened.
func TestWritableOpenRefusesJournalRewrittenByReplay(t *testing.T) {
	cases := []struct {
		name string
		mut  func(jib []byte)
		want string
	}{
		{"flags: not inside the volume", func(jib []byte) { binary.BigEndian.PutUint32(jib[0:], 0) }, "not inside the volume"},
		{"journal moved outside the volume", func(jib []byte) { binary.BigEndian.PutUint64(jib[36:], 1<<40) }, "outside the volume"},
		{"journal size changed", func(jib []byte) { binary.BigEndian.PutUint64(jib[44:], 0x40000) }, "differs"},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			var jibOff int64
			img0 := journalImageWith(t, 0, func(img []byte, jibBlock uint32) []blk {
				jibOff = int64(jibBlock) * 4096
				sec := bytes.Clone(img[jibOff : jibOff+512])
				c.mut(sec)
				return []blk{{jibOff / 512, sec}}
			})
			check := func(t *testing.T, img []byte, err error) {
				t.Helper()
				if !errors.Is(err, ErrJournaled) || !strings.Contains(err.Error(), c.want) {
					t.Fatalf("err = %v, want ErrJournaled mentioning %q", err, c.want)
				}
				if bytes.Equal(img[jibOff:jibOff+512], img0[jibOff:jibOff+512]) {
					t.Error("the replay did not write the journal info block: the refusal came from before it")
				}
				vh, _ := readVolumeHeader(bytesReaderAt(img))
				jbase := int64(vh.TotalBlocks-140) * int64(vh.BlockSize)
				h, err := decodeJournalHeader(img[jbase : jbase+512])
				if err != nil || h.start != h.end {
					t.Errorf("journal header after the refusal: %+v, %v; want it emptied", h, err)
				}
			}
			t.Run("OpenWritable", func(t *testing.T) {
				img := bytes.Clone(img0)
				v, err := OpenWritable(img, nil)
				if v != nil {
					t.Fatal("opened")
				}
				check(t, img, err)
			})
			t.Run("OpenWritableAt", func(t *testing.T) {
				path := filepath.Join(t.TempDir(), "r.img")
				if err := os.WriteFile(path, img0, 0o644); err != nil {
					t.Fatal(err)
				}
				f, err := os.OpenFile(path, os.O_RDWR, 0)
				if err != nil {
					t.Fatal(err)
				}
				defer f.Close()
				v, err := OpenWritableAt(f, int64(len(img0)))
				if v != nil {
					t.Fatal("opened")
				}
				onDisk, _ := os.ReadFile(path)
				check(t, onDisk, err)
			})
		})
	}
}
