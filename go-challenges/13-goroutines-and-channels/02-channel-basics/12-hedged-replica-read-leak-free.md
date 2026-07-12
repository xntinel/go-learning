# Exercise 12: First-Response-Wins Hedged Reads Without Leaking the Loser

**Level: Advanced**

A read path can cut tail latency by querying two or more replicas at once and
returning whichever answers first, abandoning the slower ones. The naive version
leaks: once Read has its answer and walks away, each abandoned replica still has
a result to deliver, and if it tries to send that result on a channel nobody is
receiving from, its goroutine blocks on the send forever. This exercise builds a
hedged read where every replica owns a capacity-1 reply channel, so the loser's
single send always lands in the buffer and the goroutine exits cleanly.

This module is self-contained: its own module, a `hedge` package, a demo, and tests.
Nothing here imports another exercise.

## What you'll build

```text
hedge/                       independent module: example.com/hedge
  go.mod                     go 1.26
  hedge.go                   type Replica; func Read(ctx, ...Replica) (string, error)
  cmd/demo/main.go           runnable demo: first-wins, partial failure, all-fail
  hedge_test.go              fast-wins, correctness, partial failure, all-fail join, leak-free
```

- Files: `hedge.go`, `cmd/demo/main.go`, `hedge_test.go`.
- Implement: `type Replica func(ctx context.Context) (string, error)` and `func Read(ctx context.Context, replicas ...Replica) (string, error)`.
- Test: fast replica wins while the slow one is gated; result correctness; partial failure returns the successful value; all-fail returns `errors.Join` of every error; zero leaked goroutines after the losers are released; the capacity-1 channel is load-bearing.
- Verify: `go test -count=1 -race ./...`

Set up the module:

```bash
mkdir -p go-solutions/13-goroutines-and-channels/02-channel-basics/12-hedged-replica-read-leak-free/cmd/demo
cd go-solutions/13-goroutines-and-channels/02-channel-basics/12-hedged-replica-read-leak-free
go get go.uber.org/goleak
```

Set the `go` directive in `go.mod` to `go 1.26`, then `go mod tidy`.

### Why the loser must never block on its send

The hedged-read shape is: launch N replicas, take the first success, cancel the
rest. The correctness trap is not in taking the first answer — it is in what
happens to the answers you did not take. Canceling the context asks a slow
replica to stop, but one that is mid-flight (or that ignores cancellation) will
still finish and try to hand back its result. Where does that result go?

If all replicas share one unbuffered reply channel, the loser's `ch <- result`
has no receiver — Read already returned — so the send blocks forever and the
goroutine leaks: one leaked goroutine per hedged read per abandoned replica, a
slow-motion leak that only shows up in production. The fix is a per-replica
channel with capacity 1:

1. Read makes one `chan result` of capacity 1 for each replica and launches the
   replica in a goroutine that captures only that channel.
2. Each replica performs exactly one send. Because the channel has a free buffer
   slot and each replica sends at most once, that send completes immediately
   whether or not anyone is receiving. The goroutine then returns.
3. Read waits on all the reply channels at once with `reflect.Select` and returns
   the first `result` whose error is nil, canceling the shared context on the way
   out. Losers that have not sent yet still finish, send into their own buffer,
   and exit. Nothing is stranded.

The capacity-1 buffer is the whole point: it decouples the loser's send from
Read's willingness to receive, where an unbuffered channel would couple them and
turn every loser into a permanent leak. If every replica fails, Read joins the
errors with `errors.Join`, indexed by replica so the result is deterministic
regardless of arrival order.

Create `hedge.go`:

```go
// Package hedge implements a first-response-wins hedged read across replicas.
package hedge

import (
	"context"
	"errors"
	"reflect"
)

// Replica queries one backend and returns its result or an error. The context
// is canceled once some other replica has already answered, so a replica that
// honors cancellation can stop early.
type Replica func(ctx context.Context) (string, error)

// errWon is the cancellation cause once a replica has answered; it is internal
// and never returned to the caller.
var errWon = errors.New("hedge: another replica already answered")

// result is one replica's reply.
type result struct {
	val string
	err error
}

// Read queries every replica concurrently and returns the first successful
// result, canceling the shared context so the remaining replicas can abandon
// their work. Each replica sends its single reply into its OWN capacity-1
// channel: the buffer slot means the send always completes even after Read has
// returned and no one is receiving, so the losing goroutines exit instead of
// blocking forever on the send. An unbuffered reply channel here would deadlock
// every loser and leak its goroutine. If every replica fails, Read returns
// errors.Join of all replica errors.
func Read(ctx context.Context, replicas ...Replica) (string, error) {
	if len(replicas) == 0 {
		return "", errors.New("hedge: no replicas")
	}

	ctx, cancel := context.WithCancelCause(ctx)
	defer cancel(errWon)

	// One capacity-1 reply channel per replica. Capacity 1 is load-bearing: the
	// loser's single send lands in its buffer with no receiver waiting and the
	// goroutine returns cleanly.
	cases := make([]reflect.SelectCase, len(replicas))
	origin := make([]int, len(replicas))
	for i, rep := range replicas {
		ch := make(chan result, 1)
		cases[i] = reflect.SelectCase{Dir: reflect.SelectRecv, Chan: reflect.ValueOf(ch)}
		origin[i] = i
		go func(rep Replica, ch chan<- result) {
			val, err := rep(ctx)
			ch <- result{val: val, err: err} // buffered: never blocks, never leaks
		}(rep, ch)
	}

	// Receive whichever reply arrives first. On a success, return immediately;
	// the deferred cancel abandons the slower replicas. On a failure, drop that
	// case and keep waiting on the rest. errs is indexed by original replica so
	// the joined error is deterministic regardless of arrival order.
	errs := make([]error, len(replicas))
	for len(cases) > 0 {
		chosen, recv, _ := reflect.Select(cases)
		r := recv.Interface().(result)
		if r.err == nil {
			return r.val, nil
		}
		errs[origin[chosen]] = r.err
		cases = append(cases[:chosen], cases[chosen+1:]...)
		origin = append(origin[:chosen], origin[chosen+1:]...)
	}
	return "", errors.Join(errs...)
}
```

### The runnable demo

The demo is deterministic: the "gated" replica blocks on the shared context and
can only return once another replica has already won, so the fast replica always
wins the race.

Create `cmd/demo/main.go`:

```go
package main

import (
	"context"
	"errors"
	"fmt"

	"example.com/hedge"
)

// fast answers immediately.
func fast(val string) hedge.Replica {
	return func(context.Context) (string, error) { return val, nil }
}

// gated blocks until the shared context is canceled (i.e. until another replica
// has already won), then reports that it was abandoned. It never wins a race, so
// the demo is deterministic.
func gated(val string) hedge.Replica {
	return func(ctx context.Context) (string, error) {
		<-ctx.Done()
		return val, ctx.Err()
	}
}

// fails answers immediately with an error.
func fails(msg string) hedge.Replica {
	return func(context.Context) (string, error) { return "", errors.New(msg) }
}

func main() {
	ctx := context.Background()

	// First response wins: the gated replica cannot return until the fast one
	// has already answered and Read cancels, so fast deterministically wins.
	val, err := hedge.Read(ctx, gated("replica-B"), fast("replica-A"))
	fmt.Printf("first-wins: val=%q err=%v\n", val, err)

	// Partial failure: one replica errors immediately, the other succeeds.
	val, err = hedge.Read(ctx, fails("replica-A down"), fast("replica-B"))
	fmt.Printf("partial-failure: val=%q err=%v\n", val, err)

	// All replicas fail: Read returns errors.Join, in replica order.
	val, err = hedge.Read(ctx, fails("replica-A down"), fails("replica-B down"))
	fmt.Printf("all-fail: val=%q err=%v\n", val, err)
}
```

Run it:

```bash
go run ./cmd/demo
```

Expected output:

```
first-wins: val="replica-A" err=<nil>
partial-failure: val="replica-B" err=<nil>
all-fail: val="" err=replica-A down
replica-B down
```

### Tests

`TestFastWinsWhileSlowGated` pins the race deterministically: the slow replica
blocks on a test-controlled `release` channel and cannot produce a result until
the test closes it, which happens only after Read has returned, so the fast
replica must win. `TestResultCorrectness` checks a single replica's value flows
through unchanged. `TestPartialFailureReturnsSuccess` puts a failing replica
first and asserts Read skips it and returns the second replica's value.
`TestAllFailJoinsEveryError` asserts the returned error wraps every replica error
via `errors.Is`. `TestCapacityOneIsLoadBearing` releases the loser strictly after
Read returned and then calls `goleak.VerifyNone` to prove the loser's buffered
send completed and its goroutine exited — an unbuffered channel would strand it.
`TestMain` runs `goleak.VerifyTestMain`, and every gated replica is released
before its test returns, so a correct implementation leaves no goroutine behind.

Create `hedge_test.go`:

```go
package hedge_test

import (
	"context"
	"errors"
	"testing"

	"example.com/hedge"
	"go.uber.org/goleak"
)

// TestMain asserts that no goroutine outlives the test binary. Every test that
// gates a slow replica releases it before returning, so a correct
// implementation leaves nothing behind. An unbuffered reply channel would strand
// each loser on its send and this check would fail.
func TestMain(m *testing.M) {
	goleak.VerifyTestMain(m)
}

// fast answers immediately with val.
func fast(val string) hedge.Replica {
	return func(context.Context) (string, error) { return val, nil }
}

// newGated builds a replica that blocks until release is closed, then returns
// val. finished is closed just before the replica returns, so a test can wait
// for the loser to run without sleeping.
func newGated(val string) (rep hedge.Replica, release, finished chan struct{}) {
	release = make(chan struct{})
	finished = make(chan struct{})
	rep = func(context.Context) (string, error) {
		<-release
		close(finished)
		return val, nil
	}
	return rep, release, finished
}

func TestFastWinsWhileSlowGated(t *testing.T) {
	slow, release, _ := newGated("slow")

	val, err := hedge.Read(context.Background(), slow, fast("fast"))
	if err != nil {
		t.Fatalf("Read returned error: %v", err)
	}
	if val != "fast" {
		t.Fatalf("val = %q, want %q (fast replica must win)", val, "fast")
	}

	// Release the loser so its buffered send completes and its goroutine exits
	// before TestMain's leak check.
	close(release)
}

func TestResultCorrectness(t *testing.T) {
	val, err := hedge.Read(context.Background(), fast("payload-42"))
	if err != nil {
		t.Fatalf("Read returned error: %v", err)
	}
	if val != "payload-42" {
		t.Fatalf("val = %q, want %q", val, "payload-42")
	}
}

func TestPartialFailureReturnsSuccess(t *testing.T) {
	down := func(context.Context) (string, error) {
		return "", errors.New("replica-A down")
	}
	val, err := hedge.Read(context.Background(), down, fast("replica-B"))
	if err != nil {
		t.Fatalf("Read returned error: %v", err)
	}
	if val != "replica-B" {
		t.Fatalf("val = %q, want %q (should skip the failing replica)", val, "replica-B")
	}
}

func TestAllFailJoinsEveryError(t *testing.T) {
	errA := errors.New("replica-A down")
	errB := errors.New("replica-B down")
	repA := func(context.Context) (string, error) { return "", errA }
	repB := func(context.Context) (string, error) { return "", errB }

	val, err := hedge.Read(context.Background(), repA, repB)
	if val != "" {
		t.Fatalf("val = %q, want empty on all-fail", val)
	}
	if !errors.Is(err, errA) {
		t.Fatalf("joined error %v does not wrap errA", err)
	}
	if !errors.Is(err, errB) {
		t.Fatalf("joined error %v does not wrap errB", err)
	}
}

// TestCapacityOneIsLoadBearing releases the losing replica strictly after Read
// has already returned, then proves its send completed and its goroutine exited.
// With a capacity-1 reply channel the send lands in the buffer with no receiver;
// with an unbuffered channel it would block forever and goleak.VerifyNone would
// report the stranded goroutine.
func TestCapacityOneIsLoadBearing(t *testing.T) {
	slow, release, finished := newGated("slow-loser")

	val, err := hedge.Read(context.Background(), fast("winner"), slow)
	if err != nil || val != "winner" {
		t.Fatalf("Read = (%q, %v), want (%q, nil)", val, err, "winner")
	}

	// Read has returned. Now let the loser run: it returns, then the launched
	// goroutine performs its single buffered send and exits.
	close(release)
	<-finished
	goleak.VerifyNone(t)
}

func TestNoReplicas(t *testing.T) {
	if _, err := hedge.Read(context.Background()); err == nil {
		t.Fatal("Read with no replicas should return an error")
	}
}
```

## Review

Read is correct when it returns the first successful reply and, no matter which
replicas are still in flight, leaves zero goroutines behind. The guarantee comes
from one design choice: each replica owns a capacity-1 reply channel, so its
single send always has a free buffer slot and completes without a receiver. That
decouples the loser's send from Read's departure — Read cancels the shared
context and returns the winner, and the abandoned replicas finish, send into
their own buffers, and exit. `TestCapacityOneIsLoadBearing` releases a loser only
after Read returned and then asserts, via `goleak`, that its goroutine drained;
an unbuffered channel would fail that assertion because the send would block
forever. That is the exact production bug the pattern prevents: a hedged or
timeout-racing read that leaks one goroutine per abandoned backend, invisible in
tests that never check for it and fatal to a long-running service.

## Resources

- [Go Memory Model](https://go.dev/ref/mem) -- why a buffered send with a free slot completes without a matching receive, which is what lets the loser exit.
- [`context.WithCancelCause`](https://pkg.go.dev/context#WithCancelCause) -- canceling the shared context with a cause once a replica has answered.
- [`errors.Join`](https://pkg.go.dev/errors#Join) -- combining every replica's failure into one error when all of them fail.
- [`reflect.Select`](https://pkg.go.dev/reflect#Select) -- waiting on a dynamic number of reply channels at once for the first response.

---

Back to [00-concepts.md](00-concepts.md) | Prev: [11-prefetched-key-broadcast-future.md](11-prefetched-key-broadcast-future.md) | Next: [13-two-phase-shard-commit-barrier.md](13-two-phase-shard-commit-barrier.md)
