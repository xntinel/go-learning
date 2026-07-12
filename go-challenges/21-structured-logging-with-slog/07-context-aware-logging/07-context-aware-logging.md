# 7. Context-Aware Logging

Context-aware logging is about request metadata, not hiding dependencies. This lesson builds a handler wrapper that reads request and trace IDs from `context.Context` and adds them to records.

## Concepts

### Context Values Need Narrow Scope

Use context values for data that crosses API boundaries with a request: trace IDs, request IDs, auth subjects, deadlines. Do not use context as a general dependency container for loggers.

### Handler-Based Enrichment

`Logger.InfoContext`, `Logger.Log`, and `Logger.LogAttrs` pass a context to the handler. A wrapper handler can inspect that context and add attributes before delegating to another handler.

### Typed Keys

Context keys should use an unexported custom type to avoid collisions with other packages. Export helper functions instead of exporting the keys.

## Exercises

Edit `go.mod`:

```go
module example.com/slogcontext

go 1.26
```

### Exercise 1: Build a Context Handler

Create `contexthandler.go`:

```go
package contexthandler

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
)

type ctxKey string

const (
	requestIDKey ctxKey = "request_id"
	traceIDKey   ctxKey = "trace_id"
)

var ErrNilHandler = errors.New("handler must not be nil")

type Handler struct {
	inner slog.Handler
}

func New(inner slog.Handler) (*Handler, error) {
	if inner == nil {
		return nil, fmt.Errorf("context handler: %w", ErrNilHandler)
	}
	return &Handler{inner: inner}, nil
}

func WithRequestID(ctx context.Context, id string) context.Context {
	return context.WithValue(ctx, requestIDKey, id)
}

func WithTraceID(ctx context.Context, id string) context.Context {
	return context.WithValue(ctx, traceIDKey, id)
}

func RequestID(ctx context.Context) string {
	id, _ := ctx.Value(requestIDKey).(string)
	return id
}

func TraceID(ctx context.Context) string {
	id, _ := ctx.Value(traceIDKey).(string)
	return id
}

func (h *Handler) Enabled(ctx context.Context, level slog.Level) bool {
	return h.inner.Enabled(ctx, level)
}

func (h *Handler) Handle(ctx context.Context, r slog.Record) error {
	clone := r.Clone()
	if id := RequestID(ctx); id != "" {
		clone.AddAttrs(slog.String("request_id", id))
	}
	if id := TraceID(ctx); id != "" {
		clone.AddAttrs(slog.String("trace_id", id))
	}
	return h.inner.Handle(ctx, clone)
}

func (h *Handler) WithAttrs(attrs []slog.Attr) slog.Handler {
	return &Handler{inner: h.inner.WithAttrs(attrs)}
}

func (h *Handler) WithGroup(name string) slog.Handler {
	return &Handler{inner: h.inner.WithGroup(name)}
}

func LogCharge(ctx context.Context, logger *slog.Logger, amountCents int) {
	logger.LogAttrs(ctx, slog.LevelInfo, "charge captured", slog.Int("amount_cents", amountCents))
}
```

### Exercise 2: Test Context Fields

Create `contexthandler_test.go`:

```go
package contexthandler

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"log/slog"
	"strings"
	"testing"
)

func newLogger(buf *bytes.Buffer) (*slog.Logger, error) {
	inner := slog.NewTextHandler(buf, &slog.HandlerOptions{
		ReplaceAttr: func(groups []string, a slog.Attr) slog.Attr {
			if len(groups) == 0 && a.Key == slog.TimeKey {
				return slog.Attr{}
			}
			return a
		},
	})
	handler, err := New(inner)
	if err != nil {
		return nil, err
	}
	return slog.New(handler), nil
}

func TestLogChargeAddsContextFields(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name string
		ctx  context.Context
		want []string
		deny []string
	}{
		{name: "request and trace", ctx: WithTraceID(WithRequestID(context.Background(), "req-1"), "trace-1"), want: []string{"request_id=req-1", "trace_id=trace-1"}},
		{name: "empty context", ctx: context.Background(), want: []string{"amount_cents=2500"}, deny: []string{"request_id=", "trace_id="}},
	}

	for _, tc := range tests {
		tc := tc
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()

			var buf bytes.Buffer
			logger, err := newLogger(&buf)
			if err != nil {
				t.Fatalf("newLogger() error = %v", err)
			}
			LogCharge(tc.ctx, logger, 2500)

			got := buf.String()
			for _, want := range tc.want {
				if !strings.Contains(got, want) {
					t.Fatalf("output %q missing %q", got, want)
				}
			}
			for _, deny := range tc.deny {
				if strings.Contains(got, deny) {
					t.Fatalf("output %q unexpectedly contains %q", got, deny)
				}
			}
		})
	}
}

func TestContextAccessors(t *testing.T) {
	t.Parallel()

	ctx := WithTraceID(WithRequestID(context.Background(), "req-1"), "trace-1")
	if RequestID(ctx) != "req-1" || TraceID(ctx) != "trace-1" {
		t.Fatalf("RequestID=%q TraceID=%q", RequestID(ctx), TraceID(ctx))
	}
}

func TestNewRejectsNilHandler(t *testing.T) {
	t.Parallel()

	_, err := New(nil)
	if !errors.Is(err, ErrNilHandler) {
		t.Fatalf("New(nil) error = %v, want ErrNilHandler", err)
	}
}

func ExampleLogCharge() {
	var buf bytes.Buffer
	logger, _ := newLogger(&buf)
	ctx := WithTraceID(WithRequestID(context.Background(), "req-1"), "trace-1")
	LogCharge(ctx, logger, 2500)
	fmt.Print(buf.String())
	// Output: level=INFO msg="charge captured" amount_cents=2500 request_id=req-1 trace_id=trace-1
}
```

Your turn: add a test for a context with only a request ID.

### Exercise 3: Add a Demo Command

Create `cmd/demo/main.go`:

```go
package main

import (
	"bytes"
	"context"
	"fmt"
	"log/slog"

	"example.com/slogcontext"
)

func main() {
	var buf bytes.Buffer
	inner := slog.NewTextHandler(&buf, &slog.HandlerOptions{
		ReplaceAttr: func(groups []string, a slog.Attr) slog.Attr {
			if len(groups) == 0 && a.Key == slog.TimeKey {
				return slog.Attr{}
			}
			return a
		},
	})
	handler, err := contexthandler.New(inner)
	if err != nil {
		panic(err)
	}
	logger := slog.New(handler)
	ctx := contexthandler.WithTraceID(contexthandler.WithRequestID(context.Background(), "req-1"), "trace-1")
	contexthandler.LogCharge(ctx, logger, 2500)
	fmt.Print(buf.String())
}
```

## Common Mistakes

Wrong: use a plain string as a context key.

What happens: another package can accidentally use the same key.

Fix: use an unexported custom key type and exported helper functions.

Wrong: put the logger itself in context by default.

What happens: logging becomes an implicit dependency.

Fix: pass `*slog.Logger` explicitly and use context for request metadata.

Wrong: modify the incoming record in the handler.

What happens: hidden record state can be shared.

Fix: clone the record before adding context attributes.

## Verification

From `~/go-exercises/slog-context`:

```bash
test -z "$(gofmt -l .)"
go vet ./...
go build ./...
go test -count=1 -race ./...
go run ./cmd/demo
```

Add one test of your own for partial context metadata.

## Summary

- Context-aware logging should add request metadata, not hide dependencies.
- Handler wrappers can read context from `InfoContext` and `LogAttrs` calls.
- Typed unexported keys prevent context collisions.
- Clone records before adding attributes in handlers.

## What's Next

Next, reduce high-volume logs with [Log Sampling for High Throughput](../08-log-sampling/08-log-sampling.md).

## Resources

- `log/slog` contexts overview: https://pkg.go.dev/log/slog#hdr-Contexts
- `Logger.LogAttrs` documentation: https://pkg.go.dev/log/slog#Logger.LogAttrs
- `context` package documentation: https://pkg.go.dev/context
