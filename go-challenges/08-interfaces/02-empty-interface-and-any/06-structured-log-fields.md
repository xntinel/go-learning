# Exercise 6: A Structured-Log Field Builder That Never Panics on an `any` Value

The observability boundary boxes arbitrary values into log attributes: `slog.Any`
takes an `any`, and a careless helper turns a dangling argument or a raw error
struct into a noisy or crashing log line. This module builds a field helper that
redacts sensitive keys, renders errors through `Error()`, mirrors `slog`'s own
`!BADKEY` handling for odd arguments, and uses `slog.LogValuer` so an expensive
field is only computed when the record is actually emitted.

This module is fully self-contained: its own `go mod init`, its own demo, its own
tests. Nothing here imports another exercise.

## What you'll build

```text
logfields/                 independent module: example.com/logfields
  go.mod                   go 1.26
  logfields.go             Fields(kv ...any) []slog.Attr; redaction; error rendering; Lazy LogValuer
  cmd/
    demo/
      main.go              runnable demo: JSON handler, redacted + error + lazy field
  logfields_test.go        redaction, error rendering, lazy eval counter, odd-arg !BADKEY
```

- Files: `logfields.go`, `cmd/demo/main.go`, `logfields_test.go`.
- Implement: `Fields(kv ...any) []slog.Attr` that pairs key-values into attrs, redacts a set of sensitive keys, renders `error` values via `Error()`, and appends a `!BADKEY` attr for a dangling arg instead of panicking; plus a `Lazy` `slog.LogValuer` for deferred computation.
- Test: a redacted key is masked, an error value is rendered via `Error()`, a `Lazy` field's function runs only when the record is emitted (a counter proves it), and an odd trailing arg is handled without panic.
- Verify: `go test -count=1 -race ./...`

Set up the module:

```bash
go mod edit -go=1.26
```

### Why the helper, and why LogValuer

`slog` already boxes values into `any` through `slog.Any`, and its own variadic API
tolerates a malformed call by emitting a `!BADKEY` attribute rather than panicking.
A production field helper must be at least as robust: it takes alternating
key-values, and for each pair it decides how to render the value. Three decisions
matter. First, redaction: a value under a sensitive key (`password`, `token`,
`secret`, `authorization`) is replaced with a mask before it ever reaches a handler,
so a stray `slog` call cannot leak a credential into a log aggregator. Second, error
rendering: an `error` value logged with `%v` or dumped as a struct is noisy and
sometimes lossy; rendering it through `Error()` gives the clean message. Third, the
dangling argument: if the caller passes an odd number of values, the last one has no
key, and the helper mirrors `slog` by attaching it under the literal key `!BADKEY`
instead of panicking or dropping it — a malformed log call must never crash the
request that made it.

`slog.LogValuer` is the tool for a field that is expensive to compute. A value that
implements `LogValue() slog.Value` is not resolved when you build the attribute; the
handler resolves it while formatting the record — and only if the record is actually
emitted. A log line below the handler's level is dropped before resolution, so the
expensive computation never runs. `Lazy` wraps a `func() slog.Value` so a caller can
defer, for example, serializing a large object or hashing a body until the log is
known to be kept.

Create `logfields.go`:

```go
package logfields

import (
	"log/slog"
	"strings"
)

// redacted keys are masked before their value reaches any handler.
var sensitive = map[string]struct{}{
	"password":      {},
	"token":         {},
	"secret":        {},
	"authorization": {},
}

const mask = "[REDACTED]"

// Fields turns alternating key-value arguments into []slog.Attr. A sensitive key is
// masked; an error value is rendered via Error(); a dangling final argument is
// attached under "!BADKEY" (mirroring slog) rather than causing a panic.
func Fields(kv ...any) []slog.Attr {
	var attrs []slog.Attr
	i := 0
	for i < len(kv) {
		// A dangling final argument, or a non-string key: emit !BADKEY like slog.
		key, ok := kv[i].(string)
		if !ok {
			attrs = append(attrs, slog.Any("!BADKEY", kv[i]))
			i++
			continue
		}
		if i == len(kv)-1 {
			attrs = append(attrs, slog.Any("!BADKEY", key))
			break
		}
		attrs = append(attrs, attr(key, kv[i+1]))
		i += 2
	}
	return attrs
}

func attr(key string, val any) slog.Attr {
	if _, ok := sensitive[strings.ToLower(key)]; ok {
		return slog.String(key, mask)
	}
	if err, ok := val.(error); ok {
		return slog.String(key, err.Error())
	}
	return slog.Any(key, val)
}

// Lazy defers an expensive field computation. Its LogValue is only called by a
// handler that actually emits the record, so a dropped log line costs nothing.
type Lazy struct {
	fn func() slog.Value
}

// NewLazy wraps fn as a LogValuer.
func NewLazy(fn func() slog.Value) Lazy {
	return Lazy{fn: fn}
}

// LogValue satisfies slog.LogValuer.
func (l Lazy) LogValue() slog.Value {
	return l.fn()
}
```

### The runnable demo

The demo builds a JSON logger and logs one line whose fields include a redacted
password, an error value, and a plain field, so you can see the mask and the clean
error message in the output.

Create `cmd/demo/main.go`:

```go
package main

import (
	"context"
	"errors"
	"log/slog"
	"os"

	"example.com/logfields"
)

func main() {
	logger := slog.New(slog.NewJSONHandler(os.Stdout, &slog.HandlerOptions{
		// Drop the time attribute so the output is stable.
		ReplaceAttr: func(groups []string, a slog.Attr) slog.Attr {
			if a.Key == slog.TimeKey {
				return slog.Attr{}
			}
			return a
		},
	}))

	fields := logfields.Fields(
		"user", "alice",
		"password", "hunter2",
		"err", errors.New("connection refused"),
	)
	logger.LogAttrs(context.Background(), slog.LevelInfo, "login failed", fields...)
}
```

Run it:

```bash
go run ./cmd/demo
```

Expected output:

```
{"level":"INFO","msg":"login failed","user":"alice","password":"[REDACTED]","err":"connection refused"}
```

### Tests

`TestRedaction` captures JSON output into a buffer and asserts the sensitive value is
masked. `TestErrorRendering` asserts an error is rendered as its `Error()` string,
not a struct dump. `TestLazyEvaluation` is the important one: it logs a `Lazy` field
at a level below the handler's minimum and proves, with a counter, that the
computation never ran; then logs above the level and proves it ran exactly once.
`TestOddArg` proves a dangling argument produces a `!BADKEY` attr without panicking.

Create `logfields_test.go`:

```go
package logfields

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"log/slog"
	"strings"
	"sync/atomic"
	"testing"
)

func newLogger(buf *bytes.Buffer, level slog.Level) *slog.Logger {
	return slog.New(slog.NewJSONHandler(buf, &slog.HandlerOptions{Level: level}))
}

func TestRedaction(t *testing.T) {
	t.Parallel()

	var buf bytes.Buffer
	logger := newLogger(&buf, slog.LevelInfo)
	logger.LogAttrs(context.Background(), slog.LevelInfo, "m",
		Fields("password", "hunter2", "token", "abc")...)

	out := buf.String()
	if strings.Contains(out, "hunter2") || strings.Contains(out, "abc") {
		t.Fatalf("sensitive value leaked: %s", out)
	}
	if !strings.Contains(out, mask) {
		t.Fatalf("expected mask %q in output: %s", mask, out)
	}
}

func TestErrorRendering(t *testing.T) {
	t.Parallel()

	var buf bytes.Buffer
	logger := newLogger(&buf, slog.LevelInfo)
	logger.LogAttrs(context.Background(), slog.LevelInfo, "m",
		Fields("err", errors.New("boom"))...)

	var rec map[string]any
	if err := json.Unmarshal(buf.Bytes(), &rec); err != nil {
		t.Fatalf("bad json: %v (%s)", err, buf.String())
	}
	if rec["err"] != "boom" {
		t.Fatalf("err field = %v, want %q", rec["err"], "boom")
	}
}

func TestLazyEvaluation(t *testing.T) {
	t.Parallel()

	var calls atomic.Int64
	lazy := NewLazy(func() slog.Value {
		calls.Add(1)
		return slog.StringValue("computed")
	})

	var buf bytes.Buffer
	// Handler emits only Warn and above.
	logger := newLogger(&buf, slog.LevelWarn)

	// Below the level: the record is dropped, so LogValue must not be called.
	logger.LogAttrs(context.Background(), slog.LevelInfo, "dropped", slog.Any("f", lazy))
	if got := calls.Load(); got != 0 {
		t.Fatalf("LogValue called %d times for a dropped record, want 0", got)
	}

	// At the level: the record is emitted, so LogValue runs exactly once.
	logger.LogAttrs(context.Background(), slog.LevelWarn, "kept", slog.Any("f", lazy))
	if got := calls.Load(); got != 1 {
		t.Fatalf("LogValue called %d times for an emitted record, want 1", got)
	}
}

func TestOddArg(t *testing.T) {
	t.Parallel()

	defer func() {
		if r := recover(); r != nil {
			t.Fatalf("Fields panicked on odd args: %v", r)
		}
	}()

	attrs := Fields("user", "alice", "dangling")
	var foundBad bool
	for _, a := range attrs {
		if a.Key == "!BADKEY" {
			foundBad = true
		}
	}
	if !foundBad {
		t.Fatal("expected a !BADKEY attr for the dangling argument")
	}
}
```

## Review

The helper is correct when it survives every malformed call: a dangling argument
becomes a `!BADKEY` attr (never a panic), a sensitive key is masked before it reaches
a handler, and an error renders as its message. `TestLazyEvaluation` proves the
`LogValuer` contract that separates a cheap helper from an expensive one — the
computation runs only when the record is emitted, so a debug-level field costs nothing
in production where the level is higher. The mistakes this module prevents are logging
a raw error struct with `%v` (noisy, sometimes lossy), leaking a credential because no
redaction sits at the boundary, and panicking the request on an odd argument count.
Run `go test -race` to confirm the redaction, error rendering, and lazy evaluation all
hold.

## Resources

- [`log/slog.Any`](https://pkg.go.dev/log/slog#Any) — boxing an arbitrary value into an attribute.
- [`log/slog.LogValuer`](https://pkg.go.dev/log/slog#LogValuer) — deferred, lazily-resolved field values.
- [`log/slog.Attr`](https://pkg.go.dev/log/slog#Attr) — the key/value shape a handler formats.
- [`(*slog.Logger).LogAttrs`](https://pkg.go.dev/log/slog#Logger.LogAttrs) — emit a record from a precomputed `[]slog.Attr`.

---

Back to [00-concepts.md](00-concepts.md) | Next: [07-safe-equality-any.md](07-safe-equality-any.md)
