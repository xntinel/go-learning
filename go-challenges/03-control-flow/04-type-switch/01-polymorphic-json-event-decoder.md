# Exercise 1: Route Decoded JSON to Typed Handlers with a Type Switch

An ingestion endpoint receives arbitrary JSON events and must route each one to a
handler chosen by its decoded shape. This is the canonical place a type switch
earns its keep: JSON has just six value kinds, they arrive as `any`, and the
switch dispatches each to a typed handler.

This module is fully self-contained. It has its own `go mod init`, defines every
type it needs, and ships its own demo and tests. Nothing here imports another
exercise.

## What you'll build

```text
eventdecode/                 independent module: example.com/eventdecode
  go.mod                     go 1.26
  decode.go                  Decode([]byte) (any, error) with UseNumber; Decoder.Handle type-switches
  cmd/
    demo/
      main.go                decodes six JSON kinds, routes each to a handler
  decode_test.go             table-driven routing, big-int precision, nil/unsupported, nested-unsupported
```

- Files: `decode.go`, `cmd/demo/main.go`, `decode_test.go`.
- Implement: `Decode([]byte) (any, error)` decoding with `UseNumber`, and
  `(*Decoder).Handle(source string, value any) error` dispatching by concrete
  type to per-kind handlers plus `nil` and `default` cases.
- Test: table-driven routing that counts which handler fired; a max-`int64`
  integer survives as `json.Number`; the `nil` case returns `ErrNilValue`; an
  unsupported struct returns `ErrUnsupported`; a nested unsupported value does
  not panic.
- Verify: `go test -count=1 -race ./...`

Set up the module:

```bash
mkdir -p ~/go-exercises/eventdecode/cmd/demo
cd ~/go-exercises/eventdecode
go mod init example.com/eventdecode
```

## Why UseNumber, and why json.Number is its own case

`encoding/json` decodes an unknown JSON value into one of six Go types: `string`,
`float64` (for every number, by default), `bool`, `map[string]any` (object),
`[]any` (array), and `nil` (JSON null). The default number type is the problem.
A JSON integer like `9223372036854775807` (`math.MaxInt64`) cannot be
represented exactly as a `float64` — floats have 53 bits of mantissa — so it is
silently rounded before your code ever sees it. For an event carrying an account
id or a ledger amount, that is data corruption at the boundary.

`Decoder.UseNumber` fixes this: with it set, every JSON number is decoded into a
`json.Number`, which is a `string` under the hood, preserving the exact digits.
You then convert deliberately, choosing `Int64()` or `Float64()` and checking the
error, at the point where you know what the number means. That is why the type
switch has a `case json.Number` and not a `case float64` — with `UseNumber` on,
`float64` never arrives; `json.Number` does.

`Handle` checks `value == nil` first, because `case nil` in the switch would also
catch it, but returning a named `ErrNilValue` up front makes the "null payload"
contract explicit and testable. Every other kind flows through the type switch;
the `default` branch returns `ErrUnsupported` wrapped with `%T` so an unexpected
Go type (a value that did not come from the JSON decoder) fails loudly instead of
being swallowed.

Create `decode.go`:

```go
package eventdecode

import (
	"encoding/json"
	"errors"
	"fmt"
	"strings"
)

// Sentinel errors, wrapped with %w so callers can match them via errors.Is.
var (
	ErrNilValue    = errors.New("nil value")
	ErrUnsupported = errors.New("unsupported value type")
)

// Event is a decoded payload tagged with its source.
type Event struct {
	Source  string
	Payload any
}

// Handler processes one routed event kind.
type Handler func(Event) error

// Decoder routes decoded JSON values to per-kind handlers.
type Decoder struct {
	onString Handler
	onNumber Handler
	onBool   Handler
	onObject Handler
	onArray  Handler
}

// New builds a Decoder from one handler per JSON value kind.
func New(onString, onNumber, onBool, onObject, onArray Handler) *Decoder {
	return &Decoder{
		onString: onString,
		onNumber: onNumber,
		onBool:   onBool,
		onObject: onObject,
		onArray:  onArray,
	}
}

// Decode parses raw JSON into an any tree, preserving integer precision by
// decoding numbers as json.Number instead of float64.
func Decode(raw []byte) (any, error) {
	dec := json.NewDecoder(strings.NewReader(string(raw)))
	dec.UseNumber()
	var value any
	if err := dec.Decode(&value); err != nil {
		return nil, fmt.Errorf("decode json: %w", err)
	}
	return value, nil
}

// Handle dispatches value to the handler for its concrete type. A nil payload
// returns ErrNilValue; an unexpected Go type returns ErrUnsupported.
func (d *Decoder) Handle(source string, value any) error {
	if value == nil {
		return ErrNilValue
	}
	switch v := value.(type) {
	case string:
		return d.onString(Event{Source: source, Payload: v})
	case json.Number:
		return d.onNumber(Event{Source: source, Payload: v})
	case bool:
		return d.onBool(Event{Source: source, Payload: v})
	case map[string]any:
		return d.onObject(Event{Source: source, Payload: v})
	case []any:
		return d.onArray(Event{Source: source, Payload: v})
	default:
		return fmt.Errorf("%w: %T", ErrUnsupported, value)
	}
}
```

## The runnable demo

The demo decodes one of each JSON kind and routes it through handlers that print
the kind and payload. The number handler pulls an exact `Int64` out of the
`json.Number` to show precision survived.

Create `cmd/demo/main.go`:

```go
package main

import (
	"encoding/json"
	"fmt"
	"log"

	"example.com/eventdecode"
)

func main() {
	d := eventdecode.New(
		func(e eventdecode.Event) error { fmt.Printf("string: %s\n", e.Payload); return nil },
		func(e eventdecode.Event) error {
			n := e.Payload.(json.Number)
			fmt.Printf("number: %s\n", n.String())
			return nil
		},
		func(e eventdecode.Event) error { fmt.Printf("bool: %v\n", e.Payload); return nil },
		func(e eventdecode.Event) error { fmt.Printf("object: %v\n", e.Payload); return nil },
		func(e eventdecode.Event) error { fmt.Printf("array: %v\n", e.Payload); return nil },
	)

	inputs := []string{
		`"hello"`,
		`9223372036854775807`,
		`3.14`,
		`true`,
		`{"a":1}`,
		`[1,2,3]`,
	}
	for _, raw := range inputs {
		value, err := eventdecode.Decode([]byte(raw))
		if err != nil {
			log.Printf("decode %s: %v", raw, err)
			continue
		}
		if err := d.Handle("cli", value); err != nil {
			log.Printf("handle %s: %v", raw, err)
		}
	}
}
```

Run it:

```bash
go run ./cmd/demo
```

Expected output:

```text
string: hello
number: 9223372036854775807
number: 3.14
bool: true
object: map[a:1]
array: [1 2 3]
```

## Tests

The routing test counts which handler fired for each input kind. The precision
test proves `math.MaxInt64` survives as a `json.Number` whose `Int64()` returns
the exact value. The rejection tests drive the `nil` case (`ErrNilValue`) and the
`default` case with a Go struct the decoder never produces (`ErrUnsupported`),
both asserted via `errors.Is`. The nested test proves that an object whose value
is an unsupported Go type does not panic — `Handle` treats the top-level
`map[string]any` as an object and never inspects the odd value, so the walk is
safe.

Create `decode_test.go`:

```go
package eventdecode

import (
	"encoding/json"
	"errors"
	"fmt"
	"testing"
)

func newCounting(seen map[string]int) *Decoder {
	return New(
		func(Event) error { seen["string"]++; return nil },
		func(Event) error { seen["number"]++; return nil },
		func(Event) error { seen["bool"]++; return nil },
		func(Event) error { seen["object"]++; return nil },
		func(Event) error { seen["array"]++; return nil },
	)
}

func TestDecoderRoutesByType(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name     string
		raw      string
		wantKind string
	}{
		{"string", `"hello"`, "string"},
		{"integer", `42`, "number"},
		{"float", `3.14`, "number"},
		{"bool", `true`, "bool"},
		{"object", `{"a":1}`, "object"},
		{"array", `[1,2,3]`, "array"},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			seen := map[string]int{}
			d := newCounting(seen)
			value, err := Decode([]byte(tc.raw))
			if err != nil {
				t.Fatal(err)
			}
			if err := d.Handle("test", value); err != nil {
				t.Fatal(err)
			}
			if seen[tc.wantKind] != 1 {
				t.Fatalf("routed to %v, want %s once", seen, tc.wantKind)
			}
		})
	}
}

func TestBigIntegerSurvivesAsNumber(t *testing.T) {
	t.Parallel()
	value, err := Decode([]byte(`{"id":9223372036854775807}`))
	if err != nil {
		t.Fatal(err)
	}
	obj, ok := value.(map[string]any)
	if !ok {
		t.Fatalf("value is %T, want map[string]any", value)
	}
	num, ok := obj["id"].(json.Number)
	if !ok {
		t.Fatalf("id is %T, want json.Number", obj["id"])
	}
	got, err := num.Int64()
	if err != nil {
		t.Fatalf("Int64: %v", err)
	}
	if got != 9223372036854775807 {
		t.Fatalf("id = %d, want 9223372036854775807 (float64 would truncate)", got)
	}
}

func TestNilAndUnsupported(t *testing.T) {
	t.Parallel()
	seen := map[string]int{}
	d := newCounting(seen)

	value, err := Decode([]byte(`null`))
	if err != nil {
		t.Fatal(err)
	}
	if err := d.Handle("test", value); !errors.Is(err, ErrNilValue) {
		t.Fatalf("null payload: err = %v, want ErrNilValue", err)
	}

	unsupported := struct{ x int }{x: 1}
	if err := d.Handle("test", unsupported); !errors.Is(err, ErrUnsupported) {
		t.Fatalf("struct payload: err = %v, want ErrUnsupported", err)
	}
}

func TestNestedUnsupportedDoesNotPanic(t *testing.T) {
	t.Parallel()
	seen := map[string]int{}
	d := newCounting(seen)
	// The object handler here does not descend, so an odd nested value is inert.
	payload := map[string]any{"weird": struct{ x int }{x: 1}}
	if err := d.Handle("test", payload); err != nil {
		t.Fatalf("object with nested struct: unexpected err %v", err)
	}
	if seen["object"] != 1 {
		t.Fatalf("routed to %v, want object once", seen)
	}
}

func ExampleDecode() {
	value, _ := Decode([]byte(`{"amount":100}`))
	obj := value.(map[string]any)
	n := obj["amount"].(json.Number)
	amount, _ := n.Int64()
	fmt.Println(amount)
	// Output: 100
}
```

## Review

The decoder is correct when every JSON kind routes to exactly one handler, when a
`math.MaxInt64` integer round-trips through `json.Number.Int64()` with no loss,
and when the two error paths (`ErrNilValue`, `ErrUnsupported`) fire for the null
payload and for a Go type the decoder cannot produce. The most common way to
break it is to drop `UseNumber` and add a `case float64` — that compiles, passes
a superficial test, and silently truncates large integers in production. The
second is to put a broad case (an interface everything satisfies) before the
concrete kinds, collapsing the dispatch. Keep `default` returning a typed error
so a future unexpected type fails loudly rather than vanishing.

## Resources

- [Go Specification: Type switches](https://go.dev/ref/spec#Type_switches)
- [encoding/json Decoder.UseNumber](https://pkg.go.dev/encoding/json#Decoder.UseNumber)
- [json.Number](https://pkg.go.dev/encoding/json#Number)
- [errors.Is and errors.As](https://pkg.go.dev/errors)

---

Up: [00-concepts.md](00-concepts.md) | Next: [02-json-tree-redactor.md](02-json-tree-redactor.md)
