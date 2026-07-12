# Exercise 6: Hedged Reads — Take the First Replica Reply, Cancel the Losers Without Leaking

Tail latency is dominated by the slowest replica, so the standard mitigation is a
*hedged read*: send the read to several replicas at once, take whichever answers
first, and abandon the rest. This is also the single most common place engineers
introduce a goroutine leak — the abandoned replicas block forever on a send nobody
will receive. This module builds `HedgedRead` correctly, with a buffered result
channel and a shared stop signal, and asserts under test that the losers actually
exit.

This module is fully self-contained: its own `go mod init`, all code inline, its
own demo and tests. It imports no other exercise.

## What you'll build

```text
hedgedread/                     module example.com/hedgedread
  go.mod                        go 1.26
  hedgedread.go                 HedgedRead(replicas) (string, error); shared stop; buffered replies
  cmd/
    demo/
      main.go                   one fast + two slow replicas; print the winner
  hedgedread_test.go            fastest-wins, no-goroutine-leak, all-fail aggregation
```

Files: `hedgedread.go`, `cmd/demo/main.go`, `hedgedread_test.go`.
Implement: `HedgedRead(replicas []func(stop <-chan struct{}) (string, error)) (string, error)` — first success wins, losers are cancelled and must not leak.
Test: the fast replica's value wins; goroutine count returns to baseline (losers exit); all-fail returns an aggregated error.
Verify: `go test -count=1 -race ./...`

Set up the module:

```bash
mkdir -p go-solutions/14-select-and-context/01-select-statement-basics/06-hedged-replica-read/cmd/demo
cd go-solutions/14-select-and-context/01-select-statement-basics/06-hedged-replica-read
```

## The two mechanisms that prevent the leak

`HedgedRead` launches one goroutine per replica, each calling the replica function
and sending its `(value, error)` onto a shared `replies` channel. The coordinator
receives from `replies` in a loop: the first success closes the shared `stop`
channel and returns; if a reply is an error, it is recorded and the loop waits for
the next reply. If every replica fails, it returns the aggregated error.

The leak lives in what happens to the *losers* — the replicas still running when
the winner returns. Two mechanisms, used together, guarantee they exit:

- **Buffer `replies` to `len(replicas)`.** This is the non-negotiable one. If
  `replies` were unbuffered, a loser finishing after the coordinator has already
  returned would block forever on `replies <- msg` because no one is receiving
  anymore — a goroutine leak, and therefore a memory leak, since everything the
  goroutine holds stays reachable. Buffering to the replica count means every
  producer can always complete its send and return, even if its result is never
  read.

- **Close a shared `stop` channel on the first success.** Closing `stop` is a
  broadcast: every replica still in flight, if it is watching `stop` in its own
  `select`, unblocks immediately and abandons its work instead of running to
  completion. This is what turns "wait for the slowest replica anyway" into "cancel
  the slow replicas the instant we have an answer" — the actual latency win, and
  the cooperative-cancellation shape that `context.Context` later formalizes.

Buffering alone stops the leak; `stop` alone does not (a replica ignoring `stop`
would still block on an unbuffered send). You want both: `stop` for prompt
cancellation, the buffer as the safety net that lets a loser exit no matter what.

The all-fail path aggregates every replica's error with `errors.Join`, so the
caller can match any of them with `errors.Is`. An empty replica set returns the
`ErrNoReplicas` sentinel rather than blocking forever on an empty `select`.

Create `hedgedread.go`:

```go
package hedgedread

import "errors"

// ErrNoReplicas is returned when HedgedRead is called with no replicas.
var ErrNoReplicas = errors.New("hedgedread: no replicas")

type reply struct {
	value string
	err   error
}

// HedgedRead calls every replica concurrently and returns the first successful
// reply, then closes a shared stop channel so the losing replicas abandon their
// work. The replies channel is buffered to the replica count so a loser that
// finishes after the winner returns can still send and exit rather than leaking.
// If every replica fails, HedgedRead returns their errors joined.
func HedgedRead(replicas []func(stop <-chan struct{}) (string, error)) (string, error) {
	if len(replicas) == 0 {
		return "", ErrNoReplicas
	}

	stop := make(chan struct{})
	replies := make(chan reply, len(replicas)) // buffered: losers never block on send

	for _, r := range replicas {
		go func() {
			v, err := r(stop)
			replies <- reply{value: v, err: err}
		}()
	}

	var errs []error
	for range replicas {
		msg := <-replies
		if msg.err == nil {
			close(stop) // cancel the losers; they unblock and exit
			return msg.value, nil
		}
		errs = append(errs, msg.err)
	}
	return "", errors.Join(errs...)
}
```

## The runnable demo

The demo wires one fast replica and two slow ones. Each slow replica watches
`stop`, so when the fast one wins they cancel instead of sleeping out their delay.

Create `cmd/demo/main.go`:

```go
package main

import (
	"errors"
	"fmt"
	"time"

	"example.com/hedgedread"
)

// replica returns a function that answers after delay, or bails out early if the
// shared stop channel is closed because another replica already won.
func replica(name string, delay time.Duration) func(<-chan struct{}) (string, error) {
	return func(stop <-chan struct{}) (string, error) {
		select {
		case <-time.After(delay):
			return name + ":rows", nil
		case <-stop:
			return "", errors.New(name + ": cancelled")
		}
	}
}

func main() {
	value, err := hedgedread.HedgedRead([]func(<-chan struct{}) (string, error){
		replica("replica-a", 60*time.Millisecond),
		replica("replica-b", 5*time.Millisecond), // fastest
		replica("replica-c", 90*time.Millisecond),
	})
	if err != nil {
		fmt.Println("all replicas failed:", err)
		return
	}
	fmt.Println("winner:", value)
}
```

