# Exercise 8: Deterministic TTL And Concurrency With testing/synctest

The earlier modules made time deterministic by *injecting* a clock. This module
takes the other route: `testing/synctest` (stable in Go 1.25) virtualizes the
`time` package underneath code that calls `time.Now` and `time.Sleep` directly, so
you test a real background reaper with no clock abstraction, a two-second TTL
asserted in microseconds, and goroutine scheduling made deterministic.

## What you'll build

```text
synccache/                  independent module: example.com/synccache
  go.mod
  cache.go                  Cache using time.Now directly + a background reaper goroutine
  cmd/
    demo/
      main.go               real-clock demo of the reaper evicting an entry
  cache_test.go             synctest bubble tests: lazy expiry + reaper eviction
```

Files: `cache.go`, `cmd/demo/main.go`, `cache_test.go`.
Implement: a cache reading `time.Now()` directly, lazy `Get` expiry, and `StartReaper(ctx, interval)` that deletes expired entries on a ticker.
Test: inside `synctest.Test`, `time.Sleep` past the TTL and assert eviction; use `synctest.Wait` to synchronize with the reaper.
Verify: `go test -count=1 -race ./...`

Set up the module:

```bash
mkdir -p go-solutions/12-testing-ecosystem/25-building-a-test-suite/08-synctest-deterministic-ttl/cmd/demo
cd go-solutions/12-testing-ecosystem/25-building-a-test-suite/08-synctest-deterministic-ttl
```

### Why there is no injected clock here

Contrast this deliberately with module 1. There the cache carried a
`now func() time.Time` field whose *only* purpose was to let a test freeze time.
Here the cache calls `time.Now()` directly, exactly as unremarkable production code
would, and the background reaper sleeps on a real `time.NewTicker`. The
`testing/synctest` bubble swaps the clock underneath all of it: inside
`synctest.Test(t, func(t *testing.T){ ... })`, `time.Now` starts at 2000-01-01 UTC
and only advances when every goroutine in the bubble is *durably blocked*. A
`time.Sleep(5 * time.Second)` does not wait five real seconds — it parks the
goroutine, the bubble sees everyone blocked, and it jumps the clock to the next
timer. The reason to prefer this for concurrent, timer-driven code is that it also
makes scheduling deterministic: `synctest.Wait()` blocks until every *other* bubble
goroutine is durably blocked, which removes the race between "I advanced the clock"
and "the reaper reacted".

The reaper is the piece that would be miserable to test with real sleeps: a
goroutine on a ticker that periodically deletes expired entries. Under synctest it
is trivial. Set an entry with a two-second TTL, `synctest.Wait()` to let the reaper
park on its ticker, `time.Sleep(5 * time.Second)` to advance virtual time past
several ticks, `synctest.Wait()` again to guarantee the reaper has finished its
sweep, then assert `Len() == 0`. No wall-clock wait, no flake. Two rules keep the
bubble healthy: the reaper must be cancellable (it is driven by a `context` and a
`defer cancel()`, so it exits when the bubble function returns — `synctest.Test`
waits for every bubble goroutine to exit and reports a deadlock if one leaks), and
there must be no real I/O inside the bubble (a syscall never becomes durably
blocked and would hang the clock).

Create `cache.go`. Note the direct `time.Now()` calls — no `now` field:

```go
package synccache

import (
	"context"
	"sync"
	"time"
)

type entry struct {
	value     []byte
	expiresAt time.Time
}

// Cache reads the wall clock through time.Now directly; a synctest bubble
// virtualizes that clock during tests, so no Clock abstraction is needed.
type Cache struct {
	mu   sync.Mutex
	data map[string]entry
}

func New() *Cache {
	return &Cache{data: make(map[string]entry)}
}

// Set stores value under key. ttl <= 0 means it never expires.
func (c *Cache) Set(key string, value []byte, ttl time.Duration) {
	c.mu.Lock()
	defer c.mu.Unlock()
	var expiresAt time.Time
	if ttl > 0 {
		expiresAt = time.Now().Add(ttl)
	}
	c.data[key] = entry{value: value, expiresAt: expiresAt}
}

// Get returns the value if present and unexpired. Expiry is lazy: an expired
// entry reports (nil, false) but is not deleted until the reaper sweeps it.
func (c *Cache) Get(key string) ([]byte, bool) {
	c.mu.Lock()
	defer c.mu.Unlock()
	e, ok := c.data[key]
	if !ok {
		return nil, false
	}
	if !e.expiresAt.IsZero() && time.Now().After(e.expiresAt) {
		return nil, false
	}
	return e.value, true
}

// Len reports the number of entries currently stored.
func (c *Cache) Len() int {
	c.mu.Lock()
	defer c.mu.Unlock()
	return len(c.data)
}

func (c *Cache) reap() {
	c.mu.Lock()
	defer c.mu.Unlock()
	now := time.Now()
	for k, e := range c.data {
		if !e.expiresAt.IsZero() && now.After(e.expiresAt) {
			delete(c.data, k)
		}
	}
}

// StartReaper runs a background goroutine that deletes expired entries every
// interval, until ctx is cancelled. It must be cancellable so it can exit.
func (c *Cache) StartReaper(ctx context.Context, interval time.Duration) {
	go func() {
		t := time.NewTicker(interval)
		defer t.Stop()
		for {
			select {
			case <-ctx.Done():
				return
			case <-t.C:
				c.reap()
			}
		}
	}()
}
```

