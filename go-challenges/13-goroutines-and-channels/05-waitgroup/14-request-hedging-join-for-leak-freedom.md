# Exercise 14: Request Hedging With WaitGroup Join For Leak-Freedom

**Level: Advanced**

Tail latency on a read path is often dominated by one slow replica. The backup-request
(hedging) pattern fights it: send the read to a primary replica immediately and, if it
has not answered within a small delay, fire a second copy at a backup replica; the
first success wins and the slow one is cancelled. The naive version leaks — it returns
the winner and walks away while the loser's goroutine keeps running against a live
context. This module builds a `Hedge` that joins *both* goroutines with a `WaitGroup`
before returning, so no in-flight request ever outlives the call, and that `Wait` is
what publishes the winning value safely.

This module is self-contained: its own module, a `hedge` package, a demo, and tests.
Nothing here imports another exercise.

## What you'll build

```text
hedge/                       independent module: example.com/hedge
  go.mod                     go 1.26
  hedge.go                   Hedge: primary now, backup after delay, first success wins
  cmd/demo/main.go           runnable demo: primary-wins, hedge-wins, both-fail
  hedge_test.go              winner selection, loser cancellation cause, leak-freedom
```

- Files: `hedge.go`, `cmd/demo/main.go`, `hedge_test.go`.
- Implement: `type Query func(ctx context.Context, replica int) (string, error)` and `func Hedge(ctx context.Context, delay time.Duration, query Query) (string, error)`.
- Test: the first success is returned; the loser observes `context.Cause == errHedgeWon`; the in-flight gauge is zero after return (goleak + an atomic counter); both-fail returns `errors.Join` of the two errors.
- Verify: `go test -count=1 -race ./...`

Set up the module:

```bash
mkdir -p go-solutions/13-goroutines-and-channels/05-waitgroup/14-request-hedging-join-for-leak-freedom/cmd/demo
cd go-solutions/13-goroutines-and-channels/05-waitgroup/14-request-hedging-join-for-leak-freedom
go mod edit -go=1.26
go get go.uber.org/goleak
go mod tidy
```

### Join both goroutines before you return, then read the winner

The subtle bug in hedging is not choosing the winner — it is what happens to the loser.
If `Hedge` receives the first success and returns immediately, the losing goroutine is
still blocked in a network call against a context that is still alive. That goroutine
outlives the request: it holds a connection, a buffer, and a slot in your concurrency
budget, and under load these accumulate into a goroutine leak that looks like a memory
leak. Cancelling the loser is necessary but not sufficient — cancellation is a request
to stop, not a guarantee that it *has* stopped. The guarantee comes from the join.

The protocol is four steps:

1. Derive a cancelable context with `context.WithCancelCause(ctx)`. The cause is how the
   loser learns *why* it was cancelled — not a generic `context.Canceled`, but the
   sentinel `errHedgeWon`, which a caller can distinguish from a real timeout.
2. Launch two goroutines under one `WaitGroup`, each with `defer wg.Done()`. The primary
   runs its query immediately; the backup arms a `time.NewTimer(delay)` and `select`s
   between the timer firing (issue the query) and `ctx.Done()` (the primary already won,
   so never issue). Each goroutine writes its own `results[replica]` slot — disjoint
   indices, no data race.
3. A single dedicated waiter goroutine runs `wg.Wait(); close(done)`, where `done` is the
   channel a successful query sends its replica index on. Closing from one waiter, after
   the join, is the canonical "close a shared channel exactly once" idiom: no producer
   ever races the close.
4. The caller `range`s over `done`. The first index received is the winner; that success
   triggers `cancel(errHedgeWon)`, which unblocks the loser. The range ends only when
   `done` is closed — i.e. after `wg.Wait()` returned — so by the time the loop exits,
   both goroutines have finished and every `results[i]` write is visible without a lock.

That last point is the WaitGroup memory guarantee doing real work: reading
`results[winner]` after the range is safe precisely because the `Wait` inside the waiter
happens-before the `close`, which happens-before the range receiving the close. The
`Wait` publishes the winning value.

Create `hedge.go`:

