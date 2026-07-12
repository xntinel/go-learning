# 7. unsafe.Slice and unsafe.String

Go 1.17 added `unsafe.Slice` and Go 1.20 added `unsafe.String`, `unsafe.SliceData`, and `unsafe.StringData`. These four functions are the official, supported replacements for the fragile `reflect.SliceHeader` / `reflect.StringHeader` patterns that appear in older cgo wrappers and zero-copy serializers. Understanding them precisely — what they guarantee, what they do not guarantee, and how they compose — is the prerequisite for every lesson that follows in this chapter.

```text
slicestring/
  go.mod
  slicestring.go
  slicestring_test.go
  cmd/demo/main.go
```

## Concepts

### Memory Layouts: Slice and String Headers

A Go slice is three words: a data pointer, a length, and a capacity. A Go string is two words: a data pointer and a length. These are the `reflect.SliceHeader` and `reflect.StringHeader` shapes (now deprecated). The important property for this lesson is that both contain a raw pointer to a backing array.

Before Go 1.17, constructing a slice from a raw pointer meant writing:

```go
// WRONG — pre-1.17 pattern, do not use
var s []byte
hdr := (*reflect.SliceHeader)(unsafe.Pointer(&s))
hdr.Data = uintptr(ptr)   // BUG: GC may move ptr between this line ...
hdr.Len = n               // ... and this one
hdr.Cap = n
```

The `uintptr` assignment is the trap: a `uintptr` is just an integer. The garbage collector does not treat it as a live reference, so the GC can move or collect the pointed-to allocation between the two lines. The safe versions fix this by accepting the pointer and length as a single atomic operation.

### unsafe.Slice and unsafe.SliceData (Go 1.17 / 1.20)

```go
func Slice[E any](ptr *E, len IntegerType) []E
func SliceData[E any](slice []E) *E
```

`unsafe.Slice(ptr, n)` creates a slice whose backing array is exactly the memory at `ptr`, with length and capacity `n`. The returned slice does not own the memory; it is valid only as long as the memory `ptr` points to is alive. `unsafe.SliceData` is the inverse: given a slice, it returns the underlying data pointer.

Rules enforced at runtime:
- If `ptr` is nil and `n == 0`, the result is a nil slice.
- If `ptr` is nil and `n > 0`, the call panics.
- If `n` is negative or overflows `int`, the call panics.

### unsafe.String and unsafe.StringData (Go 1.20)

```go
func String(ptr *byte, len IntegerType) string
func StringData(str string) *byte
```

`unsafe.String(ptr, n)` creates a Go string backed by the `n` bytes starting at `ptr`. Because Go strings are immutable by contract, the caller must ensure the backing memory is never modified while the string is alive. Violating this rule is undefined behavior that the race detector does not catch.

`unsafe.StringData` returns the pointer to the first byte of the string's backing array. For the empty string, the return value is unspecified (may be nil or a valid pointer); do not dereference it.

### Zero-Copy Conversions

The two most common uses are:

1. `[]byte` to `string` without allocation: valid only if you will not modify the slice again while the string is alive.
2. `string` to `[]byte` without allocation: the resulting slice must be treated as read-only; writing through it is undefined behavior.

The standard conversion (`string(b)` and `[]byte(s)`) always allocates and copies. The unsafe versions eliminate the allocation at the cost of discipline.

### Lifetime and Safety

The GC tracks pointers only when they are stored in pointer-typed variables. The returned `string` or `[]byte` holds the pointer, so it keeps the backing allocation alive. The danger arises when the backing memory is C memory (freed by `C.free`), a memory-mapped region (unmapped), or a stack allocation that the compiler decides to move. In all those cases, the string or slice becomes a dangling reference the GC cannot detect.

## Exercises

Set up the module:

```bash
mkdir -p go-solutions/28-unsafe-and-cgo/07-unsafe-slice-and-string/07-unsafe-slice-and-string/cmd/demo
cd go-solutions/28-unsafe-and-cgo/07-unsafe-slice-and-string/07-unsafe-slice-and-string
```

This is a library package, not a program. You verify it with `go test`.

### Exercise 1: The Conversion Primitives

Create `slicestring.go`:

```go
package slicestring

import (
	"unsafe"
)

// BytesToString returns a string backed by the bytes of b.
// The caller must not modify b while the returned string is in use.
// Returns "" for nil or empty b.
func BytesToString(b []byte) string {
	if len(b) == 0 {
		return ""
	}
	return unsafe.String(unsafe.SliceData(b), len(b))
}

// StringToBytes returns a []byte backed by the bytes of s.
// The caller must not write through the returned slice.
// Returns nil for the empty string.
func StringToBytes(s string) []byte {
	if len(s) == 0 {
		return nil
	}
	return unsafe.Slice(unsafe.StringData(s), len(s))
}

// SliceFromPointer creates a []int32 of length n starting at ptr.
// ptr must not be nil when n > 0.
// The caller retains ownership of the memory; the slice is valid only while
// the backing allocation is alive.
func SliceFromPointer(ptr *int32, n int) []int32 {
	if n == 0 {
		return nil
	}
	return unsafe.Slice(ptr, n)
}

// SharesMemory reports whether the first byte of s and b[0] are the same
// address. Used in tests to confirm zero-copy conversions.
func SharesMemory(s string, b []byte) bool {
	if len(s) == 0 || len(b) == 0 {
		return false
	}
	return unsafe.StringData(s) == unsafe.SliceData(b)
}
```

### Exercise 2: Tests That Pin the Contract

Create `slicestring_test.go`:

```go
package slicestring

import (
	"testing"
	"unsafe"
)

func TestBytesToStringSharesMemory(t *testing.T) {
	t.Parallel()

	b := []byte("hello, world")
	s := BytesToString(b)

	if s != "hello, world" {
		t.Fatalf("BytesToString = %q, want %q", s, "hello, world")
	}
	// The string must point into b, not a fresh allocation.
	if !SharesMemory(s, b) {
		t.Fatal("BytesToString made a copy; expected zero-copy")
	}
}

func TestBytesToStringEmpty(t *testing.T) {
	t.Parallel()

	if got := BytesToString(nil); got != "" {
		t.Fatalf("BytesToString(nil) = %q, want empty", got)
	}
	if got := BytesToString([]byte{}); got != "" {
		t.Fatalf("BytesToString([]byte{}) = %q, want empty", got)
	}
}

func TestStringToBytesSharesMemory(t *testing.T) {
	t.Parallel()

	// Use a heap-allocated string so the pointer is stable.
	s := string([]byte("zero copy test"))
	b := StringToBytes(s)

	if string(b) != s {
		t.Fatalf("StringToBytes = %q, want %q", b, s)
	}
	if !SharesMemory(s, b) {
		t.Fatal("StringToBytes made a copy; expected zero-copy")
	}
}

func TestStringToBytesEmpty(t *testing.T) {
	t.Parallel()

	if got := StringToBytes(""); got != nil {
		t.Fatalf("StringToBytes(\"\") = %v, want nil", got)
	}
}

func TestSliceFromPointer(t *testing.T) {
	t.Parallel()

	arr := [4]int32{10, 20, 30, 40}
	s := SliceFromPointer(&arr[0], 4)

	if len(s) != 4 || cap(s) != 4 {
		t.Fatalf("len=%d cap=%d, want 4 and 4", len(s), cap(s))
	}
	for i, want := range []int32{10, 20, 30, 40} {
		if s[i] != want {
			t.Errorf("s[%d] = %d, want %d", i, s[i], want)
		}
	}

	// Mutation through the slice is visible in the original array.
	s[2] = 999
	if arr[2] != 999 {
		t.Fatalf("arr[2] = %d, want 999 after mutation through slice", arr[2])
	}
}

func TestSliceFromPointerZeroLength(t *testing.T) {
	t.Parallel()

	arr := [1]int32{1}
	s := SliceFromPointer(&arr[0], 0)
	if s != nil {
		t.Fatalf("SliceFromPointer(p, 0) = %v, want nil", s)
	}
}

func TestSharesMemoryDetectsAlias(t *testing.T) {
	t.Parallel()

	b := []byte("aliased")
	s := BytesToString(b)
	if !SharesMemory(s, b) {
		t.Fatal("expected alias")
	}

	// A regular string conversion must NOT share memory.
	sCopy := string(b)
	if SharesMemory(sCopy, b) {
		t.Fatal("string(b) unexpectedly shares memory with b")
	}
}

func ExampleBytesToString() {
	b := []byte("example")
	s := BytesToString(b)
	// s points into b with no allocation.
	_ = unsafe.StringData(s) // non-nil for non-empty string
	// Output:
}

func BenchmarkBytesToStringCopy(b *testing.B) {
	data := make([]byte, 1024)
	var s string
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		s = string(data) // allocates and copies
	}
	_ = s
}

func BenchmarkBytesToStringZeroCopy(b *testing.B) {
	data := make([]byte, 1024)
	var s string
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		s = BytesToString(data) // no allocation
	}
	_ = s
}

func BenchmarkStringToBytesCopy(b *testing.B) {
	s := string(make([]byte, 1024))
	var data []byte
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		data = []byte(s) // allocates and copies
	}
	_ = data
}

func BenchmarkStringToBytesZeroCopy(b *testing.B) {
	s := string(make([]byte, 1024))
	var data []byte
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		data = StringToBytes(s) // no allocation
	}
	_ = data
}
```

