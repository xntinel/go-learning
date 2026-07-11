# Exercise 4: Modernizing a Legacy Codebase with the go fix Suite

Authoring `//go:fix inline` directives is the producer side. The consumer side —
the low-risk, high-fan-out refactor a senior is expected to drive safely — is
running the built-in modernizer suite across an existing codebase and gating
un-migrated code in CI. This exercise ships a small telemetry package in its
modernized form, shows the exact `go fix` transformations that produced it, and
gates the result with `go fix -diff ./...`.

This module is self-contained: the modernized package, a demo, behavioral tests,
and a build-tag-guarded CI-gate test. Nothing here imports another exercise.

## What you'll build

```text
telemetry/                     independent module: example.com/telemetry
  go.mod                       go 1.26
  metrics/
    metrics.go                 Summarize, Allowed, Line, Labels (modernized forms)
    metrics_test.go            behavioral tests + Example
    fix_gate_test.go           //go:build gofix_integration: asserts `go fix -diff ./...` is empty
  cmd/
    demo/
      main.go                  runnable demo
```

- Files: `metrics/metrics.go`, `cmd/demo/main.go`, `metrics/metrics_test.go`, `metrics/fix_gate_test.go`.
- Implement: `Summarize` (using `max`), `Allowed` (using `slices.Contains`), `Line` (using `fmt.Appendf`), `Labels` (using `for i := range n`) — the outputs of the `minmax`, `slicescontains`, `fmtappendf`, and `rangeint` modernizers.
- Test: behavioral tests proving the modernized forms are correct; an `Example`; a build-tag-guarded test asserting `go fix -diff ./...` is empty (the CI-clean state).
- Verify: `go test -count=1 -race ./...`, then `go fix -diff ./...` (empty).

Set up the module:

```bash
mkdir -p ~/go-exercises/telemetry/metrics ~/go-exercises/telemetry/cmd/demo
cd ~/go-exercises/telemetry
go mod init example.com/telemetry
go mod edit -go=1.26
```

### The suite, and why it is safe to run en masse

The revamped `go fix` ships a couple dozen modernizers, each an analyzer on the
same `go/analysis` framework as `go vet`. Each one both diagnoses an outdated
pattern and provides the fix, and the design intent is that the whole suite can be
applied at once without changing behavior. That is the property that makes a
repo-wide modernization a low-risk change: you are not hand-editing hundreds of
call sites and hoping, you are applying type-checked, behavior-preserving rewrites
and reviewing the diff.

The package below is written in the *modernized* forms. Each function corresponds
to one modernizer, and the next section shows the legacy code that `go fix` would
rewrite into it.

Create `metrics/metrics.go`:

```go
// Package metrics is a small telemetry helper written in modern Go idioms —
// the forms the go fix modernizer suite produces from legacy code.
package metrics

import (
	"fmt"
	"slices"
)

// Summary aggregates a batch of latency samples in milliseconds.
type Summary struct {
	Count int
	Max   int
}

// Summarize computes the count and maximum latency over samples. The running
// maximum uses the built-in max (the minmax modernizer).
func Summarize(samples []int) Summary {
	out := Summary{Count: len(samples)}
	for _, v := range samples {
		out.Max = max(out.Max, v)
	}
	return out
}

// Allowed reports whether code is in the allow-list, via slices.Contains (the
// slicescontains modernizer).
func Allowed(codes []int, code int) bool {
	return slices.Contains(codes, code)
}

// Line formats a metric line directly into bytes with fmt.Appendf, avoiding the
// intermediate string of []byte(fmt.Sprintf(...)) (the fmtappendf modernizer).
func Line(id int) []byte {
	return fmt.Appendf(nil, "metric#%d\n", id)
}

// Labels returns n indexed labels under a prefix. The loop counts with
// for i := range n (the rangeint modernizer).
func Labels(prefix string, n int) []string {
	out := make([]string, 0, n)
	for i := range n {
		out = append(out, fmt.Sprintf("%s-%d", prefix, i))
	}
	return out
}
```

### What go fix rewrote, modernizer by modernizer

Each modernizer takes a pre-1.21-style pattern to its modern equivalent. These are
the diffs `go fix ./...` would produce on the legacy version of each function.

`minmax` — a manual conditional maximum becomes the `max` built-in:

```text
-	for _, v := range samples {
-		if v > out.Max {
-			out.Max = v
-		}
-	}
+	for _, v := range samples {
+		out.Max = max(out.Max, v)
+	}
```

`slicescontains` — a hand-rolled search loop becomes `slices.Contains`:

```text
-	for _, c := range codes {
-		if c == code {
-			return true
-		}
-	}
-	return false
+	return slices.Contains(codes, code)
```

`fmtappendf` — allocating a string only to convert it becomes `fmt.Appendf`:

```text
-	return []byte(fmt.Sprintf("metric#%d\n", id))
+	return fmt.Appendf(nil, "metric#%d\n", id)
```

`rangeint` — a three-clause counting loop becomes a range over an integer:

```text
-	for i := 0; i < n; i++ {
+	for i := range n {
```

### Driving the suite: apply, select, disable, gate

The command surface mirrors `go vet`. From the module root:

```bash
# Apply every modernizer in place (start from a clean git state).
go fix ./...

# Preview without writing: the CI-gating form.
go fix -diff ./...

# Run a single modernizer.
go fix -rangeint ./...

# Run everything except one.
go fix -omitzero=false ./...

# List the registered analyzers, and read one's documentation.
go tool fix help
go tool fix help forvar
```

