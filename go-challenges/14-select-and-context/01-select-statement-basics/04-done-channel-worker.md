# Exercise 4: A Stoppable Background Worker via a Done Channel

Every backend has a goroutine pulling jobs off a queue that must stop cleanly on
shutdown. Before `context.Context` existed, and still inside its implementation
today, the tool for that is a `select` over two channels: the work channel and a
close-signalled `done` channel. This module builds that worker and proves it stops
promptly, finishes its in-flight job, and leaks no goroutine.

This module is fully self-contained: its own `go mod init`, all code inline, its
own demo and tests. It imports no other exercise.

## What you'll build

```text
queueworker/                    module example.com/queueworker
  go.mod                        go 1.26
  queueworker.go                type Worker; Run(work <-chan Job, done <-chan struct{}) int
  cmd/
    demo/
      main.go                   feed jobs, close the queue, print processed count
  queueworker_test.go           stop-on-done, process-then-stop, goroutine-leak check
```

Files: `queueworker.go`, `cmd/demo/main.go`, `queueworker_test.go`.
Implement: `Worker.Run(work <-chan Job, done <-chan struct{}) int` — select the two channels, return the processed count on stop.
Test: closing `done` returns promptly; N jobs then stop returns N; the `Run` goroutine exits (no leak).
Verify: `go test -count=1 -race ./...`

Set up the module:

```bash
mkdir -p ~/go-exercises/queueworker/cmd/demo
cd ~/go-exercises/queueworker
go mod init example.com/queueworker
```

## Two ways to stop, one way to spin

`Run` is a loop around a two-case `select`. One case receives from `work`; the
other receives from `done`. The worker parks on this `select` consuming zero CPU
until one of two things happens:

- `done` is closed. A receive on a closed channel is always ready, so
  `case <-done:` fires and `Run` returns the count. Closing a channel is a
  *broadcast*: close it once and every goroutine selecting on it wakes. That is why
  `chan struct{}` is the idiomatic stop signal — `struct{}` carries no data (its
  size is zero), so the channel exists purely to be closed. You never send a value;
  you close it, and every watcher unblocks.
- `work` is closed and drained. Here the comma-ok form earns its place:
  `case job, ok := <-work:` reports `ok == false` when the channel is closed and
  empty, and `Run` returns. This is the exact spot where the closed-channel
  busy-spin bug lives: if you ignored `ok` and just took `job`, the closed `work`
  case would be ready on *every* iteration, the loop would spin at 100% CPU
  forever, and `job` would be the zero value each time. Checking `ok` and returning
  is what stops the spin.

One honest caveat about the two-case `select`: if `done` is closed *and* a job is
simultaneously ready on `work`, the runtime's pseudo-random choice may take the
job before it notices `done`. There is no ordering guarantee between ready cases.
For most workers that is fine — one extra job before stopping is harmless. When you
need a hard stop that never processes another job once signalled, you check `done`
first in a nested `select` with a `default`; that priority pattern is lesson 08.
Here the worker finishes whatever it has already received and then stops, which is
the "do not drop the in-flight job" contract most queue consumers actually want.

Create `queueworker.go`:

```go
package queueworker

// Job is a unit of queued work.
type Job struct {
	ID int
}

// Worker consumes jobs from a queue until told to stop.
type Worker struct {
	// Process handles one job. It may be nil (jobs are then merely counted).
	Process func(Job)
}

// Run pulls jobs off work and processes them until either done is closed or work
// is closed and drained, then returns the number of jobs processed. It parks on
// the select while idle, consuming no CPU, and never spins on the closed work
// channel because it checks the comma-ok boolean.
func (w *Worker) Run(work <-chan Job, done <-chan struct{}) int {
	processed := 0
	for {
		select {
		case <-done:
			return processed
		case job, ok := <-work:
			if !ok {
				return processed // work drained and closed
			}
			if w.Process != nil {
				w.Process(job)
			}
			processed++
		}
	}
}
```

## The runnable demo

The demo feeds three jobs into a buffered queue, closes the queue, and lets the
worker drain it — the "work is closed and drained" exit path. It runs on the main
goroutine, so no synchronization is needed to read the count.

Create `cmd/demo/main.go`:

```go
package main

import (
	"fmt"

	"example.com/queueworker"
)

func main() {
	work := make(chan queueworker.Job, 3)
	for i := range 3 {
		work <- queueworker.Job{ID: i}
	}
	close(work) // no more jobs; worker will drain then stop

	done := make(chan struct{}) // never closed here; the queue close is what stops it

	var handled []int
	w := queueworker.Worker{Process: func(j queueworker.Job) {
		handled = append(handled, j.ID)
	}}

	n := w.Run(work, done)
	fmt.Printf("processed %d jobs: %v\n", n, handled)
}
```

