# Exercise 3: Passive Outlier Detection and Ejection

Active probes miss the backend that passes `/healthz` while failing real requests, so the data plane also watches the outcomes of the traffic it forwards. This exercise builds that passive detector: a sliding error-rate window per backend, an ejection that fires when the rate spikes, the never-eject-the-last-backend invariant that keeps routing alive, a timed restoration, and the careful lock ordering that makes all of it correct under concurrency.

This module is fully self-contained. It bundles the minimal state machine and cooldown it needs, starts with its own `go mod init`, and ships its own demo and tests. Nothing here imports any other exercise.

## What you'll build

```text
eject.go             State, Config, Checker, New, RecordOutcome, tryRestore, hasOtherHealthy, ejectionCooldown
cmd/
  demo/
    main.go          eject one of two backends, then watch it restore after cooldown
eject_test.go        ejection, the last-backend invariant, exponential cooldown, restore (synctest), concurrency
```

- Files: `eject.go`, `cmd/demo/main.go`, `eject_test.go`.
- Implement: `New`, `(*Checker).RecordOutcome`, `(*Checker).tryRestore`, `(*Checker).hasOtherHealthy`, `(*Checker).ejectionCooldown`, `(*Checker).StateOf`, `(*Checker).IsHealthy`.
- Test: `eject_test.go` ejects a backend whose error rate spikes, proves the last healthy backend is never ejected, checks the cooldown schedule, restores an ejected backend to `unhealthy` after the cooldown under a fake clock, and hammers `RecordOutcome` concurrently under `-race`.
- Verify: `go test -race ./...`

Set up the module:

```bash
mkdir -p outlier-ejection/cmd/demo && cd outlier-ejection
go mod init example.com/outlier-ejection
go mod edit -go=1.26
```

### The ejection path, step by step

`RecordOutcome` is called once per real request the data plane forwards, so it runs hot and must be both cheap and correct under heavy concurrency. It does five things in order, and the ordering is the whole design.

First it appends the timestamped outcome and trims everything older than `ErrorRateWindow`, so the slice always holds exactly the current window. Then it computes `failures / total` over what remains — but only if there are at least `MinSamples` observations and the backend is not already ejected; a backend with three requests and one failure has a 33% rate that means nothing, and a backend already on cooldown must not be re-evaluated. If the rate is below `ErrorRateThreshold`, it returns.

The interesting part is the commit. Before ejecting, the detector must enforce the never-eject-the-last-backend invariant, which requires counting healthy peers — and that count walks the entries map and locks each entry. Here the lock ordering bites: the rule is *map lock before entry lock, always*, so the peer count must run while this backend's own entry lock is *not* held. The code therefore releases `e.mu`, calls `hasOtherHealthy` (which takes the map read lock and each entry lock cleanly), and only if a healthy peer exists does it re-acquire `e.mu` to commit. The re-acquire is paired with a re-check: between the release and the re-acquire, a concurrent `RecordOutcome` on the same backend could already have ejected it, so the committer bails out if `e.state == StateEjected`. This release-check-reacquire-recheck dance is the standard shape when a decision spans two critical sections and cannot be made atomic without inverting a lock order. The race window is real but narrow, and its worst outcome — two backends ejected at nearly the same instant — is self-healing through the restoration timers.

On commit it bumps `consecutiveEjections`, computes the exponential cooldown, sets the state and `ejectedUntil`, clears the outcome window (so stale failures do not re-eject the backend the moment it returns), emits a `StateChange`, and schedules `tryRestore` with `time.AfterFunc`. When the cooldown fires, `tryRestore` moves the backend to `Unhealthy` — not `Healthy` — so the active checker, the only system that can confirm the backend actually works, makes the final call on readmission.

Create `eject.go`:

