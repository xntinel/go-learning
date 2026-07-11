# Exercise 3: Select with Timeout

Blocking forever on a channel is rarely acceptable in production. When your
service calls a downstream API, that API can hang, the network can stall, or the
service can be overloaded, and a goroutine that waits without a deadline holds
resources, fills connection pools, and eventually cascades failures upstream. Go
has no timeout keyword; you compose one from `select` and a timer channel. That
composition is elegant, but it hides a trap -- `time.After` allocates a timer
that lives until it fires, so calling it in a loop leaks.

## What you'll build

```text
03-select-with-timeout/
  main.go        timeout-guarded client for a slow service: select with
                 time.After, the loop leak, and the stop-drain-reset timer
```

- Build: a client that gives up on a slow downstream call once a deadline passes.
- Implement: `awaitResponseWithTimeout` using `select` with `time.After`, a request loop that demonstrates the accumulating-timer leak, and a `time.NewTimer` reused across iterations with the stop-drain-reset pattern.
- Verify: `go run main.go`.

### Why timeouts are composed, not built in

Go does not have a built-in timeout keyword. Instead, it composes timeouts from
two primitives: `time.After` (which returns a channel that receives after a
delay) and `select` (which can listen on that channel alongside the work
channel). This composability is elegant but has a subtle trap: `time.After`
allocates a timer that is not garbage collected until it fires. In a loop, this
creates a leak.

The `time.NewTimer` type offers explicit control. You can stop it, reset it, and
drain it. In any code path where timeouts happen repeatedly, `time.NewTimer` is
the safe choice.

## Step 1 -- Basic Request Timeout with time.After

Simulate an HTTP-style client that makes a request to a downstream service and gives up after a deadline. The downstream service is slow (2 seconds), but our timeout is 500ms.

```go
package main

import (
	"fmt"
	"time"
)

const requestTimeout = 500 * time.Millisecond

func simulateSlowService(response chan<- string, latency time.Duration) {
	go func() {
		time.Sleep(latency)
		response <- `{"status": "ok", "data": [1, 2, 3]}`
	}()
}

func awaitResponseWithTimeout(response <-chan string, timeout time.Duration) (string, bool) {
	select {
	case body := <-response:
		return body, true
	case <-time.After(timeout):
		return "", false
	}
}

func main() {
	response := make(chan string)

	simulateSlowService(response, 2*time.Second)

	if body, ok := awaitResponseWithTimeout(response, requestTimeout); ok {
		fmt.Println("response:", body)
	} else {
		fmt.Printf("timeout: downstream service did not respond within %v\n", requestTimeout)
	}
}
```

`time.After(500ms)` returns a `<-chan time.Time` that receives a value after 500ms. The goroutine takes 2 seconds, so the timeout case wins. Without this timeout, the goroutine would block for the full 2 seconds -- and in production, potentially forever.

### Verification
```
timeout: downstream service did not respond within 500ms
```
Change the goroutine sleep to 100ms and run again:
```
response: {"status": "ok", "data": [1, 2, 3]}
```

## Step 2 -- Fast Response Beats the Timeout

When the downstream service responds quickly, the response case is selected. The timer from `time.After` still exists in memory until it fires, but for one-shot operations this overhead is negligible.

```go
package main

import (
	"fmt"
	"time"
)

const requestTimeout = 500 * time.Millisecond

func simulateService(response chan<- string, latency time.Duration, payload string) {
	go func() {
		time.Sleep(latency)
		response <- payload
	}()
}

func awaitResponseWithTimeout(response <-chan string, timeout time.Duration) (string, bool) {
	select {
	case body := <-response:
		return body, true
	case <-time.After(timeout):
		return "", false
	}
}

func main() {
	response := make(chan string)

	simulateService(response, 50*time.Millisecond, `{"user": "alice", "role": "admin"}`)

	if body, ok := awaitResponseWithTimeout(response, requestTimeout); ok {
		fmt.Println("success:", body)
	} else {
		fmt.Println("timeout: service too slow")
	}
}
```

