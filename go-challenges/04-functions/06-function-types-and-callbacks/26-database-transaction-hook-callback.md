# Exercise 26: Database Transaction Before/After Hooks via Function Type Callbacks

**Nivel: Avanzado** — validacion normal (tabla de casos, incluye concurrencia).

A transaction manager runs cross-cutting concerns — connection warmup, audit
logging, metrics — without letting them leak into the transaction body. This
module builds that as `BeforeHook`/`AfterHook` function types: every
registered before-hook can veto the transaction before the body ever runs,
and every after-hook observes the final outcome, commit or rollback, even
when a before-hook was the one that aborted it.

## What you'll build

```text
dbtx/                        independent module: example.com/database-transaction-hook-callback
  go.mod                      go 1.24
  dbtx.go                     type BeforeHook, type AfterHook, type Manager: Before, After, Run
  cmd/
    demo/
      main.go                  runnable demo: commit path, then a before-hook veto
  dbtx_test.go                 hook ordering, veto-before-body, after sees abort/body error, concurrency (-race)
```

Files: `dbtx.go`, `cmd/demo/main.go`, `dbtx_test.go`.
Implement: `type BeforeHook func(ctx, *Tx) error`, `type AfterHook func(ctx, *Tx, error)`, `Manager` with `Before(h)`, `After(h)`, and `Run(ctx, name, body)`; before-hooks run in order and any error aborts before `body` runs, after-hooks run in order and always see the final error (nil on commit).
Test: a successful run executes the body and the after-hook sees a nil error; a failing before-hook prevents the body from running; the after-hook still runs and sees the wrapped abort error via `errors.Is`; several before/after hooks run in registration order around the body; a body error reaches the after-hook unwrapped; concurrent hook registration and concurrent `Run` calls are race-free.
Verify: `go test -count=1 -race ./...`

Set up the module:

```bash
mkdir -p ~/go-exercises/database-transaction-hook-callback/cmd/demo
cd ~/go-exercises/database-transaction-hook-callback
go mod init example.com/database-transaction-hook-callback
go mod edit -go=1.24
```

### Why the after-hook must run even when the before-hook vetoes

A transaction has three moments worth intercepting without touching the
body's own code: right before it starts (warm a connection, check a
precondition), and right after it ends, whichever way it ended (log the
outcome, release a permit, emit a metric). Modeling both as ordinary
function types — `BeforeHook` and `AfterHook` — keeps `Manager.Run` in
charge of exactly one thing: sequencing. It runs every before-hook in
order and stops at the first error, wrapping it in `ErrHookAborted` so the
caller can tell "a hook said no" apart from "the body itself failed" via
`errors.Is`. Whether the transaction committed, was vetoed, or the body
returned its own error, `Run` uses a single `defer` to fan the *one* final
error out to every after-hook — that is what makes "the after-hook always
runs, and always sees the truth" a property of the code rather than a
convention every caller has to remember. The hook slices are copied under
`Manager`'s mutex before use, so a hook that registers another hook
concurrently with a `Run` in flight cannot race with the slice being
ranged over.

Create `dbtx.go`:

```go
// Package dbtx runs a transaction body between Before and After hooks
// registered as plain function types, so cross-cutting concerns like
// connection warmup, audit logging, and metrics never touch the body.
package dbtx

import (
	"context"
	"errors"
	"fmt"
	"sync"
)

// Tx is the handle passed to hooks and the transaction body. It carries a
// name for logging/tracing only; it is not a real database handle.
type Tx struct {
	Name string
}

// BeforeHook runs before the transaction body. Returning a non-nil error
// aborts the transaction: the body never runs.
type BeforeHook func(ctx context.Context, tx *Tx) error

// AfterHook runs after the transaction body (or after a Before hook aborted
// it), observing the final error. A nil err means the body committed.
type AfterHook func(ctx context.Context, tx *Tx, err error)

var (
	// ErrHookAborted wraps the error returned by a Before hook that vetoed
	// the transaction.
	ErrHookAborted = errors.New("transaction aborted by before-hook")
)

// Manager holds the hooks shared by every transaction run through it.
type Manager struct {
	mu     sync.Mutex
	before []BeforeHook
	after  []AfterHook
}

// NewManager returns an empty Manager.
func NewManager() *Manager {
	return &Manager{}
}

// Before registers h to run before every transaction, in registration order.
func (m *Manager) Before(h BeforeHook) {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.before = append(m.before, h)
}

// After registers h to run after every transaction, in registration order.
func (m *Manager) After(h AfterHook) {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.after = append(m.after, h)
}

// Run executes body inside a transaction. Every Before hook runs first, in
// order; the first one to return an error aborts the transaction and body
// never runs. Every After hook then runs, in order, observing the final
// error (nil on success, the Before hook's wrapped error on abort, or
// body's own error).
func (m *Manager) Run(ctx context.Context, name string, body func(ctx context.Context, tx *Tx) error) (err error) {
	tx := &Tx{Name: name}

	m.mu.Lock()
	befores := append([]BeforeHook(nil), m.before...)
	afters := append([]AfterHook(nil), m.after...)
	m.mu.Unlock()

	defer func() {
		for _, h := range afters {
			h(ctx, tx, err)
		}
	}()

	for _, h := range befores {
		if berr := h(ctx, tx); berr != nil {
			err = fmt.Errorf("%w: %w", ErrHookAborted, berr)
			return err
		}
	}

	err = body(ctx, tx)
	return err
}
```

