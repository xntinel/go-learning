# 13. Performance Regression Testing

Performance regression testing turns benchmark output into a contract. The hard part is not running `go test -bench`; it is deciding what comparison data means, rejecting invalid thresholds, and making CI fail only for changes large enough to matter. This lesson builds a small library that compares benchmark summaries without shelling out to Git or requiring network access.

```text
benchguard/
  go.mod
  guard.go
  guard_test.go
  cmd/demo/main.go
```

The package accepts old and new benchmark measurements, classifies each benchmark as improved, unchanged, regressed, or missing, and returns a CI-friendly report.

## Concepts

### Benchmarks Need A Stable Contract

A benchmark function measures a named operation, but a regression gate compares distributions or summaries across time. This lesson uses a compact `Sample` type instead of raw `testing.B` output so the comparison logic can be unit-tested deterministically. Real systems can feed this logic from `benchstat`, stored CI artifacts, or parsed benchmark output.

### Thresholds Avoid Noise-Driven Failures

Small benchmark changes are often noise. A threshold such as five percent gives CI a policy: fail only when the slowdown is greater than the agreed budget. The threshold itself must be validated because negative or impossible thresholds make reports meaningless.

### Missing Benchmarks Are A Signal

If a benchmark disappears, the comparison should not silently pass. Renamed benchmarks and removed hot-path benchmarks can hide regressions. The report below records missing baselines and missing current results explicitly.

### Tests Should Exercise The Policy, Not The Machine

Unit tests for regression policy should be deterministic and fast. They should not depend on CPU speed, OS scheduling, or wall-clock timing. Actual benchmark functions still matter, but the policy package can be tested with fixed `NsPerOp` and `AllocsPerOp` values.

## Exercises

This package does not invoke Git or `benchstat`; it implements the comparison core that a CI wrapper can call after benchmark data has been collected.

### Exercise 1: Implement The Regression Classifier

Create `guard.go`:

```go
package benchguard

import (
	"errors"
	"fmt"
	"sort"
	"strings"
)

var (
	ErrInvalidThreshold = errors.New("threshold percent must be between 0 and 100")
	ErrEmptyBenchmark   = errors.New("benchmark name must not be empty")
	ErrInvalidSample    = errors.New("benchmark sample must have positive ns/op")
)

type Sample struct {
	Name        string
	NsPerOp     float64
	AllocsPerOp float64
}

type Status string

const (
	StatusImproved  Status = "IMPROVED"
	StatusUnchanged Status = "UNCHANGED"
	StatusRegressed Status = "REGRESSED"
	StatusMissing   Status = "MISSING"
)

type Result struct {
	Name          string
	OldNsPerOp    float64
	NewNsPerOp    float64
	ChangePercent float64
	Status        Status
}

type Report struct {
	ThresholdPercent float64
	Results          []Result
}

func Compare(oldSamples, newSamples []Sample, thresholdPercent float64) (Report, error) {
	if thresholdPercent < 0 || thresholdPercent > 100 {
		return Report{}, fmt.Errorf("compare benchmarks: %w: got %.2f", ErrInvalidThreshold, thresholdPercent)
	}

	oldByName, err := indexSamples(oldSamples)
	if err != nil {
		return Report{}, err
	}
	newByName, err := indexSamples(newSamples)
	if err != nil {
		return Report{}, err
	}

	names := make([]string, 0, len(oldByName)+len(newByName))
	seen := make(map[string]bool, len(oldByName)+len(newByName))
	for name := range oldByName {
		names = append(names, name)
		seen[name] = true
	}
	for name := range newByName {
		if !seen[name] {
			names = append(names, name)
		}
	}
	sort.Strings(names)

	report := Report{ThresholdPercent: thresholdPercent}
	for _, name := range names {
		oldSample, hasOld := oldByName[name]
		newSample, hasNew := newByName[name]
		if !hasOld || !hasNew {
			report.Results = append(report.Results, Result{Name: name, Status: StatusMissing})
			continue
		}

		change := ((newSample.NsPerOp - oldSample.NsPerOp) / oldSample.NsPerOp) * 100
		status := StatusUnchanged
		if change > thresholdPercent {
			status = StatusRegressed
		} else if change < -thresholdPercent {
			status = StatusImproved
		}
		report.Results = append(report.Results, Result{
			Name:          name,
			OldNsPerOp:    oldSample.NsPerOp,
			NewNsPerOp:    newSample.NsPerOp,
			ChangePercent: change,
			Status:        status,
		})
	}
	return report, nil
}

func (r Report) HasRegression() bool {
	for _, result := range r.Results {
		if result.Status == StatusRegressed || result.Status == StatusMissing {
			return true
		}
	}
	return false
}

func (r Report) Summary() string {
	var b strings.Builder
	for _, result := range r.Results {
		if result.Status == StatusMissing {
			fmt.Fprintf(&b, "%s MISSING\n", result.Name)
			continue
		}
		fmt.Fprintf(&b, "%s %s %.1f%%\n", result.Name, result.Status, result.ChangePercent)
	}
	return strings.TrimRight(b.String(), "\n")
}

func indexSamples(samples []Sample) (map[string]Sample, error) {
	indexed := make(map[string]Sample, len(samples))
	for _, sample := range samples {
		if sample.Name == "" {
			return nil, fmt.Errorf("index benchmarks: %w", ErrEmptyBenchmark)
		}
		if sample.NsPerOp <= 0 {
			return nil, fmt.Errorf("index benchmark %s: %w", sample.Name, ErrInvalidSample)
		}
		indexed[sample.Name] = sample
	}
	return indexed, nil
}
```

