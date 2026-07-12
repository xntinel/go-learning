# Exercise 3: A custom slog.Handler that injects request_id and trace_id from context

Structured logs are only useful if you can reassemble one request out of the
interleaved output of a thousand. That requires a correlation id on every line —
and threading it through every function signature is a losing battle. This module
builds a `ContextHandler` that wraps any base `slog.Handler` and, in its `Handle`
method, pulls `request_id` and `trace_id` out of the `context.Context` and stamps
them on every record, so inbound middleware sets them once and every line is
correlated automatically.

This module is fully self-contained: its own `go mod init`, demo, and tests.
Nothing here imports any other exercise.

## What you'll build

```text
ctxlog/                      independent module: example.com/ctxlog
  go.mod                     go 1.25
  ctxlog.go                  ContextHandler wrapping a base Handler; WithRequestID/WithTraceID context keys
  cmd/
    demo/
      main.go                runnable demo: two requests, correlated lines
  ctxlog_test.go             every line carries request_id; WithAttrs/WithGroup delegate
```

- Files: `ctxlog.go`, `cmd/demo/main.go`, `ctxlog_test.go`.
- Implement: a `ContextHandler` implementing `slog.Handler` (`Enabled`, `Handle`, `WithAttrs`, `WithGroup`) that reads `request_id`/`trace_id` from context in `Handle` and adds them via `Record.AddAttrs`; context setters `WithRequestID`/`WithTraceID`.
- Test: put a known `request_id` in the context, log several lines into a buffer, assert every line carries it; verify `WithAttrs`/`WithGroup` delegate to the inner handler so field chaining still works.
- Verify: `go test -count=1 -race ./...`

Set up the module:

```bash
mkdir -p go-solutions/10-error-handling/14-error-observability/03-context-scoped-correlation-fields/cmd/demo
cd go-solutions/10-error-handling/14-error-observability/03-context-scoped-correlation-fields
go mod edit -go=1.25
```

### The four-method interface, and why delegation matters

`slog.Handler` has four methods and a wrapping handler must handle all four
correctly, not just the interesting one:

- `Enabled(ctx, level) bool` — the level filter. Delegate to the inner handler so
  the wrapper does not change what levels are on.
- `Handle(ctx, record) error` — the one method that does real work here: read the
  correlation ids from `ctx`, `AddAttrs` them to the record, then hand the record
  to the inner handler.
- `WithAttrs([]slog.Attr) slog.Handler` and `WithGroup(string) slog.Handler` —
  called when a caller does `logger.With(...)` or `logger.WithGroup(...)`. They
  must return a *new* `ContextHandler` wrapping the inner handler's result, so
  that pre-bound attributes and groups still work *and* the returned handler
  still injects correlation ids. Forgetting to re-wrap here is the classic bug:
  `logger.With("k","v").Info(...)` would silently lose the correlation stamping
  because the returned handler is the bare inner one.

The context keys use an unexported `ctxKey` type, not a bare string, so no other
package can collide with the same key — the standard Go idiom for context values.
`Handle` reads with a comma-ok type assertion and only adds an attribute when the
id is actually present, so a request without a trace id simply omits `trace_id`
rather than logging an empty one.

One subtlety worth stating: `slog.Record` is passed to `Handle` by value, and
`AddAttrs` mutates the receiver's internal slice. Because each `Handle` call gets
its own copy of the record, adding attributes here is safe and does not leak into
other handlers or other calls.

A second subtlety, which the `WithGroup` test pins: when a caller has opened a
group with `logger.WithGroup("op")`, the inner handler nests *every* record
attribute under `op` — including the `request_id` this handler injects, since it
adds it to the same record. So under an open group the correlation id appears at
`op.request_id`, not top-level. That is an honest consequence of slog's group
semantics, not a defect; if you need correlation always at a stable top-level
path, add the context handler *outermost* and open groups only for
operation-scoped sub-attributes below it.

Create `ctxlog.go`:

```go
package ctxlog

import (
	"context"
	"log/slog"
)

type ctxKey int

const (
	requestIDKey ctxKey = iota
	traceIDKey
)

// WithRequestID returns a child context carrying a request id for correlation.
func WithRequestID(ctx context.Context, id string) context.Context {
	return context.WithValue(ctx, requestIDKey, id)
}

// WithTraceID returns a child context carrying a trace id for correlation.
func WithTraceID(ctx context.Context, id string) context.Context {
	return context.WithValue(ctx, traceIDKey, id)
}

// ContextHandler wraps a base slog.Handler and stamps request_id/trace_id from
// the context onto every record, so no call site has to add them by hand.
type ContextHandler struct {
	inner slog.Handler
}

// NewContextHandler wraps base so every emitted record is correlated.
func NewContextHandler(base slog.Handler) *ContextHandler {
	return &ContextHandler{inner: base}
}

func (h *ContextHandler) Enabled(ctx context.Context, level slog.Level) bool {
	return h.inner.Enabled(ctx, level)
}

func (h *ContextHandler) Handle(ctx context.Context, r slog.Record) error {
	if id, ok := ctx.Value(requestIDKey).(string); ok {
		r.AddAttrs(slog.String("request_id", id))
	}
	if id, ok := ctx.Value(traceIDKey).(string); ok {
		r.AddAttrs(slog.String("trace_id", id))
	}
	return h.inner.Handle(ctx, r)
}

// WithAttrs re-wraps so pre-bound attributes AND correlation stamping both apply.
func (h *ContextHandler) WithAttrs(attrs []slog.Attr) slog.Handler {
	return &ContextHandler{inner: h.inner.WithAttrs(attrs)}
}

// WithGroup re-wraps so an opened group AND correlation stamping both apply.
func (h *ContextHandler) WithGroup(name string) slog.Handler {
	return &ContextHandler{inner: h.inner.WithGroup(name)}
}
```

### The runnable demo

The demo simulates two concurrent-looking requests with different ids. Because
JSON key order is stable and correlation ids are injected last, each line ends
with its own `request_id`. The demo logs to stdout so the Expected-output block
is the log itself; the time field is suppressed with a `ReplaceAttr` so output is
deterministic.

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
	// Drop the time attribute so the demo output is stable.
	opts := &slog.HandlerOptions{ReplaceAttr: func(_ []string, a slog.Attr) slog.Attr {
		if a.Key == slog.TimeKey {
			return slog.Attr{}
		}
		return a
	}}
	logger := slog.New(ctxlog.NewContextHandler(slog.NewJSONHandler(os.Stdout, opts)))

	handle := func(reqID, user string) {
		ctx := ctxlog.WithRequestID(context.Background(), reqID)
		logger.InfoContext(ctx, "request received", "user", user)
		logger.ErrorContext(ctx, "operation failed", "err", "not found")
	}

	handle("req-1", "alice")
	handle("req-2", "bob")
}
```

Run it:

```bash
go run ./cmd/demo
```

Expected output:

```
{"level":"INFO","msg":"request received","user":"alice","request_id":"req-1"}
{"level":"ERROR","msg":"operation failed","err":"not found","request_id":"req-1"}
{"level":"INFO","msg":"request received","user":"bob","request_id":"req-2"}
{"level":"ERROR","msg":"operation failed","err":"not found","request_id":"req-2"}
```

### Tests

The tests log through the wrapped handler into a buffer and assert on the raw
JSON. `TestEveryLineCorrelated` proves both lines from one request carry the
`request_id` and `trace_id`. `TestWithAttrsDelegates` proves the re-wrap works:
after `logger.With("component","auth")`, a line still carries *both* the bound
`component` attribute *and* the injected `request_id`, which only holds if
`WithAttrs` returned a `ContextHandler`, not the bare inner handler.

Create `ctxlog_test.go`:

```go
package ctxlog

import (
	"bytes"
	"context"
	"encoding/json"
	"strings"
	"testing"

	"log/slog"
)

func decodeLines(t *testing.T, buf *bytes.Buffer) []map[string]any {
	t.Helper()
	var out []map[string]any
	for _, line := range strings.Split(strings.TrimSpace(buf.String()), "\n") {
		if line == "" {
			continue
		}
		var m map[string]any
		if err := json.Unmarshal([]byte(line), &m); err != nil {
			t.Fatalf("bad json line %q: %v", line, err)
		}
		out = append(out, m)
	}
	return out
}

