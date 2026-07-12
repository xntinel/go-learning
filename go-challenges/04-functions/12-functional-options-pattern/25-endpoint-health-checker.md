# Exercise 25: Load Balancer With Health Check Policy and Failure Threshold

**Nivel: Intermedio** — validacion rapida (un test corto).

A load balancer that never checks its backends is just a router waiting to
send traffic into a hole. This module builds one with functional options for
the health-check interval, the per-check timeout, and the consecutive-failure
threshold that marks a backend down — checking that the timeout can never
outlast the interval it is supposed to fit inside.

## What you'll build

```text
lb/                              independent module: example.com/endpoint-health-checker
  go.mod                         go 1.24
  lb.go                          LoadBalancer, Option, New, WithHealthCheckInterval,
                                  WithHealthCheckTimeout, WithFailureThreshold, WithClock,
                                  RecordCheck, IsHealthy, CheckDue, Next
  cmd/
    demo/
      main.go                    two backends fail, one is routed around, then recovers
  lb_test.go                     option-validation table, threshold, round-robin, clock tests
```

- Files: `lb.go`, `cmd/demo/main.go`, `lb_test.go`.
- Implement: `New(addresses []string, opts ...Option) (*LoadBalancer, error)` whose `RecordCheck` tracks consecutive failures per backend and whose `Next` round-robins over healthy backends only, validating that the health-check timeout never exceeds the health-check interval.
- Test: every option-validation case including the timeout/interval boundary, a backend going unhealthy exactly at the failure threshold and recovering on one success, `Next` skipping an unhealthy backend and erroring when none are healthy, and `CheckDue` against an injected clock.
- Verify: `go test -count=1 ./...`

Set up the module:

```bash
go mod edit -go=1.24
```

### Why the timeout must fit inside the interval

`WithHealthCheckInterval` and `WithHealthCheckTimeout` are independent
options — a caller can set either, both, or neither, in any order. If the
timeout were allowed to exceed the interval, a slow check could still be
running when the next one is scheduled to start, and the "next check" would
never actually happen on schedule; health data would silently go stale.
Neither option's closure can see the other's value while it runs, so `New`
checks `timeout > interval` once, after every option has applied — the same
constructor-boundary pattern used for every cross-field invariant in this
chapter.

### RecordCheck accumulates, Next consults

`RecordCheck` is the only thing that changes a backend's health: a failure
increments a per-backend consecutive-failure counter and flips the backend
unhealthy once that counter reaches the configured threshold; a success
resets the counter and marks the backend healthy immediately, without
waiting for a matching number of consecutive successes. `Next` never
inspects failure counts itself — it only asks whether a backend is
currently healthy, walking the address list in round-robin order and
skipping any that are not. Keeping the two separate means a caller's health
checker (a goroutine polling each backend on a ticker, in production) can
call `RecordCheck` on its own schedule while request-routing code calls
`Next` independently.

### The injected clock

`CheckDue` compares `now() - lastChecked` against the configured interval.
Using a real wall clock would make `TestCheckDueUsesInjectedClock` either
flaky or slow; `WithClock` lets both the demo and the tests advance time by
assignment instead of by sleeping, so the interval boundary is exercised
exactly, deterministically, and instantly.

Create `lb.go`:

```go
package lb

import (
	"fmt"
	"time"
)

// backendState tracks one backend's health-check history.
type backendState struct {
	healthy             bool
	consecutiveFailures int
	lastChecked         time.Time
}

// LoadBalancer routes requests across a fixed set of backend addresses,
// tracking each backend's health from reported check results.
type LoadBalancer struct {
	addresses        []string
	interval         time.Duration
	timeout          time.Duration
	failureThreshold int
	now              func() time.Time

	states  map[string]*backendState
	rrIndex int
}

// Option configures a LoadBalancer and may reject invalid input.
type Option func(*LoadBalancer) error

// New builds a LoadBalancer for addresses, seeding defaults and then applying
// opts. It is the single validation boundary: the health-check timeout must
// never exceed the health-check interval, or a check could still be in
// flight when the next one is due to start.
func New(addresses []string, opts ...Option) (*LoadBalancer, error) {
	if len(addresses) == 0 {
		return nil, fmt.Errorf("at least one backend address is required")
	}
	seen := make(map[string]bool, len(addresses))
	for _, addr := range addresses {
		if addr == "" {
			return nil, fmt.Errorf("backend address must not be empty")
		}
		if seen[addr] {
			return nil, fmt.Errorf("duplicate backend address: %q", addr)
		}
		seen[addr] = true
	}

	lb := &LoadBalancer{
		addresses:        addresses,
		interval:         10 * time.Second,
		timeout:          2 * time.Second,
		failureThreshold: 3,
		now:              time.Now,
	}
	for _, opt := range opts {
		if err := opt(lb); err != nil {
			return nil, err
		}
	}

	if lb.timeout > lb.interval {
		return nil, fmt.Errorf("health-check timeout %s exceeds health-check interval %s", lb.timeout, lb.interval)
	}

	lb.states = make(map[string]*backendState, len(addresses))
	now := lb.now()
	for _, addr := range addresses {
		lb.states[addr] = &backendState{healthy: true, lastChecked: now}
	}
	return lb, nil
}

// WithHealthCheckInterval sets how often each backend should be checked
// (> 0).
func WithHealthCheckInterval(d time.Duration) Option {
	return func(lb *LoadBalancer) error {
		if d <= 0 {
			return fmt.Errorf("health-check interval must be positive, got %s", d)
		}
		lb.interval = d
		return nil
	}
}

// WithHealthCheckTimeout sets the maximum duration a single health check may
// take (> 0).
func WithHealthCheckTimeout(d time.Duration) Option {
	return func(lb *LoadBalancer) error {
		if d <= 0 {
			return fmt.Errorf("health-check timeout must be positive, got %s", d)
		}
		lb.timeout = d
		return nil
	}
}

// WithFailureThreshold sets how many consecutive failed checks mark a
// backend unhealthy (>= 1).
func WithFailureThreshold(n int) Option {
	return func(lb *LoadBalancer) error {
		if n < 1 {
			return fmt.Errorf("failure threshold must be >= 1, got %d", n)
		}
		lb.failureThreshold = n
		return nil
	}
}

// WithClock injects the clock used to time health checks.
func WithClock(now func() time.Time) Option {
	return func(lb *LoadBalancer) error {
		if now == nil {
			return fmt.Errorf("clock is nil")
		}
		lb.now = now
		return nil
	}
}

// RecordCheck reports the outcome of a health check against addr. A failure
// increments the backend's consecutive-failure count and marks it unhealthy
// once that count reaches the failure threshold. A success resets the count
// and marks the backend healthy immediately.
func (lb *LoadBalancer) RecordCheck(addr string, success bool) error {
	s, ok := lb.states[addr]
	if !ok {
		return fmt.Errorf("unknown backend address: %q", addr)
	}
	s.lastChecked = lb.now()
	if success {
		s.consecutiveFailures = 0
		s.healthy = true
		return nil
	}
	s.consecutiveFailures++
	if s.consecutiveFailures >= lb.failureThreshold {
		s.healthy = false
	}
	return nil
}

// IsHealthy reports whether addr is currently considered healthy.
func (lb *LoadBalancer) IsHealthy(addr string) bool {
	s, ok := lb.states[addr]
	return ok && s.healthy
}

// CheckDue reports whether addr has gone at least the configured interval
// since its last recorded check.
func (lb *LoadBalancer) CheckDue(addr string) bool {
	s, ok := lb.states[addr]
	if !ok {
		return false
	}
	return lb.now().Sub(s.lastChecked) >= lb.interval
}

// Next returns the next healthy backend address in round-robin order,
// skipping unhealthy ones. It returns an error if every backend is
// currently unhealthy.
func (lb *LoadBalancer) Next() (string, error) {
	n := len(lb.addresses)
	for i := 0; i < n; i++ {
		idx := (lb.rrIndex + i) % n
		addr := lb.addresses[idx]
		if lb.states[addr].healthy {
			lb.rrIndex = (idx + 1) % n
			return addr, nil
		}
	}
	return "", fmt.Errorf("no healthy backends available")
}
```

### The runnable demo

The demo fails one backend twice in a row (crossing a threshold of two), shows
`Next` routing only across the two survivors, then recovers the failed
backend with a single success and checks whether a third backend's check is
due after the injected clock advances.

Create `cmd/demo/main.go`:

```go
package main

import (
	"fmt"
	"time"

	"example.com/endpoint-health-checker"
)

func main() {
	current := time.Unix(0, 0).UTC()
	clock := func() time.Time { return current }

	balancer, err := lb.New(
		[]string{"10.0.0.1:8080", "10.0.0.2:8080", "10.0.0.3:8080"},
		lb.WithHealthCheckInterval(30*time.Second),
		lb.WithHealthCheckTimeout(5*time.Second),
		lb.WithFailureThreshold(2),
		lb.WithClock(clock),
	)
	if err != nil {
		panic(err)
	}

	// 10.0.0.2 fails twice in a row and is marked unhealthy.
	_ = balancer.RecordCheck("10.0.0.2:8080", false)
	_ = balancer.RecordCheck("10.0.0.2:8080", false)
	fmt.Printf("10.0.0.2:8080 healthy: %t\n", balancer.IsHealthy("10.0.0.2:8080"))

	for i := 0; i < 3; i++ {
		addr, err := balancer.Next()
		if err != nil {
			panic(err)
		}
		fmt.Printf("routed request %d to %s\n", i+1, addr)
	}

	// 10.0.0.2 recovers with a single successful check.
	_ = balancer.RecordCheck("10.0.0.2:8080", true)
	fmt.Printf("10.0.0.2:8080 healthy after recovery: %t\n", balancer.IsHealthy("10.0.0.2:8080"))

	current = current.Add(45 * time.Second)
	fmt.Printf("10.0.0.1:8080 check due: %t\n", balancer.CheckDue("10.0.0.1:8080"))
}
```

Run it:

```bash
go run ./cmd/demo
```

Expected output:

```
10.0.0.2:8080 healthy: false
routed request 1 to 10.0.0.1:8080
routed request 2 to 10.0.0.3:8080
routed request 3 to 10.0.0.1:8080
10.0.0.2:8080 healthy after recovery: true
10.0.0.1:8080 check due: true
```

### Tests

`TestNewValidation` tables the construction failures, including the exact
boundary where timeout equals interval (allowed) versus exceeds it
(rejected). `TestRecordCheckMarksUnhealthyAtThreshold` proves a backend stays
healthy one failure below the threshold, goes unhealthy exactly at it, and
recovers on a single success. `TestRecordCheckRejectsUnknownAddress` guards
against typos in caller code. `TestNextSkipsUnhealthyBackends` and
`TestNextErrorsWhenAllUnhealthy` cover round-robin routing around a down
backend and the all-down case. `TestCheckDueUsesInjectedClock` proves the
interval boundary against an injected clock rather than a real sleep.

Create `lb_test.go`:

```go
package lb

import (
	"testing"
	"time"
)

func TestNewValidation(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name      string
		addresses []string
		opts      []Option
		wantErr   bool
	}{
		{name: "defaults only", addresses: []string{"a:1"}},
		{name: "no addresses", addresses: nil, wantErr: true},
		{name: "empty address", addresses: []string{""}, wantErr: true},
		{name: "duplicate address", addresses: []string{"a:1", "a:1"}, wantErr: true},
		{
			name:      "timeout exceeds interval",
			addresses: []string{"a:1"},
			opts:      []Option{WithHealthCheckInterval(time.Second), WithHealthCheckTimeout(2 * time.Second)},
			wantErr:   true,
		},
		{
			name:      "timeout equal to interval is allowed",
			addresses: []string{"a:1"},
			opts:      []Option{WithHealthCheckInterval(time.Second), WithHealthCheckTimeout(time.Second)},
		},
		{name: "invalid interval", addresses: []string{"a:1"}, opts: []Option{WithHealthCheckInterval(0)}, wantErr: true},
		{name: "invalid timeout", addresses: []string{"a:1"}, opts: []Option{WithHealthCheckTimeout(0)}, wantErr: true},
		{name: "invalid threshold", addresses: []string{"a:1"}, opts: []Option{WithFailureThreshold(0)}, wantErr: true},
		{name: "nil clock", addresses: []string{"a:1"}, opts: []Option{WithClock(nil)}, wantErr: true},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			_, err := New(tt.addresses, tt.opts...)
			if tt.wantErr && err == nil {
				t.Fatal("expected error, got nil")
			}
			if !tt.wantErr && err != nil {
				t.Fatalf("unexpected error: %v", err)
			}
		})
	}
}

func TestRecordCheckMarksUnhealthyAtThreshold(t *testing.T) {
	t.Parallel()

	balancer, err := New([]string{"a:1"}, WithFailureThreshold(3))
	if err != nil {
		t.Fatal(err)
	}

	for i := 0; i < 2; i++ {
		if err := balancer.RecordCheck("a:1", false); err != nil {
			t.Fatal(err)
		}
	}
	if !balancer.IsHealthy("a:1") {
		t.Fatal("backend should still be healthy below the failure threshold")
	}

	if err := balancer.RecordCheck("a:1", false); err != nil {
		t.Fatal(err)
	}
	if balancer.IsHealthy("a:1") {
		t.Fatal("backend should be unhealthy at the failure threshold")
	}

	if err := balancer.RecordCheck("a:1", true); err != nil {
		t.Fatal(err)
	}
	if !balancer.IsHealthy("a:1") {
		t.Fatal("a single success should immediately recover the backend")
	}
}

func TestRecordCheckRejectsUnknownAddress(t *testing.T) {
	t.Parallel()

	balancer, err := New([]string{"a:1"})
	if err != nil {
		t.Fatal(err)
	}
	if err := balancer.RecordCheck("b:1", true); err == nil {
		t.Fatal("expected error for unknown backend address")
	}
}

func TestNextSkipsUnhealthyBackends(t *testing.T) {
	t.Parallel()

	balancer, err := New([]string{"a:1", "b:1", "c:1"}, WithFailureThreshold(1))
	if err != nil {
		t.Fatal(err)
	}
	if err := balancer.RecordCheck("b:1", false); err != nil {
		t.Fatal(err)
	}

	var got []string
	for i := 0; i < 4; i++ {
		addr, err := balancer.Next()
		if err != nil {
			t.Fatal(err)
		}
		got = append(got, addr)
	}
	want := []string{"a:1", "c:1", "a:1", "c:1"}
	for i := range want {
		if got[i] != want[i] {
			t.Fatalf("Next() sequence = %v, want %v", got, want)
		}
	}
}

func TestNextErrorsWhenAllUnhealthy(t *testing.T) {
	t.Parallel()

	balancer, err := New([]string{"a:1"}, WithFailureThreshold(1))
	if err != nil {
		t.Fatal(err)
	}
	if err := balancer.RecordCheck("a:1", false); err != nil {
		t.Fatal(err)
	}
	if _, err := balancer.Next(); err == nil {
		t.Fatal("expected error when every backend is unhealthy")
	}
}

func TestCheckDueUsesInjectedClock(t *testing.T) {
	t.Parallel()

	current := time.Date(2000, 1, 1, 0, 0, 0, 0, time.UTC)
	balancer, err := New(
		[]string{"a:1"},
		WithHealthCheckInterval(30*time.Second),
		WithClock(func() time.Time { return current }),
	)
	if err != nil {
		t.Fatal(err)
	}

	if balancer.CheckDue("a:1") {
		t.Fatal("check should not be due immediately after construction")
	}
	current = current.Add(31 * time.Second)
	if !balancer.CheckDue("a:1") {
		t.Fatal("check should be due once the interval has elapsed")
	}
}
```

## Review

The load balancer is correct when the interval it schedules checks on can
always actually contain a check running against the configured timeout, and
when routing (`Next`) never needs to know anything about *why* a backend is
down — only whether `RecordCheck` currently considers it healthy. That
separation is what makes the failure-threshold logic testable in isolation:
`TestRecordCheckMarksUnhealthyAtThreshold` never has to touch round-robin
state, and `TestNextSkipsUnhealthyBackends` never has to touch failure
counters. The timeout/interval check follows the same constructor-boundary
shape as every other cross-field invariant in this chapter: seed defaults,
apply every option, then validate the combination once nothing can change
it further.

## Resources

- [Dave Cheney: Functional options for friendly APIs](https://dave.cheney.net/2014/10/17/functional-options-for-friendly-apis)
- [Kubernetes: liveness, readiness, and startup probes](https://kubernetes.io/docs/tasks/configure-pod-container/configure-liveness-readiness-startup-probes/)
- [AWS ELB health checks](https://docs.aws.amazon.com/elasticloadbalancing/latest/classic/elb-healthchecks.html)

---

Back to [00-concepts.md](00-concepts.md) | Prev: [24-metrics-event-aggregator.md](24-metrics-event-aggregator.md) | Next: [26-object-storage-codec-factory.md](26-object-storage-codec-factory.md)
