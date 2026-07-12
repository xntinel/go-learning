# Exercise 8: A Readiness Gate — Serve Only When Dependencies Are Healthy

A service that accepts traffic before its database, cache, and message broker are
reachable returns errors to real users during every deploy. Kubernetes solves the
routing half with a readiness probe; you must supply the other half: a gate that
reports not-ready until every dependency has checked out once. That gate is a
barrier over dependency probes — the last successful probe opens it. This exercise
builds it, with a startup deadline so a dependency that never comes up fails fast
instead of hanging the boot.

This module is fully self-contained: its own `go mod init`, all code inline, its
own demo and tests.

## What you'll build

```text
readiness/                  independent module: example.com/readiness
  go.mod                    go 1.26
  readiness.go              type ReadinessGate; MarkReady, Ready, WaitReady, Handler, StartProbes
  cmd/
    demo/
      main.go               3 probes succeed, gate opens, /readyz returns 200
  readiness_test.go         503-until-last-probe, idempotent open, startup-timeout (-race)
```

- Files: `readiness.go`, `cmd/demo/main.go`, `readiness_test.go`.
- Implement: a `ReadinessGate` over N probes whose `MarkReady` opens the gate when the last probe succeeds, a `/readyz` handler returning 503 vs 200, and `WaitReady(ctx)` that fails with a startup-timeout error if the deadline elapses first.
- Test: `/readyz` returns 503 while any probe is pending, flips to 200 exactly when the Nth succeeds, and stays 200 (idempotent, no double-close panic); if the deadline elapses with a probe still failing, `WaitReady` returns a startup-timeout error and `/readyz` stays 503.
- Verify: `go test -count=1 -race ./...`

Set up the module:

```bash
go mod edit -go=1.26
```

### Why a readiness gate is a barrier

The gate holds a countdown starting at N (the number of dependencies). Each
dependency's probe, once it succeeds, calls `MarkReady`, which decrements the
count; the call that brings the count to zero is the last arrival, and it opens
the gate by closing a `ready` channel. That is exactly barrier semantics: N
participants, and the Nth arrival releases. `Ready` is a non-blocking check of
whether the channel is closed (a `select` with `default`); the `/readyz` handler
returns 200 when `Ready` is true and 503 otherwise, which is the contract a
Kubernetes readiness probe expects.

Two hazards need guarding. First, several probes may finish concurrently, so the
close must happen exactly once — a second `close` panics. The decrement reaching
zero happens for exactly one caller (atomic subtraction returns zero to only one
goroutine), and a `sync.Once` around the close is a belt-and-suspenders guarantee
against any double-open. Second, startup that never becomes healthy must not hang
forever. `WaitReady(ctx)` selects on the `ready` channel and `ctx.Done()`; if the
deadline fires first it returns a startup-timeout error joined with the context
error, so the boot sequence can log and exit non-zero instead of blocking a pod
that will never pass its probe.

`StartProbes` launches one goroutine per probe that retries the probe until it
succeeds or the context is cancelled, calling `MarkReady` on success. That models
a real health subsystem polling dependencies during startup.

Create `readiness.go`:

```go
package readiness

import (
	"context"
	"errors"
	"net/http"
	"sync"
	"sync/atomic"
	"time"
)

// ErrStartupTimeout is returned by WaitReady when the deadline elapses before
// all dependencies have reported ready.
var ErrStartupTimeout = errors.New("startup timeout: not all dependencies became ready")

// Probe checks one dependency. A nil return means healthy.
type Probe func(ctx context.Context) error

// ReadinessGate opens once every registered dependency has reported ready. It is
// a barrier: the last MarkReady closes the gate and it stays open.
type ReadinessGate struct {
	remaining atomic.Int64
	ready     chan struct{}
	once      sync.Once
}

// NewReadinessGate returns a gate that opens after n dependencies report ready.
func NewReadinessGate(n int) *ReadinessGate {
	g := &ReadinessGate{ready: make(chan struct{})}
	g.remaining.Store(int64(n))
	return g
}

// MarkReady records one dependency as healthy. The call that brings the count to
// zero opens the gate. Extra calls are harmless.
func (g *ReadinessGate) MarkReady() {
	if g.remaining.Add(-1) == 0 {
		g.once.Do(func() { close(g.ready) })
	}
}

// Ready reports whether the gate is open, without blocking.
func (g *ReadinessGate) Ready() bool {
	select {
	case <-g.ready:
		return true
	default:
		return false
	}
}

// WaitReady blocks until the gate opens or ctx is done. On timeout it returns
// ErrStartupTimeout joined with the context error.
func (g *ReadinessGate) WaitReady(ctx context.Context) error {
	select {
	case <-g.ready:
		return nil
	case <-ctx.Done():
		return errors.Join(ErrStartupTimeout, ctx.Err())
	}
}

// Handler serves a Kubernetes-style readiness endpoint: 200 when open, 503 when
// not.
func (g *ReadinessGate) Handler() http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if g.Ready() {
			w.WriteHeader(http.StatusOK)
			_, _ = w.Write([]byte("ready"))
			return
		}
		w.WriteHeader(http.StatusServiceUnavailable)
		_, _ = w.Write([]byte("not ready"))
	})
}

// StartProbes launches a goroutine per probe that retries until the probe
// succeeds or ctx is cancelled, calling MarkReady on the first success.
func (g *ReadinessGate) StartProbes(ctx context.Context, probes ...Probe) {
	for _, p := range probes {
		go func() {
			for {
				if err := p(ctx); err == nil {
					g.MarkReady()
					return
				}
				select {
				case <-ctx.Done():
					return
				case <-time.After(5 * time.Millisecond):
				}
			}
		}()
	}
}
```

