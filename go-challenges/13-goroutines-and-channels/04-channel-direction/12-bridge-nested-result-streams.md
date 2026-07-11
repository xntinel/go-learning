# Exercise 12: Bridge a Receive-Only Channel of Receive-Only Result Streams

**Level: Advanced**

A batch job coordinator emits results in waves: each batch spins up its own
sub-stream of results, and the coordinator publishes those sub-streams one after
another on a single outer channel. Downstream consumers want one continuous
stream, not a channel of channels, so a `Bridge` flattens `<-chan (<-chan T)`
into a single `<-chan T`. The naive flatten leaks: when a consumer walks away and
the context is cancelled while the bridge goroutine is parked mid-drain on a slow
inner stream, a flatten that only watches its channels never returns and the
goroutine is lost forever.

This module is self-contained: its own module, a `bridge` package, a demo, and
tests. Nothing here imports another exercise.

## What you'll build

```text
bridge/                      independent module: example.com/bridge
  go.mod                     go 1.26
  bridge.go                  Bridge[T](ctx, <-chan (<-chan T)) <-chan T
  cmd/demo/main.go           runnable demo: flatten three batches, then cancel mid-inner
  bridge_test.go             pass-through order, empty outer, cancel-mid-inner, cause, goleak
```

- Files: `bridge.go`, `cmd/demo/main.go`, `bridge_test.go`.
- Implement: `Bridge[T any](ctx context.Context, chans <-chan (<-chan T)) <-chan T` — flatten a receive-only stream of receive-only streams into one receive-only output, draining each inner stream fully before advancing.
- Test: ordered concatenation with a normal close; an empty outer stream closes at once; cancelling mid-inner closes the output promptly; `context.WithCancelCause` propagates a distinguishable cause; `go.uber.org/goleak` proves no goroutine leaks.
- Verify: `go test -count=1 -race ./...`

Set up the module:

```bash
mkdir -p ~/go-exercises/bridge/cmd/demo
cd ~/go-exercises/bridge
go mod init example.com/bridge
go get go.uber.org/goleak
go mod tidy
```

### The nested direction is the whole contract, and cancellation is the hard part

The signature `Bridge(ctx context.Context, chans <-chan (<-chan T)) <-chan T`
encodes ownership at two levels at once. The outer `<-chan (<-chan T)` is
receive-only: the bridge drains the coordinator's stream of sub-streams and can
neither publish a fake sub-stream onto it nor close it. Each element it receives
is itself a `<-chan T`, receive-only: the bridge drains an inner stream but can
never send into it or close it — the batch that produced the sub-stream owns that
close. The only channel the bridge owns is the one it creates and returns, and
the returned `<-chan T` hands consumers a drain-only view they cannot corrupt.
Direction makes all three ownership rules unforgeable by the compiler.

The flatten loop is two nested drains. The outer loop receives the next inner
stream; the inner loop drains that stream to completion (a closed inner channel
yields `ok == false`, which is the signal to advance to the next sub-stream)
before the outer loop runs again. That "drain fully before advancing" is what
makes the output an ordered concatenation rather than an interleaving.

The cancellation obligation is where a naive bridge breaks. There are three
places the goroutine can park: receiving the next inner stream from `chans`,
receiving the next value from the current inner stream, and sending a forwarded
value on `out` to a slow consumer. If the context is cancelled while parked on
any of them and that receive or send has no `ctx.Done()` companion in its
`select`, the goroutine blocks forever and leaks. So every one of the three
blocking operations is a `select` with a `case <-ctx.Done(): return`. On any exit
path — outer stream closed, or cancellation at any of the three points — a single
`defer close(out)` closes the output exactly once. That defer is the only closer;
because the bridge is the sole owner of `out`, there is no double-close hazard.

Create `bridge.go`:

```go
package bridge

import "context"

// Bridge flattens a receive-only stream of receive-only streams into one
// receive-only output. It drains each inner stream fully before advancing to
// the next, forwards every value to out, and closes out when the outer stream
// closes or ctx is cancelled. The nested direction <-chan (<-chan T) is the
// whole contract: neither the outer stream nor any inner stream can be sent to
// or closed by the bridge's consumers.
func Bridge[T any](ctx context.Context, chans <-chan (<-chan T)) <-chan T {
	out := make(chan T)

	go func() {
		// The single owner of out closes it exactly once on every exit path:
		// outer stream closed, or context cancelled at any parked receive.
		defer close(out)

		for {
			var inner <-chan T
			select {
			case <-ctx.Done():
				return
			case c, ok := <-chans:
				if !ok {
					return // outer stream closed: no more inner streams.
				}
				inner = c
			}

			// Drain this inner stream fully before touching the outer stream
			// again. The parked receive here is the leak-prone spot, so it is
			// guarded by ctx.Done().
		drain:
			for {
				select {
				case <-ctx.Done():
					return
				case v, ok := <-inner:
					if !ok {
						break drain // inner drained: advance to the next stream.
					}
					// The forward send can block on a slow consumer, so it is
					// itself guarded by ctx.Done().
					select {
					case <-ctx.Done():
						return
					case out <- v:
					}
				}
			}
		}
	}()

	return out
}
```

