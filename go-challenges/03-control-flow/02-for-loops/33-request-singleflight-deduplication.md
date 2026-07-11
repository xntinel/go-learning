# Exercise 33: Request Singleflight Deduplication Across Goroutines

**Nivel: Avanzado** — validacion normal (tabla de casos, incluye borde o concurrencia).

A cache-miss stampede is what happens when a popular cache key expires and
a hundred concurrent requests all miss at once, each independently
triggering the same expensive database query or upstream call within
milliseconds of each other. The fix used throughout production Go
services (and the technique behind `golang.org/x/sync/singleflight`) is
to let exactly one of those concurrent callers actually do the work,
while every other caller for the same key waits and shares that one
result. This module builds that coordination from scratch: a mutex-guarded
registration map plus a `select`-based wait for anyone who is not the
caller doing the work.

This module is fully self-contained: its own `go mod init`, one test file,
one runnable demo.

## What you'll build

```text
singleflight/                  module example.com/singleflight
  go.mod                       go 1.24
  singleflight.go               call; Group; New(); (*Group).Do(ctx, key, fn) (any, error, bool)
  singleflight_test.go            fn runs once under concurrency, independent keys, waiter timeout, sequential calls don't dedupe
  cmd/demo/
    main.go                      five concurrent callers for one key, one loader invocation
```

- Files: `singleflight.go`, `singleflight_test.go`, `cmd/demo/main.go`.
- Implement: `(*Group).Do(ctx context.Context, key string, fn func() (any, error)) (any, error, bool)` — under `g.mu`, check the registration map and either join an existing call's wait via `select { case <-c.done: ...; case <-ctx.Done(): ... }`, or register a new `call`, release the lock, run `fn`, close `done`, and deregister.
- Test: twenty concurrent callers for the same key all get the same result and `fn` runs exactly once; distinct keys run independently and concurrently; a waiter's `ctx` timing out returns an error without disturbing the in-flight leader; two sequential (non-overlapping) calls for the same key each run `fn` — deduplication only applies to genuinely concurrent callers.
- Verify: `go test -count=1 -race ./...`

Set up the module:

```bash
mkdir -p ~/go-exercises/singleflight/cmd/demo
cd ~/go-exercises/singleflight
go mod init example.com/singleflight
go mod edit -go=1.24
```

### Why the registration check and the map write share one critical section

`Do`'s entire correctness rests on one guarantee: it must be impossible for
two concurrent callers of the same key to both conclude "I am the one who
runs `fn`." That guarantee only holds if "check whether `key` is already
registered" and "register `key` if it is not" happen as a single atomic
step — which is exactly what putting both inside one `g.mu.Lock()` /
`g.mu.Unlock()` block achieves. Splitting them into two separate critical
sections (`check` under one lock, `insert` under a second lock) reopens
the classic race: two goroutines could both see "not registered" during
their respective checks, and both proceed to register and run `fn`,
defeating the entire point of this module. This is the same "check
current state, then act, inside the same critical section" discipline
that matters whenever multiple goroutines make a decision based on shared
state — separating the read from the write is the bug, no matter how fast
the gap between them looks.

The `select` a waiter uses to join an in-flight call is doing two jobs at
once: `case <-c.done` is the ordinary path, unblocking the instant the
leader closes the channel with its result ready; `case <-ctx.Done()` is
what keeps a caller from hanging forever if the leader's `fn` is itself
stuck or simply slow past this caller's deadline. Crucially, a waiter's
context expiring only affects that one waiter's `Do` call — it does not
cancel the leader's `fn`, and it does not affect any other waiter, because
nothing about the timeout touches the shared `call` at all. That
independence is what `TestDoWaiterTimesOutWithoutAffectingTheLeader`
verifies: the leader's `fn` runs to completion and delivers its result to
every waiter who did *not* time out, regardless of the one who did.

Create `singleflight.go`:

```go
package singleflight

import (
	"context"
	"sync"
)

// call is the shared, in-flight state for one key: exactly one goroutine
// runs fn and populates val/err, and done is closed the instant it does so
// every waiting goroutine unblocks at the same time.
type call struct {
	done chan struct{}
	val  any
	err  error
}

// Group deduplicates concurrent calls for the same key: the first caller to
// register a key actually runs fn, and every other concurrent caller for
// that same key waits for that one call to finish and shares its result,
// instead of each triggering its own redundant fetch (the "thundering herd"
// a cache-miss stampede produces).
type Group struct {
	mu    sync.Mutex
	calls map[string]*call
}

// New returns an empty Group.
func New() *Group {
	return &Group{calls: make(map[string]*call)}
}

// Do runs fn for key, or, if another goroutine is already running fn for
// the same key, waits for that call's result instead of running fn again.
// The returned bool is true when the result was shared from another
// in-flight call, false when this goroutine was the one that actually ran
// fn. If ctx is cancelled while waiting on someone else's call, Do returns
// ctx.Err() without affecting the in-flight call itself -- other waiters
// and the original caller are unaffected by one waiter giving up.
//
// The registration check and the map insert happen inside the same
// critical section (a single g.mu.Lock/Unlock), which is what makes "am I
// the first caller for this key" race-free: two goroutines that both reach
// Do for the same key at nearly the same instant cannot both conclude they
// are the leader, because only one of them can hold the lock at the moment
// the map is checked and populated.
func (g *Group) Do(ctx context.Context, key string, fn func() (any, error)) (any, error, bool) {
	g.mu.Lock()
	if c, ok := g.calls[key]; ok {
		g.mu.Unlock()
		select {
		case <-c.done:
			return c.val, c.err, true
		case <-ctx.Done():
			return nil, ctx.Err(), true
		}
	}

	c := &call{done: make(chan struct{})}
	g.calls[key] = c
	g.mu.Unlock()

	val, err := fn()
	c.val, c.err = val, err
	close(c.done)

	g.mu.Lock()
	delete(g.calls, key)
	g.mu.Unlock()

	return val, err, false
}
```

