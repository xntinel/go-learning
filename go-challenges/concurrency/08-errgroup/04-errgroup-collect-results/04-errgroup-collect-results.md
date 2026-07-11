# Exercise 4: Errgroup Collect Results -- Parallel Database Queries

An analytics dashboard needs five database queries -- revenue, active users,
conversion, top products, error rates -- and each takes 100-300ms. Run them one
after another and the page waits over a second; run them in parallel and it
waits for the slowest, about 300ms. The catch is collecting the answers:
goroutines produce results, but the errgroup shape only returns an `error` from
`Wait()`, with no channel for data. This exercise works through the safe ways
for goroutines to write results into shared state -- index-based slots, a
mutex-guarded slice, a keyed map -- and shows why a naive `append` is a data
race.

## What you'll build

```text
04-errgroup-collect-results/
  main.go        parallel query runner collecting results four ways: index,
                 mutex, partial-on-error, map (each step a runnable program)
```

- Build: a parallel query runner that fans out five queries and collects their results four different ways.
- Implement: index-based `RunAllIndexBased`, mutex-protected `CollectAll`, context-cancelled `RunWithPartialResults`, and map-based `QueryAllRegions`.
- Verify: `go run main.go`, and `go run -race main.go` to catch the unsafe append in Step 1.

### Why collecting results needs a pattern

The problem: goroutines produce results, but the errgroup pattern only returns an error from `Wait()`. There is no return value for data. You need a pattern for goroutines to write their results to a shared data structure without data races.

Two patterns exist:
1. **Index-based**: Pre-allocate a slice of known size. Goroutine `i` writes to `results[i]`. No mutex needed because each goroutine touches a different memory location.
2. **Mutex-protected**: When results are filtered, combined, or keyed by non-sequential identifiers, protect the shared structure with `sync.Mutex`.

## Step 1 -- The Data Race (What Goes Wrong Without Protection)

Naive `append` from multiple goroutines is a data race. Run with `-race` to see it:

```go
package main

import (
	"fmt"
	"sync"
	"time"
)

const querySimulatedLatency = 50 * time.Millisecond

type QueryResult struct {
	Name  string
	Value string
}

type UnsafeQueryRunner struct {
	queryNames []string
}

func NewUnsafeQueryRunner(queryNames []string) *UnsafeQueryRunner {
	return &UnsafeQueryRunner{queryNames: queryNames}
}

func (qr *UnsafeQueryRunner) RunAllUnsafe() []QueryResult {
	var wg sync.WaitGroup
	var results []QueryResult // shared, unprotected

	for _, q := range qr.queryNames {
		wg.Add(1)
		go func() {
			defer wg.Done()
			time.Sleep(querySimulatedLatency)
			// DATA RACE: multiple goroutines call append concurrently.
			// append modifies the slice header (length, capacity) and may
			// reallocate the backing array. This corrupts the slice.
			results = append(results, QueryResult{Name: q, Value: "some-data"})
		}()
	}

	wg.Wait()
	return results
}

func main() {
	fmt.Println("=== Unsafe Collection (data race) ===")
	fmt.Println("Run with: go run -race main.go")

	runner := NewUnsafeQueryRunner([]string{"revenue", "active-users", "conversion", "top-products", "error-rate"})
	results := runner.RunAllUnsafe()

	fmt.Printf("Got %d results (may be wrong or corrupted due to race)\n", len(results))
}
```

**Expected output (with -race flag):**
```
=== Unsafe Collection (data race) ===
Run with: go run -race main.go
WARNING: DATA RACE
...
Got N results (may be wrong or corrupted due to race)
```

The race detector catches this immediately. In production without `-race`, the corruption is silent: you might get 3 results instead of 5, or garbage data. This class of bug is notoriously hard to reproduce because it depends on goroutine scheduling.

## Step 2 -- Index-Based Collection (No Mutex Needed)

When you know the number of results upfront and each goroutine maps to a fixed index, pre-allocate the slice. Each goroutine writes to its own slot:

```go
package main

import (
	"fmt"
	"sync"
	"time"
)

type QuerySpec struct {
	Name    string
	Latency time.Duration
	Value   float64
}

type QueryResult struct {
	Name    string
	Value   float64
	Latency time.Duration
}

type QueryRunner struct {
	queries []QuerySpec
}

func NewQueryRunner(queries []QuerySpec) *QueryRunner {
	return &QueryRunner{queries: queries}
}

func (qr *QueryRunner) RunAllIndexBased() []QueryResult {
	results := make([]QueryResult, len(qr.queries)) // pre-allocate exact size

	var wg sync.WaitGroup

	for i, q := range qr.queries {
		wg.Add(1)
		go func() {
			defer wg.Done()

			time.Sleep(q.Latency) // simulate database query

			// SAFE: each goroutine writes to a unique index.
			// Different indices are different memory locations.
			// The slice header (length, capacity, pointer) is never modified.
			results[i] = QueryResult{
				Name:    q.Name,
				Value:   q.Value,
				Latency: q.Latency,
			}
		}()
	}

	wg.Wait()
	return results
}

func printQueryResults(results []QueryResult, elapsed time.Duration) {
	fmt.Printf("\nDashboard Data (loaded in %v):\n", elapsed)
	for _, r := range results {
		fmt.Printf("  %-20s = %12.2f  (query took %v)\n", r.Name, r.Value, r.Latency)
	}
}

func main() {
	runner := NewQueryRunner([]QuerySpec{
		{"total-revenue", 150 * time.Millisecond, 1_247_893.50},
		{"active-users", 80 * time.Millisecond, 42_381},
		{"conversion-rate", 120 * time.Millisecond, 3.7},
		{"top-products", 200 * time.Millisecond, 15},
		{"error-rate", 60 * time.Millisecond, 0.02},
	})

	fmt.Println("=== Index-Based Collection (no mutex) ===")
	start := time.Now()

	results := runner.RunAllIndexBased()

	printQueryResults(results, time.Since(start).Round(time.Millisecond))
}
```

**Expected output:**
```
=== Index-Based Collection (no mutex) ===

Dashboard Data (loaded in 200ms):
  total-revenue        =  1247893.50  (query took 150ms)
  active-users         =    42381.00  (query took 80ms)
  conversion-rate      =        3.70  (query took 120ms)
  top-products         =       15.00  (query took 200ms)
  error-rate           =        0.02  (query took 60ms)
```

All 5 queries ran in parallel. Total time is ~200ms (the slowest query), not ~610ms (sum). Results are ordered by index, matching the input order, regardless of which query finished first.

Run with `go run -race main.go` to confirm: no data race warnings.

## Step 3 -- Mutex-Protected Collection (Heterogeneous Results)

When results do not map to predictable indices -- for example, only some queries produce results, or you are aggregating data from varying sources -- use a mutex:

```go
package main

import (
	"fmt"
	"sync"
	"time"
)

type Metric struct {
	Source string
	Name   string
	Value  float64
}

type DataSource struct {
	Name    string
	Latency time.Duration
	Metrics []Metric
}

type MetricCollector struct {
	sources []DataSource
	mu      sync.Mutex
	metrics []Metric
}

func NewMetricCollector(sources []DataSource) *MetricCollector {
	return &MetricCollector{sources: sources}
}

func (mc *MetricCollector) CollectAll() []Metric {
	var wg sync.WaitGroup

	for _, src := range mc.sources {
		wg.Add(1)
		go func() {
			defer wg.Done()
			mc.fetchFromSource(src)
		}()
	}

	wg.Wait()
	return mc.metrics
}

func (mc *MetricCollector) fetchFromSource(src DataSource) {
	time.Sleep(src.Latency)

	if src.Metrics == nil {
		return // this source had no data -- nothing to collect
	}

	mc.mu.Lock()
	mc.metrics = append(mc.metrics, src.Metrics...)
	mc.mu.Unlock()
}

func printMetrics(metrics []Metric, sourceCount int, elapsed time.Duration) {
	fmt.Printf("Collected %d metrics from %d sources in %v:\n", len(metrics), sourceCount, elapsed)
	for _, m := range metrics {
		fmt.Printf("  [%-12s] %-20s = %.2f\n", m.Source, m.Name, m.Value)
	}
}

func main() {
	sources := []DataSource{
		{"postgres", 100 * time.Millisecond, []Metric{
			{Source: "postgres", Name: "total-revenue", Value: 1_247_893.50},
			{Source: "postgres", Name: "order-count", Value: 8_429},
		}},
		{"redis", 30 * time.Millisecond, []Metric{
			{Source: "redis", Name: "cache-hit-rate", Value: 94.7},
		}},
		{"prometheus", 150 * time.Millisecond, []Metric{
			{Source: "prometheus", Name: "p99-latency-ms", Value: 247},
			{Source: "prometheus", Name: "error-rate", Value: 0.02},
			{Source: "prometheus", Name: "qps", Value: 12_450},
		}},
		{"empty-source", 50 * time.Millisecond, nil},
	}

	collector := NewMetricCollector(sources)

	fmt.Println("=== Mutex-Protected Collection ===")
	start := time.Now()

	metrics := collector.CollectAll()

	printMetrics(metrics, len(sources), time.Since(start).Round(time.Millisecond))
}
```

