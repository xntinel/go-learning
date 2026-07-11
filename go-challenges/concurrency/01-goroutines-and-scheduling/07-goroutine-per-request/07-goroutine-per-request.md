# Exercise 7: Goroutine Per Request

The goroutine-per-request pattern is one of Go's most common concurrency
idioms: each incoming request, job, or independent task gets its own goroutine.
It works because goroutines are cheap enough to create one per task, and the
scheduler multiplexes them onto OS threads. The discipline that makes it safe
is easy to skip -- you must collect every result or you leak goroutines, and
you must recover panics in each worker or a single bad request takes down the
whole process. This exercise builds the pattern the correct way and shows both
failure modes.

## What you'll build

```text
07-goroutine-per-request/
  main.go        one goroutine per request: fan-out, buffered result
                 collection, panic recovery, and server metrics
```

- Build: a goroutine-per-request server simulation that fans out work, collects results, and survives handler panics.
- Implement: a `RequestHandler` and `dispatchRequests` fan-out, an `APIResult` that carries either data or an error, a `safeHandler` wrapping `defer/recover`, and an `HTTPServer` that reports throughput and latency percentiles.
- Verify: `go run main.go`

### Why one goroutine per request scales

This approach has three major advantages. First, each task is isolated: a panic in one goroutine does not crash others (provided you recover it). Second, the programming model is straightforward: each goroutine can be written as simple sequential code. Third, it scales naturally: as load increases, more goroutines are created, and the scheduler distributes them across available cores.

In web servers like `net/http`, this pattern is built in -- every incoming HTTP request is handled in its own goroutine. Understanding the pattern helps you apply it to your own use cases: batch processing, fan-out/fan-in, parallel data pipelines, and more. The key discipline is always collecting all results and recovering panics to prevent goroutine leaks and process crashes.

## Step 1 -- Simulating an HTTP Server Request Handler

Each incoming HTTP connection is accepted from a channel (simulating `net.Listener.Accept()`), processed in its own goroutine (parse request, query database, format response), and the response is sent back.

```go
package main

import (
	"fmt"
	"math/rand/v2"
	"time"
)

const (
	parseLatency          = 1 * time.Millisecond
	formatLatency         = 1 * time.Millisecond
	minDBLatency          = 20
	maxExtraDBLatency     = 80
	dangerPath            = "/admin/danger"
	avgRequestLatencyMs   = 50
)

type Request struct {
	ID     int
	Path   string
	Method string
}

type Response struct {
	RequestID  int
	StatusCode int
	Body       string
	Latency    time.Duration
}

type RequestHandler struct{}

func (h *RequestHandler) Handle(req Request) Response {
	start := time.Now()

	time.Sleep(parseLatency)
	time.Sleep(time.Duration(rand.IntN(maxExtraDBLatency)+minDBLatency) * time.Millisecond)
	time.Sleep(formatLatency)

	statusCode, body := h.buildResponse(req)

	return Response{
		RequestID:  req.ID,
		StatusCode: statusCode,
		Body:       body,
		Latency:    time.Since(start),
	}
}

func (h *RequestHandler) buildResponse(req Request) (int, string) {
	if req.Path == dangerPath {
		return 500, `{"error": "internal server error"}`
	}
	return 200, fmt.Sprintf(`{"path": %q, "rows": %d}`, req.Path, rand.IntN(100))
}

func generateRequests(paths []string) <-chan Request {
	incoming := make(chan Request, len(paths))
	go func() {
		for i, path := range paths {
			incoming <- Request{ID: i + 1, Path: path, Method: "GET"}
		}
		close(incoming)
	}()
	return incoming
}

func dispatchRequests(incoming <-chan Request, handler *RequestHandler) (<-chan Response, int) {
	responses := make(chan Response, 10)
	requestCount := 0
	for req := range incoming {
		requestCount++
		go func(r Request) {
			responses <- handler.Handle(r)
		}(req)
	}
	return responses, requestCount
}

func collectAndPrintResponses(responses <-chan Response, count int) {
	for i := 0; i < count; i++ {
		resp := <-responses
		bodyPreview := resp.Body
		if len(bodyPreview) > 30 {
			bodyPreview = bodyPreview[:30]
		}
		fmt.Printf("  req %2d  %-20s  %d  %v\n",
			resp.RequestID, bodyPreview, resp.StatusCode,
			resp.Latency.Round(time.Millisecond))
	}
}

func main() {
	paths := []string{
		"/api/users", "/api/orders", "/api/products", "/health",
		"/api/search", dangerPath, "/api/metrics", "/api/config",
	}

	handler := &RequestHandler{}
	incoming := generateRequests(paths)

	start := time.Now()
	responses, requestCount := dispatchRequests(incoming, handler)
	collectAndPrintResponses(responses, requestCount)

	wallClock := time.Since(start)
	fmt.Printf("\n  Processed %d requests in %v\n", requestCount, wallClock.Round(time.Millisecond))
	fmt.Printf("  Sequential would have taken: ~%v\n",
		time.Duration(requestCount*avgRequestLatencyMs)*time.Millisecond)
}
```

