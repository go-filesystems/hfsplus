# Design: write journaled volumes through their journal (hfsplus#45)

Status: proposal; no code yet. This note answers the questions #45 asks. The
goal is to make a durable small write on a journaled HFS+ volume cost one
`F_FULLFSYNC` per commit, as macOS's own driver does, without weakening the
guarantee `DurabilityStrict` gives today.

Measured at v0.12.2 (hfsplus#35, closing comment), create + 1 byte + fsync:

| Side | time | barriers per commit |
|---|---|---|
| macOS driver | 5.13 ms | see section 3 |
| strict (today) | 12.66 ms | 3 |
| mounted (today) | 8.20 ms | 2 with data, 1 without |

One `F_FULLFSYNC` costs about 2 ms on that machine. Over fsync(2), the same six
barriers cost 0.29 ms (#35). The gap is the barrier count.

Every claim below cites a source that was opened. A claim that could not be
checked in a source is marked **UNVERIFIED**.

## 1. Sources

- Apple TN1150, "HFS Plus Volume Format": sections *Journal*, *Journal Info
  Block*, *Journal Header*, *Block List Header*, *Replaying the Journal*,
  *Journal Checksums*, *Volume Attributes* (`kHFSVolumeUnmountedBit`,
  `kHFSVolumeJournaledBit`), and the `lastMountedVersion` field of *Volume
  Header*.
- apple-oss-distributions/hfs at tag `hfs-715.100.10` (commit d1bac2f):
  - `core/hfs_journal.h` (structures, lines 47-124, 197-205);
  - `core/hfs_journal.c`:
    - `write_journal_header` (457-542)
    - `replay_journal` (1162-1541)
    - `check_free_space` (2403-2516)
    - `journal_allocate_transaction` (2521-2567)
    - `end_transaction` (3796-4075)
    - `finish_end_transaction` (4104-4418)
    - `journal_flush` (4689-4769)
    - `journal_close` (2252-2356)
    - `buffer_flushed_callback` (597-769)
    - `get_io_info` (1617-1640)
    - `journal_open` (1876-2140)
    - `size_up_tbuffer` (1557-1614)
    - `update_fs_block` (818-880)
  - `core/hfs_vfsutils.c`:
    - `hfs_flush` (3483-3546)
    - the mount-time dirty mark (706-710)
    - late journal init when `lastMountedVersion != 'HFSJ'` (723-727)
    - `JOURNAL_RESET` in late init (2886-2895)
  - `core/hfs_vfsops.c`:
    - early journal init, gated on `lastMountedVersion == 'HFSJ'` (1575-1610)
    - `hfs_flushvolumeheader` journaling both headers and writing `'HFSJ'`
      (3678-3731, 3931-3958)
    - `HFS_REPLAY_JOURNAL` sysctl and `hfs_journal_replay` (3061-3077,
      4492-4511)
  - `core/hfs_format.h:59-61` (`kHFSPlusMountVersion`, `kHFSJMountVersion`,
    `kFSKMountVersion`);
  - `core/hfs_vnops.c` (`hfs_fsync` full-sync path, 3227-3242);
  - `core/hfs_btreeio.c:230` and `core/VolumeAllocation.c:2875` (B-tree nodes
    and bitmap blocks go through `journal_modify_block_start`);
  - `lib_fsck_hfs`:
    - `fsck_journal.c` (fsck's own replay, 180-338, 466-643)
    - `fsck_journal.h:52-59`
    - `dfalib/SVerify1.c:98-166` (`CheckIfJournaled`) and `:4514-4545`
      (`journal_replay`, delegated to the kernel)
    - `dfalib/SControl.c:599-680` (quick check, journal replay into fsck's
      cache)
    - `check.c:258-270`
- This repository at c22284d:
  - `store.go` (`flush`, `fsync`, `writeData`)
  - `durability.go` (`flushHeld`, `settle`, `Batch`)
  - `hfsplus.go` (`OpenWritable*`, `Sync`, `Close`)
  - `journal.go`
  - `crash_test.go`, `crash_darwin_test.go`

## 2. What the driver does today

`OpenWritable*` refuses a journaled volume whose journal is not empty
(`checkJournalAt`, journal.go). The driver never writes the journal.

For the span of each strict commit, it clears `kHFSVolumeJournaledBit` and
`kHFSVolumeUnmountedBit` in the primary header. The reason: `fsck_hfs -q`
reports any journaled volume clean (SControl.c:610-625, "We lie for journaled
file systems"). A strict commit (`store.go: flush`) then pays three barriers:

1. data, plus the header with both bits cleared, then fsync;
2. B-tree and bitmap sectors, then fsync;
3. the alternate header, then the primary header with the bits restored, then
   fsync.

`Mounted` and `Lazy` (`durability.go: flushHeld`) clear the bits once and
restore them in `settle`. After a crash, the volume stays without its journal
until `diskutil enableJournal`.

## 3. What macOS pays, and how

`hfs_fsync` with `F_FULLFSYNC` calls `hfs_flush(HFS_FLUSH_FULL)`
(hfs_vnops.c:3233). That calls `journal_flush(JOURNAL_FLUSH_FULL)`
(hfs_vfsutils.c:3498-3511), which does the following:

1. It forces the buffered transaction out with `end_transaction(force)`
   (hfs_journal.c:4730).
2. It writes the block list header and the blocks at the journal's `end`
   (`finish_end_transaction`, 4253-4258).
3. It calls `write_journal_header(jnl, 0, …)`. That issues a
   `DKIOCSYNCHRONIZE` **before** writing the header (467-483) and **no** flush
   after it. The flush after the write happens only when `updating_start` is
   set (523).
4. It issues the in-place writes asynchronously (`buf_bawrite`, 4355), with no
   flush.
5. Back in `journal_flush`, it issues one full `DKIOCSYNCHRONIZE` only if
   step 3 did not already do a full one (`flush_count == jnl->flush_counter`,
   4757-4758).

The flush count per `F_FULLFSYNC` depends on the device:

- without `DK_FEATURE_BARRIER`: one full flush;
- with it: a barrier-only flush plus a full flush (477-480, 1637-1638).

**UNVERIFIED:** whether a disk image attached by `hdiutil` reports
`DK_FEATURE_BARRIER` or `DK_FEATURE_FORCE_UNIT_ACCESS`. As a result, the
number of flushes macOS issues per fsync on a disk image is also UNVERIFIED.
The measurement plan in section 10 counts them.

Three consequences matter for us.

**The header is not durable when fsync returns.** The transaction is durable,
but the header that names it may not be. macOS relies on the replay reading
**past `end`**: `replay_journal` keeps reading block lists after `end` while
their checksum is valid and their sequence number is the previous one or the
previous one + 1 (1211-1300, `check_past_jnl_end`). This only works if the
durable header has `start != end`, because `replay_journal` returns at once on
an empty journal (1184-1186). macOS advances `start` lazily, only when space is
needed (`check_free_space`, 2433-2500) and at `journal_close` (2317-2320), so
the journal is usually non-empty while mounted. It is not always non-empty,
though: when no older transaction is pending, `check_free_space` sets
`start = active_start` (2493), and that can equal `end`. In that corner, a
transaction past `end` whose header write did not land before a crash is not
replayed. Our design does not inherit this, because invariant C (section 5)
keeps the durable header non-empty from the first commit on.

**The header must never be durable before the transaction it names.** Suppose
the header covers a block list whose checksum is bad. If that block list is not
the first, replay restarts with `end = txn_start_offset` (1413-1439). That is
the offset of the last first-block-list seen, so the restart also drops the
previous, intact transaction. If the bad block list is the first one, replay
fails (1259-1262), the mount writes `lastMountedVersion = 'FSK!'`
(hfs_vfsops.c:1605-1630), and fsck runs. The pre-header flush in step 3 exists
to prevent this.

**`start` advances are ordered by a flush after the header** (`updating_start`,
523-539). TN1150 *Replaying the Journal* states the same rule: "You must
ensure that previous block writes complete before updating the journal
header's `start` field on disk". macOS itself issues no flush between in-place
completion and the header write that advances `start`. It relies on I/O
completion (`buffer_flushed_callback`, 649-692), and completion is not
durability. We do not copy that. Our `start` advances only past transactions
whose in-place writes an earlier barrier made durable.

## 4. The on-disk format we write

Sources: hfs_journal.h:47-124; TN1150 *Journal Header*, *Block List Header*
and *Journal Checksums*; fsck_journal.h:52-59.

**Journal header** (`journal_header`). Its fields are in the byte order that
its `endian` field states; the driver keeps the existing order.

- Fields: `magic` 0x4a4e4c78, `endian` 0x12345678, `start`, `end`, `size`,
  `blhdr_size`, `checksum`, `jhdr_size`, `sequence_num`.
- The checksum is `calc_checksum` over the first 44 bytes
  (`JOURNAL_HEADER_CKSUM_SIZE = offsetof(sequence_num)`,
  hfs_journal.h:135), computed with the field zeroed. TN1150 says "the
  header", but the code is what macOS verifies, and journal.go already checks
  the 44-byte form against a header macOS wrote.
- The header fills one sector (`jhdr_size`) and is written whole.

**Transaction.** One block list, written at `end`.

- `block_list_header`:
  - `max_blocks` (u16) = `blhdr_size/16 - 1` (`journal_max_blocks`, 544-548);
  - `num_blocks` (u16) = 1 + the number of blocks;
  - `bytes_used` (i32) = `blhdr_size` + the sum of the `bsize` values;
  - `checksum`;
  - `flags` = `BLHDR_CHECK_CHECKSUMS | BLHDR_FIRST_HEADER` (2557). TN1150
    calls this field `pad`.
- `binfo[0]`: `bnum` = 0, `bsize` = 0, and the third word holds this
  transaction's `sequence_num` (4140). TN1150 calls that word `next`.
- `binfo[i]`, for i ≥ 1:
  - `bnum` = byte offset / `jhdr_size`. fsck converts it with
    `bnum * blkSize`, and `blkSize` "is defined to be the size of the journal
    header" (fsck_journal.c:262-263, 330).
  - `bsize` = a multiple of `jhdr_size`.
  - `cksum` = `calc_checksum` over the block's bytes (4231).
- The bytes after the used `binfo` entries, up to `blhdr_size`, are 0x5a, as
  macOS writes them (2552). The blocks follow in `binfo` order.
- The block list header checksum covers its first 32 bytes, computed with the
  field zeroed: the 16-byte header plus `binfo[0]` (`BLHDR_CHECKSUM_SIZE`, 130
  and 4142-4143; TN1150 *Block List Header*). `binfo[1..]` are **not**
  covered.

**The block list header is one sector.** Because `binfo[1..]` are not covered
by the checksum, a block list header spanning several sectors can be torn in a
way that still verifies. Take a header written past `end` where sector 0 lands
but sectors 1.. still hold the block list a previous lap wrote at the same
offset. The checksum passes, and the stale entries come with their own stale
checksums. If the stale data is also still at the same offsets, those
checksums pass too, and replay writes stale blocks.

The guard:

- In `H_1`, the first header of a session, written while the journal is empty,
  we set `blhdr_size = jhdr_size`. Every block list header is then one sector,
  and it lands whole or not at all under the same one-sector assumption as the
  journal header (section 5).
- Changing `blhdr_size` while the journal is empty is safe: nothing is live
  that was written with the old size, and replay reads the size from the
  header (1215).
- macOS itself chooses `blhdr_size = phys_blksz` when its transaction buffer
  is small (1608-1609), and it recomputes the value at its own next open
  (1607-1613).
- fsck reads the block list header as one `jhdr_size` block
  (fsck_journal.c:176-182) and finds the data at `blhdr_size`
  (fsck_journal.c:274).
- **UNVERIFIED:** that both replayers accept a journal we re-sized this way.
  The oracle asserts it.

The cost of the guard is capacity. A block list holds 30 entries with
`jhdr_size = 512` (`512/16 - 1 - 1`) and 254 with 4096. Each entry is one
contiguous run, of any length that is a multiple of `jhdr_size`, so adjacent
changed sectors of one region merge into one entry. Widening a run to the
whole region (a B-tree node, a bitmap block) is allowed when that keeps the
count under the cap. A commit that still needs more entries takes the bypass
path (section 7).

The fallback, if the oracle refuses a re-sized journal, is to keep the volume's
`blhdr_size` and cap `num_blocks` so that every used entry lies in sector 0. It
gives the same 30 or 254 entries. The guard also removes a cost: with a
macOS-sized `blhdr_size` of up to 96 KiB (section 4, "Sizes"), every commit
would write a 96 KiB block list header.

**One block list per transaction.** The format allows chained block lists
(TN1150; 2855-2911), but we do not write them. If the second of two chained
lists past `end` is torn, replay stops there and applies the first one alone,
which tears the transaction (1244-1253). A transaction that does not fit one
block list or the journal takes the bypass path (section 7).

- **Sizes.** A volume arrives with whatever `blhdr_size` macOS last chose.
  `size_up_tbuffer` sets it to `tbuffer_size / jhdr_size * 16` (1607). The
  transaction buffer is 128 KiB only below 256 MB of RAM. Above that it is
  RAM / 256 MB × 128 KiB (1563-1576), capped at `size/2` and 3 MiB (1599-1605).
  So a macOS-written `blhdr_size` can reach 96 KiB. We read it from the header
  and never assume it, and we replace it at session start (above).

**Blocks.** Blocks are `jhdr_size`-aligned runs. Each changed run of a region
is widened to `jhdr_size` boundaries, and the widened bytes are read through
the overlay. With `jhdr_size = 512`, this is exactly today's `changedRuns`.
The two volume headers are journaled like any other block, as macOS does
(hfs_vfsops.c:3678-3679, 3936-3947).

**Wraparound.** The journal buffer is `[jhdr_size, size)`. A write that
reaches `size` continues at `jhdr_size`, and a block list header or a block may
be split there (TN1150 *Journal*; `do_journal_io` 341-347 and 423-431; replay 1403-1405 and
1443-1446).

**The journal is never perfectly full** (TN1150 *Journal*).

- Free space is `size - jhdr_size - used`.
- A transaction must leave at least one sector free (`free_space`, 2382-2396,
  and the strict `>` at 2419).
- `start` and `end` stay multiples of 512 and within `(0, size]`
  (`journal_open`, 2019-2057).

**Sequence numbers.**

The header's `sequence_num` is not a safe base. After a kernel replay, the
header is rewritten with the sequence number it already had
(`write_journal_header(jnl, 1, jnl->jhdr->sequence_num)`, 1510). The
transactions replayed from past `end` carried higher numbers, and
`journal_open` restarts numbering from that header value (2109). Replay past
`end` accepts a sequence number equal to the previous one, as well as one
greater (1268-1271; fsck the same, fsck_journal.c:590-593).

So "header + 1" can reproduce a number an earlier session used. Here is how
that goes wrong. Our first transaction `T_1` gets `s + 1` and is written at
`end`. If `T_1` is large enough to wrap up to an older transaction that also
carried `s + 1`, the replayer reads past `T_1` and accepts the stale
transaction as its successor.

Rule: the first transaction of a session gets
`header.sequence_num + size/jhdr_size + 1`. This is what macOS does when it
rewrites a journal after a block-size change (2097-2099, without the random
term, which we do not need). Each later transaction gets the previous number
plus one. A transaction that would get 0 gets 1 instead.

Why this is enough:

- Every block list occupies at least one `jhdr_size` sector, so the journal
  holds at most `size/jhdr_size` block lists at any time.
- The only numbers the header can lag behind are those of block lists
  physically present in the journal: the ones replayed from past `end`, and
  torn ones that replay rejected. There are at most `size/jhdr_size` of them,
  numbered consecutively from `header.sequence_num + 1`.
- So the first number of the new session is greater than every number still
  in the journal.
- Replay accepts a successor only if its number equals the previous one or
  the previous one + 1. Every number of the new session is at least the
  first, so no stale block list can follow a new one.
- The header's `sequence_num`, written in `H_k`, is `seq(T_{k-1})`, the newest
  transaction it covers (as at 4289). The empty header written at Close
  carries `seq(T_c)`, as `journal_close` writes `jnl->sequence_num` (2320).

**Exception: a session whose `H_1` never landed.** The proof above misses one
case. Suppose a session crashes in group 1 or 2, before `H_1` is durable. It
leaves its `T_1`, numbered `header + N + 1`, at `end`, behind the unchanged
empty header. The next session computes the same number and writes its own
`T_1` at the same offset, because `end` did not move.

We keep the arithmetic bump and accept this case, rather than scanning the
ring, because the case is harmless:

- While the durable header is empty, nothing is replayed. The kernel returns
  at once (1184-1186), and fsck replays only a non-empty journal
  (SControl.c:661).
- The new `T_1`'s block list header is one sector (above), so it replaces the
  old one at that offset in a single write.
- The old `T_1` cannot be written past without `H_1` landing, so it is the
  only leftover. When the new `H_1` lands, it covers the new `T_1`, and replay
  reads past `end(T_1)` into what is left of the old `T_1`'s blocks. Those
  bytes are block contents, not a block list header, so the read-past-end
  stops there.

The last point holds only while those blocks are not crafted to look like a
block list header (section 11, risk 5).

The numbers are 32 bits, and each session spends at least `size/jhdr_size` of
them. 1270-1271 compare with plain equality, so the argument should survive a
wrap, with 0 skipped. **UNVERIFIED:** the kernel's behavior across a 32-bit
wrap was not exercised.

**Replayer (PR 1) transaction boundaries.** A transaction starts at a block
list with `BLHDR_FIRST_HEADER` set (1330-1331, 2557), as the kernel does. It
does not end when `binfo[0].next == 0`, as TN1150 *Replaying the Journal*
says, because the kernel stores the sequence number in that word (4140), so
it is never 0 on a current journal.

**Journal validity at open.** These checks come on top of today's:

- `256 KiB ≤ size ≤ 1 GiB` and `size % jhdr_size == 0` (1905-1922);
- `size ≥ jhdr_size × (jhdr_size / 16)` (1910-1916): 16 KiB at 512, 1 MiB on a
  4Kn device;
- the volume is not embedded in an HFS wrapper. `bnum` counts device sectors
  (`update_fs_block` reads `fs_block` on `fsdev`, 826), and for an embedded
  volume that would add the wrapper's offset. The writable opens already
  refuse wrapped volumes (`enableWrites`, hfsplus.go:232-236);
- `blhdr_size` is a positive multiple of `jhdr_size`;
- `jhdr_size` equals the sector size the driver writes in (512 or 4096).

If any check fails, the volume is not written through the journal and takes the
bypass path.

## 5. The commit: ordering and barrier count

Notation:

- `T_k`: the k-th journal transaction of a session.
- `H_k`: the journal header written in commit k.
- A **group**: the writes between two barriers. The medium may apply them in
  any order and may tear them.

Invariants:

- **A.** A durable header never covers a transaction that is not durable.
- **B.** A durable header never has `start` past a transaction whose in-place
  writes are not durable.
- **C.** From the first commit on, the durable header covers at least one
  transaction (`start != end`). This keeps replay reading past `end`.

### `lastMountedVersion` must be `'HFSJ'` while the journal is maintained

macOS replays a journal only if the volume header it reads at mount, in place
and before any replay, says `lastMountedVersion == 'HFSJ'` and the volume is
journaled. This is early journal init (hfs_vfsops.c:1583-1590). In every other
case, mount takes late init (hfs_vfsutils.c:723-727), which opens the journal
with `JOURNAL_RESET` and throws its transactions away (hfs_vfsutils.c:2886-2895):

> if we weren't the last person to mount this volume then we need to throw
> away the journal because it is likely that someone else mucked with the disk.

`journal_open` then sets `start = end` without replaying (hfs_journal.c:2067-2070).
TN1150 *Journal* states the rule:

> If the `lastModifiedVersion` field of the volume header does not match the
> signature of an implementation known to properly use and update the
> journal, then the journal must not be replayed.

The constants are in hfs_format.h:59-61.

The #32 PR (in progress) writes `lastMountedVersion = 'GoHF'` on each commit.
This is correct for today's driver, which clears the journaled bit and does not
maintain the journal. TN1150 asks that any code that modifies the volume
identify itself in this field.

**The journal-writing PR must switch the field to `'HFSJ'` at the same moment
it starts maintaining the journal, and not before.**

- Keeping `'GoHF'` while writing transactions would make macOS discard every
  committed transaction after a crash. The in-place blocks those transactions
  were repairing would stay torn, and nothing would tell fsck. The volume says
  journaled, so `-q` passes it (SVerify1.c:148-157 treats only a non-`'HFSJ'`
  version combined with a cleared unmounted bit as unjournaled).
- Writing `'HFSJ'` earlier, while the driver still bypasses the journal, would
  claim a journal that does not describe the volume.
- Every bypass commit (section 7) and every non-journaled volume keeps
  `'GoHF'`.

### First commit of a session (2 barriers)

The journal is empty at open (`checkJournalAt` requires it), so invariant C
does not hold yet.

- **Group 1:**
  - `T_1` at `end`;
  - file data (section 6);
  - the primary header **in place**, changed only in
    `lastMountedVersion = 'HFSJ'`, unmounted bit cleared, journaled bit set.
    macOS writes the same at mount (hfs_vfsutils.c:706-710,
    hfs_vfsops.c:3728-3729).

  Then **barrier 1**.
- **Group 2:** `H_1 = [start(T_1), end(T_1))` with `seq(T_1)`. Then
  **barrier 2**.
- Then `T_1`'s blocks are written in place, with no barrier.

Group 1 changes no structure, so a crash in group 1 leaves a consistent volume
with an empty journal.

### Every later commit k ≥ 2 (1 barrier)

- **Group k:**
  - `T_k` at `end(T_{k-1})`;
  - `H_k = [start(T_{k-1}), end(T_{k-1}))` with `seq(T_{k-1})`;
  - `T_{k-1}`'s in-place writes, already issued after barrier k-1.

  Then **barrier k**.
- After barrier k, `T_k`'s blocks are written in place, with no barrier, and
  the call returns. The commit is durable.

`H_k` never names `T_k`. Replay reaches `T_k` by reading past `end`, the same
way macOS reaches the transaction its last `F_FULLFSYNC` wrote. `H_k` advances
`start` past `T_{k-2}`. Its in-place writes went out after barrier k-2 and were
made durable by barrier k-1, so invariant B holds.

**Invariant D: `T_k` must not overwrite `T_{k-2}`.** `H_k` and `T_k` are in the
same group, so while group k is in flight the durable header may still be
`H_{k-1} = [T_{k-2}]`. Replay then starts at `start(T_{k-2})`. If `T_k`
wrapped over `T_{k-2}`'s block list, replay would either:

- fail at the first block list (1259-1262), so the mount writes `'FSK!'`; or
- restart with `end = start` (1434-1435) and replay nothing, leaving
  `T_{k-1}`'s torn in-place writes unrepaired.

The live region of the journal during commit k is therefore `T_{k-2}`,
`T_{k-1}` and `T_k`. Free space for `T_k` is measured from `start(T_{k-2})`,
not from `start(T_{k-1})`.

### Crash argument against today's strict guarantee

Today's guarantee: every commit is durable when its mutator returns. A crash
anywhere leaves either a consistent volume marked clean, or a volume marked
dirty that fsck repairs.

The new guarantee is stronger. After the kernel replays, the volume holds
exactly the commits that returned, possibly plus the one in flight, whole.
Nothing needs repair. Two replay paths reach the device, and both are the
kernel's:

- **The mount.** Early journal init calls `journal_open` → `replay_journal`
  (hfs_vfsops.c:1590).
- **A writable `fsck_hfs`** (not `-n`, not `-q`). It calls
  `journal_replay(block_device)` (check.c:258-270). That is a
  `sysctl(HFS_REPLAY_JOURNAL)` on its write descriptor (SVerify1.c:4514-4538),
  which the kernel serves with `hfs_mountfs(..., journal_replay_only = 1)`
  (hfs_vfsops.c:3061-3077, 4492-4505), so the per-block checksums apply.

fsck's own replay (fsck_journal.c), which skips per-block checksums, writes
only into fsck's cache. It runs only when fsck cannot write
(`canWrite == 0 || writeRef == -1`, SControl.c:652-655). So it can make
`fsck_hfs -n` report damage the kernel would never produce (below), but it
never writes a torn `T_k` to the device. The review that prompted this
paragraph read check.c:258-270 as fsck writing its own replay. SVerify1.c:4536
shows the replay is delegated to the kernel. The oracle checks the writable
path anyway (section 9).

| Crash point | Durable on the medium | What macOS replays | Result |
|---|---|---|---|
| group 1 | maybe the header with `'HFSJ'`; empty journal | nothing (1184) | the volume as at open. Dirty and journaled, so `-q` passes it, and it is consistent |
| group 2 | `T_1`; maybe `H_1` | `T_1` if `H_1` landed; otherwise the journal is empty and nothing is replayed | before or after commit 1, whole |
| group k, `H_k` not landed | `H_{k-1}` = [`T_{k-2}`]; `T_{k-1}` | `T_{k-2}` (idempotent); then, past `end`, `T_{k-1}` (seq+1, checksums good); then `T_k` only if its block list and every block checksum are intact | `T_{k-1}`'s torn in-place writes are overwritten. State is after k-1, or after k |
| group k, `H_k` landed | `H_k` = [`T_{k-1}`]; `T_{k-1}` | `T_{k-1}`; then, past `end`, `T_k` as above | same |
| `T_k` torn, block list header bad | — | past `end`, a bad checksum stops replay cleanly (1245-1253) | after k-1 |
| `T_k` torn, header good but a block bad | — | `bad_txn_handling` with `txn_start_offset = start(T_k)` (lines 1330-1331 run before the block checks); the restart uses `end = start(T_k)` (1434-1439) | after k-1 |
| after barrier k | `H_k`, `T_k` | `T_{k-1}`, `T_k` | after k; durable on return |

**One-sector atomicity.** `H_k` is a single sector written whole, so the design
assumes the medium does not tear one sector. TN1150 makes the same assumption:
the header occupies "exactly one sector so that it can be updated atomically".
For our stores (a file on APFS under `F_FULLFSYNC`) this is **UNVERIFIED**. The
harness models sector-granular tearing.

**fsck before mount.** On an unmounted crash image, `fsck_hfs -fn` replays a
non-empty journal into its own cache (SControl.c:652-680) and also reads past
`end` (fsck_journal.c:520-546, 581-601). It does **not** check per-block
checksums (fsck_journal.c:269-338). So for an image where `T_k` has an intact
block list header and a torn block, fsck applies the torn `T_k` and the kernel
drops it. macOS's own crash images share this behavior. The oracle (section 9)
therefore takes the kernel's replay as the reference and runs fsck after the
mount.

**Measured in PR 3 (what the torn-block prediction above does not explain).** Of
the 820 distinct crash images of the writer's k = 1..4 sequence, 407 make a
pre-mount `fsck_hfs -fn` exit non-zero ("could not be verified completely"), and
the kernel mounts every one of them. They are not torn blocks: all 407 lie inside
the still-open group, before its barrier, of commits 2 to 4 (none in commit 1,
none after a barrier), and 286 of them have `T_k` complete. The flip is a single
event: in-order@19 exits 0 and in-order@20, where the first sector of `T_2` lands
past `end`, exits 8 before any block is checked. fsck's journal stage gives up on
that image; the volume is not inconsistent. So an offline `fsck_hfs` taken in the
middle of a commit cannot verify the image until macOS has mounted it (the mount
replays and empties the journal); `fsck_hfs -fn` after the mount exits 0 on all 820. The
root cause, in `fsck_journal.c:517-624` (the loop that reads past `end`), is **not**
established: this is the measurement, not the explanation.

**fsck -q.** On a journaled volume with `'HFSJ'`, the quick check reports
clean whatever the unmounted bit says (SControl.c:610-625,
SVerify1.c:148-157). This is correct here, because the journal brings the
volume back. The current crash test asserts the opposite for mid-commit images
(crash_darwin_test.go:41-46), so that assertion moves to the bypass path only.

## 6. File data

TN1150 *Journal* says the journal "does not protect the contents of a fork".
Today's strict guarantee does protect them: data is durable before the
metadata that points at it, so a crash never shows a file with stale bytes.

With one barrier, a crash in group k can still replay `T_k` (whole, past
`end`) while the data blocks it points at did not land. Three rules keep the
guarantee.

**Small data is journaled.** A commit's new data blocks, up to a bound, go into
`T_k` as ordinary blocks. The proposed bound is the smaller of 1 MiB and
`(size - jhdr_size) / 8`. That keeps three consecutive transactions under the
journal-full threshold of section 7. They are written in place after the barrier, like
metadata. The data is then atomic with its metadata, for 1 barrier. The 1-byte
benchmark falls under this rule.

**Large data stays ordered.** The data is written in place, then a barrier,
then the commit above: 2 barriers, as `Mounted` pays today. The extra 2 ms is
noise next to the I/O of a large write.

**Revoke rule.** A block journaled by a transaction that replay can still
reach must not be overwritten in place by unjournaled data. Otherwise replay
would put the old journaled contents over it; this is the classic revoke
problem.

The replay window is **three** transactions, `T_{k-2}`, `T_{k-1}` and `T_k`.
While group k (or the data group before it, on the large-data path) is in
flight, the durable header may still be `H_{k-1} = [T_{k-2}]`, and replay
starts there (section 5, invariant D).

Example: a block is journaled in `T_{k-2}`, freed in `T_{k-1}`, then reused as
large unjournaled data in commit k. A crash before barrier k replays
`T_{k-2}`'s old bytes over it.

The rule: a newly allocated data block that is in the journaled set of
`T_{k-2}`, `T_{k-1}` or `T_k` is journaled too. If that would exceed the
small-data bound, the allocation skips the block instead. The set covers three
transactions of at most 30-254 entries each, so it stays small. macOS's `journal_kill_block` covers only
the open transaction (2981). How macOS handles this across transactions is
**UNVERIFIED**.

## 7. Checkpointing, journal full, bypass

**In-place writes.** `T_k`'s in-place writes are issued right after barrier k
and made durable by barrier k+1. Reads in between see them, because `putAt`
drops the read cache, as today.

**`start` advance.** Every commit's header advances `start` past `T_{k-2}`
(section 5), at no extra barrier.

**Free-space rule (invariant D).** During commit k the live journal is
`T_{k-2}`, `T_{k-1}` and `T_k`. `T_k` fits only if
`size(T_{k-2}) + size(T_{k-1}) + size(T_k) < size - jhdr_size`, which leaves
the one free sector TN1150 requires.

**Journal full.** When `T_k` does not fit:

1. A barrier makes `T_{k-1}`'s in-place writes durable.
2. Write `H = [end(T_{k-1}), end(T_{k-1}))`, an empty journal, with
   `sequence_num = seq(T_{k-1})`, then a barrier.
3. Run the first-commit sequence for `T_k` (2 barriers). It continues the
   numbering with `seq(T_k) = seq(T_{k-1}) + 1`, not with a session-start
   bump: every number still in the ring is at most `seq(T_{k-1})`, so the
   argument of section 4 holds without one.

That is 4 barriers.

**How often.** Wrapping around the circular buffer costs nothing; only the
three-transaction fit does. With one-sector block list headers (section 4), a
small commit is about 6-8 KiB (block list header, a catalog node or two, a
bitmap sector, two volume headers, journaled data). Three of them fit any
journal macOS accepts (≥ 256 KiB, 2035). Journal-full therefore needs three
consecutive transactions averaging more than a third of the journal. The
small-data bound in section 6 is set to keep a run of small writes under
that, so a steady stream of small writes never takes this path.
**UNVERIFIED:** "rare" for mixed workloads. The churn test counts the
journal-full commits.

**Effect of invariant D on the barrier plan.** Counting from `start(T_{k-2})`
instead of `start(T_{k-1})` adds no barrier to a commit that fits. It only
lowers the transaction size at which journal-full triggers, from about half
the journal to about a third. The steady state is still 1 barrier, and the
estimate in section 10 does not move.

**Bypass.** The bypass applies when `T_k` cannot be one block list, cannot fit
an empty journal, or the journal failed the checks of section 4.

1. Empty the journal, as in journal-full steps 1-2. The empty `H` carries
   `sequence_num = seq(T_{k-1})`, the newest journal transaction. The next
   journaled commit after the bypass continues from that number + 1.
2. Write `lastMountedVersion = 'GoHF'`.
3. Run today's strict path: clear the bits, 3 barriers, restore them.

The 1314-image review covered today's strict path. **UNVERIFIED:** that it
stays correct when it is entered right after a journal is emptied, and when it
writes `'GoHF'`. The review never ran that sequence, so the oracle adds it.
The next commit after a bypass is a first commit again.

The bypass is also taken when a commit needs more block list entries than one
sector holds (section 4).

**Sync.** In journal mode every commit is already durable. Sync issues one
barrier if in-place writes are pending (cheap insurance, not needed for
correctness), and nothing otherwise.

**Close.** Close follows macOS's unmount (`journal_close`, 2252-2320):

1. Commit `T_c`: both headers with the unmounted bit set.
2. Barrier.
3. Write `T_c` in place.
4. Barrier.
5. Write an empty `H`, with `start = end = end(T_c)` and
   `sequence_num = seq(T_c)`, as `journal_close` does (2317-2320).
6. Barrier.

That is 3 barriers, once. The volume is left as macOS leaves it: an empty
journal, the clean mark set, and `'HFSJ'`.

**Failure.** A failed write or barrier latches `v.failed`, as today. The journal
on the medium is whatever the last barrier left, and replay handles it. No
further write is issued, and the header is never "repaired" by hand.

## 8. Non-journaled volumes, durability levels, Batch

**Not journaled** (the attribute bit is clear, or `kJIJournalNeedInitMask` is
set): nothing changes. Strict stays at 3 barriers, and Mounted (1.60×) remains
the faster choice, as #45 says. Initialising a journal (`NeedInit`) is out of
scope.

**Strict on a journaled volume** takes the journal path of section 5. Strict
is the default, so the default gets the gain.

**Mounted on a journaled volume** takes the same journal path. It is at least
as fast as today's Mounted and needs no dirty span. On non-journaled volumes,
Mounted keeps its current meaning.

**Lazy on a journaled volume** (later PR): commits accumulate into one
in-memory transaction until Sync or a size bound. The regions are already
kept through the overlay. Each flush is one journal commit (1 barrier), and it
is atomic.

**Batch** then becomes a real transaction when its mutations fit one block
list: all or nothing after a crash. Today Batch is documented as "not a
transaction" (durability.go:96). Until the Lazy PR lands:

- Batch keeps today's behavior;
- the journal is emptied before switching to a held level (`SetDurability`
  already settles).

**Switching levels.** A switch between the journal path and a bypass level
always goes through "empty the journal" (2 barriers). The two paths never
interleave.

## 9. What macOS must accept: the replay oracle

The oracle reuses the crash-image harness of #37/#38 (`instrument`, the event
log, `attachRaw`, `fsckExit` in crash_darwin_test.go).

**Inputs.**

- Image kinds: `apple-jnl` and `apple-jnl-dirty`.
- A multi-commit sequence, k = 1 to 4: the first commit and the steady state.
- **A wraparound sequence.** Enough commits to wrap the journal at least
  twice, on the smallest journal macOS accepts. The crash points sit where:
  - `T_k` straddles the end of the buffer;
  - `T_k` lands right behind `T_{k-2}` (invariant D at its limit);
  - `T_k` overwrites a previous lap's block list at the same offset, with
    that stale list's sectors and data still in place (section 4, the
    one-sector guard).

  It also includes a session that starts on a journal a kernel replay left
  with a lagging `sequence_num` (section 4). The base images are made by
  macOS.
- **Journal full and bypass.** A transaction too large for the free space
  (journal full), and one with too many entries (bypass), each followed by
  ordinary commits.
- **Revoke.** A block journaled in `T_{k-2}`, freed, and reused as large data
  in commit k.

**Crash images:**

- every prefix of the event log;
- for the open group, every subset of its writes;
- every torn sector of `T_k`, bounded to the first, middle and last sector of
  each block and of the block list header.

**Checks for every crash image:**

1. **Kernel replay.** `hdiutil attach` the image with mounting, which runs
   `journal_open` → `replay_journal`. Then unmount and detach.
2. **fsck.** `fsck_hfs -fn` on the result must exit 0.
3. **Durability.** Open the result read-only with this driver and walk it. It
   must hold every commit whose barrier completed before the crash point, and
   no commit may be partly present. The harness knows the mutations, so it
   checks names, sizes and bytes; for file data, it also checks for stale
   bytes.
4. **No `'FSK!'`.** Read `lastMountedVersion` after detach: it must say
   `'HFSJ'`. `'FSK!'` would mean the mount judged our journal bad.
5. **Writable fsck path.** On a second copy of the crash image, run a
   writable `fsck_hfs -fy` on the raw device. That replays through
   `sysctl(HFS_REPLAY_JOURNAL)` (section 5). Then run `fsck_hfs -fn` and the
   durability walk of step 3. The result must equal step 1's.
6. **Secondary check.** Run `fsck_hfs -fn` on the raw image before mounting.
   Its result is recorded but not asserted: PR 3 measured 413 of 820 images
   exit 0 and 407 exit 8, all of the latter inside a commit's open group
   (section 5), where an offline fsck cannot verify the image until it is
   mounted. Not a torn-block effect, and not a defect of the volume.

**Cost control.** The full set is darwin-only and opt-in, like
`TestDarwinCrashImagesFsck`. On linux, CI runs the same images against this
driver's own replayer (PR 1). That replayer is the second oracle, and it is
checked against the kernel on darwin.

## 10. Test plan and measurement plan

**Tests (all platforms):**

- **Journal-format round trip.** Encode, then decode with our replayer. Check
  agreement with a journal macOS wrote: let macOS commit, kill the process
  before `journal_close`, and capture the volume attached with `-nomount`.
  How to stop the checkpoint is **UNVERIFIED**; the fallback is
  `fs_usage`/dtrace to log the transactions.
- **Wraparound units:** a block list header split across the end, a split
  block, and `end == size`.
- **Accounting:** free space at the boundary (the journal is never full),
  sequence numbers, and the revoke set.
- **Crash model.** The model of crash_test.go, extended with the replay step,
  under both existing models ("fsynced-only", "in-order"), plus write subsets
  within the open group.
- **Fuzzing.** A fuzz target for the replayer, which parses untrusted input,
  with its corpus committed. It checks these bounds:
  - `bytes_used`;
  - `num_blocks ≤ max_blocks`;
  - `bnum * jhdr_size + bsize` inside the volume, with a 64-bit overflow
    check on the product.

  A bomb test asserts a refusal under `GOMEMLIMIT`.

**Measurement.** Use `measure_darwin_test.go` with `HFSPLUS_MEASURE=1`, 30
interleaved rounds, a 1-minute load ≤ 8, and a clean clone of the tag.

- Add a `journal` column, and count barriers per commit from the instrumented
  writer.
- **Expected:** about 1 × 2.1 ms plus about 0.3 ms of driver work and writes,
  so **≈ 2.5-3 ms** for create + 1 byte + fsync.
  - That beats macOS's 5.13 ms if macOS really pays two flushes per fsync on
    the image.
  - It is parity, about 5 ms, if macOS pays one flush plus its own overheads.
  - Which case holds is the UNVERIFIED point of section 3.
- **Measured in PR 3 (indicative: 1-minute load 4-5, 5-minute 8.6, one run).**
  The 2.5-3 ms figure is **not** met. A steady-state metadata commit (Chtimes,
  1 barrier) costs 4.2 ms median, so one `F_FULLFSYNC` costs about 4 ms on that
  run, not 2.1; the first commit (2 barriers) costs 8.7 ms; `DurabilityMounted`,
  which does not make each operation durable, costs 4.2 ms for the same
  Chtimes. macOS's Journaled HFS+ costs 8.6 ms (create) and 10.3 ms (mkdir). So
  the journal path at its best equals the mounted level in time while
  keeping per-commit durability, and the gap that is left is the price of
  `F_FULLFSYNC` itself.
- **Hypothesis, NOT measured:** the kernel's journal barrier is
  `DKIOCSYNCHRONIZE` with the barrier option (`hfs_journal.c:469-482`), which is
  closer to `F_BARRIERFSYNC` than to `F_FULLFSYNC`, and so cheaper than the
  barrier this driver issues. If true, the gap to the kernel's per-commit cost
  is the barrier primitive, not the protocol. Measuring it was deferred (load 12
  when tried), and PR 3 does **not** change the barrier: `F_FULLFSYNC` stays
  the only primitive offered, as in the write path (README).
- The first commit of a session costs 2 barriers, so the benchmark reports its
  first round separately.
- The review fixes (invariant D, the three-transaction revoke window,
  one-sector block list headers, the sequence bump) add no barrier to a
  commit that fits. The one-sector header shrinks each commit's journal write
  from up to 96 KiB to a few KiB. The estimate is unchanged.
- Count macOS's own `DKIOCSYNCHRONIZE` calls per fsync on the image
  (dtrace/`fs_usage`) to settle the UNVERIFIED point of section 3.

## 11. Risks, and what could make it slower than mounted

1. **The look-past-end dependency.** Durability of `T_k` on return rests on
   macOS replaying past `end` (1211-1300) and on fsck doing the same
   (fsck_journal.c:520-601). That behavior is in the code but not in TN1150,
   and Apple could change it.
   - Mitigation: the oracle asserts the behavior on every macOS version the CI
     runs.
   - Fallback, one switch away: write `H_k` covering `T_k` after a second
     barrier. That costs 2 barriers, the same as mounted with data.
2. **Disagreements between the replayers.**
   - The kernel drops a transaction with a torn block. fsck, run before mount,
     does not.
   - The kernel drops the previous transaction when a covered block list
     header is bad (1413-1439). Our ordering never produces that state.
   - A stale block list from an earlier lap, carrying the same sequence
     number or that number + 1, would be replayed. The session-start bump of
     `size/jhdr_size` (section 4) rules this out.
   - A multi-sector block list header torn over a previous lap's list could
     pass with stale entries. One-sector block list headers (section 4) rule
     this out, under the one-sector-atomicity assumption, which is
     UNVERIFIED.
3. **Slower than mounted.**
   - Small data is written twice (journal + in place), and large data pays 2
     barriers.
   - Journal-full (4 barriers) and bypass (3 + 2) are slower than mounted's
     1-2. Invariant D makes journal-full start at about a third of the
     journal per transaction instead of a half.
   - Bypass also catches commits with more than 30 entries at
     `jhdr_size = 512`, for example a large rename touching many B-tree
     nodes.
   - Close costs 3.
   - With a small journal (256 KiB) and a write-heavy batch, journal-full
     could dominate. Measure this with the churn test, and size the small-data
     bound from the journal.
4. **`lastMountedVersion`.** It must say `'HFSJ'` in place before the first
   transaction, or macOS resets the journal (hfs_vfsutils.c:2886-2895), and it
   must switch from #32's `'GoHF'` in the same PR that starts writing the
   journal (section 5).
5. **User bytes in the journal (security).** Small-data journaling (section 6)
   puts file contents a user controls into the journal ring. Replay reads past
   `end` into stale bytes, possibly a previous lap's journaled data. A crafted
   sector there could pass as a block list header: a valid 32-byte checksum,
   a guessable sequence number (`last` or `last + 1`), entries pointing
   anywhere on the volume, and data matching its own checksums. After a crash,
   replay would write those sectors. macOS does not journal file data, so it
   has no such exposure.

   This must be designed out before PR 4. Candidates: journal only data
   blocks that cannot hold a plausible block list header (reject a block
   whose first 32 bytes verify as one), or keep file data out of the journal
   and pay the large-data path's 2 barriers. Open, not accepted.
6. **`jhdr_size` ≠ 512 (4Kn).** Blocks widen to 4 KiB, and write amplification
   grows. The tests need a 4Kn image; whether `hdiutil` supports one is
   **UNVERIFIED**.

## 12. Incremental PRs

1. **Read side: replayer and format.**
   - Decode the journal header (all fields), the block list headers and the
     blocks. Transaction boundaries follow `BLHDR_FIRST_HEADER`, not TN1150's
     `binfo[0].next` rule (section 4).
   - Replay into an overlay.
   - Add a fuzz target and corpus, and the fixture macOS wrote.
   - `Open` gains an option to replay a non-empty journal in memory,
     read-only.
   - No write-path change.
2. **Replay on open for writing.** `OpenWritable*` replays a non-empty journal
   instead of returning `ErrJournaled`, but only with `'HFSJ'`, as TN1150
   requires. It makes each block durable before emptying `start` (TN1150
   *Replaying the Journal*). The kernel replay is the oracle.
3. **Journal writer behind a flag.**
   - The encoder, free-space accounting, wraparound and sequence numbers.
   - The commit of section 5, for metadata-only commits.
   - The `'GoHF'` → `'HFSJ'` switch, made exactly when the journal path is
     taken.
   - When enabled, Strict on a journaled volume uses this path.
   - The crash oracle of section 9.
4. **File data.** Small-data journaling, the large-data ordered path, and the
   revoke set. The crash oracle with byte checks.
5. **Journal full, bypass, Close/Sync, and switching levels.** The crash oracle
   across the wrap.
6. **Default on** for Strict and Mounted. The measurement table, plus README,
   docs site and landing page.
7. **Lazy/Batch as an in-memory group commit** (atomic Batch).

Each PR keeps today's behavior for non-journaled volumes, byte for byte.
