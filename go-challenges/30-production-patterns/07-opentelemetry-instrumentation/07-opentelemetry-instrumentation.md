# 7. OpenTelemetry Instrumentation

OpenTelemetry (OTel) is the CNCF standard for vendor-neutral observability. The hard parts are not the API calls themselves but the initialization order, the resource model, the propagation contract, and the shutdown sequence. Get those wrong and your traces are either lost, un-correlated, or leak goroutines at process exit. This lesson builds a complete, production-shaped HTTP service with traces, metrics, and correct shutdown, using the stdout exporters so nothing external is required for experimentation.

```text
otelservice/
  go.mod
  telemetry/
    telemetry.go       -- provider init + shutdown
  service/
    service.go         -- HTTP handlers, span creation, metric recording
    service_test.go    -- httptest-based tests; no network
  cmd/demo/
    main.go            -- runnable demo: go run ./cmd/demo
```

The package layout keeps telemetry setup separate from business logic. Providers are passed explicitly, not read from globals, making tests hermetic.

## Concepts

### The Three Pillars and the OTel Signal Model

OpenTelemetry defines three observability signals: traces, metrics, and logs. Each signal has its own provider type (`TracerProvider`, `MeterProvider`, `LoggerProvider`), its own SDK implementation, and its own exporter interface. The global API (`otel.SetTracerProvider`, `otel.SetMeterProvider`) provides a default that works before any SDK is configured but emits nothing — it is a no-op until you register a real implementation.

```
global API  -->  TracerProvider  -->  Tracer  -->  Span
                 MeterProvider   -->  Meter   -->  Int64Counter / Float64Histogram
```

The SDK sits below the API. Your library code imports only `go.opentelemetry.io/otel`; your main package imports the SDK and registers a real provider. This split lets libraries be instrumented without pulling in the SDK.

### Initialization Order and the Resource

A `resource.Resource` describes the entity producing telemetry: service name, version, deployment environment. It must be constructed before the providers because both the trace SDK and the metric SDK attach it to every span and data point they export.

```go
res, err := resource.New(ctx,
	resource.WithAttributes(
		semconv.ServiceName("order-service"),
		semconv.ServiceVersion("1.2.0"),
		semconv.DeploymentEnvironment("production"),
	),
)
```

`semconv` constants are generated from the OpenTelemetry semantic conventions specification. Using them instead of bare strings keeps your attributes searchable and comparable across services and tools.

### Spans: Parent-Child Relationships via Context

A span is created by passing a `context.Context` into `tracer.Start`. If the context already contains a span, the new span becomes a child; otherwise it becomes a root span. The new context returned by `Start` must be threaded into any downstream call that should appear nested in the trace.

```go
ctx, span := tracer.Start(ctx, "validate-order",
	trace.WithSpanKind(trace.SpanKindInternal),
)
defer span.End()

// Pass ctx — not the original — into the DB call so it appears as a child.
if err := db.QueryContext(ctx, ...); err != nil {
	span.RecordError(err)
	span.SetStatus(codes.Error, "database query failed")
	return err
}
```

`span.End()` must always be called, even on error paths. Deferred `End` is the safe pattern.

### Metrics: Instruments and Measurement

The metric API provides push-based instruments. The three most common:

| Instrument | Method | Use case |
| --- | --- | --- |
| `Int64Counter` | `Add(ctx, delta, opts...)` | monotonically increasing count (requests, errors) |
| `Float64Histogram` | `Record(ctx, value, opts...)` | distribution of measured values (latency in ms, payload bytes) |
| `Int64Gauge` | `Record(ctx, value, opts...)` | instantaneous snapshot (queue depth, goroutine count) |

Instruments are created once (at initialization) and reused across requests. Creating an instrument per request is a resource leak. Attributes (`metric.WithAttributes(...)`) are added at measurement time, not at instrument creation time.

### The Propagation Contract

