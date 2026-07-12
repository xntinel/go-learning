# Exercise 3: Descriptor Rule Engine

The engine is where the two algorithms become a policy system. It matches each request against a static rule set on multiple dimensions at once, runs every matching limiter, reports the most restrictive outcome, and bounds its own memory with TTL eviction of idle per-client state. This module bundles its own copies of both backends so it stands alone.

This module is fully self-contained: its own `go mod init`, both algorithm backends and the engine defined inline, its own demo and tests. Nothing here imports another exercise.

## What you'll build

```text
tokenbucket.go        unexported tokenBucket backend (golang.org/x/time/rate)
slidingwindow.go      unexported slidingWindow backend (ring buffer)
ratelimit.go          clientLimiter, Rule, Descriptor, Decision, Engine, Check
cmd/
  demo/
    main.go           drive a burst rule to exhaustion, print the decisions
ratelimit_test.go     matching, most-restrictive, isolation, eviction, synctest
```

- Files: `tokenbucket.go`, `slidingwindow.go`, `ratelimit.go`, `cmd/demo/main.go`, `ratelimit_test.go`.
- Implement: `Engine` with `New(rules []Rule, cfg Config) (*Engine, error)`, `Check(descs []Descriptor, clientKey string) (Decision, error)`, `ActiveClients() int`, and `Stop()`; plus the `Rule`, `Descriptor`, `Decision`, `Config`, and `Algorithm` types.
- Test: unmatched requests pass, the most restrictive rule wins, clients are isolated, duplicate rule names are rejected, active-client count tracks growth, and a token refills over virtual time.
- Verify: `go test -count=1 -race ./...`

Set up the module. The bundled token bucket pulls in `golang.org/x/time/rate`:

```bash
mkdir -p go-solutions/42-capstone-service-mesh-data-plane/07-rate-limiting/03-rule-engine/cmd/demo
cd go-solutions/42-capstone-service-mesh-data-plane/07-rate-limiting/03-rule-engine
go mod edit -go=1.26
go get golang.org/x/time@latest
```

### How the engine decides

Both backends satisfy one private interface — `clientLimiter`, a single `TryAllow(now)` method — so the engine treats a token bucket and a sliding window identically. A `Rule` names a policy, lists the `Descriptor` pairs that must all be present to fire, and selects an algorithm plus its parameters. `Check` extracts the request's descriptors into a map, finds every rule whose match list is a subset, and runs each one.

The matching is exact-string equality on every key-value pair, which is what gives multi-dimensional limiting. A rule matching `{route: /api}` and a second matching `{route: /api, client: x}` both fire on a request carrying both descriptors, and they are evaluated independently against separate per-client limiters.

When more than one rule matches, every matched limiter consumes a unit regardless of the verdict — the conservative path that protects upstreams. The aggregation rule is: `Allow` only if every limiter allowed; on any deny, report the rule with the longest `RetryAfter`; when all allow, report the rule with the lowest remaining budget. So the `Decision` always describes the dimension that is either blocking the request or closest to doing so.

Per-client limiters live in a `sync.Map` keyed by `ruleName + "\x00" + clientKey`, created lazily through `LoadOrStore` so a race between two goroutines that both miss the initial `Load` discards the loser's limiter rather than double-counting. A background `evictLoop` goroutine wakes on `CleanupInterval`, ranges the map, and deletes any entry whose `lastSeen` is older than `TTL`. It is shut down through a `stop` channel closed under a `sync.Once`, so `Stop` is safe to call more than once — always call it when the engine is done to release the goroutine.

First, the two bundled backends. Create `tokenbucket.go`:

```go
package ratelimit

import (
	"math"
	"time"

	"golang.org/x/time/rate"
)

// tokenBucket wraps golang.org/x/time/rate.Limiter. The limiter starts full
// (burst tokens available) and refills at ratePerSec tokens per second. It is
// safe for concurrent use.
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

// TryAllow attempts to consume one token. It uses ReserveN rather than Allow so
// that it can compute an accurate resetAt time for the X-RateLimit-Reset header.
func (tb *tokenBucket) TryAllow(now time.Time) (allowed bool, remaining int, resetAt time.Time) {
	r := tb.lim.ReserveN(now, 1)
	if !r.OK() {
		// n exceeds burst; the request can never be served by this limiter.
		reset := now.Add(time.Duration(float64(time.Second) / math.Max(tb.ratePerS, 1e-9)))
		return false, 0, reset
	}
	delay := r.DelayFrom(now)
	if delay > 0 {
		// Tokens will be available in `delay`; deny now and return the token.
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
// fixed-size time buckets. Expired buckets (older than window) are evicted on
// every TryAllow call; no background goroutine is needed.
type slidingWindow struct {
	mu      sync.Mutex
	slots   []swSlot
	slotDur time.Duration // window / numSlots
	maxReqs int
	window  time.Duration
}

type swSlot struct {
	start time.Time // zero means the slot is empty / evicted
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

// TryAllow counts active requests and either records one more (allowed) or
// rejects (denied). It is internally serialized with sw.mu.
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

Now the engine itself. Create `ratelimit.go`:

```go
package ratelimit

import (
	"errors"
	"fmt"
	"sync"
	"time"
)

// Sentinel errors returned by New. Tests should use errors.Is rather than
// comparing error strings, because New wraps these with additional context.
var (
	ErrEmptyRuleName = errors.New("ratelimit: rule Name must not be empty")
	ErrDuplicateRule = errors.New("ratelimit: duplicate rule name")
)

// clientLimiter is the common interface for both algorithm backends.
// Implementations must be safe for concurrent use (or externally serialized).
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

// Rule defines a rate limit policy. A rule fires when all of its Match
// descriptors are present in a request's descriptor set.
type Rule struct {
	// Name must be unique across all rules in an Engine.
	Name string
	// Match is the set of Descriptor pairs that must ALL be present.
	Match []Descriptor
	// Algo selects the algorithm.
	Algo Algorithm
	// Rate is tokens per second (AlgoTokenBucket) or requests per Window
	// (AlgoSlidingWindow).
	Rate float64
	// Burst is the maximum token accumulation (AlgoTokenBucket only).
	// Defaults to int(Rate) when zero.
	Burst int
	// Window is the counting period (AlgoSlidingWindow only).
	Window time.Duration
	// Slots is the number of ring-buffer buckets (AlgoSlidingWindow only).
	// Defaults to 10 when zero.
	Slots int
}

// Decision is the outcome of a rate limit check against all matching rules.
type Decision struct {
	// Allow is true when every matched rule permits the request.
	Allow bool
	// Limit is the rate ceiling of the most restrictive rule that applied.
	Limit float64
	// Remaining is the remaining budget under the most restrictive rule.
	Remaining int
	// ResetAt is when the most restrictive bucket or window resets.
	ResetAt time.Time
	// RetryAfter is the minimum wait before retrying (non-zero only when Allow
	// is false).
	RetryAfter time.Duration
	// RuleName is the name of the most restrictive rule.
	RuleName string
}

// Config holds Engine-wide options.
type Config struct {
	// TTL is the inactivity period after which a per-client limiter is evicted.
	TTL time.Duration
	// CleanupInterval is how often the eviction goroutine wakes.
	CleanupInterval time.Duration
}

// entry holds a per-client limiter and its last-seen timestamp. The mutex
// serializes TryAllow calls and protects lastSeen.
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
	clientLimiters sync.Map // key: string -> *entry
	stop           chan struct{}
	once           sync.Once
}

// New creates an Engine. Call Stop when the Engine is no longer needed to
// release the background eviction goroutine.
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
	e := &Engine{
		rules: rules,
		cfg:   cfg,
		stop:  make(chan struct{}),
	}
	go e.evictLoop()
	return e, nil
}

// Stop shuts down the eviction goroutine. It is safe to call more than once.
func (e *Engine) Stop() {
	e.once.Do(func() { close(e.stop) })
}

