# Exercise 15: Error-Budget Histogram With a NaN-Key Guard

**Nivel: Avanzado** — validacion normal (tabla de casos, incluye borde o concurrencia).

An SRE error-budget dashboard, in the shape of a Prometheus recording rule
that tracks `errors / requests` per service over a sliding window, buckets
services by how much of their error budget they are burning: which services
are at or above 10%, which are past 50%, which are effectively down. The
ratio itself is the obvious thing to compute -- `float64(errors) /
float64(total)` -- and the obvious next step is to use it directly, since a
ratio *is* what the dashboard wants to bucket on. That obvious step is where
this design goes wrong, because one input this rule sees constantly is a
service with zero traffic in the window: nothing called it, so `total` is
`0`, and `errors / total` is `0.0 / 0.0`, which IEEE 754 defines as `NaN` --
not an error, not a panic, a perfectly ordinary float64 value that silently
propagates through every calculation that touches it.

The trap springs the moment that `NaN` becomes a map key. Go map keys are
compared with `==`, and IEEE 754 defines `NaN == NaN` as `false` -- a `NaN`
is unequal to every value, including another `NaN`, including itself. A map
entry stored under a `NaN` key is not merely hard to find later; it is
provably unreachable by any lookup a caller could ever construct, because no
expression in the language produces a `NaN` that compares equal to another
one. `len(m)` still counts it. `range` still visits it. `m[anyNaN]` never
finds it. It is a permanent, silent leak that looks, from every angle except
a direct lookup, like a normal entry.

The fix is not a comparison trick -- there isn't one, `NaN` genuinely cannot
be looked up by value -- it is never letting a ratio that could be `NaN`
reach a map key in the first place. This module builds an `errbudget`
histogram that quantizes each service's ratio into an integer percentage
bucket in `[0,100]` before anything touches a map, and that explicitly
checks `total == 0` before the division even happens, so a zero-traffic
service is guarded out rather than silently misrepresented as "0% error,
fully healthy."

This module is fully self-contained: its own `go mod init`, a reusable
package, and its tests. Nothing here imports another exercise.

## What you'll build

```text
errbudget/                module example.com/errbudget
  go.mod                   go 1.24
  errbudget.go              Histogram; NewHistogram, Record, Bucket, Len
  errbudget_test.go         bucketing table, overwrite, the zero-traffic guard, clamping,
                            the NaN-key contrast, concurrency, ExampleHistogram_Bucket
```

- Files: `errbudget.go`, `errbudget_test.go`.
- Implement: `NewHistogram() *Histogram`; `(*Histogram) Record(service string, errorsCount, total int)`, guarding `total == 0` before any division and otherwise storing `service`'s ratio quantized to `clamp(int(ratio*100), 0, 100)`, replacing any previous value; `(*Histogram) Bucket(pctThreshold int) int`, clamping `pctThreshold` to `[0,100]` and returning the count of recorded services at or above it; `(*Histogram) Len() int`.
- Test: a bucketing table across several thresholds; re-recording a service overwrites rather than duplicates it; a zero-traffic `Record` leaves the service entirely unrecorded, not recorded at bucket 0; a threshold below 0 or above 100 clamps correctly, as does an `errorsCount` outside `[0, total]`; the unexported `recordNaiveFloat` contrast keys a raw `float64` ratio directly and proves the resulting `NaN`-keyed entry can never be retrieved, by any lookup, while still occupying the map; `Histogram` is safe for concurrent use; and `ExampleHistogram_Bucket` as the runnable demonstration.
- Verify: `go test -count=1 -race ./...`

Set up the module:

```bash
mkdir -p go-solutions/06-collections-arrays-slices-and-maps/14-custom-map-based-data-structure/15-error-ratio-histogram-nan-guard
cd go-solutions/06-collections-arrays-slices-and-maps/14-custom-map-based-data-structure/15-error-ratio-histogram-nan-guard
go mod edit -go=1.24
```

### NaN != NaN makes a NaN-keyed map entry permanently unreachable

