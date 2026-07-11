# Exercise 3: Resilient Limiter Middleware: Fail-Open, Timeouts, and Tiers

This is the on-the-job exercise. A limiter that only knows how to say yes or no is
not production-ready; what an on-call team relies on is the policy layer around it
— the per-call timeout so a slow Redis cannot amplify latency, the deliberate
fail-open-versus-fail-closed choice for when the store errors, per-tier limits by
caller class, jittered `Retry-After`, and a metric that lights up the moment the
limiter starts degrading. This exercise builds that facade over an abstract
`Limiter`, so it composes with either of the earlier Redis limiters and is tested
entirely with in-process fakes.

This module is fully self-contained and depends only on the standard library, so
it builds, runs, and tests with no external services at all.

## What you'll build

```text
reslimit/                        independent module: example.com/reslimit
  go.mod                         go 1.26 (standard library only)
  policy.go                      Decision, Limiter, Tier, Metrics, Policy, Handler, TieredClassifier
  cmd/
    demo/
      main.go                    a fail-open decision against a down limiter (real output)
  policy_test.go                 fake-limiter tests: fail-open/closed, timeout, tiers, jitter, Example
```

Files: `policy.go`, `cmd/demo/main.go`, `policy_test.go`.
Implement: a `Policy` wrapping any `Limiter` with a per-call `context.WithTimeout`, a `FailOpen` flag, a `Classify` function selecting a per-tier limiter and key, jittered `Retry-After`, and a `Metrics` counter for allowed/denied/degraded.
Test: a limiter error fails open (admit, count degraded) or fails closed (503) per policy; a slow limiter is capped by the timeout; the classifier maps requests to distinct tiers; jitter stays within `[base, base+MaxJitter]`.
Verify: `go test -race ./...`

Set up the module:

```bash
mkdir -p ~/go-exercises/reslimit/cmd/demo
cd ~/go-exercises/reslimit
go mod init example.com/reslimit
go mod edit -go=1.26
```

### Dependency inversion: the Limiter interface

The policy layer must not know or care whether the limiter is the sliding-window
script from Exercise 1, the GCRA middleware from Exercise 2, or a fake in a test.
It depends on one small interface — `Allow(ctx, key) (Decision, error)` — and
every concrete limiter satisfies it. That inversion is what makes the whole
facade testable without Redis: the tests inject a `fakeLimiter` that returns a
canned decision, an error, or a deliberate delay, and drive every branch of the
policy in microseconds.

### The timeout: never let the limiter amplify latency

The single most important line in the facade is the per-call timeout. The limiter
sits on the hot path of every request; if the backing store gets slow, an
unbounded `Allow` makes every request slow with it, and the limiter becomes the
outage. Wrapping each call in `context.WithTimeout` bounds that blast radius: a
struggling store costs at most the timeout budget, after which the call returns
`context.DeadlineExceeded` and the degradation policy takes over. The fake used in
the tests honors the context — it selects on `ctx.Done()` — so a 200 ms fake
delay under a 20 ms timeout returns in ~20 ms, proving the cap holds.

### Fail-open versus fail-closed is a per-deployment decision

When `Allow` errors or times out, the policy must already know what to do, and it
is a genuine trade-off. `FailOpen: true` admits the request: availability is
protected, but the thing behind the limiter is momentarily unprotected.
`FailOpen: false` rejects it with `503 Service Unavailable`: the downstream is
protected, but a Redis blip now degrades your own API. There is no universal right
answer, so the policy makes it an explicit field and — critically — increments a
`degraded` counter on either path, so an operator watching the metric sees the
limiter degrading regardless of which way it fails. A fail-open admit is counted
as degraded, not as a normal allow, so the metric isolates exactly the incident
window.

### Tiers, jitter, and the metric

`Classify` maps a request to a `Tier` — a limiter plus the key to check — so
anonymous, authenticated, and internal callers get different quotas from one
middleware. `TieredClassifier` is a small constructor that looks the limiter up by
tier name. On a denial the policy adds uniform random jitter (drawn with
`math/rand/v2`) in `[0, MaxJitter]` on top of the limiter's `RetryAfter`, so
rejected clients do not all retry at the same instant and re-create the burst.
`Metrics` holds three atomic counters an operator can scrape.

Create `policy.go`:

```go
package reslimit

import (
	"context"
	"math"
	"math/rand/v2"
	"net/http"
	"strconv"
	"sync/atomic"
	"time"
)

// Decision is the outcome of a limiter check.
type Decision struct {
	Allowed    bool
	Remaining  int
	RetryAfter time.Duration
}

// Limiter is the minimal interface the policy layer depends on. Any Redis-backed
// implementation satisfies it, and tests inject fakes.
type Limiter interface {
	Allow(ctx context.Context, key string) (Decision, error)
}

// Tier names the limiter and key that apply to a class of caller.
type Tier struct {
	Name    string
	Limiter Limiter
	Key     string
}

// Metrics counts limiter outcomes so operators can see degradation.
type Metrics struct {
	allowed  atomic.Int64
	denied   atomic.Int64
	degraded atomic.Int64
}

// Allowed reports the count of admitted requests.
func (m *Metrics) Allowed() int64 { return m.allowed.Load() }

// Denied reports the count of rejected-over-limit requests.
func (m *Metrics) Denied() int64 { return m.denied.Load() }

// Degraded reports the count of requests where the limiter errored or timed out.
func (m *Metrics) Degraded() int64 { return m.degraded.Load() }

// Policy composes a Limiter behind a production facade: a per-call timeout, a
// deliberate fail-open/fail-closed choice, per-tier limits, and jittered
// Retry-After.
type Policy struct {
	// Classify maps a request to the tier (limiter + key) that applies to it.
	Classify func(*http.Request) Tier
	// Timeout bounds each limiter call so a slow store cannot amplify latency.
	// Zero disables the per-call timeout.
	Timeout time.Duration
	// FailOpen admits the request when the limiter errors or times out; when
	// false the policy fails closed with 503.
	FailOpen bool
	// MaxJitter is the upper bound of random jitter added to Retry-After.
	MaxJitter time.Duration
	// Metrics, if non-nil, counts allowed/denied/degraded outcomes.
	Metrics *Metrics
}

func (p *Policy) mAllowed() {
	if p.Metrics != nil {
		p.Metrics.allowed.Add(1)
	}
}

func (p *Policy) mDenied() {
	if p.Metrics != nil {
		p.Metrics.denied.Add(1)
	}
}

func (p *Policy) mDegraded() {
	if p.Metrics != nil {
		p.Metrics.degraded.Add(1)
	}
}

// Handler wraps next with the limiter policy.
func (p *Policy) Handler(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		tier := p.Classify(r)

		ctx := r.Context()
		if p.Timeout > 0 {
			var cancel context.CancelFunc
			ctx, cancel = context.WithTimeout(ctx, p.Timeout)
			defer cancel()
		}

		d, err := tier.Limiter.Allow(ctx, tier.Key)
		if err != nil {
			p.mDegraded()
			if p.FailOpen {
				w.Header().Set("RateLimit-Degraded", "open")
				next.ServeHTTP(w, r)
				return
			}
			http.Error(w, "rate limiter unavailable", http.StatusServiceUnavailable)
			return
		}

		w.Header().Set("RateLimit-Remaining", strconv.Itoa(d.Remaining))
		if !d.Allowed {
			p.mDenied()
			w.Header().Set("Retry-After", strconv.Itoa(p.retryAfterSeconds(d.RetryAfter)))
			http.Error(w, "rate limit exceeded", http.StatusTooManyRequests)
			return
		}

		p.mAllowed()
		next.ServeHTTP(w, r)
	})
}

// retryDuration returns base plus uniform jitter in [0, MaxJitter].
func (p *Policy) retryDuration(base time.Duration) time.Duration {
	if base < 0 {
		base = 0
	}
	if p.MaxJitter <= 0 {
		return base
	}
	return base + time.Duration(rand.Int64N(int64(p.MaxJitter)+1))
}

// retryAfterSeconds renders the jittered Retry-After as whole seconds, at least 1.
func (p *Policy) retryAfterSeconds(base time.Duration) int {
	secs := int(math.Ceil(p.retryDuration(base).Seconds()))
	if secs < 1 {
		secs = 1
	}
	return secs
}

// TieredClassifier builds a Classify function: tierOf names the tier and key for a
// request, and the limiter is looked up by tier name.
func TieredClassifier(tierOf func(*http.Request) (name, key string), limiters map[string]Limiter) func(*http.Request) Tier {
	return func(r *http.Request) Tier {
		name, key := tierOf(r)
		return Tier{Name: name, Limiter: limiters[name], Key: key}
	}
}
```

### The runnable demo

The demo builds a policy over a limiter that always errors (a stand-in for Redis
being down) with `FailOpen: true`, then serves one request. The request is
admitted — availability is preserved — and the degraded counter records that the
limiter was not actually consulted, so an operator can see the degradation.

Create `cmd/demo/main.go`:

```go
package main

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"time"

	"example.com/reslimit"
)

type downLimiter struct{}

func (downLimiter) Allow(ctx context.Context, key string) (reslimit.Decision, error) {
	return reslimit.Decision{}, errors.New("redis: connection refused")
}

func main() {
	metrics := &reslimit.Metrics{}
	pol := &reslimit.Policy{
		Classify: func(r *http.Request) reslimit.Tier {
			return reslimit.Tier{Name: "anon", Limiter: downLimiter{}, Key: "anon:203.0.113.7"}
		},
		Timeout:   50 * time.Millisecond,
		FailOpen:  true,
		MaxJitter: 200 * time.Millisecond,
		Metrics:   metrics,
	}

	h := pol.Handler(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
	}))

	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/", nil))

	fmt.Printf("status=%d degraded-header=%q\n", rec.Code, rec.Header().Get("RateLimit-Degraded"))
	fmt.Printf("metrics allowed=%d denied=%d degraded=%d\n",
		metrics.Allowed(), metrics.Denied(), metrics.Degraded())
}
```

Run it:

```bash
go run ./cmd/demo
```

Expected output:

```
status=200 degraded-header="open"
metrics allowed=0 denied=0 degraded=1
```

### Tests

The tests inject a `fakeLimiter` to drive each branch deterministically: an error
proves the fail-open and fail-closed paths, a delay proves the timeout cap, a
canned denial proves the `429` and jittered `Retry-After`, and a table proves the
tier mapping. No network, no clock skew, no flakiness.

Create `policy_test.go`:

```go
package reslimit

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strconv"
	"testing"
	"time"
)

type fakeLimiter struct {
	decision Decision
	err      error
	delay    time.Duration
}

func (f *fakeLimiter) Allow(ctx context.Context, key string) (Decision, error) {
	if f.delay > 0 {
		select {
		case <-time.After(f.delay):
		case <-ctx.Done():
			return Decision{}, ctx.Err()
		}
	}
	return f.decision, f.err
}

func singleTier(l Limiter) func(*http.Request) Tier {
	return func(*http.Request) Tier {
		return Tier{Name: "test", Limiter: l, Key: "k"}
	}
}

func serve(p *Policy) *httptest.ResponseRecorder {
	h := p.Handler(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
	}))
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/", nil))
	return rec
}

func TestFailOpenAdmits(t *testing.T) {
	t.Parallel()
	m := &Metrics{}
	p := &Policy{
		Classify: singleTier(&fakeLimiter{err: errors.New("redis down")}),
		FailOpen: true,
		Metrics:  m,
	}
	rec := serve(p)

	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d; want 200 (fail-open)", rec.Code)
	}
	if got := rec.Header().Get("RateLimit-Degraded"); got != "open" {
		t.Fatalf("RateLimit-Degraded = %q; want %q", got, "open")
	}
	if m.Degraded() != 1 || m.Allowed() != 0 {
		t.Fatalf("metrics degraded=%d allowed=%d; want 1,0", m.Degraded(), m.Allowed())
	}
}

func TestFailClosedRejects(t *testing.T) {
	t.Parallel()
	m := &Metrics{}
	p := &Policy{
		Classify: singleTier(&fakeLimiter{err: errors.New("redis down")}),
		FailOpen: false,
		Metrics:  m,
	}
	rec := serve(p)

	if rec.Code != http.StatusServiceUnavailable {
		t.Fatalf("status = %d; want 503 (fail-closed)", rec.Code)
	}
	if m.Degraded() != 1 {
		t.Fatalf("degraded = %d; want 1", m.Degraded())
	}
}

func TestTimeoutCapsLatency(t *testing.T) {
	t.Parallel()
	m := &Metrics{}
	p := &Policy{
		Classify: singleTier(&fakeLimiter{delay: 200 * time.Millisecond, decision: Decision{Allowed: true}}),
		Timeout:  20 * time.Millisecond,
		FailOpen: true,
		Metrics:  m,
	}

	start := time.Now()
	rec := serve(p)
	elapsed := time.Since(start)

	if elapsed >= 150*time.Millisecond {
		t.Fatalf("elapsed = %v; want the 20ms timeout to cap the 200ms limiter", elapsed)
	}
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d; want 200 (fail-open after timeout)", rec.Code)
	}
	if m.Degraded() != 1 {
		t.Fatalf("degraded = %d; want 1 (deadline exceeded)", m.Degraded())
	}
}

func TestDeniedReturns429WithRetryAfter(t *testing.T) {
	t.Parallel()
	m := &Metrics{}
	p := &Policy{
		Classify:  singleTier(&fakeLimiter{decision: Decision{Allowed: false, RetryAfter: 2 * time.Second}}),
		MaxJitter: time.Second,
		Metrics:   m,
	}
	rec := serve(p)

	if rec.Code != http.StatusTooManyRequests {
		t.Fatalf("status = %d; want 429", rec.Code)
	}
	retry, err := strconv.Atoi(rec.Header().Get("Retry-After"))
	if err != nil {
		t.Fatalf("Retry-After not an integer: %v", err)
	}
	if retry < 2 || retry > 3 {
		t.Fatalf("Retry-After = %d; want in [2,3] (base 2s + up to 1s jitter)", retry)
	}
	if m.Denied() != 1 {
		t.Fatalf("denied = %d; want 1", m.Denied())
	}
}

func TestJitterBounds(t *testing.T) {
	t.Parallel()
	p := &Policy{MaxJitter: 500 * time.Millisecond}
	base := 2 * time.Second
	for range 1000 {
		d := p.retryDuration(base)
		if d < base || d > base+p.MaxJitter {
			t.Fatalf("retryDuration = %v; want in [%v, %v]", d, base, base+p.MaxJitter)
		}
	}
}

func TestTierMapping(t *testing.T) {
	t.Parallel()
	limiters := map[string]Limiter{
		"anon":     &fakeLimiter{},
		"user":     &fakeLimiter{},
		"internal": &fakeLimiter{},
	}
	tierOf := func(r *http.Request) (string, string) {
		switch {
		case r.Header.Get("X-Internal") != "":
			return "internal", "internal:svc"
		case r.Header.Get("Authorization") != "":
			return "user", "user:" + r.Header.Get("Authorization")
		default:
			return "anon", "anon:" + r.RemoteAddr
		}
	}
	classify := TieredClassifier(tierOf, limiters)

	tests := []struct {
		name     string
		headers  map[string]string
		wantTier string
		wantKey  string
	}{
		{"anonymous", nil, "anon", "anon:192.0.2.1:1234"},
		{"authenticated", map[string]string{"Authorization": "tokenX"}, "user", "user:tokenX"},
		{"internal", map[string]string{"X-Internal": "1"}, "internal", "internal:svc"},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			r := httptest.NewRequest(http.MethodGet, "/", nil)
			r.RemoteAddr = "192.0.2.1:1234"
			for k, v := range tc.headers {
				r.Header.Set(k, v)
			}
			tier := classify(r)
			if tier.Name != tc.wantTier {
				t.Fatalf("tier = %q; want %q", tier.Name, tc.wantTier)
			}
			if tier.Key != tc.wantKey {
				t.Fatalf("key = %q; want %q", tier.Key, tc.wantKey)
			}
			if tier.Limiter != limiters[tc.wantTier] {
				t.Fatalf("limiter for %q not wired to the tier's limiter", tc.wantTier)
			}
		})
	}
}

func Example() {
	down := &fakeLimiter{err: errors.New("redis: connection refused")}
	m := &Metrics{}
	p := &Policy{
		Classify: singleTier(down),
		Timeout:  50 * time.Millisecond,
		FailOpen: true,
		Metrics:  m,
	}
	rec := serve(p)
	fmt.Println("status:", rec.Code)
	fmt.Println("degraded:", m.Degraded())
	// Output:
	// status: 200
	// degraded: 1
}
```

