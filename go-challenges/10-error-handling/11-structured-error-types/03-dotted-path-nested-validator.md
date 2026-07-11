# Exercise 3: Dotted Field Paths For Nested And Repeated Fields

The original lesson described dotted paths in prose but only ever validated a
flat struct. This module builds the real thing: a recursive validator over a
nested order payload that emits precise, index-bearing paths — `customer.email`,
`items.2.sku`, `shipping.zip` — so a frontend can map each error to exactly one
form input.

This module is fully self-contained: its own `go mod init`, all code inline, its
own demo and tests.

## What you'll build

```text
orderpath/                 independent module: example.com/orderpath
  go.mod                   go 1.26
  orderpath.go             path builder (child/index); CreateOrderRequest tree; recursive Validate
  cmd/
    demo/
      main.go              runnable demo: validate a nested order, print the paths
  orderpath_test.go        exact path-set assertions; valid order -> nil
```

- Files: `orderpath.go`, `cmd/demo/main.go`, `orderpath_test.go`.
- Implement: a compositional path builder (`child(base, field)`, `index(base, i)`), a `CreateOrderRequest{Customer, []LineItem, ShippingAddress}` tree, and a recursive `Validate` accumulating `*FieldError` with fully-qualified `Field` paths.
- Test: an order with a blank `customer.email`, `items[1].qty == 0`, and `items[2].sku == ""` yields exactly `{customer.email, items.1.qty, items.2.sku}` (order-independent set), each with the right `Code`; a valid order yields `nil`.
- Verify: `go test -count=1 -race ./...`

Set up the module:

```bash
mkdir -p ~/go-exercises/orderpath/cmd/demo
cd ~/go-exercises/orderpath
go mod init example.com/orderpath
go mod edit -go=1.26
```

### Build the path on the way down

The mistake to avoid is storing a bare field name and reconstructing the path
afterward — you cannot, because by the time you have the `*FieldError` you have
lost the position it came from. The fix is to pass a base prefix down the
recursion and extend it at each level: append `.field` for a struct member and
`.N` for the Nth element of a slice. When a leaf rule fails, the fully-qualified
path is already in hand.

The two builders are tiny but they are the whole idea. `child(base, field)`
joins with a dot unless `base` is empty (the root has no prefix). `index(base, i)`
appends `strconv.Itoa(i)` as a path segment. Both use `strings.Builder` so a deep
recursion does not allocate a new string per `+` concatenation.

```go
func child(base, field string) string {
	if base == "" {
		return field
	}
	var b strings.Builder
	b.WriteString(base)
	b.WriteByte('.')
	b.WriteString(field)
	return b.String()
}

func index(base string, i int) string {
	var b strings.Builder
	b.WriteString(base)
	b.WriteByte('.')
	b.WriteString(strconv.Itoa(i))
	return b.String()
}
```

`Validate` walks the tree: it validates the customer under prefix `customer`,
each line item under `items.N`, and the shipping address under `shipping`,
threading the prefix through every level. A blank email at the top of the
customer object becomes `customer.email`; a zero quantity on the second line item
becomes `items.1.qty`. That path is a contract: the frontend highlights exactly
that input.

Create `orderpath.go`:

```go
package orderpath

import (
	"errors"
	"fmt"
	"strconv"
	"strings"
)

type Code string

const (
	CodeRequired Code = "required"
	CodeRange    Code = "range"
)

type FieldError struct {
	Code  Code
	Field string
}

func (e *FieldError) Error() string {
	return fmt.Sprintf("%s: %s", e.Field, e.Code)
}

type ValidationError struct {
	Errors []*FieldError
}

func (e *ValidationError) Error() string {
	parts := make([]string, 0, len(e.Errors))
	for _, fe := range e.Errors {
		parts = append(parts, fe.Error())
	}
	return strings.Join(parts, "; ")
}

func (e *ValidationError) Unwrap() []error {
	out := make([]error, len(e.Errors))
	for i, fe := range e.Errors {
		out[i] = fe
	}
	return out
}

// child joins a base path with a struct field name using a dot.
func child(base, field string) string {
	if base == "" {
		return field
	}
	var b strings.Builder
	b.WriteString(base)
	b.WriteByte('.')
	b.WriteString(field)
	return b.String()
}

// index appends a slice index as a path segment: items -> items.2.
func index(base string, i int) string {
	var b strings.Builder
	b.WriteString(base)
	b.WriteByte('.')
	b.WriteString(strconv.Itoa(i))
	return b.String()
}

type Customer struct {
	Name  string
	Email string
}

type LineItem struct {
	SKU string
	Qty int
}

type ShippingAddress struct {
	Zip   string
	Line1 string
}

type CreateOrderRequest struct {
	Customer Customer
	Items    []LineItem
	Shipping ShippingAddress
}

func validateCustomer(base string, c Customer, errs *[]*FieldError) {
	if c.Name == "" {
		*errs = append(*errs, &FieldError{Code: CodeRequired, Field: child(base, "name")})
	}
	if c.Email == "" {
		*errs = append(*errs, &FieldError{Code: CodeRequired, Field: child(base, "email")})
	}
}

func validateItem(base string, it LineItem, errs *[]*FieldError) {
	if it.SKU == "" {
		*errs = append(*errs, &FieldError{Code: CodeRequired, Field: child(base, "sku")})
	}
	if it.Qty <= 0 {
		*errs = append(*errs, &FieldError{Code: CodeRange, Field: child(base, "qty")})
	}
}

func validateShipping(base string, s ShippingAddress, errs *[]*FieldError) {
	if s.Zip == "" {
		*errs = append(*errs, &FieldError{Code: CodeRequired, Field: child(base, "zip")})
	}
	if s.Line1 == "" {
		*errs = append(*errs, &FieldError{Code: CodeRequired, Field: child(base, "line1")})
	}
}

// Validate walks the nested request, threading a dotted prefix through every
// level so each FieldError carries a fully-qualified path.
func (r *CreateOrderRequest) Validate() error {
	var errs []*FieldError
	validateCustomer("customer", r.Customer, &errs)
	for i, it := range r.Items {
		validateItem(index("items", i), it, &errs)
	}
	validateShipping("shipping", r.Shipping, &errs)
	if len(errs) == 0 {
		return nil
	}
	return &ValidationError{Errors: errs}
}

// Paths returns the sorted set of failing field paths, for callers that want a
// stable list.
func Paths(err error) []string {
	var ve *ValidationError
	if !asValidation(err, &ve) {
		return nil
	}
	out := make([]string, 0, len(ve.Errors))
	for _, fe := range ve.Errors {
		out = append(out, fe.Field)
	}
	return out
}

// asValidation extracts a *ValidationError from a possibly-wrapped error. It is
// a thin wrapper over errors.As so the test file can use it without importing
// errors itself.
func asValidation(err error, target **ValidationError) bool {
	return errors.As(err, target)
}
```

### The runnable demo

Create `cmd/demo/main.go`:

```go
package main

import (
	"fmt"
	"sort"

	"example.com/orderpath"
)

func main() {
	req := &orderpath.CreateOrderRequest{
		Customer: orderpath.Customer{Name: "Alice", Email: ""},
		Items: []orderpath.LineItem{
			{SKU: "AAA", Qty: 2},
			{SKU: "BBB", Qty: 0},
			{SKU: "", Qty: 1},
		},
		Shipping: orderpath.ShippingAddress{Zip: "10001", Line1: "1 Main St"},
	}

	paths := orderpath.Paths(req.Validate())
	sort.Strings(paths)
	for _, p := range paths {
		fmt.Println(p)
	}
}
```

Run it:

```bash
go run ./cmd/demo
```

Expected output:

```
customer.email
items.1.qty
items.2.sku
```

### Tests

The key test constructs an order with exactly three defects placed so the
recursion must produce exactly those three paths, then compares the path set
order-independently (the recursion order is an implementation detail, the set is
the contract). A second test confirms a fully valid order returns `nil`. A third
checks a `Code` is attached to the right path.

