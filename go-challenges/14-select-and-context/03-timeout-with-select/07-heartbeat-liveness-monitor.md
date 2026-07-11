# Exercise 7: Liveness monitor that flags a stalled producer

Behind every readiness gate and circuit breaker is a small state machine: reset a
deadline on each heartbeat, and if the deadline fires before the next beat, declare
the subject unhealthy. This exercise builds that monitor with a single reusable
`time.Timer` reset on each heartbeat, emitting a status-change event when the
subject stalls and again when it recovers.

## What you'll build

```text
livemon/                      module example.com/monitor
  go.mod
  monitor.go                  Status; Event; Monitor(beats, deadline, events)
  cmd/demo/main.go            three healthy beats, then silence flips unhealthy
  monitor_test.go             stays-healthy, goes-unhealthy-once, recovers
```

Files: `monitor.go`, `cmd/demo/main.go`, `monitor_test.go`.
Implement: `Monitor(beats <-chan time.Time, deadline time.Duration, events chan<- Event)` with a `Status` enum and an `Event{Status, At, LastSeen}`.
Test: steady beats stay healthy; a gap flips unhealthy exactly once and records last-seen; resumed beats flip back to healthy; the fire lands within a band, not early.
Verify: `go test -count=1 -race ./...`

Set up the module:

```bash
mkdir -p ~/go-exercises/livemon/cmd/demo
cd ~/go-exercises/livemon
go mod init example.com/monitor
```

### A two-state machine driven by one timer

`Monitor` holds a `status` (Healthy or Unhealthy) and a deadline `timer`. The loop
selects on two cases. A heartbeat arrives on `beats`: record its timestamp as
`lastSeen`, and if the current status is Unhealthy, transition back to Healthy and
emit a recovery event; then re-arm the timer for a fresh deadline window. The timer
fires: if the current status is Healthy, transition to Unhealthy and emit a stall
event carrying the fire time and the last-seen timestamp; then re-arm so the monitor
keeps watching. When `beats` closes, the monitor returns.

The status guard on each transition is what makes each flip happen *exactly once*.
After the subject goes Unhealthy, the timer keeps firing every `deadline` (there are
still no beats), but because `status` is already Unhealthy, no further event is
emitted — the monitor stays quietly unhealthy until a beat recovers it. Symmetric
logic prevents duplicate recovery events. Without the guard, a stalled subject would
emit an unhealthy event on every timer tick, flooding whatever consumes `events`.

The timer re-arm uses the portable Stop-drain-Reset form on the heartbeat path
(where the timer has not fired) and a plain `Reset` on the fire path (where the
`select` already consumed the tick), wrapped in one helper for uniformity. The
`events` channel is send-only from the monitor's side; callers must drain it (or
give it buffer) so the monitor never blocks trying to report a transition.

Create `monitor.go`:

```go
package monitor

import "time"

// Status is the liveness state of the monitored subject.
type Status int

const (
	Healthy Status = iota
	Unhealthy
)

func (s Status) String() string {
	if s == Unhealthy {
		return "unhealthy"
	}
	return "healthy"
}

// Event reports a status transition. At is when the transition happened; LastSeen
// is the timestamp of the most recent heartbeat observed.
type Event struct {
	Status   Status
	At       time.Time
	LastSeen time.Time
}

// Monitor watches beats, resetting a deadline timer on each one. If deadline
// elapses without a beat it emits an Unhealthy event (once). When beats resume it
// emits a Healthy event (once). It returns when beats is closed. events must be
// drained or buffered so the monitor does not block.
func Monitor(beats <-chan time.Time, deadline time.Duration, events chan<- Event) {
	timer := time.NewTimer(deadline)
	defer timer.Stop()

	rearm := func() {
		if !timer.Stop() {
			select {
			case <-timer.C:
			default:
			}
		}
		timer.Reset(deadline)
	}

	status := Healthy
	var lastSeen time.Time
	for {
		select {
		case b, ok := <-beats:
			if !ok {
				return
			}
			lastSeen = b
			if status == Unhealthy {
				status = Healthy
				events <- Event{Status: Healthy, At: b, LastSeen: lastSeen}
			}
			rearm()
		case now := <-timer.C:
			if status == Healthy {
				status = Unhealthy
				events <- Event{Status: Unhealthy, At: now, LastSeen: lastSeen}
			}
			timer.Reset(deadline)
		}
	}
}
```

### The runnable demo

