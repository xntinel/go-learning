# Exercise 3: Structured Logger Factory with Options

Every service needs one place that decides how logs are shaped: JSON in
production, text in development, a level that can be turned up at runtime without a
redeploy, and a standard set of attributes on every line. This module builds that
`slog` factory with options that configure a `slog.HandlerOptions` struct and hand
back a live `*slog.LevelVar` the caller can adjust while the process runs.

This module is fully self-contained: its own `go mod init`, demo, and tests.

## What you'll build

```text
logfactory/                      independent module: example.com/logfactory
  go.mod                         go 1.26
  logfactory.go                  Option, NewLogger(io.Writer, ...Option) (*slog.Logger, *slog.LevelVar, error),
                                 WithLevel, WithJSON, WithText, WithSource, WithAttrs, WithReplaceAttr
  cmd/
    demo/
      main.go                    logs one JSON line with default attrs and stripped time
  logfactory_test.go             parses JSON output, proves level filtering and runtime level change
```

- Files: `logfactory.go`, `cmd/demo/main.go`, `logfactory_test.go`.
- Implement: `NewLogger(w, opts...) (*slog.Logger, *slog.LevelVar, error)` that seeds a JSON handler at Info level, applies options that mutate a `slog.HandlerOptions`, and returns the logger together with the `*slog.LevelVar` for runtime adjustment.
- Test: unmarshal one JSON line and assert `level`/`msg`/default-attr keys; prove `WithLevel(slog.LevelWarn)` drops an Info record; prove `levelVar.Set(slog.LevelDebug)` re-enables Debug at runtime.
- Verify: `go test -count=1 ./...`

Set up the module:

```bash
mkdir -p ~/go-exercises/logfactory/cmd/demo
cd ~/go-exercises/logfactory
go mod init example.com/logfactory
```

### Options that configure a third-party struct

`slog` already exposes its configuration as a struct — `slog.HandlerOptions` with
`Level`, `AddSource`, and `ReplaceAttr`. The factory's options do not reinvent
that; they *populate* it. `WithSource` sets `AddSource`, `WithReplaceAttr` sets
`ReplaceAttr`, and `WithJSON`/`WithText` pick between `slog.NewJSONHandler` and
`slog.NewTextHandler`. This is a common real shape: your options are a thin,
validating, named front end over a library's own options struct.

The `Level` field is the interesting one. `slog.HandlerOptions.Level` is a
`slog.Leveler`, and `*slog.LevelVar` implements `Leveler` while also being
mutable and safe for concurrent use. By seeding the handler with a `*slog.LevelVar`
and returning it, the factory gives the caller a live handle: calling
`levelVar.Set(slog.LevelDebug)` at runtime raises verbosity for every logger built
on that handler, no restart required. That is why `NewLogger` returns the
`*slog.LevelVar` alongside the logger — the handle is the whole point of using a
`LevelVar` instead of a plain `slog.Level`.

### Deterministic output through ReplaceAttr

Structured logs normally carry a timestamp, which makes their output impossible to
assert on. `ReplaceAttr` is `slog`'s hook for rewriting or dropping attributes as
they are emitted — the same mechanism you would use in production to redact a
secret field. Here both the demo and the tests install a `ReplaceAttr` that drops
the top-level `time` key (returning the zero `slog.Attr` removes it), which makes
the JSON line stable and testable.

Create `logfactory.go`:

```go
package logfactory

import (
	"fmt"
	"io"
	"log/slog"
)

type config struct {
	level       *slog.LevelVar
	json        bool
	addSource   bool
	attrs       []slog.Attr
	replaceAttr func([]string, slog.Attr) slog.Attr
}

// Option configures the logger factory and may reject invalid input.
type Option func(*config) error

// NewLogger builds a *slog.Logger writing to w. It returns the logger and the
// *slog.LevelVar backing its level so the caller can adjust verbosity at runtime.
func NewLogger(w io.Writer, opts ...Option) (*slog.Logger, *slog.LevelVar, error) {
	c := &config{level: new(slog.LevelVar), json: true}
	c.level.Set(slog.LevelInfo)

	for _, opt := range opts {
		if err := opt(c); err != nil {
			return nil, nil, err
		}
	}

	ho := &slog.HandlerOptions{
		Level:       c.level,
		AddSource:   c.addSource,
		ReplaceAttr: c.replaceAttr,
	}

	var h slog.Handler
	if c.json {
		h = slog.NewJSONHandler(w, ho)
	} else {
		h = slog.NewTextHandler(w, ho)
	}

	logger := slog.New(h)
	if len(c.attrs) > 0 {
		args := make([]any, len(c.attrs))
		for i, a := range c.attrs {
			args[i] = a
		}
		logger = logger.With(args...)
	}
	return logger, c.level, nil
}

// WithLevel sets the initial minimum level.
func WithLevel(level slog.Level) Option {
	return func(c *config) error {
		c.level.Set(level)
		return nil
	}
}

// WithJSON selects the JSON handler (the default).
func WithJSON() Option {
	return func(c *config) error {
		c.json = true
		return nil
	}
}

// WithText selects the text handler.
func WithText() Option {
	return func(c *config) error {
		c.json = false
		return nil
	}
}

// WithSource enables source-location attributes.
func WithSource() Option {
	return func(c *config) error {
		c.addSource = true
		return nil
	}
}

// WithAttrs adds default attributes present on every record. Each attr must
// have a non-empty key.
func WithAttrs(attrs ...slog.Attr) Option {
	return func(c *config) error {
		for _, a := range attrs {
			if a.Key == "" {
				return fmt.Errorf("default attribute has empty key")
			}
		}
		c.attrs = append(c.attrs, attrs...)
		return nil
	}
}

// WithReplaceAttr installs a ReplaceAttr hook (for example to redact or drop
// fields). A nil hook is rejected.
func WithReplaceAttr(fn func([]string, slog.Attr) slog.Attr) Option {
	return func(c *config) error {
		if fn == nil {
			return fmt.Errorf("replaceAttr hook is nil")
		}
		c.replaceAttr = fn
		return nil
	}
}
```

