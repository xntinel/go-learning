# Exercise 4: Implementing slog.LogValuer to log domain errors as structured groups

A domain error is not a string; it has a code, a kind, and a retryable flag that
an operator will want to query and filter on. This module gives a domain error
type control over its own log representation by implementing `slog.LogValuer`, so
logging it emits `err.code`, `err.kind`, `err.retryable` as a nested queryable
object instead of one opaque message — resolved lazily by the handler, off the
hot path when the level is disabled.

This module is fully self-contained: its own `go mod init`, demo, and tests.
Nothing here imports any other exercise.

## What you'll build

```text
logvaluer/                   independent module: example.com/logvaluer
  go.mod                     go 1.25
  domainerr.go               DomainError with code/kind/retryable; LogValue() slog.Value returning a group
  cmd/
    demo/
      main.go                runnable demo: log the domain error as a nested object
  domainerr_test.go          JSON is a nested object for the domain type; plain error stays scalar
```

- Files: `domainerr.go`, `cmd/demo/main.go`, `domainerr_test.go`.
- Implement: a `DomainError` (code, kind, retryable) implementing `error` and `LogValue() slog.Value` returning `slog.GroupValue` of `code`/`kind`/`retryable`.
- Test: log the domain error with a JSON handler into a buffer; unmarshal and assert `err` is a nested object with `code`/`kind`/`retryable`; assert a plain `fmt.Errorf` error logs as a flat string.
- Verify: `go test -count=1 -race ./...`

Set up the module:

```bash
mkdir -p ~/go-exercises/logvaluer/cmd/demo
cd ~/go-exercises/logvaluer
go mod init example.com/logvaluer
go mod edit -go=1.25
```

### How LogValuer changes what gets logged

When you write `logger.Error("op failed", "err", err)`, slog stores the error in
an attribute whose value is `slog.AnyValue(err)` — kind `KindAny`, holding the
concrete error. Before a handler formats a record it calls `Value.Resolve()` on
each attribute, and `Resolve` checks whether the held value implements
`slog.LogValuer`. If it does, it calls `LogValue()` and substitutes the result.
So a `DomainError` that returns `slog.GroupValue(...)` from `LogValue()` causes
the `err` attribute to render as a nested group — `"err":{"code":...,
"kind":...,"retryable":...}` — instead of the flat `err.Error()` string a plain
error would produce.

Two things make this the right tool rather than pre-building the group at the call
site. First, laziness: `Resolve` is only called when the record is actually being
formatted, which only happens when the level is enabled. If error logging is off,
`LogValue` is never invoked, so the cost of building the group stays off the hot
path. Pre-formatting `slog.Group("err", ...)` at every call site pays that cost
unconditionally. Second, ownership: the type decides its own structured shape once,
and every call site that logs it gets the rich form for free — no call site has to
remember which fields to pull out.

The type still implements `error` (it has an `Error() string`), so it composes
with `errors.Is`/`errors.As` and `%w` exactly as any error does; `LogValue` only
governs how slog *renders* it. A plain `fmt.Errorf("boom")` does not implement
`LogValuer`, so slog falls back to its string form — proving the enrichment is
opt-in per type, not global.

Create `domainerr.go`:

```go
package logvaluer

import "log/slog"

// DomainError is a rich domain error whose structured log shape it controls
// itself via LogValue, so operators can filter on code/kind/retryable.
type DomainError struct {
	Code      string // stable machine code, e.g. "E_DB_TIMEOUT"
	Kind      string // category, e.g. "dependency", "validation"
	Retryable bool   // whether the caller may safely retry
	msg       string // human message
}

// NewDomainError builds a DomainError.
func NewDomainError(code, kind, msg string, retryable bool) *DomainError {
	return &DomainError{Code: code, Kind: kind, Retryable: retryable, msg: msg}
}

// Error satisfies the error interface; DomainError is a normal error too.
func (e *DomainError) Error() string { return e.Code + ": " + e.msg }

// LogValue makes slog render the error as a nested group of queryable fields
// instead of a flat message. Resolved lazily by the handler, so a disabled
// level pays nothing.
func (e *DomainError) LogValue() slog.Value {
	return slog.GroupValue(
		slog.String("code", e.Code),
		slog.String("kind", e.Kind),
		slog.Bool("retryable", e.Retryable),
		slog.String("msg", e.msg),
	)
}
```

### The runnable demo

The demo logs a `DomainError` and a plain error side by side through a JSON
handler (time suppressed for determinism) so you can see the domain error expand
into an object while the plain error stays a string.

Create `cmd/demo/main.go`:

```go
package main

import (
	"errors"
	"log/slog"
	"os"

	"example.com/logvaluer"
)

func main() {
	opts := &slog.HandlerOptions{ReplaceAttr: func(_ []string, a slog.Attr) slog.Attr {
		if a.Key == slog.TimeKey {
			return slog.Attr{}
		}
		return a
	}}
	logger := slog.New(slog.NewJSONHandler(os.Stdout, opts))

	de := logvaluer.NewDomainError("E_DB_TIMEOUT", "dependency", "query timed out", true)
	logger.Error("operation failed", "err", de)

	logger.Error("operation failed", "err", errors.New("plain boom"))
}
```

Run it:

```bash
go run ./cmd/demo
```

Expected output:

```
{"level":"ERROR","msg":"operation failed","err":{"code":"E_DB_TIMEOUT","kind":"dependency","retryable":true,"msg":"query timed out"}}
{"level":"ERROR","msg":"operation failed","err":"plain boom"}
```

### Tests

The tests unmarshal the JSON and assert on the *type* of the `err` field, not
just its content, so they catch the difference between an object and a string.
`TestDomainErrorLogsAsObject` asserts `err` is a `map` with the three queryable
fields; `TestPlainErrorLogsAsString` asserts a `fmt.Errorf` error is a `string`,
proving `LogValuer` is opt-in.

Create `domainerr_test.go`:

```go
package logvaluer

import (
	"bytes"
	"encoding/json"
	"fmt"
	"testing"

	"log/slog"
)

func logOne(t *testing.T, arg any) map[string]any {
	t.Helper()
	var buf bytes.Buffer
	logger := slog.New(slog.NewJSONHandler(&buf, nil))
	logger.Error("operation failed", "err", arg)
	var m map[string]any
	if err := json.Unmarshal(buf.Bytes(), &m); err != nil {
		t.Fatalf("bad json %q: %v", buf.String(), err)
	}
	return m
}

func TestDomainErrorLogsAsObject(t *testing.T) {
	t.Parallel()
	de := NewDomainError("E_DB_TIMEOUT", "dependency", "query timed out", true)
	m := logOne(t, de)

	obj, ok := m["err"].(map[string]any)
	if !ok {
		t.Fatalf("err is %T, want a nested object", m["err"])
	}
	if obj["code"] != "E_DB_TIMEOUT" {
		t.Fatalf("err.code = %v, want E_DB_TIMEOUT", obj["code"])
	}
	if obj["kind"] != "dependency" {
		t.Fatalf("err.kind = %v, want dependency", obj["kind"])
	}
	if obj["retryable"] != true {
		t.Fatalf("err.retryable = %v, want true", obj["retryable"])
	}
}

func TestPlainErrorLogsAsString(t *testing.T) {
	t.Parallel()
	m := logOne(t, fmt.Errorf("plain boom"))
	if _, ok := m["err"].(string); !ok {
		t.Fatalf("plain err is %T, want string (LogValuer must be opt-in)", m["err"])
	}
}

func TestDomainErrorIsStillAnError(t *testing.T) {
	t.Parallel()
	var err error = NewDomainError("E_X", "validation", "bad", false)
	if got, want := err.Error(), "E_X: bad"; got != want {
		t.Fatalf("Error() = %q, want %q", got, want)
	}
}

func ExampleDomainError_LogValue() {
	de := NewDomainError("E_X", "validation", "bad", false)
	fmt.Println(de.LogValue().Kind() == slog.KindGroup)
	// Output: true
}
```

## Review

The type is correct when the domain error renders as a queryable object and a
plain error does not. `TestDomainErrorLogsAsObject` asserts the nested
`code`/`kind`/`retryable` fields; `TestPlainErrorLogsAsString` guards that
`LogValuer` is per-type opt-in, not a global reshaping of every error.
`TestDomainErrorIsStillAnError` pins that implementing `LogValuer` does not stop
the type being a normal `error` — it composes with `%w` and `errors.As` as usual.

The trade-off to understand: `LogValue` is resolved lazily by the handler, so an
error at a disabled level costs nothing, which is why this beats building
`slog.Group("err", ...)` by hand at every call site. The one trap is recursion —
if `LogValue` returned a value that itself resolved back to the same type you get
an infinite loop; keep `LogValue` returning concrete scalar attrs as here. Combine
this with the redaction handler in the next exercise and the correlation handler
in the previous one and a single log line carries a correlated, enriched,
PII-safe view of a failure.

## Resources

- [`slog.LogValuer`](https://pkg.go.dev/log/slog#LogValuer) — the `LogValue() Value` contract and lazy `Resolve` semantics.
- [`slog.Value` and `GroupValue`](https://pkg.go.dev/log/slog#GroupValue) — building a group value and `Value.Kind`.
- [Structured Logging with slog](https://go.dev/blog/slog) — the "LogValuer" section on letting a type own its representation.

---

Back to [00-concepts.md](00-concepts.md) | Next: [05-redact-sensitive-fields.md](05-redact-sensitive-fields.md)
