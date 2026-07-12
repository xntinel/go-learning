# Exercise 4: Hedged Fan-Out — First Reply Wins, Cancel the Losers With a Cause

Hedged reads are a standard latency technique: dispatch the same query to several
replicas at once, take the first successful answer, and cancel the rest. The
subtlety a senior engineer cares about is the *why* attached to those
cancellations. Cancelling the losers with a plain context leaves their aborted
work logged as the opaque "context canceled". Cancelling them with
`context.WithCancelCause` attaches a diagnosable reason — "a winner was elected" —
that `context.Cause` can read back, turning noise into signal.

This module is fully self-contained: its own `go mod init`, package, demo, and
tests. Nothing here imports another exercise.

## What you'll build

```text
fanout/                    independent module: example.com/fanout
  go.mod                   module example.com/fanout
  fanout.go                Replica; QueryReplicas; ErrWinnerElected; ErrNoReplicas
  cmd/
    demo/
      main.go              one fast + two slow replicas; prints winner and causes
  fanout_test.go           first-reply-wins, losers-see-cause, all-fail
```

- Files: `fanout.go`, `cmd/demo/main.go`, `fanout_test.go`.
- Implement: `QueryReplicas(ctx, replicas) (string, error)` that returns the first success and cancels the in-flight losers with the `ErrWinnerElected` cause.
- Test: the fast replica's result is returned; a loser reads `context.Cause == ErrWinnerElected` while `ctx.Err()` is `context.Canceled`; when all replicas fail, an aggregated error is returned whose cause is not the winner sentinel.
- Verify: `go test -count=1 -race ./...`

Set up the module:

```bash
mkdir -p go-solutions/14-select-and-context/04-context-withcancel/04-cancel-with-cause-fanout/cmd/demo
cd go-solutions/14-select-and-context/04-context-withcancel/04-cancel-with-cause-fanout
```

### Why the cause is the whole point

Without causes, this pattern still works — you race N replicas, take the first
success, and derive a `context.WithCancel` child so cancelling it aborts the
losers. But then every loser that inspects `context.Cause(ctx)` (or `ctx.Err()`)
sees `context.Canceled`, indistinguishable from "the whole request was aborted by
the client" or "a deadline fired". When you are staring at a replica's trace at
3am wondering why it abandoned a query, "context canceled" tells you nothing.

`context.WithCancelCause(parent)` fixes this. It returns a `CancelCauseFunc` — a
`func(cause error)` instead of the usual `func()`. When the winner is elected you
call `cancel(ErrWinnerElected)`, and every loser that reads
`context.Cause(ctx)` gets back exactly that sentinel. The control-flow value is
unchanged: `ctx.Err()` is still `context.Canceled`, so any code that branches on
"canceled vs deadline" keeps working. The *diagnostic* value is now precise. This
is the division of labor to internalize: `ctx.Err()` for control flow,
`context.Cause(ctx)` for the reason.

The mechanics: `QueryReplicas` derives a `WithCancelCause` child and launches one
goroutine per replica, each calling the replica with that shared child context.
Results flow back on a buffered channel — buffered to `len(replicas)` so that a
losing replica which finishes *after* we have already returned can still send
without blocking, and its goroutine can exit cleanly (no leak). The coordinator
reads results in arrival order; the first `nil`-error result triggers
`cancel(ErrWinnerElected)` and returns. If every result carries an error, it
returns an aggregated error wrapping the last one. The deferred
`cancel(context.Canceled)` at the top is the mandatory cleanup for the winner
path — and because a second cancel is a no-op that does not overwrite the cause,
it never clobbers the `ErrWinnerElected` a winner already set.

Create `fanout.go`:

```go
package fanout

import (
	"context"
	"errors"
	"fmt"
	"sync"
)

// ErrWinnerElected is set as the cancellation cause on the losing replicas once
// a winner returns. A loser reads it via context.Cause to know why it was
// aborted, instead of the opaque context.Canceled.
var ErrWinnerElected = errors.New("fanout: winner elected, losers cancelled")

// ErrNoReplicas is returned when QueryReplicas is called with no replicas.
var ErrNoReplicas = errors.New("fanout: no replicas")

// Replica performs one read. It must honor ctx: a well-behaved replica selects
// on ctx.Done() so it aborts promptly when a sibling wins.
type Replica func(ctx context.Context) (string, error)

// QueryReplicas dispatches the same read to every replica concurrently, returns
// the first successful result, and cancels the in-flight losers with the
// ErrWinnerElected cause. If every replica fails, it returns an aggregated error.
func QueryReplicas(ctx context.Context, replicas []Replica) (string, error) {
	if len(replicas) == 0 {
		return "", ErrNoReplicas
	}

	ctx, cancel := context.WithCancelCause(ctx)
	defer cancel(context.Canceled)

	type outcome struct {
		val string
		err error
	}
	results := make(chan outcome, len(replicas))

	var wg sync.WaitGroup
	for _, r := range replicas {
		wg.Add(1)
		go func(r Replica) {
			defer wg.Done()
			v, err := r(ctx)
			results <- outcome{val: v, err: err}
		}(r)
	}
	go func() {
		wg.Wait()
		close(results)
	}()

	var lastErr error
	for o := range results {
		if o.err == nil {
			cancel(ErrWinnerElected)
			return o.val, nil
		}
		lastErr = o.err
	}
	return "", fmt.Errorf("fanout: all replicas failed: %w", lastErr)
}
```

### The runnable demo

The demo runs one instantly-succeeding replica and two slow ones that block on
`ctx.Done()`. The fast replica wins deterministically; the two losers wake on the
cancel, read `context.Cause`, and report it. `main` reads the winner, then drains
the two loser causes, so the output order is fixed.

