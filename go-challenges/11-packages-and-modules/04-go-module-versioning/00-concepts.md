# Go Module Versioning: Semantic Import Paths, MVS, and Release Hygiene — Concepts

Module versioning is the seam where three senior concerns meet: release
engineering (what version am I shipping and how do I take one back), supply-chain
security (is this dependency the byte-for-byte artifact its author published), and
API compatibility (can a v2 land without a flag day). A team that treats `go.mod`
as an opaque file the toolchain rewrites will, sooner or later, ship a `replace`
to production, force-push over a released tag, or spend an afternoon asking "why
did `go` pick *that* version." Every one of those is a versioning fact you can
reason about precisely once you hold the model. This file is that model; the
exercises that follow turn each fact into code that a real backend owner writes —
a `/version` endpoint, a CI supply-chain linter, a retraction detector, an MVS
resolver.

## The module path is the import path

The `module` line in `go.mod` is not metadata and not a registry key — there is no
registry mapping in Go modules. It is the literal import-path prefix of every
package the module contains. Declare `module example.com/billing/pricing` and the
package in `pricing/discount/` is imported as
`example.com/billing/pricing/discount`, resolved by fetching the module whose path
is the longest matching prefix. Rename the module and every import statement in
every consumer, transitive included, breaks at once. This is why the module path
is a public contract, not a naming convenience: it is baked into the source of
everyone who depends on you.

The corollary drives everything below. Because the path *is* the import, two
modules with different paths are, to the compiler, two entirely unrelated things —
even if one is "version 2" of the other. Go exploits that deliberately.

## Semantic Import Versioning: v2+ carries a /vN suffix

Go modules use semantic versioning — `vMAJOR.MINOR.PATCH`, always with the leading
`v`. The rule that surprises people: for major version 2 and above, the major
number must appear as a `/vN` suffix in the module path *and* therefore in every
import path. `module example.com/billing/pricing/v2`, imported as
`example.com/billing/pricing/v2/...`. This is Semantic Import Versioning, and it
follows directly from "the path is the import." A breaking change is, by
definition, a change consumers must opt into; giving v2 a distinct import path
makes the opt-in the import statement itself.

The payoff is that v1 and v2 are different modules and can be linked into the same
binary simultaneously. A large dependency tree where one library still needs
`pricing` v1 and your new code wants `pricing/v2` compiles cleanly — both are
present, each satisfying its consumers. There is no flag day, no repo-wide
coordinated upgrade. That is the whole reason SIV exists, and it is why forgetting
the suffix produces the blunt error `module declares its path as X but was
required as X/v2`: you told the compiler two contradictory things about the same
path. (v0 and v1 share the un-suffixed path and are treated as a single
still-stabilizing line; the suffix begins at v2.)

## go.mod is not a lockfile; MVS computes the build list

The single most common misconception: `go.mod`'s `require` lines pin exact
versions. They do not. A `require` records the *minimum acceptable* version of a
dependency. The exact version that ends up in the build — the build list — is
computed by Minimal Version Selection: for each module, take the maximum over all
the minimums that anyone in the graph requires, and select that. Max of the
minimums.

MVS has two properties that make builds trustworthy. It is deterministic: the same
graph always yields the same build list, no "latest at build time" surprise, so a
build today and a build next year from the same `go.mod`/`go.sum` are identical.
And it is monotonic: adding a new requirement can only hold a version the same or
higher, never silently downgrade an unrelated module. When someone says "go still
uses the old version of X," the answer is almost never "run go get -u"; it is "some
module in your graph requires exactly that version — find who with `go mod graph`."
MVS picked the maximum of the minimums, and that maximum was old because a
requirement said so. MVS never reaches for the newest version *available*; it
reaches for the newest version *required*. That is the reproducibility guarantee.

## go.sum is an integrity ledger, not a lockfile

