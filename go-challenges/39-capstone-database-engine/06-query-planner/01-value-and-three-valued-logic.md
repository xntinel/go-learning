# Exercise 1: Value Type and Three-Valued Logic

Every column in a SQL table can be NULL, and NULL is not a value but the absence of one. That single fact forces the whole engine onto three-valued logic, so the foundation of a query executor is a scalar type that carries its own nullability and a comparison routine that knows what to do when either side is NULL. This exercise builds that `Value` type — an integer, float, string, boolean, or NULL — together with the `CompareValues` ordering that every sort, merge, and filter downstream will lean on.

This module is fully self-contained. It depends on nothing but the standard library and ships its own demo and tests. Nothing here imports any other exercise.

## What you'll build

```text
value.go              Kind, Value, constructors, IsNull/As*, ToBool, String, CompareValues
cmd/
  demo/
    main.go           compare NULLs, coerce int/float, print three-valued results
value_test.go         NULL ordering, int/float coercion, incomparable types, ToBool
```

- Files: `value.go`, `cmd/demo/main.go`, `value_test.go`.
- Implement: `Kind`, `Value`, `Null`, `IntVal`/`FloatVal`/`StrVal`/`BoolVal`, `IsNull`, `AsInt`/`AsFloat`/`AsString`/`AsBool`, `ToBool`, `String`, and `CompareValues`.
- Test: `value_test.go` pins NULL-before-everything ordering, int/float coercion, incomparable-type detection, and the `ToBool` coercion.
- Verify: `go test -race ./...`

Set up the module:

```bash
mkdir -p go-solutions/39-capstone-database-engine/06-query-planner/01-value-and-three-valued-logic/cmd/demo && cd go-solutions/39-capstone-database-engine/06-query-planner/01-value-and-three-valued-logic
```

### Why a tagged union, and why NULL is special

A SQL scalar is a tagged union: a `Kind` discriminator plus storage for each possible payload. Booleans piggyback on the integer slot (0 or 1) so the struct stays small and comparable. The constructors (`IntVal`, `FloatVal`, `StrVal`, `BoolVal`) are the only way to build a non-NULL value, and `Null` is a package-level value with `kind == KindNull`, so "is this NULL?" is one field check.

The `ToBool` method encodes the filtering half of three-valued logic: a predicate keeps a row only when it evaluates to TRUE, and `ToBool` maps NULL to false so an unknown predicate suppresses its row. It is deliberately lossy — it collapses UNKNOWN and FALSE into the same Go `bool` — which is correct for the final keep/drop decision but wrong for combining predicates with AND and OR, where UNKNOWN must stay distinct. That combination logic lives in a later exercise; here `ToBool` only serves the final decision.

`CompareValues` is the ordering primitive. It returns a three-way result plus an `ok` flag: NULL sorts before every concrete value (so `CompareValues(NULL, anything)` is -1), two NULLs are equal, integers and floats are coerced so `2` and `2.5` are comparable, and genuinely incomparable types (a string against an integer) return `ok == false` rather than a misleading order. SQL leaves NULL ordering implementation-defined, but a deterministic total order is exactly what the sort and sort-merge-join operators need, so we fix NULL as the smallest.

Create `value.go`:

```go
package planner

import "fmt"

// Kind identifies the runtime type of a Value.
type Kind uint8

const (
	KindNull   Kind = iota
	KindInt         // int64
	KindFloat       // float64
	KindString      // string
	KindBool        // represented as ival: 0=false, 1=true
)

// Value is a nullable SQL scalar.
type Value struct {
	kind Kind
	ival int64
	fval float64
	sval string
}

// Null is the SQL NULL value.
var Null = Value{kind: KindNull}

func IntVal(v int64) Value     { return Value{kind: KindInt, ival: v} }
func FloatVal(v float64) Value { return Value{kind: KindFloat, fval: v} }
func StrVal(v string) Value    { return Value{kind: KindString, sval: v} }
func BoolVal(v bool) Value {
	if v {
		return Value{kind: KindBool, ival: 1}
	}
	return Value{kind: KindBool, ival: 0}
}

func (v Value) IsNull() bool { return v.kind == KindNull }

func (v Value) AsInt() (int64, bool) {
	if v.kind != KindInt {
		return 0, false
	}
	return v.ival, true
}

func (v Value) AsFloat() (float64, bool) {
	if v.kind != KindFloat {
		return 0, false
	}
	return v.fval, true
}

func (v Value) AsString() (string, bool) {
	if v.kind != KindString {
		return "", false
	}
	return v.sval, true
}

func (v Value) AsBool() (bool, bool) {
	if v.kind != KindBool {
		return false, false
	}
	return v.ival != 0, true
}

// ToBool converts a Value to bool using three-valued logic: NULL -> false.
func (v Value) ToBool() bool {
	switch v.kind {
	case KindBool:
		return v.ival != 0
	case KindNull:
		return false
	case KindInt:
		return v.ival != 0
	default:
		return false
	}
}

func (v Value) String() string {
	switch v.kind {
	case KindNull:
		return "NULL"
	case KindInt:
		return fmt.Sprintf("%d", v.ival)
	case KindFloat:
		return fmt.Sprintf("%g", v.fval)
	case KindString:
		return v.sval
	case KindBool:
		if v.ival != 0 {
			return "true"
		}
		return "false"
	default:
		return "?"
	}
}

// CompareValues returns -1, 0, or 1 for ordering. NULL sorts before all other values.
// Returns (0, false) if the types are incomparable.
func CompareValues(a, b Value) (int, bool) {
	if a.IsNull() && b.IsNull() {
		return 0, true
	}
	if a.IsNull() {
		return -1, true
	}
	if b.IsNull() {
		return 1, true
	}
	if a.kind != b.kind {
		// Numeric coercion: int vs float.
		if a.kind == KindInt && b.kind == KindFloat {
			af := float64(a.ival)
			if af < b.fval {
				return -1, true
			}
			if af > b.fval {
				return 1, true
			}
			return 0, true
		}
		if a.kind == KindFloat && b.kind == KindInt {
			bf := float64(b.ival)
			if a.fval < bf {
				return -1, true
			}
			if a.fval > bf {
				return 1, true
			}
			return 0, true
		}
		return 0, false
	}
	switch a.kind {
	case KindInt:
		if a.ival < b.ival {
			return -1, true
		}
		if a.ival > b.ival {
			return 1, true
		}
		return 0, true
	case KindFloat:
		if a.fval < b.fval {
			return -1, true
		}
		if a.fval > b.fval {
			return 1, true
		}
		return 0, true
	case KindString:
		if a.sval < b.sval {
			return -1, true
		}
		if a.sval > b.sval {
			return 1, true
		}
		return 0, true
	case KindBool:
		if a.ival < b.ival {
			return -1, true
		}
		if a.ival > b.ival {
			return 1, true
		}
		return 0, true
	}
	return 0, false
}
```

### The runnable demo

The demo exercises the three behaviors that downstream operators depend on: NULL orders before a concrete value, an integer and a float are coerced into a single order, and two incomparable types report `ok == false` instead of inventing an order. It also shows `ToBool` collapsing NULL to false and `String` rendering NULL as the literal text `NULL`.

Create `cmd/demo/main.go`:

```go
package main

import (
	"fmt"

	planner "example.com/three-valued-logic"
)

func main() {
	cmp1, ok1 := planner.CompareValues(planner.Null, planner.IntVal(5))
	fmt.Printf("CompareValues(NULL, 5) = %d ok=%v\n", cmp1, ok1)

	cmp2, ok2 := planner.CompareValues(planner.IntVal(5), planner.FloatVal(2.5))
	fmt.Printf("CompareValues(5, 2.5) = %d ok=%v\n", cmp2, ok2)

	cmp3, ok3 := planner.CompareValues(planner.StrVal("a"), planner.IntVal(5))
	fmt.Printf("CompareValues(\"a\", 5) = %d ok=%v\n", cmp3, ok3)

	fmt.Printf("ToBool(NULL) = %v\n", planner.Null.ToBool())
	fmt.Printf("ToBool(BoolVal(true)) = %v\n", planner.BoolVal(true).ToBool())
	fmt.Printf("NULL prints as %q\n", planner.Null.String())
}
```

