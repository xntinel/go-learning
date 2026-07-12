# 11. OpenTelemetry Collector Integration

OpenTelemetry is the CNCF standard for vendor-neutral observability. Unlike Prometheus (pull-based, metrics only), the OTel SDK pushes traces, metrics, and logs through a Collector that decouples the application from any specific backend. This lesson focuses on the hard parts: wiring the TracerProvider correctly so it shuts down cleanly, propagating trace context across HTTP hops without losing the parent span, and testing span assertions deterministically with an in-memory exporter — without a real Collector or network.

The lesson builds a small order-service package. The package is not compilable offline because it imports `go.opentelemetry.io/otel` and its contrib modules, which require `go get`. All code is validated with `gofmt` and `go vet` on extractable portions; the network build and integration test are deferred to a networked environment.

```text
ordersvc/
  go.mod
  otel.go          -- TracerProvider setup and shutdown
  service.go       -- business logic with custom spans
  service_test.go  -- span assertions using InMemoryExporter
  cmd/demo/main.go -- runnable HTTP server demo
```

## Concepts

### The TracerProvider Lifecycle

The `sdktrace.TracerProvider` is the root object that owns the exporter connection, the batch queue, and all span data. It must be shut down exactly once before the process exits; the `Shutdown(ctx)` call flushes the batch queue and closes the exporter.

The correct pattern is to register shutdown with `defer` in `main`:

```go
tp, err := initTracer(ctx)
if err != nil {
	log.Fatal(err)
}
defer func() {
	if err := tp.Shutdown(context.Background()); err != nil {
		log.Printf("tracer shutdown: %v", err)
	}
}()
otel.SetTracerProvider(tp)
```

If `Shutdown` is not called, in-flight spans in the batch queue are lost. If it is called twice (common when `defer` races a signal handler), the second call returns an error.

### Batch vs Sync Processors

`trace.WithBatcher(exporter)` — the default for production — buffers spans in memory and exports them in batches. It is non-blocking on the hot path. `trace.NewSimpleSpanProcessor(exporter)` exports each span synchronously before `span.End()` returns; it is unsuitable for production throughput but is exactly right for tests because spans are available immediately after `End()`.

The `tracetest.NewInMemoryExporter()` is designed for tests. Pair it with a `SimpleSpanProcessor`:

```go
exporter := tracetest.NewInMemoryExporter()
tp := sdktrace.NewTracerProvider(
	sdktrace.WithSpanProcessor(sdktrace.NewSimpleSpanProcessor(exporter)),
)
```

### Resource Attributes

A `resource.Resource` identifies the entity that produced the spans — the service name, version, and deployment environment. It is set once on the `TracerProvider` and attached to every span. The semantic convention for service name is `semconv.ServiceNameKey`:

```go
res, err := resource.New(ctx,
	resource.WithAttributes(
		semconv.ServiceNameKey.String("order-service"),
		semconv.ServiceVersionKey.String("1.0.0"),
	),
)
```

`resource.New` merges the supplied attributes with the default process and SDK resource detected from the environment. Calling `resource.NewWithAttributes(semconv.SchemaURL, ...)` instead skips auto-detection and produces a bare resource; the difference matters when the Collector uses resource attributes for routing.

### Span Attributes, Events, and Status

Attributes describe what a span is doing (`order.id`, `db.statement`). Events mark something that happened within the span's lifetime (`cache.hit`, `payment.authorized`). Status signals success or failure to the Collector.

```go
ctx, span := tracer.Start(ctx, "getOrder",
	trace.WithAttributes(attribute.String("order.id", id)),
)
defer span.End()

span.AddEvent("cache.miss")

order, err := fetchFromDB(ctx, id)
if err != nil {
	span.RecordError(err)
	span.SetStatus(codes.Error, err.Error())
	return Order{}, fmt.Errorf("getOrder: %w", err)
}
span.SetStatus(codes.Ok, "")
```

`RecordError` adds the error as a structured event on the span; `SetStatus(codes.Error, ...)` sets the top-level status visible in the Collector UI. Both are required for error spans to surface correctly in backends like Jaeger.

