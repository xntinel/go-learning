# Exercise 10: Channel vs Shared Memory

Go's most famous concurrency proverb -- don't communicate by sharing memory,
share memory by communicating -- is easy to quote and easy to misread as
"mutexes are bad." They are not. Channels and mutexes answer different questions,
and picking the wrong one leaves you with fifty lines of channel plumbing where a
three-line lock would do, or a polling loop where a channel would block cleanly.
This exercise builds the same feature -- a concurrent page-hit counter with event
notification -- both ways, so the tradeoff stops being a slogan and becomes an
intuition.

## What you'll build

```text
10-channel-vs-shared-memory/
  main.go        the same hit counter built with a mutex and with channels,
                 plus a side-by-side benchmark and a decision guide
```

- Build: a concurrent page-hit counter with event notification, implemented once with a mutex and once with channels.
- Implement: `MutexHitCounter`, `ChannelHitCounter` with its single-owner `run` loop, and a shared `HitCounter` interface benchmarked both ways.
- Verify: `go run -race main.go`.

### Why both tools earn their place

Both tools have their place. Channels excel when goroutines need to pass data, coordinate workflows, or signal events. Mutexes excel when you just need to protect a data structure from concurrent access with minimal overhead.

This exercise implements the same feature -- a concurrent page hit counter with event notification -- both ways, so you can develop an intuition for the tradeoffs.

## Step 1 -- The Problem: Data Race

Multiple goroutines increment a shared counter and notify listeners about page hits. Without protection, this is a data race.

```go
package main

import (
	"fmt"
	"sync"
)

const raceGoroutineCount = 1000

// unsafeIncrement demonstrates a data race: counter++ is a
// read-modify-write that is not atomic. Multiple goroutines can
// read the same value, both increment, both write -- losing increments.
func unsafeIncrement(counter *int, wg *sync.WaitGroup) {
	defer wg.Done()
	*counter++ // DATA RACE
}

func main() {
	counter := 0
	var wg sync.WaitGroup

	for i := 0; i < raceGoroutineCount; i++ {
		wg.Add(1)
		go unsafeIncrement(&counter, &wg)
	}

	wg.Wait()
	fmt.Printf("Counter: %d (expected %d, likely wrong)\n", counter, raceGoroutineCount)
}
```

### Verification
```bash
go run -race main.go
# Expected: WARNING: DATA RACE detected, counter may not be 1000
```

The `-race` flag enables Go's race detector -- essential for finding data races during development.

## Step 2 -- Solution A: Mutex (Shared Memory)

Protect the hit counter and event notification with `sync.Mutex`. Lock before reading/writing, unlock after. This is the shared-memory approach.

```go
package main

import (
	"fmt"
	"sync"
	"time"
)

const mutexHitCount = 1000

// MutexHitCounter protects page hit counts with a RWMutex.
// It fires an onChange callback (if set) after each hit.
type MutexHitCounter struct {
	mu       sync.RWMutex
	counts   map[string]int
	total    int
	onChange func(page string, count int)
}

// NewMutexHitCounter creates a counter with an optional change callback.
func NewMutexHitCounter(onChange func(string, int)) *MutexHitCounter {
	return &MutexHitCounter{
		counts:   make(map[string]int),
		onChange: onChange,
	}
}

// RecordHit atomically increments the hit count for the given page.
func (h *MutexHitCounter) RecordHit(page string) {
	h.mu.Lock()
	h.counts[page]++
	h.total++
	count := h.counts[page]
	h.mu.Unlock()

	if h.onChange != nil {
		h.onChange(page, count)
	}
}

// GetCount returns the hit count for a single page.
func (h *MutexHitCounter) GetCount(page string) int {
	h.mu.RLock()
	defer h.mu.RUnlock()
	return h.counts[page]
}

// GetTotal returns the total number of hits across all pages.
func (h *MutexHitCounter) GetTotal() int {
	h.mu.RLock()
	defer h.mu.RUnlock()
	return h.total
}

// NotificationCounter tracks the number of change notifications, protected by its own mutex.
type NotificationCounter struct {
	mu    sync.Mutex
	count int
}

// Increment safely increments the notification count.
func (nc *NotificationCounter) Increment() {
	nc.mu.Lock()
	nc.count++
	nc.mu.Unlock()
}

// Count returns the current notification count.
func (nc *NotificationCounter) Count() int {
	nc.mu.Lock()
	defer nc.mu.Unlock()
	return nc.count
}

// simulateHitLoad launches goroutines that record hits across the given pages.
func simulateHitLoad(counter *MutexHitCounter, pages []string, hitCount int) {
	var wg sync.WaitGroup
	for i := 0; i < hitCount; i++ {
		wg.Add(1)
		go func(n int) {
			defer wg.Done()
			page := pages[n%len(pages)]
			counter.RecordHit(page)
		}(i)
	}
	wg.Wait()
}

func main() {
	notifications := &NotificationCounter{}
	counter := NewMutexHitCounter(func(page string, count int) {
		notifications.Increment()
	})

	pages := []string{"/home", "/about", "/api/users", "/api/orders", "/health"}
	start := time.Now()

	simulateHitLoad(counter, pages, mutexHitCount)
	elapsed := time.Since(start)

	fmt.Println("=== Mutex Version ===")
	fmt.Printf("Total hits: %d\n", counter.GetTotal())
	for _, page := range pages {
		fmt.Printf("  %-15s %d hits\n", page, counter.GetCount(page))
	}
	fmt.Printf("Notifications sent: %d\n", notifications.Count())
	fmt.Printf("Time: %v\n", elapsed)
}
```

