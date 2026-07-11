# Exercise 10: Draining In-Flight Requests Before Force-Closing

`server.Shutdown` stops accepting new connections, but a zero-dropped-request
rolling deploy needs the component itself to know when the last handler finished.
This capstone module builds that accounting: a middleware that counts in-flight
requests, an atomic `draining` flag that sheds new work with `503` the instant
shutdown starts, and a `Stop` that waits for the group within a budget —
completing early when it drains, force-closing with a deadline error if a stuck
handler outlives the budget.

## What you'll build

```text
drain/                        independent module: example.com/drain
  go.mod                      go 1.26 (uses sync.WaitGroup.Go, Go 1.25+)
  drain.go                    Drainer: Middleware (in-flight count + 503 gate), Drain (bounded wait)
  drain_test.go               new requests 503 while draining; in-flight complete; force-close on timeout
  cmd/
    demo/
      main.go                 concurrent requests drain cleanly; a late one is shed with 503
```

Files: `drain.go`, `cmd/demo/main.go`, `drain_test.go`.
Implement: `Drainer.Middleware` that returns `503` when draining and otherwise counts the request in a `sync.WaitGroup`; `Drainer.Drain(ctx, budget)` that flips the flag, waits for the group bounded by `budget`, and returns `nil` on drain or a deadline error on force-close.
Test: fire concurrent in-flight requests gated by a channel, trigger drain, assert new requests get `503` while existing ones complete and `Drain` returns only after they finish; a separate slow request that outlives the budget makes `Drain` return a deadline error. Run under `-race`.
Verify: `go test -count=1 -race ./...`

Set up the module:

```bash
mkdir -p ~/go-exercises/drain/cmd/demo
cd ~/go-exercises/drain
go mod init example.com/drain
go mod edit -go=1.25
```

### The drain protocol

Three pieces cooperate. An `atomic.Bool` `draining` flag: while it is set, the
middleware short-circuits new requests with `503 Service Unavailable`, so the
load balancer immediately stops routing to this instance. A `sync.WaitGroup`
that counts requests currently inside a handler: the middleware does `Add(1)`
before calling the next handler and `Done()` after, so the group's count is the
number of in-flight requests. And `Drain`, which flips the flag, then waits for
the group — but *bounded*, because a single wedged handler must not block the
whole deploy forever.

The bounded wait is the delicate part. `WaitGroup.Wait` has no timeout, so `Drain`
runs it in a goroutine (using `sync.WaitGroup.Go`, added in Go 1.25, which is the
`Add(1)`/`go`/`defer Done()` pattern in one call) that closes a channel when the
group empties, then `select`s on that channel versus the budget context's `Done`.
Drained first: return `nil`. Budget first: return a deadline error and let the
orchestrator force-close — dropping the wedged request is the deliberate
trade-off, because one stuck handler outlasting the budget must not wedge every
future deploy.

There is an ordering subtlety in the `Add`/flag interplay. A request that has
already passed the `draining` check and called `Add(1)` is counted and must be
drained; a request that arrives after the flag is set is shed. The check-then-Add
must both happen while the request is "accepted"; because `Drain` sets the flag
before waiting, any request the wait must account for has already incremented the
group. Requests racing exactly at the flip either get `503` (if they read the flag
after it is set) or are counted and drained (if they incremented first) — both are
correct outcomes.

Create `drain.go`:

```go
package drain

import (
	"context"
	"fmt"
	"net/http"
	"sync"
	"sync/atomic"
	"time"
)

// Drainer tracks in-flight requests so shutdown can drain them before closing.
type Drainer struct {
	inflight sync.WaitGroup
	draining atomic.Bool
}

// Middleware counts each in-flight request and sheds new work with 503 once
// draining has begun.
func (d *Drainer) Middleware(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if d.draining.Load() {
			http.Error(w, "draining", http.StatusServiceUnavailable)
			return
		}
		d.inflight.Add(1)
		defer d.inflight.Done()
		next.ServeHTTP(w, r)
	})
}

// Draining reports whether shutdown has begun shedding new requests.
func (d *Drainer) Draining() bool { return d.draining.Load() }

// Drain begins shedding new requests (they receive 503) and waits for in-flight
// requests to finish, bounded by budget. It returns nil once they drain, or a
// deadline error if a stuck request outlives the budget (force-close).
func (d *Drainer) Drain(ctx context.Context, budget time.Duration) error {
	d.draining.Store(true)

	dctx, cancel := context.WithTimeout(ctx, budget)
	defer cancel()

	done := make(chan struct{})
	var waiter sync.WaitGroup
	waiter.Go(func() {
		d.inflight.Wait()
		close(done)
	})

	select {
	case <-done:
		return nil
	case <-dctx.Done():
		// A handler is still in-flight past the budget: force-close.
		return fmt.Errorf("drain exceeded %s: %w", budget, context.Cause(dctx))
	}
}
```

