# FIPS 140-3 Mode and the Go Cryptographic Module — Concepts

For most Go code, cryptography is a library call: you reach for `crypto/tls`,
`crypto/ecdsa`, `crypto/rand` and move on. FIPS 140-3 turns that into a
procurement and compliance question. FedRAMP, PCI-DSS, HIPAA, and a large share
of enterprise and government contracts contractually require that *all*
cryptography in the service runs inside a CMVP-validated cryptographic module —
a specific, certified build of specific code, not just "an approved algorithm".
For years the only way a Go shop could claim this was to abandon the pure-Go
toolchain and ship the BoringCrypto or golang-fips fork, dragging in cgo and a
system OpenSSL, which broke cross-compilation, static binaries, and reproducible
builds. Since Go 1.24 the native Go Cryptographic Module makes FIPS a
build-and-runtime flag on the stock toolchain, with zero code changes to callers
of `crypto/tls`, `crypto/ecdsa`, and friends.

The senior job here is almost never writing crypto. It is three operational
tasks: make a regulated service *fail closed* if it boots outside FIPS mode when
its deployment target requires FIPS; produce a TLS configuration whose negotiated
parameters are provably a subset of the FIPS-approved set, and prove it with a
conformance test rather than a comment; and turn "was this artifact FIPS-built?"
into a CI gate that reads embedded build provenance instead of relying on tribal
knowledge. This file is the conceptual foundation for the three independent
exercises that follow.

## Concepts

### The compliance boundary: a module, not an algorithm

FIPS 140-3 validates a cryptographic *module* — a defined, versioned unit of code
with a published Security Policy — not an algorithm in the abstract. The Go
Cryptographic Module v1.0.0 is a frozen snapshot of `crypto/internal/fips140`
taken from Go 1.24 and carries CMVP certificate #5247. Later module versions
(the one shipping with Go 1.26 adds post-quantum ML-DSA, for example) are
separate validated units with their own certificates and their own place on
NIST's validation lists. This is why "we use AES, so we are FIPS compliant" is
false: using an approved algorithm *outside* a validated module in FIPS mode does
not count. Compliance is the validated module, running in FIPS mode, with a real
certificate number you can cite — and which version you may *legally* claim
depends on external NIST state (the Modules-In-Process and validated lists), not
just on your `go.mod`.

### Two switches: build-time selection and runtime mode

There are two distinct controls, and confusing them is the single most common
mistake.

`GOFIPS140` is a *build* setting (`off` | `latest` | `v1.0.0` | `inprocess` |
`certified`). It selects *which* frozen snapshot of the module is compiled into
the binary — `v1.0.0` picks that certified snapshot, `inprocess` and `certified`
track NIST's in-process and validated lists, `latest` uses the in-tree code
without freezing. As a side effect, building with any non-`off` `GOFIPS140` also
defaults the runtime `fips140` GODEBUG to `on`.

`GODEBUG=fips140` is the *runtime* mode (`off` | `on` | `only`). It decides
whether the module actually operates in FIPS mode *this process*. Setting
`GOFIPS140=v1.0.0` does both at once: it freezes the module and defaults the
process to FIPS mode. But you can also enable FIPS mode at runtime on an ordinary
build with `GODEBUG=fips140=on`, in which case `Version()` reports `"latest"`
because no frozen module was selected at build time. A binary can therefore
report a frozen `Version()` while `Enabled()` is false, or vice versa — the two
switches are independent, and both matter.

### Provenance: three ways to set the GODEBUG, one of them inspectable

The `fips140` GODEBUG can be set three ways, with different provenance. As a
runtime environment variable (`GODEBUG=fips140=on ./service`), which leaves no
trace in the binary. As a `godebug fips140=on` line in `go.mod`. Or as a
`//go:debug fips140=on` directive in `package main`. The last two *bake a default
into the binary*: the toolchain records it as a `DefaultGODEBUG` build setting
that is visible through `go version -m <binary>` and through
`runtime/debug.ReadBuildInfo`. That recorded default is exactly what makes build
attestation possible — a CI job can read it from the artifact without running it.
Crucially, whichever way it is set, the `fips140` mode is read *once at process
start* and cannot be changed afterward; there is no runtime setter. That
immutability is why the only testable seam for posture logic is a pure decision
function, not a live toggle.

### What FIPS mode does at runtime

Under `fips140=on` (the production mode), `crypto/tls` silently narrows itself: it
will not negotiate any protocol version, cipher suite, signature algorithm, or
key-exchange that is not FIPS-approved, *even if your `tls.Config` lists it*. TLS
1.0 and 1.1 are refused; non-approved curves are refused; and `crypto/rand`'s
`Reader` becomes a NIST SP 800-90A DRBG seeded from the platform CSPRNG. This is
the behavior you want in production, but it has a sharp edge: an "it compiles"
binary can still fail at the first handshake, because the config only *lists*
preferences — the module decides what is actually allowed. Conformance must be
tested against a real handshake, never assumed from the config.

