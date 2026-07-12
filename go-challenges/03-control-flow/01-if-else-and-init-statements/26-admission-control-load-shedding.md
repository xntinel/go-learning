# Exercise 26: Admission Control: Reject Early When System Approaches Overload

**Nivel: Intermedio** — validacion rapida (un test corto).

A service that queues every incoming request no matter how deep the backlog
already is turns a transient spike into a death spiral: latency climbs,
clients retry, retries add to the queue, and the service never recovers on
its own. Admission control breaks that spiral by rejecting new work early —
with a fast, cheap 503 — the moment load crosses a threshold, instead of
accepting it and letting it die slowly behind everything already queued.
This module is fully self-contained: its own `go mod init`, all code inline,
its own demo and tests.

## What you'll build

```text
admission/                  independent module: example.com/admission-control-load-shedding
  go.mod                    go 1.24
  admission.go              Decide(metrics, limits), Controller (mutex-protected Admit/Release)
  cmd/
    demo/
      main.go               grace period, queue-depth shedding, latency shedding
  admission_test.go         Decide table over grace/queue/latency/ok; Controller admits then sheds
```

- Files: `admission.go`, `cmd/demo/main.go`, `admission_test.go`.
- Implement: a pure guard `Decide(m Metrics, l Limits) (admit bool, reason string)` checking, in order, an uptime grace period, queue depth, and p99 latency, and a `Controller` struct guarded by a `sync.Mutex` with `Admit(p99 time.Duration) (bool, string)` and `Release()` tracking in-flight count.
- Test: a table over `Decide` covering the grace period overriding overload signals, queue depth at the max, latency over the max, and healthy load; a sequential `Controller` test that fills the queue to its max, rejects the next request, then admits again once a slot is released.
- Verify: `go test -count=1 ./...`

Set up the module:

```bash
mkdir -p go-solutions/03-control-flow/01-if-else-and-init-statements/26-admission-control-load-shedding/cmd/demo
cd go-solutions/03-control-flow/01-if-else-and-init-statements/26-admission-control-load-shedding
go mod edit -go=1.24
```

### Why the grace period is checked first, not last

`Decide` runs three checks in a fixed order: grace period, then queue depth,
then latency. Checking the grace period first — and returning immediately if
the instance is still warming up — matters operationally: a service that
just started has an empty connection pool, cold caches, and a queue depth
that can spike briefly while background initialization finishes, none of
which reflect real overload. If the queue-depth or latency guard ran first,
a brand-new instance could shed load during the first few seconds of its
life for signals that have nothing to do with whether it can actually serve
traffic — exactly the failure this whole primitive exists to prevent, just
triggered by the admission controller's own bootstrapping instead of real
load. Keeping `Decide` a pure function of a `Metrics` snapshot and a
`Limits` config means this ordering is a one-line table test, not something
that only shows up by racing a real load generator against a real process
start.

Create `admission.go`:

```go
// Package admission implements preemptive load shedding: a service rejects
// new work before it saturates, instead of queueing every request until it
// falls over.
package admission

import (
	"sync"
	"time"
)

// Metrics is the point-in-time load snapshot a decision is made from.
type Metrics struct {
	QueueDepth int
	P99Latency time.Duration
	Uptime     time.Duration
}

// Limits configures when Decide sheds load.
type Limits struct {
	MaxQueueDepth int
	MaxP99Latency time.Duration
	GracePeriod   time.Duration // newly started instances are never shed during this warmup
}

// Decide is the pure guard behind one admission check. It runs the grace
// period, queue-depth, and latency checks in that order and stops at the
// first one that applies, so a brand-new instance still warming up its
// connection pools is never shed for a queue depth that will settle on its
// own within seconds.
func Decide(m Metrics, l Limits) (admit bool, reason string) {
	if m.Uptime < l.GracePeriod {
		return true, "grace period"
	}
	if m.QueueDepth >= l.MaxQueueDepth {
		return false, "queue depth"
	}
	if m.P99Latency > l.MaxP99Latency {
		return false, "latency"
	}
	return true, "ok"
}

// Controller tracks in-flight request count and wraps Decide with the
// service's actual clock and start time. It is safe for concurrent use.
type Controller struct {
	mu       sync.Mutex
	inFlight int
	start    time.Time
	limits   Limits
	clock    func() time.Time
}

// NewController builds a Controller whose uptime is measured from the moment
// of construction, using clock for "now".
func NewController(limits Limits, clock func() time.Time) *Controller {
	return &Controller{start: clock(), limits: limits, clock: clock}
}

// Admit decides whether one more request may proceed given p99, and — if
// admitted — increments the in-flight count. The read of the current
// in-flight count and the increment happen inside one lock acquisition, so
// two concurrent callers can never both admit past a queue depth that should
// have rejected the second one.
func (c *Controller) Admit(p99 time.Duration) (admit bool, reason string) {
	c.mu.Lock()
	defer c.mu.Unlock()

	m := Metrics{
		QueueDepth: c.inFlight,
		P99Latency: p99,
		Uptime:     c.clock().Sub(c.start),
	}
	admit, reason = Decide(m, c.limits)
	if admit {
		c.inFlight++
	}
	return admit, reason
}

// Release marks one admitted request as finished, freeing its queue slot.
func (c *Controller) Release() {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.inFlight > 0 {
		c.inFlight--
	}
}
```

### The runnable demo

The demo shows all three outcomes: an overloaded snapshot admitted purely
because the instance is still within its grace period, two ordinary requests
filling the queue to its configured max, a third rejected for queue depth,
and — after releasing a slot — a request rejected instead for latency.

Create `cmd/demo/main.go`:

```go
package main

import (
	"fmt"
	"time"

	admission "example.com/admission-control-load-shedding"
)

func main() {
	t0 := time.Date(2024, 1, 1, 0, 0, 0, 0, time.UTC)
	now := t0
	clock := func() time.Time { return now }

	limits := admission.Limits{
		MaxQueueDepth: 2,
		MaxP99Latency: 200 * time.Millisecond,
		GracePeriod:   5 * time.Second,
	}
	ctl := admission.NewController(limits, clock)

	admit, reason := ctl.Admit(50 * time.Millisecond)
	fmt.Printf("during grace period:      admit=%v reason=%q\n", admit, reason)
	ctl.Release() // free the slot the grace-period request used before the real demo

	// Advance past the grace period.
	now = t0.Add(10 * time.Second)

	admit, reason = ctl.Admit(50 * time.Millisecond)
	fmt.Printf("first request:            admit=%v reason=%q\n", admit, reason)
	admit, reason = ctl.Admit(50 * time.Millisecond)
	fmt.Printf("second request:           admit=%v reason=%q\n", admit, reason)
	admit, reason = ctl.Admit(50 * time.Millisecond)
	fmt.Printf("third request (over max): admit=%v reason=%q\n", admit, reason)

	ctl.Release()
	admit, reason = ctl.Admit(300 * time.Millisecond)
	fmt.Printf("slot freed, but slow p99: admit=%v reason=%q\n", admit, reason)
}
```

Run it:

```bash
go run ./cmd/demo
```

Expected output:

```
during grace period:      admit=true reason="grace period"
first request:            admit=true reason="ok"
second request:           admit=true reason="ok"
third request (over max): admit=false reason="queue depth"
slot freed, but slow p99: admit=false reason="latency"
```

### Tests

The table drives `Decide` directly through all four branches, and a
sequential `Controller` test fills the queue, confirms rejection, and
confirms admission resumes after a release.

Create `admission_test.go`:

```go
package admission

import (
	"testing"
	"time"
)

func TestDecide(t *testing.T) {
	t.Parallel()

	limits := Limits{
		MaxQueueDepth: 10,
		MaxP99Latency: 200 * time.Millisecond,
		GracePeriod:   5 * time.Second,
	}

	tests := []struct {
		name       string
		metrics    Metrics
		wantAdmit  bool
		wantReason string
	}{
		{
			name:       "within grace period admits despite overload signals",
			metrics:    Metrics{QueueDepth: 999, P99Latency: time.Second, Uptime: time.Second},
			wantAdmit:  true,
			wantReason: "grace period",
		},
		{
			name:       "queue depth at the max rejects",
			metrics:    Metrics{QueueDepth: 10, P99Latency: 0, Uptime: time.Minute},
			wantAdmit:  false,
			wantReason: "queue depth",
		},
		{
			name:       "latency over the max rejects",
			metrics:    Metrics{QueueDepth: 0, P99Latency: 201 * time.Millisecond, Uptime: time.Minute},
			wantAdmit:  false,
			wantReason: "latency",
		},
		{
			name:       "healthy load admits",
			metrics:    Metrics{QueueDepth: 3, P99Latency: 50 * time.Millisecond, Uptime: time.Minute},
			wantAdmit:  true,
			wantReason: "ok",
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			admit, reason := Decide(tc.metrics, limits)
			if admit != tc.wantAdmit || reason != tc.wantReason {
				t.Errorf("Decide(%+v) = (%v, %q), want (%v, %q)", tc.metrics, admit, reason, tc.wantAdmit, tc.wantReason)
			}
		})
	}
}

func TestControllerAdmitsThenSheds(t *testing.T) {
	t.Parallel()

	t0 := time.Date(2024, 1, 1, 0, 0, 0, 0, time.UTC)
	now := t0.Add(time.Minute) // past any grace period
	clock := func() time.Time { return now }

	ctl := NewController(Limits{MaxQueueDepth: 1, MaxP99Latency: time.Second, GracePeriod: 0}, clock)

	admit, _ := ctl.Admit(0)
	if !admit {
		t.Fatal("first admit should succeed")
	}
	admit, reason := ctl.Admit(0)
	if admit || reason != "queue depth" {
		t.Fatalf("second admit = (%v, %q), want (false, queue depth)", admit, reason)
	}

	ctl.Release()
	admit, _ = ctl.Admit(0)
	if !admit {
		t.Fatal("admit after release should succeed once a slot frees up")
	}
}
```

Verify: `go test -count=1 ./...`

## Review

Every field on `Metrics` is read once, up front, inside the lock in `Admit`,
and then handed to a pure `Decide` that never touches the clock or the
mutex itself. That split is what makes the grace-period-first ordering safe
to change with confidence: a reviewer can see the entire admission policy in
one function with no concurrency to reason about, and the only concurrent
part — the in-flight counter — never makes a decision on its own. Carry this
forward: when a stateful controller's decision has more than one guard,
extract the guards into a pure function over a snapshot struct, and keep the
mutex-guarded type a thin shell that builds the snapshot and applies the
verdict.

## Resources

- [Google SRE Book: Handling Overload](https://sre.google/sre-book/handling-overload/) — the load-shedding rationale this module implements directly.
- [AWS Builders' Library: Using load shedding to avoid overload](https://aws.amazon.com/builders-library/using-load-shedding-to-avoid-overload/) — a production writeup of the same admission-control pattern.
- [sync.Mutex](https://pkg.go.dev/sync#Mutex) — the primitive keeping the in-flight count and the decision consistent.

---

Back to [00-concepts.md](00-concepts.md) | Prev: [25-sliding-window-rate-limiter.md](25-sliding-window-rate-limiter.md) | Next: [27-request-coalescing-singleflight.md](27-request-coalescing-singleflight.md)
