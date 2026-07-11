# Exercise 32: Channel-Based Request Coalescing (Singleflight)

A popular user's profile is cached with a 60-second TTL. At second 61 the cache
expires, and in the next 50 milliseconds 200 requests arrive for the same key.
Without coalescing all 200 hit the database at once -- a thundering herd that
spikes latency for everyone and can cascade into failure. This exercise builds
request coalescing (also called singleflight) with channels: the first request
for a key triggers the real lookup, every concurrent request for the same key is
parked until it completes, and one result fans out to all of them. Two hundred
queries become one.

## What you'll build

```text
32-channel-request-coalescing/
  main.go        a no-coalescing baseline, single-key and multi-key
                 coalescers, and a side-by-side metrics comparison
```

- Build: a channel-native singleflight that deduplicates concurrent lookups per key.
- Implement: a central `coalescer` goroutine holding an in-flight map of key to waiter reply channels; per-key lookup goroutines that report back over a results channel; a baseline and a benchmarked comparison.
- Verify: `go run main.go`, and `go run -race main.go` for the coalescer steps.

### Why a central goroutine, not a mutex

Request coalescing solves the stampede: the first request for a given key
triggers the actual database lookup, and all subsequent requests for the same
key that arrive while the first is still in flight are "parked" -- their reply
channels collected in a waiters list. When the lookup completes, the result is
sent to all parked reply channels at once. Instead of 200 database queries,
exactly 1 is executed.

The standard library's `sync/singleflight` package provides this with mutexes.
But implementing it with channels reveals the underlying mechanics: a central
goroutine receives all lookup requests, maintains a map of in-flight keys to
waiter lists, launches lookups, and distributes results. This is the
channel-native approach to deduplication.

## Step 1 -- No Coalescing Baseline

First, measure the problem. Simulate 50 concurrent requests for the same cache key with no deduplication. Count how many database calls are made.

```go
package main

import (
	"fmt"
	"sync"
	"sync/atomic"
	"time"
)

const (
	baselineConcurrency = 50
	dbLookupDelay       = 50 * time.Millisecond
)

var dbCallCount atomic.Int64

// UserProfile represents a cached database record.
type UserProfile struct {
	Name  string
	Email string
}

// dbLookup simulates a slow database query.
func dbLookup(key string) UserProfile {
	dbCallCount.Add(1)
	time.Sleep(dbLookupDelay)
	return UserProfile{
		Name:  "Alice Johnson",
		Email: fmt.Sprintf("%s@example.com", key),
	}
}

// LookupRequest represents a client asking for a profile by key.
type LookupRequest struct {
	Key   string
	Reply chan UserProfile
}

// NewLookupRequest creates a request with an initialized reply channel.
func NewLookupRequest(key string) LookupRequest {
	return LookupRequest{Key: key, Reply: make(chan UserProfile, 1)}
}

func main() {
	fmt.Println("=== No Coalescing Baseline ===")
	dbCallCount.Store(0)
	epoch := time.Now()

	var wg sync.WaitGroup
	for i := 0; i < baselineConcurrency; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			profile := dbLookup("alice")
			_ = profile
		}()
	}
	wg.Wait()

	elapsed := time.Since(epoch).Round(time.Millisecond)
	fmt.Printf("  concurrent requests: %d\n", baselineConcurrency)
	fmt.Printf("  db calls made:       %d\n", dbCallCount.Load())
	fmt.Printf("  wall time:           %v\n", elapsed)
	fmt.Printf("  result: every request triggered a separate DB call\n")
}
```

### Verification
```bash
go run main.go
```
Expected output:
```
=== No Coalescing Baseline ===
  concurrent requests: 50
  db calls made:       50
  wall time:           50ms
  result: every request triggered a separate DB call
```
All 50 requests hit the database. With a real database under load, this would cause significant latency.

## Step 2 -- Single-Key Coalescing

Implement a coalescer goroutine that deduplicates requests for the same key. The first request triggers a lookup. Subsequent requests for the same key are parked until the lookup completes.

