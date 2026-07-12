# Exercise 8: Translating A DB Constraint Violation Into A Field Error

A duplicate-email insert is not a server crash — it is a user-visible conflict on
one field. This module builds the repository-layer translator that recognizes a
PostgreSQL unique-constraint violation (SQLSTATE `23505` plus a constraint name)
and turns it into a per-field 409 `FieldError`, while letting every unrecognized
driver error pass through untouched so a real fault is never masked.

This module is fully self-contained: its own `go mod init`, all code inline, its
own demo and tests.

## What you'll build

```text
constraintmap/             independent module: example.com/constraintmap
  go.mod                   go 1.26
  constraintmap.go         pgError (SQLSTATE+constraint); constraint->field table; MapConstraint
  cmd/
    demo/
      main.go              runnable demo: map a 23505, an unknown constraint, a timeout
  constraintmap_test.go    23505->FieldError; unknown constraint passes through; non-pg error passes through
```

- Files: `constraintmap.go`, `cmd/demo/main.go`, `constraintmap_test.go`.
- Implement: a `pgError{Code, Constraint}` driver error, a `constraint -> field` table, and `MapConstraint(err)` that recognizes a `23505` via `errors.As`, looks up the field, and returns `FieldError{Field, Code: conflict}`; everything else passes through unchanged.
- Test: `pgError{Code:"23505", Constraint:"users_email_key"}` -> `FieldError{Field:"email", Code:conflict}`; an unknown constraint returns the original error unchanged; a non-constraint error (`context.DeadlineExceeded`) is returned as-is and still matches `errors.Is`.
- Verify: `go test -count=1 -race ./...`

Set up the module:

```bash
mkdir -p go-solutions/10-error-handling/11-structured-error-types/08-db-constraint-to-field-error/cmd/demo
cd go-solutions/10-error-handling/11-structured-error-types/08-db-constraint-to-field-error
go mod edit -go=1.26
```

### Recognize the specific fault; pass the rest through

The translator has one job with two equally important halves. The first half:
recognize the *specific* fault. A real driver (`github.com/jackc/pgx` returns a
`*pgconn.PgError`) exposes the SQLSTATE code and the violated constraint name.
SQLSTATE `23505` is `unique_violation`; the constraint name (`users_email_key`)
tells you *which* column. `MapConstraint` uses `errors.As` to find the driver
error in the chain, checks the code is `23505`, looks the constraint up in a
`constraint -> field` table, and returns a `FieldError{Field: "email", Code:
conflict}` — a 409 the HTTP layer already knows how to serialize.

The second half, which is where translators go wrong: pass everything else
through *unchanged*. A driver error with a different SQLSTATE, a constraint the
table does not know, or a completely different error (a dropped connection, a
context deadline) must be returned as the identical error value. Two failure
modes to avoid: translating nothing (so a 409 leaks as a 500), and translating
too much (mapping every driver error to 409, which hides a real fault behind a
fake validation message). The rule is surgical: recognize exactly `23505` + a
known constraint, return the original for all else. Returning the original value
(not a wrapper) keeps `errors.Is` working for the caller — a `context.
DeadlineExceeded` that passes through still satisfies `errors.Is(result,
context.DeadlineExceeded)`.

Create `constraintmap.go`:

```go
package constraintmap

import (
	"errors"
	"fmt"
)

type Code string

const CodeConflict Code = "conflict"

// FieldError is the translated, user-visible result: a 409 on a specific field.
type FieldError struct {
	Code  Code
	Field string
	cause error
}

func (e *FieldError) Error() string {
	return fmt.Sprintf("%s: %s", e.Field, e.Code)
}

func (e *FieldError) Unwrap() error { return e.cause }

// pgError models the driver error a real pgx/pq exposes: a SQLSTATE code and the
// violated constraint name.
type pgError struct {
	Code       string // SQLSTATE, e.g. "23505" = unique_violation
	Constraint string // e.g. "users_email_key"
	Message    string
}

func (e *pgError) Error() string {
	return fmt.Sprintf("pg %s: %s (%s)", e.Code, e.Message, e.Constraint)
}

// sqlstateUniqueViolation is SQLSTATE 23505.
const sqlstateUniqueViolation = "23505"

// constraintFields maps a database constraint name to the request field that
// owns it.
var constraintFields = map[string]string{
	"users_email_key":   "email",
	"orders_number_key": "number",
}

// MapConstraint translates a recognized unique-constraint violation into a
// per-field conflict FieldError. Any error it does not specifically recognize is
// returned unchanged, so real faults are never masked.
func MapConstraint(err error) error {
	if err == nil {
		return nil
	}

	var pg *pgError
	if !errors.As(err, &pg) {
		return err // not a driver error we understand
	}
	if pg.Code != sqlstateUniqueViolation {
		return err // some other SQLSTATE; pass through
	}
	field, ok := constraintFields[pg.Constraint]
	if !ok {
		return err // unknown constraint; do not guess
	}
	return &FieldError{Code: CodeConflict, Field: field, cause: err}
}
```

### The runnable demo

`pgError` is unexported, so the demo — which lives in a different package — needs
an exported constructor to build a driver-style error. Add one to the library.

