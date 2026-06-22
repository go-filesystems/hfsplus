// Copyright (c) 2026, go-filesystems
// SPDX-License-Identifier: BSD-3-Clause

package hfsplus

import (
	"encoding/binary"
	"fmt"
	"time"
	"unicode/utf16"
)

// mkfs.go builds a fresh, empty HFS+ (or HFSX) volume image entirely in Go —
// no macOS tooling. The produced image passes `fsck_hfs -n` clean and mounts
// read/write on macOS.
//
// Layout (block size = mkfsBlockSize, a power-of-two >= 512). All multi-byte
// fields are big-endian. The image is sized to a whole number of allocation
// blocks; the first 1024 bytes are the reserved boot area and the volume
// header lives at byte 1024 (always inside allocation block 0 when the block
// size is 4096). Allocation blocks, in order:
//
//	block 0   reserved boot area (1024 B) + volume header (512 B) + slack
//	block 1   allocation bitmap (the allocation file)
//	block 2   extents-overflow B-tree (header node + empty leaf)
//	block 3   catalog B-tree (header node + root leaf)
//	... data blocks ...
//	last      alternate volume header lives in the LAST 1024 bytes of the image
//	          (in HFS+ this is the second-to-last 512-byte sector); its
//	          allocation block is marked used too.
//
// The catalog root leaf holds the root-folder record + its thread record. The
// extents tree has an empty leaf. This is the minimal valid HFS+ skeleton.

// mkfsBlockSize is the allocation block size used by the pure-Go formatter.
// 4096 matches what macOS hdiutil picks for small HFS+ volumes and keeps the
// header comfortably inside block 0.
const mkfsBlockSize = 4096

// Node size used for the catalog and extents B-trees the formatter emits.
// 4096 == one allocation block, the value macOS uses for the catalog of small
// volumes (the extents tree usually uses a smaller node, but a uniform 4096 is
// valid and keeps the layout simple — fsck accepts it).
const mkfsNodeSize = 4096

// catalogReserveNodes is the number of catalog B-tree nodes the formatter
// pre-allocates (node 0 header + room for splits). With 4 KiB nodes this is
// 1 MiB of catalog space — several thousand entries before the fork would need
// to grow (growing the catalog fork is a documented unsupported simplification).
const catalogReserveNodes = 256

// firstUserCatalogID is the first CNID the formatter and write path hand out;
// macOS reserves CNIDs 0..15 for the volume's special files and private data.
const firstUserCatalogID = 16

// B-tree header attribute and signature constants used by the formatter.
const (
	hfsPlusSigVersion    = 4
	btreeBigKeysMask     = 0x00000002
	btreeVariableKeyMask = 0x00000004
)

// Volume attribute bits used by the formatter. macOS sets both
// kHFSVolumeUnmountedBit (bit 8) and the historical high "last-unmounted-
// cleanly" bit 31 on a cleanly-unmounted volume; fsck_hfs's VInfoChk wants
// bit 31 set or it reports a "Volume header needs minor repair (2, 0)".
const (
	kHFSVolumeUnmountedBit     = 0x00000100 // bit 8: cleanly unmounted
	kHFSVolumeUnmountedHighBit = 0x80000000 // bit 31: set by macOS on clean unmount
	mkfsCleanUnmountAttributes = kHFSVolumeUnmountedBit | kHFSVolumeUnmountedHighBit
)

// macEpochOffset converts a Unix time to the HFS+ epoch (1904-01-01) used by
// the volume header and catalog dates.
const macEpochOffset = 2082844800

func hfsTime(t time.Time) uint32 {
	return uint32(t.Unix() + macEpochOffset)
}

