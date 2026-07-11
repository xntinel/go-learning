# Keyless Artifact Signing with Sigstore and cosign — Concepts

The last question a deploy pipeline asks before it runs a binary, a container, or
an SBOM is: *do I trust where this came from?* Sigstore answers it without asking
anyone to manage a long-lived signing key. This lesson lives at that
artifact-trust boundary. The senior task is not "call `Verify`" — it is building
the in-process verification gate (an admission controller, a release check) that
decides whether an artifact is allowed to run, using `sigstore-go` as a library
rather than shelling out to the `cosign` CLI. Three engineering problems drive the
design: pinning a trust root so verification works offline on air-gapped deploy
hosts; expressing an identity allowlist as policy-as-code so a valid signature by
the *wrong* signer is still rejected; and the short-lived-certificate timestamp
problem, which is the reason keyless verification is more than a signature check.

## Concepts

### The keyless model, end to end

Traditional signing has one hard failure mode: a long-lived private key that can
be stolen, leaked, or must be rotated. Keyless signing removes the key. The flow
is: the signer generates an *ephemeral* keypair (used for one signature and then
discarded); it presents an OIDC identity token — from GitHub Actions, Google,
GitLab, and so on — to Fulcio, Sigstore's certificate authority; Fulcio issues a
short-lived (~10 minute) X.509 code-signing certificate that *binds the identity*
(the OIDC subject, carried as the certificate's Subject Alternative Name) to the
ephemeral public key; the signer signs the artifact; Rekor, an append-only
transparency log, records the entry and returns an inclusion proof; and a Signed
Certificate Timestamp (SCT) proves the certificate itself was logged in a
Certificate Transparency log. There is no persistent private key anywhere in this
picture — nothing to steal, leak, or rotate.

### The short-lived-certificate timestamp problem (the hard why)

Because a Fulcio certificate is valid for only about ten minutes, you cannot
verify an artifact days later by checking the certificate against the current
wall clock — by then it has long expired. This is the single most important idea
in the lesson. Verification instead trusts a *timestamp* proving the signature
existed *while the certificate was still valid*. That timestamp can come from the
SCT (when the certificate was logged), from Rekor's integrated time (when the
entry was recorded), or from an RFC 3161 Timestamp Authority (TSA) that
countersigns the signature. This is why the verifier is configured with
thresholds — `WithSignedCertificateTimestamps`, `WithTransparencyLog`,
`WithObserverTimestamps` — rather than a "check expiry against now" flag.
`WithCurrentTime` exists, but it is only appropriate for a *private* Sigstore
deployment issuing long-lived certificates; using it against public-good
keyless bundles is a mistake, because the certificate is already expired.

### Identity is the policy, not a nicety

Here is the trap that makes keyless verification different from key-based
verification. Because Fulcio will issue a valid certificate to *anyone* who can
present *any* accepted OIDC identity, an attacker with a Google or GitHub account
can obtain a cryptographically valid Fulcio certificate and produce a bundle that
*verifies*. A signature that merely "verifies" therefore proves nothing about
trust. You MUST pin *who* signed: the OIDC issuer plus the Subject Alternative
Name. For CI-produced artifacts that means pinning the exact repository and
workflow ref — for example the SAN
`https://github.com/org/repo/.github/workflows/release.yml@refs/heads/main`
issued by `https://token.actions.githubusercontent.com`. The library exposes
`verify.WithoutIdentitiesUnsafe()` for the rare case where identity is checked
elsewhere; its name is a deliberate warning. A verification gate that omits an
identity policy is not a weaker gate — it is not a gate at all.

### Anchored SAN matching

When a SAN policy is expressed as a regular expression it must be anchored with
`^` and `$`. An unanchored pattern is a silent authorization bug. The pattern
`github.com/org/repo` also matches `github.com/org/repo-evil` (a repository an
attacker can create) and `evil.com/github.com/org/repo` (the substring appears
anywhere in the SAN), so it trusts workflows you never intended to trust. The
anchored form
`^https://github.com/org/repo/\.github/workflows/release\.yml@refs/heads/main$`
matches exactly one identity. This is not a style preference; an unanchored SAN
regex is one of the highest-value supply-chain vulnerabilities in a verification
gate, because everything still "passes" while trusting an attacker.

### The trust root and TUF

Verification needs Fulcio's CA roots, the Rekor and Certificate Transparency log
public keys, and the TSA roots. These live in a *trusted root* that Sigstore
distributes over The Update Framework (TUF), which makes the material
tamper-resistant and updatable. `root.FetchTrustedRoot` pulls it over the network.
That is fine for a one-off CLI invocation and wrong for a deploy gate: calling it
on every verification hits the network, is subject to rate limits, and simply
fails on an air-gapped host. The gate instead *pins* a `trusted_root.json` loaded
from disk with `root.NewTrustedRootFromPath`, and refreshes that file out of band
(a separate scheduled TUF pull), so the hot path is offline and deterministic.

### The Sigstore bundle

