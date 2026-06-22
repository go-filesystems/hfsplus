// Copyright (c) 2026, go-filesystems
// SPDX-License-Identifier: BSD-3-Clause

package hfsplus

import (
	"encoding/binary"
	"fmt"
	"os"
	"strings"
	"time"
	"unicode/utf16"

	filesystem "github.com/go-filesystems/interface"
)

// write.go implements the Filesystem write contract on top of the catalog
// write engine (catwrite.go) and the allocation bitmap (alloc.go). Every
// mutator edits the in-memory image, rewrites the affected catalog records and
// bitmap, updates the volume-header counters (nextCatalogID, fileCount,
// folderCount, freeBlocks), reparses the volume, and flushes to the backing
// store via Sync.

// Compile-time assertions: a writable Volume satisfies the full Filesystem
// contract plus the optional Labeller / Symlinker / Truncater capabilities
// whose write side this driver implements.
var (
	_ filesystem.Filesystem = (*Volume)(nil)
	_ filesystem.Labeller   = (*Volume)(nil)
	_ filesystem.Symlinker  = (*Volume)(nil)
	_ filesystem.Truncater  = (*Volume)(nil)
)

// txn bundles the mutable state a single operation touches.
type txn struct {
	v   *Volume
	cw  *catWriter
	alc *allocator
}

func (v *Volume) begin() (*txn, error) {
	cw, err := v.newCatWriter()
	if err != nil {
		return nil, err
	}
	alc, err := v.newAllocator()
	if err != nil {
		return nil, err
	}
	return &txn{v: v, cw: cw, alc: alc}, nil
}

// commit flushes the allocator bitmap, reparses the volume from the mutated
// image, and syncs to the backing store.
func (t *txn) commit() error {
	if err := t.alc.flush(); err != nil {
		return err
	}
	if err := t.v.reopen(); err != nil {
		return err
	}
	return t.v.Sync()
}

// --- volume header counter helpers ---

func (v *Volume) headerU32(off int) uint32 {
	return binary.BigEndian.Uint32(v.img[volumeHeaderOffset+off : volumeHeaderOffset+off+4])
}

func (v *Volume) setHeaderU32(off int, val uint32) {
	binary.BigEndian.PutUint32(v.img[volumeHeaderOffset+off:volumeHeaderOffset+off+4], val)
	alt := int64(len(v.img)) - 1024
	binary.BigEndian.PutUint32(v.img[alt+int64(off):alt+int64(off)+4], val)
}

const (
	vhFileCount     = 0x20
	vhFolderCount   = 0x24
	vhNextCatalogID = 0x40
	vhWriteCount    = 0x44
)

// nextCNID returns and advances the volume's next catalog ID.
func (v *Volume) nextCNID() uint32 {
	id := v.headerU32(vhNextCatalogID)
	if id < firstUserCatalogID {
		id = firstUserCatalogID
	}
	v.setHeaderU32(vhNextCatalogID, id+1)
	return id
}

func (v *Volume) bumpWriteCount() {
	v.setHeaderU32(vhWriteCount, v.headerU32(vhWriteCount)+1)
}

func (v *Volume) incFileCount(d int32) {
	v.setHeaderU32(vhFileCount, addU32(v.headerU32(vhFileCount), d))
}
func (v *Volume) incFolderCount(d int32) {
	v.setHeaderU32(vhFolderCount, addU32(v.headerU32(vhFolderCount), d))
}

func addU32(u uint32, d int32) uint32 {
	if d < 0 {
		return u - uint32(-d)
	}
	return u + uint32(d)
}

// --- path resolution for writes ---

// resolveParent splits an absolute path into (parentCNID, leafName), verifying
// the parent exists and is a directory.
func (v *Volume) resolveParent(p string) (uint32, string, error) {
	parts := splitPath(p)
	if len(parts) == 0 {
		return 0, "", fmt.Errorf("%w: cannot mutate root", ErrExists)
	}
	name := parts[len(parts)-1]
	parent := uint32(cnidRootFolder)
	for _, comp := range parts[:len(parts)-1] {
		rec, found, err := v.catalogTree.lookup(catalogKey{parentID: parent, name: comp})
		if err != nil {
			return 0, "", err
		}
		if !found {
			return 0, "", fmt.Errorf("%w: %q", ErrNotFound, comp)
		}
		if rec.folder == nil {
			return 0, "", fmt.Errorf("%w: %q", ErrNotDirectory, comp)
		}
		parent = rec.folder.folderID
	}
	return parent, name, nil
}

