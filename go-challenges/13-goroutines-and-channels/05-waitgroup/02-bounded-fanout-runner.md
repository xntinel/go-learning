# Exercise 2: Bounded-Concurrency Runner With a Semaphore Channel

Unbounded fan-out is a production hazard: one goroutine per item over a large batch
means goroutine count and memory scale with the input, and a big enough batch
exhausts the process or hammers a downstream dependency. This module caps in-flight
work with a buffered-channel semaphore while keeping the WaitGroup join and the
mutex-guarded collection from Exercise 1.

This module is fully self-contained: its own module, demo, and tests.

## What you'll build

```text
boundedrunner/             independent module: example.com/boundedrunner
  go.mod                   go 1.25
  runner.go                RunWithLimit(jobs, limit) caps in-flight goroutines
  cmd/
    demo/
      main.go              runnable demo: 6 jobs, limit 2, reports observed peak
  runner_test.go           peak<=limit test; limit<=0 defaults; single-job case
```

- Files: `runner.go`, `cmd/demo/main.go`, `runner_test.go`.
- Implement: `RunWithLimit(jobs []Job, limit int) []Result` — acquire a `chan struct{}` token before `go`, release it via `defer` inside the goroutine, join with WaitGroup, collect under a mutex; treat `limit <= 0` as 1.
- Test: instrument jobs with an atomic in-flight counter, record the observed peak, assert `peak <= limit`; keep the defaults-to-1 and single-job cases.
- Verify: `go test -count=1 -race ./...`

Set up the module:

```bash
go mod edit -go=1.25
```

### Acquire before `go`, release inside the goroutine

The semaphore is a buffered channel of `struct{}` with capacity `limit`. Sending into
it is "acquire a token"; receiving from it is "release a token". The buffer size *is*
the concurrency cap: once `limit` tokens are outstanding, the next send blocks.

The placement of the acquire is the whole lesson. Send the token **before** the `go`
statement, in the producer loop. That way, when the semaphore is full, the loop
itself blocks — backpressure lands on the producer, and you never launch a goroutine
that would immediately have to wait. If instead you acquired *inside* the goroutine,
you would spawn all N goroutines up front (defeating the memory bound) and only then
throttle them. Acquire-before-`go` caps the number of live goroutines, not just the
number doing work.

The release goes inside the goroutine, deferred, so the token returns on every exit
path. Two defers stack here: `defer wg.Done()` registered first (runs last) and
`defer func(){ <-sem }()` registered second (runs first). Order between them does not
matter for correctness — the token release and the counter decrement are
independent — but both must run unconditionally, which `defer` guarantees.

Create `runner.go`:

```go
package boundedrunner

import "sync"

// Job is a unit of work with a human-readable name.
type Job struct {
	Name string
	Run  func() error
}

// Result pairs a job's name with the error it returned (nil on success).
type Result struct {
	Name string
	Err  error
}

// RunWithLimit executes jobs concurrently but never runs more than limit at
// once. A limit of zero or less is treated as 1 (sequential). Result order is
// unspecified.
func RunWithLimit(jobs []Job, limit int) []Result {
	if limit <= 0 {
		limit = 1
	}
	sem := make(chan struct{}, limit)
	var (
		mu      sync.Mutex
		wg      sync.WaitGroup
		results = make([]Result, 0, len(jobs))
	)
	for _, j := range jobs {
		wg.Add(1)
		sem <- struct{}{} // acquire BEFORE go: backpressure on the producer
		go func() {
			defer wg.Done()
			defer func() { <-sem }() // release the token
			err := j.Run()
			mu.Lock()
			results = append(results, Result{Name: j.Name, Err: err})
			mu.Unlock()
		}()
	}
	wg.Wait()
	return results
}
```

### The runnable demo

The demo runs six slow jobs at limit 2 and reports the peak concurrency it observed
using an atomic counter — a live demonstration that the cap holds.

Create `cmd/demo/main.go`:

```go
package main

import (
	"fmt"
	"sync/atomic"
	"time"

	"example.com/boundedrunner"
)

func main() {
	var inFlight, peak atomic.Int64

	track := func() error {
		n := inFlight.Add(1)
		for {
			p := peak.Load()
			if n <= p || peak.CompareAndSwap(p, n) {
				break
			}
		}
		time.Sleep(20 * time.Millisecond)
		inFlight.Add(-1)
		return nil
	}

	jobs := make([]boundedrunner.Job, 0, 6)
	for i := range 6 {
		jobs = append(jobs, boundedrunner.Job{Name: fmt.Sprintf("j%d", i), Run: track})
	}

	results := boundedrunner.RunWithLimit(jobs, 2)
	fmt.Printf("jobs=%d peak=%d limit=%d\n", len(results), peak.Load(), 2)
}
```