**Pros:** Direct, low overhead, familiar pattern. `RWMutex` allows concurrent reads.
**Cons:** Easy to forget the lock. Callback (`onChange`) runs under the caller's goroutine, which could cause issues if it is slow. Must protect the notification counter with a separate mutex.

### Verification
```bash
go run -race main.go
# Expected: Total hits: 1000, no race warnings
```

## Step 3 -- Solution B: Channels (Share Memory by Communicating)

Send hit events to a single goroutine that owns the counter state. Notifications flow through a separate channel. No shared memory, no mutexes.

```go
package main

import (
	"fmt"
	"sync"
	"time"
)

const (
	channelHitCount       = 1000
	hitEventBufferSize    = 100
	notificationBufferSize = 100
)

// HitEvent signals that a page was visited.
type HitEvent struct {
	Page string
}

// QueryRequest asks for the hit count of a specific page.
type QueryRequest struct {
	Page  string
	Reply chan int
}

// TotalRequest asks for the total hit count across all pages.
type TotalRequest struct {
	Reply chan int
}

// ChannelHitCounter owns hit-count state in a single goroutine,
// processing hits, queries, and total requests through channels.
type ChannelHitCounter struct {
	hits          chan HitEvent
	queries       chan QueryRequest
	totals        chan TotalRequest
	notifications chan HitEvent
}

// NewChannelHitCounter creates and starts a channel-based hit counter.
func NewChannelHitCounter() *ChannelHitCounter {
	c := &ChannelHitCounter{
		hits:          make(chan HitEvent, hitEventBufferSize),
		queries:       make(chan QueryRequest),
		totals:        make(chan TotalRequest),
		notifications: make(chan HitEvent, notificationBufferSize),
	}
	go c.run()
	return c
}

func (c *ChannelHitCounter) run() {
	counts := make(map[string]int)
	total := 0

	for {
		select {
		case hit, ok := <-c.hits:
			if !ok {
				close(c.notifications)
				return
			}
			counts[hit.Page]++
			total++
			c.notifications <- hit
		case q := <-c.queries:
			q.Reply <- counts[q.Page]
		case t := <-c.totals:
			t.Reply <- total
		}
	}
}

// RecordHit sends a hit event to the counter service.
func (c *ChannelHitCounter) RecordHit(page string) {
	c.hits <- HitEvent{Page: page}
}

// GetCount returns the hit count for a specific page via request-response.
func (c *ChannelHitCounter) GetCount(page string) int {
	reply := make(chan int, 1)
	c.queries <- QueryRequest{Page: page, Reply: reply}
	return <-reply
}

// GetTotal returns the total hit count via request-response.
func (c *ChannelHitCounter) GetTotal() int {
	reply := make(chan int, 1)
	c.totals <- TotalRequest{Reply: reply}
	return <-reply
}

// Close signals the service to shut down and waits for notification drain.
func (c *ChannelHitCounter) Close() {
	close(c.hits)
}

// countNotifications drains the notification channel and returns the count.
func countNotifications(notifications <-chan HitEvent) (int, <-chan struct{}) {
	done := make(chan struct{})
	var count int
	go func() {
		for range notifications {
			count++
		}
		done <- struct{}{}
	}()
	return count, done
}

func main() {
	counter := NewChannelHitCounter()

	notifyCount := 0
	notifyDone := make(chan struct{})
	go func() {
		for range counter.notifications {
			notifyCount++
		}
		notifyDone <- struct{}{}
	}()

	pages := []string{"/home", "/about", "/api/users", "/api/orders", "/health"}
	var wg sync.WaitGroup
	start := time.Now()

	for i := 0; i < channelHitCount; i++ {
		wg.Add(1)
		go func(n int) {
			defer wg.Done()
			counter.RecordHit(pages[n%len(pages)])
		}(i)
	}

	wg.Wait()
	counter.Close()
	<-notifyDone
	elapsed := time.Since(start)

	fmt.Println("=== Channel Version ===")
	fmt.Printf("Total hits: %d\n", counter.GetTotal())
	for _, page := range pages {
		fmt.Printf("  %-15s %d hits\n", page, counter.GetCount(page))
	}
	fmt.Printf("Notifications sent: %d\n", notifyCount)
	fmt.Printf("Time: %v\n", elapsed)
}
```