// Mkfs lays down a valid empty HFS+/HFSX volume of sizeBytes bytes into a
// freshly-allocated byte slice and returns it. Pure Go, big-endian, no host
// tooling — runs on every architecture. The returned image passes
// fsck_hfs -n on macOS and can be opened with Open/OpenWritable.
func Mkfs(sizeBytes int64, cfg FormatConfig) ([]byte, error) {
	bs := int64(mkfsBlockSize)
	if sizeBytes < bs*8 {
		return nil, fmt.Errorf("%w: image too small (need >= %d bytes)", ErrCorrupt, bs*8)
	}
	totalBlocks := sizeBytes / bs
	img := make([]byte, totalBlocks*bs)

	label := cfg.Label
	if label == "" {
		label = "GOTEST"
	}

	b := &mkfsBuilder{
		img:           img,
		blockSize:     uint32(bs),
		totalBlocks:   uint32(totalBlocks),
		nodeSize:      mkfsNodeSize,
		caseSensitive: cfg.CaseSensitive,
		label:         label,
		now:           time.Now(),
	}
	b.layout()
	b.writeAllocationBitmap()
	b.writeExtentsTree()
	b.writeCatalogTree()
	b.writeVolumeHeaders()
	return b.img, nil
}

// mkfsBuilder accumulates the on-disk layout decisions while emitting blocks.
type mkfsBuilder struct {
	img           []byte
	blockSize     uint32
	totalBlocks   uint32
	nodeSize      uint16
	caseSensitive bool
	label         string
	now           time.Time

	// fixed block assignments
	allocStart  uint32 // allocation bitmap
	allocBlocks uint32
	extStart    uint32 // extents-overflow B-tree
	extBlocks   uint32
	catStart    uint32 // catalog B-tree
	catBlocks   uint32

	nextCatalogID uint32
	usedBlocks    uint32
}

// blockBytes returns the slice for allocation block n.
func (b *mkfsBuilder) blockBytes(n uint32) []byte {
	off := int64(n) * int64(b.blockSize)
	return b.img[off : off+int64(b.blockSize)]
}

// layout assigns allocation blocks to the special files.
func (b *mkfsBuilder) layout() {
	bitmapBytes := (int64(b.totalBlocks) + 7) / 8
	bitmapBlocks := uint32((bitmapBytes + int64(b.blockSize) - 1) / int64(b.blockSize))
	if bitmapBlocks == 0 {
		bitmapBlocks = 1
	}
	b.allocStart = 1
	b.allocBlocks = bitmapBlocks

	b.extStart = b.allocStart + b.allocBlocks
	b.extBlocks = uint32(int64(b.nodeSize) * 2 / int64(b.blockSize)) // header + empty leaf
	if b.extBlocks == 0 {
		b.extBlocks = 1
	}

	b.catStart = b.extStart + b.extBlocks
	// Reserve catalogReserveNodes nodes for the catalog so the write path can
	// split leaves and grow the tree height without having to grow the catalog
	// fork itself (which would require relocating it). Growing past this
	// reservation returns ErrNoSpace — a documented simplification.
	catNodeBytes := int64(b.nodeSize) * catalogReserveNodes
	b.catBlocks = uint32((catNodeBytes + int64(b.blockSize) - 1) / int64(b.blockSize))
	if b.catBlocks == 0 {
		b.catBlocks = 1
	}

	b.nextCatalogID = firstUserCatalogID
}

// markUsed sets bits [start, start+count) in the bitmap held in the allocation
// blocks.
func (b *mkfsBuilder) markUsed(bitmap []byte, start, count uint32) {
	for i := start; i < start+count; i++ {
		bitmap[i/8] |= 0x80 >> (i % 8)
		b.usedBlocks++
	}
}

func (b *mkfsBuilder) writeAllocationBitmap() {
	bitmap := make([]byte, int64(b.allocBlocks)*int64(b.blockSize))
	// block 0 (boot+header)
	b.markUsed(bitmap, 0, 1)
	b.markUsed(bitmap, b.allocStart, b.allocBlocks)
	b.markUsed(bitmap, b.extStart, b.extBlocks)
	b.markUsed(bitmap, b.catStart, b.catBlocks)
	// The alternate volume header lives in the last 1024 bytes of the image,
	// i.e. the final allocation block. Mark it used.
	b.markUsed(bitmap, b.totalBlocks-1, 1)
	copy(b.img[int64(b.allocStart)*int64(b.blockSize):], bitmap)
}

