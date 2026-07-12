# Exercise 10: Sort-Merge Join

Hash join wins when the build side fits in memory, but its output is unordered and it pays a blocking build-phase materialization. Sort-merge join is the third classic equi-join strategy: sort both inputs on the join key, then walk them with two cursors, emitting the cartesian product of each equal-key block. It wins when the inputs already arrive sorted on the key (no sort needed, a fully streaming merge), when the output must be ordered by the join key anyway, or when neither side fits in a hash table but both fit through an external sort. Here both inputs are sorted in `Init`, so the operator is a pipeline-breaker on both children.

NULL handling mirrors the hash-join rule: `NULL = NULL` is unknown, so NULL-keyed rows never match. A NULL-keyed left row is emitted NULL-padded for a LEFT join and dropped for an INNER join; NULL-keyed right rows never match anything. This module is fully self-contained: it depends only on the standard library, ships its own demo and tests, and imports no other exercise.

## What you'll build

```text
value.go            Value, Kind, constructors, CompareValues (NULL sorts first)
operator.go         ColumnDef, Schema, Tuple, the Operator interface
catalog.go          TableDef: a named in-memory table
scan.go             SeqScanOperator, NewSeqScan, Collect
mergejoin.go        JoinType, MergeJoinOperator, NewMergeJoin, drainSorted, helpers
cmd/
  demo/
    main.go         an inner sort-merge join on key k
mergejoin_test.go   inner/left semantics, error cases, duplicate-key cartesian
```

- Files: `value.go`, `operator.go`, `catalog.go`, `scan.go`, `mergejoin.go`, `cmd/demo/main.go`, `mergejoin_test.go`.
- Implement: `MergeJoinOperator`, `NewMergeJoin`, `drainSorted`, the local `JoinType`, and the `concatTuples`/`nullPadRight` helpers, plus the sentinels `ErrMergeKeyOutOfRange` and `ErrMergeUnsupportedJoin`.
- Test: inner and left semantics including duplicate keys, NULL keys never matching, unmatched-left NULL padding, key-out-of-range and unsupported-join errors, and the full cartesian product of a shared-key block.
- Verify: `go test -run 'TestMergeJoin|TestNewMergeJoin' -race ./...`

Set up the module:

```bash
mkdir -p go-solutions/39-capstone-database-engine/06-query-planner/10-sort-merge-join/cmd/demo && cd go-solutions/39-capstone-database-engine/06-query-planner/10-sort-merge-join
```

### How the merge walks two sorted runs

Once both inputs are sorted ascending on the join key, the join is a two-cursor walk. The left cursor `i` drives the outer loop. For each left key `lk`, the right cursor `j` is advanced past every NULL key and every key strictly smaller than `lk`, so it lands on the first right row whose key is `>= lk`. The block of right rows whose key equals `lk` is then `[rStart, rEnd)`, and the block of left rows sharing `lk` is gathered the same way; their cartesian product is emitted, because two left rows and three right rows with the same key produce six joined rows. After emitting, the right cursor stays at `rEnd` so the next distinct left key resumes the scan where this one stopped — that monotone, never-rewinding advance of `j` is what makes the merge O(|R| + |S|) once the inputs are sorted, rather than the O(|R| · |S|) of a nested loop.

Two correctness rules ride on `CompareValues`, which orders NULL before every concrete value. First, NULL keys never match: a left key that is NULL skips the right side entirely (emitted NULL-padded for a LEFT join, dropped for an INNER join), and the right cursor's "advance past NULL" step ensures a NULL right key is never treated as equal to anything, including another NULL. Second, an unmatched left-key block — one whose right block is empty (`rStart == rEnd`) — is dropped for an INNER join and emitted NULL-padded on the right for a LEFT join. The operator implements only INNER and LEFT; a RIGHT join is rejected at construction, since it is just a LEFT join with the inputs swapped.

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

// TableDef is a named, in-memory table: a schema plus its rows.
type TableDef struct {
	Name    string
	Columns Schema
	Rows    []*Tuple
}
```

Create `scan.go`:

```go
package planner

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

Create `mergejoin.go`:

```go
package planner

import (
	"errors"
	"fmt"
	"sort"
)

// JoinType specifies INNER, LEFT, or RIGHT join semantics.
type JoinType uint8

const (
	InnerJoin JoinType = iota
	LeftJoin
	RightJoin
)

// ErrMergeKeyOutOfRange is returned when a merge-join key index is outside the
// child schema.
var ErrMergeKeyOutOfRange = errors.New("merge join key out of range")

// ErrMergeUnsupportedJoin is returned for join types the merge join does not
// implement (only InnerJoin and LeftJoin are supported).
var ErrMergeUnsupportedJoin = errors.New("merge join supports inner and left only")

// MergeJoinOperator implements an equi-join by sorting both inputs on the join
// key and merging them. It is a blocking operator: Init drains and sorts both
// children. NULL keys never match (NULL = NULL is unknown).
type MergeJoinOperator struct {
	left     Operator
	right    Operator
	leftKey  int
	rightKey int
	joinType JoinType
	schema   Schema
	results  []*Tuple
	pos      int
}

// NewMergeJoin builds a sort-merge join. Key indices are validated against the
// child schemas, and only InnerJoin and LeftJoin are supported.
func NewMergeJoin(left, right Operator, leftKey, rightKey int, jt JoinType) (*MergeJoinOperator, error) {
	if leftKey < 0 || leftKey >= len(left.Schema()) {
		return nil, fmt.Errorf("%w: left key %d", ErrMergeKeyOutOfRange, leftKey)
	}
	if rightKey < 0 || rightKey >= len(right.Schema()) {
		return nil, fmt.Errorf("%w: right key %d", ErrMergeKeyOutOfRange, rightKey)
	}
	if jt != InnerJoin && jt != LeftJoin {
		return nil, fmt.Errorf("%w: %v", ErrMergeUnsupportedJoin, jt)
	}
	schema := append(append(Schema(nil), left.Schema()...), right.Schema()...)
	return &MergeJoinOperator{
		left:     left,
		right:    right,
		leftKey:  leftKey,
		rightKey: rightKey,
		joinType: jt,
		schema:   schema,
	}, nil
}

func (m *MergeJoinOperator) Schema() Schema { return m.schema }

func (m *MergeJoinOperator) Close() error {
	m.results = nil
	_ = m.left.Close()
	return m.right.Close()
}

// drainSorted reads every row from op and returns them sorted ascending by the
// key column. NULL keys sort first (CompareValues orders NULL before all values).
func drainSorted(op Operator, keyIdx int) ([]*Tuple, error) {
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
	sort.SliceStable(rows, func(i, j int) bool {
		cmp, ok := CompareValues(rows[i].Values[keyIdx], rows[j].Values[keyIdx])
		return ok && cmp < 0
	})
	return rows, nil
}

func (m *MergeJoinOperator) Init() error {
	if err := m.left.Init(); err != nil {
		return err
	}
	if err := m.right.Init(); err != nil {
		return err
	}
	lrows, err := drainSorted(m.left, m.leftKey)
	if err != nil {
		return err
	}
	rrows, err := drainSorted(m.right, m.rightKey)
	if err != nil {
		return err
	}

	m.results = m.results[:0]
	rightWidth := len(m.right.Schema())
	i, j := 0, 0
	for i < len(lrows) {
		lk := lrows[i].Values[m.leftKey]
		if lk.IsNull() {
			if m.joinType == LeftJoin {
				m.results = append(m.results, nullPadRight(lrows[i], rightWidth))
			}
			i++
			continue
		}
		// Advance the right cursor past NULL keys and keys smaller than lk.
		for j < len(rrows) {
			rk := rrows[j].Values[m.rightKey]
			if rk.IsNull() {
				j++
				continue
			}
			if cmp, _ := CompareValues(rk, lk); cmp < 0 {
				j++
				continue
			}
			break
		}
		// Gather the right block whose key equals lk.
		rStart := j
		for j < len(rrows) {
			if cmp, ok := CompareValues(rrows[j].Values[m.rightKey], lk); !ok || cmp != 0 {
				break
			}
			j++
		}
		rEnd := j
		// Gather the left block whose key equals lk (duplicates -> cartesian).
		lStart := i
		for i < len(lrows) {
			if cmp, ok := CompareValues(lrows[i].Values[m.leftKey], lk); !ok || cmp != 0 {
				break
			}
			i++
		}
		if rStart == rEnd {
			// No right match for this left-key block.
			if m.joinType == LeftJoin {
				for a := lStart; a < i; a++ {
					m.results = append(m.results, nullPadRight(lrows[a], rightWidth))
				}
			}
			j = rStart // cursor already sits at the first right key > lk
			continue
		}
		for a := lStart; a < i; a++ {
			for b := rStart; b < rEnd; b++ {
				m.results = append(m.results, concatTuples(lrows[a], rrows[b]))
			}
		}
		j = rEnd
	}
	m.pos = 0
	return nil
}

func (m *MergeJoinOperator) Next() (*Tuple, error) {
	if m.pos >= len(m.results) {
		return nil, nil
	}
	t := m.results[m.pos]
	m.pos++
	return t, nil
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

### The runnable demo

The demo joins a left table whose keys arrive unsorted (3, 1, 2) with a right table keyed (1, 3). The operator sorts both, then merges: key 1 matches, key 2 has no right partner and is dropped for the inner join, key 3 matches. Output is ordered by the join key because the merge walks the sorted left run.

Create `cmd/demo/main.go`:

```go
package main