### Context Propagation Across HTTP

The trace context (trace ID + span ID) travels as HTTP headers using the W3C Trace Context format (`traceparent`, `tracestate`). Two components handle this:

- `otelhttp.NewHandler` wraps an `http.Handler` to extract trace context from incoming requests and start a server-side span.
- `otelhttp.NewTransport` wraps `http.RoundTripper` to inject trace context into outgoing requests.

If you make an outgoing request without `otelhttp.NewTransport`, the downstream service receives no `traceparent` header and starts an unrelated root span, silently breaking the distributed trace.

```go
// incoming: extract context
mux.Handle("/api/orders/", otelhttp.NewHandler(handler, "orders"))

// outgoing: inject context
client := &http.Client{
	Transport: otelhttp.NewTransport(http.DefaultTransport),
}
req, _ := http.NewRequestWithContext(ctx, http.MethodGet, url, http.NoBody)
resp, err := client.Do(req) // traceparent header is set automatically
```

### OTLP gRPC Exporter

The OTel Collector speaks OTLP (OpenTelemetry Protocol) over gRPC (port 4317) or HTTP/protobuf (port 4318). The gRPC exporter is the default production choice:

```go
exp, err := otlptracegrpc.New(ctx,
	otlptracegrpc.WithEndpoint("otel-collector:4317"),
	otlptracegrpc.WithInsecure(), // remove for TLS in production
)
```

`WithInsecure()` disables TLS; in production, remove it and supply `WithTLSCredentials(creds)`. The endpoint defaults to `localhost:4317` if `WithEndpoint` is omitted; override it with the `OTEL_EXPORTER_OTLP_ENDPOINT` environment variable to avoid hardcoded addresses.

## Exercises

Set up the module (requires network access to `go get` OTel modules):

```bash
go get go.opentelemetry.io/otel@latest
go get go.opentelemetry.io/otel/sdk@latest
go get go.opentelemetry.io/otel/exporters/otlp/otlptrace/otlptracegrpc@latest
go get go.opentelemetry.io/otel/sdk/resource@latest
go get go.opentelemetry.io/otel/sdk/trace/tracetest@latest
go get go.opentelemetry.io/contrib/instrumentation/net/http/otelhttp@latest
```

### Exercise 1: TracerProvider Setup

Create `otel.go`. This file owns the provider lifecycle: it constructs the OTLP exporter, configures the resource, and returns a shutdown function that the caller must defer.

```go
package ordersvc

import (
	"context"
	"fmt"

	"go.opentelemetry.io/otel"
	"go.opentelemetry.io/otel/exporters/otlp/otlptrace/otlptracegrpc"
	"go.opentelemetry.io/otel/sdk/resource"
	sdktrace "go.opentelemetry.io/otel/sdk/trace"
	semconv "go.opentelemetry.io/otel/semconv/v1.26.0"
)

// InitTracer initialises the global TracerProvider backed by an OTLP gRPC
// exporter pointed at endpoint (e.g. "localhost:4317"). It returns a shutdown
// function that the caller must call before the process exits to flush queued
// spans. InitTracer sets the global provider via otel.SetTracerProvider so
// instrumentation libraries (otelhttp, etc.) pick it up without explicit wiring.
func InitTracer(ctx context.Context, endpoint, serviceName, serviceVersion string) (func(context.Context) error, error) {
	exp, err := otlptracegrpc.New(ctx,
		otlptracegrpc.WithEndpoint(endpoint),
		otlptracegrpc.WithInsecure(),
	)
	if err != nil {
		return nil, fmt.Errorf("ordersvc: create OTLP exporter: %w", err)
	}

	res, err := resource.New(ctx,
		resource.WithAttributes(
			semconv.ServiceNameKey.String(serviceName),
			semconv.ServiceVersionKey.String(serviceVersion),
		),
	)
	if err != nil {
		return nil, fmt.Errorf("ordersvc: create resource: %w", err)
	}

	tp := sdktrace.NewTracerProvider(
		sdktrace.WithBatcher(exp),
		sdktrace.WithResource(res),
	)
	otel.SetTracerProvider(tp)

	return tp.Shutdown, nil
}
```

