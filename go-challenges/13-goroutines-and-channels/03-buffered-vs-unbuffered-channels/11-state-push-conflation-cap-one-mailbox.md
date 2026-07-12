# Exercise 11: Live Config Push: A Cap-1 Conflating Mailbox That Never Blocks the Publisher

**Level: Intermediate**

A control plane pushes live state to worker goroutines: who the current leader is, the latest
feature-flag snapshot, a fresh routing table. The publish path sits on a hot, latency-sensitive
goroutine, so it must never block; memory must stay bounded no matter how far a worker falls behind;
and a worker that wakes up late must see the newest state, not replay a backlog of updates it no
longer cares about. An unbuffered channel forces the publisher into lockstep with the worker, and a
large buffer delivers stale states in order. This exercise builds the idiomatic answer: a cap-1
buffered channel used as a conflating mailbox, with a non-blocking send that replaces the pending
value so only the freshest update survives.

This module is self-contained: its own module, a `statepush` package, a demo, and tests.
Nothing here imports another exercise.

## What you'll build

```text
statepush/                   independent module: example.com/statepush
  go.mod                     go 1.26, require go.uber.org/goleak
  statepush.go               Mailbox[T]: cap-1 conflating mailbox (Publish/Receive/TryReceive)
  cmd/demo/main.go           runnable demo: three pushes conflate to the latest; burst; cancel
  statepush_test.go          latest-wins + replaced flags, newest-after-burst, blocking Receive, ctx cancel, concurrent invariants + goleak
```

- Files: `statepush.go`, `cmd/demo/main.go`, `statepush_test.go`.
- Implement: `NewMailbox[T any]() *Mailbox[T]`, `(*Mailbox[T]).Publish(v T) (replaced bool)`, `(*Mailbox[T]).Receive(ctx context.Context) (T, error)`, `(*Mailbox[T]).TryReceive() (v T, ok bool)`.
- Test: publishing never blocks and the latest value wins (`replaced==true` on coalesce); `Receive` after a burst returns the newest; `Receive` on a cancelled context returns `ctx.Err()` promptly; a many-publisher / one-consumer run keeps the slot depth at most 1 with race-clean accounting; goleak confirms no goroutine leak.
- Verify: `go test -count=1 -race ./...`

Set up the module:

```bash
go mod edit -go=1.26
go get go.uber.org/goleak
```

### A cap-1 channel as a conflating mailbox

The whole design turns on one number: a channel of capacity exactly 1. That single slot is the
storage, and it gives three properties at once. A `select { case ch <- v: default: }` send is
non-blocking, so the publisher never waits. `cap` is 1, so memory is bounded no matter how far the
worker lags. And because there is only one slot, an update that has not been read yet can be thrown
away and replaced with a newer one -- that replacement is conflation, and it is why a slow worker
sees the latest state instead of a queue of stale ones.

The subtle part is making "replace the pending value" atomic. Naively you would do a non-blocking
send, and if it fails because the slot is full, drain the stale value and send again. But between
your failed send and your drain, a receiver may already have taken the value, or another publisher
may be doing the same dance. The invariant we need is: at most one value is ever in the slot, and
each `Publish` deposits exactly one value while dropping at most one stale value. A mutex around the
publish path makes the drain-then-store one atomic step against other publishers; the receiver, which
only ever removes values, is safe to run without the lock because a channel receive is already
safe concurrently with a send.

The `Publish` protocol, under the mutex:

1. Try a non-blocking send. If the slot is empty the value lands, nothing was replaced, return `false`.
2. Otherwise the slot was full. Try a non-blocking receive to drain the stale value. If it succeeds, a value was coalesced (`replaced = true`); if a concurrent `Receive` already emptied the slot, the non-blocking receive takes the `default` and nothing was dropped.
3. Either way the slot is now empty, and because we hold the mutex no other publisher can refill it, so the final send fits immediately. `Publish` never blocks.

That final send is the delicate step: it is unconditional, yet guaranteed not to block, because the
only party that could re-fill the slot behind our back is another publisher, and the mutex excludes
them. Receivers only drain, so the slot stays empty until we send.

Create `statepush.go`:

