# Exercise 6: Group-By and Aggregation

Aggregation collapses many input rows into one row per group. This exercise builds `GroupByOperator`, a blocking operator that partitions its input by a set of grouping columns and computes `COUNT`, `SUM`, `AVG`, `MIN`, and `MAX` per group, with an optional `HAVING` predicate that filters the computed groups. Like sort and the hash-join build phase, it is a pipeline-breaker: `Init` must consume the entire input before the first group can be emitted, because any later row might belong to an existing group or open a new one.

This module is fully self-contained. It depends only on the standard library, ships its own demo and tests, and imports no other exercise: it carries its own copies of the value, operator, catalog, filter, and scan scaffolding the aggregator builds on.

## What you'll build

```text
group-by-aggregation/
  go.mod
  value.go           Value, Kind, CompareValues
  operator.go        Schema, Tuple, Operator interface
  catalog.go         TableDef, IndexDef, Catalog
  filter.go          FilterExpr (used by HAVING), FilterOperator
  scan.go            SeqScanOperator, Collect
  aggregate.go       AggFunc, AggSpec, GroupByOperator
  cmd/
    demo/
      main.go        GROUP BY dept with COUNT/AVG/MIN/MAX, then a HAVING filter
  aggregate_test.go  count+avg, having, min/max correctness
```

- Files: `value.go`, `operator.go`, `catalog.go`, `filter.go`, `scan.go`, `aggregate.go`, `cmd/demo/main.go`, `aggregate_test.go`.
- Implement: `AggFunc`, `AggSpec`, `GroupByOperator`/`NewGroupBy`; the value/operator/catalog/filter/scan files are reused scaffolding.
- Test: `COUNT(*)` and `AVG` across three groups, a `HAVING COUNT(*) > 1` that keeps a single group, and `MIN`/`MAX` over a group's values.
- Verify: `go test -race ./...`

### How grouping works

`GroupByOperator.Init` reads every input row, derives a group key by concatenating the string forms of the grouping columns, and looks the key up in a map of per-group accumulators. The first time a key appears, the operator allocates fresh aggregate state and records the key in an `order` slice; that slice is what makes the output deterministic — groups are emitted in first-seen order rather than in Go's randomized map-iteration order. Each row then updates every aggregate's running state: `COUNT(*)` increments unconditionally, `COUNT(col)`/`SUM`/`AVG` skip NULL inputs (SQL aggregates ignore NULLs), and `MIN`/`MAX` track the extreme non-NULL value seen.

When the input is exhausted, the operator materializes one output row per group: the grouping-column values followed by each aggregate's final value. `AVG` over zero non-NULL inputs and `MIN`/`MAX`/`SUM` over an all-NULL group yield NULL, matching SQL. The optional `HAVING` predicate is evaluated against the assembled group row — note it runs after aggregation, against the output schema, so it can reference an aggregate output column by name (for example `cnt`).

### The reused scaffolding

The value, operator, catalog, filter, and scan files are reproduced so the module stands alone. `filter.go` is here because `HAVING` is a `FilterExpr` evaluated against each group row.

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

Create `filter.go`:

```go
package planner

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
```

Create `scan.go`:

```go
package planner

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

### The aggregator

`AggSpec` names one output aggregate: the function, the input column index (`-1` for `COUNT(*)`), and the output column name. `NewGroupBy` builds the output schema (grouping columns, then one column per aggregate) and validates the grouping indices. The per-group `aggState` carries a running count, sum, current min, current max, and a `hasValue` flag that distinguishes "seen at least one non-NULL" from "empty," which is what lets `MIN`/`MAX`/`SUM` return NULL for an all-NULL group while `COUNT` still returns 0.

Create `aggregate.go`:

```go
package planner

import (
	"errors"
	"fmt"
)

// AggFunc names an aggregate function.
type AggFunc uint8

const (
	AggCount AggFunc = iota // COUNT(*) or COUNT(col)
	AggSum
	AggAvg
	AggMin
	AggMax
)

// AggSpec describes one aggregation output column.
type AggSpec struct {
	Fn       AggFunc
	ColIndex int // -1 means COUNT(*)
	OutName  string
}

// GroupByOperator partitions input by grouping columns and computes aggregates.
// It is a blocking operator: Init reads all input rows.
type GroupByOperator struct {
	child    Operator
	groupIdx []int // column indices for GROUP BY
	aggs     []AggSpec
	having   *FilterExpr
	schema   Schema
	results  []*Tuple
	pos      int
}

