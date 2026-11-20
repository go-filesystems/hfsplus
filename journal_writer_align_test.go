// Copyright (c) 2026, go-filesystems
// SPDX-License-Identifier: BSD-3-Clause

package hfsplus

import (
	"encoding/binary"
	"errors"
	"strings"
	"testing"
)

// The writer places transactions at h.end, so a start or end that is not a multiple
// of jhdr_size (inherited from the header) would put every block list header and
// block off the sector grid the forged-header guard checks. Open refuses such a
// journal; EnableJournalWrites checks it itself, for a header that changed after
// the open (the test rewrites the header sector in the image, re-signed).
func TestEnableJournalWritesRefusesMisalignedEnds(t *testing.T) {
	const size = 0x80000
	for _, off := range []uint64{0x4401, 0x43ff} {
		img, jbase := journalImage(t, size, 1000, 0x4400)
		v, err := OpenWritable(img, nil)
		if err != nil {
			t.Fatal(err)
		}
		h := buildJournalHeader(binary.LittleEndian, off, off, size, false)
		binary.LittleEndian.PutUint32(h[32:], 0x2000)
		binary.LittleEndian.PutUint32(h[44:], 1000)
		resignJournalHeader(h)
		copy(img[jbase:], h)
		err = v.EnableJournalWrites()
		if !errors.Is(err, ErrJournalWritesUnavailable) {
			t.Errorf("start = end = %#x: %v, want ErrJournalWritesUnavailable", off, err)
		}
		// loadJournalAt (512-aligned start and end) answers first, so the check in
		// EnableJournalWrites is a second line that no input reaches today.
		if err != nil && !strings.Contains(err.Error(), "512-aligned") {
			t.Errorf("start = end = %#x: reason %v", off, err)
		}
	}
}
