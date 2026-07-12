# Exercise 2: Test-Attributed slog Handler over T.Output

When code under test logs through a real `slog.Logger`, those records should land
indented under the owning test, carry no misleading `file:line` of the logging
library, and never interleave across `-parallel` tests. `T.Output` is the writer that
makes all three true. This exercise builds a logger that routes structured
diagnostics through an injected `io.Writer` — a `bytes.Buffer` in unit tests,
`t.Output()` in a real test.

This module is fully self-contained. It begins with its own `go mod init`, defines
every type it needs, and ships its own demo and tests. Nothing here imports any other
exercise.

## What you'll build

```text
testlog/                    independent module: example.com/testlog
  go.mod                    go 1.25 (T.Output needs it)
  logger.go                 NewTestLogger(w io.Writer) *slog.Logger; time stripped
  cmd/
    demo/
      main.go               wires the logger to os.Stdout, emits two records
  logger_test.go            buffer-backed format assertion; t.Output() smoke test; Example
```

- Files: `logger.go`, `cmd/demo/main.go`, `logger_test.go`.
- Implement: `NewTestLogger(w io.Writer) *slog.Logger` backed by a text handler whose `ReplaceAttr` drops the volatile time attribute for deterministic output.
- Test: a `bytes.Buffer`-backed logger whose exact formatted lines are asserted (time stripped); a real smoke test that builds the logger with `t.Output()` and logs once; an `Example` with `// Output:`.
- Verify: `go test -count=1 -race ./...`

Set up the module. `testing.T.Output` requires Go 1.25+, so pin the language version:

```bash
go mod edit -go=1.25
```

### Why inject an io.Writer

The insight that makes this testable is that `T.Output()` returns an `io.Writer` —
nothing more exotic. So the logger constructor should depend on `io.Writer`, not on
`*testing.T`. In production test code you build it with `t.Output()`; in a unit test
you build it with a `bytes.Buffer` and assert the exact bytes; in `main` you build it
with `os.Stdout`. The logger has no idea which one it got. This is why the signature
is `NewTestLogger(w io.Writer) *slog.Logger` and not `NewTestLogger(t *testing.T)`:
the narrower dependency is both more honest and directly assertable.

Why route through `Output` at all instead of the default `slog` handler on
`os.Stderr`? Because of the three properties from the concepts file. `Output` indents
each line under the owning test and keeps it grouped even under `-parallel`, where raw
`os.Stderr` writes from different tests interleave into unreadable noise. And unlike
`t.Log`, `Output` does not prepend a source location — which matters here because the
caller of the write is the slog handler, so a `file:line` would point into the logging
machinery, not your test. `Output` preserves the handler's own bytes verbatim, which
is exactly what you want when the handler has already formatted a structured record.

### Deterministic output via ReplaceAttr

A text handler's default first field is `time=2006-01-02T15:04:05...`, which changes
every run and makes golden-output assertions impossible. `slog.HandlerOptions` has a
`ReplaceAttr` hook called for each non-group attribute; returning a zero
`slog.Attr{}` (one whose `Key` is empty) drops that attribute entirely, separators
included. The handler drops the built-in time attribute — identified by
`a.Key == slog.TimeKey` at the top level, where `len(groups) == 0` — so the very
first field on every line is `level`. Guarding on `len(groups) == 0` matters: a
user attribute literally named `time` inside a group should not be silently deleted;
only the top-level built-in time is volatile.

Create `logger.go`:

```go
package testlog

import (
	"io"
	"log/slog"
)

// NewTestLogger returns a slog.Logger whose text handler writes to w. In a real
// test, pass t.Output() so each record is indented under the owning test, carries
// no misleading source location, and does not interleave across parallel tests.
// The built-in time attribute is dropped so the output is deterministic and can be
// asserted byte-for-byte in unit tests driven by a bytes.Buffer.
func NewTestLogger(w io.Writer) *slog.Logger {
	h := slog.NewTextHandler(w, &slog.HandlerOptions{
		ReplaceAttr: func(groups []string, a slog.Attr) slog.Attr {
			if len(groups) == 0 && a.Key == slog.TimeKey {
				return slog.Attr{} // drop the volatile time field
			}
			return a
		},
	})
	return slog.New(h)
}
```

### The runnable demo

