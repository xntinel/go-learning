# Exercise 5: Supervising Components With errgroup So One Failure Tears Down All

An external signal is not the only reason to shut down. A listener that cannot
bind, a worker whose downstream died, a consumer that lost its broker connection —
any fatal subsystem failure should tear the *whole* service down cleanly, not
leave a half-alive process limping. `golang.org/x/sync/errgroup` is the idiom:
components share a cancellable context, the first to fail cancels it, and every
sibling that watches `ctx.Done()` winds down in response.

This module is fully self-contained: its own `go mod init`, all code inline, its
own demo and tests. It imports no other exercise.

## What you'll build

```text
supervisor/                module example.com/supervisor
  go.mod                   go 1.26 + golang.org/x/sync
  supervisor.go            Component; Supervise(ctx, comps...) via errgroup.WithContext
  cmd/
    demo/
      main.go              three components; one fails; watch coordinated teardown
  supervisor_test.go       first-error-wins, clean-cancel, ErrServerClosed normalized
```

Files: `supervisor.go`, `cmd/demo/main.go`, `supervisor_test.go`.
Implement: `Supervise(ctx context.Context, comps ...Component) error` running each `Component.Run(gctx)` under an errgroup, normalizing `http.ErrServerClosed` to `nil`.
Test: one component's sentinel error is what `Supervise` returns and the others observe cancellation; a clean parent-cancel returns `nil`; `http.ErrServerClosed` is normalized.
Verify: `go test -count=1 -race ./...`

Set up the module:

```bash
mkdir -p ~/go-exercises/supervisor/cmd/demo
cd ~/go-exercises/supervisor
go mod init example.com/supervisor
go get golang.org/x/sync/errgroup
```

## Why errgroup is the supervision primitive

`errgroup.WithContext(parent)` returns `(g, gctx)`. `gctx` is a child of `parent`
that the group cancels the moment the first `g.Go` function returns a non-nil
error. That single mechanism gives you two shutdown triggers for free. External
trigger: cancel `parent` (from a signal) and `gctx` cancels too, so every
component watching `gctx.Done()` winds down. Internal trigger: any component
returns an error, `gctx` cancels, and every *other* component winds down in
response. `g.Wait()` blocks until all components return and yields the first error
(or `nil`).

The design wraps each component's `Run` in one normalization that is not optional:
`http.ErrServerClosed` is the value `ListenAndServe`/`Serve` returns after a clean
`Shutdown`, so it means "I stopped as asked," not "I failed." If a supervised HTTP
component let that error propagate, `g.Wait` would return it as the group's first
error and — worse — the clean stop would cancel `gctx` and take every sibling down
as if a crash had occurred. Normalizing it to `nil` is what lets a graceful HTTP
stop coexist with real fatal-error supervision.

`SetLimit(n)` bounds how many `g.Go` functions run concurrently; it is useful when
the "components" are a large fan-out of short tasks rather than a handful of
long-lived subsystems. For a fixed set of subsystems you leave it unset (unlimited)
so every subsystem runs at once, which is what a service lifecycle needs.

Create `supervisor.go`:

```go
package supervisor

import (
	"context"
	"errors"
	"net/http"

	"golang.org/x/sync/errgroup"
)

// Component is one long-lived subsystem of a service: an HTTP listener, a queue
// consumer, a background worker. Run must return when its context is cancelled,
// and return a non-nil error only on a fatal failure.
type Component struct {
	Name string
	Run  func(ctx context.Context) error
}

// Supervise runs every component under a shared errgroup context. The first
// component to return a non-nil error cancels that context, which propagates the
// shutdown to all siblings; Supervise returns that first error. A clean parent
// cancellation makes every component return and Supervise returns nil.
//
// http.ErrServerClosed is normalized to nil: it is the signal that a server
// stopped as asked, not a fatal error, so it must not cancel the group.
func Supervise(ctx context.Context, comps ...Component) error {
	g, gctx := errgroup.WithContext(ctx)
	for _, c := range comps {
		g.Go(func() error {
			err := c.Run(gctx)
			if errors.Is(err, http.ErrServerClosed) {
				return nil
			}
			return err
		})
	}
	return g.Wait()
}
```

## The runnable demo

The demo runs three components: two long-lived ones that block on their context,
and one that fails after a short delay. The failure tears the other two down, and
`Supervise` returns the failure.

Create `cmd/demo/main.go`:

```go
package main

import (
	"context"
	"errors"
	"fmt"
	"time"

	"example.com/supervisor"
)

var errDownstreamDead = errors.New("payments: downstream connection lost")

func main() {
	comps := []supervisor.Component{
		{Name: "http", Run: func(ctx context.Context) error {
			<-ctx.Done()
			fmt.Println("http: stopped")
			return nil
		}},
		{Name: "metrics", Run: func(ctx context.Context) error {
			<-ctx.Done()
			fmt.Println("metrics: stopped")
			return nil
		}},
		{Name: "payments", Run: func(ctx context.Context) error {
			time.Sleep(30 * time.Millisecond)
			return errDownstreamDead
		}},
	}

	err := supervisor.Supervise(context.Background(), comps...)
	fmt.Println("supervise returned:", err)
}
```

Run it (the two "stopped" lines may print in either order):

```bash
go run ./cmd/demo
```

Expected output:

```
http: stopped
metrics: stopped
supervise returned: payments: downstream connection lost
```

## Tests

