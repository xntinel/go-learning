# Exercise 1: The Service Contract and Ordered Lifecycle Manager

This is the spine of the framework: a `Service` interface every component
implements, and an `App` that registers services, starts them in registration
order, blocks until the root context is cancelled, and unwinds a failed critical
start. Everything in the rest of the lesson bolts onto this core.

## What you'll build

```text
svcframe/                     independent module: example.com/svcframe
  go.mod                      go 1.26
  svcframe.go                 Service, Config, App; New, Register, Run, stopAll; ErrStartFailed
  svcframe_test.go            start/stop counts, critical-abort, non-critical-continue, Example
  cmd/
    demo/
      main.go                 wires three fake services, cancels, prints start/stop counts
```

Files: `svcframe.go`, `cmd/demo/main.go`, `svcframe_test.go`.
Implement: `Service` (Name/Start/Stop), `Config{Critical, StopTimeout}`, `App` with `Register` and `Run`; a critical `Start` failure aborts boot and unwinds, a non-critical one is logged and boot continues.
Test: drive `Run` in a goroutine, cancel the root context, assert start/stop counts via `atomic.Int64`; separate tests for critical abort (wrapped error, next service never starts) and non-critical continue.
Verify: `go test -count=1 -race ./...`

### The contract and why Start must not block

`Start(ctx)` is called once, synchronously, in registration order. Its job is to
get the component *running* and return — it launches goroutines (a serve loop, a
worker), it does not *become* the loop. If `Start` blocks, `Run` never advances
to the next service and the process comes up half-started with no error to show
for it. This is the single most common bug in hand-rolled lifecycle code, so the
interface documents it and every module honors it.

`Run` derives a per-service child context from the root so that cancelling the
root propagates to every service, keeps a list of the services it actually
started, and on a critical `Start` failure calls `stopAll(started)` to unwind
before returning a wrapped error. A non-critical failure is logged and boot
continues — that severity branch is the whole point of `Config.Critical`.

The error wrapping is deliberate: `Run` wraps *both* a sentinel
(`ErrStartFailed`, so callers can branch on the class of failure with
`errors.Is`) *and* the underlying error (so the specific cause survives). Go's
`fmt.Errorf` supports two `%w` verbs in one call, which is exactly this case.

Create `svcframe.go`:

```go
package svcframe

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"sync"
	"time"
)

// ErrStartFailed classifies any critical-service start failure so callers can
// branch on it with errors.Is while still unwrapping to the concrete cause.
var ErrStartFailed = errors.New("svcframe: critical service failed to start")

// Service is the single seam every component implements. Start must not block
// indefinitely: it launches goroutines and returns once the component is
// running. Stop must complete within its budget.
type Service interface {
	Name() string
	Start(ctx context.Context) error
	Stop(ctx context.Context) error
}

// Config holds per-service lifecycle options.
type Config struct {
	Critical    bool          // if true, a Start failure aborts boot and unwinds
	StopTimeout time.Duration // budget for Stop; defaults to 5s
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

// Register appends a service with its config. Call before Run.
func (a *App) Register(svc Service, cfg Config) {
	if cfg.StopTimeout == 0 {
		cfg.StopTimeout = 5 * time.Second
	}
	a.mu.Lock()
	defer a.mu.Unlock()
	a.entries = append(a.entries, entry{svc: svc, cfg: cfg})
}

// Run starts every registered service in registration order, blocks until ctx
// is cancelled, then stops services in reverse order. A critical Start failure
// aborts boot, unwinds the services already started, and returns a wrapped
// error; a non-critical failure is logged and boot continues.
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

		a.logger.Info("starting service", "name", e.svc.Name())
		if err := e.svc.Start(svcCtx); err != nil {
			cancel()
			if e.cfg.Critical {
				a.stopAll(started)
				return fmt.Errorf("%w: %s: %w", ErrStartFailed, e.svc.Name(), err)
			}
			a.logger.Warn("non-critical service failed to start",
				"name", e.svc.Name(), "err", err)
			continue
		}
		started = append(started, e)
	}

	<-ctx.Done()
	a.stopAll(started)
	return nil
}

// stopAll tears down started services in reverse registration order. Each Stop
// receives a FRESH timeout context from context.Background(): the root context
// is already cancelled by the time stopAll runs, so reusing it would hand Stop
// zero budget.
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

The demo wires three fake services with atomic start/stop counters, runs the app,
cancels the root context after a moment, and prints the totals. No OS signals —
an explicit `cancel()` stands in for `SIGTERM`, exactly as the tests do.

Create `cmd/demo/main.go`:

```go
package main

import (
	"context"
	"fmt"
	"io"
	"log/slog"
	"sync/atomic"
	"time"

	"example.com/svcframe"
)

type counted struct {
	name    string
	started *atomic.Int64
	stopped *atomic.Int64
}

func (c *counted) Name() string { return c.name }

func (c *counted) Start(context.Context) error {
	c.started.Add(1)
	return nil
}

func (c *counted) Stop(context.Context) error {
	c.stopped.Add(1)
	return nil
}

