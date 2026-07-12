# Exercise 2: Token-Bucket / GCRA Limiting with redis_rate

Most APIs do not want a hard window; they want a smooth long-run rate that still
tolerates a short burst. That is token-bucket behavior, and `redis_rate` gives it
to you as GCRA with O(1) state per key. This exercise wraps that limiter in HTTP
middleware that emits the standard RateLimit headers, returns `429` with
`Retry-After` when denied, and partially admits an over-budget batch with
`AllowAtMost`.

This module is fully self-contained: its own `go mod init`, its own demo, its own
tests. Tests run against an in-process `miniredis`; an integration test behind a
build tag targets a real Redis.

## What you'll build

```text
gcralimit/                       independent module: example.com/gcralimit
  go.mod                         go 1.26; requires redis_rate/v10, go-redis/v9, miniredis/v2
  middleware.go                  Middleware, New, Handler, BatchHandler, RateLimit header helpers
  cmd/
    demo/
      main.go                    starts miniredis, sends 5 requests through a burst of 3
  middleware_test.go             miniredis + httptest: 429 transition, headers, AllowAtMost, Example
  middleware_integration_test.go //go:build integration — against REDIS_ADDR
```

Files: `middleware.go`, `cmd/demo/main.go`, `middleware_test.go`, `middleware_integration_test.go`.
Implement: a `Middleware` that keys on client identity, calls `Allow`, sets `RateLimit-Limit/Remaining/Reset` on success and `Retry-After` plus `429` on denial, and a `BatchHandler` using `AllowAtMost` for weighted endpoints.
Test: exhausting the burst transitions to `429` with a positive `Retry-After` and `RateLimit-Remaining: 0`; headers are correct on an allowed request; `AllowAtMost` admits fewer than requested for an over-budget batch; `Result.RetryAfter` is `-1` when allowed.
Verify: `go test -race ./...` offline; `go test -tags integration -race ./...` against Redis.

Set up the module:

```bash
mkdir -p go-solutions/54-cloud-native-platform-and-orchestration/10-redis-rate-limiting/02-token-bucket-gcra-redis-rate/cmd/demo
cd go-solutions/54-cloud-native-platform-and-orchestration/10-redis-rate-limiting/02-token-bucket-gcra-redis-rate
go mod edit -go=1.26
go get github.com/go-redis/redis_rate/v10
go get github.com/redis/go-redis/v9
go get github.com/alicebob/miniredis/v2
```

### Why GCRA, and what the Limit means

`redis_rate` implements GCRA — a token bucket expressed as a single virtual
"theoretical arrival time" per key. It gives a smooth rate with a controlled
burst and constant memory, which is what most public APIs actually want. The
policy is a `redis_rate.Limit{Rate, Burst, Period}`: `Rate` requests per `Period`
in steady state, tolerating up to `Burst` above that rate in a spike. The helpers
`PerSecond(n)`, `PerMinute(n)`, and `PerHour(n)` build the common cases with
`Rate == Burst`; construct the struct directly when you want a burst that differs
from the rate.

`Allow(ctx, key, limit)` costs one request; `AllowN(ctx, key, limit, n)` costs
`n` at once and is all-or-nothing; `AllowAtMost(ctx, key, limit, n)` admits *as
many of `n` as fit* and reports how many in `Result.Allowed`. Each returns a
`*Result{Limit, Allowed, Remaining, RetryAfter, ResetAfter}`. Two fields drive the
response contract: `RetryAfter` is how long until one more request would be
admitted (it is `-1` — a negative `time.Duration` — when the request was allowed,
so you must not emit it as a header in that case), and `ResetAfter` is how long
until the limiter returns to a fully unused state, which maps to `RateLimit-Reset`.

### The middleware

The middleware keys on client identity via an injected `KeyFunc` (so a test can
force a key and production can read an API key or authenticated subject). On every
request it sets the three RateLimit headers from the `Result`; on a denial it adds
`Retry-After` and writes `429`. `Retry-After` is an integer number of seconds, so
round `RetryAfter` up and clamp it to at least one second. A limiter error is a
different situation from a denial — here it surfaces as a `500`; the next exercise
replaces that with a deliberate degradation policy.

Create `middleware.go`:

```go
package gcralimit

import (
	"math"
	"net/http"
	"strconv"

	"github.com/go-redis/redis_rate/v10"
	"github.com/redis/go-redis/v9"
)

// Middleware enforces a per-client GCRA limit backed by Redis.
type Middleware struct {
	limiter *redis_rate.Limiter
	limit   redis_rate.Limit
	keyFn   func(*http.Request) string
}

// New builds a Middleware. keyFn maps a request to the limiter key (client id, API
// key, authenticated subject, ...).
func New(rdb *redis.Client, limit redis_rate.Limit, keyFn func(*http.Request) string) *Middleware {
	return &Middleware{
		limiter: redis_rate.NewLimiter(rdb),
		limit:   limit,
		keyFn:   keyFn,
	}
}

// setRateHeaders emits the IETF RateLimit headers from a limiter result.
func setRateHeaders(h http.Header, res *redis_rate.Result) {
	h.Set("RateLimit-Limit", strconv.Itoa(res.Limit.Rate))
	h.Set("RateLimit-Remaining", strconv.Itoa(res.Remaining))
	h.Set("RateLimit-Reset", strconv.Itoa(secondsUp(res.ResetAfter.Seconds())))
}

// secondsUp rounds a non-negative seconds value up to a whole second.
func secondsUp(s float64) int {
	if s <= 0 {
		return 0
	}
	return int(math.Ceil(s))
}

// Handler wraps next, admitting one unit of quota per request.
func (m *Middleware) Handler(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		res, err := m.limiter.Allow(r.Context(), m.keyFn(r), m.limit)
		if err != nil {
			http.Error(w, "rate limiter unavailable", http.StatusInternalServerError)
			return
		}
		setRateHeaders(w.Header(), res)
		if res.Allowed == 0 {
			retry := secondsUp(res.RetryAfter.Seconds())
			if retry < 1 {
				retry = 1
			}
			w.Header().Set("Retry-After", strconv.Itoa(retry))
			http.Error(w, "rate limit exceeded", http.StatusTooManyRequests)
			return
		}
		next.ServeHTTP(w, r)
	})
}

// BatchHandler admits up to cost(r) units at once with AllowAtMost, partially
// admitting an over-budget batch. It reports how many were admitted in the
// RateLimit-Admitted header and the response body.
func (m *Middleware) BatchHandler(cost func(*http.Request) int) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		n := cost(r)
		res, err := m.limiter.AllowAtMost(r.Context(), m.keyFn(r), m.limit, n)
		if err != nil {
			http.Error(w, "rate limiter unavailable", http.StatusInternalServerError)
			return
		}
		setRateHeaders(w.Header(), res)
		w.Header().Set("RateLimit-Admitted", strconv.Itoa(res.Allowed))
		if res.Allowed == 0 {
			retry := secondsUp(res.RetryAfter.Seconds())
			if retry < 1 {
				retry = 1
			}
			w.Header().Set("Retry-After", strconv.Itoa(retry))
			http.Error(w, "rate limit exceeded", http.StatusTooManyRequests)
			return
		}
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte("admitted " + strconv.Itoa(res.Allowed) + " of " + strconv.Itoa(n)))
	})
}
```

### The runnable demo

The demo starts `miniredis`, builds a middleware with a burst of three, and sends
five requests through it, printing the status and remaining-quota header for each.
The first three are admitted; the last two are rejected with `429`. Note that
GCRA's reported remaining is not a simple decrement — it reflects how much burst
headroom the virtual clock leaves at that instant.

