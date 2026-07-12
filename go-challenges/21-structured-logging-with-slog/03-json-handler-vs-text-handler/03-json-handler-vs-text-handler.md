# 3. JSON Handler vs Text Handler

`slog.TextHandler` and `slog.JSONHandler` receive the same records but encode them differently. This lesson builds a small factory that chooses a handler format, keeps tests deterministic with a text buffer, and verifies the JSON path by decoding actual JSON.

## Concepts

### Same Record, Different Encoding

A `*slog.Logger` creates records; the handler decides the wire format. `TextHandler` writes key-value pairs such as `level=INFO msg=...`, which is good for local reading. `JSONHandler` writes one JSON object per record, which is better for production log pipelines.

### Handler Options

Both built-in handlers accept `*slog.HandlerOptions`. `AddSource` adds source file information, `Level` filters records, and `ReplaceAttr` can rewrite or drop attributes. Tests commonly remove `slog.TimeKey` so output is stable.

### Format Selection Belongs at the Edge

Library code should not decide whether production uses text or JSON. A small constructor can accept a format enum and an `io.Writer`, then return a logger while the rest of the package only depends on `*slog.Logger`.

## Exercises

Set up the module:

```bash
mkdir -p go-solutions/21-structured-logging-with-slog/03-json-handler-vs-text-handler/03-json-handler-vs-text-handler
cd go-solutions/21-structured-logging-with-slog/03-json-handler-vs-text-handler/03-json-handler-vs-text-handler
```

Edit `go.mod`:

```go
module example.com/sloghandlers

go 1.26
```

### Exercise 1: Build a Handler Factory

Create `logger.go`:

```go
package logformat

import (
	"errors"
	"fmt"
	"io"
	"log/slog"
)

type Format string

const (
	FormatText Format = "text"
	FormatJSON Format = "json"
)

var (
	ErrNilWriter     = errors.New("writer must not be nil")
	ErrUnknownFormat = errors.New("unknown log format")
)

type Config struct {
	Format    Format
	Level     slog.Level
	AddSource bool
}

func NewLogger(w io.Writer, cfg Config) (*slog.Logger, error) {
	if w == nil {
		return nil, fmt.Errorf("new logger: %w", ErrNilWriter)
	}
	opts := &slog.HandlerOptions{
		Level:     cfg.Level,
		AddSource: cfg.AddSource,
		ReplaceAttr: func(groups []string, a slog.Attr) slog.Attr {
			if len(groups) == 0 && a.Key == slog.TimeKey {
				return slog.Attr{}
			}
			return a
		},
	}

	switch cfg.Format {
	case "", FormatText:
		return slog.New(slog.NewTextHandler(w, opts)), nil
	case FormatJSON:
		return slog.New(slog.NewJSONHandler(w, opts)), nil
	default:
		return nil, fmt.Errorf("new logger: %w: %s", ErrUnknownFormat, cfg.Format)
	}
}

func LogStartup(logger *slog.Logger, service string) {
	logger.Info("service started", slog.String("service", service), slog.Int("port", 8080))
}
```

### Exercise 2: Test Text, JSON, and Validation

Create `logger_test.go`:

```go
package logformat

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"strings"
	"testing"
)

func TestNewLoggerTextOutput(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name   string
		format Format
	}{
		{name: "explicit text", format: FormatText},
		{name: "default text", format: ""},
	}

	for _, tc := range tests {
		tc := tc
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()

			var buf bytes.Buffer
			logger, err := NewLogger(&buf, Config{Format: tc.format, Level: slog.LevelInfo})
			if err != nil {
				t.Fatalf("NewLogger() error = %v", err)
			}
			LogStartup(logger, "billing")

			got := buf.String()
			for _, want := range []string{"level=INFO", "msg=\"service started\"", "service=billing", "port=8080"} {
				if !strings.Contains(got, want) {
					t.Fatalf("text output %q missing %q", got, want)
				}
			}
		})
	}
}

func TestNewLoggerJSONOutput(t *testing.T) {
	t.Parallel()

	var buf bytes.Buffer
	logger, err := NewLogger(&buf, Config{Format: FormatJSON, Level: slog.LevelInfo})
	if err != nil {
		t.Fatalf("NewLogger() error = %v", err)
	}
	LogStartup(logger, "billing")

	var got map[string]any
	if err := json.Unmarshal(buf.Bytes(), &got); err != nil {
		t.Fatalf("json output did not decode: %v", err)
	}
	if got["msg"] != "service started" || got["service"] != "billing" || got["level"] != "INFO" {
		t.Fatalf("decoded output = %#v", got)
	}
	if got["port"] != float64(8080) {
		t.Fatalf("port = %#v, want 8080", got["port"])
	}
}

func TestNewLoggerValidation(t *testing.T) {
	t.Parallel()

	_, err := NewLogger(nil, Config{Format: FormatText})
	if !errors.Is(err, ErrNilWriter) {
		t.Fatalf("NewLogger(nil) error = %v, want ErrNilWriter", err)
	}

	tests := []struct {
		name string
		cfg  Config
		want error
	}{
		{name: "unknown format", cfg: Config{Format: "xml"}, want: ErrUnknownFormat},
	}

	for _, tc := range tests {
		tc := tc
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()

			_, err := NewLogger(new(bytes.Buffer), tc.cfg)
			if !errors.Is(err, tc.want) {
				t.Fatalf("NewLogger() error = %v, want %v", err, tc.want)
			}
		})
	}
}

func ExampleNewLogger() {
	var buf bytes.Buffer
	logger, _ := NewLogger(&buf, Config{Format: FormatText, Level: slog.LevelInfo})
	LogStartup(logger, "billing")
	fmt.Print(buf.String())
	// Output: level=INFO msg="service started" service=billing port=8080
}
```

Your turn: add a test that sets `Level: slog.LevelWarn` and proves the Info startup record is filtered.

### Exercise 3: Add a Demo Command

Create `cmd/demo/main.go`:

```go
package main

import (
	"bytes"
	"fmt"
	"log/slog"

	"example.com/sloghandlers"
)

func main() {
	var buf bytes.Buffer
	logger, err := logformat.NewLogger(&buf, logformat.Config{Format: logformat.FormatText, Level: slog.LevelInfo})
	if err != nil {
		panic(err)
	}
	logformat.LogStartup(logger, "billing")
	fmt.Print(buf.String())
}
```

## Common Mistakes

Wrong: decide text versus JSON inside business functions.

What happens: every caller inherits one deployment assumption.

Fix: build the logger at the application edge and pass `*slog.Logger` inward.

Wrong: assert JSON output with string contains only.

What happens: tests can miss invalid JSON or type changes.

Fix: decode JSON logs with `encoding/json` when testing the JSON handler.

Wrong: enable `AddSource` in exact-output examples without normalizing paths.

What happens: output depends on local file paths and line numbers.

Fix: test source behavior separately or use `ReplaceAttr` to normalize it.

## Verification

From `~/go-exercises/slog-handlers`:

```bash
test -z "$(gofmt -l .)"
go vet ./...
go build ./...
go test -count=1 -race ./...
go run ./cmd/demo
```

Add at least one test of your own before moving on.

## Summary

- `TextHandler` and `JSONHandler` encode the same records differently.
- Handler selection should happen at application boundaries.
- `ReplaceAttr` is useful for deterministic tests.
- JSON logs should be tested by decoding JSON, not by eyeballing strings.

## What's Next

Next, organize related fields with [Groups and Nested Attributes](../04-groups-and-nested-attributes/04-groups-and-nested-attributes.md).

## Resources

- `slog.NewTextHandler` documentation: https://pkg.go.dev/log/slog#NewTextHandler
- `slog.NewJSONHandler` documentation: https://pkg.go.dev/log/slog#NewJSONHandler
- `slog.HandlerOptions` documentation: https://pkg.go.dev/log/slog#HandlerOptions
- Go blog, "Structured Logging with slog": https://go.dev/blog/slog
