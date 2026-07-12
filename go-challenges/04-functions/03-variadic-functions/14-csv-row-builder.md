# Exercise 14: A CSV Row Builder with RFC 4180 Field Escaping

**Nivel: Intermedio** — validacion rapida (un test corto).

Exporting a data export or report line to CSV is a variable-arity job — a
row has however many columns the schema defines — and the correctness bar
is not "join with commas", it is "join with commas *and* quote fields that
would otherwise corrupt the format". This module builds
`Row(fields ...string) string`, forwarding each field through an escaping
step before joining, the piece a naive `strings.Join(fields, ",")` skips
entirely.

## What you'll build

```text
csvrow/                     independent module: example.com/csv-row
  go.mod                    go 1.24
  csvrow.go                 package csvrow; func Row(fields ...string) string
  csvrow_test.go            table test: plain, comma, quote, newline, zero fields
```

- Files: `csvrow.go`, `csvrow_test.go`.
- Implement: `Row(fields ...string) string` quoting any field containing a comma, a double quote, or a newline, doubling embedded quotes.
- Test: plain fields need no quoting; a field with a comma, a quote, or a newline is quoted (with quotes doubled where relevant); zero fields yields an empty string.
- Verify: `go test -count=1 ./...`

Set up the module:

```bash
go mod edit -go=1.24
```

### Escaping is per-field, joining is once

`Row` cannot just `strings.Join(fields, ",")`: a field like `"Doe, Jane"`
would silently become two columns instead of one. The fix, per RFC 4180, is
a per-field decision made *before* the join: if a field contains a comma, a
double quote, or a newline, wrap the whole field in double quotes and
double every embedded double quote (`"` becomes `""`) so the reader can tell
a literal quote from a delimiter. Fields with none of those three
characters pass through untouched — quoting everything defensively would
still be valid CSV, but this exercise pins the minimal-quoting behavior most
real writers (including `encoding/csv`) produce, so the test asserts exact
output. The variadic surface itself does the least interesting part of the
job — `fields ...string` just gives you a `[]string` to range over — the
escaping logic is where the correctness lives.

Create `csvrow.go`:

```go
// csvrow.go
package csvrow

import "strings"

// Row builds one CSV record from a variable number of fields, applying
// RFC 4180 quoting: a field containing a comma, a double quote, or a newline
// is wrapped in double quotes with any embedded double quote doubled. Fields
// needing no quoting are emitted as-is. Zero fields yields an empty string.
func Row(fields ...string) string {
	parts := make([]string, 0, len(fields))
	for _, f := range fields {
		parts = append(parts, quoteField(f))
	}
	return strings.Join(parts, ",")
}

func quoteField(f string) string {
	if !strings.ContainsAny(f, ",\"\n") {
		return f
	}
	escaped := strings.ReplaceAll(f, `"`, `""`)
	return `"` + escaped + `"`
}
```

### Test

Create `csvrow_test.go`:

```go
// csvrow_test.go
package csvrow

import "testing"

func TestRow(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name   string
		fields []string
		want   string
	}{
		{"plain fields", []string{"alice", "30", "engineer"}, "alice,30,engineer"},
		{"field with comma is quoted", []string{"Doe, Jane", "42"}, `"Doe, Jane",42`},
		{"field with quote is escaped and quoted", []string{`5" screen`, "ok"}, `"5"" screen",ok`},
		{"field with newline is quoted", []string{"line1\nline2", "x"}, "\"line1\nline2\",x"},
		{"zero fields", nil, ""},
		{"single field no quoting needed", []string{"solo"}, "solo"},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			if got := Row(tc.fields...); got != tc.want {
				t.Fatalf("Row(%v) = %q, want %q", tc.fields, got, tc.want)
			}
		})
	}
}
```

Verify: `go test -count=1 ./...`

## Review

`Row` is correct when a field with no special characters passes through
unchanged, a field with a comma, quote, or newline is wrapped in quotes
with any embedded quote doubled, and zero fields returns an empty string
rather than a stray comma or panic. The senior point: a variadic parameter
is a convenient way to accept "however many columns this row has", but the
interesting work — the per-element transform applied before the join — is
identical whether the elements arrived as literal arguments or as a
splatted `[]string`; the variadic shape only decides how the caller hands
you the data, never how you must process it.

## Resources

- [RFC 4180: Common Format for CSV Files](https://www.rfc-editor.org/rfc/rfc4180) — the quoting rule this exercise implements by hand.
- [`encoding/csv`](https://pkg.go.dev/encoding/csv) — the stdlib writer that applies the same rule for production use.

---

Back to [00-concepts.md](00-concepts.md) | Prev: [13-metric-tags-merger.md](13-metric-tags-merger.md) | Next: [15-notification-fanout-recipients.md](15-notification-fanout-recipients.md)
