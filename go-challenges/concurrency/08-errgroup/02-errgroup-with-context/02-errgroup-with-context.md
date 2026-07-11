# Exercise 2: Errgroup with Context -- Dashboard Data Loader

A dashboard page loads from four internal APIs at once -- user profile, recent orders, notifications, recommendations. If the orders API is down and errors after 50ms, the naive approach lets the other three keep running for another two seconds even though the page already cannot render completely. In production that waste is concrete: HTTP connections held open to services already under load, goroutines blocked on responses nobody will read, memory pinned until they finally return, and -- when the failing dependency is slow rather than fast -- goroutine leaks that accumulate request after request until the process runs out of memory. The fix is to cancel every remaining request the moment one fails, which is exactly what `context.WithCancel` provides and what the errgroup-with-context pattern does automatically when the first goroutine returns an error.

## What you'll build

```text
02-errgroup-with-context/
  main.go        a dashboard loader shown three ways: no cancellation (wasted
                 work), context cancellation (fail fast), and a leak demo
```

- Build: a concurrent dashboard data loader that stops all sibling requests as soon as one API fails.
- Implement: `LoadWithoutCancellation` (the wasteful baseline), `LoadWithCancellation` using `context.WithCancel` plus `cancel()` in the error path and `select` on `ctx.Done()`, a `LeakyLoader` that demonstrates stuck goroutines, and a full `Load` that threads `ctx` through each fetch.
- Verify: `go run main.go`

### Why cooperative cancellation needs an active check

`context.WithCancel` does not stop anything by itself -- it only closes `ctx.Done()`. A goroutine notices only if it selects on that channel, so cancellation is cooperative: each fetch must wrap its blocking wait in a `select` that races the work against `ctx.Done()` and returns early when the context is cancelled. Skip that check and the goroutine runs to completion regardless, which is precisely how the leak demo strands workers on a channel send nobody will ever receive.

`golang.org/x/sync/errgroup.WithContext` packages the whole pattern: it hands you a group and a derived context, cancels that context automatically when the first `Go` returns a non-nil error, and drops the manual `WaitGroup`, `Once`, `cancel()`, and error-capture mutex. The cooperative rule does not change -- your functions still have to check `ctx.Done()` -- but the plumbing that triggers cancellation is handled for you.

## Step 1 -- Without Cancellation (Wasted Resources)

First, observe the problem. Four API calls run concurrently. The orders API fails at 50ms, but the other three keep running until they finish:

```go
package main

import (
	"fmt"
	"sync"
	"time"
)

type APIEndpoint struct {
	Name    string
	Latency time.Duration
	Fail    bool
}

type DashboardLoader struct {
	endpoints []APIEndpoint
}

func NewDashboardLoader(endpoints []APIEndpoint) *DashboardLoader {
	return &DashboardLoader{endpoints: endpoints}
}

func (dl *DashboardLoader) LoadWithoutCancellation() error {
	start := time.Now()

	var wg sync.WaitGroup
	var once sync.Once
	var firstErr error

	for _, api := range dl.endpoints {
		wg.Add(1)
		go func() {
			defer wg.Done()

			time.Sleep(api.Latency)
			if api.Fail {
				once.Do(func() {
					firstErr = fmt.Errorf("%s: 503 service unavailable", api.Name)
				})
				fmt.Printf("  [%v] %s: FAILED\n", time.Since(start).Round(time.Millisecond), api.Name)
				return
			}
			fmt.Printf("  [%v] %s: completed (wasted work!)\n", time.Since(start).Round(time.Millisecond), api.Name)
		}()
	}

	wg.Wait()
	elapsed := time.Since(start).Round(time.Millisecond)
	fmt.Printf("\nError: %v\n", firstErr)
	fmt.Printf("Total time: %v -- waited for ALL goroutines despite knowing error at 50ms\n", elapsed)
	return firstErr
}

func main() {
	loader := NewDashboardLoader([]APIEndpoint{
		{"user-profile", 200 * time.Millisecond, false},
		{"recent-orders", 50 * time.Millisecond, true},
		{"notifications", 300 * time.Millisecond, false},
		{"recommendations", 400 * time.Millisecond, false},
	})

	fmt.Println("=== Without Cancellation ===")
	_ = loader.LoadWithoutCancellation()
}
```

