# 1. unsafe.Pointer and uintptr

`unsafe.Pointer` is the narrow escape hatch from Go's type system. `uintptr` is not a pointer at all; it is an integer large enough to hold an address, and the garbage collector does not treat it as a live reference. This lesson builds a tiny inspector package that exposes those rules through tested operations.

## Concepts

### `unsafe.Pointer` Is Still A Pointer

An `unsafe.Pointer` can be converted to and from any typed pointer. While it remains a pointer value, the garbage collector can see that the object is referenced. The type checker stops protecting you, so the package boundary should keep unsafe code small and tested.

### `uintptr` Is Only For Immediate Arithmetic

Converting a pointer to `uintptr` produces an integer address. Do not store that integer and rebuild a pointer later. The safe pattern is one expression: convert the base pointer to `uintptr`, add an offset, convert back to `unsafe.Pointer`, and immediately to the typed pointer.

### Keep Unsafe Behind Exported Safe Functions

The public API should describe domain behavior, not memory tricks. Callers of the package below ask for a word at an offset; they do not manipulate raw addresses themselves.

## Exercises

### Exercise 1: Build A Word Inspector

Create `inspector.go`:

```go
package unsafeptr

import (
	"errors"
	"fmt"
	"unsafe"
)

var (
	ErrNilBuffer = errors.New("buffer must not be nil")
	ErrBadIndex  = errors.New("word index out of range")
)

const WordSize = int(unsafe.Sizeof(uint32(0)))

func WordAt(buf []byte, index int) (uint32, error) {
	if len(buf) == 0 {
		return 0, ErrNilBuffer
	}
	if index < 0 || index*WordSize+WordSize > len(buf) {
		return 0, fmt.Errorf("%w: index %d len %d", ErrBadIndex, index, len(buf))
	}
	base := unsafe.Pointer(unsafe.SliceData(buf))
	ptr := (*uint32)(unsafe.Pointer(uintptr(base) + uintptr(index*WordSize)))
	return *ptr, nil
}

func ByteAddressDelta(buf []byte, a, b int) (uintptr, error) {
	if len(buf) == 0 {
		return 0, ErrNilBuffer
	}
	if a < 0 || b < 0 || a >= len(buf) || b >= len(buf) {
		return 0, fmt.Errorf("%w: indexes %d %d len %d", ErrBadIndex, a, b, len(buf))
	}
	pa := unsafe.Pointer(&buf[a])
	pb := unsafe.Pointer(&buf[b])
	return uintptr(pb) - uintptr(pa), nil
}
```

### Exercise 2: Add A Verified Example And Demo

Create `example_test.go`:

```go
package unsafeptr

import "fmt"

func ExampleWordAt() {
	buf := []byte{1, 0, 0, 0, 2, 0, 0, 0}
	word, _ := WordAt(buf, 1)
	fmt.Println(word)
	// Output: 2
}
```

Create `cmd/demo/main.go`:

```go
package main

import (
	"fmt"
	"log"

	"example.com/unsafeptr"
)

func main() {
	word, err := unsafeptr.WordAt([]byte{9, 0, 0, 0}, 0)
	if err != nil {
		log.Fatal(err)
	}
	fmt.Println(word)
}
```

### Exercise 3: Test The Boundary Rules

Create `inspector_test.go`:

```go
package unsafeptr

import (
	"errors"
	"testing"
)

func TestWordAt(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name string
		buf  []byte
		idx  int
		want uint32
		err  error
	}{
		{name: "first", buf: []byte{1, 0, 0, 0}, want: 1},
		{name: "second", buf: []byte{1, 0, 0, 0, 7, 0, 0, 0}, idx: 1, want: 7},
		{name: "empty", err: ErrNilBuffer},
		{name: "past end", buf: []byte{1, 2, 3}, err: ErrBadIndex},
	}

	for _, tt := range tests {
		got, err := WordAt(tt.buf, tt.idx)
		if !errors.Is(err, tt.err) {
			t.Fatalf("%s: err = %v, want %v", tt.name, err, tt.err)
		}
		if got != tt.want {
			t.Fatalf("%s: got %d, want %d", tt.name, got, tt.want)
		}
	}
}

func TestByteAddressDelta(t *testing.T) {
	t.Parallel()

	got, err := ByteAddressDelta([]byte{1, 2, 3, 4}, 0, 3)
	if err != nil {
		t.Fatal(err)
	}
	if got != 3 {
		t.Fatalf("delta = %d, want 3", got)
	}
}
```

## Common Mistakes

### Saving A `uintptr` For Later

Wrong: store `addr := uintptr(unsafe.Pointer(&buf[0]))` in a struct and use it later.

Fix: keep the original object alive and do pointer arithmetic only inside the expression that immediately dereferences the pointer.

### Letting Callers Pass Raw Addresses

Wrong: expose `func Read(addr uintptr) uint32`.

Fix: accept a real Go object such as `[]byte`; the package owns the unsafe conversion and can validate bounds.

### Ignoring Bounds Before Pointer Arithmetic

Wrong: compute a pointer first and check the index later.

Fix: check the slice length before converting the base pointer.

## Verification

Run this from `~/go-exercises/unsafeptr`:

```bash
test -z "$(gofmt -l .)"
go vet ./...
go build ./...
go test -count=1 -race ./...
```

Add one more test for `ByteAddressDelta` that passes an out-of-range index and asserts `errors.Is(err, ErrBadIndex)`.

## Summary

- `unsafe.Pointer` is a pointer value; `uintptr` is an integer.
- Pointer arithmetic belongs in one tightly bounded expression.
- Unsafe code should sit behind a small exported API with ordinary Go errors.
- Tests should assert sentinel errors with `errors.Is`.

## What's Next

Next: [unsafe.Sizeof, Alignof, and Offsetof](../02-unsafe-sizeof-alignof-offsetof/02-unsafe-sizeof-alignof-offsetof.md).

## Resources

- [unsafe package](https://pkg.go.dev/unsafe)
- [Go Specification: Package unsafe](https://go.dev/ref/spec#Package_unsafe)
- [Go pointer passing rules proposal](https://go.googlesource.com/proposal/+/master/design/12416-cgo-pointers.md)