// lookupChild returns the record for (parent,name).
func (v *Volume) lookupChild(parent uint32, name string) (catalogRecord, bool, error) {
	return v.catalogTree.lookup(catalogKey{parentID: parent, name: name})
}

// adjustParentValence updates the valence (child count) of the folder whose
// CNID is parent by delta, and — on HFSX volumes that carry a folderCount —
// the folderCount by folderDelta. fsck_hfs's "Invalid directory item count"
// check requires the folder record's valence to equal its number of children.
//
// The folder record is located by first reading its thread record
// (key = parent CNID, empty name) to recover its (grandparent, name) key, then
// rewriting the valence field of the folder record in place. The root folder
// (CNID 2) is keyed (1, label) and has no separate lookup wrinkle.
func (t *txn) adjustParentValence(parent uint32, delta, folderDelta int32) error {
	v := t.v
	// Find the folder's own key via its thread record.
	gp, name, ok, err := v.threadKeyOf(parent)
	if err != nil {
		return err
	}
	if !ok {
		return fmt.Errorf("%w: no thread for folder CNID %d", ErrCorrupt, parent)
	}
	// Locate the folder record (gp, name) directly in its leaf and patch it.
	return t.cw.patchFolderRecord(gp, name, func(folder []byte) {
		val := binary.BigEndian.Uint32(folder[4:8])
		binary.BigEndian.PutUint32(folder[4:8], addU32(val, delta))
		if binary.BigEndian.Uint16(folder[2:4])&kHFSHasFolderCountMask != 0 {
			fc := binary.BigEndian.Uint32(folder[84:88])
			binary.BigEndian.PutUint32(folder[84:88], addU32(fc, folderDelta))
		}
	})
}

// threadKeyOf returns the (parentCNID, name) recorded in the thread record of
// the given CNID — i.e. that node's own catalog key.
func (v *Volume) threadKeyOf(cnid uint32) (uint32, string, bool, error) {
	rec, found, err := v.catalogTree.lookup(catalogKey{parentID: cnid, name: ""})
	if err != nil {
		return 0, "", false, err
	}
	if !found || (rec.recType != recordFolderThread && rec.recType != recordFileThread) {
		return 0, "", false, nil
	}
	return rec.threadParent, rec.threadName, true, nil
}

// --- mutators ---

// WriteFile creates or overwrites the regular file at path with data. The file
// is stored contiguously across freshly-allocated allocation blocks (its data
// fork uses the inline extents). Files needing more than numInlineExtents
// extents return ErrUnsupported (extents-overflow insert is a documented
// simplification); a contiguous allocation always fits in one inline extent.
func (v *Volume) WriteFile(p string, data []byte, perm os.FileMode) error {
	if !v.writable() {
		return ErrReadOnly
	}
	parent, name, err := v.resolveParent(p)
	if err != nil {
		return err
	}
	t, err := v.begin()
	if err != nil {
		return err
	}
	// If it already exists as a file, delete it first (overwrite semantics).
	if rec, found, err := v.lookupChild(parent, name); err != nil {
		return err
	} else if found {
		if rec.file == nil {
			return fmt.Errorf("%w: %q is a directory", ErrExists, name)
		}
		if err := t.removeFile(parent, name, rec.file); err != nil {
			return err
		}
		// Re-begin so the catalog/bitmap reflect the delete before inserting.
		if err := t.commit(); err != nil {
			return err
		}
		if t, err = v.begin(); err != nil {
			return err
		}
	}
	mode := uint16(sIFREG) | uint16(perm.Perm())
	if err := t.createFile(parent, name, data, mode); err != nil {
		return err
	}
	v.bumpWriteCount()
	return t.commit()
}

// createFile allocates blocks, writes the data fork, and inserts the catalog
// file + thread records.
func (t *txn) createFile(parent uint32, name string, data []byte, mode uint16) error {
	v := t.v
	bs := int64(v.vh.BlockSize)
	nblocks := uint32((int64(len(data)) + bs - 1) / bs)
	var df forkData
	df.LogicalSize = uint64(len(data))
	df.ClumpSize = v.vh.BlockSize
	df.TotalBlocks = nblocks
	if nblocks > 0 {
		start, err := t.alc.allocContiguous(nblocks)
		if err != nil {
			return err
		}
		df.Extents[0] = extentDescriptor{StartBlock: start, BlockCount: nblocks}
		// Write data into the image.
		off := int64(start) * bs
		copy(v.img[off:off+int64(len(data))], data)
		// Zero any tail of the last block beyond the data.
		tail := off + int64(len(data))
		end := off + int64(nblocks)*bs
		for i := tail; i < end; i++ {
			v.img[i] = 0
		}
	}
	cnid := v.nextCNID()
	fileRec := encodeFileRecord(cnid, time.Now(), mode, df)
	key := encodeCatalogKey(parent, name)
	rec := assembleCatalogRecord(key, fileRec)
	if err := t.cw.insertRecord(rec); err != nil {
		return err
	}
	threadRec := encodeFileThreadRecord(parent, name)
	threadKey := encodeCatalogKey(cnid, "")
	if err := t.cw.insertRecord(assembleCatalogRecord(threadKey, threadRec)); err != nil {
		return err
	}
	v.incFileCount(1)
	return t.adjustParentValence(parent, +1, 0)
}

