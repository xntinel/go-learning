# Exercise 30: Choose SQL Transaction Isolation Based on Consistency vs Performance

**Nivel: Intermedio** — validacion rapida (un test corto).

Opening every transaction at `SERIALIZABLE` is the safe-sounding default
that quietly serializes every contended table in the database and tanks
throughput the moment traffic grows; opening every transaction at `READ
COMMITTED` is fast but lets a ledger balance read a value mid-transfer that
a concurrent write is still updating. The right choice depends entirely on
what the transaction is actually doing, and that choice only has to be
made once, at `BEGIN` time — which is exactly why it belongs in a small,
explicit mapping rather than being reasoned about ad hoc at every call
site that opens a transaction. This module is that mapping. It is
self-contained: its own `go mod init`, code, demo, and test.

## What you'll build

```text
txiso/                      independent module: example.com/transaction-isolation-level-selector
  go.mod                     go 1.24
  txiso.go                    package txiso; Requirement; IsolationLevel(Requirement) string
  cmd/demo/main.go             runnable demo over all three requirements plus an unrecognized value
  txiso_test.go                table over the three mapped requirements and the fail-closed default
```

- Implement: `IsolationLevel(r Requirement) string` — an expression switch over a closed `Requirement` enum, defaulting to the strictest level for any unrecognized value.
- Test: a table over `Serializability`, `ReadConsistency`, `PerformanceCritical`, and an out-of-range `Requirement` value.
- Verify: `go test -count=1 ./...`

Set up the module:

```bash
mkdir -p go-solutions/03-control-flow/03-switch-statements/30-transaction-isolation-level-selector/cmd/demo
cd go-solutions/03-control-flow/03-switch-statements/30-transaction-isolation-level-selector
go mod edit -go=1.24
```

### Why the default fails toward more consistency, not less

`Requirement` is a closed, three-value enum, and `IsolationLevel` is a
textbook expression switch: dispatch on a tag drawn from a closed set,
compared with `==`. The interesting design decision isn't the switch
itself — it's what the `default` case does. A `Requirement` value that
doesn't match any of the three constants can only arrive two ways in
practice: a future fourth requirement gets added to the enum and someone
forgets to add its case here (the "forgotten enum case" hazard the
concepts file calls out directly), or a corrupt/malformed value comes in
from deserialized configuration. Either way, the fail-closed answer for a
consistency-versus-performance trade is to fail *toward consistency* —
`SERIALIZABLE` is the strictest and slowest level, and choosing it as the
default means an unrecognized requirement costs throughput, never
correctness. The alternative — defaulting to `READ COMMITTED` because it's
"probably fine" — is exactly the kind of default that lets a rare, silent
data inconsistency slip into production, and unlike a slow query, a
consistency bug is often only discovered long after the transaction that
caused it has already committed and moved on.

Create `txiso.go`:

```go
// Package txiso maps an application's stated consistency requirement to
// the SQL isolation level it should open its transaction with. Choosing
// SERIALIZABLE everywhere is safe but serializes contended tables and
// tanks throughput; choosing READ COMMITTED everywhere is fast but lets a
// ledger balance read stale data mid-transfer. The mapping only has to run
// once, at transaction-start, which is exactly why it belongs in a small
// expression switch rather than being decided ad hoc at every call site.
package txiso

// Requirement is the closed set of consistency needs a transaction's
// caller declares before BEGIN.
type Requirement int

const (
	// Serializability is required when a transaction reads and later
	// writes based on a condition that must not have changed underneath
	// it -- a double-booking check, a ledger transfer, an inventory
	// decrement guarded by a stock check.
	Serializability Requirement = iota
	// ReadConsistency is required when a transaction must see a single
	// consistent snapshot across several reads, but doesn't need to
	// prevent concurrent writers from proceeding -- a multi-table report
	// generated from several SELECTs that must agree with each other.
	ReadConsistency
	// PerformanceCritical is declared when the caller has already reasoned
	// about the concurrency hazards and prioritizes throughput -- a
	// high-volume, single-row read or an idempotent append-only insert.
	PerformanceCritical
)

// IsolationLevel maps r to the SQL isolation level a transaction should
// request. An unrecognized Requirement -- one a future enum value forgot to
// add here, or a corrupt value from deserialized config -- resolves to
// SERIALIZABLE, the strictest and slowest level: for a decision that trades
// consistency against performance, failing closed means failing toward
// more consistency, never less, because a spurious anomaly is far harder
// to detect and repair after the fact than a spurious slowdown.
func IsolationLevel(r Requirement) string {
	switch r {
	case Serializability:
		return "SERIALIZABLE"
	case ReadConsistency:
		return "REPEATABLE READ"
	case PerformanceCritical:
		return "READ COMMITTED"
	default:
		return "SERIALIZABLE"
	}
}
```

### The runnable demo

Create `cmd/demo/main.go`:

```go
package main

import (
	"fmt"

	txiso "example.com/transaction-isolation-level-selector"
)

func main() {
	requirements := []txiso.Requirement{
		txiso.Serializability,
		txiso.ReadConsistency,
		txiso.PerformanceCritical,
		txiso.Requirement(99), // corrupt or forgotten value
	}

	for _, r := range requirements {
		fmt.Printf("requirement=%d -> %s\n", r, txiso.IsolationLevel(r))
	}
}
```

Run `go run ./cmd/demo`, expected output:

```
requirement=0 -> SERIALIZABLE
requirement=1 -> REPEATABLE READ
requirement=2 -> READ COMMITTED
requirement=99 -> SERIALIZABLE
```

### Tests

`TestIsolationLevel` runs a table over the three mapped requirements and
one unrecognized value, confirming it resolves to the strictest level
rather than the fastest one.

Create `txiso_test.go`:

```go
package txiso

import "testing"

func TestIsolationLevel(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name string
		r    Requirement
		want string
	}{
		{"serializability required", Serializability, "SERIALIZABLE"},
		{"read consistency required", ReadConsistency, "REPEATABLE READ"},
		{"performance critical", PerformanceCritical, "READ COMMITTED"},
		{"unrecognized requirement fails closed to the strictest level", Requirement(99), "SERIALIZABLE"},
	}

	for _, tc := range tests {
		if got := IsolationLevel(tc.r); got != tc.want {
			t.Errorf("%s: IsolationLevel(%d) = %q, want %q", tc.name, tc.r, got, tc.want)
		}
	}
}
```

Verify with:

```bash
go test -count=1 ./...
```

## Review

The selector is correct when each of the three declared requirements maps
to its intended isolation level and when an unrecognized requirement value
resolves to `SERIALIZABLE` rather than `READ COMMITTED` or any other
weaker level. Carry this forward: on any switch that trades two competing
concerns against each other (consistency vs. performance, cost vs.
capability, strictness vs. throughput), the `default` case is where that
trade-off's philosophy actually gets tested — decide, explicitly and in a
comment, which side of the trade the unknown case falls on, and make sure
it's the side that fails safe rather than the side that fails fast.

## Resources

- [Go Specification: Switch statements](https://go.dev/ref/spec#Switch_statements) — the expression switch form.
- [PostgreSQL: Transaction Isolation](https://www.postgresql.org/docs/current/transaction-iso.html) — what each isolation level actually guarantees and costs.
- [Jepsen: Consistency Models](https://jepsen.io/consistency) — a reference map of the consistency guarantees these levels sit between.

---

Back to [00-concepts.md](00-concepts.md) | Prev: [29-feature-flag-rule-evaluator.md](29-feature-flag-rule-evaluator.md) | Next: [31-bloom-filter-membership-checker.md](31-bloom-filter-membership-checker.md)