```go
package main

import (
	"fmt"
	"sync"
	"sync/atomic"
	"time"
)

const (
	coalesceConcurrency = 50
	coalesceDBDelay     = 50 * time.Millisecond
)

var coalesceDBCalls atomic.Int64

type UserProfile struct {
	Name  string
	Email string
}

func dbLookup(key string) UserProfile {
	coalesceDBCalls.Add(1)
	time.Sleep(coalesceDBDelay)
	return UserProfile{
		Name:  "Alice Johnson",
		Email: fmt.Sprintf("%s@example.com", key),
	}
}

type LookupRequest struct {
	Key   string
	Reply chan UserProfile
}

func NewLookupRequest(key string) LookupRequest {
	return LookupRequest{Key: key, Reply: make(chan UserProfile, 1)}
}

// LookupResult is sent by the lookup goroutine back to the coalescer.
type LookupResult struct {
	Key     string
	Profile UserProfile
}

// coalescer is the central goroutine that deduplicates in-flight lookups.
// It receives requests on intake, parks waiters for in-flight keys,
// and distributes results when lookups complete.
func coalescer(intake <-chan LookupRequest, done <-chan struct{}) {
	inFlight := make(map[string][]chan UserProfile)
	results := make(chan LookupResult, 10)

	for {
		select {
		case <-done:
			return

		case req := <-intake:
			waiters, exists := inFlight[req.Key]
			inFlight[req.Key] = append(waiters, req.Reply)

			if !exists {
				go func(key string) {
					profile := dbLookup(key)
					results <- LookupResult{Key: key, Profile: profile}
				}(req.Key)
			}

		case result := <-results:
			waiters := inFlight[result.Key]
			delete(inFlight, result.Key)
			for _, reply := range waiters {
				reply <- result.Profile
			}
		}
	}
}

func main() {
	fmt.Println("=== Single-Key Coalescing ===")
	coalesceDBCalls.Store(0)
	epoch := time.Now()

	intake := make(chan LookupRequest, coalesceConcurrency)
	done := make(chan struct{})
	go coalescer(intake, done)

	var wg sync.WaitGroup
	for i := 0; i < coalesceConcurrency; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			req := NewLookupRequest("alice")
			intake <- req
			profile := <-req.Reply
			_ = profile
		}()
	}
	wg.Wait()

	elapsed := time.Since(epoch).Round(time.Millisecond)
	fmt.Printf("  concurrent requests: %d\n", coalesceConcurrency)
	fmt.Printf("  db calls made:       %d\n", coalesceDBCalls.Load())
	fmt.Printf("  wall time:           %v\n", elapsed)
	fmt.Printf("  reduction:           %.0f%%\n",
		(1-float64(coalesceDBCalls.Load())/float64(coalesceConcurrency))*100)

	close(done)
}
```

How it works:
- `inFlight` maps each key to a list of waiting reply channels
- When a request arrives for a key NOT in `inFlight`, it is the first -- a lookup goroutine is launched
- When a request arrives for a key already in `inFlight`, the reply channel is appended to the waiters list
- When a `LookupResult` arrives, all parked reply channels for that key receive the result
- The key is deleted from `inFlight`, so the next request for that key will trigger a fresh lookup

### Verification
```bash
go run -race main.go
```
Expected output:
```
=== Single-Key Coalescing ===
  concurrent requests: 50
  db calls made:       1
  wall time:           50ms
  reduction:           98%
```
From 50 database calls down to 1. All 50 callers get the same result.

## Step 3 -- Multi-Key Concurrent Coalescing

Extend to handle multiple keys concurrently. Requests for "alice", "bob", and "carol" arrive simultaneously. Each key should trigger exactly one lookup, regardless of how many concurrent requests exist per key.

