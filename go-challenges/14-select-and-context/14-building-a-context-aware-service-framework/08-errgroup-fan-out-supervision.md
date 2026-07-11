# Exercise 8: Replacing Hand-Rolled Goroutine Tracking with errgroup

The run loop so far launches each long-running worker and tracks them by hand. In
production that bookkeeping — a `WaitGroup`, a shared first-error guarded by a
mutex, a `done` channel, a "cancel everyone on the first failure" flag — is
error-prone. `golang.org/x/sync/errgroup` is the structured replacement: a
derived context that cancels on the first error, and a `Wait` that aggregates it.
This module refactors the supervisor onto errgroup and uses `SetLimit` to bound
concurrent startup.

## What you'll build

```text
groupsup/                     independent module: example.com/groupsup
  go.mod                      go 1.26; requires golang.org/x/sync
  manager.go                  Worker; Manager.Run over errgroup.WithContext, SetLimit
  manager_test.go             first fatal cancels peers; Wait returns it; SetLimit bounds concurrency
  cmd/
    demo/
      main.go                 three workers; one fails; peers observe cancellation
```

Files: `manager.go`, `cmd/demo/main.go`, `manager_test.go`.
Implement: `Manager.Run` using `g, gctx := errgroup.WithContext(root)`; each `Worker.Run(gctx)` in `g.Go`; the first fatal return cancels `gctx` so peers observe `ctx.Done()`; `g.Wait()` returns the first error; `SetLimit` bounds concurrent startup.
Test: three workers, one returns a fatal error, assert the derived context is cancelled so peers exit and `Wait` returns that first error; a `SetLimit(2)` test asserts no more than two `Run`s execute concurrently (an atomic peak gauge).
Verify: `go test -count=1 -race ./...`

Set up the module:

```bash
mkdir -p ~/go-exercises/groupsup/cmd/demo
cd ~/go-exercises/groupsup
go mod init example.com/groupsup
go get golang.org/x/sync/errgroup
```

### What errgroup buys you

`errgroup.WithContext(parent)` returns a `*Group` and a derived context. The
derived context is cancelled the first time any function passed to `g.Go` returns
a non-nil error (or when `Wait` returns, whichever comes first). That single
sentence replaces a pile of manual coordination: every worker runs its
`Run(gctx)` against that context, so when one worker returns a fatal error, the
context cancels, and every peer that is selecting on `gctx.Done()` observes it and
returns. `g.Wait()` blocks until all workers have returned and yields the *first*
non-nil error — exactly the value you want for the process exit code.

`g.SetLimit(n)` caps how many `g.Go` functions run concurrently: once `n` are
active, the next `g.Go` call *blocks* until a slot frees. That throttles startup
for components expensive to initialize (each opening a large connection pool, say)
so a burst of registrations does not stampede the machine. It must be called
before any `g.Go`.

The framework wraps each worker's error with its name so the aggregated error
names the culprit. Note the peers do not need to know *why* the context was
cancelled to exit — they only need to honor `gctx.Done()`. That is the discipline
every long-running worker must follow, and it is what makes structured
cancellation compose.

Create `manager.go`:

```go
package groupsup

import (
	"context"
	"fmt"
	"log/slog"

	"golang.org/x/sync/errgroup"
)

// Worker is a long-running unit. Run must honor ctx.Done() so that when a peer
// fails and the shared context cancels, this worker returns promptly.
type Worker interface {
	Name() string
	Run(ctx context.Context) error
}

// Manager runs a set of workers under a shared errgroup: the first fatal error
// cancels the derived context (so peers observe it and exit) and is returned by
// Run. A positive limit bounds how many workers start concurrently.
type Manager struct {
	workers []Worker
	limit   int
	logger  *slog.Logger
}

// NewManager builds a Manager. A limit <= 0 means unbounded concurrency.
func NewManager(logger *slog.Logger, limit int, workers ...Worker) *Manager {
	if logger == nil {
		logger = slog.Default()
	}
	return &Manager{workers: workers, limit: limit, logger: logger}
}

// Run starts every worker under an errgroup derived from ctx and blocks until
// all workers return, yielding the first non-nil error.
func (m *Manager) Run(ctx context.Context) error {
	g, gctx := errgroup.WithContext(ctx)
	if m.limit > 0 {
		g.SetLimit(m.limit)
	}
	for _, w := range m.workers {
		g.Go(func() error {
			m.logger.Info("worker starting", "name", w.Name())
			if err := w.Run(gctx); err != nil {
				return fmt.Errorf("worker %s: %w", w.Name(), err)
			}
			return nil
		})
	}
	return g.Wait()
}
```

