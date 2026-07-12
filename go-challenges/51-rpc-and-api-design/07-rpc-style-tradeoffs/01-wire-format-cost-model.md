# Exercise 1: The Wire-Format Cost Model: JSON vs Protobuf

"Protobuf is smaller and faster" is the most-repeated and least-measured claim in
API design. This exercise turns it into a number: you encode the *same* domain
message as REST-style JSON and as gRPC/Connect protobuf, and report the encoded
size, the size ratio, and the allocations, so the trade-off is grounded in your
message instead of folklore.

This module is fully self-contained. It begins with its own `go mod init`, defines
the message once as a JSON-tagged struct and once as a `.proto`, and ships its own
demo and tests. Nothing here imports any other exercise.

## What you'll build

```text
wirecost/                    independent module: example.com/wirecost
  go.mod                     go 1.26
  order.go                   JSON-tagged Order/LineItem + SampleOrder fixture (pure)
  codec.go                   Codec interface; JSONCodec; Measure/Ratio; ErrUnsupportedCodec (pure)
  order.proto               the protobuf schema (illustrative; generate with protoc/buf)
  proto_online.go            //go:build online — ProtoCodec + SampleOrderProto (google.golang.org/protobuf)
  cmd/
    demo/
      main.go                runnable pure demo: measure the JSON encoding
  wirecost_test.go           offline round-trip + registry + ratio tests; ExampleMeasure; BenchmarkMarshalJSON
  proto_online_test.go       //go:build online — real JSON-vs-proto size assertion + BenchmarkMarshalProto
```

- Files: `order.go`, `codec.go`, `order.proto`, `proto_online.go`, `cmd/demo/main.go`, `wirecost_test.go`, `proto_online_test.go`.
- Implement: a `Codec` interface (`Name`, `Marshal`, `Unmarshal`), a `JSONCodec` over `encoding/json`, a `Measure`/`Ratio` reporting pair, a `CodecByName` registry that returns `ErrUnsupportedCodec` for an unknown name, and the online `ProtoCodec` over the protobuf v2 runtime.
- Test: offline table-driven round-trip and registry tests with the sentinel asserted via `errors.Is`, an `Example` printing the deterministic JSON size, and `BenchmarkMarshalJSON` with `b.ReportAllocs`/`b.Loop`; the online test measures both codecs and asserts protobuf is smaller.
- Verify: `go test -count=1 -race ./...` (offline core); the protobuf path builds and runs with `-tags online` after `protoc`/`buf generate`.

This is a mode=bar lesson: the offline core (JSON codec, registry, reporting) gates
cleanly, while the protobuf half needs generated `.pb.go` (codegen) and the
external `google.golang.org/protobuf` module, so it lives behind `//go:build
online` and is validated by `gofmt`/`vet` and by shape. Set up the module:

```bash
mkdir -p go-solutions/51-rpc-and-api-design/07-rpc-style-tradeoffs/01-wire-format-cost-model/cmd/demo
cd go-solutions/51-rpc-and-api-design/07-rpc-style-tradeoffs/01-wire-format-cost-model
go mod edit -go=1.26
```

### One message, two encodings

