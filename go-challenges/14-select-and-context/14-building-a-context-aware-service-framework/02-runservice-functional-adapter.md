# Exercise 2: Adapting Plain Functions into Services

Most components are not worth a bespoke struct. A cache warmer, a ticker worker,
a one-line health poller — each is a `start` function and a `stop` function. This
module builds `RunService`, a functional adapter that turns a pair of
`func(context.Context) error` values into a `Service`, with nil-func safety so a
service that only needs a `Start` (or only a `Stop`) is trivial.

## What you'll build

```text
runservice/                   independent module: example.com/runservice
  go.mod                      go 1.26
  svcframe.go                 core App (New/Register/Run) + RunService, NewRunService
  svcframe_test.go            Name(), nil-func safety, stop-closure runs, Example
  cmd/
    demo/
      main.go                 registers two function-based services, prints stop count
```

Files: `svcframe.go`, `cmd/demo/main.go`, `svcframe_test.go`.
Implement: `NewRunService(name, start, stop)` returning a `Service`; a nil `start` or `stop` is a no-op that returns `nil`.
Test: `Name()` returns the given name; a registered `RunService`'s stop closure runs on shutdown (atomic counter); nil funcs do not panic.
Verify: `go test -count=1 -race ./...`

Set up the module:

```bash
mkdir -p ~/go-exercises/runservice/cmd/demo
cd ~/go-exercises/runservice
go mod init example.com/runservice
```

### Why an adapter, and why nil-safety matters

Forcing every trivial component to declare a struct with three methods is
boilerplate that hides intent. `NewRunService` closes over two functions and
satisfies `Service` in six lines. The nil-func handling is the detail that makes
it pleasant to use: a metrics poller might have meaningful `Start` work but
nothing to do on `Stop`, so `NewRunService("poller", startFn, nil)` should be
legal and `Stop` should simply return `nil` rather than panic on a nil call. The
adapter checks each function for nil before invoking it. This is the same shape
as `http.HandlerFunc` — a function type promoted to an interface — applied to the
lifecycle contract.

The module re-includes the minimal `App` core so it stands alone; the focus is
the adapter at the bottom of the file.

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

func (a *App) stopAll(started []entry) {
	for i := len(started) - 1; i >= 0; i-- {
		e := started[i]
		e.cancel()
		stopCtx, cancel := context.WithTimeout(context.Background(), e.cfg.StopTimeout)
		if err := e.svc.Stop(stopCtx); err != nil {
			a.logger.Warn("stop error", "name", e.svc.Name(), "err", err)
		}
		cancel()
	}
}

// RunService adapts a pair of functions into a Service. A nil start or stop
// function is treated as a no-op that returns nil, so a component that needs
// only one side of the lifecycle stays a one-liner.
type RunService struct {
	name  string
	start func(ctx context.Context) error
	stop  func(ctx context.Context) error
}

// NewRunService builds a Service from start and stop functions. Either may be
// nil.
func NewRunService(name string, start, stop func(ctx context.Context) error) *RunService {
	return &RunService{name: name, start: start, stop: stop}
}

func (r *RunService) Name() string { return r.name }

func (r *RunService) Start(ctx context.Context) error {
	if r.start == nil {
		return nil
	}
	return r.start(ctx)
}

func (r *RunService) Stop(ctx context.Context) error {
	if r.stop == nil {
		return nil
	}
	return r.stop(ctx)
}
```

### The runnable demo

The demo registers a cache warmer (start-only) and a ticker worker (start and
stop), then cancels the root context and prints how many stop closures ran. Only
the worker has a stop, so the count is 1.

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

	"example.com/runservice"
)

func main() {
	var stops atomic.Int64

	app := svcframe.New(slog.New(slog.NewTextHandler(io.Discard, nil)))

	// A cache warmer: start work only, no teardown.
	app.Register(svcframe.NewRunService("cache-warmer",
		func(context.Context) error { return nil },
		nil,
	), svcframe.Config{})

	// A ticker worker: launches a goroutine on Start, counts its Stop.
	app.Register(svcframe.NewRunService("ticker",
		func(ctx context.Context) error {
			go func() {
				t := time.NewTicker(time.Hour)
				defer t.Stop()
				for {
					select {
					case <-ctx.Done():
						return
					case <-t.C:
					}
				}
			}()
			return nil
		},
		func(context.Context) error { stops.Add(1); return nil },
	), svcframe.Config{})

	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() { done <- app.Run(ctx) }()

	time.Sleep(20 * time.Millisecond)
	cancel()
	<-done

	fmt.Printf("stops=%d\n", stops.Load())
}
```

