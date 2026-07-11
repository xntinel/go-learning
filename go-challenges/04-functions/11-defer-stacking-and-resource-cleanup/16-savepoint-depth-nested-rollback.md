# Exercise 16: Nested Savepoints — LIFO Rollback Order With Deferred Cleanup

**Nivel: Avanzado** — validacion normal (tabla de casos, incluye borde).

A SQL transaction with nested `SAVEPOINT`s unwinds strictly innermost-first:
`ROLLBACK TO payment` before `ROLLBACK TO inventory` before `ROLLBACK TO order`.
That is exactly the order in which Go's own defer stack unwinds, which makes
nested savepoints one of the cleanest matches between a language feature and
a database concept. This module builds that nesting in memory, with each
level owning its own deferred rollback-unless-committed guard.

## What you'll build

```text
savepoint/                    independent module: example.com/savepoint
  go.mod
  savepoint/savepoint.go       Tx (Applied/RolledBack); withSavepoint; PlaceOrder (3 nested savepoints)
  savepoint/savepoint_test.go  table test: success, cascading failures, a recovered nested failure
  cmd/demo/main.go             runnable demo: a failed payment cascades through inventory and order
```

- Files: `savepoint/savepoint.go`, `savepoint/savepoint_test.go`, `cmd/demo/main.go`.
- Implement: `withSavepoint(tx *Tx, name string, work func() error) (err error)` that marks the transaction's current depth, defers a closure that rolls back to that mark and records the name in `tx.RolledBack` unless `work` returned `nil`; and `PlaceOrder(tx *Tx, failAt string, recoverPaymentFailure bool) (err error)`, which nests `order` around `inventory` around `payment`, with `failAt` forcing a failure at a chosen level and `recoverPaymentFailure` letting `inventory` swallow a failed payment instead of propagating it.
- Test: a table covering all-succeed (nothing rolled back), a payment failure that cascades through all three levels in LIFO order, an inventory failure that cascades through two levels, and a payment failure that `inventory` recovers from — leaving `inventory` and `order` committed and only `payment` rolled back.
- Verify: `go test -count=1 ./...`

Set up the module:

```bash
mkdir -p ~/go-exercises/savepoint/savepoint ~/go-exercises/savepoint/cmd/demo
cd ~/go-exercises/savepoint
go mod init example.com/savepoint
go mod edit -go=1.24
```

### Nesting withSavepoint calls IS acquiring nested savepoints

`withSavepoint` is the same commit-flag `defer` guard as
`14-staged-write-discard-unless-committed.md`, applied once per level instead
of once per batch: mark the current depth, run `work`, and roll back to the
mark unless `work` set `committed = true`. The nesting comes entirely from
how `PlaceOrder` calls it — `order`'s `work` closure itself calls
`withSavepoint(tx, "inventory", ...)`, whose `work` closure calls
`withSavepoint(tx, "payment", ...)`. Each call pushes one more deferred
rollback onto the *goroutine's* defer stack, in acquisition order:
order's defer first, then inventory's, then payment's. When `payment` fails,
Go unwinds that stack exactly backwards — payment's defer runs first, then
inventory's, then order's — which is precisely the sequence a database
applies nested `ROLLBACK TO` statements in. You get correct nested-savepoint
unwind ordering for free, just by nesting the calls; no explicit stack of
savepoint names is needed because Go's own call stack already is one.

### Not every inner failure has to cascade

The `recoverPaymentFailure` case is the one worth sitting with: `inventory`'s
`work` closure calls the `payment` savepoint, gets an error back, and — if
told to recover — returns `nil` anyway. `payment`'s own `withSavepoint` has
already rolled back `charge-payment` by the time `inventory` decides this,
because that rollback is `payment`'s own deferred closure, triggered the
moment its `work` returned non-nil. What `inventory` controls is only
*its own* commit flag: since `inventory`'s `work` now returns `nil`,
`inventory`'s savepoint commits, and so does `order`'s above it. This is the
same shape as an HTTP handler that catches a downstream timeout and serves a
cached fallback instead of a 500 — the failure is real and was cleaned up at
its own level, but it does not have to propagate past the level that knows
how to compensate for it.

Create `savepoint/savepoint.go`:

