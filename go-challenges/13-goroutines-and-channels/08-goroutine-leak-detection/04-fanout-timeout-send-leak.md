# Exercise 4: Fix the Classic Send-After-Receiver-Left Leak

This is the single most common goroutine leak in production Go: a fan-out to N
replicas where the caller returns on the first result (or a timeout), and the
remaining workers block forever trying to send their result on an unbuffered
channel that no longer has a receiver. This exercise reproduces the leak with a real
test, then fixes it with a buffered result channel sized to N.

This module is self-contained: its own `go mod init`, all code inline, its own demo
and tests. It uses only the standard library.

## What you'll build

```text
fanout/                      independent module: example.com/fanout
  go.mod
  fanout.go                  type Replica; QueryFirstNaive (leaks) and QueryFirst (fixed)
  cmd/
    demo/
      main.go                runnable demo: query replicas, first wins
  fanout_test.go             reproduce the naive leak; prove the fix leaks nothing
```

- Files: `fanout.go`, `cmd/demo/main.go`, `fanout_test.go`.
- Implement: `QueryFirstNaive` (unbuffered channel — the bug) and `QueryFirst` (buffered channel sized to `len(replicas)` — the fix), both returning the first successful replica result.
- Test: reproduce the leak (naive version leaves a worker parked on a send after the caller returned); prove the fix returns the winner and leaves no worker behind, even on context timeout.
- Verify: `go test -count=1 -race ./...`

Set up the module:

```bash
mkdir -p go-solutions/13-goroutines-and-channels/08-goroutine-leak-detection/04-fanout-timeout-send-leak/cmd/demo
cd go-solutions/13-goroutines-and-channels/08-goroutine-leak-detection/04-fanout-timeout-send-leak
```

### Why the naive version leaks

The naive fan-out spawns one goroutine per replica, each doing `ch <- v` on an
*unbuffered* channel. The caller does a single receive and returns the first value.
The moment the caller returns, no one will ever receive again — but the other
workers still intend to send. An unbuffered send blocks until a receiver is ready, so
each losing worker parks on that send *forever*. Cancelling a context does nothing:
the send is not selecting on `ctx.Done()`, so the goroutine never learns it should
give up. The receiver left; the senders are stranded.

This is not a rare edge case. It is the default outcome of "fan out, take the first
answer" written the obvious way. Hedged reads across database replicas, scatter-gather
across shards, "call three pricing services and use whichever answers first" — all
leak on every single call unless the send path is fixed.

### The fix: a buffer with a slot per worker

`QueryFirst` changes exactly one thing that matters: the result channel is buffered
with `len(replicas)` slots. Now every worker can complete its send into the buffer
whether or not anyone is receiving, so every worker exits. The caller receives the
first success and returns; the losers deposit their results into buffer slots that
are simply never read, and the whole channel is garbage-collected once the last
reference drops. The cost is bounded and known: N result slots of memory, freed
promptly. (The alternative fix is a context-aware send,
`select { case ch <- v: case <-ctx.Done(): }`, which uses no buffer but requires
every worker to hold and watch the context; the buffered channel is simpler when N is
small and known, which fan-out usually is.)

`QueryFirst` also collects errors: it loops up to `len(replicas)` receives, returns
the first `nil`-error result, and if all fail returns `ErrAllReplicasFailed` wrapping
the last error. On context cancellation it returns `ctx.Err()` and abandons the rest
— safely, because the buffer guarantees they can still send and exit.

Create `fanout.go`:

```go
package fanout

import (
	"context"
	"errors"
	"fmt"
)

// ErrNoReplicas is returned when the replica set is empty.
var ErrNoReplicas = errors.New("fanout: no replicas")

// ErrAllReplicasFailed wraps the last error when every replica failed.
var ErrAllReplicasFailed = errors.New("fanout: all replicas failed")

// Replica is a single backend call, e.g. a query to one database replica.
type Replica func(ctx context.Context) (string, error)

type result struct {
	val string
	err error
}

// QueryFirstNaive fans out to every replica and returns the first success. It
// is BUGGY on purpose: the result channel is unbuffered, so every replica that
// loses the race blocks forever on its send once the caller has returned.
func QueryFirstNaive(ctx context.Context, replicas []Replica) (string, error) {
	if len(replicas) == 0 {
		return "", ErrNoReplicas
	}
	ch := make(chan string) // BUG: unbuffered; losers strand on the send
	for _, r := range replicas {
		go func(r Replica) {
			v, err := r(ctx)
			if err != nil {
				return
			}
			ch <- v // no receiver after the caller returns -> leak
		}(r)
	}
	select {
	case v := <-ch:
		return v, nil
	case <-ctx.Done():
		return "", ctx.Err()
	}
}

// QueryFirst is the fixed fan-out. The result channel is buffered with one slot
// per replica, so every worker can complete its send and exit even after the
// caller has taken the first result or the context has been cancelled.
func QueryFirst(ctx context.Context, replicas []Replica) (string, error) {
	if len(replicas) == 0 {
		return "", ErrNoReplicas
	}
	ch := make(chan result, len(replicas)) // one slot per worker: no send blocks
	for _, r := range replicas {
		go func(r Replica) {
			v, err := r(ctx)
			ch <- result{val: v, err: err}
		}(r)
	}

	var lastErr error
	for range replicas {
		select {
		case res := <-ch:
			if res.err == nil {
				return res.val, nil
			}
			lastErr = res.err
		case <-ctx.Done():
			return "", ctx.Err()
		}
	}
	return "", fmt.Errorf("%w: %v", ErrAllReplicasFailed, lastErr)
}
```

### The runnable demo

Create `cmd/demo/main.go`:

```go
package main

import (
	"context"
	"errors"
	"fmt"
	"time"

	"example.com/fanout"
)

func main() {
	replicas := []fanout.Replica{
		func(ctx context.Context) (string, error) {
			time.Sleep(30 * time.Millisecond)
			return "replica-a", nil
		},
		func(ctx context.Context) (string, error) {
			return "replica-b", nil // fastest
		},
		func(ctx context.Context) (string, error) {
			return "", errors.New("replica-c down")
		},
	}

	got, err := fanout.QueryFirst(context.Background(), replicas)
	if err != nil {
		fmt.Println("query:", err)
		return
	}
	fmt.Println("first result:", got)
}
```

Run it:

```bash
go run ./cmd/demo
```

Expected output:

```
first result: replica-b
```

### The tests

`TestNaiveLeaks` reproduces the bug: a fast replica wins, a slow one is released only
after the caller has returned, and once released it blocks forever on the unbuffered
send — so the goroutine count never returns to baseline. That leaked goroutine is
unreclaimable from outside (the internal channel has no drain), which is precisely
why the naive version is a bug and not a style nit; the test asserts the leak and
documents that it persists. `TestQueryFirstNoLeak` runs the *same* scenario through
the fixed function and asserts the count returns to baseline. `TestQueryFirstTimeout`
proves the fix also survives a context timeout without leaking, and
`TestQueryFirstAllFail` checks the wrapped-error path.

Create `fanout_test.go`:

```go
package fanout

import (
	"context"
	"errors"
	"runtime"
	"testing"
	"time"
)

func countReturnsTo(base int, within time.Duration) bool {
	deadline := time.Now().Add(within)
	for time.Now().Before(deadline) {
		runtime.GC()
		if runtime.NumGoroutine() <= base {
			return true
		}
		time.Sleep(5 * time.Millisecond)
	}
	return false
}

func TestNaiveLeaks(t *testing.T) {
	runtime.GC()
	base := runtime.NumGoroutine()

	release := make(chan struct{})
	replicas := []Replica{
		func(ctx context.Context) (string, error) { return "fast", nil },
		func(ctx context.Context) (string, error) { <-release; return "slow", nil },
	}

	got, err := QueryFirstNaive(context.Background(), replicas)
	if err != nil || got != "fast" {
		t.Fatalf("QueryFirstNaive = %q,%v; want fast,nil", got, err)
	}

	// Release the loser: it returns from its work and then blocks forever on the
	// unbuffered send. There is no way to reclaim it from here.
	close(release)

	if countReturnsTo(base, 200*time.Millisecond) {
		t.Fatal("expected the naive fan-out to leak the losing worker, but it did not")
	}
	t.Log("naive fan-out leaked the losing worker on its send, as designed")
}

func TestQueryFirstNoLeak(t *testing.T) {
	runtime.GC()
	base := runtime.NumGoroutine()

	replicas := []Replica{
		func(ctx context.Context) (string, error) { return "fast", nil },
		func(ctx context.Context) (string, error) {
			time.Sleep(20 * time.Millisecond)
			return "slow", nil
		},
	}

	got, err := QueryFirst(context.Background(), replicas)
	if err != nil || got != "fast" {
		t.Fatalf("QueryFirst = %q,%v; want fast,nil", got, err)
	}

	if !countReturnsTo(base, 2*time.Second) {
		t.Fatalf("QueryFirst leaked: count did not return to baseline %d", base)
	}
}

func TestQueryFirstTimeout(t *testing.T) {
	runtime.GC()
	base := runtime.NumGoroutine()

	release := make(chan struct{})
	replicas := []Replica{
		func(ctx context.Context) (string, error) { <-release; return "a", nil },
		func(ctx context.Context) (string, error) { <-release; return "b", nil },
	}

	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Millisecond)
	defer cancel()

	_, err := QueryFirst(ctx, replicas)
	if !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("QueryFirst error = %v, want context.DeadlineExceeded", err)
	}

	// Let the workers finish their work; the buffered channel means they can
	// send and exit even though the caller already returned.
	close(release)
	if !countReturnsTo(base, 2*time.Second) {
		t.Fatalf("QueryFirst leaked after timeout: baseline %d", base)
	}
}

func TestQueryFirstAllFail(t *testing.T) {
	replicas := []Replica{
		func(ctx context.Context) (string, error) { return "", errors.New("a down") },
		func(ctx context.Context) (string, error) { return "", errors.New("b down") },
	}
	_, err := QueryFirst(context.Background(), replicas)
	if !errors.Is(err, ErrAllReplicasFailed) {
		t.Fatalf("QueryFirst error = %v, want ErrAllReplicasFailed", err)
	}
}

func TestQueryFirstNoReplicas(t *testing.T) {
	_, err := QueryFirst(context.Background(), nil)
	if !errors.Is(err, ErrNoReplicas) {
		t.Fatalf("QueryFirst error = %v, want ErrNoReplicas", err)
	}
}
```

## Review

The naive and fixed functions differ in one line — the buffer size on the result
channel — and that one line is the difference between a per-call leak and a correct
fan-out. `TestNaiveLeaks` proves the bug is real by showing the count never returns to
baseline; `TestQueryFirstNoLeak` and `TestQueryFirstTimeout` prove the fix leaves
nothing behind even when the caller returns early or the deadline fires.

The mistakes to avoid: never send a fan-out result on an unbuffered channel that the
caller can stop receiving from; size the buffer to the number of workers, or make the
send select on `ctx.Done()`. Do not assume a context timeout rescues a blocked send —
it does not, because the blocked send is not watching the context. And when you
reproduce a leak in a test, be honest that the naive path's goroutine is
unreclaimable, rather than pretending a cleanup exists. Run under `-race` to confirm
the workers and the receiver coordinate only through the channel.

## Resources

- [Go Blog: Pipelines and cancellation](https://go.dev/blog/pipelines) — the canonical treatment of fan-out, early return, and why sends need an exit.
- [`context` package](https://pkg.go.dev/context) — `context.DeadlineExceeded`, `ctx.Done`, `ctx.Err`.
- [Go Memory Model: channels](https://go.dev/ref/mem#chan) — why a buffered send can complete without a ready receiver.

---

Back to [00-concepts.md](00-concepts.md) | Prev: [03-homegrown-leak-assert-helper.md](03-homegrown-leak-assert-helper.md) | Next: [05-worker-pool-drain-on-cancel.md](05-worker-pool-drain-on-cancel.md)
