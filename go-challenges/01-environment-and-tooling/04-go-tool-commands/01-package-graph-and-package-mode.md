# Exercise 1: Package Patterns vs Single-File Mode

Every source-touching `go` subcommand takes a package *pattern*, not a file. This
module builds a small library and a command that consumes it, then proves the
difference between package mode (`go run ./cmd/demo`) and single-file mode
(`go run cmd/demo/main.go`) — the one that silently drops sibling files in a real
service package.

## What you'll build

```text
package-graph/                 module example.com/package-graph
  go.mod
  internal/
    circle/
      circle.go                Area(radius) (float64, error); ErrNegativeRadius
      circle_test.go           table-driven test of the library
  cmd/
    demo/
      main.go                  flag parsing, calls circle.Area, os.Exit codes
      banner.go                sibling file: banner() used by main.go
```

- Files: `internal/circle/circle.go`, `internal/circle/circle_test.go`, `cmd/demo/main.go`, `cmd/demo/banner.go`.
- Implement: a `circle.Area` library and a `cmd/demo` whose `main.go` depends on a sibling `banner.go` in the same package.
- Test: `circle_test.go` covers the math and the sentinel error.
- Verify: `go build ./...` and `go test -count=1 -race ./...`, then compare `go run ./cmd/demo` with `go run cmd/demo/main.go`.

Create the module:

```bash
mkdir -p package-graph/cmd/demo package-graph/internal/circle
cd package-graph
go mod init example.com/package-graph
```

### Why the pattern, not the file, is the unit

The `demo` command lives in `cmd/demo` and is spread across two files:
`main.go` holds `func main`, and `banner.go` holds a helper `banner()` that
`main` calls. That two-file split is deliberate and utterly ordinary — real
`main` packages hold flag wiring, a version string, signal handling, and the
entry point across several files. It is the smallest faithful model of a service
command.

`go run ./cmd/demo` resolves the *package* at that import path and compiles every
non-excluded `.go` file in it — `main.go` and `banner.go` together — exactly as
`go build ./cmd/demo` would. `go run cmd/demo/main.go`, by contrast, is
single-file mode: it treats the one named file as an ad-hoc package, never looks
at `banner.go`, and fails to compile because `banner` is undefined. In a package
with no cross-file references it would compile — and silently omit whatever those
other files did (an `init`, a registration, a build-tagged variant), producing a
binary that is not the one you ship. The lesson is mechanical: name the package,
never the file.

The command also demonstrates exit codes. A negative radius makes the demo call
`os.Exit(2)`. Run the built binary directly and `$?` is `2`; run it through
`go run` and the wrapper flattens every non-zero program exit to `1` while
printing `exit status 2` to stderr. To observe a program's real exit status you
must build and run the binary, not `go run` it.

Create `internal/circle/circle.go`:

```go
package circle

import (
	"errors"
	"math"
)

// ErrNegativeRadius is returned when the radius is negative. Zero is allowed;
// a zero-radius circle has area zero.
var ErrNegativeRadius = errors.New("radius must not be negative")

// Area returns the area of a circle with the given radius.
func Area(radius float64) (float64, error) {
	if radius < 0 {
		return 0, ErrNegativeRadius
	}
	return math.Pi * radius * radius, nil
}
```

Create `cmd/demo/banner.go` — the sibling file single-file mode ignores:

```go
package main

// banner is defined in a separate file of the same package. go run ./cmd/demo
// compiles it; go run cmd/demo/main.go does not.
func banner() string {
	return "circle-tool"
}
```

Create `cmd/demo/main.go`:

```go
package main

import (
	"errors"
	"flag"
	"fmt"
	"os"

	"example.com/package-graph/internal/circle"
)

func main() {
	radius := flag.Float64("radius", 5.0, "circle radius")
	flag.Parse()

	area, err := circle.Area(*radius)
	if err != nil {
		if errors.Is(err, circle.ErrNegativeRadius) {
			fmt.Fprintln(os.Stderr, banner()+": radius must not be negative")
			os.Exit(2)
		}
		fmt.Fprintln(os.Stderr, banner()+": "+err.Error())
		os.Exit(1)
	}
	fmt.Printf("%s: radius %.1f has area %.5f\n", banner(), *radius, area)
}
```

Run it in package mode:

```bash
go run ./cmd/demo
```

Expected output:

```text
circle-tool: radius 5.0 has area 78.53982
```

Now build the binary and observe the real exit code that `go run` would hide:

```bash
go build -o bin/demo ./cmd/demo
./bin/demo -radius 5 ; echo "exit=$?"
./bin/demo -radius -1 ; echo "exit=$?"
```

```text
circle-tool: radius 5.0 has area 78.53982
exit=0
circle-tool: radius must not be negative
exit=2
```

Through `go run`, the same negative radius flattens to `1`:

```bash
go run ./cmd/demo -radius -1 ; echo "exit=$?"
```

```text
circle-tool: radius must not be negative
exit status 2
exit=1
```

Finally, single-file mode drops `banner.go` and fails to compile:

```bash
go run cmd/demo/main.go
```

```text
# command-line-arguments
cmd/demo/main.go:19:28: undefined: banner
cmd/demo/main.go:22:27: undefined: banner
cmd/demo/main.go:25:48: undefined: banner
```

`banner` is undefined because single-file mode never read `banner.go`. Package
mode (`go run ./cmd/demo`) always would.

### Tests

Create `internal/circle/circle_test.go`:

```go
package circle

import (
	"errors"
	"math"
	"testing"
)

func TestArea(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name    string
		radius  float64
		want    float64
		wantErr error
	}{
		{name: "zero", radius: 0, want: 0},
		{name: "unit", radius: 1, want: math.Pi},
		{name: "five", radius: 5, want: 25 * math.Pi},
		{name: "negative", radius: -1, wantErr: ErrNegativeRadius},
	}

	const epsilon = 1e-9
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()

			got, err := Area(tc.radius)
			if tc.wantErr != nil {
				if !errors.Is(err, tc.wantErr) {
					t.Fatalf("Area(%v) err = %v, want %v", tc.radius, err, tc.wantErr)
				}
				return
			}
			if err != nil {
				t.Fatalf("Area(%v) unexpected err = %v", tc.radius, err)
			}
			if math.Abs(got-tc.want) > epsilon {
				t.Fatalf("Area(%v) = %v, want %v (+/-%v)", tc.radius, got, tc.want, epsilon)
			}
		})
	}
}
```

## Review

The module is correct when `go build ./...` compiles both files of `cmd/demo`
and `go test -count=1 -race ./...` passes. The teaching point is confirmed by the
three runs above: package mode compiles `main.go` and `banner.go` together;
single-file mode fails with `undefined: banner` because it never read the
sibling; and the built binary exits `2` on a negative radius while `go run`
flattens that to `1`. The mistake to avoid is reaching for `go run main.go` on a
package with more than one file — it does not error loudly in the general case,
it just quietly compiles a smaller program. Name the package (`./cmd/demo`), and
pass `./...` to `build`, `vet`, and `test` so the whole graph is covered.

## Resources

- [Command go — compile and run Go program](https://pkg.go.dev/cmd/go#hdr-Compile_and_run_Go_program) — how `go run` resolves file arguments versus package patterns.
- [Command go — package lists and patterns](https://pkg.go.dev/cmd/go#hdr-Package_lists_and_patterns) — the meaning of `./...` and import-path patterns.
- [os.Exit](https://pkg.go.dev/os#Exit) — process exit codes and why deferred functions do not run.

---

Back to [00-concepts.md](00-concepts.md) | Next: [02-table-driven-parallel-tests.md](02-table-driven-parallel-tests.md)
