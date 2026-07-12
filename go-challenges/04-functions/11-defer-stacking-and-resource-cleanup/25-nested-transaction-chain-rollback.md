# Exercise 25: Dependent Transaction Chain — Rollback in Dependency Order

**Nivel: Avanzado** — validacion normal (tabla de casos, incluye borde o concurrencia).

A chain of dependent transactions — debit one account, credit another,
write an audit entry — cannot be modeled as a flat list of independent
steps, because step three only makes sense if steps one and two already
happened. This module builds that chain as a small recursive function: each
step begins, defers its own rollback, and recurses into the next step. When
a deep step fails, the recursive calls unwind one frame at a time, and Go's
LIFO defer order does the rest — the most recently begun (most dependent)
step rolls back first, then the one before it, all the way back to the
start.

## What you'll build

```text
txchain/                    independent module: example.com/txchain
  go.mod
  txchain/txchain.go         Tx; NewChain; Run (recursive, defer-per-frame rollback)
  cmd/demo/main.go            three dependent steps; third fails; watch reverse rollback
  txchain/txchain_test.go     all succeed; mid-chain failure; first-step failure
```

- Files: `txchain/txchain.go`, `cmd/demo/main.go`, `txchain/txchain_test.go`.
- Implement: a `Tx` type with `Name` and an optional `Work func() error`; `NewChain` to wire a shared trace log into a slice of `*Tx`; and `Run(txs []*Tx) error`, which recursively begins each step, defers a rollback conditioned on the step's named return `err`, recurses into the next step, and commits on the way back out once every deeper step has succeeded.
- Test: all steps succeed and commit in reverse nesting order; a mid-chain step fails and only the steps that had already begun roll back, in reverse; a chain whose very first step fails rolls back nothing but itself, and later steps are never begun.
- Verify: `go test -count=1 ./...`

Set up the module:

```bash
go mod edit -go=1.24
```

### Why recursion, not a loop

A loop that defers inside it (like the cleanup-stack pattern elsewhere in
this chapter) also works, but it hides an important detail: dependent
transactions form a call stack, not a list. `Run(txs, 0)` calls `Run(txs,
1)`, which calls `Run(txs, 2)`, and so on — each frame is a real Go stack
frame, and each one registers exactly one `defer` for its own rollback
before recursing deeper. When the deepest frame fails, its `return`
triggers its own deferred rollback check first, then control returns to the
frame that called it, which sees the propagated error in its own named
return and runs its own rollback check, then returns to its caller, and so
on outward. The reverse order rollback happens for free — it is just how Go
already unwinds nested function calls — instead of something you have to
build by hand with an explicit stack slice.

On the success path the opposite happens: the deepest frame commits first
(nothing left to depend on), then control returns outward and each
enclosing frame commits after the work it depended on is already durable.

Create `txchain/txchain.go`:

```go
package txchain

import "fmt"

// Tx is one transactional resource in a dependency chain: each step depends
// on every step before it having begun successfully, so a failure anywhere
// in the chain must undo exactly the steps that already began, in reverse.
type Tx struct {
	Name string
	Work func() error // optional; nil means "begin succeeds, nothing else to do"

	begun     bool
	committed bool
	log       *[]string
}

func (t *Tx) begin() {
	t.begun = true
	*t.log = append(*t.log, "begin-"+t.Name)
}

func (t *Tx) commit() {
	t.committed = true
	*t.log = append(*t.log, "commit-"+t.Name)
}

func (t *Tx) rollback() {
	if !t.begun || t.committed {
		return
	}
	*t.log = append(*t.log, "rollback-"+t.Name)
}

// NewChain wires a shared trace log into every Tx so a test or demo can
// observe the exact order begin/work/commit/rollback happened in.
func NewChain(log *[]string, txs ...*Tx) []*Tx {
	for _, tx := range txs {
		tx.log = log
	}
	return txs
}

// Run executes the chain of dependent transactions in order via recursion:
// step i begins, defers its own rollback (armed unless the whole chain
// eventually succeeds), then recurses into step i+1. Because Go defers run
// LIFO as the recursive calls unwind, a failure at step k rolls back step
// k-1, then k-2, ... down to step 1 -- the reverse of the order the steps
// were begun in, which is also the reverse of their dependency order: the
// most dependent (most recently begun) step is undone first.
func Run(txs []*Tx) error {
	return runStep(txs, 0)
}

func runStep(txs []*Tx, i int) (err error) {
	if i == len(txs) {
		return nil
	}
	tx := txs[i]
	tx.begin()

	// Armed as soon as this step has begun; disarmed implicitly by err
	// staying nil once every deeper step (and this one) succeeds.
	defer func() {
		if err != nil {
			tx.rollback()
		}
	}()

	if tx.Work != nil {
		if wErr := tx.Work(); wErr != nil {
			err = fmt.Errorf("work %s: %w", tx.Name, wErr)
			return err
		}
	}

	if rErr := runStep(txs, i+1); rErr != nil {
		err = rErr
		return err
	}

	tx.commit()
	return nil
}
```

