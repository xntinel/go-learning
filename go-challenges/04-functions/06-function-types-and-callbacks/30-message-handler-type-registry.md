# Exercise 30: Message Handler Type Registry with Named Function Types

**Nivel: Avanzado** — validacion normal (tabla de casos, incluye concurrencia).

A message bus wants to hold handlers for many different concrete message
types in one map, but Go has no `map[string]Handler[T]` for varying `T`.
This module solves that with a generic `Handler[T Message]` named function
type registered through a package-level generic function that wraps it
into a type-erased closure — so callers write ordinary, fully-typed
handlers, and the registry stores and dispatches them uniformly, catching
a mismatched concrete type at dispatch time instead of with a panic.

## What you'll build

```text
registry/                     independent module: example.com/message-handler-type-registry
  go.mod                       go 1.24
  registry.go                  type Message, type Handler[T], func Register[T], type Registry: Dispatch
  cmd/
    demo/
      main.go                    runnable demo: two distinct message types dispatched, one unregistered type
  registry_test.go               typed dispatch, duplicate rejection, unknown type, type-mismatch via a decoy type, concurrency (-race)
```

Files: `registry.go`, `cmd/demo/main.go`, `registry_test.go`.
Implement: `type Message interface { Type() string }`, `type Handler[T Message] func(ctx, T) error`, `func Register[T Message](r *Registry, msgType string, h Handler[T]) error`, and `Registry` with `Dispatch(ctx, Message) error`; `Register` wraps `h` into a type-erased closure keyed by `msgType`, rejects a nil handler and a duplicate `msgType`, and `Dispatch` looks up by `msg.Type()` then type-asserts the message back to `T` inside the wrapper.
Test: dispatch routes a message to the handler registered for its type; registering the same `msgType` twice is rejected; dispatching an unregistered type returns `ErrNoHandler`; a message whose `Type()` string matches a registration but whose concrete Go type differs returns `ErrTypeMismatch`; two handlers for distinct message types coexist and both run; concurrent registration of new types alongside concurrent dispatch of an already-registered type is race-free.
Verify: `go test -count=1 -race ./...`

Set up the module:

```bash
mkdir -p ~/go-exercises/message-handler-type-registry/cmd/demo
cd ~/go-exercises/message-handler-type-registry
go mod init example.com/message-handler-type-registry
go mod edit -go=1.24
```

### Why `Register` is a function, not a method, and what it erases

Go methods cannot introduce new type parameters beyond the receiver's own
— `func (r *Registry) Register[T Message](...)` does not compile. So
`Register[T Message](r *Registry, msgType string, h Handler[T]) error` is
a package-level generic function that takes the registry as its first
argument instead. Its job is to turn a fully-typed `Handler[T]` — the kind
of function a caller actually wants to write, say `func(ctx
context.Context, msg OrderCreated) error` — into an `erasedHandler`, a
plain `func(ctx context.Context, msg Message) error` that the registry's
map can hold regardless of what `T` was. The closure it builds carries `T`
in its type-asserted body: `msg.(T)`. That assertion is the one place the
type erasure could go wrong, and it is also why keying purely by
`msg.Type()` string is not quite enough on its own — two unrelated
concrete types could both return `"order.created"` from `Type()` while
being registered against different `T`s (or, as the tests show, only one
of them ever gets registered at all). `Dispatch` finds the handler by
string, but the handler itself is the last line of defense: if the
message hasn't got the concrete type `T` the closure captured, the
assertion fails and `Dispatch` returns `ErrTypeMismatch` instead of a
panic.

Create `registry.go`:

```go
// Package registry dispatches messages to type-specific handlers. Each
// handler is registered as a generic Handler[T] named function type, and
// the registry stores a type-erased wrapper so heterogeneous handlers can
// share one map keyed by message type name.
package registry

import (
	"context"
	"errors"
	"fmt"
	"sync"
)

// Message is anything routable through the registry; Type identifies which
// handler should receive it.
type Message interface {
	Type() string
}

// Handler processes one concrete Message type T.
type Handler[T Message] func(ctx context.Context, msg T) error

// erasedHandler is what the registry actually stores: a Handler[T] with T
// hidden behind a runtime type assertion.
type erasedHandler func(ctx context.Context, msg Message) error

var (
	// ErrAlreadyRegistered is returned by Register for a msgType that
	// already has a handler.
	ErrAlreadyRegistered = errors.New("handler already registered for type")
	// ErrNoHandler is returned by Dispatch for an unregistered msgType.
	ErrNoHandler = errors.New("no handler registered for type")
	// ErrTypeMismatch is returned when a message's Type() string matches a
	// registration whose concrete Go type does not match the message's
	// actual concrete type.
	ErrTypeMismatch = errors.New("message concrete type does not match registered handler type")
)

// Registry maps a message type name to exactly one type-erased handler.
type Registry struct {
	mu       sync.RWMutex
	handlers map[string]erasedHandler
}

// New returns an empty Registry.
func New() *Registry {
	return &Registry{handlers: make(map[string]erasedHandler)}
}

// Register associates msgType with h. It is a package-level generic
// function (not a method) because Go methods cannot introduce their own
// type parameters. Registering twice for the same msgType is rejected.
func Register[T Message](r *Registry, msgType string, h Handler[T]) error {
	if h == nil {
		return fmt.Errorf("register %q: nil handler", msgType)
	}
	wrapped := erasedHandler(func(ctx context.Context, msg Message) error {
		typed, ok := msg.(T)
		if !ok {
			return fmt.Errorf("%w: registered for %q", ErrTypeMismatch, msgType)
		}
		return h(ctx, typed)
	})

	r.mu.Lock()
	defer r.mu.Unlock()
	if _, exists := r.handlers[msgType]; exists {
		return fmt.Errorf("%w: %q", ErrAlreadyRegistered, msgType)
	}
	r.handlers[msgType] = wrapped
	return nil
}

// Dispatch routes msg to the handler registered for msg.Type().
func (r *Registry) Dispatch(ctx context.Context, msg Message) error {
	r.mu.RLock()
	h, ok := r.handlers[msg.Type()]
	r.mu.RUnlock()
	if !ok {
		return fmt.Errorf("%w: %q", ErrNoHandler, msg.Type())
	}
	return h(ctx, msg)
}
```

### The runnable demo

Create `cmd/demo/main.go`:

```go
package main

import (
	"context"
	"fmt"

	"example.com/message-handler-type-registry"
)

type OrderCreated struct {
	OrderID string
}

func (OrderCreated) Type() string { return "order.created" }

type UserSignup struct {
	Email string
}

func (UserSignup) Type() string { return "user.signup" }

type PasswordReset struct {
	Email string
}

func (PasswordReset) Type() string { return "password.reset" }

func main() {
	r := registry.New()

	_ = registry.Register(r, "order.created", registry.Handler[OrderCreated](
		func(ctx context.Context, msg OrderCreated) error {
			fmt.Printf("order handler: charge order %s\n", msg.OrderID)
			return nil
		}))

	_ = registry.Register(r, "user.signup", registry.Handler[UserSignup](
		func(ctx context.Context, msg UserSignup) error {
			fmt.Printf("signup handler: welcome email to %s\n", msg.Email)
			return nil
		}))

	ctx := context.Background()
	_ = r.Dispatch(ctx, OrderCreated{OrderID: "order-1"})
	_ = r.Dispatch(ctx, UserSignup{Email: "a@example.com"})

	err := r.Dispatch(ctx, PasswordReset{Email: "a@example.com"})
	fmt.Println("unregistered dispatch error:", err != nil)
}
```

Run it:

```bash
go run ./cmd/demo
```

Expected output:

```
order handler: charge order order-1
signup handler: welcome email to a@example.com
unregistered dispatch error: true
```

### Tests

Create `registry_test.go`:

```go
package registry

import (
	"context"
	"errors"
	"fmt"
	"sync"
	"testing"
)

type orderCreated struct{ id string }

func (orderCreated) Type() string { return "order.created" }

type userSignup struct{ email string }

func (userSignup) Type() string { return "user.signup" }

// decoyOrder shares the "order.created" Type() string with orderCreated
// but is a distinct concrete Go type, to exercise the type-assertion
// failure branch inside a registered handler.
type decoyOrder struct{ note string }

func (decoyOrder) Type() string { return "order.created" }

func TestRegisterAndDispatchRoutesToTypedHandler(t *testing.T) {
	t.Parallel()
	r := New()
	var got string
	err := Register(r, "order.created", Handler[orderCreated](func(ctx context.Context, msg orderCreated) error {
		got = msg.id
		return nil
	}))
	if err != nil {
		t.Fatalf("Register: %v", err)
	}

	if err := r.Dispatch(context.Background(), orderCreated{id: "o-1"}); err != nil {
		t.Fatalf("Dispatch: %v", err)
	}
	if got != "o-1" {
		t.Fatalf("handler saw id %q, want o-1", got)
	}
}

func TestDuplicateRegistrationRejected(t *testing.T) {
	t.Parallel()
	r := New()
	_ = Register(r, "order.created", Handler[orderCreated](func(ctx context.Context, msg orderCreated) error { return nil }))
	err := Register(r, "order.created", Handler[orderCreated](func(ctx context.Context, msg orderCreated) error { return nil }))
	if !errors.Is(err, ErrAlreadyRegistered) {
		t.Fatalf("err = %v, want ErrAlreadyRegistered", err)
	}
}

func TestDispatchUnknownTypeErrors(t *testing.T) {
	t.Parallel()
	r := New()
	err := r.Dispatch(context.Background(), userSignup{email: "a@example.com"})
	if !errors.Is(err, ErrNoHandler) {
		t.Fatalf("err = %v, want ErrNoHandler", err)
	}
}

func TestDispatchTypeMismatchBetweenSameTypeStringDifferentConcreteType(t *testing.T) {
	t.Parallel()
	r := New()
	_ = Register(r, "order.created", Handler[orderCreated](func(ctx context.Context, msg orderCreated) error { return nil }))

	// decoyOrder.Type() also returns "order.created", so Dispatch finds
	// the handler via the map lookup, but the handler was registered for
	// the concrete type orderCreated, not decoyOrder.
	err := r.Dispatch(context.Background(), decoyOrder{note: "not an orderCreated"})
	if !errors.Is(err, ErrTypeMismatch) {
		t.Fatalf("err = %v, want ErrTypeMismatch", err)
	}
}

func TestMultipleDistinctHandlerTypesCoexist(t *testing.T) {
	t.Parallel()
	r := New()
	var orderSeen, signupSeen bool
	_ = Register(r, "order.created", Handler[orderCreated](func(ctx context.Context, msg orderCreated) error {
		orderSeen = true
		return nil
	}))
	_ = Register(r, "user.signup", Handler[userSignup](func(ctx context.Context, msg userSignup) error {
		signupSeen = true
		return nil
	}))

	_ = r.Dispatch(context.Background(), orderCreated{id: "o-1"})
	_ = r.Dispatch(context.Background(), userSignup{email: "a@example.com"})

	if !orderSeen || !signupSeen {
		t.Fatalf("orderSeen=%v signupSeen=%v, want both true", orderSeen, signupSeen)
	}
}

func TestConcurrentRegisterAndDispatchIsRaceFree(t *testing.T) {
	t.Parallel()
	r := New()
	var mu sync.Mutex
	count := 0
	_ = Register(r, "order.created", Handler[orderCreated](func(ctx context.Context, msg orderCreated) error {
		mu.Lock()
		count++
		mu.Unlock()
		return nil
	}))

	var wg sync.WaitGroup
	for i := range 20 {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			// Concurrent registrations for distinct types never collide
			// with each other or with concurrent dispatch of an
			// already-registered type.
			_ = Register(r, fmt.Sprintf("noop.%d", i), Handler[userSignup](func(ctx context.Context, msg userSignup) error {
				return nil
			}))
			_ = r.Dispatch(context.Background(), orderCreated{id: "concurrent"})
		}(i)
	}
	wg.Wait()

	if count != 20 {
		t.Fatalf("count = %d, want 20", count)
	}
}
```

## Review

The registry is correct when two independent guarantees hold: dispatch
finds the right handler by type name, and the handler it finds only ever
runs against the concrete type it was written for.
`TestRegisterAndDispatchRoutesToTypedHandler` and
`TestMultipleDistinctHandlerTypesCoexist` cover the first — a generic
`Handler[T]` stored behind type erasure still gets its fully-typed `T`
back at the call site, for as many distinct `T`s as get registered.
`TestDispatchTypeMismatchBetweenSameTypeStringDifferentConcreteType` is
the test that justifies the design's extra type assertion instead of
trusting the string key alone: `decoyOrder` proves that two concrete
types can legitimately share a `Type()` string while never having been
registered together, and the registry has to notice that at dispatch time
rather than silently misrouting or panicking. `TestDuplicateRegistrationRejected`
keeps the map itself honest — one `msgType`, one handler, no silent
overwrite. The concurrency test does not touch the type system at all; it
only proves the `RWMutex` correctly separates concurrent `Register` calls
(which mutate the map) from concurrent `Dispatch` calls (which only read
it), the same shape as any other shared registry.

## Resources

- [Go Specification: Type parameter declarations](https://go.dev/ref/spec#Type_parameter_declarations)
- [Go blog: An Introduction to Generics](https://go.dev/blog/intro-generics)
- [sync.RWMutex](https://pkg.go.dev/sync#RWMutex)

---

Back to [00-concepts.md](00-concepts.md) | Prev: [29-lru-eviction-policy-selector.md](29-lru-eviction-policy-selector.md) | Next: [31-payment-processor-adapter.md](31-payment-processor-adapter.md)
