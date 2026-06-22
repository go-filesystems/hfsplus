// Copyright (c) 2026, go-filesystems
// SPDX-License-Identifier: BSD-3-Clause

//go:build !darwin

package hfsplus

import "fmt"

// formatImageHdiutil on non-darwin platforms reports that the hdiutil escape
// hatch needs macOS. The primary Format uses the cross-platform pure-Go Mkfs,
// so this only affects the optional FormatAppleDmg alternative.
func formatImageHdiutil(_ string, _ int64, _ FormatConfig) error {
	return fmt.Errorf("%w: FormatAppleDmg needs macOS hdiutil; use Format (pure-Go) instead", ErrUnsupported)
}
