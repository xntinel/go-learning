# Exercise 7: ENHANCE_YOUR_CALM Control-Frame Flood Defense

A peer can keep a connection busy without ever making a request: a stream of PINGs, repeated SETTINGS, empty frames, or no-op WINDOW_UPDATEs all force the server to do work while no stream advances. This module builds `FloodMonitor`, which counts the cheap control frames received since the last time a stream actually made progress and trips `ENHANCE_YOUR_CALM` when that count exceeds a budget — a progress-based defense that complements the rate-based rapid-reset guard.

This module is fully self-contained. It begins with its own `go mod init`, defines every type it needs, and ships its own demo and tests. Nothing here imports any other exercise.

## What you'll build

```text
calm.go                ErrCode, FloodMonitor, ControlFrame, Progress, Disposition; ErrFlood
cmd/
  demo/
    main.go            a pure-control-frame flood trips; interleaved traffic does not
calm_test.go           trips over budget, progress resets the counter, interleaving is safe
```

- Files: `calm.go`, `cmd/demo/main.go`, `calm_test.go`.
- Implement: `FloodMonitor` with `ControlFrame`, `Progress`, `Pending`, and `Disposition`.
- Test: `calm_test.go` checks the budget trips, that a `Progress` call resets the counter, that interleaved progress never trips, and that concurrent use is race-free.
- Verify: `go test -race ./...`

Set up the module:

```bash
mkdir -p go-solutions/44-capstone-http2-implementation/05-connection-error-handling/07-enhance-your-calm-flood/cmd/demo && cd go-solutions/44-capstone-http2-implementation/05-connection-error-handling/07-enhance-your-calm-flood
go mod edit -go=1.26
```

### Progress, not rate, is the discriminator

The rapid-reset guard of the previous module measures a *rate* — cancellations per unit time — because that attack is about churning stream setups quickly. A control-frame flood is different: the attacker does not need speed, only volume that never amortizes into useful work. A legitimate connection sends control frames too — a PING for keepalive, a SETTINGS update, flow-control WINDOW_UPDATEs — but they are punctuated by real progress: HEADERS and DATA frames that advance requests. The signal of abuse is therefore a long run of control frames with *no intervening progress*, exactly the heuristic the Go standard library uses for its "too many PINGs" defense. `FloodMonitor` counts control frames since the last progress event; `ControlFrame` increments and trips `ErrFlood` once the count passes the budget, and `Progress` — called whenever a stream genuinely advances — resets the count to zero. A connection that does real work resets its budget continuously and can never accumulate enough idle control frames to trip, while a pure flood marches straight past the limit.

The monitor holds no clock: the count is purely a function of the frame sequence, which makes the tests trivially deterministic — a sequence of calls produces a fixed count regardless of how fast it runs. The concurrency it must survive is real, though. A server records control frames and progress from the connection's single read goroutine, but a metrics endpoint may read `Pending` from another, so every field lives under one mutex and the suite runs a concurrent test under `-race`. When the monitor fires, the disposition is the same `ENHANCE_YOUR_CALM` (0xb) the rapid-reset guard uses — both attacks converge on the polite "stop abusing me" code — and `Disposition` returns it.

Create `calm.go`:

```go
// Package calm defends against an HTTP/2 control-frame flood: a peer that
// sends cheap control frames (PING, SETTINGS, empty or no-op frames) without
// ever making a stream progress, keeping the server busy for free. The monitor
// counts control frames between progress events and signals ENHANCE_YOUR_CALM
// when the count exceeds a budget.
package calm

import (
	"errors"
	"fmt"
	"sync"
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

// ErrFlood signals a control-frame flood: too many control frames arrived with
// no intervening stream progress. The connection must be torn down with
// ENHANCE_YOUR_CALM.
var ErrFlood = errors.New("control-frame flood: too many control frames without stream progress")

// FloodMonitor counts cheap control frames received since the last time a
// stream made progress. All methods are safe for concurrent use.
type FloodMonitor struct {
	mu      sync.Mutex
	budget  int
	pending int
}

// NewFloodMonitor returns a monitor that trips after more than budget control
// frames arrive with no intervening progress. A budget below 1 is raised to 1.
func NewFloodMonitor(budget int) *FloodMonitor {
	if budget < 1 {
		budget = 1
	}
	return &FloodMonitor{budget: budget}
}

// ControlFrame records one cheap control frame (PING, SETTINGS, PRIORITY, an
// empty frame, or a no-op WINDOW_UPDATE). It returns ErrFlood when the number
// of control frames since the last Progress exceeds the budget.
func (m *FloodMonitor) ControlFrame() error {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.pending++
	if m.pending > m.budget {
		return fmt.Errorf("%w: %d control frames, budget %d", ErrFlood, m.pending, m.budget)
	}
	return nil
}

// Progress records that a stream genuinely advanced (a HEADERS or DATA frame
// that moved a request forward), resetting the control-frame counter.
func (m *FloodMonitor) Progress() {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.pending = 0
}

// Pending returns the number of control frames since the last progress.
func (m *FloodMonitor) Pending() int {
	m.mu.Lock()
	defer m.mu.Unlock()
	return m.pending
}

// Disposition returns the error code an endpoint sends when ErrFlood fires:
// a GOAWAY carrying ENHANCE_YOUR_CALM.
func (m *FloodMonitor) Disposition() ErrCode { return ErrCodeEnhanceYourCalm }
```

### The runnable demo

The demo first sends a pure flood of control frames until the monitor trips, then sends a thousand control frames interleaved with periodic progress and shows it never trips.

Create `cmd/demo/main.go`:

```go
package main

import (
	"fmt"

	"example.com/enhance-your-calm"
)

func main() {
	m := calm.NewFloodMonitor(100)

	// A pure flood: control frames with no request progress.
	for i := 1; ; i++ {
		if err := m.ControlFrame(); err != nil {
			fmt.Printf("flood detected after %d control frames: %v\n", i, err)
			fmt.Println("disposition:", m.Disposition())
			break
		}
	}

	// Normal traffic: control frames punctuated by real progress.
	m2 := calm.NewFloodMonitor(100)
	flooded := false
	for i := 0; i < 1000; i++ {
		if err := m2.ControlFrame(); err != nil {
			flooded = true
			break
		}
		if i%10 == 0 {
			m2.Progress() // a request advances, resetting the counter
		}
	}
	if !flooded {
		fmt.Println("1000 interleaved control frames: no flood")
	}
}
```

Run it:

```bash
go run ./cmd/demo
```

Expected output:

```
flood detected after 101 control frames: control-frame flood: too many control frames without stream progress: 101 control frames, budget 100
disposition: ENHANCE_YOUR_CALM
1000 interleaved control frames: no flood
```

### Tests

`TestTripsOverBudget` sends budget control frames cleanly and asserts the next one trips. `TestProgressResetsCounter` fills the budget, calls `Progress`, then fills it again to prove the counter zeroed. `TestInterleavedNeverTrips` alternates a control frame and a progress event a thousand times and asserts no trip. `TestConcurrentSafe` hammers the monitor from many goroutines so `-race` can validate the locking. `TestDisposition` pins the ENHANCE_YOUR_CALM code.

Create `calm_test.go`:

```go
package calm

import (
	"errors"
	"sync"
	"testing"
)

func TestTripsOverBudget(t *testing.T) {
	t.Parallel()
	m := NewFloodMonitor(3)
	for i := 1; i <= 3; i++ {
		if err := m.ControlFrame(); err != nil {
			t.Fatalf("control frame %d = %v, want nil (within budget)", i, err)
		}
	}
	if err := m.ControlFrame(); !errors.Is(err, ErrFlood) {
		t.Fatalf("fourth control frame = %v, want ErrFlood", err)
	}
}

func TestProgressResetsCounter(t *testing.T) {
	t.Parallel()
	m := NewFloodMonitor(3)
	for i := 0; i < 3; i++ {
		if err := m.ControlFrame(); err != nil {
			t.Fatalf("pre-progress control frame %d = %v, want nil", i, err)
		}
	}
	m.Progress() // resets the counter
	if got := m.Pending(); got != 0 {
		t.Fatalf("Pending after Progress = %d, want 0", got)
	}
	for i := 0; i < 3; i++ {
		if err := m.ControlFrame(); err != nil {
			t.Fatalf("post-progress control frame %d = %v, want nil", i, err)
		}
	}
	if err := m.ControlFrame(); !errors.Is(err, ErrFlood) {
		t.Fatalf("control frame after refilling budget = %v, want ErrFlood", err)
	}
}

func TestInterleavedNeverTrips(t *testing.T) {
	t.Parallel()
	m := NewFloodMonitor(2)
	for i := 0; i < 1000; i++ {
		if err := m.ControlFrame(); err != nil {
			t.Fatalf("interleaved control frame %d = %v, want nil", i, err)
		}
		m.Progress() // every control frame is followed by real progress
	}
}

func TestConcurrentSafe(t *testing.T) {
	t.Parallel()
	m := NewFloodMonitor(1000)
	var wg sync.WaitGroup
	for range 50 {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for range 100 {
				_ = m.ControlFrame()
				m.Progress()
				_ = m.Pending()
			}
		}()
	}
	wg.Wait()
}

func TestDisposition(t *testing.T) {
	t.Parallel()
	m := NewFloodMonitor(1)
	if got := m.Disposition(); got != ErrCodeEnhanceYourCalm {
		t.Errorf("Disposition = %v, want ENHANCE_YOUR_CALM", got)
	}
	if got := m.Disposition().String(); got != "ENHANCE_YOUR_CALM" {
		t.Errorf("Disposition().String() = %q, want ENHANCE_YOUR_CALM", got)
	}
}
```

## Review

The monitor is correct when only progress resets the counter and a control frame never does. The first mistake is resetting on the wrong event — if a PING or a no-op WINDOW_UPDATE reset the count, the flood would never be detected because the attacker's own frames would keep clearing the budget; only a HEADERS or DATA frame that genuinely advances a request may call `Progress`. The second is choosing a budget so low that ordinary keepalive PINGs trip it; the budget must sit comfortably above the control-frame traffic a healthy connection emits between requests. Keep the count purely a function of the frame sequence with no clock, which is what makes the tests deterministic, and guard the two fields with the mutex since the read goroutine and a metrics reader touch them concurrently — `-race` confirms it. This progress-based defense and the rate-based rapid-reset guard are complementary: a robust server runs both, and both close the connection with `ENHANCE_YOUR_CALM`.

## Resources

- [RFC 9113 §10.5 — DoS Considerations](https://httpwg.org/specs/rfc9113.html#DoSConsiderations) — the catalog of cheap-frame abuse vectors this monitor counters.
- [CVE-2019-9512 / CVE-2019-9515 — Ping and Settings Floods](https://kb.cert.org/vuls/id/605641) — the documented HTTP/2 flooding attacks built from control frames.
- [Go net/http2 flood limits (golang.org source)](https://pkg.go.dev/golang.org/x/net/http2) — the production server's control-frame and ping accounting this module mirrors.
- [RFC 9113 §7 — Error Codes](https://httpwg.org/specs/rfc9113.html#ErrorCodes) — the ENHANCE_YOUR_CALM (0xb) code the monitor signals.

---

Back to [00-concepts.md](00-concepts.md) | Prev: [06-rapid-reset-defense.md](06-rapid-reset-defense.md) | Next: [../06-full-http2-server/00-concepts.md](../06-full-http2-server/00-concepts.md)