Create `cmd/demo/main.go`:

```go
package main

import (
	"context"
	"fmt"

	"example.com/fanout"
)

func main() {
	causes := make(chan error, 2)

	fast := func(ctx context.Context) (string, error) {
		return "replica-0", nil
	}
	slow := func(ctx context.Context) (string, error) {
		<-ctx.Done()
		causes <- context.Cause(ctx)
		return "", ctx.Err()
	}

	winner, err := fanout.QueryReplicas(context.Background(),
		[]fanout.Replica{fast, slow, slow})
	if err != nil {
		fmt.Println("error:", err)
		return
	}
	fmt.Println("winner:", winner)
	for i := 0; i < 2; i++ {
		fmt.Println("loser cancelled by:", <-causes)
	}
}
```

Run it:

```bash
go run ./cmd/demo
```

Expected output:

```
winner: replica-0
loser cancelled by: fanout: winner elected, losers cancelled
loser cancelled by: fanout: winner elected, losers cancelled
```

### Tests

`TestFirstReplyWins` pairs a fast replica with a slow one and asserts the fast
result comes back. `TestLosersSeeCause` has the loser record both
`context.Cause(ctx)` and `ctx.Err()`, then asserts the cause is
`ErrWinnerElected` while the err is `context.Canceled` — the exact division of
labor. `TestAllFail` makes every replica error and asserts the aggregated error
wraps the last one and is not the winner sentinel.

Create `fanout_test.go`:

```go
package fanout

import (
	"context"
	"errors"
	"testing"
	"time"
)

func TestFirstReplyWins(t *testing.T) {
	t.Parallel()

	fast := func(ctx context.Context) (string, error) {
		return "fast", nil
	}
	slow := func(ctx context.Context) (string, error) {
		select {
		case <-ctx.Done():
			return "", ctx.Err()
		case <-time.After(time.Hour):
			return "slow", nil
		}
	}

	got, err := QueryReplicas(context.Background(), []Replica{slow, fast, slow})
	if err != nil {
		t.Fatalf("QueryReplicas err = %v, want nil", err)
	}
	if got != "fast" {
		t.Fatalf("QueryReplicas = %q, want %q", got, "fast")
	}
}

func TestLosersSeeCause(t *testing.T) {
	t.Parallel()

	type seen struct {
		cause error
		err   error
	}
	loser := make(chan seen, 1)

	fast := func(ctx context.Context) (string, error) {
		return "fast", nil
	}
	slow := func(ctx context.Context) (string, error) {
		<-ctx.Done()
		loser <- seen{cause: context.Cause(ctx), err: ctx.Err()}
		return "", ctx.Err()
	}

	got, err := QueryReplicas(context.Background(), []Replica{fast, slow})
	if err != nil || got != "fast" {
		t.Fatalf("QueryReplicas = %q, %v; want fast, nil", got, err)
	}

	select {
	case s := <-loser:
		if !errors.Is(s.cause, ErrWinnerElected) {
			t.Fatalf("loser cause = %v, want ErrWinnerElected", s.cause)
		}
		if !errors.Is(s.err, context.Canceled) {
			t.Fatalf("loser ctx.Err() = %v, want context.Canceled", s.err)
		}
	case <-time.After(time.Second):
		t.Fatal("loser did not observe cancellation within 1s")
	}
}

func TestAllFail(t *testing.T) {
	t.Parallel()

	errBoom := errors.New("replica down")
	bad := func(ctx context.Context) (string, error) {
		return "", errBoom
	}

	_, err := QueryReplicas(context.Background(), []Replica{bad, bad, bad})
	if !errors.Is(err, errBoom) {
		t.Fatalf("QueryReplicas err = %v, want wrapped errBoom", err)
	}
	if errors.Is(err, ErrWinnerElected) {
		t.Fatalf("all-fail error should not carry ErrWinnerElected: %v", err)
	}
}

func TestNoReplicas(t *testing.T) {
	t.Parallel()

	if _, err := QueryReplicas(context.Background(), nil); !errors.Is(err, ErrNoReplicas) {
		t.Fatalf("QueryReplicas(nil) err = %v, want ErrNoReplicas", err)
	}
}
```

## Review

The fan-out is correct when the first `nil`-error result is returned and the
losers are cancelled with `ErrWinnerElected` as the cause. The property that
proves you used `WithCancelCause` correctly is the split observed in
`TestLosersSeeCause`: `context.Cause(ctx)` is your sentinel while `ctx.Err()`
remains `context.Canceled`. Two traps to avoid: sizing the results channel too
small (a loser that finishes after you return would block on its send and leak its
goroutine — size it to `len(replicas)`), and logging `ctx.Err()` instead of
`context.Cause(ctx)` on the loser side, which throws away the reason you attached.
The deferred `cancel(context.Canceled)` is the mandatory cleanup for the winner
path; it is a no-op after a winner already set the cause. Run `go test -race` to
confirm the concurrent replicas and the shared context are race-free.

## Resources

- [context.WithCancelCause](https://pkg.go.dev/context#WithCancelCause) — the cause-carrying cancel and its `CancelCauseFunc`.
- [context.Cause](https://pkg.go.dev/context#Cause) — reading the reason a context was cancelled.
- [The Tail at Scale (Dean & Barroso)](https://research.google/pubs/the-tail-at-scale/) — hedged requests and why racing replicas cuts tail latency.

---

Back to [00-concepts.md](00-concepts.md) | Next: [05-context-retry-backoff.md](05-context-retry-backoff.md)
