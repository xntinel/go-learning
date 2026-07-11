# Exercise 8: Billing Amounts: An Equal Method go-cmp Respects

A `Money` value has a hard domain rule: 100 USD is not 100 EUR, no matter how the
numbers line up, and a comparison that equates them will produce a wrong invoice. It
also often carries a cached display string that has nothing to do with identity.
This exercise builds a `Money` type with an `Equal` method that encodes both facts,
and shows that `google/go-cmp` automatically dispatches to that method — while
`reflect.DeepEqual` ignores it and compares raw fields.

This module imports `github.com/google/go-cmp`. It is otherwise fully
self-contained: its own `go mod init`, demo, and tests.

## What you'll build

```text
money/                      independent module: example.com/money
  go.mod                    go 1.26; requires github.com/google/go-cmp
  money.go                  type Money{Amount,Currency,Display}; Equal (ignores Display, refuses cross-currency)
  cmd/
    demo/
      main.go               runnable demo: Equal vs cmp.Equal vs reflect.DeepEqual
  money_test.go             method dispatch; DeepEqual disagreement; cmp.Diff empty on equal
```

- Files: `money.go`, `cmd/demo/main.go`, `money_test.go`.
- Implement: `Money{Amount int64; Currency, Display string}` with `Equal(other Money) bool` that compares `Amount`+`Currency`, ignores the derived `Display`, and refuses cross-currency equality.
- Test: same value equal; cross-currency not equal; `cmp.Equal` matches `Equal` (dispatch); `reflect.DeepEqual` disagrees on the ignored-field case; `cmp.Diff` is empty when equal.
- Verify: `go test -count=1 -race ./...`

Set up the module:

```bash
mkdir -p ~/go-exercises/money/cmd/demo
cd ~/go-exercises/money
go mod init example.com/money
go get github.com/google/go-cmp/cmp
```

### Why the Equal method is the right encoding, and how go-cmp honors it

`Money` has three fields, but only two of them define identity: `Amount` and
`Currency`. `Display` is a *derived* field — a memoized formatted string like
`"$1.00"` — that two equal amounts can legitimately disagree on (one built from a
parser, one from a formatter). A field-by-field comparison would wrongly split those
two equal amounts apart because their `Display` strings differ. So the identity rule
cannot be "all fields equal"; it has to be stated. `Money.Equal` states it: compare
`Amount` and `Currency`, ignore `Display`. It also encodes the domain invariant that
different currencies are never equal — which the `Currency == Currency` check gives
directly.

Now the two comparators diverge in exactly the way this lesson is about:

- `reflect.DeepEqual(a, b)` does **not** call `Money.Equal`. It compares fields, so
  it includes `Display` and calls two equal amounts with different cached display
  strings *unequal*. It is blind to your domain rule.
- `google/go-cmp` **does** dispatch to `Equal`. When `cmp.Equal(a, b)` sees that
  `Money` has a method `Equal(Money) bool`, it uses it and returns exactly what your
  method returns. Your domain equality is honored in tests for free — no options, no
  reflection over fields. `cmp.Diff(a, b)` returns the empty string precisely when
  `Equal` reports true, which is why the idiomatic test is
  `if diff := cmp.Diff(want, got); diff != "" { t.Error(diff) }`.

The payoff is concrete: you assert invoice totals with `cmp.Equal`/`cmp.Diff` and
your "USD is not EUR" and "ignore the display cache" rules are automatically
respected, whereas a `DeepEqual`-based assertion would both leak the display field
into the comparison and, in a type without a currency field, silently equate values
your domain considers different.

Create `money.go`:

```go
package money

// Money is a currency amount in minor units (e.g. cents). Display is a derived,
// memoized formatted string that is NOT part of identity.
type Money struct {
	Amount   int64
	Currency string
	Display  string // derived cache, ignored by Equal
}

// Equal reports whether two amounts are the same money. It compares Amount and
// Currency, ignores the derived Display field, and never treats two different
// currencies as equal. google/go-cmp dispatches to this method automatically;
// reflect.DeepEqual does not (it compares fields, including Display).
func (m Money) Equal(other Money) bool {
	return m.Currency == other.Currency && m.Amount == other.Amount
}
```

### The runnable demo

The demo compares two USD amounts that are equal in value but carry different cached
display strings (`Equal` and `cmp.Equal` say equal; `DeepEqual` says not), then a
cross-currency pair (all say not equal), and shows `cmp.Diff` is empty when `Equal`
holds.

Create `cmd/demo/main.go`:

```go
package main

import (
	"fmt"
	"reflect"

	"github.com/google/go-cmp/cmp"

	"example.com/money"
)

func main() {
	a := money.Money{Amount: 100, Currency: "USD", Display: "$1.00"}
	b := money.Money{Amount: 100, Currency: "USD", Display: "USD 1.00"}
	eur := money.Money{Amount: 100, Currency: "EUR", Display: "EUR 1.00"}

	fmt.Printf("a.Equal(b): %v\n", a.Equal(b))
	fmt.Printf("cmp.Equal(a,b): %v\n", cmp.Equal(a, b))
	fmt.Printf("DeepEqual(a,b): %v\n", reflect.DeepEqual(a, b))

	fmt.Printf("a.Equal(eur): %v\n", a.Equal(eur))
	fmt.Printf("cmp.Equal(a,eur): %v\n", cmp.Equal(a, eur))

	fmt.Printf("diff a,b empty: %v\n", cmp.Diff(a, b) == "")
}
```

