# 23. Request Coalescing with Singleflight

When a cache entry expires, hundreds of goroutines may race to refill it at the
same time. Every goroutine calls the same slow backend, the backend is hit N
times instead of once, and the latency spike lasts until all N calls complete.
This is the *thundering herd* or *cache stampede* problem. Request coalescing
solves it by deduplicating in-flight calls: the first caller executes the work
and all late arrivals block on the same result. The standard library provides
this pattern via `sync.Once` for one-time initialisation, but for repeatable
deduplication of keyed calls you need a `Group` that maps keys to in-flight
calls and evicts each entry after the call completes.

```text
coalesce/
  go.mod
  internal/coalesce/coalesce.go
  internal/coalesce/coalesce_test.go
  cmd/demo/main.go
```

Module path: `example.com/coalesce`. Set up with:

```bash
mkdir -p ~/go-exercises/coalesce/internal/coalesce ~/go-exercises/coalesce/cmd/demo
cd ~/go-exercises/coalesce
go mod init example.com/coalesce
```

## Concepts

### The Thundering Herd Problem

Consider a read-through cache for a database row keyed by user ID. When the
cache entry expires, the next N goroutines that miss all see a cold entry and
all dispatch their own database query. In a service handling 10 000 req/s, "N"
can be thousands. The database receives a burst that it is not sized for, the
latency for those requests grows, and the responses all return the same row.
Coalescing deduplicates that burst: only one goroutine queries the database;
the others wait and share the result.

### How Group Works

`Group` maintains a map from key string to `*call`. A `*call` holds a
`sync.WaitGroup` set to 1, plus the result and error. When `Do(key, fn)` is
called:

1. Lock the mutex and check the map.
2. If an in-flight `*call` exists for `key`, unlock and call `c.wg.Wait()`.
   Return `c.val, c.err, shared=true` once the wait returns.
3. If no call exists, create one, put it in the map, unlock, call `fn()`,
   store the result in the call, call `c.wg.Done()`, then remove the key and
   return `shared=false`.

The `shared` return value is true for every caller that waited on someone
else's result. The function `fn` is invoked exactly once per in-flight window.

### Deduplication vs. Cancellation

Singleflight deduplicates but does not cancel. If `fn` returns an error, every
waiter receives that same error. If one waiter no longer needs the result (e.g.
its context was cancelled), there is no way to abort the in-flight call because
other waiters may still need it. For context-aware cancellation you need a
separate mechanism layered on top (e.g. check `ctx.Err()` before acting on the
shared result).

### When Not to Use Singleflight

Singleflight is for global, idempotent resources: cache refill, remote config
reload, DNS lookup. Do not use it when each caller needs independent execution:
per-user writes, calls whose side effects must happen once per caller, or
authenticated requests where the first caller's credentials should not be reused
by others.

### Forget

`Group.Forget(key)` removes the key from the map immediately. The next call to
`Do(key, fn)` starts a fresh in-flight call. Use `Forget` when a backend update
invalidates the cached result mid-flight, or when you want a TTL-based refresh
without waiting for a caller to trigger the miss naturally.

## Exercises

### Exercise 1: Implement Group

Create `internal/coalesce/coalesce.go`. Implement `Group` using only `sync`
from the standard library so the lesson compiles without network access:

```go
package coalesce

import "sync"

type call struct {
	wg  sync.WaitGroup
	val any
	err error
}

// Group deduplicates concurrent calls for the same key.
type Group struct {
	mu sync.Mutex
	m  map[string]*call
}

// Do executes fn if no call for key is in-flight; otherwise it waits for the
// in-flight call and returns the shared result. The shared return value is true
// when the result was produced by another goroutine.
func (g *Group) Do(key string, fn func() (any, error)) (v any, err error, shared bool) {
	g.mu.Lock()
	if g.m == nil {
		g.m = make(map[string]*call)
	}
	if c, ok := g.m[key]; ok {
		g.mu.Unlock()
		c.wg.Wait()
		return c.val, c.err, true
	}
	c := new(call)
	c.wg.Add(1)
	g.m[key] = c
	g.mu.Unlock()

	c.val, c.err = fn()
	c.wg.Done()

	g.mu.Lock()
	delete(g.m, key)
	g.mu.Unlock()

	return c.val, c.err, false
}

// Forget removes key from the in-flight map so the next Do call starts fresh.
func (g *Group) Forget(key string) {
	g.mu.Lock()
	delete(g.m, key)
	g.mu.Unlock()
}
```

