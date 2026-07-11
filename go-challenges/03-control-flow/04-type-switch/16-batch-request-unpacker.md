# Exercise 16: Unpack Heterogeneous Batch Requests by Decoded Format

**Nivel: Intermedio** — validacion rapida (un test corto).

A message-queue consumer fans in batches of items from several producers
that were never made to agree on a wire format: a legacy producer writes a
length-prefixed binary frame, an HTTP-originated producer forwards the
request body as raw JSON text, and an internal producer hands off a value
already decoded one layer up as `[]any`. The consumer receives all three as
`any` off the queue and must unpack each into the same `[]string` shape
before it can process a single item.

## What you'll build

```text
batch-request-unpacker/     independent module: example.com/batch-request-unpacker
  go.mod                     go 1.24
  batchunpack.go             Unpack(v any) ([]string, error)
  cmd/
    demo/
      main.go                unpacks one batch of each supported format
  batchunpack_test.go         table test over every format plus malformed input
```

- Files: `batchunpack.go`, `cmd/demo/main.go`, `batchunpack_test.go`.
- Implement: `Unpack(v any) ([]string, error)`, type-switching on `[]byte`
  (length-prefixed binary), `string` (JSON array text), and `[]any`
  (already-decoded JSON array).
- Test: a valid binary frame, JSON array text, a decoded `[]any`, a
  truncated binary frame, a `[]any` with a non-string element, malformed
  JSON text, and an unsupported type.

Set up the module:

```bash
mkdir -p ~/go-exercises/batch-request-unpacker/cmd/demo
cd ~/go-exercises/batch-request-unpacker
go mod init example.com/batch-request-unpacker
go mod edit -go=1.24
```

The binary format is a repeated sequence of records, each a 4-byte
big-endian length prefix followed by that many payload bytes — the
mechanism `encoding/binary` exists for, and one common enough at a queue
boundary that a truncated buffer (a partial write to the network, or a
crash mid-append) must be detected rather than silently under- or
over-read. `Unpack` treats a length prefix that claims more bytes than
remain in the buffer as `ErrMalformedBatch`, not a panic — a naive
implementation that slices `b[4 : 4+n]` without checking `len(b) >= 4+n`
first would panic on a truncated frame instead of returning an error the
caller can log and skip. The JSON-text and already-decoded-`[]any` cases
share the same target shape (`[]string`) but arrive from different layers
of the pipeline: a producer that talks HTTP hands the consumer the raw
request body (`string`), while an upstream decoder that has already run
`json.Unmarshal` hands it the parsed slice directly (`[]any`), and the
`[]any` case must still validate that every element is a string rather than
assuming the upstream decoder only ever produces one shape.

Create `batchunpack.go`:

```go
package batchunpack

import (
	"encoding/binary"
	"encoding/json"
	"errors"
	"fmt"
)

// ErrMalformedBatch is the sentinel for a batch payload that cannot be
// unpacked into its list of items, either because its wire format is
// truncated or because one of its items is not a string.
var ErrMalformedBatch = errors.New("malformed batch payload")

// Unpack decodes a batch of string items from whichever format the consumer
// received it in. A message-queue consumer that fans in from several
// producers sees three different shapes for the same logical "batch of
// items": a raw length-prefixed binary frame from a legacy producer, a JSON
// array encoded as text from an HTTP-originated producer, and an
// already-decoded []any from an upstream JSON decoder. Each shape needs its
// own unpacking rule, chosen by the payload's concrete Go type.
func Unpack(v any) ([]string, error) {
	switch p := v.(type) {
	case []byte:
		return unpackBinary(p)
	case string:
		var items []string
		if err := json.Unmarshal([]byte(p), &items); err != nil {
			return nil, fmt.Errorf("%w: json array text: %v", ErrMalformedBatch, err)
		}
		return items, nil
	case []any:
		items := make([]string, 0, len(p))
		for i, elem := range p {
			s, ok := elem.(string)
			if !ok {
				return nil, fmt.Errorf("%w: element %d is %T, not string", ErrMalformedBatch, i, elem)
			}
			items = append(items, s)
		}
		return items, nil
	default:
		return nil, fmt.Errorf("%w: unsupported payload type %T", ErrMalformedBatch, v)
	}
}

// unpackBinary parses the legacy wire format: a repeated sequence of records,
// each a 4-byte big-endian length prefix followed by that many payload
// bytes, until the buffer is exhausted.
func unpackBinary(b []byte) ([]string, error) {
	var items []string
	for len(b) > 0 {
		if len(b) < 4 {
			return nil, fmt.Errorf("%w: truncated length prefix (%d bytes left)", ErrMalformedBatch, len(b))
		}
		n := binary.BigEndian.Uint32(b[:4])
		b = b[4:]
		if uint64(len(b)) < uint64(n) {
			return nil, fmt.Errorf("%w: truncated record (want %d bytes, have %d)", ErrMalformedBatch, n, len(b))
		}
		items = append(items, string(b[:n]))
		b = b[n:]
	}
	return items, nil
}

// EncodeBinary is the inverse of unpackBinary, used by the demo and tests to
// build a valid binary-packed frame from a list of items.
func EncodeBinary(items []string) []byte {
	var out []byte
	for _, item := range items {
		var lenBuf [4]byte
		binary.BigEndian.PutUint32(lenBuf[:], uint32(len(item)))
		out = append(out, lenBuf[:]...)
		out = append(out, item...)
	}
	return out
}
```

