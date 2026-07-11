# Supply-Chain Security: SLSA Provenance and SBOMs — Concepts

A senior backend engineer owns what actually ships: the binary inside the
container, not the diff in the pull request. Between the reviewed source and the
running artifact sit a compiler, a dependency graph, a build machine, and a
pipeline, and every one of those is an attack surface. Supply-chain security is
the discipline of making claims about that gap *checkable*. This lesson treats
the metadata that describes a build — what went into it and who produced it — as
a first-class deliverable of the build, not a compliance checkbox bolted on
afterward. The through-line for every exercise is a single question: given only
the artifact, can you prove what went into it and who built it?

Go is unusually well positioned to answer that question, because the toolchain
already stamps every binary with a machine-readable build record. The work is not
generating provenance from nothing; it is extracting the record Go gives you for
free, rendering it in the formats consumers expect, and — the part that carries
all the value — building the verifier that rejects an artifact when the record
fails a policy.

## The consumer-side question

The reflex when someone says "supply-chain security" is to reach for tools that
*produce* things: an SBOM generator, an attestation signer, a scanner in CI. All
of that is necessary and none of it is sufficient, because production is not where
the security lives. An SBOM that no scanner ingests, a provenance file that no
deploy gate checks, a signature that nobody verifies — these are theater. They
create the paperwork of security without its substance, and they are worse than
nothing because they invite the belief that the problem is handled.

The value is entirely on the consumer side: the gate that looks at an incoming
artifact and its metadata and says "reject". A useful mental discipline is to
design every producer backward from the check it enables. You do not generate an
SBOM because a policy says to; you generate it so that when CVE-2025-whatever
lands in a transitive dependency, you can answer "are we affected?" in seconds by
querying inventories instead of grepping build logs across fifty services. You do
not produce provenance to satisfy an auditor; you produce it so a deploy gate can
refuse a container whose builder identity is not the one reusable workflow you
trust. If you cannot name the check a piece of metadata feeds, you are building
theater.

## The build record Go gives you for free

Every Go binary carries an embedded build record. With module-aware builds (the
default for a decade) the linker writes in the main module path and version, the
full transitive module graph with each dependency's `go.sum` checksum, the Go
toolchain version, and — when `-buildvcs` is on, which is the default when
building inside a repository — the VCS system, the commit revision, the commit
time, and a `vcs.modified` flag that records whether the working tree had
uncommitted changes. You read it two ways. Inside a running program,
`runtime/debug.ReadBuildInfo` returns a `*debug.BuildInfo`. Against a binary on
disk, `go version -m ./binary` prints the same record as text, and
`debug.ParseBuildInfo` turns that text back into a `*debug.BuildInfo`. This is the
cheapest provenance in any language ecosystem: no extra tool, no separate build
step, no plugin. The skill is not producing it — it is reading it and knowing what
to trust.

One honest wrinkle you must internalize before writing a line of code:
`debug.ParseBuildInfo` does not parse the leading `go\tgo1.26.0` line, so the
`GoVersion` field comes back empty when you parse `go version -m` text.
`ReadBuildInfo` papers over this by injecting `runtime.Version()` after parsing.
If you build a provenance extractor on `ParseBuildInfo` — which you must, because
that is the only path that works deterministically in a unit test — you have to
recover the toolchain version yourself by scanning the `go\t` line. A generator
that reports an empty Go version because it trusted `ParseBuildInfo` to fill it in
is a real, common bug.

## Dirty builds are not reproducible, ever

The single most valuable flag in the whole record is `vcs.modified`. When it is
`true`, the binary was built from a working tree with uncommitted changes. It may
carry a commit revision, but that revision is a lie: the bytes that went into the
compiler do not correspond to that commit's tree, and there is no commit anywhere
that reproduces this binary. A "works on my laptop" build is precisely a dirty
build that escaped. Treat `vcs.modified=true` as a hard fail for anything headed
to production, in the same breath as a missing `vcs.revision` (which means the
build ran with `-buildvcs=false`, or outside a repository, or in CI that stripped
`.git`). A revision without a clean tree is not provenance; it is decoration.

## The `go.sum` hash is a dirhash, not an artifact SHA-256