// writeExtentsTree emits an extents-overflow B-tree with a header node and a
// single empty leaf node (treeDepth 0, no records).
func (b *mkfsBuilder) writeExtentsTree() {
	ns := int(b.nodeSize)
	base := b.blockBytes(b.extStart)
	// Header node (node 0).
	b.writeBTHeaderNode(base[:ns], extentsTreeParams{
		nodeSize:     b.nodeSize,
		totalNodes:   uint32(int64(b.extBlocks) * int64(b.blockSize) / int64(b.nodeSize)),
		rootNode:     0, // empty tree: no root
		firstLeaf:    0,
		lastLeaf:     0,
		treeDepth:    0,
		maxKeyLength: 10, // HFSPlusExtentKey keyLength
		keyCompare:   0,
		attributes:   btreeBigKeysMask,
		usedNodes:    1, // just the header node; leaf is free initially
	})
	// We do not pre-allocate a leaf node for the empty extents tree; an
	// empty tree (rootNode==0, leafRecords==0) is valid. The reader treats a
	// missing/empty extents tree gracefully.
}

// writeCatalogTree emits a catalog B-tree: header node (node 0) plus a single
// leaf node (node 1) that is the tree root and holds the root-folder record
// and its thread record.
func (b *mkfsBuilder) writeCatalogTree() {
	ns := int(b.nodeSize)
	base := b.blockBytes(b.catStart)
	nodesPerTree := uint32(int64(b.catBlocks) * int64(b.blockSize) / int64(b.nodeSize))

	keyCompare := uint8(keyCompareCaseFold)
	if b.caseSensitive {
		keyCompare = keyCompareBinary
	}

	// Build the leaf (node 1) records: root thread (key parent=1,name="") then
	// the root folder record (key parent=1, name=label). Order: by catalog key
	// (parent then name); the thread key has an empty name so it sorts first.
	leaf := base[ns : 2*ns]
	b.buildCatalogRootLeaf(leaf)

	// Header node (node 0).
	b.writeBTHeaderNode(base[:ns], extentsTreeParams{
		nodeSize:     b.nodeSize,
		totalNodes:   nodesPerTree,
		rootNode:     1,
		firstLeaf:    1,
		lastLeaf:     1,
		treeDepth:    1,
		maxKeyLength: 516, // kHFSPlusCatalogKeyMaximumLength
		keyCompare:   keyCompare,
		attributes:   btreeBigKeysMask | btreeVariableKeyMask,
		usedNodes:    2, // header + the single leaf
	})
}

// buildCatalogRootLeaf writes the root folder + root thread records into a leaf
// node buffer (already zeroed). Records are laid out front-to-back; the offset
// table grows back-to-front.
func (b *mkfsBuilder) buildCatalogRootLeaf(leaf []byte) {
	ns := len(leaf)

	threadRec := encodeFolderThreadRecord(cnidRootParent, b.label)
	folderRec := encodeFolderRecord(cnidRootFolder, b.now, 0)
	if b.caseSensitive {
		// HFSX (version 5) volumes must carry the kHFSHasFolderCountMask flag
		// and a folderCount field; fsck_hfs reports a corruption otherwise.
		setHasFolderCount(folderRec, 0)
	}
	threadKey := encodeCatalogKey(cnidRootFolder, "") // thread keyed by its own CNID, empty name
	folderKey := encodeCatalogKey(cnidRootParent, b.label)

	// record 0: folder thread keyed (parent=cnidRootFolder, "")
	rec0 := assembleCatalogRecord(threadKey, threadRec)
	// record 1: folder keyed (parent=1, label)
	rec1 := assembleCatalogRecord(folderKey, folderRec)

	// Ordering within the leaf is by catalog key. Both share nothing; the
	// thread (parent=2) and the folder (parent=1): parent 1 < parent 2, so the
	// folder record sorts BEFORE the thread record.
	records := [][]byte{rec1, rec0}

	off := nodeDescriptorLen
	offsets := []uint16{uint16(off)}
	for _, r := range records {
		copy(leaf[off:], r)
		off += len(r)
		offsets = append(offsets, uint16(off))
	}
	// Node descriptor.
	writeNodeDescriptor(leaf, nodeDescriptor{
		FLink:      0,
		BLink:      0,
		Kind:       kindLeafNode,
		Height:     1,
		NumRecords: uint16(len(records)),
	})
	// Offset table: stored last-record-first at the end of the node.
	writeOffsetTable(leaf[:ns], offsets)
}

