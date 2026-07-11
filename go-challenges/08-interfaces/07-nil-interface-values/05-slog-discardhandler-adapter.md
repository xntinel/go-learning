# Exercise 5: slog wiring — nil handler normalized to DiscardHandler

`slog.New(nil)` compiles, then panics on the first log line. This module builds
the production wiring: `NewLogger(h slog.Handler) *slog.Logger` normalizes a nil
Handler to `slog.DiscardHandler` (Go 1.24), plus a level-filtering wrapper
Handler that implements the four-method `Handler` interface and delegates — so
you prove you understand the contract, not just the constructor.

## What you'll build

```text
slogwiring/                independent module: example.com/slogwiring
  go.mod                   go 1.26
  logging.go               NewLogger; levelHandler (Enabled/Handle/WithAttrs/WithGroup)
  cmd/
    demo/
      main.go              JSON handler to stdout; nil handler discards
  logging_test.go          nil discards; JSON parses; level filter; WithAttrs forwards
```

- Files: `logging.go`, `cmd/demo/main.go`, `logging_test.go`.
- Implement: `NewLogger(h slog.Handler)` that maps nil to `slog.DiscardHandler`, and `NewLevelHandler(min slog.Level, inner slog.Handler)` returning a `Handler` that drops records below `min` and delegates the other three methods.
- Test: `NewLogger(nil).Info(...)` does not panic and emits nothing; the JSON path writes parseable JSON to a buffer; the wrapper honors `Enabled` at the configured level and forwards `WithAttrs`.
- Verify: `go test -count=1 -race ./...`

Set up the module:

```bash
mkdir -p ~/go-exercises/slogwiring/cmd/demo
cd ~/go-exercises/slogwiring
go mod init example.com/slogwiring
```

### Why normalize the Handler, and what the four methods mean

`slog.New` stores the Handler and dispatches every log call to it; if the
Handler is nil, the first `logger.Info` dereferences nil and panics. This is
worse than an eager panic because it can hide on a rarely-hit path. Go 1.24
added `slog.DiscardHandler`, the canonical no-op Handler whose `Enabled` always
returns false, so `NewLogger` maps a nil Handler to it at construction and every
downstream log call is safe.

The `Handler` interface is four methods, and the wrapper implements all four to
show they are understood:

- `Enabled(ctx, level) bool` — a fast pre-check `slog` calls before building a
  `Record`; returning false lets `slog` skip the work entirely. The wrapper
  returns false for any level below `min`, then delegates to the inner handler.
- `Handle(ctx, record)` — does the actual formatting/output. The wrapper simply
  forwards to the inner handler once the level passed.
- `WithAttrs(attrs) Handler` and `WithGroup(name) Handler` — these must return a
  *new* Handler carrying the added context, never mutate the receiver, because
  `slog` clones handlers as loggers accumulate attributes and groups. The
  wrapper clones itself with the inner handler's `WithAttrs`/`WithGroup` result,
  preserving its own `min` level.

Getting `WithAttrs`/`WithGroup` wrong — returning the same handler, or losing the
`min` filter — is the classic wrapper bug: attributes added via `logger.With(...)`
would vanish, or the level filter would stop applying after a `With` call.

Create `logging.go`:

```go
package slogwiring

import (
	"context"
	"log/slog"
)

// NewLogger normalizes a nil Handler to slog.DiscardHandler so the logger never
// panics on first use.
func NewLogger(h slog.Handler) *slog.Logger {
	if h == nil {
		h = slog.DiscardHandler
	}
	return slog.New(h)
}

// levelHandler wraps another Handler, dropping records below a minimum level.
type levelHandler struct {
	min   slog.Level
	inner slog.Handler
}

// NewLevelHandler returns a Handler that filters below min and delegates to
// inner. A nil inner is normalized to slog.DiscardHandler.
func NewLevelHandler(min slog.Level, inner slog.Handler) slog.Handler {
	if inner == nil {
		inner = slog.DiscardHandler
	}
	return &levelHandler{min: min, inner: inner}
}

func (h *levelHandler) Enabled(ctx context.Context, level slog.Level) bool {
	return level >= h.min && h.inner.Enabled(ctx, level)
}

func (h *levelHandler) Handle(ctx context.Context, r slog.Record) error {
	return h.inner.Handle(ctx, r)
}

func (h *levelHandler) WithAttrs(attrs []slog.Attr) slog.Handler {
	return &levelHandler{min: h.min, inner: h.inner.WithAttrs(attrs)}
}

func (h *levelHandler) WithGroup(name string) slog.Handler {
	return &levelHandler{min: h.min, inner: h.inner.WithGroup(name)}
}
```

### The runnable demo

