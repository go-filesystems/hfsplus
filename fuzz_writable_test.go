// Copyright (c) 2026, go-filesystems
// SPDX-License-Identifier: BSD-3-Clause

package hfsplus

import "testing"

// FuzzWritable splices fuzzer bytes into a valid image, opens it writable and
// drives the mutators over it. A hostile image may make a mutator fail; it must
// never panic, whatever field of the image the patch lands on. (Added after the
// review of go-filesystems/hfsplus#34 found a panic in adjustParentValence on a
// patched image.)
func FuzzWritable(f *testing.F) {
	base := fuzzSeed(f)
	f.Add(int64(volumeHeaderOffset+112), []byte{0xff, 0xff, 0xff, 0xff})
	f.Add(int64(5*4096), []byte{0, 0, 0, 9})
	f.Add(int64(6*4096), []byte{0xff, 0xff})
	f.Fuzz(func(t *testing.T, off int64, patch []byte) {
		if len(patch) == 0 || len(patch) > 4096 || off < 0 || off >= int64(len(base)) {
			return
		}
		img := append([]byte(nil), base...)
		copy(img[off:], patch)
		v, err := OpenWritable(img, nil)
		if err != nil {
			return
		}
		_ = v.MkDir("/q", 0o755)
		_ = v.WriteFile("/q/z", make([]byte, 9000), 0o644)
		for i := 0; i < 40; i++ {
			_ = v.MkDir("/q/d"+string(rune('a'+i%26))+string(rune('a'+i/26)), 0o755)
		}
		_ = v.Truncate("/q/z", 20000)
		_ = v.DeleteFile("/q/z")
		_ = v.Rename("/q", "/r")
		_ = v.DeleteDir("/r")
	})
}
