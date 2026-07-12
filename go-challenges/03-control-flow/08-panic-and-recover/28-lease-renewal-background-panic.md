# Exercise 28: Distributed Lease with Background Renewal: Panic Isolation in Holder

**Nivel: Avanzado** — validacion normal (tabla de casos, incluye borde o concurrencia).

Holding a distributed lease against etcd, Consul, or ZooKeeper - the
mechanism behind leader election and exclusive-work claims - requires a
background goroutine that keeps renewing the lease before its TTL expires,
completely independent of whatever the foreground task holding the lease is
doing. If that renewal goroutine panics - a nil response from a flaky
client library, a malformed TTL field - and nothing catches it, the whole
process crashes, which is actually the *safe* failure mode; the genuinely
dangerous bug is a renewal panic that somehow leaves the lease looking
"held" from the foreground task's point of view while the backend has
already expired it, or a shutdown path that hangs forever waiting for a
goroutine that already died. This module builds `Holder`, whose background
goroutine recovers its own renewal panics, releases the lease exactly once,
and exposes that loss to the foreground task without ever risking a
deadlock. It is fully self-contained: its own module, demo, and tests.

## What you'll build

```text
lease/                       independent module: example.com/lease
  go.mod                      go 1.24
  lease.go                     RenewFunc, ReleaseFunc, Holder, Start, Tick, Stop, Lost, ReleaseErr
  cmd/
    demo/
      main.go                 runnable demo: 2 clean renewals, 3rd renewal panics, later ticks report stopped
  lease_test.go                 clean renewal, panic releases + stops without deadlock, release-panic kept separate, idempotent Stop
```

Files: `lease.go`, `cmd/demo/main.go`, `lease_test.go`.
Implement: `Start(renew RenewFunc, release ReleaseFunc) *Holder` launching a single background goroutine driven by `Tick()` calls (not a wall-clock timer); a renewal panic recovers, marks the holder lost, releases the lease, and exits; `Stop()` triggers a graceful release and blocks until the goroutine has fully exited.
Test: several clean ticks with a normal `Stop`; a renewal panic where a subsequent `Tick` reports the holder already stopped without deadlocking (bounded by a timeout); both `renew` and `release` panicking, asserting `Lost` and `ReleaseErr` stay in separate fields; calling `Stop` twice safely.
Verify: `go test -count=1 -race ./...`

Set up the module:

```bash
mkdir -p go-solutions/03-control-flow/08-panic-and-recover/28-lease-renewal-background-panic/cmd/demo
cd go-solutions/03-control-flow/08-panic-and-recover/28-lease-renewal-background-panic
go mod edit -go=1.24
```

### Why Tick drives renewal explicitly, and why release and lost failures are tracked in separate fields

A real lease holder renews on a wall-clock interval, but a wall-clock timer
makes both the demo and the tests dependent on real elapsed time - flaky
under load, and impossible to pin an exact "renewal #3 panics" moment to
without a fragile `time.Sleep`. `Holder` instead exposes `Tick()`, an
explicit, synchronous handshake with the single background goroutine: a
`Tick()` call only returns once the background goroutine has accepted that
tick (or has already stopped), and because that goroutine processes exactly
one tick at a time before it can accept the next, the renewal side effects
of tick *N* are guaranteed to have fully completed before a `Tick()` call
for tick *N+1* can return. This gives the demo and tests a fully
deterministic sequence of renewal outcomes to assert on, with no clock
involved anywhere.

