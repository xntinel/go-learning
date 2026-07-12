# Exercise 7: Recording errors on OpenTelemetry spans with RecordError and SetStatus

A log line tells you a request failed. In a distributed system it does not tell
you *where* — which of the eight services and twenty hops in the call graph broke,
or how long each took. That is the trace's job. This module builds a traced
operation that, on failure, records the error as a span exception *and* marks the
span failed, and shows why both calls are needed. It uses the real OpenTelemetry
SDK with an in-memory span recorder, so the test asserts on recorded spans with no
collector.

This module imports `go.opentelemetry.io/otel`, so it needs network access the
first time to fetch the modules. Gate it with `GOFLAGS=-mod=mod`.

This module is fully self-contained: its own `go mod init`, demo, and tests.
Nothing here imports any other exercise.

## What you'll build

```text
otelspan/                    independent module: example.com/otelspan
  go.mod                     go 1.25; requires go.opentelemetry.io/otel + sdk
  service.go                 Service.Fetch: start span, on error RecordError + SetStatus(Error), attrs
  cmd/
    demo/
      main.go                runnable demo: in-memory recorder, one fail one success
  service_test.go            recorded span has Error status + exception event on fail; Unset on success
```

- Files: `service.go`, `cmd/demo/main.go`, `service_test.go`.
- Implement: a `Service.Fetch(ctx, id)` that starts a span via a `trace.Tracer`, sets an `id` attribute, and on failure calls `span.RecordError(err)` and `span.SetStatus(codes.Error, err.Error())`; on success leaves the status Unset.
- Test: use the SDK's `tracetest.SpanRecorder` as the span processor; drive a failing op and assert the span has status `codes.Error`, an `exception` event, and the `id` attribute; drive a success and assert the status is `codes.Unset`.
- Verify: `GOFLAGS=-mod=mod go test -count=1 -race ./...`

Set up the module:

```bash
mkdir -p go-solutions/10-error-handling/14-error-observability/07-otel-span-error-recording/cmd/demo
cd go-solutions/10-error-handling/14-error-observability/07-otel-span-error-recording
go mod edit -go=1.25
go get go.opentelemetry.io/otel@v1.44.0 go.opentelemetry.io/otel/sdk@v1.44.0
```

### Why two calls, and what each records

OpenTelemetry deliberately separates *what happened* from *did it succeed*, and a
correct failure path sets both:

- `span.RecordError(err)` attaches an *exception event* to the span — a timestamped
  event named `exception` carrying `exception.type` and `exception.message`
  attributes drawn from the error. This is the detail: what the error was. But it
  does **not** change the span's status. A span with a recorded error and no
  status still shows as successful (`Unset`) in the trace UI — the error is
  attached but invisible in any "show me failed spans" query.
- `span.SetStatus(codes.Error, msg)` marks the span *failed*. This is what a trace
  backend filters and alerts on. But on its own it carries no error detail — you
  see a red span with no explanation.

So you do both: `RecordError` for the detail, `SetStatus(codes.Error, ...)` for the
verdict. On the success path you do *neither* — the span's default status is
`codes.Unset`, which OTel treats as "no explicit status, presumed OK." Setting
`codes.Ok` is reserved for when application logic explicitly wants to override a
downstream error; the idiomatic success path leaves it Unset.

`Start` returns a new context carrying the span and the span itself; the deferred
`span.End()` closes it. Attributes set with `SetAttributes` (here the operation's
`id`) are the low-cardinality dimensions you filter traces by — the same
cardinality discipline as metrics applies: an `id` attribute is fine on a span
(traces are sampled and per-request), but never becomes a metric label.

The SDK plumbing exists only so the test can observe spans without a collector:
`tracetest.NewSpanRecorder()` is a `SpanProcessor` that keeps every ended span in
memory, and `sdktrace.NewTracerProvider(sdktrace.WithSpanProcessor(sr))` wires it
in. In production you would swap the recorder for an OTLP exporter; the
instrumentation code in `service.go` does not change.