### The runnable demo

Create `cmd/demo/main.go`:

```go
package main

import (
	"context"
	"errors"
	"fmt"

	"example.com/database-transaction-hook-callback"
)

func main() {
	m := dbtx.NewManager()
	m.Before(func(ctx context.Context, tx *dbtx.Tx) error {
		fmt.Printf("before: warming connection for %s\n", tx.Name)
		if tx.Name == "refund-order" {
			return errors.New("insufficient balance")
		}
		return nil
	})
	m.After(func(ctx context.Context, tx *dbtx.Tx, err error) {
		if err != nil {
			fmt.Printf("after: %s rolled back: %v\n", tx.Name, err)
			return
		}
		fmt.Printf("after: %s committed\n", tx.Name)
	})

	ctx := context.Background()

	_ = m.Run(ctx, "create-order", func(ctx context.Context, tx *dbtx.Tx) error {
		fmt.Println("body: insert order row")
		return nil
	})

	_ = m.Run(ctx, "refund-order", func(ctx context.Context, tx *dbtx.Tx) error {
		fmt.Println("body: this should not print")
		return nil
	})
}
```

Run it:

```bash
go run ./cmd/demo
```

Expected output:

```
before: warming connection for create-order
body: insert order row
after: create-order committed
before: warming connection for refund-order
after: refund-order rolled back: transaction aborted by before-hook: insufficient balance
```

### Tests

Create `dbtx_test.go`:

```go
package dbtx

import (
	"context"
	"errors"
	"sync"
	"testing"
)

func TestRunCommitsAndRunsAfterHook(t *testing.T) {
	t.Parallel()
	m := NewManager()
	var afterErr error
	afterCalled := false
	m.After(func(ctx context.Context, tx *Tx, err error) {
		afterCalled = true
		afterErr = err
	})

	bodyRan := false
	err := m.Run(context.Background(), "tx1", func(ctx context.Context, tx *Tx) error {
		bodyRan = true
		return nil
	})
	if err != nil {
		t.Fatalf("Run returned %v, want nil", err)
	}
	if !bodyRan {
		t.Fatal("body did not run")
	}
	if !afterCalled {
		t.Fatal("after hook did not run")
	}
	if afterErr != nil {
		t.Fatalf("after hook saw err %v, want nil", afterErr)
	}
}

func TestBeforeHookAbortsBeforeBodyRuns(t *testing.T) {
	t.Parallel()
	m := NewManager()
	sentinel := errors.New("balance check failed")
	m.Before(func(ctx context.Context, tx *Tx) error {
		return sentinel
	})

	bodyRan := false
	err := m.Run(context.Background(), "tx1", func(ctx context.Context, tx *Tx) error {
		bodyRan = true
		return nil
	})
	if bodyRan {
		t.Fatal("body ran despite a failing before-hook")
	}
	if !errors.Is(err, ErrHookAborted) {
		t.Fatalf("err = %v, want wrapping ErrHookAborted", err)
	}
}

func TestAfterHookSeesAbortError(t *testing.T) {
	t.Parallel()
	m := NewManager()
	sentinel := errors.New("balance check failed")
	m.Before(func(ctx context.Context, tx *Tx) error {
		return sentinel
	})
	var seen error
	m.After(func(ctx context.Context, tx *Tx, err error) {
		seen = err
	})

	_ = m.Run(context.Background(), "tx1", func(ctx context.Context, tx *Tx) error {
		return nil
	})
	if !errors.Is(seen, ErrHookAborted) || !errors.Is(seen, sentinel) {
		t.Fatalf("after hook saw %v, want it to wrap both ErrHookAborted and the original error", seen)
	}
}

func TestMultipleBeforeAndAfterHooksRunInOrder(t *testing.T) {
	t.Parallel()
	m := NewManager()
	var order []string
	m.Before(func(ctx context.Context, tx *Tx) error { order = append(order, "before1"); return nil })
	m.Before(func(ctx context.Context, tx *Tx) error { order = append(order, "before2"); return nil })
	m.After(func(ctx context.Context, tx *Tx, err error) { order = append(order, "after1") })
	m.After(func(ctx context.Context, tx *Tx, err error) { order = append(order, "after2") })

	_ = m.Run(context.Background(), "tx1", func(ctx context.Context, tx *Tx) error {
		order = append(order, "body")
		return nil
	})

	want := []string{"before1", "before2", "body", "after1", "after2"}
	if len(order) != len(want) {
		t.Fatalf("order = %v, want %v", order, want)
	}
	for i := range want {
		if order[i] != want[i] {
			t.Fatalf("order = %v, want %v", order, want)
		}
	}
}

func TestBodyErrorReachesAfterHook(t *testing.T) {
	t.Parallel()
	m := NewManager()
	bodyErr := errors.New("insert failed")
	var seen error
	m.After(func(ctx context.Context, tx *Tx, err error) { seen = err })

	err := m.Run(context.Background(), "tx1", func(ctx context.Context, tx *Tx) error {
		return bodyErr
	})
	if !errors.Is(err, bodyErr) {
		t.Fatalf("Run err = %v, want %v", err, bodyErr)
	}
	if !errors.Is(seen, bodyErr) {
		t.Fatalf("after hook saw %v, want %v", seen, bodyErr)
	}
}

func TestConcurrentRegistrationAndRunIsRaceFree(t *testing.T) {
	t.Parallel()
	m := NewManager()
	var mu sync.Mutex
	count := 0

	var wg sync.WaitGroup
	for i := range 50 {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			if i%2 == 0 {
				m.Before(func(ctx context.Context, tx *Tx) error { return nil })
			} else {
				m.After(func(ctx context.Context, tx *Tx, err error) {
					mu.Lock()
					count++
					mu.Unlock()
				})
			}
		}(i)
	}
	wg.Wait()

	for range 20 {
		wg.Add(1)
		go func() {
			defer wg.Done()
			_ = m.Run(context.Background(), "concurrent", func(ctx context.Context, tx *Tx) error {
				return nil
			})
		}()
	}
	wg.Wait()
}
```

## Review

`Run` is correct when it reduces to two guarantees: no before-hook error
ever lets the body run, and no matter which of the three outcomes happens
— commit, before-hook veto, or body error — every after-hook sees that
exact final error exactly once. `TestBeforeHookAbortsBeforeBodyRuns` pins
down the veto, `TestAfterHookSeesAbortError` pins down that the veto is
still visible downstream with `errors.Is` matching both the wrapper and
the original cause, and `TestMultipleBeforeAndAfterHooksRunInOrder` pins
down that hooks compose in registration order rather than some
map-iteration order. The concurrency test is not about hook logic at all
— it is about the `Manager`'s own bookkeeping, proving that registering
hooks while transactions are running never corrupts the slice `Run`
copies under the lock, which is the one piece of shared mutable state the
package owns directly.

## Resources

- [Go Specification: Function types](https://go.dev/ref/spec#Function_types)
- [errors.Is and error wrapping](https://pkg.go.dev/errors#Is)
- [database/sql: BeginTx](https://pkg.go.dev/database/sql#DB.BeginTx)

---

Back to [00-concepts.md](00-concepts.md) | Prev: [25-data-pipeline-transform-callback.md](25-data-pipeline-transform-callback.md) | Next: [27-fsm-transition-callback-handler.md](27-fsm-transition-callback-handler.md)