Note the import is `example.com/runservice` but the package name is `svcframe`,
so the demo refers to it as `svcframe.`. Run it:

```bash
go run ./cmd/demo
```

Expected output:

```
stops=1
```

### Tests

`TestNewRunServiceName` pins the trivial-but-load-bearing `Name()`.
`TestNilFuncsAreNoOps` calls `Start` and `Stop` on a service built from two nil
functions and asserts neither panics nor errors. `TestStopClosureRuns` registers
a `RunService`, runs the app, cancels, and asserts the stop closure ran exactly
once. The `Example` shows the end-to-end adapter with deterministic output.

Create `svcframe_test.go`:

```go
package svcframe

import (
	"context"
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

func TestNewRunServiceName(t *testing.T) {
	t.Parallel()
	svc := NewRunService("myservice", nil, nil)
	if svc.Name() != "myservice" {
		t.Fatalf("Name() = %q, want %q", svc.Name(), "myservice")
	}
}

func TestNilFuncsAreNoOps(t *testing.T) {
	t.Parallel()
	svc := NewRunService("empty", nil, nil)
	if err := svc.Start(context.Background()); err != nil {
		t.Fatalf("Start with nil func: err = %v, want nil", err)
	}
	if err := svc.Stop(context.Background()); err != nil {
		t.Fatalf("Stop with nil func: err = %v, want nil", err)
	}
}

func TestStopClosureRuns(t *testing.T) {
	t.Parallel()
	var stops atomic.Int64

	app := New(testLogger())
	app.Register(NewRunService("worker",
		func(context.Context) error { return nil },
		func(context.Context) error { stops.Add(1); return nil },
	), Config{})

	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() { done <- app.Run(ctx) }()

	time.Sleep(10 * time.Millisecond)
	cancel()
	<-done

	if stops.Load() != 1 {
		t.Fatalf("stop closure ran %d times, want 1", stops.Load())
	}
}

func Example() {
	var stopped atomic.Int64
	app := New(testLogger())
	app.Register(NewRunService("worker",
		func(context.Context) error { return nil },
		func(context.Context) error { stopped.Add(1); return nil },
	), Config{Critical: true})

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

The adapter is correct when a `RunService` built from nil functions is a
well-behaved no-op (no panic, no error) and a `RunService` built from real
functions forwards to them. `NewRunService` returns `*RunService`, which
satisfies `Service` because all three methods are on the pointer receiver;
returning the concrete pointer rather than the interface keeps the type usable
for callers that want the concrete value while still being registrable. The
common mistake is running a work loop directly inside the `start` closure instead
of launching a goroutine and returning — the demo's ticker shows the correct
shape, where `start` spawns the loop and returns immediately so `Run` can proceed
to the next service. Run `go test -race` to confirm the stop-count accounting is
race-free.

## Resources

- [http.HandlerFunc](https://pkg.go.dev/net/http#HandlerFunc) — the canonical function-to-interface adapter this mirrors.
- [context package](https://pkg.go.dev/context) — the `func(context.Context) error` shape the closures take.
- [Testable Examples in Go](https://go.dev/blog/examples) — how the `// Output:` comment is verified by `go test`.

---

Back to [00-concepts.md](00-concepts.md) | Previous: [01-service-lifecycle-core.md](01-service-lifecycle-core.md) | Next: [03-reverse-order-stop-with-budget.md](03-reverse-order-stop-with-budget.md)
