# Exercise 26: Multi-Phase Transaction: Sequential Rollback When Any Step Panics

**Nivel: Avanzado** — validacion normal (tabla de casos, incluye borde o concurrencia).

An order-processing pipeline that opens a transaction, inserts a row,
updates inventory, and commits is a multi-phase operation without a single
underlying database transaction to lean on — the steps often span separate
services (a payments API, an inventory service, a message queue), so
"rollback" means explicitly undoing each already-completed step yourself,
in reverse. The step that fails can panic instead of returning a clean
error, and — the harder problem — the compensating rollback action for an
earlier step can *also* panic (a delete hitting a foreign-key constraint,
a client library bug in the undo path). A cleanup failure must never erase
the original failure that triggered the rollback in the first place; an
on-call engineer needs both. This module builds `Run`, a sequential
multi-step executor with reverse-order rollback that keeps the primary
failure and any rollback failures in clearly separate fields. It is fully
self-contained: its own module, demo, and tests.

## What you'll build

```text
txn/                        independent module: example.com/txn
  go.mod                     go 1.24
  txn.go                      Step, Result, Run, runStep, runRollback
  cmd/
    demo/
      main.go                runnable demo: begin/insert/update/commit, update panics, insert's rollback also panics
  txn_test.go                  all succeed, reverse-order rollback, rollback panic preserves original error, empty
```

Files: `txn.go`, `cmd/demo/main.go`, `txn_test.go`.
Implement: `Run(steps []Step) Result` that executes each step's `Do` in order, and on the first failure - whether an ordinary error or a panic - rolls back every already-applied step's `Rollback` in reverse order, isolating each rollback's own possible panic so it can never destroy `Result.Err`.
Test: all steps succeeding (no rollback activity); a middle step panicking with only the earlier step rolled back, in the right order; a step's rollback itself panicking, asserting `Result.Err` still names the original failing step and `RollbackErrs` separately records the rollback panic; an empty step list.
Verify: `go test -count=1 ./...`

Set up the module:

```bash
mkdir -p ~/go-exercises/multi-phase-rollback-transaction/cmd/demo
cd ~/go-exercises/multi-phase-rollback-transaction
go mod init example.com/txn
go mod edit -go=1.24
```

### Why rollback runs in reverse order, and why its panics live in a separate field from Err

`Run` only rolls back steps that actually completed (`appliedSteps`), never
the step that failed itself — that step never reached a consistent state
worth undoing, and by construction its `Do` never mutated anything durable
if it panicked partway through (or if it did, that is a defect in the step,
not something this executor can compensate for). The already-applied steps
are undone in reverse: `update_inventory`'s rollback, if it existed, would
need to run before `insert_order`'s, because a later step frequently
depends on state an earlier step created (an inventory adjustment keyed off
a row that `insert_order` just inserted). Undoing in forward order would
try to compensate for dependent state before the state it depends on is
gone, which is backwards from how the steps were built up in the first
place.

The harder design decision is what happens when a rollback step itself
panics. A naive implementation might let that panic simply propagate,
which would abort the rollback loop entirely — every step *before* the one
whose rollback panicked would be left un-rolled-back with no record of why,
and worse, the panic would have completely replaced the original failure
in whatever error value eventually surfaces to the caller ("last panic
wins" is exactly the trap `runtime/debug`-based crash reporting falls into
when cleanup is not defended). `Run` avoids both problems: `runRollback` is
its own recover boundary, structurally identical to `runStep`'s but kept
entirely separate, and its result is accumulated into `Result.RollbackErrs`
- a distinct slice from `Result.Err`. No rollback panic, however severe,
can ever overwrite the field holding the reason the transaction aborted in
the first place; a caller inspecting `Result` always finds the true root
cause and, independently, complete information about how much of the
cleanup succeeded.

Create `txn.go`:

