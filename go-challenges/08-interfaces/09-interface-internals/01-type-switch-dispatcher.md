# Exercise 1: Classify Decoded JSON Values With A Type-Switch Dispatcher

Config and event ingestion pipelines routinely decode a document into
`map[string]any` and then have to label each leaf by its dynamic type before
routing or validating it. This module builds that value-typing stage as a
type-switch dispatcher, and uses it to see how the runtime reads an interface's
type word.

This module is fully self-contained: its own module, code, demo, and tests.

## What you'll build

```text
dispatcher/                 independent module: example.com/dispatcher
  go.mod                    go 1.26
  dispatch.go               type Kind; func Classify(any) Kind (type switch)
  cmd/
    demo/
      main.go               decode JSON into map[string]any, classify each leaf
  dispatch_test.go          per-family table tests, typed-nil, consistency property
```

- Files: `dispatch.go`, `cmd/demo/main.go`, `dispatch_test.go`.
- Implement: `Classify(v any) Kind` covering nil, bool, the int/uint/float families, string, `[]byte`, and a default that names the concrete type.
- Test: one label per type family (every int/uint/float width reaches the same `Kind`), an unknown struct reaches default, a typed nil wrapped in `any` reaches default (not the nil case), and a consistency property.
- Verify: `go test -count=1 -race ./...`

Set up the module:

```bash
go mod edit -go=1.26
```

### Why a type switch, and what it actually does

After `json.Unmarshal([]byte(doc), &m)` with `m` of type `map[string]any`, every
value in the map is an `any` header whose type word records the concrete type the
decoder chose: JSON objects become `map[string]any`, arrays `[]any`, strings
`string`, booleans `bool`, and — this trips everyone once — *all* JSON numbers
become `float64`, never `int`. The ingestion stage must inspect that dynamic type
to decide what to do next.

`Classify` reads the type word with a type switch. Recall from the concepts file
that this is a linear scan of type comparisons, first match wins, so the order is
a deliberate choice: the `nil` case is first because it is the cheapest and most
distinctive check, and `default` is last because it matches everything. Grouping
`int, int8, int16, int32, int64` in a single case is a set of comparisons that all
map to the one `"int"` label — inside that case `x` keeps the static type `any`
(a multi-type case cannot narrow to one type), which is fine because we only need
the label.

The subtle case is `nil`. `case nil` matches only the *untyped* nil interface —
the eface whose type word is itself nil. A *typed* nil, such as `(*int)(nil)`
placed into an `any`, has a non-nil type word (`*int`), so it does not match
`case nil`; it falls through to `default` and is reported as `other(*int)`. That
is the typed-nil trap made visible: `x == nil` and `case nil` agree here, and both
correctly say a typed nil pointer is *not* the untyped nil.

Create `dispatch.go`:

```go
package dispatch

import "fmt"

// Kind labels the dynamic type of a decoded leaf value.
type Kind struct {
	Name string
}

// Classify inspects the dynamic type stored in an any and returns a Kind label.
// It is the value-typing stage of a config/event ingestion pipeline: after
// encoding/json decodes a document into map[string]any, every leaf arrives as an
// any and must be labelled by its concrete type before routing or validation.
func Classify(v any) Kind {
	switch x := v.(type) {
	case nil:
		return Kind{Name: "null"}
	case bool:
		return Kind{Name: "bool"}
	case int, int8, int16, int32, int64:
		return Kind{Name: "int"}
	case uint, uint8, uint16, uint32, uint64:
		return Kind{Name: "uint"}
	case float32, float64:
		return Kind{Name: "float"}
	case string:
		return Kind{Name: "string"}
	case []byte:
		return Kind{Name: "bytes"}
	default:
		return Kind{Name: fmt.Sprintf("other(%T)", x)}
	}
}
```

### The runnable demo

The demo decodes a small config document into `map[string]any`, sorts the keys
(map iteration order is randomized, so sorting keeps the output deterministic),
and classifies each leaf. It then classifies two values `json.Unmarshal` never
produces on its own — a raw `[]byte` and a typed nil pointer — to show the `bytes`
and `default` branches.

Create `cmd/demo/main.go`:

```go
package main

import (
	"encoding/json"
	"fmt"
	"sort"

	"example.com/dispatcher"
)

func main() {
	const doc = `{"enabled": true, "retries": 3, "name": "svc"}`

	var m map[string]any
	if err := json.Unmarshal([]byte(doc), &m); err != nil {
		panic(err)
	}

	keys := make([]string, 0, len(m))
	for k := range m {
		keys = append(keys, k)
	}
	sort.Strings(keys)

	for _, k := range keys {
		fmt.Printf("%s -> %s\n", k, dispatch.Classify(m[k]).Name)
	}

	fmt.Println("raw bytes ->", dispatch.Classify([]byte("x")).Name)
	fmt.Println("typed nil ->", dispatch.Classify((*int)(nil)).Name)
}
```