The background goroutine (`run`) is the only place that ever touches the
renew or release functions, and it is where the two recover boundaries
live: `renewOnce` isolates a renewal panic and turns it into an error that
`markLost` records, and `releaseSafe` isolates the release call itself,
because "release the lease so no one thinks we still hold it" must not
silently become a second, unhandled panic if the release call itself has a
bug. Critically, a release-time panic is stored in a *different* field
(`releaseErr`) from the renewal failure that triggered the release
(`lostErr`) - if they shared one field, whichever happened to run last
would silently erase the other, and an operator debugging "why did we lose
the lease" would see a cleanup-time symptom instead of the actual root
cause. `Stop` uses a `sync.Once` around closing the internal `stop` channel
specifically so it is safe to call more than once, including after the
lease was already lost on its own - closing an already-closed channel
panics, and a shutdown path is exactly the kind of code that legitimately
gets called from more than one place (an explicit shutdown *and* a deferred
safety-net shutdown, say).

Create `lease.go`:

```go
package lease

import (
	"fmt"
	"sync"
)

// RenewFunc performs one renewal attempt against the lease backend (etcd,
// Consul, ZooKeeper, ...). It may return an ordinary error, or - the case
// this package defends against - panic, because a renewal call frequently
// touches a client library response that can turn out nil on a transient
// condition the caller did not anticipate.
type RenewFunc func() error

// ReleaseFunc releases the lease against the backend.
type ReleaseFunc func() error

// Holder holds a distributed lease and renews it in a background goroutine
// driven by explicit calls to Tick, rather than a wall-clock timer, so
// renewal is fully deterministic to drive and test. If a renewal panics,
// the background goroutine recovers it itself - recover has goroutine
// scope, so the caller of Start is never on this goroutine's stack and
// could not catch it any other way - releases the lease exactly once, and
// marks the holder lost so the foreground task, which must stop treating
// the lease as held, can observe that through Lost without racing or
// deadlocking against the background goroutine.
type Holder struct {
	renew   RenewFunc
	release ReleaseFunc

	tick chan struct{}
	stop chan struct{}
	done chan struct{}

	stopOnce sync.Once

	mu         sync.Mutex
	lost       bool
	lostErr    error
	released   bool
	releaseErr error
}

// Start begins holding a lease: it launches the single background goroutine
// that owns all renewal and release activity for this holder.
func Start(renew RenewFunc, release ReleaseFunc) *Holder {
	h := &Holder{
		renew:   renew,
		release: release,
		tick:    make(chan struct{}),
		stop:    make(chan struct{}),
		done:    make(chan struct{}),
	}
	go h.run()
	return h
}

func (h *Holder) run() {
	defer close(h.done)
	for {
		select {
		case <-h.tick:
			if err := renewOnce(h.renew); err != nil {
				h.markLost(err)
				h.releaseOnce()
				return
			}
		case <-h.stop:
			h.releaseOnce()
			return
		}
	}
}

func renewOnce(renew RenewFunc) (err error) {
	defer func() {
		if r := recover(); r != nil {
			if e, ok := r.(error); ok {
				err = fmt.Errorf("renew panicked: %w", e)
				return
			}
			err = fmt.Errorf("renew panicked: %v", r)
		}
	}()
	return renew()
}

func releaseSafe(release ReleaseFunc) (err error) {
	defer func() {
		if r := recover(); r != nil {
			if e, ok := r.(error); ok {
				err = fmt.Errorf("release panicked: %w", e)
				return
			}
			err = fmt.Errorf("release panicked: %v", r)
		}
	}()
	return release()
}

// releaseOnce runs the release exactly once, no matter how many code paths
// in run reach it (a lost renewal, an explicit Stop). Its own panic is
// isolated and recorded separately from any renewal failure, in
// releaseErr, so a cleanup-time panic can never silently overwrite the
// reason the lease was lost in the first place.
func (h *Holder) releaseOnce() {
	h.mu.Lock()
	already := h.released
	h.released = true
	h.mu.Unlock()
	if already {
		return
	}
	if err := releaseSafe(h.release); err != nil {
		h.mu.Lock()
		h.releaseErr = err
		h.mu.Unlock()
	}
}

func (h *Holder) markLost(err error) {
	h.mu.Lock()
	defer h.mu.Unlock()
	if !h.lost {
		h.lost = true
		h.lostErr = err
	}
}

// Tick drives one renewal attempt on the background goroutine and blocks
// until it has been accepted. It reports false without blocking forever if
// the holder has already stopped - lost the lease or been explicitly
// Stopped - so the foreground task knows not to keep ticking a dead
// holder.
func (h *Holder) Tick() bool {
	select {
	case h.tick <- struct{}{}:
		return true
	case <-h.done:
		return false
	}
}

// Stop asks the background goroutine to release the lease and exit, and
// waits for it to fully finish before returning - guaranteeing the lease is
// released and the goroutine has exited, with no orphaned lock and no
// leaked goroutine. Safe to call more than once, or after the lease was
// already lost to a renewal panic.
func (h *Holder) Stop() {
	h.stopOnce.Do(func() { close(h.stop) })
	<-h.done
}

// Lost reports whether the lease was lost because a renewal attempt failed
// or panicked, and the error explaining why. It returns false after a
// deliberate Stop with no renewal failure.
func (h *Holder) Lost() (bool, error) {
	h.mu.Lock()
	defer h.mu.Unlock()
	return h.lost, h.lostErr
}

// ReleaseErr reports an error from releasing the lease itself, if release
// failed or panicked. It is kept separate from Lost so a cleanup-time
// failure can never be confused with, or lose, the renewal failure that
// triggered it.
func (h *Holder) ReleaseErr() error {
	h.mu.Lock()
	defer h.mu.Unlock()
	return h.releaseErr
}
```

