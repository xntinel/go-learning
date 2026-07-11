# Exercise 7: Cost-Based Planner with Predicate Pushdown

A planner is the component that turns a logical plan into a runnable operator tree, choosing concrete operators using catalog statistics. This module builds that planner end to end: a typed value with three-valued NULL semantics, the volcano operator interface, sequential and index scans backed by an in-memory catalog, an expression evaluator with a filter and a projection operator, the sort/limit/join operators, and finally the `Planner` itself, which lowers `LogicalScan`/`LogicalFilter`/`LogicalJoin` nodes into physical operators while making two cost-driven decisions: index scan versus sequential scan for a predicate, and hash join versus nested-loop join for an equi-join. Along the way it performs predicate pushdown, splitting a conjunctive WHERE clause and routing each single-table conjunct down to the matching scan.

This module is fully self-contained. It depends only on the standard library, ships its own demo and tests, and imports no other exercise.

## What you'll build

```text
value.go          Value, Kind, CompareValues, three-valued ToBool
operator.go       ColumnDef, Schema, Tuple, the Operator interface
catalog.go        Catalog, TableDef, IndexDef, EstimatedRows
scan.go           SeqScanOperator, IndexScanOperator
filter.go         FilterExpr, Eval, FilterOperator, ProjectionOperator
join.go           Sort, Limit, NestedLoopJoin, HashJoin, Explain
plan.go           Planner, LogicalScan/Filter/Join, predicate pushdown, Collect
cmd/
  demo/
    main.go       build plans and print Explain trees
planner_test.go   index-scan selection, predicate pushdown, pushed filter
```

- Files: `value.go`, `operator.go`, `catalog.go`, `scan.go`, `filter.go`, `join.go`, `plan.go`, `cmd/demo/main.go`, `planner_test.go`.
- Implement: `Planner`, `NewPlanner`, `Build`, `buildScan`/`tryIndexScan`/`buildJoin`, `extractEquiKey`, `SplitConjuncts`/`JoinConjuncts`/`onlyReferences`, `Explain`, and `Collect`.
- Test: the planner picks `IndexScanOperator` for an equality predicate on an indexed column; predicate pushdown moves a single-table conjunct to the scan side of a join; a `SeqScanOperator` with a pushed-down filter discards rows during the scan.
- Verify: `go test -race ./...`

Set up the module:

```bash
mkdir -p cost-based-planner/cmd/demo && cd cost-based-planner
go mod init example.com/cost-based-planner
```

### Typed values and three-valued logic

Every column can be NULL, so a runtime value carries its own type tag and nullability. Arithmetic or comparison touching a NULL yields NULL, and `CompareValues` defines a deterministic total order (NULL sorts before every concrete value) that sort and merge depend on. `ToBool` collapses a value to a Go bool for control flow, mapping NULL to false, but the evaluator never relies on that alone for AND and OR.

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

### Schemas, tuples, and the operator interface

A schema is an ordered list of column definitions; a tuple is a row of values in schema order. Operators advertise their schema so the planner can resolve `users.id` to an index at plan time. The `Operator` interface is the volcano contract: `Init` opens, `Next` yields one tuple (or nil for end-of-stream), `Close` releases, `Schema` reports columns.

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

The catalog holds table definitions, their rows, and any single-column indexes. `EstimatedRows` is the only statistic the cost model needs here.

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

### Scans

`SeqScanOperator` reads every row and optionally applies a pushed-down predicate, discarding non-matching rows during the scan. It clones each row so operators that retain tuples do not alias into the table's backing store. `IndexScanOperator` turns an equality predicate on an indexed column into a point lookup.

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
```

### Expression evaluator, filter, and projection

`FilterExpr` is an expression tree the evaluator walks. AND and OR use short-circuit three-valued logic before evaluating the right side; any comparison with NULL yields NULL. `SplitConjuncts`/`JoinConjuncts` and `ReferencedTables` are the machinery predicate pushdown uses to decide where a conjunct may live.

Create `filter.go`:

```go
package planner

