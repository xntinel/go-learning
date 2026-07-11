# Exercise 2: Retry Policy with Backoff and Jitter

A retry policy decides two things: whether a failed response is worth retrying at all, and how long to wait before the next attempt. Get the first wrong and you retry a POST that already committed; get the second wrong and a thousand clients retry in lockstep and turn one spike into many. This exercise builds the policy — an idempotent-method allow-list, retriable status codes, exponential backoff with full jitter, and a context-aware sleep.

This module is fully self-contained. It begins with its own `go mod init`, defines every type it needs, and ships its own demo and tests. Nothing here imports any other exercise.

## What you'll build

```text
retry.go               RetryPolicy, DefaultRetryPolicy, IsRetriable, Backoff, Sleep
cmd/
  demo/
    main.go            print retriability decisions and the backoff schedule
retry_test.go          retriability table, backoff growth and cap, jitter bounds, ctx abort
```

- Files: `retry.go`, `cmd/demo/main.go`, `retry_test.go`.
- Implement: `RetryPolicy` with `IsRetriable(method string, code int) bool`, `Backoff(n int) time.Duration`, and `Sleep(ctx, attempt) error`, plus the `DefaultRetryPolicy` constructor.
- Test: `retry_test.go` checks the method/code matrix, asserts backoff doubles and caps, bounds the jittered delay inside `[d, d*(1+f)]`, and proves `Sleep` aborts immediately on a cancelled context.
- Verify: `go test -race ./...`

Set up the module:

```bash
mkdir -p retry-backoff/cmd/demo && cd retry-backoff
go mod init example.com/retry-backoff
go mod edit -go=1.26
```

### Why only idempotent methods, and why jitter is not optional

Retriability is a two-part decision and both parts must hold. The method must be idempotent — repeating it must have the same effect as doing it once — and the status code must indicate a transient upstream failure rather than a client error. `IsRetriable` checks the method first because that gate is absolute: a 503 from a GET is a transient failure worth retrying, but a 503 from a POST is a trap, because the POST may have committed its write inside the upstream before the connection dropped, and a retry would duplicate it. The default policy therefore allows GET, HEAD, and OPTIONS, and the retriable codes are 502, 503, and 504 — the three "upstream is unavailable or timed out" gateway codes. A 500 is deliberately excluded: it usually means the request itself triggered a bug, and retrying it just reproduces the bug.

Backoff is where naive retry logic does the most damage. If every client waits a fixed 100ms after a 503, they all retry at the same instant and produce a second synchronized spike, which is precisely when the upstream can least absorb it. Exponential backoff — `base * 2^n` — spreads attempts out over time, but synchronized clients still cluster at each doubling boundary. Full jitter breaks the synchronization by adding a uniform random component: the actual delay is drawn from `[d, d*(1+jitterFactor)]` where `d` is the capped exponential. Two clients that failed at the same instant now wait different amounts and re-arrive at the upstream spread across a window rather than all at once. The AWS study in the Resources section measured this directly and found full jitter minimises total work under load.

`Sleep` waits in a `select` against `ctx.Done()` rather than calling `time.Sleep`. A bare sleep is uninterruptible: if the caller's deadline expires or the request is cancelled mid-backoff, a `time.Sleep(10*time.Second)` keeps the goroutine parked for the full ten seconds doing nothing useful. Selecting on the context means a cancellation aborts the wait the instant it happens and returns `ctx.Err()`, so the retry loop above can give up promptly instead of holding a connection slot open through a backoff nobody is waiting for.

Create `retry.go`:

```go
// Package retry implements the retry policy for forwarded HTTP requests:
// which method/status combinations are retriable, and exponential backoff
// with full jitter that honours context cancellation.
package retry

import (
	"context"
	"math"
	"math/rand/v2"
	"net/http"
	"time"
)

// RetryPolicy configures retry logic for forwarded requests.
type RetryPolicy struct {
	// MaxRetries is the maximum number of additional attempts after the first.
	MaxRetries int
	// RetriableCodes is the set of HTTP status codes that trigger a retry.
	RetriableCodes map[int]struct{}
	// RetriableMethods is the set of HTTP methods eligible for retry.
	// Only idempotent methods are retriable by default.
	RetriableMethods map[string]struct{}
	// BackoffBase is the starting duration for exponential backoff.
	BackoffBase time.Duration
	// BackoffMax caps the computed backoff before jitter is added.
	BackoffMax time.Duration
	// JitterFactor adds multiplicative randomness:
	// actual delay is in [d, d*(1+JitterFactor)] where d is the capped exponential.
	JitterFactor float64
}

// DefaultRetryPolicy returns sensible production defaults:
// three retries on 502/503/504 for GET/HEAD/OPTIONS, 100ms to 10s backoff,
// 50% jitter.
func DefaultRetryPolicy() *RetryPolicy {
	return &RetryPolicy{
		MaxRetries: 3,
		RetriableCodes: map[int]struct{}{
			http.StatusBadGateway:         {},
			http.StatusServiceUnavailable: {},
			http.StatusGatewayTimeout:     {},
		},
		RetriableMethods: map[string]struct{}{
			http.MethodGet:     {},
			http.MethodHead:    {},
			http.MethodOptions: {},
		},
		BackoffBase:  100 * time.Millisecond,
		BackoffMax:   10 * time.Second,
		JitterFactor: 0.5,
	}
}

// IsRetriable reports whether the combination of HTTP method and status code
// should trigger a retry attempt.
func (rp *RetryPolicy) IsRetriable(method string, code int) bool {
	if _, ok := rp.RetriableMethods[method]; !ok {
		return false
	}
	_, ok := rp.RetriableCodes[code]
	return ok
}

// Backoff returns the wait duration before attempt n (0-indexed).
// Formula: min(BackoffBase*2^n, BackoffMax) + uniform([0, d*JitterFactor]).
func (rp *RetryPolicy) Backoff(n int) time.Duration {
	d := time.Duration(float64(rp.BackoffBase) * math.Pow(2, float64(n)))
	if d > rp.BackoffMax {
		d = rp.BackoffMax
	}
	if rp.JitterFactor > 0 {
		d += time.Duration(float64(d) * rp.JitterFactor * rand.Float64())
	}
	return d
}

// Sleep waits for Backoff(attempt) or returns early when ctx is cancelled.
func (rp *RetryPolicy) Sleep(ctx context.Context, attempt int) error {
	t := time.NewTimer(rp.Backoff(attempt))
	defer t.Stop()
	select {
	case <-ctx.Done():
		return ctx.Err()
	case <-t.C:
		return nil
	}
}
```

Note that the cap is applied to the exponential *before* the jitter is added, so the maximum possible delay is `BackoffMax * (1+JitterFactor)`, not `BackoffMax`. That is intentional: the cap bounds the deterministic growth, and the jitter then spreads attempts within and slightly past that ceiling. `Backoff` is a pure function of `n` (apart from the random draw), which is what lets the tests assert exact values when `JitterFactor` is zero and assert bounds when it is not.

### The runnable demo

The demo disables jitter so the schedule is reproducible, prints the retriability decision for three representative method/code pairs, then prints the backoff for attempts 0 through 3 and the capped value for a large attempt number. Finally it shows that `Sleep` returns immediately on an already-cancelled context.

Create `cmd/demo/main.go`:

```go
package main

import (
	"context"
	"fmt"
	"net/http"

	"example.com/retry-backoff"
)

func main() {
	rp := retry.DefaultRetryPolicy()
	rp.JitterFactor = 0 // disable jitter for a deterministic demo

	fmt.Println("GET 503 retriable:", rp.IsRetriable(http.MethodGet, http.StatusServiceUnavailable))
	fmt.Println("POST 503 retriable:", rp.IsRetriable(http.MethodPost, http.StatusServiceUnavailable))
	fmt.Println("GET 404 retriable:", rp.IsRetriable(http.MethodGet, http.StatusNotFound))

	for n := 0; n < 4; n++ {
		fmt.Printf("backoff(%d) = %v\n", n, rp.Backoff(n))
	}
	fmt.Printf("backoff(100) capped = %v\n", rp.Backoff(100))

	ctx, cancel := context.WithCancel(context.Background())
	cancel() // already cancelled
	fmt.Println("sleep with cancelled ctx errors:", rp.Sleep(ctx, 0) != nil)
}
```

Run it:

```bash
go run ./cmd/demo
```

Expected output:

```
GET 503 retriable: true
POST 503 retriable: false
GET 404 retriable: false
backoff(0) = 100ms
backoff(1) = 200ms
backoff(2) = 400ms
backoff(3) = 800ms
backoff(100) capped = 10s
sleep with cancelled ctx errors: true
```

### Tests

`TestRetryPolicyIsRetriable` walks the full method/code matrix, including the POST-is-not-retriable and 500-is-not-retriable cases that the policy exists to enforce. `TestRetryPolicyBackoffGrows` runs with jitter disabled so it can assert exact doubling and the cap. `TestRetryPolicyJitterBounds` runs many draws with jitter enabled and asserts every result lands inside `[d, d*(1+f)]`, which is the property jitter must satisfy without being deterministic. `TestRetryPolicySleepRespectsContext` proves the context-aware wait aborts promptly.

Create `retry_test.go`:

```go
package retry

import (
	"context"
	"fmt"
	"net/http"
	"testing"
	"time"
)

func TestRetryPolicyIsRetriable(t *testing.T) {
	t.Parallel()

	rp := DefaultRetryPolicy()
	cases := []struct {
		method string
		code   int
		want   bool
	}{
		{http.MethodGet, http.StatusServiceUnavailable, true},
		{http.MethodGet, http.StatusBadGateway, true},
		{http.MethodGet, http.StatusGatewayTimeout, true},
		{http.MethodGet, http.StatusOK, false},
		{http.MethodGet, http.StatusNotFound, false},
		{http.MethodGet, http.StatusInternalServerError, false}, // 500 not retriable
		{http.MethodPost, http.StatusServiceUnavailable, false}, // POST not idempotent
		{http.MethodPut, http.StatusBadGateway, false},
		{http.MethodHead, http.StatusServiceUnavailable, true},
		{http.MethodOptions, http.StatusServiceUnavailable, true},
	}
	for _, tc := range cases {
		got := rp.IsRetriable(tc.method, tc.code)
		if got != tc.want {
			t.Errorf("IsRetriable(%q, %d) = %v, want %v", tc.method, tc.code, got, tc.want)
		}
	}
}

func TestRetryPolicyBackoffGrows(t *testing.T) {
	t.Parallel()

	rp := &RetryPolicy{
		BackoffBase:  100 * time.Millisecond,
		BackoffMax:   10 * time.Second,
		JitterFactor: 0, // disable jitter for deterministic bounds
	}
	if rp.Backoff(0) != 100*time.Millisecond {
		t.Errorf("Backoff(0) = %v, want 100ms", rp.Backoff(0))
	}
	if rp.Backoff(1) != 200*time.Millisecond {
		t.Errorf("Backoff(1) = %v, want 200ms", rp.Backoff(1))
	}
	if rp.Backoff(2) != 400*time.Millisecond {
		t.Errorf("Backoff(2) = %v, want 400ms", rp.Backoff(2))
	}
	if rp.Backoff(100) != 10*time.Second {
		t.Errorf("Backoff(100) = %v, want 10s (cap)", rp.Backoff(100))
	}
}

func TestRetryPolicyJitterBounds(t *testing.T) {
	t.Parallel()

	rp := &RetryPolicy{
		BackoffBase:  100 * time.Millisecond,
		BackoffMax:   10 * time.Second,
		JitterFactor: 0.5,
	}
	base := 100 * time.Millisecond // Backoff(0) deterministic part
	upper := base + time.Duration(float64(base)*0.5)
	for i := 0; i < 1000; i++ {
		d := rp.Backoff(0)
		if d < base || d > upper {
			t.Fatalf("Backoff(0) = %v, want within [%v, %v]", d, base, upper)
		}
	}
}

func TestRetryPolicySleepRespectsContext(t *testing.T) {
	t.Parallel()

	rp := &RetryPolicy{BackoffBase: 10 * time.Second, BackoffMax: 10 * time.Second, JitterFactor: 0}
	ctx, cancel := context.WithCancel(context.Background())
	cancel() // already cancelled

	start := time.Now()
	if err := rp.Sleep(ctx, 0); err == nil {
		t.Fatal("Sleep with cancelled context should return an error")
	}
	if elapsed := time.Since(start); elapsed > 500*time.Millisecond {
		t.Errorf("Sleep did not abort promptly: elapsed %v", elapsed)
	}
}

func ExampleRetryPolicy_IsRetriable() {
	rp := DefaultRetryPolicy()
	fmt.Println(rp.IsRetriable(http.MethodGet, http.StatusServiceUnavailable))
	fmt.Println(rp.IsRetriable(http.MethodPost, http.StatusServiceUnavailable))
	fmt.Println(rp.IsRetriable(http.MethodGet, http.StatusNotFound))
	// Output:
	// true
	// false
	// false
}
```

## Review

The policy is correct when retriability is conjunctive — method *and* code — and when the backoff math is reproducible with jitter off and bounded with jitter on. The classic mistake is putting a non-idempotent method in the retriable set; the test deliberately asserts POST and PUT are not retriable so a careless edit fails loudly. The second mistake is computing the cap after adding jitter, which would clamp every jittered value to exactly `BackoffMax` and destroy the spread; the cap is applied to the deterministic exponential first. The third is using `time.Sleep` in the wait, which cannot be cancelled — the `select` on `ctx.Done()` is what lets a deadline abort a backoff mid-wait, and `TestRetryPolicySleepRespectsContext` would hang for ten seconds if that were wrong.

## Resources

- [Exponential Backoff and Jitter (AWS Architecture Blog)](https://aws.amazon.com/blogs/architecture/exponential-backoff-and-jitter/) — the empirical analysis that recommends full jitter.
- [math/rand/v2](https://pkg.go.dev/math/rand/v2) — the modern random source used for the jitter draw.
- [context.Context](https://pkg.go.dev/context#Context) — the cancellation contract that `Sleep` honours.

---

Back to [01-circuit-breaker.md](01-circuit-breaker.md) | Next: [03-retry-budget.md](03-retry-budget.md)
