# Exercise 13: Splitting An Invoice Total Into (perLine, remainder, err)

**Nivel: Intermedio** — validacion rapida (un test corto).

Billing code that divides a total among line items in integer cents almost
never divides evenly, and dropping or inventing a cent is a real accounting
bug. This exercise builds `SplitEvenly(totalCents, lines) (perLineCents int64,
remainderCents int64, err error)`, which returns both the even share and the
leftover cents so the caller — not the split function — decides where the
remainder goes.

This module is fully self-contained: its own `go mod init`, all code inline,
one quick test file.

## What you'll build

```text
invoicesplit/               independent module: example.com/invoice-split
  go.mod                    go 1.24
  split.go                  package invoicesplit; SplitEvenly(totalCents, lines) (perLineCents, remainderCents, err); ErrInvalidLineCount/ErrNegativeTotal
  split_test.go              one table test; reconstructs total from (perLine, remainder)
```

- Files: `split.go`, `split_test.go`.
- Implement: `SplitEvenly(totalCents int64, lines int) (perLineCents int64, remainderCents int64, err error)` using integer division and modulo, rejecting `lines <= 0` and `totalCents < 0`.
- Test: a table over an exact split, a split with remainder, a single line, a zero total, a larger remainder, zero lines, negative lines, and a negative total, each success case reconstructing `lines*perLineCents + remainderCents == totalCents`.
- Verify: `go test -count=1 ./...`

Set up the module:

```bash
mkdir -p go-solutions/04-functions/01-function-declaration-and-multiple-return-values/13-invoice-split-quotient-remainder
cd go-solutions/04-functions/01-function-declaration-and-multiple-return-values/13-invoice-split-quotient-remainder
go mod edit -go=1.24
```

### Integer division always leaves a remainder to place

Money is cents, and cents are indivisible, so `totalCents / lines` almost never
divides exactly: `1000` cents across `3` lines gives `333` per line with `1`
cent left over, and that cent has to land somewhere or the invoice total stops
reconciling. `SplitEvenly` does not decide where — it hands back both the
quotient and the remainder as two named returns, and the caller (usually:
"add the remainder to the first line") makes the business decision. This is
the same idea as `x / y, x % y` in the standard library's own numeric
conventions, surfaced through a domain-named multi-return instead of two
separate operator calls the caller might forget to pair.

Two invalid-input classes get distinct sentinels: `ErrInvalidLineCount` when
`lines` is zero or negative — dividing by zero or by a negative count of line
items is nonsensical — and `ErrNegativeTotal` when the total itself is
negative, which should never happen for an invoice and signals a bug upstream
rather than a normal edge case.

Create `split.go`:

```go
package invoicesplit

import (
	"errors"
	"fmt"
)

// ErrInvalidLineCount is returned when the number of lines to split across is
// not positive.
var ErrInvalidLineCount = errors.New("invalid line count")

// ErrNegativeTotal is returned when the total to split is negative.
var ErrNegativeTotal = errors.New("negative total")

// SplitEvenly divides totalCents into lines equal integer-cent shares,
// returning the per-line amount and whatever cents are left over after
// integer division. Cents are indivisible, so the split can never be exact
// when totalCents is not a multiple of lines: perLineCents*lines +
// remainderCents always equals totalCents, and it is the caller's decision
// (usually: add the remainder to the first invoice line) where the leftover
// cents land.
func SplitEvenly(totalCents int64, lines int) (perLineCents int64, remainderCents int64, err error) {
	if lines <= 0 {
		return 0, 0, fmt.Errorf("split into %d lines: %w", lines, ErrInvalidLineCount)
	}
	if totalCents < 0 {
		return 0, 0, fmt.Errorf("split %d cents: %w", totalCents, ErrNegativeTotal)
	}

	n := int64(lines)
	perLineCents = totalCents / n
	remainderCents = totalCents % n
	return perLineCents, remainderCents, nil
}
```

At the call site: `perLine, remainder, err := invoicesplit.SplitEvenly(total,
len(lineItems))`, handling `err` first, then adding `perLine` to every line
item and `remainder` to whichever one the business rule says absorbs it
(typically the first).

### Test

Create `split_test.go`:

```go
package invoicesplit

import (
	"errors"
	"testing"
)

func TestSplitEvenly(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name          string
		totalCents    int64
		lines         int
		wantPerLine   int64
		wantRemainder int64
		wantErr       error // nil means no error expected
	}{
		{"exact split", 900, 3, 300, 0, nil},
		{"one cent remainder", 1000, 3, 333, 1, nil},
		{"single line", 4999, 1, 4999, 0, nil},
		{"zero total", 0, 5, 0, 0, nil},
		{"remainder larger", 1005, 4, 251, 1, nil},
		{"zero lines", 1000, 0, 0, 0, ErrInvalidLineCount},
		{"negative lines", 1000, -2, 0, 0, ErrInvalidLineCount},
		{"negative total", -100, 3, 0, 0, ErrNegativeTotal},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			perLine, remainder, err := SplitEvenly(tc.totalCents, tc.lines)

			if tc.wantErr == nil {
				if err != nil {
					t.Fatalf("SplitEvenly(%d, %d): unexpected error: %v", tc.totalCents, tc.lines, err)
				}
				if perLine != tc.wantPerLine || remainder != tc.wantRemainder {
					t.Fatalf("SplitEvenly(%d, %d) = (%d, %d), want (%d, %d)",
						tc.totalCents, tc.lines, perLine, remainder, tc.wantPerLine, tc.wantRemainder)
				}
				if int64(tc.lines)*perLine+remainder != tc.totalCents {
					t.Fatalf("SplitEvenly(%d, %d): reconstruction failed: %d*%d+%d != %d",
						tc.totalCents, tc.lines, tc.lines, perLine, remainder, tc.totalCents)
				}
				return
			}

			if !errors.Is(err, tc.wantErr) {
				t.Fatalf("SplitEvenly(%d, %d): err = %v, want errors.Is match for %v",
					tc.totalCents, tc.lines, err, tc.wantErr)
			}
		})
	}
}
```

## Review

`SplitEvenly` is correct when the reconstruction identity
`lines*perLineCents + remainderCents == totalCents` holds for every success
case — the table test checks this explicitly rather than trusting the
implementation, which is the right instinct any time a function splits a
whole into parts. Invalid input is separated into two sentinels: a bad line
count is `ErrInvalidLineCount`, a negative total is `ErrNegativeTotal`. The
mistake this avoids: a split function that decides on its own where the
remainder goes (say, silently dropping it, or always adding it to the last
line) instead of surfacing it and letting the caller apply the actual
accounting rule.

## Resources

- [Go spec: integer operators](https://go.dev/ref/spec#Arithmetic_operators) — integer division truncates and `%` gives the exact remainder for the identity `a == (a/b)*b + a%b`.
- [errors.Is](https://pkg.go.dev/errors#Is) — matching a returned error against one of two sentinels through `fmt.Errorf`'s `%w` wrap.

---

Back to [00-concepts.md](00-concepts.md) | Prev: [12-pagination-params-limit-offset.md](12-pagination-params-limit-offset.md) | Next: [14-semver-parse-major-minor-patch.md](14-semver-parse-major-minor-patch.md)