Distributed tracing works because trace context is propagated across network boundaries. In HTTP, the W3C TraceContext standard defines two headers: `traceparent` (span ID + trace ID + flags) and `tracestate` (vendor-specific metadata). The `otelhttp` middleware handles injection and extraction automatically when you register a `TextMapPropagator` with `otel.SetTextMapPropagator`.

Without this registration, each service starts a new root trace — spans exist but are never correlated.

### Shutdown and Flush

The SDK batches spans and metrics in memory and flushes them to exporters on a timer (default 60 s for metrics, after span `End` for traces with `WithBatcher`). At process exit, call `Shutdown(ctx)` on both providers to flush and block until done. A missing `Shutdown` causes the last batch to be silently dropped.

```go
// In main:
shutdown, err := telemetry.Init(ctx, cfg)
if err != nil { log.Fatal(err) }
defer func() {
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	if err := shutdown(ctx); err != nil {
		log.Printf("telemetry shutdown: %v", err)
	}
}()
```

### The noop Provider Pattern for Tests

The OTel API ships a noop implementation that satisfies every interface without recording anything. Use it in tests to avoid real export and global state mutations.

```go
import "go.opentelemetry.io/otel/trace/noop"

tp := noop.NewTracerProvider()
tracer := tp.Tracer("test")
```

This keeps unit tests hermetic: no goroutines, no file descriptors, no global side effects.

## Exercises

Set up the module:

```bash
go get go.opentelemetry.io/otel@latest
go get go.opentelemetry.io/otel/sdk@latest
go get go.opentelemetry.io/otel/exporters/stdout/stdouttrace@latest
go get go.opentelemetry.io/otel/exporters/stdout/stdoutmetric@latest
go get go.opentelemetry.io/contrib/instrumentation/net/http/otelhttp@latest
mkdir -p telemetry service cmd/demo
```

This is a library + server, not a bare program. Verification is `go test ./...`.

### Exercise 1: Telemetry Initialization

Create `telemetry/telemetry.go`:

```go
// telemetry/telemetry.go
package telemetry

import (
	"context"
	"fmt"
	"io"
	"time"

	"go.opentelemetry.io/otel"
	"go.opentelemetry.io/otel/exporters/stdout/stdoutmetric"
	"go.opentelemetry.io/otel/exporters/stdout/stdouttrace"
	"go.opentelemetry.io/otel/propagation"
	sdkmetric "go.opentelemetry.io/otel/sdk/metric"
	"go.opentelemetry.io/otel/sdk/resource"
	sdktrace "go.opentelemetry.io/otel/sdk/trace"
	semconv "go.opentelemetry.io/otel/semconv/v1.26.0"
)

// Config holds the telemetry initialization parameters.
type Config struct {
	ServiceName    string
	ServiceVersion string
	// TraceWriter is where trace output goes. Defaults to os.Stdout if nil.
	TraceWriter io.Writer
	// MetricWriter is where metric output goes. Defaults to os.Stdout if nil.
	MetricWriter io.Writer
	// MetricInterval controls the periodic export interval for metrics.
	// Defaults to 15s if zero.
	MetricInterval time.Duration
}

// ShutdownFunc flushes and releases all OTel SDK resources.
// It should be called with a context that allows at least 5 seconds for flushing.
type ShutdownFunc func(ctx context.Context) error

// Init configures trace and metric providers, registers them globally, and
// returns a ShutdownFunc that must be called at process exit.
//
// The providers are registered on the global otel API so that library code that
// calls otel.Tracer / otel.Meter gets a real implementation.
func Init(ctx context.Context, cfg Config) (ShutdownFunc, error) {
	if cfg.MetricInterval <= 0 {
		cfg.MetricInterval = 15 * time.Second
	}

	res, err := resource.New(ctx,
		resource.WithAttributes(
			semconv.ServiceName(cfg.ServiceName),
			semconv.ServiceVersion(cfg.ServiceVersion),
		),
	)
	if err != nil {
		return nil, fmt.Errorf("telemetry: build resource: %w", err)
	}

	tp, err := buildTracerProvider(res, cfg.TraceWriter)
	if err != nil {
		return nil, err
	}

	mp, err := buildMeterProvider(res, cfg.MetricWriter, cfg.MetricInterval)
	if err != nil {
		_ = tp.Shutdown(ctx)
		return nil, err
	}

	otel.SetTracerProvider(tp)
	otel.SetMeterProvider(mp)
	otel.SetTextMapPropagator(propagation.NewCompositeTextMapPropagator(
		propagation.TraceContext{},
		propagation.Baggage{},
	))

	shutdown := func(ctx context.Context) error {
		var first error
		if err := tp.Shutdown(ctx); err != nil && first == nil {
			first = fmt.Errorf("telemetry: trace provider shutdown: %w", err)
		}
		if err := mp.Shutdown(ctx); err != nil && first == nil {
			first = fmt.Errorf("telemetry: metric provider shutdown: %w", err)
		}
		return first
	}
	return shutdown, nil
}

func buildTracerProvider(res *resource.Resource, w io.Writer) (*sdktrace.TracerProvider, error) {
	opts := []stdouttrace.Option{stdouttrace.WithPrettyPrint()}
	if w != nil {
		opts = append(opts, stdouttrace.WithWriter(w))
	}
	exp, err := stdouttrace.New(opts...)
	if err != nil {
		return nil, fmt.Errorf("telemetry: trace exporter: %w", err)
	}
	return sdktrace.NewTracerProvider(
		sdktrace.WithBatcher(exp),
		sdktrace.WithResource(res),
	), nil
}

func buildMeterProvider(res *resource.Resource, w io.Writer, interval time.Duration) (*sdkmetric.MeterProvider, error) {
	opts := []stdoutmetric.Option{}
	if w != nil {
		opts = append(opts, stdoutmetric.WithWriter(w))
	}
	exp, err := stdoutmetric.New(opts...)
	if err != nil {
		return nil, fmt.Errorf("telemetry: metric exporter: %w", err)
	}
	reader := sdkmetric.NewPeriodicReader(exp, sdkmetric.WithInterval(interval))
	return sdkmetric.NewMeterProvider(
		sdkmetric.WithReader(reader),
		sdkmetric.WithResource(res),
	), nil
}
```

