# Exercise 34: Webhook Batch Delivery With Retry Backoff, Signing, and Deduplication

**Nivel: Intermedio** — validacion rapida (un test corto).

A webhook delivery service batches events before sending them, retries
failed deliveries with exponential backoff, signs payloads so receivers can
verify authenticity, and deduplicates events a flaky upstream might have
sent twice. This module builds that service with functional options,
validating that a batch's wait time can't outlast the retry budget meant to
deliver it, and that a signing algorithm which needs a secret always gets
one.

## What you'll build

```text
webhook/                          independent module: example.com/webhook-batch-delivery-service
  go.mod                          go 1.24
  webhook.go                      Service, Option, New, WithBatchSize, WithBatchTimeout,
                                   WithRetryTimeout, WithMaxRetries, WithBackoffBase,
                                   WithSigning, WithDedupWindow, WithClock,
                                   TakeBatch, BackoffFor, Sign, ShouldDedup
  cmd/
    demo/
      main.go                     batching, backoff, signing, and dedup across a window
  webhook_test.go                  option-validation table, batching, backoff, and dedup tests
```

- Files: `webhook.go`, `cmd/demo/main.go`, `webhook_test.go`.
- Implement: a `Service` built by `New(opts ...Option) (*Service, error)` whose `WithSigning` rejects a non-`"none"` algorithm with an empty secret immediately, and whose `New` rejects a batch timeout that exceeds the retry timeout after every option has run.
- Test: every option-validation case including the exact boundary where batch timeout equals retry timeout, batching that fills exactly and batching that falls short, exponential backoff growth, deterministic and payload-sensitive signing, and deduplication within and after the configured window.
- Verify: `go test -count=1 ./...`

Set up the module:

```bash
go mod edit -go=1.24
```

### Why the batch timeout must fit inside the retry timeout

`WithBatchTimeout` and `WithRetryTimeout` are independent options — a
caller can set either, both, or neither, in any order. If the batch timeout
were allowed to exceed the retry timeout, a batch could spend its entire
wait accumulating events and only *then* start its retry clock, which by
definition would already be too late to finish within the retry budget:
whatever downstream deadline the retry timeout represents would be missed
before delivery even began. Neither option's closure can see the other's
value while it runs, so `New` checks `batchTimeout > retryTimeout` once,
after every option has applied — the constructor-boundary pattern this
chapter uses for every cross-field invariant.

### Two kinds of invalid, caught in two different places

`WithSigning(alg, secret)` is different from the timeout check: it receives
both the algorithm and its secret in the same call, so it can reject an
empty secret for a non-`"none"` algorithm the instant it runs, exactly like
the object storage client's key-length check earlier in this chapter — no
second pass over the finished `Service` is needed, because nothing else
needs to have happened first for this check to be valid.

### Backoff, signing, and dedup are three independent, pure operations

`BackoffFor`, `Sign`, and `ShouldDedup` do not interact with each other at
all — a real delivery loop would call `ShouldDedup` to decide whether to
send at all, `Sign` to attach an authenticity header, and `BackoffFor`
between failed attempts, but nothing about this module forces them into a
particular sequence. `BackoffFor` and `Sign` are pure functions of their
inputs (and, for `Sign`, the configured secret): the same attempt number or
payload always produces the same result, which is what
`TestBackoffForDoublesEachAttempt` and `TestSignIsDeterministic` check.
`ShouldDedup` is the one stateful operation, and it uses an injected clock
for exactly the reason every interval-based check in this chapter does:
`TestShouldDedupWithinAndAfterWindow` needs to cross the window boundary
without a real sleep.

Create `webhook.go`:

```go
package webhook

import (
	"crypto/hmac"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"sync"
	"time"
)

var validSigningAlgorithms = map[string]bool{
	"none":        true,
	"hmac-sha256": true,
}

// Service batches outgoing webhook events, retries deliveries with
// exponential backoff, signs payloads, and deduplicates events seen again
// within a configured window.
type Service struct {
	batchSize     int
	batchTimeout  time.Duration
	retryTimeout  time.Duration
	maxRetries    int
	backoffBase   time.Duration
	signingAlg    string
	signingSecret []byte
	dedupWindow   time.Duration
	now           func() time.Time

	mu   sync.Mutex
	seen map[string]time.Time
}

// Option configures a Service and may reject invalid input.
type Option func(*Service) error

// New builds a Service, seeding a batch size of 10, a 5s batch timeout, a
// 30s retry timeout, 3 max retries, a 1s backoff base, no signing, and a
// one-minute dedup window, then applies opts. It is the single validation
// boundary: the batch timeout must never exceed the retry timeout, or a
// batch could still be waiting to fill after the retry budget for
// delivering it has already run out.
func New(opts ...Option) (*Service, error) {
	s := &Service{
		batchSize:    10,
		batchTimeout: 5 * time.Second,
		retryTimeout: 30 * time.Second,
		maxRetries:   3,
		backoffBase:  time.Second,
		signingAlg:   "none",
		dedupWindow:  time.Minute,
		now:          time.Now,
		seen:         make(map[string]time.Time),
	}
	for _, opt := range opts {
		if err := opt(s); err != nil {
			return nil, err
		}
	}

	if s.batchTimeout > s.retryTimeout {
		return nil, fmt.Errorf("batch timeout %s exceeds retry timeout %s", s.batchTimeout, s.retryTimeout)
	}
	return s, nil
}

// WithBatchSize sets the maximum number of events per delivered batch
// (>= 1).
func WithBatchSize(n int) Option {
	return func(s *Service) error {
		if n < 1 {
			return fmt.Errorf("batch size must be >= 1, got %d", n)
		}
		s.batchSize = n
		return nil
	}
}

// WithBatchTimeout sets the maximum time a partial batch waits to fill
// before it is sent anyway (> 0).
func WithBatchTimeout(d time.Duration) Option {
	return func(s *Service) error {
		if d <= 0 {
			return fmt.Errorf("batch timeout must be positive, got %s", d)
		}
		s.batchTimeout = d
		return nil
	}
}

// WithRetryTimeout sets the total time budget for retrying a batch delivery
// (> 0).
func WithRetryTimeout(d time.Duration) Option {
	return func(s *Service) error {
		if d <= 0 {
			return fmt.Errorf("retry timeout must be positive, got %s", d)
		}
		s.retryTimeout = d
		return nil
	}
}

// WithMaxRetries sets how many delivery attempts BackoffFor will compute a
// delay for (>= 0).
func WithMaxRetries(n int) Option {
	return func(s *Service) error {
		if n < 0 {
			return fmt.Errorf("max retries must not be negative, got %d", n)
		}
		s.maxRetries = n
		return nil
	}
}

// WithBackoffBase sets the base delay exponential backoff multiplies from
// (> 0).
func WithBackoffBase(d time.Duration) Option {
	return func(s *Service) error {
		if d <= 0 {
			return fmt.Errorf("backoff base must be positive, got %s", d)
		}
		s.backoffBase = d
		return nil
	}
}

// WithSigning sets the payload-signing algorithm ("none" or
// "hmac-sha256") and its secret. A secret is required and must be
// non-empty whenever alg is not "none".
func WithSigning(alg string, secret []byte) Option {
	return func(s *Service) error {
		if !validSigningAlgorithms[alg] {
			return fmt.Errorf("unsupported signing algorithm: %q", alg)
		}
		if alg != "none" && len(secret) == 0 {
			return fmt.Errorf("signing algorithm %q requires a non-empty secret", alg)
		}
		s.signingAlg = alg
		s.signingSecret = secret
		return nil
	}
}

// WithDedupWindow sets how long a delivered event ID is remembered to
// suppress redeliveries (>= 0; 0 disables deduplication).
func WithDedupWindow(d time.Duration) Option {
	return func(s *Service) error {
		if d < 0 {
			return fmt.Errorf("dedup window must not be negative, got %s", d)
		}
		s.dedupWindow = d
		return nil
	}
}

// WithClock injects the clock used to time the dedup window.
func WithClock(now func() time.Time) Option {
	return func(s *Service) error {
		if now == nil {
			return fmt.Errorf("clock is nil")
		}
		s.now = now
		return nil
	}
}

// TakeBatch splits pending into a batch of at most the configured batch
// size and the remaining events.
func (s *Service) TakeBatch(pending []string) (batch, remaining []string) {
	if len(pending) <= s.batchSize {
		return pending, nil
	}
	return pending[:s.batchSize], pending[s.batchSize:]
}

// BackoffFor returns the exponential backoff delay before delivery attempt
// n (1-indexed): backoffBase * 2^(n-1).
func (s *Service) BackoffFor(attempt int) time.Duration {
	if attempt < 1 {
		attempt = 1
	}
	return s.backoffBase * time.Duration(1<<uint(attempt-1))
}

// Sign returns the hex-encoded HMAC-SHA256 signature of payload, or an
// empty string if signing is disabled ("none").
func (s *Service) Sign(payload []byte) string {
	if s.signingAlg == "none" {
		return ""
	}
	mac := hmac.New(sha256.New, s.signingSecret)
	mac.Write(payload)
	return hex.EncodeToString(mac.Sum(nil))
}

// ShouldDedup reports whether eventID was already seen within the dedup
// window and records it as seen now either way. A zero dedup window
// disables deduplication entirely.
func (s *Service) ShouldDedup(eventID string) bool {
	if s.dedupWindow == 0 {
		return false
	}

	s.mu.Lock()
	defer s.mu.Unlock()

	now := s.now()
	if last, ok := s.seen[eventID]; ok && now.Sub(last) < s.dedupWindow {
		return true
	}
	s.seen[eventID] = now
	return false
}
```

