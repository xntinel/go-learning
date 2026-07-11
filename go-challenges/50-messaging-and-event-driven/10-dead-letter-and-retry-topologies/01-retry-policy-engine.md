# Exercise 1: A Retry Policy Engine — Backoff, Jitter, and Error Classification

Before you touch any broker, build the part that is identical across all of
them: the pure decision engine that answers "retry or not, and when". This
exercise builds a capped, jittered exponential backoff schedule, an error
classifier that sorts failures into retryable / terminal / rate-limited, and a
token-bucket retry budget — all deterministic and fully unit-testable, so the
two integration exercises can consume the same reasoning without re-deriving it.

This module is fully self-contained. It begins with its own `go mod init`, uses
only the standard library, and ships its own demo and tests. Nothing here
imports another exercise.

## What you'll build

```text
retrypolicy/                 independent module: example.com/retrypolicy
  go.mod                     go 1.26
  policy.go                  Backoff (capped+jitter), Classify, Budget, Engine (Plan/Do)
  cmd/
    demo/
      main.go                runnable demo: a jittered ladder, classification, a retry loop
  policy_test.go             table-driven schedule/classifier/budget/plan tests + Examples
```

- Files: `policy.go`, `cmd/demo/main.go`, `policy_test.go`.
- Implement: `Backoff.Delay(attempt)` (exponential, capped, full/equal jitter from an injected `*rand.Rand`), `Classify(err)` and `ClassifyHTTPStatus(code, retryAfter)`, a token-bucket `Budget`, and an `Engine` whose `Plan` decides retry/stop with a reason and whose `Do` runs an operation under the policy.
- Test: table-driven schedule caps and jitter bounds with a seeded source, classifier mapping via `errors.Is`/`errors.As`, budget refill, every `Plan` stop-reason, and `Do` behavior; `Example` functions with `// Output:`.
- Verify: `go test -count=1 -race ./...`

Set up the module:

```bash
mkdir -p ~/go-exercises/retrypolicy/cmd/demo
cd ~/go-exercises/retrypolicy
go mod init example.com/retrypolicy
go mod edit -go=1.26
```

### Why the engine is pure, and why the clock and rand are injected

The retry engine makes decisions; it does no I/O. That is deliberate. Every
broker's retry glue calls the same three questions — is this error worth
retrying, how long until the next attempt, and have we spent enough effort to
give up — and those questions have nothing to do with NATS or Redis. Keeping the
engine broker-agnostic means it is testable in microseconds with a table, and
reusable verbatim behind any broker's ack/nak surface.

Two dependencies would normally make it *un*-testable: randomness and time. The
schedule uses jitter, so its output is random; the budget and elapsed-time
limits depend on the wall clock. The engine injects both. `Backoff` holds a
`*rand.Rand` (from `math/rand/v2`), so a test seeds it with
`rand.New(rand.NewPCG(1, 2))` and gets a reproducible ladder; the `Budget` and
`Engine` hold a `now func() time.Time`, so a test drives time by hand. This is
the clock-and-rand-injection pattern, and it is exactly why every delay in the
tests is asserted as an *exact* value rather than a fuzzy range.

### The backoff schedule: capped growth, then jitter

`interval(attempt)` computes the raw exponential delay for a zero-based retry
index and caps it at `Max`. It grows in a loop rather than computing
`base * pow(mult, n)` in one shot, and caps *inside* the loop, so a large
attempt count can never overflow `time.Duration` (an `int64` of nanoseconds
tops out around 292 years) — the moment the running value reaches `Max` it
returns, and a separate `1e18` guard keeps the float from running away before
the cap check. `Delay(attempt)` then layers jitter on top: `JitterFull` returns
a uniform draw in `[0, capped]`, `JitterEqual` returns `capped/2 + uniform[0,
capped/2]`. The `Int64N` calls are guarded (`capped <= 0`, `half <= 0`) because
`Int64N` panics on a non-positive argument. `JitterNone` returns the raw capped
interval and never touches `Rand`, so it is safe with a nil source.

### Classification: three actions, and a deliberate default

