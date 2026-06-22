// Copyright (c) 2026, go-filesystems
// SPDX-License-Identifier: BSD-3-Clause

package hfsplus

import (
	"fmt"
	"os"

	filesystem "github.com/go-filesystems/interface"
)

// FormatConfig configures Format/Mkfs.
type FormatConfig struct {
	// Label is the volume name. Defaults to "GOTEST" when empty.
	Label string
	// CaseSensitive requests an HFSX (case-sensitive) volume instead of plain
	// case-insensitive HFS+.
	CaseSensitive bool
}

// Format creates a fresh, empty HFS+ (or HFSX) volume image at path of
// sizeBytes bytes using the pure-Go formatter (Mkfs), then opens it
// read/write. Pure Go, CGO-free, big-endian — works on every architecture.
//
// The produced image passes `fsck_hfs -n` clean on macOS and mounts
// read/write; on every platform Open/OpenWritable round-trip it. The returned
// Volume is writable: WriteFile/MkDir/DeleteFile/DeleteDir/Rename mutate it and
// flush back to path.
//
// The signature matches the apfs sibling (Format(path, sizeBytes, cfg)).
func Format(path string, sizeBytes int64, cfg FormatConfig) (filesystem.Filesystem, error) {
	img, err := Mkfs(sizeBytes, cfg)
	if err != nil {
		return nil, err
	}
	if err := os.WriteFile(path, img, 0o644); err != nil {
		return nil, fmt.Errorf("hfsplus: write image %s: %w", path, err)
	}
	return OpenFileWritable(path)
}

// FormatAppleDmg is the optional darwin-only alternative that shells out to
// the native hdiutil to author a real HFS+ image (the same tool that produced
// the read-path fixtures). It is provided as a parity escape hatch alongside
// the primary pure-Go Format, mirroring the apfs sibling's FormatAppleDmg.
// On non-darwin platforms it returns ErrUnsupported.
func FormatAppleDmg(path string, sizeBytes int64, cfg FormatConfig) (filesystem.Filesystem, error) {
	if err := formatImageHdiutil(path, sizeBytes, cfg); err != nil {
		return nil, err
	}
	return OpenFileWritable(path)
}
