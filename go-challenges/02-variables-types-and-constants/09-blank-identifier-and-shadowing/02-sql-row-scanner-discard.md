# Exercise 2: Repository Scan Loop: Skipping Columns and Never Dropping rows.Err()

A repository read path scans query rows into domain structs behind a small
`RowScanner` interface that mirrors `*sql.Rows`, so it is testable without a
database. Two rules earn their keep here: `_` cannot be a `Scan` destination, and
the post-loop `rows.Err()` check is mandatory.

This module is fully self-contained: its own `go mod init`, all code inline, its
own demo and tests. Nothing here imports another exercise.

## What you'll build

```text
rowscanner/                     module: example.com/rowscanner
  go.mod
  scanner.go                    RowScanner, User, ScanUsers, ScanActiveUsers
  cmd/
    demo/
      main.go                   in-memory rows, scan two users, print them
  scanner_test.go               normal scan, mid-iteration Err(), skipped column, Close called
```

- Files: `scanner.go`, `cmd/demo/main.go`, `scanner_test.go`.
- Implement: `ScanUsers` (two columns) and `ScanActiveUsers` (three columns, middle discarded), each with `defer rows.Close()` and a post-loop `rows.Err()` check.
- Test: a `fakeRows` driving a normal multi-row scan, a mid-iteration error surfaced only through `Err()`, and a row with an unwanted middle column proving the throwaway destination.
- Verify: `go test -count=1 -race ./...`

Set up the module:

```bash
mkdir -p go-solutions/02-variables-types-and-constants/09-blank-identifier-and-shadowing/02-sql-row-scanner-discard/cmd/demo
cd go-solutions/02-variables-types-and-constants/09-blank-identifier-and-shadowing/02-sql-row-scanner-discard
```

### Why `_` cannot be a Scan destination, and why rows.Err() is not optional

`sql.Rows.Scan(dest ...any)` writes each column into the pointer you hand it. A
destination must be an addressable, writable value; `_` is neither, so
`rows.Scan(&id, _, &name)` does not compile. When a query returns a column you do
not want, you still have to *receive* it — into a real throwaway variable whose
only job is to be ignored. `sql.RawBytes` is the conventional choice for a
discarded column: it is a `[]byte` alias that avoids a conversion the driver would
otherwise do. The variable must be per-scan (declared inside the loop), never one
shared destination across iterations, because a shared destination under
concurrent scans is a data race.

The second rule is the post-loop check. `for rows.Next()` stops when `Next`
returns `false`, and `false` is ambiguous: normal end of rows, or a driver error
that aborted iteration. Only `rows.Err()` distinguishes them. Omit it and a
connection reset midway through a large result set silently truncates your answer
to a shorter, plausible, wrong slice — with no error returned. `ScanActiveUsers`
and `ScanUsers` both close the rows with `defer` and check `rows.Err()` after the
loop; the test injects a mid-iteration error and fails if the check is missing.

Create `scanner.go`:

```go
package scanner

import (
	"database/sql"
	"fmt"
)

// RowScanner is the read subset of *sql.Rows we depend on, so the scan logic is
// testable without a real database. *sql.Rows satisfies it.
type RowScanner interface {
	Next() bool
	Scan(dest ...any) error
	Err() error
	Close() error
}

// User is a domain row.
type User struct {
	ID   int
	Name string
}

// ScanUsers reads (id, name) rows into User structs. It closes the rows and
// checks rows.Err() after the loop so a mid-iteration driver error is never
// swallowed.
func ScanUsers(rows RowScanner) ([]User, error) {
	defer rows.Close()

	var users []User
	for rows.Next() {
		var u User
		if err := rows.Scan(&u.ID, &u.Name); err != nil {
			return nil, fmt.Errorf("scan user: %w", err)
		}
		users = append(users, u)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("iterate users: %w", err)
	}
	return users, nil
}

// ScanActiveUsers reads (id, internal_flag, name) rows but wants only id and
// name. The middle column is received into a throwaway sql.RawBytes and ignored;
// _ cannot appear as a Scan destination.
func ScanActiveUsers(rows RowScanner) ([]User, error) {
	defer rows.Close()

	var users []User
	for rows.Next() {
		var (
			u       User
			discard sql.RawBytes // per-scan throwaway for the unwanted column
		)
		if err := rows.Scan(&u.ID, &discard, &u.Name); err != nil {
			return nil, fmt.Errorf("scan user: %w", err)
		}
		users = append(users, u)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("iterate users: %w", err)
	}
	return users, nil
}
```

### The runnable demo

The demo supplies an in-memory `RowScanner` (production would pass `*sql.Rows`)
and scans two users.

Create `cmd/demo/main.go`:

```go
package main

import (
	"fmt"

	scanner "example.com/rowscanner"
)

// memRows is a tiny in-memory RowScanner for the demo.
type memRows struct {
	data [][]any
	pos  int
	cur  []any
}

func (m *memRows) Next() bool {
	if m.pos >= len(m.data) {
		return false
	}
	m.cur = m.data[m.pos]
	m.pos++
	return true
}

func (m *memRows) Scan(dest ...any) error {
	for i, d := range dest {
		switch p := d.(type) {
		case *int:
			*p = m.cur[i].(int)
		case *string:
			*p = m.cur[i].(string)
		}
	}
	return nil
}

func (m *memRows) Err() error   { return nil }
func (m *memRows) Close() error { return nil }

func main() {
	rows := &memRows{data: [][]any{{1, "alice"}, {2, "bob"}}}

	users, err := scanner.ScanUsers(rows)
	if err != nil {
		fmt.Println("error:", err)
		return
	}
	for _, u := range users {
		fmt.Printf("user %d: %s\n", u.ID, u.Name)
	}
}
```

