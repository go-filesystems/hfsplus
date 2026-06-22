// Copyright (c) 2026, go-filesystems
// SPDX-License-Identifier: BSD-3-Clause

package hfsplus

import (
	"encoding/binary"
	"fmt"
	"sort"
)

// catwrite.go is the catalog B-tree write engine: insert and delete catalog
// records (keys = parent CNID + UTF-16 name) with leaf node splitting and
// index-record propagation, growing the tree height (new root) when the root
// splits. It edits the catalog fork's nodes directly inside the in-memory
// image and maintains the B-tree header node (rootNode, first/last leaf,
// treeDepth, freeNodes, leafRecords, node bitmap).
//
// The catalog fork is pre-sized by the formatter (catalogReserveNodes); the
// engine allocates new nodes from that reservation. Growing the catalog fork
// itself beyond the reservation is not implemented (returns ErrNoSpace) — a
// documented simplification suited to disk-image-sized volumes.

// catWriter wraps a writable volume's catalog tree for mutation. It reads and
// writes nodes by their byte offset within the catalog fork, which is a
// contiguous run on disk (the formatter lays the catalog file in one extent).
type catWriter struct {
	v        *Volume
	tree     *btree
	nodeSize int
	// forkBase is the absolute byte offset of catalog node 0 in the image, and
	// forkSpan its byte length. The formatter emits the catalog file as a
	// single contiguous extent, so node n lives at forkBase + n*nodeSize.
	forkBase int64
	forkSpan int64
}

// newCatWriter builds a catalog writer over the volume's catalog fork.
func (v *Volume) newCatWriter() (*catWriter, error) {
	fd := v.vh.CatalogFile
	// Require a single contiguous extent (the formatter guarantees this).
	first := fd.Extents[0]
	if first.BlockCount == 0 {
		return nil, fmt.Errorf("%w: catalog fork has no extents", ErrCorrupt)
	}
	bs := int64(v.vh.BlockSize)
	cw := &catWriter{
		v:        v,
		tree:     v.catalogTree,
		nodeSize: int(v.catalogTree.header.NodeSize),
		forkBase: int64(first.StartBlock) * bs,
		forkSpan: int64(fd.LogicalSize),
	}
	// Verify the catalog occupies one extent; multi-extent catalogs are not
	// mutated (documented).
	if fd.Extents[1].BlockCount != 0 {
		return nil, fmt.Errorf("%w: fragmented catalog not writable", ErrUnsupported)
	}
	return cw, nil
}

// node accessors operate on the live image bytes.
func (cw *catWriter) nodeBytes(n uint32) []byte {
	off := cw.forkBase + int64(n)*int64(cw.nodeSize)
	return cw.v.img[off : off+int64(cw.nodeSize)]
}

// editNode is a node's records sliced out for editing.
type editNode struct {
	num     uint32
	desc    nodeDescriptor
	records [][]byte
}

func (cw *catWriter) loadNode(n uint32) (*editNode, error) {
	b := cw.nodeBytes(n)
	desc := parseNodeDescriptor(b)
	nrec := int(desc.NumRecords)
	if nrec < 0 || (nrec+1)*2 > cw.nodeSize {
		return nil, fmt.Errorf("%w: catalog node %d bad reccount", ErrCorrupt, n)
	}
	offs := make([]int, nrec+1)
	for i := 0; i <= nrec; i++ {
		pos := cw.nodeSize - 2*(i+1)
		offs[i] = int(binary.BigEndian.Uint16(b[pos : pos+2]))
	}
	recs := make([][]byte, nrec)
	for i := 0; i < nrec; i++ {
		s, e := offs[i], offs[i+1]
		if s < nodeDescriptorLen || e > cw.nodeSize || s > e {
			return nil, fmt.Errorf("%w: catalog node %d rec %d bounds", ErrCorrupt, n, i)
		}
		recs[i] = append([]byte(nil), b[s:e]...)
	}
	return &editNode{num: n, desc: desc, records: recs}, nil
}

