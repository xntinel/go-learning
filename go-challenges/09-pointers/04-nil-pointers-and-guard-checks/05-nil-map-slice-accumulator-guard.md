# Exercise 5: Nil Maps and Nil Slices in an Aggregator

The zero value of a map is a nil map and the zero value of a slice is a nil
slice, and their safety rules are asymmetric: you can read a nil map but not
write it, while a nil slice is safe for everything including `append`. This
module builds a request-metrics aggregator that relies on both rules — reads and
ranges work on the zero value, the map is lazily created before the first write,
and events accumulate onto a nil slice.

This module is fully self-contained.

## What you'll build

```text
aggregator/               independent module: example.com/aggregator
  go.mod                  go 1.24
  aggregator.go           type Aggregator; Count/Total (read nil map), Add (lazy make), Record (append nil slice)
  cmd/
    demo/
      main.go             runnable demo: zero-value reads, then accumulate
  aggregator_test.go      zero-value safety, write-nil-map panic via recover, guarded Add
```

Files: `aggregator.go`, `cmd/demo/main.go`, `aggregator_test.go`.
Implement: an `Aggregator` whose zero value is usable for reads/ranges; `Add` lazily initializes the map before the first write; `Record` appends to a nil slice.
Test: reading and ranging the zero value never panics and reports zero; an unguarded write into a nil map panics (asserted with `recover`); the guarded `Add` path succeeds; `Record` accumulates from a nil start.
Verify: `go test -count=1 -race ./...`

Set up the module:

```bash
go mod edit -go=1.24
```

### The exact rule set

Know which zero value you hold before the first write:

- Nil map: `v := m[k]` (and comma-ok), `len(m)`, and `range m` are safe and act
  as an empty map. `m[k] = v` panics. A nil map is read-only until `make`.
- Nil slice: `len(s)`, `range s`, and `append(s, x)` are all safe. `append`
  allocates a backing array on first use and returns a real slice. A nil slice is
  a good empty slice.

The aggregator uses both. Its read methods (`Count`, `Total`) index and range the
map directly, so they work on the zero-value `Aggregator` with no initialization
— a caller can construct `var a Aggregator` and immediately read zeros. The write
method `Add` must guard: it lazily `make`s the map on first insert, because
writing to the nil map would panic. The `Record` method appends events to a
possibly-nil slice with no guard at all, because `append` to nil is safe.

```go
func (a *Aggregator) Add(route string) {
	if a.counts == nil {
		a.counts = make(map[string]int) // guard the write-to-nil-map panic
	}
	a.counts[route]++
}

func (a *Aggregator) Record(event string) {
	a.events = append(a.events, event) // safe even when a.events is nil
}
```

The design payoff: the zero value is directly useful for the read-only consumer,
and only the single write path pays for a nil check. There is no constructor
requirement for read-only use.

Create `aggregator.go`:

```go
package aggregator

// Aggregator tallies per-route request counts and records an event log. Its zero
// value is usable for reads: reading and ranging a nil map is safe. Only the
// write path (Add) lazily initializes the map.
type Aggregator struct {
	counts map[string]int
	events []string
}

// Count returns the tally for route. Reading a nil map is safe and yields 0.
func (a *Aggregator) Count(route string) int {
	return a.counts[route]
}

// Total sums all route counts. Ranging a nil map is safe and yields 0.
func (a *Aggregator) Total() int {
	total := 0
	for _, n := range a.counts {
		total += n
	}
	return total
}

// Add increments route's tally, lazily creating the map before the first write
// to avoid the write-to-nil-map panic.
func (a *Aggregator) Add(route string) {
	if a.counts == nil {
		a.counts = make(map[string]int)
	}
	a.counts[route]++
}

// Record appends an event. Appending to a nil slice is safe; append allocates.
func (a *Aggregator) Record(event string) {
	a.events = append(a.events, event)
}

// Events returns the recorded events. len on a nil slice is 0.
func (a *Aggregator) Events() []string {
	return a.events
}
```

