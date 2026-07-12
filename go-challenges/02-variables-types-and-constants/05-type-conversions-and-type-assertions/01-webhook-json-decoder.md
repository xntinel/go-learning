# Exercise 1: Decode An Untrusted Webhook Into A Typed Event

A webhook handler receives a JSON body you did not construct. Decoding it into
`map[string]any` gives you dynamic values that must be asserted into concrete Go
types before the rest of the service can trust them. This is the canonical
boundary where comma-ok assertions, a number policy, and a safe narrowing all
matter at once.

This module is fully self-contained: its own module, all code inline, its own
demo and tests.

## What you'll build

```text
webhookdecode/               independent module: example.com/webhookdecode
  go.mod                     go 1.26
  decoder.go                 type Event; Decode(io.Reader); field helpers with comma-ok
  cmd/
    demo/
      main.go                runnable demo: decode a good and a bad payload
  decoder_test.go            happy-path DeepEqual + subtests for malformed payloads
```

- Files: `decoder.go`, `cmd/demo/main.go`, `decoder_test.go`.
- Implement: `Decode(r io.Reader) (Event, error)` using `json.NewDecoder` with `UseNumber`, comma-ok assertions per field, and a platform-correct `int64`→`int` narrowing.
- Test: one happy-path `reflect.DeepEqual`, plus subtests asserting an error for missing id, string attempts, fractional price, mixed tags, and negative attempts.
- Verify: `go test -count=1 -race ./...`

Set up the module:

```bash
go mod edit -go=1.26
```

### Why UseNumber and comma-ok are both mandatory here

The body arrives as bytes. `json.Decoder.Decode(&payload)` into a
`map[string]any` gives you a map whose values are `string`, `bool`, `float64` (or
`json.Number`), `[]any`, `map[string]any`, or `nil` — never your domain types.
Two independent hazards live in that map.

First, numbers. By default `encoding/json` decodes every number into `float64`,
which cannot exactly represent a 64-bit id or an exact monetary amount above
`2^53`. Calling `decoder.UseNumber()` changes the policy so numbers arrive as
`json.Number` (a string wrapper), and you decide per field whether to
`ParseInt`, `ParseFloat`, or reject. A `price_cents` of `12.99` must be rejected
because cents are integers; `ParseInt` on `"12.99"` fails, which is exactly the
controlled failure you want.

Second, types. `payload["id"].(string)` in single-value form panics the moment a
caller sends `"id": 7`. Every field helper here uses the comma-ok form and turns
a mismatch into an error that names the field and prints the actual type with
`%T`. The narrowing from `int64` to `int` is guarded by `int(^uint(0) >> 1)`, the
maximum `int` on the current platform, so `attempts` can never wrap on a 32-bit
build, and a negative `attempts` is rejected before any conversion runs.

Create `decoder.go`. Note it calls `json.Number.String` and `strconv.ParseInt`,
never `.(int)`:

```go
// decoder.go
package webhookdecode

import (
	"encoding/json"
	"fmt"
	"io"
	"strconv"
)

// Event is the typed form the rest of the service consumes.
type Event struct {
	ID         string
	Attempts   int
	PriceCents int64
	Tags       []string
}

// Decode reads one untrusted webhook body and converts it into an Event,
// rejecting any field whose dynamic type or value does not fit.
func Decode(r io.Reader) (Event, error) {
	dec := json.NewDecoder(r)
	dec.UseNumber() // numbers arrive as json.Number, not float64

	var payload map[string]any
	if err := dec.Decode(&payload); err != nil {
		return Event{}, fmt.Errorf("decode webhook: %w", err)
	}

	id, err := stringField(payload, "id")
	if err != nil {
		return Event{}, err
	}
	attempts, err := intField(payload, "attempts")
	if err != nil {
		return Event{}, err
	}
	priceCents, err := int64Field(payload, "price_cents")
	if err != nil {
		return Event{}, err
	}
	tags, err := stringSliceField(payload, "tags")
	if err != nil {
		return Event{}, err
	}

	return Event{ID: id, Attempts: attempts, PriceCents: priceCents, Tags: tags}, nil
}

func stringField(payload map[string]any, key string) (string, error) {
	v, ok := payload[key]
	if !ok {
		return "", fmt.Errorf("missing field %q", key)
	}
	s, ok := v.(string)
	if !ok {
		return "", fmt.Errorf("field %q must be string, got %T", key, v)
	}
	if s == "" {
		return "", fmt.Errorf("field %q must not be empty", key)
	}
	return s, nil
}

// int64Field asserts json.Number and parses it, so 12.99 for an integer field
// fails at ParseInt instead of silently becoming a float.
func int64Field(payload map[string]any, key string) (int64, error) {
	v, ok := payload[key]
	if !ok {
		return 0, fmt.Errorf("missing field %q", key)
	}
	num, ok := v.(json.Number)
	if !ok {
		return 0, fmt.Errorf("field %q must be number, got %T", key, v)
	}
	n, err := strconv.ParseInt(num.String(), 10, 64)
	if err != nil {
		return 0, fmt.Errorf("field %q must be an integer: %w", key, err)
	}
	return n, nil
}

// intField narrows int64 to int with a platform-correct range check and
// rejects negatives before the conversion.
func intField(payload map[string]any, key string) (int, error) {
	n, err := int64Field(payload, key)
	if err != nil {
		return 0, err
	}
	maxInt := int64(int(^uint(0) >> 1)) // platform max int (2^31-1 or 2^63-1)
	if n < 0 || n > maxInt {
		return 0, fmt.Errorf("field %q out of int range: %d", key, n)
	}
	return int(n), nil
}

func stringSliceField(payload map[string]any, key string) ([]string, error) {
	v, ok := payload[key]
	if !ok {
		return nil, fmt.Errorf("missing field %q", key)
	}
	raw, ok := v.([]any)
	if !ok {
		return nil, fmt.Errorf("field %q must be array, got %T", key, v)
	}
	out := make([]string, 0, len(raw))
	for i, item := range raw {
		s, ok := item.(string)
		if !ok {
			return nil, fmt.Errorf("field %q[%d] must be string, got %T", key, i, item)
		}
		out = append(out, s)
	}
	return out, nil
}
```

