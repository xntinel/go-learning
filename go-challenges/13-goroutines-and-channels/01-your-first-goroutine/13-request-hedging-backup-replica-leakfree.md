# Exercise 13: Request Hedging: Race A Backup Replica, Return The Winner, Leave No Goroutine Behind

**Level: Advanced**

Tail latency on a read path is often dominated by one unlucky replica that stalls
on GC, a slow disk, or a saturated NIC. Hedging cuts that tail: issue the read to
the primary, and if it has not answered within a small delay, fire a duplicate at
a backup and take whichever answers first. The naive version leaks -- the losing
in-flight call is a launched goroutine, and if you return the winner without
cancelling and joining the loser, every slow request abandons a goroutine (and
the connection it holds) that outlives the request. This exercise builds `Hedge`,
which returns the winner and then cancels and JOINS the loser before returning.

This module is self-contained: its own module, a `hedge` package, a demo, and
tests. Nothing here imports another exercise.

## What you'll build

```text
hedge/                       independent module: example.com/hedge
  go.mod                     go 1.26
  hedge.go                   Result[V]; Hedge(ctx, delay, replicas, call) Result[V]
  cmd/demo/main.go           runnable demo: primary-wins and backup-wins scenarios
  hedge_test.go              backup-never-launched, backup-wins, no-leak, one-result, error surfaced
```

- Files: `hedge.go`, `cmd/demo/main.go`, `hedge_test.go`.
- Implement: `Hedge[V any](ctx context.Context, delay time.Duration, replicas []int, call func(ctx context.Context, replica int) (V, error)) Result[V]` -- fire the primary immediately, hedge the backup after `delay`, return the first result, then cancel and join the loser.
- Test: backup never launches when the primary is fast; the backup wins when the primary stalls; the goroutine baseline is restored; exactly one result is returned and the loser mutates nothing after cancel; the winner's error is surfaced.
- Verify: `go test -count=1 -race ./...`

Set up the module:

```bash
mkdir -p go-solutions/13-goroutines-and-channels/01-your-first-goroutine/13-request-hedging-backup-replica-leakfree/cmd/demo
cd go-solutions/13-goroutines-and-channels/01-your-first-goroutine/13-request-hedging-backup-replica-leakfree
go get go.uber.org/goleak
```

### Why the loser must be cancelled AND joined

A hedged request launches up to two goroutines but returns exactly one answer.
The goroutine whose result you discard is not free to ignore: it is still running
`call`, still holding whatever that call acquired (a pooled connection, a
server-side cursor, a rate-limit slot). If `Hedge` returns the winner and lets the
loser run on, then on every request where hedging actually fires you leak a
goroutine until it eventually finishes on its own -- and against a truly stuck
replica, "eventually" is never. That is the exact shape of a goroutine leak that
grows the process one slow request at a time.

The fix has two halves, and you need both:

1. **Cancel.** Run both calls under a context you derive from the caller's with
   `context.WithCancel`. Once the winner is in hand, call `cancel()`. A
   well-behaved `call` selects on `ctx.Done()` and returns promptly with
   `ctx.Err()`, so the loser stops instead of running to completion. Cancellation
   is what makes the loser's exit *prompt*.

2. **Join.** Cancellation only *requests* a stop; it does not prove one happened.
   Before `Hedge` returns you must `wg.Wait()` on both launched goroutines so
   their termination *happens-before* your return. Join is what makes leak-freedom
   an actual guarantee rather than a hope.

One more detail makes the join safe: the internal results channel is buffered to
capacity 2. The winner sends and is received; the loser, after it observes the
cancel and returns, still executes its `results <- ...` send. If that channel had
no room, the loser's send would block forever and `wg.Wait()` would deadlock. A
buffer of two lets the loser deposit its discarded result and exit cleanly, which
is precisely what lets the join complete. This is the "buffer the results channel
to the number of senders" rule from the concepts file, applied where it is
load-bearing.

The timer is what sequences the two launches. The primary fires immediately. A
`select` then races the primary's result against a `time.NewTimer(delay)`: if the
primary answers first, the timer branch never runs and the backup is never
launched at all; if the timer fires first, the backup is hedged and `Hedge` takes
whichever replica answers next. With `delay` set to an hour the primary always
wins the select; with `delay == 0` the timer branch is taken whenever the primary
has not already answered, which is how the tests force each ordering
deterministically without sleeping.

