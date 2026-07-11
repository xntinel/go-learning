# Exercise 21: Circuit Breaker Allows Invalid State Transition During Recovery Due to Missing Guard

**Nivel: Avanzado** — validacion normal (tabla de casos, incluye borde o concurrencia).

A circuit breaker recovering from `Open` is supposed to let exactly *one*
trial request through in `HalfOpen` to test whether the downstream has
actually recovered, and reject every other caller until that trial
reports its outcome — otherwise "half-open" is just "open" with extra
steps, and a downstream that is still fragile gets hit by a burst of
traffic the moment the cooldown timer expires instead of by a single
probe. Omitting the guard that tracks whether a trial is already in
flight means every concurrent caller sees `HalfOpen` and gets admitted,
which is invisible in a single-threaded test and only shows up once real
concurrent load hits the recovery window. This module is fully
self-contained: its own `go mod init`, all code inline, its own demo and
tests.

## What you'll build

```text
breaker/                    independent module: example.com/circuit-breaker-half-open-guard-missing
  go.mod                     go 1.21
  breaker.go                  State, Breaker, New, Allow, Report
  cmd/
    demo/
      main.go                 runnable demo: trip, cooldown, a 10-goroutine race for the trial slot
  breaker_test.go              table over every transition, plus a concurrency test for the half-open guard
```

- Files: `breaker.go`, `cmd/demo/main.go`, `breaker_test.go`.
- Implement: `Breaker` (`State`, `Allow`, `Report`) implementing closed/open/half-open with a threshold, a cooldown, and a trial-in-flight guard.
- Test: a table over every state transition (closed→open, open rejects before cooldown, open→half-open, half-open→closed, half-open→open), plus a concurrency test racing many goroutines' `Allow()` calls against a half-open breaker and asserting exactly one is admitted.
- Verify: `go test -count=1 -race ./...`.

```bash
mkdir -p ~/go-exercises/circuit-breaker-half-open-guard-missing/cmd/demo
cd ~/go-exercises/circuit-breaker-half-open-guard-missing
go mod init example.com/circuit-breaker-half-open-guard-missing
```

### Why half-open needs its own guard, not just a state

The tempting, incomplete version of `Allow` treats `HalfOpen` as "just
like `Closed`, but named differently" — anyone who asks during that state
gets a yes:

```go
case HalfOpen:
	return true // BUG: no guard against a trial already in flight
```

That reads as correct for a *single* caller: the breaker is recovering,
so let the request through and see what happens. It falls apart the
instant more than one goroutine calls `Allow()` while the breaker is
`HalfOpen` — which is exactly what happens in production the moment
cooldown expires under real concurrent load, since every in-flight
request notices the state change at roughly the same time. Every one of
them gets a `true`, so the "single probe" recovery strategy the
half-open state exists to implement becomes "let a stampede back in and
hope," which is precisely the failure mode the breaker was built to
prevent in the first place. The fix adds a `trialInFlight` boolean that
`Allow` checks and sets atomically under the same lock, and `Report`
clears when the trial resolves:

```go
case HalfOpen:
	if b.trialInFlight {
		return false
	}
	b.trialInFlight = true
	return true
```

Because `Allow` and `Report` both take `b.mu` for their entire body, the
check-then-set on `trialInFlight` is atomic with respect to every other
goroutine calling `Allow` concurrently — only one can observe
`trialInFlight == false` and flip it before releasing the lock.

Create `breaker.go`:

```go
package breaker

import (
	"sync"
	"time"
)

// State is one of the three circuit-breaker states.
type State int

const (
	Closed State = iota
	Open
	HalfOpen
)

func (s State) String() string {
	switch s {
	case Closed:
		return "closed"
	case Open:
		return "open"
	case HalfOpen:
		return "half-open"
	default:
		return "unknown"
	}
}

// Breaker is a circuit breaker: it trips to Open after threshold
// consecutive failures, waits cooldown, then admits exactly one trial
// request in HalfOpen before deciding whether to close or reopen.
type Breaker struct {
	mu            sync.Mutex
	state         State
	failures      int
	threshold     int
	openedAt      time.Time
	cooldown      time.Duration
	trialInFlight bool
}

// New creates a Breaker that opens after threshold consecutive failures and
// waits cooldown before admitting a half-open trial request.
func New(threshold int, cooldown time.Duration) *Breaker {
	return &Breaker{threshold: threshold, cooldown: cooldown}
}

// State reports the breaker's current state.
func (b *Breaker) State() State {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.state
}

// Allow reports whether a request may proceed. Closed always allows. Open
// allows nothing until cooldown has elapsed, at which point it transitions
// to HalfOpen and admits exactly the caller that triggered the transition
// as the trial request. While HalfOpen, only that one trial is in flight
// at a time -- every other concurrent caller is rejected until the trial
// is reported, which is the guard that keeps recovery from letting a burst
// of traffic hit a downstream that might still be broken.
func (b *Breaker) Allow() bool {
	b.mu.Lock()
	defer b.mu.Unlock()

	switch b.state {
	case Closed:
		return true
	case Open:
		if time.Since(b.openedAt) >= b.cooldown {
			b.state = HalfOpen
			b.trialInFlight = true
			return true
		}
		return false
	case HalfOpen:
		if b.trialInFlight {
			return false
		}
		b.trialInFlight = true
		return true
	default:
		return false
	}
}

// Report records the outcome of a request that Allow returned true for.
func (b *Breaker) Report(success bool) {
	b.mu.Lock()
	defer b.mu.Unlock()

	switch b.state {
	case HalfOpen:
		b.trialInFlight = false
		if success {
			b.state = Closed
			b.failures = 0
		} else {
			b.state = Open
			b.openedAt = time.Now()
		}
	case Closed:
		if success {
			b.failures = 0
			return
		}
		b.failures++
		if b.failures >= b.threshold {
			b.state = Open
			b.openedAt = time.Now()
		}
	}
}
```

### The runnable demo

The demo trips the breaker, waits out the cooldown, then races ten
goroutines against `Allow()` to show only one is admitted as the trial.

Create `cmd/demo/main.go`:

```go
package main

import (
	"fmt"
	"sync"
	"sync/atomic"
	"time"

	"example.com/circuit-breaker-half-open-guard-missing"
)

func main() {
	b := breaker.New(2, 20*time.Millisecond)

	// Two consecutive failures trip the breaker open.
	b.Report(false)
	b.Report(false)
	fmt.Println("state after 2 failures:", b.State())

	fmt.Println("allow during cooldown:", b.Allow())

	time.Sleep(25 * time.Millisecond)

	// Ten goroutines race to become the half-open trial; exactly one wins.
	var admitted int64
	var wg sync.WaitGroup
	for i := 0; i < 10; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			if b.Allow() {
				atomic.AddInt64(&admitted, 1)
			}
		}()
	}
	wg.Wait()

	fmt.Println("state after cooldown:", b.State())
	fmt.Println("admitted trial requests:", admitted)

	b.Report(true)
	fmt.Println("state after successful trial:", b.State())
}
```

```bash
go run ./cmd/demo
```

Expected output:

```text
state after 2 failures: open
allow during cooldown: false
state after cooldown: half-open
admitted trial requests: 1
state after successful trial: closed
```

### Tests

`TestBreakerTransitions` is a table over every legal transition — closed
staying closed, closed tripping open at threshold, open rejecting during
cooldown, open admitting the transition to half-open, and both half-open
outcomes — run with `t.Parallel()` per case since each builds its own
`Breaker`. `TestHalfOpenAdmitsExactlyOneConcurrentTrial` is the
concurrency/edge case the module exists to pin: fifty goroutines call
`Allow()` on the same half-open breaker at once, and exactly one may be
admitted.

Create `breaker_test.go`:

```go
package breaker

import (
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

func TestBreakerTransitions(t *testing.T) {
	tests := []struct {
		name   string
		build  func() *Breaker
		action func(*Breaker)
		want   State
	}{
		{
			name:  "closed stays closed on success",
			build: func() *Breaker { return New(2, time.Millisecond) },
			action: func(b *Breaker) {
				b.Report(true)
			},
			want: Closed,
		},
		{
			name:  "closed opens after threshold consecutive failures",
			build: func() *Breaker { return New(2, time.Millisecond) },
			action: func(b *Breaker) {
				b.Report(false)
				b.Report(false)
			},
			want: Open,
		},
		{
			name:  "closed does not open below threshold",
			build: func() *Breaker { return New(2, time.Millisecond) },
			action: func(b *Breaker) {
				b.Report(false)
			},
			want: Closed,
		},
		{
			name: "open rejects before cooldown elapses",
			build: func() *Breaker {
				b := New(1, time.Hour)
				b.Report(false)
				return b
			},
			action: func(b *Breaker) {
				b.Allow()
			},
			want: Open,
		},
		{
			name: "open transitions to half-open once cooldown elapses",
			build: func() *Breaker {
				b := New(1, time.Millisecond)
				b.Report(false)
				time.Sleep(2 * time.Millisecond)
				return b
			},
			action: func(b *Breaker) {
				b.Allow()
			},
			want: HalfOpen,
		},
		{
			name: "half-open success closes the breaker",
			build: func() *Breaker {
				return &Breaker{state: HalfOpen, threshold: 1}
			},
			action: func(b *Breaker) {
				b.Report(true)
			},
			want: Closed,
		},
		{
			name: "half-open failure reopens the breaker",
			build: func() *Breaker {
				return &Breaker{state: HalfOpen, threshold: 1}
			},
			action: func(b *Breaker) {
				b.Report(false)
			},
			want: Open,
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			b := tc.build()
			tc.action(b)
			if got := b.State(); got != tc.want {
				t.Fatalf("state = %v, want %v", got, tc.want)
			}
		})
	}
}

// TestHalfOpenAdmitsExactlyOneConcurrentTrial is the concurrency/edge case:
// with many goroutines racing Allow() while the breaker is half-open,
// exactly one may proceed as the trial request until it is reported.
func TestHalfOpenAdmitsExactlyOneConcurrentTrial(t *testing.T) {
	b := &Breaker{state: HalfOpen, threshold: 1}

	const goroutines = 50
	var admitted int64
	var wg sync.WaitGroup
	wg.Add(goroutines)
	for i := 0; i < goroutines; i++ {
		go func() {
			defer wg.Done()
			if b.Allow() {
				atomic.AddInt64(&admitted, 1)
			}
		}()
	}
	wg.Wait()

	if admitted != 1 {
		t.Fatalf("admitted = %d concurrent trial requests, want exactly 1", admitted)
	}
}
```

Run: `go test -count=1 -race ./...`.

## Review

`Allow` is correct when, no matter how many goroutines call it
concurrently while the breaker is half-open, exactly one is ever admitted
as the trial — proven under `-race` with dozens of goroutines, not by a
sequential call from a single test goroutine, which would pass even on
the buggy version since there is nothing to race against. The mistake
this design avoids is modeling `HalfOpen` as a state that behaves like
`Closed` for the purposes of `Allow`; the two are different specifically
*because* half-open needs an extra guard — `trialInFlight` — that closed
does not, and that guard has to be checked and set atomically under the
same lock `Report` uses to clear it, or two goroutines can both observe it
false and both be admitted.

## Resources

- [sync.Mutex](https://pkg.go.dev/sync#Mutex) — protecting a check-then-set sequence (`trialInFlight`) so only one goroutine can win the race.
- [Martin Fowler: CircuitBreaker](https://martinfowler.com/bliki/CircuitBreaker.html) — the closed/open/half-open pattern this module implements.
- [Go Race Detector](https://go.dev/doc/articles/race_detector) — running concurrency tests under `-race` to catch data races the table alone cannot.

---

Back to [00-concepts.md](00-concepts.md) | Prev: [20-sharded-cache-shadowed-shard-index.md](20-sharded-cache-shadowed-shard-index.md) | Next: [22-worker-pool-goroutine-leak-on-shutdown-race.md](22-worker-pool-goroutine-leak-on-shutdown-race.md)
