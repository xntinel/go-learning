# 9. Replacing Global Logger Patterns

Global loggers are convenient at startup and painful in tests. This lesson builds a small worker that accepts an injected `*slog.Logger`, while also demonstrating how an application can bridge legacy `log.Print` calls through `slog.SetDefault`.

## Concepts

### SetDefault Is an Application Boundary Tool

`slog.SetDefault` changes the logger used by package-level `slog.Info`, `slog.Warn`, and related functions. The documentation also states that it updates the default logger used by the standard `log` package, enabling incremental migration from `log.Print` to structured logging.

### Injection Is Better for Libraries

Library code should accept `*slog.Logger` as a dependency. That keeps tests isolated, avoids process-wide state, and allows callers to enrich or route logs per subsystem.

### Testing Global State

If a test must touch the default logger, restore it with `t.Cleanup`. Prefer package code that accepts loggers so most tests only need a local buffer and a deterministic handler.

## Exercises

Set up the module:

```bash
mkdir -p ~/go-exercises/slog-global-migration
cd ~/go-exercises/slog-global-migration
go mod init example.com/slogglobal
```

Edit `go.mod`:

```go
module example.com/slogglobal

go 1.26
```

### Exercise 1: Build an Injected Worker

Create `worker.go`:

```go
package worker

import (
	"context"
	"errors"
	"fmt"
	"log"
	"log/slog"
)

var ErrNilLogger = errors.New("logger must not be nil")

type Worker struct {
	logger *slog.Logger
}

func New(logger *slog.Logger) (*Worker, error) {
	if logger == nil {
		return nil, fmt.Errorf("worker: %w", ErrNilLogger)
	}
	return &Worker{logger: logger}, nil
}

func (w *Worker) Logger() *slog.Logger {
	return w.logger
}

func (w *Worker) Process(ctx context.Context, jobID string) {
	w.logger.LogAttrs(ctx, slog.LevelInfo, "job processed", slog.String("job_id", jobID))
}

func ConfigureDefault(logger *slog.Logger) error {
	if logger == nil {
		return fmt.Errorf("configure default: %w", ErrNilLogger)
	}
	slog.SetDefault(logger)
	return nil
}

func LegacyPrint(message string) {
	log.Print(message)
}
```

### Exercise 2: Test Injected and Global Paths

Create `worker_test.go`:

```go
package worker

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"log/slog"
	"strings"
	"testing"
)

func testLogger(buf *bytes.Buffer) *slog.Logger {
	return slog.New(slog.NewTextHandler(buf, &slog.HandlerOptions{
		ReplaceAttr: func(groups []string, a slog.Attr) slog.Attr {
			if len(groups) == 0 && a.Key == slog.TimeKey {
				return slog.Attr{}
			}
			return a
		},
	}))
}

func TestWorkerProcessUsesInjectedLogger(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name  string
		jobID string
	}{
		{name: "first job", jobID: "job-1"},
		{name: "second job", jobID: "job-2"},
	}

	for _, tc := range tests {
		tc := tc
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()

			var buf bytes.Buffer
			w, err := New(testLogger(&buf))
			if err != nil {
				t.Fatalf("New() error = %v", err)
			}
			w.Process(context.Background(), tc.jobID)

			got := buf.String()
			if !strings.Contains(got, "job_id="+tc.jobID) || !strings.Contains(got, "msg=\"job processed\"") {
				t.Fatalf("output = %q", got)
			}
		})
	}
}

func TestConfigureDefaultBridgesLegacyLog(t *testing.T) {
	var buf bytes.Buffer
	previous := slog.Default()
	t.Cleanup(func() { slog.SetDefault(previous) })

	if err := ConfigureDefault(testLogger(&buf)); err != nil {
		t.Fatalf("ConfigureDefault() error = %v", err)
	}
	LegacyPrint("legacy path")

	got := buf.String()
	if !strings.Contains(got, "level=INFO") || !strings.Contains(got, "msg=\"legacy path\"") {
		t.Fatalf("legacy log was not bridged through slog: %q", got)
	}
}

func TestValidationErrors(t *testing.T) {
	t.Parallel()

	_, err := New(nil)
	if !errors.Is(err, ErrNilLogger) {
		t.Fatalf("New(nil) error = %v, want ErrNilLogger", err)
	}
	if err := ConfigureDefault(nil); !errors.Is(err, ErrNilLogger) {
		t.Fatalf("ConfigureDefault(nil) error = %v, want ErrNilLogger", err)
	}
}

func ExampleWorker_Process() {
	var buf bytes.Buffer
	w, _ := New(testLogger(&buf))
	w.Process(context.Background(), "job-1")
	fmt.Print(buf.String())
	// Output: level=INFO msg="job processed" job_id=job-1
}
```

Your turn: add a test that configures the default logger, calls package-level `slog.Info`, and restores the previous default with `t.Cleanup`.

### Exercise 3: Add a Demo Command

Create `cmd/demo/main.go`:

```go
package main

import (
	"bytes"
	"context"
	"fmt"
	"log/slog"

	"example.com/slogglobal"
)

func main() {
	var buf bytes.Buffer
	logger := slog.New(slog.NewTextHandler(&buf, &slog.HandlerOptions{
		ReplaceAttr: func(groups []string, a slog.Attr) slog.Attr {
			if len(groups) == 0 && a.Key == slog.TimeKey {
				return slog.Attr{}
			}
			return a
		},
	}))
	w, err := worker.New(logger)
	if err != nil {
		panic(err)
	}
	w.Process(context.Background(), "job-1")
	fmt.Print(buf.String())
}
```

## Common Mistakes

Wrong: use `slog.SetDefault` inside a library constructor.

What happens: importing one package changes process-wide logging behavior.

Fix: only application startup should set the default logger.

Wrong: run tests that mutate the default logger in parallel.

What happens: tests race logically even if the logger implementation is safe.

Fix: do not call `t.Parallel` in tests that change global logging state, and restore state with `t.Cleanup`.

Wrong: keep legacy `log.Print` forever after bridging it.

What happens: logs remain message-only and lose structured fields.

Fix: use the bridge for migration, then move call sites to injected `*slog.Logger` values.

## Verification

From `~/go-exercises/slog-global-migration`:

```bash
test -z "$(gofmt -l .)"
go vet ./...
go build ./...
go test -count=1 -race ./...
go run ./cmd/demo
```

Add one test of your own for package-level `slog.Info` after `ConfigureDefault`.

## Summary

- `slog.SetDefault` is useful during application startup and migration.
- The standard `log` package can be bridged through the default slog handler.
- Libraries should accept `*slog.Logger` instead of mutating globals.
- Tests that touch default logging must restore global state and avoid parallel execution.

## What's Next

Next, continue to the next Go chapter after verifying every lesson in this chapter passes the gate.

## Resources

- `slog.SetDefault` documentation: https://pkg.go.dev/log/slog#SetDefault
- `slog.NewLogLogger` documentation: https://pkg.go.dev/log/slog#NewLogLogger
- Go blog, "Structured Logging with slog": https://go.dev/blog/slog
