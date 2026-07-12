# Exercise 1: A Sandboxed WASI Command Module: stdin/stdout Filter

A command module is a Unix filter compiled to Wasm: it reads `os.Stdin`, writes
`os.Stdout`, takes options from `os.Args` and the environment, and terminates with
an exit code. This exercise writes that guest as ordinary Go behind a `wasip1`
build tag, and builds the `wazero` host runner that wires stdin/stdout/args/env
and — the part everyone gets wrong — correctly interprets the WASI exit code.

This module is fully self-contained. It begins with its own `go mod init`, keeps
the redaction transform in a plain package the guest and the tests both use, and
ships its own demo and tests.

## What you'll build

```text
wasifilter/                 independent module: example.com/wasifilter
  go.mod                    go 1.24; requires github.com/tetratelabs/wazero
  redact.go                 pure transform: Mode, ParseMode, RedactLine (no wazero, no build tag)
  runner.go                 Runner over wazero.Runtime; Run; classifyExit; ErrInstantiate
  guest/
    main.go                 //go:build wasip1 — the guest: reads stdin/args/env, exits with a code
  cmd/
    demo/
      main.go               host: load filter.wasm, run it against buffers, print exit code
  runner_test.go            table tests for RedactLine and classifyExit; Example
```

- Files: `redact.go`, `runner.go`, `guest/main.go`, `cmd/demo/main.go`, `runner_test.go`.
- Implement: a pure `RedactLine(mode, secret, line)` transform, a `Runner` that instantiates a WASI command with `WithArgs`/`WithStdin`/`WithStdout`/`WithStderr`/`WithEnv`, and a `classifyExit` that maps the `InstantiateWithConfig` error to a `(code, error)` pair.
- Test: table-driven `RedactLine` cases; table-driven `classifyExit` over `*sys.ExitError` values and a genuine failure asserted with `errors.Is`; an `Example` with `// Output:`.
- Verify: build the guest, then `go run ./cmd/demo`.

Set up the module. `//go:wasmexport` is not used here, but the WASI target and
`wazero` still require a recent toolchain:

```bash
mkdir -p go-solutions/53-wasm-and-extensibility/04-tinygo-wasi-guest-modules/01-wasi-command-filter/guest go-solutions/53-wasm-and-extensibility/04-tinygo-wasi-guest-modules/01-wasi-command-filter/cmd/demo
cd go-solutions/53-wasm-and-extensibility/04-tinygo-wasi-guest-modules/01-wasi-command-filter
go mod edit -go=1.24
go get github.com/tetratelabs/wazero@latest
```

### Keep the transform pure and out of the guest

The redaction logic has nothing to do with Wasm — it is a string transform. If it
lives inside the guest's `main`, you can only test it by compiling to `wasip1` and
running the module, which is slow and not something a plain `go test` can do. So we
put it in `redact.go` in package `wasifilter`, with no build tag and no `wazero`
import, and both the guest and the unit tests call it. This is the single most
useful structural habit for guest authoring: the guest's `main` is a thin
I/O shell around pure, ordinary-Go logic you can test directly.

`RedactLine` supports two modes. `ModeMask` replaces every occurrence of a literal
secret token with `***` and keeps the line; `ModeDrop` drops any line containing
the secret. A literal token (not a regexp) is a deliberate choice: it keeps the
guest within TinyGo's supported surface, since TinyGo's `regexp` support is
partial. `ParseMode` turns the CLI string into a `Mode` and returns a wrapped
`ErrUnknownMode` for anything else, so the guest can map a bad flag to a distinct
exit code.

Create `redact.go`:

```go
package wasifilter

import (
	"errors"
	"fmt"
	"strings"
)

// ErrUnknownMode is returned by ParseMode for an unrecognized mode string.
var ErrUnknownMode = errors.New("unknown mode")

// Mode selects how RedactLine treats a line that contains the secret.
type Mode int

const (
	// ModeMask replaces each occurrence of the secret with a fixed mask.
	ModeMask Mode = iota
	// ModeDrop removes any line that contains the secret.
	ModeDrop
)

const mask = "***"

// ParseMode maps a CLI string to a Mode, wrapping ErrUnknownMode on failure.
func ParseMode(s string) (Mode, error) {
	switch s {
	case "mask":
		return ModeMask, nil
	case "drop":
		return ModeDrop, nil
	default:
		return 0, fmt.Errorf("%w: %q", ErrUnknownMode, s)
	}
}

// RedactLine applies mode to line using the literal secret token. It returns the
// transformed text and whether the line should be kept. An empty secret is a
// no-op: every line is kept unchanged.
func RedactLine(mode Mode, secret, line string) (out string, keep bool) {
	if secret == "" || !strings.Contains(line, secret) {
		return line, true
	}
	switch mode {
	case ModeDrop:
		return "", false
	default: // ModeMask
		return strings.ReplaceAll(line, secret, mask), true
	}
}
```