```go
package main

import (
	"fmt"
	"sync"
	"sync/atomic"
	"time"
)

const (
	multiKeyRequestsPerKey = 20
	multiKeyDBDelay        = 50 * time.Millisecond
)

var multiKeyDBCalls atomic.Int64

type UserProfile struct {
	Name  string
	Email string
}

func dbLookup(key string) UserProfile {
	multiKeyDBCalls.Add(1)
	time.Sleep(multiKeyDBDelay)
	return UserProfile{
		Name:  fmt.Sprintf("User %s", key),
		Email: fmt.Sprintf("%s@example.com", key),
	}
}

type LookupRequest struct {
	Key   string
	Reply chan UserProfile
}

func NewLookupRequest(key string) LookupRequest {
	return LookupRequest{Key: key, Reply: make(chan UserProfile, 1)}
}

type LookupResult struct {
	Key     string
	Profile UserProfile
}

func coalescer(intake <-chan LookupRequest, done <-chan struct{}) {
	inFlight := make(map[string][]chan UserProfile)
	results := make(chan LookupResult, 10)

	for {
		select {
		case <-done:
			return

		case req := <-intake:
			waiters, exists := inFlight[req.Key]
			inFlight[req.Key] = append(waiters, req.Reply)
			if !exists {
				go func(key string) {
					profile := dbLookup(key)
					results <- LookupResult{Key: key, Profile: profile}
				}(req.Key)
			}

		case result := <-results:
			waiters := inFlight[result.Key]
			delete(inFlight, result.Key)
			for _, reply := range waiters {
				reply <- result.Profile
			}
		}
	}
}

func main() {
	fmt.Println("=== Multi-Key Coalescing ===")
	multiKeyDBCalls.Store(0)
	epoch := time.Now()

	intake := make(chan LookupRequest, 100)
	done := make(chan struct{})
	go coalescer(intake, done)

	keys := []string{"alice", "bob", "carol"}
	totalRequests := len(keys) * multiKeyRequestsPerKey

	var wg sync.WaitGroup
	perKeyResults := make(map[string]int)
	var mu sync.Mutex

	for _, key := range keys {
		for j := 0; j < multiKeyRequestsPerKey; j++ {
			wg.Add(1)
			go func(k string) {
				defer wg.Done()
				req := NewLookupRequest(k)
				intake <- req
				profile := <-req.Reply
				mu.Lock()
				perKeyResults[profile.Email]++
				mu.Unlock()
			}(key)
		}
	}
	wg.Wait()

	elapsed := time.Since(epoch).Round(time.Millisecond)
	fmt.Printf("  keys:               %v\n", keys)
	fmt.Printf("  requests per key:   %d\n", multiKeyRequestsPerKey)
	fmt.Printf("  total requests:     %d\n", totalRequests)
	fmt.Printf("  db calls made:      %d\n", multiKeyDBCalls.Load())
	fmt.Printf("  wall time:          %v\n", elapsed)

	fmt.Println("  per-key results:")
	for email, count := range perKeyResults {
		fmt.Printf("    %s: %d responses\n", email, count)
	}
	fmt.Printf("  reduction: %.0f%%\n",
		(1-float64(multiKeyDBCalls.Load())/float64(totalRequests))*100)

	close(done)
}
```

Each key ("alice", "bob", "carol") triggers exactly 1 database call regardless of how many concurrent requests exist. The three lookups run in parallel since they are for different keys.

### Verification
```bash
go run -race main.go
```
Expected output:
```
=== Multi-Key Coalescing ===
  keys:               [alice bob carol]
  requests per key:   20
  total requests:     60
  db calls made:      3
  wall time:          50ms
  per-key results:
    alice@example.com: 20 responses
    bob@example.com: 20 responses
    carol@example.com: 20 responses
  reduction: 95%
```
60 requests resulted in only 3 database calls (one per unique key).

## Step 4 -- Full Comparison with Metrics

Run both approaches side by side with detailed metrics to quantify the improvement.