**What's happening here:** Eight simulated HTTP requests arrive via a channel. Each is handled in its own goroutine -- exactly how Go's `net/http` server works. Each handler parses the request, queries a simulated database, and formats a response. The buffered channel holds results without blocking the senders.

**Key insight:** Wall-clock time is approximately equal to the SLOWEST individual request (~100ms), not the SUM of all requests (~400ms). This is the fundamental benefit of the goroutine-per-request model: each connection handler is simple sequential code, but they all run concurrently.

**What would happen with an unbuffered channel?** Goroutines that finish before main reads would block on send. With enough goroutines, this effectively serializes the work because each must wait for main to read before the next can send.

### Verification
```bash
go run main.go
```
Expected output (order varies):
```
  req  4  /health               200  23ms
  req  1  /api/users            200  45ms
  req  6  /admin/danger         500  32ms
  req  3  /api/products         200  78ms
  ...

  Processed 8 requests in 82ms
  Sequential would have taken: ~400ms
```

## Step 2 -- Structured Responses with Error Handling

In a real server, you need to collect both successful responses and errors. A timeout on the database, a malformed request, or a missing resource should not prevent other requests from completing.

```go
package main

import (
	"fmt"
	"math/rand/v2"
	"time"
)

const (
	minEndpointLatency   = 20
	maxExtraEndpointLat  = 80
	recommendationFailRate = 0.6
	legacyFailRate         = 0.4
)

type APIResult struct {
	Endpoint string
	Status   int
	Data     string
	Err      error
	Latency  time.Duration
}

type EndpointCaller struct{}

func (ec *EndpointCaller) Call(endpoint string) APIResult {
	start := time.Now()
	latency := time.Duration(rand.IntN(maxExtraEndpointLat)+minEndpointLatency) * time.Millisecond
	time.Sleep(latency)

	elapsed := time.Since(start)

	if err := ec.simulateFailure(endpoint); err != nil {
		return APIResult{Endpoint: endpoint, Err: err, Latency: elapsed,
			Status: ec.failureStatusCode(endpoint)}
	}

	return APIResult{
		Endpoint: endpoint,
		Status:   200,
		Data:     fmt.Sprintf(`{"source": %q, "items": %d}`, endpoint, rand.IntN(50)+1),
		Latency:  elapsed,
	}
}

func (ec *EndpointCaller) simulateFailure(endpoint string) error {
	switch {
	case endpoint == "/api/recommendations" && rand.Float32() < recommendationFailRate:
		return fmt.Errorf("recommendation engine timeout")
	case endpoint == "/api/legacy" && rand.Float32() < legacyFailRate:
		return fmt.Errorf("bad gateway: legacy service unreachable")
	default:
		return nil
	}
}

func (ec *EndpointCaller) failureStatusCode(endpoint string) int {
	switch endpoint {
	case "/api/recommendations":
		return 503
	case "/api/legacy":
		return 502
	default:
		return 500
	}
}

func fanOutEndpointCalls(endpoints []string, caller *EndpointCaller) <-chan APIResult {
	results := make(chan APIResult, len(endpoints))
	for _, ep := range endpoints {
		go func(endpoint string) {
			results <- caller.Call(endpoint)
		}(ep)
	}
	return results
}

func collectAndSummarize(results <-chan APIResult, count int) {
	var successes, failures int
	for i := 0; i < count; i++ {
		r := <-results
		if r.Err != nil {
			failures++
			fmt.Printf("  FAIL  %-25s %d  error=%v (%v)\n",
				r.Endpoint, r.Status, r.Err, r.Latency.Round(time.Millisecond))
		} else {
			successes++
			dataPreview := r.Data
			if len(dataPreview) > 40 {
				dataPreview = dataPreview[:40]
			}
			fmt.Printf("  OK    %-25s %d  data=%s (%v)\n",
				r.Endpoint, r.Status, dataPreview, r.Latency.Round(time.Millisecond))
		}
	}
	fmt.Printf("\n  Summary: %d succeeded, %d failed out of %d endpoints\n",
		successes, failures, count)
}

func main() {
	endpoints := []string{
		"/api/user-profile", "/api/order-history", "/api/recommendations",
		"/api/notifications", "/api/legacy", "/api/settings",
	}

	caller := &EndpointCaller{}
	results := fanOutEndpointCalls(endpoints, caller)
	collectAndSummarize(results, len(endpoints))
}
```