```go
// Package statepush provides a conflating, cap-1 mailbox for pushing live state
// (a leader identity, a feature-flag snapshot, a routing table) to a worker.
// Publish never blocks the publisher; when a value is already pending it is
// replaced (coalesced) so a slow worker observes only the freshest state.
package statepush

import (
	"context"
	"sync"
)

// Mailbox holds at most one pending value. The channel's capacity of 1 is the
// storage; the mutex serializes publishers so the drain-then-store replacement
// happens as one atomic step against other publishers.
type Mailbox[T any] struct {
	mu sync.Mutex // serializes Publish so the single slot is replaced atomically
	ch chan T     // cap 1: the single-value mailbox slot
}

// NewMailbox returns an empty mailbox whose slot holds at most one value.
func NewMailbox[T any]() *Mailbox[T] {
	return &Mailbox[T]{ch: make(chan T, 1)}
}

// Publish stores v as the pending value and never blocks. If the slot is empty
// the value is deposited and replaced is false. If a value is already pending it
// is drained and discarded (coalesced) so only v survives, and replaced is true.
func (m *Mailbox[T]) Publish(v T) (replaced bool) {
	m.mu.Lock()
	defer m.mu.Unlock()

	// Fast path: slot empty, deposit and return.
	select {
	case m.ch <- v:
		return false
	default:
	}

	// Slot was full. Drain the stale value if it is still there (a concurrent
	// Receive may have already taken it). Either way the slot is now empty and,
	// because we hold the mutex, no other publisher can refill it, so the final
	// send fits immediately and Publish never blocks.
	select {
	case <-m.ch:
		replaced = true
	default:
	}
	m.ch <- v
	return replaced
}

// Receive blocks for the latest pending value or until ctx is done, in which
// case it returns the zero value and ctx.Err().
func (m *Mailbox[T]) Receive(ctx context.Context) (T, error) {
	select {
	case v := <-m.ch:
		return v, nil
	case <-ctx.Done():
		var zero T
		return zero, ctx.Err()
	}
}

// TryReceive returns the pending value without blocking. ok is false when the
// slot is empty.
func (m *Mailbox[T]) TryReceive() (v T, ok bool) {
	select {
	case v = <-m.ch:
		return v, true
	default:
		var zero T
		return zero, false
	}
}
```

### The runnable demo

The demo runs on a single goroutine so its output is deterministic. It pushes three states with no
receiver (proving the publisher never blocks and that the pushes conflate to the latest), retrieves
that latest with `TryReceive`, shows a burst collapsing to its newest value, and shows `Receive`
returning promptly on a cancelled context.

Create `cmd/demo/main.go`:

```go
package main

import (
	"context"
	"fmt"

	"example.com/statepush"
)

func main() {
	mb := statepush.NewMailbox[int]()

	// Three pushes with no receiver. None blocks; each later push conflates the
	// pending one, so only the newest state survives.
	fmt.Printf("Publish(1) replaced=%v\n", mb.Publish(1))
	fmt.Printf("Publish(2) replaced=%v\n", mb.Publish(2))
	fmt.Printf("Publish(3) replaced=%v\n", mb.Publish(3))

	v, ok := mb.TryReceive()
	fmt.Printf("TryReceive -> %d ok=%v\n", v, ok)
	_, ok = mb.TryReceive()
	fmt.Printf("TryReceive -> empty ok=%v\n", ok)

	// A burst of updates; a worker that shows up late sees only the latest.
	for i := 10; i <= 14; i++ {
		mb.Publish(i)
	}
	v, err := mb.Receive(context.Background())
	fmt.Printf("Receive after burst -> %d err=%v\n", v, err)

	// A cancelled context unblocks Receive promptly with ctx.Err().
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	_, err = mb.Receive(ctx)
	fmt.Printf("Receive on cancelled ctx -> err=%v\n", err)
}
```

Run it:

```bash
go run ./cmd/demo
```

Expected output:

```
Publish(1) replaced=false
Publish(2) replaced=true
Publish(3) replaced=true
TryReceive -> 3 ok=true
TryReceive -> empty ok=false
Receive after burst -> 14 err=<nil>
Receive on cancelled ctx -> err=context canceled
```

### Tests