### The runnable demo

Create `cmd/demo/main.go`:

```go
package main

import (
	"log/slog"
	"os"

	"example.com/logfactory"
)

func stripTime(groups []string, a slog.Attr) slog.Attr {
	if len(groups) == 0 && a.Key == slog.TimeKey {
		return slog.Attr{}
	}
	return a
}

func main() {
	logger, level, err := logfactory.NewLogger(os.Stdout,
		logfactory.WithJSON(),
		logfactory.WithReplaceAttr(stripTime),
		logfactory.WithAttrs(slog.String("service", "orders"), slog.String("version", "1.2.3")),
	)
	if err != nil {
		panic(err)
	}

	logger.Info("order placed", slog.Int("id", 42))
	logger.Debug("this is dropped at info level")

	level.Set(slog.LevelDebug)
	logger.Debug("now visible after raising verbosity")
}
```

Run it:

```bash
go run ./cmd/demo
```

Expected output:

```
{"level":"INFO","msg":"order placed","service":"orders","version":"1.2.3","id":42}
{"level":"DEBUG","msg":"now visible after raising verbosity","service":"orders","version":"1.2.3"}
```

### Tests

`TestJSONShape` logs one line, unmarshals it, and asserts the level, message, and
a default attribute all arrived. `TestLevelFiltering` proves `WithLevel(Warn)`
drops an Info record — the buffer stays empty. `TestRuntimeLevelChange` proves the
returned `*slog.LevelVar` is live: a Debug record is dropped, then after
`level.Set(Debug)` a second Debug record appears. `TestRejectsEmptyAttrKey` proves
the one validating option fails cleanly.

Create `logfactory_test.go`:

```go
package logfactory

import (
	"bytes"
	"encoding/json"
	"log/slog"
	"strings"
	"testing"
)

func stripTime(groups []string, a slog.Attr) slog.Attr {
	if len(groups) == 0 && a.Key == slog.TimeKey {
		return slog.Attr{}
	}
	return a
}

func TestJSONShape(t *testing.T) {
	t.Parallel()

	var buf bytes.Buffer
	logger, _, err := NewLogger(&buf,
		WithJSON(),
		WithReplaceAttr(stripTime),
		WithAttrs(slog.String("service", "orders")),
	)
	if err != nil {
		t.Fatal(err)
	}

	logger.Info("hello", slog.Int("id", 7))

	var rec map[string]any
	if err := json.Unmarshal(buf.Bytes(), &rec); err != nil {
		t.Fatalf("unmarshal %q: %v", buf.String(), err)
	}
	if rec["level"] != "INFO" {
		t.Errorf("level = %v, want INFO", rec["level"])
	}
	if rec["msg"] != "hello" {
		t.Errorf("msg = %v, want hello", rec["msg"])
	}
	if rec["service"] != "orders" {
		t.Errorf("service = %v, want orders", rec["service"])
	}
}

func TestLevelFiltering(t *testing.T) {
	t.Parallel()

	var buf bytes.Buffer
	logger, _, err := NewLogger(&buf, WithLevel(slog.LevelWarn), WithReplaceAttr(stripTime))
	if err != nil {
		t.Fatal(err)
	}

	logger.Info("filtered out")

	if buf.Len() != 0 {
		t.Fatalf("expected empty buffer at warn level, got %q", buf.String())
	}
}

func TestRuntimeLevelChange(t *testing.T) {
	t.Parallel()

	var buf bytes.Buffer
	logger, level, err := NewLogger(&buf, WithReplaceAttr(stripTime))
	if err != nil {
		t.Fatal(err)
	}

	logger.Debug("dropped at info")
	if buf.Len() != 0 {
		t.Fatalf("debug should be dropped at info level, got %q", buf.String())
	}

	level.Set(slog.LevelDebug)
	logger.Debug("now visible")

	if !strings.Contains(buf.String(), "now visible") {
		t.Fatalf("debug not enabled after level.Set(Debug), got %q", buf.String())
	}
}

func TestRejectsEmptyAttrKey(t *testing.T) {
	t.Parallel()

	var buf bytes.Buffer
	_, _, err := NewLogger(&buf, WithAttrs(slog.String("", "oops")))
	if err == nil {
		t.Fatal("expected error for empty attribute key, got nil")
	}
}
```

## Review

The factory is correct when its options populate `slog.HandlerOptions` faithfully
and when the returned `*slog.LevelVar` genuinely controls the live handler — the
proof is `TestRuntimeLevelChange`, where mutating the returned handle after
construction changes what the already-built logger emits. Returning the
`LevelVar` rather than swallowing it is the design decision that makes runtime log
levels possible; a plain `slog.Level` would freeze verbosity at construction. The
`ReplaceAttr` hook doubles as the tool that makes structured output testable and
the tool you would reach for to redact secrets in production, which is why it is a
first-class option here.

## Resources

- [log/slog package](https://pkg.go.dev/log/slog)
- [log/slog HandlerOptions](https://pkg.go.dev/log/slog#HandlerOptions)
- [log/slog LevelVar](https://pkg.go.dev/log/slog#LevelVar)
- [Structured Logging with slog (Go blog)](https://go.dev/blog/slog)

---

Back to [00-concepts.md](00-concepts.md) | Previous: [02-db-pool-options.md](02-db-pool-options.md) | Next: [04-http-server-timeouts-options.md](04-http-server-timeouts-options.md)