// removeFile frees the file's data-fork blocks and removes its catalog +
// thread records.
func (t *txn) removeFile(parent uint32, name string, cf *catalogFile) error {
	v := t.v
	for _, e := range cf.dataFork.Extents {
		if e.BlockCount == 0 {
			break
		}
		t.alc.freeRun(e.StartBlock, e.BlockCount)
	}
	if _, err := t.cw.deleteRecord(catalogKey{parentID: parent, name: name}); err != nil {
		return err
	}
	if _, err := t.cw.deleteRecord(catalogKey{parentID: cf.fileID, name: ""}); err != nil {
		return err
	}
	v.incFileCount(-1)
	return t.adjustParentValence(parent, -1, 0)
}

// MkDir creates a directory at path.
func (v *Volume) MkDir(p string, perm os.FileMode) error {
	if !v.writable() {
		return ErrReadOnly
	}
	parent, name, err := v.resolveParent(p)
	if err != nil {
		return err
	}
	if _, found, err := v.lookupChild(parent, name); err != nil {
		return err
	} else if found {
		return fmt.Errorf("%w: %q", ErrExists, name)
	}
	t, err := v.begin()
	if err != nil {
		return err
	}
	cnid := v.nextCNID()
	mode := uint16(sIFDIR) | uint16(perm.Perm())
	folderRec := encodeFolderRecord(cnid, time.Now(), 0)
	binary.BigEndian.PutUint16(folderRec[42:44], mode)
	if v.CaseSensitive() {
		setHasFolderCount(folderRec, 0)
	}
	key := encodeCatalogKey(parent, name)
	if err := t.cw.insertRecord(assembleCatalogRecord(key, folderRec)); err != nil {
		return err
	}
	threadRec := encodeFolderThreadRecord(parent, name)
	threadKey := encodeCatalogKey(cnid, "")
	if err := t.cw.insertRecord(assembleCatalogRecord(threadKey, threadRec)); err != nil {
		return err
	}
	v.incFolderCount(1)
	if err := t.adjustParentValence(parent, +1, +1); err != nil {
		return err
	}
	v.bumpWriteCount()
	return t.commit()
}

// DeleteFile removes the regular file (or symlink) at path.
func (v *Volume) DeleteFile(p string) error {
	if !v.writable() {
		return ErrReadOnly
	}
	parent, name, err := v.resolveParent(p)
	if err != nil {
		return err
	}
	rec, found, err := v.lookupChild(parent, name)
	if err != nil {
		return err
	}
	if !found {
		return fmt.Errorf("%w: %q", ErrNotFound, name)
	}
	if rec.file == nil {
		return fmt.Errorf("%w: %q", ErrNotRegular, name)
	}
	t, err := v.begin()
	if err != nil {
		return err
	}
	if err := t.removeFile(parent, name, rec.file); err != nil {
		return err
	}
	v.bumpWriteCount()
	return t.commit()
}

// DeleteDir removes the empty directory at path.
func (v *Volume) DeleteDir(p string) error {
	if !v.writable() {
		return ErrReadOnly
	}
	parent, name, err := v.resolveParent(p)
	if err != nil {
		return err
	}
	rec, found, err := v.lookupChild(parent, name)
	if err != nil {
		return err
	}
	if !found {
		return fmt.Errorf("%w: %q", ErrNotFound, name)
	}
	if rec.folder == nil {
		return fmt.Errorf("%w: %q", ErrNotDirectory, name)
	}
	children, err := v.catalogTree.listChildren(rec.folder.folderID)
	if err != nil {
		return err
	}
	if len(children) > 0 {
		return fmt.Errorf("%w: %q", ErrNotEmpty, name)
	}
	t, err := v.begin()
	if err != nil {
		return err
	}
	if _, err := t.cw.deleteRecord(catalogKey{parentID: parent, name: name}); err != nil {
		return err
	}
	if _, err := t.cw.deleteRecord(catalogKey{parentID: rec.folder.folderID, name: ""}); err != nil {
		return err
	}
	v.incFolderCount(-1)
	if err := t.adjustParentValence(parent, -1, -1); err != nil {
		return err
	}
	v.bumpWriteCount()
	return t.commit()
}