// extentsTreeParams carries the BTHeaderRec values for writeBTHeaderNode.
type extentsTreeParams struct {
	nodeSize     uint16
	totalNodes   uint32
	rootNode     uint32
	firstLeaf    uint32
	lastLeaf     uint32
	treeDepth    uint16
	maxKeyLength uint16
	keyCompare   uint8
	attributes   uint32
	usedNodes    uint32
}

// writeBTHeaderNode writes the B-tree header node (node 0): node descriptor,
// the 106-byte BTHeaderRec, a 128-byte user-data record, and the node bitmap
// (mapRecord) marking which nodes are in use. Three records.
func (b *mkfsBuilder) writeBTHeaderNode(node []byte, p extentsTreeParams) {
	ns := len(node)
	freeNodes := p.totalNodes - p.usedNodes

	// BTHeaderRec at offset 14.
	h := node[nodeDescriptorLen:]
	binary.BigEndian.PutUint16(h[0:2], uint16(p.treeDepth)) // treeDepth
	binary.BigEndian.PutUint32(h[2:6], p.rootNode)          // rootNode
	binary.BigEndian.PutUint32(h[6:10], leafRecordCount(p)) // leafRecords
	binary.BigEndian.PutUint32(h[10:14], p.firstLeaf)       // firstLeafNode
	binary.BigEndian.PutUint32(h[14:18], p.lastLeaf)        // lastLeafNode
	binary.BigEndian.PutUint16(h[18:20], p.nodeSize)        // nodeSize
	binary.BigEndian.PutUint16(h[20:22], p.maxKeyLength)    // maxKeyLength
	binary.BigEndian.PutUint32(h[22:26], p.totalNodes)      // totalNodes
	binary.BigEndian.PutUint32(h[26:30], freeNodes)         // freeNodes
	// reserved1 (30,2)
	binary.BigEndian.PutUint32(h[32:36], uint32(b.blockSize)) // clumpSize
	h[36] = 0                                                 // btreeType = kHFSBTreeType (0)
	h[37] = p.keyCompare                                      // keyCompareType
	binary.BigEndian.PutUint32(h[38:42], p.attributes)        // attributes
	// reserved (42..106) left zero (16 uint32 reserved)

	// Node descriptor (3 records: header, user-data, map).
	writeNodeDescriptor(node, nodeDescriptor{
		Kind:       kindHeaderNode,
		Height:     0,
		NumRecords: 3,
	})

	// Records: [0]=BTHeaderRec(106 @14), [1]=userData(128 @120), [2]=map(rest).
	const hdrRecLen = 106
	const userDataLen = 128
	rec0 := uint16(nodeDescriptorLen)
	rec1 := uint16(nodeDescriptorLen + hdrRecLen)
	rec2 := uint16(nodeDescriptorLen + hdrRecLen + userDataLen)
	// map record runs up to the start of the offset table.
	freeStart := ns - 2*4 // 4 offsets (3 records + 1 terminator)
	mapRec := node[rec2:freeStart]
	// Mark used nodes in the node bitmap (big-endian bit order: node 0 = MSB
	// of byte 0).
	for n := uint32(0); n < p.usedNodes; n++ {
		mapRec[n/8] |= 0x80 >> (n % 8)
	}
	writeOffsetTable(node, []uint16{rec0, rec1, rec2, uint16(freeStart)})
}

// leafRecordCount returns the number of leaf records for the tree params: the
// catalog root tree has 2 (folder + thread); the empty extents tree has 0.
func leafRecordCount(p extentsTreeParams) uint32 {
	if p.treeDepth == 0 {
		return 0
	}
	return 2
}

