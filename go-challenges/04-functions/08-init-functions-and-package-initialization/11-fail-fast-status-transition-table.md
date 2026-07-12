# Exercise 11: Fail-Fast init() — Validating an Order Status Transition Table

**Nivel: Intermedio** — validacion rapida (un test corto).

An order lifecycle is a fixed set of statuses and a table of legal
transitions between them. This exercise validates that table at `init()` —
every source and target status must be one of the declared statuses — so a
typo in the table crashes the binary at load instead of silently allowing (or
blocking) a transition in production.

## What you'll build

```text
orderfsm/                  independent module: example.com/orderfsm
  go.mod                    module example.com/orderfsm
  orderfsm.go                Status, transitions table, init() validation, CanTransition
  orderfsm_test.go           package-loaded proof + broken-table rejection + transition table test
```

Files: `orderfsm.go`, `orderfsm_test.go`.
Implement: a `transitions map[Status][]Status`, an `allStatuses` list, an extracted `validateTransitions` helper called from `init()`, and `CanTransition`.
Test: the package loads without panicking; `validateTransitions` rejects a table with an unknown source or target status; `CanTransition` matches the table for both legal and illegal moves.

Set up the module:

```bash
go mod edit -go=1.24
```

### Why this belongs in init(), not a runtime check

This is the second production-legitimate use of `init()`: the transition
table is static, known at build time, and either self-consistent or not.
Checking it once per request would be wasted work for an answer that never
changes; checking it lazily risks the first caller discovering the bug in
production. Panicking at load, before any traffic is accepted, is the
correct response to a build-time mistake — exactly like `regexp.MustCompile`
failing on a bad pattern.

Create `orderfsm.go`:

```go
// Package orderfsm models an order's lifecycle as a fixed transition table,
// validated for internal consistency at package initialization.
package orderfsm

import "fmt"

// Status is an order lifecycle state.
type Status string

const (
	StatusPending   Status = "pending"
	StatusPaid      Status = "paid"
	StatusShipped   Status = "shipped"
	StatusDelivered Status = "delivered"
	StatusCanceled  Status = "canceled"
)

// allStatuses lists every status the package knows about. It is the source
// of truth transitions is checked against.
var allStatuses = []Status{
	StatusPending, StatusPaid, StatusShipped, StatusDelivered, StatusCanceled,
}

// transitions maps each status to the set of statuses it may legally move
// to. An empty slice means the status is terminal.
var transitions = map[Status][]Status{
	StatusPending:   {StatusPaid, StatusCanceled},
	StatusPaid:      {StatusShipped, StatusCanceled},
	StatusShipped:   {StatusDelivered},
	StatusDelivered: {},
	StatusCanceled:  {},
}

func init() {
	if err := validateTransitions(allStatuses, transitions); err != nil {
		panic(err)
	}
}

// validateTransitions fails if the transition table references a source or
// target status that is not present in known. It is extracted from init so
// a test can drive it directly with a deliberately broken table.
func validateTransitions(known []Status, table map[Status][]Status) error {
	seen := make(map[Status]bool, len(known))
	for _, s := range known {
		seen[s] = true
	}
	for from, tos := range table {
		if !seen[from] {
			return fmt.Errorf("orderfsm: transition table has unknown source status %q", from)
		}
		for _, to := range tos {
			if !seen[to] {
				return fmt.Errorf("orderfsm: transition from %q targets unknown status %q", from, to)
			}
		}
	}
	return nil
}

// CanTransition reports whether moving from `from` to `to` is a legal
// transition according to the table.
func CanTransition(from, to Status) bool {
	for _, allowed := range transitions[from] {
		if allowed == to {
			return true
		}
	}
	return false
}
```

Create `orderfsm_test.go`:

```go
package orderfsm

import "testing"

// TestPackageLoaded proves init did not panic: if the shipped transition
// table were inconsistent, this whole test binary would never reach a test
// body because init() runs before any test function.
func TestPackageLoaded(t *testing.T) {
	if !CanTransition(StatusPending, StatusPaid) {
		t.Fatal("package failed to load a working transition table")
	}
}

func TestCanTransition(t *testing.T) {
	tests := []struct {
		name string
		from Status
		to   Status
		want bool
	}{
		{"pending_to_paid", StatusPending, StatusPaid, true},
		{"pending_to_canceled", StatusPending, StatusCanceled, true},
		{"pending_to_shipped_illegal", StatusPending, StatusShipped, false},
		{"paid_to_shipped", StatusPaid, StatusShipped, true},
		{"shipped_to_delivered", StatusShipped, StatusDelivered, true},
		{"delivered_is_terminal", StatusDelivered, StatusShipped, false},
		{"canceled_is_terminal", StatusCanceled, StatusPaid, false},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := CanTransition(tt.from, tt.to); got != tt.want {
				t.Errorf("CanTransition(%q, %q) = %v, want %v", tt.from, tt.to, got, tt.want)
			}
		})
	}
}

func TestValidateTransitionsRejectsUnknownSource(t *testing.T) {
	known := []Status{StatusPending, StatusPaid}
	broken := map[Status][]Status{
		"refunded": {StatusPaid}, // "refunded" is not in known
	}
	if err := validateTransitions(known, broken); err == nil {
		t.Fatal("validateTransitions did not reject an unknown source status")
	}
}

func TestValidateTransitionsRejectsUnknownTarget(t *testing.T) {
	known := []Status{StatusPending, StatusPaid}
	broken := map[Status][]Status{
		StatusPending: {"refunded"}, // "refunded" is not in known
	}
	if err := validateTransitions(known, broken); err == nil {
		t.Fatal("validateTransitions did not reject an unknown target status")
	}
}
```

Verify: `go test -count=1 ./...`

## Review

`init()` calling out to an extracted `validateTransitions` is the pattern to
copy: the panic-on-failure logic stays trivial, and the interesting logic is
a plain function a test can call directly with any table it likes, including
deliberately broken ones the real table will never contain. That is how
`TestValidateTransitionsRejectsUnknownSource` proves the check actually
catches a mistake, not just that the shipped table happens to pass.

## Resources

- [Go spec — Order of package initialization](https://go.dev/ref/spec#Package_initialization) — when `init()` runs relative to variables.
- [regexp.MustCompile](https://pkg.go.dev/regexp#MustCompile) — the standard library's own fail-fast-at-load pattern.

---

Back to [00-concepts.md](00-concepts.md) | Prev: [10-derived-lookup-table-init-order.md](10-derived-lookup-table-init-order.md) | Next: [12-idempotent-init-with-sync-once.md](12-idempotent-init-with-sync-once.md)
