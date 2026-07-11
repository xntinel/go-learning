# Module Proxies and GOPROXY — Concepts

In a production Go shop the module-download path is real infrastructure, not a
dev-laptop convenience. It decides whether CI is reproducible, whether a
supply-chain attack can inject tampered dependency bytes, whether an air-gapped
build farm can build at all, and whether a private registry (Artifactory, Athens,
JFrog, GCP Artifact Registry, GitLab) is reachable at the moment a build is
scheduled. A senior backend engineer owns this path: they configure `GOPROXY`
chains with the correct comma-vs-pipe failover so a 500 from a corporate proxy
does not silently fall through to the public internet; they set
`GOPRIVATE`/`GONOPROXY`/`GONOSUMDB` so internal modules never touch
`proxy.golang.org` or `sum.golang.org`; they run a checksum gate in CI so
`go.sum` drift or a poisoned cache fails the build instead of shipping; they
stand up and operate a read-only internal proxy for reproducibility and egress
control; and on Go 1.24+ they wire `GOAUTH` so private fetches authenticate
without leaking tokens. This file is the model behind all ten exercises. Read it
once and each exercise becomes an application of the same fetch-decision,
protocol, and integrity reasoning rather than a memorized environment variable.

## Concepts

### The proxy is the source of truth for versions

