# Exercise 4: The Production API Gateway Stack

A real edge gateway is a single middleware chain whose order is a set of operational decisions: recovery must wrap everything, the request ID must exist before the first log line, the access log must record rejected requests too, and the per-principal rate limiter can only run after authentication has named the principal. This exercise assembles that full stack — panic recovery, request ID, structured logging, request timeout, bearer auth, and a token-bucket rate limiter — behind one `Stack` constructor, and tests it with `net/http/httptest` to prove both the layer ordering and the short-circuit behavior where auth returns 401 and the limiter returns 429 without the handler ever running.

This module is fully self-contained. It begins with its own `go mod init`, defines every type it needs, and ships its own demo and tests. Nothing here imports any other exercise.

## What you'll build

```text
gateway.go           Middleware, Chain, Deps, Stack,
                     Recoverer, RequestID/RequestIDFrom,
                     Logging (statusRecorder, injectable clock),
                     Timeout, Auth/PrincipalFrom, RateLimit (token bucket)
cmd/
  demo/
    main.go          build the full stack and drive an authorized request,
                     a rate-limited one, an unauthorized one, and a panic
gateway_test.go      chain ordering outside-in, request-ID propagation,
                     structured log line with duration, 401 short-circuit,
                     429 after the bucket drains then refill, timeout 503,
                     panic-to-500, and a full-stack integration assertion
```

- Files: `gateway.go`, `cmd/demo/main.go`, `gateway_test.go`.
- Implement: `Stack` composing the six middlewares in the canonical outside-in order, each middleware as a `func(http.Handler) http.Handler`, with `RateLimit` keyed on the authenticated principal and both `Logging` and `RateLimit` driven by an injectable `now func() time.Time` for deterministic tests.
- Test: a tracing chain runs outside-in; the request ID reaches the log line and the response header; an unauthenticated request short-circuits to 401 before auth's inner layers; a token bucket of capacity N admits N requests then returns 429 until the clock advances enough to refill; a slow handler becomes a 503; a panic becomes a 500; and the assembled stack rejects and admits as configured.
- Verify: `go test -race ./...`

Set up the module:

```bash
mkdir -p go-solutions/24-design-patterns-in-go/08-middleware-decorator-pattern/04-production-api-gateway-stack/cmd/demo && cd go-solutions/24-design-patterns-in-go/08-middleware-decorator-pattern/04-production-api-gateway-stack
```

### Why the order is the design

Every middleware has the mirror-image shape: code before `next.ServeHTTP` runs on the way in, code after runs on the way out, and `Chain` makes the first argument the outermost wrapper. For a gateway the order is not stylistic — each position encodes an operational requirement, and getting it wrong produces a subtly broken edge that still compiles and mostly works.

`Recoverer` is outermost. A panic in any inner layer — the handler, the rate limiter, the auth validator — unwinds through every middleware between it and `Recoverer`, so only the outermost layer is guaranteed to catch it and turn it into a clean 500 instead of a dropped connection or a crashed serving goroutine. Put recovery anywhere inside and a panic in the layers above it escapes.

`RequestID` comes next so the correlation ID exists before anything that might want to log or return it. `Logging` sits just inside `RequestID` and just outside everything that can reject a request, because an access log that omits the 401s and 429s is useless for debugging an outage — you want a line for every request, including the ones a downstream layer short-circuited. Because `Logging` is outside the rejecting layers, its `statusRecorder` observes the 401 or 429 those layers wrote and reports it.

`Timeout` bounds the work of auth, rate limiting, and the handler with a single deadline. `Auth` runs before `RateLimit` for a concrete reason: the limiter is per-principal, and the principal does not exist until auth has validated the bearer token and stored it in the context. A rate limiter placed outside auth could only key on something like the client IP, which is a different and weaker policy. Authenticate, name the principal, then meter that principal — that dependency forces `Auth` to be the outer of the two.

### The rejecting layers and short-circuit

A middleware short-circuits by writing a response and simply not calling `next`. `Auth` reads the `Authorization` header, strips the `Bearer ` prefix, and asks an injected `validate` function whether the token is good. On failure it writes 401 and returns — the handler and the rate limiter below it never run. On success it stores the principal in the request context under an unexported key type (the standard guard against context-key collisions) and delegates. `PrincipalFrom` is the typed accessor the limiter uses.

