# Exercise 9: Addressability — Mutating Session Structs Stored in a Map

Map elements are not addressable, which has a sharp consequence: you cannot call a
pointer-receiver method on a struct fetched directly out of a `map[K]V`, and a
value copy you pull out and mutate is thrown away. This module builds a
`SessionStore` and shows why the correct design for mutable elements is
`map[string]*Session`, not `map[string]Session`, so pointer-receiver methods like
`Touch` update the stored session in place.

This module is fully self-contained. It begins with its own `go mod init`,
defines every type it needs, and ships its own demo and tests. Nothing here
imports any other exercise.

## What you'll build

```text
sessionstore/              independent module: example.com/sessionstore
  go.mod
  store.go                 Session (pointer receiver Touch); SessionStore over map[string]*Session
  cmd/
    demo/
      main.go              add a session, advance the clock, touch it, revoke it
  store_test.go            touch updates last-seen; value-map copy loses the write; revoke removes
```

Files: `store.go`, `cmd/demo/main.go`, `store_test.go`.
Implement: `Session{ID string, LastSeen time.Time}` with a pointer-receiver `Touch(now time.Time)`; `SessionStore` backed by `map[string]*Session` with an injected `now func() time.Time`, plus `Add`, `Get`, `Touch`, `Revoke`.
Test: `Touch` advances `LastSeen` using an injected clock; a `map[string]Session` copy path loses the mutation (documented); `Revoke` removes the session.
Verify: `go test -count=1 -race ./...`

Set up the module:

```bash
mkdir -p go-solutions/07-structs-and-methods/03-methods-value-vs-pointer-receivers/09-map-element-addressability/cmd/demo
cd go-solutions/07-structs-and-methods/03-methods-value-vs-pointer-receivers/09-map-element-addressability
```

### Why the map holds pointers

Two facts collide here. First, a pointer-receiver method needs an *addressable*
receiver — `s.Touch()` compiles only if Go can take `&s`. Second, an element read
out of a map is *not* addressable: `&m["id"]` is illegal, and consequently
`m["id"].Touch()` does not compile when `m` is a `map[string]Session` and `Touch`
has a pointer receiver. Go cannot even auto-address it, because map internals may
move entries, so there is no stable address to hand out.

The tempting workaround — pull the value out, mutate it, and rely on the map — is
worse because it *does* compile and silently loses the write:

```go
// map[string]Session: the mutation is lost.
sessions := map[string]Session{"a": {ID: "a"}}
s := sessions["a"]          // s is a COPY of the stored value
s.LastSeen = time.Now()     // mutates the copy
_ = sessions["a"].LastSeen  // still the zero time: the map was never updated
// sessions["a"].LastSeen = time.Now()  // does not even compile: not addressable
```

The fix is to store *pointers*: `map[string]*Session`. Now the map element is a
pointer — copying it out copies the pointer, not the session — and the value it
points at lives on the heap at a stable address, so `s.Touch()` mutates the one
shared session every reference sees. This is the concrete reason a store of
mutable structs is almost always `map[K]*V`. The store also injects its clock as a
`now func() time.Time` so timestamp assertions are deterministic without touching
the wall clock — the clock-injection pattern discussed in `00-concepts.md`, done
here with a plain function field.

Create `store.go`:

```go
package sessionstore

import (
	"sync"
	"time"
)

// Session is a mutable session record. Touch has a pointer receiver because it
// updates LastSeen in place.
type Session struct {
	ID       string
	LastSeen time.Time
}

// Touch records that the session was just seen.
func (s *Session) Touch(now time.Time) {
	s.LastSeen = now
}

// SessionStore holds sessions BY POINTER so their structs stay addressable and
// pointer-receiver methods mutate the stored value, not a copy.
type SessionStore struct {
	mu       sync.Mutex
	sessions map[string]*Session
	now      func() time.Time
}

// NewSessionStore builds a store with an injected clock (use time.Now in prod).
func NewSessionStore(now func() time.Time) *SessionStore {
	return &SessionStore{
		sessions: make(map[string]*Session),
		now:      now,
	}
}

// Add creates and stores a session, stamping LastSeen with the current clock.
func (st *SessionStore) Add(id string) *Session {
	st.mu.Lock()
	defer st.mu.Unlock()
	s := &Session{ID: id, LastSeen: st.now()}
	st.sessions[id] = s
	return s
}

// Get returns the stored *Session and whether it exists.
func (st *SessionStore) Get(id string) (*Session, bool) {
	st.mu.Lock()
	defer st.mu.Unlock()
	s, ok := st.sessions[id]
	return s, ok
}

// Touch advances the session's LastSeen to now. It works because the map holds a
// *Session: the fetched pointer addresses the shared, mutable session.
func (st *SessionStore) Touch(id string) bool {
	st.mu.Lock()
	defer st.mu.Unlock()
	s, ok := st.sessions[id]
	if !ok {
		return false
	}
	s.Touch(st.now())
	return true
}

// Revoke removes a session.
func (st *SessionStore) Revoke(id string) {
	st.mu.Lock()
	defer st.mu.Unlock()
	delete(st.sessions, id)
}
```

