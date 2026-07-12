# Exercise 11: Rule-Based Optimizer (Predicate and Projection Pushdown)

A cost-based planner pushes predicates while it lowers a logical plan to physical operators, so the rewrite and the operator choice are tangled together. A rule-based optimizer separates them: it rewrites the *logical* plan into an equivalent logical plan, independent of any physical operator selection, so each rewrite can be inspected and reasoned about on its own. This exercise builds two canonical, semantics-preserving rules — predicate pushdown and projection pushdown — and proves them correct by comparing the result set of the original plan against the rewritten one.

This module is fully self-contained. It depends only on the standard library, bundles every operator and logical node it needs, ships its own demo and tests, and imports no other exercise.

## What you'll build

```text
rule-based-optimizer/
  go.mod
  value.go           Value, Kind, CompareValues, three-valued ToBool
  operator.go        Schema, Tuple, Operator interface
  catalog.go         Catalog, TableDef, IndexDef
  scan.go            SeqScanOperator, IndexScanOperator
  filter.go          FilterExpr, FilterOperator, ProjectionOperator, conjunct split/join
  join.go            JoinType, NestedLoopJoinOperator, concat/null-pad helpers
  plan.go            LogicalScan/Join/Filter, availableTables, onlyReferences, Collect
  optimize.go        LogicalProjection, Optimizer, predicate + projection pushdown
  cmd/
    demo/
      main.go        optimize a join query and run original vs rewritten
  optimize_test.go   semantics-preserving + unknown-node + projection-pushdown structure
```

- Files: `value.go`, `operator.go`, `catalog.go`, `scan.go`, `filter.go`, `join.go`, `plan.go`, `optimize.go`, `cmd/demo/main.go`, `optimize_test.go`.
- Implement: `Optimizer`, `NewOptimizer`, `Optimize`, `pushPredicates`, `pushProjections`, `Build`, `LogicalProjection`, and `ErrUnknownLogicalNode`, on top of the bundled scaffolding (typed values, the operator interface, scans, filters, projection, nested-loop join, and the logical-plan nodes).
- Test: rewriting preserves the result set for a join query with a conjunctive WHERE clause; an unrecognized node returns `ErrUnknownLogicalNode`; and the `orders` scan in the rewritten plan is wrapped in a column-pruning `LogicalProjection` that drops `order_id`, proving the projection-pushdown rule fired and not merely that the rows are unchanged.
- Verify: `go test -run 'TestOptimizer|TestProjectionPushdown' -race ./...`

### Why rewrite the logical plan, and why these two rules

A logical plan is relational algebra: scan, select (σ), project (π), join (⋈). A rewrite rule maps one logical plan to another that returns the identical result set for every possible input — that property is what "semantics-preserving" means, and it is the only license a rewrite has to exist. Two rules carry most of the value.

Predicate pushdown moves a filter as close to the scan as possible so rows are discarded before they reach an expensive join. It splits a conjunctive WHERE clause into its ANDed conjuncts and, for each conjunct that references only one side of a join, pushes that conjunct down to wrap the matching input. This is valid through an inner join because σ(R ⋈ S) with a single-table predicate on R equals σ(R) ⋈ S — the conjuncts are ANDed either way, so moving one closer to the scan cannot change which combined rows survive. It is *not* generally valid to push a predicate into the null-supplying side of an outer join: a row the filter removes there would otherwise have survived NULL-padded, changing the cardinality. The optimizer therefore pushes only through `InnerJoin` and leaves every other join untouched.

Projection pushdown prunes the columns a subtree emits down to the set actually referenced above it — the final projection plus every column named by a predicate or a join condition. Because every operator resolves columns by name, dropping a column nothing references upstream cannot change the result. The optimizer walks the tree with a required-column set, intersects it with each table's schema, and inserts a column-pruning `LogicalProjection` above any scan that emits more columns than are needed.

Both rules shrink the data volume flowing up the tree without changing the answer. The test verifies that directly: it runs the original and rewritten plans over the same catalog and asserts the result sets are equal.

### The scaffolding: values, operators, scans, filters, join

The optimizer rewrites a plan, but to *check* a rewrite it must execute both versions, so the module carries a small executor. These files are the same volcano-model pieces the earlier exercises built; they are reproduced here so this module stands alone. Read them once and focus your attention on `optimize.go`.

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
	// Iterative matcher with backtracking on %.
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

Create `join.go`:

```go
package planner

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
```