```go
// Package hedge implements the backup-request pattern: a read goes to a primary
// replica immediately and, if it is slow, a hedged copy goes to a backup after a
// delay. The first success wins; the loser is cancelled; both goroutines are always
// joined before Hedge returns, so no in-flight request outlives the call.
package hedge

import (
	"context"
	"errors"
	"sync"
	"time"
)

// errHedgeWon is the cancellation cause attached to the loser's context once the
// other replica has produced the winning result. A losing query observes it via
// context.Cause(ctx).
var errHedgeWon = errors.New("hedge: peer replica already answered")

// Query issues one read against the given replica. It must honor ctx: when ctx is
// cancelled it should abandon its work and return promptly.
type Query func(ctx context.Context, replica int) (string, error)

type outcome struct {
	val string
	err error
}

// Hedge issues query to replica 0 immediately and to replica 1 after delay. It
// returns the first successful result and cancels the loser with cause errHedgeWon.
// Both goroutines are always joined before Hedge returns; if both fail it returns
// errors.Join of the two errors.
func Hedge(ctx context.Context, delay time.Duration, query Query) (string, error) {
	ctx, cancel := context.WithCancelCause(ctx)
	defer cancel(nil) // release the context; a prior cancel(cause) keeps its cause

	var (
		results [2]outcome
		wg      sync.WaitGroup
	)
	// done carries the replica index of a SUCCESSFUL query. Buffered to 2 so a
	// goroutine never blocks on send after the reader has stopped consuming.
	done := make(chan int, 2)

	issue := func(replica int) {
		val, err := query(ctx, replica)
		results[replica] = outcome{val, err}
		if err == nil {
			done <- replica
		}
	}

	wg.Add(2)
	// Primary: issued immediately.
	go func() {
		defer wg.Done()
		issue(0)
	}()
	// Backup: issued only after delay, and never if the call is cancelled first.
	go func() {
		defer wg.Done()
		timer := time.NewTimer(delay)
		defer timer.Stop()
		select {
		case <-timer.C:
			issue(1)
		case <-ctx.Done():
			results[1] = outcome{err: context.Cause(ctx)}
		}
	}()

	// Single waiter closes done exactly once, only after both goroutines have
	// called Done. That Wait is the happens-before edge that publishes results.
	go func() {
		wg.Wait()
		close(done)
	}()

	winner := -1
	for r := range done {
		if winner == -1 {
			winner = r
			cancel(errHedgeWon) // first success cancels the loser
		}
	}
	// The range ended because done was closed, which happened after wg.Wait()
	// returned, so every results[i] write is now visible without extra locking.
	if winner != -1 {
		return results[winner].val, nil
	}
	return "", errors.Join(results[0].err, results[1].err)
}
```

### The runnable demo

The demo drives all three outcomes with queries whose behavior is fixed, not timed, so
the output is deterministic. Scenario 1 gives the primary an immediate answer and a
one-hour delay, so the hedge never fires. Scenario 2 makes the primary block until it is
cancelled and lets the backup answer at once with a zero delay, so the hedge wins and the
primary observes the cancel cause. Scenario 3 fails both.

Create `cmd/demo/main.go`:

```go
package main

import (
	"context"
	"errors"
	"fmt"
	"time"

	"example.com/hedge"
)

func main() {
	// Scenario 1: the primary answers before the hedge delay, so the backup is
	// never issued. A long delay guarantees the hedge does not fire.
	primaryWins := func(ctx context.Context, replica int) (string, error) {
		if replica == 0 {
			return "primary-value", nil
		}
		<-ctx.Done()
		return "", context.Cause(ctx)
	}
	val, err := hedge.Hedge(context.Background(), time.Hour, primaryWins)
	fmt.Printf("primary-wins:  val=%q err=%v\n", val, err)

	// Scenario 2: the primary stalls until cancelled; the backup, issued after a
	// zero delay, answers first and wins. The loser observes the cancel cause.
	var loserCause error
	hedgeWins := func(ctx context.Context, replica int) (string, error) {
		if replica == 1 {
			return "backup-value", nil
		}
		<-ctx.Done()
		loserCause = context.Cause(ctx)
		return "", loserCause
	}
	val, err = hedge.Hedge(context.Background(), 0, hedgeWins)
	fmt.Printf("hedge-wins:    val=%q err=%v\n", val, err)
	fmt.Printf("loser-cause:   %v\n", loserCause)

	// Scenario 3: both replicas fail with distinct errors; Hedge joins the two.
	errPrimary := errors.New("primary refused")
	errBackup := errors.New("backup refused")
	bothFail := func(ctx context.Context, replica int) (string, error) {
		if replica == 0 {
			return "", errPrimary
		}
		return "", errBackup
	}
	val, err = hedge.Hedge(context.Background(), 0, bothFail)
	fmt.Printf("both-fail:     val=%q err=%v\n", val, err)
	fmt.Printf("both-fail Is:  primary=%t backup=%t\n",
		errors.Is(err, errPrimary), errors.Is(err, errBackup))
}
```

