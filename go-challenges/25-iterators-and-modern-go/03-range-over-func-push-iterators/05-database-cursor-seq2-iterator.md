# Exercise 5: A Streaming Database Cursor as iter.Seq2[Row, error]

A SQL result set is the canonical case for streaming: you want to process rows
one at a time without ever holding the whole set in memory, and any row read can
fail mid-stream. The classic `database/sql` answer is the `Next()/Scan()/Err()`
pull loop. This exercise adapts that pull cursor to a push `iter.Seq2[Row, error]`,
where every value carries its error alongside it, the walk materializes rows only
as far as the consumer goes, and a corrupt row surfaces as a non-nil error that
ends the stream.

This module is fully self-contained. It begins with its own `go mod init`,
defines every type it needs, and ships its own demo and tests. Nothing here
imports any other exercise.

## What you'll build

```text
rowiter.go           a fake row source and RowReader.Rows (iter.Seq2[Row, error])
cmd/
  demo/
    main.go          stream rows, then a run that hits an error row
rowiter_test.go      full stream, early termination materializes fewer rows, error row stops the stream
```

- Files: `rowiter.go`, `cmd/demo/main.go`, `rowiter_test.go`.
- Implement: a `RowReader` that produces rows lazily (incrementing a `Produced`
  counter as each row is materialized) and `RowReader.Rows() iter.Seq2[Row, error]`
  that yields `(row, nil)` per row, yields `(Row{}, err)` and stops at a configured
  bad row, and honors the yield protocol; a sentinel `ErrCorruptRow`.
- Test: `rowiter_test.go` checks a clean stream returns every row, checks early
  termination materializes only the rows consumed (not the whole set), and checks
  an error row surfaces a non-nil error wrapping `ErrCorruptRow` and ends the
  stream.
- Verify: `go test -run TestRows -race ./...`

Set up the module:

```bash
mkdir -p go-solutions/25-iterators-and-modern-go/03-range-over-func-push-iterators/05-database-cursor-seq2-iterator/cmd/demo && cd go-solutions/25-iterators-and-modern-go/03-range-over-func-push-iterators/05-database-cursor-seq2-iterator
```

### From the pull cursor to a push pair

`database/sql` exposes a result set as a pull cursor: the consumer drives it with
`for rows.Next() { rows.Scan(&x) }` and then must check `rows.Err()` afterward,
because `Next` returns `false` both at a clean end and at a failure -- the two are
distinguishable only by the separate `Err` call. That split is the part people
forget, and a missed `rows.Err()` silently turns a mid-stream failure into a
truncated-but-successful-looking read.

The push adaptation folds the error into the value stream. `iter.Seq2[Row, error]`
yields a pair every turn: on a good row, `(row, nil)`; on a failure, `(Row{}, err)`
followed immediately by a stop. The consumer becomes a single `for row, err :=
range r.Rows()` whose body checks `err` first -- there is no separate terminal
call to forget, because the error arrives inline as just another element. This is
the established convention for `Seq2` iterators that can fail, mirroring how a
function returning `(T, error)` threads the error next to the value.

### Streaming, and what "lazy" buys you here

The reader produces each row only when the iterator's loop reaches it, and bumps
`Produced` at the moment of materialization. In a real driver that materialization
is a row decoded out of the network buffer; here it is a struct built on the fly.
The counter exists so a test can prove the stream never built rows the consumer
did not ask for:

```go
func (r *RowReader) Rows() iter.Seq2[Row, error] {
	return func(yield func(Row, error) bool) {
		for i := 0; i < r.total; i++ {
			if i == r.errAt {
				yield(Row{}, fmt.Errorf("scan row %d: %w", i, ErrCorruptRow))
				return
			}
			r.Produced++
			row := Row{ID: i, Name: fmt.Sprintf("row-%d", i)}
			if !yield(row, nil) {
				return
			}
		}
	}
}
```