`RateLimit` is a token-bucket limiter keyed by principal. Each principal gets a bucket with a capacity and a refill rate; every admitted request spends one token, and tokens regenerate over time. The bucket refills lazily: instead of a background goroutine ticking, each call computes how much wall-clock time has passed since the bucket was last touched and adds `elapsed * rate` tokens, capped at capacity. That lazy-refill design is why the limiter takes an injectable clock — a test can hold the clock still to drain the bucket, then advance it a known amount to prove exactly one token came back. When no token is available the layer writes 429 and does not call `next`.

`Timeout` wraps the inner handler in the standard library's `http.TimeoutHandler`, which runs the handler in its own goroutine, cancels the request context when the deadline fires, and writes a 503 with a message. Reusing the stdlib here is the senior move: `TimeoutHandler` already solves the hard part — serializing the timeout write against a handler that may still be writing — with an internal locked response writer, so the chain does not have to reinvent a race-free timeout. The handler must still honor `r.Context().Done()` for the cancellation to actually stop in-flight work.

Create `gateway.go`:

```go
// Package gateway assembles a production HTTP middleware stack for an API edge:
// panic recovery, request ID, structured logging, request timeout, bearer auth,
// and a per-principal token-bucket rate limiter, composed outside-in.
package gateway

import (
	"context"
	"fmt"
	"io"
	"net/http"
	"strings"
	"sync"
	"time"
)

// Middleware decorates an http.Handler: it takes the next handler and returns a
// new handler that adds behavior before and after delegating to it.
type Middleware func(http.Handler) http.Handler

// Chain wraps h with mws so that mws[0] is the outermost layer: it sees the
// request first and the response last. Applying the slice in reverse makes the
// first middleware the last one applied, hence the outermost wrapper.
func Chain(h http.Handler, mws ...Middleware) http.Handler {
	for i := len(mws) - 1; i >= 0; i-- {
		h = mws[i](h)
	}
	return h
}

// Deps holds everything the canonical stack needs. The two clocks-as-functions
// (Now) are injected so tests can make logging duration and rate-limit refill
// deterministic.
type Deps struct {
	Now      func() time.Time
	Logs     io.Writer
	NewID    func() string
	Validate func(token string) (principal string, ok bool)
	Capacity float64       // token-bucket capacity per principal
	Refill   float64       // tokens regenerated per second
	Timeout  time.Duration // per-request deadline
}

// Stack composes the six middlewares in the canonical outside-in order:
// Recoverer (catches every inner panic), then RequestID (correlation ID exists
// first), then Logging (records even rejected requests), then Timeout (bounds
// the inner work), then Auth (names the principal), then RateLimit (meters that
// principal). Auth must sit outside RateLimit because the limiter keys on the
// principal Auth produces.
func Stack(h http.Handler, d Deps) http.Handler {
	return Chain(h,
		Recoverer,
		RequestID(d.NewID),
		Logging(d.Logs, d.Now),
		Timeout(d.Timeout),
		Auth(d.Validate),
		RateLimit(d.Capacity, d.Refill, d.Now),
	)
}

type ctxKey int

const (
	requestIDKey ctxKey = iota
	principalKey
)

// Recoverer catches a panic from any inner layer and turns it into a 500. It
// belongs outermost so it guards every middleware between it and the handler.
func Recoverer(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		defer func() {
			if v := recover(); v != nil {
				w.WriteHeader(http.StatusInternalServerError)
			}
		}()
		next.ServeHTTP(w, r)
	})
}

// RequestID assigns an ID from gen, sets it on the X-Request-ID response header,
// and stores it in the request context so inner layers (and logging) can read it.
func RequestID(gen func() string) Middleware {
	return func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			id := gen()
			w.Header().Set("X-Request-ID", id)
			ctx := context.WithValue(r.Context(), requestIDKey, id)
			next.ServeHTTP(w, r.WithContext(ctx))
		})
	}
}

// RequestIDFrom returns the request ID stored by RequestID, or "" if absent.
func RequestIDFrom(ctx context.Context) string {
	id, _ := ctx.Value(requestIDKey).(string)
	return id
}

// statusRecorder remembers the status code written downstream so logging can
// report it. It embeds the real ResponseWriter, so every method except
// WriteHeader is promoted unchanged.
type statusRecorder struct {
	http.ResponseWriter
	status int
}

func (s *statusRecorder) WriteHeader(code int) {
	s.status = code
	s.ResponseWriter.WriteHeader(code)
}

// Logging writes a structured access line after the inner layers respond. It is
// outside the rejecting layers, so it logs 401s and 429s too. now is injected so
// the duration is deterministic in tests.
func Logging(w io.Writer, now func() time.Time) Middleware {
	return func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(rw http.ResponseWriter, r *http.Request) {
			start := now()
			rec := &statusRecorder{ResponseWriter: rw, status: http.StatusOK}
			next.ServeHTTP(rec, r)
			fmt.Fprintf(w, "level=info request_id=%s method=%s path=%s status=%d duration=%s\n",
				RequestIDFrom(r.Context()), r.Method, r.URL.Path, rec.status, now().Sub(start))
		})
	}
}

// Timeout bounds inner work to d using the standard library's TimeoutHandler,
// which runs the handler in a goroutine, cancels its context on deadline, and
// serializes the 503 write against the handler's own writes.
func Timeout(d time.Duration) Middleware {
	return func(next http.Handler) http.Handler {
		return http.TimeoutHandler(next, d, "gateway: request timed out")
	}
}

// Auth validates the bearer token via validate. On failure it short-circuits
// with 401 and never calls next. On success it stores the principal in context.
func Auth(validate func(token string) (principal string, ok bool)) Middleware {
	return func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			token := strings.TrimPrefix(r.Header.Get("Authorization"), "Bearer ")
			principal, ok := validate(token)
			if !ok {
				w.WriteHeader(http.StatusUnauthorized)
				return
			}
			ctx := context.WithValue(r.Context(), principalKey, principal)
			next.ServeHTTP(w, r.WithContext(ctx))
		})
	}
}

// PrincipalFrom returns the principal stored by Auth, or "" if absent.
func PrincipalFrom(ctx context.Context) string {
	p, _ := ctx.Value(principalKey).(string)
	return p
}

// bucket is one principal's token bucket. tokens is the current balance; last is
// when it was last refilled, used to compute the lazy refill on the next call.
type bucket struct {
	tokens float64
	last   time.Time
}

// rateLimiter is a per-principal token-bucket limiter. It refills lazily on each
// call from the injected clock, so no background goroutine is needed and tests
// control refill precisely.
type rateLimiter struct {
	mu       sync.Mutex
	buckets  map[string]*bucket
	capacity float64
	refill   float64
	now      func() time.Time
}

func (rl *rateLimiter) allow(key string) bool {
	rl.mu.Lock()
	defer rl.mu.Unlock()

	t := rl.now()
	b := rl.buckets[key]
	if b == nil {
		b = &bucket{tokens: rl.capacity, last: t}
		rl.buckets[key] = b
	}
	elapsed := t.Sub(b.last).Seconds()
	b.tokens = min(rl.capacity, b.tokens+elapsed*rl.refill)
	b.last = t
	if b.tokens >= 1 {
		b.tokens--
		return true
	}
	return false
}

// RateLimit meters each principal with a token bucket of the given capacity and
// refill rate. It must run inside Auth so PrincipalFrom is populated; it falls
// back to the remote address for unauthenticated paths. On exhaustion it
// short-circuits with 429.
func RateLimit(capacity, refillPerSec float64, now func() time.Time) Middleware {
	rl := &rateLimiter{
		buckets:  make(map[string]*bucket),
		capacity: capacity,
		refill:   refillPerSec,
		now:      now,
	}
	return func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			key := PrincipalFrom(r.Context())
			if key == "" {
				key = r.RemoteAddr
			}
			if !rl.allow(key) {
				w.WriteHeader(http.StatusTooManyRequests)
				return
			}
			next.ServeHTTP(w, r)
		})
	}
}
```

