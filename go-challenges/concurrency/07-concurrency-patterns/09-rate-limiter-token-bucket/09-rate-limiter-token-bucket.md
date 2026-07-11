# Exercise 9: Rate Limiter: Token Bucket

Rate limiting caps how often an operation can run: it shields a service from being overwhelmed, shares resources fairly, and blunts abuse. The token bucket is the most widely used approach because it handles both a steady rate and short bursts in one mechanism -- an endpoint fronting an expensive backend (a database, an ML model, a third-party API) can serve a few requests instantly after an idle spell and then pace the rest at a fixed rate, staying responsive under normal traffic while refusing to be saturated by a spike. This exercise builds the bucket from a bare ticker limiter up to a reusable limiter shared across concurrent handlers.

## What you'll build

```text
rate-limiter-token-bucket/
  main.go        a bare ticker limiter, a token bucket with burst, a reusable
                 limiter with blocking Wait and non-blocking TryAcquire, and a
                 concurrent server whose handlers share one limiter
```

- Build: a token-bucket rate limiter and a rate-limited server that throttles many goroutines through a single shared limit.
- Implement: `BasicRateLimiter` over `time.Ticker`; a `TokenBucket` that pre-fills a buffered channel and refills it with `select`/`default`; a `RateLimiter` exposing `Wait` and `TryAcquire`; and a `RateLimitedServer` sharing one limiter across handlers.
- Verify: `go run main.go` at each step -- burst served instantly, the rest paced at the steady rate.

### Why the bucket is a buffered channel

In Go, the token bucket maps perfectly to channels: a buffered channel is the bucket, `time.Ticker` fills it at a constant rate, and workers drain it by receiving tokens. The buffer capacity determines the burst size.

```
  Token Bucket: API Rate Limiter

  +------------------+
  | token  token     |  <- buffered channel (capacity = burst)
  | token            |
  +------------------+
      ^           |
      |           |
   ticker       API handler
   refills      drains
   (10/sec)     (<-tokens)

  Burst: 3 instant requests after idle period
  Steady: 10 req/sec sustained
```

## Step 1 -- Basic Rate Limiter with Ticker

Create a rate limiter that allows one API call per interval, simulating an endpoint that processes incoming requests.

```go
package main

import (
	"fmt"
	"time"
)

const tickerInterval = 200 * time.Millisecond

// APIRequest represents an incoming HTTP request.
type APIRequest struct {
	ID     int
	Path   string
	Client string
}

// BasicRateLimiter uses a ticker to enforce a steady request rate.
type BasicRateLimiter struct {
	ticker *time.Ticker
}

func NewBasicRateLimiter(interval time.Duration) *BasicRateLimiter {
	return &BasicRateLimiter{ticker: time.NewTicker(interval)}
}

func (rl *BasicRateLimiter) Wait() {
	<-rl.ticker.C
}

func (rl *BasicRateLimiter) Stop() {
	rl.ticker.Stop()
}

func (rl *BasicRateLimiter) ServeRequests(requests []APIRequest) {
	start := time.Now()
	for _, req := range requests {
		rl.Wait()
		elapsed := time.Since(start).Round(time.Millisecond)
		fmt.Printf("  [%6v] %d %s %s -> 200 OK\n",
			elapsed, req.ID, req.Client, req.Path)
	}
	fmt.Printf("\n  %d requests served in %v (rate: 5/sec)\n", len(requests), time.Since(start))
}

func main() {
	fmt.Println("=== Basic API Rate Limiter (5 req/sec) ===")
	fmt.Println()

	requests := []APIRequest{
		{1, "/api/users/42", "mobile-app"},
		{2, "/api/orders", "web-client"},
		{3, "/api/products/search", "mobile-app"},
		{4, "/api/users/42/profile", "web-client"},
		{5, "/api/orders/create", "mobile-app"},
		{6, "/api/products/99", "web-client"},
		{7, "/api/users/list", "admin-panel"},
		{8, "/api/orders/export", "admin-panel"},
	}

	limiter := NewBasicRateLimiter(tickerInterval)
	defer limiter.Stop()

	limiter.ServeRequests(requests)
}
```

Each `<-limiter.C` blocks until the next tick, enforcing a maximum rate of 5 requests per second.

### Verification
```bash
go run main.go
```
Expected: requests spaced ~200ms apart:
```
=== Basic API Rate Limiter (5 req/sec) ===

  [ 200ms] 1 mobile-app /api/users/42 -> 200 OK
  [ 400ms] 2 web-client /api/orders -> 200 OK
  [ 600ms] 3 mobile-app /api/products/search -> 200 OK
  ...

  8 requests served in 1.6s (rate: 5/sec)
```