`TestLatestWinsAndReplacedFlags` uses a single publisher for determinism: three pushes with no
receiver never block, the first reports `replaced=false` and the next two `replaced=true`, and only
the newest value (3) is retrievable -- a second `TryReceive` reports the slot empty.
`TestReceiveReturnsNewestAfterBurst` pushes five values and asserts `Receive` returns the newest, not
the oldest. `TestReceiveBlocksUntilPublish` parks `Receive` on an empty mailbox in a goroutine and
wakes it with a later `Publish`, using the channel handoff (not a sleep) as the synchronization.
`TestReceiveCancelledContext` asserts a cancelled context yields `context.Canceled` and the zero
value. `TestConcurrentPublishersInvariants` runs sixteen publishers against one consumer and asserts
invariants only: the slot depth (`len`) never exceeds 1, and the accounting identity
`published == received + coalesced + finalRemaining` holds exactly -- every `Publish` deposits one
value, each `replaced==true` drops one stale value, each `Receive` removes one, and at most one
remains. `TestMain` wraps the suite in `goleak.VerifyTestMain` so a leaked `Receive` goroutine fails
the run.

Create `statepush_test.go`:

```go
package statepush

import (
	"context"
	"errors"
	"sync"
	"sync/atomic"
	"testing"

	"go.uber.org/goleak"
)

func TestMain(m *testing.M) {
	goleak.VerifyTestMain(m)
}

// TestLatestWinsAndReplacedFlags pins the coalescing contract with a single
// publisher: three pushes never block, the first reports replaced=false and the
// next two replaced=true, and only the newest value (3) is retrievable.
func TestLatestWinsAndReplacedFlags(t *testing.T) {
	t.Parallel()

	mb := NewMailbox[int]()
	if r := mb.Publish(1); r {
		t.Fatalf("Publish(1) replaced=%v, want false", r)
	}
	if r := mb.Publish(2); !r {
		t.Fatalf("Publish(2) replaced=%v, want true", r)
	}
	if r := mb.Publish(3); !r {
		t.Fatalf("Publish(3) replaced=%v, want true", r)
	}

	v, ok := mb.TryReceive()
	if !ok || v != 3 {
		t.Fatalf("TryReceive = (%d, %v), want (3, true)", v, ok)
	}
	if _, ok := mb.TryReceive(); ok {
		t.Fatalf("second TryReceive ok=%v, want false (slot drained)", ok)
	}
}

// TestReceiveReturnsNewestAfterBurst asserts that after a burst of updates a
// receiver observes only the latest, not a backlog of stale ones.
func TestReceiveReturnsNewestAfterBurst(t *testing.T) {
	t.Parallel()

	mb := NewMailbox[int]()
	for i := range 5 { // 0,1,2,3,4 -> newest is 4
		mb.Publish(i)
	}
	v, err := mb.Receive(context.Background())
	if err != nil {
		t.Fatalf("Receive err = %v, want nil", err)
	}
	if v != 4 {
		t.Fatalf("Receive = %d, want 4 (newest wins)", v)
	}
}

// TestReceiveBlocksUntilPublish shows Receive parks on an empty mailbox and is
// woken by a later Publish. The channel handoff is the synchronization; no sleep
// is needed for correctness.
func TestReceiveBlocksUntilPublish(t *testing.T) {
	t.Parallel()

	mb := NewMailbox[int]()
	got := make(chan int, 1)
	go func() {
		v, err := mb.Receive(context.Background())
		if err != nil {
			got <- -1
			return
		}
		got <- v
	}()

	mb.Publish(42)
	if v := <-got; v != 42 {
		t.Fatalf("received %d, want 42", v)
	}
}

// TestReceiveCancelledContext asserts Receive returns ctx.Err() promptly on a
// cancelled context instead of blocking forever on an empty mailbox.
func TestReceiveCancelledContext(t *testing.T) {
	t.Parallel()

	mb := NewMailbox[int]()
	ctx, cancel := context.WithCancel(context.Background())
	cancel()

	v, err := mb.Receive(ctx)
	if !errors.Is(err, context.Canceled) {
		t.Fatalf("err = %v, want context.Canceled", err)
	}
	if v != 0 {
		t.Fatalf("value = %d, want zero on cancellation", v)
	}
}

// TestConcurrentPublishersInvariants runs many publishers against one consumer
// and asserts the invariants only (values are nondeterministic under conflation).
// The accounting identity is the core proof:
//
//	published == received + coalesced + finalRemaining
//
// Every Publish deposits exactly one value; a replaced==true Publish discards one
// prior value (coalesced); Receive removes one; at most one remains at the end.
// The mailbox slot never holds more than one value, verified by sampling len.
func TestConcurrentPublishersInvariants(t *testing.T) {
	t.Parallel()

	const (
		publishers = 16
		perPub     = 500
	)
	mb := NewMailbox[int]()

	var published, coalesced, received atomic.Int64

	ctx, cancel := context.WithCancel(context.Background())
	consumerDone := make(chan struct{})
	go func() {
		defer close(consumerDone)
		for {
			if _, err := mb.Receive(ctx); err != nil {
				return
			}
			received.Add(1)
		}
	}()

	// Watcher samples the slot depth; len(ch) is a safe (non-data-race) read.
	watchStop := make(chan struct{})
	watchDone := make(chan struct{})
	var maxLen int
	go func() {
		defer close(watchDone)
		for {
			if n := len(mb.ch); n > maxLen {
				maxLen = n
			}
			select {
			case <-watchStop:
				return
			default:
			}
		}
	}()

	var wg sync.WaitGroup
	for p := range publishers {
		wg.Go(func() {
			for i := range perPub {
				if mb.Publish(p*perPub + i) {
					coalesced.Add(1)
				}
				published.Add(1)
			}
		})
	}
	wg.Wait()

	// Stop the consumer, then take whatever single value may remain.
	cancel()
	<-consumerDone
	close(watchStop)
	<-watchDone

	var finalRemaining int64
	if _, ok := mb.TryReceive(); ok {
		finalRemaining = 1
	}

	if maxLen > 1 {
		t.Fatalf("observed slot depth %d, want <= 1", maxLen)
	}
	pub := published.Load()
	if want := int64(publishers * perPub); pub != want {
		t.Fatalf("published = %d, want %d", pub, want)
	}
	if got := received.Load() + coalesced.Load() + finalRemaining; got != pub {
		t.Fatalf("accounting: received(%d)+coalesced(%d)+remaining(%d) = %d, want published %d",
			received.Load(), coalesced.Load(), finalRemaining, got, pub)
	}
}
```

