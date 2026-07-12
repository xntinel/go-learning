# Exercise 9: A Supply-Chain Security Gate

This is the on-the-job module. A backend team does not trust dependencies by
default; it runs a gate on every merge that fails the build if `go.mod` is dirty, a
cached module is tampered, or a reachable dependency has a known vulnerability, and
it configures how private modules are fetched. This exercise builds that gate
around a small service package.

This module is fully self-contained: its own `go mod init`, all code inline, its
own tests. Nothing here imports another exercise.

## What you'll build

```text
supplychain/                independent module: example.com/supplychain
  go.mod                    require golang.org/x/text
  user.go                   NormalizeUsername using cases.Fold
  cmd/
    demo/
      main.go               normalizes a few usernames
  user_test.go              table over normalization
```

- Files: `user.go`, `cmd/demo/main.go`, `user_test.go`.
- Implement: `NormalizeUsername(s) (string, error)` folding case and rejecting blanks.
- Test: table over valid and blank input.
- Verify: `go mod verify`; a `-mod=readonly` build that fails on a hand-broken `require`; `govulncheck ./...`; `GOPRIVATE`/`GOSUMDB` config; `go version -m` of the built binary.

Set up the module:

```bash
mkdir -p go-solutions/01-environment-and-tooling/02-go-modules-and-dependencies/09-supply-chain-security-gate/cmd/demo
cd go-solutions/01-environment-and-tooling/02-go-modules-and-dependencies/09-supply-chain-security-gate
go get golang.org/x/text/cases
```

### The service code

The package normalizes usernames to a canonical case-folded key so that `Alice`
and `alice` cannot both register. It rejects a blank name with a sentinel.

Create `user.go`:

```go
package supplychain

import (
	"errors"
	"strings"

	"golang.org/x/text/cases"
)

// ErrEmptyUsername is returned when the trimmed username is empty.
var ErrEmptyUsername = errors.New("username must not be empty")

// NormalizeUsername returns a case-folded, trimmed canonical form of a
// username, so lookups are case-insensitive. A blank username is rejected.
func NormalizeUsername(s string) (string, error) {
	trimmed := strings.TrimSpace(s)
	if trimmed == "" {
		return "", ErrEmptyUsername
	}
	return cases.Fold().String(trimmed), nil
}
```

### The demo

Create `cmd/demo/main.go`:

```go
package main

import (
	"fmt"

	"example.com/supplychain"
)

func main() {
	for _, name := range []string{"Alice", "BOB", "  carol  "} {
		out, _ := supplychain.NormalizeUsername(name)
		fmt.Printf("%-9q -> %q\n", name, out)
	}
}
```

Run it:

```bash
go run ./cmd/demo
```

Expected output:

```
"Alice"   -> "alice"
"BOB"     -> "bob"
"  carol  " -> "carol"
```

### The test

Create `user_test.go`:

```go
package supplychain

import (
	"errors"
	"testing"
)

func TestNormalizeUsername(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name    string
		input   string
		want    string
		wantErr error
	}{
		{name: "lowercases", input: "Alice", want: "alice"},
		{name: "folds all caps", input: "BOB", want: "bob"},
		{name: "trims then folds", input: "  Carol  ", want: "carol"},
		{name: "blank rejected", input: "   ", wantErr: ErrEmptyUsername},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()

			got, err := NormalizeUsername(tc.input)
			if tc.wantErr != nil {
				if !errors.Is(err, tc.wantErr) {
					t.Fatalf("NormalizeUsername(%q) err = %v, want %v", tc.input, err, tc.wantErr)
				}
				return
			}
			if err != nil {
				t.Fatalf("NormalizeUsername(%q) unexpected err = %v", tc.input, err)
			}
			if got != tc.want {
				t.Fatalf("NormalizeUsername(%q) = %q, want %q", tc.input, got, tc.want)
			}
		})
	}
}
```

### The gate: verify, readonly, scan

A CI gate for dependencies is a short script. Each line answers a specific attack
or drift.

```bash
go mod verify
GOFLAGS=-mod=readonly go build ./...
go install golang.org/x/vuln/cmd/govulncheck@latest
govulncheck ./...
```

`go mod verify` re-hashes every cached module against `go.sum` and prints `all
modules verified` on success; a tampered module cache fails here. `-mod=readonly`
(the default since Go 1.16, made explicit here) means the build *fails* if `go.mod`
is out of date rather than silently rewriting it — so a pull request that added an
import without running `go mod tidy` is rejected on the runner instead of
mutating the graph under CI. Prove it: break the requirement and rebuild.

