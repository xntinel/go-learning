# Authenticated Encryption with AES-GCM and ChaCha20-Poly1305 — Concepts

You will almost never design a cipher. You will constantly make the decisions
around one that decide whether it is secure in production: where the nonce comes
from, what context you bind as associated data, how many messages one key may
safely encrypt, how to change algorithms without a flag-day migration, and how
to encrypt an object too large for a single `Seal` call. This lesson treats
authenticated encryption with associated data (AEAD) as an application-side
building block — encrypting a PII column, a session blob, an uploaded file —
rather than TLS on the wire. The through-line is misuse resistance: every
exercise forces you to reason about nonce uniqueness and integrity boundaries
the way a reviewer would in a real crypto pull request, and to encode ciphertext
as a self-describing, versioned envelope so the scheme is still operable and
rotatable years later.

## What AEAD guarantees, and what it does not

An AEAD primitive gives you two things at once: confidentiality of the
plaintext, and integrity/authenticity of both the ciphertext *and* the
associated data. `Open` either returns the exact plaintext that was sealed, or
it returns an error; there is no third outcome where you get back subtly
corrupted bytes. That single property — "authentic or nothing" — is why AEAD
replaced the old encrypt-then-separately-MAC hand assembly that was so easy to
get wrong.

What AEAD does *not* give you is freshness, ordering, or non-replay. The library
authenticates a bag of bytes; it has no idea that the bytes are last week's
session token being replayed, or that chunk 4 of a stream arrived before chunk
3, or that the final chunk was silently dropped. Those are application-layer
properties. The library authenticates *which* bytes; the application must decide
*which context* to bind and how to prevent replay. Nearly every real AEAD bug is
a failure at that seam, not a failure of the cipher.

## The nonce contract is absolute

Go's `cipher.AEAD` documents the rule verbatim: the nonce "must be NonceSize()
bytes long and unique for all time, for a given key." Unique for all time. The
nonce need not be secret and need not be random — only unique. You can publish
it, store it next to the ciphertext, and derive it from a counter; none of that
weakens anything. The one thing you may never do is repeat it under the same
key.

Reuse is not a graceful degradation; it is catastrophic. For GCM and for
ChaCha20-Poly1305, encrypting two messages under the same key and nonce leaks
the XOR of the two plaintexts (so an attacker who knows or guesses one recovers
the other), and — worse — it exposes the polynomial authentication key (GHASH
for GCM, Poly1305 for ChaCha). Once that authentication key leaks, the attacker
can forge *arbitrary* messages that your `Open` will accept as authentic. A
single nonce reuse therefore breaks both confidentiality and integrity, for the
whole key, not just for the two colliding messages.

## Two safe nonce strategies and their limits

There are exactly two disciplined ways to satisfy the contract.

Random nonces. Draw a fresh nonce from `crypto/rand` for every message. This is
simple and stateless, but it is bounded by the birthday problem: with a 96-bit
(12-byte) GCM nonce, the probability of a collision becomes non-negligible as
you approach 2^32 messages under one key. That is exactly why NIST SP 800-38D
caps a single GCM key at roughly 2^32 invocations with random nonces, and why
Go's `cipher.NewGCMWithRandomNonce` documents a hard limit: "A given key MUST
NOT be used to encrypt more than 2^32 messages."

Counter nonces. Start at zero and increment. This gives exact uniqueness with no
birthday bound — but it demands durable, monotonic, non-resettable state. A VM
snapshot restored twice, a process that restarts and rewinds an in-memory
counter, a counter that is not fsynced before use — any of these replays a nonce
and detonates the key. Counters are the right tool inside a single message
(chunk 0, 1, 2, ...) where the state lives only for the duration of one `Encrypt`
call, and a dangerous tool across process restarts.

The 24-byte nonce of XChaCha20-Poly1305 sidesteps the birthday problem
altogether: 192 bits is enough headroom that random generation collides only
after an astronomically large number of messages. That is why XChaCha20-Poly1305
is the right default whenever nonces are generated randomly.

## Associated data is a security boundary, not metadata

The associated data (AAD) parameter is authenticated but not encrypted. It is
transmitted or reconstructed in the clear, and `Open` fails unless the AAD
presented at decrypt time is byte-for-byte identical to the AAD used at encrypt
time. That makes it the tool for binding a ciphertext to its context.

