# Exercise 12: Request-Coalescing Cache Coordinator (Cache Stampede)

**Level: Advanced**

A read-through cache sits in front of an expensive backend. When many requests
for the same cold key arrive at once, a naive cache issues one backend load per
request -- a stampede that can melt the backend the instant a hot key expires.
This exercise builds the coordinator that fixes it: an actor that owns the
in-flight map, fires exactly one backend load per key, attaches every concurrent
caller to that single load, and broadcasts the one result to all of them.

This module is self-contained: its own module, a `coalesce` package, a demo, and
tests. Nothing here imports another exercise.

## What you'll build

```text
coalesce/                    independent module: example.com/coalesce
  go.mod                     go 1.26
  coalesce.go                Coordinator: New, Run, Get, BackendCalls, Shutdown, ErrShuttingDown
  cmd/demo/main.go           runnable demo: a wave coalesces to one load, then a cache hit
  coalesce_test.go           coalescing, distinct-key concurrency, cancel-mid-flight, cache hit, window-clear
```

- Files: `coalesce.go`, `cmd/demo/main.go`, `coalesce_test.go`.
- Implement: `type Coordinator`; `func New(load func(ctx context.Context, key string) (int, error)) *Coordinator`; `func (c *Coordinator) Run()`; `func (c *Coordinator) Get(ctx context.Context, key string) (int, error)`; `func (c *Coordinator) BackendCalls() int`; `func (c *Coordinator) Shutdown()`; `var ErrShuttingDown error`.
- Test: N concurrent Gets on a cold key trigger one backend call and return the identical value; distinct keys load concurrently; a cancelled caller returns `context.Cause` without blocking the other waiters or leaking; a warm key comes from cache with no new load; a fresh wave after the window clears triggers exactly one more load.
- Verify: `go test -count=1 -race ./...`

Set up the module:

```bash
mkdir -p ~/go-exercises/coalesce/cmd/demo
cd ~/go-exercises/coalesce
go mod init example.com/coalesce
go get go.uber.org/goleak
go mod tidy
```

### One load per key, one broadcast to all waiters

The coordinator is an actor: one goroutine owns two maps -- `cache` from key to
value, and `inflight` from key to a slice of waiting reply channels -- so neither
map needs a lock. Every `Get` is a request carrying its own capacity-one reply
channel. The run loop's decision on each request is a three-way branch:

1. **Cache hit.** The key is in `cache`; reply immediately with the value.
2. **Load already running.** The key is in `inflight`; append this caller's reply
   channel to the slice and return to the loop. This caller now waits for the
   broadcast; it does not touch the backend.
3. **First arrival.** The key is in neither map. Mark it in-flight (seed
   `inflight[key]` with this one reply channel), increment the single backend
   counter, and spawn one load goroutine. The goroutine calls the backend and
   reports the outcome back to the loop on a `fills` channel, which the loop
   selects on alongside new gets.

When a `fill` arrives, the loop takes the whole waiter slice, deletes the
in-flight entry, caches the value (only on success), and sends the one result to
every waiter. That last step is the crux: **the reply channels are buffered with
capacity one** precisely so this broadcast never blocks. A caller whose context
was cancelled has stopped receiving, but its buffered channel still accepts the
one value the loop sends, so the broadcast to the remaining waiters completes and
nothing leaks.

Two ownership rules make it race-free. First, the backend load runs on a
**coordinator-owned context**, never on the first caller's context -- otherwise
one caller cancelling its `Get` would abort a load that a hundred other callers
are waiting on. Second, `BackendCalls` is answered as a request on the same loop,
so the counter is read by the one goroutine that also writes it; reading it from a
struct field would be the data race the actor design exists to avoid.

Create `coalesce.go`:

```go
package coalesce

import (
	"context"
	"errors"
	"sync"
)

// ErrShuttingDown is returned once the coordinator has been shut down.
var ErrShuttingDown = errors.New("coalesce: coordinator is shutting down")

type result struct {
	value int
	err   error
}

type getReq struct {
	key   string
	reply chan result
}

type fillMsg struct {
	key   string
	value int
	err   error
}

// Coordinator is a read-through cache that coalesces concurrent requests for the
// same cold key into a single backend load. One goroutine owns cache and
// inflight, so neither needs a lock.
type Coordinator struct {
	load   func(ctx context.Context, key string) (int, error)
	gets   chan getReq
	fills  chan fillMsg
	stats  chan chan int
	quit   chan struct{}
	done   chan struct{}
	ctx    context.Context
	cancel context.CancelCauseFunc
	wg     sync.WaitGroup
	once   sync.Once
}

// New returns a Coordinator that fetches missing keys with load. Start it with
// go c.Run(). load is invoked on a coordinator-owned context, never a caller's:
// one caller cancelling its Get must not abort a load that other callers await.
func New(load func(ctx context.Context, key string) (int, error)) *Coordinator {
	ctx, cancel := context.WithCancelCause(context.Background())
	return &Coordinator{
		load:   load,
		gets:   make(chan getReq),
		fills:  make(chan fillMsg),
		stats:  make(chan chan int),
		quit:   make(chan struct{}),
		done:   make(chan struct{}),
		ctx:    ctx,
		cancel: cancel,
	}
}

// Run is the actor loop. It owns cache and inflight and the backendCalls counter,
// so all three are read and written by exactly one goroutine.
func (c *Coordinator) Run() {
	defer close(c.done)
	cache := map[string]int{}
	inflight := map[string][]chan result{}
	var backendCalls int
	for {
		select {
		case req := <-c.gets:
			if v, ok := cache[req.key]; ok {
				req.reply <- result{value: v}
				continue
			}
			if waiters, ok := inflight[req.key]; ok {
				// A load is already running for this key: attach and wait.
				inflight[req.key] = append(waiters, req.reply)
				continue
			}
			// First arrival: mark in-flight, count the single load, spawn it.
			inflight[req.key] = []chan result{req.reply}
			backendCalls++
			key := req.key
			c.wg.Go(func() {
				v, err := c.load(c.ctx, key)
				select {
				case c.fills <- fillMsg{key: key, value: v, err: err}:
				case <-c.done:
				}
			})
		case f := <-c.fills:
			waiters := inflight[f.key]
			delete(inflight, f.key)
			if f.err == nil {
				cache[f.key] = f.value
			}
			r := result{value: f.value, err: f.err}
			for _, w := range waiters {
				// Each reply channel has capacity one, so this send always
				// completes even if that caller's ctx cancelled and it stopped
				// receiving. No waiter can wedge the broadcast.
				w <- r
			}
		case rc := <-c.stats:
			rc <- backendCalls
		case <-c.quit:
			return
		}
	}
}

// Get returns the value for key, loading it through the backend at most once per
// in-flight window no matter how many callers ask concurrently.
func (c *Coordinator) Get(ctx context.Context, key string) (int, error) {
	reply := make(chan result, 1) // capacity one: the broadcast never blocks.
	select {
	case c.gets <- getReq{key: key, reply: reply}:
	case <-ctx.Done():
		return 0, context.Cause(ctx)
	case <-c.quit:
		return 0, ErrShuttingDown
	}
	select {
	case r := <-reply:
		return r.value, r.err
	case <-ctx.Done():
		return 0, context.Cause(ctx)
	case <-c.quit:
		return 0, ErrShuttingDown
	}
}

// BackendCalls returns how many times the backend load has been invoked. It is
// answered as a request on the same loop, so the count is never read concurrently
// with the writes that produce it.
func (c *Coordinator) BackendCalls() int {
	rc := make(chan int, 1)
	select {
	case c.stats <- rc:
	case <-c.quit:
		return 0
	}
	select {
	case n := <-rc:
		return n
	case <-c.quit:
		return 0
	}
}

// Shutdown stops the loop, cancels any in-flight load, and joins every goroutine
// it started. It is synchronous and idempotent.
func (c *Coordinator) Shutdown() {
	c.once.Do(func() {
		close(c.quit)
		c.cancel(ErrShuttingDown)
		<-c.done
		c.wg.Wait()
	})
}
```

### The runnable demo

The demo gates the single load on a `release` channel so the whole wave attaches
before the one result is produced -- that keeps the printed coalescing count
deterministic. It then shows a warm cache hit and a second cold key.

Create `cmd/demo/main.go`:

```go
package main

import (
	"context"
	"fmt"
	"sync"
	"time"

	"example.com/coalesce"
)

func main() {
	// A gated backend: the first (and only) load for "alpha" blocks on release
	// so every caller in the wave attaches before the single result is produced.
	// This keeps the demo output deterministic while showing true coalescing.
	release := make(chan struct{})
	load := func(ctx context.Context, key string) (int, error) {
		if key == "alpha" {
			select {
			case <-release:
			case <-ctx.Done():
				return 0, context.Cause(ctx)
			}
		}
		return len(key) * 10, nil
	}

	c := coalesce.New(load)
	go c.Run()
	defer c.Shutdown()

	const n = 8
	results := make([]int, n)
	var wg sync.WaitGroup
	for i := range n {
		wg.Go(func() {
			v, _ := c.Get(context.Background(), "alpha")
			results[i] = v
		})
	}

	// Give the wave time to attach to the single in-flight load, then release it.
	time.Sleep(20 * time.Millisecond)
	close(release)
	wg.Wait()

	allEqual := true
	for _, v := range results {
		if v != results[0] {
			allEqual = false
		}
	}
	fmt.Printf("wave of %d concurrent gets on \"alpha\": all-equal=%t value=%d\n", n, allEqual, results[0])
	fmt.Printf("backend calls after wave: %d\n", c.BackendCalls())

	warm, _ := c.Get(context.Background(), "alpha")
	fmt.Printf("warm get \"alpha\": value=%d backend-calls=%d\n", warm, c.BackendCalls())

	other, _ := c.Get(context.Background(), "beta-key")
	fmt.Printf("cold get \"beta-key\": value=%d backend-calls=%d\n", other, c.BackendCalls())
}
```

Run it:

```bash
go run ./cmd/demo
```

Expected output:

```
wave of 8 concurrent gets on "alpha": all-equal=true value=50
backend calls after wave: 1
warm get "alpha": value=50 backend-calls=1
cold get "beta-key": value=80 backend-calls=2
```

### Tests

The tests use a `gate` helper: a load that signals every invocation on a `started`
channel and blocks each one until `release` is closed. Because coalescing produces
exactly one load per key, the first arrival's `started` signal proves the load is
in flight; the loop is then free to register every other caller as a waiter, and a
generous margin lets the whole wave attach before `release` closes.
`TestColdKeyCoalescesToOneBackendCall` fires 64 concurrent Gets on one key and
asserts `BackendCalls()==1` and that all 64 receive the identical value.
`TestDistinctKeysLoadConcurrently` drains one `started` per key, proving all K
loads run at once, then asserts `BackendCalls()==K` with correct per-key values.
`TestCancelledCallerDoesNotBlockWaiters` cancels the first arrival mid-flight,
asserts it returns `context.Cause`, and asserts the two remaining waiters still
receive the shared value with one backend call -- the capacity-one reply proves
the abandoned channel never wedges the broadcast. `TestWarmKeyServedFromCache`
asserts a second Get returns from cache with no new load.
`TestFreshWaveAfterWindowClears` uses an error-returning load (errors are never
cached) so a second wave, after the in-flight window clears, triggers exactly one
more load. `TestMain` wraps everything in `goleak.VerifyTestMain`.

Create `coalesce_test.go`:

