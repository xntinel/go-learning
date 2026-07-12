# Exercise 8: Safely Walking a Nested Pointer Chain From a Decoded Payload

A webhook payload decoded from JSON routinely has optional nested objects: the
customer may be absent, or present without an address, or the address may carry
an empty country code. Reaching `payload.Customer.Address.CountryCode` in a
straight line panics the instant any hop is nil. This module builds a guarded
accessor that returns `(value, ok)` without panicking, and contrasts it with the
panic-prone dereference.

This module is fully self-contained.

## What you'll build

```text
webhook/                  independent module: example.com/webhook
  go.mod                  go 1.24
  webhook.go              nested *struct payload; CountryCode() (string,bool) guarded; naive deref
  cmd/
    demo/
      main.go             runnable demo: full payload vs missing customer
  webhook_test.go         each hop nil in turn -> ok=false no panic; happy path; naive panics
```

Files: `webhook.go`, `cmd/demo/main.go`, `webhook_test.go`.
Implement: a `Payload` whose `Customer`, `Address` are `*struct`; a guarded `CountryCode() (string, bool)` that early-returns on any nil hop; a naive function that dereferences straight through.
Test: a table with each intermediate nil in turn (nil Customer, nil Address, present-but-empty code) asserting `ok == false` without panic, plus the fully-populated happy path returning `ok == true`; the naive dereference panics on the same nil input (asserted with `recover`).
Verify: `go test -count=1 -race ./...`

Set up the module:

```bash
mkdir -p go-solutions/09-pointers/04-nil-pointers-and-guard-checks/08-nested-pointer-chain-guard/cmd/demo
cd go-solutions/09-pointers/04-nil-pointers-and-guard-checks/08-nested-pointer-chain-guard
go mod edit -go=1.24
```

### Each hop is independently nilable

When JSON decodes into pointer fields, an omitted nested object leaves its
pointer nil. So in `payload.Customer.Address.CountryCode`, three things can each
independently be nil: the payload itself, `Customer`, and `Address`. The
expression `payload.Customer.Address.CountryCode` desugars to
`(*(*(*payload).Customer).Address).CountryCode`; the first nil hop panics.

The guarded accessor walks the chain with a short-circuiting condition, returning
early the moment any hop is nil, and only reads the leaf value once every pointer
is known non-nil:

```go
func (p *Payload) CountryCode() (string, bool) {
	if p == nil || p.Customer == nil || p.Customer.Address == nil {
		return "", false
	}
	if p.Customer.Address.CountryCode == "" {
		return "", false // present but empty is treated as "no value"
	}
	return p.Customer.Address.CountryCode, true
}
```

Go's `||` short-circuits left to right, so `p.Customer.Address` is only evaluated
after `p.Customer == nil` has been ruled out — the guard order is load-bearing.
The accessor also folds "present but empty" into `ok == false`, because a blank
country code is not a usable value for a caller that needs to route by country;
distinguishing empty-string from absent would need a `*string` leaf, which is the
same absent-vs-zero decision from the PATCH exercise applied to the leaf.

The naive version is the code this replaces — it exists in the module only to
prove, under `recover`, that it panics on the exact input the guarded accessor
handles.

Create `webhook.go`:

```go
package webhook

// Address is an optional nested object; CountryCode may be empty.
type Address struct {
	CountryCode string `json:"country_code"`
}

// Customer is an optional nested object; Address may be nil.
type Customer struct {
	ID      string   `json:"id"`
	Address *Address `json:"address"`
}

// Payload is a decoded webhook whose nested objects are each independently
// optional (nil when the JSON key was absent).
type Payload struct {
	Event    string    `json:"event"`
	Customer *Customer `json:"customer"`
}

// CountryCode safely extracts payload.Customer.Address.CountryCode, returning
// ("", false) if any hop is nil or the code is empty, and (code, true) otherwise.
func (p *Payload) CountryCode() (string, bool) {
	if p == nil || p.Customer == nil || p.Customer.Address == nil {
		return "", false
	}
	if p.Customer.Address.CountryCode == "" {
		return "", false
	}
	return p.Customer.Address.CountryCode, true
}

// naiveCountryCode dereferences the whole chain in a straight line. It panics if
// any intermediate pointer is nil. Kept only to contrast with CountryCode.
func naiveCountryCode(p *Payload) string {
	return p.Customer.Address.CountryCode
}
```