The logical-plan nodes and a few helpers the optimizer leans on live in `plan.go`. `availableTables` reports which tables a subtree can resolve, `onlyReferences` tests whether a predicate's tables are all available in a subtree (the predicate-pushdown gate), and `Collect` drains an operator so a test can compare result sets.

Create `plan.go`:

```go
package planner

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

### The optimizer

`Optimize` runs in two passes. `pushPredicates` walks the tree and, at every `LogicalFilter` directly above an `InnerJoin`, splits the predicate into conjuncts and routes each single-table conjunct to the matching side as a new `LogicalFilter`; cross-table conjuncts that neither side can satisfy alone stay above the join as a residual filter. The `InnerJoin` guard is the correctness boundary: through any other join type the predicate is left in place. `pushProjections` then walks down with a required-column set — seeded from the top projection, grown by every predicate and join condition it passes — and wraps a scan in a `LogicalProjection` whenever the scan's table has columns the set does not need. A `nil` required set means "no top-level projection, prune nothing," so a plan without a final projection is never over-pruned.

`Build` lowers any logical plan, including the `LogicalProjection` nodes the optimizer inserts, into a physical operator tree using the simplest operator for each node (sequential scan, filter, nested-loop join, projection). It is deliberately simple so the optimizer's correctness is checked independently of cost-based operator selection — the same plan, rewritten or not, runs through the same lowering.

Create `optimize.go`:

```go
package planner

import (
	"errors"
	"fmt"
)

// ErrUnknownLogicalNode is returned by the optimizer when it meets a logical plan
// node type it does not recognize.
var ErrUnknownLogicalNode = errors.New("unknown logical plan node")

// LogicalProjection restricts the columns flowing out of its child. Columns are
// named "table.col" (preferred) or bare "col".
type LogicalProjection struct {
	Child LogicalPlan
	Cols  []string
}

func (*LogicalProjection) logicalPlan() {}

// Optimizer applies semantics-preserving rule-based rewrites to a logical plan:
// predicate pushdown (move single-table filters next to their scan, through INNER
// joins) and projection pushdown (prune the columns a scan emits to those actually
// used above it). Both preserve the result set because operators resolve columns
// by name and pushed conjuncts are already implied by the original WHERE clause.
type Optimizer struct {
	cat *Catalog
}

// NewOptimizer returns an optimizer that resolves table schemas through cat.
func NewOptimizer(cat *Catalog) *Optimizer { return &Optimizer{cat: cat} }

// Optimize rewrites lp. It returns ErrUnknownLogicalNode if the plan contains an
// unrecognized node type.
func (o *Optimizer) Optimize(lp LogicalPlan) (LogicalPlan, error) {
	if err := validateLogical(lp); err != nil {
		return nil, err
	}
	p := o.pushPredicates(lp)
	p = o.pushProjections(p, initialRequired(p))
	return p, nil
}

func validateLogical(lp LogicalPlan) error {
	switch n := lp.(type) {
	case *LogicalScan:
		return nil
	case *LogicalFilter:
		return validateLogical(n.Child)
	case *LogicalProjection:
		return validateLogical(n.Child)
	case *LogicalJoin:
		if err := validateLogical(n.Left); err != nil {
			return err
		}
		return validateLogical(n.Right)
	default:
		return fmt.Errorf("%w: %T", ErrUnknownLogicalNode, lp)
	}
}

// pushPredicates moves single-table conjuncts of a filter down through an INNER
// join onto the matching input. Cross-table conjuncts stay above the join.
func (o *Optimizer) pushPredicates(lp LogicalPlan) LogicalPlan {
	switch n := lp.(type) {
	case *LogicalScan:
		return n
	case *LogicalProjection:
		return &LogicalProjection{Child: o.pushPredicates(n.Child), Cols: n.Cols}
	case *LogicalJoin:
		return &LogicalJoin{
			Left:     o.pushPredicates(n.Left),
			Right:    o.pushPredicates(n.Right),
			Cond:     n.Cond,
			JoinType: n.JoinType,
		}
	case *LogicalFilter:
		join, ok := n.Child.(*LogicalJoin)
		if !ok || join.JoinType != InnerJoin {
			return &LogicalFilter{Child: o.pushPredicates(n.Child), Pred: n.Pred}
		}
		var leftC, rightC, residual []*FilterExpr
		for _, c := range SplitConjuncts(n.Pred) {
			refs := c.ReferencedTables()
			switch {
			case onlyReferences(refs, join.Left):
				leftC = append(leftC, c)
			case onlyReferences(refs, join.Right):
				rightC = append(rightC, c)
			default:
				residual = append(residual, c)
			}
		}
		newLeft := join.Left
		if p := JoinConjuncts(leftC); p != nil {
			newLeft = &LogicalFilter{Child: newLeft, Pred: p}
		}
		newRight := join.Right
		if p := JoinConjuncts(rightC); p != nil {
			newRight = &LogicalFilter{Child: newRight, Pred: p}
		}
		newJoin := &LogicalJoin{
			Left:     o.pushPredicates(newLeft),
			Right:    o.pushPredicates(newRight),
			Cond:     join.Cond,
			JoinType: join.JoinType,
		}
		if p := JoinConjuncts(residual); p != nil {
			return &LogicalFilter{Child: newJoin, Pred: p}
		}
		return newJoin
	}
	return lp
}

