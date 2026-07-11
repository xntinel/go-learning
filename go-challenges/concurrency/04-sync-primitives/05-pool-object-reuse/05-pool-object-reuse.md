# Exercise 5: Pool: Object Reuse

Every allocation in Go eventually becomes work for the garbage collector. In a
high-throughput system -- an HTTP server answering thousands of requests a
second, a log formatter writing thousands of lines, a JSON encoder serializing
responses -- creating and discarding temporary `bytes.Buffer` objects generates
real GC pressure, which shows up as latency through collection pauses and wasted
CPU. `sync.Pool` is the standard answer: a thread-safe cache of reusable objects
you `Get`, use, `Reset`, and `Put` back, so the next request recycles memory
instead of allocating fresh. The catch that trips people up is that it is not a
permanent cache -- the GC can empty it at any time -- and a buffer put back
dirty leaks the last request's data into the next one.

## What you'll build

```text
05-pool-object-reuse/
  main.go        a bytes.Buffer pool exercised through the Get/Reset/Put
                 lifecycle, under concurrency, and benchmarked vs fresh alloc
```

- Build: a buffer pool for HTTP-style response building, proven under load.
- Implement: a `sync.Pool` with a `New` function; `Get`/`Put` wrappers that `Reset`; a `ResponseBuilder.BuildJSON` that copies out before returning the buffer; and a benchmark comparing pooled vs fresh allocation.
- Verify: `go run main.go` (and `go run -race main.go` on the concurrent builder)

### Why sync.Pool is not a cache

Consider an HTTP server: each request needs a buffer to build the response body.
At 10,000 requests per second, that is 10,000 buffer allocations per second,
each one becoming garbage immediately after the response is sent. The GC has to
track and reclaim all of them. Instead of allocating a new buffer for each
request and letting the GC reclaim it, you `Get` a buffer from the pool, use it,
`Reset` it, and `Put` it back when done. The next request gets the recycled
buffer instead of allocating fresh memory.

Key characteristics of `sync.Pool`:
- Objects in the pool may be garbage collected at any time (between GC cycles). The pool is not a permanent cache.
- The `New` function creates a fresh object when the pool is empty.
- `Get` and `Put` are safe for concurrent use without external synchronization.
- Objects returned by `Get` must be reset before use -- the pool does not clear them.

## Step 1 -- Create a Buffer Pool for HTTP Responses

Each HTTP handler needs a `bytes.Buffer` to build its response. The pool provides reusable buffers:

```go
package main

import (
	"bytes"
	"fmt"
	"sync"
)

func newTrackedBufferPool() *sync.Pool {
	return &sync.Pool{
		New: func() any {
			fmt.Println("[pool] Allocating new bytes.Buffer")
			return new(bytes.Buffer)
		},
	}
}

func getBuffer(pool *sync.Pool) *bytes.Buffer {
	return pool.Get().(*bytes.Buffer)
}

func returnBuffer(pool *sync.Pool, buf *bytes.Buffer) {
	buf.Reset()
	pool.Put(buf)
}

func demonstrateBufferPoolLifecycle() {
	pool := newTrackedBufferPool()

	// First Get: pool empty, calls New
	buf := getBuffer(pool)
	fmt.Printf("Got buffer: len=%d\n", buf.Len())

	// Simulate building an HTTP response
	buf.WriteString(`{"status":"ok","data":[1,2,3]}`)
	fmt.Printf("After write: len=%d\n", buf.Len())

	returnBuffer(pool, buf)

	// Second Get: reuses the pooled buffer (no allocation)
	recycled := getBuffer(pool)
	fmt.Printf("Recycled buffer: len=%d (reset worked)\n", recycled.Len())
	returnBuffer(pool, recycled)
}

func main() {
	demonstrateBufferPoolLifecycle()
}
```

Expected output:
```
[pool] Allocating new bytes.Buffer
Got buffer: len=0
After write: len=30
Recycled buffer: len=0 (reset worked)
```

