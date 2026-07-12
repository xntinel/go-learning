# 8. Distributed Tracing Context

Distributed tracing solves one of the hardest problems in microservices: correlating a single user request that fans out across 5, 10, or 50 services. Without trace propagation you have isolated logs per service with no shared key. With propagation every span created anywhere in a request lifetime shares one trace ID, giving you an end-to-end flame graph in tools like Jaeger or Grafana Tempo.

The hard part is not starting spans inside a single process — that is straightforward OpenTelemetry SDK usage. The hard part is the boundary crossing: injecting trace state into outgoing HTTP headers, extracting it from incoming headers, propagating W3C baggage alongside it, and knowing when to use a linked span instead of a child span (async flows, message queues). This lesson builds all three of those concerns from scratch.

```text
tracing/
  go.mod
  tracing.go          -- initTracer, propagation setup
  middleware.go       -- server-side extract middleware
  client.go           -- instrumented HTTP client
  tracing_test.go     -- unit tests using httptest + MapCarrier
  cmd/demo/main.go    -- runnable three-service demo
```

## Concepts

### W3C TraceContext: the standard header format

The W3C TraceContext specification defines two HTTP headers:

- `traceparent` carries the essential propagation fields:
  `<version>-<trace-id>-<parent-id>-<trace-flags>`
  For example: `00-a0892f3577b34da6a3ce929d0e0e4736-f03067aa0ba902b7-01`
  The version is `00`; trace-id is 128-bit hex; parent-id is the current span ID (64-bit hex); trace-flags is `01` when the span is sampled.
- `tracestate` carries vendor-specific key-value pairs for multi-vendor interoperability.

OpenTelemetry's `propagation.TraceContext{}` implements inject and extract for both headers. Inject serializes the span context from the Go `context.Context` into carrier headers; extract deserializes headers back into a span context and attaches it to a new `context.Context` as the parent.

### Propagators and carriers

A `propagation.TextMapPropagator` has two methods:

```go
Inject(ctx context.Context, carrier TextMapCarrier)
Extract(ctx context.Context, carrier TextMapCarrier) context.Context
```

A `TextMapCarrier` is any key-value store that can hold headers. The SDK ships two concrete carriers:

- `propagation.MapCarrier` (a `map[string]string`) — useful in tests to inspect what was injected without a real HTTP round-trip.
- `propagation.HeaderCarrier` (a thin wrapper over `http.Header`) — used in production middleware.

The global propagator is set with `otel.SetTextMapPropagator`. Any code that calls `otel.GetTextMapPropagator().Extract(ctx, carrier)` picks it up automatically. `otelhttp.NewHandler` and `otelhttp.NewTransport` both call the global propagator, so setting it once in `main` covers all HTTP traffic.

### Baggage: business context across service boundaries

W3C Baggage is a parallel propagation mechanism. While `traceparent` carries only trace identity, baggage carries arbitrary key-value pairs (customer ID, tenant ID, feature flag values). It travels in a `baggage` HTTP header:

```
baggage: customer.id=cust-42,tenant.id=acme
```

The Go API:

```go
member, err := baggage.NewMember("customer.id", "cust-42")
bag, err    := baggage.New(member)
ctx          = baggage.ContextWithBaggage(ctx, bag)

// downstream service reads it back:
bag     = baggage.FromContext(ctx)
val     = bag.Member("customer.id").Value()
```

`baggage.NewMember` validates that key and value conform to W3C syntax. Use only ASCII-printable, percent-encoded values; never put secrets or PII in baggage — it travels in plain HTTP headers visible to every service and proxy.

### Parent-child spans vs. span links

A child span's parent is the span that caused it synchronously, in the same call path. Parent-child is the default: pass the context carrying the current span into `tracer.Start` and the SDK records the relationship automatically.

A span link is for asynchronous or loosely-coupled relationships: the span that publishes to a message queue and the span that processes the message are causally related but are not in the same call stack and may run in different processes at different times. Creating the consumer span as a child of the producer would corrupt the latency data (the consumer's duration is not part of the producer's response time). Instead:

```go
link := trace.Link{
	SpanContext: producerSpan.SpanContext(),
	Attributes:  []attribute.KeyValue{
		attribute.String("link.reason", "async-event"),
	},
}
_, consumerSpan := tracer.Start(context.Background(), "consume-order-event",
	trace.WithLinks(link),
	trace.WithNewRoot(),    // independent trace root
)
```

`trace.WithNewRoot()` ensures the consumer span does not inherit the producer's trace ID as its parent, only the link. The link is recorded in the span's `Links` field and is visible in tracing UIs as a cross-trace arrow.

### What otelhttp does automatically

`otelhttp.NewHandler(h, "operation")` wraps an `http.Handler` and:
1. Calls `otel.GetTextMapPropagator().Extract` on `r.Header` before invoking `h`.
2. Attaches the extracted span context as the parent of a new server span named `"operation"`.
3. Ends the span when the handler returns.

`otelhttp.NewTransport(base)` wraps `http.RoundTripper` and:
1. Creates a client span named after the request method and URL.
2. Calls `otel.GetTextMapPropagator().Inject` to write `traceparent` and `baggage` into the outgoing request headers.
3. Ends the span when the response is received or an error occurs.

You get automatic propagation by using these two wrappers; you do not need to call inject/extract manually in handler or client code.

## Exercises

Set up the module. This lesson depends on external modules and cannot be compiled offline; see Verification.

```bash
mkdir -p go-solutions/30-production-patterns/08-distributed-tracing-context/08-distributed-tracing-context/cmd/demo
cd go-solutions/30-production-patterns/08-distributed-tracing-context/08-distributed-tracing-context
go get go.opentelemetry.io/otel@v1.44.0
go get go.opentelemetry.io/otel/sdk@v1.44.0
go get go.opentelemetry.io/otel/exporters/stdout/stdouttrace@v1.44.0
go get go.opentelemetry.io/contrib/instrumentation/net/http/otelhttp@v0.69.0
```

### Exercise 1: Tracer initialization and propagation setup

Create `tracing.go`. This file sets up the SDK tracer provider, registers the W3C TraceContext and Baggage propagators as the global propagator, and exposes a `Shutdown` function the caller must defer.

```go
package tracing

import (
	"context"
	"fmt"
	"io"

	"go.opentelemetry.io/otel"
	"go.opentelemetry.io/otel/exporters/stdout/stdouttrace"
	"go.opentelemetry.io/otel/propagation"
	"go.opentelemetry.io/otel/sdk/resource"
	sdktrace "go.opentelemetry.io/otel/sdk/trace"
	semconv "go.opentelemetry.io/otel/semconv/v1.26.0"
)

// Provider wraps the SDK TracerProvider and exposes a Shutdown method.
type Provider struct {
	tp *sdktrace.TracerProvider
}

// Shutdown flushes buffered spans and releases resources.
// Call it with defer in main after Init returns.
func (p *Provider) Shutdown(ctx context.Context) error {
	return p.tp.Shutdown(ctx)
}

// Init creates a TracerProvider that writes spans as JSON to w, registers it
// as the global provider, and sets the global propagator to W3C TraceContext
// + W3C Baggage.
//
// serviceName is recorded in every span as the service.name resource attribute.
// Pass os.Stdout for interactive demos; pass a bytes.Buffer in tests.
func Init(ctx context.Context, serviceName string, w io.Writer) (*Provider, error) {
	exp, err := stdouttrace.New(
		stdouttrace.WithWriter(w),
		stdouttrace.WithPrettyPrint(),
		stdouttrace.WithoutTimestamps(), // deterministic in tests
	)
	if err != nil {
		return nil, fmt.Errorf("tracing: create exporter: %w", err)
	}

	res, err := resource.New(ctx,
		resource.WithAttributes(semconv.ServiceName(serviceName)),
	)
	if err != nil {
		// resource.New can return a partial result with a non-nil error when
		// some detectors fail. Use the partial result rather than failing hard.
		res = resource.Default()
	}

	tp := sdktrace.NewTracerProvider(
		// WithSyncer uses SimpleSpanProcessor: spans are exported inline before
		// the span ends. Safe for demos and tests; use WithBatcher in production.
		sdktrace.WithSyncer(exp),
		sdktrace.WithResource(res),
	)

	otel.SetTracerProvider(tp)
	otel.SetTextMapPropagator(propagation.NewCompositeTextMapPropagator(
		propagation.TraceContext{}, // W3C traceparent / tracestate
		propagation.Baggage{},      // W3C baggage
	))

	return &Provider{tp: tp}, nil
}
```

