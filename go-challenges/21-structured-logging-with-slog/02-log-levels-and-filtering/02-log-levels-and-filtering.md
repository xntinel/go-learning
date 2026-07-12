# 2. Log Levels and Filtering

Log levels are a contract about importance. This lesson builds a logger-backed audit package that filters records with `slog.HandlerOptions.Level` and changes that filter at runtime with `slog.LevelVar`.

## Concepts

### Level Values and Minimum Filtering

`slog.Level` is an integer severity. The standard names are `LevelDebug`, `LevelInfo`, `LevelWarn`, and `LevelError`; higher levels are more severe. Built-in handlers use `HandlerOptions.Level` as the minimum enabled level. The default minimum is Info, so Debug records are usually discarded unless the handler is configured otherwise.

### Runtime Level Changes

Passing a fixed `slog.Level` to `HandlerOptions.Level` freezes the threshold when the handler is built. Passing a `*slog.LevelVar` lets the program change the threshold later. The official documentation states that `LevelVar` is safe for concurrent reads and writes, which makes it appropriate for runtime debug toggles.

### Disabled Records Still Evaluate Arguments

Filtering happens after call arguments are evaluated. Avoid expensive work in disabled log calls unless you first check `logger.Enabled(ctx, level)` or defer work with a `slog.LogValuer`.

## Exercises

Edit `go.mod`:

```go
module example.com/sloglevels

go 1.26
```

### Exercise 1: Build a Runtime Level Controller

Create `audit.go`:

```go
package audit

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"log/slog"
)

var ErrNilBuffer = errors.New("buffer must not be nil")

type AuditLog struct {
	level  *slog.LevelVar
	logger *slog.Logger
	buf    *bytes.Buffer
}

func New(buf *bytes.Buffer, initial slog.Level) (*AuditLog, error) {
	if buf == nil {
		return nil, fmt.Errorf("audit log: %w", ErrNilBuffer)
	}
	level := new(slog.LevelVar)
	level.Set(initial)
	logger := slog.New(slog.NewTextHandler(buf, &slog.HandlerOptions{
		Level: level,
		ReplaceAttr: func(groups []string, a slog.Attr) slog.Attr {
			if len(groups) == 0 && a.Key == slog.TimeKey {
				return slog.Attr{}
			}
			return a
		},
	}))
	return &AuditLog{level: level, logger: logger, buf: buf}, nil
}

func (a *AuditLog) Logger() *slog.Logger {
	return a.logger
}

func (a *AuditLog) Level() slog.Level {
	return a.level.Level()
}

func (a *AuditLog) SetLevel(level slog.Level) {
	a.level.Set(level)
}

func (a *AuditLog) Output() string {
	return a.buf.String()
}

func (a *AuditLog) RecordRequest(ctx context.Context, user string, status int) {
	a.logger.LogAttrs(ctx, slog.LevelDebug, "request details", slog.String("user", user))
	a.logger.LogAttrs(ctx, slog.LevelInfo, "request completed", slog.Int("status", status))
}
```

### Exercise 2: Verify Filtering and Errors

Create `audit_test.go`:

```go
package audit

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"log/slog"
	"strings"
	"testing"
)

func TestRecordRequestFiltersByLevel(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name      string
		level     slog.Level
		wantDebug bool
		wantInfo  bool
	}{
		{name: "info minimum", level: slog.LevelInfo, wantInfo: true},
		{name: "debug minimum", level: slog.LevelDebug, wantDebug: true, wantInfo: true},
		{name: "warn minimum", level: slog.LevelWarn},
	}

	for _, tc := range tests {
		tc := tc
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()

			var buf bytes.Buffer
			auditLog, err := New(&buf, tc.level)
			if err != nil {
				t.Fatalf("New() error = %v", err)
			}
			auditLog.RecordRequest(context.Background(), "alice", 200)

			got := auditLog.Output()
			if strings.Contains(got, "request details") != tc.wantDebug {
				t.Fatalf("debug presence = %v, want %v in %q", strings.Contains(got, "request details"), tc.wantDebug, got)
			}
			if strings.Contains(got, "request completed") != tc.wantInfo {
				t.Fatalf("info presence = %v, want %v in %q", strings.Contains(got, "request completed"), tc.wantInfo, got)
			}
		})
	}
}

func TestSetLevelChangesRuntimeFiltering(t *testing.T) {
	t.Parallel()

	var buf bytes.Buffer
	auditLog, err := New(&buf, slog.LevelInfo)
	if err != nil {
		t.Fatalf("New() error = %v", err)
	}

	auditLog.RecordRequest(context.Background(), "alice", 200)
	auditLog.SetLevel(slog.LevelDebug)
	auditLog.RecordRequest(context.Background(), "bob", 200)

	got := auditLog.Output()
	if strings.Contains(got, "user=alice") {
		t.Fatalf("debug field for alice should have been filtered: %q", got)
	}
	if !strings.Contains(got, "user=bob") {
		t.Fatalf("debug field for bob missing after SetLevel: %q", got)
	}
}

func TestNewRejectsNilBuffer(t *testing.T) {
	t.Parallel()

	_, err := New(nil, slog.LevelInfo)
	if !errors.Is(err, ErrNilBuffer) {
		t.Fatalf("New(nil) error = %v, want ErrNilBuffer", err)
	}
}

func ExampleAuditLog_SetLevel() {
	var buf bytes.Buffer
	auditLog, _ := New(&buf, slog.LevelInfo)
	auditLog.SetLevel(slog.LevelDebug)
	auditLog.RecordRequest(context.Background(), "alice", 200)
	fmt.Print(auditLog.Output())
	// Output:
	// level=DEBUG msg="request details" user=alice
	// level=INFO msg="request completed" status=200
}
```

Your turn: add a table case for a custom level between Info and Warn, then assert that Info is filtered but Warn would be enabled.

### Exercise 3: Add a Demo Command

Create `cmd/demo/main.go`:

```go
package main

import (
	"bytes"
	"context"
	"fmt"
	"log/slog"

	"example.com/sloglevels"
)

func main() {
	var buf bytes.Buffer
	auditLog, err := audit.New(&buf, slog.LevelInfo)
	if err != nil {
		panic(err)
	}
	auditLog.RecordRequest(context.Background(), "alice", 200)
	auditLog.SetLevel(slog.LevelDebug)
	auditLog.RecordRequest(context.Background(), "bob", 200)
	fmt.Print(auditLog.Output())
}
```

## Common Mistakes

Wrong: assume `slog.Debug` appears by default.

What happens: built-in handlers default to Info and discard Debug records.

Fix: configure `HandlerOptions{Level: slog.LevelDebug}` or use a `LevelVar`.

Wrong: rebuild the logger every time the operator changes verbosity.

What happens: logger references already passed through the application keep the old handler.

Fix: construct the handler once with `*slog.LevelVar` and call `Set` on that variable.

Wrong: compute expensive attributes before checking whether the level is enabled.

What happens: disabled logs still burn CPU.

Fix: use `logger.Enabled(ctx, level)` around expensive work or use `slog.LogValuer`.

## Verification

From `~/go-exercises/slog-levels`:

```bash
test -z "$(gofmt -l .)"
go vet ./...
go build ./...
go test -count=1 -race ./...
go run ./cmd/demo
```

Add at least one test of your own that changes the level at runtime.

## Summary

- `HandlerOptions.Level` is the handler's minimum enabled level.
- `slog.LevelVar` supports runtime level changes and is safe for concurrent use.
- Disabled log records do not get written, but call arguments are still evaluated.
- Tests should assert stable captured output, not terminal text inspected by eye.

## What's Next

Next, compare handler output formats in [JSON Handler vs Text Handler](../03-json-handler-vs-text-handler/03-json-handler-vs-text-handler.md).

## Resources

- `log/slog` levels documentation: https://pkg.go.dev/log/slog#hdr-Levels
- `slog.LevelVar` documentation: https://pkg.go.dev/log/slog#LevelVar
- Go blog, "Structured Logging with slog": https://go.dev/blog/slog
