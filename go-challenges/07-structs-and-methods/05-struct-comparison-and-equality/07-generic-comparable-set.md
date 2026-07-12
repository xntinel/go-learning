# Exercise 7: Message-ID Dedup: A Generic Set[T comparable]

An at-least-once queue consumer will hand you the same message twice, and dropping
the duplicate is your job. The clean primitive is a generic set backed by
`map[T]struct{}` — and the `comparable` constraint is precisely what lets `T` be a
map key. This exercise builds that `Set[T comparable]` with `Add`/`Contains`/`Len`/`Equal`,
gives it deterministic sorted output for golden assertions, and instantiates it with
a struct key to prove the constraint carries all the way through.

This module is fully self-contained: its own `go mod init`, demo, and tests.

## What you'll build

```text
idset/                      independent module: example.com/idset
  go.mod                    go 1.26
  idset.go                  Set[T comparable]; Add, Contains, Len, Equal (maps.Equal), Sorted
  cmd/
    demo/
      main.go               runnable demo: dedup a batch of message IDs
  idset_test.go             dedup; order-insensitive Equal; deterministic Sorted; struct key
```

- Files: `idset.go`, `cmd/demo/main.go`, `idset_test.go`.
- Implement: `Set[T comparable]` over `map[T]struct{}` with `Add`/`Contains`/`Len`/`Equal` (via `maps.Equal`) and a `Sorted[T cmp.Ordered]` helper (`slices.Sorted(maps.Keys(...))`).
- Test: duplicates keep `Len` stable; `Equal` is order-insensitive; `Sorted` gives a golden order; instantiate with a comparable struct key.
- Verify: `go test -count=1 -race ./...`

Set up the module:

```bash
mkdir -p go-solutions/07-structs-and-methods/05-struct-comparison-and-equality/07-generic-comparable-set/cmd/demo
cd go-solutions/07-structs-and-methods/05-struct-comparison-and-equality/07-generic-comparable-set
```

### Why `comparable` is the constraint that makes this work

`Set` stores its members as keys of a `map[T]struct{}`. A map key must be
comparable, so the type parameter must promise that `T` is comparable — which is
exactly what the predeclared constraint `comparable` means. Writing `Set[T comparable]`
is not decoration; it is the compile-time contract that lets the body use `T` as a
map key at all. It also gives callers a good error at the *instantiation* site:
`Set[[]byte]` is rejected because a slice is not comparable, before any method runs.
And it carries through to any comparable type, including a struct: a
`Set[struct{Topic string; Partition int}]` works because a struct of comparable
fields is comparable. `TestStructKey` exercises exactly that.

`Add` returns whether the value was *newly* added, which is the dedup signal a
consumer wants: `true` means "first delivery, process it", `false` means "duplicate,
drop it". `Equal` uses `maps.Equal(s.m, other.m)`, comparing two `map[T]struct{}`
type-safely — two sets with the same members are equal regardless of the order they
were built in, because a map has no order. That order-independence is a feature, and
`TestEqualOrderInsensitive` pins it.

Iterating a map is deliberately randomized in Go, so anything that needs a stable
output — a log line, a golden test assertion — must sort. `Sorted` returns the
members in ascending order via `slices.Sorted(maps.Keys(s.m))`. It is a free
function, not a method, because sorting needs an *ordered* element type
(`cmp.Ordered`), which is a stronger promise than `comparable`: you can put any
comparable type in the set, but you can only sort the ordered ones. This split is
the whole point — `Set` stays maximally general (`comparable`), and ordering is a
capability offered only where the element type supports it.

Create `idset.go`:

```go
package idset

import (
	"cmp"
	"maps"
	"slices"
)

// Set is a generic set. T must be comparable because members are stored as map
// keys; that constraint is what makes map[T]struct{} legal.
type Set[T comparable] struct {
	m map[T]struct{}
}

// New returns an empty Set.
func New[T comparable]() *Set[T] {
	return &Set[T]{m: make(map[T]struct{})}
}

// Add inserts v and reports whether it was newly added. A false result means v
// was already present (a duplicate delivery to drop).
func (s *Set[T]) Add(v T) (added bool) {
	if _, ok := s.m[v]; ok {
		return false
	}
	s.m[v] = struct{}{}
	return true
}

// Contains reports whether v is a member.
func (s *Set[T]) Contains(v T) bool {
	_, ok := s.m[v]
	return ok
}

// Len reports the number of members.
func (s *Set[T]) Len() int { return len(s.m) }

// Equal reports whether two sets have the same members, order-independently.
func (s *Set[T]) Equal(other *Set[T]) bool {
	return maps.Equal(s.m, other.m)
}

// Sorted returns the members of s in ascending order. It is a function, not a
// method, because ordering needs cmp.Ordered, a stronger promise than comparable.
func Sorted[T cmp.Ordered](s *Set[T]) []T {
	return slices.Sorted(maps.Keys(s.m))
}
```

