# Exercise 2: The Background Janitor

Lazy expiry hides expired entries but never frees their memory. The fix is a
background sweeper goroutine on a ticker — and a *second goroutine acting on the
clock* is exactly the case that makes ordinary time-tests flaky and `synctest`
shine. This exercise builds the sweeper and tests it with `synctest.Wait` plus a
cancellable context.

This module is fully self-contained. It carries its own small TTL cache so the
janitor has something to sweep, begins with its own `go mod init`, and ships its
own demo and tests. Nothing here imports any other exercise.

## What you'll build

```text
janitor/                   independent module: example.com/janitor
  go.mod                   go 1.25 (synctest needs it)
  janitor.go               type Cache[K,V]; New, Set, Len; StartJanitor, deleteExpired
  cmd/
    demo/
      main.go              runnable demo: janitor sweeps an expired key, leaves a live one
  janitor_test.go          synctest: eviction-on-tick + stop-on-cancel, synctest.Wait
```

- Files: `janitor.go`, `cmd/demo/main.go`, `janitor_test.go`.
- Implement: a TTL `Cache[K,V]` with `New`, `Set`, `Len`, plus `StartJanitor(ctx, interval)` launching a ticker goroutine that calls `deleteExpired` until `ctx` is cancelled.
- Test: a bubble that advances virtual time past several ticks, uses `synctest.Wait` to sync with the sweep, and asserts only the live entry remains; a second bubble that cancels and proves the goroutine exits.
- Verify: `go test -count=1 -race ./...`

Set up the module (`testing/synctest` requires Go 1.25+):

```bash
go mod edit -go=1.25
```

### Two details that make a background goroutine testable

A sweeper goroutine ticking on `time.NewTicker` adds the one dimension lazy expiry
lacked — a second goroutine that reacts to the clock — and that dimension is
precisely what `synctest` is built to tame. Two details in `StartJanitor` are what
make it work under a bubble.

First, the goroutine *selects on `ctx.Done()`* so it can be stopped. A goroutine
you start inside a bubble must be able to exit: `synctest.Test` waits for every
bubble goroutine to return before it finishes, and a sweeper with no stop path
would block that drain forever and be reported as a deadlock, not a silent leak.
Driving it with a context and cancelling (here via `defer cancel()` or an explicit
`cancel()`) is what lets the bubble close cleanly.

Second, it ticks on `time.NewTicker(interval)`, whose channel the bubble
virtualizes. The sweep therefore fires on *virtual* time: a test can sleep two
virtual seconds and know the 500 ms ticker fired at 0.5 s, 1 s, 1.5 s, and 2 s,
instantly and deterministically.

The test then uses the two tools that make such a goroutine assertable.
`synctest.Wait()` blocks until the janitor has finished the sweep its tick
triggered and parked again — removing the race between "I advanced the clock" and
"the sweeper reacted", which is the bug you would otherwise hit by reading `Len`
too early. And a cancelled `ctx` whose goroutine must reach its `return` before the
bubble can drain is what the stop-on-cancel test asserts.

Create `janitor.go` — a compact TTL cache plus the sweeper goroutine:

```go
package janitor

import (
	"context"
	"sync"
	"time"
)

type entry[V any] struct {
	value   V
	expires time.Time
}

// Cache is a TTL map with a background janitor that sweeps expired entries.
type Cache[K comparable, V any] struct {
	mu    sync.Mutex
	items map[K]entry[V]
}

func New[K comparable, V any]() *Cache[K, V] {
	return &Cache[K, V]{items: make(map[K]entry[V])}
}

// Set stores value under key, expiring it ttl from now.
func (c *Cache[K, V]) Set(key K, value V, ttl time.Duration) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.items[key] = entry[V]{value: value, expires: time.Now().Add(ttl)}
}

// Len reports how many entries are still stored.
func (c *Cache[K, V]) Len() int {
	c.mu.Lock()
	defer c.mu.Unlock()
	return len(c.items)
}

// StartJanitor launches a goroutine that deletes expired entries every interval
// until ctx is cancelled. The goroutine returns on cancellation, which is what
// lets a synctest bubble drain.
func (c *Cache[K, V]) StartJanitor(ctx context.Context, interval time.Duration) {
	go func() {
		ticker := time.NewTicker(interval)
		defer ticker.Stop()
		for {
			select {
			case <-ctx.Done():
				return
			case <-ticker.C:
				c.deleteExpired()
			}
		}
	}()
}

func (c *Cache[K, V]) deleteExpired() {
	c.mu.Lock()
	defer c.mu.Unlock()
	now := time.Now()
	for k, e := range c.items {
		if !now.Before(e.expires) {
			delete(c.items, k)
		}
	}
}
```

