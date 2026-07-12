# Exercise 6: Filtering Expired Sessions In Place With Zero Allocation

Garbage-collecting expired entries from a long-lived slice — a session table, a
lease set, a pending-request list — is a sweep that runs often and should cost
nothing. The `s[:0]` filter-in-place idiom compacts the survivors to the front of
the existing backing array with zero allocation. The catch, for a slice of
pointers, is that the vacated tail slots still reference the removed objects and
must be nil'd, or the "delete" is actually a memory leak.

This module is self-contained: its own module, demo, and tests.

## What you'll build

```text
sessiongc/                 independent module: example.com/sessiongc
  go.mod                   go 1.26
  sessiongc.go             Session; FilterExpired (in place, nils tail)
  cmd/
    demo/
      main.go              build a table, sweep, print survivors
  sessiongc_test.go        order + zero-alloc (AllocsPerRun) + nil-tail proof, Example
```

Files: `sessiongc.go`, `cmd/demo/main.go`, `sessiongc_test.go`.
Implement: `FilterExpired(sessions []*Session, now time.Time) []*Session` using `keep := sessions[:0]`, appending survivors, then nil-ing the freed tail slots.
Test: `testing.AllocsPerRun` proves zero allocations; assert only unexpired entries remain in order and that the freed tail slots are nil.
Verify: `go test -count=1 -race ./...`

Set up the module:

```bash
mkdir -p go-solutions/06-collections-arrays-slices-and-maps/02-slices-creation-append-capacity/06-filter-expired-sessions-in-place/cmd/demo
cd go-solutions/06-collections-arrays-slices-and-maps/02-slices-creation-append-capacity/06-filter-expired-sessions-in-place
go mod edit -go=1.26
```

### Why the sweep allocates nothing, and why the tail must be nil'd

`keep := sessions[:0]` produces a zero-length slice over the *same* backing array,
with the full original capacity available. As the loop appends each surviving
session, `append` finds `len < cap` and writes in place — the survivors are packed
toward the front of the array the input already owns. No new array is ever
allocated, so the whole sweep is zero-allocation regardless of how many sessions
there are. That is why this idiom is the right tool for a sweep on a hot path: it
reuses storage you already hold.

The subtlety is unique to slices of pointers. After the loop, `keep` has length
`k` (the survivor count), but the backing array is still full length, and the
slots from `k` to the original length still hold pointers to the *removed*
sessions. Those pointers keep the removed `*Session` objects reachable through the
array, so the garbage collector cannot free them — the sweep shrank the slice's
length but did not release the memory. For a long-lived table swept repeatedly,
that is a steadily growing leak of "deleted" sessions. The fix is to explicitly
nil every vacated slot (`for i := len(keep); i < len(sessions); i++ { sessions[i]
= nil }`, or equivalently `clear(sessions[len(keep):])`) so nothing in the array
references the removed objects. `slices.Delete` does this tail-clearing for you;
the hand-rolled filter must do it itself.

The filter also skips any `nil` slot defensively, which makes repeated sweeps over
the same (already-nil-tailed) backing array safe and idempotent.

Create `sessiongc.go`:

```go
package sessiongc

import "time"

// Session is a server-side session with an absolute expiry.
type Session struct {
	ID        string
	ExpiresAt time.Time
}

// Expired reports whether the session is expired at now (expiry is inclusive).
func (s *Session) Expired(now time.Time) bool {
	return !now.Before(s.ExpiresAt)
}

// FilterExpired removes expired (and nil) sessions in place, preserving the
// order of survivors, and returns the trimmed slice. It allocates nothing: the
// survivors are compacted into the input's backing array. The vacated tail slots
// are nil'd so the removed *Session values can be garbage-collected.
func FilterExpired(sessions []*Session, now time.Time) []*Session {
	keep := sessions[:0]
	for _, s := range sessions {
		if s != nil && !s.Expired(now) {
			keep = append(keep, s)
		}
	}
	for i := len(keep); i < len(sessions); i++ {
		sessions[i] = nil // release the removed pointer for GC
	}
	return keep
}
```

### The runnable demo

The demo builds four sessions, two already expired relative to a fixed `now`,
sweeps, and prints the surviving IDs in order.

Create `cmd/demo/main.go`:

```go
package main

import (
	"fmt"
	"time"

	"example.com/sessiongc"
)

func main() {
	now := time.Unix(1000, 0)
	sessions := []*sessiongc.Session{
		{ID: "a", ExpiresAt: now.Add(1 * time.Minute)},  // live
		{ID: "b", ExpiresAt: now.Add(-1 * time.Minute)}, // expired
		{ID: "c", ExpiresAt: now.Add(5 * time.Minute)},  // live
		{ID: "d", ExpiresAt: now.Add(-1 * time.Second)}, // expired
	}

	live := sessiongc.FilterExpired(sessions, now)
	ids := make([]string, len(live))
	for i, s := range live {
		ids[i] = s.ID
	}
	fmt.Printf("survivors: %v\n", ids)
	fmt.Printf("count: %d\n", len(live))
}
```

