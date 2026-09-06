// SPDX-License-Identifier: BSD-3-Clause
//
// Copyright (c) 2026, the go-filesystems/hfsplus authors

package hfsplus

import (
	"fmt"
)

// The Finder's own 32 bytes, which every catalog record carries and which
// nothing here could read or write until now.
//
// HFS+ (Apple TN1150) puts them at the same place in both record kinds: a
// 16-byte userInfo at record offset 48 followed by a 16-byte finderInfo at 64.
// For a file those are FInfo/FXInfo, for a folder DInfo/DXInfo, and the two
// differ in what the first eight bytes mean — but the FLAGS word sits at +8 of
// userInfo either way, so it is addressable without knowing which kind a
// record is.
const (
	finderInfoOffset = 48 // userInfo, 16 bytes; finderInfo follows at 64
	finderInfoLen    = 32 // both halves together

	// FinderFlagsOffset is where the flags word lives inside the 32 bytes
	// FinderInfo returns: DInfo.frFlags for a folder, FInfo.fdFlags for a
	// file. Big-endian, like everything else in HFS+.
	FinderFlagsOffset = 8
)

// Finder flags worth naming. The rest are documented in TN1150 and in
// CarbonCore/Finder.h; these are the ones a disk-image builder needs.
const (
	// FinderFlagHasCustomIcon marks a folder or volume as carrying its own
	// icon — for a volume root, that is what makes the Finder look for
	// .VolumeIcon.icns and draw it instead of the generic disk. Writing the
	// file without the flag does nothing at all.
	FinderFlagHasCustomIcon = uint16(0x0400)

	// FinderFlagIsInvisible hides an entry, which is how a disk image keeps
	// its .background folder out of the window it decorates.
	FinderFlagIsInvisible = uint16(0x4000)
)

// FinderInfo returns the 32 Finder bytes of the entry at p.
//
// p may be "/" for the volume root, which is the case that matters for a
// volume icon.
func (v *Volume) FinderInfo(p string) ([finderInfoLen]byte, error) {
	var out [finderInfoLen]byte
	parent, name, err := v.finderInfoKey(p)
	if err != nil {
		return out, err
	}
	body, ok, err := v.recordBody(parent, name)
	if err != nil {
		return out, err
	}
	if !ok {
		return out, fmt.Errorf("%w: %q", ErrNotFound, p)
	}
	if len(body) < finderInfoOffset+finderInfoLen {
		return out, fmt.Errorf("%w: catalog record too short for Finder info", ErrCorrupt)
	}
	copy(out[:], body[finderInfoOffset:finderInfoOffset+finderInfoLen])
	return out, nil
}

// SetFinderInfo replaces the 32 Finder bytes of the entry at p.
//
// The record is re-keyed rather than patched in place, because the catalog is
// a B-tree and its writer owns node layout; delete-then-insert with the same
// key is how SetLabel already does it.
func (v *Volume) SetFinderInfo(p string, info [finderInfoLen]byte) error {
	if !v.writable() {
		return ErrReadOnly
	}
	parent, name, err := v.finderInfoKey(p)
	if err != nil {
		return err
	}
	body, ok, err := v.recordBody(parent, name)
	if err != nil {
		return err
	}
	if !ok {
		return fmt.Errorf("%w: %q", ErrNotFound, p)
	}
	if len(body) < finderInfoOffset+finderInfoLen {
		return fmt.Errorf("%w: catalog record too short for Finder info", ErrCorrupt)
	}
	updated := append([]byte(nil), body...)
	copy(updated[finderInfoOffset:finderInfoOffset+finderInfoLen], info[:])

	t, err := v.begin()
	if err != nil {
		return err
	}
	key := catalogKey{parentID: parent, name: name}
	if _, err := t.cw.deleteRecord(key); err != nil {
		return err
	}
	if err := t.cw.insertRecord(assembleCatalogRecord(encodeCatalogKey(parent, name), updated)); err != nil {
		return err
	}
	v.bumpWriteCount()
	return t.commit()
}

// finderInfoKey resolves p to the catalog key of the entry itself, not of its
// parent. The root is the special case: its record is keyed (parent=1, name=
// the volume label), which is also what SetLabel manipulates.
func (v *Volume) finderInfoKey(p string) (uint32, string, error) {
	if len(splitPath(p)) == 0 {
		label := v.Label()
		if label == "" {
			return 0, "", fmt.Errorf("%w: root folder record missing", ErrCorrupt)
		}
		return cnidRootParent, label, nil
	}
	parent, name, err := v.resolveParent(p)
	if err != nil {
		return 0, "", err
	}
	return parent, name, nil
}