Run it:

```bash
go run ./cmd/demo
```

Expected output:

```
enabled -> bool
name -> string
retries -> float
raw bytes -> bytes
typed nil -> other(*int)
```

Note `retries` is `float`: JSON `3` decodes to `float64`.

### Tests

The tests fix each contract. The family tables assert that every width of int,
uint, and float reaches the single label for its family — proof that the grouped
cases behave as one. `TestClassifyUnknownStruct` proves an arbitrary struct reaches
`default`. `TestClassifyTypedNil` is the case the original lesson asked for: a
typed nil wrapped in `any` must reach `default`, not the `nil` case, pinning the
"a typed nil is not the untyped nil" contract. `TestClassifyIsConsistent` is a
property test: the same input always yields the same label.

Create `dispatch_test.go`:

```go
package dispatch

import (
	"fmt"
	"testing"
)

func TestClassifyByFamily(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name string
		vals []any
		want string
	}{
		{"nil", []any{nil}, "null"},
		{"bool", []any{true, false}, "bool"},
		{"int", []any{int(1), int8(1), int16(1), int32(1), int64(1)}, "int"},
		{"uint", []any{uint(1), uint8(1), uint16(1), uint32(1), uint64(1)}, "uint"},
		{"float", []any{float32(1), float64(1)}, "float"},
		{"string", []any{"hello", ""}, "string"},
		{"bytes", []any{[]byte("x"), []byte(nil)}, "bytes"},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			for _, v := range tc.vals {
				if got := Classify(v); got.Name != tc.want {
					t.Fatalf("Classify(%#v).Name = %q, want %q", v, got.Name, tc.want)
				}
			}
		})
	}
}

func TestClassifyUnknownStruct(t *testing.T) {
	t.Parallel()

	type custom struct{ X int }
	got := Classify(custom{X: 1})
	if got.Name == "int" || got.Name == "string" {
		t.Fatalf("Classify(custom) = %q, want an other(...) label", got.Name)
	}
	if got.Name != "other(dispatch.custom)" {
		t.Fatalf("Classify(custom) = %q, want other(dispatch.custom)", got.Name)
	}
}

func TestClassifyTypedNil(t *testing.T) {
	t.Parallel()

	// A typed nil pointer wrapped in any has a non-nil type word, so it must
	// reach default, not the nil case.
	var p *int
	if got := Classify(p); got.Name != "other(*int)" {
		t.Fatalf("Classify(typed nil *int) = %q, want other(*int)", got.Name)
	}
}

func TestClassifyIsConsistent(t *testing.T) {
	t.Parallel()

	for _, v := range []any{nil, true, 42, 3.14, "hello", []byte("x"), (*int)(nil)} {
		first := Classify(v)
		second := Classify(v)
		if first.Name != second.Name {
			t.Fatalf("inconsistent for %#v: %q vs %q", v, first.Name, second.Name)
		}
	}
}

func ExampleClassify() {
	fmt.Println(Classify(true).Name)
	fmt.Println(Classify(3.14).Name)
	fmt.Println(Classify((*int)(nil)).Name)
	// Output:
	// bool
	// float
	// other(*int)
}
```

## Review

The dispatcher is correct when each family maps to exactly one label and the two
edges hold: an unknown type lands in `default` with its concrete type named, and a
typed nil lands in `default` rather than `case nil`. If a typed-nil test ever
reports `null`, the switch is matching the untyped-nil case for a value whose type
word is set — which would mean the runtime lost the concrete type, and it never
does. The ordering discipline is the other lesson: `nil` first is a choice, but
`default` last is a rule, because `default` matches everything and would shadow
every case above it. Run `go test -race` to confirm the classifier is pure and
free of shared state.

## Resources

- [Go blog: Go Data Structures: Interfaces (Russ Cox)](https://research.swtch.com/interfaces) — the two-word layout and type word this exercise reads.
- [Go Specification: Type switches](https://go.dev/ref/spec#Type_switches) — the exact semantics of `switch x.(type)`, including the `nil` case.
- [encoding/json](https://pkg.go.dev/encoding/json#Unmarshal) — why `Unmarshal` into `any` yields `float64` for every number.

---

Back to [00-concepts.md](00-concepts.md) | Next: [02-typed-nil-error-trap.md](02-typed-nil-error-trap.md)
