# Exercise 12: Detect a Card Brand From Its PAN Prefix With a Tagless Switch

**Nivel: Intermedio** — validacion rapida (un test corto).

A payment form needs to show the right card-brand icon and route to the right
acquirer before a single digit is charged, and the only signal available at
that point is the PAN's own prefix. This module builds that detector as a
tagless switch over `strings.HasPrefix` and numeric prefix ranges — the
classic case where an expression switch cannot express the logic at all.

## What you'll build

```text
cardbrand/                 independent module: example.com/card-brand-detector
  go.mod                    go 1.24
  cardbrand.go              package cardbrand; ErrUnknownBrand, ErrInvalidPAN; Brand(pan) (string, error)
  cardbrand_test.go         table over each brand, an unknown prefix, and two invalid PANs
```

- Implement: `Brand(pan string) (string, error)` — validate shape first, then a tagless switch mixing `strings.HasPrefix` with numeric range checks on the first two and four digits.
- Test: a table covering every brand, an unrecognized prefix, and malformed input, both checked with `errors.Is`.
- Verify: `go test -count=1 ./...`

Set up the module:

```bash
go mod edit -go=1.24
```

### Why a tagless switch, not an expression switch

An expression switch compares a tag with `==`, so it can only ever ask "is
this PAN's prefix exactly this string?" Real BIN ranges do not cooperate:
Mastercard's legacy range is any two-digit prefix from 51 through 55, and its
newer range is any four-digit prefix from 2221 through 2720. Neither is a
single value an expression switch can match. The tagless form drops the tag
and lets each case be its own boolean expression, so `prefix2 >= 51 &&
prefix2 <= 55` and `strings.HasPrefix(pan, "4")` sit side by side as peers.
Because Visa, Mastercard, Amex, and Discover ranges start with different
leading digits, no two cases here can ever both be true for the same input —
so, unlike the retry-classifier exercise, ordering is not load-bearing; it is
still written narrowest-fact-first as a defensive habit.

Create `cardbrand.go`:

```go
package cardbrand

import (
	"errors"
	"fmt"
	"strconv"
	"strings"
)

// ErrUnknownBrand marks a PAN whose prefix doesn't match any known card
// brand's IIN/BIN range.
var ErrUnknownBrand = errors.New("cardbrand: unknown brand")

// ErrInvalidPAN marks a PAN that isn't a plausible card number at all.
var ErrInvalidPAN = errors.New("cardbrand: invalid pan")

// Brand identifies the card brand from a PAN (Primary Account Number) using
// its IIN/BIN prefix. It never guesses a brand for malformed input.
func Brand(pan string) (string, error) {
	if len(pan) < 12 || len(pan) > 19 {
		return "", fmt.Errorf("%w: length %d", ErrInvalidPAN, len(pan))
	}
	for _, r := range pan {
		if r < '0' || r > '9' {
			return "", fmt.Errorf("%w: non-digit character", ErrInvalidPAN)
		}
	}

	prefix2, _ := strconv.Atoi(pan[:2])
	prefix4, _ := strconv.Atoi(pan[:4])

	switch {
	case strings.HasPrefix(pan, "4"):
		return "visa", nil
	case prefix4 >= 2221 && prefix4 <= 2720:
		return "mastercard", nil
	case prefix2 >= 51 && prefix2 <= 55:
		return "mastercard", nil
	case prefix2 == 34, prefix2 == 37:
		return "amex", nil
	case strings.HasPrefix(pan, "6011"):
		return "discover", nil
	case prefix2 == 65:
		return "discover", nil
	default:
		return "", fmt.Errorf("%w: prefix %q", ErrUnknownBrand, pan[:2])
	}
}
```

Note the Amex case: `case prefix2 == 34, prefix2 == 37:` is a comma list
inside a tagless switch. It works exactly like a comma list in an expression
switch — it is still "the first true case wins," just with two boolean
expressions instead of two values compared against a tag.

### Test

`TestBrand` runs a table over one real test PAN per brand — including both
Mastercard ranges — plus an unrecognized prefix and two shapes of invalid
input, each checked with `errors.Is` against the sentinel it should produce.

Create `cardbrand_test.go`:

```go
package cardbrand

import (
	"errors"
	"testing"
)

func TestBrand(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name    string
		pan     string
		want    string
		wantErr error
	}{
		{"visa", "4111111111111111", "visa", nil},
		{"mastercard legacy range", "5500000000000004", "mastercard", nil},
		{"mastercard 2-series range", "2223000048400011", "mastercard", nil},
		{"amex", "340000000000009", "amex", nil},
		{"discover 6011", "6011000000000004", "discover", nil},
		{"discover 65", "6500000000000002", "discover", nil},
		{"unknown brand", "9999888877776666", "", ErrUnknownBrand},
		{"too short", "123", "", ErrInvalidPAN},
		{"non digit", "4111abcd11111111", "", ErrInvalidPAN},
	}

	for _, tc := range tests {
		got, err := Brand(tc.pan)
		if tc.wantErr != nil {
			if !errors.Is(err, tc.wantErr) {
				t.Errorf("%s: Brand(%q) error = %v, want errors.Is match for %v", tc.name, tc.pan, err, tc.wantErr)
			}
			continue
		}
		if err != nil {
			t.Errorf("%s: Brand(%q) unexpected error: %v", tc.name, tc.pan, err)
			continue
		}
		if got != tc.want {
			t.Errorf("%s: Brand(%q) = %q, want %q", tc.name, tc.pan, got, tc.want)
		}
	}
}
```

Verify with:

```bash
go test -count=1 ./...
```

## Review

The detector is correct when every real prefix range lands on its brand and
every non-matching or malformed input fails through a named sentinel instead
of an empty string that a caller might mistake for "no brand, but fine."
Carry this forward: reach for a tagless switch the moment a dispatch rule
needs a range, a prefix test, or any predicate an expression switch's `==`
cannot express — and remember a comma list works there too, not just in the
expression form.

## Resources

- [Go Specification: Switch statements](https://go.dev/ref/spec#Switch_statements) — the expressionless (tagless) switch form.
- [strings.HasPrefix](https://pkg.go.dev/strings#HasPrefix) — prefix matching used alongside numeric range checks.

---

Back to [00-concepts.md](00-concepts.md) | Prev: [11-webhook-event-router.md](11-webhook-event-router.md) | Next: [13-file-extension-storage-class.md](13-file-extension-storage-class.md)