`Classify` sorts an error into `ActionRetry`, `ActionTerminal`, or
`ActionRateLimited`. Order matters and is encoded in the function: a
`*RateLimitError` (which carries a `RetryAfter`) is checked first, then an
explicit `*TerminalError` poison marker, then context cancellation/deadline
(terminal — the caller has given up), and everything else defaults to
`ActionRetry`. That default is a design choice worth stating out loud:
*unknown errors are treated as transient*, because most infrastructure errors
are, and the cost of one wasted retry is lower than the cost of dropping a
message that would have succeeded. The corollary is a hard rule — known poison
*must* be marked with `Terminal(err)` at the point it is detected, or it will be
retried forever. `ClassifyHTTPStatus` is the same logic for HTTP responses: 429
is rate-limited, 501 and all 4xx are terminal, other 5xx are transient.

### The budget and the engine

`Budget` is a token-bucket: retries consume tokens that refill at a fixed rate,
so during a broad outage the bucket empties and `Allow` starts returning false —
most requests then get their one initial attempt and no retries, which is what
stops a retry storm from amplifying the outage. `Engine.Plan` is the whole
decision in one pure function: classify, then stop for `terminal`, then
`max_retries` (the counter check — note `attempt >= MaxRetries`, so
`MaxRetries` retries follow the initial attempt), then `max_elapsed`, then
`budget_exhausted`; otherwise return `Retry: true` with the backoff delay,
raised to `RetryAfter` when the dependency asked for a longer wait. `Do` is the
thin effectful wrapper: it runs the operation, consults `Plan`, and — on a retry
— waits the delay in a `select` against `ctx.Done()` so a cancelled context
aborts the wait immediately rather than sleeping through it.

Create `policy.go`:

```go
package retrypolicy

import (
	"context"
	"errors"
	"fmt"
	"math/rand/v2"
	"net/http"
	"sync"
	"time"
)

// JitterMode selects how randomness is applied on top of the exponential
// interval to break up synchronized retries (a thundering herd).
type JitterMode int

const (
	// JitterNone returns the raw capped interval. Simplest, but re-synchronizes
	// every failed client to retry at the same instant after a dependency blip.
	JitterNone JitterMode = iota
	// JitterFull returns a uniform random delay in [0, interval]. Maximum spread.
	JitterFull
	// JitterEqual returns interval/2 + uniform[0, interval/2]. Keeps a floor so a
	// retry is never near-immediate, while still spreading load.
	JitterEqual
)

func (j JitterMode) String() string {
	switch j {
	case JitterFull:
		return "full"
	case JitterEqual:
		return "equal"
	default:
		return "none"
	}
}

// Backoff computes a capped exponential backoff schedule. It is a pure value:
// Delay is deterministic given a seeded Rand, which is what makes the ladder
// unit-testable without real time.
type Backoff struct {
	Base       time.Duration // interval for retry index 0
	Max        time.Duration // per-attempt cap; <=0 means no cap
	Multiplier float64       // growth factor per attempt; <1 is treated as 1
	Jitter     JitterMode
	Rand       *rand.Rand // required for JitterFull/JitterEqual; unused for JitterNone
}

// interval returns the capped exponential interval for a zero-based retry index,
// before jitter. It grows Base by Multiplier each step, caps at Max, and never
// overflows time.Duration.
func (b Backoff) interval(attempt int) time.Duration {
	if attempt < 0 {
		attempt = 0
	}
	m := b.Multiplier
	if m < 1 {
		m = 1
	}
	d := float64(b.Base)
	for range attempt {
		d *= m
		if b.Max > 0 && d >= float64(b.Max) {
			return b.Max
		}
		if d > 1e18 { // ~292 years; guard the time.Duration int64 range
			d = 1e18
		}
	}
	res := time.Duration(d)
	if b.Max > 0 && res > b.Max {
		return b.Max
	}
	return res
}

// Delay returns the interval for a zero-based retry index with jitter applied.
func (b Backoff) Delay(attempt int) time.Duration {
	capped := b.interval(attempt)
	switch b.Jitter {
	case JitterFull:
		if capped <= 0 {
			return 0
		}
		return time.Duration(b.Rand.Int64N(int64(capped) + 1))
	case JitterEqual:
		half := capped / 2
		if half <= 0 {
			return capped
		}
		return half + time.Duration(b.Rand.Int64N(int64(half)+1))
	default:
		return capped
	}
}

// Action is what a caller should do with a failed message.
type Action int

const (
	// ActionRetry marks a transient failure: try again after a backoff delay.
	ActionRetry Action = iota
	// ActionTerminal marks a poison failure that will never succeed: stop and
	// park it (Term / DLQ), never redeliver.
	ActionTerminal
	// ActionRateLimited marks a failure whose dependency asked us to wait at
	// least RetryAfter before retrying.
	ActionRateLimited
)

func (a Action) String() string {
	switch a {
	case ActionTerminal:
		return "terminal"
	case ActionRateLimited:
		return "rate_limited"
	default:
		return "retry"
	}
}

// TerminalError marks a poison failure that must never be retried. Wrap a cause
// with Terminal to move it out of the retryable class.
type TerminalError struct{ Err error }

func (e *TerminalError) Error() string { return "terminal: " + e.Err.Error() }
func (e *TerminalError) Unwrap() error { return e.Err }

// Terminal wraps err as a poison failure.
func Terminal(err error) error { return &TerminalError{Err: err} }

// RateLimitError signals the dependency asked us to back off for at least
// RetryAfter before the next attempt.
type RateLimitError struct {
	RetryAfter time.Duration
	Err        error
}

func (e *RateLimitError) Error() string {
	return fmt.Sprintf("rate limited (retry after %s): %v", e.RetryAfter, e.Err)
}
func (e *RateLimitError) Unwrap() error { return e.Err }

// Classification is the result of classifying a failure.
type Classification struct {
	Action     Action
	RetryAfter time.Duration // meaningful only for ActionRateLimited
}

// Classify sorts an error into one of the three retry actions. The order matters:
// a rate-limit signal takes precedence, then an explicit terminal marker, then
// context cancellation/deadline (which is terminal: the caller is done), and
// everything else defaults to retryable. Defaulting unknown errors to retryable
// is a deliberate "most infra errors are transient" choice; known poison must be
// marked with Terminal so it is not retried forever.
func Classify(err error) Classification {
	var rl *RateLimitError
	if errors.As(err, &rl) {
		return Classification{Action: ActionRateLimited, RetryAfter: rl.RetryAfter}
	}
	var te *TerminalError
	if errors.As(err, &te) {
		return Classification{Action: ActionTerminal}
	}
	if errors.Is(err, context.Canceled) || errors.Is(err, context.DeadlineExceeded) {
		return Classification{Action: ActionTerminal}
	}
	return Classification{Action: ActionRetry}
}

// ClassifyHTTPStatus maps an HTTP status code from a failed call onto a retry
// action: 429 is rate limited, 501 Not Implemented is terminal, other 5xx are
// transient, and 4xx are terminal (a client error will not fix itself). Pass the
// Retry-After header value for 429 (0 if absent).
func ClassifyHTTPStatus(code int, retryAfter time.Duration) Classification {
	switch {
	case code == http.StatusTooManyRequests:
		return Classification{Action: ActionRateLimited, RetryAfter: retryAfter}
	case code == http.StatusNotImplemented:
		return Classification{Action: ActionTerminal}
	case code >= 500:
		return Classification{Action: ActionRetry}
	case code >= 400:
		return Classification{Action: ActionTerminal}
	default:
		return Classification{Action: ActionRetry}
	}
}

// Budget is a token-bucket retry budget. Retries consume tokens that refill at a
// fixed rate, bounding the fraction of traffic that may be retried during an
// outage so retries cannot amplify a failure into a metastable collapse.
type Budget struct {
	mu       sync.Mutex
	capacity float64
	tokens   float64
	refill   float64 // tokens per second
	last     time.Time
	now      func() time.Time
}

// NewBudget returns a budget starting full at capacity, refilling refillPerSec
// tokens each second. now may be nil to use time.Now; inject it for tests.
func NewBudget(capacity, refillPerSec float64, now func() time.Time) *Budget {
	if now == nil {
		now = time.Now
	}
	return &Budget{
		capacity: capacity,
		tokens:   capacity,
		refill:   refillPerSec,
		last:     now(),
		now:      now,
	}
}

// Allow reports whether a retry may proceed, consuming one token if so. It first
// refills the bucket based on elapsed time since the last call.
func (b *Budget) Allow() bool {
	b.mu.Lock()
	defer b.mu.Unlock()
	now := b.now()
	if elapsed := now.Sub(b.last).Seconds(); elapsed > 0 {
		b.tokens = min(b.capacity, b.tokens+elapsed*b.refill)
		b.last = now
	}
	if b.tokens >= 1 {
		b.tokens--
		return true
	}
	return false
}

// Limits bounds total retry effort for one operation.
type Limits struct {
	MaxRetries int           // retries permitted after the initial attempt
	MaxElapsed time.Duration // total wall-clock budget; <=0 means unbounded
}

// Engine combines a backoff schedule, effort limits, and an optional shared
// retry budget into a single decision function. It is the broker-agnostic core
// the JetStream and Redis exercises consume.
type Engine struct {
	Backoff Backoff
	Limits  Limits
	Budget  *Budget          // shared across operations; nil means unbounded
	Now     func() time.Time // nil means time.Now
}

func (e *Engine) now() time.Time {
	if e.Now != nil {
		return e.Now()
	}
	return time.Now()
}

// Decision is the plan for a single failed attempt. When Retry is false, Reason
// explains why (terminal, max_retries, max_elapsed, budget_exhausted).
type Decision struct {
	Retry  bool
	Delay  time.Duration
	Reason string
}

// Plan decides whether to retry after a failure. attempt is the zero-based retry
// index: 0 means the first execution just failed and we are deciding on retry #1.
// start is when the operation began, used for the MaxElapsed check.
func (e *Engine) Plan(attempt int, start time.Time, err error) Decision {
	c := Classify(err)
	if c.Action == ActionTerminal {
		return Decision{Reason: "terminal"}
	}
	if attempt >= e.Limits.MaxRetries {
		return Decision{Reason: "max_retries"}
	}
	if e.Limits.MaxElapsed > 0 && e.now().Sub(start) >= e.Limits.MaxElapsed {
		return Decision{Reason: "max_elapsed"}
	}
	if e.Budget != nil && !e.Budget.Allow() {
		return Decision{Reason: "budget_exhausted"}
	}
	delay := e.Backoff.Delay(attempt)
	if c.Action == ActionRateLimited && c.RetryAfter > delay {
		delay = c.RetryAfter
	}
	return Decision{Retry: true, Delay: delay, Reason: "retry"}
}

// Do runs op with retries governed by the engine, sleeping the planned delay
// between attempts and honoring ctx cancellation during the wait. On give-up it
// returns the last error wrapped with the abort reason and attempt count.
func (e *Engine) Do(ctx context.Context, op func(context.Context) error) error {
	start := e.now()
	for attempt := 0; ; attempt++ {
		err := op(ctx)
		if err == nil {
			return nil
		}
		d := e.Plan(attempt, start, err)
		if !d.Retry {
			return fmt.Errorf("retry aborted after %d attempt(s) (%s): %w", attempt+1, d.Reason, err)
		}
		timer := time.NewTimer(d.Delay)
		select {
		case <-ctx.Done():
			timer.Stop()
			return fmt.Errorf("retry canceled after %d attempt(s): %w", attempt+1, ctx.Err())
		case <-timer.C:
		}
	}
}
```

