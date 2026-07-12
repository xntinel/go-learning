# Exercise 8: Extract Structured Fields From Errors for Logging and Metrics

At the observability boundary, an error should become a structured log record and
a bounded-cardinality metric label — never a raw string dumped into both. This
exercise builds a logging middleware that uses `errors.As` to pull an
`*AppError{Code, Op, Fields}` out of whatever the request returned, emits a
structured `slog` record with those fields, and labels a metric by the bounded
`Code` — falling back to a fixed `internal` label when `As` fails, so error
strings never explode metric cardinality.

This module is fully self-contained: its own `go mod init`, demo, and tests.

## What you'll build

```text
obsfields/                      independent module: example.com/obsfields
  go.mod                        go 1.25
  obslog.go                     AppError, LogError(logger, err), MetricLabel(err)
  obslog_test.go                JSON handler to a buffer; code+fields present; internal fallback
  cmd/demo/main.go              runnable demo emitting a structured record
```

Files: `obslog.go`, `obslog_test.go`, `cmd/demo/main.go`.
Implement: `LogError(logger, err)` that `errors.As` into `*AppError` and emits `slog` attributes (code, op, and the Fields), or `code=internal` on failure; `MetricLabel(err)` returning the bounded `Code` or `internal`.
Test: capture `slog` output via a `slog.NewJSONHandler` writing to a `bytes.Buffer`; assert `code` and the fields on success and `code=internal` for a plain error; assert the raw message never becomes the metric label.
Verify: `go test -count=1 -race ./... && go vet ./...`

Set up the module:

```bash
mkdir -p go-solutions/10-error-handling/03-errors-is-and-errors-as/08-observability-extract-fields/cmd/demo
cd go-solutions/10-error-handling/03-errors-is-and-errors-as/08-observability-extract-fields
go mod edit -go=1.25
```

### As at the observability boundary

Observability wants two things from an error, and both are payload, not identity —
which is why this is an `errors.As` job, not `errors.Is`. First, a *structured
record*: a stable `Code`, the operation `Op` that failed, and a bag of contextual
`Fields` (a user id, a request id, a row count) that make the log line useful.
Second, a *metric label*: a single low-cardinality string to increment a counter
by. Both come from pulling an `*AppError` out of the error tree with `errors.As`
and reading its fields.

The cardinality point is the one that bites teams in production. A metric label
must have bounded cardinality — a handful of distinct values — because every
distinct label value is a separate time series in the metrics backend. If you
label an error counter with `err.Error()`, you get a new time series for every
unique message, including ones that embed ids, addresses, or timestamps; that is a
cardinality explosion that can take down the metrics system. The discipline is to
label on the bounded `Code` (`not_found`, `timeout`, `db_unavailable` — a closed
set the code defines) and nothing else. `MetricLabel` therefore extracts the
`AppError.Code` when `As` succeeds and returns the fixed string `internal` when it
does not, so an unclassified error contributes to exactly one bucket instead of
its own.

`LogError` does the structured-record half. On a successful `errors.As`, it emits
an `slog` record at error level with `code` and `op` attributes plus one attribute
per entry in `Fields`. On failure — a plain `errors.New` value with no `*AppError`
in the tree — it emits a minimal record with `code=internal` and the raw message
in a `msg`-style attribute (safe in a log, where high cardinality is fine),
*without* ever promoting that message to a metric label. Capturing `slog` output
is easy to test: point a `slog.NewJSONHandler` at a `bytes.Buffer` and assert on
the emitted JSON.

Create `obslog.go`:

```go
package obsfields

import (
	"errors"
	"log/slog"
)

// AppError carries observability payload: a bounded Code (safe as a metric
// label), the failing Op, and arbitrary contextual Fields (safe only in logs).
type AppError struct {
	Code   string
	Op     string
	Fields map[string]any
	Err    error
}

func (e *AppError) Error() string {
	if e.Err != nil {
		return e.Op + " [" + e.Code + "]: " + e.Err.Error()
	}
	return e.Op + " [" + e.Code + "]"
}

func (e *AppError) Unwrap() error { return e.Err }

// MetricLabel returns the bounded Code for use as a metric label, or "internal"
// when the error is not an *AppError. It NEVER returns the raw error string, to
// keep metric cardinality bounded.
func MetricLabel(err error) string {
	var ae *AppError
	if errors.As(err, &ae) {
		return ae.Code
	}
	return "internal"
}

// LogError emits a structured record for err. On a recovered *AppError it logs
// the code, op, and each contextual field; otherwise it logs code=internal with
// the raw message (safe in a log, never as a metric label).
func LogError(logger *slog.Logger, err error) {
	var ae *AppError
	if !errors.As(err, &ae) {
		logger.Error("request failed", slog.String("code", "internal"), slog.String("error", err.Error()))
		return
	}
	attrs := []any{slog.String("code", ae.Code), slog.String("op", ae.Op)}
	for k, v := range ae.Fields {
		attrs = append(attrs, slog.Any(k, v))
	}
	logger.Error("request failed", attrs...)
}
```

### The runnable demo

The demo logs one `*AppError` (wrapped, to prove `As` walks the chain) and one
plain error to a text handler on stdout, and prints the metric label each yields.

Create `cmd/demo/main.go`:

```go
package main

import (
	"errors"
	"fmt"
	"log/slog"
	"os"

	"example.com/obsfields"
)

func main() {
	logger := slog.New(slog.NewTextHandler(os.Stdout, &slog.HandlerOptions{
		ReplaceAttr: func(groups []string, a slog.Attr) slog.Attr {
			if a.Key == slog.TimeKey || a.Key == slog.LevelKey {
				return slog.Attr{} // drop volatile keys so demo output is stable
			}
			return a
		},
	}))

	appErr := fmt.Errorf("handler: %w", &obsfields.AppError{
		Code:   "db_unavailable",
		Op:     "GetUser",
		Fields: map[string]any{"user_id": "u1"},
	})
	obsfields.LogError(logger, appErr)
	fmt.Printf("metric label: %s\n", obsfields.MetricLabel(appErr))

	plain := errors.New("boom at 10.0.0.4:5432")
	fmt.Printf("plain metric label: %s\n", obsfields.MetricLabel(plain))
}
```

Run it:

```bash
go run ./cmd/demo
```

Expected output:

```
msg="request failed" code=db_unavailable op=GetUser user_id=u1
metric label: db_unavailable
plain metric label: internal
```

### Tests

The tests capture `slog` JSON in a buffer. `TestLogsAppErrorFields` asserts the
emitted record carries `code`, `op`, and the contextual field.
`TestLogsInternalFallback` asserts a plain error logs `code=internal`.
`TestMetricLabelBounded` asserts the label is the bounded code on success and
`internal` otherwise, and — the cardinality guard — that the raw message never
becomes the label.

Create `obslog_test.go`:

```go
package obsfields

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"strings"
	"testing"
)

func decode(t *testing.T, buf *bytes.Buffer) map[string]any {
	t.Helper()
	var m map[string]any
	if err := json.Unmarshal(buf.Bytes(), &m); err != nil {
		t.Fatalf("log line is not JSON: %q (%v)", buf.String(), err)
	}
	return m
}

func TestLogsAppErrorFields(t *testing.T) {
	t.Parallel()
	var buf bytes.Buffer
	logger := slog.New(slog.NewJSONHandler(&buf, nil))

	err := fmt.Errorf("handler: %w", &AppError{
		Code:   "db_unavailable",
		Op:     "GetUser",
		Fields: map[string]any{"user_id": "u1"},
	})
	LogError(logger, err)

	m := decode(t, &buf)
	if m["code"] != "db_unavailable" {
		t.Fatalf("code = %v, want db_unavailable", m["code"])
	}
	if m["op"] != "GetUser" {
		t.Fatalf("op = %v, want GetUser", m["op"])
	}
	if m["user_id"] != "u1" {
		t.Fatalf("user_id = %v, want u1", m["user_id"])
	}
}

func TestLogsInternalFallback(t *testing.T) {
	t.Parallel()
	var buf bytes.Buffer
	logger := slog.New(slog.NewJSONHandler(&buf, nil))

	LogError(logger, errors.New("some raw failure"))

	m := decode(t, &buf)
	if m["code"] != "internal" {
		t.Fatalf("code = %v, want internal", m["code"])
	}
}

func TestMetricLabelBounded(t *testing.T) {
	t.Parallel()
	appErr := fmt.Errorf("x: %w", &AppError{Code: "timeout", Op: "Call"})
	if got := MetricLabel(appErr); got != "timeout" {
		t.Fatalf("MetricLabel = %q, want timeout", got)
	}

	raw := errors.New("connection to 10.0.0.4:5432 failed at seq 99123")
	label := MetricLabel(raw)
	if label != "internal" {
		t.Fatalf("MetricLabel = %q, want internal", label)
	}
	if strings.Contains(label, "10.0.0.4") || strings.Contains(label, "99123") {
		t.Fatalf("raw error detail leaked into metric label: %q", label)
	}
}

func ExampleMetricLabel() {
	err := &AppError{Code: "not_found", Op: "GetUser"}
	fmt.Println(MetricLabel(err), MetricLabel(errors.New("boom")))
	// Output: not_found internal
}
```

## Review

The middleware is correct when payload extraction is total: a recovered
`*AppError` produces a structured record with `code`, `op`, and its contextual
`Fields`, and every other error produces `code=internal`. The `errors.As`
extraction is the whole mechanism — it walks the chain so a wrapped `*AppError` is
still found. The load-bearing rule is the cardinality guard: `MetricLabel` returns
only the bounded `Code` or the fixed `internal`, never `err.Error()`, so a message
carrying an address or a sequence number cannot spawn a new time series. It is fine
to put the raw message in a *log* attribute (logs tolerate high cardinality); it is
never fine to put it in a *metric* label. Run `go test -race`.

## Resources

- [log/slog](https://pkg.go.dev/log/slog) — `Logger.Error`, `slog.String`, `slog.Any`, `NewJSONHandler`.
- [errors.As](https://pkg.go.dev/errors#As) — recovering the typed `*AppError` payload.
- [slog.HandlerOptions](https://pkg.go.dev/log/slog#HandlerOptions) — `ReplaceAttr` for stable test/demo output.

---

Back to [00-concepts.md](00-concepts.md) | Previous: [07-context-error-classification.md](07-context-error-classification.md) | Next: [09-queue-consumer-ack-nack-dlq.md](09-queue-consumer-ack-nack-dlq.md)
