# Exercise 7: Bounding Boot Time So a Hung Dependency Can't Wedge Startup

A dependency that hangs during `Start` — a DB dial that never connects, a
migration that deadlocks — blocks boot forever with no signal: the process is
neither up nor crashed. This module adds a per-service `StartTimeout` built on
`context.WithTimeoutCause`, so a stalled dependency aborts boot and reports
*exactly which one* and *why* via `context.Cause`, distinguishing a `Start` that
returned an error from a `Start` that ran out of time.

## What you'll build

```text
bootbound/                    independent module: example.com/bootbound
  go.mod                      go 1.26
  svcframe.go                 App with per-service StartTimeout via WithTimeoutCause
  svcframe_test.go            hung Start aborts with the cause; normal error reported distinctly
  cmd/
    demo/
      main.go                 a hung db aborts boot, printing which dep stalled
```

Files: `svcframe.go`, `cmd/demo/main.go`, `svcframe_test.go`.
Implement: `Config.StartTimeout`; `Run` starts each service against `context.WithTimeoutCause(ctx, StartTimeout, cause)`, and on expiry aborts boot reporting `context.Cause`, unwinding already-started services in reverse; a normal `Start` error is reported as a distinct failure mode.
Test: a service whose `Start` blocks forever aborts `Run` within `StartTimeout` with the custom cause, and a previously-started service is `Stop`ped in reverse; contrast a service returning a normal error to confirm the two modes are reported distinctly.
Verify: `go test -count=1 -race ./...`

Set up the module:

```bash
mkdir -p ~/go-exercises/bootbound/cmd/demo
cd ~/go-exercises/bootbound
go mod init example.com/bootbound
```

### Bounding Start and naming the culprit

The framework bounds each `Start` call with a per-service deadline. It derives
the start context with `context.WithTimeoutCause(ctx, cfg.StartTimeout, cause)`,
where `cause` is an error that names the dependency and the budget it blew. A
well-behaved `Start` that dials the outside world passes this context to its dial
and returns `ctx.Err()` (a `context.DeadlineExceeded`) when the deadline fires.
The framework then does not rely on that returned error to identify the failure —
it asks `context.Cause(startCtx)`, which returns *the exact cause value* the
framework attached. That is the whole point of `WithTimeoutCause` over a plain
`WithTimeout`: `Context.Err()` only ever reports the generic
`context.DeadlineExceeded`, but `context.Cause` reports your sentence, so the log
reads "boot aborted: dependency \"orders-db\" did not become ready within 5s"
instead of a bare "deadline exceeded" with no hint of which service stalled.

The framework must distinguish two failure modes. If `context.Cause(startCtx)`
matches the deadline sentinel, `Start` exceeded its budget — a stall. Otherwise a
non-nil `Start` error is a genuine startup error — a dependency that answered and
said "no". They point at different root causes (a hung network path versus a
rejected credential), so they are reported as different wrapped errors. In both
cases a critical failure unwinds the already-started services in reverse before
returning; a non-critical failure is logged and boot continues.

One honest limitation to state plainly: this bounds a `Start` that *respects its
context*. A `Start` that ignores `ctx` and blocks on a bare syscall cannot be
interrupted by cancelling the context alone — bounding that requires running
`Start` in a goroutine and abandoning it, which leaks the goroutine. The
context-based bound here is the correct default because a well-written dial
already honors its context; the goroutine-abandon approach is a last resort for
third-party code that does not.

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

// ErrStartTimeout classifies a Start that exceeded its StartTimeout.
var ErrStartTimeout = errors.New("svcframe: start deadline exceeded")

// ErrStartFailed classifies a Start that returned a non-timeout error.
var ErrStartFailed = errors.New("svcframe: critical service failed to start")

// Service is the single seam every component implements.
type Service interface {
	Name() string
	Start(ctx context.Context) error
	Stop(ctx context.Context) error
}

// Config holds per-service lifecycle options.
type Config struct {
	Critical     bool
	StartTimeout time.Duration // budget for Start; 0 means unbounded
	StopTimeout  time.Duration
}

