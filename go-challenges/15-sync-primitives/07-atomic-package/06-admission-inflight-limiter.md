# Exercise 6: In-Flight Concurrency Limiter (load shedding)

When a service is at capacity, admitting one more request makes every request
slower and can tip the process into a death spiral. The fix is admission control:
count in-flight requests and shed load (HTTP 503) once you hit a ceiling. This
exercise builds a lock-free semaphore whose `TryAcquire` uses a `CompareAndSwap`
loop to increment the in-flight counter *only while below the limit*.

This module is fully self-contained.

## What you'll build

```text
admission/                 independent module: example.com/admission
  go.mod
  limiter.go               type Limiter; TryAcquire (CAS loop), Release, InFlight, Middleware
  cmd/
    demo/
      main.go              acquires up to the limit, shows a shed
  limiter_test.go          never-exceeds-limit concurrency test, httptest 503, Example
```

- Files: `limiter.go`, `cmd/demo/main.go`, `limiter_test.go`.
- Implement: a `Limiter` with a max and an `atomic.Int64` in-flight; `TryAcquire` admits via CAS only below the limit; `Release` decrements; `Middleware` returns 503 when full.
- Test: far more goroutines than the limit each acquire/hold/release; a peak tracker asserts the observed concurrency never exceeds the limit and in-flight returns to zero.
- Verify: `go test -count=1 -race ./...`

Set up the module:

```bash
mkdir -p go-solutions/15-sync-primitives/07-atomic-package/06-admission-inflight-limiter/cmd/demo
cd go-solutions/15-sync-primitives/07-atomic-package/06-admission-inflight-limiter
```

### Why a naive Add makes a bad limiter

The obvious limiter is `if l.inflight.Add(1) > l.max { l.inflight.Add(-1); return false }`.
It does bound admissions: `Add` returns unique post-increment values in the atomics
total order, so at most `max` concurrent callers ever see a return at or below the
ceiling. But it is still the wrong tool, for two production-relevant reasons. The
increment is unconditional, so every rejected request pushes the counter *past* the
limit before backing off: from `max-1`, two racing goroutines drive it to `max+1`,
and under a rejection storm the raw value can sit at several times `max`. Any
concurrent reader of that counter — the `InFlight()` gauge you export to metrics,
a readiness probe, an alert threshold — observes in-flight counts that are
physically impossible. Worse, racing rejectors inflate the counter for one
another: a caller's `Add(1)` can land on top of other rejectors' not-yet-reverted
increments and return a value above `max` while genuine in-flight work is below
capacity, so the limiter sheds load (503) when it should have admitted. The
`Add`-then-check pattern keeps admissions bounded but corrupts the counter as an
observable and rejects spuriously under contention.

The correct enforcement is a CAS loop that makes the ceiling part of the atomic
step:

```go
for {
	cur := l.inflight.Load()
	if cur >= l.max {
		return false        // full: shed load, never increment
	}
	if l.inflight.CompareAndSwap(cur, cur+1) {
		return true         // admitted exactly one slot
	}
	// contention: someone changed inflight; re-read and retry
}
```

The increment happens *inside* the CAS, conditioned on the value we checked. If we
read `cur < max` and the CAS succeeds, we are guaranteed no one pushed the count to
or past `max` in between — because if they had, `cur` would no longer match and the
CAS would fail, sending us back to re-check the limit. So the counter can never
exceed `max`. This is exactly the concepts' rule: "enforce the ceiling atomically
inside a CAS loop", not with an unconditional `Add`.

`Release` is a plain `Add(-1)` — decrementing a slot you already hold has no ceiling
to respect, so no loop is needed. `Middleware` calls `TryAcquire`, returns 503 on a
shed, and `defer`s `Release` around the wrapped handler.

Create `limiter.go`:

```go
package admission

import (
	"net/http"
	"sync/atomic"
)

// Limiter is a lock-free in-flight concurrency limiter for admission control.
// It admits at most max concurrent holders and sheds the rest.
type Limiter struct {
	inflight atomic.Int64
	max      int64
}

func NewLimiter(max int64) *Limiter {
	return &Limiter{max: max}
}

// TryAcquire admits one slot if the current in-flight count is below the limit,
// returning true. When full it returns false without incrementing, so the caller
// can shed load. The ceiling is enforced atomically via a CompareAndSwap loop.
func (l *Limiter) TryAcquire() bool {
	for {
		cur := l.inflight.Load()
		if cur >= l.max {
			return false
		}
		if l.inflight.CompareAndSwap(cur, cur+1) {
			return true
		}
	}
}

// Release returns a slot acquired by TryAcquire. Call it exactly once per
// successful TryAcquire.
func (l *Limiter) Release() {
	l.inflight.Add(-1)
}

// InFlight reports the current number of held slots.
func (l *Limiter) InFlight() int64 {
	return l.inflight.Load()
}

// Middleware sheds load with HTTP 503 when the limiter is full; otherwise it
// runs next and releases the slot afterward.
func (l *Limiter) Middleware(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if !l.TryAcquire() {
			http.Error(w, "overloaded", http.StatusServiceUnavailable)
			return
		}
		defer l.Release()
		next.ServeHTTP(w, r)
	})
}
```