### The runnable demo

The demo uses a manually-advanced clock so the output is deterministic: it adds a
session at t=1000, advances to t=2000, touches it, and prints the updated
last-seen, then revokes it.

Create `cmd/demo/main.go`:

```go
package main

import (
	"fmt"
	"time"

	"example.com/sessionstore"
)

func main() {
	current := time.Unix(1000, 0).UTC()
	clock := func() time.Time { return current }

	store := sessionstore.NewSessionStore(clock)
	store.Add("sess-1")

	if s, ok := store.Get("sess-1"); ok {
		fmt.Printf("added at:   %d\n", s.LastSeen.Unix())
	}

	current = time.Unix(2000, 0).UTC() // advance the clock
	store.Touch("sess-1")

	if s, ok := store.Get("sess-1"); ok {
		fmt.Printf("touched at: %d\n", s.LastSeen.Unix())
	}

	store.Revoke("sess-1")
	if _, ok := store.Get("sess-1"); !ok {
		fmt.Println("revoked:    gone")
	}
}
```

Run it:

```bash
go run ./cmd/demo
```

Expected output:

```
added at:   1000
touched at: 2000
revoked:    gone
```

### Tests

`TestTouchUpdatesLastSeen` advances the injected clock and asserts the stored
session moved with it — proof that the pointer map mutates in place.
`TestValueMapCopyDoesNotPersist` documents the trap directly on a local
`map[string]Session`: a fetched copy's mutation never reaches the map.
`TestRevokeRemovesSession` checks deletion.

Create `store_test.go`:

```go
package sessionstore

import (
	"testing"
	"time"
)

func TestTouchUpdatesLastSeen(t *testing.T) {
	t.Parallel()

	current := time.Unix(1000, 0).UTC()
	store := NewSessionStore(func() time.Time { return current })
	store.Add("a")

	current = time.Unix(2000, 0).UTC() // advance the clock
	if !store.Touch("a") {
		t.Fatal("Touch returned false for an existing session")
	}

	s, ok := store.Get("a")
	if !ok {
		t.Fatal("session vanished after Touch")
	}
	if !s.LastSeen.Equal(time.Unix(2000, 0).UTC()) {
		t.Fatalf("LastSeen = %d, want 2000 (Touch did not persist through the pointer)", s.LastSeen.Unix())
	}
}

func TestValueMapCopyDoesNotPersist(t *testing.T) {
	t.Parallel()

	// A map of VALUES: mutating a fetched copy is thrown away. This documents
	// exactly why SessionStore uses map[string]*Session instead.
	byValue := map[string]Session{"a": {ID: "a"}}

	// s is a COPY of the stored value; mutating it never reaches the map.
	s := byValue["a"]
	s.LastSeen = time.Unix(2000, 0).UTC()

	if got := byValue["a"].LastSeen; !got.IsZero() {
		t.Fatalf("value-map entry changed to %d; a copy mutation must not persist", got.Unix())
	}
}

func TestTouchMissingReturnsFalse(t *testing.T) {
	t.Parallel()

	store := NewSessionStore(func() time.Time { return time.Unix(1000, 0).UTC() })
	if store.Touch("nope") {
		t.Fatal("Touch of a missing session returned true")
	}
}

func TestRevokeRemovesSession(t *testing.T) {
	t.Parallel()

	store := NewSessionStore(func() time.Time { return time.Unix(1000, 0).UTC() })
	store.Add("a")
	store.Revoke("a")

	if _, ok := store.Get("a"); ok {
		t.Fatal("session still present after Revoke")
	}
}
```

## Review

The store is correct when `Touch` moves a stored session's `LastSeen` forward and
`TestValueMapCopyDoesNotPersist` confirms the opposite would silently fail on a
value map. The single design decision that makes it work is `map[string]*Session`
over `map[string]Session`: pointers keep the sessions addressable and shared, so
pointer-receiver methods mutate the real thing. The injected `now func() time.Time`
is the other production habit worth keeping — it makes every timestamp assertion
deterministic without a real sleep. Reach for `map[K]*V` whenever the values are
mutable structs you update in place.

## Resources

- [Go Spec: Address operators](https://go.dev/ref/spec#Address_operators) — why map elements are not addressable.
- [Go Spec: Calls](https://go.dev/ref/spec#Calls) — the automatic `&x` for a pointer method requires `x` to be addressable.
- [Go Spec: Index expressions](https://go.dev/ref/spec#Index_expressions) — a map index yields a value, not an addressable element.

---

Back to [00-concepts.md](00-concepts.md) | Prev: [08-interface-method-set-repository.md](08-interface-method-set-repository.md) | Next: [10-large-struct-copy-benchmark.md](10-large-struct-copy-benchmark.md)
