# Exercise 9: Bounding Fan-Out to a Downstream Service with a Weighted Semaphore

A batch job needs to call a fragile downstream API once per item — thousands
of items, but the downstream tolerates at most N requests in flight before
its latency explodes. This exercise builds the bounded fan-out twice: with
the buffered-channel semaphore idiom, then with
`golang.org/x/sync/semaphore.Weighted`, which adds the two things the
channel cannot express — context-aware blocking acquisition and *weights*,
so a heavy request can occupy three slots. An atomic high-water mark proves
the bound actually held.

## What you'll build

```text
fanoutsem/                      independent module: example.com/fanoutsem
  go.mod                        requires golang.org/x/sync
  fanout/
    fanout.go                   Gauge (atomic in-flight + CAS peak),
                                FanOutChan (channel semaphore),
                                Job, FanOutWeighted (semaphore.Weighted)
    fanout_test.go              peak-bound storm, weight-exclusion proof,
                                cancellation releasing blocked acquirers
  cmd/
    demo/
      main.go                   uniform and mixed-weight batches, bound held
```

- Files: `fanout/fanout.go`, `fanout/fanout_test.go`, `cmd/demo/main.go`.
- Implement: `FanOutChan(ctx, limit, items, call, gauge)` with a `chan struct{}` semaphore and cancellable acquire; `FanOutWeighted(ctx, limit, jobs, call, gauge)` with `semaphore.Weighted` and per-job weights; a `Gauge` whose peak is maintained with `CompareAndSwap`.
- Test: 100 tasks against limit 8 never exceed peak weight 8 under `-race`; a held weight-3 acquisition excludes three weight-1 acquisitions; canceling the context releases goroutines blocked in `Acquire` with `ctx.Err()` and no leaks.
- Verify: `go test -count=1 -race ./...` and `go run ./cmd/demo`.

Set up the module:

```bash
go get golang.org/x/sync/semaphore
```

### Admission for work, not requests: the semaphore shape

Rate limiters (Exercises 1-8) answer "how many per second?"; a fan-out bound
answers "how many *at once*?" — a concurrency cap, not a rate. The channel
idiom: acquire by sending into a `make(chan struct{}, limit)` before
spawning the worker, release by receiving when the worker finishes. Crucially
the acquire is a `select` against `ctx.Done()`, because a bare blocking send
in a cancellable pipeline is exactly the "forgetting ctx.Done in blocking
channel acquires" trap — when the downstream stalls and the batch deadline
fires, the dispatcher must stop launching, not queue forever.

Two placement decisions matter more than they look. Acquire happens in the
*dispatcher* loop, not inside the spawned goroutine — otherwise you
instantly create one goroutine per item (defeating half the purpose of
bounding) and the semaphore only bounds the downstream calls, not the
memory. Release lives in a `defer` inside the worker, so a panicking or
early-returning call still frees its slot; a leaked slot is a permanent
capacity reduction that no test notices until the fourth incident review.

### What the channel cannot say: weights

Now the requirement changes: most items are cheap `report` calls, but some
are `bulk-export` calls that hit the downstream three times as hard. The
policy you want is "total in-flight *cost* at most 6", where a bulk export
costs 3 and a report costs 1. A buffered channel cannot express this — you
could send three tokens for a heavy job, but a partial acquisition of two
tokens by one job and one by another deadlocks both (each holds capacity
the other needs, neither can complete its acquire, neither releases —
the classic multi-resource deadlock from the lock-ordering lesson, rebuilt
out of channel slots).

`semaphore.NewWeighted(n)` solves precisely this: `Acquire(ctx, k)` blocks
until k units are available *atomically*, `TryAcquire(k)` is the
non-blocking form, `Release(k)` returns them, and a canceled context aborts
a blocked `Acquire` with `ctx.Err()` without leaking the waiter. Internally
it is — fittingly for this lesson — a mutex plus a FIFO list of waiting
channels: the library composed both primitives, using the mutex for the
guarded count and channels for the wait-queue signaling. Its FIFO policy
also means a big waiter is not starved by a stream of small ones; the small
acquisitions queue behind it (worth knowing: it trades some throughput for
that fairness).

