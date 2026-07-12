# Exercise 8: Test Code That Calls os.Exit / log.Fatal Using the Subprocess Re-exec Technique

A CLI entrypoint that validates its config and calls `os.Exit(2)` on failure
cannot be tested in-process — calling it from a test would kill the test runner.
The documented technique is to re-exec the test binary as a child process guarded
by an environment variable, let the exiting code run in the child, and assert the
child's exit code and stderr from the parent. This is the canonical pattern and it
complements the `TestMain` lifecycle: it is how you cover the shutdown paths.

This module is fully self-contained: its own `go mod init`, CLI logic, demo, and
tests. Nothing here imports any other exercise.

## What you'll build

```text
cliexit/                       independent module: example.com/cliexit
  go.mod                       go 1.26
  config.go                    Validate (pure) and RunOrExit (calls os.Exit(2))
  cmd/
    demo/
      main.go                  runnable demo: validate a few ports
  config_test.go               unit test of Validate; subprocess tests of RunOrExit
```

Files: `config.go`, `cmd/demo/main.go`, `config_test.go`.
Implement: `Validate(port int) error` (pure, wraps `ErrInvalidPort`), and `RunOrExit(port int)` that prints to stderr and `os.Exit(2)` on invalid config.
Test: unit-test `Validate` with `errors.Is`; subprocess-test `RunOrExit` by re-execing `os.Args[0]` with an env guard and asserting `*exec.ExitError` / `ExitCode()`.
Verify: `go test -count=1 -race ./...`

### Why you cannot test os.Exit in-process

`os.Exit` terminates the process immediately. If a test calls a function that
reaches `os.Exit(2)`, the test binary itself exits with code 2 in the middle of
the run — no other tests run, and there is no result to assert. `log.Fatal` is the
same problem: it calls `os.Exit(1)` after logging. So the code path is untestable
directly. It must run in a *different* process.

### Separate the pure logic from the exiting shell

The first move is to make as much as possible testable without a subprocess. The
decision — is this config valid? — is pure and belongs in `Validate`, which
returns a wrapped sentinel error and is unit-tested normally with `errors.Is`. The
only thing that must call `os.Exit` is the thin shell `RunOrExit`, which calls
`Validate`, prints the error to stderr, and exits. Keeping the shell thin means
the subprocess test only has to prove "bad config produces exit 2 and a message on
stderr", while all the branch coverage lives in the fast in-process unit test.

### The re-exec pattern, step by step

The subprocess test uses one function that behaves two ways depending on an
environment guard:

1. When the guard `BE_CRASHER=1` is set, the test *is* the child: it calls
   `RunOrExit` with bad config, which runs `os.Exit(2)`. The child process exits 2.
2. When the guard is not set, the test is the parent: it runs
   `exec.Command(os.Args[0], "-test.run=TestRunOrExitBadConfig")` with
   `BE_CRASHER=1` added to the child's environment. `os.Args[0]` is the compiled
   test binary, and `-test.run=` selects only this test in the child, so the child
   re-enters this same function, takes the guard branch, and exits 2.

The parent then inspects the child's result: `cmd.Run()` returns a non-nil error
for a non-zero exit, which unwraps to `*exec.ExitError`; `ee.ExitCode()` is the
child's code, and the captured `stderr` buffer holds what the child printed. This
is exactly how the standard library tests its own `os.Exit` paths.

Create `config.go`:

```go
package cliexit

import (
	"errors"
	"fmt"
	"os"
)

// ErrInvalidPort is the sentinel for an out-of-range port.
var ErrInvalidPort = errors.New("port must be between 1 and 65535")

// Validate is the pure decision: it reports whether the config is usable. It is
// unit-testable with no subprocess.
func Validate(port int) error {
	if port < 1 || port > 65535 {
		return fmt.Errorf("invalid config: %w", ErrInvalidPort)
	}
	return nil
}

// RunOrExit is the thin CLI shell: on bad config it reports to stderr and exits
// with code 2, the convention for a usage/config error. On good config it
// returns and the caller proceeds.
func RunOrExit(port int) {
	if err := Validate(port); err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(2)
	}
}
```

### The runnable demo

The demo exercises the *pure* half — it cannot call `RunOrExit`, which would exit
the demo process — so it validates a few ports and prints the outcome.

Create `cmd/demo/main.go`:

```go
package main

import (
	"fmt"

	"example.com/cliexit"
)

func main() {
	for _, p := range []int{8080, -1, 70000} {
		if err := cliexit.Validate(p); err != nil {
			fmt.Printf("port %d: %v\n", p, err)
		} else {
			fmt.Printf("port %d: ok\n", p)
		}
	}
}
```

Run it:

```bash
go run ./cmd/demo
```

Expected output:

```
port 8080: ok
port -1: invalid config: port must be between 1 and 65535
port 70000: invalid config: port must be between 1 and 65535
```

### Tests

`TestValidate` is the fast unit test with `errors.Is`. `TestRunOrExitBadConfig` is
the subprocess test proving exit code 2 and a stderr message.
`TestRunOrExitGoodConfig` proves the happy path exits cleanly. Both subprocess
tests use the `BE_CRASHER` guard to split parent and child roles.

Create `config_test.go`:

```go
package cliexit

import (
	"bytes"
	"errors"
	"os"
	"os/exec"
	"strings"
	"testing"
)

func TestValidate(t *testing.T) {
	t.Parallel()
	cases := []struct {
		name    string
		port    int
		wantErr bool
	}{
		{"typical", 8080, false},
		{"low boundary", 1, false},
		{"high boundary", 65535, false},
		{"zero", 0, true},
		{"negative", -1, true},
		{"too high", 70000, true},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			err := Validate(tc.port)
			if tc.wantErr {
				if !errors.Is(err, ErrInvalidPort) {
					t.Fatalf("Validate(%d) = %v, want ErrInvalidPort", tc.port, err)
				}
			} else if err != nil {
				t.Fatalf("Validate(%d) = %v, want nil", tc.port, err)
			}
		})
	}
}

func TestRunOrExitBadConfig(t *testing.T) {
	if os.Getenv("BE_CRASHER") == "1" {
		RunOrExit(-1) // child: this calls os.Exit(2)
		return
	}
	cmd := exec.Command(os.Args[0], "-test.run=TestRunOrExitBadConfig")
	cmd.Env = append(os.Environ(), "BE_CRASHER=1")
	var stderr bytes.Buffer
	cmd.Stderr = &stderr

	err := cmd.Run()

	var ee *exec.ExitError
	if !errors.As(err, &ee) {
		t.Fatalf("child error = %v, want *exec.ExitError", err)
	}
	if ee.ExitCode() != 2 {
		t.Fatalf("child exit code = %d, want 2", ee.ExitCode())
	}
	if !strings.Contains(stderr.String(), "invalid config") {
		t.Fatalf("child stderr = %q, want it to contain %q", stderr.String(), "invalid config")
	}
}

func TestRunOrExitGoodConfig(t *testing.T) {
	if os.Getenv("BE_CRASHER") == "1" {
		RunOrExit(8080) // child: returns normally, so the child exits 0
		return
	}
	cmd := exec.Command(os.Args[0], "-test.run=TestRunOrExitGoodConfig")
	cmd.Env = append(os.Environ(), "BE_CRASHER=1")

	if err := cmd.Run(); err != nil {
		t.Fatalf("valid config should exit 0, got %v", err)
	}
}
```

## Review

The technique is correct when the pure decision (`Validate`) carries the branch
coverage in a fast in-process unit test, and the thin exiting shell (`RunOrExit`)
is covered by re-execing the test binary. The parent asserts the child's outcome
via `errors.As(err, &ee)` and `ee.ExitCode()`, and reads the captured stderr — you
cannot observe any of this if you call `RunOrExit` directly, because it would take
the parent process down with it. The guard env var (`BE_CRASHER`) is what lets one
test function play both parent and child. This pattern is the standard-library way
to test `os.Exit`/`log.Fatal`, and it is the natural partner to `TestMain`: the
lifecycle harness sets up the process, and this covers the ways it tears down.

## Resources

- [`os/exec.ExitError` / `ProcessState.ExitCode`](https://pkg.go.dev/os/exec#ExitError) — reading a child's exit code.
- [`os.Exit`](https://pkg.go.dev/os#Exit) — why the exiting path must run in a child process.
- [`exec.Command` and `Cmd.Env`](https://pkg.go.dev/os/exec#Command) — re-execing `os.Args[0]` with a guard env var.

---

Back to [00-concepts.md](00-concepts.md) | Prev: [07-env-timezone-global-restore.md](07-env-timezone-global-restore.md) | Next: [09-goroutine-leak-gate-after-run.md](09-goroutine-leak-gate-after-run.md)
