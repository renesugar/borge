<!-- SPDX-License-Identifier: Apache-2.0 -->

# Draft comment for golang/go#81029

Measured 2026-08-29 for `plans/PORTING_PLAN.md` §12.1i. **Not posted** — it belongs on the
issue under the account that filed it, so the person answering follow-up questions about
the machine is the person who ran it.

Everything below is one idle machine: Intel i5-9300H (Coffee Lake, 4 cores / 8 threads,
AES-NI, AVX2, **no** SHA-NI), Go's stdlib crypto, borg 2.0.0b23 for the reference numbers.

---

Some numbers from a real workload, since the reproducer in this issue measures the
single-block API in a tight loop and a mode implementation pays more than that.

Context: borge is a Go port of BorgBackup. It implements AES-256-OCB on `cipher.Block`
because that is the only way to get AES out of the standard library.

**Isolated encryption, 1 GiB, each tool's own `benchmark cpu`:**

| mode | borge (Go) | borg (OpenSSL) | ratio |
|---|---:|---:|---:|
| aes-256-ocb | **46.0 MB/s** | 881.6 MB/s | **19.2× slower** |
| chacha20-poly1305 | 260.2 MB/s | 447.9 MB/s | 1.7× slower |

The ratio against OpenSSL is not the interesting part — that invites "your Go code is just
slower". The interesting part is the **inversion inside the Go binary**:

- In OpenSSL, OCB beats ChaCha20-Poly1305 by ~2× (881.6 vs 447.9). That is what AES-NI
  hardware should do.
- In Go, OCB is **5.7× slower** than ChaCha20-Poly1305 (46.0 vs 260.2).

Same language, same runtime, same framing code, same corpus, same CPU. The only structural
difference is that `chacha20poly1305` is assembly end to end, while OCB has to go through
`cipher.Block` one 16-byte block at a time. The mode that should be fastest on this
hardware is the slowest.

**It is also worse than the bare API ceiling.** A `cipher.Block.Encrypt` on a single block
measures 103.6 ns here — about 154 MB/s. borge's OCB reaches 46 MB/s, roughly a third of
that, because a real mode also pays the offset update and two XORs per block. Those do not
pipeline either, and the reproducer's loop does not include them. Meanwhile stdlib
AES-GCM reaches 560 MB/s on the same CPU using its own private multi-block assembly — so
the hardware is not the limit, and the capability already exists inside `crypto/aes`; it is
just not reachable through the exported interface.

**What it costs a real backup.** Creating an archive from 854 MB of incompressible data,
compression off, comparing the two AEAD modes — they share HMAC-SHA-256 chunk ids, so the
cipher is the only difference:

| | AES-OCB | ChaCha20-Poly1305 | difference |
|---|---:|---:|---:|
| wall | 19.1 s | 16.6 s | +2.5 s (1.15×) |
| CPU | 52.3 s | 35.2 s | **+17.1 s (1.49×)** |

The isolated figures predict 15.3 s of additional CPU for that corpus; 17.1 s was measured,
within 12%. So the 46 MB/s is not a microbenchmark artifact — it is seventeen seconds of
CPU in a backup of under a gigabyte.

Worth noting *which* number is hidden: a worker pool and overlapped I/O mask most of the
cost in elapsed time (+15%) while the CPU cost is +49%. On a server that is a modest
slowdown. On a laptop or a phone it is battery and heat, and AES-OCB is the default mode
for the tool being ported.

A multi-block path on `cipher.Block` — or any exported way to reach the batched AES that
`crypto/cipher` already uses internally for GCM — would close this for every Go
implementation of OCB, EAX, SIV and anything else built on a block cipher, not just for
this one.
