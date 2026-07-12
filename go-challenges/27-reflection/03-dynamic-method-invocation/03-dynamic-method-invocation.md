# 3. Dynamic Method Invocation

Dynamic method invocation lets a caller look up and call a method by name at runtime, without a compile-time reference. This is the mechanism behind RPC dispatchers, command routers, and plugin systems. The tricky parts are the pointer-receiver distinction, argument validation before `Call`, and recovering cleanly from missing or mismatched methods.

```text
dispatch/
  go.mod
  dispatch.go
  dispatch_test.go
  cmd/demo/main.go
```

## Concepts

### How MethodByName Works

`reflect.Value.MethodByName(name string)` searches the method set of the concrete type (or pointer type) held in the `reflect.Value`. It returns an invalid `reflect.Value` when the method does not exist — always guard with `IsValid()`.

The method set depends on what you pass to `reflect.ValueOf`:

- For a value `T`, the method set is the value-receiver methods of `T`.
- For a pointer `*T`, the method set includes both pointer-receiver and value-receiver methods.

This is the most common footgun: calling `reflect.ValueOf(v)` where `v` is not a pointer will silently drop all pointer-receiver methods from the lookup.

### Argument Passing and Call

`method.Call(args []reflect.Value)` panics if argument count or types are wrong. The right approach is to inspect `method.Type()` before calling:

```go
t := method.Type()
t.NumIn()     // number of input parameters
t.In(i)       // type of parameter i
t.NumOut()    // number of return values
t.Out(i)      // type of return value i
```

Use `argVal.Type().AssignableTo(expectedType)` to confirm each argument is compatible before calling. This converts a potential panic into a handled error.

### Value Receivers vs Pointer Receivers

A pointer-receiver method is only in the method set of `*T`, not `T`. Passing `reflect.ValueOf(myStruct)` to a dispatcher hides all pointer-receiver methods. Passing `reflect.ValueOf(&myStruct)` exposes both sets. Production dispatchers that need to mutate state must always receive a pointer.

### Variadic Methods

A variadic method `func (T) Foo(args ...int)` has `t.IsVariadic() == true`. Call it with `method.Call(in)` when you supply all arguments explicitly, or `method.CallSlice(in)` when the last argument is already a slice that should be spread.

## Exercises

Set up the module:

```bash
mkdir -p go-solutions/27-reflection/03-dynamic-method-invocation/03-dynamic-method-invocation/cmd/demo
cd go-solutions/27-reflection/03-dynamic-method-invocation/03-dynamic-method-invocation
```

This is a library, not a program: verification is done with `go test`.

### Exercise 1: The Dispatcher Type

Create `dispatch.go`:

```go
package dispatch

import (
	"fmt"
	"reflect"
)

// Dispatcher routes string command names to methods on a target value.
type Dispatcher struct {
	target reflect.Value
}

// New creates a Dispatcher for target. Pass a pointer if the target has
// pointer-receiver methods that must be reachable.
func New(target any) *Dispatcher {
	return &Dispatcher{target: reflect.ValueOf(target)}
}

// Call invokes the named method with the given arguments.
// It returns an error if the method does not exist, if the argument count
// is wrong, or if any argument type is not assignable to the expected parameter type.
func (d *Dispatcher) Call(name string, args ...any) ([]any, error) {
	method := d.target.MethodByName(name)
	if !method.IsValid() {
		return nil, fmt.Errorf("dispatch: method %q not found", name)
	}

	mt := method.Type()
	if !mt.IsVariadic() && len(args) != mt.NumIn() {
		return nil, fmt.Errorf("dispatch: method %q expects %d args, got %d",
			name, mt.NumIn(), len(args))
	}

	in := make([]reflect.Value, len(args))
	for i, arg := range args {
		rv := reflect.ValueOf(arg)
		var want reflect.Type
		if mt.IsVariadic() && i >= mt.NumIn()-1 {
			want = mt.In(mt.NumIn() - 1).Elem()
		} else {
			want = mt.In(i)
		}
		if !rv.Type().AssignableTo(want) {
			return nil, fmt.Errorf("dispatch: method %q arg %d: cannot assign %v to %v",
				name, i, rv.Type(), want)
		}
		in[i] = rv
	}

	results := method.Call(in)
	out := make([]any, len(results))
	for i, r := range results {
		out[i] = r.Interface()
	}
	return out, nil
}

// Methods returns the names of all exported methods on the target.
func (d *Dispatcher) Methods() []string {
	t := d.target.Type()
	names := make([]string, t.NumMethod())
	for i := range names {
		names[i] = t.Method(i).Name
	}
	return names
}
```