### The guest is ordinary Go behind a build tag

The guest reads its mode from `os.Args[1]`, its secret from the `SECRET`
environment variable, and streams stdin to stdout line by line with
`bufio.Scanner` and `bufio.Writer`. It signals its result through the *exit code*,
which is how a command talks to its host: `0` means the input was clean (nothing
matched), `3` means at least one line was masked or dropped, `2` means bad
arguments, and `1` means a read error. Those distinct codes are what the host will
classify.

The `//go:build wasip1` tag matters twice. It documents that this file targets
WASI, and it keeps `go build ./...` on your development host from trying to compile
guest code that calls into the WASI runtime. When you build with
`GOOS=wasip1 GOARCH=wasm`, the tag is satisfied automatically.

Create `guest/main.go`:

```go
//go:build wasip1

// Command filter is a WASI guest: a line-oriented log redactor. Build it with
// GOOS=wasip1 GOARCH=wasm go build -o filter.wasm ./guest
// or tinygo build -o filter.wasm -target=wasip1 ./guest
package main

import (
	"bufio"
	"fmt"
	"os"

	"example.com/wasifilter"
)

func main() {
	if len(os.Args) < 2 {
		fmt.Fprintln(os.Stderr, "usage: filter <mask|drop>")
		os.Exit(2)
	}
	mode, err := wasifilter.ParseMode(os.Args[1])
	if err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(2)
	}
	secret := os.Getenv("SECRET")

	in := bufio.NewScanner(os.Stdin)
	out := bufio.NewWriter(os.Stdout)
	redacted := 0
	for in.Scan() {
		line := in.Text()
		text, keep := wasifilter.RedactLine(mode, secret, line)
		if !keep {
			redacted++
			continue
		}
		if text != line {
			redacted++
		}
		fmt.Fprintln(out, text)
	}
	if err := in.Err(); err != nil {
		fmt.Fprintln(os.Stderr, "read:", err)
		os.Exit(1)
	}
	if err := out.Flush(); err != nil {
		fmt.Fprintln(os.Stderr, "flush:", err)
		os.Exit(1)
	}
	if redacted > 0 {
		os.Exit(3)
	}
}
```

### The host runner, and reading the exit code correctly

`Runner` owns a long-lived `wazero.Runtime` with the WASI host module
instantiated once. `Run` builds a fresh `ModuleConfig` per invocation — wiring the
caller's args, env, and streams — and calls `InstantiateWithConfig`, which for a
command module runs `_start` (and thus `main`) to completion.

The subtle part is `classifyExit`. A WASI command terminates via `proc_exit`, so
even a clean run returns a `*sys.ExitError` rather than `nil`. `classifyExit`
therefore does not treat a non-nil error as failure. It uses `errors.As` to pull
out a `*sys.ExitError` and returns its `ExitCode()` (0 included) as a normal
outcome; only an error that is *not* an `ExitError` is a genuine instantiation or
validation failure, which it wraps with `ErrInstantiate` so callers can match it
with `errors.Is`. This function is pure over its error argument, which is what lets
the test drive it with constructed `sys.NewExitError` values instead of a live
module.

Create `runner.go`:

```go
package wasifilter

import (
	"context"
	"errors"
	"fmt"
	"io"

	"github.com/tetratelabs/wazero"
	"github.com/tetratelabs/wazero/imports/wasi_snapshot_preview1"
	"github.com/tetratelabs/wazero/sys"
)

// ErrInstantiate wraps a genuine instantiation or validation failure — a host
// error that is not a WASI process exit.
var ErrInstantiate = errors.New("guest instantiation failed")

// Runner holds a long-lived runtime with WASI wired in, ready to run command
// guests. Construct one per process and reuse it.
type Runner struct {
	rt wazero.Runtime
}

// NewRunner builds a runtime and instantiates the wasi_snapshot_preview1 host
// module, which every WASI guest needs to link.
func NewRunner(ctx context.Context) *Runner {
	rt := wazero.NewRuntime(ctx)
	wasi_snapshot_preview1.MustInstantiate(ctx, rt)
	return &Runner{rt: rt}
}

// Close releases the runtime and everything it created.
func (r *Runner) Close(ctx context.Context) error { return r.rt.Close(ctx) }

// RunInput is one command invocation. Args[0] is the program name by convention;
// unset streams default to WASI's discard/EOF behavior.
type RunInput struct {
	Wasm   []byte
	Args   []string
	Env    map[string]string
	Stdin  io.Reader
	Stdout io.Writer
	Stderr io.Writer
}

// Run instantiates the command guest with the given capabilities and returns its
// WASI exit code. A clean run yields (0, nil); a real host failure yields
// (-1, error) wrapping ErrInstantiate.
func (r *Runner) Run(ctx context.Context, in RunInput) (int, error) {
	cfg := wazero.NewModuleConfig().
		WithArgs(in.Args...).
		WithStdin(in.Stdin).
		WithStdout(in.Stdout).
		WithStderr(in.Stderr)
	for k, v := range in.Env {
		cfg = cfg.WithEnv(k, v)
	}
	_, err := r.rt.InstantiateWithConfig(ctx, in.Wasm, cfg)
	return classifyExit(err)
}

// classifyExit maps the error from InstantiateWithConfig to a WASI exit code.
// A command always exits via proc_exit, so success surfaces as *sys.ExitError
// with code 0 — not a nil error. Any non-ExitError is a real failure.
func classifyExit(err error) (int, error) {
	if err == nil {
		return 0, nil
	}
	var exit *sys.ExitError
	if errors.As(err, &exit) {
		return int(exit.ExitCode()), nil
	}
	return -1, fmt.Errorf("%w: %w", ErrInstantiate, err)
}
```

### The runnable demo

The demo loads the compiled `filter.wasm`, feeds it three log lines through an
in-memory buffer with the secret token supplied as `SECRET`, captures stdout, and
prints the exit code. Because two of the three lines contain the token, the guest
masks them and exits `3`.

Create `cmd/demo/main.go`:

```go
package main

import (
	"bytes"
	"context"
	"fmt"
	"os"

	"example.com/wasifilter"
)

func main() {
	ctx := context.Background()
	wasm, err := os.ReadFile("filter.wasm")
	if err != nil {
		fmt.Fprintln(os.Stderr, "read filter.wasm (build the guest first):", err)
		os.Exit(1)
	}

	r := wasifilter.NewRunner(ctx)
	defer r.Close(ctx)

	input := "user=alice token=sk-SECRET-123 ok\n" +
		"user=bob token=none\n" +
		"login token=sk-SECRET-123\n"

	var out bytes.Buffer
	code, err := r.Run(ctx, wasifilter.RunInput{
		Wasm:   wasm,
		Args:   []string{"filter", "mask"},
		Env:    map[string]string{"SECRET": "sk-SECRET-123"},
		Stdin:  bytes.NewBufferString(input),
		Stdout: &out,
		Stderr: os.Stderr,
	})
	if err != nil {
		fmt.Fprintln(os.Stderr, "run:", err)
		os.Exit(1)
	}
	fmt.Print(out.String())
	fmt.Println("exit code:", code)
}
```

Build the guest, then run the host:

```bash
GOOS=wasip1 GOARCH=wasm go build -o filter.wasm ./guest
# or, for a smaller, faster-loading artifact:
tinygo build -o filter.wasm -target=wasip1 ./guest
go run ./cmd/demo
```

Expected output:

```
user=alice token=*** ok
user=bob token=none
login token=***
exit code: 3
```

### Tests

The tests exercise the two pieces that are pure Go, and therefore testable without
running a module. `TestRedactLine` covers mask, drop, no-match, and empty-secret
cases. `TestClassifyExit` builds `*sys.ExitError` values with `sys.NewExitError`
and asserts that code 0 is a success (not an error), that non-zero codes pass
through, and that a non-ExitError is wrapped with `ErrInstantiate` — matched via
`errors.Is`. The `Example` pins the mask transform's output.

Create `runner_test.go`:

```go
package wasifilter

import (
	"errors"
	"fmt"
	"testing"

	"github.com/tetratelabs/wazero/sys"
)

func TestRedactLine(t *testing.T) {
	t.Parallel()
	const secret = "sk-SECRET-123"
	tests := []struct {
		name     string
		mode     Mode
		secret   string
		line     string
		wantOut  string
		wantKeep bool
	}{
		{"mask hit", ModeMask, secret, "token=sk-SECRET-123 ok", "token=*** ok", true},
		{"mask twice", ModeMask, secret, "sk-SECRET-123 sk-SECRET-123", "*** ***", true},
		{"mask miss", ModeMask, secret, "token=none", "token=none", true},
		{"drop hit", ModeDrop, secret, "login sk-SECRET-123", "", false},
		{"drop miss", ModeDrop, secret, "login ok", "login ok", true},
		{"empty secret", ModeMask, "", "anything", "anything", true},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			out, keep := RedactLine(tc.mode, tc.secret, tc.line)
			if out != tc.wantOut || keep != tc.wantKeep {
				t.Fatalf("RedactLine(%v, %q, %q) = (%q, %v), want (%q, %v)",
					tc.mode, tc.secret, tc.line, out, keep, tc.wantOut, tc.wantKeep)
			}
		})
	}
}

func TestClassifyExit(t *testing.T) {
	t.Parallel()
	boom := errors.New("boom")
	tests := []struct {
		name     string
		err      error
		wantCode int
		wantErr  error
	}{
		{"clean nil", nil, 0, nil},
		{"exit zero", sys.NewExitError(0), 0, nil},
		{"exit three", sys.NewExitError(3), 3, nil},
		{"bad args", sys.NewExitError(2), 2, nil},
		{"real failure", boom, -1, ErrInstantiate},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			code, err := classifyExit(tc.err)
			if code != tc.wantCode {
				t.Fatalf("classifyExit(%v) code = %d, want %d", tc.err, code, tc.wantCode)
			}
			switch {
			case tc.wantErr == nil && err != nil:
				t.Fatalf("classifyExit(%v) err = %v, want nil", tc.err, err)
			case tc.wantErr != nil && !errors.Is(err, tc.wantErr):
				t.Fatalf("classifyExit(%v) err = %v, want wrap of %v", tc.err, err, tc.wantErr)
			}
		})
	}
}

func ExampleRedactLine() {
	out, keep := RedactLine(ModeMask, "sk-SECRET-123", "token=sk-SECRET-123 ok")
	fmt.Println(out, keep)
	// Output: token=*** ok true
}
```

## Review

The runner is correct when a successful guest run is not treated as an error. The
single most common defect in WASI host code is
`if _, err := rt.InstantiateWithConfig(...); err != nil { return err }`, which
reports every clean command run as a failure because the run always returns a
`*sys.ExitError` with code 0. Confirm the fix by pointing `TestClassifyExit` at
`sys.NewExitError(0)` and seeing `(0, nil)`; if it instead returns an error, the
exit-code path is wrong.

The sandbox is correct when the guest sees only what you granted. Because
`wazero` defaults to no args, no env, discarded streams, and EOF on stdin, the
guest's `os.Getenv("SECRET")` returns `""` (and every line is kept) unless the
runner passes `WithEnv`, and `os.Args` is empty unless it passes `WithArgs`. If a
redaction "silently does nothing," the first suspect is a missing capability on
the host side, not a bug in the guest. Keeping `RedactLine` and `ParseMode` pure
and out of the guest is what makes the transform verifiable with an ordinary
`go test`; the guest's `main` is only the thin stdin/stdout/exit-code shell around
it. Note the exit-code contract in the demo: two masked lines produce exit `3`,
and the host reads that as a normal outcome, not a failure.

## Resources

- [Using WASI - TinyGo documentation](https://tinygo.org/docs/guides/webassembly/wasi/) — building a `wasip1` guest and the command lifecycle.
- [wazero ModuleConfig](https://pkg.go.dev/github.com/tetratelabs/wazero#ModuleConfig) — `WithArgs`, `WithStdin`/`WithStdout`/`WithStderr`, `WithEnv`, and their default-deny behavior.
- [wazero/sys ExitError](https://pkg.go.dev/github.com/tetratelabs/wazero/sys#ExitError) — `ExitCode` and why a clean command run surfaces as an ExitError.
- [imports/wasi_snapshot_preview1](https://pkg.go.dev/github.com/tetratelabs/wazero/imports/wasi_snapshot_preview1) — `MustInstantiate`, the host WASI module a guest links against.

---

Back to [00-concepts.md](00-concepts.md) | Next: [02-wasi-reactor-wasmexport.md](02-wasi-reactor-wasmexport.md)
