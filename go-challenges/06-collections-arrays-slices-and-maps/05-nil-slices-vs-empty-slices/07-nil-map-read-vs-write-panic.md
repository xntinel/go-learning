# Exercise 7: Aggregating counters into a lazily-initialized (nil) map

A metrics collector keeps counts in a `map[string]int`. Its zero value has a nil
map, and that is fine for reads — `m[k]`, `len`, and `range` are all safe — but
the first *write* panics with "assignment to entry in nil map". This exercise
builds the collector to lazily initialize on first write, and to hand out a
non-nil defensive snapshot that marshals to `{}` rather than `null`.

This module is fully self-contained: its own `go mod init`, its own `metrics`
package, its own demo and tests.

## What you'll build

```text
metrics/                      independent module: example.com/metrics
  go.mod
  metrics/metrics.go          MetricsCollector: Add (lazy init), Count, Total, Snapshot
  metrics/metrics_test.go     nil-read safety, no-panic Add, nil-map-write panic, marshal facts
  cmd/demo/main.go            reads before writing, adds, prints JSON snapshot
```

Files: `metrics/metrics.go`, `metrics/metrics_test.go`, `cmd/demo/main.go`.
Implement: `MetricsCollector` with `Add` (lazy-init the map), `Count`, `Total`,
and `Snapshot` (non-nil `maps.Clone`).
Test: reads on a zero-value collector are safe; `Add` on a fresh collector does
not panic; a raw nil-map write panics; `json.Marshal` of a nil map is `null` and
of an empty map is `{}`; `Snapshot` is a non-nil defensive copy.
Verify: `go test -count=1 -race ./...`

Set up the module:

```bash
mkdir -p go-solutions/06-collections-arrays-slices-and-maps/05-nil-slices-vs-empty-slices/07-nil-map-read-vs-write-panic/metrics go-solutions/06-collections-arrays-slices-and-maps/05-nil-slices-vs-empty-slices/07-nil-map-read-vs-write-panic/cmd/demo
cd go-solutions/06-collections-arrays-slices-and-maps/05-nil-slices-vs-empty-slices/07-nil-map-read-vs-write-panic
```

### The asymmetry: reads are safe, the first write panics

A nil map is not symmetric with a nil slice. Every read of a nil map is defined
and safe: `m[k]` returns the value type's zero, the comma-ok form reports
`false`, `len(m)` is `0`, and `range` iterates zero times. So a zero-value
`MetricsCollector` answers `Count` and `Total` correctly without any
initialization — which is exactly why the bug is easy to miss. The code reads
fine in every test that only reads, and then the first `Add` in production hits
`c.counts[label]++`, which is a write to a nil map, and the program panics with
`assignment to entry in nil map`.

This bites hardest when the map lives in a struct field that a constructor forgot
to initialize, or when a struct is created with a composite literal that omits
the map. The fix has two idiomatic forms: initialize the map in the constructor,
or lazily `make` it on first write. `Add` uses the lazy form —
`if c.counts == nil { c.counts = make(...) }` before the increment — so a
zero-value collector is safe to write to with no constructor call at all. Both
forms are fine; the non-negotiable is that a nil map is never written.

`Snapshot` is the boundary concern from the other exercises applied to maps. It
returns a `maps.Clone` so callers cannot mutate the collector's internal map, and
it guards the nil case explicitly, because `maps.Clone(nil)` returns nil and a
nil map marshals to `null`. Normalizing to a non-nil empty map means a
fresh collector's snapshot serializes as `{}`, the shape a metrics consumer
expects for "no counters yet."

Create `metrics/metrics.go`:

```go
package metrics

import "maps"

// MetricsCollector aggregates labeled counters. Its zero value is usable for
// reads: the counts map starts nil, and reading a nil map is always safe. The
// first WRITE must initialize the map, or it panics with "assignment to entry
// in nil map".
type MetricsCollector struct {
	counts map[string]int
}

// Add increments the counter for label. It lazily initializes the map on first
// use, which is what makes a zero-value MetricsCollector safe to write to.
func (c *MetricsCollector) Add(label string) {
	if c.counts == nil {
		c.counts = make(map[string]int)
	}
	c.counts[label]++
}

// Count returns the counter for label. Reading a nil map returns the zero value,
// so this is safe even before the first Add.
func (c *MetricsCollector) Count(label string) int {
	return c.counts[label]
}

// Total sums all counters. Ranging a nil map iterates zero times, so this is
// safe on a zero-value collector.
func (c *MetricsCollector) Total() int {
	sum := 0
	for _, n := range c.counts {
		sum += n
	}
	return sum
}

// Snapshot returns a defensive, non-nil copy suitable for JSON. maps.Clone(nil)
// is nil, so the nil case is normalized to a non-nil empty map, which marshals
// to {} rather than null.
func (c *MetricsCollector) Snapshot() map[string]int {
	if c.counts == nil {
		return map[string]int{}
	}
	return maps.Clone(c.counts)
}
```

### The runnable demo

Create `cmd/demo/main.go`:

```go
package main

import (
	"encoding/json"
	"fmt"

	"example.com/metrics/metrics"
)

func main() {
	var c metrics.MetricsCollector // zero value: nil map inside

	// Reads are safe before any write.
	fmt.Printf("pre-write total=%d count(hit)=%d\n", c.Total(), c.Count("hit"))

	c.Add("hit")
	c.Add("hit")
	c.Add("miss")

	snap := c.Snapshot()
	b, _ := json.Marshal(snap)
	fmt.Printf("after adds: %s total=%d\n", b, c.Total())

	// A fresh collector's snapshot is {} (non-nil empty), never null.
	var empty metrics.MetricsCollector
	eb, _ := json.Marshal(empty.Snapshot())
	fmt.Printf("empty snapshot: %s\n", eb)
}
```

Run it:

```bash
go run ./cmd/demo
```

Expected output:

```
pre-write total=0 count(hit)=0
after adds: {"hit":2,"miss":1} total=3
empty snapshot: {}
```

### Tests

`TestReadsOnZeroValueAreSafe` confirms `Count` and `Total` work on the nil map.
`TestAddOnZeroValueDoesNotPanic` proves lazy init makes a fresh collector
writable. `TestRawNilMapWritePanics` uses `recover` to demonstrate the exact
panic the lazy init avoids, asserting the message mentions "nil map".
`TestNilVsEmptyMapMarshal` pins the two marshal facts the boundary depends on,
and `TestSnapshotIsNonNilDefensiveCopy` proves the snapshot is both non-nil (so
it serializes to `{}`) and an independent copy (so mutating it does not leak into
the collector).

Create `metrics/metrics_test.go`:

```go
package metrics

import (
	"encoding/json"
	"fmt"
	"strings"
	"testing"
)

func TestReadsOnZeroValueAreSafe(t *testing.T) {
	t.Parallel()
	var c MetricsCollector
	if got := c.Count("nope"); got != 0 {
		t.Fatalf("Count on nil map = %d, want 0", got)
	}
	if got := c.Total(); got != 0 {
		t.Fatalf("Total on nil map = %d, want 0", got)
	}
}

func TestAddOnZeroValueDoesNotPanic(t *testing.T) {
	t.Parallel()
	var c MetricsCollector
	c.Add("hit")
	c.Add("hit")
	if got := c.Count("hit"); got != 2 {
		t.Fatalf("Count(hit) = %d, want 2", got)
	}
}

func TestRawNilMapWritePanics(t *testing.T) {
	t.Parallel()
	defer func() {
		r := recover()
		if r == nil {
			t.Fatal("expected a panic writing to a nil map")
		}
		if !strings.Contains(fmt.Sprint(r), "nil map") {
			t.Fatalf("panic = %v, want mention of nil map", r)
		}
	}()
	var m map[string]int // nil
	m["x"] = 1           // panics: assignment to entry in nil map
}

func TestNilVsEmptyMapMarshal(t *testing.T) {
	t.Parallel()
	var nilMap map[string]int
	nb, _ := json.Marshal(nilMap)
	if string(nb) != "null" {
		t.Fatalf("nil map marshals to %s, want null", nb)
	}
	eb, _ := json.Marshal(map[string]int{})
	if string(eb) != "{}" {
		t.Fatalf("empty map marshals to %s, want {}", eb)
	}
}

func TestSnapshotIsNonNilDefensiveCopy(t *testing.T) {
	t.Parallel()
	var c MetricsCollector
	snap := c.Snapshot()
	if snap == nil {
		t.Fatal("Snapshot on empty collector must be non-nil")
	}
	b, _ := json.Marshal(snap)
	if string(b) != "{}" {
		t.Fatalf("empty snapshot marshals to %s, want {}", b)
	}

	c.Add("hit")
	snap = c.Snapshot()
	snap["hit"] = 999 // mutate the copy
	if c.Count("hit") != 1 {
		t.Fatalf("snapshot mutation leaked: Count(hit) = %d", c.Count("hit"))
	}
}
```

## Review

The collector is correct when a zero value reads safely, `Add` initializes on
first write instead of panicking, and `Snapshot` is a non-nil independent copy.
The mechanism to remember is the asymmetry: nil-map reads are always defined, so
a struct with an uninitialized map field passes every read-only test and then
panics on its first write in production. Lazy init in `Add` (or initialization in
a constructor) removes the panic; the explicit nil guard in `Snapshot` removes the
`null`-vs-`{}` discrepancy that `maps.Clone(nil)` would otherwise introduce. The
`recover`-based test documents the exact failure so the reason for the lazy init
is never lost.

## Resources

- [Go maps in action — nil maps and the write panic](https://go.dev/blog/maps) — reading is safe, writing to a nil map panics.
- [maps.Clone](https://pkg.go.dev/maps#Clone) — defensive copy of a map; Clone(nil) is nil.
- [encoding/json — Marshal](https://pkg.go.dev/encoding/json#Marshal) — nil map to null, empty map to {}.

---

Back to [00-concepts.md](00-concepts.md) | Next: [08-preallocate-capacity-hot-path.md](08-preallocate-capacity-hot-path.md)