**Expected output:**
```
=== Without Cancellation ===
  [50ms] recent-orders: FAILED
  [200ms] user-profile: completed (wasted work!)
  [300ms] notifications: completed (wasted work!)
  [400ms] recommendations: completed (wasted work!)

Error: recent-orders: 503 service unavailable
Total time: 400ms -- waited for ALL goroutines despite knowing error at 50ms
```

The error was known at 50ms, but the program waited 400ms for all goroutines to finish. Those 350ms of extra work are pure waste -- the dashboard will show an error page regardless.

## Step 2 -- With Context Cancellation (Fail Fast)

Now add `context.WithCancel`. When the first error occurs, cancel the context. All other goroutines check `ctx.Done()` and bail out early:

```go
package main

import (
	"context"
	"fmt"
	"sync"
	"time"
)

type APIEndpoint struct {
	Name    string
	Latency time.Duration
	Fail    bool
}

type DashboardLoader struct {
	endpoints []APIEndpoint
}

func NewDashboardLoader(endpoints []APIEndpoint) *DashboardLoader {
	return &DashboardLoader{endpoints: endpoints}
}

func (dl *DashboardLoader) LoadWithCancellation() error {
	start := time.Now()

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	var wg sync.WaitGroup
	var once sync.Once
	var firstErr error

	for _, api := range dl.endpoints {
		wg.Add(1)
		go func() {
			defer wg.Done()

			select {
			case <-ctx.Done():
				fmt.Printf("  [%v] %s: cancelled before starting\n",
					time.Since(start).Round(time.Millisecond), api.Name)
				return
			default:
			}

			select {
			case <-ctx.Done():
				fmt.Printf("  [%v] %s: cancelled while waiting\n",
					time.Since(start).Round(time.Millisecond), api.Name)
				return
			case <-time.After(api.Latency):
			}

			if api.Fail {
				once.Do(func() {
					firstErr = fmt.Errorf("%s: 503 service unavailable", api.Name)
					cancel()
				})
				fmt.Printf("  [%v] %s: FAILED -- cancelling siblings\n",
					time.Since(start).Round(time.Millisecond), api.Name)
				return
			}

			fmt.Printf("  [%v] %s: completed\n",
				time.Since(start).Round(time.Millisecond), api.Name)
		}()
	}

	wg.Wait()
	elapsed := time.Since(start).Round(time.Millisecond)
	fmt.Printf("\nError: %v\n", firstErr)
	fmt.Printf("Total time: %v -- siblings cancelled shortly after the failure\n", elapsed)
	return firstErr
}

func main() {
	loader := NewDashboardLoader([]APIEndpoint{
		{"user-profile", 200 * time.Millisecond, false},
		{"recent-orders", 50 * time.Millisecond, true},
		{"notifications", 300 * time.Millisecond, false},
		{"recommendations", 400 * time.Millisecond, false},
	})

	fmt.Println("=== With Context Cancellation ===")
	_ = loader.LoadWithCancellation()
}
```

**Expected output:**
```
=== With Context Cancellation ===
  [50ms] recent-orders: FAILED -- cancelling siblings
  [50ms] user-profile: cancelled while waiting
  [50ms] notifications: cancelled while waiting
  [50ms] recommendations: cancelled while waiting

Error: recent-orders: 503 service unavailable
Total time: 50ms -- siblings cancelled shortly after the failure
```

Total time dropped from 400ms to 50ms. The moment `recent-orders` fails and calls `cancel()`, the `select` statement in every other goroutine detects `ctx.Done()` and exits. No wasted connections, no lingering goroutines.

## Step 3 -- Goroutine Leak Without Cancellation

Goroutine leaks are a real production problem. When a goroutine blocks on a channel send or sleep and nobody cancels it, it stays alive forever, consuming memory:

```go
package main

import (
	"fmt"
	"runtime"
	"sync"
	"time"
)

const (
	apiSimulatedLatency = 200 * time.Millisecond
	leakCheckDelay      = 50 * time.Millisecond
	abortWaitDuration   = 300 * time.Millisecond
)

type LeakyLoader struct {
	apis []string
}

func NewLeakyLoader(apis []string) *LeakyLoader {
	return &LeakyLoader{apis: apis}
}

func (ll *LeakyLoader) LoadAndLeak() {
	var wg sync.WaitGroup
	var once sync.Once
	var firstErr error

	results := make(chan string) // unbuffered channel -- receivers might never read

	for _, api := range ll.apis {
		wg.Add(1)
		go func() {
			defer wg.Done()
			time.Sleep(apiSimulatedLatency)
			results <- fmt.Sprintf("data from %s", api) // BLOCKS if nobody reads
		}()
	}

	go func() {
		firstResult := <-results
		fmt.Printf("  Got: %s\n", firstResult)
		once.Do(func() {
			firstErr = fmt.Errorf("decided to abort after first result")
		})
	}()

	time.Sleep(abortWaitDuration)
	_ = firstErr
	// The other 2 goroutines are stuck on `results <- ...` forever.
	// They cannot be garbage collected because the channel is still referenced.
}

func main() {
	fmt.Println("=== Goroutine Leak Demo ===")
	fmt.Printf("Goroutines before: %d\n", runtime.NumGoroutine())

	loader := NewLeakyLoader([]string{"user-profile", "recent-orders", "notifications"})
	loader.LoadAndLeak()

	time.Sleep(leakCheckDelay)
	fmt.Printf("Goroutines after (leaky): %d -- leaked goroutines are still alive!\n", runtime.NumGoroutine())

	// In a real server, this happens on every request.
	// 1000 requests/sec * 3 leaked goroutines = 3000 leaked goroutines/sec.
	// The process eventually runs out of memory and crashes.
}
```

**Expected output:**
```
=== Goroutine Leak Demo ===
Goroutines before: 1
  Got: data from user-profile
Goroutines after (leaky): 4 -- leaked goroutines are still alive!
```

The goroutine count increased by 3 (the API goroutines) and 2 of them are stuck forever trying to send on a channel that nobody reads. With context cancellation, each goroutine would check `ctx.Done()` before the channel send and exit cleanly.

## Step 4 -- Complete Dashboard Loader with Cancellation

Put it all together into a production-style dashboard loader. Each API fetch respects context cancellation. Pass `ctx` to any function that accepts it (like `http.NewRequestWithContext` in real code):