Instead of talking to a version-control system directly, the `go` command speaks
the GOPROXY protocol: plain HTTP GET requests to five endpoints under a module's
path. For a module `$module` the endpoints are `$module/@v/list` (the plain-text
list of known versions), `$module/@v/$version.info` (JSON metadata),
`$module/@v/$version.mod` (the `go.mod` for that version), `$module/@v/$version.zip`
(the module's file tree as a zip), and `$module/@latest` (the newest version).
There are no query parameters and no request body, which is the whole point: any
static file server, an object store behind a CDN, or even a `file://` URL can be
a proxy. Reproducibility comes from the fact that a version, once published, maps
to exactly one immutable set of bytes that every builder can fetch identically.

### Protocol path encoding

Module paths and versions travel through case-insensitive filesystems and object
stores, so the protocol case-encodes them to prevent collisions: every uppercase
letter is replaced by an exclamation mark followed by its lowercase form. `Azure`
becomes `!azure`; `github.com/Masterminds/semver` becomes
`github.com/!masterminds/semver`. Without this, `example.com/Foo` and
`example.com/foo` would collide on a case-folding filesystem and one module could
shadow another. Any tool that builds proxy URLs by hand — a mirror sync job, a
caching proxy, an audit script — must encode on the way out and decode on the way
in, or it will request a path the proxy answers with 404.

### The endpoint content types and failover status codes

A conformant proxy sets `Content-Type` per endpoint: `.info` is
`application/json` with the shape `{"Version":"v1.2.3","Time":"2019-11-09T21:39:31Z"}`
(an RFC 3339 timestamp), `.mod` is `text/plain`, and `.zip` is `application/zip`.
`@v/list` is plain text, one version per line. The status codes matter as much as
the bodies: a proxy must return 404 Not Found or 410 Gone for a version it does
not have, because those two codes are precisely what tells a comma-separated
`GOPROXY` chain to try the next proxy. A proxy that answered 500 for an unknown
version would wrongly hard-stop the chain.

### GOPROXY is an ordered chain with two separators that mean different things

`GOPROXY` is a list of entries with two possible separators, and the difference
is a security control, not a style choice. A comma (`,`) means "fall through to
the next entry ONLY on HTTP 404 or 410" — a missing module. A pipe (`|`) means
"fall through on ANY error", including a 500, a TLS failure, or a timeout. So
`https://corp.example.com,https://proxy.golang.org` will NOT leak to the public
proxy when the corporate proxy returns a 500 or is unreachable — the build fails
loudly and stays inside the perimeter. `https://corp.example.com|https://proxy.golang.org`
WILL fall through on any transient corporate error, which is convenient but means
a private module fetch can escape to the public internet. Choose the separator by
your failure and exfiltration requirements, per entry if needed.

### The terminal keywords: direct and off

Two keywords terminate the chain. `direct` means "fetch from the module's
version-control system directly" — the private-VCS mode, and the reason the
default chain ends in `direct` so that modules the public proxy does not have
still resolve. `off` means "no network at all; use only what is already in the
module cache" — the offline and air-gapped build mode. The default `GOPROXY` is
`https://proxy.golang.org,direct`: try the public proxy, and for anything it
returns 404/410 on (including private modules), go straight to VCS.

### GOMODCACHE is a content-addressed, immutable store

`GOMODCACHE` (default `$GOPATH/pkg/mod`, i.e. `$HOME/go/pkg/mod`) holds every
downloaded module keyed by `module@version`. Entries are content-addressed and
effectively immutable; the files are written read-only. That immutability is what
lets two builds share a cache and trust it. It is also why a cache shared across
incompatible toolchains, or written concurrently by two processes, corrupts: a
half-written zip or a hash computed by a buggy writer surfaces later as an
intermittent `verifying ...: checksum mismatch`. The `-modcacherw` flag (or
`GOFLAGS=-modcacherw`) makes entries writable, which trades away exactly the
protection that immutability provides.

### Integrity is a two-layer chain: go.sum and the checksum database

Downloaded bytes are verified twice. First, `go.sum` pins per-module `h1:` hashes
for both the module zip and its `go.mod` file; the hash is a base64-encoded
SHA-256 of a sorted summary of the file list and contents, computed by
`golang.org/x/mod/sumdb/dirhash` (`Hash1`/`HashZip`). On every build the `go`
command recomputes these hashes from the downloaded bytes and compares them to
`go.sum`; a mismatch fails the build. Second, the checksum database (`GOSUMDB`,
default `sum.golang.org`) is a Merkle transparency log consulted the first time a
given module hash is learned, so a proxy that serves tampered bytes is caught even
if `go.sum` did not yet pin that module. The two layers answer different threats:
`go.sum` guards against drift on a module you already trust; the sumdb guards
against a proxy or VCS lying to you the first time.

### Private routing: GOPRIVATE, GONOPROXY, GONOSUMDB

Three related knobs keep internal modules off public infrastructure. `GONOPROXY`
lists module-path patterns that bypass the proxy and fetch direct from VCS.
`GONOSUMDB` lists patterns whose hashes are not sent to the public checksum
database (so a private module path never leaks to `sum.golang.org`). `GOPRIVATE`
is shorthand that seeds BOTH `GONOPROXY` and `GONOSUMDB` with the same patterns —
setting `GOPRIVATE=*.corp.example.com` is equivalent to setting the two derived
variables to that value unless you override them. Matching uses prefix-glob
semantics implemented by `golang.org/x/mod/module.MatchPrefixPatterns`: the
patterns are a comma-separated list of `path.Match` globs, and a target matches if
ANY leading path-element prefix of it matches a glob. So `github.com/corp/*`
matches `github.com/corp/svc/internal` because the prefix `github.com/corp/svc`
matches the glob. Empty or malformed patterns are skipped, not fatal, and a
trailing slash on a pattern is stripped.

### GOINSECURE relaxes transport, not integrity

`GOINSECURE` lists patterns for which the `go` command will use plain `http` and
skip TLS certificate verification. It does NOT disable checksum verification.
A module matched by `GOINSECURE` is still hashed and, unless also excluded by
`GOPRIVATE`/`GONOSUMDB` or `GOSUMDB=off`, still validated against the public
checksum database — which means its path leaks to `sum.golang.org`. Conflating
"insecure transport" with "no integrity checking" is a real and dangerous
misconfiguration. To actually disable the checksum database you must use
`GOPRIVATE`/`GONOSUMDB` for specific paths or `GOSUMDB=off` globally.

### Go 1.24 GOAUTH

Before Go 1.24, authenticating a private proxy or private VCS over HTTPS meant a
`.netrc` file or, worse, credentials embedded in the `GOPROXY` URL (which leak
into logs, `go env`, and error messages). Go 1.24 adds `GOAUTH`: a configurable
mechanism with methods `off`, `netrc`, `git`, or an arbitrary `command`. The
`command` form lets credentials be produced dynamically — for example minting a
short-lived token from a cloud IAM identity per request — so no long-lived secret
sits in a file or a URL. `GOAUTH` supplies the `Authorization` header for both
proxy protocol requests and direct HTTPS VCS fetches.

### GOVCS controls which VCS tools may run

When `GOPROXY` falls through to `direct`, the `go` command invokes a
version-control tool (git, hg, svn, bzr, fossil). `GOVCS` controls which tools are
permitted for which module prefixes, closing an arbitrary-command-execution
surface: without it, a malicious module path could coerce the toolchain into
running an unexpected VCS binary. A typical policy is `GOVCS=*:git,public:git`
to allow only git.

### The trade-off of running an internal proxy

Standing up an internal proxy (Athens, Artifactory, JFrog, GCP Artifact Registry)
buys reproducibility, egress control, availability if an upstream module or its
VCS disappears, and a chokepoint for vulnerability gating. The cost is real:
operational ownership, a decision about the sumdb policy for mirrored public
modules, and the responsibility to keep the mirror synced with correct 404/410
semantics so clients can fail over. Getting the status codes wrong turns the
mirror from a safety net into a single point of failure.

## Common Mistakes

### Pointing GOPROXY at an untrusted or plain-http server

Wrong: `GOPROXY=http://scratch.example.com`. A malicious or compromised proxy can
serve tampered module bytes.

Fix: use a trusted proxy and keep `GOSUMDB` on so the checksum database catches
tampering even when the bytes come from a proxy you do not fully trust.

### Dropping direct from the chain

Wrong: `GOPROXY=https://proxy.golang.org`. Any module the public proxy does not
have — including every private one — fails to download.

Fix: keep a terminal `direct` (or a private-proxy entry) so unresolved modules
still have a path: `GOPROXY=https://corp.example.com,https://proxy.golang.org,direct`.

### Sharing one cache across incompatible toolchains or concurrent writers

Wrong: one `GOMODCACHE` written by two Go versions or two parallel jobs. The
read-only, content-addressed entries corrupt and surface as intermittent
`checksum mismatch` failures.

Fix: use a per-toolchain cache, or a properly locked shared cache, and never
enable `-modcacherw` on a shared cache.

### Using comma where you meant pipe, or the reverse

Wrong: relying on a comma chain and assuming a down corporate proxy falls through
(it does not — a 500/timeout hard-stops the build); or relying on a pipe chain
around a private proxy and assuming private fetches stay internal (they do not — a
transient error leaks the fetch to the next entry).

Fix: choose the separator per your failure/security requirement. Comma to contain
private fetches; pipe only where any-error fallthrough is acceptable.

### Assuming GOINSECURE disables checksum verification

Wrong: setting `GOINSECURE=corp.example.com/*` and expecting private hashes to
stay off the public sumdb. It only relaxes TLS/http; the hashes still go to
`sum.golang.org`, leaking private module paths.

Fix: exclude private paths with `GOPRIVATE`/`GONOSUMDB`, or disable the sumdb with
`GOSUMDB=off`, separately from any transport relaxation.

### Building proxy URLs without case-encoding

Wrong: requesting `github.com/Azure/azure-sdk-for-go/@v/list` literally. The proxy
stores it case-encoded and answers 404.

Fix: encode uppercase letters to `!` + lowercase (`github.com/!azure/...`) on the
way out and decode on the way in. Any hand-rolled mirror or URL builder must do
both.

### Embedding tokens in the GOPROXY URL

Wrong: `GOPROXY=https://user:token@corp.example.com`. The token leaks into build
logs, `go env`, and error output.

Fix: on Go 1.24+ use `GOAUTH` (netrc/git/command); on older toolchains use a
`.netrc` file, never inline credentials.

### Committing an untidy go.sum

Wrong: a `go.sum` missing the `/go.mod` hash for a dependency, or missing an entry
for a required module. Builds pass locally from a warm cache and fail
reproducibly in clean CI — or at deploy.

Fix: gate on `go.sum` completeness (what `go mod verify`/`go mod tidy` would
change) so drift fails the PR, not the release.

Next: [01-effective-goenv-resolver.md](01-effective-goenv-resolver.md)
