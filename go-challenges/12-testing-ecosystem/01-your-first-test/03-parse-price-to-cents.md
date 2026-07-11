# Exercise 3: ParseCents: Testing a (value, error) Contract

Money must never be a float. A billing or config layer parses a price string like
`"19.99"` into an integer number of cents and rejects anything malformed. This is
the canonical two-step assertion: check the error first, then the value.

## What you'll build

```text
pricecents/                independent module: example.com/pricecents
  go.mod
  price.go                 ErrInvalidPrice; func ParseCents(s string) (int64, error)
  price_test.go            TestParseCents_Valid, TestParseCents_Invalid, ExampleParseCents
  cmd/
    demo/
      main.go              parses a few prices, prints cents or the error
```

- Files: `price.go`, `price_test.go`, `cmd/demo/main.go`.
- Implement: `ParseCents(s string) (int64, error)` — `"19.99"` yields `1999`, rejecting `"abc"`, `"1.999"`, and `""` with `ErrInvalidPrice`.
- Test: the success path as error-first-then-value; the error path asserting `err != nil` and `errors.Is(err, ErrInvalidPrice)`.
- Verify: `gofmt -l .`, `go vet ./...`, `go test -count=1 -race ./...`.

Set up the module:

```bash
mkdir -p ~/go-exercises/pricecents/cmd/demo
cd ~/go-exercises/pricecents
go mod init example.com/pricecents
```

### Why integer cents, and why exactly two decimals

Representing `$19.99` as the float `19.99` is a bug waiting for a reconciliation
report: `0.1 + 0.2 != 0.3` in binary floating point, and money summed that way
drifts. The fix is to carry an integer count of the smallest unit — cents — and
only format to a decimal string at the edge. `ParseCents` is that edge: it turns
the string a human or a config file wrote into the `int64` the ledger stores.

The parse is deliberately strict. It splits on the decimal point, requires the
fractional part (when present) to be exactly two digits, and parses each part
with `strconv.ParseUint`, which rejects signs, spaces, and non-digits inside the
number. `"1.999"` has three fractional digits — a sub-cent price the ledger
cannot represent — so it is rejected rather than silently rounded. `"abc"` fails
the integer parse. `""` is rejected up front. A leading `-` is handled for
refunds. Strictness here is a feature: a permissive money parser that quietly
accepts garbage is how a `$0.00` charge or a `$1990` overcharge reaches
production.

The error is a wrapped sentinel. `ErrInvalidPrice` is a package-level
`errors.New`, and every failure path wraps it with `%w` and the offending input.
Callers test the category with `errors.Is(err, ErrInvalidPrice)` while still
getting the context (`parse cents "1.999": invalid price`) in logs. This is the
sentinel-plus-`%w` pattern the whole curriculum leans on.

Create `price.go`:

```go
package pricecents

import (
	"errors"
	"fmt"
	"strconv"
	"strings"
)

// ErrInvalidPrice is returned (wrapped) when a price string cannot be parsed
// into an exact integer number of cents.
var ErrInvalidPrice = errors.New("invalid price")

// ParseCents parses a decimal price like "19.99" into cents (1999). The
// fractional part, when present, must be exactly two digits. A leading '-' marks
// a refund. Any other shape is rejected with a wrapped ErrInvalidPrice.
func ParseCents(s string) (int64, error) {
	raw := strings.TrimSpace(s)
	if raw == "" {
		return 0, fmt.Errorf("parse cents %q: %w", s, ErrInvalidPrice)
	}

	body := raw
	neg := false
	if strings.HasPrefix(body, "-") {
		neg = true
		body = body[1:]
	}

	intPart, fracPart, hasDot := strings.Cut(body, ".")
	if intPart == "" {
		intPart = "0"
	}

	dollars, err := strconv.ParseUint(intPart, 10, 63)
	if err != nil {
		return 0, fmt.Errorf("parse cents %q: %w", s, ErrInvalidPrice)
	}

	var cents uint64
	if hasDot {
		if len(fracPart) != 2 {
			return 0, fmt.Errorf("parse cents %q: %w", s, ErrInvalidPrice)
		}
		cents, err = strconv.ParseUint(fracPart, 10, 63)
		if err != nil {
			return 0, fmt.Errorf("parse cents %q: %w", s, ErrInvalidPrice)
		}
	}

	total := int64(dollars)*100 + int64(cents)
	if neg {
		total = -total
	}
	return total, nil
}
```

