# Exercise 3: Wire the command as a thin adapter

A command is just a package named `main`. The discipline that keeps it
maintainable is to make `main` glue and nothing else: parse flags, build a
context, call a real package, map the outcome to an exit code. Every decision you
care about lives in a pure, tested function.

This module is fully self-contained: its own `go mod init`, its command, its
tested package, its tests.

## What you'll build

```text
urlcheck/                   independent module: example.com/urlcheck
  go.mod                    go 1.26
  internal/check/check.go   Client, Result, ErrEmptyURL, URL (the tested package)
  cmd/urlcheck/main.go      flag parsing, context, exit-code policy, shouldFail
  cmd/urlcheck/main_test.go shouldFail truth table
```

Files: `internal/check/check.go`, `cmd/urlcheck/main.go`,
`cmd/urlcheck/main_test.go`.
Implement: `cmd/urlcheck/main.go` that parses `-timeout` and `-version`, builds a
`context.WithTimeout`, calls `check.URL`, prints URL/status/duration, and exits
per policy — transport error and `5xx` exit 1, `4xx` exit 0, usage error exit 2.
Test: `shouldFail(status)` as a table-driven truth table.
Verify: `go test -count=1 ./...`, `go vet ./...`, then
`go build ./cmd/urlcheck`.

Set up the module:

```bash
mkdir -p ~/go-exercises/urlcheck/cmd/urlcheck ~/go-exercises/urlcheck/internal/check
cd ~/go-exercises/urlcheck
go mod init example.com/urlcheck
```

### The exit-code policy is a pure function

The whole point of the adapter pattern is that `main` should contain no `if` you
would want to test. Here the interesting decision is: which outcomes are process
failures? The policy is that a transport failure and a server-side `5xx` are
failures (exit 1), while a `4xx` is a *successful check* (exit 0) because the tool
reached the server and got a valid answer — whether that answer is `404` is the
caller's business, not the checker's. A usage error (wrong number of arguments)
is exit 2, the conventional code for "you invoked me wrong".

That policy lives in `shouldFail(statusCode int) bool`, a one-line pure function
that a table-driven test pins down completely. `main` calls it and translates the
boolean into `os.Exit(1)`. Because the decision is extracted, the test needs no
process, no network, and no flag parsing — it just calls `shouldFail` with
integers.

`internal/check` here is the same package from Exercise 1, placed behind the
compiler-enforced `internal/` boundary so it is shared by the command without
becoming public API.

Create `internal/check/check.go`:

```go
package check

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net/http"
	"time"
)

// ErrEmptyURL is returned when URL is called with an empty target.
var ErrEmptyURL = errors.New("url is required")

// Client is the one method of *http.Client that URL needs.
type Client interface {
	Do(*http.Request) (*http.Response, error)
}

// Result is the outcome of a single health check.
type Result struct {
	URL        string
	StatusCode int
	Duration   time.Duration
}

// URL performs a context-carrying GET and returns a structured Result.
func URL(ctx context.Context, client Client, rawURL string) (Result, error) {
	if rawURL == "" {
		return Result{}, ErrEmptyURL
	}
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
```

### The command: glue only

`main` parses the flags, handles `-version` early, validates the argument count,
builds a timeout context, calls `check.URL`, prints the result, and maps to an
exit code. The `versionString` helper prefers an ldflags-injected value and falls
back to the build info the toolchain embeds; the next exercise studies that
selection in depth, so here it stays compact.

Create `cmd/urlcheck/main.go`:

```go
package main

import (
	"context"
	"flag"
	"fmt"
	"net/http"
	"os"
	"runtime/debug"
	"time"

	"example.com/urlcheck/internal/check"
)

// version is overridable at link time: -ldflags '-X main.version=1.2.3'.
var version = "dev"

func main() {
	timeout := flag.Duration("timeout", 2*time.Second, "request timeout")
	showVersion := flag.Bool("version", false, "print version and exit")
	flag.Parse()

	if *showVersion {
		fmt.Println(versionString())
		return
	}
	if flag.NArg() != 1 {
		fmt.Fprintln(os.Stderr, "usage: urlcheck [-timeout duration] URL")
		os.Exit(2)
	}

	ctx, cancel := context.WithTimeout(context.Background(), *timeout)
	defer cancel()

	res, err := check.URL(ctx, http.DefaultClient, flag.Arg(0))
	if err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}

	fmt.Printf("%s %d %s\n", res.URL, res.StatusCode, res.Duration.Round(time.Millisecond))
	if shouldFail(res.StatusCode) {
		os.Exit(1)
	}
}

// shouldFail is the exit-code policy: a 5xx is a process failure, a 4xx is not.
func shouldFail(statusCode int) bool {
	return statusCode >= 500
}

func versionString() string {
	if version != "dev" {
		return version
	}
	if info, ok := debug.ReadBuildInfo(); ok {
		if v := info.Main.Version; v != "" && v != "(devel)" {
			return v
		}
		for _, s := range info.Settings {
			if s.Key == "vcs.revision" && s.Value != "" {
				if len(s.Value) > 12 {
					return s.Value[:12]
				}
				return s.Value
			}
		}
	}
	return version
}
```

Run it:

```bash
go run ./cmd/urlcheck -version
```

Expected output (outside a VCS checkout, no ldflags):

```
dev
```

A malformed invocation exits 2:

```bash
go run ./cmd/urlcheck
# usage: urlcheck [-timeout duration] URL   (to stderr; exit status 2)
```

### Tests

`shouldFail` is the branchable logic, so it is what the test pins down. The table
covers the boundary that matters: `499` and `500` land on opposite sides.

Create `cmd/urlcheck/main_test.go`:

```go
package main

import "testing"

func TestShouldFail(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name string
		code int
		want bool
	}{
		{name: "ok 200", code: 200, want: false},
		{name: "redirect 302", code: 302, want: false},
		{name: "client error 404", code: 404, want: false},
		{name: "last non-failing 499", code: 499, want: false},
		{name: "first failing 500", code: 500, want: true},
		{name: "bad gateway 502", code: 502, want: true},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			if got := shouldFail(tc.code); got != tc.want {
				t.Fatalf("shouldFail(%d) = %v, want %v", tc.code, got, tc.want)
			}
		})
	}
}

func TestVersionStringDefault(t *testing.T) {
	// With no ldflags override and no VCS info, versionString falls back to the
	// dev placeholder or an embedded module/revision value; it must never be
	// empty.
	if versionString() == "" {
		t.Fatal("versionString() is empty")
	}
}
```

To exercise the exit-code taxonomy end to end, a shell harness (not part of the
Go test) builds and runs the command:

```bash
go build -o urlcheck ./cmd/urlcheck
./urlcheck ; echo "usage exit: $?"        # exit 2 (no URL)
./urlcheck -version                        # prints a version string, exit 0
```

## Review

The command is correct when `main` holds no decision the tests cannot reach: the
only branch that matters, `shouldFail`, is a pure function with a truth table, and
the `4xx`/`5xx` boundary is asserted at `499`/`500`. `main` itself is
untestable-by-design glue — flag parsing, `context.WithTimeout`, `os.Exit` — and
that is the correct shape, not a gap. Keeping `os.Exit` out of `shouldFail` is
what makes the policy testable; a function that calls `os.Exit` cannot be asserted
on, it can only end the process.

The traps: do not put the `>= 500` comparison inline in `main` where no test can
see it; do not inject the version with `-X` against `internal/check.version` (it
must be `main.version`, the variable declared in this package); and always run the
command as a package (`go run ./cmd/urlcheck`), never as a file. Run
`go vet ./...` — it will flag a bad `Printf` verb in the result line if you get
the format string wrong.

## Resources

- [flag package](https://pkg.go.dev/flag) — `Duration`, `Bool`, `Parse`, `NArg`, `Arg`.
- [context.WithTimeout](https://pkg.go.dev/context#WithTimeout) — bounding the request from `main`.
- [os.Exit](https://pkg.go.dev/os#Exit) — process exit codes and the convention for usage errors.
- [cmd/go: internal directories](https://pkg.go.dev/cmd/go#hdr-Internal_Directories) — the compiler-enforced `internal/` boundary.

---

Back to [00-concepts.md](00-concepts.md) | Previous: [02-httptest-table-driven-suite.md](02-httptest-table-driven-suite.md) | Next: [04-build-info-version-stamping.md](04-build-info-version-stamping.md)
