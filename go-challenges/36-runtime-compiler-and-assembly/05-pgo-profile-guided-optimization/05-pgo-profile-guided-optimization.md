# 5. PGO: Profile-Guided Optimization

Profile-Guided Optimization (PGO) feeds a CPU pprof profile back into the compiler so it can make better decisions at compile time. The hard part is not flipping the switch — placing a `default.pgo` file is all that is required since Go 1.21 — but understanding which decisions change (inlining budget, devirtualization), how source drift affects matching, and why the optimized binary must still be re-profiled each release cycle. This lesson builds a realistic workload package, profiles it, rebuilds with PGO, and measures the difference.

```text
pgoworkload/
  go.mod
  workload.go
  workload_test.go       <- unit tests + BenchmarkRunJSONDominated + BenchmarkTokenize
  cmd/demo/main.go
  default.pgo            <- created by the profiling step (not committed initially)
```

## Concepts

### What the compiler does with a profile

A pprof CPU profile records call-stack samples at ~100 Hz. The Go compiler reads those samples to identify "hot" call sites — pairs of (caller, callee) that account for significant CPU time. Armed with that information it applies two main optimizations in Go 1.21+:

**Aggressive inlining.** The normal inlining budget is intentionally conservative to keep binary size and compilation time reasonable. PGO raises that budget specifically for hot callees. A function that was previously skipped because it exceeded the budget will be inlined when the profile shows it is frequently called. Inlining a callee exposes the callee's body to the caller's optimizer: escape analysis may then allocate on the stack what previously escaped to the heap, loop bodies may be combined, and constant propagation across the call boundary becomes possible. The Go 1.21 blog post reported that inlining `mdurl.Parse` under PGO eliminated 4.97 million heap allocations per benchmark run.

**Devirtualization.** An interface method call (`r.Read(b)`) compiles to an indirect dispatch through the interface's method table. The compiler cannot inline through it. When the profile shows that one concrete type accounts for the vast majority of calls at a particular interface call site, the compiler emits a type check and a direct call for the fast path, with the original indirect call as the fallback:

```go
// compiler-generated equivalent:
if f, ok := r.(*os.File); ok {
	// direct call — now eligible for inlining
	f.Read(b)
} else {
	// original indirect path
	r.Read(b)
}
```

The direct call on the fast path is then a candidate for inlining, which cascades into further escape and constant-folding improvements.

### The AutoFDO workflow

PGO in Go follows the AutoFDO pattern: the profile does not need to match the binary being compiled byte-for-byte. The compiler matches samples to source using (function name, line offset from the function start), so ordinary edits outside hot functions do not break matching, and profiles collected from one version of the binary can guide compilation of a later version. The recommended iterative cycle is:

1. Ship the initial binary (no profile yet, or a stale one — PGO degrades gracefully).
2. Collect a CPU profile from production or a representative benchmark.
3. Place the profile as `default.pgo` in the main package directory (or pass `-pgo=/path/to/file`).
4. Rebuild. The toolchain picks up the profile automatically with `-pgo=auto` (the default since Go 1.21).
5. Ship the new binary, collect a fresh profile, repeat.

### How the toolchain detects a profile

When `go build` or `go test` runs in a directory that contains `default.pgo`, it applies PGO automatically. No flag is needed. To disable explicitly, pass `-pgo=off`. To use a profile at a different path, pass `-pgo=/absolute/or/relative/path.pprof`. To verify that a binary was compiled with PGO:

```bash
go version -m ./mybinary | grep pgo
```

The output includes a line such as:

```
build   -pgo=/path/to/default.pgo
```

### Scope and limitations

PGO applies to the entire program: the standard library and all dependent modules are recompiled with the profile data, not just the main package. Build time increases on the first PGO build because all packages must be rebuilt; subsequent incremental builds are cached normally.

Binary size grows slightly because more functions are inlined. This is an expected trade-off.

