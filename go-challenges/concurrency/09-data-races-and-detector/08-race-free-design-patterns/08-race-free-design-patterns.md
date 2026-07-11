# Exercise 8: Race-Free Design Patterns

Earlier exercises showed how to fix a race after it appears: add a mutex, route
through a channel, reach for an atomic. Those are reactive. This exercise takes
the proactive stance -- design the concurrent code so a race cannot occur in the
first place -- because the best race fix is making races impossible by design.
Three patterns get you there, and each removes the precondition for a race rather
than guarding against its symptom: confinement keeps data in a single goroutine,
immutability hands out copies, and ownership transfer moves data through a
pipeline so exactly one stage ever touches it.

## What you'll build

```text
08-race-free-design-patterns/
  main.go        the three race-free patterns -- confinement, immutability,
                 ownership transfer -- then all three combined in a request processor
```

- Build: concurrent programs where races are impossible by construction, culminating in a complete request processor that uses all three patterns.
- Implement: a channel-confined metrics collector (`ConfinedCollector`), a deep-copy request path (`deepCopyRequest`), a `produce -> enrich -> format` ownership-transfer pipeline, and a combined `RequestPipeline` plus `ConfinedMetrics`.
- Verify: `go run -race main.go` for each step -- zero race warnings is the pass condition.

### Why design beats fixing

Each pattern eliminates a different precondition of a data race:

| Pattern | Mechanism | No Race Because |
|---------|-----------|-----------------|
| **Confinement** | Single goroutine owns the data, receives commands via channel | No concurrent access |
| **Immutability** | Pass copies, not references | Goroutines cannot modify shared data |
| **Ownership Transfer** | Data passes through a pipeline; only the current stage owns it | At any moment, exactly one goroutine owns each piece of data |

When you combine these patterns, you write concurrent code that is **correct by
construction**, not by careful locking.

## Step 1 -- Confinement: Channel-Based Metrics Collector

Confine data to one goroutine that processes commands. This is the pattern from exercise 04 applied as a design principle. No other goroutine ever touches the map:

```go
package main

import (
	"fmt"
	"sync"
	"time"
)

type metricsCmd struct {
	action   string
	key      string
	resultCh chan<- map[string]int
}

type ConfinedCollector struct {
	cmdCh chan metricsCmd
}

func NewConfinedCollector() *ConfinedCollector {
	c := &ConfinedCollector{
		cmdCh: make(chan metricsCmd, 256),
	}
	go c.run()
	return c
}

func (c *ConfinedCollector) run() {
	counters := make(map[string]int)
	for cmd := range c.cmdCh {
		switch cmd.action {
		case "inc":
			counters[cmd.key]++
		case "snapshot":
			snap := make(map[string]int, len(counters))
			for k, v := range counters {
				snap[k] = v
			}
			cmd.resultCh <- snap
		}
	}
}

func (c *ConfinedCollector) Record(key string) {
	c.cmdCh <- metricsCmd{action: "inc", key: key}
}

func (c *ConfinedCollector) Snapshot() map[string]int {
	ch := make(chan map[string]int, 1)
	c.cmdCh <- metricsCmd{action: "snapshot", resultCh: ch}
	return <-ch
}

func (c *ConfinedCollector) Close() {
	close(c.cmdCh)
}

func main() {
	collector := NewConfinedCollector()
	var wg sync.WaitGroup

	endpoints := []string{"/api/users", "/api/orders", "/api/products"}

	start := time.Now()
	for _, ep := range endpoints {
		for h := 0; h < 50; h++ {
			wg.Add(1)
			go func(endpoint string) {
				defer wg.Done()
				for r := 0; r < 100; r++ {
					collector.Record(endpoint)
				}
			}(ep)
		}
	}
	wg.Wait()
	elapsed := time.Since(start)

	fmt.Println("=== Pattern 1: Confinement ===")
	fmt.Println("The map is owned by a single goroutine. No concurrent access possible.")
	fmt.Println()
	snap := collector.Snapshot()
	total := 0
	for ep, count := range snap {
		fmt.Printf("  %-20s %d requests\n", ep, count)
		total += count
	}
	fmt.Printf("  %-20s %d requests (%v)\n", "TOTAL", total, elapsed)
	collector.Close()
}
```

Key design insight: the `counters` map is a local variable inside `run()`. It is never exported, never returned by reference, and never accessed by any other goroutine. The race is impossible by construction, not by locking.

