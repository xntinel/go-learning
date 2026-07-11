# Exercise 1: Interned Metric Labels and Series Keys

A metrics backend touches the same handful of labels millions of times, so it is the textbook case for interning: canonicalize each `Label` to one shared copy, key the counters on a small comparable handle, and let equality be a pointer compare. This exercise builds that `metric` package end to end — an interned `Label`, a `Counters` registry keyed on the handle, then a `SeriesKey` that interns a whole *set* of labels into one order-independent handle, plus a `Vec` that counts per series.

This module is fully self-contained. It begins with its own `go mod init`, defines every type it needs, and ships its own demo and tests. Nothing here imports any other lesson.

## What you'll build

```text
metric.go            Label, Tag=unique.Handle[Label], Intern; Counters (Inc/Value/Distinct)
series.go            SeriesKey=unique.Handle[string], InternSeries([]Label); Vec (Inc/Value)
cmd/
  demo/
    main.go          count per-label and per-series, showing dedup and order-independence
metric_test.go       identity, round-trip, map-key dedup, order-independence, injectivity, -race
example_test.go      Example with // Output
```

- Files: `metric.go`, `series.go`, `cmd/demo/main.go`, `metric_test.go`, `example_test.go`.
- Implement: `Intern(name, value)` returning a `Tag` (`unique.Handle[Label]`); a `Counters` registry keyed on `Tag`; `InternSeries([]Label)` returning a `SeriesKey` (`unique.Handle[string]`); a `Vec` registry keyed on `SeriesKey`.
- Test: equal labels intern to equal tags, `Value()` round-trips, reordered/rebuilt labels hit one counter, the series key is order-independent and injective, and `unique.Make` is exercised under `-race`.
- Verify: `go test -count=1 -race ./...`

Set up the module:

```bash
mkdir -p interned-metrics/cmd/demo && cd interned-metrics
go mod init example.com/interned-metrics
go mod edit -go=1.26
```

### Interning a single label: the Tag

`unique.Make[T comparable](v T)` returns a `Handle[T]` — a globally canonical handle for `v`. Equal values produce equal handles, and the handle is itself a small comparable value whose equality is, in the current implementation, a pointer compare against the single stored copy. That single call buys two things at once. The memory win: every duplicate of a label collapses to one canonical copy, so a process that produces `{method:"GET"}` a million times stores the strings once. The speed win: comparing two handles is one pointer compare instead of comparing the two underlying strings byte by byte.

`Label{Name, Value string}` is the raw dimension; `Tag` is its interned form, declared as a type alias `= unique.Handle[Label]` so the two names are interchangeable. `Intern` is the one-liner that wraps `unique.Make`. The payoff lands in `Counters`: its map is keyed directly on `Tag`, so the key comparison the runtime does on every `Inc` and `Value` lookup is the cheap pointer compare, and the map physically holds one copy of each distinct label no matter how many times it is counted. A `sync.Mutex` guards the map because a metrics registry is written from many goroutines; `unique.Make` itself is already safe for concurrent use, so only the map needs the lock.

Create `metric.go`:

```go
package metric

import (
	"sync"
	"unique"
)

// Label is a single metric dimension, for example {"method", "GET"}.
type Label struct {
	Name  string
	Value string
}

// Tag is an interned Label: a small, comparable handle that shares one
// canonical copy of the label across all equal labels.
type Tag = unique.Handle[Label]

// Intern canonicalizes a label. Equal labels return equal Tags, and comparing
// Tags is a pointer comparison rather than two string comparisons.
func Intern(name, value string) Tag {
	return unique.Make(Label{Name: name, Value: value})
}

// Counters tallies events per interned label. The Tag map key is compared by
// pointer identity, so lookups do not re-compare the label strings.
type Counters struct {
	mu     sync.Mutex
	counts map[Tag]int64
}

func NewCounters() *Counters {
	return &Counters{counts: make(map[Tag]int64)}
}

// Inc adds one to the counter for tag.
func (c *Counters) Inc(tag Tag) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.counts[tag]++
}

// Value reports the current count for tag.
func (c *Counters) Value(tag Tag) int64 {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.counts[tag]
}

// Distinct reports how many different labels have been counted.
func (c *Counters) Distinct() int {
	c.mu.Lock()
	defer c.mu.Unlock()
	return len(c.counts)
}
```

