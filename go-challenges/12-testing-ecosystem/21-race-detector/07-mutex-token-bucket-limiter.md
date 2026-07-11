# Exercise 7: A Concurrency-Safe Token-Bucket Rate Limiter Under -race

A token-bucket limiter is compound state: `Allow` must read the clock, refill the
bucket, and consume a token as one indivisible step, mutating both the token
count and the last-refill timestamp together. This is the exercise where "just
make each field atomic" fails -- two independent atomics cannot make a two-field
update atomic. The fix is one `sync.Mutex` guarding the whole refill-and-consume
critical section, and the invariant (never more than `burst` tokens) holds under
concurrent callers.

This module is self-contained: its own `go mod init`, its own racy demo, its own
tests. Nothing here imports another exercise.

## What you'll build

```text
tokenbucket/                independent module: example.com/tokenbucket
  go.mod                    go 1.26
  limiter.go                type Limiter (sync.Mutex): NewLimiter, Allow
  cmd/
    demo/
      main.go               burst of Allow calls, print granted/denied
    racy/
      main.go               unsynchronized tokens+lastRefill; run with -race
  limiter_test.go           K concurrent Allow within the allowed bound, under -race
```

Files: `limiter.go`, `cmd/demo/main.go`, `cmd/racy/main.go`, `limiter_test.go`.
Implement: a `Limiter` whose `Allow` refills-and-consumes under one mutex.
Test: `TestLimiterConcurrentAllow` fires K goroutines and asserts the granted
count stays within the mathematically allowed bound, under `-race`.
Verify: `go test -count=5 -race ./...`; then `go run -race ./cmd/racy`.

Set up the module:

```bash
mkdir -p ~/go-exercises/tokenbucket/cmd/demo ~/go-exercises/tokenbucket/cmd/racy
cd ~/go-exercises/tokenbucket
go mod init example.com/tokenbucket
```

### Why compound state needs one lock, not two atomics

`Allow` does four things that must be one atomic step: read `now`, compute the
refill from the elapsed time since `last`, clamp the token count to `burst`, and
if there is a token, decrement and grant. The token count and `last` are coupled
-- the refill amount depends on both -- so they must change together. If you made
`tokens` one `atomic.Float64`-style value and `last` another, two goroutines
could interleave between the two atomics: both read the same `last`, both compute
the same refill, both grant, and the bucket over-issues. Each field would be
individually atomic and the invariant across them still broken. That is the
canonical "atomic but still wrong."

The correct tool is a single `sync.Mutex` held across the entire
refill-and-consume. Inside the critical section the state is consistent: exactly
one goroutine refills against the true elapsed time and consumes, so the bucket
never holds more than `burst` tokens and never grants a token it does not have.
This is the compound-state branch of the fix-by-access-pattern decision tree --
the case that specifically does *not* map to `sync/atomic`.

The refill math: tokens accrue at `rate` per second, capped at `burst`. On each
call, `elapsed = now - last` seconds, `tokens = min(burst, tokens + elapsed*rate)`,
then `last = now`. Go's built-in `min` works on `float64`, so no `math` import is
needed.

Create `limiter.go`:

```go
package tokenbucket

import (
	"sync"
	"time"
)

// Limiter is a concurrency-safe token-bucket rate limiter. tokens and last are
// compound state that must change together, so a single mutex guards the whole
// refill-and-consume; independent atomics would not preserve the invariant.
type Limiter struct {
	mu     sync.Mutex
	rate   float64   // tokens added per second
	burst  float64   // maximum tokens the bucket can hold
	tokens float64   // current tokens
	last   time.Time // time of the last refill
}

// NewLimiter returns a limiter that refills at `rate` tokens/sec, holding at
// most `burst` tokens, starting full.
func NewLimiter(rate float64, burst int) *Limiter {
	return &Limiter{
		rate:   rate,
		burst:  float64(burst),
		tokens: float64(burst),
		last:   time.Now(),
	}
}

// Allow refills the bucket for the elapsed time and consumes one token if
// available, reporting whether the call is permitted. The refill and the
// consume are one critical section.
func (l *Limiter) Allow() bool {
	l.mu.Lock()
	defer l.mu.Unlock()

	now := time.Now()
	elapsed := now.Sub(l.last).Seconds()
	l.tokens = min(l.burst, l.tokens+elapsed*l.rate)
	l.last = now

	if l.tokens >= 1 {
		l.tokens--
		return true
	}
	return false
}
```

### The runnable demo

The demo uses `rate=0` so no tokens refill, making the output deterministic: a
bucket of burst 3 grants three calls then denies the rest.

Create `cmd/demo/main.go`:

```go
package main

import (
	"fmt"

	"example.com/tokenbucket"
)

func main() {
	l := tokenbucket.NewLimiter(0, 3) // burst 3, no refill

	for i := range 5 {
		fmt.Printf("call %d: allowed=%v\n", i, l.Allow())
	}
}
```

Run it:

```bash
go run -race ./cmd/demo
```

Expected output:

```text
call 0: allowed=true
call 1: allowed=true
call 2: allowed=true
call 3: allowed=false
call 4: allowed=false
```

### The racy version, for the report

Create `cmd/racy/main.go`. Run with `go run -race ./cmd/racy`:

```go
// Command racy has an unsynchronized limiter: Allow reads and writes tokens and
// last with no lock, racing on both fields. Run manually:
//
//	go run -race ./cmd/racy
//
// It is a main package with no test, so `go test -race ./...` only builds it.
package main

import (
	"fmt"
	"sync"
	"time"
)

type racyLimiter struct {
	rate   float64
	burst  float64
	tokens float64   // raced on
	last   time.Time // raced on
}

func (l *racyLimiter) allow() bool {
	now := time.Now()
	elapsed := now.Sub(l.last).Seconds()
	l.tokens = min(l.burst, l.tokens+elapsed*l.rate) // racy read+write
	l.last = now                                     // racy write
	if l.tokens >= 1 {
		l.tokens--
		return true
	}
	return false
}

func main() {
	l := &racyLimiter{rate: 100, burst: 100, tokens: 100, last: time.Now()}

	var wg sync.WaitGroup
	for range 200 {
		wg.Add(1)
		go func() {
			defer wg.Done()
			_ = l.allow()
		}()
	}
	wg.Wait()

	fmt.Println("done (run under -race to see the report on tokens/last)")
}
```

### Tests

`TestLimiterConcurrentAllow` fires K goroutines that each call `Allow` once,
against a limiter with a known rate and burst, and measures the wall-clock window
the hammer took. It asserts the granted count is within the mathematically
allowed bound: at least `burst` (there are far more callers than initial tokens,
so all initial tokens get consumed) and at most `burst + rate*window` plus a small
margin (the only extra grants come from tokens refilled during the window). With
a low rate and a microsecond-scale window, that upper bound is just above `burst`.
Passing under `-race -count=5` proves the compound-state guard holds across
interleavings.

Create `limiter_test.go`:

```go
package tokenbucket

import (
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

func TestLimiterConcurrentAllow(t *testing.T) {
	t.Parallel()

	const (
		k     = 1000
		rate  = 50.0 // tokens/sec
		burst = 100
	)
	l := NewLimiter(rate, burst)

	var granted atomic.Int64
	var wg sync.WaitGroup
	start := time.Now()
	for range k {
		wg.Add(1)
		go func() {
			defer wg.Done()
			if l.Allow() {
				granted.Add(1)
			}
		}()
	}
	wg.Wait()
	window := time.Since(start).Seconds()

	g := granted.Load()
	// Lower bound: with k >> burst callers, every initial token is consumed.
	if g < burst {
		t.Fatalf("granted %d, want at least burst=%d", g, burst)
	}
	// Upper bound: initial burst plus whatever refilled during the window, +1 margin.
	upper := int64(float64(burst) + rate*window + 1)
	if g > upper {
		t.Fatalf("granted %d, exceeds allowed bound %d (window %.4fs)", g, upper, window)
	}
}

func TestLimiterRefills(t *testing.T) {
	t.Parallel()

	l := NewLimiter(1000, 1) // 1000 tokens/sec, burst 1
	if !l.Allow() {
		t.Fatal("first Allow denied on a full bucket")
	}
	if l.Allow() {
		t.Fatal("second immediate Allow granted on an empty burst-1 bucket")
	}
	time.Sleep(5 * time.Millisecond) // ~5 tokens refill, capped at burst 1
	if !l.Allow() {
		t.Fatal("Allow denied after a refill window")
	}
}

func TestLimiterExhaustsBurst(t *testing.T) {
	t.Parallel()

	l := NewLimiter(0, 3) // no refill
	for i := range 3 {
		if !l.Allow() {
			t.Fatalf("call %d denied within burst", i)
		}
	}
	if l.Allow() {
		t.Fatal("call past burst granted with no refill")
	}
}
```

## Review

The limiter is correct when the granted count never exceeds `burst` plus what
legitimately refilled during the window, and the token count and refill time
always move together. The proof is `TestLimiterConcurrentAllow` passing under
`-race -count=5`: K goroutines contend and the detector finds no unordered access
because one mutex guards the whole compound update, while the bound assertion
confirms the bucket did not over-issue. `TestLimiterRefills` and
`TestLimiterExhaustsBurst` pin the refill and burst behavior.

The mistake to avoid is the tempting one: making `tokens` and `last` two
independent atomics. Per-field atomicity does not make the two-field update
atomic, and the bucket over-issues under contention. Compound state that changes
together needs one critical section. `cmd/racy` shows the unsynchronized version
racing on both fields. Run `go test -count=5 -race ./...`.

## Resources

- [`sync.Mutex`](https://pkg.go.dev/sync#Mutex) -- the single critical section that guards the compound refill-and-consume.
- [`time` package](https://pkg.go.dev/time) -- `time.Now`, `time.Time.Sub`, and `time.Duration.Seconds` for the refill math.
- [`golang.org/x/time/rate`](https://pkg.go.dev/golang.org/x/time/rate) -- the production token-bucket limiter, whose `Limiter` guards the same compound state.

---

Back to [00-concepts.md](00-concepts.md) | Prev: [06-worker-pool-result-fan-in.md](06-worker-pool-result-fan-in.md) | Next: [08-httptest-concurrent-handler-race.md](08-httptest-concurrent-handler-race.md)