### Proving the bound: an atomic high-water mark

"The limit was never exceeded" needs evidence, not faith. Each worker bumps
an atomic in-flight counter on entry and records the peak with a
`CompareAndSwap` loop: read the current peak, and if our in-flight value is
higher, attempt to install it, retrying if another goroutine moved the peak
first. The CAS loop is the standard lock-free max-update — a plain
`if cur > peak { peak = cur }` on an atomic would be a lost-update race
between the compare and the store. Because the gauge is bumped *inside* the
semaphore-protected region, peak can under-report momentarily but can never
exceed the true concurrency, so `peak <= limit` is a sound assertion and
`-race` guards the measurement itself.

Create `fanout/fanout.go`:

```go
// Package fanout bounds concurrent calls to a downstream service, first with
// the buffered-channel semaphore idiom, then with a weighted semaphore.
package fanout

import (
	"context"
	"errors"
	"sync"
	"sync/atomic"

	"golang.org/x/sync/semaphore"
)

// CallFunc performs one downstream request for one item.
type CallFunc func(ctx context.Context, item string) error

// Gauge records in-flight weight and its all-time peak, race-free.
type Gauge struct {
	inflight atomic.Int64
	peak     atomic.Int64
}

// enter adds w units of in-flight work and updates the peak via a CAS loop:
// the lock-free way to compute a running maximum without losing updates.
func (g *Gauge) enter(w int64) {
	cur := g.inflight.Add(w)
	for {
		p := g.peak.Load()
		if cur <= p || g.peak.CompareAndSwap(p, cur) {
			return
		}
	}
}

func (g *Gauge) exit(w int64) { g.inflight.Add(-w) }

// Peak reports the maximum in-flight weight observed.
func (g *Gauge) Peak() int64 { return g.peak.Load() }

// FanOutChan calls call for every item with at most limit calls in flight,
// using the buffered-channel semaphore idiom. The acquire selects on
// ctx.Done so a canceled batch stops launching instead of queueing forever.
func FanOutChan(ctx context.Context, limit int, items []string, call CallFunc, g *Gauge) error {
	sem := make(chan struct{}, limit)
	var (
		wg   sync.WaitGroup
		mu   sync.Mutex
		errs []error
	)
	record := func(err error) {
		mu.Lock()
		errs = append(errs, err)
		mu.Unlock()
	}

	for _, item := range items {
		select {
		case sem <- struct{}{}: // acquire one slot
		case <-ctx.Done():
			record(ctx.Err())
			wg.Wait() // in-flight workers drain before we report
			return errors.Join(errs...)
		}
		wg.Go(func() {
			defer func() { <-sem }() // release even on panic or early return
			g.enter(1)
			defer g.exit(1)
			if err := call(ctx, item); err != nil {
				record(err)
			}
		})
	}
	wg.Wait()
	return errors.Join(errs...)
}

// Job is one downstream call with a capacity cost. A bulk export that hits
// the downstream three times as hard carries Weight 3.
type Job struct {
	Item   string
	Weight int64
}

// FanOutWeighted calls call for every job, keeping total in-flight weight
// at or below limit via a weighted semaphore. Acquire is context-aware:
// a canceled batch aborts blocked acquisitions with ctx.Err().
func FanOutWeighted(ctx context.Context, limit int64, jobs []Job, call CallFunc, g *Gauge) error {
	sem := semaphore.NewWeighted(limit)
	var (
		wg   sync.WaitGroup
		mu   sync.Mutex
		errs []error
	)
	record := func(err error) {
		mu.Lock()
		errs = append(errs, err)
		mu.Unlock()
	}

	for _, job := range jobs {
		if err := sem.Acquire(ctx, job.Weight); err != nil {
			record(err) // ctx canceled or expired while waiting for capacity
			break
		}
		wg.Go(func() {
			defer sem.Release(job.Weight)
			g.enter(job.Weight)
			defer g.exit(job.Weight)
			if err := call(ctx, job.Item); err != nil {
				record(err)
			}
		})
	}
	wg.Wait()
	return errors.Join(errs...)
}
```