`Compare` is intentionally deterministic. It sorts names so reports are stable, wraps validation errors with `%w`, and treats missing benchmark names as CI failures through `HasRegression`.

### Exercise 2: Test The Policy And Example Output

Create `guard_test.go`:

```go
package benchguard

import (
	"errors"
	"fmt"
	"strings"
	"testing"
)

func TestCompareClassifiesResults(t *testing.T) {
	t.Parallel()

	oldSamples := []Sample{
		{Name: "BenchmarkEncode", NsPerOp: 100, AllocsPerOp: 2},
		{Name: "BenchmarkParse", NsPerOp: 200, AllocsPerOp: 1},
		{Name: "BenchmarkRoute", NsPerOp: 300, AllocsPerOp: 0},
	}
	newSamples := []Sample{
		{Name: "BenchmarkEncode", NsPerOp: 104, AllocsPerOp: 2},
		{Name: "BenchmarkParse", NsPerOp: 230, AllocsPerOp: 1},
		{Name: "BenchmarkRoute", NsPerOp: 260, AllocsPerOp: 0},
	}

	report, err := Compare(oldSamples, newSamples, 5)
	if err != nil {
		t.Fatal(err)
	}

	got := map[string]Status{}
	for _, result := range report.Results {
		got[result.Name] = result.Status
	}
	want := map[string]Status{
		"BenchmarkEncode": StatusUnchanged,
		"BenchmarkParse":  StatusRegressed,
		"BenchmarkRoute":  StatusImproved,
	}
	for name, status := range want {
		if got[name] != status {
			t.Fatalf("%s status = %s, want %s", name, got[name], status)
		}
	}
	if !report.HasRegression() {
		t.Fatal("HasRegression() = false, want true")
	}
}

func TestCompareValidationErrors(t *testing.T) {
	t.Parallel()

	cases := []struct {
		name      string
		old       []Sample
		new       []Sample
		threshold float64
		want      error
	}{
		{name: "bad threshold", threshold: -1, want: ErrInvalidThreshold},
		{name: "empty name", old: []Sample{{Name: "", NsPerOp: 1}}, threshold: 5, want: ErrEmptyBenchmark},
		{name: "bad sample", old: []Sample{{Name: "BenchmarkParse", NsPerOp: 0}}, threshold: 5, want: ErrInvalidSample},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()

			_, err := Compare(tc.old, tc.new, tc.threshold)
			if !errors.Is(err, tc.want) {
				t.Fatalf("Compare() error = %v, want %v", err, tc.want)
			}
		})
	}
}

func TestCompareReportsMissingBenchmarks(t *testing.T) {
	t.Parallel()

	report, err := Compare(
		[]Sample{{Name: "BenchmarkParse", NsPerOp: 100}},
		[]Sample{{Name: "BenchmarkEncode", NsPerOp: 100}},
		5,
	)
	if err != nil {
		t.Fatal(err)
	}
	if len(report.Results) != 2 {
		t.Fatalf("results = %d, want 2", len(report.Results))
	}
	if !report.HasRegression() {
		t.Fatal("missing benchmarks should fail the gate")
	}
	if !strings.Contains(report.Summary(), "BenchmarkParse MISSING") {
		t.Fatalf("summary = %q", report.Summary())
	}
}

func TestSummaryIsStable(t *testing.T) {
	t.Parallel()

	report, err := Compare(
		[]Sample{{Name: "BenchmarkB", NsPerOp: 100}, {Name: "BenchmarkA", NsPerOp: 100}},
		[]Sample{{Name: "BenchmarkB", NsPerOp: 101}, {Name: "BenchmarkA", NsPerOp: 90}},
		5,
	)
	if err != nil {
		t.Fatal(err)
	}
	want := "BenchmarkA IMPROVED -10.0%\nBenchmarkB UNCHANGED 1.0%"
	if got := report.Summary(); got != want {
		t.Fatalf("Summary() = %q, want %q", got, want)
	}
}

func ExampleCompare() {
	report, _ := Compare(
		[]Sample{{Name: "BenchmarkParse", NsPerOp: 100}},
		[]Sample{{Name: "BenchmarkParse", NsPerOp: 112}},
		5,
	)
	fmt.Println(report.Summary())
	// Output: BenchmarkParse REGRESSED 12.0%
}
```

