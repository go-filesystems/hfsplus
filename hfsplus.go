// Copyright (c) 2026, go-filesystems
// SPDX-License-Identifier: BSD-3-Clause

package hfsplus

import (
	"fmt"
	"io"
	"os"
	"strings"

	filesystem "github.com/go-filesystems/interface"
)

// Unix mode type bits used when synthesising a Stat mode for entries whose
// BSD info is absent.
const (
	sIFDIR = 0x4000
	sIFREG = 0x8000
	sIFLNK = 0xA000

	modeDirDefault  = sIFDIR | 0o755
	modeFileDefault = sIFREG | 0o644
)

// File type codes for filesystem.DirEntry.FileType.
const (
	ftRegular = 1
	ftDir     = 2
	ftSymlink = 7
)

// Volume is an opened, read-only HFS+ (or HFSX) volume.
type Volume struct {
	rs          io.ReaderAt
	size        int64
	vh          *volumeHeader
	catalogTree *btree
	extentsTree *btree
	closer      io.Closer
}

var _ filesystem.Filesystem = (*Volume)(nil)

// Open parses an HFS+ volume from rs. The caller retains ownership of rs unless
// it implements io.Closer (then Close releases it). Pass size = -1 if unknown.
func Open(rs io.ReaderAt, size int64) (*Volume, error) {
	vh, err := readVolumeHeader(rs)
	if err != nil {
		return nil, err
	}
	v := &Volume{rs: rs, size: size, vh: vh}
	if c, ok := rs.(io.Closer); ok {
		v.closer = c
	}
	// Build the extents-overflow tree first so file-fork resolution can use
	// it; tolerate an absent/empty extents file.
	if ef := newSpecialFork(v, vh.ExtentsFile); ef.size > 0 {
		if bt, err := openBTree(ef); err == nil {
			v.extentsTree = bt
		}
	}
	cf := newSpecialFork(v, vh.CatalogFile)
	if cf.size == 0 {
		return nil, fmt.Errorf("%w: empty catalog file", ErrCorrupt)
	}
	bt, err := openBTree(cf)
	if err != nil {
		return nil, err
	}
	v.catalogTree = bt
	return v, nil
}

// OpenFile opens the image at path read-only.
func OpenFile(path string) (*Volume, error) {
	f, err := os.Open(path)
	if err != nil {
		return nil, fmt.Errorf("hfsplus: open %s: %w", path, err)
	}
	st, err := f.Stat()
	if err != nil {
		f.Close()
		return nil, fmt.Errorf("hfsplus: stat %s: %w", path, err)
	}
	v, err := Open(f, st.Size())
	if err != nil {
		f.Close()
		return nil, err
	}
	return v, nil
}

// VolumeHeader exposes the decoded volume header (owned by Volume).
func (v *Volume) VolumeHeader() *volumeHeader { return v.vh }

// CaseSensitive reports whether the volume is HFSX with binary key comparison.
func (v *Volume) CaseSensitive() bool {
	return v.catalogTree != nil && v.catalogTree.header.KeyCompare == keyCompareBinary
}

// Close releases the backing handle if Volume opened one.
func (v *Volume) Close() error {
	if v.closer != nil {
		return v.closer.Close()
	}
	return nil
}

// resolved is the outcome of a path lookup: the matching catalog record plus
// the CNID it resolves to.
type resolved struct {
	rec  catalogRecord
	cnid uint32
}

// splitPath normalises an absolute path into its non-empty, non-"." parts.
func splitPath(p string) []string {
	out := make([]string, 0, 8)
	for _, s := range strings.Split(p, "/") {
		if s == "" || s == "." {
			continue
		}
		out = append(out, s)
	}
	return out
}

// lookupPath resolves an absolute path to a catalog record, walking from the
// root folder (CNID 2) one component at a time.
func (v *Volume) lookupPath(path string) (resolved, error) {
	parts := splitPath(path)
	if len(parts) == 0 {
		// The root directory itself.
		return resolved{
			rec:  catalogRecord{recType: recordFolder, folder: &catalogFolder{folderID: cnidRootFolder}},
			cnid: cnidRootFolder,
		}, nil
	}
	parent := uint32(cnidRootFolder)
	var last catalogRecord
	for i, name := range parts {
		rec, found, err := v.catalogTree.lookup(catalogKey{parentID: parent, name: name})
		if err != nil {
			return resolved{}, err
		}
		if !found {
			return resolved{}, fmt.Errorf("%w: %q", ErrNotFound, name)
		}
		last = rec
		switch {
		case rec.folder != nil:
			parent = rec.folder.folderID
		case rec.file != nil:
			if i != len(parts)-1 {
				return resolved{}, fmt.Errorf("%w: %q", ErrNotDirectory, name)
			}
			parent = rec.file.fileID
		default:
			return resolved{}, fmt.Errorf("%w: %q", ErrCorrupt, name)
		}
	}
	return resolved{rec: last, cnid: parent}, nil
}