// initialRequired returns the column set demanded by a top-level projection, or
// nil meaning "all columns required" (no pruning).
func initialRequired(lp LogicalPlan) map[string]bool {
	if pj, ok := lp.(*LogicalProjection); ok {
		req := make(map[string]bool)
		addColNames(req, pj.Cols)
		return req
	}
	return nil
}

// pushProjections inserts column-pruning projections above scans. required is the
// set of column names (qualified and bare) needed by ancestors; nil disables
// pruning entirely.
func (o *Optimizer) pushProjections(lp LogicalPlan, required map[string]bool) LogicalPlan {
	switch n := lp.(type) {
	case *LogicalProjection:
		req := make(map[string]bool)
		addColNames(req, n.Cols)
		return &LogicalProjection{Child: o.pushProjections(n.Child, req), Cols: n.Cols}
	case *LogicalFilter:
		req := cloneRequired(required)
		if req != nil {
			addExprCols(req, n.Pred)
		}
		return &LogicalFilter{Child: o.pushProjections(n.Child, req), Pred: n.Pred}
	case *LogicalJoin:
		req := cloneRequired(required)
		var leftReq, rightReq map[string]bool
		if req != nil {
			addExprCols(req, n.Cond)
			leftReq = make(map[string]bool)
			rightReq = make(map[string]bool)
			leftTabs := availableTables(n.Left)
			rightTabs := availableTables(n.Right)
			for name := range req {
				tab := tableOf(name)
				switch {
				case tab != "" && leftTabs[tab]:
					leftReq[name] = true
				case tab != "" && rightTabs[tab]:
					rightReq[name] = true
				default:
					// Unqualified or ambiguous: require on both sides (safe).
					leftReq[name] = true
					rightReq[name] = true
				}
			}
		}
		return &LogicalJoin{
			Left:     o.pushProjections(n.Left, leftReq),
			Right:    o.pushProjections(n.Right, rightReq),
			Cond:     n.Cond,
			JoinType: n.JoinType,
		}
	case *LogicalScan:
		if required == nil {
			return n
		}
		td, err := o.cat.Table(n.TableName)
		if err != nil {
			return n // leave unpruned; the executor will surface the error
		}
		var cols []string
		for _, cd := range td.Columns {
			qualified := cd.Name
			if cd.Table != "" {
				qualified = cd.Table + "." + cd.Name
			}
			if required[qualified] || required[cd.Name] {
				cols = append(cols, qualified)
			}
		}
		if len(cols) == 0 || len(cols) == len(td.Columns) {
			return n // nothing to prune, or everything is needed
		}
		return &LogicalProjection{Child: n, Cols: cols}
	}
	return lp
}

func cloneRequired(req map[string]bool) map[string]bool {
	if req == nil {
		return nil
	}
	out := make(map[string]bool, len(req))
	for k := range req {
		out[k] = true
	}
	return out
}

// addColNames records each projection column under both its qualified and bare
// name so scans can match regardless of qualification.
func addColNames(req map[string]bool, cols []string) {
	for _, c := range cols {
		req[c] = true
		req[colOf(c)] = true
	}
}

// addExprCols records every column referenced by an expression tree.
func addExprCols(req map[string]bool, e *FilterExpr) {
	if e == nil {
		return
	}
	if e.kind == ExprColumn {
		if e.table != "" {
			req[e.table+"."+e.col] = true
		}
		req[e.col] = true
	}
	addExprCols(req, e.left)
	addExprCols(req, e.right)
}

