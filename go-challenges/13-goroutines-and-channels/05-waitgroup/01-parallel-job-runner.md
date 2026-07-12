# Exercise 1: Fan-Out Job Runner With WaitGroup and Mutex-Guarded Results

The most common shape of concurrent backend work is fan-out-and-join: launch every
job in its own goroutine, then wait for all of them before returning the collected
results. This module builds that runner with the three core WaitGroup methods and a
mutex to make the shared result collection race-free.

This module is fully self-contained: its own module, its own demo, its own tests.

## What you'll build

```text
jobrunner/                 independent module: example.com/jobrunner
  go.mod                   go 1.25
  runner.go                Job, Result; Run(jobs) collects every result
  cmd/
    demo/
      main.go              runnable demo: three jobs, one failing
  runner_test.go           table-driven success + mixed; 100-job race test
```

- Files: `runner.go`, `cmd/demo/main.go`, `runner_test.go`.
- Implement: `Run(jobs []Job) []Result` — one goroutine per job, `Add(1)` before each `go`, `defer wg.Done()`, append each `Result` under a `sync.Mutex`, `Wait` before returning.
- Test: a success case, a mixed success/error case asserting exactly one error, and a 100-job concurrent case asserting all 100 results are collected.
- Verify: `go test -count=1 -race ./...`

Set up the module:

```bash
go mod edit -go=1.25
```

### Why a mutex is required here

Each goroutine produces a `Result{Name, Err}` and needs to put it somewhere the
caller can read after the join. The tempting move — `results = append(results, r)`
from every goroutine — is a data race. `append` reads the slice's length and backing
pointer, decides whether to grow, and writes a new length; two goroutines doing that
concurrently corrupt the header or lose elements, and the `-race` detector flags it
immediately. A `sync.Mutex` serializes the appends so exactly one goroutine mutates
the slice at a time.

Because appends happen in whatever order goroutines finish, the result *order* is
nondeterministic. That is fine for a runner whose contract is "give me every
result"; the tests assert set membership and count, never position. (When order
matters, Exercise 4's indexed-slice pattern is the right tool.)

The WaitGroup is the join. `Add(1)` runs before each `go` so the counter always
reflects launched work; `defer wg.Done()` is the goroutine's first statement so the
counter decrements on every exit path including a panic; `wg.Wait()` blocks until all
jobs finish, and — via the memory model — publishes every append so the returned
slice is safe to read without further locking.

Create `runner.go`:

```go
package jobrunner

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

// Run executes every job concurrently, one goroutine per job, and returns a
// Result for each once all have finished. Result order is unspecified.
func Run(jobs []Job) []Result {
	var (
		mu      sync.Mutex
		wg      sync.WaitGroup
		results = make([]Result, 0, len(jobs))
	)
	for _, j := range jobs {
		wg.Add(1)
		go func() {
			defer wg.Done()
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

Note there is no `j := j` before the goroutine: since Go 1.22 the loop variable is
per-iteration, so capturing `j` directly is correct. On older toolchains this would
have been a classic aliasing bug.

### The runnable demo

Create `cmd/demo/main.go`:

```go
package main

import (
	"errors"
	"fmt"
	"sort"

	"example.com/jobrunner"
)

func main() {
	jobs := []jobrunner.Job{
		{Name: "migrate", Run: func() error { return nil }},
		{Name: "seed", Run: func() error { return errors.New("connection refused") }},
		{Name: "warmup", Run: func() error { return nil }},
	}
	results := jobrunner.Run(jobs)

	sort.Slice(results, func(i, j int) bool { return results[i].Name < results[j].Name })
	for _, r := range results {
		if r.Err != nil {
			fmt.Printf("%s: FAIL (%v)\n", r.Name, r.Err)
			continue
		}
		fmt.Printf("%s: OK\n", r.Name)
	}
}
```

The demo sorts by name because `Run`'s output order is nondeterministic; sorting
makes the printed output stable.

Run it:

```bash
go run ./cmd/demo
```

Expected output:

```
migrate: OK
seed: FAIL (connection refused)
warmup: OK
```

### Tests

`TestRunSuccess` is the baseline: every job succeeds, so we get one nil-error result
per job. `TestRunCollectsAllResults` mixes a failure in and asserts exactly one error
is collected while the count still matches. `TestRunIsSafeUnderConcurrentJobs` is the
race proof: 100 jobs that each return an error, asserting all 100 results come back —
run under `-race`, it fails loudly if the mutex is missing or misused. All assertions
check membership and count, never order.

Create `runner_test.go`:

```go
package jobrunner