A map key type must be comparable, and `float64` is comparable -- the
language lets you write `map[float64]int` without complaint. What the type
system does not check is whether every value of that type behaves the way
`==` is supposed to behave for a key: reflexively, so that a value always
equals itself. `NaN` breaks that. IEEE 754 defines every comparison
involving `NaN` as false, `NaN == NaN` included, which is mathematically
correct -- "not a number" cannot be said to equal anything, itself included
-- and operationally catastrophic for a hash-table key, because the whole
point of `m[k]` is finding the entry whose stored key equals `k`.

```go
// recordNaiveFloat — the version that reaches for the ratio directly.
func recordNaiveFloat(m map[float64]int, errorsCount, total int) {
    ratio := float64(errorsCount) / float64(total)  // total == 0 -> NaN
    m[ratio]++
}
```

Call this for a zero-traffic service and it does not panic, does not error,
and does not even look wrong: `m[NaN()]++` runs exactly like any other map
write, `len(m)` grows by one, the entry is really there. The failure shows
up only on the next read. Every subsequent computation of `0.0 / 0.0` --
whether it is the exact same expression run again, or `math.NaN()` called
directly -- produces a `NaN` value that is, bit-for-bit, potentially
different, and even if it were identical, `==` would still report `false`
against it. There is no key a caller can construct, ever, that retrieves
that entry. It sits in the map forever, correctly counted by `len`, silently
invisible to every lookup.

`Histogram.Record` avoids the trap by never letting the computation that
could produce `NaN` happen unguarded: it checks `total == 0` *before*
dividing, and quantizes whatever ratio does get computed into an `int`
bucket before it ever becomes a key. Integers have no `NaN`; `int(ratio*100)`
after the guard is always a well-defined, comparable, retrievable value.

Create `errbudget.go`:

```go
// Package errbudget buckets services by their error rate for an SRE
// error-budget dashboard, in the shape of a Prometheus recording rule that
// tracks errors-over-requests per service across a sliding window.
//
// Every ratio is quantized into an integer percentage bucket in [0,100]
// before it is ever used as a map key, so the map is always keyed on a
// small, comparable, well-behaved int -- never on the raw float64 ratio,
// which can be NaN for a service with no traffic in the window.
package errbudget

import "sync"

// Histogram tracks the most recently recorded error-rate bucket for each
// service.
//
// Histogram is safe for concurrent use by multiple goroutines: every
// method takes the internal lock for the duration of its map access.
type Histogram struct {
	mu      sync.RWMutex
	buckets map[string]int
}

// NewHistogram returns an empty, ready-to-use Histogram.
func NewHistogram() *Histogram {
	return &Histogram{buckets: make(map[string]int)}
}

// Record sets service's current error-rate bucket from errorsCount errors
// observed out of total requests in the window, replacing any previous
// value for service. The ratio errorsCount/total is quantized to an
// integer percentage and clamped to [0,100] before it is used as a map
// key -- errorsCount above total or below zero (a stale or corrected
// count) lands on 100 or 0 respectively rather than producing an
// out-of-range bucket.
//
// A window with zero traffic (total == 0) produces an undefined 0/0
// ratio. Record guards that case explicitly, before any division happens,
// and leaves service unrecorded for this call rather than computing a
// ratio at all: a service with no traffic has no error rate to report this
// window, and silently recording it at 0% would misrepresent "no data" as
// "fully healthy".
func (h *Histogram) Record(service string, errorsCount, total int) {
	if total == 0 {
		return
	}
	ratio := float64(errorsCount) / float64(total)
	bucket := clamp(int(ratio*100), 0, 100)

	h.mu.Lock()
	defer h.mu.Unlock()
	h.buckets[service] = bucket
}

// Bucket reports how many recorded services currently have an error-rate
// bucket at or above pctThreshold: the count of services burning at least
// that much of their error budget this window. pctThreshold is clamped to
// [0,100] before comparison, so a threshold above 100 always reports zero
// and one at or below 0 reports every recorded service. A service Record
// left unrecorded because it had zero traffic never contributes to any
// threshold's count.
func (h *Histogram) Bucket(pctThreshold int) int {
	pctThreshold = clamp(pctThreshold, 0, 100)

	h.mu.RLock()
	defer h.mu.RUnlock()
	count := 0
	for _, b := range h.buckets {
		if b >= pctThreshold {
			count++
		}
	}
	return count
}

// Len reports the number of services with a currently recorded bucket.
// A service that has only ever been Recorded with total == 0 is not
// counted here.
func (h *Histogram) Len() int {
	h.mu.RLock()
	defer h.mu.RUnlock()
	return len(h.buckets)
}

// clamp restricts v to [lo, hi].
func clamp(v, lo, hi int) int {
	if v < lo {
		return lo
	}
	if v > hi {
		return hi
	}
	return v
}
```