// writeVolumeHeaders writes the primary volume header at offset 1024 and the
// alternate volume header at (size-1024).
func (b *mkfsBuilder) writeVolumeHeaders() {
	vh := make([]byte, 512)
	b.encodeVolumeHeader(vh)
	copy(b.img[volumeHeaderOffset:volumeHeaderOffset+512], vh)
	// Alternate volume header: last 1024 bytes hold it in the first 512 of
	// that 1024-byte region.
	altOff := int64(len(b.img)) - 1024
	copy(b.img[altOff:altOff+512], vh)
}

// encodeVolumeHeader fills a 512-byte volume header buffer.
func (b *mkfsBuilder) encodeVolumeHeader(vh []byte) {
	sig := uint16(sigHFSPlus)
	if b.caseSensitive {
		sig = sigHFSX
	}
	binary.BigEndian.PutUint16(vh[0:2], sig)
	binary.BigEndian.PutUint16(vh[2:4], hfsPlusSigVersion) // version 4 (HFSX uses 5, but 4 is accepted; set 5 for HFSX)
	if b.caseSensitive {
		binary.BigEndian.PutUint16(vh[2:4], 5)
	}
	binary.BigEndian.PutUint32(vh[4:8], mkfsCleanUnmountAttributes) // attributes: cleanly unmounted
	// lastMountedVersion (8,4) = "10.0" packed; use "8.10" style ascii "HFSJ"?
	copy(vh[8:12], []byte("GOFS"))
	// journalInfoBlock (12,4) = 0 (no journal)

	now := hfsTime(b.now)
	binary.BigEndian.PutUint32(vh[16:20], now) // createDate (local; we use UTC, fsck tolerant)
	binary.BigEndian.PutUint32(vh[20:24], now) // modifyDate
	binary.BigEndian.PutUint32(vh[24:28], 0)   // backupDate
	binary.BigEndian.PutUint32(vh[28:32], 0)   // checkedDate

	binary.BigEndian.PutUint32(vh[32:36], 0)             // fileCount (no user files)
	binary.BigEndian.PutUint32(vh[36:40], 0)             // folderCount (root not counted)
	binary.BigEndian.PutUint32(vh[40:44], b.blockSize)   // blockSize
	binary.BigEndian.PutUint32(vh[44:48], b.totalBlocks) // totalBlocks
	freeBlocks := b.totalBlocks - b.usedBlocks
	binary.BigEndian.PutUint32(vh[48:52], freeBlocks)             // freeBlocks
	binary.BigEndian.PutUint32(vh[52:56], b.catStart+b.catBlocks) // nextAllocation hint (first free block)
	binary.BigEndian.PutUint32(vh[56:60], b.blockSize)            // rsrcClumpSize
	binary.BigEndian.PutUint32(vh[60:64], b.blockSize)            // dataClumpSize
	binary.BigEndian.PutUint32(vh[64:68], b.nextCatalogID)        // nextCatalogID
	binary.BigEndian.PutUint32(vh[68:72], 0)                      // writeCount
	// encodingsBitmap (72,8): mark MacRoman (bit 0).
	binary.BigEndian.PutUint64(vh[72:80], 1)

	// finderInfo[8] (80..112) left zero.

	// Special-file fork descriptors begin at offset 112 (0x70).
	const specialBase = 0x70
	// allocationFile
	b.encodeForkDescriptor(vh[specialBase+0*forkDataLen:], int64(b.allocBlocks)*int64(b.blockSize), b.allocStart, b.allocBlocks)
	// extentsFile
	b.encodeForkDescriptor(vh[specialBase+1*forkDataLen:], int64(b.extBlocks)*int64(b.blockSize), b.extStart, b.extBlocks)
	// catalogFile
	b.encodeForkDescriptor(vh[specialBase+2*forkDataLen:], int64(b.catBlocks)*int64(b.blockSize), b.catStart, b.catBlocks)
	// attributesFile: empty
	b.encodeForkDescriptor(vh[specialBase+3*forkDataLen:], 0, 0, 0)
	// startupFile: empty
	b.encodeForkDescriptor(vh[specialBase+4*forkDataLen:], 0, 0, 0)
}