// storeNode serialises an editNode back into its image bytes, rebuilding the
// offset table. Returns an error if the records overflow the node.
func (cw *catWriter) storeNode(en *editNode) error {
	b := cw.nodeBytes(en.num)
	for i := range b {
		b[i] = 0
	}
	off := nodeDescriptorLen
	offsets := []uint16{uint16(off)}
	for _, r := range en.records {
		if off+len(r) > cw.nodeSize-2*(len(en.records)+1) {
			return fmt.Errorf("%w: catalog node %d overflow", errNodeFull, en.num)
		}
		copy(b[off:], r)
		off += len(r)
		offsets = append(offsets, uint16(off))
	}
	en.desc.NumRecords = uint16(len(en.records))
	writeNodeDescriptor(b, en.desc)
	writeOffsetTable(b, offsets)
	return nil
}

// errNodeFull signals a node has no room; the caller splits.
var errNodeFull = fmt.Errorf("node full")

// --- header node maintenance ---

type catHeader struct {
	b []byte // header node bytes (live)
}

func (cw *catWriter) header() *catHeader { return &catHeader{b: cw.nodeBytes(0)} }

func (h *catHeader) rec() []byte { return h.b[nodeDescriptorLen:] }

func (h *catHeader) treeDepth() uint16 { return binary.BigEndian.Uint16(h.rec()[0:2]) }
func (h *catHeader) setTreeDepth(d uint16) {
	binary.BigEndian.PutUint16(h.rec()[0:2], d)
}
func (h *catHeader) rootNode() uint32 { return binary.BigEndian.Uint32(h.rec()[2:6]) }
func (h *catHeader) setRootNode(n uint32) {
	binary.BigEndian.PutUint32(h.rec()[2:6], n)
}
func (h *catHeader) leafRecords() uint32 { return binary.BigEndian.Uint32(h.rec()[6:10]) }
func (h *catHeader) setLeafRecords(n uint32) {
	binary.BigEndian.PutUint32(h.rec()[6:10], n)
}
func (h *catHeader) setFirstLeaf(n uint32) { binary.BigEndian.PutUint32(h.rec()[10:14], n) }
func (h *catHeader) setLastLeaf(n uint32)  { binary.BigEndian.PutUint32(h.rec()[14:18], n) }
func (h *catHeader) totalNodes() uint32    { return binary.BigEndian.Uint32(h.rec()[22:26]) }
func (h *catHeader) freeNodes() uint32     { return binary.BigEndian.Uint32(h.rec()[26:30]) }
func (h *catHeader) setFreeNodes(n uint32) { binary.BigEndian.PutUint32(h.rec()[26:30], n) }

// mapRecord returns the node bitmap slice (record 2 of the header node).
func (cw *catWriter) mapRecord() ([]byte, error) {
	en, err := cw.loadNode(0)
	if err != nil {
		return nil, err
	}
	if len(en.records) < 3 {
		return nil, fmt.Errorf("%w: header node missing map record", ErrCorrupt)
	}
	// The live map bytes are in the node image; recompute its span from the
	// offset table rather than the copied record.
	b := cw.nodeBytes(0)
	nrec := int(parseNodeDescriptor(b).NumRecords)
	// record 2 start, free-space ptr = offsets[nrec]
	rec2 := int(binary.BigEndian.Uint16(b[cw.nodeSize-2*3 : cw.nodeSize-2*3+2]))
	free := int(binary.BigEndian.Uint16(b[cw.nodeSize-2*(nrec+1) : cw.nodeSize-2*(nrec+1)+2]))
	return b[rec2:free], nil
}

func (cw *catWriter) markNodeUsed(n uint32, used bool) error {
	m, err := cw.mapRecord()
	if err != nil {
		return err
	}
	if int(n/8) >= len(m) {
		return fmt.Errorf("%w: node %d beyond catalog map", ErrNoSpace, n)
	}
	if used {
		m[n/8] |= 0x80 >> (n % 8)
	} else {
		m[n/8] &^= 0x80 >> (n % 8)
	}
	return nil
}