**What's happening here:** The `APIResult` struct carries both success data and error information. Each goroutine calls an endpoint and returns a result regardless of success or failure. Some endpoints randomly fail (simulating real-world flaky services).

**Key insight:** An error in "/api/recommendations" does not prevent "/api/user-profile" from succeeding. Each goroutine is isolated. The error is captured as data, not as a crash. In production, this is how you build resilient API aggregators that return partial results when some upstream services are degraded.

### Verification
```bash
go run main.go
```
Expected output (recommendations and legacy may fail):
```
  OK    /api/user-profile         200  data={"source": "/api/user-profile",  (45ms)
  FAIL  /api/recommendations      503  error=recommendation engine timeout (32ms)
  OK    /api/order-history        200  data={"source": "/api/order-history", (67ms)
  OK    /api/notifications        200  data={"source": "/api/notifications", (55ms)
  FAIL  /api/legacy               502  error=bad gateway: legacy service unr (28ms)
  OK    /api/settings             200  data={"source": "/api/settings", "ite (41ms)

  Summary: 4 succeeded, 2 failed out of 6 endpoints
```

## Step 3 -- Panic Isolation: One Bad Handler Cannot Kill the Server

In a real server, a nil pointer dereference or index-out-of-bounds in one request handler must NOT crash the entire process. The `defer/recover` pattern ensures each goroutine handles its own panics.

```go
package main

import (
	"fmt"
	"math/rand/v2"
	"time"
)

const totalRequests = 10

type HandlerResult struct {
	RequestID int
	Status    int
	Body      string
	Panicked  bool
}

func processRequest(reqID int) HandlerResult {
	// Request 3 has a bug: write to nil map
	if reqID == 3 {
		var m map[string]string
		m["key"] = "value" // panic: assignment to entry in nil map
	}

	// Request 7 has a different bug: index out of bounds
	if reqID == 7 {
		s := []int{1, 2, 3}
		_ = s[10] // panic: index out of range
	}

	time.Sleep(time.Duration(rand.IntN(50)+10) * time.Millisecond)
	return HandlerResult{
		RequestID: reqID,
		Status:    200,
		Body:      fmt.Sprintf("request %d processed successfully", reqID),
	}
}

func safeHandler(reqID int, results chan<- HandlerResult) {
	defer func() {
		if r := recover(); r != nil {
			results <- HandlerResult{
				RequestID: reqID,
				Status:    500,
				Body:      fmt.Sprintf("recovered from panic: %v", r),
				Panicked:  true,
			}
		}
	}()
	results <- processRequest(reqID)
}

func printResultsSummary(results <-chan HandlerResult, count int) {
	var okCount, panicCount int
	for i := 0; i < count; i++ {
		r := <-results
		label := "  OK "
		if r.Panicked {
			label = "PANIC"
			panicCount++
		} else {
			okCount++
		}
		fmt.Printf("  [%s] req %2d  %d  %s\n", label, r.RequestID, r.Status, r.Body)
	}

	fmt.Printf("\n  Results: %d OK, %d panicked (recovered), %d total\n", okCount, panicCount, count)
	fmt.Println("  Server stayed up despite handler bugs. In production, each panic")
	fmt.Println("  would be logged with a stack trace for debugging.")
}

func main() {
	results := make(chan HandlerResult, totalRequests)

	for i := 1; i <= totalRequests; i++ {
		go safeHandler(i, results)
	}

	printResultsSummary(results, totalRequests)
}
```

