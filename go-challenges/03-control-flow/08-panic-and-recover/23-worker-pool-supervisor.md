# Exercise 23: Concurrent Worker Pool Supervisor with Panic Fault Tolerance and Drain-on-Threshold

**Nivel: Avanzado** — validacion normal (tabla de casos, incluye borde o concurrencia).

A worker pool that fans tasks out across N concurrent goroutines needs two
layers of defense, not one: every worker must recover its own panics so a
single bad task cannot take down a sibling worker's goroutine, and the
supervisor watching the whole batch needs to notice when panics are piling
up and stop feeding a pool that looks systemically broken, rather than
grinding through the rest of a doomed batch one panic at a time. This module
builds `RunPool`, which bounds concurrency with a semaphore, isolates every
task's panic per-goroutine, and drains (stops dispatching) once a
configurable panic threshold is reached, reporting aggregate metrics for the
whole run. It is fully self-contained: its own module, demo, and tests.

## What you'll build

```text
workerpool/                  independent module: example.com/workerpool
  go.mod                     go 1.24
  workerpool.go               Task, Result, Metrics, RunPool, runTask
  cmd/
    demo/
      main.go                runnable demo: single worker, drains after 2 panics
  workerpool_test.go           deterministic drain (n=1), under-threshold, concurrency, empty
```

Files: `workerpool.go`, `cmd/demo/main.go`, `workerpool_test.go`.
Implement: `RunPool(n int, tasks []Task, panicThreshold int) (Metrics, []Result)` that bounds concurrency to `n` via a semaphore channel, isolates each task's panic in `runTask`, and stops dispatching further tasks once the number of panics reaches `panicThreshold`.
Test: with a single worker (`n=1`), a batch where two of five tasks panic and `panicThreshold=2`, asserting the fifth task is never dispatched once the threshold is hit and `Metrics.Drained` is `true`; a batch under threshold that never drains; a 20-task, 4-worker concurrent run with no panics, asserting the full count; an empty task list.
Verify: `go test -count=1 -race ./...`

Set up the module:

```bash
mkdir -p go-solutions/03-control-flow/08-panic-and-recover/23-worker-pool-supervisor/cmd/demo
cd go-solutions/03-control-flow/08-panic-and-recover/23-worker-pool-supervisor
go mod edit -go=1.24
```

### Why the drain check happens after acquiring a semaphore slot, not before

`RunPool` bounds concurrency with `sem := make(chan struct{}, n)`: dispatching
task `k+1` first sends to `sem` (blocking until some earlier task's goroutine
releases a slot by finishing) and *only then* checks whether the panic
threshold has been reached. This ordering matters more than it looks: if the
drain check ran *before* acquiring the semaphore, it could read a stale
`drained` flag that has not yet been updated by a task whose goroutine was
dispatched but has not finished yet — the check and the acquisition would be
racing against each other, and a task could slip out after the threshold was
already hit. By checking only after the acquire succeeds, the dispatch loop
is guaranteed that at least one previously-dispatched task's goroutine has
fully completed — including updating the panic counter and the drain flag,
both of which happen before that goroutine releases its semaphore slot in
its deferred cleanup. With a single worker (`n=1`) this makes the drain
point fully deterministic: task `k+1` is dispatched only once task `k` has
completely finished and reported its outcome, which is exactly what the
`n=1` test in this module relies on to assert precisely which task is the
last one dispatched.

Each dispatched task still gets its own goroutine with its own
`defer`/`recover` in `runTask` — the semaphore only bounds *how many* run
concurrently, it does not change the isolation guarantee that one worker's
panic can never unwind into a sibling's stack. `Metrics.Drained` is the
supervisor-level signal built on top of that per-task isolation: `Metrics`
also separately tallies `OK`, `Failed`, and `Panicked` so a caller can tell
"we drained because of panics" apart from "we drained but most tasks still
succeeded," which changes whether an operator treats the drain as a full
outage or a partial degradation.

Create `workerpool.go`:

```go
package workerpool

import (
	"fmt"
	"sync"
	"sync/atomic"
)

// Task is one unit of concurrent work.
type Task struct {
	ID  string
	Run func() error
}

// Result is the per-task outcome.
type Result struct {
	ID       string
	Err      error
	Panicked bool
}

// Metrics summarizes one RunPool batch.
type Metrics struct {
	Total    int
	OK       int
	Failed   int
	Panicked int
	Drained  bool
}

// RunPool runs tasks across up to n concurrently executing workers (a
// semaphore-bounded goroutine per dispatched task). Each task's execution is
// wrapped in its own recover, so one worker's panic never takes down a
// sibling worker's goroutine, let alone the process. The supervisor tracks
// how many tasks have panicked so far; once that count reaches
// panicThreshold, it stops dispatching any further tasks (drains the
// remaining queue) instead of continuing to feed a pool that looks
// systemically broken. Dispatch of task k+1 first blocks acquiring a
// semaphore slot freed by some earlier task's completed goroutine, and only
// then checks the drain flag. Because that goroutine updates the panic
// counter and the drain flag before releasing its slot, the check made
// before dispatching task k+1 always reflects every result already
// produced — there is no window where a stale read lets an extra task slip
// out after the threshold is hit.
func RunPool(n int, tasks []Task, panicThreshold int) (Metrics, []Result) {
	if n < 1 {
		n = 1
	}

	sem := make(chan struct{}, n)
	resultCh := make(chan Result, len(tasks))
	var wg sync.WaitGroup
	var panicCount int64
	var drained int32

	dispatched := 0
	for _, task := range tasks {
		sem <- struct{}{} // blocks until a prior task's goroutine has fully finished
		if atomic.LoadInt32(&drained) == 1 {
			<-sem // don't hold a slot we are not going to use
			break
		}
		dispatched++
		wg.Add(1)
		go func(task Task) {
			defer wg.Done()
			defer func() { <-sem }()

			r := runTask(task)
			if r.Panicked {
				if atomic.AddInt64(&panicCount, 1) >= int64(panicThreshold) {
					atomic.StoreInt32(&drained, 1)
				}
			}
			resultCh <- r
		}(task)
	}

	wg.Wait()
	close(resultCh)

	var results []Result
	var m Metrics
	for r := range resultCh {
		results = append(results, r)
		m.Total++
		switch {
		case r.Panicked:
			m.Panicked++
		case r.Err != nil:
			m.Failed++
		default:
			m.OK++
		}
	}
	m.Drained = atomic.LoadInt32(&drained) == 1
	return m, results
}

// runTask is the recover boundary: exactly one task's worth of untrusted
// logic, running in its own goroutine.
func runTask(t Task) (result Result) {
	result = Result{ID: t.ID}
	defer func() {
		if r := recover(); r != nil {
			result.Panicked = true
			if e, ok := r.(error); ok {
				result.Err = fmt.Errorf("task %q panicked: %w", t.ID, e)
				return
			}
			result.Err = fmt.Errorf("task %q panicked: %v", t.ID, r)
		}
	}()
	if err := t.Run(); err != nil {
		result.Err = err
	}
	return result
}
```

### The runnable demo

A single worker runs five tasks; `t2` and `t4` panic, and with
`panicThreshold=2` the pool drains before `t5` is ever dispatched.

Create `cmd/demo/main.go`:

```go
package main

import (
	"errors"
	"fmt"

	"example.com/workerpool"
)

func main() {
	tasks := []workerpool.Task{
		{ID: "t1", Run: func() error { return nil }},
		{ID: "t2", Run: func() error { panic(errors.New("disk read failed")) }},
		{ID: "t3", Run: func() error { return nil }},
		{ID: "t4", Run: func() error { panic(errors.New("disk read failed again")) }},
		{ID: "t5", Run: func() error { return nil }},
	}

	m, results := workerpool.RunPool(1, tasks, 2)

	for _, r := range results {
		status := "ok"
		if r.Panicked {
			status = "panicked"
		} else if r.Err != nil {
			status = "failed"
		}
		fmt.Printf("%s: %s\n", r.ID, status)
	}
	fmt.Printf("metrics: total=%d ok=%d failed=%d panicked=%d drained=%v\n",
		m.Total, m.OK, m.Failed, m.Panicked, m.Drained)
}
```

Run it:

```bash
go run ./cmd/demo
```

Expected output:

```
t1: ok
t2: panicked
t3: ok
t4: panicked
metrics: total=4 ok=2 failed=0 panicked=2 drained=true
```

### Tests

`TestRunPoolDrainsAfterThreshold` uses a single worker so dispatch is
strictly serialized, making it deterministic that `t5` is never dispatched
once the second panic hits the threshold. `TestRunPoolUnderThresholdNeverDrains`
confirms a lone panic under a higher threshold processes the whole batch.
`TestRunPoolConcurrentNoPanics` exercises real concurrency (4 workers, 20
tasks) to confirm the aggregate count is correct under `-race`.
`TestRunPoolEmptyTasks` covers the boundary case.

