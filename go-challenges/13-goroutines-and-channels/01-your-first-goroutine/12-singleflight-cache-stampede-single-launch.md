# Exercise 12: Cache Stampede Guard: One Launched Compute Per Key, Shared By Every Concurrent Caller

**Level: Intermediate**

When a hot cache key expires, every in-flight request handler misses at the same
instant, and the naive fix -- each handler recomputes the value itself -- turns a
single cache miss into a stampede that hammers the database or upstream service
with N identical expensive queries at once. The fix is deduplication by key: the
first caller for an in-flight key launches exactly one background compute
goroutine, and every other concurrent caller for that same key attaches to it and
shares the single result through a broadcast instead of launching its own. This
exercise builds `Group.Do`, the purest "your first goroutine" scenario, where the
launch-or-attach decision is the entire correctness property.

This module is self-contained: its own module, a `singleflight` package, a demo,
and tests. Nothing here imports another exercise.

## What you'll build

```text
singleflight/                independent module: example.com/singleflight
  go.mod                     go 1.26
  singleflight.go            Group[K,V].Do(key, fn) (V, error, bool): one launch per in-flight key
  cmd/demo/main.go           runnable demo: no retention, distinct-key independence, concurrent sharing
  singleflight_test.go       fn-once, shared, distinct-key concurrency, recompute, error fan-out, no leak
```

- Files: `singleflight.go`, `cmd/demo/main.go`, `singleflight_test.go`.
- Implement: `Group[K comparable, V any]` with `Do(key K, fn func() (V, error)) (v V, err error, shared bool)` -- the first caller for a key launches one goroutine running `fn`; concurrent callers attach and share its result; `shared` reports whether the result went to more than one caller; the key is removed once `fn` completes.
- Test: M concurrent callers run `fn` exactly once and all receive the same value/error; distinct keys run concurrently; a resolved call recomputes on the next `Do`; an error fans out to every waiter; no goroutine leaks.
- Verify: `go test -count=1 -race ./...`

Set up the module:

```bash
go mod edit -go=1.26
```

### The launch-or-attach protocol under one mutex

The whole guard is a single decision made while holding one mutex: is a compute
for this key already in flight? The `inflight` map answers it. A `call` value is
the shared handle for one in-flight compute -- it carries a `done` channel that is
closed exactly once to broadcast completion, plus the result fields every waiter
reads afterward. The protocol has four steps:

1. Lock the mutex. If the key is already in `inflight`, this caller is a
   *follower*: increment the call's `dups` counter (so the owner can later report
   `shared`), unlock, block on `<-c.done`, and return the shared result with
   `shared == true`. A follower launches nothing.
2. Otherwise this caller is the *owner*: create a fresh `call`, register it under
   the key, and unlock. Registration happens before the `go` statement so any
   caller that locks the mutex next will find the call and follow it.
3. Launch exactly one goroutine -- this is the "your first goroutine" of the
   lesson. It runs `fn`, then under the mutex records the result, computes
   `shared` from `dups`, deletes the key, and unlocks. It closes `done` last.
4. The owner blocks on `<-c.done` just like the followers and returns the same
   result.

Two ordering facts make this correct and race-free. First, `delete(inflight, key)`
happens under the mutex, so once the compute finishes no further caller can attach
to that call -- a late caller that locks the mutex finds the key gone and becomes
the owner of a fresh compute. That is the deliberate "no retention" behavior: this
is a stampede guard for the window while a compute is in flight, not a cache that
holds results. Second, every write to `dups`, `val`, `err`, and `shared` happens
under the mutex and *before* `close(done)`; every reader reads them *after*
`<-done`. The close/receive edge and the mutex together establish happens-before,
so `-race` sees no data race even though many goroutines read the same fields.

The critical property is that `fn` runs once per in-flight key-set no matter how
the schedule interleaves. If it ran twice, the guard would have failed and the
stampede it exists to prevent would be back.

Create `singleflight.go`:

