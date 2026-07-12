# Exercise 28: gRPC Unary Interceptor Chain via Higher-Order Functions

**Nivel: Avanzado** — validacion normal (tabla de casos, incluye concurrencia).

`google.golang.org/grpc` lets a server register exactly one
`UnaryServerInterceptor`, so real services build that one interceptor by
composing smaller ones — logging, metrics, auth — around the final
handler. This module reproduces that composition, stdlib-only: a
higher-order `Chain` that turns a slice of `Interceptor`s and a `Handler`
into one `Handler`, with the first interceptor outermost and any
interceptor free to short-circuit the whole chain by never calling `next`.

## What you'll build

```text
interceptor/                  independent module: example.com/grpc-interceptor-chain-composition
  go.mod                       go 1.24
  interceptor.go                type Handler, type Interceptor, func Chain, Logging/Metrics/Auth interceptors
  cmd/
    demo/
      main.go                    runnable demo: allowed call, a denied call, log + counter
  interceptor_test.go            table/order test, auth short-circuit, empty chain, metrics count, concurrency (-race)
```

Files: `interceptor.go`, `cmd/demo/main.go`, `interceptor_test.go`.
Implement: `type Handler func(ctx, *Request) (*Response, error)`, `type Interceptor func(ctx, *Request, next Handler) (*Response, error)`, `func Chain(handler Handler, interceptors ...Interceptor) Handler`, plus `LoggingInterceptor`, `MetricsInterceptor`, and `AuthInterceptor` constructors; the first interceptor passed to `Chain` must run first on the way in and last on the way out, and any interceptor that does not call `next` must prevent every interceptor after it (and the handler) from running.
Test: two interceptors run in outer-to-inner then inner-to-outer order around the handler; an auth interceptor rejecting a method short-circuits before the handler runs; `Chain` with zero interceptors calls the handler directly; a metrics interceptor placed before an auth interceptor still counts a rejected attempt; concurrent calls through one composed chain are race-free with an accurate atomic counter and mutex-protected log.
Verify: `go test -count=1 -race ./...`

Set up the module:

```bash
go mod edit -go=1.24
```

### Why `Chain` builds the handler back-to-front

Real gRPC servers pass `grpc.ChainUnaryInterceptor(a, b, c)` and expect `a`
to be the outermost layer — it sees the request first and the response
last. Building that from a slice means folding from the *last* interceptor
inward: start with the real `handler`, wrap it with the last interceptor
to get a new `Handler`, wrap that with the second-to-last, and so on, so
that after wrapping with the first interceptor last, `a` ends up as the
outermost function. `Chain` does exactly this with a `for i := len-1; i >=
0; i--` loop, capturing `ic := interceptors[i]` and `next := h` as fresh
locals each iteration so the closure built on this pass does not alias the
next pass's rebinding of `h` — Go 1.22+ makes this automatic for `for
range`, but here we index manually, so the explicit locals matter. The
result is a single `Handler` that, when called, threads the request
through every interceptor in declared order, and any interceptor that
returns without invoking `next` — `AuthInterceptor` on a denied method —
stops everything downstream of it cold: no later interceptor runs, and the
real handler never sees the request.

Create `interceptor.go`:

```go
// Package interceptor models gRPC-style unary interceptors — the
// UnaryServerInterceptor pattern from google.golang.org/grpc — as plain
// function types, composed into a single Handler without pulling in the
// grpc module itself.
package interceptor

import (
	"context"
	"errors"
	"sync"
	"sync/atomic"
)

// Request stands in for a gRPC request: a method name and an opaque
// payload.
type Request struct {
	Method  string
	Payload any
}

// Response stands in for a gRPC response.
type Response struct {
	Payload any
}

// Handler processes one request and produces a response, mirroring
// grpc.UnaryHandler.
type Handler func(ctx context.Context, req *Request) (*Response, error)

// Interceptor observes/mutates a request around a call to next, mirroring
// grpc.UnaryServerInterceptor. Calling next is optional: an interceptor
// that never calls it short-circuits the chain.
type Interceptor func(ctx context.Context, req *Request, next Handler) (*Response, error)

// ErrUnauthenticated is a sentinel an auth interceptor can return to
// short-circuit the chain without calling next.
var ErrUnauthenticated = errors.New("unauthenticated")