### Exercise 2: A Target Service

Append to `dispatch.go`:

```go
// MathService demonstrates value-receiver methods suitable for dispatching.
type MathService struct{}

// Add returns the sum of a and b.
func (MathService) Add(a, b int) int { return a + b }

// Greet returns a greeting string.
func (MathService) Greet(name string) string { return fmt.Sprintf("Hello, %s!", name) }

// Status returns a fixed status string.
func (MathService) Status() string { return "ok" }

// Counter demonstrates pointer-receiver methods; pass &Counter{} to the Dispatcher.
type Counter struct {
	n int
}

// Inc increments the counter.
func (c *Counter) Inc() { c.n++ }

// Value returns the current count.
func (c *Counter) Value() int { return c.n }
```

### Exercise 3: Tests

Create `dispatch_test.go`:

```go
package dispatch

import (
	"fmt"
	"testing"
)

func TestCallAdd(t *testing.T) {
	t.Parallel()

	d := New(MathService{})
	out, err := d.Call("Add", 3, 4)
	if err != nil {
		t.Fatal(err)
	}
	if len(out) != 1 || out[0].(int) != 7 {
		t.Fatalf("Add(3,4) = %v, want [7]", out)
	}
}

func TestCallGreet(t *testing.T) {
	t.Parallel()

	d := New(MathService{})
	out, err := d.Call("Greet", "World")
	if err != nil {
		t.Fatal(err)
	}
	if got := out[0].(string); got != "Hello, World!" {
		t.Fatalf("Greet = %q, want %q", got, "Hello, World!")
	}
}

func TestCallStatus(t *testing.T) {
	t.Parallel()

	d := New(MathService{})
	out, err := d.Call("Status")
	if err != nil {
		t.Fatal(err)
	}
	if got := out[0].(string); got != "ok" {
		t.Fatalf("Status = %q, want ok", got)
	}
}

func TestCallMissingMethod(t *testing.T) {
	t.Parallel()

	d := New(MathService{})
	_, err := d.Call("NoSuchMethod")
	if err == nil {
		t.Fatal("expected error for missing method")
	}
}

func TestCallArgCountMismatch(t *testing.T) {
	t.Parallel()

	d := New(MathService{})
	_, err := d.Call("Add", 1) // needs 2 args
	if err == nil {
		t.Fatal("expected error for wrong arg count")
	}
}

func TestCallTypeMismatch(t *testing.T) {
	t.Parallel()

	d := New(MathService{})
	_, err := d.Call("Add", "a", "b") // expects int, int
	if err == nil {
		t.Fatal("expected error for wrong arg types")
	}
}

func TestPointerReceiverMethods(t *testing.T) {
	t.Parallel()

	c := &Counter{}
	d := New(c)

	for i := 0; i < 3; i++ {
		if _, err := d.Call("Inc"); err != nil {
			t.Fatalf("Inc: %v", err)
		}
	}
	out, err := d.Call("Value")
	if err != nil {
		t.Fatal(err)
	}
	if got := out[0].(int); got != 3 {
		t.Fatalf("Value = %d, want 3", got)
	}
}

func TestValueReceiverMissingPointerMethods(t *testing.T) {
	t.Parallel()

	// Passing Counter (value) hides pointer-receiver methods.
	d := New(Counter{})
	_, err := d.Call("Inc")
	if err == nil {
		t.Fatal("expected error: pointer-receiver method not found on value")
	}
}

func TestMethodsList(t *testing.T) {
	t.Parallel()

	d := New(MathService{})
	methods := d.Methods()
	if len(methods) == 0 {
		t.Fatal("expected at least one method")
	}
	seen := make(map[string]bool)
	for _, m := range methods {
		seen[m] = true
	}
	for _, want := range []string{"Add", "Greet", "Status"} {
		if !seen[want] {
			t.Errorf("method %q missing from Methods()", want)
		}
	}
}

func ExampleDispatcher_Call() {
	d := New(MathService{})
	out, _ := d.Call("Add", 3, 4)
	fmt.Println(out[0])
	// Output: 7
}
```