`Init` takes a `Config` struct rather than variadic options to keep the signature stable as fields are added. Both providers are shut down in the returned `ShutdownFunc`; the first error is returned, but both Shutdown calls are always attempted.

### Exercise 2: The Instrumented Service

Create `service/service.go`. The service holds its own tracer and meters so it does not rely on global state — easier to test with injected providers.

```go
// service/service.go
package service

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"math/rand/v2"
	"net/http"
	"time"

	"go.opentelemetry.io/contrib/instrumentation/net/http/otelhttp"
	"go.opentelemetry.io/otel/attribute"
	"go.opentelemetry.io/otel/codes"
	"go.opentelemetry.io/otel/metric"
	"go.opentelemetry.io/otel/trace"
)

// ErrInvalidCustomer is returned when the customer query param is empty.
var ErrInvalidCustomer = errors.New("customer is required")

// Instruments holds the metric instruments for the service.
// Instruments are created once and reused; creating them per-request is a resource leak.
type Instruments struct {
	requestCount   metric.Int64Counter
	requestLatency metric.Float64Histogram
	activeRequests metric.Int64UpDownCounter
}

// NewInstruments creates metric instruments from the given Meter.
func NewInstruments(m metric.Meter) (Instruments, error) {
	rc, err := m.Int64Counter("http.server.request.total",
		metric.WithDescription("Total HTTP requests handled"),
		metric.WithUnit("{request}"),
	)
	if err != nil {
		return Instruments{}, fmt.Errorf("service: request counter: %w", err)
	}

	rl, err := m.Float64Histogram("http.server.request.duration",
		metric.WithDescription("HTTP request duration in milliseconds"),
		metric.WithUnit("ms"),
	)
	if err != nil {
		return Instruments{}, fmt.Errorf("service: request histogram: %w", err)
	}

	ar, err := m.Int64UpDownCounter("http.server.active_requests",
		metric.WithDescription("Number of in-flight HTTP requests"),
		metric.WithUnit("{request}"),
	)
	if err != nil {
		return Instruments{}, fmt.Errorf("service: active requests gauge: %w", err)
	}

	return Instruments{
		requestCount:   rc,
		requestLatency: rl,
		activeRequests: ar,
	}, nil
}

// Handler is the HTTP handler for the order service.
type Handler struct {
	tracer      trace.Tracer
	instruments Instruments
}

// New returns an HTTP handler wired with the given tracer and instruments.
func New(tp trace.TracerProvider, mp metric.MeterProvider) (*Handler, error) {
	inst, err := NewInstruments(mp.Meter("example.com/otelservice"))
	if err != nil {
		return nil, err
	}
	return &Handler{
		tracer:      tp.Tracer("example.com/otelservice"),
		instruments: inst,
	}, nil
}

// Mount registers the handler's routes on mux and wraps the entire mux with
// the otelhttp middleware for automatic span creation per request.
func (h *Handler) Mount(mux *http.ServeMux) http.Handler {
	mux.HandleFunc("GET /order", h.handleOrder)
	mux.HandleFunc("GET /health", h.handleHealth)
	return otelhttp.NewHandler(mux, "order-service",
		otelhttp.WithSpanNameFormatter(func(op string, r *http.Request) string {
			return r.Method + " " + r.URL.Path
		}),
	)
}

func (h *Handler) handleHealth(w http.ResponseWriter, r *http.Request) {
	w.WriteHeader(http.StatusOK)
	fmt.Fprintln(w, "ok")
}

func (h *Handler) handleOrder(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	start := time.Now()

	attrs := attribute.NewSet(
		attribute.String("http.route", "/order"),
		attribute.String("http.method", r.Method),
	)

	h.instruments.activeRequests.Add(ctx, 1, metric.WithAttributeSet(attrs))
	defer h.instruments.activeRequests.Add(ctx, -1, metric.WithAttributeSet(attrs))

	customer := r.URL.Query().Get("customer")
	if customer == "" {
		http.Error(w, ErrInvalidCustomer.Error(), http.StatusBadRequest)
		h.recordMetrics(ctx, attrs, http.StatusBadRequest, time.Since(start))
		return
	}

	orderID, amount, err := h.processOrder(ctx, customer)
	if err != nil {
		http.Error(w, "internal error", http.StatusInternalServerError)
		h.recordMetrics(ctx, attrs, http.StatusInternalServerError, time.Since(start))
		return
	}

	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(map[string]any{
		"order_id": orderID,
		"amount":   amount,
		"status":   "completed",
	})
	h.recordMetrics(ctx, attrs, http.StatusOK, time.Since(start))
}

// processOrder runs the business logic for an order and returns the order ID
// and amount. Each step is represented as a child span.
func (h *Handler) processOrder(ctx context.Context, customer string) (string, float64, error) {
	ctx, span := h.tracer.Start(ctx, "process-order",
		trace.WithSpanKind(trace.SpanKindInternal),
		trace.WithAttributes(attribute.String("order.customer", customer)),
	)
	defer span.End()

	orderID := fmt.Sprintf("ORD-%06d", rand.IntN(1_000_000))
	span.SetAttributes(attribute.String("order.id", orderID))

	if err := h.queryInventory(ctx, orderID); err != nil {
		span.RecordError(err)
		span.SetStatus(codes.Error, "inventory check failed")
		return "", 0, err
	}

	amount := h.processPayment(ctx, orderID)
	span.SetAttributes(attribute.Float64("order.amount_usd", amount))
	span.SetStatus(codes.Ok, "")

	return orderID, amount, nil
}

// queryInventory simulates a database read. It creates a child span with
// database semantic convention attributes.
func (h *Handler) queryInventory(ctx context.Context, orderID string) error {
	_, span := h.tracer.Start(ctx, "db.query.inventory",
		trace.WithSpanKind(trace.SpanKindClient),
		trace.WithAttributes(
			attribute.String("db.system", "postgresql"),
			attribute.String("db.operation.name", "SELECT"),
			attribute.String("db.collection.name", "inventory"),
		),
	)
	defer span.End()

	// Simulated latency: in production this is a real query.
	time.Sleep(time.Duration(10+rand.IntN(20)) * time.Millisecond)
	return nil
}

// processPayment simulates a downstream payment call. It creates a child span
// with RPC semantic convention attributes.
func (h *Handler) processPayment(ctx context.Context, orderID string) float64 {
	_, span := h.tracer.Start(ctx, "rpc.payment.charge",
		trace.WithSpanKind(trace.SpanKindClient),
		trace.WithAttributes(
			attribute.String("rpc.system", "grpc"),
			attribute.String("rpc.service", "PaymentService"),
			attribute.String("rpc.method", "Charge"),
			attribute.String("payment.order_id", orderID),
		),
	)
	defer span.End()

	amount := float64(rand.IntN(10_000)) / 100.0
	span.SetAttributes(attribute.Float64("payment.amount_usd", amount))
	time.Sleep(time.Duration(30+rand.IntN(50)) * time.Millisecond)
	return amount
}

func (h *Handler) recordMetrics(ctx context.Context, attrs attribute.Set, status int, d time.Duration) {
	statusAttr := attribute.NewSet(
		attribute.String("http.route", "/order"),
		attribute.String("http.method", "GET"),
		attribute.Int("http.response.status_code", status),
	)
	h.instruments.requestCount.Add(ctx, 1, metric.WithAttributeSet(statusAttr))
	h.instruments.requestLatency.Record(ctx, float64(d.Milliseconds()), metric.WithAttributeSet(attrs))
}
```