`WithSyncer` is intentionally used here (vs. `WithBatcher`) because the demo is single-process and synchronous exports make span output readable. A production service uses `WithBatcher` with an OTLP exporter.

### Exercise 2: Instrumented HTTP client

Create `client.go`. The instrumented client wraps `http.DefaultTransport` with `otelhttp.NewTransport`, which injects `traceparent` and `baggage` headers on every outgoing request automatically.

```go
package tracing

import (
	"net/http"
	"time"

	"go.opentelemetry.io/contrib/instrumentation/net/http/otelhttp"
)

// NewClient returns an *http.Client whose transport injects W3C TraceContext
// and Baggage headers into every outgoing request.
//
// The client uses http.DefaultTransport as the base RoundTripper.
// Timeout defaults to 10 seconds if zero is passed.
func NewClient(timeout time.Duration) *http.Client {
	if timeout == 0 {
		timeout = 10 * time.Second
	}
	return &http.Client{
		Transport: otelhttp.NewTransport(http.DefaultTransport),
		Timeout:   timeout,
	}
}
```

### Exercise 3: Middleware helpers and a baggage accessor

Create `middleware.go`. This file provides `BaggageHandler`, which wraps any `http.Handler` with OpenTelemetry server-side instrumentation, and `WithBaggageMember` / `BaggageValue`, which set and read baggage in a context.

```go
package tracing

import (
	"context"
	"fmt"
	"net/http"

	"go.opentelemetry.io/contrib/instrumentation/net/http/otelhttp"
	"go.opentelemetry.io/otel/baggage"
)

// BaggageHandler wraps handler with otelhttp.NewHandler (which extracts trace
// context from incoming HTTP headers and creates a server span for each request).
func BaggageHandler(handler http.Handler, operation string) http.Handler {
	return otelhttp.NewHandler(handler, operation)
}

// WithBaggageMember returns a copy of ctx with key=value added to W3C Baggage.
// Returns an error if key or value violate W3C baggage syntax.
func WithBaggageMember(ctx context.Context, key, value string) (context.Context, error) {
	m, err := baggage.NewMember(key, value)
	if err != nil {
		return ctx, fmt.Errorf("tracing: invalid baggage member %q=%q: %w", key, value, err)
	}
	b, err := baggage.New(m)
	if err != nil {
		return ctx, fmt.Errorf("tracing: build baggage: %w", err)
	}
	return baggage.ContextWithBaggage(ctx, b), nil
}

// BaggageValue reads a single baggage member value from ctx.
// Returns "" when the key is absent.
func BaggageValue(ctx context.Context, key string) string {
	return baggage.FromContext(ctx).Member(key).Value()
}
```

### Exercise 4: Tests using MapCarrier and httptest

Create `tracing_test.go`. The tests are hermetic: no network, no running HTTP server for the core propagation tests. Propagation is tested by injecting into a `propagation.MapCarrier` and extracting from it. HTTP-level integration uses `httptest.NewServer`.

