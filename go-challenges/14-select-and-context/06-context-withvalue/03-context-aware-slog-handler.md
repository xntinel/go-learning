# Exercise 3: Context-Aware slog Handler

This is the production payoff of `WithValue`. `slog.Handler.Handle` receives the
context precisely so a handler can enrich every record from request-scoped values.
This exercise builds a handler that wraps a JSON handler and, in `Handle`, pulls
`TraceID`/`UserID` out of the context and attaches them to the record — so every
`InfoContext` line is correlated for free, with nothing threaded through function
signatures.

This module is fully self-contained: its own `go mod init`, all code inline, its
own demo and tests. Nothing here imports any other exercise.

## What you'll build

```text
ctxslog/                     independent module: example.com/ctxslog
  go.mod
  meta.go                    Meta; type key struct{}; WithMeta, FromContext
  handler.go                 ContextHandler wrapping a slog.Handler; NewContextHandler
  cmd/
    demo/
      main.go                logs one correlated line to stdout
  ctxslog_test.go            attrs present with Meta / absent without; delegation; -race
```

Files: `meta.go`, `handler.go`, `cmd/demo/main.go`, `ctxslog_test.go`.
Implement: a `ContextHandler` that in `Handle(ctx, record)` reads `Meta` from `ctx` and calls `record.AddAttrs`, delegating `Enabled`/`WithAttrs`/`WithGroup` correctly.
Test: `trace_id`/`user_id` appear when the context carries `Meta` and are absent when it does not; a `WithAttrs` attribute survives the wrapping; the handler mutates a copy per `Handle`.
Verify: `go test -count=1 -race ./...`

Set up the module:

```bash
mkdir -p ~/go-exercises/ctxslog/cmd/demo
cd ~/go-exercises/ctxslog
go mod init example.com/ctxslog
```

### Why wrap the handler, clone the record, and re-wrap on WithAttrs

A `slog.Handler` is the extension point of `log/slog`. By embedding a delegate
handler and overriding only `Handle`, `ContextHandler` inherits the delegate's
output format (JSON here) while injecting context-derived attributes on every
record. Handlers call `logger.InfoContext(ctx, msg)` with no fields, and the
correlation attributes appear automatically — the single highest-leverage use of
`WithValue` in modern Go.

Three subtleties make it correct rather than merely compiling:

Clone the record before mutating. The slog documentation instructs a handler that
modifies a `Record` to call `Record.Clone` first, because a `Record` stores its
attributes partly in an inline array and partly in a shared backing slice; mutating
the received record could corrupt state the caller or a sibling handler still holds.
`r = r.Clone()` before `AddAttrs` makes each `Handle` operate on its own copy — which
is exactly why the handler is safe under concurrent calls with no shared mutable
state.

Re-wrap on `WithAttrs` and `WithGroup`. `logger.With(...)` calls the handler's
`WithAttrs`, which returns a *new* handler. If `ContextHandler` embedded the
delegate and did not override `WithAttrs`, that call would return the bare delegate
— silently unwrapping the `ContextHandler`, so every subsequent log line would lose
its context enrichment. Overriding both to re-wrap the delegate's result preserves
the enrichment through `With`/`WithGroup` chains. This is a real, easy-to-miss
production bug, and a test pins it.

Emit no empty attributes. Only attach `trace_id` when `TraceID` is non-empty (and
likewise `user_id`), so a request without metadata produces a clean line with no
`"trace_id":""` noise. `Enabled` is context-independent and is inherited from the
embedded delegate unchanged.

Create `meta.go`:

```go
package ctxslog

import "context"

// Meta is the request-scoped data a log line should be correlated with.
type Meta struct {
	TraceID string
	UserID  string
}

type key struct{}

// WithMeta attaches m to ctx.
func WithMeta(ctx context.Context, m Meta) context.Context {
	return context.WithValue(ctx, key{}, m)
}

// FromContext extracts Meta; ok is false when none is present.
func FromContext(ctx context.Context) (Meta, bool) {
	m, ok := ctx.Value(key{}).(Meta)
	return m, ok
}
```