**Pros:** No shared state, impossible to forget locking, notifications flow naturally as a channel stream. The counter goroutine is fully self-contained.
**Cons:** More boilerplate, channel operations have overhead, queries require request-response pattern.

### Verification
```bash
go run -race main.go
# Expected: Total hits: 1000, no race warnings
```

## Step 4 -- Side-by-Side: Same Feature, Both Approaches

Run both implementations in the same program to compare behavior and timing.

```go
package main

import (
	"fmt"
	"sync"
	"time"
)

const benchmarkHitCount = 10000

// --- Shared interface for both counter implementations ---

// HitCounter defines the common interface for both implementations.
type HitCounter interface {
	Record(page string)
	Snapshot() (counts map[string]int, total int)
}

// --- Mutex version ---

// MutexCounter guards a map with sync.Mutex.
type MutexCounter struct {
	mu     sync.Mutex
	counts map[string]int
	events int
}

// NewMutexCounter creates a mutex-backed counter.
func NewMutexCounter() *MutexCounter {
	return &MutexCounter{counts: make(map[string]int)}
}

func (c *MutexCounter) Record(page string) {
	c.mu.Lock()
	c.counts[page]++
	c.events++
	c.mu.Unlock()
}

func (c *MutexCounter) Snapshot() (map[string]int, int) {
	c.mu.Lock()
	defer c.mu.Unlock()
	snap := make(map[string]int)
	for k, v := range c.counts {
		snap[k] = v
	}
	return snap, c.events
}

// --- Channel version ---

// counterSnapshot is the response payload for a snapshot request.
type counterSnapshot struct {
	Counts map[string]int
	Events int
}

// ChanCounter processes all mutations in a single goroutine via channels.
type ChanCounter struct {
	hits    chan string
	snapReq chan chan counterSnapshot
}

// NewChanCounter creates and starts a channel-backed counter.
func NewChanCounter() *ChanCounter {
	c := &ChanCounter{
		hits:    make(chan string, 100),
		snapReq: make(chan chan counterSnapshot),
	}
	go c.run()
	return c
}

func (c *ChanCounter) run() {
	counts := make(map[string]int)
	events := 0
	for {
		select {
		case page, ok := <-c.hits:
			if !ok {
				return
			}
			counts[page]++
			events++
		case reply := <-c.snapReq:
			snap := make(map[string]int)
			for k, v := range counts {
				snap[k] = v
			}
			reply <- counterSnapshot{Counts: snap, Events: events}
		}
	}
}

func (c *ChanCounter) Record(page string) { c.hits <- page }

func (c *ChanCounter) Snapshot() (map[string]int, int) {
	reply := make(chan counterSnapshot, 1)
	c.snapReq <- reply
	s := <-reply
	return s.Counts, s.Events
}

// Close shuts down the channel counter's event loop.
func (c *ChanCounter) Close() { close(c.hits) }

// --- Benchmark infrastructure ---

// runBenchmark fires hitCount goroutines against the counter, then prints results.
func runBenchmark(name string, counter HitCounter, pages []string, hitCount int) time.Duration {
	var wg sync.WaitGroup
	start := time.Now()

	for i := 0; i < hitCount; i++ {
		wg.Add(1)
		go func(n int) {
			defer wg.Done()
			counter.Record(pages[n%len(pages)])
		}(i)
	}

	wg.Wait()
	elapsed := time.Since(start)

	counts, events := counter.Snapshot()
	fmt.Printf("=== %s ===\n", name)
	fmt.Printf("  Total events: %d\n", events)
	for _, page := range pages {
		fmt.Printf("  %-15s %d\n", page, counts[page])
	}
	fmt.Printf("  Time: %v\n\n", elapsed)
	return elapsed
}

// printComparison displays which approach was faster.
func printComparison(mutexTime, chanTime time.Duration) {
	fmt.Println("=== Comparison ===")
	fmt.Printf("  Mutex:   %v\n", mutexTime)
	fmt.Printf("  Channel: %v\n", chanTime)
	if mutexTime < chanTime {
		fmt.Println("  Mutex is faster (expected for simple state guarding)")
	} else {
		fmt.Println("  Channel is faster (unusual for this workload)")
	}
}

func main() {
	pages := []string{"/home", "/about", "/api/users", "/api/orders", "/health"}

	mc := NewMutexCounter()
	mutexTime := runBenchmark("Mutex", mc, pages, benchmarkHitCount)

	cc := NewChanCounter()
	chanTime := runBenchmark("Channel", cc, pages, benchmarkHitCount)
	cc.Close()

	printComparison(mutexTime, chanTime)
}
```

