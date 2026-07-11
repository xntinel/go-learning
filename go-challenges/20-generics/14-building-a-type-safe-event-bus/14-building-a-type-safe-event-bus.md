# 14. Building a Type-Safe Event Bus

Go does not support heterogeneous generic collections directly, so a type-safe event bus needs a careful boundary: public generic functions provide typed subscribe and publish calls, while the bus stores erased handlers internally.

## Concepts

### Public Generic Functions Preserve Type Safety

`Subscribe[T](bus, handler)` accepts `func(T)`. A subscriber for `UserCreated` cannot accidentally receive an `OrderPlaced` value through the public API.

### Internal Type Erasure Is A Containment Technique

The bus stores handlers by `reflect.Type` and wraps typed handlers behind `func(any)`. The unsafe-looking part is private and small; callers never pass or receive `any`.

### Lifecycle Errors Need A Contract

Publishing after shutdown is a runtime state error. The package returns a wrapped sentinel error so callers can distinguish a closed bus from handler failures.

## Exercises

Set up the module:

```bash
mkdir -p ~/go-exercises/eventbus/cmd/demo
cd ~/go-exercises/eventbus
go mod init example.com/verify
```

### Exercise 1: Build The Event Bus

Create `bus.go`:

```go
package eventbus

import (
	"errors"
	"fmt"
	"reflect"
	"sync"
)

var ErrClosed = errors.New("event bus is closed")

type Bus struct {
	mu       sync.RWMutex
	closed   bool
	nextID   int
	handlers map[reflect.Type]map[int]func(any)
}

func New() *Bus {
	return &Bus{handlers: make(map[reflect.Type]map[int]func(any))}
}

func Subscribe[T any](b *Bus, handler func(T)) func() {
	b.mu.Lock()
	defer b.mu.Unlock()
	typ := reflect.TypeFor[T]()
	b.nextID++
	id := b.nextID
	if b.handlers[typ] == nil {
		b.handlers[typ] = make(map[int]func(any))
	}
	b.handlers[typ][id] = func(value any) {
		defer func() { _ = recover() }()
		handler(value.(T))
	}
	return func() {
		b.mu.Lock()
		defer b.mu.Unlock()
		delete(b.handlers[typ], id)
	}
}

func Publish[T any](b *Bus, event T) error {
	b.mu.RLock()
	if b.closed {
		b.mu.RUnlock()
		return fmt.Errorf("publish: %w", ErrClosed)
	}
	typ := reflect.TypeFor[T]()
	items := make([]func(any), 0, len(b.handlers[typ]))
	for _, handler := range b.handlers[typ] {
		items = append(items, handler)
	}
	b.mu.RUnlock()
	for _, handler := range items {
		handler(event)
	}
	return nil
}

func (b *Bus) Close() {
	b.mu.Lock()
	defer b.mu.Unlock()
	b.closed = true
	b.handlers = make(map[reflect.Type]map[int]func(any))
}
```

### Exercise 2: Add Tests And An Example

Create `bus_test.go`:

```go
package eventbus

import (
	"errors"
	"fmt"
	"testing"
)

type UserCreated struct {
	ID string
}

type OrderPlaced struct {
	ID string
}

func TestPublishRoutesByType(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name       string
		sendUser   bool
		wantUsers  int
		wantOrders int
	}{
		{name: "user", sendUser: true, wantUsers: 1},
		{name: "order", sendUser: false, wantOrders: 1},
	}

	for _, tt := range tests {
		tt := tt
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			bus := New()
			users := 0
			orders := 0
			Subscribe(bus, func(UserCreated) { users++ })
			Subscribe(bus, func(OrderPlaced) { orders++ })
			if tt.sendUser {
				_ = Publish(bus, UserCreated{ID: "u1"})
			} else {
				_ = Publish(bus, OrderPlaced{ID: "o1"})
			}
			if users != tt.wantUsers || orders != tt.wantOrders {
				t.Fatalf("users=%d orders=%d", users, orders)
			}
		})
	}
}

func TestUnsubscribeAndPanicRecovery(t *testing.T) {
	t.Parallel()

	bus := New()
	called := 0
	cancel := Subscribe(bus, func(UserCreated) { called++ })
	Subscribe(bus, func(UserCreated) { panic("handler failed") })
	cancel()
	if err := Publish(bus, UserCreated{ID: "u1"}); err != nil {
		t.Fatal(err)
	}
	if called != 0 {
		t.Fatalf("called = %d, want 0", called)
	}
}

func TestPublishAfterClose(t *testing.T) {
	t.Parallel()

	bus := New()
	bus.Close()
	if err := Publish(bus, UserCreated{ID: "u1"}); !errors.Is(err, ErrClosed) {
		t.Fatalf("err = %v, want ErrClosed", err)
	}
}

func ExamplePublish() {
	bus := New()
	Subscribe(bus, func(event UserCreated) { fmt.Println(event.ID) })
	_ = Publish(bus, UserCreated{ID: "u1"})
	// Output: u1
}
```

### Exercise 3: Add A Runnable Demo

Create `cmd/demo/main.go`:

```go
package main

import (
	"fmt"
	"log"

	eventbus "example.com/verify"
)

type PaymentProcessed struct {
	ID string
}

func main() {
	bus := eventbus.New()
	eventbus.Subscribe(bus, func(event PaymentProcessed) { fmt.Println(event.ID) })
	if err := eventbus.Publish(bus, PaymentProcessed{ID: "p1"}); err != nil {
		log.Fatal(err)
	}
}
```

## Common Mistakes

### Trying To Write Generic Methods

Wrong: `func (b *Bus) Subscribe[T any](handler func(T))`.

Fix: Go methods cannot have their own type parameters; use `Subscribe[T](b, handler)` as a generic function.

### Letting Internal `any` Leak Into The API

Wrong: exposing `func(any)` handlers to callers.

Fix: keep erased handlers private and expose typed `func(T)` subscriptions.

### Holding The Lock While Running Handlers

Wrong: call handlers while the bus mutex is locked.

Fix: copy the handler list under the lock, release it, then invoke handlers.

## Verification

Run this from `~/go-exercises/eventbus`:

```bash
test -z "$(gofmt -l .)"
go vet ./...
go build ./...
go test -count=1 -race ./...
```

Add one more test that subscribes two handlers for the same event type and verifies both are called once.

## Summary

- Generic functions can provide a type-safe event API.
- A small private type-erased layer can manage heterogeneous subscriptions.
- Publishing should copy handlers before invoking them.
- Closed-bus behavior should be tested with a sentinel error.

## What's Next

Next: continue with the next Go chapter or revisit the generics chapter by combining repositories, iterators, and middleware in one package.

## Resources

- [reflect.TypeFor](https://pkg.go.dev/reflect#TypeFor)
- [sync package](https://pkg.go.dev/sync)
- [Go Specification: Type parameter declarations](https://go.dev/ref/spec#Type_parameter_declarations)
