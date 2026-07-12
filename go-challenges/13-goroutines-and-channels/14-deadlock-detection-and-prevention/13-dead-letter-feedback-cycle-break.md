# Exercise 13: Breaking the Dead-Letter Feedback Cycle in a Retrying Consumer

**Level: Advanced**

A queue consumer processes messages across a worker pool and re-enqueues failures for a bounded number of retries before dead-lettering them. The naive design has each worker send its failures back onto the same bounded channel it reads from, which puts a cycle in the dataflow graph: under a burst of failures the channel fills, every worker blocks on the re-enqueue send, and since the workers are the only drainers they all wait for capacity only they could free. That is a textbook circular wait on a bounded buffer, and the runtime never reports it because the HTTP server and the rest of the process keep running. This exercise builds the version that cannot wedge: a dedicated dispatcher is the sole owner of the work channel, workers only report outcomes on a separate channel, and the acyclic graph drains the burst instead of deadlocking on it.

This module is self-contained: its own module, a `retrybus` package, a demo, and tests.
Nothing here imports another exercise.

## What you'll build

```text
retrybus/                    independent module: example.com/retrybus
  go.mod                     go 1.26
  retrybus.go                Bus.Run: dispatcher owns the work channel, workers only report outcomes
  cmd/demo/main.go           runnable demo: a first-attempt failure burst drains; a poison key dead-letters
  retrybus_test.go           retries-then-delivers, always-fails dead-letter, burst exactly-once, cancel no-leak
```

- Files: `retrybus.go`, `cmd/demo/main.go`, `retrybus_test.go`.
- Implement: `New(workers, maxAttempts int, process func(Item) error) *Bus` and `func (b *Bus) Run(ctx context.Context, items []Item) []Result`, with `Item{Key string; Attempts int}` and `Result{Item Item; Delivered, DeadLettered bool; Err error}`.
- Test: every item is Delivered or DeadLettered exactly once; an always-failing key dead-letters after exactly `maxAttempts` calls; a first-attempt failure burst drains under a watchdog; cancellation returns promptly and leaks no goroutine.
- Verify: `go test -count=1 -race ./...`

Set up the module:

```bash
go mod edit -go=1.26
```

The package is stdlib-only, so no `go mod tidy` for dependencies is needed.

### Why the naive cycle deadlocks, and how ownership breaks it

The wedge is entirely about the shape of the dataflow graph, not about how many retries you allow. In the naive design there is one bounded channel `work`, and every worker both receives jobs from it and, on failure, sends the retry back to it:

```
worker --recv--> work --send--> worker      (a self-loop: a cycle)
```

Map that onto the Coffman conditions. The channel's buffer slots are the held resource (mutual exclusion). A worker that has produced a retry it cannot hand off is holding its goroutine while waiting for a free slot (hold-and-wait). A channel slot is not preemptible. And under a burst the workers form a circular wait: worker A blocks sending a retry, which needs a slot that only a receiving worker can free, but every worker is blocked on the same send, so no one receives. All four conditions hold at once, and the pool freezes with the health check still green.

The fix attacks circular wait by removing the cycle from the graph. Split the single channel in two and assign a single owner to each direction:

1. A dedicated **dispatcher** goroutine is the *sole sender* on `work`. Workers only receive from it.
2. Workers send every outcome (delivered or failed) on a separate `outcomes` channel. The dispatcher is the *sole receiver* there.
3. The dispatcher decides what to do with a failure: if the item still has attempts left it goes back into an in-memory `pending` queue to be re-dispatched later; if it has hit `maxAttempts` it becomes a `DeadLettered` result.

Now the graph is `dispatcher -> work -> worker -> outcomes -> dispatcher`, a line with a feedback edge that runs entirely through the dispatcher's own memory, not through a channel a worker consumes. A worker never sends to the stage it reads, so a failure burst cannot fill a buffer against its own drainers.

The one remaining trap is inside the dispatcher: if it ever blocks *only* on sending to `work` while workers are blocked sending to `outcomes`, you have rebuilt the cycle one level up. The dispatcher avoids that with a single `select` whose receive-from-`outcomes` case is *always* enabled, and whose send-to-`work` case is enabled only when there is something to dispatch (a nil send channel is never selected). Because the dispatcher can always take an outcome, it can never wedge waiting to hand out work. Termination is exact: track `inflight`, the number of items handed to workers but not yet reported, and stop when both `pending` and `inflight` reach zero. Every blocking channel operation carries a `ctx.Done()` branch, so cancellation unwinds the whole pipeline promptly.

Create `retrybus.go`:

```go
// Package retrybus processes queue messages across a worker pool and retries
// failures with a bounded attempt cap before dead-lettering them, without ever
// letting a worker produce to a stage it also consumes.
//
// The design breaks the circular wait that wedges the naive retrying consumer.
// A single dispatcher goroutine is the sole owner of the work channel: it is the
// only sender. Workers only receive work and report outcomes on a separate
// channel. Because a worker never sends to the channel it reads, a burst of
// failures cannot fill the work buffer against its own drainers, so the
// dataflow graph is acyclic and cannot deadlock on a bounded buffer.
package retrybus

import (
	"context"
	"slices"
	"sync"
)

// Item is a unit of work. Attempts is the number of processing attempts already
// consumed: it is 0 on input and is incremented once per call to the process
// function.
type Item struct {
	Key      string
	Attempts int
}

// Result is the terminal outcome for one input item: exactly one of Delivered or
// DeadLettered is true. Err carries the last processing error on the
// dead-lettered path.
type Result struct {
	Item         Item
	Delivered    bool
	DeadLettered bool
	Err          error
}

// Bus is a retrying consumer. Its work channel is owned by one dispatcher
// goroutine; workers never send to it.
type Bus struct {
	workers     int
	maxAttempts int
	process     func(Item) error
}

// New builds a Bus with the given worker count, per-item attempt cap, and
// processing function. workers and maxAttempts are clamped to at least 1.
func New(workers, maxAttempts int, process func(Item) error) *Bus {
	if workers < 1 {
		workers = 1
	}
	if maxAttempts < 1 {
		maxAttempts = 1
	}
	return &Bus{workers: workers, maxAttempts: maxAttempts, process: process}
}

// Run processes every item and returns one Result per input item, each either
// Delivered or DeadLettered. A dedicated dispatcher is the sole sender on the
// work channel; workers only receive work and report on the outcomes channel, so
// the dataflow graph has no cycle and a failure burst drains instead of wedging.
//
// The dispatcher's select can always receive an outcome, so it never blocks
// solely trying to hand out work while workers block reporting outcomes: that is
// the property that breaks the circular wait. In-flight accounting (items handed
// to workers but not yet reported) plus the pending queue give a precise
// termination condition. Every blocking channel operation has a ctx.Done branch,
// so cancellation returns promptly and leaks no goroutine.
func (b *Bus) Run(ctx context.Context, items []Item) []Result {
	results := make([]Result, 0, len(items))
	if len(items) == 0 {
		return results
	}

	work := make(chan Item)       // dispatcher -> workers; dispatcher is the sole sender
	outcomes := make(chan Result) // workers -> dispatcher; workers are the sole senders

	var wg sync.WaitGroup
	for range b.workers {
		wg.Go(func() {
			for it := range work {
				err := b.process(it)
				r := Result{
					Item:      Item{Key: it.Key, Attempts: it.Attempts + 1},
					Delivered: err == nil,
					Err:       err,
				}
				select {
				case outcomes <- r:
				case <-ctx.Done():
					return
				}
			}
		})
	}

	pending := slices.Clone(items) // items waiting to be dispatched
	inflight := 0                  // items handed to workers, not yet reported

dispatch:
	for len(pending) > 0 || inflight > 0 {
		// A nil send channel is never selected, so the send case is enabled
		// only when there is an item to dispatch. The receive case is always
		// enabled, which is why the dispatcher can never wedge on a send.
		var sendCh chan<- Item
		var next Item
		if len(pending) > 0 {
			sendCh = work
			next = pending[0]
		}
		select {
		case sendCh <- next:
			pending = pending[1:]
			inflight++
		case r := <-outcomes:
			inflight--
			switch {
			case r.Delivered:
				results = append(results, r)
			case r.Item.Attempts >= b.maxAttempts:
				r.DeadLettered = true
				results = append(results, r)
			default:
				pending = append(pending, r.Item) // retry: Attempts already incremented
			}
		case <-ctx.Done():
			break dispatch
		}
	}

	close(work) // sole owner closes exactly once; ranging workers then exit
	wg.Wait()
	return results
}
```

### The runnable demo

The demo submits a batch where every message fails its first attempt, plus one `poison` key that never succeeds. The first-attempt failures are exactly the burst that wedges a same-channel design; here they drain, and the poison key dead-letters after `maxAttempts`. Output is sorted by key so it is deterministic.

Create `cmd/demo/main.go`:

```go
package main

import (
	"context"
	"errors"
	"fmt"
	"slices"
	"strings"

	"example.com/retrybus"
)

func main() {
	// A burst where every message fails its first attempt: this is exactly the
	// pattern that wedges a design where workers re-enqueue onto their own input
	// channel. Here the dispatcher owns the work channel, so it drains.
	items := make([]retrybus.Item, 0, 13)
	for i := range 12 {
		items = append(items, retrybus.Item{Key: fmt.Sprintf("msg-%02d", i)})
	}
	items = append(items, retrybus.Item{Key: "poison"}) // never succeeds

	process := func(it retrybus.Item) error {
		if it.Key == "poison" {
			return errors.New("permanent failure")
		}
		if it.Attempts == 0 {
			return errors.New("transient failure")
		}
		return nil
	}

	bus := retrybus.New(4, 3, process)
	results := bus.Run(context.Background(), items)

	// Deterministic report: sort by key, no map iteration.
	slices.SortFunc(results, func(a, b retrybus.Result) int {
		return strings.Compare(a.Item.Key, b.Item.Key)
	})

	delivered, deadlettered := 0, 0
	var deadErrs []error
	for _, r := range results {
		if r.Delivered {
			delivered++
			fmt.Printf("%-8s delivered      attempts=%d\n", r.Item.Key, r.Item.Attempts)
		} else {
			deadlettered++
			deadErrs = append(deadErrs, fmt.Errorf("%s: %w", r.Item.Key, r.Err))
			fmt.Printf("%-8s dead-lettered  attempts=%d\n", r.Item.Key, r.Item.Attempts)
		}
	}

	fmt.Printf("total=%d delivered=%d dead-lettered=%d\n", len(results), delivered, deadlettered)
	fmt.Printf("aggregate dead-letter error: %v\n", errors.Join(deadErrs...))
}
```

Run it:

```bash
go run ./cmd/demo
```

Expected output:

```
msg-00   delivered      attempts=2
msg-01   delivered      attempts=2
msg-02   delivered      attempts=2
msg-03   delivered      attempts=2
msg-04   delivered      attempts=2
msg-05   delivered      attempts=2
msg-06   delivered      attempts=2
msg-07   delivered      attempts=2
msg-08   delivered      attempts=2
msg-09   delivered      attempts=2
msg-10   delivered      attempts=2
msg-11   delivered      attempts=2
poison   dead-lettered  attempts=3
total=13 delivered=12 dead-lettered=1
aggregate dead-letter error: poison: permanent failure
```

### Tests

Each hang-prone test runs under `guard`, a per-test watchdog that dumps every goroutine's stack on timeout, so a regression to a wedging design fails with a map to the wedge instead of a silent stall. `TestRetriesThenDelivers` fails the first K attempts per key and asserts every item is delivered with `Attempts == K+1`. `TestAlwaysFailsDeadLetters` pins the dead-letter contract: an always-failing key is dead-lettered after *exactly* `maxAttempts` calls (checked with an atomic call counter), the reported `Attempts` equals `maxAttempts`, the run terminates via correct in-flight accounting, and the result carries the last error. `TestBurstDrainsExactlyOnce` is the acyclic-invariant test: 500 items, most failing on the first attempt, must all resolve to Delivered or DeadLettered exactly once (the `resultByKey` helper fails on any duplicate or on a result that is neither or both). `TestContextCancelReturnsPromptlyNoLeak` cancels mid-run once a worker is confirmed inside `process`, asserts `Run` returns within the watchdog, and asserts no goroutine survives.

Create `retrybus_test.go`:

```go
package retrybus

import (
	"context"
	"errors"
	"fmt"
	"runtime"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

// guard runs fn and, if it does not return within d, dumps every goroutine's
// stack and fails the test. It turns a partial deadlock (which the Go runtime
// cannot detect) into an actionable stack trace instead of a silent hang.
func guard(t *testing.T, d time.Duration, name string, fn func()) {
	t.Helper()
	done := make(chan struct{})
	go func() {
		defer close(done)
		fn()
	}()
	timer := time.NewTimer(d)
	defer timer.Stop()
	select {
	case <-done:
	case <-timer.C:
		buf := make([]byte, 1<<20)
		n := runtime.Stack(buf, true)
		t.Fatalf("%s did not finish within %s: likely deadlock.\n\n%s", name, d, buf[:n])
	}
}

// resultByKey indexes results for assertion. It also fails if any key appears
// more than once, which pins the exactly-once property.
func resultByKey(t *testing.T, results []Result) map[string]Result {
	t.Helper()
	m := make(map[string]Result, len(results))
	for _, r := range results {
		if _, dup := m[r.Key()]; dup {
			t.Fatalf("key %q reported more than once", r.Key())
		}
		if r.Delivered == r.DeadLettered {
			t.Fatalf("key %q must be exactly one of Delivered/DeadLettered, got %+v", r.Key(), r)
		}
		m[r.Key()] = r
	}
	return m
}

// Key is a tiny helper local to the test for readability.
func (r Result) Key() string { return r.Item.Key }

// TestRetriesThenDelivers: a process that fails the first K attempts per key and
// then succeeds must deliver every item, with Attempts reflecting K+1 calls.
func TestRetriesThenDelivers(t *testing.T) {
	t.Parallel()

	const k = 2
	process := func(it Item) error {
		if it.Attempts < k {
			return fmt.Errorf("transient on %s attempt %d", it.Key, it.Attempts)
		}
		return nil
	}

	items := make([]Item, 20)
	for i := range items {
		items[i] = Item{Key: fmt.Sprintf("k%02d", i)}
	}

	var results []Result
	bus := New(4, 5, process)
	guard(t, 5*time.Second, "Run(retries-then-delivers)", func() {
		results = bus.Run(context.Background(), items)
	})

	if len(results) != len(items) {
		t.Fatalf("want %d results, got %d", len(items), len(results))
	}
	byKey := resultByKey(t, results)
	for _, it := range items {
		r := byKey[it.Key]
		if !r.Delivered {
			t.Fatalf("key %q not delivered: %+v", it.Key, r)
		}
		if r.Item.Attempts != k+1 {
			t.Fatalf("key %q: want %d attempts, got %d", it.Key, k+1, r.Item.Attempts)
		}
	}
}

// TestAlwaysFailsDeadLetters: a key that always fails is dead-lettered after
// exactly maxAttempts calls, the reported Attempts equals maxAttempts, and the
// run still terminates via correct in-flight accounting.
func TestAlwaysFailsDeadLetters(t *testing.T) {
	t.Parallel()

	const maxAttempts = 3
	var calls atomic.Int64
	process := func(it Item) error {
		calls.Add(1)
		return fmt.Errorf("permanent on %s", it.Key)
	}

	items := []Item{{Key: "a"}, {Key: "b"}, {Key: "c"}}

	var results []Result
	bus := New(2, maxAttempts, process)
	guard(t, 5*time.Second, "Run(always-fails)", func() {
		results = bus.Run(context.Background(), items)
	})

	if len(results) != len(items) {
		t.Fatalf("want %d results, got %d", len(items), len(results))
	}
	byKey := resultByKey(t, results)
	for _, it := range items {
		r := byKey[it.Key]
		if !r.DeadLettered {
			t.Fatalf("key %q not dead-lettered: %+v", it.Key, r)
		}
		if r.Item.Attempts != maxAttempts {
			t.Fatalf("key %q: want %d attempts, got %d", it.Key, maxAttempts, r.Item.Attempts)
		}
		if r.Err == nil {
			t.Fatalf("key %q: dead-lettered result must carry the last error", it.Key)
		}
	}
	if got, want := calls.Load(), int64(len(items)*maxAttempts); got != want {
		t.Fatalf("process called %d times, want %d", got, want)
	}
}

// TestBurstDrainsExactlyOnce is the acyclic invariant test: a large batch where
// most items fail on the first attempt is exactly the burst that wedges a design
// in which workers re-enqueue onto their own input channel. Here it must drain,
// with each item Delivered or DeadLettered exactly once.
func TestBurstDrainsExactlyOnce(t *testing.T) {
	t.Parallel()

	const (
		n           = 500
		workers     = 8
		maxAttempts = 4
	)
	// Every non-poison key fails its first attempt (the burst) then succeeds.
	// One in seven keys is poison and always fails, exercising the dead-letter
	// path under the same burst.
	poison := func(i int) bool { return i%7 == 0 }
	process := func(it Item) error {
		if strings.HasPrefix(it.Key, "poison-") {
			return errors.New("permanent")
		}
		if it.Attempts == 0 {
			return errors.New("transient")
		}
		return nil
	}

	items := make([]Item, n)
	wantDelivered := 0
	wantDead := 0
	for i := range items {
		if poison(i) {
			items[i] = Item{Key: fmt.Sprintf("poison-%03d", i)}
			wantDead++
		} else {
			items[i] = Item{Key: fmt.Sprintf("ok-%03d", i)}
			wantDelivered++
		}
	}

	var results []Result
	bus := New(workers, maxAttempts, process)
	guard(t, 10*time.Second, "Run(burst)", func() {
		results = bus.Run(context.Background(), items)
	})

	if len(results) != n {
		t.Fatalf("want %d results, got %d", n, len(results))
	}
	byKey := resultByKey(t, results) // asserts exactly-once and mutual exclusion
	if len(byKey) != n {
		t.Fatalf("want %d distinct keys, got %d", n, len(byKey))
	}
	gotDelivered, gotDead := 0, 0
	for _, r := range results {
		if r.Delivered {
			gotDelivered++
		} else {
			gotDead++
		}
	}
	if gotDelivered != wantDelivered || gotDead != wantDead {
		t.Fatalf("delivered=%d dead=%d, want delivered=%d dead=%d",
			gotDelivered, gotDead, wantDelivered, wantDead)
	}
}

// TestContextCancelReturnsPromptlyNoLeak cancels mid-run and asserts Run returns
// promptly and leaves no goroutine behind. The process honors ctx, as a real
// handler would, so blocked workers can unwind cleanly.
func TestContextCancelReturnsPromptlyNoLeak(t *testing.T) {
	// Not parallel: it counts goroutines, so it must run without other tests
	// concurrently inflating the count.
	before := runtime.NumGoroutine()

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	started := make(chan struct{})
	var once sync.Once
	process := func(it Item) error {
		once.Do(func() { close(started) })
		<-ctx.Done() // block until cancellation, like a stuck downstream call
		return ctx.Err()
	}

	items := make([]Item, 100)
	for i := range items {
		items[i] = Item{Key: fmt.Sprintf("c%03d", i)}
	}

	bus := New(4, 3, process)
	done := make(chan []Result, 1)
	go func() { done <- bus.Run(ctx, items) }()

	<-started // at least one worker is inside process
	cancel()

	select {
	case <-done:
	case <-time.After(5 * time.Second):
		buf := make([]byte, 1<<20)
		n := runtime.Stack(buf, true)
		t.Fatalf("Run did not return promptly after cancel:\n\n%s", buf[:n])
	}

	deadline := time.Now().Add(2 * time.Second)
	for runtime.NumGoroutine() > before && time.Now().Before(deadline) {
		time.Sleep(time.Millisecond)
	}
	if leaked := runtime.NumGoroutine() - before; leaked > 0 {
		t.Fatalf("Run leaked %d goroutines after cancellation", leaked)
	}
}

// TestEmptyInput is the trivial boundary: no items, no goroutines, empty result.
func TestEmptyInput(t *testing.T) {
	t.Parallel()

	bus := New(4, 3, func(Item) error { return nil })
	if got := bus.Run(context.Background(), nil); len(got) != 0 {
		t.Fatalf("want empty result, got %v", got)
	}
}
```

