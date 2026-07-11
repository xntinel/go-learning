# Exercise 10: Build a Tag-Driven Column Mapper by Parsing reflect.StructTag

`encoding/json` is not magic: it reflects over your struct and reads the `json`
tag. Database mappers like `sqlx` do the identical thing with a `db` tag. This
module builds that mechanism from the ground up — a mapper that reflects over a
struct, reads a `db:"column,opts"` tag via `reflect.StructTag`, honors `db:"-"` to
skip, falls back to the field name, and separates the column name from its options.
Once you have written it, every struct-tag-driven library stops being opaque.

This module is fully self-contained: its own `go mod init`, demo, and tests.

## What you'll build

```text
colmap/                        independent module: example.com/colmap
  go.mod                       go 1.24
  dbmap/
    dbmap.go                   Column, Columns(any) []Column, ColumnNames(any) []string
  cmd/
    demo/
      main.go                  print the mapped columns of a Row struct
  dbmap/dbmap_test.go          names+fallback+skip, absent-vs-empty Lookup, options split
```

Files: `dbmap/dbmap.go`, `cmd/demo/main.go`, `dbmap/dbmap_test.go`.
Implement: `Columns(any) []Column` reflecting over fields, reading `db` via `StructTag.Lookup`, skipping `db:"-"` and unexported fields, falling back to the field name, and splitting name from options with `strings.Cut`.
Test: the column-name slice matches expected (custom, fallback, skipped excluded); `Lookup` distinguishes absent from empty; unexported fields are skipped; `db:"created,readonly"` parses name and options separately.
Verify: `go test -count=1 -race ./...`

Set up the module:

```bash
mkdir -p ~/go-exercises/colmap/dbmap ~/go-exercises/colmap/cmd/demo
cd ~/go-exercises/colmap
go mod init example.com/colmap
go mod edit -go=1.24
```

## The struct-tag grammar, read the right way

A struct tag is not a free-form string; it is a well-defined space-separated list
of `key:"value"` pairs, and `reflect.StructTag` gives you the parser for the outer
layer. `tag.Get("db")` returns the `db` value or `""`; `tag.Lookup("db")` returns
`(value, ok)` and is what you actually want, because it separates two cases a naive
`Get` conflates: a field with **no** `db` tag (`ok == false`, so fall back to the
field name) and a field with an **empty** `db` tag `db:""` (`ok == true, value ==
""`, a deliberate "use the default but the tag is present" signal that some
libraries treat specially). Getting this distinction right is why you never
hand-split the raw tag string with ad-hoc code.

The *inner* grammar of the value — a name followed by comma-separated options,
`db:"created,readonly"` — is defined by your library, not by `reflect`. So you own
that split, and `strings.Cut(value, ",")` is the clean way to do it: it returns the
part before the first comma (the column name) and the part after (the option list),
plus a bool for whether a comma was present. Split the option list on further commas
only if there is one.

The reflection walk itself has two rules that mirror `encoding/json`. First, only
exported fields are mappable — `reflect.StructField.IsExported()` filters out
unexported fields, exactly as `json` ignores them, because reflection cannot read
an unexported field's value anyway. Second, `db:"-"` means "skip this column", the
same sentinel `json:"-"` uses. Everything else becomes a `Column` with its resolved
name and parsed options.

Accepting `any` and handling a pointer (`reflect.Pointer` -> `Elem()`) makes the
mapper callable as `Columns(Row{})` or `Columns(&Row{})`, matching how real mappers
are invoked.

Create `dbmap/dbmap.go`:

```go
// dbmap/dbmap.go
package dbmap

import (
	"reflect"
	"strings"
)

// Column is a mapped database column: its resolved name and any tag options.
type Column struct {
	Name    string
	Options []string
}

// Columns reflects over a struct (or pointer to one) and returns its mapped
// columns, reading the db tag. Fields tagged db:"-" and unexported fields are
// skipped; an absent or empty tag falls back to the field name.
func Columns(v any) []Column {
	t := reflect.TypeOf(v)
	if t.Kind() == reflect.Pointer {
		t = t.Elem()
	}

	var cols []Column
	for i := range t.NumField() {
		f := t.Field(i)
		if !f.IsExported() {
			continue
		}

		tag, ok := f.Tag.Lookup("db")
		if ok && tag == "-" {
			continue
		}

		name := f.Name // fallback: absent tag, empty tag, or empty name part
		var options []string
		if ok && tag != "" {
			namePart, optPart, hasOpts := strings.Cut(tag, ",")
			if namePart != "" {
				name = namePart
			}
			if hasOpts && optPart != "" {
				options = strings.Split(optPart, ",")
			}
		}
		cols = append(cols, Column{Name: name, Options: options})
	}
	return cols
}

// ColumnNames returns just the resolved column names, in field order.
func ColumnNames(v any) []string {
	cols := Columns(v)
	names := make([]string, len(cols))
	for i, c := range cols {
		names[i] = c.Name
	}
	return names
}
```

## The runnable demo

