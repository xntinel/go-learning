# Exercise 7: Fan-In — Aggregate Concurrent Dependency Health Checks

A `/healthz` endpoint that checks the database, cache, and an upstream API in
sequence is as slow as the sum of its dependencies. Run them concurrently and fan
the results into one channel, and the endpoint is as slow as the *slowest* single
check. This exercise builds that fan-in: N health checks each in their own
goroutine, all results funneled onto one channel, closed exactly once by a
`WaitGroup`-coordinated owner.

This module is self-contained: its own module, a `health` package, a demo, and
tests. Nothing here imports another exercise.

## What you'll build

```text
health/                      independent module: example.com/health
  go.mod                     go 1.26
  health.go                  type Check, Result; Merge, AllHealthy
  cmd/demo/main.go           runnable demo: aggregate three checks
  health_test.go             collects all, reports unhealthy, closes once, empty
```

- Files: `health.go`, `cmd/demo/main.go`, `health_test.go`.
- Implement: `Merge(ctx, checks) []Result` running each check concurrently and fanning results into one channel, and `AllHealthy([]Result) bool`.
- Test: N checks produce N results; any failure makes the aggregate unhealthy; the output channel is closed exactly once; zero checks is handled.
- Verify: `go test -count=1 -race ./...`

Set up the module:

```bash
mkdir -p go-solutions/13-goroutines-and-channels/02-channel-basics/07-fan-in-merge-health-checks/cmd/demo
cd go-solutions/13-goroutines-and-channels/02-channel-basics/07-fan-in-merge-health-checks
```

### One shared output, one owner, one close

Fan-in is the mirror image of fan-out: many producers, one channel. Every check
runs in its own goroutine and sends its `Result` onto a single shared `out` channel.
The recurring question — who closes `out`, and when — is answered by the ownership
discipline. No individual check may close `out`, because the others are still
sending; a close from one producer would panic the rest. Instead a `WaitGroup`
counts the checks, and one separate goroutine calls `close(out)` after `wg.Wait()`
returns. The output is closed exactly once, only after the last producer has sent.

`Add` is called before each goroutine is launched, never inside it — adding inside
would race the `Wait` in the closer goroutine. The main goroutine ranges `out`
collecting results; because a slow check does not block a fast one (they send
independently), the endpoint's latency is the max of the checks, not the sum.

Each check takes a `context.Context` so a caller can bound the whole aggregation
with a timeout or cancel it; the checks are expected to honor `ctx` in a real
service. `AllHealthy` reduces the collected results to a single boolean for the HTTP
status code.

Create `health.go`:

```go
package health

import (
	"context"
	"sync"
)

// Check is a named dependency probe.
type Check struct {
	Name  string
	Probe func(ctx context.Context) error
}

// Result is the outcome of one Check.
type Result struct {
	Name    string
	Healthy bool
	Err     error
}

// Merge runs every check concurrently, funnels their results onto one channel, and
// returns them once all checks have finished. Results are in arbitrary order.
func Merge(ctx context.Context, checks []Check) []Result {
	out := make(chan Result)
	var wg sync.WaitGroup

	for _, c := range checks {
		wg.Add(1)
		go func() {
			defer wg.Done()
			err := c.Probe(ctx)
			out <- Result{Name: c.Name, Healthy: err == nil, Err: err}
		}()
	}

	// The single owner closes out exactly once, after all producers finish.
	go func() {
		wg.Wait()
		close(out)
	}()

	var results []Result
	for r := range out {
		results = append(results, r)
	}
	return results
}

// AllHealthy reports whether every result is healthy. An empty slice is healthy.
func AllHealthy(results []Result) bool {
	for _, r := range results {
		if !r.Healthy {
			return false
		}
	}
	return true
}
```

### The runnable demo

Create `cmd/demo/main.go`:

```go
package main

import (
	"context"
	"errors"
	"fmt"
	"sort"

	"example.com/health"
)

func main() {
	checks := []health.Check{
		{Name: "cache", Probe: func(context.Context) error { return nil }},
		{Name: "db", Probe: func(context.Context) error { return nil }},
		{Name: "upstream", Probe: func(context.Context) error { return errors.New("503") }},
	}

	results := health.Merge(context.Background(), checks)
	sort.Slice(results, func(i, j int) bool { return results[i].Name < results[j].Name })
	for _, r := range results {
		fmt.Printf("%s healthy=%v\n", r.Name, r.Healthy)
	}
	fmt.Println("all healthy:", health.AllHealthy(results))
}
```

