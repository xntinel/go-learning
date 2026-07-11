# Exercise 7: Stale-While-Revalidate — Serving Yesterday's Answer Fast

For read-heavy endpoints — feature flags, pricing tables, public profiles —
tail latency matters more than perfect freshness, and freshness is a spectrum,
not a boolean. This exercise builds the in-process version of RFC 5861's
`stale-while-revalidate` and `stale-if-error`: entries carry two horizons,
stale reads return instantly while exactly one background refresh runs, and a
failed refresh keeps serving stale until the hard deadline.

## What you'll build

```text
swrcache/                        independent module: example.com/swrcache
  go.mod
  swr/
    swr.go                       entry[V] {value, freshUntil, staleUntil, refreshing atomic.Bool},
                                 Cache[V]: New(fresh, stale), Get(ctx, key, loader),
                                 injected now func() time.Time, refreshHook test seam
    swr_test.go                  fake clock steps fresh -> stale -> dead; gated loader
                                 proves stale reads never block; N concurrent stale reads
                                 trigger exactly 1 refresh; stale-if-error semantics
  cmd/
    demo/
      main.go                    price endpoint: cold load, fresh hit, instant stale hit,
                                 value swapped by background refresh
```

- Files: `swr/swr.go`, `swr/swr_test.go`, `cmd/demo/main.go`.
- Implement: three-state `Get` (fresh / stale-refresh / dead-blocking), an `atomic.Bool` compare-and-swap so N stale readers start one refresh, `context.WithoutCancel` for the refresh, and stale-if-error fallback.
- Test: deterministic window transitions via an injected clock; a gated slow loader proving the stale path returns without blocking; an atomic counter proving exactly one refresh; failure tests for both stale-if-error and the dead path's wrapped sentinel.
- Verify: `go test -count=1 -race ./...`

Set up the module:

```bash
mkdir -p ~/go-exercises/swrcache/swr ~/go-exercises/swrcache/cmd/demo
cd ~/go-exercises/swrcache
go mod init example.com/swrcache
```

### Three states, three behaviors

Every entry has two horizons set when it is stored: `freshUntil = now+fresh`
and `staleUntil = freshUntil+stale`. `Get` classifies the entry against the
clock and behaves accordingly:

*Fresh* (`now < freshUntil`): return the value. No loader, no goroutine — the
common case costs a map read.

*Stale but usable* (`freshUntil <= now < staleUntil`): return the stale value
*immediately* — this is the entire point; the caller's latency is a map read
even though the data needs refreshing — and trigger exactly one background
refresh. The "exactly one" is enforced by `refreshing.CompareAndSwap(false,
true)` on the entry: of N concurrent stale readers, one wins the CAS and
spawns the refresh goroutine; the rest just take their stale value and leave.
A mutex-held flag would work too, but the CAS keeps the decision inside the
already-held critical section without extending it around goroutine spawning.

*Dead* (`now >= staleUntil`): the value is too old to serve. Block on the
loader like a cold miss, store the result, return it. Loader errors on this
path are wrapped with `%w` and surfaced — there is nothing acceptable left to
serve.

The refresh itself implements stale-if-error: if the background loader fails,
the old entry is left in place (still serving until `staleUntil`) and the
`refreshing` flag is reset so a later stale read can try again. Success stores
a new entry with fresh horizons. Either way the entry's flag ends false —
a refresh that could never be retried after one failure would silently turn
"stale-while-revalidate" into "stale-until-dead".

Two plumbing decisions matter more than they look. The refresh runs on
`context.WithoutCancel(ctx)`: it must carry the request's values (trace ids,
auth metadata) but *not* its cancellation, or the first client to disconnect
kills the refresh that every subsequent reader is counting on. And the clock
is an injected `now func() time.Time` defaulting to `time.Now` — SWR logic is
nothing but comparisons against two deadlines, and the tests step a fake clock
through the three windows deterministically instead of sleeping and hoping.
(`testing/synctest` could virtualize time instead; the injected func keeps
this module dependency-light and works on any Go version this chapter
supports.)

Create `swr/swr.go`:

```go
// Package swr implements an in-process stale-while-revalidate cache:
// RFC 5861 semantics applied to a map instead of an HTTP cache.
package swr

import (
	"context"
	"fmt"
	"sync"
	"sync/atomic"
	"time"
)

type entry[V any] struct {
	value      V
	freshUntil time.Time
	staleUntil time.Time
	refreshing atomic.Bool
}

// Cache serves values with two freshness horizons. Within fresh, reads
// are plain hits. Between fresh and stale, reads return the old value
// instantly while exactly one background refresh runs. Past stale,
// reads block on the loader like a cold miss.
type Cache[V any] struct {
	mu    sync.Mutex
	items map[string]*entry[V]
	fresh time.Duration
	stale time.Duration // extra serve-stale window after fresh ends

	now         func() time.Time       // injected for deterministic tests
	refreshHook func(key string, err error) // test seam; called after each refresh attempt
}

// New builds a Cache whose entries are fresh for fresh and then
// servable-stale for another stale.
func New[V any](fresh, stale time.Duration) *Cache[V] {
	return &Cache[V]{
		items: make(map[string]*entry[V]),
		fresh: fresh,
		stale: stale,
		now:   time.Now,
	}
}

// Get returns the value for key. Fresh hits and stale hits return
// immediately; stale hits additionally trigger one background refresh.
// Cold and dead entries block on loader.
func (c *Cache[V]) Get(ctx context.Context, key string, loader func(context.Context) (V, error)) (V, error) {
	c.mu.Lock()
	e := c.items[key]
	now := c.now()

	if e != nil && now.Before(e.freshUntil) {
		v := e.value
		c.mu.Unlock()
		return v, nil
	}

	if e != nil && now.Before(e.staleUntil) {
		v := e.value
		startRefresh := e.refreshing.CompareAndSwap(false, true)
		c.mu.Unlock()
		if startRefresh {
			// The refresh must outlive this request: keep ctx values,
			// drop its cancellation.
			go c.refresh(context.WithoutCancel(ctx), key, e, loader)
		}
		return v, nil
	}
	c.mu.Unlock()

	// Cold or dead: nothing acceptable to serve; block on the loader.
	v, err := loader(ctx)
	if err != nil {
		var zero V
		return zero, fmt.Errorf("swr load %q: %w", key, err)
	}
	c.store(key, v)
	return v, nil
}

func (c *Cache[V]) refresh(ctx context.Context, key string, old *entry[V], loader func(context.Context) (V, error)) {
	v, err := loader(ctx)
	if err == nil {
		c.store(key, v) // replaces old with a fresh entry
	}
	// On failure the old entry stays, serving stale until staleUntil.
	// Reset the flag either way so a later stale read can retry.
	old.refreshing.Store(false)
	if h := c.refreshHook; h != nil {
		h(key, err)
	}
}

func (c *Cache[V]) store(key string, v V) {
	now := c.now()
	c.mu.Lock()
	c.items[key] = &entry[V]{
		value:      v,
		freshUntil: now.Add(c.fresh),
		staleUntil: now.Add(c.fresh + c.stale),
	}
	c.mu.Unlock()
}
```

### The demo

A pricing endpoint whose loader is versioned: watch the stale window serve the
old price instantly while version 2 loads behind the scenes.

Create `cmd/demo/main.go`:

```go
package main

import (
	"context"
	"fmt"
	"sync/atomic"
	"time"

	"example.com/swrcache/swr"
)

func main() {
	var version atomic.Int64
	loader := func(ctx context.Context) (string, error) {
		return fmt.Sprintf("price-v%d", version.Add(1)), nil
	}

	c := swr.New[string](50*time.Millisecond, 200*time.Millisecond)
	ctx := context.Background()

	v, _ := c.Get(ctx, "sku:42", loader)
	fmt.Printf("cold load: %s\n", v)

	v, _ = c.Get(ctx, "sku:42", loader)
	fmt.Printf("fresh hit: %s\n", v)

	time.Sleep(70 * time.Millisecond) // past freshUntil, inside stale window

	v, _ = c.Get(ctx, "sku:42", loader)
	fmt.Printf("stale hit, served instantly: %s\n", v)

	time.Sleep(30 * time.Millisecond) // background refresh completes

	v, _ = c.Get(ctx, "sku:42", loader)
	fmt.Printf("after background refresh: %s\n", v)
	fmt.Printf("loader calls: %d\n", version.Load())
}
```

Run it:

```bash
go run ./cmd/demo
```

Expected output:

```
cold load: price-v1
fresh hit: price-v1
stale hit, served instantly: price-v1
after background refresh: price-v2
loader calls: 2
```

### Tests

The fake clock is a mutex-guarded time value — guarded because the refresh
goroutine reads it (through `c.now`) while the test advances it, and an
unguarded fake clock is a data race in the test itself. The stale test's
architecture is worth copying: the refresh loader blocks on a `gate` channel,
so the fact that all twenty stale `Get`s complete *before* the gate opens is
structural proof they never waited on the loader; the `refreshHook` seam then
lets the test wait for the refresh to finish without polling.

Create `swr/swr_test.go`:

```go
package swr

import (
	"context"
	"errors"
	"fmt"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

var errOriginDown = errors.New("origin down")

type fakeClock struct {
	mu sync.Mutex
	t  time.Time
}

func newFakeClock() *fakeClock {
	return &fakeClock{t: time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)}
}

func (f *fakeClock) Now() time.Time {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.t
}

func (f *fakeClock) Advance(d time.Duration) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.t = f.t.Add(d)
}

const (
	freshW = time.Minute
	staleW = 5 * time.Minute
)

func newTestCache(t *testing.T) (*Cache[string], *fakeClock, chan error) {
	t.Helper()
	c := New[string](freshW, staleW)
	clk := newFakeClock()
	c.now = clk.Now
	refreshed := make(chan error, 16)
	c.refreshHook = func(_ string, err error) { refreshed <- err }
	return c, clk, refreshed
}

func staticLoader(v string, calls *atomic.Int64) func(context.Context) (string, error) {
	return func(context.Context) (string, error) {
		calls.Add(1)
		return v, nil
	}
}

func TestFreshHitSkipsLoader(t *testing.T) {
	t.Parallel()

	c, _, _ := newTestCache(t)
	var calls atomic.Int64
	loader := staticLoader("v1", &calls)

	if v, err := c.Get(t.Context(), "k", loader); err != nil || v != "v1" {
		t.Fatalf("cold Get = %q, %v", v, err)
	}
	for range 5 {
		if v, err := c.Get(t.Context(), "k", loader); err != nil || v != "v1" {
			t.Fatalf("fresh Get = %q, %v", v, err)
		}
	}
	if got := calls.Load(); got != 1 {
		t.Fatalf("loader calls = %d, want 1 (fresh hits must not load)", got)
	}
}

func TestStaleServesInstantlyAndRefreshesExactlyOnce(t *testing.T) {
	t.Parallel()

	c, clk, refreshed := newTestCache(t)
	var coldCalls, refreshCalls atomic.Int64

	if _, err := c.Get(t.Context(), "k", staticLoader("v1", &coldCalls)); err != nil {
		t.Fatal(err)
	}

	clk.Advance(freshW + time.Second) // now stale, not dead

	gate := make(chan struct{})
	slowLoader := func(context.Context) (string, error) {
		refreshCalls.Add(1)
		<-gate
		return "v2", nil
	}

	const n = 20
	var wg sync.WaitGroup
	wg.Add(n)
	for i := range n {
		go func() {
			defer wg.Done()
			v, err := c.Get(t.Context(), "k", slowLoader)
			if err != nil || v != "v1" {
				t.Errorf("stale Get %d = %q, %v; want v1, nil", i, v, err)
			}
		}()
	}
	// All 20 complete while the loader is still gated: structural proof
	// that stale reads never block on the refresh.
	wg.Wait()

	close(gate)
	if err := <-refreshed; err != nil {
		t.Fatalf("refresh failed: %v", err)
	}
	if got := refreshCalls.Load(); got != 1 {
		t.Fatalf("refresh loader ran %d times for %d stale reads, want 1", got, n)
	}

	// The refresh repopulated the entry: next Get is a fresh v2 hit.
	if v, err := c.Get(t.Context(), "k", slowLoader); err != nil || v != "v2" {
		t.Fatalf("post-refresh Get = %q, %v; want v2, nil", v, err)
	}
	if got := refreshCalls.Load(); got != 1 {
		t.Fatalf("post-refresh Get invoked the loader again (calls=%d)", got)
	}
}

func TestDeadEntryBlocksOnLoader(t *testing.T) {
	t.Parallel()

	c, clk, _ := newTestCache(t)
	var calls atomic.Int64

	c.Get(t.Context(), "k", staticLoader("v1", &calls))
	clk.Advance(freshW + staleW + time.Second) // past staleUntil: dead

	v, err := c.Get(t.Context(), "k", staticLoader("v2", &calls))
	if err != nil || v != "v2" {
		t.Fatalf("dead Get = %q, %v; want v2 from a blocking load", v, err)
	}
	if got := calls.Load(); got != 2 {
		t.Fatalf("loader calls = %d, want 2", got)
	}
}

func TestRefreshFailureKeepsServingStale(t *testing.T) {
	t.Parallel()

	c, clk, refreshed := newTestCache(t)
	var coldCalls, failCalls atomic.Int64

	c.Get(t.Context(), "k", staticLoader("v1", &coldCalls))
	clk.Advance(freshW + time.Second) // stale window

	failing := func(context.Context) (string, error) {
		failCalls.Add(1)
		return "", errOriginDown
	}

	// First stale read: serves v1, background refresh fails.
	if v, err := c.Get(t.Context(), "k", failing); err != nil || v != "v1" {
		t.Fatalf("stale Get = %q, %v; want v1, nil (stale-if-error)", v, err)
	}
	if err := <-refreshed; !errors.Is(err, errOriginDown) {
		t.Fatalf("refresh err = %v, want errOriginDown", err)
	}

	// Still inside the stale window: keeps serving v1, retries refresh
	// (the refreshing flag must have been reset after the failure).
	if v, err := c.Get(t.Context(), "k", failing); err != nil || v != "v1" {
		t.Fatalf("second stale Get = %q, %v; want v1, nil", v, err)
	}
	<-refreshed
	if got := failCalls.Load(); got != 2 {
		t.Fatalf("refresh attempts = %d, want 2 (failure must not pin the flag)", got)
	}

	// Past the hard deadline: the error finally surfaces, wrapped.
	clk.Advance(staleW + time.Second)
	_, err := c.Get(t.Context(), "k", failing)
	if !errors.Is(err, errOriginDown) {
		t.Fatalf("dead Get err = %v, want wrapped errOriginDown", err)
	}
}

func ExampleCache_Get() {
	c := New[string](time.Minute, 5*time.Minute)
	v, _ := c.Get(context.Background(), "flag:new-ui",
		func(context.Context) (string, error) { return "enabled", nil })
	fmt.Println(v)
	// Output: enabled
}
```

