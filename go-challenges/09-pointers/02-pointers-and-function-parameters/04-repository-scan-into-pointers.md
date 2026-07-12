# Exercise 4: Repository Layer — Scanning DB Rows Requires Addresses

`database/sql`'s `Rows.Scan(dest ...any)` is the canonical real-world API that
*structurally* demands pointers: it writes each decoded column back through the
address you hand it. This exercise models that contract with a minimal `Scanner`
interface and a `ScanUser` that passes `&u.ID`, `&u.Name`, `&u.Email`, then proves
what happens when you forget the `&`.

This module is fully self-contained: its own `go mod init`, demo, and tests.

## What you'll build

```text
repo/                       independent module: example.com/repo
  go.mod
  repo.go                   Scanner interface; User (with nullable *string); ScanUser(Scanner, *User)
  cmd/
    demo/
      main.go               a fake row implementing Scanner; scans one user
  repo_test.go              fake Scanner writes through addresses; NULL -> nil; value-copy scan drops the write
```

- Files: `repo.go`, `cmd/demo/main.go`, `repo_test.go`.
- Implement: a `Scanner` interface like `sql.Rows`, a `User` with a nullable `Email *string`, and `ScanUser(s Scanner, u *User)` passing field addresses.
- Test: a fake Scanner copies provided values into the passed addresses; a NULL column maps to a nil pointer; a version that scans into a value copy fails to observe the write.
- Verify: `go test -count=1 -race ./...`

Set up the module:

```bash
mkdir -p go-solutions/09-pointers/02-pointers-and-function-parameters/04-repository-scan-into-pointers/cmd/demo
cd go-solutions/09-pointers/02-pointers-and-function-parameters/04-repository-scan-into-pointers
```

### Why Scan cannot take values

`Scan` receives `dest ...any`, decodes column 0 into `dest[0]`, column 1 into
`dest[1]`, and so on. "Decode into `dest[i]`" means "write the value where `dest[i]`
points." If you passed `u.ID` (a copy of the int), Scan would be handed an `any`
boxing a copy — there is no way to reach back into your struct, so the real
`database/sql` returns an error like "destination not a pointer." The `&` is not
decoration; it is the write channel. `ScanUser` therefore passes `&u.ID`,
`&u.Name`, `&u.Email`, and because `u` is itself a `*User` parameter, those writes
land in the caller's struct.

Nullable columns are why `Email` is a `*string` and Scan is handed `&u.Email` (a
`**string`). A SQL NULL maps to a nil `*string`, distinct from a non-null empty
string. This mirrors how `sql.NullString` or a `*string` destination handles NULLs
in real code: the pointer's nil-ness carries the "was this column NULL" bit that a
plain `string` cannot represent.

Create `repo.go`:

```go
package repo

// Scanner mirrors the one method of database/sql.Rows this layer uses. Scan
// writes each decoded column value back through the pointer in dest[i], which is
// why a repository must pass addresses, not values.
type Scanner interface {
	Scan(dest ...any) error
}

// User is a domain row. Email is nullable: a NULL column becomes a nil *string,
// distinct from a non-null empty string.
type User struct {
	ID    int64
	Name  string
	Email *string
}

// ScanUser reads one row into u. It passes the ADDRESS of each field so Scan can
// write the decoded value through the pointer into the caller's struct.
func ScanUser(s Scanner, u *User) error {
	return s.Scan(&u.ID, &u.Name, &u.Email)
}
```

### The runnable demo

The demo defines its own `Scanner` — realistic, since in production the `Scanner`
is `*sql.Rows`; here a small in-memory row plays that role. It shows one row with a
present email and one with a NULL email.

Create `cmd/demo/main.go`:

```go
package main

import (
	"fmt"

	"example.com/repo"
)

// memRow is an in-memory Scanner: it copies its stored column values into the
// addresses Scan is given, exactly as *sql.Rows would.
type memRow struct {
	id    int64
	name  string
	email any // string for a value, nil for SQL NULL
}

func (r *memRow) Scan(dest ...any) error {
	*(dest[0].(*int64)) = r.id
	*(dest[1].(*string)) = r.name
	pe := dest[2].(**string)
	if r.email == nil {
		*pe = nil
	} else {
		v := r.email.(string)
		*pe = &v
	}
	return nil
}

func main() {
	var withEmail repo.User
	_ = repo.ScanUser(&memRow{id: 1, name: "Ada", email: "ada@x.com"}, &withEmail)
	fmt.Printf("id=%d name=%s email=%s\n", withEmail.ID, withEmail.Name, *withEmail.Email)

	var nullEmail repo.User
	_ = repo.ScanUser(&memRow{id: 2, name: "Bob", email: nil}, &nullEmail)
	fmt.Printf("id=%d name=%s email-is-nil=%v\n", nullEmail.ID, nullEmail.Name, nullEmail.Email == nil)
}
```