### The runnable demo

The demo runs the janitor against the real clock so you can watch it work: it
ticks every 20 ms, stores a key that expires in 30 ms and one that lives a second,
sleeps 60 ms, and prints the length before and after — the short-lived key is
swept, the long-lived one remains.

Create `cmd/demo/main.go`:

```go
package main

import (
	"context"
	"fmt"
	"time"

	"example.com/janitor"
)

func main() {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	c := janitor.New[string, int]()
	c.StartJanitor(ctx, 20*time.Millisecond)
	c.Set("a", 1, 30*time.Millisecond)
	c.Set("b", 2, time.Second)

	fmt.Println("len before:", c.Len())
	time.Sleep(60 * time.Millisecond)
	fmt.Println("len after:", c.Len()) // a was swept; b remains
}
```

Run it:

```bash
go run ./cmd/demo
```

Expected output:

```
len before: 2
len after: 1
```

### Tests

`TestJanitorEvictsExpired` is the showcase: it starts a 500 ms janitor, stores a
one-second and a ten-second entry, sleeps two virtual seconds so the ticker fires
four times, then calls `synctest.Wait()` so the final sweep is guaranteed complete
before it reads `Len`, and asserts exactly the long-lived entry survives. Without
the `Wait`, the assertion could race the sweep. `TestJanitorStopsOnCancel` proves
the goroutine actually exits: it cancels immediately and calls `synctest.Wait()` —
if the janitor failed to return on `ctx.Done()`, the bubble could not drain and
`synctest.Test` would report a deadlock.

Create `janitor_test.go`:

```go
package janitor

import (
	"context"
	"testing"
	"testing/synctest"
	"time"
)

func TestJanitorEvictsExpired(t *testing.T) {
	t.Parallel()
	synctest.Test(t, func(t *testing.T) {
		ctx, cancel := context.WithCancel(t.Context())
		defer cancel()

		c := New[string, int]()
		c.StartJanitor(ctx, 500*time.Millisecond)
		c.Set("short", 1, time.Second)
		c.Set("long", 2, 10*time.Second)

		time.Sleep(2 * time.Second) // janitor ticks at 0.5s, 1s, 1.5s, 2s
		synctest.Wait()             // let the janitor finish its sweep and park

		if got := c.Len(); got != 1 {
			t.Fatalf("Len = %d after expiry; want 1 (long survives)", got)
		}
	})
}

func TestJanitorStopsOnCancel(t *testing.T) {
	t.Parallel()
	synctest.Test(t, func(t *testing.T) {
		ctx, cancel := context.WithCancel(t.Context())
		c := New[string, int]()
		c.StartJanitor(ctx, time.Second)

		cancel()        // ask the janitor to stop
		synctest.Wait() // it must reach its return and exit; a leaked goroutine
		// would stop the bubble from draining and synctest.Test would report a
		// deadlock.
	})
}
```

## Review

The janitor is correct when the sweep is driven entirely by the virtualized ticker
and the assertion is gated by `synctest.Wait`. The single most common failure is
reading `Len` (or any shared state) right after `time.Sleep` without the `Wait`:
the clock advanced and woke the ticker, but the sweeper goroutine may not have run
yet, so the read races the delete. `synctest.Wait()` is the fix — it returns only
once the sweeper is durably blocked again, meaning its sweep is finished. The
second failure is a sweeper with no stop path: omit the `ctx.Done()` case and the
goroutine never exits, the bubble cannot drain, and `synctest.Test` reports a
deadlock — which is exactly what `TestJanitorStopsOnCancel` guards against. Run
`go test -race` to confirm `deleteExpired`'s map mutation is properly synchronized
against concurrent `Set`/`Len`.

## Resources

- [`testing/synctest`](https://pkg.go.dev/testing/synctest) — `synctest.Wait` and the durably-blocked contract that makes the sweep assertable.
- [`time.NewTicker`](https://pkg.go.dev/time#NewTicker) — the periodic timer the janitor ticks on, virtualized by the bubble.
- [`context.WithCancel`](https://pkg.go.dev/context#WithCancel) — the cancellation that lets the goroutine exit so the bubble can drain.

---

Back to [00-concepts.md](00-concepts.md) | Next: [03-sliding-expiration.md](03-sliding-expiration.md)