The honest way to compare wire formats is to hold the *message* fixed and vary
only the codec. So the domain `Order` is defined once as a Go struct with JSON
tags — the shape a REST/JSON handler would marshal — and once as a `.proto` that
compiles to the shape a gRPC/Connect handler would marshal. The fixture
`SampleOrder` builds a realistically structured message: an id, an `int64`
customer id, an enum-like status, a repeated list of line items (the field where
protobuf's omission of field names pays off most), an `int64` money total, and a
timestamp. Numeric and repeated data is exactly where binary encoding wins, and a
string-only message is where it does not, so a fixture that mixes both keeps the
measurement honest.

Create `order.go`:

```go
package wirecost

import "time"

// LineItem is one row of an order. The JSON tags are what a REST handler would
// serialize; the same fields appear in the .proto with numbered fields.
type LineItem struct {
	SKU      string `json:"sku"`
	Quantity int32  `json:"quantity"`
	Price    int64  `json:"price_cents"`
}

// Order is the domain message compared across codecs. It mixes a string id, an
// int64 id, an enum-like status string, a large repeated field, an int64 money
// amount, and a timestamp so the JSON-vs-protobuf ratio is representative rather
// than cherry-picked.
type Order struct {
	ID         string     `json:"id"`
	CustomerID int64      `json:"customer_id"`
	Status     string     `json:"status"`
	Items      []LineItem `json:"items"`
	Total      int64      `json:"total_cents"`
	CreatedAt  time.Time  `json:"created_at"`
}

// SampleOrder builds a deterministic fixture: twelve line items so the repeated
// field dominates, with fixed values so the encoded size is reproducible.
func SampleOrder() *Order {
	items := make([]LineItem, 0, 12)
	var total int64
	for i := range 12 {
		li := LineItem{
			SKU:      "SKU-" + string(rune('A'+i)) + "0042",
			Quantity: int32(i%5 + 1),
			Price:    int64((i + 1) * 1999),
		}
		total += int64(li.Quantity) * li.Price
		items = append(items, li)
	}
	return &Order{
		ID:         "ord_01HZ8QconsistentULID",
		CustomerID: 8823771002,
		Status:     "STATUS_CONFIRMED",
		Items:      items,
		Total:      total,
		CreatedAt:  time.Date(2026, 3, 14, 9, 26, 53, 0, time.UTC),
	}
}
```

### The Codec abstraction and the reporting pair

A `Codec` is the minimal interface that both encodings satisfy: a name, a marshal,
and an unmarshal over `any`. Keeping it `any`-typed is deliberate — the JSON codec
marshals the Go struct, while the protobuf codec marshals a generated
`proto.Message`, and the two message types are genuinely different, so the
interface cannot be generic over one concrete type. `Measure` marshals a value and
reports its encoded byte count; `Ratio` divides two measurements so you can state
"JSON is N times larger than protobuf" as a computed fact.

`CodecByName` is a tiny registry. The offline core knows only `"json"`; any other
name — including `"proto"`, which is wired only in the online build — returns
`ErrUnsupportedCodec`, wrapped with `%w` so a caller branches on
`errors.Is(err, ErrUnsupportedCodec)`. This is the sentinel-error path the test
exercises with an unknown codec name.

Create `codec.go`:

```go
package wirecost

import (
	"encoding/json"
	"errors"
	"fmt"
)

// ErrUnsupportedCodec is returned by CodecByName for a name the offline core does
// not know. It is wrapped with %w so callers assert with errors.Is.
var ErrUnsupportedCodec = errors.New("unsupported codec")

// Codec encodes and decodes a message. It is any-typed because the JSON codec
// operates on the Go struct while the protobuf codec operates on a proto.Message,
// which are different concrete types.
type Codec interface {
	Name() string
	Marshal(v any) ([]byte, error)
	Unmarshal(data []byte, v any) error
}

// JSONCodec is the REST/JSON encoding over encoding/json.
type JSONCodec struct{}

func (JSONCodec) Name() string { return "json" }

func (JSONCodec) Marshal(v any) ([]byte, error) { return json.Marshal(v) }

func (JSONCodec) Unmarshal(data []byte, v any) error { return json.Unmarshal(data, v) }

// CodecByName resolves a codec name. The offline core registers only "json"; the
// protobuf codec is constructed directly as ProtoCodec{} in the online build.
// Any unknown name returns ErrUnsupportedCodec.
func CodecByName(name string) (Codec, error) {
	switch name {
	case "json":
		return JSONCodec{}, nil
	default:
		return nil, fmt.Errorf("codec %q: %w", name, ErrUnsupportedCodec)
	}
}

// Sizing is the encoded size of one message under one codec.
type Sizing struct {
	Codec string
	Bytes int
}

// Measure marshals v with c and reports the encoded byte count.
func Measure(c Codec, v any) (Sizing, error) {
	b, err := c.Marshal(v)
	if err != nil {
		return Sizing{}, fmt.Errorf("measure %s: %w", c.Name(), err)
	}
	return Sizing{Codec: c.Name(), Bytes: len(b)}, nil
}

// Ratio reports a.Bytes / b.Bytes, e.g. Ratio(jsonSize, protoSize) is how many
// times larger the JSON encoding is than the protobuf encoding.
func Ratio(a, b Sizing) float64 {
	return float64(a.Bytes) / float64(b.Bytes)
}
```

### The protobuf twin (online)

The protobuf side is a schema plus a thin codec. The schema names the same fields
with wire numbers; the numbers, not the names, are what travels, which is the whole
reason protobuf is compact. The status becomes a real enum, the timestamp becomes
`google.protobuf.Timestamp`, and note that the protobuf-to-JSON mapping would emit
`customer_id` and `total_cents` (both `int64`) as quoted strings to survive
JavaScript's 53-bit number precision — a concrete instance of the JSON integer
trap from the concepts file.

This is the illustrative schema; it is a `proto` block, not assembled Go:

```proto
syntax = "proto3";
package order.v1;
option go_package = "example.com/wirecost/orderpb;orderpb";

import "google/protobuf/timestamp.proto";

enum Status {
  STATUS_UNSPECIFIED = 0;
  STATUS_PENDING = 1;
  STATUS_CONFIRMED = 2;
  STATUS_SHIPPED = 3;
}

message LineItem {
  string sku = 1;
  int32 quantity = 2;
  int64 price_cents = 3;
}

message Order {
  string id = 1;
  int64 customer_id = 2;
  Status status = 3;
  repeated LineItem items = 4;
  int64 total_cents = 5;
  google.protobuf.Timestamp created_at = 6;
}
```

Generate the Go types (once, on a machine with the plugins installed):

```bash
protoc --go_out=. --go_opt=paths=source_relative order.proto
# or, with buf:  buf generate
```

The `ProtoCodec` is the protobuf half of the `Codec` interface. It type-asserts
its argument to `proto.Message` (v2) and delegates to `proto.Marshal`/
`proto.Unmarshal`; a non-message value returns `ErrUnsupportedCodec`.
`SampleOrderProto` builds the exact same order as `SampleOrder` so the two sizes
are comparable. Because it imports the external protobuf module and the generated
`orderpb` package, the file is behind `//go:build online` and excluded from the
offline gate.

Create `proto_online.go`:

```go
//go:build online

// This file holds the protobuf half of the comparison. It is excluded from the
// default build because it imports the generated orderpb package (produced by
// protoc/buf) and google.golang.org/protobuf. Build and test it with -tags online
// after generating the code. The pure JSON core is tested offline.
package wirecost

import (
	"fmt"
	"time"

	orderpb "example.com/wirecost/orderpb"
	"google.golang.org/protobuf/proto"
	"google.golang.org/protobuf/types/known/timestamppb"
)

// ProtoCodec is the gRPC/Connect encoding over the protobuf v2 runtime.
type ProtoCodec struct{}

func (ProtoCodec) Name() string { return "proto" }

func (ProtoCodec) Marshal(v any) ([]byte, error) {
	m, ok := v.(proto.Message)
	if !ok {
		return nil, fmt.Errorf("proto marshal: %w", ErrUnsupportedCodec)
	}
	return proto.Marshal(m)
}

func (ProtoCodec) Unmarshal(data []byte, v any) error {
	m, ok := v.(proto.Message)
	if !ok {
		return fmt.Errorf("proto unmarshal: %w", ErrUnsupportedCodec)
	}
	return proto.Unmarshal(data, m)
}

// SampleOrderProto is the protobuf twin of SampleOrder: the same twelve items and
// the same field values, so proto.Size and len(jsonBytes) are measuring the same
// logical message.
func SampleOrderProto() *orderpb.Order {
	items := make([]*orderpb.LineItem, 0, 12)
	var total int64
	for i := range 12 {
		li := &orderpb.LineItem{
			Sku:        "SKU-" + string(rune('A'+i)) + "0042",
			Quantity:   int32(i%5 + 1),
			PriceCents: int64((i + 1) * 1999),
		}
		total += int64(li.GetQuantity()) * li.GetPriceCents()
		items = append(items, li)
	}
	return &orderpb.Order{
		Id:         "ord_01HZ8QconsistentULID",
		CustomerId: 8823771002,
		Status:     orderpb.Status_STATUS_CONFIRMED,
		Items:      items,
		TotalCents: total,
		CreatedAt:  timestamppb.New(time.Date(2026, 3, 14, 9, 26, 53, 0, time.UTC)),
	}
}
```

### The runnable demo

The demo stays in the offline core: it measures the JSON encoding of the fixture
and prints the byte count and item count. It runs with no codegen and no external
module, so `go run ./cmd/demo` works immediately.

Create `cmd/demo/main.go`:

```go
package main

import (
	"fmt"

	"example.com/wirecost"
)

func main() {
	o := wirecost.SampleOrder()
	s, err := wirecost.Measure(wirecost.JSONCodec{}, o)
	if err != nil {
		fmt.Println("error:", err)
		return
	}
	fmt.Printf("codec=%s bytes=%d items=%d\n", s.Codec, s.Bytes, len(o.Items))
}
```

Run it:

```bash
go run ./cmd/demo
```

Expected output:

```
codec=json bytes=784 items=12
```

### Tests

The offline tests prove the parts that carry bugs: the JSON round-trip preserves
the message (including the timestamp, compared with `Time.Equal` rather than `==`
so monotonic-clock and location differences do not cause a false failure), the
registry returns `ErrUnsupportedCodec` for an unknown name, and `Ratio` computes
correctly. `ExampleMeasure` locks the exact JSON size for the fixture, and
`BenchmarkMarshalJSON` uses Go 1.24's `b.Loop()` with `b.ReportAllocs()` so a
regression in allocations shows up.

Create `wirecost_test.go`:

```go
package wirecost

import (
	"errors"
	"fmt"
	"testing"
)

func TestJSONRoundTrip(t *testing.T) {
	t.Parallel()
	c := JSONCodec{}
	orig := SampleOrder()
	b, err := c.Marshal(orig)
	if err != nil {
		t.Fatalf("Marshal: %v", err)
	}
	var got Order
	if err := c.Unmarshal(b, &got); err != nil {
		t.Fatalf("Unmarshal: %v", err)
	}
	if got.ID != orig.ID || len(got.Items) != len(orig.Items) || got.Total != orig.Total {
		t.Fatalf("round trip mismatch: got %+v", got)
	}
	if !got.CreatedAt.Equal(orig.CreatedAt) {
		t.Fatalf("CreatedAt mismatch: got %v want %v", got.CreatedAt, orig.CreatedAt)
	}
}

func TestCodecByName(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name    string
		codec   string
		wantErr error
	}{
		{name: "json known", codec: "json"},
		{name: "xml unknown", codec: "xml", wantErr: ErrUnsupportedCodec},
		{name: "empty unknown", codec: "", wantErr: ErrUnsupportedCodec},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			c, err := CodecByName(tc.codec)
			if tc.wantErr != nil {
				if !errors.Is(err, tc.wantErr) {
					t.Fatalf("CodecByName err = %v, want errors.Is %v", err, tc.wantErr)
				}
				return
			}
			if err != nil {
				t.Fatalf("unexpected err: %v", err)
			}
			if c.Name() != tc.codec {
				t.Fatalf("Name = %q, want %q", c.Name(), tc.codec)
			}
		})
	}
}

func TestRatio(t *testing.T) {
	t.Parallel()
	json := Sizing{Codec: "json", Bytes: 800}
	proto := Sizing{Codec: "proto", Bytes: 200}
	if r := Ratio(json, proto); r != 4.0 {
		t.Fatalf("Ratio = %v, want 4", r)
	}
}

func BenchmarkMarshalJSON(b *testing.B) {
	c := JSONCodec{}
	o := SampleOrder()
	b.ReportAllocs()
	for b.Loop() {
		if _, err := c.Marshal(o); err != nil {
			b.Fatal(err)
		}
	}
}

func ExampleMeasure() {
	s, _ := Measure(JSONCodec{}, SampleOrder())
	fmt.Printf("json bytes=%d\n", s.Bytes)
	// Output: json bytes=784
}
```

The online test is where the headline claim is proven. It measures the fixture
under both codecs, asserts the protobuf encoding is strictly smaller, prints the
ratio, and round-trips the protobuf message with `proto.Equal` (never `==` or
`reflect.DeepEqual`, which break on a message's unexported state). It is behind
`//go:build online` and runs with `-tags online` after codegen.

Create `proto_online_test.go`:

```go
//go:build online

package wirecost

import (
	"testing"

	"google.golang.org/protobuf/proto"
)

// TestProtoSmallerThanJSON proves the wire-cost claim on this message rather than
// quoting a benchmark: it measures both encodings and asserts protobuf is smaller.
func TestProtoSmallerThanJSON(t *testing.T) {
	jsonSize, err := Measure(JSONCodec{}, SampleOrder())
	if err != nil {
		t.Fatalf("json measure: %v", err)
	}
	protoSize, err := Measure(ProtoCodec{}, SampleOrderProto())
	if err != nil {
		t.Fatalf("proto measure: %v", err)
	}
	if protoSize.Bytes >= jsonSize.Bytes {
		t.Fatalf("expected protobuf smaller: json=%d proto=%d", jsonSize.Bytes, protoSize.Bytes)
	}
	t.Logf("json=%d proto=%d ratio=%.2f", jsonSize.Bytes, protoSize.Bytes, Ratio(jsonSize, protoSize))
}

func TestProtoRoundTrip(t *testing.T) {
	c := ProtoCodec{}
	orig := SampleOrderProto()
	b, err := c.Marshal(orig)
	if err != nil {
		t.Fatalf("Marshal: %v", err)
	}
	got := SampleOrderProto()
	got.Reset()
	if err := c.Unmarshal(b, got); err != nil {
		t.Fatalf("Unmarshal: %v", err)
	}
	if !proto.Equal(orig, got) {
		t.Fatalf("round trip mismatch")
	}
}

func BenchmarkMarshalProto(b *testing.B) {
	c := ProtoCodec{}
	o := SampleOrderProto()
	b.ReportAllocs()
	for b.Loop() {
		if _, err := c.Marshal(o); err != nil {
			b.Fatal(err)
		}
	}
}
```

## Review

The measurement is honest only if both codecs encode the same logical message, so
the fixtures must stay in lockstep: `SampleOrderProto` mirrors `SampleOrder` field
for field, and if you change one you must change the other or the ratio becomes
meaningless. The common mistakes this exercise defends against are all in the
comparison discipline. Comparing decoded protobuf messages with `==` or
`reflect.DeepEqual` is wrong because generated messages carry unexported state;
the test uses `proto.Equal`. Comparing the JSON timestamp with `==` is fragile for
the same class of reason (location and monotonic clock); the test uses
`Time.Equal`. And quoting a size ratio you did not measure is the mistake the whole
module exists to prevent: the online test computes the ratio on your message.

Confirm the offline core with `go test -race ./...`; the round-trip must preserve
the fixture, the registry must return `ErrUnsupportedCodec` for an unknown name,
and `ExampleMeasure` must reproduce `784`. To prove the protobuf half, run
`protoc`/`buf generate`, add the module requirements, and
`go test -tags online ./...`; a passing `TestProtoSmallerThanJSON` is the measured
form of "protobuf is smaller", and the `-bench` runs quantify the allocation
difference.

## Resources

- [`google.golang.org/protobuf/proto`](https://pkg.go.dev/google.golang.org/protobuf/proto) — `Marshal`, `Unmarshal`, `Size`, `Equal`, and the `proto.Message` interface.
- [`encoding/json`](https://pkg.go.dev/encoding/json) — `Marshal`/`Unmarshal` and struct tag semantics for the REST/JSON side.
- [ProtoJSON mapping](https://protobuf.dev/programming-guides/json/) — why `int64`/`uint64` are emitted as JSON strings, and the rest of the proto3-to-JSON rules.
- [`testing.B.Loop`](https://pkg.go.dev/testing#B.Loop) — the Go 1.24 benchmark loop used with `b.ReportAllocs`.

---

Back to [00-concepts.md](00-concepts.md) | Next: [02-one-endpoint-three-protocols.md](02-one-endpoint-three-protocols.md)