New code paths not present in the profile are not optimized. After a significant refactor, collect a new profile so the compiler can track the new hot paths. An unrepresentative profile does not make the program slower — the compiler is conservative and only adds optimizations; it does not pessimize cold code.

Performance gains for most Go programs fall in the 2–14% range (Go 1.22 data). Workloads dominated by interface dispatch and function-call overhead see larger gains; compute-bound tight loops with few calls see smaller gains.

### Measuring the difference

Use `benchstat` (part of `golang.org/x/perf`) to compare benchmark runs with and without PGO. A single benchmark run has statistical noise; `benchstat` computes medians and 95% confidence intervals and reports whether the difference is significant:

```bash
go install golang.org/x/perf/cmd/benchstat@latest
go test -pgo=off -bench=. -count=10 > nopgo.txt
go test          -bench=. -count=10 > withpgo.txt
benchstat nopgo.txt withpgo.txt
```

## Exercises

Set up the module:

```bash
mkdir -p go-solutions/36-runtime-compiler-and-assembly/05-pgo-profile-guided-optimization/05-pgo-profile-guided-optimization/cmd/demo
cd go-solutions/36-runtime-compiler-and-assembly/05-pgo-profile-guided-optimization/05-pgo-profile-guided-optimization
```

This is a library verified by `go test`. The `cmd/demo` binary is a runnable demonstration.

### Exercise 1: Build a realistic workload package

Create `workload.go`. The package implements two `Processor` implementations and a `Run` function that dispatches through the `Processor` interface. This gives the compiler's devirtualizer real interface-call sites to work with. The `Tokenize` function gives the inliner hot string-processing to optimize.

```go
package pgoworkload

import (
	"encoding/json"
	"errors"
	"sort"
	"strings"
	"unicode"
)

// Processor processes a chunk of data and returns the number of items produced.
type Processor interface {
	Process(data []byte) (int, error)
}

// ErrEmptyData is returned when an empty slice is passed to a processor.
var ErrEmptyData = errors.New("pgoworkload: empty data")

// JSONProcessor counts the number of top-level keys in a JSON object.
type JSONProcessor struct{}

// Process implements Processor for JSON input.
func (JSONProcessor) Process(data []byte) (int, error) {
	if len(data) == 0 {
		return 0, ErrEmptyData
	}
	var m map[string]json.RawMessage
	if err := json.Unmarshal(data, &m); err != nil {
		return 0, err
	}
	return len(m), nil
}

// WordProcessor counts words in UTF-8 text after normalizing whitespace.
type WordProcessor struct {
	// MinLength skips words shorter than this many runes (0 means keep all).
	MinLength int
}

// Process implements Processor for plain-text input.
func (p WordProcessor) Process(data []byte) (int, error) {
	if len(data) == 0 {
		return 0, ErrEmptyData
	}
	words := strings.FieldsFunc(string(data), func(r rune) bool {
		return !unicode.IsLetter(r) && !unicode.IsNumber(r)
	})
	if p.MinLength <= 1 {
		return len(words), nil
	}
	n := 0
	for _, w := range words {
		if len([]rune(w)) >= p.MinLength {
			n++
		}
	}
	return n, nil
}

// Run dispatches each input to each processor and returns the total item count.
// The inner loop over processors is the interface dispatch hot spot that PGO's
// devirtualizer targets when one concrete type dominates the profile.
func Run(processors []Processor, inputs [][]byte) (int, error) {
	total := 0
	for _, input := range inputs {
		for _, p := range processors {
			n, err := p.Process(input)
			if err != nil && !errors.Is(err, ErrEmptyData) {
				return total, err
			}
			total += n
		}
	}
	return total, nil
}

// Tokenize splits text into unique lowercase tokens sorted alphabetically.
// It is a hot helper intentionally written to sit just above the normal inlining
// budget so PGO's extended budget can inline it at its call sites.
func Tokenize(text string) []string {
	raw := strings.FieldsFunc(strings.ToLower(text), func(r rune) bool {
		return !unicode.IsLetter(r)
	})
	seen := make(map[string]struct{}, len(raw))
	for _, t := range raw {
		seen[t] = struct{}{}
	}
	out := make([]string, 0, len(seen))
	for t := range seen {
		out = append(out, t)
	}
	sort.Strings(out)
	return out
}
```

