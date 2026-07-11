# Exercise 3: An App That Shuts Down In Reverse Dependency Order

A service is more than one server: it is an HTTP listener plus a set of background
workers, all sharing a cancellation. The coordinator that starts them and then
tears them down in the *right order* is the heart of `main()`. This module builds
that `App`: it starts HTTP then workers, blocks until the root context is
cancelled, and drains in reverse — HTTP first so no new work enters, workers
second — aggregating per-phase failures with `errors.Join`.

This module is fully self-contained: it bundles its own minimal `Worker` and
`HTTPService`, its own demo and tests. It imports no other exercise.

## What you'll build

```text
orchestrator/              module example.com/orchestrator
  go.mod                   go 1.26
  app.go                   Worker, HTTPService, App{AddWorker,SetHTTP,Run}
  cmd/
    demo/
      main.go              two workers + HTTP, cancel, watch reverse-order drain
  app_test.go              exits-on-cancel, HTTP-first ordering, reports worker timeout
```

Files: `app.go`, `cmd/demo/main.go`, `app_test.go`.
Implement: `App` with `AddWorker`, `SetHTTP`, and `Run(ctx, httpTimeout, workerTimeout) error` that starts HTTP then workers, blocks on `ctx.Done()`, drains HTTP first then workers, and returns `errors.Join` of phase failures.
Test: three workers all observe cancellation; HTTP drains before workers; a stuck worker makes `Run` return a joined error within ~300ms rather than hanging.
Verify: `go test -count=1 -race ./...`

Set up the module:

```bash
mkdir -p ~/go-exercises/orchestrator/cmd/demo
cd ~/go-exercises/orchestrator
go mod init example.com/orchestrator
```

## Why the order and the aggregation matter

`Run` encodes the reverse-dependency invariant as three phases. It starts the
HTTP service first and the workers second, then blocks on `ctx.Done()`. When the
root context is cancelled — by a signal, in production; by `cancel()`, in tests —
it does *not* tear down in start order. It reverses: phase one drains the HTTP
server so no new requests arrive, phase two waits for the workers that may have
been serving those requests. Draining workers first would leave in-flight
handlers calling a worker-provided dependency that is already gone.

Each phase is independently bounded. The HTTP drain gets `httpTimeout`; the worker
drain gets `workerTimeout` per worker via the bounded `Wait` from Exercise 1. A
stuck worker therefore cannot hang `Run`: it degrades to a bounded delay plus an
error. This is the canary property — `TestAppReportsWorkerTimeout` cancels
immediately with a worker that never exits and asserts `Run` returns a non-nil
error within ~300ms. A broken timeout path fails that test in one of two telling
ways: it hangs (no per-phase bound) or it returns `nil` (the error was swallowed).

Failures aggregate with `errors.Join`, which combines the HTTP-drain error and
every worker-drain error into one value while preserving each for `errors.Is`
inspection. The caller — `main()` — maps a non-nil result to a non-zero exit code
so the orchestrator learns the drain was incomplete. A clean drain returns `nil`.

Create `app.go`:

```go
package orchestrator

import (
	"context"
	"errors"
	"fmt"
	"net"
	"net/http"
	"sync"
	"time"
)

// Worker is a background goroutine that runs until its context is cancelled.
type Worker struct {
	name    string
	work    func(ctx context.Context)
	stopped chan struct{}
}

// NewWorker returns a Worker that runs work when Start is called.
func NewWorker(name string, work func(ctx context.Context)) *Worker {
	return &Worker{name: name, work: work, stopped: make(chan struct{})}
}

// Name returns the worker's name.
func (w *Worker) Name() string { return w.name }

// Start launches the work goroutine, closing stopped when work returns.
func (w *Worker) Start(ctx context.Context) {
	go func() {
		defer close(w.stopped)
		w.work(ctx)
	}()
}

// Wait blocks until the worker exits or timeout elapses.
func (w *Worker) Wait(timeout time.Duration) error {
	select {
	case <-w.stopped:
		return nil
	case <-time.After(timeout):
		return fmt.Errorf("worker %s did not stop within %v", w.name, timeout)
	}
}

// HTTPService wraps an http.Server with named lifecycle methods.
type HTTPService struct {
	name   string
	server *http.Server
}

// NewHTTPService returns an HTTPService wrapping server.
func NewHTTPService(name string, server *http.Server) *HTTPService {
	return &HTTPService{name: name, server: server}
}

// Serve runs the server on ln, normalizing http.ErrServerClosed to nil.
func (s *HTTPService) Serve(ln net.Listener) error {
	err := s.server.Serve(ln)
	if errors.Is(err, http.ErrServerClosed) {
		return nil
	}
	return err
}

// Shutdown drains in-flight requests within timeout, using a fresh context.
func (s *HTTPService) Shutdown(timeout time.Duration) error {
	ctx, cancel := context.WithTimeout(context.Background(), timeout)
	defer cancel()
	if err := s.server.Shutdown(ctx); err != nil {
		return fmt.Errorf("http service %s shutdown: %w", s.name, err)
	}
	return nil
}

// App coordinates an HTTP service and a set of workers through a shared context
// and tears them down in reverse dependency order.
type App struct {
	mu      sync.Mutex
	workers []*Worker
	http    *HTTPService
	httpLn  net.Listener
}

// AddWorker registers a worker. Call before Run.
func (a *App) AddWorker(w *Worker) {
	a.mu.Lock()
	defer a.mu.Unlock()
	a.workers = append(a.workers, w)
}

// SetHTTP registers the HTTP service and the listener it should serve on. Call
// before Run. Passing an explicit listener lets a caller bind 127.0.0.1:0 in a
// test and know the drain phase acts on a real, started server.
func (a *App) SetHTTP(s *HTTPService, ln net.Listener) {
	a.mu.Lock()
	defer a.mu.Unlock()
	a.http = s
	a.httpLn = ln
}

// Run starts the HTTP service then the workers, blocks until ctx is cancelled,
// then drains in reverse order: HTTP first (no new requests), workers second.
// It returns errors.Join of the per-phase failures, or nil on a clean drain.
func (a *App) Run(ctx context.Context, httpTimeout, workerTimeout time.Duration) error {
	a.mu.Lock()
	workers := make([]*Worker, len(a.workers))
	copy(workers, a.workers)
	svc, ln := a.http, a.httpLn
	a.mu.Unlock()

	var serveErr <-chan error
	if svc != nil && ln != nil {
		ch := make(chan error, 1)
		go func() { ch <- svc.Serve(ln) }()
		serveErr = ch
	}
	for _, w := range workers {
		w.Start(ctx)
	}

	<-ctx.Done()

	var errs []error

	// Phase 1: stop ingress so no new requests enter.
	if svc != nil && ln != nil {
		if err := svc.Shutdown(httpTimeout); err != nil {
			errs = append(errs, err)
		}
		if err := <-serveErr; err != nil {
			errs = append(errs, fmt.Errorf("serve: %w", err))
		}
	}

	// Phase 2: drain workers, each bounded by workerTimeout.
	for _, w := range workers {
		if err := w.Wait(workerTimeout); err != nil {
			errs = append(errs, err)
		}
	}

	return errors.Join(errs...)
}
```

## The runnable demo

The demo wires an HTTP service on `127.0.0.1:0` plus two workers, cancels after a
moment, and prints the clean-drain result.

Create `cmd/demo/main.go`:

```go
package main

import (
	"context"
	"fmt"
	"net"
	"net/http"
	"time"

	"example.com/orchestrator"
)

func main() {
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		fmt.Println("listen:", err)
		return
	}
	svc := orchestrator.NewHTTPService("api", &http.Server{Handler: http.NewServeMux()})

	var app orchestrator.App
	app.SetHTTP(svc, ln)
	for _, name := range []string{"metrics", "cache-refresh"} {
		app.AddWorker(orchestrator.NewWorker(name, func(ctx context.Context) {
			<-ctx.Done()
		}))
	}

	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() { done <- app.Run(ctx, time.Second, time.Second) }()

	time.Sleep(20 * time.Millisecond)
	cancel()

	if err := <-done; err != nil {
		fmt.Println("shutdown had errors:", err)
		return
	}
	fmt.Println("clean shutdown: HTTP drained, then workers")
}
```

Run it:

```bash
go run ./cmd/demo
```

Expected output:

```
clean shutdown: HTTP drained, then workers
```

## Tests

`TestAppRunExitsOnContextCancel` registers three workers that block on
`ctx.Done()`, cancels, and asserts `Run` returns `nil` within 500ms with all three
having observed cancellation (an atomic counter reaching 3).
`TestAppHTTPServiceShutdownsFirst` records the phase order in a slice under a
mutex and asserts the worker's post-drain marker lands after the HTTP drain
returns. `TestAppReportsWorkerTimeout` is the canary: a stuck worker, immediate
cancel, and an assertion that `Run` returns a non-nil error within ~300ms.
`ExampleApp` locks the observable clean-drain contract with `// Output:`.

Create `app_test.go`:

```go
package orchestrator

import (
	"context"
	"fmt"
	"net"
	"net/http"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

func TestAppRunExitsOnContextCancel(t *testing.T) {
	t.Parallel()

	var stopped atomic.Int64
	var app App
	for range 3 {
		app.AddWorker(NewWorker("w", func(ctx context.Context) {
			<-ctx.Done()
			stopped.Add(1)
		}))
	}

	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() { done <- app.Run(ctx, 100*time.Millisecond, 100*time.Millisecond) }()

	cancel()

	select {
	case err := <-done:
		if err != nil {
			t.Fatalf("Run = %v, want nil", err)
		}
	case <-time.After(500 * time.Millisecond):
		t.Fatal("Run did not return within 500ms after cancel")
	}
	if got := stopped.Load(); got != 3 {
		t.Fatalf("stopped = %d, want 3", got)
	}
}

func TestAppHTTPServiceShutdownsFirst(t *testing.T) {
	t.Parallel()

	var mu sync.Mutex
	var order []string
	record := func(name string) {
		mu.Lock()
		defer mu.Unlock()
		order = append(order, name)
	}

	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("listen: %v", err)
	}
	server := &http.Server{Handler: http.NewServeMux()}
	// RegisterOnShutdown fires exactly when Server.Shutdown is called, i.e. at
	// the start of Run's phase 1, so it genuinely observes the HTTP-drain phase.
	server.RegisterOnShutdown(func() { record("http") })
	svc := NewHTTPService("api", server)

	var app App
	app.SetHTTP(svc, ln)
	// The worker records "worker" only after cancellation plus a delay, so the
	// HTTP drain has recorded "http" first if the phase order is correct.
	app.AddWorker(NewWorker("bg", func(ctx context.Context) {
		<-ctx.Done()
		time.Sleep(30 * time.Millisecond)
		record("worker")
	}))

	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() { done <- app.Run(ctx, 200*time.Millisecond, 200*time.Millisecond) }()

	time.Sleep(20 * time.Millisecond)
	cancel()

	select {
	case <-done:
	case <-time.After(time.Second):
		t.Fatal("Run did not return within 1s")
	}

	mu.Lock()
	defer mu.Unlock()
	if len(order) != 2 || order[0] != "http" || order[1] != "worker" {
		t.Fatalf("phase order = %v, want [http worker]", order)
	}
}

func TestAppReportsWorkerTimeout(t *testing.T) {
	t.Parallel()

	var app App
	app.AddWorker(NewWorker("stuck", func(ctx context.Context) {
		time.Sleep(10 * time.Second) // never exits within the test
	}))

	ctx, cancel := context.WithCancel(context.Background())
	cancel() // cancel immediately

	start := time.Now()
	err := app.Run(ctx, 50*time.Millisecond, 50*time.Millisecond)
	elapsed := time.Since(start)

	if err == nil {
		t.Fatal("Run = nil, want a joined error from the stuck worker")
	}
	if elapsed > 300*time.Millisecond {
		t.Fatalf("Run blocked %v; want under 300ms (per-phase bound)", elapsed)
	}
}

func ExampleApp() {
	var stopped atomic.Int64
	var app App
	app.AddWorker(NewWorker("w1", func(ctx context.Context) {
		<-ctx.Done()
		stopped.Add(1)
	}))
	app.AddWorker(NewWorker("w2", func(ctx context.Context) {
		<-ctx.Done()
		stopped.Add(1)
	}))

	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() { done <- app.Run(ctx, 100*time.Millisecond, 100*time.Millisecond) }()

	time.Sleep(5 * time.Millisecond)
	cancel()
	err := <-done
	fmt.Println("workers stopped:", stopped.Load(), "err:", err)
	// Output: workers stopped: 2 err: <nil>
}
```

## Review

`Run` is correct when the phases are ordered and bounded. Ordering: the HTTP drain
runs before any worker `Wait`, which `TestAppHTTPServiceShutdownsFirst` pins so a
future refactor cannot silently swap them and expose in-flight handlers to a
stopped worker dependency. Bounding: every worker `Wait` carries `workerTimeout`,
so `TestAppReportsWorkerTimeout` returns an error fast instead of hanging — the
canary for a broken timeout path. Aggregation: `errors.Join` preserves each phase
failure so `main()` can log the causes and exit non-zero. The mistakes to avoid
are draining in start order (workers before HTTP), sharing one deadline across
phases (a stuck HTTP drain starving the worker drain), and swallowing the joined
error so a broken drain still exits 0. Run `go test -race` to confirm the
concurrent `Serve`, worker goroutines, and the order slice are clean.

## Resources

- [errors.Join](https://pkg.go.dev/errors#Join) — aggregating per-phase failures while preserving each for errors.Is.
- [net/http Server.Shutdown](https://pkg.go.dev/net/http#Server.Shutdown) — the ingress-stop phase that must run first.
- [Twelve-Factor App: Disposability](https://12factor.net/disposability) — fast startup and graceful shutdown as an operational contract.

---

Back to [00-concepts.md](00-concepts.md) | Previous: [02-http-graceful-drain.md](02-http-graceful-drain.md) | Next: [04-signal-notify-context-shutdown.md](04-signal-notify-context-shutdown.md)
