# 14. gRPC Interceptors

gRPC interceptors are the canonical way to add cross-cutting concerns — authentication, logging, metrics, and panic recovery — to every RPC without modifying service handlers. The hard part is not the concept (it is HTTP middleware renamed) but the two orthogonal axes: unary vs. stream RPCs, and server vs. client side. Each axis has a distinct function signature; getting them wrong compiles silently but breaks at runtime. A secondary challenge is testing interceptors in isolation: you do not need a running gRPC server because interceptors are plain functions you can call directly.

```text
grpcmiddleware/
  go.mod
  interceptor.go
  chain.go
  interceptor_test.go
  cmd/demo/main.go
```

The package implements four interceptors (logging, auth, metrics, recovery) and a `ChainUnary` helper. Tests call the interceptors as functions — no server required. `cmd/demo` shows how the same interceptors attach to a real `grpc.NewServer`.

## Concepts

### The Interceptor as a Wrapper Function

A `grpc.UnaryServerInterceptor` is a function value:

```go
type UnaryServerInterceptor func(
	ctx     context.Context,
	req     any,
	info    *grpc.UnaryServerInfo,
	handler grpc.UnaryHandler,
) (any, error)
```

`handler` is the next function in the chain — either another interceptor or the actual service method. Calling `handler(ctx, req)` passes control forward; not calling it short-circuits the request (the auth interceptor does this on rejection). The interceptor can inspect or mutate both before and after the call:

```go
func timingInterceptor(
	ctx     context.Context,
	req     any,
	info    *grpc.UnaryServerInfo,
	handler grpc.UnaryHandler,
) (any, error) {
	start := time.Now()
	resp, err := handler(ctx, req)
	// post-handler: duration is now known
	slog.Info("rpc", "method", info.FullMethod, "ms", time.Since(start).Milliseconds())
	return resp, err
}
```

### Unary vs. Stream Signatures

| Kind | Type | Key difference |
|---|---|---|
| Unary server | `grpc.UnaryServerInterceptor` | receives `req any`, returns `(any, error)` |
| Stream server | `grpc.StreamServerInterceptor` | receives `grpc.ServerStream`, returns `error` |
| Unary client | `grpc.UnaryClientInterceptor` | runs before the request is sent; can add `CallOption`s |
| Stream client | `grpc.StreamClientInterceptor` | wraps `grpc.ClientStream` after the dial |

The stream interceptor cannot inspect individual messages; it wraps the stream object. To propagate a modified context into a stream handler (for example, to attach metadata-derived values), embed `grpc.ServerStream` and override `Context()`:

```go
type wrappedStream struct {
	grpc.ServerStream
	ctx context.Context
}

func (w *wrappedStream) Context() context.Context { return w.ctx }
```

Pass `&wrappedStream{ss, enrichedCtx}` to `handler` instead of the raw `ss`.

### Execution Order and the Handler Chain

`grpc.ChainUnaryInterceptor(a, b, c)` executes `a -> b -> c -> handler`. The first interceptor in the list is the outermost wrapper. This order matters:

- Recovery must be outermost so it catches panics from all subsequent interceptors and from the handler itself.
- Auth follows recovery so a panicking auth implementation is also caught.
- Logging wraps auth so it records the final status code, including `Unauthenticated`.
- Metrics sits innermost to measure only calls that pass all prior gates.

Typical order: `recovery -> logging -> auth -> metrics`.

### Metadata as the Cross-Cutting Carrier

gRPC metadata (`google.golang.org/grpc/metadata`) is the equivalent of HTTP headers. On the server, incoming metadata arrives in the RPC context:

```go
md, ok := metadata.FromIncomingContext(ctx)
if !ok {
	return nil, status.Error(codes.Unauthenticated, "missing metadata")
}
vals := md.Get("authorization") // keys are normalized to lowercase
```

`metadata.MD` is `map[string][]string`. Keys are always lowercase after transmission; `md.Get("Authorization")` and `md.Get("authorization")` both work in `metadata.Get` because the method lowercases the key, but the raw map key is always lowercase.