```go
package tracing_test

import (
	"bytes"
	"context"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"example.com/tracing"
	"go.opentelemetry.io/otel"
	"go.opentelemetry.io/otel/propagation"
	"go.opentelemetry.io/otel/trace"
)

func initTestProvider(t *testing.T) {
	t.Helper()
	_, err := tracing.Init(context.Background(), "test-service", &bytes.Buffer{})
	if err != nil {
		t.Fatalf("Init: %v", err)
	}
}

// TestPropagationRoundTrip verifies that a span context survives inject -> extract
// through a MapCarrier (no HTTP, no network).
func TestPropagationRoundTrip(t *testing.T) {
	t.Parallel()
	initTestProvider(t)

	tracer := otel.Tracer("test")
	ctx, span := tracer.Start(context.Background(), "root")
	defer span.End()

	// Inject into an in-memory carrier.
	carrier := propagation.MapCarrier{}
	otel.GetTextMapPropagator().Inject(ctx, carrier)

	// The traceparent header must be present.
	tp := carrier["traceparent"]
	if tp == "" {
		t.Fatal("traceparent header missing after inject")
	}
	// W3C format: 00-<traceID>-<spanID>-<flags>
	parts := strings.Split(tp, "-")
	if len(parts) != 4 {
		t.Fatalf("traceparent %q: want 4 dash-separated parts, got %d", tp, len(parts))
	}

	// Extract from the carrier into a fresh context.
	ctx2 := otel.GetTextMapPropagator().Extract(context.Background(), carrier)
	sc := trace.SpanContextFromContext(ctx2)
	if !sc.IsValid() {
		t.Fatal("extracted span context is not valid")
	}
	if sc.TraceID() != span.SpanContext().TraceID() {
		t.Fatalf("trace IDs do not match: got %s, want %s",
			sc.TraceID(), span.SpanContext().TraceID())
	}
}

// TestBaggageRoundTrip verifies that a baggage member survives inject -> extract.
func TestBaggageRoundTrip(t *testing.T) {
	t.Parallel()
	initTestProvider(t)

	ctx := context.Background()
	var err error
	ctx, err = tracing.WithBaggageMember(ctx, "customer.id", "cust-42")
	if err != nil {
		t.Fatalf("WithBaggageMember: %v", err)
	}

	carrier := propagation.MapCarrier{}
	otel.GetTextMapPropagator().Inject(ctx, carrier)

	if carrier["baggage"] == "" {
		t.Fatal("baggage header missing after inject")
	}

	ctx2 := otel.GetTextMapPropagator().Extract(context.Background(), carrier)
	got := tracing.BaggageValue(ctx2, "customer.id")
	if got != "cust-42" {
		t.Fatalf("BaggageValue = %q, want %q", got, "cust-42")
	}
}

// TestBaggageValueAbsent verifies that BaggageValue returns "" for a missing key.
func TestBaggageValueAbsent(t *testing.T) {
	t.Parallel()
	got := tracing.BaggageValue(context.Background(), "no-such-key")
	if got != "" {
		t.Fatalf("BaggageValue for absent key = %q, want empty", got)
	}
}

// TestWithBaggageMemberInvalidKey verifies that an invalid W3C baggage key
// returns a non-nil error and that the original context is returned unchanged.
func TestWithBaggageMemberInvalidKey(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	_, err := tracing.WithBaggageMember(ctx, "", "value")
	if err == nil {
		t.Fatal("expected error for empty baggage key, got nil")
	}
}

// TestBaggageHandlerExtractsContext verifies that BaggageHandler extracts
// the trace context from incoming HTTP headers and makes it available in
// the handler's request context.
func TestBaggageHandlerExtractsContext(t *testing.T) {
	t.Parallel()
	initTestProvider(t)

	tracer := otel.Tracer("test-client")

	var gotTraceID trace.TraceID
	inner := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		sc := trace.SpanContextFromContext(r.Context())
		gotTraceID = sc.TraceID()
		w.WriteHeader(http.StatusOK)
	})

	srv := httptest.NewServer(tracing.BaggageHandler(inner, "test-op"))
	defer srv.Close()

	// Create a span and inject its context into request headers manually
	// to simulate what otelhttp.NewTransport would do.
	ctx, span := tracer.Start(context.Background(), "client-root")
	defer span.End()

	wantTraceID := span.SpanContext().TraceID()

	req, _ := http.NewRequestWithContext(ctx, http.MethodGet, srv.URL, nil)
	otel.GetTextMapPropagator().Inject(ctx, propagation.HeaderCarrier(req.Header))

	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("request: %v", err)
	}
	resp.Body.Close()

	if gotTraceID != wantTraceID {
		t.Fatalf("handler trace ID = %s, want %s", gotTraceID, wantTraceID)
	}
}

// ExampleBaggageValue demonstrates reading a baggage value from a context.
func ExampleBaggageValue() {
	// In production this context comes from a handler's r.Context() after
	// otelhttp.NewHandler has extracted the incoming baggage header.
	// For the example we populate it directly.
	ctx := context.Background()
	ctx, _ = tracing.WithBaggageMember(ctx, "tenant.id", "acme")

	val := tracing.BaggageValue(ctx, "tenant.id")
	fmt.Println(val)
	// Output: acme
}
```

