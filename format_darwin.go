// Copyright (c) 2026, go-filesystems
// SPDX-License-Identifier: BSD-3-Clause

//go:build darwin

package hfsplus

import (
	"fmt"
	"os"
	"os/exec"
	"strings"
)

// formatImage creates a raw HFS+ image via hdiutil (layout NONE → a bare
// volume with the header at offset 1024, exactly what Open expects).
func formatImage(path string, sizeBytes int64, cfg FormatConfig) error {
	label := cfg.Label
	if label == "" {
		label = "GOTEST"
	}
	fsType := "HFS+"
	if cfg.CaseSensitive {
		fsType = "Case-sensitive HFS+"
	}
	mb := (sizeBytes + (1 << 20) - 1) / (1 << 20)
	if mb < 1 {
		mb = 1
	}
	args := []string{
		"create",
		"-size", fmt.Sprintf("%dm", mb),
		"-fs", fsType,
		"-volname", label,
		"-layout", "NONE",
		"-ov",
		path,
	}
	out, err := exec.Command("hdiutil", args...).CombinedOutput()
	if err != nil {
		return fmt.Errorf("hfsplus: hdiutil create: %v: %s", err, strings.TrimSpace(string(out)))
	}
	// hdiutil may append .dmg when the path lacks a recognised extension.
	if _, statErr := os.Stat(path); statErr != nil {
		if _, e2 := os.Stat(path + ".dmg"); e2 == nil {
			if rerr := os.Rename(path+".dmg", path); rerr != nil {
				return fmt.Errorf("hfsplus: rename created image: %w", rerr)
			}
		}
	}
	return nil
}