## Review

Correct here means one terminal `Result` per input item, each exactly one of Delivered or DeadLettered, a run that always terminates, and no goroutine left behind on cancellation. The guarantee comes from a single structural decision: the work channel has one owner, the dispatcher, and workers never send to the stage they read. That makes the dataflow graph acyclic, which removes the circular-wait condition the naive same-channel retry design depends on, so a failure burst drains rather than wedging its own drainers. The dispatcher's `select` keeps the receive-from-outcomes case always enabled and gates the send-to-work case behind a non-nil channel, so the dispatcher itself can never become the new cycle; `inflight` plus `pending` reaching zero is the exact termination signal. `TestBurstDrainsExactlyOnce` drives 500 items through the wedging burst and proves each resolves once under a watchdog; `TestAlwaysFailsDeadLetters` counts process calls to prove the attempt cap is honored exactly; `TestContextCancelReturnsPromptlyNoLeak` proves the ctx.Done branches unwind the pipeline. The production bug this prevents is the silent one: a retrying consumer that runs fine until a downstream outage makes many messages fail at once, at which point the pool freezes, latency climbs, the health check stays green, and nothing in the logs explains it.

## Resources

- [The Go Memory Model](https://go.dev/ref/mem) -- why the dispatcher-owns-the-channel discipline makes the concurrent result assembly race-free.
- [`context` package](https://pkg.go.dev/context) -- cancellation propagation and `ctx.Done`, the exit every blocking channel op needs.
- [Go by Example: Worker Pools](https://gobyexample.com/worker-pools) -- the baseline pool this exercise hardens against a feedback cycle.
- [`errors.Join`](https://pkg.go.dev/errors#Join) -- aggregating the dead-letter errors into one report, as the demo does.

---

Back to [00-concepts.md](00-concepts.md) | Prev: [12-connection-pool-acquire-no-capacity-loss.md](12-connection-pool-acquire-no-capacity-loss.md) | Next: [14-singleflight-stampede-no-wedge.md](14-singleflight-stampede-no-wedge.md)