### Exercise 2: Test the Deduplication Invariants

Create `internal/coalesce/coalesce_test.go`.

The key to making the concurrency tests reliable: use a start-gate channel so
all goroutines enter `Do` at the same instant, and make `fn` block until a
release signal so late-arriving goroutines actually see an in-flight call.

```go
package coalesce_test

import (
	"errors"
	"fmt"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"example.com/coalesce/internal/coalesce"
)

var errBackend = errors.New("backend error")

// TestSameKey_OnlyOneCall verifies that 100 concurrent goroutines requesting
// the same key result in exactly one invocation of fn.
func TestSameKey_OnlyOneCall(t *testing.T) {
	t.Parallel()

	var g coalesce.Group
	var calls atomic.Int64

	// release is closed after all goroutines have started so fn holds the
	// in-flight window open long enough for everyone to queue up.
	release := make(chan struct{})
	fn := func() (any, error) {
		calls.Add(1)
		<-release
		return "data", nil
	}

	const n = 100
	var wg sync.WaitGroup
	results := make([]any, n)
	wg.Add(n)
	for i := 0; i < n; i++ {
		i := i
		go func() {
			defer wg.Done()
			v, err, _ := g.Do("key", fn)
			if err != nil {
				t.Errorf("goroutine %d: unexpected error: %v", i, err)
			}
			results[i] = v
		}()
	}

	// Give goroutines time to pile up on the in-flight call, then release fn.
	time.Sleep(20 * time.Millisecond)
	close(release)
	wg.Wait()

	if got := calls.Load(); got != 1 {
		t.Fatalf("fn called %d times, want 1", got)
	}
	for i, r := range results {
		if r != "data" {
			t.Fatalf("goroutine %d got %v, want data", i, r)
		}
	}
}

// TestDifferentKeys_ExactCallCount verifies that goroutines across N distinct
// keys each produce exactly one backend call per key during an overlapping
// window.
func TestDifferentKeys_ExactCallCount(t *testing.T) {
	t.Parallel()

	var g coalesce.Group
	var calls atomic.Int64

	release := make(chan struct{})
	fn := func() (any, error) {
		calls.Add(1)
		<-release
		return "data", nil
	}

	const goroutines = 100
	const keys = 10

	var wg sync.WaitGroup
	wg.Add(goroutines)
	for i := 0; i < goroutines; i++ {
		key := fmt.Sprintf("key-%d", i%keys)
		go func(k string) {
			defer wg.Done()
			g.Do(k, fn) //nolint:errcheck
		}(key)
	}

	time.Sleep(20 * time.Millisecond)
	close(release)
	wg.Wait()

	if got := calls.Load(); got != keys {
		t.Fatalf("fn called %d times, want %d", got, keys)
	}
}

// TestErrorPropagation verifies that all waiters receive the same error when
// fn fails.
func TestErrorPropagation(t *testing.T) {
	t.Parallel()

	var g coalesce.Group

	release := make(chan struct{})
	fn := func() (any, error) {
		<-release
		return nil, errBackend
	}

	const n = 50
	var wg sync.WaitGroup
	errs := make([]error, n)
	wg.Add(n)
	for i := 0; i < n; i++ {
		i := i
		go func() {
			defer wg.Done()
			_, err, _ := g.Do("fail", fn)
			errs[i] = err
		}()
	}

	time.Sleep(20 * time.Millisecond)
	close(release)
	wg.Wait()

	for i, err := range errs {
		if !errors.Is(err, errBackend) {
			t.Fatalf("goroutine %d: got %v, want errBackend", i, err)
		}
	}
}

// TestForget_AllowsFreshCall verifies that Forget causes the next Do to invoke
// fn again rather than sharing the previous result.
func TestForget_AllowsFreshCall(t *testing.T) {
	t.Parallel()

	var g coalesce.Group
	var calls atomic.Int64

	fn := func() (any, error) {
		calls.Add(1)
		return "data", nil
	}

	g.Do("k", fn) //nolint:errcheck
	g.Forget("k")
	g.Do("k", fn) //nolint:errcheck

	if got := calls.Load(); got != 2 {
		t.Fatalf("fn called %d times after Forget, want 2", got)
	}
}

func ExampleGroup() {
	var g coalesce.Group
	var calls atomic.Int64

	do := func() (any, error) {
		calls.Add(1)
		return "data", nil
	}

	v1, _, _ := g.Do("k", do)
	// First call is done; key is gone from the map, so this is a fresh call.
	v2, _, _ := g.Do("k", do)
	fmt.Println(v1, v2, calls.Load())
	// Output: data data 2
}
```

