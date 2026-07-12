# Exercise 16: Idempotency Key Gate with Sliding Time Window

**Nivel: Intermedio** — validacion rapida (un test corto).

An at-least-once delivery system — a queue consumer, a webhook receiver, a
retrying HTTP client — will redeliver the same logical request more than
once, and downstream side effects (charging a card, sending an email) must
run exactly once per request. This module builds the deduplicator: a store
of request keys with the timestamp they were last seen, gated by a sliding
time window, plus a sweep that ranges the store to evict entries the window
has already passed so the store does not grow forever. The module is fully
self-contained: its own `go mod init`, no external dependencies.

## What you'll build

```text
idemgate/                   independent module: example.com/idempotency-key-gate-windowed
  go.mod                    go 1.24
  idemgate.go               type Gate; Allow(key, now) bool; Sweep(now) int
  cmd/
    demo/
      main.go               runnable demo: duplicate rejected, sweep evicts
  idemgate_test.go          table test: window edge + sweep behavior
```

- Files: `idemgate.go`, `cmd/demo/main.go`, `idemgate_test.go`.
- Implement: `Gate.Allow(key string, now time.Time) bool` and
  `Gate.Sweep(now time.Time) int`, both operating on a
  `map[string]time.Time` keyed by request key.
- Test: one table over `Allow` sequences crossing the window boundary, plus a
  `Sweep` case asserting eviction count and post-sweep state.
- Verify: `go test -count=1 ./...`

Set up the module:

```bash
go mod edit -go=1.24
```

### Why Allow and Sweep are two separate ranges over the same map

`Allow` never ranges the map — it does a single keyed lookup, which is the
whole point of using a map here: O(1) membership plus timestamp, no scan per
request on the hot path. The range only happens in `Sweep`, which is called
on its own schedule (a ticker, a background goroutine) and is the only place
that walks every tracked key. Splitting the two matters because they have
different jobs: `Allow` decides "is this a duplicate right now," a per-request
decision; `Sweep` decides "which entries no longer matter to any future
`Allow` call," a bulk maintenance decision. Conflating them — say, sweeping
inside `Allow` — would turn every request into an O(n) scan of the whole
store, the exact cost the map was chosen to avoid.

`Sweep` deletes the currently-ranged key directly inside the loop
(`delete(g.seen, key)` while ranging `g.seen`). That is one of the few map
mutations Go defines as safe during range: a key deleted before the loop
reaches it is guaranteed not to be produced, so nothing is double-counted or
skipped by this deletion.

Create `idemgate.go`:

```go
package idemgate

import "time"

// Gate deduplicates requests by key within a sliding time window, suitable
// for an at-least-once delivery consumer that must not process the same
// request twice even though its transport can redeliver it.
type Gate struct {
	window time.Duration
	seen   map[string]time.Time
}

// New builds a Gate that treats a key as a duplicate if it was last seen
// less than window ago.
func New(window time.Duration) *Gate {
	return &Gate{
		window: window,
		seen:   make(map[string]time.Time),
	}
}

// Allow reports whether key should be processed now: true the first time a
// key is seen, or once its previous sighting has fallen outside window.
// Either way it records key at now, sliding the window forward.
func (g *Gate) Allow(key string, now time.Time) bool {
	if last, ok := g.seen[key]; ok && now.Sub(last) < g.window {
		return false
	}
	g.seen[key] = now
	return true
}

// Sweep ranges the store once and deletes every key whose last sighting is
// now outside window, bounding the store's memory instead of growing it
// forever. Deleting the current key while ranging is defined and safe in Go.
// It returns how many entries were removed.
func (g *Gate) Sweep(now time.Time) int {
	removed := 0
	for key, at := range g.seen {
		if now.Sub(at) >= g.window {
			delete(g.seen, key)
			removed++
		}
	}
	return removed
}

// Len reports how many keys the gate currently tracks.
func (g *Gate) Len() int {
	return len(g.seen)
}
```