Run it:

```bash
go run ./cmd/demo
```

Expected output:

```
a.Equal(b): true
cmp.Equal(a,b): true
DeepEqual(a,b): false
a.Equal(eur): false
cmp.Equal(a,eur): false
diff a,b empty: true
```

### Tests

`TestEqualContract` is table-driven over same-value, ignored-display, and
cross-currency cases. `TestCmpDispatchesToEqual` asserts `cmp.Equal(a, b)` returns
the same answer as `a.Equal(b)` for every case — the dispatch guarantee.
`TestDeepEqualDisagrees` pins the divergence: on two equal amounts with different
`Display` values, `Equal`/`cmp.Equal` say equal while `reflect.DeepEqual` says not.
`TestDiffEmptyWhenEqual` checks `cmp.Diff` is `""` exactly when `Equal` is true and
non-empty otherwise.

Create `money_test.go`:

```go
package money

import (
	"reflect"
	"testing"

	"github.com/google/go-cmp/cmp"
)

var (
	usd1     = Money{Amount: 100, Currency: "USD", Display: "$1.00"}
	usd1alt  = Money{Amount: 100, Currency: "USD", Display: "USD 1.00"}
	usd2     = Money{Amount: 200, Currency: "USD", Display: "$2.00"}
	eur1     = Money{Amount: 100, Currency: "EUR", Display: "EUR 1.00"}
	equalSet = []struct {
		name string
		a, b Money
		want bool
	}{
		{"identical", usd1, usd1, true},
		{"same value different display", usd1, usd1alt, true},
		{"different amount", usd1, usd2, false},
		{"cross currency same amount", usd1, eur1, false},
	}
)

func TestEqualContract(t *testing.T) {
	t.Parallel()

	for _, tt := range equalSet {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			if got := tt.a.Equal(tt.b); got != tt.want {
				t.Fatalf("Equal = %v, want %v", got, tt.want)
			}
		})
	}
}

func TestCmpDispatchesToEqual(t *testing.T) {
	t.Parallel()

	for _, tt := range equalSet {
		if got := cmp.Equal(tt.a, tt.b); got != tt.a.Equal(tt.b) {
			t.Fatalf("%s: cmp.Equal = %v, but Equal = %v (dispatch failed)",
				tt.name, got, tt.a.Equal(tt.b))
		}
	}
}

func TestDeepEqualDisagrees(t *testing.T) {
	t.Parallel()

	// Same money value, different cached Display.
	if !usd1.Equal(usd1alt) {
		t.Fatal("precondition: Equal should ignore Display")
	}
	if reflect.DeepEqual(usd1, usd1alt) {
		t.Fatal("DeepEqual should disagree: it compares Display and ignores Equal")
	}
	if !cmp.Equal(usd1, usd1alt) {
		t.Fatal("cmp.Equal should agree with Equal via method dispatch")
	}
}

func TestDiffEmptyWhenEqual(t *testing.T) {
	t.Parallel()

	if diff := cmp.Diff(usd1, usd1alt); diff != "" {
		t.Fatalf("expected empty diff for equal money, got:\n%s", diff)
	}
	if diff := cmp.Diff(usd1, eur1); diff == "" {
		t.Fatal("expected non-empty diff for cross-currency money")
	}
}
```

## Review

`Money` is correct when equality is the domain rule — amount plus currency, display
ignored, currencies never crossed — and when that rule is what your tests actually
assert. The load-bearing insight is `TestDeepEqualDisagrees`: `reflect.DeepEqual`
compares fields and therefore both leaks the `Display` cache into the comparison and
ignores your `Equal` method, while `go-cmp` dispatches to `Equal` and honors it. That
is why `cmp.Equal`/`cmp.Diff`, not `DeepEqual`, is the right tool for asserting
domain values. Remember `cmp.Diff` returns a string, empty on equality — treat a
non-empty diff as failure, never the diff itself as a bool. Run `go test -race`.

## Resources

- [google/go-cmp: cmp.Equal and cmp.Diff](https://pkg.go.dev/github.com/google/go-cmp/cmp) — the comparator and the `Equal`-method dispatch rule.
- [reflect.DeepEqual](https://pkg.go.dev/reflect#DeepEqual) — the field-based comparator that ignores `Equal` methods.
- [Go spec: Method sets](https://go.dev/ref/spec#Method_sets) — why a value-receiver `Equal(T) bool` is in `Money`'s method set.

---

Back to [00-concepts.md](00-concepts.md) | Next: [09-golden-dto-gocmp-diff.md](09-golden-dto-gocmp-diff.md)