**What's happening here:** Ten request handlers run concurrently. Requests 3 and 7 have bugs that cause panics. The `defer/recover` in each goroutine catches the panic and sends a 500 response instead of crashing. All other requests complete normally.

**Key insight:** An UNRECOVERED panic in ANY goroutine crashes the ENTIRE Go process. The `defer/recover` pattern is essential for request handlers. Go's `net/http` server includes this pattern automatically, but when you build your own worker pools or task queues, you MUST add it yourself. Without recovery, one buggy request kills every other in-flight request.

### Verification
```bash
go run main.go
```
Expected output:
```
  [PANIC] req  3  500  recovered from panic: runtime error: ...
  [PANIC] req  7  500  recovered from panic: runtime error: ...
  [  OK ] req  1  200  request 1 processed successfully
  [  OK ] req  2  200  request 2 processed successfully
  [  OK ] req  4  200  request 4 processed successfully
  ...

  Results: 8 OK, 2 panicked (recovered), 10 total
  Server stayed up despite handler bugs. In production, each panic
  would be logged with a stack trace for debugging.
```

## Step 4 -- Full Server Simulation with Metrics

Build a complete HTTP server simulation: accept connections, process each in a goroutine, collect responses, and report server metrics.

```go
package main

import (
	"cmp"
	"fmt"
	"math/rand/v2"
	"slices"
	"time"
)

const simulatedRequestCount = 20

type HTTPRequest struct {
	ID     int
	Method string
	Path   string
}

type HTTPResponse struct {
	RequestID  int
	StatusCode int
	Latency    time.Duration
}

type ServerMetrics struct {
	Responses    []HTTPResponse
	StatusCounts map[int]int
	TotalLatency time.Duration
	WallClock    time.Duration
}

type HTTPServer struct {
	Paths []string
}

func NewHTTPServer(paths []string) *HTTPServer {
	return &HTTPServer{Paths: paths}
}

func (s *HTTPServer) HandleRequest(req HTTPRequest) HTTPResponse {
	start := time.Now()

	time.Sleep(1 * time.Millisecond) // parse request

	switch req.Path {
	case "/api/search":
		time.Sleep(time.Duration(rand.IntN(60)+40) * time.Millisecond)
	case "/health":
		time.Sleep(2 * time.Millisecond)
	default:
		time.Sleep(time.Duration(rand.IntN(40)+15) * time.Millisecond)
	}

	time.Sleep(1 * time.Millisecond) // format response

	return HTTPResponse{
		RequestID:  req.ID,
		StatusCode: s.determineStatusCode(req.ID),
		Latency:    time.Since(start),
	}
}

func (s *HTTPServer) determineStatusCode(reqID int) int {
	switch {
	case reqID%11 == 0:
		return 500
	case reqID%7 == 0:
		return 404
	case reqID%13 == 0:
		return 429
	default:
		return 200
	}
}

func (s *HTTPServer) DispatchRequests(count int) <-chan HTTPResponse {
	responses := make(chan HTTPResponse, count)
	for i := 1; i <= count; i++ {
		go func(id int) {
			req := HTTPRequest{
				ID:     id,
				Method: "GET",
				Path:   s.Paths[id%len(s.Paths)],
			}
			responses <- s.HandleRequest(req)
		}(i)
	}
	return responses
}

func collectMetrics(responses <-chan HTTPResponse, count int, wallStart time.Time) ServerMetrics {
	metrics := ServerMetrics{StatusCounts: map[int]int{}}
	for i := 0; i < count; i++ {
		resp := <-responses
		metrics.StatusCounts[resp.StatusCode]++
		metrics.TotalLatency += resp.Latency
		metrics.Responses = append(metrics.Responses, resp)
	}
	metrics.WallClock = time.Since(wallStart)

	slices.SortFunc(metrics.Responses, func(a, b HTTPResponse) int {
		return cmp.Compare(a.Latency, b.Latency)
	})
	return metrics
}

func printServerMetrics(metrics ServerMetrics, requestCount int) {
	p50 := metrics.Responses[len(metrics.Responses)/2].Latency
	p95 := metrics.Responses[int(float64(len(metrics.Responses))*0.95)].Latency
	p99 := metrics.Responses[len(metrics.Responses)-1].Latency

	fmt.Println("=== Server Metrics ===")
	fmt.Printf("  Requests processed:  %d\n", requestCount)
	fmt.Printf("  Wall-clock time:     %v\n", metrics.WallClock.Round(time.Millisecond))
	fmt.Printf("  Throughput:          %.0f req/sec\n",
		float64(requestCount)/metrics.WallClock.Seconds())
	fmt.Printf("  Concurrency gain:    %.1fx\n",
		float64(metrics.TotalLatency)/float64(metrics.WallClock))
	fmt.Println()
	fmt.Println("  Latency percentiles:")
	fmt.Printf("    p50: %v\n", p50.Round(time.Millisecond))
	fmt.Printf("    p95: %v\n", p95.Round(time.Millisecond))
	fmt.Printf("    p99: %v\n", p99.Round(time.Millisecond))
	fmt.Println()
	fmt.Printf("  Status codes: 200=%d  404=%d  429=%d  500=%d\n",
		metrics.StatusCounts[200], metrics.StatusCounts[404],
		metrics.StatusCounts[429], metrics.StatusCounts[500])
}

func main() {
	server := NewHTTPServer([]string{
		"/api/users", "/api/orders", "/api/products", "/health",
		"/api/search", "/api/config", "/api/metrics",
	})

	start := time.Now()
	responses := server.DispatchRequests(simulatedRequestCount)
	metrics := collectMetrics(responses, simulatedRequestCount, start)
	printServerMetrics(metrics, simulatedRequestCount)
}
```

