# 4. Setting Values with Reflect

Reading struct fields through reflection is one thing; writing them is another. The wrinkle is addressability: `reflect.ValueOf(x)` produces a copy, and copies are not settable. You must reflect on a pointer to reach settable memory. This lesson builds a `FillStruct` function — the core of any config loader, form decoder, or ORM hydrator — and tests its behavior precisely.

```text
fillstruct/
  go.mod
  fillstruct.go
  fillstruct_test.go
  cmd/demo/main.go
```

## Concepts

### Addressability and CanSet

`reflect.Value.CanSet()` returns `true` only when the value was obtained by dereferencing a pointer. The rule is: a value is settable if and only if it was obtained through at least one level of pointer indirection inside the reflect chain.

```go
x := 42
reflect.ValueOf(x).CanSet()          // false — copy of x
reflect.ValueOf(&x).Elem().CanSet()  // true  — x's own memory
```

If you try to call `Set` on a non-settable value, the runtime panics with "reflect: reflect.Value.SetInt using value obtained using unexported field" or a similar message. Always check `CanSet()` first, or guard entry points so you only ever call `Elem()` after confirming the input is a pointer.

### Set, SetInt, SetString, and Friends

Once you have a settable `reflect.Value`, you can assign a new value with:

- `v.Set(reflect.ValueOf(newVal))` — generic; panics if the type is wrong.
- `v.SetInt(n int64)` — for all integer kinds (`Int`, `Int8`, ..., `Int64`).
- `v.SetString(s string)` — for `String` kind.
- `v.SetBool(b bool)` — for `Bool` kind.
- `v.SetFloat(f float64)` — for `Float32` and `Float64` kinds.

Using the kind-specific setters bypasses the exact-type requirement: `v.SetInt(42)` works whether `v` is `int`, `int32`, or `int64`. The generic `v.Set(reflect.ValueOf(42))` would fail if the field is `int32` but `42` is an untyped integer that becomes `int`.

### AssignableTo vs ConvertibleTo

Before calling `Set`, you must confirm types are compatible:

- `T.AssignableTo(U)` — `T` can be assigned to a variable of type `U` without conversion (same type, or interface satisfaction).
- `T.ConvertibleTo(U)` — a value of type `T` can be converted to `U` with an explicit conversion, such as `float64` to `int`.

JSON unmarshal decodes all numbers as `float64`, so a JSON decoder handing you `map[string]any` will provide `float64(30)` for an `int` field. Your `FillStruct` must use `ConvertibleTo` and `Convert` to handle this cross-type case.

### Unexported Fields

`CanSet()` returns `false` for unexported fields even when you have a pointer. The reflection package enforces the same visibility rules as the compiler. Skip unexported fields silently — do not return an error, because the caller has no way to change the struct definition.

## Exercises

Set up the module:

```bash
mkdir -p ~/go-exercises/fillstruct/cmd/demo
cd ~/go-exercises/fillstruct
go mod init example.com/fillstruct
```

### Exercise 1: Addressability Exploration and setField

Create `fillstruct.go`:

```go
package fillstruct

import (
	"fmt"
	"reflect"
	"strings"
)

// ErrNotPointerToStruct is returned when target is not a pointer to a struct.
var ErrNotPointerToStruct = fmt.Errorf("fillstruct: target must be a non-nil pointer to a struct")

// ErrFieldNotFound is returned when the named field does not exist.
var ErrFieldNotFound = fmt.Errorf("fillstruct: field not found")

// ErrFieldNotSettable is returned when the field exists but cannot be set
// (typically because it is unexported).
var ErrFieldNotSettable = fmt.Errorf("fillstruct: field not settable")

// ErrTypeMismatch is returned when the provided value cannot be assigned or
// converted to the field's type.
var ErrTypeMismatch = fmt.Errorf("fillstruct: type mismatch")

// SetField sets a single exported struct field by name.
// target must be a non-nil pointer to a struct.
func SetField(target any, fieldName string, value any) error {
	v := reflect.ValueOf(target)
	if v.Kind() != reflect.Ptr || v.IsNil() || v.Elem().Kind() != reflect.Struct {
		return ErrNotPointerToStruct
	}

	field := v.Elem().FieldByName(fieldName)
	if !field.IsValid() {
		return fmt.Errorf("%w: %q", ErrFieldNotFound, fieldName)
	}
	if !field.CanSet() {
		return fmt.Errorf("%w: %q (unexported?)", ErrFieldNotSettable, fieldName)
	}

	rv := reflect.ValueOf(value)
	if rv.Type().AssignableTo(field.Type()) {
		field.Set(rv)
		return nil
	}
	if rv.Type().ConvertibleTo(field.Type()) {
		field.Set(rv.Convert(field.Type()))
		return nil
	}
	return fmt.Errorf("%w: cannot assign %v to field %q of type %v",
		ErrTypeMismatch, rv.Type(), fieldName, field.Type())
}

// FillStruct populates exported fields of target from data.
// Keys are matched against json struct tags first, then field names.
// JSON-decoded numeric values (float64) are converted to int when needed.
// target must be a non-nil pointer to a struct.
func FillStruct(target any, data map[string]any) error {
	v := reflect.ValueOf(target)
	if v.Kind() != reflect.Ptr || v.IsNil() || v.Elem().Kind() != reflect.Struct {
		return ErrNotPointerToStruct
	}
	return fillStructValue(v.Elem(), data)
}

func fillStructValue(sv reflect.Value, data map[string]any) error {
	st := sv.Type()
	for i := 0; i < st.NumField(); i++ {
		field := st.Field(i)
		if !field.IsExported() {
			continue
		}
		key := jsonKey(field)
		val, ok := data[key]
		if !ok {
			continue
		}
		fv := sv.Field(i)
		if !fv.CanSet() {
			continue
		}
		if err := setFieldValue(fv, val); err != nil {
			return fmt.Errorf("fillstruct: field %q: %w", field.Name, err)
		}
	}
	return nil
}

// jsonKey returns the effective key for a field: the json tag name if present,
// otherwise the field name.
func jsonKey(f reflect.StructField) string {
	tag := f.Tag.Get("json")
	if tag == "" || tag == "-" {
		return f.Name
	}
	if idx := strings.IndexByte(tag, ','); idx >= 0 {
		tag = tag[:idx]
	}
	if tag == "" {
		return f.Name
	}
	return tag
}

func setFieldValue(fv reflect.Value, val any) error {
	rv := reflect.ValueOf(val)
	if rv.Type().AssignableTo(fv.Type()) {
		fv.Set(rv)
		return nil
	}
	if rv.Type().ConvertibleTo(fv.Type()) {
		fv.Set(rv.Convert(fv.Type()))
		return nil
	}
	return fmt.Errorf("%w: cannot assign %v to %v", ErrTypeMismatch, rv.Type(), fv.Type())
}
```

