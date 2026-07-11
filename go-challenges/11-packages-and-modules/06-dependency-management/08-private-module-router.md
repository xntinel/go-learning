# Exercise 8: Onboard a private module — GOPRIVATE / checksum-DB routing

A real service tree mixes public dependencies with private ones —
`github.com/acme-corp/*` on GitHub Enterprise or an Artifactory proxy. This exercise
implements the decision the `go` command makes for every module path: given a
`GOPRIVATE` glob list, does this path go through the public proxy and checksum
database, or bypass both? Getting it wrong either leaks private paths or disables
verification for public code.

This module is fully self-contained. It has its own `go mod init`, its own demo,
and its own tests, and imports nothing from the other exercises.

## What you'll build

```text
modrouter/                 independent module: example.com/modrouter
  go.mod                   go 1.26; requires golang.org/x/mod
  modrouter.go             Resolve(path, goprivate) (Route, error)
  cmd/
    demo/
      main.go              routes a public and two private module paths
  modrouter_test.go        table of paths vs GOPRIVATE globs; prefix-match semantics
```

- Files: `modrouter.go`, `cmd/demo/main.go`, `modrouter_test.go`.
- Implement: `Resolve` using `module.CheckPath` to validate the path and `module.MatchPrefixPatterns` to decide whether the path is private (bypass proxy and sumdb) or public (use both).
- Test: a table of module paths against GOPRIVATE glob lists asserting the routing, including the path-prefix matching semantics and an invalid path.
- Verify: `go test -count=1 -race ./...`

Set up the module:

```bash
mkdir -p ~/go-exercises/modrouter/cmd/demo
cd ~/go-exercises/modrouter
go mod init example.com/modrouter
go get golang.org/x/mod
```

### The routing rule

`GOPRIVATE` is a comma-separated list of glob patterns matched against module path
*prefixes*. When a module path matches, the `go` command treats it as private: it is
the umbrella variable that sets the defaults for `GONOPROXY` (do not use the public
module proxy — fetch directly from the VCS) and `GONOSUMDB` (do not consult the
public checksum database). So a private match bypasses *both* the proxy and the
sumdb; a non-match uses both.

The matching is prefix-based, and getting the semantics right is the whole point.
`module.MatchPrefixPatterns(globs, target)` reports whether any path *prefix* of
`target` matches any pattern — so `github.com/acme-corp/*` matches
`github.com/acme-corp/svc` and also `github.com/acme-corp/svc/internal/db`, and a
bare `internal.example.com` matches everything under that host. This is exactly the
algorithm behind `go help module-private`, which is why using the library instead of
hand-rolling glob matching is non-negotiable — the prefix rule is subtle and easy to
get wrong. Validate the path first with `module.CheckPath`, which rejects malformed
module paths (bad characters, empty elements) the way the `go` command does.

Create `modrouter.go`:

```go
package modrouter

import (
	"fmt"

	"golang.org/x/mod/module"
)

// Route is the resolution decision for a module path.
type Route struct {
	Private       bool // matched a GOPRIVATE pattern
	UseProxy      bool // fetch through the public module proxy
	UseChecksumDB bool // verify against the public checksum database
}

// Resolve decides how the go command would fetch and verify a module at path,
// given a GOPRIVATE glob list. A path matching GOPRIVATE bypasses both the proxy
// and the checksum database (GOPRIVATE is the umbrella for GONOPROXY+GONOSUMDB);
// a non-matching public path uses both.
func Resolve(path, goprivate string) (Route, error) {
	if err := module.CheckPath(path); err != nil {
		return Route{}, fmt.Errorf("invalid module path %q: %w", path, err)
	}
	if module.MatchPrefixPatterns(goprivate, path) {
		return Route{Private: true, UseProxy: false, UseChecksumDB: false}, nil
	}
	return Route{Private: false, UseProxy: true, UseChecksumDB: true}, nil
}
```

### The runnable demo

Create `cmd/demo/main.go`:

```go
package main

import (
	"fmt"

	"example.com/modrouter"
)

func main() {
	const goprivate = "github.com/acme-corp/*,internal.example.com"
	paths := []string{
		"github.com/google/uuid",
		"github.com/acme-corp/billing",
		"github.com/acme-corp/billing/internal/db",
		"internal.example.com/platform/auth",
	}
	for _, p := range paths {
		r, err := modrouter.Resolve(p, goprivate)
		if err != nil {
			fmt.Printf("%s: %v\n", p, err)
			continue
		}
		fmt.Printf("%s: private=%v proxy=%v sumdb=%v\n", p, r.Private, r.UseProxy, r.UseChecksumDB)
	}
}
```

Run it:

```bash
go run ./cmd/demo
```

Expected output:

```
github.com/google/uuid: private=false proxy=true sumdb=true
github.com/acme-corp/billing: private=true proxy=false sumdb=false
github.com/acme-corp/billing/internal/db: private=true proxy=false sumdb=false
internal.example.com/platform/auth: private=true proxy=false sumdb=false
```

### Tests

Create `modrouter_test.go`:

```go
package modrouter

import (
	"fmt"
	"testing"
)

func TestResolve(t *testing.T) {
	t.Parallel()
	const goprivate = "github.com/acme-corp/*,internal.example.com"
	tests := []struct {
		name        string
		path        string
		wantPrivate bool
	}{
		{"public", "github.com/google/uuid", false},
		{"private-direct", "github.com/acme-corp/billing", true},
		{"private-deep-prefix", "github.com/acme-corp/billing/internal/db", true},
		{"private-host", "internal.example.com/platform/auth", true},
		{"public-lookalike", "github.com/acme-corp-evil/pkg", false},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			r, err := Resolve(tc.path, goprivate)
			if err != nil {
				t.Fatalf("Resolve(%q): %v", tc.path, err)
			}
			if r.Private != tc.wantPrivate {
				t.Errorf("Private = %v, want %v", r.Private, tc.wantPrivate)
			}
			// A private module bypasses both proxy and sumdb; a public one uses both.
			if r.UseProxy == tc.wantPrivate || r.UseChecksumDB == tc.wantPrivate {
				t.Errorf("routing inconsistent: private=%v proxy=%v sumdb=%v", r.Private, r.UseProxy, r.UseChecksumDB)
			}
		})
	}
}

func TestResolveInvalidPath(t *testing.T) {
	t.Parallel()
	if _, err := Resolve("github.com//bad", ""); err == nil {
		t.Fatal("want error for malformed module path, got nil")
	}
}

func ExampleResolve() {
	r, _ := Resolve("github.com/acme-corp/billing", "github.com/acme-corp/*")
	fmt.Printf("private=%v proxy=%v\n", r.Private, r.UseProxy)
	// Output: private=true proxy=false
}
```

## Review

The router is correct when a public path is verified through the proxy and checksum
database while any path matching a `GOPRIVATE` prefix bypasses both — and when the
prefix semantics hold: `github.com/acme-corp/*` covers arbitrarily deep subpaths but
does not match the lookalike host segment `github.com/acme-corp-evil`. That
lookalike case is the one that bites in production: a too-loose pattern silently
turns off verification for modules you did not mean to trust, and a too-tight one
leaks private paths to the public proxy. Delegating to `module.MatchPrefixPatterns`
rather than writing glob logic by hand is what keeps those semantics exactly aligned
with the `go` command. Run `go test -race`.

## Resources

- [Private modules](https://go.dev/ref/mod#private-modules) — GOPRIVATE, GONOPROXY, GONOSUMDB, GOINSECURE and how they interact.
- [`module.MatchPrefixPatterns`](https://pkg.go.dev/golang.org/x/mod/module#MatchPrefixPatterns) — the prefix-glob algorithm the go command uses.
- [`go help module-private`](https://go.dev/ref/mod#module-private) — the reference for private-module path matching.

---

Back to [00-concepts.md](00-concepts.md) | Prev: [07-gosum-integrity-verifier.md](07-gosum-integrity-verifier.md) | Next: [09-tool-directive-pinning.md](09-tool-directive-pinning.md)
