# Exercise 3: Retry Budget

Retries make a slow service worse: when an upstream starts failing, every client retries, and the retries are extra load piled on exactly the service that cannot handle it. A retry budget caps retries as a fraction of real traffic over a sliding window, so a partial outage cannot be amplified into a total one. This exercise builds that budget — a concurrency-safe, time-windowed ratio gate.

This module is fully self-contained. It begins with its own `go mod init`, defines every type it needs, and ships its own demo and tests. Nothing here imports any other exercise.

## What you'll build

```text
budget.go              RetryBudget, NewRetryBudget, Record, Allow (sliding-window ratio)
cmd/
  demo/
    main.go            seed originals, allow a retry, then hit the limit
budget_test.go         within-ratio allow, exhaustion at the limit, empty budget, window pruning
```

- Files: `budget.go`, `cmd/demo/main.go`, `budget_test.go`.
- Implement: `RetryBudget` with `Record(isRetry bool)` and `Allow() error`, plus the `NewRetryBudget(maxRatio, window)` constructor.
- Test: `budget_test.go` allows a retry inside the ratio, blocks at the limit, returns nil when there are no originals, and proves old entries fall out of the window.
- Verify: `go test -race ./...`

Set up the module:

```bash
go mod edit -go=1.26
```

### Why a ratio over a window, and why "no originals" means "allow"

The budget answers one question: of the traffic seen recently, what fraction is retries, and is that fraction over the limit? It cannot answer that question per request. A single request in isolation has one original; the first retry pushes the ratio to 100%, so any per-request budget below 100% would block all retries and any at 100% would block none. The metric only means something across many requests, so the budget keeps a sliding window of recent entries — each tagged original or retry — and computes the ratio over the whole window.

`Record` stamps each entry with the current time and appends it; `Allow` first prunes everything older than the window, then counts originals and retries and compares `retries/originals` against `maxRatio`. Pruning on every call keeps the window honest without a background goroutine: entries naturally age out as the cutoff advances. Because the entries are time-ordered (each append happens at a later instant than the last), pruning is a single forward scan to the first entry inside the window, then a reslice — no scan of the whole buffer.

The `originals == 0` case returns nil, meaning "allow". This is the deliberate low-traffic behaviour. With no originals there is no denominator and no basis to call anything excessive, so the budget gets out of the way: at low request rates it acts as no-limit, and the cap only bites once sustained traffic has accumulated enough originals for the ratio to be meaningful. The comparison uses `>=` rather than `>` so that hitting the limit exactly blocks the retry — a 20% budget with five originals allows the first retry (ratio 0/5 = 0) but blocks the second (ratio 1/5 = 0.20, which is not below 0.20).

Create `budget.go`:

```go
// Package budget implements a retry budget: a sliding-window limit on the
// fraction of total traffic that may be retries, which caps retry storms
// during an outage. It is safe for concurrent use.
package budget

import (
	"errors"
	"fmt"
	"sync"
	"time"
)

// ErrBudgetExhausted is returned by Allow when retries exceed the configured
// ratio. Callers check identity with errors.Is.
var ErrBudgetExhausted = errors.New("retry budget exhausted")

// RetryBudget limits the fraction of total traffic that may be retries,
// measured over a sliding time window. It is safe for concurrent use.
type RetryBudget struct {
	mu       sync.Mutex
	window   time.Duration
	maxRatio float64
	entries  []budgetEntry
}

type budgetEntry struct {
	at      time.Time
	isRetry bool
}

// NewRetryBudget creates a RetryBudget.
//   - maxRatio: maximum allowed ratio of retries to originals (e.g. 0.20 = 20%).
//   - window: sliding measurement window.
func NewRetryBudget(maxRatio float64, window time.Duration) *RetryBudget {
	return &RetryBudget{maxRatio: maxRatio, window: window}
}

// Record registers a request. Pass isRetry=false for original requests and
// isRetry=true for retry attempts.
func (b *RetryBudget) Record(isRetry bool) {
	b.mu.Lock()
	defer b.mu.Unlock()
	b.pruneLocked()
	b.entries = append(b.entries, budgetEntry{at: time.Now(), isRetry: isRetry})
}

// Allow returns nil if a retry is within budget, or a wrapped ErrBudgetExhausted.
// Call Allow before issuing a retry; call Record(true) after Allow returns nil.
func (b *RetryBudget) Allow() error {
	b.mu.Lock()
	defer b.mu.Unlock()
	b.pruneLocked()

	var originals, retries int
	for _, e := range b.entries {
		if e.isRetry {
			retries++
		} else {
			originals++
		}
	}
	if originals == 0 {
		return nil
	}
	ratio := float64(retries) / float64(originals)
	if ratio >= b.maxRatio {
		return fmt.Errorf("%w: ratio %.2f >= limit %.2f", ErrBudgetExhausted, ratio, b.maxRatio)
	}
	return nil
}

// pruneLocked discards entries older than the sliding window.
// Caller must hold b.mu.
func (b *RetryBudget) pruneLocked() {
	cutoff := time.Now().Add(-b.window)
	i := 0
	for i < len(b.entries) && b.entries[i].at.Before(cutoff) {
		i++
	}
	b.entries = b.entries[i:]
}
```

