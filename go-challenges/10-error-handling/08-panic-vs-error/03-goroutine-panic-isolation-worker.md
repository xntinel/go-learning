# Exercise 3: Worker Pool That Isolates a Panicking Job

The most dangerous fact about panic in a concurrent server: a panic in *any*
goroutine, unrecovered, terminates the whole process. A worker pool that runs
untrusted job functions must therefore treat every worker goroutine as its own
recovery boundary, or one bad job takes down every other in-flight request. This
module builds that pool and proves a panicking job cannot cross the goroutine line.

This module is fully self-contained: its own `go mod init`, all code inline, its
own demo and tests. Nothing here imports another exercise.

## What you'll build

```text
workerpool/                  independent module: example.com/workerpool
  go.mod                     go 1.26
  workerpool.go              Job; JobError; RunAll(ctx, workers, jobs) []error
  cmd/
    demo/
      main.go                runnable demo: mix of good and panicking jobs
  workerpool_test.go         mix of success/panic/runtime-fault; drains; process survives; -race
```

Files: `workerpool.go`, `cmd/demo/main.go`, `workerpool_test.go`.
Implement: `RunAll(ctx context.Context, workers int, jobs []Job) []error` running jobs across a bounded worker pool where each worker converts a job panic into a `*JobError` (carrying the recovered value and its stack) instead of crashing.
Test: a mix of succeeding jobs, one panicking with a value, one triggering a runtime fault (index out of range); assert all successes return nil, both panics surface as non-nil `*JobError` with a non-empty stack, the pool drains cleanly, and the test process never crashes.
Verify: `go test -count=1 -race ./...`

Set up the module:

```bash
mkdir -p go-solutions/10-error-handling/08-panic-vs-error/03-goroutine-panic-isolation-worker/cmd/demo
cd go-solutions/10-error-handling/08-panic-vs-error/03-goroutine-panic-isolation-worker
go mod edit -go=1.26
```

### The recovery must live in the worker, not the caller

If you wrote `go job()` and `job` panicked, no amount of `recover` in the function
that spawned it would help: `recover` only catches a panic unwinding the *same*
goroutine's stack. The spawning goroutine has already moved on; the panicking
goroutine unwinds independently, reaches the top of its own stack with no deferred
`recover`, and the runtime kills the process. This is why "just put a recover in
`main`" or "the HTTP middleware handles it" is wrong for spawned work — the recover
has to be *inside* the goroutine that runs the untrusted code.

So each worker runs every job through `runOne`, whose deferred closure recovers
the panic and turns it into a `*JobError`. Crucially the worker's `for` loop then
continues to the next job: one job panicking does not stop the worker, does not
stop its sibling workers, and does not stop the process. The pool degrades
gracefully — a bad job produces an error in the results, exactly as if the job had
returned that error itself.

`RunAll` fans work out over a fixed number of workers reading from a `tasks`
channel, and writes each job's outcome into `results[i]`. Writing to distinct
slice indices from different goroutines is race-free (they are distinct memory
locations), so no mutex is needed for the results. A `sync.WaitGroup` makes
`RunAll` block until every worker has drained the channel and exited, which is what
"drains cleanly" means: no goroutine is left running when `RunAll` returns.

Create `workerpool.go`:

```go
package workerpool

import (
	"context"
	"fmt"
	"runtime/debug"
	"sync"
)

// Job is a unit of untrusted work. It may return an error or panic; the pool
// treats both as a failed job without crashing.
type Job func(ctx context.Context) error

// JobError wraps a panic that escaped a job, preserving the recovered value and
// the stack captured at the recovery site for observability.
type JobError struct {
	Value any
	Stack []byte
}

func (e *JobError) Error() string {
	return fmt.Sprintf("job panicked: %v", e.Value)
}

// runOne executes a single job behind a recovery boundary. A panic becomes a
// *JobError; a returned error passes through unchanged.
func runOne(ctx context.Context, job Job) (err error) {
	defer func() {
		if r := recover(); r != nil {
			err = &JobError{Value: r, Stack: debug.Stack()}
		}
	}()
	return job(ctx)
}

// RunAll runs jobs across a bounded pool of workers and returns each job's outcome
// by index. A panic in any job is isolated to that job: it becomes a *JobError and
// never crashes the process or interrupts a sibling job.
func RunAll(ctx context.Context, workers int, jobs []Job) []error {
	if workers < 1 {
		workers = 1
	}
	results := make([]error, len(jobs))

	type task struct {
		i   int
		job Job
	}
	tasks := make(chan task)

	var wg sync.WaitGroup
	for range workers {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for t := range tasks {
				results[t.i] = runOne(ctx, t.job)
			}
		}()
	}

	for i, j := range jobs {
		tasks <- task{i: i, job: j}
	}
	close(tasks)
	wg.Wait()

	return results
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

	"example.com/workerpool"
)

func main() {
	jobs := []workerpool.Job{
		func(context.Context) error { return nil },
		func(context.Context) error { return errors.New("boom") },
		func(context.Context) error { panic("nil session") },
		func(context.Context) error { return nil },
	}

	results := workerpool.RunAll(context.Background(), 2, jobs)

	for i, err := range results {
		switch {
		case err == nil:
			fmt.Printf("job %d: ok\n", i)
		default:
			var pe *workerpool.JobError
			if errors.As(err, &pe) {
				fmt.Printf("job %d: recovered panic (%v)\n", i, pe.Value)
			} else {
				fmt.Printf("job %d: error (%v)\n", i, err)
			}
		}
	}
	fmt.Println("pool drained; process still alive")
}
```