### The runnable demo

The demo seeds the ladder with a fixed PCG source so its output is
reproducible, prints a capped full-jitter ladder, shows classification of a few
representative errors, then runs `Do` twice: once on an operation that fails
transiently twice and then succeeds, and once on a poison error that stops after
a single call. The base intervals are tiny so it returns promptly.

Create `cmd/demo/main.go`:

```go
package main

import (
	"context"
	"errors"
	"fmt"
	"math/rand/v2"
	"time"

	"example.com/retrypolicy"
)

func main() {
	// A capped, fully-jittered ladder with a fixed seed so the demo is
	// reproducible. In production the seed comes from the OS.
	b := retrypolicy.Backoff{
		Base:       100 * time.Millisecond,
		Max:        2 * time.Second,
		Multiplier: 2,
		Jitter:     retrypolicy.JitterFull,
		Rand:       rand.New(rand.NewPCG(1, 2)),
	}
	fmt.Println("full-jitter ladder (cap 2s):")
	for attempt := range 7 {
		fmt.Printf("  retry %d: %s\n", attempt, b.Delay(attempt))
	}

	// Classification decides Nak (retry) vs Term (park).
	fmt.Println("classification:")
	fmt.Println("  timeout        ->", retrypolicy.Classify(errors.New("i/o timeout")).Action)
	fmt.Println("  poison payload ->", retrypolicy.Classify(retrypolicy.Terminal(errors.New("schema invalid"))).Action)
	rl := &retrypolicy.RateLimitError{RetryAfter: 3 * time.Second, Err: errors.New("429")}
	fmt.Println("  rate limited   ->", retrypolicy.Classify(rl).Action)
	fmt.Println("  http 503       ->", retrypolicy.ClassifyHTTPStatus(503, 0).Action)
	fmt.Println("  http 400       ->", retrypolicy.ClassifyHTTPStatus(400, 0).Action)

	// Run an operation that fails twice (transient) then succeeds. Tiny base so
	// the demo returns promptly.
	eng := &retrypolicy.Engine{
		Backoff: retrypolicy.Backoff{Base: time.Millisecond, Max: 10 * time.Millisecond, Multiplier: 2, Jitter: retrypolicy.JitterNone},
		Limits:  retrypolicy.Limits{MaxRetries: 5, MaxElapsed: time.Second},
	}
	calls := 0
	err := eng.Do(context.Background(), func(context.Context) error {
		calls++
		if calls < 3 {
			return errors.New("connection reset")
		}
		return nil
	})
	fmt.Printf("Do succeeded after %d calls, err=%v\n", calls, err)

	// A poison error stops immediately.
	calls = 0
	err = eng.Do(context.Background(), func(context.Context) error {
		calls++
		return retrypolicy.Terminal(errors.New("unknown message type"))
	})
	fmt.Printf("poison stopped after %d call(s): %v\n", calls, err)
}
```

