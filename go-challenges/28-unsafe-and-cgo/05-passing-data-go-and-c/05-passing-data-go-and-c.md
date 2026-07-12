# 5. Passing Data Between Go and C

Passing data through cgo is harder than calling a C function. The wrapper must respect the pointer-passing rules, keep Go pointers out of C-owned memory, and make ownership obvious. This lesson wraps C functions that read and write integer buffers.

## Concepts

### C May Borrow Some Go Memory Briefly

Go can pass a pointer to C for the duration of a call when the pointed-to memory contains no Go pointers. A `[]int32` backing array qualifies; a `[]string` does not.

### C-Owned Memory Must Be Released

When C allocates memory, Go must call the matching C release function. Hide that behind a function that copies the result into a Go slice before freeing the C buffer.

### Empty Slices Need Special Handling

Taking `&values[0]` on an empty slice panics. Validate length before deriving a pointer.

## Exercises

### Exercise 1: Wrap Buffer Functions

Create `buffer.go`:

```go
package cgodata

/*
#include <stdint.h>
#include <stdlib.h>

int32_t sum_ints(const int32_t* values, int n) {
	int32_t total = 0;
	for (int i = 0; i < n; i++) {
		total += values[i];
	}
	return total;
}

void fill_squares(int32_t* values, int n) {
	for (int i = 0; i < n; i++) {
		values[i] = (int32_t)((i + 1) * (i + 1));
	}
}

int32_t* make_range(int n) {
	if (n <= 0) {
		return NULL;
	}
	int32_t* values = (int32_t*)malloc(sizeof(int32_t) * (size_t)n);
	if (values == NULL) {
		return NULL;
	}
	for (int i = 0; i < n; i++) {
		values[i] = (int32_t)i;
	}
	return values;
}
*/
import "C"

import (
	"errors"
	"fmt"
	"unsafe"
)

var (
	ErrEmptyInput = errors.New("input must not be empty")
	ErrBadLength  = errors.New("length must be positive")
	ErrCAlloc     = errors.New("c allocation failed")
)

func Sum(values []int32) (int32, error) {
	if len(values) == 0 {
		return 0, ErrEmptyInput
	}
	return int32(C.sum_ints((*C.int32_t)(unsafe.Pointer(&values[0])), C.int(len(values)))), nil
}

func Squares(n int) ([]int32, error) {
	if n <= 0 {
		return nil, fmt.Errorf("%w: %d", ErrBadLength, n)
	}
	out := make([]int32, n)
	C.fill_squares((*C.int32_t)(unsafe.Pointer(&out[0])), C.int(n))
	return out, nil
}

func Range(n int) ([]int32, error) {
	if n <= 0 {
		return nil, fmt.Errorf("%w: %d", ErrBadLength, n)
	}
	ptr := C.make_range(C.int(n))
	if ptr == nil {
		return nil, ErrCAlloc
	}
	defer C.free(unsafe.Pointer(ptr))
	view := unsafe.Slice((*int32)(unsafe.Pointer(ptr)), n)
	out := append([]int32(nil), view...)
	return out, nil
}
```

### Exercise 2: Add Example And Demo

Create `example_test.go`:

```go
package cgodata

import "fmt"

func ExampleSquares() {
	values, _ := Squares(4)
	fmt.Println(values)
	// Output: [1 4 9 16]
}
```

Create `cmd/demo/main.go`:

```go
package main

import (
	"fmt"
	"log"

	"example.com/cgodata"
)

func main() {
	values, err := cgodata.Range(5)
	if err != nil {
		log.Fatal(err)
	}
	fmt.Println(values)
}
```

### Exercise 3: Test Ownership And Validation

Create `buffer_test.go`:

```go
package cgodata

import (
	"errors"
	"reflect"
	"testing"
)

func TestBufferFunctions(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name string
		run  func() ([]int32, error)
		want []int32
		err  error
	}{
		{name: "squares", run: func() ([]int32, error) { return Squares(3) }, want: []int32{1, 4, 9}},
		{name: "range", run: func() ([]int32, error) { return Range(4) }, want: []int32{0, 1, 2, 3}},
		{name: "bad length", run: func() ([]int32, error) { return Squares(0) }, err: ErrBadLength},
	}

	for _, tt := range tests {
		got, err := tt.run()
		if !errors.Is(err, tt.err) {
			t.Fatalf("%s: err = %v, want %v", tt.name, err, tt.err)
		}
		if !reflect.DeepEqual(got, tt.want) {
			t.Fatalf("%s: got %v, want %v", tt.name, got, tt.want)
		}
	}
}

func TestSum(t *testing.T) {
	t.Parallel()

	got, err := Sum([]int32{2, 3, 5})
	if err != nil {
		t.Fatal(err)
	}
	if got != 10 {
		t.Fatalf("sum = %d, want 10", got)
	}
	if _, err := Sum(nil); !errors.Is(err, ErrEmptyInput) {
		t.Fatalf("err = %v, want ErrEmptyInput", err)
	}
}
```

## Common Mistakes

### Passing Go Memory That Contains Go Pointers

Wrong: pass a pointer to a `[]string` backing array to C.

Fix: pass only pointer-free memory such as numeric arrays, or copy strings into C memory.

### Forgetting Empty Slice Checks

Wrong: pass `&values[0]` without checking `len(values)`.

Fix: return a validation error before taking the address.

### Returning A View Over Freed C Memory

Wrong: build a Go slice over C memory, call `C.free`, and return the slice.

Fix: copy into a Go-owned slice before freeing, as `Range` does.

## Verification

Run this from `~/go-exercises/cgodata`:

```bash
test -z "$(gofmt -l .)"
go vet ./...
go build ./...
go test -count=1 -race ./...
```

Add a test that calls `Range(-1)` and checks `errors.Is(err, ErrBadLength)`.

## Summary

- C can borrow pointer-free Go memory only for the duration of a call.
- C-allocated memory needs explicit release.
- Copy data into Go memory before returning it from a wrapper.
- Validate lengths before deriving pointers from slices.

## What's Next

Next: [cgo Performance Overhead](../06-cgo-performance-overhead/06-cgo-performance-overhead.md).

## Resources

- [Command cgo: Passing pointers](https://pkg.go.dev/cmd/cgo#hdr-Passing_pointers)
- [Cgo pointer passing proposal](https://go.googlesource.com/proposal/+/master/design/12416-cgo-pointers.md)
- [unsafe.Slice](https://pkg.go.dev/unsafe#Slice)