```bash
go mod edit -require golang.org/x/text@v0.0.0
GOFLAGS=-mod=readonly go build ./...
```

The build fails with an "updates to go.mod needed" / "missing go.sum entry" error
because readonly forbids the toolchain from reconciling the graph. Restore it:

```bash
go mod edit -require golang.org/x/text@v0.38.0
go mod tidy
```

`govulncheck` is the vulnerability scanner. It is call-graph-aware: it reports a
CVE only when your code actually *reaches* the vulnerable symbol, so it does not
drown you in advisories for code paths you never call. A govulncheck finding in
an affected project (one that actually reaches the vulnerable symbol) looks like
this:

```text
=== Symbol Results ===

Vulnerability #1: GO-2021-0113
    Out-of-bounds read in golang.org/x/text
  More info: https://pkg.go.dev/vuln/GO-2021-0113
  Module: golang.org/x/text
    Found in: golang.org/x/text@v0.3.5
    Fixed in: golang.org/x/text@v0.3.7
    Example traces found:
      #1: cmd/demo/main.go:14:33: main.main calls language.Parse

Your code is affected by 1 vulnerability from 1 module.
```

`govulncheck` exits non-zero when it finds an affected vulnerability, which is what
fails the pipeline. The fix is to raise the dependency past the "Fixed in" version.

### Private modules and binary forensics

Internal modules must not be fetched through the public proxy or checked against
the public checksum database. `GOPRIVATE` marks path prefixes as private in one
setting; it implies both `GONOSUMDB`/`GONOSUMCHECK` (skip the checksum DB) and
proxy bypass for those paths. `GOSUMDB` names the checksum database (or `off`),
`GOINSECURE` allows plain HTTP for named prefixes, and `GOVCS` restricts which
version-control tools may be used.

```bash
go env -w GOPRIVATE=github.com/acme/*,git.internal.example.com/*
go env -w GOSUMDB=sum.golang.org
```

Finally, an already-built binary carries its own bill of materials. `go version -m`
reads the module versions embedded at link time — the answer to "which version did
we actually ship?" during an incident:

```bash
go build -o /tmp/demo ./cmd/demo
go version -m /tmp/demo
```

```text
/tmp/demo: go1.26.0
	path	example.com/supplychain/cmd/demo
	mod	example.com/supplychain	(devel)
	dep	golang.org/x/text	v0.38.0	h1:sXmwo9DwP3OK9EZ7PqAdaooSGozfl/3a6/xJcbzPRhE=
	build	-buildmode=exe
	...
```

## Review

The gate is correct when `go mod verify` is silent, a hand-broken `require` fails
the `-mod=readonly` build, `govulncheck` exits zero on a clean tree and non-zero on
an affected one, and `go version -m` reports the versions you expect. The mistakes
this trains you out of: relying on the default writable build in CI (a dirty
`go.mod` should fail the pipeline, not be silently fixed — set `-mod=readonly`);
treating `govulncheck` as noise because a naive scanner over-reports (it is
reachability-aware, so a finding means your code path is actually exposed); and
fetching private modules through the public proxy and checksum DB (set `GOPRIVATE`
so internal code never leaks a path to `sum.golang.org`). This four-command gate is
the concrete artifact a backend team wires into its merge pipeline.

## Resources

- [Tutorial: Find and fix vulnerable dependencies with govulncheck](https://go.dev/doc/tutorial/govulncheck) — installing and running the scanner, and reading its report.
- [go mod verify](https://go.dev/ref/mod#go-mod-verify) and [build modes (-mod)](https://go.dev/ref/mod#build-commands) — checksum verification and readonly enforcement.
- [Private modules configuration](https://go.dev/ref/mod#private-modules) — `GOPRIVATE`, `GOSUMDB`, `GONOSUMDB`, `GOINSECURE`, `GOVCS`.
- [go version -m](https://pkg.go.dev/cmd/go#hdr-Print_Go_version) — reading module versions embedded in a binary.

---

Back to [08-minimal-version-selection-and-graph-pruning.md](08-minimal-version-selection-and-graph-pruning.md) | Next: [10-vendoring-for-hermetic-builds.md](10-vendoring-for-hermetic-builds.md)
