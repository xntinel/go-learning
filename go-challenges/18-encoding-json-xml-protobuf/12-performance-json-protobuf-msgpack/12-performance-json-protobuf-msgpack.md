# 12. Performance -- JSON vs Protobuf vs MessagePack

Serialization benchmarks are only useful after correctness is proven. This offline lesson uses only the standard library: `encoding/json`, `encoding/gob`, and a small protobuf-like binary codec. Real protobuf and MessagePack packages are deliberately deferred until a networked environment can fetch and pin dependencies.

## Concepts

### Compare Equal Logical Data

Every codec must encode the same `Order` value and decode it back before timing matters. Otherwise the benchmark measures different work.

### Benchmarks Need Isolation

Use `testing.B`, call `b.ReportAllocs`, keep setup outside the timed loop, and benchmark marshal and unmarshal separately. The demo can print sizes; the benchmark reports timings.

### Offline Baselines Still Teach Trade-Offs

JSON is portable and readable. gob is Go-specific and convenient. The custom binary codec is not real protobuf, but it demonstrates compact varints and length-delimited strings without network dependencies.

## Exercises

Set up the module:

```bash
mkdir -p go-solutions/18-encoding-json-xml-protobuf/12-performance-json-protobuf-msgpack/12-performance-json-protobuf-msgpack/cmd/demo
cd go-solutions/18-encoding-json-xml-protobuf/12-performance-json-protobuf-msgpack/12-performance-json-protobuf-msgpack
go mod edit -go=1.26
```

### Exercise 1: Define the Shared Order Model

Create `order.go`:

```go
package ordercodec

type Order struct {
	id       uint64
	customer string
	total    uint64
	items    []Item
}

type Item struct {
	sku      string
	quantity uint64
	price    uint64
}

func NewOrder(id uint64, customer string, total uint64, items []Item) Order {
	return Order{id: id, customer: customer, total: total, items: append([]Item(nil), items...)}
}

func NewItem(sku string, quantity uint64, price uint64) Item {
	return Item{sku: sku, quantity: quantity, price: price}
}

func (o Order) ID() uint64       { return o.id }
func (o Order) Customer() string { return o.customer }
func (o Order) Total() uint64    { return o.total }
func (o Order) Items() []Item    { return append([]Item(nil), o.items...) }
func (i Item) SKU() string       { return i.sku }
func (i Item) Quantity() uint64  { return i.quantity }
func (i Item) Price() uint64     { return i.price }

type wireOrder struct {
	ID       uint64     `json:"id"`
	Customer string     `json:"customer"`
	Total    uint64     `json:"total"`
	Items    []wireItem `json:"items"`
}

type wireItem struct {
	SKU      string `json:"sku"`
	Quantity uint64 `json:"quantity"`
	Price    uint64 `json:"price"`
}

func toWireOrder(o Order) wireOrder {
	items := make([]wireItem, 0, len(o.items))
	for _, item := range o.items {
		items = append(items, wireItem{SKU: item.sku, Quantity: item.quantity, Price: item.price})
	}
	return wireOrder{ID: o.id, Customer: o.customer, Total: o.total, Items: items}
}

func fromWireOrder(o wireOrder) Order {
	items := make([]Item, 0, len(o.Items))
	for _, item := range o.Items {
		items = append(items, NewItem(item.SKU, item.Quantity, item.Price))
	}
	return NewOrder(o.ID, o.Customer, o.Total, items)
}
```

### Exercise 2: Implement Three Codecs

Create `codec.go`:

```go
package ordercodec

import (
	"bytes"
	"encoding/binary"
	"encoding/gob"
	"encoding/json"
	"errors"
	"fmt"
	"io"
)

var ErrInvalidPayload = errors.New("invalid payload")

func MarshalJSON(o Order) ([]byte, error) {
	data, err := json.Marshal(toWireOrder(o))
	if err != nil {
		return nil, fmt.Errorf("%w: json marshal: %v", ErrInvalidPayload, err)
	}
	return data, nil
}

func UnmarshalJSON(data []byte) (Order, error) {
	var wire wireOrder
	if err := json.Unmarshal(data, &wire); err != nil {
		return Order{}, fmt.Errorf("%w: json unmarshal: %v", ErrInvalidPayload, err)
	}
	return fromWireOrder(wire), nil
}

func MarshalGob(o Order) ([]byte, error) {
	var b bytes.Buffer
	if err := gob.NewEncoder(&b).Encode(toWireOrder(o)); err != nil {
		return nil, fmt.Errorf("%w: gob encode: %v", ErrInvalidPayload, err)
	}
	return b.Bytes(), nil
}

func UnmarshalGob(data []byte) (Order, error) {
	var wire wireOrder
	if err := gob.NewDecoder(bytes.NewReader(data)).Decode(&wire); err != nil {
		return Order{}, fmt.Errorf("%w: gob decode: %v", ErrInvalidPayload, err)
	}
	return fromWireOrder(wire), nil
}

func MarshalBinary(o Order) ([]byte, error) {
	var b bytes.Buffer
	writeUvarint(&b, o.id)
	writeString(&b, o.customer)
	writeUvarint(&b, o.total)
	writeUvarint(&b, uint64(len(o.items)))
	for _, item := range o.items {
		writeString(&b, item.sku)
		writeUvarint(&b, item.quantity)
		writeUvarint(&b, item.price)
	}
	return b.Bytes(), nil
}

func UnmarshalBinary(data []byte) (Order, error) {
	r := bytes.NewReader(data)
	id, err := readUvarint(r)
	if err != nil {
		return Order{}, err
	}
	customer, err := readString(r)
	if err != nil {
		return Order{}, err
	}
	total, err := readUvarint(r)
	if err != nil {
		return Order{}, err
	}
	count, err := readUvarint(r)
	if err != nil {
		return Order{}, err
	}
	if count > 10000 {
		return Order{}, fmt.Errorf("%w: item count too large", ErrInvalidPayload)
	}
	items := make([]Item, 0, count)
	for i := uint64(0); i < count; i++ {
		sku, err := readString(r)
		if err != nil {
			return Order{}, err
		}
		quantity, err := readUvarint(r)
		if err != nil {
			return Order{}, err
		}
		price, err := readUvarint(r)
		if err != nil {
			return Order{}, err
		}
		items = append(items, NewItem(sku, quantity, price))
	}
	if r.Len() != 0 {
		return Order{}, fmt.Errorf("%w: trailing bytes", ErrInvalidPayload)
	}
	return NewOrder(id, customer, total, items), nil
}

func writeString(w *bytes.Buffer, s string) {
	writeUvarint(w, uint64(len(s)))
	w.WriteString(s)
}

func writeUvarint(w *bytes.Buffer, v uint64) {
	var buf [binary.MaxVarintLen64]byte
	n := binary.PutUvarint(buf[:], v)
	w.Write(buf[:n])
}

func readString(r *bytes.Reader) (string, error) {
	n, err := readUvarint(r)
	if err != nil {
		return "", err
	}
	if n > uint64(r.Len()) {
		return "", fmt.Errorf("%w: string length exceeds input", ErrInvalidPayload)
	}
	buf := make([]byte, n)
	if _, err := io.ReadFull(r, buf); err != nil {
		return "", fmt.Errorf("%w: reading string: %v", ErrInvalidPayload, err)
	}
	return string(buf), nil
}

func readUvarint(r *bytes.Reader) (uint64, error) {
	v, err := binary.ReadUvarint(r)
	if err != nil {
		return 0, fmt.Errorf("%w: reading uvarint: %v", ErrInvalidPayload, err)
	}
	return v, nil
}
```

### Exercise 3: Test and Benchmark the Codecs

Create `codec_test.go`:

```go
package ordercodec

import (
	"errors"
	"fmt"
	"reflect"
	"testing"
)

func sampleOrder() Order {
	return NewOrder(1001, "cust-9", 4599, []Item{NewItem("book", 2, 1299), NewItem("pen", 5, 399)})
}

func TestCodecRoundTrip(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name      string
		marshal   func(Order) ([]byte, error)
		unmarshal func([]byte) (Order, error)
	}{
		{name: "json", marshal: MarshalJSON, unmarshal: UnmarshalJSON},
		{name: "gob", marshal: MarshalGob, unmarshal: UnmarshalGob},
		{name: "binary", marshal: MarshalBinary, unmarshal: UnmarshalBinary},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			want := sampleOrder()
			data, err := tc.marshal(want)
			if err != nil {
				t.Fatal(err)
			}
			got, err := tc.unmarshal(data)
			if err != nil {
				t.Fatal(err)
			}
			if !reflect.DeepEqual(got, want) {
				t.Fatalf("round trip = %#v, want %#v", got, want)
			}
		})
	}
}

func TestDecodeErrors(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name      string
		unmarshal func([]byte) (Order, error)
		data      []byte
	}{
		{name: "json", unmarshal: UnmarshalJSON, data: []byte("{")},
		{name: "gob", unmarshal: UnmarshalGob, data: []byte("bad")},
		{name: "binary", unmarshal: UnmarshalBinary, data: []byte{255}},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			_, err := tc.unmarshal(tc.data)
			if !errors.Is(err, ErrInvalidPayload) {
				t.Fatalf("error = %v, want ErrInvalidPayload", err)
			}
		})
	}
}

func ExampleMarshalBinary() {
	order := NewOrder(7, "cust-1", 2500, []Item{NewItem("hat", 1, 2500)})
	data, _ := MarshalBinary(order)
	decoded, _ := UnmarshalBinary(data)
	fmt.Println(decoded.ID(), decoded.Customer(), decoded.Total(), decoded.Items()[0].SKU())
	// Output:
	// 7 cust-1 2500 hat
}

func BenchmarkMarshalJSON(b *testing.B)   { benchmarkMarshal(b, MarshalJSON) }
func BenchmarkMarshalGob(b *testing.B)    { benchmarkMarshal(b, MarshalGob) }
func BenchmarkMarshalBinary(b *testing.B) { benchmarkMarshal(b, MarshalBinary) }

func benchmarkMarshal(b *testing.B, fn func(Order) ([]byte, error)) {
	order := sampleOrder()
	b.ReportAllocs()
	for b.Loop() {
		if _, err := fn(order); err != nil {
			b.Fatal(err)
		}
	}
}

func BenchmarkUnmarshalJSON(b *testing.B) {
	data, _ := MarshalJSON(sampleOrder())
	benchmarkUnmarshal(b, data, UnmarshalJSON)
}
func BenchmarkUnmarshalGob(b *testing.B) {
	data, _ := MarshalGob(sampleOrder())
	benchmarkUnmarshal(b, data, UnmarshalGob)
}
func BenchmarkUnmarshalBinary(b *testing.B) {
	data, _ := MarshalBinary(sampleOrder())
	benchmarkUnmarshal(b, data, UnmarshalBinary)
}

func benchmarkUnmarshal(b *testing.B, data []byte, fn func([]byte) (Order, error)) {
	b.ReportAllocs()
	for b.Loop() {
		if _, err := fn(data); err != nil {
			b.Fatal(err)
		}
	}
}
```

Your turn: add a large-order benchmark with at least 100 items and compare it to the small fixture.

Create `cmd/demo/main.go`:

```go
package main

import (
	"fmt"
	"log"

	ordercodec "example.com/codec-performance"
)

func main() {
	order := ordercodec.NewOrder(9, "cust-demo", 4999, []ordercodec.Item{ordercodec.NewItem("bag", 1, 4999)})
	jsonData, err := ordercodec.MarshalJSON(order)
	if err != nil {
		log.Fatal(err)
	}
	gobData, err := ordercodec.MarshalGob(order)
	if err != nil {
		log.Fatal(err)
	}
	binData, err := ordercodec.MarshalBinary(order)
	if err != nil {
		log.Fatal(err)
	}
	decoded, err := ordercodec.UnmarshalBinary(binData)
	if err != nil {
		log.Fatal(err)
	}
	fmt.Printf("json=%d gob=%d binary=%d order=%d customer=%s\n", len(jsonData), len(gobData), len(binData), decoded.ID(), decoded.Customer())
}
```

## Common Mistakes

- Wrong: benchmark before testing round trips. What happens: fast broken codecs look good. Fix: require round-trip tests first.
- Wrong: claim universal winners from one local benchmark. What happens: payload shape and Go version are ignored. Fix: report environment-specific results.
- Wrong: import unpinned third-party MessagePack or protobuf packages in an offline gate. What happens: verification depends on network. Fix: defer those implementations to a networked run.

## Verification

From `~/go-exercises/codec-performance`:

```bash
test -z "$(gofmt -l .)"
go vet ./...
go build ./...
go test -count=1 -race ./...
go test -bench=. -benchmem ./...
go run ./cmd/demo
```

All commands must pass. Add at least one benchmark variant of your own before considering the lesson complete.

## Summary

- Performance comparisons require equivalent data and round-trip correctness.
- JSON, gob, and compact binary codecs optimize for different constraints.
- `testing.B` with `ReportAllocs` gives repeatable local evidence.
- Real protobuf and MessagePack should be added only with pinned dependencies and generated code in a networked environment.

## What's Next

Next: [Reading and Writing Files](../../19-io-and-filesystem/01-reading-and-writing-files/01-reading-and-writing-files.md).

## Resources

- [encoding/json package documentation](https://pkg.go.dev/encoding/json)
- [encoding/gob package documentation](https://pkg.go.dev/encoding/gob)
- [encoding/binary package documentation](https://pkg.go.dev/encoding/binary)
- [testing.B documentation](https://pkg.go.dev/testing#B)
