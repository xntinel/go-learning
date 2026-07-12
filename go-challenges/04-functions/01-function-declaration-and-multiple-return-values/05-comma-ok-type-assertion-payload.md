# Exercise 5: Comma-Ok Type Assertions On A Dynamic Webhook Payload

Webhooks arrive as untyped JSON. Decoded into `map[string]any`, every field is an
`any` you must assert to a concrete type before using — and the single-return
assertion `v := x.(T)` *panics* on a mismatch. This exercise builds a generic
`Field[T any](payload, key) (T, bool)` around the safe comma-ok assertion, the
second canonical `(value, ok)` site after the map index.

This module is fully self-contained: its own `go mod init`, all code inline, its
own demo and tests.

## What you'll build

```text
webhook/                   independent module: example.com/webhook
  go.mod                   go 1.25
  webhook.go               Field[T any](map[string]any, key) (T, bool); Decode helper
  cmd/
    demo/
      main.go              decodes a webhook body and extracts typed fields
  webhook_test.go          string/number/bool extraction; no-panic on mismatch; JSON int is float64; -race
```

- Files: `webhook.go`, `cmd/demo/main.go`, `webhook_test.go`.
- Implement: `Field[T any](payload map[string]any, key string) (T, bool)` using `v, ok := payload[key].(T)`; a `Decode([]byte) (map[string]any, error)` helper.
- Test: `Field[string]` on a string key returns `(v, true)`; `Field[float64]` on a number returns `(n, true)`; `Field[string]` on a number returns `("", false)` with no panic; a missing key returns the zero value and false; a regression test documents that JSON integers decode as `float64`.
- Verify: `go test -count=1 -race ./...`

Set up the module:

```bash
mkdir -p go-solutions/04-functions/01-function-declaration-and-multiple-return-values/05-comma-ok-type-assertion-payload/cmd/demo
cd go-solutions/04-functions/01-function-declaration-and-multiple-return-values/05-comma-ok-type-assertion-payload
go mod edit -go=1.25
```

### The two-return assertion is the safe one

A type assertion has two forms. The single-return form `v := x.(T)` *panics* if
`x` does not hold a `T`. The two-return form `v, ok := x.(T)` never panics: on a
mismatch it returns the zero `T` and `false`. On anything you did not produce
yourself — an `any` from a decoded JSON map, an `interface{}` from a plugin
boundary — you must use the comma-ok form, because you cannot prove the dynamic
type at compile time. This is the same `(value, ok)` contract as the map index,
and for the same reason: absence (here, "wrong type") is a normal, branchable
outcome, not a crash.

`Field` combines the two `(value, ok)` sites in one expression. `payload[key]` is
a map index: if the key is missing it yields the zero `any` (a nil interface). The
`.(T)` then asserts the type. When the key is missing, `nil.(T)` fails the
assertion and returns `(zero, false)` — so one comma-ok expression handles both
"missing key" and "wrong type" and never panics. Generics keep it type-safe:
`Field[string]` returns a `string`, `Field[float64]` a `float64`, with no `any` for
the caller to re-assert.

### The float64 trap

`encoding/json` decodes every JSON number as `float64` when the target is
`any`/`map[string]any` — there is no `int` in the decoded map, even for `42`. So
`Field[int](payload, "count")` on a JSON `42` returns `(0, false)`: the dynamic
type is `float64`, not `int`, and the assertion fails. This is the single most
common webhook-parsing bug. The fix is to assert `float64` and convert, or to
decode into a typed struct, or to use `json.Number`. The regression test below
pins this behavior so nobody "fixes" `Field` to magically coerce.

Create `webhook.go`:

```go
package webhook

import "encoding/json"

// Decode parses a webhook body into an untyped map. Every JSON number in the
// result is a float64, every object a map[string]any, every array a []any.
func Decode(body []byte) (map[string]any, error) {
	var m map[string]any
	if err := json.Unmarshal(body, &m); err != nil {
		return nil, err
	}
	return m, nil
}

// Field extracts payload[key] as a T using a comma-ok type assertion. It returns
// (zero, false) for a missing key OR a type mismatch, and never panics.
func Field[T any](payload map[string]any, key string) (T, bool) {
	v, ok := payload[key].(T)
	return v, ok
}
```

### The runnable demo

Create `cmd/demo/main.go`:

```go
package main

import (
	"fmt"

	"example.com/webhook"
)

func main() {
	body := []byte(`{"event":"charge.succeeded","amount":4200,"livemode":true}`)
	payload, err := webhook.Decode(body)
	if err != nil {
		panic(err)
	}

	event, _ := webhook.Field[string](payload, "event")
	amount, _ := webhook.Field[float64](payload, "amount")
	live, _ := webhook.Field[bool](payload, "livemode")
	fmt.Printf("event=%s amount=%.0f live=%t\n", event, amount, live)

	// A JSON number is float64, not int: asking for int fails safely, no panic.
	_, ok := webhook.Field[int](payload, "amount")
	fmt.Printf("Field[int](amount) ok=%t\n", ok)

	// A missing key returns the zero value and false, no panic.
	_, ok = webhook.Field[string](payload, "customer")
	fmt.Printf("Field[string](customer) ok=%t\n", ok)
}
```

Run it:

```bash
go run ./cmd/demo
```

Expected output:

```
event=charge.succeeded amount=4200 live=true
Field[int](amount) ok=false
Field[string](customer) ok=false
```

### Tests

Create `webhook_test.go`:

```go
package webhook

import (
	"fmt"
	"testing"
)

func decodedPayload(t *testing.T) map[string]any {
	t.Helper()
	p, err := Decode([]byte(`{"event":"ok","amount":4200,"live":true}`))
	if err != nil {
		t.Fatalf("Decode: %v", err)
	}
	return p
}

func TestFieldString(t *testing.T) {
	t.Parallel()
	v, ok := Field[string](decodedPayload(t), "event")
	if !ok || v != "ok" {
		t.Fatalf("Field[string](event) = (%q, %t), want (ok, true)", v, ok)
	}
}

func TestFieldNumber(t *testing.T) {
	t.Parallel()
	v, ok := Field[float64](decodedPayload(t), "amount")
	if !ok || v != 4200 {
		t.Fatalf("Field[float64](amount) = (%v, %t), want (4200, true)", v, ok)
	}
}

func TestFieldBool(t *testing.T) {
	t.Parallel()
	v, ok := Field[bool](decodedPayload(t), "live")
	if !ok || v != true {
		t.Fatalf("Field[bool](live) = (%v, %t), want (true, true)", v, ok)
	}
}

func TestFieldWrongTypeNoPanic(t *testing.T) {
	t.Parallel()
	// amount is a number; asking for string must return ("", false), not panic.
	v, ok := Field[string](decodedPayload(t), "amount")
	if ok {
		t.Fatalf("Field[string](amount) = (%q, true), want (\"\", false)", v)
	}
}

func TestFieldMissingKey(t *testing.T) {
	t.Parallel()
	v, ok := Field[string](decodedPayload(t), "nope")
	if ok || v != "" {
		t.Fatalf("Field[string](nope) = (%q, %t), want (\"\", false)", v, ok)
	}
}

// TestJSONIntegerIsFloat64 documents the stdlib behavior: a JSON integer decodes
// as float64 into map[string]any, so Field[int] fails and Field[float64] succeeds.
func TestJSONIntegerIsFloat64(t *testing.T) {
	t.Parallel()
	p := decodedPayload(t)
	if _, ok := Field[int](p, "amount"); ok {
		t.Fatal("Field[int](amount) succeeded; JSON numbers are float64, not int")
	}
	if _, ok := Field[float64](p, "amount"); !ok {
		t.Fatal("Field[float64](amount) failed; a JSON number should assert as float64")
	}
}

func ExampleField() {
	p, _ := Decode([]byte(`{"event":"ping"}`))
	event, ok := Field[string](p, "event")
	fmt.Println(event, ok)
	// Output: ping true
}
```

## Review

`Field` is correct when it extracts the value for a matching type, returns the
zero value and `false` for a wrong type or a missing key, and *never panics* —
`TestFieldWrongTypeNoPanic` is the whole point, because the single-return
assertion `payload[key].(T)` without the `ok` would crash the goroutine handling
the webhook. The generic type parameter is what lets the caller get a concrete
`string`/`float64`/`bool` back instead of an `any` to re-assert.

The mistake this exercise inoculates against is `Field[int]` on a JSON number.
`TestJSONIntegerIsFloat64` proves that fails, which surprises people every time:
`encoding/json` has no way to know you wanted an `int`, so it produces `float64`
for every number. Assert `float64` and convert, or decode into a typed struct.
Never reach for the panicking single-return assertion on data you did not build.

## Resources

- [Go Spec: Type assertions](https://go.dev/ref/spec#Type_assertions) — the one- and two-result forms and when each panics.
- [encoding/json.Unmarshal](https://pkg.go.dev/encoding/json#Unmarshal) — the rule that JSON numbers decode as `float64` into `any`.
- [Go Spec: Type parameters](https://go.dev/ref/spec#Type_parameter_declarations) — the generics that keep `Field` type-safe.

---

Back to [00-concepts.md](00-concepts.md) | Previous: [04-repository-notfound-vs-failure.md](04-repository-notfound-vs-failure.md) | Next: [06-constructor-returns-cleanup-func.md](06-constructor-returns-cleanup-func.md)
