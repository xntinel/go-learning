# Exercise 10: Lease/heartbeat renewal loop that renews before expiry, tested with synctest

Distributed locks and leader election rest on a lease: a holder must keep renewing
its lease before the TTL expires, or another node takes over. The renewal loop
runs a ticker at half the TTL so a single missed renewal still leaves margin. This
is time-dependent concurrent code that also does I/O to a lease backend — the ideal
case for `synctest`: run the real ticker loop unchanged in a bubble, stub the
backend with an in-memory fake, and prove renewals land on schedule, a failure
lets the lease lapse, and cancellation stops the loop cleanly.

## What you'll build

```text
leaserenew/                    independent module: example.com/leaserenew
  go.mod
  lease.go                     Backend interface; Holder.Run(ctx) — ticker at ttl/2
  cmd/
    demo/
      main.go                  run a holder on a real short ttl; print renewal count
  lease_test.go                synctest: renew count across intervals; failure lapses; cancel exits
```

Files: `lease.go`, `cmd/demo/main.go`, `lease_test.go`.
Implement: a `Holder` that renews a lease every `ttl/2` via `time.NewTicker`, calling an injected `Backend.Renew`, returning the backend's error if a renewal fails and `ctx.Err()` on cancellation.
Test: `synctest.Test` with a fake backend recording renew calls — advance virtual time across several `ttl/2` intervals and assert the count; script a renewal failure and assert the holder reports lease-lost and stops; cancel and confirm the goroutine exits.
Verify: `go test -count=1 -race ./...`

Set up the module (synctest is stable in Go 1.25):

```bash
mkdir -p go-solutions/12-testing-ecosystem/16-testing-time-dependent-code/10-lease-renewal-heartbeat/cmd/demo
cd go-solutions/12-testing-ecosystem/16-testing-time-dependent-code/10-lease-renewal-heartbeat
go mod edit -go=1.25
```

### Renew at ttl/2, and why the backend is an interface

A lease with TTL `d` must be renewed *before* `d` elapses, or the lock is
considered lost and a peer can grab it. Renewing at exactly `d` is a race against
the clock and network latency; the standard margin is to renew every `d/2`, so one
dropped renewal still leaves a full `d/2` of slack before the lease actually
expires. The `Holder` therefore runs `time.NewTicker(ttl/2)` and calls
`Backend.Renew` on each tick.

The backend is an *interface* for two reasons. In production it is a Redis
`SET ... XX PX`, an etcd lease keep-alive, or a database row update — real I/O.
Real I/O inside a `synctest` bubble would block on a syscall that never counts as
durably blocked, stalling the virtual clock and hanging the test. So the test
injects an in-memory fake `Backend` that records calls and returns instantly,
keeping everything inside the bubble. This is the general recipe for testing a
loop that mixes timers with I/O under synctest: keep the timer real, stub the I/O.