Run it:

```bash
go run ./cmd/demo
```

Expected output:

```text
CompareValues(NULL, 5) = -1 ok=true
CompareValues(5, 2.5) = 1 ok=true
CompareValues("a", 5) = 0 ok=false
ToBool(NULL) = false
ToBool(BoolVal(true)) = true
NULL prints as "NULL"
```

### Tests

The table pins the four ordering cases that the rest of the engine relies on — NULL before a value, a value after NULL, NULL equal to NULL, and int/float coercion — plus the incomparable case that must report `ok == false`. The `ToBool` test pins the coercion that the filter operator will use to make its final keep/drop decision.

Create `value_test.go`:

```go
package planner

import (
	"fmt"
	"testing"
)

func TestCompareValues(t *testing.T) {
	t.Parallel()

	cases := []struct {
		name string
		a, b Value
		cmp  int
		ok   bool
	}{
		{"null before int", Null, IntVal(5), -1, true},
		{"int after null", IntVal(5), Null, 1, true},
		{"null equals null", Null, Null, 0, true},
		{"int below float", IntVal(2), FloatVal(2.5), -1, true},
		{"incomparable types", StrVal("a"), IntVal(1), 0, false},
	}
	for _, tc := range cases {
		tc := tc
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			cmp, ok := CompareValues(tc.a, tc.b)
			if cmp != tc.cmp || ok != tc.ok {
				t.Fatalf("CompareValues = (%d,%v), want (%d,%v)", cmp, ok, tc.cmp, tc.ok)
			}
		})
	}
}

func TestToBool(t *testing.T) {
	t.Parallel()

	if !BoolVal(true).ToBool() {
		t.Fatal("BoolVal(true).ToBool() = false, want true")
	}
	if Null.ToBool() {
		t.Fatal("Null.ToBool() = true, want false")
	}
	if IntVal(0).ToBool() {
		t.Fatal("IntVal(0).ToBool() = true, want false")
	}
	if !IntVal(3).ToBool() {
		t.Fatal("IntVal(3).ToBool() = false, want true")
	}
}

func TestNullString(t *testing.T) {
	t.Parallel()

	if Null.String() != "NULL" {
		t.Fatalf("Null.String() = %q, want NULL", Null.String())
	}
}

func ExampleCompareValues() {
	cmp, ok := CompareValues(Null, IntVal(5))
	fmt.Println(cmp, ok)
	// Output: -1 true
}
```

## Review

The value type is correct when NULL is consistently the smallest thing in any order, when an integer and a float compare as numbers rather than as different kinds, and when truly incomparable types refuse to invent an order by returning `ok == false`. Confirm that `ToBool` maps NULL to false — this is the filtering coercion, not the AND/OR logic, which must keep UNKNOWN distinct and is built later — and that `String` renders NULL as the literal `NULL` so plan dumps and demo output read cleanly. The whole engine rests on these few methods, so a bug here surfaces as a mysterious wrong-row-count failure three exercises away.

## Resources

- [Volcano - An Extensible and Parallel Query Evaluation System, Graefe 1994](https://paperhub.s3.amazonaws.com/dace52a42c07f7f8348b08dc2b186061.pdf) — the iterator model these values flow through.
- [Modern SQL: Three-Valued Logic](https://modern-sql.com/concept/three-valued-logic) — NULL semantics for comparisons, AND, OR, and NOT.
- [pkg.go.dev/sort](https://pkg.go.dev/sort) — the stable-sort interface that consumes `CompareValues` in later operators.

---

Back to [00-concepts.md](00-concepts.md) | Next: [02-operator-interface.md](02-operator-interface.md)
