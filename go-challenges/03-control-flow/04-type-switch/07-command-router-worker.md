# Exercise 7: Dispatch Heterogeneous Commands in a Worker via Type Switch

A worker consumes a stream of commands — `CreateOrder`, `CancelOrder`, `Refund` —
carried as `any` on a channel, and routes each to its handler by concrete type.
This is the command-processing loop at the heart of an event-driven service, and
it is where the type-switch-versus-visitor trade-off becomes a real design
choice.

This module is fully self-contained: its own `go mod init`, all code inline, its
own demo and tests.

## What you'll build

```text
cmdrouter/                   independent module: example.com/cmdrouter
  go.mod                     go 1.26
  router.go                  Command structs; Worker.Dispatch (type switch); Worker.Run (ctx + channel)
  cmd/
    demo/
      main.go                feeds a mixed command stream through a worker goroutine
  router_test.go             mixed routing counts; unknown default; ctx cancel; value-vs-pointer
```

- Files: `router.go`, `cmd/demo/main.go`, `router_test.go`.
- Implement: command structs `CreateOrder`, `CancelOrder`, `Refund`; a `Worker`
  with `Dispatch(cmd any) error` (type switch) and `Run(ctx, in <-chan any) error`
  draining a channel, collecting per-command errors with `errors.Join`.
- Test: a mixed slice routes each command to the right handler (counters per type);
  an unregistered command type hits `default` and returns a typed error without
  stopping the worker; context cancellation stops the loop; a value command
  routes but its pointer form does not (type switch distinguishes `T` from `*T`).
- Verify: `go test -count=1 -race ./...`

Set up the module:

```bash
mkdir -p go-solutions/03-control-flow/04-type-switch/07-command-router-worker/cmd/demo
cd go-solutions/03-control-flow/04-type-switch/07-command-router-worker
```

## Type-switch dispatch versus a method on the command

Two shapes solve "run the right handler for each command". A type switch
centralizes the dispatch in one place — `Dispatch` knows every command type and
routes it. A visitor puts a `Handle(ctx) error` method on each command struct and
calls `cmd.Handle(ctx)` polymorphically. The trade-off is the axis of change: the
type switch is easy when you keep adding *operations* over a fixed command set
(add a new method beside `Dispatch`, touch no command), and the visitor is easy
when you keep adding *command types* (add a struct with a `Handle`, touch no
dispatcher). A command worker usually has a stable-ish command set and grows
operations (dispatch, validate, audit, meter), so the type switch is the common
and defensible choice here. This module documents that and picks the type switch.

The subtlety the type switch surfaces: **it distinguishes `CreateOrder` from
`*CreateOrder`.** They are different dynamic types, so `case CreateOrder` does not
match a `*CreateOrder` and vice versa. If producers send values but a switch
lists pointer cases (or the reverse), commands silently fall to `default`. This
module routes value commands and proves in a test that a pointer form is not
matched — a deliberate demonstration of the trap, and a reminder to fix the
producer/consumer to agree on value-vs-pointer.

`Run` selects on `ctx.Done()` and the input channel, so a cancelled context stops
the loop immediately; an unknown command produces a typed error that is collected
(via `errors.Join`) but does not stop the worker, because one poison message must
not halt the whole stream.

Create `router.go`:

```go
package cmdrouter

import (
	"context"
	"errors"
	"fmt"
	"sync"
)

// Command payloads carried on the worker's channel as any.
type CreateOrder struct{ ID string }
type CancelOrder struct{ ID string }
type Refund struct {
	ID     string
	Amount int64
}

// ErrUnknownCommand is returned for a command type the worker does not handle.
var ErrUnknownCommand = errors.New("unknown command")

// Worker routes commands to handlers, counting how many of each it processed.
type Worker struct {
	mu     sync.Mutex
	counts map[string]int
}

func NewWorker() *Worker {
	return &Worker{counts: make(map[string]int)}
}

// Count reports how many commands of the given kind were dispatched.
func (w *Worker) Count(kind string) int {
	w.mu.Lock()
	defer w.mu.Unlock()
	return w.counts[kind]
}

func (w *Worker) bump(kind string) {
	w.mu.Lock()
	w.counts[kind]++
	w.mu.Unlock()
}

// Dispatch routes one command by concrete type. An unhandled type (including a
// pointer where a value was expected) returns ErrUnknownCommand naming the type.
func (w *Worker) Dispatch(cmd any) error {
	switch c := cmd.(type) {
	case CreateOrder:
		w.bump("create")
		_ = c.ID
		return nil
	case CancelOrder:
		w.bump("cancel")
		_ = c.ID
		return nil
	case Refund:
		w.bump("refund")
		_ = c.Amount
		return nil
	default:
		return fmt.Errorf("%w: %T", ErrUnknownCommand, cmd)
	}
}

// Run drains in until it closes or ctx is cancelled. Per-command errors are
// collected and returned joined; a single bad command does not stop the loop.
func (w *Worker) Run(ctx context.Context, in <-chan any) error {
	var errs []error
	for {
		select {
		case <-ctx.Done():
			return errors.Join(append(errs, ctx.Err())...)
		case cmd, ok := <-in:
			if !ok {
				return errors.Join(errs...)
			}
			if err := w.Dispatch(cmd); err != nil {
				errs = append(errs, err)
			}
		}
	}
}
```

