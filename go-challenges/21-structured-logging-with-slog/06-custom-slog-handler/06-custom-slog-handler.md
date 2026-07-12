# 6. Custom Slog Handler

Custom handlers should preserve `slog` semantics: do not mutate shared records, keep `WithAttrs` and `WithGroup` immutable, and delegate filtering correctly. This lesson builds a routing wrapper around any `slog.Handler`.

## Concepts

### The Handler Contract

The `slog.Handler` interface has `Enabled`, `Handle`, `WithAttrs`, and `WithGroup`. `Enabled` is called before a record is built fully enough to be handled. `Handle` writes an enabled record. `WithAttrs` and `WithGroup` must return handlers that include extra state without mutating the original handler.

### Cloning Records

The `log/slog` documentation warns that records contain hidden state for attributes. A handler that modifies a record should call `Record.Clone()` first, then add attributes to the clone.

### Wrappers over Reimplementation

Most production handlers are wrappers: they add fields, route levels, sample events, or bridge to another backend. Wrapping a built-in handler avoids reimplementing escaping, grouping, locking, and formatting.

## Exercises

Edit `go.mod`:

```go
module example.com/slogcustomhandler

go 1.26
```

### Exercise 1: Build a Routing Handler

Create `routehandler.go`:

```go
package routehandler

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
)

var ErrNilHandler = errors.New("handler must not be nil")

type Handler struct {
	inner slog.Handler
	name  string
}

func New(inner slog.Handler, name string) (*Handler, error) {
	if inner == nil {
		return nil, fmt.Errorf("route handler: %w", ErrNilHandler)
	}
	if name == "" {
		name = "default"
	}
	return &Handler{inner: inner, name: name}, nil
}

func (h *Handler) Name() string {
	return h.name
}

func (h *Handler) Enabled(ctx context.Context, level slog.Level) bool {
	return h.inner.Enabled(ctx, level)
}

func (h *Handler) Handle(ctx context.Context, r slog.Record) error {
	clone := r.Clone()
	clone.AddAttrs(slog.String("route", h.name))
	return h.inner.Handle(ctx, clone)
}

func (h *Handler) WithAttrs(attrs []slog.Attr) slog.Handler {
	return &Handler{inner: h.inner.WithAttrs(attrs), name: h.name}
}

func (h *Handler) WithGroup(name string) slog.Handler {
	return &Handler{inner: h.inner.WithGroup(name), name: h.name}
}

type MultiHandler struct {
	handlers []slog.Handler
}

func NewMulti(handlers ...slog.Handler) (*MultiHandler, error) {
	for _, handler := range handlers {
		if handler == nil {
			return nil, fmt.Errorf("multi handler: %w", ErrNilHandler)
		}
	}
	return &MultiHandler{handlers: append([]slog.Handler(nil), handlers...)}, nil
}

func (h *MultiHandler) HandlerCount() int {
	return len(h.handlers)
}

func (h *MultiHandler) Enabled(ctx context.Context, level slog.Level) bool {
	for _, handler := range h.handlers {
		if handler.Enabled(ctx, level) {
			return true
		}
	}
	return false
}

func (h *MultiHandler) Handle(ctx context.Context, r slog.Record) error {
	for _, handler := range h.handlers {
		if !handler.Enabled(ctx, r.Level) {
			continue
		}
		if err := handler.Handle(ctx, r.Clone()); err != nil {
			return err
		}
	}
	return nil
}

func (h *MultiHandler) WithAttrs(attrs []slog.Attr) slog.Handler {
	next := make([]slog.Handler, 0, len(h.handlers))
	for _, handler := range h.handlers {
		next = append(next, handler.WithAttrs(attrs))
	}
	return &MultiHandler{handlers: next}
}

func (h *MultiHandler) WithGroup(name string) slog.Handler {
	next := make([]slog.Handler, 0, len(h.handlers))
	for _, handler := range h.handlers {
		next = append(next, handler.WithGroup(name))
	}
	return &MultiHandler{handlers: next}
}
```

### Exercise 2: Test Handler Behavior

Create `routehandler_test.go`:

```go
package routehandler

import (
	"bytes"
	"errors"
	"fmt"
	"log/slog"
	"strings"
	"testing"
)

func textHandler(buf *bytes.Buffer, level slog.Level) slog.Handler {
	return slog.NewTextHandler(buf, &slog.HandlerOptions{
		Level: level,
		ReplaceAttr: func(groups []string, a slog.Attr) slog.Attr {
			if len(groups) == 0 && a.Key == slog.TimeKey {
				return slog.Attr{}
			}
			return a
		},
	})
}

func TestHandlerAddsRoute(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name  string
		route string
		want  string
	}{
		{name: "named", route: "audit", want: "route=audit"},
		{name: "default", route: "", want: "route=default"},
	}

	for _, tc := range tests {
		tc := tc
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()

			var buf bytes.Buffer
			handler, err := New(textHandler(&buf, slog.LevelInfo), tc.route)
			if err != nil {
				t.Fatalf("New() error = %v", err)
			}
			logger := slog.New(handler).WithGroup("svc").With(slog.String("component", "billing"))
			logger.Info("started")

			got := buf.String()
			for _, want := range []string{tc.want, "svc.component=billing", "msg=started"} {
				if !strings.Contains(got, want) {
					t.Fatalf("output %q missing %q", got, want)
				}
			}
		})
	}
}

func TestMultiHandlerRoutesToEnabledHandlers(t *testing.T) {
	t.Parallel()

	var infoBuf bytes.Buffer
	var warnBuf bytes.Buffer
	multi, err := NewMulti(textHandler(&infoBuf, slog.LevelInfo), textHandler(&warnBuf, slog.LevelWarn))
	if err != nil {
		t.Fatalf("NewMulti() error = %v", err)
	}
	logger := slog.New(multi)
	logger.Info("info event")
	logger.Warn("warn event")

	if strings.Count(infoBuf.String(), "level=") != 2 {
		t.Fatalf("info handler output = %q, want two records", infoBuf.String())
	}
	if strings.Count(warnBuf.String(), "level=") != 1 || strings.Contains(warnBuf.String(), "info event") {
		t.Fatalf("warn handler output = %q, want only warn record", warnBuf.String())
	}
}

func TestValidationErrors(t *testing.T) {
	t.Parallel()

	_, err := New(nil, "audit")
	if !errors.Is(err, ErrNilHandler) {
		t.Fatalf("New(nil) error = %v, want ErrNilHandler", err)
	}
	_, err = NewMulti(textHandler(new(bytes.Buffer), slog.LevelInfo), nil)
	if !errors.Is(err, ErrNilHandler) {
		t.Fatalf("NewMulti(..., nil) error = %v, want ErrNilHandler", err)
	}
}

func ExampleNew() {
	var buf bytes.Buffer
	handler, _ := New(textHandler(&buf, slog.LevelInfo), "audit")
	slog.New(handler).Info("started")
	fmt.Print(buf.String())
	// Output: level=INFO msg=started route=audit
}
```

Your turn: add a test proving `Handler.Name()` reports `default` when the constructor receives an empty name.

### Exercise 3: Add a Demo Command

Create `cmd/demo/main.go`:

```go
package main

import (
	"bytes"
	"fmt"
	"log/slog"

	"example.com/slogcustomhandler"
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
	handler, err := routehandler.New(inner, "audit")
	if err != nil {
		panic(err)
	}
	slog.New(handler).Info("started")
	fmt.Print(buf.String())
}
```

## Common Mistakes

Wrong: mutate the incoming `slog.Record` directly.

What happens: hidden attribute state can be shared in surprising ways.

Fix: call `r.Clone()` before adding or changing attributes.

Wrong: have `WithAttrs` mutate the receiver.

What happens: sibling loggers leak attributes into each other.

Fix: return a new handler value with a wrapped inner handler.

Wrong: ignore `Enabled` in a fan-out handler.

What happens: handlers receive records they explicitly disabled.

Fix: check each target handler's `Enabled` method before calling `Handle`.

## Verification

From `~/go-exercises/slog-custom-handler`:

```bash
test -z "$(gofmt -l .)"
go vet ./...
go build ./...
go test -count=1 -race ./...
go run ./cmd/demo
```

Add one test of your own for `WithAttrs` or `WithGroup` immutability.

## Summary

- A custom handler must implement `Enabled`, `Handle`, `WithAttrs`, and `WithGroup`.
- Use `Record.Clone()` before modifying a record.
- Handler wrappers are usually safer than reimplementing formatting.
- Fan-out handlers should respect each target's `Enabled` decision.

## What's Next

Next, enrich records from `context.Context` in [Context-Aware Logging](../07-context-aware-logging/07-context-aware-logging.md).

## Resources

- `slog.Handler` documentation: https://pkg.go.dev/log/slog#Handler
- `slog.Record.Clone` documentation: https://pkg.go.dev/log/slog#Record.Clone
- Go slog handler guide: https://github.com/golang/example/blob/master/slog-handler-guide/README.md
- `log/slog` handler overview: https://pkg.go.dev/log/slog#hdr-Writing_a_handler
