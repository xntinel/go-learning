# Exercise 10: Panic-Isolating Worker That Keeps the WaitGroup Balanced

An individual job handler can panic — a nil dereference, a bad type assertion, an
out-of-range index on malformed input. In a fan-out worker that panic must not crash
the process, and it must not leak the WaitGroup counter and hang `Wait` forever.
This module builds a worker where each goroutine uses layered defers to convert a
panic into a `Result.Err` while still calling `Done` on every path — and it hinges on
`defer`'s LIFO ordering.

This module is fully self-contained: its own module, demo, and tests.

## What you'll build

```text
panicworker/               independent module: example.com/panicworker
  go.mod                   go 1.25
  worker.go                Run recovers per-job panics, keeps the counter balanced
  cmd/
    demo/
      main.go              runnable demo: ok, error, and panicking jobs
  worker_test.go           panic surfaced as error; stack captured; Wait returns
```

- Files: `worker.go`, `cmd/demo/main.go`, `worker_test.go`.
- Implement: `Run(jobs []Job) []Result` — `defer wg.Done()` first, then a deferred `recover()` that records the panic as `Result.Err` with `debug.Stack()`; collect under a mutex.
- Test: a mix of nil-return, error-return, and panicking handlers; assert one `Result` per job, the panic surfaced as a non-nil error containing the recovered value, `debug.Stack()` populated for the panicking job, and `Wait` returned (no hang).
- Verify: `go test -count=1 -race ./...`

Set up the module:

```bash
mkdir -p go-solutions/13-goroutines-and-channels/05-waitgroup/10-panic-safe-worker-wg/cmd/demo
cd go-solutions/13-goroutines-and-channels/05-waitgroup/10-panic-safe-worker-wg
go mod edit -go=1.25
```

### defer LIFO is the whole trick

A panicking goroutine that no code recovers takes down the entire process — a single
bad job kills the server. And if the panic unwinds past `wg.Done`, the counter never
reaches zero and `Wait` hangs. Both failures are avoided with two deferred functions,
and the order in which you register them matters because `defer` runs LIFO (last
registered runs first).

Register `defer wg.Done()` *first*, so it runs *last*. Then register a deferred
closure that calls `recover()`, converts a non-nil recovered value into the result's
error (capturing `debug.Stack()` for diagnostics), and records the result. Because
this closure was registered second, it runs first during unwinding: it stops the
panic and populates the result. Then `wg.Done` runs, decrementing the counter — so the
join stays balanced even though a panic occurred. The goroutine exits normally, the
process survives, and `Wait` returns.

Why capture `debug.Stack()` inside the recovering closure? At the moment a deferred
function runs because of a panic, the panicking frames have not been fully unwound, so
`debug.Stack()` still includes the origin of the panic. Recording it turns an opaque
"job failed" into an actionable stack trace in your logs. (Note `wg.Go` from Exercise 3
would *not* help here — it does not recover panics, so risky handlers still need this
explicit `recover`.)

Create `worker.go`:

```go
package panicworker

import (
	"fmt"
	"runtime/debug"
	"sync"
)

// Job is a unit of work whose handler may return an error or panic.
type Job struct {
	Name string
	Run  func() error
}

// Result carries a job's outcome. Err is non-nil on either a returned error or a
// recovered panic; Stack holds the panic stack trace when the job panicked.
type Result struct {
	Name  string
	Err   error
	Stack string
}

// Run executes every job concurrently. A job that panics is isolated: its panic
// becomes Result.Err, the process is not crashed, and the WaitGroup stays balanced
// so Run always returns.
func Run(jobs []Job) []Result {
	var (
		mu      sync.Mutex
		wg      sync.WaitGroup
		results = make([]Result, 0, len(jobs))
	)
	for _, j := range jobs {
		wg.Add(1)
		go func() {
			defer wg.Done() // registered FIRST -> runs LAST, always decrements

			res := Result{Name: j.Name}
			defer func() { // registered SECOND -> runs FIRST, recovers the panic
				if r := recover(); r != nil {
					res.Err = fmt.Errorf("job %q panicked: %v", j.Name, r)
					res.Stack = string(debug.Stack())
				}
				mu.Lock()
				results = append(results, res)
				mu.Unlock()
			}()

			res.Err = j.Run()
		}()
	}
	wg.Wait()
	return results
}
```

### The runnable demo

Create `cmd/demo/main.go`:

```go
package main

import (
	"errors"
	"fmt"
	"sort"

	"example.com/panicworker"
)

func main() {
	jobs := []panicworker.Job{
		{Name: "ok", Run: func() error { return nil }},
		{Name: "err", Run: func() error { return errors.New("validation failed") }},
		{Name: "panic", Run: func() error {
			var m map[string]int
			m["x"] = 1 // nil map write: panics
			return nil
		}},
	}

	results := panicworker.Run(jobs)
	sort.Slice(results, func(i, j int) bool { return results[i].Name < results[j].Name })
	for _, r := range results {
		switch {
		case r.Stack != "":
			fmt.Printf("%s: PANIC (%v)\n", r.Name, r.Err)
		case r.Err != nil:
			fmt.Printf("%s: ERROR (%v)\n", r.Name, r.Err)
		default:
			fmt.Printf("%s: OK\n", r.Name)
		}
	}
}
```

