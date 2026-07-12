# Exercise 1: A Worker That Stops When Its Done Channel Closes

The foundational shape of the whole pattern: a worker loop that folds a stream of
work into a running total, finishes cleanly when the work channel closes, and abandons
its work the instant a `done` channel is closed. This is the on-the-job skeleton of any
background consumer — a queue drainer, a log shipper, a batch aggregator — that must be
cancellable mid-flight.

## What you'll build

```text
cancellableworker/                 independent module: example.com/cancellableworker
  go.mod
  worker.go                        type Worker; Run(done, work) (int, error); Result(); ErrStopped
  cmd/
    demo/
      main.go                      runnable demo: drain some work, then cancel mid-stream
  worker_test.go                   full-drain, cancel, and partial-cancel contract; -race on Result
```

Files: `worker.go`, `cmd/demo/main.go`, `worker_test.go`.
Implement: `Worker.Run(done <-chan struct{}, work <-chan int) (int, error)` accumulating a running sum, returning `(sum, nil)` on work-close and `(partial, ErrStopped)` on cancel; `Result()` reading the running sum under a mutex.
Test: full drain sums correctly; `close(done)` returns `ErrStopped` via `errors.Is`; a partial-cancel run returns the partial sum and `ErrStopped`.
Verify: `go test -count=1 -race ./...`

Set up the module:

```bash
mkdir -p go-solutions/13-goroutines-and-channels/07-done-channel-pattern/01-cancellable-worker/cmd/demo
cd go-solutions/13-goroutines-and-channels/07-done-channel-pattern/01-cancellable-worker
```

### The two-signal loop

`Run` is a `for` loop wrapped around a two-case `select`. One case receives from `done`;
the other receives from `work` with the comma-ok form. That pairing is the crux: a
worker must handle *both* the way it ends normally (the producer closed `work`, so
there is no more to do) and the way it is cancelled (someone closed `done`). Handle only
one and the worker is broken — either it never stops early, or it never finishes a
finite job.

```go
for {
	select {
	case <-done:
		return sum, ErrStopped
	case v, ok := <-work:
		if !ok {
			return sum, nil
		}
		sum += v
	}
}
```

`ErrStopped` is a package-level sentinel created with `errors.New`, so callers can test
for it with `errors.Is` rather than comparing error strings. Returning the *partial*
sum alongside `ErrStopped` is a deliberate contract: cancellation does not discard the
work already done, it reports how far the worker got. That matters in production — a
cancelled aggregation often still wants to flush or log its partial result.

### Why Result needs a mutex

`Run` updates a running total that an observer on another goroutine may want to read
mid-flight (a progress meter, a metrics scrape). Reading and writing `w.result` from two
goroutines without synchronization is a data race, and `go test -race` will flag it. The
worker writes `w.result` under `w.mu` on every step, and `Result()` reads it under the
same lock, so an observer always sees a consistent, fully-written value. The running
`sum` local is what the loop accumulates; `w.result` is the published snapshot of it.

Note the ownership boundary: `Run` takes `done <-chan struct{}` — receive-only — so the
worker can only *observe* cancellation. The caller owns the bidirectional channel and is
the sole closer. That is what the receive-only type buys you: the worker physically
cannot close a channel it does not own.

Create `worker.go`:

```go
package cancellableworker

import (
	"errors"
	"sync"
)

// ErrStopped is returned by Run when the done channel is closed before the
// work channel is drained.
var ErrStopped = errors.New("worker stopped")

// Worker folds a stream of ints into a running sum and publishes it for a
// concurrent observer.
type Worker struct {
	mu     sync.Mutex
	result int
}

// New returns a ready Worker.
func New() *Worker {
	return &Worker{}
}

// Run accumulates values from work until work is closed, returning (sum, nil).
// If done is closed first, it returns the partial sum and ErrStopped. done is
// receive-only: Run observes cancellation but never triggers it.
func (w *Worker) Run(done <-chan struct{}, work <-chan int) (int, error) {
	var sum int
	for {
		select {
		case <-done:
			return sum, ErrStopped
		case v, ok := <-work:
			if !ok {
				return sum, nil
			}
			sum += v
			w.mu.Lock()
			w.result = sum
			w.mu.Unlock()
		}
	}
}

// Result returns the latest published running sum. It is safe to call from
// another goroutine while Run is executing.
func (w *Worker) Result() int {
	w.mu.Lock()
	defer w.mu.Unlock()
	return w.result
}
```

### The runnable demo

The demo drives the worker on the main goroutine's behalf from a separate goroutine so
it can cancel mid-stream: it feeds three values, closes `done`, and prints the partial
sum and the sentinel error.

Create `cmd/demo/main.go`:

```go
package main

import (
	"fmt"

	"example.com/cancellableworker"
)

func main() {
	done := make(chan struct{})
	work := make(chan int)

	w := cancellableworker.New()
	result := make(chan struct{})
	var sum int
	var err error
	go func() {
		sum, err = w.Run(done, work)
		close(result)
	}()

	work <- 10
	work <- 20
	close(done)
	<-result

	fmt.Printf("partial sum: %d\n", sum)
	fmt.Printf("stopped: %v\n", err)
}
```

Run it:

```bash
go run ./cmd/demo
```

Expected output:

```
partial sum: 30
stopped: worker stopped
```

### Tests

`TestRunProcessesAllWork` proves the normal path: a buffered work channel is filled and
closed, and `Run` returns the full sum with a nil error. `TestRunStopsOnDone` proves
cancellation with an idiomatic `close(done)` (not a send). `TestRunStopsAfterPartialWork`
pins the partial-cancel contract: it feeds two values, waits until they are both
reflected in `Result()`, then cancels and asserts the returned sum is the partial total
and the error is `ErrStopped`. The `Example` documents the full-drain result.

Create `worker_test.go`:

```go
package cancellableworker

import (
	"errors"
	"fmt"
	"runtime"
	"testing"
	"time"
)

func TestRunProcessesAllWork(t *testing.T) {
	t.Parallel()

	done := make(chan struct{})
	defer close(done)
	work := make(chan int, 3)
	work <- 1
	work <- 2
	work <- 3
	close(work)

	w := New()
	sum, err := w.Run(done, work)
	if err != nil {
		t.Fatalf("Run returned error: %v", err)
	}
	if sum != 6 {
		t.Fatalf("sum = %d, want 6", sum)
	}
}

func TestRunStopsOnDone(t *testing.T) {
	t.Parallel()

	done := make(chan struct{})
	work := make(chan int) // never sent on

	w := New()
	close(done)

	_, err := w.Run(done, work)
	if !errors.Is(err, ErrStopped) {
		t.Fatalf("err = %v, want ErrStopped", err)
	}
}

func TestRunStopsAfterPartialWork(t *testing.T) {
	t.Parallel()

	done := make(chan struct{})
	work := make(chan int) // unbuffered; never closed, so only done ends Run

	w := New()
	type outcome struct {
		sum int
		err error
	}
	res := make(chan outcome, 1)
	go func() {
		sum, err := w.Run(done, work)
		res <- outcome{sum, err}
	}()

	work <- 2
	work <- 3
	// A completed unbuffered send guarantees receipt, not that the fold has been
	// published yet. Spin until Run has folded both values (published sum == 5)
	// before cancelling, so the partial sum is deterministic.
	deadline := time.Now().Add(2 * time.Second)
	for w.Result() != 5 {
		if time.Now().After(deadline) {
			t.Fatal("timed out waiting for Run to fold both values")
		}
		runtime.Gosched()
	}
	close(done)

	got := <-res
	if !errors.Is(got.err, ErrStopped) {
		t.Fatalf("err = %v, want ErrStopped", got.err)
	}
	if got.sum != 5 {
		t.Fatalf("partial sum = %d, want 5", got.sum)
	}
}

func ExampleWorker_Run() {
	done := make(chan struct{})
	defer close(done)
	work := make(chan int, 2)
	work <- 4
	work <- 5
	close(work)

	w := New()
	sum, err := w.Run(done, work)
	fmt.Println(sum, err)
	// Output: 9 <nil>
}
```

## Review

The worker is correct when both exit paths are honored: draining a closed work channel
returns the full sum with no error, and closing `done` returns the sum accumulated so
far with `ErrStopped`. The partial-cancel test is the load-bearing one — it proves the
worker neither loses the work it already did nor keeps processing past cancellation. The
unbuffered work channel is what makes the partial test deterministic: an unbuffered send
does not complete until `Run` has received the value, so after `work <- 3` returns, both
values are guaranteed folded and published, and `Result()` reading 5 confirms it before
`done` is closed. Run `go test -race` to prove `Run` writing `w.result` and `Result()`
reading it never race. A common early mistake is to send on `done` instead of closing it;
for a single worker a send happens to work, but it does not scale to fan-out and it is
not the idiom — close.

## Resources

- [Go Language Spec: Select statements](https://go.dev/ref/spec#Select_statements)
- [Effective Go: Channels](https://go.dev/doc/effective_go#channels)
- [pkg.go.dev: errors.Is and sentinel errors](https://pkg.go.dev/errors#Is)

---

Back to [00-concepts.md](00-concepts.md) | Next: [02-spawn-result-channel.md](02-spawn-result-channel.md)
