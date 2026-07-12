# Exercise 8: Retire `any` — A Type-Safe Generic Store Replacing an `interface{}` Cache

The capstone is a trade-off made concrete. This module takes an `any`-valued
in-memory store — the kind that forces every caller into an assertion and boxes every
value on the way in — and refactors it into `Store[K comparable, V any]`, where the
compiler enforces the value type and the boxing disappears. It shows precisely when
`any` is the wrong tool.

This module is fully self-contained: its own `go mod init`, its own demo, its own
tests. Nothing here imports another exercise.

## What you'll build

```text
genericstore/              independent module: example.com/genericstore
  go.mod                   go 1.26
  store.go                 legacy AnyStore (map[string]any) + generic Store[K comparable, V any]
  cmd/
    demo/
      main.go              runnable demo: the assertion tax vs the typed Get
  store_test.go            behavior parity, no-assertion Get, AllocsPerRun boxing comparison
```

- Files: `store.go`, `cmd/demo/main.go`, `store_test.go`.
- Implement: a legacy `AnyStore` whose `Get` returns `(any, bool)` and forces a caller assertion, and a `Store[K comparable, V any]` whose `Get` returns `(V, bool)` with no assertion; both concurrency-safe with `sync.RWMutex`.
- Test: behavior parity between the two, a typed `Get` with no call-site assertion, and a `testing.AllocsPerRun` comparison proving the generic path avoids the per-value `eface` allocation the `any` path incurs.
- Verify: `go test -count=1 -race ./...`

Set up the module:

```bash
mkdir -p go-solutions/08-interfaces/02-empty-interface-and-any/08-generics-over-any-store/cmd/demo
cd go-solutions/08-interfaces/02-empty-interface-and-any/08-generics-over-any-store
go mod edit -go=1.26
```

### The assertion tax and the boxing tax

The `any`-valued store looks convenient: one type holds anything. The bill arrives at
every call site. `Get` returns `(any, bool)`, so a caller that stored a `*User` must
write `v, ok := s.Get("u"); u := v.(*User)` — a runtime assertion that can fail if the
wrong thing was stored, and that the compiler cannot check. Worse, the type identity is
gone: nothing stops one part of the code from storing a `*User` under a key and another
from reading it back as an `*Order`. That mismatch compiles cleanly and panics (or, with
comma-ok, silently mishandles) at runtime. The `any` store trades a compile-time error
for a production incident.

`Store[K comparable, V any]` moves the whole contract to compile time. The value type is
a type parameter, so `Get` returns `(V, bool)` — a real, typed value with no assertion —
and storing a `*User` in a `Store[string, *User]` and reading an `*Order` is not a
runtime bug, it is a compile error. There is a second, quieter win: boxing. Putting a
concrete value into `map[string]any` boxes it into an `eface`, which for a non-pointer
value above the small-integer cache allocates on the heap. `Store[string, int]` stores
the `int` directly in a `map[string]int` with no box and no allocation. The
`AllocsPerRun` test measures exactly this. The lesson is not "generics are faster"; it is
"`any` here was paying an assertion tax and a boxing tax for a flexibility the code never
needed."

Create `store.go`:

```go
package genericstore

import "sync"

// AnyStore is the legacy design: one value type, any. Every caller pays an assertion
// tax on Get, and every non-pointer value is boxed into an eface on Set.
type AnyStore struct {
	mu sync.RWMutex
	m  map[string]any
}

// NewAnyStore returns an empty AnyStore.
func NewAnyStore() *AnyStore {
	return &AnyStore{m: make(map[string]any)}
}

// Set stores value under key. value is boxed into an any.
func (s *AnyStore) Set(key string, value any) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.m[key] = value
}

// Get returns the stored any and whether it was present. The caller must assert the
// concrete type, a check the compiler cannot make.
func (s *AnyStore) Get(key string) (any, bool) {
	s.mu.RLock()
	defer s.mu.RUnlock()
	v, ok := s.m[key]
	return v, ok
}

// Store is the type-safe replacement. The value type is a parameter, so Get returns a
// typed V with no assertion and the compiler enforces the value type at every call site.
type Store[K comparable, V any] struct {
	mu sync.RWMutex
	m  map[K]V
}

// NewStore returns an empty Store.
func NewStore[K comparable, V any]() *Store[K, V] {
	return &Store[K, V]{m: make(map[K]V)}
}

// Set stores value under key. No boxing: value is stored as its concrete type.
func (s *Store[K, V]) Set(key K, value V) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.m[key] = value
}

// Get returns the typed value and whether it was present. No assertion at the call site.
func (s *Store[K, V]) Get(key K) (V, bool) {
	s.mu.RLock()
	defer s.mu.RUnlock()
	v, ok := s.m[key]
	return v, ok
}
```

The mismatch the generic store makes impossible is worth seeing. Against the `any` store
this compiles and blows up at runtime:

```go
// Illustrative only — NOT part of the module. It compiles, then panics at runtime.
s := NewAnyStore()
s.Set("u", &User{Name: "alice"})
o := s.Get("u")           // (any, bool)
order := o.(*Order)        // panics: interface conversion *User is not *Order
_ = order
```

Against `Store[string, *User]`, the equivalent mistake — `var o *Order = st.Get("u")` —
does not compile at all, because `Get` returns `(*User, bool)`. The bug is caught before
it ships.