Create `hedge.go`:

```go
package hedge

import (
	"context"
	"sync"
	"time"
)

// Result carries the winning replica's answer. Replica is the index into the
// replicas slice that produced Value (or Err).
type Result[V any] struct {
	Value   V
	Replica int
	Err     error
}

// Hedge issues a read to the primary replica (replicas[0]) immediately under a
// derived, cancellable context. If the primary has not answered within delay,
// Hedge launches a hedged duplicate to the backup replica (replicas[1]). It
// returns the first result to arrive, then cancels the derived context and JOINS
// the losing goroutine before returning, so no goroutine outlives the call.
//
// The internal results channel is buffered to cap 2 so the loser can deposit its
// (discarded) result and exit without blocking, even though nobody receives it.
// call must honor ctx.Done() for the loser's cancellation to be prompt.
//
// Requires len(replicas) >= 2.
func Hedge[V any](ctx context.Context, delay time.Duration, replicas []int, call func(ctx context.Context, replica int) (V, error)) Result[V] {
	if len(replicas) < 2 {
		panic("hedge: requires at least 2 replicas")
	}

	dctx, cancel := context.WithCancel(ctx)
	defer cancel()

	// Buffered to 2: both the winner and the abandoned loser can send and exit
	// even though only one value is ever received.
	results := make(chan Result[V], 2)
	var wg sync.WaitGroup

	launch := func(replica int) {
		wg.Go(func() {
			v, err := call(dctx, replica)
			results <- Result[V]{Value: v, Replica: replica, Err: err}
		})
	}

	launch(replicas[0]) // primary fires immediately

	timer := time.NewTimer(delay)
	defer timer.Stop()

	var winner Result[V]
	select {
	case winner = <-results:
		// Primary answered before the hedge delay elapsed: the backup is never
		// launched at all.
	case <-timer.C:
		// Primary is too slow: fire the hedged duplicate and take whichever
		// replica answers first.
		launch(replicas[1])
		winner = <-results
	}

	cancel()  // signal the loser (if any) to abandon its in-flight call
	wg.Wait() // JOIN every launched goroutine before returning: no leak
	return winner
}
```

### The runnable demo

The demo runs both orderings back to back. Scenario A gives the primary an
hour-long hedge delay and a fast reply, so it wins and the backup never launches.
Scenario B stalls the primary and uses a zero delay, so the backup is hedged and
wins while the stalled primary is cancelled and joined. Both outcomes are fully
determined, so the output never varies.

Create `cmd/demo/main.go`:

```go
package main

import (
	"context"
	"fmt"
	"time"

	"example.com/hedge"
)

func main() {
	replicas := []int{0, 1}

	// Scenario A: the primary answers within the hedge delay, so the backup is
	// never launched. The hour-long delay guarantees the primary's fast reply
	// always wins the select before the timer could fire.
	a := hedge.Hedge(context.Background(), time.Hour, replicas,
		func(ctx context.Context, replica int) (string, error) {
			if replica == 0 {
				return "primary-value", nil
			}
			<-ctx.Done() // backup, if it ever ran, would abandon on cancel
			return "", ctx.Err()
		})
	fmt.Printf("scenario A: winner=replica-%d value=%q\n", a.Replica, a.Value)

	// Scenario B: the primary stalls, so after a zero delay the backup is hedged
	// and wins. The stalled primary is cancelled and joined before Hedge returns.
	b := hedge.Hedge(context.Background(), 0, replicas,
		func(ctx context.Context, replica int) (string, error) {
			if replica == 1 {
				return "backup-value", nil
			}
			<-ctx.Done() // primary stalls until the winner cancels it
			return "", ctx.Err()
		})
	fmt.Printf("scenario B: winner=replica-%d value=%q\n", b.Replica, b.Value)
}
```

Run it:

```bash
go run ./cmd/demo
```

Expected output:

```
scenario A: winner=replica-0 value="primary-value"
scenario B: winner=replica-1 value="backup-value"
```

### Tests

