# Exercise 27: Schema Migration Sequence — Stack Down Operations for Rollback

**Nivel: Avanzado** — validacion normal (tabla de casos, incluye borde o concurrencia).

Applying a sequence of schema migrations is inherently dynamic: you do not
know in advance how many will run before one fails. This module builds a
migration runner that pushes each migration's own down-operation onto a
LIFO stack as it is applied, and — on failure or panic — unwinds that stack
in reverse so the schema lands back exactly where it started, never in a
half-migrated state.

## What you'll build

```text
migrate/                    independent module: example.com/migrate
  go.mod
  migrate/migrate.go          Migration; Apply (LIFO down-stack; version tracking)
  cmd/demo/main.go             three migrations; third fails; watch reverse downs
  migrate/migrate_test.go      mid-failure rollback; full success; down error; panic
```

- Files: `migrate/migrate.go`, `cmd/demo/main.go`, `migrate/migrate_test.go`.
- Implement: a `Migration{Name string; Up, Down func() error}`; and `Apply(migrations []Migration) (version int, err error)`, which runs each migration's `Up` in order, pushes its `Down` onto a stack and advances `version` on success, and on failure (or panic) runs the down-stack in reverse via `errors.Join`, resetting `version` to 0.
- Test: migration 3 of 4 fails and migrations 2 then 1 roll back in reverse while migration 4 never applies; all four succeed and no down-operation runs; a down-operation that itself errors is surfaced via `errors.Join` while the remaining downs still run; a panic mid-migration rolls back what had applied and then re-panics.
- Verify: `go test -count=1 ./...`

Set up the module:

```bash
go mod edit -go=1.24
```

### A stack, because the migration count is not fixed at compile time

A migration runner cannot write one `defer downN()` per migration, because
it does not know `N` until it reads the list of pending migrations at
runtime — that list might have three entries today and eight after the next
release. The down-stack is what a fixed sequence of defers would be if you
could grow it dynamically: `Apply` pushes `m.Down` onto `downs` only after
`m.Up` has actually succeeded, so the stack always mirrors exactly the
migrations that have taken effect so far. `runDowns` then walks that slice
back to front — the newest migration undone first, which is also the only
order that makes sense, since migration 3 might depend on schema changes
migration 2 made, and undoing 2 before 3 would leave 3's down-operation
looking at a schema it does not expect.

The deferred closure at the top of `Apply` is what actually triggers the
unwind: it inspects the function's own named return values, `version` and
`err`, after everything else has run (or after a panic has started
unwinding), and only calls `runDowns` if something went wrong. On full
success that closure runs too — defers always run — but finds `err` is
`nil` and does nothing, leaving every migration in place.

Create `migrate/migrate.go`:

```go
package migrate

import (
	"errors"
	"fmt"
)

// Migration is one schema step: Up applies it, Down reverses it.
type Migration struct {
	Name string
	Up   func() error
	Down func() error
}

// Apply runs migrations in order, pushing each successfully-applied
// migration's Down function onto a LIFO stack as it goes. If any Up fails
// (or panics), the stack is unwound in reverse -- newest migration first --
// so the schema is left at exactly the version it started from. On full
// success the stack is left unused and version reports how many migrations
// landed.
func Apply(migrations []Migration) (version int, err error) {
	var downs []func() error

	defer func() {
		if p := recover(); p != nil {
			// The function is about to panic out, so its own return values
			// are moot -- still, best-effort rollback so the schema is not
			// left half-migrated, and then let the panic continue.
			_ = runDowns(downs)
			version = 0
			panic(p)
		}
		if err != nil {
			if dErr := runDowns(downs); dErr != nil {
				err = errors.Join(err, dErr)
			}
			version = 0
		}
	}()

	for i, m := range migrations {
		if uErr := m.Up(); uErr != nil {
			return version, fmt.Errorf("migration %d (%s): up: %w", i+1, m.Name, uErr)
		}
		downs = append(downs, m.Down)
		version = i + 1
	}
	return version, nil
}

// runDowns executes every down migration in reverse (LIFO) order, joining
// any errors instead of stopping at the first one -- an interrupted rollback
// would leave the schema in a worse, partially-undone state.
func runDowns(downs []func() error) error {
	var errs []error
	for i := len(downs) - 1; i >= 0; i-- {
		if downs[i] == nil {
			continue
		}
		if err := downs[i](); err != nil {
			errs = append(errs, err)
		}
	}
	return errors.Join(errs...)
}
```

### The runnable demo

Create `cmd/demo/main.go`:

```go
package main

import (
	"fmt"

	"example.com/migrate/migrate"
)

func main() {
	var trace []string

	step := func(name string, fail bool) migrate.Migration {
		return migrate.Migration{
			Name: name,
			Up: func() error {
				if fail {
					trace = append(trace, "up-"+name+"-FAIL")
					return fmt.Errorf("cannot apply %s", name)
				}
				trace = append(trace, "up-"+name)
				return nil
			},
			Down: func() error {
				trace = append(trace, "down-"+name)
				return nil
			},
		}
	}

	version, err := migrate.Apply([]migrate.Migration{
		step("create-users-table", false),
		step("add-users-email-index", false),
		step("add-orders-fk", true),
	})

	fmt.Println("version:", version)
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
version: 0
err: migration 3 (add-orders-fk): up: cannot apply add-orders-fk
up-create-users-table
up-add-users-email-index
up-add-orders-fk-FAIL
down-add-users-email-index
down-create-users-table
```