### The runnable demo

The demo registers three probes that all succeed immediately, waits for the gate
with a generous deadline, and reports the `/readyz` status code before and after.

Create `cmd/demo/main.go`:

```go
package main

import (
	"context"
	"fmt"
	"net/http"
	"net/http/httptest"
	"time"

	"example.com/readiness"
)

func main() {
	gate := readiness.NewReadinessGate(3)

	status := func() int {
		rec := httptest.NewRecorder()
		gate.Handler().ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/readyz", nil))
		return rec.Code
	}
	fmt.Printf("before probes: /readyz=%d\n", status())

	ok := func(ctx context.Context) error { return nil }
	gate.StartProbes(context.Background(), ok, ok, ok)

	ctx, cancel := context.WithTimeout(context.Background(), time.Second)
	defer cancel()
	if err := gate.WaitReady(ctx); err != nil {
		fmt.Printf("startup failed: %v\n", err)
		return
	}
	fmt.Printf("after probes: /readyz=%d\n", status())
}
```

Run it:

```bash
go run ./cmd/demo
```

Expected output:

```
before probes: /readyz=503
after probes: /readyz=200
```

### Tests

`TestGateOpensOnLastProbe` calls `MarkReady` two of three times and asserts
`/readyz` is 503, then the third time and asserts it flips to 200, then a fourth
time to prove the extra call does not panic and the endpoint stays 200 —
exercising the idempotent open. `TestStartupTimeout` registers three probes where
one always fails, drives them with `StartProbes` under a short deadline, and
asserts `WaitReady` returns `ErrStartupTimeout` (and `context.DeadlineExceeded`)
while `/readyz` stays 503.

Create `readiness_test.go`:

```go
package readiness

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"
)

func status(t *testing.T, g *ReadinessGate) int {
	t.Helper()
	rec := httptest.NewRecorder()
	g.Handler().ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/readyz", nil))
	return rec.Code
}

func TestGateOpensOnLastProbe(t *testing.T) {
	t.Parallel()

	g := NewReadinessGate(3)

	g.MarkReady()
	g.MarkReady()
	if code := status(t, g); code != http.StatusServiceUnavailable {
		t.Fatalf("/readyz with 2/3 ready = %d, want 503", code)
	}

	g.MarkReady() // the last arrival opens the gate
	if code := status(t, g); code != http.StatusOK {
		t.Fatalf("/readyz with 3/3 ready = %d, want 200", code)
	}

	g.MarkReady() // extra call must not panic or reclose
	if code := status(t, g); code != http.StatusOK {
		t.Fatalf("/readyz after extra MarkReady = %d, want 200", code)
	}
}

func TestStartupTimeout(t *testing.T) {
	t.Parallel()

	g := NewReadinessGate(3)
	ok := func(ctx context.Context) error { return nil }
	bad := func(ctx context.Context) error { return errors.New("dependency down") }

	ctx, cancel := context.WithTimeout(context.Background(), 40*time.Millisecond)
	defer cancel()
	g.StartProbes(ctx, ok, ok, bad)

	err := g.WaitReady(ctx)
	if !errors.Is(err, ErrStartupTimeout) {
		t.Fatalf("WaitReady err = %v, want ErrStartupTimeout", err)
	}
	if !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("WaitReady err = %v, want it to wrap DeadlineExceeded", err)
	}
	if code := status(t, g); code != http.StatusServiceUnavailable {
		t.Fatalf("/readyz after failed startup = %d, want 503", code)
	}
}
```

## Review

The readiness gate is correct when `/readyz` is 503 until the last dependency
reports and 200 forever after, with no panic when probes finish concurrently or
`MarkReady` is called an extra time — the atomic countdown plus `sync.Once`
guarantees the open happens exactly once. Modeling it as a barrier is the insight:
N probes, and the Nth arrival releases. The startup deadline is the other half of
production-readiness: without `WaitReady`'s `ctx.Done()` branch, a permanently
unreachable dependency hangs the boot; with it, startup fails fast with an error
you can log and exit on. Run `-race`, since real probes call `MarkReady` from
independent goroutines.

## Resources

- [Kubernetes: Configure Liveness, Readiness and Startup Probes](https://kubernetes.io/docs/tasks/configure-pod-container/configure-liveness-readiness-startup-probes/) — the 200-vs-503 readiness contract this gate implements.
- [sync.Once](https://pkg.go.dev/sync#Once) — guaranteeing the gate opens exactly once.
- [errors.Join](https://pkg.go.dev/errors#Join) — combining the startup-timeout sentinel with the context error.

---

Back to [00-concepts.md](00-concepts.md) | Previous: [07-reusable-cyclic-barrier.md](07-reusable-cyclic-barrier.md) | Next: [09-drain-on-shutdown-semaphore.md](09-drain-on-shutdown-semaphore.md)
