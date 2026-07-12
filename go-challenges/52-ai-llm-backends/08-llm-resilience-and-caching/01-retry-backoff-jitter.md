# Exercise 1: Retry Policy with Exponential Backoff and Full Jitter

The first layer of LLM resilience is a retry policy that knows *which* errors are
worth retrying, waits a decorrelated amount of time between attempts, and never
overshoots the caller's deadline. This exercise builds that policy as
provider-agnostic middleware over a `Completer` interface, so every behavior is
tested offline with a scripted fake, a seeded RNG, and a real context deadline.

This module is fully self-contained. It begins with its own `go mod init`, defines
the `Completer` seam and every helper it needs, and ships its own demo and tests.
Nothing here imports any other exercise. The optional live smoke test against the
real Anthropic SDK lives behind a build tag so the default build stays offline.

## What you'll build

```text
retry/                       independent module: example.com/retry
  go.mod                     go 1.26
  retry.go                   Completer, Request/Response, APIError, Retrier, classification
  anthropic_online.go        //go:build online: adapter over the real Anthropic SDK
  cmd/
    demo/
      main.go                runnable demo: fail twice, succeed on the third attempt
  retry_test.go              classification, retries-until-success, backoff window, budget
```

- Files: `retry.go`, `anthropic_online.go`, `cmd/demo/main.go`, `retry_test.go`.
- Implement: a `Retrier` wrapping a `Completer` that classifies retryable vs terminal errors, computes full-jitter exponential backoff bounded by a per-step cap and a max-attempts cap, prefers a server `Retry-After` (clamped to the remaining deadline), and sleeps interruptibly on `ctx.Done()`.
- Test: a scripted fake proving the retrier stops on the first success and reports the attempt count; terminal errors returning immediately via `errors.As`; the computed backoff falling in `[0, min(cap, base*2^n))` under a seeded RNG; a short deadline proving the loop returns a context error without exceeding the budget.
- Verify: `go test -count=1 -race ./...`

Set up the module:

```bash
go mod edit -go=1.26
# only needed to run the online smoke test:
go get github.com/anthropics/anthropic-sdk-go
```

### The Completer seam

Every resilience wrapper in this chapter composes over one tiny interface. The
business code depends on `Completer`, not on any SDK, so the retry logic is
provider-agnostic and — crucially — testable without a network. A test injects a
`scriptedCompleter` that returns a canned sequence of errors; production injects an
adapter over the real Anthropic or OpenAI SDK. The `Request` and `Response` types
carry only what the resilience layers need to reason about.

### Classifying errors is the whole policy

A retry policy is mostly a function `retryable(error) bool`. The transport-level
truth is carried by an `APIError` that exposes the HTTP `StatusCode` and, when the
server sent one, a `RetryAfter` hint. This mirrors the real SDK, where you unwrap
to `*anthropic.Error` and read its `StatusCode` field. The classification is: retry
408, 409, 429, and any 5xx, plus raw connection failures; never retry 400, 401,
403, 404, or 422, because a bad request or bad key fails identically forever.
`context.Canceled` and `context.DeadlineExceeded` are checked first and are always
terminal — the caller has already given up, so another attempt is pure waste. We
check them with `errors.Is` and unwrap `APIError` with `errors.As`, so a wrapped
error deep in a chain still classifies correctly.

### Full jitter, bounded twice

The backoff window for attempt `n` is `min(cap, base*2^(n-1))`, and the actual
sleep is a uniform random draw in `[0, window)` — full jitter. Without the random
draw, a fleet that all failed at once would retry in lockstep and re-spike the
provider. The window is bounded by the per-step `Cap` so late attempts do not
sleep for minutes; the loop is bounded by `MaxAttempts` so a doomed request fails
fast. The randomness is an injected `*rand.Rand` built from `rand.NewPCG` — never
the package-level `rand.N`, which is unseedable — so the exact sleep is
reproducible under test. The shift `base << (n-1)` is guarded against overflow: a
large `n` falls back to `Cap` rather than wrapping to a negative duration.