import (
	"fmt"
	"log"

	planner "example.com/sort-merge-join"
)

func main() {
	leftSchema := planner.Schema{
		{Name: "k", Table: "l", Kind: planner.KindInt},
		{Name: "lv", Table: "l", Kind: planner.KindString},
	}
	left := &planner.TableDef{
		Name:    "l",
		Columns: leftSchema,
		Rows: []*planner.Tuple{
			{Values: []planner.Value{planner.IntVal(3), planner.StrVal("c")}},
			{Values: []planner.Value{planner.IntVal(1), planner.StrVal("a")}},
			{Values: []planner.Value{planner.IntVal(2), planner.StrVal("b")}},
		},
	}
	rightSchema := planner.Schema{
		{Name: "k", Table: "r", Kind: planner.KindInt},
		{Name: "rv", Table: "r", Kind: planner.KindString},
	}
	right := &planner.TableDef{
		Name:    "r",
		Columns: rightSchema,
		Rows: []*planner.Tuple{
			{Values: []planner.Value{planner.IntVal(1), planner.StrVal("x")}},
			{Values: []planner.Value{planner.IntVal(3), planner.StrVal("z")}},
		},
	}

	op, err := planner.NewMergeJoin(planner.NewSeqScan(left), planner.NewSeqScan(right), 0, 0, planner.InnerJoin)
	if err != nil {
		log.Fatal(err)
	}
	rows, err := planner.Collect(op)
	if err != nil {
		log.Fatal(err)
	}
	fmt.Println("Sort-merge inner join on k:")
	for _, r := range rows {
		fmt.Printf("  l.k=%s l.lv=%s r.rv=%s\n",
			r.Values[0].String(), r.Values[1].String(), r.Values[3].String())
	}
}
```

Run it:

```bash
go run ./cmd/demo
```

Expected output:

```
Sort-merge inner join on k:
  l.k=1 l.lv=a r.rv=x
  l.k=3 l.lv=c r.rv=z
```

### Tests

The table-driven test covers both supported join types over fixtures with a duplicate left key (two rows at key 1, producing a 2 x 1 cartesian), an unmatched left key (key 2), and NULL keys on both sides that must never match. The inner join yields 3 rows; the left join adds the two unmatched left rows NULL-padded, for 5. `TestNewMergeJoinErrors` checks the two sentinels via `errors.Is`, including the rejected RIGHT join. `TestMergeJoinCartesian` pins the multiplicity rule directly: three left rows and two right rows sharing a key produce the full 3 x 2 = 6-row product.

Create `mergejoin_test.go`:

```go
package planner

import (
	"errors"
	"testing"
)

func mergeLeft() *TableDef {
	schema := Schema{
		{Name: "k", Table: "l", Kind: KindInt},
		{Name: "lv", Table: "l", Kind: KindString},
	}
	rows := []*Tuple{
		{Values: []Value{IntVal(3), StrVal("c")}},
		{Values: []Value{IntVal(1), StrVal("a")}},
		{Values: []Value{IntVal(1), StrVal("a2")}}, // duplicate key -> cartesian
		{Values: []Value{IntVal(2), StrVal("b")}},  // no right match
		{Values: []Value{Null, StrVal("n")}},       // NULL key: never matches
	}
	return &TableDef{Name: "l", Columns: schema, Rows: rows}
}

func mergeRight() *TableDef {
	schema := Schema{
		{Name: "k", Table: "r", Kind: KindInt},
		{Name: "rv", Table: "r", Kind: KindString},
	}
	rows := []*Tuple{
		{Values: []Value{IntVal(1), StrVal("x")}},
		{Values: []Value{IntVal(3), StrVal("z")}},
		{Values: []Value{Null, StrVal("rn")}}, // NULL key: never matches
	}
	return &TableDef{Name: "r", Columns: schema, Rows: rows}
}