func main() {
	var started, stopped atomic.Int64

	app := svcframe.New(slog.New(slog.NewTextHandler(io.Discard, nil)))
	for _, name := range []string{"db", "cache", "api"} {
		app.Register(&counted{name: name, started: &started, stopped: &stopped},
			svcframe.Config{Critical: true})
	}

	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() { done <- app.Run(ctx) }()

	time.Sleep(20 * time.Millisecond)
	cancel()
	<-done

	fmt.Printf("started=%d stopped=%d\n", started.Load(), stopped.Load())
}
```

Run it:

```bash
go run ./cmd/demo
```

Expected output:

```
started=3 stopped=3
```

### Tests

The tests drive the framework with `context.WithCancel` only — no OS signals —
which is exactly how you test a lifecycle manager deterministically. A fake
`Service` with atomic counters and an optional start error is all the machinery
needed. `TestServicesStartAndStop` asserts every registered service is started
and stopped. `TestCriticalServiceAborts` asserts a critical failure returns an
error wrapping both `ErrStartFailed` and the concrete cause, and that the next
service never starts. `TestNonCriticalServiceDoesNotAbort` asserts a non-critical
failure is swallowed and a downstream service still starts.

Create `svcframe_test.go`:

```go
package svcframe

import (
	"context"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"sync/atomic"
	"testing"
	"time"
)

func testLogger() *slog.Logger {
	return slog.New(slog.NewTextHandler(io.Discard, nil))
}

type fakeService struct {
	name     string
	startErr error
	started  *atomic.Int64
	stopped  *atomic.Int64
}

func (f *fakeService) Name() string { return f.name }

func (f *fakeService) Start(context.Context) error {
	if f.startErr != nil {
		return f.startErr
	}
	if f.started != nil {
		f.started.Add(1)
	}
	return nil
}

func (f *fakeService) Stop(context.Context) error {
	if f.stopped != nil {
		f.stopped.Add(1)
	}
	return nil
}

func runUntilCancel(t *testing.T, app *App) {
	t.Helper()
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() { done <- app.Run(ctx) }()

	time.Sleep(10 * time.Millisecond)
	cancel()

	select {
	case err := <-done:
		if err != nil {
			t.Fatalf("Run: err = %v, want nil", err)
		}
	case <-time.After(time.Second):
		t.Fatal("Run did not return within 1s")
	}
}

func TestServicesStartAndStop(t *testing.T) {
	t.Parallel()

	var started, stopped atomic.Int64
	app := New(testLogger())
	for range 3 {
		app.Register(&fakeService{name: "svc", started: &started, stopped: &stopped},
			Config{Critical: true})
	}

	runUntilCancel(t, app)

	if started.Load() != 3 {
		t.Fatalf("started = %d, want 3", started.Load())
	}
	if stopped.Load() != 3 {
		t.Fatalf("stopped = %d, want 3", stopped.Load())
	}
}

func TestCriticalServiceAborts(t *testing.T) {
	t.Parallel()

	startErr := errors.New("db unavailable")
	var secondStarted atomic.Int64

	app := New(testLogger())
	app.Register(&fakeService{name: "db", startErr: startErr}, Config{Critical: true})
	app.Register(&fakeService{name: "api", started: &secondStarted}, Config{Critical: true})

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	err := app.Run(ctx)
	if err == nil {
		t.Fatal("expected critical start error, got nil")
	}
	if !errors.Is(err, ErrStartFailed) {
		t.Fatalf("err = %v, want wrapping ErrStartFailed", err)
	}
	if !errors.Is(err, startErr) {
		t.Fatalf("err = %v, want wrapping the concrete startErr", err)
	}
	if secondStarted.Load() != 0 {
		t.Fatal("second service must not start after a critical failure")
	}
}

func TestNonCriticalServiceDoesNotAbort(t *testing.T) {
	t.Parallel()

	var apiStarted atomic.Int64
	app := New(testLogger())
	app.Register(&fakeService{name: "metrics", startErr: errors.New("no metrics")},
		Config{Critical: false})
	app.Register(&fakeService{name: "api", started: &apiStarted}, Config{Critical: true})

	runUntilCancel(t, app)

	if apiStarted.Load() != 1 {
		t.Fatal("api must start even when a non-critical service fails")
	}
}

func Example() {
	var stopped atomic.Int64
	app := New(testLogger())
	app.Register(&fakeService{name: "worker", stopped: &stopped}, Config{Critical: true})

	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() { done <- app.Run(ctx) }()

	time.Sleep(5 * time.Millisecond)
	cancel()
	<-done

	fmt.Println("stopped:", stopped.Load())
	// Output: stopped: 1
}
```

## Review

The core is correct when three invariants hold. Services start in registration
order and every started service is stopped: the count test proves it. A critical
`Start` failure aborts boot, unwinds what started, and returns an error that
`errors.Is` matches against both `ErrStartFailed` and the concrete cause — the
double-`%w` wrap is what makes both assertions pass, and dropping either verb
breaks one of them. A non-critical failure is swallowed and downstream services
still start. The subtle part is `stopAll` deriving a fresh budget from
`context.Background()`; the reverse-order and fresh-budget behavior get their own
dedicated test in Exercise 3, but the wiring lives here. Run `go test -race` to
confirm `Register`, `Run`, and the counters are free of data races.

## Resources

- [context package](https://pkg.go.dev/context) — `WithCancel`, `Context.Done`, cancellation propagation.
- [errors.Is and %w wrapping](https://pkg.go.dev/errors#Is) — matching a sentinel through a wrap chain.
- [log/slog](https://pkg.go.dev/log/slog) — structured logging with a discard handler for tests.
- [fmt.Errorf](https://pkg.go.dev/fmt#Errorf) — multiple `%w` verbs in one wrap.

---

Back to [00-concepts.md](00-concepts.md) | Next: [02-runservice-functional-adapter.md](02-runservice-functional-adapter.md)