### Exercise 2: Tests

Create `fillstruct_test.go`:

```go
package fillstruct

import (
	"errors"
	"fmt"
	"testing"
)

type Config struct {
	Host    string  `json:"host"`
	Port    int     `json:"port"`
	Debug   bool    `json:"debug"`
	Timeout float64 `json:"timeout"`
	secret  string  // unexported — must be skipped
}

func TestSetFieldString(t *testing.T) {
	t.Parallel()

	cfg := &Config{}
	if err := SetField(cfg, "Host", "localhost"); err != nil {
		t.Fatal(err)
	}
	if cfg.Host != "localhost" {
		t.Fatalf("Host = %q, want localhost", cfg.Host)
	}
}

func TestSetFieldInt(t *testing.T) {
	t.Parallel()

	cfg := &Config{}
	if err := SetField(cfg, "Port", 8080); err != nil {
		t.Fatal(err)
	}
	if cfg.Port != 8080 {
		t.Fatalf("Port = %d, want 8080", cfg.Port)
	}
}

func TestSetFieldConversionFloat64ToInt(t *testing.T) {
	t.Parallel()

	// JSON decodes numbers as float64; FillStruct must convert.
	cfg := &Config{}
	if err := SetField(cfg, "Port", float64(9090)); err != nil {
		t.Fatal(err)
	}
	if cfg.Port != 9090 {
		t.Fatalf("Port = %d, want 9090", cfg.Port)
	}
}

func TestSetFieldNotFound(t *testing.T) {
	t.Parallel()

	cfg := &Config{}
	err := SetField(cfg, "Missing", "value")
	if !errors.Is(err, ErrFieldNotFound) {
		t.Fatalf("err = %v, want ErrFieldNotFound", err)
	}
}

func TestSetFieldNotSettableUnexported(t *testing.T) {
	t.Parallel()

	cfg := &Config{}
	err := SetField(cfg, "secret", "x")
	if !errors.Is(err, ErrFieldNotSettable) {
		t.Fatalf("err = %v, want ErrFieldNotSettable", err)
	}
}

func TestSetFieldNotPointerToStruct(t *testing.T) {
	t.Parallel()

	err := SetField(Config{}, "Host", "x")
	if !errors.Is(err, ErrNotPointerToStruct) {
		t.Fatalf("err = %v, want ErrNotPointerToStruct", err)
	}
}

func TestFillStructFromMap(t *testing.T) {
	t.Parallel()

	data := map[string]any{
		"host":    "example.com",
		"port":    float64(443), // typical from JSON decode
		"debug":   true,
		"timeout": float64(30),
	}
	cfg := &Config{}
	if err := FillStruct(cfg, data); err != nil {
		t.Fatal(err)
	}
	if cfg.Host != "example.com" {
		t.Errorf("Host = %q, want example.com", cfg.Host)
	}
	if cfg.Port != 443 {
		t.Errorf("Port = %d, want 443", cfg.Port)
	}
	if !cfg.Debug {
		t.Error("Debug should be true")
	}
}

func TestFillStructUnexportedFieldIsSkipped(t *testing.T) {
	t.Parallel()

	// Providing the unexported field via its name must not cause an error.
	data := map[string]any{"secret": "hunter2"}
	cfg := &Config{}
	if err := FillStruct(cfg, data); err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if cfg.secret != "" {
		t.Fatal("unexported field must not be set")
	}
}

func TestFillStructMissingKeysLeaveZeroValues(t *testing.T) {
	t.Parallel()

	cfg := &Config{Port: 8080}
	if err := FillStruct(cfg, map[string]any{}); err != nil {
		t.Fatal(err)
	}
	// Port is not in the map, so it must retain its original value.
	if cfg.Port != 8080 {
		t.Fatalf("Port = %d, want 8080 (should not be overwritten)", cfg.Port)
	}
}

func ExampleFillStruct() {
	type Point struct {
		X int `json:"x"`
		Y int `json:"y"`
	}
	p := &Point{}
	FillStruct(p, map[string]any{"x": float64(3), "y": float64(4)})
	fmt.Printf("X=%d Y=%d\n", p.X, p.Y)
	// Output: X=3 Y=4
}
```

