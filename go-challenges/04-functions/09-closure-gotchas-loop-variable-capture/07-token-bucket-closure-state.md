# Exercise 7: Rate Limiter as a Stateful Closure: Intended Capture-by-Reference

Not every captured variable is a bug. A token-bucket limiter is a closure that
deliberately captures shared mutable state — the token count and the last-refill
time — and mutates it across calls. This exercise is the other side of the
lesson: capture-by-reference AS the design. The loop trap reappears in a business
costume, though — when you build one limiter per tenant in a loop, each tenant
must capture its OWN state cell, or every tenant shares one bucket.

## What you'll build

```text
ratelimit/                   independent module: example.com/ratelimit
  go.mod                     go 1.26
  ratelimit.go               Allow closure over mutex-guarded state; NewLimiter, PerTenant
  cmd/
    demo/
      main.go                runnable demo: burst, exhaust, refill
  ratelimit_test.go          burst/refill, independent tenants, -race concurrent Allow
```

- Files: `ratelimit.go`, `cmd/demo/main.go`, `ratelimit_test.go`.
- Implement: `NewLimiter(capacity, refill)` returning an `Allow func() bool` closure over mutex-guarded `tokens`/`last`; `PerTenant(tenants, ...)` building one independent limiter per tenant in a loop.
- Test: burst to capacity returns true then false; refill after simulated time restores tokens; per-tenant limiters are independent (exhausting A does not affect B); concurrent `Allow` is `-race` clean.
- Verify: `go test -count=1 -race ./...`

Set up the module:

```bash
mkdir -p ~/go-exercises/ratelimit/cmd/demo
cd ~/go-exercises/ratelimit
go mod init example.com/ratelimit
go mod edit -go=1.26
```

### Capture-by-reference as the design

The limiter is a closure. `NewLimiter` declares local variables `tokens`, `last`,
and a `mu sync.Mutex`, and returns an `Allow func() bool` that closes over them.
Because closures capture the variable, every call to `Allow` reads and writes the
same `tokens` cell — that persistence across calls IS the limiter. This is the
exact mechanism that causes the loop-capture bug, used on purpose. The state
escapes to the heap (the closure outlives `NewLimiter`), which is expected and
correct here.

Because a limiter is typically shared across request-handling goroutines, `Allow`
must guard its state. A `sync.Mutex` around the read-modify-write of `tokens` is
what turns an otherwise-racy shared closure into a safe one; without it, the
race detector flags `tokens++`/`tokens--` immediately. This is the concrete form
of the concepts rule "guard captured state with a mutex when the closure escapes
to multiple goroutines."

To keep the test deterministic without sleeping real seconds, time is injected
through a `now func() time.Time` parameter — a clock the test controls. (This is
the clock-injection pattern; a `synctest` bubble is the alternative, but injecting
`now` keeps this module buildable on any toolchain.) Refill is computed lazily on
each `Allow`: elapsed time since `last` times the refill rate, capped at capacity.

`PerTenant` is where the loop trap returns. It iterates tenant IDs and builds one
limiter per tenant, storing each `Allow` in a map. Each `NewLimiter` call creates
a FRESH set of captured variables, so each tenant's limiter owns independent
state. If instead you hoisted `tokens`/`last` out of the loop and closed over
them, every tenant would share one bucket — exhausting tenant A would throttle
tenant B. The test pins that the buckets are independent.

Create `ratelimit.go`:

```go
package ratelimit

import (
	"sync"
	"time"
)

// Allow reports whether one unit of work may proceed now, consuming a token.
type Allow func() bool

// NewLimiter returns a token-bucket limiter as a closure over its own state.
// capacity is the burst size; refillPerSec tokens are added per second, read
// from the injected now clock so tests stay deterministic.
func NewLimiter(capacity float64, refillPerSec float64, now func() time.Time) Allow {
	var mu sync.Mutex
	tokens := capacity
	last := now()

	return func() bool {
		mu.Lock()
		defer mu.Unlock()

		t := now()
		elapsed := t.Sub(last).Seconds()
		tokens += elapsed * refillPerSec
		if tokens > capacity {
			tokens = capacity
		}
		last = t

		if tokens >= 1 {
			tokens--
			return true
		}
		return false
	}
}

// PerTenant builds one independent limiter per tenant. Each NewLimiter call
// captures its own state cell, so tenants do not share a bucket.
func PerTenant(tenants []string, capacity, refillPerSec float64, now func() time.Time) map[string]Allow {
	limiters := make(map[string]Allow, len(tenants))
	for _, tenant := range tenants {
		limiters[tenant] = NewLimiter(capacity, refillPerSec, now)
	}
	return limiters
}
```

### The runnable demo

The demo uses a controllable clock: it bursts through a capacity-3 bucket, sees
the fourth call denied, advances the clock two seconds at 1 token/sec, and sees
calls allowed again.

Create `cmd/demo/main.go`:

```go
package main

import (
	"fmt"
	"sync"
	"time"

	"example.com/ratelimit"
)

func main() {
	var mu sync.Mutex
	current := time.Unix(0, 0)
	now := func() time.Time {
		mu.Lock()
		defer mu.Unlock()
		return current
	}
	advance := func(d time.Duration) {
		mu.Lock()
		defer mu.Unlock()
		current = current.Add(d)
	}

	allow := ratelimit.NewLimiter(3, 1, now)

	for i := range 4 {
		fmt.Printf("burst %d: %v\n", i+1, allow())
	}

	advance(2 * time.Second)
	fmt.Println("after 2s refill:", allow())
}
```

Run it:

```bash
go run ./cmd/demo
```

Expected output:

```
burst 1: true
burst 2: true
burst 3: true
burst 4: false
after 2s refill: true
```

### Tests

`TestBurstThenRefill` exhausts the bucket, asserts denial, advances the injected
clock, and asserts a refill. `TestTenantsAreIndependent` is the loop guard: it
builds limiters for several tenants, drains one, and asserts the others are
untouched. `TestConcurrentAllowIsRaceClean` hammers one limiter from many
goroutines to prove the mutex guards the shared state.

Create `ratelimit_test.go`:

```go
package ratelimit

import (
	"fmt"
	"sync"
	"testing"
	"time"
)

// fakeClock is a test-controlled clock, safe for concurrent reads.
type fakeClock struct {
	mu sync.Mutex
	t  time.Time
}

func (c *fakeClock) now() time.Time {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.t
}

func (c *fakeClock) advance(d time.Duration) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.t = c.t.Add(d)
}

func TestBurstThenRefill(t *testing.T) {
	t.Parallel()

	clk := &fakeClock{t: time.Unix(0, 0)}
	allow := NewLimiter(3, 1, clk.now)

	for i := range 3 {
		if !allow() {
			t.Fatalf("burst call %d denied, want allowed", i+1)
		}
	}
	if allow() {
		t.Fatal("4th call allowed, want denied (bucket empty)")
	}

	clk.advance(2 * time.Second) // refill 2 tokens at 1/sec
	if !allow() {
		t.Fatal("call after refill denied, want allowed")
	}
	if !allow() {
		t.Fatal("second call after 2s refill denied, want allowed")
	}
	if allow() {
		t.Fatal("third call after 2s refill allowed, want denied")
	}
}

func TestTenantsAreIndependent(t *testing.T) {
	t.Parallel()

	clk := &fakeClock{t: time.Unix(0, 0)}
	limiters := PerTenant([]string{"a", "b", "c"}, 2, 1, clk.now)

	// Drain tenant a completely.
	for limiters["a"]() {
	}
	if limiters["a"]() {
		t.Fatal("tenant a should be exhausted")
	}

	// Tenants b and c must be untouched: full burst still available.
	for _, tenant := range []string{"b", "c"} {
		if !limiters[tenant]() || !limiters[tenant]() {
			t.Fatalf("tenant %q was coupled to tenant a (shared bucket)", tenant)
		}
		if limiters[tenant]() {
			t.Fatalf("tenant %q allowed a 3rd call past capacity 2", tenant)
		}
	}
}

func TestConcurrentAllowIsRaceClean(t *testing.T) {
	t.Parallel()

	clk := &fakeClock{t: time.Unix(0, 0)}
	allow := NewLimiter(1000, 0, clk.now)

	var wg sync.WaitGroup
	for range 50 {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for range 20 {
				allow()
			}
		}()
	}
	wg.Wait()
	// 1000 capacity, no refill, 1000 calls: the 1001st must be denied.
	if allow() {
		t.Fatal("limiter allowed more than capacity under concurrency")
	}
}

func ExampleNewLimiter() {
	allow := NewLimiter(1, 0, func() time.Time { return time.Unix(0, 0) })
	fmt.Println(allow(), allow())
	// Output: true false
}
```

## Review

The limiter is correct when a fresh bucket bursts up to capacity, denies once
empty, and refills proportionally to injected elapsed time; and when per-tenant
limiters are independent. `TestTenantsAreIndependent` is the loop guard: hoisting
the state out of the `PerTenant` loop would couple every tenant to one bucket and
fail it. The design lesson is that capture-by-reference is the FEATURE here — the
same mechanism as the bug — and the mutex is mandatory precisely because the
captured state escapes to multiple goroutines. Note `TestConcurrentAllowIsRace
Clean` uses capacity 1000 with 1000 total calls to assert no over-admission; run
`go test -race` so the mutex around `tokens` is actually exercised.

## Resources

- [`sync.Mutex`](https://pkg.go.dev/sync#Mutex) — guarding the captured token state.
- [`time.Time.Sub`](https://pkg.go.dev/time#Time.Sub) — elapsed time for lazy refill.
- [Go spec: Function literals](https://go.dev/ref/spec#Function_literals) — function literals are closures that may refer to variables defined in a surrounding function.

---

Back to [00-concepts.md](00-concepts.md) | Next: [08-shutdown-cleanup-stack.md](08-shutdown-cleanup-stack.md)
