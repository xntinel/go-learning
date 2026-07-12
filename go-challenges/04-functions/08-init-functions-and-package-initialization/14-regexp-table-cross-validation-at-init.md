# Exercise 14: Fail-Fast init() — Cross-Validating a Regexp Redaction Table

**Nivel: Intermedio** — validacion rapida (un test corto).

A log redaction package keeps a list of sensitive field names and a table of
compiled patterns describing each field's expected shape. This exercise
validates at `init()` that the two stay in sync — every sensitive field has a
pattern, and every pattern belongs to a declared sensitive field — so a typo
in either list crashes the binary at load instead of leaking a secret or
silently redacting an unrelated field.

## What you'll build

```text
redact/                    independent module: example.com/redact
  go.mod                     module example.com/redact
  redact.go                   sensitiveFields, patterns table, init() validation, Redact
  redact_test.go              package-loaded proof + broken-table rejection + Redact table test
```

Files: `redact.go`, `redact_test.go`.
Implement: `sensitiveFields []string`, `patterns map[string]*regexp.Regexp`, an extracted `validatePatterns` helper called from `init()`, and `Redact(field, value string) string`.
Test: the package loads without panicking; `validatePatterns` rejects a table missing a pattern or holding an orphaned one; `Redact` masks matching sensitive values and leaves everything else untouched.

Set up the module:

```bash
mkdir -p go-solutions/04-functions/08-init-functions-and-package-initialization/14-regexp-table-cross-validation-at-init
cd go-solutions/04-functions/08-init-functions-and-package-initialization/14-regexp-table-cross-validation-at-init
go mod edit -go=1.24
```

### Why two tables need a consistency check, not just MustCompile

`regexp.MustCompile` alone only proves each individual pattern is valid
regexp syntax — it says nothing about whether the *set* of patterns lines up
with the *set* of fields it is supposed to cover. A field added to
`sensitiveFields` without a matching entry in `patterns` would silently pass
every value straight through unredacted; a leftover pattern for a field that
was removed from `sensitiveFields` is dead weight that hides a stale
assumption. `validatePatterns` closes that gap by checking both directions
of the mapping, once, at load.

Create `redact.go`:

```go
// Package redact masks sensitive structured-log field values that match an
// expected shape, using a package-level regexp table cross-checked for
// consistency at init.
package redact

import (
	"fmt"
	"regexp"
)

// sensitiveFields lists log field names that must be redacted before a
// record is emitted.
var sensitiveFields = []string{"password", "ssn", "credit_card", "api_key"}

// patterns holds one compiled matcher per sensitive field, keyed by field
// name. A value is redacted only if it matches the field's expected shape,
// so an obviously malformed value (which is more likely a bug than real
// secret data) is left visible for debugging.
var patterns = map[string]*regexp.Regexp{
	"password":    regexp.MustCompile(`.+`),
	"ssn":         regexp.MustCompile(`^\d{3}-\d{2}-\d{4}$`),
	"credit_card": regexp.MustCompile(`^\d{13,19}$`),
	"api_key":     regexp.MustCompile(`^[A-Za-z0-9_-]{16,}$`),
}

func init() {
	if err := validatePatterns(sensitiveFields, patterns); err != nil {
		panic(err)
	}
}

// validatePatterns fails if a sensitive field has no compiled pattern, or a
// pattern exists for a field that was never declared sensitive. Extracted
// from init so a test can drive it directly with a deliberately broken
// table.
func validatePatterns(fields []string, table map[string]*regexp.Regexp) error {
	known := make(map[string]bool, len(fields))
	for _, f := range fields {
		known[f] = true
		if table[f] == nil {
			return fmt.Errorf("redact: field %q has no compiled pattern", f)
		}
	}
	for f := range table {
		if !known[f] {
			return fmt.Errorf("redact: pattern table has pattern for undeclared field %q", f)
		}
	}
	return nil
}

// Redact masks value with "***" if field is sensitive and value matches its
// expected shape. A sensitive field whose value does NOT match the shape,
// and any field that is not sensitive at all, is returned unchanged.
func Redact(field, value string) string {
	re, ok := patterns[field]
	if !ok {
		return value
	}
	if re.MatchString(value) {
		return "***"
	}
	return value
}
```

Create `redact_test.go`:

```go
package redact

import (
	"regexp"
	"testing"
)

// TestPackageLoaded proves init did not panic: if the shipped table were
// inconsistent, this test binary would never reach a test body.
func TestPackageLoaded(t *testing.T) {
	if got := Redact("password", "hunter2"); got != "***" {
		t.Fatalf("package failed to load a working pattern table, got %q", got)
	}
}

func TestRedact(t *testing.T) {
	tests := []struct {
		name  string
		field string
		value string
		want  string
	}{
		{"password_matches", "password", "hunter2", "***"},
		{"ssn_matches", "ssn", "123-45-6789", "***"},
		{"ssn_malformed_left_visible", "ssn", "not-an-ssn", "not-an-ssn"},
		{"credit_card_matches", "credit_card", "4111111111111111", "***"},
		{"api_key_matches", "api_key", "abcDEF1234567890", "***"},
		{"non_sensitive_field_untouched", "username", "alice", "alice"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := Redact(tt.field, tt.value); got != tt.want {
				t.Errorf("Redact(%q, %q) = %q, want %q", tt.field, tt.value, got, tt.want)
			}
		})
	}
}

func TestValidatePatternsRejectsMissingPattern(t *testing.T) {
	fields := []string{"password", "ssn"}
	broken := map[string]*regexp.Regexp{
		"password": regexp.MustCompile(`.+`),
		// "ssn" is declared sensitive but has no compiled pattern here.
	}
	if err := validatePatterns(fields, broken); err == nil {
		t.Fatal("validatePatterns did not reject a sensitive field with no pattern")
	}
}

func TestValidatePatternsRejectsOrphanPattern(t *testing.T) {
	fields := []string{"password"}
	broken := map[string]*regexp.Regexp{
		"password": regexp.MustCompile(`.+`),
		"ssn":      regexp.MustCompile(`^\d+$`), // not in fields
	}
	if err := validatePatterns(fields, broken); err == nil {
		t.Fatal("validatePatterns did not reject a pattern for an undeclared field")
	}
}
```

Verify: `go test -count=1 ./...`

## Review

The check runs in both directions on purpose: `TestValidatePatternsRejectsMissingPattern`
catches a sensitive field with nothing to match against, and
`TestValidatePatternsRejectsOrphanPattern` catches a pattern for a field
nobody declared sensitive. Either mistake alone would pass a naive check that
only walks one of the two collections. Panicking at `init()` on either
failure means the binary never accepts a request with a redaction table it
cannot trust, which is a far cheaper place to find the bug than a request
log full of an unmasked credit card number.

## Resources

- [regexp.MustCompile](https://pkg.go.dev/regexp#MustCompile) — compiles a static pattern once, panicking on invalid syntax.
- [Go spec — Package initialization](https://go.dev/ref/spec#Package_initialization) — when `init()` runs relative to package-level variables.

---

Back to [00-concepts.md](00-concepts.md) | Prev: [13-constructor-refactor-for-parallel-tests.md](13-constructor-refactor-for-parallel-tests.md) | Next: [15-multi-package-init-ordering-with-dependencies.md](15-multi-package-init-ordering-with-dependencies.md)
