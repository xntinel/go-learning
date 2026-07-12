# Exercise 10: Add structured logging and a subcommand surface

A single-shot tool becomes operable when it grows a subcommand surface and
machine-readable output. This module turns the checker into a CLI with `check` and
`version` subcommands built on independent `flag.FlagSet` instances, and emits
results as `log/slog` JSON so a log pipeline can ingest them — while keeping the
exit-code taxonomy and a testable dispatcher.

This module is fully self-contained: its own `go mod init`, all code inline, its
own tests.

## What you'll build

```text
urlcheckcli/                independent module: example.com/urlcheckcli
  go.mod                    go 1.26
  main.go                   run dispatcher, check/version subcommands, slog emit
  main_test.go              dispatcher exit codes + slog JSON round-trip
```

Files: `main.go`, `main_test.go`.
Implement: a `run(args, out, errw) int` dispatcher routing to `check` and
`version` subcommands via separate `flag.NewFlagSet` instances, an `emit` helper
that logs one `slog` JSON line with `url`/`status`/`duration`, and a `-json` flag
selecting JSON versus text output.
Test: the dispatcher (unknown subcommand and no args exit 2; routing), and `emit`
wired to a `bytes.Buffer` asserting one JSON line whose keys round-trip through
`encoding/json`.
Verify: `go test -count=1 -race ./...`; a shell harness runs the subcommands and
checks exit codes. `gofmt -l` empty.

Set up the module:

```bash
mkdir -p go-solutions/01-environment-and-tooling/01-your-first-go-program/10-structured-output-and-subcommands
cd go-solutions/01-environment-and-tooling/01-your-first-go-program/10-structured-output-and-subcommands
```

### A testable dispatcher, not an untestable main

`main` shrinks to one line: `os.Exit(run(os.Args[1:], os.Stdout, os.Stderr))`. All
the logic lives in `run`, which takes the argument slice and two `io.Writer`s and
returns an exit code — so a test drives it with a `bytes.Buffer` and asserts the
returned int, no process spawn and no global state. This is the same
adapter-versus-logic split as the exit-code policy earlier, applied to the whole
command surface: `main` is the untestable boundary, `run` is the tested core.

Each subcommand gets its own `flag.NewFlagSet`, which is how you give `check` and
`version` independent flag namespaces (`check` has `-json` and `-timeout`;
`version` has none). A shared global `flag.CommandLine` cannot express that. An
unknown subcommand, or no subcommand at all, is a usage error and returns exit 2.
The flag sets use `flag.ContinueOnError` so a parse failure returns an error `run`
can turn into exit 2, instead of the default behavior of calling `os.Exit` from
inside `Parse` (which would be untestable).

### slog for machine-readable output

`log/slog` is the structured logger in the standard library. `slog.NewJSONHandler`
writes one JSON object per record; `slog.NewTextHandler` writes `key=value` text.
The `-json` flag picks the handler, so the same `emit` call produces human output
interactively and ingestible JSON in automation. `emit` attaches typed attributes
with `slog.String`, `slog.Int`, and `slog.Duration`, which land as top-level keys
`url`, `status`, and `duration` in the JSON object. Because `emit` takes a
`*slog.Logger`, a test points it at a `bytes.Buffer` and inspects the exact line.
`slog.Duration` serializes as an integer nanosecond count in JSON, which is the
detail the test asserts against.

Create `main.go`:

