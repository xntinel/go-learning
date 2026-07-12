# Exercise 8: Log Formatting Gotcha: Pointer-Receiver Stringer Not in a Value's Method Set

A `String()` method defined on a pointer receiver quietly fails to format values:
print a `Status` value, or range over a `[]Status` or `map[string]Status`, and you
get raw integers in your logs instead of names. This module reproduces that gotcha
and fixes it by moving `String` to a value receiver, with tests pinning both
behaviors.

This module is fully self-contained. Nothing here imports another exercise.

## What you'll build

```text
statusfmt/                  independent module: example.com/statusfmt
  go.mod                    go 1.25
  status.go                 badStatus (ptr String); Status (value String); names
  cmd/
    demo/
      main.go               print value, slice, map with both types
  status_test.go            ptr-recv prints raw int; value-recv prints name; table
```

- Files: `status.go`, `cmd/demo/main.go`, `status_test.go`.
- Implement: a `badStatus` int with a pointer-receiver `String`, and a `Status` int with a value-receiver `String`, over the same set of names.
- Test: `fmt.Sprintf("%v", badStatus(x))` prints the raw integer (String not called); `fmt.Sprintf("%v", Status(x))` prints the name; a map value of `badStatus` prints raw, of `Status` prints the name.
- Verify: `go test -count=1 -race ./...`

Set up the module:

```bash
go mod edit -go=1.25
```

### Why fmt does not call a pointer-receiver String on a value

`fmt` decides whether to call `String()` by testing whether the value it was
handed implements `fmt.Stringer` — a check against that value's method set. When
`String` has a pointer receiver, `badStatus`'s (value) method set does **not**
include it; only `*badStatus` does. So `fmt.Sprintf("%v", badStatus(2))` sees a
value that is not a `Stringer` and falls back to the default formatting of an
`int`, printing `2`. The method you wrote is simply never reached.

It gets worse in collections. Ranging over a `[]badStatus` or `map[string]badStatus`
and printing each element passes fmt a **copy** of the element (the conversion to
`any` copies the value), and that copy is a non-addressable value whose method set
excludes the pointer method — so none of them format. Map elements are doubly
cursed: they are not addressable at all, so you cannot even write `&m[k]` to reach
the pointer method. The result is a log line full of `0 1 2` where you expected
`Pending Active Failed`.

The fix is one character of receiver: define `String` on a **value** receiver.
Then `Status`'s method set includes it, `*Status`'s does too, and every value — a
lone `Status`, a slice element, a map value, an interface-boxed copy — formats
through it. This is why `fmt.Stringer` implementations are conventionally
value-receiver methods.

Create `status.go`:

```go
// status.go
package statusfmt

// names maps a status code to its display name, shared by both types.
var names = map[int]string{0: "Pending", 1: "Active", 2: "Failed"}

func name(code int) string {
	if n, ok := names[code]; ok {
		return n
	}
	return "Unknown"
}

// badStatus defines String on a POINTER receiver. This is the gotcha: a
// badStatus value does not implement fmt.Stringer, so fmt prints the raw int.
type badStatus int

func (s *badStatus) String() string { return name(int(*s)) }

// Status defines String on a VALUE receiver. Every Status value implements
// fmt.Stringer, so fmt formats it — in a slice, a map, or alone.
type Status int

const (
	Pending Status = iota
	Active
	Failed
)

func (s Status) String() string { return name(int(s)) }
```

### The runnable demo

The demo prints both types alone, in a slice, and in a map, so you can see the
pointer-receiver version leak raw integers while the value-receiver version formats
names everywhere.

Create `cmd/demo/main.go`:

```go
// cmd/demo/main.go
package main

import (
	"fmt"

	"example.com/statusfmt"
)

func main() {
	// Pointer-receiver String: fmt prints the raw int for a value.
	fmt.Printf("badStatus value:  %v\n", statusfmt.BadStatus(2))

	// Value-receiver String: fmt prints the name.
	fmt.Printf("Status value:     %v\n", statusfmt.Failed)

	// In a slice: value receiver formats every element.
	fmt.Printf("Status slice:     %v\n", []statusfmt.Status{statusfmt.Pending, statusfmt.Active, statusfmt.Failed})

	// In a map value: index a real map so the label matches what prints.
	byName := map[string]statusfmt.Status{"a": statusfmt.Active}
	fmt.Printf("Status map value: %v\n", byName["a"])
}
```

