package hfsplus

import (
	"encoding/binary"
	"fmt"
	"os"

	filesystem "github.com/go-filesystems/interface"
)

var _ filesystem.MetadataSetter = (*Volume)(nil)

// HFSPlusBSDInfo (Apple TN1150, "Catalog File Data Records") sits at byte 32 of
// both the file and the folder catalog record: ownerID(4) groupID(4)
// adminFlags(1) ownerFlags(1) fileMode(2) special(4). The record is patched in
// place, so its length, and with it the B-tree node, does not change.
const (
	bsdOwnerOff = 32
	bsdGroupOff = 36
	bsdModeOff  = 42
	bsdInfoEnd  = 48

	sISUID = 0o4000
	sISGID = 0o2000
	sISVTX = 0o1000
)

// Chmod replaces the permission bits (including setuid, setgid and sticky) of
// the file or folder at path, keeping the file-type bits of fileMode. ctime
// (attributeModDate) is set to the clock. The root folder is refused, as it is
// for Chtimes.
func (v *Volume) Chmod(p string, perm os.FileMode) error {
	bits := uint16(perm.Perm())
	if perm&os.ModeSetuid != 0 {
		bits |= sISUID
	}
	if perm&os.ModeSetgid != 0 {
		bits |= sISGID
	}
	if perm&os.ModeSticky != 0 {
		bits |= sISVTX
	}
	return v.patchBSD(p, func(data []byte) {
		old := binary.BigEndian.Uint16(data[bsdModeOff:])
		binary.BigEndian.PutUint16(data[bsdModeOff:], old&^0o7777|bits)
	})
}

// Chown sets ownerID and groupID of the file or folder at path (HFSPlusBSDInfo,
// stored as given). ctime (attributeModDate) is set to the clock; mode, body and
// the other dates are left alone.
func (v *Volume) Chown(p string, uid, gid uint32) error {
	return v.patchBSD(p, func(data []byte) {
		binary.BigEndian.PutUint32(data[bsdOwnerOff:], uid)
		binary.BigEndian.PutUint32(data[bsdGroupOff:], gid)
	})
}

// patchBSD applies mutate to the BSD info of the record at p and refreshes
// attributeModDate, all before anything is written if the clock or the record is
// unusable.
func (v *Volume) patchBSD(p string, mutate func(data []byte)) error {
	if err := v.mutable(); err != nil {
		return err
	}
	c, err := encodeHFSDate("now", hfsNow())
	if err != nil {
		return err
	}
	parent, name, err := v.resolveParent(p)
	if err != nil {
		return err
	}
	body, found, err := v.recordBody(parent, name)
	if err != nil {
		return err
	}
	if !found {
		return fmt.Errorf("%w: %q", ErrNotFound, name)
	}
	if len(body) < bsdInfoEnd || (int16(binary.BigEndian.Uint16(body)) != recordFile && int16(binary.BigEndian.Uint16(body)) != recordFolder) {
		return fmt.Errorf("%w: %q has no BSD info", ErrCorrupt, name)
	}
	t, err := v.begin()
	if err != nil {
		return err
	}
	if err := t.cw.patchRecord(parent, name, func(data []byte) {
		mutate(data)
		binary.BigEndian.PutUint32(data[20:], c) // attributeModDate
	}); err != nil {
		return err
	}
	v.bumpWriteCount()
	return t.commit()
}
