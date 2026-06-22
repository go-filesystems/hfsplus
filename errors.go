// Copyright (c) 2026, go-filesystems
// SPDX-License-Identifier: BSD-3-Clause

// Package hfsplus is a pure-Go, CGO-free reader for the HFS+ (Mac OS
// Extended) on-disk format, including its HFSX (case-sensitive) variant.
//
// HFS+ stores every multi-byte field big-endian. The volume header lives at
// byte offset 1024 and carries the block size, block counts, and the special
// fork descriptors for the catalog, extents-overflow, and allocation files.
// File and directory metadata live in the catalog B-tree, keyed by the parent
// CNID plus the UTF-16 node name; file contents are addressed by allocation
// blocks via up to eight inline extents per fork, spilling into the
// extents-overflow B-tree for fragmented files.
//
// This package implements the read path against the shared
// github.com/go-filesystems/interface Filesystem contract: Open an image,
// list directories, stat paths, and read file data back byte-for-byte.
//
// Scope and honest limitations:
//
//   - Read is implemented and validated against real macOS HFS+ images
//     (hdiutil-created). Directory listing, path lookup, Stat, and full file
//     read (inline extents plus extents-overflow continuation) work.
//   - Case-insensitive name comparison uses a documented simplification: a
//     pragmatic Unicode case fold rather than Apple's full HFS+ fast
//     case-fold table. This resolves ASCII and common Latin names; exotic
//     case-folding corner cases may not match.
//   - Mutating methods (WriteFile, MkDir, ...) return ErrReadOnly. A minimal
//     Format/Mkfs producing a valid empty volume is provided best-effort and
//     is gated/documented as such.
//   - Journaling, hardlink (indirect-node) following, HFS+ decmpfs
//     compression, resource forks, and the full write path are not yet
//     implemented; see the README Status section.
package hfsplus

import "errors"

// Sentinel errors. Compare with errors.Is so wrapped errors keep matching.
var (
	// ErrReadOnly is returned by every mutating method of the Filesystem
	// contract. The HFS+ reader does not yet write through these.
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

	// ErrUnsupported is returned for on-disk features the reader does not yet
	// decode (e.g. compressed forks).
	ErrUnsupported = errors.New("hfsplus: unsupported feature")
)
