# Exercise 13: Stress-Verify That Request Hedging Reaps Every Loser With Zero Goroutine Leak

**Level: Advanced**

A latency-sensitive read path hedges: it fires the same request at two or three replicas, takes the first success, and cancels the losers. The classic production defect is a loser goroutine that leaks -- it blocks forever trying to send a late result on a channel no one reads, or it never observes cancellation -- so under steady load the service accumulates goroutines until it OOMs. This exercise implements generic hedging with correct cancellation and then pins leak-freedom as an invariant: thousands of hedges, including a replica that hangs until cancelled, must leave zero leaked goroutines.

This module is self-contained: its own module, a `hedge` package, a demo, and tests.
Nothing here imports another exercise.

## What you'll build

```text
hedge/                       independent module: example.com/hedge
  go.mod                     go 1.26
  hedge.go                   Do[T](ctx, replicas...) -- race replicas, first success wins, reap the rest
  cmd/demo/main.go           runnable demo: fast wins and loser is reaped, all-fail joins, parent cancel aborts
  hedge_test.go              fast-wins-slow-reaped, all-fail-joins, parent-cancel, and a >=2000 hedge leak stress guard
```

- Files: `hedge.go`, `cmd/demo/main.go`, `hedge_test.go`.
- Implement: `Do[T any](ctx context.Context, replicas ...func(ctx context.Context) (T, error)) (T, error)` and `Result[T any]{ Val T; Err error }`.
- Test: fast replica wins and the slow one is reaped; all-fail returns an `errors.Join`; a caller-cancelled parent aborts every replica; a stress loop of 2000 hedges leaves the goroutine set at baseline (goleak).
- Verify: `go test -count=1 -race ./...`

Set up the module:

```bash
mkdir -p ~/go-exercises/hedge/cmd/demo
cd ~/go-exercises/hedge
go mod init example.com/hedge
go get go.uber.org/goleak
go mod tidy
```

### The two ways a hedge leaks, and the two rules that prevent it

Hedging is a latency trade: you spend extra work (N in-flight requests instead of one) to cut the tail, because the slowest replica no longer dictates your p99. The value of the pattern is that `Do` returns the instant the *first* replica succeeds -- it must not wait for the losers. That is exactly what makes it dangerous. When `Do` returns early, the loser goroutines are still running, and if they are not built correctly they never stop.

There are precisely two failure modes, and both are cancellation failures:

1. **The unobserved cancellation.** A replica that does blocking I/O but never selects on `ctx.Done()` will keep running to completion (or forever) after a winner is chosen. It ignores the signal telling it to quit. On a goroutine dump this is a growing pile parked in the replica's own blocking call.
2. **The blocked send.** A replica finishes its work and tries to deliver its result on a channel -- but `Do` has already returned and no one is receiving. On an unbuffered channel the send blocks forever, wedging the goroutine on `chan send`. This is the subtler bug: the replica *did* the right thing (it produced a result), and it leaks anyway because delivery has no reader.

The implementation closes both holes with two rules. First, every replica runs under a **child context derived with `context.WithCancelCause`**, and `Do` cancels that child the moment it has a winner (and, via `defer`, on every other exit path too). A well-behaved replica selects on `ctx.Done()` and returns, so cancellation is observed. Second, **result delivery is non-blocking**: the result channel is buffered to the replica count, so every goroutine can send its one result without a reader present -- even a loser whose result will be discarded. A buffered slot per replica means no send can ever wedge. Together: cancellation guarantees the loser *wants* to exit, and the buffered channel guarantees its final send *can* complete. That is the whole leak-free invariant.

The winner selection itself is a simple fan-in loop. Each replica sends one `{index, Result}` onto the buffered channel; `Do` receives up to N of them; the first with a nil error wins and triggers the cancel; if all N carry errors, `Do` returns `errors.Join` of them in replica order (the index keeps the join deterministic regardless of which replica finished first).

Create `hedge.go`:

```go
package hedge

import (
	"context"
	"errors"
)

// Result pairs a replica's value with its error. Exactly one of the two is
// meaningful per replica: a nil Err means Val is valid.
type Result[T any] struct {
	Val T
	Err error
}

// ErrNoReplicas is returned when Do is called with no replicas to race.
var ErrNoReplicas = errors.New("hedge: no replicas")

// errSuperseded is the cancellation cause attached to losing replicas once a
// faster replica has produced the winning result.
var errSuperseded = errors.New("hedge: superseded by a faster replica")

// Do races every replica against the others under a child context and returns
// the first successful (Val, nil). The moment a winner is found the child
// context is cancelled so the losers observe cancellation and exit. If every
// replica fails, Do returns the errors.Join of their errors in replica order.
//
// The one hard invariant is leak-freedom: a loser must never wedge on delivering
// its late result. The result channel is buffered to the replica count, so every
// goroutine can send exactly once without a reader present, and cancellation of
// the child context guarantees a blocking replica unblocks and returns.
func Do[T any](ctx context.Context, replicas ...func(ctx context.Context) (T, error)) (T, error) {
	var zero T
	if len(replicas) == 0 {
		return zero, ErrNoReplicas
	}

	ctx, cancel := context.WithCancelCause(ctx)
	// On every return path the losers are cancelled; passing nil keeps an
	// already-set cause (errSuperseded) as the winning one.
	defer cancel(nil)

	type indexed struct {
		i int
		r Result[T]
	}
	// Buffered to len(replicas): the leak-free guarantee. Every replica delivers
	// one result without blocking, even a loser whose result no one will read.
	ch := make(chan indexed, len(replicas))

	for i, replica := range replicas {
		go func() {
			val, err := replica(ctx)
			ch <- indexed{i, Result[T]{Val: val, Err: err}}
		}()
	}

	errs := make([]error, len(replicas))
	for range replicas {
		got := <-ch
		if got.r.Err == nil {
			cancel(errSuperseded) // reap the losers
			return got.r.Val, nil
		}
		errs[got.i] = got.r.Err
	}
	return zero, errors.Join(errs...)
}
```

