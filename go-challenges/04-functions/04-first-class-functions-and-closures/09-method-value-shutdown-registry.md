# Exercise 9: Graceful-Shutdown Registry Built from Method Values

Graceful shutdown is where method values earn their keep. A `Registry` collects
named shutdown functions and runs them in LIFO order on termination; the
components it shuts down — an HTTP server, a DB pool — register their `Close`
methods as *method values* (`srv.Shutdown`, `pool.Close`) with the receiver
already bound. This module builds that registry and shows the difference between a
method value (receiver bound now) and a method expression (receiver supplied
later).

This module is fully self-contained.

## What you'll build

```text
shutdown/                  independent module: example.com/shutdown
  go.mod                   go 1.26
  shutdown.go              Registry, Register, ShutdownAll (LIFO), Pool component
  cmd/
    demo/
      main.go              registers http.Server.Shutdown and Pool.Close
  shutdown_test.go         LIFO order, error aggregation, receiver binding
```

- Files: `shutdown.go`, `cmd/demo/main.go`, `shutdown_test.go`.
- Implement: `Registry` with `Register(name string, fn func(context.Context) error)` and `ShutdownAll(ctx) error` running hooks in reverse (LIFO), aggregating errors with `errors.Join`, stopping early only if `ctx` is already cancelled.
- Test: closers append their name to a captured slice to prove LIFO order; a closer returning an error is included in the joined result but does not stop the others; a method value bound to one receiver closes THAT instance, not another.
- Verify: `go test -count=1 -race ./...`

Set up the module:

```bash
mkdir -p go-solutions/04-functions/04-first-class-functions-and-closures/09-method-value-shutdown-registry/cmd/demo
cd go-solutions/04-functions/04-first-class-functions-and-closures/09-method-value-shutdown-registry
```

### Method values bind the receiver

A method value is what you get when you write `pool.Close` without calling it:
Go captures `pool` as the bound receiver and hands you a `func(context.Context)
error` that, when called, runs `pool.Close(ctx)` on *that* pool. This is exactly
the right currency for a shutdown registry. Each component knows how to close
itself; the registry just needs to hold a bag of "close me" functions and call
them in the right order, without knowing anything about the concrete types. By
registering `srv.Shutdown` and `pool.Close`, you store each method already bound
to its instance — the registry never sees `*http.Server` or `*Pool`, only
`func(context.Context) error`.

Contrast the **method expression** `(*Pool).Close`, which does *not* bind a
receiver: its type is `func(*Pool, context.Context) error`, and you must supply
the pool at the call site. Method expressions are how you build a dispatch table
keyed by a type where the receiver varies per call; method values are how you
capture a specific object's behavior for later. The registry wants values.

`ShutdownAll` runs the hooks in **LIFO** order — reverse of registration — because
resources are typically initialized in dependency order (open the DB, then start
the server that uses it) and must be torn down in the reverse (stop accepting
requests, then close the DB). It aggregates errors with `errors.Join` so one
component failing to close does not skip the rest; a leaked resource is worse than
a logged error. It checks `ctx.Err()` before each hook so a shutdown deadline
actually bounds the teardown.

Create `shutdown.go`:

```go
package shutdown

import (
	"context"
	"errors"
	"fmt"
	"sync"
)

type hook struct {
	name string
	fn   func(context.Context) error
}

// Registry collects named shutdown hooks and runs them in LIFO order.
type Registry struct {
	mu    sync.Mutex
	hooks []hook
}

func New() *Registry { return &Registry{} }

// Register adds a named shutdown function. fn is typically a method value such
// as srv.Shutdown or pool.Close, with its receiver already bound.
func (r *Registry) Register(name string, fn func(context.Context) error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.hooks = append(r.hooks, hook{name: name, fn: fn})
}

// ShutdownAll runs every hook in reverse registration order (LIFO), aggregating
// errors with errors.Join. A hook error does not stop the others. It stops early
// only if ctx is already cancelled, recording that as an error.
func (r *Registry) ShutdownAll(ctx context.Context) error {
	r.mu.Lock()
	hooks := make([]hook, len(r.hooks))
	copy(hooks, r.hooks)
	r.mu.Unlock()

	var errs []error
	for i := len(hooks) - 1; i >= 0; i-- {
		if err := ctx.Err(); err != nil {
			errs = append(errs, fmt.Errorf("shutdown aborted before %q: %w", hooks[i].name, err))
			break
		}
		if err := hooks[i].fn(ctx); err != nil {
			errs = append(errs, fmt.Errorf("%s: %w", hooks[i].name, err))
		}
	}
	return errors.Join(errs...)
}

// Pool is a stand-in for a resource (e.g. a database connection pool) whose
// Close method is registered as a method value.
type Pool struct {
	Name   string
	closed bool
}

// Close marks the pool closed. Its signature matches the registry's hook type,
// so pool.Close is a valid method value to register.
func (p *Pool) Close(ctx context.Context) error {
	p.closed = true
	return nil
}

// Closed reports whether Close ran (exported for the demo, which is package main).
func (p *Pool) Closed() bool { return p.closed }
```

### The runnable demo

