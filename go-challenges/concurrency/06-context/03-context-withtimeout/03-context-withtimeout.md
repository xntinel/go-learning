# Exercise 3: Context WithTimeout

Every call to an external service -- a database, a REST API, a gRPC endpoint --
can hang. Network partitions, overloaded servers, and DNS failures can make a
simple HTTP call block for minutes, and without a timeout that goroutine holds a
connection, memory, and a worker-pool slot indefinitely; let hundreds of requests
pile up behind a dead dependency and the whole system stops responding. That is a
cascading failure, and `context.WithTimeout` is the guardrail against it -- a
context that auto-cancels after a fixed duration whether or not anyone calls
`cancel()`, so a 2-second budget on a query means the goroutine is freed within 2
seconds no matter what happens downstream.

## What you'll build

```text
03-context-withtimeout/
  main.go        timeout-protected API clients, the leak from a forgotten
                 cancel, timeout-vs-cancel classification, and deadline inheritance
```

- Build: five programs -- a client that times out on a slow service, one that completes within budget, a leak demonstration, a timeout-vs-cancel classifier, and a parent/child deadline test.
- Implement: `context.WithTimeout` with `defer cancel()`, a `select` over `time.After` and `ctx.Done()`, and `errors.Is` checks against `context.DeadlineExceeded` and `context.Canceled`.
- Verify: `go run main.go`

### Why cancel must be deferred even with a timeout

The cancel function returned by `WithTimeout` must still be deferred. Even if
the timeout fires first, calling `cancel()` releases internal timer resources
immediately instead of waiting for garbage collection. Forgetting it leaves a
timer goroutine alive until the deadline expires; at a thousand requests a
second, those accumulate into real memory pressure, which is exactly what
Step 3 measures.

## Step 1 -- API Client with Timeout

Build a client that calls an external payment verification service. If the service does not respond in 2 seconds, give up and return an error:

```go
package main

import (
	"context"
	"fmt"
	"time"
)

const (
	paymentTimeout    = 2 * time.Second
	paymentServiceLatency = 3 * time.Second
)

type PaymentClient struct {
	timeout time.Duration
}

func NewPaymentClient(timeout time.Duration) *PaymentClient {
	return &PaymentClient{timeout: timeout}
}

func (c *PaymentClient) VerifyTransaction(ctx context.Context, transactionID string) (string, error) {
	fmt.Printf("[payment-api] verifying transaction %s...\n", transactionID)

	select {
	case <-time.After(paymentServiceLatency):
		return "verified", nil
	case <-ctx.Done():
		return "", fmt.Errorf("payment verification failed: %w", ctx.Err())
	}
}

func main() {
	ctx, cancel := context.WithTimeout(context.Background(), paymentTimeout)
	defer cancel()

	client := NewPaymentClient(paymentTimeout)

	fmt.Printf("Calling payment verification service (timeout: %v)...\n", paymentTimeout)
	start := time.Now()

	result, err := client.VerifyTransaction(ctx, "TXN-2024-98765")
	elapsed := time.Since(start).Round(time.Millisecond)

	if err != nil {
		fmt.Printf("[error] %v (after %v)\n", err, elapsed)
		fmt.Println("[action] falling back to manual review queue")
	} else {
		fmt.Printf("[success] %s (after %v)\n", result, elapsed)
	}
}
```

### Verification
```bash
go run main.go
```
Expected output:
```
Calling payment verification service (timeout: 2s)...
[payment-api] verifying transaction TXN-2024-98765...
[error] payment verification failed: context deadline exceeded (after 2s)
[action] falling back to manual review queue
```

The service needed 3 seconds but the context only allowed 2. After 2 seconds, `ctx.Done()` closed, the select picked up the cancellation, and `ctx.Err()` returned `context.DeadlineExceeded`. Without this timeout, the goroutine would block for the full 3 seconds -- or forever if the service is completely down.

