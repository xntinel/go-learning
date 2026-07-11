# Exercise 7: Per-client token-bucket rate limiting

A hot client must not be able to starve an upstream or database. This module
builds a `RateLimit` middleware that keeps one `golang.org/x/time/rate.Limiter`
per client behind a mutex, returns 429 with `Retry-After` when the bucket is
empty, and is safe under concurrent load.

Fully self-contained: its own `go mod init`, demo, and tests. It imports the
external module `golang.org/x/time/rate`; the gate fetches it. Nothing here imports
another exercise.

## What you'll build

```text
ratelimit/                   independent module: example.com/ratelimit
  go.mod                     go 1.26 + require golang.org/x/time/rate
  middleware.go              keyed limiter store + RateLimit middleware
  cmd/demo/main.go           runnable demo: first request allowed, second 429
  middleware_test.go         allow/deny, per-key isolation, -race concurrency
```

- Files: `middleware.go`, `cmd/demo/main.go`, `middleware_test.go`.
- Implement: a `Limiter` store keying `*rate.Limiter` by client (IP from `RemoteAddr`) behind a `sync.Mutex`, and a `RateLimit` middleware that returns 429 with `Retry-After` when `Allow()` is false.
- Test: with rate 0 / burst 1, the first request is 200 and the immediate second is 429 with `Retry-After` set; two distinct clients each get their own bucket; run concurrent requests under `-race`.
- Verify: `go test -count=1 -race ./...`

Set up the module:

```bash
mkdir -p ~/go-exercises/ratelimit/cmd/demo
cd ~/go-exercises/ratelimit
go mod init example.com/ratelimit
go mod edit -go=1.26
go get golang.org/x/time/rate
```

### Keyed buckets, persisted, behind a lock

The requirement is *per-client* limiting, which rules out a single shared
`rate.Limiter` (that throttles everyone together) and a per-request limiter (that
resets every call and never limits). The correct structure is a
`map[string]*rate.Limiter` that *persists* across requests, keyed by client
identity — here the IP parsed from `r.RemoteAddr`. Each bucket is
`rate.NewLimiter(rate.Every(interval), burst)`: `rate.Every(interval)` converts a
"one token every interval" rate into a `rate.Limit`, and `burst` is how many
tokens can be spent at once before throttling. `Allow()` reports whether a token
was available *now*, consuming one if so; it is the non-blocking check a middleware
wants (never block the request goroutine waiting for a token).

The map is shared mutable state touched by every concurrent request, so it must be
guarded. A `sync.Mutex` around the get-or-create protects both the map read and
the lazy insert; without it, two requests for a new client could race on the map
and corrupt it — the exact data race `go test -race` exists to catch. On rejection
the middleware writes 429 and a `Retry-After` header (seconds the client should
wait) so well-behaved clients back off instead of hammering.

One production caveat stated honestly: this map grows unbounded — a new key per
distinct client IP, never removed. A real deployment caps it with an LRU or a TTL
sweep so a flood of unique clients cannot exhaust memory. That eviction is out of
scope here but is the natural follow-up; the limiter *store* is itself a resource
to manage.

Create `middleware.go`:

```go
package ratelimit

import (
	"net"
	"net/http"
	"strconv"
	"sync"
	"time"

	"golang.org/x/time/rate"
)

type Handler = http.Handler

type Middleware func(Handler) Handler

// Limiter holds one token bucket per client key, created lazily.
type Limiter struct {
	mu         sync.Mutex
	buckets    map[string]*rate.Limiter
	rate       rate.Limit
	burst      int
	retryAfter int // seconds advertised on 429
}

// NewLimiter builds a store issuing one token every interval with the given burst.
func NewLimiter(interval time.Duration, burst int) *Limiter {
	retry := int(interval / time.Second)
	if retry < 1 {
		retry = 1
	}
	return &Limiter{
		buckets:    make(map[string]*rate.Limiter),
		rate:       rate.Every(interval),
		burst:      burst,
		retryAfter: retry,
	}
}

// get returns the bucket for key, creating it on first use under the lock.
func (l *Limiter) get(key string) *rate.Limiter {
	l.mu.Lock()
	defer l.mu.Unlock()
	b, ok := l.buckets[key]
	if !ok {
		b = rate.NewLimiter(l.rate, l.burst)
		l.buckets[key] = b
	}
	return b
}

func clientKey(r *http.Request) string {
	host, _, err := net.SplitHostPort(r.RemoteAddr)
	if err != nil {
		return r.RemoteAddr // no port present; use as-is
	}
	return host
}

// Middleware returns 429 with Retry-After when the client's bucket is empty.
func (l *Limiter) Middleware(next Handler) Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if !l.get(clientKey(r)).Allow() {
			w.Header().Set("Retry-After", strconv.Itoa(l.retryAfter))
			http.Error(w, "rate limit exceeded", http.StatusTooManyRequests)
			return
		}
		next.ServeHTTP(w, r)
	})
}
```

