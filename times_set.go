package hfsplus

import (
	"encoding/binary"
	"errors"
	"fmt"
	"time"
)

// The driver has Chtimes but not Chmod or Chown, so it does not satisfy the
// bundled filesystem.MetadataSetter; callers probe for the single method.
var _ interface {
	Chtimes(path string, atime, mtime time.Time) error
} = (*Volume)(nil)

// hfsNow is the clock Chtimes stamps attributeModDate with.
var hfsNow = time.Now

// ErrTimeOutOfRange is what Chtimes returns, wrapped in a *TimeRangeError, for
// a time a catalog date cannot hold: an unsigned 32-bit count of seconds since
// 1904-01-01 00:00:00 GMT, so 1904-01-01 00:00:01 .. 2040-02-06 06:28:15 GMT.
// The value 0 (1904-01-01 00:00:00 itself) means "not recorded" to a reader, so
// it is refused too rather than written as a date that reads back as none.
var ErrTimeOutOfRange = errors.New("hfsplus: time outside the HFS+ range 1904-2040")

// TimeRangeError names the field and the value Chtimes refused.
type TimeRangeError struct {
	Field string // "atime", "mtime" or "now" (the clock, for attributeModDate)
	Time  time.Time
}

func (e *TimeRangeError) Error() string {
	return fmt.Sprintf("hfsplus: %s %s is outside the HFS+ range 1904-01-01 00:00:01 to 2040-02-06 06:28:15 GMT", e.Field, e.Time.UTC().Format(time.RFC3339))
}

func (e *TimeRangeError) Unwrap() error { return ErrTimeOutOfRange }

// encodeHFSDate is the inverse of decodeHFSDate: seconds since 1904 GMT, the
// sub-second part dropped (rounded down, the resolution Times reports).
func encodeHFSDate(field string, t time.Time) (uint32, error) {
	s := t.Unix() - hfsEpochToUnix
	if s < 1 || s > 0xFFFFFFFF {
		return 0, &TimeRangeError{Field: field, Time: t}
	}
	return uint32(s), nil
}

// Chtimes sets the access and modification times of path (Apple TN1150, "HFS
// Plus Dates" and "Catalog File Data Records").
//
//   - mtime is the record's contentModDate and atime its accessDate, each a
//     GMT count of whole seconds since 1904, rounded down.
//   - attributeModDate (what Times reports as Ctime) is set to the current time,
//     as POSIX utimes refreshes ctime. createDate (Btime) is untouched.
//
// A zero time.Time leaves that field unchanged; with both zero nothing is
// touched at all, ctime included. A time outside 1904-01-01 00:00:01 ..
// 2040-02-06 06:28:15 GMT is refused with a *TimeRangeError (errors.Is
// ErrTimeOutOfRange) before anything is written. The root folder is not
// served by Times and is refused here too.
//
// The record is patched in place in its catalog leaf node (the length does not
// change, so no node is split or merged and the B-tree stays valid), then the
// image is committed like every other mutation.
func (v *Volume) Chtimes(p string, atime, mtime time.Time) error {
	if !v.writable() {
		return ErrReadOnly
	}
	var a, m, c uint32
	var err error
	if !atime.IsZero() {
		if a, err = encodeHFSDate("atime", atime); err != nil {
			return err
		}
	}
	if !mtime.IsZero() {
		if m, err = encodeHFSDate("mtime", mtime); err != nil {
			return err
		}
	}
	if c, err = encodeHFSDate("now", hfsNow()); err != nil {
		return err
	}
	parent, name, err := v.resolveParent(p)
	if err != nil {
		return err
	}
	if _, found, err := v.lookupChild(parent, name); err != nil {
		return err
	} else if !found {
		return fmt.Errorf("%w: %q", ErrNotFound, name)
	}
	if atime.IsZero() && mtime.IsZero() {
		return nil
	}
	t, err := v.begin()
	if err != nil {
		return err
	}
	if err := t.cw.patchRecord(parent, name, func(data []byte) {
		putHFSDates(data, atime, a, mtime, m, c)
	}); err != nil {
		return err
	}
	v.bumpWriteCount()
	return t.commit()
}

// putHFSDates stores the encoded dates into a catalog record's data (the bytes
// after the key): createDate at 12, contentModDate 16, attributeModDate 20,
// accessDate 24. A time whose source was zero is skipped; attributeModDate
// always takes c.
func putHFSDates(data []byte, atime time.Time, a uint32, mtime time.Time, m, c uint32) {
	be := binary.BigEndian
	if !mtime.IsZero() {
		be.PutUint32(data[16:], m)
	}
	be.PutUint32(data[20:], c)
	if !atime.IsZero() {
		be.PutUint32(data[24:], a)
	}
}
