# Exercise 2: Repository Layer — Stream Rows as `iter.Seq` Without Materializing a Slice

A repository method that returns `[]User` loads the whole result set into memory;
on a large table that is an out-of-memory waiting to happen. This exercise builds
`ScanUsers`, which returns an `iter.Seq[User]` that yields rows one at a time and
closes the cursor via `defer` inside the iterator body — so an early consumer
break releases the database connection instead of leaking it.

## What you'll build

```text
repo/                     independent module: example.com/repo
  go.mod                  module example.com/repo
  repo.go                 Rows interface, MemoryRows stub, ScanUsers, First
  cmd/
    demo/
      main.go             runnable demo: stream users, print rows and Close count
  repo_test.go            full-drain, early-break, and Err-surfacing tests, -race
```

Files: `repo.go`, `cmd/demo/main.go`, `repo_test.go`.
Implement: `ScanUsers(rs Rows, errp *error) iter.Seq[User]` that defers `Close`, a `MemoryRows` cursor stub, and `First` that reads one row and stops.
Test: full drain closes once and yields all; break after one row still closes; `rs.Err()` surfaces via the captured `*error`.
Verify: `go test -count=1 -race ./...`

Set up the module:

```bash
mkdir -p go-solutions/03-control-flow/09-range-over-integers-and-functions/02-repository-seq-scan/cmd/demo
cd go-solutions/03-control-flow/09-range-over-integers-and-functions/02-repository-seq-scan
```

## The design

`ScanUsers` models a `database/sql` scan: `rs.Next()` advances the cursor,
`rs.Scan(&dest...)` copies the current row, `rs.Err()` reports a terminal error,
`rs.Close()` releases the connection. Returning an `iter.Seq[User]` means the
caller pulls rows on demand — a `First` that wants one row never forces the driver
to buffer the rest, and the memory footprint is one row, not the whole table.

The resource contract is the reason this is an iterator and not a channel. The
`defer rs.Close()` sits at the top of the iterator body, so it runs whether the
loop drains naturally, the consumer breaks after one row, or the body panics. If
you closed the cursor after the loop instead, an early `break` would skip it and
the connection would leak back to the pool held-open — the classic
`rows.Close()`-in-the-wrong-place bug, now made worse because the consumer, not
the repository, decides when to stop.

Errors use the captured-`*error` idiom rather than `iter.Seq2[T, error]`, because
a scan error here is terminal (the cursor failed), not per-row. This mirrors
`database/sql`'s own `rows.Err()`: the consumer ranges to completion, then reads
the error the iterator wrote through the pointer. The per-item `Seq2[T, error]`
idiom is the subject of a later exercise; the two are complementary, and the split
is by whether the failure is terminal or per-item.

Create `repo.go`:

```go
package repo

import "iter"

// User is one row of the users table.
type User struct {
	ID   int
	Name string
}

// Rows is the subset of database/sql.Rows this repository needs.
type Rows interface {
	Next() bool
	Scan(dest ...any) error
	Close() error
	Err() error
}

// ScanUsers streams rows one at a time. It closes the cursor via defer so an
// early consumer break still releases the connection, and writes any terminal
// error through errp for the caller to read after the loop.
func ScanUsers(rs Rows, errp *error) iter.Seq[User] {
	return func(yield func(User) bool) {
		defer rs.Close()
		for rs.Next() {
			var u User
			if err := rs.Scan(&u.ID, &u.Name); err != nil {
				if errp != nil {
					*errp = err
				}
				return
			}
			if !yield(u) {
				return
			}
		}
		if err := rs.Err(); err != nil && errp != nil {
			*errp = err
		}
	}
}

// First reads a single value from seq and stops, releasing any resource the
// producer holds via its cooperative-stop path.
func First[T any](seq iter.Seq[T]) (T, bool) {
	for v := range seq {
		return v, true
	}
	var zero T
	return zero, false
}

// MemoryRows is an in-memory Rows for tests and demos, modeling a sql.Rows
// cursor without a real database. It records call counts so tests can assert
// the cursor was closed and not over-scanned.
type MemoryRows struct {
	data       []User
	pos        int // current row index; -1 before the first Next
	err        error
	NextCalls  int
	CloseCalls int
}

// NewMemoryRows returns a cursor over data. Optionally set err to simulate a
// terminal cursor failure surfaced through Err.
func NewMemoryRows(data []User, err error) *MemoryRows {
	return &MemoryRows{data: data, pos: -1, err: err}
}

func (r *MemoryRows) Next() bool {
	r.NextCalls++
	r.pos++
	return r.pos < len(r.data)
}

func (r *MemoryRows) Scan(dest ...any) error {
	if r.pos < 0 || r.pos >= len(r.data) {
		return nil
	}
	u := r.data[r.pos]
	*(dest[0].(*int)) = u.ID
	*(dest[1].(*string)) = u.Name
	return nil
}

func (r *MemoryRows) Close() error {
	r.CloseCalls++
	return nil
}

func (r *MemoryRows) Err() error { return r.err }
```

