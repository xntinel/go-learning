# Exercise 8: Set Algebra over map[K]struct{} for Authorization Scopes

Authorization is set membership. A token carries a set of granted scopes; a route
requires a set of scopes; the request is allowed when the required set is a subset
of the granted set, and the missing permissions are the difference. This module
builds union, intersection, and difference over `map[string]struct{}` sets and
uses them to answer "does this token have all required scopes, and if not, which
are missing?"

This module is fully self-contained: its own `go mod init`, all code inline, its
own demo and tests. It gates alone.

## What you'll build

```text
scopeset/                   independent module: example.com/scopeset
  go.mod                    go 1.26
  scopeset.go               Set, NewSet, Union, Intersection, Difference, HasAllScopes
  cmd/
    demo/
      main.go               granted vs required scopes, missing set
  scopeset_test.go          union/intersection/difference incl. empty/disjoint, no mutation
```

Files: `scopeset.go`, `cmd/demo/main.go`, `scopeset_test.go`.
Implement: `Set = map[string]struct{}`; `Union`, `Intersection`, `Difference`; `HasAllScopes(granted, required) (bool, []string)`.
Test: union/intersection/difference including empty and disjoint sets; operands are never mutated; `HasAllScopes` returns the sorted missing set; nil sets behave as empty.
Verify: `go test -count=1 -race ./...`

Set up the module:

```bash
mkdir -p ~/go-exercises/scopeset/cmd/demo
cd ~/go-exercises/scopeset
go mod init example.com/scopeset
```

## Why struct{} sets, and how the algebra composes

When you only need to answer "is this element present," the idiomatic Go set is
`map[K]struct{}`. The value `struct{}{}` occupies zero bytes, so the set stores
only keys; membership is the comma-ok test `_, ok := set[k]`. This beats
`map[K]bool`, which spends a byte per entry and invites the ambiguity of a stored
`false` — is the key absent, or present-but-false? For a permission set there is no
such thing as present-but-false, so `struct{}` is the honest type.

The three operations compose on top of `maps.Clone` and iteration:

- `Union(a, b)` clones `a`, then adds every key of `b`. Every element of either set
  ends up present.
- `Intersection(a, b)` walks the smaller set and keeps only keys that are also in
  the other. Only elements in both survive.
- `Difference(a, b)` clones `a`, then deletes every key that is in `b` (a
  `maps.DeleteFunc` whose predicate is membership in `b`). What remains is "in `a`
  but not `b`."

The invariant across all three is that they never mutate their operands — each
returns a fresh set, so a granted-scopes set is never accidentally modified by an
authorization check. `Union` and `Difference` get this from cloning first;
`Intersection` builds a new set from scratch. The tests assert this by snapshotting
the inputs and comparing after.

`HasAllScopes(granted, required)` is the authorization decision: the missing scopes
are `Difference(required, granted)` — required minus granted. If that difference is
empty, the token has every required scope and the request is allowed; otherwise the
sorted missing set is exactly what you return in a 403 body so the caller knows what
to request. Sorting the missing set (`slices.Sorted(maps.Keys(...))`) keeps the
error deterministic instead of churning per request.

Create `scopeset.go`:

```go
package scopeset

import (
	"maps"
	"slices"
)

// Set is a membership set. The zero-width struct{} value stores no data.
type Set = map[string]struct{}

// NewSet builds a set from a list of elements.
func NewSet(elems ...string) Set {
	s := make(Set, len(elems))
	for _, e := range elems {
		s[e] = struct{}{}
	}
	return s
}

// Union returns a new set containing every element of a or b. Neither operand is
// mutated.
func Union(a, b Set) Set {
	out := maps.Clone(a)
	if out == nil {
		out = make(Set)
	}
	for k := range b {
		out[k] = struct{}{}
	}
	return out
}

// Intersection returns a new set of the elements present in both a and b.
func Intersection(a, b Set) Set {
	out := make(Set)
	small, large := a, b
	if len(b) < len(a) {
		small, large = b, a
	}
	for k := range small {
		if _, ok := large[k]; ok {
			out[k] = struct{}{}
		}
	}
	return out
}

// Difference returns a new set of the elements in a that are not in b.
func Difference(a, b Set) Set {
	out := maps.Clone(a)
	if out == nil {
		out = make(Set)
	}
	maps.DeleteFunc(out, func(k string, _ struct{}) bool {
		_, inB := b[k]
		return inB
	})
	return out
}

// HasAllScopes reports whether granted contains every element of required, and
// returns the sorted set of missing scopes (empty when allowed).
func HasAllScopes(granted, required Set) (ok bool, missing []string) {
	diff := Difference(required, granted)
	missing = slices.Sorted(maps.Keys(diff))
	return len(missing) == 0, missing
}
```

### The runnable demo

Create `cmd/demo/main.go`:

```go
package main

import (
	"fmt"

	"example.com/scopeset"
)

func main() {
	granted := scopeset.NewSet("read:orders", "read:users", "write:orders")
	required := scopeset.NewSet("read:orders", "write:orders", "admin:billing")

	ok, missing := scopeset.HasAllScopes(granted, required)
	fmt.Println("authorized:", ok)
	fmt.Println("missing:", missing)

	effective := scopeset.Intersection(granted, required)
	fmt.Println("effective count:", len(effective))
}
```

