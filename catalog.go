// Copyright (c) 2026, go-filesystems
// SPDX-License-Identifier: BSD-3-Clause

package hfsplus

import (
	"encoding/binary"
	"unicode/utf16"
)

// Well-known catalog node IDs (CNIDs).
const (
	cnidRootParent = 1 // parent of the root folder
	cnidRootFolder = 2 // the volume root directory
)

// Catalog record types (HFSPlusCatalogFolder/File/Thread), stored as the first
// int16 of the record data.
const (
	recordFolder       = 0x0001
	recordFile         = 0x0002
	recordFolderThread = 0x0003
	recordFileThread   = 0x0004
)

// catalogKey is a decoded HFSPlusCatalogKey: a parent CNID and a UTF-16 node
// name (already decoded to a Go string for comparison/display).
type catalogKey struct {
	parentID uint32
	name     string   // decoded
	nameU16  []uint16 // raw UTF-16 code units (for ordered comparison)
}

// parseCatalogKey decodes the key bytes of a catalog record. keyBytes excludes
// the 2-byte key-length prefix (it is rec[keyStart:keyStart+keyLen]).
func parseCatalogKey(keyBytes []byte) (catalogKey, bool) {
	if len(keyBytes) < 6 {
		return catalogKey{}, false
	}
	parent := binary.BigEndian.Uint32(keyBytes[0:4])
	nameLen := int(binary.BigEndian.Uint16(keyBytes[4:6]))
	u16 := make([]uint16, 0, nameLen)
	off := 6
	for i := 0; i < nameLen; i++ {
		if off+2 > len(keyBytes) {
			return catalogKey{}, false
		}
		u16 = append(u16, binary.BigEndian.Uint16(keyBytes[off:off+2]))
		off += 2
	}
	return catalogKey{
		parentID: parent,
		name:     string(utf16.Decode(u16)),
		nameU16:  u16,
	}, true
}

// keyFromRecord decodes the catalog key embedded in a B-tree record.
func keyFromRecord(rec []byte) (catalogKey, bool) {
	keyLen, keyStart, ok := recordKeyLen(rec)
	if !ok {
		return catalogKey{}, false
	}
	return parseCatalogKey(rec[keyStart : keyStart+keyLen])
}

// catalogFolder / catalogFile carry the subset of fields the reader exposes.
type catalogFolder struct {
	folderID uint32
	valence  uint32
	flags    uint16
}

type catalogFile struct {
	fileID   uint32
	flags    uint16
	dataFork forkData
	rsrcFork forkData
	permMode uint16 // BSD mode from HFSPlusBSDInfo
}

// Decoded record union: exactly one of folder/file/thread is set.
type catalogRecord struct {
	recType      int16
	folder       *catalogFolder
	file         *catalogFile
	threadName   string // for thread records: the node's own name
	threadParent uint32
}

// parseCatalogRecord decodes the data portion of a catalog leaf record.
func parseCatalogRecord(data []byte) (catalogRecord, bool) {
	if len(data) < 2 {
		return catalogRecord{}, false
	}
	rt := int16(binary.BigEndian.Uint16(data[0:2]))
	switch rt {
	case recordFolder:
		// HFSPlusCatalogFolder: recordType(2) flags(2) valence(4) folderID(4) ...
		if len(data) < 14 {
			return catalogRecord{}, false
		}
		return catalogRecord{
			recType: rt,
			folder: &catalogFolder{
				flags:    binary.BigEndian.Uint16(data[2:4]),
				valence:  binary.BigEndian.Uint32(data[4:8]),
				folderID: binary.BigEndian.Uint32(data[8:12]),
			},
		}, true
	case recordFile:
		// HFSPlusCatalogFile layout (offsets within record data):
		//   0  recordType (2)
		//   2  flags (2)
		//   4  reserved1 (4)
		//   8  fileID (4)
		//   12 five uint32 dates (createDate..backupDate) → through 31
		//   32 HFSPlusBSDInfo (16): ownerID(4) groupID(4) adminFlags(1)
		//      ownerFlags(1) fileMode(2 at +10 = offset 42) special(4)
		//   48 userInfo FInfo (16), 64 finderInfo FXInfo (16)
		//   80 textEncoding (4), 84 reserved2 (4)
		//   88 HFSPlusForkData dataFork (80)
		//   168 HFSPlusForkData rsrcFork (80)
		if len(data) < 88+forkDataLen {
			return catalogRecord{}, false
		}
		cf := &catalogFile{
			flags:  binary.BigEndian.Uint16(data[2:4]),
			fileID: binary.BigEndian.Uint32(data[8:12]),
		}
		// fileMode lives at offset 42 within the record (HFSPlusBSDInfo starts
		// at 32; fileMode is at +10).
		cf.permMode = binary.BigEndian.Uint16(data[42:44])
		cf.dataFork = parseForkData(data[88 : 88+forkDataLen])
		if len(data) >= 168+forkDataLen {
			cf.rsrcFork = parseForkData(data[168 : 168+forkDataLen])
		}
		return catalogRecord{recType: rt, file: cf}, true
	case recordFolderThread, recordFileThread:
		// HFSPlusCatalogThread: recordType(2) reserved(2) parentID(4)
		// nodeName (HFSUniStr255: length(2) + UTF-16).
		if len(data) < 10 {
			return catalogRecord{}, false
		}
		parent := binary.BigEndian.Uint32(data[4:8])
		nameLen := int(binary.BigEndian.Uint16(data[8:10]))
		u16 := make([]uint16, 0, nameLen)
		off := 10
		for i := 0; i < nameLen && off+2 <= len(data); i++ {
			u16 = append(u16, binary.BigEndian.Uint16(data[off:off+2]))
			off += 2
		}
		return catalogRecord{recType: rt, threadParent: parent, threadName: string(utf16.Decode(u16))}, true
	}
	return catalogRecord{recType: rt, file: nil, folder: nil}, true
}