Run it:

```bash
go run ./cmd/demo
```

Expected output:

```
err: ERROR (validation failed)
ok: OK
panic: PANIC (job "panic" panicked: assignment to entry in nil map)
```

### Tests

`TestRunIsolatesPanic` runs three handlers — one returning nil, one returning a
sentinel error, one panicking — and asserts each yields one `Result`: the panicking
job's error is non-nil and mentions the recovered value, its `Stack` is populated, and
the sentinel error is preserved via `errors.Is`. Crucially, the test reaching its
assertions at all proves `Wait` returned — a leaked counter would hang here.
`TestRunManyPanicsStayBalanced` runs 100 panicking jobs and asserts all 100 results
come back, proving the counter stays balanced at scale under `-race`.

Create `worker_test.go`:

```go
package panicworker

import (
	"errors"
	"fmt"
	"strings"
	"testing"
)

var errValidation = errors.New("validation failed")

func TestRunIsolatesPanic(t *testing.T) {
	t.Parallel()

	jobs := []Job{
		{Name: "ok", Run: func() error { return nil }},
		{Name: "err", Run: func() error { return errValidation }},
		{Name: "panic", Run: func() error { panic("boom") }},
	}

	results := Run(jobs)
	if len(results) != 3 {
		t.Fatalf("len = %d, want 3", len(results))
	}

	byName := map[string]Result{}
	for _, r := range results {
		byName[r.Name] = r
	}

	if byName["ok"].Err != nil {
		t.Fatalf("ok.Err = %v, want nil", byName["ok"].Err)
	}
	if !errors.Is(byName["err"].Err, errValidation) {
		t.Fatalf("err.Err = %v, want errValidation", byName["err"].Err)
	}

	p := byName["panic"]
	if p.Err == nil {
		t.Fatal("panic job Err = nil, want non-nil")
	}
	if !strings.Contains(p.Err.Error(), "boom") {
		t.Fatalf("panic job Err = %q, want it to contain the recovered value %q", p.Err.Error(), "boom")
	}
	if p.Stack == "" {
		t.Fatal("panic job Stack is empty, want a captured stack trace")
	}
}

func TestRunManyPanicsStayBalanced(t *testing.T) {
	t.Parallel()

	const n = 100
	jobs := make([]Job, 0, n)
	for i := range n {
		jobs = append(jobs, Job{
			Name: fmt.Sprintf("job-%d", i),
			Run:  func() error { panic(fmt.Sprintf("boom-%d", i)) },
		})
	}

	results := Run(jobs) // returns only if the counter stayed balanced
	if len(results) != n {
		t.Fatalf("len = %d, want %d", len(results), n)
	}
	for _, r := range results {
		if r.Err == nil {
			t.Fatalf("%s: Err = nil, want a recovered panic", r.Name)
		}
	}
}

func ExampleRun() {
	jobs := []Job{{Name: "safe", Run: func() error { return nil }}}
	results := Run(jobs)
	fmt.Println(results[0].Name, results[0].Err)
	// Output: safe <nil>
}
```

## Review

The worker is correct when every job — including a panicking one — produces exactly
one `Result`, a panic surfaces as a non-nil error that mentions the recovered value
and carries a captured stack, a returned error is preserved via `errors.Is`, and
`Run` always returns rather than hanging. The 100-panic test is the balance proof: if
a single panic ever skipped `Done`, `Wait` would block and the test would time out.

The lesson is the defer ordering. `defer wg.Done()` must be registered first so it
runs last, after the `recover` closure has stopped the panic and recorded the result —
that ordering is what keeps the counter balanced through a panic. Get it backwards and
either the panic escapes before the result is recorded, or the counter leaks. Remember
too that `wg.Go` does not do any of this for you; risky handlers always need their own
`recover`. Run `go test -race` to confirm the collection and the balance together.

## Resources

- [`recover`](https://go.dev/ref/spec#Handling_panics) — the language spec on panic/recover and deferred functions.
- [`runtime/debug.Stack`](https://pkg.go.dev/runtime/debug#Stack) — capturing the panic stack for diagnostics.
- [Effective Go: Defer, Panic, Recover](https://go.dev/blog/defer-panic-and-recover) — defer LIFO ordering and recover semantics.

---

Back to [00-concepts.md](00-concepts.md) | Previous: [09-reusable-waitgroup-paged-backfill.md](09-reusable-waitgroup-paged-backfill.md) | Next: [11-hierarchical-region-shard-read-nested-waitgroups.md](11-hierarchical-region-shard-read-nested-waitgroups.md)