// ErrUnexpectedAgg is returned when an unknown aggregate function is requested.
var ErrUnexpectedAgg = errors.New("unexpected aggregate function")

func NewGroupBy(child Operator, groupIdx []int, aggs []AggSpec, having *FilterExpr) (*GroupByOperator, error) {
	childSchema := child.Schema()
	outCols := make(Schema, 0, len(groupIdx)+len(aggs))
	for _, gi := range groupIdx {
		if gi < 0 || gi >= len(childSchema) {
			return nil, fmt.Errorf("group column index %d out of range", gi)
		}
		outCols = append(outCols, childSchema[gi])
	}
	for _, a := range aggs {
		outCols = append(outCols, ColumnDef{Name: a.OutName, Kind: KindFloat})
	}
	return &GroupByOperator{
		child:    child,
		groupIdx: groupIdx,
		aggs:     aggs,
		having:   having,
		schema:   outCols,
	}, nil
}

func (g *GroupByOperator) Schema() Schema { return g.schema }
func (g *GroupByOperator) Close() error   { g.results = nil; return g.child.Close() }

type aggState struct {
	count    int64
	sum      float64
	min      Value
	max      Value
	hasValue bool
}

func (g *GroupByOperator) Init() error {
	if err := g.child.Init(); err != nil {
		return err
	}

	// Use a string key derived from group-column values.
	type entry struct {
		key     string
		keyVals []Value
		states  []aggState
	}
	order := make([]string, 0)
	groups := make(map[string]*entry)

	for {
		t, err := g.child.Next()
		if err != nil {
			return err
		}
		if t == nil {
			break
		}
		// Build group key.
		var kb []byte
		keyVals := make([]Value, len(g.groupIdx))
		for i, gi := range g.groupIdx {
			v := t.Values[gi]
			keyVals[i] = v
			kb = append(kb, []byte(v.String()+"\x00")...)
		}
		k := string(kb)

		e, ok := groups[k]
		if !ok {
			e = &entry{key: k, keyVals: keyVals, states: make([]aggState, len(g.aggs))}
			groups[k] = e
			order = append(order, k)
		}
		// Update each aggregate.
		for i, a := range g.aggs {
			st := &e.states[i]
			var v Value
			if a.ColIndex >= 0 && a.ColIndex < len(t.Values) {
				v = t.Values[a.ColIndex]
			}
			switch a.Fn {
			case AggCount:
				if a.ColIndex < 0 || !v.IsNull() {
					st.count++
				}
			case AggSum:
				if !v.IsNull() {
					st.sum += toFloat(v)
					st.hasValue = true
				}
			case AggAvg:
				if !v.IsNull() {
					st.sum += toFloat(v)
					st.count++
					st.hasValue = true
				}
			case AggMin:
				if !v.IsNull() {
					if !st.hasValue {
						st.min = v
						st.hasValue = true
					} else {
						cmp, ok := CompareValues(v, st.min)
						if ok && cmp < 0 {
							st.min = v
						}
					}
				}
			case AggMax:
				if !v.IsNull() {
					if !st.hasValue {
						st.max = v
						st.hasValue = true
					} else {
						cmp, ok := CompareValues(v, st.max)
						if ok && cmp > 0 {
							st.max = v
						}
					}
				}
			}
		}
	}

	// Emit one row per group.
	g.results = g.results[:0]
	for _, k := range order {
		e := groups[k]
		vals := make([]Value, 0, len(g.groupIdx)+len(g.aggs))
		vals = append(vals, e.keyVals...)
		for i, a := range g.aggs {
			st := e.states[i]
			var out Value
			switch a.Fn {
			case AggCount:
				out = IntVal(st.count)
			case AggSum:
				if st.hasValue {
					out = FloatVal(st.sum)
				} else {
					out = Null
				}
			case AggAvg:
				if st.count > 0 {
					out = FloatVal(st.sum / float64(st.count))
				} else {
					out = Null
				}
			case AggMin:
				out = st.min
				if !st.hasValue {
					out = Null
				}
			case AggMax:
				out = st.max
				if !st.hasValue {
					out = Null
				}
			}
			vals = append(vals, out)
		}
		row := &Tuple{Values: vals}
		if g.having == nil || g.having.Eval(row, g.schema).ToBool() {
			g.results = append(g.results, row)
		}
	}
	g.pos = 0
	return nil
}

func (g *GroupByOperator) Next() (*Tuple, error) {
	if g.pos >= len(g.results) {
		return nil, nil
	}
	t := g.results[g.pos]
	g.pos++
	return t, nil
}