`TestFirstErrorTearsDownAll` runs three components where one returns a sentinel
after 20ms; `Supervise` must return exactly that sentinel (`errors.Is`) and the
other two must have observed cancellation (an atomic counter reaching 2).
`TestCleanCancelReturnsNil` cancels the parent with no component failing and
asserts `nil` and all three stopped. `TestErrServerClosedNormalized` has one
component return `http.ErrServerClosed`; because that is normalized to `nil` it
does not cancel the group, so a parent cancel releases the blocker and `Supervise`
returns `nil` — proving a clean HTTP stop is not read as a fatal error.

Create `supervisor_test.go`:

```go
package supervisor

import (
	"context"
	"errors"
	"net/http"
	"sync/atomic"
	"testing"
	"time"
)

var errBoom = errors.New("boom")

func TestFirstErrorTearsDownAll(t *testing.T) {
	t.Parallel()

	var stopped atomic.Int64
	blocker := func(ctx context.Context) error {
		<-ctx.Done()
		stopped.Add(1)
		return nil
	}
	comps := []Component{
		{Name: "a", Run: blocker},
		{Name: "b", Run: blocker},
		{Name: "boomer", Run: func(ctx context.Context) error {
			select {
			case <-time.After(20 * time.Millisecond):
				return errBoom
			case <-ctx.Done():
				return ctx.Err()
			}
		}},
	}

	err := Supervise(context.Background(), comps...)
	if !errors.Is(err, errBoom) {
		t.Fatalf("Supervise = %v, want errBoom", err)
	}
	if got := stopped.Load(); got != 2 {
		t.Fatalf("stopped = %d, want 2 (both blockers observed cancellation)", got)
	}
}

func TestCleanCancelReturnsNil(t *testing.T) {
	t.Parallel()

	var stopped atomic.Int64
	blocker := func(ctx context.Context) error {
		<-ctx.Done()
		stopped.Add(1)
		return nil
	}
	comps := []Component{
		{Name: "a", Run: blocker},
		{Name: "b", Run: blocker},
		{Name: "c", Run: blocker},
	}

	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() { done <- Supervise(ctx, comps...) }()

	time.Sleep(20 * time.Millisecond)
	cancel()

	select {
	case err := <-done:
		if err != nil {
			t.Fatalf("Supervise = %v, want nil on clean cancel", err)
		}
	case <-time.After(time.Second):
		t.Fatal("Supervise did not return within 1s after cancel")
	}
	if got := stopped.Load(); got != 3 {
		t.Fatalf("stopped = %d, want 3", got)
	}
}

func TestErrServerClosedNormalized(t *testing.T) {
	t.Parallel()

	comps := []Component{
		// A "server" that stops cleanly, returning the sentinel.
		{Name: "http", Run: func(ctx context.Context) error {
			return http.ErrServerClosed
		}},
		// A blocker that only the parent cancel can release.
		{Name: "worker", Run: func(ctx context.Context) error {
			<-ctx.Done()
			return nil
		}},
	}

	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() { done <- Supervise(ctx, comps...) }()

	// If ErrServerClosed were treated as fatal, gctx would already be cancelled
	// and Supervise would return it. Instead the worker is still blocked, so we
	// must cancel to release it, and the result must be nil.
	time.Sleep(20 * time.Millisecond)
	cancel()

	select {
	case err := <-done:
		if err != nil {
			t.Fatalf("Supervise = %v, want nil (ErrServerClosed normalized)", err)
		}
	case <-time.After(time.Second):
		t.Fatal("Supervise did not return within 1s")
	}
}

func ExampleSupervise() {
	comps := []Component{
		{Name: "one", Run: func(ctx context.Context) error { <-ctx.Done(); return nil }},
	}
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() { done <- Supervise(ctx, comps...) }()
	cancel()
	if err := <-done; err == nil {
		println("clean")
	}
	// Output:
}
```

## Review

`Supervise` is correct when both shutdown triggers work and the sentinel
normalization holds. `TestFirstErrorTearsDownAll` proves the internal trigger: one
failure cancels the group and the survivors observe it, and `g.Wait` surfaces
exactly the first error via `errors.Is`. `TestCleanCancelReturnsNil` proves the
external trigger returns `nil`. `TestErrServerClosedNormalized` proves the one
normalization that keeps a clean HTTP stop from masquerading as a crash. The
mistakes to avoid: letting `http.ErrServerClosed` propagate (a graceful stop then
cancels every sibling), and expecting `g.Wait` to return a *join* of all errors —
it returns only the first, by design, so per-component error aggregation belongs
in the drain phase, not here. Run `go test -race`; the shared atomic counter and
the group goroutines must be clean.

## Resources

- [golang.org/x/sync/errgroup](https://pkg.go.dev/golang.org/x/sync/errgroup) — `WithContext`, `Go`, `Wait`, and `SetLimit`.
- [errgroup.WithContext](https://pkg.go.dev/golang.org/x/sync/errgroup#WithContext) — the shared context that the first error cancels.
- [http.ErrServerClosed](https://pkg.go.dev/net/http#pkg-variables) — the sentinel a supervised HTTP component must normalize to nil.

---

Back to [00-concepts.md](00-concepts.md) | Previous: [04-signal-notify-context-shutdown.md](04-signal-notify-context-shutdown.md) | Next: [06-phase-timeout-budget-and-exit-code.md](06-phase-timeout-budget-and-exit-code.md)
