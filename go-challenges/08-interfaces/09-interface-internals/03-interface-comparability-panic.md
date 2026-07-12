# Exercise 3: Guard A Deduplication Set Against Uncomparable Interface Values

An idempotency or dedup set keyed on `any` is a common backend building block:
drop a message you have already processed. It has a runtime landmine — feed it a
value whose dynamic type is a slice, map, or func and the `==` or map-key
comparison panics. This module builds the guard that turns that panic into a
handled error.

This module is fully self-contained: its own module, code, demo, and tests.

## What you'll build

```text
dedup/                      independent module: example.com/dedup
  go.mod                    go 1.26
  dedup.go                  Comparable(any) bool; Set with a guarded Add; ErrUncomparable
  cmd/
    demo/
      main.go               add comparable values, reject a slice, catch a raw panic
  dedup_test.go             table of comparable/uncomparable types, recover proof
```

- Files: `dedup.go`, `cmd/demo/main.go`, `dedup_test.go`.
- Implement: `Comparable(any) bool` (via `reflect.Type.Comparable`), a `Set` over `map[any]struct{}` whose `Add` rejects uncomparable values with `ErrUncomparable` (wrapped `%w`) instead of panicking.
- Test: comparable dynamic types (int, string, comparable struct) are accepted; slice/map/func values are rejected by the guard; a `recover`-based test proves the unguarded map insert would panic.
- Verify: `go test -count=1 -race ./...`

Set up the module:

```bash
go mod edit -go=1.26
```

### Why == on an interface can blow up

Comparing two interface values with `==` first compares their type words; if those
match, it compares the concrete values. That second step is defined only for
*comparable* types. Slices, maps, and functions are not comparable — there is no
well-defined equality for them (Go deliberately refuses reference-identity
equality for these) — so comparing them panics with "runtime error: comparing
uncomparable type". A `map[any]struct{}` hits the same wall harder: inserting or
looking up a key hashes it and compares it, so using a slice as a key panics with
"runtime error: hash of unhashable type". The dynamic type is only known at
runtime, so the compiler cannot catch this; an `any` that happens to hold a
`[]byte` sails past the type checker and detonates in production.

The guard is `reflect.TypeOf(x).Comparable()`. `reflect.Type` exposes exactly the
comparability bit the runtime uses for `==` and for map keys, so it is the
authoritative precheck. One edge: `reflect.TypeOf(nil)` returns a nil
`reflect.Type`, and calling `.Comparable()` on it would panic — so `Comparable`
special-cases a nil interface as comparable (the untyped nil is comparable and is
a valid map key). With the precheck in place, `Add` inspects the value first and
returns `ErrUncomparable` (wrapped with `%w` so callers can match it with
`errors.Is`) rather than letting the map insert panic. That converts an
unrecoverable crash on the hot path into an ordinary, testable error return.

Create `dedup.go`:

```go
package dedup

import (
	"errors"
	"fmt"
	"reflect"
)

// ErrUncomparable is returned when a value cannot be used as a set key because
// its dynamic type is not comparable (slice, map, or func).
var ErrUncomparable = errors.New("uncomparable value")

// Comparable reports whether x can be compared with == and used as a map key
// without panicking. A nil interface is comparable.
func Comparable(x any) bool {
	if x == nil {
		return true
	}
	return reflect.TypeOf(x).Comparable()
}

// Set is a deduplication set keyed on any. Add rejects uncomparable values
// instead of panicking on the map insert.
type Set struct {
	seen map[any]struct{}
}

// NewSet builds an empty Set.
func NewSet() *Set {
	return &Set{seen: make(map[any]struct{})}
}

// Add inserts x and reports whether it was newly added. It returns an error
// wrapping ErrUncomparable if x cannot safely be a map key.
func (s *Set) Add(x any) (bool, error) {
	if !Comparable(x) {
		return false, fmt.Errorf("cannot dedup %T: %w", x, ErrUncomparable)
	}
	if _, ok := s.seen[x]; ok {
		return false, nil
	}
	s.seen[x] = struct{}{}
	return true, nil
}

// Len reports the number of distinct values stored.
func (s *Set) Len() int {
	return len(s.seen)
}
```

### The runnable demo

The demo adds two distinct comparable values and a duplicate (showing the second
add is a no-op), then tries to add a slice and prints the guarded error. Finally
it shows what the guard prevents: inserting a slice key into a raw
`map[any]struct{}` inside a `recover`, so the demo can print the panic text
instead of crashing.

Create `cmd/demo/main.go`:

```go
package main

import (
	"errors"
	"fmt"

	"example.com/dedup"
)

func main() {
	s := dedup.NewSet()

	added, _ := s.Add("evt-1")
	fmt.Printf("add evt-1: added=%v\n", added)
	added, _ = s.Add("evt-1")
	fmt.Printf("add evt-1 again: added=%v\n", added)
	added, _ = s.Add(42)
	fmt.Printf("add 42: added=%v len=%d\n", added, s.Len())

	_, err := s.Add([]string{"a", "b"})
	fmt.Printf("add slice: rejected=%v\n", errors.Is(err, dedup.ErrUncomparable))

	fmt.Println("unguarded panic:", rawPanic())
}

// rawPanic shows what Add prevents: a slice key panics a bare map[any]struct{}.
func rawPanic() (msg string) {
	defer func() {
		if r := recover(); r != nil {
			msg = fmt.Sprint(r)
		}
	}()
	m := map[any]struct{}{}
	m[[]int{1}] = struct{}{}
	return "no panic"
}
```

