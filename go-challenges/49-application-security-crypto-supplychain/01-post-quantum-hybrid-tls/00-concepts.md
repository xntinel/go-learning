# Post-Quantum Hybrid TLS with crypto/mlkem — Concepts

The most consequential cryptographic migration of the decade is already
underway, and it is largely invisible: the switch of TLS key exchange from
classical elliptic-curve Diffie-Hellman to a post-quantum hybrid. It matters to
a backend platform team not because a quantum computer exists today, but because
the threat does not require one to exist yet. This file is the conceptual
foundation. Read it once and you have the model you need to reason through the
three exercises: reconstructing a hybrid KEM from primitives, negotiating
`X25519MLKEM768` end to end in `crypto/tls`, and enforcing a post-quantum
posture at the edge of an HTTP service.

## Concepts

### Harvest-now, decrypt-later is the whole reason

The driving threat is not a live attack. It is an adversary who records your
encrypted TLS traffic today, stores it cheaply, and decrypts it years from now
once a cryptographically relevant quantum computer (CRQC) can break the
classical key exchange that protected it. Shor's algorithm, on a large enough
machine, solves the discrete-log problem that X25519 rests on, which recovers
the session key and therefore the plaintext of every recorded handshake.

The consequence is that the confidentiality of *today's* traffic is a *future*
liability, and the clock started the moment the ciphertext was recorded. Any
payload with a long secrecy lifetime — database replication streams, backups,
API tokens with multi-year validity, PII, cross-region secret material — is
already exposed even though the break is a decade out. You cannot retroactively
protect data an attacker has already captured; the only defense is to make the
key exchange quantum-resistant *before* the recording happens. That is why this
is shipping now rather than when a CRQC is announced.

Note the asymmetry with authentication. A forged signature or a spoofed
certificate must be produced *live*, during a handshake that is happening now, so
post-quantum *signatures* are a real but less urgent problem — an attacker cannot
retroactively forge a signature against a session that already completed.
Post-quantum *key exchange* protects confidentiality against a future adversary
and so goes first. This lesson is about key exchange.

### A KEM is not Diffie-Hellman

ML-KEM (Module-Lattice Key Encapsulation Mechanism, standardized as FIPS 203,
formerly the CRYSTALS-Kyber submission) is a Key Encapsulation Mechanism, and
its shape is different from a Diffie-Hellman key agreement. In DH, both parties
contribute a public value and each independently computes the same shared secret
from its own private key and the peer's public value; it is symmetric.

A KEM is asymmetric in roles:

- One party generates a keypair: a *decapsulation key* (private) and an
  *encapsulation key* (public). It publishes the encapsulation key.
- The peer runs `Encapsulate()` on that public key. This produces two outputs at
  once: a fresh *shared secret* and a *ciphertext*. Crucially, `Encapsulate`
  takes no randomness argument in the Go API — it draws its own internal
  randomness, so it is non-deterministic by design and you cannot make it
  reproducible by seeding it. The peer keeps the shared secret and sends the
  ciphertext.
- The original party runs `Decapsulate(ciphertext)` with its decapsulation key
  and recovers the *same* shared secret.

ML-KEM-768 is the NIST security category 3 parameter set (roughly comparable to
AES-192). Its wire sizes are fixed and worth memorizing because they drive the
operational cost discussed below: the encapsulation key is 1184 bytes, the
ciphertext is 1088 bytes, and the shared secret is 32 bytes. A decapsulation key
in Go is stored and restored as a 64-byte seed.

### Why hybrid, and never pure PQ

Go's default post-quantum group is `X25519MLKEM768`, a *hybrid* that runs a
classical X25519 ECDH *and* an ML-KEM-768 encapsulation in the same handshake,
then combines the two shared secrets into one session key. It is not pure
ML-KEM, and that is a deliberate, defensible design decision.

Hybrid buys defense-in-depth against two independent failure modes:

1. A future quantum break of X25519. ML-KEM covers this.
2. An as-yet-undiscovered classical flaw or an implementation bug in the
   relatively young ML-KEM code and its lattice analysis. X25519 — decades old,
   heavily reviewed — covers this.