```go
package singleflight

import "sync"

// call is one in-flight compute for a key. done is closed exactly once, by the
// owner's compute goroutine, to broadcast the result to every waiter. dups, val,
// err, and shared are written only under Group.mu (dups while the call is still
// in flight, the rest once at completion) and are safe to read after done closes.
type call[V any] struct {
	done   chan struct{}
	dups   int // number of extra callers that attached instead of launching
	val    V
	err    error
	shared bool
}

// Group deduplicates concurrent computes by key. The first caller for an in-flight
// key launches exactly one goroutine to run fn; every concurrent caller for that
// same key attaches to the in-flight call and shares its single result. The zero
// value is ready to use.
type Group[K comparable, V any] struct {
	mu       sync.Mutex
	inflight map[K]*call[V]
}

// Do returns the value for key. If no compute for key is in flight, the caller
// launches ONE goroutine running fn and waits on it; concurrent callers for the
// same key do not launch -- they block on the in-flight call and receive its
// result. shared reports whether the returned result was shared with at least one
// other caller. Once fn completes the key is removed, so the next call recomputes:
// this is a stampede guard, not a cache with retention.
func (g *Group[K, V]) Do(key K, fn func() (V, error)) (v V, err error, shared bool) {
	g.mu.Lock()
	if g.inflight == nil {
		g.inflight = make(map[K]*call[V])
	}
	if c, ok := g.inflight[key]; ok {
		c.dups++ // attach: do not launch, wait for the in-flight compute
		g.mu.Unlock()
		<-c.done
		return c.val, c.err, true
	}
	c := &call[V]{done: make(chan struct{})}
	g.inflight[key] = c
	g.mu.Unlock()

	// This caller owns the key: launch exactly one goroutine to run fn.
	go func() {
		val, ferr := fn()
		g.mu.Lock()
		c.val, c.err = val, ferr
		c.shared = c.dups > 0
		delete(g.inflight, key) // no retention: the next Do recomputes
		g.mu.Unlock()
		close(c.done) // broadcast to every waiter
	}()

	<-c.done
	return c.val, c.err, c.shared
}
```

### The runnable demo

The demo shows three deterministic facts: a resolved key recomputes on the next
call (no retention), distinct keys compute independently, and however the
launch/attach race resolves, every concurrent caller observes the same value.

Create `cmd/demo/main.go`:

```go
package main

import (
	"fmt"
	"sync"
	"sync/atomic"

	"example.com/singleflight"
)

func main() {
	var g singleflight.Group[string, int]
	var computes atomic.Int64

	// price recomputes the value for a key and counts how many times it actually
	// ran -- the stampede signal we want to keep at exactly one per key-set.
	price := func(key string) func() (int, error) {
		return func() (int, error) {
			computes.Add(1)
			return len(key) * 100, nil
		}
	}

	// Phase 1: one key, called twice in sequence. Each cold call launches the
	// compute; there is no retention, so the second call recomputes.
	v1, _, s1 := g.Do("prices:eu", price("prices:eu"))
	v2, _, s2 := g.Do("prices:eu", price("prices:eu"))
	fmt.Println("phase 1: sequential calls on one key (no retention)")
	fmt.Printf("  call 1 -> value=%d shared=%v\n", v1, s1)
	fmt.Printf("  call 2 -> value=%d shared=%v\n", v2, s2)
	fmt.Printf("  computes so far: %d\n", computes.Load())

	// Phase 2: distinct keys are independent -- each launches its own compute.
	va, _, _ := g.Do("region:us", price("region:us"))
	vb, _, _ := g.Do("region:eu", price("region:eu"))
	fmt.Println("phase 2: distinct keys compute independently")
	fmt.Printf("  region:us -> value=%d\n", va)
	fmt.Printf("  region:eu -> value=%d\n", vb)
	fmt.Printf("  computes so far: %d\n", computes.Load())

	// Phase 3: many concurrent callers on one key. However the launch/attach race
	// resolves, every caller observes the same value -- that is the guard.
	const callers = 6
	values := make([]int, callers)
	var wg sync.WaitGroup
	for i := range callers {
		wg.Go(func() {
			v, _, _ := g.Do("inventory:sku-42", price("inventory:sku-42"))
			values[i] = v
		})
	}
	wg.Wait()

	same := true
	for _, v := range values {
		if v != values[0] {
			same = false
		}
	}
	fmt.Println("phase 3: 6 concurrent callers on one key")
	fmt.Printf("  all %d callers observed value=%d: %v\n", callers, values[0], same)
}
```

Run it:

```bash
go run ./cmd/demo
```

Expected output:

```
phase 1: sequential calls on one key (no retention)
  call 1 -> value=900 shared=false
  call 2 -> value=900 shared=false
  computes so far: 2
phase 2: distinct keys compute independently
  region:us -> value=900
  region:eu -> value=900
  computes so far: 4
phase 3: 6 concurrent callers on one key
  all 6 callers observed value=1600: true
```

### Tests