`TestMain` installs `goleak.VerifyTestMain`, which fails the run if any test
leaves a goroutine behind -- a global proof that `Hedge` never leaks the loser.
`TestPrimaryWinsBackupNeverLaunched` uses an hour-long delay and a released
primary and asserts the atomic call counter is exactly 1, proving the backup was
never launched. `TestBackupWinsPrimaryLoserTerminates` uses `delay == 0` with a
released backup and a primary blocked on `ctx.Done()`, and asserts the backup's
replica wins, both calls were made, and the primary's cancel path ran before
`Hedge` returned. `TestNoLeakAfterReturn` samples `runtime.NumGoroutine` and
asserts the baseline is restored after `Hedge` returns -- cancelled and joined,
not leaked. `TestExactlyOneResultLoserDoesNotMutate` asserts exactly one success
is recorded and it matches the winner, so the cancelled loser mutated nothing.
`TestWinnerErrorSurfaced` asserts an error from the winning replica reaches
`Result.Err`.

Create `hedge_test.go`:

```go
package hedge

import (
	"context"
	"errors"
	"runtime"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"go.uber.org/goleak"
)

// TestMain proves globally that no goroutine started by any test outlives it:
// goleak fails the run if Hedge ever leaks the loser.
func TestMain(m *testing.M) {
	goleak.VerifyTestMain(m)
}

// TestPrimaryWinsBackupNeverLaunched pins property (1): with an effectively
// infinite delay and the primary released through a gate, the backup is never
// launched (exactly one call is made) and the primary's replica wins.
func TestPrimaryWinsBackupNeverLaunched(t *testing.T) {
	t.Parallel()

	replicas := []int{7, 9}
	var calls atomic.Int64
	primaryGate := make(chan struct{})
	close(primaryGate) // primary may return immediately

	call := func(ctx context.Context, replica int) (int, error) {
		calls.Add(1)
		if replica == replicas[0] {
			<-primaryGate
			return replica * 10, nil
		}
		<-ctx.Done() // backup must never reach here
		return 0, ctx.Err()
	}

	res := Hedge(context.Background(), time.Hour, replicas, call)

	if got := calls.Load(); got != 1 {
		t.Fatalf("call count = %d, want 1 (backup must not launch)", got)
	}
	if res.Replica != replicas[0] {
		t.Fatalf("winner replica = %d, want %d (primary)", res.Replica, replicas[0])
	}
	if res.Value != 70 {
		t.Fatalf("value = %d, want 70", res.Value)
	}
	if res.Err != nil {
		t.Fatalf("err = %v, want nil", res.Err)
	}
}

// TestBackupWinsPrimaryLoserTerminates pins property (2): with delay==0 both
// replicas are launched, the released backup wins, and the primary loser --
// blocked in call on ctx.Done() -- is cancelled and terminates.
func TestBackupWinsPrimaryLoserTerminates(t *testing.T) {
	t.Parallel()

	replicas := []int{3, 5}
	var calls atomic.Int64
	backupGate := make(chan struct{})
	close(backupGate) // backup may return immediately

	primaryReturned := make(chan struct{})
	call := func(ctx context.Context, replica int) (int, error) {
		calls.Add(1)
		if replica == replicas[1] {
			<-backupGate
			return replica * 10, nil
		}
		<-ctx.Done() // primary stalls until Hedge cancels it
		close(primaryReturned)
		return 0, ctx.Err()
	}

	res := Hedge(context.Background(), 0, replicas, call)

	if res.Replica != replicas[1] {
		t.Fatalf("winner replica = %d, want %d (backup)", res.Replica, replicas[1])
	}
	if res.Value != 50 {
		t.Fatalf("value = %d, want 50", res.Value)
	}
	if got := calls.Load(); got != 2 {
		t.Fatalf("call count = %d, want 2 (both launched)", got)
	}
	// Hedge joins the loser before returning, so its cancel path has run.
	select {
	case <-primaryReturned:
	default:
		t.Fatalf("primary loser did not terminate before Hedge returned")
	}
}

// TestNoLeakAfterReturn pins property (3) at the granularity of a single call:
// the NumGoroutine baseline is restored after Hedge returns, proving the loser
// was cancelled AND joined, not leaked. (goleak in TestMain proves it globally.)
func TestNoLeakAfterReturn(t *testing.T) {
	replicas := []int{1, 2}
	backupGate := make(chan struct{})
	close(backupGate)

	call := func(ctx context.Context, replica int) (int, error) {
		if replica == replicas[1] {
			<-backupGate
			return replica * 10, nil
		}
		<-ctx.Done()
		return 0, ctx.Err()
	}

	base := runtime.NumGoroutine()
	res := Hedge(context.Background(), 0, replicas, call)
	if res.Replica != replicas[1] {
		t.Fatalf("winner replica = %d, want %d", res.Replica, replicas[1])
	}

	for range 2000 {
		if runtime.NumGoroutine() <= base {
			return
		}
		runtime.Gosched()
	}
	t.Fatalf("goroutines did not return to baseline: base=%d now=%d",
		base, runtime.NumGoroutine())
}

// TestExactlyOneResultLoserDoesNotMutate pins property (4): exactly one Result
// is returned, and the loser -- cancelled while blocked -- never records an
// observable success after cancellation.
func TestExactlyOneResultLoserDoesNotMutate(t *testing.T) {
	t.Parallel()

	replicas := []int{4, 8}
	backupGate := make(chan struct{})
	close(backupGate)

	var mu sync.Mutex
	var successes []int
	call := func(ctx context.Context, replica int) (int, error) {
		if replica == replicas[1] {
			<-backupGate
			mu.Lock()
			successes = append(successes, replica) // only the winner records
			mu.Unlock()
			return replica * 10, nil
		}
		<-ctx.Done()
		// Loser observed cancellation: it must NOT record a success.
		return 0, ctx.Err()
	}

	res := Hedge(context.Background(), 0, replicas, call)

	mu.Lock()
	defer mu.Unlock()
	if len(successes) != 1 {
		t.Fatalf("recorded successes = %v, want exactly one", successes)
	}
	if successes[0] != res.Replica {
		t.Fatalf("recorded replica = %d, winner = %d; must match", successes[0], res.Replica)
	}
}

// TestWinnerErrorSurfaced pins property (5): an error returned by the winning
// replica is surfaced in Result.Err.
func TestWinnerErrorSurfaced(t *testing.T) {
	t.Parallel()

	replicas := []int{2, 6}
	wantErr := errors.New("replica down")
	call := func(ctx context.Context, replica int) (int, error) {
		if replica == replicas[0] {
			return 0, wantErr
		}
		<-ctx.Done()
		return 0, ctx.Err()
	}

	res := Hedge(context.Background(), time.Hour, replicas, call)

	if !errors.Is(res.Err, wantErr) {
		t.Fatalf("err = %v, want %v", res.Err, wantErr)
	}
	if res.Replica != replicas[0] {
		t.Fatalf("winner replica = %d, want %d", res.Replica, replicas[0])
	}
}
```