### Exercise 2: Write the test file with unit tests and benchmarks

Create `workload_test.go`. Unit tests verify the contract; benchmarks produce the data for comparing PGO vs non-PGO builds.

```go
package pgoworkload

import (
	"errors"
	"fmt"
	"strings"
	"testing"
)

// --- unit tests ---

func TestJSONProcessorCounts(t *testing.T) {
	t.Parallel()

	cases := []struct {
		name    string
		input   []byte
		want    int
		wantErr error
	}{
		{
			name:  "three keys",
			input: []byte(`{"a":1,"b":2,"c":3}`),
			want:  3,
		},
		{
			name:  "empty object",
			input: []byte(`{}`),
			want:  0,
		},
		{
			name:    "empty slice",
			input:   []byte{},
			wantErr: ErrEmptyData,
		},
		{
			name:    "invalid JSON",
			input:   []byte(`not json`),
			wantErr: nil, // json.Unmarshal error, not ErrEmptyData
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			got, err := JSONProcessor{}.Process(tc.input)
			if tc.wantErr != nil {
				if !errors.Is(err, tc.wantErr) {
					t.Fatalf("err = %v, want %v", err, tc.wantErr)
				}
				return
			}
			if tc.name == "invalid JSON" {
				if err == nil {
					t.Fatal("expected error for invalid JSON, got nil")
				}
				return
			}
			if err != nil {
				t.Fatalf("unexpected error: %v", err)
			}
			if got != tc.want {
				t.Fatalf("got %d, want %d", got, tc.want)
			}
		})
	}
}

func TestWordProcessorCounts(t *testing.T) {
	t.Parallel()

	cases := []struct {
		name      string
		input     string
		minLength int
		want      int
	}{
		{"simple sentence", "hello world foo", 0, 3},
		{"min length filters", "a bb ccc dddd", 3, 2},
		{"punctuation stripped", "one, two; three.", 0, 3},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			got, err := WordProcessor{MinLength: tc.minLength}.Process([]byte(tc.input))
			if err != nil {
				t.Fatalf("unexpected error: %v", err)
			}
			if got != tc.want {
				t.Fatalf("got %d, want %d", got, tc.want)
			}
		})
	}
}

func TestWordProcessorEmptyReturnsError(t *testing.T) {
	t.Parallel()

	_, err := WordProcessor{}.Process([]byte{})
	if !errors.Is(err, ErrEmptyData) {
		t.Fatalf("err = %v, want ErrEmptyData", err)
	}
}

func TestRunAccumulatesTotal(t *testing.T) {
	t.Parallel()

	procs := []Processor{
		JSONProcessor{},
		WordProcessor{},
	}
	// JSONProcessor counts top-level keys; WordProcessor counts letter/number tokens.
	// {"alpha":"one"}:             JSON=1 key, Word=2 tokens (alpha, one)   => 3
	// {"beta":"two","gamma":"three"}: JSON=2 keys, Word=4 tokens (beta, two, gamma, three) => 6
	// total = 9
	inputs := [][]byte{
		[]byte(`{"alpha":"one"}`),
		[]byte(`{"beta":"two","gamma":"three"}`),
	}
	got, err := Run(procs, inputs)
	if err != nil {
		t.Fatalf("Run error: %v", err)
	}
	if got != 9 {
		t.Fatalf("got %d, want 9", got)
	}
}

func TestTokenizeDeduplicatesAndSorts(t *testing.T) {
	t.Parallel()

	got := Tokenize("Go go GO is great great")
	want := []string{"go", "great", "is"}
	if len(got) != len(want) {
		t.Fatalf("got %v, want %v", got, want)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Fatalf("got[%d] = %q, want %q", i, got[i], want[i])
		}
	}
}

// ExampleTokenize documents the public contract and is auto-verified by go test.
func ExampleTokenize() {
	tokens := Tokenize("the quick brown fox the fox")
	fmt.Println(strings.Join(tokens, " "))
	// Output: brown fox quick the
}

// --- benchmarks ---
// These are the benchmarks used to generate default.pgo and to measure the
// improvement. Run them with -cpuprofile to produce a profile, then rebuild
// and re-run to compare.

func BenchmarkRunJSONDominated(b *testing.B) {
	// One dominant concrete type (JSONProcessor): ideal for devirtualization.
	procs := []Processor{
		JSONProcessor{},
		JSONProcessor{},
		JSONProcessor{},
		WordProcessor{},
	}
	inputs := make([][]byte, 64)
	for i := range inputs {
		inputs[i] = []byte(fmt.Sprintf(`{"key%d":%d,"val%d":"hello world"}`, i, i, i))
	}
	b.ResetTimer()
	for b.Loop() {
		_, _ = Run(procs, inputs)
	}
}

func BenchmarkTokenize(b *testing.B) {
	text := strings.Repeat("the quick brown fox jumps over the lazy dog ", 32)
	b.ResetTimer()
	for b.Loop() {
		_ = Tokenize(text)
	}
}
```