func TestEveryLineCorrelated(t *testing.T) {
	t.Parallel()
	var buf bytes.Buffer
	logger := slog.New(NewContextHandler(slog.NewJSONHandler(&buf, nil)))

	ctx := WithTraceID(WithRequestID(context.Background(), "req-abc"), "trace-xyz")
	logger.InfoContext(ctx, "one")
	logger.ErrorContext(ctx, "two", "err", "boom")

	lines := decodeLines(t, &buf)
	if len(lines) != 2 {
		t.Fatalf("got %d lines, want 2", len(lines))
	}
	for i, m := range lines {
		if m["request_id"] != "req-abc" {
			t.Fatalf("line %d request_id = %v, want req-abc", i, m["request_id"])
		}
		if m["trace_id"] != "trace-xyz" {
			t.Fatalf("line %d trace_id = %v, want trace-xyz", i, m["trace_id"])
		}
	}
}

func TestNoIDsNoFields(t *testing.T) {
	t.Parallel()
	var buf bytes.Buffer
	logger := slog.New(NewContextHandler(slog.NewJSONHandler(&buf, nil)))
	logger.InfoContext(context.Background(), "bare")

	m := decodeLines(t, &buf)[0]
	if _, ok := m["request_id"]; ok {
		t.Fatalf("request_id present with no id in context: %v", m)
	}
}

func TestWithAttrsDelegates(t *testing.T) {
	t.Parallel()
	var buf bytes.Buffer
	logger := slog.New(NewContextHandler(slog.NewJSONHandler(&buf, nil))).With("component", "auth")

	ctx := WithRequestID(context.Background(), "req-1")
	logger.InfoContext(ctx, "bound")

	m := decodeLines(t, &buf)[0]
	if m["component"] != "auth" {
		t.Fatalf("bound attr lost; component = %v", m["component"])
	}
	if m["request_id"] != "req-1" {
		t.Fatalf("correlation lost after With(); request_id = %v", m["request_id"])
	}
}

func TestWithGroupDelegates(t *testing.T) {
	t.Parallel()
	var buf bytes.Buffer
	logger := slog.New(NewContextHandler(slog.NewJSONHandler(&buf, nil))).WithGroup("op")

	ctx := WithRequestID(context.Background(), "req-1")
	logger.InfoContext(ctx, "grouped", "name", "Get")

	m := decodeLines(t, &buf)[0]
	// WithGroup delegates: the grouped attr nests under "op". Because Handle adds
	// the correlation id to the record and the inner handler has "op" open, the
	// injected request_id nests under "op" too -- an honest consequence of slog's
	// group semantics, not a bug.
	grp, ok := m["op"].(map[string]any)
	if !ok || grp["name"] != "Get" {
		t.Fatalf("group delegation broken; op = %v", m["op"])
	}
	if grp["request_id"] != "req-1" {
		t.Fatalf("correlation lost after WithGroup(); op.request_id = %v", grp["request_id"])
	}
}
```

## Review

The handler is correct when correlation is total and delegation is faithful. Total
means every line from a request carries its ids without the call site adding them
(`TestEveryLineCorrelated`), and a request with no ids logs cleanly with no empty
fields (`TestNoIDsNoFields`). Faithful means `WithAttrs` and `WithGroup` re-wrap:
`TestWithAttrsDelegates` and `TestWithGroupDelegates` fail loudly if either
returns the bare inner handler and drops correlation — the single most common bug
when writing a wrapping `slog.Handler`.

Note why `AddAttrs` on the by-value `Record` is safe: `Handle` receives its own
copy, so injecting attributes does not race with or leak into other handlers.
Note also that correlation ids are stamped in `Handle` from the context passed to
`InfoContext`/`ErrorContext` — which is exactly why the service layer in
Exercise 1 uses `ErrorContext` and not `Error`. If it used `Error`, the context
would never reach this handler and the lines would be uncorrelated.

## Resources

- [`log/slog` Handler](https://pkg.go.dev/log/slog#Handler) — the four-method interface and the `Record`/`AddAttrs` contract.
- [Structured Logging with slog](https://go.dev/blog/slog) — the official guide's "wrapping a handler" and context sections.
- [`context`](https://pkg.go.dev/context) — `context.WithValue` and the unexported-key idiom for request-scoped values.

---

Back to [00-concepts.md](00-concepts.md) | Next: [04-logvaluer-error-enrichment.md](04-logvaluer-error-enrichment.md)
