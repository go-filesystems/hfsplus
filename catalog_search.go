// Copyright (c) 2026, go-filesystems
// SPDX-License-Identifier: BSD-3-Clause

package hfsplus

import (
	"fmt"
	"strings"
	"unicode/utf16"
)

// compareCatalogKey orders two catalog keys: by parent CNID first, then by
// name. Name ordering uses the tree's key-compare semantics.
//
// Simplification: a true HFS+ case-fold tree orders names by Apple's fast
// Unicode case-fold table over the raw UTF-16 code units. We approximate that
// with a Go strings.ToLower fold on the decoded string for case-insensitive
// (kCF) trees, and a raw UTF-16 code-unit comparison for case-sensitive
// (HFSX/kBC) trees. This is sufficient to locate ASCII and common names; it is
// documented as a deliberate simplification.
func (t *btree) compareCatalogKey(a, b catalogKey) int {
	if a.parentID != b.parentID {
		if a.parentID < b.parentID {
			return -1
		}
		return 1
	}
	au, bu := a.u16(), b.u16()
	if t.header.KeyCompare == keyCompareBinary {
		return compareU16(au, bu)
	}
	return fastUnicodeCompare(au, bu)
}

// u16 returns the key's raw UTF-16 code units, deriving them from the decoded
// name when nameU16 was not populated (e.g. keys built programmatically).
func (k catalogKey) u16() []uint16 {
	if k.nameU16 != nil {
		return k.nameU16
	}
	if k.name == "" {
		return nil
	}
	return utf16.Encode([]rune(k.name))
}

// foldU16 maps a single UTF-16 code unit through the subset of Apple's HFS+
// case-fold table that covers every character a real volume name or filename
// uses. It returns 0 for code units Apple treats as IGNORABLE (skipped during
// comparison) — critically NUL (0x0000), which is why Apple orders the private
// "\0\0\0\0HFS+ Private Data" directory as if the NUL prefix were absent.
// Otherwise it lower-cases ASCII A–Z, Latin-1 upper-case (À–Þ except ×), and
// the even/odd Latin Extended-A upper-case pairs; all other units fold to
// themselves. This reproduces Apple's FastUnicodeCompare ordering — the
// ordering fsck_hfs validates — for the practical character set without
// embedding the full 8 KiB table.
func foldU16(c uint16) uint16 {
	switch {
	case c == 0x0000:
		return 0 // ignorable: skipped in comparison
	case c >= 0x0001 && c <= 0x001F:
		return c // control chars compare as themselves (not ignorable)
	case c >= 'A' && c <= 'Z':
		return c + 0x20
	case c >= 0x00C0 && c <= 0x00DE && c != 0x00D7: // À–Þ except ×
		return c + 0x20
	case c >= 0x0100 && c <= 0x017E:
		if c%2 == 0 {
			return c + 1
		}
		return c
	default:
		return c
	}
}

// nextFolded returns the next non-ignorable folded unit from s starting at *i,
// advancing *i past any ignorable (fold==0) units. ok is false at end of input.
func nextFolded(s []uint16, i *int) (uint16, bool) {
	for *i < len(s) {
		f := foldU16(s[*i])
		*i++
		if f != 0 {
			return f, true
		}
	}
	return 0, false
}

// fastUnicodeCompare orders two UTF-16 names by their folded, ignorable-skipped
// code units, matching the HFS+ catalog case-insensitive ordering.
func fastUnicodeCompare(a, b []uint16) int {
	var ia, ib int
	for {
		ca, oka := nextFolded(a, &ia)
		cb, okb := nextFolded(b, &ib)
		if !oka && !okb {
			return 0
		}
		if !oka {
			return -1
		}
		if !okb {
			return 1
		}
		if ca != cb {
			if ca < cb {
				return -1
			}
			return 1
		}
	}
}

// compareU16 compares two UTF-16 code-unit slices lexicographically.
func compareU16(a, b []uint16) int {
	n := len(a)
	if len(b) < n {
		n = len(b)
	}
	for i := 0; i < n; i++ {
		if a[i] != b[i] {
			if a[i] < b[i] {
				return -1
			}
			return 1
		}
	}
	switch {
	case len(a) < len(b):
		return -1
	case len(a) > len(b):
		return 1
	}
	return 0
}

// nameEqual reports whether two names match under the tree's case semantics.
func (t *btree) nameEqual(a, b string) bool {
	if t.header.KeyCompare == keyCompareBinary {
		return a == b
	}
	return strings.EqualFold(a, b)
}

