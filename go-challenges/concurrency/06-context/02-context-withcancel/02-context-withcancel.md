# Exercise 2: Context WithCancel

In real services one user action fans out into several concurrent operations --
a search hits a database, a cache, and a full-text index at once. When the user
cancels, or when the first source answers, every other query should stop
immediately; otherwise those goroutines keep running, holding connections and
memory that nobody will ever read. `context.WithCancel` gives you exactly that
lever: a derived context plus a `cancel` function whose call closes the
context's `Done()` channel and signals every listener at once. Cancellation is
cooperative -- the goroutine must check `ctx.Done()` and choose to stop -- so the
real skill is wiring that check in everywhere it matters.

## What you'll build

```text
02-context-withcancel/
  main.go        multi-source search with cancel, a goroutine-leak demo, the
                 first-result-wins fix, and downward-only cancellation scope
```

- Build: a multi-source search that cancels every in-flight query the moment one wins.
- Implement: `context.WithCancel` with `defer cancel()`, source goroutines that select on `ctx.Done()`, a first-result-wins searcher, and a demo that cancellation flows down to children but never up to the parent.
- Verify: `go run main.go`, watching `runtime.NumGoroutine()` return to its baseline (zero leaks).

### Why a leaked goroutine is a production outage

The real consequence of not using cancellation: in a service handling 10,000 requests per second, each leaking one goroutine, you will exhaust memory in minutes. This is not hypothetical -- it is one of the most common production incidents in Go services.

## Step 1 -- Multi-Source User Search with Cancellation

Build a user search that queries three data sources concurrently. When the user clicks "cancel" (simulated), all ongoing queries stop:

```go
package main

import (
	"context"
	"fmt"
	"math/rand/v2"
	"time"
)

type SearchAggregator struct{}

func NewSearchAggregator() *SearchAggregator {
	return &SearchAggregator{}
}

func (s *SearchAggregator) searchDatabase(ctx context.Context, query string, results chan<- string) {
	delay := time.Duration(100+rand.IntN(200)) * time.Millisecond
	select {
	case <-time.After(delay):
		results <- fmt.Sprintf("database: found user %q in %v", query, delay)
	case <-ctx.Done():
		fmt.Printf("[database]    search cancelled: %v\n", ctx.Err())
	}
}

func (s *SearchAggregator) searchCache(ctx context.Context, query string, results chan<- string) {
	delay := time.Duration(50+rand.IntN(100)) * time.Millisecond
	select {
	case <-time.After(delay):
		results <- fmt.Sprintf("cache: found user %q in %v", query, delay)
	case <-ctx.Done():
		fmt.Printf("[cache]       search cancelled: %v\n", ctx.Err())
	}
}

func (s *SearchAggregator) searchIndex(ctx context.Context, query string, results chan<- string) {
	delay := time.Duration(150+rand.IntN(300)) * time.Millisecond
	select {
	case <-time.After(delay):
		results <- fmt.Sprintf("index: found user %q in %v", query, delay)
	case <-ctx.Done():
		fmt.Printf("[full-index]  search cancelled: %v\n", ctx.Err())
	}
}

func (s *SearchAggregator) SearchAll(ctx context.Context, query string) <-chan string {
	results := make(chan string, 3)
	go s.searchDatabase(ctx, query, results)
	go s.searchCache(ctx, query, results)
	go s.searchIndex(ctx, query, results)
	return results
}

func simulateUserCancel(cancel context.CancelFunc, after time.Duration) {
	time.Sleep(after)
	fmt.Println("\n[user] clicked cancel")
	cancel()
}

func main() {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	aggregator := NewSearchAggregator()

	fmt.Println("Starting user search across 3 sources...")
	results := aggregator.SearchAll(ctx, "alice@example.com")

	go simulateUserCancel(cancel, 120*time.Millisecond)

	for {
		select {
		case result := <-results:
			fmt.Printf("[result] %s\n", result)
		case <-ctx.Done():
			fmt.Printf("\nSearch ended: %v\n", ctx.Err())
			time.Sleep(50 * time.Millisecond)
			return
		}
	}
}
```

### Verification
```bash
go run main.go
```
Expected output (timing varies, some sources may return before cancel):
```
Starting user search across 3 sources...
[result] cache: found user "alice@example.com" in 73ms

[user] clicked cancel
[database]    search cancelled: context canceled
[full-index]  search cancelled: context canceled

Search ended: context canceled
```

When cancel is called, all goroutines listening on `ctx.Done()` receive the signal. Sources that finished before the cancel return results; sources still running get cancelled. No goroutine is left behind.

## Step 2 -- Goroutine Leak: What Happens Without Cancellation

This is the critical anti-pattern. When you launch goroutines without a cancellable context, they keep running even after nobody cares about their results. Run this example and observe the leak:

```go
package main

import (
	"fmt"
	"runtime"
	"time"
)

type LeakySearcher struct{}

func (l *LeakySearcher) search(query string, results chan<- string) {
	time.Sleep(500 * time.Millisecond)
	select {
	case results <- fmt.Sprintf("found: %s", query):
	default:
	}
}

func (l *LeakySearcher) SearchWithoutContext(query string) string {
	results := make(chan string, 3)

	go l.search(query+"-db", results)
	go l.search(query+"-cache", results)
	go l.search(query+"-index", results)

	return <-results
}

func reportGoroutines(label string) int {
	count := runtime.NumGoroutine()
	fmt.Printf("%s: %d\n", label, count)
	return count
}

func main() {
	searcher := &LeakySearcher{}

	before := reportGoroutines("Goroutines before")

	for i := 0; i < 5; i++ {
		result := searcher.SearchWithoutContext(fmt.Sprintf("query-%d", i))
		fmt.Printf("Request %d: %s\n", i, result)
	}

	after := reportGoroutines("\nGoroutines after")
	fmt.Printf("Leaked goroutines: %d\n", after-before)
	fmt.Println("Each request leaks ~2 goroutines (the 2 slower sources).")
	fmt.Println("At 10,000 req/s, this exhausts memory in minutes.")
}
```

### Verification
```bash
go run main.go
```
Expected output:
```
Goroutines before: 1
Request 0: found: query-0-db
Request 1: found: query-1-db
Request 2: found: query-2-db
Request 3: found: query-3-db
Request 4: found: query-4-db

Goroutines after: 11
Leaked goroutines: 10
Each request leaks ~2 goroutines (the 2 slower sources).
At 10,000 req/s, this exhausts memory in minutes.
```

Each request launches 3 goroutines but only consumes 1 result. The other 2 goroutines have no way to know they should stop. This is the most common goroutine leak in production Go code.

## Step 3 -- First Result Wins with Proper Cancellation

Fix the leak from Step 2. Use `WithCancel` to stop all remaining queries as soon as the first result arrives:

```go
package main

import (
	"context"
	"fmt"
	"runtime"
	"time"
)

type DataSource struct {
	Name  string
	Delay time.Duration
}

type SafeSearcher struct {
	sources []DataSource
}

func NewSafeSearcher() *SafeSearcher {
	return &SafeSearcher{
		sources: []DataSource{
			{Name: "database", Delay: 200 * time.Millisecond},
			{Name: "cache", Delay: 80 * time.Millisecond},
			{Name: "index", Delay: 350 * time.Millisecond},
		},
	}
}

func (s *SafeSearcher) querySource(ctx context.Context, source DataSource, results chan<- string) {
	select {
	case <-time.After(source.Delay):
		select {
		case results <- fmt.Sprintf("[%s] found result in %v", source.Name, source.Delay):
		case <-ctx.Done():
		}
	case <-ctx.Done():
		fmt.Printf("  [%s] cancelled, releasing resources\n", source.Name)
	}
}

func (s *SafeSearcher) FirstResult(query string) string {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	results := make(chan string, len(s.sources))
	for _, src := range s.sources {
		go s.querySource(ctx, src, results)
	}

	return <-results
}

func main() {
	searcher := NewSafeSearcher()

	goroutinesBefore := runtime.NumGoroutine()
	fmt.Printf("Goroutines before: %d\n\n", goroutinesBefore)

	for i := 0; i < 5; i++ {
		result := searcher.FirstResult(fmt.Sprintf("query-%d", i))
		fmt.Printf("Request %d: %s\n", i, result)
		time.Sleep(100 * time.Millisecond)
		fmt.Println()
	}

	time.Sleep(200 * time.Millisecond)

	goroutinesAfter := runtime.NumGoroutine()
	fmt.Printf("Goroutines after:  %d\n", goroutinesAfter)
	fmt.Printf("Leaked goroutines: %d (should be 0)\n", goroutinesAfter-goroutinesBefore)
}
```

### Verification
```bash
go run main.go
```
Expected output:
```
Goroutines before: 1

Request 0: [cache] found result in 80ms
  [database] cancelled, releasing resources
  [index] cancelled, releasing resources

Request 1: [cache] found result in 80ms
  [database] cancelled, releasing resources
  [index] cancelled, releasing resources

...

Goroutines after:  1
Leaked goroutines: 0 (should be 0)
```

When `defer cancel()` runs on return, it closes the `Done()` channel, and the remaining goroutines detect this and exit. Zero goroutine leaks, no matter how many requests you handle.

## Step 4 -- Cancellation Propagates Down, Never Up

In a real system, you might cancel a sub-operation without affecting the parent. Cancelling a child context leaves the parent and siblings unaffected:

```go
package main

import (
	"context"
	"fmt"
	"time"
)

const replicaQueryDelay = 200 * time.Millisecond

type ReplicaQueryResult struct {
	Name    string
	Message string
}

type DatabaseCluster struct{}

func NewDatabaseCluster() *DatabaseCluster {
	return &DatabaseCluster{}
}

func (d *DatabaseCluster) QueryReplica(ctx context.Context, name string, done chan<- ReplicaQueryResult) {
	select {
	case <-time.After(replicaQueryDelay):
		done <- ReplicaQueryResult{Name: name, Message: "query complete"}
	case <-ctx.Done():
		done <- ReplicaQueryResult{Name: name, Message: fmt.Sprintf("cancelled (%v)", ctx.Err())}
	}
}

func (d *DatabaseCluster) DemonstrateCancellationScope() {
	parent, cancelParent := context.WithCancel(context.Background())
	defer cancelParent()

	primaryCtx, cancelPrimary := context.WithCancel(parent)
	replicaCtx, cancelReplica := context.WithCancel(parent)
	defer cancelReplica()

	primaryDone := make(chan ReplicaQueryResult, 1)
	replicaDone := make(chan ReplicaQueryResult, 1)

	go d.QueryReplica(primaryCtx, "primary-db", primaryDone)
	go d.QueryReplica(replicaCtx, "replica-db", replicaDone)

	fmt.Println("Cancelling primary query only...")
	cancelPrimary()

	primary := <-primaryDone
	replica := <-replicaDone
	fmt.Printf("%s: %s\n", primary.Name, primary.Message)
	fmt.Printf("%s: %s\n", replica.Name, replica.Message)

	fmt.Printf("\nparent.Err():  %v (unaffected)\n", parent.Err())
	fmt.Printf("primary.Err(): %v (cancelled)\n", primaryCtx.Err())
	fmt.Printf("replica.Err(): %v (still running)\n", replicaCtx.Err())
}

func main() {
	cluster := NewDatabaseCluster()
	cluster.DemonstrateCancellationScope()
}
```

### Verification
```bash
go run main.go
```
Expected output:
```
Cancelling primary query only...
primary-db: cancelled (context canceled)
replica-db: query complete

parent.Err():  <nil> (unaffected)
primary.Err(): context canceled (cancelled)
replica.Err(): <nil> (still running)
```

Cancellation flows down, never up. This is critical: a failing sub-operation should not tear down unrelated parts of the system. The replica query continues undisturbed.

## Common Mistakes

### Forgetting to Call Cancel (Goroutine Leak)
**Wrong:**
```go
package main

import (
	"context"
	"fmt"
)

func main() {
	ctx, cancel := context.WithCancel(context.Background())
	_ = cancel // unused -- resource leak!
	fmt.Printf("ctx.Err(): %v\n", ctx.Err())
}
```
**What happens:** The derived context and its internal goroutine are never cleaned up. The Go runtime cannot garbage-collect the context's internal resources until cancel is called.

**Fix:** Always `defer cancel()` immediately after creating the context:
```go
package main

import (
	"context"
	"fmt"
)

func main() {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	fmt.Printf("ctx.Err(): %v\n", ctx.Err())
}
```

### Not Checking ctx.Done() in the Goroutine
**Wrong:**
```go
func processQueue(ctx context.Context, items []string) {
    for _, item := range items {
        heavyProcessing(item) // never checks ctx.Done() -- runs forever
    }
}
```
**What happens:** The goroutine ignores the cancellation signal and continues consuming CPU and memory. Calling `cancel()` has no effect because nobody is listening.

**Fix:** Check cancellation between units of work:
```go
func processQueue(ctx context.Context, items []string) {
    for _, item := range items {
        select {
        case <-ctx.Done():
            return // stop processing
        default:
        }
        heavyProcessing(item)
    }
}
```

### Passing the Cancel Function to Other Goroutines
Prefer keeping the cancel function close to where the context was created. Passing it to multiple goroutines makes it unclear who is responsible for cancellation, leading to premature or accidental cancellation. If a goroutine needs to signal that an operation should stop, use a separate channel and let the owner call cancel.

## Review

`context.WithCancel` derives a child context and hands back a `cancel` function;
calling it closes the child's `Done()` channel and wakes every goroutine
selecting on it at the same instant. That is the whole mechanism, and it is
cooperative -- the context forcibly kills nothing, so a goroutine that never
checks `ctx.Done()` keeps running no matter how many times you call `cancel()`.
The pattern this unlocks is "first result wins": launch a query per source, take
the first value off the results channel, and `defer cancel()` so returning tears
down the losers. The asymmetry to remember is directional -- cancellation flows
from a parent down to all its descendants but never back up to the parent or
sideways to siblings, so cancelling one sub-operation leaves the rest of the tree
untouched.

Skip the `defer cancel()` and you leak: each abandoned goroutine holds its memory
and often a connection, and at ten thousand requests a second a single leaked
goroutine per request exhausts the box in minutes -- which is why `cancel` is
safe to call more than once and you should always defer it the moment the context
is born. You should now be able to build the canonical drill without looking
back: a concurrent file search across three directories, one goroutine each, a
cancellable context so the first directory to find the file cancels the other
two, and a `runtime.NumGoroutine()` check before and after proving the leak count
is zero.

## Resources
- [Package context: WithCancel](https://pkg.go.dev/context#WithCancel) -- the exact signature and the contract that you must always call the returned cancel.
- [Go Blog: Context](https://go.dev/blog/context) -- how cancellation and deadlines propagate through a context tree.
- [Go Concurrency Patterns: Pipelines](https://go.dev/blog/pipelines) -- the cancellation-via-done-channel pattern that context generalizes.

---

Back to [Concurrency](../../concurrency.md) | Next: [03-context-withtimeout](../03-context-withtimeout/03-context-withtimeout.md)
