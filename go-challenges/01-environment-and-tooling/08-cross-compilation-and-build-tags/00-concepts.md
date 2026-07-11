# Cross-Compilation and Build Tags — Concepts

A backend team almost never ships to one target. The same Go service runs on
linux/amd64 EC2, linux/arm64 Graviton and Raspberry Pi edge nodes, and the
developer's darwin/arm64 laptop, and it is all produced from a single source
tree in a single CI job with no C cross-toolchain in sight. The release
engineering skills that make that possible are the subject of this lesson:
selecting targets with `GOOS`/`GOARCH`, producing statically linked
`CGO_ENABLED=0` binaries, stamping version metadata with `-ldflags -X`, auditing
exactly which files and dependencies land in a binary, gating slow tests behind a
build tag, picking a `GOAMD64` microarchitecture level for a known fleet, and
reading embedded build provenance back out of a shipped binary for incident
forensics. This file is the conceptual foundation; read it once and you have what
you need for each of the ten independent exercises that follow.

## Concepts

### GOOS and GOARCH are two independent selectors

`GOOS` is the target operating system (`linux`, `darwin`, `windows`, `freebsd`,
`js`, `wasip1`, and more); `GOARCH` is the target CPU architecture (`amd64`,
`arm64`, `386`, `arm`, `riscv64`, `wasm`, and more). They are orthogonal, not one
knob: `linux/arm64` (Graviton, a Pi) is a different binary from `darwin/arm64`
(Apple Silicon) even though both are arm64, and different again from
`linux/amd64`. The authoritative, version-specific matrix is `go tool dist list`
(around 49 pairs on recent releases). The classic bug is exporting `GOOS` while
forgetting `GOARCH`: the build silently inherits the host's architecture, so on
an Apple laptop `GOOS=linux go build` produces a `linux/arm64` binary when you
meant `linux/amd64`. Always set both.

### Cross-compilation is first-class; cgo is the one thing that breaks it

The Go toolchain ships every target's runtime and assembler, so cross-compiling
is a zero-setup operation: set the two variables and `go build` emits a native
binary for that pair. The single thing that breaks this is cgo, because calling C
requires a C cross-compiler for each target. `CGO_ENABLED=0` disables cgo and
yields a pure-Go, statically linked binary that cross-compiles cleanly and runs
on `scratch`, distroless, and minimal containers with no libc present. The vast
majority of Go services ship this way.