### Exercise 5: Three-service demo

Create `cmd/demo/main.go`. The demo starts three in-process HTTP servers that propagate trace context across HTTP boundaries and prints spans to stdout. Run it with `go run ./cmd/demo`.

```go
package main

import (
	"context"
	"encoding/json"
	"fmt"
	"log"
	"math/rand"
	"net/http"
	"net/http/httptest"
	"os"
	"time"

	"example.com/tracing"
	"go.opentelemetry.io/otel"
	"go.opentelemetry.io/otel/attribute"
	"go.opentelemetry.io/otel/trace"
)

func main() {
	ctx := context.Background()

	provider, err := tracing.Init(ctx, "api-gateway", os.Stdout)
	if err != nil {
		log.Fatalf("tracing.Init: %v", err)
	}
	defer func() {
		if err := provider.Shutdown(ctx); err != nil {
			log.Printf("tracing shutdown: %v", err)
		}
	}()

	client := tracing.NewClient(10 * time.Second)

	// --- Payment Service ---
	paymentMux := http.NewServeMux()
	paymentMux.HandleFunc("POST /charge", func(w http.ResponseWriter, r *http.Request) {
		span := trace.SpanFromContext(r.Context())
		customerID := tracing.BaggageValue(r.Context(), "customer.id")
		span.SetAttributes(
			attribute.String("customer.id", customerID),
			attribute.Float64("payment.amount", float64(rand.Intn(10000))/100.0),
		)
		time.Sleep(time.Duration(30+rand.Intn(50)) * time.Millisecond)
		span.AddEvent("payment.charged")

		w.Header().Set("Content-Type", "application/json")
		if err := json.NewEncoder(w).Encode(map[string]string{
			"status":      "charged",
			"customer_id": customerID,
		}); err != nil {
			log.Printf("payment encode: %v", err)
		}
	})

	paymentSrv := httptest.NewServer(tracing.BaggageHandler(paymentMux, "payment-service"))
	defer paymentSrv.Close()

	// --- Order Service ---
	orderMux := http.NewServeMux()
	orderMux.HandleFunc("POST /orders", func(w http.ResponseWriter, r *http.Request) {
		ctx := r.Context()
		tracer := otel.Tracer("order-service")

		// Synchronous child span: process-order is causally part of this request.
		ctx, processSpan := tracer.Start(ctx, "process-order")
		orderID := fmt.Sprintf("ORD-%06d", rand.Intn(999999))
		processSpan.SetAttributes(attribute.String("order.id", orderID))
		time.Sleep(20 * time.Millisecond)
		processSpan.End()

		// Call Payment Service; the instrumented client injects traceparent + baggage.
		req, err := http.NewRequestWithContext(ctx, http.MethodPost, paymentSrv.URL+"/charge", nil)
		if err != nil {
			http.Error(w, "build request: "+err.Error(), http.StatusInternalServerError)
			return
		}
		resp, err := client.Do(req)
		if err != nil {
			http.Error(w, "payment failed: "+err.Error(), http.StatusBadGateway)
			return
		}
		resp.Body.Close()

		// Asynchronous span: emit-order-event is linked, not a child.
		// It represents work that will happen later in a separate execution context.
		parentSC := trace.SpanFromContext(ctx).SpanContext()
		_, asyncSpan := tracer.Start(context.Background(), "emit-order-event",
			trace.WithLinks(trace.Link{
				SpanContext: parentSC,
				Attributes: []attribute.KeyValue{
					attribute.String("link.reason", "async-event"),
				},
			}),
			trace.WithNewRoot(), // independent root: not a child
		)
		asyncSpan.SetAttributes(attribute.String("event.type", "order.created"))
		time.Sleep(5 * time.Millisecond)
		asyncSpan.End()

		w.Header().Set("Content-Type", "application/json")
		if err := json.NewEncoder(w).Encode(map[string]string{"order_id": orderID}); err != nil {
			log.Printf("order encode: %v", err)
		}
	})

	orderSrv := httptest.NewServer(tracing.BaggageHandler(orderMux, "order-service"))
	defer orderSrv.Close()

	// --- API Gateway: one request to drive the demo ---
	gatewayTracer := otel.Tracer("api-gateway")
	ctx, rootSpan := gatewayTracer.Start(ctx, "POST /api/orders")
	defer rootSpan.End()

	ctx, err = tracing.WithBaggageMember(ctx, "customer.id", "cust-42")
	if err != nil {
		log.Fatalf("WithBaggageMember: %v", err)
	}

	req, err := http.NewRequestWithContext(ctx, http.MethodPost, orderSrv.URL+"/orders", nil)
	if err != nil {
		log.Fatalf("build gateway request: %v", err)
	}
	resp, err := client.Do(req)
	if err != nil {
		log.Fatalf("gateway call: %v", err)
	}
	resp.Body.Close()

	log.Println("demo complete; span output above")
}
```