### The runnable demo

The demo builds the full stack with a deterministic clock and drives five requests: three by principal `alice` (the first two admitted, the third drains her bucket and is rejected with 429), an unauthorized request rejected with 401, and a `/panic` request by a *different* principal `bob` whose own bucket is fresh, so it reaches the handler and the panic becomes a 500. The 401 and 429 each get an access-log line because those rejecting layers sit inside `Logging` and return normally; the panic request gets *no* log line because `Recoverer` is outside `Logging`, so the panic unwinds past `Logging`'s post-handler line — the same order-is-behavior effect from exercise 2 — yet is still turned into a clean 500. That bob is not rate-limited by alice's exhaustion is the point of keying the bucket on the principal.

Create `cmd/demo/main.go`:

```go
package main

import (
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"time"

	"example.com/api-gateway"
)

func main() {
	var n int
	clock := time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)

	deps := gateway.Deps{
		Now:   func() time.Time { return clock },
		Logs:  os.Stdout,
		NewID: func() string { n++; return fmt.Sprintf("req-%d", n) },
		Validate: func(token string) (string, bool) {
			switch token {
			case "secret":
				return "alice", true
			case "vip":
				return "bob", true
			default:
				return "", false
			}
		},
		Capacity: 2,
		Refill:   1,
		Timeout:  time.Second,
	}

	final := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/panic" {
			panic("boom")
		}
		fmt.Fprintf(w, "hello %s", gateway.PrincipalFrom(r.Context()))
	})
	h := gateway.Stack(final, deps)

	type call struct{ path, token string }
	calls := []call{
		{"/orders", "secret"}, // alice          -> 200
		{"/orders", "secret"}, // alice          -> 200
		{"/orders", "secret"}, // alice drained  -> 429
		{"/orders", "wrong"},  // bad token      -> 401
		{"/panic", "vip"},     // bob, fresh bucket, panic -> 500
	}
	for _, c := range calls {
		req := httptest.NewRequest(http.MethodGet, c.path, nil)
		req.Header.Set("Authorization", "Bearer "+c.token)
		rec := httptest.NewRecorder()
		h.ServeHTTP(rec, req)
		fmt.Printf("%-7s token=%-6s -> status=%d\n", c.path, c.token, rec.Code)
	}
}
```

