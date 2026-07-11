# Exercise 5: Fail CI On Goroutine Leaks With goleak

The context detector catches a pinned context *node*; Uber's `goleak` catches the
other half of the same defect — a *goroutine* parked on `ctx.Done()` that never
exits. This module wires `goleak.VerifyTestMain` into a worker-pool package so the
build fails the instant any test leaks a goroutine, and demonstrates the guard by
asserting a leak is detectable mid-test and then proving the fixed pool exits
cleanly.

This module is fully self-contained: its own `go mod init`, all code inline, its
own demo and tests. Nothing here imports another exercise.

## What you'll build

```text
workerpool/                        module example.com/workerpool
  go.mod                           requires go.uber.org/goleak
  workerpool.go                    Pool.Start(ctx)/Stop()/Running(); workers block on ctx.Done
  workerpool_test.go               TestMain -> goleak.VerifyTestMain; fixed-pool + leak-detectable tests
  cmd/
    demo/
      main.go                      start pool, show goroutine growth, stop, show it drain
```

Files: `workerpool.go`, `workerpool_test.go`, `cmd/demo/main.go`.
Implement: a `Pool` whose `Start(ctx)` launches N workers blocked in `select` on `ctx.Done()`, a `Stop()` that cancels and waits, and `Running()`.
Test: `TestMain` gates the package with `goleak.VerifyTestMain`; the fixed pool returns `NumGoroutine` to baseline after `Stop`; a leak-detectable test asserts `goleak.Find() != nil` while workers run, then cleans up.
Verify: `go test -count=1 -race ./...`

Set up the module:

```bash
mkdir -p ~/go-exercises/workerpool/cmd/demo
cd ~/go-exercises/workerpool
go mod init example.com/workerpool
go get go.uber.org/goleak
```

### Why VerifyTestMain, not VerifyNone

`goleak` works by snapshotting the set of running goroutines and failing if any
unexpected ones survive. It offers two entry points with very different scopes.
`goleak.VerifyNone(t)` checks at the end of a *single* test — but it cannot tell
your leaked goroutine apart from goroutines belonging to other tests running
concurrently, so it is unsafe in any package that uses `t.Parallel()`.
`goleak.VerifyTestMain(m)` runs once, after *all* tests in the package have
finished, from a `TestMain`; at that point the only goroutines left should be the
runtime's own, so a survivor is unambiguously a leak. For a package with any
parallel tests, `VerifyTestMain` is the correct gate. This module keeps its tests
serial so it can *also* use `goleak.Find()` inside a test to prove a leak exists at
a chosen moment.

The demonstration is the delicate part. If a test leaked a real goroutine,
`VerifyTestMain` would fail the whole package — including this lesson's gate. So
`TestLeakIsDetectable` does not actually leak: it starts a pool, calls
`goleak.Find()` and asserts it returns a non-nil error (a leak *is* present right
now, because the workers are blocked), and then calls `Stop()` to drain them
before returning. The package ends with zero survivors, `VerifyTestMain` passes,
and the test has still proven that `goleak` sees the leak. `goleak.Find` returns an
error rather than failing a test, which is exactly why it is the right tool for an
in-test assertion.

`Running()` is the pool's own live-worker counter, decremented as each worker
exits. It is the goroutine-layer analogue of the context detector's
`ActiveContexts`: after `Start(ctx)` with N workers, `Running()` is N and
`runtime.NumGoroutine()` has grown by N; after `Stop()`, both return to their
baselines. Cross-checking the two is how you localize a climb — if `NumGoroutine`
grows but `Running()` does not, the leak is somewhere other than this pool.

Create `workerpool.go`:

```go
package workerpool

import (
	"context"
	"sync"
	"sync/atomic"
)

// Pool runs a fixed number of worker goroutines bound to the lifetime of the
// context passed to Start. Each worker blocks until the context is cancelled.
type Pool struct {
	n       int
	wg      sync.WaitGroup
	cancel  context.CancelFunc
	running atomic.Int64
}

// New returns a Pool that will run n workers.
func New(n int) *Pool {
	return &Pool{n: n}
}

// Start launches n workers. They exit when the derived context is cancelled by
// Stop (or when the parent ctx is cancelled). Start does not block.
func (p *Pool) Start(ctx context.Context) {
	ctx, p.cancel = context.WithCancel(ctx)
	for range p.n {
		p.wg.Add(1)
		p.running.Add(1)
		go func() {
			defer p.wg.Done()
			defer p.running.Add(-1)
			<-ctx.Done() // block until cancelled; no busy-wait
		}()
	}
}

// Stop cancels the workers' context and waits for every worker to exit. It is
// safe to call once after Start; calling it is what prevents the leak.
func (p *Pool) Stop() {
	if p.cancel != nil {
		p.cancel()
	}
	p.wg.Wait()
}

// Running reports how many workers are still alive.
func (p *Pool) Running() int64 {
	return p.running.Load()
}
```