Add the missing import to the test file by updating the import block — the example function uses `fmt`.

**Your turn:** add `TestCallAddNegative` that calls `d.Call("Add", -10, 5)` and asserts the result is `-5`.

### Exercise 4: Demo Program

Create `cmd/demo/main.go`:

```go
package main

import (
	"fmt"
	"log"

	"example.com/dispatch"
)

func main() {
	d := dispatch.New(dispatch.MathService{})

	fmt.Println("Methods:", d.Methods())

	out, err := d.Call("Add", 10, 32)
	if err != nil {
		log.Fatal(err)
	}
	fmt.Println("Add(10, 32) =", out[0])

	out, err = d.Call("Greet", "reflection")
	if err != nil {
		log.Fatal(err)
	}
	fmt.Println(out[0])

	// Missing method — handled gracefully.
	_, err = d.Call("Nonexistent")
	fmt.Println("expected error:", err)

	// Pointer-receiver methods require a pointer target.
	c := &dispatch.Counter{}
	dc := dispatch.New(c)
	dc.Call("Inc")
	dc.Call("Inc")
	out, _ = dc.Call("Value")
	fmt.Println("Counter value:", out[0])
}
```

Run with `go run ./cmd/demo`.

## Common Mistakes

### Passing a Value When a Pointer Is Required

Wrong: `d := New(myStruct)` when `myStruct` has pointer-receiver methods. `MethodByName` returns an invalid value and `Call` returns an error.

What happens: the pointer-receiver methods are not in the method set of the value type, so `MethodByName` silently returns an invalid `reflect.Value`.

Fix: always pass `&myStruct` when the target has pointer receivers or needs to mutate state.

### Calling Without Checking IsValid

Wrong: calling `method.Call(in)` without first checking `method.IsValid()`.

What happens: `Call` on an invalid `reflect.Value` panics with "reflect: call of reflect.Value.Call on zero Value".

Fix: always check `method.IsValid()` before calling.

### Ignoring Type Mismatches Before Call

Wrong: passing `[]reflect.Value{reflect.ValueOf("not-an-int")}` to a method expecting `int`.

What happens: `Call` panics with a type-mismatch message that is hard to attribute to a specific call site.

Fix: validate each argument with `argType.AssignableTo(expectedType)` and return a descriptive error before calling.

## Verification

```bash
test -z "$(gofmt -l .)"
go vet ./...
go build ./...
go test -count=1 -race ./...
```

All four must pass. `go test` is the verification — `ExampleDispatcher_Call` is auto-checked.

## Summary

- `MethodByName` looks up exported methods by name; always call `IsValid()` before `Call`.
- The method set of a value `T` excludes pointer-receiver methods; pass `*T` to include them.
- Validate argument count (`NumIn`) and types (`In(i)`, `AssignableTo`) before calling to avoid panics.
- `Call` returns `[]reflect.Value`; convert back to concrete types with `.Interface()`.
- Variadic methods distinguish `Call` (explicit args) from `CallSlice` (spread last arg as slice).

## What's Next

Next: [Setting Values with Reflect](../04-setting-values-with-reflect/04-setting-values-with-reflect.md).

## Resources

- [reflect.Value.MethodByName](https://pkg.go.dev/reflect#Value.MethodByName)
- [reflect.Value.Call](https://pkg.go.dev/reflect#Value.Call)
- [The Laws of Reflection](https://go.dev/blog/laws-of-reflection)
- [Go Specification: Method sets](https://go.dev/ref/spec#Method_sets)
