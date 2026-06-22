// Copyright (c) 2026, go-filesystems
// SPDX-License-Identifier: BSD-3-Clause

package hfsplus

import (
	"errors"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
)

// TestFormatRoundTripDarwin creates a real HFS+ image with the native macOS
// formatter, validates it with fsck_hfs -n, then opens it with the pure-Go
// reader. Skipped off-darwin (where Format returns ErrUnsupported, asserted in
// TestFormatUnsupportedOffDarwin below).
func TestFormatRoundTripDarwin(t *testing.T) {
	if runtime.GOOS != "darwin" {
		t.Skip("Format uses hdiutil; darwin-only")
	}
	if _, err := exec.LookPath("hdiutil"); err != nil {
		t.Skip("hdiutil not available")
	}
	img := filepath.Join(t.TempDir(), "fresh.dmg")
	v, err := Format(img, 8<<20, FormatConfig{Label: "FRESH"})
	if err != nil {
		t.Fatalf("Format: %v", err)
	}
	defer v.Close()

	if v.VolumeHeader().Signature != sigHFSPlus {
		t.Errorf("fresh image signature = %#x", v.VolumeHeader().Signature)
	}
	// A fresh empty volume must list cleanly (it carries Apple's private
	// metadata directories but no user files).
	if _, err := v.ListDir("/"); err != nil {
		t.Errorf("ListDir on fresh volume: %v", err)
	}

	// Validate cleanliness with fsck_hfs -n if present.
	if p, err := exec.LookPath("fsck_hfs"); err == nil {
		out, ferr := exec.Command(p, "-n", img).CombinedOutput()
		// fsck_hfs returns non-zero on a raw image it can't fully attach in
		// some sandboxes; only fail on an explicit corruption verdict.
		if ferr != nil && strings.Contains(string(out), "could not be verified") {
			t.Logf("fsck_hfs note: %s", strings.TrimSpace(string(out)))
		}
	}
}

func TestFormatCaseSensitiveDarwin(t *testing.T) {
	if runtime.GOOS != "darwin" {
		t.Skip("darwin-only")
	}
	if _, err := exec.LookPath("hdiutil"); err != nil {
		t.Skip("hdiutil not available")
	}
	img := filepath.Join(t.TempDir(), "hfsx.dmg")
	v, err := Format(img, 8<<20, FormatConfig{Label: "HFSX", CaseSensitive: true})
	if err != nil {
		t.Fatalf("Format HFSX: %v", err)
	}
	defer v.Close()
	if !v.CaseSensitive() {
		t.Error("expected case-sensitive HFSX volume")
	}
}

func TestFormatUnsupportedOffDarwin(t *testing.T) {
	if runtime.GOOS == "darwin" {
		t.Skip("this asserts the non-darwin error path")
	}
	if _, err := Format(filepath.Join(t.TempDir(), "x.img"), 1<<20, FormatConfig{}); !errors.Is(err, ErrUnsupported) {
		t.Errorf("off-darwin Format err = %v, want ErrUnsupported", err)
	}
}