`Handler.New` accepts interfaces (`trace.TracerProvider`, `metric.MeterProvider`) rather than concrete SDK types, so tests can inject a noop provider without importing the SDK.

### Exercise 3: Tests

Create `service/service_test.go`. All tests use `httptest` and noop providers — no network, no file I/O, no global state.

```go
// service/service_test.go
package service

import (
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"testing"

	metricnoop "go.opentelemetry.io/otel/metric/noop"
	tracenoop "go.opentelemetry.io/otel/trace/noop"
)

func newTestHandler(t *testing.T) *Handler {
	t.Helper()
	h, err := New(tracenoop.NewTracerProvider(), metricnoop.NewMeterProvider())
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	return h
}

func TestHandleHealth(t *testing.T) {
	t.Parallel()

	h := newTestHandler(t)
	mux := http.NewServeMux()
	handler := h.Mount(mux)

	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodGet, "/health", nil)
	handler.ServeHTTP(rec, req)

	if rec.Code != http.StatusOK {
		t.Errorf("status = %d, want %d", rec.Code, http.StatusOK)
	}
}

func TestHandleOrderMissingCustomer(t *testing.T) {
	t.Parallel()

	h := newTestHandler(t)
	mux := http.NewServeMux()
	handler := h.Mount(mux)

	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodGet, "/order", nil)
	handler.ServeHTTP(rec, req)

	if rec.Code != http.StatusBadRequest {
		t.Errorf("status = %d, want %d", rec.Code, http.StatusBadRequest)
	}
}

func TestHandleOrderSuccess(t *testing.T) {
	t.Parallel()

	h := newTestHandler(t)
	mux := http.NewServeMux()
	handler := h.Mount(mux)

	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodGet, "/order?customer=alice", nil)
	handler.ServeHTTP(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want %d", rec.Code, http.StatusOK)
	}

	var body map[string]any
	if err := json.NewDecoder(rec.Body).Decode(&body); err != nil {
		t.Fatalf("decode body: %v", err)
	}

	if body["status"] != "completed" {
		t.Errorf("status = %q, want %q", body["status"], "completed")
	}
	if _, ok := body["order_id"]; !ok {
		t.Error("response missing order_id field")
	}
	if _, ok := body["amount"]; !ok {
		t.Error("response missing amount field")
	}
}

func TestHandleOrderTable(t *testing.T) {
	t.Parallel()

	cases := []struct {
		name       string
		path       string
		wantStatus int
	}{
		{"no customer", "/order", http.StatusBadRequest},
		{"empty customer", "/order?customer=", http.StatusBadRequest},
		{"valid customer", "/order?customer=bob", http.StatusOK},
	}

	h := newTestHandler(t)
	mux := http.NewServeMux()
	handler := h.Mount(mux)

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			rec := httptest.NewRecorder()
			req := httptest.NewRequest(http.MethodGet, tc.path, nil)
			handler.ServeHTTP(rec, req)
			if rec.Code != tc.wantStatus {
				t.Errorf("status = %d, want %d", rec.Code, tc.wantStatus)
			}
		})
	}
}

func ExampleNew() {
	h, err := New(tracenoop.NewTracerProvider(), metricnoop.NewMeterProvider())
	if err != nil {
		fmt.Println("error:", err)
		return
	}
	mux := http.NewServeMux()
	handler := h.Mount(mux)
	fmt.Println(handler != nil)
	// Output:
	// true
}
```

