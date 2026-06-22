// Copyright (c) 2026, go-filesystems
// SPDX-License-Identifier: BSD-3-Clause

// Package hfsplus is a pure-Go, CGO-free read/write driver for the HFS+ (Mac
// OS Extended) on-disk format, including its HFSX (case-sensitive) variant.
//
// HFS+ stores every multi-byte field big-endian. The volume header lives at
// byte offset 1024 and carries the block size, block counts, and the special
// fork descriptors for the catalog, extents-overflow, and allocation files.
// File and directory metadata live in the catalog B-tree, keyed by the parent
// CNID plus the UTF-16 node name; file contents are addressed by allocation
// blocks via up to eight inline extents per fork, spilling into the
// extents-overflow B-tree for fragmented files.
//
// The package implements the full shared github.com/go-filesystems/interface
// Filesystem contract:
//
//   - Open / OpenFile open a volume read-only.
//   - OpenWritable / OpenFileWritable open it for read/write; the whole image
//     is held in memory, mutated in place, and flushed by Sync.
//   - Format (and the lower-level Mkfs) lay down a fresh, empty HFS+/HFSX
//     volume in pure Go — no host tooling — that passes fsck_hfs -n clean and
//     mounts read/write on macOS.
//   - WriteFile, MkDir, DeleteFile, DeleteDir, Rename mutate the catalog
//     B-tree (insert/delete with node splitting and tree-height growth),
//     manage the allocation bitmap, and keep the volume-header counters in
//     sync. The optional Labeller (SetLabel), Symlinker (Symlink), and
//     Truncater (Truncate) capabilities are implemented too.
//
// Every write path is validated against the native macOS tooling: fsck_hfs -n
// reports the volume clean and macOS mounts the image read/write and reads the
// exact files and bytes the Go side wrote, in both directions (Go-formatted →
// macOS-read and macOS-created → Go-written → macOS-read). The cross-arch,
// big-endian (s390x) round-trip runs in pure Go on every architecture.
//
// Case-folding: case-insensitive name comparison implements Apple's
// FastUnicodeCompare for the practical character set (ASCII, Latin-1, Latin
// Extended-A, and the ignorable-NUL handling fsck requires) rather than
// embedding the full 8 KiB fold table; exotic case-folding corner cases
// outside that range fall back to identity ordering.
//
// Documented simplifications (kept honest, like the btrfs/xfs siblings):
//
//   - A file's data fork is stored as one contiguous run using the inline
//     extents; the extents-overflow insert path (>8 fragments) is not
//     implemented — such a write returns ErrUnsupported. Reading fragmented
//     forks via the extents-overflow tree is fully supported.
//   - Deletion frees the record/thread and its blocks but does not strictly
//     re-merge underflowing B-tree nodes; the tree stays fsck-clean.
//   - The catalog fork is pre-sized by the formatter; growing it past that
//     reservation (a fragmented/relocated catalog) returns ErrNoSpace.
//   - Journaling, decmpfs compression, resource forks, and indirect-node
//     hardlink following are not implemented; see the README Status section.
package hfsplus

import "errors"

// Sentinel errors. Compare with errors.Is so wrapped errors keep matching.
var (
	// ErrReadOnly is returned by every mutating method when the volume was
	// opened read-only (Open / OpenFile). Open it writable (OpenWritable /
	// OpenFileWritable / Format) to mutate it.
	ErrReadOnly = errors.New("hfsplus: filesystem is read-only")

	// ErrBadHeader is returned when the volume header at offset 1024 lacks a
	// recognized HFS+ ("H+") or HFSX ("HX") signature.
	ErrBadHeader = errors.New("hfsplus: no valid volume header")

	// ErrNotFound is returned when a path component cannot be located in the
	// catalog.
	ErrNotFound = errors.New("hfsplus: path not found")

	// ErrNotDirectory is returned when ListDir targets a non-directory.
	ErrNotDirectory = errors.New("hfsplus: not a directory")

	// ErrNotRegular is returned when ReadFile targets a non-regular file.
	ErrNotRegular = errors.New("hfsplus: not a regular file")

	// ErrNotSymlink is returned by ReadLink when the target is not a symlink.
	ErrNotSymlink = errors.New("hfsplus: not a symbolic link")

	// ErrCorrupt is returned when an on-disk structure fails a sanity check.
	ErrCorrupt = errors.New("hfsplus: corrupt image")

	// ErrUnsupported is returned for on-disk features the driver does not yet
	// decode/encode (e.g. compressed forks).
	ErrUnsupported = errors.New("hfsplus: unsupported feature")

	// ErrNoSpace is returned by the write path when the volume has no free
	// allocation blocks (or no contiguous run) to satisfy a request.
	ErrNoSpace = errors.New("hfsplus: no space left on volume")

	// ErrExists is returned by mutators when the target path already exists.
	ErrExists = errors.New("hfsplus: path already exists")

	// ErrNotEmpty is returned by DeleteDir when the directory still has
	// children.
	ErrNotEmpty = errors.New("hfsplus: directory not empty")
)
