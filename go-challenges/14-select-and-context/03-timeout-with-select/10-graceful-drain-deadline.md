# Exercise 10: Draining in-flight work under a shutdown deadline

Graceful shutdown has a hard core: stop accepting new work, then wait up to a
deadline for the in-flight tasks to finish, and if they do not, give up and report
how many are still outstanding. This exercise builds that drain with pure
`select`-plus-timer and a `sync.WaitGroup` surfaced through a channel — the
mechanism `context`-based shutdown wraps later in the chapter.

## What you'll build

```text
draindeadline/                module example.com/pool
  go.mod
  pool.go                     ErrTimeout; Pool.Go; Pool.Drain(within)
  cmd/demo/main.go            a clean drain and a timed-out drain
  pool_test.go                all-finish, times-out-with-count, waits-not-early, no-double-close
```

Files: `pool.go`, `cmd/demo/main.go`, `pool_test.go`.
Implement: `Pool` with `Go(task func())` and `Drain(within time.Duration) (int, error)`.
Test: all workers finish before the deadline returns nil and zero outstanding; hanging workers return `ErrTimeout` with the correct count; the drain does not return before either all-done or the deadline; two sequential drains do not double-close.
Verify: `go test -count=1 -race ./...`

Set up the module:

```bash
mkdir -p ~/go-exercises/draindeadline/cmd/demo
cd ~/go-exercises/draindeadline
go mod init example.com/pool
```

### A WaitGroup surfaced through a channel

`Pool` tracks in-flight work two ways: a `sync.WaitGroup` that counts running tasks,
and an atomic `inflight` counter that lets `Drain` report *how many* are still
outstanding when the deadline fires. `Go` increments both, launches the task, and
decrements both when it returns. A `WaitGroup` alone cannot be `select`-ed on —
`wg.Wait()` blocks — so `Drain` bridges it to a channel: it spawns a small goroutine
that calls `wg.Wait()` and then `close(done)`. Now `Drain` can select between
`done` (all tasks finished) and a `time.NewTimer(within)` (deadline fired).

If the deadline wins, `Drain` returns `int(inflight.Load())` and `ErrTimeout`. The
`wg.Wait` goroutine is still running — the hung tasks have not finished — but it does
not leak in the pathological sense: it is blocked on `wg.Wait`, and when those tasks
eventually complete it wakes, closes `done`, and exits. Nobody reads `done` after the
timeout, and a `close` needs no reader, so there is no send-on-closed and no
deadlock. Each `Drain` call creates its own fresh `done` channel closed by exactly
one goroutine, so repeated drains never double-close.

The correctness properties worth stating: `Drain` returns nil with zero outstanding
only when every task genuinely finished; it never returns *before* either all-done
or the deadline (no early exit); and the outstanding count on timeout reflects the
tasks still running. The timer is stopped with `defer` to release its runtime entry.

Create `pool.go`:

```go
package pool

import (
	"errors"
	"sync"
	"sync/atomic"
	"time"
)

// ErrTimeout is returned when the drain deadline fires before all tasks finish.
var ErrTimeout = errors.New("pool: drain deadline exceeded")

// Pool runs tasks as goroutines and can drain them under a deadline.
type Pool struct {
	wg       sync.WaitGroup
	inflight atomic.Int64
}

// Go launches task as a tracked goroutine.
func (p *Pool) Go(task func()) {
	p.wg.Add(1)
	p.inflight.Add(1)
	go func() {
		defer p.wg.Done()
		defer p.inflight.Add(-1)
		task()
	}()
}

// Drain waits up to within for all in-flight tasks to finish. It returns (0, nil)
// if they all complete, or (outstanding, ErrTimeout) if the deadline fires first.
func (p *Pool) Drain(within time.Duration) (int, error) {
	done := make(chan struct{})
	go func() {
		p.wg.Wait()
		close(done)
	}()

	timer := time.NewTimer(within)
	defer timer.Stop()

	select {
	case <-done:
		return 0, nil
	case <-timer.C:
		return int(p.inflight.Load()), ErrTimeout
	}
}
```

### The runnable demo

Create `cmd/demo/main.go`:

```go
package main

import (
	"errors"
	"fmt"
	"time"

	"example.com/pool"
)

func main() {
	var clean pool.Pool
	for i := range 3 {
		d := time.Duration(20*(i+1)) * time.Millisecond
		clean.Go(func() { time.Sleep(d) })
	}
	n, err := clean.Drain(500 * time.Millisecond)
	fmt.Printf("clean drain: outstanding=%d err=%v\n", n, err)

	var stuck pool.Pool
	block := make(chan struct{})
	for range 2 {
		stuck.Go(func() { <-block })
	}
	n, err = stuck.Drain(50 * time.Millisecond)
	fmt.Printf("timed drain: outstanding=%d timedOut=%v\n", n, errors.Is(err, pool.ErrTimeout))
	close(block)
}
```