### The runnable demo

The demo runs two phases. First it publishes three batches with known values and
drains the bridged output, showing the ordered concatenation and the close that
follows the outer stream closing. Then it builds a single inner stream that
yields one value and is never closed, so the bridge parks mid-drain; the demo
reads the one value, cancels with a distinguishable cause, confirms the output
closes, and prints the cause. Every inner stream is buffered and closed before
publication, so the print order is deterministic.

Create `cmd/demo/main.go`:

```go
package main

import (
	"context"
	"errors"
	"fmt"

	"example.com/bridge"
)

// batches publishes one receive-only inner stream per seed slice. Each inner
// stream is buffered and closed before it is published, so draining order is
// fully deterministic. The returned channel is receive-only: consumers can only
// drain it, never send or close.
func batches(seed [][]int) <-chan (<-chan int) {
	outer := make(chan (<-chan int))
	go func() {
		defer close(outer)
		for _, b := range seed {
			inner := make(chan int, len(b))
			for _, v := range b {
				inner <- v
			}
			close(inner)
			outer <- inner
		}
	}()
	return outer
}

var errBatchAborted = errors.New("batch job aborted")

func main() {
	// Part 1: pass-through. Bridge flattens three inner streams into one
	// ordered output, and the output closes after the outer stream closes.
	outer := batches([][]int{{1, 2}, {3}, {4, 5, 6}})
	for v := range bridge.Bridge(context.Background(), outer) {
		fmt.Printf("result %d\n", v)
	}
	fmt.Println("outer closed -> output closed")

	// Part 2: cancel while parked on an inner stream. The inner stream yields
	// one value and is never closed, so the bridge goroutine parks on its next
	// inner receive. Cancelling with a cause abandons it and closes the output.
	ctx, cancel := context.WithCancelCause(context.Background())
	slow := make(chan int, 1)
	slow <- 100
	pending := make(chan (<-chan int), 1)
	pending <- slow // never closed; the bridge will park draining it.

	out := bridge.Bridge(ctx, pending)
	fmt.Printf("received %d, then cancelling mid-inner\n", <-out)
	cancel(errBatchAborted)
	for range out { // drains until Bridge closes out on cancellation.
	}
	fmt.Printf("output closed, cause: %v\n", context.Cause(ctx))
}
```

Run it:

```bash
go run ./cmd/demo
```

Expected output:

```
result 1
result 2
result 3
result 4
result 5
result 6
outer closed -> output closed
received 100, then cancelling mid-inner
output closed, cause: batch job aborted
```

### Tests

`TestBridgePassThroughOrderedConcatenation` drains a bridge over four inner
streams (one of them empty) and asserts the output equals the ordered
concatenation and then closes. `TestBridgeEmptyOuterClosesImmediately` closes the
outer stream with no inner streams and asserts the output closes at once.
`TestBridgeCancelMidInnerStopsPromptly` parks the bridge on a slow inner stream
that never closes, cancels, and asserts the output closes with no further value.
`TestBridgeCancelCausePropagates` uses `context.WithCancelCause` and asserts the
cause survives the abandonment. `TestMain` wraps the package in
`goleak.VerifyTestMain`, so the goroutine parked on an inner receive is proven
gone after every test.

Create `bridge_test.go`:

```go
package bridge

import (
	"context"
	"errors"
	"slices"
	"testing"
	"time"

	"go.uber.org/goleak"
)

// TestMain proves leak-freedom: after every test returns, goleak asserts that
// the bridge goroutine parked on an inner receive is verifiably gone.
func TestMain(m *testing.M) {
	goleak.VerifyTestMain(m)
}

// publish emits one receive-only inner stream per seed slice. Each inner stream
// is buffered and closed before publication, so draining order is deterministic.
func publish(seed [][]int) <-chan (<-chan int) {
	outer := make(chan (<-chan int))
	go func() {
		defer close(outer)
		for _, b := range seed {
			inner := make(chan int, len(b))
			for _, v := range b {
				inner <- v
			}
			close(inner)
			outer <- inner
		}
	}()
	return outer
}

// TestBridgePassThroughOrderedConcatenation pins down the happy path: with no
// cancellation the output is the ordered concatenation of every inner stream
// (an empty inner stream is drained and skipped), and the output closes after
// the outer stream closes.
func TestBridgePassThroughOrderedConcatenation(t *testing.T) {
	t.Parallel()

	seed := [][]int{{1, 2, 3}, {}, {4}, {5, 6}}
	want := []int{1, 2, 3, 4, 5, 6}

	var got []int
	for v := range Bridge(context.Background(), publish(seed)) {
		got = append(got, v)
	}
	if !slices.Equal(got, want) {
		t.Fatalf("flattened output = %v, want %v", got, want)
	}
}

// TestBridgeEmptyOuterClosesImmediately pins down that an already-closed outer
// stream yields no values and closes the output at once.
func TestBridgeEmptyOuterClosesImmediately(t *testing.T) {
	t.Parallel()

	outer := make(chan (<-chan int))
	close(outer)

	out := Bridge(context.Background(), outer)
	select {
	case v, ok := <-out:
		if ok {
			t.Fatalf("empty outer produced a value %v", v)
		}
	case <-time.After(time.Second):
		t.Fatal("empty outer did not close the output")
	}
}

// TestBridgeCancelMidInnerStopsPromptly pins down cancellation correctness: with
// the bridge parked on a slow inner stream that never closes, cancelling the
// context closes the output promptly and forwards nothing further.
func TestBridgeCancelMidInnerStopsPromptly(t *testing.T) {
	t.Parallel()

	ctx, cancel := context.WithCancel(context.Background())

	slow := make(chan int, 1)
	slow <- 1
	outer := make(chan (<-chan int), 1)
	outer <- slow // inner never closed, outer never closed.

	out := Bridge(ctx, outer)
	if got := <-out; got != 1 {
		t.Fatalf("first value = %d, want 1", got)
	}

	// The bridge is now parked on slow's next receive. Cancelling must abandon
	// it, close out, and forward nothing more (slow has no further buffered
	// values, so the next receive on out must be the close).
	cancel()
	select {
	case v, ok := <-out:
		if ok {
			t.Fatalf("received value %v after cancel, want closed output", v)
		}
	case <-time.After(time.Second):
		t.Fatal("output not closed promptly after cancel")
	}
}

// TestBridgeCancelCausePropagates pins down that context.WithCancelCause carries
// a distinguishable cause across the bridge's abandonment.
func TestBridgeCancelCausePropagates(t *testing.T) {
	t.Parallel()

	ctx, cancel := context.WithCancelCause(context.Background())
	errAborted := errors.New("batch job aborted mid-stream")

	inner := make(chan int, 1)
	inner <- 7
	outer := make(chan (<-chan int), 1)
	outer <- inner // never closed; bridge parks after forwarding 7.

	out := Bridge(ctx, outer)
	if got := <-out; got != 7 {
		t.Fatalf("first value = %d, want 7", got)
	}

	cancel(errAborted)
	for range out { // drains until Bridge closes out on cancellation.
	}
	if got := context.Cause(ctx); !errors.Is(got, errAborted) {
		t.Fatalf("cause = %v, want %v", got, errAborted)
	}
}
```

## Review

The bridge is correct when the output is the ordered concatenation of every inner
stream under normal completion, and when a cancellation at any parked point
abandons the goroutine, closes the output exactly once, and forwards nothing
further. The nested receive-only direction `<-chan (<-chan T)` is what guarantees
the bridge can only drain the outer and inner streams — never publish a fake
sub-stream or close a batch's channel — while the sole `defer close(out)` on the
owned output guarantees exactly-once close across all four exit paths. The three
`ctx.Done()` guards, one per blocking operation, are the load-bearing detail:
`goleak.VerifyTestMain` fails the suite if any of them is missing, because the
goroutine parked on a slow inner receive would survive the cancelled test. That
is the exact production bug this pattern prevents: a result-stream flattener that
leaks one goroutine per abandoned batch until the coordinator exhausts memory.

## Resources

- [Go Concurrency Patterns: Pipelines and cancellation](https://go.dev/blog/pipelines) — the bridge/flatten and explicit-cancellation patterns this exercise generalizes to a nested stream.
- [`context.WithCancelCause`](https://pkg.go.dev/context#WithCancelCause) — the cancel-with-cause API and `context.Cause` that carry a distinguishable abandonment reason.
- [`go.uber.org/goleak`](https://pkg.go.dev/go.uber.org/goleak) — `VerifyTestMain`, the goroutine-leak detector that turns "no leak" into a failing assertion.
- [Go spec: Channel types](https://go.dev/ref/spec#Channel_types) — the nested receive-only direction that encodes ownership at both the outer and inner level.

---

Back to [00-concepts.md](00-concepts.md) | Prev: [11-sequential-segment-replay-concat.md](11-sequential-segment-replay-concat.md) | Next: [13-credit-based-flow-control.md](13-credit-based-flow-control.md)
