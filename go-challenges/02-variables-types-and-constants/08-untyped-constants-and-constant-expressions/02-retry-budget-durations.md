# Exercise 2: Retry And Timeout Budget From Constant Duration Arithmetic

A resilience layer splits a total request budget into a connect timeout, a TLS
handshake timeout, and a read timeout, and computes an exponential backoff that
caps out. This module encodes those as *typed* `time.Duration` constants — where
the type is part of the contract and a bare `int` will not compile — and contrasts
them with the untyped multipliers that scale them.

This module is fully self-contained: its own `go mod init`, all code inline, its
own demo and tests. Nothing here imports any other exercise.

## What you'll build

```text
retrybudget/                  independent module: example.com/retrybudget
  go.mod                      go 1.26
  budget.go                   typed Duration constants; Backoff(attempt), BudgetFitsSLO,
                              TimeoutSum
  cmd/
    demo/
      main.go                 prints the backoff schedule and the budget check
  budget_test.go              monotonic-then-clamped backoff; constant sum within SLO
```

Files: `budget.go`, `cmd/demo/main.go`, `budget_test.go`.
Implement: `ConnectTimeout`, `TLSTimeout`, `ReadTimeout`, `TotalBudget`,
`BaseBackoff`, `MaxBackoff` as typed `Duration` constants; `Backoff(attempt)` with
exponential growth capped at `MaxBackoff`; `TimeoutSum()` and `BudgetFitsSLO()`.
Test: backoff is monotonic up to the cap and clamps there; the constant timeout
sum stays within the total budget; attempts 0..8.
Verify: `go test -count=1 -race ./...`

Set up the module:

```bash
mkdir -p ~/go-exercises/retrybudget/cmd/demo
cd ~/go-exercises/retrybudget
go mod init example.com/retrybudget
```

### Typed Duration constants are the contract

`time.Second` is a *typed* constant of type `time.Duration`. When you write
`const ConnectTimeout = 2 * time.Second`, the result is a typed `Duration`, and a
function parameter of type `time.Duration` will reject a bare `2` — you must supply
a `Duration`. That refusal is deliberate: it eliminates the "was that seconds or
nanoseconds?" bug at the call site. Contrast the untyped multipliers: in
`2 * time.Second`, the `2` is an untyped integer constant that adapts to the
`Duration` on its right, and in `BaseBackoff << uint(attempt)` the shift count is a
plain integer. The units are typed; the scalars that move them are untyped.

Passing a bare int where a `Duration` is expected is a compile error on purpose.
`Retry(5)` for a `func Retry(d time.Duration)` will not build; you write
`Retry(5 * time.Second)`. Do not defeat this with `time.Duration(5)` — that is five
*nanoseconds*, almost never what you meant. This exercise documents that constraint
rather than executing it, because the whole value is that it never compiles.

### Backoff that grows then clamps

`Backoff(attempt)` returns `BaseBackoff << attempt`, doubling each attempt, capped
at `MaxBackoff`. The subtlety is overflow: shifting a 50ms `Duration` (an `int64`)
left far enough wraps to a negative value. The guard is to stop shifting once the
result is guaranteed to exceed the cap. With `BaseBackoff = 50ms` and
`MaxBackoff = 2s`, `50ms << 6 = 3.2s`, already past the cap, so any attempt ≥ 6
returns `MaxBackoff` directly and never shifts far enough to overflow. Below that,
`min(BaseBackoff<<attempt, MaxBackoff)` (the `min` builtin, Go 1.21+) yields the
doubling schedule, saturating cleanly. The result is monotonic non-decreasing and
provably bounded.

Create `budget.go`:

```go
package retrybudget

import "time"

// Typed time.Duration constants: the type is part of the contract, so a bare
// int cannot be passed where one of these is expected.
const (
	ConnectTimeout = 2 * time.Second
	TLSTimeout     = 1 * time.Second
	ReadTimeout    = 5 * time.Second

	// TotalBudget is the SLO ceiling for one request attempt's timeouts.
	TotalBudget = 10 * time.Second

	BaseBackoff = 50 * time.Millisecond
	MaxBackoff  = 2 * time.Second
)

// capShift is the smallest shift at which BaseBackoff<<capShift already exceeds
// MaxBackoff, so we clamp before the shift can overflow the int64 Duration.
const capShift = 6

// Backoff returns the delay before retry number attempt (0-based): BaseBackoff
// doubled per attempt, clamped at MaxBackoff. It is monotonic non-decreasing.
func Backoff(attempt int) time.Duration {
	if attempt < 0 {
		attempt = 0
	}
	if attempt >= capShift {
		return MaxBackoff
	}
	return min(BaseBackoff<<uint(attempt), MaxBackoff)
}

// TimeoutSum is the compile-time sum of the per-phase timeouts.
func TimeoutSum() time.Duration {
	return ConnectTimeout + TLSTimeout + ReadTimeout
}

// BudgetFitsSLO reports whether the per-phase timeouts fit inside TotalBudget.
func BudgetFitsSLO() bool {
	return TimeoutSum() <= TotalBudget
}
```