Run with `go run ./cmd/demo`.

Expected output:

```
processed 3 jobs: [0 1 2]
```

## Tests

`TestWorkerStopsOnDone` starts `Run` on an empty queue in a goroutine, closes
`done`, and asserts `Run` returns within a short budget with zero jobs processed —
proving the `done` case unblocks a parked worker. `TestWorkerProcessesThenStops`
sends N jobs over an unbuffered queue (each send rendezvouses with a receive, so
after the loop the worker has consumed all N), then closes `done` and asserts the
returned count is exactly N — the worker processed everything it was handed and
stopped, dropping nothing. `TestWorkerRunGoroutineExits` records
`runtime.NumGoroutine`, runs and stops a worker, and asserts the count settles back
to baseline, proving `Run`'s goroutine actually exits rather than leaking.

Create `queueworker_test.go`:

```go
package queueworker

import (
	"runtime"
	"sync/atomic"
	"testing"
	"time"
)

func TestWorkerStopsOnDone(t *testing.T) {
	t.Parallel()

	work := make(chan Job)      // never fed
	done := make(chan struct{}) // the stop signal
	result := make(chan int, 1)

	w := Worker{}
	go func() { result <- w.Run(work, done) }()

	close(done)

	select {
	case n := <-result:
		if n != 0 {
			t.Fatalf("processed %d jobs, want 0", n)
		}
	case <-time.After(time.Second):
		t.Fatal("Run did not return promptly after done was closed")
	}
}

func TestWorkerProcessesThenStops(t *testing.T) {
	t.Parallel()

	const n = 8
	work := make(chan Job) // unbuffered: each send waits for the worker to receive
	done := make(chan struct{})
	result := make(chan int, 1)

	var seen atomic.Int64
	w := Worker{Process: func(Job) { seen.Add(1) }}
	go func() { result <- w.Run(work, done) }()

	for i := range n {
		work <- Job{ID: i} // returns only once the worker has taken this job
	}
	close(done) // work queue is now empty; nothing more can be taken

	select {
	case got := <-result:
		if got != n {
			t.Fatalf("Run returned %d, want %d", got, n)
		}
	case <-time.After(time.Second):
		t.Fatal("Run did not return after done was closed")
	}
	if seen.Load() != n {
		t.Fatalf("Process called %d times, want %d", seen.Load(), n)
	}
}

func TestWorkerRunGoroutineExits(t *testing.T) {
	t.Parallel()

	base := runtime.NumGoroutine()

	work := make(chan Job)
	done := make(chan struct{})
	stopped := make(chan struct{})

	w := Worker{}
	go func() {
		w.Run(work, done)
		close(stopped)
	}()

	close(done)
	<-stopped // Run has returned

	// The goroutine count should settle back to baseline once Run's goroutine exits.
	if !settlesTo(base, 2*time.Second) {
		t.Fatalf("goroutines did not return to baseline %d (now %d): Run leaked", base, runtime.NumGoroutine())
	}
}

// settlesTo polls until NumGoroutine is at or below target, or the deadline passes.
func settlesTo(target int, within time.Duration) bool {
	deadline := time.Now().Add(within)
	for time.Now().Before(deadline) {
		if runtime.NumGoroutine() <= target {
			return true
		}
		time.Sleep(5 * time.Millisecond)
	}
	return runtime.NumGoroutine() <= target
}
```

## Review

The worker is correct when it parks on the `select` at zero CPU while idle, returns
promptly the instant `done` is closed, drains and stops when `work` is closed, and
never spins on the closed `work` channel — the comma-ok `ok` check is the one line
that prevents the 100%-CPU bug. The leak test is the other half of the contract: a
stoppable worker that does not actually stop its goroutine is a memory leak wearing
a graceful-shutdown costume. Remember that when both `done` and `work` are ready the
choice is random; if your operator needs a guaranteed hard stop, that is the
priority-select pattern in lesson 08, not a promise this two-case `select` makes.

## Resources

- [Go Specification: Select statements](https://go.dev/ref/spec#Select_statements) — two-case receive and the no-priority-among-ready-cases rule.
- [Go Blog: Pipelines and cancellation](https://go.dev/blog/pipelines) — the done-channel idiom this module isolates, the ancestor of `context.Done`.
- [runtime.NumGoroutine](https://pkg.go.dev/runtime#NumGoroutine) — the goroutine census used to assert no leak.

---

Back to [00-concepts.md](00-concepts.md) | Previous: [03-fan-in-demo-cli.md](03-fan-in-demo-cli.md) | Next: [05-bounded-result-collector.md](05-bounded-result-collector.md)
