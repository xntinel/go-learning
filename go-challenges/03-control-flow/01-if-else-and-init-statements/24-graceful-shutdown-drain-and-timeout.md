# Exercise 24: Graceful Shutdown: Drain Pending, Timeout, Force Kill

**Nivel: Avanzado** — validacion normal (tabla de casos, incluye borde o concurrencia).

A service that exits the instant it receives a shutdown signal cuts off
every request currently in flight, turning a routine deploy into a burst
of client-visible errors. A service that waits forever for requests to
finish can hang a rolling deploy indefinitely if one request is stuck.
The correct behavior sits between those two: track in-flight work, wait
for it to drain, but give up and force-kill once a deadline passes. This
module is fully self-contained: its own `go mod init`, all code inline,
its own demo and tests.

## What you'll build

```text
shutdown/                   independent module: example.com/graceful-shutdown-drain-and-timeout
  go.mod                    go 1.24
  shutdown.go               Draining (mutex-protected), Begin, Decide, Shutdown
  cmd/
    demo/
      main.go               a clean drain, a stuck request force-killed, rejection after shutdown
  shutdown_test.go          Decide table; clean drain; force-kill; concurrent begin/release -race
```

- Files: `shutdown.go`, `cmd/demo/main.go`, `shutdown_test.go`.
- Implement: a `Draining` struct guarded by a `sync.Mutex` with `Begin() (release func(), ok bool)` that rejects new work once closed, a pure guard `Decide(inFlight int, now, deadline time.Time) string` returning `"drained"`, `"wait"`, or `"force-kill"`, and `Shutdown(deadline time.Time, pollEvery time.Duration, clock func() time.Time) error` that polls `Decide` until it stops waiting.
- Test: a table over `Decide`'s three outcomes including the boundary where `now` equals `deadline`; a `Shutdown` that drains cleanly; one that force-kills because the deadline has already passed; `Begin` rejected once shutdown starts; and a concurrency test hammering `Begin`/release from many goroutines at once, under `-race`.
- Verify: `go test -count=1 -race ./...`

Set up the module:

```bash
go mod edit -go=1.24
```

### Why the deadline decision is a pure function injected with a clock

`Decide` takes `inFlight`, `now`, and `deadline` as plain values and
returns a string — it touches no mutex and calls no `time.Now()` itself.
That is deliberate: the three-way decision (drained / wait / force-kill)
is the part of this module with actual branching logic worth testing
exhaustively, including the exact boundary where `now` equals `deadline`
(this module treats "exactly at the deadline" as force-kill, not one more
grace tick — `!now.Before(deadline)` is true at equality). Keeping it pure
means the table test in `TestDecide` never needs a mutex, a goroutine, or
a real clock. `Shutdown` is the thin, stateful wrapper around it: it takes
a `clock func() time.Time` parameter specifically so tests and the demo
can force an "already past deadline" scenario without a real sleep,
exercising the force-kill path in zero wall-clock time instead of an
actual timeout.

Create `shutdown.go`:

```go
// Package shutdown implements a graceful drain: in-flight requests are
// tracked under a mutex, and a shutdown either waits for them to finish, or
// force-kills once a deadline passes.
package shutdown

import (
	"fmt"
	"sync"
	"time"
)

// Draining tracks the number of in-flight requests and whether new ones are
// still accepted. It is safe for concurrent use; the zero value accepts work.
type Draining struct {
	mu       sync.Mutex
	inFlight int
	closed   bool
}

// Begin registers one in-flight request. ok is false once Shutdown has
// started (Begin rejects new work during a drain), in which case release is
// a no-op and the caller must not proceed with the request.
func (d *Draining) Begin() (release func(), ok bool) {
	d.mu.Lock()
	defer d.mu.Unlock()

	if d.closed {
		return func() {}, false
	}
	d.inFlight++
	return func() {
		d.mu.Lock()
		d.inFlight--
		d.mu.Unlock()
	}, true
}

func (d *Draining) inFlightCount() int {
	d.mu.Lock()
	defer d.mu.Unlock()
	return d.inFlight
}

// Decide is the pure guard behind one poll of a shutdown loop: given the
// current in-flight count, now, and the deadline by which shutdown must
// complete, it reports whether the drain is done, must keep waiting, or has
// run out of time and must force-kill.
func Decide(inFlight int, now, deadline time.Time) string {
	if inFlight == 0 {
		return "drained"
	}
	if !now.Before(deadline) {
		return "force-kill"
	}
	return "wait"
}

// Shutdown marks the drain closed — rejecting new Begin calls — then polls
// Decide every pollEvery (using clock for "now") until it stops returning
// "wait". It returns nil on a clean drain and an error once the deadline
// passes with requests still in flight.
func (d *Draining) Shutdown(deadline time.Time, pollEvery time.Duration, clock func() time.Time) error {
	d.mu.Lock()
	d.closed = true
	d.mu.Unlock()

	for {
		n := d.inFlightCount()
		decision := Decide(n, clock(), deadline)

		if decision == "drained" {
			return nil
		}
		if decision == "force-kill" {
			return fmt.Errorf("shutdown timed out with %d request(s) still in flight", n)
		}
		time.Sleep(pollEvery)
	}
}
```

### The runnable demo

The demo runs both outcomes: three requests that all finish before
shutdown is requested drain cleanly, and a single request that never
finishes is force-killed instantly because the demo passes a deadline that
has already elapsed — no real waiting required to see the outcome. It
also shows a new request being rejected once a shutdown is underway.

Create `cmd/demo/main.go`:

```go
package main

import (
	"fmt"
	"time"

	shutdown "example.com/graceful-shutdown-drain-and-timeout"
)

func main() {
	// Scenario 1: three requests begin and finish before shutdown is asked
	// for, so the drain completes cleanly.
	clean := &shutdown.Draining{}
	var releases []func()
	for i := 0; i < 3; i++ {
		release, ok := clean.Begin()
		fmt.Printf("clean: request %d began, ok=%v\n", i, ok)
		releases = append(releases, release)
	}
	for _, release := range releases {
		release()
	}
	err := clean.Shutdown(time.Now().Add(time.Minute), time.Millisecond, time.Now)
	fmt.Println("clean shutdown result:", err)

	// Scenario 2: one request begins and never finishes; the shutdown
	// deadline has already passed, so it force-kills on the first check
	// instead of waiting.
	stuck := &shutdown.Draining{}
	_, _ = stuck.Begin()
	pastDeadline := time.Now().Add(-time.Second)
	err = stuck.Shutdown(pastDeadline, time.Millisecond, time.Now)
	fmt.Println("stuck shutdown result:", err)

	// New requests are rejected once shutdown has started.
	_, ok := stuck.Begin()
	fmt.Println("new request after shutdown started, ok:", ok)
}
```

Run it:

```bash
go run ./cmd/demo
```

Expected output:

```
clean: request 0 began, ok=true
clean: request 1 began, ok=true
clean: request 2 began, ok=true
clean shutdown result: <nil>
stuck shutdown result: shutdown timed out with 1 request(s) still in flight
new request after shutdown started, ok: false
```

### Tests

`TestDecide` locks in all three outcomes, including the equal-to-deadline
boundary. Two `Shutdown` tests cover the clean drain and the immediate
force-kill. `TestBeginRejectedAfterShutdownStarts` proves the closed flag
takes effect the moment `Shutdown` is called, even on an already-empty
drain. The concurrency test hammers `Begin`/release from 64 goroutines at
once and asserts the in-flight count settles back to zero, under `-race`.

Create `shutdown_test.go`:

```go
package shutdown

import (
	"sync"
	"testing"
	"time"
)

func TestDecide(t *testing.T) {
	t.Parallel()

	now := time.Date(2024, 1, 1, 0, 0, 0, 0, time.UTC)
	deadline := now.Add(time.Second)

	tests := []struct {
		name     string
		inFlight int
		now      time.Time
		want     string
	}{
		{name: "drained regardless of time", inFlight: 0, now: deadline.Add(time.Hour), want: "drained"},
		{name: "waits before the deadline", inFlight: 2, now: now, want: "wait"},
		{name: "force-kills exactly at the deadline", inFlight: 2, now: deadline, want: "force-kill"},
		{name: "force-kills past the deadline", inFlight: 1, now: deadline.Add(time.Minute), want: "force-kill"},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			if got := Decide(tc.inFlight, tc.now, deadline); got != tc.want {
				t.Errorf("Decide(%d, %v, %v) = %q, want %q", tc.inFlight, tc.now, deadline, got, tc.want)
			}
		})
	}
}

func TestShutdownDrainsCleanly(t *testing.T) {
	t.Parallel()

	d := &Draining{}
	release, ok := d.Begin()
	if !ok {
		t.Fatal("Begin() ok = false, want true before shutdown")
	}
	release()

	err := d.Shutdown(time.Now().Add(time.Minute), time.Millisecond, time.Now)
	if err != nil {
		t.Fatalf("Shutdown() = %v, want nil", err)
	}
}

func TestShutdownForceKillsPastDeadline(t *testing.T) {
	t.Parallel()

	d := &Draining{}
	if _, ok := d.Begin(); !ok {
		t.Fatal("Begin() ok = false, want true")
	}

	pastDeadline := time.Now().Add(-time.Second)
	err := d.Shutdown(pastDeadline, time.Millisecond, time.Now)
	if err == nil {
		t.Fatal("Shutdown() = nil, want a force-kill error")
	}
}

func TestBeginRejectedAfterShutdownStarts(t *testing.T) {
	t.Parallel()

	d := &Draining{}
	if err := d.Shutdown(time.Now().Add(time.Minute), time.Millisecond, time.Now); err != nil {
		t.Fatalf("Shutdown() on an empty drain = %v, want nil", err)
	}

	_, ok := d.Begin()
	if ok {
		t.Fatal("Begin() ok = true after shutdown started, want false")
	}
}

func TestConcurrentBeginReleaseNeverRaces(t *testing.T) {
	t.Parallel()

	d := &Draining{}
	const n = 64
	var wg sync.WaitGroup
	for range n {
		wg.Add(1)
		go func() {
			defer wg.Done()
			release, ok := d.Begin()
			if ok {
				release()
			}
		}()
	}
	wg.Wait()

	if got := d.inFlightCount(); got != 0 {
		t.Fatalf("inFlightCount() after all concurrent begin/release = %d, want 0", got)
	}
}
```

Verify: `go test -count=1 -race ./...`

## Review

Treating "exactly at the deadline" as force-kill rather than one more
grace tick is a small decision with a real consequence: a deploy tool
enforcing its own hard timeout at that same instant must never be left
waiting on a service that gave itself an extra tick of leeway. The pure
`Decide` function is what makes that boundary a one-line table test
instead of something only reachable by timing a real sleep precisely.
Carry this forward: whenever a stateful, mutex-guarded type makes a
non-trivial decision, split the decision into a pure function you can
table-test directly, and keep the mutex and the polling loop as a thin
shell around it.

## Resources

- [Kubernetes: Pod Lifecycle — termination](https://kubernetes.io/docs/concepts/workloads/pods/pod-lifecycle/#pod-termination) — the production deadline (`terminationGracePeriodSeconds`) this module mirrors.
- [net/http: Server.Shutdown](https://pkg.go.dev/net/http#Server.Shutdown) — the standard library's own graceful-drain-then-force-close shape.
- [context.Context](https://pkg.go.dev/context) — the idiomatic way a real service would carry the shutdown deadline through request-scoped work.

---

Back to [00-concepts.md](00-concepts.md) | Prev: [23-dead-letter-queue-error-classification.md](23-dead-letter-queue-error-classification.md) | Next: [25-sliding-window-rate-limiter.md](25-sliding-window-rate-limiter.md)