// Check evaluates the given descriptors for the given clientKey against all
// matching rules. When no rule matches, it returns Decision{Allow: true}.
//
// Token consumption is conservative: all matched limiters consume a unit
// regardless of whether the request is ultimately allowed or denied. This makes
// limits slightly tighter when multiple rules match a denied request, which is
// the safer behavior for protecting upstreams.
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
			// Track the rule with the longest retry-after (most restrictive deny).
			if overall.Allow || retryAfter > overall.RetryAfter {
				overall.RetryAfter = retryAfter
				overall.ResetAt = resetAt
				overall.Limit = rule.Rate
				overall.RuleName = rule.Name
				overall.Remaining = 0
			}
			overall.Allow = false
		} else if overall.Allow {
			// Still allowing: track the rule with the lowest remaining budget.
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

// ActiveClients returns the number of per-client limiter entries currently
// tracked across all rules. Useful for monitoring memory growth.
func (e *Engine) ActiveClients() int {
	n := 0
	e.clientLimiters.Range(func(_, _ any) bool {
		n++
		return true
	})
	return n
}

// matchRules returns all rules whose Match descriptors are a subset of descs.
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

// getOrCreate returns the per-client entry for the given rule and client key,
// creating it atomically if it does not yet exist.
func (e *Engine) getOrCreate(r Rule, clientKey string) *entry {
	k := r.Name + "\x00" + clientKey
	if v, ok := e.clientLimiters.Load(k); ok {
		return v.(*entry)
	}
	ent := &entry{
		lim:      newClientLimiter(r),
		lastSeen: time.Now(),
	}
	// LoadOrStore handles the race where two goroutines both miss the Load above;
	// the loser's ent is discarded and GC'd.
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
	default: // AlgoTokenBucket
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

// evictLoop runs in a dedicated goroutine and periodically removes per-client
// entries that have not been seen for longer than cfg.TTL.
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

### The runnable demo

The demo configures a single token-bucket rule with `burst=3`, fires five requests from one client at machine speed (far faster than the 10-tokens-per-second refill), and prints each decision. The burst absorbs three, then the fourth and fifth are denied. One client touching one rule leaves exactly one tracked entry.

Create `cmd/demo/main.go`:

```go
package main

import (
	"fmt"
	"time"

	"example.com/rule-engine"
)

func main() {
	rules := []ratelimit.Rule{{
		Name:  "api-burst",
		Match: []ratelimit.Descriptor{{Key: "svc", Value: "orders"}},
		Algo:  ratelimit.AlgoTokenBucket,
		Rate:  10,
		Burst: 3,
	}}
	eng, err := ratelimit.New(rules, ratelimit.Config{
		TTL:             time.Minute,
		CleanupInterval: time.Minute,
	})
	if err != nil {
		panic(err)
	}
	defer eng.Stop()

	descs := []ratelimit.Descriptor{{Key: "svc", Value: "orders"}}
	fmt.Println("rule api-burst: token bucket, rate=10/s, burst=3")
	for i := 1; i <= 5; i++ {
		d, _ := eng.Check(descs, "c1")
		fmt.Printf("  request %d: allow=%v  remaining=%d  rule=%s\n", i, d.Allow, d.Remaining, d.RuleName)
	}
	fmt.Printf("active clients tracked: %d\n", eng.ActiveClients())
}
```

Run it:

```bash
go run ./cmd/demo
```

Expected output:

```
rule api-burst: token bucket, rate=10/s, burst=3
  request 1: allow=true  remaining=2  rule=api-burst
  request 2: allow=true  remaining=1  rule=api-burst
  request 3: allow=true  remaining=0  rule=api-burst
  request 4: allow=false  remaining=0  rule=api-burst
  request 5: allow=false  remaining=0  rule=api-burst
active clients tracked: 1
```

### Tests

The tests pin the engine's contracts. A request whose descriptors match no rule always passes. When a loose global rule and a tight per-client rule both fire, the tighter one decides and names itself in the `Decision`. Two clients hitting the same rule have independent buckets. Duplicate rule names are rejected at construction, checked with `errors.Is` against the sentinel. The active-client count tracks the number of distinct clients seen. Finally, a `synctest` test exhausts a one-token rule, sleeps one *virtual* second, and confirms the engine — which reads the wall clock internally inside `Check` — refills exactly on schedule; it stops the engine and waits for the eviction goroutine before the bubble closes.

Create `ratelimit_test.go`:

```go
package ratelimit

import (
	"errors"
	"fmt"
	"testing"
	"testing/synctest"
	"time"
)

func TestEngineNoMatchAllows(t *testing.T) {
	t.Parallel()
	eng, err := New([]Rule{{
		Name:  "api",
		Match: []Descriptor{{Key: "route", Value: "/api"}},
		Algo:  AlgoTokenBucket,
		Rate:  10,
		Burst: 5,
	}}, Config{TTL: time.Minute, CleanupInterval: time.Minute})
	if err != nil {
		t.Fatal(err)
	}
	defer eng.Stop()

	d, err := eng.Check([]Descriptor{{Key: "route", Value: "/health"}}, "c1")
	if err != nil {
		t.Fatal(err)
	}
	if !d.Allow {
		t.Fatal("unmatched descriptors should always be allowed")
	}
}

func TestEngineMostRestrictiveRuleApplied(t *testing.T) {
	t.Parallel()
	rules := []Rule{
		{
			Name:  "global",
			Match: []Descriptor{{Key: "route", Value: "/api"}},
			Algo:  AlgoTokenBucket,
			Rate:  100,
			Burst: 10,
		},
		{
			Name:  "per-client",
			Match: []Descriptor{{Key: "route", Value: "/api"}, {Key: "client", Value: "x"}},
			Algo:  AlgoTokenBucket,
			Rate:  1,
			Burst: 1,
		},
	}
	eng, err := New(rules, Config{TTL: time.Minute, CleanupInterval: time.Minute})
	if err != nil {
		t.Fatal(err)
	}
	defer eng.Stop()

	descs := []Descriptor{{Key: "route", Value: "/api"}, {Key: "client", Value: "x"}}
	if d1, _ := eng.Check(descs, "x"); !d1.Allow {
		t.Fatal("first request should be allowed")
	}
	d2, _ := eng.Check(descs, "x")
	if d2.Allow {
		t.Fatal("second request should be denied: per-client burst=1 exhausted")
	}
	if d2.RuleName != "per-client" {
		t.Fatalf("RuleName = %q, want per-client", d2.RuleName)
	}
}

func TestEngineClientIsolation(t *testing.T) {
	t.Parallel()
	eng, err := New([]Rule{{
		Name:  "per-ip",
		Match: []Descriptor{{Key: "svc", Value: "api"}},
		Algo:  AlgoTokenBucket,
		Rate:  1,
		Burst: 1,
	}}, Config{TTL: time.Minute, CleanupInterval: time.Minute})
	if err != nil {
		t.Fatal(err)
	}
	defer eng.Stop()

	descs := []Descriptor{{Key: "svc", Value: "api"}}
	eng.Check(descs, "client-A")
	if dA, _ := eng.Check(descs, "client-A"); dA.Allow {
		t.Fatal("client-A should be rate limited")
	}
	if dB, _ := eng.Check(descs, "client-B"); !dB.Allow {
		t.Fatal("client-B should be allowed: its bucket is independent")
	}
}

func TestEngineDuplicateRuleNameRejected(t *testing.T) {
	t.Parallel()
	_, err := New([]Rule{
		{Name: "x", Match: []Descriptor{{Key: "k", Value: "v"}}, Algo: AlgoTokenBucket, Rate: 1, Burst: 1},
		{Name: "x", Match: []Descriptor{{Key: "k", Value: "v"}}, Algo: AlgoTokenBucket, Rate: 1, Burst: 1},
	}, Config{})
	if err == nil {
		t.Fatal("New should reject duplicate rule names")
	}
	if !errors.Is(err, ErrDuplicateRule) {
		t.Fatalf("expected errors.Is(err, ErrDuplicateRule), got: %v", err)
	}
}

func TestEngineActiveClients(t *testing.T) {
	t.Parallel()
	eng, err := New([]Rule{{
		Name:  "count",
		Match: []Descriptor{{Key: "svc", Value: "api"}},
		Algo:  AlgoTokenBucket,
		Rate:  100,
		Burst: 10,
	}}, Config{TTL: time.Minute, CleanupInterval: time.Minute})
	if err != nil {
		t.Fatal(err)
	}
	defer eng.Stop()

	descs := []Descriptor{{Key: "svc", Value: "api"}}
	const n = 50
	for i := range n {
		eng.Check(descs, fmt.Sprintf("client-%d", i))
	}
	if got := eng.ActiveClients(); got != n {
		t.Fatalf("ActiveClients = %d, want %d", got, n)
	}
}

func TestEngineRefillsOverVirtualTime(t *testing.T) {
	t.Parallel()
	synctest.Test(t, func(t *testing.T) {
		eng, err := New([]Rule{{
			Name:  "tight",
			Match: []Descriptor{{Key: "svc", Value: "api"}},
			Algo:  AlgoTokenBucket,
			Rate:  1,
			Burst: 1,
		}}, Config{TTL: time.Minute, CleanupInterval: time.Minute})
		if err != nil {
			t.Fatal(err)
		}
		descs := []Descriptor{{Key: "svc", Value: "api"}}
		if d, _ := eng.Check(descs, "c1"); !d.Allow {
			t.Fatal("first request should be allowed")
		}
		if d, _ := eng.Check(descs, "c1"); d.Allow {
			t.Fatal("second immediate request should be denied: burst=1")
		}
		time.Sleep(time.Second) // virtual: refill one token at 1/sec
		if d, _ := eng.Check(descs, "c1"); !d.Allow {
			t.Fatal("request after virtual refill should be allowed")
		}
		eng.Stop()
		synctest.Wait()
	})
}

func ExampleEngine_Check() {
	rules := []Rule{{
		Name:  "api-burst",
		Match: []Descriptor{{Key: "svc", Value: "orders"}},
		Algo:  AlgoTokenBucket,
		Rate:  10,
		Burst: 3,
	}}
	eng, _ := New(rules, Config{TTL: time.Minute, CleanupInterval: time.Minute})
	defer eng.Stop()

	descs := []Descriptor{{Key: "svc", Value: "orders"}}
	d1, _ := eng.Check(descs, "c1")
	d2, _ := eng.Check(descs, "c1")
	d3, _ := eng.Check(descs, "c1")
	d4, _ := eng.Check(descs, "c1") // burst=3 exhausted
	fmt.Println(d1.Allow, d2.Allow, d3.Allow, d4.Allow)
	// Output: true true true false
}
```

## Review

The engine is correct when an unmatched request returns `Decision{Allow: true}` with no limiter touched, and when, among matched rules, the aggregation reports the right dimension: the longest `RetryAfter` on a deny, the lowest remaining on an all-allow. `TestEngineMostRestrictiveRuleApplied` is the guard that a tight per-client rule overrides a loose global one and names itself. Client isolation depends on the `sync.Map` key including the client portion, and the `LoadOrStore` is what keeps a construction race from double-creating a limiter — `TestEngineClientIsolation` and the `-race` flag together prove both. Always call `Stop`: the eviction goroutine outlives the last request otherwise, and in the `synctest` test it must be stopped and waited on or the bubble reports a lingering goroutine. The refill test passing under virtual time confirms `Check` reads only the clock that the bubble virtualizes, with no hidden real-time dependency.

## Resources

- [`sync.Map`](https://pkg.go.dev/sync#Map) — the per-client store, including the `LoadOrStore` race-free creation and the read-mostly trade-offs.
- [`errors.Is`](https://pkg.go.dev/errors#Is) — matching wrapped sentinel errors, used to assert on `ErrDuplicateRule`.
- [Envoy global rate limiting](https://www.envoyproxy.io/docs/envoy/latest/intro/arch_overview/other_features/global_rate_limiting) — the descriptor-and-rule model this engine mirrors.

---

Back to [02-sliding-window.md](02-sliding-window.md) | Next: [04-http-middleware.md](04-http-middleware.md)