Disabling cgo is not purely free: it changes behavior. The pure-Go `net` and
`os/user` resolvers replace the libc (NSS) ones, so DNS resolution and
`user.Lookup`/getpwuid semantics differ from a cgo build. For containers this is
usually what you want (no dependency on the host's `/etc/nsswitch.conf`), but it
is a real difference to be aware of when a lookup behaves differently in the
container than on the developer's machine.

### //go:build uses real boolean grammar and needs the blank line

Since Go 1.17 the build-constraint comment is `//go:build` (no space after the
slashes); it replaced the old `// +build` form. It understands a real boolean
grammar with `&&`, `||`, `!`, and parentheses, so `//go:build (linux || darwin)
&& amd64` means exactly what it reads. The constraint must appear before the
package clause, separated from it by exactly one blank line:

```go
//go:build linux

package server
```

Miss that blank line and the line degrades into an ordinary doc comment that is
silently ignored, so the file compiles on every platform — the single most common
"why is my build tag not working" bug. `gofmt` and `go vet` are the guardrails
that catch it. Do not mix `//go:build` with `// +build`; they can drift out of
sync, and `gofmt` will rewrite one from the other in ways you did not intend.

### The predefined tags beyond GOOS/GOARCH

The toolchain satisfies more than the OS/arch terms. `unix` is true for any
Unix-like `GOOS` (added in Go 1.19). `gc` and `gccgo` name the compiler. `cgo` is
satisfied when cgo is enabled (it tracks `CGO_ENABLED`). And there is a `go1.N`
term for every release up to the one building the code, so `//go:build go1.24`
lets a file participate only from that version forward — the mechanism behind
version-conditional shims.

### File-name suffixes are implicit constraints

A file named `foo_linux.go` carries an implicit `//go:build linux`;
`foo_linux_amd64.go` implies both `GOOS=linux` and `GOARCH=amd64`. The suffix and
an explicit `//go:build` on the same file are ANDed — both must hold. Prefer the
suffix when the constraint is a single tag; prefer an explicit `//go:build` for
any boolean expression. The failure mode to know: a *mis*-spelled suffix
(`platform_pluto.go`, `util_darvin.go`) is not a recognized GOOS/GOARCH, so it is
treated as an ordinary file with no constraint and compiles on every platform —
the opposite of the intent, and invisible until something breaks on a target it
should not have been in.

### Always provide a fallback for a tag-defined symbol

If a symbol exists only in tagged files, every unlisted `GOOS`/`GOARCH` fails to
build with `undefined: platformFact`. The fix is a fallback file — typically
`//go:build !linux && !darwin && !windows` — that supplies a default. This is the
surprise that bites when someone adds a new target to the release matrix and the
build breaks on the platform nobody wrote a file for.

### Custom -tags are conditional compilation, not runtime flags

`go build -tags debug` marks `debug` satisfied, so files behind `//go:build
debug` are included. This is compile-time selection, not a runtime branch: code
behind the tag is *physically absent* from a default build. That is categorically
different from `if debug { ... }`, which leaves the code (and any assertions or
embedded secrets) in the shipped binary. Build tags are the canonical mechanism
for debug/tracing builds, enterprise-versus-OSS editions, and separating a slow
integration-test binary from the fast unit suite. Contrast with
`testing.Short()`: a short-skipped test is still *compiled* into the binary and
merely skipped at runtime, whereas a `//go:build integration` test is compiled
out entirely unless the tag is set.

### go list is the audit trail

`go list -f '{{.GoFiles}}' ./pkg` and `go list -deps` answer, per target,
"exactly what source and which dependencies are compiled into this binary?"
Because `GOOS`/`GOARCH` parameterize the query
(`GOOS=linux go list -f '{{.GoFiles}}' ...`), this is how you audit that exactly
one `platform_*.go` is selected per OS, and it is the evidence for a supply-chain
review of the transitive dependency set.

### -ldflags -X stamps metadata into package-level string vars

`-ldflags "-X importpath.name=value"` sets a variable at link time. It works only
on package-level *string* variables — not consts, not ints, not function locals —
and the key must be the full import path, not the short package name. This is how
CI injects a version, git SHA, and build date without editing source. Two
companions: `-ldflags "-s -w"` strips the symbol and DWARF debug tables to shrink
the binary, and `-trimpath` removes absolute build paths so the artifact does not
leak `$HOME` and is reproducible across machines.

### Build provenance is embedded automatically

Modern Go records provenance into every binary. In-process,
`runtime/debug.ReadBuildInfo()` returns the Go version, module path, dependency
versions and sums, and a `Settings` list that includes `vcs.revision`,
`vcs.time`, and `vcs.modified` (the dirty flag). On disk,
`debug/buildinfo.ReadFile(path)` (and `go version -m binary`) read the same from
a binary you never ran. This is free forensics: pull a binary off a crashing
production host and recover the exact commit and dirty state it was built from.
It is controlled by `-buildvcs`; a build run outside the VCS tree, or with
`-buildvcs=false`, omits the `vcs.*` settings.

### Reproducible builds and the microarchitecture level

Identical source, toolchain, and flags should yield byte-identical binaries.
`-trimpath`, a pinned toolchain, `CGO_ENABLED=0`, and controlling the embedded
VCS/time inputs make that achievable, which is what underpins verifiable supply
chains (SLSA) and cacheable release artifacts. Separately, `GOAMD64` (`v1`..`v4`)
and `GOARM64` (`v8.x`/`v9.x`) set a *minimum* CPU microarchitecture: a higher
level lets the compiler emit newer instructions (SSE4, AVX, AVX2, AVX-512) for
speed, but a `v3` binary aborts at startup on a CPU that lacks those
instructions. The level must match the least-capable machine in the fleet. The
build cache is microarch-aware, so switching levels needs no cache cleaning.

### Cross binaries do not run on the host, and //go:build ignore

A cross-compiled binary does not run on the machine that built it: a Linux ELF
will not load under the macOS Mach-O loader. The workflow is build-then-transfer
(or run under a container/emulator), which is why `GOOS=linux go run` on a Mac is
almost always a mistake. Finally, the `//go:build ignore` convention marks a Go
file excluded from the normal package build and meant to be run standalone with
`go run file.go` — the idiomatic way to ship a small build/release orchestrator
inside a repo without it becoming part of the package.

## Common Mistakes

### Using the deprecated // +build syntax

Wrong: putting `// +build linux` on a file (or mixing it with `//go:build`). The
two forms can drift apart and `gofmt` will rewrite one from the other
unexpectedly. Fix: use `//go:build linux` alone; it is the only form since Go
1.17.

### Omitting the blank line before package

Wrong: `//go:build linux` immediately followed by `package main` with no blank
line. The constraint becomes a plain doc comment and is ignored, so the file
builds everywhere. Fix: exactly one blank line between the constraint and the
package clause; `go vet` flags the mistake.

### Defining a tagged symbol with no fallback

Wrong: `platform_linux.go` and `platform_darwin.go` only, then building for
`windows` or `freebsd` and getting `undefined: platformFact`. Fix: add a fallback
file (`//go:build !linux && !darwin && !windows`) so every target compiles.

### Cross-compiling cgo, or forgetting what CGO_ENABLED=0 changes

Wrong: cross-compiling a cgo-dependent package without a C cross-toolchain and
getting a missing-C-compiler error. Fix: `CGO_ENABLED=0`. Conversely, forgetting
that `CGO_ENABLED=0` swaps in the pure-Go `net`/`os/user` resolvers and changes
DNS and user-lookup behavior is its own bug.

### Setting GOOS without GOARCH

Wrong: `GOOS=linux go build .` on an Apple laptop, silently producing a
`linux/arm64` binary. Fix: always set both, `GOOS=linux GOARCH=amd64 go build .`.

### Running a cross binary on the build host

Wrong: executing a `linux/amd64` binary on macOS and getting an exec-format
error. Fix: cross binaries must be transferred to the target OS/arch (or run in a
container); they do not run on the builder.

### Expecting -ldflags -X to set a const, int, or local

Wrong: `-X pkg.BuildNumber=42` on an `int`, or targeting a function-local
variable, or using the short package name in the key. `-X` silently no-ops on
anything that is not a package-level string var, and the key must be the full
import path. Fix: declare an exported package-level `var Version string` and use
its full import path.

### Assuming build metadata appears for free

Wrong: expecting `vcs.revision` and finding it missing. It is absent when the
build ran outside the VCS tree, or with `-buildvcs=false`. Fix: build inside the
repo with VCS stamping on, and read `vcs.modified` to detect a dirty tree.

### Shipping too high a microarch level

Wrong: distributing a `GOAMD64=v3`/`v4` (or `GOARM64` v9) binary to a fleet with
older CPUs, causing a hard startup abort rather than a graceful fallback. Fix:
build for the least-capable machine in the fleet.

### Using a runtime if for debug code, or Short() for integration tests

Wrong: `if debug { ... }` leaves the debug code and any embedded secrets in the
release binary; gating a slow integration test only with `testing.Short()` still
compiles it into every test binary. Fix: put debug-only code behind `//go:build
debug` and slow tests behind `//go:build integration` so they are excluded from
the artifact entirely unless the tag is set.

Next: [01-platform-abstraction-build-tags.md](01-platform-abstraction-build-tags.md)
