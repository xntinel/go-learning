# Exercise 8: A Health Aggregator That Fans One Checker Port Over DB, Cache, and Queue

A `/healthz` endpoint has to probe wildly different dependencies — a database, a
cache, a message queue — each with its own big API. Segregation lets all of them
share one narrow seam: a `Checker` with a single `Check(ctx) error` method. This
module builds an aggregator that runs a slice of named checkers concurrently with
a per-check timeout and reports 200 when all pass, 503 when any fails.

## What you'll build

```text
health/                        independent module: example.com/health
  go.mod                       go 1.24
  health.go                    Checker (one method); Aggregator runs named checks concurrently with timeout
  adapters.go                  DB/cache/queue fat types adapted to Checker
  cmd/
    demo/
      main.go                  registers healthy + failing checkers, prints aggregate
  health_test.go               healthy/failing/slow checkers; 503 on any failure; timeout via context
```

Files: `health.go`, `adapters.go`, `cmd/demo/main.go`, `health_test.go`.
Implement: `Checker interface { Check(ctx) error }`, an `Aggregator` that fans checks out concurrently with `context.WithTimeout` and a `sync.WaitGroup`, and an `http.Handler` returning 200/503.
Test: fake checkers (healthy, error, blocks-past-timeout); assert per-name status, that the slow one is unhealthy via deadline, and overall 503; run under `-race`.
Verify: `go test -count=1 -race ./...`

Set up the module:

```bash
mkdir -p go-solutions/08-interfaces/06-interface-segregation/08-health-check-aggregator/cmd/demo
cd go-solutions/08-interfaces/06-interface-segregation/08-health-check-aggregator
go mod edit -go=1.24
```

### One narrow seam over heterogeneous dependencies

The database driver has dozens of methods; the cache client has a dozen; the
queue client has its own. None of that is relevant to a health probe, which needs
exactly one thing: "are you reachable right now?" So each dependency is adapted to
a one-method `Checker`. The DB adapter's `Check` runs a ping query; the cache
adapter's `Check` does a trivial round-trip; the queue adapter's `Check` verifies
the connection. The aggregator holds a slice of `namedChecker` and does not know
or care what any of them concretely is — segregation lets heterogeneous
dependencies share a uniform narrow seam.

The aggregator runs the checks *concurrently* because a health endpoint must be
fast and one slow dependency should not serialize behind the others. Each check
runs in its own goroutine under a `sync.WaitGroup`; a per-check
`context.WithTimeout` bounds how long a hung dependency can stall the probe. If a
checker ignores the deadline and blocks, the aggregator still returns once the
timeout fires — but note the honest caveat: the goroutine running a truly
unkillable check will linger until it returns; the point of passing `ctx` is that
a well-behaved checker watches `ctx.Done()` and abandons its work. The aggregator
records that check as failed with the context error.

Results are collected into a map keyed by name. Because multiple goroutines write
results, each writes to its own preallocated slot (indexed, no shared map
writes), which is race-free without a mutex; the `WaitGroup` join establishes the
happens-before for the read. The HTTP handler serializes the result to JSON and
sets 200 if every check passed, 503 otherwise — the standard health-endpoint
contract a load balancer reads.

Create `health.go`:

