# Exercise 2: Aggregate Access-Log Records into Deterministic Exposition Text

A metrics endpoint aggregates request events into counters keyed by a label set, then
renders them as text a Prometheus scraper reads. If the render ranges the counter map
directly, every scrape reorders the lines and every golden-file test flaps. This
module builds the aggregate-in-a-map, emit-via-sorted-keys pattern with the modern
iterator API.

This module is fully self-contained: its own `go mod init`, all code inline, its own
demo and tests. Nothing here imports any other exercise.

## What you'll build

```text
metrics/                   independent module: example.com/metrics
  go.mod                   go 1.26
  metrics.go               type Label, Aggregator; Record, Counters, Render
  cmd/
    demo/
      main.go              feeds a few records, prints the exposition
  metrics_test.go          golden-string, determinism, shuffle-property, Example
```

- Files: `metrics.go`, `cmd/demo/main.go`, `metrics_test.go`.
- Implement: `Label{Method,Route,Class}`, `Aggregator` with `Record(Label)`,
  `Counters() map[Label]int` (a clone), and `Render() string` (sorted lines).
- Test: fixed record set to golden exposition; render twice and compare; shuffle the
  input order and assert identical output; `maps.Equal` on the intermediate map.
- Verify: `go test -count=1 -race ./...`

Set up the module:

```bash
mkdir -p ~/go-exercises/metrics/cmd/demo
cd ~/go-exercises/metrics
go mod init example.com/metrics
```

### Aggregate in a map, emit via sorted keys

The `Label` is a struct of three comparable strings, so it is itself comparable and
can be a map key directly — no string concatenation, no `fmt.Sprintf` key building, no
risk of a delimiter collision. The `Aggregator` is a `map[Label]int`; `Record`
increments. That half is order-free and O(1) per event.

`Render` is where determinism is enforced. It must not `range` the counter map into the
builder — that reorders every call. Instead it collects the keys and sorts them. The
modern idiom is `slices.SortedFunc(maps.Keys(m), cmp)`: `maps.Keys` returns an
`iter.Seq[Label]` (a range-over-function iterator, Go 1.23), and `slices.SortedFunc`
collects it into a slice and sorts it with your comparator in one call — no manual
`keys := make([]Label, 0, len(m))` / `for k := range m` / `sort.Slice` boilerplate.

The comparator orders by method, then route, then class. `cmp.Or` is the clean way to
express "compare by the first field; on a tie, the next; on a tie, the next":
`cmp.Or(cmp.Compare(a.Method, b.Method), cmp.Compare(a.Route, b.Route),
cmp.Compare(a.Class, b.Class))`. `cmp.Or` returns its first non-zero argument (and
`cmp.Compare` returns 0 only when the fields are equal), so this reads exactly as the
tie-break chain it is. Each argument is evaluated, but `cmp.Compare` on strings is
cheap; for a hot path you would nest `if`s, but for a render path clarity wins.

`Counters` returns `maps.Clone(a.counters)` rather than the live map, so a caller that
inspects the counts cannot mutate the aggregator's internal state.

Create `metrics.go`:

```go
package metrics

import (
	"cmp"
	"fmt"
	"maps"
	"slices"
	"strings"
)

// Label identifies one time series. Being a struct of comparable fields, it is
// itself comparable and usable as a map key directly.
type Label struct {
	Method string // GET, POST, ...
	Route  string // /users, /orders/{id}, ...
	Class  string // 2xx, 4xx, 5xx
}

// Aggregator accumulates request counts per label set.
type Aggregator struct {
	counters map[Label]int
}

// New returns an empty Aggregator ready for Record.
func New() *Aggregator {
	return &Aggregator{counters: make(map[Label]int)}
}

// Record increments the counter for one request.
func (a *Aggregator) Record(l Label) {
	a.counters[l]++
}

// Counters returns an independent copy of the current counts.
func (a *Aggregator) Counters() map[Label]int {
	return maps.Clone(a.counters)
}

// Render produces a stable exposition: one line per label set, sorted by
// method, then route, then class. The order never depends on map iteration.
func (a *Aggregator) Render() string {
	labels := slices.SortedFunc(maps.Keys(a.counters), func(x, y Label) int {
		return cmp.Or(
			cmp.Compare(x.Method, y.Method),
			cmp.Compare(x.Route, y.Route),
			cmp.Compare(x.Class, y.Class),
		)
	})

	var b strings.Builder
	for _, l := range labels {
		fmt.Fprintf(&b, "requests_total{method=%q,route=%q,status=%q} %d\n",
			l.Method, l.Route, l.Class, a.counters[l])
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

	"example.com/metrics"
)

func main() {
	a := metrics.New()
	a.Record(metrics.Label{Method: "GET", Route: "/users", Class: "2xx"})
	a.Record(metrics.Label{Method: "GET", Route: "/users", Class: "2xx"})
	a.Record(metrics.Label{Method: "POST", Route: "/users", Class: "4xx"})
	a.Record(metrics.Label{Method: "GET", Route: "/orders", Class: "5xx"})

	fmt.Print(a.Render())
}
```