### The runnable demo

The demo sends a key twice inside the window (rejected the second time), a
different key once, then advances time past the window and sweeps.

Create `cmd/demo/main.go`:

```go
package main

import (
	"fmt"
	"time"

	"example.com/idempotency-key-gate-windowed"
)

func main() {
	gate := idemgate.New(1 * time.Minute)
	base := time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)

	fmt.Println(gate.Allow("req-1", base))                     // true: first sighting
	fmt.Println(gate.Allow("req-1", base.Add(10*time.Second))) // false: retry within window
	fmt.Println(gate.Allow("req-2", base.Add(20*time.Second))) // true: different key

	removed := gate.Sweep(base.Add(2 * time.Minute))
	fmt.Printf("tracked=%d removed=%d\n", gate.Len(), removed)
}
```

Run it:

```bash
go run ./cmd/demo
```

Expected output:

```text
true
false
true
tracked=0 removed=2
```

### Tests

The table drives a sequence of `Allow` calls that crosses the window
boundary for the same key, then a separate case exercises `Sweep`, asserting
both the eviction count and that a still-fresh key survives.

Create `idemgate_test.go`:

```go
package idemgate

import (
	"testing"
	"time"
)

func TestGateAllow(t *testing.T) {
	t.Parallel()

	base := time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)

	tests := []struct {
		name   string
		window time.Duration
		calls  []struct {
			key string
			at  time.Time
		}
		want []bool
	}{
		{
			name:   "duplicate within window rejected, after window allowed",
			window: 30 * time.Second,
			calls: []struct {
				key string
				at  time.Time
			}{
				{"req-1", base},
				{"req-1", base.Add(10 * time.Second)},
				{"req-1", base.Add(40 * time.Second)},
				{"req-2", base.Add(40 * time.Second)},
			},
			want: []bool{true, false, true, true},
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			g := New(tc.window)
			for i, call := range tc.calls {
				got := g.Allow(call.key, call.at)
				if got != tc.want[i] {
					t.Errorf("call %d Allow(%q, %v) = %v, want %v", i, call.key, call.at, got, tc.want[i])
				}
			}
		})
	}
}

func TestGateSweep(t *testing.T) {
	t.Parallel()

	base := time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)
	g := New(1 * time.Minute)

	g.Allow("stale", base)
	g.Allow("fresh", base.Add(50*time.Second))

	removed := g.Sweep(base.Add(70 * time.Second))
	if removed != 1 {
		t.Fatalf("Sweep removed = %d, want 1", removed)
	}
	if g.Len() != 1 {
		t.Fatalf("Len() = %d, want 1", g.Len())
	}
	if g.Allow("fresh", base.Add(70*time.Second)) {
		t.Fatal("Allow(fresh) = true, want false: entry should still be within its own window")
	}
}
```

Run it:

```bash
go test -count=1 ./...
```

## Review

The gate is correct when a redelivered key inside its window is always
rejected and a key outside its window (whether never seen or swept) is
always allowed. The common bug this design avoids is conflating "reject
duplicates" with "bound memory" into one operation — sweeping on every
`Allow` call would make request latency depend on how many stale keys have
piled up, which is exactly the kind of hidden O(n) that shows up as a
production latency regression under load. Keeping `Sweep` as its own,
separately-scheduled range keeps `Allow` O(1) regardless of store size.

## Resources

- [Go Specification: For statements (range over map, delete during range)](https://go.dev/ref/spec#For_range)
- [package time](https://pkg.go.dev/time)
- [Implementing Stripe-style idempotency keys](https://stripe.com/blog/idempotency) — production rationale for windowed idempotency stores.

---

Back to [00-concepts.md](00-concepts.md) | Prev: [15-tag-dedup-first-seen.md](15-tag-dedup-first-seen.md) | Next: [17-log-error-aggregator-by-type.md](17-log-error-aggregator-by-type.md)
