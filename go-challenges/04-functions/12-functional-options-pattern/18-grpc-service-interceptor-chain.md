# Exercise 18: gRPC Server Middleware Stack With Ordered Interceptors

**Nivel: Avanzado** — validacion normal (tabla de casos, incluye borde o concurrencia).

A gRPC-style interceptor chain is normally built by listing interceptors in
the order they should run — which means the order they run in is exactly
the order a caller happened to write them down. This module inverts that:
each option tags its interceptor with a *category*, and the constructor
sorts by category after every option has run, so logging and metrics always
run before auth, and panic recovery always sits closest to the handler,
regardless of what order the caller registered them in.

## What you'll build

```text
interceptorchain/                independent module: example.com/interceptorchain
  go.mod                         go 1.24
  interceptorchain.go            UnaryHandler, UnaryInterceptor, Server, Option, New,
                                  WithLogging, WithMetrics, WithAuth, WithRecovery,
                                  WithStandardPreset, Names, Build
  cmd/
    demo/
      main.go                    registers out of order, shows the chain still runs in category order
  interceptorchain_test.go       table test over registration, ordering, short-circuit, -race concurrency
```

- Files: `interceptorchain.go`, `cmd/demo/main.go`, `interceptorchain_test.go`.
- Implement: `New(opts ...Option) (*Server, error)` whose `Build` composes an interceptor chain where observability interceptors always run first, auth second, and recovery last (innermost), independent of registration order.
- Test: nil-function and duplicate-registration rejection, that chain order is category-driven, that an auth failure short-circuits before the handler runs, that recovery only wraps the handler, and a `-race` concurrency check.
- Verify: `go test -count=1 -race ./...`

Set up the module:

```bash
go mod edit -go=1.24
```

### Category, not registration order, decides position

`UnaryHandler` and `UnaryInterceptor` are modeled on
`google.golang.org/grpc`'s own types, without taking the dependency — the
shape is what matters here, not the wire protocol. Each `With*` option
appends a `namedInterceptor` tagged with a `category`
(`categoryObservability`, `categoryAuth`, `categoryRecovery`). `New` applies
every option first, then runs a single `sort.SliceStable` over the whole
slice by category. That stability matters: two observability interceptors
(logging and metrics) keep whatever relative order the caller registered
them in, while auth and recovery are pinned to their categories regardless
of when they were added. The demo registers auth and recovery *before*
logging and metrics on purpose, to prove the final chain order does not
depend on it.

### Why recovery wraps only the handler

`WithRecovery`'s interceptor is deliberately the innermost layer, directly
around the handler, not the outermost layer around the whole chain. That
means it only recovers panics from application code in the handler itself —
a bug in the logging or auth interceptor is not silently swallowed, which
would hide a real defect in infrastructure code behind what looks like a
clean RPC failure. `WithAuth` and `WithRecovery` each allow only one
registration: two competing auth interceptors would make it ambiguous which
one actually decided access, and two recovery layers would make it
ambiguous which one reported a given panic.

### `WithStandardPreset`: an option built from other options

`WithStandardPreset` demonstrates that an `Option` can itself be assembled
by calling other option-constructing functions and composing their closures
— a "preset" is just an option whose closure applies several sub-options in
sequence. This is the same idea as `New(opts ...Option)` one level up: a
function that takes `Option`s and produces one is a natural way to bundle a
house style ("always attach logging, metrics, auth, and recovery this way")
without repeating four calls at every call site.

Create `interceptorchain.go`:

```go
package interceptorchain

import (
	"context"
	"fmt"
	"sort"
)

// UnaryHandler is the terminal RPC handler, modeled on
// google.golang.org/grpc's UnaryHandler without the dependency.
type UnaryHandler func(ctx context.Context, req any) (any, error)

// UnaryInterceptor wraps a handler, modeled on grpc.UnaryServerInterceptor.
type UnaryInterceptor func(ctx context.Context, req any, next UnaryHandler) (any, error)

// category fixes the position an interceptor occupies in the chain,
// independent of the order its option was passed to New.
type category int

const (
	categoryObservability category = iota // logging, metrics
	categoryAuth
	categoryRecovery
)

type namedInterceptor struct {
	name     string
	category category
	fn       UnaryInterceptor
}

// Server holds an ordered interceptor chain built from options.
type Server struct {
	interceptors []namedInterceptor
	hasAuth      bool
	hasRecovery  bool
}

// Option configures a Server's interceptor chain and may reject invalid
// input.
type Option func(*Server) error

// New applies opts in order, then stably sorts the interceptors by category
// so that, regardless of registration order, observability interceptors run
// before auth, and recovery runs last, directly wrapping the handler.
func New(opts ...Option) (*Server, error) {
	s := &Server{}
	for _, opt := range opts {
		if err := opt(s); err != nil {
			return nil, err
		}
	}
	sort.SliceStable(s.interceptors, func(i, j int) bool {
		return s.interceptors[i].category < s.interceptors[j].category
	})
	return s, nil
}

// WithLogging adds an observability-phase interceptor that calls fn before
// and after invoking the next handler.
func WithLogging(fn func(method string)) Option {
	return func(s *Server) error {
		if fn == nil {
			return fmt.Errorf("logging function is nil")
		}
		s.interceptors = append(s.interceptors, namedInterceptor{
			name:     "logging",
			category: categoryObservability,
			fn: func(ctx context.Context, req any, next UnaryHandler) (any, error) {
				fn("start")
				resp, err := next(ctx, req)
				fn("end")
				return resp, err
			},
		})
		return nil
	}
}

// WithMetrics adds an observability-phase interceptor that calls fn once per
// call.
func WithMetrics(fn func(method string)) Option {
	return func(s *Server) error {
		if fn == nil {
			return fmt.Errorf("metrics function is nil")
		}
		s.interceptors = append(s.interceptors, namedInterceptor{
			name:     "metrics",
			category: categoryObservability,
			fn: func(ctx context.Context, req any, next UnaryHandler) (any, error) {
				fn("recorded")
				return next(ctx, req)
			},
		})
		return nil
	}
}

// WithAuth adds the auth-phase interceptor. Only one is allowed: two
// competing authentication schemes make "which one decided access"
// ambiguous, so a second call is rejected.
func WithAuth(authFn func(ctx context.Context) error) Option {
	return func(s *Server) error {
		if authFn == nil {
			return fmt.Errorf("auth function is nil")
		}
		if s.hasAuth {
			return fmt.Errorf("auth interceptor already registered")
		}
		s.hasAuth = true
		s.interceptors = append(s.interceptors, namedInterceptor{
			name:     "auth",
			category: categoryAuth,
			fn: func(ctx context.Context, req any, next UnaryHandler) (any, error) {
				if err := authFn(ctx); err != nil {
					return nil, fmt.Errorf("auth: %w", err)
				}
				return next(ctx, req)
			},
		})
		return nil
	}
}

// WithRecovery adds the recovery-phase interceptor, which always occupies
// the innermost position, directly wrapping the handler so it catches
// panics from application code without also swallowing bugs in earlier
// middleware. Only one is allowed, for the same reason WithAuth allows only
// one: two recovery layers would make it ambiguous which one reports the
// panic.
func WithRecovery(onPanic func(recovered any)) Option {
	return func(s *Server) error {
		if onPanic == nil {
			return fmt.Errorf("onPanic function is nil")
		}
		if s.hasRecovery {
			return fmt.Errorf("recovery interceptor already registered")
		}
		s.hasRecovery = true
		s.interceptors = append(s.interceptors, namedInterceptor{
			name:     "recovery",
			category: categoryRecovery,
			fn: func(ctx context.Context, req any, next UnaryHandler) (resp any, err error) {
				defer func() {
					if r := recover(); r != nil {
						onPanic(r)
						err = fmt.Errorf("panic recovered: %v", r)
					}
				}()
				return next(ctx, req)
			},
		})
		return nil
	}
}

// WithStandardPreset composes logging, metrics, auth, and recovery into a
// single option, demonstrating that an Option can itself be built from other
// Options.
func WithStandardPreset(logFn func(string), metricsFn func(string), authFn func(context.Context) error, onPanic func(any)) Option {
	sub := []Option{
		WithLogging(logFn),
		WithMetrics(metricsFn),
		WithAuth(authFn),
		WithRecovery(onPanic),
	}
	return func(s *Server) error {
		for _, opt := range sub {
			if err := opt(s); err != nil {
				return err
			}
		}
		return nil
	}
}

// Names returns the interceptor names in final chain order (outermost
// first), useful for tests and diagnostics.
func (s *Server) Names() []string {
	names := make([]string, len(s.interceptors))
	for i, ic := range s.interceptors {
		names[i] = ic.name
	}
	return names
}

// Build composes the chain around handler, outermost interceptor first.
func (s *Server) Build(handler UnaryHandler) UnaryHandler {
	final := handler
	for i := len(s.interceptors) - 1; i >= 0; i-- {
		ic := s.interceptors[i]
		next := final
		final = func(ctx context.Context, req any) (any, error) {
			return ic.fn(ctx, req, next)
		}
	}
	return final
}
```

