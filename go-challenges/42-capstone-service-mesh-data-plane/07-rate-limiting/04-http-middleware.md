# Exercise 4: HTTP Middleware

The middleware is where the engine meets the wire. It derives a client key and a descriptor set from each `*http.Request`, asks the engine for a `Decision`, writes the standard `X-RateLimit-*` headers on every response, and turns a deny into a `429` with `Retry-After`. This module bundles the full engine so it runs standalone.

This module is fully self-contained: its own `go mod init`, both backends, the engine, and the middleware defined inline, its own demo and tests. Nothing here imports another exercise.

## What you'll build

```text
tokenbucket.go        unexported tokenBucket backend (golang.org/x/time/rate)
slidingwindow.go      unexported slidingWindow backend (ring buffer)
ratelimit.go          Rule, Descriptor, Decision, Engine, Check
middleware.go         Middleware: extractors -> headers -> 429
cmd/
  demo/
    main.go           wrap a mux, fire 5 requests, print status + headers
middleware_test.go    200 on allow, 429 on deny, header presence, synctest
```

- Files: `tokenbucket.go`, `slidingwindow.go`, `ratelimit.go`, `middleware.go`, `cmd/demo/main.go`, `middleware_test.go`.
- Implement: `Middleware(eng *Engine, clientID func(*http.Request) string, descs func(*http.Request) []Descriptor, next http.Handler) http.Handler`, plus the engine it drives.
- Test: an allowed request returns 200 with rate-limit headers, a denied request returns 429 with `Retry-After`, and a denied client recovers after a virtual refill.
- Verify: `go test -count=1 -race ./...`

Set up the module. The bundled token bucket pulls in `golang.org/x/time/rate`:

```bash
go mod edit -go=1.26
go get golang.org/x/time@latest
```

### Why extractors are injected, and headers go on every response

The middleware never parses HTTP itself. It takes two functions — `clientID` to derive the per-client key (remote IP, JWT subject, mTLS peer CN) and `descs` to extract the descriptor set (route path, plan header) — so the same engine drives IP limits, identity limits, and tenant limits without coupling to request internals. Both are called on every request.

The order of operations matters. The middleware sets `X-RateLimit-Limit`, `X-RateLimit-Remaining`, and `X-RateLimit-Reset` *before* it decides whether to allow, so the metadata is present on a 200 just as much as on a 429. That is what lets a cooperative client read its remaining budget on a successful response and back off on its own, before it ever trips the limit. Only when the `Decision` denies does the middleware add `Retry-After` (an integer number of seconds, floored at one) and write the `429`; otherwise it calls `next`. Writing headers after `next.ServeHTTP` would be too late — the handler may have already flushed the response — so the headers go on first, unconditionally.

First the bundled engine, then the middleware. Create `tokenbucket.go`:

```go
package ratelimit

import (
	"math"
	"time"

	"golang.org/x/time/rate"
)

// tokenBucket wraps golang.org/x/time/rate.Limiter. The limiter starts full and
// refills at ratePerSec tokens per second. It is safe for concurrent use.
type tokenBucket struct {
	lim      *rate.Limiter
	ratePerS float64
}

func newTokenBucket(ratePerSec float64, burst int) *tokenBucket {
	return &tokenBucket{
		lim:      rate.NewLimiter(rate.Limit(ratePerSec), burst),
		ratePerS: ratePerSec,
	}
}

// TryAllow attempts to consume one token, using ReserveN so it can compute an
// accurate resetAt time for the X-RateLimit-Reset header.
func (tb *tokenBucket) TryAllow(now time.Time) (allowed bool, remaining int, resetAt time.Time) {
	r := tb.lim.ReserveN(now, 1)
	if !r.OK() {
		reset := now.Add(time.Duration(float64(time.Second) / math.Max(tb.ratePerS, 1e-9)))
		return false, 0, reset
	}
	delay := r.DelayFrom(now)
	if delay > 0 {
		r.CancelAt(now)
		rem := int(math.Max(0, tb.lim.TokensAt(now)))
		return false, rem, now.Add(delay)
	}
	rem := int(math.Max(0, tb.lim.TokensAt(now)))
	var reset time.Time
	if tb.ratePerS > 0 {
		reset = now.Add(time.Duration(float64(time.Second) / tb.ratePerS))
	}
	return true, rem, reset
}
```

