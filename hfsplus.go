// Copyright (c) 2026, go-filesystems
// SPDX-License-Identifier: BSD-3-Clause

package hfsplus

import (
	"fmt"
	"io"
	"os"
	"strings"

	filesystem "github.com/go-filesystems/interface"
	"github.com/go-volumes/safeio"
)

// maxImageBytes caps the in-memory image a writable open will allocate. HFS+
// volumes addressed here are disk-image sized; 64 GiB is a generous ceiling
// that still defeats a bogus stat size triggering a wild allocation.
const maxImageBytes = 64 << 30

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

// Volume is an opened HFS+ (or HFSX) volume. When opened read-only (Open /
// OpenFile) the mutating methods return ErrReadOnly. When opened writable
// (OpenWritable / OpenFileWritable / Format) the whole image is held in an
// in-memory byte slice that the write path edits in place; Sync (and the
// mutators, which Sync implicitly) flush the bytes back to the backing
// io.WriterAt when one is present.
type Volume struct {
	rs          io.ReaderAt
	size        int64
	vh          *volumeHeader
	catalogTree *btree
	extentsTree *btree
	closer      io.Closer

	// img is the full mutable image. Non-nil only for writable volumes; the
	// write engine edits it in place and rebuilds catalogTree/vh from it.
	img []byte
	// extentsWriter is the active extents-overflow tree writer during a
	// mutation; the catalog-fork growth path spills its extents through it.
	// Bound by txn.begin and cleared on commit.
	extentsWriter *extentsWriter
	// wa is the optional backing store flushed by Sync. nil for purely
	// in-memory writable volumes (OpenWritable on a []byte).
	wa io.WriterAt
}

// writable reports whether the volume was opened for writing.
func (v *Volume) writable() bool { return v.img != nil }

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
	if err := v.openTrees(); err != nil {
		return nil, err
	}
	return v, nil
}

// openTrees (re)builds the extents-overflow and catalog B-trees from the
// current volume header. The extents tree is opened first (from its inline
// extents) so the catalog fork — which may have spilled past its eight inline
// extents — can resolve its full extent list through it.
func (v *Volume) openTrees() error {
	v.extentsTree = nil
	if ef := newSpecialFork(v, v.vh.ExtentsFile); ef.size > 0 {
		if bt, err := openBTree(ef); err == nil {
			v.extentsTree = bt
		}
	}
	// If the extents file itself spilled past its inline extents, re-resolve it
	// through the (now-open) extents tree and reopen.
	if v.extentsTree != nil && extentsForkNeedsOverflow(v.vh.ExtentsFile) {
		if ef, err := newSpecialForkResolved(v, kHFSExtentsFileID, v.vh.ExtentsFile); err == nil {
			if bt, err := openBTree(ef); err == nil {
				v.extentsTree = bt
			}
		}
	}
	cf, err := newSpecialForkResolved(v, kHFSCatalogFileID, v.vh.CatalogFile)
	if err != nil {
		return err
	}
	if cf.size == 0 {
		return fmt.Errorf("%w: empty catalog file", ErrCorrupt)
	}
	bt, err := openBTree(cf)
	if err != nil {
		return err
	}
	v.catalogTree = bt
	return nil
}

// extentsForkNeedsOverflow reports whether a special file's inline extents do
// not cover its total blocks (so a continuation lives in the overflow tree).
func extentsForkNeedsOverflow(fd forkData) bool {
	var covered uint32
	for _, e := range fd.Extents {
		covered += e.BlockCount
	}
	return covered < fd.TotalBlocks
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

// OpenWritable opens an HFS+ image held entirely in img for read/write. The
// volume edits img in place; callers can retrieve the mutated bytes with
// Bytes() or, if wa is non-nil, flush them with Sync. Pass wa = nil for a
// purely in-memory writable volume.
func OpenWritable(img []byte, wa io.WriterAt) (*Volume, error) {
	v, err := Open(bytesReaderAt(img), int64(len(img)))
	if err != nil {
		return nil, err
	}
	v.img = img
	v.wa = wa
	// Rebind the reader to the in-memory image so the write path sees its own
	// edits immediately.
	v.rs = bytesReaderAt(img)
	return v, nil
}

// OpenFileWritable opens the image at path for read/write. The whole image is
// read into memory; mutations are flushed back to the file by Sync (and
// implicitly by every mutator).
func OpenFileWritable(path string) (*Volume, error) {
	f, err := os.OpenFile(path, os.O_RDWR, 0)
	if err != nil {
		return nil, fmt.Errorf("hfsplus: open %s: %w", path, err)
	}
	st, err := f.Stat()
	if err != nil {
		f.Close()
		return nil, fmt.Errorf("hfsplus: stat %s: %w", path, err)
	}
	// Bound the in-memory image allocation against a generous ceiling so a
	// bogus stat size cannot trigger a wild allocation (safeio class A).
	img, err := safeio.MakeBytes(st.Size(), maxImageBytes)
	if err != nil {
		f.Close()
		return nil, fmt.Errorf("hfsplus: image size %d: %w", st.Size(), err)
	}
	if _, err := io.ReadFull(f, img); err != nil {
		f.Close()
		return nil, fmt.Errorf("hfsplus: read %s: %w", path, err)
	}
	v, err := OpenWritable(img, &fileWriterAt{f})
	if err != nil {
		f.Close()
		return nil, err
	}
	v.closer = f
	return v, nil
}

// Bytes returns the current (possibly mutated) image bytes for a writable
// volume, or nil for a read-only one. The slice aliases the volume's internal
// buffer; copy it if you need a stable snapshot.
func (v *Volume) Bytes() []byte { return v.img }

// Sync flushes the in-memory image back to the backing store, if any. It is a
// no-op for read-only or purely in-memory volumes.
func (v *Volume) Sync() error {
	if v.wa == nil || v.img == nil {
		return nil
	}
	if _, err := v.wa.WriteAt(v.img, 0); err != nil {
		return fmt.Errorf("hfsplus: sync: %w", err)
	}
	if s, ok := v.wa.(interface{ Sync() error }); ok {
		return s.Sync()
	}
	return nil
}

// reopen rebuilds the parsed view (volume header + catalog/extents trees) from
// the current image bytes after a mutation rewrote on-disk structures.
func (v *Volume) reopen() error {
	vh, err := readVolumeHeader(v.rs)
	if err != nil {
		return err
	}
	v.vh = vh
	return v.openTrees()
}

// fileWriterAt adapts an *os.File to io.WriterAt with a Sync method.
type fileWriterAt struct{ f *os.File }

func (w *fileWriterAt) WriteAt(p []byte, off int64) (int, error) { return w.f.WriteAt(p, off) }
func (w *fileWriterAt) Sync() error                              { return w.f.Sync() }

// bytesReaderAt is a minimal io.ReaderAt over a byte slice (avoids importing
// bytes for a one-method shim and lets the writable volume re-bind to a live
// image buffer).
type bytesReaderAt []byte

func (b bytesReaderAt) ReadAt(p []byte, off int64) (int, error) {
	if off < 0 || off >= int64(len(b)) {
		return 0, io.EOF
	}
	n := copy(p, b[off:])
	if n < len(p) {
		return n, io.EOF
	}
	return n, nil
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

// Mutating methods (WriteFile, MkDir, DeleteFile, DeleteDir, Rename) plus the
// optional Symlinker/Truncater/Labeller capabilities live in write.go. On a
// read-only volume (Open / OpenFile) they return ErrReadOnly; on a writable
// volume (OpenWritable / OpenFileWritable / Format) they edit the image and
// flush via Sync.
