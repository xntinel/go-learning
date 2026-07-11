# Exercise 3: Generic Set[T] for a dedup / membership pipeline

Two jobs show up constantly in backend code: drop already-seen items (dedup a
stream of message IDs so a retried delivery is processed once) and test membership
(is this scope in the token's allowed set?). Both are a set, and Go does not ship
one — you build it on `map[T]struct{}`. This module builds a generic, reusable
`Set[T]` and, along the way, nails the two details people get wrong: using
`struct{}` for a zero-byte value, and never trusting map iteration order.

This module is fully self-contained: its own `go mod init`, all code inline, its
own demo and tests.

## What you'll build

```text
set/                       independent module: example.com/set
  go.mod
  set.go                   type Set[T]; Add, Has, Delete, Len, Union, Intersect, Ordered
  cmd/
    demo/
      main.go              dedup a message stream + scope membership check
  set_test.go              idempotence, Union/Intersect via Ordered, absent-vs-zero
```

- Files: `set.go`, `cmd/demo/main.go`, `set_test.go`.
- Implement: `Set[T cmp.Ordered]` backed by `map[T]struct{}` with `Add`, `Has`, `Delete`, `Len`, `Union`, `Intersect`, and `Ordered() []T` (sorted, deterministic).
- Test: `Add`/`Delete` idempotence, `Union`/`Intersect` compared via `Ordered()` (never raw range order), and `Has` distinguishing an absent key from the zero value.
- Verify: `go test -count=1 -race ./...`

Set up the module:

```bash
mkdir -p ~/go-exercises/set/cmd/demo
cd ~/go-exercises/set
go mod init example.com/set
```

### `map[T]struct{}`, and why the constraint is `cmp.Ordered` not `comparable`

The value type is `struct{}` — the empty struct — because a set stores no value,
only membership. The empty struct occupies zero bytes, so the value half of every
entry is free; and because there is nothing to inspect, membership can only be
tested with comma-ok on the key (`_, ok := m[v]`), which sidesteps the
false-vs-absent ambiguity that `map[T]bool` invites. That comma-ok is exactly
what `Has` does.

Membership itself needs only `comparable` (map keys must be comparable). This set
constrains one notch tighter, to `cmp.Ordered`, for a single reason: `Ordered()`
must produce a deterministic, sorted slice, and you cannot sort a merely-
comparable type. `cmp.Ordered` is a subset of `comparable` (every ordered type is
comparable), so the map still works; the extra constraint buys deterministic
output. This is the deliberate answer to the map-order trap: a set built on a map
has *no* stable iteration order, so any time you need to serialize it, log it, or
assert on it in a test, you go through `Ordered()` and never through a raw
`range`.

Create `set.go`:

```go
package set

import (
	"cmp"
	"maps"
	"slices"
)

// Set is a generic set backed by a map with zero-byte values. T is constrained
// to cmp.Ordered (not just comparable) so Ordered can return a sorted slice for
// deterministic output.
type Set[T cmp.Ordered] struct {
	m map[T]struct{}
}

// New returns a set containing the given items.
func New[T cmp.Ordered](items ...T) *Set[T] {
	s := &Set[T]{m: make(map[T]struct{}, len(items))}
	for _, it := range items {
		s.m[it] = struct{}{}
	}
	return s
}

// Add inserts v. Adding an existing element is a no-op (idempotent).
func (s *Set[T]) Add(v T) { s.m[v] = struct{}{} }

// Has reports whether v is present, using comma-ok so an absent key is
// distinguished from a stored zero value.
func (s *Set[T]) Has(v T) bool {
	_, ok := s.m[v]
	return ok
}

// Delete removes v. Deleting an absent element is a no-op.
func (s *Set[T]) Delete(v T) { delete(s.m, v) }

// Len reports the number of elements.
func (s *Set[T]) Len() int { return len(s.m) }

// Union returns a new set containing every element of s or other.
func (s *Set[T]) Union(other *Set[T]) *Set[T] {
	out := New[T]()
	for v := range s.m {
		out.m[v] = struct{}{}
	}
	for v := range other.m {
		out.m[v] = struct{}{}
	}
	return out
}

// Intersect returns a new set of the elements in both s and other. It ranges the
// smaller set and probes the larger, so the cost is O(min(len)).
func (s *Set[T]) Intersect(other *Set[T]) *Set[T] {
	small, large := s, other
	if large.Len() < small.Len() {
		small, large = large, small
	}
	out := New[T]()
	for v := range small.m {
		if large.Has(v) {
			out.m[v] = struct{}{}
		}
	}
	return out
}

// Ordered returns the elements sorted ascending. Use this for any deterministic
// output — never range the set directly, because map iteration order is random.
func (s *Set[T]) Ordered() []T {
	return slices.Sorted(maps.Keys(s.m))
}
```

### The runnable demo

The demo runs both use cases: dedup a stream of message IDs (a duplicate delivery
is dropped), and test scope membership for an authorization check.

Create `cmd/demo/main.go`:

```go
package main

import (
	"fmt"

	"example.com/set"
)

func main() {
	// Dedup stage: a redelivered message ("m-2") must be processed once.
	seen := set.New[string]()
	var processed []string
	for _, id := range []string{"m-1", "m-2", "m-2", "m-3", "m-1"} {
		if seen.Has(id) {
			continue
		}
		seen.Add(id)
		processed = append(processed, id)
	}
	fmt.Printf("processed unique: %v\n", processed)

	// Membership: does the token carry the scope we require?
	scopes := set.New("read:orders", "write:orders")
	fmt.Printf("can write orders: %v\n", scopes.Has("write:orders"))
	fmt.Printf("can delete orders: %v\n", scopes.Has("delete:orders"))
}
```

Run it:

```bash
go run ./cmd/demo
```

Expected output:

```
processed unique: [m-1 m-2 m-3]
can write orders: true
can delete orders: false
```

### Tests

The tests compare sets through `Ordered()` so they never depend on map order. The
absent-vs-zero test is the one that catches the classic bug: for an `int` set that
does not contain `0`, `Has(0)` must be false even though `0` is the zero value.

Create `set_test.go`:

```go
package set

import (
	"slices"
	"testing"
)

func TestAddDeleteIdempotent(t *testing.T) {
	t.Parallel()

	s := New[string]()
	s.Add("a")
	s.Add("a") // duplicate Add is a no-op
	if s.Len() != 1 {
		t.Fatalf("Len after duplicate Add = %d, want 1", s.Len())
	}
	s.Delete("a")
	s.Delete("a") // duplicate Delete is a no-op
	if s.Len() != 0 {
		t.Fatalf("Len after duplicate Delete = %d, want 0", s.Len())
	}
}

func TestUnionIntersect(t *testing.T) {
	t.Parallel()

	a := New(1, 2, 3)
	b := New(2, 3, 4)

	if got, want := a.Union(b).Ordered(), []int{1, 2, 3, 4}; !slices.Equal(got, want) {
		t.Fatalf("Union = %v, want %v", got, want)
	}
	if got, want := a.Intersect(b).Ordered(), []int{2, 3}; !slices.Equal(got, want) {
		t.Fatalf("Intersect = %v, want %v", got, want)
	}
}

func TestHasDistinguishesAbsentFromZero(t *testing.T) {
	t.Parallel()

	s := New(1, 2) // does not contain the zero value 0
	if s.Has(0) {
		t.Fatal("Has(0) = true, want false: absent key must not read as the zero value")
	}
	s.Add(0)
	if !s.Has(0) {
		t.Fatal("Has(0) = false after Add(0), want true")
	}
}

func TestOrderedIsDeterministic(t *testing.T) {
	t.Parallel()

	s := New("banana", "apple", "cherry")
	want := []string{"apple", "banana", "cherry"}
	// Two calls must agree and be sorted, regardless of internal map order.
	for range 5 {
		if got := s.Ordered(); !slices.Equal(got, want) {
			t.Fatalf("Ordered = %v, want %v", got, want)
		}
	}
}
```

## Review

The set is correct when `Add`/`Delete` are idempotent, `Has` is comma-ok (so an
absent key never reads as the zero value), and every set-valued result is compared
through `Ordered()` rather than a raw `range`. The two mistakes this module
inoculates against are storing `map[T]bool` (a wasted byte and a false-vs-absent
ambiguity — `struct{}` avoids both) and letting map iteration order leak into
output or a test (the `TestOrderedIsDeterministic` loop exists precisely to prove
`Ordered()` is stable while the map is not). Run `go test -count=1 -race ./...`.

## Resources

- [`cmp` package](https://pkg.go.dev/cmp) — the `cmp.Ordered` constraint.
- [`maps` package](https://pkg.go.dev/maps) — `maps.Keys` returning an `iter.Seq`.
- [`slices` package](https://pkg.go.dev/slices) — `slices.Sorted` and `slices.Equal`.
- [Go blog: range over function types](https://go.dev/blog/range-functions) — how `maps.Keys` iterators compose with `slices.Sorted`.

---

Back to [00-concepts.md](00-concepts.md) | Prev: [02-cms-probabilistic-tests.md](02-cms-probabilistic-tests.md) | Next: [04-lru-cache.md](04-lru-cache.md)
