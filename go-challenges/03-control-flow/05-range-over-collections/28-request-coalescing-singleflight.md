# Exercise 28: Request Coalescing with Single-Flight Pattern

**Nivel: Intermedio** — validacion rapida (un test corto).

When a cache entry expires under real traffic, it is common for dozens of
requests for that exact key to arrive within microseconds of each other —
every one of them a cache miss that, left unchecked, fires its own identical
database query or upstream API call at the same instant. That stampede can
turn one slow backend call into a self-inflicted denial of service the
moment the cache entry that was absorbing the load disappears. Request
coalescing fixes this at the source: the first caller for a key actually
runs the expensive fetch, every other caller that arrives while it is still
running joins that one in-flight call and waits for its result instead of
starting a redundant fetch of its own. This module builds that coalescing
group, and ranges the list of joined waiters to deliver the single fetched
result to every one of them over a channel. The module is fully self-
contained: its own `go mod init`, no external dependencies.

## What you'll build

```text
coalesce/                   independent module: example.com/request-coalescing-singleflight
  go.mod                    go 1.24
  coalesce.go               type Group; Do(key, fn), Waiting(key)
  cmd/
    demo/
      main.go               runnable demo: 3 concurrent callers, 1 fetch, 2 shared results
  coalesce_test.go           table-style tests: solo caller, error propagation, concurrent coalescing under -race
```

- Files: `coalesce.go`, `cmd/demo/main.go`, `coalesce_test.go`.
- Implement: `Group.Do(key string, fn func() (string, error)) (string, error, bool)`
  and `Group.Waiting(key string) int`, both synchronized under one
  `sync.Mutex`.
- Test: a solo-caller case, an error-propagation case, and a concurrent-
  coalescing case proving `fn` runs exactly once for N simultaneous callers.
- Verify: `go test -count=1 -race ./...`

Set up the module:

```bash
mkdir -p go-solutions/03-control-flow/05-range-over-collections/28-request-coalescing-singleflight/cmd/demo
cd go-solutions/03-control-flow/05-range-over-collections/28-request-coalescing-singleflight
go mod edit -go=1.24
```

### Why the waiter list is ranged, not broadcast on a shared channel

A tempting shortcut is to give every in-flight call a single shared
`chan result` and have every waiter receive from it, relying on Go's channel
semantics to fan the value out — but an unbuffered or single-slot channel
delivers a value to exactly *one* receiver, not to all of them; the other
waiters would either block forever or race for the one delivered value.
`call.waiters` instead holds one independent, buffer-of-one channel per
joined caller, appended under the lock as each caller arrives. When the
leader's `fn` returns, `Do` takes the lock one last time, reads the finished
`waiters` slice out of the call, deletes the call from the map so the *next*
request for this key starts a fresh fetch instead of joining a dead one,
and only then ranges `waiters` outside the lock to send the identical
`result{val, err}` into each channel and close it — a fan-out that a single
shared channel cannot express, but an explicit range over a slice of
channels can.

The `shared` boolean return exists because a caller frequently needs to know
whether it paid for the fetch or rode along on someone else's: a caller that
gets `shared == false` might log the fetch latency as its own, while a
caller that gets `shared == true` should not double-count that latency
against its own request. `Waiting` is not part of the coalescing logic at
all — it is an observability hook, the same shape as a metrics gauge you
would export in production ("N callers currently piggybacking on key X"),
and reading `len(c.waiters)` under the same lock `Do` uses is what makes it
safe to call concurrently with `Do` itself.

Create `coalesce.go`:

```go
package coalesce

import "sync"

// result carries a Do call's outcome to every waiter joined on it.
type result struct {
	val string
	err error
}

// call tracks one in-flight fetch: the goroutines that arrived after the
// first one for the same key each register a channel here and block on it
// instead of re-running fn.
type call struct {
	waiters []chan result
}

// Group deduplicates concurrent Do calls that share a key, so a stampede of
// callers asking for the same expensive resource at the same moment causes
// exactly one fn invocation, not one per caller.
type Group struct {
	mu    sync.Mutex
	calls map[string]*call
}

// New builds an empty Group.
func New() *Group {
	return &Group{calls: make(map[string]*call)}
}

// Do runs fn for key if no call for that key is already in flight, or joins
// the in-flight call and waits for its result if one exists. The third
// return is true for every caller except the one that actually executed fn.
func (g *Group) Do(key string, fn func() (string, error)) (val string, err error, shared bool) {
	g.mu.Lock()
	if c, ok := g.calls[key]; ok {
		ch := make(chan result, 1)
		c.waiters = append(c.waiters, ch)
		g.mu.Unlock()

		r := <-ch
		return r.val, r.err, true
	}

	c := &call{}
	g.calls[key] = c
	g.mu.Unlock()

	val, err = fn()

	g.mu.Lock()
	waiters := c.waiters
	delete(g.calls, key)
	g.mu.Unlock()

	// Range every waiter that joined while fn was running and deliver the
	// single result to each of them exactly once.
	for _, ch := range waiters {
		ch <- result{val: val, err: err}
		close(ch)
	}
	return val, err, false
}

// Waiting reports how many callers are currently blocked waiting on the
// in-flight call for key, not counting the caller executing fn itself. It
// exists for observability (a metrics gauge on coalescing pressure) and lets
// callers deterministically detect that other goroutines have joined an
// in-flight call.
func (g *Group) Waiting(key string) int {
	g.mu.Lock()
	defer g.mu.Unlock()

	c, ok := g.calls[key]
	if !ok {
		return 0
	}
	return len(c.waiters)
}
```

### The runnable demo

