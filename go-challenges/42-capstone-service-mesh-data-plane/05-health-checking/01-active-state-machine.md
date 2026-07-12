# Exercise 1: The Active Health-Checking State Machine

Active health checking is the data plane's smoke detector: it probes every backend on a fixed interval and moves each one through a flap-resistant state machine so a single dropped packet never pulls a backend out of rotation, but a real outage does. This exercise builds that checker end to end — the probe loop, the threshold counters, the healthy/unhealthy transitions, the change notifications, and two concrete probes (TCP and HTTP).

This module is fully self-contained. It starts with its own `go mod init`, defines every type it needs, and ships its own demo and tests. Nothing here imports any other exercise.

## What you'll build

```text
health.go            State, Config, Checker, New, Start, recordActive, IsHealthy, StateOf, tcpProbe, HTTPProbe
cmd/
  demo/
    main.go          probe two httptest servers, then drive a checker to unhealthy
health_test.go       threshold transitions, flap resistance, recovery, HTTP probe, Start loop (synctest)
```

- Files: `health.go`, `cmd/demo/main.go`, `health_test.go`.
- Implement: `New`, `(*Checker).Start`, `(*Checker).recordActive`, `(*Checker).IsHealthy`, `(*Checker).StateOf`, the `HTTPProbe` constructor, and the `State`/`Config`/`StateChange` types.
- Test: `health_test.go` drives the threshold transitions directly, asserts one failure does not unseat a healthy backend, exercises recovery, validates both HTTP-probe outcomes, and runs the full `Start` loop under `testing/synctest` with a fake clock.
- Verify: `go test -race ./...`

Set up the module:

```bash
go mod edit -go=1.26
```

### Why a state machine and not a boolean

The naive design stores one bit per backend: up or down. It flaps. Real networks drop packets, time out a probe under momentary load, and recover a millisecond later; a boolean flips on every blip, and every flip is a routing change — connections drained, traffic reshuffled, caches cold. The cost of a false transition is high enough that the checker must be deliberately reluctant to make one.

The reluctance is encoded as two counters and two thresholds. `consecutiveFailures` must reach `UnhealthyThreshold` before a healthy backend is demoted, and a single success resets it to zero; symmetrically `consecutiveSuccesses` must reach `HealthyThreshold` before an unhealthy backend is promoted, and a single failure resets that. A backend that alternates pass/fail/pass/fail never accumulates enough of either to transition — exactly the behavior you want, because such a backend is neither cleanly up nor cleanly down and yanking it in and out helps no one. Only a *run* of failures, or a *run* of successes, moves the machine.

Each transition, and only a real transition, emits a `StateChange` on the `Notifications` channel. The send is non-blocking (a `select` with a `default`): the checker must never stall on a slow or absent subscriber, and the channel is buffered so a burst of changes does not drop under normal load. The load balancer subscribes to this channel and also calls `IsHealthy` directly; the two together are the entire integration surface.

Create `health.go`:

