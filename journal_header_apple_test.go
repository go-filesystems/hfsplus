package hfsplus

import (
	"encoding/binary"
	"errors"
	"strings"
	"testing"
)

// The three checks hfsplus#48 relaxed: each header below is one Apple's journal_open
// accepts (core/hfs_journal.c, hfs-715.100.10), and each case is red against the
// stricter reader.

func legacyJournalHeader(bo binary.ByteOrder, mutate func(h []byte)) []byte {
	h := buildJournalHeader(bo, 0x4400, 0x4400, 0x80000, false)
	bo.PutUint32(h[0:], journalOldHeaderMagic)
	if mutate != nil {
		mutate(h)
	}
	return h
}

func TestJournalHeaderOldMagicAndEndianField(t *testing.T) {
	for _, o := range []struct {
		name string
		bo   binary.ByteOrder
	}{{"little", binary.LittleEndian}, {"big", binary.BigEndian}} {
		bo := o.bo
		cases := []struct {
			name string
			h    []byte
			ok   bool
		}{
			// 'JHDR' as found (little-endian) only: a swapped one fails Apple's
			// journal_open at :1979 (the security review of v0.19.0..main).
			{"JHDR", legacyJournalHeader(bo, nil), bo == binary.LittleEndian},
			{"JHDR, endian field zero", legacyJournalHeader(bo, func(h []byte) { bo.PutUint32(h[4:], 0) }), bo == binary.LittleEndian},
			{"JHDR, no checksum", legacyJournalHeader(bo, func(h []byte) { bo.PutUint32(h[36:], 0) }), bo == binary.LittleEndian},
			{"JNLx, endian field zero", func() []byte {
				h := buildJournalHeader(bo, 0x4400, 0x4400, 0x80000, false)
				bo.PutUint32(h[4:], 0)
				resignJournalHeader(h)
				return h
			}(), true},
			{"JNLx, endian field of the other order", func() []byte {
				h := buildJournalHeader(bo, 0x4400, 0x4400, 0x80000, false)
				bo.PutUint32(h[4:], 0x78563412)
				resignJournalHeader(h)
				return h
			}(), true},
			// This reader checks the 'JNLx' checksum; Apple's check does not refuse
			// (hfs_journal.c:1988-1993), so this is stricter than Apple.
			{"JNLx, bad checksum", buildJournalHeader(bo, 0x4400, 0x4400, 0x80000, true), false},
			{"unknown magic", legacyJournalHeader(bo, func(h []byte) { bo.PutUint32(h[0:], 0x4a484453) }), false},
		}
		for _, c := range cases {
			t.Run(o.name+"/"+c.name, func(t *testing.T) {
				jh, err := decodeJournalHeader(c.h)
				if !c.ok {
					if !errors.Is(err, ErrJournaled) {
						t.Fatalf("err = %v, want ErrJournaled", err)
					}
					return
				}
				if err != nil {
					t.Fatalf("decode: %v", err)
				}
				if jh.bo != bo || jh.start != 0x4400 || jh.end != 0x4400 || jh.size != 0x80000 {
					t.Errorf("decoded %+v, want order %v and 0x4400/0x4400/0x80000", jh, bo)
				}
			})
		}
	}
}

// A volume whose journal header carries the old magic opens writable (the reader
// no longer refuses it), and the writer leaves a 'JNLx' header with a valid
// checksum and the endian field, in the journal's byte order.
func TestJournalOldMagicOpensAndIsRewritten(t *testing.T) {
	// Little-endian only: a swapped 'JHDR' is refused (TestJournalSwappedOldMagicIsRefused).
	for _, bo := range []binary.ByteOrder{binary.LittleEndian} {
		h := legacyJournalHeader(bo, func(h []byte) { bo.PutUint32(h[4:], 0) })
		img, _, jBlock := journaledImage(t, kJIJournalInFSMask, h, 0x80000)
		v, err := OpenWritable(img, nil)
		if err != nil {
			t.Fatalf("%v: OpenWritable: %v", bo, err)
		}
		if err := v.EnableJournalWrites(); err != nil {
			t.Fatalf("%v: EnableJournalWrites: %v", bo, err)
		}
		if err := v.MkDir("/a", 0o755); err != nil {
			t.Fatal(err)
		}
		jbase := int64(jBlock) * 4096
		if got := bo.Uint32(img[jbase:]); got != journalHeaderMagic {
			t.Errorf("%v: magic %#x after a commit, want JNLx", bo, got)
		}
		if got := bo.Uint32(img[jbase+4:]); got != journalEndianMagic {
			t.Errorf("%v: endian field %#x after a commit", bo, got)
		}
		if _, err := decodeJournalHeader(img[jbase : jbase+512]); err != nil {
			t.Errorf("%v: the header the writer left does not verify: %v", bo, err)
		}
	}
}

// The journal size in the header and in the journal info block need not agree
// for a reader (Apple compares nothing); the writer refuses only a ring larger
// than the extent the info block declares.
func TestJournalSizeAgainstInfoBlock(t *testing.T) {
	const size = 0x80000
	empty := buildJournalHeader(binary.LittleEndian, 0x4400, 0x4400, size, false)
	for _, c := range []struct {
		name    string
		jibSize uint64
		want    string // "" = the writer is enabled
	}{
		{"info block larger", size + 4096, ""},
		{"equal", size, ""},
		{"info block smaller", size - 4096, "exceeds the journal info block"},
	} {
		t.Run(c.name, func(t *testing.T) {
			img, _, _ := journaledImage(t, kJIJournalInFSMask, empty, c.jibSize)
			v, err := OpenWritable(img, nil)
			if err != nil {
				t.Fatalf("OpenWritable: %v", err)
			}
			err = v.EnableJournalWrites()
			if c.want == "" {
				if err != nil {
					t.Fatalf("EnableJournalWrites: %v", err)
				}
				return
			}
			if !errors.Is(err, ErrJournalWritesUnavailable) || !strings.Contains(err.Error(), c.want) {
				t.Fatalf("err = %v, want ErrJournalWritesUnavailable mentioning %q", err, c.want)
			}
			if err := v.MkDir("/a", 0o755); err != nil {
				t.Errorf("MkDir after the refusal: %v", err)
			}
		})
	}
}

// A header size that runs past the end of the volume is refused by the reader
// (the one bound the dropped comparison used to give).
func TestJournalHeaderSizePastTheVolume(t *testing.T) {
	h := buildJournalHeader(binary.LittleEndian, 0x4400, 0x4400, 1<<29, false)
	img, _, _ := journaledImage(t, kJIJournalInFSMask, h, 0x80000)
	_, err := OpenWritable(img, nil)
	if !errors.Is(err, ErrJournaled) || !strings.Contains(err.Error(), "outside the volume") {
		t.Fatalf("err = %v, want ErrJournaled outside the volume", err)
	}
}