import (
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
	// Recursive matcher: dp[i] matches text[:i] against pattern[:j].
	// Use iterative approach for correctness.
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
	cols    []string // output column names in order
	indices []int    // index into child schema for each output column
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
```

### Sort, limit, and the join operators

`SortOperator` materializes and sorts its input (a pipeline-breaker); `LimitOperator` streams at most `limit` rows after skipping `offset`. `NestedLoopJoinOperator` rescans the inner relation per outer row and is the only choice for non-equi conditions; it emits unmatched outer rows NULL-padded for a LEFT join. `HashJoinOperator` builds a hash table on the build side and probes it, never inserting or probing a NULL key (because `NULL = NULL` is unknown), and preserves the build side for LEFT joins by emitting unmatched build rows NULL-padded after the probe drains. `Explain` renders the physical tree so a test can assert plan shape, not just result rows.

Create `join.go`:

```go
package planner

import (
	"fmt"
	"hash/fnv"
	"sort"
)

// SortKey specifies one column in an ORDER BY clause.
type SortKey struct {
	ColIndex int
	Desc     bool
}

// SortOperator materializes all input rows and yields them in sorted order.
// It is a blocking operator: Next blocks until the first call after Init.
type SortOperator struct {
	child  Operator
	keys   []SortKey
	rows   []*Tuple
	pos    int
	schema Schema
}

func NewSort(child Operator, keys []SortKey) *SortOperator {
	return &SortOperator{child: child, keys: keys, schema: child.Schema()}
}

func (s *SortOperator) Schema() Schema { return s.schema }
func (s *SortOperator) Close() error   { s.rows = nil; return s.child.Close() }

func (s *SortOperator) Init() error {
	if err := s.child.Init(); err != nil {
		return err
	}
	s.rows = s.rows[:0]
	for {
		t, err := s.child.Next()
		if err != nil {
			return err
		}
		if t == nil {
			break
		}
		s.rows = append(s.rows, t)
	}
	sort.SliceStable(s.rows, func(i, j int) bool {
		for _, k := range s.keys {
			a := s.rows[i].Values[k.ColIndex]
			b := s.rows[j].Values[k.ColIndex]
			cmp, ok := CompareValues(a, b)
			if !ok || cmp == 0 {
				continue
			}
			if k.Desc {
				return cmp > 0
			}
			return cmp < 0
		}
		return false
	})
	s.pos = 0
	return nil
}

func (s *SortOperator) Next() (*Tuple, error) {
	if s.pos >= len(s.rows) {
		return nil, nil
	}
	t := s.rows[s.pos]
	s.pos++
	return t, nil
}

// LimitOperator yields at most Limit rows starting at Offset.
type LimitOperator struct {
	child   Operator
	limit   int
	offset  int
	skipped int
	emitted int
	schema  Schema
}

func NewLimit(child Operator, limit, offset int) *LimitOperator {
	return &LimitOperator{child: child, limit: limit, offset: offset, schema: child.Schema()}
}

func (l *LimitOperator) Schema() Schema { return l.schema }
func (l *LimitOperator) Close() error   { return l.child.Close() }

func (l *LimitOperator) Init() error {
	l.skipped = 0
	l.emitted = 0
	return l.child.Init()
}

func (l *LimitOperator) Next() (*Tuple, error) {
	if l.limit >= 0 && l.emitted >= l.limit {
		return nil, nil
	}
	for {
		t, err := l.child.Next()
		if err != nil || t == nil {
			return t, err
		}
		if l.skipped < l.offset {
			l.skipped++
			continue
		}
		l.emitted++
		return t, nil
	}
}

// JoinType specifies INNER, LEFT, or RIGHT join semantics.
type JoinType uint8

const (
	InnerJoin JoinType = iota
	LeftJoin
	RightJoin
)

// NestedLoopJoinOperator implements a nested-loop join.
// For each outer row, it rescans the inner child entirely.
type NestedLoopJoinOperator struct {
	outer     Operator
	inner     Operator
	cond      *FilterExpr
	joinType  JoinType
	schema    Schema
	outerRow  *Tuple
	innerDone bool
	matched   bool // did current outerRow match any inner row?
}

func NewNestedLoopJoin(outer, inner Operator, cond *FilterExpr, jt JoinType) *NestedLoopJoinOperator {
	schema := append(append(Schema(nil), outer.Schema()...), inner.Schema()...)
	return &NestedLoopJoinOperator{
		outer:    outer,
		inner:    inner,
		cond:     cond,
		joinType: jt,
		schema:   schema,
	}
}

func (n *NestedLoopJoinOperator) Schema() Schema { return n.schema }

func (n *NestedLoopJoinOperator) Init() error {
	if err := n.outer.Init(); err != nil {
		return err
	}
	if err := n.inner.Init(); err != nil {
		return err
	}
	n.outerRow = nil
	n.innerDone = true
	n.matched = false
	return nil
}

func (n *NestedLoopJoinOperator) Close() error {
	_ = n.outer.Close()
	return n.inner.Close()
}

func (n *NestedLoopJoinOperator) Next() (*Tuple, error) {
	for {
		// Advance outer when inner is exhausted.
		if n.innerDone {
			// Emit unmatched outer row for LEFT JOIN.
			if n.outerRow != nil && !n.matched && n.joinType == LeftJoin {
				t := nullPadRight(n.outerRow, len(n.inner.Schema()))
				n.outerRow = nil
				return t, nil
			}
			var err error
			n.outerRow, err = n.outer.Next()
			if err != nil || n.outerRow == nil {
				return nil, err
			}
			if err = n.inner.Init(); err != nil {
				return nil, err
			}
			n.innerDone = false
			n.matched = false
		}

		innerRow, err := n.inner.Next()
		if err != nil {
			return nil, err
		}
		if innerRow == nil {
			n.innerDone = true
			continue
		}

		combined := concatTuples(n.outerRow, innerRow)
		if n.cond == nil || n.cond.Eval(combined, n.schema).ToBool() {
			n.matched = true
			return combined, nil
		}
	}
}

// hashBuildEntry holds one materialized build-side row plus whether any probe
// row has matched it. The matched flag drives LEFT-join unmatched emission.
type hashBuildEntry struct {
	tuple   *Tuple
	matched bool
}

// HashJoinOperator implements an equi-join using a hash table on the build side.
// It is a blocking operator on the build side: Init reads all build rows.
//
// NULL join keys never produce a match. By SQL three-valued logic NULL = NULL is
// unknown, not true (the same rule the filter evaluator applies), so a NULL-keyed
// build row is never inserted into the hash table and a NULL-keyed probe row is
// never looked up. Such rows can only appear in the output as the NULL-padded side
// of an outer join, never as a match.
//
// The combined output tuple is always build || probe, so the build side is the
// "left" side of the result and the probe side is the "right" side:
//   - InnerJoin: emit matches only.
//   - LeftJoin:  preserve the build side. After the probe is exhausted, emit every
//     build row that no probe row matched, NULL-padded on the right.
//   - RightJoin: preserve the probe side. Emit every probe row that matched no
//     build row, NULL-padded on the left.
type HashJoinOperator struct {
	build     Operator
	probe     Operator
	buildKey  int // column index in build schema
	probeKey  int // column index in probe schema
	joinType  JoinType
	schema    Schema
	table     map[uint64][]*hashBuildEntry
	buildRows []*hashBuildEntry // every build row, for LEFT unmatched emission
	probeRow  *Tuple
	cands     []*hashBuildEntry
	candIdx   int
	matched   bool // did the current probe row match any build row?
	emitting  bool // LEFT-join unmatched-emission phase
	emitIdx   int
}

func NewHashJoin(build, probe Operator, buildKeyIdx, probeKeyIdx int, jt JoinType) *HashJoinOperator {
	schema := append(append(Schema(nil), build.Schema()...), probe.Schema()...)
	return &HashJoinOperator{
		build:    build,
		probe:    probe,
		buildKey: buildKeyIdx,
		probeKey: probeKeyIdx,
		joinType: jt,
		schema:   schema,
	}
}

func (h *HashJoinOperator) Schema() Schema { return h.schema }

func (h *HashJoinOperator) Init() error {
	if err := h.build.Init(); err != nil {
		return err
	}
	if err := h.probe.Init(); err != nil {
		return err
	}
	// Build phase: materialize all build-side rows into a hash table.
	h.table = make(map[uint64][]*hashBuildEntry)
	h.buildRows = h.buildRows[:0]
	for {
		t, err := h.build.Next()
		if err != nil {
			return err
		}
		if t == nil {
			break
		}
		e := &hashBuildEntry{tuple: t}
		h.buildRows = append(h.buildRows, e)
		k := t.Values[h.buildKey]
		if k.IsNull() {
			// NULL build key never matches; keep it in buildRows so a LEFT join
			// can emit it NULL-padded, but never insert it into the hash table.
			continue
		}
		h.table[hashValue(k)] = append(h.table[hashValue(k)], e)
	}
	h.probeRow = nil
	h.cands = nil
	h.candIdx = 0
	h.matched = false
	h.emitting = false
	h.emitIdx = 0
	return nil
}

func (h *HashJoinOperator) Close() error {
	h.table = nil
	h.buildRows = nil
	_ = h.build.Close()
	return h.probe.Close()
}

func (h *HashJoinOperator) Next() (*Tuple, error) {
	for {
		// LEFT-join unmatched build-row emission, after the probe is drained.
		if h.emitting {
			for h.emitIdx < len(h.buildRows) {
				e := h.buildRows[h.emitIdx]
				h.emitIdx++
				if !e.matched {
					return nullPadRight(e.tuple, len(h.probe.Schema())), nil
				}
			}
			return nil, nil
		}

		// Exhaust current candidate list for the active probe row.
		for h.candIdx < len(h.cands) {
			e := h.cands[h.candIdx]
			h.candIdx++
			// For hash join, the combined tuple is build || probe.
			e.matched = true
			h.matched = true
			return concatTuples(e.tuple, h.probeRow), nil
		}

		// Emit an unmatched probe row for RIGHT JOIN.
		if h.probeRow != nil && !h.matched && h.joinType == RightJoin {
			t := nullPadLeft(len(h.build.Schema()), h.probeRow)
			h.probeRow = nil
			return t, nil
		}

		// Advance probe side.
		var err error
		h.probeRow, err = h.probe.Next()
		if err != nil {
			return nil, err
		}
		if h.probeRow == nil {
			// Probe drained. Switch to LEFT-join unmatched emission if needed.
			if h.joinType == LeftJoin {
				h.emitting = true
				h.emitIdx = 0
				continue
			}
			return nil, nil
		}
		h.matched = false
		pk := h.probeRow.Values[h.probeKey]
		if pk.IsNull() {
			// NULL probe key matches nothing (three-valued logic).
			h.cands = nil
		} else {
			h.cands = h.table[hashValue(pk)]
		}
		h.candIdx = 0
	}
}

func hashValue(v Value) uint64 {
	h := fnv.New64a()
	switch v.kind {
	case KindInt:
		b := [8]byte{
			byte(v.ival), byte(v.ival >> 8), byte(v.ival >> 16), byte(v.ival >> 24),
			byte(v.ival >> 32), byte(v.ival >> 40), byte(v.ival >> 48), byte(v.ival >> 56),
		}
		_, _ = h.Write(b[:])
	case KindString:
		_, _ = h.Write([]byte(v.sval))
	case KindNull:
		_, _ = h.Write([]byte("__null__"))
	}
	return h.Sum64()
}

func concatTuples(a, b *Tuple) *Tuple {
	vals := make([]Value, len(a.Values)+len(b.Values))
	copy(vals, a.Values)
	copy(vals[len(a.Values):], b.Values)
	return &Tuple{Values: vals}
}

func nullPadRight(t *Tuple, n int) *Tuple {
	vals := make([]Value, len(t.Values)+n)
	copy(vals, t.Values)
	for i := len(t.Values); i < len(vals); i++ {
		vals[i] = Null
	}
	return &Tuple{Values: vals}
}

func nullPadLeft(n int, t *Tuple) *Tuple {
	vals := make([]Value, n+len(t.Values))
	for i := 0; i < n; i++ {
		vals[i] = Null
	}
	copy(vals[n:], t.Values)
	return &Tuple{Values: vals}
}

// Explain returns a readable indented description of the operator tree.
func Explain(op Operator, indent int) string {
	prefix := fmt.Sprintf("%*s", indent*2, "")
	switch o := op.(type) {
	case *SeqScanOperator:
		pred := "(none)"
		if o.filter != nil {
			pred = "(filter)"
		}
		return fmt.Sprintf("%sSeqScan[%s] pred=%s\n", prefix, o.table.Name, pred)
	case *IndexScanOperator:
		return fmt.Sprintf("%sIndexScan[%s] key=%s\n", prefix, o.index.Name, o.key.String())
	case *FilterOperator:
		return fmt.Sprintf("%sFilter\n", prefix) + Explain(o.child, indent+1)
	case *ProjectionOperator:
		return fmt.Sprintf("%sProjection%v\n", prefix, o.cols) + Explain(o.child, indent+1)
	case *SortOperator:
		return fmt.Sprintf("%sSort\n", prefix) + Explain(o.child, indent+1)
	case *LimitOperator:
		return fmt.Sprintf("%sLimit(limit=%d,offset=%d)\n", prefix, o.limit, o.offset) +
			Explain(o.child, indent+1)
	case *NestedLoopJoinOperator:
		return fmt.Sprintf("%sNestedLoopJoin[%v]\n", prefix, o.joinType) +
			Explain(o.outer, indent+1) +
			Explain(o.inner, indent+1)
	case *HashJoinOperator:
		return fmt.Sprintf("%sHashJoin[%v]\n", prefix, o.joinType) +
			Explain(o.build, indent+1) +
			Explain(o.probe, indent+1)
	default:
		return fmt.Sprintf("%s<unknown>\n", prefix)
	}
}
```

### The planner

`Planner.Build` lowers a logical plan to physical operators. A `LogicalFilter` over a scan becomes either an `IndexScanOperator` (when an equality predicate matches an index) or a `SeqScanOperator` with the predicate pushed in. A `LogicalFilter` over a `LogicalJoin` triggers predicate pushdown: each single-table conjunct is routed to the matching side, and only cross-table conjuncts remain as a residual filter above the join. For an equi-join the planner chooses a hash join, swapping build and probe to put the smaller side on the build only for inner joins.

Create `plan.go`:

```go
package planner

import (
	"errors"
	"fmt"
)

// ErrNoTable is returned when a logical plan references an unknown table.
var ErrNoTable = errors.New("table not in catalog")

// hashJoinThreshold: tables with estimated rows below this use HashJoin.
const hashJoinThreshold = 10_000

// LogicalScan is a leaf node in the logical plan.
type LogicalScan struct {
	TableName string
}

// LogicalJoin represents a binary join in the logical plan.
type LogicalJoin struct {
	Left, Right LogicalPlan
	Cond        *FilterExpr
	JoinType    JoinType
}

// LogicalFilter applies a predicate above a child plan.
type LogicalFilter struct {
	Child LogicalPlan
	Pred  *FilterExpr
}

// LogicalPlan is a logical plan node.
type LogicalPlan interface {
	logicalPlan()
}

func (*LogicalScan) logicalPlan()   {}
func (*LogicalJoin) logicalPlan()   {}
func (*LogicalFilter) logicalPlan() {}

// PlannerOptions controls cost model behavior.
type PlannerOptions struct {
	HashJoinThreshold int // row count below which we prefer HashJoin
}

// DefaultPlannerOptions returns sensible defaults.
func DefaultPlannerOptions() PlannerOptions {
	return PlannerOptions{HashJoinThreshold: hashJoinThreshold}
}

// Planner converts a logical plan into a physical operator tree.
type Planner struct {
	cat  *Catalog
	opts PlannerOptions
}

// NewPlanner creates a planner backed by cat.
func NewPlanner(cat *Catalog, opts PlannerOptions) *Planner {
	return &Planner{cat: cat, opts: opts}
}

// Build converts a logical plan to a physical operator tree.
// It applies predicate pushdown when logical.Filter wraps a logical.Join.
func (p *Planner) Build(plan LogicalPlan) (Operator, error) {
	return p.build(plan, nil)
}

func (p *Planner) build(plan LogicalPlan, pushedPred *FilterExpr) (Operator, error) {
	switch lp := plan.(type) {
	case *LogicalScan:
		return p.buildScan(lp, pushedPred)
	case *LogicalFilter:
		return p.build(lp.Child, lp.Pred)
	case *LogicalJoin:
		return p.buildJoin(lp, pushedPred)
	default:
		return nil, fmt.Errorf("unknown logical plan node %T", plan)
	}
}

func (p *Planner) buildScan(lp *LogicalScan, pred *FilterExpr) (Operator, error) {
	td, err := p.cat.Table(lp.TableName)
	if err != nil {
		return nil, fmt.Errorf("%w: %s", ErrNoTable, lp.TableName)
	}
	// Check whether an index can satisfy the predicate (point lookup only).
	if pred != nil {
		if scan, ok := p.tryIndexScan(td, pred); ok {
			return scan, nil
		}
	}
	return NewSeqScan(td, pred), nil
}

// tryIndexScan detects a simple col = literal predicate and returns an IndexScan if
// the catalog has a matching index.
func (p *Planner) tryIndexScan(td *TableDef, pred *FilterExpr) (*IndexScanOperator, bool) {
	if pred == nil || pred.kind != ExprBinOp || pred.op != BinEq {
		return nil, false
	}
	var colName string
	var key Value
	if pred.left != nil && pred.left.kind == ExprColumn && pred.right != nil && pred.right.kind == ExprLiteral {
		colName = pred.left.col
		key = pred.right.literal
	} else if pred.right != nil && pred.right.kind == ExprColumn && pred.left != nil && pred.left.kind == ExprLiteral {
		colName = pred.right.col
		key = pred.left.literal
	} else {
		return nil, false
	}
	if _, ok := td.Indexes[colName]; !ok {
		return nil, false
	}
	scan, err := NewIndexScan(td, colName, key)
	if err != nil {
		return nil, false
	}
	return scan, true
}

func (p *Planner) buildJoin(lp *LogicalJoin, pushedPred *FilterExpr) (Operator, error) {
	// Predicate pushdown: split the WHERE predicate into per-table conjuncts.
	var leftPred, rightPred, remainPred *FilterExpr
	if pushedPred != nil {
		conjuncts := SplitConjuncts(pushedPred)
		var leftConj, rightConj, remain []*FilterExpr
		for _, c := range conjuncts {
			tables := c.ReferencedTables()
			leftOnly := onlyReferences(tables, lp.Left)
			rightOnly := onlyReferences(tables, lp.Right)
			switch {
			case leftOnly:
				leftConj = append(leftConj, c)
			case rightOnly:
				rightConj = append(rightConj, c)
			default:
				remain = append(remain, c)
			}
		}
		leftPred = JoinConjuncts(leftConj)
		rightPred = JoinConjuncts(rightConj)
		remainPred = JoinConjuncts(remain)
	}

	left, err := p.build(lp.Left, leftPred)
	if err != nil {
		return nil, err
	}
	right, err := p.build(lp.Right, rightPred)
	if err != nil {
		return nil, err
	}

	var join Operator
	// Choose hash join for equi-joins when build side is small enough.
	if buildIdx, probeIdx, ok := extractEquiKey(lp.Cond, left.Schema(), right.Schema()); ok {
		// Only swap build/probe for INNER joins. For outer joins the preserved
		// side is fixed (build for LEFT, probe for RIGHT) and the output column
		// order is build||probe, so swapping would change join semantics and
		// reorder columns. Keep build=left, probe=right for outer joins.
		if lp.JoinType == InnerJoin && estimatedRows(lp.Left, p.cat) > p.opts.HashJoinThreshold {
			join = NewHashJoin(right, left, probeIdx, buildIdx, lp.JoinType)
		} else {
			join = NewHashJoin(left, right, buildIdx, probeIdx, lp.JoinType)
		}
	} else {
		join = NewNestedLoopJoin(left, right, lp.Cond, lp.JoinType)
	}

	// Wrap with a filter for residual predicates (cross-table conjuncts after pushdown).
	if remainPred != nil {
		return NewFilter(join, remainPred), nil
	}
	return join, nil
}

// extractEquiKey detects a simple col = col equi-join condition and returns
// the column indices (build side, probe side). Returns ok=false for non-equi joins.
func extractEquiKey(cond *FilterExpr, leftSchema, rightSchema Schema) (buildIdx, probeIdx int, ok bool) {
	if cond == nil || cond.kind != ExprBinOp || cond.op != BinEq {
		return 0, 0, false
	}
	if cond.left == nil || cond.right == nil {
		return 0, 0, false
	}
	if cond.left.kind != ExprColumn || cond.right.kind != ExprColumn {
		return 0, 0, false
	}
	li := leftSchema.ColIndex(cond.left.table, cond.left.col)
	ri := rightSchema.ColIndex(cond.right.table, cond.right.col)
	if li < 0 || ri < 0 {
		return 0, 0, false
	}
	return li, ri, true
}

// onlyReferences returns true if all tables in refs belong to the subtree plan.
func onlyReferences(refs map[string]bool, plan LogicalPlan) bool {
	available := availableTables(plan)
	for t := range refs {
		if !available[t] {
			return false
		}
	}
	return len(refs) > 0
}

func availableTables(plan LogicalPlan) map[string]bool {
	switch lp := plan.(type) {
	case *LogicalScan:
		return map[string]bool{lp.TableName: true}
	case *LogicalJoin:
		out := availableTables(lp.Left)
		for k, v := range availableTables(lp.Right) {
			out[k] = v
		}
		return out
	case *LogicalFilter:
		return availableTables(lp.Child)
	}
	return nil
}

func estimatedRows(plan LogicalPlan, cat *Catalog) int {
	switch lp := plan.(type) {
	case *LogicalScan:
		td, err := cat.Table(lp.TableName)
		if err != nil {
			return 0
		}
		return td.EstimatedRows()
	case *LogicalJoin:
		return estimatedRows(lp.Left, cat) * estimatedRows(lp.Right, cat)
	case *LogicalFilter:
		return estimatedRows(lp.Child, cat) / 3 // assume 33% selectivity
	}
	return 0
}

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

// ErrColumnNotFound wraps ColumnNotFoundError for errors.Is checks.
var ErrColumnNotFound = errors.New("column not found")

func (e *ColumnNotFoundError) Unwrap() error { return ErrColumnNotFound }
```

### The runnable demo

The demo builds two logical plans and prints their physical form with `Explain`. The first is a point query on an indexed column, which the planner lowers to an `IndexScan`. The second is a filter above a join, where predicate pushdown moves `users.id = 1` onto the users side before the join runs.

Create `cmd/demo/main.go`:

```go
package main

import (
	"fmt"
	"log"

	planner "example.com/cost-based-planner"
)

func main() {
	cat := planner.NewCatalog()

	usersSchema := planner.Schema{
		{Name: "id", Table: "users", Kind: planner.KindInt},
		{Name: "name", Table: "users", Kind: planner.KindString},
		{Name: "age", Table: "users", Kind: planner.KindInt},
	}
	users := &planner.TableDef{
		Name:    "users",
		Columns: usersSchema,
		Rows: []*planner.Tuple{
			{Values: []planner.Value{planner.IntVal(1), planner.StrVal("alice"), planner.IntVal(30)}},
			{Values: []planner.Value{planner.IntVal(2), planner.StrVal("bob"), planner.IntVal(25)}},
			{Values: []planner.Value{planner.IntVal(3), planner.StrVal("carol"), planner.IntVal(30)}},
			{Values: []planner.Value{planner.IntVal(4), planner.StrVal("dave"), planner.IntVal(40)}},
		},
		Indexes: make(map[string]*planner.IndexDef),
	}
	idx := &planner.IndexDef{Name: "users_id_idx", Column: "id", Entries: make(map[planner.Value][]*planner.Tuple)}
	for _, r := range users.Rows {
		idx.Entries[r.Values[0]] = append(idx.Entries[r.Values[0]], r)
	}
	users.Indexes["id"] = idx

	orders := &planner.TableDef{
		Name: "orders",
		Columns: planner.Schema{
			{Name: "order_id", Table: "orders", Kind: planner.KindInt},
			{Name: "user_id", Table: "orders", Kind: planner.KindInt},
			{Name: "amount", Table: "orders", Kind: planner.KindFloat},
		},
		Rows: []*planner.Tuple{
			{Values: []planner.Value{planner.IntVal(10), planner.IntVal(1), planner.FloatVal(99.9)}},
			{Values: []planner.Value{planner.IntVal(11), planner.IntVal(1), planner.FloatVal(49.5)}},
			{Values: []planner.Value{planner.IntVal(12), planner.IntVal(3), planner.FloatVal(5.0)}},
		},
		Indexes: make(map[string]*planner.IndexDef),
	}

	cat.Register(users)
	cat.Register(orders)
	p := planner.NewPlanner(cat, planner.DefaultPlannerOptions())

	// Plan 1: point query on an indexed column -> IndexScan.
	logical1 := &planner.LogicalFilter{
		Child: &planner.LogicalScan{TableName: "users"},
		Pred:  planner.Binop(planner.BinEq, planner.Col("users", "id"), planner.Literal(planner.IntVal(3))),
	}
	op1, err := p.Build(logical1)
	if err != nil {
		log.Fatal(err)
	}
	fmt.Println("Plan 1 (WHERE users.id = 3):")
	fmt.Print(planner.Explain(op1, 0))
	rows1, err := planner.Collect(op1)
	if err != nil {
		log.Fatal(err)
	}
	for _, r := range rows1 {
		fmt.Printf("  id=%s name=%s\n", r.Values[0].String(), r.Values[1].String())
	}

	// Plan 2: filter above a join -> predicate pushdown.
	logical2 := &planner.LogicalFilter{
		Child: &planner.LogicalJoin{
			Left:     &planner.LogicalScan{TableName: "users"},
			Right:    &planner.LogicalScan{TableName: "orders"},
			Cond:     planner.Binop(planner.BinEq, planner.Col("users", "id"), planner.Col("orders", "user_id")),
			JoinType: planner.InnerJoin,
		},
		Pred: planner.Binop(planner.BinEq, planner.Col("users", "id"), planner.Literal(planner.IntVal(1))),
	}
	op2, err := p.Build(logical2)
	if err != nil {
		log.Fatal(err)
	}
	fmt.Println("Plan 2 (JOIN ... WHERE users.id = 1):")
	fmt.Print(planner.Explain(op2, 0))
	rows2, err := planner.Collect(op2)
	if err != nil {
		log.Fatal(err)
	}
	fmt.Printf("  matched rows: %d\n", len(rows2))
}
```

Run it:

```bash
go run ./cmd/demo
```

Expected output:

```text
Plan 1 (WHERE users.id = 3):
IndexScan[users_id_idx] key=3
  id=3 name=carol
Plan 2 (JOIN ... WHERE users.id = 1):
HashJoin[0]
  IndexScan[users_id_idx] key=1
  SeqScan[orders] pred=(none)
  matched rows: 2
```

### Tests

The tests pin the two cost-driven decisions and the pushdown path. `TestPlannerChoosesIndexScan` asserts the operator *type* is `*IndexScanOperator`, not merely that the result is correct, because the whole point is the strategy. `TestPlannerPredicatePushdown` checks the row count and that `Explain` renders a non-empty tree. `TestSeqScanWithPushedFilter` exercises the scan-level filter directly.

Create `planner_test.go`:

```go
package planner

import "testing"

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

func makeOrdersTable() *TableDef {
	schema := Schema{
		{Name: "order_id", Table: "orders", Kind: KindInt},
		{Name: "user_id", Table: "orders", Kind: KindInt},
		{Name: "amount", Table: "orders", Kind: KindFloat},
	}
	rows := []*Tuple{
		{Values: []Value{IntVal(10), IntVal(1), FloatVal(99.9)}},
		{Values: []Value{IntVal(11), IntVal(1), FloatVal(49.5)}},
		{Values: []Value{IntVal(12), IntVal(2), FloatVal(200.0)}},
		{Values: []Value{IntVal(13), IntVal(3), FloatVal(5.0)}},
	}
	return &TableDef{Name: "orders", Columns: schema, Rows: rows, Indexes: make(map[string]*IndexDef)}
}

func TestIndexScanPointLookup(t *testing.T) {
	t.Parallel()

	td := makeUsersTable()
	idx := &IndexDef{
		Name:    "users_id_idx",
		Column:  "id",
		Entries: make(map[Value][]*Tuple),
	}
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
		t.Fatalf("index scan result name = %q, want bob", rows[0].Values[1].String())
	}
}