The demo drops the volatile `time` attribute via `ReplaceAttr` so the output is
deterministic.

Create `cmd/demo/main.go`:

```go
package main

import (
	"fmt"
	"log/slog"
	"os"

	"example.com/slogwiring"
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

	log := slogwiring.NewLogger(slog.NewJSONHandler(os.Stdout, opts))
	log.Info("user login", "user", "alice")

	// A nil handler is normalized to DiscardHandler: this line produces nothing.
	quiet := slogwiring.NewLogger(nil)
	quiet.Info("this is discarded")

	fmt.Println("done")
}
```

Run it:

```bash
go run ./cmd/demo
```

Expected output:

```
{"level":"INFO","msg":"user login","user":"alice"}
done
```

### Tests

Create `logging_test.go`:

```go
package slogwiring

import (
	"bytes"
	"encoding/json"
	"log/slog"
	"strings"
	"testing"
)

func TestNilHandlerDiscards(t *testing.T) {
	t.Parallel()

	// Must not panic, and DiscardHandler reports disabled at every level.
	log := NewLogger(nil)
	log.Info("nothing happens here")

	if slog.DiscardHandler.Enabled(t.Context(), slog.LevelInfo) {
		t.Fatal("DiscardHandler should report Enabled == false")
	}
}

func TestJSONHandlerWritesParseableJSON(t *testing.T) {
	t.Parallel()

	var buf bytes.Buffer
	log := NewLogger(slog.NewJSONHandler(&buf, nil))
	log.Info("hello", "user", "alice")

	var rec map[string]any
	if err := json.Unmarshal(buf.Bytes(), &rec); err != nil {
		t.Fatalf("output is not valid JSON: %v", err)
	}
	if rec["msg"] != "hello" {
		t.Fatalf("msg = %v; want hello", rec["msg"])
	}
	if rec["user"] != "alice" {
		t.Fatalf("user = %v; want alice", rec["user"])
	}
}

func TestLevelHandlerFiltersBelowMin(t *testing.T) {
	t.Parallel()

	var buf bytes.Buffer
	inner := slog.NewJSONHandler(&buf, &slog.HandlerOptions{Level: slog.LevelDebug})
	log := slog.New(NewLevelHandler(slog.LevelWarn, inner))

	log.Info("dropped")
	log.Warn("kept")

	out := buf.String()
	if strings.Contains(out, "dropped") {
		t.Fatalf("Info below Warn should be filtered; got %q", out)
	}
	if !strings.Contains(out, "kept") {
		t.Fatalf("Warn should pass the filter; got %q", out)
	}
}

func TestLevelHandlerForwardsWithAttrs(t *testing.T) {
	t.Parallel()

	var buf bytes.Buffer
	inner := slog.NewJSONHandler(&buf, nil)
	log := slog.New(NewLevelHandler(slog.LevelInfo, inner)).With("service", "api")

	log.Warn("boom")

	var rec map[string]any
	if err := json.Unmarshal(buf.Bytes(), &rec); err != nil {
		t.Fatalf("output is not valid JSON: %v", err)
	}
	if rec["service"] != "api" {
		t.Fatalf("attr forwarded through WithAttrs missing: %v", rec)
	}
}
```

## Review

The wiring is correct when a nil Handler can never reach `slog.New` un-normalized
and the wrapper honors the full `Handler` contract. `NewLogger` maps nil to
`slog.DiscardHandler`, so `TestNilHandlerDiscards` logs without panic and reads
back `Enabled == false`. The JSON path writes parseable output that
`TestJSONHandlerWritesParseableJSON` decodes. The wrapper's `Enabled` gate is
proven by `TestLevelHandlerFiltersBelowMin` (an `Info` below a `Warn` minimum is
dropped), and `WithAttrs` forwarding is proven by
`TestLevelHandlerForwardsWithAttrs` — the `service` attribute survives a `With`
call, which only works because `WithAttrs` returns a new handler wrapping the
inner handler's `WithAttrs` result. The mistake to avoid is `slog.New(nil)`: it
does not panic at construction, only on the first log line.

## Resources

- [`log/slog` package](https://pkg.go.dev/log/slog) — the `Handler` interface, `slog.New`, and `slog.NewJSONHandler`.
- [`slog.DiscardHandler`](https://pkg.go.dev/log/slog#DiscardHandler) — the canonical no-op Handler (Go 1.24).
- [A Guide to Writing slog Handlers](https://github.com/golang/example/blob/master/slog-handler-guide/README.md) — the official guide to implementing the four methods correctly.

---

Back to [00-concepts.md](00-concepts.md) | Next: [06-closer-cleanup-nil-skip.md](06-closer-cleanup-nil-skip.md)