Error aggregation uses `errors.Join`, so callers can still match individual
failures with `errors.Is` — including `context.Canceled` from an aborted
acquire — and joining an empty slice yields `nil` for the happy path.

### The demo: uniform batch, then mixed weights

Create `cmd/demo/main.go`:

```go
package main

import (
	"context"
	"fmt"
	"time"

	"example.com/fanoutsem/fanout"
)

func main() {
	ctx := context.Background()
	call := func(ctx context.Context, item string) error {
		select {
		case <-time.After(10 * time.Millisecond): // fake downstream latency
			return nil
		case <-ctx.Done():
			return ctx.Err()
		}
	}

	items := make([]string, 20)
	for i := range items {
		items[i] = fmt.Sprintf("order-%02d", i)
	}
	var g fanout.Gauge
	err := fanout.FanOutChan(ctx, 4, items, call, &g)
	fmt.Printf("channel fan-out : items=20 limit=4 bound held=%v err=%v\n",
		g.Peak() <= 4, err)

	jobs := []fanout.Job{
		{Item: "bulk-export", Weight: 3},
		{Item: "report", Weight: 1},
		{Item: "report", Weight: 1},
		{Item: "report", Weight: 1},
		{Item: "bulk-export", Weight: 3},
		{Item: "report", Weight: 1},
	}
	var gw fanout.Gauge
	err = fanout.FanOutWeighted(ctx, 4, jobs, call, &gw)
	fmt.Printf("weighted fan-out: jobs=6 limit=4 bound held=%v err=%v\n",
		gw.Peak() <= 4, err)
}
```

Run it:

```bash
go run ./cmd/demo
```

Expected output:

```
channel fan-out : items=20 limit=4 bound held=true err=<nil>
weighted fan-out: jobs=6 limit=4 bound held=true err=<nil>
```

### Tests

The storm launches 100 tasks against limit 8 with real (small) fake-latency
and asserts the peak never exceeded 8 while every item was processed — the
processed count is itself atomic, since the test must not race on its own
bookkeeping. The exclusion test pins the weighted semantics at the
`TryAcquire` level: while 3 of 3 units are held, not even a weight-1
acquisition fits. The cancellation test is the shutdown story: with limit 1
and a call that only returns on cancellation, the dispatcher is blocked in
`Acquire` when the cancel lands; both the blocked acquire and the in-flight
call must surface `context.Canceled`, and `FanOutWeighted` returning at all
proves no goroutine was left behind (its final `wg.Wait` cannot pass
otherwise).

Create `fanout/fanout_test.go`:

```go
package fanout

import (
	"context"
	"errors"
	"fmt"
	"sync/atomic"
	"testing"
	"time"

	"golang.org/x/sync/semaphore"
)

func TestChannelFanOutHoldsBound(t *testing.T) {
	t.Parallel()

	var processed atomic.Int64
	call := func(ctx context.Context, item string) error {
		select {
		case <-time.After(2 * time.Millisecond):
			processed.Add(1)
			return nil
		case <-ctx.Done():
			return ctx.Err()
		}
	}

	items := make([]string, 100)
	for i := range items {
		items[i] = fmt.Sprintf("item-%d", i)
	}

	var g Gauge
	if err := FanOutChan(t.Context(), 8, items, call, &g); err != nil {
		t.Fatalf("FanOutChan = %v, want nil", err)
	}
	if got := processed.Load(); got != 100 {
		t.Fatalf("processed = %d, want 100", got)
	}
	if peak := g.Peak(); peak > 8 {
		t.Fatalf("peak concurrency = %d, exceeded the limit of 8", peak)
	}
}

func TestWeightedFanOutHoldsWeightBound(t *testing.T) {
	t.Parallel()

	call := func(ctx context.Context, item string) error {
		select {
		case <-time.After(2 * time.Millisecond):
			return nil
		case <-ctx.Done():
			return ctx.Err()
		}
	}

	jobs := make([]Job, 0, 40)
	for i := range 40 {
		w := int64(1)
		if i%4 == 0 {
			w = 3 // every fourth job is heavy
		}
		jobs = append(jobs, Job{Item: fmt.Sprintf("job-%d", i), Weight: w})
	}

	var g Gauge
	if err := FanOutWeighted(t.Context(), 5, jobs, call, &g); err != nil {
		t.Fatalf("FanOutWeighted = %v, want nil", err)
	}
	if peak := g.Peak(); peak > 5 {
		t.Fatalf("peak in-flight weight = %d, exceeded the limit of 5", peak)
	}
}

func TestWeightThreeExcludesWeightOnes(t *testing.T) {
	t.Parallel()

	sem := semaphore.NewWeighted(3)
	if !sem.TryAcquire(3) {
		t.Fatal("TryAcquire(3) failed on a fresh semaphore of size 3")
	}
	// The heavy holder occupies ALL capacity: not even one unit fits.
	if sem.TryAcquire(1) {
		t.Fatal("TryAcquire(1) succeeded while a weight-3 acquisition was held")
	}
	sem.Release(3)
	for i := range 3 {
		if !sem.TryAcquire(1) {
			t.Fatalf("TryAcquire(1) #%d failed after the weight-3 release", i)
		}
	}
}

func TestCancelReleasesBlockedAcquirers(t *testing.T) {
	t.Parallel()

	ctx, cancel := context.WithCancel(t.Context())
	time.AfterFunc(30*time.Millisecond, cancel)

	// The call only finishes on cancellation, so with limit 1 the
	// dispatcher is stuck inside sem.Acquire when the cancel arrives.
	call := func(ctx context.Context, item string) error {
		<-ctx.Done()
		return ctx.Err()
	}
	jobs := []Job{{Item: "a", Weight: 1}, {Item: "b", Weight: 1}, {Item: "c", Weight: 1}}

	var g Gauge
	err := FanOutWeighted(ctx, 1, jobs, call, &g)
	if !errors.Is(err, context.Canceled) {
		t.Fatalf("FanOutWeighted = %v, want context.Canceled in the join", err)
	}
	// Returning at all proves wg.Wait drained every worker: no leaks.
}

func ExampleFanOutChan() {
	var g Gauge
	err := FanOutChan(context.Background(), 2, []string{"a", "b", "c"},
		func(ctx context.Context, item string) error { return nil }, &g)
	fmt.Println(err, g.Peak() <= 2)
	// Output: <nil> true
}
```

## Review

Check your instincts against the three traps this artifact sets. Acquiring
inside the worker goroutine instead of the dispatcher loop bounds the calls
but not the goroutines — 100k items become 100k goroutines instantly, and
the storm test's memory profile (not its assertions) is where you would
notice. Releasing without `defer` leaks a slot on any early return, shrinking
capacity permanently and invisibly. And building weighted acquisition out of
multiple single-token channel sends deadlocks under contention, because two
jobs can each hold part of what the other needs — the atomic multi-unit
`Acquire` is not a convenience, it is the correctness fix.

Also note what the cancellation test quietly proves: `semaphore.Weighted`
aborts *blocked waiters* on context cancellation, which a bare channel send
cannot do without the extra `select` — and once weights exist, even the
`select` idiom stops composing. Verify with `go test -count=1 -race ./...`;
the race detector covers the CAS gauge, the error-slice mutex, and the
dispatcher/worker handoff in one run.

## Resources

- [golang.org/x/sync/semaphore](https://pkg.go.dev/golang.org/x/sync/semaphore) — `NewWeighted`, `Acquire`, `TryAcquire`, `Release`, and the cancellation contract.
- [sync/atomic: CompareAndSwap](https://pkg.go.dev/sync/atomic#Int64.CompareAndSwap) — the primitive behind the lock-free peak.
- [errors.Join](https://pkg.go.dev/errors#Join) — aggregated errors that still work with `errors.Is`.
- [Go Concurrency Patterns: Pipelines and cancellation](https://go.dev/blog/pipelines) — the wider pattern family bounded fan-out belongs to.

---

Prev: [08-x-time-rate-migration.md](08-x-time-rate-migration.md) | Back to [00-concepts.md](00-concepts.md) | Next: [../11-lock-free-data-structures/00-concepts.md](../11-lock-free-data-structures/00-concepts.md)