Add `"fmt"` to the import block of `fillstruct_test.go` — the Example uses it.

**Your turn:** add `TestSetFieldTypeMismatch` that calls `SetField(cfg, "Host", 42)` (an `int` into a `string` field) and asserts `errors.Is(err, ErrTypeMismatch)`.

### Exercise 3: Demo Program

Create `cmd/demo/main.go`:

```go
package main

import (
	"fmt"
	"log"

	"example.com/fillstruct"
)

type ServerConfig struct {
	Host    string  `json:"host"`
	Port    int     `json:"port"`
	Debug   bool    `json:"debug"`
	Timeout float64 `json:"timeout"`
}

func main() {
	// Simulate data arriving from JSON decode — all numbers are float64.
	data := map[string]any{
		"host":    "api.example.com",
		"port":    float64(8443),
		"debug":   false,
		"timeout": float64(15),
	}

	cfg := &ServerConfig{}
	if err := fillstruct.FillStruct(cfg, data); err != nil {
		log.Fatal(err)
	}
	fmt.Printf("host=%s port=%d debug=%v timeout=%.0f\n",
		cfg.Host, cfg.Port, cfg.Debug, cfg.Timeout)

	// Direct single-field set.
	if err := fillstruct.SetField(cfg, "Port", 9090); err != nil {
		log.Fatal(err)
	}
	fmt.Println("updated port:", cfg.Port)

	// Error path: missing field.
	err := fillstruct.SetField(cfg, "NoSuchField", "x")
	fmt.Println("missing field error:", err)
}
```

Run with `go run ./cmd/demo`.

## Common Mistakes

### Forgetting Elem() After ValueOf

Wrong: `reflect.ValueOf(cfg).FieldByName("Host").SetString("x")` where `cfg` is already a pointer.

What happens: `ValueOf(cfg)` produces a `reflect.Value` of kind `Ptr`. `FieldByName` panics because `Ptr` values do not have fields.

Fix: call `Elem()` to get the struct value before calling `FieldByName`:
`reflect.ValueOf(cfg).Elem().FieldByName("Host")`.

### Calling Set Without Checking CanSet

Wrong: calling `field.SetString("x")` directly on a value obtained from `reflect.ValueOf(s)` where `s` is not a pointer.

What happens: panic — "reflect: reflect.Value.SetString using value obtained using unexported field" or "using non-addressable value".

Fix: always reflect on a pointer and check `CanSet()`, or only call `Set*` on fields reached through `Elem()` of a pointer.

### Using AssignableTo When ConvertibleTo Is Needed

Wrong: checking only `AssignableTo` before `Set`, then failing when a `float64` JSON value meets an `int` field.

What happens: the error `cannot assign float64 to field of type int` surfaces even though the conversion is numerically correct.

Fix: try `AssignableTo` first, then fall through to `ConvertibleTo` + `Convert`.

## Verification

```bash
test -z "$(gofmt -l .)"
go vet ./...
go build ./...
go test -count=1 -race ./...
```

## Summary

- A `reflect.Value` is settable only when it was obtained through a pointer; reach it with `reflect.ValueOf(&x).Elem()`.
- Check `CanSet()` before any `Set*` call; it returns `false` for copies and for unexported fields.
- Use kind-specific setters (`SetInt`, `SetString`, `SetBool`, `SetFloat`) to avoid exact-type requirements.
- Prefer `AssignableTo` for same-type assignment; use `ConvertibleTo` + `Convert` for numeric coercion (e.g., `float64` from JSON into `int`).
- Skip unexported fields silently — the caller cannot change the struct definition.

## What's Next

Next: [Building a Struct Validator](../05-building-a-struct-validator/05-building-a-struct-validator.md).

## Resources

- [reflect.Value.Set](https://pkg.go.dev/reflect#Value.Set)
- [reflect.Value.CanSet](https://pkg.go.dev/reflect#Value.CanSet)
- [The Laws of Reflection — Settability](https://go.dev/blog/laws-of-reflection)
- [reflect.Type.AssignableTo and ConvertibleTo](https://pkg.go.dev/reflect#Type)