### The runnable demo

Create `cmd/demo/main.go`:

```go
package main

import (
	"fmt"

	"example.com/retrybudget"
)

func main() {
	fmt.Printf("timeouts sum=%s budget=%s fits=%v\n",
		retrybudget.TimeoutSum(), retrybudget.TotalBudget, retrybudget.BudgetFitsSLO())

	for attempt := 0; attempt <= 8; attempt++ {
		fmt.Printf("attempt %d backoff=%s\n", attempt, retrybudget.Backoff(attempt))
	}
}
```

Run it:

```bash
go run ./cmd/demo
```

Expected output:

```
timeouts sum=8s budget=10s fits=true
attempt 0 backoff=50ms
attempt 1 backoff=100ms
attempt 2 backoff=200ms
attempt 3 backoff=400ms
attempt 4 backoff=800ms
attempt 5 backoff=1.6s
attempt 6 backoff=2s
attempt 7 backoff=2s
attempt 8 backoff=2s
```

### Tests

`TestBackoffSchedule` asserts each attempt's exact delay from a table.
`TestBackoffMonotonicAndClamped` walks attempts 0..8 and asserts the sequence never
decreases and never exceeds `MaxBackoff`, and that it has reached the cap by the
end. `TestBudgetFitsSLO` proves the constant timeout sum stays within the budget —
a check that runs at compile time in spirit and is asserted here for the record.

Create `budget_test.go`:

```go
package retrybudget

import (
	"testing"
	"time"
)

func TestBackoffSchedule(t *testing.T) {
	t.Parallel()

	tests := []struct {
		attempt int
		want    time.Duration
	}{
		{-1, 50 * time.Millisecond},
		{0, 50 * time.Millisecond},
		{1, 100 * time.Millisecond},
		{2, 200 * time.Millisecond},
		{3, 400 * time.Millisecond},
		{4, 800 * time.Millisecond},
		{5, 1600 * time.Millisecond},
		{6, 2 * time.Second},
		{7, 2 * time.Second},
		{8, 2 * time.Second},
	}
	for _, tt := range tests {
		got := Backoff(tt.attempt)
		if got != tt.want {
			t.Errorf("Backoff(%d) = %s, want %s", tt.attempt, got, tt.want)
		}
	}
}

func TestBackoffMonotonicAndClamped(t *testing.T) {
	t.Parallel()

	prev := time.Duration(-1)
	var last time.Duration
	for attempt := 0; attempt <= 8; attempt++ {
		d := Backoff(attempt)
		if d < prev {
			t.Fatalf("Backoff not monotonic at attempt %d: %s < %s", attempt, d, prev)
		}
		if d > MaxBackoff {
			t.Fatalf("Backoff(%d) = %s exceeds MaxBackoff %s", attempt, d, MaxBackoff)
		}
		prev, last = d, d
	}
	if last != MaxBackoff {
		t.Fatalf("Backoff(8) = %s, want clamped to %s", last, MaxBackoff)
	}
}

func TestBudgetFitsSLO(t *testing.T) {
	t.Parallel()

	if got := TimeoutSum(); got != 8*time.Second {
		t.Fatalf("TimeoutSum() = %s, want 8s", got)
	}
	if !BudgetFitsSLO() {
		t.Fatalf("timeouts %s exceed budget %s", TimeoutSum(), TotalBudget)
	}
}
```

## Review

The backoff is correct when it doubles from `BaseBackoff`, never decreases, and
saturates exactly at `MaxBackoff` without ever overflowing — which is why the shift
is guarded by `capShift` instead of shifting an arbitrary amount. The budget check
is a compile-time truth (`ConnectTimeout + TLSTimeout + ReadTimeout` is a constant),
asserted here so a future edit that bumps a timeout past the budget is caught by a
red test. The mistake to avoid is treating the `Duration` type as a nuisance and
reaching for `time.Duration(n)` conversions to pass raw numbers — that reintroduces
exactly the unit bug the typed constant is there to prevent.

## Resources

- [time.Duration](https://pkg.go.dev/time#Duration) — the typed Duration and its unit constants.
- [Go Language Specification: Constant expressions](https://go.dev/ref/spec#Constant_expressions) — how typed and untyped operands combine.
- [min and max builtins](https://pkg.go.dev/builtin#min) — the clamp used in Backoff.

---

Back to [00-concepts.md](00-concepts.md) | Previous: [01-upload-limit-policy.md](01-upload-limit-policy.md) | Next: [03-byte-unit-ladder-si-vs-binary.md](03-byte-unit-ladder-si-vs-binary.md)