The concurrent tests use a deterministic barrier instead of a sleep: because the
test lives in the `singleflight` package, it can spin (yielding with
`runtime.Gosched`) until the in-flight call's `dups` counter shows that every
follower has attached, and only then release the gate that `fn` is blocked on.
Waiting on `dups`, not merely on the callers having started, is what makes
exactly-once deterministic: it closes the window in which a late caller could find
the key already deleted and launch a second compute.

`TestConcurrentCallersLaunchFnOnce` holds `fn` at the gate until all M callers
attach, then asserts `fn` ran exactly once, every caller got the identical value
and nil error, and at least M-1 callers report `shared`. `TestDistinctKeysRunConcurrently`
blocks four distinct keys on one gate at the same time and asserts each `fn` ran
once -- they are independent. `TestRecomputesAfterResolve` proves no retention: a
second `Do` after the first resolved runs `fn` again. `TestErrorSharedToAllWaiters`
fans a single error out to every waiter via `errors.Is`. `TestNoGoroutineLeak`
checks `runtime.NumGoroutine` returns to baseline after all calls resolve.

Create `singleflight_test.go`:

```go
package singleflight

import (
	"errors"
	"runtime"
	"sync"
	"sync/atomic"
	"testing"
)

// waitForDups spins (yielding, not sleeping) until the in-flight call for key has
// exactly want extra callers attached, proving every waiter has joined before the
// test releases the gate. Returns the last observed count and whether it matched.
func waitForDups[V any](g *Group[string, V], key string, want int) (int, bool) {
	last := -1
	for range 1_000_000 {
		g.mu.Lock()
		if c := g.inflight[key]; c != nil {
			last = c.dups
		}
		g.mu.Unlock()
		if last == want {
			return last, true
		}
		runtime.Gosched()
	}
	return last, false
}

// waitForInflight spins until the map holds exactly n in-flight keys.
func waitForInflight[V any](g *Group[string, V], n int) bool {
	for range 1_000_000 {
		g.mu.Lock()
		got := len(g.inflight)
		g.mu.Unlock()
		if got == n {
			return true
		}
		runtime.Gosched()
	}
	return false
}

// waitGoroutines polls (yielding) until NumGoroutine drops to at most target.
func waitGoroutines(target int) bool {
	for range 2000 {
		if runtime.NumGoroutine() <= target {
			return true
		}
		runtime.Gosched()
	}
	return runtime.NumGoroutine() <= target
}

func TestConcurrentCallersLaunchFnOnce(t *testing.T) {
	var g Group[string, int]
	var calls atomic.Int64
	gate := make(chan struct{})
	const M = 8

	fn := func() (int, error) {
		calls.Add(1)
		<-gate // hold the single compute until every caller has attached
		return 4242, nil
	}

	type result struct {
		v      int
		err    error
		shared bool
	}
	results := make([]result, M)

	var wg sync.WaitGroup
	for i := range M {
		wg.Go(func() {
			v, err, sh := g.Do("k", fn)
			results[i] = result{v, err, sh} // disjoint slot per caller
		})
	}

	if got, ok := waitForDups(&g, "k", M-1); !ok {
		t.Fatalf("callers did not all attach: dups=%d want %d", got, M-1)
	}
	close(gate)
	wg.Wait()

	if n := calls.Load(); n != 1 {
		t.Fatalf("fn ran %d times, want exactly 1", n)
	}
	sharedCount := 0
	for i, r := range results {
		if r.v != 4242 {
			t.Fatalf("caller %d value=%d, want 4242", i, r.v)
		}
		if r.err != nil {
			t.Fatalf("caller %d err=%v, want nil", i, r.err)
		}
		if r.shared {
			sharedCount++
		}
	}
	if sharedCount < M-1 {
		t.Fatalf("shared==true for %d callers, want at least %d", sharedCount, M-1)
	}
}

func TestDistinctKeysRunConcurrently(t *testing.T) {
	var g Group[string, int]
	keys := []string{"a", "b", "c", "d"}
	gate := make(chan struct{})

	counters := make(map[string]*atomic.Int64, len(keys))
	for _, k := range keys {
		counters[k] = new(atomic.Int64)
	}

	var wg sync.WaitGroup
	for _, k := range keys {
		wg.Go(func() {
			g.Do(k, func() (int, error) {
				counters[k].Add(1)
				<-gate // all keys block here at once: they run concurrently
				return len(k), nil
			})
		})
	}

	if !waitForInflight(&g, len(keys)) {
		t.Fatalf("not all keys reached in-flight concurrently")
	}
	close(gate)
	wg.Wait()

	for _, k := range keys {
		if n := counters[k].Load(); n != 1 {
			t.Fatalf("key %q fn ran %d times, want 1", k, n)
		}
	}
}

func TestRecomputesAfterResolve(t *testing.T) {
	var g Group[string, int]
	var calls atomic.Int64
	fn := func() (int, error) {
		calls.Add(1)
		return 7, nil
	}

	v1, err1, shared1 := g.Do("k", fn)
	if v1 != 7 || err1 != nil {
		t.Fatalf("first Do = (%d,%v), want (7,nil)", v1, err1)
	}
	if shared1 {
		t.Fatalf("first Do shared=true, want false (no other caller)")
	}

	v2, _, _ := g.Do("k", fn)
	if v2 != 7 {
		t.Fatalf("second Do value=%d, want 7", v2)
	}
	if n := calls.Load(); n != 2 {
		t.Fatalf("fn ran %d times, want 2 (no retention between resolved calls)", n)
	}
}

func TestErrorSharedToAllWaiters(t *testing.T) {
	var g Group[string, int]
	var calls atomic.Int64
	boom := errors.New("compute failed")
	gate := make(chan struct{})
	const M = 6

	fn := func() (int, error) {
		calls.Add(1)
		<-gate
		return 0, boom
	}

	errs := make([]error, M)
	var wg sync.WaitGroup
	for i := range M {
		wg.Go(func() {
			_, err, _ := g.Do("k", fn)
			errs[i] = err
		})
	}

	if got, ok := waitForDups(&g, "k", M-1); !ok {
		t.Fatalf("callers did not all attach: dups=%d want %d", got, M-1)
	}
	close(gate)
	wg.Wait()

	if n := calls.Load(); n != 1 {
		t.Fatalf("fn ran %d times, want exactly 1", n)
	}
	for i := range M {
		if !errors.Is(errs[i], boom) {
			t.Fatalf("caller %d err=%v, want boom propagated", i, errs[i])
		}
	}
}

func TestNoGoroutineLeak(t *testing.T) {
	base := runtime.NumGoroutine()

	var g Group[string, int]
	var calls atomic.Int64
	gate := make(chan struct{})
	const M = 10

	fn := func() (int, error) {
		calls.Add(1)
		<-gate
		return 1, nil
	}

	var wg sync.WaitGroup
	for range M {
		wg.Go(func() {
			g.Do("k", fn)
		})
	}
	if _, ok := waitForDups(&g, "k", M-1); !ok {
		t.Fatalf("callers did not all attach")
	}
	close(gate)
	wg.Wait()

	if !waitGoroutines(base) {
		t.Fatalf("goroutines did not return to baseline: base=%d now=%d",
			base, runtime.NumGoroutine())
	}
}
```