## Demo

Create `cmd/demo/main.go`:

```go
package main

import (
	"fmt"

	"example.com/repo"
)

func main() {
	rows := repo.NewMemoryRows([]repo.User{
		{ID: 1, Name: "alice"},
		{ID: 2, Name: "bob"},
		{ID: 3, Name: "carol"},
	}, nil)

	var err error
	for u := range repo.ScanUsers(rows, &err) {
		fmt.Printf("%d %s\n", u.ID, u.Name)
	}
	if err != nil {
		fmt.Println("scan error:", err)
	}
	fmt.Printf("closed %d time(s)\n", rows.CloseCalls)
}
```

Run it:

```bash
go run ./cmd/demo
```

Expected output:

```
1 alice
2 bob
3 carol
closed 1 time(s)
```

## Tests

Create `repo_test.go`:

```go
package repo

import (
	"errors"
	"testing"
)

func rows() []User {
	return []User{{ID: 1, Name: "alice"}, {ID: 2, Name: "bob"}, {ID: 3, Name: "carol"}}
}

func TestFullDrainClosesOnce(t *testing.T) {
	t.Parallel()

	rs := NewMemoryRows(rows(), nil)
	var err error
	var got []User
	for u := range ScanUsers(rs, &err) {
		got = append(got, u)
	}

	if err != nil {
		t.Fatalf("unexpected err: %v", err)
	}
	if len(got) != 3 {
		t.Fatalf("yielded %d rows, want 3", len(got))
	}
	if rs.CloseCalls != 1 {
		t.Fatalf("CloseCalls = %d, want 1", rs.CloseCalls)
	}
}

func TestEarlyBreakStillCloses(t *testing.T) {
	t.Parallel()

	rs := NewMemoryRows(rows(), nil)
	u, ok := First(ScanUsers(rs, nil))

	if !ok || u.ID != 1 {
		t.Fatalf("First = %+v,%v, want id 1,true", u, ok)
	}
	if rs.CloseCalls != 1 {
		t.Fatalf("CloseCalls = %d, want 1 (defer must fire on early break)", rs.CloseCalls)
	}
	if rs.NextCalls > 2 {
		t.Fatalf("NextCalls = %d, want <= 2 (must not scan the whole table)", rs.NextCalls)
	}
}

func TestCursorErrSurfaces(t *testing.T) {
	t.Parallel()

	sentinel := errors.New("connection reset")
	rs := NewMemoryRows(rows(), sentinel)
	var err error
	for range ScanUsers(rs, &err) {
	}

	if !errors.Is(err, sentinel) {
		t.Fatalf("err = %v, want %v", err, sentinel)
	}
	if rs.CloseCalls != 1 {
		t.Fatalf("CloseCalls = %d, want 1", rs.CloseCalls)
	}
}
```

## Review

The method is correct when `Close` is called exactly once on every exit path —
full drain, early break, and error — which is what the `defer rs.Close()` at the
top of the iterator body guarantees and what the `CloseCalls` counter pins down.
The early-break test is the important one: `First` returns after the first row,
the range loop signals the producer to stop, and the deferred `Close` still fires;
`NextCalls <= 2` proves the driver was not asked to walk the whole table. The
error is terminal, so it flows through the captured `*error` the way
`database/sql` surfaces `rows.Err()`, and `errors.Is` recovers the sentinel. Run
`go test -race` to confirm the cursor stub has no data race under the scan.

## Resources

- [`iter` package documentation](https://pkg.go.dev/iter)
- [`database/sql` Rows](https://pkg.go.dev/database/sql#Rows)
- [Go blog: Range Over Function Types](https://go.dev/blog/range-functions)

---

Back to [00-concepts.md](00-concepts.md) | Prev: [01-streaming-pipeline-combinators.md](01-streaming-pipeline-combinators.md) | Next: [03-paginated-list-iterator.md](03-paginated-list-iterator.md)