func TestPlannerChoosesIndexScan(t *testing.T) {
	t.Parallel()

	td := makeUsersTable()
	idx := &IndexDef{
		Name:    "users_id_idx",
		Column:  "id",
		Entries: make(map[Value][]*Tuple),
	}
	for _, r := range td.Rows {
		idx.Entries[r.Values[0]] = append(idx.Entries[r.Values[0]], r)
	}
	td.Indexes["id"] = idx

	cat := NewCatalog()
	cat.Register(td)

	logical := &LogicalFilter{
		Child: &LogicalScan{TableName: "users"},
		Pred:  Binop(BinEq, Col("", "id"), Literal(IntVal(3))),
	}
	planner := NewPlanner(cat, DefaultPlannerOptions())
	op, err := planner.Build(logical)
	if err != nil {
		t.Fatal(err)
	}
	if _, ok := op.(*IndexScanOperator); !ok {
		t.Fatalf("planner chose %T, want *IndexScanOperator", op)
	}
	rows, err := Collect(op)
	if err != nil {
		t.Fatal(err)
	}
	if len(rows) != 1 || rows[0].Values[1].String() != "carol" {
		t.Fatalf("index scan via planner: got %v rows", len(rows))
	}
}

func TestPlannerPredicatePushdown(t *testing.T) {
	t.Parallel()

	users := makeUsersTable()
	orders := makeOrdersTable()
	cat := NewCatalog()
	cat.Register(users)
	cat.Register(orders)

	// SELECT * FROM users JOIN orders ON users.id = orders.user_id WHERE users.id = 1
	logical := &LogicalFilter{
		Child: &LogicalJoin{
			Left:     &LogicalScan{TableName: "users"},
			Right:    &LogicalScan{TableName: "orders"},
			Cond:     Binop(BinEq, Col("users", "id"), Col("orders", "user_id")),
			JoinType: InnerJoin,
		},
		Pred: Binop(BinEq, Col("users", "id"), Literal(IntVal(1))),
	}

	planner := NewPlanner(cat, DefaultPlannerOptions())
	op, err := planner.Build(logical)
	if err != nil {
		t.Fatal(err)
	}
	rows, err := Collect(op)
	if err != nil {
		t.Fatal(err)
	}
	// alice has 2 orders -> 2 rows.
	if len(rows) != 2 {
		t.Fatalf("pushdown join: got %d rows, want 2", len(rows))
	}

	plan := Explain(op, 0)
	if len(plan) == 0 {
		t.Fatal("Explain returned empty string")
	}
}