```go
package healthcheck

import (
	"context"
	"fmt"
	"net"
	"net/http"
	"sync"
	"time"
)

// State is the health state of a single backend address.
type State int

const (
	StateHealthy   State = iota // in the pool; active probes passing
	StateUnhealthy              // removed from pool; active probes failing
)

// String returns a human-readable label for the state.
func (s State) String() string {
	switch s {
	case StateHealthy:
		return "healthy"
	case StateUnhealthy:
		return "unhealthy"
	default:
		return fmt.Sprintf("State(%d)", int(s))
	}
}

// StateChange is delivered on Checker.Notifications whenever a backend
// transitions between states.
type StateChange struct {
	Addr  string
	State State
}

// Config holds the tunable parameters for active checking.
type Config struct {
	Interval           time.Duration // how often to probe each backend
	Timeout            time.Duration // per-probe deadline
	HealthyThreshold   int           // consecutive successes to become healthy
	UnhealthyThreshold int           // consecutive failures to become unhealthy
}

// entry holds the mutable state for one backend. Its mu is acquired only after
// Checker.mu, never the reverse.
type entry struct {
	mu                   sync.Mutex
	state                State
	consecutiveFailures  int
	consecutiveSuccesses int
}

// Checker runs active health checking for a fixed set of backend addresses.
type Checker struct {
	cfg     Config
	mu      sync.RWMutex      // guards entries; acquired before any entry.mu
	entries map[string]*entry // key = "host:port"

	// Notifications receives a StateChange on every transition. It is buffered
	// and closed when Start returns.
	Notifications chan StateChange

	// probe runs one active check; inject a fake in tests, or pass nil to use
	// the default TCP-connect probe.
	probe func(ctx context.Context, addr string) bool
}

// New creates a Checker for addrs. Pass nil for probe to use the TCP probe.
func New(cfg Config, addrs []string, probe func(ctx context.Context, addr string) bool) *Checker {
	if probe == nil {
		probe = tcpProbe
	}
	c := &Checker{
		cfg:           cfg,
		entries:       make(map[string]*entry, len(addrs)),
		Notifications: make(chan StateChange, 64),
		probe:         probe,
	}
	for _, addr := range addrs {
		c.entries[addr] = &entry{state: StateHealthy}
	}
	return c
}

// Start launches one goroutine per backend and blocks until ctx is cancelled.
// When Start returns, Notifications is closed.
func (c *Checker) Start(ctx context.Context) {
	c.mu.RLock()
	addrs := make([]string, 0, len(c.entries))
	for addr := range c.entries {
		addrs = append(addrs, addr)
	}
	c.mu.RUnlock()

	var wg sync.WaitGroup
	for _, addr := range addrs {
		wg.Add(1)
		go func(a string) {
			defer wg.Done()
			c.runLoop(ctx, a)
		}(addr)
	}
	wg.Wait()
	close(c.Notifications)
}

func (c *Checker) runLoop(ctx context.Context, addr string) {
	ticker := time.NewTicker(c.cfg.Interval)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			probeCtx, cancel := context.WithTimeout(ctx, c.cfg.Timeout)
			ok := c.probe(probeCtx, addr)
			cancel()
			c.recordActive(addr, ok)
		}
	}
}

// recordActive applies one active-probe result to the state machine.
func (c *Checker) recordActive(addr string, success bool) {
	c.mu.RLock()
	e, ok := c.entries[addr]
	c.mu.RUnlock()
	if !ok {
		return
	}

	e.mu.Lock()
	prev := e.state
	if success {
		e.consecutiveFailures = 0
		e.consecutiveSuccesses++
		if e.state != StateHealthy && e.consecutiveSuccesses >= c.cfg.HealthyThreshold {
			e.state = StateHealthy
		}
	} else {
		e.consecutiveSuccesses = 0
		e.consecutiveFailures++
		if e.state == StateHealthy && e.consecutiveFailures >= c.cfg.UnhealthyThreshold {
			e.state = StateUnhealthy
		}
	}
	next := e.state
	e.mu.Unlock()

	if next != prev {
		select {
		case c.Notifications <- StateChange{Addr: addr, State: next}:
		default:
		}
	}
}

// IsHealthy reports whether addr is currently in the healthy state.
func (c *Checker) IsHealthy(addr string) bool {
	s, ok := c.StateOf(addr)
	return ok && s == StateHealthy
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

// tcpProbe is the default active probe: dial the address and close immediately.
func tcpProbe(ctx context.Context, addr string) bool {
	d := net.Dialer{}
	conn, err := d.DialContext(ctx, "tcp", addr)
	if err != nil {
		return false
	}
	_ = conn.Close()
	return true
}

// HTTPProbe returns a probe that GETs path on the backend and accepts the given
// status codes (defaulting to 200 OK).
func HTTPProbe(path string, acceptable ...int) func(ctx context.Context, addr string) bool {
	if len(acceptable) == 0 {
		acceptable = []int{http.StatusOK}
	}
	set := make(map[int]struct{}, len(acceptable))
	for _, code := range acceptable {
		set[code] = struct{}{}
	}
	client := &http.Client{}
	return func(ctx context.Context, addr string) bool {
		req, err := http.NewRequestWithContext(ctx, http.MethodGet, "http://"+addr+path, nil)
		if err != nil {
			return false
		}
		resp, err := client.Do(req)
		if err != nil {
			return false
		}
		_ = resp.Body.Close()
		_, ok := set[resp.StatusCode]
		return ok
	}
}
```

The whole transition logic lives in `recordActive`, and reading it is reading the state machine. It snapshots `prev`, applies the success or failure branch under the entry lock, snapshots `next`, releases, and only then — comparing `next != prev` — decides whether to notify. Notifying after the unlock is deliberate: the channel send must not happen while holding a lock, both to keep the critical section short and to avoid any chance of a subscriber's handler re-entering the checker under the lock. Probe injection through the `probe` field is what makes the machine testable without a network: a test hands `recordActive` results directly, and the `Start`/`runLoop` plumbing — the ticker, the per-probe timeout context — is exercised separately with a fake clock.