## Review

The facade is correct when a struggling limiter can never do more than the policy
allows. The timeout test is the load-bearing one: a limiter that sleeps 200 ms
under a 20 ms budget returns in about 20 ms, so a slow store adds the timeout and
nothing more to request latency. The fail-open and fail-closed tests prove the two
degradation paths and that both increment the `degraded` counter, which is what
makes the choice observable — a limiter silently failing during an incident is the
worst outcome, so the metric is not optional. The jitter test proves rejected
clients are spread over `[base, base+MaxJitter]` rather than retrying in lockstep.

The mistakes to avoid: do not omit the timeout (an unbounded `Allow` turns a Redis
blip into site-wide latency); do not pick fail-open or fail-closed by reflex
(decide per endpoint and count the degradation either way); and do not return a
fixed `Retry-After` (the synchronized retry storm it causes is the exact thing the
limiter exists to prevent). Because the policy depends only on the `Limiter`
interface, swap in the Redis limiter from Exercise 1 or 2 in production and keep
these fakes in the test. Confirm with `go test -race ./...`.

## Resources

- [`context.WithTimeout`](https://pkg.go.dev/context#WithTimeout) — bounding a call so a slow dependency cannot amplify latency.
- [`math/rand/v2`](https://pkg.go.dev/math/rand/v2#Int64N) — `Int64N` for uniform jitter without a shared, locked global source.
- [Google SRE Book: Handling Overload](https://sre.google/sre-book/handling-overload/) — graceful degradation, and why a limiter must fail deliberately.
- [`net/http` server](https://pkg.go.dev/net/http#Handler) — middleware over `http.Handler` and the `429`/`503` status codes.

---

Prev: [02-token-bucket-gcra-redis-rate.md](02-token-bucket-gcra-redis-rate.md) | Back to [00-concepts.md](00-concepts.md) | Next: [../../go.md](../../go.md)
