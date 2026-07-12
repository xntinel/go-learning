# 8. Protocol Buffers

Real protobuf work uses `.proto` files, `protoc`, generated Go types, and `google.golang.org/protobuf`. This offline lesson cannot fetch those dependencies, so it teaches the wire-format ideas with a small standard-library codec: field numbers, wire types, varints, and length-delimited nested messages.

## Concepts

### Field Keys Combine Number and Wire Type

A protobuf field key is encoded as a varint: `field_number << 3 | wire_type`. This lesson implements wire type `0` for varints and wire type `2` for length-delimited bytes.

### Varints Need Careful Error Handling

`binary.Uvarint` returns `(value, n)`. A positive `n` means success, `n == 0` means the buffer is too small, and `n < 0` means overflow. Treating all three cases the same hides malformed input.

### Unknown Fields Are a Policy Choice

Real protobuf decoders preserve or skip unknown fields for compatibility. This teaching codec rejects unknown fields so tests can make every branch explicit.

## Exercises

Set up the module:

```bash
mkdir -p go-solutions/18-encoding-json-xml-protobuf/08-protocol-buffers/08-protocol-buffers/cmd/demo
cd go-solutions/18-encoding-json-xml-protobuf/08-protocol-buffers/08-protocol-buffers
go mod edit -go=1.26
```

### Exercise 1: Encode Contacts With Varint Fields

Create `book.go`:

```go
package codecbook

import (
	"encoding/binary"
	"errors"
	"fmt"
)

var (
	ErrInvalidWire = errors.New("invalid wire data")
	ErrTruncated   = errors.New("truncated wire data")
)

type PhoneType uint64

const (
	PhoneMobile PhoneType = 1
	PhoneHome   PhoneType = 2
	PhoneWork   PhoneType = 3
)

type Phone struct {
	number string
	typ    PhoneType
}

func NewPhone(number string, typ PhoneType) Phone { return Phone{number: number, typ: typ} }
func (p Phone) Number() string                    { return p.number }
func (p Phone) Type() PhoneType                   { return p.typ }

type Contact struct {
	id     uint64
	name   string
	email  string
	phones []Phone
}

func NewContact(id uint64, name, email string, phones []Phone) Contact {
	return Contact{id: id, name: name, email: email, phones: append([]Phone(nil), phones...)}
}

func (c Contact) ID() uint64      { return c.id }
func (c Contact) Name() string    { return c.name }
func (c Contact) Email() string   { return c.email }
func (c Contact) Phones() []Phone { return append([]Phone(nil), c.phones...) }

func EncodeContact(c Contact) []byte {
	var out []byte
	out = appendVarintField(out, 1, c.id)
	out = appendBytesField(out, 2, []byte(c.name))
	out = appendBytesField(out, 3, []byte(c.email))
	for _, phone := range c.phones {
		out = appendBytesField(out, 4, encodePhone(phone))
	}
	return out
}

func DecodeContact(data []byte) (Contact, error) {
	var c Contact
	for len(data) > 0 {
		field, wire, rest, err := readKey(data)
		if err != nil {
			return Contact{}, err
		}
		data = rest
		switch field {
		case 1:
			if wire != 0 {
				return Contact{}, fmt.Errorf("id: %w", ErrInvalidWire)
			}
			v, rest, err := readVarint(data)
			if err != nil {
				return Contact{}, fmt.Errorf("id: %w", err)
			}
			c.id = v
			data = rest
		case 2:
			v, rest, err := readLengthDelimited(data, wire)
			if err != nil {
				return Contact{}, fmt.Errorf("name: %w", err)
			}
			c.name = string(v)
			data = rest
		case 3:
			v, rest, err := readLengthDelimited(data, wire)
			if err != nil {
				return Contact{}, fmt.Errorf("email: %w", err)
			}
			c.email = string(v)
			data = rest
		case 4:
			v, rest, err := readLengthDelimited(data, wire)
			if err != nil {
				return Contact{}, fmt.Errorf("phone: %w", err)
			}
			phone, err := decodePhone(v)
			if err != nil {
				return Contact{}, fmt.Errorf("phone: %w", err)
			}
			c.phones = append(c.phones, phone)
			data = rest
		default:
			return Contact{}, fmt.Errorf("field %d: %w", field, ErrInvalidWire)
		}
	}
	return c, nil
}

func encodePhone(p Phone) []byte {
	var out []byte
	out = appendBytesField(out, 1, []byte(p.number))
	out = appendVarintField(out, 2, uint64(p.typ))
	return out
}

func decodePhone(data []byte) (Phone, error) {
	var p Phone
	for len(data) > 0 {
		field, wire, rest, err := readKey(data)
		if err != nil {
			return Phone{}, err
		}
		data = rest
		switch field {
		case 1:
			v, rest, err := readLengthDelimited(data, wire)
			if err != nil {
				return Phone{}, fmt.Errorf("number: %w", err)
			}
			p.number = string(v)
			data = rest
		case 2:
			if wire != 0 {
				return Phone{}, fmt.Errorf("type: %w", ErrInvalidWire)
			}
			v, rest, err := readVarint(data)
			if err != nil {
				return Phone{}, fmt.Errorf("type: %w", err)
			}
			p.typ = PhoneType(v)
			data = rest
		default:
			return Phone{}, fmt.Errorf("phone field %d: %w", field, ErrInvalidWire)
		}
	}
	return p, nil
}

func appendKey(out []byte, field, wire uint64) []byte {
	return binary.AppendUvarint(out, field<<3|wire)
}

func appendVarintField(out []byte, field, value uint64) []byte {
	out = appendKey(out, field, 0)
	return binary.AppendUvarint(out, value)
}

func appendBytesField(out []byte, field uint64, value []byte) []byte {
	out = appendKey(out, field, 2)
	out = binary.AppendUvarint(out, uint64(len(value)))
	return append(out, value...)
}

func readKey(data []byte) (uint64, uint64, []byte, error) {
	key, rest, err := readVarint(data)
	if err != nil {
		return 0, 0, nil, fmt.Errorf("key: %w", err)
	}
	field := key >> 3
	wire := key & 7
	if field == 0 {
		return 0, 0, nil, fmt.Errorf("field zero: %w", ErrInvalidWire)
	}
	return field, wire, rest, nil
}

func readVarint(data []byte) (uint64, []byte, error) {
	value, n := binary.Uvarint(data)
	if n > 0 {
		return value, data[n:], nil
	}
	if n == 0 {
		return 0, nil, ErrTruncated
	}
	return 0, nil, ErrInvalidWire
}

func readLengthDelimited(data []byte, wire uint64) ([]byte, []byte, error) {
	if wire != 2 {
		return nil, nil, ErrInvalidWire
	}
	size, rest, err := readVarint(data)
	if err != nil {
		return nil, nil, err
	}
	if size > uint64(len(rest)) {
		return nil, nil, ErrTruncated
	}
	return rest[:size], rest[size:], nil
}
```