### The runnable demo

The demo shows the two call sites side by side: the `any` store forces an assertion, the
generic store returns a typed value directly.

Create `cmd/demo/main.go`:

```go
package main

import (
	"fmt"

	"example.com/genericstore"
)

type User struct {
	Name string
}

func main() {
	// The any store: Get returns any; the caller must assert.
	as := genericstore.NewAnyStore()
	as.Set("u", &User{Name: "alice"})
	if v, ok := as.Get("u"); ok {
		u := v.(*User) // the assertion tax
		fmt.Println("any store:    ", u.Name)
	}

	// The generic store: Get returns *User directly; no assertion.
	gs := genericstore.NewStore[string, *User]()
	gs.Set("u", &User{Name: "bob"})
	if u, ok := gs.Get("u"); ok {
		fmt.Println("generic store:", u.Name) // u is already *User
	}
}
```

Run it:

```bash
go run ./cmd/demo
```

Expected output:

```
any store:     alice
generic store: bob
```

### Tests

`TestParity` proves the two stores behave identically for present and absent keys.
`TestTypedGetNoAssertion` shows the generic `Get` returns a typed value used with no
assertion. `TestGenericAvoidsBoxing` is the headline: it measures allocations per `Set`
with `testing.AllocsPerRun` and asserts the generic store allocates strictly fewer than
the `any` store, because boxing a non-pointer value above the small-integer cache into an
`eface` allocates while storing it in a `map[string]int` does not.

Create `store_test.go`:

```go
package genericstore

import "testing"

func TestParity(t *testing.T) {
	t.Parallel()

	as := NewAnyStore()
	gs := NewStore[string, int]()

	as.Set("a", 1)
	gs.Set("a", 1)

	av, aok := as.Get("a")
	gv, gok := gs.Get("a")
	if aok != gok || av.(int) != gv {
		t.Fatalf("parity present: any=(%v,%v) generic=(%v,%v)", av, aok, gv, gok)
	}

	_, aok = as.Get("missing")
	_, gok = gs.Get("missing")
	if aok || gok {
		t.Fatalf("parity absent: any ok=%v generic ok=%v; want both false", aok, gok)
	}
}

func TestTypedGetNoAssertion(t *testing.T) {
	t.Parallel()

	gs := NewStore[string, []int]()
	gs.Set("nums", []int{1, 2, 3})

	// v is []int directly — no assertion, and slice indexing compiles.
	v, ok := gs.Get("nums")
	if !ok {
		t.Fatal("Get(nums) not found")
	}
	if len(v) != 3 || v[0] != 1 {
		t.Fatalf("typed Get returned %v, want [1 2 3]", v)
	}
}

func TestGenericAvoidsBoxing(t *testing.T) {
	// No t.Parallel: testing.AllocsPerRun must not run inside a parallel test.
	as := NewAnyStore()
	gs := NewStore[string, int]()
	as.Set("k", 0)
	gs.Set("k", 0)

	// The value must be a runtime, non-constant int above the small-integer cache
	// (0..255): a constant would be boxed statically and hide the allocation, and a
	// small value would hit the runtime's cached-int table. Overwriting the same key
	// isolates the boxing cost from map growth.
	var v int
	anyAllocs := testing.AllocsPerRun(1000, func() {
		v++
		as.Set("k", v+1000) // boxes a runtime int into an eface: allocates
	})
	genAllocs := testing.AllocsPerRun(1000, func() {
		v++
		gs.Set("k", v+1000) // stored directly in map[string]int: no box
	})

	if genAllocs >= anyAllocs {
		t.Fatalf("generic Set allocs=%.1f, any Set allocs=%.1f; want generic < any",
			genAllocs, anyAllocs)
	}
	if genAllocs != 0 {
		t.Fatalf("generic Set should not allocate, got %.1f", genAllocs)
	}
}
```

## Review

The refactor is correct when the generic store is a behavioral drop-in for the `any`
store — `TestParity` proves that — while eliminating both taxes the `any` design paid.
`TestTypedGetNoAssertion` shows the call site is assertion-free: `Get` hands back a real
`[]int` you can index immediately. `TestGenericAvoidsBoxing` measures the boxing tax
directly and asserts the generic path allocates zero where the `any` path allocates per
`Set`. The mistake this whole chapter has been circling is using `any` as a lazy
substitute for a real type: it defers a compile-time error to a runtime panic and adds
boxing the code never needed. Reach for `any` only at a genuine untyped boundary — the
JSON, config, context, driver, and log-attr artifacts of the earlier modules — and let a
type parameter or a small interface carry everything else. Run `go test -race` to confirm
parity and the allocation contract hold.

## Resources

- [Go Specification: Type parameters](https://go.dev/ref/spec#Type_parameter_declarations) — generic functions and types.
- [`testing.AllocsPerRun`](https://pkg.go.dev/testing#AllocsPerRun) — measuring allocations per call.
- [Go blog: An Introduction To Generics](https://go.dev/blog/intro-generics) — type parameters and the `comparable` constraint.

---

Back to [00-concepts.md](00-concepts.md) | Next: [../03-type-assertions-and-type-switches/00-concepts.md](../03-type-assertions-and-type-switches/00-concepts.md)