### The runnable demo

The demo runs three scenarios. In the first, a fast replica wins over one that hangs on `ctx.Done()`; a `reaped` channel makes the loser's exit observable so the print is deterministic. The second races two failing replicas and confirms the joined error carries both. The third passes an already-cancelled parent and confirms every replica aborts with `context.Canceled`.

Create `cmd/demo/main.go`:

```go
package main

import (
	"context"
	"errors"
	"fmt"

	"example.com/hedge"
)

func main() {
	// Scenario 1: a fast replica wins; a replica that hangs until cancelled is
	// reaped. The reaped channel makes the loser's exit observable so the demo
	// prints deterministically.
	reaped := make(chan struct{})
	fast := func(ctx context.Context) (string, error) {
		return "replica-A", nil
	}
	slow := func(ctx context.Context) (string, error) {
		defer close(reaped)
		<-ctx.Done() // hang until the hedge cancels us
		return "", context.Cause(ctx)
	}
	val, err := hedge.Do(context.Background(), fast, slow)
	<-reaped // the loser has observed cancellation and exited
	fmt.Printf("scenario 1: winner=%q err=%v loser-reaped=true\n", val, err)

	// Scenario 2: every replica fails; Do returns errors.Join in replica order.
	errA := errors.New("replica-A: connection refused")
	errB := errors.New("replica-B: timeout")
	_, err = hedge.Do(context.Background(),
		func(ctx context.Context) (string, error) { return "", errA },
		func(ctx context.Context) (string, error) { return "", errB },
	)
	fmt.Println("scenario 2: all-fail is errA:", errors.Is(err, errA), "is errB:", errors.Is(err, errB))

	// Scenario 3: a caller-cancelled parent aborts every replica.
	pctx, pcancel := context.WithCancel(context.Background())
	pcancel()
	block := func(ctx context.Context) (string, error) {
		<-ctx.Done()
		return "", ctx.Err()
	}
	_, err = hedge.Do(pctx, block, block)
	fmt.Println("scenario 3: parent-cancelled is context.Canceled:", errors.Is(err, context.Canceled))
}
```

Run it:

```bash
go run ./cmd/demo
```

Expected output:

```
scenario 1: winner="replica-A" err=<nil> loser-reaped=true
scenario 2: all-fail is errA: true is errB: true
scenario 3: parent-cancelled is context.Canceled: true
```

### Tests

`TestMain` wraps the whole package in `goleak.VerifyTestMain`, so any goroutine that outlives its test -- the signature of a blocked send or an unobserved cancellation -- fails the build. `TestFastWinsSlowReaped` pins the core behavior: the fast replica's value is returned and the slow one is confirmed reaped via a channel it closes on exit, making the assertion deterministic rather than timing-based. `TestAllFailJoins` pins that a total failure returns an `errors.Join` in which each error is matchable with `errors.Is`. `TestParentCancelAbortsAll` pins that a pre-cancelled parent aborts every replica. `TestNoReplicas` pins the empty-input guard. `TestLeakFreedomUnderLoad` is the stress invariant: 2000 hedges, each with a replica that hangs until cancellation, each asserting the loser exited that iteration, with `goleak.VerifyNone` at the end proving the goroutine set returned to baseline.

Create `hedge_test.go`:

```go
package hedge

import (
	"context"
	"errors"
	"testing"

	"go.uber.org/goleak"
)

// TestMain wraps the whole package so a blocked-send or unobserved-cancellation
// regression -- any goroutine that outlives its test -- fails the build.
func TestMain(m *testing.M) {
	goleak.VerifyTestMain(m)
}

// TestFastWinsSlowReaped pins the core hedge behavior: with one fast replica and
// one that blocks on ctx.Done(), Do returns the fast value and the slow replica
// observes cancellation and exits. The reaped channel makes the loser's exit
// observable, so the assertion is deterministic rather than timing-based.
func TestFastWinsSlowReaped(t *testing.T) {
	reaped := make(chan struct{})
	fast := func(ctx context.Context) (int, error) { return 42, nil }
	slow := func(ctx context.Context) (int, error) {
		defer close(reaped)
		<-ctx.Done()
		return 0, context.Cause(ctx)
	}

	got, err := Do(context.Background(), fast, slow)
	if err != nil {
		t.Fatalf("Do err = %v; want nil", err)
	}
	if got != 42 {
		t.Fatalf("Do value = %d; want 42", got)
	}
	<-reaped // the slow replica was reaped, not leaked
}

// TestAllFailJoins pins that when every replica fails, Do returns an errors.Join
// carrying each replica's error, matchable with errors.Is.
func TestAllFailJoins(t *testing.T) {
	errA := errors.New("replica-A down")
	errB := errors.New("replica-B down")
	errC := errors.New("replica-C down")

	_, err := Do(context.Background(),
		func(ctx context.Context) (int, error) { return 0, errA },
		func(ctx context.Context) (int, error) { return 0, errB },
		func(ctx context.Context) (int, error) { return 0, errC },
	)
	for _, want := range []error{errA, errB, errC} {
		if !errors.Is(err, want) {
			t.Fatalf("Do err = %v; does not carry %v", err, want)
		}
	}
}

// TestParentCancelAbortsAll pins that a caller-cancelled parent aborts every
// replica: all replicas block on ctx.Done(), the parent is already cancelled, so
// they all return context.Canceled and Do surfaces it.
func TestParentCancelAbortsAll(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	cancel()

	block := func(ctx context.Context) (int, error) {
		<-ctx.Done()
		return 0, ctx.Err()
	}
	_, err := Do(ctx, block, block, block)
	if !errors.Is(err, context.Canceled) {
		t.Fatalf("Do err = %v; want context.Canceled", err)
	}
}

// TestNoReplicas pins the empty-input guard.
func TestNoReplicas(t *testing.T) {
	_, err := Do[int](context.Background())
	if !errors.Is(err, ErrNoReplicas) {
		t.Fatalf("Do err = %v; want ErrNoReplicas", err)
	}
}

// TestLeakFreedomUnderLoad is the stress invariant: >=2000 hedges, each with a
// replica that hangs until cancellation, must leave the goroutine set at
// baseline. Per iteration the loser signals its own exit, so the reaping is
// asserted directly; goleak.VerifyNone at the end proves no goroutine survived
// the whole loop. A blocked-send or unobserved-cancellation regression fails here.
func TestLeakFreedomUnderLoad(t *testing.T) {
	defer goleak.VerifyNone(t)

	const hedges = 2000
	for range hedges {
		exited := make(chan struct{})
		fast := func(ctx context.Context) (int, error) { return 1, nil }
		hang := func(ctx context.Context) (int, error) {
			defer close(exited)
			<-ctx.Done()
			return 0, context.Cause(ctx)
		}
		got, err := Do(context.Background(), fast, hang)
		if err != nil || got != 1 {
			t.Fatalf("Do = (%d, %v); want (1, nil)", got, err)
		}
		<-exited // the loser was reaped this iteration
	}
}
```

## Review

Correct hedging here means two things at once: `Do` returns the first success without waiting for the losers, and every loser is nonetheless reaped so the goroutine count is flat under load. The invariant that guarantees it is the pair of rules baked into `Do` -- a `context.WithCancelCause` child that is cancelled the instant a winner is chosen (so a blocking replica observes `ctx.Done()` and exits) and a result channel buffered to the replica count (so a loser's final send always has a slot and can never wedge on `chan send`). The tests prove it from both ends: `TestFastWinsSlowReaped` observes the slow replica actually exit rather than assuming it did, and `TestLeakFreedomUnderLoad` runs 2000 hedges each with a hang-until-cancelled replica and lets `goleak` assert the goroutine set returned to its baseline. Drop either rule -- forget the cancel and the hang replicas never quit; make the channel unbuffered and every discarded loser blocks on its send -- and the leak test turns red. This is the exact production bug the pattern prevents: a hedged read path that looks fast in a benchmark but leaks one goroutine per request until the service OOMs at 03:00.

## Resources

- [`context.WithCancelCause`](https://pkg.go.dev/context#WithCancelCause) -- deriving the cancellable child and attaching a cause the losers can read via `context.Cause`.
- [`errors.Join`](https://pkg.go.dev/errors#Join) -- combining every replica's error when the whole hedge fails, each still matchable with `errors.Is`.
- [`go.uber.org/goleak`](https://pkg.go.dev/go.uber.org/goleak) -- `VerifyTestMain` and `VerifyNone`, the baseline-diff leak guard that turns a leaked loser into a build failure.
- [Go Blog: Pipelines and cancellation](https://go.dev/blog/pipelines) -- the cancellation-and-cleanup discipline this hedge applies to fan-in over racing replicas.

---

Back to [00-concepts.md](00-concepts.md) | Prev: [12-outbox-leak-localize-profile-delta.md](12-outbox-leak-localize-profile-delta.md) | Next: [../../14-select-and-context/01-select-statement-basics/00-concepts.md](../../14-select-and-context/01-select-statement-basics/00-concepts.md)