`go.sum` records a line like `h1:GokP8Fi...=` for each module. It is tempting to
drop that value into an SBOM as a "SHA-256" and move on. That is wrong in a way
that matters. The `h1:` scheme is a *module dirhash*: Go builds a deterministic
listing of the module's files and their individual hashes, then takes the SHA-256
of that listing and base64-encodes it. So the underlying bytes really are a
SHA-256 — but of a synthetic file manifest, not of any downloadable archive. A
consumer who takes your "SHA-256" and runs `sha256sum module.tar.gz` will get a
different value and conclude your SBOM is corrupt. The honest rendering, and the
one the real `cyclonedx-gomod` tool uses, is to strip `h1:`, base64-decode to the
32 raw bytes, hex-encode those for the standard hash field, and additionally
preserve the original `h1:` token verbatim in a namespaced property so a verifier
can recompute it the right way (via `golang.org/x/mod/sumdb/dirhash`). This
distinction — a real hash, but of the manifest, verified with the dirhash
algorithm and not `sha256sum` — is exactly what separates a functioning SBOM from
a decorative one.

## SBOM and provenance answer different questions

An SBOM (Software Bill of Materials) is an inventory: what components are inside
this artifact. Provenance is a history: who built it, from what source, with what
parameters. They are complementary and you need both. An SBOM lets a scanner join
your components against CVE feeds and tell you that you ship a vulnerable version
of some library; it says nothing about whether the build was tampered with.
Provenance proves the build ran on a trusted platform from a known commit; it does
not hand a scanner the component list it needs to find CVEs. The two even overlap
in a useful way: provenance's `resolvedDependencies` records the dependencies that
were *actually* the build's inputs, which is the integrity claim behind the SBOM's
mere list. Ship one without the other and you have half a story.

There are two dominant SBOM standards. CycloneDX (an OWASP project) is
security-focused, with a rich model for components, their dependency relationships,
and vulnerabilities; SPDX (a Linux Foundation / ISO standard) leans toward
licensing and provenance. Both are valid — pick per what your consumers ingest.
What makes either interoperable is the Package URL (`purl`): a component identity
like `pkg:golang/github.com/google/uuid@v1.6.0` that scanners use to join your
SBOM entries to vulnerability databases without guessing.

## SLSA levels rate the build, not the code

SLSA (Supply-chain Levels for Software Artifacts) is a framework whose build track
defines a ladder of trust in the *provenance*, not in the source. This is the most
misunderstood point in the whole area, so hold it precisely. SLSA Build L1 means
provenance exists and is complete — it may be unsigned, it may be self-attested by
the builder itself. L2 means the provenance is signed by a hosted build platform,
so a consumer can authenticate who produced it and forging it requires an actual
compromise rather than a misconfiguration. L3 means the build runs isolated, with
the signing keys unreachable by any user-controlled build step, so even the
build's own scripts cannot forge provenance for a different artifact. Every rung
constrains the *builder*, not the developer and not the code's quality. A
beautifully reviewed codebase built by a laptop is L0; a mediocre codebase built
by a hardened, isolated platform can be L3. Levels are a statement about how much
you can trust the claim, and about nothing else.

## in-toto is the envelope; SLSA provenance is one payload

The in-toto attestation framework provides the envelope that makes all of this
uniform. A `Statement` binds a *subject* (an artifact plus its digest) to a
`predicateType` and a `predicate` payload. SLSA provenance is one such predicate;
an SBOM attestation, a vulnerability-scan result, and a test report are others.
Keeping the envelope separate from the predicate is what lets a single
verification pipeline handle every kind of claim the same way: parse the
statement, check the subject digest matches the artifact in hand, dispatch on the
predicate type, validate the payload. In the Go bindings a `Statement` is a
protobuf message whose `Predicate` is a `structpb.Struct`; you build the strongly
typed SLSA `Provenance` message, run its `Validate()`, and convert it into the
predicate struct with `protojson`. Because these are protobuf messages, JSON
serialization goes through `protojson`, not `encoding/json` — the field naming
(for example the `_type` key) depends on it.

## Signing is the L1→L2 step, and it composes

