# 12. Distributed Rate Limiter

A single-node rate limiter is straightforward: track requests per client, reject those that exceed the limit. A distributed system complicates this because each of N gateway nodes sees only a fraction of the traffic. Without coordination, a client can exceed the global limit by spreading requests evenly across all nodes — each node accepts the request because its local count is within its local quota. This lesson builds the core algorithms (token bucket and sliding window), implements three coordination strategies (local-division, centralized, gossip-based), and shows where each breaks.

```text
ratelimiter/
  go.mod
  limiter.go
  limiter_test.go
  cmd/demo/main.go
```

The package exports `TokenBucket`, `SlidingWindow`, and `GossipLimiter`. Tests are hermetic and do not touch the network.

## Concepts

### Token Bucket

A token bucket holds up to `capacity` tokens. Tokens are added at a fixed `rate` (tokens per second). Each admitted request consumes one token. If the bucket is empty, the request is rejected. Because the bucket can accumulate up to `capacity` tokens when traffic is absent, it absorbs short bursts.

The key invariant: at any wall-clock time `t`, the number of available tokens is:

```
available = min(capacity, last_tokens + rate * (t - last_refill_time))
```

An atomic implementation computes the available tokens on each `Allow` call from the last-stored count and the elapsed time, then uses a compare-and-swap to update the stored state. No background goroutine is required.

### Sliding Window Counter

A sliding window counter divides time into fixed-width buckets (for example, one-second buckets). The current count is a weighted sum of the counts in the current bucket and the immediately preceding bucket, weighted by how far through the current bucket we are:

```
count = prev_bucket_count * (1 - fraction_of_current_bucket_elapsed)
      + current_bucket_count
```

This approximation avoids storing every individual request timestamp (which would be an `O(n)` sliding log) while being more accurate than a pure fixed-window counter, which can allow a burst of `2 * limit` at the window boundary.

### Three Coordination Strategies

**Local division.** Each of N nodes enforces `global_limit / N` independently. Admission decisions are instantaneous (no network). The failure mode: uneven traffic distribution. If one node handles 80% of traffic and enforces only `global_limit / N`, that node admits too many requests while other nodes sit idle. Under even traffic this is accurate; under skewed traffic it over-admits.

**Centralized.** A single coordinator tracks the global count. Nodes send a token-acquisition request to the coordinator for each incoming request. Admission decisions are globally accurate. The failure mode: the coordinator is a single point of failure and adds at least one network round-trip of latency per decision. Batched pre-fetching (a node pre-fetches M tokens at a time, caching them locally) reduces round-trips at the cost of brief over-admission when the node crashes with pre-fetched tokens unconsumed.

**Gossip-based.** Each node tracks its own local count and receives periodic count updates from peers. The node estimates the global total as the sum of its local count and the last-known counts from all peers, then rejects a request if the estimated global total would exceed the limit. The failure mode: convergence delay. A node's estimate is stale by up to one gossip interval. During that interval the system can over-admit. The gossip interval is a tunable accuracy/latency knob.

### Accuracy vs Latency

No coordination strategy is strictly better than the others; they trade accuracy for latency:

| Strategy       | Accuracy                         | Latency overhead | SPOF |
|----------------|----------------------------------|------------------|------|
| Local division | Low under skewed traffic         | None             | None |
| Centralized    | Exact                            | 1 round-trip     | Yes  |
| Gossip         | Approximate (lag = gossip interval) | None (async)  | None |

## Exercises

Set up the module:

```bash
mkdir -p ~/go-exercises/ratelimiter/cmd/demo
cd ~/go-exercises/ratelimiter
go mod init example.com/ratelimiter
```

This is a library. Verify it with `go test`.

### Exercise 1: Token Bucket

Create `limiter.go`:

```go
package ratelimiter

import (
	"errors"
	"fmt"
	"sync"
	"time"
)

// ErrRateLimited is returned when a request is rejected by the limiter.
var ErrRateLimited = errors.New("rate limited")

// ErrInvalidRate is returned when the rate or capacity is not positive.
var ErrInvalidRate = errors.New("rate and capacity must be positive")

// TokenBucket is a thread-safe token-bucket rate limiter.
// Tokens are replenished continuously at the configured rate.
// The bucket holds at most Capacity tokens; burst requests consume tokens
// until the bucket is empty.
type TokenBucket struct {
	rate     float64 // tokens per second
	capacity float64 // maximum tokens

	mu       sync.Mutex
	tokens   float64
	lastFill time.Time
}

// NewTokenBucket creates a TokenBucket that allows rate tokens per second
// with a burst capacity of capacity tokens.
func NewTokenBucket(rate, capacity float64) (*TokenBucket, error) {
	if rate <= 0 || capacity <= 0 {
		return nil, fmt.Errorf("ratelimiter: %w", ErrInvalidRate)
	}
	return &TokenBucket{
		rate:     rate,
		capacity: capacity,
		tokens:   capacity,
		lastFill: time.Now(),
	}, nil
}

// Allow returns nil if a token is available and consumes it; otherwise it
// returns a wrapped ErrRateLimited.
func (tb *TokenBucket) Allow() error {
	tb.mu.Lock()
	defer tb.mu.Unlock()

	now := time.Now()
	elapsed := now.Sub(tb.lastFill).Seconds()
	tb.tokens += elapsed * tb.rate
	if tb.tokens > tb.capacity {
		tb.tokens = tb.capacity
	}
	tb.lastFill = now

	if tb.tokens < 1 {
		return fmt.Errorf("token bucket: %w", ErrRateLimited)
	}
	tb.tokens--
	return nil
}

// Available returns the current token count (approximate; useful for tests
// and diagnostics).
func (tb *TokenBucket) Available() float64 {
	tb.mu.Lock()
	defer tb.mu.Unlock()

	elapsed := time.Since(tb.lastFill).Seconds()
	avail := tb.tokens + elapsed*tb.rate
	if avail > tb.capacity {
		avail = tb.capacity
	}
	return avail
}
```

`NewTokenBucket` validates inputs and returns a sentinel-wrapped error. `Allow` refills tokens based on elapsed wall time on each call — no background goroutine is required.

### Exercise 2: Sliding Window Counter

Append to `limiter.go`:

```go
// SlidingWindow is a thread-safe approximate sliding-window rate limiter.
// It divides time into fixed one-second buckets and uses a weighted sum of
// the current and previous bucket counts to approximate the true request rate.
type SlidingWindow struct {
	limit int64 // maximum requests per second

	mu        sync.Mutex
	curBucket int64 // unix second of the current bucket
	curCount  int64
	prevCount int64
}

// NewSlidingWindow creates a SlidingWindow that allows at most limit requests
// per second.
func NewSlidingWindow(limit int64) (*SlidingWindow, error) {
	if limit <= 0 {
		return nil, fmt.Errorf("ratelimiter: sliding window: %w", ErrInvalidRate)
	}
	return &SlidingWindow{limit: limit}, nil
}

// Allow returns nil if the request is within the sliding-window limit;
// otherwise it returns a wrapped ErrRateLimited.
func (sw *SlidingWindow) Allow() error {
	return sw.allowAt(time.Now())
}

func (sw *SlidingWindow) allowAt(now time.Time) error {
	sw.mu.Lock()
	defer sw.mu.Unlock()

	bucket := now.Unix()
	fraction := float64(now.UnixNano()%int64(time.Second)) / float64(time.Second)

	switch {
	case bucket == sw.curBucket:
		// still in the same second
	case bucket == sw.curBucket+1:
		// rolled into next second
		sw.prevCount = sw.curCount
		sw.curCount = 0
		sw.curBucket = bucket
	default:
		// gap of two or more seconds: reset
		sw.prevCount = 0
		sw.curCount = 0
		sw.curBucket = bucket
	}

	estimated := float64(sw.prevCount)*(1-fraction) + float64(sw.curCount)
	if int64(estimated)+1 > sw.limit {
		return fmt.Errorf("sliding window: %w", ErrRateLimited)
	}
	sw.curCount++
	return nil
}
```

### Exercise 3: Per-Client Limiter and Gossip-Based Global Estimate

Append to `limiter.go`:

```go
// ClientLimiter tracks an independent TokenBucket per client identifier
// (API key, IP address, etc.).
type ClientLimiter struct {
	rate     float64
	capacity float64

	mu      sync.Mutex
	clients map[string]*TokenBucket
}

// NewClientLimiter creates a per-client rate limiter.
func NewClientLimiter(rate, capacity float64) (*ClientLimiter, error) {
	if rate <= 0 || capacity <= 0 {
		return nil, fmt.Errorf("ratelimiter: client limiter: %w", ErrInvalidRate)
	}
	return &ClientLimiter{
		rate:     rate,
		capacity: capacity,
		clients:  make(map[string]*TokenBucket),
	}, nil
}

// Allow returns nil if the named client is within its rate limit.
func (cl *ClientLimiter) Allow(clientID string) error {
	cl.mu.Lock()
	tb, ok := cl.clients[clientID]
	if !ok {
		var err error
		tb, err = NewTokenBucket(cl.rate, cl.capacity)
		if err != nil {
			cl.mu.Unlock()
			return err
		}
		cl.clients[clientID] = tb
	}
	cl.mu.Unlock()
	return tb.Allow()
}

// GossipLimiter implements a gossip-based global rate limiter.
// Each node maintains a local count and receives count updates from peers
// via AcceptGossip. Allow rejects a request when the estimated global total
// (local + last-known peer counts) would exceed the limit.
type GossipLimiter struct {
	limit int64 // global request limit per gossip window

	mu         sync.Mutex
	localCount int64
	peerCounts map[string]int64 // nodeID -> count received from that peer
}

// NewGossipLimiter creates a GossipLimiter for a cluster of the given global
// limit.
func NewGossipLimiter(globalLimit int64) (*GossipLimiter, error) {
	if globalLimit <= 0 {
		return nil, fmt.Errorf("ratelimiter: gossip: %w", ErrInvalidRate)
	}
	return &GossipLimiter{
		limit:      globalLimit,
		peerCounts: make(map[string]int64),
	}, nil
}

// Allow returns nil when the estimated global count is below the limit.
func (g *GossipLimiter) Allow() error {
	g.mu.Lock()
	defer g.mu.Unlock()

	estimated := g.localCount
	for _, c := range g.peerCounts {
		estimated += c
	}
	if estimated >= g.limit {
		return fmt.Errorf("gossip limiter: %w", ErrRateLimited)
	}
	g.localCount++
	return nil
}

// AcceptGossip records the latest count received from a peer node.
// In a real cluster this is called by the gossip receive path when a peer
// broadcasts its local count.
func (g *GossipLimiter) AcceptGossip(nodeID string, count int64) {
	g.mu.Lock()
	g.peerCounts[nodeID] = count
	g.mu.Unlock()
}

// LocalCount returns this node's local request count (for broadcasting to peers).
func (g *GossipLimiter) LocalCount() int64 {
	g.mu.Lock()
	defer g.mu.Unlock()
	return g.localCount
}

// Reset resets the local count and all peer counts to zero (call at the start
// of each gossip window).
func (g *GossipLimiter) Reset() {
	g.mu.Lock()
	g.localCount = 0
	for k := range g.peerCounts {
		g.peerCounts[k] = 0
	}
	g.mu.Unlock()
}
```

### Exercise 4: Tests

Create `limiter_test.go`:

```go
package ratelimiter

import (
	"errors"
	"fmt"
	"testing"
	"time"
)

// -- TokenBucket tests -------------------------------------------------------

func TestTokenBucketRejectsInvalidArgs(t *testing.T) {
	t.Parallel()

	cases := []struct {
		name           string
		rate, capacity float64
	}{
		{"zero rate", 0, 10},
		{"negative rate", -1, 10},
		{"zero capacity", 5, 0},
		{"both zero", 0, 0},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			_, err := NewTokenBucket(tc.rate, tc.capacity)
			if !errors.Is(err, ErrInvalidRate) {
				t.Fatalf("NewTokenBucket(%v, %v): err = %v, want ErrInvalidRate",
					tc.rate, tc.capacity, err)
			}
		})
	}
}

func TestTokenBucketAllowsUpToCapacity(t *testing.T) {
	t.Parallel()

	tb, err := NewTokenBucket(1, 5)
	if err != nil {
		t.Fatal(err)
	}
	// bucket starts full (capacity=5), so five requests must be allowed
	for i := range 5 {
		if err := tb.Allow(); err != nil {
			t.Fatalf("request %d rejected unexpectedly: %v", i, err)
		}
	}
	// sixth request must be rejected
	if err := tb.Allow(); !errors.Is(err, ErrRateLimited) {
		t.Fatalf("Allow() = %v, want ErrRateLimited", err)
	}
}

func TestTokenBucketRejectsWhenEmpty(t *testing.T) {
	t.Parallel()

	// capacity=1, rate=0.001 (very slow refill)
	tb, err := NewTokenBucket(0.001, 1)
	if err != nil {
		t.Fatal(err)
	}
	if err := tb.Allow(); err != nil {
		t.Fatalf("first Allow() failed: %v", err)
	}
	if err := tb.Allow(); !errors.Is(err, ErrRateLimited) {
		t.Fatalf("Allow() = %v, want ErrRateLimited", err)
	}
}

func ExampleNewTokenBucket() {
	tb, err := NewTokenBucket(10, 2)
	if err != nil {
		panic(err)
	}
	// bucket starts full (capacity=2)
	fmt.Println(tb.Allow() == nil)                     // first token consumed
	fmt.Println(tb.Allow() == nil)                     // second token consumed
	fmt.Println(errors.Is(tb.Allow(), ErrRateLimited)) // empty
	// Output:
	// true
	// true
	// true
}

// -- SlidingWindow tests -----------------------------------------------------

func TestSlidingWindowRejectsInvalidArgs(t *testing.T) {
	t.Parallel()

	if _, err := NewSlidingWindow(0); !errors.Is(err, ErrInvalidRate) {
		t.Fatalf("err = %v, want ErrInvalidRate", err)
	}
}

func TestSlidingWindowAllowsUpToLimit(t *testing.T) {
	t.Parallel()

	sw, err := NewSlidingWindow(3)
	if err != nil {
		t.Fatal(err)
	}
	// all requests arrive at the same instant in the same bucket
	now := time.Unix(1_000_000, 500_000_000) // 0.5 s into a second
	for i := range 3 {
		if err := sw.allowAt(now); err != nil {
			t.Fatalf("request %d rejected: %v", i, err)
		}
	}
	if err := sw.allowAt(now); !errors.Is(err, ErrRateLimited) {
		t.Fatalf("allowAt() = %v, want ErrRateLimited", err)
	}
}

func TestSlidingWindowResetsAfterTwoSeconds(t *testing.T) {
	t.Parallel()

	sw, err := NewSlidingWindow(2)
	if err != nil {
		t.Fatal(err)
	}
	t0 := time.Unix(1_000_000, 0)
	// fill the first bucket
	if err := sw.allowAt(t0); err != nil {
		t.Fatal(err)
	}
	if err := sw.allowAt(t0); err != nil {
		t.Fatal(err)
	}
	// jump two full seconds forward — gap resets counters
	t2 := t0.Add(2 * time.Second)
	if err := sw.allowAt(t2); err != nil {
		t.Fatalf("after 2s gap: %v", err)
	}
}

// -- ClientLimiter tests -----------------------------------------------------

func TestClientLimiterIsolatesClients(t *testing.T) {
	t.Parallel()

	cl, err := NewClientLimiter(1, 2)
	if err != nil {
		t.Fatal(err)
	}
	// alice gets 2 tokens (capacity=2)
	if err := cl.Allow("alice"); err != nil {
		t.Fatal(err)
	}
	if err := cl.Allow("alice"); err != nil {
		t.Fatal(err)
	}
	if err := cl.Allow("alice"); !errors.Is(err, ErrRateLimited) {
		t.Fatalf("alice 3rd Allow() = %v, want ErrRateLimited", err)
	}
	// bob's bucket is independent; his first request must succeed
	if err := cl.Allow("bob"); err != nil {
		t.Fatalf("bob Allow() = %v", err)
	}
}

func TestClientLimiterRejectsInvalidArgs(t *testing.T) {
	t.Parallel()

	if _, err := NewClientLimiter(0, 1); !errors.Is(err, ErrInvalidRate) {
		t.Fatalf("err = %v, want ErrInvalidRate", err)
	}
}

// -- GossipLimiter tests -----------------------------------------------------

func TestGossipLimiterRejectsInvalidArgs(t *testing.T) {
	t.Parallel()

	if _, err := NewGossipLimiter(0); !errors.Is(err, ErrInvalidRate) {
		t.Fatalf("err = %v, want ErrInvalidRate", err)
	}
}

func TestGossipLimiterLocalOnly(t *testing.T) {
	t.Parallel()

	gl, err := NewGossipLimiter(3)
	if err != nil {
		t.Fatal(err)
	}
	for i := range 3 {
		if err := gl.Allow(); err != nil {
			t.Fatalf("request %d: %v", i, err)
		}
	}
	if err := gl.Allow(); !errors.Is(err, ErrRateLimited) {
		t.Fatalf("Allow() = %v, want ErrRateLimited", err)
	}
}

func TestGossipLimiterAccountsForPeers(t *testing.T) {
	t.Parallel()

	// global limit = 5; one peer has already consumed 4
	gl, err := NewGossipLimiter(5)
	if err != nil {
		t.Fatal(err)
	}
	gl.AcceptGossip("node-B", 4)

	// local count = 0, peer count = 4, estimated = 4 < 5 → allow
	if err := gl.Allow(); err != nil {
		t.Fatalf("first Allow() with peer count 4: %v", err)
	}
	// now estimated = 5 >= 5 → reject
	if err := gl.Allow(); !errors.Is(err, ErrRateLimited) {
		t.Fatalf("Allow() = %v, want ErrRateLimited", err)
	}
}

func TestGossipLimiterResetClearsCounters(t *testing.T) {
	t.Parallel()

	gl, err := NewGossipLimiter(2)
	if err != nil {
		t.Fatal(err)
	}
	if err := gl.Allow(); err != nil {
		t.Fatal(err)
	}
	if err := gl.Allow(); err != nil {
		t.Fatal(err)
	}
	// full; reset starts a new window
	gl.Reset()
	if err := gl.Allow(); err != nil {
		t.Fatalf("after Reset Allow() = %v", err)
	}
}
```