`go.sum` is easy to mistake for a lockfile because it lists versions with hashes,
but its job is different. It is an append-only ledger of cryptographic hashes — one
for each module's zip and one for its `go.mod` — recorded the first time you depend
on that version. On every download the toolchain recomputes the hash and refuses
the module if it does not match, and for public modules it cross-checks against the
transparent checksum database at `sum.golang.org`. It is a tamper-evidence control:
it proves the bytes you are compiling are the exact bytes the author published, so a
compromised proxy or a rewritten tag cannot slip altered code into your build.

Two consequences. First, never hand-edit `go.sum` to silence a mismatch — a
mismatch means the content changed, which is either a legitimate regeneration
(reconcile with `go mod tidy`, then `go mod verify`) or tampering you want to
know about. Editing the hash to match the new bytes defeats the entire mechanism.
Second, you cannot "fix" a bad release by force-pushing over its git tag: the
original hash is already recorded in the checksum database, so every consumer's
download now fails a security check against your republished bytes. The
supply-chain control is doing its job; the release process was wrong.

## Pseudo-versions order untagged commits

Sometimes you depend on a commit that has no semver tag — a bugfix on `main` you
need before the maintainer cuts a release. MVS still needs a total order over
versions, so the toolchain synthesizes a pseudo-version:
`vX.Y.Z-yyyymmddhhmmss-abcdefabcdef`. It encodes the commit's UTC time and a
12-character prefix of its hash, and it comes in three shapes depending on whether
the commit sits after a tag on that line. The ordering is engineered: a
pseudo-version sorts *above* its base tag but *below* the next real tag, and two
pseudo-versions sharing a base sort by their embedded timestamp — chronologically.
So an untagged commit is greater than the last release it descends from and less
than the release that will supersede it, which is exactly what you want MVS to
believe. Seeing a pseudo-version in `go.mod` is a signal: this dependency is pinned
to an unreleased commit, deliberately or by accident, and that is worth a second
look before it ships.

## +incompatible: a v2+ tag from a pre-modules repo

Not every repository that tagged `v2.0.0` adopted the `/v2` module path — many
predate modules entirely. When the toolchain encounters a `v2+` tag on a module
whose `go.mod` (if any) has no `/vN` suffix, it appends `+incompatible` to the
version, e.g. `v3.1.0+incompatible`. The marker says: this major version never
opted into Semantic Import Versioning, so the module system treats the whole repo
as one un-versioned import path across all its major tags. It is not a harmless
annotation. Because those majors share an import path, the toolchain can auto-select
a genuinely breaking major without the import-path opt-in that normally guards you.
A CI policy that flags `+incompatible` catches the class of upgrade that changes API
under a stable import path.

## retract: taking back a release the right way

You published `v1.2.0`, then discovered it leaks a credential or corrupts data. You
cannot delete it (the checksum DB remembers) and you must not overwrite the tag. The
correct move is `retract`: a directive you add to `go.mod` and publish in a *new,
higher* version, which tells the toolchain to stop offering the bad release. It
comes in two forms — a single version, `retract v1.2.0`, or a closed interval,
`retract [v1.0.0, v1.1.9]`, both bounds inclusive — and each may carry a rationale
comment that `go` surfaces to users. A retracted version still exists and still
resolves if something explicitly pins it, but `go get` and `go list -m -u` stop
recommending it and warn when it is in use. Retraction is the sanctioned incident
response for a broken or leaked release; force-pushing a tag is the anti-pattern it
replaces.

## replace and exclude are main-module-only

