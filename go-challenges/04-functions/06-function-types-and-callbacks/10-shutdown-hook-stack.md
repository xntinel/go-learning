# Exercise 10: LIFO Cleanup/Shutdown Hook Registry

Graceful shutdown runs cleanup callbacks in reverse of registration — LIFO, mirroring
`defer` — so a dependency closes after its dependents. This module builds that registry:
`Register(fn)` pushes a `CleanupFunc`, `Shutdown(ctx)` runs them reversed, honoring the
context deadline and aggregating every error with `errors.Join` instead of stopping at
the first failure.

This module is fully self-contained: its own `go mod init`, all code inline, its own demo
and tests.

## What you'll build

```text
shutdown/                   independent module: example.com/shutdown
  go.mod                    go 1.26
  shutdown.go               CleanupFunc; Registry: Register, Shutdown (LIFO, join)
  cmd/
    demo/
      main.go               runnable demo: register server/workers/db, shut down
  shutdown_test.go          LIFO order, error-continues, deadline, idempotent tests
```

Files: `shutdown.go`, `cmd/demo/main.go`, `shutdown_test.go`.
Implement: `type CleanupFunc func(ctx context.Context) error`, a `Registry` with `Register(fn)` (rejecting nil) and `Shutdown(ctx)` that runs hooks LIFO, runs all even on error, joins errors, honors ctx, and is idempotent.
Test: three hooks run reverse-of-registration; a failing hook does not stop the rest and its error is joined; a hook honoring ctx respects a short deadline; `Shutdown` is idempotent; concurrent `Register`/`Shutdown` is race-free.
Verify: `go test -count=1 -race ./...`

Set up the module:

```bash
mkdir -p ~/go-exercises/shutdown/cmd/demo
cd ~/go-exercises/shutdown
go mod init example.com/shutdown
```

### Why LIFO, and why run every hook

You start a service in dependency order: open the DB pool, start the workers that use it,
start the HTTP server that dispatches to the workers. Shutdown must reverse that: stop the
HTTP server (so no new requests arrive), then drain the workers (so nothing new touches
the DB), then close the DB pool. Closing the pool first — FIFO — tears down a dependency
while its dependents are still using it, exactly the leak `defer`'s LIFO ordering exists to
prevent. So `Register` pushes onto a stack and `Shutdown` runs the stack in reverse.

Two more properties are non-negotiable in a shutdown path. First, run *every* hook even if
an early one fails: stopping at the first error leaves the remaining resources open, which
is the opposite of what shutdown is for. Collect the failures with `errors.Join` so the
caller sees all of them and `errors.Is` can match any. Second, honor the context deadline:
`Shutdown(ctx)` is called with a bounded context (`context.WithTimeout`) during a SIGTERM
window, and each hook receives that context so a slow hook can bail out; the registry also
checks `ctx.Err()` between hooks so a blown deadline stops the sequence promptly rather
than running hooks that will only pile up more timeouts.

The registry rejects a nil `CleanupFunc` at registration — a nil hook is a nil-call panic
during shutdown, the worst possible time. `Shutdown` is idempotent: a second call is a
no-op returning nil, because shutdown paths get invoked from multiple signals
(SIGTERM plus a health-check failure) and must not double-close. A `sync.Mutex` guards the
slice so `Register` and `Shutdown` are safe under concurrency, and `slices.Reverse` (or a
reverse loop) produces the LIFO order.

Create `shutdown.go`:

```go
package shutdown

import (
	"context"
	"errors"
	"fmt"
	"sync"
)

// ErrNilHook is returned by Register for a nil hook.
var ErrNilHook = errors.New("nil cleanup hook")

// CleanupFunc releases a resource. It is ctx-first so a slow close can observe
// the shutdown deadline.
type CleanupFunc func(ctx context.Context) error

// Registry holds cleanup hooks and runs them LIFO on Shutdown. It is safe for
// concurrent Register and Shutdown.
type Registry struct {
	mu    sync.Mutex
	hooks []CleanupFunc
	done  bool
}

func NewRegistry() *Registry {
	return &Registry{}
}

// Register pushes a hook onto the stack. It rejects a nil hook.
func (r *Registry) Register(fn CleanupFunc) error {
	if fn == nil {
		return fmt.Errorf("register: %w", ErrNilHook)
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	r.hooks = append(r.hooks, fn)
	return nil
}

// Shutdown runs the hooks in reverse of registration, running every hook even if
// one fails, joining their errors. It honors ctx: if the context is cancelled it
// stops starting new hooks and includes ctx.Err() in the result. A second call
// is a no-op.
func (r *Registry) Shutdown(ctx context.Context) error {
	r.mu.Lock()
	if r.done {
		r.mu.Unlock()
		return nil
	}
	r.done = true
	hooks := r.hooks
	r.hooks = nil
	r.mu.Unlock()

	var errs []error
	for i := len(hooks) - 1; i >= 0; i-- {
		if err := ctx.Err(); err != nil {
			errs = append(errs, err)
			break
		}
		if err := hooks[i](ctx); err != nil {
			errs = append(errs, err)
		}
	}
	return errors.Join(errs...)
}
```

### The runnable demo

Create `cmd/demo/main.go`:

```go
package main

import (
	"context"
	"fmt"

	"example.com/shutdown"
)

func main() {
	r := shutdown.NewRegistry()

	// Registered in startup order: db, then workers, then server.
	_ = r.Register(func(ctx context.Context) error {
		fmt.Println("close db pool")
		return nil
	})
	_ = r.Register(func(ctx context.Context) error {
		fmt.Println("stop workers")
		return nil
	})
	_ = r.Register(func(ctx context.Context) error {
		fmt.Println("stop http server")
		return nil
	})

	// Shutdown runs them in reverse: server, workers, db.
	if err := r.Shutdown(context.Background()); err != nil {
		fmt.Println("shutdown error:", err)
	}
	// Idempotent: the second call does nothing.
	_ = r.Shutdown(context.Background())
}
```

Run it:

```bash
go run ./cmd/demo
```

Expected output:

```
stop http server
stop workers
close db pool
```

### Tests

Create `shutdown_test.go`:

```go
package shutdown

import (
	"context"
	"errors"
	"fmt"
	"sync"
	"testing"
	"time"
)

func TestLIFOOrder(t *testing.T) {
	t.Parallel()
	r := NewRegistry()
	var order []string
	for _, name := range []string{"db", "workers", "server"} {
		_ = r.Register(func(ctx context.Context) error {
			order = append(order, name)
			return nil
		})
	}
	if err := r.Shutdown(context.Background()); err != nil {
		t.Fatalf("Shutdown: %v", err)
	}
	want := []string{"server", "workers", "db"}
	if len(order) != len(want) {
		t.Fatalf("order = %v, want %v", order, want)
	}
	for i := range want {
		if order[i] != want[i] {
			t.Fatalf("order[%d] = %q, want %q (full %v)", i, order[i], want[i], order)
		}
	}
}

func TestErrorDoesNotStopTheRest(t *testing.T) {
	t.Parallel()
	failHook := errors.New("close failed")
	r := NewRegistry()
	ran := 0
	_ = r.Register(func(ctx context.Context) error { ran++; return nil })
	_ = r.Register(func(ctx context.Context) error { ran++; return failHook })
	_ = r.Register(func(ctx context.Context) error { ran++; return nil })

	err := r.Shutdown(context.Background())
	if ran != 3 {
		t.Fatalf("ran %d hooks, want 3 (a failure must not stop the rest)", ran)
	}
	if !errors.Is(err, failHook) {
		t.Fatalf("err = %v, want to contain failHook", err)
	}
}

func TestHonorsDeadline(t *testing.T) {
	t.Parallel()
	r := NewRegistry()
	_ = r.Register(func(ctx context.Context) error {
		// A hook that respects the shutdown deadline.
		select {
		case <-time.After(time.Second):
			return nil
		case <-ctx.Done():
			return ctx.Err()
		}
	})

	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Millisecond)
	defer cancel()

	start := time.Now()
	err := r.Shutdown(ctx)
	if time.Since(start) > 500*time.Millisecond {
		t.Fatalf("Shutdown ignored the deadline; took %s", time.Since(start))
	}
	if !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("err = %v, want DeadlineExceeded", err)
	}
}

func TestIdempotent(t *testing.T) {
	t.Parallel()
	r := NewRegistry()
	runs := 0
	_ = r.Register(func(ctx context.Context) error { runs++; return nil })

	if err := r.Shutdown(context.Background()); err != nil {
		t.Fatalf("first Shutdown: %v", err)
	}
	if err := r.Shutdown(context.Background()); err != nil {
		t.Fatalf("second Shutdown: %v", err)
	}
	if runs != 1 {
		t.Fatalf("hook ran %d times, want 1 (Shutdown must be idempotent)", runs)
	}
}

func TestNilHookRejected(t *testing.T) {
	t.Parallel()
	r := NewRegistry()
	if err := r.Register(nil); !errors.Is(err, ErrNilHook) {
		t.Fatalf("Register(nil) = %v, want ErrNilHook", err)
	}
}

func TestConcurrentRegisterShutdown(t *testing.T) {
	t.Parallel()
	r := NewRegistry()
	var wg sync.WaitGroup
	for range 50 {
		wg.Add(1)
		go func() {
			defer wg.Done()
			_ = r.Register(func(ctx context.Context) error { return nil })
		}()
	}
	wg.Add(1)
	go func() {
		defer wg.Done()
		_ = r.Shutdown(context.Background())
	}()
	wg.Wait()
	// A final shutdown must be safe and a no-op if already done.
	if err := r.Shutdown(context.Background()); err != nil {
		t.Fatalf("final Shutdown: %v", err)
	}
}

func ExampleRegistry_Shutdown() {
	r := NewRegistry()
	_ = r.Register(func(ctx context.Context) error { return nil })
	err := r.Shutdown(context.Background())
	fmt.Println(err == nil)
	// Output: true
}
```

## Review

The registry is correct when four properties hold together. LIFO: hooks run reverse of
registration, so a resource closes after everything that depends on it — `TestLIFOOrder`
pins `server, workers, db` from a `db, workers, server` registration. Run-all-on-error:
a failing hook neither stops the rest nor swallows its own error, and `errors.Join` makes
the failure matchable with `errors.Is` while later hooks still run. Deadline: each hook
gets the shutdown context and the registry checks `ctx.Err()` between hooks, so
`TestHonorsDeadline` returns `DeadlineExceeded` in 20 ms against a one-second hook rather
than blocking a full second. Idempotence: the `done` flag makes a second `Shutdown` a
no-op, which matters because shutdown is triggered from multiple signals. Note the loop in
`TestLIFOOrder` captures the range variable `name` in each hook closure with no `name :=
name` shadow: under Go 1.22+ the range variable is per-iteration, so each closure sees its
own value, and the old shadow is now a stale idiom. Run `-race` to confirm the mutex guards
concurrent `Register`/`Shutdown`.

## Resources

- [errors.Join](https://pkg.go.dev/errors#Join)
- [context.WithTimeout](https://pkg.go.dev/context#WithTimeout)
- [slices.Reverse](https://pkg.go.dev/slices#Reverse)
- [Go blog: Defer, Panic, and Recover](https://go.dev/blog/defer-panic-and-recover)

---

Back to [00-concepts.md](00-concepts.md) | Previous: [09-predicate-stream-filter.md](09-predicate-stream-filter.md) | Next: [11-retry-with-classifier-callback.md](11-retry-with-classifier-callback.md)