`fips140=only` is a *stricter, best-effort test/debug mode*: non-approved calls
return errors or panic by design. The Go team is explicit that it is intended for
testing and assessment, is not required by the Security Policy, and *introduces
crashes on purpose*. It must never ship to production; production wants
`fips140=on`.

### What FIPS mode costs you

Primitives many Go services already use become unavailable or simply never get
negotiated. ChaCha20-Poly1305 and bare X25519 are not FIPS-approved.
TLS 1.0/1.1 and non-approved elliptic curves are refused. `rsa.SignPSS` with
`PSSSaltLengthAuto` is capped at the hash length rather than the maximum. And the
module runs self-integrity machinery automatically: an integrity self-check over
the module image at init, known-answer tests per the Implementation Guidance, and
pairwise consistency tests on every generated keypair. That last one can roughly
*double* the cost of ephemeral key generation — a real latency and throughput
concern for a high-connection-rate TLS server, and something to budget for before
enabling FIPS in front of production traffic.

### crypto/fips140 is introspection, not configuration

The `crypto/fips140` package does not configure anything; it reports and,
narrowly, escapes. `Enabled() bool` (since Go 1.24) reports whether the module is
running in FIPS mode. `Version() string` (Go 1.26) reports the module version
(`"latest"` when not built against a frozen module). `Enforced() bool` (Go 1.26)
reports whether strict `fips140=only` enforcement is currently active.
`WithoutEnforcement(f func())` (Go 1.26) runs `f` with strict enforcement
suspended — inherited by any goroutines `f` spawns — for the rare, legitimately
non-approved operation you must perform while otherwise running strict. None of
these turns FIPS on or off; the mode is fixed at startup.

### Fail-closed is the design principle

A regulated service that boots outside FIPS mode should refuse to start, or
report not-ready, rather than silently serving non-compliant cryptography. A
mis-set GODEBUG or a non-FIPS build is a configuration mistake that is invisible
until an auditor — or an attacker — finds it. So the posture must be *observable*
(a readiness endpoint, a structured log line) and *asserted* at startup against a
declared requirement. That is the whole point of the first exercise.

### Platform and portability reality

FIPS mode is unsupported on OpenBSD, Wasm, AIX, and 32-bit Windows. A
cross-compiled artifact targeting one of those platforms will silently *not* be
in FIPS mode even if you built it with `GOFIPS140` set — another reason the
running process, not the build command, is the source of truth. And each frozen
module version is only supported until its successor earns its own CMVP
certificate, so the version you can claim tracks NIST's lists over time.

## Common Mistakes

### Believing a build flag makes the app "FIPS certified"

Wrong: treating `GOFIPS140=on` (or merely importing `crypto/tls`) as compliance.
Fix: compliance is the *validated module* running in FIPS mode with a real CMVP
certificate number, and the version you may claim depends on NIST's
in-process/validated lists. The build flag is necessary, not sufficient.

### Shipping fips140=only to production

Wrong: enabling `GODEBUG=fips140=only` in a production deployment because "only"
sounds strongest. Fix: `only` is a best-effort test/debug mode that returns
errors and panics by design and is explicitly not for production. Production wants
`fips140=on`.

### Trying to toggle FIPS mode after startup

Wrong: exposing a setter that flips FIPS mode at runtime. Fix: the `fips140`
GODEBUG is read once at process start and cannot change; the only testable seam is
a pure decision function over an observed posture, not a live mutation.

### Assuming favorite primitives still work

Wrong: keeping ChaCha20-Poly1305, raw X25519, TLS 1.1, or a non-approved curve in
the config and shipping because it compiles. Fix: those are unavailable or never
negotiated under FIPS; exercise a real handshake in a conformance test so the
failure surfaces before production.

### Confusing GOFIPS140 with GODEBUG=fips140

Wrong: setting the build flag and expecting the runtime effect, or vice versa.
Fix: `GOFIPS140` selects which frozen module is compiled in (embedded in the
binary); `GODEBUG=fips140` decides whether it runs in FIPS mode this process.
Setting one does not guarantee the other's effect.

### Verifying build status with a wiki page

Wrong: recording "this pipeline builds FIPS" in a comment or a runbook. Fix: read
the embedded `GOFIPS140` and `DefaultGODEBUG` settings from the artifact with
`go version -m` or `runtime/debug.ReadBuildInfo`, and fail the CI gate closed if
they are absent — otherwise a non-FIPS binary sails through.

### Ignoring the pairwise-consistency-test cost

Wrong: enabling FIPS on a high-connection-rate service and being surprised by
handshake-latency regressions. Fix: budget for the pairwise consistency test on
ephemeral keygen, which can roughly double that cost.

### Forgetting unsupported platforms

Wrong: cross-compiling for OpenBSD/Wasm/AIX/32-bit Windows and assuming the
`GOFIPS140` build flag put you in FIPS mode. Fix: FIPS mode does not run there;
verify the running process's posture on the actual target.

Next: [01-fips-startup-posture.md](01-fips-startup-posture.md)