Run it:

```bash
go run ./cmd/demo
```

Expected output:

```
id=1 name=Ada email=ada@x.com
id=2 name=Bob email-is-nil=true
```

### Tests

The tests pin three things: Scan populates every field through the addresses, a
NULL maps to a nil pointer, and the value-copy version (the classic mistake) fails
to observe the write.

Create `repo_test.go`:

```go
package repo

import "testing"

// fakeRows copies its stored columns into the destination addresses, like
// *sql.Rows. cols holds one value per column; a nil entry means SQL NULL.
type fakeRows struct {
	cols []any
}

func (f *fakeRows) Scan(dest ...any) error {
	for i, d := range dest {
		switch p := d.(type) {
		case *int64:
			*p = f.cols[i].(int64)
		case *string:
			*p = f.cols[i].(string)
		case **string:
			if f.cols[i] == nil {
				*p = nil
			} else {
				v := f.cols[i].(string)
				*p = &v
			}
		}
	}
	return nil
}

// brokenScanUser scans into a COPY of u (value parameter). The writes land in the
// copy, so the caller never sees them — the exact bug pointers prevent.
func brokenScanUser(s Scanner, u User) error {
	return s.Scan(&u.ID, &u.Name, &u.Email)
}

func TestScanUserPopulatesFields(t *testing.T) {
	t.Parallel()
	rows := &fakeRows{cols: []any{int64(7), "Ada", "ada@x.com"}}
	var u User
	if err := ScanUser(rows, &u); err != nil {
		t.Fatalf("ScanUser: %v", err)
	}
	if u.ID != 7 || u.Name != "Ada" {
		t.Fatalf("scalar fields wrong: %+v", u)
	}
	if u.Email == nil || *u.Email != "ada@x.com" {
		t.Fatalf("Email = %v, want ada@x.com", u.Email)
	}
}

func TestScanUserNullEmail(t *testing.T) {
	t.Parallel()
	rows := &fakeRows{cols: []any{int64(8), "Bob", nil}}
	var u User
	if err := ScanUser(rows, &u); err != nil {
		t.Fatalf("ScanUser: %v", err)
	}
	if u.Email != nil {
		t.Fatalf("Email = %v, want nil for NULL column", u.Email)
	}
}

// TestValueCopyScanDropsWrite documents why Scan must reach the caller's struct:
// scanning into a value copy leaves the original zero.
func TestValueCopyScanDropsWrite(t *testing.T) {
	t.Parallel()
	rows := &fakeRows{cols: []any{int64(7), "Ada", "ada@x.com"}}
	var u User
	if err := brokenScanUser(rows, u); err != nil {
		t.Fatalf("brokenScanUser: %v", err)
	}
	if u.ID != 0 || u.Name != "" || u.Email != nil {
		t.Fatalf("value-copy scan unexpectedly mutated caller: %+v", u)
	}
}
```

## Review

The layer is correct when Scan writes reach the caller's `User` — which requires
both `ScanUser` taking `*User` and passing `&u.Field` for each destination. The
`brokenScanUser` test is the whole lesson in one assertion: scan into a value copy
and the caller's struct stays zero, because the writes went into a parameter that
died at the return. Nullable columns need a pointer destination so NULL and empty
string stay distinct — the same "pointer carries an extra bit of presence" idea from
the PATCH exercise, here imposed by the database, not by you. In real code prefer
`sql.NullString`/`sql.Null[T]` or a `*string` per column; the mechanism is
identical. Run `go test -race` to confirm the fakes and scan are clean.

## Resources

- [`database/sql` Rows.Scan](https://pkg.go.dev/database/sql#Rows.Scan) — the real signature and its pointer requirement.
- [`sql.NullString`](https://pkg.go.dev/database/sql#NullString) — the stdlib way to scan a nullable column.
- [Accessing a SQL database (Go tutorial)](https://go.dev/doc/tutorial/database-access) — end-to-end use of `rows.Scan` with addresses.

---

Back to [00-concepts.md](00-concepts.md) | Previous: [03-functional-options-constructor.md](03-functional-options-constructor.md) | Next: [05-mutate-slice-elements-in-place.md](05-mutate-slice-elements-in-place.md)
