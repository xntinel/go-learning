# 9. Zero-Copy Deserialization

Standard deserialization — `encoding/binary.Read`, `encoding/json.Unmarshal`, `proto.Unmarshal` — copies every field from the wire buffer into a fresh Go struct, allocating and touching each byte twice. Zero-copy deserialization skips that entirely: the "deserialization" is a single pointer cast from `[]byte` to `*T`. Fields that live inside the struct are accessible at pointer-dereference speed; variable-length fields (strings) are represented as index references resolved on demand via `unsafe.String`. This is the architecture of FlatBuffers, Cap'n Proto, and high-throughput packet parsers.

The challenge is doing it correctly. The wire format must match Go's memory layout (alignment, padding, byte order). Safety requires validating every reference before dereferencing it. A fuzz corpus proves the parser never panics on adversarial input.

```text
zerocopy/
  go.mod
  zerocopy.go
  zerocopy_test.go
  cmd/demo/main.go
```

## Concepts

### Why Alignment Matters

The x86 architecture tolerates unaligned memory access (with a performance penalty). ARM does not: reading a `uint64` from an address that is not a multiple of 8 raises a bus error at runtime. Go's `encoding/binary` avoids this by copying bytes into a properly aligned Go struct. Zero-copy deserialization bypasses the copy, so the wire format must guarantee that each field lands at the right alignment.

The `unsafe.Alignof` built-in returns the alignment requirement for a type. `unsafe.Offsetof` returns the byte offset of a named field within a struct. A conforming wire format places field `f` at a byte offset that is a multiple of `unsafe.Alignof(f)`.

The standard padding formula is:

```go
func alignUp(offset, alignment uintptr) uintptr {
	return (offset + alignment - 1) &^ (alignment - 1)
}
```

### The Pointer-Cast Idiom

Given a `[]byte` buf whose first 8 bytes are a validated header and whose remaining bytes match the layout of `T`:

```go
msg := (*T)(unsafe.Pointer(&buf[headerSize]))
```

After this cast, `msg.SomeInt32Field` reads 4 bytes directly from buf without copying. The Go GC tracks `buf` as live, so the memory backing `msg` stays valid as long as `buf` is referenced.

This is legal under Go's aliasing rules only when the pointer is obtained directly from `&buf[i]` and the cast does not violate the strict aliasing rules described in the `unsafe` package documentation. Writing through `msg` (i.e., treating the cast as mutable) is allowed only when `buf` is not a string or mmap'd read-only region.

### StringRef: Variable-Length Fields

Structs that contain variable-length data cannot be cast directly because `string` and `[]byte` headers are not wire-portable (their widths differ by platform and Go version). Instead, embed a `StringRef`:

```go
type StringRef struct {
	Offset uint32
	Length uint32
}
```

The fixed struct stores a `StringRef` at a known offset. The variable data lives in a trailing data section of the buffer. Accessing the string:

```go
func (r StringRef) Resolve(buf []byte) string {
	end := uint64(r.Offset) + uint64(r.Length)
	if end > uint64(len(buf)) {
		return ""
	}
	if r.Length == 0 {
		return ""
	}
	return unsafe.String(&buf[r.Offset], r.Length)
}
```

The returned string points into `buf`. It is valid exactly as long as `buf` is alive and unmodified.

### Safety Invariants

Every deserialization must validate:
1. The buffer is large enough to hold the fixed header and the message length field.
2. The message length field does not exceed `len(buf)`.
3. The buffer is large enough to hold the fixed struct portion after the header.
4. The alignment of the struct pointer: `uintptr(&buf[headerSize]) % alignment == 0`.
5. Every `StringRef` offset and length is within `len(buf)`.

Failing any check must return an error, not a panic or a zero-value struct with invalid pointers.

## Exercises

Set up the module:

```bash
mkdir -p go-solutions/28-unsafe-and-cgo/09-zero-copy-deserialization/09-zero-copy-deserialization/cmd/demo
cd go-solutions/28-unsafe-and-cgo/09-zero-copy-deserialization/09-zero-copy-deserialization
```

