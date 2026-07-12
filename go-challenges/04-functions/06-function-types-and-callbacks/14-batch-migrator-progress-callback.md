# Exercise 14: Batch Migrator Driven by a Progress-Report Callback

**Nivel: Intermedio** — validacion rapida (un test corto).

A data migration over thousands of rows needs two callbacks with entirely
different jobs: one that actually migrates a single item, and one that
reports how far along the batch is — for a CLI progress bar, a structured log
line, or a metrics gauge. This module keeps them separate and fail-fast.

## What you'll build

```text
migrator/                   independent module: example.com/batch-migrator
  go.mod                     go 1.24
  migrator.go                type MigrateFunc, type ProgressFunc, func RunBatch
  migrator_test.go            table test: full success vs mid-batch failure
```

Files: `migrator.go`, `migrator_test.go`.
Implement: `type MigrateFunc func(item string) error`,
`type ProgressFunc func(done, total int)`, and
`func RunBatch(items []string, batchSize int, migrate MigrateFunc, report ProgressFunc) error`.
Test: a table covering an all-succeed run across an uneven final batch, and a
run that fails partway through a batch, asserting the exact sequence of
`report` calls in both cases.
Verify: `go test -count=1 ./...`

```bash
go mod edit -go=1.24
```

### Why report only fires after a complete batch

`ProgressFunc` is a pure side-effect callback — it has no way to signal
"stop" or "retry," so its contract has to be about *when* it fires, not what
it returns. The design commits to fail-fast: `RunBatch` stops at the first
`migrate` error and returns immediately, and crucially it never calls
`report` for a batch that didn't finish. That means a caller can treat every
`report(done, total)` call as "these `done` items are durably migrated," a
guarantee that would break if progress were reported mid-batch.

Create `migrator.go`:

```go
package migrator

import "fmt"

// MigrateFunc transforms a single item. It is the unit of work the batch
// runner drives; the runner itself knows nothing about what migrating an
// item actually means.
type MigrateFunc func(item string) error

// ProgressFunc is called after every completed batch with the running count
// of items done and the total to process, so a caller can drive a progress
// bar or log a checkpoint without RunBatch knowing anything about UIs.
type ProgressFunc func(done, total int)

// RunBatch drives migrate over items in batches of batchSize, calling report
// once after each fully completed batch. It stops at the first error
// (fail-fast): a batch that errors part-way through does not get a report
// call, so "reported progress" always means "durably completed," never
// "partially attempted."
func RunBatch(items []string, batchSize int, migrate MigrateFunc, report ProgressFunc) error {
	total := len(items)
	done := 0

	for start := 0; start < total; start += batchSize {
		end := start + batchSize
		if end > total {
			end = total
		}

		for _, item := range items[start:end] {
			if err := migrate(item); err != nil {
				return fmt.Errorf("migrating %q (item %d of %d): %w", item, done+1, total, err)
			}
			done++
		}

		report(done, total)
	}

	return nil
}
```

### Tests

Five items with `batchSize` 2 split into batches of 2, 2, 1. The first case
lets everything succeed and checks the exact `(done, total)` sequence the
uneven final batch produces. The second case fails on the third item, inside
the second batch, and asserts only the first batch's report fired — the
second batch's partial work never gets reported.

Create `migrator_test.go`:

```go
package migrator

import (
	"strings"
	"testing"
)

type progress struct {
	done, total int
}

func TestRunBatch(t *testing.T) {
	items := []string{"item1", "item2", "item3", "item4", "item5"}

	tests := []struct {
		name        string
		batchSize   int
		failOn      string
		wantReports []progress
		wantErr     bool
		wantErrPart string
	}{
		{
			name:        "all items succeed across uneven final batch",
			batchSize:   2,
			failOn:      "",
			wantReports: []progress{{2, 5}, {4, 5}, {5, 5}},
			wantErr:     false,
		},
		{
			name:        "stops mid-batch and never reports the incomplete batch",
			batchSize:   2,
			failOn:      "item3",
			wantReports: []progress{{2, 5}},
			wantErr:     true,
			wantErrPart: "item3",
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			var reports []progress

			migrate := func(item string) error {
				if item == tc.failOn {
					return errMigrationFailed
				}
				return nil
			}
			report := func(done, total int) {
				reports = append(reports, progress{done, total})
			}

			err := RunBatch(items, tc.batchSize, migrate, report)

			if tc.wantErr && err == nil {
				t.Fatal("RunBatch() = nil, want an error")
			}
			if !tc.wantErr && err != nil {
				t.Fatalf("RunBatch() = %v, want nil", err)
			}
			if tc.wantErr && !strings.Contains(err.Error(), tc.wantErrPart) {
				t.Errorf("RunBatch() error = %q, want it to mention %q", err.Error(), tc.wantErrPart)
			}

			if len(reports) != len(tc.wantReports) {
				t.Fatalf("reports = %v, want %v", reports, tc.wantReports)
			}
			for i, want := range tc.wantReports {
				if reports[i] != want {
					t.Errorf("reports[%d] = %+v, want %+v", i, reports[i], want)
				}
			}
		})
	}
}

var errMigrationFailed = migrationError("migration failed")

type migrationError string

func (e migrationError) Error() string { return string(e) }
```

Run it: `go test -count=1 ./...`

## Review

Splitting `MigrateFunc` from `ProgressFunc` keeps two unrelated concerns
unrelated: the migration logic never has to know a progress bar exists, and
the reporting logic never touches the actual data. Fail-fast was a deliberate
choice here, matching the retry loop's "permanent error stops immediately"
case rather than the validation pipeline's "collect every violation" case —
a migration usually needs to halt and let an operator inspect the failure,
not silently skip a bad row and keep going.

## Resources

- [fmt.Errorf and %w](https://pkg.go.dev/fmt#Errorf)
- [Effective Go: Function values](https://go.dev/doc/effective_go#functions)

---

Back to [00-concepts.md](00-concepts.md) | Prev: [13-config-tree-visitor-early-stop.md](13-config-tree-visitor-early-stop.md) | Next: [15-reconciliation-differ-callback.md](15-reconciliation-differ-callback.md)