type entry struct {
	svc Service
	cfg Config
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

// Run starts services in order with a bounded Start, blocks on ctx, and stops
// in reverse. A critical Start that stalls past StartTimeout aborts boot and
// reports the dependency via context.Cause; a critical Start that returns an
// error aborts boot as a distinct failure mode. Both unwind started services.
func (a *App) Run(ctx context.Context) error {
	a.mu.Lock()
	entries := make([]entry, len(a.entries))
	copy(entries, a.entries)
	a.mu.Unlock()

	var started []entry
	for i := range entries {
		e := entries[i]
		if err := a.startOne(ctx, &e); err != nil {
			if e.cfg.Critical {
				a.stopAll(started)
				return err
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

// startOne bounds the Start call with StartTimeout (via a cause) and classifies
// the outcome. It sets e.cancel to the service's lifetime cancel on success.
func (a *App) startOne(ctx context.Context, e *entry) error {
	name := e.svc.Name()
	cause := fmt.Errorf("%w: dependency %q did not become ready within %s",
		ErrStartTimeout, name, e.cfg.StartTimeout)

	var startCtx context.Context
	var cancel context.CancelFunc
	if e.cfg.StartTimeout > 0 {
		startCtx, cancel = context.WithTimeoutCause(ctx, e.cfg.StartTimeout, cause)
	} else {
		startCtx, cancel = context.WithCancel(ctx)
	}

	err := e.svc.Start(startCtx)
	deadlineCause := context.Cause(startCtx)
	cancel()

	switch {
	case err == nil:
		return nil // started; readiness work completed within the deadline
	case errors.Is(deadlineCause, ErrStartTimeout):
		return fmt.Errorf("boot aborted: %w", deadlineCause)
	default:
		return fmt.Errorf("%w: %s: %w", ErrStartFailed, name, err)
	}
}

func (a *App) stopAll(started []entry) {
	for i := len(started) - 1; i >= 0; i-- {
		e := started[i]
		stopCtx, cancel := context.WithTimeout(context.Background(), e.cfg.StopTimeout)
		a.logger.Info("stopping service", "name", e.svc.Name())
		if err := e.svc.Stop(stopCtx); err != nil {
			a.logger.Warn("service stop error", "name", e.svc.Name(), "err", err)
		}
		cancel()
	}
}
```

The start context bounds only the `Start` call itself. A service here does
synchronous readiness work — dial, migrate, warm — and returns once ready; it
does not retain the start context for long-running goroutines (those would be
cancelled the instant `Start` returns and the framework calls `cancel`). A
production framework that also runs a serve loop would use a two-phase
`Start`/`Serve` split so the serve loop gets a lifetime context; this module
keeps the single-method contract to isolate the bounded-startup mechanism.

### The runnable demo

The demo registers a healthy `cache` and then a `db` whose `Start` blocks on its
context — a stuck dial. With a 50ms `StartTimeout`, boot aborts and prints which
dependency stalled, and the already-started `cache` is stopped in reverse.

Create `cmd/demo/main.go`:

```go
package main

import (
	"context"
	"fmt"
	"io"
	"log/slog"
	"time"

	"example.com/bootbound"
)

type dep struct {
	name    string
	hangs   bool
	stopped chan string
}

func (d *dep) Name() string { return d.name }

func (d *dep) Start(ctx context.Context) error {
	if d.hangs {
		<-ctx.Done() // a stuck dial that honors its context
		return ctx.Err()
	}
	return nil
}

func (d *dep) Stop(context.Context) error {
	d.stopped <- d.name
	return nil
}

func main() {
	stopped := make(chan string, 4)

	app := svcframe.New(slog.New(slog.NewTextHandler(io.Discard, nil)))
	app.Register(&dep{name: "cache", stopped: stopped},
		svcframe.Config{Critical: true, StartTimeout: time.Second})
	app.Register(&dep{name: "db", hangs: true, stopped: stopped},
		svcframe.Config{Critical: true, StartTimeout: 50 * time.Millisecond})

	err := app.Run(context.Background())
	fmt.Println("run error:", err)

	close(stopped)
	for name := range stopped {
		fmt.Println("stopped:", name)
	}
}
```

Run it:

```bash
go run ./cmd/demo
```

Expected output:

```
run error: boot aborted: svcframe: start deadline exceeded: dependency "db" did not become ready within 50ms
stopped: cache
```

### Tests

`TestHungStartAbortsWithCause` registers a healthy `cache` then a `db` whose
`Start` blocks forever; it asserts `Run` returns within a small multiple of the
`StartTimeout`, the error wraps `ErrStartTimeout` and names `db`, and `cache` was
`Stop`ped (the reverse unwind). `TestNormalStartErrorIsDistinct` registers a
service whose `Start` returns an ordinary error and asserts the error wraps
`ErrStartFailed`, not `ErrStartTimeout` — the two failure modes are reported
distinctly.

Create `svcframe_test.go`:

```go
package svcframe

import (
	"context"
	"errors"
	"io"
	"log/slog"
	"strings"
	"sync/atomic"
	"testing"
	"time"
)

func testLogger() *slog.Logger {
	return slog.New(slog.NewTextHandler(io.Discard, nil))
}

type dep struct {
	name     string
	startErr error
	hangs    bool
	stopped  *atomic.Int64
}

func (d *dep) Name() string { return d.name }

func (d *dep) Start(ctx context.Context) error {
	if d.hangs {
		<-ctx.Done()
		return ctx.Err()
	}
	return d.startErr
}

func (d *dep) Stop(context.Context) error {
	if d.stopped != nil {
		d.stopped.Add(1)
	}
	return nil
}

func TestHungStartAbortsWithCause(t *testing.T) {
	t.Parallel()

	var cacheStopped atomic.Int64
	app := New(testLogger())
	app.Register(&dep{name: "cache", stopped: &cacheStopped},
		Config{Critical: true, StartTimeout: time.Second})
	app.Register(&dep{name: "db", hangs: true},
		Config{Critical: true, StartTimeout: 40 * time.Millisecond})

	start := time.Now()
	err := app.Run(context.Background())
	elapsed := time.Since(start)

	if err == nil {
		t.Fatal("expected boot to abort, got nil")
	}
	if !errors.Is(err, ErrStartTimeout) {
		t.Fatalf("err = %v, want wrapping ErrStartTimeout", err)
	}
	if !strings.Contains(err.Error(), "db") {
		t.Fatalf("err = %v, want it to name the stalled dependency %q", err, "db")
	}
	if elapsed > 500*time.Millisecond {
		t.Fatalf("Run took %v, StartTimeout bound not applied", elapsed)
	}
	if cacheStopped.Load() != 1 {
		t.Fatalf("cache stopped %d times, want 1 (reverse unwind)", cacheStopped.Load())
	}
}

func TestNormalStartErrorIsDistinct(t *testing.T) {
	t.Parallel()

	app := New(testLogger())
	app.Register(&dep{name: "db", startErr: errors.New("bad credentials")},
		Config{Critical: true, StartTimeout: time.Second})

	err := app.Run(context.Background())
	if err == nil {
		t.Fatal("expected a start error, got nil")
	}
	if !errors.Is(err, ErrStartFailed) {
		t.Fatalf("err = %v, want wrapping ErrStartFailed", err)
	}
	if errors.Is(err, ErrStartTimeout) {
		t.Fatalf("err = %v, must NOT be classified as a timeout", err)
	}
}
```

## Review

The module is correct when a stalled `Start` and a failed `Start` are two
different, distinguishable outcomes. The timeout path is keyed off
`context.Cause(startCtx)` matching `ErrStartTimeout`, not off the returned error,
which is what lets the framework name the exact dependency in the abort message —
`Context.Err()` alone would only say "deadline exceeded". The error path wraps
`ErrStartFailed` and the concrete cause. Both unwind already-started services in
reverse, which the `cacheStopped` assertion pins. The classic mistake is using a
plain `context.WithTimeout` and then having no way to say *which* dependency
stalled; `WithTimeoutCause` plus `context.Cause` is precisely the tool that turns
a silent boot hang into an actionable log line. Run `go test -race`; the elapsed
assertion confirms the bound actually fired rather than the test getting lucky.

## Resources

- [context.WithTimeoutCause](https://pkg.go.dev/context#WithTimeoutCause) — attaching a custom cause to a deadline.
- [context.Cause](https://pkg.go.dev/context#Cause) — retrieving that cause after the deadline fires.
- [context.DeadlineExceeded](https://pkg.go.dev/context#pkg-variables) — the generic error `Context.Err` reports, which the cause replaces for reporting.
- [errors.Is](https://pkg.go.dev/errors#Is) — classifying the two failure modes through the wrap chain.

---

Back to [00-concepts.md](00-concepts.md) | Previous: [06-supervised-restart-with-backoff.md](06-supervised-restart-with-backoff.md) | Next: [08-errgroup-fan-out-supervision.md](08-errgroup-fan-out-supervision.md)