// Chain composes interceptors around handler so that the first
// interceptor is outermost (runs first on the way in, last on the way
// out) and handler runs only if every interceptor calls its next.
func Chain(handler Handler, interceptors ...Interceptor) Handler {
	h := handler
	for i := len(interceptors) - 1; i >= 0; i-- {
		ic := interceptors[i]
		next := h
		h = func(ctx context.Context, req *Request) (*Response, error) {
			return ic(ctx, req, next)
		}
	}
	return h
}

// LoggingInterceptor appends "before:Method" then "after:Method" to log,
// guarded by a mutex since it may be shared across concurrent calls.
func LoggingInterceptor(mu *sync.Mutex, log *[]string) Interceptor {
	return func(ctx context.Context, req *Request, next Handler) (*Response, error) {
		mu.Lock()
		*log = append(*log, "before:"+req.Method)
		mu.Unlock()

		resp, err := next(ctx, req)

		mu.Lock()
		*log = append(*log, "after:"+req.Method)
		mu.Unlock()
		return resp, err
	}
}

// MetricsInterceptor atomically counts every call that reaches it.
func MetricsInterceptor(counter *int64) Interceptor {
	return func(ctx context.Context, req *Request, next Handler) (*Response, error) {
		atomic.AddInt64(counter, 1)
		return next(ctx, req)
	}
}

// AuthInterceptor rejects any method not in allowed without calling next.
func AuthInterceptor(allowed map[string]bool) Interceptor {
	return func(ctx context.Context, req *Request, next Handler) (*Response, error) {
		if !allowed[req.Method] {
			return nil, ErrUnauthenticated
		}
		return next(ctx, req)
	}
}
```

### The runnable demo

Create `cmd/demo/main.go`:

```go
package main

import (
	"context"
	"fmt"
	"sync"

	"example.com/grpc-interceptor-chain-composition"
)

func main() {
	var mu sync.Mutex
	var log []string
	var calls int64

	handler := func(ctx context.Context, req *interceptor.Request) (*interceptor.Response, error) {
		return &interceptor.Response{Payload: "ok:" + req.Method}, nil
	}

	chained := interceptor.Chain(handler,
		interceptor.LoggingInterceptor(&mu, &log),
		interceptor.MetricsInterceptor(&calls),
		interceptor.AuthInterceptor(map[string]bool{"/orders.Get": true}),
	)

	resp, err := chained(context.Background(), &interceptor.Request{Method: "/orders.Get"})
	fmt.Printf("call 1: resp=%v err=%v\n", resp.Payload, err)

	resp, err = chained(context.Background(), &interceptor.Request{Method: "/orders.Delete"})
	fmt.Printf("call 2: resp=%v err=%v\n", resp, err)

	fmt.Println("log:", log)
	fmt.Println("calls counted:", calls)
}
```

Run it:

```bash
go run ./cmd/demo
```

Expected output:

```
call 1: resp=ok:/orders.Get err=<nil>
call 2: resp=<nil> err=unauthenticated
log: [before:/orders.Get after:/orders.Get before:/orders.Delete after:/orders.Delete]
calls counted: 2
```

### Tests

Create `interceptor_test.go`:

```go
package interceptor

import (
	"context"
	"errors"
	"sync"
	"sync/atomic"
	"testing"
)

func orderInterceptor(name string, order *[]string, mu *sync.Mutex) Interceptor {
	return func(ctx context.Context, req *Request, next Handler) (*Response, error) {
		mu.Lock()
		*order = append(*order, "before:"+name)
		mu.Unlock()

		resp, err := next(ctx, req)

		mu.Lock()
		*order = append(*order, "after:"+name)
		mu.Unlock()
		return resp, err
	}
}

func TestChainRunsOutermostFirstOnTheWayInAndLastOnTheWayOut(t *testing.T) {
	t.Parallel()
	var mu sync.Mutex
	var order []string

	handler := func(ctx context.Context, req *Request) (*Response, error) {
		mu.Lock()
		order = append(order, "handler")
		mu.Unlock()
		return &Response{}, nil
	}

	chained := Chain(handler,
		orderInterceptor("A", &order, &mu),
		orderInterceptor("B", &order, &mu),
	)

	_, err := chained(context.Background(), &Request{Method: "/m"})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	want := []string{"before:A", "before:B", "handler", "after:B", "after:A"}
	if len(order) != len(want) {
		t.Fatalf("order = %v, want %v", order, want)
	}
	for i := range want {
		if order[i] != want[i] {
			t.Fatalf("order = %v, want %v", order, want)
		}
	}
}