Note the `waiter` goroutine launched by `sync.WaitGroup.Go` may outlive a
force-close (it stays blocked on `d.inflight.Wait()` until the stuck handler
eventually returns). That is intentional and not a leak in the shutdown sense: the
process is exiting on force-close, and the goroutine returns the moment the
handler does. `Drain` itself returns promptly on the budget either way.

### The runnable demo

The demo fires several concurrent requests that block until released, triggers a
drain in the background, shows a late request receiving `503`, then releases the
in-flight requests and confirms `Drain` returned cleanly. It uses
`sync.WaitGroup.Go` to fan out the concurrent clients.

Create `cmd/demo/main.go`:

```go
package main

import (
	"context"
	"fmt"
	"net/http"
	"net/http/httptest"
	"sync"
	"sync/atomic"
	"time"

	"example.com/drain"
)

type blockingHandler struct {
	entered chan<- struct{}
	release <-chan struct{}
	done    *atomic.Int64
}

func (h *blockingHandler) ServeHTTP(w http.ResponseWriter, _ *http.Request) {
	h.entered <- struct{}{} // signals AFTER the middleware's Add(1)
	<-h.release
	h.done.Add(1)
	w.WriteHeader(http.StatusOK)
}

func main() {
	var d drain.Drainer
	release := make(chan struct{})
	var completed atomic.Int64

	inHandler := make(chan struct{}, 3)
	slow := d.Middleware(&blockingHandler{entered: inHandler, release: release, done: &completed})

	var clients sync.WaitGroup
	for range 3 {
		clients.Go(func() {
			rec := httptest.NewRecorder()
			slow.ServeHTTP(rec, httptest.NewRequest("GET", "/work", nil))
		})
	}
	for range 3 {
		<-inHandler // each signal follows the Add(1), so all three are counted
	}

	drainDone := make(chan error, 1)
	go func() { drainDone <- d.Drain(context.Background(), time.Second) }()
	for !d.Draining() {
		time.Sleep(time.Millisecond)
	}

	lateRec := httptest.NewRecorder()
	slow.ServeHTTP(lateRec, httptest.NewRequest("GET", "/work", nil))
	fmt.Println("late request status:", lateRec.Code)

	close(release)
	clients.Wait()
	fmt.Println("drain error:", <-drainDone)
	fmt.Println("completed in-flight:", completed.Load())
}
```

Run it:

```bash
go run ./cmd/demo
```

Expected output:

```
late request status: 503
drain error: <nil>
completed in-flight: 3
```

### Tests

`TestDrainWaitsForInFlight` fires concurrent requests that block on a release
channel, triggers `Drain` in a goroutine, waits until `Draining()` is set, and
asserts a fresh request gets `503` while `Drain` has not yet returned; it then
releases the in-flight requests and asserts they completed with `200` and `Drain`
returned `nil`. `TestDrainForceCloseOnTimeout` fires a request that never
releases within the budget and asserts `Drain` returns a `context.DeadlineExceeded`
error rather than blocking forever. Both run under `-race`.

Create `drain_test.go`:

```go
package drain

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

type gateHandler struct {
	entered chan<- struct{}
	release <-chan struct{}
	done    *atomic.Int64
}

func (h *gateHandler) ServeHTTP(w http.ResponseWriter, _ *http.Request) {
	if h.entered != nil {
		h.entered <- struct{}{} // sent AFTER the middleware's Add(1), so Add happens-before Drain's Wait
	}
	<-h.release
	if h.done != nil {
		h.done.Add(1)
	}
	w.WriteHeader(http.StatusOK)
}

func waitUntil(t *testing.T, cond func() bool) {
	t.Helper()
	deadline := time.Now().Add(time.Second)
	for time.Now().Before(deadline) {
		if cond() {
			return
		}
		time.Sleep(time.Millisecond)
	}
	t.Fatal("condition not met within 1s")
}

func TestDrainWaitsForInFlight(t *testing.T) {
	t.Parallel()

	var d Drainer
	release := make(chan struct{})
	var completed atomic.Int64

	const n = 4
	inHandler := make(chan struct{}, n)
	h := d.Middleware(&gateHandler{entered: inHandler, release: release, done: &completed})

	var clients sync.WaitGroup
	codes := make([]int, n)
	for i := range n {
		clients.Go(func() {
			rec := httptest.NewRecorder()
			h.ServeHTTP(rec, httptest.NewRequest("GET", "/", nil))
			codes[i] = rec.Code
		})
	}
	for range n {
		<-inHandler // all n Add(1)s have happened; all n are blocked inside the handler
	}

	drainDone := make(chan error, 1)
	go func() { drainDone <- d.Drain(context.Background(), 2*time.Second) }()
	waitUntil(t, d.Draining)

	// A new request while draining is shed with 503.
	lateRec := httptest.NewRecorder()
	h.ServeHTTP(lateRec, httptest.NewRequest("GET", "/", nil))
	if lateRec.Code != http.StatusServiceUnavailable {
		t.Fatalf("late request code = %d, want 503", lateRec.Code)
	}

	// Drain must not have returned while requests are still in-flight.
	select {
	case err := <-drainDone:
		t.Fatalf("Drain returned early (err=%v) with requests still in-flight", err)
	case <-time.After(20 * time.Millisecond):
	}

	// Release the in-flight requests; Drain then completes.
	close(release)
	clients.Wait()

	select {
	case err := <-drainDone:
		if err != nil {
			t.Fatalf("Drain: err = %v, want nil", err)
		}
	case <-time.After(time.Second):
		t.Fatal("Drain did not return after in-flight requests finished")
	}

	if completed.Load() != n {
		t.Fatalf("completed = %d, want %d in-flight requests drained", completed.Load(), n)
	}
	for i, c := range codes {
		if c != http.StatusOK {
			t.Fatalf("in-flight request %d code = %d, want 200", i, c)
		}
	}
}

func TestDrainForceCloseOnTimeout(t *testing.T) {
	t.Parallel()

	var d Drainer
	release := make(chan struct{})
	defer close(release) // let the stuck handler exit at test end

	inHandler := make(chan struct{}, 1)
	h := d.Middleware(&gateHandler{entered: inHandler, release: release})

	go func() {
		rec := httptest.NewRecorder()
		h.ServeHTTP(rec, httptest.NewRequest("GET", "/", nil))
	}()
	<-inHandler // Add(1) has happened; the request is in-flight and will not release before the budget

	err := d.Drain(context.Background(), 40*time.Millisecond)
	if err == nil {
		t.Fatal("Drain returned nil, want a force-close deadline error")
	}
	if !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("Drain: err = %v, want wrapping context.DeadlineExceeded", err)
	}
}
```

## Review

The drain is correct when three properties hold together. New requests receive
`503` the instant `Draining()` is set, so the load balancer stops sending work —
the `lateRec` assertion. In-flight requests are not dropped: `Drain` blocks until
the `WaitGroup` empties, so all `n` complete with `200` and `Drain` returns `nil`
only afterward. And a stuck handler cannot wedge the deploy forever: past the
budget, `Drain` returns a `context.DeadlineExceeded` error and the caller
force-closes. The common mistake is assuming `server.Shutdown` alone proves
requests drained — it stops accepting connections, but only this `WaitGroup`
accounting tells you the last handler finished. The `-race` flag is essential
here: the `draining` flag, the `WaitGroup`, and the per-index `codes` writes
(each goroutine writes its own index) must all be race-clean, and the gate runs
`go test -race` to prove it.

## Resources

- [sync.WaitGroup.Go](https://pkg.go.dev/sync#WaitGroup.Go) — the Go 1.25 launch-and-track helper used for the bounded wait and the test clients.
- [net/http Server.Shutdown](https://pkg.go.dev/net/http#Server.Shutdown) — graceful shutdown, and why explicit in-flight accounting complements it.
- [sync/atomic.Bool](https://pkg.go.dev/sync/atomic#Bool) — the lock-free draining flag.
- [context.WithTimeout and context.Cause](https://pkg.go.dev/context#WithTimeout) — bounding the drain and reporting the force-close reason.

---

Back to [00-concepts.md](00-concepts.md) | Previous: [09-request-scoped-context-and-detached-background-work.md](09-request-scoped-context-and-detached-background-work.md) | Next: [../../15-sync-primitives/01-sync-mutex/00-concepts.md](../../15-sync-primitives/01-sync-mutex/00-concepts.md)