Run it:

```bash
go run ./cmd/demo
```

Expected output:

```
authorized: false
missing: [admin:billing]
effective count: 2
```

The token is missing `admin:billing`, so the request is denied and the 403 body can
name exactly what to request; the effective (granted ∩ required) scopes are the two
the token does satisfy.

### Tests

The tests table-drive union, intersection, and difference including the empty and
disjoint cases, assert the operands are not mutated, and pin `HasAllScopes` on both
the allowed and denied paths with a sorted missing set. A nil-operand case confirms
nil is treated as the empty set.

Create `scopeset_test.go`:

```go
package scopeset

import (
	"fmt"
	"maps"
	"slices"
	"testing"
)

func TestSetAlgebra(t *testing.T) {
	t.Parallel()

	a := NewSet("x", "y", "z")
	b := NewSet("y", "z", "w")

	if got, want := Union(a, b), NewSet("x", "y", "z", "w"); !maps.Equal(got, want) {
		t.Errorf("Union = %v, want %v", keys(got), keys(want))
	}
	if got, want := Intersection(a, b), NewSet("y", "z"); !maps.Equal(got, want) {
		t.Errorf("Intersection = %v, want %v", keys(got), keys(want))
	}
	if got, want := Difference(a, b), NewSet("x"); !maps.Equal(got, want) {
		t.Errorf("Difference = %v, want %v", keys(got), keys(want))
	}
}

func TestDisjointAndEmpty(t *testing.T) {
	t.Parallel()

	a := NewSet("a", "b")
	b := NewSet("c", "d")

	if got := Intersection(a, b); len(got) != 0 {
		t.Errorf("Intersection of disjoint sets = %v, want empty", keys(got))
	}
	if got, want := Union(a, NewSet()), a; !maps.Equal(got, want) {
		t.Errorf("Union with empty = %v, want %v", keys(got), keys(want))
	}
	if got := Difference(NewSet(), a); len(got) != 0 {
		t.Errorf("Difference of empty minus anything = %v, want empty", keys(got))
	}
}

func TestOperandsNotMutated(t *testing.T) {
	t.Parallel()

	a := NewSet("x", "y")
	b := NewSet("y", "z")
	aBefore := maps.Clone(a)
	bBefore := maps.Clone(b)

	_ = Union(a, b)
	_ = Intersection(a, b)
	_ = Difference(a, b)

	if !maps.Equal(a, aBefore) || !maps.Equal(b, bBefore) {
		t.Fatal("a set operation mutated one of its operands")
	}
}

func TestHasAllScopes(t *testing.T) {
	t.Parallel()

	granted := NewSet("read", "write")

	if ok, missing := HasAllScopes(granted, NewSet("read")); !ok || len(missing) != 0 {
		t.Errorf("subset: ok=%v missing=%v, want true, []", ok, missing)
	}
	ok, missing := HasAllScopes(granted, NewSet("read", "admin", "delete"))
	if ok {
		t.Error("should be denied when scopes are missing")
	}
	if !slices.Equal(missing, []string{"admin", "delete"}) {
		t.Errorf("missing = %v, want [admin delete] (sorted)", missing)
	}
}

func TestNilSetsAreEmpty(t *testing.T) {
	t.Parallel()

	var nilSet Set
	if got := Union(nilSet, NewSet("a")); !maps.Equal(got, NewSet("a")) {
		t.Errorf("Union(nil, {a}) = %v, want {a}", keys(got))
	}
	if got := Difference(NewSet("a"), nilSet); !maps.Equal(got, NewSet("a")) {
		t.Errorf("Difference({a}, nil) = %v, want {a}", keys(got))
	}
	if got := Intersection(nilSet, NewSet("a")); len(got) != 0 {
		t.Errorf("Intersection(nil, {a}) = %v, want empty", keys(got))
	}
}

func keys(s Set) []string { return slices.Sorted(maps.Keys(s)) }

func ExampleHasAllScopes() {
	ok, missing := HasAllScopes(NewSet("read"), NewSet("read", "write"))
	fmt.Println(ok, missing)
	// Output: false [write]
}
```

## Review

The algebra is correct when union, intersection, and difference match their
set-theory definitions on every case including empty and disjoint inputs, and — the
property that matters most for an auth path — when none of them mutate an operand,
which `TestOperandsNotMutated` guards. `HasAllScopes` reduces authorization to
`Difference(required, granted)`: empty means allowed, and the non-empty sorted
remainder is precisely the missing-permissions list a 403 should carry. Using
`map[K]struct{}` rather than `map[K]bool` keeps the sets honest (no present-but-false
ambiguity) and allocation-free on the value side. Run `go test -race`.

## Resources

- [maps package](https://pkg.go.dev/maps) — `Clone`, `DeleteFunc`, `Keys`, `Equal`.
- [slices package](https://pkg.go.dev/slices) — `Sorted`, `Equal`.
- [Go: the empty struct](https://dave.cheney.net/2014/03/25/the-empty-struct) — why `struct{}` is the set value type.

---

Back to [07-ttl-cache-sweep.md](07-ttl-cache-sweep.md) | Next: [09-grouped-index-pagination.md](09-grouped-index-pagination.md)
