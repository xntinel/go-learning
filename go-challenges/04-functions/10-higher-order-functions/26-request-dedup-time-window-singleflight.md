# Exercise 26: Request Deduplication with Time Window and Singleflight

**Nivel: Avanzado** — validacion normal (tabla de casos, incluye borde o concurrencia).

Classic singleflight collapses only calls that are *concurrently*
in-flight — the moment the first call returns, the very next caller pays
full price again. `Group.Do` extends that: after a call completes, its
result stays valid for a configurable time window, so a burst of requests
for the same key that arrive a few milliseconds apart — not just
simultaneously — still shares one upstream call.

## What you'll build

```text
dedupflight/                 independent module: example.com/dedupflight
  go.mod                     go 1.24
  dedupflight.go              type Group, call, cacheEntry; func New; method Do
  dedupflight_test.go         concurrent dedup, window reuse, expiry, independent keys, cached errors
  cmd/demo/
    main.go                  three sequential calls: fresh, cached, expired-then-fresh
```

- Files: `dedupflight.go`, `dedupflight_test.go`, `cmd/demo/main.go`.
- Implement: `Group` with `New(window time.Duration, now func() time.Time) *Group` and `func (g *Group) Do(key string, fn func() (any, error)) (val any, err error, shared bool)`.
- Test: concurrent calls for the same key run `fn` exactly once and both see `shared`; a call within the window reuses the cached result; a call after the window expires re-runs `fn`; different keys never dedupe against each other; an error result is cached and shared too.
- Verify: `go test -count=1 -race ./...`

Set up the module:

```bash
go mod edit -go=1.24
```

### Two maps, two lifetimes, one lock

`Group` tracks two different things under the same mutex: `inflight`,
calls currently running, and `cached`, calls that already finished and
are still within their window. `Do` checks them in that order — cache
first, then in-flight — because a fresh cache hit is cheaper than joining
a wait, and because once a call finishes and is cached, the entry is
promptly deleted from `inflight`, so the two maps never both hold an
entry for the same key. The one non-obvious correctness detail is why the
waiting side of the in-flight path needs no lock of its own: `call.val`
and `call.err` are written by exactly one goroutine — the one that
registered the call — and only *after* that write does it `close(c.done)`.
Every other goroutine waits on `<-c.done` before reading `val`/`err`, and
a channel close happens-before any receive that observes it completing,
which is what makes the read side race-free without a second mutex
acquisition.

The clock is injected the same way `Sleeper` was in Exercise 24: swapping
`now` for a fake, manually-advanced clock is what makes "the window
expired" a deterministic, instant assertion in a test instead of a real
sleep racing a real timer.

Create `dedupflight.go`:

```go
package dedupflight

import (
	"sync"
	"time"
)

// call is the shared record for one in-flight fn invocation. done is
// closed exactly once, after val and err are written, so every waiter's
// receive on done happens-after that write (channel close is a
// synchronization point), making the read side race-free without its own
// lock.
type call struct {
	done chan struct{}
	val  any
	err  error
}

// cacheEntry is a completed call's result, valid for reuse until
// expiresAt.
type cacheEntry struct {
	val       any
	err       error
	expiresAt time.Time
}

// Group deduplicates concurrent calls for the same key (classic
// singleflight) and additionally memoizes the result for window after
// the call completes, so a burst of calls for the same key — concurrent
// or merely close together in time — runs fn at most once per window.
type Group struct {
	mu       sync.Mutex
	window   time.Duration
	now      func() time.Time
	inflight map[string]*call
	cached   map[string]cacheEntry
}

// New builds a Group whose cache entries live for window, using now as
// the clock. Production code passes time.Now; tests pass a fake clock so
// window expiry is deterministic instead of racing a real timer.
func New(window time.Duration, now func() time.Time) *Group {
	return &Group{
		window:   window,
		now:      now,
		inflight: make(map[string]*call),
		cached:   make(map[string]cacheEntry),
	}
}

// Do runs fn for key, or reuses another caller's result for the same key
// if one is in flight or still within its cache window. shared reports
// whether this call's result came from another invocation rather than
// running fn itself.
func (g *Group) Do(key string, fn func() (any, error)) (val any, err error, shared bool) {
	g.mu.Lock()
	if entry, ok := g.cached[key]; ok && g.now().Before(entry.expiresAt) {
		g.mu.Unlock()
		return entry.val, entry.err, true
	}
	if c, ok := g.inflight[key]; ok {
		g.mu.Unlock()
		<-c.done
		return c.val, c.err, true
	}

	c := &call{done: make(chan struct{})}
	g.inflight[key] = c
	g.mu.Unlock()

	c.val, c.err = fn()

	g.mu.Lock()
	delete(g.inflight, key)
	g.cached[key] = cacheEntry{val: c.val, err: c.err, expiresAt: g.now().Add(g.window)}
	g.mu.Unlock()

	close(c.done)
	return c.val, c.err, false
}
```