Run it:

```bash
go run ./cmd/demo
```

Expected output:

```
clean drain: outstanding=0 err=<nil>
timed drain: outstanding=2 timedOut=true
```

### Tests

`TestDrainAllFinish` launches five short tasks and asserts a clean drain: nil error,
zero outstanding. `TestDrainTimesOut` launches three tasks that block on a channel,
drains with a short deadline, and asserts `ErrTimeout` with outstanding equal to
three and an elapsed time at least the deadline (proving it waited). `TestDrainWaits`
launches one task longer than nothing-but-shorter-than-the-deadline and asserts the
drain returns only after the task finished, never early. `TestNoDoubleClose` runs two
sequential drains on the same pool — the first times out, the second succeeds — to
confirm each drain's own `done` channel is closed exactly once.

Create `pool_test.go`:

```go
package pool

import (
	"errors"
	"testing"
	"time"
)

func TestDrainAllFinish(t *testing.T) {
	t.Parallel()
	var p Pool
	for range 5 {
		p.Go(func() { time.Sleep(20 * time.Millisecond) })
	}
	n, err := p.Drain(500 * time.Millisecond)
	if err != nil {
		t.Fatalf("err = %v, want nil", err)
	}
	if n != 0 {
		t.Fatalf("outstanding = %d, want 0", n)
	}
}

func TestDrainTimesOut(t *testing.T) {
	t.Parallel()
	var p Pool
	release := make(chan struct{})
	for range 3 {
		p.Go(func() { <-release })
	}
	start := time.Now()
	n, err := p.Drain(50 * time.Millisecond)
	elapsed := time.Since(start)
	if !errors.Is(err, ErrTimeout) {
		t.Fatalf("err = %v, want ErrTimeout", err)
	}
	if n != 3 {
		t.Fatalf("outstanding = %d, want 3", n)
	}
	if elapsed < 40*time.Millisecond {
		t.Fatalf("drain returned too early: %v", elapsed)
	}
	close(release)
}

func TestDrainWaits(t *testing.T) {
	t.Parallel()
	var p Pool
	p.Go(func() { time.Sleep(60 * time.Millisecond) })
	start := time.Now()
	n, err := p.Drain(500 * time.Millisecond)
	elapsed := time.Since(start)
	if err != nil {
		t.Fatalf("err = %v, want nil", err)
	}
	if n != 0 {
		t.Fatalf("outstanding = %d, want 0", n)
	}
	if elapsed < 50*time.Millisecond {
		t.Fatalf("drain returned before the worker finished: %v", elapsed)
	}
}

func TestNoDoubleClose(t *testing.T) {
	t.Parallel()
	var p Pool
	release := make(chan struct{})
	for range 4 {
		p.Go(func() { <-release })
	}
	if _, err := p.Drain(30 * time.Millisecond); !errors.Is(err, ErrTimeout) {
		t.Fatalf("first drain err = %v, want ErrTimeout", err)
	}
	close(release)
	if n, err := p.Drain(500 * time.Millisecond); err != nil || n != 0 {
		t.Fatalf("second drain n=%d err=%v, want 0,nil", n, err)
	}
}
```

## Review

The drain is correct when it returns cleanly only after all tasks finish, reports
the right outstanding count on a timeout, and never exits early. The two failure
modes it guards against are the `select`-on-a-WaitGroup temptation (you cannot
`select` on `wg.Wait()`; the channel bridge is required) and double-closing a shared
completion channel across repeated drains — each `Drain` allocates its own `done`,
closed by exactly one goroutine, which `TestNoDoubleClose` and `-race` verify. The
outstanding count comes from the atomic counter, not the WaitGroup, because a
WaitGroup exposes no count. Run `go test -race` with the real task goroutines to
confirm no send-on-closed and no data race on the counter.

## Resources

- [`sync.WaitGroup`](https://pkg.go.dev/sync#WaitGroup) — counting in-flight tasks.
- [`sync/atomic.Int64`](https://pkg.go.dev/sync/atomic#Int64) — the outstanding-count surface a WaitGroup lacks.
- [`time.NewTimer`](https://pkg.go.dev/time#NewTimer) — the shutdown deadline in the select.

---

Back to [00-concepts.md](00-concepts.md) | Previous: [09-deadline-budget-split.md](09-deadline-budget-split.md) | Next: [../04-context-withcancel/00-concepts.md](../04-context-withcancel/00-concepts.md)
