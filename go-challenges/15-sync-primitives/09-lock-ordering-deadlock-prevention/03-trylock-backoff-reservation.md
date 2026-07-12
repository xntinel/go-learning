# Exercise 3: Inventory Reservation with TryLock and Jittered Backoff

Sometimes there is no total order to lock by: two stock locations are
identified by opaque UUIDs owned by different teams, and nobody will renumber
them for your locking discipline. This exercise builds the other classical
deadlock fix — break hold-and-wait with `sync.Mutex.TryLock`, releasing
everything and retrying with jittered backoff — and is honest about what that
trades away.

This module is fully self-contained. It begins with its own `go mod init`,
defines every type it needs, and ships its own demo and tests. Nothing here
imports any other exercise.

## What you'll build

```text
stockreserve/              independent module: example.com/stockreserve
  go.mod
  warehouse/
    reserve.go             type Location; NewLocation, Available, Release;
                           ReserveBoth (lock, TryLock, back off, retry);
                           ErrContended, ErrInsufficientStock
  cmd/
    demo/
      main.go              runnable demo: reserve across two locations,
                           insufficient-stock error path
  warehouse/reserve_test.go pre-locked-location contention test (deterministic
                           ErrContended), opposite-order stress under -race
```

- Files: `warehouse/reserve.go`, `cmd/demo/main.go`, `warehouse/reserve_test.go`.
- Implement: `ReserveBoth(a, b, na, nb, retries, backoff)` that locks the first location, `TryLock`s the second, and on failure releases everything, sleeps a jittered backoff (`rand/v2.Int64N`), and retries up to a budget before returning `ErrContended`; a same-location fast path; `Release` to return units.
- Test: a deterministic contention test that pre-locks the second location and asserts `ErrContended` without hanging; a 2x500-iteration opposite-order stress test asserting stock conservation.
- Verify: `go test -count=1 -race ./...`

### Breaking hold-and-wait instead of circular wait

The ordered-locking discipline from the transfer engine assumed a total order
over the resources. Here the locations' IDs are opaque strings from two
different inventory systems; you *could* order lexically by UUID, and the
Review section argues you usually should — but suppose the IDs are not stable
(locations get re-keyed on warehouse migrations) or the two lock types live in
different packages that cannot know about each other. Then the remaining
classical fix is to never hold one lock while *blocking* on another:

1. Lock location A.
2. `TryLock` location B — a non-blocking attempt (Go 1.18+).
3. If it succeeds, both are held: validate stock, commit, unlock both.
4. If it fails, unlock A immediately, sleep a randomized backoff, retry.

No goroutine ever waits while holding, so the hold-and-wait Coffman condition
is broken and deadlock is structurally impossible, in any acquisition order.

What you bought deadlock-freedom with is *livelock* risk: two goroutines
reserving the same pair in opposite orders can each grab their first lock, each
fail the `TryLock`, each release, and — if they retry on the same schedule —
collide again, forever. Two things prevent that. Jitter: the backoff duration
includes a random component (`rand/v2.Int64N`), so the contenders desynchronize
and one of them wins within a retry or two with overwhelming probability.
A budget: retries are bounded, and exhaustion returns a typed `ErrContended`
that the caller can surface as an HTTP 503, a queue redelivery, or a
user-visible "try again" — turning pathological contention into an observable,
bounded error instead of an unbounded spin. An unjittered, unbounded
`for !mu.TryLock() {}` loop is the classic wrong version: it burns a core and
can livelock indefinitely.

The same-location guard reappears in a new costume: an order can ask for two
line items that live in the *same* location. `a == b` must take a single-lock
fast path reserving `na+nb` together — the ordered/TryLock path would lock the
same non-reentrant mutex twice and self-deadlock.

Note the shape of the stock checks: A's stock is validated while only A is
held (fail fast before touching B), but the *commit* — both decrements —
happens only when both locks are held, so a reservation is all-or-nothing.
Decrementing A before winning B would require a compensation path to undo the
decrement on failure; exercise 10 explores that design when it is actually
warranted.

Create `warehouse/reserve.go`:

```go
// Package warehouse reserves stock across locations whose identifiers are
// opaque strings with no agreed global ordering. Multi-location
// reservations use TryLock with jittered backoff to break hold-and-wait:
// no goroutine ever blocks on one location's lock while holding another.
package warehouse

import (
	"errors"
	"fmt"
	"math/rand/v2"
	"sync"
	"time"
)

var (
	// ErrContended reports that the retry budget was exhausted without
	// ever holding both locations at once. Callers map it to a retryable
	// failure (HTTP 503, queue redelivery), never to data corruption:
	// nothing has been reserved when it is returned.
	ErrContended = errors.New("warehouse: reservation contended")

	// ErrInsufficientStock reports that a location cannot cover the
	// requested units. Nothing has been reserved when it is returned.
	ErrInsufficientStock = errors.New("warehouse: insufficient stock")

	// ErrNonPositiveQuantity rejects zero and negative unit counts.
	ErrNonPositiveQuantity = errors.New("warehouse: non-positive quantity")
)

// Location is a stock location owned by an external inventory system.
// ID is opaque (a UUID): it is NOT a usable lock-ordering key.
type Location struct {
	ID    string
	mu    sync.Mutex
	units int
}

// NewLocation returns a location holding units of stock.
func NewLocation(id string, units int) *Location {
	return &Location{ID: id, units: units}
}

// Available returns the unreserved units, read under the lock.
func (l *Location) Available() int {
	l.mu.Lock()
	defer l.mu.Unlock()
	return l.units
}

// Release returns n units to the location (a cancelled or shipped
// reservation).
func (l *Location) Release(n int) {
	l.mu.Lock()
	defer l.mu.Unlock()
	l.units += n
}

// ReserveBoth atomically reserves na units at a and nb units at b: either
// both decrements happen or neither does. It holds a's lock and TryLocks
// b; on contention it releases a, sleeps a jittered backoff, and retries,
// returning ErrContended once retries are exhausted. baseBackoff must be
// positive; a non-positive value is replaced with 200 microseconds.
func ReserveBoth(a, b *Location, na, nb int, retries int, baseBackoff time.Duration) error {
	if na <= 0 || nb <= 0 {
		return fmt.Errorf("%w: na=%d nb=%d", ErrNonPositiveQuantity, na, nb)
	}
	if baseBackoff <= 0 {
		baseBackoff = 200 * time.Microsecond
	}

	// Same-location fast path: the order needs two line items from one
	// place. The two-lock path would lock the same non-reentrant mutex
	// twice and self-deadlock, so reserve the sum under a single lock.
	if a == b {
		a.mu.Lock()
		defer a.mu.Unlock()
		if a.units < na+nb {
			return fmt.Errorf("%w: location %s has %d, need %d", ErrInsufficientStock, a.ID, a.units, na+nb)
		}
		a.units -= na + nb
		return nil
	}

	for attempt := 0; attempt <= retries; attempt++ {
		a.mu.Lock()
		if a.units < na {
			have := a.units
			a.mu.Unlock()
			return fmt.Errorf("%w: location %s has %d, need %d", ErrInsufficientStock, a.ID, have, na)
		}
		if b.mu.TryLock() {
			// Both held: all-or-nothing commit.
			if b.units < nb {
				have := b.units
				b.mu.Unlock()
				a.mu.Unlock()
				return fmt.Errorf("%w: location %s has %d, need %d", ErrInsufficientStock, b.ID, have, nb)
			}
			a.units -= na
			b.units -= nb
			b.mu.Unlock()
			a.mu.Unlock()
			return nil
		}
		// Contended: release EVERYTHING before waiting (this is the whole
		// point), then back off with jitter so opposite-order contenders
		// desynchronize instead of livelocking in lockstep.
		a.mu.Unlock()
		time.Sleep(baseBackoff + time.Duration(rand.Int64N(int64(baseBackoff))))
	}
	return fmt.Errorf("%w: locations %s and %s after %d attempts", ErrContended, a.ID, b.ID, retries+1)
}
```

### The runnable demo

Create `cmd/demo/main.go`:

```go
package main

import (
	"fmt"
	"time"

	"example.com/stockreserve/warehouse"
)

func main() {
	east := warehouse.NewLocation("b7f9c2d4-east", 100)
	west := warehouse.NewLocation("a31e8f06-west", 40)

	if err := warehouse.ReserveBoth(east, west, 30, 25, 5, time.Millisecond); err != nil {
		fmt.Println("err:", err)
		return
	}
	fmt.Printf("after order 1: east=%d west=%d\n", east.Available(), west.Available())

	// Second order over-asks the west location: nothing is reserved.
	if err := warehouse.ReserveBoth(east, west, 10, 50, 5, time.Millisecond); err != nil {
		fmt.Println("order 2 rejected:", err)
	}
	fmt.Printf("after order 2: east=%d west=%d\n", east.Available(), west.Available())
}
```

Run it:

```bash
go run ./cmd/demo
```

Expected output:

```
after order 1: east=70 west=15
order 2 rejected: warehouse: insufficient stock: location a31e8f06-west has 15, need 50
after order 2: east=70 west=15
```

### Tests

`TestReserveBothContended` is the deterministic one: the test (same package,
so it can reach the unexported mutex) pre-locks the second location, calls
`ReserveBoth` with a three-retry budget and a tiny backoff, and asserts
`ErrContended` comes back promptly — no hang, and no stock deducted from the
first location. `TestOppositeOrderStress` is the livelock probe: two
goroutines reserve the same pair in *opposite* argument orders 500 times each,
releasing after every success and retrying on `ErrContended`. With TryLock
plus jitter this always completes; the final assertion checks stock
conservation, and the whole thing runs under `-race`. The insufficient-stock
table pins the all-or-nothing property: when the second location cannot cover
the request, the first location's stock is untouched.

Create `warehouse/reserve_test.go`:

```go
package warehouse

import (
	"errors"
	"fmt"
	"sync"
	"testing"
	"time"
)

func TestReserveBothSuccess(t *testing.T) {
	t.Parallel()

	a := NewLocation("loc-a", 10)
	b := NewLocation("loc-b", 10)
	if err := ReserveBoth(a, b, 3, 4, 3, 100*time.Microsecond); err != nil {
		t.Fatal(err)
	}
	if got := a.Available(); got != 7 {
		t.Errorf("a = %d, want 7", got)
	}
	if got := b.Available(); got != 6 {
		t.Errorf("b = %d, want 6", got)
	}
}

func TestReserveBothSameLocation(t *testing.T) {
	t.Parallel()

	a := NewLocation("loc-a", 10)
	if err := ReserveBoth(a, a, 3, 4, 3, 100*time.Microsecond); err != nil {
		t.Fatal(err)
	}
	if got := a.Available(); got != 3 {
		t.Errorf("a = %d, want 3", got)
	}
	// Over-asking the combined quantity fails without deducting.
	err := ReserveBoth(a, a, 2, 2, 3, 100*time.Microsecond)
	if !errors.Is(err, ErrInsufficientStock) {
		t.Fatalf("err = %v, want ErrInsufficientStock", err)
	}
	if got := a.Available(); got != 3 {
		t.Errorf("a = %d after failed reserve, want 3", got)
	}
}

func TestReserveBothErrors(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name    string
		aUnits  int
		bUnits  int
		na, nb  int
		wantErr error
	}{
		{name: "first short", aUnits: 2, bUnits: 10, na: 3, nb: 1, wantErr: ErrInsufficientStock},
		{name: "second short", aUnits: 10, bUnits: 2, na: 1, nb: 3, wantErr: ErrInsufficientStock},
		{name: "zero quantity", aUnits: 10, bUnits: 10, na: 0, nb: 1, wantErr: ErrNonPositiveQuantity},
		{name: "negative quantity", aUnits: 10, bUnits: 10, na: 1, nb: -1, wantErr: ErrNonPositiveQuantity},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()

			a := NewLocation("loc-a", tc.aUnits)
			b := NewLocation("loc-b", tc.bUnits)
			err := ReserveBoth(a, b, tc.na, tc.nb, 3, 100*time.Microsecond)
			if !errors.Is(err, tc.wantErr) {
				t.Fatalf("err = %v, want %v", err, tc.wantErr)
			}
			// All-or-nothing: a failed reservation deducts nothing.
			if got := a.Available(); got != tc.aUnits {
				t.Errorf("a = %d after failure, want %d", got, tc.aUnits)
			}
			if got := b.Available(); got != tc.bUnits {
				t.Errorf("b = %d after failure, want %d", got, tc.bUnits)
			}
		})
	}
}

func TestReserveBothContended(t *testing.T) {
	t.Parallel()

	a := NewLocation("loc-a", 10)
	b := NewLocation("loc-b", 10)

	// Hold b's lock so every TryLock fails deterministically.
	b.mu.Lock()
	defer b.mu.Unlock()

	start := time.Now()
	err := ReserveBoth(a, b, 1, 1, 3, 100*time.Microsecond)
	if !errors.Is(err, ErrContended) {
		t.Fatalf("err = %v, want ErrContended", err)
	}
	if elapsed := time.Since(start); elapsed > 5*time.Second {
		t.Fatalf("contended reservation took %v; budget should bound it", elapsed)
	}
	if got := a.Available(); got != 10 {
		t.Errorf("a = %d after contended failure, want 10", got)
	}
}

func TestOppositeOrderStress(t *testing.T) {
	t.Parallel()

	a := NewLocation("loc-a", 100)
	b := NewLocation("loc-b", 100)
	const iterations = 500

	var wg sync.WaitGroup
	reserveLoop := func(first, second *Location) {
		defer wg.Done()
		for range iterations {
			// Retry the whole reservation on contention; jittered
			// backoff guarantees progress (no livelock).
			for {
				err := ReserveBoth(first, second, 1, 1, 10, 50*time.Microsecond)
				if err == nil {
					break
				}
				if !errors.Is(err, ErrContended) {
					t.Errorf("unexpected error: %v", err)
					return
				}
			}
			first.Release(1)
			second.Release(1)
		}
	}

	wg.Add(2)
	go reserveLoop(a, b)
	go reserveLoop(b, a) // opposite order: deadlock bait for blocking locks
	wg.Wait()

	if got := a.Available(); got != 100 {
		t.Errorf("a = %d at quiescence, want 100", got)
	}
	if got := b.Available(); got != 100 {
		t.Errorf("b = %d at quiescence, want 100", got)
	}
}

func ExampleReserveBoth() {
	a := NewLocation("loc-a", 5)
	b := NewLocation("loc-b", 5)
	if err := ReserveBoth(a, b, 2, 3, 3, time.Millisecond); err != nil {
		fmt.Println("err:", err)
		return
	}
	fmt.Println(a.Available(), b.Available())
	// Output: 3 2
}
```

