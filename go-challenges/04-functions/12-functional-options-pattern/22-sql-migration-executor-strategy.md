# Exercise 22: Database Migration Executor With Rollback Strategy and Dry-Run

**Nivel: Avanzado** — validacion normal (tabla de casos, incluye borde o concurrencia).

A migration executor runs schema changes in order and, if one fails
partway through, has to decide what to do about the ones that already
succeeded. This module builds that executor through options, and it checks
the one precondition that only matters for one specific rollback strategy:
reverting a migration by running its down script requires that a down
script actually exists.

## What you'll build

```text
migrator/                        independent module: example.com/migrator
  go.mod                         go 1.24
  migrator.go                    RollbackMode, Migration, Executor, Option, New,
                                  WithRollbackMode, WithValidationTimeout,
                                  WithDryRun, WithMigrations, Execute
  cmd/
    demo/
      main.go                    a failing third migration triggers reverse-order rollback
  migrator_test.go                table test over options plus rollback-mode behavior and context handling
```

- Files: `migrator.go`, `cmd/demo/main.go`, `migrator_test.go`.
- Implement: `New(opts ...Option) (*Executor, error)` whose `Execute` runs migrations under a per-step timeout and rolls back completed ones in reverse order according to `CopyBackup`/`LogOnly`/`Revert`, validating that `Revert` only applies when every migration carries a down script.
- Test: every option-validation case, all three rollback modes' distinct behavior on a mid-run failure, the all-succeed path, and an already-canceled context.
- Verify: `go test -count=1 -race ./...`

Set up the module:

```bash
mkdir -p go-solutions/04-functions/12-functional-options-pattern/22-sql-migration-executor-strategy/cmd/demo
cd go-solutions/04-functions/12-functional-options-pattern/22-sql-migration-executor-strategy
go mod edit -go=1.24
```

### Why `Revert` needs a check `WithMigrations` cannot make

`WithRollbackMode` and `WithMigrations` are independent options — a caller
can register migrations before or after choosing a rollback mode, in either
order, any number of times. `WithMigrations` validates each migration it is
given (non-empty name, non-nil `Up`), but it has no way to know what
rollback mode the caller will eventually choose. Only after every option has
run does `New` know both the full migration list and the rollback mode, and
only then can it check that `Revert` — the one mode that actually calls
`Down` — has a down script for every migration it might need to undo.
`CopyBackup` and `LogOnly` never call `Down`, so they impose no such
requirement.

### Rolling back in reverse, regardless of mode

`Execute` runs migrations in order; the moment one fails, it rolls back
every migration that already succeeded, in reverse order, through
`rollBack`. What "roll back" means depends entirely on the configured mode:
`Revert` calls each migration's `Down`, `CopyBackup` conceptually restores
from a pre-migration snapshot (no down script needed), and `LogOnly` just
records which migrations now need a human to intervene. The demo and the
table test both prove all three modes produce a distinct trace — `Revert`
is the only one that ever calls `Down`.

### A per-migration timeout without a real timer

`Execute` wraps each migration's `Up` in `context.WithTimeout(ctx, validationTimeout)`.
Testing an actual timeout deterministically doesn't require waiting for one:
`TestExecuteRespectsAlreadyCanceledContext` passes an already-canceled
parent context, so the per-migration child context is expired from the
moment it is created — no `time.Sleep`, no flakiness, and the assertion is
exact.

Create `migrator.go`:

```go
package migrator

import (
	"context"
	"fmt"
	"time"
)

// RollbackMode selects what happens to already-applied migrations when a
// later one fails.
type RollbackMode int

const (
	// CopyBackup restores each applied migration from a pre-migration
	// backup rather than running its down script.
	CopyBackup RollbackMode = iota
	// LogOnly records which migrations need manual rollback but changes
	// nothing automatically.
	LogOnly
	// Revert runs each applied migration's down script, in reverse order.
	Revert
)

func (m RollbackMode) String() string {
	switch m {
	case CopyBackup:
		return "CopyBackup"
	case LogOnly:
		return "LogOnly"
	case Revert:
		return "Revert"
	default:
		return fmt.Sprintf("RollbackMode(%d)", int(m))
	}
}

// Migration is one schema change. Down is optional unless RollbackMode is
// Revert.
type Migration struct {
	Name string
	Up   func(ctx context.Context) error
	Down func(ctx context.Context) error
}

// Executor applies migrations in order, validating each against a per-step
// timeout, and rolls back according to the configured RollbackMode if one
// fails partway through.
type Executor struct {
	rollbackMode      RollbackMode
	validationTimeout time.Duration
	dryRun            bool
	migrations        []Migration
}

// Option configures an Executor and may reject invalid input.
type Option func(*Executor) error

// New seeds defaults, applies opts in order, then validates the invariant
// no single option could see: under Revert, every registered migration must
// carry a down script, because Revert has no other way to undo it.
func New(opts ...Option) (*Executor, error) {
	e := &Executor{
		rollbackMode:      LogOnly,
		validationTimeout: 30 * time.Second,
	}
	for _, opt := range opts {
		if err := opt(e); err != nil {
			return nil, err
		}
	}

	if len(e.migrations) == 0 {
		return nil, fmt.Errorf("at least one migration must be registered via WithMigrations")
	}
	if e.rollbackMode == Revert {
		for _, m := range e.migrations {
			if m.Down == nil {
				return nil, fmt.Errorf("migration %q has no down script, required when rollback mode is Revert", m.Name)
			}
		}
	}
	return e, nil
}

// WithRollbackMode selects the rollback strategy from the closed set of
// named constants.
func WithRollbackMode(mode RollbackMode) Option {
	return func(e *Executor) error {
		switch mode {
		case CopyBackup, LogOnly, Revert:
			e.rollbackMode = mode
			return nil
		default:
			return fmt.Errorf("unknown rollback mode: %d", int(mode))
		}
	}
}

// WithValidationTimeout bounds how long each migration's Up may run (> 0).
func WithValidationTimeout(d time.Duration) Option {
	return func(e *Executor) error {
		if d <= 0 {
			return fmt.Errorf("validation timeout must be positive, got %s", d)
		}
		e.validationTimeout = d
		return nil
	}
}

// WithDryRun makes Execute validate context deadlines without calling Up.
func WithDryRun(dryRun bool) Option {
	return func(e *Executor) error {
		e.dryRun = dryRun
		return nil
	}
}

// WithMigrations appends migrations, each requiring a non-empty name and a
// non-nil Up.
func WithMigrations(migrations ...Migration) Option {
	return func(e *Executor) error {
		for _, m := range migrations {
			if m.Name == "" {
				return fmt.Errorf("migration name must not be empty")
			}
			if m.Up == nil {
				return fmt.Errorf("migration %q has a nil Up", m.Name)
			}
		}
		e.migrations = append(e.migrations, migrations...)
		return nil
	}
}

// Execute runs migrations in order under ctx, each bounded by the
// validation timeout. On failure it rolls back every already-applied
// migration according to the configured RollbackMode and returns the
// applied and rolled-back names alongside the error.
func (e *Executor) Execute(ctx context.Context) (applied, rolledBack []string, err error) {
	for _, m := range e.migrations {
		mctx, cancel := context.WithTimeout(ctx, e.validationTimeout)
		var upErr error
		if e.dryRun {
			upErr = mctx.Err()
		} else {
			upErr = m.Up(mctx)
		}
		cancel()

		if upErr != nil {
			err = fmt.Errorf("migration %q failed: %w", m.Name, upErr)
			rolledBack = e.rollBack(ctx, applied)
			return applied, rolledBack, err
		}
		applied = append(applied, m.Name)
	}
	return applied, nil, nil
}

func (e *Executor) rollBack(ctx context.Context, applied []string) []string {
	byName := make(map[string]Migration, len(e.migrations))
	for _, m := range e.migrations {
		byName[m.Name] = m
	}

	var rolledBack []string
	for i := len(applied) - 1; i >= 0; i-- {
		name := applied[i]
		switch e.rollbackMode {
		case Revert:
			_ = byName[name].Down(ctx)
			rolledBack = append(rolledBack, name+":reverted")
		case CopyBackup:
			rolledBack = append(rolledBack, name+":restored-from-backup")
		default: // LogOnly
			rolledBack = append(rolledBack, name+":logged-for-manual-rollback")
		}
	}
	return rolledBack
}
```

### The runnable demo

The demo registers three migrations where the third fails, under
`RollbackMode.Revert`, and prints the call log proving `Down` ran for the
two completed migrations in reverse order. It also shows `New` rejecting
`Revert` when a migration has no down script.

Create `cmd/demo/main.go`:

```go
package main

import (
	"context"
	"errors"
	"fmt"

	"example.com/migrator"
)

func main() {
	var log []string

	migrations := []migrator.Migration{
		{
			Name: "001_create_users",
			Up:   func(ctx context.Context) error { log = append(log, "up:001"); return nil },
			Down: func(ctx context.Context) error { log = append(log, "down:001"); return nil },
		},
		{
			Name: "002_add_index",
			Up:   func(ctx context.Context) error { log = append(log, "up:002"); return nil },
			Down: func(ctx context.Context) error { log = append(log, "down:002"); return nil },
		},
		{
			Name: "003_bad_migration",
			Up:   func(ctx context.Context) error { return errors.New("syntax error in migration") },
			Down: func(ctx context.Context) error { log = append(log, "down:003"); return nil },
		},
	}

	e, err := migrator.New(
		migrator.WithMigrations(migrations...),
		migrator.WithRollbackMode(migrator.Revert),
	)
	if err != nil {
		panic(err)
	}

	applied, rolledBack, err := e.Execute(context.Background())
	fmt.Printf("applied: %v\n", applied)
	fmt.Printf("rolled back: %v\n", rolledBack)
	fmt.Printf("error: %v\n", err)
	fmt.Printf("call log: %v\n", log)

	_, err = migrator.New(
		migrator.WithMigrations(migrator.Migration{Name: "no-down", Up: func(context.Context) error { return nil }}),
		migrator.WithRollbackMode(migrator.Revert),
	)
	fmt.Printf("revert without down script: %v\n", err)
}
```

Run it:

```bash
go run ./cmd/demo
```

Expected output:

```
applied: [001_create_users 002_add_index]
rolled back: [002_add_index:reverted 001_create_users:reverted]
error: migration "003_bad_migration" failed: syntax error in migration
call log: [up:001 up:002 down:002 down:001]
revert without down script: migration "no-down" has no down script, required when rollback mode is Revert
```

### Tests

`TestNewValidation` tables no-migrations, `Revert` with and without down
scripts, `LogOnly` needing none, unknown rollback modes, and the remaining
per-option checks. `TestExecuteRollsBackOnFailure` runs the same
three-migration failure scenario under all three modes and asserts each
produces its own distinct rollback trace, with `Down` called only under
`Revert`. `TestExecuteAllSucceed` proves a clean run reports no rollback.
`TestExecuteRespectsAlreadyCanceledContext` proves the timeout wrapping
surfaces an already-canceled parent context immediately.

Create `migrator_test.go`:

```go
package migrator

import (
	"context"
	"errors"
	"testing"
)

func okMigration(name string) Migration {
	return Migration{
		Name: name,
		Up:   func(context.Context) error { return nil },
		Down: func(context.Context) error { return nil },
	}
}

func TestNewValidation(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name    string
		opts    []Option
		wantErr bool
	}{
		{name: "no migrations registered", wantErr: true},
		{name: "one migration, defaults", opts: []Option{WithMigrations(okMigration("001"))}},
		{name: "Revert with down scripts present", opts: []Option{
			WithMigrations(okMigration("001"), okMigration("002")), WithRollbackMode(Revert),
		}},
		{name: "Revert with a missing down script", opts: []Option{
			WithMigrations(Migration{Name: "001", Up: func(context.Context) error { return nil }}),
			WithRollbackMode(Revert),
		}, wantErr: true},
		{name: "LogOnly does not require down scripts", opts: []Option{
			WithMigrations(Migration{Name: "001", Up: func(context.Context) error { return nil }}),
			WithRollbackMode(LogOnly),
		}},
		{name: "unknown rollback mode", opts: []Option{
			WithMigrations(okMigration("001")), WithRollbackMode(RollbackMode(99)),
		}, wantErr: true},
		{name: "invalid validation timeout", opts: []Option{
			WithMigrations(okMigration("001")), WithValidationTimeout(0),
		}, wantErr: true},
		{name: "empty migration name rejected", opts: []Option{
			WithMigrations(Migration{Name: "", Up: func(context.Context) error { return nil }}),
		}, wantErr: true},
		{name: "nil Up rejected", opts: []Option{
			WithMigrations(Migration{Name: "001"}),
		}, wantErr: true},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			_, err := New(tt.opts...)
			if tt.wantErr && err == nil {
				t.Fatal("expected error, got nil")
			}
			if !tt.wantErr && err != nil {
				t.Fatalf("unexpected error: %v", err)
			}
		})
	}
}

func TestExecuteRollsBackOnFailure(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name           string
		mode           RollbackMode
		wantRolledBack []string
	}{
		{name: "Revert calls down scripts in reverse", mode: Revert, wantRolledBack: []string{"002:reverted", "001:reverted"}},
		{name: "CopyBackup restores without calling down", mode: CopyBackup, wantRolledBack: []string{"002:restored-from-backup", "001:restored-from-backup"}},
		{name: "LogOnly only records the names", mode: LogOnly, wantRolledBack: []string{"002:logged-for-manual-rollback", "001:logged-for-manual-rollback"}},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			var downCalls []string
			migrations := []Migration{
				{
					Name: "001",
					Up:   func(context.Context) error { return nil },
					Down: func(context.Context) error { downCalls = append(downCalls, "001"); return nil },
				},
				{
					Name: "002",
					Up:   func(context.Context) error { return nil },
					Down: func(context.Context) error { downCalls = append(downCalls, "002"); return nil },
				},
				{
					Name: "003",
					Up:   func(context.Context) error { return errors.New("boom") },
					Down: func(context.Context) error { downCalls = append(downCalls, "003"); return nil },
				},
			}

			e, err := New(WithMigrations(migrations...), WithRollbackMode(tt.mode))
			if err != nil {
				t.Fatal(err)
			}

			applied, rolledBack, err := e.Execute(context.Background())
			if err == nil {
				t.Fatal("expected an error from the failing migration")
			}
			if len(applied) != 2 || applied[0] != "001" || applied[1] != "002" {
				t.Fatalf("applied = %v, want [001 002]", applied)
			}
			if len(rolledBack) != len(tt.wantRolledBack) {
				t.Fatalf("rolledBack = %v, want %v", rolledBack, tt.wantRolledBack)
			}
			for i, name := range tt.wantRolledBack {
				if rolledBack[i] != name {
					t.Fatalf("rolledBack[%d] = %s, want %s (full: %v)", i, rolledBack[i], name, rolledBack)
				}
			}
			if tt.mode == Revert {
				if len(downCalls) != 2 || downCalls[0] != "002" || downCalls[1] != "001" {
					t.Fatalf("downCalls = %v, want [002 001]", downCalls)
				}
			} else if len(downCalls) != 0 {
				t.Fatalf("downCalls = %v, want none under %s", downCalls, tt.mode)
			}
		})
	}
}

func TestExecuteAllSucceed(t *testing.T) {
	t.Parallel()

	e, err := New(WithMigrations(okMigration("001"), okMigration("002")))
	if err != nil {
		t.Fatal(err)
	}

	applied, rolledBack, err := e.Execute(context.Background())
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if len(applied) != 2 {
		t.Fatalf("applied = %v, want 2 entries", applied)
	}
	if rolledBack != nil {
		t.Fatalf("rolledBack = %v, want nil", rolledBack)
	}
}

func TestExecuteRespectsAlreadyCanceledContext(t *testing.T) {
	t.Parallel()

	ctx, cancel := context.WithCancel(context.Background())
	cancel()

	e, err := New(WithMigrations(Migration{
		Name: "001",
		Up: func(ctx context.Context) error {
			return ctx.Err()
		},
	}))
	if err != nil {
		t.Fatal(err)
	}

	applied, _, err := e.Execute(ctx)
	if err == nil {
		t.Fatal("expected an error from an already-canceled context")
	}
	if len(applied) != 0 {
		t.Fatalf("applied = %v, want none", applied)
	}
}
```

## Review

The executor is correct when `Revert` can never be selected alongside a
migration missing the down script it would need, and when a mid-run failure
always rolls back exactly the migrations that succeeded, in reverse order,
regardless of which mode is active. The precondition check is the same
"strategy implies a requirement on a different piece of configuration"
pattern seen with the router's dedicated resources and the key rotator's
active version: only the constructor, after every option has run, holds
both the strategy and the data it constrains. Testing the timeout path
through an already-canceled context rather than a real deadline is what
keeps the whole suite fast and exact.

## Resources

- [pkg.go.dev: context.WithTimeout](https://pkg.go.dev/context#WithTimeout)
- [golang-migrate/migrate](https://github.com/golang-migrate/migrate)
- [Flyway: undo migrations](https://documentation.red-gate.com/fd/undo-migrations-184127857.html)

---

Back to [00-concepts.md](00-concepts.md) | Prev: [21-multi-tenant-router-isolation.md](21-multi-tenant-router-isolation.md) | Next: [23-message-dedupe-sliding-window.md](23-message-dedupe-sliding-window.md)