func TestAuthInterceptorShortCircuitsWithoutCallingHandler(t *testing.T) {
	t.Parallel()
	handlerCalled := false
	handler := func(ctx context.Context, req *Request) (*Response, error) {
		handlerCalled = true
		return &Response{}, nil
	}

	chained := Chain(handler, AuthInterceptor(map[string]bool{"/allowed": true}))

	_, err := chained(context.Background(), &Request{Method: "/denied"})
	if !errors.Is(err, ErrUnauthenticated) {
		t.Fatalf("err = %v, want ErrUnauthenticated", err)
	}
	if handlerCalled {
		t.Fatal("handler ran despite auth rejection")
	}
}

func TestChainWithNoInterceptorsCallsHandlerDirectly(t *testing.T) {
	t.Parallel()
	handler := func(ctx context.Context, req *Request) (*Response, error) {
		return &Response{Payload: "direct"}, nil
	}
	chained := Chain(handler)
	resp, err := chained(context.Background(), &Request{Method: "/m"})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if resp.Payload != "direct" {
		t.Fatalf("resp.Payload = %v, want direct", resp.Payload)
	}
}

func TestMetricsInterceptorCountsEveryAttempt(t *testing.T) {
	t.Parallel()
	var count int64
	handler := func(ctx context.Context, req *Request) (*Response, error) {
		return &Response{}, nil
	}
	chained := Chain(handler,
		MetricsInterceptor(&count),
		AuthInterceptor(map[string]bool{"/allowed": true}),
	)

	_, _ = chained(context.Background(), &Request{Method: "/allowed"})
	_, _ = chained(context.Background(), &Request{Method: "/denied"})

	if got := atomic.LoadInt64(&count); got != 2 {
		t.Fatalf("count = %d, want 2 (both attempts reach the metrics interceptor)", got)
	}
}

func TestConcurrentCallsThroughChainAreRaceFree(t *testing.T) {
	t.Parallel()
	var mu sync.Mutex
	var log []string
	var count int64

	handler := func(ctx context.Context, req *Request) (*Response, error) {
		return &Response{Payload: req.Method}, nil
	}
	chained := Chain(handler,
		LoggingInterceptor(&mu, &log),
		MetricsInterceptor(&count),
	)

	var wg sync.WaitGroup
	for range 50 {
		wg.Add(1)
		go func() {
			defer wg.Done()
			_, _ = chained(context.Background(), &Request{Method: "/concurrent"})
		}()
	}
	wg.Wait()

	if got := atomic.LoadInt64(&count); got != 50 {
		t.Fatalf("count = %d, want 50", got)
	}
	mu.Lock()
	gotLog := len(log)
	mu.Unlock()
	if gotLog != 100 {
		t.Fatalf("log entries = %d, want 100 (before+after per call)", gotLog)
	}
}
```

## Review

The chain is correct when it satisfies two independent properties:
ordering and short-circuiting. `TestChainRunsOutermostFirstOnTheWayInAndLastOnTheWayOut`
nails down ordering — the classic onion-layer shape everyone expects from
middleware, proven with an explicit sequence rather than just "it didn't
panic". `TestAuthInterceptorShortCircuitsWithoutCallingHandler` nails down
the other half: an interceptor that returns without calling `next` must
stop the world downstream of it, and `TestMetricsInterceptorCountsEveryAttempt`
is the test that catches the subtle placement bug — a metrics interceptor
positioned *before* auth in the chain still counts rejected calls, which
is correct (you want to know an attempt happened) but easy to get backwards
if `Chain` folded the slice in the wrong direction. The concurrency test
is not about interceptor logic; it is about the closures `Chain` builds
being safe to invoke from many goroutines at once, which holds here
because every piece of state an interceptor touches — the counter, the
log — is protected by an atomic or a mutex, not because `Chain` itself
does anything special.

## Resources

- [google.golang.org/grpc: UnaryServerInterceptor](https://pkg.go.dev/google.golang.org/grpc#UnaryServerInterceptor)
- [google.golang.org/grpc: ChainUnaryInterceptor](https://pkg.go.dev/google.golang.org/grpc#ChainUnaryInterceptor)
- [sync/atomic package](https://pkg.go.dev/sync/atomic)

---

Back to [00-concepts.md](00-concepts.md) | Prev: [27-fsm-transition-callback-handler.md](27-fsm-transition-callback-handler.md) | Next: [29-lru-eviction-policy-selector.md](29-lru-eviction-policy-selector.md)
