# borg 2 repository format — reference for the borge port

Every citation below is `file:line` in the **pinned** upstream checkout:

```
repo    /home/renes/projects/borg
commit  114bd1e944c4ade6e512be20b36bcdd6398ad78e  (2026-08-16, master)
```

This document is the specification the port is written against and the thing later
stages check themselves *against*, rather than re-reading Python each time. It is
descriptive, not normative: **where it disagrees with the pinned source, the source
wins and this file is a bug.** Line numbers drift if the pin ever moves; re-verify
before trusting them after a rebase.

Coverage is deliberately partial — it documents what Stages 1–4 need. Archive and
item stream details are filled in when Stage 5 reaches them.

---

## 1. Repository layout

A borg 2 repository is a `borgstore` object store. Namespaces and their nesting
levels (`repository.py:684-692`):

| Namespace | Nesting | Contents |
| --- | --- | --- |
| `archives/` | `levels: [0]` — flat | one **empty** file per archive, named `archives/<hex archive id>` (`manifest.py:131,348`). The file name *is* the data; the directory listing is the archive directory. |
| `cache/` | `[0]` | repository-side caches, e.g. `cache/checked-packs` (`repository.py:481`) |
| `config/` | `[0]` | `config/readme`, `config/version`, `config/id`, `config/manifest` |
| `index/` | `[0]` | `index/<sha256 of content>` — persisted chunk indexes, incremental, merged/compacted |
| `keys/` | `[0]` | `keys/<digest>` — repokey key blobs (`repository.py:818`) |
| `locks/` | `[0]` | lock objects, shared and exclusive |
| `packs/` | `levels: [1]` | `packs/<xx>/<sha256 of content>` — pack files |

`packs/` is the only nested namespace: one level of hex-prefix subdirectory. Nesting
exists so a repository with millions of chunks does not put millions of entries in one
directory — the same problem borge is trying to solve on the *restore* side.

Objects in `packs/` and `index/` are **named by the SHA-256 of their content**
(`repository.py:998`). The name commits to the bytes, so a backend can verify a
download and a cache can be shared across repositories safely.

### 1.1 `config/` — the four files

| Object | Content |
| --- | --- |
| `config/readme` | exactly `"This is a Borg Backup repository.\nSee https://borgbackup.readthedocs.io/\n"` (`constants.py:275-277`) |
| `config/version` | the repository version as decimal ASCII: `"4"` (`repository.py:788-789`) |
| `config/id` | the 32-byte repository id as 64 lowercase hex characters (`repository.py:790`) |
| `config/manifest` | the manifest, as a `BORG_OBJ` blob (`repository.py:1746,1752`) |

> **Interop trap.** `Repository.open()` compares `config/readme` for **exact string
> equality** against `REPOSITORY_README` and raises `InvalidRepository` on any
> difference (`repository.py:869-872`). borge must write borg's text verbatim — it
> cannot write a "borge repository" readme without making the repository unreadable
> by borg, which would defeat the entire interop gate. Note this in the code where it
> will surprise someone.

`Repository.acceptable_repo_versions = (4,)` (`repository.py:749`).

### 1.2 Repository creation order

`Repository.create()` (`repository.py:779-800`): create the store, then store
`config/readme`, `config/version`, `config/id`, then write an empty chunk index to
`index/`. The last step is a performance measure, not a correctness one — it saves the
first operation from having to rebuild the index by listing every `packs/`
subdirectory. borge should reproduce it for the same reason.

---

## 2. The `BORG_OBJ` envelope (`repoobj.py`)

Every repository object — manifest, archive metadata, item stream chunk, file content
chunk — is wrapped in this envelope. Objects live inside pack files, concatenated.

### 2.1 Header — 49 bytes, little-endian

`repoobj.py:60-84`, struct format `"<8sB32sII"`:

| Offset | Size | Field |
| --- | --- | --- |
| 0 | 8 | `magic` = `BORG_OBJ` |
| 8 | 1 | `version` |
| 9 | 32 | `chunk_id` |
| 41 | 4 | `meta_size` (u32) |
| 45 | 4 | `data_size` (u32) |

Body: `meta_encrypted[meta_size] || data_encrypted[data_size]`.
Total object size: `49 + meta_size + data_size`.

### 2.2 Versions and AEAD additional authenticated data

| Version | Name | AAD |
| --- | --- | --- |
| `0x01` | `OBJ_VERSION_NO_HEADER_AAD` | `chunk_id` only. **Read-only** — borge must parse it, must not write it. |
| `0x02` | `OBJ_VERSION_HEADER_AAD` | `header_aad || slot_tag`. This is what `format()` writes. |

For `0x02`:

```
header_aad = magic(8) || version(1) || chunk_id(32)     # 41 bytes, REPOOBJ_HEADER_AAD_SIZE
meta AAD   = header_aad || "M"                          # META_AAD_TAG
data AAD   = header_aad || "D"                          # DATA_AAD_TAG
```

`meta_size` and `data_size` are deliberately **excluded** from the AAD, because they
are only known after encryption; tampering with either still fails authentication by
changing the ciphertext slice length (`repoobj.py:70-76`).

Getting the AAD wrong yields objects that borg rejects with an authentication failure
and no clue why. Test the AAD construction on its own, before anything else in Stage 3.

### 2.3 Metadata dict

`meta` is msgpack, encrypted into the meta slot. Keys observed in `format()`/`parse()`:

| Key | Meaning |
| --- | --- |
| `type` | one of the `ROBJ_*` values below; `parse()` rejects a mismatch |
| `size` | plaintext size |
| `csize` | compressed size — always the overall size |
| `ctype` | compressor id (see §3) |
| `clevel` | compression level |
| `psize` | *optional*, obfuscation only: payload size, `psize <= csize`; the bytes past `psize` are padding |

Object types (`constants.py:47-52`):

| Value | Constant | Meaning |
| --- | --- | --- |
| `"M"` | `ROBJ_MANIFEST` | manifest |
| `"A"` | `ROBJ_ARCHIVE_META` | archive metadata object |
| `"C"` | `ROBJ_ARCHIVE_CHUNKIDS` | a block of item-stream chunk ids |
| `"S"` | `ROBJ_ARCHIVE_STREAM` | item metadata stream chunk |
| `"F"` | `ROBJ_FILE_STREAM` | file content chunk |
| `"*"` | `ROBJ_DONTCARE` | parse without type assertion; never written |

### 2.4 Size limits (`constants.py:57,74-77`)

```
MAX_DATA_SIZE  = 20971479          # 20 MiB - 41, historical: borg <1.3 PUT header
MAX_OBJECT_SIZE = MAX_DATA_SIZE + 41
```

`put` raises `IntegrityError` above `MAX_DATA_SIZE` (`repository.py:1396-1397`).

### 2.5 `BORG_ASSERT_ID`

`repoobj.py:15-57`. Configurable places where `parse()` re-hashes the plaintext and
checks it equals the chunk id: `read`, `repair`, `transfer`, `rechunk`. Default is
`repair,transfer,rechunk` — i.e. **not** the hot read path, because for keyed modes
the AEAD already authenticates the payload against that specific chunk id.
`verify_data` (i.e. `borg check --verify-data`) always verifies and cannot be
switched off.

This is a real performance decision, not a detail: enabling `read` puts a full hash
pass over every byte of every restore. borge must reproduce both the default and the
env var, or its benchmarks will not be comparable to borg's.

For keys where `id_check_is_authentication` is true (the `none-*` modes, whose
envelope checksum is unkeyed), the id check always happens regardless.

---

## 3. Compression (`compress.pyx`)

Ids are format-visible; they are stored in `meta["ctype"]`.

