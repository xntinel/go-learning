# 13. Generic Middleware and Decorator

Middleware is a decorator around a handler. With generics, the same composition machinery can wrap handlers for different request and response types without losing type information.

## Concepts

### A Handler Type Carries The Domain Types

`Handler[Req, Resp]` states that a request of type `Req` returns a response of type `Resp`. Middleware with the same type parameters cannot accidentally pass a different request or return a different response.

### Composition Order Must Be Explicit

If middleware is listed as `A, B`, most Go APIs make `A` the outer wrapper and `B` the inner wrapper. The chain implementation applies middleware in reverse to preserve that order.

### Middleware Constructors Can Validate Policy

Retry middleware needs a positive attempt count. The constructor returns a wrapped sentinel error instead of creating a chain that will behave strangely at runtime.

## Exercises

Set up the module:

```bash
mkdir -p go-solutions/20-generics/13-generic-middleware-and-decorator/13-generic-middleware-and-decorator/cmd/demo
cd go-solutions/20-generics/13-generic-middleware-and-decorator/13-generic-middleware-and-decorator
```

### Exercise 1: Build The Chain And Retry Middleware

Create `middleware.go`:

```go
package genericmiddleware

import (
	"context"
	"errors"
	"fmt"
	"time"
)

var ErrInvalidAttempts = errors.New("attempts must be greater than zero")

type Handler[Req, Resp any] func(context.Context, Req) (Resp, error)

type Middleware[Req, Resp any] func(Handler[Req, Resp]) Handler[Req, Resp]

func Chain[Req, Resp any](handler Handler[Req, Resp], middleware ...Middleware[Req, Resp]) Handler[Req, Resp] {
	for i := len(middleware) - 1; i >= 0; i-- {
		handler = middleware[i](handler)
	}
	return handler
}

func WithTimeout[Req, Resp any](timeout time.Duration) Middleware[Req, Resp] {
	return func(next Handler[Req, Resp]) Handler[Req, Resp] {
		return func(ctx context.Context, req Req) (Resp, error) {
			ctx, cancel := context.WithTimeout(ctx, timeout)
			defer cancel()
			return next(ctx, req)
		}
	}
}

func Retry[Req, Resp any](attempts int, retryable func(error) bool) (Middleware[Req, Resp], error) {
	if attempts < 1 {
		return nil, fmt.Errorf("retry: %w", ErrInvalidAttempts)
	}
	return func(next Handler[Req, Resp]) Handler[Req, Resp] {
		return func(ctx context.Context, req Req) (Resp, error) {
			var zero Resp
			var last error
			for i := 0; i < attempts; i++ {
				resp, err := next(ctx, req)
				if err == nil {
					return resp, nil
				}
				last = err
				if !retryable(err) {
					return zero, err
				}
			}
			return zero, last
		}
	}, nil
}
```

### Exercise 2: Add Tests And An Example

Create `middleware_test.go`:

```go
package genericmiddleware

import (
	"context"
	"errors"
	"fmt"
	"testing"
)

var errTemporary = errors.New("temporary failure")

func TestRetry(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name     string
		attempts int
		failures int
		wantErr  error
	}{
		{name: "eventual success", attempts: 3, failures: 2},
		{name: "invalid attempts", attempts: 0, wantErr: ErrInvalidAttempts},
	}

	for _, tt := range tests {
		tt := tt
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			mw, err := Retry[string, string](tt.attempts, func(err error) bool { return errors.Is(err, errTemporary) })
			if !errors.Is(err, tt.wantErr) {
				t.Fatalf("err = %v, want %v", err, tt.wantErr)
			}
			if tt.wantErr != nil {
				return
			}
			calls := 0
			handler := Chain(func(ctx context.Context, req string) (string, error) {
				calls++
				if calls <= tt.failures {
					return "", errTemporary
				}
				return req + " ok", nil
			}, mw)
			got, err := handler(context.Background(), "work")
			if err != nil {
				t.Fatal(err)
			}
			if got != "work ok" || calls != 3 {
				t.Fatalf("got %q after %d calls", got, calls)
			}
		})
	}
}

func TestChainOrder(t *testing.T) {
	t.Parallel()

	var order []string
	wrap := func(name string) Middleware[string, string] {
		return func(next Handler[string, string]) Handler[string, string] {
			return func(ctx context.Context, req string) (string, error) {
				order = append(order, name+" before")
				resp, err := next(ctx, req)
				order = append(order, name+" after")
				return resp, err
			}
		}
	}
	handler := Chain(func(ctx context.Context, req string) (string, error) { return req, nil }, wrap("a"), wrap("b"))
	_, _ = handler(context.Background(), "x")
	if fmt.Sprint(order) != "[a before b before b after a after]" {
		t.Fatalf("order = %v", order)
	}
}

func ExampleChain() {
	handler := Chain(func(ctx context.Context, req string) (string, error) { return req + " ok", nil })
	resp, _ := handler(context.Background(), "demo")
	fmt.Println(resp)
	// Output: demo ok
}
```

### Exercise 3: Add A Runnable Demo

Create `cmd/demo/main.go`:

```go
package main

import (
	"context"
	"fmt"
	"log"

	genericmiddleware "example.com/verify"
)

func main() {
	mw, err := genericmiddleware.Retry[string, string](1, func(error) bool { return false })
	if err != nil {
		log.Fatal(err)
	}
	handler := genericmiddleware.Chain(func(ctx context.Context, req string) (string, error) { return req + " ok", nil }, mw)
	resp, err := handler(context.Background(), "request")
	if err != nil {
		log.Fatal(err)
	}
	fmt.Println(resp)
}
```

## Common Mistakes

### Reversing Chain Order

Wrong: applying middleware from left to right and making the last middleware outermost.

Fix: loop backward so the first middleware listed is the first to see the request.

### Dropping Type Parameters Inside Middleware

Wrong: converting the request to `any` inside the chain.

Fix: keep `Handler[Req, Resp]` all the way through the decorator.

### Creating Invalid Retry Policies

Wrong: allowing zero attempts.

Fix: return `ErrInvalidAttempts` from the retry constructor.

## Verification

Run this from `~/go-exercises/genericmiddleware`:

```bash
test -z "$(gofmt -l .)"
go vet ./...
go build ./...
go test -count=1 -race ./...
```

Add one more test proving a non-retryable error stops after one call.

## Summary

- Generic handlers preserve request and response types through middleware.
- Chain order is part of the API contract and must be tested.
- Middleware constructors should validate invalid policy values.
- Sentinel errors make invalid middleware configuration easy to assert.

## What's Next

Next: [Building a Type-Safe Event Bus](../14-building-a-type-safe-event-bus/14-building-a-type-safe-event-bus.md).

## Resources

- [context package](https://pkg.go.dev/context)
- [Go Blog: When To Use Generics](https://go.dev/blog/when-generics)
- [Go Specification: Function types](https://go.dev/ref/spec#Function_types)