Run it:

```bash
go run ./cmd/demo
```

Expected output:

```
jobs=6 peak=2 limit=2
```

### Tests

`TestRunWithLimitRespectsLimit` is the core proof. Each job increments a shared
`atomic.Int64` on entry, records the running maximum with a compare-and-swap loop,
sleeps briefly so several jobs overlap, then decrements. Over 20 slow jobs at limit 2,
the observed peak must be `<= 2`. `TestRunWithLimitDefaultsToOne` pins the
`limit <= 0` guard, and `TestRunWithLimitSingleJob` covers the trivial case.

Create `runner_test.go`:

```go
package boundedrunner

import (
	"fmt"
	"sync/atomic"
	"testing"
	"time"
)

// peakTracker returns a job body that records the maximum observed concurrency.
func peakTracker(inFlight, peak *atomic.Int64) func() error {
	return func() error {
		n := inFlight.Add(1)
		for {
			p := peak.Load()
			if n <= p || peak.CompareAndSwap(p, n) {
				break
			}
		}
		time.Sleep(2 * time.Millisecond)
		inFlight.Add(-1)
		return nil
	}
}

func TestRunWithLimitRespectsLimit(t *testing.T) {
	t.Parallel()

	const (
		total = 20
		limit = 2
	)
	var inFlight, peak atomic.Int64
	body := peakTracker(&inFlight, &peak)

	jobs := make([]Job, 0, total)
	for i := range total {
		jobs = append(jobs, Job{Name: fmt.Sprintf("j%d", i), Run: body})
	}

	results := RunWithLimit(jobs, limit)
	if len(results) != total {
		t.Fatalf("len = %d, want %d", len(results), total)
	}
	if got := peak.Load(); got > limit {
		t.Fatalf("peak concurrency = %d, want <= %d", got, limit)
	}
}

func TestRunWithLimitDefaultsToOne(t *testing.T) {
	t.Parallel()

	var inFlight, peak atomic.Int64
	body := peakTracker(&inFlight, &peak)

	jobs := make([]Job, 0, 5)
	for i := range 5 {
		jobs = append(jobs, Job{Name: fmt.Sprintf("j%d", i), Run: body})
	}

	results := RunWithLimit(jobs, 0) // <= 0 means sequential
	if len(results) != 5 {
		t.Fatalf("len = %d, want 5", len(results))
	}
	if got := peak.Load(); got != 1 {
		t.Fatalf("peak concurrency = %d, want 1 (sequential)", got)
	}
}

func TestRunWithLimitSingleJob(t *testing.T) {
	t.Parallel()

	jobs := []Job{{Name: "only", Run: func() error { return nil }}}
	results := RunWithLimit(jobs, 4)
	if len(results) != 1 {
		t.Fatalf("len = %d, want 1", len(results))
	}
	if results[0].Name != "only" || results[0].Err != nil {
		t.Fatalf("result = %+v, want {only <nil>}", results[0])
	}
}

func ExampleRunWithLimit() {
	jobs := []Job{{Name: "x", Run: func() error { return nil }}}
	results := RunWithLimit(jobs, 3)
	fmt.Println(len(results), results[0].Name)
	// Output: 1 x
}
```

## Review

The bounded runner is correct when the observed peak concurrency never exceeds
`limit`, a non-positive limit collapses to sequential (`peak == 1`), and every job
still produces exactly one result. The atomic-counter instrumentation is how you turn
"it feels bounded" into an assertion: a compare-and-swap loop keeps a running maximum
that the test reads after the join.

The one mistake that quietly breaks the bound is acquiring the semaphore token
*inside* the goroutine instead of before `go`. That still limits how many jobs run
simultaneously, but it spawns every goroutine up front, so the memory and
goroutine-count bound — the entire point under load — is lost. Keep the `sem <-
struct{}{}` in the producer loop. Run `go test -race` to confirm the mutex and the
atomic are both used correctly.

## Resources

- [`sync.WaitGroup`](https://pkg.go.dev/sync#WaitGroup) — the join primitive.
- [`sync/atomic`](https://pkg.go.dev/sync/atomic) — `Int64.Add`, `Load`, `CompareAndSwap` for the peak tracker.
- [Go Blog: Pipelines and cancellation](https://go.dev/blog/pipelines) — bounded fan-out with a semaphore channel.

---

Back to [00-concepts.md](00-concepts.md) | Previous: [01-parallel-job-runner.md](01-parallel-job-runner.md) | Next: [03-healthcheck-aggregator-wg-go.md](03-healthcheck-aggregator-wg-go.md)