The demo launches 3 concurrent callers for the same key against a `fetch`
that blocks on a `start` channel. It spins on `Waiting` until the other 2
callers have registered as joiners — a deterministic handshake, not a timed
sleep — before releasing `fetch`, so the printed counts are exact every run.

Create `cmd/demo/main.go`:

```go
package main

import (
	"fmt"
	"runtime"
	"sync"
	"sync/atomic"

	"example.com/request-coalescing-singleflight"
)

func main() {
	g := coalesce.New()

	var fetches int32
	start := make(chan struct{})
	fetch := func() (string, error) {
		atomic.AddInt32(&fetches, 1)
		<-start // held open until every caller below has joined this call
		return "db-row-42", nil
	}

	const callers = 3
	type outcome struct {
		val    string
		shared bool
	}
	results := make(chan outcome, callers)

	var wg sync.WaitGroup
	for i := 0; i < callers; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			val, _, shared := g.Do("user:42", fetch)
			results <- outcome{val: val, shared: shared}
		}()
	}

	// Spin until the other callers-1 goroutines have joined the in-flight
	// call as waiters, so releasing start below cannot race a joiner that
	// has not yet registered.
	for g.Waiting("user:42") < callers-1 {
		runtime.Gosched()
	}
	close(start)

	wg.Wait()
	close(results)

	shared := 0
	for r := range results {
		if r.shared {
			shared++
		}
		if r.val != "db-row-42" {
			fmt.Printf("unexpected value: %q\n", r.val)
		}
	}

	fmt.Printf("fetches=%d shared=%d of %d\n", atomic.LoadInt32(&fetches), shared, callers)
}
```

Run it:

```bash
go run ./cmd/demo
```

Expected output:

```text
fetches=1 shared=2 of 3
```

### Tests

A solo-caller case proves the simple path returns `shared == false`, an
error-propagation case proves a failing `fn`'s error reaches the caller
unchanged, and the concurrency test — the one that actually exercises the
coalescing guarantee — spins on `Waiting` exactly like the demo before
releasing 4 concurrent callers' shared `fn`, then asserts it ran exactly
once.

Create `coalesce_test.go`:

```go
package coalesce

import (
	"errors"
	"runtime"
	"sync"
	"sync/atomic"
	"testing"
)

func TestDoSingleCallerRunsFn(t *testing.T) {
	t.Parallel()

	g := New()
	val, err, shared := g.Do("k", func() (string, error) { return "v", nil })
	if err != nil {
		t.Fatalf("Do() err = %v, want nil", err)
	}
	if val != "v" {
		t.Fatalf("Do() val = %q, want %q", val, "v")
	}
	if shared {
		t.Fatalf("Do() shared = true for the only caller, want false")
	}
}

func TestDoPropagatesError(t *testing.T) {
	t.Parallel()

	g := New()
	wantErr := errors.New("upstream unavailable")
	_, err, _ := g.Do("k", func() (string, error) { return "", wantErr })
	if !errors.Is(err, wantErr) {
		t.Fatalf("Do() err = %v, want %v", err, wantErr)
	}
}

func TestDoCoalescesConcurrentCallers(t *testing.T) {
	t.Parallel()

	g := New()
	var fetches int32
	start := make(chan struct{})
	fn := func() (string, error) {
		atomic.AddInt32(&fetches, 1)
		<-start
		return "computed-once", nil
	}

	const callers = 4
	type outcome struct {
		val    string
		shared bool
	}
	results := make(chan outcome, callers)

	var wg sync.WaitGroup
	for i := 0; i < callers; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			val, err, shared := g.Do("shared-key", fn)
			if err != nil {
				t.Errorf("Do() err = %v, want nil", err)
			}
			results <- outcome{val: val, shared: shared}
		}()
	}

	for g.Waiting("shared-key") < callers-1 {
		runtime.Gosched()
	}
	close(start)

	wg.Wait()
	close(results)

	sharedCount := 0
	for r := range results {
		if r.val != "computed-once" {
			t.Errorf("Do() val = %q, want %q", r.val, "computed-once")
		}
		if r.shared {
			sharedCount++
		}
	}

	if got := atomic.LoadInt32(&fetches); got != 1 {
		t.Fatalf("fn invocations = %d, want exactly 1", got)
	}
	if sharedCount != callers-1 {
		t.Fatalf("shared callers = %d, want %d", sharedCount, callers-1)
	}
}
```

Run it:

```bash
go test -count=1 -race ./...
```

## Review

The group is correct when exactly one `fn` invocation serves every caller
that arrives while it is running, every joined waiter receives the same
`(val, err)` pair, and the call is removed from `g.calls` before its
result is delivered, so the very next request for that key starts a fresh
fetch rather than joining a call that has already finished. The bug this
design specifically avoids is deleting the call from the map *before*
capturing its `waiters` slice: doing the delete first and the range second
(as this implementation does) still works, but doing them in the other
order — deleting only after ranging while a slow joiner is still appending
to `c.waiters` under a separately-acquired lock — would drop that joiner's
channel from the fan-out and leave it blocked forever, which is exactly the
kind of goroutine leak that a channel-based fan-in must guard against by
construction.

## Resources

- [sync.Mutex](https://pkg.go.dev/sync#Mutex)
- [golang.org/x/sync/singleflight](https://pkg.go.dev/golang.org/x/sync/singleflight) — the production package this exercise's `Group` models a simplified version of.
- [Go Specification: For statements (range over slice)](https://go.dev/ref/spec#For_statements)

---

Back to [00-concepts.md](00-concepts.md) | Prev: [27-bloom-filter-false-positive-dedup.md](27-bloom-filter-false-positive-dedup.md) | Next: [29-zipf-distribution-hot-key-tracker.md](29-zipf-distribution-hot-key-tracker.md)
