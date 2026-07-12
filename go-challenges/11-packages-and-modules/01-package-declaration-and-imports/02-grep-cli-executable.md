# Exercise 2: The grep CLI Executable with a Testable run() Seam

A `package main` is where a service meets the operating system: `os.Args`,
`os.Stdin`, exit codes. The problem is that `main()` itself is nearly impossible
to unit-test — it reads process globals and calls `os.Exit`, which kills the test
binary. The senior pattern is a thin `main` over a `run(...)` function that takes
its inputs as parameters and *returns* an exit code, so the whole command is
table-testable.

This module is self-contained: it bundles its own copy of the grep library and
ships its own tests. Nothing here imports another exercise.

## What you'll build

```text
grepcli/                           module: example.com/grepcli
  go.mod
  internal/grep/grep.go            bundled matcher library (package grep)
  cmd/pkgsimports/main.go          package main: main() over run(args, stdin, stdout, stderr) int
  cmd/pkgsimports/main_test.go     drives run() with bytes.Buffer, asserting exit codes
```

- Files: `internal/grep/grep.go`, `cmd/pkgsimports/main.go`, `cmd/pkgsimports/main_test.go`.
- Implement: `run(args []string, stdin io.Reader, stdout, stderr io.Writer) int` with `main()` a one-line shell over it; three import groups (stdlib, third-party slot, internal).
- Test: happy path prints `1:hello world\n3:hello again\n` and returns 0; no-match returns 1 with an error on stderr; missing argument returns 2 with usage on stderr.
- Verify: `go test -count=1 -race ./...`

Set up the module:

```bash
go mod edit -go=1.26
```

### The run() seam

`main()` should do exactly one testable-irrelevant thing: translate process
globals into `run`'s parameters and translate `run`'s return value into an exit
code.

```go
func main() {
	os.Exit(run(os.Args[1:], os.Stdin, os.Stdout, os.Stderr))
}
```

Everything worth testing lives in `run`, which takes its stdin as an `io.Reader`
and its two output streams as `io.Writer`s. A test passes `bytes.Buffer`s for all
three and inspects them; it never touches the real process I/O and never calls
`os.Exit`. `run` returns an `int` exit code rather than calling `os.Exit` itself,
because `os.Exit` inside `run` would terminate the test process. This is the
single most useful refactor for making a Go CLI testable, and it costs nothing.

The exit-code mapping is a real operational contract: `0` for success, `1` for "no
match" (mirroring real `grep`, which returns 1 when nothing matched), `1` for an
I/O error, and `2` for a usage error (missing argument). Callers — shell scripts,
CI, systemd — branch on these.

### Three import groups, and the internal boundary

`main.go` imports the matcher through the module path
`example.com/grepcli/internal/grep`. It cannot use a relative import; module mode
does not support `import "./internal/grep"`. The three-group convention is stdlib,
then third-party, then this module's internal packages, separated by blank lines.
This command has no third-party dependency, so only the stdlib and internal groups
are present; the middle group would sit between them when a dependency like
`github.com/spf13/cobra` is added. The `internal/` placement means only code in
this module can import the matcher — a relative import from *outside* the module
would be rejected by the import-path resolver, not just by style.

First, the bundled library. Create `internal/grep/grep.go`:

```go
package grep

import (
	"bufio"
	"errors"
	"io"
	"strings"
)

// ErrNoMatch is returned when the scan completes without finding the substring.
var ErrNoMatch = errors.New("grep: no match")

// ErrEmptyPattern is returned when a Matcher has no substring configured.
var ErrEmptyPattern = errors.New("grep: empty pattern")

// Result is one matching line and its one-indexed position in the input.
type Result struct {
	LineNum int
	Line    string
}

// Matcher scans an io.Reader for lines containing Substr.
type Matcher struct {
	Substr string
}

// New returns a Matcher for the given substring.
func New(substr string) *Matcher {
	return &Matcher{Substr: substr}
}

// Match scans r line by line, returning every line containing Substr.
func (m *Matcher) Match(r io.Reader) ([]Result, error) {
	if m.Substr == "" {
		return nil, ErrEmptyPattern
	}
	sc := bufio.NewScanner(r)
	var out []Result
	lineNum := 0
	for sc.Scan() {
		lineNum++
		line := sc.Text()
		if strings.Contains(line, m.Substr) {
			out = append(out, Result{LineNum: lineNum, Line: line})
		}
	}
	if err := sc.Err(); err != nil {
		return nil, err
	}
	if len(out) == 0 {
		return nil, ErrNoMatch
	}
	return out, nil
}
```