```go
package main

import (
	"fmt"
	"sync"
	"sync/atomic"
	"time"
)

const (
	benchRequestsPerKey = 30
	benchDBDelay        = 40 * time.Millisecond
)

type UserProfile struct {
	Name  string
	Email string
}

type LookupRequest struct {
	Key   string
	Reply chan UserProfile
}

func NewLookupRequest(key string) LookupRequest {
	return LookupRequest{Key: key, Reply: make(chan UserProfile, 1)}
}

type LookupResult struct {
	Key     string
	Profile UserProfile
}

// BenchResult holds metrics from a test run.
type BenchResult struct {
	Label       string
	DBCalls     int64
	TotalReqs   int
	WallTime    time.Duration
	AvgLatency  time.Duration
}

func (r BenchResult) Print() {
	fmt.Printf("  total requests:   %d\n", r.TotalReqs)
	fmt.Printf("  db calls:         %d\n", r.DBCalls)
	fmt.Printf("  wall time:        %v\n", r.WallTime)
	fmt.Printf("  avg latency:      %v\n", r.AvgLatency)
	if r.TotalReqs > 0 {
		fmt.Printf("  call reduction:   %.0f%%\n",
			(1-float64(r.DBCalls)/float64(r.TotalReqs))*100)
	}
}

func runBaseline(keys []string, reqsPerKey int) BenchResult {
	var dbCalls atomic.Int64
	dbLookup := func(key string) UserProfile {
		dbCalls.Add(1)
		time.Sleep(benchDBDelay)
		return UserProfile{Name: key, Email: key + "@example.com"}
	}

	var wg sync.WaitGroup
	var latencySum atomic.Int64
	totalReqs := len(keys) * reqsPerKey
	epoch := time.Now()

	for _, key := range keys {
		for j := 0; j < reqsPerKey; j++ {
			wg.Add(1)
			go func(k string) {
				defer wg.Done()
				start := time.Now()
				_ = dbLookup(k)
				latencySum.Add(int64(time.Since(start)))
			}(key)
		}
	}
	wg.Wait()

	avgLatency := time.Duration(latencySum.Load() / int64(totalReqs))
	return BenchResult{
		Label:      "No Coalescing",
		DBCalls:    dbCalls.Load(),
		TotalReqs:  totalReqs,
		WallTime:   time.Since(epoch).Round(time.Millisecond),
		AvgLatency: avgLatency.Round(time.Millisecond),
	}
}

func runCoalesced(keys []string, reqsPerKey int) BenchResult {
	var dbCalls atomic.Int64
	dbLookup := func(key string) UserProfile {
		dbCalls.Add(1)
		time.Sleep(benchDBDelay)
		return UserProfile{Name: key, Email: key + "@example.com"}
	}

	intake := make(chan LookupRequest, 200)
	done := make(chan struct{})

	go func() {
		inFlight := make(map[string][]chan UserProfile)
		results := make(chan LookupResult, 20)
		for {
			select {
			case <-done:
				return
			case req := <-intake:
				waiters, exists := inFlight[req.Key]
				inFlight[req.Key] = append(waiters, req.Reply)
				if !exists {
					go func(key string) {
						profile := dbLookup(key)
						results <- LookupResult{Key: key, Profile: profile}
					}(req.Key)
				}
			case result := <-results:
				waiters := inFlight[result.Key]
				delete(inFlight, result.Key)
				for _, reply := range waiters {
					reply <- result.Profile
				}
			}
		}
	}()

	var wg sync.WaitGroup
	var latencySum atomic.Int64
	totalReqs := len(keys) * reqsPerKey
	epoch := time.Now()

	for _, key := range keys {
		for j := 0; j < reqsPerKey; j++ {
			wg.Add(1)
			go func(k string) {
				defer wg.Done()
				start := time.Now()
				req := NewLookupRequest(k)
				intake <- req
				<-req.Reply
				latencySum.Add(int64(time.Since(start)))
			}(key)
		}
	}
	wg.Wait()

	avgLatency := time.Duration(latencySum.Load() / int64(totalReqs))
	result := BenchResult{
		Label:      "With Coalescing",
		DBCalls:    dbCalls.Load(),
		TotalReqs:  totalReqs,
		WallTime:   time.Since(epoch).Round(time.Millisecond),
		AvgLatency: avgLatency.Round(time.Millisecond),
	}
	close(done)
	return result
}

func main() {
	keys := []string{"alice", "bob", "carol", "dave", "eve"}

	fmt.Println("=== Baseline (No Coalescing) ===")
	baseline := runBaseline(keys, benchRequestsPerKey)
	baseline.Print()

	fmt.Println()
	fmt.Println("=== With Coalescing ===")
	coalesced := runCoalesced(keys, benchRequestsPerKey)
	coalesced.Print()

	fmt.Println()
	fmt.Println("=== Comparison ===")
	fmt.Printf("  DB calls: %d -> %d (%.0fx reduction)\n",
		baseline.DBCalls, coalesced.DBCalls,
		float64(baseline.DBCalls)/float64(coalesced.DBCalls))
	fmt.Printf("  Wall time: %v -> %v\n",
		baseline.WallTime, coalesced.WallTime)
}
```

