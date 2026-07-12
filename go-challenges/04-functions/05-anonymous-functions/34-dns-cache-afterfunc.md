# Exercise 34: DNS Cache Invalidation Scheduled via context.AfterFunc TTL

**Nivel: Avanzado** — validacion normal (tabla de casos, incluye concurrencia).

A DNS cache that never expires an entry will happily serve a stale address
forever; one that expires entries with a background sweeper goroutine per
key wastes a goroutine per entry sitting idle almost all the time. This
module builds `Cache.Set` on `context.AfterFunc` instead: it schedules a
one-shot cleanup closure that runs — in its own goroutine, without blocking
anyone — the instant the entry's context becomes done, whether that's a
manual `Invalidate`, a real TTL timeout, or a test's own `cancel()`.

This module is fully self-contained. Nothing here imports another
exercise.

## What you'll build

```text
dnscache/                     module example.com/dnscache
  go.mod
  dnscache.go                   Cache, Set (context.AfterFunc scheduling), Lookup, Invalidate
  dnscache_test.go                cancel evicts, Invalidate stops cleanup, re-Set stops stale cleanup
  cmd/demo/main.go              a short real TTL expiring an entry
```

- Files: `dnscache.go`, `dnscache_test.go`, `cmd/demo/main.go`.
- Implement: `Cache.Set(ctx, host, addr)` calling `context.AfterFunc(ctx, cleanup)`, storing the returned `stop` alongside the entry; `Invalidate` and re-`Set` both calling a stale entry's `stop()`.
- Test: canceling an entry's context evicts it, observed deterministically via an `OnEvict` hook (no polling or sleeping past a TTL); `Invalidate` stops the scheduled cleanup so a later cancel is a no-op; re-`Set` on the same host stops the *old* entry's cleanup so it can never evict the new value.
- Verify: `go test -count=1 -race ./...`

Set up the module:

```bash
go mod edit -go=1.24
```

### AfterFunc's stop is the whole cleanup-safety story

`context.AfterFunc(ctx, f)` arranges for `f` to run in its own goroutine
once `ctx` is done, and returns a `stop func() bool` that prevents `f` from
running at all if called before `ctx` becomes done. `Set` uses that `stop`
in two places: first, before storing the new entry, it calls the *previous*
entry's `stop()` (if `host` was already cached) so an old TTL expiring
later can never reach in and delete a value that has since been
overwritten; second, `Invalidate` calls `stop()` itself when removing an
entry early, so the scheduled cleanup can never fire for an entry that is
already gone. Inside the cleanup closure itself, the check `e.addr == addr`
under the same lock that does the delete is the last line of defense —
even if scheduling raced, the closure only ever removes the exact entry it
was scheduled for, never whatever now happens to be at that key. Because
`AfterFunc`'s callback runs asynchronously in its own goroutine, the tests
below use an injected `OnEvict` hook and a channel to observe it
deterministically, rather than sleeping past a real TTL and hoping the
timing works out.

Create `dnscache.go`:

```go
package dnscache

import (
	"context"
	"sync"
)

// Cache maps hostnames to resolved addresses, invalidating each entry
// when the context it was Set with becomes done.
type Cache struct {
	mu      sync.Mutex
	entries map[string]entry

	// OnEvict, if non-nil, is called after the scheduled cleanup actually
	// removes an entry. It exists purely so callers -- and tests -- can
	// observe eviction deterministically via a channel instead of polling
	// or sleeping past the TTL.
	OnEvict func(host, addr string)
}

type entry struct {
	addr string
	stop func() bool
}

// New returns an empty Cache.
func New() *Cache {
	return &Cache{entries: make(map[string]entry)}
}

// Set stores addr for host and schedules its invalidation for whenever ctx
// is done -- pass context.WithTimeout(parent, ttl) to expire an entry
// after a fixed TTL. context.AfterFunc runs the cleanup closure in its own
// goroutine once ctx is done, without blocking whoever cancels or times
// out ctx. Re-Setting a host first stops the previous entry's scheduled
// cleanup, so an old TTL expiring later can never evict a newer value.
func (c *Cache) Set(ctx context.Context, host, addr string) {
	c.mu.Lock()
	if old, ok := c.entries[host]; ok {
		old.stop()
	}
	c.mu.Unlock()

	stop := context.AfterFunc(ctx, func() {
		c.mu.Lock()
		e, ok := c.entries[host]
		evicted := ok && e.addr == addr // check-then-act stays inside the lock
		if evicted {
			delete(c.entries, host)
		}
		c.mu.Unlock()
		if evicted && c.OnEvict != nil {
			c.OnEvict(host, addr)
		}
	})

	c.mu.Lock()
	c.entries[host] = entry{addr: addr, stop: stop}
	c.mu.Unlock()
}

// Lookup returns the cached address for host, if present and not yet
// invalidated.
func (c *Cache) Lookup(host string) (string, bool) {
	c.mu.Lock()
	defer c.mu.Unlock()
	e, ok := c.entries[host]
	return e.addr, ok
}

// Invalidate removes host immediately and stops its scheduled cleanup, so
// the AfterFunc callback never fires for an entry that is already gone.
func (c *Cache) Invalidate(host string) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if e, ok := c.entries[host]; ok {
		e.stop()
		delete(c.entries, host)
	}
}
```

### The runnable demo

The demo sets a real 20ms TTL, then sleeps a generous margin past it so the
scheduled cleanup has certainly already run before printing the result.

Create `cmd/demo/main.go`:

```go
package main

import (
	"context"
	"fmt"
	"time"

	"example.com/dnscache"
)

func main() {
	c := dnscache.New()

	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Millisecond)
	defer cancel()
	c.Set(ctx, "api.example.com", "10.0.0.1")

	if addr, ok := c.Lookup("api.example.com"); ok {
		fmt.Println("before TTL:", addr)
	}

	// A generous margin past the TTL so the scheduled AfterFunc cleanup has
	// certainly run by the time we look again.
	time.Sleep(200 * time.Millisecond)

	if _, ok := c.Lookup("api.example.com"); !ok {
		fmt.Println("after TTL: evicted")
	}
}
```

Run it:

```bash
go run ./cmd/demo
```

Expected output:

```
before TTL: 10.0.0.1
after TTL: evicted
```

### Tests

`TestSetLookupBeforeCancelFindsEntry` checks the baseline: a fresh entry is
readable before its context is ever canceled. `TestCancelEvictsEntryDeterministically`
uses `context.WithCancel` (not a real TTL) plus the `OnEvict` hook and a
channel to wait for eviction without any sleep or polling.
`TestInvalidateStopsScheduledCleanup` invalidates an entry, then cancels its
context afterward and checks `OnEvict` never fires — proving `stop()`
actually prevented the callback. `TestReSetStopsPreviousEntrysCleanup`
overwrites a host with a new context, cancels the *old* one, and checks the
new value survives untouched.

Create `dnscache_test.go`:

```go
package dnscache

import (
	"context"
	"testing"
	"time"
)

func TestSetLookupBeforeCancelFindsEntry(t *testing.T) {
	t.Parallel()
	c := New()
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	c.Set(ctx, "host", "1.2.3.4")
	if addr, ok := c.Lookup("host"); !ok || addr != "1.2.3.4" {
		t.Fatalf("Lookup() = (%q, %v), want (1.2.3.4, true)", addr, ok)
	}
}

func TestCancelEvictsEntryDeterministically(t *testing.T) {
	t.Parallel()
	c := New()
	evicted := make(chan struct{})
	c.OnEvict = func(host, addr string) {
		if host == "host" && addr == "1.2.3.4" {
			close(evicted)
		}
	}

	ctx, cancel := context.WithCancel(context.Background())
	c.Set(ctx, "host", "1.2.3.4")
	cancel()

	select {
	case <-evicted:
	case <-time.After(2 * time.Second):
		t.Fatal("OnEvict was not called after ctx was canceled")
	}

	if _, ok := c.Lookup("host"); ok {
		t.Fatal("Lookup() found an entry that should have been evicted on cancel")
	}
}

func TestInvalidateStopsScheduledCleanup(t *testing.T) {
	t.Parallel()
	c := New()
	evictCalled := make(chan struct{}, 1)
	c.OnEvict = func(string, string) { evictCalled <- struct{}{} }

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	c.Set(ctx, "host", "1.2.3.4")

	c.Invalidate("host")
	if _, ok := c.Lookup("host"); ok {
		t.Fatal("Lookup() found an entry right after Invalidate")
	}

	// Cancel only after Invalidate already stopped the scheduled cleanup;
	// OnEvict must never fire for an entry that is already gone.
	cancel()
	select {
	case <-evictCalled:
		t.Fatal("OnEvict fired for an already-invalidated entry")
	case <-time.After(50 * time.Millisecond):
	}
}

func TestReSetStopsPreviousEntrysCleanup(t *testing.T) {
	t.Parallel()
	c := New()
	evictCalled := make(chan string, 2)
	c.OnEvict = func(host, addr string) { evictCalled <- addr }

	oldCtx, oldCancel := context.WithCancel(context.Background())
	c.Set(oldCtx, "host", "old-addr")

	newCtx, newCancel := context.WithCancel(context.Background())
	defer newCancel()
	c.Set(newCtx, "host", "new-addr") // must stop oldCtx's scheduled cleanup

	// oldCtx becoming done later must not evict the new value.
	oldCancel()

	if addr, ok := c.Lookup("host"); !ok || addr != "new-addr" {
		t.Fatalf("Lookup() = (%q, %v), want (new-addr, true)", addr, ok)
	}

	select {
	case addr := <-evictCalled:
		t.Fatalf("OnEvict fired for %q after the entry was overwritten", addr)
	case <-time.After(50 * time.Millisecond):
	}
}
```

## Review

`Cache` is correct when every entry is eventually evicted exactly once —
by cancellation, by `Invalidate`, or never if it was overwritten first —
and never evicted twice or evicted after being overwritten. The `stop()`
calls at both re-`Set` and `Invalidate` are what make that hold: without
the one in `Set`, an old TTL context outliving a newer value for the same
host would eventually delete the new entry out from under its caller with
no warning. The `evicted := ok && e.addr == addr` check inside the
cleanup closure, taken under the same lock as the delete, is the
completed check-then-act that guards the case `stop()` didn't quite catch
in time — the closure had already started running before `stop()` could
prevent it. Skipping either guard is a real, if rare, production bug: a
cache serving a stale value with total confidence, right after correctly
reporting that a newer one was set.

## Resources

- [context.AfterFunc](https://pkg.go.dev/context#AfterFunc)
- [context.WithTimeout](https://pkg.go.dev/context#WithTimeout)
- [Go 1.21 release notes](https://go.dev/doc/go1.21)

---

Back to [00-concepts.md](00-concepts.md) | Prev: [33-connection-state-deferred.md](33-connection-state-deferred.md) | Next: [35-dedup-partition-racing.md](35-dedup-partition-racing.md)
