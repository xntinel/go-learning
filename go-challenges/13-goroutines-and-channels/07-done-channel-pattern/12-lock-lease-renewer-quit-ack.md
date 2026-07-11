# Exercise 12: Distributed-Lock Lease Renewer With Quit/Ack Shutdown

**Level: Intermediate**

A service holds a distributed lock by renewing its lease on a fixed interval from a background goroutine. Before it can safely release the lock it must guarantee the renewer has *fully stopped* — a renewal that fires after release would extend a lock the service no longer owns, and another node could then be holding it too. The naive `close(done)` and return leaves a window: the owner has signalled stop, but a tick already in flight can still call renew. This exercise closes that window with a two-channel handshake — the owner closes `done` to signal stop, and the renewer closes a separate `stopped` channel to acknowledge it has exited — so "no renewal after Stop returns" becomes a hard ordering guarantee rather than a hope.

This module is self-contained: its own module, a `lease` package, a demo, and tests.
Nothing here imports another exercise.

## What you'll build

```text
lease/                       independent module: example.com/lease
  go.mod                     go 1.26 (requires go.uber.org/goleak for the leak check)
  lease.go                   type Renewer; New, Start, Stop (quit/ack handshake), Renewals
  cmd/demo/main.go           runnable demo: renew a lease, Stop, prove the count is frozen
  lease_test.go              renews-once, stop-freezes-count, stop-idempotent, stop-idle, no-leak
```

- Files: `lease.go`, `cmd/demo/main.go`, `lease_test.go`.
- Implement: `New(interval, renew) *Renewer`, `(*Renewer).Start()`, `(*Renewer).Stop()`, `(*Renewer).Renewals() int`.
- Test: a short-interval renewer renews at least once (observed via a notify channel, not a sleep); `Stop` blocks until the goroutine acks, so the count is frozen the instant it returns; `Stop` twice does not panic; an idle renewer stops promptly; goleak proves no goroutine or ticker leaks.
- Verify: `go test -count=1 -race ./...`

Set up the module:

```bash
mkdir -p ~/go-exercises/lease/cmd/demo
cd ~/go-exercises/lease
go mod init example.com/lease
go get go.uber.org/goleak
```

### Why a plain done channel is not enough here

Every other worker in this lesson stops on `close(done)` and that is sufficient, because the caller does not care exactly when the goroutine's last unit of work completed — draining a few extra queued items during shutdown is harmless. Lease renewal is different: the *ordering* between "the owner has stopped renewing" and "the owner releases the lock" is a correctness invariant. If renew can fire even once after the release path begins, the lease is extended past the point the service believes it owns the lock, and a second node that acquired the lease now shares it. That is the split-brain a distributed lock exists to prevent.

So `Stop` cannot just signal and return. It must *wait for proof* that the renew goroutine has left its loop. That proof is a second channel:

1. The owner calls `Stop`, which closes `done` under a `sync.Once` (the single close, guarded so a double `Stop` cannot panic on "close of closed channel").
2. The renew goroutine's `select` observes the closed `done`, returns from its loop, and — via `defer close(stopped)` — closes the `stopped` channel as the very last thing it does.
3. `Stop` blocks on `<-stopped`. A receive on a closed channel returns immediately, so `Stop` unblocks exactly when — and not before — the goroutine has provably exited.

The `defer close(stopped)` placement matters: because it is deferred, it runs after `defer ticker.Stop()` and after the loop's final `renew` call has returned. When `Stop`'s `<-stopped` completes, there is no in-flight tick and no live goroutine, so `Renewals()` is frozen. That is the handshake: `done` is the quit, `stopped` is the ack, and `Stop` is synchronous on the ack.

Because `stopped` is closed (not sent to), a second `Stop` also receives from it without blocking, and the `sync.Once` swallows the second close attempt — `Stop` is idempotent and panic-free on every call.

Create `lease.go`:

```go
package lease

import (
	"sync"
	"sync/atomic"
	"time"
)

// Renewer holds a distributed lock by calling renew on a fixed interval from
// its own goroutine. Shutdown is a two-channel handshake: the owner closes done
// to request stop, and the renew goroutine closes stopped to acknowledge that it
// has exited. Stop blocks on that acknowledgement, so "no renewal after Stop
// returns" is a hard ordering guarantee, not a hope.
type Renewer struct {
	interval time.Duration
	renew    func()

	done    chan struct{} // owner closes to request stop
	stopped chan struct{} // renew goroutine closes to acknowledge exit
	once    sync.Once     // guards the single close(done)

	count atomic.Int64
}

// New builds a Renewer that will call renew every interval once Start runs.
func New(interval time.Duration, renew func()) *Renewer {
	return &Renewer{
		interval: interval,
		renew:    renew,
		done:     make(chan struct{}),
		stopped:  make(chan struct{}),
	}
}

// Start launches the renew loop on its own goroutine.
func (r *Renewer) Start() {
	go r.loop()
}

func (r *Renewer) loop() {
	// The ack: closing stopped is the last thing the goroutine does, so an
	// observer of stopped knows the goroutine has fully returned.
	defer close(r.stopped)

	t := time.NewTicker(r.interval)
	defer t.Stop()

	for {
		select {
		case <-t.C:
			r.count.Add(1)
			r.renew()
		case <-r.done:
			return
		}
	}
}

// Stop closes done once and blocks until the renew goroutine closes stopped.
// After Stop returns, renew is guaranteed never to be called again: the ack
// proves the goroutine has exited its select loop. Stop is idempotent and
// panic-free because sync.Once guards the close and a receive on an
// already-closed stopped channel never blocks.
func (r *Renewer) Stop() {
	r.once.Do(func() { close(r.done) })
	<-r.stopped
}

// Renewals reports how many times renew has been called.
func (r *Renewer) Renewals() int {
	return int(r.count.Load())
}
```

### The runnable demo

The demo renews a lease on a 20 ms interval and uses a buffered notify channel to observe the first renewal deterministically — no sleeping to "wait for a tick". It then calls `Stop`, snapshots `Renewals()`, sleeps well past several intervals, and confirms the count did not move. Because the printed lines are booleans about invariants (not raw counts, which are timing-dependent), the output is identical on every run.

Create `cmd/demo/main.go`:

```go
package main

import (
	"fmt"
	"sync"
	"time"

	"example.com/lease"
)

func main() {
	// first is a buffered notify channel: the renew callback signals it exactly
	// once, on its first invocation. Waiting on it is a deterministic way to
	// observe "at least one renewal happened" without sleeping and guessing.
	first := make(chan struct{}, 1)
	var once sync.Once
	r := lease.New(20*time.Millisecond, func() {
		once.Do(func() { first <- struct{}{} })
	})
	r.Start()

	<-first // proceed only after the renewer has renewed the lease at least once
	r.Stop()
	n := r.Renewals()

	// Stop returned only after the ack, so the goroutine has exited and the
	// count cannot grow. Sleeping past several would-be intervals proves it.
	time.Sleep(60 * time.Millisecond)
	frozen := r.Renewals() == n

	// Stop is idempotent: a second call must not panic on a double close.
	r.Stop()

	fmt.Println("renewed at least once:", n >= 1)
	fmt.Println("frozen after Stop:", frozen)
	fmt.Println("second Stop safe:", true)
}
```

Run it:

```bash
go run ./cmd/demo
```

Expected output:

```
renewed at least once: true
frozen after Stop: true
second Stop safe: true
```

### Tests

`TestRenewsAtLeastOnce` starts a 1 ms renewer whose callback signals a buffered channel on its first call; the test blocks on that channel rather than sleeping, so it never races the timer, then asserts `Renewals() >= 1`. `TestStopFreezesRenewals` is the ordering invariant, and it is checked deterministically instead of with a sleep: the renew callback parks itself in-flight on a `release` channel, so at the moment `Stop` is called a renewal is provably still running. The test then asserts `Stop` does **not** return within 100 ms — it is blocked on the ack — and only unblocks after `release` lets the in-flight renewal finish and the goroutine closes `stopped`. A fire-and-forget `Stop` that merely closed `done` and returned would unblock immediately while the renewal was still live, which is precisely the split-brain window, so this test fails against that broken implementation. `TestStopIdempotent` calls `Stop` twice and asserts no panic (the `sync.Once`-guarded close). `TestStopBeforeAnyRenewal` stops an hour-interval renewer that never ticks and asserts `Stop` still returns promptly via the `done` branch with a zero count. The `goleak.VerifyTestMain` `TestMain` fails the package if any renew goroutine or ticker outlives its test.

Create `lease_test.go`:

```go
package lease

import (
	"sync"
	"testing"
	"time"

	"go.uber.org/goleak"
)

// TestMain proves the ticker and the renew goroutine do not outlive the tests:
// every Renewer that is Started must be Stopped, or goleak flags the leak.
func TestMain(m *testing.M) {
	goleak.VerifyTestMain(m)
}

// TestRenewsAtLeastOnce observes a renewal deterministically. The renew callback
// signals a buffered channel on its first call; the test blocks on that channel
// instead of sleeping, so it never races the timer.
func TestRenewsAtLeastOnce(t *testing.T) {
	t.Parallel()

	first := make(chan struct{}, 1)
	var once sync.Once
	r := New(time.Millisecond, func() {
		once.Do(func() { first <- struct{}{} })
	})
	r.Start()
	defer r.Stop()

	select {
	case <-first:
	case <-time.After(2 * time.Second):
		t.Fatal("renewer did not renew within 2s")
	}
	if r.Renewals() < 1 {
		t.Fatalf("Renewals() = %d, want >= 1", r.Renewals())
	}
}

// TestStopFreezesRenewals is the ordering invariant, checked deterministically.
// A renew call is parked in-flight before Stop is invoked; Stop must not return
// until that renew has completed and the goroutine has acked by closing stopped.
// A fire-and-forget Stop that only closed done and returned would unblock early
// here, while a renewal is still running -- exactly the split-brain window. The
// test forces that window to exist and asserts Stop refuses to cross it.
func TestStopFreezesRenewals(t *testing.T) {
	t.Parallel()

	entered := make(chan struct{})
	release := make(chan struct{})
	var once sync.Once
	r := New(time.Millisecond, func() {
		once.Do(func() {
			close(entered) // the first renew is now in flight
			<-release      // park here to simulate a renewal in progress
		})
	})
	r.Start()

	<-entered // a renew is provably in flight and parked inside the callback

	stopReturned := make(chan struct{})
	go func() {
		r.Stop()
		close(stopReturned)
	}()

	// Stop must block while the renew is in flight: the ack (stopped closed)
	// cannot happen until the goroutine leaves renew and its select loop.
	select {
	case <-stopReturned:
		t.Fatal("Stop returned while a renewal was still in flight")
	case <-time.After(100 * time.Millisecond):
	}

	close(release) // let the in-flight renewal finish

	select {
	case <-stopReturned:
	case <-time.After(2 * time.Second):
		t.Fatal("Stop did not return after the in-flight renewal completed")
	}
}

// TestStopIdempotent calls Stop twice. The sync.Once-guarded close means the
// second call neither panics on a double close nor blocks: a receive on the
// already-closed stopped channel returns immediately.
func TestStopIdempotent(t *testing.T) {
	t.Parallel()

	r := New(time.Millisecond, func() {})
	r.Start()

	r.Stop()
	r.Stop() // must not panic
}

// TestStopBeforeAnyRenewal stops a renewer with a long interval before it ever
// ticks. Stop must still return promptly via the done branch, and the count is
// zero.
func TestStopBeforeAnyRenewal(t *testing.T) {
	t.Parallel()

	r := New(time.Hour, func() { t.Error("renew should not have fired") })
	r.Start()

	done := make(chan struct{})
	go func() {
		r.Stop()
		close(done)
	}()
	select {
	case <-done:
	case <-time.After(2 * time.Second):
		t.Fatal("Stop did not return for an idle renewer")
	}
	if r.Renewals() != 0 {
		t.Fatalf("Renewals() = %d, want 0", r.Renewals())
	}
}
```

## Review

The renewer is correct when `Stop` establishes a happens-before edge: every call to `renew` completes before `Stop` returns, so the renewal count is frozen the instant control returns to the caller and the service can release the lock knowing no further renewal is possible. The invariant is carried by the quit/ack handshake — `done` closed under `sync.Once` is the quit, `stopped` closed by a `defer` in the goroutine is the ack, and `Stop`'s `<-stopped` is a synchronous wait on that ack. `TestStopFreezesRenewals` proves it deterministically: it parks a renewal in-flight, then shows `Stop` refuses to return until that renewal completes and the goroutine closes `stopped`, which is only true because `Stop` is a synchronous wait on the ack. The production bug this prevents is the classic distributed-lock split-brain — a fire-and-forget `close(done)` lets a tick already in flight extend the lease after the owner thinks it released the lock, so two nodes hold it at once. `defer ticker.Stop()` plus the goleak `TestMain` close the resource side: no leaked timer, no leaked goroutine. Run `go test -count=2 -race` to confirm the `atomic.Int64` writer and the `Renewals()` readers stay race-free across repeats.

## Resources

- [Go Blog: Go Concurrency Patterns — Pipelines and cancellation](https://go.dev/blog/pipelines) -- the done-channel cancellation idiom this handshake extends with an acknowledgement.
- [pkg.go.dev: sync.Once](https://pkg.go.dev/sync#Once) -- guarantees the single `close(done)` so a double `Stop` cannot panic.
- [pkg.go.dev: time.NewTicker and Ticker.Stop](https://pkg.go.dev/time#NewTicker) -- why the renew loop must `defer ticker.Stop()` on exit.
- [go.uber.org/goleak](https://pkg.go.dev/go.uber.org/goleak) -- fails the test binary if the renew goroutine or ticker outlives a test.

---

Back to [00-concepts.md](00-concepts.md) | Prev: [11-rolling-deploy-health-gate-preempt.md](11-rolling-deploy-health-gate-preempt.md) | Next: [13-two-replica-request-hedging-cancel-loser.md](13-two-replica-request-hedging-cancel-loser.md)
