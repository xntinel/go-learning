# Exercise 10: Aggregate and Classify Concurrent Failures for Observability

A collect-all run that returns `errors.Join(errs...)` gives the caller a
correct-but-opaque wall of text. In production you need more: one structured log
record per failure, each classified by *type* — is this retryable or terminal? —
so dashboards can count retryables and alerts can fire on terminals. This module
combines `errors.Join` aggregation with `errors.As` classification and `slog`
structured logging, turning a batch of concurrent failures into something a human
can triage and a machine can route.

This module is fully self-contained: its own `go mod init`, all code inline, its
own demo and tests. Nothing here imports another exercise.

## What you'll build

```text
aggreport/                   independent module: example.com/aggreport
  go.mod                     go 1.26
  aggreport.go               RetryableError, TerminalError; RunAndReport (Join + As + slog)
  cmd/
    demo/
      main.go                runnable demo: mixed failures, JSON log per failure, joined error
  aggreport_test.go          tests: errors.As extracts type from join; one JSON record per failure
```

Files: `aggreport.go`, `cmd/demo/main.go`, `aggreport_test.go`.
Implement: typed `*RetryableError` and `*TerminalError`; `RunAndReport(ctx, jobs, logger)` that runs jobs collect-all, logs one structured record per failure (classified via `errors.As`), and returns the joined error.
Test: a mix of retryable and terminal typed failures — `errors.As` extracts each concrete type from the joined error; capture `slog` JSON over a `bytes.Buffer` and assert one record per failure with the expected job name and level.
Verify: `go test -count=1 -race ./...`

Set up the module:

```bash
mkdir -p go-solutions/10-error-handling/09-error-handling-in-goroutines/10-structured-error-aggregation-slog/cmd/demo
cd go-solutions/10-error-handling/09-error-handling-in-goroutines/10-structured-error-aggregation-slog
go mod edit -go=1.26
```

### Aggregate for the return, structure for the human

Two typed errors carry the classification. `*RetryableError` marks a transient
failure (a deadlock, a timeout) worth retrying; `*TerminalError` marks a permanent
one (a constraint violation, a malformed record) that retrying cannot fix. Each
wraps an underlying error and implements `Unwrap`, so `errors.Is` still reaches the
cause and `errors.As` can pull the concrete type back out. That last property is
the key: `errors.Join` builds a multi-error whose `Unwrap() []error` lets `errors.As`
traverse *every* joined part, so `errors.As(joined, &retryable)` finds a retryable
error anywhere in the aggregate. The aggregate stays classifiable.

`RunAndReport` runs the jobs collect-all — every job runs to completion, every
outcome recorded — then does the observability work *after* `wg.Wait`, in the
single calling goroutine. That sequencing is deliberate and matters for
correctness: `slog` handlers are safe for concurrent use, but the `bytes.Buffer`
the test wires one to is not, and more generally logging sequentially after the
concurrent phase keeps the record order tied to a stable iteration rather than to
scheduler races. For each failure it classifies the error with `errors.As` — the
log `level` and a `class` attribute reflect retryable vs terminal — and emits one
`slog` record with the job name, the class, and the error. Then it wraps each job's
error with its name and joins them, returning an aggregate that `errors.Is`/`errors.As`
can still walk. The result: a caller gets one classifiable error value, an operator
gets one structured line per failure, and neither has to parse a wall of text.

Create `aggreport.go`:

