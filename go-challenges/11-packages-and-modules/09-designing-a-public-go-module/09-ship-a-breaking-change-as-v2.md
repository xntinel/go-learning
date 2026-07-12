# Exercise 9: Pay for a real breaking change with a semantic-import-versioned /v2

Some changes cannot be made compatibly. Redesigning `Truncate` to be rune-safe and
to return `(string, bool)` instead of `(string, error)` breaks every caller — there
is no additive path. When a break is genuinely worth it, you pay for it with a
major version: a `v2` module at a new import path, so v1 and v2 are distinct
packages a single program can import side by side and consumers migrate on their
own schedule. This exercise builds that coexistence and proves the two majors
resolve independently.

This module is fully self-contained: its own `go mod init`, the v1 `Truncate`, a
`v2/` package with the redesigned `Truncate`, a demo importing both, and tests
exercising the new contract and the side-by-side build.

## What you'll build

```text
publicstr/                       independent module: example.com/publicstr
  go.mod                         go 1.26
  truncate.go                    v1: Truncate(s, n) (string, error), byte-bounded
  v2/
    truncate.go                  v2: Truncate(s, maxRunes) (string, bool), rune-safe
    truncate_test.go             v2 contract tests
  cmd/
    demo/
      main.go                    imports v1 and v2 together
  coexist_test.go                external test importing both majors at once
```

- Files: `truncate.go`, `v2/truncate.go`, `v2/truncate_test.go`, `cmd/demo/main.go`, `coexist_test.go`.
- Implement: the v1 `Truncate` returning `(string, error)`; the v2 `Truncate` returning `(string, bool)`, rune-safe, ellipsis counting toward the limit.
- Test: v2's new contract (rune-safe, `(string, bool)`); a consumer test importing both `example.com/publicstr` and `example.com/publicstr/v2` and using each, proving they are distinct, independently-resolvable packages.
- Verify: `go test -count=1 -race ./...`

### How semantic import versioning encodes the major version

Go encodes the major version in the *import path*. Major versions 2 and above live
in a module whose module path ends in `/vN`: a `v2/` directory containing its own
`go.mod` whose module line reads `example.com/publicstr/v2` and whose `go` line is
`go 1.26`. That path — `example.com/publicstr/v2` — is a *different package* from
`example.com/publicstr`, so a single program can import both at once, alias them
(`v1`, `v2`), and call each. The v2 package is still named `publicstr` by
convention (the last non-version element of the path names the package), which is
why the import needs an alias to disambiguate.

The payoff is incremental migration. Because the two majors are distinct import
paths, a large consumer can adopt v2 one call site at a time, running both during
the transition, rather than cutting over on a flag day. That is what makes a major
bump survivable at organizational scale: `go get example.com/publicstr/v2` adds a
new dependency; it does not remove the old one. When you redesign `Truncate`'s
return tuple — an unambiguously breaking change — this is the mechanism that lets
you ship it without breaking anyone who is not ready.

In a published repository, the `v2/` directory carries its **own** `go.mod`
declaring the module path `example.com/publicstr/v2` (and `go 1.26`), which is
what makes the module system resolve it as a separate, independently-versioned
module. To keep this exercise a single buildable unit, both majors live under one
`go.mod` here; the import paths and the package APIs are exactly what you would
ship, and the only production addition is that second `go.mod`. You create it with
one command run inside the `v2/` directory — `go mod init example.com/publicstr/v2`
— which writes a `go.mod` whose module line is `example.com/publicstr/v2` and whose
language line is `go 1.26`.

The v1 package keeps the original byte-bounded, error-returning contract. Create
`truncate.go`:

```go
package publicstr

import (
	"errors"
	"strings"
)

// ErrEmpty is returned by Truncate on empty input.
var ErrEmpty = errors.New("publicstr: empty string")

// Truncate returns s bounded to at most n bytes; when longer, "..." is appended
// and counts toward the limit. This is the v1 contract: byte-bounded and
// error-returning. The v2 package redesigns it to be rune-safe.
func Truncate(s string, n int) (string, error) {
	if s == "" {
		return "", ErrEmpty
	}
	if n <= 3 {
		return strings.Repeat(".", n), nil
	}
	if len(s) <= n {
		return s, nil
	}
	return s[:n-3] + "...", nil
}
```

Create the redesigned `v2/truncate.go`. Note the package is still named
`publicstr`, and the new signature returns `(string, bool)`:

```go
// v2/truncate.go
package publicstr

// Truncate returns s bounded to at most maxRunes runes and reports whether it
// truncated. It is rune-safe: multi-byte characters are never split, and the
// appended ellipsis rune counts toward the limit. This is the v2 contract,
// replacing v1's byte-bounded (string, error) return.
func Truncate(s string, maxRunes int) (string, bool) {
	runes := []rune(s)
	if len(runes) <= maxRunes {
		return s, false
	}
	if maxRunes <= 0 {
		return "", true
	}
	if maxRunes == 1 {
		return "…", true
	}
	return string(runes[:maxRunes-1]) + "…", true
}
```