**Expected output:**
```
=== Mutex-Protected Collection ===
Collected 6 metrics from 4 sources in 150ms:
  [redis       ] cache-hit-rate       = 94.70
  [postgres    ] total-revenue        = 1247893.50
  [postgres    ] order-count          = 8429.00
  [prometheus  ] p99-latency-ms       = 247.00
  [prometheus  ] error-rate           = 0.02
  [prometheus  ] qps                  = 12450.00
```

The order depends on goroutine scheduling (redis finishes first at 30ms, then postgres at 100ms, then prometheus at 150ms). The `empty-source` contributed nothing. The mutex protects `append` because multiple goroutines modify the slice header.

## Step 4 -- Collecting Partial Results on Error

When some queries fail but you still want results from the ones that succeeded, combine index-based collection with context cancellation:

```go
package main

import (
	"context"
	"fmt"
	"sync"
	"time"
)

type DashboardQuery struct {
	Name    string
	Latency time.Duration
	Fail    bool
}

type DashboardResult struct {
	Name  string
	Value string
	OK    bool
}

type QueryRunner struct {
	queries []DashboardQuery
}

func NewQueryRunner(queries []DashboardQuery) *QueryRunner {
	return &QueryRunner{queries: queries}
}

func (qr *QueryRunner) RunWithPartialResults() ([]DashboardResult, error) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	results := make([]DashboardResult, len(qr.queries))

	var wg sync.WaitGroup
	var once sync.Once
	var firstErr error

	for i, q := range qr.queries {
		wg.Add(1)
		go func() {
			defer wg.Done()

			select {
			case <-ctx.Done():
				results[i] = DashboardResult{Name: q.Name, OK: false}
				return
			case <-time.After(q.Latency):
			}

			if q.Fail {
				once.Do(func() {
					firstErr = fmt.Errorf("query %q: connection timeout after %v", q.Name, q.Latency)
					cancel()
				})
				results[i] = DashboardResult{Name: q.Name, OK: false}
				return
			}

			results[i] = DashboardResult{
				Name:  q.Name,
				Value: fmt.Sprintf("data-for-%s", q.Name),
				OK:    true,
			}
		}()
	}

	wg.Wait()
	return results, firstErr
}

func printPartialResults(results []DashboardResult, total int, elapsed time.Duration) {
	fmt.Printf("\nQuery results (in %v):\n", elapsed)
	succeeded := 0
	for _, r := range results {
		status := "FAIL"
		if r.OK {
			status = " OK "
			succeeded++
		}
		fmt.Printf("  [%s] %-15s %s\n", status, r.Name, r.Value)
	}
	fmt.Printf("\nSucceeded: %d/%d\n", succeeded, total)
}

func main() {
	runner := NewQueryRunner([]DashboardQuery{
		{"revenue", 80 * time.Millisecond, false},
		{"active-users", 50 * time.Millisecond, false},
		{"conversion", 120 * time.Millisecond, true}, // THIS QUERY FAILS
		{"top-products", 200 * time.Millisecond, false},
		{"error-rate", 40 * time.Millisecond, false},
	})

	fmt.Println("=== Partial Results on Error ===")
	start := time.Now()

	results, err := runner.RunWithPartialResults()

	printPartialResults(results, len(results), time.Since(start).Round(time.Millisecond))
	if err != nil {
		fmt.Printf("Error: %v\n", err)
	}
}
```