// encodeForkDescriptor writes a HFSPlusForkData (80 bytes) with a single inline
// extent (startBlock,blockCount) covering the file.
func (b *mkfsBuilder) encodeForkDescriptor(dst []byte, logicalSize int64, startBlock, blockCount uint32) {
	binary.BigEndian.PutUint64(dst[0:8], uint64(logicalSize)) // logicalSize
	binary.BigEndian.PutUint32(dst[8:12], b.blockSize)        // clumpSize
	binary.BigEndian.PutUint32(dst[12:16], blockCount)        // totalBlocks
	off := 16
	if blockCount > 0 {
		binary.BigEndian.PutUint32(dst[off:off+4], startBlock)
		binary.BigEndian.PutUint32(dst[off+4:off+8], blockCount)
	}
	// remaining 7 extents zero
}

// ---- catalog record encoders ----

// encodeCatalogKey encodes an HFSPlusCatalogKey (without the outer record
// length prefix): keyLength(2) parentID(4) nodeName(HFSUniStr255).
func encodeCatalogKey(parent uint32, name string) []byte {
	u16 := utf16.Encode([]rune(name))
	// keyLength counts bytes AFTER the keyLength field: parentID(4) +
	// nameLength(2) + name(2*len).
	keyLen := 4 + 2 + 2*len(u16)
	buf := make([]byte, 2+keyLen)
	binary.BigEndian.PutUint16(buf[0:2], uint16(keyLen))
	binary.BigEndian.PutUint32(buf[2:6], parent)
	binary.BigEndian.PutUint16(buf[6:8], uint16(len(u16)))
	off := 8
	for _, u := range u16 {
		binary.BigEndian.PutUint16(buf[off:off+2], u)
		off += 2
	}
	return buf
}

// assembleCatalogRecord concatenates an encoded key (with its length prefix)
// and the record data, padding so the data starts on a 2-byte boundary (it
// already does because keys are even-length).
func assembleCatalogRecord(key, data []byte) []byte {
	rec := make([]byte, 0, len(key)+len(data))
	rec = append(rec, key...)
	if len(rec)%2 != 0 {
		rec = append(rec, 0)
	}
	rec = append(rec, data...)
	return rec
}

// encodeFolderRecord encodes an HFSPlusCatalogFolder.
func encodeFolderRecord(folderID uint32, t time.Time, valence uint32) []byte {
	buf := make([]byte, catalogFolderLen)
	binary.BigEndian.PutUint16(buf[0:2], recordFolder)
	binary.BigEndian.PutUint16(buf[2:4], 0)         // flags
	binary.BigEndian.PutUint32(buf[4:8], valence)   // valence
	binary.BigEndian.PutUint32(buf[8:12], folderID) // folderID
	ht := hfsTime(t)
	binary.BigEndian.PutUint32(buf[12:16], ht) // createDate
	binary.BigEndian.PutUint32(buf[16:20], ht) // contentModDate
	binary.BigEndian.PutUint32(buf[20:24], ht) // attributeModDate
	binary.BigEndian.PutUint32(buf[24:28], 0)  // accessDate
	binary.BigEndian.PutUint32(buf[28:32], ht) // backupDate
	// HFSPlusBSDInfo at offset 32: ownerID(4) groupID(4) adminFlags(1)
	// ownerFlags(1) fileMode(2) special(4)
	binary.BigEndian.PutUint16(buf[42:44], sIFDIR|0o755) // fileMode
	// userInfo(48,16) finderInfo(64,16) textEncoding(80,4) reserved(84,4)
	return buf
}

// encodeFileRecord encodes an HFSPlusCatalogFile with the given mode and data
// fork.
func encodeFileRecord(fileID uint32, t time.Time, mode uint16, df forkData) []byte {
	buf := make([]byte, catalogFileLen)
	binary.BigEndian.PutUint16(buf[0:2], recordFile)
	binary.BigEndian.PutUint16(buf[2:4], kHFSThreadExistsMask) // flags: thread exists
	binary.BigEndian.PutUint32(buf[8:12], fileID)
	ht := hfsTime(t)
	binary.BigEndian.PutUint32(buf[12:16], ht)
	binary.BigEndian.PutUint32(buf[16:20], ht)
	binary.BigEndian.PutUint32(buf[20:24], ht)
	binary.BigEndian.PutUint32(buf[24:28], 0)
	binary.BigEndian.PutUint32(buf[28:32], ht)
	binary.BigEndian.PutUint16(buf[42:44], mode) // fileMode
	encodeForkInto(buf[88:88+forkDataLen], df)
	// resource fork at 168 left zero
	return buf
}

