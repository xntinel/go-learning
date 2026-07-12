# Exercise 6: Bulk Operation With Per-Item Context And Partial Success

A worker that writes 1000 records and hits two bad rows should commit the 998 good
ones and report exactly which two failed — not throw away the whole batch on the
first error, and not collapse the result to a bare error that hides how much
succeeded. This exercise builds `ProcessBatch`, whose honest return shape is
`(succeeded int, err error)` where `err` is the `errors.Join` of the failures.

This module is fully self-contained: its own `go mod init`, all code inline, its
own demo and tests.

## What you'll build

```text
batch/                     independent module: example.com/batch
  go.mod                   go 1.26
  batch.go                 Record; ProcessBatch(records, op) (int, error)
  cmd/
    demo/
      main.go              processes a mixed batch, prints count and failures
  batch_test.go            mixed, all-ok, all-fail; exact succeeded count
```

- Files: `batch.go`, `cmd/demo/main.go`, `batch_test.go`.
- Implement: `ProcessBatch(records []Record, op func(Record) error) (succeeded int, err error)` that applies `op` to each record, wraps each failure with `fmt.Errorf("record %d (%s): %w", i, id, err)`, counts successes, and returns the count plus `errors.Join` of failures.
- Test: mixed batch — exact `succeeded` count, aggregate `errors.Is` each failed cause, message contains each failing index; all-success — `succeeded == len`, `err == nil`; all-fail — `succeeded == 0`.
- Verify: `go test -count=1 -race ./...`

Set up the module:

```bash
mkdir -p go-solutions/10-error-handling/07-multiple-error-returns/06-batch-partial-success/cmd/demo
cd go-solutions/10-error-handling/07-multiple-error-returns/06-batch-partial-success
```

### Why return both a count and an aggregate

A bulk operation has three possible outcomes the caller must distinguish: all
succeeded, all failed, or partial. A single `error` return cannot express partial
success — a non-nil error tells the caller nothing about how many rows landed, and
a nil error is only honest if *nothing* failed. The truthful signature carries both
halves: `succeeded int` says how much work committed, and `err error` (an
`errors.Join` of the failures) says exactly what did not. The caller commits the
`succeeded` rows, then logs or retries the failures the aggregate names.

Per-item context is what makes the aggregate actionable. `fmt.Errorf("record %d (%s): %w", i, r.ID, err)`
stamps the index and the record's ID onto each failure while `%w` keeps the
underlying cause reachable by `errors.Is`. So the aggregate is both a readable list
("record 3 (sku-42): out of stock") and a matchable tree (`errors.Is(agg, ErrOutOfStock)`
still works). This is wrap-then-join applied to a loop over records.

Create `batch.go`:

```go
package batch

import (
	"errors"
	"fmt"
)

// Record is one item in a bulk operation, identified for error reporting.
type Record struct {
	ID   string
	Data int
}

// ProcessBatch applies op to every record, continuing past failures. It returns
// the number that succeeded and errors.Join of the failures (nil when none fail).
// Each failure is wrapped with its index and record ID so the aggregate names
// exactly what did not commit.
func ProcessBatch(records []Record, op func(Record) error) (succeeded int, err error) {
	var errs []error
	for i, r := range records {
		if e := op(r); e != nil {
			errs = append(errs, fmt.Errorf("record %d (%s): %w", i, r.ID, e))
			continue
		}
		succeeded++
	}
	return succeeded, errors.Join(errs...)
}
```

### The runnable demo

The demo processes five records where the operation rejects negative data,
simulating two bad rows in a batch, and prints the partial-success result.

Create `cmd/demo/main.go`:

```go
package main

import (
	"errors"
	"fmt"

	"example.com/batch"
)

var errNegative = errors.New("data must be non-negative")

func main() {
	records := []batch.Record{
		{ID: "sku-1", Data: 10},
		{ID: "sku-2", Data: -3},
		{ID: "sku-3", Data: 7},
		{ID: "sku-4", Data: -1},
		{ID: "sku-5", Data: 2},
	}

	op := func(r batch.Record) error {
		if r.Data < 0 {
			return errNegative
		}
		return nil
	}

	succeeded, err := batch.ProcessBatch(records, op)
	fmt.Printf("committed %d of %d records\n", succeeded, len(records))
	if err != nil {
		fmt.Println("failures:")
		fmt.Println(err)
	}
}
```

Run it:

```bash
go run ./cmd/demo
```

Expected output:

```
committed 3 of 5 records
failures:
record 1 (sku-2): data must be non-negative
record 3 (sku-4): data must be non-negative
```

### Tests

`TestProcessBatchMixed` asserts the exact `succeeded` count, that the aggregate is
`errors.Is` the failing cause, and that the message contains each failing index.
`TestProcessBatchAllSuccess` asserts `succeeded == len` and `err == nil` — the
partial-success shape must return a clean nil when nothing fails.
`TestProcessBatchAllFail` asserts `succeeded == 0`. The exact-count assertions are
the point: they fail if the loop short-circuits or miscounts.

Create `batch_test.go`:

```go
package batch

import (
	"errors"
	"strings"
	"testing"
)

var errBad = errors.New("bad record")

func recs(n int) []Record {
	out := make([]Record, n)
	for i := range n {
		out[i] = Record{ID: "id-" + string(rune('a'+i)), Data: i}
	}
	return out
}

func TestProcessBatchMixed(t *testing.T) {
	t.Parallel()

	records := recs(5)
	// Fail records at index 1 and 3.
	op := func(r Record) error {
		if r.Data == 1 || r.Data == 3 {
			return errBad
		}
		return nil
	}

	succeeded, err := ProcessBatch(records, op)
	if succeeded != 3 {
		t.Errorf("succeeded = %d, want 3", succeeded)
	}
	if !errors.Is(err, errBad) {
		t.Error("aggregate missing errBad")
	}
	msg := err.Error()
	for _, idx := range []string{"record 1", "record 3"} {
		if !strings.Contains(msg, idx) {
			t.Errorf("message %q missing %q", msg, idx)
		}
	}
}

func TestProcessBatchAllSuccess(t *testing.T) {
	t.Parallel()

	records := recs(4)
	succeeded, err := ProcessBatch(records, func(Record) error { return nil })
	if succeeded != len(records) {
		t.Errorf("succeeded = %d, want %d", succeeded, len(records))
	}
	if err != nil {
		t.Errorf("err = %v, want nil", err)
	}
}

func TestProcessBatchAllFail(t *testing.T) {
	t.Parallel()

	records := recs(3)
	succeeded, err := ProcessBatch(records, func(Record) error { return errBad })
	if succeeded != 0 {
		t.Errorf("succeeded = %d, want 0", succeeded)
	}
	if !errors.Is(err, errBad) {
		t.Error("aggregate missing errBad")
	}
}
```

## Review

`ProcessBatch` is correct when the returned count is exactly the number of records
whose `op` succeeded, the aggregate names each failing index and is `errors.Is` the
cause, and an all-success batch returns `(len, nil)`. The count is the load-bearing
value — a caller commits precisely those rows, so an off-by-one hides or duplicates
work. Do not stop the loop on the first failure (the `continue` keeps it going); do
not return only the error (that discards the success count). Run with `-race`;
`ProcessBatch` is sequential, but the same shape scales to a concurrent worker where
the accumulation must be guarded (Exercise 9).

## Resources

- [errors.Join](https://pkg.go.dev/errors#Join) — aggregating the per-record failures.
- [fmt.Errorf and %w](https://pkg.go.dev/fmt#Errorf) — wrapping each failure with its index and ID.

---

Back to [00-concepts.md](00-concepts.md) | Prev: [05-close-all-resources.md](05-close-all-resources.md) | Next: [07-custom-multierror-unwrap-slice.md](07-custom-multierror-unwrap-slice.md)
