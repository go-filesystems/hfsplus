package hfsplus

import (
	"encoding/binary"
	"time"

	filesystem "github.com/go-filesystems/interface"
)

var _ filesystem.TimesReader = (*Volume)(nil)

// hfsEpochToUnix is the number of seconds from 1904-01-01 to 1970-01-01. A
// catalog date counts seconds since midnight 1904-01-01 GMT (Apple TN1150,
// "HFS Plus Dates"); the range ends 2040-02-06 06:28:15 GMT.
const hfsEpochToUnix = -2082844800

// hfsTimeRes is the resolution of every HFS+ date: one second.
const hfsTimeRes = time.Second

// Times returns the times recorded in the catalog record of path.
//
//   - Btime is createDate, Mtime is contentModDate, Ctime is attributeModDate
//     (Mac OS X's BSD layer returns it as st_ctime), Atime is accessDate.
//   - All are GMT in catalog records, resolution 1 s. (Only the Volume Header's
//     createDate is local time, per TN1150; it is not used here.)
//   - A date of 0 is reported as not recorded: TN1150 says traditional Mac OS
//     leaves accessDate at zero, and attributeModDate "may be treated as
//     reserved" by an implementation.
//   - The root folder is synthesised by this reader without its catalog
//     record, so none of its times are recorded. Symbolic links are not
//     followed.
func (v *Volume) Times(path string) (filesystem.FileTimes, error) {
	r, err := v.lookupPath(path)
	if err != nil {
		return filesystem.FileTimes{}, err
	}
	switch {
	case r.rec.folder != nil:
		return decodeHFSTimes(r.rec.folder.dates[:]), nil
	case r.rec.file != nil:
		return decodeHFSTimes(r.rec.file.dates[:]), nil
	}
	return filesystem.FileTimes{}, nil
}

// decodeHFSTimes decodes the 20 bytes createDate, contentModDate,
// attributeModDate, accessDate, backupDate of a catalog record.
func decodeHFSTimes(b []byte) filesystem.FileTimes {
	le := binary.BigEndian
	return filesystem.FileTimes{
		Btime: decodeHFSDate(le.Uint32(b[0:])),
		Mtime: decodeHFSDate(le.Uint32(b[4:])),
		Ctime: decodeHFSDate(le.Uint32(b[8:])),
		Atime: decodeHFSDate(le.Uint32(b[12:])),
	}
}

// decodeHFSDate converts one HFS+ date; 0 is "not recorded".
func decodeHFSDate(d uint32) filesystem.Timestamp {
	if d == 0 {
		return filesystem.Timestamp{}
	}
	return filesystem.NewTimestamp(time.Unix(int64(d)+hfsEpochToUnix, 0), hfsTimeRes)
}
