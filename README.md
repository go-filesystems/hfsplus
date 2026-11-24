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

- **Journaling** — since v0.18.0 a writable open of a journaled volume writes its commits as journal transactions at `DurabilityStrict` and `DurabilityMounted` (see "Writing through the journal" below; every other case, including non-journaled volumes, writes directly as described next). The rest of this bullet describes the direct path. A non-empty `HFSJ` journal is replayed at a writable open, and by `Open(..., WithJournalReplay())` in memory, see below. A **journaled** volume (`kHFSVolumeJournaledBit`, e.g. `hdiutil create -fs "Journaled HFS+"`) whose journal is **empty** (`start == end`, the state after a clean unmount) is opened read/write and written directly: macOS's replay at mount has nothing to copy over the written blocks, and the journal info block, journal and attribute are left as they were (since v0.12.3 the first commit that changes anything stamps `lastMountedVersion` = `GoHF` into both volume headers, journaled or not; not `HFSJ`, the driver does not maintain the journal). One whose journal holds transactions is replayed first when its `lastMountedVersion` is `HFSJ` (see "Writable opens replay it too" below) and refused with `ErrJournaled` otherwise; one whose journal cannot be verified (info block or journal outside the volume, other-device journal, bad magic or checksum, size mismatch, or since v0.12.3 the Apple `journal_open` range checks: `jhdr_size` 0, journal size outside [256 KiB, 1 GiB], start/end 0, beyond the journal size or 1 GiB, or not a multiple of 512; three older checks stay stricter than Apple's and can refuse a journal macOS opens: the magic, where we accept only `JNLx` and Apple also accepts `JHDR`; the endian field; and the journal size recorded in the journal header against the Journal Info Block's, hfsplus#48) is refused by `OpenWritable` / `OpenFileWritable` with `ErrJournaled`, because macOS would replay the journalled blocks over the driver's writes. Read-only `Open` is never refused.
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
  opened is left that way. The cost is three fsyncs per mutation. The guarantee is exactly that the
  fsyncs separate the three groups (data and the cleared bit; B-tree and bitmap sectors; both headers);
  the alternate and the primary header are written back to back with no fsync between them, and the
  B-tree sectors are edited in place, so a crash is repaired by `fsck_hfs`, not by the driver (there is
  no journal).
