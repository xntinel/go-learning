# Exercise 11: A Per-Client Rate Limiter With No Struct at All

**Nivel: Intermedio** — validacion rapida (un test corto).

`NewLimiter` returns a closure that *is* a rate limiter — no struct, no
methods, just a few captured variables that persist across calls, plus the
clock-injection trick that lets the test assert refill behavior without a
single `time.Sleep`. This module is fully self-contained.

## What you'll build

```text
ratelimiter/               independent module: example.com/token-bucket-closure
  go.mod                   go 1.24
  limiter.go               NewLimiter returns func() bool (token bucket)
  cmd/
    demo/
      main.go              drains a bucket, advances a fake clock, refills
  limiter_test.go          table test: drain, refuse, refill; two-limiter independence
```

- Files: `limiter.go`, `cmd/demo/main.go`, `limiter_test.go`.
- Implement: `NewLimiter(capacity int, refill int, now func() time.Time) func() bool`, an integer token bucket closing over `tokens` and `lastRefill`.
- Test: a table drains the bucket, gets refused, advances the fake clock 2s at `refill=1` for 2 more grants, then refuses; a second test proves two limiters never share tokens.
- Verify: `go test -count=1 ./...`

Set up the module:

```bash
go mod edit -go=1.24
```

### The bucket is just two captured variables

`NewLimiter` declares `tokens` (starting at `capacity`) and `lastRefill`
(starting at `now()`), then returns a closure over both. Every call figures
out how many whole seconds passed since the last refill, adds `refill` tokens
per elapsed second (clamped at `capacity`), advances `lastRefill` by exactly
those whole seconds — not by resetting it to `now()`, so a leftover fractional
second isn't discarded — and spends one token if any remain. There is no
struct and no `New...` type: the closure's captured environment *is* the
limiter's state, and since each call to `NewLimiter` allocates its own fresh
`tokens`/`lastRefill`, two limiters built this way never share memory even
with identical arguments — exactly what a per-client limiter needs.

Taking `now func() time.Time` as a parameter instead of calling `time.Now()`
is what makes the refill math testable: the test passes a closure over a
variable it advances by reassignment, so "2 seconds elapsed" is one line
instead of a real sleep. This version is single-goroutine on purpose — no
mutex guards `tokens`. Production wraps the closure in a `sync.Mutex` (as the
earlier rate-limiter exercise in this lesson does) or reaches for
`golang.org/x/time/rate` instead of hand-rolling this.

Create `limiter.go`:

```go
package ratelimiter

import "time"

// NewLimiter returns a closure implementing a per-client token-bucket rate
// limiter. The bucket starts full with capacity tokens and refills at refill
// tokens per whole elapsed second, capped at capacity. now is injected so
// tests advance a fake clock instead of sleeping.
//
// This implementation is single-goroutine: it has no lock, so calling the
// returned closure from multiple goroutines races on the captured tokens.
// Production code either wraps the closure in a mutex or reaches for
// golang.org/x/time/rate.
func NewLimiter(capacity int, refill int, now func() time.Time) func() bool {
	tokens := capacity
	lastRefill := now()

	return func() bool {
		elapsed := now().Sub(lastRefill)
		seconds := int(elapsed / time.Second)
		if seconds > 0 {
			tokens += refill * seconds
			if tokens > capacity {
				tokens = capacity
			}
			lastRefill = lastRefill.Add(time.Duration(seconds) * time.Second)
		}

		if tokens > 0 {
			tokens--
			return true
		}
		return false
	}
}
```

### The runnable demo

Create `cmd/demo/main.go`:

```go
package main

import (
	"fmt"
	"time"

	"example.com/token-bucket-closure"
)

func main() {
	now := time.Now()
	clock := func() time.Time { return now }
	allow := ratelimiter.NewLimiter(3, 1, clock)

	for i := range 4 {
		fmt.Printf("call %d: %v\n", i+1, allow())
	}

	now = now.Add(2 * time.Second) // refill 2 tokens (refill=1/sec)
	fmt.Printf("after 2s: %v\n", allow())
	fmt.Printf("after 2s: %v\n", allow())
	fmt.Printf("after 2s: %v\n", allow())
}
```

Run it:

```bash
go run ./cmd/demo
```

Expected output:

```
call 1: true
call 2: true
call 3: true
call 4: false
after 2s: true
after 2s: true
after 2s: false
```

### Tests

Create `limiter_test.go`:

```go
package ratelimiter

import (
	"testing"
	"time"
)

// fakeClock returns a clock closure over a cursor plus an advance function,
// so the test moves time forward without sleeping.
func fakeClock(start time.Time) (func() time.Time, func(time.Duration)) {
	cur := start
	now := func() time.Time { return cur }
	advance := func(d time.Duration) { cur = cur.Add(d) }
	return now, advance
}

func TestNewLimiterDrainRefillDeny(t *testing.T) {
	now, advance := fakeClock(time.Unix(0, 0))
	allow := NewLimiter(3, 1, now)

	tests := []struct {
		name    string
		advance time.Duration
		want    bool
	}{
		{"drain 1 of 3", 0, true},
		{"drain 2 of 3", 0, true},
		{"drain 3 of 3", 0, true},
		{"bucket empty", 0, false},
		{"refill after 2s (1/sec), spend 1 of 2", 2 * time.Second, true},
		{"spend 2 of 2 refilled", 0, true},
		{"empty again", 0, false},
	}

	for _, tc := range tests {
		advance(tc.advance)
		if got := allow(); got != tc.want {
			t.Fatalf("%s: allow() = %v, want %v", tc.name, got, tc.want)
		}
	}
}

func TestTwoLimitersDoNotShareTokens(t *testing.T) {
	now, _ := fakeClock(time.Unix(0, 0))
	a := NewLimiter(1, 1, now)
	b := NewLimiter(1, 1, now)

	if !a() {
		t.Fatal("a: first call denied, want allowed (capacity 1)")
	}
	if a() {
		t.Fatal("a: second call allowed, want denied (drained)")
	}
	if !b() {
		t.Fatal("b: first call denied, want allowed — b must not share a's captured tokens")
	}
}
```

## Review

The table test walks the bucket through exactly the sequence the closure
promises: three grants, a refusal, a 2-second advance that refills two tokens
at `refill=1`, two more grants, and a refusal again — one test, one clock, no
sleeping. The second test is the didactic core: `a` and `b` come from two
separate `NewLimiter` calls, so draining `a` has zero effect on `b`, proving
each call captures its own `tokens` and `lastRefill` instead of sharing state.
That is the whole case for this shape: one `NewLimiter` call per client, and
the compiler — not a map key or a struct field — keeps their state apart.

## Resources

- [Go spec: Function literals](https://go.dev/ref/spec#Function_literals) — how a closure captures its enclosing variables.
- [pkg.go.dev: time.Time.Sub](https://pkg.go.dev/time#Time.Sub) — elapsed duration between two instants.
- [pkg.go.dev: golang.org/x/time/rate](https://pkg.go.dev/golang.org/x/time/rate) — the production token-bucket limiter this exercise mirrors.

---

Back to [00-concepts.md](00-concepts.md) | Prev: [10-idempotency-once-guard.md](10-idempotency-once-guard.md) | Next: [12-keyset-pagination-cursor-codec.md](12-keyset-pagination-cursor-codec.md)