Two operational notes. First, one modernization can unlock another, so running
`go fix` twice (to a fixed point) is normal. Second, `-fixtool` lets you point
`go fix` at an alternative analysis tool built on the same unitchecker framework,
which is how a team ships its own house modernizers alongside the built-in suite.

### The runnable demo

Create `cmd/demo/main.go`:

```go
package main

import (
	"fmt"

	"example.com/telemetry/metrics"
)

func main() {
	s := metrics.Summarize([]int{12, 40, 7, 40, 3})
	fmt.Printf("count=%d max=%d\n", s.Count, s.Max)

	fmt.Printf("allowed(404)=%v\n", metrics.Allowed([]int{200, 201, 204}, 404))

	fmt.Printf("%s", metrics.Line(1001))

	fmt.Println(metrics.Labels("shard", 3))
}
```

Run it:

```bash
go run ./cmd/demo
```

Expected output:

```
count=5 max=40
allowed(404)=false
metric#1001
[shard-0 shard-1 shard-2]
```

### Proving the modernizers preserved semantics

The whole promise of the suite is behavior preservation, so the tests assert the
modernized forms compute the right answers. `Allowed` is checked against
`slices.Contains` directly to confirm the rewrite is faithful to the API it
adopted.

Create `metrics/metrics_test.go`:

```go
package metrics

import (
	"bytes"
	"fmt"
	"slices"
	"testing"
)

func TestSummarize(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name    string
		samples []int
		want    Summary
	}{
		{"empty", nil, Summary{Count: 0, Max: 0}},
		{"single", []int{7}, Summary{Count: 1, Max: 7}},
		{"many", []int{3, 9, 1, 9, 2}, Summary{Count: 5, Max: 9}},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			if got := Summarize(tc.samples); got != tc.want {
				t.Fatalf("Summarize(%v) = %+v, want %+v", tc.samples, got, tc.want)
			}
		})
	}
}

func TestAllowed(t *testing.T) {
	t.Parallel()
	codes := []int{200, 201, 204}
	if !Allowed(codes, 201) {
		t.Fatal("Allowed(codes, 201) = false, want true")
	}
	if Allowed(codes, 500) {
		t.Fatal("Allowed(codes, 500) = true, want false")
	}
	if Allowed(codes, 200) != slices.Contains(codes, 200) {
		t.Fatal("Allowed disagrees with slices.Contains")
	}
}

func TestLine(t *testing.T) {
	t.Parallel()
	got := Line(42)
	want := []byte("metric#42\n")
	if !bytes.Equal(got, want) {
		t.Fatalf("Line(42) = %q, want %q", got, want)
	}
}

func TestLabels(t *testing.T) {
	t.Parallel()
	got := Labels("cpu", 3)
	want := []string{"cpu-0", "cpu-1", "cpu-2"}
	if !slices.Equal(got, want) {
		t.Fatalf("Labels = %v, want %v", got, want)
	}
}

func Example() {
	fmt.Printf("%s", Line(7))
	// Output: metric#7
}
```

### Gating un-migrated code in CI

The CI check is `go fix -diff ./...`: it prints what it would change and writes
nothing, so a non-empty output means the tree still has un-modernized code. On the
already-modernized package here, the output is empty. The build-tag-guarded test
below encodes that gate; it shells out to the toolchain, so it is excluded from the
default `go test` run:

```bash
go test -tags gofix_integration ./metrics
```

Create `metrics/fix_gate_test.go`:

```go
//go:build gofix_integration

package metrics_test

import (
	"os/exec"
	"testing"
)

// TestModuleIsModernized asserts `go fix -diff ./...` produces no diff, i.e. the
// suite has no remaining work. On a legacy version of this package the diff would
// be non-empty and this test would fail — that is the CI gate.
func TestModuleIsModernized(t *testing.T) {
	cmd := exec.CommandContext(t.Context(), "go", "fix", "-diff", "./...")
	out, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("go fix -diff failed: %v\n%s", err, out)
	}
	if len(out) != 0 {
		t.Fatalf("modernizers still have work; go fix -diff is not empty:\n%s", out)
	}
}
```

## Review

The package is correct when every modernized form computes what its legacy
predecessor did: `TestSummarize`, `TestAllowed`, `TestLine`, and `TestLabels` pin
that down, and checking `Allowed` against `slices.Contains` confirms the rewrite
matches the API it adopted. Because the modernizers are behavior-preserving by
design, passing tests on the modernized code is the evidence that the refactor was
safe.

The operational mistakes to avoid: do not run `go fix ./...` in CI and let it
mutate the tree — gate with `go fix -diff ./...` and keep the actual rewrite a
reviewed developer or bot action against a clean git state. Run the suite twice to
catch fixes that unlock other fixes, and remember analyzers are selectable
(`go fix -rangeint ./...`) and disable-able (`go fix -omitzero=false ./...`) by
name just like `go vet`. Confirm behavior with `go test -race ./...`; where the
toolchain is available, run the tagged gate test to assert the module stays
modern.

## Resources

- [Using go fix to modernize Go code](https://go.dev/blog/gofix) — running the suite, selecting and disabling analyzers, and the `-diff` CI gate.
- [`modernize` analyzers](https://pkg.go.dev/golang.org/x/tools/go/analysis/passes/modernize) — the full list of modernizers and what each rewrites.
- [Go 1.26 Release Notes: go fix](https://go.dev/doc/go1.26) — the revamped command and its modernizer suite.

---

Back to [03-inline-constants-and-type-aliases.md](03-inline-constants-and-type-aliases.md) | Next: [../../49-application-security-crypto-supplychain/01-post-quantum-hybrid-tls/00-concepts.md](../../49-application-security-crypto-supplychain/01-post-quantum-hybrid-tls/00-concepts.md)