Bind the owning record's identifier as AAD and a valid ciphertext can no longer
be silently moved from one row to another: `Open` with the wrong record ID
fails. Bind the tenant ID and you prevent a ciphertext from one customer being
replayed against another (a confused-deputy / ciphertext-substitution attack).
Bind the key version and field name and you pin the ciphertext to exactly the
slot it was written for. None of this hides the record ID — it is often right
there in the same row — but it makes the binding unforgeable.

The failure mode is subtle: because AAD must be *reconstructed* identically at
decrypt time, any difference in how you serialize it (integer endianness, string
formatting, field order, a trailing space) makes `Open` fail every time. The
temptation then is to "fix" the flaky decrypt by dropping the AAD entirely,
which throws away the whole protection. Decide the AAD encoding once, make it
canonical, and test that it round-trips.

## Ciphertext must be a self-describing, versioned envelope

Real systems outlive one algorithm and one key. The ciphertext you write today
will still be sitting in a database when your team wants to migrate off it. If
the on-disk bytes are a bare AEAD output with no header, that migration is a
flag day: you cannot tell an AES-GCM blob from an XChaCha blob, and you cannot
roll a key version.

The fix is to make the ciphertext self-describing. Reserve the first byte (or
first few bytes) for an algorithm identifier and, where relevant, a key/version
id, then let `Open` read the header and dispatch to the matching decryptor. Now
adding a second cipher is mechanical: new writers emit the new algorithm id, old
readers still decrypt old blobs, and you migrate lazily on next write. This
versioned-envelope seam is also exactly where the next lesson plugs in: envelope
encryption (a key-encryption key wrapping a per-message data-encryption key)
stores the wrapped DEK and its key id in the same header.

## Choosing the cipher

AES-256-GCM is the fastest option *when the CPU has AES-NI hardware*, and it is
the FIPS-approved default. The catch Go documents explicitly: the GHASH
operation in GCM "is not constant-time" except when the underlying block cipher
was created by `aes.NewCipher` on a machine with hardware AES support. On
hardware without AES acceleration, or with a table-based software AES, AES-GCM
can leak key material through cache-timing side channels.

ChaCha20-Poly1305 is constant-time in pure software by construction. On hardware
without AES-NI (some ARM, embedded, or constrained environments) it is both
faster and safer, and its XChaCha variant is the natural choice for random-nonce
use because of the 24-byte nonce. A defensible design doc says: AES-256-GCM
where AES-NI is guaranteed and FIPS is required; XChaCha20-Poly1305 otherwise,
especially when nonces are random.

## Structural facts that shape the API

GCM's numbers drive how you call it. The standard nonce is 12 bytes; the
authentication tag is 16 bytes, which is what `Overhead()` reports; and a single
`Seal` call is capped at roughly 64 GiB of plaintext (2^39 − 256 bits). That
last number is not a suggestion — it is why a file or backup stream larger than
that cannot be sealed in one call and must be chunked.

`Seal` and `Open` use append semantics. `Seal(dst, nonce, plaintext, aad)`
appends the ciphertext to `dst` and returns the grown slice, so the idiom
`blob := aead.Seal(nonce, nonce, plaintext, aad)` passes the nonce slice as
`dst` to prepend the nonce to the ciphertext in one call. The rule to respect is
that `dst`'s spare capacity must not overlap the plaintext, and `dst` must not
overlap the additional data. `Open(dst, nonce, ciphertext, aad)` returns the
plaintext and an error, and a non-nil error is a hard authentication failure:
you discard everything, including any bytes that may have been written into
`dst`. Never use the leftover `dst` on error.

## Truncation and ordering are application problems

A stream encrypted as a sequence of independently-sealed chunks is individually
authentic — each chunk's tag verifies — but the *sequence* is not. An attacker
who cannot forge a single chunk can still drop the last few chunks (truncation)
or swap two chunks (reordering), and every remaining chunk still verifies on its
own. This is the classic GCM truncation attack against naive chunking.

The defense is the STREAM construction: bind the chunk index and an explicit
last-chunk flag into each chunk's associated data. Now a dropped final chunk is
detected because the new final chunk authenticates with `last=false` but is
decrypted expecting `last=true`; a reordered chunk is detected because its index
no longer matches its position; a duplicated chunk lands at the wrong index and
fails. Per-chunk AEAD checks, plus index-and-flag AAD, turn whole-stream
integrity into something the cipher can enforce.