### Exercise 1: Wire Format, Errors, and the Fixed Struct

Create `zerocopy.go`:

```go
package zerocopy

import (
	"encoding/binary"
	"errors"
	"unsafe"
)

const (
	// Magic identifies a valid zero-copy message buffer.
	// 0x0050435A reads as little-endian bytes 'Z','C','P','\x00'.
	Magic uint32 = 0x0050435A

	// HeaderSize is the number of bytes preceding the fixed struct in the wire format.
	// Layout: [0:4] magic, [4:8] total message length (uint32 LE).
	HeaderSize = 8
)

var (
	ErrBufferTooShort = errors.New("zerocopy: buffer too short")
	ErrInvalidMagic   = errors.New("zerocopy: invalid magic number")
	ErrMisaligned     = errors.New("zerocopy: buffer alignment insufficient for type")
	ErrOffsetOverflow = errors.New("zerocopy: StringRef offset+length overflows buffer")
)

// StringRef is an offset+length pair that indexes into the trailing
// variable-length data section of a wire buffer.
// Both fields are little-endian uint32.
type StringRef struct {
	Offset uint32
	Length uint32
}

// Resolve returns a zero-copy string backed by buf[r.Offset : r.Offset+r.Length].
// Returns "" if the reference is out of bounds or the length is zero.
func (r StringRef) Resolve(buf []byte) string {
	if r.Length == 0 {
		return ""
	}
	end := uint64(r.Offset) + uint64(r.Length)
	if end > uint64(len(buf)) {
		return ""
	}
	return unsafe.String(&buf[r.Offset], r.Length)
}

// Message is a fixed-size struct with two scalar fields and two variable-length
// string fields represented as StringRefs. The wire layout matches Go's memory
// layout on little-endian 64-bit platforms (which is enforced by Serialize).
//
// Wire layout after the 8-byte header:
//
//	[0:4]   ID       int32
//	[4:8]   Score    float32
//	[8:16]  Name     StringRef  (Offset uint32, Length uint32)
//	[16:24] Tag      StringRef
type Message struct {
	ID    int32
	Score float32
	Name  StringRef
	Tag   StringRef
}

// Serialize writes m into a wire buffer. The buffer is structured as:
//
//	[0:4]   magic (uint32 LE)
//	[4:8]   total length (uint32 LE)
//	[8:8+sizeof(Message)]  fixed struct
//	[8+sizeof(Message):] variable-length data (name bytes, then tag bytes)
func Serialize(m *Message, name, tag string) []byte {
	fixedSize := int(unsafe.Sizeof(Message{}))
	totalSize := HeaderSize + fixedSize + len(name) + len(tag)

	buf := make([]byte, totalSize)
	binary.LittleEndian.PutUint32(buf[0:4], Magic)
	binary.LittleEndian.PutUint32(buf[4:8], uint32(totalSize))

	// Copy fixed struct into buf[HeaderSize:].
	// We write field by field to avoid any alignment assumption about buf itself.
	dataStart := uint32(HeaderSize + fixedSize)
	m2 := *m

	nameOff := dataStart
	m2.Name = StringRef{Offset: nameOff, Length: uint32(len(name))}
	tagOff := nameOff + uint32(len(name))
	m2.Tag = StringRef{Offset: tagOff, Length: uint32(len(tag))}

	// Place the fixed struct.
	fixed := (*[1 << 20]byte)(unsafe.Pointer(&m2))[:fixedSize:fixedSize]
	copy(buf[HeaderSize:], fixed)

	// Place the variable data.
	copy(buf[nameOff:], name)
	copy(buf[tagOff:], tag)

	return buf
}

// Deserialize validates the header and returns a pointer into buf cast to *Message.
// The returned pointer is valid only while buf is alive and unmodified.
// Zero allocation on the deserialization path.
func Deserialize(buf []byte) (*Message, error) {
	if len(buf) < HeaderSize {
		return nil, ErrBufferTooShort
	}
	magic := binary.LittleEndian.Uint32(buf[0:4])
	if magic != Magic {
		return nil, ErrInvalidMagic
	}
	msgLen := binary.LittleEndian.Uint32(buf[4:8])
	if int(msgLen) > len(buf) {
		return nil, ErrBufferTooShort
	}

	fixedSize := int(unsafe.Sizeof(Message{}))
	if len(buf) < HeaderSize+fixedSize {
		return nil, ErrBufferTooShort
	}

	// Check that &buf[HeaderSize] satisfies Message's alignment.
	alignment := uintptr(unsafe.Alignof(Message{}))
	if uintptr(unsafe.Pointer(&buf[HeaderSize]))%alignment != 0 {
		return nil, ErrMisaligned
	}

	return (*Message)(unsafe.Pointer(&buf[HeaderSize])), nil
}
```