On the client side, inject outgoing metadata with `metadata.AppendToOutgoingContext(ctx, "authorization", "Bearer "+token)` inside a `grpc.UnaryClientInterceptor`.

### gRPC Status Errors

Interceptors signal errors with `google.golang.org/grpc/status` and `google.golang.org/grpc/codes`:

```go
return nil, status.Error(codes.Unauthenticated, "invalid token")
```

The caller extracts the code with `status.Code(err)` or unpacks the full status with `status.FromError(err)`. Status errors are not plain wrapped errors; do not use `errors.Is` to check them. Use `status.Code(err) == codes.Unauthenticated` in tests.

### Recovery and the Named-Return Pattern

A panic in an RPC handler crashes the goroutine unless recovered. The recovery interceptor uses a named return so the deferred function can assign `err` after the stack unwinds:

```go
func RecoveryUnary() grpc.UnaryServerInterceptor {
	return func(
		ctx     context.Context,
		req     any,
		info    *grpc.UnaryServerInfo,
		handler grpc.UnaryHandler,
	) (resp any, err error) {
		defer func() {
			if r := recover(); r != nil {
				err = status.Errorf(codes.Internal, "internal server error")
			}
		}()
		return handler(ctx, req)
	}
}
```

Without the named return, assigning to `err` inside `defer` modifies a local variable and the caller sees the panic-zero value instead.

## Exercises

Set up the module:

```bash
mkdir -p go-solutions/33-tcp-udp-and-networking/14-grpc-interceptors/14-grpc-interceptors/cmd/demo
cd go-solutions/33-tcp-udp-and-networking/14-grpc-interceptors/14-grpc-interceptors
go get google.golang.org/grpc@v1.73.0
go mod tidy
```

### Exercise 1: Core Interceptors

Create `interceptor.go`. Each interceptor is a method on a dedicated struct so callers can inject dependencies (logger, token validator) through a constructor rather than package-level state.

