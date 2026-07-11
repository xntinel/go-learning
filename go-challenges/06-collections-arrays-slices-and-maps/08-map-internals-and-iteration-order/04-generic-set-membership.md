# Exercise 4: Generic Set[T] over map[T]struct{} for Allowlists and ID Dedup

Deduping a batch of user IDs before a bulk write, or checking a requested scope against
an allowlist, is a set operation — and Go has no built-in set type, so you build one on
a map. This module builds a generic `Set[T comparable]` backed by `map[T]struct{}`,
with the algebra (union, intersection, difference) and a deterministic `Sorted` for
output.

This module is fully self-contained: its own `go mod init`, all code inline, its own
demo and tests. Nothing here imports any other exercise.

## What you'll build

```text
set/                       independent module: example.com/set
  go.mod                   go 1.26
  set.go                   Set[T]; New, Add, Has, Delete, Len, Union, Intersect,
                           Difference, Sorted
  cmd/
    demo/
      main.go              dedups a batch and checks an allowlist
  set_test.go              table tests per operation, string and int instantiations
```

- Files: `set.go`, `cmd/demo/main.go`, `set_test.go`.
- Implement: `Set[T comparable]` over `map[T]struct{}` with `Add`, `Has`, `Delete`,
  `Len`, `Union`, `Intersect`, `Difference`, and `Sorted` (requires `cmp.Ordered`).
- Test: each operation with expected sorted slices; idempotent `Add`; `Has` on a nil
  set returns false; commutativity where it applies; both string and int instantiations.
- Verify: `go test -count=1 -race ./...`

Set up the module:

```bash
mkdir -p ~/go-exercises/set/cmd/demo
cd ~/go-exercises/set
go mod init example.com/set
```

### Why struct{} and not bool

A set only needs to record *presence*, so the value type is irrelevant — but it is not
free. `map[T]bool` stores a byte per entry and invites the bug of testing `m[k]` (which
returns `false` for both "absent" and "present-but-false") instead of the comma-ok
form. `map[T]struct{}` stores the *empty struct*, which occupies zero bytes: every
`struct{}{}` value shares one address, so a million-element set pays for keys only, not
values. Presence is then unambiguous — you always test with comma-ok (`_, ok :=
m[k]`), because a `struct{}` carries no truthiness to be tempted by. `struct{}{}` is
the (only) value of the empty struct type; `Add` writes it as the placeholder.

`T` is constrained to `comparable`, which is exactly the set of types usable as map
keys. A subtle trap the concepts file names: `comparable` includes interface types, but
an interface holding a *dynamic* type that is not comparable (a `[]byte` boxed in an
`any`) satisfies the constraint at compile time and then panics at runtime when used as
a key. For a set of IDs you instantiate `Set[string]` or `Set[int]`, which are
concretely comparable and safe.

`Sorted` is where determinism comes back. A set is inherently unordered (it is a map),
so any method that emits its contents must sort. `Sorted` needs ordered elements, so it
is a *free function* constrained to `cmp.Ordered` rather than a method — a `Set[T
comparable]` may hold a non-ordered comparable type (a struct key), which cannot be
sorted, so the ordering capability is opt-in at the call site: `set.Sorted(s)`. It
returns `slices.Sorted(maps.Keys(s.m))`.

Create `set.go`:

```go
package set

import (
	"cmp"
	"maps"
	"slices"
)

// Set is an unordered collection of distinct comparable values, backed by a map
// with zero-size values so only the keys cost memory.
type Set[T comparable] struct {
	m map[T]struct{}
}

// New returns an empty set containing the given elements.
func New[T comparable](elems ...T) *Set[T] {
	s := &Set[T]{m: make(map[T]struct{}, len(elems))}
	for _, e := range elems {
		s.m[e] = struct{}{}
	}
	return s
}

// Add inserts e. Adding an element already present is a no-op (idempotent).
func (s *Set[T]) Add(e T) { s.m[e] = struct{}{} }

// Has reports whether e is a member. It is nil-safe: the zero Set has a nil map,
// and reading a nil map returns (_, false).
func (s *Set[T]) Has(e T) bool {
	_, ok := s.m[e]
	return ok
}

// Delete removes e if present; removing an absent element is a no-op.
func (s *Set[T]) Delete(e T) { delete(s.m, e) }

// Len returns the number of members.
func (s *Set[T]) Len() int { return len(s.m) }

// Union returns a new set with every element in s or other.
func (s *Set[T]) Union(other *Set[T]) *Set[T] {
	out := New[T]()
	maps.Copy(out.m, s.m)
	maps.Copy(out.m, other.m)
	return out
}

// Intersect returns a new set with the elements in both s and other. It ranges
// the smaller operand for efficiency.
func (s *Set[T]) Intersect(other *Set[T]) *Set[T] {
	small, large := s, other
	if large.Len() < small.Len() {
		small, large = large, small
	}
	out := New[T]()
	for e := range small.m {
		if large.Has(e) {
			out.Add(e)
		}
	}
	return out
}

// Difference returns a new set with the elements in s but not in other.
func (s *Set[T]) Difference(other *Set[T]) *Set[T] {
	out := New[T]()
	for e := range s.m {
		if !other.Has(e) {
			out.Add(e)
		}
	}
	return out
}

// Sorted returns the members of s in ascending order. It is a free function, not
// a method, because a Set[T comparable] may hold a non-ordered element type;
// only when T is cmp.Ordered can it be sorted.
func Sorted[T cmp.Ordered](s *Set[T]) []T {
	return slices.Sorted(maps.Keys(s.m))
}
```

