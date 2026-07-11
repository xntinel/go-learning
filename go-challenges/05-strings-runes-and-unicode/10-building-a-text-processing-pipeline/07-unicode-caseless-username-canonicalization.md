# Exercise 7: Canonicalize Usernames with Full Unicode Case Folding

A signup uniqueness check has to decide whether two usernames are "the same"
ignoring case — and the three obvious tools give three different answers. This
module builds the canonicalizer a real signup path should use: NFC then full
Unicode case folding, producing a stable key you store and index, and it contrasts
that with `strings.EqualFold` and `strings.ToLower` so the trade-offs are explicit.

This module is fully self-contained. It begins with its own `go mod init`, defines
every type it needs, and ships its own demo and tests. Nothing here imports any
other exercise. It uses `golang.org/x/text`; the gate fetches it.

## What you'll build

```text
username/                 independent module: example.com/username
  go.mod                  go 1.26 (requires golang.org/x/text)
  username.go             Canonicalize(string) string; UniqueSet type
  cmd/
    demo/
      main.go             runnable demo comparing EqualFold, ToLower, and Fold
  username_test.go        EqualFold-vs-Fold table, uniqueness-set, idempotence tests
```

- Files: `username.go`, `cmd/demo/main.go`, `username_test.go`.
- Implement: `Canonicalize(string) string` (NFC then `cases.Fold`) and a `UniqueSet` that reports whether a username's canonical form is already taken.
- Test: a table over `İstanbul`/`ISTANBUL`, German `ß`/`SS`, and mixed-case ASCII showing `EqualFold`'s answers and `Canonicalize`'s; the folded key collides where it should in a uniqueness set; the NFC-then-Fold pipeline is idempotent.
- Verify: `go test -count=1 -race ./...`

Set up the module:

```bash
mkdir -p ~/go-exercises/username/cmd/demo
cd ~/go-exercises/username
go mod init example.com/username
go get golang.org/x/text/cases golang.org/x/text/unicode/norm
```

### Three tools, three answers

- `strings.EqualFold(a, b)` performs Unicode *simple* case folding and answers one
  pairwise question — equal ignoring case? It is convenient but it is a comparison,
  not a value: you cannot store it or index it, and enforcing uniqueness with it
  means table-scanning every existing row on each signup. Simple folding also does
  not handle one-to-many foldings, so `EqualFold("straße", "STRASSE")` is `false`.
- `strings.ToLower(s)` is Unicode-aware lowercasing. It produces a value, but
  lowercasing is not caseless matching: it does not fold `ß` to `ss`, and it is not
  the canonical form Unicode defines for case-insensitive comparison.
- `cases.Fold().String(s)` produces a full Unicode case-folded string — a stable
  canonical key. `cases.Fold().String("straße")` and `cases.Fold().String("STRASSE")`
  both yield `"strasse"`, so the pair collides in a uniqueness set where `EqualFold`
  said they differed.

The recommended canonical for a username is NFC first (so canonical-equivalent
spellings agree) then fold (so case and one-to-many foldings agree). Store that
canonical column and put a `UNIQUE` index on it; the database enforces uniqueness
with an index instead of a fold-at-query-time table scan. The composition is
idempotent, so re-canonicalizing on every write is safe.

Two honest rows keep the lesson truthful. German `ß`/`SS` is the case that
`cases.Fold` unifies but `EqualFold` does not — the reason to prefer a stored
folded key. Turkish `İstanbul`/`ISTANBUL` is the case that neither unifies:
`İ` (U+0130, dotted capital I) folds to `i` followed by a combining dot above
(U+0307), which is not the same as the plain `i` in `ISTANBUL`. Caseless matching
is defined by Unicode, not by intuition, and a canonicalizer that claimed to unify
`İstanbul` and `ISTANBUL` would be wrong.

Create `username.go`:

```go
package username

import (
	"golang.org/x/text/cases"
	"golang.org/x/text/unicode/norm"
)

// Canonicalize returns the caseless canonical key for a username: NFC first so
// canonical-equivalent spellings agree, then full Unicode case folding so case
// and one-to-many foldings agree. Store this key and index it.
func Canonicalize(s string) string {
	return cases.Fold().String(norm.NFC.String(s))
}

// UniqueSet tracks taken usernames by their canonical form.
type UniqueSet struct {
	taken map[string]struct{}
}

// NewUniqueSet returns an empty set.
func NewUniqueSet() *UniqueSet {
	return &UniqueSet{taken: make(map[string]struct{})}
}

// Add registers name and reports true if it was newly added, or false if a
// canonical-equivalent name was already present.
func (u *UniqueSet) Add(name string) bool {
	key := Canonicalize(name)
	if _, ok := u.taken[key]; ok {
		return false
	}
	u.taken[key] = struct{}{}
	return true
}
```

### The runnable demo

Create `cmd/demo/main.go`:

```go
package main

import (
	"fmt"
	"strings"

	"example.com/username"
)

func main() {
	pairs := [][2]string{
		{"straße", "STRASSE"},
		{"İstanbul", "ISTANBUL"},
		{"Admin", "admin"},
	}
	for _, p := range pairs {
		fmt.Printf("%-10q %-10q EqualFold=%-5v canonEqual=%v\n",
			p[0], p[1],
			strings.EqualFold(p[0], p[1]),
			username.Canonicalize(p[0]) == username.Canonicalize(p[1]))
	}

	set := username.NewUniqueSet()
	fmt.Printf("add straße: %v\n", set.Add("straße"))
	fmt.Printf("add STRASSE: %v\n", set.Add("STRASSE"))
}
```

Run it:

```bash
go run ./cmd/demo
```

Expected output:

```
"straße"   "STRASSE"  EqualFold=false canonEqual=true
"İstanbul" "ISTANBUL" EqualFold=false canonEqual=false
"Admin"    "admin"    EqualFold=true  canonEqual=true
add straße: true
add STRASSE: false
```

### Tests

The table pins the exact behavior of both `strings.EqualFold` and `Canonicalize`
for each pair, so the difference between "simple fold comparison" and "full-fold
canonical key" is asserted, not asserted-away. `TestUniqueSetCollides` proves the
folded key makes `straße`/`STRASSE` collide in the signup set. `TestIdempotent`
checks the NFC-then-Fold pipeline is a fixed point.

Create `username_test.go`:

```go
package username

import (
	"fmt"
	"strings"
	"testing"
)

func TestFoldVersusEqualFold(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name          string
		a, b          string
		wantEqualFold bool
		wantCanonEq   bool
	}{
		{"eszett vs SS", "straße", "STRASSE", false, true},
		{"turkish dotted I", "İstanbul", "ISTANBUL", false, false},
		{"ascii mixed case", "Admin", "admin", true, true},
		{"ascii upper", "HELLO", "hello", true, true},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			if got := strings.EqualFold(tt.a, tt.b); got != tt.wantEqualFold {
				t.Fatalf("EqualFold(%q,%q) = %v, want %v", tt.a, tt.b, got, tt.wantEqualFold)
			}
			got := Canonicalize(tt.a) == Canonicalize(tt.b)
			if got != tt.wantCanonEq {
				t.Fatalf("canonEqual(%q,%q) = %v, want %v", tt.a, tt.b, got, tt.wantCanonEq)
			}
		})
	}
}

func TestUniqueSetCollides(t *testing.T) {
	t.Parallel()

	set := NewUniqueSet()
	if !set.Add("straße") {
		t.Fatal("first Add should succeed")
	}
	if set.Add("STRASSE") {
		t.Fatal("STRASSE should collide with straße under full case folding")
	}
	// A genuinely different name is accepted.
	if !set.Add("İstanbul") {
		t.Fatal("İstanbul should be a distinct canonical key")
	}
}

func TestIdempotent(t *testing.T) {
	t.Parallel()

	for _, s := range []string{"straße", "İstanbul", "Admin", "café", ""} {
		once := Canonicalize(s)
		twice := Canonicalize(once)
		if once != twice {
			t.Fatalf("Canonicalize not idempotent for %q: %q vs %q", s, once, twice)
		}
	}
}

func ExampleCanonicalize() {
	fmt.Println(Canonicalize("straße") == Canonicalize("STRASSE"))
	// Output: true
}
```

## Review

The canonicalizer is correct when `Canonicalize` = fold(NFC(s)) produces one
stable key that collides exactly when two names are caseless-equal in the full
Unicode sense — so `straße` and `STRASSE` unify (which `EqualFold` alone does not)
while `İstanbul` and `ISTANBUL` correctly do not. The mistake to avoid is reaching
for `EqualFold` or `ToLower` as if either were a canonical form: `EqualFold` is a
pairwise comparison you cannot store or index, and `ToLower` does not fold `ß`.
Store the folded canonical column, index it, and let the database enforce
uniqueness. Because the pipeline is idempotent, re-canonicalizing on every write
never drifts. Run `go test -race` to confirm the set's map access is clean.

## Resources

- [golang.org/x/text/cases: Fold, Caser.String](https://pkg.go.dev/golang.org/x/text/cases) — full Unicode case folding.
- [strings.EqualFold](https://pkg.go.dev/strings#EqualFold) — simple-fold pairwise comparison, and its limits.
- [Unicode case folding (FAQ)](https://www.unicode.org/faq/casemap_charprop.html) — what caseless matching means, including the Turkish-I and eszett cases.
- [golang.org/x/text/unicode/norm](https://pkg.go.dev/golang.org/x/text/unicode/norm) — NFC, applied before folding.

---

Back to [00-concepts.md](00-concepts.md) | Prev: [06-diacritic-folding-slug-generator.md](06-diacritic-folding-slug-generator.md) | Next: [08-rune-safe-length-truncation.md](08-rune-safe-length-truncation.md)