## Step 2 -- Token Bucket with Burst Support

Implement a token bucket that allows bursts by pre-filling tokens in a buffered channel. This models a real API that allows a short burst of requests after an idle period.

```go
package main

import (
	"fmt"
	"time"
)

const (
	tokenRefillRate     = 100 * time.Millisecond
	burstCapacity       = 3
	burstDemoRequestCount = 10
)

// TokenBucket implements rate limiting with burst support.
type TokenBucket struct {
	tokens chan struct{}
	ticker *time.Ticker
}

func NewTokenBucket(rate time.Duration, burst int) *TokenBucket {
	tb := &TokenBucket{
		tokens: make(chan struct{}, burst),
		ticker: time.NewTicker(rate),
	}

	for i := 0; i < burst; i++ {
		tb.tokens <- struct{}{}
	}

	go tb.refill()
	return tb
}

func (tb *TokenBucket) refill() {
	for range tb.ticker.C {
		select {
		case tb.tokens <- struct{}{}:
		default:
		}
	}
}

func (tb *TokenBucket) Wait() {
	<-tb.tokens
}

func (tb *TokenBucket) Stop() {
	tb.ticker.Stop()
}

func main() {
	fmt.Println("=== Token Bucket with Burst (rate=10/sec, burst=3) ===")
	fmt.Println()

	bucket := NewTokenBucket(tokenRefillRate, burstCapacity)
	defer bucket.Stop()

	start := time.Now()
	for req := 1; req <= burstDemoRequestCount; req++ {
		bucket.Wait()
		elapsed := time.Since(start).Round(time.Millisecond)
		fmt.Printf("  [%6v] request %d -> 200 OK\n", elapsed, req)
	}

	fmt.Println("\n  First 3 requests served instantly (burst).")
	fmt.Println("  Remaining requests throttled to 10/sec.")
}
```

The first 3 requests are served immediately (burst from pre-filled tokens). Subsequent requests are served at the steady-state rate.

### Verification
```bash
go run main.go
```
Expected: first 3 instant, then ~100ms apart:
```
=== Token Bucket with Burst (rate=10/sec, burst=3) ===

  [   0ms] request 1 -> 200 OK
  [   0ms] request 2 -> 200 OK
  [   0ms] request 3 -> 200 OK
  [ 100ms] request 4 -> 200 OK
  [ 200ms] request 5 -> 200 OK
  [ 300ms] request 6 -> 200 OK
  ...

  First 3 requests served instantly (burst).
  Remaining requests throttled to 10/sec.
```

## Step 3 -- Rate Limiter as a Reusable Type

Wrap the token bucket into a clean struct with both blocking (`Wait`) and non-blocking (`TryAcquire`) methods. `TryAcquire` is what you use when you want to reject excess requests with HTTP 429 instead of queuing them.

```go
package main

import (
	"fmt"
	"time"
)

const (
	limiterRate  = 100 * time.Millisecond
	limiterBurst = 3
	refillPause  = 300 * time.Millisecond
)

// RateLimiter implements a token bucket with blocking and non-blocking acquire.
type RateLimiter struct {
	tokens chan struct{}
	ticker *time.Ticker
	stop   chan struct{}
}

func NewRateLimiter(rate time.Duration, burst int) *RateLimiter {
	rl := &RateLimiter{
		tokens: make(chan struct{}, burst),
		ticker: time.NewTicker(rate),
		stop:   make(chan struct{}),
	}

	for i := 0; i < burst; i++ {
		rl.tokens <- struct{}{}
	}

	go rl.refill()
	return rl
}

func (rl *RateLimiter) refill() {
	for {
		select {
		case <-rl.ticker.C:
			select {
			case rl.tokens <- struct{}{}:
			default:
			}
		case <-rl.stop:
			return
		}
	}
}

func (rl *RateLimiter) Wait() {
	<-rl.tokens
}

func (rl *RateLimiter) TryAcquire() bool {
	select {
	case <-rl.tokens:
		return true
	default:
		return false
	}
}

func (rl *RateLimiter) Stop() {
	rl.ticker.Stop()
	close(rl.stop)
}

func demoBlocking() {
	fmt.Println("--- Blocking (Wait) ---")
	rl := NewRateLimiter(limiterRate, limiterBurst)
	start := time.Now()
	for i := 1; i <= 8; i++ {
		rl.Wait()
		fmt.Printf("  [%6v] request %d served\n",
			time.Since(start).Round(time.Millisecond), i)
	}
	rl.Stop()
}

func demoNonBlocking() {
	fmt.Println("\n--- Non-Blocking (TryAcquire) ---")
	rl := NewRateLimiter(limiterRate, limiterBurst)

	var accepted, rejected int
	for i := 1; i <= 10; i++ {
		if rl.TryAcquire() {
			accepted++
			fmt.Printf("  request %d -> 200 OK\n", i)
		} else {
			rejected++
			fmt.Printf("  request %d -> 429 Too Many Requests\n", i)
		}
	}
	fmt.Printf("\n  Accepted: %d, Rejected: %d\n", accepted, rejected)

	time.Sleep(refillPause)
	fmt.Println("\n--- After 300ms idle (tokens refilled) ---")
	for i := 11; i <= 14; i++ {
		if rl.TryAcquire() {
			fmt.Printf("  request %d -> 200 OK\n", i)
		} else {
			fmt.Printf("  request %d -> 429 Too Many Requests\n", i)
		}
	}
	rl.Stop()
}

func main() {
	fmt.Println("=== Rate Limiter: Blocking vs Non-Blocking ===")
	fmt.Println()

	demoBlocking()
	demoNonBlocking()
}
```

