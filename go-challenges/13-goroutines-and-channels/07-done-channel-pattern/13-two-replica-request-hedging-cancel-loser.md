# Exercise 13: Two-Replica Request Hedging, First Wins, Loser Cancelled Without Leak

**Level: Advanced**

To cut tail latency, a hot read is dispatched to two replicas at once and the first response wins. The naive version leaks: the moment you take the winner and return, the losing goroutine is still computing an answer nobody will ever read, and if its result channel is unbuffered its send blocks forever and the goroutine lives until the process dies. This exercise builds the leak-free hedge by combining two idioms from this lesson: a shared `done` channel whose `close` broadcasts cancellation to the loser, and per-replica cap-1 result channels so the loser's send always succeeds into a buffer and the goroutine can return.

This module is self-contained: its own module, a `hedge` package, a demo, and tests.
Nothing here imports another exercise.

## What you'll build

```text
hedge/                       independent module: example.com/hedge
  go.mod                     go 1.26
  hedge.go                   Do runs two replicas, first success wins, loser cancelled via done
  cmd/demo/main.go           runnable demo: replica 0 wins, replica 1 observes done and exits
  hedge_test.go              A-wins, B-wins, both-fail joined error, zero goroutine leaks (goleak)
```

- Files: `hedge.go`, `cmd/demo/main.go`, `hedge_test.go`.
- Implement: `type Result struct { Replica int; Value string }` and `Do(replicas [2]func(done <-chan struct{}) (string, error)) (Result, error)`.
- Test: the gated first replica wins and the loser observes `done` and exits; symmetric when the other replica is gated first; both-fail returns `errors.Join` mentioning both; zero goroutine leaks across all orderings.
- Verify: `go test -count=1 -race ./...`

Set up the module:

```bash
mkdir -p go-solutions/13-goroutines-and-channels/07-done-channel-pattern/13-two-replica-request-hedging-cancel-loser/cmd/demo
cd go-solutions/13-goroutines-and-channels/07-done-channel-pattern/13-two-replica-request-hedging-cancel-loser
go get go.uber.org/goleak
go mod tidy
```

### Why cap-1 buffers and a shared done together, not one or the other

Hedging is the classic tail-latency trick: send the same read to two replicas and use whichever answers first. The correctness problem is not choosing the winner — that is a `select` — it is disposing of the loser. Two failure modes hide here, and each idiom alone fixes only one of them.

Consider the loser's goroutine after the winner is chosen. It is inside the replica function, perhaps mid-fetch. Two things must be true for it to terminate:

1. It must learn it should stop. That is what the shared `done` is for. `Do` creates one `done chan struct{}`, hands each replica the receive-only end (`done <-chan struct{}`, which the replica may only observe, never close), and closes it the instant a winner is chosen. `close` is a broadcast: the loser's `<-done` becomes ready immediately, so a cooperative replica returns promptly.

2. When the loser finally produces its result and tries to deliver it, that delivery must not block. `Do` has already returned; nobody is receiving. If the result channel were unbuffered, the loser's `ch <- outcome` would block forever with no receiver, and the goroutine would leak — one leaked goroutine per hedged request, which under load is a slow-motion memory exhaustion. The fix is one slot of buffer: each replica gets its **own** `make(chan outcome, 1)`. The send lands in the buffer with no reader, the goroutine returns, and the buffered value is garbage-collected with the channel.

Neither idiom is sufficient alone. Without `done`, an uncooperative loser never even tries to stop. Without the cap-1 buffer, a loser that *does* stop still blocks on its final send. You need both: `done` to tell the loser to stop, and the buffer to let its last send succeed so it actually exits.

Each replica delivers on a separate cap-1 channel rather than a shared one because a shared cap-1 channel would only absorb one send; the second replica's send would still block. Two channels, one buffered slot each, and every goroutine — winner and loser — is guaranteed a place to put its result and a clean return.

Create `hedge.go`:

```go
package hedge

import "errors"

// Result is the answer from the replica that responded first.
type Result struct {
	Replica int
	Value   string
}

// outcome is what one replica goroutine delivers on its own cap-1 channel.
type outcome struct {
	replica int
	value   string
	err     error
}

// Do runs both replica functions concurrently and returns the first successful
// result. The moment a winner is chosen it closes the shared done to cancel the
// loser. Each replica delivers on its own cap-1 channel, so the losing goroutine
// can always complete its send into the buffer and return even though nobody will
// ever read its value. If both replicas fail, Do returns errors.Join of both.
func Do(replicas [2]func(done <-chan struct{}) (string, error)) (Result, error) {
	done := make(chan struct{})
	// Closing done cancels the loser on the winning path, and is a harmless
	// no-op on the both-failed path (both goroutines have already returned).
	defer close(done)

	// Each goroutine owns one cap-1 channel. cap 1 is load-bearing: it lets the
	// loser's send succeed with no reader, so the loser goroutine never blocks
	// on an abandoned send and leaks.
	var chans [2]chan outcome
	for i := range 2 {
		chans[i] = make(chan outcome, 1)
		go func(i int) {
			v, err := replicas[i](done)
			chans[i] <- outcome{replica: i, value: v, err: err}
		}(i)
	}

	var errs [2]error
	for range 2 {
		var o outcome
		select {
		case o = <-chans[0]:
		case o = <-chans[1]:
		}
		if o.err == nil {
			// First success wins; the deferred close(done) cancels the loser.
			return Result{Replica: o.replica, Value: o.value}, nil
		}
		errs[o.replica] = o.err
	}

	// Both failed. Join in replica-index order for a deterministic message.
	return Result{}, errors.Join(errs[0], errs[1])
}
```

### The runnable demo

Replica 0 answers immediately and wins. Replica 1 is the slow replica that only produces a value after it observes `done`, so replica 0 always wins and the demo is deterministic. The demo waits for the loser to observe `done` before reporting, which is exactly the property the pattern guarantees.

Create `cmd/demo/main.go`:

```go
package main

import (
	"errors"
	"fmt"

	"example.com/hedge"
)

func main() {
	// Replica 0 answers immediately and wins. Replica 1 is the slow replica: it
	// blocks until it observes done (closed when replica 0 wins), then exits.
	// Because replica 1 never has a value ready before done fires, replica 0
	// always wins, so this demo is deterministic.
	loserCancelled := make(chan struct{})

	replicas := [2]func(done <-chan struct{}) (string, error){
		func(done <-chan struct{}) (string, error) {
			return "payload-from-replica-0", nil
		},
		func(done <-chan struct{}) (string, error) {
			<-done
			close(loserCancelled)
			return "", errors.New("cancelled after loss")
		},
	}

	res, err := hedge.Do(replicas)
	if err != nil {
		fmt.Println("both replicas failed:", err)
		return
	}

	<-loserCancelled // prove the loser observed done before we report

	fmt.Printf("winner: replica %d\n", res.Replica)
	fmt.Printf("value: %s\n", res.Value)
	fmt.Println("loser observed done and exited")
}
```

Run it:

```bash
go run ./cmd/demo
```

Expected output:

```
winner: replica 0
value: payload-from-replica-0
loser observed done and exited
```

### Tests

`TestReplicaAWinsLoserBObservesDone` gives replica A a value it can return immediately and makes replica B a loser with no value ready, so B can only exit by observing `done`; it asserts A's result comes back and B's `<-done` unblocks. `TestReplicaBWinsLoserAObservesDone` is the symmetric case with the roles swapped, proving the winner is chosen by which replica has an answer, not by position. `TestBothFailJoinsErrors` makes both replicas fail and asserts the returned error mentions both messages, confirming the `errors.Join` fallback. `TestMain` wraps the whole package in `goleak.VerifyTestMain`, so every one of these orderings must leave zero stray goroutines — a leaked loser fails the suite, which is the whole reason for the cap-1 buffers plus `done` cancellation.

Create `hedge_test.go`:

```go
package hedge

import (
	"errors"
	"strings"
	"testing"
	"time"

	"go.uber.org/goleak"
)

// TestMain runs the whole package under goleak so every ordering below must
// leave zero stray goroutines. A leaked loser fails the suite, which is the
// entire point of the cap-1 buffers plus done cancellation.
func TestMain(m *testing.M) {
	goleak.VerifyTestMain(m)
}

func TestReplicaAWinsLoserBObservesDone(t *testing.T) {
	t.Parallel()

	bObservedDone := make(chan struct{})

	replicas := [2]func(done <-chan struct{}) (string, error){
		func(done <-chan struct{}) (string, error) {
			return "value-A", nil
		},
		func(done <-chan struct{}) (string, error) {
			// Loser: has no value ready, so it only exits when done fires.
			<-done
			close(bObservedDone)
			return "", errors.New("B cancelled")
		},
	}

	res, err := Do(replicas)
	if err != nil {
		t.Fatalf("Do returned error: %v", err)
	}
	if res.Replica != 0 || res.Value != "value-A" {
		t.Fatalf("res = %+v, want {Replica:0 Value:value-A}", res)
	}

	select {
	case <-bObservedDone:
	case <-time.After(2 * time.Second):
		t.Fatal("loser B never observed done after A won")
	}
}

func TestReplicaBWinsLoserAObservesDone(t *testing.T) {
	t.Parallel()

	aObservedDone := make(chan struct{})

	replicas := [2]func(done <-chan struct{}) (string, error){
		func(done <-chan struct{}) (string, error) {
			<-done
			close(aObservedDone)
			return "", errors.New("A cancelled")
		},
		func(done <-chan struct{}) (string, error) {
			return "value-B", nil
		},
	}

	res, err := Do(replicas)
	if err != nil {
		t.Fatalf("Do returned error: %v", err)
	}
	if res.Replica != 1 || res.Value != "value-B" {
		t.Fatalf("res = %+v, want {Replica:1 Value:value-B}", res)
	}

	select {
	case <-aObservedDone:
	case <-time.After(2 * time.Second):
		t.Fatal("loser A never observed done after B won")
	}
}

func TestBothFailJoinsErrors(t *testing.T) {
	t.Parallel()

	replicas := [2]func(done <-chan struct{}) (string, error){
		func(done <-chan struct{}) (string, error) {
			return "", errors.New("replica-0 down")
		},
		func(done <-chan struct{}) (string, error) {
			return "", errors.New("replica-1 down")
		},
	}

	res, err := Do(replicas)
	if err == nil {
		t.Fatalf("Do returned nil error, want joined failure; res = %+v", res)
	}
	msg := err.Error()
	if !strings.Contains(msg, "replica-0 down") || !strings.Contains(msg, "replica-1 down") {
		t.Fatalf("joined error %q does not mention both replicas", msg)
	}
}
```

## Review

`Do` is correct when it returns the first successful result, cancels the loser the instant a winner is chosen, and leaves no goroutine behind on any ordering. The winner is whichever replica's `outcome` the `select` reads first; the deferred `close(done)` then broadcasts cancellation, and because each replica owns a cap-1 channel, the loser's final send lands in its buffer with no reader and the goroutine returns cleanly. The two gated tests prove the winner is content-determined and symmetric, the both-fail test pins the `errors.Join` fallback, and `goleak.VerifyTestMain` is the assertion that matters most: it fails the suite if even one loser leaks, which is precisely the bug this pattern prevents — an unbuffered result channel plus no cancellation signal leaks one goroutine per hedged request, and under production read load that is a heap that grows until the process is OOM-killed. Run `go test -count=2 -race` to confirm the winner selection and the loser's abandoned send never race and never leak.

## Resources

- [Go Blog: Go Concurrency Patterns — Pipelines and cancellation](https://go.dev/blog/pipelines) -- the `done`-channel cancellation model this hedge is built on.
- [pkg.go.dev: errors.Join](https://pkg.go.dev/errors#Join) -- how the both-failed path combines two replica errors into one.
- [pkg.go.dev: go.uber.org/goleak](https://pkg.go.dev/go.uber.org/goleak) -- the leak detector that turns "the loser exited" into a test assertion.
- [Go Language Spec: Select statements](https://go.dev/ref/spec#Select_statements) -- why the first ready `outcome` wins and how the buffered send never blocks.

---

Back to [00-concepts.md](00-concepts.md) | Prev: [12-lock-lease-renewer-quit-ack.md](12-lock-lease-renewer-quit-ack.md) | Next: [14-singleflight-cache-stampede-done-broadcast.md](14-singleflight-cache-stampede-done-broadcast.md)
