# Exercise 10: Nullable DB Columns — sql.Null[T] And Pointers vs Zero

The most damaging zero-value bug lives in the DB layer: scanning a nullable
column straight into a `string` or `int64` turns `NULL` into `""` or `0`, so
"this user has no nickname" and "this user's nickname is the empty string" become
the same thing, and "not deleted" and "deleted at the epoch" collapse. This
exercise builds a scan layer that maps nullable columns through `sql.Null[T]` and
into domain *pointers*, keeping `NULL` distinct from the zero value.

This module is fully self-contained: its own `go mod init`, all code inline, its
own demo and tests. It uses `database/sql` types only — no live database, no
driver — so it builds and tests offline.

## What you'll build

```text
repo/                      independent module: example.com/repo
  go.mod
  repo.go                  User (pointer fields), Row, ScanUser, fakeRow
  cmd/
    demo/
      main.go              scans NULL vs present rows, prints the domain user
  repo_test.go             NULL -> nil pointer; present-zero -> non-nil; wrap err
```

Files: `repo.go`, `cmd/demo/main.go`, `repo_test.go`.
Implement: a domain `User{ID int64; Nickname *string; DeletedAt *time.Time}`; a `ScanUser(Row)` that scans into `sql.NullString`/`sql.Null[time.Time]` targets and converts to pointers; a `fakeRow` implementing the `Scan(dest ...any)` contract for tests.
Test: `Valid=false` yields a nil domain pointer, not the zero value; `Valid=true` carries the value through; a genuinely-present empty string is preserved as a non-nil `*string`; a scan error is wrapped with `%w`.
Verify: `go test -count=1 -race ./...`

Set up the module:

```bash
mkdir -p go-solutions/02-variables-types-and-constants/02-zero-values-and-default-initialization/10-sql-null-vs-pointer-columns/cmd/demo
cd go-solutions/02-variables-types-and-constants/02-zero-values-and-default-initialization/10-sql-null-vs-pointer-columns
```

## Why sql.Null[T] first, pointer second

`database/sql` cannot scan a SQL `NULL` into a plain `string` or `int64` — the
scan errors, or with some drivers silently yields the zero value, which is the
data-corruption path. The correct scan target for a nullable column is a type
that carries validity alongside the value: the generic `sql.Null[T]` (Go 1.22+),
whose fields are `V T` and `Valid bool`, or the older concrete forms
`sql.NullString{String, Valid}` and `sql.NullInt64{Int64, Valid}`. After the
scan, `Valid` tells you whether the column was `NULL` (`false`) or held a real
value (`true`), *including* a real zero value.

The conversion to the domain type is where the distinction is preserved. A domain
`*string` nickname is `nil` when `Valid == false` (the column was `NULL`) and
points at the scanned string when `Valid == true` — even if that string is `""`.
That is the crux: a present-but-empty nickname produces a non-nil pointer to
`""`, which is genuinely different from a `NULL` nickname's nil pointer. Collapse
those two and you have merged "no value" with "empty value", a bug that silently
corrupts every downstream decision keyed on nickname presence. Same story for
`DeletedAt`: a nil `*time.Time` means "not deleted", distinct from a non-nil
pointer to any instant — you never want a `NULL deleted_at` to read as the epoch
and mark a live row deleted.

`ScanUser` takes a `Row` interface (`Scan(dest ...any) error`) — the shape both
`*sql.Row` and `*sql.Rows` satisfy — so it works against a real query in
production and against the in-memory `fakeRow` in tests. `fakeRow` implements the
same `Scan` contract by assigning stored values into the caller's typed
destinations, which is exactly how a driver's `Scan` populates
`*sql.NullString`/`*sql.Null[time.Time]`. Scan errors are wrapped with `%w` so
callers can match them with `errors.Is`.

Create `repo.go`:

```go
package repo

import (
	"database/sql"
	"fmt"
	"time"
)

// User is the domain entity. Nullable columns map to pointers: nil means the
// column was NULL, non-nil means a present value (including a zero value).
type User struct {
	ID        int64
	Nickname  *string
	DeletedAt *time.Time
}

// Row is the scan contract satisfied by *sql.Row and *sql.Rows.
type Row interface {
	Scan(dest ...any) error
}

// ScanUser scans one row into a domain User, mapping nullable columns through
// sql.Null targets so NULL stays distinct from the zero value.
func ScanUser(row Row) (User, error) {
	var (
		id       int64
		nickname sql.NullString
		deleted  sql.Null[time.Time]
	)
	if err := row.Scan(&id, &nickname, &deleted); err != nil {
		return User{}, fmt.Errorf("scan user: %w", err)
	}

	u := User{ID: id}
	if nickname.Valid {
		v := nickname.String
		u.Nickname = &v
	}
	if deleted.Valid {
		v := deleted.V
		u.DeletedAt = &v
	}
	return u, nil
}

// fakeRow is an in-memory Row for demos and tests. It assigns its stored values
// into the typed destinations, mimicking a driver's Scan.
type fakeRow struct {
	id       int64
	nickname sql.NullString
	deleted  sql.Null[time.Time]
	err      error
}

func (r fakeRow) Scan(dest ...any) error {
	if r.err != nil {
		return r.err
	}
	if len(dest) != 3 {
		return fmt.Errorf("fakeRow: expected 3 destinations, got %d", len(dest))
	}
	if p, ok := dest[0].(*int64); ok {
		*p = r.id
	} else {
		return fmt.Errorf("fakeRow: dest[0] is %T, want *int64", dest[0])
	}
	if p, ok := dest[1].(*sql.NullString); ok {
		*p = r.nickname
	} else {
		return fmt.Errorf("fakeRow: dest[1] is %T, want *sql.NullString", dest[1])
	}
	if p, ok := dest[2].(*sql.Null[time.Time]); ok {
		*p = r.deleted
	} else {
		return fmt.Errorf("fakeRow: dest[2] is %T, want *sql.Null[time.Time]", dest[2])
	}
	return nil
}
```

## The runnable demo

The demo scans three rows: one with a present nickname and a deletion time, one
with `NULL` in both nullable columns, and one whose nickname is a genuinely
present empty string. It prints how each maps to nil vs non-nil pointers.

Create `cmd/demo/main.go`:

```go
package main

import (
	"database/sql"
	"fmt"
	"time"

	"example.com/repo"
)

func show(label string, u repo.User) {
	nick := "<nil>"
	if u.Nickname != nil {
		nick = fmt.Sprintf("%q", *u.Nickname)
	}
	del := "<nil>"
	if u.DeletedAt != nil {
		del = u.DeletedAt.Format("2006-01-02")
	}
	fmt.Printf("%s: id=%d nickname=%s deleted=%s\n", label, u.ID, nick, del)
}

func main() {
	when := time.Date(2026, 3, 1, 0, 0, 0, 0, time.UTC)

	present := repo.NewFakeRow(1,
		sql.NullString{String: "ada", Valid: true},
		sql.Null[time.Time]{V: when, Valid: true}, nil)
	nulls := repo.NewFakeRow(2,
		sql.NullString{Valid: false},
		sql.Null[time.Time]{Valid: false}, nil)
	emptyPresent := repo.NewFakeRow(3,
		sql.NullString{String: "", Valid: true},
		sql.Null[time.Time]{Valid: false}, nil)

	for _, r := range []repo.Row{present, nulls, emptyPresent} {
		u, err := repo.ScanUser(r)
		if err != nil {
			fmt.Println("error:", err)
			continue
		}
		show(fmt.Sprintf("user-%d", u.ID), u)
	}
}
```

The demo needs an exported constructor for `fakeRow`. Append to `repo.go`:

```go
// NewFakeRow builds a Row backed by fixed values, for demos and tests.
func NewFakeRow(id int64, nickname sql.NullString, deleted sql.Null[time.Time], err error) Row {
	return fakeRow{id: id, nickname: nickname, deleted: deleted, err: err}
}
```

Run it:

```bash
go run ./cmd/demo
```

Expected output:

```
user-1: id=1 nickname="ada" deleted=2026-03-01
user-2: id=2 nickname=<nil> deleted=<nil>
user-3: id=3 nickname="" deleted=<nil>
```

Note `user-2` (NULL nickname) yields `<nil>` while `user-3` (present empty
string) yields `""` — the distinction the whole exercise exists to protect.

