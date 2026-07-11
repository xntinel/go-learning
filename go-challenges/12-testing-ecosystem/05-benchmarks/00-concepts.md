# Benchmarking Go Code for Production Performance Work — Concepts

Benchmarks are the evidence layer for every performance claim a senior engineer
makes. When you say "the new JSON path is faster", "this cache change removed an
allocation from the hot loop", or "the parser is linear, not quadratic", the only
thing that separates that from a guess is a benchmark you can re-run. Go bakes
benchmarking into the same `go test` tool that runs unit tests, which means the
measurement discipline lives next to the code it measures and travels with it in
version control. This file is the conceptual foundation. Read it once and you have
what you need to reason through each of the ten independent exercises that follow —
each one is a real backend artifact (a request-normalization helper, a DTO marshal
path, a read-through cache, a log parser, a concurrent metrics counter, a batch
worker, a repository lookup, a response builder) benchmarked the way you would
actually benchmark it on the job.

Treat a benchmark as a measurement instrument, not a syntax curiosity. Like any
instrument it has a zero point to calibrate (dead-code elimination), a region of
interest to isolate (setup exclusion), a unit to report in (ns/op, MB/s,
allocs/op, or a domain metric), and a noise floor you must characterize before you
trust a reading (statistics with benchstat). Everything below is one of those four
concerns.

## Concepts

### What a benchmark is and how the framework drives it

A benchmark is a function named `BenchmarkXxx(b *testing.B)`, in a `_test.go`
file, alongside your tests. It is not run by a plain `go test`; you opt in with
`go test -bench=<regexp>`, and the regexp selects which benchmarks run by name
(`-bench=.` runs all, `-bench=Marshal` runs the ones matching `Marshal`). A bare
`-bench` with no pattern runs none — the pattern is mandatory.

The framework does not run your benchmark body a fixed number of times. It runs it
once, times it, then repeatedly grows the iteration count and re-runs until the
timed region has lasted at least `-benchtime` (default one second). Only then does
it divide elapsed time by iterations to report `ns/op`. This is why a benchmark of
a nanosecond-scale function still produces a stable number: the framework has run
it hundreds of millions of times to accumulate a measurable interval. The
consequence for you is that the per-iteration cost must be *constant* across
iterations — anything that runs once but is counted every time (setup inside the
loop) corrupts the average.

### The classic loop: `for i := range b.N`

In the form every existing codebase uses, `b.N` is the framework-chosen iteration
count and your loop is `for i := range b.N { work() }` (or the older
`for i := 0; i < b.N; i++`). Two rules follow directly from "the framework chooses
`b.N`": you must never hardcode an assumption about its value (it changes run to
run and grows into the hundreds of millions), and every piece of one-time setup —
allocating inputs, parsing a fixture, warming a connection — must live *before* the
loop, because anything inside it is paid `b.N` times and folded into every op.

### The modern loop: `for b.Loop()` (Go 1.24)

Go 1.24 added `for b.Loop() { work() }`. It runs the body a framework-chosen number
of times exactly like `b.N`, but it additionally fixes the two classic footguns in
one stroke. First, it executes any code *before* the `for` and any code *after* it
exactly once, outside the timed region — so setup written naturally above the loop
and cleanup below it are automatically excluded, no `ResetTimer` needed. Second, it
keeps the loop's inputs and results alive across the call boundary, so the compiler
cannot prove the work is dead and delete it. `b.Loop` is the recommended modern
form. `b.N` remains valid, correct, and ubiquitous, so you must be fluent in both:
you will write new benchmarks with `b.Loop` and read (and maintain) thousands
written with `b.N`.

### Dead-code elimination: the number-one source of fake results

