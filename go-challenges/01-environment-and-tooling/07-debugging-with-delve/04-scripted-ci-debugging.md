# Exercise 4: Headless Scripted Sessions with dlv exec --init

Debugger output becomes useful in CI only when a machine can produce and assert on
it without a human at the REPL. Delve replays a command file with `--init` and
loads more commands mid-session with `source`, so a pipeline can build a binary,
drive a scripted session, capture the output, and fail the job if an expected
marker is missing. This module builds a metrics aggregator and a `debug.script`
that Delve replays non-interactively.

This module is fully self-contained: its own `go mod init`, demo, and test.

## What you'll build

```text
metricsdbg/                independent module: example.com/metricsdbg
  go.mod                   go 1.24
  metrics/
    metrics.go             type Summary; Summarize([]int) Summary
  cmd/
    demo/
      main.go              prints the summary of a fixed batch
  metrics/metrics_test.go  table-driven test + Example
  debug.script             the replayed Delve command file
```

- Files: `metrics/metrics.go`, `cmd/demo/main.go`, `metrics/metrics_test.go`.
- Implement: `Summarize(xs []int) Summary` returning count, total, and max.
- Test: table-driven cases (empty, single, many) plus an `Example`.
- Verify: `go test -count=1 -race ./...`, then a CI-style block: build, `dlv exec --init`, capture, `grep -q`.

Set up the module:

```bash
mkdir -p go-solutions/01-environment-and-tooling/07-debugging-with-delve/04-scripted-ci-debugging/metrics go-solutions/01-environment-and-tooling/07-debugging-with-delve/04-scripted-ci-debugging/cmd/demo
cd go-solutions/01-environment-and-tooling/07-debugging-with-delve/04-scripted-ci-debugging
go mod edit -go=1.24
```

### Why scripted debugging over a running binary

The interactive REPL is unusable in a pipeline: nothing types the commands. Two
Delve features make it non-interactive. `--init <file>` runs every command in the
file before the REPL would start; end the file with `quit` and Delve exits after
the script, so the whole session is one command with a captured stdout. `source
<file>` does the same mid-session, letting you factor a long sequence into a
reusable file you load on demand. Because `dlv exec` consumes a pre-built binary
rather than compiling one, you must build it yourself with `-gcflags='all=-N -l'`
first, or the debugger sees an optimized binary and prints `<optimized out>` for
the variables the script tries to read.

The payoff is an assertable artifact: redirect the session's output to a file and
`grep -q` for the value you expect. If the marker is missing the grep exits
non-zero and the CI step fails, which is exactly the contract a pipeline wants.

Create `metrics/metrics.go`:

```go
package metrics

// Summary aggregates a batch of integers.
type Summary struct {
	Count int
	Total int
	Max   int
}

// Summarize computes the count, sum, and maximum of xs. For an empty input the
// zero Summary is returned (Max is 0).
func Summarize(xs []int) Summary {
	s := Summary{}
	for _, x := range xs {
		s.Count++
		s.Total += x
		if s.Count == 1 || x > s.Max {
			s.Max = x
		}
	}
	return s
}
```

Create `cmd/demo/main.go`:

```go
package main

import (
	"fmt"

	"example.com/metricsdbg/metrics"
)

func main() {
	batch := []int{10, 20, 30, 40, 50}
	s := metrics.Summarize(batch)
	fmt.Printf("count=%d total=%d max=%d\n", s.Count, s.Total, s.Max)
}
```

Run it:

```bash
go run ./cmd/demo
```

Expected output:

```text
count=5 total=150 max=50
```

### The replayed script

Create `debug.script`:

```text
break metrics/metrics.go:16
continue
print xs
print s.Total
quit
```

Line 16 is the loop body inside `Summarize`. The script stops there on the first
element, prints the slice header for `xs`, prints the running `Total`, and quits.
Replay it against a binary built with debug info:

```bash
go build -gcflags='all=-N -l' -o bin/demo ./cmd/demo

dlv exec ./bin/demo --init debug.script 2>&1 | tee dbg.out

grep -q '\[\]int len: 5, cap: 5' dbg.out || { echo "missing slice header"; exit 1; }
echo OK
```

Captured output includes the slice header Delve decodes for you:

```text
[]int len: 5, cap: 5, [10,20,30,40,50]
```

The `grep -q` asserts the header appears; the `exit 1` fails the job when it does
not. Swapping `--init debug.script` for a `source debug.script` line typed inside
a live session loads the same commands mid-flight, which is how you keep a library
of debugging scripts and pull one in when a session needs it.

### The test verifies the aggregation itself

Create `metrics/metrics_test.go`:

```go
package metrics

import (
	"fmt"
	"testing"
)

func TestSummarize(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name string
		in   []int
		want Summary
	}{
		{name: "empty", in: nil, want: Summary{}},
		{name: "single", in: []int{7}, want: Summary{Count: 1, Total: 7, Max: 7}},
		{name: "many", in: []int{10, 20, 30, 40, 50}, want: Summary{Count: 5, Total: 150, Max: 50}},
		{name: "negatives", in: []int{-5, -2, -9}, want: Summary{Count: 3, Total: -16, Max: -2}},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			if got := Summarize(tc.in); got != tc.want {
				t.Fatalf("Summarize(%v) = %+v; want %+v", tc.in, got, tc.want)
			}
		})
	}
}

func ExampleSummarize() {
	s := Summarize([]int{10, 20, 30, 40, 50})
	fmt.Printf("count=%d total=%d max=%d\n", s.Count, s.Total, s.Max)
	// Output: count=5 total=150 max=50
}
```

The `many` case asserts `Total: 150` — the correct sum of the batch, matching the
demo's printed `total=150`. Under the debugger, `print s.Total` at the breakpoint
shows `0` on the first hit, because execution stops just before the `s.Total += x`
on that line runs; the demo's `150` is the final value after the loop completes.
Run the gate to confirm the aggregation:

```bash
gofmt -l .
go vet ./...
go build ./...
go test -count=1 -race ./...
```

## Review

The aggregator is correct when `Summarize` returns count, sum, and max over the
batch and the zero `Summary` for an empty input, which the table pins including
the negatives case where `Max` must be the least-negative element rather than
`0`. The scripting proof is the captured artifact: `dlv exec --init` writes the
slice header to a file and `grep -q` turns its presence into an exit code. The
mistakes to avoid are building the binary without `-gcflags='all=-N -l'` (the
script's `print xs` then reads `<optimized out>`) and forgetting `quit` at the end
of the script (Delve drops into an interactive REPL that CI cannot answer). Use
`source` to load a shared script mid-session and `--init` to run one before the
REPL.

## Resources

- [Delve CLI command reference](https://github.com/go-delve/delve/blob/master/Documentation/cli/README.md) — `source` and the scriptable REPL commands.
- [`dlv exec` usage](https://github.com/go-delve/delve/blob/master/Documentation/usage/dlv_exec.md) — running a pre-built binary under Delve with `--init`.
- [`dlv debug` usage](https://github.com/go-delve/delve/blob/master/Documentation/usage/dlv_debug.md) — the `--init` flag and how Delve builds with `-N -l`.

---

Back to [00-concepts.md](00-concepts.md) | Next: [05-debug-failing-test.md](05-debug-failing-test.md)