// kHFSThreadExistsMask marks a file/folder catalog record that has a thread
// record (required for files in HFS+ so they can be looked up by CNID).
const kHFSThreadExistsMask = 0x0002

// kHFSHasFolderCountMask marks a HFSPlusCatalogFolder that carries a valid
// folderCount field (offset 84). HFSX (version 5) volumes require it.
const kHFSHasFolderCountMask = 0x0010

// setHasFolderCount sets the HasFolderCount flag bit and writes folderCount at
// offset 84 in an 88-byte HFSPlusCatalogFolder record.
func setHasFolderCount(folder []byte, count uint32) {
	flags := binary.BigEndian.Uint16(folder[2:4])
	binary.BigEndian.PutUint16(folder[2:4], flags|kHFSHasFolderCountMask)
	binary.BigEndian.PutUint32(folder[84:88], count)
}

// encodeForkInto serialises a forkData into an 80-byte buffer.
func encodeForkInto(dst []byte, f forkData) {
	binary.BigEndian.PutUint64(dst[0:8], f.LogicalSize)
	binary.BigEndian.PutUint32(dst[8:12], f.ClumpSize)
	binary.BigEndian.PutUint32(dst[12:16], f.TotalBlocks)
	off := 16
	for i := 0; i < numInlineExtents; i++ {
		binary.BigEndian.PutUint32(dst[off:off+4], f.Extents[i].StartBlock)
		binary.BigEndian.PutUint32(dst[off+4:off+8], f.Extents[i].BlockCount)
		off += 8
	}
}

// encodeFolderThreadRecord / encodeFileThreadRecord encode HFSPlusCatalogThread.
func encodeFolderThreadRecord(parent uint32, name string) []byte {
	return encodeThreadRecord(recordFolderThread, parent, name)
}

func encodeFileThreadRecord(parent uint32, name string) []byte {
	return encodeThreadRecord(recordFileThread, parent, name)
}

func encodeThreadRecord(recType int16, parent uint32, name string) []byte {
	u16 := utf16.Encode([]rune(name))
	buf := make([]byte, 10+2*len(u16))
	binary.BigEndian.PutUint16(buf[0:2], uint16(recType))
	// reserved (2,2)
	binary.BigEndian.PutUint32(buf[4:8], parent)
	binary.BigEndian.PutUint16(buf[8:10], uint16(len(u16)))
	off := 10
	for _, u := range u16 {
		binary.BigEndian.PutUint16(buf[off:off+2], u)
		off += 2
	}
	return buf
}

// catalogFolderLen / catalogFileLen are the on-disk record sizes.
const (
	catalogFolderLen = 88
	catalogFileLen   = 248 // 88 + dataFork(80) + rsrcFork(80) = 248
)

// ---- node descriptor / offset table writers ----

func writeNodeDescriptor(node []byte, d nodeDescriptor) {
	binary.BigEndian.PutUint32(node[0:4], d.FLink)
	binary.BigEndian.PutUint32(node[4:8], d.BLink)
	node[8] = byte(d.Kind)
	node[9] = d.Height
	binary.BigEndian.PutUint16(node[10:12], d.NumRecords)
	// reserved (12,2)
}

// writeOffsetTable writes the trailing record-offset table. offsets has
// numRecords+1 entries (record starts plus the free-space pointer); they are
// stored last-entry-first from the end of the node.
func writeOffsetTable(node []byte, offsets []uint16) {
	ns := len(node)
	for i, off := range offsets {
		pos := ns - 2*(i+1)
		binary.BigEndian.PutUint16(node[pos:pos+2], off)
	}
}
