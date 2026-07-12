# Exercise 3: First-Of-N Healthy Responder

When a request can be served by any of several replicas, the tail-tolerant move is to ask all of them at once and take the first one that answers healthily, cancelling the rest. This exercise builds that combiner: an or-channel inverted from "first reason to stop" into "first good answer", with the two senior twists that separate it from a naive race — the winner must be healthy, not merely fast, and the losers must actually be cancelled.

This module is fully self-contained. It begins with its own `go mod init`, defines everything it needs, and ships its own demo and tests. Nothing here imports any other exercise.

## What you'll build

```text
firstof.go           First[T] — race replicas, return first nil-error result, cancel the rest
cmd/
  demo/
    main.go          three replicas (one fast failure, two healthy), report the winner
firstof_test.go      first-healthy beats fast-failure, all-fail aggregates, losers cancelled, empty
```

- Files: `firstof.go`, `cmd/demo/main.go`, `firstof_test.go`.
- Implement: `First[T any](ctx context.Context, replicas []string, fn func(context.Context, string) (T, error)) (T, error)`.
- Test: a healthy-but-slower replica beats a fast failure, all-fail returns an aggregated error, the losing replicas observe cancellation, and an empty replica list errors.
- Verify: `go test -race ./...`

### Why "first healthy" and why cancellation is load-bearing

This is the technique Dean and Barroso describe in "The Tail at Scale": fire a request at redundant replicas so a single slow or sick one cannot dominate the response time. Expressed as an or-channel it is "the first event wins and cancels the others", but two details make it senior rather than a toy `select`.

The first is that the winning event must be a healthy result, not the first response. A replica that fails instantly is the fastest to respond, and a naive race would hand the caller that fast failure while a healthy replica was still 20 ms from succeeding. So `First` distinguishes a result with a nil error (a win) from one with a non-nil error (a recorded failure): a failure does not end the race, it is appended to an error list and the loop keeps reading until a healthy result arrives or every replica has failed. When all fail, the aggregated error from `errors.Join` carries every replica's reason, which is what an operator needs to debug a total outage.

The second is that cancellation is not cleanup, it is the point. `First` derives a cancellable context from the caller's, hands it to every replica call, and cancels it the instant a winner is chosen. Without that cancel, the losing replicas keep running — holding connections, occupying the very capacity the redundancy was meant to spare — long after their answers became useless. The `defer cancel()` also covers the all-fail path, so no context leaks regardless of outcome.

The leak-free fan-out hinges on one buffer-size decision. Each replica goroutine sends its result into a channel and exits; the main loop reads from that channel. Because `First` returns as soon as the first healthy result lands, the remaining goroutines are still running and will still send. If the channel were unbuffered, those sends would block forever on a receiver that has already returned, and the goroutines would leak. Buffering the channel to exactly `len(replicas)` guarantees every goroutine can deposit its result and exit even after the winner has been chosen and the reader is gone.

Create `firstof.go`:

```go
package firstof

import (
	"context"
	"errors"
)

// First runs fn against every replica concurrently and returns the first
// healthy (nil-error) result. A replica that returns an error does not win;
// its error is recorded and the race continues. As soon as a healthy result
// arrives, First cancels the context so the remaining replicas stop, then
// returns. If every replica fails, First returns the joined error.
func First[T any](ctx context.Context, replicas []string, fn func(context.Context, string) (T, error)) (T, error) {
	var zero T
	if len(replicas) == 0 {
		return zero, errors.New("firstof: no replicas")
	}

	ctx, cancel := context.WithCancel(ctx)
	defer cancel()

	type result struct {
		val T
		err error
	}
	// Buffered to len(replicas) so a loser can always send and exit, even
	// after a winner has been chosen and the reader has returned.
	results := make(chan result, len(replicas))

	for _, replica := range replicas {
		replica := replica
		go func() {
			val, err := fn(ctx, replica)
			results <- result{val: val, err: err}
		}()
	}

	var errs []error
	for range replicas {
		got := <-results
		if got.err == nil {
			cancel() // stop the losers; their work is now useless.
			return got.val, nil
		}
		errs = append(errs, got.err)
	}
	return zero, errors.Join(errs...)
}
```

Read the loop as the inverse of the core or-channel: instead of waiting for the first input to close, it waits for the first result whose error is nil. A failed result is not the end — it is appended to `errs` and the loop reads again, up to `len(replicas)` times. The `cancel()` on the winning path is what converts "I have my answer" into "everyone else, stop", and the buffered channel is what lets those stopped goroutines exit instead of leaking on a send.

### The runnable demo

The demo runs three replicas. Replica A fails fast at 10 ms — the fastest to respond, but unhealthy, so it must not win. Replicas B and C succeed at 30 ms and 60 ms. The healthy winner is B, and C is cancelled mid-flight. The outcome is deterministic because the fast failure is strictly faster than the first success and the first success is strictly faster than the second.

Create `cmd/demo/main.go`:

```go
package main

import (
	"context"
	"fmt"
	"time"

	"example.com/first-of-n"
)

func main() {
	latency := map[string]time.Duration{"A": 10 * time.Millisecond, "B": 30 * time.Millisecond, "C": 60 * time.Millisecond}
	healthy := map[string]bool{"A": false, "B": true, "C": true}

	call := func(ctx context.Context, replica string) (string, error) {
		select {
		case <-time.After(latency[replica]):
			if !healthy[replica] {
				return "", fmt.Errorf("replica %s: unhealthy", replica)
			}
			return "answer-from-" + replica, nil
		case <-ctx.Done():
			return "", ctx.Err()
		}
	}

	val, err := firstof.First(context.Background(), []string{"A", "B", "C"}, call)
	if err != nil {
		fmt.Println("all replicas failed:", err)
		return
	}
	fmt.Printf("served by the first healthy replica: %s\n", val)
}
```