### The runnable demo

The demo decodes one valid payload and one with a fractional `price_cents`, so
you can see the boundary accept the first and reject the second by name.

Create `cmd/demo/main.go`:

```go
// cmd/demo/main.go
package main

import (
	"fmt"
	"strings"

	"example.com/webhookdecode"
)

func main() {
	good := `{"id":"evt_123","attempts":2,"price_cents":1299,"tags":["paid","priority"]}`
	ev, err := webhookdecode.Decode(strings.NewReader(good))
	if err != nil {
		fmt.Println("unexpected error:", err)
		return
	}
	fmt.Printf("ok: id=%s attempts=%d price=%d tags=%v\n",
		ev.ID, ev.Attempts, ev.PriceCents, ev.Tags)

	bad := `{"id":"evt_123","attempts":2,"price_cents":12.99,"tags":[]}`
	if _, err := webhookdecode.Decode(strings.NewReader(bad)); err != nil {
		fmt.Println("rejected:", err)
	}
}
```

Run it:

```bash
go run ./cmd/demo
```

Expected output:

```
ok: id=evt_123 attempts=2 price=1299 tags=[paid priority]
rejected: field "price_cents" must be an integer: strconv.ParseInt: parsing "12.99": invalid syntax
```

### Tests

The happy path asserts the whole `Event` with `reflect.DeepEqual`. The rejection
table proves each failure mode is caught: a missing id, an attempts sent as a
string, a fractional price, a mixed-type tags array, and a negative attempts that
is rejected before the `int` conversion.

Create `decoder_test.go`:

```go
// decoder_test.go
package webhookdecode

import (
	"reflect"
	"strings"
	"testing"
)

func TestDecodeHappyPath(t *testing.T) {
	t.Parallel()
	in := strings.NewReader(`{
		"id":"evt_123",
		"attempts":2,
		"price_cents":1299,
		"tags":["paid","priority"]
	}`)

	got, err := Decode(in)
	if err != nil {
		t.Fatalf("Decode: %v", err)
	}
	want := Event{ID: "evt_123", Attempts: 2, PriceCents: 1299, Tags: []string{"paid", "priority"}}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("Event = %#v, want %#v", got, want)
	}
}

func TestDecodeRejects(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name  string
		input string
	}{
		{"missing id", `{"attempts":1,"price_cents":1299,"tags":[]}`},
		{"attempts string", `{"id":"evt_1","attempts":"2","price_cents":1299,"tags":[]}`},
		{"fractional cents", `{"id":"evt_1","attempts":2,"price_cents":12.99,"tags":[]}`},
		{"mixed tags", `{"id":"evt_1","attempts":2,"price_cents":1299,"tags":["paid",7]}`},
		{"negative attempts", `{"id":"evt_1","attempts":-1,"price_cents":1299,"tags":[]}`},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			if _, err := Decode(strings.NewReader(tt.input)); err == nil {
				t.Fatalf("Decode(%s) = nil error, want rejection", tt.name)
			}
		})
	}
}
```

## Review

The decoder is correct when no path through it can panic on hostile input and
every rejection names the offending field. The single most important property is
that the number fields never touch `.(int)` or `.(float64)`: with `UseNumber`
they are `json.Number`, parsed with `ParseInt`, so a fractional value for an
integer field fails loudly at parse time rather than rounding. The narrowing
guard uses `int(^uint(0) >> 1)` rather than a hardcoded `math.MaxInt64`, so the
range check is correct on a 32-bit target as well; the negative-attempts subtest
exists precisely to prove the value is rejected before `int(n)` runs. If you find
yourself reaching for the single-value assertion form anywhere in this file,
stop — a webhook body is the definition of untrusted input.

## Resources

- [Go Specification: Type assertions](https://go.dev/ref/spec#Type_assertions) — the comma-ok form and panic semantics.
- [encoding/json Decoder.UseNumber](https://pkg.go.dev/encoding/json#Decoder.UseNumber) — deferring the number-decoding policy to your code.
- [json.Number](https://pkg.go.dev/encoding/json#Number) — the string-backed number type and its `Int64`/`Float64`/`String` methods.
- [strconv.ParseInt](https://pkg.go.dev/strconv#ParseInt) — base and bit-size controlled integer parsing.

---

Back to [00-concepts.md](00-concepts.md) | Next: [02-error-type-switch-retry-classifier.md](02-error-type-switch-retry-classifier.md)
