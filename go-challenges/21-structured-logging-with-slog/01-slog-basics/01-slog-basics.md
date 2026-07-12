# 1. Slog Basics

Structured logging is useful because the event message and the event data stay separate. This lesson builds a tiny library that logs account activity with `log/slog`, captures output through `slog.NewTextHandler`, and verifies the result with tests instead of visual inspection.

## Concepts

### Records, Handlers, and Attributes

`log/slog` separates the frontend `*slog.Logger` from the backend `slog.Handler`. A logger call creates a record with a time, level, message, and attributes. The handler decides whether the record is enabled and how to write it.

Attributes are not string concatenation. `slog.String("user", "alice")` keeps the key and value separate, which lets handlers emit parseable text or JSON. The alternating key-value form is convenient, but explicit `slog.Attr` constructors are harder to get wrong and work with `LogAttrs`.

### Deterministic Output in Tests

The built-in handlers normally include a timestamp, which makes exact output assertions brittle. `slog.HandlerOptions.ReplaceAttr` can remove the top-level `time` attribute. Capturing output with `slog.NewTextHandler(&buf, opts)` gives deterministic log lines that `go test` can verify.

### Logger Injection

Library code should accept a logger instead of calling the package-level `slog.Info` functions. That makes the code testable, avoids global state, and lets callers choose `TextHandler`, `JSONHandler`, levels, and destinations.

## Exercises

Set up the module:

```bash
mkdir -p go-solutions/21-structured-logging-with-slog/01-slog-basics/01-slog-basics
cd go-solutions/21-structured-logging-with-slog/01-slog-basics/01-slog-basics
```

Edit `go.mod` so it pins the toolchain version used by this curriculum:

```go
module example.com/slogbasics

go 1.26
```

This is a library, not a print-only program. The demo is separate under `cmd/demo`, and the contract is verified with `go test`.

### Exercise 1: Build a Recorder

Create `activity.go`:

```go
package activity

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"strings"
)

var ErrEmptyUser = errors.New("user must not be empty")

type Recorder struct {
	logger *slog.Logger
}

func New(logger *slog.Logger) (*Recorder, error) {
	if logger == nil {
		return nil, fmt.Errorf("activity recorder: %w", ErrNilLogger)
	}
	return &Recorder{logger: logger}, nil
}

var ErrNilLogger = errors.New("logger must not be nil")

func (r *Recorder) Logger() *slog.Logger {
	return r.logger
}

func (r *Recorder) Login(ctx context.Context, user, role string) error {
	user = strings.TrimSpace(user)
	if user == "" {
		return fmt.Errorf("login: %w", ErrEmptyUser)
	}
	if role == "" {
		role = "member"
	}
	r.logger.LogAttrs(ctx, slog.LevelInfo, "user logged in",
		slog.String("user", user),
		slog.String("role", role),
	)
	return nil
}
```

Defaults and validation belong in the package, not in an example program. `ErrEmptyUser` and `ErrNilLogger` are sentinels so callers and tests can use `errors.Is`.

### Exercise 2: Capture Text Output

Create `activity_test.go`:

```go
package activity

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

func TestLoginWritesStructuredFields(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name string
		user string
		role string
		want []string
	}{
		{name: "explicit role", user: "alice", role: "admin", want: []string{"level=INFO", "msg=\"user logged in\"", "user=alice", "role=admin"}},
		{name: "default role", user: "bob", role: "", want: []string{"user=bob", "role=member"}},
	}

	for _, tc := range tests {
		tc := tc
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()

			var buf bytes.Buffer
			recorder, err := New(testLogger(&buf))
			if err != nil {
				t.Fatalf("New() error = %v", err)
			}

			if err := recorder.Login(context.Background(), tc.user, tc.role); err != nil {
				t.Fatalf("Login() error = %v", err)
			}

			got := buf.String()
			for _, part := range tc.want {
				if !strings.Contains(got, part) {
					t.Fatalf("log output %q missing %q", got, part)
				}
			}
		})
	}
}

func TestLoginRejectsEmptyUser(t *testing.T) {
	t.Parallel()

	var buf bytes.Buffer
	recorder, err := New(testLogger(&buf))
	if err != nil {
		t.Fatalf("New() error = %v", err)
	}

	err = recorder.Login(context.Background(), "   ", "admin")
	if !errors.Is(err, ErrEmptyUser) {
		t.Fatalf("Login() error = %v, want ErrEmptyUser", err)
	}
}

func TestNewRejectsNilLogger(t *testing.T) {
	t.Parallel()

	_, err := New(nil)
	if !errors.Is(err, ErrNilLogger) {
		t.Fatalf("New(nil) error = %v, want ErrNilLogger", err)
	}
}

func ExampleRecorder_Login() {
	var buf bytes.Buffer
	recorder, _ := New(testLogger(&buf))
	_ = recorder.Login(context.Background(), "alice", "admin")
	fmt.Print(buf.String())
	// Output: level=INFO msg="user logged in" user=alice role=admin
}
```

Your turn: add a table row that logs a user with surrounding spaces and asserts that the trimmed value appears in the output.

### Exercise 3: Add a Demo Command

Create `cmd/demo/main.go`:

```go
package main

import (
	"bytes"
	"context"
	"fmt"
	"log/slog"

	"example.com/slogbasics"
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

	recorder, err := activity.New(logger)
	if err != nil {
		panic(err)
	}
	_ = recorder.Login(context.Background(), "alice", "admin")
	fmt.Print(buf.String())
}
```

The demo imports the package and uses only exported API: `activity.New`, `Recorder.Login`, and the standard `slog` constructors.

## Common Mistakes

Wrong: build messages with `fmt.Sprintf("user %s logged in", user)` and call that structured logging.

What happens: downstream systems must parse prose to recover fields that were already available in code.

Fix: keep the stable message short and attach data with `slog.String`, `slog.Int`, and other attribute constructors.

Wrong: use package-level `slog.Info` inside library code.

What happens: tests must mutate global state, and applications cannot route that library's logs independently.

Fix: accept `*slog.Logger` in a constructor and capture output with a handler in tests.

Wrong: assert exact timestamps in tests.

What happens: the test changes every run.

Fix: remove the top-level `time` attribute with `ReplaceAttr` or assert only stable fields.

## Verification

From `~/go-exercises/slog-basics`:

```bash
test -z "$(gofmt -l .)"
go vet ./...
go build ./...
go test -count=1 -race ./...
go run ./cmd/demo
```

Add at least one test of your own before considering the lesson complete. `go test` is the verification; the demo is only a runnable example of the exported API.

## Summary

- `slog` emits records with levels, messages, and structured attributes.
- `TextHandler` can write deterministic output to a `bytes.Buffer` for tests and examples.
- Library packages should receive a `*slog.Logger` instead of using global logging directly.
- Sentinel validation errors make logging helpers testable with `errors.Is`.

## What's Next

Next, configure minimum levels and runtime filtering in [Log Levels and Filtering](../02-log-levels-and-filtering/02-log-levels-and-filtering.md).

## Resources

- `log/slog` package documentation: https://pkg.go.dev/log/slog
- Go blog, "Structured Logging with slog": https://go.dev/blog/slog
- Effective Go, formatting guidance: https://go.dev/doc/effective_go#formatting