```go
package health

import (
	"context"
	"encoding/json"
	"net/http"
	"sync"
	"time"
)

// Checker is the one-method seam every dependency adapts to.
type Checker interface {
	Check(ctx context.Context) error
}

// namedChecker pairs a Checker with the name reported in the health output.
type namedChecker struct {
	name    string
	checker Checker
}

// Status is one dependency's health result.
type Status struct {
	Name    string `json:"name"`
	Healthy bool   `json:"healthy"`
	Error   string `json:"error,omitempty"`
}

// Aggregator fans a per-check timeout over a set of named Checkers concurrently.
type Aggregator struct {
	checks  []namedChecker
	timeout time.Duration
}

// NewAggregator builds an aggregator with a per-check timeout.
func NewAggregator(timeout time.Duration) *Aggregator {
	return &Aggregator{timeout: timeout}
}

// Register adds a named checker.
func (a *Aggregator) Register(name string, c Checker) {
	a.checks = append(a.checks, namedChecker{name: name, checker: c})
}

// Run executes every check concurrently under the per-check timeout and returns
// the per-name statuses plus whether all passed.
func (a *Aggregator) Run(ctx context.Context) ([]Status, bool) {
	results := make([]Status, len(a.checks))
	var wg sync.WaitGroup

	for i, nc := range a.checks {
		wg.Add(1)
		go func() {
			defer wg.Done()
			cctx, cancel := context.WithTimeout(ctx, a.timeout)
			defer cancel()
			err := nc.checker.Check(cctx)
			st := Status{Name: nc.name, Healthy: err == nil}
			if err != nil {
				st.Error = err.Error()
			}
			results[i] = st
		}()
	}
	wg.Wait()

	allOK := true
	for _, st := range results {
		if !st.Healthy {
			allOK = false
			break
		}
	}
	return results, allOK
}

// ServeHTTP writes the aggregate as JSON: 200 if all healthy, 503 otherwise.
func (a *Aggregator) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	statuses, allOK := a.Run(r.Context())
	w.Header().Set("Content-Type", "application/json")
	if allOK {
		w.WriteHeader(http.StatusOK)
	} else {
		w.WriteHeader(http.StatusServiceUnavailable)
	}
	_ = json.NewEncoder(w).Encode(statuses)
}
```

Create `adapters.go`. Three fat dependencies, each adapted to `Checker`:

```go
package health

import "context"

// dbClient models a database driver with a large API; only Ping matters here.
type dbClient struct{ reachable bool }

func (d *dbClient) Ping(_ context.Context) error {
	if !d.reachable {
		return context.DeadlineExceeded
	}
	return nil
}

// Query, Exec, Begin, ... stand in for the rest of the fat driver surface.
func (d *dbClient) Query(context.Context, string) error { return nil }
func (d *dbClient) Exec(context.Context, string) error  { return nil }

// dbChecker adapts dbClient to the one-method Checker.
type dbChecker struct{ db *dbClient }

func (c dbChecker) Check(ctx context.Context) error { return c.db.Ping(ctx) }

// cacheClient models a cache with many methods; only a round-trip matters.
type cacheClient struct{ reachable bool }

func (c *cacheClient) Get(context.Context, string) (string, error) { return "", nil }
func (c *cacheClient) Set(context.Context, string, string) error   { return nil }

func (c *cacheClient) Roundtrip(_ context.Context) error {
	if !c.reachable {
		return context.DeadlineExceeded
	}
	return nil
}

type cacheChecker struct{ cache *cacheClient }

func (c cacheChecker) Check(ctx context.Context) error { return c.cache.Roundtrip(ctx) }

// NewDBChecker and NewCacheChecker expose the adapters for wiring.
func NewDBChecker(reachable bool) Checker {
	return dbChecker{db: &dbClient{reachable: reachable}}
}

func NewCacheChecker(reachable bool) Checker {
	return cacheChecker{cache: &cacheClient{reachable: reachable}}
}

var (
	_ Checker = dbChecker{}
	_ Checker = cacheChecker{}
)
```

### The runnable demo

Create `cmd/demo/main.go`:

```go
package main

import (
	"context"
	"fmt"
	"time"

	"example.com/health"
)

func main() {
	agg := health.NewAggregator(100 * time.Millisecond)
	agg.Register("database", health.NewDBChecker(true))
	agg.Register("cache", health.NewCacheChecker(false)) // unreachable

	statuses, allOK := agg.Run(context.Background())
	for _, st := range statuses {
		fmt.Printf("%s healthy=%t\n", st.Name, st.Healthy)
	}
	fmt.Printf("all healthy: %t\n", allOK)
}
```

Run it:

```bash
go run ./cmd/demo
```

Expected output:

```
database healthy=true
cache healthy=false
all healthy: false
```

### Tests

The tests register three kinds of checker: one healthy, one that returns an
error, and one that blocks past the timeout to prove the deadline path. The slow
checker watches `ctx.Done()` so it exits cleanly at the deadline (a well-behaved
check), and the aggregator records it as unhealthy via the context error.

Create `health_test.go`:

```go
package health

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"
)

// funcChecker adapts a function to Checker (a tiny test seam).
type funcChecker func(ctx context.Context) error

func (f funcChecker) Check(ctx context.Context) error { return f(ctx) }

var errUnhealthy = errors.New("dependency down")

func TestAggregatorReportsPerNameStatus(t *testing.T) {
	t.Parallel()

	agg := NewAggregator(50 * time.Millisecond)
	agg.Register("ok", funcChecker(func(context.Context) error { return nil }))
	agg.Register("bad", funcChecker(func(context.Context) error { return errUnhealthy }))

	statuses, allOK := agg.Run(context.Background())
	if allOK {
		t.Fatal("allOK should be false when one check fails")
	}
	byName := map[string]Status{}
	for _, st := range statuses {
		byName[st.Name] = st
	}
	if !byName["ok"].Healthy {
		t.Fatal("ok should be healthy")
	}
	if byName["bad"].Healthy {
		t.Fatal("bad should be unhealthy")
	}
	if byName["bad"].Error != errUnhealthy.Error() {
		t.Fatalf("bad error = %q, want %q", byName["bad"].Error, errUnhealthy.Error())
	}
}

func TestSlowCheckerFailsViaDeadline(t *testing.T) {
	t.Parallel()

	agg := NewAggregator(20 * time.Millisecond)
	// A well-behaved slow check that watches ctx.Done and exits at the deadline.
	agg.Register("slow", funcChecker(func(ctx context.Context) error {
		select {
		case <-time.After(time.Second):
			return nil
		case <-ctx.Done():
			return ctx.Err()
		}
	}))

	start := time.Now()
	statuses, allOK := agg.Run(context.Background())
	elapsed := time.Since(start)

	if allOK {
		t.Fatal("slow check should be reported unhealthy")
	}
	if statuses[0].Healthy {
		t.Fatal("slow check should be unhealthy")
	}
	if elapsed > 500*time.Millisecond {
		t.Fatalf("aggregate took %v; timeout should have bounded it", elapsed)
	}
}

func TestServeHTTPReturns503OnFailure(t *testing.T) {
	t.Parallel()

	agg := NewAggregator(50 * time.Millisecond)
	agg.Register("ok", funcChecker(func(context.Context) error { return nil }))
	agg.Register("bad", funcChecker(func(context.Context) error { return errUnhealthy }))

	req := httptest.NewRequest(http.MethodGet, "/healthz", nil)
	rec := httptest.NewRecorder()
	agg.ServeHTTP(rec, req)

	if rec.Code != http.StatusServiceUnavailable {
		t.Fatalf("status = %d, want 503", rec.Code)
	}
}

func TestServeHTTPReturns200WhenAllHealthy(t *testing.T) {
	t.Parallel()

	agg := NewAggregator(50 * time.Millisecond)
	agg.Register("db", funcChecker(func(context.Context) error { return nil }))
	agg.Register("cache", funcChecker(func(context.Context) error { return nil }))

	req := httptest.NewRequest(http.MethodGet, "/healthz", nil)
	rec := httptest.NewRecorder()
	agg.ServeHTTP(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200", rec.Code)
	}
}
```

## Review

The aggregator is correct when every dependency, however fat its real API,
reaches it through the single `Checker.Check` method, and when the concurrent
fan-out is race-free — each goroutine writes its own preallocated slot and the
`WaitGroup` join establishes the ordering, so `go test -race` is clean. The
timeout is the production-critical part: a health endpoint must not hang behind
one wedged dependency, so each check runs under `context.WithTimeout` and a
well-behaved checker abandons on `ctx.Done()`. The honest limitation to state is
that a checker that ignores its context can still leak a goroutine past the
deadline; the port's contract is that a check honors cancellation. The HTTP
contract — 200 all-healthy, 503 any-failure — is what a load balancer keys on.

## Resources

- [context package (WithTimeout, Done, Err)](https://pkg.go.dev/context)
- [sync.WaitGroup](https://pkg.go.dev/sync#WaitGroup)
- [net/http/httptest package](https://pkg.go.dev/net/http/httptest)
- [Go Code Review Comments: Interfaces](https://go.dev/wiki/CodeReviewComments#interfaces)

---

Back to [00-concepts.md](00-concepts.md) | Previous: [07-compose-role-interfaces.md](07-compose-role-interfaces.md) | Next: [09-segregate-pubsub-port.md](09-segregate-pubsub-port.md)