Create `workerpool_test.go`:

```go
package workerpool

import (
	"errors"
	"testing"
)

func TestRunPoolDrainsAfterThreshold(t *testing.T) {
	// With a single worker, dispatch is strictly serialized, so which
	// tasks get skipped after the drain threshold fires is deterministic.
	tasks := []Task{
		{ID: "t1", Run: func() error { return nil }},
		{ID: "t2", Run: func() error { panic(errors.New("boom")) }},
		{ID: "t3", Run: func() error { return nil }},
		{ID: "t4", Run: func() error { panic(errors.New("boom again")) }},
		{ID: "t5", Run: func() error { return nil }},
	}

	m, results := RunPool(1, tasks, 2)

	if !m.Drained {
		t.Fatal("m.Drained = false, want true after 2 panics with threshold 2")
	}
	if m.Total != 4 {
		t.Fatalf("m.Total = %d, want 4 (t5 must be skipped once drained)", m.Total)
	}
	if m.Panicked != 2 {
		t.Fatalf("m.Panicked = %d, want 2", m.Panicked)
	}
	if m.OK != 2 {
		t.Fatalf("m.OK = %d, want 2", m.OK)
	}
	if len(results) != 4 {
		t.Fatalf("len(results) = %d, want 4", len(results))
	}
	for _, r := range results {
		if r.ID == "t5" {
			t.Fatal("t5 should never have been dispatched")
		}
	}
}

func TestRunPoolUnderThresholdNeverDrains(t *testing.T) {
	tasks := []Task{
		{ID: "a", Run: func() error { return nil }},
		{ID: "b", Run: func() error { panic("only one panic") }},
		{ID: "c", Run: func() error { return nil }},
	}

	m, results := RunPool(1, tasks, 5)

	if m.Drained {
		t.Fatal("m.Drained = true, want false (only 1 panic, threshold 5)")
	}
	if m.Total != 3 || len(results) != 3 {
		t.Fatalf("m=%+v results=%v, want all 3 tasks processed", m, results)
	}
}

func TestRunPoolConcurrentNoPanics(t *testing.T) {
	tasks := make([]Task, 20)
	for i := range tasks {
		tasks[i] = Task{ID: "task", Run: func() error { return nil }}
	}

	m, results := RunPool(4, tasks, 1)

	if m.Drained {
		t.Fatal("m.Drained = true, want false: no task panicked")
	}
	if m.Total != 20 || m.OK != 20 || len(results) != 20 {
		t.Fatalf("m=%+v, want Total=OK=20", m)
	}
}

func TestRunPoolEmptyTasks(t *testing.T) {
	m, results := RunPool(3, nil, 1)
	if m.Total != 0 || len(results) != 0 {
		t.Fatalf("m=%+v results=%v, want empty", m, results)
	}
}
```

## Review

`RunPool` is correct when every dispatched task's panic is isolated to its
own goroutine, when the drain decision never lets an extra task slip out
after the threshold is reached, and when `Metrics` accurately breaks down
`OK`/`Failed`/`Panicked` rather than collapsing everything into a single
pass/fail count. The semaphore-acquire-then-check ordering is what makes the
drain deterministic for `n=1` and race-free for `n>1` — checking the flag
*before* acquiring would read state from a task that has not necessarily
finished yet, which is exactly the off-by-one bug this exercise is built to
catch. As with every other goroutine-spawning pattern in this chapter, the
recover in `runTask` protects the process from a single worker's bug; the
threshold-and-drain logic is the separate, supervisor-level policy decision
about when enough panics mean the pool should stop feeding itself more work.

## Resources

- [Go Blog: Defer, Panic, and Recover](https://go.dev/blog/defer-panic-and-recover) — the per-goroutine recover boundary every worker relies on.
- [sync/atomic](https://pkg.go.dev/sync/atomic) — the panic counter and drain flag shared safely across worker goroutines.
- [Bounded concurrency with buffered channels](https://go.dev/blog/pipelines) — the semaphore-channel pattern this pool uses to cap concurrency.

---

Back to [00-concepts.md](00-concepts.md) | Prev: [22-cache-invalidation-multi-backend.md](22-cache-invalidation-multi-backend.md) | Next: [24-streaming-json-recovery.md](24-streaming-json-recovery.md)
