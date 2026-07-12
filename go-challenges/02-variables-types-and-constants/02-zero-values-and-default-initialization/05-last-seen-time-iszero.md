# Exercise 5: A Heartbeat Tracker Using time.Time's Zero Value As "Never Seen"

A liveness tracker must distinguish three states, not two: a node that has never
reported, a node reporting healthily, and a node that reported once but has gone
stale. The zero `time.Time` — detectable with `IsZero()` — is the idiomatic
marker for "never seen", and conflating it with "seen long ago" is a real
alerting bug: a never-started node looks infinitely stale and pages you for the
wrong reason.

This module is fully self-contained: its own `go mod init`, all code inline, its
own demo and tests.

## What you'll build

```text
heartbeat/                 independent module: example.com/heartbeat
  go.mod
  heartbeat.go             Tracker, Seen, LastSeen, Classify, Status
  cmd/
    demo/
      main.go              records beats at injected times, prints classifications
  heartbeat_test.go        deterministic (injected now) IsZero/boundary tests
```

Files: `heartbeat.go`, `cmd/demo/main.go`, `heartbeat_test.go`.
Implement: a `Tracker` mapping node id -> last-seen `time.Time`; `Seen(id, now)`, `LastSeen(id)`, and `Classify(id, now, ttl)` returning `NeverSeen`/`Healthy`/`Stale`.
Test: a never-seen node is `NeverSeen` via `IsZero`, not treated as infinitely stale; a node just past `ttl` is `Stale`; exactly at `ttl` is `Stale`; all times injected — no `time.Now()` in tests.
Verify: `go test -count=1 -race ./...`

Set up the module:

```bash
mkdir -p go-solutions/02-variables-types-and-constants/02-zero-values-and-default-initialization/05-last-seen-time-iszero/cmd/demo
cd go-solutions/02-variables-types-and-constants/02-zero-values-and-default-initialization/05-last-seen-time-iszero
```

## Why IsZero, and why the clock is injected

Reading a missing key from the `map[string]time.Time` returns the zero
`time.Time` — midnight January 1, year 1, UTC. That value is not a plausible
heartbeat, and `time.Time.IsZero()` is the canonical test for it. The classifier
checks `IsZero` (equivalently, the comma-ok read) first: if the node has no
recorded beat, it is `NeverSeen`, full stop. Only after establishing that the
node has *some* beat does staleness even make sense, computed as `now.Sub(last)
>= ttl`. Doing it in that order is what keeps "never started" distinct from
"started, then died an hour ago" — two states that demand different operator
responses.

The boundary is defined explicitly: exactly `ttl` since the last beat counts as
stale (`>=`), so a node whose beat is precisely one TTL old is already flagged.
That precision is only assertable because time is *injected*: `Seen` and
`Classify` both take a `now time.Time` parameter rather than calling
`time.Now()`. Injecting the clock keeps the tests hermetic and exact — no sleeps,
no wall-clock slack, no flakes — and is the right production shape anyway, since a
scheduler or replay tool wants to drive the tracker with its own notion of time.
(Note the difference from the synctest approach used elsewhere: for pure logic
like this that takes `now` as data, a plain parameter is simpler than a bubble.)

`Seen` owns the lazy allocation of the map — the same zero-value pattern as the
collector — so a bare `var t Tracker` is usable, and the read methods take the
lock without nesting it.

Create `heartbeat.go`:

```go
package heartbeat

import (
	"sync"
	"time"
)

// Status classifies a node's liveness.
type Status int

const (
	NeverSeen Status = iota
	Healthy
	Stale
)

func (s Status) String() string {
	switch s {
	case NeverSeen:
		return "never-seen"
	case Healthy:
		return "healthy"
	case Stale:
		return "stale"
	default:
		return "unknown"
	}
}

// Tracker records the last time each node was seen. Its zero value is usable:
// var t Tracker. The clock is injected as a parameter, never read internally.
type Tracker struct {
	mu   sync.Mutex
	seen map[string]time.Time
}

// Seen records that node id reported at time now.
func (t *Tracker) Seen(id string, now time.Time) {
	t.mu.Lock()
	defer t.mu.Unlock()
	if t.seen == nil {
		t.seen = make(map[string]time.Time)
	}
	t.seen[id] = now
}

// LastSeen returns the last time id was seen, or the zero time if never seen.
func (t *Tracker) LastSeen(id string) time.Time {
	t.mu.Lock()
	defer t.mu.Unlock()
	return t.seen[id]
}

// Classify reports whether id is never-seen, healthy, or stale as of now, given
// a staleness ttl. A node whose last beat is ttl or more in the past is stale.
func (t *Tracker) Classify(id string, now time.Time, ttl time.Duration) Status {
	t.mu.Lock()
	defer t.mu.Unlock()

	last, ok := t.seen[id]
	if !ok || last.IsZero() {
		return NeverSeen
	}
	if now.Sub(last) >= ttl {
		return Stale
	}
	return Healthy
}
```