## Tests

`TestScanUser` is a table asserting the nil-vs-present mapping: a NULL nickname
becomes a nil pointer, a present nickname (including an empty string) becomes a
non-nil pointer to the exact value, and NULL/present `deleted_at` map the same
way. `TestScanErrorWrapped` proves a scan failure is wrapped so `errors.Is`
matches the underlying sentinel.

Create `repo_test.go`:

```go
package repo

import (
	"database/sql"
	"errors"
	"testing"
	"time"
)

func strp(s string) *string { return &s }

func TestScanUser(t *testing.T) {
	t.Parallel()

	when := time.Date(2026, 3, 1, 0, 0, 0, 0, time.UTC)

	tests := []struct {
		name         string
		row          fakeRow
		wantNickname *string
		wantDeleted  bool // whether DeletedAt should be non-nil
	}{
		{
			name:         "present values",
			row:          fakeRow{id: 1, nickname: sql.NullString{String: "ada", Valid: true}, deleted: sql.Null[time.Time]{V: when, Valid: true}},
			wantNickname: strp("ada"),
			wantDeleted:  true,
		},
		{
			name:         "null nickname is nil pointer",
			row:          fakeRow{id: 2, nickname: sql.NullString{Valid: false}, deleted: sql.Null[time.Time]{Valid: false}},
			wantNickname: nil,
			wantDeleted:  false,
		},
		{
			name:         "present empty string is non-nil pointer",
			row:          fakeRow{id: 3, nickname: sql.NullString{String: "", Valid: true}, deleted: sql.Null[time.Time]{Valid: false}},
			wantNickname: strp(""),
			wantDeleted:  false,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			u, err := ScanUser(tt.row)
			if err != nil {
				t.Fatalf("ScanUser: %v", err)
			}
			switch {
			case tt.wantNickname == nil && u.Nickname != nil:
				t.Fatalf("Nickname = %q, want nil", *u.Nickname)
			case tt.wantNickname != nil && u.Nickname == nil:
				t.Fatalf("Nickname = nil, want %q", *tt.wantNickname)
			case tt.wantNickname != nil && *u.Nickname != *tt.wantNickname:
				t.Fatalf("Nickname = %q, want %q", *u.Nickname, *tt.wantNickname)
			}
			if gotDeleted := u.DeletedAt != nil; gotDeleted != tt.wantDeleted {
				t.Fatalf("DeletedAt non-nil = %v, want %v", gotDeleted, tt.wantDeleted)
			}
		})
	}
}

func TestScanErrorWrapped(t *testing.T) {
	t.Parallel()

	sentinel := errors.New("connection reset")
	_, err := ScanUser(fakeRow{err: sentinel})
	if !errors.Is(err, sentinel) {
		t.Fatalf("error = %v, want wrapped %v", err, sentinel)
	}
}
```

## Review

The scan layer is correct when `NULL` and a present zero value produce different
domain values: a NULL nickname must be a nil `*string` and a present `""` must be
a non-nil pointer to `""`. The table's third case is the one that catches the
bug — if you scanned straight into a `string`, both would read as `""` and the
test could not tell them apart. Keep the `Valid` check between the scan target
and the domain field; that is the only place the distinction is preserved. Wrap
scan errors with `%w` so a caller can classify them with `errors.Is` rather than
string-matching. This module builds offline because it uses only `database/sql`
value types — a real repository would additionally open a `*sql.DB` and pass
`rows` where the demo passes `fakeRow`.

## Resources

- [`database/sql.Null`](https://pkg.go.dev/database/sql#Null) — the generic `Null[T]{V, Valid}` scan target.
- [`database/sql.NullString`](https://pkg.go.dev/database/sql#NullString) — the concrete nullable-string form.
- [`database/sql.Rows.Scan`](https://pkg.go.dev/database/sql#Rows.Scan) — the `Scan(dest ...any)` contract the `Row` interface mirrors.

---

Back to [00-concepts.md](00-concepts.md) | Prev: [09-comparable-struct-cache-key.md](09-comparable-struct-cache-key.md) | Next: [../03-basic-types/00-concepts.md](../03-basic-types/00-concepts.md)