### The runnable demo

Create `cmd/demo/main.go`:

```go
package main

import (
	"fmt"

	"example.com/aggregator"
)

func main() {
	var a aggregator.Aggregator // zero value: nil map, nil slice

	// Reads on the zero value are safe.
	fmt.Printf("zero: count=%d total=%d events=%d\n",
		a.Count("/health"), a.Total(), len(a.Events()))

	// Writes lazily initialize.
	a.Add("/health")
	a.Add("/health")
	a.Add("/login")
	a.Record("started")

	fmt.Printf("after: /health=%d total=%d events=%d\n",
		a.Count("/health"), a.Total(), len(a.Events()))
}
```

Run it:

```bash
go run ./cmd/demo
```

Expected output:

```
zero: count=0 total=0 events=0
after: /health=2 total=3 events=1
```

### Tests

The tests encode the rule set directly. The zero-value reads must not panic and
must report zero. An unguarded write into a raw nil map must panic — asserted by
recovering from it — which is exactly the panic the guard in `Add` prevents. The
guarded `Add` and the nil-start `Record` must succeed.

Create `aggregator_test.go`:

```go
package aggregator

import (
	"testing"
)

func TestZeroValueReadsAreSafe(t *testing.T) {
	t.Parallel()

	var a Aggregator
	if got := a.Count("/x"); got != 0 {
		t.Fatalf("Count = %d, want 0", got)
	}
	if got := a.Total(); got != 0 {
		t.Fatalf("Total = %d, want 0", got)
	}
	if got := len(a.Events()); got != 0 {
		t.Fatalf("len(Events) = %d, want 0", got)
	}
}

func TestWriteToNilMapPanics(t *testing.T) {
	t.Parallel()

	defer func() {
		if r := recover(); r == nil {
			t.Fatal("writing to a nil map did not panic")
		}
	}()
	var m map[string]int // nil
	m["x"] = 1           // panics: assignment to entry in nil map
}

func TestGuardedAddSucceeds(t *testing.T) {
	t.Parallel()

	var a Aggregator
	a.Add("/health")
	a.Add("/health")
	a.Add("/login")

	if got := a.Count("/health"); got != 2 {
		t.Fatalf("Count(/health) = %d, want 2", got)
	}
	if got := a.Total(); got != 3 {
		t.Fatalf("Total = %d, want 3", got)
	}
}

func TestRecordAppendsFromNil(t *testing.T) {
	t.Parallel()

	var a Aggregator // nil events slice
	a.Record("a")
	a.Record("b")

	got := a.Events()
	if len(got) != 2 || got[0] != "a" || got[1] != "b" {
		t.Fatalf("Events = %v, want [a b]", got)
	}
}
```

## Review

The aggregator is correct when its zero value serves all reads and only `Add`
guards the map creation. `TestZeroValueReadsAreSafe` proves read/range on nil is
fine; `TestWriteToNilMapPanics` proves the panic that `Add`'s guard exists to
prevent; `TestGuardedAddSucceeds` and `TestRecordAppendsFromNil` prove the write
paths. If `Add` dropped its `if a.counts == nil` guard, it would panic on the
first call exactly as `TestWriteToNilMapPanics` shows.

The mistake avoided: assuming a nil map is writable like a nil slice, or eagerly
allocating both when the reader only needs the nil-safe read path.

## Resources

- [Go Blog: Go maps in action](https://go.dev/blog/maps) — nil maps, reading vs. writing, and when to `make`.
- [Go Spec: Appending to and copying slices](https://go.dev/ref/spec#Appending_and_copying_slices) — `append` allocates for a nil slice.
- [Go Spec: Index expressions](https://go.dev/ref/spec#Index_expressions) — reading a nil map yields the zero value; writing panics.

---

Back to [00-concepts.md](00-concepts.md) | Prev: [04-typed-nil-interface-error-trap.md](04-typed-nil-interface-error-trap.md) | Next: [06-optional-observability-hook-guard.md](06-optional-observability-hook-guard.md)
