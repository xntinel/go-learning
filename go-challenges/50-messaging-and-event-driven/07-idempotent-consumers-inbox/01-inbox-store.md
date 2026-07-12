# Exercise 1: Inbox Store with Unique-Key Deduplication

The inbox pattern lives or dies on one operation: an *atomic claim* of a message
id that reports whether this caller is the first to see it. This exercise builds
that operation as a concurrency-safe store that models a database inbox table with
a `UNIQUE (consumer_group, message_id)` constraint — the in-memory analogue of
`INSERT ... ON CONFLICT DO NOTHING`.

This module is fully self-contained. It has its own `go mod init`, defines every
type it needs, and ships its own demo and tests. Nothing here imports any other
exercise.

## What you'll build

```text
inbox/                     independent module: example.com/inbox
  go.mod                   go 1.26
  inbox.go                 type Store; Record, ConsumedAt, Len, Prune; ErrDuplicate
  cmd/
    demo/
      main.go              runnable demo: first vs duplicate, per-group scoping
  inbox_test.go            table + concurrency (-race) + scoping tests, Example
```

- Files: `inbox.go`, `cmd/demo/main.go`, `inbox_test.go`.
- Implement: a `Store` with `Record(consumerGroup, messageID) (firstSeen bool, err error)` guarded by a `sync.RWMutex`, keyed by the composite `(consumerGroup, messageID)`, recording a consumed-at timestamp, plus `ConsumedAt`, `Len`, and `Prune`; a package-level sentinel `ErrDuplicate`.
- Test: distinct keys all report `firstSeen=true`; a repeat reports `firstSeen=false` and `ErrDuplicate` via `errors.Is`; N concurrent claims of one key yield exactly one `firstSeen` under `-race`; the same id under two groups both succeed.
- Verify: `go test -count=1 -race ./...`

Set up the module:

```bash
go mod edit -go=1.26
```

### Why the claim is atomic, not check-then-write

The whole point of the store is to be correct when two goroutines claim the *same*
id at the same moment — the exact situation a broker creates on a rebalance or a
visibility-timeout expiry. The tempting shape, `if !Has(id) { Put(id) }`, has a
time-of-check-to-time-of-use gap: both goroutines run `Has`, both see the id
missing, both `Put`, and both believe they were first. The double-apply follows.

The fix is to fuse the read and the write into one critical section. `Record` takes
the write lock, checks the map, and either inserts (returning `firstSeen=true`) or
reports the duplicate (returning `firstSeen=false, ErrDuplicate`) — all before
releasing the lock. No other goroutine can observe the map between the check and
the insert, so exactly one caller per key ever gets `firstSeen=true`. This is
precisely what a `UNIQUE` index does in a database: the engine serializes the
competing inserts and lets exactly one win. The lock here plays the role of that
constraint.

The store returns *both* a `firstSeen` boolean and an error on purpose. Callers
that prefer boolean control flow branch on `firstSeen`; callers that prefer
error-driven flow test `errors.Is(err, ErrDuplicate)`. The two encode the same
fact — a duplicate is `(false, ErrDuplicate)`, a first sighting is `(true, nil)`.

### The composite key scopes dedup per consumer group

The map key is a two-field struct, `{group, messageID}`, not the message id alone.
That is what makes the same message processable once *per subscriber*: the billing
group and the analytics group each get their own `firstSeen=true` for the same id,
because their keys differ in the `group` field. A global dedup keyed on id alone
would let whichever group recorded first hide the message from the other forever.

`ConsumedAt` reads through the *read* lock (`RLock`), so many readers can inspect
timestamps concurrently while only claims take the exclusive lock. `Prune` deletes
rows whose consumed-at instant is before a cutoff; it is the retention job that
keeps the inbox from growing without bound, sized in production to exceed the
broker's maximum redelivery window.

Create `inbox.go`:

```go
package inbox

import (
	"errors"
	"fmt"
	"sync"
	"time"
)

// ErrDuplicate reports that a (consumerGroup, messageID) pair was already
// recorded. Callers that prefer error-driven flow test it with errors.Is.
var ErrDuplicate = errors.New("inbox: duplicate message")

// key is the composite dedup key. Scoping by consumerGroup lets independent
// subscribers of the same topic each process a message id once.
type key struct {
	group     string
	messageID string
}

type record struct {
	consumedAt time.Time
}

// Store models a database inbox table with a UNIQUE constraint on
// (consumer_group, message_id). It is safe for concurrent use.
type Store struct {
	mu   sync.RWMutex
	seen map[key]record
}

// New returns an empty Store.
func New() *Store {
	return &Store{seen: make(map[key]record)}
}

// Record atomically claims (consumerGroup, messageID). It returns
// firstSeen=true exactly once per pair, with a nil error; every later call for
// the same pair returns firstSeen=false wrapping ErrDuplicate. The check and the
// insert happen under one write lock, which is the in-memory analogue of
// INSERT ... ON CONFLICT DO NOTHING against a UNIQUE index: exactly one writer
// wins even under concurrent redelivery.
func (s *Store) Record(consumerGroup, messageID string) (firstSeen bool, err error) {
	k := key{group: consumerGroup, messageID: messageID}
	s.mu.Lock()
	defer s.mu.Unlock()
	if _, ok := s.seen[k]; ok {
		return false, fmt.Errorf("%s/%s: %w", consumerGroup, messageID, ErrDuplicate)
	}
	s.seen[k] = record{consumedAt: time.Now()}
	return true, nil
}

// ConsumedAt returns when the pair was recorded, and whether it exists. It reads
// under the shared read lock so inspection does not block other readers.
func (s *Store) ConsumedAt(consumerGroup, messageID string) (time.Time, bool) {
	s.mu.RLock()
	defer s.mu.RUnlock()
	r, ok := s.seen[key{group: consumerGroup, messageID: messageID}]
	return r.consumedAt, ok
}

// Len reports how many pairs are currently recorded.
func (s *Store) Len() int {
	s.mu.RLock()
	defer s.mu.RUnlock()
	return len(s.seen)
}

// Prune deletes every record consumed before the cutoff and returns how many it
// removed. It is the retention job; size the cutoff to exceed the broker's
// maximum redelivery window, or a late redelivery of a pruned id double-applies.
func (s *Store) Prune(before time.Time) int {
	s.mu.Lock()
	defer s.mu.Unlock()
	n := 0
	for k, r := range s.seen {
		if r.consumedAt.Before(before) {
			delete(s.seen, k)
			n++
		}
	}
	return n
}
```

### The runnable demo

The demo claims one id twice (first vs duplicate), then claims the *same* id under
a second consumer group to show that dedup is scoped, not global.

Create `cmd/demo/main.go`:

```go
package main

import (
	"errors"
	"fmt"

	"example.com/inbox"
)

func main() {
	s := inbox.New()

	first, _ := s.Record("billing", "order-42")
	fmt.Printf("billing first sighting: %v\n", first)

	dup, err := s.Record("billing", "order-42")
	fmt.Printf("billing redelivery: firstSeen=%v duplicate=%v\n", dup, errors.Is(err, inbox.ErrDuplicate))

	other, _ := s.Record("analytics", "order-42")
	fmt.Printf("analytics first sighting (same id): %v\n", other)

	fmt.Printf("recorded pairs: %d\n", s.Len())
}
```

Run it:

```bash
go run ./cmd/demo
```

Expected output:

```
billing first sighting: true
billing redelivery: firstSeen=false duplicate=true
analytics first sighting (same id): true
recorded pairs: 2
```

### Tests

`TestRecord` is table-driven: distinct pairs each report `firstSeen=true`, and a
replay of a pair reports `firstSeen=false` with `ErrDuplicate`. `TestScopedByGroup`
proves the same id under two groups both succeed. `TestConcurrentSameKey` is the
important one: it launches N goroutines that all claim one id, counts the
`firstSeen=true` results with an `atomic.Int64`, and asserts the count is exactly
one — run under `-race`, a missing or wrong lock fails here. `TestPrune` records two
rows and checks that a cutoff after them drops both while a cutoff before them drops
none.

Create `inbox_test.go`:

```go
package inbox

import (
	"errors"
	"fmt"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

func TestRecord(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name      string
		group     string
		id        string
		preRecord bool
		wantFirst bool
		wantDup   bool
	}{
		{name: "first sighting", group: "billing", id: "m1", wantFirst: true},
		{name: "distinct id", group: "billing", id: "m2", wantFirst: true},
		{name: "duplicate", group: "billing", id: "m3", preRecord: true, wantFirst: false, wantDup: true},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			s := New()
			if tc.preRecord {
				if first, err := s.Record(tc.group, tc.id); !first || err != nil {
					t.Fatalf("pre-record Record = %v, %v; want true, nil", first, err)
				}
			}
			first, err := s.Record(tc.group, tc.id)
			if first != tc.wantFirst {
				t.Errorf("firstSeen = %v; want %v", first, tc.wantFirst)
			}
			if tc.wantDup {
				if !errors.Is(err, ErrDuplicate) {
					t.Errorf("err = %v; want ErrDuplicate", err)
				}
			} else if err != nil {
				t.Errorf("err = %v; want nil", err)
			}
		})
	}
}

func TestScopedByGroup(t *testing.T) {
	t.Parallel()
	s := New()
	if first, err := s.Record("billing", "shared-id"); !first || err != nil {
		t.Fatalf("billing Record = %v, %v; want true, nil", first, err)
	}
	first, err := s.Record("analytics", "shared-id")
	if !first || err != nil {
		t.Fatalf("analytics Record = %v, %v; want true, nil (dedup is per group)", first, err)
	}
}

func TestConcurrentSameKey(t *testing.T) {
	t.Parallel()
	s := New()
	const n = 200
	var firsts atomic.Int64
	var wg sync.WaitGroup
	for range n {
		wg.Add(1)
		go func() {
			defer wg.Done()
			if first, _ := s.Record("billing", "hot-id"); first {
				firsts.Add(1)
			}
		}()
	}
	wg.Wait()
	if got := firsts.Load(); got != 1 {
		t.Fatalf("firstSeen count = %d; want exactly 1", got)
	}
	if got := s.Len(); got != 1 {
		t.Fatalf("Len = %d; want 1", got)
	}
}

func TestPrune(t *testing.T) {
	t.Parallel()
	s := New()
	s.Record("billing", "a")
	s.Record("billing", "b")

	if got := s.Prune(time.Now().Add(-time.Hour)); got != 0 {
		t.Errorf("Prune(past) removed %d; want 0", got)
	}
	if got := s.Prune(time.Now().Add(time.Hour)); got != 2 {
		t.Errorf("Prune(future) removed %d; want 2", got)
	}
	if got := s.Len(); got != 0 {
		t.Errorf("Len after prune = %d; want 0", got)
	}
}

func Example() {
	s := New()
	first, _ := s.Record("billing", "msg-1")
	dup, err := s.Record("billing", "msg-1")
	fmt.Println(first, dup, errors.Is(err, ErrDuplicate))
	// Output: true false true
}

// Your turn: add a test that ConsumedAt returns a timestamp that is not before a
// time captured just before the Record call, and (time.Time{}, false) for an id
// that was never recorded.
```

## Review

The store is correct when the `firstSeen=true` result is unique per composite key
under any interleaving. The proof is `TestConcurrentSameKey`: 200 goroutines race
to claim one id, and exactly one wins. If you had written `Has` then `Put` as two
locked calls, two goroutines would slip between them and the count would exceed one
under `-race` — the failure is the whole reason the check and insert share a single
critical section. The second proof is `TestScopedByGroup`: dedup is a property of
`(group, id)`, so the same id under a different group is a genuinely new claim, not
a duplicate.

The mistakes to avoid are the ones the concepts named. Do not split the check from
the insert — even two `RLock`/`Lock` calls reopen the TOCTOU gap. Do not key on the
message id alone, or a second consumer group can never process the message. Do not
forget retention: `Prune` exists because an unbounded inbox eventually dominates
write latency, and its cutoff must stay outside the broker's redelivery window or a
late redelivery of a pruned id will double-apply. Run `go test -race` to confirm
the lock actually serializes concurrent claims.

## Resources

- [Idempotent Consumer pattern - microservices.io](https://microservices.io/patterns/communication-style/idempotent-consumer.html) — the composite `(subscriber_id, message_id)` key and its rationale.
- [database/sql Tx](https://pkg.go.dev/database/sql#Tx) — how the real inbox row and domain mutation share one transaction.
- [sync.RWMutex](https://pkg.go.dev/sync#RWMutex) — the read/write lock backing the store's atomic claim.

---

Back to [00-concepts.md](00-concepts.md) | Next: [02-idempotent-consumer.md](02-idempotent-consumer.md)
