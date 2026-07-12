# Exercise 12: Nested Savepoints — Deferred Release in Reverse Creation Order

**Nivel: Intermedio** — validacion rapida (un test corto).

A repository write that batches several risky inserts inside one transaction
often nests SQL savepoints: an outer savepoint around the whole write, a
middle one around a batch, an inner one around a single insert that might
violate a constraint. This module builds that nesting in-memory and proves
that stacking one `defer` per savepoint releases them in the only order a
database ever accepts: innermost first.

## What you'll build

```text
savepoint/                 independent module: example.com/nested-savepoint-release-order
  go.mod                    go 1.24
  savepoint.go              Tx.Begin(name) func(); Tx.Log(); Run(tx)
  savepoint_test.go         asserts the full create/release order
```

- Files: `savepoint.go`, `savepoint_test.go`.
- Implement: `(*Tx).Begin(name string) func()`, `(*Tx).Log() []string`, and `Run(tx *Tx)` which opens three nested savepoints.
- Test: run `Run` against a fresh `Tx` and assert the recorded log matches the exact create-then-release sequence.
- Verify: `go test -count=1 ./...`

Set up the module:

```bash
mkdir -p go-solutions/03-control-flow/07-defer-semantics-and-ordering/12-nested-savepoint-release-order
cd go-solutions/03-control-flow/07-defer-semantics-and-ordering/12-nested-savepoint-release-order
go mod edit -go=1.24
```

### Why LIFO is the only correct release order here

Each savepoint nests inside the one before it, the same way a pooled
connection nests a transaction which nests an advisory lock. Releasing them
in creation order — outer first — is not just suboptimal, it is invalid: you
cannot release an outer savepoint while an inner one is still open on top of
it. Deferring each release right where its savepoint is created turns the
required reverse order into something that falls out of `defer`'s LIFO rule
automatically, with no manual bookkeeping and no way to get the order wrong
by accident.

Create `savepoint.go`:

```go
package savepoint

// Tx models a transaction that supports nested savepoints. Real code wraps
// *sql.Tx and issues SAVEPOINT / RELEASE SAVEPOINT statements; this Tx only
// records the order operations happen in, which is exactly what the test
// below needs to verify.
type Tx struct {
	log []string
}

// Begin creates a savepoint named name and returns a release function meant
// to be deferred by the caller. Begin's side effect — recording that the
// savepoint was created — happens immediately, at the defer statement;
// the returned closure is what actually gets deferred, and it records the
// release when it runs at function return.
func (tx *Tx) Begin(name string) func() {
	tx.log = append(tx.log, "savepoint "+name)
	return func() {
		tx.log = append(tx.log, "release "+name)
	}
}

// Log returns the recorded operations in the order they happened.
func (tx *Tx) Log() []string {
	return tx.log
}

// Run opens three nested savepoints — outer, then middle, then inner — the
// way a repository method might: an outer savepoint around the whole write,
// a middle one around a batch, an inner one around a single risky insert.
// Deferring each release right after creating it means the LIFO order of
// defer unwinds them in the only order that is ever correct: innermost
// first, then middle, then outer. Reordering the three defer lines here
// would release a savepoint while one nested inside it is still open — a
// bug the database itself would reject.
func Run(tx *Tx) {
	defer tx.Begin("outer")()
	defer tx.Begin("middle")()
	defer tx.Begin("inner")()
}
```

### Test

`TestRun` runs `Run` once against a fresh `Tx` and checks the full recorded
sequence: three creations in nesting order, then three releases in the
reverse order.

Create `savepoint_test.go`:

```go
package savepoint

import "testing"

func TestRun(t *testing.T) {
	t.Parallel()

	tx := &Tx{}
	Run(tx)

	want := []string{
		"savepoint outer",
		"savepoint middle",
		"savepoint inner",
		"release inner",
		"release middle",
		"release outer",
	}

	got := tx.Log()
	if len(got) != len(want) {
		t.Fatalf("log = %v, want %v", got, want)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Errorf("log[%d] = %q, want %q", i, got[i], want[i])
		}
	}
}
```

Verify: `go test -count=1 ./...`

## Review

`Run` never mentions "reverse order" anywhere in its own body — it just
defers one release per savepoint, in the order each savepoint opens. The
correctness comes entirely from `defer`'s LIFO guarantee, not from any
explicit ordering logic a future maintainer could get wrong while adding a
fourth nested savepoint. That is the pattern to recognize: whenever a
resource must release in the exact reverse of its acquisition order, one
`defer` per acquisition is the whole solution, and hand-written release
ordering is a sign the code fought the language instead of using it.

## Resources

- [Go Specification: Defer statements](https://go.dev/ref/spec#Defer_statements) — "deferred function calls are executed in last-in-first-out order."
- [PostgreSQL: SAVEPOINT](https://www.postgresql.org/docs/current/sql-savepoint.html) — nested savepoints and why release order is constrained.

---

Back to [00-concepts.md](00-concepts.md) | Prev: [11-correlation-id-defer-argument-capture.md](11-correlation-id-defer-argument-capture.md) | Next: [13-inflight-request-gauge.md](13-inflight-request-gauge.md)