```go
package coalesce

import (
	"context"
	"errors"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"go.uber.org/goleak"
)

func TestMain(m *testing.M) {
	goleak.VerifyTestMain(m)
}

// gate returns a load that signals every invocation on started and blocks each
// one until release is closed (or the coordinator context is cancelled).
func gate(started chan<- string, release <-chan struct{}, value func(string) int, err error) func(context.Context, string) (int, error) {
	return func(ctx context.Context, key string) (int, error) {
		select {
		case started <- key:
		case <-ctx.Done():
			return 0, context.Cause(ctx)
		}
		select {
		case <-release:
		case <-ctx.Done():
			return 0, context.Cause(ctx)
		}
		return value(key), err
	}
}

func TestColdKeyCoalescesToOneBackendCall(t *testing.T) {
	const n = 64
	started := make(chan string, 1)
	release := make(chan struct{})
	c := New(gate(started, release, func(string) int { return 777 }, nil))
	go c.Run()
	defer c.Shutdown()

	var wg sync.WaitGroup
	got := make([]int, n)
	errs := make([]error, n)
	for i := range n {
		wg.Go(func() {
			got[i], errs[i] = c.Get(context.Background(), "cold")
		})
	}

	// The single load has started; the loop is now free to register every other
	// caller as a waiter. Wait for the whole wave to attach before releasing.
	<-started
	time.Sleep(50 * time.Millisecond)
	close(release)
	wg.Wait()

	if bc := c.BackendCalls(); bc != 1 {
		t.Fatalf("BackendCalls() = %d, want 1", bc)
	}
	for i := range n {
		if errs[i] != nil {
			t.Fatalf("Get #%d error = %v", i, errs[i])
		}
		if got[i] != 777 {
			t.Fatalf("Get #%d = %d, want 777 (identical shared result)", i, got[i])
		}
	}
}

func TestDistinctKeysLoadConcurrently(t *testing.T) {
	const k = 8
	keys := []string{"a", "b", "c", "d", "e", "f", "g", "h"}
	started := make(chan string, k)
	release := make(chan struct{})
	c := New(gate(started, release, func(key string) int { return int(key[0]) }, nil))
	go c.Run()
	defer c.Shutdown()

	var wg sync.WaitGroup
	got := make([]int, k)
	for i := range k {
		wg.Go(func() {
			got[i], _ = c.Get(context.Background(), keys[i])
		})
	}

	// Every distinct key must reach the backend; all k loads run at once. Draining
	// k start signals proves all k loads are in flight simultaneously.
	for range k {
		<-started
	}
	close(release)
	wg.Wait()

	if bc := c.BackendCalls(); bc != k {
		t.Fatalf("BackendCalls() = %d, want %d", bc, k)
	}
	for i := range k {
		if want := int(keys[i][0]); got[i] != want {
			t.Fatalf("Get(%q) = %d, want %d", keys[i], got[i], want)
		}
	}
}

func TestCancelledCallerDoesNotBlockWaiters(t *testing.T) {
	started := make(chan string, 1)
	release := make(chan struct{})
	c := New(gate(started, release, func(string) int { return 99 }, nil))
	go c.Run()
	defer c.Shutdown()

	cause := errors.New("caller gave up")
	ctxA, cancelA := context.WithCancelCause(context.Background())

	// A is the first arrival; it triggers the single load.
	var aVal int
	var aErr error
	doneA := make(chan struct{})
	go func() {
		defer close(doneA)
		aVal, aErr = c.Get(ctxA, "shared")
	}()
	<-started

	// B and C attach as waiters on the same in-flight load.
	var wg sync.WaitGroup
	var bVal, cVal int
	var bErr, cErr error
	wg.Go(func() { bVal, bErr = c.Get(context.Background(), "shared") })
	wg.Go(func() { cVal, cErr = c.Get(context.Background(), "shared") })
	time.Sleep(50 * time.Millisecond) // let B and C attach

	// A abandons its Get before the load completes.
	cancelA(cause)
	<-doneA
	if !errors.Is(aErr, cause) {
		t.Fatalf("cancelled caller err = %v, want %v", aErr, cause)
	}

	// The load completes; the remaining waiters must still receive the result,
	// and A's abandoned capacity-one reply must absorb the broadcast without leak.
	close(release)
	wg.Wait()

	if bErr != nil || cErr != nil {
		t.Fatalf("waiter errors: B=%v C=%v", bErr, cErr)
	}
	if bVal != 99 || cVal != 99 {
		t.Fatalf("waiters got B=%d C=%d, want 99 each", bVal, cVal)
	}
	if aVal != 0 {
		t.Fatalf("cancelled caller value = %d, want 0", aVal)
	}
	if bc := c.BackendCalls(); bc != 1 {
		t.Fatalf("BackendCalls() = %d, want 1", bc)
	}
}

func TestWarmKeyServedFromCache(t *testing.T) {
	var loads atomic.Int64
	load := func(ctx context.Context, key string) (int, error) {
		loads.Add(1)
		return 1234, nil
	}
	c := New(load)
	go c.Run()
	defer c.Shutdown()

	first, err := c.Get(context.Background(), "warm")
	if err != nil {
		t.Fatalf("first Get error = %v", err)
	}
	if bc := c.BackendCalls(); bc != 1 {
		t.Fatalf("after first Get BackendCalls() = %d, want 1", bc)
	}

	second, err := c.Get(context.Background(), "warm")
	if err != nil {
		t.Fatalf("second Get error = %v", err)
	}
	if second != first {
		t.Fatalf("cached value = %d, want %d", second, first)
	}
	if bc := c.BackendCalls(); bc != 1 {
		t.Fatalf("after cache hit BackendCalls() = %d, want 1 (no new load)", bc)
	}
	if n := loads.Load(); n != 1 {
		t.Fatalf("backend invoked %d times, want 1", n)
	}
}

func TestFreshWaveAfterWindowClears(t *testing.T) {
	const m = 16
	loadErr := errors.New("backend miss") // errors are never cached
	started := make(chan string, 1)
	release := make(chan struct{})
	// The closure reads release by reference so a second wave can install a fresh
	// gate. Each wave's write happens-before the next wave's goroutines, so there
	// is no race on the variable.
	load := func(ctx context.Context, key string) (int, error) {
		r := release
		select {
		case started <- key:
		case <-ctx.Done():
			return 0, context.Cause(ctx)
		}
		select {
		case <-r:
		case <-ctx.Done():
			return 0, context.Cause(ctx)
		}
		return 0, loadErr
	}
	coord := New(load)
	go coord.Run()
	defer coord.Shutdown()

	// wave fires m concurrent gets for the same key, all coalescing onto one
	// load, and returns once the window has fully closed (every get returned).
	wave := func() {
		var wg sync.WaitGroup
		errs := make([]error, m)
		for i := range m {
			wg.Go(func() { _, errs[i] = coord.Get(context.Background(), "k") })
		}
		<-started
		time.Sleep(50 * time.Millisecond) // let the wave attach to the one load
		close(release)
		wg.Wait()
		for i := range m {
			if !errors.Is(errs[i], loadErr) {
				t.Errorf("wave get #%d err = %v, want %v", i, errs[i], loadErr)
			}
		}
	}

	wave()
	if bc := coord.BackendCalls(); bc != 1 {
		t.Fatalf("after first wave BackendCalls() = %d, want 1", bc)
	}

	// The in-flight window has cleared and nothing was cached (the load errored),
	// so a fresh wave must trigger exactly one additional load.
	release = make(chan struct{})
	wave()
	if bc := coord.BackendCalls(); bc != 2 {
		t.Fatalf("after second wave BackendCalls() = %d, want 2", bc)
	}
}
```

