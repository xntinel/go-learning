# Exercise 26: Worker Pool Task Enqueuer for Batches

**Nivel: Intermedio** — validacion rapida (un test corto).

A batch of tasks needs to be handed out to a fixed pool of idle workers
before any of them start running. The simplest fair distribution is
round-robin: task 0 to worker 0, task 1 to worker 1, and so on, wrapping
back to worker 0 once every worker has one. Modeling the whole batch as a
variadic list of `Task` values means the caller never builds an
intermediate `[]Task` just to hand it to the distributor — `Distribute(3,
t1, t2, t3, t4, t5)` reads exactly like what it does.

## What you'll build

```text
workerpool/                independent module: example.com/workerpool
  go.mod                   go 1.24
  workerpool.go            package workerpool; type Task struct{ID, Payload string}; Distribute(workerCount int, tasks ...Task) ([][]Task, error)
  cmd/
    demo/
      main.go              runnable demo: five tasks across three workers, then the zero-worker error
  workerpool_test.go        table tests: round-robin split, fewer tasks than workers, zero workers with/without tasks
```

- Files: `workerpool.go`, `cmd/demo/main.go`, `workerpool_test.go`.
- Implement: `type Task struct{ ID, Payload string }` and `Distribute(workerCount int, tasks ...Task) ([][]Task, error)`, returning one slice per worker in worker order.
- Test: five tasks across three workers split `{t1,t4}`, `{t2,t5}`, `{t3}`; fewer tasks than workers leaves the extra workers with empty (not nil-vs-empty-inconsistent) queues; zero workers with pending tasks is an error, zero workers with zero tasks is not.
- Verify: `go test -count=1 ./...`

Set up the module:

```bash
mkdir -p ~/go-exercises/workerpool/cmd/demo
cd ~/go-exercises/workerpool
go mod init example.com/workerpool
go mod edit -go=1.24
```

### Why round-robin over a variadic batch, and why zero workers is conditional

`Distribute` takes the whole batch as `tasks ...Task` because enqueueing is
naturally a one-shot operation on a fixed set of work: the caller has
collected every task for this dispatch cycle and wants an assignment back,
not a channel to push into one at a time. The assignment itself is `i %
workerCount` — the task's position in the batch modulo the worker count —
which is the simplest rule that spreads consecutive tasks across distinct
workers and only repeats a worker once every one has had a turn. This
module intentionally stays synchronous: it returns the *plan* (which
worker gets which tasks), not a running pool of goroutines pulling off
channels, because pinning down that plan first, and testing it in
isolation, is what makes the eventual concurrent dispatcher easy to get
right — bugs in "who gets what" are far easier to find in a pure function
than layered under real goroutine scheduling.

The zero-worker case is deliberately conditional rather than always an
error. `Distribute(0)` with no tasks has nothing to assign and nowhere to
assign it, so it succeeds trivially and returns `nil` — a caller that
short-circuits "no pending work" before even looking at the worker pool
should not have to handle a spurious error. `Distribute(0, task)` with at
least one task, though, describes an impossible request: there is work
and no worker can ever receive it, so it must fail loudly instead of
silently returning an empty plan that would make the task vanish.

Create `workerpool.go`:

```go
// workerpool.go
package workerpool

import "fmt"

// Task is one unit of work an idle worker will execute.
type Task struct {
	ID      string
	Payload string
}

// Distribute assigns tasks to workerCount workers using round-robin: the
// first task goes to worker 0, the second to worker 1, and so on, wrapping
// back to worker 0 after the last worker. It returns one slice per worker,
// in worker order, each holding that worker's tasks in the order they were
// given. Distribute reports an error if workerCount is not positive and
// there is at least one task to assign — zero workers cannot receive work.
func Distribute(workerCount int, tasks ...Task) ([][]Task, error) {
	if workerCount <= 0 {
		if len(tasks) == 0 {
			return nil, nil
		}
		return nil, fmt.Errorf("workerpool: workerCount must be positive, got %d", workerCount)
	}

	queues := make([][]Task, workerCount)
	for i, task := range tasks {
		w := i % workerCount
		queues[w] = append(queues[w], task)
	}
	return queues, nil
}
```

### The runnable demo

Create `cmd/demo/main.go`:

```go
// cmd/demo/main.go
package main

import (
	"fmt"

	"example.com/workerpool"
)

func main() {
	queues, err := workerpool.Distribute(3,
		workerpool.Task{ID: "t1", Payload: "resize-image"},
		workerpool.Task{ID: "t2", Payload: "send-email"},
		workerpool.Task{ID: "t3", Payload: "reindex-doc"},
		workerpool.Task{ID: "t4", Payload: "purge-cache"},
		workerpool.Task{ID: "t5", Payload: "generate-thumb"},
	)
	if err != nil {
		fmt.Println("error:", err)
		return
	}
	for w, tasks := range queues {
		fmt.Printf("worker %d:", w)
		for _, task := range tasks {
			fmt.Printf(" %s", task.ID)
		}
		fmt.Println()
	}

	_, err = workerpool.Distribute(0, workerpool.Task{ID: "t1"})
	fmt.Println("error:", err)
}
```

Run it:

```bash
go run ./cmd/demo
```

Expected output:

```
worker 0: t1 t4
worker 1: t2 t5
worker 2: t3
error: workerpool: workerCount must be positive, got 0
```

### Tests

`TestDistributeRoundRobin` pins the exact split shown in the demo output;
`TestDistributeFewerTasksThanWorkers` checks that idle workers get an
empty, safely-rangeable queue rather than a nil that a careless caller
might treat differently.

Create `workerpool_test.go`:

```go
// workerpool_test.go
package workerpool

import (
	"testing"
)

func TestDistributeRoundRobin(t *testing.T) {
	t.Parallel()

	queues, err := Distribute(3,
		Task{ID: "t1"}, Task{ID: "t2"}, Task{ID: "t3"},
		Task{ID: "t4"}, Task{ID: "t5"},
	)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	want := [][]string{
		{"t1", "t4"},
		{"t2", "t5"},
		{"t3"},
	}
	if len(queues) != len(want) {
		t.Fatalf("got %d worker queues, want %d", len(queues), len(want))
	}
	for w, tasks := range queues {
		if len(tasks) != len(want[w]) {
			t.Fatalf("worker %d: got %d tasks, want %d", w, len(tasks), len(want[w]))
		}
		for i, task := range tasks {
			if task.ID != want[w][i] {
				t.Errorf("worker %d task %d = %q, want %q", w, i, task.ID, want[w][i])
			}
		}
	}
}

func TestDistributeZeroWorkersWithTasksErrors(t *testing.T) {
	t.Parallel()

	_, err := Distribute(0, Task{ID: "t1"})
	if err == nil {
		t.Fatal("expected an error for zero workers with pending tasks")
	}
}

func TestDistributeZeroWorkersNoTasks(t *testing.T) {
	t.Parallel()

	queues, err := Distribute(0)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if queues != nil {
		t.Fatalf("queues = %v, want nil", queues)
	}
}

func TestDistributeFewerTasksThanWorkers(t *testing.T) {
	t.Parallel()

	queues, err := Distribute(4, Task{ID: "only"})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if len(queues) != 4 {
		t.Fatalf("got %d queues, want 4", len(queues))
	}
	if len(queues[0]) != 1 || queues[0][0].ID != "only" {
		t.Errorf("queues[0] = %v, want [only]", queues[0])
	}
	for w := 1; w < 4; w++ {
		if len(queues[w]) != 0 {
			t.Errorf("queues[%d] = %v, want empty", w, queues[w])
		}
	}
}
```

## Review

`Distribute` is correct when task `i` always lands in worker `i %
workerCount`, every worker slice (even an idle one) is safe to range over,
and the impossible case — pending work with no workers to receive it — is
the one case that must fail loudly. The senior point is separating
"compute the assignment" from "run the workers": a pure function that maps
tasks to worker indices is trivial to unit test exhaustively, while a real
worker pool built on top of it (goroutines each draining their own
channel) only has to trust that the plan it was handed is already correct.
Round-robin is the right default when tasks are roughly uniform in cost;
if tasks vary wildly in size, a real system would switch to a
least-loaded-worker or work-stealing assignment instead, but the variadic
entry point stays the same shape either way.

## Resources

- [Go Spec: Passing arguments to `...` parameters](https://go.dev/ref/spec#Passing_arguments_to_..._parameters)
- [Go blog: Concurrency patterns — pipelines and worker pools](https://go.dev/blog/pipelines)

---

Back to [00-concepts.md](00-concepts.md) | Prev: [25-transaction-fee-accumulator-rules.md](25-transaction-fee-accumulator-rules.md) | Next: [27-batch-collector-size-trigger.md](27-batch-collector-size-trigger.md)
