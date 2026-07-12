# Exercise 4: You Cannot Assign to m[k].Field — Session Store, Values vs Pointers

A sliding-window session store must bump `LastSeen` on every request. Write it
over a `map[string]Session` and the obvious line — `store[id].LastSeen = now` —
does not compile, because a map value is not addressable. This module implements
`Touch` two correct ways behind one interface: read-modify-write on a value map,
and mutate-through-pointer on a pointer map.

This module is fully self-contained: its own `go mod init`, all code inline, its
own demo and tests.

## What you'll build

```text
sessionstore/                 independent module: example.com/sessionstore
  go.mod                      go 1.24
  store.go                    Store interface; valueStore (map[string]Session) + ptrStore (map[string]*Session)
  store_test.go               write-back works; pointer mutation visible on held *Session; missing id errors
  cmd/demo/main.go            runnable demo touching a session in both stores
```

Files: `store.go`, `store_test.go`, `cmd/demo/main.go`.
Implement: a `Store` interface with `Touch(id, now)` and `Get(id)`; a value-map
implementation that read-modify-writes the whole struct back; a pointer-map
implementation that mutates through the pointer.
Test: value-map write-back updates `LastSeen`; pointer-map mutation is visible
through a held `*Session`; touching an unknown id returns `ErrNoSession`.
Verify: `go test -count=1 -race ./...`

Set up the module:

```bash
mkdir -p go-solutions/09-pointers/08-pointers-in-slices-and-maps/04-map-value-not-addressable-session-store/cmd/demo
cd go-solutions/09-pointers/08-pointers-in-slices-and-maps/04-map-value-not-addressable-session-store
```

### Why the value map cannot be mutated in place

`m[id]` on a `map[string]Session` evaluates to a *copy* of the stored value, and
that copy is not addressable — it has no stable location you could assign a field
through. The line

```go
store[id].LastSeen = now // compile error: cannot assign to struct field store[id].LastSeen in map
```

is rejected outright. The map does not expose an addressable slot the way a slice
element `s[i]` does, because a map may rehash and move its entries; handing out
`&m[id]` would be a pointer into memory the map is free to relocate. So Go forbids
it. The two supported patterns are: read the value out, modify the copy, and write
the whole thing back (`s := m[id]; s.LastSeen = now; m[id] = s`); or store pointers
and mutate through them.

The write-back pattern is correct and idiomatic for small structs — it costs one
extra struct copy and one map store per mutation, and it keeps the value semantics
(each stored `Session` is independent). The pointer map (`map[string]*Session`)
compiles `m[id].LastSeen = now` because `m[id]` is a `*Session` and
`(*Session).LastSeen` is addressable; it also lets a caller hold the `*Session` and
observe later mutations. The trade-off is exactly the mutation-semantics decision
from the concepts: values give isolation and a write-back cost, pointers give
shared in-place mutation and a nil-and-escape surface.

Create `store.go`:

```go
package sessionstore

import (
	"errors"
	"time"
)

// ErrNoSession is returned when an id is not present.
var ErrNoSession = errors.New("no such session")

type Session struct {
	ID       string
	User     string
	LastSeen time.Time
}

// Store is the common interface both implementations satisfy.
type Store interface {
	Add(s Session)
	Touch(id string, now time.Time) error
	Get(id string) (Session, error)
}

// valueStore keeps sessions by value. It cannot mutate a field in place; it must
// read-modify-write the whole struct back.
type valueStore struct {
	m map[string]Session
}

func NewValueStore() Store {
	return &valueStore{m: make(map[string]Session)}
}

func (v *valueStore) Add(s Session) { v.m[s.ID] = s }

func (v *valueStore) Touch(id string, now time.Time) error {
	s, ok := v.m[id]
	if !ok {
		return ErrNoSession
	}
	// v.m[id].LastSeen = now would NOT compile: map value is not addressable.
	s.LastSeen = now
	v.m[id] = s // write the whole struct back
	return nil
}

func (v *valueStore) Get(id string) (Session, error) {
	s, ok := v.m[id]
	if !ok {
		return Session{}, ErrNoSession
	}
	return s, nil
}

// ptrStore keeps sessions by pointer and mutates through the pointer in place.
type ptrStore struct {
	m map[string]*Session
}

func NewPointerStore() Store {
	return &ptrStore{m: make(map[string]*Session)}
}

func (p *ptrStore) Add(s Session) {
	cp := s
	p.m[s.ID] = &cp
}

func (p *ptrStore) Touch(id string, now time.Time) error {
	s, ok := p.m[id]
	if !ok {
		return ErrNoSession
	}
	s.LastSeen = now // legal: s is *Session, (*s).LastSeen is addressable
	return nil
}

func (p *ptrStore) Get(id string) (Session, error) {
	s, ok := p.m[id]
	if !ok {
		return Session{}, ErrNoSession
	}
	return *s, nil
}

// handle exposes the live *Session so a test can observe in-place mutation. It
// is only meaningful on the pointer store.
func (p *ptrStore) handle(id string) *Session { return p.m[id] }
```

