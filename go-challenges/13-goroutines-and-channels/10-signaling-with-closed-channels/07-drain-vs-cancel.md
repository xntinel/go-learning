# Exercise 7: Drain Versus Cancel: Two Distinct Close Signals

Two closes with the same syntax mean opposite things. Closing the *work* channel
says "no more items — finish what remains" (graceful drain). Closing a *stop*
channel says "abandon what is in flight — return now" (forced cancel). A queue
processor must handle both, and the classic bug is a cancelled consumer that then
tries to drain a producer that has already gone, deadlocking on a receive that
never completes. This exercise builds the processor that keeps the two signals
distinct and never blocks on a dead producer.

This module is fully self-contained: its own `go mod init`, all code inline, its
own demo and tests.

## What you'll build

```text
drainer/                     independent module: example.com/drainer
  go.mod                     go mod init example.com/drainer
  processor.go               type Processor; New, Process(work) (int, bool), Cancel
  cmd/
    demo/
      main.go                runnable demo: graceful drain and forced cancel
  processor_test.go          drain-sums-all, cancel-returns-partial, no-leak
```

Files: `processor.go`, `cmd/demo/main.go`, `processor_test.go`.
Implement: a `Processor` that reads from `work` until `work` is closed (drain: sum every remaining item) or a separate `stop chan struct{}` is closed (cancel: return the partial sum immediately). On cancel it must not block trying to drain.
Test: close `work` and the processor drains every enqueued item (sum matches); close `stop` mid-stream and it returns promptly with a partial result without blocking on an unbuffered `work`; assert no goroutine leak in either path via a `WaitGroup`.
Verify: `go test -count=1 -race ./...`

### The two post-conditions

The loop selects over `stop` and `work`, and the two arms have different
post-conditions:

```go
for {
	select {
	case <-p.stop:
		return sum, true // cancel: discard in-flight, return now
	case v, ok := <-work:
		if !ok {
			return sum, false // drain: producer closed work, finished
		}
		sum += v
	}
}
```

`ok == false` on the `work` arm is the *drain* signal: the producer closed the
channel, there is nothing left, and the accumulated sum is complete. A closed
`stop` is the *cancel* signal: return the partial sum immediately, regardless of
what is still queued. The boolean return distinguishes the two so the caller
knows whether it got a complete or a partial result.

The trap is thinking "cancel" means "stop reading new items but drain the rest":

```text
// Wrong: deadlocks if the producer has already exited.
case <-p.stop:
	for v := range work { // range blocks forever on an unbuffered, unclosed work
		sum += v
	}
	return sum, true
```

If `stop` is closed because the whole pipeline is being torn down, the producer
is gone and will never close `work`. Ranging over `work` blocks on a receive that
never completes — a deadlock. The rule: on cancel, return immediately; never block
on a producer that may have exited. If you genuinely need a *bounded* final drain,
drain only what is already buffered with a non-blocking `select`/`default`, never
an unbounded `range`.

Set up the module:

```bash
mkdir -p go-solutions/13-goroutines-and-channels/10-signaling-with-closed-channels/07-drain-vs-cancel/cmd/demo
cd go-solutions/13-goroutines-and-channels/10-signaling-with-closed-channels/07-drain-vs-cancel
```

Create `processor.go`:

```go
package drainer

import (
	"sync"
)

// Processor consumes ints from a work channel. It distinguishes a graceful drain
// (work closed: finish what remains) from a forced cancel (stop closed: return
// the partial sum now, without draining).
type Processor struct {
	stop chan struct{}
	once sync.Once
}

// New returns a ready Processor.
func New() *Processor {
	return &Processor{stop: make(chan struct{})}
}

// Process accumulates values from work until work is closed (drain) or Cancel is
// called (forced). It returns the sum and whether it was cancelled. On cancel it
// returns immediately and never blocks trying to drain a possibly-dead producer.
func (p *Processor) Process(work <-chan int) (sum int, cancelled bool) {
	for {
		select {
		case <-p.stop:
			return sum, true
		case v, ok := <-work:
			if !ok {
				return sum, false
			}
			sum += v
		}
	}
}

// Cancel broadcasts a forced stop. It is idempotent.
func (p *Processor) Cancel() {
	p.once.Do(func() { close(p.stop) })
}
```

