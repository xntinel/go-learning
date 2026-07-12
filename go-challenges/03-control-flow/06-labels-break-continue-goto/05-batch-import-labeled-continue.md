# Exercise 5: Batch ETL that skips a poison batch with labeled continue

A bulk importer reads pages of records and writes them downstream. When a single
record in a page is structurally poisoned, the whole page is unsafe to commit —
you want all-or-nothing per batch. That is a labeled `continue`: abandon the
current inner record loop and jump to the *next batch*, recording the failure.
This exercise contrasts labeled `continue` (skip the batch) with bare `continue`
(skip one record) and labeled `break` (stop everything).

This module is fully self-contained: its own `go mod init`, all code inline, its
own demo and tests. Nothing here imports any other exercise.

## What you'll build

```text
batchimport/               independent module: example.com/batchimport
  go.mod                   go 1.24
  batchimport.go           Record, Batch; Import commits whole batches, skips poison ones
  cmd/
    demo/
      main.go              runnable demo: 3 batches, middle one poisoned
  batchimport_test.go      poison batch skipped whole; good batches committed; errors joined
```

- Files: `batchimport.go`, `cmd/demo/main.go`, `batchimport_test.go`.
- Implement: `Import(batches) ([]string, error)` with an outer loop over batches and an inner loop over records; a poisoned record triggers `continue nextBatch`, abandoning the current batch (committing none of it) and recording an error naming the batch.
- Test: with batch index 1 poisoned, every good record from batches 0 and 2 is imported, no record from batch 1 is imported, and the joined error names batch 1 via `errors.Is`.
- Verify: `go test -count=1 -race ./...`

Set up the module:

```bash
go mod edit -go=1.24
```

### Why labeled continue, not bare continue

The semantics are all-or-nothing per batch: a batch commits every one of its
records or none of them. The inner loop stages a batch's record IDs into a
temporary slice; only after the inner loop completes without hitting a poison
record does the staged slice get appended to the committed output. When a poison
record is found, `continue nextBatch` jumps straight to the next iteration of the
outer loop, skipping the commit — so nothing from the poisoned batch reaches the
output.

Contrast the three control-flow choices, because choosing wrong changes the data
that lands downstream:

- Bare `continue` inside the inner loop would skip only the poison record and keep
  importing the rest of that batch — a *partial* batch commit, the exact thing
  all-or-nothing semantics forbid.
- Labeled `break nextBatch` (were the label on a `break`) would stop the entire
  import at the first poison record, abandoning the good batches that follow.
- Labeled `continue nextBatch` skips the current batch and proceeds with the next
  — the correct behavior.

Because `continue` only targets a `for`, the label `nextBatch` sits on the outer
batch loop, and `continue nextBatch` continues *that* loop. The errors are
collected and returned with `errors.Join`, so a caller can `errors.Is` them
against the sentinel and see exactly which batches were rejected.

Create `batchimport.go`:

```go
package batchimport

import (
	"errors"
	"fmt"
)

// Record is one row in a batch. Valid is false for a structurally poisoned row.
type Record struct {
	ID    string
	Valid bool
}

// Batch is a page of records with a stable index for error reporting.
type Batch struct {
	Index   int
	Records []Record
}

// ErrPoisonBatch is the sentinel wrapped for every batch abandoned because it
// contained a poisoned record.
var ErrPoisonBatch = errors.New("poison batch")

// Import commits every record of every batch, all-or-nothing per batch. A batch
// containing any invalid record is abandoned entirely (none of its records are
// imported) and an error naming it is recorded; import continues with the next
// batch. It returns the imported record IDs in order and the joined batch errors.
func Import(batches []Batch) ([]string, error) {
	var imported []string
	var errs []error

nextBatch:
	for _, b := range batches {
		staged := make([]string, 0, len(b.Records))
		for _, r := range b.Records {
			if !r.Valid {
				errs = append(errs, fmt.Errorf("batch %d: %w", b.Index, ErrPoisonBatch))
				continue nextBatch // abandon this batch; commit nothing from it
			}
			staged = append(staged, r.ID)
		}
		imported = append(imported, staged...) // commit only a clean batch
	}

	return imported, errors.Join(errs...)
}
```

### The runnable demo

Create `cmd/demo/main.go`:

```go
package main

import (
	"fmt"

	"example.com/batchimport"
)

func main() {
	good := func(id string) batchimport.Record { return batchimport.Record{ID: id, Valid: true} }
	poison := func(id string) batchimport.Record { return batchimport.Record{ID: id, Valid: false} }

	batches := []batchimport.Batch{
		{Index: 0, Records: []batchimport.Record{good("a0"), good("a1")}},
		{Index: 1, Records: []batchimport.Record{good("b0"), poison("b1"), good("b2")}},
		{Index: 2, Records: []batchimport.Record{good("c0"), good("c1")}},
	}

	imported, err := batchimport.Import(batches)
	fmt.Println("imported:", imported)
	fmt.Println("error:", err)
}
```

