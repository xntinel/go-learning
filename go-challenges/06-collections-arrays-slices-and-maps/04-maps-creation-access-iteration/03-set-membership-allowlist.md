# Exercise 3: Sets with map[T]struct{}: an origin/scope allowlist with zero-byte values

Go has no built-in set, and the idiom is `map[T]struct{}` — a map whose value type
occupies zero bytes, so membership costs only the key. This exercise builds a
generic `Set[T comparable]` on that idiom and uses it as a CORS-origin and
OAuth-scope allowlist, with `Union`, `Intersect`, and a deterministic `Sorted`
view.

This module is fully self-contained. It begins with its own `go mod init`, defines
every type it needs, and ships its own demo and tests. Nothing here imports any
other exercise.

## What you'll build

```text
allowset/                  independent module: example.com/allowset
  go.mod
  set.go                   Set[T comparable] over map[T]struct{}; Add, Contains, Remove, Len, Union, Intersect, Sorted
  cmd/
    demo/
      main.go              runnable demo: origin allowlist, scope intersection
  set_test.go              idempotent Add, Contains, Union/Intersect, deterministic Sorted, zero-width value
```

- Files: `set.go`, `cmd/demo/main.go`, `set_test.go`.
- Implement: `Set[T comparable]` backed by `map[T]struct{}` with `Add`, `Contains` (comma-ok), `Remove`, `Len`, `Union`, `Intersect`, and a package-level `Sorted[T cmp.Ordered]`.
- Test: `Add` is idempotent, `Contains` reports membership, `Union`/`Intersect` are correct, `Sorted` is deterministic across insertion orders, and the value type is zero-width.
- Verify: `go test -count=1 -race ./...`

Set up the module:

```bash
mkdir -p ~/go-exercises/allowset/cmd/demo
cd ~/go-exercises/allowset
go mod init example.com/allowset
```

### Why struct{} and not bool

The set stores presence, nothing else, so the value carries no information — it
exists only to make the key present. `struct{}` is the type with no fields and a
size of zero bytes, so a set of a million origins pays for a million keys and
nothing for the values. Using `map[T]bool` instead would waste a byte per entry
and, worse, invites the bug where someone stores `false` and `Contains` becomes
ambiguous: is a `false` entry "present-but-false" or "absent"? With
`map[T]struct{}`, membership is unambiguous — the key is either in the map or it
is not, tested with comma-ok (`_, ok := s.m[v]`). The only literal you ever write
as a value is `struct{}{}`, the single value of the empty-struct type.

`Sorted` is a package-level function, not a method, because sorting requires the
element type to be *ordered* (`cmp.Ordered`), a stronger constraint than the
`comparable` that a set key needs — you can build a `Set[somestruct]` but only
sort a `Set[string]` or `Set[int]`. It is the canonical deterministic-output
idiom: `slices.Sorted(maps.Keys(s.m))` collects the keys from the
`maps.Keys` iterator and sorts them, so the same members always print in the same
order regardless of the randomized map iteration underneath.

Create `set.go`:

```go
package allowset

import (
	"cmp"
	"maps"
	"slices"
)

// Set is a generic set backed by map[T]struct{}: the zero-width value means an
// entry costs only its key.
type Set[T comparable] struct {
	m map[T]struct{}
}

// New returns a Set containing the given items.
func New[T comparable](items ...T) *Set[T] {
	s := &Set[T]{m: make(map[T]struct{}, len(items))}
	for _, v := range items {
		s.Add(v)
	}
	return s
}

// Add inserts v. Adding a member already present is a no-op (idempotent).
func (s *Set[T]) Add(v T) { s.m[v] = struct{}{} }

// Contains reports whether v is a member, via comma-ok.
func (s *Set[T]) Contains(v T) bool {
	_, ok := s.m[v]
	return ok
}

// Remove deletes v. Removing an absent member is a no-op.
func (s *Set[T]) Remove(v T) { delete(s.m, v) }

// Len reports the number of members.
func (s *Set[T]) Len() int { return len(s.m) }

// Union returns a new Set containing every member of s or other.
func (s *Set[T]) Union(other *Set[T]) *Set[T] {
	out := New[T]()
	for v := range s.m {
		out.Add(v)
	}
	for v := range other.m {
		out.Add(v)
	}
	return out
}

// Intersect returns a new Set containing members present in both s and other.
func (s *Set[T]) Intersect(other *Set[T]) *Set[T] {
	out := New[T]()
	for v := range s.m {
		if other.Contains(v) {
			out.Add(v)
		}
	}
	return out
}

// Sorted returns the members of s in ascending order. It is a function, not a
// method, because sorting needs cmp.Ordered, stronger than a set key's
// comparable constraint.
func Sorted[T cmp.Ordered](s *Set[T]) []T {
	return slices.Sorted(maps.Keys(s.m))
}
```

### The runnable demo

The demo builds an origin allowlist for a CORS check and intersects a set of
granted OAuth scopes with the scopes a request requires.

Create `cmd/demo/main.go`:

```go
package main

import (
	"fmt"

	"example.com/allowset"
)

func main() {
	origins := allowset.New(
		"https://app.example.com",
		"https://admin.example.com",
	)
	fmt.Printf("app allowed=%v\n", origins.Contains("https://app.example.com"))
	fmt.Printf("evil allowed=%v\n", origins.Contains("https://evil.example.com"))
	fmt.Printf("allowlist: %v\n", allowset.Sorted(origins))

	granted := allowset.New("read", "write", "admin")
	required := allowset.New("write", "delete")
	fmt.Printf("satisfied scopes: %v\n", allowset.Sorted(granted.Intersect(required)))
	fmt.Printf("all scopes: %v\n", allowset.Sorted(granted.Union(required)))
}
```

Run it:

```bash
go run ./cmd/demo
```

Expected output:

```
app allowed=true
evil allowed=false
allowlist: [https://admin.example.com https://app.example.com]
satisfied scopes: [write]
all scopes: [admin delete read write]
```

The allowlist and scope views print in sorted order every run because `Sorted`
collects and sorts the keys rather than ranging the map, whose order is
randomized.

### Tests

`TestAddIdempotent` proves adding a member twice does not grow the set.
`TestContains` covers the true and false branches. `TestUnionIntersect` pins the
set algebra. `TestSortedDeterministic` builds the same members in two different
insertion orders and asserts `Sorted` returns identical slices — the property that
makes set output safe to serialize. `TestValueIsZeroWidth` asserts, via
`unsafe.Sizeof`, that the map's value type occupies zero bytes, which is the whole
reason `struct{}` is the idiom.

Create `set_test.go`:

```go
package allowset

import (
	"fmt"
	"slices"
	"testing"
	"unsafe"
)

func TestAddIdempotent(t *testing.T) {
	t.Parallel()

	s := New("a")
	s.Add("a")
	s.Add("a")
	if got := s.Len(); got != 1 {
		t.Fatalf("Len() = %d, want 1 after adding a duplicate", got)
	}
}

func TestContains(t *testing.T) {
	t.Parallel()

	s := New("read", "write")
	if !s.Contains("read") {
		t.Fatal("Contains(read) = false, want true")
	}
	if s.Contains("delete") {
		t.Fatal("Contains(delete) = true, want false")
	}
	s.Remove("read")
	if s.Contains("read") {
		t.Fatal("Contains(read) = true after Remove, want false")
	}
}

func TestUnionIntersect(t *testing.T) {
	t.Parallel()

	a := New(1, 2, 3)
	b := New(2, 3, 4)

	if got := Sorted(a.Union(b)); !slices.Equal(got, []int{1, 2, 3, 4}) {
		t.Fatalf("Union = %v, want [1 2 3 4]", got)
	}
	if got := Sorted(a.Intersect(b)); !slices.Equal(got, []int{2, 3}) {
		t.Fatalf("Intersect = %v, want [2 3]", got)
	}
}

func TestSortedDeterministic(t *testing.T) {
	t.Parallel()

	forward := New("c", "a", "b", "e", "d")
	backward := New("d", "e", "b", "a", "c")

	if got, want := Sorted(forward), Sorted(backward); !slices.Equal(got, want) {
		t.Fatalf("Sorted differs by insertion order: %v vs %v", got, want)
	}
	if got := Sorted(forward); !slices.Equal(got, []string{"a", "b", "c", "d", "e"}) {
		t.Fatalf("Sorted = %v, want [a b c d e]", got)
	}
}

func TestValueIsZeroWidth(t *testing.T) {
	t.Parallel()

	if sz := unsafe.Sizeof(struct{}{}); sz != 0 {
		t.Fatalf("struct{} size = %d, want 0", sz)
	}
}

func ExampleSet_Contains() {
	s := New("https://app.example.com")
	fmt.Println(s.Contains("https://app.example.com"))
	fmt.Println(s.Contains("https://evil.example.com"))
	// Output:
	// true
	// false
}
```

## Review

The set is correct when membership is exactly key-presence: `Add` inserts the
zero-width `struct{}{}`, `Contains` reports the comma-ok `ok`, and `Remove` deletes
the key — there is no value to be ambiguous about, which is the failure mode of the
`map[T]bool` alternative. `Union` and `Intersect` build fresh sets so neither
operand is mutated. `Sorted` is the deterministic view: because it collects and
sorts keys instead of ranging the map, its output is stable across runs and across
insertion orders, which is exactly what `TestSortedDeterministic` locks in. The
`unsafe.Sizeof` assertion documents why the idiom is `struct{}` and not `bool`.

## Resources

- [Go blog: Go maps in action](https://go.dev/blog/maps) — the set idiom and why `map[T]struct{}`.
- [maps.Keys](https://pkg.go.dev/maps#Keys) and [slices.Sorted](https://pkg.go.dev/slices#Sorted) — the deterministic key-collection idiom.
- [The empty struct](https://go.dev/ref/spec#Struct_types) — a struct with no fields has size zero.

---

Back to [00-concepts.md](00-concepts.md) | Prev: [02-fixed-window-rate-limiter.md](02-fixed-window-rate-limiter.md) | Next: [04-group-by-inverted-index.md](04-group-by-inverted-index.md)