### Verification
```bash
go run main.go
```
The second Get should NOT trigger "Allocating new bytes.Buffer" -- it reuses the pooled object.

## Step 2 -- Concurrent Buffer Pool Under Load

Simulate an HTTP server handling 100 concurrent requests. Each request gets a buffer, builds a response, and returns the buffer:

```go
package main

import (
	"bytes"
	"fmt"
	"sync"
	"sync/atomic"
)

const (
	totalRequests      = 100
	requestsPerHandler = 5
)

type BufferPool struct {
	pool       sync.Pool
	allocCount atomic.Int64
}

func NewBufferPool() *BufferPool {
	bp := &BufferPool{}
	bp.pool.New = func() any {
		bp.allocCount.Add(1)
		return new(bytes.Buffer)
	}
	return bp
}

func (bp *BufferPool) Get() *bytes.Buffer {
	buf := bp.pool.Get().(*bytes.Buffer)
	buf.Reset()
	return buf
}

func (bp *BufferPool) Put(buf *bytes.Buffer) {
	buf.Reset()
	bp.pool.Put(buf)
}

func (bp *BufferPool) Allocations() int64 {
	return bp.allocCount.Load()
}

func simulateHTTPHandler(bp *BufferPool, requestID int) {
	for iteration := 0; iteration < requestsPerHandler; iteration++ {
		buf := bp.Get()
		fmt.Fprintf(buf, `{"request":%d,"iteration":%d,"status":"ok"}`, requestID, iteration)
		_ = buf.Bytes() // in real code: write to http.ResponseWriter
		bp.Put(buf)
	}
}

func runConcurrentRequests(bp *BufferPool) {
	var wg sync.WaitGroup

	for i := 0; i < totalRequests; i++ {
		wg.Add(1)
		go func(requestID int) {
			defer wg.Done()
			simulateHTTPHandler(bp, requestID)
		}(i)
	}

	wg.Wait()
}

func printPoolStats(totalGetCalls int, allocations int64) {
	reuseRate := float64(totalGetCalls-int(allocations)) / float64(totalGetCalls) * 100
	fmt.Printf("Total Get() calls: %d\n", totalGetCalls)
	fmt.Printf("Actual allocations: %d\n", allocations)
	fmt.Printf("Reuse rate: %.1f%%\n", reuseRate)
}

func main() {
	bp := NewBufferPool()
	runConcurrentRequests(bp)
	printPoolStats(totalRequests*requestsPerHandler, bp.Allocations())
}
```

Expected output:
```
Total Get() calls: 500
Actual allocations: ~50
Reuse rate: ~90.0%
```

### Verification
```bash
go run main.go
```
The allocation count should be much less than 500. The pool reuses buffers across goroutines.

## Step 3 -- Realistic HTTP Response Builder

The full pattern: get a buffer from the pool, build the response, copy the result out, reset, and return the buffer. This is exactly how production HTTP middleware works:

```go
package main

import (
	"bytes"
	"fmt"
	"sync"
)

type User struct {
	ID   int
	Name string
}

type ResponseBuilder struct {
	pool sync.Pool
}

func NewResponseBuilder() *ResponseBuilder {
	rb := &ResponseBuilder{}
	rb.pool.New = func() any { return new(bytes.Buffer) }
	return rb
}

func (rb *ResponseBuilder) BuildJSON(userID int, name string) []byte {
	buf := rb.pool.Get().(*bytes.Buffer)
	buf.Reset()

	buf.WriteString(`{"user_id":`)
	fmt.Fprintf(buf, "%d", userID)
	buf.WriteString(`,"name":"`)
	buf.WriteString(name)
	buf.WriteString(`","status":"active"}`)

	// CRITICAL: copy before returning the buffer to the pool.
	// After Put, another goroutine may overwrite the buffer contents.
	result := make([]byte, buf.Len())
	copy(result, buf.Bytes())

	buf.Reset()
	rb.pool.Put(buf)
	return result
}