The compiler is allowed to delete work whose result nothing observes. If your
benchmark computes a hash and throws the return value away, the optimizer may prove
the entire call has no effect and remove it, and you will "measure" a function that
never ran — the tell is a sub-nanosecond `ns/op`, say `0.3 ns/op`, which is faster
than a single memory load and therefore physically impossible for real work. That
number is a bug in the benchmark, not a fast function. There are two defenses.
Assign the result to a package-level variable (a "sink") each iteration, so the
compiler cannot prove the value is unused; a package-level sink specifically defeats
the escape analysis that a local variable would not. Or use `for b.Loop()`, which
keeps results live for you. Either works; do one of them, always, for any
benchmark of a pure function.

### Isolating the region of interest: ResetTimer, StopTimer, StartTimer

When setup is unavoidable inside the timed function's vicinity, three methods carve
it out of the measurement. `b.ResetTimer()` zeroes the elapsed-time and
memory-allocation counters at the point you call it, so everything before it — a
warm-up that fills a cache with N entries, for instance — is excluded from the
reported per-op cost. `b.StopTimer()` and `b.StartTimer()` bracket a region *inside*
the loop that must not be measured (regenerating a fresh input for each iteration).
Time spent between a `StopTimer` and the next `StartTimer` is not counted. The trap:
`StopTimer`/`StartTimer` themselves cost something, so in an extremely tight loop
the overhead of toggling the timer can dominate the thing you are trying to measure;
prefer preparing all inputs before the loop when you can. Note that `b.Loop`'s
automatic before/after exclusion makes `ResetTimer` unnecessary for the common case
of one-time setup written above the loop.

### Allocations: the cheapest, most stable regression signal

`b.ReportAllocs()` (or the global `-benchmem` flag) adds two columns: `B/op`, bytes
allocated per operation, and `allocs/op`, the number of heap allocations per
operation. These numbers matter out of proportion to their size because they are
*deterministic and machine-independent*: the same code compiled by the same Go
version allocates the same number of times on your laptop and on a CI runner, with
no dependence on CPU speed, thermal state, or background load. That makes
`allocs/op` the ideal regression gate. A new escape-to-heap in a hot path — a
`fmt.Sprintf` where a `strconv.Itoa` would do, a slice that now outlives the stack —
shows up as `allocs/op` going from 1 to 2 with total reliability, whereas the same
regression in `ns/op` might hide under run-to-run noise. Senior reviewers gate on
`allocs/op` precisely because it does not lie.

### Reporting the right unit: SetBytes and ReportMetric

`ns/op` is the wrong unit for data-volume-bound code. For a parser, encoder,
compressor, or any I/O path, what you care about is *throughput*: how many bytes per
second it can push. `b.SetBytes(n)` tells the framework that each operation
processed `n` bytes, and `go test` then reports an `MB/s` column derived from
`n` and the measured time. Throughput normalizes across input sizes and is the unit
you can compare against a network link or disk. For domain-shaped measurements,
`b.ReportMetric(value, unit)` emits an arbitrary custom column — `events/op`,
`bytes/event`, `rows/op`. The unit must be a single trailing token (the convention
is `something/op`), and if you report the same unit on multiple iterations the
framework averages it. A custom metric communicates amortized cost (the per-item
efficiency of a batch worker) that raw latency hides.

### Contention: RunParallel and SetParallelism

A sequential benchmark measures a function with no competition for its data. In
production that function runs on dozens of goroutines hammering the same mutex or
the same cache line, and the per-op cost there can be an order of magnitude worse.
`b.RunParallel(func(pb *testing.PB) { for pb.Next() { work() } })` runs the body
across `GOMAXPROCS` goroutines, each calling `pb.Next()` until the shared iteration
budget is exhausted, so it exposes lock contention and false sharing that the
sequential number systematically hides. `b.SetParallelism(p)` scales the goroutine
count to `p * GOMAXPROCS` to model an oversubscribed server. The classic finding: a
`sync.Mutex` counter that looks fine sequentially collapses under `RunParallel`,
while an `atomic.Int64` or a sharded counter stays flat — a difference invisible
without the parallel benchmark.

### Numbers are only comparable within one machine, one build, one quiet run