func toFloat(v Value) float64 {
	if v.kind == KindInt {
		return float64(v.ival)
	}
	if v.kind == KindFloat {
		return v.fval
	}
	return 0
}
```

### The runnable demo

The demo groups four users by department, computing the count, average salary, and salary range per department, then re-runs the grouping under a `HAVING COUNT(*) > 1` filter. Groups print in first-seen order: `eng` (alice arrives first), then `mktg`.

Create `cmd/demo/main.go`:

```go
package main

import (
	"fmt"
	"log"

	planner "example.com/group-by-aggregation"
)

func main() {
	users := &planner.TableDef{
		Name: "users",
		Columns: planner.Schema{
			{Name: "id", Table: "users", Kind: planner.KindInt},
			{Name: "name", Table: "users", Kind: planner.KindString},
			{Name: "dept", Table: "users", Kind: planner.KindString},
			{Name: "salary", Table: "users", Kind: planner.KindInt},
		},
		Rows: []*planner.Tuple{
			{Values: []planner.Value{planner.IntVal(1), planner.StrVal("alice"), planner.StrVal("eng"), planner.IntVal(100)}},
			{Values: []planner.Value{planner.IntVal(2), planner.StrVal("bob"), planner.StrVal("mktg"), planner.IntVal(80)}},
			{Values: []planner.Value{planner.IntVal(3), planner.StrVal("carol"), planner.StrVal("eng"), planner.IntVal(140)}},
			{Values: []planner.Value{planner.IntVal(4), planner.StrVal("dave"), planner.StrVal("eng"), planner.IntVal(90)}},
		},
		Indexes: make(map[string]*planner.IndexDef),
	}

	// GROUP BY dept: COUNT(*), AVG(salary), MIN(salary), MAX(salary).
	gb, err := planner.NewGroupBy(
		planner.NewSeqScan(users, nil),
		[]int{2},
		[]planner.AggSpec{
			{Fn: planner.AggCount, ColIndex: -1, OutName: "cnt"},
			{Fn: planner.AggAvg, ColIndex: 3, OutName: "avg_salary"},
			{Fn: planner.AggMin, ColIndex: 3, OutName: "min_salary"},
			{Fn: planner.AggMax, ColIndex: 3, OutName: "max_salary"},
		},
		nil,
	)
	if err != nil {
		log.Fatal(err)
	}
	rows, err := planner.Collect(gb)
	if err != nil {
		log.Fatal(err)
	}
	fmt.Println("dept aggregates:")
	for _, r := range rows {
		fmt.Printf("  dept=%s cnt=%s avg=%s min=%s max=%s\n",
			r.Values[0].String(), r.Values[1].String(), r.Values[2].String(),
			r.Values[3].String(), r.Values[4].String())
	}

	// HAVING COUNT(*) > 1.
	gb2, err := planner.NewGroupBy(
		planner.NewSeqScan(users, nil),
		[]int{2},
		[]planner.AggSpec{{Fn: planner.AggCount, ColIndex: -1, OutName: "cnt"}},
		planner.Binop(planner.BinGt, planner.Col("", "cnt"), planner.Literal(planner.IntVal(1))),
	)
	if err != nil {
		log.Fatal(err)
	}
	rows2, err := planner.Collect(gb2)
	if err != nil {
		log.Fatal(err)
	}
	fmt.Println("depts with more than one member:")
	for _, r := range rows2 {
		fmt.Printf("  dept=%s cnt=%s\n", r.Values[0].String(), r.Values[1].String())
	}
}
```

Run it:

```bash
go run ./cmd/demo
```

Expected output:

```
dept aggregates:
  dept=eng cnt=3 avg=110 min=90 max=140
  dept=mktg cnt=1 avg=80 min=80 max=80
depts with more than one member:
  dept=eng cnt=3
```

### Tests

The tests group by age (which carries a NULL for `dave`) to pin three behaviors: `COUNT(*)` and `AVG` across the three age groups, a `HAVING COUNT(*) > 1` that keeps only the two-member group, and `MIN`/`MAX` over a group with more than one row.

Create `aggregate_test.go`:

```go
package planner

