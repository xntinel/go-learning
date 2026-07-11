# Exercise 8: Retry Budgets — A Token Bucket That Caps the Retry Storm

A per-request `MaxAttempts` bounds one call but does nothing to stop a whole fleet
from tripling its load on a struggling dependency. The systemic defense, from
Google's SRE practice, is a client-side *retry budget*: a token bucket where each
retry costs a token and successful requests refill it, so retries can never exceed a
fixed fraction of traffic. When the budget runs dry — which happens precisely during
an outage — retries are suppressed and the original error is returned.

This module is fully self-contained: its own `go mod init`, all types inline, its
own demo and tests.

## What you'll build

```text
retrybudget/               independent module: example.com/retrybudget
  go.mod                   go 1.26
  budget.go                Budget token bucket; Client.Do consuming it
  cmd/
    demo/
      main.go              runnable demo: budget caps the retries in a failure burst
  budget_test.go           tests: retries bounded by ratio; successes refill; -race
```

Files: `budget.go`, `cmd/demo/main.go`, `budget_test.go`.
Implement: a `Budget` with a token bucket (each retry costs 1 token, each request adds `Ratio` tokens, capped at `Max`), `TryRetry() bool`, and a `Client.Do` that only retries when the budget permits.
Test: a burst of failing ops retries no more than the budget allows (not `MaxAttempts × requests`); a stream of successes refills and re-enables retries; `-race` with no negative token count.
Verify: `go test -count=1 -race ./...`

```bash
mkdir -p ~/go-exercises/retrybudget/cmd/demo
cd ~/go-exercises/retrybudget
go mod init example.com/retrybudget
go mod edit -go=1.26
```

### Retries as a fraction of traffic, not a per-call constant

The insight behind the SRE retry budget is that the dangerous quantity is not "how
many times does one request retry" but "how much extra load does the fleet generate".
`MaxAttempts: 3` looks bounded per call, yet a million requests all failing and all
retrying twice is two million *extra* requests aimed at a dependency that is already
down. That is the retry amplification that turns a partial outage into a total one.

A token bucket caps the *ratio* instead. Model it as a bucket of tokens:

- Every *request* (success or failure) adds `Ratio` tokens, up to a maximum `Max`.
  With `Ratio = 0.1`, ten normal requests earn one token.
- Every *retry* costs one token. A retry is permitted only if a token is available;
  `TryRetry` atomically checks-and-decrements.

The steady-state effect: when almost everything is succeeding, tokens accumulate and
the occasional retry is freely funded. When almost everything is *failing* — an
outage — few requests complete to refill the bucket, so it drains and additional
retries are suppressed. The extra load a total outage can generate is therefore
capped at roughly `Ratio` times normal traffic (plus the `Max` burst), *no matter how
many clients or requests* there are. This is the guardrail `MaxAttempts` cannot
provide, because `MaxAttempts` has no notion of aggregate traffic.

Google's implementation frames it as "for every request, credit the budget; for every
retry, debit it; only retry if the balance is positive". The `Ratio` is the fraction
of extra load you are willing to tolerate — 0.1 for +10%. This module implements
exactly that, with the token count guarded by a mutex so concurrent requests never
drive it negative or race.

`Client.Do` composes the budget with an ordinary retry loop: it always makes the
first attempt (the budget only governs *retries*, never the initial request), credits
the budget once for the request, and on a retryable failure asks
`budget.TryRetry()` before trying again. If the budget refuses, `Do` returns the last
error immediately — the request fails fast rather than adding to the storm.

Create `budget.go`:

```go
package retrybudget

import (
	"context"
	"sync"
)

// Budget is a client-side retry budget: a token bucket where each request credits
// Ratio tokens (capped at Max) and each retry debits one. It caps retries as a
// fraction of traffic, preventing fleet-wide retry amplification.
type Budget struct {
	Ratio float64 // tokens credited per request (e.g. 0.1 for +10% retries)
	Max   float64 // maximum tokens the bucket can hold (burst)

	mu     sync.Mutex
	tokens float64
}

// NewBudget returns a Budget starting with a full bucket so early retries are funded.
func NewBudget(ratio, max float64) *Budget {
	return &Budget{Ratio: ratio, Max: max, tokens: max}
}

// Credit records that a request happened, adding Ratio tokens up to Max.
func (b *Budget) Credit() {
	b.mu.Lock()
	defer b.mu.Unlock()
	b.tokens += b.Ratio
	if b.tokens > b.Max {
		b.tokens = b.Max
	}
}

// TryRetry consumes one token if available, returning true if a retry is permitted.
func (b *Budget) TryRetry() bool {
	b.mu.Lock()
	defer b.mu.Unlock()
	if b.tokens >= 1 {
		b.tokens--
		return true
	}
	return false
}

// Tokens returns the current balance (for observability and tests).
func (b *Budget) Tokens() float64 {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.tokens
}

// Op is the retryable unit of work.
type Op func(ctx context.Context) error

// Client retries an Op but only while the shared Budget permits.
type Client struct {
	Budget      *Budget
	MaxAttempts int
	Retryable   func(error) bool
}

// Do makes the first attempt unconditionally, then retries a transient failure only
// while the budget has tokens. It always credits the budget once per call.
func (c *Client) Do(ctx context.Context, op Op) error {
	c.Budget.Credit() // one request = one credit
	var lastErr error
	for attempt := range c.MaxAttempts {
		err := op(ctx)
		if err == nil {
			return nil
		}
		lastErr = err
		if c.Retryable != nil && !c.Retryable(err) {
			return err
		}
		if attempt == c.MaxAttempts-1 {
			break
		}
		if !c.Budget.TryRetry() {
			return lastErr // budget exhausted: fail fast, do not add to the storm
		}
		if err := ctx.Err(); err != nil {
			return err
		}
	}
	return lastErr
}
```

### The runnable demo

The demo starts an empty-ish budget (small `Max`), fires a burst of always-failing
requests, and shows that the total number of *retries* is bounded by the budget, not
by `requests × MaxAttempts`.

Create `cmd/demo/main.go`:

```go
package main

import (
	"context"
	"errors"
	"fmt"

	"example.com/retrybudget"
)

func main() {
	// Ratio 0.1, Max 5: at most ~5 burst retries plus 0.1 per request.
	budget := retrybudget.NewBudget(0.1, 5)
	client := &retrybudget.Client{
		Budget:      budget,
		MaxAttempts: 3,
		Retryable:   func(error) bool { return true },
	}

	var totalOpCalls int
	fail := func(context.Context) error { totalOpCalls++; return errors.New("down") }

	const requests = 100
	for range requests {
		_ = client.Do(context.Background(), fail)
	}

	// Without a budget, 100 requests * 3 attempts = 300 op calls.
	// With the budget, retries are capped: op calls = 100 first-tries + bounded retries.
	retries := totalOpCalls - requests
	fmt.Printf("requests: %d\n", requests)
	fmt.Printf("total op calls: %d\n", totalOpCalls)
	fmt.Printf("retries (bounded by budget, not %d): %d\n", requests*2, retries)
	fmt.Printf("naive would be: %d\n", requests*3)
}
```

Run it:

```bash
go run ./cmd/demo
```

Expected output:

```
requests: 100
total op calls: 114
retries (bounded by budget, not 200): 14
naive would be: 300
```

The retry count is roughly the initial burst (`Max = 5`) plus `Ratio × requests`
(`0.1 × 100 = 10`), with a fractional token stranded below 1 — far below the naive
200 extra attempts.

### Tests

`TestRetriesBoundedByBudget` fires a burst of failing requests and asserts the total
retries is close to `Max + Ratio × requests`, not `requests × (MaxAttempts-1)`.
`TestSuccessesRefill` drains the budget, then runs successful requests and asserts the
balance climbs back so retries are re-enabled. `TestNeverNegative` hammers `TryRetry`
and `Credit` concurrently under `-race` and asserts the token count never goes below
zero.