- **Durability levels** (`SetDurability`, `Batch`; hfsplus#35). Barriers are `File.Sync`, which on darwin
  is `fcntl(F_FULLFSYNC)` (Go's `internal/poll/fd_fsync_darwin.go`, `Fsync`, line 22); `man 2 fsync` says
  plain fsync(2) leaves the drive free to reorder, `man 2 fcntl` says F_FULLFSYNC "acts as a barrier",
  so the ordering above is only a guarantee over F_FULLFSYNC and no weaker primitive is offered.
  Fsyncs for a first mutation on a volume clean at open, a second without file data, a third with file
  data, then `Sync` (counted by `TestDurabilityBarriers`):

  | Level | first | next (no data) | with data | `Sync` | Durable when the mutator returns | Volume between mutations |
  |---|---|---|---|---|---|---|
  | `DurabilityStrict` (default) | 3 | 3 | 3 | 1 | yes | marked clean |
  | `DurabilityMounted` | 2 | 1 | 2 | 1 | yes | marked dirty until `Sync`/`Close` |
  | `DurabilityLazy` | 1 | 0 | 0 | 2 | no, only after `Sync`/`Close` | marked dirty until `Sync`/`Close` |
  | `Strict` or `Mounted`, journaled volume, journal writes on (**default since v0.18.0**) | 2 | 1 | 1 (small data) | 0 or 1 | yes | mounted, journal live; `Close` marks it clean |
  | `Lazy` or `Batch`, journaled volume, journal writes on (**v0.19.0**) | 0 | 0 | 0 (large data: written in place, ordered by one barrier before the transaction) | 2 for a session's first flush, 1 after, 0 when nothing is pending | no, only after `Sync`/`Close`/a level switch | mounted, journal live, never marked dirty |

  The first three rows are the direct path: every non-journaled volume, and a journaled volume that fell back
  (see "Writing through the journal"). The fourth row replaces the `Strict` and `Mounted` rows on a journaled
  volume, which is where the journal is on by default: both levels run the same journal commit, so `Mounted` no
  longer has a dirty span there (the journal makes each commit atomic). A single commit that cannot be
  journaled while the journal is on (a forged sector, more than 30 runs) takes the `Strict` direct path at
  `Mounted` too: the volume is never held dirty while it journals. The last row replaces the `Lazy` row on a
  journaled volume: **a group commit**. A Lazy commit writes nothing and takes no barrier; its changed sectors
  (and its small data) are kept in memory, and `Sync`, `Close`, `SetDurability` away from `Lazy` or the group's
  size bound write them as ONE journal transaction. The memory is bounded: the group never holds more than the
  journal's data bound (the smaller of 1 MiB and one eighth of the journal, at most 2048 sectors; 32704 bytes on a
  256 KiB journal), at most 30 runs and 4096 freed block spans, on top of the per-mutation limit that already
  existed. A commit that does not fit beside the group flushes the group first and starts the next one.

  `Mounted` is data-ordered (like ext4 `data=ordered`): a commit that wrote file data puts a barrier
  between the data and the metadata that points at it, so no crash image shows a file holding bytes it
  was never written with (`TestDurabilityNoStaleData`, red without that barrier). On the direct path `Lazy`
  is data-writeback, by design: after a crash a file may contain stale bytes of a previously deleted file
  (a disclosure), so do not use `Lazy` or `Batch` on a volume that is not written through its journal where
  different users share it. On a journaled volume written through its journal this exposure is gone, as far
  as the tests show: small data is in the transaction, large data is written in place only into blocks no
  durable state references (blocks the group freed are treated as still referenced and their new data goes
  through the transaction) and a barrier orders it before the transaction (`TestGroupBatchAtomicCrashOracle`,
  its `delete-then-reuse` case is red without that rule: the surviving file read the deleted file's bytes).
  On the direct path, under `Mounted` and `Lazy` the journaled bit stays cleared for the whole held span, not
  one commit: after a crash in it the volume stays non-journaled until `diskutil enableJournal`; the group
  commit never clears it. `SetDurability` and `Batch` must not run concurrently with mutators.

  Three barriers is the minimum for a commit that leaves the volume clean: the dirty mark must be durable
  before a sector moves, and those sectors before the clean mark. `Mounted` is TN1150's own protocol
  ("An implementation must clear this bit on the media when it mounts a volume for writing ... set this
  bit ... as the last step of unmounting"): the mark is cleared once, sectors are written in place and made
  durable with one barrier, and `Sync`/`Close` restores the clean marks. Nothing inside a commit is ordered,
  the dirty mark is what protects it: a crash between `Sync`s leaves a volume that `fsck_hfs` checks at
  mount, never one that claims to be clean and is not. `Batch(fn)` runs `fn` lazily then `Sync`s: a group
  of mutations (an SMB write plus its times) pays one set of barriers. On the direct path it is not a
  transaction (a crash loses the group in whole or in part, with the volume marked dirty). **On a journaled
  volume written through its journal (v0.19.0) a Batch whose mutations fit one group is ATOMIC**: after a
  crash the volume shows all of it or none of it, file bytes included (`TestGroupBatchAtomicCrashOracle`,
  every crash prefix, both write orders; and macOS's own mount and `fsck_hfs` on sampled crash images,
  `TestDarwinJournalWriterBatchKernelReplay`). A Batch larger than one group is NOT atomic: it is a sequence
  of groups, each made of whole mutations, so a crash leaves a prefix of the mutations, never part of one
  (`TestGroupBatchLargerThanOneGroupLeavesAPrefix`). If `fn` returns an error the mutations it made stay and
  are committed by the Batch's `Sync`, as before; the error is returned. `Close` settles. A failed volume is left
  dirty. Checked by `TestDurabilityCrashModel` (both crash models plus every write of every fsync group
  landing alone or missing alone) and `TestDarwinCrashImagesFsck` at every level.

  Measured against macOS's own HFS+ driver (v0.12.2, 2026-11-16, 1-min load 4.19 to 4.75, 30 interleaved
  rounds, opt-in `measure_darwin_test.go` with `HFSPLUS_MEASURE=1`), create + 1 byte + fsync: macOS
  5.13 ms; `Strict` 12.66 ms (2.47x); `Mounted` 8.20 ms (1.60x); `Lazy` 0.095 ms (not durable until
  `Sync`). The same 6 barriers over plain fsync(2) cost 0.29 ms, so the gap is the `F_FULLFSYNC` count.
  Closing it is hfsplus#45 (journal writes); the design is `docs/design/journal-write.md`.

  Re-measured at v0.16.0 (2026-11-21, 1-min load 4.85 to 5.29 before and after, same test, 30 interleaved
  rounds, medians of rounds 2-30), create + 1 byte + fsync:

  | Driver | Time | Barriers |
  |---|--:|--:|
  | macOS, HFS+ | 5.24 ms | (kernel) |
  | macOS, Journaled HFS+ | 9.41 ms | (kernel) |
  | `Strict`, Journaled HFS+ image (direct path) | 13.41 ms | 3 |
  | `Mounted` (direct path) | 8.84 ms | 2 |
  | `EnableJournalWrites`, Journaled HFS+ image | **4.37 ms** | 1 |

  On a journaled image, journal writes are 3.1x faster than `Strict` and 2.2x faster than macOS's own driver
  on the same kind of volume. mkdir and Chtimes are metadata-only: they cost 4.24 ms and 4.27 ms, the same as
  `Mounted` (one barrier each). One run on one machine; the first round of each, which includes the session's
  first commit, is reported apart in the test log.

  Since v0.18.0 (PR 6 of hfsplus#45) the journal is the default at `Strict` and `Mounted` on a journaled
  volume, so on that volume both levels now run the code of the `EnableJournalWrites` row and cost what it
  costs; the `Strict` and `Mounted` rows above are what they cost on a journaled volume that falls back, and on
  a non-journaled one. **That the default path reproduces the 4.37 ms row has not been re-measured**: the
  measurement was deferred because the machine's 1-minute load was 63 to 82 at the time (the rule is below 8).
  The barrier counts, which do not depend on load, are asserted by `TestJournalWritesDefaultOn`: 2, 1, 1 at both levels.
- **Journaled volumes.** `fsck_hfs -q`, the quick check macOS runs before mounting, exits 0 on a
  journaled volume whatever the unmounted bit says (it trusts the journal), so on a journaled volume
  the cleared bit alone would not get a half-written volume checked. The commit therefore also clears
  `kHFSVolumeJournaledBit` in its first header write, whether or not the volume was cleanly unmounted at open
  (a journaled volume left dirty with an empty journal is a real state; the journal was empty at open
  and is never written), and the final header restores it. Measured with `fsck_hfs` on crash images of
  Go-made, Apple HFS+, Apple Journaled HFS+ and Apple Journaled HFS+ left dirty
  (`TestDarwinCrashImagesFsck`): every dirty image makes `-q` exit non-zero (for the volume dirty at
  open, every image with the journaled bit cleared mid-commit), every image still marked clean passes `-fn`. What macOS does with a volume left
  mid-commit (`TestDarwinMountOfCrashedJournaled`, opt-in): it mounts it read/write as plain
  "HFS+", not journaled, sets the unmounted bit when it unmounts it, and `fsck_hfs -fn` is clean
  afterwards; the journal is not re-enabled (the volume stays non-journaled until
  `diskutil enableJournal`) and nothing is lost. Whether macOS ran a full check at mount was not
  observed directly. Restoring the bit is safe in every state: the journal is never written.
- **A failed write** (a `WriteAt` or an fsync error after the first write of a mutation) leaves the disk
  in an unknown state, so the volume is latched: every later mutation returns `ErrVolumeFailed` and
  writes nothing until the volume is closed and opened again; reads still work. A failure before the
  first write (while writing the file's data) latches nothing.
- **`freeBlocks`** is counted from the bitmap at the first write of a volume (one pass over the bitmap,
  in chunks), then kept in step bit by bit, so a wrong count in the header is put right.
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

## Reading a journaled volume whose journal holds transactions

`Open(rs, size, hfsplus.WithJournalReplay())` (and `OpenFile(path, hfsplus.WithJournalReplay())`)
serves such a volume as macOS would see it after replaying the journal: the journaled blocks are
replayed in memory, over the image, and read from there. The image is not written and the volume
stays read-only; the size must be known (not -1). It is a port of Apple's `replay_journal`
(`hfs-715.100.10`, `core/hfs_journal.c`), so a transaction is read past the header's `end` while
sequence numbers continue, a torn block drops its transaction, and wraparound is handled. A bad
block list header inside the journal's `start`..`end` range drops that transaction AND the one
before it (the kernel restarts at the start of the last list that began a transaction); past `end`
a bad list just ends the read. Every size taken from the journal is bounded. The bound on the
replayed block data, `min(journal size, 256 MiB)`, is this driver's own resource limit, not
Apple's (the kernel streams blocks to the device): past it `Open` fails with `ErrReplayBudget`. A
journal the replayer refuses fails `Open` with `ErrJournalReplay` (which wraps `ErrJournaled`;
`ErrReplayBudget` wraps `ErrJournalReplay`).
Without the option, `Open` reads the blocks as they are on the medium, without the transactions.
Design: `docs/design/journal-write.md`, section 4; hfsplus#45.

### Writable opens replay it too (since PR 2 of hfsplus#45)

`OpenWritable`, `OpenWritableAt` and `OpenFileWritable` on a journaled volume whose journal holds
transactions **and whose `lastMountedVersion` is `HFSJ`** now replay the journal into the volume
instead of returning `ErrJournaled`, then carry on with the usual write path (including the `GoHF`
stamp of the first commit). What is written at open, in this order: the replayed blocks, **a
barrier**, the journal header with `start == end` (the header sector, checksum recomputed, sequence
number kept), **a barrier**. That is 2 barriers at open, at every durability level (the replay is
always made durable before the header that empties it, which is stricter than the kernel, whose
`write_journal_header` flushes only after the header). A crash leaves the volume as it was with its
journal still replaying, or the replayed volume with an empty journal, never an empty journal over
blocks that did not land. A journal that cannot be replayed (`ErrJournalReplay`, `ErrReplayBudget`)
is refused before anything is written. A `WriteAt` or `Sync` error part-way is crash-safe, not
"writes nothing": the journal stays non-empty and the next open replays to the same result. The
open's checks after the replay can still refuse a volume the replay rewrote (a replayed journal
info block that makes the journal unreadable), with the replayed blocks already written. Bytes
given to `OpenWritable(img, wa)` with a `wa` that has no `Sync` are not replayed (`ErrJournaled`):
without a barrier the order above cannot be kept, and an emptied journal over missing blocks is
declared by nothing (a torn commit of the write path is declared by the cleared unmounted bit).
The replayed blocks are bounded by the image size, as the kernel and `WithJournalReplay` do, not
by the old volume header's `totalBlocks`, because an interrupted `hfs_extendfs` journals the new
alternate header past the old end.

Still refused with `ErrJournaled`: a journal with transactions on a volume whose
`lastMountedVersion` is **not** `HFSJ` (it was last mounted by something else). TN1150 says such a journal must not be replayed, and
macOS throws it away at mount (`hfs_vfsutils.c:2886-2895`); this driver does not port that reset,
because discarding transactions is a loss of data the caller did not ask for. Mount and unmount the
volume on macOS to have the journal reset. Read-only `Open` WITHOUT `WithJournalReplay` shows the
blocks in place; `WithJournalReplay` shows the journal's view, which macOS would discard, not
what macOS will show. Also still refused: every journal `checkJournalAt` cannot verify.
Before v0.18.0 the driver did not write transactions by default: the write path bypassed the journal.

### Writing through the journal (on by default since v0.18.0, PR 3 and PR 6 of hfsplus#45)

**What changes for an existing caller (v0.18.0).** `OpenWritable`, `OpenWritableAt` and `OpenFileWritable`
now call `EnableJournalWrites` themselves. On a journaled volume whose journal passes its checks, `Strict`
(the default) and `Mounted` commits become journal transactions: fewer barriers (2 on a session's first
commit, 1 after, instead of 3, or 2 and 1 for `Mounted`), the volume reads `lastMountedVersion` `HFSJ`
(not `GoHF`) and not-cleanly-unmounted while the session runs, and `Close` writes a last transaction and
empties the journal, so **`Close` (or `Sync`) matters more than before**: a volume that is never closed is
left with a live journal that macOS replays at its next mount, which is correct but is not the clean image
the direct path left. `Mounted` on a journaled volume no longer keeps a dirty span. (Since v0.19.0
`SetDurability(Lazy)` and `Batch` are a group commit on a journaled volume: nothing is written until
`Sync`, `Close` or a level switch, then one transaction; no level switch empties the journal. For an existing
caller of `Lazy` or `Batch`: the volume is no longer marked dirty or written in place between `Sync`s, a Batch
that fits one group is atomic, and `Bytes()` of an in-memory journaled volume shows the image as of the last
flush, not the last mutation.) A non-journaled volume is written byte for byte as before (`TestNonJournaledVolumeUnchangedByDefault`).
When the checks fail (not journaled, no `Sync` on the writer, journal never initialised, `jhdr_size` not
512, ...) the open still succeeds, silently, and the volume writes directly exactly as in v0.17.0:
`(*Volume).JournalWritesReason()` returns nil when the journal is on and otherwise the reason, wrapping
`ErrJournalWritesUnavailable`. There is no opt-out switch: no caller in the go-filesystems organisation
used `EnableJournalWrites`; say so on hfsplus#45 if you need an in-place mode. `EnableJournalWrites()` stays, a no-op when
the journal is already on, and re-tries the checks after a fallback.

`v.EnableJournalWrites()` is what the opens call. When on, a **metadata-only commit at
`DurabilityStrict` or `DurabilityMounted`** on a journaled volume is written as a journal transaction instead of in place
first: **2 barriers on a session's first commit, 1 on every later one** (3 today). The first commit
also switches `lastMountedVersion` from `GoHF` to `HFSJ`, in the same commit, and clears the unmounted
bit as macOS does at mount; `Close` commits a last transaction whose headers carry the clean mark,
then empties the journal as macOS's unmount does (3 barriers, once; since PR 5 of hfsplus#45). `Sync` in
journal mode costs one barrier if in-place writes are pending since the last one, none otherwise. A crash leaves a journal macOS replays to the volume after the last commit that returned, or
the one in flight, whole (checked against macOS's own replay, and `fsck_hfs` after the mount; a crash inside `Close` is covered too). An **offline `fsck_hfs -fn` taken in the middle of a commit
cannot verify the image until macOS has mounted it**: on 407 of the 820 crash images measured it exits 8
("could not be verified completely"), always inside a commit's still-open group, and macOS mounts every one of
them and `fsck_hfs -fn` then exits 0. The volume is not inconsistent; why fsck's journal stage gives up
(`fsck_journal.c:517-624`) is not established.

**Cost.** Each barrier is `F_FULLFSYNC`. In one indicative run (load 4-5, 5-minute average 8.6) a steady-state
metadata commit (1 barrier) took 4.2 ms, the first commit of a session (2 barriers) 8.7 ms, against 12.8 ms for
today's strict path (3 barriers) and 8.6 / 10.3 ms for macOS's own create / mkdir on Journaled HFS+.
Since v0.16.0 a commit with small file data also takes 1 barrier: 4.37 ms for create + 1 byte + fsync
on a quiet machine, against 9.41 ms for macOS on Journaled HFS+ (table above). The design's 2.5-3 ms
estimate is still not met, because one `F_FULLFSYNC` costs about 4 ms here. A hypothesis, **not measured**: the kernel's journal barrier is
`DKIOCSYNCHRONIZE` with the barrier option (`hfs_journal.c:469-482`), roughly `F_BARRIERFSYNC` and cheaper
than `F_FULLFSYNC`; this driver does not offer it and this PR does not change the barrier.

**File data through the journal (PR 4 of hfsplus#45, v0.16.0).** A commit that writes file data
(`WriteFile`) is journaled too, at `DurabilityStrict`. The bound is `min(1 MiB, (journal size - jhdr_size) / 8)`
of data per commit. **Small data** (staged bytes within the bound) goes into the transaction with the
metadata: one barrier (2 on a session's first commit). **Large data** is written in place first, then a
barrier, then the metadata commit: two barriers (3 on the first). Staged data over the bound is spilled to
in-place writes. Blocks the revoke rule forces into the journal are staged without a bound of their own:
today the allocator keeps them within the small-data bound, and file creation is the only caller
(go-filesystems/hfsplus#61). **Revoke rule:** replay copies every block of every
transaction in the ring, in order, so a block journaled in T(k-2), T(k-1) or T(k) that is later freed and
reused as file data must not be written in place, or a replay would overwrite it with the old bytes. A
newly allocated data block inside that window is therefore journaled in the new transaction (the allocator
counts those against the bound and skips a block that would exceed it). A data sector that verifies as a
block list header is never put in the ring: the data takes the ordered path (in place, barrier first); if
a revoke-forced block carries one, the commit falls back to the direct path after emptying the journal.
Checked by crash oracles with byte checks (small, large, the revoke reuse, and the reverse write order) and
by macOS's own replay (`HFSPLUS_JOURNAL_ORACLE=1`).

**What falls back** to today's direct path, after emptying the journal (2 barriers) if a
transaction is live: a commit that carries a sector which verifies as a block list header (v0.15.1, hfsplus#56: replay follows such a
sector past the journal's end, and catalog nodes hold file names a user chooses; every 512-byte sector of
every journaled block is checked, in both byte orders, before the transaction is written, and the writer
never emits sequence number 0; a planted sector makes every commit that touches the poisoned catalog node take the direct path, whoever makes it; and, since v0.15.2 (hfsplus#58), the first journaled commit of a session first scrubs the ring: it reads the journal once, 64 KiB at a time, and zeroes every 512-byte sector that verifies as a block list header in either byte order (a sector a v0.15.0 session wrote before the guard, or another implementation left), with one extra barrier that makes the zeroes durable before the transaction lands, and only when a sector was zeroed. Unproven: whether the kernel's replay or `fsck_hfs` follows a forged list, since the proof covers this driver's replayer only), a commit
that changes more than 30 runs of 512-byte sectors, a run that is not whole sectors, or does not fit even an empty journal, (`DurabilityLazy` and `Batch` are a group commit since PR 7 of hfsplus#45, see "Durability levels"; a group the
journal cannot take is flushed and the commit goes the Strict way). A commit that fits an empty journal but not the live one (three transactions
must fit, TN1150 leaves one sector free) is **journal full**, no longer a fallback (PR 5): the journal is
emptied (2 barriers) and the commit goes in as a first commit (2 more), numbered previous + 1, with no
ring scrub. Every such fallback ("bypass") empties the journal first and writes `GoHF` as before. At `Mounted`
the fallback commit takes the `Strict` direct path (bracketed, three barriers), never the held dirty span: a volume
is never held dirty while it journals, so the clean marks `Sync`, `Close` and `SetDurability` write never land over a
live journal (`TestMountedFallbackNeverHeldWhileJournaling`); the next
journaled commit is a first commit with the session-start sequence bump (the design text says previous + 1
there; the bump is the more conservative and was kept). `EnableJournalWrites` itself returns `ErrJournalWritesUnavailable` and the
volume keeps writing directly when the writer given to `OpenWritable` cannot `Sync` (every barrier would be
a no-op; a nil writer or an `OpenFileWritable` file is fine), it is not journaled, the journal was never initialised, its
`jhdr_size` is not 512, or its size or `blhdr_size` fail the checks of the design. A transaction
uses a one-sector block list header (`blhdr_size` is set to `jhdr_size` in the first header), so it
holds at most 30 blocks. Design: `docs/design/journal-write.md`.

A commit with several candidates that exceeds 30 runs or does not fit the ring writes its data on the
ordered path instead (in place, barrier, then the transaction).

## API

| Function / method | Purpose |
|---|---|
| `Open(rs io.ReaderAt, size int64, opts ...OpenOption) (*Volume, error)` | Parse an HFS+/HFSX volume read-only from a `ReaderAt`; `WithJournalReplay()` replays a non-empty journal in memory (see above) |
| `OpenFile(path string, opts ...OpenOption) (*Volume, error)` | Open an image file read-only |
| `OpenWritable(img []byte, wa io.WriterAt) (*Volume, error)` | Open an in-memory image read/write |
| `OpenFileWritable(path string) (*Volume, error)` | Open an image file read/write in place (no copy of the image; each mutation writes and fsyncs what it changed) |
| `OpenWritableAt(rw ReadWriteSyncerAt, size int64) (*Volume, error)` | The same in-place open over a store the caller already holds (an `*os.File` opened with its own checks, an `os.Root`, a descriptor passed in), so the image is not reopened by name. `rw` stays the caller's: `Close` does not close it. Same `ErrJournaled` check. `size` must be at least `totalBlocks x blockSize` and a volume header must sit at the alternate location, 1024 bytes before the end of the *volume* (TN1150, "Volume Header"), else `ErrVolumeSize` and nothing is written; a file longer than the volume is written up to the volume's end only. After `Close`, mutators return `ErrClosed`. HFS-wrapped volumes are not supported |
| `Mkfs(size int64, cfg FormatConfig) ([]byte, error)` | Build a fresh empty HFS+/HFSX image in pure Go |
| `Format(path string, size int64, cfg FormatConfig) (filesystem.Filesystem, error)` | Pure-Go format + open read/write |
| `FormatAppleDmg(path, size, cfg)` | Optional darwin-only `hdiutil` alternative |
| `(*Volume) WriteFile / MkDir / DeleteFile / DeleteDir / Rename` | Mutate the volume |
| `(*Volume) SetLabel / Symlink / Truncate` | Optional `Labeller` / `Symlinker` / `Truncater` capabilities |
| `(*Volume) Sync() error` | Final fsync; at `DurabilityMounted`/`DurabilityLazy` also makes everything durable and restores the clean marks |
| `(*Volume) JournalWritesReason() error` | nil when commits go through the journal (default on a journaled volume, v0.18.0), `ErrReadOnly` on a read-only volume, else why the volume writes directly (wraps `ErrJournalWritesUnavailable`) |
| `(*Volume) SetDurability(Durability) error` / `Durability()` | Choose `DurabilityStrict` (default), `DurabilityMounted` or `DurabilityLazy`; see "Durability levels" |
| `(*Volume) Batch(fn func() error) error` | Run `fn` lazily, then one `Sync`; on a journaled volume one atomic journal transaction when `fn`'s mutations fit one group |
| `(*Volume) ListDir / ReadFile / Stat / ReadLink` | Read paths |
| `(*Volume) Chtimes` | `Chtimes(path, atime, mtime)`: contentModDate and accessDate as whole GMT seconds since 1904 (rounded down), attributeModDate (ctime) set to the clock, createDate untouched; a zero time leaves its field unchanged; a time outside 1904-01-01 00:00:01 .. 2040-02-06 06:28:15 GMT (the date 0 reads as "not recorded") is a `*TimeRangeError` (`errors.Is(err, ErrTimeOutOfRange)`), nothing written; the catalog record is patched in place, so the B-tree is unchanged in shape. Checked with `fsck_hfs` and a macOS mount |
| `(*Volume) Chmod`, `Chown` | with `Chtimes`, the whole `filesystem.MetadataSetter` (what SMB/NFS/SFTP servers probe for). `Chmod` replaces the 12 permission bits (setuid/setgid/sticky included) of `HFSPlusBSDInfo.fileMode`, keeping the type bits; `Chown` sets ownerID/groupID; both patch the catalog record in place (file or folder), set attributeModDate to the clock, refuse the root folder, and `Stat` now reports a folder's recorded mode. Checked with a macOS mount (`stat -f %Lp`) |
| `(*Volume) Times` | `filesystem.TimesReader`: createDate, contentModDate, attributeModDate (ctime), accessDate from the catalog record, GMT seconds since 1904, resolution 1 s; a zero date and the root folder are unrecorded |
| `(*Volume) OpenFile` (`filesystem.Opener`) | Positional reads of a data fork: each `ReadAt` reads only the extents of its window. A 1 MiB read at 66 MiB of a 72 MiB file allocates nothing through it and 75.5 MB through `ReadFile` |
| `(*Volume) CaseSensitive() bool` | Report HFSX binary key comparison |
| `ErrJournalWritesUnavailable` | `(*Volume).EnableJournalWrites`, and `(*Volume).JournalWritesReason()` after an open that fell back to direct writes, on a volume that cannot be written through its journal (not journaled, journal never initialised, `jhdr_size` not 512, checks failed); the volume keeps writing directly |
| `ErrReplayBudget` | `Open(..., WithJournalReplay())` where the blocks to replay exceed this driver's own bound, `min(journal size, 256 MiB)`; wraps `ErrJournalReplay` |
| `ErrJournalReplay` | `Open(..., WithJournalReplay())` on a journal the replayer cannot replay (the kernel would abandon it, or a size in it is outside the bounds); wraps `ErrJournaled` |
| `ErrJournaled` | `OpenWritable` / `OpenFileWritable` on a journaled volume whose journal is not provably empty (transactions pending on a volume whose `lastMountedVersion` is not `HFSJ` or that the replayer refuses, or unreadable / off-volume / other-device / bad checksum / a header field outside Apple's `journal_open` ranges since v0.12.3; three older checks stay stricter than Apple's, see hfsplus#48); `errors.Is(err, ErrJournaled)`, the reason is in the message. Nothing is written, except when a replay succeeded and a check after it refused the volume (the replayed blocks are then on the medium, the journal empty), or a writer without `Sync` was refused before any write |

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
