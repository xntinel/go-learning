# Envelope Encryption: KEK and DEK — Concepts

Every system that has to encrypt "millions of rows" or "every blob in the bucket"
converges on the same design, and it is not "hold one master key and encrypt
everything with it." AWS KMS, GCP KMS with customer-managed keys, HashiCorp
Vault's transit engine, and the user's own Bastion secrets manager all use
*envelope encryption*: a cheap, per-object Data Encryption Key (DEK) does the
bulk work, and a scarce, centrally guarded Key Encryption Key (KEK) does nothing
but encrypt DEKs. This file is the conceptual foundation. Read it once and you
have everything you need to reason through the three independent exercises that
follow, each of which builds a real envelope codec you can gate on its own.

## Concepts

### The two-key hierarchy and why it exists

There is exactly one job the KEK does: it encrypts (wraps) the 32 bytes of a DEK.
It never touches your plaintext. The DEK, freshly generated per object, does the
actual AEAD encryption of the payload — a row, a file, a secret blob — and is
then thrown away in plaintext form; only its *wrapped* form is stored, sitting
right next to the ciphertext it protects. To decrypt, you unwrap the DEK with the
KEK, use it to decrypt the data, and discard it again.

The reason this shape is forced on you at scale is operational, not
mathematical. In a real deployment the KEK lives inside an HSM or a KMS and never
leaves it; you call a remote `Wrap`/`Unwrap` API and the key material stays on the
far side of a hardware boundary. If the KEK encrypted bulk data directly, every
byte of every object would have to travel to the KMS to be encrypted or
decrypted — a throughput and cost disaster, and a needless widening of the
KEK's exposure. Envelope encryption keeps the high-value key off the data path:
the KMS only ever sees 32-byte DEKs, and the bulk AEAD happens locally at line
rate. One KEK can protect an unbounded number of DEKs because each Wrap call is
tiny and independent.

### Blast radius: what a leak actually costs

The hierarchy is a blast-radius control. If a single DEK leaks, the attacker can
decrypt exactly one object — the one that DEK belongs to — and nothing else,
because every other object has its own independent DEK. If the KEK leaks, the
attacker can unwrap any DEK they can also get their hands on, but the KEK by
itself decrypts nothing: it only reveals DEKs, which then reveal data. Crucially,
recovering from a KEK compromise is possible without touching a single byte of
ciphertext: you generate a new KEK and *rewrap* every DEK under it. Contrast this
with the naive "one master key encrypts everything" design, where a single leak
exposes the entire dataset at once and rotation means decrypting and
re-encrypting everything.

### Rotation economics: O(objects), not O(bytes)

This is the payoff a senior engineer must internalize. Rotating the KEK does not
re-encrypt data. It unwraps each fixed-size DEK with the old KEK and rewraps it
under the new KEK. The work is proportional to the *number of objects*, each unit
of work is a 32-byte AEAD operation, and the plaintext is never exposed during
rotation (you only ever handle the DEK, and only in memory). Re-encrypting the
bulk ciphertext — O(total bytes) — is the expensive mistake that envelope
encryption exists to avoid. For a store with billions of objects and petabytes of
data, rewrapping billions of 32-byte DEKs is a background job you can run
incrementally; decrypting and re-encrypting petabytes is not something you do at
all.

### Every envelope carries a KEK identifier

Rotation is not instantaneous; it is a *period* during which old and new KEKs
coexist. Objects written yesterday are wrapped under the old KEK; objects written
after the switch are wrapped under the new one; the background rewrapper is
somewhere in the middle of the backlog. For any of this to work, every envelope
must record *which* KEK wrapped its DEK — a `KEKID`. On `Open`, you look the KEK
up by that id in a keyring that holds all currently-valid KEK versions, so old
data stays decryptable throughout. A KEK may only be retired from the keyring
once every envelope that referenced it has been rewrapped; drop it too early and
you have created undecryptable ciphertext, which is data loss.

### Why AES-GCM to wrap, not RFC 3394 AES-KW

There is a dedicated standard for key wrapping, RFC 3394 (AES Key Wrap), but the
Go standard library does not implement it. The idiomatic and safe alternative is
to treat the DEK as ordinary plaintext and AEAD-encrypt it under the KEK with
AES-256-GCM — exactly the same primitive used for the bulk data. Both layers are
then authenticated encryption: tampering with either the wrapped DEK or the
ciphertext produces an authentication failure on `Open`, not silent corruption.
Rolling your own AES-KW from the RFC when a well-tested AEAD is right there would
be a step backwards.

### Nonce discipline is the number-one GCM footgun

AES-GCM is catastrophic under nonce reuse: encrypt two different messages with
the same (key, nonce) pair and you leak the XOR of the plaintexts and, worse, the
GCM authentication subkey, which lets an attacker forge ciphertexts. Manual nonce
counters, a hardcoded zero nonce, or reusing one DEK across millions of messages
with random 96-bit nonces (past the birthday bound) are all ways to trip this.