Your turn: add `TestNewInstrumentsErrorPropagation` that passes a broken `metric.Meter` implementation and confirms `New` returns a non-nil error.

Create `cmd/demo/main.go`:

```go
// cmd/demo/main.go
package main

import (
	"context"
	"fmt"
	"log"
	"net/http"
	"os"
	"os/signal"
	"time"

	"example.com/otelservice/service"
	"example.com/otelservice/telemetry"
	"go.opentelemetry.io/otel"
)

func main() {
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt)
	defer stop()

	shutdown, err := telemetry.Init(ctx, telemetry.Config{
		ServiceName:    "order-service",
		ServiceVersion: "1.0.0",
		MetricInterval: 10 * time.Second,
	})
	if err != nil {
		log.Fatalf("telemetry init: %v", err)
	}
	defer func() {
		sctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		if err := shutdown(sctx); err != nil {
			log.Printf("telemetry shutdown: %v", err)
		}
	}()

	h, err := service.New(otel.GetTracerProvider(), otel.GetMeterProvider())
	if err != nil {
		log.Fatalf("service init: %v", err)
	}

	mux := http.NewServeMux()
	handler := h.Mount(mux)

	srv := &http.Server{
		Addr:         ":8080",
		Handler:      handler,
		ReadTimeout:  5 * time.Second,
		WriteTimeout: 10 * time.Second,
	}

	go func() {
		<-ctx.Done()
		sctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		if err := srv.Shutdown(sctx); err != nil {
			log.Printf("http shutdown: %v", err)
		}
	}()

	fmt.Println("listening on :8080")
	fmt.Println("try: curl 'http://localhost:8080/order?customer=alice'")
	if err := srv.ListenAndServe(); err != nil && err != http.ErrServerClosed {
		log.Fatalf("listen: %v", err)
	}
}
```

