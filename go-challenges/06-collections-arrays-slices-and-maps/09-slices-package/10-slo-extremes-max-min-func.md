# Exercise 10: Compute Latency Extremes And Locate The Worst Endpoint (Max / Min / MaxFunc / IndexFunc)

An observability hook summarizing a window of per-request samples needs the min
and max latency, the single worst sample (which endpoint, which trace), and the
first endpoint that breached an SLO threshold. `slices.Min`/`Max` give the scalar
extremes, `slices.MaxFunc` finds the worst struct, and `slices.IndexFunc`/
`ContainsFunc` locate the first breach. The senior detail is the empty-window
contract: `Max`/`Min`/`MaxFunc`/`MinFunc` panic on an empty slice, so a metrics
path must guard length explicitly.

This module is fully self-contained: its own `go mod init`, all code inline, its
own demo and tests. Nothing here imports another exercise.

## What you'll build

```text
sloextremes/                   module example.com/sloextremes
  go.mod                       go 1.24
  slo.go                       type Sample; MinMax (guarded), Worst (MaxFunc), FirstBreach (IndexFunc)
  cmd/
    demo/
      main.go                  runnable demo: summarize a request window
  slo_test.go                  single/multi/all-equal, MaxFunc ties to first, empty guard vs raw panic, IndexFunc -1
```

- Files: `slo.go`, `cmd/demo/main.go`, `slo_test.go`.
- Implement: `MinMax([]Sample) (min, max time.Duration, ok bool)` guarding the empty window; `Worst([]Sample) (Sample, bool)` via `slices.MaxFunc`; `FirstBreach([]Sample, threshold) int` via `slices.IndexFunc`; `AnyBreach` via `slices.ContainsFunc`.
- Test: single, multiple, and all-equal samples; `MaxFunc` returns the first of tied worst; the empty window does not panic through the wrapper (and the raw `slices.Max` panic is shown via recover); `IndexFunc` returns -1 when nothing breaches and the first index when several do.
- Verify: `go test -count=1 -race ./...`

Set up the module:

```bash
go mod edit -go=1.24
```

### The empty-window panic is the contract to respect

`slices.Max(s)` and `slices.Min(s)` return the largest and smallest element of a
slice of ordered values; `slices.MaxFunc(s, cmp)` and `MinFunc(s, cmp)` do the same
with a comparator, returning the actual element. All four panic if the slice is
empty — there is no "maximum of nothing" to return, and Go chose a panic over a
zero value so the mistake is loud. In a metrics summary that runs on a possibly
empty window (a quiet minute, a cold start), an unguarded `slices.Max(samples)`
takes down the observability path. So the code guards: `MinMax` checks
`len(samples) == 0` and returns `ok = false` instead of calling into the package.
The test covers both the safe wrapper (no panic on empty) and, via `recover`, the
raw `slices.Max([]) ` panic it is shielding against.

`slices.MaxFunc(s, cmp)` finds the worst sample by latency. When several samples
tie for the maximum, `MaxFunc` returns the *first* one it encountered (the
documented behavior: it returns the first element for which the comparator reports
the maximum). That determinism matters for a summary — "the worst request" should
be a single, reproducible sample. The test builds tied worst samples and asserts
the first is chosen.

`slices.IndexFunc(s, pred)` returns the index of the first element satisfying the
predicate, or -1 if none does. `FirstBreach` uses it with "latency > threshold" to
find the first endpoint that violated the SLO; -1 means the whole window was
within budget. `slices.ContainsFunc(s, pred)` is the boolean form, used by
`AnyBreach` when you only need to know whether any breach occurred (it is
`IndexFunc(...) >= 0`, but reads clearer as a yes/no).

Create `slo.go`:

```go
package sloextremes

import (
	"cmp"
	"slices"
	"time"
)

// Sample is one request measurement in a window.
type Sample struct {
	Endpoint string
	Trace    string
	Latency  time.Duration
}

// MinMax returns the min and max latency in the window. ok is false for an empty
// window, avoiding the panic that slices.Min/Max raise on an empty slice.
func MinMax(samples []Sample) (minLat, maxLat time.Duration, ok bool) {
	if len(samples) == 0 {
		return 0, 0, false
	}
	lats := make([]time.Duration, len(samples))
	for i, s := range samples {
		lats[i] = s.Latency
	}
	return slices.Min(lats), slices.Max(lats), true
}

// Worst returns the single highest-latency sample. Ties resolve to the first
// such sample (MaxFunc's documented behavior). ok is false for an empty window.
func Worst(samples []Sample) (Sample, bool) {
	if len(samples) == 0 {
		return Sample{}, false
	}
	return slices.MaxFunc(samples, func(a, b Sample) int {
		return cmp.Compare(a.Latency, b.Latency)
	}), true
}

// FirstBreach returns the index of the first sample whose latency exceeds the
// threshold, or -1 if the whole window is within budget.
func FirstBreach(samples []Sample, threshold time.Duration) int {
	return slices.IndexFunc(samples, func(s Sample) bool {
		return s.Latency > threshold
	})
}

// AnyBreach reports whether any sample exceeded the threshold.
func AnyBreach(samples []Sample, threshold time.Duration) bool {
	return slices.ContainsFunc(samples, func(s Sample) bool {
		return s.Latency > threshold
	})
}
```

### The runnable demo

Create `cmd/demo/main.go`:

```go
package main

import (
	"fmt"
	"time"

	"example.com/sloextremes"
)

func main() {
	ms := time.Millisecond
	window := []sloextremes.Sample{
		{Endpoint: "/login", Trace: "t1", Latency: 12 * ms},
		{Endpoint: "/search", Trace: "t2", Latency: 340 * ms},
		{Endpoint: "/checkout", Trace: "t3", Latency: 88 * ms},
	}

	if lo, hi, ok := sloextremes.MinMax(window); ok {
		fmt.Printf("min=%v max=%v\n", lo, hi)
	}
	if w, ok := sloextremes.Worst(window); ok {
		fmt.Printf("worst: %s (%v)\n", w.Endpoint, w.Latency)
	}

	threshold := 100 * ms
	if i := sloextremes.FirstBreach(window, threshold); i >= 0 {
		fmt.Printf("first breach: %s at index %d\n", window[i].Endpoint, i)
	}
	fmt.Printf("any breach: %v\n", sloextremes.AnyBreach(window, threshold))

	// Empty window does not panic.
	_, _, ok := sloextremes.MinMax(nil)
	fmt.Printf("empty window ok: %v\n", ok)
}
```

Run it:

```bash
go run ./cmd/demo
```

Expected output:

```
min=12ms max=340ms
worst: /search (340ms)
first breach: /search at index 1
any breach: true
empty window ok: false
```

The window's extremes are 12ms and 340ms; the worst sample is `/search`; the first
endpoint over the 100ms threshold is `/search` at index 1; an empty window returns
`ok=false` instead of panicking.

### Tests

`TestMinMaxCases` covers single, multiple, and all-equal windows. `TestWorstTieToFirst`
proves `MaxFunc` returns the first of tied worst samples. `TestEmptyWindow` proves
the wrapper does not panic and that the raw `slices.Max` on an empty slice does
(via `recover`). `TestFirstBreach` pins -1 when nothing breaches and the first
index when several do.

Create `slo_test.go`:

```go
package sloextremes

import (
	"slices"
	"testing"
	"time"
)

func mkSamples(latsMS ...int) []Sample {
	out := make([]Sample, len(latsMS))
	for i, ms := range latsMS {
		out[i] = Sample{Endpoint: "/e", Latency: time.Duration(ms) * time.Millisecond}
	}
	return out
}

func TestMinMaxCases(t *testing.T) {
	t.Parallel()

	cases := []struct {
		name      string
		lats      []int
		wantMinMS int
		wantMaxMS int
	}{
		{"single", []int{50}, 50, 50},
		{"multiple", []int{50, 10, 90, 30}, 10, 90},
		{"all equal", []int{20, 20, 20}, 20, 20},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			lo, hi, ok := MinMax(mkSamples(tc.lats...))
			if !ok {
				t.Fatal("MinMax ok = false for non-empty window")
			}
			if lo != time.Duration(tc.wantMinMS)*time.Millisecond {
				t.Fatalf("min = %v, want %dms", lo, tc.wantMinMS)
			}
			if hi != time.Duration(tc.wantMaxMS)*time.Millisecond {
				t.Fatalf("max = %v, want %dms", hi, tc.wantMaxMS)
			}
		})
	}
}

func TestWorstTieToFirst(t *testing.T) {
	t.Parallel()

	samples := []Sample{
		{Endpoint: "/a", Latency: 90 * time.Millisecond},
		{Endpoint: "/b", Latency: 90 * time.Millisecond}, // tied worst
		{Endpoint: "/c", Latency: 10 * time.Millisecond},
	}
	w, ok := Worst(samples)
	if !ok {
		t.Fatal("Worst ok = false")
	}
	if w.Endpoint != "/a" {
		t.Fatalf("worst tie = %q, want /a (first of tied max)", w.Endpoint)
	}
}

func TestEmptyWindow(t *testing.T) {
	t.Parallel()

	if _, _, ok := MinMax(nil); ok {
		t.Fatal("MinMax(nil) ok = true, want false")
	}
	if _, ok := Worst(nil); ok {
		t.Fatal("Worst(nil) ok = true, want false")
	}

	// The raw package call panics on empty; the wrapper is what prevents it.
	func() {
		defer func() {
			if r := recover(); r == nil {
				t.Fatal("slices.Max on empty slice did not panic")
			}
		}()
		_ = slices.Max([]time.Duration{})
	}()
}

func TestFirstBreach(t *testing.T) {
	t.Parallel()

	threshold := 100 * time.Millisecond
	within := mkSamples(10, 20, 30)
	if i := FirstBreach(within, threshold); i != -1 {
		t.Fatalf("FirstBreach with no breach = %d, want -1", i)
	}
	if AnyBreach(within, threshold) {
		t.Fatal("AnyBreach = true for within-budget window")
	}

	breaching := mkSamples(10, 200, 30, 400)
	if i := FirstBreach(breaching, threshold); i != 1 {
		t.Fatalf("FirstBreach = %d, want 1 (first over threshold)", i)
	}
	if !AnyBreach(breaching, threshold) {
		t.Fatal("AnyBreach = false despite a breach")
	}
}
```

## Review

The summary is correct when the extremes match the window, the worst sample is the
first of any tied maxima, and an empty window returns `ok=false` rather than
panicking. The guard in `MinMax` and `Worst` is the whole point: `slices.Max`/`Min`
and their `Func` variants panic on empty input by design, and a metrics hook must
never let that reach production, so it checks length first — the `recover` test
documents exactly the panic being shielded. `MaxFunc`'s first-of-ties rule makes
"the worst request" reproducible. `IndexFunc` returning -1 is the sentinel for "no
breach"; `ContainsFunc` is its boolean twin. Run `go test -race`; each table case
owns its samples.

## Resources

- [`slices.Max`](https://pkg.go.dev/slices#Max) / [`slices.Min`](https://pkg.go.dev/slices#Min) — scalar extremes; panic on empty.
- [`slices.MaxFunc`](https://pkg.go.dev/slices#MaxFunc) — comparator extreme, first-of-ties.
- [`slices.IndexFunc`](https://pkg.go.dev/slices#IndexFunc) / [`slices.ContainsFunc`](https://pkg.go.dev/slices#ContainsFunc) — first match and boolean any-match.

---

Back to [00-concepts.md](00-concepts.md) | Next: [11-raft-log-conflict-splice-replace.md](11-raft-log-conflict-splice-replace.md)