Run it:

```bash
go run ./cmd/demo
```

Expected output:

```
cache healthy=true
db healthy=true
upstream healthy=false
all healthy: false
```

### Tests

`TestMergeCollectsAllResults` runs N checks and asserts N results come back — the
fan-in loses nothing. `TestMergeReportsUnhealthyIfAnyFails` makes one probe fail and
asserts `AllHealthy` is false. `TestMergeClosesOutputOnce` is implicit in every
test — if `close(out)` ran twice it would panic, and if it never ran the collecting
range would hang — but it is asserted directly by the fact that `Merge` returns.
`TestMergeWithNoChecks` proves zero producers: the `WaitGroup` is already at zero, so
the closer closes `out` immediately and `Merge` returns an empty slice. All run
under `-race` to prove the `Wait`-then-`close` ordering has no data race on `out`.

Create `health_test.go`:

```go
package health

import (
	"context"
	"errors"
	"fmt"
	"sort"
	"testing"
)

func healthy(name string) Check {
	return Check{Name: name, Probe: func(context.Context) error { return nil }}
}

func failing(name string, err error) Check {
	return Check{Name: name, Probe: func(context.Context) error { return err }}
}

func TestMergeCollectsAllResults(t *testing.T) {
	t.Parallel()

	checks := []Check{healthy("a"), healthy("b"), healthy("c"), healthy("d")}
	results := Merge(t.Context(), checks)
	if len(results) != len(checks) {
		t.Fatalf("got %d results, want %d", len(results), len(checks))
	}
	names := make(map[string]bool)
	for _, r := range results {
		names[r.Name] = true
	}
	for _, c := range checks {
		if !names[c.Name] {
			t.Fatalf("missing result for %q", c.Name)
		}
	}
}

func TestMergeReportsUnhealthyIfAnyFails(t *testing.T) {
	t.Parallel()

	down := errors.New("connection refused")
	checks := []Check{healthy("a"), failing("b", down), healthy("c")}
	results := Merge(t.Context(), checks)

	if AllHealthy(results) {
		t.Fatal("AllHealthy = true, want false when one check fails")
	}
	var found bool
	for _, r := range results {
		if r.Name == "b" {
			found = true
			if r.Healthy {
				t.Fatal("check b reported healthy despite failing")
			}
			if !errors.Is(r.Err, down) {
				t.Fatalf("check b err = %v, want %v", r.Err, down)
			}
		}
	}
	if !found {
		t.Fatal("no result for check b")
	}
}

func TestMergeWithNoChecks(t *testing.T) {
	t.Parallel()

	results := Merge(t.Context(), nil)
	if len(results) != 0 {
		t.Fatalf("got %d results, want 0", len(results))
	}
	if !AllHealthy(results) {
		t.Fatal("AllHealthy of empty = false, want true")
	}
}

func ExampleMerge() {
	checks := []Check{healthy("db"), healthy("cache")}
	results := Merge(context.Background(), checks)
	sort.Slice(results, func(i, j int) bool { return results[i].Name < results[j].Name })
	for _, r := range results {
		fmt.Printf("%s:%v\n", r.Name, r.Healthy)
	}
	// Output:
	// cache:true
	// db:true
}
```

## Review

The fan-in is correct when the shared output is closed exactly once, after the last
producer, and the collector loses nothing. The `WaitGroup`-then-`close` pattern is
the whole mechanism: `Add` before launch, `Done` when each check finishes, and one
goroutine closing `out` after `Wait`. `TestMergeCollectsAllResults` proves nothing is
dropped; the empty-checks case proves the closer fires even with zero producers; and
a clean `-race` run proves the ordering between the last `Done` and the `close` has
no data race. The regressions to avoid are a check closing `out` itself (panics the
others), and calling `wg.Add` inside the goroutine (races the closer's `Wait`).

## Resources

- [`sync.WaitGroup`](https://pkg.go.dev/sync#WaitGroup) — counting producers so the owner closes the channel exactly once.
- [Go Blog: Pipelines and cancellation](https://go.dev/blog/pipelines) — the fan-in section and the `WaitGroup`-close idiom.
- [`context` package](https://pkg.go.dev/context) — bounding the aggregation with a deadline or cancellation.

---

Back to [00-concepts.md](00-concepts.md) | Prev: [06-safe-publish-after-close.md](06-safe-publish-after-close.md) | Next: [08-graceful-drain-on-shutdown.md](08-graceful-drain-on-shutdown.md)