```go
package txn

import "fmt"

// Step is one phase of a multi-phase transaction: Do performs the phase,
// Rollback undoes it. Both may panic - Do because a step's own logic has a
// bug, Rollback because undoing a phase (a delete, a compensating API call)
// can fail in ways the step's author did not anticipate.
type Step struct {
	Name     string
	Do       func() error
	Rollback func() error
}

// Result is the outcome of running a full Run.
type Result struct {
	Applied      []string // steps whose Do succeeded, in the order they ran
	RolledBack   []string // steps whose Rollback was invoked, in reverse of Applied
	Err          error     // the failure that triggered rollback; nil on full success
	RollbackErrs []error   // one entry per rollback that itself failed or panicked
}

// Run executes steps in order. If every step's Do succeeds, Run returns a
// Result with no Err and no rollback activity. If a step's Do fails - by
// returning an error or by panicking - Run stops advancing, and rolls back
// every step that had already succeeded, in reverse order (last applied,
// first undone), which is the only order that is safe when later steps may
// depend on state earlier steps created.
//
// A rollback step can itself panic (a compensating delete hitting a foreign
// key constraint, a client library bug). That must never destroy the
// original failure: Result.Err always holds exactly the error that caused
// the transaction to abort, and any rollback failures are accumulated
// separately in RollbackErrs, so a cleanup-time panic can degrade the
// rollback's completeness without ever making the caller lose track of why
// the transaction failed in the first place.
func Run(steps []Step) Result {
	var result Result
	var appliedSteps []Step

	for _, s := range steps {
		if err := runStep(s); err != nil {
			result.Err = fmt.Errorf("step %q failed: %w", s.Name, err)
			break
		}
		appliedSteps = append(appliedSteps, s)
		result.Applied = append(result.Applied, s.Name)
	}

	if result.Err == nil {
		return result
	}

	for i := len(appliedSteps) - 1; i >= 0; i-- {
		s := appliedSteps[i]
		if err := runRollback(s); err != nil {
			result.RollbackErrs = append(result.RollbackErrs, fmt.Errorf("rollback %q failed: %w", s.Name, err))
		}
		result.RolledBack = append(result.RolledBack, s.Name)
	}

	return result
}

// runStep is the recover boundary around one step's forward action.
func runStep(s Step) (err error) {
	defer func() {
		if r := recover(); r != nil {
			if e, ok := r.(error); ok {
				err = fmt.Errorf("panicked: %w", e)
				return
			}
			err = fmt.Errorf("panicked: %v", r)
		}
	}()
	return s.Do()
}

// runRollback is the recover boundary around one step's compensating
// action, isolated from runStep so a rollback panic can never be confused
// with - or overwrite - the forward-phase failure that triggered it.
func runRollback(s Step) (err error) {
	defer func() {
		if r := recover(); r != nil {
			if e, ok := r.(error); ok {
				err = fmt.Errorf("panicked: %w", e)
				return
			}
			err = fmt.Errorf("panicked: %v", r)
		}
	}()
	return s.Rollback()
}
```

### The runnable demo

Four steps model an order-processing transaction: `begin`, `insert_order`,
`update_inventory`, and `commit`. `update_inventory` panics on an
out-of-range warehouse lookup, so `commit` is never reached; `insert_order`'s
own rollback (a compensating delete) panics with a foreign-key violation,
demonstrating that this does not erase the original `update_inventory`
failure.

Create `cmd/demo/main.go`:

```go
package main

import (
	"fmt"

	"example.com/txn"
)

func main() {
	steps := []txn.Step{
		{
			Name:     "begin",
			Do:       func() error { fmt.Println("begin: transaction opened"); return nil },
			Rollback: func() error { fmt.Println("rollback begin: transaction closed"); return nil },
		},
		{
			Name:     "insert_order",
			Do:       func() error { fmt.Println("insert_order: row inserted"); return nil },
			Rollback: func() error { panic("delete failed: foreign key constraint") },
		},
		{
			Name: "update_inventory",
			Do: func() error {
				var warehouses []string
				fmt.Println(warehouses[3]) // index out of range: panics
				return nil
			},
			Rollback: func() error { return nil },
		},
		{
			Name:     "commit",
			Do:       func() error { fmt.Println("commit: never reached"); return nil },
			Rollback: func() error { return nil },
		},
	}

	result := txn.Run(steps)

	fmt.Println("applied:", result.Applied)
	fmt.Println("rolled back:", result.RolledBack)
	fmt.Println("err:", result.Err)
	for _, e := range result.RollbackErrs {
		fmt.Println("rollback error:", e)
	}
}
```

Run it:

```bash
go run ./cmd/demo
```

Expected output:

```
begin: transaction opened
insert_order: row inserted
rollback begin: transaction closed
applied: [begin insert_order]
rolled back: [insert_order begin]
err: step "update_inventory" failed: panicked: runtime error: index out of range [3] with length 0
rollback error: rollback "insert_order" failed: panicked: delete failed: foreign key constraint
```

### Tests

`TestRunRollsBackInReverseOnPanic` confirms only the steps that actually
succeeded get rolled back, in the correct reverse order.
`TestRunPreservesOriginalErrorWhenRollbackPanics` mirrors the demo's harder
case: a rollback panics, and the test asserts `Result.Err` still names the
original failing step and message, while the rollback panic lands
separately in `RollbackErrs`.

Create `txn_test.go`:

```go
package txn

import (
	"errors"
	"strings"
	"testing"
)

func TestRunAllStepsSucceed(t *testing.T) {
	steps := []Step{
		{Name: "a", Do: func() error { return nil }, Rollback: func() error { return nil }},
		{Name: "b", Do: func() error { return nil }, Rollback: func() error { return nil }},
	}
	result := Run(steps)
	if result.Err != nil {
		t.Fatalf("Err = %v, want nil", result.Err)
	}
	if len(result.Applied) != 2 || len(result.RolledBack) != 0 {
		t.Fatalf("result = %+v, want 2 applied, 0 rolled back", result)
	}
}

func TestRunRollsBackInReverseOnPanic(t *testing.T) {
	steps := []Step{
		{Name: "step1", Do: func() error { return nil }, Rollback: func() error { return nil }},
		{Name: "step2", Do: func() error { panic(errors.New("bad state")) }, Rollback: func() error { return nil }},
		{Name: "step3", Do: func() error { return nil }, Rollback: func() error { return nil }},
	}
	result := Run(steps)

	if len(result.Applied) != 1 || result.Applied[0] != "step1" {
		t.Fatalf("Applied = %v, want [step1]", result.Applied)
	}
	if len(result.RolledBack) != 1 || result.RolledBack[0] != "step1" {
		t.Fatalf("RolledBack = %v, want [step1]", result.RolledBack)
	}
	if result.Err == nil || !strings.Contains(result.Err.Error(), "step2") || !strings.Contains(result.Err.Error(), "bad state") {
		t.Fatalf("Err = %v, want it to name step2 and wrap bad state", result.Err)
	}
	if len(result.RollbackErrs) != 0 {
		t.Fatalf("RollbackErrs = %v, want none", result.RollbackErrs)
	}
}

func TestRunPreservesOriginalErrorWhenRollbackPanics(t *testing.T) {
	steps := []Step{
		{Name: "begin", Do: func() error { return nil }, Rollback: func() error { return nil }},
		{Name: "insert", Do: func() error { return nil }, Rollback: func() error { panic("fk violation") }},
		{Name: "update", Do: func() error {
			var rows []int
			return errors.New(string(rune(rows[0]))) // index out of range: panics before returning
		}, Rollback: func() error { return nil }},
	}
	result := Run(steps)

	if result.Err == nil || !strings.Contains(result.Err.Error(), "update") {
		t.Fatalf("Err = %v, want it to name the update step", result.Err)
	}
	if !strings.Contains(result.Err.Error(), "index out of range") {
		t.Fatalf("Err = %v, want the original panic message preserved", result.Err)
	}

	if len(result.RolledBack) != 2 || result.RolledBack[0] != "insert" || result.RolledBack[1] != "begin" {
		t.Fatalf("RolledBack = %v, want [insert begin]", result.RolledBack)
	}
	if len(result.RollbackErrs) != 1 || !strings.Contains(result.RollbackErrs[0].Error(), "fk violation") {
		t.Fatalf("RollbackErrs = %v, want one entry about fk violation", result.RollbackErrs)
	}
}

func TestRunEmptySteps(t *testing.T) {
	result := Run(nil)
	if result.Err != nil || len(result.Applied) != 0 || len(result.RolledBack) != 0 {
		t.Fatalf("result = %+v, want a fully empty result", result)
	}
}
```

## Review

`Run` is correct when the rollback order is always the exact reverse of the
apply order and when a rollback's own panic degrades only the completeness
of the cleanup, never the caller's ability to see why the transaction
failed in the first place. The single most consequential design decision
here is keeping `Err` and `RollbackErrs` in separate fields rather than
merging them: a merged single error value invites exactly the "last panic
wins" bug this exercise defends against, where a cosmetic rollback failure
during cleanup silently buries the real, actionable root cause an operator
needs to see first.

## Resources

- [Go Blog: Defer, Panic, and Recover](https://go.dev/blog/defer-panic-and-recover) — the isolated recover boundaries `runStep` and `runRollback` both rely on.
- [errors package](https://pkg.go.dev/errors) — wrapping a step's failure with `%w` so a caller can still `errors.As`/`errors.Is` through the rollback bookkeeping.
- [Saga pattern (compensating transactions)](https://microservices.io/patterns/data/saga.html) — the distributed-systems pattern this module's reverse-order rollback implements in miniature.

---

Back to [00-concepts.md](00-concepts.md) | Prev: [25-mutex-critical-section-panic.md](25-mutex-critical-section-panic.md) | Next: [27-request-coalescing-singleflight.md](27-request-coalescing-singleflight.md)
