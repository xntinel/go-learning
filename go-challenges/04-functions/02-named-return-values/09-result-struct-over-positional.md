# Exercise 9: Replace 3+ Positional Returns with a Self-Describing Struct

Named returns stop earning their keep the moment you have three or more non-error
values: the names document nothing the body does not already say, and callers still
unpack positionally, where a swapped pair is a silent data corruption. The senior
move is to return a struct. This exercise refactors a `(id, name, email string, err
error)` lookup into `LookupUser(...) (UserRecord, error)` and shows why the struct
form makes a field swap a compile error.

This module is self-contained: its own `go mod init`, its own demo, its own tests.

## What you'll build

```text
directory/                  independent module: example.com/directory
  go.mod
  directory.go              UserRecord; Directory.LookupUser (struct return)
  cmd/demo/
    main.go                 runnable demo: look up a user, print fields by name
  directory_test.go         field-by-field assertions, not-found sentinel
```

- Files: `directory.go`, `cmd/demo/main.go`, `directory_test.go`.
- Implement: `LookupUser(id) (UserRecord, error)` returning a self-describing struct instead of three positional strings.
- Test: field-by-field assertions on `UserRecord`, and a not-found case asserted with `errors.Is`; the prose contrasts the positional form that let a caller swap name/email undetected.
- Verify: `go test -count=1 -race ./...`

Set up the module:

```bash
mkdir -p go-solutions/04-functions/02-named-return-values/09-result-struct-over-positional/cmd/demo
cd go-solutions/04-functions/02-named-return-values/09-result-struct-over-positional
```

### Why a struct beats four positional returns

Here is the shape to avoid. It compiles and it is a trap:

```go
// The positional form: three strings a caller must order correctly.
func lookupOld(id string) (uid, name, email string, err error) { ... }

// A caller, refactoring under time pressure, writes this and nobody notices:
_, email, name, _ := lookupOld("42") // name and email SWAPPED, compiles fine
```

Because all three results are `string`, the compiler cannot tell that the caller
bound `email` to the name column and `name` to the email column. The program keeps
running and quietly emails the wrong field or renders the wrong label. Naming the
returns `(uid, name, email string, err error)` does not help: the caller's local
names are independent of the callee's, so the swap is invisible at the call site.

The struct form removes the hazard by construction:

```go
func (d *Directory) LookupUser(id string) (UserRecord, error) { ... }

rec, err := d.LookupUser("42")
// rec.Name and rec.Email are addressed by field name; you cannot swap them
// without writing the wrong field name, which changes behavior visibly.
```

There is no positional unpacking, so there is nothing to mis-order. Adding a field
later (a `Department`) is a non-breaking change to a struct, whereas adding a fourth
string to a positional return breaks every call site. The guideline that falls out:
one value, return it; a value plus an error, return the pair; three or more values,
return a struct — and reserve named returns for the defer-coupled patterns in the
other modules, where they do real work.

Create `directory.go`:

```go
package directory

import (
	"errors"
	"fmt"
)

// ErrNotFound is returned (wrapped) when no user has the given id.
var ErrNotFound = errors.New("user not found")

// UserRecord is a self-describing result: callers address fields by name, so a
// field cannot be silently swapped the way positional returns can.
type UserRecord struct {
	ID    string
	Name  string
	Email string
}

// Directory looks up users by id.
type Directory struct {
	byID map[string]UserRecord
}

// New builds a Directory from a set of records.
func New(records ...UserRecord) *Directory {
	m := make(map[string]UserRecord, len(records))
	for _, r := range records {
		m[r.ID] = r
	}
	return &Directory{byID: m}
}

// LookupUser returns the record for an id, or a wrapped ErrNotFound. It returns a
// struct rather than (id, name, email string, err error) so callers use field
// names instead of fragile positional unpacking.
func (d *Directory) LookupUser(id string) (UserRecord, error) {
	rec, ok := d.byID[id]
	if !ok {
		return UserRecord{}, fmt.Errorf("lookup %q: %w", id, ErrNotFound)
	}
	return rec, nil
}
```

### The runnable demo

Create `cmd/demo/main.go`:

```go
package main

import (
	"errors"
	"fmt"

	"example.com/directory"
)

func main() {
	dir := directory.New(
		directory.UserRecord{ID: "42", Name: "Ada Lovelace", Email: "ada@example.com"},
	)

	rec, err := dir.LookupUser("42")
	if err != nil {
		fmt.Println("error:", err)
		return
	}
	fmt.Printf("name=%s email=%s\n", rec.Name, rec.Email)

	_, err = dir.LookupUser("99")
	fmt.Println("missing:", err)
	fmt.Println("is not-found:", errors.Is(err, directory.ErrNotFound))
}
```

Run it:

```bash
go run ./cmd/demo
```

Expected output:

```
name=Ada Lovelace email=ada@example.com
missing: lookup "99": user not found
is not-found: true
```

### Tests

The tests assert field-by-field on the returned struct — which is only possible
*because* the fields are named — and cover the not-found path with `errors.Is`.

Create `directory_test.go`:

```go
package directory

import (
	"errors"
	"fmt"
	"testing"
)

func TestLookupUserFound(t *testing.T) {
	t.Parallel()

	dir := New(UserRecord{ID: "42", Name: "Ada", Email: "ada@example.com"})
	rec, err := dir.LookupUser("42")
	if err != nil {
		t.Fatalf("LookupUser: unexpected error: %v", err)
	}
	if rec.ID != "42" {
		t.Fatalf("ID = %q, want 42", rec.ID)
	}
	if rec.Name != "Ada" {
		t.Fatalf("Name = %q, want Ada", rec.Name)
	}
	if rec.Email != "ada@example.com" {
		t.Fatalf("Email = %q, want ada@example.com", rec.Email)
	}
}

func TestLookupUserNotFound(t *testing.T) {
	t.Parallel()

	dir := New(UserRecord{ID: "42", Name: "Ada"})
	_, err := dir.LookupUser("99")
	if !errors.Is(err, ErrNotFound) {
		t.Fatalf("LookupUser err = %v, want ErrNotFound", err)
	}
}

func ExampleDirectory_LookupUser() {
	dir := New(UserRecord{ID: "42", Name: "Ada", Email: "ada@example.com"})
	rec, _ := dir.LookupUser("42")
	fmt.Printf("%s <%s>\n", rec.Name, rec.Email)
	// Output: Ada <ada@example.com>
}
```

## Review

The lookup is correct when a found id yields a struct whose fields match by name and
a missing id yields a wrapped `ErrNotFound`. The point of the exercise is structural,
not behavioral: the struct return makes a field mis-order impossible, because callers
address `rec.Name` and `rec.Email` by name rather than by position. Field-by-field
test assertions are the tell that the shape is right — you could not write them
against a `(string, string, string, error)` return without re-introducing the same
positional fragility in the test. Reserve named returns for the defer-coupled
patterns; for a wide result, reach for a struct. Run `go test -race`.

## Resources

- [Go Spec: Struct types](https://go.dev/ref/spec#Struct_types)
- [Go Code Review Comments: Named Result Parameters](https://go.dev/wiki/CodeReviewComments#named-result-parameters)
- [Effective Go: Named result parameters](https://go.dev/doc/effective_go#named-results)

---

Back to [00-concepts.md](00-concepts.md) | Previous: [08-error-returning-handler-adapter.md](08-error-returning-handler-adapter.md) | Next: [10-acquire-all-or-none-cleanup.md](10-acquire-all-or-none-cleanup.md)