### Interning a derived form: the SeriesKey

Interning is most powerful applied not to a raw value but to a *derived canonical form*. A metric time series is identified by a whole *set* of labels, and `{method=GET, status=200}` is the same series as `{status=200, method=GET}` — the order the caller happened to pass them in must not matter. `InternSeries` removes that order by sorting the labels, joining them into one canonical string, and interning that string into a single `SeriesKey`. Two differently-ordered label sets therefore collapse to the same handle, so series identity, deduplication, and map-key equality all reduce to one pointer compare no matter how many labels the series has.

The detail that makes the canonical string correct is that the encoding must be *injective*: distinct label sets must never render to the same string. A naive `name=value,` join is not injective — the two-label set `{a="b", c="d"}` and the one-label set `{a="b,c=d"}` both produce `a=b,c=d`, so two different series would silently share one counter. The fix is to quote each component with `strconv.Quote` before joining; the `,` and `=` separators can then never appear unescaped inside a component, and the encoding becomes injective. `TestSeriesKeyIsInjective` pins exactly that property. The input slice is cloned before sorting (`slices.Clone`) so `InternSeries` never mutates the caller's slice.

`Vec` is to series what `Counters` is to labels: a mutex-guarded map keyed on the interned `SeriesKey`, so a per-series lookup is one pointer compare regardless of label count or order.

Create `series.go`, which sorts the labels into a canonical string and interns that:

```go
package metric

import (
	"slices"
	"strconv"
	"strings"
	"sync"
	"unique"
)

// SeriesKey identifies a unique combination of labels (a time series). The
// labels are sorted and joined into one canonical string, then interned, so two
// label sets that differ only in order share the same handle.
type SeriesKey = unique.Handle[string]

// InternSeries canonicalizes a set of labels into a SeriesKey. The input slice
// is not modified. It assumes the labels are a set with distinct names; the
// canonical encoding quotes each component (strconv.Quote) so the join is
// injective -- {a="b", c="d"} can never collide with the single label
// {a="b,c=d"}, a bug a naive "name=value," join would have.
func InternSeries(labels []Label) SeriesKey {
	sorted := slices.Clone(labels)
	slices.SortFunc(sorted, func(a, b Label) int {
		if c := strings.Compare(a.Name, b.Name); c != 0 {
			return c
		}
		return strings.Compare(a.Value, b.Value)
	})

	var b strings.Builder
	for i, l := range sorted {
		if i > 0 {
			b.WriteByte(',')
		}
		b.WriteString(strconv.Quote(l.Name))
		b.WriteByte('=')
		b.WriteString(strconv.Quote(l.Value))
	}
	return unique.Make(b.String())
}

// Vec counts events per series. Its map key is the interned SeriesKey, so a
// lookup is one pointer compare no matter how many labels the series has.
type Vec struct {
	mu     sync.Mutex
	counts map[SeriesKey]int64
}

func NewVec() *Vec {
	return &Vec{counts: make(map[SeriesKey]int64)}
}

func (v *Vec) Inc(key SeriesKey) {
	v.mu.Lock()
	defer v.mu.Unlock()
	v.counts[key]++
}

func (v *Vec) Value(key SeriesKey) int64 {
	v.mu.Lock()
	defer v.mu.Unlock()
	return v.counts[key]
}
```

### The runnable demo

A test proves a property in the abstract; a demo makes the package concrete. This one counts four requests per label — observing that `method=GET` appears three times and that the registry tracks four distinct labels — then counts two reordered copies of the same series and shows they land on one `Vec` counter.

Create `cmd/demo/main.go`:

```go
package main

import (
	"fmt"

	metric "example.com/interned-metrics"
)

func main() {
	// Per-label counters: the interned Tag is the map key, so each lookup is a
	// pointer compare and the map holds one copy of each distinct label.
	c := metric.NewCounters()
	requests := []struct{ method, status string }{
		{"GET", "200"}, {"GET", "200"}, {"GET", "404"}, {"POST", "200"},
	}
	for _, r := range requests {
		c.Inc(metric.Intern("method", r.method))
		c.Inc(metric.Intern("status", r.status))
	}
	fmt.Println("GET:", c.Value(metric.Intern("method", "GET")))
	fmt.Println("200:", c.Value(metric.Intern("status", "200")))
	fmt.Println("distinct:", c.Distinct())

	// Per-series counters: the SeriesKey canonicalizes the whole label set, so a
	// reordered label set hits the same counter.
	v := metric.NewVec()
	v.Inc(metric.InternSeries([]metric.Label{{Name: "method", Value: "GET"}, {Name: "status", Value: "200"}}))
	v.Inc(metric.InternSeries([]metric.Label{{Name: "status", Value: "200"}, {Name: "method", Value: "GET"}}))
	key := metric.InternSeries([]metric.Label{{Name: "method", Value: "GET"}, {Name: "status", Value: "200"}})
	fmt.Println("series GET/200:", v.Value(key))
}
```

Run it:

```bash
go run ./cmd/demo
```

Expected output:

```
GET: 3
200: 3
distinct: 4
series GET/200: 2
```

### Tests

The tests pin the properties the package promises, and one detail is subtle enough to be worth its own technique: to prove interning collapses *value-equal* labels rather than only literally-identical pointers, several tests build a string at runtime (`strings.Repeat("G", 1) + "ET"` or `string([]byte{'G','E','T'})`) so it does not share backing storage with the `"GET"` literal — yet the two still produce equal handles. The suite covers identity (equal labels intern to equal tags, different labels do not), round-trip (`Value()` reconstructs the label), map-key dedup (reordered or rebuilt labels hit one counter), order-independence and injectivity of the series key, stability of a canonical entry across a `runtime.GC()`, and a concurrency test under `-race`, since `unique.Make` is documented safe for concurrent use.

Create `metric_test.go`:

```go
package metric

import (
	"runtime"
	"strings"
	"sync"
	"testing"
)

func TestEqualLabelsInternToEqualTags(t *testing.T) {
	t.Parallel()

	// "GET" built at runtime: a different backing array from the literal.
	built := strings.Repeat("G", 1) + "ET"
	a := Intern("method", "GET")
	b := Intern("method", built)

	if a != b {
		t.Fatal("equal labels must produce equal Tags")
	}
}

func TestDifferentLabelsInternToDifferentTags(t *testing.T) {
	t.Parallel()

	if Intern("method", "GET") == Intern("method", "POST") {
		t.Fatal("different labels must produce different Tags")
	}
	if Intern("method", "GET") == Intern("route", "GET") {
		t.Fatal("different names must produce different Tags")
	}
}

func TestValueRoundTrips(t *testing.T) {
	t.Parallel()

	got := Intern("status", "200").Value()
	if got != (Label{Name: "status", Value: "200"}) {
		t.Fatalf("Value() = %+v, want {status 200}", got)
	}
}

func TestTagAsMapKeyDedups(t *testing.T) {
	t.Parallel()

	c := NewCounters()
	get := Intern("method", "GET")
	getAgain := Intern("method", strings.Repeat("G", 1)+"ET")
	post := Intern("method", "POST")

	c.Inc(get)
	c.Inc(getAgain) // same canonical label as get
	c.Inc(post)

	if n := c.Value(get); n != 2 {
		t.Fatalf("GET count = %d, want 2 (both increments hit one key)", n)
	}
	if n := c.Distinct(); n != 2 {
		t.Fatalf("distinct labels = %d, want 2", n)
	}
}

func TestSeriesKeyIsOrderIndependent(t *testing.T) {
	t.Parallel()

	ab := InternSeries([]Label{{"method", "GET"}, {"status", "200"}})
	ba := InternSeries([]Label{{"status", "200"}, {"method", "GET"}})
	if ab != ba {
		t.Fatal("series keys must be independent of label order")
	}

	other := InternSeries([]Label{{"method", "POST"}, {"status", "200"}})
	if ab == other {
		t.Fatal("different label sets must produce different series keys")
	}
}

func TestSeriesKeyIsInjective(t *testing.T) {
	t.Parallel()

	// Two genuinely different label sets that a naive "name=value," join would
	// collapse to the same string "a=b,c=d". Quoting the components keeps the
	// encoding injective, so the series keys must differ.
	twoLabels := InternSeries([]Label{{"a", "b"}, {"c", "d"}})
	oneLabel := InternSeries([]Label{{"a", "b,c=d"}})
	if twoLabels == oneLabel {
		t.Fatal("distinct label sets collided into one series key (encoding not injective)")
	}
}

func TestVecCountsBySeries(t *testing.T) {
	t.Parallel()

	v := NewVec()
	v.Inc(InternSeries([]Label{{"method", "GET"}, {"status", "200"}}))
	v.Inc(InternSeries([]Label{{"status", "200"}, {"method", "GET"}})) // same series, reordered

	key := InternSeries([]Label{{"method", "GET"}, {"status", "200"}})
	if got := v.Value(key); got != 2 {
		t.Fatalf("series count = %d, want 2 (reordered labels hit one key)", got)
	}
}

func TestManyCopiesShareOneHandle(t *testing.T) {
	t.Parallel()

	first := Intern("method", "GET")
	for i := range 1000 {
		// Build a fresh "GET" (new backing array) each iteration.
		got := Intern("method", string([]byte{'G', 'E', 'T'}))
		if got != first {
			t.Fatalf("iteration %d produced a different handle", i)
		}
	}
}

func TestHandleStableAcrossGC(t *testing.T) {
	t.Parallel()

	// Intern a value, then force a GC. Re-interning the same value must still
	// produce a handle equal to the first: the canonical identity is stable even
	// after the runtime has had a chance to reclaim unused entries.
	want := Intern("env", "prod")
	runtime.GC()
	got := Intern("env", "prod")
	if got != want {
		t.Fatal("re-interning the same value after a GC must return an equal handle")
	}
}

func TestConcurrentInternAndCount(t *testing.T) {
	t.Parallel()

	c := NewCounters()
	var wg sync.WaitGroup
	for range 200 {
		wg.Add(1)
		go func() {
			defer wg.Done()
			c.Inc(Intern("method", "GET"))
		}()
	}
	wg.Wait()

	if n := c.Value(Intern("method", "GET")); n != 200 {
		t.Fatalf("GET count = %d, want 200", n)
	}
}
```