### Exercise 3: Implement cmd/demo

Create `cmd/demo/main.go`. It uses only the exported API and shows the PGO build info so the learner can confirm the profile was applied:

```go
package main

import (
	"fmt"
	"log"
	"runtime/debug"
	"strings"

	"example.com/pgoworkload"
)

func main() {
	procs := []pgoworkload.Processor{
		pgoworkload.JSONProcessor{},
		pgoworkload.WordProcessor{MinLength: 2},
	}
	inputs := [][]byte{
		[]byte(`{"language":"Go","feature":"pgo","release":"1.21"}`),
		[]byte(`{"phase":"hot","paths":"faster"}`),
	}

	total, err := pgoworkload.Run(procs, inputs)
	if err != nil {
		log.Fatalf("Run: %v", err)
	}
	fmt.Printf("total items processed: %d\n", total)

	tokens := pgoworkload.Tokenize("Profile Guided Optimization profile guided")
	fmt.Printf("unique tokens: %s\n", strings.Join(tokens, ", "))

	// Print build metadata to verify PGO was applied.
	info, ok := debug.ReadBuildInfo()
	if !ok {
		fmt.Println("build info unavailable")
		return
	}
	pgoApplied := false
	for _, s := range info.Settings {
		if s.Key == "-pgo" {
			fmt.Printf("PGO profile: %s\n", s.Value)
			pgoApplied = true
		}
	}
	if !pgoApplied {
		fmt.Println("PGO: not applied (no default.pgo found)")
	}
}
```

### Exercise 4: Run the full PGO workflow

This exercise cannot be automated offline because it requires collecting a real CPU profile from a running benchmark. Follow the steps in order.

**Step 1 — Baseline benchmark (no PGO).**

```bash
cd ~/go-exercises/pgoworkload
go test -pgo=off -bench=. -benchtime=3s -count=10 > nopgo.txt
cat nopgo.txt
```

Confirm you see benchmark lines for `BenchmarkRunJSONDominated` and `BenchmarkTokenize`.

**Step 2 — Collect a CPU profile.**

```bash
go test -bench=BenchmarkRunJSONDominated -benchtime=20s -cpuprofile=default.pgo
```

The `-cpuprofile` flag writes a pprof CPU profile to `default.pgo`. The Go toolchain will pick this file up automatically on the next build because it resides in the main package directory.

