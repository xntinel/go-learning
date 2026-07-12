# Exercise 3: Proving Teardown Runs Even When Assertions Fail

The reason transactional rollback belongs in a cleanup and not in a trailing
statement is that a failed assertion aborts the test *above* that statement. This
exercise builds a transaction fixture whose rollback is registered with `t.Cleanup`
and proves the rollback runs on the happy path, on an uncommitted path, and even
when the test body aborts the way `t.Fatal` does — by calling `runtime.Goexit`.

This module is fully self-contained: its own `go mod init`, its own demo, its own
tests. Nothing here imports any other exercise.

## What you'll build

```text
txfixture/                   independent module: example.com/txfixture
  go.mod                     go 1.24
  tx.go                      Tx (Exec/Commit/Rollback) with committed/rolledBack state
  cmd/
    demo/
      main.go                runnable demo: exec, rollback, observe cleared statements
  tx_test.go                 beginTx(t) fixture; rollback-on-uncommitted; abort proof
```

- Files: `tx.go`, `cmd/demo/main.go`, `tx_test.go`.
- Implement: a `Tx` that records statements and tracks whether it was committed or rolled back, with a rollback that is a no-op once committed.
- Test: a `beginTx(t)` fixture registering a `t.Cleanup` that rolls back unless committed; proofs that it rolls back on an uncommitted body and does not roll back after commit; a harness that reproduces `t.Fatal`'s `runtime.Goexit` abort and proves teardown still runs.
- Verify: `go test -count=1 -race ./...`

Set up the module:

```bash
go mod edit -go=1.24
```

### Why rollback belongs in Cleanup, and how we prove it

Consider the tempting shape: begin a transaction, run the test body, and at the
bottom write `if !committed { tx.Rollback() }`. The instant any assertion above
that line calls `t.Fatal`, the test goroutine unwinds and that trailing line never
runs — the transaction is left open. Registering the rollback with `t.Cleanup`
fixes this because `t.Fatal`/`t.FailNow` abort by calling `runtime.Goexit`, which
runs the goroutine's deferred functions, and the test runner's deferred teardown is
what invokes your cleanups. So the rollback runs on every exit path.

Proving "cleanup runs on `t.Fatal`" has a wrinkle: a genuinely failing subtest
marks its parent failed too, which would turn this module red and fail the gate. So
for the abort case we reproduce the exact mechanism `t.Fatal` uses rather than
calling it. `t.FailNow` is documented to call `runtime.Goexit`; cleanups run
because they are deferred and `Goexit` runs deferred functions. Our `abortLikeFatal`
harness runs a body in a goroutine, registers the teardown with a `defer` (mirroring
where the test runner invokes `t.Cleanup`), and lets the body call `runtime.Goexit`
(mirroring `t.Fatal`). The teardown still runs — which is precisely why real
`t.Cleanup` survives real `t.Fatal`. The happy-path and uncommitted-path tests use
the *actual* `t.Cleanup` API through the `beginTx` fixture.

Create `tx.go`:

```go
package txfixture

import "sync"

// Tx models a database transaction: statements accumulate, then the transaction
// is either committed or rolled back. Rollback after commit is a no-op.
type Tx struct {
	mu         sync.Mutex
	statements []string
	committed  bool
	rolledBack bool
}

// Begin starts an empty transaction.
func Begin() *Tx {
	return &Tx{}
}

// Exec records a statement in the transaction.
func (tx *Tx) Exec(stmt string) {
	tx.mu.Lock()
	defer tx.mu.Unlock()
	tx.statements = append(tx.statements, stmt)
}

// Commit finalizes the transaction; a subsequent Rollback does nothing.
func (tx *Tx) Commit() {
	tx.mu.Lock()
	defer tx.mu.Unlock()
	tx.committed = true
}

// Rollback discards pending statements unless the transaction was committed.
func (tx *Tx) Rollback() {
	tx.mu.Lock()
	defer tx.mu.Unlock()
	if tx.committed {
		return
	}
	tx.rolledBack = true
	tx.statements = nil
}

// Committed reports whether Commit was called.
func (tx *Tx) Committed() bool {
	tx.mu.Lock()
	defer tx.mu.Unlock()
	return tx.committed
}

// RolledBack reports whether Rollback discarded the transaction.
func (tx *Tx) RolledBack() bool {
	tx.mu.Lock()
	defer tx.mu.Unlock()
	return tx.rolledBack
}

// Pending reports how many statements are still buffered.
func (tx *Tx) Pending() int {
	tx.mu.Lock()
	defer tx.mu.Unlock()
	return len(tx.statements)
}
```

### The runnable demo

The demo shows the rollback clearing buffered statements.

Create `cmd/demo/main.go`:

```go
package main

import (
	"fmt"

	"example.com/txfixture"
)

func main() {
	tx := txfixture.Begin()
	tx.Exec("INSERT INTO orders (id) VALUES (1)")
	tx.Exec("UPDATE stock SET qty = qty - 1 WHERE id = 1")
	fmt.Printf("pending before rollback: %d\n", tx.Pending())

	tx.Rollback()
	fmt.Printf("rolled back: %v, pending: %d\n", tx.RolledBack(), tx.Pending())
}
```

Run it:

```bash
go run ./cmd/demo
```

Expected output:

```
pending before rollback: 2
rolled back: true, pending: 0
```

### The tests

`beginTx` is the real fixture: it registers `t.Cleanup` to roll back unless the
transaction was committed. `TestRollbackOnUncommittedBody` runs the body in a
subtest that never commits and, after the subtest returns, asserts the cleanup
rolled it back. `TestCommitSkipsRollback` commits inside the subtest and asserts the
cleanup did *not* roll back. `TestCleanupRunsAfterFatalAbort` uses the
`abortLikeFatal` harness to reproduce a `t.Fatal`-style abort and proves teardown
still ran.

Create `tx_test.go`:

```go
package txfixture

import (
	"runtime"
	"sync"
	"testing"
)

// beginTx starts a transaction and registers a cleanup that rolls it back unless
// it was committed. This is the real t.Cleanup API in action.
func beginTx(t *testing.T) *Tx {
	t.Helper()
	tx := Begin()
	t.Cleanup(func() {
		if !tx.Committed() {
			tx.Rollback()
		}
	})
	return tx
}

func TestRollbackOnUncommittedBody(t *testing.T) {
	t.Parallel()
	var tx *Tx
	t.Run("body", func(t *testing.T) {
		tx = beginTx(t)
		tx.Exec("INSERT INTO audit_log (event) VALUES ('login')")
		// No Commit: the fixture cleanup must roll back at subtest exit.
	})
	if !tx.RolledBack() {
		t.Fatal("uncommitted transaction was not rolled back by the cleanup")
	}
	if tx.Pending() != 0 {
		t.Fatalf("pending statements after rollback = %d, want 0", tx.Pending())
	}
}

func TestCommitSkipsRollback(t *testing.T) {
	t.Parallel()
	var tx *Tx
	t.Run("body", func(t *testing.T) {
		tx = beginTx(t)
		tx.Exec("INSERT INTO audit_log (event) VALUES ('login')")
		tx.Commit()
	})
	if tx.RolledBack() {
		t.Fatal("committed transaction must not be rolled back")
	}
	if !tx.Committed() {
		t.Fatal("transaction should be committed")
	}
}

// abortLikeFatal runs body in a goroutine with teardown registered as a defer,
// reproducing exactly how the test runner invokes t.Cleanup after t.Fatal calls
// runtime.Goexit. It lets us assert teardown-on-abort while staying green.
func abortLikeFatal(body, teardown func()) {
	var wg sync.WaitGroup
	wg.Add(1)
	go func() {
		defer wg.Done()
		defer teardown()
		body()
	}()
	wg.Wait()
}

func TestCleanupRunsAfterFatalAbort(t *testing.T) {
	t.Parallel()
	tx := Begin()
	abortLikeFatal(
		func() {
			tx.Exec("INSERT INTO audit_log (event) VALUES ('login')")
			// A failed assertion in a real test would call t.Fatal here, which
			// aborts via runtime.Goexit. We invoke Goexit directly.
			runtime.Goexit()
		},
		func() {
			if !tx.Committed() {
				tx.Rollback()
			}
		},
	)
	if !tx.RolledBack() {
		t.Fatal("teardown did not roll back after a Fatal-style abort")
	}
}
```

## Review

The fixture is correct when an uncommitted transaction is always rolled back at
test exit and a committed one never is — `TestRollbackOnUncommittedBody` and
`TestCommitSkipsRollback` pin both. The failure-survival contract is the real
lesson: `TestCleanupRunsAfterFatalAbort` reproduces `t.Fatal`'s `runtime.Goexit`
abort and shows the teardown still runs, which is why rollback goes in `t.Cleanup`
and not in a trailing statement. The subtle point to internalize: a `defer` written
at the same position as the cleanup would also run after `Goexit`, but only if it
sits in the test function itself; the value of `t.Cleanup` is that a *fixture* can
register it, so the caller cannot forget. Run `go test -race` to confirm the shared
transaction state is safe across the harness goroutine.

## Resources

- [`testing.T.Cleanup`](https://pkg.go.dev/testing#T.Cleanup) — runs on failure and on `FailNow`.
- [`testing.T.FailNow`](https://pkg.go.dev/testing#T.FailNow) — documented to stop the test by calling `runtime.Goexit`.
- [`runtime.Goexit`](https://pkg.go.dev/runtime#Goexit) — runs deferred functions before terminating the goroutine.

---

Back to [00-concepts.md](00-concepts.md) | Previous: [02-tempdir-config-scratch.md](02-tempdir-config-scratch.md) | Next: [04-lifo-teardown-order.md](04-lifo-teardown-order.md)