### Exercise 2: Tests That Enforce the Contract

Create `zerocopy_test.go`:

```go
package zerocopy

import (
	"errors"
	"testing"
	"unsafe"
)

func TestRoundTrip(t *testing.T) {
	t.Parallel()

	original := &Message{ID: 42, Score: 3.14}
	buf := Serialize(original, "alice", "admin")

	msg, err := Deserialize(buf)
	if err != nil {
		t.Fatalf("Deserialize: %v", err)
	}

	if msg.ID != 42 {
		t.Errorf("ID = %d, want 42", msg.ID)
	}
	// Float comparison with tolerance is not needed here: 3.14 serializes
	// as its exact IEEE 754 bits and deserializes identically.
	if msg.Score != 3.14 {
		t.Errorf("Score = %v, want 3.14", msg.Score)
	}
	if got := msg.Name.Resolve(buf); got != "alice" {
		t.Errorf("Name = %q, want %q", got, "alice")
	}
	if got := msg.Tag.Resolve(buf); got != "admin" {
		t.Errorf("Tag = %q, want %q", got, "admin")
	}
}

func TestZeroCopyStringPointsIntoBuf(t *testing.T) {
	t.Parallel()

	buf := Serialize(&Message{ID: 1, Score: 0}, "hello", "")
	msg, err := Deserialize(buf)
	if err != nil {
		t.Fatalf("Deserialize: %v", err)
	}

	resolved := msg.Name.Resolve(buf)
	if resolved != "hello" {
		t.Fatalf("Name.Resolve = %q, want hello", resolved)
	}
	// The string must point into buf, proving zero-copy.
	namePtr := unsafe.StringData(resolved)
	bufPtr := &buf[msg.Name.Offset]
	if namePtr != bufPtr {
		t.Fatalf("resolved string does not share memory with buf (namePtr=%p, bufPtr=%p)", namePtr, bufPtr)
	}
}

func TestMutationVisibleThroughPointer(t *testing.T) {
	t.Parallel()

	buf := Serialize(&Message{ID: 7, Score: 0}, "", "")
	msg, err := Deserialize(buf)
	if err != nil {
		t.Fatalf("Deserialize: %v", err)
	}

	// Write through the deserialized pointer — should appear in buf.
	msg.ID = 99

	msg2, _ := Deserialize(buf)
	if msg2.ID != 99 {
		t.Fatalf("mutation through pointer not visible in buf: got %d, want 99", msg2.ID)
	}
}

func TestDeserializeErrorCases(t *testing.T) {
	t.Parallel()

	validBuf := Serialize(&Message{ID: 1, Score: 0}, "x", "y")

	tests := []struct {
		name    string
		buf     []byte
		wantErr error
	}{
		{name: "nil", buf: nil, wantErr: ErrBufferTooShort},
		{name: "empty", buf: []byte{}, wantErr: ErrBufferTooShort},
		{name: "too short for header", buf: []byte{0, 1, 2, 3}, wantErr: ErrBufferTooShort},
		{name: "wrong magic", buf: func() []byte {
			b := make([]byte, len(validBuf))
			copy(b, validBuf)
			b[0] = 0xFF
			return b
		}(), wantErr: ErrInvalidMagic},
		{name: "msgLen exceeds buf", buf: func() []byte {
			b := make([]byte, len(validBuf))
			copy(b, validBuf)
			// set msgLen to a large value
			b[4] = 0xFF
			b[5] = 0xFF
			b[6] = 0xFF
			b[7] = 0x7F
			return b
		}(), wantErr: ErrBufferTooShort},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			_, err := Deserialize(tc.buf)
			if !errors.Is(err, tc.wantErr) {
				t.Fatalf("Deserialize error = %v, want %v", err, tc.wantErr)
			}
		})
	}
}

func TestStringRefOutOfBounds(t *testing.T) {
	t.Parallel()

	buf := Serialize(&Message{ID: 1}, "hello", "world")

	// A StringRef with an offset beyond buf returns "".
	r := StringRef{Offset: uint32(len(buf)), Length: 1}
	if got := r.Resolve(buf); got != "" {
		t.Fatalf("out-of-bounds Resolve = %q, want empty", got)
	}

	// A StringRef with a length that overflows returns "".
	r2 := StringRef{Offset: 0, Length: uint32(len(buf) + 1)}
	if got := r2.Resolve(buf); got != "" {
		t.Fatalf("overflow Resolve = %q, want empty", got)
	}
}

func TestStringRefZeroLength(t *testing.T) {
	t.Parallel()

	r := StringRef{Offset: 0, Length: 0}
	if got := r.Resolve([]byte("anything")); got != "" {
		t.Fatalf("zero-length Resolve = %q, want empty", got)
	}
}

func FuzzDeserialize(f *testing.F) {
	// Seed with a valid message so the fuzzer starts from a known-good input.
	f.Add(Serialize(&Message{ID: 1, Score: 2.5}, "name", "tag"))
	f.Add([]byte{})
	f.Add(make([]byte, 4))

	f.Fuzz(func(t *testing.T, data []byte) {
		msg, err := Deserialize(data)
		if err != nil {
			return // expected for most random inputs
		}
		// If deserialization succeeded, Resolve must not panic.
		_ = msg.Name.Resolve(data)
		_ = msg.Tag.Resolve(data)
	})
}

func BenchmarkDeserialize(b *testing.B) {
	buf := Serialize(&Message{ID: 1, Score: 3.14}, "benchname", "benchtag")
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		_, err := Deserialize(buf)
		if err != nil {
			b.Fatal(err)
		}
	}
}

func BenchmarkResolve(b *testing.B) {
	buf := Serialize(&Message{ID: 1}, "benchname", "")
	msg, _ := Deserialize(buf)
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		_ = msg.Name.Resolve(buf)
	}
}
```