### The runnable demo

Create `cmd/demo/main.go`:

```go
package main

import (
	"fmt"
	"time"

	"example.com/sessionstore"
)

func main() {
	base := time.Date(2026, 1, 1, 12, 0, 0, 0, time.UTC)
	later := base.Add(30 * time.Minute)

	for _, tc := range []struct {
		name  string
		store sessionstore.Store
	}{
		{"value", sessionstore.NewValueStore()},
		{"pointer", sessionstore.NewPointerStore()},
	} {
		tc.store.Add(sessionstore.Session{ID: "s1", User: "alice", LastSeen: base})
		_ = tc.store.Touch("s1", later)
		s, _ := tc.store.Get("s1")
		fmt.Printf("%s store: LastSeen=%s\n", tc.name, s.LastSeen.Format("15:04"))
	}
}
```

Run it:

```bash
go run ./cmd/demo
```

Expected output:

```
value store: LastSeen=12:30
pointer store: LastSeen=12:30
```

### Tests

`TestValueMapRequiresWriteBack` proves the write-back pattern updates `LastSeen`
through `Get`. `TestPointerMapMutatesInPlace` holds the live `*Session` and asserts
the field changes through that held pointer after `Touch` — the property a value
map cannot give you. `TestTouchMissingIsNoOp` asserts an unknown id returns
`ErrNoSession` with no panic, for both implementations.

Create `store_test.go`:

```go
package sessionstore

import (
	"errors"
	"testing"
	"time"
)

func TestValueMapRequiresWriteBack(t *testing.T) {
	t.Parallel()

	base := time.Date(2026, 1, 1, 12, 0, 0, 0, time.UTC)
	later := base.Add(time.Hour)

	s := NewValueStore()
	s.Add(Session{ID: "s1", User: "alice", LastSeen: base})
	if err := s.Touch("s1", later); err != nil {
		t.Fatal(err)
	}

	got, err := s.Get("s1")
	if err != nil {
		t.Fatal(err)
	}
	if !got.LastSeen.Equal(later) {
		t.Fatalf("LastSeen = %v, want %v (write-back must persist)", got.LastSeen, later)
	}
}

func TestPointerMapMutatesInPlace(t *testing.T) {
	t.Parallel()

	base := time.Date(2026, 1, 1, 12, 0, 0, 0, time.UTC)
	later := base.Add(time.Hour)

	p := NewPointerStore().(*ptrStore)
	p.Add(Session{ID: "s1", User: "alice", LastSeen: base})

	held := p.handle("s1") // a live handle a caller could keep
	if err := p.Touch("s1", later); err != nil {
		t.Fatal(err)
	}
	if !held.LastSeen.Equal(later) {
		t.Fatalf("held.LastSeen = %v, want %v (mutation must be visible through the pointer)", held.LastSeen, later)
	}
}

func TestTouchMissingIsNoOp(t *testing.T) {
	t.Parallel()

	now := time.Now()
	for _, s := range []Store{NewValueStore(), NewPointerStore()} {
		if err := s.Touch("missing", now); !errors.Is(err, ErrNoSession) {
			t.Fatalf("Touch(missing) err = %v, want ErrNoSession", err)
		}
	}
}
```

## Review

The compile error `cannot assign to struct field m[id].LastSeen in map` is the
gate this exercise teaches you to walk through, not around. The value store must
`s := m[id]; s.LastSeen = now; m[id] = s`; `TestValueMapRequiresWriteBack` proves
that pattern persists. The pointer store mutates `m[id].LastSeen` directly and,
crucially, a caller holding the `*Session` sees the change —
`TestPointerMapMutatesInPlace` reads through a `held` handle to prove it. Choose
the value map with write-back when you want isolated sessions and can afford the
struct copy; choose the pointer map when many call sites mutate one session or the
struct is large — and then handle the nil case on lookup, which the next exercise
makes its whole subject.

## Resources

- [Go spec: Assignments](https://go.dev/ref/spec#Assignments) — a map index expression is not addressable, so its fields cannot be assigned.
- [Go FAQ: Why can't I take the address of a map value?](https://go.dev/doc/faq#map_values) — the rationale: maps may relocate entries.
- [`time.Time.Equal`](https://pkg.go.dev/time#Time.Equal) — comparing instants correctly in the tests.

---

Back to [00-concepts.md](00-concepts.md) | Next: [05-append-reslice-dangling-pointers.md](05-append-reslice-dangling-pointers.md)