Run it:

```bash
go run ./cmd/demo
# In another terminal:
curl 'http://localhost:8080/order?customer=alice'
curl 'http://localhost:8080/health'
```

Trace spans print to stdout as JSON. After 10 s, metric data appears.

## Common Mistakes

**Wrong: Calling `otel.Tracer()` before `otel.SetTracerProvider`.**

`otel.Tracer()` returns a tracer from whatever provider is currently registered. If called at package init time (in a `var` or `init()`), the SDK provider has not been set yet and the tracer is a noop. Spans created before `SetTracerProvider` are silently discarded.

Fix: initialize the SDK first in `main`, then pass `tp.Tracer("name")` explicitly to the component that needs it.

**Wrong: Using `math/rand` instead of `math/rand/v2`.**

`math/rand.Intn` requires a seeded source in Go 1.19 and earlier; in Go 1.20+ the global source is auto-seeded but `math/rand.Intn` is still deprecated in favor of `math/rand/v2`. Use `rand.IntN(n)` from `math/rand/v2`.

Fix: import `"math/rand/v2"` and use `rand.IntN(n)`.

**Wrong: Ignoring the error from `Meter.Int64Counter` (and similar).**

```go
// Wrong
rc, _ := m.Int64Counter("requests")
```

Instrument creation can fail if the meter is misconfigured or if the name violates the OTel specification (e.g., starts with a digit). A silently failed instrument is a noop; you lose all metric data with no indication.

