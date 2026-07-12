# Exercise 8: A Custom Error That Logs Structured, Redacted Fields via slog

An error type is also an observability contract. This module builds a
`ServiceError` implementing `slog.LogValuer` so it renders as a group of typed
attributes (`op`, `entity`, `kind`, `request_id`, `attempt`) when logged — while
redacting a sensitive token from both `Error()` and the log value. The type
decides its own structured, safe representation.

This module is fully self-contained: its own module, code, demo, and tests.
Nothing here imports any other exercise.

## What you'll build

```text
errslog/                   independent module: example.com/errslog
  go.mod                   go 1.24
  errslog.go               ServiceError implementing error + slog.LogValuer
  cmd/
    demo/
      main.go              logs the error as JSON to stdout
  errslog_test.go          parse JSON: grouped typed attrs present, token redacted
```

Files: `errslog.go`, `cmd/demo/main.go`, `errslog_test.go`.
Implement: a `*ServiceError` whose `Error()` omits the token and whose `LogValue()` returns a `slog.GroupValue` of typed attributes, also omitting the token.
Test: log through a `slog.JSONHandler` into a `bytes.Buffer`, parse the JSON, assert the grouped attributes are present and typed, assert the token is absent from both the JSON and `Error()`, and assert `LogValue()` does not panic on a zero-value error.
Verify: `go test -count=1 -race ./...`

Set up the module:

```bash
go mod edit -go=1.24
```

### The error decides its own log representation

Structured logging wants typed fields it can index — `kind="not_found"`,
`op="ChargeCard"`, `request_id="req-abc"` — not a single opaque message string. The
`slog.LogValuer` interface lets a value supply exactly that: `LogValue() slog.Value`
returns a `slog.Value`, and when you log the error as an attribute, slog *resolves*
it by calling `LogValue()` and using the result instead of the raw value. Returning
`slog.GroupValue(...)` makes the error render as a nested object of typed
attributes under its key.

This inverts the usual arrangement. Instead of the logging call site knowing how to
pick apart the error, the error tells the logger how it wants to appear. Every log
site that records this error gets the same well-structured shape for free, and the
shape lives in one place — with the type.

### Redaction is the error's responsibility

`ServiceError` carries an unexported `token` (a credential involved in the failed
operation). It must never reach a log sink or a client. Both surfaces the outside
world can see — `Error()` and `LogValue()` — deliberately omit it. `Error()`
formats `op`/`entity`/`kind`/`request_id` and stops; `LogValue()` emits the same
fields as typed attributes and stops. The secret exists inside the struct for the
code that must use it, but the type's *representations* are redacted by
construction.

This is the safe default: a secret that is never placed into `Error()` or
`LogValue()` cannot leak through them, no matter how the error is logged or
serialized downstream. The test proves it by scanning the raw JSON bytes and the
`Error()` string for the token and asserting it appears in neither.

### LogValue must be panic-safe on a zero value

slog may resolve a value at an unexpected time, and defensive code sometimes logs a
partially built error. `LogValue()` must therefore never panic on a zero-value
`ServiceError` — it reads only plain string and int fields, all of which are valid
at their zero values, so it returns a group of empty strings and a zero attempt
rather than dereferencing anything. The test calls it on a zero value to lock this
in.

Create `errslog.go`:

```go
// Package errslog shows a ServiceError that implements slog.LogValuer to render
// as typed, grouped, redacted log attributes.
package errslog

import (
	"fmt"
	"log/slog"
)

// ServiceError is a domain error that also acts as an observability contract. The
// exported fields are safe to surface; token is a secret that must never reach a
// log or a client.
type ServiceError struct {
	Op        string
	Entity    string
	Kind      string
	RequestID string
	Attempt   int
	token     string // sensitive: redacted from Error() and LogValue()
}

// NewServiceError builds a ServiceError. The token is stored but never rendered.
func NewServiceError(op, entity, kind, requestID string, attempt int, token string) *ServiceError {
	return &ServiceError{
		Op:        op,
		Entity:    entity,
		Kind:      kind,
		RequestID: requestID,
		Attempt:   attempt,
		token:     token,
	}
}

// Error formats the safe fields only. The token is deliberately omitted.
func (e *ServiceError) Error() string {
	return fmt.Sprintf("%s %s: %s (request_id=%s attempt=%d)",
		e.Op, e.Entity, e.Kind, e.RequestID, e.Attempt)
}

// LogValue returns a group of typed attributes for structured logging. It omits
// the token and never panics on a zero value (it reads only plain fields).
func (e *ServiceError) LogValue() slog.Value {
	return slog.GroupValue(
		slog.String("op", e.Op),
		slog.String("entity", e.Entity),
		slog.String("kind", e.Kind),
		slog.String("request_id", e.RequestID),
		slog.Int("attempt", e.Attempt),
	)
}
```

### The runnable demo

The demo logs the error as JSON to stdout through a handler configured to drop the
time key (so the output is deterministic), showing the grouped attributes and the
absence of the token.

Create `cmd/demo/main.go`:

```go
package main

import (
	"log/slog"
	"os"

	"example.com/errslog"
)

func main() {
	// Drop the time key so the demo output is stable.
	h := slog.NewJSONHandler(os.Stdout, &slog.HandlerOptions{
		ReplaceAttr: func(groups []string, a slog.Attr) slog.Attr {
			if a.Key == slog.TimeKey {
				return slog.Attr{}
			}
			return a
		},
	})
	logger := slog.New(h)

	err := errslog.NewServiceError("ChargeCard", "payment", "declined", "req-abc", 2, "secret-token-xyz")
	logger.Error("operation failed", slog.Any("error", err))
}
```

Run it:

```bash
go run ./cmd/demo
```

Expected output:

```
{"level":"ERROR","msg":"operation failed","error":{"op":"ChargeCard","entity":"payment","kind":"declined","request_id":"req-abc","attempt":2}}
```

### Tests

`TestLogValueStructured` logs the error into a `bytes.Buffer`, parses the JSON, and
asserts the `error` group holds the typed attributes (`op` as a string, `attempt`
as a number). `TestTokenRedacted` scans both the JSON bytes and the `Error()`
string for the secret and asserts it appears in neither. `TestZeroValueNoPanic`
calls `LogValue()` on a zero-value error.

Create `errslog_test.go`:

```go
package errslog

import (
	"bytes"
	"encoding/json"
	"log/slog"
	"strings"
	"testing"
)

// logToJSON logs err under key "error" and returns the parsed top-level object.
func logToJSON(t *testing.T, err error) map[string]any {
	t.Helper()
	var buf bytes.Buffer
	logger := slog.New(slog.NewJSONHandler(&buf, nil))
	logger.Error("operation failed", slog.Any("error", err))

	var got map[string]any
	if uerr := json.Unmarshal(buf.Bytes(), &got); uerr != nil {
		t.Fatalf("unmarshal log line %q: %v", buf.String(), uerr)
	}
	return got
}

func TestLogValueStructured(t *testing.T) {
	t.Parallel()
	err := NewServiceError("ChargeCard", "payment", "declined", "req-abc", 2, "secret-token-xyz")
	got := logToJSON(t, err)

	group, ok := got["error"].(map[string]any)
	if !ok {
		t.Fatalf("error attr is not a group: %v", got["error"])
	}
	if op, ok := group["op"].(string); !ok || op != "ChargeCard" {
		t.Errorf("op = %v; want string ChargeCard", group["op"])
	}
	if kind, _ := group["kind"].(string); kind != "declined" {
		t.Errorf("kind = %v; want declined", group["kind"])
	}
	if rid, _ := group["request_id"].(string); rid != "req-abc" {
		t.Errorf("request_id = %v; want req-abc", group["request_id"])
	}
	// JSON numbers decode to float64; assert the attempt is present and typed.
	if attempt, ok := group["attempt"].(float64); !ok || attempt != 2 {
		t.Errorf("attempt = %v; want number 2", group["attempt"])
	}
}

func TestTokenRedacted(t *testing.T) {
	t.Parallel()
	const secret = "secret-token-xyz"
	err := NewServiceError("ChargeCard", "payment", "declined", "req-abc", 2, secret)

	var buf bytes.Buffer
	logger := slog.New(slog.NewJSONHandler(&buf, nil))
	logger.Error("operation failed", slog.Any("error", err))

	if strings.Contains(buf.String(), secret) {
		t.Errorf("log output leaked the token: %s", buf.String())
	}
	if strings.Contains(err.Error(), secret) {
		t.Errorf("Error() leaked the token: %s", err.Error())
	}
}

func TestZeroValueNoPanic(t *testing.T) {
	t.Parallel()
	defer func() {
		if r := recover(); r != nil {
			t.Fatalf("LogValue panicked on zero value: %v", r)
		}
	}()
	_ = (&ServiceError{}).LogValue()
}
```

## Review

The type owns its own log shape: `LogValue()` returns a group of typed attributes,
so `TestLogValueStructured` reads `op` as a string and `attempt` as a number from
the parsed JSON without any per-call-site formatting. Redaction is by construction —
the token is in neither surface the world sees, and `TestTokenRedacted` scans the
raw bytes and the message to prove it, which is the honest way to test a
non-leak (assert the secret string is truly absent). `LogValue()` reads only
zero-valid fields, so it is panic-safe on a partially built error. This is the
pattern that makes logs queryable and safe at the same time: one type, one place,
deciding both what to say and what to hide. Run `go test -race` to confirm.

## Resources

- [log/slog: LogValuer](https://pkg.go.dev/log/slog#LogValuer) — the interface and how slog resolves it.
- [log/slog: GroupValue](https://pkg.go.dev/log/slog#GroupValue) — building a group of attributes as a `slog.Value`.
- [Go Blog: Structured Logging with slog](https://go.dev/blog/slog) — attributes, groups, and handlers.

---

Back to [00-concepts.md](00-concepts.md) | Previous: [07-ratelimit-quota-error.md](07-ratelimit-quota-error.md) | Next: [09-astype-generic-matching.md](09-astype-generic-matching.md)