**Step 3 — Rebuild with PGO and benchmark again.**

No special flag is needed. The presence of `default.pgo` triggers `-pgo=auto`:

```bash
go test -bench=. -benchtime=3s -count=10 > withpgo.txt
```

Verify PGO was applied by inspecting the test binary:

```bash
go test -c -o pgoworkload.test
go version -m ./pgoworkload.test | grep pgo
```

Expected output (the exact path will vary):

```
build	-pgo=/home/you/go-exercises/pgoworkload/default.pgo
```

**Step 4 — Compare results with benchstat.**

```bash
go install golang.org/x/perf/cmd/benchstat@latest
benchstat nopgo.txt withpgo.txt
```

Typical output format:

```
goos: linux
goarch: amd64
pkg: example.com/pgoworkload
                       │  nopgo.txt  │             withpgo.txt              │
                       │   sec/op    │   sec/op     vs base                 │
RunJSONDominated-8       123.4µ ± 2%   118.2µ ± 1%  -4.21% (p=0.000 n=10)
Tokenize-8                45.6µ ± 1%    43.9µ ± 1%  -3.73% (p=0.002 n=10)
```

A 3–7% improvement is expected. The JSON-dominated benchmark benefits most because three of the four processors are `JSONProcessor`, which makes that call site strongly typed in the profile — the prime candidate for devirtualization.

**Step 5 — Inspect what changed.**

Compare inlining decisions with and without the profile:

```bash
go build -pgo=off -gcflags='-m' ./... 2>&1 | grep "can inline\|cannot inline\|inlining call" | head -20
go build          -gcflags='-m' ./... 2>&1 | grep "can inline\|cannot inline\|inlining call" | head -20
```

Functions inlined only in the PGO build appear in the second list but not the first. The `-m -m` (double `-m`) flag prints the full call graph, which shows why a function was or was not inlined and what the budget was.

**Step 6 — Understand when to re-profile.**

Add a field to `WordProcessor`, rename `Tokenize` to `TokenizeText`, or move `Run` to a different file, then repeat steps 2–4. Observe that the compiler gracefully degrades: samples that no longer match a function are dropped; only the unmatched portion of the budget is lost. After a significant refactor, step 2 should be repeated to rebuild the profile from the new hot paths.

Your turn: add a `BenchmarkTokenizeShort` that benchmarks `Tokenize` on a 10-word string. After generating a new profile with this benchmark included alongside `BenchmarkRunJSONDominated`, confirm that `Tokenize` shows a larger improvement because it now appears more prominently in the profile.

## Common Mistakes

### Placing default.pgo in the wrong directory

Wrong: placing `default.pgo` in a subdirectory (e.g., `profiles/default.pgo`) and expecting `go build` to find it automatically.

What happens: the build silently ignores the profile; `go version -m` shows no `-pgo` setting; benchmark results are identical to the no-PGO build.

Fix: place `default.pgo` in the directory containing the `package main` file. For a module with `cmd/app/main.go`, the file must be at `cmd/app/default.pgo`, not at the module root. Alternatively, pass `-pgo=/absolute/path/profile.pprof` explicitly.

### Profiling a microbenchmark that does not represent production

Wrong: running `-cpuprofile` on a benchmark that benchmarks a single small function (e.g., a 5-line string formatter), then using that profile for the entire binary.

What happens: the profile shows only one call site as hot; devirtualization fires only there; the rest of the program is unchanged; the measured speedup is small or zero.

Fix: profile a workload that exercises the same code paths as production. For a web service, a realistic HTTP load test is better than a unit benchmark. For batch processing, a full pipeline run is better than an isolated component benchmark.

### Comparing PGO benchmarks without -count=10 and benchstat

Wrong: running one benchmark pass with PGO and one without, and comparing the median ns/op by eye.

What happens: a 3% difference disappears in the noise of a single run; the learner concludes PGO has no effect and disables it.

