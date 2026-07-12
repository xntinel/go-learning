# Exercise 15: Connection Pool: Isolating a Panicking Worker Without Crashing the Pool

**Nivel: Intermedio** — validacion rapida (un test corto).

A connection pool that fans work out across borrowed connections cannot let
one bad operation — a driver bug, an unexpected server response — take the
whole pool down with it. Real pools (database/sql's internal pool, a Redis
client's connection manager) run each borrowed connection's work in its own
goroutine and must isolate a panic to that one goroutine, retire the
connection it was using, and let every other in-flight operation finish
normally. This module builds `Pool.Run`, which does exactly that: it fans a
batch of tasks out across a fixed set of connections and guarantees the pool
itself survives a panicking task. It is fully self-contained: its own
module, demo, and tests.

## What you'll build

```text
connpool/                   independent module: example.com/connpool
  go.mod                    go 1.24
  connpool.go                Conn, Task, TaskResult, Pool, NewPool, Run, runOne
  cmd/
    demo/
      main.go               runnable demo: 5 tasks, one panics mid-migration
  connpool_test.go            panic isolates + retires a connection; empty run
```

Files: `connpool.go`, `cmd/demo/main.go`, `connpool_test.go`.
Implement: `Pool.Run(tasks []Task) []TaskResult` that spawns one goroutine per task, borrows a connection from the pool's available queue, and isolates each task's panic in a per-goroutine `runOne` so a broken connection is marked and never returned to the queue.
Test: a batch of 4 tasks where one panics with a sentinel error; assert all 4 results come back, the panicking task is marked, its error wraps the sentinel, the other three succeed cleanly, and the pool's available count drops by exactly one after the run; an empty task list leaves the pool untouched.
Verify: `go test -count=1 -race ./...`

Set up the module:

```bash
mkdir -p go-solutions/03-control-flow/08-panic-and-recover/15-connection-pool-worker-isolation/cmd/demo
cd go-solutions/03-control-flow/08-panic-and-recover/15-connection-pool-worker-isolation
go mod edit -go=1.24
```

### Why the recover lives one goroutine per task, not one for the whole pool

A single `defer`/`recover` wrapped around the loop that launches goroutines
would not help at all: `recover` only catches a panic unwinding through its
own goroutine's stack, and the loop's goroutine is not the one running the
task — `go f()` starts a new stack that the launcher's recover can never see.
If a spawned task panics with no recover of its own, that panic unwinds to
the top of its goroutine, finds nothing, and takes the entire process down —
every other in-flight task included. The only correct placement is inside
the function each spawned goroutine runs, which is why `runOne` — not `Run`
— owns the `defer`/`recover`.

The other detail that makes this a *pool* exercise rather than a plain batch
exercise is what happens to the borrowed `*Conn` when its task panics.
`runOne`'s deferred function marks `conn.Broken = true` and simply does not
send it back on `p.available`. A connection that panicked mid-operation may
be holding an aborted transaction, a corrupted read buffer, or a half-sent
protocol frame — handing it to the next task would leak that broken state
into unrelated work. The connection is deliberately retired: the pool
permanently has one fewer usable slot after a panic, which is the correct
trade-off between availability and correctness.

Create `connpool.go`:

```go
package connpool

import (
	"fmt"
	"runtime/debug"
	"sync"
)

// Conn represents one pooled connection handle.
type Conn struct {
	ID     string
	Broken bool
}

// Task is one unit of work executed against a borrowed connection.
type Task struct {
	Name string
	Run  func(*Conn) error
}

// TaskResult is the per-task outcome, including which connection ran it.
type TaskResult struct {
	Name     string
	ConnID   string
	Err      error
	Panicked bool
}

// Pool supervises a fixed set of connections and hands them out to
// concurrently running tasks.
type Pool struct {
	available chan *Conn
}

// NewPool creates a pool of n freshly labeled, available connections.
func NewPool(n int) *Pool {
	p := &Pool{available: make(chan *Conn, n)}
	for i := 0; i < n; i++ {
		p.available <- &Conn{ID: fmt.Sprintf("conn-%d", i)}
	}
	return p
}

// Available reports how many healthy connections currently sit in the pool's
// available queue. Call it only once all Run calls have returned.
func (p *Pool) Available() int {
	return len(p.available)
}

// Run executes every task concurrently, one goroutine per task, each
// borrowing a connection from the pool. A panic inside one task's Run is
// isolated to that task's own goroutine: recover fires in runOne, marks the
// borrowed connection Broken, and does NOT return it to the available
// queue — a connection that panicked mid-operation is not safe to hand to
// the next task. Every other goroutine keeps running to completion, so the
// pool itself never crashes and Run always returns len(tasks) results, in
// task order.
func (p *Pool) Run(tasks []Task) []TaskResult {
	results := make([]TaskResult, len(tasks))
	var wg sync.WaitGroup
	for i := range tasks {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			conn := <-p.available
			results[i] = p.runOne(conn, tasks[i])
		}(i)
	}
	wg.Wait()
	return results
}

// runOne is the recover boundary: exactly one goroutine's worth of
// untrusted task logic, wrapped around exactly one borrowed connection.
func (p *Pool) runOne(conn *Conn, task Task) (result TaskResult) {
	result = TaskResult{Name: task.Name, ConnID: conn.ID}
	defer func() {
		if r := recover(); r != nil {
			_ = debug.Stack() // captured at recovery time; production code would log it
			conn.Broken = true
			result.Panicked = true
			if err, ok := r.(error); ok {
				result.Err = fmt.Errorf("task %q panicked on %s: %w", task.Name, conn.ID, err)
			} else {
				result.Err = fmt.Errorf("task %q panicked on %s: %v", task.Name, conn.ID, r)
			}
			return
		}
		if !conn.Broken {
			p.available <- conn
		}
	}()

	if err := task.Run(conn); err != nil {
		result.Err = err
		return result
	}
	return result
}
```

### The runnable demo

Five tasks run against a pool of three connections; `run-migration` panics.
The connection it held is retired, and the other four tasks (three clean,
none in this demo returning a plain error) complete normally.

Create `cmd/demo/main.go`:

```go
package main

import (
	"errors"
	"fmt"

	"example.com/connpool"
)

func main() {
	pool := connpool.NewPool(3)

	tasks := []connpool.Task{
		{Name: "insert-order", Run: func(c *connpool.Conn) error { return nil }},
		{Name: "update-balance", Run: func(c *connpool.Conn) error { return nil }},
		{Name: "run-migration", Run: func(c *connpool.Conn) error {
			panic(errors.New("unexpected schema version"))
		}},
		{Name: "select-report", Run: func(c *connpool.Conn) error { return nil }},
		{Name: "delete-stale", Run: func(c *connpool.Conn) error { return nil }},
	}

	results := pool.Run(tasks)

	for _, r := range results {
		status := "ok"
		if r.Panicked {
			status = "panicked"
		} else if r.Err != nil {
			status = "failed"
		}
		fmt.Printf("%s: %s\n", r.Name, status)
	}
	fmt.Printf("available connections after run: %d\n", pool.Available())
}
```

Run it:

```bash
go run ./cmd/demo
```

Expected output (result order matches task order; connection IDs are
intentionally omitted from this printout since which physical connection
serves which task is not deterministic):

```
insert-order: ok
update-balance: ok
run-migration: panicked
select-report: ok
delete-stale: ok
available connections after run: 2
```

### Tests

`TestPoolIsolatesPanickingWorker` runs four tasks — one of which panics with
a sentinel error — and asserts every result comes back, the panicking task
is marked and wraps the sentinel, the other three finished cleanly, and
exactly one connection was retired. `TestPoolEmptyTaskList` confirms an
empty batch is a safe no-op.

Create `connpool_test.go`:

```go
package connpool

import (
	"errors"
	"testing"
)

func TestPoolIsolatesPanickingWorker(t *testing.T) {
	pool := NewPool(3)
	sentinel := errors.New("bad migration state")

	tasks := []Task{
		{Name: "t0", Run: func(c *Conn) error { return nil }},
		{Name: "t1", Run: func(c *Conn) error { panic(sentinel) }},
		{Name: "t2", Run: func(c *Conn) error { return nil }},
		{Name: "t3", Run: func(c *Conn) error { return errors.New("business rule failed") }},
	}

	results := pool.Run(tasks)

	if len(results) != len(tasks) {
		t.Fatalf("len(results) = %d, want %d", len(results), len(tasks))
	}

	byName := make(map[string]TaskResult, len(results))
	for _, r := range results {
		byName[r.Name] = r
	}

	if !byName["t1"].Panicked {
		t.Fatal("t1 should be marked Panicked")
	}
	if !errors.Is(byName["t1"].Err, sentinel) {
		t.Fatalf("t1 error %v does not wrap the sentinel", byName["t1"].Err)
	}
	if byName["t0"].Panicked || byName["t0"].Err != nil {
		t.Fatalf("t0 should have succeeded cleanly, got %+v", byName["t0"])
	}
	if byName["t2"].Panicked || byName["t2"].Err != nil {
		t.Fatalf("t2 should have succeeded cleanly, got %+v", byName["t2"])
	}
	if byName["t3"].Panicked || byName["t3"].Err == nil {
		t.Fatalf("t3 should be a plain failure, not a panic, got %+v", byName["t3"])
	}

	// The pool started with 3 connections; exactly one was permanently lost
	// to the panic, so 2 must be available once every goroutine has
	// returned its connection (or, for the panicking one, deliberately not).
	if got := pool.Available(); got != 2 {
		t.Fatalf("pool.Available() = %d, want 2 (one connection retired after the panic)", got)
	}
}

func TestPoolEmptyTaskList(t *testing.T) {
	pool := NewPool(2)
	results := pool.Run(nil)
	if len(results) != 0 {
		t.Fatalf("len(results) = %d, want 0", len(results))
	}
	if got := pool.Available(); got != 2 {
		t.Fatalf("pool.Available() = %d, want 2 (untouched)", got)
	}
}
```

## Review

`Pool.Run` is correct when every goroutine it spawns has its own recover —
never the launching loop's — and when a panicking connection is retired
rather than silently reused. The structural rule matches every other
goroutine-spawning pattern in this chapter: recover has goroutine scope, so
a boundary in the launcher protects nothing that happens in a child
goroutine's own stack. The second, pool-specific rule is what to do with the
*resource* a panicking task was holding: returning a connection that just
panicked to the available queue would let its corrupted state leak into the
next unrelated task, so `runOne` marks it `Broken` and simply never sends it
back. Losing one connection permanently per panic is a deliberate,
observable cost — far better than serving a subtly corrupted connection to
the next caller.

## Resources

- [Go Blog: Defer, Panic, and Recover](https://go.dev/blog/defer-panic-and-recover) — the per-goroutine recover boundary this pool relies on.
- [sync.WaitGroup](https://pkg.go.dev/sync#WaitGroup) — coordinating a batch of goroutines that each may or may not panic.
- [database/sql](https://pkg.go.dev/database/sql) — a standard-library connection pool with the same "retire a bad connection" discipline.

---

Back to [00-concepts.md](00-concepts.md) | Prev: [14-job-runner-report.md](14-job-runner-report.md) | Next: [16-rate-limiter-panic-containment.md](16-rate-limiter-panic-containment.md)