// ListDir enumerates the directory at path.
func (v *Volume) ListDir(path string) ([]filesystem.DirEntry, error) {
	r, err := v.lookupPath(path)
	if err != nil {
		return nil, err
	}
	if r.rec.folder == nil {
		return nil, fmt.Errorf("%w: %s", ErrNotDirectory, path)
	}
	children, err := v.catalogTree.listChildren(r.rec.folder.folderID)
	if err != nil {
		return nil, err
	}
	out := make([]filesystem.DirEntry, 0, len(children))
	for _, c := range children {
		var (
			ftype uint8
			inode uint64
		)
		switch {
		case c.rec.folder != nil:
			ftype = ftDir
			inode = uint64(c.rec.folder.folderID)
		case c.rec.file != nil:
			ftype = ftRegular
			inode = uint64(c.rec.file.fileID)
			if isSymlinkMode(c.rec.file.permMode) {
				ftype = ftSymlink
			}
		default:
			continue
		}
		out = append(out, filesystem.NewDirEntry(inode, c.key.name, ftype))
	}
	return out, nil
}

// Stat resolves path and returns mode, size, and the CNID as a pseudo-inode.
func (v *Volume) Stat(path string) (filesystem.Stat, error) {
	r, err := v.lookupPath(path)
	if err != nil {
		return nil, err
	}
	switch {
	case r.rec.folder != nil:
		return filesystem.NewStat(modeDirDefault, 0, uint64(r.rec.folder.folderID)), nil
	case r.rec.file != nil:
		mode := r.rec.file.permMode
		if mode == 0 {
			mode = modeFileDefault
		}
		return filesystem.NewStat(mode, r.rec.file.dataFork.LogicalSize, uint64(r.rec.file.fileID)), nil
	}
	return nil, fmt.Errorf("%w: %s", ErrCorrupt, path)
}

// dataForkFor builds a fork reader for the data fork of a catalog file,
// resolving inline + overflow extents.
func (v *Volume) dataForkFor(cf *catalogFile) (*fork, error) {
	exts, err := v.resolveForkExtents(cf.fileID, forkTypeData, cf.dataFork)
	if err != nil {
		return nil, err
	}
	return &fork{vol: v, size: int64(cf.dataFork.LogicalSize), extents: exts}, nil
}

// ReadFile returns the full contents of the regular file at path.
func (v *Volume) ReadFile(path string) ([]byte, error) {
	r, err := v.lookupPath(path)
	if err != nil {
		return nil, err
	}
	if r.rec.file == nil {
		return nil, fmt.Errorf("%w: %s", ErrNotRegular, path)
	}
	if isCompressed(r.rec.file.flags) {
		return nil, fmt.Errorf("%w: compressed fork", ErrUnsupported)
	}
	f, err := v.dataForkFor(r.rec.file)
	if err != nil {
		return nil, err
	}
	return f.readAll()
}

// ReadLink returns the target of a symbolic link. HFS+ stores the target as
// the data-fork contents of a file whose BSD mode marks it S_IFLNK.
func (v *Volume) ReadLink(path string) (string, error) {
	r, err := v.lookupPath(path)
	if err != nil {
		return "", err
	}
	if r.rec.file == nil || !isSymlinkMode(r.rec.file.permMode) {
		return "", fmt.Errorf("%w: %s", ErrNotSymlink, path)
	}
	f, err := v.dataForkFor(r.rec.file)
	if err != nil {
		return "", err
	}
	target, err := f.readAll()
	if err != nil {
		return "", err
	}
	return string(target), nil
}

// isSymlinkMode reports whether a BSD mode value marks a symbolic link.
func isSymlinkMode(mode uint16) bool {
	return mode&0xF000 == sIFLNK
}

// kFileFlagCompressed marks a file whose data is stored via decmpfs
// compression (HFSPlusCatalogFile.flags bit 0x0020). We surface it as
// unsupported rather than returning garbage.
const kFileFlagCompressed = 0x0020

func isCompressed(flags uint16) bool { return flags&kFileFlagCompressed != 0 }

// --- Mutating methods: the reader is read-only. ---

func (v *Volume) WriteFile(string, []byte, os.FileMode) error { return ErrReadOnly }
func (v *Volume) MkDir(string, os.FileMode) error             { return ErrReadOnly }
func (v *Volume) DeleteFile(string) error                     { return ErrReadOnly }
func (v *Volume) DeleteDir(string) error                      { return ErrReadOnly }
func (v *Volume) Rename(string, string) error                 { return ErrReadOnly }