Create `orderpath_test.go`:

```go
package orderpath

import (
	"testing"
)

func pathSet(err error) map[string]Code {
	var ve *ValidationError
	if !asValidation(err, &ve) {
		return nil
	}
	m := make(map[string]Code, len(ve.Errors))
	for _, fe := range ve.Errors {
		m[fe.Field] = fe.Code
	}
	return m
}

func TestNestedPaths(t *testing.T) {
	t.Parallel()

	req := &CreateOrderRequest{
		Customer: Customer{Name: "Alice", Email: ""},
		Items: []LineItem{
			{SKU: "AAA", Qty: 2},
			{SKU: "BBB", Qty: 0},
			{SKU: "", Qty: 1},
		},
		Shipping: ShippingAddress{Zip: "10001", Line1: "1 Main St"},
	}

	got := pathSet(req.Validate())
	want := map[string]Code{
		"customer.email": CodeRequired,
		"items.1.qty":    CodeRange,
		"items.2.sku":    CodeRequired,
	}

	if len(got) != len(want) {
		t.Fatalf("got %d errors %v, want %d %v", len(got), got, len(want), want)
	}
	for path, code := range want {
		if got[path] != code {
			t.Fatalf("path %q: code = %q, want %q", path, got[path], code)
		}
	}
}

func TestValidOrder(t *testing.T) {
	t.Parallel()

	req := &CreateOrderRequest{
		Customer: Customer{Name: "Alice", Email: "a@b.c"},
		Items:    []LineItem{{SKU: "AAA", Qty: 1}},
		Shipping: ShippingAddress{Zip: "10001", Line1: "1 Main St"},
	}

	if err := req.Validate(); err != nil {
		t.Fatalf("valid order -> %v, want nil", err)
	}
}

func TestIndexSegments(t *testing.T) {
	t.Parallel()

	if got := index("items", 2); got != "items.2" {
		t.Fatalf("index = %q, want items.2", got)
	}
	if got := child("items.2", "sku"); got != "items.2.sku" {
		t.Fatalf("child = %q, want items.2.sku", got)
	}
}
```

An `Example` verified against its `// Output:` comment:

```go
// orderpath_example_test.go
package orderpath

import (
	"fmt"
	"sort"
)

func ExampleCreateOrderRequest_Validate() {
	req := &CreateOrderRequest{
		Customer: Customer{Name: "", Email: ""},
		Items:    []LineItem{{SKU: "AAA", Qty: 1}},
		Shipping: ShippingAddress{Zip: "10001", Line1: "1 Main St"},
	}
	paths := Paths(req.Validate())
	sort.Strings(paths)
	fmt.Println(paths)
	// Output: [customer.email customer.name]
}
```

## Review

The validator is correct when the emitted path set is exactly the set of broken
leaves and each path is fully qualified from the root: a blank second-item
quantity is `items.1.qty`, never a bare `qty`, and never `items.qty`. The path
must be built compositionally on the way down — `index("items", i)` then
`child(prefix, "qty")` — because the position is only knowable during the walk.
The set comparison in the test is deliberate: the order the recursion appends
errors is not a contract, but the set of paths is. A valid order returns `nil`,
not an empty `*ValidationError`, so callers can `if err != nil` without unwrapping.
Run `go test -race`.

## Resources

- [`strings.Builder`](https://pkg.go.dev/strings#Builder) — allocation-free path assembly across a deep recursion.
- [`strconv.Itoa`](https://pkg.go.dev/strconv#Itoa) — the index-to-segment conversion.
- [`errors.As`](https://pkg.go.dev/errors#As) — extracting the `*ValidationError` from the returned error.

---

Back to [00-concepts.md](00-concepts.md) | Prev: [02-validationerror-unwrap-tree-traversal.md](02-validationerror-unwrap-tree-traversal.md) | Next: [04-validation-rule-registry.md](04-validation-rule-registry.md)
