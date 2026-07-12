# Exercise 18: Schema Migration Compatibility Validator

**Nivel: Intermedio** — validacion rapida (un test corto).

Before a migration runs, a tool has to answer two very different
questions: "would this migration break existing readers" and "could I even
check that, or is the schema registry down". Collapsing both into one
`error` return makes an operational hiccup look like a real incompatibility
(and vice versa). This exercise builds
`ValidateMigration(currentName, targetName) (compatible bool, reason
string, error)`, where `compatible`/`reason` are the business verdict and
`error` is reserved for the registry actually failing to answer.

This module is fully self-contained: its own `go mod init`, all code
inline, one quick test file.

## What you'll build

```text
migrationcheck/              independent module: example.com/schema-migration-compatibility-check
  go.mod                     go 1.24
  migrationcheck.go          package migrationcheck; Schema, SchemaStore, CheckCompatible, ValidateMigration(current,target) (compatible,reason,error)
  cmd/
    demo/
      main.go                additive migration, a dropped column, and a forced registry failure
  migrationcheck_test.go     table over compatible/incompatible outcomes; a store failure test; an unknown-name test
```

- Files: `migrationcheck.go`, `cmd/demo/main.go`, `migrationcheck_test.go`.
- Implement: `CheckCompatible(current, target Schema) (compatible bool, reason string)` (dropping a column or changing its type is incompatible; adding columns is fine), wrapped by `(*SchemaStore).ValidateMigration(currentName, targetName string) (compatible bool, reason string, err error)` which loads both schemas first.
- Test: an additive migration is compatible; a dropped column and a retyped column each report `compatible == false` with the right reason; a registry failure returns a non-nil error with `compatible == false` and an empty reason.
- Verify: `go test -count=1 ./...`

Set up the module:

```bash
go mod edit -go=1.24
```

### Three outcomes, not two

A naive signature `ValidateMigration(current, target) (bool, error)` forces
an awkward choice: does a dropped column return `(false, nil)` or
`(false, someErr)`? Neither is right on its own. `(false, nil)` throws away
*why* it is incompatible — the migration tool can only say "no", not "no,
because column `email` would be dropped". `(false, someErr)` is worse: it
makes a perfectly ordinary business fact (this migration is not backward
compatible) look identical to the registry being unreachable, so a caller
matching on `err != nil` to decide "should I retry" ends up retrying a
migration that will never become compatible no matter how many times it
asks.

The fix is the same shape this lesson has been building all along —
business outcome and operational failure travel on separate returns:

```go
func (s *SchemaStore) ValidateMigration(currentName, targetName string) (compatible bool, reason string, err error) {
	current, err := s.load(currentName)
	if err != nil {
		return false, "", err // operational: couldn't even compare
	}
	target, err := s.load(targetName)
	if err != nil {
		return false, "", err
	}
	compatible, reason = CheckCompatible(current, target)
	return compatible, reason, nil // business verdict: nil error either way
}
```

`err` is nil on both a compatible and an incompatible verdict — the load
succeeded, the comparison ran, the answer is just "no" with a reason.

Create `migrationcheck.go`:

```go
package migrationcheck

import (
	"errors"
	"fmt"
	"sort"
)

// ErrSchemaNotFound is the sentinel for a schema name that was never
// registered — distinct from a store-level operational failure.
var ErrSchemaNotFound = errors.New("schema not found")

// ColumnType names a column's storage type, e.g. "int", "string", "bool".
type ColumnType string

// Schema is a table's shape: its set of columns and their types.
type Schema struct {
	Name    string
	Columns map[string]ColumnType
}

// SchemaStore is an in-memory registry of named schemas, standing in for a
// migrations table or schema registry service.
type SchemaStore struct {
	schemas  map[string]Schema
	failNext error
}

func NewSchemaStore() *SchemaStore {
	return &SchemaStore{schemas: make(map[string]Schema)}
}

// Add registers a schema under its own Name.
func (s *SchemaStore) Add(schema Schema) {
	s.schemas[schema.Name] = schema
}

// FailNextWith forces the next load to fail with a wrapped copy of err,
// simulating a registry outage (unrelated to whether a name is registered).
func (s *SchemaStore) FailNextWith(err error) {
	s.failNext = err
}

func (s *SchemaStore) load(name string) (Schema, error) {
	if s.failNext != nil {
		err := s.failNext
		s.failNext = nil
		return Schema{}, fmt.Errorf("load schema %q: %w", name, err)
	}
	schema, ok := s.schemas[name]
	if !ok {
		return Schema{}, fmt.Errorf("load schema %q: %w", name, ErrSchemaNotFound)
	}
	return schema, nil
}

// CheckCompatible compares a currently-deployed schema against a migration
// target and reports whether the target is backward compatible: every
// column the current schema relies on must still exist in the target with
// the same type. Adding new columns is always compatible; dropping a
// column or changing its type is not. It never fails — both schemas are
// already-loaded values, so there is nothing left to go wrong operationally.
func CheckCompatible(current, target Schema) (compatible bool, reason string) {
	names := make([]string, 0, len(current.Columns))
	for name := range current.Columns {
		names = append(names, name)
	}
	sort.Strings(names) // deterministic order: report the first violation alphabetically

	for _, name := range names {
		wantType := current.Columns[name]
		gotType, ok := target.Columns[name]
		if !ok {
			return false, fmt.Sprintf("column %q dropped in target schema %q", name, target.Name)
		}
		if gotType != wantType {
			return false, fmt.Sprintf("column %q type changed from %s to %s", name, wantType, gotType)
		}
	}
	return true, ""
}

// ValidateMigration loads the current and target schemas by name and
// checks compatibility. Its three return values distinguish three
// outcomes a migration tool must handle differently:
//   - compatible:   (true, "", nil)
//   - incompatible: (false, human-readable reason, nil)      -> block the migration, show the reason
//   - store failure: (false, "", non-nil error)               -> retry/alert, the schemas were never even compared
func (s *SchemaStore) ValidateMigration(currentName, targetName string) (compatible bool, reason string, err error) {
	current, err := s.load(currentName)
	if err != nil {
		return false, "", err
	}
	target, err := s.load(targetName)
	if err != nil {
		return false, "", err
	}
	compatible, reason = CheckCompatible(current, target)
	return compatible, reason, nil
}
```

### The runnable demo

Create `cmd/demo/main.go`:

```go
package main

import (
	"errors"
	"fmt"

	"example.com/schema-migration-compatibility-check"
)

func main() {
	store := migrationcheck.NewSchemaStore()
	store.Add(migrationcheck.Schema{
		Name: "v1",
		Columns: map[string]migrationcheck.ColumnType{
			"id":    "int",
			"email": "string",
		},
	})
	store.Add(migrationcheck.Schema{
		Name: "v2-add-column",
		Columns: map[string]migrationcheck.ColumnType{
			"id":         "int",
			"email":      "string",
			"created_at": "string",
		},
	})
	store.Add(migrationcheck.Schema{
		Name: "v2-drop-column",
		Columns: map[string]migrationcheck.ColumnType{
			"id": "int",
		},
	})

	compatible, reason, err := store.ValidateMigration("v1", "v2-add-column")
	fmt.Printf("add column:  compatible=%t reason=%q err=%v\n", compatible, reason, err)

	compatible, reason, err = store.ValidateMigration("v1", "v2-drop-column")
	fmt.Printf("drop column: compatible=%t reason=%q err=%v\n", compatible, reason, err)

	store.FailNextWith(errors.New("registry connection refused"))
	compatible, reason, err = store.ValidateMigration("v1", "v2-add-column")
	fmt.Printf("store down:  compatible=%t reason=%q err=%v\n", compatible, reason, err)
}
```

Run it:

```bash
go run ./cmd/demo
```

Expected output:

```
add column:  compatible=true reason="" err=<nil>
drop column: compatible=false reason="column \"email\" dropped in target schema \"v2-drop-column\"" err=<nil>
store down:  compatible=false reason="" err=load schema "v1": registry connection refused
```

### Tests

Create `migrationcheck_test.go`:

```go
package migrationcheck

import (
	"errors"
	"testing"
)

func TestValidateMigration(t *testing.T) {
	store := NewSchemaStore()
	store.Add(Schema{Name: "v1", Columns: map[string]ColumnType{"id": "int", "email": "string"}})
	store.Add(Schema{Name: "v2-add", Columns: map[string]ColumnType{"id": "int", "email": "string", "created_at": "string"}})
	store.Add(Schema{Name: "v2-drop", Columns: map[string]ColumnType{"id": "int"}})
	store.Add(Schema{Name: "v2-retype", Columns: map[string]ColumnType{"id": "int", "email": "int"}})

	cases := []struct {
		name           string
		target         string
		wantCompatible bool
		wantReason     string
	}{
		{"additive migration is compatible", "v2-add", true, ""},
		{"dropped column is incompatible", "v2-drop", false, `column "email" dropped in target schema "v2-drop"`},
		{"retyped column is incompatible", "v2-retype", false, `column "email" type changed from string to int`},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			compatible, reason, err := store.ValidateMigration("v1", tc.target)
			if err != nil {
				t.Fatalf("unexpected error: %v", err)
			}
			if compatible != tc.wantCompatible {
				t.Fatalf("compatible = %t, want %t", compatible, tc.wantCompatible)
			}
			if reason != tc.wantReason {
				t.Fatalf("reason = %q, want %q", reason, tc.wantReason)
			}
		})
	}
}

func TestValidateMigrationStoreFailureIsNotIncompatibility(t *testing.T) {
	store := NewSchemaStore()
	store.Add(Schema{Name: "v1", Columns: map[string]ColumnType{"id": "int"}})
	store.Add(Schema{Name: "v2", Columns: map[string]ColumnType{"id": "int"}})
	store.FailNextWith(errors.New("connection refused"))

	compatible, reason, err := store.ValidateMigration("v1", "v2")
	if err == nil {
		t.Fatal("want a store error, got nil")
	}
	if compatible {
		t.Fatal("compatible = true on a store failure, want false")
	}
	if reason != "" {
		t.Fatalf("reason = %q on a store failure, want empty", reason)
	}
}

func TestValidateMigrationUnknownSchemaName(t *testing.T) {
	store := NewSchemaStore()
	store.Add(Schema{Name: "v1", Columns: map[string]ColumnType{"id": "int"}})

	_, _, err := store.ValidateMigration("v1", "does-not-exist")
	if !errors.Is(err, ErrSchemaNotFound) {
		t.Fatalf("err = %v, want ErrSchemaNotFound", err)
	}
}
```

## Review

`ValidateMigration` is correct when a store failure and an incompatible
verdict never look alike: the former always carries a non-nil error with
`reason` left empty, the latter always carries a nil error with a specific
`reason`. `TestValidateMigrationStoreFailureIsNotIncompatibility` is the
test that would catch the collapsed-signature mistake — if `ValidateMigration`
folded a registry outage into `(false, "", nil)`, this test fails because it
expects a non-nil error there. The alphabetical column ordering in
`CheckCompatible` also matters: without `sort.Strings`, iterating a Go map
in nondeterministic order would make `reason` flap between runs whenever a
schema has more than one violation.

The mistake to avoid is having `ValidateMigration` swallow the load error
into the boolean (`return false, "", nil` on a registry failure) — that
tells an operator "this migration is broken" when the true state is "we
don't know yet", and a caller that blocks deploys on `compatible == false`
would block forever on a transient outage instead of retrying.

## Resources

- [Go spec: multiple return values](https://go.dev/ref/spec#Function_types) — the function type grammar behind three-value returns like `(bool, string, error)`.
- [errors.Is](https://pkg.go.dev/errors#Is) — matching `ErrSchemaNotFound` through the wrapped load error.
- [Expand/contract pattern for schema migrations](https://martinfowler.com/bliki/ParallelChange.html) — the backward-compatibility rule (add before you drop) this validator enforces.

---

Back to [00-concepts.md](00-concepts.md) | Prev: [17-oauth-token-decode-claims.md](17-oauth-token-decode-claims.md) | Next: [19-dns-resolver-with-ttl.md](19-dns-resolver-with-ttl.md)
