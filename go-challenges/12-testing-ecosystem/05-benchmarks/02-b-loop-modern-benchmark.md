# Exercise 2: Modern Benchmarking with b.Loop() (Go 1.24)

The Go 1.24 benchmark loop, `for b.Loop()`, fixes the two classic footguns of the
`b.N` form in one construct: it excludes setup written above the loop from the timed
region, and it keeps inputs and results alive so the compiler cannot delete the work.
This module benchmarks a response-DTO JSON marshal path — the encode step every
JSON API runs on every response — in both the `b.Loop` and the `b.N` forms so you
can see they measure the same thing and know what each one guarantees.

## What you'll build

```text
marshal/                   independent module: example.com/marshal
  go.mod                   go 1.24
  marshal.go               type OrderResponse; MarshalResponse(OrderResponse) ([]byte, error)
  cmd/
    demo/
      main.go              runnable demo: marshal a sample response, print the JSON
  marshal_test.go          round-trip correctness test; BenchmarkMarshalLoop (b.Loop) and
                           BenchmarkMarshalBN (b.N); Example
```

- Files: `marshal.go`, `cmd/demo/main.go`, `marshal_test.go`.
- Implement: an `OrderResponse` DTO and `MarshalResponse` that `json.Marshal`s it.
- Test: round-trip (marshal then unmarshal equals source), plus the two equivalent benchmarks.
- Verify: `go test -count=1 -race ./...` then `go test -bench=BenchmarkMarshal -benchmem`.

Set up the module:

```bash
go mod edit -go=1.24
```

### What b.Loop guarantees over b.N

Look at the two benchmarks side by side. In the `b.N` version you must manually keep
the result alive — here by assigning it to a package-level `sink` — because if you
wrote `_, _ = MarshalResponse(resp)` and dropped the bytes, the optimizer could in
principle prove the call has no observable effect and delete it. In the `b.Loop`
version there is no `sink`: `b.Loop` keeps the loop's inputs (`resp`) and results
alive across the call, so the compiler is not allowed to remove the marshal. Both
loops also build `resp` exactly once above the loop; with `b.N` that placement is a
rule you must remember, and with `b.Loop` it is enforced by the construct (code
before the `for` runs once, outside the timed region, by definition).

The payoff is that `b.Loop` makes the *correct* benchmark also the *natural* one:
setup goes above the loop where you would write it anyway, the body is just the call,
and you never think about sinks or `ResetTimer`. Both forms report the same `allocs/op`
because they marshal the identical value — a JSON encode of this DTO allocates the
same number of times regardless of how the loop is driven — which is the concrete
sense in which they are equivalent measurements of one function.

`MarshalResponse` is a thin wrapper over `encoding/json.Marshal` so the benchmark
measures the standard library encode path, not custom serialization. The DTO carries
the field mix a real order response has: scalars, a nested struct, and a slice, so
the encoder does real structural work.

Create `marshal.go`:

```go
package marshal

import (
	"encoding/json"
	"time"
)

// LineItem is one line of an order response.
type LineItem struct {
	SKU      string `json:"sku"`
	Quantity int    `json:"quantity"`
	Cents    int64  `json:"price_cents"`
}

// OrderResponse is the DTO a JSON order API serializes on every response. It mixes
// scalars, a timestamp, a nested struct, and a slice so the encoder does real work.
type OrderResponse struct {
	ID        string     `json:"id"`
	Status    string     `json:"status"`
	Total     int64      `json:"total_cents"`
	CreatedAt time.Time  `json:"created_at"`
	Customer  string     `json:"customer"`
	Items     []LineItem `json:"items"`
}

// MarshalResponse encodes r as JSON via the standard library encoder.
func MarshalResponse(r OrderResponse) ([]byte, error) {
	return json.Marshal(r)
}
```

### The runnable demo

The demo marshals a fixed response with a fixed timestamp so the output is stable,
and prints the JSON.

Create `cmd/demo/main.go`:

```go
package main

import (
	"fmt"
	"time"

	"example.com/marshal"
)

func main() {
	resp := marshal.OrderResponse{
		ID:        "ord_100",
		Status:    "shipped",
		Total:     3500,
		CreatedAt: time.Date(2026, 7, 2, 10, 0, 0, 0, time.UTC),
		Customer:  "alice",
		Items: []marshal.LineItem{
			{SKU: "sku-1", Quantity: 2, Cents: 1000},
			{SKU: "sku-2", Quantity: 1, Cents: 1500},
		},
	}
	b, err := marshal.MarshalResponse(resp)
	if err != nil {
		panic(err)
	}
	fmt.Println(string(b))
}
```

Run it:

```bash
go run ./cmd/demo
```

Expected output:

```
{"id":"ord_100","status":"shipped","total_cents":3500,"created_at":"2026-07-02T10:00:00Z","customer":"alice","items":[{"sku":"sku-1","quantity":2,"price_cents":1000},{"sku":"sku-2","quantity":1,"price_cents":1500}]}
```

### Tests

`TestRoundTrip` proves the marshal path is correct by unmarshaling the output back
into an `OrderResponse` and comparing it to the source with `reflect.DeepEqual` —
the honest correctness check for a serializer. The two benchmarks then demonstrate
the `b.Loop`/`b.N` equivalence; only the `b.N` form needs the package-level `sink`.

Create `marshal_test.go`:

```go
package marshal

import (
	"encoding/json"
	"fmt"
	"reflect"
	"testing"
	"time"
)

func sample() OrderResponse {
	return OrderResponse{
		ID:        "ord_100",
		Status:    "shipped",
		Total:     3500,
		CreatedAt: time.Date(2026, 7, 2, 10, 0, 0, 0, time.UTC),
		Customer:  "alice",
		Items: []LineItem{
			{SKU: "sku-1", Quantity: 2, Cents: 1000},
			{SKU: "sku-2", Quantity: 1, Cents: 1500},
		},
	}
}

func TestRoundTrip(t *testing.T) {
	t.Parallel()
	src := sample()
	data, err := MarshalResponse(src)
	if err != nil {
		t.Fatalf("MarshalResponse: %v", err)
	}
	var got OrderResponse
	if err := json.Unmarshal(data, &got); err != nil {
		t.Fatalf("Unmarshal: %v", err)
	}
	if !reflect.DeepEqual(got, src) {
		t.Fatalf("round trip mismatch:\n got %+v\nwant %+v", got, src)
	}
}

// sink defeats dead-code elimination in the b.N benchmark; b.Loop does not need it.
var sink []byte

func BenchmarkMarshalBN(b *testing.B) {
	resp := sample() // one-time setup, above the loop
	b.ReportAllocs()
	for range b.N {
		out, err := MarshalResponse(resp)
		if err != nil {
			b.Fatal(err)
		}
		sink = out // keep the result alive
	}
}

func BenchmarkMarshalLoop(b *testing.B) {
	resp := sample() // b.Loop excludes this from the timed region automatically
	b.ReportAllocs()
	for b.Loop() {
		out, err := MarshalResponse(resp)
		if err != nil {
			b.Fatal(err)
		}
		_ = out // b.Loop keeps out alive; no package-level sink required
	}
}

func ExampleMarshalResponse() {
	r := OrderResponse{ID: "ord_1", Status: "new", Total: 500,
		CreatedAt: time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC), Customer: "bob"}
	b, _ := MarshalResponse(r)
	fmt.Println(string(b))
	// Output: {"id":"ord_1","status":"new","total_cents":500,"created_at":"2026-01-01T00:00:00Z","customer":"bob","items":null}
}
```

Run the benchmarks (illustrative numbers; note the identical allocs/op):

```bash
go test -bench=BenchmarkMarshal -benchmem
```

```text
BenchmarkMarshalBN-8      2984101       402 ns/op      256 B/op    3 allocs/op
BenchmarkMarshalLoop-8    2971544       405 ns/op      256 B/op    3 allocs/op
PASS
```

## Review

The correctness gate is the round-trip: a serializer is right when unmarshaling its
output reconstructs the source value, and `reflect.DeepEqual` over the whole struct
catches a dropped or mistyped field that eyeballing the JSON would miss. The
benchmark lesson is the equivalence: `b.Loop` and `b.N` produce the same `allocs/op`
and the same order-of-magnitude `ns/op` because they measure the same call, but the
`b.Loop` version is safe by construction — setup is excluded and the result is kept
live without a `sink`, so it is the form to reach for in new code. The `b.N` version
is the one you will read and maintain in existing code; keeping both here makes the
translation between them mechanical. Note the `Example`'s `items:null` output: a nil
slice marshals to JSON `null`, not `[]`, which is a real serialization gotcha worth
seeing.

## Resources

- [`testing.B.Loop`](https://pkg.go.dev/testing#B.Loop) — the Go 1.24 loop, setup exclusion, and keep-alive semantics.
- [Go 1.24 release notes](https://go.dev/doc/go1.24) — `testing.B.Loop` and other testing additions.
- [`encoding/json.Marshal`](https://pkg.go.dev/encoding/json#Marshal) — the encode path being benchmarked, including nil-slice behavior.

---

Back to [00-concepts.md](00-concepts.md) | Previous: [01-reverse-string-benchmark.md](01-reverse-string-benchmark.md) | Next: [03-prevent-dead-code-elimination.md](03-prevent-dead-code-elimination.md)
