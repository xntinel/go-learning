# 4. cgo Basics

cgo lets Go call C, but it also changes the build: a C compiler is involved, C memory must be freed manually, and the public Go API should hide the pseudo-package `C`. This lesson builds a small arithmetic wrapper with real error handling.

## Concepts

### `import "C"` Belongs In The Implementation

The comment immediately above `import "C"` is the C preamble. It can include headers and helper functions. Keep that detail inside one package file so users import a normal Go package.

### C Types Need Explicit Conversion

Go `int` and C `int` are different types. Convert at the boundary, then return Go values. For strings, allocate with `C.CString` and release with `C.free`.

### C Error Conventions Need Go Errors

C APIs often use return codes or sentinel values. A wrapper should translate those into sentinel Go errors that callers can inspect with `errors.Is`.

## Exercises

Set up the module:

```bash
mkdir -p go-solutions/28-unsafe-and-cgo/04-cgo-basics/04-cgo-basics
cd go-solutions/28-unsafe-and-cgo/04-cgo-basics/04-cgo-basics
```

### Exercise 1: Wrap C Functions

Create `calc.go`:

```go
package cgobasics

/*
#include <stdlib.h>

int add_ints(int a, int b) {
	return a + b;
}

int checked_divide(int a, int b, int* out) {
	if (b == 0) {
		return -1;
	}
	*out = a / b;
	return 0;
}

int c_strlen(const char* s) {
	int n = 0;
	while (s[n] != 0) {
		n++;
	}
	return n;
}
*/
import "C"

import (
	"errors"
	"fmt"
	"unsafe"
)

var ErrDivideByZero = errors.New("divide by zero")

func Add(a, b int) int {
	return int(C.add_ints(C.int(a), C.int(b)))
}

func Divide(a, b int) (int, error) {
	var out C.int
	if code := C.checked_divide(C.int(a), C.int(b), &out); code != 0 {
		return 0, fmt.Errorf("c divide: %w", ErrDivideByZero)
	}
	return int(out), nil
}

func CStringLength(s string) int {
	cs := C.CString(s)
	defer C.free(unsafe.Pointer(cs))
	return int(C.c_strlen(cs))
}
```

### Exercise 2: Add Example And Demo

Create `example_test.go`:

```go
package cgobasics

import "fmt"

func ExampleDivide() {
	q, _ := Divide(20, 5)
	fmt.Println(q)
	// Output: 4
}
```

Create `cmd/demo/main.go`:

```go
package main

import (
	"fmt"
	"log"

	"example.com/cgobasics"
)

func main() {
	q, err := cgobasics.Divide(12, 3)
	if err != nil {
		log.Fatal(err)
	}
	fmt.Println(cgobasics.Add(q, cgobasics.CStringLength("go")))
}
```

### Exercise 3: Test The Wrapper

Create `calc_test.go`:

```go
package cgobasics

import (
	"errors"
	"testing"
)

func TestAddAndDivide(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name string
		a    int
		b    int
		want int
		err  error
	}{
		{name: "divide", a: 21, b: 7, want: 3},
		{name: "divide by zero", a: 1, b: 0, err: ErrDivideByZero},
	}

	for _, tt := range tests {
		got, err := Divide(tt.a, tt.b)
		if !errors.Is(err, tt.err) {
			t.Fatalf("%s: err = %v, want %v", tt.name, err, tt.err)
		}
		if got != tt.want {
			t.Fatalf("%s: got %d, want %d", tt.name, got, tt.want)
		}
	}

	if Add(2, 3) != 5 {
		t.Fatal("Add(2, 3) should be 5")
	}
}

func TestCStringLength(t *testing.T) {
	t.Parallel()

	if got := CStringLength("gopher"); got != 6 {
		t.Fatalf("length = %d, want 6", got)
	}
}
```

## Common Mistakes

### Forgetting To Free `C.CString`

Wrong: call `C.CString` and return without `C.free`.

Fix: `defer C.free(unsafe.Pointer(cs))` immediately after allocation.

### Exposing C Types In The Go API

Wrong: return `C.int` from an exported function.

Fix: convert to ordinary Go types at the boundary.

### Testing Only With A Demo Program

Wrong: run `go run` and compare printed output manually.

Fix: put the contract in `*_test.go` and let `go test` fail automatically.

## Verification

Run this from `~/go-exercises/cgobasics`:

```bash
test -z "$(gofmt -l .)"
go vet ./...
go build ./...
go test -count=1 -race ./...
```

Add a test that asserts `errors.Is(err, ErrDivideByZero)` for `Divide(10, 0)`.

## Summary

- cgo code is compiled by both Go and a C compiler.
- The public API should use Go types, not C types.
- C allocations must be released explicitly.
- C error codes should become Go errors.

## What's Next

Next: [Passing Data Between Go and C](../05-passing-data-go-and-c/05-passing-data-go-and-c.md).

## Resources

- [Command cgo](https://pkg.go.dev/cmd/cgo)
- [Cgo wiki](https://go.dev/wiki/cgo)
- [unsafe package](https://pkg.go.dev/unsafe)