## The runnable demo

The demo starts the worker in a goroutine, feeds a mixed command stream, closes
the channel, waits, and prints the per-type counts.

Create `cmd/demo/main.go`:

```go
package main

import (
	"context"
	"fmt"
	"sync"

	"example.com/cmdrouter"
)

func main() {
	w := cmdrouter.NewWorker()
	in := make(chan any)

	var wg sync.WaitGroup
	wg.Add(1)
	go func() {
		defer wg.Done()
		_ = w.Run(context.Background(), in)
	}()

	in <- cmdrouter.CreateOrder{ID: "o1"}
	in <- cmdrouter.CreateOrder{ID: "o2"}
	in <- cmdrouter.CancelOrder{ID: "o1"}
	in <- cmdrouter.Refund{ID: "o2", Amount: 500}
	close(in)
	wg.Wait()

	fmt.Printf("create=%d cancel=%d refund=%d\n",
		w.Count("create"), w.Count("cancel"), w.Count("refund"))
}
```

Run it:

```bash
go run ./cmd/demo
```

Expected output:

```text
create=2 cancel=1 refund=1
```

## Tests

The routing test dispatches a mixed slice and checks the per-type counters. The
unknown test asserts a stray type returns `ErrUnknownCommand` and that the worker
keeps going. The cancellation test proves `Run` returns once the context is
cancelled. The value-vs-pointer test dispatches a `*CreateOrder` and asserts it
hits `default` — the type switch does not equate `T` and `*T`.

Create `router_test.go`:

```go
package cmdrouter

import (
	"context"
	"errors"
	"testing"
	"time"
)

func TestDispatchRoutesByType(t *testing.T) {
	t.Parallel()
	w := NewWorker()
	cmds := []any{
		CreateOrder{ID: "a"},
		Refund{ID: "a", Amount: 1},
		CancelOrder{ID: "a"},
		CreateOrder{ID: "b"},
	}
	for _, c := range cmds {
		if err := w.Dispatch(c); err != nil {
			t.Fatalf("Dispatch(%T): %v", c, err)
		}
	}
	if w.Count("create") != 2 || w.Count("cancel") != 1 || w.Count("refund") != 1 {
		t.Fatalf("counts create=%d cancel=%d refund=%d",
			w.Count("create"), w.Count("cancel"), w.Count("refund"))
	}
}

func TestUnknownCommandDoesNotStop(t *testing.T) {
	t.Parallel()
	w := NewWorker()
	if err := w.Dispatch(42); !errors.Is(err, ErrUnknownCommand) {
		t.Fatalf("Dispatch(int) = %v, want ErrUnknownCommand", err)
	}
	// The worker is still usable after a bad command.
	if err := w.Dispatch(CreateOrder{ID: "x"}); err != nil {
		t.Fatalf("Dispatch after bad command: %v", err)
	}
	if w.Count("create") != 1 {
		t.Fatalf("create = %d, want 1", w.Count("create"))
	}
}

func TestPointerCommandNotMatched(t *testing.T) {
	t.Parallel()
	w := NewWorker()
	// A *CreateOrder is a different dynamic type than CreateOrder.
	if err := w.Dispatch(&CreateOrder{ID: "p"}); !errors.Is(err, ErrUnknownCommand) {
		t.Fatalf("Dispatch(*CreateOrder) = %v, want ErrUnknownCommand", err)
	}
	if w.Count("create") != 0 {
		t.Fatalf("pointer command was routed to the value case")
	}
}

func TestContextCancelStops(t *testing.T) {
	t.Parallel()
	w := NewWorker()
	ctx, cancel := context.WithCancel(context.Background())
	in := make(chan any)
	done := make(chan error, 1)
	go func() { done <- w.Run(ctx, in) }()

	in <- CreateOrder{ID: "a"}
	cancel()

	select {
	case err := <-done:
		if !errors.Is(err, context.Canceled) {
			t.Fatalf("Run returned %v, want context.Canceled", err)
		}
	case <-time.After(time.Second):
		t.Fatal("Run did not stop after cancel")
	}
}
```

## Review

The router is correct when each command type reaches its handler, when an
unhandled type returns `ErrUnknownCommand` without stopping the stream, and when
context cancellation ends the loop promptly. The type-switch-specific trap is
value-versus-pointer: `case CreateOrder` and `case *CreateOrder` are different
branches, so producer and consumer must agree on which they send, or commands
vanish into `default`. The design note worth keeping is the axis-of-change
reasoning: type switch when operations grow over a fixed command set, method on
the command when command types grow — this worker took the former on purpose.

## Resources

- [Go Specification: Type switches](https://go.dev/ref/spec#Type_switches)
- [errors.Join](https://pkg.go.dev/errors#Join)
- [context.Context](https://pkg.go.dev/context#Context)
- [Effective Go: the select statement](https://go.dev/ref/spec#Select_statements)

---

Prev: [06-slog-attr-encoder.md](06-slog-attr-encoder.md) | Up: [00-concepts.md](00-concepts.md) | Next: [08-domain-error-to-http-status.md](08-domain-error-to-http-status.md)