Append to `constraintmap.go`:

```go
// NewPgUniqueViolation builds a driver-style unique-violation error for callers
// (like the demo) outside this package.
func NewPgUniqueViolation(constraint, message string) error {
	return &pgError{Code: sqlstateUniqueViolation, Constraint: constraint, Message: message}
}
```

Create `cmd/demo/main.go`:

```go
package main

import (
	"context"
	"errors"
	"fmt"

	"example.com/constraintmap"
)

func main() {
	dup := fmt.Errorf("insert user: %w",
		constraintmap.NewPgUniqueViolation("users_email_key", "duplicate key"))
	fmt.Println(constraintmap.MapConstraint(dup))

	unknown := constraintmap.NewPgUniqueViolation("widgets_pkey", "dup")
	fmt.Println(constraintmap.MapConstraint(unknown))

	out := constraintmap.MapConstraint(context.DeadlineExceeded)
	fmt.Println(errors.Is(out, context.DeadlineExceeded))
}
```

Run it:

```bash
go run ./cmd/demo
```

Expected output:

```
email: conflict
pg 23505: dup (widgets_pkey)
true
```

The first line is the translated field conflict; the second is the unknown
constraint passed through untouched (its own `Error()` string); the third proves
a deadline survives the mapper and still matches `errors.Is`.

### Tests

The table covers the three paths that define the translator: a recognized `23505`
on a known constraint becomes a `FieldError`; a `23505` on an *unknown* constraint
passes through as the identical error value; and a non-driver error
(`context.DeadlineExceeded`) passes through and still satisfies `errors.Is`.

Create `constraintmap_test.go`:

```go
package constraintmap

import (
	"context"
	"errors"
	"fmt"
	"testing"
)

func TestMapsKnownUniqueViolation(t *testing.T) {
	t.Parallel()

	err := fmt.Errorf("insert: %w", &pgError{Code: "23505", Constraint: "users_email_key"})

	out := MapConstraint(err)
	var fe *FieldError
	if !errors.As(out, &fe) {
		t.Fatalf("MapConstraint did not yield a *FieldError: %v", out)
	}
	if fe.Field != "email" || fe.Code != CodeConflict {
		t.Fatalf("FieldError = %s/%s, want email/conflict", fe.Field, fe.Code)
	}
}

func TestUnknownConstraintPassesThrough(t *testing.T) {
	t.Parallel()

	orig := &pgError{Code: "23505", Constraint: "widgets_pkey"}
	if out := MapConstraint(orig); out != error(orig) {
		t.Fatalf("unknown constraint should pass through unchanged, got %v", out)
	}
}

func TestOtherSQLStatePassesThrough(t *testing.T) {
	t.Parallel()

	orig := &pgError{Code: "23503", Constraint: "orders_number_key"} // foreign_key_violation
	if out := MapConstraint(orig); out != error(orig) {
		t.Fatalf("non-23505 should pass through unchanged, got %v", out)
	}
}

func TestNonDriverErrorPassesThrough(t *testing.T) {
	t.Parallel()

	out := MapConstraint(context.DeadlineExceeded)
	if !errors.Is(out, context.DeadlineExceeded) {
		t.Fatalf("deadline should pass through and match errors.Is, got %v", out)
	}
}

func TestNilPassesThrough(t *testing.T) {
	t.Parallel()

	if out := MapConstraint(nil); out != nil {
		t.Fatalf("MapConstraint(nil) = %v, want nil", out)
	}
}
```

An `Example` verified against its `// Output:` comment:

```go
// constraintmap_example_test.go
package constraintmap

import "fmt"

func ExampleMapConstraint() {
	err := &pgError{Code: "23505", Constraint: "orders_number_key"}
	fmt.Println(MapConstraint(err))
	// Output: number: conflict
}
```

## Review

The translator is correct when it changes *only* the fault it fully recognizes —
SQLSTATE `23505` on a constraint in the table — into a `FieldError`, and returns
the byte-identical original value for every other input. The `out != error(orig)`
identity check in the tests is deliberate: passing through must not wrap or copy,
because callers rely on `errors.Is` and on the original error's own type. The two
symmetric mistakes: returning a 500 for a duplicate-key (it is a user-visible
409) and mapping every driver error to 409 (which buries a dropped connection or
a deadlock under a fake validation message). Recognize the specific SQLSTATE and
constraint; pass the rest through. Run `go test -race`.

## Resources

- [`errors.As`](https://pkg.go.dev/errors#As) — pulling the driver error out of a wrapped chain.
- [PostgreSQL Error Codes](https://www.postgresql.org/docs/current/errcodes-appendix.html) — SQLSTATE `23505` `unique_violation` and the class 23 integrity constraints.
- [`pgconn.PgError`](https://pkg.go.dev/github.com/jackc/pgx/v5/pgconn#PgError) — the real driver error this module models (`Code`, `ConstraintName`).

---

Back to [00-concepts.md](00-concepts.md) | Prev: [07-error-code-to-http-status-mapper.md](07-error-code-to-http-status-mapper.md) | Next: [09-bulk-import-error-cap.md](09-bulk-import-error-cap.md)