Create `handler.go`:

```go
package ctxslog

import (
	"context"
	"log/slog"
)

// ContextHandler wraps a delegate slog.Handler and enriches every record with
// request-scoped attributes pulled from the context.
type ContextHandler struct {
	slog.Handler
}

// NewContextHandler wraps delegate so that Handle injects context Meta.
func NewContextHandler(delegate slog.Handler) *ContextHandler {
	return &ContextHandler{Handler: delegate}
}

// Handle attaches trace_id/user_id from the context, then delegates. It clones
// the record before mutating it, per the slog handler contract.
func (h *ContextHandler) Handle(ctx context.Context, r slog.Record) error {
	m, ok := FromContext(ctx)
	if !ok {
		return h.Handler.Handle(ctx, r)
	}

	var attrs []slog.Attr
	if m.TraceID != "" {
		attrs = append(attrs, slog.String("trace_id", m.TraceID))
	}
	if m.UserID != "" {
		attrs = append(attrs, slog.String("user_id", m.UserID))
	}
	if len(attrs) == 0 {
		return h.Handler.Handle(ctx, r)
	}

	r = r.Clone()
	r.AddAttrs(attrs...)
	return h.Handler.Handle(ctx, r)
}

// WithAttrs re-wraps the delegate so enrichment survives logger.With(...).
func (h *ContextHandler) WithAttrs(attrs []slog.Attr) slog.Handler {
	return &ContextHandler{Handler: h.Handler.WithAttrs(attrs)}
}

// WithGroup re-wraps the delegate so enrichment survives logger.WithGroup(...).
func (h *ContextHandler) WithGroup(name string) slog.Handler {
	return &ContextHandler{Handler: h.Handler.WithGroup(name)}
}
```

### The demo

The demo logs one line through the wrapper to stdout with a context carrying a
trace and user, showing the correlation attributes appear without being passed to
`InfoContext`. To keep the output deterministic, it strips the volatile `time`
attribute with a `ReplaceAttr` hook.

Create `cmd/demo/main.go`:

```go
package main

import (
	"context"
	"log/slog"
	"os"

	"example.com/ctxslog"
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
	logger := slog.New(ctxslog.NewContextHandler(slog.NewJSONHandler(os.Stdout, opts)))

	ctx := ctxslog.WithMeta(context.Background(), ctxslog.Meta{TraceID: "tr-1", UserID: "u-7"})
	logger.InfoContext(ctx, "order created")
}
```

Run it:

```bash
go run ./cmd/demo
```

Expected output:

```
{"level":"INFO","msg":"order created","trace_id":"tr-1","user_id":"u-7"}
```

### The tests

The tests log into a `bytes.Buffer` and `json.Unmarshal` the line. With `Meta` in
the context the trace/user attributes appear; without it they are absent (no empty
keys). `TestWithAttrsSurvivesWrapping` proves the re-wrap: after `logger.With`, a
later `InfoContext` still carries both the `With` attribute and the context
enrichment. `TestConcurrentHandleIsRaceFree` runs many `Handle` calls in parallel
to prove the clone-per-call design has no shared mutable state.

Create `ctxslog_test.go`:

```go
package ctxslog

import (
	"bytes"
	"context"
	"encoding/json"
	"log/slog"
	"sync"
	"testing"
)

func newLogger(buf *bytes.Buffer) *slog.Logger {
	opts := &slog.HandlerOptions{
		ReplaceAttr: func(groups []string, a slog.Attr) slog.Attr {
			if a.Key == slog.TimeKey {
				return slog.Attr{}
			}
			return a
		},
	}
	return slog.New(NewContextHandler(slog.NewJSONHandler(buf, opts)))
}

func decode(t *testing.T, buf *bytes.Buffer) map[string]any {
	t.Helper()
	var m map[string]any
	if err := json.Unmarshal(bytes.TrimSpace(buf.Bytes()), &m); err != nil {
		t.Fatalf("unmarshal %q: %v", buf.String(), err)
	}
	return m
}

func TestEnrichesFromContext(t *testing.T) {
	t.Parallel()

	var buf bytes.Buffer
	logger := newLogger(&buf)

	ctx := WithMeta(context.Background(), Meta{TraceID: "tr-9", UserID: "u-3"})
	logger.InfoContext(ctx, "hello")

	m := decode(t, &buf)
	if m["trace_id"] != "tr-9" {
		t.Fatalf("trace_id = %v, want tr-9", m["trace_id"])
	}
	if m["user_id"] != "u-3" {
		t.Fatalf("user_id = %v, want u-3", m["user_id"])
	}
}

func TestNoMetaNoEmptyAttrs(t *testing.T) {
	t.Parallel()

	var buf bytes.Buffer
	logger := newLogger(&buf)

	logger.InfoContext(context.Background(), "hello")

	m := decode(t, &buf)
	if _, ok := m["trace_id"]; ok {
		t.Fatalf("trace_id present without Meta: %v", m["trace_id"])
	}
	if _, ok := m["user_id"]; ok {
		t.Fatalf("user_id present without Meta: %v", m["user_id"])
	}
}

func TestWithAttrsSurvivesWrapping(t *testing.T) {
	t.Parallel()

	var buf bytes.Buffer
	logger := newLogger(&buf).With(slog.String("component", "orders"))

	ctx := WithMeta(context.Background(), Meta{TraceID: "tr-1", UserID: "u-1"})
	logger.InfoContext(ctx, "hello")

	m := decode(t, &buf)
	if m["component"] != "orders" {
		t.Fatalf("component = %v, want orders (WithAttrs lost)", m["component"])
	}
	if m["trace_id"] != "tr-1" {
		t.Fatalf("trace_id = %v, want tr-1 (enrichment lost after With)", m["trace_id"])
	}
}

func TestConcurrentHandleIsRaceFree(t *testing.T) {
	t.Parallel()

	var buf bytes.Buffer
	logger := newLogger(&buf)

	var wg sync.WaitGroup
	for range 50 {
		wg.Add(1)
		go func() {
			defer wg.Done()
			ctx := WithMeta(context.Background(), Meta{TraceID: "t", UserID: "u"})
			logger.InfoContext(ctx, "n")
		}()
	}
	wg.Wait()
}
```

## Review

The handler is correct when a log call with no explicit fields still emits the
context's `trace_id` and `user_id`, and a call with no `Meta` emits neither key.
The two design decisions that separate a working handler from a subtly broken one
are the record clone and the `WithAttrs`/`WithGroup` re-wrap. Skip the clone and a
handler that later fans out to multiple delegates can corrupt shared record state;
skip the re-wrap and the first `logger.With(...)` silently unwraps your handler so
every subsequent line loses its correlation — `TestWithAttrsSurvivesWrapping` is
the guard against exactly that regression. `Enabled` is inherited unchanged because
it does not depend on the context. Run `go test -race`: because each `Handle`
mutates only its own cloned record, concurrent logging through one logger is clean.

## Resources

- [log/slog Handler](https://pkg.go.dev/log/slog#Handler) — the four-method interface and the `Handle(ctx, Record)` contract.
- [slog.Record.Clone](https://pkg.go.dev/log/slog#Record.Clone) — why a middleware handler must clone before mutating.
- [Go Blog: A Guide to Writing slog Handlers](https://go.dev/blog/slog-handlers) — the canonical guide to wrapping handlers correctly, including `WithAttrs`/`WithGroup`.

---

Back to [00-concepts.md](00-concepts.md) | Previous: [02-request-id-middleware.md](02-request-id-middleware.md) | Next: [04-auth-principal-guard.md](04-auth-principal-guard.md)