The demo wires the logger to `os.Stdout` and emits two structured records — an info
line and an error line — so you can see the text-handler encoding without the time
field. Because time is stripped, the output is stable enough to check by eye.

Create `cmd/demo/main.go`:

```go
package main

import (
	"os"

	"example.com/testlog"
)

func main() {
	logger := testlog.NewTestLogger(os.Stdout)
	logger.Info("server started", "addr", ":8080", "env", "prod")
	logger.Error("upstream timeout", "service", "billing", "after_ms", 3000)
}
```

Run it:

```bash
go run ./cmd/demo
```

Expected output:

```
level=INFO msg="server started" addr=:8080 env=prod
level=ERROR msg="upstream timeout" service=billing after_ms=3000
```

### Tests

The unit test builds the logger over a `bytes.Buffer` and asserts the exact two lines,
proving the format is stable and that there are no duplicate trailing newlines (the
text handler emits exactly one `\n` per record). The smoke test builds the logger with
`t.Output()` and logs one record — it does not assert bytes (the indentation is the
test framework's) but it proves the real writer path compiles and runs on `*testing.T`;
run it under `go test -v` to see the line indented under the test. The `Example`
demonstrates the deterministic encoding through `os.Stdout` with an `// Output:` block.

A note the concepts file stresses: any goroutine that writes through `t.Output()` must
be joined before the test returns, because the writer may not be used after the test
and its parents return. The smoke test logs synchronously, so there is nothing to
join here; keep that discipline the moment you add a background writer.

Create `logger_test.go`:

```go
package testlog

import (
	"bytes"
	"os"
	"testing"
)

func TestLoggerFormat(t *testing.T) {
	t.Parallel()
	var buf bytes.Buffer
	logger := NewTestLogger(&buf)

	logger.Info("request handled", "method", "GET", "status", 200)
	logger.Warn("slow query", "ms", 512)

	got := buf.String()
	want := "level=INFO msg=\"request handled\" method=GET status=200\n" +
		"level=WARN msg=\"slow query\" ms=512\n"
	if got != want {
		t.Fatalf("logger output:\ngot  %q\nwant %q", got, want)
	}
}

// TestOutputWriterPath proves the real t.Output() path: NewTestLogger accepts the
// io.Writer that Output returns, and the record is flushed as the test ends. Run
// `go test -v` to see it indented under this test.
func TestOutputWriterPath(t *testing.T) {
	logger := NewTestLogger(t.Output())
	logger.Info("smoke", "component", "testlog", "ok", true)
}

func ExampleNewTestLogger() {
	logger := NewTestLogger(os.Stdout)
	logger.Info("cache miss", "key", "session:42", "backend", "redis")
	// Output: level=INFO msg="cache miss" key=session:42 backend=redis
}
```

## Review

The logger is correct when its output is deterministic and framed by the handler, not
by `Output`. Determinism comes from dropping the top-level time attribute in
`ReplaceAttr`; `TestLoggerFormat` asserts the exact bytes, so if you forgot the
`len(groups) == 0` guard and dropped a nested `time` attribute too, or if you left
the time field in, the assertion fails. The single trailing `\n` per line is the text
handler's own framing — `Output` adds none of its own, which is precisely why it is
the right sink for pre-formatted records.

The mistakes to avoid: do not make the constructor take `*testing.T` — depending on
`io.Writer` is what lets the same code be driven by a `bytes.Buffer`, `os.Stdout`, and
`t.Output()` interchangeably. Do not use `fmt.Println` or a handler on `os.Stderr` for
in-test diagnostics; you lose the per-test indentation and get interleaving under
`t.Parallel()`. And never write to the `Output` writer from a goroutine that outlives
the test: `TestOutputWriterPath` logs synchronously for that reason. Run
`go test -race -v` and confirm the smoke line appears indented under its test.

## Resources

- [`testing.T.Output`](https://pkg.go.dev/testing#T.Output) — the writer's indentation, line-buffering, flush-on-Log, and lifetime rules.
- [`log/slog`](https://pkg.go.dev/log/slog) — `slog.New`, `slog.NewTextHandler`, and the record encoding.
- [`slog.HandlerOptions`](https://pkg.go.dev/log/slog#HandlerOptions) — the `ReplaceAttr` hook and how returning an empty `Attr` drops a field.

---

Back to [00-concepts.md](00-concepts.md) | Next: [03-json-attr-report.md](03-json-attr-report.md)