### The runnable demo

Create `cmd/demo/main.go`:

```go
package main

import (
	"fmt"

	"example.com/pricecents"
)

func main() {
	for _, s := range []string{"19.99", "5", "-2.50", "1.999", "abc"} {
		cents, err := pricecents.ParseCents(s)
		if err != nil {
			fmt.Printf("%-7q -> error: %v\n", s, err)
			continue
		}
		fmt.Printf("%-7q -> %d cents\n", s, cents)
	}
}
```

Run it:

```bash
go run ./cmd/demo
```

Expected output:

```
"19.99" -> 1999 cents
"5"     -> 500 cents
"-2.50" -> -250 cents
"1.999" -> error: parse cents "1.999": invalid price
"abc"   -> error: parse cents "abc": invalid price
```

### The tests

Create `price_test.go`:

```go
package pricecents

import (
	"errors"
	"fmt"
	"testing"
)

func TestParseCents_Valid(t *testing.T) {
	t.Parallel()

	// Two-step contract: the error is fatal (the value is meaningless on error),
	// then the value is asserted.
	got, err := ParseCents("19.99")
	if err != nil {
		t.Fatalf("ParseCents(%q) returned error: %v", "19.99", err)
	}
	if want := int64(1999); got != want {
		t.Errorf("ParseCents(%q) = %d, want %d", "19.99", got, want)
	}

	got, err = ParseCents("-2.50")
	if err != nil {
		t.Fatalf("ParseCents(%q) returned error: %v", "-2.50", err)
	}
	if want := int64(-250); got != want {
		t.Errorf("ParseCents(%q) = %d, want %d", "-2.50", got, want)
	}
}

func TestParseCents_Invalid(t *testing.T) {
	t.Parallel()

	// The error path is a first-class assertion, never ignored.
	for _, in := range []string{"abc", "1.999", ""} {
		_, err := ParseCents(in)
		if err == nil {
			t.Fatalf("ParseCents(%q) = nil error, want ErrInvalidPrice", in)
		}
		if !errors.Is(err, ErrInvalidPrice) {
			t.Errorf("ParseCents(%q) error = %v, want errors.Is ErrInvalidPrice", in, err)
		}
	}
}

func ExampleParseCents() {
	cents, err := ParseCents("19.99")
	fmt.Println(cents, err)
	// Output: 1999 <nil>
}
```

## Review

The parser is correct when the value is meaningful only on a nil error, which is
why the success test checks the error with `t.Fatalf` before touching the `int64`
— reversing that order would dereference a garbage value and produce a confusing
message. The error test proves the negative contract two ways: `err != nil`
(fatal, because there is nothing else to check) and `errors.Is(err,
ErrInvalidPrice)`, which is only true because every failure path wraps the
sentinel with `%w`. The strictness — exactly two fractional digits — is the whole
value of a money parser; loosen it and `"1.999"` silently becomes `199` or
`200` cents. Gate with `gofmt -l .`, `go vet ./...`, and
`go test -count=1 -race ./...`.

## Resources

- [strconv package](https://pkg.go.dev/strconv) — `ParseUint`, `ParseInt`, and the parse-error shapes.
- [errors package](https://pkg.go.dev/errors) — `errors.New`, `errors.Is`, and `%w` wrapping.
- [strings.Cut](https://pkg.go.dev/strings#Cut) — splitting on the first separator.

---

Back to [00-concepts.md](00-concepts.md) | Previous: [02-retryable-http-status-classifier.md](02-retryable-http-status-classifier.md) | Next: [04-slugify-url-path.md](04-slugify-url-path.md)