Key design decisions:
- The function returns a `func(context.Context) error` rather than exposing `*sdktrace.TracerProvider`. The caller only needs to shut down; it should not call `Shutdown` twice.
- `WithBatcher` is used (not `WithSyncer`) so the export path is non-blocking.
- `resource.New` (not `resource.NewWithAttributes`) merges auto-detected process information with the supplied attributes.

### Exercise 2: Business Logic with Custom Spans

Create `service.go`. The service simulates an order-lookup that spans a cache check and a database query. Both operations create child spans so the trace shows the internal breakdown.

```go
package ordersvc

import (
	"context"
	"errors"
	"fmt"

	"go.opentelemetry.io/otel"
	"go.opentelemetry.io/otel/attribute"
	"go.opentelemetry.io/otel/codes"
	"go.opentelemetry.io/otel/trace"
)

// ErrNotFound is returned when an order does not exist.
var ErrNotFound = errors.New("order not found")

// Order holds a minimal order representation.
type Order struct {
	ID    string
	Items int
}

// Service provides order retrieval with OTel tracing.
type Service struct {
	tracer trace.Tracer
	// cache holds a simple in-memory map for the demo.
	cache map[string]Order
}

// NewService creates a Service. tracer should be obtained from
// otel.Tracer(instrumentationName) after calling InitTracer.
func NewService(tracer trace.Tracer) *Service {
	return &Service{
		tracer: tracer,
		cache:  make(map[string]Order),
	}
}

// Tracer returns the instrumentation tracer (exported for cmd/demo).
func (s *Service) Tracer() trace.Tracer { return s.tracer }

// GetOrder looks up an order by ID. It checks an in-memory cache first; on
// a miss it falls back to a simulated database query. Both operations are
// recorded as child spans so the Collector shows the breakdown.
func (s *Service) GetOrder(ctx context.Context, id string) (Order, error) {
	ctx, span := s.tracer.Start(ctx, "GetOrder",
		trace.WithAttributes(attribute.String("order.id", id)),
		trace.WithSpanKind(trace.SpanKindInternal),
	)
	defer span.End()

	if id == "" {
		err := fmt.Errorf("GetOrder: %w", ErrNotFound)
		span.RecordError(err)
		span.SetStatus(codes.Error, "empty order id")
		return Order{}, err
	}

	order, hit, err := s.checkCache(ctx, id)
	if err != nil {
		span.RecordError(err)
		span.SetStatus(codes.Error, err.Error())
		return Order{}, err
	}
	if hit {
		span.AddEvent("cache.hit", trace.WithAttributes(attribute.String("order.id", id)))
		span.SetStatus(codes.Ok, "")
		return order, nil
	}

	span.AddEvent("cache.miss")

	order, err = s.queryDB(ctx, id)
	if err != nil {
		span.RecordError(err)
		span.SetStatus(codes.Error, err.Error())
		return Order{}, err
	}

	s.cache[id] = order
	span.SetStatus(codes.Ok, "")
	return order, nil
}

// checkCache checks the in-memory cache. It records a child span so the cache
// latency is visible separately from the database latency.
func (s *Service) checkCache(ctx context.Context, id string) (Order, bool, error) {
	_, span := s.tracer.Start(ctx, "checkCache",
		trace.WithAttributes(attribute.String("cache.key", id)),
	)
	defer span.End()

	order, ok := s.cache[id]
	span.SetStatus(codes.Ok, "")
	return order, ok, nil
}

// queryDB simulates a database lookup. In a real service this would execute a
// SQL query and the span would carry db.statement and db.system attributes.
func (s *Service) queryDB(ctx context.Context, id string) (Order, error) {
	_, span := s.tracer.Start(ctx, "queryDB",
		trace.WithAttributes(
			attribute.String("db.system", "postgresql"),
			attribute.String("db.statement", "SELECT * FROM orders WHERE id = $1"),
			attribute.String("order.id", id),
		),
	)
	defer span.End()

	// Simulate: only the order "ord-1" exists.
	if id == "ord-1" {
		span.SetStatus(codes.Ok, "")
		return Order{ID: id, Items: 3}, nil
	}

	err := fmt.Errorf("queryDB: %w", ErrNotFound)
	span.RecordError(err)
	span.SetStatus(codes.Error, err.Error())
	return Order{}, err
}

// instrumentationName is the tracer scope that identifies this package.
const instrumentationName = "example.com/ordersvc"

// DefaultTracer returns a Tracer scoped to this package from the global provider.
func DefaultTracer() trace.Tracer {
	return otel.Tracer(instrumentationName)
}
```