### Using it

Call `Record` once per service each time a new window's counts are
available -- a Prometheus scrape interval, a batch rollup, whatever cadence
the dashboard runs on -- and `Record` replaces that service's bucket rather
than accumulating across calls, so `Bucket` always reflects the most recent
window. Query `Bucket(pctThreshold)` for however many threshold lines the
dashboard renders (10%, 50%, 90%, whatever the error-budget policy defines).
A service reported with zero traffic simply does not appear in any
threshold's count, which is the guarantee `Len` exposes for a caller that
wants to distinguish "no services are unhealthy" from "no services reported
any traffic at all."

The `Example` below is the runnable demonstration of this module: `go test`
executes it and compares its standard output against the `// Output:`
comment, so the usage shown here cannot drift from the code that actually
runs.

```go
func ExampleHistogram_Bucket() {
	h := NewHistogram()
	h.Record("checkout", 5, 100)
	h.Record("search", 40, 100)
	h.Record("payments", 90, 100)
	h.Record("idle-service", 0, 0) // zero traffic: guarded, never recorded

	fmt.Println("services tracked:", h.Len())
	fmt.Println("at or above 10%:", h.Bucket(10))
	fmt.Println("at or above 50%:", h.Bucket(50))
	fmt.Println("at or above 0%:", h.Bucket(0))

	// Output:
	// services tracked: 3
	// at or above 10%: 2
	// at or above 50%: 1
	// at or above 0%: 3
}
```

### Tests

`TestRecordAndBucketBasic` pins the ordinary bucketing behavior across
several thresholds. `TestRecordOverwritesPreviousValue` checks that
re-recording a service updates its bucket in place rather than creating a
second entry. `TestZeroTrafficServiceIsGuarded` is one half of the module's
core claim: a `Record` with `total == 0` must leave the service out of
`Len` entirely, not silently present it at bucket 0, which is what
distinguishes "guarded" from merely "didn't crash."
`TestBucketClampsThreshold` and `TestRecordClampsOutOfRangeRatio` cover the
boundary arithmetic on both sides of the API.

`TestNaNKeyIsPermanentlyUnreachable` is the module's other, and more
important, core test. `recordNaiveFloat` is unexported and unreachable from
the package API; it exists so the test can demonstrate, concretely, the
trap `Histogram`'s guard exists to avoid: a `map[float64]int` keyed on a
raw `0/0` ratio stores an entry that `len` counts but that no lookup --
including one built from an independently computed `NaN`, including
`math.NaN()` itself -- can ever retrieve again. `TestHistogramIsSafeForConcurrentUse`
runs twenty goroutines each recording and querying their own service under
`-race`.

Create `errbudget_test.go`:

```go
package errbudget

import (
	"fmt"
	"math"
	"sync"
	"testing"
)

// recordNaiveFloat is the histogram key as it is usually written the first
// time: bucket services directly by their raw float64 error ratio, with no
// guard for zero traffic. It is never exported and never reachable from
// the package API; it exists only so the tests can pin the trap it falls
// into for a zero-traffic service.
func recordNaiveFloat(m map[float64]int, errorsCount, total int) {
	ratio := float64(errorsCount) / float64(total) // total == 0 -> NaN
	m[ratio]++
}

func TestRecordAndBucketBasic(t *testing.T) {
	t.Parallel()

	h := NewHistogram()
	h.Record("checkout", 5, 100)  // 5%
	h.Record("search", 40, 100)   // 40%
	h.Record("payments", 90, 100) // 90%

	tests := []struct {
		threshold int
		want      int
	}{
		{threshold: 0, want: 3},
		{threshold: 10, want: 2},
		{threshold: 50, want: 1},
		{threshold: 91, want: 0},
	}
	for _, tc := range tests {
		if got := h.Bucket(tc.threshold); got != tc.want {
			t.Errorf("Bucket(%d) = %d, want %d", tc.threshold, got, tc.want)
		}
	}
}

func TestRecordOverwritesPreviousValue(t *testing.T) {
	t.Parallel()

	h := NewHistogram()
	h.Record("checkout", 5, 100) // 5%
	if got := h.Bucket(10); got != 0 {
		t.Fatalf("Bucket(10) = %d, want 0 before the update", got)
	}

	h.Record("checkout", 80, 100) // 80%, replacing the earlier value
	if got := h.Bucket(10); got != 1 {
		t.Fatalf("Bucket(10) = %d, want 1 after the update", got)
	}
	if got := h.Len(); got != 1 {
		t.Fatalf("Len() = %d, want 1; re-recording checkout must not create a second entry", got)
	}
}

// TestZeroTrafficServiceIsGuarded pins the explicit total == 0 guard: a
// service reported with no traffic must not appear as a recorded entry at
// all -- not at bucket 0 (which would misrepresent "no data" as "fully
// healthy"), and not at any other bucket either.
func TestZeroTrafficServiceIsGuarded(t *testing.T) {
	t.Parallel()

	h := NewHistogram()
	h.Record("idle-service", 0, 0)

	if got := h.Len(); got != 0 {
		t.Fatalf("Len() = %d, want 0; a zero-traffic Record must not create an entry", got)
	}
	if got := h.Bucket(0); got != 0 {
		t.Fatalf("Bucket(0) = %d, want 0; the zero-traffic service must not count as healthy-at-0%%", got)
	}
}

func TestBucketClampsThreshold(t *testing.T) {
	t.Parallel()

	h := NewHistogram()
	h.Record("checkout", 5, 100)

	if got := h.Bucket(-10); got != 1 {
		t.Fatalf("Bucket(-10) = %d, want 1 (clamped to 0, so every recorded service counts)", got)
	}
	if got := h.Bucket(200); got != 0 {
		t.Fatalf("Bucket(200) = %d, want 0 (clamped to 100, unreachable by a 5%% service)", got)
	}
}

func TestRecordClampsOutOfRangeRatio(t *testing.T) {
	t.Parallel()

	h := NewHistogram()
	h.Record("over-reported", 150, 100) // errors > total: a stale/corrected count
	h.Record("negative", -5, 100)       // a corrected count going the other way

	if got := h.Bucket(100); got != 1 {
		t.Fatalf("Bucket(100) = %d, want 1; over-reported must clamp to exactly 100", got)
	}
	if got := h.Bucket(1); got != 1 {
		t.Fatalf("Bucket(1) = %d, want 1; negative must clamp to 0 and drop out of every positive threshold", got)
	}
}

// TestNaNKeyIsPermanentlyUnreachable is the heart of the module. It
// demonstrates the trap Histogram's total == 0 guard exists to avoid: a
// map keyed directly on a raw float64 ratio stores an entry under NaN for
// a zero-traffic service, and because NaN != NaN for any two NaN values --
// even the same 0/0 computed twice -- that entry can never be looked up
// again by any key a caller could construct.
func TestNaNKeyIsPermanentlyUnreachable(t *testing.T) {
	t.Parallel()

	m := map[float64]int{}
	recordNaiveFloat(m, 0, 0) // a zero-traffic service: 0/0 is NaN

	if len(m) != 1 {
		t.Fatalf("len(m) = %d, want 1; the NaN entry was stored", len(m))
	}

	zero := 0
	lookupRatio := float64(zero) / float64(zero) // an independently computed NaN
	if _, ok := m[lookupRatio]; ok {
		t.Fatal("looked up the NaN-keyed entry with a freshly computed NaN; NaN != NaN should have prevented this")
	}
	if _, ok := m[math.NaN()]; ok {
		t.Fatal("looked up the NaN-keyed entry via math.NaN(); it should be permanently unreachable")
	}

	// The entry is not gone -- len(m) still reports it -- it is simply
	// unreachable by any lookup, which is exactly the permanent-leak shape
	// Histogram's total == 0 guard exists to prevent.
	if len(m) != 1 {
		t.Fatalf("len(m) after the failed lookups = %d, want 1 (the entry still occupies the map)", len(m))
	}
}

func TestHistogramIsSafeForConcurrentUse(t *testing.T) {
	t.Parallel()

	h := NewHistogram()
	var wg sync.WaitGroup
	for i := range 20 {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			service := fmt.Sprintf("service-%d", i)
			h.Record(service, i, 100)
			h.Bucket(i)
		}(i)
	}
	wg.Wait()

	if got := h.Len(); got != 20 {
		t.Fatalf("Len() = %d, want 20", got)
	}
}

// ExampleHistogram_Bucket is the runnable demonstration of this module: go
// test executes it and compares its stdout against the Output comment
// below.
func ExampleHistogram_Bucket() {
	h := NewHistogram()
	h.Record("checkout", 5, 100)
	h.Record("search", 40, 100)
	h.Record("payments", 90, 100)
	h.Record("idle-service", 0, 0) // zero traffic: guarded, never recorded

	fmt.Println("services tracked:", h.Len())
	fmt.Println("at or above 10%:", h.Bucket(10))
	fmt.Println("at or above 50%:", h.Bucket(50))
	fmt.Println("at or above 0%:", h.Bucket(0))

	// Output:
	// services tracked: 3
	// at or above 10%: 2
	// at or above 50%: 1
	// at or above 0%: 3
}
```

