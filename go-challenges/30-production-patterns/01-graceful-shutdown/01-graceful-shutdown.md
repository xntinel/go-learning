# 1. Graceful Shutdown

Production services receive SIGTERM before being killed by the orchestrator (Kubernetes, systemd, ECS). Between the signal and the kill, your program has a window — typically 30 seconds — to finish in-flight work, flush buffers, and close resources. A hard `os.Exit` skips all of that: requests drop mid-flight, connections break, and dependent services see spurious errors.

The mechanics are harder than they look. `signal.NotifyContext` delivers the signal as a context cancellation, but that same cancelled context cannot be passed to `http.Server.Shutdown` (it is already done). Background workers need their own context derived before the signal arrives. Shutdown order matters: consumers first, then producers, then external resources.

```text
gracefulshutdown/
  go.mod
  shutdown.go
  shutdown_test.go
  cmd/demo/main.go
```

## Concepts

### Signal Interception With signal.NotifyContext

`signal.NotifyContext(parent, sig...)` returns a context that is cancelled when any of the listed OS signals arrives. It also returns a `stop` function that unregisters the handler (important: call it with `defer stop()` so that a second SIGINT restores the default behavior — killing the process — rather than being swallowed).

```go
ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
defer stop()
<-ctx.Done() // blocks until signal arrives
```

The signal context is cancelled at the moment the signal arrives. You cannot use it for work that happens during shutdown — it is already done.

### HTTP Draining With http.Server.Shutdown

`(*http.Server).Shutdown(ctx)` stops the listener immediately and then waits for active connections to become idle before closing them. It does not interrupt hijacked connections (WebSockets) — those need `Server.RegisterOnShutdown`. `Serve` and `ListenAndServe` return `http.ErrServerClosed` after `Shutdown` is called; treat that error as non-fatal.

The drain context must be fresh — created after the signal arrives with `context.WithTimeout(context.Background(), drainTimeout)`. Passing the cancelled signal context to `Shutdown` causes it to return immediately without draining anything.

### Worker Cancellation With Context And sync.WaitGroup

Background workers receive a context derived from the main context. When the signal arrives the context is cancelled; each worker detects `<-ctx.Done()` in its select loop and exits. A `sync.WaitGroup` tracks all workers so the caller can block until all have exited:

```go
wg.Add(1)
go func() {
	defer wg.Done()
	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			doWork()
		}
	}
}()
```

Workers that need a bounded cleanup budget can receive a deadline context for their final actions, separate from the main cancellation context.

### Shutdown Order: Consumers Before Producers

Shut down in dependency order — stop the top of the call stack first:

1. Stop accepting new HTTP requests (`server.Shutdown`).
2. Wait for in-flight requests to complete.
3. Cancel workers (signal through context).
4. Wait for workers to exit (`wg.Wait`).
5. Close external resources (database connections, message queue clients).

Reversing the order causes errors: if you close the database before the HTTP server is drained, in-flight requests that query the database will fail.

### The "Two Context" Pattern

Graceful shutdown always needs at least two contexts:

- The **signal context**: cancelled when the OS signal arrives. Used only to detect shutdown, never passed to the shutdown functions themselves.
- One or more **drain contexts**: created with `context.WithTimeout` after the signal arrives, each with an appropriate budget, used for `server.Shutdown` and `wg.Wait` timeouts.

## Exercises

This is a library plus a demo program. The library is verified with `go test`.

### Exercise 1: The Manager Type

Create `shutdown.go`:

```go
package shutdown

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"sync"
	"time"
)

// ErrDrainTimeout is returned when HTTP connections do not drain within the
// configured timeout.
var ErrDrainTimeout = errors.New("shutdown: HTTP drain timed out")

// WorkerFunc is the signature for a background worker. The worker must return
// when ctx is cancelled.
type WorkerFunc func(ctx context.Context)

// Closer is any resource that can be closed during shutdown.
type Closer interface {
	Close() error
}

// Manager coordinates the shutdown of an HTTP server, a set of background
// workers, and a set of closeable resources.
type Manager struct {
	server       *http.Server
	drainTimeout time.Duration
	workers      []WorkerFunc
	closers      []Closer
	mu           sync.Mutex
}

// New returns a Manager for srv. drainTimeout is the maximum time to wait for
// in-flight HTTP requests to complete.
func New(srv *http.Server, drainTimeout time.Duration) *Manager {
	return &Manager{
		server:       srv,
		drainTimeout: drainTimeout,
	}
}

// AddWorker registers a background worker. Workers are started by Run and
// cancelled when the signal arrives.
func (m *Manager) AddWorker(w WorkerFunc) {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.workers = append(m.workers, w)
}

// AddCloser registers a resource to close after workers exit.
func (m *Manager) AddCloser(c Closer) {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.closers = append(m.closers, c)
}

// Shutdown executes the shutdown sequence:
//  1. Drain in-flight HTTP requests (bounded by drainTimeout).
//  2. Cancel workers (via workerCtxCancel) and wait for them.
//  3. Close registered closers in LIFO order.
//
// workerCtxCancel must be the cancel function for the context that was passed
// to the workers when they were started.
func (m *Manager) Shutdown(workerCtxCancel context.CancelFunc) error {
	// Phase 1: drain HTTP connections.
	drainCtx, drainCancel := context.WithTimeout(context.Background(), m.drainTimeout)
	defer drainCancel()

	if err := m.server.Shutdown(drainCtx); err != nil {
		if errors.Is(err, context.DeadlineExceeded) {
			return fmt.Errorf("%w: %w", ErrDrainTimeout, err)
		}
		return fmt.Errorf("shutdown: HTTP drain: %w", err)
	}

	// Phase 2: stop workers.
	workerCtxCancel()

	// Phase 3: close resources in LIFO order.
	m.mu.Lock()
	closers := make([]Closer, len(m.closers))
	copy(closers, m.closers)
	m.mu.Unlock()

	var errs []error
	for i := len(closers) - 1; i >= 0; i-- {
		if err := closers[i].Close(); err != nil {
			errs = append(errs, err)
		}
	}
	if len(errs) > 0 {
		return fmt.Errorf("shutdown: close errors: %v", errs)
	}
	return nil
}

// Run starts the registered workers under workerCtx, starts the HTTP server,
// then blocks until signalCtx is cancelled. When signalCtx is cancelled it
// calls Shutdown and returns.
//
// Run returns a non-nil error only if the server fails to start (i.e. an
// error other than http.ErrServerClosed).
func (m *Manager) Run(signalCtx context.Context, workerCtx context.Context, workerCtxCancel context.CancelFunc) error {
	var wg sync.WaitGroup
	m.mu.Lock()
	workers := make([]WorkerFunc, len(m.workers))
	copy(workers, m.workers)
	m.mu.Unlock()

	for _, w := range workers {
		wg.Add(1)
		w := w
		go func() {
			defer wg.Done()
			w(workerCtx)
		}()
	}

	serverErr := make(chan error, 1)
	go func() {
		if err := m.server.ListenAndServe(); !errors.Is(err, http.ErrServerClosed) {
			serverErr <- err
		}
		close(serverErr)
	}()

	select {
	case err := <-serverErr:
		if err != nil {
			return fmt.Errorf("shutdown: server: %w", err)
		}
		return nil
	case <-signalCtx.Done():
	}

	if err := m.Shutdown(workerCtxCancel); err != nil {
		return err
	}
	wg.Wait()
	return nil
}
```

`Manager` holds three resource categories. `Shutdown` works in three phases. `Run` wires them together.

### Exercise 2: A Closeable Resource Stub

Append to `shutdown.go`:

```go
// closeFunc wraps a plain function as a Closer. Useful for resources that
// expose Close() error but do not implement a named interface.
type closeFunc struct {
	name string
	fn   func() error
}

func (c *closeFunc) Close() error {
	return c.fn()
}

// CloserFunc returns a Closer backed by fn. name is used for logging only.
func CloserFunc(name string, fn func() error) Closer {
	return &closeFunc{name: name, fn: fn}
}

// Name returns the name passed to CloserFunc.
func (c *closeFunc) Name() string { return c.name }
```

### Exercise 3: Tests

Create `shutdown_test.go`:

```go
package shutdown

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"sync/atomic"
	"testing"
	"time"
)

func TestShutdownDrainsInFlightRequest(t *testing.T) {
	t.Parallel()

	// Use httptest.NewServer to get a real listener without racing on ports.
	started := make(chan struct{})
	done := make(chan struct{})

	handler := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		close(started)
		<-done
		fmt.Fprintln(w, "ok")
	})

	ts := httptest.NewServer(handler)
	defer ts.Close()

	// Wrap the httptest server's underlying http.Server.
	mgr := New(ts.Config, 5*time.Second)
	_, workerCancel := context.WithCancel(context.Background())

	// Fire a request in the background.
	reqDone := make(chan error, 1)
	go func() {
		resp, err := http.Get(ts.URL + "/")
		if resp != nil {
			resp.Body.Close()
		}
		reqDone <- err
	}()

	// Wait until the handler is running, then shut down.
	<-started
	close(done)

	if err := mgr.Shutdown(workerCancel); err != nil {
		t.Fatalf("Shutdown() error = %v", err)
	}
	workerCancel()

	if err := <-reqDone; err != nil {
		t.Fatalf("in-flight request failed: %v", err)
	}
}

func TestShutdownCancelsWorkerContext(t *testing.T) {
	t.Parallel()

	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		fmt.Fprintln(w, "ok")
	}))
	defer ts.Close()

	mgr := New(ts.Config, 2*time.Second)

	workerCtx, workerCancel := context.WithCancel(context.Background())
	var workerStopped atomic.Bool

	mgr.AddWorker(func(ctx context.Context) {
		<-ctx.Done()
		workerStopped.Store(true)
	})

	// Shutdown should cancel the worker context.
	if err := mgr.Shutdown(workerCancel); err != nil {
		t.Fatalf("Shutdown() error = %v", err)
	}

	// workerCtx must be cancelled after Shutdown returns.
	select {
	case <-workerCtx.Done():
		// ok
	case <-time.After(time.Second):
		t.Fatal("workerCtx was not cancelled after Shutdown")
	}
}

func TestShutdownClosesResourcesInLIFOOrder(t *testing.T) {
	t.Parallel()

	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {}))
	defer ts.Close()

	mgr := New(ts.Config, 2*time.Second)
	_, workerCancel := context.WithCancel(context.Background())

	var order []string
	for _, name := range []string{"db", "cache", "queue"} {
		name := name
		mgr.AddCloser(CloserFunc(name, func() error {
			order = append(order, name)
			return nil
		}))
	}

	if err := mgr.Shutdown(workerCancel); err != nil {
		t.Fatalf("Shutdown() error = %v", err)
	}

	want := []string{"queue", "cache", "db"}
	for i, got := range order {
		if got != want[i] {
			t.Fatalf("close order[%d] = %q, want %q", i, got, want[i])
		}
	}
}

func TestShutdownReturnsErrDrainTimeoutOnExpiry(t *testing.T) {
	t.Parallel()

	// A handler that blocks until the request context is cancelled (which
	// happens when the server forcefully closes the connection after the drain
	// timeout) or an explicit release is signalled.
	started := make(chan struct{})
	release := make(chan struct{})
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		close(started)
		select {
		case <-r.Context().Done():
		case <-release:
		}
	}))

	// Fire a request that will block in the handler.
	go http.Get(ts.URL + "/") //nolint:errcheck

	<-started

	mgr := New(ts.Config, 50*time.Millisecond) // very short drain timeout
	_, workerCancel := context.WithCancel(context.Background())

	err := mgr.Shutdown(workerCancel)

	// Unblock the handler and clean up. CloseClientConnections cancels the
	// request context, unblocking the handler so ts.Close() does not hang.
	close(release)
	ts.CloseClientConnections()
	ts.Close()

	if !errors.Is(err, ErrDrainTimeout) {
		t.Fatalf("err = %v, want ErrDrainTimeout", err)
	}
}

func TestCloserFuncName(t *testing.T) {
	t.Parallel()

	c := CloserFunc("mydb", func() error { return nil })
	cf, ok := c.(*closeFunc)
	if !ok {
		t.Fatal("CloserFunc did not return *closeFunc")
	}
	if cf.Name() != "mydb" {
		t.Fatalf("Name() = %q, want %q", cf.Name(), "mydb")
	}
}

func ExampleCloserFunc() {
	var closed bool
	c := CloserFunc("example", func() error {
		closed = true
		return nil
	})
	_ = c.Close()
	fmt.Println(closed)
	// Output:
	// true
}
```

Your turn: add `TestShutdownWithNoWorkersOrClosers` that creates a `Manager` with no workers or closers, calls `Shutdown`, and asserts that it returns `nil`.

### Exercise 4: The Demo Program

Create `cmd/demo/main.go`:

```go
// cmd/demo/main.go
package main

import (
	"context"
	"fmt"
	"log"
	"net/http"
	"os"
	"os/signal"
	"syscall"
	"time"

	shutdown "example.com/gracefulshutdown"
)

func main() {
	addr := ":8080"
	if v := os.Getenv("ADDR"); v != "" {
		addr = v
	}

	mux := http.NewServeMux()
	mux.HandleFunc("/", func(w http.ResponseWriter, r *http.Request) {
		time.Sleep(500 * time.Millisecond)
		fmt.Fprintln(w, "ok")
	})
	mux.HandleFunc("/health", func(w http.ResponseWriter, r *http.Request) {
		fmt.Fprintln(w, "healthy")
	})

	srv := &http.Server{Addr: addr, Handler: mux}
	mgr := shutdown.New(srv, 10*time.Second)

	// Register a background worker.
	mgr.AddWorker(func(ctx context.Context) {
		ticker := time.NewTicker(3 * time.Second)
		defer ticker.Stop()
		for {
			select {
			case <-ctx.Done():
				log.Println("worker: stopped")
				return
			case <-ticker.C:
				log.Println("worker: tick")
			}
		}
	})

	// Register a simulated database closer.
	mgr.AddCloser(shutdown.CloserFunc("database", func() error {
		log.Println("database: closed")
		return nil
	}))

	// Signal context: cancelled on SIGINT or SIGTERM.
	signalCtx, stop := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	defer stop()

	// Worker context: cancelled by Shutdown after HTTP drain.
	workerCtx, workerCancel := context.WithCancel(context.Background())

	log.Printf("listening on %s", addr)
	if err := mgr.Run(signalCtx, workerCtx, workerCancel); err != nil {
		log.Fatalf("error: %v", err)
	}
	log.Println("shutdown complete")
}
```

Run the demo:

```bash
go run ./cmd/demo &
curl -s localhost:8080/health
kill -SIGTERM %1
```

## Common Mistakes

### Passing The Signal Context To server.Shutdown

Wrong:

```go
ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGTERM)
defer stop()
<-ctx.Done()
srv.Shutdown(ctx) // ctx is already cancelled — Shutdown returns immediately
```

What happens: `Shutdown` receives an already-cancelled context, returns `context.Canceled` at once, and active connections are closed without draining.

Fix: create a fresh timeout context for the drain:

```go
drainCtx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
defer cancel()
srv.Shutdown(drainCtx)
```

### Calling os.Exit Instead Of Returning From main

Wrong:

```go
sigCh := make(chan os.Signal, 1)
signal.Notify(sigCh, syscall.SIGTERM)
<-sigCh
os.Exit(0) // deferred cleanup never runs
```

What happens: `defer` calls (including context cancellation, database closes, and WaitGroup waits) are skipped.

Fix: let `main` return naturally or use `signal.NotifyContext` and allow the shutdown path to run to completion.

### Forgetting To Call The Stop Function

Wrong:

```go
ctx, _ := signal.NotifyContext(context.Background(), syscall.SIGTERM)
```

What happens: a second SIGTERM does not kill the program because the signal is still being diverted to the context. The stop function is also never called, so signal handling is never cleaned up.

Fix:

```go
ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGTERM)
defer stop()
```

### Cancelling Workers Before Draining HTTP

Wrong:

```go
workerCancel() // cancel workers first
srv.Shutdown(drainCtx) // then drain HTTP
```

What happens: in-flight HTTP handlers that call the worker's resources (e.g. a database connection managed by the worker) fail because the worker has already torn down the resource.

Fix: drain HTTP first, then cancel workers, then close external resources.

## Verification

From `~/go-exercises/gracefulshutdown`:

```bash
test -z "$(gofmt -l .)"
go vet ./...
go build ./...
go test -count=1 -race ./...
```

All four must pass. The demo binary is optional to run for manual verification.

## Summary

- `signal.NotifyContext` converts an OS signal into a context cancellation. Call `defer stop()` immediately.
- The signal context is cancelled when the signal arrives; never pass it to `server.Shutdown`.
- `http.Server.Shutdown(ctx)` stops the listener and drains in-flight connections. Use a fresh `context.WithTimeout` for the drain budget.
- Coordinate background workers with a context and `sync.WaitGroup`. Cancel the context after HTTP draining, not before.
- Shut down in order: HTTP drain → worker cancellation → external resource close (consumers before producers).
- `CloserFunc` wraps any `func() error` as a `Closer`, enabling LIFO cleanup without coupling the manager to specific resource types.

## What's Next

Next: [Layered Configuration](../02-configuration-layered/02-configuration-layered.md).

## Resources

- [os/signal.NotifyContext](https://pkg.go.dev/os/signal#NotifyContext)
- [net/http Server.Shutdown](https://pkg.go.dev/net/http#Server.Shutdown)
- [Go Blog: Contexts and structs](https://go.dev/blog/context-and-structs)
- [Go Blog: Concurrency patterns — Context](https://go.dev/blog/context)
- [net/http/httptest package](https://pkg.go.dev/net/http/httptest)