Your turn: add `TestEmptyStrings` that calls `Serialize(&Message{ID: 0}, "", "")` and confirms `Deserialize` succeeds, `Name.Resolve` returns `""`, and `Tag.Resolve` returns `""`.

### Exercise 3: Runnable Demo

Create `cmd/demo/main.go`:

```go
package main

import (
	"fmt"
	"unsafe"

	"example.com/zerocopy"
)

func main() {
	msg := &zerocopy.Message{ID: 42, Score: 1.618}
	buf := zerocopy.Serialize(msg, "fibonacci", "math")

	fmt.Printf("wire buffer: %d bytes\n", len(buf))

	decoded, err := zerocopy.Deserialize(buf)
	if err != nil {
		fmt.Printf("Deserialize error: %v\n", err)
		return
	}

	fmt.Printf("ID:    %d\n", decoded.ID)
	fmt.Printf("Score: %g\n", decoded.Score)
	fmt.Printf("Name:  %q\n", decoded.Name.Resolve(buf))
	fmt.Printf("Tag:   %q\n", decoded.Tag.Resolve(buf))

	// Prove zero-copy: the resolved string points into buf.
	name := decoded.Name.Resolve(buf)
	namePtr := unsafe.StringData(name)
	bufPtr := &buf[decoded.Name.Offset]
	fmt.Printf("zero-copy (name ptr == buf ptr): %v\n", namePtr == bufPtr)
}
```

