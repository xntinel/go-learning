# Exercise 3: Reverse-Order Teardown with Per-Service Stop Budgets

Teardown is where a lifecycle manager earns its keep. This module implements the
two invariants that make shutdown safe: services stop in the exact reverse of
their registration order, and each `Stop` runs against a *fresh* timeout context
so a single wedged `Stop` cannot hang the whole shutdown.

## What you'll build

```text
teardown/                     independent module: example.com/teardown
  go.mod                      go 1.26
  svcframe.go                 core App with reverse-order stopAll and per-service budgets
  svcframe_test.go            exact reverse order, no-hang on a blocking Stop
  cmd/
    demo/
      main.go                 registers db/cache/api, prints observed stop order
```

Files: `svcframe.go`, `cmd/demo/main.go`, `svcframe_test.go`.
Implement: `stopAll` iterating in reverse, each `Stop` wrapped in a fresh `context.WithTimeout(context.Background(), cfg.StopTimeout)`; a `Stop` that exceeds its budget is logged and skipped.
Test: register db/cache/api, record `Stop` order under a mutex, assert `[api, cache, db]`; a separate no-hang test where a `Stop` blocks on `<-ctx.Done()` and must return once the budget fires.
Verify: `go test -count=1 -race ./...`

### Why reverse order, and why a fresh budget

Registration order encodes dependency order: `db` before `cache` before `api`,
because `api` handlers read through `cache` which reads through `db`. If teardown
went forward, `db` would close first and a still-serving `api` handler would hit
a closed connection. Reverse order — `api`, then `cache`, then `db` — guarantees
that by the time a resource closes, everything that depends on it has already
stopped. The framework does not need a dependency graph to get this right; the
registration list *is* the graph, walked backward.

The budget subtlety is the part that silently breaks graceful shutdown. When
`stopAll` runs, the root context is *already cancelled* — that cancellation is
what triggered shutdown. If you pass that context to `Stop`, `Stop` gets zero
remaining budget: `server.Shutdown(rootCtx)` sees a done context and returns
`context.Canceled` immediately, draining nothing. Each `Stop` must therefore get
a *new* deadline rooted at `context.Background()`, which is never cancelled:
`context.WithTimeout(context.Background(), cfg.StopTimeout)`. The per-service
timeout is the only bound, and a `Stop` that exceeds it is logged and skipped so
one stuck teardown never starves the steps queued behind it.

Note `stopAll` cancels each service's own lifetime context (`e.cancel()`) *and*
then calls `Stop` with the fresh budget. The cancel signals the service's
long-running goroutines to exit; the fresh-budget `Stop` performs the bounded
drain. They are two different jobs.

Create `svcframe.go`:

```go
package svcframe

import (
	"context"
	"log/slog"
	"sync"
	"time"
)

// Service is the single seam every component implements.
type Service interface {
	Name() string
	Start(ctx context.Context) error
	Stop(ctx context.Context) error
}

// Config holds per-service lifecycle options.
type Config struct {
	Critical    bool
	StopTimeout time.Duration
}

type entry struct {
	svc    Service
	cfg    Config
	cancel context.CancelFunc
}

// App manages the lifecycle of registered services.
type App struct {
	mu      sync.Mutex
	entries []entry
	logger  *slog.Logger
}

// New returns an App logging to logger; nil means slog.Default().
func New(logger *slog.Logger) *App {
	if logger == nil {
		logger = slog.Default()
	}
	return &App{logger: logger}
}

// Register appends a service with its config.
func (a *App) Register(svc Service, cfg Config) {
	if cfg.StopTimeout == 0 {
		cfg.StopTimeout = 5 * time.Second
	}
	a.mu.Lock()
	defer a.mu.Unlock()
	a.entries = append(a.entries, entry{svc: svc, cfg: cfg})
}

// Run starts services in order, blocks on ctx, stops in reverse.
func (a *App) Run(ctx context.Context) error {
	a.mu.Lock()
	entries := make([]entry, len(a.entries))
	copy(entries, a.entries)
	a.mu.Unlock()

	var started []entry
	for i := range entries {
		e := entries[i]
		svcCtx, cancel := context.WithCancel(ctx)
		e.cancel = cancel
		if err := e.svc.Start(svcCtx); err != nil {
			cancel()
			if e.cfg.Critical {
				a.stopAll(started)
				return err
			}
			a.logger.Warn("non-critical start failed", "name", e.svc.Name(), "err", err)
			continue
		}
		started = append(started, e)
	}

	<-ctx.Done()
	a.stopAll(started)
	return nil
}

// stopAll tears down started services in reverse registration order. Each Stop
// gets a FRESH budget from context.Background() because the root context is
// already cancelled by the time stopAll runs. A Stop that exceeds its budget is
// logged and skipped so it cannot starve later teardown steps.
func (a *App) stopAll(started []entry) {
	for i := len(started) - 1; i >= 0; i-- {
		e := started[i]
		e.cancel()

		stopCtx, cancel := context.WithTimeout(context.Background(), e.cfg.StopTimeout)
		a.logger.Info("stopping service", "name", e.svc.Name())
		if err := e.svc.Stop(stopCtx); err != nil {
			a.logger.Warn("service stop error", "name", e.svc.Name(), "err", err)
		}
		cancel()
	}
}
```

### The runnable demo

The demo registers `db`, `cache`, `api` as function-based services (via an inline
adapter) that record their `Stop` order into a mutex-guarded slice, then prints
the observed order to show it is the reverse of registration.

