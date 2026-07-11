# Exercise 12: Admission Control Middleware That Sheds Load With TryAcquire

**Level: Intermediate**

A service in front of a scarce downstream — a small connection pool, a rate-limited
provider, a single-writer store — must cap how many requests it processes at once.
The naive fix is a blocking semaphore, but behind an HTTP handler that just moves
the problem: overflow requests queue, latency climbs without bound, and clients that
already gave up still hold goroutines. The right shape under a burst is to *shed*:
admit up to a cap and reject the rest immediately with 503 so callers back off fast.
This exercise builds `net/http` middleware backed by a non-blocking semaphore whose
`TryAcquire` grabs a slot or fails instantly.

This module is self-contained: its own module, an `admission` package, a demo, and
tests. Nothing here imports another exercise.

## What you'll build

```text
admission/                   independent module: example.com/admission
  go.mod                     go 1.26
  admission.go               type Limiter; New, Middleware, InFlight, Shed
  cmd/demo/main.go           runnable demo: fill both slots, shed a third, recover
  admission_test.go          shed-on-overflow, peak<=cap under -race, slot recovery
```

- Files: `admission.go`, `cmd/demo/main.go`, `admission_test.go`.
- Implement: `New(maxInFlight int) *Limiter`, `(*Limiter) Middleware(next http.Handler) http.Handler`, `(*Limiter) InFlight() int`, `(*Limiter) Shed() int64`.
- Test: overflow requests get 503 + `Retry-After` and bump the shed counter; admitted concurrency never exceeds the cap under `-race`; a released slot is returned on the normal path so a fresh request is admitted.
- Verify: `go test -count=1 -race ./...`

Set up the module:

```bash
mkdir -p ~/go-exercises/admission/cmd/demo
cd ~/go-exercises/admission
go mod init example.com/admission
go mod edit -go=1.26
```

### Non-blocking acquire is the difference between shedding and queuing

A buffered `chan struct{}` of capacity `n` is a counting semaphore: a send takes a
slot, a receive returns one. The *blocking* send `sem <- struct{}{}` waits when the
buffer is full — exactly the wrong behavior in a request handler, because a waiting
caller pins a goroutine long after it has timed out. The fix is a `select` with a
`default` branch, which turns the send into `TryAcquire`: grab a slot if one is free,
otherwise take the `default` and fail without waiting. That single `default` is the
whole load-shedding mechanism.

The middleware follows a strict protocol:

1. `select` on `sem <- struct{}{}` with a `default`. If the send succeeds the request
   is *admitted* and holds one slot.
2. On the admitted path, `defer func() { <-sem }()` on the line right after the
   successful send, then call `next.ServeHTTP`. The deferred receive returns the slot
   on *every* exit path — a normal return, an early return inside the handler, or a
   panic unwinding through it. A slot leaked here is a permanent loss of capacity: the
   cap silently ratchets down toward zero and the service stalls with no error to
   point at.
3. On the `default` path the request is *shed*: increment the shed counter, set a
   `Retry-After` header so a well-behaved client backs off, and write `503`. This path
   never touches the semaphore, so a shed request must not consume or leak a slot.

Two invariants make the design trustworthy. First, balance: the semaphore is sent to
exactly once per admitted request and received from exactly once, so occupancy is
always the true count of in-flight handlers — which is why `InFlight` can simply read
`len(sem)`. Second, the accounting is race-free without a mutex: the slot count lives
in the channel itself and the shed tally is an `atomic.Int64`. There is no shared
mutable field the handler writes, so the middleware scales to real concurrency.

Create `admission.go`:

```go
package admission

import (
	"net/http"
	"sync/atomic"
)

// retryAfterSeconds is the Retry-After hint sent with every shed response. A
// small fixed value tells a well-behaved client to back off briefly before
// retrying, which spreads a burst out over time instead of hammering the door.
const retryAfterSeconds = "1"

// Limiter is admission-control middleware. It caps concurrent in-flight
// requests with a non-blocking semaphore: an admitted request holds one slot
// for the duration of the handler; a request that finds no slot free is shed
// immediately with 503 rather than queued.
type Limiter struct {
	sem  chan struct{}
	shed atomic.Int64
}

// New returns a Limiter that admits at most maxInFlight concurrent requests.
// maxInFlight must be >= 1.
func New(maxInFlight int) *Limiter {
	return &Limiter{sem: make(chan struct{}, maxInFlight)}
}

// Middleware wraps next so that a request is served only if a slot is free.
// The slot is grabbed non-blockingly: a full semaphore means the request is
// shed instantly with 503 and a Retry-After header, never parked. The slot is
// released by a deferred receive so it is returned on every exit path from the
// handler, including a panic.
func (l *Limiter) Middleware(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		select {
		case l.sem <- struct{}{}:
			// Admitted: hold the slot until the handler returns.
			defer func() { <-l.sem }()
			next.ServeHTTP(w, r)
		default:
			// No slot free: shed the request immediately.
			l.shed.Add(1)
			w.Header().Set("Retry-After", retryAfterSeconds)
			http.Error(w, "server overloaded", http.StatusServiceUnavailable)
		}
	})
}

// InFlight reports the number of currently admitted requests.
func (l *Limiter) InFlight() int { return len(l.sem) }

// Shed reports the total number of requests rejected since New.
func (l *Limiter) Shed() int64 { return l.shed.Load() }
```

### The runnable demo

The demo wires the middleware in front of a handler that signals its arrival on a
buffered channel and then blocks on a release channel, so the test driver controls
exactly when handlers hold or free their slots. It fills both slots, probes with a
third request that must be shed, releases the two, and then admits a fresh one — a
fully deterministic sequence because every step is gated on a channel, not a timer.

Create `cmd/demo/main.go`:

```go
package main

import (
	"fmt"
	"net/http"
	"net/http/httptest"
	"sync"

	"example.com/admission"
)

func main() {
	// arrived is buffered so a handler's arrival signal never blocks the
	// handler; we receive exactly the signals we need to synchronize on.
	arrived := make(chan struct{}, 8)
	release := make(chan struct{})

	h := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		arrived <- struct{}{}
		<-release
		w.WriteHeader(http.StatusOK)
	})

	lim := admission.New(2)
	srv := httptest.NewServer(lim.Middleware(h))
	defer srv.Close()
	client := srv.Client()

	// Fire two requests and wait until both are inside the handler, so both
	// slots are held before we probe with a third.
	var wg sync.WaitGroup
	statuses := make([]int, 2)
	for i := range 2 {
		wg.Go(func() {
			resp, err := client.Get(srv.URL)
			if err != nil {
				panic(err)
			}
			resp.Body.Close()
			statuses[i] = resp.StatusCode
		})
	}
	<-arrived
	<-arrived

	// Both slots are now held. A third request must be shed instantly, without
	// ever entering the handler.
	resp, err := client.Get(srv.URL)
	if err != nil {
		panic(err)
	}
	thirdStatus := resp.StatusCode
	thirdRetry := resp.Header.Get("Retry-After")
	resp.Body.Close()

	fmt.Printf("both slots held: inflight=%d\n", lim.InFlight())
	fmt.Printf("third: status=%d retryAfter=%q shed=%d\n", thirdStatus, thirdRetry, lim.Shed())

	// Release the two held requests and confirm both completed with 200.
	close(release)
	wg.Wait()
	fmt.Printf("admitted statuses=%v inflight=%d\n", statuses, lim.InFlight())

	// Slots are free again: a fresh request is admitted (release already closed,
	// so the handler returns without blocking).
	fresh, err := client.Get(srv.URL)
	if err != nil {
		panic(err)
	}
	fresh.Body.Close()
	fmt.Printf("fresh: status=%d shed=%d\n", fresh.StatusCode, lim.Shed())
}
```

Run it:

```bash
go run ./cmd/demo
```

Expected output:

```
both slots held: inflight=2
third: status=503 retryAfter="1" shed=1
admitted statuses=[200 200] inflight=0
fresh: status=200 shed=1
```

### Tests