### The runnable demo

The demo runs three workers. Two block until the shared context is cancelled; the
third fails after a brief moment. The failure cancels the derived context, the two
blockers observe it and exit, and `Run` returns the first error. The output is
deterministic because only the failing worker prints a distinctive line and the
blockers print an exit line each.

Create `cmd/demo/main.go`:

```go
package main

import (
	"context"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"sort"
	"time"

	"example.com/groupsup"
)

type blocker struct {
	name string
	out  chan string
}

func (b *blocker) Name() string { return b.name }

func (b *blocker) Run(ctx context.Context) error {
	<-ctx.Done()
	b.out <- b.name + " exited"
	return nil
}

type failer struct{ name string }

func (f *failer) Name() string { return f.name }

func (f *failer) Run(context.Context) error {
	time.Sleep(30 * time.Millisecond)
	return errors.New("boom")
}

func main() {
	out := make(chan string, 8)
	mgr := groupsup.NewManager(
		slog.New(slog.NewTextHandler(io.Discard, nil)), 0,
		&blocker{name: "worker-a", out: out},
		&blocker{name: "worker-b", out: out},
		&failer{name: "worker-c"},
	)

	err := mgr.Run(context.Background())

	close(out)
	var exits []string
	for s := range out {
		exits = append(exits, s)
	}
	sort.Strings(exits)
	for _, s := range exits {
		fmt.Println(s)
	}
	fmt.Println("run error:", err)
}
```

Run it:

```bash
go run ./cmd/demo
```

Expected output:

```
worker-a exited
worker-b exited
run error: worker worker-c: boom
```

### Tests

`TestFirstErrorCancelsPeers` runs a failing worker alongside two blockers that
return only when the shared context is cancelled; because `Wait` returns, the
blockers must have observed cancellation and exited, and the returned error wraps
the failing worker's sentinel. `TestSetLimitBoundsConcurrency` runs four workers
under `SetLimit(2)`, each recording the peak concurrent count via an atomic
gauge, and asserts the peak never exceeded two while all four ran.

Create `manager_test.go`:

```go
package groupsup

import (
	"context"
	"errors"
	"io"
	"log/slog"
	"sync/atomic"
	"testing"
	"time"
)

func testLogger() *slog.Logger {
	return slog.New(slog.NewTextHandler(io.Discard, nil))
}

var errBoom = errors.New("boom")

type blockWorker struct {
	name string
	ran  *atomic.Int64
}

func (b *blockWorker) Name() string { return b.name }

func (b *blockWorker) Run(ctx context.Context) error {
	<-ctx.Done()
	b.ran.Add(1)
	return nil
}

type failWorker struct{ name string }

func (f *failWorker) Name() string { return f.name }

func (f *failWorker) Run(context.Context) error {
	time.Sleep(10 * time.Millisecond)
	return errBoom
}

func TestFirstErrorCancelsPeers(t *testing.T) {
	t.Parallel()

	var peersExited atomic.Int64
	mgr := NewManager(testLogger(), 0,
		&blockWorker{name: "a", ran: &peersExited},
		&blockWorker{name: "b", ran: &peersExited},
		&failWorker{name: "c"},
	)

	err := mgr.Run(context.Background())
	if !errors.Is(err, errBoom) {
		t.Fatalf("Run: err = %v, want wrapping errBoom", err)
	}
	if peersExited.Load() != 2 {
		t.Fatalf("peers exited = %d, want 2 (both observed cancellation)", peersExited.Load())
	}
}

type gaugeWorker struct {
	cur     *atomic.Int64
	peak    *atomic.Int64
	release <-chan struct{}
}

func (g *gaugeWorker) Name() string { return "gauge" }

func (g *gaugeWorker) Run(context.Context) error {
	n := g.cur.Add(1)
	for {
		p := g.peak.Load()
		if n <= p || g.peak.CompareAndSwap(p, n) {
			break
		}
	}
	<-g.release
	g.cur.Add(-1)
	return nil
}

func TestSetLimitBoundsConcurrency(t *testing.T) {
	t.Parallel()

	var cur, peak atomic.Int64
	release := make(chan struct{})

	workers := make([]Worker, 4)
	for i := range workers {
		workers[i] = &gaugeWorker{cur: &cur, peak: &peak, release: release}
	}
	mgr := NewManager(testLogger(), 2, workers...)

	done := make(chan error, 1)
	go func() { done <- mgr.Run(context.Background()) }()

	// Give the group time to saturate its limit, then release everyone.
	time.Sleep(50 * time.Millisecond)
	if got := peak.Load(); got > 2 {
		t.Fatalf("peak concurrency = %d, want <= 2 under SetLimit(2)", got)
	}
	close(release)

	select {
	case err := <-done:
		if err != nil {
			t.Fatalf("Run: err = %v, want nil", err)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("Run did not return")
	}

	if peak.Load() > 2 {
		t.Fatalf("final peak concurrency = %d, want <= 2", peak.Load())
	}
}

func TestPeersObserveContextViaWait(t *testing.T) {
	t.Parallel()

	// Sanity: with no failing worker and a cancellable root, cancelling the
	// root cancels the derived context and every worker exits.
	var exited atomic.Int64
	mgr := NewManager(testLogger(), 0,
		&blockWorker{name: "a", ran: &exited},
		&blockWorker{name: "b", ran: &exited},
	)

	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() { done <- mgr.Run(ctx) }()

	time.Sleep(20 * time.Millisecond)
	cancel()

	select {
	case err := <-done:
		if err != nil {
			t.Fatalf("Run: err = %v, want nil", err)
		}
	case <-time.After(time.Second):
		t.Fatal("Run did not return after root cancel")
	}
	if exited.Load() != 2 {
		t.Fatalf("workers exited = %d, want 2", exited.Load())
	}
}
```

## Review

The refactor is correct when the first fatal error cancels the derived context —
the `peersExited == 2` assertion proves both blockers observed `gctx.Done()` and
returned before `Wait` unblocked — and when `Wait` yields that first error wrapped
with the worker's name. The `SetLimit(2)` test pins the concurrency bound with an
atomic peak gauge; the invariant is `peak <= 2`, not `peak == 2`, because the
scheduler decides how many of the admitted workers are actually running at any
instant. The discipline errgroup enforces is that every worker must honor
`gctx.Done()`; a worker that ignores it turns the first failure into a hang,
because `Wait` cannot return until that worker does. Run `go test -race`: the
atomic gauges and the release channel are what keep the concurrency accounting
race-free.

## Resources

- [golang.org/x/sync/errgroup](https://pkg.go.dev/golang.org/x/sync/errgroup) — `WithContext`, `Go`, `Wait`, `SetLimit`, and `TryGo`.
- [Go Blog: Pipelines and cancellation](https://go.dev/blog/pipelines) — the structured-cancellation pattern errgroup packages.
- [sync/atomic](https://pkg.go.dev/sync/atomic) — the peak-concurrency gauge used in the limit test.

---

Back to [00-concepts.md](00-concepts.md) | Previous: [07-bounded-startup-with-deadline-cause.md](07-bounded-startup-with-deadline-cause.md) | Next: [09-request-scoped-context-and-detached-background-work.md](09-request-scoped-context-and-detached-background-work.md)