### Verification
```
success: {"user": "alice", "role": "admin"}
```

## Step 3 -- The Cascade: Showing Both Outcomes

Build a function that calls a downstream service with a configurable delay and a fixed timeout. Run it twice to show the fast path (success) and the slow path (timeout).

```go
package main

import (
	"fmt"
	"time"
)

type ServiceCall struct {
	Name    string
	Latency time.Duration
	Timeout time.Duration
}

func (sc ServiceCall) Execute() {
	response := make(chan string, 1)

	go func() {
		time.Sleep(sc.Latency)
		response <- fmt.Sprintf("%s responded", sc.Name)
	}()

	select {
	case body := <-response:
		fmt.Printf("[%s] success: %s\n", sc.Name, body)
	case <-time.After(sc.Timeout):
		fmt.Printf("[%s] timeout after %v (service latency: %v)\n", sc.Name, sc.Timeout, sc.Latency)
	}
}

func main() {
	calls := []ServiceCall{
		{Name: "user-service", Latency: 80 * time.Millisecond, Timeout: 200 * time.Millisecond},
		{Name: "payment-service", Latency: 500 * time.Millisecond, Timeout: 200 * time.Millisecond},
	}

	for _, call := range calls {
		call.Execute()
	}
}
```

### Verification
```
[user-service] success: user-service responded
[payment-service] timeout after 200ms (service latency: 500ms)
```
The user-service responds within the deadline. The payment-service is too slow -- without the timeout, our caller would block for the full 500ms.

## Step 4 -- The time.After Leak in Loops

When `time.After` is called inside a loop, each iteration creates a new timer. If the main case resolves before the timeout, the timer still exists in memory until it fires. In a high-throughput request loop, this wastes significant memory.

```go
package main

import (
	"fmt"
	"time"
)

const (
	totalRequests  = 1000
	requestDelay   = 1 * time.Millisecond
	leakyTimeout   = 1 * time.Second
)

func produceRequests(requests chan<- string, count int) {
	go func() {
		for i := 0; i < count; i++ {
			requests <- fmt.Sprintf("request-%d", i)
			time.Sleep(requestDelay)
		}
		close(requests)
	}()
}

func main() {
	requests := make(chan string, 1)

	produceRequests(requests, totalRequests)

	processed := 0
	for req := range requests {
		// BAD: Each iteration allocates a 1-second timer.
		// We process requests every 1ms, so ~1000 timers accumulate,
		// all scheduled to fire 1 second from now.
		select {
		case <-time.After(leakyTimeout):
			fmt.Println("timeout")
			return
		default:
			_ = req
			processed++
		}
	}
	fmt.Printf("processed %d requests (created ~%d leaked timers)\n", processed, totalRequests)
}
```

### Verification
```
processed 1000 requests (created ~1000 leaked timers)
```
In a real server processing millions of requests, this timer leak puts heavy pressure on the runtime's timer heap and wastes memory.

## Step 5 -- Safe Timeouts with time.NewTimer

Replace `time.After` with `time.NewTimer`. Stop the timer when it is no longer needed, and reset it between iterations. This is what production HTTP servers use internally.

