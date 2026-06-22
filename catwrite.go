// Copyright (c) 2026, go-filesystems
// SPDX-License-Identifier: BSD-3-Clause

package hfsplus

import (
	"encoding/binary"
	"fmt"
)

// catwrite.go is the catalog B-tree write engine. It inserts and deletes
// catalog records (keys = parent CNID + UTF-16 name) on top of the shared
// generic B-tree machinery (btops.go / btdelete.go / btgrow.go), which handles
// leaf/index splitting, node-underflow rebalancing/merging, tree-height
// growth/shrink, and growing the catalog fork itself (allocating blocks,
// extending its extents into the extents-overflow tree, and enlarging the node
// bitmap) when the reservation is exhausted.

// catalogOps adapts the generic engine to catalog keys, carrying the tree so it
// can honour the case-fold vs binary key-compare type.
type catalogOps struct{ tree *btree }

func (o catalogOps) compareRecords(a, b []byte) int {
	ka, oka := keyFromRecord(a)
	kb, okb := keyFromRecord(b)
	if !oka || !okb {
		return 0
	}
	return o.tree.compareCatalogKey(ka, kb)
}

func (o catalogOps) indexRecord(firstKey []byte, child uint32) []byte {
	return indexRecordFor(firstKey, child)
}

// catWriter wraps a writable volume's catalog tree for mutation, delegating the
// B-tree mechanics to the generic btreeWriter.
type catWriter struct {
	v        *Volume
	tree     *btree
	bw       *btreeWriter
	nodeSize int
}

// newCatWriter builds a catalog writer over the volume's catalog fork. The fork
// may be fragmented (multi-extent) and may grow; the generic engine resolves
// its extents and grows it as needed.
func (v *Volume) newCatWriter(alc *allocator) (*catWriter, error) {
	bw, err := v.newBTreeWriter(v.catalogTree, forkCatalogFile, catalogOps{tree: v.catalogTree}, alc)
	if err != nil {
		return nil, err
	}
	return &catWriter{
		v:        v,
		tree:     v.catalogTree,
		bw:       bw,
		nodeSize: int(v.catalogTree.header.NodeSize),
	}, nil
}

// editNode is a node's records sliced out for editing.
type editNode struct {
	num     uint32
	desc    nodeDescriptor
	records [][]byte
}

// errNodeFull signals a node has no room; the engine splits.
var errNodeFull = fmt.Errorf("node full")

// --- key helpers ---

// indexRecordFor builds an index record: the child's first key + child node num.
func indexRecordFor(firstKey []byte, child uint32) []byte {
	rec := append([]byte(nil), firstKey...)
	if len(rec)%2 != 0 {
		rec = append(rec, 0)
	}
	var c [4]byte
	binary.BigEndian.PutUint32(c[:], child)
	rec = append(rec, c[:]...)
	return rec
}

// keyBytesOf extracts the full encoded key (with length prefix) from a record.
func keyBytesOf(rec []byte) []byte {
	keyLen, keyStart, ok := recordKeyLen(rec)
	if !ok {
		return nil
	}
	return rec[:keyStart+keyLen]
}

// --- catalog-level insert/delete ---

// insertRecord inserts a leaf record (key+data) into the catalog tree. The
// caller guarantees the key does not already exist.
func (cw *catWriter) insertRecord(rec []byte) error {
	if _, ok := keyFromRecord(rec); !ok {
		return fmt.Errorf("%w: bad record key on insert", ErrCorrupt)
	}
	return cw.bw.insert(rec)
}

// deleteRecord removes the leaf record matching key (parentID + name under the
// tree's case rules) with full node-underflow rebalancing. Returns whether a
// record was removed.
func (cw *catWriter) deleteRecord(key catalogKey) (bool, error) {
	removed := false
	probe := encodeCatalogKey(key.parentID, key.name)
	err := cw.bw.deleteByKey(probe, func(rec []byte) bool {
		k, ok := keyFromRecord(rec)
		if !ok {
			return false
		}
		if k.parentID == key.parentID && cw.tree.nameEqual(k.name, key.name) {
			removed = true
			return true
		}
		return false
	})
	return removed, err
}

// patchFolderRecord locates the folder record keyed (parent, name) and applies
// mutate to its data (the bytes after the key), re-storing the leaf node. Used
// to keep a parent folder's valence/folderCount in sync.
func (cw *catWriter) patchFolderRecord(parent uint32, name string, mutate func(folder []byte)) error {
	key := catalogKey{parentID: parent, name: name}
	probe := assembleProbe(encodeCatalogKey(parent, name))
	path, _, err := cw.bw.descend(probe)
	if err != nil {
		return err
	}
	leafNum := path[len(path)-1]
	en, err := cw.bw.loadNode(leafNum)
	if err != nil {
		return err
	}
	for i, r := range en.records {
		k, ok := keyFromRecord(r)
		if !ok {
			continue
		}
		if k.parentID == key.parentID && cw.tree.nameEqual(k.name, key.name) {
			data, ok := recordData(en.records[i])
			if !ok {
				return fmt.Errorf("%w: patchFolder recordData (%d,%q) leaf %d", ErrCorrupt, parent, name, leafNum)
			}
			mutate(data)
			return cw.bw.storeNode(en)
		}
	}
	return fmt.Errorf("%w: folder record (%d,%q) not found", ErrCorrupt, parent, name)
}