Create `example_test.go`:

```go
package metric

import "fmt"

func Example() {
	a := Intern("status", "200")
	b := Intern("status", "200")

	fmt.Println(a == b)
	fmt.Printf("%s=%s\n", a.Value().Name, a.Value().Value)

	// Output:
	// true
	// status=200
}
```

## Review

The package is correct when interning collapses *value-equal* inputs, not merely identical pointers: that is why the identity tests rebuild `"GET"` at runtime so it has a fresh backing array, and the handles still compare equal. Confirm the two map-keyed registries dedup — reordered or rebuilt labels increment one counter and `Distinct()` counts canonical labels, not call sites — and that `InternSeries` is both order-independent (sorted before joining) and injective (`strconv.Quote` on each component), which `TestSeriesKeyIsInjective` proves by feeding it the `{a="b", c="d"}` versus `{a="b,c=d"}` pair that a naive join would collapse. The `-race` concurrency test backs the claim that `unique.Make` is safe for concurrent use, and `TestHandleStableAcrossGC` shows the canonical entry survives a forced GC.

Common mistakes for this feature. The first is comparing `a.Value() == b.Value()` instead of `a == b`: that throws away the whole point and re-compares the underlying strings. The second is a non-injective canonical encoding — joining with a bare `name=value,` separator lets a separator inside a value forge a different set's string, silently merging two series; quote the components. The third is interning one-shot values (a request ID, a UUID): you pay the sharded-map lookup and never get the dedup, so reserve interning for values with real reuse. The fourth is forgetting that `InternSeries` re-runs `slices.Clone` + `SortFunc` + a builder allocation on every call, even a cache hit, so a hot path should cache the `SeriesKey` per collector rather than re-canonicalize each event.

## Resources

- [`unique` package](https://pkg.go.dev/unique) — the standard-library reference for `Make` and `Handle`, including the documented concurrency safety and `Value()`.
- [New unique package (Go blog)](https://go.dev/blog/unique) — the design rationale: weak-backed canonical store, interning, and why it does not leak like a hand-rolled map.
- [Go 1.23 release notes: unique](https://go.dev/doc/go1.23#unique) — the release note that introduced the package and its intended use for canonicalizing comparable values.
- [`strconv.Quote`](https://pkg.go.dev/strconv#Quote) — the escaping used to keep the joined series-key encoding injective.

---

Back to [00-concepts.md](00-concepts.md)