**What's happening here:** 20 HTTP requests are processed concurrently, each in its own goroutine. We compute real server metrics: throughput, latency percentiles (p50/p95/p99), and status code distribution. This is exactly the kind of instrumentation you would add to a production server.

**Key insight:** This is exactly how `net/http` works internally. Each `ListenAndServe` call accepts connections and spawns a goroutine per connection. The server naturally scales because each goroutine handles its request independently. Wall-clock time (~100ms) is a fraction of total work (~1000ms), demonstrating a 10x concurrency gain.

### Verification
```bash
go run main.go
```
Expected output:
```
=== Server Metrics ===
  Requests processed:  20
  Wall-clock time:     95ms
  Throughput:          210 req/sec
  Concurrency gain:    10.5x

  Latency percentiles:
    p50: 35ms
    p95: 85ms
    p99: 98ms

  Status codes: 200=15  404=2  429=1  500=2
```

## Common Mistakes

### Not Buffering the Result Channel

**Wrong -- complete program:**
```go
package main

import (
	"fmt"
	"time"
)

func main() {
	endpoints := []string{"/users", "/orders", "/products", "/reviews", "/config"}
	results := make(chan string) // unbuffered!

	for _, ep := range endpoints {
		go func(e string) {
			time.Sleep(10 * time.Millisecond)
			results <- e // blocks until someone reads
		}(ep)
	}

	for i := 0; i < len(endpoints); i++ {
		fmt.Println(<-results)
	}
}
```

**What happens:** With an unbuffered channel, each goroutine blocks on send until main reads. This effectively serializes the collection, negating the benefit of concurrency.

**Correct -- buffer to expected capacity:**
```go
package main

import (
	"fmt"
	"time"
)

func main() {
	endpoints := []string{"/users", "/orders", "/products", "/reviews", "/config"}
	results := make(chan string, len(endpoints)) // buffered!

	for _, ep := range endpoints {
		go func(e string) {
			time.Sleep(10 * time.Millisecond)
			results <- e // non-blocking: buffer has room
		}(ep)
	}

	for i := 0; i < len(endpoints); i++ {
		fmt.Println(<-results)
	}
}
```