### Exercise 2: Test Round Trips and Wire Errors

Create `book_test.go`:

```go
package codecbook

import (
	"errors"
	"fmt"
	"testing"
)

func TestContactRoundTrip(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name    string
		contact Contact
	}{
		{name: "without phones", contact: NewContact(7, "Ada", "ada@example.test", nil)},
		{name: "with phones", contact: NewContact(42, "Grace", "grace@example.test", []Phone{NewPhone("111-222", PhoneMobile), NewPhone("333-444", PhoneWork)})},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			got, err := DecodeContact(EncodeContact(tt.contact))
			if err != nil {
				t.Fatal(err)
			}
			if got.ID() != tt.contact.ID() || got.Name() != tt.contact.Name() || got.Email() != tt.contact.Email() {
				t.Fatalf("decoded contact = %#v, want %#v", got, tt.contact)
			}
			if len(got.Phones()) != len(tt.contact.Phones()) {
				t.Fatalf("phones = %d, want %d", len(got.Phones()), len(tt.contact.Phones()))
			}
		})
	}
}

func TestDecodeErrors(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name string
		data []byte
		want error
	}{
		{name: "truncated key", data: []byte{0x80}, want: ErrTruncated},
		{name: "field zero", data: []byte{0x00}, want: ErrInvalidWire},
		{name: "wrong id wire type", data: []byte{0x0a, 0x00}, want: ErrInvalidWire},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			_, err := DecodeContact(tt.data)
			if !errors.Is(err, tt.want) {
				t.Fatalf("DecodeContact() error = %v, want %v", err, tt.want)
			}
		})
	}
}

func ExampleEncodeContact() {
	contact := NewContact(9, "Lin", "lin@example.test", []Phone{NewPhone("555-0100", PhoneHome)})
	decoded, _ := DecodeContact(EncodeContact(contact))
	fmt.Println(decoded.ID(), decoded.Name(), decoded.Phones()[0].Number())
	// Output:
	// 9 Lin 555-0100
}
```

Your turn: add a test proving an unknown top-level field returns `ErrInvalidWire`.

Create `cmd/demo/main.go`:

```go
package main

import (
	"fmt"
	"log"

	"example.com/codecbook"
)

func main() {
	contact := codecbook.NewContact(1, "Ada Lovelace", "ada@example.test", []codecbook.Phone{codecbook.NewPhone("555-0101", codecbook.PhoneMobile)})
	data := codecbook.EncodeContact(contact)
	decoded, err := codecbook.DecodeContact(data)
	if err != nil {
		log.Fatal(err)
	}
	fmt.Printf("%d %s %s\n", decoded.ID(), decoded.Name(), decoded.Phones()[0].Number())
}
```

## Common Mistakes

- Wrong: treating protobuf as JSON with smaller syntax. What happens: the lesson misses field numbers and wire types. Fix: encode keys and varints explicitly.
- Wrong: ignoring `binary.Uvarint`'s `n` value. What happens: truncated data and overflow look successful. Fix: branch on positive, zero, and negative `n`.
- Wrong: importing generated protobuf packages in an offline exercise. What happens: verification needs network access. Fix: keep this lesson stdlib-only and defer generated protobuf to a networked run.

## Verification

From `~/go-exercises/codecbook`:

```bash
test -z "$(gofmt -l .)"
go vet ./...
go build ./...
go test -count=1 -race ./...
go run ./cmd/demo
```

All commands must pass. Add at least one test of your own before considering the lesson complete.

## Summary

- Protobuf-style wire data is field-numbered binary data, not a text format.
- Varints save space but require careful error handling.
- Length-delimited fields can contain strings, bytes, or nested messages.
- This offline codec teaches the wire model without third-party generated code.

## What's Next

Next: [gRPC Service](../09-grpc-service/09-grpc-service.md).

## Resources

- [encoding/binary package documentation](https://pkg.go.dev/encoding/binary)
- [binary.Uvarint documentation](https://pkg.go.dev/encoding/binary#Uvarint)
- [Protocol Buffers encoding guide](https://protobuf.dev/programming-guides/encoding/)
- [errors.Is documentation](https://pkg.go.dev/errors#Is)
