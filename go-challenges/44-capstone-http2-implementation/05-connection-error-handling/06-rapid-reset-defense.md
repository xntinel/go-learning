# Exercise 6: Rapid Reset Defense (CVE-2023-44487)

The HTTP/2 Rapid Reset attack opens a stream and immediately resets it, over and over: each `RST_STREAM` frees the concurrency slot instantly, so the attacker never trips `MAX_CONCURRENT_STREAMS` while the server still pays the per-stream setup cost for every request. This module builds `RapidResetGuard`, which counts stream cancellations that land within a short window of the stream opening and tears the connection down with `ENHANCE_YOUR_CALM` once that rate exceeds a budget — without punishing the legitimate client that cancels a long-running request.

This module is fully self-contained. It begins with its own `go mod init`, defines every type it needs, and ships its own demo and tests. Nothing here imports any other exercise.

## What you'll build

```text
rapidreset.go          ErrCode, RapidResetGuard, RecordReset, Disposition; ErrRapidReset
cmd/
  demo/
    main.go            an attacker trips the guard; a slow client never does
rapidreset_test.go     trips over budget, legitimate cancels ignored, spread-out resets ok
```

- Files: `rapidreset.go`, `cmd/demo/main.go`, `rapidreset_test.go`.
- Implement: `RapidResetGuard` with `RecordReset`, `RapidCount`, and `Disposition`, built on an injected clock.
- Test: `rapidreset_test.go` checks the budget trips, that a cancellation older than the rapid threshold never counts, and that resets spread across more than the window never trip.
- Verify: `go test -race ./...`

Set up the module:

```bash
mkdir -p rapid-reset-defense/cmd/demo && cd rapid-reset-defense
go mod init example.com/rapid-reset-defense
go mod edit -go=1.26
```

### Why "rapid" is the whole signal

A naive defense that rate-limits every `RST_STREAM` is itself a denial of service: real clients cancel streams all the time — a user navigates away, a `fetch` is aborted, a request times out — and those cancellations are legitimate. What makes CVE-2023-44487 distinctive is the *timing*: the attacker resets a stream almost the instant it opens, because the goal is to churn through stream setups, not to abort real work. `RapidResetGuard` encodes exactly that distinction. `RecordReset(openedAt)` measures how long the stream lived before the reset; if that age is at or beyond the `rapidWithin` threshold the cancellation is treated as legitimate and ignored, and only a reset that lands inside the threshold counts toward the abuse budget. A long-running stream that a client cancels after seconds never moves the counter, so well-behaved clients are never throttled.

The counting itself is a sliding window over an injected clock. The guard keeps the timestamps of recent rapid resets, prunes any older than `window` on each call, appends the current one, and returns `ErrRapidReset` when the count in the window exceeds `maxResets`. Injecting the clock — `now func() time.Time` rather than a call to `time.Now` inside — is what makes the behavior testable to the microsecond: a test advances a fake clock by exact amounts and asserts the precise reset on which the guard trips, with no sleeping and no scheduler dependence. When the guard fires, the connection's disposition is a GOAWAY with `ENHANCE_YOUR_CALM` (0xb), the polite "you are abusing me" code, which `Disposition` returns so the caller does not have to hard-code it.

Create `rapidreset.go`:

```go
// Package rapidreset defends against the HTTP/2 Rapid Reset attack
// (CVE-2023-44487): a peer that opens a stream and immediately cancels it,
// repeatedly, to make the server do per-stream work without hitting the
// concurrent-stream limit. The guard caps the rate of rapid cancellations and
// signals ENHANCE_YOUR_CALM when it is exceeded.
package rapidreset

import (
	"errors"
	"fmt"
	"sync"
	"time"
)

// ErrCode is the 32-bit HTTP/2 error code.
type ErrCode uint32

const ErrCodeEnhanceYourCalm ErrCode = 0xb

func (c ErrCode) String() string {
	if c == ErrCodeEnhanceYourCalm {
		return "ENHANCE_YOUR_CALM"
	}
	return fmt.Sprintf("UNKNOWN_ERROR_CODE_0x%x", uint32(c))
}

// ErrRapidReset signals the Rapid Reset attack: the peer is cancelling streams
// faster than the configured budget. The connection must be torn down with
// ENHANCE_YOUR_CALM.
var ErrRapidReset = errors.New("rapid reset: stream-cancellation rate exceeded")

// RapidResetGuard caps the rate of rapid stream cancellations over a sliding
// window. All methods are safe for concurrent use.
type RapidResetGuard struct {
	mu          sync.Mutex
	window      time.Duration
	maxResets   int
	rapidWithin time.Duration
	now         func() time.Time
	events      []time.Time
}

// NewRapidResetGuard returns a guard that trips when more than maxResets rapid
// cancellations occur within window. A cancellation counts as "rapid" only if
// the stream was reset within rapidWithin of opening; later cancellations are
// treated as legitimate. now supplies the clock; nil defaults to time.Now.
func NewRapidResetGuard(maxResets int, window, rapidWithin time.Duration, now func() time.Time) *RapidResetGuard {
	if now == nil {
		now = time.Now
	}
	if maxResets < 1 {
		maxResets = 1
	}
	return &RapidResetGuard{
		window:      window,
		maxResets:   maxResets,
		rapidWithin: rapidWithin,
		now:         now,
	}
}

func (g *RapidResetGuard) pruneLocked(now time.Time) {
	cutoff := now.Add(-g.window)
	kept := g.events[:0]
	for _, t := range g.events {
		if t.After(cutoff) {
			kept = append(kept, t)
		}
	}
	g.events = kept
}

// RecordReset records that a stream opened at openedAt has just been reset.
// A reset within rapidWithin of the opening counts toward the abuse budget; a
// stream that ran longer before being cancelled is treated as legitimate and
// ignored. RecordReset returns ErrRapidReset when the count of rapid resets in
// the sliding window exceeds the budget.
func (g *RapidResetGuard) RecordReset(openedAt time.Time) error {
	g.mu.Lock()
	defer g.mu.Unlock()

	now := g.now()
	if now.Sub(openedAt) >= g.rapidWithin {
		return nil // legitimate cancellation, not counted
	}

	g.pruneLocked(now)
	g.events = append(g.events, now)
	if len(g.events) > g.maxResets {
		return fmt.Errorf("%w: %d rapid resets within %s (budget %d)",
			ErrRapidReset, len(g.events), g.window, g.maxResets)
	}
	return nil
}

// RapidCount returns the number of rapid resets currently within the window.
func (g *RapidResetGuard) RapidCount() int {
	g.mu.Lock()
	defer g.mu.Unlock()
	g.pruneLocked(g.now())
	return len(g.events)
}

// Disposition returns the error code an endpoint sends when ErrRapidReset
// fires: a GOAWAY carrying ENHANCE_YOUR_CALM.
func (g *RapidResetGuard) Disposition() ErrCode { return ErrCodeEnhanceYourCalm }
```

### The runnable demo

The demo runs an attacker that opens and instantly resets streams until the guard trips, then runs a well-behaved client that cancels long-running streams and is never flagged. Both drive the same injected clock, so the output is exact.

Create `cmd/demo/main.go`:

```go
package main

import (
	"fmt"
	"time"

	"example.com/rapid-reset-defense"
)

type clock struct{ t time.Time }

func (c *clock) Now() time.Time { return c.t }

func main() {
	c := &clock{t: time.Unix(0, 0).UTC()}
	g := rapidreset.NewRapidResetGuard(5, time.Second, 100*time.Millisecond, c.Now)

	// Attacker: open a stream, reset it 1ms later, repeat.
	for i := 1; i <= 8; i++ {
		opened := c.Now()
		c.t = c.t.Add(time.Millisecond)
		if err := g.RecordReset(opened); err != nil {
			fmt.Printf("connection torn down after %d rapid resets: %v\n", i, err)
			fmt.Println("disposition:", g.Disposition())
			break
		}
	}

	// Well-behaved client: cancels streams that ran 500ms, beyond the rapid
	// threshold, so they never count toward the abuse budget.
	g2 := rapidreset.NewRapidResetGuard(5, time.Second, 100*time.Millisecond, c.Now)
	clean := true
	for i := 0; i < 20; i++ {
		opened := c.Now().Add(-500 * time.Millisecond)
		if err := g2.RecordReset(opened); err != nil {
			clean = false
		}
		c.t = c.t.Add(time.Millisecond)
	}
	if clean {
		fmt.Println("20 legitimate cancellations: no abuse detected")
	}
}
```

Run it:

```bash
go run ./cmd/demo
```

Expected output:

```
connection torn down after 6 rapid resets: rapid reset: stream-cancellation rate exceeded: 6 rapid resets within 1s (budget 5)
disposition: ENHANCE_YOUR_CALM
20 legitimate cancellations: no abuse detected
```

### Tests

`TestTripsOverBudget` resets streams immediately at a fixed clock and asserts the first five pass and the sixth returns `ErrRapidReset`. `TestLegitimateCancelNotCounted` records twenty cancellations of streams that lived well past the rapid threshold and asserts none count. `TestSpreadOutResetsDoNotTrip` advances the clock by more than the window between each reset and asserts the sliding window keeps the count at one, so a low budget is never exceeded. `TestDisposition` pins the ENHANCE_YOUR_CALM code.

Create `rapidreset_test.go`:

```go
package rapidreset

import (
	"errors"
	"sync"
	"testing"
	"time"
)

// fakeClock is a manually advanced clock for deterministic timing tests.
type fakeClock struct {
	mu sync.Mutex
	t  time.Time
}

func (c *fakeClock) Now() time.Time {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.t
}

func (c *fakeClock) Advance(d time.Duration) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.t = c.t.Add(d)
}

func TestTripsOverBudget(t *testing.T) {
	t.Parallel()
	c := &fakeClock{t: time.Unix(0, 0)}
	g := NewRapidResetGuard(5, time.Second, 100*time.Millisecond, c.Now)

	// Five immediate resets are within budget.
	for i := 1; i <= 5; i++ {
		opened := c.Now()
		c.Advance(time.Millisecond)
		if err := g.RecordReset(opened); err != nil {
			t.Fatalf("reset %d = %v, want nil (within budget)", i, err)
		}
	}
	// The sixth rapid reset exceeds the budget.
	opened := c.Now()
	c.Advance(time.Millisecond)
	if err := g.RecordReset(opened); !errors.Is(err, ErrRapidReset) {
		t.Fatalf("sixth reset = %v, want ErrRapidReset", err)
	}
}

func TestLegitimateCancelNotCounted(t *testing.T) {
	t.Parallel()
	c := &fakeClock{t: time.Unix(100, 0)}
	g := NewRapidResetGuard(5, time.Second, 100*time.Millisecond, c.Now)

	for i := 0; i < 20; i++ {
		// Stream ran 500ms before being cancelled: beyond the rapid threshold.
		opened := c.Now().Add(-500 * time.Millisecond)
		if err := g.RecordReset(opened); err != nil {
			t.Fatalf("legitimate cancel %d = %v, want nil", i, err)
		}
		c.Advance(time.Millisecond)
	}
	if got := g.RapidCount(); got != 0 {
		t.Errorf("RapidCount = %d, want 0 (no rapid resets)", got)
	}
}

func TestSpreadOutResetsDoNotTrip(t *testing.T) {
	t.Parallel()
	c := &fakeClock{t: time.Unix(0, 0)}
	// Budget of 1: any two rapid resets within the window would trip.
	g := NewRapidResetGuard(1, 500*time.Millisecond, time.Second, c.Now)

	for i := 0; i < 10; i++ {
		opened := c.Now()
		if err := g.RecordReset(opened); err != nil {
			t.Fatalf("reset %d = %v, want nil (spread beyond window)", i, err)
		}
		// Advance past the window so the prior event is pruned.
		c.Advance(600 * time.Millisecond)
	}
}

func TestDisposition(t *testing.T) {
	t.Parallel()
	g := NewRapidResetGuard(1, time.Second, time.Second, nil)
	if got := g.Disposition(); got != ErrCodeEnhanceYourCalm {
		t.Errorf("Disposition = %v, want ENHANCE_YOUR_CALM", got)
	}
	if got := g.Disposition().String(); got != "ENHANCE_YOUR_CALM" {
		t.Errorf("Disposition().String() = %q, want ENHANCE_YOUR_CALM", got)
	}
}
```

## Review

The guard is correct when "rapid" is the only thing it counts and the window genuinely slides. The first mistake is rate-limiting all resets, which throttles legitimate clients that cancel real work — the `rapidWithin` age check is what excludes them, and the legitimate-cancel test proves a stream that lived past the threshold never moves the counter. The second is forgetting to prune, so the count only ever grows and a connection that resets slowly over hours eventually trips on stale events; the spread-out test advances past the window between resets and demands the count stays at one. Inject the clock so the tests are exact rather than sleep-based, and take the mutex around the event slice since a real server records resets from the connection's read goroutine while a metrics reader may call `RapidCount` concurrently — `-race` confirms the pairing. When the guard fires, the connection-level response is GOAWAY with `ENHANCE_YOUR_CALM`, which `Disposition` names.

## Resources

- [CVE-2023-44487 — HTTP/2 Rapid Reset](https://nvd.nist.gov/vuln/detail/CVE-2023-44487) — the official record of the attack this module defends against.
- [The HTTP/2 Rapid Reset attack (Google Cloud blog)](https://cloud.google.com/blog/products/identity-security/google-cloud-mitigated-largest-ddos-attack-peaking-above-398-million-rps) — the engineering write-up of the 2023 record-setting DDoS and its mitigation.
- [Go net/http2 fix CVE-2023-44487 (golang.org/issue/63417)](https://github.com/golang/go/issues/63417) — how the Go standard library added rapid-reset accounting.
- [RFC 9113 §7 — Error Codes](https://httpwg.org/specs/rfc9113.html#ErrorCodes) — the ENHANCE_YOUR_CALM (0xb) code the guard signals.

---

Back to [00-concepts.md](00-concepts.md) | Prev: [05-graceful-drain.md](05-graceful-drain.md) | Next: [07-enhance-your-calm-flood.md](07-enhance-your-calm-flood.md)
