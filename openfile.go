// Copyright (c) 2026, go-filesystems
// SPDX-License-Identifier: BSD-3-Clause

package hfsplus

import (
	"fmt"
	"io"

	filesystem "github.com/go-filesystems/interface"
)

// Verify the package offers positional reads (filesystem.Opener), so that a
// server reads a range of a file instead of the whole of it for each request.
var _ filesystem.Opener = (*Volume)(nil)

// OpenFile opens the regular file at path for positional reads of its data
// fork. Nothing of the file is read here: each ReadAt reads only the extents of
// its own window (fork.readAt). A compressed file is ErrUnsupported, as in
// ReadFile.
func (v *Volume) OpenFile(path string) (filesystem.File, error) {
	r, err := v.lookupPath(path)
	if err != nil {
		return nil, err
	}
	if r.rec.file == nil {
		return nil, fmt.Errorf("%w: %s", ErrNotRegular, path)
	}
	if isCompressed(r.rec.file.flags) {
		return nil, fmt.Errorf("%w: compressed fork", ErrUnsupported)
	}
	f, err := v.dataForkFor(r.rec.file)
	if err != nil {
		return nil, err
	}
	if f.size < 0 { // a logical size past 2^63 is no size
		return nil, fmt.Errorf("hfsplus: %s: the data fork's size does not fit an int64", path)
	}
	return &forkFile{f: f}, nil
}

type forkFile struct{ f *fork }

func (ff *forkFile) Size() int64 { return ff.f.size }

// ReadAt implements io.ReaderAt: io.EOF, with the bytes there are, when the
// window runs past the end of the file.
func (ff *forkFile) ReadAt(p []byte, off int64) (int, error) {
	n, err := ff.f.readAt(p, off)
	if err == nil && n < len(p) {
		err = io.EOF
	}
	return n, err
}

func (ff *forkFile) Close() error { return nil }