Create `cmd/demo/main.go`:

```go
package main

import (
	"context"
	"fmt"
	"io"
	"log/slog"
	"sync"
	"time"

	"example.com/teardown"
)

type recorder struct {
	name  string
	order *[]string
	mu    *sync.Mutex
}

func (r *recorder) Name() string { return r.name }

func (r *recorder) Start(context.Context) error { return nil }

func (r *recorder) Stop(context.Context) error {
	r.mu.Lock()
	*r.order = append(*r.order, r.name)
	r.mu.Unlock()
	return nil
}

func main() {
	var mu sync.Mutex
	var order []string

	app := svcframe.New(slog.New(slog.NewTextHandler(io.Discard, nil)))
	for _, name := range []string{"db", "cache", "api"} {
		app.Register(&recorder{name: name, order: &order, mu: &mu}, svcframe.Config{})
	}

	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() { done <- app.Run(ctx) }()

	time.Sleep(20 * time.Millisecond)
	cancel()
	<-done

	mu.Lock()
	fmt.Println("stop order:", order)
	mu.Unlock()
}
```

Run it:

```bash
go run ./cmd/demo
```

Expected output:

```
stop order: [api cache db]
```

### Tests

`TestStopOrderIsReversed` is the framework's core contract: it registers db,
cache, api, records the `Stop` sequence, and asserts exactly `[api, cache, db]`.
`TestStopTimeoutDoesNotHang` registers a service whose `Stop` blocks on
`<-ctx.Done()`; because `Stop` gets a 30ms budget, that block returns the moment
the budget fires, and `Run` returns well under a wall-clock deadline — proving a
wedged `Stop` cannot hang shutdown.

Create `svcframe_test.go`:

```go
package svcframe

import (
	"context"
	"io"
	"log/slog"
	"sync"
	"testing"
	"time"
)

func testLogger() *slog.Logger {
	return slog.New(slog.NewTextHandler(io.Discard, nil))
}

type recordService struct {
	name   string
	stopFn func(ctx context.Context) error
}

type recorder struct {
	mu    sync.Mutex
	order []string
}

func (r *recorder) add(name string) {
	r.mu.Lock()
	r.order = append(r.order, name)
	r.mu.Unlock()
}

func (r *recorder) snapshot() []string {
	r.mu.Lock()
	defer r.mu.Unlock()
	out := make([]string, len(r.order))
	copy(out, r.order)
	return out
}

func (s *recordService) Name() string { return s.name }

func (s *recordService) Start(context.Context) error { return nil }

func (s *recordService) Stop(ctx context.Context) error { return s.stopFn(ctx) }

func TestStopOrderIsReversed(t *testing.T) {
	t.Parallel()

	rec := &recorder{}
	app := New(testLogger())
	for _, name := range []string{"db", "cache", "api"} {
		app.Register(&recordService{
			name:   name,
			stopFn: func(context.Context) error { rec.add(name); return nil },
		}, Config{})
	}

	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() { done <- app.Run(ctx) }()

	time.Sleep(10 * time.Millisecond)
	cancel()
	<-done

	want := []string{"api", "cache", "db"}
	got := rec.snapshot()
	if len(got) != len(want) {
		t.Fatalf("stop order = %v, want %v", got, want)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Fatalf("stop order[%d] = %q, want %q (full: %v)", i, got[i], want[i], got)
		}
	}
}

func TestStopTimeoutDoesNotHang(t *testing.T) {
	t.Parallel()

	app := New(testLogger())
	app.Register(&recordService{
		name: "slow",
		stopFn: func(ctx context.Context) error {
			<-ctx.Done() // block until the stop budget fires
			return ctx.Err()
		},
	}, Config{StopTimeout: 30 * time.Millisecond})

	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() { done <- app.Run(ctx) }()

	time.Sleep(10 * time.Millisecond)
	cancel()

	select {
	case <-done:
		// The 30ms stop budget fired and Run returned.
	case <-time.After(500 * time.Millisecond):
		t.Fatal("Run blocked past the stop budget")
	}
}
```

## Review

Correctness rests on two assertions. The order test must see exactly
`[api, cache, db]`; if it sees registration order, `stopAll` is iterating
forward and the reverse-teardown invariant is broken. The no-hang test must
return in well under 500ms; if it hangs, `Stop` was handed the already-cancelled
root context (zero budget, but then it would return *immediately* — the opposite
failure) or, more likely, `stopAll` reused a context that never times out. The
fresh `context.WithTimeout(context.Background(), budget)` is what makes a blocking
`Stop` return exactly at its budget and no later. Note the test's `Stop` returns
`ctx.Err()` (a `context.DeadlineExceeded`) which `stopAll` logs and moves past —
a timed-out `Stop` is not fatal to shutdown. Run `go test -race`; the recorder's
mutex is what keeps the order slice race-free under the teardown goroutine.

## Resources

- [context.WithTimeout](https://pkg.go.dev/context#WithTimeout) — deriving a fresh bounded context from `context.Background()`.
- [net/http Server.Shutdown](https://pkg.go.dev/net/http#Server.Shutdown) — the real-world consumer of the fresh stop budget.
- [context.Context.Done and Err](https://pkg.go.dev/context#Context) — how a blocking `Stop` observes its budget expiring.

---

Back to [00-concepts.md](00-concepts.md) | Previous: [02-runservice-functional-adapter.md](02-runservice-functional-adapter.md) | Next: [04-signal-driven-graceful-http-shutdown.md](04-signal-driven-graceful-http-shutdown.md)