Run it:

```bash
go run ./cmd/demo
```

Expected output:

```
served by the first healthy replica: answer-from-B
```

### Tests

The suite pins the two senior behaviors and the boundaries. `TestFirstHealthyBeatsFastFailure` makes the unhealthy replica respond first and asserts the slower healthy replica still wins. `TestAllFailAggregates` makes every replica fail and asserts the joined error names all of them. `TestLosersAreCancelled` makes one replica win immediately and asserts every loser observed `ctx.Done()`, using a `WaitGroup` so the assertion is deterministic rather than timing-dependent. `TestEmptyReplicasErrors` asserts an empty list is rejected.

Create `firstof_test.go`:

```go
package firstof

import (
	"context"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

func TestFirstHealthyBeatsFastFailure(t *testing.T) {
	t.Parallel()

	call := func(ctx context.Context, replica string) (string, error) {
		switch replica {
		case "fast-fail":
			return "", &replicaError{replica}
		case "slow-ok":
			select {
			case <-time.After(20 * time.Millisecond):
				return "ok-from-slow", nil
			case <-ctx.Done():
				return "", ctx.Err()
			}
		}
		return "", nil
	}

	val, err := First(context.Background(), []string{"fast-fail", "slow-ok"}, call)
	if err != nil {
		t.Fatalf("First returned error, want healthy result: %v", err)
	}
	if val != "ok-from-slow" {
		t.Fatalf("val = %q, want %q", val, "ok-from-slow")
	}
}

func TestAllFailAggregates(t *testing.T) {
	t.Parallel()

	call := func(ctx context.Context, replica string) (string, error) {
		return "", &replicaError{replica}
	}

	_, err := First(context.Background(), []string{"a", "b", "c"}, call)
	if err == nil {
		t.Fatal("First returned nil error when every replica failed")
	}
	for _, name := range []string{"a", "b", "c"} {
		if !strings.Contains(err.Error(), name) {
			t.Fatalf("joined error %q missing replica %q", err.Error(), name)
		}
	}
}

func TestLosersAreCancelled(t *testing.T) {
	t.Parallel()

	const n = 8
	replicas := make([]string, n)
	for i := range replicas {
		replicas[i] = string(rune('a' + i))
	}

	var cancelled atomic.Int32
	var wg sync.WaitGroup
	wg.Add(n)

	call := func(ctx context.Context, replica string) (string, error) {
		defer wg.Done()
		if replica == "a" {
			return "winner", nil // wins immediately.
		}
		select {
		case <-ctx.Done():
			cancelled.Add(1)
			return "", ctx.Err()
		case <-time.After(time.Second):
			return "too-slow", nil
		}
	}

	val, err := First(context.Background(), replicas, call)
	if err != nil || val != "winner" {
		t.Fatalf("First = (%q, %v), want (\"winner\", nil)", val, err)
	}

	wg.Wait()
	if got := cancelled.Load(); got != n-1 {
		t.Fatalf("cancelled losers = %d, want %d", got, n-1)
	}
}

func TestEmptyReplicasErrors(t *testing.T) {
	t.Parallel()

	_, err := First(context.Background(), nil, func(context.Context, string) (string, error) {
		return "", nil
	})
	if err == nil {
		t.Fatal("First with no replicas returned nil error")
	}
}

type replicaError struct{ name string }

func (e *replicaError) Error() string { return "replica " + e.name + " failed" }
```

## Review

The combiner is correct when a fast failure cannot win, every loser stops, and no goroutine leaks. The "first healthy" rule lives in one branch — `if got.err == nil` wins, otherwise the error is recorded and the loop reads again — so deleting that branch and returning the first result of any kind is the classic bug that lets a fast failure beat a healthy replica. Cancellation lives in the `cancel()` on the winning path backed by `defer cancel()` for the all-fail path; `TestLosersAreCancelled` is deterministic because the losers signal `wg.Done()` and the assertion runs only after `wg.Wait()`. The buffered `results` channel sized to `len(replicas)` is the third invariant: shrink it to unbuffered and the losers block forever on their send after the reader returns, which the race detector and a goroutine-leak check would both catch.

The mistakes to avoid are the two twists stated as failures. Treating "first to respond" as "first healthy" hands the caller fast failures. Forgetting to cancel the losers keeps useless work running and defeats the tail-tolerance the pattern exists for. Both, plus the all-fail aggregation and the empty-list guard, are pinned by the suite; run `go test -race ./...` to confirm the fan-out is leak-free and the cancellation is real.

## Resources

- [The Tail at Scale](https://research.google/pubs/the-tail-at-scale/) — Dean and Barroso, CACM 2013: the paper that motivates racing redundant requests to tolerate slow replicas.
- [`context`](https://pkg.go.dev/context) — `WithCancel` and the `Done()` channel that propagate the loser-cancellation through every replica call.
- [`errors.Join`](https://pkg.go.dev/errors#Join) — the standard-library aggregation used to report every replica's failure when none succeed.

---

Back to [02-combine-cancellation-sources.md](02-combine-cancellation-sources.md) | Next: [Or-Done Channel Pattern](../10-or-done-channel-pattern/10-or-done-channel-pattern.md)
</content>
