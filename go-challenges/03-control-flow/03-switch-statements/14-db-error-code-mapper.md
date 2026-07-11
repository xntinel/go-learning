# Exercise 14: Map SQLSTATE Codes to Domain Errors With an Expression Switch

**Nivel: Intermedio** — validacion rapida (un test corto).

A repository layer sees raw Postgres SQLSTATE codes, and leaking those five
character codes up into handlers means every caller has to know what
`23505` means. This module builds the translator that turns SQLSTATE codes
into a small, closed set of domain sentinels using an expression switch —
the direct-value counterpart to the domain-error-to-status exercise's
wrapped-error dispatch.

## What you'll build

```text
dberr/                      independent module: example.com/db-error-code-mapper
  go.mod                     go 1.24
  dberr.go                   package dberr; ErrConflict, ErrInvalidInput, ErrRetryable, ErrTimeout, ErrInternal; Translate(sqlstate) error
  dberr_test.go              table over every mapped code plus an unrecognized one and an empty one
```

- Implement: `Translate(sqlstate string) error` — an expression switch on the raw code string, with comma-separated cases grouping codes that map to the same domain error, and a fail-closed default.
- Test: a table covering every mapped SQLSTATE code, an unrecognized code, and an empty string, each checked with `errors.Is`.
- Verify: `go test -count=1 ./...`

Set up the module:

```bash
mkdir -p ~/go-exercises/dberr
cd ~/go-exercises/dberr
go mod init example.com/db-error-code-mapper
go mod edit -go=1.24
```

### Expression switch on a code, not errors.Is on a wrapped error

The domain-error-to-status exercise dispatches on *wrapped-error identity* —
a tagless switch with `errors.Is` in every case, because the input there is
already a Go `error` value carrying a sentinel somewhere in its chain. Here
the input is a plain five-character SQLSTATE string straight off the driver,
a closed value from a fixed set defined by the Postgres error-codes
appendix. That is exactly the shape an expression switch wants: compare a tag
against literal values with `==`. Two SQLSTATE codes often deserve the same
domain error — `23505` (unique_violation) and `23503` (foreign_key_violation)
both mean "this write conflicts with existing data" from the caller's point
of view — so those pairs are written as comma case lists rather than
duplicated bodies.

The default is deliberately fail-closed: any code this switch has not been
taught about becomes `ErrInternal`, a 500-equivalent, never something a
caller might treat as retryable or safe to ignore. A repository layer that
guessed "unrecognized code, probably fine" would be the kind of bug that
turns a rare driver error into silent data loss.

Create `dberr.go`:

```go
package dberr

import "errors"

// Sentinel domain errors that a repository layer returns instead of leaking
// raw driver error codes into callers.
var (
	ErrConflict     = errors.New("dberr: conflict")
	ErrInvalidInput = errors.New("dberr: invalid input")
	ErrRetryable    = errors.New("dberr: retryable")
	ErrTimeout      = errors.New("dberr: timeout")
	ErrInternal     = errors.New("dberr: internal")
)

// Translate maps a Postgres SQLSTATE error code to one of a small, closed set
// of domain sentinel errors, so callers branch with errors.Is instead of
// hardcoding SQLSTATE codes throughout the codebase.
func Translate(sqlstate string) error {
	switch sqlstate {
	case "23505", "23503":
		return ErrConflict
	case "23502", "23514":
		return ErrInvalidInput
	case "40001", "40P01":
		return ErrRetryable
	case "57014":
		return ErrTimeout
	default:
		return ErrInternal
	}
}
```

### Test

`TestTranslate` runs a table over every mapped SQLSTATE code — both members
of each comma group, so a regression that drops one code from its group is
caught — plus an unrecognized code and an empty string, both expected to
fall through to `ErrInternal`.

Create `dberr_test.go`:

```go
package dberr

import (
	"errors"
	"testing"
)

func TestTranslate(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name     string
		sqlstate string
		want     error
	}{
		{"unique_violation", "23505", ErrConflict},
		{"foreign_key_violation", "23503", ErrConflict},
		{"not_null_violation", "23502", ErrInvalidInput},
		{"check_violation", "23514", ErrInvalidInput},
		{"serialization_failure", "40001", ErrRetryable},
		{"deadlock_detected", "40P01", ErrRetryable},
		{"query_canceled", "57014", ErrTimeout},
		{"unrecognized code", "99999", ErrInternal},
		{"empty code", "", ErrInternal},
	}

	for _, tc := range tests {
		got := Translate(tc.sqlstate)
		if !errors.Is(got, tc.want) {
			t.Errorf("%s: Translate(%q) = %v, want errors.Is match for %v", tc.name, tc.sqlstate, got, tc.want)
		}
	}
}
```

Verify with:

```bash
go test -count=1 ./...
```

## Review

The mapper is correct when every code in a comma group produces the same
sentinel as its sibling, and anything outside the five mapped groups —
including the empty string, which a caller could pass by accident — resolves
to `ErrInternal` rather than being silently treated as fine. Carry this
forward: when the input is already a closed set of literal codes, reach for
an expression switch and comma case lists to group codes that share meaning;
save the tagless `errors.Is` form for when the input is a wrapped Go error
instead of a raw code.

## Resources

- [Go Specification: Switch statements](https://go.dev/ref/spec#Switch_statements) — expression switch with comma-separated case lists.
- [PostgreSQL Error Codes](https://www.postgresql.org/docs/current/errcodes-appendix.html) — the SQLSTATE codes this switch classifies.

---

Back to [00-concepts.md](00-concepts.md) | Prev: [13-file-extension-storage-class.md](13-file-extension-storage-class.md) | Next: [15-permission-level-cascade.md](15-permission-level-cascade.md)