```go
package healthcheck

import (
	"fmt"
	"math"
	"sync"
	"time"
)

// State is the health state of a single backend address.
type State int

const (
	StateHealthy   State = iota // in the pool
	StateUnhealthy              // out of the pool; awaiting active-probe recovery
	StateEjected                // passively ejected; on cooldown
)

// String returns a human-readable label for the state.
func (s State) String() string {
	switch s {
	case StateHealthy:
		return "healthy"
	case StateUnhealthy:
		return "unhealthy"
	case StateEjected:
		return "ejected"
	default:
		return fmt.Sprintf("State(%d)", int(s))
	}
}

// StateChange is delivered on Checker.Notifications on every transition.
type StateChange struct {
	Addr  string
	State State
}

// Config holds the passive-detection parameters.
type Config struct {
	ErrorRateWindow    time.Duration // sliding window over which the rate is computed
	ErrorRateThreshold float64       // [0,1]; eject when the rate exceeds this
	MinSamples         int           // minimum observations before the rate is trusted
	BaseEjectionTime   time.Duration // cooldown for the first ejection
	MaxEjectionTime    time.Duration // cooldown ceiling for repeated ejections
}

// outcome is one timestamped request result.
type outcome struct {
	ts      time.Time
	success bool
}

// entry holds the mutable state for one backend. Its mu is acquired only after
// Checker.mu, never the reverse.
type entry struct {
	mu                   sync.Mutex
	state                State
	consecutiveEjections int
	outcomes             []outcome
	ejectedUntil         time.Time
}

// Checker runs passive outlier detection for a fixed set of backends.
type Checker struct {
	cfg     Config
	mu      sync.RWMutex      // guards entries; acquired before any entry.mu
	entries map[string]*entry // key = "host:port"

	// Notifications receives a StateChange on every transition. It is buffered.
	Notifications chan StateChange
}

// New creates a Checker for addrs, all initially healthy.
func New(cfg Config, addrs []string) *Checker {
	c := &Checker{
		cfg:           cfg,
		entries:       make(map[string]*entry, len(addrs)),
		Notifications: make(chan StateChange, 64),
	}
	for _, addr := range addrs {
		c.entries[addr] = &entry{state: StateHealthy}
	}
	return c
}

// RecordOutcome records one real request result for addr and ejects the backend
// if its sliding-window error rate exceeds the threshold, unless addr is the
// last healthy backend.
func (c *Checker) RecordOutcome(addr string, success bool) {
	c.mu.RLock()
	e, ok := c.entries[addr]
	c.mu.RUnlock()
	if !ok {
		return
	}

	now := time.Now()

	e.mu.Lock()
	// Append and trim observations outside the window.
	e.outcomes = append(e.outcomes, outcome{ts: now, success: success})
	cutoff := now.Add(-c.cfg.ErrorRateWindow)
	i := 0
	for i < len(e.outcomes) && e.outcomes[i].ts.Before(cutoff) {
		i++
	}
	e.outcomes = e.outcomes[i:]

	// Do not evaluate an already-ejected backend or one with too little history.
	if e.state == StateEjected || len(e.outcomes) < c.cfg.MinSamples {
		e.mu.Unlock()
		return
	}
	failures := 0
	for _, o := range e.outcomes {
		if !o.success {
			failures++
		}
	}
	rate := float64(failures) / float64(len(e.outcomes))
	shouldEject := rate > c.cfg.ErrorRateThreshold

	// Release the entry lock before hasOtherHealthy to preserve the lock order:
	// map lock is always acquired before an entry lock, never the reverse.
	e.mu.Unlock()

	if !shouldEject {
		return
	}
	if !c.hasOtherHealthy(addr) {
		return // never eject the last healthy backend
	}

	// Re-acquire to commit; re-check state in case a concurrent path ejected it.
	e.mu.Lock()
	if e.state == StateEjected {
		e.mu.Unlock()
		return
	}
	e.consecutiveEjections++
	cooldown := c.ejectionCooldown(e.consecutiveEjections)
	e.state = StateEjected
	e.ejectedUntil = now.Add(cooldown)
	e.outcomes = e.outcomes[:0] // reset the window after ejection
	e.mu.Unlock()

	select {
	case c.Notifications <- StateChange{Addr: addr, State: StateEjected}:
	default:
	}
	time.AfterFunc(cooldown, func() { c.tryRestore(addr) })
}

// tryRestore runs after the ejection cooldown and moves the backend to
// unhealthy so active probes can bring it back to healthy.
func (c *Checker) tryRestore(addr string) {
	c.mu.RLock()
	e, ok := c.entries[addr]
	c.mu.RUnlock()
	if !ok {
		return
	}
	e.mu.Lock()
	if e.state == StateEjected && !time.Now().Before(e.ejectedUntil) {
		e.state = StateUnhealthy
		e.mu.Unlock()
		select {
		case c.Notifications <- StateChange{Addr: addr, State: StateUnhealthy}:
		default:
		}
		return
	}
	e.mu.Unlock()
}

// hasOtherHealthy reports whether any backend other than excludeAddr is healthy.
// It takes the map read lock and each entry lock in turn, so callers must hold
// no entry lock when calling it.
func (c *Checker) hasOtherHealthy(excludeAddr string) bool {
	c.mu.RLock()
	defer c.mu.RUnlock()
	for addr, e := range c.entries {
		if addr == excludeAddr {
			continue
		}
		e.mu.Lock()
		s := e.state
		e.mu.Unlock()
		if s == StateHealthy {
			return true
		}
	}
	return false
}

// ejectionCooldown is the exponential cooldown for the nth ejection.
func (c *Checker) ejectionCooldown(n int) time.Duration {
	d := float64(c.cfg.BaseEjectionTime) * math.Pow(2, float64(n-1))
	if d > float64(c.cfg.MaxEjectionTime) {
		return c.cfg.MaxEjectionTime
	}
	return time.Duration(d)
}

// StateOf returns the current State for addr and whether addr is known.
func (c *Checker) StateOf(addr string) (State, bool) {
	c.mu.RLock()
	e, ok := c.entries[addr]
	c.mu.RUnlock()
	if !ok {
		return 0, false
	}
	e.mu.Lock()
	s := e.state
	e.mu.Unlock()
	return s, true
}

// IsHealthy reports whether addr is currently healthy.
func (c *Checker) IsHealthy(addr string) bool {
	s, ok := c.StateOf(addr)
	return ok && s == StateHealthy
}
```