```go
package savepoint

import "errors"

// Tx is an in-memory stand-in for a SQL transaction. Applied is the ordered
// list of statements currently "committed" within the transaction; RolledBack
// records, in the order it happened, the name of every savepoint that had to
// unwind. Real code would issue SAVEPOINT / ROLLBACK TO / RELEASE against a
// database connection; the shape of the control flow is identical.
type Tx struct {
	Applied    []string
	RolledBack []string
}

func (tx *Tx) apply(stmt string) { tx.Applied = append(tx.Applied, stmt) }

// withSavepoint marks the transaction's current depth, runs work, and -- via
// a deferred closure -- rolls back to that mark unless work returned nil.
// Because Go's defer stack is itself LIFO, nesting withSavepoint calls
// acquires savepoints outer-to-inner and unwinds them inner-to-outer: the
// same order a database applies nested ROLLBACK TO statements in.
func withSavepoint(tx *Tx, name string, work func() error) (err error) {
	mark := len(tx.Applied)
	committed := false
	defer func() {
		if !committed {
			tx.Applied = tx.Applied[:mark]
			tx.RolledBack = append(tx.RolledBack, name)
		}
	}()

	if err := work(); err != nil {
		return err
	}
	committed = true
	return nil
}

// PlaceOrder chains three nested savepoints -- order, inventory, payment --
// mirroring a real checkout: creating the order record, reserving inventory,
// and charging payment. failAt forces a failure at that step ("inventory" or
// "payment"); recoverPaymentFailure, if true, makes the inventory step treat
// a failed payment as non-fatal (e.g. a free-trial fallback) instead of
// letting it cascade, demonstrating that only a savepoint whose error
// actually propagates past it gets rolled back.
func PlaceOrder(tx *Tx, failAt string, recoverPaymentFailure bool) (err error) {
	return withSavepoint(tx, "order", func() error {
		tx.apply("create-order")

		return withSavepoint(tx, "inventory", func() error {
			tx.apply("reserve-inventory")
			if failAt == "inventory" {
				return errors.New("inventory: out of stock")
			}

			err := withSavepoint(tx, "payment", func() error {
				tx.apply("charge-payment")
				if failAt == "payment" {
					return errors.New("payment: card declined")
				}
				return nil
			})
			if err != nil && recoverPaymentFailure {
				// The payment savepoint has already rolled back its own
				// statement by the time we get here; inventory's step still
				// succeeds, so its savepoint commits.
				return nil
			}
			return err
		})
	})
}
```

### The runnable demo

Create `cmd/demo/main.go`:

```go
package main

import (
	"fmt"

	"example.com/savepoint/savepoint"
)

func main() {
	tx := &savepoint.Tx{}
	err := savepoint.PlaceOrder(tx, "payment", false)
	fmt.Println("error:", err)
	fmt.Println("applied:", tx.Applied)
	fmt.Println("rolled back (LIFO order):", tx.RolledBack)
}
```

Run it:

```bash
go run ./cmd/demo
```

Expected output:

```
error: payment: card declined
applied: []
rolled back (LIFO order): [payment inventory order]
```

### Tests

Create `savepoint/savepoint_test.go`:

```go
package savepoint

import (
	"slices"
	"testing"
)

func TestPlaceOrder(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name                  string
		failAt                string
		recoverPaymentFailure bool
		wantErr               bool
		wantApplied           []string
		wantRolledBack        []string
	}{
		{
			name:           "all steps succeed, nothing rolled back",
			failAt:         "",
			wantErr:        false,
			wantApplied:    []string{"create-order", "reserve-inventory", "charge-payment"},
			wantRolledBack: nil,
		},
		{
			name:           "payment fails, cascades to inventory and order in LIFO order",
			failAt:         "payment",
			wantErr:        true,
			wantApplied:    nil,
			wantRolledBack: []string{"payment", "inventory", "order"},
		},
		{
			name:           "inventory fails, cascades to order only",
			failAt:         "inventory",
			wantErr:        true,
			wantApplied:    nil,
			wantRolledBack: []string{"inventory", "order"},
		},
		{
			name:                  "payment fails but is recovered, inventory and order still commit",
			failAt:                "payment",
			recoverPaymentFailure: true,
			wantErr:               false,
			wantApplied:           []string{"create-order", "reserve-inventory"},
			wantRolledBack:        []string{"payment"},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			tx := &Tx{}
			err := PlaceOrder(tx, tt.failAt, tt.recoverPaymentFailure)

			if (err != nil) != tt.wantErr {
				t.Fatalf("err = %v, wantErr %v", err, tt.wantErr)
			}
			if !slices.Equal(tx.Applied, tt.wantApplied) {
				t.Fatalf("Applied = %v, want %v", tx.Applied, tt.wantApplied)
			}
			if !slices.Equal(tx.RolledBack, tt.wantRolledBack) {
				t.Fatalf("RolledBack = %v, want %v", tx.RolledBack, tt.wantRolledBack)
			}
		})
	}
}
```

Verify:

```bash
go test -count=1 ./...
```

## Review

The four cases separate three ideas that are easy to blur together. First,
the happy path proves nothing rolls back when nothing fails. Second, the
payment-fails case proves the cascade is not just "roll back the failing
level" but a full LIFO unwind — `RolledBack` is `[payment, inventory, order]`,
in that exact order, because each level's own `defer` only fires once its own
`work` has returned, innermost first. Third, the inventory-fails case proves
the cascade depth matches where the error was actually raised — `payment`
never even ran, so it never appears in `RolledBack`. The recovered case is
the one to protect with a regression test: it is tempting to assume "a nested
savepoint failed" and "the enclosing savepoint rolls back" are the same fact,
and they are not — only a `work` closure that lets the error escape causes
its own level to roll back. Get the order of `committed = true` and the
`return err` wrong, and either a doomed transaction commits partial work or a
recovered one gets rolled back anyway.

## Resources

- [Go Language Specification: Defer statements](https://go.dev/ref/spec#Defer_statements)
- [PostgreSQL: SAVEPOINT](https://www.postgresql.org/docs/current/sql-savepoint.html)
- [database/sql: Tx](https://pkg.go.dev/database/sql#Tx) — the real transaction type this module stands in for.

---

Back to [00-concepts.md](00-concepts.md) | Previous: [15-request-scoped-accumulator-defer-flush.md](15-request-scoped-accumulator-defer-flush.md) | Next: [17-flag-flip-panic-restore.md](17-flag-flip-panic-restore.md)
