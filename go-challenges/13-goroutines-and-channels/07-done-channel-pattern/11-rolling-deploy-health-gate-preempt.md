# Exercise 11: Rolling Deploy Health Gate With Strict Cancellation Preempt

**Level: Intermediate**

A rolling-deploy controller promotes instances to a new version one batch at a time, pulling the next batch from a work channel. When a rollback signal fires — an abort done channel closes — the controller must promote ZERO further batches, even if the queue still holds ready items. The naive fix, a `select` racing abort against work, is wrong: when both are ready it picks uniformly at random, so an already-cancelled deploy can still promote one more batch and widen the blast radius. This exercise builds the controller that makes abort strictly win.

This module is self-contained: its own module, a `promoter` package, a demo, and tests.
Nothing here imports another exercise.

## What you'll build

```text
promoter/                    independent module: example.com/promoter
  go.mod                     go 1.26
  promoter.go                Batch, Promoter, New, Run with strict abort preemption
  cmd/demo/main.go           runnable demo: pre-aborted, clean, and mid-deploy-gated rollouts
  promoter_test.go           zero-on-pre-abort, in-order clean rollout, gated-at-K, empty queue
```

- Files: `promoter.go`, `cmd/demo/main.go`, `promoter_test.go`.
- Implement: `type Batch struct { ID, Instances int }`; `type Promoter`; `New(promote func(Batch)) *Promoter`; `(*Promoter).Run(abort <-chan struct{}, batches <-chan Batch) int`.
- Test: a pre-closed abort with a full queue promotes exactly zero; a never-closed abort with a closed queue promotes every batch in order; an abort that closes during the K-th promotion admits nothing beyond K.
- Verify: `go test -count=1 -race ./...`

Set up the module:

```bash
mkdir -p go-solutions/13-goroutines-and-channels/07-done-channel-pattern/11-rolling-deploy-health-gate-preempt/cmd/demo
cd go-solutions/13-goroutines-and-channels/07-done-channel-pattern/11-rolling-deploy-health-gate-preempt
```

Set the `go` directive in `go.mod` to `go 1.26`; the toolchain auto-downloads.

### Why a bare select leaks a promotion

The controller loop reads the next batch from `batches` and, unless told to stop, promotes it. The stop signal is a bare done channel: the owner of the deploy holds the bidirectional `chan struct{}` and closes it to broadcast rollback; the controller receives a `<-chan struct{}` it can only observe. The obvious loop body is:

```go
select {
case <-abort:
    return
case b := <-batches:
    promote(b)
}
```

This is subtly, dangerously wrong for a deploy. When abort has just closed and the queue still holds a ready batch, both cases are ready in the same iteration, and `select` chooses uniformly at random. Roughly half the time it picks the work case and promotes one more batch after the rollback decision was already made. In a rolling deploy that extra batch is more instances running the version you just decided to pull back — a wider blast radius, the exact failure the abort was supposed to prevent.

Strict preemption needs an explicit priority that `select`'s randomness cannot give you. The fix is a non-blocking re-check of abort, `select { case <-abort: return; default: }`, placed where it matters:

1. At the top of the loop, before work is even considered, so a closed abort preempts before the blocking select runs at all. This alone makes the pre-aborted case promote exactly zero — the first thing the loop does is observe the closed abort and return.
2. Immediately before the `promote` call, after the blocking select has already handed you a batch. The blocking select may have picked the work case while abort was also ready; this second guard re-checks abort right before the side effect, so the random pick can never leak a promotion. Between "I received a batch" and "I promote it" there is a window, and this guard closes it.

The blocking select still needs its own `case <-abort` so the loop wakes when the queue is empty and abort later closes. The two non-blocking guards handle priority; the blocking case handles liveness.

Create `promoter.go`:

```go
// Package promoter drives a rolling deploy one batch at a time, treating a
// closed abort channel as strictly higher priority than any pending work.
package promoter

// Batch is one unit a rolling deploy promotes to the new version.
type Batch struct {
	ID        int
	Instances int
}

// Promoter admits batches from a work channel and calls promote for each one
// it decides to ship. It is single-use: one goroutine calls Run once.
type Promoter struct {
	promote  func(Batch)
	promoted int
}

// New returns a Promoter that calls promote for each admitted batch.
func New(promote func(Batch)) *Promoter {
	return &Promoter{promote: promote}
}

// Run drains batches one at a time. It treats a closed abort as strictly higher
// priority than pending work: once abort is closed it admits no further batch.
//
// A bare select { case <-abort: case b := <-batches: } is not enough. When both
// cases are ready in the same iteration, select picks uniformly at random, so an
// already-aborted deploy could still promote one more batch and widen the blast
// radius. Two non-blocking re-checks close that gap: one at the top of the loop
// so a closed abort preempts before work is even considered, and one immediately
// before the promote so the random choice in the blocking select can never leak
// an extra promotion. Run returns the number of batches promoted.
func (p *Promoter) Run(abort <-chan struct{}, batches <-chan Batch) int {
	for {
		// Strict preempt: a closed abort wins before we look at work at all.
		select {
		case <-abort:
			return p.promoted
		default:
		}

		select {
		case <-abort:
			return p.promoted
		case b, ok := <-batches:
			if !ok {
				return p.promoted
			}
			// Re-check abort right before the side effect. The blocking select
			// above may have chosen this work case while abort was also ready;
			// this guard makes the abort win regardless of that random pick.
			select {
			case <-abort:
				return p.promoted
			default:
			}
			p.promote(b)
			p.promoted++
		}
	}
}
```

### The runnable demo

The demo runs three rollouts against pre-filled, closed queues, so there is no timing and the output is deterministic. The first has abort pre-closed; the second never aborts; the third closes abort as a side effect of the third promotion, modeling a health check that trips mid-deploy.

Create `cmd/demo/main.go`:

```go
package main

import (
	"fmt"

	"example.com/promoter"
)

// fill returns a closed channel pre-loaded with n ready batches, so every
// scenario below is deterministic: no timing, no goroutines racing the reader.
func fill(n int) <-chan promoter.Batch {
	ch := make(chan promoter.Batch, n)
	for i := range n {
		ch <- promoter.Batch{ID: i + 1, Instances: 10}
	}
	close(ch)
	return ch
}

func main() {
	// Scenario 1: abort already closed, queue full of ready work.
	// Strict preempt means ZERO promotions, not "about half".
	{
		abort := make(chan struct{})
		close(abort)
		var shipped []int
		p := promoter.New(func(b promoter.Batch) { shipped = append(shipped, b.ID) })
		n := p.Run(abort, fill(5))
		fmt.Printf("pre-aborted:   promoted=%d shipped=%v\n", n, shipped)
	}

	// Scenario 2: abort never fires, work channel is closed after 4 batches.
	// Every batch ships, in order.
	{
		abort := make(chan struct{}) // never closed
		var shipped []int
		p := promoter.New(func(b promoter.Batch) { shipped = append(shipped, b.ID) })
		n := p.Run(abort, fill(4))
		fmt.Printf("clean rollout: promoted=%d shipped=%v\n", n, shipped)
	}

	// Scenario 3: a health check trips during the 3rd promotion and closes
	// abort. No batch beyond the 3rd is admitted, even though 6 were queued.
	{
		abort := make(chan struct{})
		var shipped []int
		p := promoter.New(func(b promoter.Batch) {
			shipped = append(shipped, b.ID)
			if len(shipped) == 3 {
				close(abort) // rollback signal fires mid-deploy
			}
		})
		n := p.Run(abort, fill(6))
		fmt.Printf("gated at 3:    promoted=%d shipped=%v\n", n, shipped)
	}
}
```

Run it:

```bash
go run ./cmd/demo
```

Expected output:

```
pre-aborted:   promoted=0 shipped=[]
clean rollout: promoted=4 shipped=[1 2 3 4]
gated at 3:    promoted=3 shipped=[1 2 3]
```

### Tests

`TestPreAbortedPromotesZero` is the load-bearing determinism proof: with abort pre-closed and a 1000-item queue, `Run` must promote exactly zero, and it repeats the trial 200 times so the random pick inside the blocking select is exercised many times. A naive select racing abort against work would promote roughly half of a full queue, so asserting a hard zero — not "few" — pins the strict-priority guarantee. `TestCleanRolloutPromotesAllInOrder` proves the happy path: abort never closes, the closed queue drains completely, and order is preserved. `TestAbortAfterKAdmitsNothingBeyondK` closes abort as the side effect of the K-th promotion and asserts nothing beyond K is admitted even though the queue holds more, also repeated 200 times. `TestEmptyClosedQueuePromotesZero` covers the degenerate rollout with nothing to do.

Create `promoter_test.go`:

```go
package promoter

import (
	"slices"
	"testing"
)

// fill returns a closed channel pre-loaded with n ready batches (IDs 1..n).
// Because it is already closed and buffered, the reader in Run never blocks:
// on every iteration both the abort case and the work case can be ready at
// once, which is exactly the condition that makes a naive random select leak.
func fill(n int) <-chan Batch {
	ch := make(chan Batch, n)
	for i := range n {
		ch <- Batch{ID: i + 1, Instances: 10}
	}
	close(ch)
	return ch
}

// TestPreAbortedPromotesZero is the load-bearing determinism proof. With abort
// pre-closed and the queue full of ready work, strict preemption means exactly
// zero promotions. A naive select racing abort against work would promote
// roughly half of the queue; asserting zero (and repeating so the random pick
// is exercised many times) pins the strict-priority guarantee.
func TestPreAbortedPromotesZero(t *testing.T) {
	t.Parallel()

	const queue = 1000
	const trials = 200
	for range trials {
		abort := make(chan struct{})
		close(abort)

		var shipped []int
		p := New(func(b Batch) { shipped = append(shipped, b.ID) })
		got := p.Run(abort, fill(queue))

		if got != 0 {
			t.Fatalf("promoted = %d, want 0 (abort was pre-closed)", got)
		}
		if len(shipped) != 0 {
			t.Fatalf("shipped = %v, want none (abort was pre-closed)", shipped)
		}
	}
}

// TestCleanRolloutPromotesAllInOrder proves that with abort never closed and a
// closed work channel, Run admits every batch and preserves queue order.
func TestCleanRolloutPromotesAllInOrder(t *testing.T) {
	t.Parallel()

	abort := make(chan struct{}) // never closed

	var shipped []int
	p := New(func(b Batch) { shipped = append(shipped, b.ID) })
	got := p.Run(abort, fill(5))

	if got != 5 {
		t.Fatalf("promoted = %d, want 5", got)
	}
	if want := []int{1, 2, 3, 4, 5}; !slices.Equal(shipped, want) {
		t.Fatalf("shipped = %v, want %v", shipped, want)
	}
}

// TestAbortAfterKAdmitsNothingBeyondK proves that when abort closes during the
// K-th promotion, no batch beyond K is admitted even though the queue holds
// more ready items. The callback closes abort as its side effect, modeling a
// health check tripping mid-deploy.
func TestAbortAfterKAdmitsNothingBeyondK(t *testing.T) {
	t.Parallel()

	const gate = 3
	const queue = 20
	const trials = 200
	for range trials {
		abort := make(chan struct{})

		var shipped []int
		p := New(func(b Batch) {
			shipped = append(shipped, b.ID)
			if len(shipped) == gate {
				close(abort)
			}
		})
		got := p.Run(abort, fill(queue))

		if got != gate {
			t.Fatalf("promoted = %d, want %d (abort closed at the gate)", got, gate)
		}
		if want := []int{1, 2, 3}; !slices.Equal(shipped, want) {
			t.Fatalf("shipped = %v, want %v", shipped, want)
		}
	}
}

// TestEmptyClosedQueuePromotesZero covers the degenerate rollout: nothing to do.
func TestEmptyClosedQueuePromotesZero(t *testing.T) {
	t.Parallel()

	abort := make(chan struct{})
	p := New(func(Batch) { t.Fatal("promote called on an empty queue") })
	if got := p.Run(abort, fill(0)); got != 0 {
		t.Fatalf("promoted = %d, want 0", got)
	}
}
```

## Review

Correct here means one thing: once the rollback signal fires, not one more instance is promoted. The guarantee comes from treating a closed abort as strictly higher priority than pending work, which `select` alone cannot express because it chooses uniformly at random among ready cases. Two non-blocking `select { case <-abort: return; default: }` re-checks supply the missing priority — one at the top of the loop so a pre-closed abort promotes zero, one immediately before the `promote` side effect so a batch handed over by the random blocking select is still gated. `TestPreAbortedPromotesZero` proves it by asserting a hard zero across a 1000-item queue and 200 trials: without the guards the random select would ship roughly half, so zero is only reachable with strict preemption, and the assertion holds on every iteration under `-count=2 -race`. The production bug this prevents is the widened blast radius of a rolling deploy that keeps promoting after the health gate has already said stop.

## Resources

- [Go Blog: Go Concurrency Patterns — Pipelines and cancellation](https://go.dev/blog/pipelines) -- the done-channel cancellation idiom this controller is built on.
- [The Go Programming Language Specification: Select statements](https://go.dev/ref/spec#Select_statements) -- defines the uniform-random choice among ready cases that makes a bare select unsafe for strict priority.
- [Effective Go: Concurrency](https://go.dev/doc/effective_go#concurrency) -- channels, select, and the closed-channel-is-always-ready behavior the guards rely on.

---

Back to [00-concepts.md](00-concepts.md) | Prev: [10-bridge-done-to-context.md](10-bridge-done-to-context.md) | Next: [12-lock-lease-renewer-quit-ack.md](12-lock-lease-renewer-quit-ack.md)