### The runnable demo

Five goroutines call `Do` for the same key. The loader deliberately blocks
until every one of the four followers has joined the in-flight call, so
the deduplication is guaranteed to happen rather than being a matter of
scheduling luck. The demo prints aggregate counts rather than a per-caller
breakdown, because *which* of the five goroutines happens to become the
leader is inherently racy — the guarantee singleflight makes is about the
aggregate outcome (one load, one shared value), not about any particular
goroutine's identity.

Create `cmd/demo/main.go`:

```go
package main

import (
	"context"
	"fmt"
	"sync"
	"time"

	"example.com/singleflight"
)

func main() {
	g := singleflight.New()

	loads := 0
	leaderStarted := make(chan struct{})
	release := make(chan struct{})
	loader := func() (any, error) {
		close(leaderStarted)
		<-release
		loads++
		return fmt.Sprintf("value-%d", loads), nil
	}

	const followers = 4
	var wg sync.WaitGroup
	var mu sync.Mutex
	var values []any
	sharedCount := 0

	wg.Add(1)
	go func() {
		defer wg.Done()
		val, _, shared := g.Do(context.Background(), "user:42", loader)
		mu.Lock()
		values = append(values, val)
		if shared {
			sharedCount++
		}
		mu.Unlock()
	}()
	<-leaderStarted // the first caller is now registered as the in-flight leader

	for i := 0; i < followers; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			val, _, shared := g.Do(context.Background(), "user:42", loader)
			mu.Lock()
			values = append(values, val)
			if shared {
				sharedCount++
			}
			mu.Unlock()
		}()
	}
	// Scheduling headroom, not a correctness-timing assertion: gives every
	// follower goroutine above a chance to actually reach g.Do and register
	// as a waiter before the leader is released.
	time.Sleep(100 * time.Millisecond)
	close(release) // every caller has joined; let the loader finish

	wg.Wait()

	distinct := make(map[any]bool)
	for _, v := range values {
		distinct[v] = true
	}

	fmt.Printf("concurrent callers: %d\n", 1+followers)
	fmt.Printf("loader invocations: %d\n", loads)
	fmt.Printf("distinct values observed: %d\n", len(distinct))
	fmt.Printf("shared (deduplicated) responses: %d\n", sharedCount)
}
```

Run it:

```bash
go run ./cmd/demo
```

Expected output:

```text
concurrent callers: 5
loader invocations: 1
distinct values observed: 1
shared (deduplicated) responses: 4
```

### Tests

`TestDoRunsFnExactlyOnceForConcurrentCallers` holds twenty goroutines'
`fn` calls behind a shared gate until all twenty have reached `Do`, then
releases them together and asserts `fn` ran exactly once and exactly one
caller reports `shared == false`. `TestDoDifferentKeysRunIndependently`
confirms unrelated keys are not accidentally coalesced.
`TestDoWaiterTimesOutWithoutAffectingTheLeader` is the sharpest one: a
waiter with a short-deadline context gives up while the leader is still
blocked, and the test asserts the waiter's own `fn` never runs (it would
`t.Fatal` if it did) and the leader is unaffected.
`TestDoKeyIsRemovedAfterCompletionSoNextCallRunsAgain` proves
deduplication does not leak across time — two sequential, non-overlapping
calls for the same key each genuinely run `fn`.

Create `singleflight_test.go`:

```go
package singleflight

import (
	"context"
	"fmt"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

func TestDoRunsFnExactlyOnceForConcurrentCallers(t *testing.T) {
	t.Parallel()

	g := New()
	var loads int64
	// gate holds the leader's fn call open until every one of the callers
	// below has had a chance to reach g.Do, so the leader is guaranteed to
	// still be registered (and therefore every other caller becomes a
	// waiter, not a second leader) when the gate finally opens. The short
	// sleep before closing the gate is scheduling headroom, not a
	// correctness-timing assertion -- it is the same technique used by
	// golang.org/x/sync/singleflight's own test suite to let concurrently
	// started goroutines actually reach their blocking point before the
	// one in-flight call is allowed to complete.
	gate := make(chan struct{})
	fn := func() (any, error) {
		<-gate
		atomic.AddInt64(&loads, 1)
		return "loaded-value", nil
	}

	const callers = 20
	var wg sync.WaitGroup
	var mu sync.Mutex
	var sharedCount, leaderCount int

	for i := 0; i < callers; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			val, err, shared := g.Do(context.Background(), "user:1", fn)
			if err != nil {
				t.Errorf("Do() error = %v", err)
				return
			}
			if val != "loaded-value" {
				t.Errorf("Do() val = %v, want loaded-value", val)
			}
			mu.Lock()
			if shared {
				sharedCount++
			} else {
				leaderCount++
			}
			mu.Unlock()
		}()
	}
	time.Sleep(100 * time.Millisecond)
	close(gate)
	wg.Wait()

	if got := atomic.LoadInt64(&loads); got != 1 {
		t.Fatalf("fn ran %d times, want exactly 1", got)
	}
	if leaderCount != 1 {
		t.Fatalf("leaderCount = %d, want 1", leaderCount)
	}
	if sharedCount != callers-1 {
		t.Fatalf("sharedCount = %d, want %d", sharedCount, callers-1)
	}
}

func TestDoDifferentKeysRunIndependently(t *testing.T) {
	t.Parallel()

	g := New()
	var loads int64
	fn := func() (any, error) {
		atomic.AddInt64(&loads, 1)
		return "v", nil
	}

	var wg sync.WaitGroup
	for i := 0; i < 5; i++ {
		key := fmt.Sprintf("key-%d", i)
		wg.Add(1)
		go func() {
			defer wg.Done()
			g.Do(context.Background(), key, fn)
		}()
	}
	wg.Wait()

	if got := atomic.LoadInt64(&loads); got != 5 {
		t.Fatalf("fn ran %d times, want 5 (one per distinct key)", got)
	}
}

func TestDoWaiterTimesOutWithoutAffectingTheLeader(t *testing.T) {
	t.Parallel()

	g := New()
	release := make(chan struct{})
	leaderStarted := make(chan struct{})

	leaderDone := make(chan struct{})
	go func() {
		defer close(leaderDone)
		g.Do(context.Background(), "slow-key", func() (any, error) {
			close(leaderStarted)
			<-release
			return "eventually", nil
		})
	}()

	<-leaderStarted

	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Millisecond)
	defer cancel()

	_, err, shared := g.Do(ctx, "slow-key", func() (any, error) {
		t.Fatal("waiter's own fn must never run while a leader call is in flight")
		return nil, nil
	})
	if err == nil {
		t.Fatal("Do() error = nil, want context deadline exceeded")
	}
	if !shared {
		t.Fatal("shared = false, want true (this goroutine was a waiter, not the leader)")
	}

	close(release)
	<-leaderDone
}

func TestDoKeyIsRemovedAfterCompletionSoNextCallRunsAgain(t *testing.T) {
	t.Parallel()

	g := New()
	var loads int64
	fn := func() (any, error) {
		atomic.AddInt64(&loads, 1)
		return "v", nil
	}

	g.Do(context.Background(), "key", fn)
	g.Do(context.Background(), "key", fn)

	if got := atomic.LoadInt64(&loads); got != 2 {
		t.Fatalf("fn ran %d times, want 2 (sequential calls must not dedupe against each other)", got)
	}
}
```

## Review

`Do` is correct when, for any set of goroutines calling it concurrently
with the same key, exactly one of them executes `fn` and every one of
them (leader and waiters alike) observes the same `(val, err)` pair,
unless its own context expires first. That correctness depends entirely
on the registration check and the map write being one atomic operation
under `g.mu`. The common mistake this design avoids is checking the map
under the lock, releasing the lock, and *then* deciding whether to
register and run `fn` — the gap between the check and the write, however
small, is exactly where two goroutines can both decide they are the
leader, and the bug only shows up under real concurrency, never in a
single-threaded smoke test. Run `go test -count=1 -race ./...`.

## Resources

- [golang.org/x/sync/singleflight](https://pkg.go.dev/golang.org/x/sync/singleflight) — the production package this module reimplements a simplified version of.
- [Go Blog: Share Memory By Communicating](https://go.dev/blog/codelab-share) — the channel-based coordination `Do`'s wait path is built on.
- [sync.Mutex](https://pkg.go.dev/sync#Mutex) — guarding the registration map's check-then-write as one critical section.
- [context package](https://pkg.go.dev/context) — `ctx.Done()`, the cancellation path for a waiter that gives up.

---

Back to [00-concepts.md](00-concepts.md) | Prev: [32-dns-discovery-ttl-cache-refresh.md](32-dns-discovery-ttl-cache-refresh.md) | Next: [34-exponential-backoff-deadline-budget.md](34-exponential-backoff-deadline-budget.md)