### Forgetting to Collect All Results

**Wrong -- complete program:**
```go
package main

import (
	"fmt"
	"time"
)

func main() {
	results := make(chan string, 10)

	for i := 0; i < 10; i++ {
		go func(id int) {
			time.Sleep(10 * time.Millisecond)
			results <- fmt.Sprintf("response %d", id)
		}(i)
	}

	// Only read 3 responses -- 7 goroutines' results are silently lost
	for i := 0; i < 3; i++ {
		fmt.Println(<-results)
	}
}
```

**What happens:** With a buffered channel, the remaining 7 goroutines complete and their results sit in the buffer until the process exits. With an unbuffered channel, those 7 goroutines would be leaked (blocked on send forever). In a long-running server, this slowly consumes memory.

**Fix:** Always collect exactly as many results as goroutines you launched, or use a `sync.WaitGroup`.

### Not Recovering Panics in Worker Goroutines

**Wrong -- complete program:**
```go
package main

import "fmt"

func handleRequest(id int) {
	if id == 3 {
		panic("nil pointer in request handler")
	}
	fmt.Printf("request %d handled\n", id)
}

func main() {
	for i := 0; i < 5; i++ {
		go handleRequest(i) // if request 3 panics, ENTIRE server crashes
	}
	select {} // block forever (will crash before reaching here)
}
```

**Fix:** Add defer/recover to every worker goroutine:
```go
package main

import "fmt"

func safeHandle(id int) {
	defer func() {
		if r := recover(); r != nil {
			fmt.Printf("request %d panicked: %v\n", id, r)
		}
	}()

	if id == 3 {
		panic("nil pointer in request handler")
	}
	fmt.Printf("request %d handled\n", id)
}

func main() {
	done := make(chan struct{})
	for i := 0; i < 5; i++ {
		go func(id int) {
			safeHandle(id)
			done <- struct{}{}
		}(i)
	}
	for i := 0; i < 5; i++ {
		<-done
	}
}
```

## Review

The pattern rests on three properties working together. Each request runs in
its own goroutine as plain sequential code, so the handler stays simple even
though many run at once; the buffered result channel lets every worker deposit
its outcome without waiting for the collector, so senders never serialize
behind the reader; and because goroutines are independent, one worker's failure
is contained. That containment is only real if you make it real -- an
unrecovered panic in any goroutine tears down the entire process, so every
worker needs its own defer/recover, and you must read exactly as many results
as you launched or the surplus goroutines linger and leak. The payoff is that
wall-clock time approaches the slowest single request rather than the sum,
which is why this same shape underlies Go's `net/http` and gRPC servers.

To prove you own the pattern, build an API gateway that accepts twenty
simulated requests from a channel, launches one goroutine per request that runs
the full lifecycle (parse, a database query with random latency, format), fails
roughly ten percent of them with assorted status codes, and wraps each handler
in defer/recover for panic safety. Collect every result into a buffered channel
sized to the request count, then print a metrics report: throughput, p50/p95/p99
latency, error rate, and status-code distribution. If your report is missing
entries or your program hangs at the end, you have either dropped results or
leaked a goroutine -- the two mistakes this exercise exists to train out.

## Resources
- [Go Tour: Channels](https://go.dev/tour/concurrency/2) -- the basics of sending and receiving, including the buffered form this pattern relies on for result collection.
- [Effective Go: Channels](https://go.dev/doc/effective_go#channels) -- idiomatic channel use and the buffered-channel-as-queue idiom.
- [Go Blog: Go Concurrency Patterns](https://go.dev/blog/pipelines) -- how fan-out and fan-in goroutines compose into larger concurrent structures.
- [net/http: Handler goroutine model](https://pkg.go.dev/net/http#Server) -- the standard-library server whose accept loop spawns one goroutine per connection, the real-world instance of this pattern.

---

Back to [Concurrency](../../concurrency.md) | Next: [08-million-goroutines](../08-million-goroutines/08-million-goroutines.md)