Run it:

```bash
go run ./cmd/demo
```

Expected output:

```
primary-wins:  val="primary-value" err=<nil>
hedge-wins:    val="backup-value" err=<nil>
loser-cause:   hedge: peer replica already answered
both-fail:     val="" err=primary refused
backup refused
both-fail Is:  primary=true backup=true
```

(The blank-looking line break after `primary refused` is `errors.Join` rendering its two
errors one per line.)

### Tests

Determinism comes from channel/context-gated queries, never from sleeps. `TestPrimaryWins`
gives the primary an immediate value and a one-hour delay, so the backup is never issued;
it asserts the returned value, that the in-flight atomic gauge reads zero after `Hedge`
returns, and that the backup replica was touched zero times. `TestHedgeWins` makes the
primary block on `ctx.Done()` and lets the backup succeed under a zero delay, so the
backup deterministically wins; it asserts the winning value, that the loser's observed
`context.Cause` is `errHedgeWon`, and that the gauge is zero. `TestBothFail` fails both
replicas with distinct sentinel errors and asserts `errors.Is` matches each in the joined
result. `TestMain` wraps every test in `goleak.VerifyTestMain`, so any goroutine that
outlived a call fails the suite — the leak-freedom proof that complements the gauge.

Create `hedge_test.go`:

```go
package hedge

import (
	"context"
	"errors"
	"sync/atomic"
	"testing"
	"time"

	"go.uber.org/goleak"
)

func TestMain(m *testing.M) {
	goleak.VerifyTestMain(m)
}

// TestPrimaryWins: the primary answers before the hedge delay, so the backup is
// never issued. The delay is time.Hour, so determinism comes from the query, not a
// sleep. After Hedge returns the in-flight gauge is zero (no request outlived the
// call) and the backup replica was never touched.
func TestPrimaryWins(t *testing.T) {
	t.Parallel()

	var inFlight, backupIssued atomic.Int64
	query := func(ctx context.Context, replica int) (string, error) {
		inFlight.Add(1)
		defer inFlight.Add(-1)
		if replica == 0 {
			return "primary-value", nil
		}
		backupIssued.Add(1)
		<-ctx.Done()
		return "", context.Cause(ctx)
	}

	val, err := Hedge(context.Background(), time.Hour, query)
	if err != nil {
		t.Fatalf("err = %v, want nil", err)
	}
	if val != "primary-value" {
		t.Fatalf("val = %q, want %q", val, "primary-value")
	}
	if got := inFlight.Load(); got != 0 {
		t.Fatalf("in-flight gauge = %d after return, want 0 (leaked request)", got)
	}
	if got := backupIssued.Load(); got != 0 {
		t.Fatalf("backup issued %d times, want 0 (hedge should not have fired)", got)
	}
}

// TestHedgeWins: the primary stalls until cancelled; the backup, issued after a
// zero delay, answers first. Determinism comes from the primary blocking on
// ctx.Done rather than any timing. The loser observes context.Cause == errHedgeWon,
// and the in-flight gauge is zero once Hedge has joined both goroutines.
func TestHedgeWins(t *testing.T) {
	t.Parallel()

	var inFlight atomic.Int64
	loserCause := make(chan error, 1)
	query := func(ctx context.Context, replica int) (string, error) {
		inFlight.Add(1)
		defer inFlight.Add(-1)
		if replica == 1 {
			return "backup-value", nil
		}
		<-ctx.Done()
		cause := context.Cause(ctx)
		loserCause <- cause
		return "", cause
	}

	val, err := Hedge(context.Background(), 0, query)
	if err != nil {
		t.Fatalf("err = %v, want nil", err)
	}
	if val != "backup-value" {
		t.Fatalf("val = %q, want %q", val, "backup-value")
	}
	if got := inFlight.Load(); got != 0 {
		t.Fatalf("in-flight gauge = %d after return, want 0 (leaked request)", got)
	}
	// The primary sent its observed cause before returning, and Hedge joined it
	// before returning, so this receive does not block.
	if cause := <-loserCause; !errors.Is(cause, errHedgeWon) {
		t.Fatalf("loser cause = %v, want errHedgeWon", cause)
	}
}

// TestBothFail: both replicas fail with distinct errors and Hedge returns
// errors.Join of the two, so errors.Is matches each. Both goroutines are joined,
// so the in-flight gauge is zero afterward.
func TestBothFail(t *testing.T) {
	t.Parallel()

	errPrimary := errors.New("primary refused")
	errBackup := errors.New("backup refused")

	var inFlight atomic.Int64
	query := func(ctx context.Context, replica int) (string, error) {
		inFlight.Add(1)
		defer inFlight.Add(-1)
		if replica == 0 {
			return "", errPrimary
		}
		return "", errBackup
	}

	val, err := Hedge(context.Background(), 0, query)
	if val != "" {
		t.Fatalf("val = %q, want empty", val)
	}
	if !errors.Is(err, errPrimary) {
		t.Fatalf("err = %v, want it to wrap errPrimary", err)
	}
	if !errors.Is(err, errBackup) {
		t.Fatalf("err = %v, want it to wrap errBackup", err)
	}
	if got := inFlight.Load(); got != 0 {
		t.Fatalf("in-flight gauge = %d after return, want 0 (leaked request)", got)
	}
}
```

## Review

`Hedge` is correct when it returns the first successful replica's value, cancels the
loser with the specific cause `errHedgeWon`, and — the property that matters most in
production — leaves no goroutine or request alive past its return. The single invariant
that guarantees leak-freedom is the join: a lone waiter runs `wg.Wait(); close(done)`,
and the caller's `range` over `done` cannot exit until that close, so both goroutines have
provably finished before `Hedge` returns. The same `Wait` supplies the happens-before edge
that makes reading `results[winner]` safe without a lock, which is why cancellation and
join are separate concerns: cancellation asks the loser to stop, the join proves it did.
The tests pin this down deterministically with context/channel gating — never sleeps — and
prove leak-freedom two ways at once: an atomic in-flight gauge that must read zero after
each call, and `goleak.VerifyTestMain` catching any surviving goroutine. The bug this
prevents is the classic hedging leak: returning the winner while the slow replica's call
runs on against a live context, quietly exhausting connections and goroutines under load.

## Resources

- [`context.WithCancelCause`](https://pkg.go.dev/context#WithCancelCause) -- deriving a cancelable context whose cancellation carries a distinguishable cause read back via `context.Cause`.
- [`sync.WaitGroup`](https://pkg.go.dev/sync#WaitGroup) -- the join counter and the memory guarantee that `Wait` publishes the goroutines' writes.
- [`errors.Join`](https://pkg.go.dev/errors#Join) -- combining both replicas' failures into one error that `errors.Is` still matches against each.
- [Go Concurrency Patterns: Pipelines and cancellation](https://go.dev/blog/pipelines) -- how explicit cancellation plus joining every goroutine prevents the leak that hedging otherwise causes.

---

Back to [00-concepts.md](00-concepts.md) | Prev: [13-transitive-closure-dynamic-add-traversal.md](13-transitive-closure-dynamic-add-traversal.md) | Next: [../06-ranging-over-channels/00-concepts.md](../06-ranging-over-channels/00-concepts.md)