`replace` (swap a module or version for another path/version) and `exclude` (forbid
a specific version) are powerful and locally scoped: they take effect *only* in the
main module's `go.mod` and are ignored entirely when your module is consumed as a
dependency. This is a deliberate safety property — a library cannot reach up and
rewrite its consumer's build graph — but it is also a trap. A `replace
example.com/dep => ./fork` that patches an upstream bug on your machine, or points a
dependency at a local checkout during development, does nothing for anyone who
imports you. It is a development-and-hotfix tool, never a shipped contract. Committing
one to a library that others depend on is a latent surprise: your CI passes with the
fork, their build silently uses the unpatched upstream.

## runtime/debug: which commit is actually running

At link time the toolchain embeds build metadata into the binary: the main module's
version, the full dependency build list, and a set of build settings including
`vcs.revision` (the git commit), `vcs.time`, and `vcs.modified` (whether the tree
had uncommitted changes). `runtime/debug.ReadBuildInfo` reads it back at runtime.
This is the canonical way a running service answers "exactly which commit is this?"
— indispensable during an incident when the deployed tag and the git history have
to be reconciled fast. A `/version` endpoint that returns `Main.Version`,
`GoVersion`, and the `vcs.*` settings turns forensic guesswork into a curl.

## Direct vs indirect requirements

A `require` line marked `// indirect` is a transitive dependency the toolchain
pinned into your `go.mod` because no *direct* dependency's `go.mod` pinned a high
enough version of it — MVS needed a minimum recorded somewhere, so it recorded it in
yours. After a refactor that drops or adds imports, those `// indirect` lines drift:
stale ones linger, needed ones go missing. `go mod tidy` reconciles the `require` set
with the imports actually present, and `go mod graph` / `go list -m all` expose the
full build list so you can audit direct versus indirect and answer "who pulled this
in." Leaving the require set un-tidied is how reproducible builds and dependency
audits quietly go wrong.

## Common Mistakes

### Omitting the /vN suffix on a v2+ module

Wrong: releasing a breaking change as `v2.0.0` while `go.mod` still says
`module example.com/x`. Consumers who require it get
`module declares its path as example.com/x but was required as example.com/x/v2`.
Fix: the module path carries the major — `module example.com/x/v2` — and imports
become `example.com/x/v2/...`. The path is the import; v2 is a different path.

### Treating go.mod as a lockfile

Wrong: expecting `require X v1.4.0` to mean "use the latest," or being surprised the
build "still uses the old version." Fix: `require` is a minimum; MVS selects the max
of all required minimums. If an old version is selected, some module requires it —
run `go mod graph | grep X` to find who, and raise that module's requirement.

### Hand-editing go.sum to silence a mismatch

Wrong: a checksum mismatch appears, so you paste the new hash into `go.sum`. That
disables the exact tamper check the file exists for. Fix: a mismatch means the
content changed — reconcile with `go mod tidy`, verify with `go mod verify`, and
investigate why the bytes differ before trusting them.

### Force-pushing over a released tag

Wrong: `v1.2.0` was bad, so you re-tag `v1.2.0` on a fixed commit and push. The
checksum database already recorded the original hash; every consumer's download now
fails a security error. Fix: publish `v1.2.1` with the fix and `retract v1.2.0` (in
a new version) to stop offering the bad one.

### Shipping a replace or a pseudo-version to production

Wrong: committing `replace example.com/dep => ./local-fork` or a `require` on a
pseudo-version to a library others consume. `replace` is ignored downstream, so
consumers silently get the unpatched upstream; a pseudo-version advertises an
unreleased, unpinned dependency. Fix: land the fix upstream and require a real
tagged version; keep `replace` to local development and short-lived hotfix branches.

### Passing a version without the leading v to x/mod/semver

Wrong: `semver.Compare("1.2.0", "1.5.0")`. Every function in `golang.org/x/mod/semver`
treats a version without the leading `v` as invalid and returns the empty/false/zero
result, so the comparison silently no-ops. Fix: validate with `semver.IsValid` first
and always carry the `v` prefix (`v1.2.0`).

### Assuming +incompatible is cosmetic

Wrong: ignoring a `v3.4.0+incompatible` in the build list. It means that major never
adopted the `/vN` module path, so the toolchain can auto-upgrade across a real
breaking boundary under a stable import path. Fix: treat `+incompatible` as a policy
signal — pin it deliberately or push the upstream to publish a proper `/vN` module.

### Leaving the require set un-tidied

Wrong: adding or removing imports and never running `go mod tidy`, so `// indirect`
lines drift out of sync with reality. Fix: run `go mod tidy` after import changes;
the require set must match the packages actually imported, or audits and reproducible
builds drift.

Next: [01-versioned-pricing-library.md](01-versioned-pricing-library.md)
