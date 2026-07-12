# Exercise 3: Catalog and Sequential Scan

Every query begins at a leaf: an operator that reads rows out of a stored table. This exercise builds that leaf in two forms — a sequential scan that walks every row, and an index scan that does a point lookup through an in-memory index — together with the catalog that holds table definitions and statistics. These are the only operators that touch storage; everything above them in the plan tree consumes their output through the volcano `Init`/`Next`/`Close` protocol.

This module is fully self-contained. It depends only on the standard library and ships its own demo and tests. Nothing here imports any other exercise.

## What you'll build

```text
value.go        Kind, Value, constructors, IsNull/As*, ToBool, String, CompareValues
operator.go     ColumnDef, Schema, ColIndex, Tuple, Clone, Operator interface
catalog.go      IndexDef, TableDef, Catalog, Register, Table, ErrTableNotFound
scan.go         SeqScanOperator, IndexScanOperator, Collect
cmd/
  demo/
    main.go      register a table, scan it, build an index, point-lookup
scan_test.go    sequential scan, index point lookup, missing-table error
```

- Files: `value.go`, `operator.go`, `catalog.go`, `scan.go`, `cmd/demo/main.go`, `scan_test.go`.
- Implement: `Catalog` with `Register`/`Table`, `SeqScanOperator` with `NewSeqScan`, `IndexScanOperator` with `NewIndexScan`, and the `Collect` drain helper.
- Test: a full sequential scan yields every row, an index lookup returns the single matching row, and an unknown table reports `ErrTableNotFound`.
- Verify: `go test -race ./...`

### The value type and the operator protocol

Before any operator exists there has to be something to carry through it. A `Value` is a nullable SQL scalar: it tags its own runtime kind (int, float, string, bool, or NULL) so that three-valued logic and ordering can be decided per value rather than per column. `CompareValues` defines a total order in which NULL sorts before every concrete value; sort and merge-join lean on that determinism even though SQL leaves NULL ordering implementation-defined.

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

A schema is the ordered list of columns an operator emits; a tuple is a row in schema order. `ColIndex` resolves a column reference to a position so operators above can extract by index. The `Operator` interface is the whole volcano contract: `Init` opens, `Next` yields one tuple or nil, `Close` releases, `Schema` reports the output columns.

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

### The catalog

The catalog is the engine's metadata store: it maps a table name to its schema, its rows, and any indexes. `EstimatedRows` is the single statistic a cost-based planner needs to choose between scan and join strategies (later exercises use it). An `IndexDef` here is deliberately simplified to a map from a column value to the rows carrying it — enough to model a point lookup without a real B-tree.

Create `catalog.go`:

```go
package planner

import (
	"errors"
	"fmt"
)

// ErrTableNotFound is returned by Catalog.Table when the table is absent.
var ErrTableNotFound = errors.New("table not found")

// IndexDef describes a single-column B-tree index.
type IndexDef struct {
	Name    string
	Column  string
	Entries map[Value][]*Tuple // simplified: value -> matching rows
}

// TableDef is the catalog entry for one table.
type TableDef struct {
	Name    string
	Columns Schema
	Rows    []*Tuple
	Indexes map[string]*IndexDef // column name -> index
}

// EstimatedRows returns the catalog's row-count estimate.
func (td *TableDef) EstimatedRows() int { return len(td.Rows) }

// Catalog is an in-memory schema registry.
type Catalog struct {
	tables map[string]*TableDef
}

func NewCatalog() *Catalog { return &Catalog{tables: make(map[string]*TableDef)} }

// Register adds or replaces a table definition.
func (c *Catalog) Register(td *TableDef) { c.tables[td.Name] = td }

// Table returns the definition for name or ErrTableNotFound.
func (c *Catalog) Table(name string) (*TableDef, error) {
	td, ok := c.tables[name]
	if !ok {
		return nil, fmt.Errorf("%w: %s", ErrTableNotFound, name)
	}
	return td, nil
}
```

### The two scan operators

`SeqScanOperator` walks the row slice and returns each row's clone. The clone is not optional: a scan that returned a pointer into `TableDef.Rows` would let a downstream operator (a hash join's build side, a sort buffer) retain a tuple that the next `Next` then overwrites. `IndexScanOperator` skips the walk entirely — `Init` looks the key up in the index once, and `Next` streams the matching rows. `Collect` is a small driver that runs the whole protocol and gathers the output, which the demo and tests use instead of hand-writing the `Init`/`Next`/`Close` loop each time.

Create `scan.go`:

```go
package planner

import "fmt"

// SeqScanOperator reads all rows from a TableDef sequentially.
type SeqScanOperator struct {
	table  *TableDef
	pos    int
	schema Schema
}

// NewSeqScan creates a sequential scan over td.
func NewSeqScan(td *TableDef) *SeqScanOperator {
	return &SeqScanOperator{table: td, schema: td.Columns}
}

func (s *SeqScanOperator) Init() error    { s.pos = 0; return nil }
func (s *SeqScanOperator) Schema() Schema { return s.schema }

func (s *SeqScanOperator) Next() (*Tuple, error) {
	if s.pos >= len(s.table.Rows) {
		return nil, nil
	}
	t := s.table.Rows[s.pos]
	s.pos++
	return t.Clone(), nil
}

func (s *SeqScanOperator) Close() error { return nil }

// IndexScanOperator performs a point or range lookup via an IndexDef.
type IndexScanOperator struct {
	index   *IndexDef
	schema  Schema
	key     Value
	matches []*Tuple
	pos     int
}

func NewIndexScan(td *TableDef, columnName string, key Value) (*IndexScanOperator, error) {
	idx, ok := td.Indexes[columnName]
	if !ok {
		return nil, fmt.Errorf("no index on column %q in table %q", columnName, td.Name)
	}
	return &IndexScanOperator{index: idx, schema: td.Columns, key: key}, nil
}

func (s *IndexScanOperator) Schema() Schema { return s.schema }

func (s *IndexScanOperator) Init() error {
	s.matches = s.index.Entries[s.key]
	s.pos = 0
	return nil
}

func (s *IndexScanOperator) Next() (*Tuple, error) {
	if s.pos >= len(s.matches) {
		return nil, nil
	}
	t := s.matches[s.pos].Clone()
	s.pos++
	return t, nil
}

func (s *IndexScanOperator) Close() error { return nil }

// Collect drains all rows from an operator into a slice.
func Collect(op Operator) ([]*Tuple, error) {
	if err := op.Init(); err != nil {
		return nil, err
	}
	defer op.Close()
	var rows []*Tuple
	for {
		t, err := op.Next()
		if err != nil {
			return nil, err
		}
		if t == nil {
			break
		}
		rows = append(rows, t)
	}
	return rows, nil
}
```

### The runnable demo

The demo registers a `users` table, scans every row through the sequential operator, then builds an in-memory index on `id` and resolves a single key through the index scan — the same protocol, two cost profiles.

Create `cmd/demo/main.go`:

```go
package main

import (
	"fmt"
	"log"

	planner "example.com/catalog-and-scan"
)

func main() {
	cat := planner.NewCatalog()
	schema := planner.Schema{
		{Name: "id", Table: "users", Kind: planner.KindInt},
		{Name: "name", Table: "users", Kind: planner.KindString},
		{Name: "age", Table: "users", Kind: planner.KindInt},
	}
	users := &planner.TableDef{
		Name:    "users",
		Columns: schema,
		Rows: []*planner.Tuple{
			{Values: []planner.Value{planner.IntVal(1), planner.StrVal("alice"), planner.IntVal(30)}},
			{Values: []planner.Value{planner.IntVal(2), planner.StrVal("bob"), planner.IntVal(25)}},
			{Values: []planner.Value{planner.IntVal(3), planner.StrVal("carol"), planner.IntVal(30)}},
		},
		Indexes: make(map[string]*planner.IndexDef),
	}
	cat.Register(users)

	td, err := cat.Table("users")
	if err != nil {
		log.Fatal(err)
	}
	rows, err := planner.Collect(planner.NewSeqScan(td))
	if err != nil {
		log.Fatal(err)
	}
	fmt.Printf("seq scan users (%d rows):\n", len(rows))
	for _, r := range rows {
		fmt.Printf("  id=%s name=%s age=%s\n", r.Values[0].String(), r.Values[1].String(), r.Values[2].String())
	}

	// Build an in-memory index on id and do a point lookup.
	idx := &planner.IndexDef{Name: "users_id_idx", Column: "id", Entries: make(map[planner.Value][]*planner.Tuple)}
	for _, r := range users.Rows {
		idx.Entries[r.Values[0]] = append(idx.Entries[r.Values[0]], r)
	}
	users.Indexes["id"] = idx

	scan, err := planner.NewIndexScan(td, "id", planner.IntVal(2))
	if err != nil {
		log.Fatal(err)
	}
	hits, err := planner.Collect(scan)
	if err != nil {
		log.Fatal(err)
	}
	fmt.Printf("index lookup id=2: %s\n", hits[0].Values[1].String())
}
```

Run it:

```bash
go run ./cmd/demo
```

Expected output:

```
seq scan users (3 rows):
  id=1 name=alice age=30
  id=2 name=bob age=25
  id=3 name=carol age=30
index lookup id=2: bob
```

### Tests

The tests pin three properties: the sequential scan visits every row, the index scan resolves a key to exactly its matching row, and the catalog reports a typed sentinel for an unknown table so callers can branch on `errors.Is`.

Create `scan_test.go`:

```go
package planner

import (
	"errors"
	"fmt"
	"testing"
)

func makeUsersTable() *TableDef {
	schema := Schema{
		{Name: "id", Table: "users", Kind: KindInt},
		{Name: "name", Table: "users", Kind: KindString},
		{Name: "age", Table: "users", Kind: KindInt},
	}
	rows := []*Tuple{
		{Values: []Value{IntVal(1), StrVal("alice"), IntVal(30)}},
		{Values: []Value{IntVal(2), StrVal("bob"), IntVal(25)}},
		{Values: []Value{IntVal(3), StrVal("carol"), IntVal(30)}},
		{Values: []Value{IntVal(4), StrVal("dave"), Null}},
	}
	return &TableDef{Name: "users", Columns: schema, Rows: rows, Indexes: make(map[string]*IndexDef)}
}

func TestSeqScanYieldsAllRows(t *testing.T) {
	t.Parallel()

	rows, err := Collect(NewSeqScan(makeUsersTable()))
	if err != nil {
		t.Fatal(err)
	}
	if len(rows) != 4 {
		t.Fatalf("got %d rows, want 4", len(rows))
	}
}

func TestIndexScanPointLookup(t *testing.T) {
	t.Parallel()

	td := makeUsersTable()
	idx := &IndexDef{Name: "users_id_idx", Column: "id", Entries: make(map[Value][]*Tuple)}
	for _, r := range td.Rows {
		idx.Entries[r.Values[0]] = append(idx.Entries[r.Values[0]], r)
	}
	td.Indexes["id"] = idx

	scan, err := NewIndexScan(td, "id", IntVal(2))
	if err != nil {
		t.Fatal(err)
	}
	rows, err := Collect(scan)
	if err != nil {
		t.Fatal(err)
	}
	if len(rows) != 1 {
		t.Fatalf("index scan: got %d rows, want 1", len(rows))
	}
	if rows[0].Values[1].String() != "bob" {
		t.Fatalf("index scan name = %q, want bob", rows[0].Values[1].String())
	}
}

func TestCatalogTableNotFound(t *testing.T) {
	t.Parallel()

	cat := NewCatalog()
	cat.Register(makeUsersTable())
	if _, err := cat.Table("missing"); !errors.Is(err, ErrTableNotFound) {
		t.Fatalf("err = %v, want ErrTableNotFound", err)
	}
}

func ExampleSeqScanOperator() {
	schema := Schema{
		{Name: "id", Table: "t", Kind: KindInt},
		{Name: "val", Table: "t", Kind: KindString},
	}
	rows := []*Tuple{
		{Values: []Value{IntVal(1), StrVal("a")}},
		{Values: []Value{IntVal(2), StrVal("b")}},
		{Values: []Value{IntVal(3), StrVal("c")}},
	}
	td := &TableDef{Name: "t", Columns: schema, Rows: rows, Indexes: make(map[string]*IndexDef)}
	rows2, _ := Collect(NewSeqScan(td))
	fmt.Println(len(rows2))
	// Output: 3
}
```

## Review

The scan layer is correct when the sequential scan returns one independent clone per stored row, the index scan returns exactly the rows registered under the lookup key, and an absent table surfaces `ErrTableNotFound` rather than a nil-map panic. The cloning rule is the one that bites later: if `SeqScanOperator.Next` ever returns the stored `*Tuple` directly, an operator that holds tuples across `Next` calls will see them mutated underneath it. `Collect` exists so callers never forget the `Init`-then-`Next`-until-nil-then-`Close` sequence, which is the protocol every operator in this lesson obeys.

## Resources

- [Volcano - An Extensible and Parallel Query Evaluation System, Graefe 1994](https://paperhub.s3.amazonaws.com/dace52a42c07f7f8348b08dc2b186061.pdf) — the original iterator model that `Init`/`Next`/`Close` follows.
- [CMU 15-445 Query Execution](https://15445.courses.cs.cmu.edu/fall2024/slides/) — access methods, sequential vs index scan, and the iterator interface.
- [pkg.go.dev/errors](https://pkg.go.dev/errors) — `errors.New` and `errors.Is` for the sentinel `ErrTableNotFound`.

---

Back to [02-operator-interface.md](02-operator-interface.md) | Next: [04-expression-evaluator-and-filter.md](04-expression-evaluator-and-filter.md)