The demo registers a real `*http.Server`'s `Shutdown` method value and a `Pool`'s
`Close` method value, then shuts them down. The server was never started, so
`Shutdown` returns nil immediately; both close cleanly, and the pool reports it
was closed.

Create `cmd/demo/main.go`:

```go
package main

import (
	"context"
	"fmt"
	"net/http"

	"example.com/shutdown"
)

func main() {
	reg := shutdown.New()

	srv := &http.Server{Addr: ":0"}
	pool := &shutdown.Pool{Name: "primary"}

	reg.Register("http-server", srv.Shutdown) // method value: receiver srv bound
	reg.Register("db-pool", pool.Close)       // method value: receiver pool bound

	err := reg.ShutdownAll(context.Background())
	fmt.Printf("err=%v pool.Closed=%v\n", err, pool.Closed())
}
```

Run it:

```bash
go run ./cmd/demo
```

Expected output:

```
err=<nil> pool.Closed=true
```

### Tests

Create `shutdown_test.go`:

```go
package shutdown

import (
	"context"
	"errors"
	"slices"
	"testing"
)

// recorder is a fake component whose close method value is registered.
type recorder struct {
	name  string
	order *[]string
	err   error
}

func (rc recorder) close(ctx context.Context) error {
	*rc.order = append(*rc.order, rc.name)
	return rc.err
}

func TestShutdownRunsLIFO(t *testing.T) {
	t.Parallel()
	var order []string
	reg := New()
	reg.Register("a", recorder{name: "a", order: &order}.close)
	reg.Register("b", recorder{name: "b", order: &order}.close)
	reg.Register("c", recorder{name: "c", order: &order}.close)

	if err := reg.ShutdownAll(context.Background()); err != nil {
		t.Fatalf("ShutdownAll err = %v, want nil", err)
	}
	want := []string{"c", "b", "a"}
	if !slices.Equal(order, want) {
		t.Fatalf("order = %v, want %v (LIFO)", order, want)
	}
}

func TestShutdownAggregatesErrorsAndContinues(t *testing.T) {
	t.Parallel()
	errClose := errors.New("close failed")
	var order []string
	reg := New()
	reg.Register("ok1", recorder{name: "ok1", order: &order}.close)
	reg.Register("bad", recorder{name: "bad", order: &order, err: errClose}.close)
	reg.Register("ok2", recorder{name: "ok2", order: &order}.close)

	err := reg.ShutdownAll(context.Background())
	if !errors.Is(err, errClose) {
		t.Fatalf("err = %v, want to wrap errClose", err)
	}
	// All three ran despite the middle one failing (LIFO: ok2, bad, ok1).
	want := []string{"ok2", "bad", "ok1"}
	if !slices.Equal(order, want) {
		t.Fatalf("order = %v, want %v", order, want)
	}
}

func TestMethodValueBindsSpecificReceiver(t *testing.T) {
	t.Parallel()
	a := &Pool{Name: "a"}
	b := &Pool{Name: "b"}

	reg := New()
	reg.Register("a", a.Close) // only a's Close is registered

	if err := reg.ShutdownAll(context.Background()); err != nil {
		t.Fatalf("err = %v", err)
	}
	if !a.Closed() {
		t.Fatal("a.Close (bound method value) did not close a")
	}
	if b.Closed() {
		t.Fatal("b was closed, but only a's method value was registered")
	}
}

func TestShutdownStopsOnCancelledContext(t *testing.T) {
	t.Parallel()
	var order []string
	reg := New()
	reg.Register("a", recorder{name: "a", order: &order}.close)
	reg.Register("b", recorder{name: "b", order: &order}.close)

	ctx, cancel := context.WithCancel(context.Background())
	cancel()

	err := reg.ShutdownAll(ctx)
	if !errors.Is(err, context.Canceled) {
		t.Fatalf("err = %v, want context.Canceled", err)
	}
	if len(order) != 0 {
		t.Fatalf("ran %v hooks on a cancelled context, want none", order)
	}
}
```

## Review

The registry is correct when hooks run in LIFO order, every hook runs even if an
earlier one errors (with all errors joined and each matchable via `errors.Is`),
and a pre-cancelled context aborts before running anything. The load-bearing
concept is the method value: `pool.Close` and `srv.Shutdown` capture their
receivers at registration time, so calling them later closes those specific
instances — `TestMethodValueBindsSpecificReceiver` proves the binding by closing
`a` and leaving `b` untouched. A method value is receiver-bound; a method
expression like `(*Pool).Close` leaves the receiver as an argument. Run
`go test -race`.

## Resources

- [Go spec: Method values](https://go.dev/ref/spec#Method_values) — receiver binding when you take `x.M`.
- [pkg.go.dev: net/http Server.Shutdown](https://pkg.go.dev/net/http#Server.Shutdown) — the real method value the demo registers.
- [pkg.go.dev: errors.Join](https://pkg.go.dev/errors#Join) — aggregating shutdown errors.

---

Back to [00-concepts.md](00-concepts.md) | Previous: [08-validator-pipeline.md](08-validator-pipeline.md) | Next: [10-idempotency-once-guard.md](10-idempotency-once-guard.md)