```go
package main

import (
	"fmt"
	"time"
)

const (
	responseInterval = 50 * time.Millisecond
	responseTimeout  = 200 * time.Millisecond
	totalResponses   = 10
)

func produceResponses(responses chan<- string, count int, interval time.Duration) {
	go func() {
		for i := 0; i < count; i++ {
			responses <- fmt.Sprintf("response-%d", i)
			time.Sleep(interval)
		}
		close(responses)
	}()
}

func resetTimer(timer *time.Timer, duration time.Duration) {
	// Stop-drain-reset: prevents a stale timeout from the previous iteration.
	if !timer.Stop() {
		select {
		case <-timer.C:
		default:
		}
	}
	timer.Reset(duration)
}

func consumeWithTimeout(responses <-chan string, timeout time.Duration) {
	timer := time.NewTimer(timeout)
	defer timer.Stop()

	for {
		resetTimer(timer, timeout)

		select {
		case body, ok := <-responses:
			if !ok {
				fmt.Println("all responses received")
				return
			}
			fmt.Println("received:", body)
		case <-timer.C:
			fmt.Printf("timeout: no response for %v, aborting\n", timeout)
			return
		}
	}
}

func main() {
	responses := make(chan string)

	produceResponses(responses, totalResponses, responseInterval)
	consumeWithTimeout(responses, responseTimeout)
}
```

Key points:
- `timeout.Stop()` returns false if the timer already fired. In that case, drain the channel to prevent a stale fire on the next iteration.
- `timeout.Reset()` rearms the timer for the next loop iteration.
- `defer timeout.Stop()` ensures cleanup if the function exits via any path.

### Verification
```
received: response-0
received: response-1
received: response-2
received: response-3
received: response-4
received: response-5
received: response-6
received: response-7
received: response-8
received: response-9
all responses received
```
Change the producer sleep to 300ms to verify the timeout fires:
```
received: response-0
timeout: no response for 200ms, aborting
```

## Common Mistakes

### 1. Using time.After in Hot Loops
Every call allocates a new timer that lives until it fires. In a loop processing thousands of requests per second, this leaks memory rapidly. Always use `time.NewTimer` in loops.

### 2. Not Draining the Timer Channel Before Reset
If the timer fired between `Stop()` and `Reset()`, the channel has a pending value. The next `select` will immediately see the old timeout. The drain pattern prevents this:

```go
if !timer.Stop() {
    select {
    case <-timer.C: // drain the stale value
    default:        // nothing to drain
    }
}
timer.Reset(duration)
```

### 3. Forgetting defer Stop()
If you return from the function without stopping the timer, it remains in the runtime's timer heap until it fires. Always `defer timer.Stop()`.

### 4. Goroutine Leak from Slow Dependencies
When a timeout fires, the goroutine making the slow call still exists. It is blocked on `response <- result`, but nobody is reading from `response` anymore. Using a buffered channel (`make(chan string, 1)`) lets the goroutine send and exit even after the caller moves on.

## Review

`time.After` is the convenient form: it hands back a channel that fires once
after a delay, perfect for a one-shot timeout guarding a single call. The catch
is that each call allocates a timer that survives until it fires, so calling it
once per iteration of a hot loop piles up timers on the runtime's heap even when
the work case wins every time. `time.NewTimer` gives you a single timer you own
across iterations -- `Stop` it, drain a stale value from its channel if `Stop`
reports it already fired, then `Reset` it for the next round. In the HTTP-client
scenario the point of all this is the same: a timeout keeps a goroutine from
blocking forever on a slow dependency, which is how you stop one stalled
downstream from cascading into an outage upstream.

You should be able to explain, from memory, why `time.After` in a loop leaks
timers, write the stop-drain-reset sequence without looking it up, name when
`time.After` is perfectly safe (one-shot operations outside a loop), and say why
a `Stop` that returns false forces you to drain the channel before resetting --
because the timer already fired and left a value that the next `select` would
otherwise pick up as a phantom timeout.

## Resources
- [time.After](https://pkg.go.dev/time#After) -- the one-shot timer channel and the allocation it costs.
- [time.NewTimer](https://pkg.go.dev/time#NewTimer) -- a reusable timer you can Stop and Reset across loop iterations.
- [time.Timer.Reset](https://pkg.go.dev/time#Timer.Reset) -- the exact preconditions for reusing a timer safely.

---

Back to [Concurrency](../../concurrency.md) | Next: [04-select-priority-trick](../04-select-priority-trick/04-select-priority-trick.md)