### Verification
```bash
go run main.go
```
Expected: blocking mode queues, non-blocking mode rejects excess:
```
=== Rate Limiter: Blocking vs Non-Blocking ===

--- Blocking (Wait) ---
  [   0ms] request 1 served
  [   0ms] request 2 served
  [   0ms] request 3 served
  [ 100ms] request 4 served
  [ 200ms] request 5 served
  ...

--- Non-Blocking (TryAcquire) ---
  request 1 -> 200 OK
  request 2 -> 200 OK
  request 3 -> 200 OK
  request 4 -> 429 Too Many Requests
  request 5 -> 429 Too Many Requests
  ...

  Accepted: 3, Rejected: 7

--- After 300ms idle (tokens refilled) ---
  request 11 -> 200 OK
  request 12 -> 200 OK
  request 13 -> 200 OK
  request 14 -> 429 Too Many Requests
```

## Step 4 -- Rate Limiter with Concurrent Workers

Apply the rate limiter to a pool of concurrent API handlers, simulating a real server where multiple goroutines handle requests but all share a single rate limit.

```go
package main

import (
	"fmt"
	"sync"
	"time"
)

const (
	serverRate         = 100 * time.Millisecond
	serverBurst        = 3
	concurrentHandlers = 20
	handlerWorkTime    = 10 * time.Millisecond
)

// RateLimiter implements a token bucket shared across concurrent handlers.
type RateLimiter struct {
	tokens chan struct{}
	ticker *time.Ticker
	stop   chan struct{}
}

func NewRateLimiter(rate time.Duration, burst int) *RateLimiter {
	rl := &RateLimiter{
		tokens: make(chan struct{}, burst),
		ticker: time.NewTicker(rate),
		stop:   make(chan struct{}),
	}
	for i := 0; i < burst; i++ {
		rl.tokens <- struct{}{}
	}
	go rl.refill()
	return rl
}

func (rl *RateLimiter) refill() {
	for {
		select {
		case <-rl.ticker.C:
			select {
			case rl.tokens <- struct{}{}:
			default:
			}
		case <-rl.stop:
			return
		}
	}
}

func (rl *RateLimiter) Wait() { <-rl.tokens }
func (rl *RateLimiter) Stop() { rl.ticker.Stop(); close(rl.stop) }

// RateLimitedServer simulates concurrent API handlers sharing a rate limiter.
type RateLimitedServer struct {
	limiter    *RateLimiter
	numHandlers int
}

func NewRateLimitedServer(rate time.Duration, burst, handlers int) *RateLimitedServer {
	return &RateLimitedServer{
		limiter:    NewRateLimiter(rate, burst),
		numHandlers: handlers,
	}
}

func (s *RateLimitedServer) handleRequest(reqID int, start time.Time, wg *sync.WaitGroup) {
	defer wg.Done()
	s.limiter.Wait()
	elapsed := time.Since(start).Round(time.Millisecond)
	fmt.Printf("  [%6v] handler processed request %d\n", elapsed, reqID)
	time.Sleep(handlerWorkTime)
}

func (s *RateLimitedServer) Serve() {
	fmt.Printf("=== Rate-Limited API Server (10 req/sec, burst=%d, %d handlers) ===\n\n",
		serverBurst, s.numHandlers)

	start := time.Now()
	var wg sync.WaitGroup

	for i := 1; i <= s.numHandlers; i++ {
		wg.Add(1)
		go s.handleRequest(i, start, &wg)
	}

	wg.Wait()
	total := time.Since(start)
	fmt.Printf("\n  %d requests processed in %v\n", s.numHandlers, total)
	fmt.Printf("  Effective rate: %.1f req/sec\n", float64(s.numHandlers)/total.Seconds())
	s.limiter.Stop()
}

func main() {
	server := NewRateLimitedServer(serverRate, serverBurst, concurrentHandlers)
	server.Serve()
}
```