// allocNode reserves a free node from the catalog reservation, marks it used,
// decrements freeNodes, and returns its number. The node bytes are zeroed.
func (cw *catWriter) allocNode() (uint32, error) {
	m, err := cw.mapRecord()
	if err != nil {
		return 0, err
	}
	h := cw.header()
	total := h.totalNodes()
	for n := uint32(0); n < total; n++ {
		if int(n/8) >= len(m) {
			break
		}
		if m[n/8]&(0x80>>(n%8)) == 0 {
			m[n/8] |= 0x80 >> (n % 8)
			h.setFreeNodes(h.freeNodes() - 1)
			b := cw.nodeBytes(n)
			for i := range b {
				b[i] = 0
			}
			return n, nil
		}
	}
	return 0, fmt.Errorf("%w: catalog tree full (raise catalogReserveNodes)", ErrNoSpace)
}

func (cw *catWriter) freeNode(n uint32) error {
	if err := cw.markNodeUsed(n, false); err != nil {
		return err
	}
	h := cw.header()
	h.setFreeNodes(h.freeNodes() + 1)
	return nil
}

// --- key helpers ---

// catKeyOf returns the catalog key of a record (parent + decoded name + raw
// u16) for ordering.
func catKeyOf(rec []byte) (catalogKey, bool) {
	return keyFromRecord(rec)
}