```go
package grpcmiddleware

import (
	"context"
	"log/slog"
	"sync"
	"sync/atomic"
	"time"

	"google.golang.org/grpc"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/metadata"
	"google.golang.org/grpc/status"
)

// LoggingInterceptor logs method, duration, and gRPC status code for every RPC.
type LoggingInterceptor struct {
	logger *slog.Logger
}

// NewLogging creates a LoggingInterceptor. If logger is nil, slog.Default() is used.
func NewLogging(logger *slog.Logger) *LoggingInterceptor {
	if logger == nil {
		logger = slog.Default()
	}
	return &LoggingInterceptor{logger: logger}
}

// Unary returns the server-side unary interceptor.
func (l *LoggingInterceptor) Unary() grpc.UnaryServerInterceptor {
	return func(ctx context.Context, req any, info *grpc.UnaryServerInfo, handler grpc.UnaryHandler) (any, error) {
		start := time.Now()
		resp, err := handler(ctx, req)
		st, _ := status.FromError(err)
		l.logger.InfoContext(ctx, "unary rpc",
			"method", info.FullMethod,
			"duration_ms", time.Since(start).Milliseconds(),
			"code", st.Code().String(),
		)
		return resp, err
	}
}

// Stream returns the server-side stream interceptor.
func (l *LoggingInterceptor) Stream() grpc.StreamServerInterceptor {
	return func(srv any, ss grpc.ServerStream, info *grpc.StreamServerInfo, handler grpc.StreamHandler) error {
		start := time.Now()
		err := handler(srv, ss)
		st, _ := status.FromError(err)
		l.logger.InfoContext(ss.Context(), "stream rpc",
			"method", info.FullMethod,
			"duration_ms", time.Since(start).Milliseconds(),
			"code", st.Code().String(),
			"client_stream", info.IsClientStream,
			"server_stream", info.IsServerStream,
		)
		return err
	}
}

// AuthInterceptor validates bearer tokens in incoming metadata.
type AuthInterceptor struct {
	validate func(token string) bool
}

// NewAuth creates an AuthInterceptor. validate receives the raw token value
// (the part after "Bearer ") and returns true if it is valid.
func NewAuth(validate func(token string) bool) *AuthInterceptor {
	return &AuthInterceptor{validate: validate}
}

const bearerPrefix = "Bearer "

func (a *AuthInterceptor) authorize(ctx context.Context) error {
	md, ok := metadata.FromIncomingContext(ctx)
	if !ok {
		return status.Error(codes.Unauthenticated, "missing metadata")
	}
	vals := md.Get("authorization")
	if len(vals) == 0 || len(vals[0]) <= len(bearerPrefix) || vals[0][:len(bearerPrefix)] != bearerPrefix {
		return status.Error(codes.Unauthenticated, "missing or malformed bearer token")
	}
	if !a.validate(vals[0][len(bearerPrefix):]) {
		return status.Error(codes.Unauthenticated, "invalid token")
	}
	return nil
}

// Unary returns the server-side unary auth interceptor.
func (a *AuthInterceptor) Unary() grpc.UnaryServerInterceptor {
	return func(ctx context.Context, req any, info *grpc.UnaryServerInfo, handler grpc.UnaryHandler) (any, error) {
		if err := a.authorize(ctx); err != nil {
			return nil, err
		}
		return handler(ctx, req)
	}
}

// Stream returns the server-side stream auth interceptor.
func (a *AuthInterceptor) Stream() grpc.StreamServerInterceptor {
	return func(srv any, ss grpc.ServerStream, info *grpc.StreamServerInfo, handler grpc.StreamHandler) error {
		if err := a.authorize(ss.Context()); err != nil {
			return err
		}
		return handler(srv, ss)
	}
}

// MethodMetrics holds per-method counters for a single RPC method.
type MethodMetrics struct {
	Calls  atomic.Int64
	Errors atomic.Int64
}

// MetricsInterceptor tracks call counts and error counts per method name.
type MetricsInterceptor struct {
	mu      sync.Mutex
	methods map[string]*MethodMetrics
}

// NewMetrics creates a MetricsInterceptor.
func NewMetrics() *MetricsInterceptor {
	return &MetricsInterceptor{methods: make(map[string]*MethodMetrics)}
}

func (m *MetricsInterceptor) get(method string) *MethodMetrics {
	m.mu.Lock()
	defer m.mu.Unlock()
	if mm, ok := m.methods[method]; ok {
		return mm
	}
	mm := &MethodMetrics{}
	m.methods[method] = mm
	return mm
}

// Snapshot returns the call and error counts for method. Returns (0, 0) if the
// method has never been called.
func (m *MetricsInterceptor) Snapshot(method string) (calls, errs int64) {
	m.mu.Lock()
	mm, ok := m.methods[method]
	m.mu.Unlock()
	if !ok {
		return 0, 0
	}
	return mm.Calls.Load(), mm.Errors.Load()
}

// Unary returns the server-side unary metrics interceptor.
func (m *MetricsInterceptor) Unary() grpc.UnaryServerInterceptor {
	return func(ctx context.Context, req any, info *grpc.UnaryServerInfo, handler grpc.UnaryHandler) (any, error) {
		mm := m.get(info.FullMethod)
		mm.Calls.Add(1)
		resp, err := handler(ctx, req)
		if err != nil {
			mm.Errors.Add(1)
		}
		return resp, err
	}
}

// RecoveryUnary returns a unary server interceptor that converts panics into
// codes.Internal errors, preventing a panicking handler from crashing the server.
func RecoveryUnary() grpc.UnaryServerInterceptor {
	return func(ctx context.Context, req any, info *grpc.UnaryServerInfo, handler grpc.UnaryHandler) (resp any, err error) {
		defer func() {
			if r := recover(); r != nil {
				err = status.Errorf(codes.Internal, "internal server error")
			}
		}()
		return handler(ctx, req)
	}
}
```