func TestMergeJoin(t *testing.T) {
	t.Parallel()

	cases := []struct {
		name     string
		joinType JoinType
		wantRows int
		wantPad  int // rows whose right side is NULL-padded
	}{
		// key 1: 2 left x 1 right = 2; key 3: 1 x 1 = 1; key 2 and NULL: none.
		{"inner", InnerJoin, 3, 0},
		// inner 3 + unmatched left rows (key 2, NULL) NULL-padded = 5.
		{"left", LeftJoin, 5, 2},
	}
	for _, tc := range cases {
		tc := tc
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			op, err := NewMergeJoin(NewSeqScan(mergeLeft()), NewSeqScan(mergeRight()), 0, 0, tc.joinType)
			if err != nil {
				t.Fatalf("NewMergeJoin: %v", err)
			}
			rows, err := Collect(op)
			if err != nil {
				t.Fatalf("Collect: %v", err)
			}
			if len(rows) != tc.wantRows {
				t.Fatalf("got %d rows, want %d", len(rows), tc.wantRows)
			}
			pad := 0
			for _, r := range rows {
				if r.Values[3].IsNull() { // right "rv" column
					pad++
				}
			}
			if pad != tc.wantPad {
				t.Fatalf("got %d NULL-padded rows, want %d", pad, tc.wantPad)
			}
		})
	}
}

func TestNewMergeJoinErrors(t *testing.T) {
	t.Parallel()

	l := NewSeqScan(mergeLeft())
	r := NewSeqScan(mergeRight())
	cases := []struct {
		name     string
		leftKey  int
		rightKey int
		jt       JoinType
		want     error
	}{
		{"left key out of range", 9, 0, InnerJoin, ErrMergeKeyOutOfRange},
		{"right key out of range", 0, 9, InnerJoin, ErrMergeKeyOutOfRange},
		{"unsupported right join", 0, 0, RightJoin, ErrMergeUnsupportedJoin},
	}
	for _, tc := range cases {
		tc := tc
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			_, err := NewMergeJoin(l, r, tc.leftKey, tc.rightKey, tc.jt)
			if !errors.Is(err, tc.want) {
				t.Fatalf("err = %v, want %v", err, tc.want)
			}
		})
	}
}

func TestMergeJoinCartesian(t *testing.T) {
	t.Parallel()

	lschema := Schema{
		{Name: "k", Table: "l", Kind: KindInt},
		{Name: "lv", Table: "l", Kind: KindString},
	}
	left := &TableDef{
		Name:    "l",
		Columns: lschema,
		Rows: []*Tuple{
			{Values: []Value{IntVal(5), StrVal("l1")}},
			{Values: []Value{IntVal(5), StrVal("l2")}},
			{Values: []Value{IntVal(5), StrVal("l3")}},
		},
	}
	rschema := Schema{
		{Name: "k", Table: "r", Kind: KindInt},
		{Name: "rv", Table: "r", Kind: KindString},
	}
	right := &TableDef{
		Name:    "r",
		Columns: rschema,
		Rows: []*Tuple{
			{Values: []Value{IntVal(5), StrVal("r1")}},
			{Values: []Value{IntVal(5), StrVal("r2")}},
		},
	}
	op, err := NewMergeJoin(NewSeqScan(left), NewSeqScan(right), 0, 0, InnerJoin)
	if err != nil {
		t.Fatalf("NewMergeJoin: %v", err)
	}
	rows, err := Collect(op)
	if err != nil {
		t.Fatalf("Collect: %v", err)
	}
	if len(rows) != 6 {
		t.Fatalf("got %d rows, want 6 (3x2 cartesian for key 5)", len(rows))
	}
}
```

## Review

The join is correct when, for each join key, it emits exactly the cartesian product of the left and right blocks sharing that key, and nothing for NULL keys. Confirm that duplicate keys multiply (the 2 x 1 block at key 1, the 3 x 2 block in the cartesian test), that an unmatched left-key block is dropped for INNER and NULL-padded for LEFT, that NULL keys on either side never match — not even another NULL — and that the right cursor advances monotonically so the merge stays linear once sorted. `NewMergeJoin` must reject an out-of-range key index and an unsupported RIGHT join with errors satisfying `errors.Is`. The classic bug is letting the right cursor treat a NULL key as equal to a NULL left key; the "advance past NULL" step in the merge is what prevents it.

## Resources

- [CMU 15-445 Query Execution](https://15445.courses.cs.cmu.edu/fall2024/slides/) — sort-merge join among the physical join algorithms and when it wins.
- [pkg.go.dev/sort](https://pkg.go.dev/sort) — `sort.SliceStable`, used to sort both inputs on the join key.
- [Volcano - An Extensible and Parallel Query Evaluation System, Graefe 1994](https://paperhub.s3.amazonaws.com/dace52a42c07f7f8348b08dc2b186061.pdf) — the iterator model these operators implement.

---

Back to [09-top-n-bounded-heap.md](09-top-n-bounded-heap.md) | Next: [11-rule-based-optimizer.md](11-rule-based-optimizer.md)
