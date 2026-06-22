// Copyright (c) 2026, go-filesystems
// SPDX-License-Identifier: BSD-3-Clause

package hfsplus

import (
	"encoding/binary"
	"fmt"
)

// btmap.go maintains a B-tree's node-allocation bitmap. The bitmap starts in
// record 2 of the header node and continues, when it overflows, into linked
// map nodes (kind=2), each holding one record that is more bitmap. Bit N (MSB
// of byte 0 first) marks node N used.

// mapSpan returns, for the header node, the byte slice of record 2 (the first
// stretch of the node bitmap) within the live header-node image.
func (bw *btreeWriter) headerMapSlice() ([]byte, error) {
	b := bw.nodeBytes(0)
	if b == nil {
		return nil, fmt.Errorf("%w: header node unmapped", ErrCorrupt)
	}
	nrec := int(parseNodeDescriptor(b).NumRecords)
	if nrec < 3 {
		return nil, fmt.Errorf("%w: header node missing map record", ErrCorrupt)
	}
	// record 2 start, free-space ptr = offsets[nrec].
	rec2 := int(binary.BigEndian.Uint16(b[bw.nodeSize-2*3 : bw.nodeSize-2*3+2]))
	free := int(binary.BigEndian.Uint16(b[bw.nodeSize-2*(nrec+1) : bw.nodeSize-2*(nrec+1)+2]))
	if rec2 < nodeDescriptorLen || free > bw.nodeSize || rec2 > free {
		return nil, fmt.Errorf("%w: header map bounds", ErrCorrupt)
	}
	return b[rec2:free], nil
}

// mapNodeSlice returns the bitmap byte slice held in map node n (its single
// record). Map nodes carry one record: the offset table has 1 record + the
// free-space terminator, so the record runs from offsets[0] to offsets[1].
func (bw *btreeWriter) mapNodeSlice(n uint32) ([]byte, error) {
	b := bw.nodeBytes(n)
	if b == nil {
		return nil, fmt.Errorf("%w: map node %d unmapped", ErrCorrupt, n)
	}
	rec0 := int(binary.BigEndian.Uint16(b[bw.nodeSize-2 : bw.nodeSize]))
	rec1 := int(binary.BigEndian.Uint16(b[bw.nodeSize-4 : bw.nodeSize-2]))
	if rec0 < nodeDescriptorLen || rec1 > bw.nodeSize || rec0 > rec1 {
		return nil, fmt.Errorf("%w: map node %d bounds", ErrCorrupt, n)
	}
	return b[rec0:rec1], nil
}

// mapBitmap walks the header map record then the linked map-node chain,
// invoking fn for each stretch with the bit offset (number of bits preceding
// this stretch). fn may mutate the stretch. Stops when stop returns true.
func (bw *btreeWriter) walkMap(fn func(bitBase uint32, m []byte) (stop bool, err error)) error {
	m, err := bw.headerMapSlice()
	if err != nil {
		return err
	}
	bitBase := uint32(0)
	stop, err := fn(bitBase, m)
	if err != nil || stop {
		return err
	}
	bitBase += uint32(len(m)) * 8
	// Follow the map-node chain from the header node's FLink.
	hdr := parseNodeDescriptor(bw.nodeBytes(0))
	cur := hdr.FLink
	for cur != 0 {
		ms, err := bw.mapNodeSlice(cur)
		if err != nil {
			return err
		}
		stop, err := fn(bitBase, ms)
		if err != nil || stop {
			return err
		}
		bitBase += uint32(len(ms)) * 8
		cur = parseNodeDescriptor(bw.nodeBytes(cur)).FLink
	}
	return nil
}

// mapCapacityBits returns the total number of node bits the current bitmap
// (header record + map-node chain) can address.
func (bw *btreeWriter) mapCapacityBits() (uint32, error) {
	var cap uint32
	err := bw.walkMap(func(_ uint32, m []byte) (bool, error) {
		cap += uint32(len(m)) * 8
		return false, nil
	})
	return cap, err
}

// setNodeBit sets or clears node n's used bit, locating the right stretch.
func (bw *btreeWriter) setNodeBit(n uint32, used bool) error {
	found := false
	err := bw.walkMap(func(bitBase uint32, m []byte) (bool, error) {
		if n < bitBase || n >= bitBase+uint32(len(m))*8 {
			return false, nil
		}
		rel := n - bitBase
		if used {
			m[rel/8] |= 0x80 >> (rel % 8)
		} else {
			m[rel/8] &^= 0x80 >> (rel % 8)
		}
		found = true
		return true, nil
	})
	if err != nil {
		return err
	}
	if !found {
		return fmt.Errorf("%w: node %d beyond bitmap", ErrCorrupt, n)
	}
	return nil
}

// firstFreeNode scans the bitmap for the lowest free node number below
// totalNodes. ok is false when the bitmap is full.
func (bw *btreeWriter) firstFreeNode() (uint32, bool, error) {
	total := bw.totalNodes()
	var found uint32
	ok := false
	err := bw.walkMap(func(bitBase uint32, m []byte) (bool, error) {
		for i := 0; i < len(m)*8; i++ {
			n := bitBase + uint32(i)
			if n >= total {
				return true, nil
			}
			if m[i/8]&(0x80>>(i%8)) == 0 {
				found = n
				ok = true
				return true, nil
			}
		}
		return false, nil
	})
	return found, ok, err
}
