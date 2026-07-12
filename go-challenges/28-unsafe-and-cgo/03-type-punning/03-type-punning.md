# 3. Type Punning

Type punning reinterprets bytes as another type without a semantic conversion. In Go, the standard library already provides safe helpers for common cases such as floating-point bits, so this lesson uses unsafe only for a packet header whose layout is explicitly tested.

## Concepts

### Reinterpretation Is Not Conversion

Converting `float64(3)` to `uint64` changes the value. Reinterpreting the same memory as `uint64` reads the IEEE 754 representation. Those are different operations with different contracts.

### The Layout Must Be Fixed

Unsafe type punning only makes sense when size, alignment, and byte order are controlled. Network protocols usually specify byte order, so this package validates the header and returns ordinary Go values.

### Prefer Standard Library Helpers

For floats, use `math.Float64bits` and `math.Float64frombits`. Unsafe punning is reserved here for a small fixed header so the caller never touches `unsafe.Pointer`.

## Exercises

### Exercise 1: Parse A Header

Create `header.go`:

```go
package punning

import (
	"encoding/binary"
	"errors"
	"fmt"
	"math"
	"unsafe"
)

var (
	ErrShortHeader = errors.New("header buffer too short")
	ErrBadVersion  = errors.New("unsupported header version")
)

type Header struct {
	Version uint16
	Flags   uint16
	Length  uint32
}

const HeaderSize = int(unsafe.Sizeof(Header{}))

func ParseHeader(buf []byte) (Header, error) {
	if len(buf) < HeaderSize {
		return Header{}, fmt.Errorf("%w: got %d need %d", ErrShortHeader, len(buf), HeaderSize)
	}
	h := *(*Header)(unsafe.Pointer(unsafe.SliceData(buf)))
	if h.Version != 1 {
		return Header{}, fmt.Errorf("%w: %d", ErrBadVersion, h.Version)
	}
	return h, nil
}

func EncodeHeader(h Header) []byte {
	buf := make([]byte, HeaderSize)
	binary.LittleEndian.PutUint16(buf[0:2], h.Version)
	binary.LittleEndian.PutUint16(buf[2:4], h.Flags)
	binary.LittleEndian.PutUint32(buf[4:8], h.Length)
	return buf
}

func FloatBits(v float64) uint64 {
	return math.Float64bits(v)
}
```

### Exercise 2: Add Example And Demo

Create `example_test.go`:

```go
package punning

import "fmt"

func ExampleParseHeader() {
	h, _ := ParseHeader(EncodeHeader(Header{Version: 1, Flags: 2, Length: 64}))
	fmt.Println(h.Version, h.Flags, h.Length)
	// Output: 1 2 64
}
```

Create `cmd/demo/main.go`:

```go
package main

import (
	"fmt"
	"log"

	"example.com/punning"
)

func main() {
	h, err := punning.ParseHeader(punning.EncodeHeader(punning.Header{Version: 1, Length: 12}))
	if err != nil {
		log.Fatal(err)
	}
	fmt.Println(h.Length)
}
```

### Exercise 3: Test The Contract

Create `header_test.go`:

```go
package punning

import (
	"errors"
	"math"
	"testing"
)

func TestParseHeader(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name string
		buf  []byte
		want Header
		err  error
	}{
		{name: "valid", buf: EncodeHeader(Header{Version: 1, Flags: 9, Length: 99}), want: Header{Version: 1, Flags: 9, Length: 99}},
		{name: "short", buf: []byte{1}, err: ErrShortHeader},
		{name: "bad version", buf: EncodeHeader(Header{Version: 2}), err: ErrBadVersion},
	}

	for _, tt := range tests {
		got, err := ParseHeader(tt.buf)
		if !errors.Is(err, tt.err) {
			t.Fatalf("%s: err = %v, want %v", tt.name, err, tt.err)
		}
		if got != tt.want {
			t.Fatalf("%s: got %+v, want %+v", tt.name, got, tt.want)
		}
	}
}

func TestFloatBitsUsesStdlibContract(t *testing.T) {
	t.Parallel()

	if got := FloatBits(math.Pi); got != math.Float64bits(math.Pi) {
		t.Fatalf("FloatBits = %x", got)
	}
}
```

## Common Mistakes

### Punning Without A Layout Test

Wrong: cast bytes to a struct and assume the fields are where the protocol says they are.

Fix: define `HeaderSize`, encode a known header, and test the parsed result.

### Reimplementing `math.Float64bits`

Wrong: use unsafe to inspect every float bit pattern.

Fix: use `math.Float64bits`; it documents exactly what it returns.

### Ignoring Endianness

Wrong: compare type-punned bytes across machines without defining byte order.

Fix: use `encoding/binary` at the serialization boundary and keep the unsafe read behind validation.

## Verification

Run this from `~/go-exercises/punning`:

```bash
test -z "$(gofmt -l .)"
go vet ./...
go build ./...
go test -count=1 -race ./...
```

Add a test that encodes `Flags: 7` and proves `ParseHeader` returns `Flags == 7`.

## Summary

- Type punning reinterprets representation; it does not convert values.
- Use standard library helpers for standard representations.
- Unsafe parsers need explicit length and version validation.
- Tests pin byte layout so future changes do not silently break the contract.

## What's Next

Next: [cgo Basics](../04-cgo-basics/04-cgo-basics.md).

## Resources

- [unsafe package](https://pkg.go.dev/unsafe)
- [math.Float64bits](https://pkg.go.dev/math#Float64bits)
- [encoding/binary](https://pkg.go.dev/encoding/binary)