Run it:

```bash
go run ./cmd/demo
```

Expected output:

```
full-jitter ladder (cap 2s):
  retry 0: 76.937327ms
  retry 1: 123.287245ms
  retry 2: 313.771201ms
  retry 3: 637.277263ms
  retry 4: 374.842033ms
  retry 5: 82.410514ms
  retry 6: 999.823511ms
classification:
  timeout        -> retry
  poison payload -> terminal
  rate limited   -> rate_limited
  http 503       -> retry
  http 400       -> terminal
Do succeeded after 3 calls, err=<nil>
poison stopped after 1 call(s): retry aborted after 1 attempt(s) (terminal): terminal: unknown message type
```

### Tests

The tests are where the injected clock and seed pay off. `TestIntervalCapAndGrowth`
pins the exact capped ladder and proves a deep attempt count stays capped rather
than overflowing. `TestJitterBounds` draws each jittered delay 200 times per
attempt and asserts it stays within its mode's bounds (and above the equal-jitter
floor). `TestJitterDeterministicWithSeed` proves two backoffs with the same seed
produce identical ladders. `TestClassify` and `TestClassifyHTTPStatus` map every
error and status onto its action with `errors.Is`/`errors.As`. `TestBudgetRefill`
drives the token bucket by hand through a fake clock. `TestPlanReasons` covers
every stop-reason, and the `TestDo*` tests cover success-after-retries,
stop-on-terminal (asserting the sentinel is wrapped), max-retries exhaustion, and
context cancellation during the wait. Two `Example`s lock the exact jittered
ladder and the classification output.

Create `policy_test.go`:

```go
package retrypolicy

import (
	"context"
	"errors"
	"fmt"
	"math/rand/v2"
	"testing"
	"time"
)

func TestIntervalCapAndGrowth(t *testing.T) {
	t.Parallel()
	b := Backoff{Base: 100 * time.Millisecond, Max: time.Second, Multiplier: 2, Jitter: JitterNone}
	tests := []struct {
		attempt int
		want    time.Duration
	}{
		{0, 100 * time.Millisecond},
		{1, 200 * time.Millisecond},
		{2, 400 * time.Millisecond},
		{3, 800 * time.Millisecond},
		{4, time.Second},  // 1600ms capped to 1s
		{50, time.Second}, // deep attempts stay capped, never overflow
	}
	for _, tc := range tests {
		if got := b.Delay(tc.attempt); got != tc.want {
			t.Errorf("Delay(%d) = %s, want %s", tc.attempt, got, tc.want)
		}
	}
}

func TestJitterBounds(t *testing.T) {
	t.Parallel()
	// A seeded source makes the ladder reproducible; here we assert the property
	// that jittered delays stay within their mode's bounds for many draws.
	for _, mode := range []JitterMode{JitterFull, JitterEqual} {
		b := Backoff{Base: 100 * time.Millisecond, Max: time.Second, Multiplier: 2, Jitter: mode, Rand: rand.New(rand.NewPCG(1, 2))}
		for attempt := range 8 {
			capped := b.interval(attempt)
			for range 200 {
				d := b.Delay(attempt)
				if d < 0 || d > capped {
					t.Fatalf("%s Delay(%d) = %s out of [0,%s]", mode, attempt, d, capped)
				}
				if mode == JitterEqual && capped/2 > 0 && d < capped/2 {
					t.Fatalf("equal-jitter Delay(%d) = %s below floor %s", attempt, d, capped/2)
				}
			}
		}
	}
}

func TestJitterDeterministicWithSeed(t *testing.T) {
	t.Parallel()
	mk := func() Backoff {
		return Backoff{Base: 100 * time.Millisecond, Max: 2 * time.Second, Multiplier: 2, Jitter: JitterFull, Rand: rand.New(rand.NewPCG(42, 7))}
	}
	a, b := mk(), mk()
	for attempt := range 10 {
		if a.Delay(attempt) != b.Delay(attempt) {
			t.Fatalf("same seed produced different ladders at attempt %d", attempt)
		}
	}
}

func TestClassify(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name       string
		err        error
		wantAction Action
		wantAfter  time.Duration
	}{
		{"nil is retry-class default", errors.New("boom"), ActionRetry, 0},
		{"terminal marker", Terminal(errors.New("bad schema")), ActionTerminal, 0},
		{"wrapped terminal", fmt.Errorf("handler: %w", Terminal(errors.New("bad"))), ActionTerminal, 0},
		{"rate limited", &RateLimitError{RetryAfter: 5 * time.Second, Err: errors.New("429")}, ActionRateLimited, 5 * time.Second},
		{"wrapped rate limited", fmt.Errorf("call: %w", &RateLimitError{RetryAfter: time.Second}), ActionRateLimited, time.Second},
		{"context canceled is terminal", context.Canceled, ActionTerminal, 0},
		{"deadline exceeded is terminal", context.DeadlineExceeded, ActionTerminal, 0},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			c := Classify(tc.err)
			if c.Action != tc.wantAction {
				t.Errorf("Classify action = %s, want %s", c.Action, tc.wantAction)
			}
			if c.RetryAfter != tc.wantAfter {
				t.Errorf("Classify retryAfter = %s, want %s", c.RetryAfter, tc.wantAfter)
			}
		})
	}
}

func TestClassifyHTTPStatus(t *testing.T) {
	t.Parallel()
	tests := []struct {
		code int
		want Action
	}{
		{429, ActionRateLimited},
		{500, ActionRetry},
		{503, ActionRetry},
		{501, ActionTerminal},
		{400, ActionTerminal},
		{404, ActionTerminal},
		{200, ActionRetry}, // callers pass only failures; a non-4xx/5xx defaults to retry
	}
	for _, tc := range tests {
		if got := ClassifyHTTPStatus(tc.code, 0).Action; got != tc.want {
			t.Errorf("ClassifyHTTPStatus(%d) = %s, want %s", tc.code, got, tc.want)
		}
	}
}

func TestBudgetRefill(t *testing.T) {
	t.Parallel()
	now := time.Unix(0, 0)
	clock := func() time.Time { return now }
	// capacity 2, refill 1 token/sec.
	b := NewBudget(2, 1, clock)
	if !b.Allow() || !b.Allow() {
		t.Fatal("first two retries should be allowed from a full bucket")
	}
	if b.Allow() {
		t.Fatal("third retry should be denied: bucket empty")
	}
	now = now.Add(1500 * time.Millisecond) // refill 1.5 tokens
	if !b.Allow() {
		t.Fatal("retry should be allowed after refill")
	}
	if b.Allow() {
		t.Fatal("only one full token had refilled; second must be denied")
	}
}

func TestPlanReasons(t *testing.T) {
	t.Parallel()
	start := time.Unix(1000, 0)
	newEngine := func(mut func(*Engine)) *Engine {
		e := &Engine{
			Backoff: Backoff{Base: 100 * time.Millisecond, Max: time.Second, Multiplier: 2, Jitter: JitterNone},
			Limits:  Limits{MaxRetries: 3, MaxElapsed: time.Minute},
			Now:     func() time.Time { return start },
		}
		if mut != nil {
			mut(e)
		}
		return e
	}

	tests := []struct {
		name       string
		engine     *Engine
		attempt    int
		err        error
		wantRetry  bool
		wantReason string
		wantDelay  time.Duration
	}{
		{"transient retries", newEngine(nil), 0, errors.New("timeout"), true, "retry", 100 * time.Millisecond},
		{"poison stops", newEngine(nil), 0, Terminal(errors.New("bad")), false, "terminal", 0},
		{"max retries", newEngine(nil), 3, errors.New("timeout"), false, "max_retries", 0},
		{
			"max elapsed",
			newEngine(func(e *Engine) { e.Now = func() time.Time { return start.Add(2 * time.Minute) } }),
			0, errors.New("timeout"), false, "max_elapsed", 0,
		},
		{
			"budget exhausted",
			newEngine(func(e *Engine) { e.Budget = NewBudget(0, 0, func() time.Time { return start }) }),
			0, errors.New("timeout"), false, "budget_exhausted", 0,
		},
		{
			"rate-after overrides backoff",
			newEngine(nil),
			0, &RateLimitError{RetryAfter: 5 * time.Second}, true, "retry", 5 * time.Second,
		},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			d := tc.engine.Plan(tc.attempt, start, tc.err)
			if d.Retry != tc.wantRetry {
				t.Fatalf("Retry = %v, want %v (reason %q)", d.Retry, tc.wantRetry, d.Reason)
			}
			if d.Reason != tc.wantReason {
				t.Errorf("Reason = %q, want %q", d.Reason, tc.wantReason)
			}
			if tc.wantRetry && d.Delay != tc.wantDelay {
				t.Errorf("Delay = %s, want %s", d.Delay, tc.wantDelay)
			}
		})
	}
}

func TestDoSucceedsAfterRetries(t *testing.T) {
	t.Parallel()
	e := &Engine{
		Backoff: Backoff{Base: time.Millisecond, Max: 5 * time.Millisecond, Multiplier: 2, Jitter: JitterNone},
		Limits:  Limits{MaxRetries: 5, MaxElapsed: time.Second},
	}
	calls := 0
	err := e.Do(t.Context(), func(context.Context) error {
		calls++
		if calls < 3 {
			return errors.New("transient")
		}
		return nil
	})
	if err != nil {
		t.Fatalf("Do returned %v, want nil", err)
	}
	if calls != 3 {
		t.Fatalf("op called %d times, want 3", calls)
	}
}

func TestDoStopsOnTerminal(t *testing.T) {
	t.Parallel()
	e := &Engine{
		Backoff: Backoff{Base: time.Millisecond, Multiplier: 2, Jitter: JitterNone},
		Limits:  Limits{MaxRetries: 5, MaxElapsed: time.Second},
	}
	sentinel := errors.New("poison")
	calls := 0
	err := e.Do(t.Context(), func(context.Context) error {
		calls++
		return Terminal(sentinel)
	})
	if calls != 1 {
		t.Fatalf("op called %d times, want 1 (terminal must not retry)", calls)
	}
	if !errors.Is(err, sentinel) {
		t.Fatalf("err = %v, want to wrap sentinel", err)
	}
}

func TestDoExhaustsMaxRetries(t *testing.T) {
	t.Parallel()
	e := &Engine{
		Backoff: Backoff{Base: time.Millisecond, Max: 2 * time.Millisecond, Multiplier: 2, Jitter: JitterNone},
		Limits:  Limits{MaxRetries: 2, MaxElapsed: time.Second},
	}
	calls := 0
	err := e.Do(t.Context(), func(context.Context) error {
		calls++
		return errors.New("always fails")
	})
	if calls != 3 { // 1 initial + 2 retries
		t.Fatalf("op called %d times, want 3", calls)
	}
	if err == nil {
		t.Fatal("Do should return the last error after exhausting retries")
	}
}

func TestDoHonorsContextCancel(t *testing.T) {
	t.Parallel()
	e := &Engine{
		Backoff: Backoff{Base: time.Hour, Multiplier: 2, Jitter: JitterNone}, // long delay
		Limits:  Limits{MaxRetries: 5, MaxElapsed: time.Hour},
	}
	ctx, cancel := context.WithCancel(context.Background())
	go func() {
		time.Sleep(5 * time.Millisecond)
		cancel()
	}()
	err := e.Do(ctx, func(context.Context) error { return errors.New("transient") })
	if !errors.Is(err, context.Canceled) {
		t.Fatalf("err = %v, want context.Canceled", err)
	}
}

func ExampleBackoff_Delay() {
	b := Backoff{
		Base:       100 * time.Millisecond,
		Max:        2 * time.Second,
		Multiplier: 2,
		Jitter:     JitterFull,
		Rand:       rand.New(rand.NewPCG(1, 2)),
	}
	for attempt := range 6 {
		fmt.Printf("retry %d: %s\n", attempt, b.Delay(attempt))
	}
	// Output:
	// retry 0: 76.937327ms
	// retry 1: 123.287245ms
	// retry 2: 313.771201ms
	// retry 3: 637.277263ms
	// retry 4: 374.842033ms
	// retry 5: 82.410514ms
}

func ExampleClassify() {
	fmt.Println(Classify(errors.New("i/o timeout")).Action)
	fmt.Println(Classify(Terminal(errors.New("schema invalid"))).Action)
	fmt.Println(Classify(&RateLimitError{RetryAfter: 2 * time.Second}).Action)
	// Output:
	// retry
	// terminal
	// rate_limited
}
```