`MetricsInterceptor` guards the map with a `sync.Mutex` because `get` is called concurrently from different RPC goroutines. The atomic counters inside `MethodMetrics` avoid a second lock for increment operations.

### Exercise 2: ChainUnary Helper

`grpc.ChainUnaryInterceptor` returns a `grpc.ServerOption`, not a standalone `grpc.UnaryServerInterceptor`. Tests need a chain they can call directly. Create `chain.go`:

```go
package grpcmiddleware

import (
	"context"

	"google.golang.org/grpc"
)

// ChainUnary combines multiple UnaryServerInterceptors into one.
// The first interceptor is the outermost wrapper: execution order is
// interceptors[0] -> interceptors[1] -> ... -> handler.
func ChainUnary(interceptors ...grpc.UnaryServerInterceptor) grpc.UnaryServerInterceptor {
	return func(ctx context.Context, req any, info *grpc.UnaryServerInfo, handler grpc.UnaryHandler) (any, error) {
		h := handler
		for i := len(interceptors) - 1; i >= 0; i-- {
			ic := interceptors[i]
			ih := h
			h = func(ctx context.Context, req any) (any, error) {
				return ic(ctx, req, info, ih)
			}
		}
		return h(ctx, req)
	}
}
```

The loop builds in reverse: the last interceptor wraps the handler first, then each earlier interceptor wraps the already-wrapped version, producing left-to-right execution.

### Exercise 3: Tests

Create `interceptor_test.go`. Tests call interceptors as plain functions — no gRPC server, no network.

