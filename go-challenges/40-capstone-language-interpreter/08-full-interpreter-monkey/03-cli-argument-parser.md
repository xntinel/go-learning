# Exercise 3: CLI Argument Parser

A shell depends on the interpreter's exit codes to decide whether a pipeline of `monkey` invocations succeeded, so the argument parser carries a contract far heavier than its size suggests: usage errors must exit 2, runtime errors 1, success 0, and that three-way split must be exactly right. This exercise builds the dispatch layer as a package with no dependency on any interpreter internal, so the whole contract — parse a string slice into a typed config, and map each error class to its exit code — can be tested as a pure function without standing up a lexer.

This module is fully self-contained. It depends on nothing but the standard library and ships its own demo and tests. Nothing here imports any other exercise.

## What you'll build

```text
cli/
  parse.go            Command, Config, Parse, ExitCode, sentinel errors
  parse_test.go       per-subcommand parsing, flag handling, exit-code mapping
cmd/
  demo/
    main.go           parse os.Args and print the resolved config
```

- Files: `cli/parse.go`, `cli/parse_test.go`, `cmd/demo/main.go`.
- Implement: `Command` with its subcommand constants, `Config`, `Parse`, `ExitCode`, and the `ErrNoSubcommand` / `ErrUnknownCommand` / `ErrNoFile` sentinels.
- Test: `parse_test.go` covers each subcommand, the `--profile` flag, the file-required cases, and the three exit-code classes.
- Verify: `go test -race ./...`

Set up the module:

```bash
mkdir -p go-solutions/40-capstone-language-interpreter/08-full-interpreter-monkey/03-cli-argument-parser/cli go-solutions/40-capstone-language-interpreter/08-full-interpreter-monkey/03-cli-argument-parser/cmd/demo
cd go-solutions/40-capstone-language-interpreter/08-full-interpreter-monkey/03-cli-argument-parser
```

### Parsing as a pure function, and why exit codes are the point

`Parse` takes the arguments after the binary name and returns either a `Config` or one of three sentinel errors, each wrapped so a caller can match it with `errors.Is`. Keeping it a pure function over a `[]string` — no reading of `os.Args` inside, no file system, no interpreter — is what makes it trivially testable: every case is one slice in and one config-or-error out. The binary's `main` reads `os.Args[1:]` and hands the slice to `Parse`; the test hands it literals.

The exit codes are the real contract. `ExitCode` maps the three usage errors to 2, every other error to 1, and nil to 0. That distinction is not cosmetic: a shell script that chains `monkey run a.mk && monkey run b.mk` needs to tell "the script failed at runtime" (exit 1) apart from "you invoked the tool wrong" (exit 2), and a CI pipeline keys retries and error messages off exactly that split. Encoding it with sentinel errors and `errors.Is` means the mapping survives wrapping: a caller can add context with `fmt.Errorf("...: %w", ErrNoFile)` and `ExitCode` still returns 2.

The subcommand switch is also a small security boundary. The default case must reject an unknown subcommand with `ErrUnknownCommand` rather than do anything clever with the string — never treat a user-supplied argument as something to execute. File-bearing subcommands require their file argument up front and scan the remaining arguments for `--profile`; `repl` takes no file and no flag. That is the entire grammar, and its smallness is the point: a contract a shell relies on should be small enough to read in one sitting and test exhaustively.

Create `cli/parse.go`:

```go
// Package cli parses the monkey interpreter's command-line arguments.
// It is intentionally free of the interpreter's internal packages so that
// the argument-parsing contract can be tested in isolation.
package cli

import (
	"errors"
	"fmt"
)

// ErrNoSubcommand is returned when os.Args[1:] is empty.
var ErrNoSubcommand = errors.New("no subcommand given")

// ErrUnknownCommand is returned when the first argument is not a known subcommand.
var ErrUnknownCommand = errors.New("unknown subcommand")

// ErrNoFile is returned when a file argument is required but absent.
var ErrNoFile = errors.New("file argument required")

// Command names a monkey CLI subcommand.
type Command string

const (
	CmdRun    Command = "run"
	CmdREPL   Command = "repl"
	CmdFmt    Command = "fmt"
	CmdAST    Command = "ast"
	CmdTokens Command = "tokens"
	CmdTest   Command = "test"
)

// Config holds the parsed state of a monkey invocation.
type Config struct {
	Command Command
	File    string // non-empty for file-bearing subcommands
	Profile bool   // --profile flag; meaningful only for CmdRun
}

// Parse parses the arguments that follow the binary name (os.Args[1:]).
// It returns ErrNoSubcommand, ErrUnknownCommand, or ErrNoFile, each
// wrapped with fmt.Errorf("%w", ...) so callers can use errors.Is.
func Parse(args []string) (Config, error) {
	if len(args) == 0 {
		return Config{}, ErrNoSubcommand
	}
	cmd := Command(args[0])
	switch cmd {
	case CmdRun, CmdFmt, CmdAST, CmdTokens, CmdTest:
		if len(args) < 2 {
			return Config{}, fmt.Errorf("%w for %s", ErrNoFile, cmd)
		}
		cfg := Config{Command: cmd, File: args[1]}
		for _, a := range args[2:] {
			if a == "--profile" {
				cfg.Profile = true
			}
		}
		return cfg, nil
	case CmdREPL:
		return Config{Command: CmdREPL}, nil
	default:
		return Config{}, fmt.Errorf("%w: %q", ErrUnknownCommand, cmd)
	}
}

// ExitCode returns the process exit code appropriate for err.
// Usage errors (no subcommand, unknown command, missing file) return 2.
// Other errors return 1. A nil error returns 0.
func ExitCode(err error) int {
	if err == nil {
		return 0
	}
	if errors.Is(err, ErrNoSubcommand) ||
		errors.Is(err, ErrUnknownCommand) ||
		errors.Is(err, ErrNoFile) {
		return 2
	}
	return 1
}
```

### The runnable demo

The demo is the real entry point in miniature: it reads `os.Args[1:]`, runs `Parse`, and on error prints a usage line to stderr and exits with the mapped code. On success it prints the resolved config so you can see the flag and file fields populated. Run it with and without arguments to watch the exit code change.

Create `cmd/demo/main.go`:

```go
// cmd/demo exercises the cli package. Run with:
//
//	go run ./cmd/demo run hello.mk --profile
package main

import (
	"fmt"
	"os"

	"example.com/cli-argument-parser/cli"
)

func main() {
	cfg, err := cli.Parse(os.Args[1:])
	if err != nil {
		fmt.Fprintf(os.Stderr, "monkey: %v\n\nUsage: monkey <run|repl|fmt|ast|tokens|test> [file] [--profile]\n", err)
		os.Exit(cli.ExitCode(err))
	}
	fmt.Printf("command=%s file=%s profile=%t\n", cfg.Command, cfg.File, cfg.Profile)
}
```

Run it:

```bash
go run ./cmd/demo run hello.mk --profile
```

Expected output:

```
command=run file=hello.mk profile=true
```

### Tests

The tests pin every branch of the grammar and every exit-code class. The single-case tests check the error sentinels and the flag in isolation; the table test sweeps the full subcommand matrix in one place, asserting both the success rows and the three error rows. `TestParseASTRequiresFile` guards the easy-to-miss case that `ast` with no file is a usage error, not a panic. The exit-code tests confirm the mapping survives wrapping by passing a wrapped sentinel through `ExitCode`.

Create `cli/parse_test.go`:

```go
package cli

import (
	"errors"
	"fmt"
	"testing"
)

func TestParseNoArgs(t *testing.T) {
	t.Parallel()

	_, err := Parse(nil)
	if !errors.Is(err, ErrNoSubcommand) {
		t.Fatalf("err = %v, want ErrNoSubcommand", err)
	}
}

func TestParseUnknownCommand(t *testing.T) {
	t.Parallel()

	_, err := Parse([]string{"exec"})
	if !errors.Is(err, ErrUnknownCommand) {
		t.Fatalf("err = %v, want ErrUnknownCommand", err)
	}
}

func TestParseRunRequiresFile(t *testing.T) {
	t.Parallel()

	_, err := Parse([]string{"run"})
	if !errors.Is(err, ErrNoFile) {
		t.Fatalf("err = %v, want ErrNoFile", err)
	}
}

func TestParseASTRequiresFile(t *testing.T) {
	t.Parallel()

	_, err := Parse([]string{"ast"})
	if !errors.Is(err, ErrNoFile) {
		t.Fatalf("err = %v, want ErrNoFile", err)
	}
}

func TestParseProfileFlagRun(t *testing.T) {
	t.Parallel()

	cfg, err := Parse([]string{"run", "main.mk", "--profile"})
	if err != nil {
		t.Fatal(err)
	}
	if cfg.Command != CmdRun || cfg.File != "main.mk" || !cfg.Profile {
		t.Fatalf("cfg = %+v", cfg)
	}
}

func TestParseProfileFlagAbsent(t *testing.T) {
	t.Parallel()

	cfg, err := Parse([]string{"run", "main.mk"})
	if err != nil {
		t.Fatal(err)
	}
	if cfg.Profile {
		t.Fatalf("Profile should be false without --profile flag")
	}
}

func TestParseREPLNeedsNoFile(t *testing.T) {
	t.Parallel()

	cfg, err := Parse([]string{"repl"})
	if err != nil {
		t.Fatal(err)
	}
	if cfg.Command != CmdREPL || cfg.File != "" {
		t.Fatalf("cfg = %+v, want Command=repl File=''", cfg)
	}
}

var parseTable = []struct {
	name     string
	args     []string
	wantCmd  Command
	wantFile string
	wantErr  error
}{
	{"run basic", []string{"run", "hello.mk"}, CmdRun, "hello.mk", nil},
	{"fmt", []string{"fmt", "hello.mk"}, CmdFmt, "hello.mk", nil},
	{"ast", []string{"ast", "hello.mk"}, CmdAST, "hello.mk", nil},
	{"tokens", []string{"tokens", "hello.mk"}, CmdTokens, "hello.mk", nil},
	{"test", []string{"test", "suite.mk"}, CmdTest, "suite.mk", nil},
	{"repl", []string{"repl"}, CmdREPL, "", nil},
	{"no args", nil, "", "", ErrNoSubcommand},
	{"unknown", []string{"exec"}, "", "", ErrUnknownCommand},
	{"run no file", []string{"run"}, "", "", ErrNoFile},
	{"fmt no file", []string{"fmt"}, "", "", ErrNoFile},
}

func TestParseTable(t *testing.T) {
	t.Parallel()

	for _, tc := range parseTable {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()

			cfg, err := Parse(tc.args)
			if tc.wantErr != nil {
				if !errors.Is(err, tc.wantErr) {
					t.Fatalf("Parse(%v): err = %v, want %v", tc.args, err, tc.wantErr)
				}
				return
			}
			if err != nil {
				t.Fatalf("Parse(%v): unexpected error: %v", tc.args, err)
			}
			if cfg.Command != tc.wantCmd || cfg.File != tc.wantFile {
				t.Fatalf("Parse(%v): cfg = %+v, want Command=%s File=%s",
					tc.args, cfg, tc.wantCmd, tc.wantFile)
			}
		})
	}
}

func TestExitCodeNil(t *testing.T) {
	t.Parallel()

	if got := ExitCode(nil); got != 0 {
		t.Fatalf("ExitCode(nil) = %d, want 0", got)
	}
}

func TestExitCodeUsageErrors(t *testing.T) {
	t.Parallel()

	for _, err := range []error{ErrNoSubcommand, ErrUnknownCommand, ErrNoFile} {
		if got := ExitCode(fmt.Errorf("wrapped: %w", err)); got != 2 {
			t.Errorf("ExitCode(%v) = %d, want 2", err, got)
		}
	}
}

func ExampleParse() {
	cfg, err := Parse([]string{"run", "hello.mk", "--profile"})
	if err != nil {
		panic(err)
	}
	fmt.Printf("command=%s file=%s profile=%t\n", cfg.Command, cfg.File, cfg.Profile)
	// Output:
	// command=run file=hello.mk profile=true
}

func ExampleExitCode() {
	fmt.Println(ExitCode(nil))
	fmt.Println(ExitCode(ErrNoSubcommand))
	// Output:
	// 0
	// 2
}
```

## Review

The parser is correct when each subcommand resolves to the right `Config`, the file-bearing subcommands reject a missing file with `ErrNoFile`, and an unknown subcommand returns `ErrUnknownCommand` rather than doing anything with the string. The exit-code mapping is the load-bearing half: usage errors exit 2, runtime errors exit 1, success exits 0, and the mapping must survive wrapping so a caller can add context with `%w` and still get the right code through `ExitCode`. The default case is a security boundary as much as a usability one — rejecting the unknown subcommand is what stops a typo from becoming command execution. Because the package touches nothing but its own types, the whole contract stays exhaustively testable as a pure function.

## Resources

- [Writing An Interpreter In Go, Thorsten Ball](https://interpreterbook.com/) — the REPL and run paths these subcommands dispatch to.
- [pkg.go.dev/os](https://pkg.go.dev/os) — `os.Args` and `os.Exit` used by the demo entry point.
- [go.dev/blog/go1.13-errors](https://go.dev/blog/go1.13-errors) — `errors.Is` and `%w` wrapping behind the exit-code mapping.

---

Back to [02-module-cache-circular-import.md](02-module-cache-circular-import.md) | Next: [04-interpreter-pipeline-dispatch.md](04-interpreter-pipeline-dispatch.md)