Note: the demo imports `net/http/httptest` for convenience (it starts real HTTP servers on random ports so there are no port conflicts). In a real multi-process deployment each service runs in its own binary and uses `http.ListenAndServe`.

## Common Mistakes

### Setting the global propagator after creating spans

Wrong: calling `otel.SetTextMapPropagator` after the first span is created, or not setting it at all and relying on the no-op default.

What happens: the SDK's no-op propagator writes nothing into outgoing headers and reads nothing from incoming headers. All spans are disconnected; you get one trace per service instead of one trace across all services.

Fix: call `otel.SetTextMapPropagator(propagation.NewCompositeTextMapPropagator(...))` in `main`, before any HTTP handler or client is invoked.

### Using `context.Background()` for the outgoing request instead of the current context

Wrong:
```go
req, _ := http.NewRequestWithContext(context.Background(), "POST", url, nil)
```

What happens: the outgoing request carries no trace context. The downstream service sees no `traceparent` header and starts a new root span, breaking the trace.

Fix:
```go
req, _ := http.NewRequestWithContext(ctx, "POST", url, nil)
```

Always thread the current request's context through to outgoing calls. The instrumented transport then injects the parent span's context into the request headers automatically.

### Creating an async span as a child instead of a linked span

Wrong:
```go
// ctx still contains the HTTP handler's span as parent
_, asyncSpan := tracer.Start(ctx, "consume-event") // child of the HTTP handler
asyncSpan.End()
```

What happens: the async span's duration is recorded as part of the HTTP handler's latency. Trace visualizations show artificially long handlers. If the consumer runs in a separate goroutine after the handler returns, the parent span may already be ended.

Fix: use `context.Background()` with `trace.WithNewRoot()` and a `trace.WithLinks(...)` linking back to the producer's span context:
```go
_, asyncSpan := tracer.Start(context.Background(), "consume-event",
	trace.WithNewRoot(),
	trace.WithLinks(trace.Link{SpanContext: producerSC}),
)
```