Now the command. Create `cmd/pkgsimports/main.go`:

```go
package main

import (
	"errors"
	"fmt"
	"io"
	"os"

	"example.com/grepcli/internal/grep"
)

func main() {
	os.Exit(run(os.Args[1:], os.Stdin, os.Stdout, os.Stderr))
}

// run is the testable core: it reads pattern-then-stdin, writes matches to
// stdout, diagnostics to stderr, and returns a process exit code.
func run(args []string, stdin io.Reader, stdout, stderr io.Writer) int {
	if len(args) < 1 {
		fmt.Fprintln(stderr, "usage: pkgsimports PATTERN")
		return 2
	}
	results, err := grep.New(args[0]).Match(stdin)
	if err != nil {
		if errors.Is(err, grep.ErrNoMatch) {
			fmt.Fprintln(stderr, "pkgsimports: no match")
			return 1
		}
		fmt.Fprintln(stderr, "pkgsimports:", err)
		return 1
	}
	for _, r := range results {
		fmt.Fprintf(stdout, "%d:%s\n", r.LineNum, r.Line)
	}
	return 0
}
```

Run it against stdin:

```bash
go run ./cmd/pkgsimports hello <<< $'hello world\nfoo bar\nhello again'
```

Expected output:

```text
1:hello world
3:hello again
```

### Tests

Because `run` takes readers and writers and returns an `int`, the whole command
is table-testable with no process globals and no `os.Exit`. Each case supplies a
stdin buffer and inspects stdout, stderr, and the returned code.

Create `cmd/pkgsimports/main_test.go`:

```go
package main

import (
	"bytes"
	"strings"
	"testing"
)

func TestRun(t *testing.T) {
	t.Parallel()

	cases := []struct {
		name       string
		args       []string
		stdin      string
		wantCode   int
		wantStdout string
		wantErrHas string // substring expected on stderr ("" means empty)
	}{
		{
			name:       "happy path",
			args:       []string{"hello"},
			stdin:      "hello world\nfoo bar\nhello again",
			wantCode:   0,
			wantStdout: "1:hello world\n3:hello again\n",
		},
		{
			name:       "no match",
			args:       []string{"zzz"},
			stdin:      "hello world\nfoo bar",
			wantCode:   1,
			wantErrHas: "no match",
		},
		{
			name:       "missing arg",
			args:       nil,
			stdin:      "",
			wantCode:   2,
			wantErrHas: "usage:",
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			var out, errBuf bytes.Buffer
			code := run(tc.args, strings.NewReader(tc.stdin), &out, &errBuf)

			if code != tc.wantCode {
				t.Errorf("exit code = %d, want %d", code, tc.wantCode)
			}
			if out.String() != tc.wantStdout {
				t.Errorf("stdout = %q, want %q", out.String(), tc.wantStdout)
			}
			if tc.wantErrHas == "" {
				if errBuf.Len() != 0 {
					t.Errorf("stderr = %q, want empty", errBuf.String())
				}
			} else if !strings.Contains(errBuf.String(), tc.wantErrHas) {
				t.Errorf("stderr = %q, want to contain %q", errBuf.String(), tc.wantErrHas)
			}
		})
	}
}
```

## Review

The command is correct when `run` is a pure function of its four inputs and its
return code, and `main` adds nothing but the `os` glue. The design proof is the
test itself: if you could not drive `run` with `bytes.Buffer`s, the seam would be
in the wrong place. Two traps to avoid. First, do not call `os.Exit` inside `run`
— it would kill the test process; return the code and let `main` exit. Second, do
not reach for a relative import to the matcher; module mode requires the full
module path, and `internal/` means only this module may import it anyway. The
exit-code contract (0 success, 1 no-match/IO-error, 2 usage) is the part
operators actually depend on, so it is asserted explicitly, not left implicit.

## Resources

- [`os` package](https://pkg.go.dev/os#Args) — `os.Args`, `os.Stdin`, `os.Exit`.
- [Go Command Documentation: internal packages](https://pkg.go.dev/cmd/go#hdr-Internal_Directories) — the `internal/` import boundary.
- [Testable commands (Go blog: errors are values)](https://go.dev/blog/errors-are-values) — the return-a-value-not-exit discipline.

---

Back to [00-concepts.md](00-concepts.md) | Prev: [01-grep-matcher-library.md](01-grep-matcher-library.md) | Next: [03-codec-registry-blank-import.md](03-codec-registry-blank-import.md)
