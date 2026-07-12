# Exercise 9: Service Bootstrap — Deferred Shutdown in Reverse Startup Order

A service `Run` starts subsystems in order — open the DB pool, start the metrics
server, start the HTTP server — and registers a deferred shutdown for each *the
moment it succeeds*. Two properties fall out for free: a partial-startup failure
unwinds only the subsystems that actually started, and a full shutdown runs in
reverse startup order (LIFO). Shutdown errors from every subsystem are collected
with `errors.Join`, and the whole thing is driven by a cancelable context.

This module is fully self-contained: its own `go mod init`, all code inline, its
own demo and tests. It uses fake subsystems so no real sockets are needed, but the
`Subsystem` interface mirrors what a real `*sql.DB` or `*http.Server` wrapper
exposes.

## What you'll build

```text
bootstrap/                   independent module: example.com/bootstrap
  go.mod                     module example.com/bootstrap
  bootstrap.go               Subsystem, Run (per-subsystem defer + errors.Join)
  cmd/
    demo/
      main.go                runnable demo: start 3 subsystems, shut down on timeout
  bootstrap_test.go          reverse-order stop; partial-startup unwind; joined stop errors
```

- Files: `bootstrap.go`, `cmd/demo/main.go`, `bootstrap_test.go`.
- Implement: `Run(ctx, subs) (err error)` that, for each subsystem, calls `Start` and — on success — defers a closure calling `Stop`; a join closure registered *first* (so it runs last) collects every `Stop` error with `errors.Join`. `Run` blocks on `ctx.Done()` then returns, letting the defers unwind.
- Test (fake subsystems): all-start then cancel stops everything in reverse order; a failure starting subsystem 2 unwinds subsystem 1 only and never starts 3; multiple `Stop` errors all surface via `errors.Join`.
- Verify: `go test -count=1 -race ./...`

### Why register the shutdown right after each start

The dependency order of subsystems is their startup order: the HTTP server depends
on the metrics server being up (it reports to it) and on the DB pool being open (it
queries through it). So shutdown must be the reverse — stop accepting HTTP first,
then metrics, then close the pool last — or an in-flight request hits a closed
pool. Registering one `defer` per subsystem *immediately after its `Start`
succeeds* produces exactly this reverse order via LIFO, with no ordered list to
maintain by hand.

It also makes partial-startup failures correct automatically. If subsystem 2 fails
to start, `Run` returns before registering subsystem 2's stop *and* before starting
subsystem 3. On the way out, only subsystem 1's deferred stop runs — which is
right, because it is the only thing that actually started. You never stop a
subsystem that failed to start, and you never stop one that was never reached. This
is the defer-in-a-loop pattern used *correctly*: here the accumulation of one
deferred stop per started subsystem is exactly what you want, because they should
all fire together at `Run`'s return, in reverse.

Collecting the shutdown errors needs one ordering trick. Each stop closure appends
its error to a shared slice, and a final closure joins them into the returned
`err`. That join must run *after* all the stops. Since defers are LIFO, registering
the join closure *first* — before the loop — makes it run *last*. The join also
folds in `Run`'s own `err` (from a failed start) with `errors.Join`, so a startup
failure and any shutdown failures all reach the caller.

`Stop` is called with a fresh `context.Background()`, not the canceled `ctx`,
because shutdown of a real subsystem (e.g. `http.Server.Shutdown`) needs a live
context to run its graceful-drain deadline — passing the already-canceled context
would abort the drain immediately.

Create `bootstrap.go`:

```go
package bootstrap

import (
	"context"
	"errors"
	"fmt"
)

// Subsystem is one startable/stoppable component. Real implementations wrap a
// *sql.DB (Stop = Close) or an *http.Server (Start = ListenAndServe in a
// goroutine, Stop = Shutdown).
type Subsystem interface {
	Name() string
	Start(ctx context.Context) error
	Stop(ctx context.Context) error
}

// Run starts each subsystem in order, registering a deferred Stop for each one
// that starts, then blocks until ctx is canceled and unwinds in reverse order.
func Run(ctx context.Context, subs []Subsystem) (err error) {
	var stopErrs []error

	// Registered first, so it runs LAST: after every Stop has appended its error.
	defer func() {
		if joined := errors.Join(stopErrs...); joined != nil {
			err = errors.Join(err, joined)
		}
	}()

	for _, s := range subs {
		if startErr := s.Start(ctx); startErr != nil {
			// Subsystems started before this one still unwind via their defers;
			// this one and any after it never started.
			return fmt.Errorf("start %s: %w", s.Name(), startErr)
		}
		// Registered only after a successful Start. Reverse startup order via LIFO.
		defer func() {
			if stopErr := s.Stop(context.Background()); stopErr != nil {
				stopErrs = append(stopErrs, fmt.Errorf("stop %s: %w", s.Name(), stopErr))
			}
		}()
	}

	<-ctx.Done()
	return nil
}
```

### The runnable demo

The demo starts three fake subsystems and shuts down when its context is canceled.
It uses `signal.NotifyContext` so a real Ctrl-C would trigger shutdown, plus a
short timeout so the demo terminates on its own; the printed order shows LIFO
teardown.

Create `cmd/demo/main.go`:

```go
package main

import (
	"context"
	"fmt"
	"os"
	"os/signal"
	"time"

	"example.com/bootstrap"
)

type sub struct{ name string }

func (s sub) Name() string { return s.name }

func (s sub) Start(ctx context.Context) error {
	fmt.Println("start-" + s.name)
	return nil
}

func (s sub) Stop(ctx context.Context) error {
	fmt.Println("stop-" + s.name)
	return nil
}

func main() {
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt)
	defer stop()
	ctx, cancel := context.WithTimeout(ctx, 20*time.Millisecond)
	defer cancel()

	subs := []bootstrap.Subsystem{
		sub{name: "db"},
		sub{name: "metrics"},
		sub{name: "http"},
	}
	if err := bootstrap.Run(ctx, subs); err != nil {
		fmt.Println("run:", err)
	}
}
```

Run it:

```bash
go run ./cmd/demo
```

Expected output:

```
start-db
start-metrics
start-http
stop-http
stop-metrics
stop-db
```

### Tests

`TestStopsInReverseOrder` pre-cancels the context so `Run` starts everything then
immediately unwinds, and asserts the stop order is the reverse of the start order.
`TestPartialStartupUnwinds` makes subsystem 2 fail to start and asserts subsystem 1
was stopped while subsystem 3 was never started. `TestStopErrorsAreJoined` makes
two subsystems' `Stop` fail and asserts both surface via `errors.Join`.

Create `bootstrap_test.go`:

```go
package bootstrap

import (
	"context"
	"errors"
	"slices"
	"sync"
	"testing"
)

// recorder captures ordered lifecycle events.
type recorder struct {
	mu     sync.Mutex
	events []string
}

func (r *recorder) add(e string) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.events = append(r.events, e)
}

func (r *recorder) snapshot() []string {
	r.mu.Lock()
	defer r.mu.Unlock()
	return slices.Clone(r.events)
}

// fakeSub records start/stop and can fail either.
type fakeSub struct {
	name     string
	rec      *recorder
	startErr error
	stopErr  error
}

func (f *fakeSub) Name() string { return f.name }
func (f *fakeSub) Start(ctx context.Context) error {
	f.rec.add("start-" + f.name)
	return f.startErr
}
func (f *fakeSub) Stop(ctx context.Context) error {
	f.rec.add("stop-" + f.name)
	return f.stopErr
}

func TestStopsInReverseOrder(t *testing.T) {
	t.Parallel()

	rec := &recorder{}
	subs := []Subsystem{
		&fakeSub{name: "db", rec: rec},
		&fakeSub{name: "metrics", rec: rec},
		&fakeSub{name: "http", rec: rec},
	}

	// Pre-cancel: Run starts all, then ctx.Done() fires immediately, then unwinds.
	ctx, cancel := context.WithCancel(context.Background())
	cancel()

	if err := Run(ctx, subs); err != nil {
		t.Fatalf("Run() = %v", err)
	}

	want := []string{
		"start-db", "start-metrics", "start-http",
		"stop-http", "stop-metrics", "stop-db",
	}
	if got := rec.snapshot(); !slices.Equal(got, want) {
		t.Fatalf("events =\n  %v\nwant\n  %v", got, want)
	}
}

func TestPartialStartupUnwinds(t *testing.T) {
	t.Parallel()

	rec := &recorder{}
	errStart := errors.New("metrics port in use")
	subs := []Subsystem{
		&fakeSub{name: "db", rec: rec},
		&fakeSub{name: "metrics", rec: rec, startErr: errStart},
		&fakeSub{name: "http", rec: rec},
	}

	err := Run(context.Background(), subs)
	if !errors.Is(err, errStart) {
		t.Fatalf("Run() = %v, want to wrap errStart", err)
	}

	got := rec.snapshot()
	want := []string{"start-db", "start-metrics", "stop-db"}
	if !slices.Equal(got, want) {
		t.Fatalf("events =\n  %v\nwant\n  %v", got, want)
	}
	// http must never have started; metrics must never have stopped.
	if slices.Contains(got, "start-http") {
		t.Error("http was started after metrics failed")
	}
	if slices.Contains(got, "stop-metrics") {
		t.Error("metrics was stopped though its Start failed")
	}
}

func TestStopErrorsAreJoined(t *testing.T) {
	t.Parallel()

	rec := &recorder{}
	errDB := errors.New("db close failed")
	errHTTP := errors.New("http shutdown timed out")
	subs := []Subsystem{
		&fakeSub{name: "db", rec: rec, stopErr: errDB},
		&fakeSub{name: "http", rec: rec, stopErr: errHTTP},
	}

	ctx, cancel := context.WithCancel(context.Background())
	cancel()

	err := Run(ctx, subs)
	if !errors.Is(err, errDB) {
		t.Errorf("Run() = %v, want to wrap errDB", err)
	}
	if !errors.Is(err, errHTTP) {
		t.Errorf("Run() = %v, want to wrap errHTTP", err)
	}
}
```

## Review

`Run` is correct when three things hold: subsystems stop in reverse startup order
(`TestStopsInReverseOrder`), a failed start unwinds only what started and reaches
nothing after it (`TestPartialStartupUnwinds`), and every shutdown error surfaces
(`TestStopErrorsAreJoined`). All three come from one discipline: register a
subsystem's deferred `Stop` immediately after its `Start` succeeds, and register
the error-joining closure first so it runs last. This is the defer-in-a-loop shape
used deliberately — the accumulation is the feature, because each started subsystem
should contribute exactly one deferred stop that fires at `Run`'s return. In a real
service the subsystems wrap `*sql.DB` and `*http.Server`, `Start` for the HTTP
server launches `ListenAndServe` in a goroutine, and `Stop` calls `Shutdown` with a
bounded context; the `Run` skeleton here is exactly what drives them.

## Resources

- [`errors.Join`](https://pkg.go.dev/errors#Join) — collecting shutdown errors from every subsystem.
- [`signal.NotifyContext`](https://pkg.go.dev/os/signal#NotifyContext) — a context canceled on SIGINT/SIGTERM.
- [`http.Server.Shutdown`](https://pkg.go.dev/net/http#Server.Shutdown) — the real graceful-stop a `Stop` wraps.
- [`context` package](https://pkg.go.dev/context) — `WithCancel`/`WithTimeout` driving the lifecycle.

---

Back to [00-concepts.md](00-concepts.md) | Previous: [08-mutex-unlock-scope.md](08-mutex-unlock-scope.md) | Next: [10-slow-query-warner.md](10-slow-query-warner.md)