The two notification sends and the `AfterFunc` call all happen *after* the relevant `e.mu.Unlock()`, for the same reason as in the active checker: a channel send or a timer registration must never run inside the entry's critical section. `hasOtherHealthy` is the function the lock ordering is built around — it takes the map lock then each entry lock, the canonical order, so as long as no caller invokes it while holding an entry lock the system cannot deadlock. The `ejectionCooldown` here uses `math.Pow` for brevity within the detector; the cooldown exercise builds the overflow-safe integer-space version, which is what a production policy with no bound on the ejection count would use.

### The runnable demo

The demo makes the invariant visible. Two backends start healthy; ten failures on `a:80` drive its error rate to 1.0 and eject it, while `b:80` — the surviving healthy peer — is what makes that ejection permissible. After a short wait past the base cooldown, the restoration timer has moved `a:80` to `unhealthy`, ready for active probes to readmit it. All three printed states are deterministic: the ejection is synchronous in `RecordOutcome`, and the wait is three times the base cooldown, comfortably past when `AfterFunc` fires.

Create `cmd/demo/main.go`:

```go
package main

import (
	"fmt"
	"time"

	"example.com/outlier-ejection"
)

func main() {
	cfg := healthcheck.Config{
		ErrorRateWindow:    300 * time.Millisecond,
		ErrorRateThreshold: 0.5,
		MinSamples:         5,
		BaseEjectionTime:   50 * time.Millisecond,
		MaxEjectionTime:    500 * time.Millisecond,
	}
	c := healthcheck.New(cfg, []string{"a:80", "b:80"})

	fmt.Println("a:80 healthy before failures:", c.IsHealthy("a:80"))
	fmt.Println("b:80 healthy:                ", c.IsHealthy("b:80"))

	for i := 0; i < 10; i++ {
		c.RecordOutcome("a:80", false)
	}
	sa, _ := c.StateOf("a:80")
	fmt.Println("a:80 state after high error rate:", sa)
	fmt.Println("b:80 still healthy (last-backend protected):", c.IsHealthy("b:80"))

	time.Sleep(3 * cfg.BaseEjectionTime)
	sa, _ = c.StateOf("a:80")
	fmt.Println("a:80 state after cooldown:       ", sa)
}
```

