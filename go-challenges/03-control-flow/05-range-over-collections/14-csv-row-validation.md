# Exercise 14: CSV Row Validation Collecting Per-Index Errors

**Nivel: Intermedio** — validacion rapida (un test corto).

A bulk-import endpoint that rejects an entire CSV upload because one row was
bad is a bad experience — the caller needs to know exactly *which* rows failed
and why. This module ranges parsed rows with the index/value form, runs three
field checks per row, and reports failures keyed by the row's original
position, so a caller can point a user straight at "row 2".

## What you'll build

```text
csvvalidate/                independent module: example.com/csv-row-validate
  go.mod                     go 1.24
  validate.go                 type RowError; ValidateRows(rows [][]string) []RowError
  validate_test.go            table test: valid + multi-problem + malformed rows
```

- Files: `validate.go`, `validate_test.go`.
- Implement: `ValidateRows(rows [][]string) []RowError` ranging rows with
  `for i, row := range rows`, checking id/email/age, and collecting every
  problem found per row under that row's index.
- Test: one table mixing a valid row, a row with three simultaneous problems,
  a row with one problem, and a malformed (wrong column count) row.
- Verify: `go test -count=1 ./...`

Set up the module:

```bash
go mod edit -go=1.24
```

### Why the index is the point, not a detail

Every other exercise in this chapter blanks the index with `_` because the
value is all that matters. Here it is the opposite: the index *is* the output.
`for i, row := range rows` is chosen specifically because a `RowError` without
its origin row number is useless to the caller — "email missing @" tells a
user nothing; "row 1: email missing @" tells them where to look. Collecting
every problem a row has, rather than stopping at the first, also matters in
practice: a row with a blank id, a bad email, and a bad age should be reported
once with all three problems, not as three separate round trips through a
fix-resubmit-fail cycle. Rows that pass are simply absent from the result —
the caller only hears about what needs fixing.

Create `validate.go`:

```go
package csvvalidate

import (
	"strconv"
	"strings"
)

// RowError collects every validation problem found in one CSV row, keyed by
// the row's zero-based position in the input.
type RowError struct {
	Index    int
	Problems []string
}

// ValidateRows checks each row — expected columns are [id, email, age] — and
// returns one RowError per row that failed, in input order. Ranging with the
// index/value form is what lets each error report exactly which row it came
// from; rows that pass every check are simply absent from the result.
func ValidateRows(rows [][]string) []RowError {
	var errs []RowError

	for i, row := range rows {
		var problems []string

		if len(row) != 3 {
			errs = append(errs, RowError{Index: i, Problems: []string{"expected 3 columns"}})
			continue
		}

		id, email, age := row[0], row[1], row[2]

		if strings.TrimSpace(id) == "" {
			problems = append(problems, "id is empty")
		}
		if !strings.Contains(email, "@") {
			problems = append(problems, "email missing @")
		}
		if n, err := strconv.Atoi(age); err != nil || n <= 0 {
			problems = append(problems, "age is not a positive integer")
		}

		if len(problems) > 0 {
			errs = append(errs, RowError{Index: i, Problems: problems})
		}
	}

	return errs
}
```

### Test

The table drives four rows through one call: a valid row (absent from the
result), a row failing all three checks at once, a row failing only the age
check, and a malformed row with too few columns.

Create `validate_test.go`:

```go
package csvvalidate

import (
	"reflect"
	"testing"
)

func TestValidateRows(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name string
		rows [][]string
		want []RowError
	}{
		{
			name: "no rows",
			rows: [][]string{},
			want: nil,
		},
		{
			name: "mixed valid, invalid, and malformed rows",
			rows: [][]string{
				{"u1", "a@example.com", "30"},
				{"", "not-an-email", "-5"},
				{"u3", "b@example.com", "abc"},
				{"u1-only-two-cols", "a@example.com"},
			},
			want: []RowError{
				{Index: 1, Problems: []string{"id is empty", "email missing @", "age is not a positive integer"}},
				{Index: 2, Problems: []string{"age is not a positive integer"}},
				{Index: 3, Problems: []string{"expected 3 columns"}},
			},
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			got := ValidateRows(tc.rows)
			if !reflect.DeepEqual(got, tc.want) {
				t.Errorf("ValidateRows() = %+v, want %+v", got, tc.want)
			}
		})
	}
}
```

Run it:

```bash
go test -count=1 ./...
```

## Review

The function is correct when every failing row appears exactly once with every
problem it has, at its original index, and every passing row is silent. The
malformed-row branch (`len(row) != 3`) has to `continue` before indexing
`row[0]`/`row[1]`/`row[2]`, or a two-column row panics with an index-out-of-range
instead of reporting a clean "expected 3 columns". Note row index 0 in the test
is intentionally valid — it pins that the zero value of `int` (`Index: 0`)
never gets confused with "no error for this row" in a reader's mental model,
since a `RowError` is only ever constructed for a row with an actual problem.

## Resources

- [Go Specification: For statements (range over slice)](https://go.dev/ref/spec#For_range)
- [strconv.Atoi](https://pkg.go.dev/strconv#Atoi)

---

Back to [00-concepts.md](00-concepts.md) | Prev: [13-leaderboard-top-n.md](13-leaderboard-top-n.md) | Next: [15-tag-dedup-first-seen.md](15-tag-dedup-first-seen.md)