### Retry-After wins, but is clamped

When the error carries a `RetryAfter`, the policy uses it instead of the computed
backoff: the server knows better than the formula. But a hostile
`Retry-After: 3600` must not pin the request open for an hour, so it is clamped to
the time remaining before the context deadline. If the context has no deadline the
hint is used as-is.

### Sleeping without ignoring cancellation

`time.Sleep` blocks straight through cancellation. The `sleep` helper instead
selects on `ctx.Done()` versus a `time.NewTimer`, returns `context.Cause(ctx)` the
instant the context is cancelled or its deadline passes, and always `Stop`s the
timer. This is what lets a short deadline cut the retry sequence off mid-sleep
instead of waiting out the full backoff. The `rng` is guarded by a mutex so a
`Retrier` shared across goroutines is race-free.

Create `retry.go`:

```go
package retry

import (
	"context"
	"errors"
	"fmt"
	rand "math/rand/v2"
	"net/http"
	"sync"
	"time"
)

// Message is one turn in a conversation.
type Message struct {
	Role    string
	Content string
}

// Request is a provider-agnostic completion request.
type Request struct {
	Model       string
	System      string
	Messages    []Message
	Temperature float64
	MaxTokens   int
}

// Response is a provider-agnostic completion response.
type Response struct {
	Text  string
	Model string
}

// Completer is the seam every resilience wrapper composes over. Production wires
// an SDK adapter; tests wire a fake.
type Completer interface {
	Complete(ctx context.Context, req Request) (Response, error)
}

// ErrConnection marks a transport failure where no response was received. Such a
// request produced no output and is safe to retry.
var ErrConnection = errors.New("retry: connection error")

// APIError is a provider-agnostic transport error carrying the HTTP status and an
// optional server-supplied Retry-After hint. It mirrors *anthropic.Error.
type APIError struct {
	StatusCode int
	RetryAfter time.Duration // zero when the server gave no hint
	Err        error
}

func (e *APIError) Error() string {
	return fmt.Sprintf("api error: status %d (%s)", e.StatusCode, http.StatusText(e.StatusCode))
}

func (e *APIError) Unwrap() error { return e.Err }

// retryable reports whether err is worth another attempt. Context cancellation is
// terminal; connection errors and transient HTTP statuses are retryable; client
// errors (4xx other than 408/409/429) are not.
func retryable(err error) bool {
	if errors.Is(err, context.Canceled) || errors.Is(err, context.DeadlineExceeded) {
		return false
	}
	if errors.Is(err, ErrConnection) {
		return true
	}
	var apiErr *APIError
	if errors.As(err, &apiErr) {
		switch {
		case apiErr.StatusCode == http.StatusRequestTimeout, // 408
			apiErr.StatusCode == http.StatusConflict,        // 409
			apiErr.StatusCode == http.StatusTooManyRequests, // 429
			apiErr.StatusCode >= 500:
			return true
		default:
			return false
		}
	}
	return false
}

// retryAfter returns a server-supplied Retry-After hint, if any.
func retryAfter(err error) (time.Duration, bool) {
	var apiErr *APIError
	if errors.As(err, &apiErr) && apiErr.RetryAfter > 0 {
		return apiErr.RetryAfter, true
	}
	return 0, false
}

// Policy configures a Retrier.
type Policy struct {
	MaxAttempts int           // total attempts including the first
	Base        time.Duration // base backoff before jitter
	Cap         time.Duration // per-step ceiling on the backoff window
}

// Retrier wraps a Completer with full-jitter exponential backoff.
type Retrier struct {
	next Completer
	pol  Policy

	mu  sync.Mutex
	rng *rand.Rand
}

var _ Completer = (*Retrier)(nil)

// NewRetrier builds a Retrier. The RNG is injected so tests are deterministic;
// production passes rand.New(rand.NewPCG(uint64(time.Now().UnixNano()), 0)).
func NewRetrier(next Completer, pol Policy, rng *rand.Rand) *Retrier {
	return &Retrier{next: next, pol: pol, rng: rng}
}

// Complete calls the wrapped Completer, retrying transient failures with backoff
// until it succeeds, exhausts MaxAttempts, hits a terminal error, or the context
// is cancelled.
func (r *Retrier) Complete(ctx context.Context, req Request) (Response, error) {
	var lastErr error
	for attempt := 1; attempt <= r.pol.MaxAttempts; attempt++ {
		resp, err := r.next.Complete(ctx, req)
		if err == nil {
			return resp, nil
		}
		lastErr = err
		if !retryable(err) {
			return Response{}, err
		}
		if attempt == r.pol.MaxAttempts {
			break
		}
		if err := sleep(ctx, r.backoff(ctx, attempt, err)); err != nil {
			return Response{}, err
		}
	}
	return Response{}, fmt.Errorf("retry: giving up after %d attempts: %w", r.pol.MaxAttempts, lastErr)
}

// window returns min(cap, base*2^(attempt-1)), guarding the shift against overflow.
func (r *Retrier) window(attempt int) time.Duration {
	w := r.pol.Cap
	if shift := attempt - 1; shift >= 0 && shift < 62 {
		if scaled := r.pol.Base << uint(shift); scaled > 0 && scaled < w {
			w = scaled
		}
	}
	return w
}

// backoff prefers a clamped Retry-After hint, else a full-jitter draw in the window.
func (r *Retrier) backoff(ctx context.Context, attempt int, err error) time.Duration {
	if ra, ok := retryAfter(err); ok {
		return clampToDeadline(ctx, ra)
	}
	w := r.window(attempt)
	if w < 1 {
		return 0
	}
	r.mu.Lock()
	d := time.Duration(r.rng.Int64N(int64(w)))
	r.mu.Unlock()
	return d
}

// clampToDeadline caps d at the time remaining before ctx's deadline, so a large
// Retry-After never pins a request open past its budget.
func clampToDeadline(ctx context.Context, d time.Duration) time.Duration {
	dl, ok := ctx.Deadline()
	if !ok {
		return d
	}
	rem := time.Until(dl)
	if rem < 0 {
		return 0
	}
	if d > rem {
		return rem
	}
	return d
}

// sleep waits for d or returns the moment ctx is cancelled, whichever comes first.
func sleep(ctx context.Context, d time.Duration) error {
	if d <= 0 {
		select {
		case <-ctx.Done():
			return context.Cause(ctx)
		default:
			return nil
		}
	}
	t := time.NewTimer(d)
	defer t.Stop()
	select {
	case <-ctx.Done():
		return context.Cause(ctx)
	case <-t.C:
		return nil
	}
}
```