### The runnable demo

Create `cmd/demo/main.go`:

```go
package main

import (
	"fmt"
	"log"

	"example.com/batch-request-unpacker"
)

func main() {
	binaryFrame := batchunpack.EncodeBinary([]string{"order-1", "order-2"})
	batches := []any{
		binaryFrame,
		`["invoice-9","invoice-10"]`,
		[]any{"refund-3", "refund-4"},
	}
	for _, b := range batches {
		items, err := batchunpack.Unpack(b)
		if err != nil {
			log.Printf("reject: %v", err)
			continue
		}
		fmt.Printf("%T -> %v\n", b, items)
	}
}
```

```bash
go run ./cmd/demo
```

Expected output:

```text
[]uint8 -> [order-1 order-2]
string -> [invoice-9 invoice-10]
[]interface {} -> [refund-3 refund-4]
```

### Tests

The truncated-binary case reuses `EncodeBinary` to build a valid frame and
then slices one byte off the end, so the test proves the truncation check
fires on a real encoding rather than on a hand-built malformed buffer that
might not resemble what a genuine partial write produces.

Create `batchunpack_test.go`:

```go
package batchunpack

import (
	"errors"
	"reflect"
	"testing"
)

func TestUnpack(t *testing.T) {
	t.Parallel()

	binaryFrame := EncodeBinary([]string{"order-1", "order-2"})
	truncated := binaryFrame[:len(binaryFrame)-1]

	tests := []struct {
		name    string
		value   any
		want    []string
		wantErr bool
	}{
		{"binary frame", binaryFrame, []string{"order-1", "order-2"}, false},
		{"json array text", `["a","b","c"]`, []string{"a", "b", "c"}, false},
		{"decoded any slice", []any{"x", "y"}, []string{"x", "y"}, false},
		{"truncated binary frame", truncated, nil, true},
		{"non-string element in any slice", []any{"x", 2}, nil, true},
		{"malformed json text", `not json`, nil, true},
		{"unsupported type", 7, nil, true},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			got, err := Unpack(tt.value)
			if tt.wantErr {
				if !errors.Is(err, ErrMalformedBatch) {
					t.Fatalf("Unpack(%v) err = %v, want ErrMalformedBatch", tt.value, err)
				}
				return
			}
			if err != nil {
				t.Fatalf("Unpack(%v) unexpected error: %v", tt.value, err)
			}
			if !reflect.DeepEqual(got, tt.want) {
				t.Fatalf("Unpack(%v) = %v, want %v", tt.value, got, tt.want)
			}
		})
	}
}
```

Verify: `go test -count=1 ./...`

## Review

The binary unpacker is correct because it always checks remaining buffer
length against the claimed record size before slicing, both for the 4-byte
length prefix and for the payload it describes — either check missing turns
a truncated frame into a panic instead of `ErrMalformedBatch`. The `[]any`
case is correct because it validates every element's type on the way
through rather than trusting that "decoded from JSON" implies "every
element is a string"; a JSON array can freely mix types, and only this
consumer's contract says otherwise. The one thing worth watching if you
extend this: adding a fourth wire format means adding a `case` here, and
`default` returning a typed error is what keeps that omission loud instead
of a fourth format silently landing on the unsupported-type path.

## Resources

- [Go Specification: Type switches](https://go.dev/ref/spec#Type_switches)
- [encoding/binary](https://pkg.go.dev/encoding/binary)
- [encoding/json](https://pkg.go.dev/encoding/json)

---

Back to [00-concepts.md](00-concepts.md) | Prev: [15-idempotency-key-validator.md](15-idempotency-key-validator.md) | Next: [17-health-check-router.md](17-health-check-router.md)