## Modern Go idioms for this space

`crypto/cipher.NewGCMWithRandomNonce` (Go 1.24) removes the manual nonce dance:
`NonceSize()` is zero, `Seal` generates a fresh random 96-bit nonce and prepends
it, `Open` extracts it, and the 2^32-message limit is enforced by construction.
`crypto/rand.Read` never returns a short read — it fills the buffer entirely or
returns an error — so `io.ReadFull` wrapping is unnecessary, and `crypto/rand.Text`
(Go 1.24) gives a base32 secret string when you need one. Keys arrive as `[]byte`
from a KMS or a DEK, never hard-coded. When framing chunks, use `for i := range n`
with no loop-variable aliasing. These are the small choices a reviewer expects to
see in current Go crypto code.

## Common Mistakes

### Reusing a nonce

Wrong: a constant or zero nonce "because it is simpler", or a counter that lives
in memory and resets to zero on process restart. This is the single most
catastrophic and most common AEAD failure. For GCM and Poly1305 it breaks
confidentiality *and* authentication for the entire key, not just one message.

Fix: draw the nonce from `crypto/rand` for every message, or use
`NewGCMWithRandomNonce` / XChaCha20-Poly1305 so the library owns it. If you must
use a counter, persist it durably and fsync before the nonce is used.

### Exceeding the random-nonce message limit

Wrong: encrypting far more than 2^32 messages under one AES-GCM key with random
96-bit nonces, ignoring the birthday bound.

Fix: switch to XChaCha20-Poly1305 (192-bit nonce), rotate to a fresh key well
before the limit, or use `NewGCMWithRandomNonce`, which enforces the 2^32 cap.

### Ignoring the error from Open

Wrong: reading the returned slice from `Open` without checking the error, or
using partial `dst` bytes when `err != nil`.

Fix: a non-nil error from `Open` means the data is unauthenticated. Discard it
entirely and treat it as an attack, not a parse failure.

### Not binding associated data

Wrong: sealing a PII field with no AAD, so a valid ciphertext can be copied from
one row (or tenant, or user) to another and still decrypt.

Fix: bind a stable record/tenant/key-version identifier as AAD so a
substituted ciphertext fails `Open`.

### Reconstructing AAD differently at decrypt time

Wrong: encoding the record ID one way on encrypt and another on decrypt
(different integer endianness, string format, or field order), so `Open` always
fails — then "fixing" it by removing the AAD.

Fix: define one canonical AAD encoding, share it between `Seal` and `Open`, and
test that it round-trips.

### Treating the nonce as secret, or losing it

Wrong: encrypting or "protecting" the nonce as though it were key material, or
forgetting to store it at all.

Fix: the nonce is not secret but it MUST be recoverable. Prepend it to the
ciphertext (or let `NewGCMWithRandomNonce` do it) so decrypt always has it.

### Rolling a homemade nonce

Wrong: building a nonce from `time.Now()` or `math/rand`. Low-resolution clocks
repeat under load, and `math/rand` is fully predictable.

Fix: nonces come from `crypto/rand` or a durable counter — never from a clock or
a non-cryptographic PRNG.

### Sealing large data in one call, or chunking without binding order

Wrong: one `Seal` on a multi-gigabyte upload (hitting the ~64 GiB GCM ceiling),
or chunking without an index and last-chunk flag, leaving the stream open to
truncation and reordering.

Fix: chunk large payloads and bind each chunk's index and a last-chunk flag as
AAD (the STREAM construction).

### One hard-coded algorithm with no header

Wrong: writing a bare ciphertext with no version or algorithm byte, so any later
cipher migration or key rotation becomes an un-decryptable format change.

Fix: prefix the ciphertext with an algorithm/version identifier and dispatch on
it in `Open`.

### Assuming AES-GCM is always constant-time

Wrong: choosing AES-GCM everywhere and assuming it is side-channel free.

Fix: on hardware without AES-NI, AES-GCM leaks via cache timing and GHASH is not
constant-time; prefer ChaCha20-Poly1305 as the software default.

Next: [01-aes-gcm-field-encryption.md](01-aes-gcm-field-encryption.md)