## The runnable demo

The demo uses a fixed base time and offsets from it, so the output is stable and
readable. It records two nodes at different times, then classifies three nodes —
including one that was never seen — as of a fixed "now".

Create `cmd/demo/main.go`:

```go
package main

import (
	"fmt"
	"time"

	"example.com/heartbeat"
)

func main() {
	base := time.Date(2026, 7, 2, 12, 0, 0, 0, time.UTC)
	ttl := 30 * time.Second

	var t heartbeat.Tracker
	// web-1 beats now (healthy); web-2 beat a minute ago (stale at base).
	t.Seen("web-1", base)
	t.Seen("web-2", base.Add(-time.Minute))

	now := base
	for _, id := range []string{"web-1", "web-2", "web-3"} {
		fmt.Printf("%s: %s\n", id, t.Classify(id, now, ttl))
	}
}
```

Run it:

```bash
go run ./cmd/demo
```

Expected output:

```
web-1: healthy
web-2: stale
web-3: never-seen
```

## Tests

`TestNeverSeen` proves an unrecorded node classifies as `NeverSeen` and its
`LastSeen` is `IsZero`, never as an infinitely-stale node. `TestClassify` is a
table pinning the boundary: just inside `ttl` is `Healthy`, exactly `ttl` is
`Stale`, past `ttl` is `Stale` — all with injected times.

Create `heartbeat_test.go`:

```go
package heartbeat

import (
	"testing"
	"time"
)

var base = time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)

func TestNeverSeen(t *testing.T) {
	t.Parallel()

	var tr Tracker
	if got := tr.Classify("ghost", base, time.Minute); got != NeverSeen {
		t.Fatalf("Classify(ghost) = %s, want never-seen", got)
	}
	if last := tr.LastSeen("ghost"); !last.IsZero() {
		t.Fatalf("LastSeen(ghost) = %v, want zero time", last)
	}
}

func TestClassify(t *testing.T) {
	t.Parallel()

	const ttl = 30 * time.Second

	tests := []struct {
		name     string
		lastSeen time.Time
		now      time.Time
		want     Status
	}{
		{"just seen", base, base, Healthy},
		{"one second old", base, base.Add(time.Second), Healthy},
		{"just inside ttl", base, base.Add(ttl - time.Nanosecond), Healthy},
		{"exactly ttl", base, base.Add(ttl), Stale},
		{"past ttl", base, base.Add(ttl + time.Second), Stale},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			var tr Tracker
			tr.Seen("node", tt.lastSeen)
			if got := tr.Classify("node", tt.now, ttl); got != tt.want {
				t.Fatalf("Classify = %s, want %s", got, tt.want)
			}
		})
	}
}
```

## Review

The tracker is correct when "never seen" and "seen but stale" are genuinely
different outputs: `Classify` must check `IsZero`/comma-ok before it ever
computes `now.Sub(last)`, or a never-seen node's zero time — which is enormously
far in the past — will be reported as stale and page you for a node that never
existed. The boundary is a deliberate `>=`, so exactly one TTL of silence is
already stale; if your alerting wants strictly-greater, that is a one-character
change you should make consciously. Keep the clock injected: every method takes
`now`, no method calls `time.Now()`, and the tests never do either — that is what
makes them exact and non-flaky. Run `go test -race` since the tracker is shared.

## Resources

- [`time.Time.IsZero`](https://pkg.go.dev/time#Time.IsZero) — reports whether the time is the zero instant.
- [`time.Time.Sub`](https://pkg.go.dev/time#Time.Sub) — duration between two instants, for the staleness check.
- [`time.Time.Before`](https://pkg.go.dev/time#Time.Before) — the ordering primitive for time comparisons.

---

Back to [00-concepts.md](00-concepts.md) | Prev: [04-atomic-zero-value-counters.md](04-atomic-zero-value-counters.md) | Next: [06-nil-vs-empty-slice-json.md](06-nil-vs-empty-slice-json.md)