## Review

`Do` is correct when `fn` runs exactly once per in-flight key-set and its single
result reaches every concurrent caller unchanged -- value, error, and the `shared`
flag. The invariant that guarantees it is that both the launch-or-attach decision
and the `delete` from `inflight` happen under one mutex: a follower can only attach
while the owner's call is still registered, and once the compute deletes the key no
new follower can join, so the "did a compute already start" question always has one
answer. The result fields are published under the mutex and before `close(done)`
and read only after `<-done`, so the fan-out is race-free -- that is what
`-race -count=2` verifies. The tests pin exactly-once with a barrier on the `dups`
counter rather than a sleep, which is why they are deterministic. This is the
pattern behind production `singleflight`: it collapses a thundering herd of
identical cache-miss recomputations into a single upstream call, so an expired hot
key costs the database one query instead of N.

## Resources

- [golang.org/x/sync/singleflight](https://pkg.go.dev/golang.org/x/sync/singleflight) -- the production implementation of this exact pattern, with `Do`, `DoChan`, and `Forget`.
- [The Go Memory Model](https://go.dev/ref/mem) -- the channel-close and mutex happens-before rules that make the shared result fields safe to read after `done` closes.
- [sync.Mutex](https://pkg.go.dev/sync#Mutex) -- the single lock that guards the launch-or-attach decision and the in-flight map.
- [sync.WaitGroup.Go](https://pkg.go.dev/sync#WaitGroup.Go) -- the Go 1.25 launch-and-join helper used by the demo and tests.

---

Back to [00-concepts.md](00-concepts.md) | Prev: [11-outbox-relay-batch-contiguous-cursor.md](11-outbox-relay-batch-contiguous-cursor.md) | Next: [13-request-hedging-backup-replica-leakfree.md](13-request-hedging-backup-replica-leakfree.md)