### The runnable demo

Two clean renewals happen first; the third panics on a nil-pointer field
access before it can print anything, so `run` releases the lease and exits.
The remaining ticks report that the holder already stopped.

Create `cmd/demo/main.go`:

```go
package main

import (
	"fmt"

	"example.com/lease"
)

func main() {
	renewCount := 0
	renew := func() error {
		renewCount++
		if renewCount == 3 {
			var resp *struct{ TTL int }
			fmt.Println(resp.TTL) // nil pointer dereference
		}
		fmt.Printf("renew #%d: lease extended\n", renewCount)
		return nil
	}
	released := false
	release := func() error {
		released = true
		fmt.Println("lease released")
		return nil
	}

	h := lease.Start(renew, release)

	for i := 1; i <= 5; i++ {
		if !h.Tick() {
			fmt.Printf("tick %d skipped: holder already stopped\n", i)
		}
	}

	h.Stop()
	lost, err := h.Lost()
	fmt.Println("lost:", lost)
	fmt.Println("err:", err)
	fmt.Println("release err:", h.ReleaseErr())
	fmt.Println("released:", released)
}
```

Run it:

```bash
go run ./cmd/demo
```

Expected output:

```
renew #1: lease extended
renew #2: lease extended
lease released
tick 4 skipped: holder already stopped
tick 5 skipped: holder already stopped
lost: true
err: renew panicked: runtime error: invalid memory address or nil pointer dereference
release err: <nil>
released: true
```

### Tests

`TestHolderReleasesLeaseWhenRenewPanics` drives one renewal panic and then
proves, with a bounded timeout on a second `Tick` call, that the holder
stops and reports `false` rather than deadlocking.
`TestHolderReleaseErrKeptSeparateFromRenewLoss` makes both `renew` and
`release` panic and asserts each error lands in its own field.
`TestHolderStopIsIdempotent` confirms calling `Stop` twice never panics on a
double `close`.

Create `lease_test.go`:

```go
package lease

import (
	"errors"
	"strings"
	"sync"
	"testing"
	"time"
)

func TestHolderRenewsSuccessfully(t *testing.T) {
	count := 0
	renew := func() error { count++; return nil }
	released := false
	release := func() error { released = true; return nil }

	h := Start(renew, release)
	for i := 0; i < 3; i++ {
		if !h.Tick() {
			t.Fatalf("Tick %d = false, want true", i)
		}
	}
	h.Stop()

	if count != 3 {
		t.Fatalf("count = %d, want 3", count)
	}
	if lost, err := h.Lost(); lost || err != nil {
		t.Fatalf("Lost() = (%v, %v), want (false, nil)", lost, err)
	}
	if !released {
		t.Fatal("release was never called by Stop")
	}
}

func TestHolderReleasesLeaseWhenRenewPanics(t *testing.T) {
	renew := func() error { panic(errors.New("etcd lease not found")) }
	var mu sync.Mutex
	released := false
	release := func() error {
		mu.Lock()
		released = true
		mu.Unlock()
		return nil
	}

	h := Start(renew, release)

	if ok := h.Tick(); !ok {
		t.Fatal("first Tick() = false, want true (accepted before renew runs)")
	}

	// The background goroutine releases and exits on its own after the
	// panic; a second Tick must report false, not deadlock.
	result := make(chan bool, 1)
	go func() { result <- h.Tick() }()
	select {
	case ok := <-result:
		if ok {
			t.Fatal("second Tick() = true, want false: holder should have stopped")
		}
	case <-time.After(2 * time.Second):
		t.Fatal("second Tick() deadlocked: holder never released and stopped")
	}

	h.Stop() // must return immediately even though the goroutine already exited

	lost, err := h.Lost()
	if !lost {
		t.Fatal("Lost() = false, want true")
	}
	if err == nil || !strings.Contains(err.Error(), "etcd lease not found") {
		t.Fatalf("err = %v, want it to wrap the original renew panic", err)
	}

	mu.Lock()
	defer mu.Unlock()
	if !released {
		t.Fatal("release was never called after renew panicked")
	}
}

func TestHolderReleaseErrKeptSeparateFromRenewLoss(t *testing.T) {
	renew := func() error { panic("connection reset") }
	release := func() error { panic(errors.New("release rpc timed out")) }

	h := Start(renew, release)
	h.Tick()
	h.Stop()

	lost, lostErr := h.Lost()
	if !lost || lostErr == nil || !strings.Contains(lostErr.Error(), "connection reset") {
		t.Fatalf("Lost() = (%v, %v), want (true, renew panic)", lost, lostErr)
	}
	relErr := h.ReleaseErr()
	if relErr == nil || !strings.Contains(relErr.Error(), "release rpc timed out") {
		t.Fatalf("ReleaseErr() = %v, want release panic, not lost", relErr)
	}
}

func TestHolderStopIsIdempotent(t *testing.T) {
	renew := func() error { return nil }
	release := func() error { return nil }
	h := Start(renew, release)
	h.Tick()
	h.Stop()
	h.Stop() // must not panic or hang
}
```

## Review

`Holder` is correct when a renewal panic can never leave the process either
believing it still holds a lease it does not, or waiting forever for a
background goroutine that has already exited. The `Tick`/`done`-based
handshake is what makes both the demo and the tests fully deterministic
without a single wall-clock wait, and the two-field `Lost`/`ReleaseErr`
split is what keeps a cleanup-time bug from ever masking the renewal
failure that actually caused the lease to be lost - the failure mode an
on-call engineer would otherwise spend the most time chasing down.

## Resources

- [etcd clientv3 Lease](https://pkg.go.dev/go.etcd.io/etcd/client/v3#Lease) — the real-world API this exercise's `RenewFunc`/`ReleaseFunc` abstraction models.
- [Go Blog: Defer, Panic, and Recover](https://go.dev/blog/defer-panic-and-recover) — the recover boundaries in `renewOnce` and `releaseSafe`.
- [sync.Once](https://pkg.go.dev/sync#Once) — the idempotency guarantee behind a safely-repeatable `Stop`.

---

Back to [00-concepts.md](00-concepts.md) | Prev: [27-request-coalescing-singleflight.md](27-request-coalescing-singleflight.md) | Next: [29-broadcast-observer-registry.md](29-broadcast-observer-registry.md)