The demo maps a `Row` struct that exercises every case: custom names, a field with
no tag (fallback), a field with a name plus a `readonly` option, a skipped column,
and an unexported field.

Create `cmd/demo/main.go`:

```go
// cmd/demo/main.go
package main

import (
	"fmt"

	"example.com/colmap/dbmap"
)

type Row struct {
	ID        int    `db:"id"`
	Email     string `db:"email"`
	FullName  string // no db tag: falls back to field name
	CreatedAt string `db:"created,readonly"`
	Ignored   string `db:"-"`
	checksum  string // unexported: skipped
}

func main() {
	for _, c := range dbmap.Columns(Row{}) {
		if len(c.Options) > 0 {
			fmt.Printf("%s %v\n", c.Name, c.Options)
		} else {
			fmt.Println(c.Name)
		}
	}
}
```

Run it:

```bash
go run ./cmd/demo
```

Expected output:

```
id
email
FullName
created [readonly]
```

`Ignored` (`db:"-"`) and `checksum` (unexported) never appear.

## Tests

`TestColumnNames` asserts the resolved-name slice, proving custom names, field-name
fallback, and the exclusion of skipped and unexported fields. `TestLookupAbsentVsEmpty`
asserts `reflect.StructTag.Lookup` returns `ok == false` for an absent tag and
`ok == true, value == ""` for an empty one. `TestOptionsParsed` asserts
`db:"created,readonly"` splits into name and options. `TestSkipsUnexported` asserts
an unexported field never becomes a column.

Create `dbmap/dbmap_test.go`:

```go
// dbmap/dbmap_test.go
package dbmap

import (
	"fmt"
	"reflect"
	"slices"
	"testing"
)

type row struct {
	ID       int    `db:"id"`
	Email    string `db:"email"`
	FullName string
	Created  string `db:"created,readonly"`
	Ignored  string `db:"-"`
	checksum string
}

func TestColumnNames(t *testing.T) {
	t.Parallel()
	got := ColumnNames(row{})
	want := []string{"id", "email", "FullName", "created"}
	if !slices.Equal(got, want) {
		t.Fatalf("ColumnNames = %v, want %v", got, want)
	}
}

func TestColumnNamesPointer(t *testing.T) {
	t.Parallel()
	got := ColumnNames(&row{})
	want := []string{"id", "email", "FullName", "created"}
	if !slices.Equal(got, want) {
		t.Fatalf("ColumnNames(&row) = %v, want %v", got, want)
	}
}

func TestLookupAbsentVsEmpty(t *testing.T) {
	t.Parallel()
	type tagged struct {
		Empty  string `db:""`
		Absent string
	}
	rt := reflect.TypeOf(tagged{})

	if _, ok := rt.Field(0).Tag.Lookup("db"); !ok {
		t.Fatal("empty tag db:\"\" should Lookup ok == true")
	}
	if _, ok := rt.Field(1).Tag.Lookup("db"); ok {
		t.Fatal("absent tag should Lookup ok == false")
	}
}

func TestOptionsParsed(t *testing.T) {
	t.Parallel()
	var created Column
	for _, c := range Columns(row{}) {
		if c.Name == "created" {
			created = c
		}
	}
	if created.Name != "created" {
		t.Fatalf("did not find created column")
	}
	if !slices.Equal(created.Options, []string{"readonly"}) {
		t.Fatalf("options = %v, want [readonly]", created.Options)
	}
}

func TestSkipsUnexported(t *testing.T) {
	t.Parallel()
	for _, name := range ColumnNames(row{}) {
		if name == "checksum" {
			t.Fatal("unexported field must not be mapped")
		}
	}
}

func ExampleColumnNames() {
	fmt.Println(ColumnNames(row{}))
	// Output: [id email FullName created]
}
```

## Review

The mapper is correct when the resolved names match the expected slice — custom
tags win, an untagged field falls back to its name, and `db:"-"` and unexported
fields are absent — and when `db:"created,readonly"` yields a `created` column with
a `readonly` option. The two things worth internalizing are that `Lookup` is the
right tool because it distinguishes absent from empty (a distinction `Get` throws
away), and that the outer `key:"value"` parse belongs to `reflect.StructTag` while
the inner `name,opt,opt` parse belongs to you via `strings.Cut`. This is exactly
how `encoding/json`, `sqlx`, and every validator you use read their tags; the
mechanism is small, well-defined, and no longer a black box.

## Resources

- [`reflect.StructTag`](https://pkg.go.dev/reflect#StructTag) — the `Get`/`Lookup` methods and the tag grammar.
- [`reflect.StructField`](https://pkg.go.dev/reflect#StructField) — `Tag`, `IsExported`, and iterating fields with `NumField`/`Field`.
- [`strings.Cut`](https://pkg.go.dev/strings#Cut) — splitting a tag value into name and options at the first comma.

---

Back to [00-concepts.md](00-concepts.md) | Previous: [09-number-precision-usenumber.md](09-number-precision-usenumber.md) | Next: [../03-methods-value-vs-pointer-receivers/00-concepts.md](../03-methods-value-vs-pointer-receivers/00-concepts.md)