Run it:

```bash
go run ./cmd/demo
```

Expected output:

```
level=info request_id=req-1 method=GET path=/orders status=200 duration=0s
/orders token=secret -> status=200
level=info request_id=req-2 method=GET path=/orders status=200 duration=0s
/orders token=secret -> status=200
level=info request_id=req-3 method=GET path=/orders status=429 duration=0s
/orders token=secret -> status=429
level=info request_id=req-4 method=GET path=/orders status=401 duration=0s
/orders token=wrong  -> status=401
/panic  token=vip    -> status=500
```

The first four requests each produced exactly one access-log line — including the 429 and the 401 — because those rejecting layers sit inside `Logging` and return normally, so `Logging`'s `statusRecorder` sees the status they wrote. The fifth request, the panic, produced *no* log line: `Recoverer` is outside `Logging`, so the panic unwinds past `Logging`'s post-handler `fmt.Fprintf` and is caught one layer further out, which still yields a clean 500 but skips the log line — the same behavior exercise 2 pinned. The duration is `0s` because the demo's clock is frozen; in production the same `now` would be `time.Now` and report real latency. The third request is a 429 even with a valid token: alice's bucket of capacity two was drained by the first two requests and, with the clock frozen, never refilled. The fifth request reaches the handler at all only because it authenticates as bob, whose bucket is independent of alice's — per-principal metering in action.

### Tests

The tests drive everything through `httptest.NewRecorder` and `httptest.NewRequest`, so each handler runs synchronously in the test goroutine — no live server, no background goroutine to race when the test reads what was recorded. The ordering test chains tracing middlewares and asserts the exact in/out sequence. The short-circuit tests prove the handler never runs when auth or the limiter rejects. The rate-limit test drains the bucket with the clock held still, then advances the injected clock by a known amount to prove exactly one token regenerated.

Create `gateway_test.go`:

```go
package gateway

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
)

// trace records "label:in" before delegating and "label:out" after.
func trace(log *[]string, label string) Middleware {
	return func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			*log = append(*log, label+":in")
			next.ServeHTTP(w, r)
			*log = append(*log, label+":out")
		})
	}
}

func TestChain_OrdersLayersOutsideIn(t *testing.T) {
	t.Parallel()

	var log []string
	handler := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		log = append(log, "handler")
	})

	h := Chain(handler, trace(&log, "A"), trace(&log, "B"), trace(&log, "C"))
	h.ServeHTTP(httptest.NewRecorder(), httptest.NewRequest(http.MethodGet, "/", nil))

	got := strings.Join(log, ",")
	want := "A:in,B:in,C:in,handler,C:out,B:out,A:out"
	if got != want {
		t.Errorf("order =\n  %s\nwant\n  %s", got, want)
	}
}

func TestRequestID_ReachesLogAndHeader(t *testing.T) {
	t.Parallel()

	var buf strings.Builder
	handler := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {})
	h := Chain(handler,
		RequestID(func() string { return "fixed-id" }),
		Logging(&buf, fixedClock(time.Unix(0, 0))),
	)

	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/", nil))

	if got := rec.Header().Get("X-Request-ID"); got != "fixed-id" {
		t.Errorf("header X-Request-ID = %q, want fixed-id", got)
	}
	if !strings.Contains(buf.String(), "request_id=fixed-id") {
		t.Errorf("log = %q, want it to carry request_id=fixed-id", buf.String())
	}
}

// stepClock returns a clock that advances by step on every call, so a single
// Logging middleware (which calls now twice) reports a non-zero duration.
func stepClock(start time.Time, step time.Duration) func() time.Time {
	t := start
	return func() time.Time {
		cur := t
		t = t.Add(step)
		return cur
	}
}

func fixedClock(at time.Time) func() time.Time {
	return func() time.Time { return at }
}

func TestLogging_StructuredLineWithDuration(t *testing.T) {
	t.Parallel()

	var buf strings.Builder
	handler := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusTeapot)
	})
	h := Chain(handler, Logging(&buf, stepClock(time.Unix(0, 0), 5*time.Millisecond)))

	h.ServeHTTP(httptest.NewRecorder(), httptest.NewRequest(http.MethodPost, "/widgets", nil))

	got := strings.TrimSpace(buf.String())
	want := "level=info request_id= method=POST path=/widgets status=418 duration=5ms"
	if got != want {
		t.Errorf("log = %q\nwant   %q", got, want)
	}
}

func TestAuth_ShortCircuitsWith401(t *testing.T) {
	t.Parallel()

	var reached bool
	handler := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		reached = true
	})
	h := Chain(handler, Auth(func(tok string) (string, bool) { return "", false }))

	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/", nil))

	if rec.Code != http.StatusUnauthorized {
		t.Errorf("status = %d, want 401", rec.Code)
	}
	if reached {
		t.Error("handler ran despite failed auth; auth must not call next")
	}
}

func TestRateLimit_429AfterDrainThenRefill(t *testing.T) {
	t.Parallel()

	now := time.Unix(0, 0)
	clock := func() time.Time { return now }
	handler := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {})
	// Capacity 2, refill 1 token/sec, keyed by RemoteAddr (no auth here).
	h := Chain(handler, RateLimit(2, 1, clock))

	do := func() int {
		rec := httptest.NewRecorder()
		req := httptest.NewRequest(http.MethodGet, "/", nil)
		req.RemoteAddr = "10.0.0.1:1234"
		h.ServeHTTP(rec, req)
		return rec.Code
	}

	if c := do(); c != http.StatusOK {
		t.Fatalf("request 1 = %d, want 200", c)
	}
	if c := do(); c != http.StatusOK {
		t.Fatalf("request 2 = %d, want 200", c)
	}
	if c := do(); c != http.StatusTooManyRequests {
		t.Fatalf("request 3 = %d, want 429 (bucket drained)", c)
	}

	now = now.Add(time.Second) // refill exactly one token
	if c := do(); c != http.StatusOK {
		t.Fatalf("after refill = %d, want 200", c)
	}
	if c := do(); c != http.StatusTooManyRequests {
		t.Fatalf("after spending the refilled token = %d, want 429", c)
	}
}

func TestTimeout_SlowHandlerBecomes503(t *testing.T) {
	t.Parallel()

	handler := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		select {
		case <-time.After(time.Second):
			w.WriteHeader(http.StatusOK)
		case <-r.Context().Done():
		}
	})
	h := Chain(handler, Timeout(10*time.Millisecond))

	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/", nil))

	if rec.Code != http.StatusServiceUnavailable {
		t.Errorf("status = %d, want 503", rec.Code)
	}
}

func TestRecoverer_PanicBecomes500(t *testing.T) {
	t.Parallel()

	handler := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		panic("boom")
	})
	h := Chain(handler, Recoverer)

	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/", nil))

	if rec.Code != http.StatusInternalServerError {
		t.Errorf("status = %d, want 500", rec.Code)
	}
}

func TestStack_AdmitsAndRejects(t *testing.T) {
	t.Parallel()

	now := time.Unix(0, 0)
	var ids int
	deps := Deps{
		Now:      func() time.Time { return now },
		Logs:     &strings.Builder{},
		NewID:    func() string { ids++; return "id" },
		Validate: func(tok string) (string, bool) { return "alice", tok == "good" },
		Capacity: 1,
		Refill:   1,
		Timeout:  time.Second,
	}
	final := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if PrincipalFrom(r.Context()) != "alice" {
			t.Error("principal not propagated to handler")
		}
	})
	h := Stack(final, deps)

	send := func(token string) int {
		req := httptest.NewRequest(http.MethodGet, "/", nil)
		req.Header.Set("Authorization", "Bearer "+token)
		rec := httptest.NewRecorder()
		h.ServeHTTP(rec, req)
		return rec.Code
	}

	if c := send("bad"); c != http.StatusUnauthorized {
		t.Errorf("bad token = %d, want 401", c)
	}
	if c := send("good"); c != http.StatusOK {
		t.Errorf("first good = %d, want 200", c)
	}
	if c := send("good"); c != http.StatusTooManyRequests {
		t.Errorf("second good = %d, want 429 (capacity 1, clock frozen)", c)
	}
}
```