## Step 2 -- Fast Response Completes Before Timeout

When the service responds within the timeout, everything proceeds normally. The deferred `cancel()` is still required to free internal timer resources:

```go
package main

import (
	"context"
	"fmt"
	"time"
)

const (
	profileTimeout = 2 * time.Second
	profileLatency = 200 * time.Millisecond
)

type UserProfileClient struct{}

func NewUserProfileClient() *UserProfileClient {
	return &UserProfileClient{}
}

func (c *UserProfileClient) Fetch(ctx context.Context, userID string) (string, error) {
	select {
	case <-time.After(profileLatency):
		return fmt.Sprintf("User{id: %s, name: Alice, plan: premium}", userID), nil
	case <-ctx.Done():
		return "", fmt.Errorf("fetch user profile: %w", ctx.Err())
	}
}

func main() {
	ctx, cancel := context.WithTimeout(context.Background(), profileTimeout)
	defer cancel()

	client := NewUserProfileClient()

	fmt.Printf("Fetching user profile (timeout: %v, expected latency: %v)...\n", profileTimeout, profileLatency)
	start := time.Now()

	profile, err := client.Fetch(ctx, "user-42")
	elapsed := time.Since(start).Round(time.Millisecond)

	if err != nil {
		fmt.Printf("[error] %v (after %v)\n", err, elapsed)
	} else {
		fmt.Printf("[success] %s (after %v)\n", profile, elapsed)
	}

	fmt.Printf("Context error after success: %v (nil means timeout has not fired)\n", ctx.Err())
}
```

### Verification
```bash
go run main.go
```
Expected output:
```
Fetching user profile (timeout: 2s, expected latency: 200ms)...
[success] User{id: user-42, name: Alice, plan: premium} (after 200ms)
Context error after success: <nil> (nil means timeout has not fired)
```

The operation finished in 200ms, well within the 2-second timeout. `ctx.Err()` is nil because the timeout has not fired yet. The deferred `cancel()` stops the internal timer immediately on function return.

## Step 3 -- Resource Leak When You Forget Cancel

This demonstrates what happens when you do not call `cancel()`. The internal timer goroutine leaks:

```go
package main

import (
	"context"
	"fmt"
	"runtime"
	"time"
)

const leakyTimeoutDuration = 10 * time.Second

func createLeakyTimeout() {
	ctx, _ := context.WithTimeout(context.Background(), leakyTimeoutDuration)
	_ = ctx
}

func createProperTimeout() {
	ctx, cancel := context.WithTimeout(context.Background(), leakyTimeoutDuration)
	defer cancel()
	_ = ctx
}

func reportGoroutines(label string) int {
	count := runtime.NumGoroutine()
	fmt.Printf("%s: %d\n", label, count)
	return count
}

func main() {
	baseline := reportGoroutines("Baseline goroutines")
	fmt.Println()

	fmt.Println("Creating 100 timeouts WITHOUT cancel...")
	for i := 0; i < 100; i++ {
		createLeakyTimeout()
	}
	leaked := reportGoroutines("Goroutines after leaky calls")
	fmt.Printf("Leaked: %d\n\n", leaked-baseline)

	fmt.Println("Creating 100 timeouts WITH proper cancel...")
	for i := 0; i < 100; i++ {
		createProperTimeout()
	}
	proper := reportGoroutines("Goroutines after proper calls")
	fmt.Printf("Leaked from proper: %d\n", proper-leaked)
	fmt.Println("\nThe leaky calls left timer goroutines running for 10 seconds each.")
	fmt.Println("In a server handling 1000 req/s, this consumes gigabytes of memory.")
}
```

### Verification
```bash
go run main.go
```
Expected output:
```
Baseline goroutines: 1

Creating 100 timeouts WITHOUT cancel...
Goroutines after leaky calls: 101
Leaked: 100

Creating 100 timeouts WITH proper cancel...
Goroutines after proper calls: 101
Leaked from proper: 0

The leaky calls left timer goroutines running for 10 seconds each.
In a server handling 1000 req/s, this consumes gigabytes of memory.
```