### Verification
```bash
go run -race main.go
```
Expected: 5000 per endpoint, 15000 total, zero race warnings.

## Step 2 -- Immutability: Pass Copies, Not References

When processing requests, pass copies of data to each goroutine. Each goroutine gets its own independent snapshot that it can read (or even modify locally) without affecting anyone else:

```go
package main

import (
	"fmt"
	"strings"
	"sync"
)

type Request struct {
	ID       int
	UserID   string
	Endpoint string
	Headers  map[string]string
}

func copyHeaders(src map[string]string) map[string]string {
	dst := make(map[string]string, len(src))
	for k, v := range src {
		dst[k] = v
	}
	return dst
}

func deepCopyRequest(r Request) Request {
	return Request{
		ID:       r.ID,
		UserID:   r.UserID,
		Endpoint: r.Endpoint,
		Headers:  copyHeaders(r.Headers),
	}
}

type ProcessingResult struct {
	RequestID int
	Summary   string
}

func processRequest(req Request) ProcessingResult {
	// Safe to read and even modify req: it is our own copy.
	req.Headers["X-Processed"] = "true"
	return ProcessingResult{
		RequestID: req.ID,
		Summary: fmt.Sprintf("user=%s endpoint=%s headers=%d",
			req.UserID, req.Endpoint, len(req.Headers)),
	}
}

func main() {
	fmt.Println("=== Pattern 2: Immutability (Pass Copies) ===")
	fmt.Println("Each goroutine gets a deep copy. Modifications are local only.")
	fmt.Println()

	baseHeaders := map[string]string{
		"Content-Type":  "application/json",
		"Authorization": "Bearer token-xyz",
	}

	requests := []Request{
		{ID: 1, UserID: "alice", Endpoint: "/api/users", Headers: baseHeaders},
		{ID: 2, UserID: "bob", Endpoint: "/api/orders", Headers: baseHeaders},
		{ID: 3, UserID: "charlie", Endpoint: "/api/billing", Headers: baseHeaders},
		{ID: 4, UserID: "diana", Endpoint: "/api/shipping", Headers: baseHeaders},
	}

	results := make(chan ProcessingResult, len(requests))
	var wg sync.WaitGroup

	for _, req := range requests {
		wg.Add(1)
		copied := deepCopyRequest(req)
		go func(r Request) {
			defer wg.Done()
			results <- processRequest(r)
		}(copied)
	}

	go func() {
		wg.Wait()
		close(results)
	}()

	for res := range results {
		fmt.Printf("  Request %d: %s\n", res.RequestID, res.Summary)
	}

	// Original headers are untouched.
	fmt.Println()
	fmt.Printf("  Original headers still have %d entries (not modified)\n", len(baseHeaders))
	fmt.Printf("  Headers: %s\n", formatMap(baseHeaders))
}

func formatMap(m map[string]string) string {
	parts := make([]string, 0, len(m))
	for k, v := range m {
		parts = append(parts, fmt.Sprintf("%s=%s", k, v))
	}
	return strings.Join(parts, ", ")
}
```

Key design insight: `deepCopyRequest` creates a completely independent copy, including deep-copying the `Headers` map. Each goroutine can even modify its copy (adding `X-Processed`) without affecting anyone else. The original `baseHeaders` map remains untouched.

**Warning**: if you only do a shallow copy of a struct with map/slice/pointer fields, the goroutines still share the underlying data. Always deep-copy fields that contain references.

### Verification
```bash
go run -race main.go
```
Expected: all four requests processed correctly, original headers untouched, zero race warnings.

## Step 3 -- Ownership Transfer: Producer-Consumer Pipeline

Build a pipeline where data flows through stages. At each stage, ownership transfers: the sender relinquishes the data, the receiver takes full ownership. No two goroutines ever touch the same data simultaneously:

```go
package main

import (
	"fmt"
	"strings"
	"time"
)

type LogEntry struct {
	Timestamp time.Time
	Level     string
	Message   string
	Source    string
}

type EnrichedEntry struct {
	LogEntry
	Normalized string
	Priority   int
}

type OutputRecord struct {
	EnrichedEntry
	FormattedLine string
}

func produce(entries []LogEntry) <-chan LogEntry {
	out := make(chan LogEntry)
	go func() {
		defer close(out)
		for _, entry := range entries {
			out <- entry // ownership transfers to the channel
			// After sending, the producer no longer uses this entry.
		}
	}()
	return out
}

func enrich(in <-chan LogEntry) <-chan EnrichedEntry {
	out := make(chan EnrichedEntry)
	go func() {
		defer close(out)
		for entry := range in {
			// We OWN this entry. No one else can access it.
			priority := 0
			switch entry.Level {
			case "ERROR":
				priority = 3
			case "WARN":
				priority = 2
			case "INFO":
				priority = 1
			}
			out <- EnrichedEntry{
				LogEntry:   entry,
				Normalized: strings.ToLower(entry.Message),
				Priority:   priority,
			}
		}
	}()
	return out
}

func format(in <-chan EnrichedEntry) <-chan OutputRecord {
	out := make(chan OutputRecord)
	go func() {
		defer close(out)
		for enriched := range in {
			line := fmt.Sprintf("[%s] P%d %-6s %s (%s)",
				enriched.Timestamp.Format("15:04:05"),
				enriched.Priority,
				enriched.Level,
				enriched.Normalized,
				enriched.Source)
			out <- OutputRecord{
				EnrichedEntry: enriched,
				FormattedLine: line,
			}
		}
	}()
	return out
}

func main() {
	fmt.Println("=== Pattern 3: Ownership Transfer (Producer-Consumer Pipeline) ===")
	fmt.Println("Each stage owns the data exclusively. No concurrent access.")
	fmt.Println()

	now := time.Now()
	entries := []LogEntry{
		{Timestamp: now, Level: "INFO", Message: "Server started on port 8080", Source: "main"},
		{Timestamp: now, Level: "INFO", Message: "Connected to database", Source: "db"},
		{Timestamp: now, Level: "WARN", Message: "Slow query detected (2.3s)", Source: "db"},
		{Timestamp: now, Level: "ERROR", Message: "Connection refused: redis:6379", Source: "cache"},
		{Timestamp: now, Level: "INFO", Message: "Health check passed", Source: "healthz"},
		{Timestamp: now, Level: "ERROR", Message: "Timeout calling payment API", Source: "billing"},
	}

	// Connect the pipeline: produce -> enrich -> format -> consume.
	produced := produce(entries)
	enriched := enrich(produced)
	formatted := format(enriched)

	for record := range formatted {
		fmt.Printf("  %s\n", record.FormattedLine)
	}

	fmt.Println()
	fmt.Println("Data flowed: produce -> enrich -> format -> consume")
	fmt.Println("At each stage, exactly one goroutine owned each LogEntry.")
	fmt.Println("No mutexes, no atomics, no races possible.")
}
```

Key design insight: each stage receives data from an input channel, processes it, and sends results to an output channel. After sending, the sender never touches that data again. After receiving, the receiver has full ownership. At no point do two goroutines access the same data. The pipeline is race-free by architecture, not by locking.

### Verification
```bash
go run -race main.go
```
Expected: six formatted log entries with correct priority levels, zero race warnings.

## Step 4 -- Combined: Complete Request Processor

Build a complete request processor that uses all three patterns together. This represents a realistic server middleware pipeline:

```go
package main

import (
	"fmt"
	"sync"
	"time"
)

const metricsBufferSize = 128

// --- Immutability: deep copy requests ---

// IncomingRequest represents an HTTP request entering the pipeline.
type IncomingRequest struct {
	ID      int
	Method  string
	Path    string
	UserID  string
	Headers map[string]string
}

func (r IncomingRequest) DeepCopy() IncomingRequest {
	headers := make(map[string]string, len(r.Headers))
	for k, v := range r.Headers {
		headers[k] = v
	}
	return IncomingRequest{
		ID:      r.ID,
		Method:  r.Method,
		Path:    r.Path,
		UserID:  r.UserID,
		Headers: headers,
	}
}

// --- Ownership Transfer: pipeline stages ---

// ValidatedRequest is an IncomingRequest after authentication check.
type ValidatedRequest struct {
	IncomingRequest
	IsAuthenticated bool
}

// ProcessedResult is the final outcome of processing a request.
type ProcessedResult struct {
	RequestID  int
	StatusCode int
	Body       string
	Duration   time.Duration
}

// RequestPipeline orchestrates validate -> process stages via channels.
type RequestPipeline struct{}

func (RequestPipeline) Validate(in <-chan IncomingRequest) <-chan ValidatedRequest {
	out := make(chan ValidatedRequest)
	go func() {
		defer close(out)
		for req := range in {
			authenticated := req.Headers["Authorization"] != ""
			out <- ValidatedRequest{
				IncomingRequest: req,
				IsAuthenticated: authenticated,
			}
		}
	}()
	return out
}

func (RequestPipeline) Process(in <-chan ValidatedRequest) <-chan ProcessedResult {
	out := make(chan ProcessedResult)
	go func() {
		defer close(out)
		for vr := range in {
			start := time.Now()
			status := 200
			body := fmt.Sprintf("OK: %s %s for user %s", vr.Method, vr.Path, vr.UserID)
			if !vr.IsAuthenticated {
				status = 401
				body = "Unauthorized"
			}
			out <- ProcessedResult{
				RequestID:  vr.ID,
				StatusCode: status,
				Body:       body,
				Duration:   time.Since(start),
			}
		}
	}()
	return out
}

// --- Confinement: metrics collector goroutine ---

type metricsCmd struct {
	key      string
	resultCh chan<- map[string]int
}

// ConfinedMetrics owns a counters map via a single goroutine.
type ConfinedMetrics struct {
	cmdCh  chan metricsCmd
	snapCh chan metricsCmd
	done   chan struct{}
}

func NewConfinedMetrics() *ConfinedMetrics {
	m := &ConfinedMetrics{
		cmdCh:  make(chan metricsCmd, metricsBufferSize),
		snapCh: make(chan metricsCmd, 1),
		done:   make(chan struct{}),
	}
	go m.run()
	return m
}

func (m *ConfinedMetrics) run() {
	defer close(m.done)
	counters := make(map[string]int)
	for {
		select {
		case cmd, ok := <-m.cmdCh:
			if !ok {
				select {
				case s := <-m.snapCh:
					s.resultCh <- m.copyCounters(counters)
				default:
				}
				return
			}
			counters[cmd.key]++
		case s := <-m.snapCh:
			s.resultCh <- m.copyCounters(counters)
		}
	}
}

func (m *ConfinedMetrics) copyCounters(counters map[string]int) map[string]int {
	snap := make(map[string]int, len(counters))
	for k, v := range counters {
		snap[k] = v
	}
	return snap
}

func (m *ConfinedMetrics) Record(key string) {
	m.cmdCh <- metricsCmd{key: key}
}

func (m *ConfinedMetrics) Snapshot() map[string]int {
	ch := make(chan map[string]int, 1)
	m.snapCh <- metricsCmd{resultCh: ch}
	return <-ch
}

func (m *ConfinedMetrics) Stop() {
	close(m.cmdCh)
	<-m.done
}

// --- Wiring ---

func feedPipeline(requests []IncomingRequest) <-chan IncomingRequest {
	inputCh := make(chan IncomingRequest, len(requests))
	var wg sync.WaitGroup

	for _, req := range requests {
		wg.Add(1)
		copied := req.DeepCopy()
		go func(r IncomingRequest) {
			defer wg.Done()
			inputCh <- r
		}(copied)
	}

	go func() {
		wg.Wait()
		close(inputCh)
	}()
	return inputCh
}

func consumeResults(results <-chan ProcessedResult, metrics *ConfinedMetrics) {
	fmt.Println("Results:")
	for result := range results {
		status := "OK"
		if result.StatusCode != 200 {
			status = "FAIL"
		}
		fmt.Printf("  Request %d: [%d] %s (%v)\n",
			result.RequestID, result.StatusCode, result.Body, result.Duration)
		metrics.Record(fmt.Sprintf("status_%d", result.StatusCode))
		metrics.Record(status)
	}
}

func printMetrics(metrics *ConfinedMetrics) {
	fmt.Println()
	fmt.Println("Metrics (confined to single goroutine):")
	for k, v := range metrics.Snapshot() {
		fmt.Printf("  %-15s %d\n", k, v)
	}
}

func printDesignSummary() {
	fmt.Println()
	fmt.Println("--- Design Summary ---")
	fmt.Println("  Immutability:       deep-copied requests before pipeline entry")
	fmt.Println("  Ownership Transfer: validate -> process pipeline, each stage owns the data")
	fmt.Println("  Confinement:        metrics map owned by a single collector goroutine")
	fmt.Println()
	fmt.Println("No mutexes. No atomics. Races are impossible by architecture.")
}

func main() {
	fmt.Println("=== Combined: Complete Request Processor ===")
	fmt.Println("Using all three race-free patterns together.")
	fmt.Println()

	metrics := NewConfinedMetrics()

	requests := []IncomingRequest{
		{ID: 1, Method: "GET", Path: "/api/users", UserID: "alice",
			Headers: map[string]string{"Authorization": "Bearer abc"}},
		{ID: 2, Method: "POST", Path: "/api/orders", UserID: "bob",
			Headers: map[string]string{"Authorization": "Bearer def"}},
		{ID: 3, Method: "GET", Path: "/api/products", UserID: "anon",
			Headers: map[string]string{}},
		{ID: 4, Method: "DELETE", Path: "/api/users/5", UserID: "charlie",
			Headers: map[string]string{"Authorization": "Bearer ghi"}},
		{ID: 5, Method: "GET", Path: "/healthz", UserID: "monitor",
			Headers: map[string]string{}},
	}

	pipeline := RequestPipeline{}
	inputCh := feedPipeline(requests)
	validated := pipeline.Validate(inputCh)
	results := pipeline.Process(validated)

	consumeResults(results, metrics)
	printMetrics(metrics)
	metrics.Stop()

	printDesignSummary()

	fmt.Println()
	fmt.Printf("Original request headers untouched: %v\n",
		requests[0].Headers["Authorization"] == "Bearer abc")
}
```