### Exercise 3: Demo Binary

Create `cmd/demo/main.go` to show coalescing behaviour through the exported API:

```go
package main

import (
	"fmt"
	"sync"
	"sync/atomic"
	"time"

	"example.com/coalesce/internal/coalesce"
)

func main() {
	var g coalesce.Group
	var backendCalls atomic.Int64

	fetch := func() (any, error) {
		backendCalls.Add(1)
		time.Sleep(50 * time.Millisecond)
		return "result", nil
	}

	const n = 20
	var wg sync.WaitGroup
	wg.Add(n)
	for i := 0; i < n; i++ {
		go func() {
			defer wg.Done()
			v, _, shared := g.Do("resource", fetch)
			_ = v
			_ = shared
		}()
	}
	wg.Wait()

	fmt.Printf("backend called %d time(s) for %d concurrent requests\n", backendCalls.Load(), n)
}
```

## Common Mistakes

### Sharing fn Results Across Unrelated Callers

Wrong: using a single `sync.Once` to deduplicate a per-user fetch.

What happens: the first user's data is returned to every subsequent caller,
leaking data across security boundaries.

Fix: `Group` is correct only for truly shared, user-independent data (public
config, shared reference tables). Per-user fetches must use separate keys or
forgo coalescing entirely.

### Ignoring the Shared Return Value in Metrics

Wrong: counting `Do` calls as backend calls without checking `shared`.

What happens: your metrics show 1000 backend calls when only 10 actually
happened, making the optimisation invisible.

Fix: only count calls where `shared == false` as real backend calls. Log
`shared == true` calls as coalesced hits.

### Forgetting That fn Errors Reach All Waiters

Wrong: returning a transient error from fn without retrying, assuming only the
"original" caller sees the failure.

What happens: a single flaky backend call propagates that error to every waiter.
A thundering herd of errors replaces a thundering herd of requests.

Fix: wrap fn with retry logic before passing it to `Do`. The coalescing layer
should not retry; the caller-supplied function should be reliable or handle its
own retries.

### Using Forget Before fn Returns

Wrong: calling `Forget(key)` inside fn (the in-flight call) to allow
parallel requests.

What happens: a second `Do` call for the same key starts a new call while the
first is still running, defeating the deduplication invariant.

Fix: call `Forget` only after `Do` returns, from an external TTL goroutine or
from the consumer code that detected a stale result.

## Verification

From `~/go-exercises/coalesce`:

```bash
test -z "$(gofmt -l .)"
go vet ./...
go build ./...
go test -count=1 -race ./...
```

All four commands must exit 0. The test output will show the example function
result and confirm no data races under -race.

## Summary

- `Group.Do` deduplicates concurrent calls for the same key: fn executes once;
  all waiters share the result.
- `shared == true` means the caller received another goroutine's result.
- `shared == false` means this goroutine actually ran fn.
- `Forget` evicts a key so the next `Do` starts a fresh call.
- Error propagation is total: one failure in fn reaches all current waiters.
- Do not use coalescing for per-user or non-idempotent operations.

## What's Next

Next: [Streaming Pipeline with Backpressure](../24-streaming-pipeline-backpressure/24-streaming-pipeline-backpressure.md).

## Resources

- [sync.Once documentation](https://pkg.go.dev/sync#Once) - standard library one-shot deduplication
- [Cache stampede (Wikipedia)](https://en.wikipedia.org/wiki/Cache_stampede) - the problem this pattern solves
- [singleflight source code (x/sync)](https://cs.opensource.google/go/x/sync/+/refs/tags/v0.6.0:singleflight/singleflight.go) - reference implementation this lesson reimplements
- [Go Blog: Share Memory By Communicating](https://go.dev/blog/codelab-share) - channel vs mutex philosophy
- [golang.org/x/sync/singleflight](https://pkg.go.dev/golang.org/x/sync/singleflight) - standard extended package API