| Id | Name | Notes |
| --- | --- | --- |
| `0x00` | `none` | |
| `0x01` | `lz4` | **raw LZ4 block format**, not the frame format. borg's default `LZ4_COMPRESSOR`. |
| `0x02` | `lzma` | |
| `0x03` | `zstd` | |
| `0x04` | `obfuscate` | wrapper: pads, sets `psize` |
| `0x05` | `zlib` | |
| `0x08` | `zlib_legacy` | borg 1.x; recognised via `detect()`, not by an id byte |
| `0xFF` | reserved, unused | |

Behaviours that must be ported, not approximated:

- **`DecidingCompressor`** (`compress.pyx:223`): compresses, and if the result is not
  smaller than the input, stores it as `none` instead. Whether a given chunk lands as
  `none` or as its nominal type is therefore format-visible.
- **`Auto`** (`compress.pyx:549`): probes with LZ4 first and only runs the expensive
  compressor if LZ4 found the data compressible.
- **`ObfuscateSize`** (`compress.pyx:630`): appends padding and records `psize`.
- `Compressor.detect(bytes([ctype, clevel]))` maps the two-byte compression header
  back to a class + level.

Note the split: for a `RepoObj`, the compression type/level live in the **metadata
dict**, and the compressed payload is *not* prefixed with type/level bytes
(`repoobj.py:150-152`). Borg 1 put them inline; borg 2 does not. Easy to port wrong.

---

## 4. Chunking (`chunkers/`, `constants.py:143-178`)

| Constant | Value |
| --- | --- |
| `CH_BUZHASH` | `"buzhash"` |
| `CH_BUZHASH64` | `"buzhash64"` |
| `CH_FASTCDC` | `"fastcdc"` |
| `CH_RABIN_AES` | `"rabin-aes"` |
| `CH_GOLDILOCKS_AES` | `"goldilocks-aes"` |
| `CH_TOEPLITZ_AES` | `"toeplitz-aes"` |
| `CH_FIXED` | `"fixed"` |
| `CH_FAIL` | `"fail"` (testing only) |

**Defaults:**

```
CHUNKER_PARAMS       = FASTCDC_PARAMS = (CH_FASTCDC, CHUNK_MIN_EXP, CHUNK_MAX_EXP, HASH_MASK_BITS, NC_LEVEL)
ITEMS_CHUNKER_PARAMS = (CH_FASTCDC, 15, 19, 17, NC_LEVEL)
```

`fastcdc` is borg 2's default for file content data — port it first. The item metadata
stream uses its own, smaller parameters.

`chunkers/reader.pyx` emits typed runs, not just bytes (`constants.py:178`):

```
CH_DATA, CH_ALLOC, CH_HOLE = 0, 1, 2
```

Sparse-file awareness lives here. Treating a hole as data silently inflates the
repository; treating data as a hole silently corrupts a restore. Both are invisible in
casual testing, so the Stage 7 synthetic corpus must include sparse files.

C implementations exist upstream for the hot chunkers (`fastcdc_impl.c`,
`buzhash64_impl.c`, `rabin_aes_impl.c`, `goldilocks_aes_impl.c`,
`toeplitz_aes_impl.c`). Per plan §0.4 these are ported to Go; C is revisited only if
Stage 9 measurement demands it.

---

## 5. Keys (`crypto/key.py`, `constants.py:238-268`)

The type byte identifies **only the ciphersuite**. Where the key is stored (keyfile
vs repokey) is *not* encoded in it — that decoupling is new in borg 2 and is worth
preserving deliberately (`constants.py:253-256`).

### 5.1 Types in scope for borge