func buildAllResponses(builder *ResponseBuilder, users []User) [][]byte {
	var wg sync.WaitGroup
	responses := make([][]byte, len(users))

	for i, user := range users {
		wg.Add(1)
		go func(idx int, u User) {
			defer wg.Done()
			responses[idx] = builder.BuildJSON(u.ID, u.Name)
		}(i, user)
	}

	wg.Wait()
	return responses
}

func printResponses(responses [][]byte) {
	fmt.Println("=== HTTP Responses ===")
	for _, resp := range responses {
		fmt.Printf("  %s\n", resp)
	}
}

func main() {
	users := []User{
		{1, "Alice"}, {2, "Bob"}, {3, "Carol"}, {4, "Dave"}, {5, "Eve"},
	}

	builder := NewResponseBuilder()
	responses := buildAllResponses(builder, users)
	printResponses(responses)
}
```

Expected output:
```
=== HTTP Responses ===
  {"user_id":1,"name":"Alice","status":"active"}
  {"user_id":2,"name":"Bob","status":"active"}
  {"user_id":3,"name":"Carol","status":"active"}
  {"user_id":4,"name":"Dave","status":"active"}
  {"user_id":5,"name":"Eve","status":"active"}
```

### Verification
```bash
go run -race main.go
```
Each goroutine should produce a valid JSON response with no race warnings.

## Step 4 -- Benchmarking: Pool vs Fresh Allocation

Measure the actual difference. This benchmark simulates 10,000 requests with and without pooling:

```go
package main

import (
	"bytes"
	"fmt"
	"runtime"
	"sync"
	"time"
)

const benchmarkIterations = 10000

type BenchResult struct {
	Elapsed    time.Duration
	AllocBytes uint64
}

func measureMemory(fn func()) (time.Duration, uint64) {
	var memBefore runtime.MemStats
	runtime.GC()
	runtime.ReadMemStats(&memBefore)

	start := time.Now()
	fn()
	elapsed := time.Since(start)

	var memAfter runtime.MemStats
	runtime.ReadMemStats(&memAfter)
	return elapsed, memAfter.TotalAlloc - memBefore.TotalAlloc
}

func buildPayload(buf *bytes.Buffer, requestID int) {
	fmt.Fprintf(buf, `{"request":%d,"data":"some payload here for request %d"}`, requestID, requestID)
}

func benchWithoutPool(iterations int) (time.Duration, uint64) {
	return measureMemory(func() {
		var wg sync.WaitGroup
		for i := 0; i < iterations; i++ {
			wg.Add(1)
			go func(id int) {
				defer wg.Done()
				buf := new(bytes.Buffer) // fresh allocation every time
				buildPayload(buf, id)
				_ = buf.Bytes()
			}(i)
		}
		wg.Wait()
	})
}

func benchWithPool(iterations int) (time.Duration, uint64) {
	pool := &sync.Pool{
		New: func() any { return new(bytes.Buffer) },
	}

	return measureMemory(func() {
		var wg sync.WaitGroup
		for i := 0; i < iterations; i++ {
			wg.Add(1)
			go func(id int) {
				defer wg.Done()
				buf := pool.Get().(*bytes.Buffer)
				buf.Reset()
				buildPayload(buf, id)
				_ = buf.Bytes()
				buf.Reset()
				pool.Put(buf)
			}(i)
		}
		wg.Wait()
	})
}

func printBenchComparison(withoutTime time.Duration, withoutAlloc uint64, withTime time.Duration, withAlloc uint64) {
	fmt.Printf("Without pool: %v, ~%d KB allocated\n", withoutTime.Round(time.Millisecond), withoutAlloc/1024)
	fmt.Printf("With pool:    %v, ~%d KB allocated\n", withTime.Round(time.Millisecond), withAlloc/1024)

	if withAlloc < withoutAlloc {
		reduction := float64(withoutAlloc-withAlloc) / float64(withoutAlloc) * 100
		fmt.Printf("\nPool reduced allocations by %.0f%%\n", reduction)
	}
}