### The runnable demo

The demo splits 25 pending events into a batch of 10 and 15 remaining,
prints three exponential backoff delays, signs a payload with HMAC-SHA256,
and shows an event deduplicated on its second delivery but accepted again
once the injected clock advances past the dedup window.

Create `cmd/demo/main.go`:

```go
package main

import (
	"fmt"
	"time"

	"example.com/webhook-batch-delivery-service"
)

func main() {
	current := time.Unix(0, 0).UTC()
	clock := func() time.Time { return current }

	svc, err := webhook.New(
		webhook.WithBatchSize(10),
		webhook.WithBatchTimeout(5*time.Second),
		webhook.WithRetryTimeout(30*time.Second),
		webhook.WithBackoffBase(time.Second),
		webhook.WithSigning("hmac-sha256", []byte("shh-its-secret")),
		webhook.WithDedupWindow(time.Minute),
		webhook.WithClock(clock),
	)
	if err != nil {
		panic(err)
	}

	pending := make([]string, 25)
	for i := range pending {
		pending[i] = fmt.Sprintf("evt-%d", i)
	}
	batch, remaining := svc.TakeBatch(pending)
	fmt.Printf("batch size: %d, remaining: %d\n", len(batch), len(remaining))

	for attempt := 1; attempt <= 3; attempt++ {
		fmt.Printf("attempt %d backoff: %s\n", attempt, svc.BackoffFor(attempt))
	}

	sig := svc.Sign([]byte(`{"event":"evt-0"}`))
	fmt.Printf("signature: %s\n", sig)

	fmt.Printf("evt-0 first delivery is duplicate: %t\n", svc.ShouldDedup("evt-0"))
	fmt.Printf("evt-0 second delivery is duplicate: %t\n", svc.ShouldDedup("evt-0"))
	current = current.Add(90 * time.Second)
	fmt.Printf("evt-0 after dedup window elapses is duplicate: %t\n", svc.ShouldDedup("evt-0"))

	_, err = webhook.New(webhook.WithBatchTimeout(60 * time.Second))
	fmt.Printf("batch timeout exceeding retry timeout rejected: %t\n", err != nil)
}
```

Run it:

```bash
go run ./cmd/demo
```

Expected output:

```
batch size: 10, remaining: 15
attempt 1 backoff: 1s
attempt 2 backoff: 2s
attempt 3 backoff: 4s
signature: 07376f763511c740b76ef75a89ee699b9ae0296fe8b0142c79186666a3a7f87b
evt-0 first delivery is duplicate: false
evt-0 second delivery is duplicate: true
evt-0 after dedup window elapses is duplicate: false
batch timeout exceeding retry timeout rejected: true
```

### Tests

`TestNewValidation` tables construction failures, including the exact
boundary where batch timeout equals retry timeout (allowed) versus exceeds
it (rejected), and both signing outcomes. `TestTakeBatchSplitsAtBatchSize`
and `TestTakeBatchReturnsEverythingWhenBelowBatchSize` cover both sides of
the batching boundary. `TestBackoffForDoublesEachAttempt` asserts the exact
exponential sequence. `TestSignReturnsEmptyWhenDisabled` and
`TestSignIsDeterministic` cover signing on and off, and that it is a pure
function of the payload. `TestShouldDedupWithinAndAfterWindow` and
`TestShouldDedupDisabledWithZeroWindow` cover deduplication across the
window boundary and its disable switch.

Create `webhook_test.go`:

```go
package webhook

import (
	"testing"
	"time"
)

func TestNewValidation(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name    string
		opts    []Option
		wantErr bool
	}{
		{name: "defaults only"},
		{name: "invalid batch size", opts: []Option{WithBatchSize(0)}, wantErr: true},
		{name: "invalid batch timeout", opts: []Option{WithBatchTimeout(0)}, wantErr: true},
		{name: "invalid retry timeout", opts: []Option{WithRetryTimeout(0)}, wantErr: true},
		{name: "invalid max retries", opts: []Option{WithMaxRetries(-1)}, wantErr: true},
		{name: "invalid backoff base", opts: []Option{WithBackoffBase(0)}, wantErr: true},
		{name: "invalid dedup window", opts: []Option{WithDedupWindow(-time.Second)}, wantErr: true},
		{name: "nil clock", opts: []Option{WithClock(nil)}, wantErr: true},
		{name: "unsupported signing algorithm", opts: []Option{WithSigning("md5", []byte("x"))}, wantErr: true},
		{
			name:    "hmac-sha256 requires a secret",
			opts:    []Option{WithSigning("hmac-sha256", nil)},
			wantErr: true,
		},
		{
			name: "hmac-sha256 with a secret is allowed",
			opts: []Option{WithSigning("hmac-sha256", []byte("secret"))},
		},
		{name: "none does not require a secret", opts: []Option{WithSigning("none", nil)}},
		{
			name:    "batch timeout exceeds retry timeout",
			opts:    []Option{WithBatchTimeout(60 * time.Second), WithRetryTimeout(30 * time.Second)},
			wantErr: true,
		},
		{
			name: "batch timeout equal to retry timeout is allowed",
			opts: []Option{WithBatchTimeout(30 * time.Second), WithRetryTimeout(30 * time.Second)},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			_, err := New(tt.opts...)
			if tt.wantErr && err == nil {
				t.Fatal("expected error, got nil")
			}
			if !tt.wantErr && err != nil {
				t.Fatalf("unexpected error: %v", err)
			}
		})
	}
}

func TestTakeBatchSplitsAtBatchSize(t *testing.T) {
	t.Parallel()

	svc, err := New(WithBatchSize(10))
	if err != nil {
		t.Fatal(err)
	}

	pending := make([]string, 25)
	for i := range pending {
		pending[i] = "evt"
	}
	batch, remaining := svc.TakeBatch(pending)
	if len(batch) != 10 || len(remaining) != 15 {
		t.Fatalf("TakeBatch() = (%d, %d), want (10, 15)", len(batch), len(remaining))
	}
}

func TestTakeBatchReturnsEverythingWhenBelowBatchSize(t *testing.T) {
	t.Parallel()

	svc, err := New(WithBatchSize(10))
	if err != nil {
		t.Fatal(err)
	}

	pending := []string{"a", "b", "c"}
	batch, remaining := svc.TakeBatch(pending)
	if len(batch) != 3 || remaining != nil {
		t.Fatalf("TakeBatch() = (%d, %v), want (3, nil)", len(batch), remaining)
	}
}

func TestBackoffForDoublesEachAttempt(t *testing.T) {
	t.Parallel()

	svc, err := New(WithBackoffBase(time.Second))
	if err != nil {
		t.Fatal(err)
	}

	want := []time.Duration{time.Second, 2 * time.Second, 4 * time.Second, 8 * time.Second}
	for i, w := range want {
		if got := svc.BackoffFor(i + 1); got != w {
			t.Fatalf("BackoffFor(%d) = %s, want %s", i+1, got, w)
		}
	}
}

func TestSignReturnsEmptyWhenDisabled(t *testing.T) {
	t.Parallel()

	svc, err := New()
	if err != nil {
		t.Fatal(err)
	}
	if got := svc.Sign([]byte("payload")); got != "" {
		t.Fatalf("Sign() = %q, want empty string when signing is disabled", got)
	}
}

func TestSignIsDeterministic(t *testing.T) {
	t.Parallel()

	svc, err := New(WithSigning("hmac-sha256", []byte("secret")))
	if err != nil {
		t.Fatal(err)
	}
	first := svc.Sign([]byte("payload"))
	if first == "" {
		t.Fatal("Sign() returned an empty signature with signing enabled")
	}
	if got := svc.Sign([]byte("payload")); got != first {
		t.Fatalf("Sign() = %q, want %q (same payload must sign identically)", got, first)
	}
	if got := svc.Sign([]byte("different payload")); got == first {
		t.Fatal("different payloads should not produce the same signature")
	}
}

func TestShouldDedupWithinAndAfterWindow(t *testing.T) {
	t.Parallel()

	current := time.Date(2000, 1, 1, 0, 0, 0, 0, time.UTC)
	svc, err := New(
		WithDedupWindow(time.Minute),
		WithClock(func() time.Time { return current }),
	)
	if err != nil {
		t.Fatal(err)
	}

	if svc.ShouldDedup("evt-1") {
		t.Fatal("first delivery should never be a duplicate")
	}
	if !svc.ShouldDedup("evt-1") {
		t.Fatal("second delivery within the window should be a duplicate")
	}

	current = current.Add(90 * time.Second)
	if svc.ShouldDedup("evt-1") {
		t.Fatal("delivery after the dedup window elapses should not be a duplicate")
	}
}

func TestShouldDedupDisabledWithZeroWindow(t *testing.T) {
	t.Parallel()

	svc, err := New(WithDedupWindow(0))
	if err != nil {
		t.Fatal(err)
	}
	if svc.ShouldDedup("evt-1") || svc.ShouldDedup("evt-1") {
		t.Fatal("a zero dedup window should disable deduplication entirely")
	}
}
```

## Review

The delivery service is correct when a batch's maximum wait time can never
exceed the budget meant to retry delivering it, when a signing algorithm
that needs a secret can never be configured without one, and when
batching, backoff, signing, and deduplication remain independent enough
that each can be tested — and reasoned about — on its own. That
independence is deliberate: a real delivery loop composes these four
pieces in whatever order its retry logic needs, and none of them should
have to know about, or coordinate with, the others to work correctly. The
one place two of them do interact — batch timeout against retry timeout —
gets exactly the same constructor-boundary treatment as every other
cross-field rule in this chapter: seed defaults, apply every option,
validate the combination once.

## Resources

- [Dave Cheney: Functional options for friendly APIs](https://dave.cheney.net/2014/10/17/functional-options-for-friendly-apis)
- [crypto/hmac](https://pkg.go.dev/crypto/hmac)
- [Stripe: verifying webhook signatures](https://stripe.com/docs/webhooks/signatures)

---

Back to [00-concepts.md](00-concepts.md) | Prev: [33-api-request-quota-manager.md](33-api-request-quota-manager.md) | Next: [../../05-strings-runes-and-unicode/01-string-basics/00-concepts.md](../../05-strings-runes-and-unicode/01-string-basics/00-concepts.md)
