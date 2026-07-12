# 1. reflect.TypeOf and reflect.ValueOf

Build a small inspection package that reports the concrete type, kind, and formatted value of any input. Reflection starts from an interface value, so the lesson focuses on the difference between the dynamic type returned by `reflect.TypeOf`, the wrapped runtime value returned by `reflect.ValueOf`, and the broader category returned by `Kind`.

## Concepts

### Type, Value, and Kind Are Different Questions

`reflect.TypeOf(x)` answers "what concrete type is stored in this interface value?" `reflect.ValueOf(x)` gives a handle that can inspect the runtime value. `Kind` is coarser than type: a named type such as `UserID` can have type `reflectinfo.UserID` while its kind is `int64`.

### Nil Needs Separate Handling

`reflect.TypeOf(nil)` returns `nil`, not a usable `reflect.Type`. Code that calls `Kind` before checking for nil panics. A small reflection utility should make nil behavior part of its contract instead of letting callers discover it through a panic.

### Reflection Is For Boundaries

Reflection is useful at package boundaries where the static type is intentionally unknown: logging, validation, serialization, configuration loading, and diagnostic tooling. Inside ordinary application logic, direct typed code is clearer and safer.

## Exercises

### Exercise 1: Implement the Inspector

Create `inspect.go`:

```go
package reflectinfo

import (
	"errors"
	"fmt"
	"reflect"
)

var ErrNilValue = errors.New("value is nil")

type UserID int64

type Report struct {
	Type  string
	Kind  string
	Value string
}

func Inspect(v any) (Report, error) {
	t := reflect.TypeOf(v)
	if t == nil {
		return Report{}, fmt.Errorf("inspect: %w", ErrNilValue)
	}
	rv := reflect.ValueOf(v)
	return Report{Type: t.String(), Kind: t.Kind().String(), Value: fmt.Sprint(rv.Interface())}, nil
}
```

### Exercise 2: Add an Example

Create `inspect_example_test.go`:

```go
package reflectinfo

import "fmt"

func ExampleInspect() {
	report, _ := Inspect(UserID(42))
	fmt.Println(report.Type)
	fmt.Println(report.Kind)
	fmt.Println(report.Value)
	// Output:
	// reflectinfo.UserID
	// int64
	// 42
}
```

### Exercise 3: Test the Contract

Create `inspect_test.go`:

```go
package reflectinfo

import (
	"errors"
	"testing"
)

func TestInspectReportsTypeKindAndValue(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name string
		in   any
		want Report
	}{
		{name: "int", in: 42, want: Report{Type: "int", Kind: "int", Value: "42"}},
		{name: "string", in: "hello", want: Report{Type: "string", Kind: "string", Value: "hello"}},
		{name: "named", in: UserID(7), want: Report{Type: "reflectinfo.UserID", Kind: "int64", Value: "7"}},
	}

	for _, tt := range tests {
		got, err := Inspect(tt.in)
		if err != nil {
			t.Fatalf("Inspect(%s) error = %v", tt.name, err)
		}
		if got != tt.want {
			t.Fatalf("Inspect(%s) = %+v, want %+v", tt.name, got, tt.want)
		}
	}
}

func TestInspectRejectsNil(t *testing.T) {
	t.Parallel()

	_, err := Inspect(nil)
	if !errors.Is(err, ErrNilValue) {
		t.Fatalf("err = %v, want ErrNilValue", err)
	}
}
```

### Exercise 4: Run a Demo

Create `cmd/demo/main.go`:

```go
package main

import (
	"fmt"
	"log"

	"example.com/reflectinfo"
)

func main() {
	report, err := reflectinfo.Inspect(reflectinfo.UserID(1001))
	if err != nil {
		log.Fatal(err)
	}
	fmt.Printf("%s has kind %s and value %s\n", report.Type, report.Kind, report.Value)
}
```

## Common Mistakes

### Treating Kind as the Full Type

Wrong: using only `Kind` to identify a named type. `UserID` and `int64` both have kind `int64`.

Fix: use `Type` when identity matters and `Kind` only when writing broad logic for categories such as all strings or all structs.

### Calling Methods on a Nil Type

Wrong: `reflect.TypeOf(v).Kind()` when `v` might be nil. This panics because `TypeOf(nil)` returns nil.

Fix: check `t == nil` before calling methods. `Inspect` wraps `ErrNilValue` so callers can use `errors.Is`.

### Using Reflection Where a Type Parameter or Interface Is Clearer

Wrong: reflecting over values when the accepted behavior can be expressed with an interface.

Fix: reserve reflection for unknown runtime shapes. Prefer interfaces or generics when the required operations are known at compile time.

## Verification

Run this from `~/go-exercises/reflectinfo`:

```bash
test -z "$(gofmt -l .)"
go vet ./...
go build ./...
go test -count=1 -race ./...
```

Add one more table case for a `[]int{1, 2}` input and verify that its kind is `slice`.

## Summary

- `TypeOf` returns the concrete dynamic type stored in an interface value.
- `ValueOf` returns a reflective handle around the runtime value.
- `Kind` groups many concrete types into broad runtime categories.
- Nil reflection must be handled before calling methods on `reflect.Type` or `reflect.Value`.

## What's Next

Next: [Inspecting Struct Fields and Tags](../02-inspecting-struct-fields-tags/02-inspecting-struct-fields-tags.md).

## Resources

- [reflect package](https://pkg.go.dev/reflect)
- [Go Blog: The Laws of Reflection](https://go.dev/blog/laws-of-reflection)
- [Go Specification: Interface types](https://go.dev/ref/spec#Interface_types)