Run it:

```bash
go run ./cmd/demo
```

Expected output:

```
job 0: ok
job 1: error (boom)
job 2: recovered panic (nil session)
job 3: ok
pool drained; process still alive
```

### Tests

`TestPanicIsIsolated` submits a mix: successes, a normal error, a deliberate panic,
and a genuine runtime fault (indexing past a slice). It asserts each success is
`nil`, the normal error passes through, both panics surface as `*JobError` with a
non-empty stack, and — implicitly, by the test completing at all — the process was
never crashed. `TestManyPanicsUnderRace` hammers the pool with many panicking jobs
under `-race` to prove the isolation holds concurrently and the pool always drains.

Create `workerpool_test.go`:

```go
package workerpool

import (
	"context"
	"errors"
	"fmt"
	"testing"
)

func TestPanicIsIsolated(t *testing.T) {
	t.Parallel()
	var empty []int
	jobs := []Job{
		func(context.Context) error { return nil },
		func(context.Context) error { return errors.New("normal") },
		func(context.Context) error { panic("deliberate") },
		func(context.Context) error { _ = empty[5]; return nil }, // runtime fault
		func(context.Context) error { return nil },
	}

	results := RunAll(context.Background(), 3, jobs)
	if len(results) != len(jobs) {
		t.Fatalf("got %d results, want %d", len(results), len(jobs))
	}

	if results[0] != nil {
		t.Fatalf("job 0 err = %v, want nil", results[0])
	}
	// A normally-returned error passes through as itself, not wrapped as a panic.
	var notPanic *JobError
	if results[1] == nil || errors.As(results[1], &notPanic) {
		t.Fatalf("job 1 err = %v, want the normal error (not a *JobError)", results[1])
	}
	if results[4] != nil {
		t.Fatalf("job 4 err = %v, want nil", results[4])
	}

	// Both the deliberate panic and the runtime fault become *JobError.
	for _, i := range []int{2, 3} {
		var pe *JobError
		if !errors.As(results[i], &pe) {
			t.Fatalf("job %d err = %v, want *JobError", i, results[i])
		}
		if len(pe.Stack) == 0 {
			t.Fatalf("job %d JobError.Stack is empty; stack was not captured", i)
		}
	}
}

func TestManyPanicsUnderRace(t *testing.T) {
	t.Parallel()
	const n = 200
	jobs := make([]Job, n)
	wantOK := 0
	for i := range n {
		if i%2 == 0 {
			jobs[i] = func(context.Context) error { return nil }
			wantOK++
		} else {
			jobs[i] = func(context.Context) error { panic("boom") }
		}
	}

	results := RunAll(context.Background(), 8, jobs)

	gotOK := 0
	for _, err := range results {
		if err == nil {
			gotOK++
		}
	}
	if gotOK != wantOK {
		t.Fatalf("succeeded %d jobs, want %d", gotOK, wantOK)
	}
}

func ExampleRunAll() {
	jobs := []Job{
		func(context.Context) error { return nil },
		func(context.Context) error { panic("boom") },
	}
	results := RunAll(context.Background(), 2, jobs)
	fmt.Println(results[0] == nil, results[1] != nil)
	// Output: true true
}
```

## Review

The pool is correct when a panic in one job never escapes its worker goroutine:
each success returns `nil`, each panic (deliberate or a `runtime.Error` like the
out-of-range index) becomes a `*JobError` with a captured stack, and the pool
always drains — which the tests confirm simply by running to completion, because a
missed recovery would crash the whole `go test` process. The load-bearing decision
is that the `recover` lives *inside* the worker goroutine, not in `RunAll`'s caller;
a `recover` in the spawning goroutine catches nothing. Writing results to distinct
slice indices needs no lock, and `-race` confirms it. If you removed the `recover`
from `runOne`, `TestManyPanicsUnderRace` would not fail with an assertion — it would
kill the test binary, which is precisely the operational lesson.

## Resources

- [Go Blog: Defer, Panic, and Recover](https://go.dev/blog/defer-panic-and-recover) — a panic unwinds only its own goroutine.
- [Go Memory Model](https://go.dev/ref/mem) — why disjoint slice-index writes across goroutines are race-free.
- [`runtime/debug.Stack`](https://pkg.go.dev/runtime/debug#Stack) — capturing the worker goroutine's stack at recovery.

---

Back to [00-concepts.md](00-concepts.md) | Next: [04-generic-must-helper-and-package-init.md](04-generic-must-helper-and-package-init.md)
