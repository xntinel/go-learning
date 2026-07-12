# Exercise 2: Walk an Untyped Webhook Payload Decoded into `any`

The most common real source of `any` in a backend is `json.Unmarshal` into an
interface value: a webhook whose shape you do not control, or an event whose schema
varies by type. This module builds a `GetPath` walker over the `map[string]any` /
`[]any` tree that decoding produces, and pins the single most dangerous trap — a
64-bit ID silently corrupted through `float64` unless you decode with
`UseNumber()`.

This module is fully self-contained: its own `go mod init`, its own demo, its own
tests. Nothing here imports another exercise.

## What you'll build

```text
jsonwalk/                  independent module: example.com/jsonwalk
  go.mod                   go 1.26
  jsonwalk.go              DecodeDefault/DecodeNumbers; GetPath walks map[string]any / []any
  cmd/
    demo/
      main.go              runnable demo: walk a webhook, show float64 vs json.Number
  jsonwalk_test.go         path hit/miss/wrong-type/null; float64 precision-loss vs json.Number
```

- Files: `jsonwalk.go`, `cmd/demo/main.go`, `jsonwalk_test.go`.
- Implement: `DecodeDefault` (plain `json.Unmarshal` into `any`), `DecodeNumbers` (a `json.Decoder` with `UseNumber()`), and `GetPath(root any, path ...string) (any, bool)` walking nested `map[string]any` and numeric-index `[]any`.
- Test: `GetPath` on a real webhook fixture for a hit, a missing key, a wrong-type mid-path, and a `null`; and a test proving a 64-bit order ID survives `json.Number.Int64()` but is corrupted through `float64`.
- Verify: `go test -count=1 -race ./...`

Set up the module:

```bash
mkdir -p go-solutions/08-interfaces/02-empty-interface-and-any/02-decode-arbitrary-json/cmd/demo
cd go-solutions/08-interfaces/02-empty-interface-and-any/02-decode-arbitrary-json
go mod edit -go=1.26
```

### The fixed mapping, and the float64 trap

`json.Unmarshal(data, &v)` with `v` of type `any` produces a fixed set of dynamic
types: an object is `map[string]any`, an array is `[]any`, a string is `string`, a
boolean is `bool`, `null` is `nil`, and every number is `float64`. Walking such a
tree is a recursion over exactly two container types. `GetPath` takes a path of
string segments; at each step, if the current node is a `map[string]any` it indexes
by the segment, and if it is a `[]any` it parses the segment as an integer index.
Any other node type mid-path — a scalar where the path expected a container — means
the path does not resolve, and `GetPath` returns `(nil, false)` rather than
panicking. That is the whole discipline of walking untyped data: every step is a
comma-ok index or a bounds check, never an unguarded assertion.

The `float64` mapping is the trap that bites real systems. A JSON number decodes as
`float64`, which has a 52-bit mantissa, so integers above 2^53 lose their low bits.
An order ID like `9007199254740993` (2^53 + 1) becomes `9007199254740992` — off by
one, silently, and you refund the wrong order. The fix is `json.Decoder` with
`UseNumber()`: numbers decode as `json.Number`, a string type whose `Int64()`
returns the exact integer and whose `Float64()` and `String()` are available when
you want them. At any boundary carrying 64-bit identifiers or money, decode with
`UseNumber` and pull integers through `Int64()`.

Create `jsonwalk.go`:

```go
package jsonwalk

import (
	"bytes"
	"encoding/json"
	"strconv"
)

// DecodeDefault unmarshals JSON into an any tree using the default number mapping,
// where every number becomes float64.
func DecodeDefault(data []byte) (any, error) {
	var v any
	if err := json.Unmarshal(data, &v); err != nil {
		return nil, err
	}
	return v, nil
}

// DecodeNumbers unmarshals JSON into an any tree with Decoder.UseNumber, so every
// number becomes a json.Number and 64-bit integers survive exactly.
func DecodeNumbers(data []byte) (any, error) {
	dec := json.NewDecoder(bytes.NewReader(data))
	dec.UseNumber()
	var v any
	if err := dec.Decode(&v); err != nil {
		return nil, err
	}
	return v, nil
}

// GetPath walks a decoded any tree along path. Each segment indexes a
// map[string]any by name or a []any by numeric index. It returns (value, true)
// when the whole path resolves, else (nil, false). It never panics on a
// type or bounds mismatch.
func GetPath(root any, path ...string) (any, bool) {
	cur := root
	for _, seg := range path {
		switch node := cur.(type) {
		case map[string]any:
			next, ok := node[seg]
			if !ok {
				return nil, false
			}
			cur = next
		case []any:
			i, err := strconv.Atoi(seg)
			if err != nil || i < 0 || i >= len(node) {
				return nil, false
			}
			cur = node[i]
		default:
			// A scalar (or nil) where the path expected a container.
			return nil, false
		}
	}
	return cur, true
}
```

### The runnable demo

The demo decodes a small webhook both ways and shows the same order-ID field
arriving as a lossy `float64` under the default decoder and as an exact
`json.Number` under `UseNumber`.

Create `cmd/demo/main.go`:

```go
package main

import (
	"encoding/json"
	"fmt"

	"example.com/jsonwalk"
)

func main() {
	payload := []byte(`{
		"event": "order.paid",
		"data": {"order_id": 9007199254740993, "customer": {"email": "a@b.com"}}
	}`)

	def, _ := jsonwalk.DecodeDefault(payload)
	if v, ok := jsonwalk.GetPath(def, "data", "order_id"); ok {
		fmt.Printf("default   order_id: %v (%T)\n", v, v)
	}
	if email, ok := jsonwalk.GetPath(def, "data", "customer", "email"); ok {
		fmt.Printf("email: %v\n", email)
	}

	num, _ := jsonwalk.DecodeNumbers(payload)
	if v, ok := jsonwalk.GetPath(num, "data", "order_id"); ok {
		n, _ := v.(json.Number).Int64()
		fmt.Printf("usenumber order_id: %d (exact)\n", n)
	}

	if _, ok := jsonwalk.GetPath(def, "data", "missing"); !ok {
		fmt.Println("missing path: ok=false")
	}
}
```

Run it:

```bash
go run ./cmd/demo
```

Expected output:

```
default   order_id: 9.007199254740992e+15 (float64)
email: a@b.com
usenumber order_id: 9007199254740993 (exact)
missing path: ok=false
```

### Tests

`TestGetPath` drives a real webhook fixture through the four cases that matter: a
resolving nested path, a missing key, a wrong-type mid-path (indexing into a scalar),
and a `null` leaf that resolves to `(nil, true)`. `TestNumberPrecision` is the
headline: it decodes a 2^53+1 order ID both ways and proves the default `float64`
path corrupts it while the `UseNumber` / `Int64()` path returns it exactly.

Create `jsonwalk_test.go`:

```go
package jsonwalk

import (
	"encoding/json"
	"fmt"
	"testing"
)

const webhook = `{
	"event": "order.paid",
	"data": {
		"order_id": 42,
		"items": ["sku-1", "sku-2"],
		"customer": {"email": "a@b.com", "vip": null}
	}
}`

func TestGetPath(t *testing.T) {
	t.Parallel()

	root, err := DecodeDefault([]byte(webhook))
	if err != nil {
		t.Fatalf("decode: %v", err)
	}

	tests := []struct {
		name    string
		path    []string
		wantOK  bool
		wantVal any
	}{
		{"nested string", []string{"data", "customer", "email"}, true, "a@b.com"},
		{"array index", []string{"data", "items", "1"}, true, "sku-2"},
		{"top scalar", []string{"event"}, true, "order.paid"},
		{"null leaf", []string{"data", "customer", "vip"}, true, nil},
		{"missing key", []string{"data", "nope"}, false, nil},
		{"wrong type mid-path", []string{"event", "x"}, false, nil},
		{"index out of range", []string{"data", "items", "9"}, false, nil},
		{"non-numeric index", []string{"data", "items", "x"}, false, nil},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			got, ok := GetPath(root, tc.path...)
			if ok != tc.wantOK {
				t.Fatalf("GetPath(%v) ok = %v, want %v", tc.path, ok, tc.wantOK)
			}
			if ok && got != tc.wantVal {
				t.Fatalf("GetPath(%v) = %v, want %v", tc.path, got, tc.wantVal)
			}
		})
	}
}

func TestNumberPrecision(t *testing.T) {
	t.Parallel()

	// 2^53 + 1: the smallest int64 that float64 cannot represent exactly.
	const raw = `{"order_id": 9007199254740993}`
	const want int64 = 9007199254740993

	def, err := DecodeDefault([]byte(raw))
	if err != nil {
		t.Fatalf("decode default: %v", err)
	}
	f, ok := GetPath(def, "order_id")
	if !ok {
		t.Fatal("default: order_id not found")
	}
	if got := int64(f.(float64)); got == want {
		t.Fatalf("expected float64 path to CORRUPT the id, but got exact %d", got)
	}

	num, err := DecodeNumbers([]byte(raw))
	if err != nil {
		t.Fatalf("decode numbers: %v", err)
	}
	n, ok := GetPath(num, "order_id")
	if !ok {
		t.Fatal("usenumber: order_id not found")
	}
	got, err := n.(json.Number).Int64()
	if err != nil {
		t.Fatalf("Int64: %v", err)
	}
	if got != want {
		t.Fatalf("json.Number path = %d, want %d", got, want)
	}
}

func ExampleGetPath() {
	root, _ := DecodeDefault([]byte(`{"a":{"b":["x","y"]}}`))
	v, ok := GetPath(root, "a", "b", "0")
	fmt.Println(v, ok)
	// Output: x true
}
```

## Review

The walker is correct when every step is a guarded index — a comma-ok map lookup or
a parsed-and-bounds-checked slice index — and a scalar mid-path resolves to
`(nil, false)` instead of panicking. The four `GetPath` cases plus the two
index-error cases pin that contract. The precision test is the one that separates a
senior from a junior: the default decoder's `float64` genuinely cannot hold
2^53+1, and the test asserts the corruption rather than hiding it, then proves
`UseNumber` + `Int64()` recovers the exact value. The mistake this module exists to
prevent is assuming `payload["id"].(int)` works after decoding into `any` — it does
not, because the dynamic type is `float64`, and even the `float64` is already wrong
for large IDs. Run `go test -race` to confirm the walker and both decoders behave
across the table.

## Resources

- [`encoding/json.Unmarshal`](https://pkg.go.dev/encoding/json#Unmarshal) — the fixed mapping of JSON into an `any` tree.
- [`encoding/json.Number`](https://pkg.go.dev/encoding/json#Number) — `Int64`, `Float64`, `String` on a decoded number.
- [`(*json.Decoder).UseNumber`](https://pkg.go.dev/encoding/json#Decoder.UseNumber) — decode numbers as `json.Number` instead of `float64`.

---

Back to [00-concepts.md](00-concepts.md) | Next: [03-context-value-typed-keys.md](03-context-value-typed-keys.md)