### The runnable demo

The demo replays a batch of message IDs with duplicates through the set, counting
how many were first-time deliveries, and prints the deterministic sorted membership.

Create `cmd/demo/main.go`:

```go
package main

import (
	"fmt"

	"example.com/idset"
)

func main() {
	seen := idset.New[string]()
	batch := []string{"m1", "m2", "m1", "m3", "m2", "m1"}

	delivered := 0
	for _, id := range batch {
		if seen.Add(id) {
			delivered++
		}
	}

	fmt.Printf("batch size: %d\n", len(batch))
	fmt.Printf("unique delivered: %d\n", delivered)
	fmt.Printf("contains m2: %v\n", seen.Contains("m2"))
	fmt.Printf("sorted: %v\n", idset.Sorted(seen))
}
```

Run it:

```bash
go run ./cmd/demo
```

Expected output:

```
batch size: 6
unique delivered: 3
contains m2: true
sorted: [m1 m2 m3]
```

### Tests

`TestAddDedups` adds duplicates and asserts `Len` stays stable and `Add` reports
`false` on repeats. `TestEqualOrderInsensitive` builds two sets with the same
members in different orders and asserts `Equal`. `TestSortedDeterministic` pins the
golden ascending order regardless of insertion order. `TestStructKey` instantiates
the set with a comparable struct key, proving the `comparable` constraint carries
through to composite types.

Create `idset_test.go`:

```go
package idset

import (
	"slices"
	"testing"
)

func TestAddDedups(t *testing.T) {
	t.Parallel()

	s := New[string]()
	if !s.Add("a") {
		t.Fatal("first Add should report added=true")
	}
	if s.Add("a") {
		t.Fatal("duplicate Add should report added=false")
	}
	s.Add("b")
	s.Add("a")
	if s.Len() != 2 {
		t.Fatalf("Len = %d, want 2", s.Len())
	}
	if !s.Contains("b") || s.Contains("z") {
		t.Fatal("Contains gave a wrong answer")
	}
}

func TestEqualOrderInsensitive(t *testing.T) {
	t.Parallel()

	a := New[int]()
	for _, v := range []int{1, 2, 3} {
		a.Add(v)
	}
	b := New[int]()
	for _, v := range []int{3, 1, 2} {
		b.Add(v)
	}
	if !a.Equal(b) {
		t.Fatal("sets with the same members must be Equal regardless of insertion order")
	}

	b.Add(4)
	if a.Equal(b) {
		t.Fatal("sets with different members must not be Equal")
	}
}

func TestSortedDeterministic(t *testing.T) {
	t.Parallel()

	s := New[string]()
	for _, v := range []string{"m3", "m1", "m2", "m1"} {
		s.Add(v)
	}
	if got := Sorted(s); !slices.Equal(got, []string{"m1", "m2", "m3"}) {
		t.Fatalf("Sorted = %v, want [m1 m2 m3]", got)
	}
}

func TestStructKey(t *testing.T) {
	t.Parallel()

	type Partition struct {
		Topic string
		ID    int
	}
	s := New[Partition]()
	s.Add(Partition{Topic: "orders", ID: 0})
	s.Add(Partition{Topic: "orders", ID: 1})
	s.Add(Partition{Topic: "orders", ID: 0}) // duplicate by value

	if s.Len() != 2 {
		t.Fatalf("Len = %d, want 2 (struct keys dedup by value)", s.Len())
	}
	if !s.Contains(Partition{Topic: "orders", ID: 1}) {
		t.Fatal("Contains should find an equal struct value")
	}
}
```

## Review

The set is correct when membership is idempotent (`Add` of a present value is a
no-op returning `false`) and `Equal` depends only on the members, not their order.
`TestStructKey` is the assertion that proves the `comparable` constraint is doing
real work: a struct value that matches by fields dedups, so the constraint carried
through composite types exactly as promised. The one design subtlety worth internalizing
is why `Sorted` is a free function bounded by `cmp.Ordered` rather than a method:
`Set` accepts any comparable `T`, but only ordered types can be sorted, so ordering
lives outside the type where the stronger constraint can be stated. Run `go test -race`.

## Resources

- [The comparable constraint](https://go.dev/ref/spec#Comparison_operators) — what `comparable` admits, and why it equals map-key-ability.
- [maps.Equal](https://pkg.go.dev/maps#Equal) and [maps.Keys](https://pkg.go.dev/maps#Keys) — type-safe map comparison and key iteration.
- [slices.Sorted](https://pkg.go.dev/slices#Sorted) — collecting an iterator into a sorted slice for deterministic output.

---

Back to [00-concepts.md](00-concepts.md) | Next: [08-money-equal-method-gocmp.md](08-money-equal-method-gocmp.md)