`TestShedsOverflow` starts two requests at `maxInFlight=2`, blocks on a channel until
both are inside the handler (so both slots are provably held), then fires a third and
asserts it returns `503` with a `Retry-After` header while `Shed()==1` and
`InFlight()==2` — proving the shed path neither consumed nor leaked a slot. All
sequencing is channel-based; there are no sleeps or timing assertions.
`TestPeakNeverExceedsCap` drives 200 concurrent requests through the middleware and,
inside the handler, tracks live concurrency with an atomic `CompareAndSwap` peak; the
assertion `peak <= cap` under `-race` is the trustworthy proof the cap actually held.
`TestSlotReturnedOnNormalPath` holds both slots, releases exactly one, and asserts a
fresh request is admitted with `200` — proving the deferred release returned the slot
on the ordinary return path. It uses two routes with separate release channels so the
driver controls precisely which single slot is freed.

Create `admission_test.go`:

```go
package admission

import (
	"net/http"
	"net/http/httptest"
	"sync"
	"sync/atomic"
	"testing"
)

// TestShedsOverflow starts maxInFlight=2 requests, waits (via a channel) until
// both are inside the handler holding both slots, then asserts a third request
// is shed with 503 + Retry-After while Shed()==1 and InFlight()==2. All
// synchronization is channel-based; there are no timing assertions.
func TestShedsOverflow(t *testing.T) {
	t.Parallel()

	arrived := make(chan struct{}, 4)
	release := make(chan struct{})
	h := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		arrived <- struct{}{}
		<-release
		w.WriteHeader(http.StatusOK)
	})

	lim := New(2)
	srv := httptest.NewServer(lim.Middleware(h))
	defer srv.Close()
	client := srv.Client()

	var wg sync.WaitGroup
	for range 2 {
		wg.Go(func() {
			resp, err := client.Get(srv.URL)
			if err != nil {
				return
			}
			resp.Body.Close()
		})
	}
	// Block until both admitted requests are inside the handler.
	<-arrived
	<-arrived

	if got := lim.InFlight(); got != 2 {
		t.Fatalf("InFlight() = %d, want 2", got)
	}

	// A third request finds no free slot and must be shed immediately.
	resp, err := client.Get(srv.URL)
	if err != nil {
		t.Fatalf("third request error: %v", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusServiceUnavailable {
		t.Fatalf("third status = %d, want %d", resp.StatusCode, http.StatusServiceUnavailable)
	}
	if ra := resp.Header.Get("Retry-After"); ra == "" {
		t.Fatal("third response missing Retry-After header")
	}
	if got := lim.Shed(); got != 1 {
		t.Fatalf("Shed() = %d, want 1", got)
	}
	if got := lim.InFlight(); got != 2 {
		t.Fatalf("InFlight() after shed = %d, want 2 (shed must not consume a slot)", got)
	}

	// Let the two admitted requests finish so the server drains cleanly.
	close(release)
	wg.Wait()
}

// TestPeakNeverExceedsCap drives many concurrent requests through the
// middleware and, inside the handler, tracks admitted concurrency with an
// atomic CompareAndSwap peak. The cap must hold: peak <= maxInFlight. Under
// -race this proves the slot accounting does not admit more than the cap.
func TestPeakNeverExceedsCap(t *testing.T) {
	t.Parallel()

	const capN = 3
	var live, peak atomic.Int64
	h := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		cur := live.Add(1)
		for {
			old := peak.Load()
			if cur <= old || peak.CompareAndSwap(old, cur) {
				break
			}
		}
		live.Add(-1)
		w.WriteHeader(http.StatusOK)
	})

	lim := New(capN)
	srv := httptest.NewServer(lim.Middleware(h))
	defer srv.Close()
	client := srv.Client()

	var wg sync.WaitGroup
	for range 200 {
		wg.Go(func() {
			resp, err := client.Get(srv.URL)
			if err != nil {
				return
			}
			resp.Body.Close()
		})
	}
	wg.Wait()

	if pk := peak.Load(); pk > capN {
		t.Fatalf("peak admitted concurrency = %d, want <= %d", pk, capN)
	}
}

// TestSlotReturnedOnNormalPath proves the deferred release returns the slot on
// the ordinary (non-panic) return path: hold both slots, release one, then a
// fresh request must be admitted (200) rather than shed. Two routes with their
// own release channels keep control deterministic — the held requests block on
// releaseHold, the probe on releaseProbe — so exactly one slot is freed.
func TestSlotReturnedOnNormalPath(t *testing.T) {
	t.Parallel()

	holdArrived := make(chan struct{}, 4)
	releaseHold := make(chan struct{}, 4)
	probeArrived := make(chan struct{}, 4)
	releaseProbe := make(chan struct{})

	mux := http.NewServeMux()
	mux.HandleFunc("/hold", func(w http.ResponseWriter, r *http.Request) {
		holdArrived <- struct{}{}
		<-releaseHold
		w.WriteHeader(http.StatusOK)
	})
	mux.HandleFunc("/probe", func(w http.ResponseWriter, r *http.Request) {
		probeArrived <- struct{}{}
		<-releaseProbe
		w.WriteHeader(http.StatusOK)
	})

	lim := New(2)
	srv := httptest.NewServer(lim.Middleware(mux))
	defer srv.Close()
	client := srv.Client()

	// Occupy both slots with two /hold requests.
	holdDone := make(chan int, 2)
	for range 2 {
		go func() {
			resp, err := client.Get(srv.URL + "/hold")
			if err != nil {
				holdDone <- 0
				return
			}
			resp.Body.Close()
			holdDone <- resp.StatusCode
		}()
	}
	<-holdArrived
	<-holdArrived

	// Release exactly one held request and wait for it to fully return, which
	// runs the middleware's deferred slot release before the client sees the
	// response.
	releaseHold <- struct{}{}
	if code := <-holdDone; code != http.StatusOK {
		t.Fatalf("released hold status = %d, want 200", code)
	}

	// The returned slot must now admit a fresh probe request.
	probeStatus := make(chan int, 1)
	go func() {
		resp, err := client.Get(srv.URL + "/probe")
		if err != nil {
			probeStatus <- 0
			return
		}
		resp.Body.Close()
		probeStatus <- resp.StatusCode
	}()
	// The probe entering its handler proves it was admitted, not shed.
	<-probeArrived
	close(releaseProbe)

	if code := <-probeStatus; code != http.StatusOK {
		t.Fatalf("probe status = %d, want 200 (slot was not returned)", code)
	}
	if got := lim.Shed(); got != 0 {
		t.Fatalf("Shed() = %d, want 0 (no request should have been shed)", got)
	}

	// Drain the still-held /hold request.
	releaseHold <- struct{}{}
	<-holdDone
}
```