Run it:

```bash
go run ./cmd/demo
```

Expected output:

```
add evt-1: added=true
add evt-1 again: added=false
add 42: added=true len=2
add slice: rejected=true
unguarded panic: runtime error: hash of unhashable type []int
```

### Tests

The table drives comparable and uncomparable dynamic types through the same guard:
`int`, `string`, and a struct of comparable fields are accepted; a slice, a map,
and a func are rejected with an error that `errors.Is` matches to
`ErrUncomparable`. `TestUnguardedMapKeyPanics` proves the guard is load-bearing by
inserting a slice key into a bare map inside a `recover` and asserting a panic
actually occurred — if the guard were removed, `Add` would panic exactly there.
`TestStructWithSliceFieldRejected` covers the subtle case: a struct is comparable
only if all its fields are, so a struct containing a slice is uncomparable.

Create `dedup_test.go`:

```go
package dedup

import (
	"errors"
	"testing"
)

func TestAddComparable(t *testing.T) {
	t.Parallel()

	type point struct{ X, Y int }
	tests := []struct {
		name string
		val  any
	}{
		{"int", 42},
		{"string", "evt"},
		{"comparable struct", point{1, 2}},
		{"nil", nil},
		{"pointer", new(int)},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			s := NewSet()
			added, err := s.Add(tc.val)
			if err != nil {
				t.Fatalf("Add(%#v) error = %v, want nil", tc.val, err)
			}
			if !added {
				t.Fatalf("Add(%#v) added = false, want true", tc.val)
			}
		})
	}
}

func TestAddUncomparableRejected(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name string
		val  any
	}{
		{"slice", []int{1, 2}},
		{"map", map[string]int{"a": 1}},
		{"func", func() {}},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			s := NewSet()
			_, err := s.Add(tc.val)
			if !errors.Is(err, ErrUncomparable) {
				t.Fatalf("Add(%s) error = %v, want ErrUncomparable", tc.name, err)
			}
			if s.Len() != 0 {
				t.Fatalf("rejected value must not be stored, len = %d", s.Len())
			}
		})
	}
}

func TestStructWithSliceFieldRejected(t *testing.T) {
	t.Parallel()

	type payload struct {
		ID   int
		Tags []string // makes the struct uncomparable
	}
	s := NewSet()
	_, err := s.Add(payload{ID: 1, Tags: []string{"x"}})
	if !errors.Is(err, ErrUncomparable) {
		t.Fatalf("struct with slice field: error = %v, want ErrUncomparable", err)
	}
}

func TestDedupSkipsDuplicates(t *testing.T) {
	t.Parallel()

	s := NewSet()
	for range 3 {
		if _, err := s.Add("same"); err != nil {
			t.Fatalf("Add(same) error = %v", err)
		}
	}
	if s.Len() != 1 {
		t.Fatalf("Len after 3 identical adds = %d, want 1", s.Len())
	}
}

func TestUnguardedMapKeyPanics(t *testing.T) {
	t.Parallel()

	panicked := func() (p bool) {
		defer func() {
			if recover() != nil {
				p = true
			}
		}()
		m := map[any]struct{}{}
		m[[]int{1}] = struct{}{} // hash of unhashable type -> panic
		return false
	}()

	if !panicked {
		t.Fatal("expected a slice map key to panic; the guard prevents this")
	}
}
```

## Review

The set is correct when the guard is total: every value `Add` accepts is one the
runtime would accept as a map key, and every value it rejects is one that would
have panicked. The `recover` test is the proof that the danger is real —
`m[[]int{1}]` panics with "hash of unhashable type", which is exactly the crash
`Add` converts into an `ErrUncomparable` return. The struct case is the one people
miss: comparability is recursive, so a struct is a valid key only if *all* its
fields are comparable; add one slice field and the whole struct is out. Match the
rejection with `errors.Is(err, ErrUncomparable)`, never by string, since the
message includes the concrete type. Run `go test -race` to confirm the map is not
touched concurrently without synchronization.

## Resources

- [Go Specification: Comparison operators](https://go.dev/ref/spec#Comparison_operators) — which types are comparable and why slices/maps/funcs are not.
- [reflect.Type.Comparable](https://pkg.go.dev/reflect#Type) — the authoritative comparability bit used by the guard.
- [Effective Go: Recover](https://go.dev/doc/effective_go#recover) — the `recover` pattern the panic proof relies on.

---

Back to [00-concepts.md](00-concepts.md) | Prev: [02-typed-nil-error-trap.md](02-typed-nil-error-trap.md) | Next: [04-boxing-allocation-hotpath.md](04-boxing-allocation-hotpath.md)