Create `cmd/demo/main.go`:

```go
package main

import (
	"fmt"
	"time"

	"example.com/monitor"
)

func main() {
	beats := make(chan time.Time)
	events := make(chan monitor.Event, 8)
	go monitor.Monitor(beats, 60*time.Millisecond, events)

	for range 3 {
		beats <- time.Now()
		time.Sleep(30 * time.Millisecond) // under the 60ms deadline
	}
	// go silent; the deadline fires and flips unhealthy
	e := <-events
	fmt.Printf("status=%s\n", e.Status)
	close(beats)
}
```

Run it:

```bash
go run ./cmd/demo
```

Expected output:

```
status=unhealthy
```

### Tests

`TestStaysHealthy` sends five beats spaced under the deadline and asserts no event
was emitted — the subject never stalls. `TestGoesUnhealthy` sends one beat, stops,
and asserts a single Unhealthy event arrives within a band measured from the beat,
carrying the correct `LastSeen`; it then waits past another deadline window to
confirm no duplicate is emitted. `TestRecovers` drives the subject unhealthy, then
resumes beats and asserts a Healthy recovery event. Bands have a lower bound (the
timer must not fire early) and a generous upper bound for loaded CI.

Create `monitor_test.go`:

```go
package monitor

import (
	"testing"
	"time"
)

func TestStaysHealthy(t *testing.T) {
	t.Parallel()
	beats := make(chan time.Time)
	events := make(chan Event, 8)
	go Monitor(beats, 60*time.Millisecond, events)

	for range 5 {
		beats <- time.Now()
		time.Sleep(20 * time.Millisecond) // under the 60ms deadline
	}
	close(beats)

	select {
	case e := <-events:
		t.Fatalf("unexpected event while healthy: %+v", e)
	default:
	}
}

func TestGoesUnhealthy(t *testing.T) {
	t.Parallel()
	beats := make(chan time.Time)
	events := make(chan Event, 8)
	go Monitor(beats, 50*time.Millisecond, events)

	last := time.Now()
	beats <- last
	start := time.Now()
	e := <-events
	elapsed := time.Since(start)

	if e.Status != Unhealthy {
		t.Fatalf("status = %v, want Unhealthy", e.Status)
	}
	if !e.LastSeen.Equal(last) {
		t.Fatalf("LastSeen = %v, want %v", e.LastSeen, last)
	}
	if elapsed < 40*time.Millisecond {
		t.Fatalf("fired too early: %v", elapsed)
	}
	if elapsed > 500*time.Millisecond {
		t.Fatalf("fired too late: %v", elapsed)
	}

	// No duplicate within the next deadline window.
	select {
	case e2 := <-events:
		t.Fatalf("duplicate event: %+v", e2)
	case <-time.After(120 * time.Millisecond):
	}
	close(beats)
}

func TestRecovers(t *testing.T) {
	t.Parallel()
	beats := make(chan time.Time)
	events := make(chan Event, 8)
	go Monitor(beats, 40*time.Millisecond, events)

	beats <- time.Now()
	if e := <-events; e.Status != Unhealthy {
		t.Fatalf("status = %v, want Unhealthy", e.Status)
	}

	beats <- time.Now()
	if e := <-events; e.Status != Healthy {
		t.Fatalf("status = %v, want Healthy", e.Status)
	}
	close(beats)
}
```

## Review

The monitor is correct when each status flip happens exactly once and the stall
fires only after a genuine full deadline gap. The trap the status guard closes is
the duplicate-event flood: without checking `status` before emitting, every timer
tick during a stall would emit another Unhealthy event. The `LastSeen` field must
be captured on the beat, not on the fire, so the stall event reports when the
subject was last alive — a detail an operator relies on. Run `go test -race`; the
heartbeat-producing goroutine and the monitor communicate only through channels, so
there is no shared state to race on.

## Resources

- [`time.Timer.Reset`](https://pkg.go.dev/time#Timer.Reset) — re-arming the deadline on each heartbeat.
- [`time.Time`](https://pkg.go.dev/time#Time) — the heartbeat and last-seen timestamps.
- [Go by Example: Timeouts](https://gobyexample.com/timeouts) — the select-plus-timer building block.

---

Back to [00-concepts.md](00-concepts.md) | Previous: [06-batch-flush-on-timer.md](06-batch-flush-on-timer.md) | Next: [08-debounce-events.md](08-debounce-events.md)