This program demonstrates:
- **Immutability**: `copyRequest()` deep-copies each request before it enters the pipeline
- **Ownership Transfer**: `validate -> process` pipeline, each stage exclusively owns the data it receives
- **Confinement**: the metrics map is owned by a single goroutine, accessed only through function closures over channels

### Verification
```bash
go run -race main.go
```
Expected: all five requests processed, metrics collected, zero race warnings. No mutexes or atomics used anywhere.

## Common Mistakes

### Sharing Slices by Accident
```go
data := []int{1, 2, 3, 4, 5}
go func() {
    data[0] = 99 // modifies the underlying array
}()
fmt.Println(data[0]) // race: reads the same array
```
Slices contain a pointer to the underlying array. Passing a slice to a goroutine does NOT copy the elements. Use `copy()` or pass non-overlapping sub-slices.

### Thinking Structs Are Always Copied
Structs are copied by value when assigned or passed as parameters. However, if a struct contains **pointer fields, slices, or maps**, only the reference is copied, not the underlying data. Always deep-copy these fields.

### Breaking Ownership After Sending to a Channel
```go
out <- entry
entry.Level = "DEBUG" // BUG: you no longer own this data
```
After sending data to a channel, consider it gone. The receiver now owns it. Any modification after sending is a race.

## Review

The three patterns are one idea seen from three angles: remove the sharing and
you remove the race. Confinement gives a single goroutine sole ownership of the
state -- the metrics map lives inside `run()` and is reachable only through
commands on a channel, so no other goroutine can touch it. Immutability hands
each goroutine its own deep copy, so a modification is local and the original is
untouched -- but only if the copy is genuinely deep, because a shallow copy of a
struct with map, slice, or pointer fields still shares the underlying data.
Ownership transfer moves each value down a `produce -> enrich -> format`
pipeline, and the discipline is absolute: once you send on a channel the value is
gone, and reading or writing it afterward is a race. Combine all three and the
architecture, not careful locking, is what makes the code correct -- no mutex, no
atomic, and `go run -race` reporting zero warnings is the proof. That is also the
selection rule: if each goroutine can work on its own copy, reach for immutability
first; if data flows through stages, use ownership transfer; if shared state
genuinely must be updated in place, confine it to one goroutine, and only drop to
`sync/atomic` for a simple counter or `sync.Mutex` for complex high-contention
state when a design-level answer will not fit.

Make sure you can articulate the difference between confinement and ownership
transfer, name the point where deep-copying grows expensive enough that a mutex
becomes the better choice, and explain why designing so races are impossible
beats fixing them with locks after the fact. When you write new concurrent code,
start from the question the whole section leads to: how can I design this so races
are impossible?

## Resources

- [Effective Go: Share by Communicating](https://go.dev/doc/effective_go#sharing) -- the Go proverb behind confinement and ownership transfer: pass data, don't share it.
- [Go Concurrency Patterns](https://go.dev/talks/2012/concurrency.slide) -- Rob Pike's pipeline and fan-out slides that ownership transfer builds on.
- [Advanced Go Concurrency Patterns](https://go.dev/talks/2013/advconc.slide) -- deeper patterns for confining state to a single owning goroutine.
- [Go Proverbs](https://go-proverbs.github.io/) -- the one-line distillations, including "don't communicate by sharing memory."

---

Back to [Concurrency](../../concurrency.md)