Run it:

```bash
go run ./cmd/demo
```

Expected output:

```
survivors: [a c]
count: 2
```

### Tests

`TestFilterOrderAndSurvivors` asserts the survivors are the unexpired entries in
their original order. `TestFilterNilsTail` keeps the original full-length header
and asserts every slot past the survivor count is nil — the leak-prevention
guarantee. `TestFilterZeroAlloc` uses `testing.AllocsPerRun` (not parallel, since
`AllocsPerRun` forbids it) to prove the sweep allocates nothing.

Create `sessiongc_test.go`:

```go
package sessiongc

import (
	"fmt"
	"testing"
	"time"
)

func mkSessions(now time.Time) []*Session {
	return []*Session{
		{ID: "a", ExpiresAt: now.Add(time.Minute)},
		{ID: "b", ExpiresAt: now.Add(-time.Minute)},
		{ID: "c", ExpiresAt: now.Add(time.Minute)},
		{ID: "d", ExpiresAt: now.Add(-time.Second)},
		{ID: "e", ExpiresAt: now.Add(time.Hour)},
	}
}

func TestFilterOrderAndSurvivors(t *testing.T) {
	t.Parallel()
	now := time.Unix(1000, 0)
	live := FilterExpired(mkSessions(now), now)

	var ids []string
	for _, s := range live {
		ids = append(ids, s.ID)
	}
	want := []string{"a", "c", "e"}
	if len(ids) != len(want) {
		t.Fatalf("survivors = %v, want %v", ids, want)
	}
	for i := range want {
		if ids[i] != want[i] {
			t.Fatalf("survivors = %v, want %v", ids, want)
		}
	}
}

func TestFilterNilsTail(t *testing.T) {
	t.Parallel()
	now := time.Unix(1000, 0)
	all := mkSessions(now)
	n := len(all)
	live := FilterExpired(all, now)

	for i := len(live); i < n; i++ {
		if all[i] != nil {
			t.Fatalf("tail slot %d not nil: %v (removed session leaked)", i, all[i])
		}
	}
}

func TestFilterZeroAlloc(t *testing.T) {
	now := time.Unix(1000, 0)
	// A larger table so a growth would be unmistakable.
	master := make([]*Session, 0, 1000)
	for i := range 1000 {
		exp := now.Add(time.Minute)
		if i%3 == 0 {
			exp = now.Add(-time.Minute) // ~a third expired
		}
		master = append(master, &Session{ID: "s", ExpiresAt: exp})
	}
	var sink []*Session
	allocs := testing.AllocsPerRun(50, func() {
		sink = FilterExpired(master, now)
	})
	if allocs != 0 {
		t.Errorf("FilterExpired allocated %v times, want 0", allocs)
	}
	_ = sink
}

func ExampleFilterExpired() {
	now := time.Unix(1000, 0)
	sessions := []*Session{
		{ID: "live", ExpiresAt: now.Add(time.Minute)},
		{ID: "dead", ExpiresAt: now.Add(-time.Minute)},
	}
	live := FilterExpired(sessions, now)
	fmt.Println(len(live), live[0].ID)
	// Output: 1 live
}
```

## Review

The sweep is correct when survivors keep their order, the backing array is reused
(zero allocation), and every removed pointer slot is nil'd. `TestFilterZeroAlloc`
proves the reuse; `TestFilterNilsTail` proves the leak fix — the assertion that
matters most, because a filter that "works" (returns the right survivors) can still
leak every removed object through the un-nil'd tail. If you would rather not
hand-roll the tail-clearing, `slices.Delete` clears the tail for you; the manual
version here exists to make the leak and its fix explicit. `AllocsPerRun` cannot
run inside a parallel test, so `TestFilterZeroAlloc` is intentionally serial. Run
`-race` to confirm the in-place mutation is sound.

## Resources

- [Go Wiki: SliceTricks (filtering without allocating)](https://go.dev/wiki/SliceTricks)
- [`slices.Delete`](https://pkg.go.dev/slices#Delete)
- [`testing.AllocsPerRun`](https://pkg.go.dev/testing#AllocsPerRun)

---

Back to [00-concepts.md](00-concepts.md) | Prev: [05-three-index-slice-protocol-framing.md](05-three-index-slice-protocol-framing.md) | Next: [07-remove-from-pool-ordered-vs-swap.md](07-remove-from-pool-ordered-vs-swap.md)
