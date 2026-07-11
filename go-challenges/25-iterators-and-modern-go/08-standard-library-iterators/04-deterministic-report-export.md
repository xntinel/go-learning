# Exercise 4: Deterministic Report Export

A reporting job that reads a map of aggregates and writes a report has one job
beyond arithmetic: produce the same bytes every time it runs on the same input.
That property is what lets the output be committed as a golden file, diffed in
review, or cached under a checksum. This exercise builds a `report` package that
turns a `map[string]Stat` of per-service counters into a stable, reproducible
text report by funneling the map through `slices.Sorted(maps.Keys(...))` and
materializing the result with `slices.Collect`, and it ships a test that renders
the same input a thousand times and asserts every run is byte-identical.

This module is fully self-contained. It begins with its own `go mod init`,
defines every function it needs, and ships its own demo and tests. Nothing here
imports any other exercise.

## What you'll build

```text
report.go            Stat, orderedKeys, lines, Render, Totals
cmd/
  demo/
    main.go          render a service report and print it with a totals line
report_test.go       byte-stable rendering across runs, golden output, totals
```

- Files: `report.go`, `cmd/demo/main.go`, `report_test.go`.
- Implement: the `Stat` type, the generic `orderedKeys` helper, the `lines` iterator, `Render`, and `Totals`.
- Test: `report_test.go` asserts the rendered bytes are stable across a thousand runs, match a fixed golden string, and that `Totals` sums correctly while empty input renders empty.
- Verify: `go test -race ./...`

Set up the module:

```bash
mkdir -p deterministic-report-export/cmd/demo && cd deterministic-report-export
go mod init example.com/deterministic-report-export
```

### Why determinism is the whole point of an export job

An aggregate is almost always stored in a map: a counter keyed by service name,
region, status code, or user. A map is the right structure to accumulate into,
because each update is an O(1) keyed write. It is the wrong structure to read
out of in any context where the output is compared, because Go randomizes map
iteration order on every run by design. The moment a map feeds bytes that a
human diffs, a golden-file test asserts on, or a cache keys by content hash, the
randomization turns into nondeterminism, and the failure mode is the worst kind:
the code is correct, the test passes most of the time, and then a CI run shuffles
the keys and a golden comparison fails for no reason a reader can see in the
diff.

The fix is to impose order exactly once, at the boundary where the map becomes
output. `orderedKeys` is that boundary: `slices.Sorted(maps.Keys(m))` collects
the randomized key sequence and sorts it ascending in a single call, returning a
slice whose order is identical on every run. It is generic over any ordered key
type and any value type, so the same one-liner serves every export shape. Every
function that produces ordered output ranges over this slice, never over the raw
map, which is what guarantees the bytes are reproducible.

### The line producer and the single allocation point

`lines` is an `iter.Seq[string]` producer: it yields one fully formatted report
line per service, walking the services in the deterministic order that
`orderedKeys` provides. Because it is a lazy iterator rather than a slice, it
allocates nothing itself; it computes and yields each line on demand. It carries
the `!yield(line)` early-stop guard so that a consumer which breaks early halts
the walk, which keeps the producer reusable even though the consumer used here
drains it fully.

`Render` is that consumer. It calls `slices.Collect(lines(stats))` to drain the
line sequence into a `[]string` — the one and only allocation in the pipeline,
since the producer upstream allocates nothing — and joins the lines with a
newline into a single string. The result is reproducible by construction: the
same `stats` map yields the same sorted key order, the same per-line formatting,
and therefore the same bytes, which is precisely what makes the output safe to
commit as a golden file or store in a content-addressed cache. The error-rate
field is formatted with a fixed `%.4f` width so that floating-point rendering
cannot introduce run-to-run variation either.

`Totals` is the deliberate counterexample. It sums every service's counters by
ranging `maps.Values(stats)` directly, with no sorting, because a sum does not
depend on iteration order — addition is commutative, so the randomized order
that ruins a report leaves a total untouched. Keeping `Totals` unsorted while
`Render` sorts is the lesson in miniature: impose order only on the output that
depends on it, and pay nothing for the output that does not.

Create `report.go`:

```go
package report

import (
	"cmp"
	"fmt"
	"iter"
	"maps"
	"slices"
	"strings"
)

// Stat holds the aggregate counters collected for one service over a window.
type Stat struct {
	Requests int
	Errors   int
}

// orderedKeys returns the keys of m in ascending order. This is the single
// place determinism is imposed: maps.Keys yields in the runtime's randomized
// order, and slices.Sorted collects and sorts that sequence in one call.
func orderedKeys[K cmp.Ordered, V any](m map[K]V) []K {
	return slices.Sorted(maps.Keys(m))
}

// lines yields one formatted report line per service, in ascending service
// order. It is an iter.Seq[string] producer built on the deterministic key
// order, so the sequence it yields is identical on every run and it allocates
// nothing of its own.
func lines(stats map[string]Stat) iter.Seq[string] {
	return func(yield func(string) bool) {
		for _, name := range orderedKeys(stats) {
			s := stats[name]
			rate := 0.0
			if s.Requests > 0 {
				rate = float64(s.Errors) / float64(s.Requests)
			}
			line := fmt.Sprintf("%s\trequests=%d\terrors=%d\terror_rate=%.4f", name, s.Requests, s.Errors, rate)
			if !yield(line) {
				return
			}
		}
	}
}

// Render materializes the report into a single newline-joined string. It drains
// the deterministic line sequence with slices.Collect — the one allocation in
// the pipeline — so the bytes it returns are reproducible: identical inputs
// always render identical output, which is what makes the result safe to commit
// as a golden file or key a cache by.
func Render(stats map[string]Stat) string {
	return strings.Join(slices.Collect(lines(stats)), "\n")
}

// Totals sums the counters across every service. Order does not affect a sum,
// so it ranges maps.Values directly with no sorting.
func Totals(stats map[string]Stat) (requests, errors int) {
	for s := range maps.Values(stats) {
		requests += s.Requests
		errors += s.Errors
	}
	return requests, errors
}
```

