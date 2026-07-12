# Exercise 2: Schema, Tuple, and the Operator Interface

The volcano model gives every relational operator the same four-method shape, and every operator advertises the schema of the rows it produces. This exercise defines that contract: the `Operator` interface (`Init`, `Next`, `Close`, `Schema`), the `Schema` and `ColumnDef` that describe a row's columns, and the `Tuple` that carries one row's values. Get these right and every later operator — scan, filter, join, aggregate — slots into the same pull-based protocol.

This module is fully self-contained. It depends on nothing but the standard library and ships its own demo and tests. Nothing here imports any other exercise.

## What you'll build

```text
value.go              Value scaffolding reused by Tuple and ColumnDef
operator.go           ColumnDef, Schema, ColIndex, Tuple, Clone, Operator interface
cmd/
  demo/
    main.go           a minimal Operator driven through Init/Next/Close
operator_test.go      ColIndex resolution and Tuple.Clone independence
```

- Files: `value.go`, `operator.go`, `cmd/demo/main.go`, `operator_test.go`.
- Implement: `ColumnDef`, `Schema`, `Schema.ColIndex`, `Tuple`, `Tuple.Clone`, and the `Operator` interface.
- Test: `operator_test.go` pins qualified/unqualified/missing column resolution and that `Clone` produces an independent copy.
- Verify: `go test -race ./...`

Set up the module:

```bash
mkdir -p go-solutions/39-capstone-database-engine/06-query-planner/02-operator-interface/cmd/demo && cd go-solutions/39-capstone-database-engine/06-query-planner/02-operator-interface
```

### The four-method contract

Every operator implements `Init`, `Next`, `Close`, and `Schema`. `Init` (the classic "Open") acquires resources and rewinds: a scan resets its cursor, a sort drains and orders its input, a hash join builds its table. `Next` returns exactly one tuple, or a nil tuple to signal end-of-stream; the consumer calls it repeatedly until nil. `Close` releases resources no matter how many rows were consumed, so a query that stops early still frees its handles. `Schema` reports the ordered output columns, which lets the planner resolve a reference like `users.id` to a position at plan time, before any data moves.

A schema is an ordered list of `ColumnDef`, each a column name, an optional table qualifier, and a kind. `ColIndex` resolves a `(table, name)` reference to its position, returning -1 when nothing matches. The table qualifier is optional on the lookup side: an unqualified request matches the first column with the right name, and a column with no recorded table matches any qualifier, which keeps the common single-table case simple while still letting a join disambiguate `users.id` from `orders.id`.

A `Tuple` is a slice of values in schema order. `Clone` makes a deep copy of that slice, and it is not a convenience — it is a correctness requirement. Operators that retain tuples across `Next` calls, most importantly the build side of a hash join, must not alias into a scan's backing array, or the next read would overwrite values they still hold. Cloning at the boundary makes every emitted tuple independent of the storage it came from.

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
```

Create `operator.go`:

```go
package planner

// ColumnDef describes one column in a schema.
type ColumnDef struct {
	Name  string
	Table string // qualifier; empty means unqualified
	Kind  Kind
}

// Schema is an ordered list of column definitions.
type Schema []ColumnDef

// ColIndex returns the zero-based index of the first column matching name
// (and optionally table qualifier). Returns -1 if not found.
func (s Schema) ColIndex(table, name string) int {
	for i, c := range s {
		if c.Name == name && (table == "" || c.Table == table || c.Table == "") {
			return i
		}
	}
	return -1
}

// Tuple is a row in schema order.
type Tuple struct {
	Values []Value
}

// Clone returns a deep copy of the tuple.
func (t *Tuple) Clone() *Tuple {
	if t == nil {
		return nil
	}
	vals := make([]Value, len(t.Values))
	copy(vals, t.Values)
	return &Tuple{Values: vals}
}

// Operator is the volcano-model iterator interface.
// Init must be called before the first Next.
// Next returns nil, nil to signal end of stream.
// Close must be called when done, even on error.
type Operator interface {
	Init() error
	Next() (*Tuple, error)
	Close() error
	Schema() Schema
}
```

### The runnable demo

To show the protocol in motion the demo defines `sliceOp`, the smallest possible operator: it yields a fixed slice of tuples and reports a schema. Driving it through `Init`, repeated `Next`, and `Close` is exactly how a consumer drives the root of any plan. The demo then resolves a few column references through `ColIndex` and proves `Clone` isolates a copy by mutating the clone and showing the original is untouched.

Create `cmd/demo/main.go`:

```go
package main

import (
	"fmt"

	planner "example.com/operator-interface"
)

// sliceOp is a minimal Operator that yields a fixed slice of tuples, showing the
// volcano Init/Next/Close pull protocol.
type sliceOp struct {
	schema planner.Schema
	rows   []*planner.Tuple
	pos    int
}

func (s *sliceOp) Init() error            { s.pos = 0; return nil }
func (s *sliceOp) Schema() planner.Schema { return s.schema }
func (s *sliceOp) Close() error           { return nil }