## Review

"Correct" here means three things hold together: `Publish` never blocks, memory stays bounded at one
pending value, and a receiver sees the freshest state rather than a stale backlog. The cap-1 channel
is what bounds memory; the non-blocking `select` send is what keeps the publisher off the worker's
critical path; and the drain-then-store under the mutex is what makes coalescing atomic, so the slot
holds at most one value and each `Publish` drops at most one stale value. The concurrency test proves
this without depending on nondeterministic values: it checks the sampled slot depth stays at most 1
and that the exact accounting identity `published == received + coalesced + finalRemaining` balances,
which can only hold if no value is ever lost or double-counted; goleak proves the blocking `Receive`
goroutine is always released by context cancellation. The production bug this prevents is the one you
get from the two naive alternatives: an unbuffered channel couples the control-plane publisher to the
slowest worker (a stall in one worker stalls every state push), and an oversized buffer delivers a
worker a queue of obsolete leaders and routing tables it must churn through before it reaches the one
that is actually current. Conflation is the right shape whenever only the latest value matters.

## Resources

- [The Go Memory Model](https://go.dev/ref/mem) -- why a buffered send/receive orders each value's own handoff, the guarantee the mailbox relies on.
- [Effective Go: Channels](https://go.dev/doc/effective_go#channels) -- buffered channels, non-blocking sends with `select`+`default`, and the cap-1 idiom.
- [pkg.go.dev: sync.Mutex](https://pkg.go.dev/sync#Mutex) -- serializing the drain-then-store so the single slot is replaced atomically against other publishers.
- [pkg.go.dev: go.uber.org/goleak](https://pkg.go.dev/go.uber.org/goleak) -- goroutine-leak detection that catches a `Receive` that never returns.

---

Back to [00-concepts.md](00-concepts.md) | Prev: [10-outbox-relay-unbuffered-confirm-handoff.md](10-outbox-relay-unbuffered-confirm-handoff.md) | Next: [12-hedged-request-buffered-reply-noleak.md](12-hedged-request-buffered-reply-noleak.md)
