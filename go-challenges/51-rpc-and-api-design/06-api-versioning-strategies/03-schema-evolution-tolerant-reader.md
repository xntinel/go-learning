# Exercise 3: Schema Evolution and the Tolerant Reader

The best version is the one you never ship. This exercise builds the in-band
alternative to bumping versions: evolve one JSON contract compatibly. You add a
field, rename one behind an alias, drop another, migrate v1 to v2, and decode the
same wire bytes two ways — tolerantly for the public edge and strictly for
internal callers.

This module is fully self-contained: its own `go mod init`, code, demo, and tests.

## What you'll build

```text
schemaevolution/               independent module: example.com/schemaevolution
  go.mod                       go 1.24
  schemaevolution.go           OrderV1, OrderV2 (alias UnmarshalJSON), MigrateV1toV2,
                               Envelope (RawMessage), DecodeOrder, DecodeTolerant/Strict
  cmd/
    demo/
      main.go                  decode a v1 and a v2 envelope; tolerant vs strict read
  schemaevolution_test.go      round-trip tables + omitempty golden + Example
```

- Files: `schemaevolution.go`, `cmd/demo/main.go`, `schemaevolution_test.go`.
- Implement: `OrderV1` and `OrderV2` (v2 adds `currency`, renames `customer` to `customer_id` via a tolerant `UnmarshalJSON`, drops `note`), a `MigrateV1toV2`, an `Envelope` using `json.RawMessage` to defer the payload, a `DecodeOrder` that dispatches on the schema discriminator, and generic `DecodeTolerant` / `DecodeStrict`.
- Test: a v1 payload decodes into v2 with the alias and defaults; a v2 payload decodes into v1 tolerantly (unknown keys ignored); the same v2 payload is rejected by the strict decoder; the migration maps the renamed field; `omitempty` keeps an unset field out of the golden output; an unknown schema returns a wrapped sentinel.
- Verify: `go test -count=1 -race ./...`

### Additive change is free; renames need an alias

`OrderV2` differs from `OrderV1` in three ways, each illustrating a rule from the
concepts. It *adds* `currency` — safe, because old clients ignore an unknown key
and new clients default it when it is absent, tagged `omitempty` so it never
appears when empty. It *drops* `note` — the reader for `note` simply stops seeing
it; because `note` was optional this breaks no one. And it *renames* `customer` to
`customer_id`, which is the dangerous one: a plain rename would break every payload
still written with `customer`.

The fix is a tolerant `UnmarshalJSON` on `OrderV2` that reads the new key or the
legacy one. The idiom is a local `type alias OrderV2`, which sheds the method set
so the inner `json.Unmarshal` does not recurse into `UnmarshalJSON` forever, plus
an embedded pointer to that alias and one extra field for the legacy `customer`
key. After unmarshalling, if `CustomerID` was not set from the new key but the
legacy field was, it copies the legacy value across. That dual-read window is what
lets you rename a field without a new version.

### The envelope and deferred decode

`Envelope` carries a `schema_version` discriminator alongside a
`json.RawMessage` payload. `RawMessage` captures the payload bytes *without*
decoding them, so `DecodeOrder` can read the discriminator first and only then
unmarshal the payload into the right struct — a v1 payload is decoded as `OrderV1`
and migrated, a v2 payload straight into `OrderV2`, and an unknown version returns
`ErrUnknownSchema` wrapped with `%w`. Deferring the decode is what makes a single
endpoint able to accept multiple schema versions on the wire.

### Tolerant vs strict on the same bytes

`DecodeTolerant` is a plain `json.Unmarshal`: unknown keys are silently ignored,
which is the forward-compatibility default for anything a partner sends you.
`DecodeStrict` builds a `json.Decoder` and calls `DisallowUnknownFields`, so an
unexpected key becomes an error. The point of showing both on the *same* bytes is
that the choice is a policy, not a property of the data: strict decoding belongs on
internal boundaries where an unknown key is a bug, and using it on public ingress
turns every additive change a partner makes into a 400.