**Expected output:**
```
=== Partial Results on Error ===

Query results (in 120ms):
  [ OK ] error-rate      data-for-error-rate
  [ OK ] active-users    data-for-active-users
  [FAIL] conversion
  [ OK ] revenue         data-for-revenue
  [FAIL] top-products

Succeeded: 3/5
Error: query "conversion": connection timeout after 120ms
```

Queries that completed before the failure (error-rate at 40ms, active-users at 50ms, revenue at 80ms) have their results. Conversion failed at 120ms, triggering cancellation. Top-products (200ms) was cancelled before it could complete. The dashboard can render a partial view with the 3 successful queries and show a "data unavailable" message for the other 2.

## Step 5 -- Map-Based Collection for Named Results

When query results are keyed by string identifiers rather than sequential indices, use a mutex-protected map:

```go
package main

import (
	"fmt"
	"sync"
	"time"
)

const regionQueryLatency = 80 * time.Millisecond

type RegionStats struct {
	Region   string
	Revenue  float64
	Orders   int
	AvgValue float64
}

type RegionQueryRunner struct {
	regionNames []string
	mu          sync.Mutex
	results     map[string]RegionStats
}

func NewRegionQueryRunner(regionNames []string) *RegionQueryRunner {
	return &RegionQueryRunner{
		regionNames: regionNames,
		results:     make(map[string]RegionStats),
	}
}

func (rqr *RegionQueryRunner) QueryAllRegions() map[string]RegionStats {
	var wg sync.WaitGroup

	for _, region := range rqr.regionNames {
		wg.Add(1)
		go func() {
			defer wg.Done()
			rqr.queryRegionShard(region)
		}()
	}

	wg.Wait()
	return rqr.results
}

func (rqr *RegionQueryRunner) queryRegionShard(region string) {
	time.Sleep(regionQueryLatency)

	stats := rqr.buildRegionStats(region)

	rqr.mu.Lock()
	rqr.results[region] = stats
	rqr.mu.Unlock()
}

func (rqr *RegionQueryRunner) buildRegionStats(region string) RegionStats {
	stats := RegionStats{Region: region}
	switch region {
	case "us-east":
		stats.Revenue, stats.Orders = 523_400, 4_200
	case "us-west":
		stats.Revenue, stats.Orders = 312_100, 2_800
	case "eu-central":
		stats.Revenue, stats.Orders = 445_700, 3_600
	case "ap-southeast":
		stats.Revenue, stats.Orders = 198_300, 1_900
	}
	if stats.Orders > 0 {
		stats.AvgValue = stats.Revenue / float64(stats.Orders)
	}
	return stats
}

func printRegionalBreakdown(regionNames []string, data map[string]RegionStats, elapsed time.Duration) {
	fmt.Printf("Regional breakdown (loaded in %v):\n", elapsed)
	var totalRevenue float64
	var totalOrders int
	for _, region := range regionNames {
		s := data[region]
		fmt.Printf("  %-15s  revenue=$%10.2f  orders=%5d  avg=$%.2f\n",
			s.Region, s.Revenue, s.Orders, s.AvgValue)
		totalRevenue += s.Revenue
		totalOrders += s.Orders
	}
	fmt.Printf("  %-15s  revenue=$%10.2f  orders=%5d\n", "TOTAL", totalRevenue, totalOrders)
}

func main() {
	regionNames := []string{"us-east", "us-west", "eu-central", "ap-southeast"}
	runner := NewRegionQueryRunner(regionNames)

	fmt.Println("=== Map-Based Collection ===")
	start := time.Now()

	data := runner.QueryAllRegions()

	printRegionalBreakdown(regionNames, data, time.Since(start).Round(time.Millisecond))
}
```

**Expected output:**
```
=== Map-Based Collection ===
Regional breakdown (loaded in 80ms):
  us-east          revenue=$ 523400.00  orders= 4200  avg=$124.62
  us-west          revenue=$ 312100.00  orders= 2800  avg=$111.46
  eu-central       revenue=$ 445700.00  orders= 3600  avg=$123.81
  ap-southeast     revenue=$ 198300.00  orders= 1900  avg=$104.37
  TOTAL            revenue=$1479500.00  orders=12500
```