Run it:

```bash
go run ./cmd/demo
```

Expected output:

```
user 1: alice
user 2: bob
```

### Tests

`fakeRows` drives all three cases. `TestScanUsersSurfacesErr` sets an error that
`Err()` returns after the rows are exhausted, proving `ScanUsers` reports it
instead of returning a truncated slice.

Create `scanner_test.go`:

```go
package scanner

import (
	"database/sql"
	"errors"
	"fmt"
	"reflect"
	"testing"
)

type fakeRows struct {
	data   [][]any
	pos    int
	cur    []any
	err    error // returned by Err(), models a mid-iteration driver error
	closed bool
}

var _ RowScanner = (*fakeRows)(nil)

func (f *fakeRows) Next() bool {
	if f.pos >= len(f.data) {
		return false
	}
	f.cur = f.data[f.pos]
	f.pos++
	return true
}

func (f *fakeRows) Err() error   { return f.err }
func (f *fakeRows) Close() error { f.closed = true; return nil }

func (f *fakeRows) Scan(dest ...any) error {
	if len(dest) != len(f.cur) {
		return fmt.Errorf("scan: %d destinations, %d columns", len(dest), len(f.cur))
	}
	for i, d := range dest {
		if err := assign(d, f.cur[i]); err != nil {
			return err
		}
	}
	return nil
}

func assign(dst, src any) error {
	switch d := dst.(type) {
	case *int:
		v, ok := src.(int)
		if !ok {
			return fmt.Errorf("assign int: got %T", src)
		}
		*d = v
	case *string:
		v, ok := src.(string)
		if !ok {
			return fmt.Errorf("assign string: got %T", src)
		}
		*d = v
	case *sql.RawBytes:
		// throwaway destination: accept the column and ignore its value
		*d = nil
	default:
		return fmt.Errorf("assign: unsupported destination %T", dst)
	}
	return nil
}

func TestScanUsers(t *testing.T) {
	t.Parallel()

	rows := &fakeRows{data: [][]any{{1, "alice"}, {2, "bob"}}}
	got, err := ScanUsers(rows)
	if err != nil {
		t.Fatal(err)
	}
	want := []User{{ID: 1, Name: "alice"}, {ID: 2, Name: "bob"}}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("ScanUsers() = %#v, want %#v", got, want)
	}
	if !rows.closed {
		t.Fatal("rows.Close() was not called")
	}
}

func TestScanUsersSurfacesErr(t *testing.T) {
	t.Parallel()

	driverErr := errors.New("connection reset")
	rows := &fakeRows{data: [][]any{{1, "alice"}}, err: driverErr}

	_, err := ScanUsers(rows)
	if !errors.Is(err, driverErr) {
		t.Fatalf("error = %v, want wrap of %v", err, driverErr)
	}
	if !rows.closed {
		t.Fatal("rows.Close() was not called")
	}
}

func TestScanActiveUsersSkipsMiddle(t *testing.T) {
	t.Parallel()

	rows := &fakeRows{data: [][]any{
		{1, "internal-a", "alice"},
		{2, "internal-b", "bob"},
	}}
	got, err := ScanActiveUsers(rows)
	if err != nil {
		t.Fatal(err)
	}
	want := []User{{ID: 1, Name: "alice"}, {ID: 2, Name: "bob"}}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("ScanActiveUsers() = %#v, want %#v", got, want)
	}
}

func ExampleScanUsers() {
	rows := &fakeRows{data: [][]any{{7, "carol"}}}
	users, _ := ScanUsers(rows)
	fmt.Println(users)
	// Output: [{7 carol}]
}
```

## Review

The scan path is correct when a driver error ending iteration is reported, not
absorbed. `TestScanUsersSurfacesErr` sets an error that only `rows.Err()` exposes;
drop the post-loop check and it fails with a nil error and a truncated slice.
`TestScanActiveUsersSkipsMiddle` proves the throwaway pattern: the middle column
is received into a per-scan `sql.RawBytes` and discarded, and the resulting structs
are exactly right. Both functions `defer rows.Close()`, asserted through the
`closed` flag. The mistakes to avoid: `_` as a `Scan` destination (does not
compile), one throwaway shared across scans (a race), and forgetting `rows.Err()`.

## Resources

- [`database/sql.Rows`](https://pkg.go.dev/database/sql#Rows) — `Next`, `Scan`, `Err`, `Close` and their contract.
- [`database/sql.RawBytes`](https://pkg.go.dev/database/sql#RawBytes) — the throwaway destination type.
- [Go Specification: Blank identifier](https://go.dev/ref/spec#Blank_identifier) — why `_` is not an addressable destination.

---

Back to [00-concepts.md](00-concepts.md) | Next: [03-tx-named-return-shadow.md](03-tx-named-return-shadow.md)