This lesson stops at building and validating the predicate structure and running
the digest-and-builder gate. What it deliberately leaves out is the signature: a
DSSE envelope, keyless signing with cosign against Fulcio, and a transparency-log
entry in Rekor are the next lesson. The two compose cleanly: the provenance
statement built here is the *payload*, and cosign is the signature and the log
around it. That composition is exactly why an unsigned-but-complete provenance is
L1 and a signed one is L2 — the structure is the same, the signature is what a
verifier can authenticate.

## Trust anchoring is the whole point of the verifier

The final and most important idea: verifying provenance is meaningless without a
policy that pins what you expect. A verifier that accepts any `builder.id`
provides no security — an attacker's own build platform will happily emit valid,
well-formed, even signed provenance naming itself. The gate must compare the
builder identity against an allowlist you control (a specific reusable workflow
ref, for example `.../build.yml@refs/heads/main`), and it must check that the
subject digest equals the digest of the artifact actually in hand. Provenance you
accept from an unpinned builder proves nothing except that someone, somewhere, ran
a build.

## Common Mistakes

### Assuming ReadBuildInfo behaves the same under `go test`

`runtime/debug.ReadBuildInfo` can return `ok=false`, or a synthetic module path,
when the code runs under the test harness rather than as a normal binary. Drive
your extraction logic from `debug.ParseBuildInfo` on injected fixture text in
unit tests, and reserve `ReadBuildInfo` for the running-binary path. Relatedly,
remember that `ParseBuildInfo` does not populate `GoVersion` from the `go\t` line —
recover it by scanning the text yourself, or your provenance reports an empty
toolchain version.

### Trusting any binary that carries a revision

A commit hash in the record is not enough. If `vcs.modified` is `true`, the tree
was dirty and the revision does not describe the bytes. Check the modified flag,
not just the presence of a revision.

### Building with `-buildvcs=false` or in a `.git`-stripped CI, then expecting provenance

VCS stamping only happens when the build runs inside the repository with the VCS
tool available. A container build that copies in source without `.git`, or a
pipeline step that passes `-buildvcs=false`, produces a binary with no revision.
The fix is a build environment, not a code change.

### Mislabeling the `go.sum` `h1:` value as an artifact SHA-256

It is a module dirhash — SHA-256 of a synthetic file manifest, base64-encoded.
Decode and hex-encode it for the standard hash field, keep the original token in a
property, and never tell a consumer to verify it with `sha256sum` on an archive.

### Hand-writing SBOM or attestation JSON by string concatenation

Emitting these formats with `fmt.Sprintf` produces output that fails schema
validation in subtle ways: wrong `bomFormat`, missing `specVersion`, malformed
purls, protobuf fields under the wrong JSON names. Use the typed libraries
(`cyclonedx-go`, the in-toto bindings) and `protojson` so the structure is correct
by construction.

### Generating metadata but never verifying it — or verifying without pinning

Provenance you never check, or check without pinning `builder.id` and the source
repository, is theater. The security is the gate, and the gate must compare
against an allowlist you own.

### Shipping only one of SBOM or provenance

They answer different questions. An SBOM alone does not prove the build was not
tampered with; provenance alone does not give a scanner the component inventory.
Ship both.

### Treating SLSA levels as a property of the code or the developer

Levels describe the trustworthiness of the build platform and the provenance, not
code quality, test coverage, or review rigor. Higher levels constrain the builder.

### Passing non-JSON types into `structpb.NewStruct`

`structpb.NewStruct` accepts only `nil`, `bool`, numbers as `float64`, `string`,
`[]any`, and `map[string]any`. Hand it an `int64`, a `time.Time`, or a custom
struct and it returns an error. Build `externalParameters` from a `map[string]any`
of strings and let `protojson` handle typed protobuf fields like timestamps.

### Forgetting that non-deterministic fields break byte-stable output

Timestamps, random serial numbers, and map iteration order make otherwise
identical builds emit differing SBOMs and attestations, which defeats diffing and
reproducibility checks. Pin or sort those fields when byte-stability matters.

Next: [01-provenance-from-buildinfo.md](01-provenance-from-buildinfo.md)
