# Exercise 9: Optimistic Concurrency with a Version Field

When two workers load the same entity, both change it, and both save, the second
save silently overwrites the first — a lost update. This module builds the fix
every SQL and event-sourced system uses: a monotonic `Version` field and an
`Update` that is *conditional* on the version the caller last saw, rejecting a
stale write with `ErrVersionConflict` instead of clobbering.

This module is fully self-contained: its own `go mod init`, all code inline, its
own demo and tests. Nothing here imports another exercise.

## What you'll build

```text
versioned/                  independent module: example.com/versioned
  go.mod                    go 1.26
  store.go                  type Document (id/body/version); type Store; Create/Get/Update
  cmd/
    demo/
      main.go               runnable demo: create, update, stale update rejected
  store_test.go             tests: conflict on stale version, not-found, value-copy, concurrent updates
```

- Files: `store.go`, `cmd/demo/main.go`, `store_test.go`.
- Implement: a `Document` with an `id`/`body`/`version`, and a `Store` guarded by a `sync.Mutex` with `Create`, `Get` (returns a copy), and `Update(id, expectedVersion, body)` that returns `ErrVersionConflict` on mismatch and bumps the version on success.
- Test: two loads at version N, the first `Update` succeeds and bumps to N+1, the second with stale N returns `ErrVersionConflict`; `Update` of a missing id returns `ErrNotFound`; a fetched value is a copy, not an alias; concurrent updates run clean under `-race`.
- Verify: `go test -count=1 -race ./...`

### The conditional update is the whole mechanism

Optimistic concurrency assumes conflicts are rare and pays for that assumption
only when one actually happens, rather than holding a lock across the whole
read-modify-write. Each `Document` carries a `version` that increments on every
successful write. A caller reads a document (learning its version), does its work,
and calls `Update(id, expectedVersion, newBody)` where `expectedVersion` is the
version it read. The store compares: if the stored version still equals
`expectedVersion`, no one wrote in between, so the update applies and the version
bumps to `expectedVersion+1`; if they differ, someone else wrote first, and the
update is rejected with `ErrVersionConflict` so the caller can reload and retry.
This is exactly `UPDATE docs SET body=?, version=version+1 WHERE id=? AND
version=?` — a row count of zero means conflict — and exactly an event store's
"append expecting version N" check.

The lost-update scenario is the reason this exists. Without the version check, two
workers both load version 1, both write, and the second `store[id] = ...` silently
discards the first worker's change with no error anywhere. The conditional update
converts that silent data loss into a visible, retryable `ErrVersionConflict` at
the exact moment it happens.

`Document` is a value type with unexported fields, so storing it in the map and
returning it from `Get` both copy it — there is no shared pointer through which a
caller could mutate the stored document behind the store's back. That is the
"return copies, not aliases" rule made automatic by value semantics: a `Document`
you fetched keeps the version it had even after someone else updates the store.
The map is guarded by a `sync.Mutex` because `Get` and `Update` both touch it and
must not race; `Store` therefore uses pointer receivers and must not be copied
(it holds the mutex).

Create `store.go`:

```go
package versioned

import (
	"errors"
	"fmt"
	"sync"
)

var (
	ErrNotFound        = errors.New("versioned: document not found")
	ErrVersionConflict = errors.New("versioned: version conflict (stale update)")
)

// Document is a value type: copying it (into or out of the store) is a full copy,
// so no caller shares mutable state with the store.
type Document struct {
	id      string
	body    string
	version int
}

func (d Document) ID() string   { return d.id }
func (d Document) Body() string { return d.body }
func (d Document) Version() int { return d.version }

// Store is an in-memory repository with optimistic-concurrency updates.
type Store struct {
	mu   sync.Mutex
	docs map[string]Document
}

func NewStore() *Store {
	return &Store{docs: make(map[string]Document)}
}

// Create inserts a new document at version 1.
func (s *Store) Create(id, body string) (Document, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	d := Document{id: id, body: body, version: 1}
	s.docs[id] = d
	return d, nil
}

// Get returns a copy of the stored document.
func (s *Store) Get(id string) (Document, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	d, ok := s.docs[id]
	if !ok {
		return Document{}, fmt.Errorf("%w: %q", ErrNotFound, id)
	}
	return d, nil
}

// Update applies newBody only if expectedVersion matches the stored version,
// bumping the version on success and returning ErrVersionConflict on mismatch.
func (s *Store) Update(id string, expectedVersion int, newBody string) (Document, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	cur, ok := s.docs[id]
	if !ok {
		return Document{}, fmt.Errorf("%w: %q", ErrNotFound, id)
	}
	if cur.version != expectedVersion {
		return Document{}, fmt.Errorf("%w: have %d, expected %d", ErrVersionConflict, cur.version, expectedVersion)
	}
	updated := Document{id: id, body: newBody, version: cur.version + 1}
	s.docs[id] = updated
	return updated, nil
}
```

### The runnable demo

The demo simulates two workers that both loaded version 1: the first update wins,
the second is rejected as stale.

Create `cmd/demo/main.go`:

```go
package main

import (
	"errors"
	"fmt"

	"example.com/versioned"
)

func main() {
	store := versioned.NewStore()
	store.Create("doc-1", "draft")

	// Both workers load version 1.
	a, _ := store.Get("doc-1")
	b, _ := store.Get("doc-1")

	updated, _ := store.Update("doc-1", a.Version(), "worker A edit")
	fmt.Printf("worker A wrote version %d\n", updated.Version())

	if _, err := store.Update("doc-1", b.Version(), "worker B edit"); errors.Is(err, versioned.ErrVersionConflict) {
		fmt.Println("worker B rejected: stale version")
	}

	final, _ := store.Get("doc-1")
	fmt.Printf("final: %q at version %d\n", final.Body(), final.Version())
}
```

Run it:

```bash
go run ./cmd/demo
```

Expected output:

```
worker A wrote version 2
worker B rejected: stale version
final: "worker A edit" at version 2
```

### Tests

The tests pin the concurrency contract. The lost-update test loads twice at version
1, updates once (bumping to 2), then updates again with the stale version 1 and
asserts `ErrVersionConflict`. The not-found test updates a missing id. The
value-copy test fetches a document, updates the store, and asserts the earlier copy
still reads the old version — proof `Get` returned a copy, not an alias. The
concurrency test fires many goroutines that each read-then-update in a retry loop;
under `-race` it must be clean, and every increment must be accounted for.

Create `store_test.go`:

```go
package versioned

import (
	"errors"
	"sync"
	"testing"
)

func TestLostUpdateRejected(t *testing.T) {
	t.Parallel()
	s := NewStore()
	s.Create("doc-1", "draft")

	a, _ := s.Get("doc-1")
	b, _ := s.Get("doc-1")
	if a.Version() != 1 || b.Version() != 1 {
		t.Fatalf("both loads should be version 1, got %d and %d", a.Version(), b.Version())
	}

	got, err := s.Update("doc-1", a.Version(), "A")
	if err != nil {
		t.Fatal(err)
	}
	if got.Version() != 2 {
		t.Fatalf("after update version = %d, want 2", got.Version())
	}

	if _, err := s.Update("doc-1", b.Version(), "B"); !errors.Is(err, ErrVersionConflict) {
		t.Fatalf("stale update err = %v, want ErrVersionConflict", err)
	}
}

func TestUpdateMissingIsNotFound(t *testing.T) {
	t.Parallel()
	s := NewStore()
	if _, err := s.Update("nope", 1, "x"); !errors.Is(err, ErrNotFound) {
		t.Fatalf("err = %v, want ErrNotFound", err)
	}
}

func TestGetReturnsCopy(t *testing.T) {
	t.Parallel()
	s := NewStore()
	s.Create("doc-1", "draft")

	snapshot, _ := s.Get("doc-1")
	if _, err := s.Update("doc-1", 1, "changed"); err != nil {
		t.Fatal(err)
	}
	if snapshot.Version() != 1 || snapshot.Body() != "draft" {
		t.Fatalf("earlier Get was aliased to the store: %+v", snapshot)
	}
}

func TestConcurrentUpdates(t *testing.T) {
	t.Parallel()
	s := NewStore()
	s.Create("doc-1", "v")

	const workers = 50
	var wg sync.WaitGroup
	var successes int64
	var mu sync.Mutex
	for i := range workers {
		wg.Add(1)
		go func(n int) {
			defer wg.Done()
			for {
				cur, err := s.Get("doc-1")
				if err != nil {
					return
				}
				if _, err := s.Update("doc-1", cur.Version(), "w"); err == nil {
					mu.Lock()
					successes++
					mu.Unlock()
					return
				}
				// conflict: reload and retry
			}
		}(i)
	}
	wg.Wait()

	final, _ := s.Get("doc-1")
	if int64(final.Version()) != 1+successes {
		t.Fatalf("version %d != 1 + successes %d", final.Version(), successes)
	}
	if successes != workers {
		t.Fatalf("successes = %d, want %d (every worker retries until it wins)", successes, workers)
	}
}
```

## Review

The store is correct when `Update` writes only if the stored version matches the
caller's expectation, and every successful write bumps the version by exactly one.
The value-copy test proves `Get` hands out a snapshot, so a fetched document does
not mutate under the caller when the store moves on. The concurrency test, run
under `-race`, proves the mutex actually guards the map and that the retry loop
converges — every worker eventually wins exactly once, so the final version equals
one plus the number of successes. The mistakes to avoid: an unconditional
`store[id] = ...` (which silently loses updates), and returning a pointer into the
store (which would alias the caller to shared mutable state).

## Resources

- [`sync.Mutex`](https://pkg.go.dev/sync#Mutex) — guarding the store; must not be copied after first use.
- [Optimistic concurrency control (concept)](https://en.wikipedia.org/wiki/Optimistic_concurrency_control) — the version-check pattern this mirrors.
- [`errors.Is`](https://pkg.go.dev/errors#Is) — matching `ErrVersionConflict` and `ErrNotFound`.

---

Back to [00-concepts.md](00-concepts.md) | Previous: [08-order-state-machine-transitions.md](08-order-state-machine-transitions.md) | Next: [10-thread-safe-repository.md](10-thread-safe-repository.md)