Your turn: add `TestSliceFromPointerSubRange` that calls `SliceFromPointer(&arr[2], 2)` on a `[4]int32` and confirms the resulting slice contains only elements `[2]` and `[3]` of the original array.

### Exercise 3: Runnable Demo

Create `cmd/demo/main.go`:

```go
package main

import (
	"fmt"

	"example.com/slicestring"
)

func main() {
	// Zero-copy []byte -> string
	b := []byte("zero-copy conversion")
	s := slicestring.BytesToString(b)
	fmt.Printf("string: %q\n", s)
	fmt.Printf("shares memory: %v\n", slicestring.SharesMemory(s, b))

	// Zero-copy string -> []byte (read-only)
	src := "read me without copying"
	ro := slicestring.StringToBytes(src)
	fmt.Printf("bytes: %q\n", ro)
	fmt.Printf("shares memory: %v\n", slicestring.SharesMemory(src, ro))

	// SliceFromPointer: view an array as a slice
	arr := [5]int32{1, 2, 3, 4, 5}
	view := slicestring.SliceFromPointer(&arr[0], 5)
	fmt.Printf("array view: %v\n", view)
	view[0] = 99
	fmt.Printf("arr[0] after mutation through slice: %d\n", arr[0])
}
```

Run with:

```bash
go run ./cmd/demo
```

## Common Mistakes

### Modifying the Backing Slice After BytesToString

Wrong:

```go
b := []byte("hello")
s := BytesToString(b)
b[0] = 'H' // undefined behavior: mutates the string's backing memory
fmt.Println(s)
```

What happens: the mutation may or may not be visible in `s`. Go strings are immutable; the compiler and runtime may cache the string value. This is a data race if `b` and `s` are used from different goroutines.

Fix: copy the string if you need to modify `b` afterward, or do not use the zero-copy form.

### Writing Through the StringToBytes Slice

Wrong:

```go
s := "immutable"
b := StringToBytes(s)
b[0] = 'I' // undefined behavior: writes to a string literal's backing store
```

What happens: string literals may reside in read-only memory segments. On some platforms this triggers a segmentation fault. On others it silently corrupts memory.

Fix: convert with `[]byte(s)` if you need a mutable copy. Use `StringToBytes` only for read operations (search, comparison, hashing).

### Using uintptr Instead of unsafe.Pointer

Wrong:

```go
// Pre-1.17 fragile pattern
addr := uintptr(unsafe.Pointer(&arr[0]))
s := *(*[]int32)(unsafe.Pointer(addr)) // addr may be stale: GC ran between the two lines
```

Fix: use `unsafe.Slice(&arr[0], n)` directly. Never store a pointer as `uintptr` with the intent to dereference it later.

### Passing a Pointer to a Go Slice's Internal Header to C

Wrong:

```go
cgo // inside a cgo call
C.process(unsafe.Pointer(&mySlice)) // passes the slice header, not the data
```

Fix: pass `unsafe.Pointer(unsafe.SliceData(mySlice))` to give C a pointer to the actual element data.

## Verification

From `~/go-exercises/slicestring`:

```bash
test -z "$(gofmt -l .)"
go vet ./...
go build ./...
go test -count=1 -race ./...
go test -bench=Benchmark -benchmem ./...
```

All five must pass. The benchmarks should show `0 allocs/op` for the zero-copy variants and `1 allocs/op` for the copy variants on 1 KB buffers.

## Summary

- `unsafe.Slice(ptr, n)` creates a `[]E` of length and capacity `n` backed by the memory at `ptr`. It replaces the deprecated `reflect.SliceHeader` pattern.
- `unsafe.String(ptr, n)` creates a string backed by `n` bytes at `ptr`. It replaces the `*(*string)(unsafe.Pointer(&hdr))` pattern.
- `unsafe.SliceData(s)` and `unsafe.StringData(s)` extract the underlying data pointer from a slice or string.
- Zero-copy `[]byte`-to-`string` (via `BytesToString`) is safe only if the slice is not modified while the string is alive.
- Zero-copy `string`-to-`[]byte` (via `StringToBytes`) is safe only if the slice is used read-only.
- Store the result in a pointer-typed variable, not a `uintptr`, to keep the backing allocation alive across GC pauses.

## What's Next

Next: [Wrapping a C Library](../08-wrapping-a-c-library/08-wrapping-a-c-library.md).

## Resources

- [unsafe package — Slice, SliceData, String, StringData](https://pkg.go.dev/unsafe)
- [Go 1.17 release notes: unsafe.Slice](https://go.dev/doc/go1.17#unsafe)
- [Go 1.20 release notes: unsafe additions](https://go.dev/doc/go1.20#unsafe)
- [Go specification: Package unsafe](https://go.dev/ref/spec#Package_unsafe)
- [Go wiki: cgo — converting C arrays to Go slices](https://go.dev/wiki/cgo#turning-c-arrays-into-go-slices)
