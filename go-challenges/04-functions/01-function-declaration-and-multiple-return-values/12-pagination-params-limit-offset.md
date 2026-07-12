# Exercise 12: Pagination Params Returning (limit, offset, err)

**Nivel: Intermedio** — validacion rapida (un test corto).

Every list endpoint reads `limit` and `offset` from the query string, applies
defaults when they are absent, and rejects values outside a sane range before
touching the database. This exercise builds
`ParseListParams(q) (limit int, offset int, err error)`, the small multi-return
function that guards every paginated handler's entry point.

This module is fully self-contained: its own `go mod init`, all code inline,
one quick test file.

## What you'll build

```text
pagination/                independent module: example.com/pagination-params
  go.mod                   go 1.24
  pagination.go            package pagination; ParseListParams(q) (limit, offset, err); ErrInvalidLimit/ErrInvalidOffset
  pagination_test.go       one table test; errors.Is against both sentinels
```

- Files: `pagination.go`, `pagination_test.go`.
- Implement: `ParseListParams(q url.Values) (limit int, offset int, err error)` defaulting `limit` to 20 and `offset` to 0 when absent, converting present values with `strconv.Atoi`, and range-checking `limit` to `1..100` and `offset` to `>= 0`.
- Test: a table over defaults, explicit valid values, limit at each bound, a non-numeric limit, an out-of-range limit, a non-numeric offset, and a negative offset, asserting `errors.Is` against `ErrInvalidLimit` or `ErrInvalidOffset`.
- Verify: `go test -count=1 ./...`

Set up the module:

```bash
mkdir -p go-solutions/04-functions/01-function-declaration-and-multiple-return-values/12-pagination-params-limit-offset
cd go-solutions/04-functions/01-function-declaration-and-multiple-return-values/12-pagination-params-limit-offset
go mod edit -go=1.24
```

### Defaults first, validation only when present

A query parameter that is simply absent is not an error — `GET /items` with no
`?limit=` at all should behave exactly like `?limit=20`. The function therefore
seeds each return with its default and only parses when `q.Get(name)` returns a
non-empty string. This mirrors the `(value, ok)` instinct from the concepts
file, but inverted: here "not present" is handled inline with a default instead
of surfacing a bool, because a paginated list genuinely has a sensible fallback,
unlike a repository read.

Two failure modes get distinct sentinels: `ErrInvalidLimit` when `limit` is
present but non-numeric or outside `1..100`; `ErrInvalidOffset` when `offset` is
present but non-numeric or negative. A caller building an HTTP 400 response
branches on which sentinel came back to write a specific message ("limit must
be between 1 and 100" vs. "offset must not be negative") instead of a generic
"bad request".

Create `pagination.go`:

```go
package pagination

import (
	"errors"
	"fmt"
	"net/url"
	"strconv"
)

const (
	defaultLimit = 20
	maxLimit     = 100
	minLimit     = 1
)

// ErrInvalidLimit is returned when the "limit" query parameter is present but
// not a valid integer, or falls outside 1..100.
var ErrInvalidLimit = errors.New("invalid limit")

// ErrInvalidOffset is returned when the "offset" query parameter is present
// but not a valid integer, or is negative.
var ErrInvalidOffset = errors.New("invalid offset")

// ParseListParams reads "limit" and "offset" from an HTTP query string,
// applying defaults when a parameter is absent and validating bounds when it
// is present. limit defaults to 20 and must be in 1..100; offset defaults to
// 0 and must be >= 0.
func ParseListParams(q url.Values) (limit int, offset int, err error) {
	limit = defaultLimit
	if raw := q.Get("limit"); raw != "" {
		limit, err = strconv.Atoi(raw)
		if err != nil {
			return 0, 0, fmt.Errorf("limit %q: %w", raw, ErrInvalidLimit)
		}
		if limit < minLimit || limit > maxLimit {
			return 0, 0, fmt.Errorf("limit %d: %w", limit, ErrInvalidLimit)
		}
	}

	offset = 0
	if raw := q.Get("offset"); raw != "" {
		offset, err = strconv.Atoi(raw)
		if err != nil {
			return 0, 0, fmt.Errorf("offset %q: %w", raw, ErrInvalidOffset)
		}
		if offset < 0 {
			return 0, 0, fmt.Errorf("offset %d: %w", offset, ErrInvalidOffset)
		}
	}

	return limit, offset, nil
}
```

At the call site: `limit, offset, err := pagination.ParseListParams(r.URL.Query())`,
handling `err` first and passing `limit, offset` straight into the repository
query on success.

### Test

Create `pagination_test.go`:

```go
package pagination

import (
	"errors"
	"net/url"
	"testing"
)

func TestParseListParams(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name       string
		query      string
		wantLimit  int
		wantOffset int
		wantErr    error // nil means no error expected
	}{
		{"defaults", "", 20, 0, nil},
		{"explicit valid", "limit=50&offset=100", 50, 100, nil},
		{"limit at min", "limit=1", 1, 0, nil},
		{"limit at max", "limit=100", 100, 0, nil},
		{"limit non-numeric", "limit=abc", 0, 0, ErrInvalidLimit},
		{"limit too small", "limit=0", 0, 0, ErrInvalidLimit},
		{"limit too large", "limit=101", 0, 0, ErrInvalidLimit},
		{"offset non-numeric", "offset=xyz", 0, 0, ErrInvalidOffset},
		{"offset negative", "offset=-1", 0, 0, ErrInvalidOffset},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			q, err := url.ParseQuery(tc.query)
			if err != nil {
				t.Fatalf("bad test query %q: %v", tc.query, err)
			}

			limit, offset, err := ParseListParams(q)

			if tc.wantErr == nil {
				if err != nil {
					t.Fatalf("ParseListParams(%q): unexpected error: %v", tc.query, err)
				}
				if limit != tc.wantLimit || offset != tc.wantOffset {
					t.Fatalf("ParseListParams(%q) = (%d, %d), want (%d, %d)",
						tc.query, limit, offset, tc.wantLimit, tc.wantOffset)
				}
				return
			}

			if !errors.Is(err, tc.wantErr) {
				t.Fatalf("ParseListParams(%q): err = %v, want errors.Is match for %v", tc.query, err, tc.wantErr)
			}
		})
	}
}
```

## Review

`ParseListParams` is correct when an empty query yields the defaults, an
explicit valid query yields exactly those values, and every malformed input
lands on the right sentinel: a bad or out-of-range `limit` is `ErrInvalidLimit`,
a bad or negative `offset` is `ErrInvalidOffset`. The table test's bound cases
(`limit=1`, `limit=100`) prove the range check is inclusive on both ends, which
is exactly where off-by-one bugs hide. The mistake this avoids: validating
before checking for absence, which would reject the common case of a client
that simply omits `limit` and `offset` and expects the defaults.

## Resources

- [net/url.Values](https://pkg.go.dev/net/url#Values) — reading query parameters via `Get`, which returns `""` for an absent key.
- [errors.Is](https://pkg.go.dev/errors#Is) — matching a returned error against one of two sentinels through `fmt.Errorf`'s `%w` wrap.

---

Back to [00-concepts.md](00-concepts.md) | Prev: [11-hostport-config-parser.md](11-hostport-config-parser.md) | Next: [13-invoice-split-quotient-remainder.md](13-invoice-split-quotient-remainder.md)