Run it:

```bash
go run ./cmd/demo
```

Expected output:

```
a:80 healthy before failures: true
b:80 healthy:                 true
a:80 state after high error rate: ejected
b:80 still healthy (last-backend protected): true
a:80 state after cooldown:        unhealthy
```

### Tests

`TestPassiveOutlierEjectsBackend` drives a high error rate on one of two backends and asserts it ejects while the peer stays healthy. `TestTooFewSamplesNoEjection` proves the `MinSamples` guard. `TestLastBackendNeverEjected` records twenty failures on a lone backend and asserts it is never ejected. `TestEjectionCooldownExponential` pins the cooldown schedule. `TestPassiveOutlierIgnoresEjectedBackend` keeps recording failures on an already-ejected backend and asserts `consecutiveEjections` does not climb during one ejection, reaching into the unexported `entry` because the test is in-package. `TestEjectAndRestore` runs the eject-then-restore cycle inside `synctest.Test`, where `time.Sleep` advances a fake clock so the `AfterFunc` restoration fires deterministically and `synctest.Wait` blocks until it has. `TestConcurrentRecordOutcomeNoPanic` hammers the detector from many goroutines under `-race` to exercise the release-recheck lock dance.

Create `eject_test.go`:

```go
package healthcheck

import (
	"sync"
	"sync/atomic"
	"testing"
	"testing/synctest"
	"time"
)

func cfg() Config {
	return Config{
		ErrorRateWindow:    200 * time.Millisecond,
		ErrorRateThreshold: 0.5,
		MinSamples:         5,
		BaseEjectionTime:   50 * time.Millisecond,
		MaxEjectionTime:    200 * time.Millisecond,
	}
}

func TestPassiveOutlierEjectsBackend(t *testing.T) {
	t.Parallel()
	c := New(cfg(), []string{"a:80", "b:80"})
	for i := 0; i < 10; i++ {
		c.RecordOutcome("a:80", false)
	}
	if s, _ := c.StateOf("a:80"); s != StateEjected {
		t.Fatalf("state = %s, want ejected", s)
	}
	if !c.IsHealthy("b:80") {
		t.Fatal("b:80 should still be healthy")
	}
}

func TestTooFewSamplesNoEjection(t *testing.T) {
	t.Parallel()
	c := New(cfg(), []string{"a:80", "b:80"})
	for i := 0; i < cfg().MinSamples-1; i++ {
		c.RecordOutcome("a:80", false)
	}
	if s, _ := c.StateOf("a:80"); s == StateEjected {
		t.Fatal("must not eject below MinSamples")
	}
}

func TestLastBackendNeverEjected(t *testing.T) {
	t.Parallel()
	c := New(cfg(), []string{"a:80"})
	for i := 0; i < 20; i++ {
		c.RecordOutcome("a:80", false)
	}
	if s, _ := c.StateOf("a:80"); s == StateEjected {
		t.Fatal("last backend must never be ejected")
	}
}

func TestEjectionCooldownExponential(t *testing.T) {
	t.Parallel()
	c := New(cfg(), nil)
	cases := []struct {
		n    int
		want time.Duration
	}{
		{1, 50 * time.Millisecond},
		{2, 100 * time.Millisecond},
		{3, 200 * time.Millisecond},
		{4, 200 * time.Millisecond},
	}
	for _, tc := range cases {
		if got := c.ejectionCooldown(tc.n); got != tc.want {
			t.Errorf("ejectionCooldown(%d) = %v, want %v", tc.n, got, tc.want)
		}
	}
}

func TestPassiveOutlierIgnoresEjectedBackend(t *testing.T) {
	t.Parallel()
	c := New(cfg(), []string{"a:80", "b:80"})
	for i := 0; i < 10; i++ {
		c.RecordOutcome("a:80", false)
	}
	e := c.entries["a:80"]
	e.mu.Lock()
	first := e.consecutiveEjections
	e.mu.Unlock()
	for i := 0; i < 10; i++ {
		c.RecordOutcome("a:80", false)
	}
	e.mu.Lock()
	again := e.consecutiveEjections
	e.mu.Unlock()
	if first != 1 || again != 1 {
		t.Fatalf("consecutiveEjections changed while ejected: first=%d again=%d", first, again)
	}
}

func TestEjectAndRestore(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		c := New(cfg(), []string{"a:80", "b:80"})
		for i := 0; i < 10; i++ {
			c.RecordOutcome("a:80", false)
		}
		if s, _ := c.StateOf("a:80"); s != StateEjected {
			t.Fatalf("state = %s, want ejected", s)
		}
		time.Sleep(60 * time.Millisecond)
		synctest.Wait()
		if s, _ := c.StateOf("a:80"); s != StateUnhealthy {
			t.Fatalf("state = %s, want unhealthy after cooldown", s)
		}
	})
}

func TestConcurrentRecordOutcomeNoPanic(t *testing.T) {
	t.Parallel()
	addrs := []string{"a:80", "b:80", "c:80"}
	c := New(cfg(), addrs)
	var wg sync.WaitGroup
	var panics atomic.Int64
	const workers = 60
	for _, addr := range addrs {
		for i := 0; i < workers; i++ {
			wg.Add(1)
			go func(a string, idx int) {
				defer wg.Done()
				defer func() {
					if r := recover(); r != nil {
						panics.Add(1)
					}
				}()
				c.RecordOutcome(a, idx%3 != 0)
			}(addr, i)
		}
	}
	wg.Wait()
	if panics.Load() != 0 {
		t.Fatalf("panics under concurrency: %d", panics.Load())
	}
}
```

