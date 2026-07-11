# Exercise 17: Sliding-Window Rate Limiter With Decay Strategy

**Nivel: Intermedio** — validacion rapida (un test corto).

A token-bucket rate limiter has a window and a refill rate, but *how* tokens
replenish inside that window is itself a choice: continuously, exponentially
toward capacity, or only in fixed steps. This module picks that decay
strategy through an option, and it catches the one combination that makes no
sense: a step interval that does not evenly divide the window it is
supposed to refill within.

## What you'll build

```text
ratelimiter/                     independent module: example.com/ratelimiter
  go.mod                         go 1.24
  ratelimiter.go                 DecayStrategy, Limiter, Option, New, WithWindow,
                                  WithRefillRate, WithDecayStrategy, WithStepInterval,
                                  WithClock, Allow, Tokens
  cmd/
    demo/
      main.go                    manual clock drives a SteppedDecay refill
  ratelimiter_test.go            table test over decay/window combos plus refill behavior
```

- Files: `ratelimiter.go`, `cmd/demo/main.go`, `ratelimiter_test.go`.
- Implement: `New(opts ...Option) (*Limiter, error)` whose `Allow`/`Tokens` refill according to `LinearDecay`, `ExponentialDecay`, or `SteppedDecay`, validating that a step interval is only set alongside `SteppedDecay` and evenly divides the window.
- Test: every decay-strategy/step-interval combination, plus a `SteppedDecay` and a `LinearDecay` refill trace against an injected clock.
- Verify: `go test -count=1 ./...`

Set up the module:

```bash
mkdir -p ~/go-exercises/ratelimiter/cmd/demo
cd ~/go-exercises/ratelimiter
go mod init example.com/ratelimiter
go mod edit -go=1.24
```

`WithStepInterval` and `WithDecayStrategy` are independent options — a
caller could set the interval before choosing the strategy, or never choose
`SteppedDecay` at all. Two things can go wrong that neither option sees on
its own: a step interval set under `LinearDecay` or `ExponentialDecay` (it
would be silently ignored, which is worse than an error), and `SteppedDecay`
paired with a window that isn't a whole number of steps (the last partial
step would either refill early or never trigger). `New` tracks whether a
step interval was ever set with a `stepIntervalSet` bool and checks both
directions after every option has run — the same technique used for the
sample-ratio mismatch earlier in this chapter.

Create `ratelimiter.go`:

```go
package ratelimiter

import (
	"fmt"
	"math"
	"sync"
	"time"
)

// DecayStrategy selects how tokens replenish toward capacity as time passes.
type DecayStrategy int

const (
	// LinearDecay refills tokens at a constant rate.
	LinearDecay DecayStrategy = iota
	// ExponentialDecay approaches capacity asymptotically, refilling faster
	// when far from capacity.
	ExponentialDecay
	// SteppedDecay only refills in whole StepInterval increments, carrying
	// any partial step forward instead of refilling continuously.
	SteppedDecay
)

func (d DecayStrategy) String() string {
	switch d {
	case LinearDecay:
		return "LinearDecay"
	case ExponentialDecay:
		return "ExponentialDecay"
	case SteppedDecay:
		return "SteppedDecay"
	default:
		return fmt.Sprintf("DecayStrategy(%d)", int(d))
	}
}

// Limiter is a concurrency-safe sliding-window token limiter.
type Limiter struct {
	mu              sync.Mutex
	window          time.Duration
	refillRate      float64 // tokens per second
	decay           DecayStrategy
	stepInterval    time.Duration
	stepIntervalSet bool
	capacity        float64
	now             func() time.Time
	tokens          float64
	lastRefill      time.Time
}

// Option configures a Limiter and may reject invalid input.
type Option func(*Limiter) error

// New seeds defaults, applies opts in order, then validates the cross-field
// invariant between decay strategy and window/step-interval: no single
// option can see both, because each only knows its own argument.
func New(opts ...Option) (*Limiter, error) {
	l := &Limiter{
		window:     time.Second,
		refillRate: 10,
		decay:      LinearDecay,
		now:        time.Now,
	}
	for _, opt := range opts {
		if err := opt(l); err != nil {
			return nil, err
		}
	}

	if l.stepIntervalSet && l.decay != SteppedDecay {
		return nil, fmt.Errorf("step interval was set but decay strategy is %s, not SteppedDecay", l.decay)
	}
	if l.decay == SteppedDecay {
		if !l.stepIntervalSet {
			return nil, fmt.Errorf("SteppedDecay requires WithStepInterval")
		}
		if l.window%l.stepInterval != 0 {
			return nil, fmt.Errorf("window %s must be an integer multiple of step interval %s", l.window, l.stepInterval)
		}
	}

	l.capacity = l.refillRate * l.window.Seconds()
	l.tokens = l.capacity
	l.lastRefill = l.now()
	return l, nil
}

// WithWindow sets the sliding window duration (> 0).
func WithWindow(d time.Duration) Option {
	return func(l *Limiter) error {
		if d <= 0 {
			return fmt.Errorf("window must be positive, got %s", d)
		}
		l.window = d
		return nil
	}
}

// WithRefillRate sets tokens replenished per second (> 0).
func WithRefillRate(tokensPerSecond float64) Option {
	return func(l *Limiter) error {
		if tokensPerSecond <= 0 {
			return fmt.Errorf("refill rate must be positive, got %v", tokensPerSecond)
		}
		l.refillRate = tokensPerSecond
		return nil
	}
}

// WithDecayStrategy selects how tokens replenish, from the closed set of
// named constants.
func WithDecayStrategy(d DecayStrategy) Option {
	return func(l *Limiter) error {
		switch d {
		case LinearDecay, ExponentialDecay, SteppedDecay:
			l.decay = d
			return nil
		default:
			return fmt.Errorf("unknown decay strategy: %d", int(d))
		}
	}
}

// WithStepInterval sets the refill granularity for SteppedDecay (> 0).
// It is only compatible with SteppedDecay and only valid when window is an
// integer multiple of it.
func WithStepInterval(d time.Duration) Option {
	return func(l *Limiter) error {
		if d <= 0 {
			return fmt.Errorf("step interval must be positive, got %s", d)
		}
		l.stepInterval = d
		l.stepIntervalSet = true
		return nil
	}
}

// WithClock injects the clock used to time refills.
func WithClock(now func() time.Time) Option {
	return func(l *Limiter) error {
		if now == nil {
			return fmt.Errorf("clock is nil")
		}
		l.now = now
		return nil
	}
}

// Allow refills based on elapsed time and reports whether n tokens were
// available, consuming them if so.
func (l *Limiter) Allow(n float64) bool {
	l.mu.Lock()
	defer l.mu.Unlock()

	l.refillLocked()
	if l.tokens >= n {
		l.tokens -= n
		return true
	}
	return false
}

// Tokens reports the current token count after refilling for elapsed time.
func (l *Limiter) Tokens() float64 {
	l.mu.Lock()
	defer l.mu.Unlock()
	l.refillLocked()
	return l.tokens
}

func (l *Limiter) refillLocked() {
	elapsed := l.now().Sub(l.lastRefill)
	if elapsed <= 0 {
		return
	}

	switch l.decay {
	case LinearDecay:
		l.tokens = math.Min(l.capacity, l.tokens+l.refillRate*elapsed.Seconds())
		l.lastRefill = l.now()
	case ExponentialDecay:
		frac := 1 - math.Exp(-elapsed.Seconds()/l.window.Seconds())
		l.tokens = l.tokens + (l.capacity-l.tokens)*frac
		l.lastRefill = l.now()
	case SteppedDecay:
		steps := int64(elapsed / l.stepInterval)
		if steps <= 0 {
			return
		}
		l.tokens = math.Min(l.capacity, l.tokens+float64(steps)*l.refillRate*l.stepInterval.Seconds())
		l.lastRefill = l.lastRefill.Add(time.Duration(steps) * l.stepInterval)
	}
}
```

### The runnable demo

The demo uses `SteppedDecay` with a 1-second step inside a 4-second window,
draining the bucket, then advancing a manual clock by 3.5 real-seconds'
worth of simulated time — only the three *whole* steps refill, and the
half-second leftover is carried forward rather than lost.

Create `cmd/demo/main.go`:

```go
package main

import (
	"fmt"
	"time"

	"example.com/ratelimiter"
)

func main() {
	current := time.Unix(0, 0).UTC()
	clock := func() time.Time { return current }

	l, err := ratelimiter.New(
		ratelimiter.WithWindow(4*time.Second),
		ratelimiter.WithStepInterval(time.Second),
		ratelimiter.WithDecayStrategy(ratelimiter.SteppedDecay),
		ratelimiter.WithRefillRate(2),
		ratelimiter.WithClock(clock),
	)
	if err != nil {
		panic(err)
	}

	fmt.Printf("capacity tokens available: %t\n", l.Allow(8))
	fmt.Printf("empty bucket allows more: %t\n", l.Allow(1))

	current = current.Add(3500 * time.Millisecond) // 3 whole 1s steps
	fmt.Printf("tokens after 3 steps: %.0f\n", l.Tokens())
	fmt.Printf("allow 6 after partial refill: %t\n", l.Allow(6))
}
```

Run it:

```bash
go run ./cmd/demo
```

Expected output:

```
capacity tokens available: true
empty bucket allows more: false
tokens after 3 steps: 6
allow 6 after partial refill: true
```

### Tests