### The optional live adapter

The adapter over the real Anthropic SDK is the one place that touches the network,
so it lives behind `//go:build online` and is excluded from the default build. Note
the single most important line: `option.WithMaxRetries(0)`. The SDK retries twice
by default; leaving that on underneath this `Retrier` would double-retry, turning
one logical call into up to nine attempts. Disabling the SDK's retries makes this
package's loop the single, observable retry path. `classifySDKError` folds the
SDK's `*anthropic.Error` (whose `StatusCode` field carries the HTTP status) into
this package's `APIError`, so the same `retryable` classification applies.

Create `anthropic_online.go`:

```go
//go:build online

package retry

import (
	"context"
	"errors"

	"github.com/anthropics/anthropic-sdk-go"
	"github.com/anthropics/anthropic-sdk-go/option"
)

// anthropicCompleter adapts the official Anthropic SDK to the Completer seam. It
// disables the SDK's built-in retries so this package's Retrier is the sole retry
// layer rather than compounding with the SDK's default two retries.
type anthropicCompleter struct {
	client anthropic.Client
	model  anthropic.Model
}

func newAnthropicCompleter(model anthropic.Model) anthropicCompleter {
	return anthropicCompleter{
		client: anthropic.NewClient(option.WithMaxRetries(0)),
		model:  model,
	}
}

func (a anthropicCompleter) Complete(ctx context.Context, req Request) (Response, error) {
	msgs := make([]anthropic.MessageParam, 0, len(req.Messages))
	for _, m := range req.Messages {
		msgs = append(msgs, anthropic.NewUserMessage(anthropic.NewTextBlock(m.Content)))
	}
	resp, err := a.client.Messages.New(ctx, anthropic.MessageNewParams{
		Model:     a.model,
		MaxTokens: int64(req.MaxTokens),
		Messages:  msgs,
	})
	if err != nil {
		return Response{}, classifySDKError(err)
	}
	var text string
	for _, block := range resp.Content {
		text += block.Text
	}
	return Response{Text: text, Model: string(resp.Model)}, nil
}

// classifySDKError converts an *anthropic.Error into this package's APIError so the
// Retrier's status-code classification applies uniformly.
func classifySDKError(err error) error {
	var apiErr *anthropic.Error
	if errors.As(err, &apiErr) {
		return &APIError{StatusCode: apiErr.StatusCode, Err: err}
	}
	return err
}
```