Run it:

```bash
go run ./cmd/demo
```

Expected output:

```
requests_total{method="GET",route="/orders",status="5xx"} 1
requests_total{method="GET",route="/users",status="2xx"} 2
requests_total{method="POST",route="/users",status="4xx"} 1
```

### Tests

`TestRenderGolden` feeds a fixed record set and asserts the exact multi-line string.
`TestRenderIsDeterministic` renders twice and compares. `TestRenderIgnoresInsertOrder`
is the property test: it feeds the same multiset of records in two different orders and
asserts the exposition is identical, proving the output depends on the data, not on the
sequence of inserts or on map iteration. `TestCountersClone` checks the returned map is
compared with `maps.Equal` and is an independent copy.

Create `metrics_test.go`:

```go
package metrics

import (
	"fmt"
	"maps"
	"testing"
)

func feed(a *Aggregator, labels []Label) {
	for _, l := range labels {
		a.Record(l)
	}
}

func TestRenderGolden(t *testing.T) {
	t.Parallel()

	a := New()
	feed(a, []Label{
		{"GET", "/users", "2xx"},
		{"GET", "/users", "2xx"},
		{"POST", "/users", "4xx"},
		{"GET", "/orders", "5xx"},
	})

	want := `requests_total{method="GET",route="/orders",status="5xx"} 1
requests_total{method="GET",route="/users",status="2xx"} 2
requests_total{method="POST",route="/users",status="4xx"} 1
`
	if got := a.Render(); got != want {
		t.Fatalf("Render() mismatch:\n got:\n%s\nwant:\n%s", got, want)
	}
}

func TestRenderIsDeterministic(t *testing.T) {
	t.Parallel()

	a := New()
	feed(a, []Label{
		{"GET", "/a", "2xx"}, {"GET", "/b", "2xx"}, {"GET", "/c", "2xx"},
		{"POST", "/a", "4xx"}, {"DELETE", "/b", "5xx"},
	})
	if first, second := a.Render(), a.Render(); first != second {
		t.Fatalf("Render not deterministic:\n%q\n%q", first, second)
	}
}

func TestRenderIgnoresInsertOrder(t *testing.T) {
	t.Parallel()

	forward := []Label{
		{"GET", "/a", "2xx"}, {"GET", "/a", "2xx"},
		{"POST", "/b", "4xx"}, {"GET", "/c", "5xx"},
	}
	reversed := make([]Label, len(forward))
	for i, l := range forward {
		reversed[len(forward)-1-i] = l
	}

	a1, a2 := New(), New()
	feed(a1, forward)
	feed(a2, reversed)

	if a1.Render() != a2.Render() {
		t.Fatalf("exposition depends on insert order:\n%s\n---\n%s", a1.Render(), a2.Render())
	}
	if !maps.Equal(a1.Counters(), a2.Counters()) {
		t.Fatal("intermediate counter maps differ")
	}
}

func TestCountersClone(t *testing.T) {
	t.Parallel()

	a := New()
	a.Record(Label{"GET", "/x", "2xx"})
	snap := a.Counters()
	snap[Label{"GET", "/x", "2xx"}] = 999 // mutate the copy

	if a.counters[Label{"GET", "/x", "2xx"}] != 1 {
		t.Fatal("Counters returned a live view; mutation leaked into the aggregator")
	}
}

func ExampleAggregator_Render() {
	a := New()
	a.Record(Label{"GET", "/health", "2xx"})
	a.Record(Label{"GET", "/health", "2xx"})
	fmt.Print(a.Render())
	// Output:
	// requests_total{method="GET",route="/health",status="2xx"} 2
}
```

## Review

The aggregator is correct when the exposition is a function of the counter multiset and
nothing else. The two failure modes it defends against are both about leaking map order:
ranging the counters into the builder (caught by the determinism and shuffle tests) and
returning the live map from `Counters` (caught by `TestCountersClone`). Note the
comparator uses `cmp.Or` over `cmp.Compare`, which is the idiomatic multi-key sort in
modern Go — dropping any level of the chain would make ties between, say, two routes of
the same method non-deterministic again. Run `go test -race` to confirm the parallel
subtests share no state.

## Resources

- [`maps.Keys`](https://pkg.go.dev/maps#Keys) — returns an `iter.Seq[K]` over a map's keys.
- [`slices.SortedFunc`](https://pkg.go.dev/slices#SortedFunc) — collect an iterator and sort with a comparator.
- [`cmp.Or`](https://pkg.go.dev/cmp#Or) and [`cmp.Compare`](https://pkg.go.dev/cmp#Compare) — the multi-key tie-break chain.
- [Prometheus text exposition format](https://prometheus.io/docs/instrumenting/exposition_formats/) — the shape of the rendered lines.

---

Back to [00-concepts.md](00-concepts.md) | Next: [03-nil-safe-layered-config-merge.md](03-nil-safe-layered-config-merge.md)