Run it:

```bash
go run ./cmd/demo
```

Expected output:

```
imported: [a0 a1 c0 c1]
error: batch 1: poison batch
```

Note that `b0`, the good record *before* the poison in batch 1, is not imported —
the whole batch was abandoned, not just the poison record.

### Tests

`TestSkipsWholePoisonBatch` is the core assertion: batch 1 is poisoned, and the
imported set is exactly the records of batches 0 and 2 — proving `b0` (the good
record before the poison) was *not* partially committed, which a bare `continue`
would have done. `TestAllValid` confirms a clean run returns a nil error.
`TestMultiplePoisonBatchesJoined` proves two poisoned batches produce two joined
errors, both matchable with `errors.Is`.

Create `batchimport_test.go`:

```go
package batchimport

import (
	"errors"
	"slices"
	"strings"
	"testing"
)

func good(id string) Record   { return Record{ID: id, Valid: true} }
func poison(id string) Record { return Record{ID: id, Valid: false} }

func unwrapJoined(err error) []error {
	if err == nil {
		return nil
	}
	if u, ok := err.(interface{ Unwrap() []error }); ok {
		return u.Unwrap()
	}
	return []error{err}
}

func TestSkipsWholePoisonBatch(t *testing.T) {
	t.Parallel()

	batches := []Batch{
		{Index: 0, Records: []Record{good("a0"), good("a1")}},
		{Index: 1, Records: []Record{good("b0"), poison("b1"), good("b2")}},
		{Index: 2, Records: []Record{good("c0"), good("c1")}},
	}

	imported, err := Import(batches)

	want := []string{"a0", "a1", "c0", "c1"}
	if !slices.Equal(imported, want) {
		t.Fatalf("imported = %v, want %v (no record from batch 1, including b0)", imported, want)
	}
	if !errors.Is(err, ErrPoisonBatch) {
		t.Fatalf("err = %v, want it to wrap ErrPoisonBatch", err)
	}
	if !strings.Contains(err.Error(), "batch 1") {
		t.Fatalf("err = %q, want it to name batch 1", err.Error())
	}
}

func TestAllValid(t *testing.T) {
	t.Parallel()

	batches := []Batch{
		{Index: 0, Records: []Record{good("a0")}},
		{Index: 1, Records: []Record{good("b0"), good("b1")}},
	}

	imported, err := Import(batches)
	if err != nil {
		t.Fatalf("err = %v, want nil", err)
	}
	want := []string{"a0", "b0", "b1"}
	if !slices.Equal(imported, want) {
		t.Fatalf("imported = %v, want %v", imported, want)
	}
}

func TestMultiplePoisonBatchesJoined(t *testing.T) {
	t.Parallel()

	batches := []Batch{
		{Index: 0, Records: []Record{poison("a0")}},
		{Index: 1, Records: []Record{good("b0")}},
		{Index: 2, Records: []Record{poison("c0")}},
	}

	imported, err := Import(batches)
	if !slices.Equal(imported, []string{"b0"}) {
		t.Fatalf("imported = %v, want [b0]", imported)
	}
	errs := unwrapJoined(err)
	if len(errs) != 2 {
		t.Fatalf("joined %d errors, want 2", len(errs))
	}
	for _, e := range errs {
		if !errors.Is(e, ErrPoisonBatch) {
			t.Fatalf("joined error %v does not wrap ErrPoisonBatch", e)
		}
	}
}
```

## Review

The importer is correct when a poisoned batch contributes zero records to the
output and every clean batch contributes all of its records, with each rejection
recorded as a joined, `errors.Is`-matchable error. The bug this exercise guards
against is a bare `continue` that skips only the poison record, leaking a partial
batch (`b0` without `b2`) downstream — `TestSkipsWholePoisonBatch` fails against
that version because it would see `b0` in the imported set. Staging into a
temporary slice and committing only after a clean inner loop is what makes the
labeled `continue` all-or-nothing. `errors.Join` with a wrapped sentinel keeps the
error set both machine-checkable and human-readable.

## Resources

- [Go Specification: Continue statements](https://go.dev/ref/spec#Continue_statements) — a labeled `continue` continues the named `for`.
- [errors.Join](https://pkg.go.dev/errors#Join) — collecting multiple failures into one error.
- [errors.Is](https://pkg.go.dev/errors#Is) — matching a wrapped sentinel through a join.

---

Back to [00-concepts.md](00-concepts.md) | Prev: [04-worker-fanin-graceful-drain.md](04-worker-fanin-graceful-drain.md) | Next: [06-multi-resource-acquire-goto-cleanup.md](06-multi-resource-acquire-goto-cleanup.md)