### The runnable demo

The demo scripts a completer that fails twice with a 503 and then succeeds, runs it
through the retrier with millisecond backoff so the run is fast, and prints how many
attempts it took.

Create `cmd/demo/main.go`:

```go
package main

import (
	"context"
	"fmt"
	rand "math/rand/v2"
	"time"

	"example.com/retry"
)

// flaky fails the first two calls with a 503, then succeeds.
type flaky struct {
	calls int
}

func (f *flaky) Complete(ctx context.Context, req retry.Request) (retry.Response, error) {
	f.calls++
	if f.calls <= 2 {
		return retry.Response{}, &retry.APIError{StatusCode: 503}
	}
	return retry.Response{Text: "pong", Model: req.Model}, nil
}

func main() {
	f := &flaky{}
	r := retry.NewRetrier(f, retry.Policy{
		MaxAttempts: 5,
		Base:        time.Millisecond,
		Cap:         10 * time.Millisecond,
	}, rand.New(rand.NewPCG(1, 2)))

	resp, err := r.Complete(context.Background(), retry.Request{Model: "demo-model"})
	if err != nil {
		fmt.Println("failed:", err)
		return
	}
	fmt.Printf("result: %q after %d attempts\n", resp.Text, f.calls)
}
```

Run it:

```bash
go run ./cmd/demo
```

Expected output:

```
result: "pong" after 3 attempts
```

### Tests

The tests pin every behavior deterministically. `TestClassification` is a table over
`retryable`. `TestRetriesUntilSuccess` scripts errors then a success and asserts the
attempt count. `TestTerminalNoRetry` proves a 400 returns immediately and is still
an `*APIError` via `errors.As`. `TestBackoffWindow` seeds the RNG and asserts every
draw lands in `[0, min(cap, base*2^n))`. `TestBudgetCancellation` gives a short
deadline against an always-failing fake and proves the loop returns
`context.DeadlineExceeded` without exhausting all attempts.

Create `retry_test.go`:

```go
package retry

import (
	"context"
	"errors"
	"fmt"
	rand "math/rand/v2"
	"testing"
	"time"
)

// scriptedCompleter returns errs in order; once exhausted it returns resp.
type scriptedCompleter struct {
	errs  []error
	resp  Response
	calls int
}

func (s *scriptedCompleter) Complete(_ context.Context, _ Request) (Response, error) {
	i := s.calls
	s.calls++
	if i < len(s.errs) {
		return Response{}, s.errs[i]
	}
	return s.resp, nil
}

func newRetrier(next Completer, attempts int) *Retrier {
	return NewRetrier(next, Policy{
		MaxAttempts: attempts,
		Base:        time.Millisecond,
		Cap:         5 * time.Millisecond,
	}, rand.New(rand.NewPCG(1, 2)))
}

func TestClassification(t *testing.T) {
	t.Parallel()
	cases := []struct {
		name string
		err  error
		want bool
	}{
		{"500", &APIError{StatusCode: 500}, true},
		{"503", &APIError{StatusCode: 503}, true},
		{"429", &APIError{StatusCode: 429}, true},
		{"408", &APIError{StatusCode: 408}, true},
		{"409", &APIError{StatusCode: 409}, true},
		{"400", &APIError{StatusCode: 400}, false},
		{"401", &APIError{StatusCode: 401}, false},
		{"404", &APIError{StatusCode: 404}, false},
		{"422", &APIError{StatusCode: 422}, false},
		{"connection", ErrConnection, true},
		{"canceled", context.Canceled, false},
		{"deadline", context.DeadlineExceeded, false},
		{"wrapped 503", fmt.Errorf("send: %w", &APIError{StatusCode: 503}), true},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			if got := retryable(tc.err); got != tc.want {
				t.Fatalf("retryable(%v) = %v, want %v", tc.err, got, tc.want)
			}
		})
	}
}

func TestRetriesUntilSuccess(t *testing.T) {
	t.Parallel()
	fake := &scriptedCompleter{
		errs: []error{&APIError{StatusCode: 503}, &APIError{StatusCode: 429}},
		resp: Response{Text: "ok"},
	}
	r := newRetrier(fake, 5)

	resp, err := r.Complete(context.Background(), Request{})
	if err != nil {
		t.Fatalf("Complete: %v", err)
	}
	if resp.Text != "ok" {
		t.Fatalf("resp.Text = %q, want ok", resp.Text)
	}
	if fake.calls != 3 {
		t.Fatalf("calls = %d, want 3 (two failures then success)", fake.calls)
	}
}

func TestExhaustsAttempts(t *testing.T) {
	t.Parallel()
	fake := &scriptedCompleter{
		errs: []error{
			&APIError{StatusCode: 503},
			&APIError{StatusCode: 503},
			&APIError{StatusCode: 503},
		},
	}
	r := newRetrier(fake, 3)

	_, err := r.Complete(context.Background(), Request{})
	if err == nil {
		t.Fatal("expected error after exhausting attempts")
	}
	if fake.calls != 3 {
		t.Fatalf("calls = %d, want 3", fake.calls)
	}
	var apiErr *APIError
	if !errors.As(err, &apiErr) || apiErr.StatusCode != 503 {
		t.Fatalf("errors.As did not recover the wrapped 503: %v", err)
	}
}

func TestTerminalNoRetry(t *testing.T) {
	t.Parallel()
	fake := &scriptedCompleter{errs: []error{&APIError{StatusCode: 400}}}
	r := newRetrier(fake, 5)

	_, err := r.Complete(context.Background(), Request{})
	if fake.calls != 1 {
		t.Fatalf("calls = %d, want 1 (400 is terminal)", fake.calls)
	}
	var apiErr *APIError
	if !errors.As(err, &apiErr) || apiErr.StatusCode != 400 {
		t.Fatalf("err = %v, want *APIError 400", err)
	}
}

func TestBackoffWindow(t *testing.T) {
	t.Parallel()
	r := NewRetrier(nil, Policy{
		MaxAttempts: 10,
		Base:        100 * time.Millisecond,
		Cap:         2 * time.Second,
	}, rand.New(rand.NewPCG(7, 11)))

	err := &APIError{StatusCode: 503} // no Retry-After: uses jitter
	for attempt := 1; attempt <= 8; attempt++ {
		want := r.window(attempt)
		for range 50 {
			d := r.backoff(context.Background(), attempt, err)
			if d < 0 || d >= want {
				t.Fatalf("attempt %d: backoff %v out of [0, %v)", attempt, d, want)
			}
		}
	}
}

func TestRetryAfterClamped(t *testing.T) {
	t.Parallel()
	r := newRetrier(nil, 3)
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Millisecond)
	defer cancel()

	// A hostile one-hour Retry-After must be clamped to the remaining budget.
	got := r.backoff(ctx, 1, &APIError{StatusCode: 429, RetryAfter: time.Hour})
	if got > 20*time.Millisecond {
		t.Fatalf("backoff = %v, want clamped to <= 20ms", got)
	}
}

func TestBudgetCancellation(t *testing.T) {
	t.Parallel()
	fake := &scriptedCompleter{errs: make([]error, 100)}
	for i := range fake.errs {
		fake.errs[i] = &APIError{StatusCode: 503}
	}
	r := NewRetrier(fake, Policy{
		MaxAttempts: 100,
		Base:        25 * time.Millisecond,
		Cap:         50 * time.Millisecond,
	}, rand.New(rand.NewPCG(1, 2)))

	ctx, cancel := context.WithTimeout(t.Context(), 40*time.Millisecond)
	defer cancel()

	_, err := r.Complete(ctx, Request{})
	if !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("err = %v, want context.DeadlineExceeded", err)
	}
	if fake.calls >= 100 {
		t.Fatalf("calls = %d, want the budget to cut the loop short", fake.calls)
	}
}

func Example() {
	fake := &scriptedCompleter{
		errs: []error{&APIError{StatusCode: 503}, &APIError{StatusCode: 503}},
		resp: Response{Text: "pong"},
	}
	r := NewRetrier(fake, Policy{
		MaxAttempts: 5,
		Base:        time.Millisecond,
		Cap:         2 * time.Millisecond,
	}, rand.New(rand.NewPCG(1, 2)))

	resp, err := r.Complete(context.Background(), Request{})
	fmt.Println(resp.Text, err, fake.calls)
	// Output: pong <nil> 3
}
```