Create `slidingwindow.go`:

```go
package ratelimit

import (
	"sync"
	"time"
)

// slidingWindow implements an approximate sliding window using a ring buffer of
// fixed-size time buckets, evicting expired buckets on every TryAllow call.
type slidingWindow struct {
	mu      sync.Mutex
	slots   []swSlot
	slotDur time.Duration
	maxReqs int
	window  time.Duration
}

type swSlot struct {
	start time.Time
	count int
}

func newSlidingWindow(maxReqs int, window time.Duration, numSlots int) *slidingWindow {
	if numSlots <= 0 {
		numSlots = 10
	}
	return &slidingWindow{
		slots:   make([]swSlot, numSlots),
		slotDur: window / time.Duration(numSlots),
		maxReqs: maxReqs,
		window:  window,
	}
}

func (sw *slidingWindow) TryAllow(now time.Time) (allowed bool, remaining int, resetAt time.Time) {
	sw.mu.Lock()
	defer sw.mu.Unlock()

	cutoff := now.Add(-sw.window)
	total := 0
	var earliestStart time.Time

	for i := range sw.slots {
		s := &sw.slots[i]
		if s.start.IsZero() || !s.start.After(cutoff) {
			s.start = time.Time{}
			s.count = 0
			continue
		}
		total += s.count
		if earliestStart.IsZero() || s.start.Before(earliestStart) {
			earliestStart = s.start
		}
	}

	if !earliestStart.IsZero() {
		resetAt = earliestStart.Add(sw.window)
	} else {
		resetAt = now.Add(sw.window)
	}

	if total >= sw.maxReqs {
		return false, 0, resetAt
	}

	idx := sw.slotIndex(now)
	cur := &sw.slots[idx]
	slotStart := now.Truncate(sw.slotDur)
	if cur.start != slotStart {
		cur.start = slotStart
		cur.count = 0
	}
	cur.count++
	return true, sw.maxReqs - total - 1, resetAt
}

func (sw *slidingWindow) slotIndex(t time.Time) int {
	ns := sw.slotDur.Nanoseconds()
	if ns <= 0 {
		return 0
	}
	return int((t.UnixNano() / ns) % int64(len(sw.slots)))
}
```

Create `ratelimit.go`:

```go
package ratelimit

import (
	"errors"
	"fmt"
	"sync"
	"time"
)

var (
	ErrEmptyRuleName = errors.New("ratelimit: rule Name must not be empty")
	ErrDuplicateRule = errors.New("ratelimit: duplicate rule name")
)

type clientLimiter interface {
	TryAllow(now time.Time) (allowed bool, remaining int, resetAt time.Time)
}

// Algorithm selects the rate limiting algorithm for a Rule.
type Algorithm string

const (
	AlgoTokenBucket   Algorithm = "token_bucket"
	AlgoSlidingWindow Algorithm = "sliding_window"
)

// Descriptor is a single key-value attribute extracted from a request.
type Descriptor struct {
	Key   string
	Value string
}

// Rule defines a rate limit policy. It fires when all Match descriptors are
// present in a request's descriptor set.
type Rule struct {
	Name   string
	Match  []Descriptor
	Algo   Algorithm
	Rate   float64
	Burst  int
	Window time.Duration
	Slots  int
}

// Decision is the outcome of a check against all matching rules.
type Decision struct {
	Allow      bool
	Limit      float64
	Remaining  int
	ResetAt    time.Time
	RetryAfter time.Duration
	RuleName   string
}

// Config holds Engine-wide options.
type Config struct {
	TTL             time.Duration
	CleanupInterval time.Duration
}

type entry struct {
	mu       sync.Mutex
	lim      clientLimiter
	lastSeen time.Time
}

// Engine evaluates requests against a static rule set and manages per-client
// limiter state.
type Engine struct {
	rules          []Rule
	cfg            Config
	clientLimiters sync.Map
	stop           chan struct{}
	once           sync.Once
}

// New creates an Engine. Call Stop to release the background eviction goroutine.
func New(rules []Rule, cfg Config) (*Engine, error) {
	seen := make(map[string]bool, len(rules))
	for _, r := range rules {
		if r.Name == "" {
			return nil, fmt.Errorf("%w", ErrEmptyRuleName)
		}
		if seen[r.Name] {
			return nil, fmt.Errorf("ratelimit: duplicate rule name %q: %w", r.Name, ErrDuplicateRule)
		}
		seen[r.Name] = true
	}
	if cfg.TTL <= 0 {
		cfg.TTL = 5 * time.Minute
	}
	if cfg.CleanupInterval <= 0 {
		cfg.CleanupInterval = 30 * time.Second
	}
	e := &Engine{rules: rules, cfg: cfg, stop: make(chan struct{})}
	go e.evictLoop()
	return e, nil
}

// Stop shuts down the eviction goroutine. It is safe to call more than once.
func (e *Engine) Stop() {
	e.once.Do(func() { close(e.stop) })
}

// Check evaluates descriptors for clientKey against all matching rules. With no
// match it returns Decision{Allow: true}. All matched limiters consume a unit
// regardless of the verdict (conservative, upstream-protecting).
func (e *Engine) Check(descs []Descriptor, clientKey string) (Decision, error) {
	now := time.Now()
	matched := e.matchRules(descs)
	if len(matched) == 0 {
		return Decision{Allow: true}, nil
	}

	overall := Decision{Allow: true}
	firstAllow := true

	for _, rule := range matched {
		ent := e.getOrCreate(rule, clientKey)
		ent.mu.Lock()
		ent.lastSeen = now
		allowed, remaining, resetAt := ent.lim.TryAllow(now)
		ent.mu.Unlock()

		if !allowed {
			retryAfter := time.Until(resetAt)
			if retryAfter < 0 {
				retryAfter = 0
			}
			if overall.Allow || retryAfter > overall.RetryAfter {
				overall.RetryAfter = retryAfter
				overall.ResetAt = resetAt
				overall.Limit = rule.Rate
				overall.RuleName = rule.Name
				overall.Remaining = 0
			}
			overall.Allow = false
		} else if overall.Allow {
			if firstAllow || remaining < overall.Remaining {
				overall.Remaining = remaining
				overall.ResetAt = resetAt
				overall.Limit = rule.Rate
				overall.RuleName = rule.Name
				firstAllow = false
			}
		}
	}
	return overall, nil
}

// ActiveClients returns the number of per-client limiter entries tracked.
func (e *Engine) ActiveClients() int {
	n := 0
	e.clientLimiters.Range(func(_, _ any) bool {
		n++
		return true
	})
	return n
}

func (e *Engine) matchRules(descs []Descriptor) []Rule {
	descSet := make(map[string]string, len(descs))
	for _, d := range descs {
		descSet[d.Key] = d.Value
	}
	var out []Rule
	for _, r := range e.rules {
		if ruleMatches(r, descSet) {
			out = append(out, r)
		}
	}
	return out
}

func ruleMatches(r Rule, descSet map[string]string) bool {
	for _, d := range r.Match {
		if v, ok := descSet[d.Key]; !ok || v != d.Value {
			return false
		}
	}
	return true
}

func (e *Engine) getOrCreate(r Rule, clientKey string) *entry {
	k := r.Name + "\x00" + clientKey
	if v, ok := e.clientLimiters.Load(k); ok {
		return v.(*entry)
	}
	ent := &entry{lim: newClientLimiter(r), lastSeen: time.Now()}
	actual, _ := e.clientLimiters.LoadOrStore(k, ent)
	return actual.(*entry)
}

func newClientLimiter(r Rule) clientLimiter {
	switch r.Algo {
	case AlgoSlidingWindow:
		slots := r.Slots
		if slots <= 0 {
			slots = 10
		}
		return newSlidingWindow(int(r.Rate), r.Window, slots)
	default:
		burst := r.Burst
		if burst <= 0 {
			burst = int(r.Rate)
		}
		if burst < 1 {
			burst = 1
		}
		return newTokenBucket(r.Rate, burst)
	}
}

func (e *Engine) evictLoop() {
	t := time.NewTicker(e.cfg.CleanupInterval)
	defer t.Stop()
	for {
		select {
		case <-t.C:
			e.evictStale()
		case <-e.stop:
			return
		}
	}
}

func (e *Engine) evictStale() {
	now := time.Now()
	e.clientLimiters.Range(func(k, v any) bool {
		ent := v.(*entry)
		ent.mu.Lock()
		stale := now.Sub(ent.lastSeen) > e.cfg.TTL
		ent.mu.Unlock()
		if stale {
			e.clientLimiters.Delete(k)
		}
		return true
	})
}
```