// indexRecordFor builds an index record: the leaf's first key + child node num.
func indexRecordFor(firstKey []byte, child uint32) []byte {
	// firstKey is the full encoded key (length prefix + body). Append the
	// 4-byte child pointer, padded to a 2-byte boundary after the key.
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

// --- insertion ---

// insertRecord inserts a leaf record (key+data) into the catalog tree. The
// caller guarantees the key does not already exist.
func (cw *catWriter) insertRecord(rec []byte) error {
	key, ok := catKeyOf(rec)
	if !ok {
		return fmt.Errorf("%w: bad record key on insert", ErrCorrupt)
	}
	h := cw.header()
	if h.rootNode() == 0 {
		// Empty tree: create a root leaf.
		leaf, err := cw.allocNode()
		if err != nil {
			return err
		}
		en := &editNode{num: leaf, desc: nodeDescriptor{Kind: kindLeafNode, Height: 1}, records: [][]byte{rec}}
		if err := cw.storeNode(en); err != nil {
			return err
		}
		h.setRootNode(leaf)
		h.setFirstLeaf(leaf)
		h.setLastLeaf(leaf)
		h.setTreeDepth(1)
		h.setLeafRecords(1)
		return nil
	}
	// Descend recording the path so we can propagate splits upward.
	path, err := cw.descendPath(key)
	if err != nil {
		return err
	}
	// Insert into the leaf (last element of path).
	splitKey, splitChild, err := cw.insertIntoLeaf(path[len(path)-1], rec, key)
	if err != nil {
		return err
	}
	h.setLeafRecords(h.leafRecords() + 1)
	if splitChild == 0 {
		return nil
	}
	if len(path) == 1 {
		// The root was itself a leaf and split: grow a new index root above
		// the old root leaf and its new sibling.
		return cw.growRoot(path[0], splitKey, splitChild)
	}
	// Propagate the split up the path.
	return cw.propagateSplit(path, splitKey, splitChild)
}

// descendPath returns the node numbers from root down to the target leaf.
func (cw *catWriter) descendPath(key catalogKey) ([]uint32, error) {
	cur := cw.header().rootNode()
	path := []uint32{}
	for depth := 0; depth < 64; depth++ {
		en, err := cw.loadNode(cur)
		if err != nil {
			return nil, err
		}
		path = append(path, cur)
		if en.desc.Kind == kindLeafNode {
			return path, nil
		}
		if en.desc.Kind != kindIndexNode {
			return nil, fmt.Errorf("%w: catalog descend hit kind %d", ErrCorrupt, en.desc.Kind)
		}
		// pick last child whose key <= search key
		var child uint32
		chosen := false
		for _, r := range en.records {
			k, ok := catKeyOf(r)
			if !ok {
				continue
			}
			if cw.tree.compareCatalogKey(k, key) <= 0 {
				if c, ok := indexChild(r); ok {
					child = c
					chosen = true
				}
			} else {
				break
			}
		}
		if !chosen {
			c, ok := indexChild(en.records[0])
			if !ok {
				return nil, ErrCorrupt
			}
			child = c
		}
		cur = child
	}
	return nil, ErrCorrupt
}

// insertIntoLeaf inserts rec into leaf node n in key order. If the node
// overflows it splits, returning the promoted (firstKeyOfNewNode, newNode);
// otherwise returns (nil,0).
func (cw *catWriter) insertIntoLeaf(n uint32, rec []byte, key catalogKey) ([]byte, uint32, error) {
	en, err := cw.loadNode(n)
	if err != nil {
		return nil, 0, err
	}
	cw.insertSorted(en, rec, key)
	if cw.recordsFit(en) {
		return nil, 0, cw.storeNode(en)
	}
	return cw.splitLeaf(en)
}

// insertSorted inserts rec into en.records keeping catalog-key order.
func (cw *catWriter) insertSorted(en *editNode, rec []byte, key catalogKey) {
	idx := sort.Search(len(en.records), func(i int) bool {
		k, _ := catKeyOf(en.records[i])
		return cw.tree.compareCatalogKey(k, key) >= 0
	})
	en.records = append(en.records, nil)
	copy(en.records[idx+1:], en.records[idx:])
	en.records[idx] = rec
}

// recordsFit reports whether en's records fit in a node.
func (cw *catWriter) recordsFit(en *editNode) bool {
	used := nodeDescriptorLen + 2*(len(en.records)+1)
	for _, r := range en.records {
		used += len(r)
	}
	return used <= cw.nodeSize
}

// splitLeaf splits a full leaf node into two, returning the first key of the
// new (right) node and its node number for promotion.
func (cw *catWriter) splitLeaf(en *editNode) ([]byte, uint32, error) {
	right, err := cw.allocNode()
	if err != nil {
		return nil, 0, err
	}
	mid := len(en.records) / 2
	rightRecs := append([][]byte(nil), en.records[mid:]...)
	en.records = en.records[:mid]

	// Link nodes: right takes en's old FLink; en -> right -> oldFLink.
	oldF := en.desc.FLink
	rightEN := &editNode{
		num:     right,
		desc:    nodeDescriptor{Kind: kindLeafNode, Height: 1, FLink: oldF, BLink: en.num},
		records: rightRecs,
	}
	en.desc.FLink = right
	// Fix the old forward neighbour's BLink.
	if oldF != 0 {
		if of, err := cw.loadNode(oldF); err == nil {
			of.desc.BLink = right
			if err := cw.storeNode(of); err != nil {
				return nil, 0, err
			}
		}
	} else {
		// en was the last leaf; right is now the last leaf.
		cw.header().setLastLeaf(right)
	}
	if err := cw.storeNode(en); err != nil {
		return nil, 0, err
	}
	if err := cw.storeNode(rightEN); err != nil {
		return nil, 0, err
	}
	return keyBytesOf(rightRecs[0]), right, nil
}

// propagateSplit inserts an index record for a freshly-split child up the
// path. path is root..leaf; the leaf already split. We walk back up inserting
// (splitKey -> splitChild) into the parent, splitting index nodes as needed,
// and growing a new root when the root itself splits.
func (cw *catWriter) propagateSplit(path []uint32, splitKey []byte, splitChild uint32) error {
	for level := len(path) - 2; level >= 0; level-- {
		parent := path[level]
		en, err := cw.loadNode(parent)
		if err != nil {
			return err
		}
		idxRec := indexRecordFor(splitKey, splitChild)
		k, _ := catKeyOf(idxRec)
		cw.insertSorted(en, idxRec, k)
		if cw.recordsFit(en) {
			return cw.storeNode(en)
		}
		// Split this index node.
		nk, nc, err := cw.splitIndex(en)
		if err != nil {
			return err
		}
		splitKey, splitChild = nk, nc
		if level == 0 {
			// Root split: grow a new root.
			return cw.growRoot(path[0], splitKey, splitChild)
		}
	}
	return nil
}

// splitIndex splits a full index node, returning the promoted key+child.
func (cw *catWriter) splitIndex(en *editNode) ([]byte, uint32, error) {
	right, err := cw.allocNode()
	if err != nil {
		return nil, 0, err
	}
	mid := len(en.records) / 2
	rightRecs := append([][]byte(nil), en.records[mid:]...)
	en.records = en.records[:mid]
	rightEN := &editNode{
		num:     right,
		desc:    nodeDescriptor{Kind: kindIndexNode, Height: en.desc.Height, FLink: en.desc.FLink, BLink: en.num},
		records: rightRecs,
	}
	en.desc.FLink = right
	if err := cw.storeNode(en); err != nil {
		return nil, 0, err
	}
	if err := cw.storeNode(rightEN); err != nil {
		return nil, 0, err
	}
	return keyBytesOf(rightRecs[0]), right, nil
}

// growRoot creates a new root index node above oldRoot referencing oldRoot and
// the newly-split sibling.
func (cw *catWriter) growRoot(oldRoot uint32, splitKey []byte, splitChild uint32) error {
	newRoot, err := cw.allocNode()
	if err != nil {
		return err
	}
	h := cw.header()
	// The new root's two index records: lowest-possible key -> oldRoot, and
	// splitKey -> splitChild. We synthesise a minimal key for the oldRoot
	// pointer using oldRoot's actual first key.
	oldEN, err := cw.loadNode(oldRoot)
	if err != nil {
		return err
	}
	oldFirstKey := keyBytesOf(oldEN.records[0])
	rec0 := indexRecordFor(oldFirstKey, oldRoot)
	rec1 := indexRecordFor(splitKey, splitChild)
	en := &editNode{
		num:     newRoot,
		desc:    nodeDescriptor{Kind: kindIndexNode, Height: uint8(h.treeDepth() + 1)},
		records: [][]byte{rec0, rec1},
	}
	if err := cw.storeNode(en); err != nil {
		return err
	}
	h.setRootNode(newRoot)
	h.setTreeDepth(h.treeDepth() + 1)
	return nil
}

// patchFolderRecord locates the folder record keyed (parent, name) and applies
// mutate to its data (the bytes after the key), re-storing the leaf node. Used
// to keep a parent folder's valence/folderCount in sync.
func (cw *catWriter) patchFolderRecord(parent uint32, name string, mutate func(folder []byte)) error {
	key := catalogKey{parentID: parent, name: name}
	en, leafNum, idx, err := cw.findLeafRecord(key)
	if err != nil {
		return err
	}
	if idx < 0 {
		return fmt.Errorf("%w: folder record (%d,%q) not found", ErrCorrupt, parent, name)
	}
	rec := en.records[idx]
	data, ok := recordData(rec)
	if !ok {
		return ErrCorrupt
	}
	mutate(data)
	_ = leafNum
	return cw.storeNode(en)
}

// findLeafRecord descends to the leaf for key and returns the loaded leaf, its
// node number, and the index of the matching record (or -1).
func (cw *catWriter) findLeafRecord(key catalogKey) (*editNode, uint32, int, error) {
	path, err := cw.descendPath(key)
	if err != nil {
		return nil, 0, 0, err
	}
	leafNum := path[len(path)-1]
	en, err := cw.loadNode(leafNum)
	if err != nil {
		return nil, 0, 0, err
	}
	for i, r := range en.records {
		k, ok := catKeyOf(r)
		if !ok {
			continue
		}
		if k.parentID == key.parentID && cw.tree.nameEqual(k.name, key.name) {
			return en, leafNum, i, nil
		}
	}
	return en, leafNum, -1, nil
}

// --- deletion ---

// deleteRecord removes the leaf record matching key. It does not merge
// underflowing nodes (a documented simplification): the record is removed, the
// leaf re-stored, and if the leaf becomes empty it is unlinked and freed and
// any parent index record pointing at it is removed. The tree stays
// fsck-clean because empty index/leaf nodes are not referenced.
func (cw *catWriter) deleteRecord(key catalogKey) (bool, error) {
	h := cw.header()
	if h.rootNode() == 0 {
		return false, nil
	}
	path, err := cw.descendPath(key)
	if err != nil {
		return false, err
	}
	leafNum := path[len(path)-1]
	en, err := cw.loadNode(leafNum)
	if err != nil {
		return false, err
	}
	idx := -1
	for i, r := range en.records {
		k, ok := catKeyOf(r)
		if !ok {
			continue
		}
		if k.parentID == key.parentID && cw.tree.nameEqual(k.name, key.name) {
			idx = i
			break
		}
	}
	if idx < 0 {
		return false, nil
	}
	en.records = append(en.records[:idx], en.records[idx+1:]...)
	h.setLeafRecords(h.leafRecords() - 1)
	if len(en.records) > 0 {
		return true, cw.storeNode(en)
	}
	// Leaf became empty: unlink and free it, and drop the parent's pointer.
	if err := cw.unlinkLeaf(en); err != nil {
		return false, err
	}
	if len(path) >= 2 {
		if err := cw.removeChildPointer(path, leafNum); err != nil {
			return false, err
		}
	} else {
		// The root leaf emptied: tree is now empty.
		h.setRootNode(0)
		h.setFirstLeaf(0)
		h.setLastLeaf(0)
		h.setTreeDepth(0)
	}
	return true, cw.freeNode(leafNum)
}

// unlinkLeaf fixes the FLink/BLink of an empty leaf's neighbours and the
// header's first/last leaf pointers.
func (cw *catWriter) unlinkLeaf(en *editNode) error {
	h := cw.header()
	if en.desc.BLink != 0 {
		if p, err := cw.loadNode(en.desc.BLink); err == nil {
			p.desc.FLink = en.desc.FLink
			if err := cw.storeNode(p); err != nil {
				return err
			}
		}
	} else {
		h.setFirstLeaf(en.desc.FLink)
	}
	if en.desc.FLink != 0 {
		if nfwd, err := cw.loadNode(en.desc.FLink); err == nil {
			nfwd.desc.BLink = en.desc.BLink
			if err := cw.storeNode(nfwd); err != nil {
				return err
			}
		}
	} else {
		h.setLastLeaf(en.desc.BLink)
	}
	return nil
}

// removeChildPointer drops the index record pointing at child from its parent
// (path[len-2]). If that index node empties, recurse up. The root collapses
// when it has a single child.
func (cw *catWriter) removeChildPointer(path []uint32, child uint32) error {
	for level := len(path) - 2; level >= 0; level-- {
		parent := path[level]
		en, err := cw.loadNode(parent)
		if err != nil {
			return err
		}
		idx := -1
		for i, r := range en.records {
			if c, ok := indexChild(r); ok && c == child {
				idx = i
				break
			}
		}
		if idx < 0 {
			return nil
		}
		en.records = append(en.records[:idx], en.records[idx+1:]...)
		h := cw.header()
		if len(en.records) >= 2 || (level == 0 && len(en.records) >= 1) {
			// Root may legitimately keep a single child only transiently; if a
			// non-root index has 1 child we collapse it below. Store and stop.
			if level == 0 && len(en.records) == 1 {
				// Root has a single child: make that child the new root.
				if only, ok := indexChild(en.records[0]); ok {
					h.setRootNode(only)
					h.setTreeDepth(h.treeDepth() - 1)
					return cw.freeNode(parent)
				}
			}
			return cw.storeNode(en)
		}
		// Index node emptied entirely (no records): free it and continue up.
		if len(en.records) == 0 {
			if level == 0 {
				h.setRootNode(0)
				h.setTreeDepth(0)
				return cw.freeNode(parent)
			}
			child = parent
			if err := cw.freeNode(parent); err != nil {
				return err
			}
			continue
		}
		return cw.storeNode(en)
	}
	return nil
}
