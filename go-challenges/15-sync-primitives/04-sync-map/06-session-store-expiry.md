# Exercise 6: Session store with concurrent lookups and an expiry sweeper

An in-memory session store is read-mostly on the hot path (every authenticated
request looks up its session) with a background sweeper reclaiming expired entries.
The subtle bug is the sweeper racing a refresh: it decides a session is expired,
then deletes it a moment after a request extended its lifetime, silently logging
the user out. This module builds the store and fixes that race with `LoadAndDelete`
— atomic remove-and-return — so the sweeper only drops the exact version it
inspected.

This module is fully self-contained: its own `go mod init`, all code inline, its
own demo and tests. Nothing here imports any other exercise.

## What you'll build

```text
sessionstore/                 independent module: example.com/sessionstore
  go.mod                      go 1.26
  store.go                    type Session, type Store; Put, Get, Sweep
  cmd/
    demo/
      main.go                 runnable demo: put live+expired, sweep, observe
  store_test.go               sweep-removes-only-expired, refresh-not-clobbered race, Example
```

- Files: `store.go`, `cmd/demo/main.go`, `store_test.go`.
- Implement: `Store` with `Put(id string, s Session)`, `Get(id string) (Session, bool)` (expired treated as absent), `Sweep(now time.Time) int`.
- Test: `Sweep` removes only expired entries; a sweeper racing `Put`/`Get` never drops a session refreshed past its original expiry.
- Verify: `go test -count=1 -race ./...`

Set up the module:

```bash
mkdir go-solutions/15-sync-primitives/04-sync-map/06-session-store-expiry && cd go-solutions/15-sync-primitives/04-sync-map/06-session-store-expiry
```

### Why LoadAndDelete, not Delete, in the sweeper

`Get` treats a session whose `ExpiresAt` is not after now as absent (lazy expiry) —
it does not delete, it just declines to return it. Reclaiming the memory is the
sweeper's job. The naive sweeper ranges the map and calls `Delete(id)` on anything
that looks expired. The race: between the moment `Range` hands the sweeper an
expired-looking session and the moment the sweeper calls `Delete`, a live request
can `Put` a refreshed session under the same id with a later expiry. A blind
`Delete` drops that refreshed session — the user is logged out by a sweeper acting
on stale information.

`LoadAndDelete(id)` closes the window. It atomically removes the key *and* returns
the value it removed, so the sweeper can inspect exactly what it deleted. If the
returned value is still expired, the delete was correct and the entry is counted as
swept. If the returned value turns out to be live — a writer refreshed it in the
race window, so `LoadAndDelete` removed the *refreshed* session — the sweeper
immediately `Store`s it back, undoing a deletion it should never have made. The
result: the sweeper only truly removes sessions that were expired at the instant it
removed them, and a refresh that lands anywhere in the race is never silently lost.

This is the general shape of read-mostly-with-background-eviction, and the reason
`LoadAndDelete` exists over plain `Delete`: whenever a concurrent writer can
resurrect a key you are about to evict, you need the atomic remove-and-return to
decide, after the fact, whether the eviction was still correct.

Create `store.go`:

```go
package sessionstore

import (
	"sync"
	"time"
)

// Session is a stored login session. It is treated as absent once now is no
// longer before ExpiresAt.
type Session struct {
	UserID    string
	ExpiresAt time.Time
}

func (s Session) expired(now time.Time) bool {
	return !now.Before(s.ExpiresAt)
}

// Store is a concurrency-safe session store: a hot read path (Get) plus a
// background sweeper (Sweep) that reclaims expired entries without clobbering a
// concurrently-refreshed session.
type Store struct {
	sessions sync.Map // map[string]Session
}

// NewStore returns an empty store ready for concurrent use.
func NewStore() *Store {
	return &Store{}
}

// Put stores or refreshes the session for id.
func (s *Store) Put(id string, sess Session) {
	s.sessions.Store(id, sess)
}

// Get returns the session for id if present and not expired. An expired session
// reports (zero, false) but is left for the sweeper to reclaim.
func (s *Store) Get(id string) (Session, bool) {
	v, ok := s.sessions.Load(id)
	if !ok {
		return Session{}, false
	}
	sess := v.(Session)
	if sess.expired(time.Now()) {
		return Session{}, false
	}
	return sess, true
}

// Sweep removes every session expired as of now and returns how many it removed.
// It uses LoadAndDelete so a session refreshed during the sweep is restored
// rather than silently dropped.
func (s *Store) Sweep(now time.Time) int {
	removed := 0
	s.sessions.Range(func(key, value any) bool {
		if !value.(Session).expired(now) {
			return true // still live as seen by Range; leave it
		}
		id := key.(string)
		got, loaded := s.sessions.LoadAndDelete(id)
		if !loaded {
			return true // already deleted by someone else
		}
		if sess := got.(Session); !sess.expired(now) {
			// A writer refreshed it in the race window; put it back.
			s.sessions.Store(id, sess)
			return true
		}
		removed++
		return true
	})
	return removed
}
```

### The runnable demo

The demo pins `now` to a fixed instant so the output is deterministic: it stores
one live and two expired sessions, sweeps, and reports how many were removed and
which survive.

Create `cmd/demo/main.go`:

```go
package main

import (
	"fmt"
	"time"

	"example.com/sessionstore"
)

func main() {
	// Anchor far in the future: Get checks expiry against the wall clock, so a
	// past anchor would make every "live" session read as expired.
	now := time.Date(2100, 1, 1, 12, 0, 0, 0, time.UTC)
	s := sessionstore.NewStore()
	s.Put("live", sessionstore.Session{UserID: "alice", ExpiresAt: now.Add(time.Hour)})
	s.Put("stale1", sessionstore.Session{UserID: "bob", ExpiresAt: now.Add(-time.Minute)})
	s.Put("stale2", sessionstore.Session{UserID: "carol", ExpiresAt: now.Add(-time.Hour)})

	removed := s.Sweep(now)
	fmt.Println("removed:", removed)

	if _, ok := s.Get("live"); ok {
		fmt.Println("live session survives")
	}
	if _, ok := s.Get("stale1"); !ok {
		fmt.Println("stale1 gone")
	}
}
```

Run it:

```bash
go run ./cmd/demo
```

Expected output:

```
removed: 2
live session survives
stale1 gone
```

### Tests

`TestSweepRemovesOnlyExpired` populates a mix and asserts `Sweep` removes exactly
the expired ones and leaves the live ones. `TestRefreshNotClobbered` is the race
contract: a sweeper runs concurrently with a refresh (`Put` with a future expiry)
on the same key that started expired; across many iterations, whenever the refresh
lands, the session must still be present afterward — the sweeper must never
silently drop a refreshed session. `-race` proves the concurrent `Range`/`Put`/
`LoadAndDelete` path is clean.

Create `store_test.go`:

```go
package sessionstore

import (
	"fmt"
	"sync"
	"testing"
	"time"
)

func TestSweepRemovesOnlyExpired(t *testing.T) {
	t.Parallel()

	// Far-future anchor: Get compares against the wall clock, so the live
	// sessions below must expire after the real time this test runs.
	now := time.Date(2100, 1, 1, 0, 0, 0, 0, time.UTC)
	s := NewStore()
	s.Put("live1", Session{UserID: "a", ExpiresAt: now.Add(time.Hour)})
	s.Put("live2", Session{UserID: "b", ExpiresAt: now.Add(time.Minute)})
	s.Put("dead1", Session{UserID: "c", ExpiresAt: now.Add(-time.Second)})
	s.Put("dead2", Session{UserID: "d", ExpiresAt: now.Add(-time.Hour)})

	if got := s.Sweep(now); got != 2 {
		t.Fatalf("Sweep removed %d, want 2", got)
	}
	for _, id := range []string{"live1", "live2"} {
		if _, ok := s.Get(id); !ok {
			t.Errorf("live session %q was swept", id)
		}
	}
	for _, id := range []string{"dead1", "dead2"} {
		if _, ok := s.sessions.Load(id); ok {
			t.Errorf("expired session %q survived the sweep", id)
		}
	}
}

func TestRefreshNotClobbered(t *testing.T) {
	t.Parallel()

	base := time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)
	const iterations = 500

	for range iterations {
		s := NewStore()
		// Start expired as of base.
		s.Put("sess", Session{UserID: "u", ExpiresAt: base.Add(-time.Minute)})

		var wg sync.WaitGroup
		wg.Add(2)
		// Sweeper acts as of base (the original session is expired).
		go func() {
			defer wg.Done()
			s.Sweep(base)
		}()
		// Concurrent refresh extends the session well past base.
		go func() {
			defer wg.Done()
			s.Put("sess", Session{UserID: "u", ExpiresAt: base.Add(time.Hour)})
		}()
		wg.Wait()

		// The refresh always writes a live session (expiry base+1h). Whatever the
		// interleaving, a refreshed session must never be silently dropped: the
		// final stored value must be the live one.
		v, ok := s.sessions.Load("sess")
		if !ok {
			t.Fatalf("iteration lost the refreshed session entirely")
		}
		if got := v.(Session); got.expired(base) {
			t.Fatalf("final session expired=%v; refresh was clobbered by the sweeper", got)
		}
	}
}

func ExampleStore() {
	now := time.Date(2026, 1, 1, 12, 0, 0, 0, time.UTC)
	s := NewStore()
	s.Put("a", Session{UserID: "alice", ExpiresAt: now.Add(time.Hour)})
	s.Put("b", Session{UserID: "bob", ExpiresAt: now.Add(-time.Minute)})

	fmt.Println("removed:", s.Sweep(now))
	_, ok := s.sessions.Load("a")
	fmt.Println("a present:", ok)
	// Output:
	// removed: 1
	// a present: true
}
```

## Review

The store is correct when expiry is lazy on the read path and the sweeper never
drops a session that a concurrent writer refreshed. The mechanism is
`LoadAndDelete`: by removing and returning atomically, the sweeper can check
whether the value it actually deleted was still expired, and restore it if a
refresh had slipped in. `TestRefreshNotClobbered` runs the race hundreds of times
and asserts the refreshed session is never lost — the invariant a plain `Delete`
would violate. The traps to avoid are using `Delete` (which cannot tell you what it
removed, so it clobbers refreshes) and assuming `Range` is a snapshot (a session
can be refreshed after `Range` reads it, which is precisely why the sweeper
re-checks the `LoadAndDelete` result). Run `go test -race` to confirm the
concurrent sweep-versus-refresh path is race-free.

## Resources

- [sync.Map.LoadAndDelete](https://pkg.go.dev/sync#Map.LoadAndDelete) — atomic remove-and-return, added in Go 1.15.
- [sync.Map.Range](https://pkg.go.dev/sync#Map.Range) — best-effort iteration for the sweep.
- [time.Time.Before / After](https://pkg.go.dev/time#Time.Before) — the expiry comparison.

---

Back to [05-metrics-registry.md](05-metrics-registry.md) | Next: [07-versioned-config-cas.md](07-versioned-config-cas.md)
