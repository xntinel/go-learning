# Exercise 9: Scanning Nullable DB Columns Into Pointers and sql.Null[T]

A NULL database column is the archetypal "absent, not zero": a NULL `email` means
unknown, not the empty string. This module builds a row-mapper that turns
nullable columns into pointer fields on the domain struct — using `sql.NullString`
for one and the generic `sql.Null[T]` (Go 1.22+) for another — and round-trips
the semantic both directions, NULL becoming nil and back.

This module is fully self-contained.

## What you'll build

```text
nullscan/                 independent module: example.com/nullscan
  go.mod                  go 1.24
  nullscan.go             type User (pointer fields); mapRow; toNullString/toNullInt (Value direction)
  cmd/
    demo/
      main.go             runnable demo: NULL -> absent, value -> set
  nullscan_test.go        NullString/Null[int64] mapping both directions
```

Files: `nullscan.go`, `cmd/demo/main.go`, `nullscan_test.go`.
Implement: a `User` with `*string`/`*int64` optional fields; `mapRow` that turns `sql.NullString`/`sql.Null[int64]` into nil-or-set pointer fields; helpers turning the pointer fields back into `sql.NullString`/`sql.Null[int64]` for writes.
Test: `NullString{Valid:false}` maps to a nil field; `Valid:true` maps the value through; same for `sql.Null[int64]`; the `Value()` direction turns a nil field into a NULL driver value.
Verify: `go test -count=1 -race ./...`

Set up the module:

```bash
mkdir -p go-solutions/09-pointers/04-nil-pointers-and-guard-checks/09-sql-null-scan-nullable-columns/cmd/demo
cd go-solutions/09-pointers/04-nil-pointers-and-guard-checks/09-sql-null-scan-nullable-columns
go mod edit -go=1.24
```

### NULL is absent, not zero

`database/sql` cannot scan a NULL into a `string` — there is no string value to
produce, and a zero `""` would erase the distinction between "no email on file"
and "email is the empty string". The `sql.Null*` types carry the extra bit: a
`sql.NullString` is `{ String string; Valid bool }`, and `Valid == false` means
the column was NULL. `sql.Null[T]` (Go 1.22+) generalizes this to any type:
`sql.Null[int64]` is `{ V int64; Valid bool }`. Both implement `sql.Scanner`
(NULL sets `Valid=false` on scan) and `driver.Valuer` (an invalid value produces
a NULL on write), so NULL round-trips with no sentinel like `-1` or `""`.

The domain type should not carry `sql.Null*` fields — those are a persistence
concern. The mapper translates once at the boundary: a NULL column becomes a nil
pointer on the domain struct, a non-NULL column becomes a pointer to the value:

```go
func mapRow(name string, email sql.NullString, age sql.Null[int64]) User {
	u := User{Name: name}
	if email.Valid {
		v := email.String
		u.Email = &v
	}
	if age.Valid {
		v := age.V
		u.Age = &v
	}
	return u
}
```

Taking the address of a fresh local `v` (not of `email.String` directly) gives
each pointer its own backing storage, so a later reuse of the scan target cannot
alias the stored value. The write direction is the mirror: a nil domain field
becomes an invalid (`Valid:false`) `sql.Null*`, which the driver writes as NULL.

Create `nullscan.go`:

```go
package nullscan

import "database/sql"

// User is the domain type. Email and Age are optional: a nil pointer means the
// corresponding column was NULL (absent), not empty/zero.
type User struct {
	Name  string
	Email *string
	Age   *int64
}

// mapRow translates a scanned row into the domain type: a NULL column
// (Valid=false) becomes a nil pointer, a present column becomes a set pointer.
func mapRow(name string, email sql.NullString, age sql.Null[int64]) User {
	u := User{Name: name}
	if email.Valid {
		v := email.String
		u.Email = &v
	}
	if age.Valid {
		v := age.V
		u.Age = &v
	}
	return u
}

// toNullString turns an optional domain field into a sql.NullString for writing:
// nil becomes NULL (Valid=false), a set pointer becomes the value.
func toNullString(s *string) sql.NullString {
	if s == nil {
		return sql.NullString{}
	}
	return sql.NullString{String: *s, Valid: true}
}

// toNullInt turns an optional domain field into a sql.Null[int64] for writing.
func toNullInt(n *int64) sql.Null[int64] {
	if n == nil {
		return sql.Null[int64]{}
	}
	return sql.Null[int64]{V: *n, Valid: true}
}
```

### The runnable demo

The demo maps two rows — one with NULLs, one fully populated — and prints how the
domain pointers reflect absence versus presence.

Create `cmd/demo/main.go`:

```go
package main

import (
	"database/sql"
	"fmt"

	"example.com/nullscan"
)

func main() {
	// A row with NULL email and NULL age.
	nullRow := nullscan.MapRow("alice", sql.NullString{}, sql.Null[int64]{})
	fmt.Printf("%s: email set=%v age set=%v\n",
		nullRow.Name, nullRow.Email != nil, nullRow.Age != nil)

	// A fully-populated row.
	fullRow := nullscan.MapRow("bob",
		sql.NullString{String: "bob@example.com", Valid: true},
		sql.Null[int64]{V: 41, Valid: true})
	fmt.Printf("%s: email=%s age=%d\n", fullRow.Name, *fullRow.Email, *fullRow.Age)
}
```

The demo needs an exported entry point. Add one:

Append to `nullscan.go`:

```go
// MapRow is the exported wrapper around mapRow for use from other packages.
func MapRow(name string, email sql.NullString, age sql.Null[int64]) User {
	return mapRow(name, email, age)
}
```

Run it:

```bash
go run ./cmd/demo
```

Expected output:

```
alice: email set=false age set=false
bob: email=bob@example.com age=41
```

### Tests

The tests exercise the mapping both directions without a live database: feed
`Valid:false` and assert the domain field is nil; feed a value and assert it maps
through; then check the write direction turns a nil field into a NULL driver
value (`Value()` returns `nil, nil` for an invalid `sql.Null*`).

Create `nullscan_test.go`:

```go
package nullscan

import (
	"database/sql"
	"testing"
)

func TestMapRowNullBecomesNilField(t *testing.T) {
	t.Parallel()

	u := mapRow("alice", sql.NullString{}, sql.Null[int64]{})
	if u.Email != nil {
		t.Fatalf("Email = %v, want nil for NULL column", *u.Email)
	}
	if u.Age != nil {
		t.Fatalf("Age = %v, want nil for NULL column", *u.Age)
	}
	if u.Name != "alice" {
		t.Fatalf("Name = %q, want alice", u.Name)
	}
}

func TestMapRowValueMapsThrough(t *testing.T) {
	t.Parallel()

	u := mapRow("bob",
		sql.NullString{String: "bob@example.com", Valid: true},
		sql.Null[int64]{V: 41, Valid: true})

	if u.Email == nil || *u.Email != "bob@example.com" {
		t.Fatalf("Email = %v, want bob@example.com", u.Email)
	}
	if u.Age == nil || *u.Age != 41 {
		t.Fatalf("Age = %v, want 41", u.Age)
	}
}

func TestToNullStringNilBecomesNullValue(t *testing.T) {
	t.Parallel()

	ns := toNullString(nil)
	if ns.Valid {
		t.Fatal("nil field produced Valid=true, want NULL")
	}
	// driver.Valuer: an invalid NullString writes as a NULL (nil driver value).
	v, err := ns.Value()
	if err != nil {
		t.Fatal(err)
	}
	if v != nil {
		t.Fatalf("Value() = %v, want nil (NULL) for a nil field", v)
	}
}

func TestToNullStringValueRoundTrips(t *testing.T) {
	t.Parallel()

	s := "carol@example.com"
	ns := toNullString(&s)
	if !ns.Valid || ns.String != s {
		t.Fatalf("NullString = %+v, want {%q true}", ns, s)
	}
	v, err := ns.Value()
	if err != nil {
		t.Fatal(err)
	}
	if got, ok := v.(string); !ok || got != s {
		t.Fatalf("Value() = %v, want %q", v, s)
	}
}

func TestToNullIntNilBecomesNullValue(t *testing.T) {
	t.Parallel()

	n := toNullInt(nil)
	if n.Valid {
		t.Fatal("nil field produced Valid=true, want NULL")
	}
	v, err := n.Value()
	if err != nil {
		t.Fatal(err)
	}
	if v != nil {
		t.Fatalf("Value() = %v, want nil (NULL) for a nil field", v)
	}
}
```

## Review

The mapper is correct when NULL (`Valid:false`) becomes a nil domain pointer and
a present column becomes a set pointer, in both directions. `TestMapRowNullBecomesNilField`
and `TestMapRowValueMapsThrough` pin the read direction;
`TestToNullStringNilBecomesNullValue` and `TestToNullIntNilBecomesNullValue` pin
that a nil field writes back as a NULL driver value, closing the round-trip. The
tests need no live database because the mapping is a pure function of the scanned
`sql.Null*` values.

The mistake avoided: scanning nullable columns into plain `string`/`int64`
fields, which either fails to scan a NULL or silently turns NULL into a zero,
collapsing "unknown" into "empty".

## Resources

- [database/sql: NullString](https://pkg.go.dev/database/sql#NullString) — the `{String, Valid}` type and its Scanner/Valuer behavior.
- [database/sql: Null[T]](https://pkg.go.dev/database/sql#Null) — the generic nullable wrapper added in Go 1.22.
- [database/sql/driver: Valuer](https://pkg.go.dev/database/sql/driver#Valuer) — how a value converts to a driver value, with nil meaning NULL.

---

Back to [00-concepts.md](00-concepts.md) | Prev: [08-nested-pointer-chain-guard.md](08-nested-pointer-chain-guard.md) | Next: [../05-pointers-to-structs/00-concepts.md](../05-pointers-to-structs/00-concepts.md)