Your turn: add `TestGossipLimiterLocalCountIncrementsOnAllow` — call `Allow` three times on a fresh `GossipLimiter` and assert that `LocalCount()` equals 3 after the three calls.

### Exercise 5: Demo

Create `cmd/demo/main.go`:

```go
package main

import (
	"errors"
	"fmt"
	"log"

	"example.com/ratelimiter"
)

func main() {
	// Token bucket: 2 requests/s, burst of 3
	tb, err := ratelimiter.NewTokenBucket(2, 3)
	if err != nil {
		log.Fatal(err)
	}

	fmt.Println("=== Token Bucket (rate=2/s, capacity=3) ===")
	for i := range 5 {
		result := "OK"
		if err := tb.Allow(); errors.Is(err, ratelimiter.ErrRateLimited) {
			result = "RATE LIMITED"
		}
		fmt.Printf("  request %d: %s\n", i+1, result)
	}

	// Sliding window: 3 requests/s
	sw, err := ratelimiter.NewSlidingWindow(3)
	if err != nil {
		log.Fatal(err)
	}
	fmt.Println("\n=== Sliding Window (limit=3/s) ===")
	for i := range 5 {
		result := "OK"
		if err := sw.Allow(); errors.Is(err, ratelimiter.ErrRateLimited) {
			result = "RATE LIMITED"
		}
		fmt.Printf("  request %d: %s\n", i+1, result)
	}

	// Per-client limiter: 1/s, burst=2
	cl, err := ratelimiter.NewClientLimiter(1, 2)
	if err != nil {
		log.Fatal(err)
	}
	fmt.Println("\n=== Per-Client Limiter (rate=1/s, capacity=2) ===")
	for _, client := range []string{"alice", "alice", "alice", "bob"} {
		result := "OK"
		if err := cl.Allow(client); errors.Is(err, ratelimiter.ErrRateLimited) {
			result = "RATE LIMITED"
		}
		fmt.Printf("  %-6s: %s\n", client, result)
	}

	// Gossip limiter: global limit=4, simulating two nodes
	gl, err := ratelimiter.NewGossipLimiter(4)
	if err != nil {
		log.Fatal(err)
	}
	// simulate peer having consumed 2 tokens in this window
	gl.AcceptGossip("node-B", 2)
	fmt.Println("\n=== Gossip Limiter (globalLimit=4, peer=2) ===")
	for i := range 4 {
		result := "OK"
		if err := gl.Allow(); errors.Is(err, ratelimiter.ErrRateLimited) {
			result = "RATE LIMITED"
		}
		fmt.Printf("  request %d: %s  (localCount=%d)\n",
			i+1, result, gl.LocalCount())
	}
	fmt.Printf("  window ends — Reset\n")
	gl.Reset()
}
```