Because `cmd/demo` is a separate package that can only touch exported names, add a
small exported constructor for the buggy type. Append to `status.go`:

```go
// Append to status.go

// BadStatus exposes the pointer-receiver type to the demo package so it can show
// the raw-int formatting. Exported only for the demonstration.
func BadStatus(code int) badStatus { return badStatus(code) }
```

Run it:

```bash
go run ./cmd/demo
```

Expected output:

```
badStatus value:  2
Status value:     Failed
Status slice:     [Pending Active Failed]
Status map value: Active
```

### Tests

`TestPointerReceiverPrintsRawInt` asserts the gotcha directly: formatting a
`badStatus` value yields the integer string. `TestValueReceiverPrintsName` asserts
the fix. `TestMapValues` shows the collection case: a `map[string]badStatus` value
prints raw, a `map[string]Status` value prints the name. An addressability aside
confirms that taking the address of an addressable `badStatus` variable *does*
reach the pointer method.

Create `status_test.go`:

```go
// status_test.go
package statusfmt

import (
	"fmt"
	"testing"
)

func TestPointerReceiverPrintsRawInt(t *testing.T) {
	t.Parallel()

	// A badStatus value is not a Stringer (String has a pointer receiver), so
	// fmt prints the underlying int.
	got := fmt.Sprintf("%v", badStatus(2))
	if got != "2" {
		t.Fatalf("badStatus value %%v = %q, want %q (raw int, String not called)", got, "2")
	}
}

func TestPointerAddressReachesString(t *testing.T) {
	t.Parallel()

	// An addressable variable's address DOES implement Stringer.
	s := badStatus(2)
	got := fmt.Sprintf("%v", &s)
	if got != "Failed" {
		t.Fatalf("&badStatus %%v = %q, want %q", got, "Failed")
	}
}

func TestValueReceiverPrintsName(t *testing.T) {
	t.Parallel()

	got := fmt.Sprintf("%v", Failed)
	if got != "Failed" {
		t.Fatalf("Status value %%v = %q, want %q", got, "Failed")
	}
}

func TestMapValues(t *testing.T) {
	t.Parallel()

	bad := map[string]badStatus{"a": badStatus(1)}
	if got := fmt.Sprintf("%v", bad["a"]); got != "1" {
		t.Fatalf("badStatus map value = %q, want raw int %q", got, "1")
	}

	good := map[string]Status{"a": Active}
	if got := fmt.Sprintf("%v", good["a"]); got != "Active" {
		t.Fatalf("Status map value = %q, want %q", got, "Active")
	}
}

func TestStatusTable(t *testing.T) {
	t.Parallel()

	cases := []struct {
		s    Status
		want string
	}{
		{Pending, "Pending"},
		{Active, "Active"},
		{Failed, "Failed"},
		{Status(9), "Unknown"},
	}
	for _, tc := range cases {
		t.Run(tc.want, func(t *testing.T) {
			t.Parallel()
			if got := tc.s.String(); got != tc.want {
				t.Fatalf("String() = %q, want %q", got, tc.want)
			}
		})
	}
}

func ExampleStatus() {
	fmt.Println([]Status{Pending, Active, Failed})
	// Output: [Pending Active Failed]
}
```

## Review

The gotcha and its fix sit side by side: `badStatus` and `Status` differ only in
the receiver of `String`, and the tests show that difference deciding whether a
value formats as `2` or `Failed`. The rule is not about the method existing — it is
about the method set of the exact value fmt receives, and a plain value's method
set excludes pointer methods. For any type you print (a status, a level, an ID
wrapper), define `String` on a value receiver so slices, map values, and lone
values all format uniformly. The `TestPointerAddressReachesString` case is the
precise boundary: the pointer method is reachable only when you hand fmt an
addressable value's address — which you cannot do for map elements at all.

## Resources

- [fmt.Stringer](https://pkg.go.dev/fmt#Stringer) — the interface fmt consults, checked against the value's method set.
- [Go Specification: Method sets](https://go.dev/ref/spec#Method_sets) — why a value's method set excludes pointer methods.
- [Go Specification: Address operators](https://go.dev/ref/spec#Address_operators) — which values are addressable (and map elements are not).

---

Back to [00-concepts.md](00-concepts.md) | Prev: [07-method-value-callback-worker.md](07-method-value-callback-worker.md) | Next: [09-sort-interface-pointer-receiver.md](09-sort-interface-pointer-receiver.md)