// Rename moves/renames oldPath to newPath. Both parents must exist; newPath
// must not already exist. The data fork and CNID are preserved (catalog key
// change + thread parent/name update).
func (v *Volume) Rename(oldPath, newPath string) error {
	if !v.writable() {
		return ErrReadOnly
	}
	oparent, oname, err := v.resolveParent(oldPath)
	if err != nil {
		return err
	}
	nparent, nname, err := v.resolveParent(newPath)
	if err != nil {
		return err
	}
	rec, found, err := v.lookupChild(oparent, oname)
	if err != nil {
		return err
	}
	if !found {
		return fmt.Errorf("%w: %q", ErrNotFound, oname)
	}
	if _, exists, err := v.lookupChild(nparent, nname); err != nil {
		return err
	} else if exists {
		return fmt.Errorf("%w: %q", ErrExists, nname)
	}
	t, err := v.begin()
	if err != nil {
		return err
	}
	var cnid uint32
	var isDir bool
	switch {
	case rec.file != nil:
		cnid = rec.file.fileID
	case rec.folder != nil:
		cnid = rec.folder.folderID
		isDir = true
	default:
		return fmt.Errorf("%w: %q", ErrCorrupt, oname)
	}
	// Re-fetch the raw record body so we re-insert it verbatim under the new
	// key. We rebuild it from the decoded record by re-encoding the full leaf
	// record bytes captured via lookupRecordBytes.
	body, ok, err := v.recordBody(oparent, oname)
	if err != nil {
		return err
	}
	if !ok {
		return fmt.Errorf("%w: %q", ErrNotFound, oname)
	}
	// Delete old key + old thread, insert new key + new thread.
	if _, err := t.cw.deleteRecord(catalogKey{parentID: oparent, name: oname}); err != nil {
		return err
	}
	if _, err := t.cw.deleteRecord(catalogKey{parentID: cnid, name: ""}); err != nil {
		return err
	}
	newKey := encodeCatalogKey(nparent, nname)
	if err := t.cw.insertRecord(assembleCatalogRecord(newKey, body)); err != nil {
		return err
	}
	var threadRec []byte
	if isDir {
		threadRec = encodeFolderThreadRecord(nparent, nname)
	} else {
		threadRec = encodeFileThreadRecord(nparent, nname)
	}
	if err := t.cw.insertRecord(assembleCatalogRecord(encodeCatalogKey(cnid, ""), threadRec)); err != nil {
		return err
	}
	if oparent != nparent {
		var fd int32
		if isDir {
			fd = 1
		}
		if err := t.adjustParentValence(oparent, -1, -fd); err != nil {
			return err
		}
		if err := t.adjustParentValence(nparent, +1, +fd); err != nil {
			return err
		}
	}
	v.bumpWriteCount()
	return t.commit()
}

// recordBody returns the raw catalog record data (after the key) for
// (parent,name) so Rename can re-key it verbatim.
func (v *Volume) recordBody(parent uint32, name string) ([]byte, bool, error) {
	leaf, err := v.catalogTree.findLeaf(catalogKey{parentID: parent, name: name})
	if err != nil {
		return nil, false, err
	}
	for leaf != 0 {
		nd, err := v.catalogTree.readNode(leaf)
		if err != nil {
			return nil, false, err
		}
		for _, r := range nd.records {
			k, ok := keyFromRecord(r)
			if !ok {
				continue
			}
			if k.parentID == parent && v.catalogTree.nameEqual(k.name, name) {
				data, ok := recordData(r)
				if !ok {
					return nil, false, ErrCorrupt
				}
				return append([]byte(nil), data...), true, nil
			}
			if k.parentID > parent {
				return nil, false, nil
			}
		}
		leaf = nd.desc.FLink
	}
	return nil, false, nil
}

// --- optional capabilities ---

// Symlink creates a symbolic link at linkPath pointing at target. HFS+ stores
// the target as the data fork of an S_IFLNK file.
func (v *Volume) Symlink(target, linkPath string) error {
	if !v.writable() {
		return ErrReadOnly
	}
	parent, name, err := v.resolveParent(linkPath)
	if err != nil {
		return err
	}
	if _, found, err := v.lookupChild(parent, name); err != nil {
		return err
	} else if found {
		return fmt.Errorf("%w: %q", ErrExists, name)
	}
	t, err := v.begin()
	if err != nil {
		return err
	}
	mode := uint16(sIFLNK) | 0o777
	if err := t.createFile(parent, name, []byte(target), mode); err != nil {
		return err
	}
	v.bumpWriteCount()
	return t.commit()
}