## Review

`Histogram` is correct when a service with no traffic in the window never
shows up as a recorded entry at any bucket, including 0 -- silence is not
the same claim as "0% error rate", and treating it that way would hide an
outage behind a healthy-looking dashboard tile. The trap `total == 0`
creates is worse than a wrong value: keying a map directly on
`errors/total` turns that one input into a `NaN` key, and `NaN != NaN`
makes the resulting entry provably unreachable forever, which
`TestNaNKeyIsPermanentlyUnreachable` demonstrates against the unexported,
unreachable `recordNaiveFloat`. `Record`'s guard -- checking `total == 0`
*before* the division, and quantizing every surviving ratio into a clamped
`int` bucket -- means no float, `NaN` or otherwise, is ever a map key in
this package. `Bucket` clamps its own threshold the same way, so a caller
passing an out-of-range value gets a well-defined answer instead of an
arithmetic surprise. `Histogram` guards its map with an internal
`RWMutex`, so `Record` and `Bucket` are safe to call from many goroutines
at once. Run `go test -count=1 -race ./...` to confirm the bucketing table,
the zero-traffic guard, the clamping cases, the `NaN`-key contrast, and the
concurrent-use test.

## Resources

- [IEEE 754: NaN comparisons](https://pkg.go.dev/math#IsNaN) — `math.IsNaN` and the standard library's own acknowledgment that `NaN` cannot be compared with `==`.
- [Go Spec: Comparison operators](https://go.dev/ref/spec#Comparison_operators) — map keys must be comparable, and `NaN`'s comparison behavior is exactly why a `NaN`-valued key is unreachable.
- [Prometheus: Recording rules](https://prometheus.io/docs/prometheus/latest/configuration/recording_rules/) — the production pattern (`errors_total / requests_total`) this histogram's domain is modeled on.
- [Go Wiki: CodeReviewComments](https://go.dev/wiki/CodeReviewComments) — general guidance on guarding division and other partial operations before they run, not after.

---

Back to [00-concepts.md](00-concepts.md) | Prev: [14-conn-stats-pointer-map.md](14-conn-stats-pointer-map.md) | Next: [16-rib-import-sizehint-tool.md](16-rib-import-sizehint-tool.md)