A single benchmark number is meaningless in isolation, and a delta between two
single runs on a laptop is noise. Turbo boost, thermal throttling, and background
load move `ns/op` by more than most real optimizations do. The discipline is to run
each variant many times with `-count=N`, save the two result sets to files, and feed
them to `benchstat` (`golang.org/x/perf/cmd/benchstat`), which reports each variant's
median, its variation, and a p-value for the difference. Only a statistically
significant, replicated delta justifies a performance claim in a PR or a CI gate.
"It was 8% faster on my machine once" is not evidence; "benchstat reports −8.2% at
p=0.002 over ten runs" is.

### Sub-benchmarks and complexity detection

`b.Run(name, func(b *testing.B){ ... })` nests a sub-benchmark, exactly like
`t.Run` nests a sub-test. Sweeping one input parameter — input size, implementation
variant — across a set of `b.Run` calls in a single benchmark function turns the
tool into a complexity detector. Run the same lookup over sizes 100, 1k, 10k, 100k
and read the `ns/op` curve: a flat curve is O(1), a curve that grows in proportion
to size is O(n), and a curve that grows faster than size (10x the input costing 100x
the time) is the superlinear signature of an accidental O(n^2) — a linear scan where
a map was needed, a quadratic string build, an N+1 query. This is how a benchmark
catches an algorithmic regression before it reaches production traffic.

## Common Mistakes

### Setup inside the timed loop

Wrong: allocating the input, parsing a fixture, or warming a connection inside
`for i := range b.N`, so its cost is paid every iteration and amortized into every
reported op. Fix: move one-time setup above the loop; for per-iteration setup that
cannot move, bracket it with `StopTimer`/`StartTimer`; or use `for b.Loop()`, which
excludes code before and after the loop automatically.

### Discarding the result and celebrating a sub-nanosecond number

Wrong: `for i := range b.N { Hash(data) }` with the return value dropped, then
reporting `0.3 ns/op` as a triumph. The compiler deleted the call; you measured
nothing. Fix: assign to a package-level sink (`sink = Hash(data)`) or use
`for b.Loop()`, which keeps results alive.

### Redundant timer control inside b.Loop

Wrong: calling `b.ResetTimer()` or `StopTimer`/`StartTimer` inside a `for b.Loop()`
range, or hand-rolling a `for i := range b.N` loop and manually excluding setup that
`b.Loop` already excludes. Fix: let `b.Loop` do its job — put setup before the loop,
cleanup after, and leave the timer alone.

### Trusting a single-run laptop delta

Wrong: running each version once, seeing new is 6% faster, and putting that in the
PR. Thermal and scheduling noise dwarf 6%. Fix: `-count=10` into two files and
`benchstat old.txt new.txt`; read the p-value before you claim anything.

### Comparing across machines or on a busy CI runner

Wrong: gating a release on a raw `ns/op` threshold measured on whatever CI node was
free, or comparing a number from an M-series laptop to one from an x86 server.
Fix: compare only controlled, replicated runs on the same machine and build; gate
on a benchstat delta, not an absolute number. `allocs/op`, being
machine-independent, is the one figure safe to gate as an absolute.

### Forgetting -benchmem / ReportAllocs

Wrong: benchmarking a hot path without allocation reporting, so a new heap
allocation ships invisibly and shows up later as GC pressure in production. Fix:
call `b.ReportAllocs()` (or always pass `-benchmem`) and treat `allocs/op` as a
regression gate in review.

### Benchmarking only sequentially

Wrong: measuring a mutex-guarded counter with a single-goroutine benchmark, seeing
a fine number, and shipping it — then watching it collapse under real concurrent
traffic. Fix: add a `RunParallel` benchmark for anything touched by multiple
goroutines, and compare the parallel cost of your synchronization choices.

### Misusing ReportMetric

Wrong: `b.ReportMetric(x, "events per op")` (the unit must be a single trailing
token, not a phrase with spaces), or reporting a metric the framework already
derives, producing confusing duplicate columns. Fix: use a single-token unit like
`"events/op"`, and only report metrics `go test` does not already compute.

Next: [01-reverse-string-benchmark.md](01-reverse-string-benchmark.md)