### The runnable demo

Create `cmd/demo/main.go`:

```go
package main

import (
	"fmt"

	"example.com/drainer"
)

func main() {
	// Graceful drain: producer closes work, consumer sums every item.
	p := drainer.New()
	work := make(chan int, 5)
	for _, v := range []int{1, 2, 3, 4, 5} {
		work <- v
	}
	close(work)
	sum, cancelled := p.Process(work)
	fmt.Printf("drain: sum=%d cancelled=%v\n", sum, cancelled)

	// Forced cancel: no items, Cancel returns the partial sum immediately and
	// does not deadlock on the unbuffered, never-closed work channel.
	p2 := drainer.New()
	live := make(chan int)
	go p2.Cancel()
	sum2, cancelled2 := p2.Process(live)
	fmt.Printf("cancel: sum=%d cancelled=%v\n", sum2, cancelled2)
}
```

Run it:

```bash
go run ./cmd/demo
```

Expected output:

```
drain: sum=15 cancelled=false
cancel: sum=0 cancelled=true
```

### Tests

Create `processor_test.go`:

```go
package drainer

import (
	"sync"
	"testing"
	"time"
)

func TestDrainSumsEveryItem(t *testing.T) {
	t.Parallel()

	p := New()
	work := make(chan int, 4)
	work <- 1
	work <- 2
	work <- 3
	work <- 4
	close(work)

	sum, cancelled := p.Process(work)
	if cancelled {
		t.Fatal("cancelled = true on a clean drain")
	}
	if sum != 10 {
		t.Fatalf("sum = %d, want 10", sum)
	}
}

func TestCancelReturnsPromptlyNoDeadlock(t *testing.T) {
	t.Parallel()

	p := New()
	work := make(chan int) // unbuffered, no producer

	var wg sync.WaitGroup
	res := make(chan int, 1)
	wg.Add(1)
	go func() {
		defer wg.Done()
		sum, cancelled := p.Process(work)
		if !cancelled {
			t.Errorf("cancelled = false, want true")
		}
		res <- sum
	}()

	p.Cancel()
	wg.Wait() // returns only if Process did NOT deadlock draining a dead producer

	if sum := <-res; sum != 0 {
		t.Fatalf("sum = %d, want 0", sum)
	}
}

func TestCancelReturnsPartial(t *testing.T) {
	t.Parallel()

	p := New()
	work := make(chan int, 100)
	work <- 1
	work <- 2
	work <- 3
	// work is NOT closed: the producer is still "alive" but idle.

	var wg sync.WaitGroup
	res := make(chan int, 1)
	wg.Add(1)
	go func() {
		defer wg.Done()
		sum, _ := p.Process(work)
		res <- sum
	}()

	// Let Process drain the three buffered items and park on the select, then
	// cancel. The partial result reflects the items processed before cancel.
	time.Sleep(20 * time.Millisecond)
	p.Cancel()
	wg.Wait()

	if sum := <-res; sum != 6 {
		t.Fatalf("partial sum = %d, want 6", sum)
	}
}
```

## Review

The processor is correct when a closed `work` yields `(sum, false)` with every
item counted and a closed `stop` yields `(partial, true)` without blocking. The
no-leak proof is the `WaitGroup`: `TestCancelReturnsPromptlyNoDeadlock` would hang
forever if `Process` tried to drain the dead, unbuffered producer, so `wg.Wait()`
returning is the assertion that it did not. The mistake to avoid is conflating the
two closes — a cancelled consumer that ranges over `work` to "finish up" deadlocks
the instant the producer is already gone. Keep the two signals distinct: `ok ==
false` means finished, a closed `stop` means abandon. Run `go test -race`.

## Resources

- [The Go Programming Language Specification: Receive operator](https://go.dev/ref/spec#Receive_operator) — the two-value receive and what `ok == false` means on a closed channel.
- [Go Blog: Pipelines and cancellation](https://go.dev/blog/pipelines) — draining versus cancelling a pipeline stage cleanly.
- [pkg.go.dev: sync.WaitGroup](https://pkg.go.dev/sync#WaitGroup) — the leak/deadlock proof for both exit paths.

---

Back to [00-concepts.md](00-concepts.md) | Next: [08-deadline-broadcast-flush.md](08-deadline-broadcast-flush.md)