func TestSeqScanWithPushedFilter(t *testing.T) {
	t.Parallel()

	td := makeUsersTable()
	pred := Binop(BinEq, Col("users", "age"), Literal(IntVal(30)))
	rows, err := Collect(NewSeqScan(td, pred))
	if err != nil {
		t.Fatal(err)
	}
	if len(rows) != 2 {
		t.Fatalf("pushed filter: got %d rows, want 2", len(rows))
	}
	got := map[string]bool{}
	for _, r := range rows {
		got[r.Values[1].String()] = true
	}
	if !got["alice"] || !got["carol"] {
		t.Fatalf("pushed filter rows = %v, want alice and carol", got)
	}
}
```

## Review

The planner is correct when it makes the right strategy choice and the rewrite preserves the answer. An equality predicate on an indexed column must lower to an `IndexScanOperator`, verified by a type assertion rather than by the result alone; the same predicate without an index lowers to a `SeqScanOperator` carrying the pushed filter. Predicate pushdown over an inner join must route each single-table conjunct to the matching scan and keep cross-table conjuncts as a residual filter above the join, so a `users.id = 1` filter discards users rows before the join builds. Confirm that the pushed-down `SeqScanOperator` filter drops rows during the scan, that `Explain` renders a non-empty, correctly nested tree, and that the join still returns the same rows it would without pushdown.

The pitfalls live in the pushdown logic. Pushing a cross-table conjunct such as `users.id = orders.user_id` to one scan resolves the foreign column to nothing, evaluates to NULL, and discards every row; the guard is that `onlyReferences` requires every referenced table to be available in the subtree before a conjunct is pushed. Swapping build and probe for an outer join would reorder the output columns and flip which side is preserved, so the planner only swaps for inner joins.

## Resources

- [Volcano - An Extensible and Parallel Query Evaluation System, Graefe 1994](https://paperhub.s3.amazonaws.com/dace52a42c07f7f8348b08dc2b186061.pdf) — the iterator model the planner lowers to.
- [CMU 15-445 Query Planning and Optimization](https://15445.courses.cs.cmu.edu/fall2024/slides/) — logical vs physical plans, cost-based selection, and rule rewrites.
- [pkg.go.dev/hash/fnv](https://pkg.go.dev/hash/fnv) — FNV-1a used by the hash join build phase.

---

Back to [06-group-by-and-aggregation.md](06-group-by-and-aggregation.md) | Next: [08-query-engine-and-test-suite.md](08-query-engine-and-test-suite.md)