Each forgotten `cancel()` leaves a timer goroutine running until the timeout expires. In a long-running server, these accumulate and cause memory exhaustion.

## Step 4 -- Distinguishing Timeout vs Manual Cancellation

When diagnosing issues, you need to know whether an operation was cancelled by the caller or timed out on its own. `ctx.Err()` tells you which:

```go
package main

import (
	"context"
	"errors"
	"fmt"
	"time"
)

const (
	serviceLatency     = 5 * time.Second
	shortTimeout       = 100 * time.Millisecond
	longTimeout        = 5 * time.Second
	cancelDelay        = 100 * time.Millisecond
)

type APIClient struct{}

func NewAPIClient() *APIClient {
	return &APIClient{}
}

func (c *APIClient) Call(ctx context.Context, name string) error {
	select {
	case <-time.After(serviceLatency):
		return nil
	case <-ctx.Done():
		return ctx.Err()
	}
}

func classifyContextError(err error) string {
	if errors.Is(err, context.DeadlineExceeded) {
		return "TIMEOUT: service too slow, consider increasing timeout or adding cache"
	}
	if errors.Is(err, context.Canceled) {
		return "CANCELLED: caller gave up (client disconnect, user abort)"
	}
	return "UNKNOWN"
}

func main() {
	client := NewAPIClient()

	fmt.Println("=== Case 1: Timeout ===")
	ctx1, cancel1 := context.WithTimeout(context.Background(), shortTimeout)
	defer cancel1()

	err1 := client.Call(ctx1, "inventory")
	fmt.Printf("Error: %v\n", err1)
	fmt.Printf("Diagnosis: %s\n\n", classifyContextError(err1))

	fmt.Println("=== Case 2: Manual Cancel ===")
	ctx2, cancel2 := context.WithTimeout(context.Background(), longTimeout)

	go func() {
		time.Sleep(cancelDelay)
		cancel2()
	}()

	err2 := client.Call(ctx2, "inventory")
	fmt.Printf("Error: %v\n", err2)
	fmt.Printf("Diagnosis: %s\n", classifyContextError(err2))
}
```

### Verification
```bash
go run main.go
```
Expected output:
```
=== Case 1: Timeout ===
Error: context deadline exceeded
Diagnosis: TIMEOUT: service too slow, consider increasing timeout or adding cache

=== Case 2: Manual Cancel ===
Error: context canceled
Diagnosis: CANCELLED: caller gave up (client disconnect, user abort)
```

This distinction drives real decisions: timeouts trigger alerts about slow dependencies; cancellations are usually normal (clients disconnecting) and should not page the on-call engineer. Use `errors.Is(err, context.DeadlineExceeded)` vs `errors.Is(err, context.Canceled)` to classify them in your logging and metrics.

## Step 5 -- Child Cannot Extend Parent Timeout

A fundamental rule: a child context cannot have a longer timeout than its parent. The shorter deadline always wins. This prevents a downstream layer from circumventing the caller's budget:

```go
package main

import (
	"context"
	"fmt"
	"time"
)

const (
	gatewayTimeout  = 1 * time.Second
	requestedDBTimeout = 10 * time.Second
)

func main() {
	gateway, cancelGateway := context.WithTimeout(context.Background(), gatewayTimeout)
	defer cancelGateway()

	dbQuery, cancelDB := context.WithTimeout(gateway, requestedDBTimeout)
	defer cancelDB()

	gatewayDeadline, _ := gateway.Deadline()
	dbDeadline, _ := dbQuery.Deadline()

	fmt.Printf("Gateway deadline: %v (1s from now)\n",
		time.Until(gatewayDeadline).Round(time.Millisecond))
	fmt.Printf("DB query requested: 10s\n")
	fmt.Printf("DB query actual:    %v (inherits gateway's shorter deadline)\n",
		time.Until(dbDeadline).Round(time.Millisecond))
	fmt.Println("\nYou can tighten a timeout (shorter) but never loosen it (longer).")
}
```

