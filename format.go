// Copyright (c) 2026, go-filesystems
// SPDX-License-Identifier: BSD-3-Clause

package hfsplus

// FormatConfig configures Format/Mkfs.
type FormatConfig struct {
	// Label is the volume name. Defaults to "GOTEST" when empty.
	Label string
	// CaseSensitive requests an HFSX (case-sensitive) volume instead of plain
	// case-insensitive HFS+.
	CaseSensitive bool
}

// Format creates a fresh, empty HFS+ (or HFSX) volume image at path of
// sizeBytes bytes, then opens it read-only.
//
// Scope/honesty: this is a best-effort Mkfs. On macOS it shells out to the
// native hdiutil to produce a real, fsck_hfs-clean raw HFS+ image (the same
// tool used to author the test fixtures). A from-scratch pure-Go formatter is
// not yet implemented, so off-darwin Format returns ErrUnsupported. The
// returned Volume is read-only (the write path is gated — see the package
// docs). The platform-specific formatter lives in format_darwin.go /
// format_other.go.
func Format(path string, sizeBytes int64, cfg FormatConfig) (*Volume, error) {
	if err := formatImage(path, sizeBytes, cfg); err != nil {
		return nil, err
	}
	return OpenFile(path)
}