### Putting sensitive data in baggage

Wrong: `WithBaggageMember(ctx, "auth-token", token)`

What happens: the `baggage` header travels in plain HTTP and is visible to every service, proxy, and load balancer in the path. It may be logged by infrastructure you do not control.

Fix: use baggage only for non-sensitive correlation identifiers (customer ID, request ID, A/B variant). Pass secrets in encrypted headers or request bodies.

### Checking only string equality on errors from NewMember

Wrong:
```go
if err != nil && err.Error() == "key is not valid" { ... }
```

Fix: baggage errors do not use sentinel errors; check `err != nil` and wrap with `%w` in your own layer to let callers use `errors.Is` on your error types.

## Verification

This lesson uses external modules (`go.opentelemetry.io/otel`, `otel/sdk`, `otel/exporters/stdout/stdouttrace`, `otel/contrib/instrumentation/net/http/otelhttp`). It cannot be gated fully offline. Apply the capstone bar: prose validated against code with §15; gofmt and go vet applied to extractable blocks.

From `~/go-exercises/tracing` after `go get` (requires network access once to download modules):

```bash
test -z "$(gofmt -l .)"
go vet ./...
go build ./...
go test -count=1 -race ./...
go run ./cmd/demo
```

All five must pass. The `go test` output should show five passing tests. The `go run ./cmd/demo` output should show JSON span objects where all synchronous spans share one TraceID, while the linked async span (emit-order-event) has a NEW TraceID and a `Links` field referencing the original root trace ID.

Your turn: add `TestNewClientHasTimeout` that calls `tracing.NewClient(5*time.Second)` and asserts the returned client's `Timeout` field equals `5*time.Second`. Then add `TestNewClientDefaultTimeout` that calls `tracing.NewClient(0)` and asserts `Timeout == 10*time.Second`.

## Summary

- W3C TraceContext (`traceparent`/`tracestate`) is the standard format for distributed trace propagation across services and vendors.
- `otel.SetTextMapPropagator` must be called with a composite propagator (TraceContext + Baggage) before any spans are created or any HTTP traffic flows.
- `otelhttp.NewHandler` extracts incoming trace context and creates server spans; `otelhttp.NewTransport` injects outgoing trace context and creates client spans.
- Baggage carries non-sensitive business context (customer ID, tenant ID) in the `baggage` HTTP header alongside `traceparent`; it is readable by every service in the call chain.
- Linked spans (`trace.WithLinks` + `trace.WithNewRoot`) represent async relationships where parent-child would corrupt latency data; the producer and consumer remain in different traces connected by a cross-trace link.
- `propagation.MapCarrier` enables hermetic unit tests for inject/extract logic without any HTTP round-trip.

## What's Next

Next: [Circuit Breaker with Half-Open State](../09-circuit-breaker-half-open/09-circuit-breaker-half-open.md).

## Resources

- [W3C Trace Context specification](https://www.w3.org/TR/trace-context/) -- defines `traceparent` and `tracestate` header format
- [W3C Baggage specification](https://www.w3.org/TR/baggage/) -- defines the `baggage` header format and member syntax
- [OpenTelemetry context propagation concepts](https://opentelemetry.io/docs/concepts/context-propagation/) -- propagators, carriers, inject/extract lifecycle
- [pkg.go.dev: go.opentelemetry.io/otel/propagation](https://pkg.go.dev/go.opentelemetry.io/otel/propagation) -- MapCarrier, HeaderCarrier, TraceContext, Baggage, NewCompositeTextMapPropagator
- [pkg.go.dev: go.opentelemetry.io/contrib/instrumentation/net/http/otelhttp](https://pkg.go.dev/go.opentelemetry.io/contrib/instrumentation/net/http/otelhttp) -- NewHandler, NewTransport signatures and options