### Verification
```bash
go run main.go
```
Expected output:
```
Gateway deadline: 1s (1s from now)
DB query requested: 10s
DB query actual:    1s (inherits gateway's shorter deadline)

You can tighten a timeout (shorter) but never loosen it (longer).
```

## Common Mistakes

### Not Deferring Cancel on WithTimeout
**Wrong:**
```go
package main

import (
	"context"
	"fmt"
	"time"
)

func main() {
	ctx, _ := context.WithTimeout(context.Background(), 5*time.Second)
	// forgot defer cancel() -- timer goroutine leaks until timeout fires!
	fmt.Printf("ctx.Err(): %v\n", ctx.Err())
}
```
**What happens:** The internal timer runs for the full 5 seconds even if the operation finishes in 10 milliseconds. In a server handling thousands of requests, timer goroutines pile up.

**Fix:** Always `defer cancel()` immediately:
```go
package main

import (
	"context"
	"fmt"
	"time"
)

func main() {
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	fmt.Printf("ctx.Err(): %v\n", ctx.Err())
}
```

### Ignoring ctx.Err() After Timeout
**Wrong:**
```go
select {
case <-ctx.Done():
    return nil // caller has no idea what went wrong
}
```
**Fix:**
```go
select {
case <-ctx.Done():
    return fmt.Errorf("operation failed: %w", ctx.Err())
}
```

Always wrap and return `ctx.Err()` so callers can distinguish timeout from cancellation and make appropriate retry or fallback decisions.

### Setting Timeout Longer Than Parent's
As shown in Step 5, setting a child timeout longer than the parent's is not an error, but it is misleading code. The child inherits the parent's shorter deadline, and the longer timeout has no effect. This confuses developers reading the code.

## Review

`context.WithTimeout(parent, duration)` returns a context whose `Done()` channel
closes when either the duration elapses or someone calls `cancel()`, and
`ctx.Err()` then tells the two apart: `context.DeadlineExceeded` for a timeout,
`context.Canceled` for a manual stop. That distinction is not cosmetic -- a
timeout usually means a dependency is slow and deserves an alert, while a
cancellation usually means a client disconnected and should not page anyone, so
classify with `errors.Is` before you log or retry. The cancel function is
mandatory even when the timeout is what fires: it releases the internal timer
immediately instead of leaving a timer goroutine alive until the deadline, and at
scale forgetting it is the leak Step 3 makes visible. One structural guarantee
holds it all together: a child context cannot loosen its parent's deadline -- the
shorter one always wins -- so a downstream layer can tighten the budget but never
escape it. Put a timeout on every call that crosses a process boundary.

To prove it end to end, build a client that calls two services -- a fast user
service (100ms latency, 500ms timeout) and a slow recommendation service (2s
latency, 300ms timeout) -- each behind its own `WithTimeout` and `defer cancel()`,
with `Call` selecting between `time.After(latency)` and `ctx.Done()`. The user
service should return OK and the recommendation service should fail with
`context deadline exceeded`. If you can write that from the pattern alone, the
timeout, the deferred cancel, and the error classification have all landed.

## Resources

- [Package context: WithTimeout](https://pkg.go.dev/context#WithTimeout) -- the signature and the rule that the returned cancel must be called.
- [Go Blog: Context](https://go.dev/blog/context) -- the design of context propagation, deadlines, and cancellation across API boundaries.
- [Dave Cheney: Context is for Cancellation](https://dave.cheney.net/2017/08/20/context-isnt-for-cancellation) -- an opinionated take on what context should and should not carry.

---

Back to [Concurrency](../../concurrency.md) | Next: [04-context-withdeadline](../04-context-withdeadline/04-context-withdeadline.md)
