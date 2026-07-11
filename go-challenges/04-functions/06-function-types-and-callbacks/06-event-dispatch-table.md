# Exercise 6: Event Router With a Callback Dispatch Table

A `map[string]HandlerFunc` turns a sprawling `switch kind {...}` into data: handlers
register themselves, dispatch is a map lookup, and a new event type needs no edit to a
central switch. This module builds that router with the production decisions made
explicit — nil rejection, unknown-kind fallback, multiple subscribers per kind with
aggregated errors, and documented concurrency safety.

This module is fully self-contained: its own `go mod init`, all code inline, its own
demo and tests.

## What you'll build

```text
eventbus/                   independent module: example.com/eventbus
  go.mod                    go 1.26
  eventbus.go               Event, HandlerFunc, Router: Register, Default, Dispatch
  cmd/
    demo/
      main.go               runnable demo: register, dispatch, unknown, multi
  eventbus_test.go          routing, unknown fallback, multi+join, nil-reject tests
```

Files: `eventbus.go`, `cmd/demo/main.go`, `eventbus_test.go`.
Implement: `type HandlerFunc func(ctx, Event) error`, `Router` with `Register(kind, h)`, `SetDefault(h)`, and `Dispatch(ctx, Event)`; multiple handlers per kind, errors joined with `errors.Join`, nil handlers rejected, unknown kinds routed to the default or a sentinel.
Test: dispatch routes to the right handler; unknown kind hits the default or returns the sentinel via `errors.Is`; two handlers for one kind both run and their errors join; a nil registration is rejected; concurrent dispatch is race-free.
Verify: `go test -count=1 -race ./...`

Set up the module:

```bash
mkdir -p ~/go-exercises/eventbus/cmd/demo
cd ~/go-exercises/eventbus
go mod init example.com/eventbus
```

### The table versus the switch, and the decisions it forces

A giant `switch e.Kind { case "order.created": ...; case "user.signup": ... }` couples
every handler to one function that grows without bound and that a plugin cannot extend.
The dispatch table replaces it: `Register("order.created", h)` appends to a
`map[string][]HandlerFunc`, and `Dispatch` looks the kind up and runs the handlers.
Registration is decoupled from invocation, and adding a kind touches no existing code.

The table forces decisions the switch hid:

- Nil handlers. A `nil` `HandlerFunc` in the map is a panic waiting to happen at
  dispatch. `Register` rejects nil at registration with a wrapped `ErrNilHandler`, so
  the failure is immediate and local, not a nil-call somewhere deep in production.
- Unknown kinds. A kind with no handler is not silently dropped. This router routes it
  to a default handler if one is set (a dead-letter sink, a metric), otherwise returns
  `ErrNoHandler` so the caller learns the event was unroutable.
- Multiple subscribers. Several handlers may care about one kind. The map value is a
  *slice*, `Dispatch` runs all of them, and it collects their errors with
  `errors.Join` rather than stopping at the first — so one failing subscriber does not
  hide another, and `errors.Is` at the call site matches any of the joined sentinels.
- Concurrency. This router is documented as safe for concurrent `Dispatch` after
  registration completes; a `sync.RWMutex` guards the map so `Dispatch` takes a read
  lock and `Register` a write lock. The test exercises concurrent dispatch under
  `-race`.

Create `eventbus.go`:

```go
package eventbus

import (
	"context"
	"errors"
	"fmt"
	"sync"
)

// Event is a typed message. Kind selects the handler(s); Payload is opaque.
type Event struct {
	Kind    string
	Payload any
}

// HandlerFunc processes one event. It is ctx-first for cancellation.
type HandlerFunc func(ctx context.Context, e Event) error

var (
	// ErrNilHandler is returned by Register for a nil handler.
	ErrNilHandler = errors.New("nil handler")
	// ErrNoHandler is returned by Dispatch for an unknown kind with no default.
	ErrNoHandler = errors.New("no handler for event kind")
)

// Router maps an event kind to one or more handlers. It is safe for concurrent
// Dispatch once registration is complete.
type Router struct {
	mu       sync.RWMutex
	handlers map[string][]HandlerFunc
	fallback HandlerFunc
}

func NewRouter() *Router {
	return &Router{handlers: make(map[string][]HandlerFunc)}
}

// Register appends a handler for kind. It rejects a nil handler.
func (r *Router) Register(kind string, h HandlerFunc) error {
	if h == nil {
		return fmt.Errorf("register %q: %w", kind, ErrNilHandler)
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	r.handlers[kind] = append(r.handlers[kind], h)
	return nil
}

// SetDefault installs the handler used for kinds with no registered handler.
func (r *Router) SetDefault(h HandlerFunc) error {
	if h == nil {
		return fmt.Errorf("set default: %w", ErrNilHandler)
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	r.fallback = h
	return nil
}

// Dispatch runs every handler registered for e.Kind, joining their errors. An
// unknown kind goes to the default handler, or returns ErrNoHandler if none.
func (r *Router) Dispatch(ctx context.Context, e Event) error {
	r.mu.RLock()
	hs := r.handlers[e.Kind]
	fallback := r.fallback
	r.mu.RUnlock()

	if len(hs) == 0 {
		if fallback != nil {
			return fallback(ctx, e)
		}
		return fmt.Errorf("%w: %q", ErrNoHandler, e.Kind)
	}

	var errs []error
	for _, h := range hs {
		if err := h(ctx, e); err != nil {
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

	"example.com/eventbus"
)

func main() {
	r := eventbus.NewRouter()
	_ = r.Register("order.created", func(ctx context.Context, e eventbus.Event) error {
		fmt.Printf("charge card for %v\n", e.Payload)
		return nil
	})
	_ = r.Register("order.created", func(ctx context.Context, e eventbus.Event) error {
		fmt.Printf("send receipt for %v\n", e.Payload)
		return nil
	})
	_ = r.SetDefault(func(ctx context.Context, e eventbus.Event) error {
		fmt.Printf("dead-letter: %s\n", e.Kind)
		return nil
	})

	ctx := context.Background()
	_ = r.Dispatch(ctx, eventbus.Event{Kind: "order.created", Payload: "order-42"})
	_ = r.Dispatch(ctx, eventbus.Event{Kind: "unknown.kind", Payload: nil})
}
```

Run it:

```bash
go run ./cmd/demo
```

Expected output:

```
charge card for order-42
send receipt for order-42
dead-letter: unknown.kind
```

### Tests

Create `eventbus_test.go`:

```go
package eventbus

import (
	"context"
	"errors"
	"fmt"
	"sync"
	"testing"
)

func TestDispatchRoutes(t *testing.T) {
	t.Parallel()
	r := NewRouter()
	var got string
	if err := r.Register("a", func(ctx context.Context, e Event) error {
		got = e.Kind
		return nil
	}); err != nil {
		t.Fatalf("Register: %v", err)
	}
	if err := r.Dispatch(context.Background(), Event{Kind: "a"}); err != nil {
		t.Fatalf("Dispatch: %v", err)
	}
	if got != "a" {
		t.Fatalf("handler saw kind %q, want a", got)
	}
}

func TestUnknownKind(t *testing.T) {
	t.Parallel()
	r := NewRouter()
	err := r.Dispatch(context.Background(), Event{Kind: "missing"})
	if !errors.Is(err, ErrNoHandler) {
		t.Fatalf("err = %v, want ErrNoHandler", err)
	}
}

func TestDefaultHandlerCatchesUnknown(t *testing.T) {
	t.Parallel()
	r := NewRouter()
	caught := ""
	if err := r.SetDefault(func(ctx context.Context, e Event) error {
		caught = e.Kind
		return nil
	}); err != nil {
		t.Fatalf("SetDefault: %v", err)
	}
	if err := r.Dispatch(context.Background(), Event{Kind: "missing"}); err != nil {
		t.Fatalf("Dispatch: %v", err)
	}
	if caught != "missing" {
		t.Fatalf("default caught %q, want missing", caught)
	}
}

func TestMultipleHandlersJoinErrors(t *testing.T) {
	t.Parallel()
	errA := errors.New("handler A failed")
	errB := errors.New("handler B failed")
	r := NewRouter()
	ranA, ranB := false, false
	_ = r.Register("x", func(ctx context.Context, e Event) error { ranA = true; return errA })
	_ = r.Register("x", func(ctx context.Context, e Event) error { ranB = true; return errB })

	err := r.Dispatch(context.Background(), Event{Kind: "x"})
	if !ranA || !ranB {
		t.Fatalf("both handlers must run: ranA=%v ranB=%v", ranA, ranB)
	}
	if !errors.Is(err, errA) || !errors.Is(err, errB) {
		t.Fatalf("joined error missing a branch: %v", err)
	}
}

func TestNilHandlerRejected(t *testing.T) {
	t.Parallel()
	r := NewRouter()
	if err := r.Register("x", nil); !errors.Is(err, ErrNilHandler) {
		t.Fatalf("Register(nil) = %v, want ErrNilHandler", err)
	}
	if err := r.SetDefault(nil); !errors.Is(err, ErrNilHandler) {
		t.Fatalf("SetDefault(nil) = %v, want ErrNilHandler", err)
	}
}

func TestConcurrentDispatch(t *testing.T) {
	t.Parallel()
	r := NewRouter()
	var mu sync.Mutex
	count := 0
	_ = r.Register("tick", func(ctx context.Context, e Event) error {
		mu.Lock()
		count++
		mu.Unlock()
		return nil
	})

	var wg sync.WaitGroup
	for range 100 {
		wg.Add(1)
		go func() {
			defer wg.Done()
			_ = r.Dispatch(context.Background(), Event{Kind: "tick"})
		}()
	}
	wg.Wait()
	if count != 100 {
		t.Fatalf("count = %d, want 100", count)
	}
}

func ExampleRouter_Dispatch() {
	r := NewRouter()
	_ = r.Register("ping", func(ctx context.Context, e Event) error { return nil })
	err := r.Dispatch(context.Background(), Event{Kind: "ping"})
	fmt.Println(err == nil)
	// Output: true
}
```

## Review

The router is correct when dispatch is a pure map lookup plus a fan-out: a registered
kind runs exactly the handlers registered for it (in registration order), an unknown
kind falls back to the default or returns a `%w`-wrapped `ErrNoHandler` that
`errors.Is` matches, and multiple handlers for one kind all run with their failures
joined by `errors.Join` so no subscriber's error is lost. Nil is rejected at
registration, not at dispatch — `TestNilHandlerRejected` proves both `Register` and
`SetDefault` refuse it. The concurrency claim is backed by the `RWMutex` and exercised
by 100 concurrent dispatches under `-race`; note the handler itself still needs its own
lock around shared mutable state, because the router only guards its own map, not your
handler's side effects. Contrast this with the type switch: every property here — plugin
registration, multi-subscriber, per-kind errors — is awkward or impossible in a central
switch.

## Resources

- [errors.Join](https://pkg.go.dev/errors#Join)
- [sync.RWMutex](https://pkg.go.dev/sync#RWMutex)
- [maps package](https://pkg.go.dev/maps)
- [Go blog: Working with Errors in Go 1.13](https://go.dev/blog/go1.13-errors)

---

Back to [00-concepts.md](00-concepts.md) | Previous: [05-retry-with-operation-callback.md](05-retry-with-operation-callback.md) | Next: [07-validation-rule-pipeline.md](07-validation-rule-pipeline.md)