func tableOf(name string) string {
	for i := 0; i < len(name); i++ {
		if name[i] == '.' {
			return name[:i]
		}
	}
	return ""
}

func colOf(name string) string {
	for i := 0; i < len(name); i++ {
		if name[i] == '.' {
			return name[i+1:]
		}
	}
	return name
}

// Build lowers a logical plan (including LogicalProjection) into a physical
// operator tree using a straightforward operator choice (sequential scan, filter,
// nested-loop join, projection). It is deliberately simple so the optimizer's
// correctness can be checked independently of cost-based operator selection.
func (o *Optimizer) Build(lp LogicalPlan) (Operator, error) {
	switch n := lp.(type) {
	case *LogicalScan:
		td, err := o.cat.Table(n.TableName)
		if err != nil {
			return nil, err
		}
		return NewSeqScan(td, nil), nil
	case *LogicalFilter:
		child, err := o.Build(n.Child)
		if err != nil {
			return nil, err
		}
		return NewFilter(child, n.Pred), nil
	case *LogicalJoin:
		l, err := o.Build(n.Left)
		if err != nil {
			return nil, err
		}
		r, err := o.Build(n.Right)
		if err != nil {
			return nil, err
		}
		return NewNestedLoopJoin(l, r, n.Cond, n.JoinType), nil
	case *LogicalProjection:
		child, err := o.Build(n.Child)
		if err != nil {
			return nil, err
		}
		return NewProjection(child, n.Cols)
	default:
		return nil, fmt.Errorf("%w: %T", ErrUnknownLogicalNode, lp)
	}
}
```

### The runnable demo

The demo builds the query `SELECT users.name, orders.amount FROM users JOIN orders ON users.id = orders.user_id WHERE users.age = 30 AND orders.amount > 10`, optimizes it, and runs both the original and the rewritten plan through the same `Build` and `Collect`. The two must agree row for row — that is the whole claim of a semantics-preserving rewrite, made visible.

Create `cmd/demo/main.go`:

```go
package main

import (
	"fmt"
	"log"

	planner "example.com/rule-based-optimizer"
)

func main() {
	cat := planner.NewCatalog()
	cat.Register(usersTable())
	cat.Register(ordersTable())

	// SELECT users.name, orders.amount
	// FROM users JOIN orders ON users.id = orders.user_id
	// WHERE users.age = 30 AND orders.amount > 10
	original := &planner.LogicalProjection{
		Child: &planner.LogicalFilter{
			Child: &planner.LogicalJoin{
				Left:     &planner.LogicalScan{TableName: "users"},
				Right:    &planner.LogicalScan{TableName: "orders"},
				Cond:     planner.Binop(planner.BinEq, planner.Col("users", "id"), planner.Col("orders", "user_id")),
				JoinType: planner.InnerJoin,
			},
			Pred: planner.Binop(planner.BinAnd,
				planner.Binop(planner.BinEq, planner.Col("users", "age"), planner.Literal(planner.IntVal(30))),
				planner.Binop(planner.BinGt, planner.Col("orders", "amount"), planner.Literal(planner.FloatVal(10))),
			),
		},
		Cols: []string{"users.name", "orders.amount"},
	}

	opt := planner.NewOptimizer(cat)
	rewritten, err := opt.Optimize(original)
	if err != nil {
		log.Fatal(err)
	}

	origRows := run(opt, original)
	optRows := run(opt, rewritten)

	fmt.Printf("original plan rows: %d\n", len(origRows))
	fmt.Printf("rewritten plan rows: %d\n", len(optRows))
	fmt.Println("rows:")
	for _, r := range optRows {
		fmt.Printf("  name=%s amount=%s\n", r.Values[0].String(), r.Values[1].String())
	}
}

func run(opt *planner.Optimizer, plan planner.LogicalPlan) []*planner.Tuple {
	op, err := opt.Build(plan)
	if err != nil {
		log.Fatal(err)
	}
	rows, err := planner.Collect(op)
	if err != nil {
		log.Fatal(err)
	}
	return rows
}

