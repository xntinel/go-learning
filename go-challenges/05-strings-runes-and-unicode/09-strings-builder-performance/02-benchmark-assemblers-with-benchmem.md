# Exercise 2: Measure the Cost: Benchmarking Allocations with b.Loop and -benchmem

A claim that `strings.Builder` is faster than `+` is worthless until it is measured
and committed as a regression detector. This exercise benchmarks the two assemblers
with Go 1.24's `for b.Loop()` harness under `-benchmem`, so `ns/op` and `allocs/op`
are visible, and teaches reading the numbers as ratios rather than machine-specific
absolutes.

This module is self-contained: it carries its own copy of the two assemblers so it
gates alone.

## What you'll build

```text
logbench/                    independent module: example.com/logbench
  go.mod
  logbench.go                Naive and Builder assemblers (same behavior as ex1)
  cmd/
    demo/
      main.go                prints the two outputs and confirms they match
  logbench_test.go           correctness guard + BenchmarkNaive/BenchmarkBuilder
```

Files: `logbench.go`, `cmd/demo/main.go`, `logbench_test.go`.
Implement: `Naive` and `Builder` (identical output), then `BenchmarkNaive` and `BenchmarkBuilder` using `for b.Loop()` with `ReportAllocs`.
Test: a correctness test guards the benched functions; the benchmarks measure them.
Verify: `go test -bench=. -benchmem -run=^$ ./...`

```bash
mkdir -p ~/go-exercises/logbench/cmd/demo
cd ~/go-exercises/logbench
go mod init example.com/logbench
```

### Why b.Loop, and what it changes

The classic benchmark loop was `for i := 0; i < b.N; i++ { ... }`, and it had two
recurring hazards. First, any setup done before the loop was timed unless you
remembered to call `b.ResetTimer()`. Second, the compiler could observe that the
loop's result was unused and delete the work, so you had to assign it to a package
sink to keep it alive. Go 1.24 added `for b.Loop()`, which fixes both: it excludes
everything before the first `b.Loop()` call from the timing automatically, and it
keeps the loop body's results alive so the optimizer cannot eliminate the work you
are measuring. The idiom is simply: do the setup (build the `kv` slice) before the
loop, then `for b.Loop() { _ = Builder(...) }`. No `ResetTimer`, no manual sink.

`b.ReportAllocs()` (equivalently, running with the `-benchmem` flag) adds two columns
to the output: `B/op`, the bytes allocated per operation, and `allocs/op`, the number
of distinct allocations per operation. Those two are the numbers that matter here.
`Naive` allocates one throwaway string per `+=`, so its `allocs/op` scales with the
field count; `Builder` allocates its growing buffer (often once, given `Grow`) plus
the final string, so its `allocs/op` is small and roughly constant. `allocs/op` is a
far more stable and portable signal than `ns/op`: it does not change with CPU clock
speed, and a jump in it is exactly what a reintroduced `+` loop looks like.

Read the output as a ratio and an allocation count, not as absolute nanoseconds. A
line like `BenchmarkBuilder-8   ...   3 allocs/op` next to
`BenchmarkNaive-8   ...   9 allocs/op` is the deliverable: it says Builder does a
third of the allocations, and if a future change pushes Builder's number up, the
benchmark — run in CI or with `benchstat` across before/after — flags the regression.
The gate for this exercise runs `go test -race` (no `-bench`), so the benchmarks only
need to compile and the correctness test must pass; you run the benchmark yourself
with the `-bench` command to see the numbers.

Create `logbench.go`:

```go
package logbench

import "strings"

// Naive assembles a log line with += in a loop: one throwaway string allocation
// per key=value pair.
func Naive(level, ts, msg string, kv []string) string {
	s := level + " " + ts + " " + msg
	for i := 0; i+1 < len(kv); i += 2 {
		s += " " + kv[i] + "=" + kv[i+1]
	}
	return s
}

// Builder assembles the same line into one strings.Builder pre-sized with Grow.
func Builder(level, ts, msg string, kv []string) string {
	var b strings.Builder
	b.Grow(len(level) + 1 + len(ts) + 1 + len(msg) + len(kv)*16)
	b.WriteString(level)
	b.WriteByte(' ')
	b.WriteString(ts)
	b.WriteByte(' ')
	b.WriteString(msg)
	for i := 0; i+1 < len(kv); i += 2 {
		b.WriteByte(' ')
		b.WriteString(kv[i])
		b.WriteByte('=')
		b.WriteString(kv[i+1])
	}
	return b.String()
}
```

