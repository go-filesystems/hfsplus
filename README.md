<p align="center"><img src="https://raw.githubusercontent.com/go-filesystems/brand/main/social/go-filesystems-hfsplus.png" alt="go-filesystems/hfsplus" width="720"></p>

# hfsplus

[![Go Reference](https://pkg.go.dev/badge/github.com/go-filesystems/hfsplus.svg)](https://pkg.go.dev/github.com/go-filesystems/hfsplus)
[![License: BSD-3-Clause](https://img.shields.io/badge/License-BSD%203--Clause-blue.svg)](https://opensource.org/licenses/BSD-3-Clause)
[![CI](https://github.com/go-filesystems/hfsplus/actions/workflows/ci.yml/badge.svg)](https://github.com/go-filesystems/hfsplus/actions/workflows/ci.yml)

Pure-Go, CGO-free **reader** for the **HFS+** (Mac OS Extended) on-disk format
and its **HFSX** (case-sensitive) variant — part of the
[go-filesystems](https://github.com/go-filesystems) family.

HFS+ is the filesystem Apple shipped on Macs from Mac OS 8.1 through the move
to APFS. It stores all metadata **big-endian**: a volume header at byte offset
1024, a catalog B-tree keyed by (parent CNID, UTF-16 name) for the directory
hierarchy, an extents-overflow B-tree for fragmented files, and per-fork
allocation-block extents for file contents. This driver decodes those
structures and exposes the volume through the shared
[`interface`](https://github.com/go-filesystems/interface) `Filesystem` API.

The read path is validated against **real HFS+ images created by macOS**
(`hdiutil create -fs "HFS+"`): the test suite lists directories and reads files
back **byte-for-byte** (MD5-verified against the checksums macOS computed at
fixture-creation time). A small (≈7 KB gzipped) raw fixture is committed so the
reader tests run everywhere — including the big-endian **s390x** CI job, which
is the endianness correctness test.

## Support summary

| Feature | Status | Notes |
|---|---:|---|
| Open / Close | ✅ | Volume header at offset 1024; `H+` (HFS+) and `HX` (HFSX) signatures |
| ListDir | ✅ | Catalog B-tree walk; thread records filtered out |
| Stat | ✅ | Mode (BSD perms for files), size, CNID as pseudo-inode |
| ReadFile | ✅ | Data fork via 8 inline extents **+ extents-overflow** continuation |
| Path lookup | ✅ | Component-by-component from the root folder (CNID 2) |
| Case-insensitive names | ✅ | Pragmatic Unicode case-fold (documented simplification, not Apple's full fast-case-fold table) |
| Case-sensitive (HFSX) | ✅ | Binary UTF-16 key comparison when `keyCompareType` is binary |
| ReadLink / symlinks | ✅ | Symlink target = data-fork bytes of an `S_IFLNK` catalog file |
| Multi-extent / fragmented files | ✅ | Inline extents then the extents-overflow B-tree |
| Format / Mkfs | ⚠️ | Best-effort, **darwin-only** (shells to `hdiutil`); off-darwin returns `ErrUnsupported` |
| Write operations | ❌ | Mutators return `ErrReadOnly` (see Status) |

## Status

**Implemented (validated against real macOS images):**

- Volume header decode (`H+` / `HX`, block size, block counts, special-file fork descriptors).
- Generic HFS+ B-tree node reader (node descriptor + trailing record-offset table) shared by the catalog and extents-overflow trees.
- Catalog B-tree: index descent + leaf scan, folder/file/thread records, directory listing and path lookup.
- File read: data-fork inline extents plus extents-overflow continuation for fragmented files.
- Symlink resolution; HFSX case-sensitive comparison.

**Gated / not yet implemented** (documented honestly, like the `btrfs`/`xfs`
siblings track pending work):

- **Write path** — every mutating `Filesystem` method returns `ErrReadOnly`.
- **Mkfs** — only a best-effort darwin `hdiutil` wrapper; no pure-Go formatter yet.
- **Journaling** — the HFS+ journal is not read or replayed (the reader assumes a cleanly-unmounted volume).
- **Hardlinks** — Apple's indirect-node (`HFS+ Private Data`) hardlink scheme is not yet followed; the link inode is listed but not resolved to its target.
- **Compression** — decmpfs-compressed forks are detected and rejected with `ErrUnsupported` rather than silently returning wrong bytes.
- **Resource forks / extended attributes** — the Attributes B-tree is not decoded.

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
	fs, err := hfsplus.OpenFile("disk.hfs")
	if err != nil {
		panic(err)
	}
	defer fs.Close()

	entries, _ := fs.ListDir("/")
	for _, e := range entries {
		fmt.Println(e.Name(), e.FileType())
	}

	data, _ := fs.ReadFile("/path/to/file.txt")
	fmt.Printf("%d bytes\n", len(data))
}
```

`Open(io.ReaderAt, size)` is also available when you already hold the image
bytes (e.g. a decompressed DMG in memory). The returned `*Volume` implements
`github.com/go-filesystems/interface.Filesystem`.

## API

| Function / method | Purpose |
|---|---|
| `Open(rs io.ReaderAt, size int64) (*Volume, error)` | Parse an HFS+/HFSX volume from a `ReaderAt` |
| `OpenFile(path string) (*Volume, error)` | Open an image file read-only |
| `Format(path string, size int64, cfg FormatConfig) (*Volume, error)` | Best-effort Mkfs (darwin-only) |
| `(*Volume) ListDir(path) ([]DirEntry, error)` | Enumerate a directory |
| `(*Volume) ReadFile(path) ([]byte, error)` | Read a file's data fork |
| `(*Volume) Stat(path) (Stat, error)` | Mode / size / CNID |
| `(*Volume) ReadLink(path) (string, error)` | Resolve a symbolic link |
| `(*Volume) CaseSensitive() bool` | Report HFSX binary key comparison |

## Validation

The committed fixture (`testdata/hfsplus.dmg.gz`) was produced on macOS with:

```sh
hdiutil create -size 4m -fs "HFS+" -volname GOTEST -layout NONE fixture.dmg
hdiutil attach fixture.dmg -nobrowse -noautoopen
# populate known files (hello.txt, fox.txt, subdir/nested/deep.txt, big.bin,
# empty.txt, /subdir/a.txt, link.txt -> /subdir/a.txt)
hdiutil detach …
gzip -9 fixture.dmg          # ≈7 KB, committed
```

The reader lists those entries and reads each file back with the exact MD5 the
macOS side computed. `Format` (darwin) round-trips through `hdiutil` and is
spot-checked with `fsck_hfs -n`.

## References

- Apple TN1150, *HFS Plus Volume Format*.
- `man hdiutil`, `man fsck_hfs`, `man newfs_hfs`.

## License

BSD-3-Clause © the go-filesystems authors.
