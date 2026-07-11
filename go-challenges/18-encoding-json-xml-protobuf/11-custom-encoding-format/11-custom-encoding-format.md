# 11. Custom Encoding Format

A custom encoding format is a contract, not a byte-writing trick. This lesson builds a `cfmt` library with magic bytes, a version byte, varint lengths, strict trailing-byte checks, wrapped sentinel errors, table-driven tests, a verified example, and a demo that consumes the exported API.

## Concepts

### Magic and Version Bytes Make Failure Explicit

The first bytes identify the format and version. A decoder that cannot recognize them should fail immediately instead of trying to interpret unrelated data as a valid message.

### Length Prefixes Need Bounds

Length-prefixed strings and arrays are simple, but hostile or corrupt data can claim impossible lengths. Check lengths against remaining bytes and impose reasonable count limits.

### A Format Should Reject Trailing Data

If one message is expected, trailing bytes are suspicious. Rejecting them catches concatenation mistakes and partial parser bugs.

## Exercises

Set up the module:

```bash
mkdir -p ~/go-exercises/custom-format/cmd/demo
cd ~/go-exercises/custom-format
go mod init example.com/custom-format
go mod edit -go=1.26
```

### Exercise 1: Implement the CEF1 Format

Create `event.go`:

```go
package cfmt

import (
	"bytes"
	"encoding/binary"
	"errors"
	"fmt"
	"io"
)

var (
	ErrInvalidFormat      = errors.New("invalid custom encoding format")
	ErrUnsupportedVersion = errors.New("unsupported custom encoding version")
)

var magic = []byte{'C', 'E', 'F', '1'}

type Event struct {
	id     uint64
	name   string
	active bool
	tags   []string
}

func NewEvent(id uint64, name string, active bool, tags []string) Event {
	return Event{id: id, name: name, active: active, tags: append([]string(nil), tags...)}
}

func (e Event) ID() uint64     { return e.id }
func (e Event) Name() string   { return e.name }
func (e Event) Active() bool   { return e.active }
func (e Event) Tags() []string { return append([]string(nil), e.tags...) }

func Marshal(e Event) ([]byte, error) {
	var b bytes.Buffer
	b.Write(magic)
	writeUvarint(&b, e.id)
	writeString(&b, e.name)
	if e.active {
		b.WriteByte(1)
	} else {
		b.WriteByte(0)
	}
	writeUvarint(&b, uint64(len(e.tags)))
	for _, tag := range e.tags {
		writeString(&b, tag)
	}
	return b.Bytes(), nil
}

func Unmarshal(data []byte) (Event, error) {
	r := bytes.NewReader(data)
	header := make([]byte, len(magic))
	if _, err := io.ReadFull(r, header); err != nil {
		return Event{}, fmt.Errorf("%w: reading magic: %v", ErrInvalidFormat, err)
	}
	if !bytes.Equal(header[:3], magic[:3]) {
		return Event{}, fmt.Errorf("%w: bad magic", ErrInvalidFormat)
	}
	if header[3] != magic[3] {
		return Event{}, fmt.Errorf("%w: version byte %q", ErrUnsupportedVersion, header[3])
	}
	id, err := readUvarint(r)
	if err != nil {
		return Event{}, err
	}
	name, err := readString(r)
	if err != nil {
		return Event{}, err
	}
	flag, err := r.ReadByte()
	if err != nil {
		return Event{}, fmt.Errorf("%w: reading active flag: %v", ErrInvalidFormat, err)
	}
	if flag > 1 {
		return Event{}, fmt.Errorf("%w: invalid active flag %d", ErrInvalidFormat, flag)
	}
	count, err := readUvarint(r)
	if err != nil {
		return Event{}, err
	}
	if count > 1024 {
		return Event{}, fmt.Errorf("%w: too many tags", ErrInvalidFormat)
	}
	tags := make([]string, 0, count)
	for i := uint64(0); i < count; i++ {
		tag, err := readString(r)
		if err != nil {
			return Event{}, err
		}
		tags = append(tags, tag)
	}
	if r.Len() != 0 {
		return Event{}, fmt.Errorf("%w: trailing bytes", ErrInvalidFormat)
	}
	return NewEvent(id, name, flag == 1, tags), nil
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
		return "", fmt.Errorf("%w: string length exceeds input", ErrInvalidFormat)
	}
	buf := make([]byte, n)
	if _, err := io.ReadFull(r, buf); err != nil {
		return "", fmt.Errorf("%w: reading string: %v", ErrInvalidFormat, err)
	}
	return string(buf), nil
}

func readUvarint(r *bytes.Reader) (uint64, error) {
	v, err := binary.ReadUvarint(r)
	if err != nil {
		return 0, fmt.Errorf("%w: reading uvarint: %v", ErrInvalidFormat, err)
	}
	return v, nil
}
```

