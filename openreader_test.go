package hfsplus

import (
	"bytes"
	"testing"
)

// A real image, built the way this package's own tests build one.
func TestOpenReaderOpensWhatOpenOpens(t *testing.T) {
	img, err := Mkfs(8<<20, FormatConfig{Label: "READER"})
	if err != nil {
		t.Fatal(err)
	}
	through, err := OpenReader(bytes.NewReader(img), int64(len(img)))
	if err != nil {
		t.Fatalf("OpenReader: %v", err)
	}
	defer through.Close()
	if _, err := through.ListDir("/"); err != nil {
		t.Errorf("listing through OpenReader: %v", err)
	}
}

// What the wrapper adds beyond Open is the error path, and the one thing that
// can go wrong there: a typed nil inside an interface is NOT nil, so a caller
// checking the value rather than the error would be told it has a filesystem.
func TestOpenReaderReturnsNothingWhenItFails(t *testing.T) {
	fs, err := OpenReader(bytes.NewReader(make([]byte, 4096)), 4096)
	if err == nil {
		t.Fatal("OpenReader accepted 4 KiB of zeros as an image")
	}
	if fs != nil {
		t.Error("OpenReader returned an error AND a filesystem")
	}
}