import (
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

func TestGroupByCountAndAvg(t *testing.T) {
	t.Parallel()

	users := makeUsersTable()
	// GROUP BY age, compute COUNT(*) and AVG(id).
	// age is at index 2; id is at index 0.
	gb, err := NewGroupBy(
		NewSeqScan(users, nil),
		[]int{2},
		[]AggSpec{
			{Fn: AggCount, ColIndex: -1, OutName: "cnt"}, // COUNT(*)
			{Fn: AggAvg, ColIndex: 0, OutName: "avg_id"}, // AVG(id)
		},
		nil,
	)
	if err != nil {
		t.Fatal(err)
	}
	rows, err := Collect(gb)
	if err != nil {
		t.Fatal(err)
	}
	// Groups: 30->alice+carol(2), 25->bob(1), NULL->dave(1).
	if len(rows) != 3 {
		t.Fatalf("group by: got %d groups, want 3", len(rows))
	}
}

func TestGroupByHaving(t *testing.T) {
	t.Parallel()

	users := makeUsersTable()
	// HAVING COUNT(*) > 1 should return only the age=30 group.
	gb, err := NewGroupBy(
		NewSeqScan(users, nil),
		[]int{2},
		[]AggSpec{
			{Fn: AggCount, ColIndex: -1, OutName: "cnt"},
		},
		Binop(BinGt, Col("", "cnt"), Literal(IntVal(1))),
	)
	if err != nil {
		t.Fatal(err)
	}
	rows, err := Collect(gb)
	if err != nil {
		t.Fatal(err)
	}
	if len(rows) != 1 {
		t.Fatalf("having: got %d groups, want 1", len(rows))
	}
	cnt, _ := rows[0].Values[1].AsInt()
	if cnt != 2 {
		t.Fatalf("having group cnt=%d, want 2", cnt)
	}
}

func TestGroupByMinMax(t *testing.T) {
	t.Parallel()

	users := makeUsersTable()
	// GROUP BY age, compute MIN(id) and MAX(id). The first group emitted is
	// age=30 (alice is seen first): its members are alice(id 1) and carol(id 3).
	gb, err := NewGroupBy(
		NewSeqScan(users, nil),
		[]int{2},
		[]AggSpec{
			{Fn: AggMin, ColIndex: 0, OutName: "min_id"},
			{Fn: AggMax, ColIndex: 0, OutName: "max_id"},
		},
		nil,
	)
	if err != nil {
		t.Fatal(err)
	}
	rows, err := Collect(gb)
	if err != nil {
		t.Fatal(err)
	}
	if len(rows) != 3 {
		t.Fatalf("group by: got %d groups, want 3", len(rows))
	}
	minID, _ := rows[0].Values[1].AsInt()
	maxID, _ := rows[0].Values[2].AsInt()
	if minID != 1 || maxID != 3 {
		t.Fatalf("age=30 group: min=%d max=%d, want 1 and 3", minID, maxID)
	}
}

func ExampleGroupByOperator() {
	schema := Schema{
		{Name: "dept", Table: "t", Kind: KindString},
	}
	rows := []*Tuple{
		{Values: []Value{StrVal("eng")}},
		{Values: []Value{StrVal("eng")}},
		{Values: []Value{StrVal("mktg")}},
	}
	td := &TableDef{Name: "t", Columns: schema, Rows: rows, Indexes: make(map[string]*IndexDef)}
	gb, _ := NewGroupBy(NewSeqScan(td, nil), []int{0}, []AggSpec{{Fn: AggCount, ColIndex: -1, OutName: "cnt"}}, nil)
	out, _ := Collect(gb)
	for _, r := range out {
		fmt.Printf("%s=%s\n", r.Values[0].String(), r.Values[1].String())
	}
	// Output:
	// eng=2
	// mktg=1
}
```

## Review

Grouping is correct when groups are emitted deterministically in first-seen order, when SQL's NULL rules hold (NULLs are ignored by `COUNT(col)`, `SUM`, `AVG`, `MIN`, and `MAX`, while `COUNT(*)` counts every row and an all-NULL group yields NULL for the value aggregates but 0 for the count), and when `HAVING` runs after aggregation against the group row so it can reference an aggregate output column by name. `TestGroupByHaving` confirms the single surviving group has the count it should, and `TestGroupByMinMax` confirms the extremes are tracked across a multi-row group. Run the suite under `go test -race ./...` and confirm the demo's averages and ranges match the expected output exactly.

## Resources

- [CMU 15-445 Query Execution](https://15445.courses.cs.cmu.edu/fall2024/slides/) — hash aggregation and the GROUP BY operator.
- [Modern SQL: Three-Valued Logic](https://modern-sql.com/concept/three-valued-logic) — why aggregates ignore NULL and how HAVING differs from WHERE.
- [pkg.go.dev/sort](https://pkg.go.dev/sort) — ordering utilities used elsewhere in the engine.

---

Back to [05-sort-limit-and-joins.md](05-sort-limit-and-joins.md) | Next: [07-cost-based-planner.md](07-cost-based-planner.md)
