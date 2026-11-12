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
| OpenWritable / OpenFileWritable / Sync | ✅ | Read/write **in place**: no copy of the image; each mutation writes only the sectors it changed (see "Cost of a write") |
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
- Catalog & extents-overflow B-trees: index descent, leaf scan, **insert/delete with leaf and index node splitting, index-record propagation, new-root growth, node-underflow rebalancing (rotate/merge), emptied-node freeing, and tree-height shrink**; folder/file/thread records; valence and HasFolderCount maintenance.
- **Growable B-tree files** — when a tree exhausts its node reservation it allocates more blocks, extends its backing fork's extents (inline, then spilling into the extents-overflow tree), grows the node-allocation bitmap (adding map nodes), and updates the header-node `totalNodes`/`freeNodes` and the volume-header special-file fork descriptor + `freeBlocks`.
- Allocation bitmap allocate/free of contiguous runs **and multi-fragment allocations** with `freeBlocks` sync.
- Pure-Go `Format`/`Mkfs`; `WriteFile`/`MkDir`/`DeleteFile`/`DeleteDir`/`Rename`; `SetLabel`/`Symlink`/`Truncate`.
- File read/write: data-fork **inline extents plus extents-overflow continuation for both reading and writing** fragmented forks — a written file needing more than eight extents fills the inline descriptors and inserts the remainder into the extents-overflow B-tree, round-tripping exactly.

**Out of scope** (as with the `btrfs`/`xfs` siblings — these are deliberate non-goals, not partial implementations):

- **Journaling** — the HFS+ journal is not written/replayed (volumes are authored cleanly-unmounted). A **journaled** volume (`kHFSVolumeJournaledBit`, e.g. `hdiutil create -fs "Journaled HFS+"`) whose journal is **empty** (`start == end`, the state after a clean unmount) is opened read/write and written directly: macOS's replay at mount has nothing to copy over the written blocks, and the journal info block, journal and attribute are left as they were. One whose journal holds transactions, or whose journal cannot be verified (info block or journal outside the volume, other-device journal, bad magic or checksum, size mismatch) is refused by `OpenWritable` / `OpenFileWritable` with `ErrJournaled`, because macOS would replay the journalled blocks over the driver's writes. Read-only `Open` is never refused. To make such a volume writable, mount and unmount it on macOS first so its journal is replayed.
- **Do not write an image that is mounted.** macOS keeps its own view of a mounted volume (cached nodes, bitmap and headers) and the two clobber each other (see `detach` in `macos_validation_test.go`).
- **Hardlinks / compression / resource forks / xattrs** — indirect-node hardlinks, decmpfs compression, resource forks, and the Attributes B-tree are not handled (compression is detected and rejected rather than returning wrong bytes).

## Cost of a write

`OpenFileWritable` reads and writes the image **in place** over the file; it never holds a copy of it, so
opening costs a few KiB whatever the image size, and the 64 GiB in-memory ceiling is gone.