### Verification
```bash
go run -race main.go
# Expected: both produce 10000 total events, mutex is typically faster
```

## Step 5 -- When to Use Which

| Use Channels When | Use Mutexes When |
|---|---|
| Passing ownership of data between goroutines | Protecting internal state of a struct |
| Coordinating multiple goroutines (fan-out, pipeline) | Simple read/write protection (counters, caches) |
| Signaling events (done, quit, ready) | Performance-critical hot paths |
| The "server" pattern (request-response) | You need RWMutex for read-heavy workloads |
| You want to compose concurrency operations | The protected section is small and self-contained |
| Event notification streams need to flow naturally | You only guard access, not coordinate workflow |

**Rule of thumb:** If you are transferring data or coordinating work, use channels. If you are guarding access to a shared data structure, use a mutex. Most real Go programs use both tools where each is most appropriate.

### The Real Consequence

**Without either protection:** Data races corrupt your counters. You report wrong analytics, bill customers incorrectly, or lose events. The `-race` flag catches this in development, but in production it is silent data corruption.

**With mutex when channels are better:** You end up with polling loops, condition variables for notifications, and complex lock ordering. The code becomes fragile and hard to extend.

**With channels when mutex is simpler:** You write 50 lines of channel plumbing for what a 3-line mutex block would handle. Over-engineering.

## Verification

Run both versions with `-race` and confirm:
1. Both produce correct counts (no races)
2. The mutex version is typically faster for pure state guarding
3. The channel version handles event notification more naturally
4. Neither has data races

## Common Mistakes

### Using Channels Where a Mutex Is Simpler

**Wrong:** 50 lines of channel plumbing just to increment a counter with no event notification.

**Fix:** Use `sync.Mutex` or `sync/atomic` for simple state protection. Do not over-engineer.

### Using Mutex Where Channels Communicate Better

**Wrong:**
```go
var mu sync.Mutex
var buffer []string

// Producer:
mu.Lock()
buffer = append(buffer, entry)
mu.Unlock()

// Consumer (must poll in a loop):
for {
    mu.Lock()
    if len(buffer) > 0 {
        val = buffer[0]
        buffer = buffer[1:]
        mu.Unlock()
        break
    }
    mu.Unlock()
    time.Sleep(10 * time.Millisecond) // ugly polling
}
```

**Fix:** Use a channel. It is a built-in thread-safe queue with blocking semantics -- no polling needed.

## Review

Both channels and mutexes solve concurrency problems, but they answer different
questions. A mutex protects shared state that lives inside a struct -- a counter,
a cache, a config map -- with low overhead and, in `RWMutex` form, concurrent
reads. A channel moves data and coordinates work: pipelines, fan-out and fan-in,
request-response, and event streams that flow naturally as a sequence of
receives. For simple state guarding the mutex is usually clearer and faster,
which the side-by-side benchmark makes visible; for workflow the channel version
reads as intent rather than plumbing. "Share memory by communicating" is a design
preference that steers you toward channels when goroutines pass data, not an
absolute rule that outlaws locks.

You should be able to answer, without rerunning either version, which tool fits
three cases. A simple in-memory cache with get and set is state protection, so a
mutex (or `sync/atomic`) wins. A pipeline that pushes log entries through filter,
transform, and output stages is coordination, so channels connect the stages
cleanly. And the real danger of choosing neither is the quietest of the three:
unguarded shared state races, corrupting counters and losing events in ways the
`-race` flag catches during development but that stay silent in production.

## Resources
- [Go Blog: Share Memory By Communicating](https://go.dev/blog/codelab-share) -- the essay behind the proverb, with the counter example this exercise mirrors.
- [Effective Go: Concurrency](https://go.dev/doc/effective_go#concurrency) -- when to reach for goroutines and channels versus shared memory.
- [Go Wiki: Mutex or Channel](https://go.dev/wiki/MutexOrChannel) -- a direct decision guide for the exact choice this lesson trains.
- [sync package](https://pkg.go.dev/sync) -- Mutex, RWMutex, and the primitives the shared-memory solution uses.

---

Back to [Concurrency](../../concurrency.md) | Next: [11-channel-error-propagation](../11-channel-error-propagation/11-channel-error-propagation.md)