import (
	"errors"
	"fmt"
	"testing"
)

var errBoom = errors.New("boom")

func TestRunSuccess(t *testing.T) {
	t.Parallel()

	jobs := []Job{
		{Name: "a", Run: func() error { return nil }},
		{Name: "b", Run: func() error { return nil }},
	}
	results := Run(jobs)
	if len(results) != 2 {
		t.Fatalf("len = %d, want 2", len(results))
	}
	for _, r := range results {
		if r.Err != nil {
			t.Fatalf("%s: err = %v, want nil", r.Name, r.Err)
		}
	}
}

func TestRunCollectsAllResults(t *testing.T) {
	t.Parallel()

	jobs := []Job{
		{Name: "a", Run: func() error { return nil }},
		{Name: "b", Run: func() error { return errBoom }},
		{Name: "c", Run: func() error { return nil }},
	}
	results := Run(jobs)
	if len(results) != 3 {
		t.Fatalf("len = %d, want 3", len(results))
	}

	got := map[string]error{}
	for _, r := range results {
		got[r.Name] = r.Err
	}
	if !errors.Is(got["b"], errBoom) {
		t.Fatalf("job b err = %v, want errBoom", got["b"])
	}
	errCount := 0
	for _, r := range results {
		if r.Err != nil {
			errCount++
		}
	}
	if errCount != 1 {
		t.Fatalf("errCount = %d, want 1", errCount)
	}
}

func TestRunIsSafeUnderConcurrentJobs(t *testing.T) {
	t.Parallel()

	const n = 100
	jobs := make([]Job, 0, n)
	for i := range n {
		jobs = append(jobs, Job{
			Name: fmt.Sprintf("job-%d", i),
			Run:  func() error { return errBoom },
		})
	}
	results := Run(jobs)
	if len(results) != n {
		t.Fatalf("len = %d, want %d", len(results), n)
	}

	seen := make(map[string]bool, n)
	for _, r := range results {
		if !errors.Is(r.Err, errBoom) {
			t.Fatalf("%s: err = %v, want errBoom", r.Name, r.Err)
		}
		seen[r.Name] = true
	}
	for i := range n {
		want := fmt.Sprintf("job-%d", i)
		if !seen[want] {
			t.Fatalf("missing result for %q", want)
		}
	}
}

func ExampleRun() {
	jobs := []Job{
		{Name: "only", Run: func() error { return nil }},
	}
	results := Run(jobs)
	fmt.Println(len(results), results[0].Name, results[0].Err)
	// Output: 1 only <nil>
}
```

## Review

The runner is correct when three properties hold: every launched job produces
exactly one `Result` (count matches), the collection is race-free under `-race`
(the mutex actually guards the append), and errors are preserved so
`errors.Is(r.Err, errBoom)` finds each failure. The order of results is deliberately
unspecified — that is the price of concurrent appends, and the tests respect it by
asserting membership.

The traps are the ones the concepts warned about: `Add(1)` must sit before `go` or
`Wait` can return early; `defer wg.Done()` must be the first line so an error branch
or panic still decrements; and the append must be locked or the race detector will
catch the corruption. Run `go test -race` to prove all three at once.

## Resources

- [`sync.WaitGroup`](https://pkg.go.dev/sync#WaitGroup) — `Add`, `Done`, `Wait`, and the happens-before guarantee.
- [`sync.Mutex`](https://pkg.go.dev/sync#Mutex) — serializing the shared append.
- [Go memory model: WaitGroup](https://go.dev/ref/mem) — why `Wait` publishes the goroutines' writes.

---

Back to [00-concepts.md](00-concepts.md) | Next: [02-bounded-fanout-runner.md](02-bounded-fanout-runner.md)