### The runnable demo

Create `cmd/demo/main.go`:

```go
package main

import (
	"context"
	"fmt"
	"runtime"

	"example.com/workerpool"
)

func main() {
	base := runtime.NumGoroutine()

	p := workerpool.New(4)
	p.Start(context.Background())
	fmt.Println("workers running:", p.Running())
	fmt.Println("goroutines grew by:", runtime.NumGoroutine()-base)

	p.Stop() // the line that prevents the leak
	fmt.Println("workers running after stop:", p.Running())
}
```

Run it:

```bash
go run ./cmd/demo
```

Expected output:

```
workers running: 4
goroutines grew by: 4
workers running after stop: 0
```

### Tests

Create `workerpool_test.go`:

```go
package workerpool

import (
	"context"
	"runtime"
	"testing"
	"time"

	"go.uber.org/goleak"
)

// TestMain gates the whole package: after every test finishes, any surviving
// goroutine fails the build. This is the CI guardrail.
func TestMain(m *testing.M) {
	goleak.VerifyTestMain(m)
}

// waitForGoroutines polls until NumGoroutine drops to want, or fails. Goroutine
// teardown is asynchronous, so a fixed sleep would be flaky.
func waitForGoroutines(t *testing.T, want int) {
	t.Helper()
	for range 100 {
		if runtime.NumGoroutine() <= want {
			return
		}
		time.Sleep(5 * time.Millisecond)
	}
	t.Fatalf("goroutines did not return to %d, still %d", want, runtime.NumGoroutine())
}

func TestFixedPoolNoLeak(t *testing.T) {
	base := runtime.NumGoroutine()

	p := New(4)
	p.Start(context.Background())
	if got := p.Running(); got != 4 {
		t.Fatalf("Running = %d after Start, want 4", got)
	}

	p.Stop()
	if got := p.Running(); got != 0 {
		t.Fatalf("Running = %d after Stop, want 0", got)
	}
	waitForGoroutines(t, base)
}

// TestLeakIsDetectable proves goleak sees a live pool as a leak, then cleans up
// so VerifyTestMain stays green.
func TestLeakIsDetectable(t *testing.T) {
	p := New(3)
	p.Start(context.Background())

	if err := goleak.Find(); err == nil {
		t.Fatal("goleak.Find found no leak while 3 workers are blocked; want a leak")
	}

	p.Stop() // drain, so the package ends clean
	if err := goleak.Find(); err != nil {
		t.Fatalf("goleak.Find still reports a leak after Stop: %v", err)
	}
}
```

Note both tests are serial (no `t.Parallel()`). `TestLeakIsDetectable` calls
`goleak.Find()` and must see *only* its own workers, so it cannot tolerate another
test's goroutines running at the same time — the precise reason `VerifyNone`/`Find`
and `t.Parallel` do not mix.

## Review

The guard is correct when the fixed pool returns `NumGoroutine` to baseline and
`Running()` to zero, and when `goleak.Find()` reports a leak for a live pool but
not after `Stop`. The structural payoff is `TestMain`: with `VerifyTestMain`
wired in, any future test that forgets to `Stop` its pool fails CI automatically,
with a goroutine stack pointing at the blocked worker — no manual assertion
required. `goleak.Find` returning an error (rather than failing) is what lets
`TestLeakIsDetectable` assert the leak and then recover; using `VerifyNone` there
would fail the test outright. Run under `-race`: `Running()` is read and written
from multiple goroutines, so `atomic.Int64` is mandatory, not decorative.

## Resources

- [go.uber.org/goleak](https://pkg.go.dev/go.uber.org/goleak) — `VerifyTestMain`, `VerifyNone`, `Find`, and the `Ignore*` options.
- [goleak README](https://github.com/uber-go/goleak) — the recommended `TestMain` wiring and how snapshotting works.
- [runtime.NumGoroutine](https://pkg.go.dev/runtime#NumGoroutine) — the raw count cross-checked against `Running()`.

---

Back to [04-demo-leak-cli.md](04-demo-leak-cli.md) | Next: [06-httptest-handler-leak.md](06-httptest-handler-leak.md)
