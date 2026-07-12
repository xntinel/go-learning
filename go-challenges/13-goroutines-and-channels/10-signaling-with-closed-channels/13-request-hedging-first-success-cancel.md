# Exercise 13: Request Hedging: First Success Closes Done And Cancels The Losers

**Level: Advanced**

To cut tail latency, a read-heavy client fans the same request out to several
replicas and takes whichever answers first, then cancels the slower ones so they
stop burning CPU and connections on a result nobody will read. This is the
success-mirror of first-error abort: instead of the first *failure* aborting the
siblings, the first *success* closes a done channel exactly once, that close
broadcasts to every loser, and the winner's value is delivered. The correctness
lives in the edges -- exactly one winner when two succeed at once, a joined error
when all replicas fail, and provably no replica left blocked after the winner is
chosen.

This module is self-contained: its own module, a `hedge` package, a demo, and
tests. Nothing here imports another exercise.

## What you'll build

```text
hedge/                       independent module: example.com/hedge
  go.mod                     go 1.26
  hedge.go                   Race[T]: first success wins, close-once cancels the losers
  cmd/demo/main.go           runnable demo: fastest replica wins, all-fail join, caller cancel
  hedge_test.go              first-success-cancels, simultaneous-success, all-fail-join, caller-cancel, leak-free
```

- Files: `hedge.go`, `cmd/demo/main.go`, `hedge_test.go`.
- Implement: `type Replica[T any] func(ctx context.Context) (T, error)` and `func Race[T any](ctx context.Context, replicas []Replica[T]) (T, error)`.
- Test: the first success determines the value and cancels the losers; two simultaneous successes yield exactly one winner with no double-close; all failures return an `errors.Join`; a caller-cancelled context returns `ctx.Err()`; every replica goroutine has exited before `Race` returns (goleak).
- Verify: `go test -count=1 -race ./...`

Set up the module:

```bash
go get go.uber.org/goleak
go mod tidy
```

### The done-broadcast that cancels the losers

Hedging inverts the first-error pattern but reuses the same primitive: a single
`close` that fans out to every waiter at once. Here the broadcast is the derived
context's `Done()` channel, and the "close" is a call to its `cancel`.

The protocol is:

1. Derive `cctx, cancel := context.WithCancel(ctx)` and hand `cctx` to every
   replica. `cctx.Done()` is the latch the losers watch.
2. Launch one goroutine per replica. On a replica's first success, a `sync.Once`
   records the value, calls `cancel()`, and closes a private `won` channel. The
   `cancel()` closes `cctx.Done()`, which every loser observes in its own
   `select`; the `won` close wakes the `Race` goroutine.
3. `sync.Once` is load-bearing, not decorative. Two replicas can succeed in the
   same instant; a bare `cancel(); close(won)` in each would double-close `won`
   and panic. `Once.Do` collapses any number of simultaneous winners into exactly
   one record and one broadcast.
4. The last replica to finish (win or fail) closes an `allDone` channel via an
   atomic counter. That is how `Race` learns "every replica failed" without a
   monitor goroutine that would itself need joining.

The `Race` goroutine wakes on whichever fact lands first -- a winner (`won`), the
caller's cancellation (`ctx.Done()`), or every replica finishing (`allDone`) --
then blocks on `wg.Wait()`. That wait is the termination contract: it cannot
return until every replica goroutine has run its `defer wg.Done()`, so no replica
outlives `Race`. Only after the wait does `Race` resolve by priority: a winner
outranks everything, because `won` may close in the same instant as `allDone`.

Crucially, the losers never write to a channel nobody reads. A loser's only job is
to notice `cctx.Done()` and return; its error lands in a pre-sized slice at its
own index, never on a channel. That is why there is no "send on a full channel
after the winner left" leak -- the losers have nothing to send.

Create `hedge.go`:

```go
package hedge

import (
	"context"
	"errors"
	"sync"
	"sync/atomic"
)

// Replica is one candidate that can answer the request. It must honour ctx: once
// ctx is cancelled (a sibling won, or the caller aborted) it must return promptly.
type Replica[T any] func(ctx context.Context) (T, error)

// Race launches every replica against a derived context, returns the first
// successful result, and cancels the rest via a single close-once broadcast (the
// derived context's Done channel). If every replica fails it returns
// errors.Join of all their errors. If ctx is cancelled first it returns the zero
// value and ctx.Err(). Every launched replica goroutine is guaranteed to have
// exited before Race returns.
func Race[T any](ctx context.Context, replicas []Replica[T]) (T, error) {
	var zero T
	if len(replicas) == 0 {
		return zero, errors.New("hedge: no replicas")
	}

	// cctx.Done() is the broadcast latch. cancel() closes it exactly once no
	// matter how many callers race into it, so it is the safe fan-out signal.
	cctx, cancel := context.WithCancel(ctx)
	defer cancel()

	var (
		wg      sync.WaitGroup
		once    sync.Once
		result  T
		won     = make(chan struct{}) // closed by the single winner
		allDone = make(chan struct{}) // closed by the last replica to finish
		errs    = make([]error, len(replicas))
		left    atomic.Int64
	)
	left.Store(int64(len(replicas)))

	for i, r := range replicas {
		wg.Add(1)
		go func() {
			defer wg.Done()
			// The last replica to finish closes allDone exactly once; that is how
			// Race learns "every replica failed" without a monitor goroutine.
			defer func() {
				if left.Add(-1) == 0 {
					close(allDone)
				}
			}()

			v, err := r(cctx)
			if err != nil {
				errs[i] = err // distinct index per goroutine: no shared write
				return
			}
			// First success wins. sync.Once makes the record-and-broadcast happen
			// exactly once even when two replicas succeed in the same instant.
			once.Do(func() {
				result = v
				cancel()   // broadcast: closes cctx.Done(); losers observe and exit
				close(won) // wake the main goroutine
			})
		}()
	}

	// Wake on whichever fact lands first: a winner, the caller's cancellation, or
	// every replica having finished.
	select {
	case <-won:
	case <-ctx.Done():
	case <-allDone:
	}

	// Termination contract: no replica goroutine is running past this point.
	wg.Wait()

	// Resolve by priority. A winner outranks everything else, because won may be
	// closed in the same instant allDone or ctx.Done fires.
	select {
	case <-won:
		return result, nil
	default:
	}
	if err := ctx.Err(); err != nil {
		return zero, err
	}
	return zero, errors.Join(errs...)
}
```

### The runnable demo

The demo runs three fixed scenarios in order, so its output is deterministic: no
timers, no map iteration, and every print happens after `Race` has joined all
goroutines.

Create `cmd/demo/main.go`:

```go
package main

import (
	"context"
	"errors"
	"fmt"
	"sync/atomic"

	"example.com/hedge"
)

func main() {
	// Scenario 1: one replica answers immediately, two block until cancelled.
	// The fast one wins; the losers observe the cancelled context and exit.
	var cancelled atomic.Int64
	slow := func(ctx context.Context) (string, error) {
		<-ctx.Done() // wait for the winner's broadcast
		cancelled.Add(1)
		return "", ctx.Err()
	}
	replicas := []hedge.Replica[string]{
		slow,
		func(ctx context.Context) (string, error) { return "replica-2 body", nil },
		slow,
	}
	val, err := hedge.Race(context.Background(), replicas)
	fmt.Printf("winner value: %q\n", val)
	fmt.Printf("winner error: %v\n", err)
	fmt.Printf("losers that observed cancellation: %d\n", cancelled.Load())

	// Scenario 2: every replica fails; Race returns errors.Join of all of them.
	failing := []hedge.Replica[string]{
		func(context.Context) (string, error) { return "", errors.New("replica-0: 503") },
		func(context.Context) (string, error) { return "", errors.New("replica-1: timeout") },
	}
	_, jerr := hedge.Race(context.Background(), failing)
	fmt.Printf("all-fail error: %v\n", jerr)

	// Scenario 3: the caller cancels before anyone succeeds.
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	_, cerr := hedge.Race(ctx, []hedge.Replica[string]{slow, slow})
	fmt.Printf("caller-cancelled error: %v\n", cerr)
}
```

Run it:

```bash
go run ./cmd/demo
```

Expected output:

```
winner value: "replica-2 body"
winner error: <nil>
losers that observed cancellation: 2
all-fail error: replica-0: 503
replica-1: timeout
caller-cancelled error: context canceled
```

### Tests

`TestFirstSuccessWinsAndCancelsLosers` is the core: the winner's value is returned
and, because `Race` joins every goroutine, all three losers have provably observed
the cancellation by the time it returns -- no sleep, no timing margin.
`TestSimultaneousSuccessSingleWinner` fires 64 replicas that all succeed at once;
`sync.Once` must yield exactly one winner and no double-close panic, which `-race`
enforces. `TestAllFailJoined` checks the returned error contains every replica's
error via `errors.Is`. `TestCallerCancelAborts` cancels the context up front and
expects `context.Canceled`. `TestWinnerBeatsSlowLoser` proves the loser exits by
cancellation rather than by finishing its own never-completing work. `TestMain`
wires `goleak` so any leaked replica fails the run.

Create `hedge_test.go`:

```go
package hedge

import (
	"context"
	"errors"
	"strings"
	"sync/atomic"
	"testing"

	"go.uber.org/goleak"
)

func TestMain(m *testing.M) {
	// Proves no replica goroutine outlives Race: goleak fails the run if any
	// goroutine is still alive after the tests finish.
	goleak.VerifyTestMain(m)
}

// TestFirstSuccessWinsAndCancelsLosers pins the core property: the successful
// replica's value is returned, and its broadcast cancels the losers so they exit
// via the context rather than by running to completion. No sleep in the library
// proves the cancel; the losers block until ctx.Done and count that they saw it.
func TestFirstSuccessWinsAndCancelsLosers(t *testing.T) {
	t.Parallel()

	var sawCancel atomic.Int64
	loser := func(ctx context.Context) (string, error) {
		<-ctx.Done() // would block forever without the winner's broadcast
		sawCancel.Add(1)
		return "", ctx.Err()
	}
	replicas := []Replica[string]{
		loser,
		func(context.Context) (string, error) { return "the-winner", nil },
		loser,
		loser,
	}

	val, err := Race(context.Background(), replicas)
	if err != nil {
		t.Fatalf("Race err = %v, want nil", err)
	}
	if val != "the-winner" {
		t.Fatalf("Race val = %q, want %q", val, "the-winner")
	}
	// Race waits for every goroutine, so by the time it returns all three losers
	// must have observed the cancellation. No timing margin is needed.
	if got := sawCancel.Load(); got != 3 {
		t.Fatalf("losers that observed cancel = %d, want 3", got)
	}
}

// TestSimultaneousSuccessSingleWinner fires many replicas that all succeed at
// once. sync.Once must record exactly one winner with no double-close panic, and
// the returned value must be one of the produced values. Run under -race.
func TestSimultaneousSuccessSingleWinner(t *testing.T) {
	t.Parallel()

	const n = 64
	replicas := make([]Replica[int], n)
	for i := range n {
		replicas[i] = func(context.Context) (int, error) { return i, nil }
	}

	val, err := Race(context.Background(), replicas)
	if err != nil {
		t.Fatalf("Race err = %v, want nil", err)
	}
	if val < 0 || val >= n {
		t.Fatalf("Race val = %d, want a value in [0,%d)", val, n)
	}
}

// TestAllFailJoined pins the all-fail edge: the returned error is an
// errors.Join that contains every replica's error.
func TestAllFailJoined(t *testing.T) {
	t.Parallel()

	errA := errors.New("replica-a down")
	errB := errors.New("replica-b down")
	errC := errors.New("replica-c down")
	replicas := []Replica[string]{
		func(context.Context) (string, error) { return "", errA },
		func(context.Context) (string, error) { return "", errB },
		func(context.Context) (string, error) { return "", errC },
	}

	val, err := Race(context.Background(), replicas)
	if val != "" {
		t.Fatalf("Race val = %q, want zero value", val)
	}
	for _, want := range []error{errA, errB, errC} {
		if !errors.Is(err, want) {
			t.Fatalf("Race err = %v, want it to contain %v", err, want)
		}
	}
}

// TestCallerCancelAborts pins the caller-cancellation edge: a context cancelled
// before anyone succeeds returns ctx.Err and every replica exits.
func TestCallerCancelAborts(t *testing.T) {
	t.Parallel()

	ctx, cancel := context.WithCancel(context.Background())
	blocker := func(ctx context.Context) (string, error) {
		<-ctx.Done()
		return "", ctx.Err()
	}
	replicas := []Replica[string]{blocker, blocker, blocker}

	// Cancel after launch so the replicas are already parked on ctx.Done.
	cancel()
	val, err := Race(ctx, replicas)
	if val != "" {
		t.Fatalf("Race val = %q, want zero value", val)
	}
	if !errors.Is(err, context.Canceled) {
		t.Fatalf("Race err = %v, want context.Canceled", err)
	}
}

// TestWinnerBeatsSlowLoser proves the winner returns before a slow loser would
// naturally finish: the loser only returns once cancelled, and it records via a
// flag whether it exited by cancellation or by completing its own (never-taken)
// path. A generous timing margin is unnecessary because the loser has no timer.
func TestWinnerBeatsSlowLoser(t *testing.T) {
	t.Parallel()

	var loserFinishedNaturally atomic.Bool
	loser := func(ctx context.Context) (string, error) {
		select {
		case <-ctx.Done():
			return "", ctx.Err() // cancelled by the winner: the intended path
		case <-neverFires():
			loserFinishedNaturally.Store(true)
			return "slow", nil
		}
	}
	replicas := []Replica[string]{
		func(context.Context) (string, error) { return "fast", nil },
		loser,
	}

	val, err := Race(context.Background(), replicas)
	if err != nil || val != "fast" {
		t.Fatalf("Race = (%q, %v), want (fast, nil)", val, err)
	}
	if loserFinishedNaturally.Load() {
		t.Fatal("loser completed its own work; the winner failed to cancel it")
	}
}

// neverFires returns a channel that is never ready, standing in for arbitrarily
// slow replica work without any real delay.
func neverFires() <-chan struct{} { return make(chan struct{}) }

func TestNoReplicas(t *testing.T) {
	t.Parallel()
	_, err := Race[int](context.Background(), nil)
	if err == nil || !strings.Contains(err.Error(), "no replicas") {
		t.Fatalf("Race err = %v, want a no-replicas error", err)
	}
}
```