### Tests

Create `migrate/migrate_test.go`:

```go
package migrate

import (
	"errors"
	"slices"
	"testing"
)

func rec(trace *[]string, name string) Migration {
	return Migration{
		Name: name,
		Up: func() error {
			*trace = append(*trace, "up-"+name)
			return nil
		},
		Down: func() error {
			*trace = append(*trace, "down-"+name)
			return nil
		},
	}
}

func TestApplyMidFailureRollsBackAppliedInReverse(t *testing.T) {
	t.Parallel()

	var trace []string
	stepErr := errors.New("fk violation")
	migrations := []Migration{
		rec(&trace, "one"),
		rec(&trace, "two"),
		{
			Name: "three",
			Up:   func() error { trace = append(trace, "up-three-FAIL"); return stepErr },
			Down: func() error { trace = append(trace, "down-three"); return nil },
		},
	}

	version, err := Apply(migrations)

	if !errors.Is(err, stepErr) {
		t.Fatalf("err = %v, want errors.Is %v", err, stepErr)
	}
	if version != 0 {
		t.Fatalf("version = %d, want 0 after rollback", version)
	}
	want := []string{"up-one", "up-two", "up-three-FAIL", "down-two", "down-one"}
	if !slices.Equal(trace, want) {
		t.Fatalf("trace = %v, want %v", trace, want)
	}
}

func TestApplyAllSucceedKeepsVersionNoRollback(t *testing.T) {
	t.Parallel()

	var trace []string
	migrations := []Migration{rec(&trace, "one"), rec(&trace, "two"), rec(&trace, "three")}

	version, err := Apply(migrations)

	if err != nil {
		t.Fatalf("err = %v, want nil", err)
	}
	if version != 3 {
		t.Fatalf("version = %d, want 3", version)
	}
	want := []string{"up-one", "up-two", "up-three"}
	if !slices.Equal(trace, want) {
		t.Fatalf("trace = %v, want %v (no downs)", trace, want)
	}
}

func TestApplySurfacesDownError(t *testing.T) {
	t.Parallel()

	upErr := errors.New("boom")
	downErr := errors.New("down failed too")
	var trace []string

	migrations := []Migration{
		{
			Name: "one",
			Up:   func() error { trace = append(trace, "up-one"); return nil },
			Down: func() error { trace = append(trace, "down-one-ERR"); return downErr },
		},
		{
			Name: "two",
			Up:   func() error { trace = append(trace, "up-two-FAIL"); return upErr },
		},
	}

	_, err := Apply(migrations)

	if !errors.Is(err, upErr) {
		t.Errorf("err = %v, want errors.Is %v (up)", err, upErr)
	}
	if !errors.Is(err, downErr) {
		t.Errorf("err = %v, want errors.Is %v (down)", err, downErr)
	}
}

func TestApplyPanicRollsBackThenRePanics(t *testing.T) {
	t.Parallel()

	var trace []string
	migrations := []Migration{
		rec(&trace, "one"),
		{
			Name: "two",
			Up:   func() error { panic("kaboom") },
		},
	}

	defer func() {
		r := recover()
		if r == nil {
			t.Fatal("expected panic to propagate")
		}
		want := []string{"up-one", "down-one"}
		if !slices.Equal(trace, want) {
			t.Fatalf("trace = %v, want %v", trace, want)
		}
	}()

	_, _ = Apply(migrations)
}
```

## Review

The runner is correct when a mid-sequence failure rolls back exactly the
migrations that had already applied, in reverse, and no further; when full
success leaves `version` matching the migration count and runs no downs;
when a down-operation that itself errors is surfaced (via `errors.Join`)
without stopping the remaining downs from running; and when a panic
mid-migration triggers the same rollback before the panic continues. The
mistake this pattern exists to prevent is tracking migrations with a plain
counter and rolling back "the last N migrations" by re-deriving N from
some other source of truth after the fact — the down-stack instead carries
the exact closures needed, built up as a side effect of what actually
succeeded, so there is nothing to get out of sync.

## Resources

- [errors.Join](https://pkg.go.dev/errors#Join)
- [The Go Programming Language Specification: Defer statements](https://go.dev/ref/spec#Defer_statements)
- [golang-migrate/migrate](https://github.com/golang-migrate/migrate) — a real Go migration tool with the same up/down pairing.

---

Back to [00-concepts.md](00-concepts.md) | Prev: [26-observable-stat-snapshot-defer.md](26-observable-stat-snapshot-defer.md) | Next: [28-rate-limiter-quota-release.md](28-rate-limiter-quota-release.md)