A Sigstore bundle (`.sigstore.json`) is a single self-describing protobuf that
carries everything a verifier needs: the signing certificate (or public key), the
signature, the Rekor transparency-log entry, and any timestamps, plus a media type
and version. It is the interchange format that both `cosign` and `sigstore-go`
read and write, which is exactly why library-based verification interoperates with
CLI-based signing: a bundle produced by `cosign sign-blob --bundle` verifies with
`sigstore-go`, and vice versa.

### Plain blob versus DSSE attestation

The content type determines the verification path. Signing raw bytes
(`sign.PlainData`) produces a `hashedrekord` entry and is verified by binding the
*artifact digest* (`verify.WithArtifactDigest`). Signing an in-toto statement
(`sign.DSSEData` with an in-toto payload type) produces a `dsse` entry, and on
success the verification result carries a parsed `Statement` — this is how the
SLSA provenance and SBOM attestations from the previous lesson are signed and
later read by policy. If you sign a plain blob but expect an attestation, the
result's `Statement` is empty; the mismatch is a wiring bug, not a crypto failure.

### cosign versus sigstore-go

`cosign` is the operator-facing CLI: ideal for CI steps and human operators.
`sigstore-go` is the Go library that `cosign` and the Kubernetes policy-controller
are built on. Reach for the CLI for ad-hoc signing and CI ergonomics; embed
`sigstore-go` when you need in-process verification with structured results and no
process exec — an admission controller, a deploy gate, or a custom policy engine
on the request path. Shelling out to `cosign` from a long-running service to
verify on the hot path is the anti-pattern this lesson exists to replace.

### Operational reality and trade-offs

Keyless signing depends on the availability and rate limits of the public-good
Fulcio, Rekor, and TUF infrastructure. It trades privacy for transparency: your
identity and the artifact digest become *public* entries in Rekor. That is a
feature for monitoring — you can watch Rekor for rogue signings of your identity —
and a leak for a private repository. Air-gapped environments need a private
Sigstore stack or must fall back to key-based signing. None of these are reasons
to avoid it; they are the constraints a senior weighs when choosing it.

## Common Mistakes

### Verifying the signature but not the identity

Wrong: constructing a policy with no certificate identity, or reaching for
`verify.WithoutIdentitiesUnsafe()` to make verification "just pass". Because
Fulcio issues valid certificates to any accepted identity, this trusts every
signer on earth.

Fix: always bind an identity with `verify.WithCertificateIdentity`, pinning the
OIDC issuer and the SAN. Treat an identity-less policy as a build error in your
policy layer, not a runtime warning.

### Unanchored SAN regex

Wrong: `^https://github.com/org/repo` — the missing trailing `$` lets it match
`repo-evil` and any longer path, and a pattern with no `^` matches the substring
anywhere.

Fix: anchor both ends,
`^https://github.com/org/repo/\.github/workflows/release\.yml@refs/heads/main$`.
Better still, make the policy layer reject an unanchored SAN regex outright.

### Requiring no timestamp, or using WithCurrentTime on short-lived certs

Wrong: building a verifier with zero transparency-log or observer-timestamp
thresholds, or `verify.WithCurrentTime` against a public-good keyless bundle whose
certificate expired minutes after it was issued.

Fix: for public-good keyless bundles require
`WithSignedCertificateTimestamps(1)`, `WithTransparencyLog(1)`, and
`WithObserverTimestamps(1)`. Reserve `WithCurrentTime` for private deployments
with long-lived certificates.

### Fetching the trust root on every verification

Wrong: calling `root.FetchTrustedRoot()` on the hot path. It hits TUF and the
network, is rate limited, and breaks air-gapped hosts.

Fix: pin `trusted_root.json` and load it with `root.NewTrustedRootFromPath`;
refresh the file out of band.

### Transposing the arguments of NewShortCertificateIdentity

Wrong: `verify.NewShortCertificateIdentity(issuer, issuerRegex, sanValue, sanRegex)`
takes four strings, and swapping any two silently yields a policy that matches
nothing or everything.

Fix: cover the constructed identity with a table test, or build the matchers
explicitly with `verify.NewSANMatcher` and `verify.NewIssuerMatcher` where the
argument roles are unambiguous.

### Mismatched digest algorithm string

Wrong: passing `"sha256"` to `verify.WithArtifactDigest` when the bundle was
signed over a SHA-512 digest (or vice versa). The digest bytes will not match the
declared algorithm and verification fails or, worse, appears to succeed against
the wrong artifact.

Fix: derive the algorithm from how the bundle was produced, not a hard-coded
guess.

### Signing the wrong content type

Wrong: signing a plain blob when downstream policy needs to read a SLSA statement,
or signing a DSSE attestation when the verifier binds an artifact digest. The
content type (`sign.PlainData` versus `sign.DSSEData`) picks the Rekor entry type
and the verification path; a mismatch leaves `result.Statement` empty.

Fix: choose the content type from what downstream policy must read.

Next: [01-verify-bundle-gate.md](01-verify-bundle-gate.md)