Run with `go run ./cmd/demo`.

Expected output:

```
winner: replica-b:rows
```

## Tests

`TestHedgedReturnsFastest` wires one fast and several slow replicas and asserts the
fast value wins and the call returns well before the slow delays.
`TestHedgedNoGoroutineLeak` records the goroutine count, runs a hedged read whose losers sleep
far longer than the winner, and asserts the count settles back to baseline — proof
that closing `stop` plus the buffered channel let the losers exit rather than leak.
`TestHedgedAllFail` gives only failing replicas and asserts the joined error
matches each replica's sentinel via `errors.Is`.

Create `hedgedread_test.go`:

```go
package hedgedread

import (
	"errors"
	"runtime"
	"testing"
	"time"
)

func fast(value string) func(<-chan struct{}) (string, error) {
	return func(<-chan struct{}) (string, error) { return value, nil }
}

func slow(value string, delay time.Duration) func(<-chan struct{}) (string, error) {
	return func(stop <-chan struct{}) (string, error) {
		select {
		case <-time.After(delay):
			return value, nil
		case <-stop:
			return "", errors.New("cancelled")
		}
	}
}

func TestHedgedReturnsFastest(t *testing.T) {
	t.Parallel()

	start := time.Now()
	got, err := HedgedRead([]func(<-chan struct{}) (string, error){
		slow("slow-a", time.Second),
		fast("fast"),
		slow("slow-b", time.Second),
	})
	elapsed := time.Since(start)

	if err != nil {
		t.Fatalf("HedgedRead: unexpected error %v", err)
	}
	if got != "fast" {
		t.Fatalf("HedgedRead: got %q, want %q", got, "fast")
	}
	if elapsed > 500*time.Millisecond {
		t.Fatalf("HedgedRead waited on a slow replica: %v", elapsed)
	}
}

func TestHedgedNoGoroutineLeak(t *testing.T) {
	t.Parallel()

	base := runtime.NumGoroutine()

	// The winner returns immediately; the losers would each sleep 10s if not
	// cancelled. If they leaked, the goroutine count would not settle.
	got, err := HedgedRead([]func(<-chan struct{}) (string, error){
		fast("winner"),
		slow("loser-a", 10*time.Second),
		slow("loser-b", 10*time.Second),
	})
	if err != nil || got != "winner" {
		t.Fatalf("HedgedRead: got %q, err %v; want winner, nil", got, err)
	}

	if !settlesTo(base, 2*time.Second) {
		t.Fatalf("goroutines did not return to baseline %d (now %d): losers leaked",
			base, runtime.NumGoroutine())
	}
}

func TestHedgedAllFail(t *testing.T) {
	t.Parallel()

	errA := errors.New("replica a down")
	errB := errors.New("replica b down")
	failWith := func(e error) func(<-chan struct{}) (string, error) {
		return func(<-chan struct{}) (string, error) { return "", e }
	}

	_, err := HedgedRead([]func(<-chan struct{}) (string, error){
		failWith(errA),
		failWith(errB),
	})
	if err == nil {
		t.Fatal("HedgedRead: err = nil with all replicas failing, want joined error")
	}
	if !errors.Is(err, errA) || !errors.Is(err, errB) {
		t.Fatalf("HedgedRead: err = %v, want to wrap both %v and %v", err, errA, errB)
	}
}

func TestHedgedNoReplicas(t *testing.T) {
	t.Parallel()

	_, err := HedgedRead(nil)
	if !errors.Is(err, ErrNoReplicas) {
		t.Fatalf("HedgedRead(nil): err = %v, want ErrNoReplicas", err)
	}
}

// settlesTo polls until NumGoroutine is at or below target, or the deadline passes.
func settlesTo(target int, within time.Duration) bool {
	deadline := time.Now().Add(within)
	for time.Now().Before(deadline) {
		if runtime.NumGoroutine() <= target {
			return true
		}
		time.Sleep(5 * time.Millisecond)
	}
	return runtime.NumGoroutine() <= target
}
```

## Review

The hedged read is correct when the first successful value wins, the losers are
cancelled the moment `stop` closes, and — the assertion that actually matters —
the loser goroutines exit rather than leak. `TestHedgedNoGoroutineLeak` is the
teeth of the module: comment out `close(stop)` and change the buffer to unbuffered,
and it fails, which is the whole point. Keep both defenses: `stop` for prompt
cancellation and the `len(replicas)` buffer as the guarantee that a loser can
always finish its send. The all-fail path joins every error so the caller can match
any of them with `errors.Is`. This cooperative stop-channel shape is exactly what
`context.WithCancel` generalizes in lesson 04.

## Resources

- [errors.Join](https://pkg.go.dev/errors#Join) — aggregating every replica's failure into one matchable error.
- [The Google SRE Book: hedged requests](https://sre.google/sre-book/addressing-cascading-failures/) — why tail-latency hedging is worth the extra load.
- [runtime.NumGoroutine](https://pkg.go.dev/runtime#NumGoroutine) — the leak census this module asserts against.

---

Back to [00-concepts.md](00-concepts.md) | Previous: [05-bounded-result-collector.md](05-bounded-result-collector.md) | Next: [07-drain-without-busyspin.md](07-drain-without-busyspin.md)