`Run` returns when the context is cancelled (`ctx.Err()`, i.e.
`context.Canceled`) or when a renewal fails (the backend's error) — the latter is
"lease lost," the signal for the holder to stop acting as leader. Because `Run`
returns a value, the test captures it over a buffered channel from the goroutine.
`defer ticker.Stop()` guarantees no ticker leaks, which the bubble would otherwise
report as a deadlock.

Create `lease.go`:

```go
package leaserenew

import (
	"context"
	"time"
)

// Backend renews the lease. In production this is Redis/etcd/SQL; a test injects
// an in-memory fake so the synctest bubble stays free of real I/O.
type Backend interface {
	Renew(ctx context.Context) error
}

// Holder keeps a lease alive by renewing it every ttl/2.
type Holder struct {
	backend Backend
	ttl     time.Duration
}

func NewHolder(backend Backend, ttl time.Duration) *Holder {
	return &Holder{backend: backend, ttl: ttl}
}

// Run renews the lease every ttl/2 until ctx is cancelled (returns ctx.Err())
// or a renewal fails (returns that error, meaning the lease was lost).
func (h *Holder) Run(ctx context.Context) error {
	t := time.NewTicker(h.ttl / 2)
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-t.C:
			if err := h.backend.Renew(ctx); err != nil {
				return err
			}
		}
	}
}
```

### The runnable demo

The demo runs a holder against a real 40ms TTL (renewing every 20ms) with a
counting backend, lets a few renewals happen, then cancels and prints the count.

Create `cmd/demo/main.go`:

```go
package main

import (
	"context"
	"fmt"
	"sync/atomic"
	"time"

	"example.com/leaserenew"
)

type countingBackend struct {
	n atomic.Int64
}

func (b *countingBackend) Renew(context.Context) error {
	b.n.Add(1)
	return nil
}

func main() {
	be := &countingBackend{}
	h := leaserenew.NewHolder(be, 40*time.Millisecond)

	ctx, cancel := context.WithCancel(context.Background())
	go h.Run(ctx)

	time.Sleep(90 * time.Millisecond) // ~4 renewals at 20ms each
	cancel()
	time.Sleep(10 * time.Millisecond)

	fmt.Printf("renewals: %d\n", be.n.Load())
}
```

Run it:

```bash
go run ./cmd/demo
```

Expected output:

```
renewals: 4
```

### Tests

`TestRenewsOnSchedule` runs inside a bubble: with `ttl = 2s` the ticker fires every
second, so advancing one second at a time (with `synctest.Wait` after each) should
increment the renew count by one each interval. `TestRenewalFailureLapses` scripts
the fake to fail on its second renewal and asserts `Run` returns that error — the
lease-lost signal. `TestCancelStops` cancels the context and asserts `Run` returns
`context.Canceled`, and the bubble confirms the goroutine exits.

Create `lease_test.go`:

```go
package leaserenew

import (
	"context"
	"errors"
	"sync"
	"testing"
	"testing/synctest"
	"time"
)

// fakeBackend records renew calls and can be scripted to fail on the Nth call.
type fakeBackend struct {
	mu      sync.Mutex
	calls   int
	failOn  int // if >0, Renew returns errLost on this call number
	errLost error
}

func (b *fakeBackend) Renew(context.Context) error {
	b.mu.Lock()
	defer b.mu.Unlock()
	b.calls++
	if b.failOn > 0 && b.calls == b.failOn {
		return b.errLost
	}
	return nil
}

func (b *fakeBackend) count() int {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.calls
}

func TestRenewsOnSchedule(t *testing.T) {
	t.Parallel()
	synctest.Test(t, func(t *testing.T) {
		be := &fakeBackend{}
		h := NewHolder(be, 2*time.Second) // renews every 1s

		ctx, cancel := context.WithCancel(context.Background())
		defer cancel()
		done := make(chan error, 1)
		go func() { done <- h.Run(ctx) }()

		for i := 1; i <= 3; i++ {
			time.Sleep(time.Second) // one ttl/2 interval
			synctest.Wait()
			if got := be.count(); got != i {
				t.Fatalf("after %d intervals: renewals = %d, want %d", i, got, i)
			}
		}
	})
}

func TestRenewalFailureLapses(t *testing.T) {
	t.Parallel()
	synctest.Test(t, func(t *testing.T) {
		errLost := errors.New("lease lost")
		be := &fakeBackend{failOn: 2, errLost: errLost}
		h := NewHolder(be, 2*time.Second)

		ctx, cancel := context.WithCancel(context.Background())
		defer cancel()
		done := make(chan error, 1)
		go func() { done <- h.Run(ctx) }()

		// First renewal succeeds, second fails and returns.
		time.Sleep(2 * time.Second)
		synctest.Wait()

		select {
		case err := <-done:
			if !errors.Is(err, errLost) {
				t.Fatalf("Run returned %v, want lease-lost error", err)
			}
		default:
			t.Fatal("Run did not return after a failed renewal")
		}
	})
}

func TestCancelStops(t *testing.T) {
	t.Parallel()
	synctest.Test(t, func(t *testing.T) {
		be := &fakeBackend{}
		h := NewHolder(be, 2*time.Second)

		ctx, cancel := context.WithCancel(context.Background())
		done := make(chan error, 1)
		go func() { done <- h.Run(ctx) }()

		time.Sleep(time.Second)
		synctest.Wait()
		cancel()
		synctest.Wait()

		select {
		case err := <-done:
			if !errors.Is(err, context.Canceled) {
				t.Fatalf("Run returned %v, want context.Canceled", err)
			}
		default:
			t.Fatal("Run did not return after cancel")
		}
	})
}
```

## Review

The holder is correct when it renews once per `ttl/2` interval, returns the
backend's error the moment a renewal fails (lease lost), and returns
`context.Canceled` on cancellation — with no leaked ticker goroutine. Running it
under `synctest` proves the schedule without waiting real seconds: `synctest.Wait`
after each virtual interval guarantees the renewal completed before the count is
read, and the bubble catches a leaked loop as a deadlock. The design lesson is the
interface seam: a lease loop that called Redis directly could not run in a bubble
(the syscall never counts as durably blocked and the clock would stall), so the
backend is an interface stubbed in-memory for the test while the ticker stays real.
Renewing at `ttl/2` rather than `ttl` is the operational point — it leaves margin
for one dropped renewal. Run `go test -race`; the fake backend's counter is mutex
guarded.

## Resources

- [`testing/synctest`](https://pkg.go.dev/testing/synctest) — `synctest.Test` and `synctest.Wait` for the renewal schedule.
- [`time.NewTicker`](https://pkg.go.dev/time#NewTicker) and [`Ticker.Stop`](https://pkg.go.dev/time#Ticker.Stop) — the real ticker, virtualized in the bubble.
- [etcd lease / keepalive](https://etcd.io/docs/latest/learning/api/#lease-api) — a production lease with TTL and periodic keep-alive, the pattern this models.

---

Back to [00-concepts.md](00-concepts.md) | Prev: [09-token-expiry-clock-skew.md](09-token-expiry-clock-skew.md) | Next: [../17-testing-with-environment-variables/00-concepts.md](../17-testing-with-environment-variables/00-concepts.md)