```go
package aggreport

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"sync"
)

// RetryableError marks a transient failure worth retrying.
type RetryableError struct{ Err error }

func (e *RetryableError) Error() string { return "retryable: " + e.Err.Error() }
func (e *RetryableError) Unwrap() error { return e.Err }

// TerminalError marks a permanent failure that retrying cannot fix.
type TerminalError struct{ Err error }

func (e *TerminalError) Error() string { return "terminal: " + e.Err.Error() }
func (e *TerminalError) Unwrap() error { return e.Err }

// Job is a named unit of work.
type Job struct {
	Name string
	Run  func(ctx context.Context) error
}

type outcome struct {
	name string
	err  error
}

// RunAndReport runs every job collect-all, emits one structured log record per
// failure (classified retryable vs terminal via errors.As), and returns the
// joined error. The joined error remains inspectable: errors.Is and errors.As
// traverse every part.
func RunAndReport(ctx context.Context, jobs []Job, logger *slog.Logger) error {
	var (
		mu       sync.Mutex
		outcomes = make([]outcome, 0, len(jobs))
		wg       sync.WaitGroup
	)
	for _, j := range jobs {
		wg.Go(func() {
			err := j.Run(ctx)
			mu.Lock()
			outcomes = append(outcomes, outcome{name: j.Name, err: err})
			mu.Unlock()
		})
	}
	wg.Wait()

	// Log and join sequentially, after the concurrent phase: one record per
	// failure, each classified by its concrete error type.
	var errs []error
	for _, o := range outcomes {
		if o.err == nil {
			continue
		}
		class, level := classify(o.err)
		logger.LogAttrs(ctx, level, "job failed",
			slog.String("job", o.name),
			slog.String("class", class),
			slog.String("error", o.err.Error()),
		)
		errs = append(errs, fmt.Errorf("job %q: %w", o.name, o.err))
	}
	return errors.Join(errs...)
}

// classify maps a job error to a class string and a log level via errors.As.
func classify(err error) (class string, level slog.Level) {
	var retryable *RetryableError
	var terminal *TerminalError
	switch {
	case errors.As(err, &retryable):
		return "retryable", slog.LevelWarn
	case errors.As(err, &terminal):
		return "terminal", slog.LevelError
	default:
		return "unknown", slog.LevelError
	}
}
```

### The runnable demo

The demo runs four jobs: two succeed, one fails retryably, one fails terminally. It
logs one JSON record per failure to stdout and prints whether the joined error is
still classifiable with `errors.As`. Because `RunAndReport` logs sequentially and
the outcome order is scheduler-dependent, the demo sorts nothing — instead it shows
the classification result, which is order-independent.

Create `cmd/demo/main.go`:

```go
package main

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"os"

	"example.com/aggreport"
)

func main() {
	// JSON handler at Debug so both Warn (retryable) and Error (terminal) print,
	// with time removed so the output is stable.
	handler := slog.NewJSONHandler(os.Stdout, &slog.HandlerOptions{
		Level: slog.LevelDebug,
		ReplaceAttr: func(groups []string, a slog.Attr) slog.Attr {
			if a.Key == slog.TimeKey {
				return slog.Attr{}
			}
			return a
		},
	})
	logger := slog.New(handler)

	jobs := []aggreport.Job{
		{Name: "ok-1", Run: func(ctx context.Context) error { return nil }},
		{Name: "deadlock", Run: func(ctx context.Context) error {
			return &aggreport.RetryableError{Err: errors.New("deadlock detected")}
		}},
		{Name: "ok-2", Run: func(ctx context.Context) error { return nil }},
		{Name: "bad-input", Run: func(ctx context.Context) error {
			return &aggreport.TerminalError{Err: errors.New("constraint violation")}
		}},
	}

	err := aggreport.RunAndReport(context.Background(), jobs, logger)

	var retryable *aggreport.RetryableError
	var terminal *aggreport.TerminalError
	fmt.Printf("joined error classifiable as retryable: %t\n", errors.As(err, &retryable))
	fmt.Printf("joined error classifiable as terminal: %t\n", errors.As(err, &terminal))
}
```

Run it:

```bash
go run ./cmd/demo
```

The two failure records may appear in either order (outcomes are collected
concurrently). One possible run:

```
{"level":"WARN","msg":"job failed","job":"deadlock","class":"retryable","error":"retryable: deadlock detected"}
{"level":"ERROR","msg":"job failed","job":"bad-input","class":"terminal","error":"terminal: constraint violation"}
joined error classifiable as retryable: true
joined error classifiable as terminal: true
```

### Tests

`TestClassifiesThroughJoin` runs one retryable and one terminal failure and asserts
`errors.As` extracts *both* concrete types from the single joined error — the proof
that `errors.Join` keeps every part classifiable. `TestOneRecordPerFailure` wires a
`slog.JSONHandler` to a `bytes.Buffer`, runs a mix of successes and failures, and
asserts exactly one JSON record per failure (successes produce none), each with the
right `job`, `class`, and `level`. Parsing the buffer line by line into a map keeps
the assertions independent of record order. All run under `-race`.