Create `schemaevolution.go`:

```go
package schemaevolution

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
)

// ErrUnknownSchema is returned when an envelope names a schema version this
// service does not know how to decode.
var ErrUnknownSchema = errors.New("unknown schema version")

// OrderV1 is the original contract. Note is an optional field that v2 drops.
type OrderV1 struct {
	ID       string `json:"id"`
	Customer string `json:"customer"`
	Total    int    `json:"total_cents"`
	Note     string `json:"note,omitempty"`
}

// OrderV2 evolves the contract additively: it adds Currency, renames Customer
// to CustomerID (still readable from the legacy "customer" key via a tolerant
// UnmarshalJSON), and drops Note.
type OrderV2 struct {
	ID         string `json:"id"`
	CustomerID string `json:"customer_id"`
	Total      int    `json:"total_cents"`
	Currency   string `json:"currency,omitempty"`
}

// UnmarshalJSON reads OrderV2 from either the new "customer_id" key or the
// legacy "customer" key, so a v2 reader stays backward compatible with wire
// bytes written against v1's field name.
func (o *OrderV2) UnmarshalJSON(data []byte) error {
	type alias OrderV2 // shed the method set to avoid infinite recursion
	aux := struct {
		*alias
		LegacyCustomer string `json:"customer"`
	}{alias: (*alias)(o)}
	if err := json.Unmarshal(data, &aux); err != nil {
		return err
	}
	if o.CustomerID == "" && aux.LegacyCustomer != "" {
		o.CustomerID = aux.LegacyCustomer
	}
	return nil
}

// MigrateV1toV2 maps a v1 value onto the v2 shape, carrying the renamed field
// and defaulting the newly added Currency.
func MigrateV1toV2(v1 OrderV1) OrderV2 {
	return OrderV2{
		ID:         v1.ID,
		CustomerID: v1.Customer,
		Total:      v1.Total,
		Currency:   "USD",
	}
}

// Envelope carries a schema-version discriminator alongside a deferred payload.
// json.RawMessage postpones decoding the payload until the version is known.
type Envelope struct {
	SchemaVersion string          `json:"schema_version"`
	Payload       json.RawMessage `json:"payload"`
}

// DecodeOrder reads the envelope discriminator and decodes the payload into the
// current (v2) shape, migrating a v1 payload on the way.
func DecodeOrder(data []byte) (OrderV2, error) {
	var env Envelope
	if err := json.Unmarshal(data, &env); err != nil {
		return OrderV2{}, fmt.Errorf("decode envelope: %w", err)
	}
	switch env.SchemaVersion {
	case "v1":
		var v1 OrderV1
		if err := json.Unmarshal(env.Payload, &v1); err != nil {
			return OrderV2{}, fmt.Errorf("decode v1 payload: %w", err)
		}
		return MigrateV1toV2(v1), nil
	case "v2":
		var v2 OrderV2
		if err := json.Unmarshal(env.Payload, &v2); err != nil {
			return OrderV2{}, fmt.Errorf("decode v2 payload: %w", err)
		}
		return v2, nil
	default:
		return OrderV2{}, fmt.Errorf("decode order: %w: %q", ErrUnknownSchema, env.SchemaVersion)
	}
}

// DecodeTolerant unmarshals into v, silently ignoring unknown fields. This is
// the forward-compatibility default for public ingress.
func DecodeTolerant[T any](data []byte, v *T) error {
	return json.Unmarshal(data, v)
}

// DecodeStrict unmarshals into v but rejects any key not present in T. Use it on
// internal boundaries where an unexpected field is a bug, never on the public
// edge where it would turn a partner's additive change into a 400.
func DecodeStrict[T any](data []byte, v *T) error {
	dec := json.NewDecoder(bytes.NewReader(data))
	dec.DisallowUnknownFields()
	return dec.Decode(v)
}
```

### The runnable demo

The demo decodes a v1 and a v2 envelope through the same `DecodeOrder`, then reads
a v2 body with both the tolerant and the strict decoder to show one accept and one
reject.