Create `budget_test.go`:

```go
package retrybudget

import (
	"context"
	"errors"
	"fmt"
	"sync"
	"testing"
)

var errDown = errors.New("down")

func TestRetriesBoundedByBudget(t *testing.T) {
	t.Parallel()
	budget := NewBudget(0.1, 5)
	client := &Client{Budget: budget, MaxAttempts: 3, Retryable: func(error) bool { return true }}

	var opCalls int
	fail := func(context.Context) error { opCalls++; return errDown }

	const requests = 100
	for range requests {
		_ = client.Do(context.Background(), fail)
	}
	retries := opCalls - requests

	// Budget allows Max (5) burst + Ratio*requests (0.1*100=10) = 15 retries.
	wantMax := 5 + int(0.1*requests) + 1 // +1 slack for float rounding
	if retries > wantMax {
		t.Fatalf("retries = %d, want <= %d (budget must cap the storm)", retries, wantMax)
	}
	// Sanity: it must be far below the naive requests*(MaxAttempts-1)=200.
	if retries >= requests*(3-1) {
		t.Fatalf("retries = %d, budget had no effect", retries)
	}
}

func TestSuccessesRefillBudget(t *testing.T) {
	t.Parallel()
	budget := NewBudget(0.5, 3)

	// Drain the bucket.
	for budget.TryRetry() {
	}
	if budget.TryRetry() {
		t.Fatal("budget should be empty")
	}

	// Ten successful requests credit 0.5 each = 5 tokens (capped at Max=3).
	client := &Client{Budget: budget, MaxAttempts: 3, Retryable: func(error) bool { return true }}
	for range 10 {
		_ = client.Do(context.Background(), func(context.Context) error { return nil })
	}
	if !budget.TryRetry() {
		t.Fatal("budget did not refill after successes")
	}
}

func TestNeverNegativeUnderConcurrency(t *testing.T) {
	t.Parallel()
	budget := NewBudget(0.3, 50)

	var wg sync.WaitGroup
	for range 500 {
		wg.Add(1)
		go func() {
			defer wg.Done()
			budget.Credit()
			budget.TryRetry()
		}()
	}
	wg.Wait()
	if got := budget.Tokens(); got < 0 {
		t.Fatalf("tokens = %v, want >= 0 (never negative)", got)
	}
}

func ExampleBudget() {
	budget := NewBudget(0.1, 2)
	fmt.Println(budget.TryRetry()) // token available
	fmt.Println(budget.TryRetry()) // last token
	fmt.Println(budget.TryRetry()) // empty
	budget.Credit()
	budget.Credit()
	fmt.Println(budget.Tokens())
	// Output:
	// true
	// true
	// false
	// 0.2
}
```

## Review

The budget is correct when a burst of failures produces retries bounded by
`Max + Ratio × requests` rather than `requests × (MaxAttempts−1)` — that gap is the
retry amplification the budget removes. Successes must refill the bucket so retries
recover once the dependency does, and the token count must never go negative under
concurrency. The mistake this design prevents: aggressive retries with no systemic
cap, so a sustained outage receives multiples of normal load and becomes a
self-sustaining metastable failure. Run `go test -race`; the token count is guarded by
a mutex, so the concurrent `Credit`/`TryRetry` test must be clean and never negative.

## Resources

- [Google SRE Book: Handling Overload](https://sre.google/sre-book/handling-overload/) — the client-side retry budget and retry amplification.
- [gRPC: Retry Design](https://github.com/grpc/proposal/blob/master/A6-client-retries.md) — a production retry-throttling token bucket.
- [`sync#Mutex`](https://pkg.go.dev/sync#Mutex) — guarding the token count.

---

Back to [00-concepts.md](00-concepts.md) | Previous: [07-circuit-breaker-with-retry.md](07-circuit-breaker-with-retry.md) | Next: [09-retry-observability.md](09-retry-observability.md)
