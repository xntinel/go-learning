# Exercise 3: A slog.Handler That Injects Request-Scoped Attributes

Observability plumbing that stamps every log line with a request id or trace id
is built by extending `slog.Handler`. You embed the real handler, override
`Handle` to pull the id from the context, and — the part everyone gets wrong once
— re-wrap `WithAttrs`/`WithGroup` so the override survives child loggers.

This module is fully self-contained: its own `go mod init`, all code inline, its
own demo and tests. Nothing here imports another exercise.

## What you'll build

```text
ctxlog/                    independent module: example.com/ctxlog
  go.mod                   go 1.26
  ctxlog.go                contextHandler embeds slog.Handler; Handle override; WithAttrs/WithGroup re-wrap
  cmd/
    demo/
      main.go              runnable demo: log with a request id in context
  ctxlog_test.go           id appears; With(...) still injects; Enabled delegated
```

- Files: `ctxlog.go`, `cmd/demo/main.go`, `ctxlog_test.go`.
- Implement: `contextHandler` embedding `slog.Handler`, overriding `Handle` to `AddAttrs` a request id from the context, and re-wrapping `WithAttrs`/`WithGroup` to return a `contextHandler`.
- Test: the id attribute appears in output; a logger built via `With(...)` still injects the id (proving the re-wrap); `Enabled` is delegated to the base.
- Verify: `go test -count=1 -race ./...`

### Overriding Handle, and why WithAttrs must be re-wrapped

`slog.Handler` is a four-method interface: `Enabled`, `Handle`, `WithAttrs`, and
`WithGroup`. To enrich records with a request id you only need to touch `Handle`,
which receives the `context.Context` and the `slog.Record`. You pull the id out of
the context and call `record.AddAttrs(slog.String("request_id", id))` before
delegating to the embedded handler's `Handle`. Because `contextHandler` embeds
`slog.Handler`, the other three methods are promoted for free — so far so good.

The trap is `WithAttrs` and `WithGroup`. `slog` calls these to build derived
handlers: `logger.With("service", "api")` calls `handler.WithAttrs(...)` and
stores the result. If you leave `WithAttrs` promoted, the embedded handler's
`WithAttrs` runs and returns *its own* type — a plain `*slog.TextHandler`, not
your `contextHandler`. From that point on, the derived logger has lost your
`Handle` override entirely, and request ids silently stop appearing for any logger
that carries attributes. The fix is to override `WithAttrs`/`WithGroup` so they
call the inner handler and box the result back into a `contextHandler`, preserving
the override down the whole chain of derived loggers. `Enabled` needs no such
treatment — it returns a bool, not a handler — so it stays promoted.

One API nicety: `WithGroup("")` must return the receiver unchanged per the
`slog.Handler` contract, so the override checks for the empty name.

Create `ctxlog.go`:

```go
package ctxlog

import (
	"context"
	"log/slog"
)

type ctxKey struct{}

// WithRequestID returns a context carrying the request id for the logger to pick
// up in Handle.
func WithRequestID(ctx context.Context, id string) context.Context {
	return context.WithValue(ctx, ctxKey{}, id)
}

func requestID(ctx context.Context) (string, bool) {
	id, ok := ctx.Value(ctxKey{}).(string)
	return id, ok
}

// contextHandler wraps a slog.Handler and stamps every record with the request
// id found in the context. It embeds the handler so Enabled is promoted, and
// re-wraps WithAttrs/WithGroup so the Handle override survives derived handlers.
type contextHandler struct {
	slog.Handler
}

// NewContextHandler wraps inner so that Handle injects the context request id.
func NewContextHandler(inner slog.Handler) *contextHandler {
	return &contextHandler{Handler: inner}
}

// Handle is the override: enrich the record, then delegate to the embedded handler.
func (h *contextHandler) Handle(ctx context.Context, r slog.Record) error {
	if id, ok := requestID(ctx); ok {
		r.AddAttrs(slog.String("request_id", id))
	}
	return h.Handler.Handle(ctx, r)
}

// WithAttrs re-wraps so a derived handler keeps the Handle override.
func (h *contextHandler) WithAttrs(attrs []slog.Attr) slog.Handler {
	return &contextHandler{Handler: h.Handler.WithAttrs(attrs)}
}

// WithGroup re-wraps as well; an empty name returns the receiver per contract.
func (h *contextHandler) WithGroup(name string) slog.Handler {
	if name == "" {
		return h
	}
	return &contextHandler{Handler: h.Handler.WithGroup(name)}
}
```

### The runnable demo

The demo logs one line with a request id in the context. To keep the output
deterministic it strips the timestamp with a `ReplaceAttr` hook (real logs keep
it). The request id appears after the call's own attributes because `AddAttrs`
appends it to the record.

Create `cmd/demo/main.go`:

```go
package main

import (
	"context"
	"log/slog"
	"os"

	"example.com/ctxlog"
)

func main() {
	opts := &slog.HandlerOptions{
		ReplaceAttr: func(groups []string, a slog.Attr) slog.Attr {
			if a.Key == slog.TimeKey {
				return slog.Attr{}
			}
			return a
		},
	}
	base := slog.NewTextHandler(os.Stdout, opts)
	logger := slog.New(ctxlog.NewContextHandler(base))

	ctx := ctxlog.WithRequestID(context.Background(), "req-42")
	logger.InfoContext(ctx, "handling payment", "amount", 100)
}
```

Run it:

```bash
go run ./cmd/demo
```

Expected output:

```
level=INFO msg="handling payment" amount=100 request_id=req-42
```

### Tests

`TestInjectsRequestID` logs into a `bytes.Buffer` with an id in the context and
asserts `request_id=req-99` appears. `TestWithAttrsKeepsOverride` is the one that
proves the re-wrap: it derives a logger with `With("service", "api")` and asserts
the derived logger *still* injects the request id — which only holds if
`WithAttrs` returned a `contextHandler`. `TestEnabledDelegated` builds a handler
at `LevelError` and confirms `Enabled(Info)` is false, i.e. the promoted `Enabled`
reaches the base's level check.

Create `ctxlog_test.go`:

```go
package ctxlog

import (
	"bytes"
	"context"
	"log/slog"
	"strings"
	"testing"
)

func TestInjectsRequestID(t *testing.T) {
	t.Parallel()
	var buf bytes.Buffer
	logger := slog.New(NewContextHandler(slog.NewTextHandler(&buf, nil)))

	ctx := WithRequestID(context.Background(), "req-99")
	logger.InfoContext(ctx, "hello")

	if got := buf.String(); !strings.Contains(got, "request_id=req-99") {
		t.Fatalf("log missing request id; got %q", got)
	}
}

func TestNoRequestIDWhenAbsent(t *testing.T) {
	t.Parallel()
	var buf bytes.Buffer
	logger := slog.New(NewContextHandler(slog.NewTextHandler(&buf, nil)))

	logger.InfoContext(context.Background(), "hello")

	if got := buf.String(); strings.Contains(got, "request_id=") {
		t.Fatalf("log should not carry a request id; got %q", got)
	}
}

func TestWithAttrsKeepsOverride(t *testing.T) {
	t.Parallel()
	var buf bytes.Buffer
	logger := slog.New(NewContextHandler(slog.NewTextHandler(&buf, nil)))

	derived := logger.With("service", "api")
	ctx := WithRequestID(context.Background(), "req-7")
	derived.InfoContext(ctx, "hello")

	got := buf.String()
	if !strings.Contains(got, "service=api") {
		t.Errorf("derived logger dropped its own attrs; got %q", got)
	}
	if !strings.Contains(got, "request_id=req-7") {
		t.Errorf("derived logger lost the Handle override; got %q", got)
	}
}

func TestEnabledDelegated(t *testing.T) {
	t.Parallel()
	opts := &slog.HandlerOptions{Level: slog.LevelError}
	h := NewContextHandler(slog.NewTextHandler(&bytes.Buffer{}, opts))

	if h.Enabled(context.Background(), slog.LevelInfo) {
		t.Error("Enabled(Info) should be false when base level is Error")
	}
	if !h.Enabled(context.Background(), slog.LevelError) {
		t.Error("Enabled(Error) should be true")
	}
}
```

## Review

The handler is correct when the request id rides on every line a logger emits,
including loggers derived with `With(...)`. `TestWithAttrsKeepsOverride` is the
proof that matters: delete the `WithAttrs` override and it fails, because
`logger.With(...)` would return a bare `TextHandler` and the id would vanish for
derived loggers — the exact silent bug that makes context enrichment "work in the
simple test and disappear in production." Leave `Enabled` promoted (delegated) and
`WithGroup("")` returning the receiver to honor the `slog.Handler` contract
precisely. There is no shared mutable state, but run `go test -race` anyway to keep
the concurrent-logging path honest.

## Resources

- [`log/slog.Handler`](https://pkg.go.dev/log/slog#Handler) — the four-method interface and the `WithAttrs`/`WithGroup`/`Enabled`/`Handle` contract.
- [`log/slog.Record.AddAttrs`](https://pkg.go.dev/log/slog#Record.AddAttrs) — appending attributes to a record inside `Handle`.
- [A Guide to Writing slog Handlers](https://github.com/golang/example/blob/master/slog-handler-guide/README.md) — the official guide, including the wrap-and-re-wrap pattern.

---

Back to [00-concepts.md](00-concepts.md) | Next: [04-rwmutex-embedded-store.md](04-rwmutex-embedded-store.md)
