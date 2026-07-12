# Exercise 21: Write-Ahead Log — Rollback Entries in Reverse Order

**Nivel: Avanzado** — validacion normal (tabla de casos, incluye borde).

`08-cleanup-stack-lifo-rollback.md` built a cleanup stack that undoes
completed steps in reverse order and then forgets about them. This module
keeps a *permanent* record of what happened: every step attempted, and every
rollback performed, lands in an append-only write-ahead log that survives
the operation — success or failure — so it can be inspected or persisted
afterward. The rollback mechanics are the same LIFO shape; what's new is
that the log itself, not just the side effects, has to end up correct.

## What you'll build

```text
wal/                        independent module: example.com/wal
  go.mod
  wal/wal.go                  WAL (Log/Entries); Step; RunSteps (defer LIFO rollback + logging)
  wal/wal_test.go             table of cases: full success, mid-failure rollback, rollback-of-rollback failure, panic
  cmd/demo/main.go            runnable demo: a step fails, watch the rollback entry land in the log
```

- Files: `wal/wal.go`, `wal/wal_test.go`, `cmd/demo/main.go`.
- Implement: a concurrency-safe `WAL` with `Log(entry string)` and `Entries() []string`; a `Step{Name string; Do, Undo func() error}`; and `RunSteps(w *WAL, steps []Step) (err error)` that logs and runs each step in order, tracks which ones completed, and — in a deferred closure — rolls the completed ones back in reverse, logging both the rollback attempt and any rollback failure, joining rollback errors into the returned error, unless every step committed.
- Test: all steps succeed (log holds only the forward entries, nothing rolled back); a step fails partway (log shows the completed steps, then their rollbacks in reverse, and the step that never got a chance to run is absent); a rollback that itself fails (its failure is logged and joined into the returned error, but the *next* rollback in the reverse order still runs); a panic mid-step (completed steps still roll back before the panic re-propagates).
- Verify: `go test -count=1 -race ./...`

Set up the module:

```bash
mkdir -p go-solutions/04-functions/11-defer-stacking-and-resource-cleanup/21-write-ahead-log-rollback/wal go-solutions/04-functions/11-defer-stacking-and-resource-cleanup/21-write-ahead-log-rollback/cmd/demo
cd go-solutions/04-functions/11-defer-stacking-and-resource-cleanup/21-write-ahead-log-rollback
go mod edit -go=1.24
```

### The log is the deliverable, the rollback is just how it gets written

`RunSteps` logs a step's name *before* calling its `Do`, so the log records
an attempt was made even if that attempt is the one that fails — the very
next test asserts exactly this: `reserve-funds` appears in the log even
though its `Do` returned an error and it was never added to `completed`
(only steps whose `Do` succeeded get pushed there, because only those need
an `Undo`). The rollback entries are written the same way: `"rollback: " +
name` before calling `Undo`, and `"rollback-failed: " + name` if `Undo`
itself errors. Reading the log back after the fact tells you not just "what
happened" but "what was attempted and in what order," which a cleanup stack
that silently discards its state after `Run()` cannot give you.

### A rollback failing does not stop the rest of the rollback

`TestRunStepsRollbackFailureIsJoinedAndDoesNotStopFurtherRollback` is the
edge case this exercise exists to cover: `charge-payment`'s `Undo` fails (a
refund API is down, say), but `reserve-inventory`'s `Undo` — earlier in
acquisition order, later in rollback order — still has to run, because
skipping it would leave the inventory reservation held forever. The loop
never returns or breaks on an `Undo` error; it only appends to
`rollbackErrs` and keeps going, exactly the same "keep cleaning up even when
one step of cleanup fails" discipline the `Cleanup.Run()` stack in exercise
8 uses. `errors.Join` at the end combines the original failure with every
rollback failure into one error a caller can `errors.Is` against any of them.

Create `wal/wal.go`:

```go
package wal

import (
	"errors"
	"fmt"
	"sync"
)

// WAL is a minimal write-ahead log: an append-only, ordered audit trail of
// every step attempted and every rollback performed. Unlike a plain cleanup
// stack, the log itself is the artifact -- it survives and can be inspected
// (or persisted) even after the operation finishes, forward or rolled back.
type WAL struct {
	mu      sync.Mutex
	entries []string
}

// Log appends one entry.
func (w *WAL) Log(entry string) {
	w.mu.Lock()
	defer w.mu.Unlock()
	w.entries = append(w.entries, entry)
}

// Entries returns a defensive copy of the log so far, in append order.
func (w *WAL) Entries() []string {
	w.mu.Lock()
	defer w.mu.Unlock()
	out := make([]string, len(w.entries))
	copy(out, w.entries)
	return out
}

// Step is one unit of a multi-step operation: Do performs it, Undo reverses
// it. Undo is only ever called for a Step whose Do already succeeded.
type Step struct {
	Name string
	Do   func() error
	Undo func() error
}

// RunSteps logs and runs each step in order. If a step's Do fails (or the
// loop panics), a deferred closure walks the steps that DID complete in
// reverse -- LIFO, newest first -- logging and invoking each one's Undo. A
// rollback step's own failure does not stop the rest of the rollback from
// running; every rollback error is joined into the returned error alongside
// whatever caused the rollback in the first place. On full success nothing
// is undone and the log holds only the forward steps.
func RunSteps(w *WAL, steps []Step) (err error) {
	var completed []Step
	committed := false

	defer func() {
		r := recover()
		if !committed {
			var rollbackErrs []error
			for i := len(completed) - 1; i >= 0; i-- {
				s := completed[i]
				w.Log("rollback: " + s.Name)
				if uerr := s.Undo(); uerr != nil {
					w.Log("rollback-failed: " + s.Name)
					rollbackErrs = append(rollbackErrs, fmt.Errorf("rollback %s: %w", s.Name, uerr))
				}
			}
			if len(rollbackErrs) > 0 {
				err = errors.Join(append([]error{err}, rollbackErrs...)...)
			}
		}
		if r != nil {
			panic(r)
		}
	}()

	for _, s := range steps {
		w.Log(s.Name)
		if derr := s.Do(); derr != nil {
			return derr
		}
		completed = append(completed, s)
	}
	committed = true
	return nil
}
```

### The runnable demo

Create `cmd/demo/main.go`:

```go
package main

import (
	"errors"
	"fmt"

	"example.com/wal/wal"
)

func main() {
	w := &wal.WAL{}

	steps := []wal.Step{
		{
			Name: "create-account",
			Do:   func() error { fmt.Println("create-account: done"); return nil },
			Undo: func() error { fmt.Println("create-account: undone"); return nil },
		},
		{
			Name: "reserve-funds",
			Do:   func() error { return errors.New("insufficient funds") },
			Undo: func() error { return nil },
		},
	}

	err := wal.RunSteps(w, steps)
	fmt.Println("error:", err)
	fmt.Println("log:", w.Entries())
}
```

Run it:

```bash
go run ./cmd/demo
```

Expected output:

```
create-account: done
create-account: undone
error: insufficient funds
log: [create-account reserve-funds rollback: create-account]
```

### Tests

Create `wal/wal_test.go`:

```go
package wal

import (
	"errors"
	"slices"
	"testing"
)

func noop() error { return nil }

func TestRunStepsAllSucceedNothingRolledBack(t *testing.T) {
	t.Parallel()

	w := &WAL{}
	steps := []Step{
		{Name: "create-account", Do: noop, Undo: noop},
		{Name: "reserve-funds", Do: noop, Undo: noop},
		{Name: "send-receipt", Do: noop, Undo: noop},
	}

	if err := RunSteps(w, steps); err != nil {
		t.Fatalf("err = %v, want nil", err)
	}

	want := []string{"create-account", "reserve-funds", "send-receipt"}
	if !slices.Equal(w.Entries(), want) {
		t.Fatalf("Entries = %v, want %v", w.Entries(), want)
	}
}

func TestRunStepsMidFailureRollsBackCompletedStepsInReverse(t *testing.T) {
	t.Parallel()

	w := &WAL{}
	wantErr := errors.New("insufficient funds")
	ranSendReceipt := false

	steps := []Step{
		{Name: "create-account", Do: noop, Undo: noop},
		{Name: "reserve-funds", Do: func() error { return wantErr }, Undo: noop},
		{Name: "send-receipt", Do: func() error { ranSendReceipt = true; return nil }, Undo: noop},
	}

	err := RunSteps(w, steps)
	if !errors.Is(err, wantErr) {
		t.Fatalf("err = %v, want %v", err, wantErr)
	}
	if ranSendReceipt {
		t.Fatal("send-receipt must never run once reserve-funds fails")
	}

	want := []string{"create-account", "reserve-funds", "rollback: create-account"}
	if !slices.Equal(w.Entries(), want) {
		t.Fatalf("Entries = %v, want %v", w.Entries(), want)
	}
}

func TestRunStepsRollbackFailureIsJoinedAndDoesNotStopFurtherRollback(t *testing.T) {
	t.Parallel()

	w := &WAL{}
	wantErr := errors.New("payment declined")
	wantUndoErr := errors.New("compensating refund unavailable")
	secondUndone := false

	steps := []Step{
		{Name: "reserve-inventory", Do: noop, Undo: func() error { secondUndone = true; return nil }},
		{Name: "charge-payment", Do: noop, Undo: func() error { return wantUndoErr }},
		{Name: "ship-order", Do: func() error { return wantErr }, Undo: noop},
	}

	err := RunSteps(w, steps)
	if !errors.Is(err, wantErr) {
		t.Fatalf("err = %v, want it to wrap %v", err, wantErr)
	}
	if !errors.Is(err, wantUndoErr) {
		t.Fatalf("err = %v, want it to wrap %v", err, wantUndoErr)
	}
	if !secondUndone {
		t.Fatal("reserve-inventory's Undo must still run even though charge-payment's Undo failed")
	}

	want := []string{
		"reserve-inventory", "charge-payment", "ship-order",
		"rollback: charge-payment", "rollback-failed: charge-payment",
		"rollback: reserve-inventory",
	}
	if !slices.Equal(w.Entries(), want) {
		t.Fatalf("Entries = %v, want %v", w.Entries(), want)
	}
}

func TestRunStepsPanicMidStepRollsBackCompletedStepsThenRePanics(t *testing.T) {
	t.Parallel()

	w := &WAL{}
	undone := false
	steps := []Step{
		{Name: "open-batch", Do: noop, Undo: func() error { undone = true; return nil }},
		{Name: "process-record", Do: func() error { panic("corrupt record") }, Undo: noop},
	}

	func() {
		defer func() {
			if r := recover(); r == nil {
				t.Fatal("expected panic to propagate")
			}
		}()
		_ = RunSteps(w, steps)
	}()

	if !undone {
		t.Fatal("open-batch must be rolled back even though process-record panicked")
	}

	want := []string{"open-batch", "process-record", "rollback: open-batch"}
	if !slices.Equal(w.Entries(), want) {
		t.Fatalf("Entries = %v, want %v", w.Entries(), want)
	}
}
```

Verify:

```bash
go test -count=1 -race ./...
```

## Review

Four cases, one invariant: `completed` only ever holds steps whose `Do`
actually returned `nil`, so the deferred rollback only ever undoes real
work — a step that itself failed, or one that never ran because an earlier
step failed first, never appears in the reverse-order rollback and never
gets its `"rollback: "` entry. The rollback-failure case is the one worth
double-checking in any real implementation: a naive rollback loop that
`return`s the instant one `Undo` errors would abandon every rollback earlier
in the reverse order, leaking exactly the resources this whole exercise
exists to release. And because the log records rollbacks as data, not just
side effects, a test failure here points precisely at which step's forward
or backward action is missing or out of order — reading `w.Entries()` is
strictly more informative than asserting on side effects alone.

## Resources

- [Go Language Specification: Defer statements](https://go.dev/ref/spec#Defer_statements)
- [errors.Join](https://pkg.go.dev/errors#Join)
- [Write-ahead logging (PostgreSQL docs)](https://www.postgresql.org/docs/current/wal-intro.html) — the real-world WAL this module is a miniature of.

---

Back to [00-concepts.md](00-concepts.md) | Previous: [20-distributed-lock-unlock-timeout.md](20-distributed-lock-unlock-timeout.md) | Next: [22-multipart-upload-abort-defer.md](22-multipart-upload-abort-defer.md)