### The runnable demo

The demo renders a three-service report and prints it, followed by an
order-independent totals line. Run it twice and the output is byte-for-byte
identical, which is the entire point of pushing the map through a sorting
consumer before it becomes text.

Create `cmd/demo/main.go`:

```go
package main

import (
	"fmt"

	"example.com/deterministic-report-export"
)

func main() {
	stats := map[string]report.Stat{
		"auth":    {Requests: 1200, Errors: 12},
		"billing": {Requests: 300, Errors: 9},
		"catalog": {Requests: 5000, Errors: 0},
	}

	fmt.Println(report.Render(stats))

	requests, errors := report.Totals(stats)
	fmt.Printf("TOTAL\trequests=%d\terrors=%d\n", requests, errors)
}
```

Run it:

```bash
go run ./cmd/demo
```

Expected output:

```
auth	requests=1200	errors=12	error_rate=0.0100
billing	requests=300	errors=9	error_rate=0.0300
catalog	requests=5000	errors=0	error_rate=0.0000
TOTAL	requests=6500	errors=21
```

### Tests

The tests pin the property the package exists to provide. `TestRenderIsByteStable`
renders the same input a thousand times and fails if any run differs from the
first — the assertion a raw map walk could not survive. `TestRenderGolden` pins
the exact bytes against a fixed string, the way a real golden-file test would.
`TestRenderEmpty` checks that an empty map renders an empty string rather than
panicking, and `TestTotals` checks the order-independent sum.

Create `report_test.go`:

```go
package report

import (
	"testing"
)

func sampleStats() map[string]Stat {
	return map[string]Stat{
		"auth":    {Requests: 1200, Errors: 12},
		"billing": {Requests: 300, Errors: 9},
		"catalog": {Requests: 5000, Errors: 0},
	}
}

func TestRenderIsByteStable(t *testing.T) {
	t.Parallel()

	stats := sampleStats()
	first := Render(stats)
	for i := 0; i < 1000; i++ {
		if got := Render(stats); got != first {
			t.Fatalf("run %d: Render drifted:\n%q\nvs\n%q", i, got, first)
		}
	}
}

func TestRenderGolden(t *testing.T) {
	t.Parallel()

	want := "auth\trequests=1200\terrors=12\terror_rate=0.0100\n" +
		"billing\trequests=300\terrors=9\terror_rate=0.0300\n" +
		"catalog\trequests=5000\terrors=0\terror_rate=0.0000"
	if got := Render(sampleStats()); got != want {
		t.Fatalf("Render =\n%q\nwant\n%q", got, want)
	}
}

func TestRenderEmpty(t *testing.T) {
	t.Parallel()

	if got := Render(nil); got != "" {
		t.Fatalf("Render(nil) = %q, want empty", got)
	}
}

func TestTotals(t *testing.T) {
	t.Parallel()

	if r, e := Totals(sampleStats()); r != 6500 || e != 21 {
		t.Fatalf("Totals = (%d, %d), want (6500, 21)", r, e)
	}
}
```

## Review

The package is correct when the only thing that orders the output is
`orderedKeys`, and the only thing that allocates is `slices.Collect`. `Render`
must never range over the raw `stats` map; the instant it does, the output stops
being reproducible and `TestRenderGolden` becomes a flake that passes locally and
fails in CI on a different map seed. Confirm `TestRenderIsByteStable` clears its
thousand runs — that is the direct proof of the property the rest of the package
is built to deliver, and it is the test a hand-written map-walk-and-format would
fail intermittently.

The subtle trap is the float field. Two services with the same integer counters
must format to the same string, so the rate is pinned to a fixed `%.4f` width;
printing it with `%v` or an unbounded precision would let the exact decimal
expansion vary the bytes and quietly break golden comparisons. The deliberate
asymmetry between `Render` and `Totals` is the conceptual takeaway: `Render`
sorts because its output is order-dependent text, while `Totals` skips sorting
because a sum is order-independent. Adding a sort to `Totals` would be harmless
but wasteful, and removing the sort from `Render` would be a correctness bug;
knowing which is which is the skill this exercise trains.

## Resources

- [`slices` package](https://pkg.go.dev/slices) — `Sorted` and `Collect`, the consumers that impose order and materialize the result exactly once.
- [`maps` package](https://pkg.go.dev/maps) — `Keys` and `Values`, the map producers feeding the sorting consumer and the order-independent sum.
- [`iter` package](https://pkg.go.dev/iter) — the `Seq` type the `lines` producer is built on and that every standard iterator shares.
- [Go Blog: Range Over Function Types](https://go.dev/blog/range-functions) — the iterator model behind `maps.Keys`, `slices.Sorted`, and `slices.Collect`.

---

Back to [00-concepts.md](00-concepts.md) | Previous: [03-text-iterators.md](03-text-iterators.md) | Next: [05-log-metrics-ingest.md](05-log-metrics-ingest.md)