The map requires a mutex because concurrent writes to a Go map are a race condition. Iterating the map after `wg.Wait()` is safe because all writes are complete (the Wait establishes a happens-before relationship).

## Verification

At this point, verify:
1. `go run -race main.go` catches the unsafe append in Step 1
2. Index-based collection produces ordered results with no race
3. Mutex-protected collection handles variable-length results from multiple sources
4. Partial results show empty slots for failed/cancelled queries
5. Map-based collection works for string-keyed results

## Common Mistakes

### Reading results before Wait returns

**Wrong:**
```go
for i, q := range queries {
    wg.Add(1)
    go func() {
        defer wg.Done()
        results[i] = runQuery(q)
    }()
}
fmt.Println(results) // DATA RACE: goroutines may still be writing
wg.Wait()
```

**What happens:** You read the results slice while goroutines are still writing to it. The race detector catches this, but in production without `-race`, you get intermittent wrong data.

**Fix:** Always read results AFTER `wg.Wait()` returns. The `Wait()` call establishes a happens-before relationship -- all goroutine writes are visible after Wait.

### Using index-based pattern with a zero-length slice

**Wrong:**
```go
results := make([]QueryResult, 0) // length 0, capacity 0
results[i] = value // PANIC: index out of range
```

**Fix:** Pre-allocate with `make([]QueryResult, len(queries))`.

### Holding the mutex too long

**Wrong:**
```go
mu.Lock()
result := expensiveQuery() // holds the lock for 200ms
results = append(results, result)
mu.Unlock()
```

**What happens:** Other goroutines block on `mu.Lock()` for the entire query duration. Concurrency is effectively serialized.

**Fix:** Do the work outside the lock, only lock for the append:
```go
result := expensiveQuery() // no lock held
mu.Lock()
results = append(results, result)
mu.Unlock()
```

## Review

The errgroup (and WaitGroup) shape gives you an `error` back from `Wait()` and
nothing else, so returning data is on you, and which collection pattern to
reach for depends entirely on how results map to storage. When there is one
result per task and the order is known, pre-allocate `results[len(tasks)]` and
let goroutine `i` write `results[i]`: distinct indices are distinct memory, so
no mutex is needed and the output stays in input order. When the count is
unknown or filtered, guard an `append` with a `sync.Mutex`; when results are
keyed by a string rather than a position, guard a map the same way, because
concurrent map writes are an outright race. The rule underneath all three is
timing -- read the collected results only after `Wait()` returns, never before,
because `Wait` is what establishes the happens-before that makes every
goroutine's write visible.

Run the program under `-race` and each pattern proves itself. Step 1's naive
`append` from many goroutines trips the detector immediately, because `append`
rewrites the slice header and may reallocate the backing array under other
writers. The index-based collector runs clean and ordered with no lock at all;
the mutex-protected collector absorbs a variable number of metrics from sources
that each contribute a different count; the partial-results runner uses context
cancellation to leave zero-value slots for the queries that failed or were
cancelled, which you detect after `Wait`; and the map-based runner keys results
by region under a lock. One efficiency habit ties them together -- do the
expensive query outside the critical section and hold the mutex only for the
`append` itself, or you serialize the very concurrency you built.

## Resources
- [Go Memory Model: happens-before](https://go.dev/ref/mem) -- why a `Wait()` or lock is required before reading what other goroutines wrote.
- [Go Race Detector](https://go.dev/doc/articles/race_detector) -- the tool that catches the unsafe append in Step 1, and how to run it.
- [sync.Mutex documentation](https://pkg.go.dev/sync#Mutex) -- the lock that guards the shared slice and map in the mutex-protected patterns.
- [errgroup package documentation](https://pkg.go.dev/golang.org/x/sync/errgroup) -- the Group whose Go/Wait shape motivates this whole collection problem.

---

Back to [Concurrency](../../concurrency.md) | Next: [05-errgroup-vs-waitgroup](../05-errgroup-vs-waitgroup/05-errgroup-vs-waitgroup.md)