Now the middleware. Create `middleware.go`:

```go
package ratelimit

import (
	"fmt"
	"net/http"
	"strconv"
)

// Middleware wraps next with rate limit enforcement using eng.
//
// clientID derives a per-client key from the request (remote IP, JWT subject,
// mTLS peer CN). descs extracts the descriptor set for rule matching (route
// path, plan header). Both are called on every request and must be non-nil.
func Middleware(
	eng *Engine,
	clientID func(*http.Request) string,
	descs func(*http.Request) []Descriptor,
	next http.Handler,
) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		d, err := eng.Check(descs(r), clientID(r))
		if err != nil {
			http.Error(w, "rate limiter internal error", http.StatusInternalServerError)
			return
		}

		h := w.Header()
		if d.Limit > 0 {
			h.Set("X-RateLimit-Limit", strconv.FormatFloat(d.Limit, 'f', 0, 64))
		}
		h.Set("X-RateLimit-Remaining", strconv.Itoa(d.Remaining))
		if !d.ResetAt.IsZero() {
			h.Set("X-RateLimit-Reset", strconv.FormatInt(d.ResetAt.Unix(), 10))
		}

		if !d.Allow {
			secs := int64(d.RetryAfter.Seconds())
			if secs < 1 {
				secs = 1
			}
			h.Set("Retry-After", fmt.Sprintf("%d", secs))
			http.Error(w, "429 Too Many Requests", http.StatusTooManyRequests)
			return
		}
		next.ServeHTTP(w, r)
	})
}
```

### The runnable demo

The demo wraps a small mux with two rules that both match `/api/v1/orders` for a free-plan client: a token bucket with `burst=3` and a sliding window of `2` requests per second. Most-restrictive wins, so the sliding window denies the third request even though the bucket would still allow it. Five rapid requests run far faster than either limit refills, so the output is deterministic, and the two rules times one client leave two tracked entries.

Create `cmd/demo/main.go`:

```go
package main

import (
	"fmt"
	"net/http"
	"net/http/httptest"
	"time"

	"example.com/http-middleware"
)

func main() {
	rules := []ratelimit.Rule{
		{
			Name:  "global-api",
			Match: []ratelimit.Descriptor{{Key: "route", Value: "/api/v1/orders"}},
			Algo:  ratelimit.AlgoTokenBucket,
			Rate:  5,
			Burst: 3,
		},
		{
			Name: "free-plan-orders",
			Match: []ratelimit.Descriptor{
				{Key: "route", Value: "/api/v1/orders"},
				{Key: "plan", Value: "free"},
			},
			Algo:   ratelimit.AlgoSlidingWindow,
			Rate:   2, // 2 requests per Window
			Window: time.Second,
			Slots:  10,
		},
	}

	eng, err := ratelimit.New(rules, ratelimit.Config{
		TTL:             30 * time.Second,
		CleanupInterval: 10 * time.Second,
	})
	if err != nil {
		panic(err)
	}
	defer eng.Stop()

	clientID := func(r *http.Request) string {
		if id := r.Header.Get("X-Client-ID"); id != "" {
			return id
		}
		return r.RemoteAddr
	}
	extractDescs := func(r *http.Request) []ratelimit.Descriptor {
		ds := []ratelimit.Descriptor{{Key: "route", Value: r.URL.Path}}
		if plan := r.Header.Get("X-Plan"); plan != "" {
			ds = append(ds, ratelimit.Descriptor{Key: "plan", Value: plan})
		}
		return ds
	}

	mux := http.NewServeMux()
	mux.HandleFunc("/api/v1/orders", func(w http.ResponseWriter, _ *http.Request) {
		fmt.Fprintln(w, `{"status":"ok","resource":"orders"}`)
	})
	handler := ratelimit.Middleware(eng, clientID, extractDescs, mux)

	fmt.Println("Simulating 5 rapid requests to /api/v1/orders (global-api burst=3 and free-plan window=2/s, both fire):")
	for i := range 5 {
		req := httptest.NewRequest("GET", "/api/v1/orders", nil)
		req.Header.Set("X-Client-ID", "demo-client")
		req.Header.Set("X-Plan", "free")
		rec := httptest.NewRecorder()
		handler.ServeHTTP(rec, req)
		fmt.Printf("  req %d: status=%d  X-RateLimit-Remaining=%s\n",
			i+1, rec.Code, rec.Header().Get("X-RateLimit-Remaining"))
	}

	fmt.Printf("\nActive client entries tracked: %d\n", eng.ActiveClients())
}
```

Run it:

```bash
go run ./cmd/demo
```

Expected output:

```
Simulating 5 rapid requests to /api/v1/orders (global-api burst=3 and free-plan window=2/s, both fire):
  req 1: status=200  X-RateLimit-Remaining=1
  req 2: status=200  X-RateLimit-Remaining=0
  req 3: status=429  X-RateLimit-Remaining=0
  req 4: status=429  X-RateLimit-Remaining=0
  req 5: status=429  X-RateLimit-Remaining=0

Active client entries tracked: 2
```

### Tests

The first three tests pin the wire contract at machine speed: a lenient rule yields 200, a `burst=1` rule yields 200 then 429 with `Retry-After`, and every allowed response carries all three `X-RateLimit-*` headers. The synctest test then drives a `burst=1` rule through a virtual second to confirm a rate-limited client recovers exactly on refill, stopping the engine and waiting for its goroutine before the bubble closes. A shared `testHandler` builds the middleware with fixed extractors over a trivial 200 handler.

Create `middleware_test.go`:

```go
package ratelimit

import (
	"net/http"
	"net/http/httptest"
	"testing"
	"testing/synctest"
	"time"
)

func TestMiddlewarePasses200OnAllow(t *testing.T) {
	t.Parallel()
	eng, _ := New([]Rule{{
		Name:  "lenient",
		Match: []Descriptor{{Key: "route", Value: "/"}},
		Algo:  AlgoTokenBucket,
		Rate:  100,
		Burst: 10,
	}}, Config{TTL: time.Minute, CleanupInterval: time.Minute})
	defer eng.Stop()

	h := testHandler(eng)
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest("GET", "/", nil))
	if rec.Code != http.StatusOK {
		t.Fatalf("got %d, want 200", rec.Code)
	}
}

func TestMiddlewareReturns429WhenDenied(t *testing.T) {
	t.Parallel()
	eng, _ := New([]Rule{{
		Name:  "strict",
		Match: []Descriptor{{Key: "route", Value: "/"}},
		Algo:  AlgoTokenBucket,
		Rate:  1,
		Burst: 1,
	}}, Config{TTL: time.Minute, CleanupInterval: time.Minute})
	defer eng.Stop()

	h := testHandler(eng)
	req := httptest.NewRequest("GET", "/", nil)

	w1 := httptest.NewRecorder()
	h.ServeHTTP(w1, req)
	if w1.Code != http.StatusOK {
		t.Fatalf("first request: got %d, want 200", w1.Code)
	}

	w2 := httptest.NewRecorder()
	h.ServeHTTP(w2, req)
	if w2.Code != http.StatusTooManyRequests {
		t.Fatalf("second request: got %d, want 429", w2.Code)
	}
	if w2.Header().Get("Retry-After") == "" {
		t.Fatal("Retry-After header must be present on 429")
	}
}

func TestMiddlewareSetsRateLimitHeaders(t *testing.T) {
	t.Parallel()
	eng, _ := New([]Rule{{
		Name:  "hdr",
		Match: []Descriptor{{Key: "route", Value: "/"}},
		Algo:  AlgoTokenBucket,
		Rate:  10,
		Burst: 5,
	}}, Config{TTL: time.Minute, CleanupInterval: time.Minute})
	defer eng.Stop()

	h := testHandler(eng)
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest("GET", "/", nil))

	for _, name := range []string{
		"X-RateLimit-Limit",
		"X-RateLimit-Remaining",
		"X-RateLimit-Reset",
	} {
		if rec.Header().Get(name) == "" {
			t.Errorf("header %s missing on 200 response", name)
		}
	}
}

func TestMiddlewareRecoversAfterVirtualRefill(t *testing.T) {
	t.Parallel()
	synctest.Test(t, func(t *testing.T) {
		eng, _ := New([]Rule{{
			Name:  "strict",
			Match: []Descriptor{{Key: "route", Value: "/"}},
			Algo:  AlgoTokenBucket,
			Rate:  1,
			Burst: 1,
		}}, Config{TTL: time.Minute, CleanupInterval: time.Minute})

		h := testHandler(eng)
		req := httptest.NewRequest("GET", "/", nil)

		w1 := httptest.NewRecorder()
		h.ServeHTTP(w1, req)
		if w1.Code != http.StatusOK {
			t.Fatalf("first request: got %d, want 200", w1.Code)
		}

		w2 := httptest.NewRecorder()
		h.ServeHTTP(w2, req)
		if w2.Code != http.StatusTooManyRequests {
			t.Fatalf("second request: got %d, want 429", w2.Code)
		}

		time.Sleep(time.Second) // virtual: refill one token at 1/sec

		w3 := httptest.NewRecorder()
		h.ServeHTTP(w3, req)
		if w3.Code != http.StatusOK {
			t.Fatalf("after refill: got %d, want 200", w3.Code)
		}

		eng.Stop()
		synctest.Wait()
	})
}

// testHandler builds a Middleware around a trivial 200 handler using fixed
// extractor functions.
func testHandler(eng *Engine) http.Handler {
	return Middleware(
		eng,
		func(r *http.Request) string { return r.RemoteAddr },
		func(r *http.Request) []Descriptor {
			return []Descriptor{{Key: "route", Value: r.URL.Path}}
		},
		http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
			w.WriteHeader(http.StatusOK)
		}),
	)
}
```

## Review

The middleware is correct when the headers are written before the allow/deny branch, so a 200 carries the same `X-RateLimit-*` metadata a 429 does — `TestMiddlewareSetsRateLimitHeaders` guards that on the success path. A deny must produce a `429` and a `Retry-After` of at least one second; `TestMiddlewareReturns429WhenDenied` checks both. The recovery test under `synctest` proves the whole stack — middleware, engine, token bucket — depends only on the virtualized clock, with no real-time leak, and that the engine goroutine is stopped cleanly before the bubble ends. Keep the extractor functions injected rather than parsing the request inside the middleware: that separation is what lets the same code limit by IP, by identity, or by tenant. The `-race` run exercises the engine's `sync.Map` and per-entry mutex under the test handler.

## Resources

- [`net/http` Handler middleware](https://pkg.go.dev/net/http#Handler) — the `http.Handler` wrapping pattern the middleware uses.
- [IETF draft-ietf-httpapi-ratelimit-headers](https://datatracker.ietf.org/doc/draft-ietf-httpapi-ratelimit-headers/) — the standard `RateLimit` response header fields.
- [Envoy rate limit filter](https://www.envoyproxy.io/docs/envoy/latest/configuration/http/http_filters/rate_limit_filter) — a production data-plane filter built on the same descriptor model.

---

Back to [03-rule-engine.md](03-rule-engine.md) | Next: [../08-observability/00-concepts.md](../08-observability/00-concepts.md)