### The runnable demo

Create `cmd/demo/main.go`:

```go
package main

import (
	"encoding/json"
	"fmt"

	"example.com/webhook"
)

func main() {
	full := []byte(`{"event":"order","customer":{"id":"c1","address":{"country_code":"US"}}}`)
	partial := []byte(`{"event":"order"}`)

	for _, raw := range [][]byte{full, partial} {
		var p webhook.Payload
		if err := json.Unmarshal(raw, &p); err != nil {
			fmt.Println("decode error:", err)
			continue
		}
		if code, ok := p.CountryCode(); ok {
			fmt.Printf("country: %s\n", code)
		} else {
			fmt.Println("country: unknown")
		}
	}
}
```

Run it:

```bash
go run ./cmd/demo
```

Expected output:

```
country: US
country: unknown
```

### Tests

The table drives each intermediate to nil in turn plus the present-but-empty leaf
and the happy path, all through the guarded accessor with no panic; a separate
test proves the naive dereference panics on the nil-customer input the accessor
handles gracefully.

Create `webhook_test.go`:

```go
package webhook

import (
	"encoding/json"
	"testing"
)

func TestCountryCodeGuarded(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name     string
		body     string
		wantCode string
		wantOK   bool
	}{
		{"full", `{"customer":{"address":{"country_code":"US"}}}`, "US", true},
		{"nil customer", `{"event":"x"}`, "", false},
		{"nil address", `{"customer":{"id":"c1"}}`, "", false},
		{"empty code", `{"customer":{"address":{"country_code":""}}}`, "", false},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			var p Payload
			if err := json.Unmarshal([]byte(tt.body), &p); err != nil {
				t.Fatal(err)
			}
			code, ok := p.CountryCode()
			if ok != tt.wantOK || code != tt.wantCode {
				t.Fatalf("CountryCode() = %q,%v; want %q,%v", code, ok, tt.wantCode, tt.wantOK)
			}
		})
	}
}

func TestNilPayloadReceiver(t *testing.T) {
	t.Parallel()

	var p *Payload // nil receiver
	if _, ok := p.CountryCode(); ok {
		t.Fatal("nil payload returned ok=true")
	}
}

func TestNaiveDerefPanics(t *testing.T) {
	t.Parallel()

	defer func() {
		if r := recover(); r == nil {
			t.Fatal("naive dereference did not panic on nil customer")
		}
	}()
	p := &Payload{Event: "x"} // Customer is nil
	_ = naiveCountryCode(p)   // panics: nil pointer dereference
}
```

## Review

The accessor is correct when it returns `ok == false` for every nil hop and for a
blank leaf, and `(code, true)` only when the full chain is present and the code is
non-empty. The table pins each hop; `TestNaiveDerefPanics` proves the panic the
guard prevents on the same input; `TestNilPayloadReceiver` covers the outermost
nil. Guard order matters — if the `||` chain checked `p.Customer.Address` before
`p.Customer == nil`, it would panic on a nil customer, so the test with "nil
address" would flake.

The mistake avoided: walking a nested pointer chain in a straight line and
trusting the decoder to have populated every level.

## Resources

- [encoding/json: Unmarshal](https://pkg.go.dev/encoding/json#Unmarshal) — how absent keys leave pointer fields nil.
- [Go Spec: Selectors](https://go.dev/ref/spec#Selectors) — how `x.f` on a pointer auto-dereferences and why a nil hop panics.
- [Go Spec: Logical operators](https://go.dev/ref/spec#Logical_operators) — `||` short-circuits, which the guard chain relies on.

---

Back to [00-concepts.md](00-concepts.md) | Prev: [07-atomic-pointer-hot-config-reload.md](07-atomic-pointer-hot-config-reload.md) | Next: [09-sql-null-scan-nullable-columns.md](09-sql-null-scan-nullable-columns.md)
