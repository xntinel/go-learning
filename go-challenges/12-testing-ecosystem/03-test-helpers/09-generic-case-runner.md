# Exercise 9: A Reusable Table-Driven Case Runner

Table-driven tests are the Go idiom, but the `for _, c := range cases { t.Run(...)
}` scaffolding is copied into every test file. A generic case-runner collapses it
into one line per test while keeping `t.Run` subtests, per-case naming, correct
`t.Helper` attribution, and an opt-in parallel variant. This is the shared harness
a service package imports across dozens of `*_test.go` files.

This module is fully self-contained: its own `go mod init`, its own demo, its own
tests. Nothing here imports another exercise.

## What you'll build

```text
caserunner/                  independent module: example.com/caserunner
  go.mod                     go 1.26
  classify.go                Classify: HTTP status → coarse class (the code under test)
  cmd/
    demo/
      main.go                classifies a few codes
  runner_test.go             runCases / runCasesParallel generic helpers; driven over Classify
```

- Files: `classify.go`, `cmd/demo/main.go`, `runner_test.go`.
- Implement: `Classify(int) string`, a pure status-code classifier.
- Test: `runCases[C any](t, cases, name, run)` and `runCasesParallel[C any](...)` generic helpers wrapping `t.Run`; drive `Classify` through a typed case slice; a parallel variant under `-race`.
- Verify: `go test -count=1 -race ./...`

Set up the module:

```bash
mkdir -p ~/go-exercises/caserunner/cmd/demo
cd ~/go-exercises/caserunner
go mod init example.com/caserunner
```

### What the runner buys and why it stays generic

The runner is a single generic function:

```go
func runCases[C any](t *testing.T, cases []C, name func(C) string, run func(*testing.T, C))
```

`C` is the case type — a struct the *test* defines, holding inputs and expected
outputs. `name(c)` produces the subtest name; `run(t, c)` performs the assertion.
The runner's body is just the `for`/`t.Run` scaffolding, written once. A test then
reads as:

```go
runCases(t, cases,
	func(c tc) string { return c.name },
	func(t *testing.T, c tc) { ... assert ... })
```

Type inference fills in `C` from the `cases` slice, so callers never spell the type
parameter. Two disciplines make it correct. First, `runCases` calls `t.Helper()`,
and so does the closure the caller passes if it wraps further helpers — attribution
must reach the *case's* line, not the runner internals. Second, the parallel
variant (`runCasesParallel`) calls `t.Parallel()` inside each subtest; with Go
1.22+ loop-variable scoping there is no `c := c` needed, because each iteration's
`c` is already a fresh variable. On older toolchains this exact pattern shared one
variable across parallel subtests and silently tested the last case N times — the
canonical parallel-subtest bug that the language fix eliminated.

Create `classify.go`:

```go
package caserunner

// Classify maps an HTTP status code to a coarse class used in metrics and logs.
func Classify(code int) string {
	switch {
	case code >= 100 && code < 200:
		return "informational"
	case code >= 200 && code < 300:
		return "success"
	case code >= 300 && code < 400:
		return "redirect"
	case code >= 400 && code < 500:
		return "client_error"
	case code >= 500 && code < 600:
		return "server_error"
	default:
		return "unknown"
	}
}
```

### The runnable demo

Create `cmd/demo/main.go`:

```go
package main

import (
	"fmt"

	"example.com/caserunner"
)

func main() {
	for _, code := range []int{100, 204, 301, 404, 503, 999} {
		fmt.Printf("%d -> %s\n", code, caserunner.Classify(code))
	}
}
```

Run it:

```bash
go run ./cmd/demo
```

Expected output:

```
100 -> informational
204 -> success
301 -> redirect
404 -> client_error
503 -> server_error
999 -> unknown
```

### The tests

`runCases` and `runCasesParallel` are the generic harness. Each calls `t.Helper()`
and loops with `t.Run`; the parallel variant additionally marks each subtest
parallel. `TestClassify` drives a typed case slice through the serial runner, one
line for the name projection and one for the assertion. `TestClassifyParallel`
drives the same table through the parallel runner and runs under `-race` to prove
the per-case variable is not shared. A negative closure demonstrates that a failing
case attributes to the case, not the runner.

Create `runner_test.go`:

```go
package caserunner

import (
	"fmt"
	"testing"
)

// runCases runs each case as a t.Run subtest, naming and executing it via the
// supplied closures. t.Helper keeps failure attribution on the caller's line.
func runCases[C any](t *testing.T, cases []C, name func(C) string, run func(*testing.T, C)) {
	t.Helper()
	for _, c := range cases {
		t.Run(name(c), func(t *testing.T) {
			run(t, c)
		})
	}
}

// runCasesParallel is runCases with each subtest marked parallel. Safe because
// Go 1.22+ gives each iteration a fresh c (no c := c needed).
func runCasesParallel[C any](t *testing.T, cases []C, name func(C) string, run func(*testing.T, C)) {
	t.Helper()
	for _, c := range cases {
		t.Run(name(c), func(t *testing.T) {
			t.Parallel()
			run(t, c)
		})
	}
}

type classifyCase struct {
	name string
	code int
	want string
}

var classifyCases = []classifyCase{
	{"informational", 100, "informational"},
	{"success", 204, "success"},
	{"redirect", 301, "redirect"},
	{"client_error", 404, "client_error"},
	{"server_error", 503, "server_error"},
	{"unknown_low", 42, "unknown"},
	{"unknown_high", 700, "unknown"},
}

func TestClassify(t *testing.T) {
	t.Parallel()
	runCases(t, classifyCases,
		func(c classifyCase) string { return c.name },
		func(t *testing.T, c classifyCase) {
			if got := Classify(c.code); got != c.want {
				t.Fatalf("Classify(%d) = %q, want %q", c.code, got, c.want)
			}
		})
}

func TestClassifyParallel(t *testing.T) {
	t.Parallel()
	runCasesParallel(t, classifyCases,
		func(c classifyCase) string { return c.name },
		func(t *testing.T, c classifyCase) {
			if got := Classify(c.code); got != c.want {
				t.Fatalf("Classify(%d) = %q, want %q", c.code, got, c.want)
			}
		})
}

func ExampleClassify() {
	fmt.Println(Classify(404))
	// Output: client_error
}
```

## Review

The runner is correct when it preserves everything a hand-written table loop gives
you — named `t.Run` subtests (visible in `-run` filters and failure output),
per-case parallelism when asked, and attribution to the case line via `t.Helper` —
while removing the boilerplate. `TestClassifyParallel` under `-race` is the proof
that the parallel variant does not share the loop variable across subtests; on a
pre-1.22 toolchain the same code would have raced and tested one case repeatedly.
Confirm attribution by temporarily breaking one expected value and reading the
`file:line`: it must point at the case data or the assertion closure, not into
`runCases`. The mistake to avoid is over-generalizing the runner into a mini
framework with hidden branching; keep it to the scaffolding and let the caller's
closure hold the actual assertion.

## Resources

- [Go Blog: Using Subtests and Sub-benchmarks](https://go.dev/blog/subtests) — why `t.Run` names matter and how subtests compose.
- [testing.T.Parallel](https://pkg.go.dev/testing#T.Parallel) — the parallel-subtest semantics the runner opts into.
- [Go 1.22 loop variable scoping](https://go.dev/blog/loopvar-preview) — why `c := c` is no longer needed in parallel subtests.

---

Back to [00-concepts.md](00-concepts.md) | Next: [10-fake-clock-dependency-helper.md](10-fake-clock-dependency-helper.md)