### The runnable demo

The demo shows both halves: the `HTTPProbe` returning the right boolean against two `httptest` servers (one healthy, one returning 503), and the state machine being driven to `unhealthy` by an always-failing probe. The HTTP outcomes are deterministic; so is the final state, because the 200 ms run performs far more than `UnhealthyThreshold` probes at a 10 ms interval, so the backend is reliably unhealthy by the time the context expires.

Create `cmd/demo/main.go`:

```go
package main

import (
	"context"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"time"

	"example.com/active-state-machine"
)

func main() {
	healthy := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
	}))
	defer healthy.Close()
	sick := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusServiceUnavailable)
	}))
	defer sick.Close()

	probe := healthcheck.HTTPProbe("/healthz", http.StatusOK)
	ctx := context.Background()
	fmt.Println("http probe healthy backend:", probe(ctx, strings.TrimPrefix(healthy.URL, "http://")))
	fmt.Println("http probe sick backend:   ", probe(ctx, strings.TrimPrefix(sick.URL, "http://")))

	cfg := healthcheck.Config{Interval: 10 * time.Millisecond, Timeout: 5 * time.Millisecond, HealthyThreshold: 2, UnhealthyThreshold: 2}
	c := healthcheck.New(cfg, []string{"a:80"}, func(context.Context, string) bool { return false })
	fmt.Println("backend healthy at start:        ", c.IsHealthy("a:80"))

	runCtx, cancel := context.WithTimeout(ctx, 200*time.Millisecond)
	defer cancel()
	c.Start(runCtx)
	fmt.Println("backend healthy after failed probes:", c.IsHealthy("a:80"))
}
```

Run it:

```bash
go run ./cmd/demo
```

Expected output:

```
http probe healthy backend: true
http probe sick backend:    false
backend healthy at start:         true
backend healthy after failed probes: false
```

### Tests

The transition tests call `recordActive` directly so they pin the state machine without any timing: `UnhealthyThreshold` failures reach `unhealthy`, one failure does not, and a failure run followed by a success run recovers. `TestNotificationSentOnTransition` proves the channel fires exactly on the demotion. The two HTTP-probe tests use real `httptest` servers and stay outside any synctest bubble because they do real network I/O. `TestStartDrivesUnhealthy` is the one that needs a clock: it runs the full `Start` loop inside `synctest.Test`, where `time.Sleep` advances a fake clock so the ticker fires deterministically, and `synctest.Wait` blocks until the probe goroutine has settled before the assertion.

Create `health_test.go`:

```go
package healthcheck

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"testing/synctest"
	"time"
)

func shortConfig() Config {
	return Config{Interval: 10 * time.Millisecond, Timeout: 5 * time.Millisecond, HealthyThreshold: 2, UnhealthyThreshold: 2}
}

func constProbe(ok bool) func(ctx context.Context, addr string) bool {
	return func(context.Context, string) bool { return ok }
}

func TestStateString(t *testing.T) {
	t.Parallel()
	cases := []struct {
		s    State
		want string
	}{{StateHealthy, "healthy"}, {StateUnhealthy, "unhealthy"}, {State(99), "State(99)"}}
	for _, tc := range cases {
		if got := tc.s.String(); got != tc.want {
			t.Errorf("State.String() = %q, want %q", got, tc.want)
		}
	}
}

func TestActiveProbeTransitionsToUnhealthy(t *testing.T) {
	t.Parallel()
	cfg := shortConfig()
	c := New(cfg, []string{"a:80"}, constProbe(false))
	for i := 0; i < cfg.UnhealthyThreshold; i++ {
		c.recordActive("a:80", false)
	}
	if s, _ := c.StateOf("a:80"); s != StateUnhealthy {
		t.Fatalf("state = %s, want unhealthy", s)
	}
}

func TestSingleFailureDoesNotMakeUnhealthy(t *testing.T) {
	t.Parallel()
	c := New(shortConfig(), []string{"a:80"}, constProbe(false))
	c.recordActive("a:80", false)
	if s, _ := c.StateOf("a:80"); s != StateHealthy {
		t.Fatalf("state = %s after one failure, want healthy", s)
	}
}

func TestActiveProbeRecovery(t *testing.T) {
	t.Parallel()
	cfg := shortConfig()
	c := New(cfg, []string{"a:80"}, constProbe(false))
	for i := 0; i < cfg.UnhealthyThreshold; i++ {
		c.recordActive("a:80", false)
	}
	for i := 0; i < cfg.HealthyThreshold; i++ {
		c.recordActive("a:80", true)
	}
	if s, _ := c.StateOf("a:80"); s != StateHealthy {
		t.Fatalf("state = %s, want healthy after recovery", s)
	}
}

func TestIsHealthyReturnsFalseForUnknown(t *testing.T) {
	t.Parallel()
	c := New(shortConfig(), nil, nil)
	if c.IsHealthy("unknown:9999") {
		t.Fatal("IsHealthy must be false for unknown address")
	}
}

func TestNotificationSentOnTransition(t *testing.T) {
	t.Parallel()
	cfg := shortConfig()
	c := New(cfg, []string{"x:80"}, constProbe(false))
	for i := 0; i < cfg.UnhealthyThreshold; i++ {
		c.recordActive("x:80", false)
	}
	select {
	case chg := <-c.Notifications:
		if chg.Addr != "x:80" || chg.State != StateUnhealthy {
			t.Fatalf("unexpected notification %+v", chg)
		}
	case <-time.After(time.Second):
		t.Fatal("no notification received")
	}
}

func TestHTTPProbeSucceeds(t *testing.T) {
	t.Parallel()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/healthz" {
			w.WriteHeader(http.StatusOK)
			return
		}
		w.WriteHeader(http.StatusNotFound)
	}))
	t.Cleanup(srv.Close)
	probe := HTTPProbe("/healthz", http.StatusOK)
	ctx, cancel := context.WithTimeout(context.Background(), time.Second)
	defer cancel()
	if !probe(ctx, strings.TrimPrefix(srv.URL, "http://")) {
		t.Fatal("probe should succeed for /healthz -> 200")
	}
}

func TestHTTPProbeFailsOnWrongStatus(t *testing.T) {
	t.Parallel()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusServiceUnavailable)
	}))
	t.Cleanup(srv.Close)
	probe := HTTPProbe("/healthz", http.StatusOK)
	ctx, cancel := context.WithTimeout(context.Background(), time.Second)
	defer cancel()
	if probe(ctx, strings.TrimPrefix(srv.URL, "http://")) {
		t.Fatal("probe should fail on 503")
	}
}

func TestStartDrivesUnhealthy(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		c := New(shortConfig(), []string{"a:80"}, constProbe(false))
		ctx, cancel := context.WithCancel(context.Background())
		done := make(chan struct{})
		go func() { c.Start(ctx); close(done) }()
		time.Sleep(100 * time.Millisecond)
		synctest.Wait()
		if s, _ := c.StateOf("a:80"); s != StateUnhealthy {
			t.Fatalf("state = %s, want unhealthy", s)
		}
		cancel()
		<-done
	})
}
```

## Review

The machine is correct when it transitions only on a *run*. Confirm `UnhealthyThreshold` consecutive failures demote a healthy backend and a single failure does not, that `HealthyThreshold` consecutive successes promote it back and a single failure resets the success run, and that each transition — and only a transition — emits one `StateChange`. The probe is correct when `HTTPProbe` accepts exactly the configured codes and rejects the rest. The `Start` test confirms the ticker, the per-probe timeout context, and the goroutine-per-backend plumbing actually drive `recordActive`; running it under `synctest` with a fake clock makes that deterministic and `-race`-clean.

Common mistakes for this feature. Storing a single up/down bit instead of threshold counters reintroduces flapping the moment a backend drops one packet. Sending on `Notifications` while holding the entry lock lengthens the critical section and risks a subscriber re-entering the checker under the lock — emit after the unlock. Forgetting the non-blocking `default` on the send lets one slow subscriber stall every probe goroutine. And testing the probe loop with real `time.Sleep` outside a synctest bubble makes the test both slow and flaky; either drive `recordActive` directly or run the loop under a fake clock.

## Resources

- [Envoy: health checking](https://www.envoyproxy.io/docs/envoy/latest/intro/arch_overview/upstream/health_checking) — the reference design for active probing and the healthy/unhealthy threshold model this exercise follows.
- [`testing/synctest`](https://pkg.go.dev/testing/synctest) — the fake-clock test bubble (`Test` and `Wait`) that makes the ticker-driven `Start` loop deterministic.
- [`net/http/httptest`](https://pkg.go.dev/net/http/httptest) — the in-process HTTP server used to test `HTTPProbe` without a real network.

---

Back to [00-concepts.md](00-concepts.md) | Next: [02-exponential-ejection-cooldown.md](02-exponential-ejection-cooldown.md)