| Type | `ENC_NAME` | id hash | envelope |
| --- | --- | --- | --- |
| `0x10` | `aes256-ocb` | HMAC-SHA-256 | AEAD |
| `0x20` | `chacha20-poly1305` | HMAC-SHA-256 | AEAD |
| `0x30` | blake3 + AES-OCB | BLAKE3 | AEAD |
| `0x40` | blake3 + ChaCha20-Poly1305 | BLAKE3 | AEAD |
| `0x60` | `authenticated-sha256` | HMAC-SHA-256 | keyed MAC tag, no encryption |
| `0x70` | `authenticated-blake3` | BLAKE3 | keyed MAC tag |
| `0x80` | `none-sha256` | SHA-256 | unkeyed checksum |
| `0x90` | `none-blake3` | BLAKE3 | unkeyed checksum |

`0x50` is **reserved and must be rejected**: it was a borg 2 beta format dropped
before release (borg #9104), and the byte is deliberately not reused
(`constants.py:261-263`).

### 5.2 Legacy types — out of scope

`0x00` `KEYFILE`, `0x01` `PASSPHRASE`, `0x02` `PLAINTEXT`, `0x03` `REPO`,
`0x04` `BLAKE2KEYFILE`, `0x05` `BLAKE2REPO`, `0x06` `BLAKE2AUTHENTICATED`,
`0x07` `AUTHENTICATED` — all borg 1.x, all AES-CTR + HMAC. Per plan §0.6 borge does
not read borg 1 repositories, so these appear only as values borge must **recognise
and reject with a clear message**, never as codepaths.

Note `key.py:674-675`: a blake2b id key from borg 1.x is 128 bytes and is not
supported any more; hmac-sha256 and blake3 both use 32.

### 5.3 Derivations

- **Session key** (AEAD modes, `key.py:1434-1440`): domain
  `b"borg-session-key-" + CIPHERSUITE.__name__`, cipher header length `1+1+6+24`,
  `aad_offset=0`.
- **Envelope MAC key** (`authenticated-*` modes, `key.py:1193-1204`): derived from
  `crypt_key`, **not** from `id_key` — chunk ids are public, so a MAC keyed on the id
  key would be forgeable by anyone who can see the repository. Domains:
  `b"borg-repoobj-mac-hmac-sha256"` (`key.py:1255`) and `b"borg-repoobj-mac-blake3"`
  (`key.py:1296`).
- **Passphrase KDF**: argon2id with borg's `ARGON2_ARGS` and `ARGON2_SALT_BYTES`
  (`key.py:615-623`).

### 5.4 Storage

- **repokey**: the key blob lives at `keys/<digest>` in the store
  (`repository.py:818,837,852`).
- **keyfile**: in borg, `~/.config/borg/keys/`. borge uses `~/.config/borge/keys/`
  (plan §0.5) — the *contents* are interoperable, the location is not shared.

Blob structure is `EncryptedKey` (`item.pyx:390-416`): `version`, `algorithm`,
`iterations`, `salt`, `hash`, `data`, `argon2_time_cost`, `argon2_memory_cost`,
`argon2_parallelism`, `argon2_type`, `label`.

The decrypted key is `Key` (`item.pyx:431-450`): `version`, `repository_id`,
`crypt_key`, `id_key`, `chunk_seed`, `tam_required` (legacy — borg 2 always requires
TAM implicitly).

---

## 6. Chunk index (`hashindex.pyx`)

```
key:   32 bytes (the chunk id)
value: flags:u32 || size:u32 || pack_id:[32]byte || obj_offset:u32 || obj_size:u32
```

`ChunkIndexEntryFormat = (flags="I", size="I", pack_id="32s", obj_offset="I", obj_size="I")`
(`hashindex.pyx:39-41`). 48 bytes of value per 32-byte key.

Flags (`hashindex.pyx:47-56`):

| Flag | Value | Meaning |
| --- | --- | --- |
| `F_NONE` | `0` | |
| `F_USED` | `1 << 0` | chunk is referenced |
| `F_COMPRESS` | `1 << 1` | chunk shall be (re-)compressed |
| `F_PENDING` | `1 << 2` | pack location not resolved yet |
| `F_NEW` | `1 << 24` | **system** flag: not yet present in `index/` |
| `M_USER` | `0x00ffffff` | user-flag mask |
| `M_SYSTEM` | `0xff000000` | system-flag mask, always shown as 0 to callers |

System flags are masked out by `hide_system_flags()` before reaching callers — the
distinction is part of the API, not an implementation detail.

`size` is the **plaintext** chunk size, filled in by the cache layer, not by
`PackWriter.add()` which writes 0 (`repository.py:200`).

The underlying table is `borghash.HashTableNT` — an external package.
`UNKNOWN_INT32` / `UNKNOWN_BYTES32` (`constants.py`) are the placeholders for pack
location fields that are not yet known.

At the reference scale (recipedb: 1,623,610 unique chunks) this table holds ~130 MB of
entries. That is why it is an open-addressed table with a fixed value layout and not a
`map[[32]byte]entry`; reproduce the structure, not just the semantics.

---

## 7. Packs (`repository.py`, `PackWriter` / `PackReader`)

A pack is the plain concatenation of complete `BORG_OBJ` blobs, stored at
`packs/<xx>/<hex sha256 of the whole pack>`.

### 7.1 Sizing (`constants.py:68,74`, `repository.py:886-899`)

```
DEFAULT_PACK_MAX_SIZE = 50 * 1000 * 1000     # 50 MB (decimal), size-bound by default
MIN_PACK_SIZE         = DEFAULT_PACK_MAX_SIZE // 50   # 1 MB
PACK_READER_CACHE_SIZE = 3                   # LRU of whole-pack readers
```

Environment overrides: `BORG_PACK_MAX_COUNT`, `BORG_PACK_MAX_SIZE`. Setting
`max_count` switches off the default size bound unless `max_size` is also given.
`BORG_PACK_ASYNC=no` disables the background store thread; `BORG_PACK_TRACE=yes`
prints its lifecycle markers.

### 7.2 The write-side concurrency invariant

`PackWriter` (`repository.py:127-190`) keeps **at most one pack store in flight**.
The rule that makes it safe, stated in the upstream docstring and worth restating in
the Go port:

> the `ChunkIndex` is only ever touched by the calling thread; the store-thread's
> results (or error) are applied when it is joined.

Consequences a port must preserve exactly:

- `add()` returns the *previous* pack's results while the current store is in flight.
- A store error surfaces **one pack later**, from whichever `add()`/`flush()` joins.
- On failure, the failed pack's chunk ids are deleted from the index
  (`_apply_outcome`), and the still-buffered pieces are dropped (`_drop_buffered`),
  so no `F_PENDING` leftovers survive into the index persisted at close.
- `flush()` is a barrier: after it, nothing is buffered, nothing is in flight, and no
  chunk written through the writer is `F_PENDING`.

In Go this is a goroutine plus a channel, which is easier to write than the Python —
and easier to get subtly wrong in a way that only shows up under load on a large
repository. `make race` exists for this.

### 7.3 The read side

`PackReader.iter_headers()` (`repository.py:377-421`) walks the fixed 49-byte headers
to locate every object without reading payloads — one short range read per object.
Error rules, which are load-bearing:

- a **trailing partial header** is a clean end of pack, not corruption;
- a bad magic, or an object extending past the pack, raises `IntegrityError` and must
  **not** silently end the walk. Upstream explains why: a truncated walk would rebuild
  a chunk index that is quietly missing the rest of the pack, and `borg check --repair`
  would then "fix" the archives by dropping those chunks. A wrong repair is worse than
  a loud failure.

`check_pack_objects()` (`repository.py:423+`) validates the offset-ordered
`(obj_offset, obj_size)` ranges of a pack's indexed objects: an overlap, or an object
ending past the file, is index corruption.

---

## 8. Manifest (`manifest.py`)

- `MANIFEST_ID = b"\0" * 32` (`manifest.py:476`) — stored at `config/manifest` as a
  `BORG_OBJ` with `ro_type = ROBJ_MANIFEST`.
- `version = 2`.
- **`manifest["archives"]` is always empty in borg 2** (`manifest.py:141-142`): the
  `archives/` namespace *is* the archive directory. This is a structural change from
  borg 1 and it is what makes archive listing a directory listing.
- `item_keys` moved into `manifest["config"]`; the top-level `item_keys` is read only
  for borg 1.x compatibility (`manifest.py:521-523`).
- `timestamp`, `config` (a `StableDict`).

An archive is registered by storing an **empty** object at
`archives/<hex archive id>` (`manifest.py:348`). Delete is a soft-delete via
`store_move(..., delete=True)` (`manifest.py:353`); `undelete` reverses it
(`manifest.py:358`); hard delete is `store_delete(..., deleted=True)`
(`manifest.py:363`). borge's `store` layer must implement soft-delete for `undelete`
to work at all.

---

## 9. Items and archives (`item.pyx`) — outline

Filled in properly when Stage 5 reaches it. Recorded now because it constrains the
`item` package in Stage 1.5.

`ArchiveItem` v2 fields (`item.pyx:467-489`): `version`, `name`, `item_ptrs` (list of
chunk ids of blocks of item-stream chunk ids), `command_line`, `hostname`, `username`,
`start`, `end`, `tags`, `comment`, `chunker_params`, `size`, `nfiles`, `cwd`.
The legacy `items`, `cmdline`, `recreate_cmdline`, `time_end` fields are borg 1.x.

`Item` fields (`item.pyx:245-299`), with their encodings:

- **surrogate-escaped str**: `path`, `target`, `user`, `group`, `hostname`,
  `username`, `command_line`. Python stores non-UTF-8 POSIX bytes via
  `surrogateescape`. Go's byte-sequence strings are the *easier* model, but the
  mapping must be reproduced exactly or such paths will not round-trip.
- **int (ns) via msgpack ext**: `atime`, `ctime`, `mtime`, `birthtime`.
- **bytes**: `acl_access`, `acl_default`, `acl_extended`, `acl_nfs4`, `hlid`.
- **int**: `mode`, `uid`, `gid`, `rdev`, `bsdflags`, `size`, `inode`, `nlink`.
- **list**: `chunks`, `chunks_healthy`.
- **`StableDict`**: `xattrs` — sorted key order, because the packed bytes are hashed.
- **legacy**: `source`, `hardlink_master`, `part`.

`REQUIRED_ITEM_KEYS = {"path", "mtime"}`;
`REQUIRED_ARCHIVE_KEYS = {"version", "name", "item_ptrs", "command_line", "time"}`
(`constants.py:10,33`). `ITEM_KEYS` must be kept complete or `RobustUnpacker`
malfunctions during `check --repair`.

---

## 10. Open questions

Tracked here rather than in code comments, so they are visible before the stage that
depends on them starts.

| # | Question | Blocks |
| --- | --- | --- |
| 1 | Exact on-disk serialization of `borghash.HashTableNT` (header, capacity, load factor, tombstones) — it is in the `borghash` package, not in borg. | Stage 1.6 |
| 2 | `borgstore` soft-delete representation on the posixfs backend (name mangling? sidecar?). | Stage 2 |
| 3 | `storelocking` object naming, contents and the stale-lock timeout rules. | Stage 3 |
| 4 | `index/` incremental write and merge/compaction algorithm. | Stage 3 |
| 5 | Exact argon2 parameters in `ARGON2_ARGS`, and the `hash` field's construction in `EncryptedKey`. | Stage 4 |
| 6 | The `chunk_seed` role in chunker initialisation. | Stage 1.4 |

Each is answered by reading the pinned source when its stage starts, and this document
updated in the same commit.