All 20 goroutines launch immediately, but they are throttled by the shared rate limiter.

### Verification
```bash
go run main.go
```
Expected: first 3 instant (burst), then ~100ms between each:
```
=== Rate-Limited API Server (10 req/sec, burst=3, 20 handlers) ===

  [   0ms] handler processed request 3
  [   0ms] handler processed request 1
  [   0ms] handler processed request 2
  [ 100ms] handler processed request 5
  [ 200ms] handler processed request 4
  ...

  20 requests processed in 1.7s
  Effective rate: 11.8 req/sec
```

## Common Mistakes

### Forgetting to Stop the Ticker
**Wrong:**
```go
ticker := time.NewTicker(100 * time.Millisecond)
// use ticker...
// forgot ticker.Stop()
```
**What happens:** The ticker goroutine leaks, continuously ticking and trying to fill the buffer.

**Fix:** Always call `ticker.Stop()` when done. Use `defer` to ensure cleanup.

### Not Using Default in the Refiller
**Wrong:**
```go
for range ticker.C {
	tokens <- struct{}{} // blocks if bucket is full!
}
```
**What happens:** The refiller goroutine blocks when the bucket is full, and tokens from subsequent ticks are lost (they back up in the ticker channel).

**Fix:** Use `select` with `default` to discard excess tokens: `select { case tokens <- struct{}{}: default: }`

### Setting Burst to Zero
**Wrong:**
```go
tokens := make(chan struct{}, 0) // unbuffered = no burst
```
**What happens:** The channel cannot hold any tokens. The refiller blocks on every send, and the rate becomes erratic.

**Fix:** Burst must be at least 1. The buffer capacity determines how many tokens can accumulate.

## Review

The token bucket falls out of Go's primitives with almost no glue: a buffered channel is the bucket, its capacity is the burst size, a `time.Ticker` is the refill clock whose interval sets the steady-state rate, and receiving from the channel is how a request spends a token. Two details make it behave. Pre-filling the channel to capacity gives the initial burst, so a client that has been idle can fire several requests instantly before pacing kicks in. And the refiller must use `select`/`default` when adding a token, so that a full bucket silently drops the extra rather than blocking the refill goroutine and letting ticks back up. On top of that sits the acquisition choice: `Wait()` blocks until a token frees up (queue the excess), while `TryAcquire()` returns immediately and lets you reject the excess with a 429. Because the limiter is just a shared channel, any number of concurrent handlers can drain the same bucket and be throttled to one global rate -- and because the ticker owns a runtime timer, you must `Stop()` it to avoid a leak.

Confirm the behavior end to end from a single run. The basic limiter should space requests about 200ms apart; the token bucket should serve the first three instantly and then pace the rest around 100ms; blocking mode should eventually serve everything while non-blocking mode rejects the overflow with 429 and then accepts again once the bucket refills after an idle pause; and twenty concurrent handlers sharing one limiter should settle to roughly a 10/sec effective rate. If you can point at which knob -- buffer capacity or ticker interval -- controls each of those numbers, the algorithm is yours.

## Resources

- [Go by Example: Rate Limiting](https://gobyexample.com/rate-limiting) -- the minimal channel-and-ticker limiter this exercise expands on.
- [Token Bucket Algorithm (Wikipedia)](https://en.wikipedia.org/wiki/Token_bucket) -- the general algorithm and the rate-versus-burst distinction it encodes.
- [golang.org/x/time/rate](https://pkg.go.dev/golang.org/x/time/rate) -- the production-grade limiter in the extended standard library, for when you outgrow the hand-rolled version.

---

Back to [Concurrency](../../concurrency.md) | Next: [10-end-to-end-pipeline-with-cancel](../10-end-to-end-pipeline-with-cancel/10-end-to-end-pipeline-with-cancel.md)
