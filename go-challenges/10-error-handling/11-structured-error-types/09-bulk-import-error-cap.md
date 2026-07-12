# Exercise 9: Bounding Error Output In A Bulk Import

A batch endpoint that imports N rows must not return an unbounded error list or
scan forever on a huge adversarial file. This module builds `ValidateBatch`: it
tags each failure with a `rows.N.field` path, enforces a `MaxErrors` cap with a
`Truncated` flag, supports a fail-fast mode, and honors context cancellation — the
operational concerns the happy-path validator ignores.

This module is fully self-contained: its own `go mod init`, all code inline, its
own demo and tests.

## What you'll build

```text
bulkimport/                independent module: example.com/bulkimport
  go.mod                   go 1.26
  bulkimport.go            Row; Options{MaxErrors,FailFast}; Result{Errors,Truncated}; ValidateBatch(ctx,...)
  cmd/
    demo/
      main.go              runnable demo: capped run and a fail-fast run
  bulkimport_test.go       1000 bad rows+cap=50 -> 50+Truncated; FailFast -> 1; cancelled ctx -> Cause
```

- Files: `bulkimport.go`, `cmd/demo/main.go`, `bulkimport_test.go`.
- Implement: `ValidateBatch(ctx, rows, opts)` that tags each `FieldError` with `rows.N.field`, caps at `opts.MaxErrors` (setting `Truncated`), stops at the first bad row when `opts.FailFast`, and returns `context.Cause(ctx)` when the context is cancelled.
- Test: 1000 all-invalid rows with `MaxErrors=50` -> exactly 50 errors and `Truncated == true`; `FailFast` -> 1 error; a cancelled context stops early and returns `context.Cause`; row paths are `rows.<i>.<field>`.
- Verify: `go test -count=1 -race ./...`

Set up the module:

```bash
go mod edit -go=1.26
```

### Bound the output; respect the deadline

The happy-path validator collects *all* errors, which is right for a 3-field
form and catastrophic for a million-row import: the response balloons and the
handler burns CPU building errors no client will read. A batch validator needs
three guards the single-record one does not:

- A `MaxErrors` cap. Stop appending once the cap is reached and set a `Truncated`
  flag so the client knows the list is partial ("50 shown of many"). The cap is
  checked per-error, not per-row, so a row that would push the total over the cap
  is stopped mid-row.
- A `FailFast` mode. For a validate-then-abort import (reject the whole file on
  the first bad row), stop at the first invalid row and return immediately. There
  is no point scanning the remaining 999,999 rows if the transaction will roll
  back anyway.
- Context cancellation. A very large input can exceed a request deadline; the
  loop checks `ctx.Err()` at the top of each iteration and returns
  `context.Cause(ctx)` — the specific cause set by `context.WithCancelCause` or
  the deadline error — so the caller learns *why* it stopped, not just that it
  did.

Every `FieldError` carries a `rows.N.field` path so a client can point at the
exact cell in the exact row. `rowPath(i)` builds the `rows.N` prefix once per row;
the field name is appended per failure.

Create `bulkimport.go`:

```go
package bulkimport

import (
	"context"
	"fmt"
	"strconv"
)

type Code string

const CodeRequired Code = "required"

type FieldError struct {
	Code  Code
	Field string
}

func (e *FieldError) Error() string {
	return fmt.Sprintf("%s: %s", e.Field, e.Code)
}

// Row is one record in the import.
type Row struct {
	Name  string
	Email string
}

// validate returns every failure for this row, each tagged with the row's path
// prefix.
func (r Row) validate(base string) []*FieldError {
	var out []*FieldError
	if r.Name == "" {
		out = append(out, &FieldError{Code: CodeRequired, Field: base + ".name"})
	}
	if r.Email == "" {
		out = append(out, &FieldError{Code: CodeRequired, Field: base + ".email"})
	}
	return out
}

// Options controls how much work ValidateBatch does under bad input.
type Options struct {
	MaxErrors int  // 0 means unbounded
	FailFast  bool // stop at the first invalid row
}

// Result is the bounded outcome. Truncated is true when MaxErrors capped the
// list.
type Result struct {
	Errors    []*FieldError
	Truncated bool
}

// rowPath builds the "rows.N" prefix for row i.
func rowPath(i int) string {
	return "rows." + strconv.Itoa(i)
}

// ValidateBatch validates rows under the given bounds. It stops early on a
// cancelled context (returning context.Cause), on FailFast at the first bad row,
// or on reaching MaxErrors (setting Truncated).
func ValidateBatch(ctx context.Context, rows []Row, opts Options) (*Result, error) {
	res := &Result{}
	for i := range rows {
		if err := ctx.Err(); err != nil {
			return res, context.Cause(ctx)
		}

		fes := rows[i].validate(rowPath(i))
		if len(fes) == 0 {
			continue
		}

		if opts.FailFast {
			res.Errors = append(res.Errors, fes...)
			return res, nil
		}

		for _, fe := range fes {
			if opts.MaxErrors > 0 && len(res.Errors) >= opts.MaxErrors {
				res.Truncated = true
				return res, nil
			}
			res.Errors = append(res.Errors, fe)
		}
	}
	return res, nil
}
```

### The runnable demo

The demo runs the same bad batch twice — once capped, once fail-fast — to show the
two bounding strategies.

Create `cmd/demo/main.go`:

```go
package main

import (
	"context"
	"fmt"

	"example.com/bulkimport"
)

func main() {
	rows := make([]bulkimport.Row, 5)
	// every row is missing its name

	capped, _ := bulkimport.ValidateBatch(context.Background(), rows,
		bulkimport.Options{MaxErrors: 2})
	fmt.Printf("capped: %d errors, truncated=%v\n", len(capped.Errors), capped.Truncated)

	fast, _ := bulkimport.ValidateBatch(context.Background(), rows,
		bulkimport.Options{FailFast: true})
	fmt.Printf("failfast: %d errors, first=%s\n", len(fast.Errors), fast.Errors[0].Field)
}
```

Run it:

```bash
go run ./cmd/demo
```

Expected output:

```
capped: 2 errors, truncated=true
failfast: 2 errors, first=rows.0.name
```

Each empty row fails two rules (name and email). With `MaxErrors: 2` the cap is
hit after the first row's two errors, so `Truncated` is set. In `FailFast` mode
the first row's two errors are returned and the scan stops immediately, so the
first path is `rows.0.name`.

### Tests

The table drives each bound. The cap test builds 1000 all-invalid rows and asserts
exactly `MaxErrors` errors plus `Truncated`. The fail-fast test asserts the scan
stops at the first bad row. The cancellation test cancels the context with a cause
and asserts `ValidateBatch` returns that cause. A path test pins the `rows.N.field`
format.

Create `bulkimport_test.go`:

```go
package bulkimport

import (
	"context"
	"errors"
	"testing"
)

func allBad(n int) []Row {
	rows := make([]Row, n)
	for i := range rows {
		rows[i] = Row{Name: "", Email: "x@y.z"} // exactly one failure per row
	}
	return rows
}

func TestMaxErrorsCapAndTruncated(t *testing.T) {
	t.Parallel()

	res, err := ValidateBatch(context.Background(), allBad(1000), Options{MaxErrors: 50})
	if err != nil {
		t.Fatal(err)
	}
	if len(res.Errors) != 50 {
		t.Fatalf("len = %d, want 50", len(res.Errors))
	}
	if !res.Truncated {
		t.Fatal("Truncated = false, want true")
	}
}

func TestFailFastStopsAtFirstRow(t *testing.T) {
	t.Parallel()

	res, err := ValidateBatch(context.Background(), allBad(1000), Options{FailFast: true})
	if err != nil {
		t.Fatal(err)
	}
	if len(res.Errors) != 1 {
		t.Fatalf("len = %d, want 1", len(res.Errors))
	}
	if res.Truncated {
		t.Fatal("Truncated = true, want false for FailFast")
	}
}

func TestCancelledContextReturnsCause(t *testing.T) {
	t.Parallel()

	cause := errors.New("import aborted by operator")
	ctx, cancel := context.WithCancelCause(context.Background())
	cancel(cause)

	res, err := ValidateBatch(ctx, allBad(1000), Options{})
	if !errors.Is(err, cause) {
		t.Fatalf("err = %v, want %v", err, cause)
	}
	if len(res.Errors) != 0 {
		t.Fatalf("expected no errors after immediate cancel, got %d", len(res.Errors))
	}
}

func TestRowPaths(t *testing.T) {
	t.Parallel()

	rows := []Row{
		{Name: "Alice", Email: "a@b.c"}, // valid
		{Name: "", Email: ""},           // two failures at index 1
	}
	res, err := ValidateBatch(context.Background(), rows, Options{})
	if err != nil {
		t.Fatal(err)
	}
	want := map[string]bool{"rows.1.name": true, "rows.1.email": true}
	for _, fe := range res.Errors {
		if !want[fe.Field] {
			t.Fatalf("unexpected path %q", fe.Field)
		}
	}
	if len(res.Errors) != 2 {
		t.Fatalf("len = %d, want 2", len(res.Errors))
	}
}
```

An `Example` verified against its `// Output:` comment:

```go
// bulkimport_example_test.go
package bulkimport

import (
	"context"
	"fmt"
)

func ExampleValidateBatch() {
	rows := make([]Row, 3) // all missing name and email
	res, _ := ValidateBatch(context.Background(), rows, Options{MaxErrors: 4})
	fmt.Println(len(res.Errors), res.Truncated)
	// Output: 4 true
}
```

## Review

The batch validator is correct when it never emits more than `MaxErrors` errors
(and flags `Truncated` when it stops for that reason), when `FailFast` returns as
soon as the first invalid row is seen, and when a cancelled context stops the scan
and surfaces `context.Cause` so the caller knows why. The row-path format
`rows.N.field` is what lets a client map a batch error back to a specific input
cell. The mistake this module closes is the unbounded error list: on an
adversarial million-row file, collecting every failure blows up response size and
latency, so a batch path must cap and it must honor cancellation. Run
`go test -race` to confirm the bounds hold and nothing leaks a goroutine.

## Resources

- [`context.Cause`](https://pkg.go.dev/context#Cause) and [`context.WithCancelCause`](https://pkg.go.dev/context#WithCancelCause) — propagating the specific reason a batch stopped.
- [`errors` package](https://pkg.go.dev/errors) — `errors.Is` against the cancellation cause.
- [`strconv.Itoa`](https://pkg.go.dev/strconv#Itoa) — building the `rows.N` path segment.

---

Back to [00-concepts.md](00-concepts.md) | Prev: [08-db-constraint-to-field-error.md](08-db-constraint-to-field-error.md) | Next: [../12-retry-patterns-with-backoff/00-concepts.md](../12-retry-patterns-with-backoff/00-concepts.md)