## Review

The stack is correct when the ordering test prints `A:in, B:in, C:in, handler, C:out, B:out, A:out`; that single assertion proves `Chain` nests the canonical order outside-in. The two short-circuit tests are the operational core: `TestAuth_ShortCircuitsWith401` asserts the handler's `reached` flag stays false, which is the only way to prove auth did not call `next`, and `TestRateLimit_429AfterDrainThenRefill` drives the bucket to empty with the clock frozen, then advances the injected clock by exactly one second to show one token — and only one — came back. If the limiter refilled on a background ticker instead of lazily from the clock, that test could not be deterministic; the injectable `now` is what makes the refill assertion exact.

Order is behavior here, just as in the simpler chain. `Logging` outside the rejecting layers is what produces a log line for the 401 and the 429; move it inside auth and those rejected requests would vanish from the access log. `Auth` outside `RateLimit` is what lets the limiter key on the principal; swap them and the limiter would only ever see an empty principal and fall back to the remote address, silently changing the policy from per-user to per-IP. `Recoverer` outermost is what guarantees a panic anywhere below becomes a 500 rather than a dropped connection. Reusing `http.TimeoutHandler` for `Timeout` is deliberate: it already serializes the 503 write against a handler that may still be writing, so the chain inherits a race-free timeout instead of hand-rolling one — which is also why `go test -race` stays clean even though the timeout path involves a second goroutine.

## Resources

- [`http.Handler` and `http.HandlerFunc`](https://pkg.go.dev/net/http#Handler) — the single-method interface every layer in the stack decorates.
- [`http.TimeoutHandler`](https://pkg.go.dev/net/http#TimeoutHandler) — the standard library's race-safe request timeout, reused by the `Timeout` middleware.
- [`net/http/httptest`](https://pkg.go.dev/net/http/httptest) — `NewRecorder` and `NewRequest`, used to drive the stack synchronously in tests.
- [Rate limiting (Cloudflare learning center)](https://www.cloudflare.com/learning/bots/what-is-rate-limiting/) — what token-bucket rate limiting protects against at an API edge.

---

Back to [00-concepts.md](00-concepts.md) | Previous: [03-generic-function-decorators.md](03-generic-function-decorators.md) | Next: [05-resilience-decorators.md](05-resilience-decorators.md)
