# Exercise 4: Expression Evaluator and Filter Operator

A scan emits rows; a WHERE clause keeps some of them. This exercise builds the expression tree that a predicate compiles to, the evaluator that runs it against a tuple under SQL three-valued logic, and the two operators that consume the result: a filter that passes rows whose predicate is TRUE and a projection that selects and renames columns. The evaluator is where NULL stops being a footnote: every comparison touching NULL yields NULL, AND and OR follow the three-valued truth tables, and a row survives the filter only when the predicate is genuinely TRUE — UNKNOWN is dropped.

This module is fully self-contained. It depends only on the standard library and ships its own demo and tests. Nothing here imports any other exercise.

## What you'll build

```text
value.go        Kind, Value, ToBool, CompareValues
operator.go     Schema, ColIndex, Tuple, Operator
catalog.go      TableDef, Catalog (storage for the scan source)
scan.go         SeqScanOperator (with optional pushed filter), IndexScan, Collect
filter.go       FilterExpr, Eval, likeMatch, FilterOperator, ProjectionOperator
cmd/
  demo/
    main.go      filter age>20 (drops NULL), project name, LIKE 'a%'
filter_test.go  NULL handling, IS NULL/IS NOT NULL, projection, LIKE
```

- Files: `value.go`, `operator.go`, `catalog.go`, `scan.go`, `filter.go`, `cmd/demo/main.go`, `filter_test.go`.
- Implement: `FilterExpr` with `Eval`/`evalBinOp` (three-valued AND/OR), `likeMatch`, `FilterOperator`, `ProjectionOperator`, and `ColumnNotFoundError`/`ErrColumnNotFound`.
- Test: `age > 20` excludes the NULL-age row, `IS NULL`/`IS NOT NULL` partition the rows, projection narrows the tuple, an unknown projected column reports `ErrColumnNotFound`, and `LIKE` matches `%`/`_`.
- Verify: `go test -race ./...`

Set up the module:

```bash
mkdir -p filter-and-projection/cmd/demo && cd filter-and-projection
go mod init example.com/filter-and-projection
```

### Values, schema, and storage

The value type, operator interface, and catalog are the same substrate as the previous module; the scan here keeps an optional pushed-down predicate so a filter can later be fused into the scan, though this exercise drives it with `nil` and filters above.

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
type Operator interface {
	Init() error
	Next() (*Tuple, error)
	Close() error
	Schema() Schema
}
```

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
	Entries map[Value][]*Tuple
}

// TableDef is the catalog entry for one table.
type TableDef struct {
	Name    string
	Columns Schema
	Rows    []*Tuple
	Indexes map[string]*IndexDef
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

Create `scan.go`:

```go
package planner

import "fmt"

// SeqScanOperator reads all rows from a TableDef sequentially.
type SeqScanOperator struct {
	table  *TableDef
	filter *FilterExpr // optional pushed-down predicate
	pos    int
	schema Schema
}

// NewSeqScan creates a sequential scan, optionally with a pushed-down filter.
func NewSeqScan(td *TableDef, filter *FilterExpr) *SeqScanOperator {
	return &SeqScanOperator{table: td, filter: filter, schema: td.Columns}
}

func (s *SeqScanOperator) Init() error    { s.pos = 0; return nil }
func (s *SeqScanOperator) Schema() Schema { return s.schema }