```go
package grpcmiddleware

import (
	"context"
	"fmt"
	"log/slog"
	"testing"

	"google.golang.org/grpc"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/metadata"
	"google.golang.org/grpc/status"
)

// mockStream satisfies grpc.ServerStream with a configurable context.
// Only Context() is implemented; any other method panics (acceptable for these tests).
type mockStream struct {
	grpc.ServerStream
	ctx context.Context
}

func (m *mockStream) Context() context.Context { return m.ctx }

var unaryInfo = &grpc.UnaryServerInfo{FullMethod: "/example.Svc/Method"}

func okHandler(_ context.Context, _ any) (any, error) { return "ok", nil }
func errHandler(_ context.Context, _ any) (any, error) {
	return nil, status.Error(codes.NotFound, "not found")
}
func panicHandler(_ context.Context, _ any) (any, error) { panic("boom") }

func ctxWithToken(token string) context.Context {
	md := metadata.Pairs("authorization", "Bearer "+token)
	return metadata.NewIncomingContext(context.Background(), md)
}

func TestAuthInterceptorRejectsMissingMetadata(t *testing.T) {
	t.Parallel()

	auth := NewAuth(func(_ string) bool { return true })
	_, err := auth.Unary()(context.Background(), nil, unaryInfo, okHandler)
	if status.Code(err) != codes.Unauthenticated {
		t.Fatalf("err = %v, want Unauthenticated", err)
	}
}

func TestAuthInterceptorRejectsBadToken(t *testing.T) {
	t.Parallel()

	auth := NewAuth(func(_ string) bool { return false })
	cases := []struct {
		name string
		ctx  context.Context
	}{
		{
			name: "no bearer prefix",
			ctx: func() context.Context {
				md := metadata.Pairs("authorization", "Token abc")
				return metadata.NewIncomingContext(context.Background(), md)
			}(),
		},
		{
			name: "valid format but wrong token",
			ctx:  ctxWithToken("wrong-secret"),
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			_, err := auth.Unary()(tc.ctx, nil, unaryInfo, okHandler)
			if status.Code(err) != codes.Unauthenticated {
				t.Errorf("err = %v, want Unauthenticated", err)
			}
		})
	}
}

func TestAuthInterceptorPassesValidToken(t *testing.T) {
	t.Parallel()

	auth := NewAuth(func(token string) bool { return token == "s3cr3t" })
	resp, err := auth.Unary()(ctxWithToken("s3cr3t"), nil, unaryInfo, okHandler)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if resp != "ok" {
		t.Fatalf("resp = %v, want ok", resp)
	}
}

func TestAuthStreamInterceptorRejectsInvalidToken(t *testing.T) {
	t.Parallel()

	auth := NewAuth(func(token string) bool { return token == "s3cr3t" })
	ss := &mockStream{ctx: context.Background()} // no metadata in context
	info := &grpc.StreamServerInfo{FullMethod: "/example.Svc/Stream"}
	handler := func(_ any, _ grpc.ServerStream) error { return nil }

	err := auth.Stream()(nil, ss, info, handler)
	if status.Code(err) != codes.Unauthenticated {
		t.Fatalf("err = %v, want Unauthenticated", err)
	}
}

func TestMetricsInterceptorCountsCallsAndErrors(t *testing.T) {
	t.Parallel()

	m := NewMetrics()
	intercept := m.Unary()

	// Two successful calls.
	intercept(context.Background(), nil, unaryInfo, okHandler)
	intercept(context.Background(), nil, unaryInfo, okHandler)
	// One error call.
	intercept(context.Background(), nil, unaryInfo, errHandler)

	calls, errs := m.Snapshot(unaryInfo.FullMethod)
	if calls != 3 || errs != 1 {
		t.Fatalf("calls=%d errs=%d, want calls=3 errs=1", calls, errs)
	}
}

func TestMetricsInterceptorUnknownMethodReturnsZero(t *testing.T) {
	t.Parallel()

	m := NewMetrics()
	calls, errs := m.Snapshot("/never/called")
	if calls != 0 || errs != 0 {
		t.Fatalf("want (0, 0) for unknown method, got (%d, %d)", calls, errs)
	}
}

func TestRecoveryInterceptorCatchesPanic(t *testing.T) {
	t.Parallel()

	_, err := RecoveryUnary()(context.Background(), nil, unaryInfo, panicHandler)
	if status.Code(err) != codes.Internal {
		t.Fatalf("err = %v, want Internal", err)
	}
}

func TestChainUnaryExecutionOrder(t *testing.T) {
	t.Parallel()

	var order []string
	makeInterceptor := func(name string) grpc.UnaryServerInterceptor {
		return func(ctx context.Context, req any, info *grpc.UnaryServerInfo, handler grpc.UnaryHandler) (any, error) {
			order = append(order, name+":before")
			resp, err := handler(ctx, req)
			order = append(order, name+":after")
			return resp, err
		}
	}

	chain := ChainUnary(makeInterceptor("A"), makeInterceptor("B"), makeInterceptor("C"))
	chain(context.Background(), nil, unaryInfo, okHandler)

	want := []string{"A:before", "B:before", "C:before", "C:after", "B:after", "A:after"}
	if fmt.Sprint(order) != fmt.Sprint(want) {
		t.Fatalf("order = %v, want %v", order, want)
	}
}

func TestLoggingInterceptorPassesThroughResponse(t *testing.T) {
	t.Parallel()

	logger := slog.New(slog.NewTextHandler(noopWriter{}, nil))
	l := NewLogging(logger)

	resp, err := l.Unary()(context.Background(), nil, unaryInfo, okHandler)
	if err != nil || resp != "ok" {
		t.Fatalf("resp=%v err=%v, want ok/nil", resp, err)
	}
}

// noopWriter discards log output so tests stay silent.
type noopWriter struct{}

func (noopWriter) Write(p []byte) (int, error) { return len(p), nil }

// Your turn: add TestMetricsInterceptorDistinctMethods that calls the interceptor
// with two different FullMethod values ("/svc/A" and "/svc/B") and asserts that
// Snapshot("/svc/B") returns (0, 0) after only "/svc/A" has been called.

// ExampleChainUnary demonstrates building a chain and invoking it without a gRPC server.
func ExampleChainUnary() {
	auth := NewAuth(func(token string) bool { return token == "s3cr3t" })
	metrics := NewMetrics()

	chain := ChainUnary(auth.Unary(), metrics.Unary())

	md := metadata.Pairs("authorization", "Bearer s3cr3t")
	ctx := metadata.NewIncomingContext(context.Background(), md)
	info := &grpc.UnaryServerInfo{FullMethod: "/svc/Hello"}
	handler := func(_ context.Context, _ any) (any, error) { return "pong", nil }

	resp, err := chain(ctx, nil, info, handler)
	calls, _ := metrics.Snapshot("/svc/Hello")
	fmt.Println(resp, err, calls)
	// Output: pong <nil> 1
}
```