### Verification
```bash
go run -race main.go
```
Expected output (approximate):
```
=== Baseline (No Coalescing) ===
  total requests:   150
  db calls:         150
  wall time:        40ms
  avg latency:      40ms
  call reduction:   0%

=== With Coalescing ===
  total requests:   150
  db calls:         5
  wall time:        40ms
  avg latency:      40ms
  call reduction:   97%

=== Comparison ===
  DB calls: 150 -> 5 (30x reduction)
  Wall time: 40ms -> 40ms
```
Same wall time, same latency, but 30x fewer database calls. Under real database load, the coalesced version would be dramatically faster because the database is not overloaded.

## Common Mistakes

### Launching Lookup Inside the Request Handler (No Central Goroutine)
**What happens:** If each goroutine independently checks a shared `inFlight` map with a mutex and launches lookups, race conditions arise between the check and the launch. Two goroutines can both see the key as "not in flight" and both launch lookups.

**Fix:** Use a single coalescer goroutine that processes all requests sequentially via a channel. Since only one goroutine reads and writes the `inFlight` map, no mutex is needed and no race is possible.

### Forgetting to Delete the Key After Result Distribution
**What happens:** If the key is not deleted from `inFlight` after distributing results, subsequent requests for the same key find an empty waiters list and never trigger a new lookup. The coalescer is stuck.

**Fix:** Always delete the key after fanning out results:
```go
case result := <-results:
    waiters := inFlight[result.Key]
    delete(inFlight, result.Key)
    for _, reply := range waiters {
        reply <- result.Profile
    }
```

### Unbuffered Reply Channels
**What happens:** With unbuffered reply channels, the coalescer blocks on `reply <- result.Profile` if the requesting goroutine is not yet waiting on its reply channel. With many waiters, the coalescer is blocked for a long time, unable to process new requests.

**Fix:** Buffer reply channels with capacity 1:
```go
Reply: make(chan UserProfile, 1)
```
The coalescer sends and moves on immediately. The requester reads at its own pace.

## Review

Request coalescing turns N concurrent lookups for one key into a single backend
call, and the design that makes it race-free is having exactly one goroutine own
the in-flight map. That coalescer receives every request on a channel, so its
reads and writes of the map are serialized by construction -- no mutex, and no
check-then-launch window where two goroutines both decide they are first. The
first request for a key appends its reply channel and launches the lookup; later
requests for the same key only append; when the lookup's result arrives, the
coalescer fans it out to every parked reply channel and deletes the key so the
next request starts a fresh lookup. Different keys never contend, so their
lookups run in parallel, and under a cache stampede the reduction in backend
calls lands in the 95-99% range.

Two extensions test whether you actually own the pattern. Add a TTL: give each
lookup a deadline with `time.After`, and if it overruns, deliver an error result
to every parked waiter instead of a value -- which means introducing an
error-carrying result type the coalescer can fan out. Then add batch coalescing:
rather than one lookup per key, have the coalescer accumulate the unique keys
that arrive within a 10ms window and issue a single batch call for all of them,
routing each key's result back to its own waiters. Both force you to keep the
in-flight bookkeeping correct while the shapes of the request and the result
change.

## Resources
- [Go Concurrency Patterns (Rob Pike)](https://go.dev/talks/2012/concurrency.slide) -- the reply-channel and central-goroutine idioms this coalescer is built from.
- [golang.org/x/sync/singleflight](https://pkg.go.dev/golang.org/x/sync/singleflight) -- the standard mutex-based implementation of the pattern you build here by hand.
- [Go Blog: Share Memory By Communicating](https://go.dev/blog/codelab-share) -- why serializing state through one goroutine beats guarding it with locks.
- [Effective Go: Channels](https://go.dev/doc/effective_go#channels) -- channel fundamentals, including the buffered reply channels and select this design depends on.

---

Back to [Concurrency](../../concurrency.md) | Next: [01-select-basics](../../03-select-and-multiplexing/01-select-basics/01-select-basics.md)