func usersTable() *planner.TableDef {
	schema := planner.Schema{
		{Name: "id", Table: "users", Kind: planner.KindInt},
		{Name: "name", Table: "users", Kind: planner.KindString},
		{Name: "age", Table: "users", Kind: planner.KindInt},
	}
	rows := []*planner.Tuple{
		{Values: []planner.Value{planner.IntVal(1), planner.StrVal("alice"), planner.IntVal(30)}},
		{Values: []planner.Value{planner.IntVal(2), planner.StrVal("bob"), planner.IntVal(25)}},
		{Values: []planner.Value{planner.IntVal(3), planner.StrVal("carol"), planner.IntVal(30)}},
		{Values: []planner.Value{planner.IntVal(4), planner.StrVal("dave"), planner.Null}},
	}
	return &planner.TableDef{Name: "users", Columns: schema, Rows: rows, Indexes: make(map[string]*planner.IndexDef)}
}

func ordersTable() *planner.TableDef {
	schema := planner.Schema{
		{Name: "order_id", Table: "orders", Kind: planner.KindInt},
		{Name: "user_id", Table: "orders", Kind: planner.KindInt},
		{Name: "amount", Table: "orders", Kind: planner.KindFloat},
	}
	rows := []*planner.Tuple{
		{Values: []planner.Value{planner.IntVal(10), planner.IntVal(1), planner.FloatVal(99.9)}},
		{Values: []planner.Value{planner.IntVal(11), planner.IntVal(1), planner.FloatVal(49.5)}},
		{Values: []planner.Value{planner.IntVal(12), planner.IntVal(2), planner.FloatVal(200.0)}},
		{Values: []planner.Value{planner.IntVal(13), planner.IntVal(3), planner.FloatVal(5.0)}},
	}
	return &planner.TableDef{Name: "orders", Columns: schema, Rows: rows, Indexes: make(map[string]*planner.IndexDef)}
}
```

Run it:

```bash
go run ./cmd/demo
```

Expected output:

```
original plan rows: 2
rewritten plan rows: 2
rows:
  name=alice amount=99.9
  name=alice amount=49.5
```

### Tests

The first test is the semantics guarantee: it optimizes the join query and asserts the rewritten plan returns the same multiset of rows as the original (`sameResultSet` compares counts, not order, because a rewrite may reorder rows). The second pins the error path for an unrecognized node. The third reaches past the result set and into the plan *shape*: it walks the rewritten tree and asserts the `orders` scan is wrapped in a `LogicalProjection` that drops `order_id`, which proves the projection-pushdown rule actually fired rather than the rows merely happening to match.

Create `optimize_test.go`:

```go
package planner