### Exercise 3: Tests with InMemoryExporter

Create `service_test.go`. The `tracetest.InMemoryExporter` captures every span synchronously (via `SimpleSpanProcessor`) so tests can assert span names, attributes, events, and status codes deterministically without a network.

```go
package ordersvc

import (
	"context"
	"errors"
	"testing"

	"go.opentelemetry.io/otel/attribute"
	"go.opentelemetry.io/otel/codes"
	sdktrace "go.opentelemetry.io/otel/sdk/trace"
	"go.opentelemetry.io/otel/sdk/trace/tracetest"
)

// newTestProvider returns a TracerProvider backed by an InMemoryExporter and a
// SimpleSpanProcessor. Spans are available in the exporter immediately after
// span.End() returns — no batching delay.
func newTestProvider() (*sdktrace.TracerProvider, *tracetest.InMemoryExporter) {
	exporter := tracetest.NewInMemoryExporter()
	tp := sdktrace.NewTracerProvider(
		sdktrace.WithSpanProcessor(sdktrace.NewSimpleSpanProcessor(exporter)),
	)
	return tp, exporter
}

func TestGetOrderCacheHit(t *testing.T) {
	t.Parallel()

	tp, exporter := newTestProvider()
	tracer := tp.Tracer("test")

	svc := NewService(tracer)
	// Pre-populate the cache so the first call is a cache hit.
	svc.cache["ord-1"] = Order{ID: "ord-1", Items: 3}

	_, err := svc.GetOrder(context.Background(), "ord-1")
	if err != nil {
		t.Fatalf("GetOrder: %v", err)
	}

	spans := exporter.GetSpans()
	// Expect: GetOrder + checkCache (no queryDB on a hit).
	if len(spans) != 2 {
		t.Fatalf("want 2 spans, got %d", len(spans))
	}

	root := spans[1] // SimpleSpanProcessor records in End order; parent ends last.
	if root.Name != "GetOrder" {
		t.Errorf("root span name = %q, want GetOrder", root.Name)
	}
	if root.Status.Code != codes.Ok {
		t.Errorf("root status = %v, want Ok", root.Status.Code)
	}

	// Verify the cache.hit event is on the root span.
	var foundEvent bool
	for _, e := range root.Events {
		if e.Name == "cache.hit" {
			foundEvent = true
		}
	}
	if !foundEvent {
		t.Error("want cache.hit event on GetOrder span")
	}
}

func TestGetOrderCacheMissDBHit(t *testing.T) {
	t.Parallel()

	tp, exporter := newTestProvider()
	tracer := tp.Tracer("test")
	svc := NewService(tracer)

	order, err := svc.GetOrder(context.Background(), "ord-1")
	if err != nil {
		t.Fatalf("GetOrder: %v", err)
	}
	if order.Items != 3 {
		t.Errorf("Items = %d, want 3", order.Items)
	}

	spans := exporter.GetSpans()
	// Expect: queryDB + checkCache + GetOrder.
	if len(spans) != 3 {
		t.Fatalf("want 3 spans, got %d", len(spans))
	}

	// Find the queryDB span and verify its attributes.
	var dbSpan *tracetest.SpanStub
	for i := range spans {
		if spans[i].Name == "queryDB" {
			dbSpan = &spans[i]
		}
	}
	if dbSpan == nil {
		t.Fatal("queryDB span not found")
	}

	wantAttr := attribute.String("db.system", "postgresql")
	var found bool
	for _, kv := range dbSpan.Attributes {
		if kv == wantAttr {
			found = true
		}
	}
	if !found {
		t.Errorf("queryDB span missing attribute %v; got %v", wantAttr, dbSpan.Attributes)
	}
}

func TestGetOrderNotFound(t *testing.T) {
	t.Parallel()

	tp, exporter := newTestProvider()
	tracer := tp.Tracer("test")
	svc := NewService(tracer)

	_, err := svc.GetOrder(context.Background(), "ord-999")
	if !errors.Is(err, ErrNotFound) {
		t.Fatalf("err = %v, want ErrNotFound", err)
	}

	spans := exporter.GetSpans()
	// Find the root GetOrder span and verify its status is Error.
	var root *tracetest.SpanStub
	for i := range spans {
		if spans[i].Name == "GetOrder" {
			root = &spans[i]
		}
	}
	if root == nil {
		t.Fatal("GetOrder span not found")
	}
	if root.Status.Code != codes.Error {
		t.Errorf("status = %v, want Error", root.Status.Code)
	}
}

func TestGetOrderEmptyID(t *testing.T) {
	t.Parallel()

	tp, exporter := newTestProvider()
	tracer := tp.Tracer("test")
	svc := NewService(tracer)

	_, err := svc.GetOrder(context.Background(), "")
	if !errors.Is(err, ErrNotFound) {
		t.Fatalf("err = %v, want ErrNotFound", err)
	}

	spans := exporter.GetSpans()
	if len(spans) == 0 {
		t.Fatal("want at least one span")
	}
	var root *tracetest.SpanStub
	for i := range spans {
		if spans[i].Name == "GetOrder" {
			root = &spans[i]
		}
	}
	if root == nil {
		t.Fatal("GetOrder span not found")
	}
	if root.Status.Code != codes.Error {
		t.Errorf("status = %v, want Error", root.Status.Code)
	}
}

func TestSpanParentChildRelationship(t *testing.T) {
	t.Parallel()

	tp, exporter := newTestProvider()
	tracer := tp.Tracer("test")
	svc := NewService(tracer)

	_, _ = svc.GetOrder(context.Background(), "ord-1")

	spans := exporter.GetSpans()
	// All child spans must share the same TraceID as the root.
	if len(spans) == 0 {
		t.Fatal("no spans recorded")
	}
	traceID := spans[0].SpanContext.TraceID()
	for _, sp := range spans {
		if sp.SpanContext.TraceID() != traceID {
			t.Errorf("span %q has TraceID %v, want %v", sp.Name, sp.SpanContext.TraceID(), traceID)
		}
	}
}

// Your turn: add TestCachePopulatedAfterDBHit that calls GetOrder("ord-1") twice
// on a fresh Service, asserts no error on both calls, and verifies that the
// second call produces only 2 spans (GetOrder + checkCache) instead of 3 —
// proving the result was cached after the first lookup.
```