### The runnable demo

The demo uses `rate.Every(time.Hour)` with burst 1, so the same client gets one
request through and the next is throttled immediately — deterministic output
without waiting for a real refill.

Create `cmd/demo/main.go`:

```go
package main

import (
	"fmt"
	"net/http"
	"net/http/httptest"
	"time"

	"example.com/ratelimit"
)

func main() {
	final := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		fmt.Fprint(w, "ok")
	})
	limiter := ratelimit.NewLimiter(time.Hour, 1)
	handler := limiter.Middleware(final)

	send := func() int {
		rec := httptest.NewRecorder()
		req := httptest.NewRequest(http.MethodGet, "/", nil)
		req.RemoteAddr = "10.0.0.1:5000"
		handler.ServeHTTP(rec, req)
		return rec.Code
	}

	fmt.Printf("first request:  %d\n", send())
	fmt.Printf("second request: %d\n", send())
}
```

Run it:

```bash
go run ./cmd/demo
```

Expected output:

```
first request:  200
second request: 429
```

### Tests

`TestAllowThenDeny` drives two requests from one client through a burst-1 bucket:
200 then 429 with `Retry-After` set. `TestPerClientIsolation` proves two distinct
`RemoteAddr`s each get their own bucket (both first requests 200).
`TestConcurrentRace` fires many concurrent requests to exercise the map+mutex under
`-race`.

Create `middleware_test.go`:

```go
package ratelimit

import (
	"net/http"
	"net/http/httptest"
	"strconv"
	"sync"
	"testing"
	"time"
)

func send(h http.Handler, addr string) *httptest.ResponseRecorder {
	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodGet, "/", nil)
	req.RemoteAddr = addr
	h.ServeHTTP(rec, req)
	return rec
}

func okHandler() http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
	})
}

func TestAllowThenDeny(t *testing.T) {
	t.Parallel()

	h := NewLimiter(time.Hour, 1).Middleware(okHandler())

	if got := send(h, "1.2.3.4:1000").Code; got != http.StatusOK {
		t.Fatalf("first = %d, want 200", got)
	}
	rec := send(h, "1.2.3.4:1000")
	if rec.Code != http.StatusTooManyRequests {
		t.Fatalf("second = %d, want 429", rec.Code)
	}
	if rec.Header().Get("Retry-After") == "" {
		t.Fatal("429 missing Retry-After header")
	}
}

func TestPerClientIsolation(t *testing.T) {
	t.Parallel()

	h := NewLimiter(time.Hour, 1).Middleware(okHandler())

	if got := send(h, "1.1.1.1:100").Code; got != http.StatusOK {
		t.Fatalf("client A first = %d, want 200", got)
	}
	if got := send(h, "2.2.2.2:100").Code; got != http.StatusOK {
		t.Fatalf("client B first = %d, want 200 (separate bucket)", got)
	}
}

func TestConcurrentRace(t *testing.T) {
	t.Parallel()

	h := NewLimiter(time.Millisecond, 100).Middleware(okHandler())
	var wg sync.WaitGroup
	for i := range 50 {
		wg.Add(1)
		go func() {
			defer wg.Done()
			send(h, "9.9.9.9:"+strconv.Itoa(i))
		}()
	}
	wg.Wait()
}
```

## Review

The limiter is correct when it is keyed, persisted, and locked: each client has its
own bucket, buckets survive across requests, and the map is only touched under the
mutex. `TestAllowThenDeny` proves the bucket actually limits (burst 1 means the
second immediate request is denied), and `TestPerClientIsolation` proves one
client's exhaustion does not affect another — the property a single shared limiter
would violate. `TestConcurrentRace` under `-race` is non-negotiable for this
lesson: the get-or-create on a shared map is the textbook middleware data race.
Remember the map grows without bound; a real service caps it with LRU/TTL eviction,
which is the intended next step, not an optional nicety.

## Resources

- [golang.org/x/time/rate](https://pkg.go.dev/golang.org/x/time/rate) — the token-bucket `Limiter`, `NewLimiter`, and `Allow`.
- [rate#Every](https://pkg.go.dev/golang.org/x/time/rate#Every) — converting an interval into a `rate.Limit`.
- [net#SplitHostPort](https://pkg.go.dev/net#SplitHostPort) — extracting the client IP from `RemoteAddr`.
- [net/http#StatusTooManyRequests](https://pkg.go.dev/net/http#StatusTooManyRequests) — the 429 status the middleware returns.

---

Back to [00-concepts.md](00-concepts.md) | Prev: [06-request-id-context-middleware.md](06-request-id-context-middleware.md) | Next: [08-request-timeout-middleware.md](08-request-timeout-middleware.md)