`TestNewValidation` tables every decay/step-interval combination, including
the non-dividing-window case. `TestSteppedDecayRefillsOnlyOnWholeSteps`
proves the 0.5-second leftover after 3.5 seconds does not refill early.
`TestLinearDecayRefillsContinuously` proves the default strategy refills
proportionally to elapsed time rather than in discrete steps.

Create `ratelimiter_test.go`:

```go
package ratelimiter

import (
	"testing"
	"time"
)

func TestNewValidation(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name    string
		opts    []Option
		wantErr bool
	}{
		{name: "defaults only"},
		{name: "step interval with default LinearDecay", opts: []Option{
			WithStepInterval(time.Second),
		}, wantErr: true},
		{name: "SteppedDecay without step interval", opts: []Option{
			WithDecayStrategy(SteppedDecay),
		}, wantErr: true},
		{name: "SteppedDecay with non-dividing window", opts: []Option{
			WithDecayStrategy(SteppedDecay), WithWindow(5 * time.Second), WithStepInterval(2 * time.Second),
		}, wantErr: true},
		{name: "SteppedDecay with dividing window", opts: []Option{
			WithDecayStrategy(SteppedDecay), WithWindow(6 * time.Second), WithStepInterval(2 * time.Second),
		}},
		{name: "invalid window", opts: []Option{WithWindow(0)}, wantErr: true},
		{name: "invalid refill rate", opts: []Option{WithRefillRate(0)}, wantErr: true},
		{name: "unknown decay strategy", opts: []Option{WithDecayStrategy(DecayStrategy(99))}, wantErr: true},
		{name: "invalid step interval", opts: []Option{WithStepInterval(-time.Second)}, wantErr: true},
		{name: "nil clock rejected", opts: []Option{WithClock(nil)}, wantErr: true},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			_, err := New(tt.opts...)
			if tt.wantErr && err == nil {
				t.Fatal("expected error, got nil")
			}
			if !tt.wantErr && err != nil {
				t.Fatalf("unexpected error: %v", err)
			}
		})
	}
}

func TestSteppedDecayRefillsOnlyOnWholeSteps(t *testing.T) {
	t.Parallel()

	base := time.Date(2000, 1, 1, 0, 0, 0, 0, time.UTC)
	current := base

	l, err := New(
		WithWindow(4*time.Second),
		WithStepInterval(time.Second),
		WithDecayStrategy(SteppedDecay),
		WithRefillRate(2),
		WithClock(func() time.Time { return current }),
	)
	if err != nil {
		t.Fatal(err)
	}

	if !l.Allow(8) {
		t.Fatal("expected full capacity (8 tokens) to be available at start")
	}
	if l.Allow(1) {
		t.Fatal("expected bucket to be empty right after draining capacity")
	}

	current = base.Add(3500 * time.Millisecond) // 3 whole steps, 0.5s leftover
	if got := l.Tokens(); got != 6 {
		t.Fatalf("Tokens() = %v, want 6 (3 steps * 2 tokens/step)", got)
	}
}

func TestLinearDecayRefillsContinuously(t *testing.T) {
	t.Parallel()

	base := time.Date(2000, 1, 1, 0, 0, 0, 0, time.UTC)
	current := base

	l, err := New(
		WithWindow(time.Second),
		WithRefillRate(4),
		WithClock(func() time.Time { return current }),
	)
	if err != nil {
		t.Fatal(err)
	}

	if !l.Allow(4) {
		t.Fatal("expected capacity of 4 tokens at start")
	}

	current = base.Add(500 * time.Millisecond)
	if got := l.Tokens(); got != 2 {
		t.Fatalf("Tokens() = %v, want 2 after half a second at 4 tokens/sec", got)
	}
}
```

## Review

The limiter is correct when a decay strategy's own configuration knob — the
step interval — cannot outlive a switch away from that strategy, and when
`SteppedDecay`'s window always divides evenly by its step so no partial step
is ever silently dropped or double-counted. `stepIntervalSet` is the same
"was this option ever called" technique used for the sample-ratio mismatch
elsewhere in this chapter: the constructor, not either option, is the only
place both facts are visible together. The refill math itself stays
deterministic throughout because every test and the demo drive it through an
injected clock rather than real elapsed time.

## Resources

- [Cloudflare: how we designed a rate limiting system](https://blog.cloudflare.com/counting-things-a-lot-of-different-things/)
- [pkg.go.dev: math package](https://pkg.go.dev/math)
- [Stripe API: rate limiters](https://docs.stripe.com/rate-limits)

---

Back to [00-concepts.md](00-concepts.md) | Prev: [16-circuit-breaker-state-machine.md](16-circuit-breaker-state-machine.md) | Next: [18-grpc-service-interceptor-chain.md](18-grpc-service-interceptor-chain.md)