func (s *sliceOp) Next() (*planner.Tuple, error) {
	if s.pos >= len(s.rows) {
		return nil, nil
	}
	t := s.rows[s.pos]
	s.pos++
	return t, nil
}

func main() {
	schema := planner.Schema{
		{Name: "id", Table: "users", Kind: planner.KindInt},
		{Name: "name", Table: "users", Kind: planner.KindString},
	}
	op := &sliceOp{
		schema: schema,
		rows: []*planner.Tuple{
			{Values: []planner.Value{planner.IntVal(1), planner.StrVal("alice")}},
			{Values: []planner.Value{planner.IntVal(2), planner.StrVal("bob")}},
		},
	}

	if err := op.Init(); err != nil {
		panic(err)
	}
	defer op.Close()

	fmt.Println("pulling tuples:")
	for {
		t, err := op.Next()
		if err != nil {
			panic(err)
		}
		if t == nil {
			break
		}
		fmt.Printf("  id=%s name=%s\n", t.Values[0].String(), t.Values[1].String())
	}

	fmt.Printf("ColIndex(users.name) = %d\n", schema.ColIndex("users", "name"))
	fmt.Printf("ColIndex(id) = %d\n", schema.ColIndex("", "id"))
	fmt.Printf("ColIndex(missing) = %d\n", schema.ColIndex("", "missing"))

	orig := &planner.Tuple{Values: []planner.Value{planner.IntVal(7)}}
	clone := orig.Clone()
	clone.Values[0] = planner.IntVal(99)
	fmt.Printf("orig=%s clone=%s\n", orig.Values[0].String(), clone.Values[0].String())
}
```

Run it:

```bash
go run ./cmd/demo
```

Expected output:

```text
pulling tuples:
  id=1 name=alice
  id=2 name=bob
ColIndex(users.name) = 1
ColIndex(id) = 0
ColIndex(missing) = -1
orig=7 clone=99
```

### Tests

The first test pins all three `ColIndex` outcomes — qualified hit, unqualified hit, and a miss returning -1 — because the planner and projection both depend on -1 meaning "no such column." The second test mutates a clone and asserts the original is unchanged, locking in the aliasing fix that the hash join will rely on.

Create `operator_test.go`:

```go
package planner

import (
	"fmt"
	"testing"
)

func TestColIndex(t *testing.T) {
	t.Parallel()

	s := Schema{
		{Name: "id", Table: "users", Kind: KindInt},
		{Name: "name", Table: "users", Kind: KindString},
	}
	if got := s.ColIndex("users", "name"); got != 1 {
		t.Fatalf("qualified ColIndex = %d, want 1", got)
	}
	if got := s.ColIndex("", "id"); got != 0 {
		t.Fatalf("unqualified ColIndex = %d, want 0", got)
	}
	if got := s.ColIndex("", "missing"); got != -1 {
		t.Fatalf("missing ColIndex = %d, want -1", got)
	}
}

func TestTupleClone(t *testing.T) {
	t.Parallel()

	orig := &Tuple{Values: []Value{IntVal(1), StrVal("a")}}
	clone := orig.Clone()
	clone.Values[0] = IntVal(99)

	if got, _ := orig.Values[0].AsInt(); got != 1 {
		t.Fatalf("clone mutation leaked into original: id=%d, want 1", got)
	}
	if got, _ := clone.Values[0].AsInt(); got != 99 {
		t.Fatalf("clone not updated: id=%d, want 99", got)
	}
}

func ExampleSchema_ColIndex() {
	s := Schema{
		{Name: "id", Table: "t", Kind: KindInt},
	}
	fmt.Println(s.ColIndex("", "id"), s.ColIndex("", "nope"))
	// Output: 0 -1
}
```

## Review

The interface is correct when a consumer can drive any operator with the same `Init` / loop-on-`Next` / `Close` sequence the demo uses, and when `Next` returning a nil tuple is the one and only end-of-stream signal. Confirm `ColIndex` returns -1 (not 0) for a missing column, since callers branch on the negative result, and that `Clone` copies the value slice rather than sharing it — the test proves a clone mutation cannot reach the original, which is the property the hash join's retained build rows depend on.

## Resources

- [Volcano - An Extensible and Parallel Query Evaluation System, Graefe 1994](https://paperhub.s3.amazonaws.com/dace52a42c07f7f8348b08dc2b186061.pdf) — the original Open/Next/Close iterator interface.
- [CMU 15-445 Query Execution](https://15445.courses.cs.cmu.edu/fall2024/slides/) — physical operators and the iterator execution model.
- [pkg.go.dev/builtin](https://pkg.go.dev/builtin) — `copy` and slice semantics behind `Tuple.Clone`.

---

Back to [01-value-and-three-valued-logic.md](01-value-and-three-valued-logic.md) | Next: [03-catalog-and-sequential-scan.md](03-catalog-and-sequential-scan.md)