## Review

The coordinator is correct when concurrent demand for one cold key collapses into
a single backend load whose result reaches every caller. The invariant that
guarantees it: one goroutine owns `cache` and `inflight`, so the "is a load
already running?" check and the "start one load" action are one indivisible step
of the select loop -- no two callers can both see the key absent and both start a
load. `TestColdKeyCoalescesToOneBackendCall` proves it with 64 racing callers and
`BackendCalls()==1`; `TestFreshWaveAfterWindowClears` proves the coalescing window
is scoped to one in-flight load, not the process lifetime, by forcing a second
load once the first cleared. The capacity-one reply channel is what makes
cancellation safe: `TestCancelledCallerDoesNotBlockWaiters` shows the broadcast
completing for the survivors even though one waiter walked away, with goleak
confirming no goroutine or channel was stranded. The production bug this prevents
is the cache stampede: without coalescing, a single popular key expiring sends
every in-flight request straight to the backend at once, and the backend -- a
database, an upstream API, a rendering service -- falls over exactly when traffic
is highest.

## Resources

- [`golang.org/x/sync/singleflight`](https://pkg.go.dev/golang.org/x/sync/singleflight) -- the standard library-adjacent implementation of this exact coalescing pattern; compare its `Do` to this actor.
- [`context.WithCancelCause` and `context.Cause`](https://pkg.go.dev/context#WithCancelCause) -- how the cancelled caller surfaces exactly why it abandoned its Get.
- [Go Memory Model](https://go.dev/ref/mem) -- why single-goroutine ownership of the maps makes the coalescing check race-free without a lock.
- [`sync.WaitGroup.Go`](https://pkg.go.dev/sync#WaitGroup.Go) -- the Go 1.25 helper used to spawn and join each load goroutine cleanly.

---

Back to [00-concepts.md](00-concepts.md) | Prev: [11-control-plane-priority-actor.md](11-control-plane-priority-actor.md) | Next: [13-correlation-id-response-demux.md](13-correlation-id-response-demux.md)