### Exercise 4: HTTP Server Demo

Create `cmd/demo/main.go`. This is a runnable HTTP server that wires the TracerProvider, registers an `otelhttp`-instrumented handler, and shows how outgoing requests carry the trace context forward.

```go
package main

import (
	"context"
	"fmt"
	"io"
	"log"
	"log/slog"
	"net/http"
	"os"
	"os/signal"
	"syscall"

	"go.opentelemetry.io/contrib/instrumentation/net/http/otelhttp"
	"go.opentelemetry.io/otel"

	"example.com/ordersvc"
)

func main() {
	ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	defer stop()

	endpoint := os.Getenv("OTEL_EXPORTER_OTLP_ENDPOINT")
	if endpoint == "" {
		endpoint = "localhost:4317"
	}

	shutdown, err := ordersvc.InitTracer(ctx, endpoint, "order-service", "0.1.0")
	if err != nil {
		log.Fatalf("init tracer: %v", err)
	}
	defer func() {
		if err := shutdown(context.Background()); err != nil {
			slog.Error("tracer shutdown", "err", err)
		}
	}()

	tracer := otel.Tracer("example.com/ordersvc/cmd/demo")
	svc := ordersvc.NewService(tracer)

	// outClient propagates trace context on outgoing HTTP requests.
	outClient := &http.Client{
		Transport: otelhttp.NewTransport(http.DefaultTransport),
	}

	mux := http.NewServeMux()

	// GET /api/orders/{id}
	mux.Handle("/api/orders/", otelhttp.NewHandler(
		http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			id := r.URL.Path[len("/api/orders/"):]
			order, err := svc.GetOrder(r.Context(), id)
			if err != nil {
				http.Error(w, err.Error(), http.StatusNotFound)
				return
			}
			fmt.Fprintf(w, `{"id":%q,"items":%d}`, order.ID, order.Items)
		}),
		"GET /api/orders/{id}",
	))

	// GET /api/upstream — demonstrates injecting trace context into an outgoing call.
	mux.Handle("/api/upstream", otelhttp.NewHandler(
		http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			req, err := http.NewRequestWithContext(r.Context(), http.MethodGet, "https://httpbin.org/get", http.NoBody)
			if err != nil {
				http.Error(w, err.Error(), http.StatusInternalServerError)
				return
			}
			resp, err := outClient.Do(req)
			if err != nil {
				http.Error(w, err.Error(), http.StatusBadGateway)
				return
			}
			defer resp.Body.Close()
			body, _ := io.ReadAll(resp.Body)
			w.Header().Set("Content-Type", "application/json")
			w.Write(body)
		}),
		"GET /api/upstream",
	))

	srv := &http.Server{Addr: ":8080", Handler: mux}

	go func() {
		slog.Info("server listening", "addr", ":8080")
		if err := srv.ListenAndServe(); err != nil && err != http.ErrServerClosed {
			slog.Error("listen", "err", err)
			os.Exit(1)
		}
	}()

	<-ctx.Done()
	slog.Info("shutting down")
	_ = srv.Shutdown(context.Background())
}
```