The protocol is precise: call `Allow` before issuing a retry, and only if it returns nil call `Record(true)` for that retry. This two-step shape lets the caller back out — if `Allow` says no, no retry entry is recorded and the ratio is not perturbed. Originals are recorded unconditionally with `Record(false)` because every real request counts toward the denominator whether or not it is ever retried.

### The runnable demo

The demo seeds five originals, shows that the first retry is allowed (ratio 0/5), records that retry to push the ratio to 1/5 = 0.20, and shows the next retry is now blocked because 0.20 is not below the 0.20 limit. A fresh, empty budget then demonstrates the no-originals allow.

Create `cmd/demo/main.go`:

```go
package main

import (
	"fmt"
	"time"

	"example.com/retry-budget"
)

func main() {
	b := budget.NewRetryBudget(0.20, time.Minute)
	for i := 0; i < 5; i++ {
		b.Record(false) // five original requests
	}

	fmt.Println("5 originals, allow first retry:", b.Allow() == nil)
	b.Record(true) // ratio is now 1/5 = 0.20, at the limit

	fmt.Println("after 1 retry (ratio 0.20), allow next:", b.Allow() == nil)

	empty := budget.NewRetryBudget(0.20, time.Minute)
	fmt.Println("no originals, allow:", empty.Allow() == nil)
}
```

Run it:

```bash
go run ./cmd/demo
```

Expected output:

```
5 originals, allow first retry: true
after 1 retry (ratio 0.20), allow next: false
no originals, allow: true
```

### Tests

`TestRetryBudgetAllowsWithinRatio` seeds originals and checks the first retry is admitted. `TestRetryBudgetExhaustedAtLimit` records that retry and asserts the next `Allow` returns `ErrBudgetExhausted`. `TestRetryBudgetNoOriginals` pins the low-traffic allow. `TestRetryBudgetWindowPrunes` uses a tiny window and a sleep to prove that entries age out, so an old burst of retries does not block retries forever.

Create `budget_test.go`:

```go
package budget

import (
	"errors"
	"testing"
	"time"
)

func TestRetryBudgetAllowsWithinRatio(t *testing.T) {
	t.Parallel()

	// 5 originals, maxRatio=0.20 -> the first retry is within budget.
	b := NewRetryBudget(0.20, time.Minute)
	for i := 0; i < 5; i++ {
		b.Record(false)
	}
	if err := b.Allow(); err != nil {
		t.Fatalf("first retry within budget: %v", err)
	}
}

func TestRetryBudgetExhaustedAtLimit(t *testing.T) {
	t.Parallel()

	b := NewRetryBudget(0.20, time.Minute)
	for i := 0; i < 5; i++ {
		b.Record(false)
	}
	if err := b.Allow(); err != nil { // first retry admitted
		t.Fatalf("first retry should be allowed: %v", err)
	}
	b.Record(true) // ratio becomes 1/5 = 0.20
	if err := b.Allow(); !errors.Is(err, ErrBudgetExhausted) {
		t.Fatalf("second retry should be blocked: %v", err)
	}
}

func TestRetryBudgetNoOriginals(t *testing.T) {
	t.Parallel()

	b := NewRetryBudget(0.20, time.Minute)
	// No originals recorded: budget returns nil (no basis for a ratio).
	if err := b.Allow(); err != nil {
		t.Fatalf("Allow with no originals = %v, want nil", err)
	}
}

func TestRetryBudgetWindowPrunes(t *testing.T) {
	t.Parallel()

	b := NewRetryBudget(0.20, 20*time.Millisecond)
	for i := 0; i < 5; i++ {
		b.Record(false)
	}
	b.Record(true) // ratio 1/5 = 0.20 -> exhausted now
	if err := b.Allow(); !errors.Is(err, ErrBudgetExhausted) {
		t.Fatalf("expected exhaustion before window expiry: %v", err)
	}

	time.Sleep(40 * time.Millisecond) // all entries age out of the window
	if err := b.Allow(); err != nil {
		t.Fatalf("after window expiry, Allow = %v, want nil (entries pruned)", err)
	}
}
```

## Review

The budget is correct when the ratio is computed over a window, not per request, and when the empty-window case allows rather than blocks. The most consequential mistake is treating the budget as per-request state — resetting after each call — which makes a 20% budget behave as "one retry per request always", exactly the no-budget behaviour the gate is meant to prevent during a broad outage. The second mistake is using `>` instead of `>=` in the comparison, which lets the ratio sit one retry over the intended cap. The third is forgetting to prune inside the lock on every `Record` and `Allow`; without pruning the window never advances and an old retry burst blocks traffic indefinitely, which `TestRetryBudgetWindowPrunes` catches. Running under `go test -race` confirms `Record` and `Allow` never touch `entries` without holding `mu`.

## Resources

- [Google SRE Book: Handling Overload](https://sre.google/sre-book/handling-overload/) — client-side throttling and retry budgets; the source of the "limit retries as a fraction of requests" heuristic.
- [Envoy: retry budgets (circuit breaking)](https://www.envoyproxy.io/docs/envoy/latest/intro/arch_overview/upstream/circuit_breaking) — how a production proxy expresses retries as a budget rather than a fixed count.
- [sync.Mutex](https://pkg.go.dev/sync#Mutex) — the lock guarding the entries slice.

---

Back to [02-retry-backoff.md](02-retry-backoff.md) | Next: [04-route-timeout.md](04-route-timeout.md)
