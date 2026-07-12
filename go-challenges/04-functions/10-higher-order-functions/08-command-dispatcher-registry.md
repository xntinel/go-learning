# Exercise 8: A Handler Registry — map[string]Handler Dispatch

A webhook router, a command bus, and a job-type worker are the same artifact: a
registry of named handlers and a dispatch that looks one up by message type. Here
the handler is a first-class `func`, and `Use` wraps every registered handler with
middleware, reusing the decorator idea from the earlier exercises.

## What you'll build

```text
dispatcher/                  independent module: example.com/dispatcher
  go.mod                     go 1.25
  dispatcher.go              type Handler, Middleware; Register, Use, Dispatch; ErrNoHandler
  dispatcher_test.go         correct handler runs, unknown->ErrNoHandler, last-wins, Use wraps once, -race
  cmd/demo/
    main.go                  registers two message types and dispatches each
```

- Files: `dispatcher.go`, `dispatcher_test.go`, `cmd/demo/main.go`.
- Implement: `Handler func(context.Context, Message) error`, `Register(name, h)`, `Use(mw ...Middleware)`, `Dispatch(ctx, msg)` returning `ErrNoHandler` for unknown types, all guarded by an `RWMutex`.
- Test: the right handler runs; an unknown type returns `ErrNoHandler` via `errors.Is` and runs nothing; double-registration is last-wins; a middleware added via `Use` wraps every handler exactly once; concurrent `Dispatch` under `-race`.
- Verify: `go test -count=1 -race ./...`

Set up the module:

```bash
mkdir -p go-solutions/04-functions/10-higher-order-functions/08-command-dispatcher-registry/cmd/demo
cd go-solutions/04-functions/10-higher-order-functions/08-command-dispatcher-registry
go mod edit -go=1.25
```

### Registry, lookup, and the middleware seam

The core is a `map[string]Handler` guarded by an `RWMutex`: `Register` takes the
write lock and stores (last registration for a name wins, a policy the test pins),
`Dispatch` takes the read lock, looks up by `msg.Type`, and either calls the handler
or returns `ErrNoHandler` wrapped with the unknown type. Returning a typed sentinel
lets callers distinguish "no handler for this type" (a routing problem, maybe a
dead-letter) from a handler that ran and failed (a processing problem). The wrap
uses `%w` so `errors.Is(err, ErrNoHandler)` matches while the message still carries
the offending type for logs.

`Use(mw...)` is the decorator seam. Each `Middleware` is a `Handler -> Handler`, so
`Use` composes the supplied middlewares into the chain applied at dispatch time. The
important guarantee is "wraps every handler exactly once": you apply the middleware
chain when the handler runs, and the test asserts a counting middleware increments
once per dispatch, not once per registered handler and not twice. Applying the chain
at dispatch (rather than mutating the stored handler at `Register`) means middleware
added after registration still covers earlier handlers, and a handler is never
double-wrapped.

Concurrency is real here: a dispatcher is shared across worker goroutines. The
`RWMutex` lets many `Dispatch` calls read the map at once while `Register`/`Use` take
the write lock. The `-race` test dispatches concurrently to prove the map access is
safe.

Create `dispatcher.go`:

```go
package dispatcher

import (
	"context"
	"errors"
	"fmt"
	"sync"
)

// Message is a typed payload routed by Type.
type Message struct {
	Type string
	Body string
}

// Handler processes a message.
type Handler func(ctx context.Context, msg Message) error

// Middleware decorates a Handler.
type Middleware func(Handler) Handler

// ErrNoHandler is returned when no handler is registered for a message type.
var ErrNoHandler = errors.New("no handler registered")

// Dispatcher routes messages to registered handlers, optionally wrapping every
// dispatch with middleware. It is safe for concurrent use.
type Dispatcher struct {
	mu       sync.RWMutex
	handlers map[string]Handler
	mws      []Middleware
}

// New returns an empty dispatcher.
func New() *Dispatcher {
	return &Dispatcher{handlers: make(map[string]Handler)}
}

// Register stores h for a message type. Registering the same name twice keeps
// the last handler (last-wins).
func (d *Dispatcher) Register(name string, h Handler) {
	d.mu.Lock()
	defer d.mu.Unlock()
	d.handlers[name] = h
}

// Use appends middleware applied to every dispatched handler, outermost-first in
// call order.
func (d *Dispatcher) Use(mw ...Middleware) {
	d.mu.Lock()
	defer d.mu.Unlock()
	d.mws = append(d.mws, mw...)
}

// Dispatch routes msg to its handler, wrapping it with the registered
// middleware. An unknown type returns ErrNoHandler wrapped with the type.
func (d *Dispatcher) Dispatch(ctx context.Context, msg Message) error {
	d.mu.RLock()
	h, ok := d.handlers[msg.Type]
	mws := d.mws
	d.mu.RUnlock()

	if !ok {
		return fmt.Errorf("%q: %w", msg.Type, ErrNoHandler)
	}
	for i := len(mws) - 1; i >= 0; i-- {
		h = mws[i](h)
	}
	return h(ctx, msg)
}
```

### The runnable demo

The demo registers handlers for two message types, adds a logging middleware, and
dispatches one of each plus an unknown type to show the `ErrNoHandler` path.

Create `cmd/demo/main.go`:

```go
package main

import (
	"context"
	"errors"
	"fmt"

	"example.com/dispatcher"
)

func main() {
	d := dispatcher.New()

	d.Register("order.created", func(ctx context.Context, m dispatcher.Message) error {
		fmt.Printf("handling order.created: %s\n", m.Body)
		return nil
	})
	d.Register("user.deleted", func(ctx context.Context, m dispatcher.Message) error {
		fmt.Printf("handling user.deleted: %s\n", m.Body)
		return nil
	})

	d.Use(func(next dispatcher.Handler) dispatcher.Handler {
		return func(ctx context.Context, m dispatcher.Message) error {
			fmt.Printf("dispatch type=%s\n", m.Type)
			return next(ctx, m)
		}
	})

	ctx := context.Background()
	_ = d.Dispatch(ctx, dispatcher.Message{Type: "order.created", Body: "o-42"})
	_ = d.Dispatch(ctx, dispatcher.Message{Type: "user.deleted", Body: "u-7"})

	err := d.Dispatch(ctx, dispatcher.Message{Type: "unknown.type"})
	fmt.Printf("unknown dispatch: is ErrNoHandler = %v\n", errors.Is(err, dispatcher.ErrNoHandler))
}
```

Run it:

```bash
go run ./cmd/demo
```

Expected output:

```
dispatch type=order.created
handling order.created: o-42
dispatch type=user.deleted
handling user.deleted: u-7
unknown dispatch: is ErrNoHandler = true
```

Each dispatch prints the middleware line then the handler line, so you can see the
middleware wrapping every handler. The unknown type never reaches a handler; it
returns `ErrNoHandler`, which `errors.Is` confirms.

### Tests

The routing test registers two handlers, each flipping a distinct flag, and asserts
dispatching one runs only that handler. The unknown-type test asserts `ErrNoHandler`
via `errors.Is` and that no handler ran. The last-wins test registers the same name
twice and asserts the second handler is the one that runs. The middleware test uses
an atomic counter to prove `Use` wraps each dispatch exactly once. The concurrency
test dispatches from many goroutines under `-race`.

Create `dispatcher_test.go`:

```go
package dispatcher

import (
	"context"
	"errors"
	"sync"
	"sync/atomic"
	"testing"
)

func TestDispatchRoutesToCorrectHandler(t *testing.T) {
	t.Parallel()

	d := New()
	var ranA, ranB bool
	d.Register("a", func(ctx context.Context, m Message) error { ranA = true; return nil })
	d.Register("b", func(ctx context.Context, m Message) error { ranB = true; return nil })

	if err := d.Dispatch(context.Background(), Message{Type: "a"}); err != nil {
		t.Fatal(err)
	}
	if !ranA || ranB {
		t.Fatalf("ranA=%v ranB=%v, want true,false", ranA, ranB)
	}
}

func TestDispatchUnknownReturnsErrNoHandler(t *testing.T) {
	t.Parallel()

	d := New()
	var ran bool
	d.Register("known", func(ctx context.Context, m Message) error { ran = true; return nil })

	err := d.Dispatch(context.Background(), Message{Type: "missing"})
	if !errors.Is(err, ErrNoHandler) {
		t.Fatalf("err = %v, want ErrNoHandler", err)
	}
	if ran {
		t.Fatal("a handler ran for an unknown type")
	}
}

func TestRegisterLastWins(t *testing.T) {
	t.Parallel()

	d := New()
	var which string
	d.Register("x", func(ctx context.Context, m Message) error { which = "first"; return nil })
	d.Register("x", func(ctx context.Context, m Message) error { which = "second"; return nil })

	if err := d.Dispatch(context.Background(), Message{Type: "x"}); err != nil {
		t.Fatal(err)
	}
	if which != "second" {
		t.Fatalf("which = %q, want second (last-wins)", which)
	}
}

func TestUseWrapsEveryHandlerOnce(t *testing.T) {
	t.Parallel()

	d := New()
	d.Register("a", func(ctx context.Context, m Message) error { return nil })
	d.Register("b", func(ctx context.Context, m Message) error { return nil })

	var wraps atomic.Int64
	d.Use(func(next Handler) Handler {
		return func(ctx context.Context, m Message) error {
			wraps.Add(1)
			return next(ctx, m)
		}
	})

	_ = d.Dispatch(context.Background(), Message{Type: "a"})
	_ = d.Dispatch(context.Background(), Message{Type: "b"})

	if w := wraps.Load(); w != 2 {
		t.Fatalf("middleware ran %d times, want 2 (once per dispatch)", w)
	}
}

func TestConcurrentDispatch(t *testing.T) {
	t.Parallel()

	d := New()
	var count atomic.Int64
	d.Register("hit", func(ctx context.Context, m Message) error {
		count.Add(1)
		return nil
	})

	var wg sync.WaitGroup
	for range 100 {
		wg.Add(1)
		go func() {
			defer wg.Done()
			_ = d.Dispatch(context.Background(), Message{Type: "hit"})
		}()
	}
	wg.Wait()

	if c := count.Load(); c != 100 {
		t.Fatalf("handler ran %d times, want 100", c)
	}
}
```

## Review

The dispatcher is correct when a message routes to exactly the handler registered
for its type, an unknown type returns `ErrNoHandler` (matched by `errors.Is`) without
running anything, and re-registering a name replaces the handler. Applying the
middleware chain at dispatch time — not at registration — is what makes "wrap every
handler exactly once" true and lets middleware added later cover earlier handlers;
the counting middleware test pins it at one increment per dispatch. Wrap the
unknown-type error with `%w` so callers can both classify it with `errors.Is` and
read the offending type from the message. The `RWMutex` lets concurrent dispatches
read the map while registration takes the write lock; run `go test -race` to confirm.

## Resources

- [context package](https://pkg.go.dev/context) — the `Handler` receives a cancellable context.
- [errors package](https://pkg.go.dev/errors) — `Is` and `%w`-wrapped sentinels like `ErrNoHandler`.
- [sync package](https://pkg.go.dev/sync) — `RWMutex` for a concurrently-read registry.

---

Back to [07-generic-collection-ops.md](07-generic-collection-ops.md) | Next: [09-backoff-with-jitter.md](09-backoff-with-jitter.md)