## Review

`Hedge` is correct when it returns exactly one result -- the first replica to
answer -- and leaves no goroutine behind under either ordering. The
leak-freedom guarantee rests on three cooperating pieces: the derived cancellable
context whose `cancel()` tells the loser to abandon its in-flight `call`; the
`sync.WaitGroup` whose `Wait()` establishes that both launched goroutines have
actually terminated *before* `Hedge` returns; and the cap-2 results channel that
lets the discarded loser deposit its result and exit instead of blocking forever
on a full channel and deadlocking the join. The tests make each ordering
deterministic with gates rather than sleeps -- an hour-long delay forces the
primary to win the select, a zero delay forces the timer branch when the primary
stalls -- so `-count=2 -race` exercises the real cancel-versus-answer race without
flakiness, and `goleak` in `TestMain` turns "no leak" from a claim into a checked
invariant. The production bug this prevents is the hedged-read goroutine leak:
returning the fast answer while the slow replica's call runs on unattended, one
stuck connection per unlucky request, until the pool is drained and the service
degrades.

## Resources

- [context package](https://pkg.go.dev/context) -- `WithCancel` and the `ctx.Done()` contract that make the loser's cancellation prompt.
- [sync.WaitGroup.Go](https://pkg.go.dev/sync#WaitGroup.Go) -- the Go 1.25 idiom that fuses `Add`, `go`, and `Done` so the join cannot be miscounted.
- [time.NewTimer](https://pkg.go.dev/time#NewTimer) -- the timer whose fire sequences the hedged launch after `delay` elapses.
- [goleak](https://pkg.go.dev/go.uber.org/goleak) -- goroutine-leak detector used in `TestMain` to prove no goroutine outlives a test.

---

Back to [00-concepts.md](00-concepts.md) | Prev: [12-singleflight-cache-stampede-single-launch.md](12-singleflight-cache-stampede-single-launch.md) | Next: [14-lease-keeper-renew-cancel-cause.md](14-lease-keeper-renew-cancel-cause.md)