Create `service.go`:

```go
package otelspan

import (
	"context"
	"errors"
	"fmt"

	"go.opentelemetry.io/otel/attribute"
	"go.opentelemetry.io/otel/codes"
	"go.opentelemetry.io/otel/trace"
)

// ErrNotFound is the sentinel a Fetch miss wraps.
var ErrNotFound = errors.New("not found")

// Service performs a traced Fetch. It holds a Tracer, not a global, so a test
// can inject a tracer backed by an in-memory recorder.
type Service struct {
	tracer trace.Tracer
	byID   map[string]string
}

// New returns a Service tracing through the given tracer.
func New(tracer trace.Tracer, seed map[string]string) *Service {
	m := make(map[string]string, len(seed))
	for k, v := range seed {
		m[k] = v
	}
	return &Service{tracer: tracer, byID: m}
}

// Fetch looks up id inside a span. On a miss it records the error on the span
// (exception event) AND marks the span failed (Error status), then returns the
// wrapped error. On a hit it leaves the span status Unset.
func (s *Service) Fetch(ctx context.Context, id string) (string, error) {
	_, span := s.tracer.Start(ctx, "Service.Fetch")
	defer span.End()
	span.SetAttributes(attribute.String("id", id))

	v, ok := s.byID[id]
	if !ok {
		err := fmt.Errorf("fetch %q: %w", id, ErrNotFound)
		span.RecordError(err)                    // exception event: the detail
		span.SetStatus(codes.Error, err.Error()) // Error status: the verdict
		return "", err
	}
	return v, nil
}
```

### The runnable demo

The demo builds an in-memory recorder, runs one failing and one succeeding
`Fetch`, and prints each recorded span's status and event count — so you can see
the failed span carry an `exception` event and `Error` status while the successful
one stays `Unset` with no events.

Create `cmd/demo/main.go`:

```go
package main

import (
	"context"
	"fmt"

	sdktrace "go.opentelemetry.io/otel/sdk/trace"
	"go.opentelemetry.io/otel/sdk/trace/tracetest"

	"example.com/otelspan"
)

func main() {
	sr := tracetest.NewSpanRecorder()
	tp := sdktrace.NewTracerProvider(sdktrace.WithSpanProcessor(sr))
	svc := otelspan.New(tp.Tracer("demo"), map[string]string{"u1": "alice"})

	ctx := context.Background()
	_, _ = svc.Fetch(ctx, "u1")      // hit -> Unset
	_, _ = svc.Fetch(ctx, "missing") // miss -> Error + exception

	for _, span := range sr.Ended() {
		id := ""
		for _, a := range span.Attributes() {
			if a.Key == "id" {
				id = a.Value.AsString()
			}
		}
		fmt.Printf("id=%s status=%s events=%d\n", id, span.Status().Code, len(span.Events()))
	}
}
```

Run it (first run fetches the OTel modules):

```bash
go run ./cmd/demo
```

Expected output:

```
id=u1 status=Unset events=0
id=missing status=Error events=1
```

### Tests

The tests use the in-memory recorder as the tracer provider and assert on the
recorded `ReadOnlySpan`. `TestFailureRecordsErrorAndStatus` proves the failed span
has `codes.Error` status, an `exception` event (from `RecordError`), and the `id`
attribute. `TestSuccessLeavesStatusUnset` proves the happy path records neither an
error status nor an event — the assertion that catches the "I set the status on
every span" bug.

Create `service_test.go`:

```go
package otelspan

import (
	"context"
	"testing"

	"go.opentelemetry.io/otel/attribute"
	"go.opentelemetry.io/otel/codes"
	sdktrace "go.opentelemetry.io/otel/sdk/trace"
	"go.opentelemetry.io/otel/sdk/trace/tracetest"
)

func newRecorded(t *testing.T, seed map[string]string) (*Service, *tracetest.SpanRecorder) {
	t.Helper()
	sr := tracetest.NewSpanRecorder()
	tp := sdktrace.NewTracerProvider(sdktrace.WithSpanProcessor(sr))
	return New(tp.Tracer("test"), seed), sr
}

func TestFailureRecordsErrorAndStatus(t *testing.T) {
	t.Parallel()
	svc, sr := newRecorded(t, nil)

	if _, err := svc.Fetch(context.Background(), "missing"); err == nil {
		t.Fatal("Fetch(missing) err = nil, want ErrNotFound")
	}

	spans := sr.Ended()
	if len(spans) != 1 {
		t.Fatalf("recorded %d spans, want 1", len(spans))
	}
	span := spans[0]

	if got := span.Status().Code; got != codes.Error {
		t.Fatalf("span status = %v, want Error", got)
	}
	events := span.Events()
	if len(events) != 1 || events[0].Name != "exception" {
		t.Fatalf("events = %+v, want one exception event", events)
	}
	if !hasAttr(span.Attributes(), "id", "missing") {
		t.Fatalf("span missing id=missing attribute; got %v", span.Attributes())
	}
}

func TestSuccessLeavesStatusUnset(t *testing.T) {
	t.Parallel()
	svc, sr := newRecorded(t, map[string]string{"u1": "alice"})

	if _, err := svc.Fetch(context.Background(), "u1"); err != nil {
		t.Fatalf("Fetch(u1) err = %v, want nil", err)
	}

	span := sr.Ended()[0]
	if got := span.Status().Code; got != codes.Unset {
		t.Fatalf("success span status = %v, want Unset", got)
	}
	if len(span.Events()) != 0 {
		t.Fatalf("success span has %d events, want 0", len(span.Events()))
	}
}

func hasAttr(attrs []attribute.KeyValue, key, val string) bool {
	for _, a := range attrs {
		if string(a.Key) == key && a.Value.AsString() == val {
			return true
		}
	}
	return false
}
```

## Review

The instrumentation is correct when failure and success are distinguishable in the
trace. `TestFailureRecordsErrorAndStatus` proves the failed span carries both the
`Error` verdict (from `SetStatus`) and the `exception` detail (from
`RecordError`) plus its `id` attribute; `TestSuccessLeavesStatusUnset` proves the
happy path leaves the status `Unset` and records no event. Drop either failure
call and one of these tests fails, which is the point: `RecordError` without
`SetStatus` leaves the span looking green, and `SetStatus` without `RecordError`
leaves a red span with no explanation.

Keep the cardinality discipline from the metrics exercises in mind: an `id`
attribute on a span is fine because traces are per-request and sampled, but the
same value must never become a metric label. The instrumentation lives in the
service method here for teaching; in a larger system it moves into a traced-client
wrapper or middleware so the domain stays transport-free — same separation as
every other pillar in this chapter.

## Resources

- [OpenTelemetry Go trace API](https://pkg.go.dev/go.opentelemetry.io/otel/trace) — `Tracer.Start`, `Span.RecordError`, `SetStatus`, `SetAttributes`, `End`.
- [`go.opentelemetry.io/otel/codes`](https://pkg.go.dev/go.opentelemetry.io/otel/codes) — `codes.Error`, `codes.Unset`, `codes.Ok` and their meaning.
- [`sdk/trace/tracetest`](https://pkg.go.dev/go.opentelemetry.io/otel/sdk/trace/tracetest) — the in-memory `SpanRecorder` for asserting on spans in tests.
- [OTel spec: recording errors](https://opentelemetry.io/docs/specs/otel/trace/exceptions/) — why RecordError and status are separate.

---

Back to [00-concepts.md](00-concepts.md) | Next: [08-red-metrics-http-middleware.md](08-red-metrics-http-middleware.md)