### Exercise 2: Test Round Trips and Malformed Input

Create `event_test.go`:

```go
package cfmt

import (
	"errors"
	"fmt"
	"reflect"
	"testing"
)

func TestRoundTrip(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name  string
		event Event
	}{
		{name: "empty tags", event: NewEvent(1, "created", true, nil)},
		{name: "with tags", event: NewEvent(99, "shipped", false, []string{"order", "priority"})},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			data, err := Marshal(tc.event)
			if err != nil {
				t.Fatal(err)
			}
			got, err := Unmarshal(data)
			if err != nil {
				t.Fatal(err)
			}
			if got.ID() != tc.event.ID() || got.Name() != tc.event.Name() || got.Active() != tc.event.Active() {
				t.Fatalf("decoded scalar fields mismatch: got %#v want %#v", got, tc.event)
			}
			if !reflect.DeepEqual(got.Tags(), tc.event.Tags()) {
				t.Fatalf("tags = %#v, want %#v", got.Tags(), tc.event.Tags())
			}
		})
	}
}

func TestUnmarshalErrors(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name string
		data []byte
		want error
	}{
		{name: "short input", data: []byte("CE"), want: ErrInvalidFormat},
		{name: "bad magic", data: []byte("BAD1"), want: ErrInvalidFormat},
		{name: "unsupported version", data: []byte("CEF2"), want: ErrUnsupportedVersion},
		{name: "invalid bool", data: []byte{'C', 'E', 'F', '1', 1, 0, 2, 0}, want: ErrInvalidFormat},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			_, err := Unmarshal(tc.data)
			if !errors.Is(err, tc.want) {
				t.Fatalf("Unmarshal() error = %v, want %v", err, tc.want)
			}
		})
	}
}

func Example() {
	event := NewEvent(7, "indexed", true, []string{"search"})
	data, _ := Marshal(event)
	decoded, _ := Unmarshal(data)
	fmt.Println(decoded.ID(), decoded.Name(), decoded.Active(), decoded.Tags()[0])
	// Output:
	// 7 indexed true search
}
```

Your turn: add a test that appends one extra byte to a valid message and expects `ErrInvalidFormat`.

Create `cmd/demo/main.go`:

```go
package main

import (
	"fmt"
	"log"

	cfmt "example.com/custom-format"
)

func main() {
	event := cfmt.NewEvent(42, "published", true, []string{"news", "public"})
	data, err := cfmt.Marshal(event)
	if err != nil {
		log.Fatal(err)
	}
	decoded, err := cfmt.Unmarshal(data)
	if err != nil {
		log.Fatal(err)
	}
	fmt.Printf("%d %s %t %v\n", decoded.ID(), decoded.Name(), decoded.Active(), decoded.Tags())
}
```

## Common Mistakes

- Wrong: skipping magic bytes and version bytes. What happens: random data can be misread as your format. Fix: identify the format first.
- Wrong: accepting trailing bytes. What happens: parser bugs and concatenated messages go unnoticed. Fix: check `r.Len() == 0`.
- Wrong: returning internal tag slices directly. What happens: callers mutate decoded values. Fix: return copies.

## Verification

From `~/go-exercises/custom-format`:

```bash
test -z "$(gofmt -l .)"
go vet ./...
go build ./...
go test -count=1 -race ./...
go run ./cmd/demo
```

All commands must pass. Add at least one test of your own before considering the lesson complete.

## Summary

- Custom formats need magic bytes, versioning, and strict parse boundaries.
- Varint lengths are compact but must be checked against the remaining input.
- Wrapped sentinel errors make malformed-data cases testable.
- A demo should import the library rather than contain the library logic.

## What's Next

Next: [Performance -- JSON vs Protobuf vs MessagePack](../12-performance-json-protobuf-msgpack/12-performance-json-protobuf-msgpack.md).

## Resources

- [encoding/binary package documentation](https://pkg.go.dev/encoding/binary)
- [binary.ReadUvarint documentation](https://pkg.go.dev/encoding/binary#ReadUvarint)
- [bytes.Reader documentation](https://pkg.go.dev/bytes#Reader)
- [errors.Is documentation](https://pkg.go.dev/errors#Is)