## Review

The detector is correct when it ejects on a sustained high error rate, never ejects the last healthy backend, restores to `unhealthy` (never straight to `healthy`) after the cooldown, and survives concurrent `RecordOutcome` under `-race`. Confirm the `MinSamples` guard suppresses ejection on a thin history, that the outcome window is cleared at ejection so a restored backend does not re-eject on its first request, and that recording on an already-ejected backend does not climb `consecutiveEjections` a second time within one ejection. The `synctest` restore test makes the timer deterministic; the concurrency test exercises the release-recheck lock dance that the lock ordering depends on.

Common mistakes for this feature. Calling `hasOtherHealthy` while still holding `e.mu` inverts the map-before-entry lock order and deadlocks under concurrent ejection — release the entry lock first, then re-acquire and re-check before committing. Leaving the outcome window intact at ejection lets stale failures re-eject the backend the instant it returns. Restoring straight to `healthy` puts an unverified backend back into rotation before any probe has confirmed it. Ejecting below `MinSamples` punishes a cold backend for a single early error. And sending on `Notifications` while holding a lock, or with a blocking send, stalls the hot path on a slow subscriber.

## Resources

- [Envoy: outlier detection](https://www.envoyproxy.io/docs/envoy/latest/intro/arch_overview/upstream/outlier) — the reference design for passive ejection, the minimum-hosts guard, and the ejection-time multiplier.
- [`time.AfterFunc`](https://pkg.go.dev/time#AfterFunc) — schedules the restoration callback after the cooldown without blocking any goroutine.
- [`sync.RWMutex`](https://pkg.go.dev/sync#RWMutex) — the map-level read-write lock that keeps concurrent outcomes on different backends from serializing.
- [`testing/synctest`](https://pkg.go.dev/testing/synctest) — the fake-clock bubble that makes the cooldown-and-restore timer deterministic.

---

Back to [02-exponential-ejection-cooldown.md](02-exponential-ejection-cooldown.md) | Next: [../06-traffic-management/00-concepts.md](../06-traffic-management/00-concepts.md)