Create `cmd/demo/main.go`:

```go
package main

import (
	"encoding/json"
	"fmt"

	"example.com/schemaevolution"
)

func main() {
	v1Envelope := []byte(`{"schema_version":"v1","payload":{"id":"ord-1","customer":"alice","total_cents":1999,"note":"gift"}}`)
	v2Envelope := []byte(`{"schema_version":"v2","payload":{"id":"ord-2","customer_id":"bob","total_cents":4200,"currency":"EUR"}}`)

	for _, env := range [][]byte{v1Envelope, v2Envelope} {
		order, err := schemaevolution.DecodeOrder(env)
		if err != nil {
			fmt.Println("error:", err)
			continue
		}
		out, _ := json.Marshal(order)
		fmt.Printf("decoded -> %s\n", out)
	}

	// A v2 body read by an old v1 tolerant reader: unknown keys are ignored.
	v2Body := []byte(`{"id":"ord-2","customer_id":"bob","total_cents":4200,"currency":"EUR"}`)
	var asV1 schemaevolution.OrderV1
	if err := schemaevolution.DecodeTolerant(v2Body, &asV1); err != nil {
		fmt.Println("tolerant error:", err)
	}
	fmt.Printf("tolerant v1 read of v2 body: id=%q total_cents=%d\n", asV1.ID, asV1.Total)

	// The same body under a strict internal decoder is rejected.
	if err := schemaevolution.DecodeStrict(v2Body, new(schemaevolution.OrderV1)); err != nil {
		fmt.Printf("strict rejects unknown field: %v\n", err)
	}
}
```

Run it:

```bash
go run ./cmd/demo
```

Expected output:

```
decoded -> {"id":"ord-1","customer_id":"alice","total_cents":1999,"currency":"USD"}
decoded -> {"id":"ord-2","customer_id":"bob","total_cents":4200,"currency":"EUR"}
tolerant v1 read of v2 body: id="ord-2" total_cents=4200
strict rejects unknown field: json: unknown field "customer_id"
```

### Tests

The tests are round-trip tables that prove each compatibility claim.
`TestV1PayloadIntoV2Struct` reads a legacy `customer` key through the alias and
checks the added field defaults. `TestV2PayloadIntoV1TolerantIgnoresUnknown` proves
additive change is non-breaking for old readers. `TestStrictRejectsUnknownField`
shows the same bytes rejected by `DisallowUnknownFields`. `TestMigrateV1toV2`
checks the rename mapping. `TestDecodeOrderUnknownSchema` asserts the wrapped
sentinel via `errors.Is`. `TestOmitemptyGolden` pins that an unset optional field
stays out of the encoded form.

Create `schemaevolution_test.go`:

```go
package schemaevolution

import (
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"testing"
)

func TestV1PayloadIntoV2Struct(t *testing.T) {
	t.Parallel()
	// A v1 payload (legacy "customer" key, no currency) decoded into the v2
	// struct: the renamed field is read via the alias and the added field takes
	// its zero value.
	data := []byte(`{"id":"ord-1","customer":"alice","total_cents":1999}`)
	var v2 OrderV2
	if err := json.Unmarshal(data, &v2); err != nil {
		t.Fatalf("Unmarshal: %v", err)
	}
	if v2.CustomerID != "alice" {
		t.Fatalf("CustomerID = %q; want alice (read from legacy key)", v2.CustomerID)
	}
	if v2.Currency != "" {
		t.Fatalf("Currency = %q; want empty default", v2.Currency)
	}
}

func TestV2PayloadIntoV1TolerantIgnoresUnknown(t *testing.T) {
	t.Parallel()
	// The tolerant reader proves an additive change is non-breaking: a v1
	// client reading a v2 body keeps the fields it knows and ignores the rest.
	data := []byte(`{"id":"ord-2","customer_id":"bob","total_cents":4200,"currency":"EUR"}`)
	var v1 OrderV1
	if err := DecodeTolerant(data, &v1); err != nil {
		t.Fatalf("DecodeTolerant: %v", err)
	}
	if v1.ID != "ord-2" || v1.Total != 4200 {
		t.Fatalf("v1 = %+v; want ID=ord-2 Total=4200", v1)
	}
}

func TestStrictRejectsUnknownField(t *testing.T) {
	t.Parallel()
	data := []byte(`{"id":"ord-2","customer_id":"bob","total_cents":4200,"currency":"EUR"}`)
	err := DecodeStrict(data, new(OrderV1))
	if err == nil {
		t.Fatal("DecodeStrict accepted unknown fields; want error")
	}
	if !strings.Contains(err.Error(), "unknown field") {
		t.Fatalf("err = %v; want an unknown-field error", err)
	}
}

func TestMigrateV1toV2(t *testing.T) {
	t.Parallel()
	v1 := OrderV1{ID: "ord-9", Customer: "carol", Total: 500, Note: "dropped"}
	got := MigrateV1toV2(v1)
	want := OrderV2{ID: "ord-9", CustomerID: "carol", Total: 500, Currency: "USD"}
	if got != want {
		t.Fatalf("MigrateV1toV2 = %+v; want %+v", got, want)
	}
}

func TestDecodeOrderUnknownSchema(t *testing.T) {
	t.Parallel()
	_, err := DecodeOrder([]byte(`{"schema_version":"v3","payload":{}}`))
	if !errors.Is(err, ErrUnknownSchema) {
		t.Fatalf("err = %v; want ErrUnknownSchema", err)
	}
}

func TestOmitemptyGolden(t *testing.T) {
	t.Parallel()
	// omitempty keeps the optional/added field out of the wire form when unset,
	// which is what makes dropping or defaulting a field non-breaking.
	const golden = `{"id":"ord-3","customer_id":"dave","total_cents":100}`
	out, err := json.Marshal(OrderV2{ID: "ord-3", CustomerID: "dave", Total: 100})
	if err != nil {
		t.Fatalf("Marshal: %v", err)
	}
	if string(out) != golden {
		t.Fatalf("Marshal = %s; want %s", out, golden)
	}
}

func ExampleDecodeOrder() {
	env := []byte(`{"schema_version":"v1","payload":{"id":"ord-1","customer":"alice","total_cents":1999}}`)
	order, err := DecodeOrder(env)
	if err != nil {
		fmt.Println(err)
		return
	}
	out, _ := json.Marshal(order)
	fmt.Println(string(out))
	// Output: {"id":"ord-1","customer_id":"alice","total_cents":1999,"currency":"USD"}
}
```

## Review

The contract evolves correctly when additive change costs nothing and the one
rename is invisible to old writers. The failures to avoid: writing a naive
`UnmarshalJSON` that calls `json.Unmarshal(data, o)` on the receiver's real type
and recurses forever (the `type alias` trick exists to break that); reaching for
`DisallowUnknownFields` on public ingress, which converts a partner's harmless new
field into a 400; and forgetting `omitempty`, which leaks zero-valued optional
fields into the wire form and can itself become a compatibility surprise. The
golden test is the cheapest guard against that last one. Run `go test -race` to
confirm the decoders share no mutable state.

## Resources

- [`encoding/json.Decoder.DisallowUnknownFields`](https://pkg.go.dev/encoding/json#Decoder.DisallowUnknownFields) — strict decoding for internal boundaries.
- [`encoding/json.RawMessage`](https://pkg.go.dev/encoding/json#RawMessage) — deferring payload decode behind a discriminator.
- [Protocol Buffers — proto3 language guide](https://protobuf.dev/programming-guides/proto3/) — the canonical field-number evolution rules (`reserved`, additive-only).

---

Back to [00-concepts.md](00-concepts.md) | Previous: [02-deprecation-sunset-lifecycle.md](02-deprecation-sunset-lifecycle.md) | Next: [../07-rpc-style-tradeoffs/00-concepts.md](../07-rpc-style-tradeoffs/00-concepts.md)