The combined session key is secure as long as *either* primitive holds. You are
betting that at least one of "elliptic curves are still safe against classical
attack" and "the new lattice scheme has no catastrophic bug" is true, which is a
far safer bet than staking everything on the newer, less-battle-tested primitive
alone. Deploying pure ML-KEM would throw away the hedge against ML-KEM's own
immaturity — precisely the risk that is hardest to quantify for a five-year-old
scheme.

### Combine by concatenate-then-KDF, never XOR

The correct combiner concatenates the X25519 shared secret with the ML-KEM
shared secret and feeds the concatenation through a key derivation function
(HKDF here). The security-if-either-holds property comes from the KDF over the
concatenation: an attacker must break *both* inputs to predict the output.

XOR does not give you this. If you XOR the two secrets and one of them is later
recovered, the attacker can strip it off and is left attacking only the other —
worse, if one secret is ever predictable, XOR leaks the other directly. "Pick
one of the two" is obviously wrong for the same reason. The rule is
concatenate-then-KDF; the TLS spec for `X25519MLKEM768` does exactly this
internally, and Exercise 1 reconstructs it so the reasoning is concrete.

### Implicit rejection: a security feature that reads as a footgun

ML-KEM's `Decapsulate` does *not* return an error when handed a ciphertext that
was not produced under the matching encapsulation key. By specification it
returns a *different, pseudo-random* shared secret. This is called implicit
rejection, and it is intentional: a decapsulation routine that branched (error
vs. success) on whether the ciphertext was valid would leak, through timing or
control flow, information an attacker could exploit in a chosen-ciphertext
attack. Constant-behavior decapsulation closes that channel.

The operational consequence is sharp: you cannot detect a tampered or
wrong-key ciphertext from the decapsulation step. Both sides simply end up with
*different* secrets, and the mismatch only surfaces downstream when the derived
keys fail to agree — in TLS, the Finished-message MAC will not verify; in a
hand-rolled protocol, the AEAD tag will not check out. Integrity therefore lives
in the KDF/MAC/AEAD that consumes the KEM output, never in a
`Decapsulate`-returned error. Code written to "check `err` from Decapsulate to
detect tampering" is broken.

### Observe the negotiated group; do not assume it

The only correct way to know which key-exchange group a TLS connection actually
negotiated is to read it from the completed handshake:
`ConnectionState.CurveID`, added in Go 1.25. Two common substitutes are wrong:

- Inspecting the cipher suite (AES-GCM, ChaCha20-Poly1305) tells you nothing
  about the key exchange. The cipher suite is the symmetric/AEAD algorithm and
  is orthogonal to the group; a connection can be post-quantum in key exchange
  and use any TLS 1.3 cipher suite.
- Assuming the group from your own `CurvePreferences` is wrong because the peer
  may not support it, in which case a different group is negotiated (or the
  handshake fails). You must read what was *negotiated*, not what you *offered*.

### CurvePreferences is a set, not an ordered list

Intuition from cipher-suite configuration misleads here. `tls.Config`'s
`CurvePreferences` field is treated as a *set* of acceptable groups; Go applies
its own internal preference order and *ignores the order you write*. You express
policy by *membership* — which groups are in the slice — not by position. You do
not "make PQ win" by putting it first. You enforce a required group by checking
`ConnectionState.CurveID` after the handshake and rejecting anything outside your
approved set. Exercise 3 builds exactly that enforcement.

### PQ exists only on TLS 1.3

Post-quantum key exchange is a TLS 1.3 construction. There is no
`X25519MLKEM768` on a TLS 1.2 handshake — none at all. This means `MinVersion`
is part of your post-quantum posture, not an unrelated knob: a configuration
that permits TLS 1.2 will, on any connection that negotiates 1.2, silently
provide zero post-quantum protection while everything still "works". Pin
`MinVersion: tls.VersionTLS13` anywhere PQ matters.

### The operational cost is real

