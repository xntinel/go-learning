# Exercise 25: Sliding Window Rate Limiter: Exact Per-Second Boundary Protection

**Nivel: Intermedio** — validacion rapida (un test corto).

A token-bucket limiter approximates a rate limit by refilling capacity on a
schedule; it is cheap but it can let a caller burst well past the stated
limit right at a refill boundary. A sliding-window limiter looks at the
actual timestamps of a caller's recent requests instead of an approximated
budget: it always answers "how many requests did this caller actually make
in the last N seconds?" exactly, which is what a billing-relevant or
abuse-relevant limit usually needs to guarantee. This module is fully
self-contained: its own `go mod init`, all code inline, its own demo and
tests.

## What you'll build

```text
slidingwindow/              independent module: example.com/sliding-window-rate-limiter
  go.mod                    go 1.24
  window.go                 Decide(timestamps, now, window, limit), Limiter (mutex-protected)
  cmd/
    demo/
      main.go               five requests from one caller, one rejected, one admitted after aging out
  window_test.go            Decide table incl. exact window-old boundary; Limiter sequence; per-caller isolation
```

- Files: `window.go`, `cmd/demo/main.go`, `window_test.go`.
- Implement: a pure guard `Decide(timestamps []time.Time, now time.Time, window time.Duration, limit int) (kept []time.Time, allowed bool)` that prunes aged-out timestamps and admits only if the pruned count plus one does not exceed the limit, and a `Limiter` struct guarded by a `sync.Mutex` with `Allow(caller string, now time.Time) bool` wrapping `Decide` per caller key.
- Test: a table over `Decide` covering an empty history, under the limit, at the limit, the exact window-old boundary (pruned, not kept), and one entry aging out to free room; a sequential `Limiter.Allow` walk that rejects a fourth request and then admits a fifth once the window has moved; independent windows per caller key.
- Verify: `go test -count=1 ./...`

Set up the module:

```bash
mkdir -p ~/go-exercises/slidingwindow/cmd/demo
cd ~/go-exercises/slidingwindow
go mod init example.com/sliding-window-rate-limiter
go mod edit -go=1.24
```

### Why "exactly window-old" must be pruned, not kept

`Decide` uses `ts.After(cutoff)` where `cutoff := now.Add(-window)`, not
`!ts.Before(cutoff)`. That choice is deliberate: a request stamped exactly
`window` ago has already had its full window's worth of time pass, so it must
not count against the caller anymore. Using an inclusive comparison here
would let a caller's oldest request linger for one tick longer than the
window actually allows, which compounds every time the caller sends requests
at a steady cadence exactly `window`-spaced apart — the boundary is not an
edge case that rarely matters, it is the case a steady-rate caller hits on
every single request. Keeping `Decide` pure (no mutex, no clock call) means
this exact boundary is a one-line table test rather than something only
reachable by timing real requests down to the millisecond.

Create `window.go`:

```go
// Package slidingwindow implements a precise sliding-window rate limiter: it
// looks at the actual timestamps of a caller's recent requests rather than
// approximating with a refillable token bucket.
package slidingwindow

import (
	"sync"
	"time"
)

// Decide is the pure guard behind one admission check. Given the caller's
// recorded request timestamps, the current instant, the window length, and
// the limit, it prunes any timestamp that has aged out of the window and
// reports whether one more request fits. It touches no mutex and calls no
// clock itself, so every boundary — including a timestamp exactly window-old
// — is a one-line table test.
func Decide(timestamps []time.Time, now time.Time, window time.Duration, limit int) (kept []time.Time, allowed bool) {
	cutoff := now.Add(-window)

	kept = timestamps[:0:0] // fresh backing array; never alias the caller's slice
	for _, ts := range timestamps {
		if ts.After(cutoff) {
			kept = append(kept, ts)
		}
	}

	if len(kept)+1 > limit {
		return kept, false
	}
	return append(kept, now), true
}

// Limiter enforces Decide per caller, guarding the per-caller history with a
// mutex. The zero value is not ready to use; construct with NewLimiter.
type Limiter struct {
	mu       sync.Mutex
	limit    int
	window   time.Duration
	requests map[string][]time.Time
}

// NewLimiter builds a Limiter admitting at most limit requests per window,
// per caller key.
func NewLimiter(limit int, window time.Duration) *Limiter {
	return &Limiter{
		limit:    limit,
		window:   window,
		requests: make(map[string][]time.Time),
	}
}

// Allow reports whether caller may proceed at now. It prunes expired history
// and, if the request is admitted, records now — all inside one critical
// section, so two concurrent calls for the same caller never both read the
// same stale history and both admit past the limit.
func (l *Limiter) Allow(caller string, now time.Time) bool {
	l.mu.Lock()
	defer l.mu.Unlock()

	kept, allowed := Decide(l.requests[caller], now, l.window, l.limit)
	l.requests[caller] = kept
	return allowed
}
```

### The runnable demo

The demo sends five requests from one caller against a 3-per-second limit:
the first three are admitted, the fourth is rejected for arriving inside the
same window, and the fifth is admitted a little over a second later once the
earliest requests have aged out.

Create `cmd/demo/main.go`:

```go
package main

import (
	"fmt"
	"time"

	slidingwindow "example.com/sliding-window-rate-limiter"
)

func main() {
	limiter := slidingwindow.NewLimiter(3, time.Second)
	t0 := time.Date(2024, 1, 1, 0, 0, 0, 0, time.UTC)

	step := func(label string, offset time.Duration) {
		now := t0.Add(offset)
		allowed := limiter.Allow("caller-a", now)
		fmt.Printf("%-28s offset=%-8v allowed=%v\n", label, offset, allowed)
	}

	step("req 1", 0)
	step("req 2", 100*time.Millisecond)
	step("req 3", 200*time.Millisecond)
	step("req 4 (over limit)", 300*time.Millisecond)
	step("req 5 (after req1 ages out)", 1100*time.Millisecond)
}
```

Run it:

```bash
go run ./cmd/demo
```

Expected output:

```
req 1                        offset=0s       allowed=true
req 2                        offset=100ms    allowed=true
req 3                        offset=200ms    allowed=true
req 4 (over limit)           offset=300ms    allowed=false
req 5 (after req1 ages out)  offset=1.1s     allowed=true
```

### Tests

The table drives `Decide` directly through the boundary cases, and a
sequential test walks `Limiter.Allow` through the same scenario as the demo,
plus a check that two different caller keys never share a window.

Create `window_test.go`:

```go
package slidingwindow

import (
	"testing"
	"time"
)

func TestDecide(t *testing.T) {
	t.Parallel()

	now := time.Date(2024, 1, 1, 0, 0, 0, 0, time.UTC)
	window := time.Second

	tests := []struct {
		name        string
		timestamps  []time.Time
		limit       int
		wantAllowed bool
		wantKept    int
	}{
		{
			name:        "empty history admits",
			timestamps:  nil,
			limit:       3,
			wantAllowed: true,
			wantKept:    1,
		},
		{
			name:        "under the limit admits",
			timestamps:  []time.Time{now.Add(-500 * time.Millisecond)},
			limit:       3,
			wantAllowed: true,
			wantKept:    2,
		},
		{
			name: "at the limit rejects",
			timestamps: []time.Time{
				now.Add(-900 * time.Millisecond),
				now.Add(-500 * time.Millisecond),
				now.Add(-100 * time.Millisecond),
			},
			limit:       3,
			wantAllowed: false,
			wantKept:    3,
		},
		{
			name:        "timestamp exactly window-old is pruned, not kept",
			timestamps:  []time.Time{now.Add(-window)},
			limit:       1,
			wantAllowed: true,
			wantKept:    1,
		},
		{
			name: "one entry ages out, freeing room to admit",
			timestamps: []time.Time{
				now.Add(-window - time.Millisecond), // aged out
				now.Add(-200 * time.Millisecond),    // still in window
			},
			limit:       2,
			wantAllowed: true,
			wantKept:    2,
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			kept, allowed := Decide(tc.timestamps, now, window, tc.limit)
			if allowed != tc.wantAllowed {
				t.Errorf("allowed = %v, want %v", allowed, tc.wantAllowed)
			}
			if len(kept) != tc.wantKept {
				t.Errorf("len(kept) = %d, want %d", len(kept), tc.wantKept)
			}
		})
	}
}

func TestLimiterAllowSequence(t *testing.T) {
	t.Parallel()

	l := NewLimiter(3, time.Second)
	t0 := time.Date(2024, 1, 1, 0, 0, 0, 0, time.UTC)

	if !l.Allow("caller-a", t0) {
		t.Fatal("request 1 should be allowed")
	}
	if !l.Allow("caller-a", t0.Add(100*time.Millisecond)) {
		t.Fatal("request 2 should be allowed")
	}
	if !l.Allow("caller-a", t0.Add(200*time.Millisecond)) {
		t.Fatal("request 3 should be allowed")
	}
	if l.Allow("caller-a", t0.Add(300*time.Millisecond)) {
		t.Fatal("request 4 should be rejected, over the limit")
	}
	if !l.Allow("caller-a", t0.Add(1100*time.Millisecond)) {
		t.Fatal("request 5 should be allowed once earlier requests age out")
	}

	// A distinct caller has its own independent window.
	if !l.Allow("caller-b", t0.Add(300*time.Millisecond)) {
		t.Fatal("a different caller must not be affected by caller-a's history")
	}
}
```

Verify: `go test -count=1 ./...`

## Review

`Decide`'s pruning always builds a fresh slice (`timestamps[:0:0]`) rather
than reusing the input's backing array, which matters the moment `Allow`
stores the result back into the map: aliasing the caller's old slice would
mean a later `append` inside `Decide` could silently corrupt a slice another
goroutine still holds a reference to during a race-prone read. Carry this
forward: whenever a pure decision function prunes and returns a slice
derived from its input, build a genuinely independent one rather than
trimming and hoping nothing else observes the shared backing array.

## Resources

- [Cloudflare: How to build a rate limiter](https://blog.cloudflare.com/counting-things-a-lot-of-different-things/) — a production comparison of sliding-window and token-bucket rate limiting.
- [time.Time.After](https://pkg.go.dev/time#Time.After) — the exact comparison this module's boundary decision depends on.
- [sync.Mutex](https://pkg.go.dev/sync#Mutex) — the primitive guarding the per-caller history.

---

Back to [00-concepts.md](00-concepts.md) | Prev: [24-graceful-shutdown-drain-and-timeout.md](24-graceful-shutdown-drain-and-timeout.md) | Next: [26-admission-control-load-shedding.md](26-admission-control-load-shedding.md)