Run with a local Collector:

```bash
# start the Collector (adjust the config path as needed)
docker run --rm -p 4317:4317 \
  -v $(pwd)/otel-collector-config.yaml:/etc/otel/config.yaml \
  otel/opentelemetry-collector:latest \
  --config /etc/otel/config.yaml &

OTEL_EXPORTER_OTLP_ENDPOINT=localhost:4317 go run ./cmd/demo &

# send a request — the span appears in the Collector output
curl http://localhost:8080/api/orders/ord-1
```

## Common Mistakes

### Not Deferring TracerProvider.Shutdown

Wrong: calling `InitTracer` in `main` and returning without ever calling `Shutdown`. The batch queue is discarded; spans from the last seconds before exit are lost.

What happens: requests complete, spans are written to the in-memory queue, the process exits, the queue is dropped. The Collector receives partial traces.

Fix: defer the shutdown function returned by `InitTracer` before any other deferred call so it runs last:

```go
shutdown, err := ordersvc.InitTracer(ctx, endpoint, "order-service", "0.1.0")
if err != nil {
	log.Fatal(err)
}
defer func() { _ = shutdown(context.Background()) }()
```

### Using WithBatcher in Tests

Wrong: creating the test `TracerProvider` with `sdktrace.WithBatcher(exporter)`. The batcher delays export; `exporter.GetSpans()` called immediately after `span.End()` returns zero spans.

What happens: the test asserts `len(spans) == 3` and gets 0, then flakes or sleeps.

Fix: use `sdktrace.NewSimpleSpanProcessor(exporter)` in tests. `SimpleSpanProcessor` exports synchronously inside `span.End()`, so spans are available immediately.

### Making Outgoing HTTP Requests Without otelhttp.NewTransport

Wrong:

```go
resp, err := http.Get("https://downstream/api")
```

What happens: no `traceparent` header is sent. The downstream service starts a new root span. The distributed trace is silently broken — both services show separate root spans with no parent-child link.

Fix:

```go
client := &http.Client{Transport: otelhttp.NewTransport(http.DefaultTransport)}
req, _ := http.NewRequestWithContext(ctx, http.MethodGet, "https://downstream/api", http.NoBody)
resp, err := client.Do(req)
```