## Review

The engine is correct when three properties hold. First, the schedule is a pure
function of `(attempt, seed)`: the same seed reproduces the same ladder, every
delay stays within `[0, Max]`, and a deep attempt count stays capped instead of
overflowing — that is what `TestIntervalCapAndGrowth`, `TestJitterBounds`, and
`TestJitterDeterministicWithSeed` prove. Second, classification is total and
ordered: rate-limit beats terminal beats context-cancel beats the retryable
default, and known poison must be wrapped with `Terminal` at the point of
detection or it will be retried forever. Third, `Plan` stops for exactly one
reason at a time and in the right precedence, and `Do` waits in a `select`
against the context so cancellation aborts the sleep rather than blocking through
it.

The mistakes to avoid map onto the concepts. Do not compute `base * 2^attempt`
without a cap — it overflows and produces multi-hour delays; the loop-and-cap in
`interval` is deliberate. Do not use the real global `rand` or `time.Now`
directly in the engine, or the tests cannot assert exact delays; inject the seed
and the clock. Do not confuse `attempt >= MaxRetries` with `>`: `MaxRetries`
counts retries *after* the initial attempt, so `MaxRetries: 2` means three total
calls, which `TestDoExhaustsMaxRetries` pins. Confirm correctness with
`go test -count=1 -race ./...`; the race detector matters because the `Budget`
is designed to be shared across goroutines and guards its tokens with a mutex.

## Resources

- [AWS Builders' Library: Timeouts, retries, and backoff with jitter](https://aws.amazon.com/builders-library/timeouts-retries-and-backoff-with-jitter/) — the canonical treatment of exponential backoff, full/equal jitter, and retry budgets.
- [`math/rand/v2`](https://pkg.go.dev/math/rand/v2) — `rand.New`, `rand.NewPCG`, and `(*rand.Rand).Int64N` used for reproducible jitter.
- [`errors` package](https://pkg.go.dev/errors) — `errors.Is` and `errors.As` for classifying wrapped sentinel and typed errors.

---

Back to [00-concepts.md](00-concepts.md) | Next: [02-jetstream-dlq-topology.md](02-jetstream-dlq-topology.md)