## Review

The retrier is correct when three properties hold together. Classification is pure:
`retryable` returns true only for connection errors and the transient status set
(408/409/429/5xx), and `TestClassification` pins every case including a wrapped one.
The loop honors both bounds: it stops on the first success (`TestRetriesUntilSuccess`
counts three calls), it stops after `MaxAttempts` with the last error wrapped so
`errors.As` still recovers the status (`TestExhaustsAttempts`), and it returns a
terminal error on the first attempt (`TestTerminalNoRetry`). Backoff stays in the
full-jitter window (`TestBackoffWindow`) and a hostile `Retry-After` is clamped to
the remaining deadline (`TestRetryAfterClamped`).

The mistakes to avoid: retrying a 400/401/422 and burning the whole budget on a
request that can never succeed; using `time.Sleep` so a cancelled request keeps
sleeping — `TestBudgetCancellation` proves the `select`-on-`ctx.Done` path cuts the
loop short and returns `context.DeadlineExceeded`; drawing backoff from the
unseedable package-level `rand` so tests cannot be deterministic; and — the big one
in production — layering this retrier on top of an SDK that already retries. The
online adapter disables the SDK's retries with `option.WithMaxRetries(0)` for
exactly that reason. Run `go test -race` to prove the mutex makes a shared `Retrier`
safe. Offline, the default build excludes `anthropic_online.go`; add
`github.com/anthropics/anthropic-sdk-go` and run `go test -tags online` (with
`ANTHROPIC_API_KEY` set) for the live smoke test.

## Resources

- [`math/rand/v2`](https://pkg.go.dev/math/rand/v2) — `New`, `NewPCG`, and `(*Rand).Int64N` for a seeded, deterministic jitter source.
- [Exponential Backoff And Jitter (AWS Architecture Blog)](https://aws.amazon.com/blogs/architecture/exponential-backoff-and-jitter/) — the study showing full jitter minimizes contention and completion time.
- [anthropic-sdk-go error handling and retries](https://pkg.go.dev/github.com/anthropics/anthropic-sdk-go) — `*anthropic.Error` with `StatusCode`, and `option.WithMaxRetries`.
- [`context` package](https://pkg.go.dev/context) — `context.Cause` and deadline-aware cancellation used by the interruptible sleep.

---

Back to [00-concepts.md](00-concepts.md) | Next: [02-timeout-budgets-and-circuit-breaker.md](02-timeout-budgets-and-circuit-breaker.md)