Fix: always use `-count=10` (or `-count=6` minimum) and `benchstat` to get confidence intervals and p-values. A result with `p > 0.05` is not statistically significant regardless of the numeric difference.

### Forgetting that -pgo=off is needed for a true no-PGO baseline

Wrong: running the "no PGO" baseline after `default.pgo` already exists in the directory, without passing `-pgo=off`.

What happens: both baseline and PGO runs use the profile; the comparison shows 0% improvement and the learner concludes PGO is broken.

Fix: always use `go test -pgo=off` for the baseline run when `default.pgo` exists in the directory.

### Assuming PGO replaces other profiling work

Wrong: enabling PGO, seeing a 4% improvement, and stopping performance work there.

What happens: algorithmic hot spots (O(n^2) loops, excessive allocations, lock contention) remain; PGO cannot optimize them because the algorithm is the problem.

Fix: use `pprof` to find and fix algorithmic hot spots first. Apply PGO after the program is already well-optimized. PGO compounds with good algorithmic design; it does not substitute for it.

## Verification

From `~/go-exercises/pgoworkload`:

```bash
# Format
test -z "$(gofmt -l .)"

# Vet
go vet ./...

# Unit tests (no profile needed, no network)
go test -count=1 -race ./...

# Demo binary compiles
go build ./cmd/demo

# Confirm ExampleTokenize output matches
go test -run=ExampleTokenize -v ./...
```

For the full PGO workflow (requires a real run, not offline):

```bash
go test -bench=BenchmarkRunJSONDominated -benchtime=20s -cpuprofile=default.pgo
go test -pgo=off -bench=. -benchtime=3s -count=10 > nopgo.txt
go test          -bench=. -benchtime=3s -count=10 > withpgo.txt
benchstat nopgo.txt withpgo.txt
```

Expected: `go test -count=1 -race ./...` passes with no failures; `ExampleTokenize` output matches; benchstat shows a measurable improvement after the profile is applied.

## Summary

- PGO feeds a pprof CPU profile back to the compiler, enabling two key optimizations: extended inlining budget for hot callees, and devirtualization of interface call sites where one concrete type dominates.
- Since Go 1.21, placing `default.pgo` in the main package directory is sufficient; no build flag is needed.
- Disable with `-pgo=off`; override the path with `-pgo=/path/to/file.pprof`.
- Verify a binary was compiled with PGO via `go version -m ./binary | grep pgo`.
- Profile from production or a representative benchmark; microbenchmarks on isolated functions are poor profile sources.
- The compiler matches samples to source by function name and line offset, so ordinary edits outside hot functions do not break matching.
- Typical improvement: 2–14% on CPU-bound Go programs (Go 1.22 data).
- Always compare with `-count=10` and `benchstat`; single-run comparisons are unreliable.
- PGO applies to the entire program including stdlib and dependencies, not just the main package.

## What's Next

Next: [Compiler Devirtualization](../06-compiler-devirtualization/06-compiler-devirtualization.md).

## Resources

- [Profile-Guided Optimization in Go](https://go.dev/doc/pgo) — official documentation covering the workflow, `-pgo` flag, stability properties, and alternative profile sources.
- [Introducing Profile-Guided Optimization in Go 1.21](https://go.dev/blog/pgo) — the announcement post with the markdown-service case study, allocation elimination example, and devirtualization explanation.
- [PGO proposal (GitHub issue 55022)](https://github.com/golang/go/issues/55022) — the design discussion and rationale for the AutoFDO approach.
- [runtime/pprof](https://pkg.go.dev/runtime/pprof) — `StartCPUProfile(w io.Writer) error` and `StopCPUProfile()` for programmatic profile collection.
- [golang.org/x/perf/cmd/benchstat](https://pkg.go.dev/golang.org/x/perf/cmd/benchstat) — statistical benchmark comparison tool used to measure PGO improvements reliably.