func (s *SeqScanOperator) Next() (*Tuple, error) {
	for s.pos < len(s.table.Rows) {
		t := s.table.Rows[s.pos]
		s.pos++
		if s.filter == nil {
			return t.Clone(), nil
		}
		result := s.filter.Eval(t, s.schema)
		if result.ToBool() {
			return t.Clone(), nil
		}
	}
	return nil, nil
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

### The expression tree and three-valued evaluation

A `FilterExpr` is a small tree: literals, column references, binary operators, `NOT`, and the two NULL tests. `Eval` walks it against a tuple and returns a `Value`, never a Go `bool`, so that UNKNOWN can be represented as `Null` and propagate correctly. The interesting code is `evalBinOp`. For AND it short-circuits on a FALSE left operand (FALSE dominates a conjunction) and otherwise inspects NULLs explicitly: `NULL AND TRUE` is UNKNOWN (Null), `NULL AND FALSE` is FALSE. For OR it short-circuits on TRUE and treats `FALSE OR NULL` as UNKNOWN. Every other comparison returns `Null` the instant either side is NULL, so `age > 20` over a NULL age yields UNKNOWN and the row is dropped — exactly what SQL requires. `likeMatch` implements `%` (any run) and `_` (one character) with a linear backtracking matcher.

The `FilterOperator` keeps a row only when `pred.Eval(...).ToBool()` is true, and `ProjectionOperator` resolves each requested column name to a child-schema index once, in its constructor, then extracts by index per row. An unrecognized projected column produces `ColumnNotFoundError`, which unwraps to the sentinel `ErrColumnNotFound` so callers can match it with `errors.Is`.

Create `filter.go`:

```go
package planner

import (
	"errors"
	"strings"
)

// ExprKind identifies the expression node type.
type ExprKind uint8

const (
	ExprLiteral ExprKind = iota
	ExprColumn
	ExprBinOp
	ExprUnaryNot
	ExprIsNull
	ExprIsNotNull
)

// BinOp is a binary operator in a filter expression.
type BinOp uint8

const (
	BinEq   BinOp = iota // =
	BinNeq               // <>
	BinLt                // <
	BinLte               // <=
	BinGt                // >
	BinGte               // >=
	BinAnd               // AND
	BinOr                // OR
	BinLike              // LIKE (% and _ wildcards)
)

// FilterExpr is a node in an expression tree.
type FilterExpr struct {
	kind    ExprKind
	literal Value
	table   string
	col     string
	op      BinOp
	left    *FilterExpr
	right   *FilterExpr
}

// Literal wraps a constant value.
func Literal(v Value) *FilterExpr { return &FilterExpr{kind: ExprLiteral, literal: v} }

// Col references a column by optional table qualifier and name.
func Col(table, name string) *FilterExpr {
	return &FilterExpr{kind: ExprColumn, table: table, col: name}
}

// Binop creates a binary expression node.
func Binop(op BinOp, left, right *FilterExpr) *FilterExpr {
	return &FilterExpr{kind: ExprBinOp, op: op, left: left, right: right}
}

// Not wraps an expression in a NOT.
func Not(e *FilterExpr) *FilterExpr { return &FilterExpr{kind: ExprUnaryNot, left: e} }

// IsNull creates an IS NULL check.
func IsNull(e *FilterExpr) *FilterExpr { return &FilterExpr{kind: ExprIsNull, left: e} }

// IsNotNull creates an IS NOT NULL check.
func IsNotNull(e *FilterExpr) *FilterExpr { return &FilterExpr{kind: ExprIsNotNull, left: e} }

// Eval evaluates the expression against a tuple. Returns Null on type errors.
func (e *FilterExpr) Eval(t *Tuple, s Schema) Value {
	switch e.kind {
	case ExprLiteral:
		return e.literal
	case ExprColumn:
		idx := s.ColIndex(e.table, e.col)
		if idx < 0 || idx >= len(t.Values) {
			return Null
		}
		return t.Values[idx]
	case ExprIsNull:
		return BoolVal(e.left.Eval(t, s).IsNull())
	case ExprIsNotNull:
		return BoolVal(!e.left.Eval(t, s).IsNull())
	case ExprUnaryNot:
		v := e.left.Eval(t, s)
		if v.IsNull() {
			return Null
		}
		return BoolVal(!v.ToBool())
	case ExprBinOp:
		return e.evalBinOp(t, s)
	}
	return Null
}

func (e *FilterExpr) evalBinOp(t *Tuple, s Schema) Value {
	// AND/OR use short-circuit three-valued logic before evaluating RHS.
	if e.op == BinAnd {
		l := e.left.Eval(t, s)
		if !l.IsNull() && !l.ToBool() {
			return BoolVal(false) // FALSE AND anything = FALSE
		}
		r := e.right.Eval(t, s)
		if l.IsNull() || r.IsNull() {
			if r.IsNull() || !r.ToBool() {
				if r.IsNull() {
					return Null
				}
				return BoolVal(false)
			}
			return Null
		}
		return BoolVal(l.ToBool() && r.ToBool())
	}
	if e.op == BinOr {
		l := e.left.Eval(t, s)
		if !l.IsNull() && l.ToBool() {
			return BoolVal(true) // TRUE OR anything = TRUE
		}
		r := e.right.Eval(t, s)
		if l.IsNull() || r.IsNull() {
			if !l.IsNull() && !l.ToBool() {
				return Null // FALSE OR NULL = NULL
			}
			if !r.IsNull() && r.ToBool() {
				return BoolVal(true)
			}
			return Null
		}
		return BoolVal(l.ToBool() || r.ToBool())
	}

	lv := e.left.Eval(t, s)
	rv := e.right.Eval(t, s)

	// Any comparison with NULL yields NULL (three-valued logic).
	if lv.IsNull() || rv.IsNull() {
		return Null
	}

	if e.op == BinLike {
		ls, lok := lv.AsString()
		rs, rok := rv.AsString()
		if !lok || !rok {
			return Null
		}
		return BoolVal(likeMatch(ls, rs))
	}

	cmp, ok := CompareValues(lv, rv)
	if !ok {
		return Null
	}
	switch e.op {
	case BinEq:
		return BoolVal(cmp == 0)
	case BinNeq:
		return BoolVal(cmp != 0)
	case BinLt:
		return BoolVal(cmp < 0)
	case BinLte:
		return BoolVal(cmp <= 0)
	case BinGt:
		return BoolVal(cmp > 0)
	case BinGte:
		return BoolVal(cmp >= 0)
	}
	return Null
}

// likeMatch implements SQL LIKE: % matches any sequence, _ matches one character.
func likeMatch(text, pattern string) bool {
	t, p := []rune(text), []rune(pattern)
	ti, pi := 0, 0
	starIdx, match := -1, 0
	for ti < len(t) {
		if pi < len(p) && (p[pi] == '_' || p[pi] == t[ti]) {
			ti++
			pi++
		} else if pi < len(p) && p[pi] == '%' {
			starIdx = pi
			match = ti
			pi++
		} else if starIdx != -1 {
			pi = starIdx + 1
			match++
			ti = match
		} else {
			return false
		}
	}
	for pi < len(p) && p[pi] == '%' {
		pi++
	}
	return pi == len(p)
}

// ReferencedTables returns the set of table names referenced in the expression.
func (e *FilterExpr) ReferencedTables() map[string]bool {
	tables := make(map[string]bool)
	e.collectTables(tables)
	return tables
}

func (e *FilterExpr) collectTables(out map[string]bool) {
	if e == nil {
		return
	}
	if e.kind == ExprColumn && e.table != "" {
		out[e.table] = true
	}
	e.left.collectTables(out)
	e.right.collectTables(out)
}

// SplitConjuncts splits an AND tree into its conjuncts.
func SplitConjuncts(e *FilterExpr) []*FilterExpr {
	if e == nil {
		return nil
	}
	if e.kind == ExprBinOp && e.op == BinAnd {
		return append(SplitConjuncts(e.left), SplitConjuncts(e.right)...)
	}
	return []*FilterExpr{e}
}

// JoinConjuncts re-assembles a list of predicates with AND.
func JoinConjuncts(exprs []*FilterExpr) *FilterExpr {
	if len(exprs) == 0 {
		return nil
	}
	result := exprs[0]
	for _, e := range exprs[1:] {
		result = Binop(BinAnd, result, e)
	}
	return result
}

// FilterOperator wraps a child and yields only tuples where predicate is true.
type FilterOperator struct {
	child  Operator
	pred   *FilterExpr
	schema Schema
}

func NewFilter(child Operator, pred *FilterExpr) *FilterOperator {
	return &FilterOperator{child: child, pred: pred, schema: child.Schema()}
}

func (f *FilterOperator) Init() error    { return f.child.Init() }
func (f *FilterOperator) Close() error   { return f.child.Close() }
func (f *FilterOperator) Schema() Schema { return f.schema }

func (f *FilterOperator) Next() (*Tuple, error) {
	for {
		t, err := f.child.Next()
		if err != nil || t == nil {
			return t, err
		}
		if f.pred.Eval(t, f.schema).ToBool() {
			return t, nil
		}
	}
}

// ProjectionOperator selects and renames columns.
type ProjectionOperator struct {
	child   Operator
	cols    []string
	indices []int
	schema  Schema
}

func NewProjection(child Operator, cols []string) (*ProjectionOperator, error) {
	childSchema := child.Schema()
	indices := make([]int, len(cols))
	outSchema := make(Schema, len(cols))
	for i, name := range cols {
		// Allow "table.col" syntax.
		table := ""
		col := name
		if dot := strings.IndexByte(name, '.'); dot >= 0 {
			table = name[:dot]
			col = name[dot+1:]
		}
		idx := childSchema.ColIndex(table, col)
		if idx < 0 {
			return nil, &ColumnNotFoundError{Table: table, Name: col}
		}
		indices[i] = idx
		outSchema[i] = ColumnDef{Name: col, Table: table, Kind: childSchema[idx].Kind}
	}
	return &ProjectionOperator{
		child:   child,
		cols:    cols,
		indices: indices,
		schema:  outSchema,
	}, nil
}

func (p *ProjectionOperator) Init() error    { return p.child.Init() }
func (p *ProjectionOperator) Close() error   { return p.child.Close() }
func (p *ProjectionOperator) Schema() Schema { return p.schema }

func (p *ProjectionOperator) Next() (*Tuple, error) {
	t, err := p.child.Next()
	if err != nil || t == nil {
		return t, err
	}
	vals := make([]Value, len(p.indices))
	for i, idx := range p.indices {
		vals[i] = t.Values[idx]
	}
	return &Tuple{Values: vals}, nil
}

// ColumnNotFoundError is returned when a column reference cannot be resolved.
type ColumnNotFoundError struct {
	Table string
	Name  string
}

func (e *ColumnNotFoundError) Error() string {
	if e.Table != "" {
		return "column not found: " + e.Table + "." + e.Name
	}
	return "column not found: " + e.Name
}

// ErrColumnNotFound wraps ColumnNotFoundError for errors.Is checks.
var ErrColumnNotFound = errors.New("column not found")

func (e *ColumnNotFoundError) Unwrap() error { return ErrColumnNotFound }
```

### The runnable demo

The demo shows three things the evaluator must get right: `age > 20` drops the NULL-age row (UNKNOWN is not TRUE), a projection narrows each tuple to one column, and `LIKE 'a%'` matches only names beginning with `a`.

Create `cmd/demo/main.go`:

```go
package main

import (
	"fmt"
	"log"

	planner "example.com/filter-and-projection"
)

func main() {
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
			{Values: []planner.Value{planner.IntVal(4), planner.StrVal("dave"), planner.Null}},
		},
		Indexes: make(map[string]*planner.IndexDef),
	}

	// age > 20 excludes dave (age IS NULL) by three-valued logic.
	pred := planner.Binop(planner.BinGt, planner.Col("users", "age"), planner.Literal(planner.IntVal(20)))
	rows, err := planner.Collect(planner.NewFilter(planner.NewSeqScan(users, nil), pred))
	if err != nil {
		log.Fatal(err)
	}
	fmt.Printf("age > 20 keeps %d of 4 rows:\n", len(rows))
	for _, r := range rows {
		fmt.Printf("  %s age=%s\n", r.Values[1].String(), r.Values[2].String())
	}

	// Project just the name column.
	proj, err := planner.NewProjection(planner.NewSeqScan(users, nil), []string{"name"})
	if err != nil {
		log.Fatal(err)
	}
	projRows, err := planner.Collect(proj)
	if err != nil {
		log.Fatal(err)
	}
	names := make([]string, 0, len(projRows))
	for _, r := range projRows {
		names = append(names, r.Values[0].String())
	}
	fmt.Printf("names: %v\n", names)

	// LIKE 'a%' matches only names starting with a.
	like := planner.Binop(planner.BinLike, planner.Col("users", "name"), planner.Literal(planner.StrVal("a%")))
	likeRows, err := planner.Collect(planner.NewFilter(planner.NewSeqScan(users, nil), like))
	if err != nil {
		log.Fatal(err)
	}
	matched := make([]string, 0, len(likeRows))
	for _, r := range likeRows {
		matched = append(matched, r.Values[1].String())
	}
	fmt.Printf("names LIKE 'a%%': %v\n", matched)
}
```

Run it:

```bash
go run ./cmd/demo
```

Expected output:

```
age > 20 keeps 3 of 4 rows:
  alice age=30
  bob age=25
  carol age=30
names: [alice bob carol dave]
names LIKE 'a%': [alice]
```

### Tests

The tests pin the three-valued behavior the evaluator exists for: a comparison against NULL drops the row, `IS NULL` and `IS NOT NULL` partition the table, projection narrows the tuple width, an unknown projected column reports the sentinel, and `LIKE` honors `%` and `_`.

Create `filter_test.go`:

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

func TestFilterOperatorNullHandling(t *testing.T) {
	t.Parallel()

	// age > 20 should exclude the row where age IS NULL (three-valued logic).
	pred := Binop(BinGt, Col("users", "age"), Literal(IntVal(20)))
	op := NewFilter(NewSeqScan(makeUsersTable(), nil), pred)
	rows, err := Collect(op)
	if err != nil {
		t.Fatal(err)
	}
	if len(rows) != 3 {
		t.Fatalf("got %d rows, want 3 (NULL row must be excluded)", len(rows))
	}
}

func TestFilterIsNullAndIsNotNull(t *testing.T) {
	t.Parallel()

	rows, err := Collect(NewFilter(NewSeqScan(makeUsersTable(), nil), IsNull(Col("users", "age"))))
	if err != nil {
		t.Fatal(err)
	}
	if len(rows) != 1 {
		t.Fatalf("IS NULL: got %d rows, want 1", len(rows))
	}
	if rows[0].Values[1].String() != "dave" {
		t.Fatalf("IS NULL row name = %q, want dave", rows[0].Values[1].String())
	}

	rows2, err := Collect(NewFilter(NewSeqScan(makeUsersTable(), nil), IsNotNull(Col("users", "age"))))
	if err != nil {
		t.Fatal(err)
	}
	if len(rows2) != 3 {
		t.Fatalf("IS NOT NULL: got %d rows, want 3", len(rows2))
	}
}

func TestProjection(t *testing.T) {
	t.Parallel()

	proj, err := NewProjection(NewSeqScan(makeUsersTable(), nil), []string{"name"})
	if err != nil {
		t.Fatal(err)
	}
	rows, err := Collect(proj)
	if err != nil {
		t.Fatal(err)
	}
	if len(rows) != 4 {
		t.Fatalf("projection rows: got %d, want 4", len(rows))
	}
	for _, r := range rows {
		if len(r.Values) != 1 {
			t.Fatalf("projected tuple width = %d, want 1", len(r.Values))
		}
	}
}

func TestProjectionUnknownColumnError(t *testing.T) {
	t.Parallel()

	_, err := NewProjection(NewSeqScan(makeUsersTable(), nil), []string{"nonexistent"})
	if err == nil {
		t.Fatal("expected error for unknown column")
	}
	if !errors.Is(err, ErrColumnNotFound) {
		t.Fatalf("err = %v, want ErrColumnNotFound", err)
	}
}

func TestLikeMatching(t *testing.T) {
	t.Parallel()

	cases := []struct {
		text    string
		pattern string
		want    bool
	}{
		{"alice", "al%", true},
		{"alice", "al_ce", true},
		{"alice", "%ce", true},
		{"alice", "%z%", false},
		{"", "%", true},
		{"alice", "alice", true},
		{"alice", "ALICE", false},
	}
	for _, tc := range cases {
		if got := likeMatch(tc.text, tc.pattern); got != tc.want {
			t.Errorf("likeMatch(%q,%q)=%v, want %v", tc.text, tc.pattern, got, tc.want)
		}
	}
}

func ExampleFilterOperator() {
	schema := Schema{
		{Name: "id", Table: "t", Kind: KindInt},
	}
	rows := []*Tuple{
		{Values: []Value{IntVal(1)}},
		{Values: []Value{IntVal(2)}},
		{Values: []Value{IntVal(3)}},
	}
	td := &TableDef{Name: "t", Columns: schema, Rows: rows, Indexes: make(map[string]*IndexDef)}
	pred := Binop(BinGt, Col("t", "id"), Literal(IntVal(1)))
	op := NewFilter(NewSeqScan(td, nil), pred)
	rows2, _ := Collect(op)
	fmt.Println(len(rows2))
	// Output: 2
}
```

## Review

The evaluator is correct when UNKNOWN is a distinct outcome from FALSE, not a synonym for it. `age > 20` over a NULL age must yield NULL and the row must vanish; `NULL AND TRUE` must be NULL while `NULL AND FALSE` is FALSE; `NULL OR TRUE` must be TRUE while `FALSE OR NULL` is NULL. The filter passes a row only on a genuine TRUE via `ToBool`, and the projection resolves names to indices once in its constructor so per-row work is a slice copy. The unknown-column path is an error, not a silent empty result, and it unwraps to `ErrColumnNotFound` so the caller can branch on it.

## Resources

- [Modern SQL: Three-Valued Logic](https://modern-sql.com/concept/three-valued-logic) — the AND/OR/NOT and comparison truth tables `evalBinOp` implements.
- [CMU 15-445 Query Execution](https://15445.courses.cs.cmu.edu/fall2024/slides/) — filter and projection as pipelined operators.
- [pkg.go.dev/strings](https://pkg.go.dev/strings) — `strings.IndexByte`, used to split `table.col` in the projection.

---

Back to [03-catalog-and-sequential-scan.md](03-catalog-and-sequential-scan.md) | Next: [05-sort-limit-and-joins.md](05-sort-limit-and-joins.md)