Go 1.24 added `cipher.NewGCMWithRandomNonce`, which removes the sharpest edge: its
`Seal` generates a fresh random 96-bit nonce and prepends it to the ciphertext,
and its `Open` reads that prefix back out. You never handle a nonce yourself —
you pass `nil` as the nonce argument to both. The documented safety limit is that
a single key must not encrypt more than 2^32 messages, to keep the chance of a
random-nonce collision negligible. Because envelope encryption mints a *fresh*
DEK per object, each DEK encrypts essentially one message, so this bound is
trivially satisfied. That is a second, quieter reason per-object DEKs are the
right design: they make random nonces unconditionally safe. The AEAD's
`Overhead()` is 28 bytes — a 12-byte nonce plus a 16-byte tag — so a wrapped
32-byte DEK is always 60 bytes and a ciphertext is always `len(plaintext) + 28`.

### Associated data binds context without encrypting it

AEAD takes a fourth argument, the *associated data* (AAD): bytes that are
authenticated but not encrypted. On `Seal` the AAD is folded into the tag; on
`Open` you must supply the identical AAD or authentication fails. This is the tool
that defeats *envelope-swap* and *confused-deputy* attacks. Consider two objects
A and B under the same KEK. Without binding, an attacker who can write to object B
could copy A's valid wrapped-DEK-plus-ciphertext onto B; both layers verify
(same KEK, intact ciphertext) and B now silently returns A's plaintext. Bind the
data ciphertext to the object's identity — `{tenant, object-id}` — via AAD, and
presenting A's envelope under B's context makes `Open` recompute a different AAD
and fail. The context is not stored in the envelope; the caller reconstructs it
from where the object lives, so an attacker cannot simply rewrite it.

The AAD must be encoded *canonically*, meaning the mapping from context to bytes
is unambiguous. Naive string concatenation is not: `tenant="a", object="bc"` and
`tenant="ab", object="c"` both concatenate to `"abc"`, so an attacker could move
an envelope between two contexts that happen to collide. Length-prefixing each
field (write a 4-byte big-endian length, then the field) makes every distinct
context map to a distinct byte string, closing the gap.

### DEKs come from crypto/rand, not from derivation

A DEK must be full-entropy random. Generate it with `crypto/rand.Read` into a
32-byte buffer and check the error. Never derive a DEK from a password, a counter,
or any low-entropy material. When you genuinely need to derive multiple subkeys
from one high-entropy secret — say a root secret split into an encryption key and
a MAC key — the tool is `crypto/hkdf` (also new in Go 1.24), whose `info`
parameter provides domain separation so the same secret yields independent,
purpose-bound subkeys. That is derivation from high entropy; it is not a
substitute for `crypto/rand` when minting a fresh DEK.

### Failures are authentication errors, never partial output

Any tampering — with the wrapped DEK, the ciphertext, the embedded nonce, or the
AAD — makes GCM's `Open` return a non-nil error and a nil plaintext. Treat that
error as fatal to the operation: never return partially decrypted bytes, never
retry as if it were transient, and never log the DEK or KEK on the way out. In Go,
returning `nil, err` and letting the caller branch on a wrapped sentinel via
`errors.Is` is the whole contract.

### Key material hygiene in Go

Keep KEK and DEK bytes on a short leash. Minimize copies, keep them out of
long-lived structs, and never write them to logs or error strings. Be honest
about Go's limits here: a `[]byte` cannot be reliably zeroed, because the garbage
collector may have already copied it during a heap move and `runtime` gives no
guarantee about when the backing array is reclaimed. The real mitigation is
architectural, not a `for i := range b { b[i] = 0 }` loop: keep the KEK inside an
HSM or KMS so your process never holds it at all, and hold each DEK only for the
duration of a single Seal or Open.

## Common Mistakes

Reusing a (key, nonce) pair. Manual nonce counters, a zero nonce, or one DEK
encrypting many messages with random nonces past the 2^32 birthday bound all
break GCM. Prefer fresh per-object DEKs and `NewGCMWithRandomNonce`, which manages
the nonce for you.

Encrypting bulk data directly with the KEK. This defeats the entire pattern: it
puts the high-value key on the data path and turns rotation into an O(bytes)
re-encryption instead of an O(objects) rewrap.

Omitting the KEK id from the envelope. After the first rotation you can no longer
tell which KEK unwraps an old DEK, and the data becomes undecryptable.

Re-encrypting the bulk ciphertext during KEK rotation. Rotation must only unwrap
and rewrap the DEK; the data ciphertext bytes are left untouched.

Forgetting AAD context binding. A valid wrapped-DEK-plus-ciphertext can then be
replayed against a different object or tenant (envelope-swap / confused deputy).

Non-canonical AAD construction. Naive concatenation lets distinct contexts
collide (`"a"+"bc"` == `"ab"+"c"`); length-prefix or otherwise structure the AAD.

Treating an `Open` error as recoverable, or returning partial plaintext. Any
authentication failure must abort the operation and yield no bytes.

Logging or persisting the unwrapped DEK or the KEK, or parking them in
long-lived structs where they outlive the single operation that needs them.

Ignoring the `crypto/rand` error when generating a key. Always confirm key
generation succeeded before using the bytes.

Passing a 16-byte KEK when the policy is AES-256. `aes.NewCipher` silently
accepts 16, 24, and 32-byte keys, so AES-256 is a policy *you* must enforce with
an explicit length check.

Next: [01-dek-wrap-roundtrip.md](01-dek-wrap-roundtrip.md)