### The runnable demo

Create `cmd/demo/main.go`:

```go
package main

import (
	"fmt"

	"example.com/logbench"
)

func main() {
	kv := []string{"user", "alice", "ip", "10.0.0.1", "method", "POST", "path", "/api/users", "status", "200"}
	n := logbench.Naive("INFO", "2024-01-15T10:30:00.123Z", "request handled", kv)
	b := logbench.Builder("INFO", "2024-01-15T10:30:00.123Z", "request handled", kv)
	fmt.Println(b)
	fmt.Println("identical:", n == b)
	fmt.Println("run: go test -bench=. -benchmem -run=^$")
}
```

Run it:

```bash
go run ./cmd/demo
```

Expected output:

```
INFO 2024-01-15T10:30:00.123Z request handled user=alice ip=10.0.0.1 method=POST path=/api/users status=200
identical: true
run: go test -bench=. -benchmem -run=^$
```

### Tests and benchmarks

The correctness test is the guard that keeps the benchmark honest: if the two
functions diverge, the benchmark is comparing different work and its numbers are
meaningless, so the test runs in the default `go test` path. The benchmarks use
`for b.Loop()`; note the `kv` slice is built before the loop and is therefore
excluded from the timing automatically.

Create `logbench_test.go`:

```go
package logbench

import (
	"testing"
)

var benchKV = []string{"user", "alice", "ip", "10.0.0.1", "method", "POST", "path", "/api/users", "status", "200"}

func TestAssemblersAgree(t *testing.T) {
	t.Parallel()
	n := Naive("INFO", "2024-01-15T10:30:00.123Z", "request handled", benchKV)
	b := Builder("INFO", "2024-01-15T10:30:00.123Z", "request handled", benchKV)
	if n != b {
		t.Fatalf("assemblers disagree:\n  naive = %q\n  build = %q", n, b)
	}
}

func BenchmarkNaive(b *testing.B) {
	kv := benchKV
	b.ReportAllocs()
	for b.Loop() {
		_ = Naive("INFO", "2024-01-15T10:30:00.123Z", "request handled", kv)
	}
}

func BenchmarkBuilder(b *testing.B) {
	kv := benchKV
	b.ReportAllocs()
	for b.Loop() {
		_ = Builder("INFO", "2024-01-15T10:30:00.123Z", "request handled", kv)
	}
}
```

Run the benchmarks yourself:

```bash
go test -bench=. -benchmem -run=^$ ./...
```

A representative run (absolute `ns/op` varies by machine; the ratio and `allocs/op`
are the point):

```text
BenchmarkNaive-8      2500000    470 ns/op    528 B/op    9 allocs/op
BenchmarkBuilder-8    6000000    150 ns/op    192 B/op    2 allocs/op
```

## Review

The benchmark is meaningful only because the correctness test proves the two
functions do identical work; without that guard, a faster-but-wrong `Builder` would
look like a win. Read the result as allocations and a ratio: `Builder` here does a
fraction of `Naive`'s allocations, which is stable across machines, whereas `ns/op`
is not — do not commit an assertion on nanoseconds. The trap this exercise retires is
the old `for i := 0; i < b.N; i++` scaffolding with manual `ResetTimer`: `for b.Loop()`
resets the timer after setup and keeps results alive for you, and forgetting
`-benchmem`/`ReportAllocs` would hide the very allocation regression the benchmark
exists to catch.

## Resources

- [testing.B.Loop](https://pkg.go.dev/testing#B.Loop) — the Go 1.24 benchmark loop.
- [testing.B.ReportAllocs](https://pkg.go.dev/testing#B.ReportAllocs) — per-op allocation reporting.
- [go test flags: -bench, -benchmem](https://pkg.go.dev/cmd/go#hdr-Testing_flags) — running benchmarks and memory stats.

---

Prev: [01-log-line-assembler-builder-vs-naive.md](01-log-line-assembler-builder-vs-naive.md) | Back to [00-concepts.md](00-concepts.md) | Next: [03-sql-bulk-insert-placeholder-builder.md](03-sql-bulk-insert-placeholder-builder.md)
