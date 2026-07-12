# Exercise 5: Match module paths against GOPRIVATE/GONOPROXY/GONOSUMDB patterns

Onboarding a private registry means being certain that every corp module bypasses
`proxy.golang.org` and never leaks its path to `sum.golang.org`. That certainty
comes from the prefix-glob matcher the `go` command uses. This exercise implements
that matcher from first principles and cross-checks it against the reference.

## What you'll build

```text
goprivmatch/               independent module: example.com/goprivmatch
  go.mod                   go 1.26 (requires golang.org/x/mod for the parity test)
  match.go                 MatchPrefixPatterns, Router, Decision, FromEnv
  cmd/
    demo/
      main.go              routes four modules through a private-registry config
  match_test.go            table-driven matcher + router + shorthand tests
  parity_test.go           cross-check against x/mod/module.MatchPrefixPatterns
  example_test.go          ExampleRouter_Route with // Output
```

- Files: `match.go`, `cmd/demo/main.go`, `match_test.go`, `parity_test.go`, `example_test.go`.
- Implement: `MatchPrefixPatterns(globs, target) bool` (comma-separated `path.Match` globs, any leading path-element prefix may match, empty/malformed skipped, trailing slash stripped) and a `Router` that decides bypass-proxy and skip-sumdb per module.
- Test: prefix matching, `*.corp.example.com`, public non-match, malformed-skipped; a parity test cross-checking a grid against `golang.org/x/mod/module.MatchPrefixPatterns`.
- Verify: `go test -count=1 -race ./...`

Set up the module:

```bash
go mod edit -go=1.26
go get golang.org/x/mod/module
```

### The matching rule is prefix-by-path-element, not substring

The subtle part of `GOPRIVATE` matching is that a pattern matches if it matches
ANY leading path-element prefix of the target, not the whole path and not a
substring. So `github.com/corp/*` matches `github.com/corp/svc/internal`: the glob
has two slashes, so it is compared against the first three path elements of the
target — `github.com/corp/svc` — and `path.Match` succeeds there. The algorithm
counts the slashes in the glob (call it N), walks the target truncating it just
before its (N+1)th slash, and runs `path.Match(glob, prefix)`. Empty patterns are
skipped, a trailing slash on a pattern is stripped first, and a malformed glob
(one `path.Match` rejects) is skipped rather than fatal — a typo in one pattern
must not disable routing for the others. This is exactly the behavior of
`golang.org/x/mod/module.MatchPrefixPatterns`, which the `go` command itself uses,
so we reproduce it precisely and then prove parity in a test.

The `Router` on top is the onboarding artifact: given the `GONOPROXY` and
`GONOSUMDB` pattern lists, it decides per module whether to bypass the proxy
(fetch direct from VCS) and whether to skip the checksum database. `FromEnv`
applies the `GOPRIVATE` shorthand — both lists default to `GOPRIVATE` unless set
explicitly — mirroring the resolver from Exercise 1 so the two views stay
consistent.

Create `match.go`:

```go
// Package goprivmatch implements the prefix-glob matching the go command uses for
// GOPRIVATE/GONOPROXY/GONOSUMDB, and routes a module path to a fetch decision.
package goprivmatch

import (
	"path"
	"strings"
)

// MatchPrefixPatterns reports whether target matches any of the comma-separated
// path.Match globs in globs, where a glob may match any leading path-element
// prefix of target. Empty patterns are skipped and a trailing slash is stripped.
// It mirrors golang.org/x/mod/module.MatchPrefixPatterns.
func MatchPrefixPatterns(globs, target string) bool {
	for globs != "" {
		var glob string
		if i := strings.Index(globs, ","); i >= 0 {
			glob, globs = globs[:i], globs[i+1:]
		} else {
			glob, globs = globs, ""
		}
		glob = strings.TrimSuffix(glob, "/")
		if glob == "" {
			continue
		}

		// A glob with N slashes must match the first N+1 path elements of target.
		n := strings.Count(glob, "/")
		prefix := target
		for i := 0; i < len(target); i++ {
			if target[i] == '/' {
				if n == 0 {
					prefix = target[:i]
					break
				}
				n--
			}
		}
		if n > 0 {
			// target has fewer elements than the glob: cannot match.
			continue
		}
		if matched, _ := path.Match(glob, prefix); matched {
			return true
		}
	}
	return false
}

// Decision is the routing outcome for one module path.
type Decision struct {
	BypassProxy bool // GONOPROXY matched: fetch direct from VCS
	SkipSumDB   bool // GONOSUMDB matched: do not consult the public checksum db
}

// Router decides, per module path, whether to bypass the proxy and whether to
// skip the checksum database, from the GONOPROXY and GONOSUMDB pattern lists.
type Router struct {
	NoProxy string
	NoSumDB string
}

// FromEnv builds a Router applying the GOPRIVATE shorthand: GONOPROXY and
// GONOSUMDB default to GOPRIVATE unless set explicitly.
func FromEnv(goprivate, gonoproxy, gonosumdb string) Router {
	if gonoproxy == "" {
		gonoproxy = goprivate
	}
	if gonosumdb == "" {
		gonosumdb = goprivate
	}
	return Router{NoProxy: gonoproxy, NoSumDB: gonosumdb}
}

// Route classifies a module path.
func (r Router) Route(modulePath string) Decision {
	return Decision{
		BypassProxy: MatchPrefixPatterns(r.NoProxy, modulePath),
		SkipSumDB:   MatchPrefixPatterns(r.NoSumDB, modulePath),
	}
}
```

### The runnable demo

The demo configures a private-registry onboarding — `*.corp.example.com` plus
`github.com/corp/*` — and routes four modules, showing that internal ones bypass
the proxy and sumdb while public ones do not.

Create `cmd/demo/main.go`:

```go
package main

import (
	"fmt"

	"example.com/goprivmatch"
)

func main() {
	// Onboarding a private registry: corp modules must bypass the proxy and the
	// public checksum database.
	r := goprivmatch.FromEnv("*.corp.example.com,github.com/corp/*", "", "")

	modules := []string{
		"github.com/corp/billing/internal",
		"git.corp.example.com/team/lib",
		"github.com/public/cobra",
		"golang.org/x/mod",
	}
	for _, m := range modules {
		d := r.Route(m)
		fmt.Printf("%-38s bypass-proxy=%-5v skip-sumdb=%v\n", m, d.BypassProxy, d.SkipSumDB)
	}
}
```

Run it:

```bash
go run ./cmd/demo
```

Expected output:

```text
github.com/corp/billing/internal       bypass-proxy=true  skip-sumdb=true
git.corp.example.com/team/lib          bypass-proxy=true  skip-sumdb=true
github.com/public/cobra                bypass-proxy=false skip-sumdb=false
golang.org/x/mod                       bypass-proxy=false skip-sumdb=false
```

### Tests

The table pins each rule of the algorithm; the router test confirms the
bypass/skip decisions; and the parity test cross-checks a grid of globs against
targets versus the real `x/mod/module.MatchPrefixPatterns`, so any divergence from
the `go` command's own behavior is caught. (In a strictly dependency-free core
you would guard the parity file behind a `//go:build` tag; here we run it directly
so the gate actually proves parity.)

Create `match_test.go`:

```go
package goprivmatch

import "testing"

func TestMatchPrefixPatterns(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name   string
		globs  string
		target string
		want   bool
	}{
		{"prefix match on corp glob", "github.com/corp/*", "github.com/corp/svc/internal", true},
		{"exact repo prefix", "github.com/corp/svc", "github.com/corp/svc/internal", true},
		{"star suffix domain", "*.corp.example.com", "git.corp.example.com/team/lib", true},
		{"public module does not match", "github.com/corp/*", "github.com/public/lib", false},
		{"empty pattern skipped", "", "github.com/corp/svc", false},
		{"malformed glob is skipped not fatal", "[", "github.com/corp/svc", false},
		{"trailing slash stripped", "github.com/corp/", "github.com/corp/svc", true},
		{"multiple patterns, second matches", "example.com/a,github.com/corp/*", "github.com/corp/x", true},
		{"glob longer than target does not match", "github.com/corp/svc/sub", "github.com/corp", false},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			if got := MatchPrefixPatterns(tt.globs, tt.target); got != tt.want {
				t.Errorf("MatchPrefixPatterns(%q,%q) = %v; want %v", tt.globs, tt.target, got, tt.want)
			}
		})
	}
}

func TestRoute(t *testing.T) {
	t.Parallel()
	r := FromEnv("*.corp.example.com,github.com/corp/*", "", "")
	tests := []struct {
		modPath string
		want    Decision
	}{
		{"github.com/corp/svc", Decision{BypassProxy: true, SkipSumDB: true}},
		{"git.corp.example.com/team/lib", Decision{BypassProxy: true, SkipSumDB: true}},
		{"github.com/public/lib", Decision{BypassProxy: false, SkipSumDB: false}},
	}
	for _, tt := range tests {
		if got := r.Route(tt.modPath); got != tt.want {
			t.Errorf("Route(%q) = %+v; want %+v", tt.modPath, got, tt.want)
		}
	}
}

func TestFromEnvShorthand(t *testing.T) {
	t.Parallel()
	// GOPRIVATE seeds both; an explicit GONOPROXY overrides only that side.
	r := FromEnv("*.corp.example.com", "vcs.corp.example.com", "")
	if r.NoProxy != "vcs.corp.example.com" {
		t.Errorf("NoProxy = %q; want explicit override", r.NoProxy)
	}
	if r.NoSumDB != "*.corp.example.com" {
		t.Errorf("NoSumDB = %q; want GOPRIVATE default", r.NoSumDB)
	}
}
```

Create `parity_test.go`:

```go
package goprivmatch

import (
	"testing"

	xmod "golang.org/x/mod/module"
)

// TestParityWithXmod cross-checks our matcher against the reference
// implementation in golang.org/x/mod/module across a grid of cases.
func TestParityWithXmod(t *testing.T) {
	t.Parallel()
	globsList := []string{
		"github.com/corp/*",
		"*.corp.example.com",
		"github.com/corp/svc",
		"example.com/a,github.com/corp/*",
		"github.com/corp/",
		"",
	}
	targets := []string{
		"github.com/corp/svc/internal",
		"git.corp.example.com/team/lib",
		"github.com/public/lib",
		"github.com/corp",
		"example.com/a/b",
	}
	for _, globs := range globsList {
		for _, target := range targets {
			got := MatchPrefixPatterns(globs, target)
			want := xmod.MatchPrefixPatterns(globs, target)
			if got != want {
				t.Errorf("parity mismatch globs=%q target=%q: ours=%v xmod=%v", globs, target, got, want)
			}
		}
	}
}
```

Create `example_test.go`:

```go
package goprivmatch

import "fmt"

func ExampleRouter_Route() {
	r := FromEnv("*.corp.example.com", "", "")
	d := r.Route("git.corp.example.com/team/lib")
	fmt.Printf("bypass=%v skipsumdb=%v\n", d.BypassProxy, d.SkipSumDB)
	// Output: bypass=true skipsumdb=true
}
```

## Review

The matcher is correct when it matches on a leading path-element prefix (not a
substring), skips empty and malformed patterns without failing, and agrees with
`x/mod/module.MatchPrefixPatterns` on the whole grid — the parity test is the real
proof, because it ties your implementation to the exact algorithm the toolchain
runs. The mistake to avoid is treating the pattern as matching the full path:
`github.com/corp/*` would then miss `github.com/corp/svc/internal`, and a corp
module would silently be fetched through the public proxy and its path leaked to
the public sumdb. Wire `GONOPROXY` and `GONOSUMDB` from the same `GOPRIVATE`
shorthand so a single onboarding pattern governs both, and run `go test -race`.

## Resources

- [`golang.org/x/mod/module.MatchPrefixPatterns`](https://pkg.go.dev/golang.org/x/mod/module#MatchPrefixPatterns) — the reference algorithm this reproduces.
- [Go Modules Reference: Private modules (GOPRIVATE, GONOPROXY, GONOSUMDB)](https://go.dev/ref/mod#private-modules) — how the patterns route fetches.
- [`path.Match`](https://pkg.go.dev/path#Match) — the glob syntax each comma-separated pattern uses.

---

Back to [00-concepts.md](00-concepts.md) | Prev: [04-minimal-goproxy-server.md](04-minimal-goproxy-server.md) | Next: [06-module-ziphash-verifier.md](06-module-ziphash-verifier.md)