## Review

The load-bearing property is that `ReserveBoth` never blocks while holding:
the only blocking acquisition (`a.mu.Lock`) happens with nothing held, and the
second acquisition is a `TryLock` whose failure path releases `a` *before*
sleeping. Move the `time.Sleep` above the `a.mu.Unlock` and you have
reinvented hold-and-wait with extra steps — the opposite-order stress test
will wedge. The other properties worth re-reading: jitter comes from
`rand/v2.Int64N` so contenders desynchronize; the retry budget converts
pathological contention into `ErrContended`, which callers must treat as
retryable and not as failure of the order; and the commit is all-or-nothing
because both decrements happen only under both locks.

Be honest about when to use this. The `sync.Mutex.TryLock` documentation says
correct uses are rare and often indicate a deeper problem, and this exercise
agrees: if you can derive any stable total order — even lexical comparison of
the UUID strings — ordered blocking acquisition is simpler, fair, and free of
livelock. Reach for TryLock-and-backoff when the order genuinely cannot exist
(locks owned by mutually-unaware packages, keys that get rewritten), and keep
the budget and jitter, because they are what separates this pattern from a
spin loop. Confirm with `go test -count=1 -race ./...`.

## Resources

- [sync package — Mutex.TryLock](https://pkg.go.dev/sync#Mutex.TryLock) — the API and its "correct uses are rare" warning.
- [math/rand/v2 — Int64N](https://pkg.go.dev/math/rand/v2#Int64N) — the jitter source; no seeding required.
- [Exponential Backoff And Jitter (AWS Architecture Blog)](https://aws.amazon.com/blogs/architecture/exponential-backoff-and-jitter/) — why jitter beats fixed retry schedules under contention.

---

Back to [00-concepts.md](00-concepts.md) | Next: [04-context-aware-lock.md](04-context-aware-lock.md)
