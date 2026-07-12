# Exercise 32: Request Coalescing Deduplicator — Collapse Concurrent Identical Requests into One Flight

**Nivel: Avanzado** — validacion normal (tabla de casos, incluye borde o concurrencia).

A cache expiring at the exact moment a popular key gets hit by two hundred
concurrent requests turns one cache miss into two hundred simultaneous
identical queries against the database it was protecting -- a cache
stampede. The fix is not a faster database; it is refusing to run the same
expensive, idempotent operation more than once at a time for the same key,
and making every other concurrent caller wait for and share that one
in-flight result instead of triggering their own redundant copy. This
exercise is an independent module with its own `go mod init`.

## What you'll build

```text
coalesce/                  independent module: example.com/request-coalescing-singleflight
  go.mod                    module example.com/request-coalescing-singleflight
  coalesce.go               Group, New, Do, DoAll, Result
  cmd/
    demo/
      main.go               runnable demo: 5 requests, 2 keys repeated, one fetch each
  coalesce_test.go           coalescing under real concurrency, error sharing, no stale dedup, DoAll ordering, early-stop
```

Implement: `New() *Group`, `(*Group) Do(key string, fn func() (any, error)) (val any, err error, shared bool)` collapsing concurrent identical-key calls into one execution of `fn`, and `(*Group) DoAll(keys []string, fn func(key string) (any, error)) iter.Seq[Result]` running `fn` concurrently across `keys` (deduplicating repeats via `Do`) and yielding one `Result{Key, Value, Err, Shared}` per key in `keys`'s original order.
Test: ten goroutines calling `Do` with the same key while `fn` is deliberately held open run `fn` exactly once, and every follower reports `Shared=true`; an error from `fn` reaches every waiter; two calls to `Do` that do not overlap in time both run `fn` (no permanent, stale dedup); `DoAll` yields results in key order regardless of goroutine completion order; a consumer break stops `DoAll`'s output early.
Verify: `go test -race -count=1 ./...`

Set up the module:

```bash
mkdir -p go-solutions/03-control-flow/09-range-over-integers-and-functions/32-request-coalescing-singleflight/cmd/demo
cd go-solutions/03-control-flow/09-range-over-integers-and-functions/32-request-coalescing-singleflight
go mod edit -go=1.24
```

`Do`'s correctness rests entirely on one thing: the check ("is `key` already
in flight?") and the act ("register this call as the one in flight") happen
inside the same `g.mu.Lock()`/`Unlock()` pair. If those were two separate
critical sections -- check under one lock, then insert under a second lock
-- two goroutines could both observe no in-flight call for `key` in the gap
between them and both proceed to run `fn`, which is precisely the stampede
this type exists to prevent. The `call` type is always handled through a
`*call` pointer and never copied by value, because it embeds a
`sync.WaitGroup`: copying a `WaitGroup` after use corrupts its internal
counters, and `go vet`'s `copylocks` check exists specifically to catch that
mistake before it ships. `DoAll` layers a deterministic-ordering guarantee
on top of that concurrency-safe core: each of its goroutines writes only to
its own reserved index in a pre-sized `results` slice, so there is no shared
mutable state for two goroutines to race on, and `wg.Wait()` establishes the
happens-before edge that makes reading the whole slice afterward safe --
the final iteration is what turns "whichever goroutine finished first" into
"always in the order `keys` was given."

Create `coalesce.go`:

```go
package coalesce

import (
	"iter"
	"sync"
)

// call tracks one in-flight invocation of fn for a given key. It is always
// accessed through a *call, never copied by value, because it embeds a
// sync.WaitGroup: copying a WaitGroup after it has been used corrupts its
// internal state and go vet's copylocks check flags exactly this mistake.
type call struct {
	wg  sync.WaitGroup
	val any
	err error
}

// Group deduplicates concurrent calls that share a key: only one goroutine
// actually runs fn for a given key at a time, and every other concurrent
// caller for that same key blocks and receives the same result instead of
// re-running fn itself. This is the cache-stampede fix for an expensive,
// idempotent operation (a DB query, an upstream API call) that many
// goroutines might request at once for the same argument.
type Group struct {
	mu    sync.Mutex
	calls map[string]*call
}

// New creates an empty Group.
func New() *Group {
	return &Group{calls: make(map[string]*call)}
}

// Do runs fn for key, or waits for and returns the result of an
// already-in-flight call for the same key. shared reports whether this
// particular caller was a follower that reused someone else's in-flight
// result (true) rather than the leader that actually executed fn (false).
//
// The check ("is key already in flight") and the act ("register this call
// as the one in flight") happen inside the same critical section -- one
// g.mu.Lock()/Unlock() pair -- which is what makes this race-free. Splitting
// them into a lock-check-unlock followed by a separate lock-insert-unlock
// would leave a window where two goroutines both observe no in-flight call
// for key and both proceed to run fn, which defeats deduplication entirely.
func (g *Group) Do(key string, fn func() (any, error)) (val any, err error, shared bool) {
	g.mu.Lock()
	if c, ok := g.calls[key]; ok {
		g.mu.Unlock()
		c.wg.Wait()
		return c.val, c.err, true
	}
	c := new(call)
	c.wg.Add(1)
	g.calls[key] = c
	g.mu.Unlock()

	c.val, c.err = fn()
	c.wg.Done()

	g.mu.Lock()
	delete(g.calls, key)
	g.mu.Unlock()

	return c.val, c.err, false
}

// Result is one key's outcome from DoAll.
type Result struct {
	Key    string
	Value  any
	Err    error
	Shared bool
}

// DoAll fires fn concurrently for every key in keys -- deduplicating
// identical keys through Do exactly as concurrent external callers would --
// and yields one Result per key once every call has finished. Results are
// yielded in the original order of keys, not completion order: each
// goroutine writes only to its own reserved slot in a pre-sized slice, so
// there is no shared mutable state to race on, and the final range over
// that slice after wg.Wait() is what makes the output deterministic
// regardless of which goroutine happened to finish first.
func (g *Group) DoAll(keys []string, fn func(key string) (any, error)) iter.Seq[Result] {
	return func(yield func(Result) bool) {
		results := make([]Result, len(keys))
		var wg sync.WaitGroup
		for i, key := range keys {
			wg.Add(1)
			go func(i int, key string) {
				defer wg.Done()
				v, err, shared := g.Do(key, func() (any, error) { return fn(key) })
				results[i] = Result{Key: key, Value: v, Err: err, Shared: shared}
			}(i, key)
		}
		wg.Wait()

		for _, r := range results {
			if !yield(r) {
				return
			}
		}
	}
}
```

Create `cmd/demo/main.go`:

```go
package main

import (
	"fmt"

	"example.com/request-coalescing-singleflight"
)

func main() {
	g := coalesce.New()

	// sku-1 and sku-2 each appear more than once, simulating a cache
	// stampede: several concurrent requests for the same product landing
	// at once.
	keys := []string{"sku-1", "sku-2", "sku-1", "sku-3", "sku-2"}

	fetch := func(key string) (any, error) {
		return "priced:" + key, nil
	}

	for r := range g.DoAll(keys, fetch) {
		fmt.Printf("key=%-6s value=%v\n", r.Key, r.Value)
	}
}
```

### The runnable demo

```bash
go run ./cmd/demo
```

Expected output:

```
key=sku-1  value=priced:sku-1
key=sku-2  value=priced:sku-2
key=sku-1  value=priced:sku-1
key=sku-3  value=priced:sku-3
key=sku-2  value=priced:sku-2
```

`DoAll` yields exactly one `Result` per entry in `keys`, in that original
order, even though `sku-1` and `sku-2` were fetched only once each behind
the scenes -- the repeated entries reuse the shared, deduplicated result.
The printed `Value` is intentionally the only thing shown: whether a given
repeat happened to observe `Shared=true` depends on precise goroutine
timing that varies machine to machine, so the demo shows only the outcome
that is guaranteed, not the coalescing mechanics -- the test suite is where
`Shared` is pinned down deterministically, using a `fn` that blocks on a
channel until the test releases it.

### Tests

Create `coalesce_test.go`:

```go
package coalesce

import (
	"errors"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

func TestDoCoalescesConcurrentCallsForSameKey(t *testing.T) {
	t.Parallel()

	g := New()
	const n = 10
	var calls int32
	proceed := make(chan struct{})

	fn := func() (any, error) {
		atomic.AddInt32(&calls, 1)
		<-proceed
		return "value", nil
	}

	var wg sync.WaitGroup
	shared := make([]bool, n)
	for i := 0; i < n; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			_, _, s := g.Do("same-key", fn)
			shared[i] = s
		}(i)
	}

	// Give every goroutine a chance to reach Do and either register the
	// in-flight call or observe it before fn is allowed to return.
	time.Sleep(20 * time.Millisecond)
	close(proceed)
	wg.Wait()

	if got := atomic.LoadInt32(&calls); got != 1 {
		t.Fatalf("calls = %d, want 1: exactly one goroutine should execute fn", got)
	}
	sharedCount := 0
	for _, s := range shared {
		if s {
			sharedCount++
		}
	}
	if sharedCount != n-1 {
		t.Fatalf("sharedCount = %d, want %d (every caller except the leader)", sharedCount, n-1)
	}
}

func TestDoPropagatesErrorToAllWaiters(t *testing.T) {
	t.Parallel()

	g := New()
	wantErr := errors.New("upstream failed")
	proceed := make(chan struct{})
	fn := func() (any, error) {
		<-proceed
		return nil, wantErr
	}

	var wg sync.WaitGroup
	errs := make([]error, 3)
	for i := 0; i < 3; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			_, err, _ := g.Do("key", fn)
			errs[i] = err
		}(i)
	}
	time.Sleep(20 * time.Millisecond)
	close(proceed)
	wg.Wait()

	for i, err := range errs {
		if !errors.Is(err, wantErr) {
			t.Fatalf("errs[%d] = %v, want %v", i, err, wantErr)
		}
	}
}

func TestDoRunsAgainAfterPreviousCallCompletes(t *testing.T) {
	t.Parallel()

	g := New()
	var calls int32
	fn := func() (any, error) {
		atomic.AddInt32(&calls, 1)
		return "v", nil
	}

	g.Do("key", fn)
	g.Do("key", fn)

	if got := atomic.LoadInt32(&calls); got != 2 {
		t.Fatalf("calls = %d, want 2: sequential (non-overlapping) calls must not stay coalesced forever", got)
	}
}

func TestDoAllYieldsInKeyOrderNotCompletionOrder(t *testing.T) {
	t.Parallel()

	g := New()
	keys := []string{"c", "a", "b"}
	fn := func(key string) (any, error) { return "v:" + key, nil }

	var got []string
	for r := range g.DoAll(keys, fn) {
		got = append(got, r.Key)
	}
	want := []string{"c", "a", "b"}
	if len(got) != len(want) {
		t.Fatalf("got = %v, want %v", got, want)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Fatalf("got[%d] = %s, want %s", i, got[i], want[i])
		}
	}
}

func TestDoAllStopsYieldingOnBreak(t *testing.T) {
	t.Parallel()

	g := New()
	keys := []string{"k1", "k2", "k3", "k4"}
	fn := func(key string) (any, error) { return key, nil }

	count := 0
	for range g.DoAll(keys, fn) {
		count++
		if count == 2 {
			break
		}
	}
	if count != 2 {
		t.Fatalf("count = %d, want 2", count)
	}
}
```

## Review

`TestDoRunsAgainAfterPreviousCallCompletes` exists to rule out the opposite
bug from the one this type is built to prevent: a `Group` that deduplicates
correctly while a call is in flight but then, through some bookkeeping
mistake, never removes the finished `call` from its map would coalesce
*every* future call for that key forever, silently serving a permanently
stale cached value with no way to ever refresh it. The `delete(g.calls,
key)` after `fn` returns is what bounds coalescing to genuinely overlapping
requests. The other property worth internalizing is why `DoAll` hands each
goroutine its own pre-reserved slice index instead of, say, appending to a
shared slice under a mutex: `append` under a lock would still be race-free,
but it would make the output order depend on which goroutine's `Do` call
happened to finish first, and a caller matching results back up to the
`keys` they asked for would need to search instead of index.

## Resources

- [`iter.Seq` documentation](https://pkg.go.dev/iter#Seq)
- [`golang.org/x/sync/singleflight`](https://pkg.go.dev/golang.org/x/sync/singleflight)
- [The Go Memory Model](https://go.dev/ref/mem)

---

Back to [00-concepts.md](00-concepts.md) | Prev: [31-time-series-bucketing-aggregator.md](31-time-series-bucketing-aggregator.md) | Next: [33-hierarchical-token-quota-manager.md](33-hierarchical-token-quota-manager.md)