### The runnable demo

The demo registers `WithAuth` and `WithRecovery` before `WithLogging` and
`WithMetrics` — the opposite of the order they end up running in — to make
the point that category, not registration order, decides the chain.

Create `cmd/demo/main.go`:

```go
package main

import (
	"context"
	"fmt"

	"example.com/interceptorchain"
)

func main() {
	var trace []string

	s, err := interceptorchain.New(
		// Registered out of execution order on purpose: auth first, then
		// recovery, then the observability interceptors. The chain still
		// runs logging/metrics, then auth, then recovery, because category
		// -- not registration order -- decides position.
		interceptorchain.WithAuth(func(ctx context.Context) error {
			trace = append(trace, "auth")
			return nil
		}),
		interceptorchain.WithRecovery(func(recovered any) {
			trace = append(trace, fmt.Sprintf("recovery(%v)", recovered))
		}),
		interceptorchain.WithLogging(func(phase string) {
			trace = append(trace, "logging:"+phase)
		}),
		interceptorchain.WithMetrics(func(string) {
			trace = append(trace, "metrics")
		}),
	)
	if err != nil {
		panic(err)
	}

	fmt.Println("chain order:", s.Names())

	handler := s.Build(func(ctx context.Context, req any) (any, error) {
		trace = append(trace, "handler")
		return "ok", nil
	})

	resp, err := handler(context.Background(), "request")
	fmt.Printf("response: %v, err: %v\n", resp, err)
	fmt.Println("execution trace:", trace)
}
```

Run it:

```bash
go run ./cmd/demo
```

Expected output:

```
chain order: [logging metrics auth recovery]
response: ok, err: <nil>
execution trace: [logging:start metrics auth handler logging:end]
```

### Tests

`TestNewValidation` tables nil-function rejection and the duplicate-auth
and duplicate-recovery guards. `TestChainOrderIsCategoryDrivenNotRegistrationOrder`
registers in the same scrambled order as the demo and asserts the final
`Names()` order. `TestAuthRunsBeforeHandlerAndCanShortCircuit` proves a
denied auth check stops the handler from ever running.
`TestRecoveryWrapsOnlyTheHandler` proves a handler panic surfaces as an
error through `Build`'s composed chain. `TestConcurrentInvocations` runs
`-race` over concurrent calls through the same built handler.

Create `interceptorchain_test.go`:

```go
package interceptorchain

import (
	"context"
	"errors"
	"sync"
	"testing"
)

func noopFn(string)                  {}
func noopAuth(context.Context) error { return nil }
func noopPanic(any)                  {}

func TestNewValidation(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name    string
		opts    []Option
		wantErr bool
	}{
		{name: "no options"},
		{name: "nil logging fn", opts: []Option{WithLogging(nil)}, wantErr: true},
		{name: "nil metrics fn", opts: []Option{WithMetrics(nil)}, wantErr: true},
		{name: "nil auth fn", opts: []Option{WithAuth(nil)}, wantErr: true},
		{name: "nil recovery fn", opts: []Option{WithRecovery(nil)}, wantErr: true},
		{name: "duplicate auth rejected", opts: []Option{WithAuth(noopAuth), WithAuth(noopAuth)}, wantErr: true},
		{name: "duplicate recovery rejected", opts: []Option{WithRecovery(noopPanic), WithRecovery(noopPanic)}, wantErr: true},
		{name: "one of each is fine", opts: []Option{
			WithLogging(noopFn), WithMetrics(noopFn), WithAuth(noopAuth), WithRecovery(noopPanic),
		}},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			_, err := New(tt.opts...)
			if tt.wantErr && err == nil {
				t.Fatal("expected error, got nil")
			}
			if !tt.wantErr && err != nil {
				t.Fatalf("unexpected error: %v", err)
			}
		})
	}
}

func TestChainOrderIsCategoryDrivenNotRegistrationOrder(t *testing.T) {
	t.Parallel()

	s, err := New(
		WithAuth(noopAuth),
		WithRecovery(noopPanic),
		WithLogging(noopFn),
		WithMetrics(noopFn),
	)
	if err != nil {
		t.Fatal(err)
	}

	want := []string{"logging", "metrics", "auth", "recovery"}
	got := s.Names()
	if len(got) != len(want) {
		t.Fatalf("Names() = %v, want %v", got, want)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Fatalf("Names()[%d] = %s, want %s (full: %v)", i, got[i], want[i], got)
		}
	}
}

func TestAuthRunsBeforeHandlerAndCanShortCircuit(t *testing.T) {
	t.Parallel()

	var trace []string
	s, err := New(
		WithLogging(func(phase string) { trace = append(trace, "logging:"+phase) }),
		WithAuth(func(ctx context.Context) error {
			trace = append(trace, "auth")
			return errors.New("denied")
		}),
	)
	if err != nil {
		t.Fatal(err)
	}

	handler := s.Build(func(ctx context.Context, req any) (any, error) {
		trace = append(trace, "handler")
		return "ok", nil
	})

	_, err = handler(context.Background(), "req")
	if err == nil {
		t.Fatal("expected auth failure to propagate, got nil error")
	}
	for _, name := range trace {
		if name == "handler" {
			t.Fatalf("handler ran despite auth failure, trace: %v", trace)
		}
	}
}

func TestRecoveryWrapsOnlyTheHandler(t *testing.T) {
	t.Parallel()

	var recovered any
	s, err := New(
		WithRecovery(func(r any) { recovered = r }),
	)
	if err != nil {
		t.Fatal(err)
	}

	handler := s.Build(func(ctx context.Context, req any) (any, error) {
		panic("boom")
	})

	_, err = handler(context.Background(), "req")
	if err == nil {
		t.Fatal("expected recovered panic to surface as an error")
	}
	if recovered != "boom" {
		t.Fatalf("recovered = %v, want boom", recovered)
	}
}

func TestConcurrentInvocations(t *testing.T) {
	t.Parallel()

	s, err := New(
		WithLogging(noopFn),
		WithAuth(noopAuth),
		WithRecovery(noopPanic),
	)
	if err != nil {
		t.Fatal(err)
	}
	handler := s.Build(func(ctx context.Context, req any) (any, error) {
		return req, nil
	})

	var wg sync.WaitGroup
	for i := 0; i < 50; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			resp, err := handler(context.Background(), i)
			if err != nil {
				t.Errorf("unexpected error: %v", err)
				return
			}
			if resp.(int) != i {
				t.Errorf("resp = %v, want %d", resp, i)
			}
		}(i)
	}
	wg.Wait()
}
```

## Review

The chain is correct when its final order reflects a fixed policy —
observability first, auth second, recovery innermost — rather than an
accident of registration order, and when recovery only shields the handler
it directly wraps. `sort.SliceStable` after the option loop is what turns
"whatever order the caller wrote these in" into "whatever order the design
requires"; stability is what lets two interceptors in the same category
(logging and metrics) still respect each other's relative registration
order. `Build` itself has no shared mutable state — each call composes a
fresh closure chain around the given handler — which is why the concurrency
test needs no mutex: the chain is safe to invoke from many goroutines simply
because there is nothing in it to race over.

## Resources

- [grpc-go: UnaryServerInterceptor](https://pkg.go.dev/google.golang.org/grpc#UnaryServerInterceptor)
- [grpc-ecosystem/go-grpc-middleware](https://github.com/grpc-ecosystem/go-grpc-middleware)
- [pkg.go.dev: sort.SliceStable](https://pkg.go.dev/sort#SliceStable)

---

Back to [00-concepts.md](00-concepts.md) | Prev: [17-sliding-window-rate-limiter.md](17-sliding-window-rate-limiter.md) | Next: [19-event-store-compaction-policy.md](19-event-store-compaction-policy.md)