Fix:
```go
// Fix
rc, err := m.Int64Counter("http.server.request.total")
if err != nil {
	return fmt.Errorf("service: request counter: %w", err)
}
```

**Wrong: Not calling `span.End()` on all code paths.**

```go
// Wrong
ctx, span := tracer.Start(ctx, "work")
result, err := doWork(ctx)
if err != nil {
	return err // span never ended — memory and exporter goroutine leaked
}
span.End()
```

Fix: always `defer span.End()` immediately after `tracer.Start`.

**Wrong: Recording an error without setting the span status.**

`span.RecordError(err)` adds an exception event to the span, but the span status remains unset (`Unset`). Jaeger and similar tools do not mark the span as failed unless the status is explicitly set to `Error`.

```go
// Wrong
span.RecordError(err)

// Fix
span.RecordError(err)
span.SetStatus(codes.Error, "brief description")
```

**Wrong: Creating metric instruments per-request.**

```go
// Wrong — inside handleOrder:
counter, _ := meter.Int64Counter("requests")
counter.Add(ctx, 1)
```

Each `Int64Counter("requests")` call registers a new instrument with the SDK. Under load this allocates unboundedly and the SDK may warn or ignore duplicates.

Fix: create instruments once (in `NewInstruments` or a constructor) and store them in a struct.

## Verification

From `~/go-exercises/otelservice`:

```bash
test -z "$(gofmt -l ./telemetry/ ./service/)"
go vet ./telemetry/... ./service/...
go build ./...
go test -count=1 -race ./service/...
```

All four must pass. The demo requires a network port; run it separately:

```bash
go run ./cmd/demo
```

## Summary

- The resource is the foundation: it identifies the service and must be built before providers.
- `TracerProvider` and `MeterProvider` are initialized once in `main` and passed down; do not read global state inside library code.
- Spans follow the context: always thread the context returned by `tracer.Start` into downstream calls to create parent-child relationships.
- Always `defer span.End()` and always call `span.SetStatus(codes.Error, ...)` alongside `span.RecordError`.
- Metric instruments are created once and reused; attributes are added at measurement time via `metric.WithAttributeSet`.
- `otelhttp.NewHandler` adds automatic span creation and HTTP metric recording to any `http.Handler`.
- Register `propagation.TraceContext{}` so W3C `traceparent` headers are extracted and injected automatically.
- Always call `Shutdown` on both providers at process exit with a bounded context timeout to flush pending data.
- Use noop providers (`trace/noop`, `metric/noop`) in tests to stay hermetic.

## What's Next

[8. Distributed Tracing Context](../08-distributed-tracing-context/08-distributed-tracing-context.md)

## Resources

- [OpenTelemetry Go getting started guide](https://opentelemetry.io/docs/languages/go/getting-started/) — canonical step-by-step for the SDK
- [pkg.go.dev: go.opentelemetry.io/otel](https://pkg.go.dev/go.opentelemetry.io/otel) — top-level API: Tracer, Meter, SetTracerProvider, SetMeterProvider
- [pkg.go.dev: go.opentelemetry.io/otel/sdk/trace](https://pkg.go.dev/go.opentelemetry.io/otel/sdk/trace) — TracerProvider, WithBatcher, WithResource, Shutdown
- [pkg.go.dev: go.opentelemetry.io/otel/sdk/metric](https://pkg.go.dev/go.opentelemetry.io/otel/sdk/metric) — MeterProvider, NewPeriodicReader, WithInterval
- [OpenTelemetry semantic conventions](https://opentelemetry.io/docs/specs/semconv/) — canonical attribute names for HTTP, DB, RPC spans
