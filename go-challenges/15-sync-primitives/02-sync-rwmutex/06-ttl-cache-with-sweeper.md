# Exercise 6: Session-token TTL cache with a background sweeper

Auth middleware validates a bearer token on every request, and hitting the
session store for every one of them is how you melt a database — so the
middleware keeps a small TTL cache in front. This exercise builds that cache the
way the lazy-expiry rule demands: `Get` reports an expired entry as a miss under
`RLock` *without deleting it*, and a background sweeper goroutine does the
physical eviction in batches under one `Lock` — with a graceful, idempotent
`Stop` so the goroutine never leaks.

This module is fully self-contained: its own `go mod init`, all code inline, its
own demo and tests. Nothing here imports another exercise.

## What you'll build

```text
ttlcache/                    independent module: example.com/ttlcache
  go.mod                     module example.com/ttlcache
  ttlcache.go                type Cache; New, Set, Get, Len, Sweep, Stop; sweeper loop
  cmd/
    demo/
      main.go                runnable demo: set, hit, expire, forced sweep, stop
  ttlcache_test.go           injected-clock expiry tests, sweeper-exit test, -race stress
```

Files: `ttlcache.go`, `cmd/demo/main.go`, `ttlcache_test.go`.
Implement: `Cache` with `Set(token, principal, ttl)` under `Lock`, `Get(token)` that treats a past-deadline entry as a miss under `RLock` without deleting it, `Sweep()` batch-evicting expired entries under `Lock`, a ticker-driven sweeper goroutine, and an idempotent `Stop()` via `sync.Once`.
Test: an injectable `now func() time.Time` drives expiry without sleeping; an expired entry is invisible to `Get` yet still counted by `Len` until `Sweep` removes it; concurrent `Get`/`Set` race the live sweeper under `-race`; `Stop` is idempotent and the sweeper provably exits.
Verify: `go test -count=1 -race ./...`

Set up the module:

```bash
mkdir -p ~/go-exercises/ttlcache/cmd/demo
cd ~/go-exercises/ttlcache
go mod init example.com/ttlcache
```

### Why the read path must not delete

An expired entry discovered inside `Get` is dead weight, and the reflex is to
delete it on the spot. Under an `RWMutex` both ways of doing that are bugs. A
`delete` while holding only `RLock` mutates the map concurrently with other
readers — a data race, and the `-race` detector will say so. Upgrading —
calling `Lock` while still holding `RLock` — is the self-deadlock from the
concepts file: the pending `Lock` blocks new readers, and the upgrader is
itself still a reader. So the read path does the only safe thing: it *answers
the question*. A token past its deadline reports `("", false)` exactly as if it
were never stored, and the corpse stays in the map.

Physical eviction moves to a sweeper: a goroutine on a `time.Ticker` that
periodically takes one `Lock` and deletes every expired entry in a single
batch. That amortizes the exclusive lock over many evictions and keeps it off
the request path entirely — readers only ever contend with a brief, occasional
batch delete instead of per-request write locks. The observable consequence is
deliberate: between expiry and the next sweep, `Get` says miss while `Len`
still counts the entry. The test suite pins that divergence as correct
behavior, because it is the proof that the read path is lazy.

### The clock and the shutdown are the two design decisions

Expiry logic is a pure function of "now", so the cache reads the clock through
a `now func() time.Time` field instead of calling `time.Now()` inline.
Production code constructs the cache with `time.Now`; tests construct it with a
hand-advanced fake and assert a one-minute TTL in microseconds, no sleeping.
(Go 1.25's `testing/synctest` can virtualize `time.Now()` itself and remove
this seam; the injected func is the portable version of the same idea.)

The sweeper is a goroutine you own, so you owe it a shutdown path. `Stop`
closes a `done` channel the sweeper selects on; `sync.Once` makes `Stop`
idempotent, so a double call (middleware teardown plus a defensive `defer`)
does not panic on a double close. The sweeper signals its own exit by closing a
`stopped` channel on the way out — that acknowledgment is what lets a test
*prove* the goroutine terminated instead of hoping. Skipping any of these three
pieces (done, Once, ack) is how ticker loops leak in production: every
constructor call pins a goroutine and a ticker forever, and graceful shutdown
hangs or lies.

Note the ticker itself is stopped with `defer ticker.Stop()` inside the
sweeper. A stopped `time.Ticker` releases its runtime timer; abandoning it
instead is a slow leak that monitoring will eventually surface as unexplained
timer growth.

Create `ttlcache.go`:

```go
package ttlcache

import (
	"sync"
	"time"
)

type entry struct {
	principal string
	deadline  time.Time
}

// Cache is a session-token cache with lazy TTL expiry. Get reports expired
// entries as misses without deleting them; a background sweeper batch-evicts
// under the write lock.
type Cache struct {
	mu      sync.RWMutex
	entries map[string]entry

	now func() time.Time

	done     chan struct{}
	stopped  chan struct{}
	stopOnce sync.Once
}

// New returns a Cache using the real clock and starts a sweeper goroutine that
// evicts expired entries every sweepEvery. Call Stop to shut the sweeper down.
func New(sweepEvery time.Duration) *Cache {
	c := newCache(time.Now)
	go c.sweeper(sweepEvery)
	return c
}

// newCache builds a Cache around an injectable clock and does not start a
// sweeper; tests drive expiry and eviction by hand.
func newCache(now func() time.Time) *Cache {
	return &Cache{
		entries: make(map[string]entry),
		now:     now,
		done:    make(chan struct{}),
		stopped: make(chan struct{}),
	}
}

// Set stores the principal for token, expiring ttl from now.
func (c *Cache) Set(token, principal string, ttl time.Duration) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.entries[token] = entry{principal: principal, deadline: c.now().Add(ttl)}
}

// Get returns the principal for token if the entry exists and is not past its
// deadline. An expired entry is a miss, but it is NOT deleted here: deleting
// under RLock is a race and upgrading to Lock is a self-deadlock. Eviction is
// the sweeper's job.
func (c *Cache) Get(token string) (string, bool) {
	c.mu.RLock()
	defer c.mu.RUnlock()
	e, ok := c.entries[token]
	if !ok || !c.now().Before(e.deadline) {
		return "", false
	}
	return e.principal, true
}

// Len reports the number of entries physically stored, expired or not.
func (c *Cache) Len() int {
	c.mu.RLock()
	defer c.mu.RUnlock()
	return len(c.entries)
}

// Sweep batch-evicts every expired entry under one write lock and reports how
// many it removed. The sweeper calls it on a ticker; tests call it directly.
func (c *Cache) Sweep() int {
	c.mu.Lock()
	defer c.mu.Unlock()
	now := c.now()
	removed := 0
	for token, e := range c.entries {
		if !now.Before(e.deadline) {
			delete(c.entries, token)
			removed++
		}
	}
	return removed
}

// Stop shuts the sweeper down. It is idempotent: sync.Once guards the close so
// a second Stop neither panics nor blocks.
func (c *Cache) Stop() {
	c.stopOnce.Do(func() { close(c.done) })
}

func (c *Cache) sweeper(every time.Duration) {
	defer close(c.stopped) // ack: proves to observers that the goroutine exited
	ticker := time.NewTicker(every)
	defer ticker.Stop()
	for {
		select {
		case <-c.done:
			return
		case <-ticker.C:
			c.Sweep()
		}
	}
}
```

### The runnable demo

The demo parks the sweeper on a one-hour interval and forces the sweep by hand,
so the output is deterministic: a hit before the TTL, a lazy miss after it, the
corpse still counted by `Len`, then gone after `Sweep`.

Create `cmd/demo/main.go`:

```go
package main

import (
	"fmt"
	"time"

	"example.com/ttlcache"
)

func main() {
	c := ttlcache.New(time.Hour) // sweeper idles; the demo forces the sweep
	defer c.Stop()

	c.Set("sess-abc", "user-42", 50*time.Millisecond)

	if v, ok := c.Get("sess-abc"); ok {
		fmt.Printf("before expiry: %s\n", v)
	}

	time.Sleep(80 * time.Millisecond)

	if _, ok := c.Get("sess-abc"); !ok {
		fmt.Println("after ttl: miss")
	}
	fmt.Printf("stored before sweep: %d\n", c.Len())
	fmt.Printf("sweep removed: %d\n", c.Sweep())
	fmt.Printf("stored after sweep: %d\n", c.Len())
}
```

Run it:

```bash
go run ./cmd/demo
```

Expected output:

```
before expiry: user-42
after ttl: miss
stored before sweep: 1
sweep removed: 1
stored after sweep: 0
```

### Tests

`TestExpiryBoundary` is the table test: an injected clock advances by exact
amounts and pins the boundary — one nanosecond before the deadline is a hit, the
deadline instant itself is a miss. `TestExpiredCountedUntilSweep` proves the
lazy read path: after expiry, `Get` misses while `Len` still counts the entry,
and only `Sweep` reconciles the two. `TestConcurrentWithSweeper` runs real
`Get`/`Set` traffic against a live 1 ms sweeper under `-race`.
`TestStopIdempotentSweeperExits` calls `Stop` twice (no panic, no block) and
then waits on the `stopped` ack channel with a timeout derived from
`t.Context()` — the only way to *know* the goroutine exited rather than leaked.

Create `ttlcache_test.go`:

```go
package ttlcache

import (
	"context"
	"fmt"
	"sync"
	"testing"
	"time"
)

func TestExpiryBoundary(t *testing.T) {
	t.Parallel()
	base := time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)

	tests := []struct {
		name    string
		ttl     time.Duration
		advance time.Duration
		wantHit bool
	}{
		{"live well before deadline", time.Minute, 30 * time.Second, true},
		{"live one ns before deadline", time.Minute, time.Minute - time.Nanosecond, true},
		{"expired exactly at deadline", time.Minute, time.Minute, false},
		{"expired well past deadline", time.Minute, 2 * time.Minute, false},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			now := base
			c := newCache(func() time.Time { return now })
			c.Set("sess", "user-7", tc.ttl)

			now = now.Add(tc.advance)

			_, ok := c.Get("sess")
			if ok != tc.wantHit {
				t.Fatalf("Get after advancing %v = hit:%v, want hit:%v", tc.advance, ok, tc.wantHit)
			}
		})
	}
}

func TestExpiredCountedUntilSweep(t *testing.T) {
	t.Parallel()
	now := time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)
	c := newCache(func() time.Time { return now })

	c.Set("live", "user-1", time.Hour)
	c.Set("dead", "user-2", time.Minute)

	now = now.Add(2 * time.Minute)

	if _, ok := c.Get("dead"); ok {
		t.Fatal("expired entry still visible to Get")
	}
	if got := c.Len(); got != 2 {
		t.Fatalf("Len before sweep = %d, want 2 (lazy read path must not delete)", got)
	}
	if removed := c.Sweep(); removed != 1 {
		t.Fatalf("Sweep removed %d entries, want 1", removed)
	}
	if got := c.Len(); got != 1 {
		t.Fatalf("Len after sweep = %d, want 1", got)
	}
	if v, ok := c.Get("live"); !ok || v != "user-1" {
		t.Fatalf("Get(live) after sweep = %q,%v, want user-1,true", v, ok)
	}
}

func TestConcurrentWithSweeper(t *testing.T) {
	t.Parallel()
	c := New(time.Millisecond)
	defer c.Stop()

	var wg sync.WaitGroup
	for g := range 8 {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for i := range 200 {
				token := fmt.Sprintf("sess-%d-%d", g, i%10)
				ttl := time.Duration(i%3) * time.Millisecond // some already expired
				c.Set(token, "user", ttl)
				c.Get(token)
				c.Len()
			}
		}()
	}
	wg.Wait()
}

func TestStopIdempotentSweeperExits(t *testing.T) {
	t.Parallel()
	c := New(time.Millisecond)
	c.Set("sess", "user", time.Minute)

	c.Stop()
	c.Stop() // must not panic (double close) and must not block

	ctx, cancel := context.WithTimeout(t.Context(), 2*time.Second)
	defer cancel()
	select {
	case <-c.stopped:
		// sweeper acknowledged its own exit
	case <-ctx.Done():
		t.Fatal("sweeper goroutine did not exit after Stop")
	}
}

func Example() {
	now := time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)
	c := newCache(func() time.Time { return now })

	c.Set("sess-abc", "user-42", time.Minute)
	v, ok := c.Get("sess-abc")
	fmt.Println(v, ok)

	now = now.Add(2 * time.Minute)
	_, ok = c.Get("sess-abc")
	fmt.Println(ok)
	fmt.Println(c.Sweep())
	// Output:
	// user-42 true
	// false
	// 1
}
```

## Review

The correctness core is one predicate — an entry is live iff `now.Before(deadline)`
— evaluated under `RLock` and never accompanied by a mutation. The mistakes this
module exists to inoculate against: deleting the expired entry inside `Get`
(write under a read lock — race), "upgrading" to `Lock` inside `Get` to do that
delete (self-deadlock), and starting the sweeper with no `done`/`Stop`/ack
machinery (a goroutine and ticker leak per cache instance). If your
`TestExpiredCountedUntilSweep` fails on the `Len == 2` assertion, your read path
is eagerly deleting — that is the exact bug being drilled. Confirm the whole
thing with `go test -count=1 -race ./...`: the stress test races real traffic
against the live sweeper, and the exit test converts "I think the goroutine
stopped" into a channel receive.

## Resources

- [`sync.RWMutex`](https://pkg.go.dev/sync#RWMutex) — the no-upgrade rule that forces lazy expiry onto the read path.
- [`time.NewTicker` and `Ticker.Stop`](https://pkg.go.dev/time#NewTicker) — the sweeper's timer, and why it must be stopped.
- [`sync.Once`](https://pkg.go.dev/sync#Once) — the idempotency guard around closing the done channel.
- [Testing concurrent code with testing/synctest](https://go.dev/blog/synctest) — the Go 1.25 alternative to injecting a clock func.

---

Back to [00-concepts.md](00-concepts.md) | Next: [07-per-key-rate-limiter-registry.md](07-per-key-rate-limiter-registry.md)