One harness caveat: these `RateLimit-Remaining` values are what the in-process
`miniredis` GCRA implementation reports, which is one lower than a real Redis
server on the first request (miniredis reports `burst-2`, e.g. `1` for a burst of
3, where a real server's `redis_rate` reports `burst-1`, e.g. `2`). All shipped
code here runs against `miniredis`, so the demo, the `Example`, and the header
assertions are internally consistent; if you point the same middleware at a live
Redis, expect the first request's remaining to be one higher.

Create `cmd/demo/main.go`:

```go
package main

import (
	"fmt"
	"log"
	"net/http"
	"net/http/httptest"
	"time"

	"github.com/alicebob/miniredis/v2"
	"github.com/go-redis/redis_rate/v10"
	"github.com/redis/go-redis/v9"

	"example.com/gcralimit"
)

func main() {
	mr, err := miniredis.Run()
	if err != nil {
		log.Fatal(err)
	}
	defer mr.Close()

	rdb := redis.NewClient(&redis.Options{Addr: mr.Addr()})
	defer rdb.Close()

	limit := redis_rate.Limit{Rate: 10, Burst: 3, Period: time.Second}
	mw := gcralimit.New(rdb, limit, func(r *http.Request) string { return "client:demo" })

	handler := mw.Handler(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
	}))

	fmt.Println("GCRA rate=10/s burst=3")
	for i := 1; i <= 5; i++ {
		rec := httptest.NewRecorder()
		handler.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/", nil))
		fmt.Printf("request %d: status=%d remaining=%s\n",
			i, rec.Code, rec.Header().Get("RateLimit-Remaining"))
	}
}
```

Run it:

```bash
go run ./cmd/demo
```

Expected output:

```
GCRA rate=10/s burst=3
request 1: status=200 remaining=1
request 2: status=200 remaining=1
request 3: status=200 remaining=0
request 4: status=429 remaining=0
request 5: status=429 remaining=0
```

### Tests

The tests drive the middleware with `httptest.NewRequest` and
`httptest.NewRecorder` against a `miniredis`-backed limiter. `TestExhaustBurst`
sends the whole burst, asserts each is `200`, then asserts the next request is
`429` with `RateLimit-Remaining: 0` and a positive `Retry-After`.
`TestHeadersOnAllowed` checks the RateLimit headers on the first request.
`TestAllowAtMostPartial` sends a batch larger than the burst and asserts fewer
were admitted. `TestRetryAfterMinusOneWhenAllowed` calls the limiter directly to
pin the documented sentinel: `RetryAfter` is `-1` on an allowed request, which is
why the middleware never emits a `Retry-After` header in that case.

Create `middleware_test.go`:

```go
package gcralimit

import (
	"context"
	"fmt"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/alicebob/miniredis/v2"
	"github.com/go-redis/redis_rate/v10"
	"github.com/redis/go-redis/v9"
)

func newMiddleware(t *testing.T, limit redis_rate.Limit) (*Middleware, *redis.Client) {
	t.Helper()
	mr := miniredis.RunT(t)
	rdb := redis.NewClient(&redis.Options{Addr: mr.Addr()})
	t.Cleanup(func() { _ = rdb.Close() })
	mw := New(rdb, limit, func(r *http.Request) string { return "client:test" })
	return mw, rdb
}

func okHandler() http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
	})
}

func TestExhaustBurst(t *testing.T) {
	t.Parallel()
	mw, _ := newMiddleware(t, redis_rate.Limit{Rate: 10, Burst: 3, Period: time.Second})
	h := mw.Handler(okHandler())

	for i := range 3 {
		rec := httptest.NewRecorder()
		h.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/", nil))
		if rec.Code != http.StatusOK {
			t.Fatalf("burst request %d: status = %d; want 200", i, rec.Code)
		}
	}

	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/", nil))
	if rec.Code != http.StatusTooManyRequests {
		t.Fatalf("over-burst status = %d; want 429", rec.Code)
	}
	if got := rec.Header().Get("RateLimit-Remaining"); got != "0" {
		t.Fatalf("RateLimit-Remaining = %q; want %q", got, "0")
	}
	if got := rec.Header().Get("Retry-After"); got == "" || got == "0" {
		t.Fatalf("Retry-After = %q; want a positive integer", got)
	}
}

func TestHeadersOnAllowed(t *testing.T) {
	t.Parallel()
	mw, _ := newMiddleware(t, redis_rate.Limit{Rate: 10, Burst: 3, Period: time.Second})
	h := mw.Handler(okHandler())

	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/", nil))

	if got := rec.Header().Get("RateLimit-Limit"); got != "10" {
		t.Fatalf("RateLimit-Limit = %q; want %q", got, "10")
	}
	if got := rec.Header().Get("RateLimit-Remaining"); got != "1" {
		t.Fatalf("RateLimit-Remaining = %q; want %q", got, "1")
	}
	if got := rec.Header().Get("Retry-After"); got != "" {
		t.Fatalf("Retry-After = %q; want empty on an allowed request", got)
	}
}

func TestAllowAtMostPartial(t *testing.T) {
	t.Parallel()
	mw, _ := newMiddleware(t, redis_rate.Limit{Rate: 10, Burst: 3, Period: time.Second})
	h := mw.BatchHandler(func(r *http.Request) int { return 5 })

	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/", nil))

	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d; want 200 (partial admit)", rec.Code)
	}
	if got := rec.Header().Get("RateLimit-Admitted"); got != "3" {
		t.Fatalf("RateLimit-Admitted = %q; want %q (3 of 5 fit the burst)", got, "3")
	}
}

func TestRetryAfterMinusOneWhenAllowed(t *testing.T) {
	t.Parallel()
	_, rdb := newMiddleware(t, redis_rate.Limit{})
	limiter := redis_rate.NewLimiter(rdb)

	res, err := limiter.Allow(context.Background(), "solo", redis_rate.PerSecond(5))
	if err != nil {
		t.Fatal(err)
	}
	if res.Allowed != 1 {
		t.Fatalf("Allowed = %d; want 1", res.Allowed)
	}
	if res.RetryAfter != -1 {
		t.Fatalf("RetryAfter = %v; want -1 on an allowed request", res.RetryAfter)
	}
}

func Example() {
	mr, _ := miniredis.Run()
	defer mr.Close()
	rdb := redis.NewClient(&redis.Options{Addr: mr.Addr()})
	defer rdb.Close()

	limiter := redis_rate.NewLimiter(rdb)
	limit := redis_rate.Limit{Rate: 10, Burst: 2, Period: time.Second}
	for i := 1; i <= 3; i++ {
		res, _ := limiter.Allow(context.Background(), "user:1", limit)
		fmt.Printf("req %d allowed=%d remaining=%d\n", i, res.Allowed, res.Remaining)
	}
	// Output:
	// req 1 allowed=1 remaining=0
	// req 2 allowed=1 remaining=0
	// req 3 allowed=0 remaining=0
}
```

The integration test exercises the same middleware against a real Redis, behind
the `integration` tag so the default test path stays offline.

Create `middleware_integration_test.go`:

```go
//go:build integration

package gcralimit

import (
	"net/http"
	"net/http/httptest"
	"os"
	"testing"
	"time"

	"github.com/go-redis/redis_rate/v10"
	"github.com/redis/go-redis/v9"
)

func TestExhaustBurstRedis(t *testing.T) {
	addr := os.Getenv("REDIS_ADDR")
	if addr == "" {
		addr = "127.0.0.1:6379"
	}
	rdb := redis.NewClient(&redis.Options{Addr: addr})
	t.Cleanup(func() { _ = rdb.Close() })

	key := "test:gcra:" + t.Name()
	_ = redis_rate.NewLimiter(rdb).Reset(t.Context(), key)

	mw := New(rdb, redis_rate.Limit{Rate: 10, Burst: 3, Period: time.Second},
		func(r *http.Request) string { return key })
	h := mw.Handler(okHandler())

	statuses := make([]int, 0, 4)
	for range 4 {
		rec := httptest.NewRecorder()
		h.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/", nil))
		statuses = append(statuses, rec.Code)
	}
	if statuses[3] != http.StatusTooManyRequests {
		t.Fatalf("4th status = %d; want 429", statuses[3])
	}
}
```

## Review

The middleware is correct when the response contract matches the limiter's answer
exactly: RateLimit headers on every request, `429` with a rounded-up `Retry-After`
only on a denial, and no `Retry-After` when the request was allowed. That last
point is the easy bug — `Result.RetryAfter` is `-1` on an allowed request, so
emitting it unconditionally sends a nonsensical `Retry-After: -1`. The `Handler`
guards it by only setting the header on the denied branch, and
`TestRetryAfterMinusOneWhenAllowed` pins the sentinel so the assumption cannot
silently rot.

The other subtlety is `AllowAtMost` versus `AllowN`: `AllowN` is all-or-nothing,
so a batch of five against a burst of three is fully rejected, while `AllowAtMost`
admits the three that fit and reports `Allowed == 3`. Use `AllowAtMost` for a
weighted endpoint that can do useful partial work; use `Allow`/`AllowN` when the
unit is indivisible. Confirm with `go test -race ./...`; the burst-exhaustion
test proves the `200`-to-`429` transition and the headers, and the partial test
proves the batch path.

## Resources

- [redis_rate v10](https://pkg.go.dev/github.com/go-redis/redis_rate/v10) — `Limiter`, `Limit`, `Allow`/`AllowN`/`AllowAtMost`, and the `Result` fields.
- [go-redis/redis_rate source](https://github.com/go-redis/redis_rate) — the GCRA Lua script and the `dur` helper that returns `-1` when allowed.
- [RateLimit header fields for HTTP (IETF draft)](https://datatracker.ietf.org/doc/draft-ietf-httpapi-ratelimit-headers/) — `RateLimit-Limit`, `RateLimit-Remaining`, `RateLimit-Reset`.
- [alicebob/miniredis](https://pkg.go.dev/github.com/alicebob/miniredis/v2) — in-process Redis (EVAL, TIME) for the offline tests.

---

Prev: [01-sliding-window-atomic-lua.md](01-sliding-window-atomic-lua.md) | Back to [00-concepts.md](00-concepts.md) | Next: [03-resilient-limiter-degradation.md](03-resilient-limiter-degradation.md)