## Review

The middleware is correct when admitted concurrency never exceeds `maxInFlight`, every
overflow request is rejected immediately with `503` and a `Retry-After` header rather
than queued, and every admitted request returns its slot on the way out. The
non-blocking `select`-with-`default` send is what makes it shed instead of queue: a
full semaphore takes the `default` and fails in constant time, so a burst turns into
fast rejections a client can react to, not an unbounded queue that inflates tail
latency. The deferred `<-sem` on the admitted path is the balance guarantee — the slot
comes back on a normal return, an early return, or a panic — and `TestSlotReturnedOnNormalPath`
proves it by admitting a fresh request after releasing exactly one held slot.
`TestPeakNeverExceedsCap` measures live concurrency inside the handler under `-race`
and asserts `peak <= cap`, the only proof that actually distinguishes a working limiter
from one that returned the right answer once. The production bug this prevents is the
classic overload spiral: a blocking limiter in a request path parks a goroutine per
waiting caller, latency climbs, clients retry, and the pile-up takes the service down
with the very dependency it was meant to protect.

## Resources

- [Effective Go: Channels](https://go.dev/doc/effective_go#channels) — buffered channels as counting semaphores and the `select`/`default` non-blocking idiom.
- [net/http/httptest](https://pkg.go.dev/net/http/httptest) — `NewServer` and its client for driving middleware in-process.
- [net/http: Handler and Retry-After](https://pkg.go.dev/net/http#Handler) — the middleware `func(next http.Handler) http.Handler` shape and 503 semantics.
- [sync/atomic: Int64](https://pkg.go.dev/sync/atomic#Int64) — the lock-free counter used for the shed tally.

---

Back to [00-concepts.md](00-concepts.md) | Prev: [11-keyed-semaphore-per-tenant-isolation.md](11-keyed-semaphore-per-tenant-isolation.md) | Next: [13-quorum-barrier-multi-region-ack.md](13-quorum-barrier-multi-region-ack.md)
