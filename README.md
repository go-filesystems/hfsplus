<p align="center"><img src="https://raw.githubusercontent.com/go-filesystems/brand/main/social/go-filesystems-hfsplus.png" alt="go-filesystems/hfsplus" width="720"></p>

# hfsplus

[![Go Reference](https://pkg.go.dev/badge/github.com/go-filesystems/hfsplus.svg)](https://pkg.go.dev/github.com/go-filesystems/hfsplus)
[![License: BSD-3-Clause](https://img.shields.io/badge/License-BSD%203--Clause-blue.svg)](https://opensource.org/licenses/BSD-3-Clause)
[![CI](https://github.com/go-filesystems/hfsplus/actions/workflows/ci.yml/badge.svg)](https://github.com/go-filesystems/hfsplus/actions/workflows/ci.yml)

Pure-Go, CGO-free **read/write** driver for the **HFS+** (Mac OS Extended)
on-disk format and its **HFSX** (case-sensitive) variant — part of the
[go-filesystems](https://github.com/go-filesystems) family.

HFS+ is the filesystem Apple shipped on Macs from Mac OS 8.1 through the move
to APFS. It stores all metadata **big-endian**: a volume header at byte offset
1024, a catalog B-tree keyed by (parent CNID, UTF-16 name) for the directory
hierarchy, an extents-overflow B-tree for fragmented files, and per-fork
allocation-block extents for file contents. This driver decodes **and writes**
those structures and exposes the volume through the shared
[`interface`](https://github.com/go-filesystems/interface) `Filesystem` API.

Both paths are validated against the native **macOS** tooling. The pure-Go
formatter (`Format` / `Mkfs`) and write path produce images that pass
`fsck_hfs -n` clean and that macOS mounts **read/write**, reading back the exact
files and bytes the Go side wrote — verified in **both directions**
(Go-formatted → macOS-read, and `hdiutil`-created → Go-written → macOS-read). A
small (≈7 KB gzipped) raw fixture is committed so the cross-arch round-trip runs
everywhere — including the big-endian **s390x** CI job, the endianness
correctness test.

## Support summary

| Feature | Status | Notes |
|---|---:|---|
| Open / OpenFile / Close | ✅ | Read-only; volume header at offset 1024; `H+` (HFS+) and `HX` (HFSX) signatures |
| OpenWritable / OpenFileWritable / Sync | ✅ | Read/write; image held in memory, mutated in place, flushed by `Sync` |
| ListDir / Stat / ReadFile / ReadLink | ✅ | Catalog walk; BSD perms; data fork via 8 inline extents **+ extents-overflow**; `S_IFLNK` targets |
| **Format / Mkfs (pure Go)** | ✅ | Lays down a valid empty HFS+/HFSX volume on **every arch**; `fsck_hfs -n` clean + macOS mounts RW |
| **WriteFile** | ✅ | Allocates blocks, writes the data fork, inserts catalog file + thread records |
| **MkDir / DeleteFile / DeleteDir / Rename** | ✅ | Catalog insert/delete with **B-tree node splitting + tree-height growth**; valence/freeBlocks kept in sync |
| **SetLabel / Symlink / Truncate** | ✅ | Optional `Labeller` / `Symlinker` / `Truncater` capabilities |
| Case-insensitive names | ✅ | Apple `FastUnicodeCompare` over the practical character set incl. ignorable-NUL (documented subset of the full fold table) |
| Case-sensitive (HFSX) | ✅ | Binary UTF-16 key comparison; HasFolderCount maintained |
| `FormatAppleDmg` (alt) | ⚠️ | Optional **darwin-only** escape hatch that shells to `hdiutil`; off-darwin returns `ErrUnsupported`. The primary `Format` is pure-Go. |

## Status

**Implemented and validated against real macOS images (read + write):**

- Volume header decode/encode (`H+` / `HX`, primary + alternate, block counts, special-file fork descriptors, clean-unmount attributes).
- Generic HFS+ B-tree node read/write (descriptor + trailing record-offset table) shared by the catalog and extents-overflow trees.
- Catalog B-tree: index descent, leaf scan, **insert/delete with leaf and index node splitting, index-record propagation, and new-root growth**; folder/file/thread records; valence and HasFolderCount maintenance.
- Allocation bitmap allocate/free of contiguous runs with `freeBlocks` sync.
- Pure-Go `Format`/`Mkfs`; `WriteFile`/`MkDir`/`DeleteFile`/`DeleteDir`/`Rename`; `SetLabel`/`Symlink`/`Truncate`.
- File read/write: data-fork inline extents plus extents-overflow continuation for reading fragmented files.

**Documented simplifications** (kept honest, like the `btrfs`/`xfs` siblings):

- **Write extents** — a written file's data fork is a single contiguous run via the inline extents; the extents-overflow *insert* path (>8 fragments) is not implemented and returns `ErrUnsupported`. Reading fragmented forks is fully supported.
- **Catalog growth** — the catalog fork is pre-sized by the formatter; growing it past that reservation returns `ErrNoSpace`.
- **Node merge** — deletion frees the record/thread and blocks but does not strictly re-merge underflowing nodes (stays `fsck`-clean).
- **Journaling** — the HFS+ journal is not written/replayed (volumes are authored cleanly-unmounted).
- **Hardlinks / compression / resource forks / xattrs** — indirect-node hardlinks, decmpfs compression, resource forks, and the Attributes B-tree are not yet handled (compression is detected and rejected rather than returning wrong bytes).

## Module

```
github.com/go-filesystems/hfsplus
```

## Install

```sh
go get github.com/go-filesystems/hfsplus
```

## Usage

```go
package main

import (
	"fmt"

	"github.com/go-filesystems/hfsplus"
)

func main() {
	// Format a fresh volume in pure Go and write into it.
	fs, err := hfsplus.Format("disk.hfs", 16<<20, hfsplus.FormatConfig{Label: "DATA"})
	if err != nil {
		panic(err)
	}
	_ = fs.MkDir("/sub", 0o755)
	_ = fs.WriteFile("/sub/hello.txt", []byte("hello\n"), 0o644)
	fs.Close() // flushes to disk.hfs

	// Reopen read-only and read it back.
	v, _ := hfsplus.OpenFile("disk.hfs")
	defer v.Close()
	entries, _ := v.ListDir("/sub")
	for _, e := range entries {
		fmt.Println(e.Name(), e.FileType())
	}
	data, _ := v.ReadFile("/sub/hello.txt")
	fmt.Printf("%d bytes\n", len(data))
}
```

`Open(io.ReaderAt, size)` / `OpenWritable([]byte, io.WriterAt)` are also
available when you already hold the image bytes (e.g. a decompressed DMG in
memory). The returned `*Volume` implements
`github.com/go-filesystems/interface.Filesystem` plus the optional
`Labeller` / `Symlinker` / `Truncater` capabilities.

## API

| Function / method | Purpose |
|---|---|
| `Open(rs io.ReaderAt, size int64) (*Volume, error)` | Parse an HFS+/HFSX volume read-only from a `ReaderAt` |
| `OpenFile(path string) (*Volume, error)` | Open an image file read-only |
| `OpenWritable(img []byte, wa io.WriterAt) (*Volume, error)` | Open an in-memory image read/write |
| `OpenFileWritable(path string) (*Volume, error)` | Open an image file read/write (mutations flushed by `Sync`) |
| `Mkfs(size int64, cfg FormatConfig) ([]byte, error)` | Build a fresh empty HFS+/HFSX image in pure Go |
| `Format(path string, size int64, cfg FormatConfig) (filesystem.Filesystem, error)` | Pure-Go format + open read/write |
| `FormatAppleDmg(path, size, cfg)` | Optional darwin-only `hdiutil` alternative |
| `(*Volume) WriteFile / MkDir / DeleteFile / DeleteDir / Rename` | Mutate the volume |
| `(*Volume) SetLabel / Symlink / Truncate` | Optional `Labeller` / `Symlinker` / `Truncater` capabilities |
| `(*Volume) Sync() error` | Flush the in-memory image to the backing store |
| `(*Volume) ListDir / ReadFile / Stat / ReadLink` | Read paths |
| `(*Volume) CaseSensitive() bool` | Report HFSX binary key comparison |

## Validation

The committed read-path fixture (`testdata/hfsplus.dmg.gz`) was produced on
macOS with `hdiutil create -fs "HFS+"` and populated with known files; the
reader lists those entries and reads each back with the exact MD5 macOS
computed (also the big-endian **s390x** correctness test).

The write path is validated by darwin-only tests
(`macos_validation_test.go`) that exercise the native tooling:

```sh
# Pure-Go format → fsck clean + macOS reads the bytes Go wrote
go test -run TestDarwinWriteRoundTrip
# hdiutil-created image → Go writes into it → fsck clean + macOS reads it
go test -run TestDarwinWriteIntoAppleImage
```

Each test formats/writes with the pure-Go driver, attaches the raw image with
`hdiutil`, asserts `fsck_hfs -n` reports the volume **clean**, then mounts it
read/write and compares the files and bytes macOS sees against what Go wrote.
The cross-arch pure-Go round-trip (format → write → read) in `write_test.go`
runs on every architecture, including big-endian s390x.

## References

- Apple TN1150, *HFS Plus Volume Format*.
- `man hdiutil`, `man fsck_hfs`, `man newfs_hfs`.

## License

BSD-3-Clause © the go-filesystems authors.
