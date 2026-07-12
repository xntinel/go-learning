# Exercise 11: Join Multiple Field Errors Behind One Named err

An order has several fields to validate — quantity, price, email — and a caller
wants to see every problem at once, not just the first one hit. The function keeps
checking after the first bad field, collects the failures, and joins them into one
named `err` that a single deferred closure prefixes with the order ID exactly once.

**Nivel: Intermedio** — validacion rapida (un test corto).

## What you'll build

```text
ordervalidate/              independent module: example.com/ordervalidate
  go.mod
  ordervalidate.go          Order; Validate (collects, joins, defers one wrap)
  ordervalidate_test.go     valid order, one bad field, several bad fields
```

- Files: `ordervalidate.go`, `ordervalidate_test.go`.
- Implement: `Validate(o Order) (err error)` that checks every field without stopping at the first failure, joins the collected field errors with `errors.Join`, and uses a deferred closure keyed on the named `err` to prefix the order ID once.
- Test: a valid order returns nil; a single bad field reports just that field; several bad fields all appear in one error.
- Verify: `go test -count=1 ./...`

Set up the module:

```bash
go mod edit -go=1.24
```

### Collect first, wrap once

`Validate` runs every check regardless of earlier failures, appending to a local
slice, then joins whatever accumulated. The deferred closure only ever sees the
final, joined `err` — it does not know or care how many fields failed:

```go
defer func() {
    if err != nil {
        err = fmt.Errorf("validate order %s: %w", o.ID, err)
    }
}()
```

That single line is the only place the order ID appears. Without a named `err` the
same prefix would have to be duplicated after every possible early return, and a
new field check added later would be one more place to forget it.

Create `ordervalidate.go`:

```go
package ordervalidate

import (
	"errors"
	"fmt"
	"strings"
)

// Order is the input to Validate.
type Order struct {
	ID    string
	Qty   int
	Price float64
	Email string
}

// Validate checks every field independently — it does not stop at the first
// bad one — and joins all field failures into a single error, prefixed once
// with the order ID by a deferred closure keyed on the named err.
func Validate(o Order) (err error) {
	defer func() {
		if err != nil {
			err = fmt.Errorf("validate order %s: %w", o.ID, err)
		}
	}()

	var fieldErrs []error
	if o.Qty <= 0 {
		fieldErrs = append(fieldErrs, fmt.Errorf("qty must be positive, got %d", o.Qty))
	}
	if o.Price < 0 {
		fieldErrs = append(fieldErrs, fmt.Errorf("price must not be negative, got %.2f", o.Price))
	}
	if !strings.Contains(o.Email, "@") {
		fieldErrs = append(fieldErrs, fmt.Errorf("email %q missing @", o.Email))
	}

	if len(fieldErrs) > 0 {
		err = errors.Join(fieldErrs...)
	}
	return
}
```

### Tests

Create `ordervalidate_test.go`:

```go
package ordervalidate

import (
	"strings"
	"testing"
)

func TestValidate(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name    string
		order   Order
		wantErr bool
		contain []string
	}{
		{
			name:    "valid order passes",
			order:   Order{ID: "o1", Qty: 2, Price: 9.99, Email: "a@b.com"},
			wantErr: false,
		},
		{
			name:    "single bad field wraps once",
			order:   Order{ID: "o2", Qty: -1, Price: 5, Email: "a@b.com"},
			wantErr: true,
			contain: []string{"validate order o2", "qty must be positive"},
		},
		{
			name:    "multiple bad fields all reported",
			order:   Order{ID: "o3", Qty: 0, Price: -1, Email: "no-at-sign"},
			wantErr: true,
			contain: []string{
				"validate order o3",
				"qty must be positive",
				"price must not be negative",
				`email "no-at-sign" missing @`,
			},
		},
	}

	for _, tt := range tests {
		tt := tt
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			err := Validate(tt.order)
			if tt.wantErr && err == nil {
				t.Fatal("Validate: want error, got nil")
			}
			if !tt.wantErr && err != nil {
				t.Fatalf("Validate: unexpected error: %v", err)
			}
			for _, want := range tt.contain {
				if !strings.Contains(err.Error(), want) {
					t.Errorf("error %q does not contain %q", err.Error(), want)
				}
			}
		})
	}
}
```

Verify: `go test -count=1 ./...`

## Review

`Validate` demonstrates a different accumulation mechanic than a single early-return
wrap: the body never returns early on the first bad field, so by the time the
deferred closure runs, `err` already holds every failure `errors.Join` combined.
The defer contributes exactly one thing — the order ID prefix — and it would need
to be pasted at every early exit without a named result to key on. The mistake to
avoid is calling `errors.Join` inside the defer itself, which would require passing
the field-error slice out of the body through a second named result instead of
settling it before the defer ever runs.

## Resources

- [`errors.Join`](https://pkg.go.dev/errors#Join)
- [Go Spec: Defer statements](https://go.dev/ref/spec#Defer_statements)

---

Back to [00-concepts.md](00-concepts.md) | Prev: [10-acquire-all-or-none-cleanup.md](10-acquire-all-or-none-cleanup.md) | Next: [12-retry-loop-attempt-count.md](12-retry-loop-attempt-count.md)