### The runnable demo importing both majors

Because the two majors are distinct import paths, one program imports and uses both
under aliases.

Create `cmd/demo/main.go`:

```go
package main

import (
	"fmt"

	v1 "example.com/publicstr"
	v2 "example.com/publicstr/v2"
)

func main() {
	// v1: byte-bounded, error-returning.
	oldOut, _ := v1.Truncate("hello world", 5)
	fmt.Printf("v1: %q\n", oldOut)

	// v2: rune-safe, (string, bool). Multi-byte input is never split.
	newOut, truncated := v2.Truncate("héllo wörld", 8)
	fmt.Printf("v2: %q truncated=%v\n", newOut, truncated)

	whole, truncated2 := v2.Truncate("short", 10)
	fmt.Printf("v2: %q truncated=%v\n", whole, truncated2)
}
```

Run it:

```bash
go run ./cmd/demo
```

Expected output:

```
v1: "he..."
v2: "héllo w…" truncated=true
v2: "short" truncated=false
```

### Tests

`v2/truncate_test.go` pins the new contract; `coexist_test.go` is an external test
(its own package) that imports both majors and uses each, proving they are separate,
independently-resolvable packages rather than one overwriting the other.

Create `v2/truncate_test.go`:

```go
// v2/truncate_test.go
package publicstr

import (
	"testing"
	"unicode/utf8"
)

func TestV2TruncateRuneSafe(t *testing.T) {
	t.Parallel()
	cases := []struct {
		in        string
		max       int
		want      string
		truncated bool
	}{
		{"short", 10, "short", false},
		{"héllo wörld", 8, "héllo w…", true},
		{"exactly", 7, "exactly", false},
		{"語語語語", 2, "語…", true},
	}
	for _, tc := range cases {
		got, tr := Truncate(tc.in, tc.max)
		if got != tc.want || tr != tc.truncated {
			t.Errorf("Truncate(%q,%d) = %q,%v; want %q,%v",
				tc.in, tc.max, got, tr, tc.want, tc.truncated)
		}
		if !utf8.ValidString(got) {
			t.Errorf("Truncate(%q,%d) produced invalid UTF-8", tc.in, tc.max)
		}
	}
}
```

Create `coexist_test.go`:

```go
package publicstr_test

import (
	"testing"

	v1 "example.com/publicstr"
	v2 "example.com/publicstr/v2"
)

// TestMajorsCoexist proves v1 and v2 are distinct packages that resolve
// independently: each has its own Truncate with its own signature, imported
// side by side in one file.
func TestMajorsCoexist(t *testing.T) {
	t.Parallel()

	// v1 returns (string, error).
	oldOut, err := v1.Truncate("hello world", 5)
	if err != nil {
		t.Fatalf("v1.Truncate error: %v", err)
	}
	if oldOut != "he..." {
		t.Fatalf("v1.Truncate = %q, want he...", oldOut)
	}

	// v2 returns (string, bool) and is rune-safe.
	newOut, truncated := v2.Truncate("héllo wörld", 8)
	if !truncated || newOut != "héllo w…" {
		t.Fatalf("v2.Truncate = %q,%v; want héllo w…,true", newOut, truncated)
	}
}
```

## Review

The major bump is correct when v1 and v2 coexist as distinct import paths, each
with its own `Truncate` contract, and a single program imports both. The design
lesson is *when* to pay this cost: only for a change with no additive path — here,
retyping the return tuple from `(string, error)` to `(string, bool)` and switching
from bytes to runes, which no option or sibling function could express compatibly.
The mechanism lesson is *how*: the major version lives in the import path
(`.../v2`), backed in production by a second `go.mod` declaring the `/v2` module,
so consumers `go get` v2 without losing v1 and migrate incrementally. The mistake
this exercise trains against is tagging `v2.0.0` at the *same* import path with a
changed API — that makes it impossible to depend on both and turns migration into a
flag-day. Note the v2 package is still named `publicstr`; the alias in the import
disambiguates the two.

## Resources

- [Go Modules Reference: versions and semantic import versioning](https://go.dev/ref/mod#versions) — how `/vN` encodes the major version.
- [Go Blog: Go Modules: v2 and Beyond](https://go.dev/blog/v2-go-modules) — the full workflow for publishing a v2.
- [Go Blog: Keeping your modules compatible](https://go.dev/blog/module-compatibility) — when a change requires a major bump.

---

Back to [00-concepts.md](00-concepts.md) | Prev: [08-gate-releases-with-an-api-compat-check.md](08-gate-releases-with-an-api-compat-check.md) | Next: [../10-monorepo-module-strategy/00-concepts.md](../10-monorepo-module-strategy/00-concepts.md)