### The runnable demo

Create `cmd/demo/main.go`:

```go
package main

import (
	"fmt"

	"example.com/txchain/txchain"
)

func main() {
	var trace []string

	txs := txchain.NewChain(&trace,
		&txchain.Tx{Name: "debit-account-A"},
		&txchain.Tx{Name: "credit-account-B"},
		&txchain.Tx{Name: "audit-log", Work: func() error {
			return fmt.Errorf("audit service unreachable")
		}},
	)

	err := txchain.Run(txs)

	fmt.Println("err:", err)
	for _, e := range trace {
		fmt.Println(e)
	}
}
```

Run it:

```bash
go run ./cmd/demo
```

Expected output:

```
err: work audit-log: audit service unreachable
begin-debit-account-A
begin-credit-account-B
begin-audit-log
rollback-audit-log
rollback-credit-account-B
rollback-debit-account-A
```

### Tests

Create `txchain/txchain_test.go`:

```go
package txchain

import (
	"errors"
	"fmt"
	"slices"
	"testing"
)

func TestRunAllSucceedCommitsInReverseNestingOrder(t *testing.T) {
	t.Parallel()

	var trace []string
	txs := NewChain(&trace,
		&Tx{Name: "one"},
		&Tx{Name: "two"},
		&Tx{Name: "three"},
	)

	if err := Run(txs); err != nil {
		t.Fatalf("Run() = %v, want nil", err)
	}

	want := []string{
		"begin-one", "begin-two", "begin-three",
		"commit-three", "commit-two", "commit-one",
	}
	if !slices.Equal(trace, want) {
		t.Fatalf("trace = %v, want %v", trace, want)
	}
}

func TestRunMidChainFailureRollsBackInReverse(t *testing.T) {
	t.Parallel()

	var trace []string
	stepErr := errors.New("audit unreachable")
	txs := NewChain(&trace,
		&Tx{Name: "debit"},
		&Tx{Name: "credit"},
		&Tx{Name: "audit", Work: func() error { return stepErr }},
		&Tx{Name: "notify"}, // never begun: chain never reaches it
	)

	err := Run(txs)
	if !errors.Is(err, stepErr) {
		t.Fatalf("err = %v, want errors.Is %v", err, stepErr)
	}

	want := []string{
		"begin-debit", "begin-credit", "begin-audit",
		"rollback-audit", "rollback-credit", "rollback-debit",
	}
	if !slices.Equal(trace, want) {
		t.Fatalf("trace = %v, want %v", trace, want)
	}
}

func TestRunFirstStepFailsRollsBackNothing(t *testing.T) {
	t.Parallel()

	var trace []string
	stepErr := errors.New("cannot open")
	txs := NewChain(&trace,
		&Tx{Name: "one", Work: func() error { return stepErr }},
		&Tx{Name: "two"},
	)

	err := Run(txs)
	if !errors.Is(err, stepErr) {
		t.Fatalf("err = %v, want errors.Is %v", err, stepErr)
	}

	// The failing step began (and is rolled back, though it did nothing);
	// step "two" was never reached at all.
	want := []string{"begin-one", "rollback-one"}
	if !slices.Equal(trace, want) {
		t.Fatalf("trace = %v, want %v", trace, want)
	}
}

func Example() {
	var trace []string
	txs := NewChain(&trace,
		&Tx{Name: "a"},
		&Tx{Name: "b", Work: func() error { return fmt.Errorf("b failed") }},
	)
	_ = Run(txs)
	fmt.Println(trace)
	// Output: [begin-a begin-b rollback-b rollback-a]
}
```

## Review

The chain is correct when every step's rollback fires only if that step had
actually begun, when a mid-chain failure undoes exactly the steps already
begun and no more, and when a failure at the very first step rolls back
that step alone (or nothing, if the step never got as far as beginning).
The mistake this pattern exists to prevent is treating a chain of dependent
transactions as an independent list and rolling them back in the order they
were declared rather than the reverse of the order they actually began in —
which, for a chain where step three depends on step two which depends on
step one, would try to undo step one while step two's effects (which
depended on it) were still standing. Recursion makes the correct order
automatic: it is the same LIFO discipline as a stack of `defer` statements,
expressed as call frames instead.

## Resources

- [The Go Programming Language Specification: Defer statements](https://go.dev/ref/spec#Defer_statements)
- [errors.Is](https://pkg.go.dev/errors#Is)
- [Saga pattern (compensating transactions)](https://learn.microsoft.com/en-us/azure/architecture/patterns/saga)

---

Back to [00-concepts.md](00-concepts.md) | Prev: [24-read-lock-demote-upgrade-defer.md](24-read-lock-demote-upgrade-defer.md) | Next: [26-observable-stat-snapshot-defer.md](26-observable-stat-snapshot-defer.md)
