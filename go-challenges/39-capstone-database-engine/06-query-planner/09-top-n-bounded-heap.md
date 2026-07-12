# Exercise 9: Top-N Operator with a Bounded Heap

A `SortOperator` followed by a `LimitOperator` answers `ORDER BY ... LIMIT k` correctly, but wastefully: it materializes and fully sorts every input row at O(n log n) time and O(n) space, only to throw away all but `offset+limit` of them. A Top-N operator keeps only `k = offset+limit` rows in a bounded max-heap, evicting the current worst whenever the heap overflows, so it costs O(n log k) time and O(k) space. It is still a pipeline-breaker: `Init` must drain the whole child before the first row is emitted, because the very last input row could still belong in the result.

This module is fully self-contained. It depends only on the standard library, ships its own demo and tests, and imports no other exercise. It carries just the scaffolding the operator needs: the nullable `Value`, the `Operator` interface, a minimal sequential scan, and the Top-N operator itself.

## What you'll build

```text
value.go            Value, Kind, constructors, CompareValues (NULL sorts first)
operator.go         ColumnDef, Schema, Tuple, the Operator interface
catalog.go          TableDef: a named in-memory table
scan.go             SeqScanOperator, NewSeqScan, Collect
topn.go             SortKey, TopNOperator, NewTopN, ErrInvalidTopN, the bounded heap
cmd/
  demo/
    main.go         ORDER BY score DESC LIMIT 2 over a small table
topn_test.go        ordering cases, negative-parameter rejection, NULL-sorts-first
```

- Files: `value.go`, `operator.go`, `catalog.go`, `scan.go`, `topn.go`, `cmd/demo/main.go`, `topn_test.go`.
- Implement: `TopNOperator`, `NewTopN`, `ErrInvalidTopN`, `compareByKeys`, and the `topNHeap` that backs the bounded buffer.
- Test: multi-key ordering with `Desc` per key, `offset` skipping, a limit larger than the input, a zero limit, rejection of negative `limit`/`offset`, and that a NULL sort key sorts first under ASC.
- Verify: `go test -run 'TestTopN|TestNewTopN' -race ./...`

### Why a heap and not a sort

The key insight is that an `ORDER BY ... LIMIT k` query never needs more than `k` rows resident at once. If the buffer already holds the best `k` rows seen so far, a new row matters only if it beats the worst of those `k`; otherwise it is discarded immediately. To make "the worst of the kept rows" cheap to find and cheap to replace, the buffer is a max-heap *under the ORDER BY ordering*: its root is the row that sorts last among those kept, so it is exactly the one to evict on overflow. Each input row costs one comparison against the root plus, at most, one O(log k) sift, giving O(n log k) overall against the O(n log n) of a full sort, and O(k) space against O(n).

The subtlety is that Go's `container/heap` keeps the `Less`-minimum at the root, but we want the ordering-maximum (the worst kept row) there. The heap therefore reports the ordering-larger row as the `Less`-smaller one, inverting the comparison. After the scan drains, the heap holds the best `k` rows in heap order, not sorted order, so `Init` does one final O(k log k) sort to put them in ORDER BY order before dropping the leading `offset`. The operator is a blocking pipeline-breaker: `Init` cannot emit anything until it has seen every input row, because the last row read could be the new minimum.

NULL ordering rides on `CompareValues`, which places NULL before every concrete value. So a NULL sort key is the smallest possible key under ASC and the largest under DESC, and `top-1 ASC` over data containing a NULL key returns the NULL-keyed row.

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

Create `topn.go`:

```go
package planner

import (
	"container/heap"
	"errors"
	"fmt"
	"sort"
)

// SortKey specifies one column in an ORDER BY clause.
type SortKey struct {
	ColIndex int
	Desc     bool
}

// ErrInvalidTopN is returned when a Top-N operator is built with a negative
// limit or offset.
var ErrInvalidTopN = errors.New("invalid top-n parameters")

// TopNOperator returns the first (offset+limit) rows under a multi-key ORDER BY,
// then drops the leading offset, yielding at most limit rows. Unlike a Sort
// followed by a Limit, it keeps only offset+limit rows in memory using a bounded
// heap: O(n log k) time and O(k) space for n input rows and k = offset+limit.
// It is a blocking operator: Init drains the child.
type TopNOperator struct {
	child   Operator
	keys    []SortKey
	limit   int
	offset  int
	schema  Schema
	results []*Tuple
	pos     int
}

// NewTopN builds a Top-N operator. limit and offset must be non-negative.
func NewTopN(child Operator, keys []SortKey, limit, offset int) (*TopNOperator, error) {
	if limit < 0 || offset < 0 {
		return nil, fmt.Errorf("%w: limit=%d offset=%d", ErrInvalidTopN, limit, offset)
	}
	return &TopNOperator{
		child:  child,
		keys:   keys,
		limit:  limit,
		offset: offset,
		schema: child.Schema(),
	}, nil
}

func (o *TopNOperator) Schema() Schema { return o.schema }
func (o *TopNOperator) Close() error   { o.results = nil; return o.child.Close() }

// compareByKeys orders two tuples by the ORDER BY keys, honoring Desc per key.
// Incomparable or equal keys fall through to the next key; all-equal -> 0.
func compareByKeys(a, b *Tuple, keys []SortKey) int {
	for _, k := range keys {
		cmp, ok := CompareValues(a.Values[k.ColIndex], b.Values[k.ColIndex])
		if !ok || cmp == 0 {
			continue
		}
		if k.Desc {
			cmp = -cmp
		}
		return cmp
	}
	return 0
}

// topNHeap is a max-heap under the ORDER BY ordering: its root is the row that is
// "largest" (worst) under the ordering, so it is the one to evict on overflow.
type topNHeap struct {
	rows []*Tuple
	keys []SortKey
}

func (h *topNHeap) Len() int { return len(h.rows) }
func (h *topNHeap) Less(i, j int) bool {
	// container/heap keeps the Less-minimum at the root. We want the ordering-
	// maximum at the root, so report the ordering-larger row as the Less-smaller.
	return compareByKeys(h.rows[i], h.rows[j], h.keys) > 0
}
func (h *topNHeap) Swap(i, j int) { h.rows[i], h.rows[j] = h.rows[j], h.rows[i] }
func (h *topNHeap) Push(x any)    { h.rows = append(h.rows, x.(*Tuple)) }
func (h *topNHeap) Pop() any {
	old := h.rows
	n := len(old)
	t := old[n-1]
	h.rows = old[:n-1]
	return t
}

func (o *TopNOperator) Init() error {
	if err := o.child.Init(); err != nil {
		return err
	}
	capacity := o.offset + o.limit
	h := &topNHeap{keys: o.keys}
	for {
		t, err := o.child.Next()
		if err != nil {
			return err
		}
		if t == nil {
			break
		}
		if capacity == 0 {
			continue // limit+offset == 0: nothing can be emitted.
		}
		if h.Len() < capacity {
			heap.Push(h, t)
			continue
		}
		// Replace the current worst row if t is better (smaller under ordering).
		if compareByKeys(t, h.rows[0], o.keys) < 0 {
			h.rows[0] = t
			heap.Fix(h, 0)
		}
	}
	// The heap holds the best `capacity` rows in heap order. Sort ascending under
	// the ordering, then drop the offset.
	kept := h.rows
	sort.SliceStable(kept, func(i, j int) bool {
		return compareByKeys(kept[i], kept[j], o.keys) < 0
	})
	if o.offset < len(kept) {
		kept = kept[o.offset:]
	} else {
		kept = nil
	}
	o.results = kept
	o.pos = 0
	return nil
}

func (o *TopNOperator) Next() (*Tuple, error) {
	if o.pos >= len(o.results) {
		return nil, nil
	}
	t := o.results[o.pos]
	o.pos++
	return t, nil
}
```

### The runnable demo

The demo runs `ORDER BY score DESC, id ASC LIMIT 2` over a five-row table. Two rows tie at score 90, so the secondary key on `id` breaks the tie deterministically, yielding ids 2 then 4.

Create `cmd/demo/main.go`:

```go
package main

import (
	"fmt"
	"log"

	planner "example.com/top-n"
)

func main() {
	schema := planner.Schema{
		{Name: "id", Table: "t", Kind: planner.KindInt},
		{Name: "score", Table: "t", Kind: planner.KindInt},
		{Name: "name", Table: "t", Kind: planner.KindString},
	}
	rows := []*planner.Tuple{
		{Values: []planner.Value{planner.IntVal(1), planner.IntVal(50), planner.StrVal("a")}},
		{Values: []planner.Value{planner.IntVal(2), planner.IntVal(90), planner.StrVal("b")}},
		{Values: []planner.Value{planner.IntVal(3), planner.IntVal(70), planner.StrVal("c")}},
		{Values: []planner.Value{planner.IntVal(4), planner.IntVal(90), planner.StrVal("d")}},
		{Values: []planner.Value{planner.IntVal(5), planner.IntVal(10), planner.StrVal("e")}},
	}
	td := &planner.TableDef{Name: "t", Columns: schema, Rows: rows}

	// ORDER BY score DESC, id ASC LIMIT 2.
	keys := []planner.SortKey{{ColIndex: 1, Desc: true}, {ColIndex: 0, Desc: false}}
	op, err := planner.NewTopN(planner.NewSeqScan(td), keys, 2, 0)
	if err != nil {
		log.Fatal(err)
	}
	result, err := planner.Collect(op)
	if err != nil {
		log.Fatal(err)
	}
	fmt.Println("Top-2 by score DESC, id ASC:")
	for _, r := range result {
		fmt.Printf("  id=%s score=%s name=%s\n",
			r.Values[0].String(), r.Values[1].String(), r.Values[2].String())
	}
}
```

Run it:

```bash
go run ./cmd/demo
```

Expected output:

```
Top-2 by score DESC, id ASC:
  id=2 score=90 name=b
  id=4 score=90 name=d
```

### Tests

The table-driven test pins multi-key ordering with per-key direction, `offset` skipping the leading rows, a limit larger than the input (the heap never overflows, so all rows survive sorted), and a zero limit (nothing can be emitted). `TestNewTopNRejectsNegative` checks the sentinel error via `errors.Is`. `TestTopNNullSortsFirst` proves the NULL ordering: a `top-1 ASC` over data containing a NULL score returns the NULL-keyed row, because `CompareValues` orders NULL before every concrete value.

Create `topn_test.go`:

```go
package planner

import (
	"errors"
	"testing"
)

func topNFixture() *TableDef {
	schema := Schema{
		{Name: "id", Table: "t", Kind: KindInt},
		{Name: "score", Table: "t", Kind: KindInt},
		{Name: "name", Table: "t", Kind: KindString},
	}
	rows := []*Tuple{
		{Values: []Value{IntVal(1), IntVal(50), StrVal("a")}},
		{Values: []Value{IntVal(2), IntVal(90), StrVal("b")}},
		{Values: []Value{IntVal(3), IntVal(70), StrVal("c")}},
		{Values: []Value{IntVal(4), IntVal(90), StrVal("d")}},
		{Values: []Value{IntVal(5), IntVal(10), StrVal("e")}},
	}
	return &TableDef{Name: "t", Columns: schema, Rows: rows}
}

func TestTopNOperator(t *testing.T) {
	t.Parallel()

	cases := []struct {
		name    string
		keys    []SortKey
		limit   int
		offset  int
		wantIDs []int64
	}{
		{
			name:    "top2 by score desc then id asc",
			keys:    []SortKey{{ColIndex: 1, Desc: true}, {ColIndex: 0, Desc: false}},
			limit:   2,
			offset:  0,
			wantIDs: []int64{2, 4}, // scores 90,90 -> id 2 then 4
		},
		{
			name:    "offset skips the top row",
			keys:    []SortKey{{ColIndex: 1, Desc: true}, {ColIndex: 0, Desc: false}},
			limit:   2,
			offset:  1,
			wantIDs: []int64{4, 3}, // skip id2; next are id4(90) then id3(70)
		},
		{
			name:    "ascending by score",
			keys:    []SortKey{{ColIndex: 1, Desc: false}, {ColIndex: 0, Desc: false}},
			limit:   3,
			offset:  0,
			wantIDs: []int64{5, 1, 3}, // scores 10,50,70
		},
		{
			name:    "limit larger than input",
			keys:    []SortKey{{ColIndex: 0, Desc: false}},
			limit:   100,
			offset:  0,
			wantIDs: []int64{1, 2, 3, 4, 5},
		},
		{
			name:    "zero limit yields nothing",
			keys:    []SortKey{{ColIndex: 0, Desc: false}},
			limit:   0,
			offset:  0,
			wantIDs: nil,
		},
	}
	for _, tc := range cases {
		tc := tc
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			op, err := NewTopN(NewSeqScan(topNFixture()), tc.keys, tc.limit, tc.offset)
			if err != nil {
				t.Fatalf("NewTopN: %v", err)
			}
			rows, err := Collect(op)
			if err != nil {
				t.Fatalf("Collect: %v", err)
			}
			if len(rows) != len(tc.wantIDs) {
				t.Fatalf("got %d rows, want %d", len(rows), len(tc.wantIDs))
			}
			for i, want := range tc.wantIDs {
				got, _ := rows[i].Values[0].AsInt()
				if got != want {
					t.Errorf("row %d: id=%d, want %d", i, got, want)
				}
			}
		})
	}
}

func TestNewTopNRejectsNegative(t *testing.T) {
	t.Parallel()

	cases := []struct {
		name   string
		limit  int
		offset int
	}{
		{"negative limit", -1, 0},
		{"negative offset", 2, -3},
	}
	for _, tc := range cases {
		tc := tc
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			_, err := NewTopN(NewSeqScan(topNFixture()), nil, tc.limit, tc.offset)
			if !errors.Is(err, ErrInvalidTopN) {
				t.Fatalf("err = %v, want ErrInvalidTopN", err)
			}
		})
	}
}

func TestTopNNullSortsFirst(t *testing.T) {
	t.Parallel()

	schema := Schema{
		{Name: "id", Table: "t", Kind: KindInt},
		{Name: "score", Table: "t", Kind: KindInt},
	}
	rows := []*Tuple{
		{Values: []Value{IntVal(1), IntVal(50)}},
		{Values: []Value{IntVal(2), Null}}, // NULL score sorts first under ASC
		{Values: []Value{IntVal(3), IntVal(70)}},
	}
	td := &TableDef{Name: "t", Columns: schema, Rows: rows}

	op, err := NewTopN(NewSeqScan(td), []SortKey{{ColIndex: 1, Desc: false}}, 1, 0)
	if err != nil {
		t.Fatalf("NewTopN: %v", err)
	}
	out, err := Collect(op)
	if err != nil {
		t.Fatalf("Collect: %v", err)
	}
	if len(out) != 1 {
		t.Fatalf("got %d rows, want 1", len(out))
	}
	if !out[0].Values[1].IsNull() {
		t.Fatalf("top-1 ASC score = %v, want NULL (sorts first)", out[0].Values[1])
	}
	id, _ := out[0].Values[0].AsInt()
	if id != 2 {
		t.Fatalf("top-1 ASC id = %d, want 2 (the NULL-score row)", id)
	}
}
```

## Review

The operator is correct when it returns the same rows in the same order as a full sort followed by a limit, while only ever holding `offset+limit` rows. Confirm that multi-key ordering honors `Desc` per key and that ties break on the next key; that `offset` drops the leading rows after the final sort, not before; that a limit exceeding the input returns every row sorted; that a zero capacity yields nothing without touching the heap; and that `NewTopN` rejects a negative `limit` or `offset` with an error satisfying `errors.Is(err, ErrInvalidTopN)`. The NULL test pins the ordering contract: under ASC a NULL key is the smallest, so it survives as `top-1`. The heap inversion is the easiest thing to get wrong — the root must be the worst kept row so the cheap eviction keeps the best `k`; reversing it silently keeps the worst `k`.

## Resources

- [pkg.go.dev/container/heap](https://pkg.go.dev/container/heap) — `heap.Interface`, `heap.Push`, and `heap.Fix`, the bounded-heap primitives this operator drives.
- [pkg.go.dev/sort](https://pkg.go.dev/sort) — `sort.SliceStable`, used for the final ordering of the kept rows.
- [CMU 15-445 Query Execution](https://15445.courses.cs.cmu.edu/fall2024/slides/) — top-N and limit as physical operators in the execution engine.

---

Back to [08-query-engine-and-test-suite.md](08-query-engine-and-test-suite.md) | Next: [10-sort-merge-join.md](10-sort-merge-join.md)