```go
package main

import (
	"context"
	"flag"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"os"
	"runtime/debug"
	"time"
)

// version is overridable at link time: -ldflags '-X main.version=1.2.3'.
var version = "dev"

// Result is one health-check outcome.
type Result struct {
	URL        string
	StatusCode int
	Duration   time.Duration
}

func main() {
	os.Exit(run(os.Args[1:], os.Stdout, os.Stderr))
}

// run dispatches to a subcommand and returns the process exit code.
func run(args []string, out, errw io.Writer) int {
	if len(args) == 0 {
		fmt.Fprintln(errw, "usage: urlcheck <check|version> [flags]")
		return 2
	}
	switch args[0] {
	case "version":
		return runVersion(args[1:], out)
	case "check":
		return runCheck(args[1:], out, errw)
	default:
		fmt.Fprintf(errw, "unknown subcommand %q\n", args[0])
		return 2
	}
}

func runVersion(args []string, out io.Writer) int {
	fs := flag.NewFlagSet("version", flag.ContinueOnError)
	fs.SetOutput(io.Discard)
	if err := fs.Parse(args); err != nil {
		return 2
	}
	fmt.Fprintln(out, versionString())
	return 0
}

func runCheck(args []string, out, errw io.Writer) int {
	fs := flag.NewFlagSet("check", flag.ContinueOnError)
	fs.SetOutput(errw)
	jsonOut := fs.Bool("json", false, "emit one JSON line per result")
	timeout := fs.Duration("timeout", 2*time.Second, "request timeout")
	if err := fs.Parse(args); err != nil {
		return 2
	}
	if fs.NArg() != 1 {
		fmt.Fprintln(errw, "usage: urlcheck check [-json] [-timeout d] URL")
		return 2
	}

	ctx, cancel := context.WithTimeout(context.Background(), *timeout)
	defer cancel()

	logger := newLogger(*jsonOut, out)
	res, err := checkURL(ctx, http.DefaultClient, fs.Arg(0))
	if err != nil {
		logger.Error("check failed", slog.String("url", fs.Arg(0)), slog.String("err", err.Error()))
		return 1
	}
	emit(logger, res)
	if res.StatusCode >= 500 {
		return 1
	}
	return 0
}

// newLogger selects JSON or text output.
func newLogger(jsonOut bool, w io.Writer) *slog.Logger {
	if jsonOut {
		return slog.New(slog.NewJSONHandler(w, nil))
	}
	return slog.New(slog.NewTextHandler(w, nil))
}

// emit logs one structured record for a successful check.
func emit(l *slog.Logger, r Result) {
	l.Info("checked",
		slog.String("url", r.URL),
		slog.Int("status", r.StatusCode),
		slog.Duration("duration", r.Duration),
	)
}

func checkURL(ctx context.Context, client *http.Client, rawURL string) (Result, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, rawURL, nil)
	if err != nil {
		return Result{}, fmt.Errorf("new request %q: %w", rawURL, err)
	}
	start := time.Now()
	resp, err := client.Do(req)
	if err != nil {
		return Result{}, fmt.Errorf("get %s: %w", rawURL, err)
	}
	defer resp.Body.Close()
	_, _ = io.Copy(io.Discard, resp.Body)
	return Result{URL: rawURL, StatusCode: resp.StatusCode, Duration: time.Since(start)}, nil
}

func versionString() string {
	if version != "dev" {
		return version
	}
	if info, ok := debug.ReadBuildInfo(); ok {
		if v := info.Main.Version; v != "" && v != "(devel)" {
			return v
		}
	}
	return version
}
```

### Running the command

The command is its own demo: the artifact is the CLI, so you run the subcommands
rather than a separate `cmd/demo`. Build and exercise the surface:

```bash
go build -o urlcheck .
./urlcheck version ; echo "version exit: $?"
./urlcheck bogus ; echo "bogus exit: $?"
```

Expected output (outside a VCS checkout, no ldflags):

```
dev
version exit: 0
unknown subcommand "bogus"
bogus exit: 2
```

Against a live URL, `./urlcheck check -json https://go.dev` prints one JSON line
with `url`, `status`, and `duration` keys and exits 0; because the status and
duration are real network observations, that output is not baked into a test.

### Tests

The dispatcher tests assert exit codes and routing with buffers, no process. The
`emit` test wires a JSON logger to a `bytes.Buffer` and round-trips the line
through `encoding/json`, asserting the `url`, `status`, and `duration` keys are
present with the right values.

Create `main_test.go`:

```go
package main

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"os"
	"strings"
	"testing"
	"time"
)

func TestRunDispatch(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name string
		args []string
		want int
	}{
		{name: "no args is usage error", args: nil, want: 2},
		{name: "unknown subcommand", args: []string{"frobnicate"}, want: 2},
		{name: "version ok", args: []string{"version"}, want: 0},
		{name: "check without url", args: []string{"check"}, want: 2},
		{name: "check bad flag", args: []string{"check", "-nope"}, want: 2},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			var out, errw bytes.Buffer
			if got := run(tc.args, &out, &errw); got != tc.want {
				t.Fatalf("run(%v) = %d, want %d (stderr: %q)", tc.args, got, tc.want, errw.String())
			}
		})
	}
}

func TestVersionSubcommandPrints(t *testing.T) {
	t.Parallel()
	var out bytes.Buffer
	if code := run([]string{"version"}, &out, &bytes.Buffer{}); code != 0 {
		t.Fatalf("version exit = %d, want 0", code)
	}
	if strings.TrimSpace(out.String()) == "" {
		t.Fatal("version printed nothing")
	}
}

func TestEmitJSON(t *testing.T) {
	t.Parallel()
	var buf bytes.Buffer
	logger := slog.New(slog.NewJSONHandler(&buf, nil))

	emit(logger, Result{URL: "https://go.dev", StatusCode: 200, Duration: 5 * time.Millisecond})

	line := strings.TrimSpace(buf.String())
	if strings.Contains(line, "\n") {
		t.Fatalf("expected one JSON line, got %q", line)
	}
	var m map[string]any
	if err := json.Unmarshal([]byte(line), &m); err != nil {
		t.Fatalf("emitted line is not valid JSON: %v", err)
	}
	if m["url"] != "https://go.dev" {
		t.Fatalf("url = %v, want https://go.dev", m["url"])
	}
	if m["status"] != float64(200) {
		t.Fatalf("status = %v, want 200", m["status"])
	}
	if m["duration"] != float64(5*time.Millisecond) {
		t.Fatalf("duration = %v, want %v ns", m["duration"], float64(5*time.Millisecond))
	}
}

func Example_unknownSubcommand() {
	code := run([]string{"nope"}, io.Discard, os.Stdout)
	fmt.Println("exit", code)
	// Output:
	// unknown subcommand "nope"
	// exit 2
}
```

The `Example_unknownSubcommand` example uses `fmt`, `io`, and `os`, which is why
they are in the test file's import block. A shell harness confirms the built
command's exit codes:

```bash
go build -o urlcheck .
./urlcheck version ; echo "version exit: $?"      # exit 0
./urlcheck bogus ; echo "bogus exit: $?"          # exit 2, "unknown subcommand"
./urlcheck check -json https://go.dev             # one JSON line, exit 0 (live network)
```

## Review

The command is operable when its surface is testable: `run` returns an exit code
from a buffer-driven call, each subcommand owns its flag namespace via
`flag.NewFlagSet`, and `emit` produces one structured JSON line whose keys are
asserted by a round-trip rather than a substring match. The exit-code taxonomy
survives the split — no subcommand and unknown subcommand are exit 2, a `check`
with a missing URL or a bad flag is exit 2, a `5xx` is exit 1 — and every one of
those is a table row in `TestRunDispatch`.

The traps: do not share one global `flag.CommandLine` across subcommands — each
needs `flag.NewFlagSet` so `check`'s `-json` does not bleed into `version`. Do not
assert on `slog` output with `strings.Contains`; parse the JSON and check the keys,
because attribute order and formatting are not part of the contract. And keep
`main` a one-liner delegating to `run`; the moment a decision lives in `main`, it
has left the reach of the tests.

## Resources

- [flag.NewFlagSet](https://pkg.go.dev/flag#NewFlagSet) — independent flag namespaces for subcommands.
- [log/slog](https://pkg.go.dev/log/slog) — `NewJSONHandler`, `NewTextHandler`, and typed attributes.
- [slog.JSONHandler](https://pkg.go.dev/log/slog#JSONHandler) — the one-object-per-record JSON format.
- [Structured Logging with slog](https://go.dev/blog/slog) — the design and usage of the standard structured logger.

---

Back to [00-concepts.md](00-concepts.md) | Previous: [09-vet-and-static-analysis-gate.md](09-vet-and-static-analysis-gate.md) | Next: [../02-go-modules-and-dependencies/00-concepts.md](../02-go-modules-and-dependencies/00-concepts.md)