- **Memory** is what one mutation touches (the B-tree nodes and bitmap chunks it edits, the two volume
  headers; refused beyond 64 MiB) plus a read cache of at most 8 MiB. It does not grow with the image.
  The bitmap is read in chunks of at most 4 KiB, never loaded whole. (`OpenWritable` over a `[]byte` is
  the caller's own copy, kept current for `Bytes()`; `Format`/`Mkfs` still build the new image in memory.)
- **Bytes written** per mutation are the file's data blocks plus the 512-byte sectors that changed:
  a 1-byte `WriteFile` is a data block, a few catalog and bitmap sectors and the two 512-byte headers
  (tens of KiB at the most); `Chtimes` is one catalog node's changed sectors and the headers. Not the image.
  Measured on a 1 GiB sparse image by `TestInPlaceLargeSparseImage`.
- **Order** of each mutation (Apple TN1150, "Volume Header", "Allocation File"): file data, then the
  primary header rewritten with `kHFSVolumeUnmountedBit` cleared, fsync; the B-tree and bitmap sectors,
  fsync; then the alternate header and, last, the primary header (bit set again), fsync. A crash anywhere
  in between leaves the bit clear, so the next mount runs `fsck_hfs` instead of trusting a half-written
  volume (go-filesystems/hfsplus#32, finding 6). A volume that was already not cleanly unmounted when
  opened is left that way. The cost is three fsyncs per mutation; a crash between the B-tree writes is
  repaired by fsck, not by the driver (there is no journal).
- **Allocation** scans the bitmap from block 0 for a free run, so a very full large volume reads more
  bitmap per write (reads only; the bytes written stay O(change)).

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
| `OpenFileWritable(path string) (*Volume, error)` | Open an image file read/write in place (no copy of the image; each mutation writes and fsyncs what it changed) |
| `Mkfs(size int64, cfg FormatConfig) ([]byte, error)` | Build a fresh empty HFS+/HFSX image in pure Go |
| `Format(path string, size int64, cfg FormatConfig) (filesystem.Filesystem, error)` | Pure-Go format + open read/write |
| `FormatAppleDmg(path, size, cfg)` | Optional darwin-only `hdiutil` alternative |
| `(*Volume) WriteFile / MkDir / DeleteFile / DeleteDir / Rename` | Mutate the volume |
| `(*Volume) SetLabel / Symlink / Truncate` | Optional `Labeller` / `Symlinker` / `Truncater` capabilities |
| `(*Volume) Sync() error` | Final fsync (every mutator has already written and fsynced its changes) |
| `(*Volume) ListDir / ReadFile / Stat / ReadLink` | Read paths |
| `(*Volume) Chtimes` | `Chtimes(path, atime, mtime)`: contentModDate and accessDate as whole GMT seconds since 1904 (rounded down), attributeModDate (ctime) set to the clock, createDate untouched; a zero time leaves its field unchanged; a time outside 1904-01-01 00:00:01 .. 2040-02-06 06:28:15 GMT (the date 0 reads as "not recorded") is a `*TimeRangeError` (`errors.Is(err, ErrTimeOutOfRange)`), nothing written; the catalog record is patched in place, so the B-tree is unchanged in shape. Checked with `fsck_hfs` and a macOS mount |
| `(*Volume) Chmod`, `Chown` | with `Chtimes`, the whole `filesystem.MetadataSetter` (what SMB/NFS/SFTP servers probe for). `Chmod` replaces the 12 permission bits (setuid/setgid/sticky included) of `HFSPlusBSDInfo.fileMode`, keeping the type bits; `Chown` sets ownerID/groupID; both patch the catalog record in place (file or folder), set attributeModDate to the clock, refuse the root folder, and `Stat` now reports a folder's recorded mode. Checked with a macOS mount (`stat -f %Lp`) |
| `(*Volume) Times` | `filesystem.TimesReader`: createDate, contentModDate, attributeModDate (ctime), accessDate from the catalog record, GMT seconds since 1904, resolution 1 s; a zero date and the root folder are unrecorded |
| `(*Volume) OpenFile` (`filesystem.Opener`) | Positional reads of a data fork: each `ReadAt` reads only the extents of its window. A 1 MiB read at 66 MiB of a 72 MiB file allocates nothing through it and 75.5 MB through `ReadFile` |
| `(*Volume) CaseSensitive() bool` | Report HFSX binary key comparison |
| `ErrJournaled` | `OpenWritable` / `OpenFileWritable` on a journaled volume whose journal is not provably empty (transactions pending, or unreadable / off-volume / other-device / bad checksum); `errors.Is(err, ErrJournaled)`, the reason is in the message. Nothing is written |

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
# journaled image with an empty journal → Go writes → macOS replays nothing and reads it
go test -run TestDarwinWriteIntoJournaledImage
```

The tests take the mount point from `hdiutil attach`'s output, never from a
spelled `/Volumes/<name>`: a volume of the same name left mounted by an earlier
run (or another session) pushes the new one to `/Volumes/<name> 1`, and a test
that read the spelled path read the stale volume, which is how
`TestDarwinWriteIntoAppleImage` reported the driver's files as missing
(#29; `TestDarwinMountPointsAreNotShared` keeps it fixed).

Each test formats/writes with the pure-Go driver, attaches the raw image with
`hdiutil`, asserts `fsck_hfs -n` reports the volume **clean**, then mounts it
read/write and compares the files and bytes macOS sees against what Go wrote.
The cross-arch pure-Go round-trip (format → write → read) in `write_test.go`
runs on every architecture, including big-endian s390x.

## Untrusted images

An image is attacker-controlled input and every size in it is a claim. Each
input-driven allocation is bounded before it is made (`bombs_test.go` measures
each road with `runtime.MemStats`):

| Claim in the image | Bound |
|---|---|
| a fork's logical size (`ReadFile`, `ReadLink`) | at most the image size, every extent the fork uses must lie inside the image (checked before the buffer is made); the last byte is probed as well |
| the same with the image size unknown (`Open(rs, -1)`) | the buffer grows (1 MiB steps, doubling) with the bytes the reader delivers, so a forged 512 MiB claim over a 4 MiB volume costs 4 MiB |
| the B-tree leaf-chain walk (cycles) | at most image size / node size steps (was: the extents' claim, up to 2^24) |
| a B-tree node | `nodeSize` is a power of two in [512, 32768] |

Fuzz targets: `FuzzOpenHeader`, `FuzzOpenPatched`, `FuzzBombs` (asserts a
per-input allocation budget), `FuzzJournalHeader` and `FuzzCheckJournal` (the
journal header and journal info block that decide whether a journaled volume is
writable).

## References

- Apple TN1150, *HFS Plus Volume Format* (Journal, Journal Info Block, Journal Checksums, Replaying the Journal).
- Linux `fs/hfsplus/super.c`: a journaled volume is mounted read-only unless forced.
- `man hdiutil`, `man fsck_hfs`, `man newfs_hfs`.

## License

BSD-3-Clause © the go-filesystems authors.