import (
	"errors"
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

func tupleKey(t *Tuple) string {
	var b []byte
	for _, v := range t.Values {
		b = append(b, []byte(v.String())...)
		b = append(b, '|')
	}
	return string(b)
}

func sameResultSet(a, b []*Tuple) bool {
	if len(a) != len(b) {
		return false
	}
	counts := make(map[string]int)
	for _, t := range a {
		counts[tupleKey(t)]++
	}
	for _, t := range b {
		counts[tupleKey(t)]--
	}
	for _, c := range counts {
		if c != 0 {
			return false
		}
	}
	return true
}

func mustCollect(t *testing.T, opt *Optimizer, plan LogicalPlan) []*Tuple {
	t.Helper()
	op, err := opt.Build(plan)
	if err != nil {
		t.Fatalf("Build: %v", err)
	}
	rows, err := Collect(op)
	if err != nil {
		t.Fatalf("Collect: %v", err)
	}
	return rows
}

// optimizerFixture is the join query both structural and semantic tests use:
// SELECT users.name, orders.amount
// FROM users JOIN orders ON users.id = orders.user_id
// WHERE users.age = 30 AND orders.amount > 10
func optimizerFixture() *LogicalProjection {
	return &LogicalProjection{
		Child: &LogicalFilter{
			Child: &LogicalJoin{
				Left:     &LogicalScan{TableName: "users"},
				Right:    &LogicalScan{TableName: "orders"},
				Cond:     Binop(BinEq, Col("users", "id"), Col("orders", "user_id")),
				JoinType: InnerJoin,
			},
			Pred: Binop(BinAnd,
				Binop(BinEq, Col("users", "age"), Literal(IntVal(30))),
				Binop(BinGt, Col("orders", "amount"), Literal(FloatVal(10))),
			),
		},
		Cols: []string{"users.name", "orders.amount"},
	}
}

func TestOptimizerSemanticsPreserving(t *testing.T) {
	t.Parallel()

	cat := NewCatalog()
	cat.Register(makeUsersTable())
	cat.Register(makeOrdersTable())

	original := optimizerFixture()

	opt := NewOptimizer(cat)
	rewritten, err := opt.Optimize(original)
	if err != nil {
		t.Fatalf("Optimize: %v", err)
	}

	origRows := mustCollect(t, opt, original)
	optRows := mustCollect(t, opt, rewritten)
	if !sameResultSet(origRows, optRows) {
		t.Fatalf("optimizer changed the result set: orig=%d opt=%d", len(origRows), len(optRows))
	}
	// alice(age 30) has orders 99.9 and 49.5 (both > 10); carol(age 30) has 5.0
	// (filtered out); everyone else fails the age predicate. So 2 rows survive.
	if len(optRows) != 2 {
		t.Fatalf("got %d rows, want 2", len(optRows))
	}
}

type unknownLogical struct{}

func (unknownLogical) logicalPlan() {}

func TestOptimizerRejectsUnknownNode(t *testing.T) {
	t.Parallel()

	opt := NewOptimizer(NewCatalog())
	_, err := opt.Optimize(unknownLogical{})
	if !errors.Is(err, ErrUnknownLogicalNode) {
		t.Fatalf("err = %v, want ErrUnknownLogicalNode", err)
	}
}

// findScanProjection returns the LogicalProjection that directly wraps the scan
// of table, or nil if that scan is not wrapped in a projection.
func findScanProjection(plan LogicalPlan, table string) *LogicalProjection {
	switch n := plan.(type) {
	case *LogicalProjection:
		if s, ok := n.Child.(*LogicalScan); ok && s.TableName == table {
			return n
		}
		return findScanProjection(n.Child, table)
	case *LogicalFilter:
		return findScanProjection(n.Child, table)
	case *LogicalJoin:
		if p := findScanProjection(n.Left, table); p != nil {
			return p
		}
		return findScanProjection(n.Right, table)
	}
	return nil
}

func TestProjectionPushdownDropsColumn(t *testing.T) {
	t.Parallel()

	cat := NewCatalog()
	cat.Register(makeUsersTable())
	cat.Register(makeOrdersTable())

	opt := NewOptimizer(cat)
	rewritten, err := opt.Optimize(optimizerFixture())
	if err != nil {
		t.Fatalf("Optimize: %v", err)
	}

	// Only orders.user_id (join key) and orders.amount (predicate + projection)
	// are needed above the orders scan, so order_id must be pruned away.
	proj := findScanProjection(rewritten, "orders")
	if proj == nil {
		t.Fatal("orders scan was not wrapped in a column-pruning projection")
	}
	for _, c := range proj.Cols {
		if c == "order_id" || c == "orders.order_id" {
			t.Fatalf("projection failed to drop order_id: %v", proj.Cols)
		}
	}
	hasAmount := false
	for _, c := range proj.Cols {
		if c == "orders.amount" || c == "amount" {
			hasAmount = true
		}
	}
	if !hasAmount {
		t.Fatalf("projection dropped a needed column: %v", proj.Cols)
	}
}
```

## Review

The optimizer is correct when the rewritten plan returns the identical multiset of rows as the original for every input — verified here by `sameResultSet` over the join query — and when the rewrite is visible in the plan shape, not just the result. Predicate pushdown must move `users.age = 30` to wrap the users scan and `orders.amount > 10` to wrap the orders scan while leaving the cross-table join condition on the join, and it must do this only through an inner join: pushing a predicate into the null-supplying side of an outer join would drop rows that should have survived NULL-padded. Projection pushdown must wrap the orders scan in a `LogicalProjection` that keeps `user_id` and `amount` and drops `order_id`, because those are the only orders columns any operator above the scan references. An unrecognized node must surface as `ErrUnknownLogicalNode` rather than a silent misrewrite. Run the suite under `go test -race` to confirm the rewrites stay clean.

## Resources

- [CMU 15-445 Query Planning & Optimization](https://15445.courses.cs.cmu.edu/fall2024/slides/) — rule-based rewrites, including predicate and projection pushdown, at the logical level.
- [Modern SQL: Three-Valued Logic](https://modern-sql.com/concept/three-valued-logic) — why a predicate cannot be pushed into the null-supplying side of an outer join.
- [Volcano - An Extensible and Parallel Query Evaluation System, Graefe 1994](https://paperhub.s3.amazonaws.com/dace52a42c07f7f8348b08dc2b186061.pdf) — the iterator model the executor used to check the rewrites is built on.

---

Back to [10-sort-merge-join.md](10-sort-merge-join.md) | Next: [Multi-Version Concurrency Control (MVCC)](../07-mvcc/00-concepts.md)
