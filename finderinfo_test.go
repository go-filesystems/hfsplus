package hfsplus

import (
	"encoding/binary"
	"path/filepath"
	"testing"
)

func finderVolume(t *testing.T) *Volume {
	t.Helper()
	p := filepath.Join(t.TempDir(), "fi.img")
	if _, err := Format(p, 16<<20, FormatConfig{Label: "FINDER"}); err != nil {
		t.Fatalf("Format: %v", err)
	}
	v, err := OpenFileWritable(p)
	if err != nil {
		t.Fatalf("OpenFileWritable: %v", err)
	}
	t.Cleanup(func() { _ = v.Close() })
	return v
}

func TestFinderInfoRoundTripsOnAFile(t *testing.T) {
	v := finderVolume(t)
	if err := v.WriteFile("/f.txt", []byte("x"), 0o644); err != nil {
		t.Fatal(err)
	}
	got, err := v.FinderInfo("/f.txt")
	if err != nil {
		t.Fatalf("FinderInfo: %v", err)
	}
	if got != [finderInfoLen]byte{} {
		t.Errorf("a fresh file starts with %v, want all zero", got)
	}
	var want [finderInfoLen]byte
	copy(want[:], []byte("TEXTttxt"))                            // fdType + fdCreator
	binary.BigEndian.PutUint16(want[FinderFlagsOffset:], 0x4000) // fdFlags
	binary.BigEndian.PutUint16(want[finderInfoLen-2:], 0xBEEF)   // into FXInfo
	if err := v.SetFinderInfo("/f.txt", want); err != nil {
		t.Fatalf("SetFinderInfo: %v", err)
	}
	got, err = v.FinderInfo("/f.txt")
	if err != nil {
		t.Fatalf("FinderInfo after set: %v", err)
	}
	if got != want {
		t.Errorf("round trip = %v, want %v", got, want)
	}
	// …and the file itself is untouched.
	if b, err := v.ReadFile("/f.txt"); err != nil || string(b) != "x" {
		t.Errorf("content = %q, %v — Finder info must not disturb the fork", b, err)
	}
}

// The root is the case a volume icon needs, and it is keyed differently from
// every other entry: (parent=1, name=label) rather than (parent=2, name=…).
func TestFinderInfoOnTheVolumeRoot(t *testing.T) {
	v := finderVolume(t)
	info, err := v.FinderInfo("/")
	if err != nil {
		t.Fatalf("FinderInfo(/): %v", err)
	}
	binary.BigEndian.PutUint16(info[FinderFlagsOffset:], FinderFlagHasCustomIcon)
	if err := v.SetFinderInfo("/", info); err != nil {
		t.Fatalf("SetFinderInfo(/): %v", err)
	}
	back, err := v.FinderInfo("/")
	if err != nil {
		t.Fatalf("FinderInfo(/) after set: %v", err)
	}
	if f := binary.BigEndian.Uint16(back[FinderFlagsOffset:]); f != FinderFlagHasCustomIcon {
		t.Errorf("root flags = 0x%04x, want 0x%04x", f, FinderFlagHasCustomIcon)
	}
	// The label must survive: SetFinderInfo re-keys the same record SetLabel
	// manipulates, so getting that wrong would rename the volume.
	if got := v.Label(); got != "FINDER" {
		t.Errorf("label = %q after touching root Finder info, want FINDER", got)
	}
}

func TestFinderInfoOnADirectory(t *testing.T) {
	v := finderVolume(t)
	if err := v.MkDir("/.background", 0o755); err != nil {
		t.Fatal(err)
	}
	info, err := v.FinderInfo("/.background")
	if err != nil {
		t.Fatalf("FinderInfo: %v", err)
	}
	binary.BigEndian.PutUint16(info[FinderFlagsOffset:], FinderFlagIsInvisible)
	if err := v.SetFinderInfo("/.background", info); err != nil {
		t.Fatalf("SetFinderInfo: %v", err)
	}
	back, err := v.FinderInfo("/.background")
	if err != nil {
		t.Fatal(err)
	}
	if f := binary.BigEndian.Uint16(back[FinderFlagsOffset:]); f != FinderFlagIsInvisible {
		t.Errorf("flags = 0x%04x, want invisible 0x%04x", f, FinderFlagIsInvisible)
	}
	// the directory is still a directory and still listable
	if _, err := v.ListDir("/.background"); err != nil {
		t.Errorf("ListDir after SetFinderInfo: %v", err)
	}
}

func TestFinderInfoErrors(t *testing.T) {
	v := finderVolume(t)
	if _, err := v.FinderInfo("/nope"); err == nil {
		t.Error("FinderInfo on a missing path should fail")
	}
	if err := v.SetFinderInfo("/nope", [finderInfoLen]byte{}); err == nil {
		t.Error("SetFinderInfo on a missing path should fail")
	}
	// A read-only volume refuses the write.
	p := filepath.Join(t.TempDir(), "ro.img")
	if _, err := Format(p, 16<<20, FormatConfig{Label: "RO"}); err != nil {
		t.Fatal(err)
	}
	ro, err := OpenFile(p)
	if err != nil {
		t.Fatal(err)
	}
	defer ro.Close()
	if err := ro.SetFinderInfo("/", [finderInfoLen]byte{}); err == nil {
		t.Error("SetFinderInfo on a read-only volume should fail")
	}
}