The `otelhttp.NewTransport` reads the current span from `ctx` and injects `traceparent` into the request headers.

### Calling SetStatus Without RecordError (Or Vice Versa)

Wrong: calling only `span.SetStatus(codes.Error, msg)` without `span.RecordError(err)`.

What happens: the Collector marks the span as failed but there is no error event. Backends like Jaeger show the red status but no error message or stack trace in the span detail view.

Fix: always call both. `RecordError` creates a structured event with the error message and optional stack trace. `SetStatus` sets the top-level status that drives the red/green indicator in the UI.

### Capturing the Span From the Wrong Context

Wrong:

```go
ctx, span := tracer.Start(context.Background(), "getOrder") // fresh context
defer span.End()
order, err := queryDB(ctx, id)  // child span is rooted correctly ...
```

... but the caller passed `r.Context()` (containing the HTTP parent span), which is discarded.

What happens: `getOrder` becomes a root span disconnected from the HTTP request span.

Fix: always propagate the context received from the caller:

```go
func (s *Service) GetOrder(ctx context.Context, id string) (Order, error) {
	ctx, span := s.tracer.Start(ctx, "GetOrder") // ctx carries the parent
	defer span.End()
	// ...
}
```

## Verification

From `~/go-exercises/ordersvc`:

```bash
test -z "$(gofmt -l .)"
go vet ./...
go build ./...
go test -count=1 -race ./...
```

The test suite does not require a running Collector or Docker. The `InMemoryExporter` captures spans in process. All four commands must pass before pushing.

For integration testing with a real Collector:

```bash
docker run --rm -p 4317:4317 otel/opentelemetry-collector:latest &
OTEL_EXPORTER_OTLP_ENDPOINT=localhost:4317 go run ./cmd/demo &
curl http://localhost:8080/api/orders/ord-1
# inspect Collector stdout for the exported span JSON
```

Add a test: `TestCachePopulatedAfterDBHit` — call `svc.GetOrder(ctx, "ord-1")` twice on a fresh `Service`, assert both return no error, then assert the second call produced only 2 spans (verifying the cache was populated after the first lookup). This pins the caching contract.

## Summary

- The `TracerProvider` owns the exporter connection and batch queue. Shut it down exactly once with `defer tp.Shutdown(ctx)` or the returned shutdown function; unshutdown providers drop spans.
- Use `WithBatcher` in production (non-blocking), `NewSimpleSpanProcessor` in tests (synchronous; spans available immediately after `End()`).
- Set attributes at span start when possible (samplers can use them); add events for significant moments within the span; call both `RecordError` and `SetStatus(codes.Error, ...)` on failures.
- Propagate the context across every function boundary. A fresh `context.Background()` breaks the parent-child relationship.
- Wrap outgoing HTTP clients with `otelhttp.NewTransport`; use `otelhttp.NewHandler` for incoming request instrumentation. Both handle W3C `traceparent` header injection and extraction.
- `tracetest.InMemoryExporter` makes span assertions deterministic in unit tests; no Collector required.

## What's Next

Next: [Race Condition Reproduction](../../32-concurrency-debugging-and-testing/01-race-condition-reproduction/01-race-condition-reproduction.md).

## Resources

- [OpenTelemetry Go SDK — pkg.go.dev](https://pkg.go.dev/go.opentelemetry.io/otel)
- [go.opentelemetry.io/otel/sdk/trace — BatchSpanProcessor, SimpleSpanProcessor, TracerProvider](https://pkg.go.dev/go.opentelemetry.io/otel/sdk/trace)
- [go.opentelemetry.io/otel/sdk/trace/tracetest — InMemoryExporter](https://pkg.go.dev/go.opentelemetry.io/otel/sdk/trace/tracetest)
- [go.opentelemetry.io/contrib/instrumentation/net/http/otelhttp — NewHandler, NewTransport](https://pkg.go.dev/go.opentelemetry.io/contrib/instrumentation/net/http/otelhttp)
- [OpenTelemetry Go manual instrumentation guide](https://opentelemetry.io/docs/languages/go/instrumentation/)
