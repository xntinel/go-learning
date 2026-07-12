# Exercise 9: Graceful Shutdown — Ordered Teardown of Server, DB, and Flushers

Graceful shutdown is `defer`/LIFO applied to the process lifecycle. Startup
acquires resources in order — open the database, then start the metrics flusher,
then start the server accepting requests — and teardown must be the reverse: stop
accepting requests first, then close the database, then flush telemetry, all under
a bounded context so a stuck dependency cannot hang the process forever. This
module builds that `Run` function with layered defers and fakes you can assert
against.

This module is fully self-contained: its own `go mod init`, its own demo, its own
tests. Nothing here imports any other exercise.

## What you'll build

```text
shutdown/                   independent module: example.com/shutdown
  go.mod
  shutdown/shutdown.go       Server/DB/Flusher interfaces; Run (layered defers); OnShutdown (AfterFunc)
  cmd/demo/main.go           cancel a context and watch ordered teardown
  shutdown/shutdown_test.go  order server->db->flush; bounded timeout; AfterFunc fires once
```

- Files: `shutdown/shutdown.go`, `cmd/demo/main.go`, `shutdown/shutdown_test.go`.
- Implement: `Run(ctx, srv, db, flusher, timeout)` that blocks until `ctx` is done, then tears down under a `context.WithTimeout` using layered defers so LIFO order gives server-shutdown, then db-close, then flush; and `OnShutdown(ctx, fn)` wrapping `context.AfterFunc`.
- Test: cancelling the context runs `srv.Shutdown` before `db.Close` before `flusher.Flush`; a server shutdown that exceeds the bounded context surfaces `context.DeadlineExceeded` while the remaining closers still run; the `AfterFunc` cleanup fires exactly once on cancel.
- Verify: `go test -count=1 -race ./...`

### LIFO defer order is the teardown order

If you think of the process as acquiring resources in a fixed order — database
first, flusher next, the request-accepting server last — then the correct teardown
is the exact reverse, and that reverse is precisely what LIFO `defer` gives you.
`Run` registers three deferred closures in acquisition order: first a closure that
flushes telemetry, then one that closes the database, then one that shuts down the
server. Because defers run last-registered-first, the server shutdown runs first,
then the database close, then the flush. The order is not something you hand-sort;
it falls out of registering cleanups in the order the resources came up.

The order matters for correctness, not just tidiness. You shut the server down
*first* so it stops accepting new requests and drains the in-flight ones
(`http.Server.Shutdown` does exactly this: it stops listeners and waits for active
requests to finish). Only once no request can still touch the database do you
close the database. And you flush telemetry *last*, so the metrics and traces from
the requests that were draining are captured before the process exits.

Two more production requirements are baked in:

- **A bounded shutdown context.** `http.Server.Shutdown(context.Background())`
  waits forever for in-flight requests; a single wedged request then blocks the
  process from ever exiting. `Run` builds a `context.WithTimeout` and passes it to
  `Shutdown`, so a stuck request causes `Shutdown` to return
  `context.DeadlineExceeded` and teardown proceeds to close the remaining
  resources instead of hanging. Every closer runs regardless of the others'
  errors; the errors are joined into the return.

- **`context.AfterFunc` for cancel-triggered cleanup.** `OnShutdown` wraps
  `context.AfterFunc(ctx, fn)`, which runs `fn` in its own goroutine once `ctx` is
  done and returns a `stop` function to cancel the registration. It is the
  idiomatic way to attach a one-shot cleanup to a context's cancellation without
  spinning up a goroutine that blocks on `<-ctx.Done()` yourself.

In production `ctx` comes from `signal.NotifyContext(ctx, os.Interrupt, syscall.SIGTERM)`,
which cancels when the process receives an interrupt or termination signal; the
demo and tests use `context.WithCancel` so cancellation is deterministic.

Create `shutdown/shutdown.go`:

```go
package shutdown

import (
	"context"
	"errors"
	"fmt"
	"time"
)

// Server stops accepting new requests and drains in-flight ones under ctx.
// *http.Server satisfies this with its Shutdown method.
type Server interface {
	Shutdown(ctx context.Context) error
}

// DB releases the database handle.
type DB interface {
	Close() error
}

// Flusher ships buffered telemetry before exit.
type Flusher interface {
	Flush() error
}

// Run blocks until ctx is cancelled, then tears down in the reverse of startup
// order using layered defers: server first (stop accepting requests), then db,
// then flusher. Shutdown is bounded by timeout so a stuck request cannot hang the
// process; every closer runs even if an earlier one errors.
func Run(ctx context.Context, srv Server, db DB, flusher Flusher, timeout time.Duration) (err error) {
	<-ctx.Done()

	shutCtx, cancel := context.WithTimeout(context.Background(), timeout)
	defer cancel()

	// Registered in startup order; LIFO makes them fire in reverse.
	defer func() { // runs LAST: flush telemetry
		if ferr := flusher.Flush(); ferr != nil {
			err = errors.Join(err, fmt.Errorf("flush: %w", ferr))
		}
	}()
	defer func() { // runs SECOND: close the database
		if derr := db.Close(); derr != nil {
			err = errors.Join(err, fmt.Errorf("db close: %w", derr))
		}
	}()
	defer func() { // runs FIRST: stop accepting requests, drain in-flight
		if serr := srv.Shutdown(shutCtx); serr != nil {
			err = errors.Join(err, fmt.Errorf("server shutdown: %w", serr))
		}
	}()

	return nil
}

// OnShutdown runs fn once when ctx is cancelled, using context.AfterFunc. The
// returned stop cancels the registration; it reports whether it prevented fn.
func OnShutdown(ctx context.Context, fn func()) (stop func() bool) {
	return context.AfterFunc(ctx, fn)
}
```

### The runnable demo

One `step` type satisfies all three interfaces, so the demo can wire three of them
as server, db, and flusher and watch the teardown order deterministically after a
context cancel.

Create `cmd/demo/main.go`:

```go
package main

import (
	"context"
	"fmt"
	"time"

	"example.com/shutdown/shutdown"
)

type step struct{ name string }

func (s step) Shutdown(ctx context.Context) error { fmt.Println("shutdown:", s.name); return nil }
func (s step) Close() error                       { fmt.Println("close:", s.name); return nil }
func (s step) Flush() error                       { fmt.Println("flush:", s.name); return nil }

func main() {
	ctx, cancel := context.WithCancel(context.Background())
	go func() {
		time.Sleep(10 * time.Millisecond)
		cancel() // stands in for a SIGTERM
	}()

	err := shutdown.Run(ctx, step{"http"}, step{"postgres"}, step{"otel"}, time.Second)
	fmt.Println("err:", err)
}
```

Run it:

```bash
go run ./cmd/demo
```

Expected output:

```
shutdown: http
close: postgres
flush: otel
err: <nil>
```

### Tests

The fakes record the order of `Shutdown`/`Close`/`Flush` into a shared slice
guarded by a mutex (the closers run on `Run`'s goroutine, but the test reads the
slice from another, so the lock keeps `-race` happy). One fake server can block
until its context expires, to prove the bounded-timeout path.

Create `shutdown/shutdown_test.go`:

```go
package shutdown

import (
	"context"
	"errors"
	"slices"
	"sync"
	"testing"
	"time"
)

type fakeResource struct {
	mu    *sync.Mutex
	order *[]string
	name  string
	slow  bool // if true, Shutdown blocks until its context expires
	err   error
}

func (f fakeResource) record(op string) {
	f.mu.Lock()
	*f.order = append(*f.order, op+":"+f.name)
	f.mu.Unlock()
}

func (f fakeResource) Shutdown(ctx context.Context) error {
	f.record("shutdown")
	if f.slow {
		<-ctx.Done()
		return ctx.Err()
	}
	return f.err
}

func (f fakeResource) Close() error { f.record("close"); return f.err }
func (f fakeResource) Flush() error { f.record("flush"); return f.err }

func TestRunTearsDownInOrder(t *testing.T) {
	t.Parallel()

	var mu sync.Mutex
	var order []string
	srv := fakeResource{mu: &mu, order: &order, name: "http"}
	db := fakeResource{mu: &mu, order: &order, name: "db"}
	fl := fakeResource{mu: &mu, order: &order, name: "otel"}

	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() { done <- Run(ctx, srv, db, fl, time.Second) }()

	cancel()
	if err := <-done; err != nil {
		t.Fatalf("Run = %v", err)
	}

	mu.Lock()
	got := slices.Clone(order)
	mu.Unlock()
	want := []string{"shutdown:http", "close:db", "flush:otel"}
	if !slices.Equal(got, want) {
		t.Fatalf("teardown order = %v, want %v", got, want)
	}
}

func TestRunBoundedShutdownTimeout(t *testing.T) {
	t.Parallel()

	var mu sync.Mutex
	var order []string
	srv := fakeResource{mu: &mu, order: &order, name: "http", slow: true}
	db := fakeResource{mu: &mu, order: &order, name: "db"}
	fl := fakeResource{mu: &mu, order: &order, name: "otel"}

	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() { done <- Run(ctx, srv, db, fl, 20*time.Millisecond) }()

	cancel()
	err := <-done
	if !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("err = %v, want DeadlineExceeded", err)
	}

	// The remaining closers still ran despite the server timing out.
	mu.Lock()
	got := slices.Clone(order)
	mu.Unlock()
	if !slices.Contains(got, "close:db") || !slices.Contains(got, "flush:otel") {
		t.Fatalf("order = %v, want db close and flush to have run", got)
	}
}

func TestOnShutdownFiresOnceOnCancel(t *testing.T) {
	t.Parallel()

	ctx, cancel := context.WithCancel(context.Background())
	fired := make(chan struct{}, 2)
	stop := OnShutdown(ctx, func() { fired <- struct{}{} })
	defer stop()

	cancel()

	select {
	case <-fired:
	case <-time.After(time.Second):
		t.Fatal("OnShutdown did not fire on cancel")
	}

	// It must not fire a second time.
	select {
	case <-fired:
		t.Fatal("OnShutdown fired more than once")
	case <-time.After(50 * time.Millisecond):
	}
}
```

## Review

`Run` is correct when teardown follows the reverse of startup — server shutdown,
then database close, then flush — which the order test asserts directly, and when
a stuck server does not hang the process: the bounded-timeout test shows
`Shutdown` returning `context.DeadlineExceeded` while the database close and the
flush still run and the error is surfaced. The mistake to avoid is
`Server.Shutdown(context.Background())`, which lets a single wedged request block
the process from ever exiting; every shutdown wait must be bounded, and teardown
must proceed to the remaining resources when the bound expires. The LIFO defer
ordering is the same principle as the connection pool's nested acquire/release,
lifted to process scope. Run `go test -race`; the shared order slice is read
across goroutines, so the mutex is what keeps the teardown-order assertion sound.

## Resources

- [net/http: Server.Shutdown](https://pkg.go.dev/net/http#Server.Shutdown)
- [context: AfterFunc](https://pkg.go.dev/context#AfterFunc)
- [context: WithTimeout](https://pkg.go.dev/context#WithTimeout)
- [os/signal: NotifyContext](https://pkg.go.dev/os/signal#NotifyContext)

---

Back to [00-concepts.md](00-concepts.md) | Previous: [08-cleanup-stack-lifo-rollback.md](08-cleanup-stack-lifo-rollback.md) | Next: [10-sandbox-acquire-static-defer-unwind.md](10-sandbox-acquire-static-defer-unwind.md)