### The runnable demo

Create `cmd/demo/main.go`:

```go
package main

import (
	"fmt"

	"example.com/admission"
)

func main() {
	l := admission.NewLimiter(2)

	fmt.Println("acquire 1:", l.TryAcquire())
	fmt.Println("acquire 2:", l.TryAcquire())
	fmt.Println("acquire 3 (full):", l.TryAcquire())
	fmt.Println("in-flight:", l.InFlight())

	l.Release()
	fmt.Println("after release, acquire:", l.TryAcquire())
	fmt.Println("in-flight:", l.InFlight())
}
```

Run it:

```bash
go run ./cmd/demo
```

Expected output:

```
acquire 1: true
acquire 2: true
acquire 3 (full): false
in-flight: 2
after release, acquire: true
in-flight: 2
```

### Tests

`TestNeverExceedsLimit` launches far more goroutines than the limit. Each keeps
retrying `TryAcquire` until admitted, then — while holding the slot — increments a
live-concurrency counter and feeds it to a peak tracker, holds briefly, decrements,
and `Release`s. The observed peak concurrency must never exceed the limit, and the
final in-flight count must be zero. The peak tracker is a tiny CAS-loop max, bundled
inline so the module stays standalone.

Create `limiter_test.go`:

```go
package admission

import (
	"net/http"
	"net/http/httptest"
	"sync"
	"sync/atomic"
	"testing"
)

// peakMax is a small inline high-water-mark tracker (there is no atomic Max).
type peakMax struct{ v atomic.Int64 }

func (p *peakMax) observe(x int64) {
	for {
		cur := p.v.Load()
		if x <= cur {
			return
		}
		if p.v.CompareAndSwap(cur, x) {
			return
		}
	}
}

func TestNeverExceedsLimit(t *testing.T) {
	t.Parallel()

	const limit = 4
	l := NewLimiter(limit)

	var live atomic.Int64
	var peak peakMax

	const goroutines = 200
	var wg sync.WaitGroup
	wg.Add(goroutines)
	for range goroutines {
		go func() {
			defer wg.Done()
			for !l.TryAcquire() {
				// full: retry until admitted
			}
			peak.observe(live.Add(1))
			live.Add(-1)
			l.Release()
		}()
	}
	wg.Wait()

	if got := peak.v.Load(); got > limit {
		t.Fatalf("peak concurrency = %d, exceeds limit %d", got, limit)
	}
	if got := l.InFlight(); got != 0 {
		t.Fatalf("in-flight = %d after all released, want 0", got)
	}
}

func TestMiddlewareSheds(t *testing.T) {
	t.Parallel()

	l := NewLimiter(1)
	// Occupy the only slot so the middleware must shed.
	if !l.TryAcquire() {
		t.Fatal("first TryAcquire should succeed")
	}
	defer l.Release()

	h := l.Middleware(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
	}))
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/", nil))

	if rec.Code != http.StatusServiceUnavailable {
		t.Fatalf("status = %d, want 503 (shed)", rec.Code)
	}
}

func TestMiddlewareAdmits(t *testing.T) {
	t.Parallel()

	l := NewLimiter(1)
	h := l.Middleware(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
	}))
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/", nil))

	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200", rec.Code)
	}
	if got := l.InFlight(); got != 0 {
		t.Fatalf("in-flight = %d after handler, want 0", got)
	}
}
```

## Review

The limiter is correct when the observed peak concurrency never exceeds `max` under
any interleaving and every acquired slot is released — `TestNeverExceedsLimit` under
`-race` is the proof, and the in-flight-returns-to-zero check catches a
Release/Acquire imbalance. The defining mistake is the `Add(1)`-then-check pattern,
which lets rejected callers push the counter past the limit (corrupting the
`InFlight()` gauge every probe and dashboard reads) and shed load spuriously when
racing rejectors inflate the count; the ceiling must be part of the atomic step,
which is what the CAS loop provides. Pair every successful `TryAcquire` with
exactly one `Release` (the middleware's `defer` guarantees it).

## Resources

- [`atomic.Int64.CompareAndSwap`](https://pkg.go.dev/sync/atomic#Int64.CompareAndSwap) — the bounded-increment primitive.
- [`golang.org/x/sync/semaphore`](https://pkg.go.dev/golang.org/x/sync/semaphore) — a weighted semaphore for the blocking variant of this pattern.
- [Handling Overload (Google SRE)](https://sre.google/sre-book/handling-overload/) — why load shedding beats unbounded admission.

---

Back to [00-concepts.md](00-concepts.md) | Previous: [05-peak-gauge-highwater-cas.md](05-peak-gauge-highwater-cas.md) | Next: [07-exactly-once-guard.md](07-exactly-once-guard.md)