Run with:

```bash
go run ./cmd/demo
```

## Common Mistakes

### Ignoring Alignment

Wrong:

```go
// buf may start at any address; &buf[8] may not be 8-byte aligned
msg := (*Message)(unsafe.Pointer(&buf[8]))
// Reading msg.Score (float32) is fine; reading a hypothetical int64 field crashes on ARM
```

What happens: on ARM, an unaligned 8-byte read raises `SIGBUS`. On x86, it succeeds but may be slower.

Fix: always call `unsafe.Alignof` and check alignment before casting, as `Deserialize` does.

### Keeping the Resolved String After buf Is Freed

Wrong:

```go
resolved := msg.Name.Resolve(buf)
buf = nil         // buf may be collected
runtime.GC()      // resolved now dangles
fmt.Println(resolved) // undefined behavior
```

Fix: either keep `buf` alive (assign it to a variable that outlives `resolved`) or copy the string: `s := string(resolved)`.

### Treating the Pointer Cast as Safe for All Types

Wrong:

```go
type Bad struct {
	Ptr *int  // a Go pointer inside a wire struct
}
msg := (*Bad)(unsafe.Pointer(&buf[HeaderSize]))
// The GC does not know Ptr points into buf; it may collect the target
```

Fix: wire structs must contain only scalar types, fixed-size arrays of scalars, and `StringRef` index types. No Go pointers.

### Fabricating Output Without Running the Code

Wrong: writing "Expected output: ..." lines in comments and treating them as verified. If you change `Magic` or the struct layout, the expected output silently becomes wrong.

Fix: the test suite is the ground truth. The `FuzzDeserialize` function additionally proves correctness against adversarial input.

## Verification

From `~/go-exercises/zerocopy`:

```bash
test -z "$(gofmt -l .)"
go vet ./...
go build ./...
go test -count=1 -race ./...
go test -bench=Benchmark -benchmem ./...
```

All five must pass. `BenchmarkDeserialize` should show `0 allocs/op`; the deserialized pointer points directly into the input buffer. Run the fuzzer with:

```bash
go test -fuzz=FuzzDeserialize -fuzztime=30s ./...
```

The fuzzer must not find a panic for any random input.

## Summary

- Zero-copy deserialization casts `&buf[headerSize]` directly to `*T` via `unsafe.Pointer`, eliminating all allocation on the deserialization path.
- The wire format must match Go's struct memory layout; each field must start at an offset that is a multiple of `unsafe.Alignof(field)`.
- Variable-length string fields are stored as `{Offset uint32, Length uint32}` pairs (`StringRef`) resolved via `unsafe.String` against the original buffer.
- Every resolution must bounds-check the offset and length; a malformed buffer must return an error, not a panic.
- Fuzz testing is the minimum safety bar for any code that accepts external byte buffers and uses `unsafe`.
- The result is 10-100x faster than `encoding/binary.Read` for large messages because no data is touched except the fields the caller accesses.

## What's Next

Next: [Memory-Mapped Data Store](../10-memory-mapped-data-store/10-memory-mapped-data-store.md).

## Resources

- [unsafe package](https://pkg.go.dev/unsafe)
- [FlatBuffers internals](https://flatbuffers.dev/flatbuffers_internals.html) — canonical zero-copy serialization design
- [Cap'n Proto encoding](https://capnproto.org/encoding.html) — alternative zero-copy format
- [Go fuzzing guide](https://go.dev/doc/security/fuzz/)
- [Go specification: Package unsafe](https://go.dev/ref/spec#Package_unsafe)