Run the gate:

```bash
gofmt -l . && go vet ./... && go test -count=1 -race ./...
```

## Review

The state machine is the artifact: fresh serves, stale serves-and-refreshes,
dead blocks. When porting this, the bugs cluster in three places. The CAS on
`refreshing` must happen while the entry is still the *current* one — the code
does it under the mutex before unlocking — or two readers can race past each
other and double-refresh. The flag must be reset on *both* refresh outcomes;
miss the failure path and one origin blip disables refresh for that key until
it dies, which users experience as "the cache serves week-old data and then
suddenly errors". And `context.WithoutCancel` is not optional decoration:
with the request context, a cancelled winner leaves `refreshing=true` on an
entry whose refresh silently died mid-flight — the same pinning bug by
another road.

Note what the tests never do: sleep to wait for a state transition. The fake
clock makes fresh/stale/dead boundaries exact, the gate channel makes
"non-blocking" a structural fact rather than a timing observation, and the
hook channel replaces polling. Deterministic concurrency tests are designed,
not tuned. Run `go test -count=1 -race ./...` — the twenty-reader stale test
under the race detector is the module's real gate.

## Resources

- [RFC 5861 — HTTP Cache-Control Extensions for Stale Content](https://www.rfc-editor.org/rfc/rfc5861) — the stale-while-revalidate and stale-if-error semantics this module implements in-process.
- [`context.WithoutCancel`](https://pkg.go.dev/context#WithoutCancel) — detaching background work from request cancellation.
- [`sync/atomic`](https://pkg.go.dev/sync/atomic) — `atomic.Bool.CompareAndSwap`, the one-refresh guard.

---

Back to [00-concepts.md](00-concepts.md) | Prev: [06-ttl-jitter-negative-caching.md](06-ttl-jitter-negative-caching.md) | Next: [08-cache-metrics-expvar.md](08-cache-metrics-expvar.md)