Create `aggreport_test.go`:

```go
package aggreport

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"log/slog"
	"strings"
	"testing"
)

func TestClassifiesThroughJoin(t *testing.T) {
	t.Parallel()
	logger := slog.New(slog.NewJSONHandler(bytes.NewBuffer(nil), nil))
	jobs := []Job{
		{Name: "transient", Run: func(ctx context.Context) error {
			return &RetryableError{Err: errors.New("timeout")}
		}},
		{Name: "permanent", Run: func(ctx context.Context) error {
			return &TerminalError{Err: errors.New("bad input")}
		}},
	}
	err := RunAndReport(context.Background(), jobs, logger)

	var retryable *RetryableError
	if !errors.As(err, &retryable) {
		t.Fatalf("joined error = %v, want a *RetryableError reachable via errors.As", err)
	}
	var terminal *TerminalError
	if !errors.As(err, &terminal) {
		t.Fatalf("joined error = %v, want a *TerminalError reachable via errors.As", err)
	}
}

func TestOneRecordPerFailure(t *testing.T) {
	t.Parallel()
	var buf bytes.Buffer
	logger := slog.New(slog.NewJSONHandler(&buf, &slog.HandlerOptions{Level: slog.LevelDebug}))
	jobs := []Job{
		{Name: "ok", Run: func(ctx context.Context) error { return nil }},
		{Name: "retry-me", Run: func(ctx context.Context) error {
			return &RetryableError{Err: errors.New("deadlock")}
		}},
		{Name: "give-up", Run: func(ctx context.Context) error {
			return &TerminalError{Err: errors.New("constraint")}
		}},
	}
	_ = RunAndReport(context.Background(), jobs, logger)

	byJob := make(map[string]map[string]any)
	for _, line := range strings.Split(strings.TrimSpace(buf.String()), "\n") {
		if line == "" {
			continue
		}
		var rec map[string]any
		if err := json.Unmarshal([]byte(line), &rec); err != nil {
			t.Fatalf("log line is not valid JSON: %q (%v)", line, err)
		}
		byJob[rec["job"].(string)] = rec
	}

	if len(byJob) != 2 {
		t.Fatalf("got %d failure records, want 2 (successes must not log)", len(byJob))
	}
	if got := byJob["retry-me"]; got["class"] != "retryable" || got["level"] != "WARN" {
		t.Fatalf("retry-me record = %v, want class=retryable level=WARN", got)
	}
	if got := byJob["give-up"]; got["class"] != "terminal" || got["level"] != "ERROR" {
		t.Fatalf("give-up record = %v, want class=terminal level=ERROR", got)
	}
	if _, ok := byJob["ok"]; ok {
		t.Fatal("the successful job must not produce a log record")
	}
}
```

## Review

The reporter is correct when the aggregate stays classifiable and the logs stay
structured. Classifiability: `errors.As` extracts *both* the retryable and terminal
concrete types from the single joined error, which works only because each typed
error implements `Unwrap` and `errors.Join` exposes `Unwrap() []error` for `As` to
traverse. Structure: exactly one JSON record per failure, each with the job name,
class, and a level that reflects the classification — successes log nothing. The
logging happens after `wg.Wait` in the calling goroutine, which keeps the
non-concurrent-safe `bytes.Buffer` race-free and the record set deterministic in
count. The mistake this closes is `return errors.Join(errs...)` of bare,
unwrapped, unlogged errors — technically correct, operationally useless. Wrap each
part with its job name, classify with `errors.As`, and emit a structured record.
Run `go test -race` and `go vet ./...` to confirm.

## Resources

- [`errors.Join`](https://pkg.go.dev/errors#Join) — the multi-error whose `Is`/`As` traverse every part.
- [`errors.As`](https://pkg.go.dev/errors#As) — extracting a concrete error type from the aggregate for classification.
- [`log/slog`](https://pkg.go.dev/log/slog) — `LogAttrs`, `JSONHandler`, and structured records.

---

Back to [00-concepts.md](00-concepts.md) | Next: [../10-error-handling-middleware/00-concepts.md](../10-error-handling-middleware/00-concepts.md)