Three behaviors live in that loop. A good row increments `Produced`, then yields
`(row, nil)`; if `yield` returns `false` the consumer broke, so the iterator
returns and the rows past that point are never produced -- which is the whole
reason you stream a million-row table instead of loading it. The error row yields
`(Row{}, err)` and returns unconditionally: a failed read is terminal, so the
stream does not continue past it, and the bad row is not counted in `Produced`
because no row was successfully materialized. The wrapped sentinel lets the
consumer test the failure category with `errors.Is(err, ErrCorruptRow)` while
still reading the contextual message.

Create `rowiter.go`:

```go
// Package rowiter adapts a pull-style database cursor to a push
// iter.Seq2[Row, error] that streams rows lazily and surfaces a failed read as
// an inline error that ends the stream.
package rowiter

import (
	"errors"
	"fmt"
	"iter"
)

// ErrCorruptRow is the sentinel a failed row read wraps.
var ErrCorruptRow = errors.New("corrupt row")

// Row is one decoded record.
type Row struct {
	ID   int
	Name string
}

// RowReader is a fake streaming result set. total is how many rows the set would
// hold; errAt is the index whose read fails (-1 for none); Produced counts the
// rows actually materialized, so a test can prove the read was lazy.
type RowReader struct {
	total    int
	errAt    int
	Produced int
}

// NewRowReader returns a clean result set of total rows with no failures.
func NewRowReader(total int) *RowReader {
	return &RowReader{total: total, errAt: -1}
}

// WithErrorAt returns a result set of total rows whose read fails at index errAt.
func WithErrorAt(total, errAt int) *RowReader {
	return &RowReader{total: total, errAt: errAt}
}

// Rows streams the result set as (row, nil) pairs. It materializes each row only
// when the consumer reaches it, stops when the consumer breaks, and on the
// configured bad row yields (Row{}, err) once and ends the stream.
func (r *RowReader) Rows() iter.Seq2[Row, error] {
	return func(yield func(Row, error) bool) {
		for i := 0; i < r.total; i++ {
			if i == r.errAt {
				yield(Row{}, fmt.Errorf("scan row %d: %w", i, ErrCorruptRow))
				return
			}
			r.Produced++
			row := Row{ID: i, Name: fmt.Sprintf("row-%d", i)}
			if !yield(row, nil) {
				return
			}
		}
	}
}
```

### The runnable demo

The demo streams a clean four-row set and reports how many rows were produced,
then streams a set that fails on its third row to show the error arriving inline
and the loop stopping. The error branch uses `errors.Is` to confirm the category.

Create `cmd/demo/main.go`:

```go
package main

import (
	"errors"
	"fmt"

	"example.com/rowiter"
)

func main() {
	clean := rowiter.NewRowReader(4)
	fmt.Print("rows:")
	for row, err := range clean.Rows() {
		if err != nil {
			fmt.Printf(" [error: %v]", err)
			break
		}
		fmt.Printf(" %s", row.Name)
	}
	fmt.Printf("\nproduced %d rows\n", clean.Produced)

	bad := rowiter.WithErrorAt(5, 2)
	fmt.Print("rows:")
	for row, err := range bad.Rows() {
		if err != nil {
			fmt.Printf(" [stopped: corrupt=%v]", errors.Is(err, rowiter.ErrCorruptRow))
			break
		}
		fmt.Printf(" %s", row.Name)
	}
	fmt.Printf("\nproduced %d rows\n", bad.Produced)
}
```

Run it:

```bash
go run ./cmd/demo
```

Expected output:

```
rows: row-0 row-1 row-2 row-3
produced 4 rows
rows: row-0 row-1 [stopped: corrupt=true]
produced 2 rows
```

### Tests

`TestRowsAll` streams a clean set and asserts every row arrives in order with no
error, and that `Produced` equals the set size. `TestRowsEarlyTermination` is the
streaming proof: it breaks after three rows out of a thousand and asserts
`Produced == 3`, demonstrating the iterator built only what was consumed and never
the whole set. `TestRowsError` streams a set that fails at index two and asserts
the consumer saw exactly two good rows, then a non-nil error that
`errors.Is(err, ErrCorruptRow)` matches, and that the stream ended there with
`Produced == 2` -- the bad row never counted.

Create `rowiter_test.go`:

```go
package rowiter

import (
	"errors"
	"testing"
)

func TestRowsAll(t *testing.T) {
	t.Parallel()

	r := NewRowReader(5)
	var ids []int
	for row, err := range r.Rows() {
		if err != nil {
			t.Fatalf("unexpected error: %v", err)
		}
		ids = append(ids, row.ID)
	}
	want := []int{0, 1, 2, 3, 4}
	if len(ids) != len(want) {
		t.Fatalf("streamed %d rows, want %d", len(ids), len(want))
	}
	for i := range want {
		if ids[i] != want[i] {
			t.Fatalf("row %d id = %d, want %d", i, ids[i], want[i])
		}
	}
	if r.Produced != 5 {
		t.Fatalf("produced %d rows, want 5", r.Produced)
	}
}

func TestRowsEarlyTermination(t *testing.T) {
	t.Parallel()

	r := NewRowReader(1000)
	var got []int
	for row, err := range r.Rows() {
		if err != nil {
			t.Fatalf("unexpected error: %v", err)
		}
		got = append(got, row.ID)
		if len(got) == 3 {
			break
		}
	}
	if len(got) != 3 {
		t.Fatalf("consumed %d rows, want 3", len(got))
	}
	if r.Produced != 3 {
		t.Fatalf("materialized %d of 1000 rows, want only 3 (no full load)", r.Produced)
	}
}

func TestRowsError(t *testing.T) {
	t.Parallel()

	r := WithErrorAt(5, 2)
	var good []int
	var gotErr error
	for row, err := range r.Rows() {
		if err != nil {
			gotErr = err
			break
		}
		good = append(good, row.ID)
	}
	if want := []int{0, 1}; len(good) != len(want) || good[0] != 0 || good[1] != 1 {
		t.Fatalf("good rows = %v, want %v", good, want)
	}
	if gotErr == nil {
		t.Fatal("expected an error from the bad row, got nil")
	}
	if !errors.Is(gotErr, ErrCorruptRow) {
		t.Fatalf("error %v does not wrap ErrCorruptRow", gotErr)
	}
	if r.Produced != 2 {
		t.Fatalf("produced %d rows, want 2 (bad row not counted)", r.Produced)
	}
}
```

## Review

The streaming row iterator is correct when three properties hold: a clean set
streams every row in order, an early break materializes only the rows consumed,
and a failed read surfaces inline and ends the stream. The first is checked by
`TestRowsAll`. The second -- the reason to stream at all -- is checked by
`TestRowsEarlyTermination` asserting `Produced == 3` out of a thousand, which can
only hold if the iterator returns out of its loop on the consumer's `break`. The
third is checked by `TestRowsError`: two good rows, then a non-nil error wrapping
`ErrCorruptRow`, then nothing, with `Produced == 2`. Confirm the consumer checks
`err` before using `row`, since on the error turn the row is the zero `Row{}`.

Common mistakes for this feature. The first is buffering the whole result set
into a slice and ranging that, which passes the clean and error cases but fails
the streaming proof because every row is materialized regardless of where the
consumer stops -- the opposite of why you stream. The second is yielding the error
and then continuing the loop, or yielding a real row alongside a non-nil error; a
failed read is terminal and the row on that turn must be the zero value, so the
iterator returns right after the error yield. The third is dropping the sentinel
wrap and returning a bare formatted error, which leaves the consumer unable to
classify the failure with `errors.Is`; always wrap with `%w`.

## Resources

- [`iter` package](https://pkg.go.dev/iter) -- `Seq2[K, V]` and the two-value yield
  contract, here used as `Seq2[Row, error]`.
- [`database/sql` Rows](https://pkg.go.dev/database/sql#Rows) -- the
  `Next()/Scan()/Err()` pull cursor this exercise adapts into a push iterator.
- [`errors` package](https://pkg.go.dev/errors) -- `errors.Is` and `%w` wrapping,
  which let the consumer classify the inline failure.

---

Back to [04-paginated-api-push-iterator.md](04-paginated-api-push-iterator.md) | Next: [../04-range-over-func-pull-iterators/00-concepts.md](../04-range-over-func-pull-iterators/00-concepts.md)