## Review

`Race` is correct when the first successful replica's value comes back and its
close-once broadcast provably cancels every loser before `Race` returns. The
`sync.Once` around `cancel(); close(won)` is what guarantees exactly one winner
under simultaneous success -- without it, two racing successes double-close and
panic, which `TestSimultaneousSuccessSingleWinner` under `-race` would catch. The
`WaitGroup` is the termination contract: `wg.Wait()` cannot return until every
replica ran its `defer wg.Done()`, so `TestFirstSuccessWinsAndCancelsLosers` can
assert all losers observed cancellation with zero timing slack, and `goleak`
confirms none leaked. Because losers only *read* `cctx.Done()` and write their
error to a private slice index, there is no channel a loser could block writing to
after the winner left -- the exact production bug (a hedged request goroutine
wedged on a send to an abandoned result channel) this shape prevents. In
production, reach for `golang.org/x/sync/errgroup` with a cancellable context,
which is this pattern with cancellation and error capture already wired.

## Resources

- [pkg.go.dev: context.WithCancel](https://pkg.go.dev/context#WithCancel) -- the derived-context cancel whose Done channel is the loser broadcast.
- [pkg.go.dev: sync.Once](https://pkg.go.dev/sync#Once) -- the single-winner guard under simultaneous success.
- [pkg.go.dev: errors.Join](https://pkg.go.dev/errors#Join) -- combining every replica error when all fail.
- [pkg.go.dev: golang.org/x/sync/errgroup](https://pkg.go.dev/golang.org/x/sync/errgroup) -- the production version of this fan-out with cancellation and error capture wired in.

---

Back to [00-concepts.md](00-concepts.md) | Prev: [12-singleflight-stampede-completion-barrier.md](12-singleflight-stampede-completion-barrier.md) | Next: [../11-goroutine-lifecycle-management/00-concepts.md](../11-goroutine-lifecycle-management/00-concepts.md)