### cmd/demo/main.go

Create `cmd/demo/main.go`. This shows the same interceptors attached to a real `grpc.NewServer` and also demonstrates calling the chain directly:

```go
package main

import (
	"context"
	"fmt"
	"log/slog"
	"net"
	"os"

	"google.golang.org/grpc"
	"google.golang.org/grpc/metadata"
	"google.golang.org/grpc/status"

	mw "example.com/grpcmiddleware"
)

func main() {
	logger := slog.New(slog.NewTextHandler(os.Stderr, &slog.HandlerOptions{Level: slog.LevelWarn}))

	auth := mw.NewAuth(func(token string) bool { return token == "demo-secret" })
	metrics := mw.NewMetrics()
	logging := mw.NewLogging(logger)

	chain := mw.ChainUnary(
		mw.RecoveryUnary(),
		logging.Unary(),
		auth.Unary(),
		metrics.Unary(),
	)

	info := &grpc.UnaryServerInfo{FullMethod: "/demo.Service/Hello"}
	handler := func(_ context.Context, req any) (any, error) {
		return fmt.Sprintf("Hello, %v!", req), nil
	}

	// Case 1: no auth metadata — expect Unauthenticated.
	_, err := chain(context.Background(), "World", info, handler)
	fmt.Printf("no auth     -> %s\n", status.Code(err))

	// Case 2: valid auth — expect a response.
	md := metadata.Pairs("authorization", "Bearer demo-secret")
	ctx := metadata.NewIncomingContext(context.Background(), md)
	resp, err := chain(ctx, "World", info, handler)
	fmt.Printf("valid auth  -> resp=%v err=%v\n", resp, err)

	// Case 3: panic in handler — expect Internal.
	panicHandler := func(_ context.Context, _ any) (any, error) { panic("nil pointer") }
	_, err = chain(ctx, nil, info, panicHandler)
	fmt.Printf("panic       -> %s\n", status.Code(err))

	// Metrics: two calls reached the handler (case 2 and case 3), no errors from the handler.
	calls, errs := metrics.Snapshot(info.FullMethod)
	fmt.Printf("metrics     -> calls=%d errors=%d\n", calls, errs)

	// Attach the same interceptors to a real gRPC server.
	srv := grpc.NewServer(
		grpc.ChainUnaryInterceptor(
			mw.RecoveryUnary(),
			logging.Unary(),
			auth.Unary(),
			metrics.Unary(),
		),
		grpc.ChainStreamInterceptor(
			logging.Stream(),
			auth.Stream(),
		),
	)

	lis, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		slog.Error("listen failed", "err", err)
		os.Exit(1)
	}
	fmt.Printf("server ready on %s (demo: not serving)\n", lis.Addr())
	lis.Close()
	srv.Stop()
}
```

Run the demo:

```bash
go run ./cmd/demo
```

Expected output (the address port is random):

```
no auth     -> Unauthenticated
valid auth  -> resp=Hello, World! err=<nil>
panic       -> Internal
metrics     -> calls=2 errors=0
server ready on 127.0.0.1:NNNNN (demo: not serving)
```

## Common Mistakes

### Using a Capital Key in metadata.Get