// Truncate resizes the regular file at path to newSize bytes. Growing
// reallocates a larger contiguous run (zero-filled); shrinking reallocates a
// smaller run. Both rewrite the file's single inline extent.
func (v *Volume) Truncate(p string, newSize int64) error {
	if !v.writable() {
		return ErrReadOnly
	}
	if newSize < 0 {
		return fmt.Errorf("%w: negative size", ErrCorrupt)
	}
	parent, name, err := v.resolveParent(p)
	if err != nil {
		return err
	}
	rec, found, err := v.lookupChild(parent, name)
	if err != nil {
		return err
	}
	if !found {
		return fmt.Errorf("%w: %q", ErrNotFound, name)
	}
	if rec.file == nil {
		return fmt.Errorf("%w: %q", ErrNotRegular, name)
	}
	// Read existing content, resize, rewrite via overwrite.
	old, err := v.dataForkFor(rec.file)
	if err != nil {
		return err
	}
	content, err := old.readAll()
	if err != nil {
		return err
	}
	if int64(len(content)) > newSize {
		content = content[:newSize]
	} else if int64(len(content)) < newSize {
		content = append(content, make([]byte, newSize-int64(len(content)))...)
	}
	mode := rec.file.permMode
	if mode == 0 {
		mode = modeFileDefault
	}
	t, err := v.begin()
	if err != nil {
		return err
	}
	if err := t.removeFile(parent, name, rec.file); err != nil {
		return err
	}
	if err := t.commit(); err != nil {
		return err
	}
	if t, err = v.begin(); err != nil {
		return err
	}
	if err := t.createFile(parent, name, content, mode); err != nil {
		return err
	}
	v.bumpWriteCount()
	return t.commit()
}

// Label returns the volume label (the root folder's name in the catalog).
func (v *Volume) Label() string {
	// The root folder record is keyed (parent=1, name=label).
	leaf, err := v.catalogTree.findLeaf(catalogKey{parentID: cnidRootParent})
	if err != nil {
		return ""
	}
	for leaf != 0 {
		nd, err := v.catalogTree.readNode(leaf)
		if err != nil {
			return ""
		}
		for _, r := range nd.records {
			k, ok := keyFromRecord(r)
			if !ok {
				continue
			}
			if k.parentID == cnidRootParent {
				return k.name
			}
			if k.parentID > cnidRootParent {
				return ""
			}
		}
		leaf = nd.desc.FLink
	}
	return ""
}

// SetLabel renames the volume. The label lives as the root folder's catalog
// key (parent=1, name=label); SetLabel rewrites that key (and the root thread
// name) and is reflected by the reader and by macOS.
func (v *Volume) SetLabel(label string) error {
	if !v.writable() {
		return ErrReadOnly
	}
	label = strings.TrimSpace(label)
	if label == "" {
		return fmt.Errorf("%w: empty label", ErrCorrupt)
	}
	if utf16Len(label) > 255 {
		return fmt.Errorf("%w: label too long", ErrCorrupt)
	}
	old := v.Label()
	if old == label {
		return nil
	}
	t, err := v.begin()
	if err != nil {
		return err
	}
	// Fetch the root folder record body, re-key it, and rewrite the root
	// thread's nodeName.
	body, ok, err := v.recordBody(cnidRootParent, old)
	if err != nil {
		return err
	}
	if !ok {
		return fmt.Errorf("%w: root folder record missing", ErrCorrupt)
	}
	if _, err := t.cw.deleteRecord(catalogKey{parentID: cnidRootParent, name: old}); err != nil {
		return err
	}
	if _, err := t.cw.deleteRecord(catalogKey{parentID: cnidRootFolder, name: ""}); err != nil {
		return err
	}
	if err := t.cw.insertRecord(assembleCatalogRecord(encodeCatalogKey(cnidRootParent, label), body)); err != nil {
		return err
	}
	threadRec := encodeFolderThreadRecord(cnidRootParent, label)
	if err := t.cw.insertRecord(assembleCatalogRecord(encodeCatalogKey(cnidRootFolder, ""), threadRec)); err != nil {
		return err
	}
	v.bumpWriteCount()
	return t.commit()
}

func utf16Len(s string) int { return len(utf16.Encode([]rune(s))) }
