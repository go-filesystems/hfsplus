// Copyright (c) 2026, go-filesystems
// SPDX-License-Identifier: BSD-3-Clause

//go:build !darwin

package hfsplus

import "fmt"

// formatImage on non-darwin platforms reports that the best-effort Mkfs needs
// macOS tooling (hdiutil). A from-scratch pure-Go formatter is not yet
// implemented.
func formatImage(_ string, _ int64, _ FormatConfig) error {
	return fmt.Errorf("%w: pure-Go Mkfs not yet implemented (darwin uses hdiutil)", ErrUnsupported)
}