### The runnable demo

The demo makes three sequential `Do` calls for the same key against a
manually advanced fake clock: the first runs `fn`, the second reuses the
cached result because it is still within the window, and the third —
after the clock jumps two minutes forward, past the one-minute window —
runs `fn` again.

Create `cmd/demo/main.go`:

```go
package main

import (
	"fmt"
	"time"

	"example.com/dedupflight"
)

// fakeClock is a manually advanced clock, used here (single-threaded, so
// no locking needed) to make the cache-window expiry deterministic
// without a real sleep.
type fakeClock struct{ t time.Time }

func (c *fakeClock) Now() time.Time          { return c.t }
func (c *fakeClock) Advance(d time.Duration) { c.t = c.t.Add(d) }

func main() {
	clock := &fakeClock{t: time.Unix(0, 0)}
	g := dedupflight.New(time.Minute, clock.Now)

	calls := 0
	fn := func() (any, error) {
		calls++
		return fmt.Sprintf("profile-snapshot-%d", calls), nil
	}

	v1, _, shared1 := g.Do("user:42", fn)
	fmt.Printf("call 1: value=%v shared=%v total-fn-calls=%d\n", v1, shared1, calls)

	v2, _, shared2 := g.Do("user:42", fn)
	fmt.Printf("call 2 (same window): value=%v shared=%v total-fn-calls=%d\n", v2, shared2, calls)

	clock.Advance(2 * time.Minute)
	v3, _, shared3 := g.Do("user:42", fn)
	fmt.Printf("call 3 (after window): value=%v shared=%v total-fn-calls=%d\n", v3, shared3, calls)
}
```

Run it:

```bash
go run ./cmd/demo
```

Expected output:

```
call 1: value=profile-snapshot-1 shared=false total-fn-calls=1
call 2 (same window): value=profile-snapshot-1 shared=true total-fn-calls=1
call 3 (after window): value=profile-snapshot-2 shared=false total-fn-calls=2
```

### Tests

`TestDoConcurrentCallsForSameKeyRunFnOnce` is the core singleflight
property under `-race`: it uses a `started` channel to guarantee the
in-flight entry is registered before a second goroutine calls `Do` for
the same key, then releases both with `proceed` and asserts `fn` ran
exactly once. `TestDoReusesCachedResultWithinWindow` and
`TestDoRunsAgainAfterWindowExpires` are the two halves of the window
behavior that plain singleflight does not have — the fake clock makes
the expiry boundary an exact, instant assertion instead of a flaky sleep.
`TestDoDifferentKeysAreIndependent` guards against a broken key
comparison collapsing unrelated requests. `TestDoCachesErrorsWithinWindowToo`
pins down a deliberate choice: a failed upstream call is cached and
shared just like a success, so a burst of requests against a downed
dependency does not each independently retry it.

Create `dedupflight_test.go`:

```go
package dedupflight

import (
	"errors"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

// fakeClock is a manually advanced, mutex-protected clock so tests can
// control window expiry without a real sleep, even when Now is called
// from multiple goroutines.
type fakeClock struct {
	mu sync.Mutex
	t  time.Time
}

func (c *fakeClock) Now() time.Time {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.t
}

func (c *fakeClock) Advance(d time.Duration) {
	c.mu.Lock()
	c.t = c.t.Add(d)
	c.mu.Unlock()
}

func TestDoConcurrentCallsForSameKeyRunFnOnce(t *testing.T) {
	t.Parallel()

	clock := &fakeClock{t: time.Unix(0, 0)}
	g := New(time.Minute, clock.Now)

	var calls atomic.Int32
	started := make(chan struct{})
	proceed := make(chan struct{})
	fn := func() (any, error) {
		calls.Add(1)
		close(started)
		<-proceed
		return "shared-value", nil
	}

	results := make([]struct {
		val    any
		shared bool
	}, 2)

	var wg sync.WaitGroup
	wg.Add(1)
	go func() {
		defer wg.Done()
		v, _, shared := g.Do("key", fn)
		results[0].val, results[0].shared = v, shared
	}()

	<-started // the first Do has registered the in-flight call before fn returns

	wg.Add(1)
	go func() {
		defer wg.Done()
		v, _, shared := g.Do("key", fn)
		results[1].val, results[1].shared = v, shared
	}()

	// Give the second goroutine a chance to reach the in-flight wait
	// before releasing the first call. There is no race here: whichever
	// order the two Do calls acquire g.mu, the second either finds the
	// in-flight entry (already present since before fn started) or, in
	// the extremely unlikely case it runs after completion, the cache
	// entry — either way shared ends up true and fn is called once.
	close(proceed)
	wg.Wait()

	if got := calls.Load(); got != 1 {
		t.Fatalf("fn called %d times, want 1", got)
	}
	if results[0].val != "shared-value" || results[1].val != "shared-value" {
		t.Fatalf("results = %+v, want both to be shared-value", results)
	}
	if !results[0].shared && !results[1].shared {
		t.Fatalf("neither call reported shared=true, want at least one")
	}
}

func TestDoReusesCachedResultWithinWindow(t *testing.T) {
	t.Parallel()

	clock := &fakeClock{t: time.Unix(0, 0)}
	g := New(time.Minute, clock.Now)

	var calls atomic.Int32
	fn := func() (any, error) {
		calls.Add(1)
		return "value", nil
	}

	v1, _, shared1 := g.Do("key", fn)
	v2, _, shared2 := g.Do("key", fn)

	if calls.Load() != 1 {
		t.Fatalf("fn called %d times, want 1", calls.Load())
	}
	if shared1 {
		t.Fatal("first call reported shared=true, want false")
	}
	if !shared2 {
		t.Fatal("second call within window reported shared=false, want true")
	}
	if v1 != v2 {
		t.Fatalf("v1=%v v2=%v, want equal", v1, v2)
	}
}

func TestDoRunsAgainAfterWindowExpires(t *testing.T) {
	t.Parallel()

	clock := &fakeClock{t: time.Unix(0, 0)}
	g := New(time.Minute, clock.Now)

	var calls atomic.Int32
	fn := func() (any, error) {
		n := calls.Add(1)
		return n, nil
	}

	g.Do("key", fn)
	clock.Advance(2 * time.Minute)
	v, _, shared := g.Do("key", fn)

	if calls.Load() != 2 {
		t.Fatalf("fn called %d times, want 2 (window expired)", calls.Load())
	}
	if shared {
		t.Fatal("call after window expiry reported shared=true, want false")
	}
	if v != int32(2) {
		t.Fatalf("v = %v, want 2", v)
	}
}

func TestDoDifferentKeysAreIndependent(t *testing.T) {
	t.Parallel()

	clock := &fakeClock{t: time.Unix(0, 0)}
	g := New(time.Minute, clock.Now)

	var calls atomic.Int32
	fn := func() (any, error) {
		calls.Add(1)
		return "value", nil
	}

	g.Do("key-a", fn)
	g.Do("key-b", fn)

	if calls.Load() != 2 {
		t.Fatalf("fn called %d times, want 2 (distinct keys must not dedupe)", calls.Load())
	}
}

func TestDoCachesErrorsWithinWindowToo(t *testing.T) {
	t.Parallel()

	clock := &fakeClock{t: time.Unix(0, 0)}
	g := New(time.Minute, clock.Now)

	errBoom := errors.New("upstream unavailable")
	var calls atomic.Int32
	fn := func() (any, error) {
		calls.Add(1)
		return nil, errBoom
	}

	_, err1, _ := g.Do("key", fn)
	_, err2, shared2 := g.Do("key", fn)

	if calls.Load() != 1 {
		t.Fatalf("fn called %d times, want 1", calls.Load())
	}
	if !errors.Is(err1, errBoom) || !errors.Is(err2, errBoom) {
		t.Fatalf("err1=%v err2=%v, want both errBoom", err1, err2)
	}
	if !shared2 {
		t.Fatal("second call reported shared=false, want true (error was cached)")
	}
}
```

## Review

`Do` is correct because it never lets two goroutines both register an
in-flight call for the same key: the cache check, the in-flight check,
and the registration of a new `call` all happen inside one `g.mu.Lock()`
critical section, with no gap between "no one else is handling this key"
and "now I am." The waiting path's safety does not come from a second
lock — it comes from the happens-before guarantee of a channel close,
which is why `c.val`/`c.err` must be written strictly before
`close(c.done)`, never after. Treating a cached error exactly like a
cached success is a deliberate product decision, not an oversight: state
it explicitly, since a reader might expect errors to bypass the cache and
retry immediately. Run `go test -race`, since the entire value of this
exercise is what happens when multiple goroutines call `Do` for the same
key at once.

## Resources

- [golang.org/x/sync/singleflight](https://pkg.go.dev/golang.org/x/sync/singleflight) — the standard extended-library implementation this exercise's in-flight half is modeled on.
- [sync package](https://pkg.go.dev/sync) — `Mutex`, `WaitGroup`, the primitives protecting `Group`'s two maps.
- [Go memory model: channel communication](https://go.dev/ref/mem#chan) — the happens-before rule that makes closing `done` after writing `val`/`err` safe to read without a second lock.

---

Back to [00-concepts.md](00-concepts.md) | Prev: [25-feature-flag-variant-selector.md](25-feature-flag-variant-selector.md) | Next: [27-visitor-tree-traversal-injected.md](27-visitor-tree-traversal-injected.md)
