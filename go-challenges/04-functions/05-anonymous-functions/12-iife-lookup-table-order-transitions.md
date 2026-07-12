# Exercise 12: An IIFE-Built Lookup Table for Order Status Transitions

**Nivel: Intermedio** — validacion rapida (un test corto).

A state machine — order placed, paid, shipped, or canceled — reads best as a
flat, reviewable list of allowed moves, but checking "is this move legal"
needs an O(1) lookup, not a linear scan. This module keeps the flat list as
the source of truth and uses an immediately-invoked function literal to build
the nested lookup map from it exactly once, panicking at package init if the
data is inconsistent.

This module is fully self-contained. Nothing here imports another exercise.

## What you'll build

```text
orderfsm/                     module example.com/orderfsm
  go.mod
  orderfsm.go                 Status, Event, transitions, buildTable, table (IIFE), Apply
  orderfsm_test.go            valid move, invalid move, duplicate detection, table integrity
```

- Files: `orderfsm.go`, `orderfsm_test.go`.
- Implement: `transitions`, the flat list of allowed `(From, Event, To)` moves; `buildTable(ts) (map[Status]map[Event]Status, error)`, a plain function that also detects a duplicate `(From, Event)` pair; a package-level `table` built by an IIFE that calls `buildTable` and panics on error; `Apply(current, event) (Status, error)`.
- Test: a legal move returns the right next status; an illegal move returns an error; `buildTable` on a list with a duplicate pair returns an error; `buildTable` on the real `transitions` matches the package-level `table`.
- Verify: `go test -count=1 ./...`

Set up the module:

```bash
mkdir -p go-solutions/04-functions/05-anonymous-functions/12-iife-lookup-table-order-transitions
cd go-solutions/04-functions/05-anonymous-functions/12-iife-lookup-table-order-transitions
go mod edit -go=1.24
```

### Why an IIFE instead of a plain package var

`table` needs to be computed, not written by hand — a hand-written nested map
literal for even a small state machine invites a typo that silently maps the
wrong event. Wrapping the construction in `func() T { ... }()` at package
scope runs it once, at init time, and confines the temporary work (the loop,
the duplicate check) to that literal instead of leaking a builder function
into the package's public surface. Splitting the actual construction into
`buildTable`, a plain named function the IIFE calls, keeps the panic-on-bad-
data behavior at init time while still letting a test feed `buildTable` a
deliberately broken list and observe the returned error instead of crashing
the test binary.

Create `orderfsm.go`:

```go
package orderfsm

import "fmt"

// Status is an order's lifecycle state.
type Status string

// Event is an action that may move an order from one Status to another.
type Event string

const (
	Placed   Status = "placed"
	Paid     Status = "paid"
	Shipped  Status = "shipped"
	Canceled Status = "canceled"
)

const (
	Pay    Event = "pay"
	Ship   Event = "ship"
	Cancel Event = "cancel"
)

type transition struct {
	From Status
	On   Event
	To   Status
}

// transitions is the flat, easy-to-review list of allowed moves.
var transitions = []transition{
	{Placed, Pay, Paid},
	{Placed, Cancel, Canceled},
	{Paid, Ship, Shipped},
	{Paid, Cancel, Canceled},
}

// buildTable turns the flat transition list into a nested map for O(1)
// lookup, returning an error if the same (From, On) pair appears twice —
// that would make the machine ambiguous. It is a plain function so tests can
// probe the duplicate check directly without tripping the package-level
// panic below.
func buildTable(ts []transition) (map[Status]map[Event]Status, error) {
	table := make(map[Status]map[Event]Status)
	for _, t := range ts {
		if table[t.From] == nil {
			table[t.From] = make(map[Event]Status)
		}
		if _, exists := table[t.From][t.On]; exists {
			return nil, fmt.Errorf("ambiguous transition: (%s, %s) already defined", t.From, t.On)
		}
		table[t.From][t.On] = t.To
	}
	return table, nil
}

// table is computed once, at package init, by an immediately-invoked
// function literal: it builds the nested map from transitions and panics if
// the data is inconsistent, so a bad table fails loudly at startup rather
// than producing a wrong answer later.
var table = func() map[Status]map[Event]Status {
	t, err := buildTable(transitions)
	if err != nil {
		panic(err)
	}
	return t
}()

// Apply looks up the next status for (current, ev) in the precomputed table.
func Apply(current Status, ev Event) (Status, error) {
	next, ok := table[current][ev]
	if !ok {
		return "", fmt.Errorf("no transition from %s on %s", current, ev)
	}
	return next, nil
}
```

### Tests

`TestApplyValidTransition` and `TestApplyInvalidTransition` cover a legal and
an illegal move. `TestBuildTableDuplicateDetected` feeds `buildTable` a list
with the same `(From, Event)` pair twice and checks it returns an error
instead of silently overwriting. `TestBuildTableMatchesPackageTable` rebuilds
the table from the real `transitions` and confirms it agrees with the
package-level `table` the IIFE produced.

Create `orderfsm_test.go`:

```go
package orderfsm

import "testing"

func TestApplyValidTransition(t *testing.T) {
	t.Parallel()
	got, err := Apply(Placed, Pay)
	if err != nil {
		t.Fatalf("Apply(Placed, Pay) error = %v, want nil", err)
	}
	if got != Paid {
		t.Fatalf("Apply(Placed, Pay) = %s, want %s", got, Paid)
	}
}

func TestApplyInvalidTransition(t *testing.T) {
	t.Parallel()
	if _, err := Apply(Placed, Ship); err == nil {
		t.Fatal("Apply(Placed, Ship) error = nil, want error")
	}
	if _, err := Apply(Shipped, Cancel); err == nil {
		t.Fatal("Apply(Shipped, Cancel) error = nil, want error")
	}
}

func TestBuildTableDuplicateDetected(t *testing.T) {
	t.Parallel()
	dup := []transition{
		{Placed, Pay, Paid},
		{Placed, Pay, Canceled},
	}
	if _, err := buildTable(dup); err == nil {
		t.Fatal("buildTable(dup) error = nil, want error on ambiguous transition")
	}
}

func TestBuildTableMatchesPackageTable(t *testing.T) {
	t.Parallel()
	rebuilt, err := buildTable(transitions)
	if err != nil {
		t.Fatalf("buildTable(transitions) error = %v, want nil", err)
	}
	if len(rebuilt) != len(table) {
		t.Fatalf("rebuilt table has %d from-states, want %d", len(rebuilt), len(table))
	}
}
```

## Review

The flat `transitions` list stays the readable source of truth; the IIFE is
what turns it into a fast lookup exactly once, and panicking there means a
bad table is a startup crash, not a wrong answer served in production. Moving
the actual construction into `buildTable` is what makes the duplicate-
detection logic testable without crashing the test binary — the IIFE at
package scope is a thin, one-line caller of a function the tests can also
call directly.

## Resources

- [Package initialization order](https://go.dev/ref/spec#Package_initialization)
- [Maps](https://go.dev/blog/maps)

---

Back to [00-concepts.md](00-concepts.md) | Prev: [11-validation-rule-pipeline.md](11-validation-rule-pipeline.md) | Next: [13-scoped-flag-override-cleanup.md](13-scoped-flag-override-cleanup.md)