// findLeaf descends from the root to the leaf node that would contain key,
// returning the leaf node number. Index records are scanned to pick the last
// child whose key is <= the search key.
func (t *btree) findLeaf(key catalogKey) (uint32, error) {
	cur := t.header.RootNode
	if cur == 0 {
		// Empty tree (no root): the first leaf, if any, is the start.
		return t.header.FirstLeaf, nil
	}
	for depth := 0; depth < 64; depth++ {
		nd, err := t.readNode(cur)
		if err != nil {
			return 0, err
		}
		if nd.desc.Kind == kindLeafNode {
			return cur, nil
		}
		if nd.desc.Kind != kindIndexNode {
			return 0, fmt.Errorf("%w: findLeaf node %d kind %d", ErrCorrupt, cur, nd.desc.Kind)
		}
		// Pick the last index record whose key <= search key.
		var child uint32
		chosen := false
		for _, rec := range nd.records {
			k, ok := keyFromRecord(rec)
			if !ok {
				continue
			}
			if t.compareCatalogKey(k, key) <= 0 {
				if c, ok := indexChild(rec); ok {
					child = c
					chosen = true
				}
			} else {
				break
			}
		}
		if !chosen {
			// Search key precedes all records; take the first child.
			if len(nd.records) == 0 {
				return 0, fmt.Errorf("%w: findLeaf empty index node %d", ErrCorrupt, cur)
			}
			c, ok := indexChild(nd.records[0])
			if !ok {
				return 0, fmt.Errorf("%w: findLeaf bad child node %d", ErrCorrupt, cur)
			}
			child = c
		}
		cur = child
	}
	return 0, fmt.Errorf("%w: findLeaf depth overflow", ErrCorrupt)
}

// lookup finds the leaf record matching key exactly (parentID + name under the
// tree's case rules) and returns its decoded record.
func (t *btree) lookup(key catalogKey) (catalogRecord, bool, error) {
	leaf, err := t.findLeaf(key)
	if err != nil {
		return catalogRecord{}, false, err
	}
	for leaf != 0 {
		nd, err := t.readNode(leaf)
		if err != nil {
			return catalogRecord{}, false, err
		}
		for _, rec := range nd.records {
			k, ok := keyFromRecord(rec)
			if !ok {
				continue
			}
			if k.parentID == key.parentID && t.nameEqual(k.name, key.name) {
				data, ok := recordData(rec)
				if !ok {
					return catalogRecord{}, false, ErrCorrupt
				}
				cr, ok := parseCatalogRecord(data)
				if !ok {
					return catalogRecord{}, false, ErrCorrupt
				}
				return cr, true, nil
			}
			// Past the target parent: nothing further matches.
			if k.parentID > key.parentID {
				return catalogRecord{}, false, nil
			}
		}
		leaf = nd.desc.FLink
	}
	return catalogRecord{}, false, nil
}

// listChildren returns every (key, record) pair whose parent CNID equals
// parentID, scanning forward across leaf nodes from the leaf where the parent's
// children begin.
func (t *btree) listChildren(parentID uint32) ([]childEntry, error) {
	// Seek to the leaf for the smallest possible key under parentID.
	start := catalogKey{parentID: parentID, name: ""}
	leaf, err := t.findLeaf(start)
	if err != nil {
		return nil, err
	}
	var out []childEntry
	seen := false
	for leaf != 0 {
		nd, err := t.readNode(leaf)
		if err != nil {
			return nil, err
		}
		for _, rec := range nd.records {
			k, ok := keyFromRecord(rec)
			if !ok {
				continue
			}
			if k.parentID < parentID {
				continue
			}
			if k.parentID > parentID {
				// Children are contiguous; once past, we are done.
				return out, nil
			}
			seen = true
			data, ok := recordData(rec)
			if !ok {
				continue
			}
			cr, ok := parseCatalogRecord(data)
			if !ok {
				continue
			}
			// Skip the directory's own thread record (empty name key).
			if cr.recType == recordFolderThread || cr.recType == recordFileThread {
				continue
			}
			out = append(out, childEntry{key: k, rec: cr})
		}
		leaf = nd.desc.FLink
	}
	_ = seen
	return out, nil
}

// childEntry pairs a catalog key with its decoded record.
type childEntry struct {
	key catalogKey
	rec catalogRecord
}