Wrong: `md.Get("Authorization")` appears to work in unit tests because `metadata.Get` lowercases the key argument before lookup. However, calling the same code with `md["Authorization"]` (map indexing) returns nothing because the map key is always stored in lowercase after transmission.

Fix: always use `md.Get("authorization")` (lowercase) through the API, and never index `metadata.MD` directly with a mixed-case key.

### Forgetting to Call handler in a Non-Rejecting Interceptor

Wrong:

```go
func badLogging(ctx context.Context, req any, info *grpc.UnaryServerInfo, handler grpc.UnaryHandler) (any, error) {
	slog.Info("rpc start", "method", info.FullMethod)
	// forgot to call handler — request is silently dropped
	return nil, nil
}
```

Fix: every interceptor that does not intentionally short-circuit must call `handler(ctx, req)` and return its results:

```go
resp, err := handler(ctx, req)
slog.Info("rpc end", "method", info.FullMethod, "code", status.Code(err))
return resp, err
```

### Recovery Interceptor Not Outermost

Wrong: placing recovery after logging or auth:

```go
grpc.ChainUnaryInterceptor(logging.Unary(), mw.RecoveryUnary(), auth.Unary())
```

A panic inside `logging.Unary()` is not covered by the recovery interceptor because recovery runs after logging in the call chain. The server goroutine crashes.

Fix: recovery is always `interceptors[0]`:

```go
grpc.ChainUnaryInterceptor(mw.RecoveryUnary(), logging.Unary(), auth.Unary())
```

### Named Return Required for Recovery

Wrong:

```go
func RecoveryUnary() grpc.UnaryServerInterceptor {
	return func(ctx context.Context, req any, info *grpc.UnaryServerInfo, handler grpc.UnaryHandler) (any, error) {
		defer func() {
			if r := recover(); r != nil {
				// err is a new local variable here, not the return value
				err := status.Errorf(codes.Internal, "internal server error")
				_ = err // silently discarded
			}
		}()
		return handler(ctx, req)
	}
}
```

Fix: declare named returns `(resp any, err error)` so the deferred assignment reaches the caller:

```go
) (resp any, err error) {
	defer func() {
		if r := recover(); r != nil {
			err = status.Errorf(codes.Internal, "internal server error")
		}
	}()
```

## Verification

From `~/go-exercises/grpcmiddleware`:

```bash
test -z "$(gofmt -l .)"
go vet ./...
go build ./...
go test -count=1 -race ./...
go run ./cmd/demo
```

All five must pass. `go test` is the verification — the `ExampleChainUnary` output block is auto-checked by the test runner.

## Summary

- A `grpc.UnaryServerInterceptor` is a function; call it directly in tests — no gRPC server needed.
- Unary interceptors receive and return `(any, error)`; stream interceptors wrap `grpc.ServerStream` and return only `error`.
- Execution order matches the argument order of `grpc.ChainUnaryInterceptor`; recovery must be first.
- `metadata.FromIncomingContext(ctx)` retrieves request-scoped key-value pairs; keys are always lowercase.
- gRPC status errors are checked with `status.Code(err)`, not `errors.Is`.
- The named-return pattern is required for a deferred function to assign the error return after a panic.
- Client interceptors inject outgoing metadata with `metadata.AppendToOutgoingContext`.

## What's Next

Next: [Custom HTTP Transport](../15-custom-http-transport/15-custom-http-transport.md).

## Resources

- [gRPC Go interceptors documentation](https://grpc.io/docs/languages/go/interceptors/)
- [grpc package — UnaryServerInterceptor, ChainUnaryInterceptor](https://pkg.go.dev/google.golang.org/grpc)
- [grpc/metadata package — FromIncomingContext, AppendToOutgoingContext](https://pkg.go.dev/google.golang.org/grpc/metadata)
- [grpc/status and grpc/codes packages](https://pkg.go.dev/google.golang.org/grpc/status)
- [grpc-go metadata interceptor example](https://github.com/grpc/grpc-go/blob/master/examples/features/metadata_interceptor/README.md)
