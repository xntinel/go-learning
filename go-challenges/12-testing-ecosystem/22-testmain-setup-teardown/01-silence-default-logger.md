# Exercise 1: Silence and Restore the Default Logger in TestMain

Real services log through a package-level `slog` logger, and their tests should
not spray structured log lines all over the test output. The clean way to quiet a
suite is to swap the process default logger for a discarding one exactly once in
`TestMain`, restore the original afterward, and prove — with a test — that the
suite really is silent.

This module is fully self-contained: its own `go mod init`, its own logger, its
own demo, and its own tests. Nothing here imports any other exercise.

## What you'll build

```text
loggersilence/                 independent module: example.com/loggersilence
  go.mod                       go 1.26
  logger.go                    Info/Warn wrappers over slog.Default()
  cmd/
    demo/
      main.go                  runnable demo: emit an Info and a Warn line
  logger_test.go               TestMain swaps in a discarding handler and restores it
```

Files: `logger.go`, `cmd/demo/main.go`, `logger_test.go`.
Implement: `Info(msg, args...)` and `Warn(msg, args...)` delegating to `slog.Default()`.
Test: a `TestMain` that saves `slog.Default()`, installs a handler writing to a `bytes.Buffer`, runs, restores, and `os.Exit(m.Run())`; plus `TestInfoLogs`, `TestWarnLogs`, `TestLoggerIsSilent` (captures output and asserts it is empty), and `TestWarnPassesLeveledHandler` (asserts a WARN record passes a WARN-leveled handler).
Verify: `go test -count=1 -race ./...`

Set up the module:

```bash
mkdir -p go-solutions/12-testing-ecosystem/22-testmain-setup-teardown/01-silence-default-logger/cmd/demo
cd go-solutions/12-testing-ecosystem/22-testmain-setup-teardown/01-silence-default-logger
```

### Why the wrappers delegate to slog.Default()

The logger is not its own handler. `Info` and `Warn` call `slog.Default().Info`
and `slog.Default().Warn`, so whatever handler is installed as the process default
at call time is the one that formats the record. In production that default is a
`slog.NewJSONHandler(os.Stderr, ...)` set up in `main`. In tests we want it to be
a handler that throws bytes away. Because the wrappers read `slog.Default()` on
every call (rather than capturing a `*slog.Logger` at package-init time), swapping
the default in `TestMain` actually takes effect for the code under test. If the
wrappers had captured `var logger = slog.Default()` at init, the swap in
`TestMain` would silence nothing — the wrappers would still hold the old logger.
That subtle timing is the whole reason this pattern lives in `TestMain` and reads
the default lazily.

### Why save and restore

`slog.SetDefault` mutates process-global state. If the test binary for this
package runs alongside others (as under `go test ./...`), an unrestored default
handler could leak into a sibling package's expectations. The discipline is:
`prev := slog.Default()` before the swap, `slog.SetDefault(prev)` after `m.Run()`.
Because we also call `os.Exit`, the restore must happen *before* `os.Exit` — which
is why the code captures `code := m.Run()` first, restores, and only then exits.

### The silence contract

`TestLoggerIsSilent` does not trust the global swap blindly; it pins the contract
directly. It builds a *local* handler over a `bytes.Buffer` at a level that
discards `Info`, installs it, calls `Info`, and asserts the buffer is empty. This
proves the leveling actually suppresses `Info` records rather than merely writing
them somewhere we forgot to check. It restores the default it replaced so it does
not disturb the buffer `TestMain` installed.

Create `logger.go`:

```go
package loggersilence

import "log/slog"

// Info logs at INFO through the process default logger. It reads slog.Default()
// on every call, so a handler swapped in by TestMain takes effect here.
func Info(msg string, args ...any) {
	slog.Default().Info(msg, args...)
}

// Warn logs at WARN through the process default logger.
func Warn(msg string, args ...any) {
	slog.Default().Warn(msg, args...)
}
```

### The runnable demo

The demo installs a real text handler on stderr (as `main` would) and emits one
`Info` and one `Warn` line, so you can see the wrappers formatting real records.

Create `cmd/demo/main.go`:

```go
package main

import (
	"log/slog"
	"os"

	"example.com/loggersilence"
)

func main() {
	// Deterministic output: no timestamps, write to stdout for the demo.
	h := slog.NewTextHandler(os.Stdout, &slog.HandlerOptions{
		ReplaceAttr: func(groups []string, a slog.Attr) slog.Attr {
			if a.Key == slog.TimeKey {
				return slog.Attr{}
			}
			return a
		},
	})
	slog.SetDefault(slog.New(h))

	loggersilence.Info("service started", "port", 8080)
	loggersilence.Warn("cache miss", "key", "session:42")
}
```

Run it:

```bash
go run ./cmd/demo
```

Expected output:

```
level=INFO msg="service started" port=8080
level=WARN msg="cache miss" key=session:42
```

### Tests

`TestMain` is the suite-wide silencer. It saves the current default, installs a
discarding handler (here a text handler over a `bytes.Buffer`), runs the tests,
restores the saved default, and exits with the captured code. `TestInfoLogs` and
`TestWarnLogs` simply exercise the wrappers — their output is swallowed.
`TestLoggerIsSilent` proves the silence contract with a leveled local handler.

Create `logger_test.go`:

```go
package loggersilence

import (
	"bytes"
	"log/slog"
	"os"
	"testing"
)

func TestMain(m *testing.M) {
	prev := slog.Default()
	// Silence the whole suite: everything the wrappers emit goes to a buffer
	// that no test reads.
	var sink bytes.Buffer
	slog.SetDefault(slog.New(slog.NewTextHandler(&sink, nil)))

	code := m.Run()

	slog.SetDefault(prev) // restore before exiting
	os.Exit(code)
}

func TestInfoLogs(t *testing.T) {
	t.Parallel()
	// Output is swallowed by the handler TestMain installed.
	Info("hello", "user", "alice")
}

func TestWarnLogs(t *testing.T) {
	t.Parallel()
	Warn("warn", "code", 42)
}

func TestLoggerIsSilent(t *testing.T) {
	// Not parallel: it swaps the process default for the duration of the test.
	prev := slog.Default()
	defer slog.SetDefault(prev)

	var buf bytes.Buffer
	// A handler leveled at WARN discards INFO records entirely.
	h := slog.NewTextHandler(&buf, &slog.HandlerOptions{Level: slog.LevelWarn})
	slog.SetDefault(slog.New(h))

	Info("this must not appear", "user", "bob")

	if buf.Len() != 0 {
		t.Fatalf("expected no output for INFO under a WARN handler, got %q", buf.String())
	}
}

func TestWarnPassesLeveledHandler(t *testing.T) {
	prev := slog.Default()
	defer slog.SetDefault(prev)

	var buf bytes.Buffer
	slog.SetDefault(slog.New(slog.NewTextHandler(&buf, &slog.HandlerOptions{Level: slog.LevelWarn})))

	Warn("disk almost full", "pct", 92)

	if !bytes.Contains(buf.Bytes(), []byte("disk almost full")) {
		t.Fatalf("WARN record should pass a WARN-leveled handler, got %q", buf.String())
	}
}
```

## Review

The suite is correct when three things hold. First, the wrappers read
`slog.Default()` lazily on each call, so `TestMain`'s swap actually silences them;
if you had captured the logger at init, `TestLoggerIsSilent` would still see
output. Second, `TestMain` restores the prior default before `os.Exit`, so the
mutation does not leak — and it captures `code := m.Run()` first precisely because
`os.Exit` skips anything after it. Third, `TestLoggerIsSilent` proves the contract
with a leveled handler rather than assuming the global buffer stays empty, and it
restores whatever default it replaced. Run `go test -race` to confirm the parallel
`TestInfoLogs`/`TestWarnLogs` do not race on the default logger (they do not:
`slog`'s default is safe for concurrent use). The classic mistake to avoid is
`m.Run()` without `os.Exit(code)` — that would green-wash a real failure.

## Resources

- [`log/slog`: Default, SetDefault, handlers](https://pkg.go.dev/log/slog) — the process-default logger and how `SetDefault` mutates it.
- [`testing`: Main / TestMain](https://pkg.go.dev/testing#hdr-Main) — the runner contract and `os.Exit(m.Run())`.
- [`slog.HandlerOptions`](https://pkg.go.dev/log/slog#HandlerOptions) — `Level` and `ReplaceAttr`, used to level and de-timestamp handlers.

---

Back to [00-concepts.md](00-concepts.md) | Next: [02-run-wrapper-for-deferred-teardown.md](02-run-wrapper-for-deferred-teardown.md)