### The runnable demo

Create `cmd/demo/main.go`:

```go
package main

import (
	"fmt"

	"example.com/set"
)

func main() {
	// Dedup a batch of user IDs before a bulk write.
	batch := []int{7, 3, 7, 1, 3, 9, 1}
	unique := set.New(batch...)
	fmt.Println("unique ids:", set.Sorted(unique))

	// Check requested scopes against an allowlist.
	allow := set.New("read", "write")
	for _, scope := range []string{"read", "delete"} {
		fmt.Printf("scope %-6s allowed=%v\n", scope, allow.Has(scope))
	}
}
```

Run it:

```bash
go run ./cmd/demo
```

Expected output:

```
unique ids: [1 3 7 9]
scope read   allowed=true
scope delete allowed=false
```

### Tests

Each operation gets a table test asserting the sorted result, so the assertions are
deterministic despite the underlying map. `Add` idempotence, `Has` on a nil set, and
both string and int instantiations are covered.

Create `set_test.go`:

```go
package set

import (
	"slices"
	"testing"
)

func TestAddIsIdempotent(t *testing.T) {
	t.Parallel()

	s := New[string]()
	s.Add("a")
	s.Add("a")
	s.Add("a")
	if s.Len() != 1 {
		t.Fatalf("Len = %d after adding 'a' thrice, want 1", s.Len())
	}
}

func TestHasOnNilSetIsFalse(t *testing.T) {
	t.Parallel()

	var zero Set[int] // nil backing map
	if zero.Has(5) {
		t.Fatal("Has on a zero Set should be false, not panic")
	}
	if zero.Len() != 0 {
		t.Fatalf("Len = %d on zero Set, want 0", zero.Len())
	}
}

func TestSetAlgebra(t *testing.T) {
	t.Parallel()

	a := New(1, 2, 3, 4)
	b := New(3, 4, 5, 6)

	tests := []struct {
		name string
		got  []int
		want []int
	}{
		{"union", Sorted(a.Union(b)), []int{1, 2, 3, 4, 5, 6}},
		{"union-commutes", Sorted(b.Union(a)), []int{1, 2, 3, 4, 5, 6}},
		{"intersect", Sorted(a.Intersect(b)), []int{3, 4}},
		{"intersect-commutes", Sorted(b.Intersect(a)), []int{3, 4}},
		{"difference-a-b", Sorted(a.Difference(b)), []int{1, 2}},
		{"difference-b-a", Sorted(b.Difference(a)), []int{5, 6}},
	}
	for _, tc := range tests {
		if !slices.Equal(tc.got, tc.want) {
			t.Errorf("%s = %v, want %v", tc.name, tc.got, tc.want)
		}
	}
}

func TestDelete(t *testing.T) {
	t.Parallel()

	s := New("x", "y", "z")
	s.Delete("y")
	s.Delete("absent") // no-op
	if s.Has("y") {
		t.Fatal("y still present after Delete")
	}
	if got, want := Sorted(s), []string{"x", "z"}; !slices.Equal(got, want) {
		t.Fatalf("Sorted = %v, want %v", got, want)
	}
}

func TestStringInstantiation(t *testing.T) {
	t.Parallel()

	s := New("banana", "apple", "cherry")
	if got, want := Sorted(s), []string{"apple", "banana", "cherry"}; !slices.Equal(got, want) {
		t.Fatalf("Sorted = %v, want %v", got, want)
	}
}

func TestSortedIsDeterministic(t *testing.T) {
	t.Parallel()

	s := New(5, 1, 4, 2, 3)
	if first, second := Sorted(s), Sorted(s); !slices.Equal(first, second) {
		t.Fatalf("Sorted not deterministic: %v vs %v", first, second)
	}
}
```

## Review

The set is correct when membership is exact and emission is deterministic. The value
type `struct{}` makes presence unambiguous (there is no value to misread) and costs no
memory; `Has` uses comma-ok, never a truthiness test. `Union`/`Intersect`/`Difference`
each return a new set and leave the operands untouched, so they compose without
surprise; the commutativity subtests pin that union and intersection do not depend on
operand order while difference correctly does. `Sorted` is the only deterministic view
of an inherently unordered structure — every test asserts through it rather than
ranging the map. Run `go test -race`.

## Resources

- [The empty struct](https://dave.cheney.net/2014/03/25/the-empty-struct) — why `struct{}` is the right set value.
- [`slices.Sorted`](https://pkg.go.dev/slices#Sorted) — collect an iterator into a sorted slice.
- [Go Specification: Type constraints](https://go.dev/ref/spec#Type_constraints) — `comparable` and `cmp.Ordered`.

---

Back to [00-concepts.md](00-concepts.md) | Next: [05-config-drift-diff.md](05-config-drift-diff.md)