```go
package main

import (
	"context"
	"fmt"
	"sync"
	"time"
)

type DashboardData struct {
	UserName        string
	OrderCount      int
	Notifications   int
	Recommendations []string
}

type APICallFn func(context.Context) error

type DashboardLoader struct {
	ordersDown bool
}

func NewDashboardLoader(ordersDown bool) *DashboardLoader {
	return &DashboardLoader{ordersDown: ordersDown}
}

func (dl *DashboardLoader) Load() (*DashboardData, error) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	var wg sync.WaitGroup
	var once sync.Once
	var firstErr error

	var data DashboardData
	var mu sync.Mutex

	calls := dl.buildAPICalls(&data, &mu)

	for _, call := range calls {
		wg.Add(1)
		go func() {
			defer wg.Done()
			if err := call(ctx); err != nil {
				once.Do(func() {
					firstErr = err
					cancel()
				})
			}
		}()
	}

	wg.Wait()

	if firstErr != nil {
		return nil, firstErr
	}
	return &data, nil
}

func (dl *DashboardLoader) buildAPICalls(data *DashboardData, mu *sync.Mutex) []APICallFn {
	return []APICallFn{
		dl.fetchUserProfile(data, mu),
		dl.fetchRecentOrders(data, mu),
		dl.fetchNotifications(data, mu),
		dl.fetchRecommendations(data, mu),
	}
}

func (dl *DashboardLoader) fetchUserProfile(data *DashboardData, mu *sync.Mutex) APICallFn {
	return func(ctx context.Context) error {
		if err := simulateAPI(ctx, 100*time.Millisecond); err != nil {
			return fmt.Errorf("user-profile: %w", err)
		}
		mu.Lock()
		data.UserName = "alice"
		mu.Unlock()
		return nil
	}
}

func (dl *DashboardLoader) fetchRecentOrders(data *DashboardData, mu *sync.Mutex) APICallFn {
	return func(ctx context.Context) error {
		if dl.ordersDown {
			time.Sleep(40 * time.Millisecond)
			return fmt.Errorf("recent-orders: 503 service unavailable")
		}
		if err := simulateAPI(ctx, 80*time.Millisecond); err != nil {
			return fmt.Errorf("recent-orders: %w", err)
		}
		mu.Lock()
		data.OrderCount = 42
		mu.Unlock()
		return nil
	}
}

func (dl *DashboardLoader) fetchNotifications(data *DashboardData, mu *sync.Mutex) APICallFn {
	return func(ctx context.Context) error {
		if err := simulateAPI(ctx, 120*time.Millisecond); err != nil {
			return fmt.Errorf("notifications: %w", err)
		}
		mu.Lock()
		data.Notifications = 7
		mu.Unlock()
		return nil
	}
}

func (dl *DashboardLoader) fetchRecommendations(data *DashboardData, mu *sync.Mutex) APICallFn {
	return func(ctx context.Context) error {
		if err := simulateAPI(ctx, 150*time.Millisecond); err != nil {
			return fmt.Errorf("recommendations: %w", err)
		}
		mu.Lock()
		data.Recommendations = []string{"item-1", "item-2", "item-3"}
		mu.Unlock()
		return nil
	}
}

func simulateAPI(ctx context.Context, latency time.Duration) error {
	select {
	case <-ctx.Done():
		return ctx.Err()
	case <-time.After(latency):
		return nil
	}
}

func printDashboardResult(data *DashboardData, err error, start time.Time) {
	if err != nil {
		fmt.Printf("Error: %v\n", err)
	} else {
		fmt.Printf("Dashboard: user=%s, orders=%d, notifications=%d, recs=%d\n",
			data.UserName, data.OrderCount, data.Notifications, len(data.Recommendations))
	}
	fmt.Printf("Time: %v\n", time.Since(start).Round(time.Millisecond))
}

func main() {
	fmt.Println("=== Dashboard Data Loader ===")

	fmt.Println("\n--- Scenario 1: All APIs healthy ---")
	start := time.Now()
	loader := NewDashboardLoader(false)
	data, err := loader.Load()
	printDashboardResult(data, err, start)

	fmt.Println("\n--- Scenario 2: Orders API failing ---")
	start = time.Now()
	loader = NewDashboardLoader(true)
	data, err = loader.Load()
	if err != nil {
		fmt.Printf("Error: %v\n", err)
	}
	fmt.Printf("Time: %v (fast failure, no wasted work)\n", time.Since(start).Round(time.Millisecond))
}
```

**Expected output:**
```
=== Dashboard Data Loader ===

--- Scenario 1: All APIs healthy ---
Dashboard: user=alice, orders=42, notifications=7, recs=3
Time: 150ms

--- Scenario 2: Orders API failing ---
Error: recent-orders: 503 service unavailable
Time: 40ms (fast failure, no wasted work)
```

The `simulateAPI` function uses the same `select` pattern you would use with `http.NewRequestWithContext` in real code. When the context is cancelled, the simulated API call returns immediately with `context.Canceled`.

The `golang.org/x/sync/errgroup` package provides exactly this pattern via `errgroup.WithContext`:

```go
// With errgroup.WithContext, the same code becomes:
//   g, ctx := errgroup.WithContext(context.Background())
//   g.Go(func() error { return fetchUserProfile(ctx) })
//   g.Go(func() error { return fetchOrders(ctx) })
//   g.Go(func() error { return fetchNotifications(ctx) })
//   g.Go(func() error { return fetchRecommendations(ctx) })
//   err := g.Wait()
//
// No WaitGroup, no Once, no manual cancel(), no mutex for error capture.
// The context is automatically cancelled when the first Go() returns an error.
```

## Verification

At this point, verify:
1. Without cancellation, total time equals the slowest API (~400ms)
2. With cancellation, total time equals the time to first failure (~50ms)
3. The goroutine leak demo shows goroutine count increasing
4. The complete dashboard loader returns in ~40ms when an API is down

## Common Mistakes

### Not checking ctx.Done() in goroutines

**Wrong:**
```go
go func() {
    defer wg.Done()
    time.Sleep(10 * time.Second) // blocks regardless of cancellation
    results <- data
}()
```

**What happens:** The context is cancelled but the goroutine does not notice. It runs the full 10 seconds, then tries to send on a channel that may already be abandoned. Context cancellation is cooperative -- goroutines must check.

**Fix:** Use `select` with `ctx.Done()`:
```go
go func() {
    defer wg.Done()
    select {
    case <-ctx.Done():
        return
    case <-time.After(10 * time.Second):
        results <- data
    }
}()
```

### Returning ctx.Err() when your task is the first to fail

**Wrong:**
```go
if somethingFailed {
    return ctx.Err() // might be nil if you are the first to fail!
}
```

**What happens:** If your goroutine is the first to fail, the context has not been cancelled yet. `ctx.Err()` returns nil. Your error is silently lost.

**Fix:** Return your own descriptive error. Only return `ctx.Err()` when reacting to a sibling's cancellation:
```go
if somethingFailed {
    return fmt.Errorf("orders API returned 503")
}
```

### Forgetting defer cancel() on the parent

**Wrong:**
```go
ctx, cancel := context.WithCancel(context.Background())
// no defer cancel()
// if loadDashboard returns early on error, cancel is never called
```

**What happens:** The context and its resources are never freed. The Go vet tool will warn about this.

**Fix:** Always `defer cancel()` immediately after creating the context.

## Review

Context cancellation exists to stop wasted work the instant a result is already known to be an error: without it, every sibling goroutine runs to completion regardless, and the ones blocked on a channel or a slow call leak until they finally return. The working pattern is small and fixed -- `context.WithCancel`, `cancel()` in the error handler, and a `select` on `ctx.Done()` inside every goroutine -- and it is cooperative, so a goroutine that never checks `ctx.Done()` never notices the cancellation. Two habits keep it correct: always `defer cancel()` so the context's resources are released even on an early return, and return your own descriptive error when your task is the one that failed, falling back to `ctx.Err()` only when you are reacting to a sibling's cancellation, since your context has not been cancelled yet at the moment you fail first. `golang.org/x/sync/errgroup.WithContext` bakes this in -- it cancels the derived context automatically when the first `Go` returns non-nil -- while leaving the cooperative check to you.

Run the full program and confirm you can see each claim: without cancellation all four API calls finish even after the failure at 50ms, with cancellation the siblings exit within milliseconds of that failure, the leak demo shows the goroutine count climb and stay elevated, and the complete loader returns in about 40ms when orders is down while still succeeding cleanly when every API is healthy.

## Resources
- [context.WithCancel documentation](https://pkg.go.dev/context#WithCancel) -- the constructor that returns the cancel func driving this whole pattern.
- [Go Blog: Context](https://go.dev/blog/context) -- how context propagates cancellation and deadlines across API boundaries.
- [errgroup.WithContext documentation](https://pkg.go.dev/golang.org/x/sync/errgroup#WithContext) -- the group that auto-cancels its context on the first error, replacing the manual scaffolding.
- [Go Concurrency Patterns: Context](https://go.dev/blog/context) -- the worked patterns for wiring context through concurrent request fan-out.

---

Back to [Concurrency](../../concurrency.md) | Next: [03-errgroup-setlimit](../03-errgroup-setlimit/03-errgroup-setlimit.md)