### The runnable demo

The demo runs against the real clock (not a bubble) with generous margins so you
can watch the reaper actively reclaim an expired entry's memory: store a session
for 30 ms with a reaper ticking every 10 ms, then sleep 100 ms and observe the
length drop to zero.

Create `cmd/demo/main.go`:

```go
package main

import (
	"context"
	"fmt"
	"time"

	"example.com/synccache"
)

func main() {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	c := synccache.New()
	c.StartReaper(ctx, 10*time.Millisecond)

	c.Set("session", []byte("alice"), 30*time.Millisecond)
	fmt.Printf("stored: len=%d\n", c.Len())

	time.Sleep(100 * time.Millisecond)
	fmt.Printf("after ttl: len=%d\n", c.Len())
}
```

Run it:

```bash
go run ./cmd/demo
```

Expected output:

```
stored: len=1
after ttl: len=0
```

### The tests

`TestLazyGetExpiry` proves the lazy-expiry path in virtual time; `TestReaperEvicts`
proves the background reaper actually reclaims memory, synchronizing with it via
`synctest.Wait`. The outer tests may call `t.Parallel()`; the function passed to
`synctest.Test` must not.

Create `cache_test.go`:

```go
package synccache

import (
	"context"
	"testing"
	"testing/synctest"
	"time"
)

func TestLazyGetExpiry(t *testing.T) {
	t.Parallel()
	synctest.Test(t, func(t *testing.T) {
		c := New()
		c.Set("a", []byte("v"), time.Second)

		if _, ok := c.Get("a"); !ok {
			t.Fatal("Get(a) missing right after Set")
		}

		time.Sleep(2 * time.Second) // virtual: returns immediately

		if _, ok := c.Get("a"); ok {
			t.Fatal("Get(a) still present after TTL expired")
		}
	})
}

func TestReaperEvicts(t *testing.T) {
	t.Parallel()
	synctest.Test(t, func(t *testing.T) {
		c := New()
		ctx, cancel := context.WithCancel(context.Background())
		defer cancel() // reaper exits when the bubble function returns

		c.StartReaper(ctx, time.Second)
		c.Set("a", []byte("v"), 2*time.Second)

		synctest.Wait() // reaper parked on its ticker
		if got := c.Len(); got != 1 {
			t.Fatalf("Len before expiry = %d; want 1", got)
		}

		time.Sleep(5 * time.Second) // advance past the TTL and several ticks
		synctest.Wait()             // reaper has finished sweeping and re-parked

		if got := c.Len(); got != 0 {
			t.Fatalf("reaper left %d entries; want 0", got)
		}
	})
}
```

## Review

The cache is correct when expiry is a pure function of `time.Now()` and the stored
deadline, and the reaper eventually removes what lazy `Get` merely hides. The
synctest proof is that `time.Sleep(5 * time.Second)` advances virtual time
instantly and `synctest.Wait()` pins the reaper to a quiescent point before the
assertion, so `Len() == 0` is deterministic rather than a race you got lucky on.
The two failure modes to avoid are both about the bubble: a reaper with no way to
stop leaks a goroutine and turns into a reported deadlock, so drive it with a
context and `defer cancel()`; and any real I/O inside the bubble never becomes
durably blocked and hangs the clock. Prefer this approach over clock injection
whenever the code under test is concurrent or timer-driven; keep injection for a
plain synchronous unit check.

## Resources

- [`testing/synctest`](https://pkg.go.dev/testing/synctest) — the bubble, the fake clock, `synctest.Test`, and `synctest.Wait`.
- [Go Blog: Testing concurrent code with testing/synctest](https://go.dev/blog/synctest) — why no `Clock` interface is needed.
- [`context.WithCancel`](https://pkg.go.dev/context#WithCancel) — the cancellation that lets the reaper exit cleanly.

---

Back to [00-concepts.md](00-concepts.md) | Prev: [07-httptest-cache-aside-handler.md](07-httptest-cache-aside-handler.md) | Next: [09-benchmark-suite-b-loop.md](09-benchmark-suite-b-loop.md)