The ML-KEM-768 encapsulation key (1184 bytes) plus the classical share pushes
the TLS ClientHello past the roughly 1500-byte Ethernet MTU and beyond a single
initial TCP segment. This is not a theoretical concern: it exposes broken
middleboxes, load balancers, and non-RFC-compliant network stacks that mishandle
a fragmented or unusually large ClientHello, causing handshakes to stall or fail
on some paths. There is also a modest CPU and handshake-size overhead versus
pure X25519. A responsible rollout treats this as a change that must be
observable and reversible: you watch the negotiated-group metric climb across the
fleet, you alert on regressions, and you keep a documented kill switch —
`GODEBUG=tlsmlkem=0` disables the ML-KEM hybrid group so a bad interaction can be
rolled back without a redeploy.

### Go version timeline

- Go 1.24 landed `crypto/mlkem`, `crypto/hkdf`, and the `X25519MLKEM768`
  `CurveID`, and made the hybrid group a client *and* server default.
- Go 1.25 added `ConnectionState.CurveID`, the only correct way to observe the
  negotiated group.
- Go 1.26 adds `SecP256r1MLKEM768` and `SecP384r1MLKEM1024` to the defaults for
  environments (FIPS, NIST-P-curve fleets) that need a P-curve hybrid.

Every exercise in this lesson requires a Go 1.25+ toolchain to build; the gate
uses Go 1.26.

## Common Mistakes

### Treating ML-KEM as a drop-in for a Diffie-Hellman shared secret

ML-KEM is encapsulate/decapsulate with asymmetric roles, not a symmetric key
agreement. There is no "both sides compute the same thing from their own
private key". One side publishes an encapsulation key; the peer encapsulates to
get `(sharedKey, ciphertext)`; the owner decapsulates the ciphertext. Modeling
it as DH leads to code that never type-checks against the real API.

### Believing pure ML-KEM is strictly better than hybrid

It is not. The point of `X25519MLKEM768` is defense-in-depth against a bug in
ML-KEM itself, which is the young half of the pair. Pure PQ discards that hedge.
Unless a specific compliance regime forces it, hybrid is the safer default —
which is exactly why Go made it the default.

### Combining the two secrets with XOR, or using only one

The security-if-either-holds property comes from concatenating both shared
secrets and running the concatenation through a KDF. XOR loses that property and
can leak one secret if the other is ever recovered or predictable. Never XOR,
never pick one.

### Branching on a Decapsulate error to detect tampering

`Decapsulate` returns a *different* pseudo-random key on a wrong or tampered
ciphertext (implicit rejection), not an error. It only returns an error for a
structurally malformed ciphertext of the wrong length. Integrity against
tampering must come from the downstream KDF/AEAD/Finished MAC, never from the
decapsulation result.

### Detecting PQ from the cipher suite or a string

The cipher suite is orthogonal to the key-exchange group. Only
`ConnectionState.CurveID` tells you the group that was negotiated. Parsing a
connection description string or checking for AES-GCM proves nothing about
post-quantum key exchange.

### Expecting CurvePreferences to honor list order

The field is a set; Go ignores your ordering and applies its own preference. You
enforce a required group by checking `CurveID` after the handshake, not by
reordering the slice and hoping the first entry wins.

### Leaving MinVersion permissive and expecting PQ anyway

On a TLS 1.2 handshake there is no post-quantum key exchange at all, silently.
`MinVersion: tls.VersionTLS13` is part of the posture.

### Using the old draft name instead of tls.X25519MLKEM768

The experimental `X25519Kyber768Draft00` name and any hand-rolled constant are
gone or wrong. Use `tls.X25519MLKEM768` (4588). Draft Kyber and standardized
ML-KEM are not interchangeable identifiers.

### Reading r.TLS.CurveID without a nil check

A plain-HTTP request has `r.TLS == nil`. Enforcement middleware that dereferences
`r.TLS.CurveID` without first checking for nil panics on plain HTTP instead of
failing closed. Fail closed: no TLS state means reject.

### Conflating the seed and the key bytes on restore

A decapsulation key is regenerated from its 64-byte seed via
`NewDecapsulationKey768(seed)`. In `crypto/mlkem` the value returned by the
key's `Bytes()` method *is* that 64-byte seed. Persist the seed and restore
from it; storing some other blob and expecting it to reconstruct the key breaks
rotation and restore.

Next: [01-hybrid-kem-handshake.md](01-hybrid-kem-handshake.md)