## Common Mistakes

### Refilling Tokens With A Background Goroutine

Wrong: start a `time.Ticker` goroutine that adds tokens to the bucket every interval. This creates a goroutine per limiter, leaks when the limiter is abandoned, and complicates testing (tests must wait for ticks).

Fix: compute the token refill lazily on each `Allow` call from the elapsed wall time. The lesson's `TokenBucket.Allow` does this: `elapsed := now.Sub(tb.lastFill).Seconds(); tb.tokens += elapsed * tb.rate`.

### Using A Fixed Window Instead Of A Sliding Window

Wrong: reset the counter to zero at the start of each fixed second. A client can send `limit` requests at second 0.99 and `limit` more at second 1.01, exceeding the rate by 2x at the boundary.

Fix: use a weighted sum of the current and previous bucket, as `SlidingWindow.allowAt` does. The weighted estimate smooths the boundary burst.

### Racing On The Token Bucket Without A Lock

Wrong: read `tb.tokens` and `tb.lastFill` outside a mutex, compute new tokens, then write them back. Two goroutines can race and both see the bucket as non-empty for the same token.

Fix: hold `tb.mu` for the entire read-modify-write in `Allow`. The lesson does this. Alternatively, a 128-bit CAS over `(tokens, lastFill)` works but is not portable; `sync.Mutex` is the idiomatic choice.

### Gossip Estimate Being Permanently Stale

Wrong: never resetting peer counts between gossip windows. A peer that went offline after broadcasting a high count keeps the estimate inflated forever.

Fix: call `Reset()` at the start of each gossip window to zero all counters. The lesson's `GossipLimiter.Reset` does this.

### Not Wrapping Sentinel Errors With `%w`

Wrong: `return ErrRateLimited`. The caller cannot distinguish "rate limited by which limiter" without string matching, and wrapping in a higher-level error breaks `errors.Is`.

Fix: `return fmt.Errorf("token bucket: %w", ErrRateLimited)`. The lesson wraps every sentinel. The test asserts with `errors.Is(err, ErrRateLimited)`, which unwraps the chain.

## Verification

From `~/go-exercises/ratelimiter`:

```bash
test -z "$(gofmt -l .)"
go vet ./...
go build ./...
go test -count=1 -race ./...
```

All four must pass. Run the demo with:

```bash
go run ./cmd/demo
```

## Summary

- A token bucket admits up to `capacity` burst requests and replenishes tokens at `rate` tokens/second; compute the refill lazily on each `Allow` call from elapsed wall time.
- A sliding window counter weights the current and previous one-second bucket to smooth the fixed-window boundary burst.
- Local-division coordination requires no network but over-admits under skewed traffic; each node enforces only `global_limit / N`.
- Centralized coordination is globally accurate but adds one network round-trip per decision and creates a single point of failure; batched pre-fetching reduces round-trips.
- Gossip-based coordination adds no per-request latency but has a convergence lag equal to the gossip interval; `Reset` at each window boundary prevents stale peer counts from permanently inflating the estimate.
- Per-client limiting isolates client buckets so one heavy client does not starve others.

## What's Next

Next: [Sharded Key-Value Store](../13-sharded-key-value-store/13-sharded-key-value-store.md).

## Resources

- [pkg.go.dev/sync](https://pkg.go.dev/sync) — Mutex and the memory model for synchronized access
- [pkg.go.dev/time](https://pkg.go.dev/time) — Time, Duration, and Since used in token-bucket refill
- [Go Blog: Concurrency Patterns (pipelines and cancellation)](https://go.dev/blog/pipelines) — canonical Go concurrency model underlying rate-limiter design
- [Stripe Engineering: Rate Limiters](https://stripe.com/blog/rate-limiters) — practical token bucket and sliding window strategies in production
- [Google Cloud: Rate limiting strategies and techniques](https://cloud.google.com/architecture/rate-limiting-strategies-techniques) — centralized vs. distributed approaches with accuracy/latency analysis