The tests are table-driven where validation behavior varies, and every test uses `t.Parallel`. The example is executed by `go test`, so the documented summary format cannot drift silently.

### Exercise 3: Add A Demo That Uses Only Exported API

Create `cmd/demo/main.go`:

```go
package main

import (
	"fmt"
	"log"

	"benchguard"
)

func main() {
	report, err := benchguard.Compare(
		[]benchguard.Sample{{Name: "BenchmarkParse", NsPerOp: 100}},
		[]benchguard.Sample{{Name: "BenchmarkParse", NsPerOp: 108}},
		5,
	)
	if err != nil {
		log.Fatal(err)
	}

	fmt.Println(report.Summary())
	if report.HasRegression() {
		fmt.Println("performance gate failed")
	}
}
```

The demo is small because the package is deliberately focused: compare already-collected benchmark summaries and return a deterministic gate decision.

## Common Mistakes

### Comparing A Single Benchmark Run As Truth

Wrong: fail CI because one benchmark run is five percent slower. A single run can be dominated by scheduler noise, CPU scaling, or a busy machine.

Fix: collect repeated benchmark data and feed summarized results into a deterministic policy. Use tools such as `benchstat` for statistical comparison before applying a CI threshold.

### Letting Missing Benchmarks Pass

Wrong: compare only names present in both old and new output. A removed benchmark disappears from the report and the gate passes.

Fix: report the union of benchmark names. This lesson marks missing names as `MISSING` and treats them as gate failures.

### Testing The Shell Wrapper Instead Of The Policy

Wrong: make every unit test run Git commands and real benchmarks. The tests become slow and machine-dependent.

Fix: test the comparison policy with fixed samples. Keep end-to-end benchmark collection as a separate CI job.

## Verification

Run this from `~/go-exercises/benchguard`:

```bash
test -z "$(gofmt -l .)"
go vet ./...
go build ./...
go test -count=1 -race ./...
```

Then add one more validation test for a threshold greater than `100` and assert `errors.Is(err, ErrInvalidThreshold)`.

## Summary

- Performance gates need deterministic policy code in addition to benchmark functions.
- Thresholds prevent noisy measurements from failing CI for meaningless changes.
- Missing benchmark names should be reported because they can hide regressions.
- Validation errors should be sentinel errors wrapped with `%w` and asserted with `errors.Is`.
- Unit tests should exercise the regression policy without depending on local machine speed.

## What's Next

Next: [Optimizing a Real-World Hot Path](../14-optimizing-a-real-world-hot-path/14-optimizing-a-real-world-hot-path.md).

## Resources

- [testing package: benchmarks and examples](https://pkg.go.dev/testing)
- [benchstat documentation](https://pkg.go.dev/golang.org/x/perf/cmd/benchstat)
- [Go command testing flags](https://pkg.go.dev/cmd/go#hdr-Testing_flags)
- [Go Diagnostics](https://go.dev/doc/diagnostics)
