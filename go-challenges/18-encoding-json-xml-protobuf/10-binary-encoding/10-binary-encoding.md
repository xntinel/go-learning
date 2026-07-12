# 10. Binary Encoding

Binary formats are compact because every byte has a job. This lesson builds a `packetbin` library with a fixed big-endian header, variable payload, and CRC32 checksum. The tests prove round trips and reject short packets, wrong versions, bad lengths, and corrupted checksums.

## Concepts

### Byte Order Is Part of the Protocol

`binary.BigEndian` is not an implementation detail. It is part of the wire contract. If one side writes little-endian and another reads big-endian, every multi-byte number is wrong.

### Length and Checksum Validate Different Things

The payload length proves how many bytes belong to the payload. The checksum proves those bytes are unchanged. Check both before trusting the decoded packet.

### Accessors Protect Byte Slices

Payloads are mutable slices. A library should copy payloads on input and output so callers cannot mutate internal state accidentally.

## Exercises

Set up the module:

```bash
go mod edit -go=1.26
```

### Exercise 1: Encode and Decode a Packet

Create `packet.go`:

```go
package packetbin

import (
	"encoding/binary"
	"errors"
	"fmt"
	"hash/crc32"
	"time"
)

const (
	Version      uint8 = 1
	TypePing     uint8 = 1
	TypeData     uint8 = 2
	TypeAck      uint8 = 3
	headerSize         = 16
	checksumSize       = 4
	maxPayload         = 65535
)

var ErrInvalidPacket = errors.New("invalid packet")

type Packet struct {
	packetType uint8
	sequence   uint32
	timestamp  time.Time
	payload    []byte
}

func New(packetType uint8, sequence uint32, timestamp time.Time, payload []byte) (Packet, error) {
	p := Packet{packetType: packetType, sequence: sequence, timestamp: timestamp.UTC(), payload: append([]byte(nil), payload...)}
	if err := p.validate(); err != nil {
		return Packet{}, err
	}
	return p, nil
}

func Encode(p Packet) ([]byte, error) {
	if err := p.validate(); err != nil {
		return nil, err
	}
	size := headerSize + len(p.payload) + checksumSize
	out := make([]byte, size)
	out[0] = Version
	out[1] = p.packetType
	binary.BigEndian.PutUint32(out[2:6], p.sequence)
	binary.BigEndian.PutUint64(out[6:14], uint64(p.timestamp.UnixNano()))
	binary.BigEndian.PutUint16(out[14:16], uint16(len(p.payload)))
	copy(out[16:], p.payload)
	sum := crc32.ChecksumIEEE(out[:headerSize+len(p.payload)])
	binary.BigEndian.PutUint32(out[headerSize+len(p.payload):], sum)
	return out, nil
}

func Decode(data []byte) (Packet, error) {
	if len(data) < headerSize+checksumSize {
		return Packet{}, fmt.Errorf("%w: packet too short", ErrInvalidPacket)
	}
	if data[0] != Version {
		return Packet{}, fmt.Errorf("%w: unsupported version %d", ErrInvalidPacket, data[0])
	}
	payloadLen := int(binary.BigEndian.Uint16(data[14:16]))
	wantLen := headerSize + payloadLen + checksumSize
	if len(data) != wantLen {
		return Packet{}, fmt.Errorf("%w: length mismatch", ErrInvalidPacket)
	}
	wantSum := binary.BigEndian.Uint32(data[headerSize+payloadLen:])
	gotSum := crc32.ChecksumIEEE(data[:headerSize+payloadLen])
	if gotSum != wantSum {
		return Packet{}, fmt.Errorf("%w: checksum mismatch", ErrInvalidPacket)
	}
	p := Packet{packetType: data[1], sequence: binary.BigEndian.Uint32(data[2:6]), timestamp: time.Unix(0, int64(binary.BigEndian.Uint64(data[6:14]))).UTC(), payload: append([]byte(nil), data[16:16+payloadLen]...)}
	if err := p.validate(); err != nil {
		return Packet{}, err
	}
	return p, nil
}

func (p Packet) Type() uint8          { return p.packetType }
func (p Packet) Sequence() uint32     { return p.sequence }
func (p Packet) Timestamp() time.Time { return p.timestamp }
func (p Packet) Payload() []byte      { return append([]byte(nil), p.payload...) }

func (p Packet) validate() error {
	switch p.packetType {
	case TypePing, TypeData, TypeAck:
	default:
		return fmt.Errorf("%w: unknown packet type %d", ErrInvalidPacket, p.packetType)
	}
	if len(p.payload) > maxPayload {
		return fmt.Errorf("%w: payload too large", ErrInvalidPacket)
	}
	return nil
}
```

### Exercise 2: Test Round Trips and Corruption

Create `packet_test.go`:

```go
package packetbin

import (
	"errors"
	"fmt"
	"testing"
	"time"
)

func TestEncodeDecode(t *testing.T) {
	t.Parallel()

	now := time.Unix(1700000000, 123).UTC()
	tests := []struct {
		name    string
		packet  Packet
		wantErr error
	}{
		{name: "data packet", packet: Packet{packetType: TypeData, sequence: 42, timestamp: now, payload: []byte("hello")}},
		{name: "unknown type", packet: Packet{packetType: 99, sequence: 42, timestamp: now, payload: []byte("hello")}, wantErr: ErrInvalidPacket},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			encoded, err := Encode(tt.packet)
			if tt.wantErr != nil {
				if !errors.Is(err, tt.wantErr) {
					t.Fatalf("Encode() error = %v, want %v", err, tt.wantErr)
				}
				return
			}
			if err != nil {
				t.Fatal(err)
			}
			decoded, err := Decode(encoded)
			if err != nil {
				t.Fatal(err)
			}
			if decoded.Type() != tt.packet.Type() || decoded.Sequence() != tt.packet.Sequence() || string(decoded.Payload()) != string(tt.packet.Payload()) {
				t.Fatalf("decoded = %#v", decoded)
			}
		})
	}
}

func TestDecodeRejectsInvalidData(t *testing.T) {
	t.Parallel()

	packet, err := New(TypeData, 7, time.Unix(1700000000, 0).UTC(), []byte("payload"))
	if err != nil {
		t.Fatal(err)
	}
	valid, err := Encode(packet)
	if err != nil {
		t.Fatal(err)
	}
	badChecksum := append([]byte(nil), valid...)
	badChecksum[len(badChecksum)-1] ^= 1
	badVersion := append([]byte(nil), valid...)
	badVersion[0] = 2

	tests := []struct {
		name string
		data []byte
	}{
		{name: "short packet", data: []byte{1, 2, 3}},
		{name: "bad checksum", data: badChecksum},
		{name: "bad version", data: badVersion},
		{name: "bad length", data: valid[:len(valid)-1]},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			_, err := Decode(tt.data)
			if !errors.Is(err, ErrInvalidPacket) {
				t.Fatalf("Decode() error = %v, want %v", err, ErrInvalidPacket)
			}
		})
	}
}

func ExampleEncode() {
	packet, _ := New(TypeData, 10, time.Unix(1700000000, 0).UTC(), []byte("go"))
	data, _ := Encode(packet)
	decoded, _ := Decode(data)
	fmt.Println(decoded.Type())
	fmt.Println(decoded.Sequence())
	fmt.Println(string(decoded.Payload()))
	// Output:
	// 2
	// 10
	// go
}
```

Your turn: add a test proving a packet with an invalid decoded type returns `ErrInvalidPacket`.

Create `cmd/demo/main.go`:

```go
package main

import (
	"fmt"
	"log"
	"time"

	"example.com/packetbin"
)

func main() {
	packet, err := packetbin.New(packetbin.TypeData, 42, time.Now().UTC(), []byte("hello"))
	if err != nil {
		log.Fatal(err)
	}
	data, err := packetbin.Encode(packet)
	if err != nil {
		log.Fatal(err)
	}
	decoded, err := packetbin.Decode(data)
	if err != nil {
		log.Fatal(err)
	}
	fmt.Printf("type=%d sequence=%d payload=%q\n", decoded.Type(), decoded.Sequence(), decoded.Payload())
}
```

## Common Mistakes

- Wrong: using host byte order. What happens: the format changes across architectures or implementations. Fix: specify `binary.BigEndian`.
- Wrong: checking CRC32 before checking the packet length. What happens: slice bounds panics. Fix: validate length first.
- Wrong: returning the internal payload slice. What happens: callers mutate packet state. Fix: copy on input and output.

## Verification

From `~/go-exercises/packetbin`:

```bash
test -z "$(gofmt -l .)"
go vet ./...
go build ./...
go test -count=1 -race ./...
go run ./cmd/demo
```

All commands must pass. Add at least one test of your own before considering the lesson complete.

## Summary

- Binary protocols require explicit byte order, lengths, and validation.
- `encoding/binary` handles fixed-size integer layout.
- CRC32 detects accidental corruption but is not authentication.
- Slice accessors should return copies.

## What's Next

Next: [Custom Encoding Format](../11-custom-encoding-format/11-custom-encoding-format.md).

## Resources

- [encoding/binary package documentation](https://pkg.go.dev/encoding/binary)
- [hash/crc32 package documentation](https://pkg.go.dev/hash/crc32)
- [binary.ByteOrder documentation](https://pkg.go.dev/encoding/binary#ByteOrder)
- [errors.Is documentation](https://pkg.go.dev/errors#Is)