func main() {
	fmt.Printf("Benchmark: %d simulated HTTP requests\n\n", benchmarkIterations)

	withoutTime, withoutAlloc := benchWithoutPool(benchmarkIterations)
	withTime, withAlloc := benchWithPool(benchmarkIterations)

	printBenchComparison(withoutTime, withoutAlloc, withTime, withAlloc)
}
```

Expected output (values vary):
```
Benchmark: 10000 simulated HTTP requests

Without pool: 45ms, ~2048 KB allocated
With pool:    30ms, ~512 KB allocated

Pool reduced allocations by 75%
```

### Verification
```bash
go run main.go
```
The pooled version should show significantly fewer allocations.

## Common Mistakes

### Forgetting to Reset Before Put

```go
buf := pool.Get().(*bytes.Buffer)
buf.WriteString(sensitiveUserData)
pool.Put(buf) // WRONG: next Get receives buffer with old data!
```

**What happens:** The next consumer gets a buffer containing leftover data from the previous user. This can leak sensitive information (auth tokens, PII) between requests.

**Fix:** Always `buf.Reset()` before `pool.Put(buf)`.

### Using After Put

```go
buf := pool.Get().(*bytes.Buffer)
pool.Put(buf)
buf.WriteString(data) // WRONG: another goroutine may already be using this buffer
```

**What happens:** Use-after-free semantics. Once you Put an object back, you must not use it. Another goroutine may have already called Get and started writing to the same buffer.

### Using Pool as a Permanent Cache
**Wrong:** Expecting objects to persist across GC cycles.

**Reality:** The GC may clear the pool at any time. `sync.Pool` is for reducing allocations, not for caching data. If you need a permanent cache, use a map with a mutex.

### Pool of Large Objects Without Size Limits
Pools that store arbitrarily large buffers can waste memory. If one request causes a buffer to grow to 10 MB, that oversized buffer stays in the pool. Consider capping the size:
```go
buf.Reset()
if buf.Cap() > maxBufSize {
    return // let GC reclaim oversized buffers
}
pool.Put(buf)
```

## Review

`sync.Pool` is a thread-safe cache of reusable objects whose only job is to
reduce GC pressure, and the canonical use is `bytes.Buffer` pooling in HTTP
servers and log formatters. Its `New` function mints a fresh object when the
pool is empty, and `Get`/`Put` are safe to call from any goroutine without extra
locking. The lifecycle has two non-negotiable rules that the exercise drills:
always `Reset()` before `Put` so the next consumer does not inherit the previous
request's bytes (a real way to leak tokens or PII between users), and never touch
an object after `Put`, because another goroutine may already have taken it. When
you need to keep the result, copy it out -- `copy(result, buf.Bytes())` -- before
handing the buffer back, since the buffer's storage is about to be reused. And
because the GC may empty the pool between cycles, it is never a place to stash
data you need to persist; if a buffer grows huge, drop it rather than pooling the
bloat.

To confirm the lifecycle is second nature, build a small log-formatting system
on a `bytes.Buffer` pool: a `FormatLog(level, message string) string` that gets a
buffer, writes a line with timestamp and level, copies the result out, resets,
and returns the buffer to the pool. Run a thousand concurrent formats under
`-race` and confirm correctness, then benchmark it against a version that
allocates a fresh buffer every call -- the allocation gap is the entire point of
the pattern.

## Resources
- [sync.Pool documentation](https://pkg.go.dev/sync#Pool) -- the Get/Put/New contract and the warning that entries may be reclaimed at any time.
- [Go Blog: Profiling Go Programs](https://go.dev/blog/pprof) -- how to measure the allocation and GC pressure a pool is meant to relieve.
- [bytes.Buffer with sync.Pool](https://pkg.go.dev/bytes#Buffer) -- the Buffer API, including `Reset` and `Cap`, that the pooled objects rely on.

---

Back to [Concurrency](../../concurrency.md) | Next: [06-cond-signal-broadcast](../06-cond-signal-broadcast/06-cond-signal-broadcast.md)
