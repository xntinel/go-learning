# Exercise 27: Aggregate Metrics from Distributed Trace Span Trees

**Nivel: Intermedio** — validacion rapida (un test corto).

A distributed trace arrives at a collector as a flat list of spans, each
naming its own parent, sent by whichever client SDK happened to instrument
that hop of the request. Reassembling that list into a tree and summing
latency and error counts over it is naturally recursive: a span's
contribution is its own duration plus whatever its children contribute.
The catch is that the tree is not data you own — it is reconstructed from
spans emitted by many uncoordinated, sometimes buggy clients, so nothing
guarantees it is actually a tree. A re-parenting bug can turn two spans
into a cycle; a chain of retries can turn "deep" into "deep enough to
matter." The aggregation has to recurse the way the problem wants, while
refusing to trust the shape of its input past a configured depth.

This module is fully self-contained: its own `go mod init`, the
aggregation inline, its own demo and tests.

## What you'll build

```text
spanagg/                      independent module: example.com/spanagg
  go.mod                        go 1.24
  spanagg.go                     type Span; BuildTree; Metrics; Aggregate (recursive, depth-guarded)
  spanagg_test.go                build+aggregate happy path, unknown parent, multiple roots, no root, long chain, cycle, bad maxDepth
  cmd/
    demo/
      main.go                     aggregates a small real trace, then rejects a manufactured cyclic one
```

- Files: `spanagg.go`, `cmd/demo/main.go`, `spanagg_test.go`.
- Implement: `Span{ID, ParentID, Name string; DurationMS int64; IsError bool; Children []*Span}`, `BuildTree(spans []Span) (*Span, error)`, `Metrics{SpanCount int; TotalDurationMS int64; ErrorCount int; MaxDepthSeen int}`, and `Aggregate(root *Span, maxDepth int) (Metrics, error)` recursing with a depth counter.
- Test: a happy-path build-then-aggregate with known totals; an unknown parent reference; multiple roots; no root at all; a 300-span chain rejected past `maxDepth`; a manufactured parent/child cycle rejected the same way; an invalid `maxDepth`.
- Verify: `go test -count=1 ./...`

Set up the module:

```bash
mkdir -p ~/go-exercises/spanagg/cmd/demo
cd ~/go-exercises/spanagg
go mod init example.com/spanagg
go mod edit -go=1.24
```

### The depth guard defends against cycles too, not just long chains

`Aggregate` recurses over `Children`, and its only defense is a plain
integer depth counter checked at the top of every call: once `depth`
exceeds `maxDepth`, it returns `ErrMaxDepthExceeded` instead of descending
further. It is worth noticing that this single check handles two different
failure shapes with no extra code. A pathologically long span chain — a
retry storm that re-parents each attempt under the last — is caught
because the counter eventually exceeds `maxDepth` on a real, finite tree.
A **cycle**, which a naive recursive walk would follow forever (each call
recursing into a child that eventually recurses back into an ancestor,
with no base case ever reached), is caught for exactly the same reason:
the depth counter increments on every call regardless of whether the
underlying structure is finite, so it exceeds `maxDepth` and returns an
error long before the goroutine stack is anywhere near its limit. No
separate "have I visited this span before" set is needed; the recursion's
own progress measure is the visited-set.

`BuildTree` does the separate, non-recursive job of turning the flat span
list the collector actually receives into the `*Span` tree `Aggregate`
walks, validating along the way that every span's parent reference
resolves to a span that exists and that exactly one span is the root.

Create `spanagg.go`:

```go
// Package spanagg builds a distributed trace's span tree from a flat list
// of spans and recursively aggregates latency and error metrics over it.
// Traces arrive from many uncoordinated client SDKs, and a buggy one can
// send a span that re-parents itself into a cycle, or a chain so long it
// would blow the stack of a naive unbounded recursive walk. Aggregate
// enforces a maximum depth so a malformed trace is rejected cleanly instead
// of hanging or crashing the collector.
package spanagg

import (
	"errors"
	"fmt"
	"sort"
)

// ErrMaxDepthExceeded is returned when the span tree nests deeper than the
// configured maximum.
var ErrMaxDepthExceeded = errors.New("spanagg: span tree exceeds maximum depth")

// Span is one span in a distributed trace.
type Span struct {
	ID         string
	ParentID   string
	Name       string
	DurationMS int64
	IsError    bool
	Children   []*Span
}

// BuildTree assembles a span tree from a flat list of spans, as received
// from a tracing collector in arbitrary order, by linking each span into
// its parent's Children slice. It returns the root span (the one with an
// empty ParentID).
func BuildTree(spans []Span) (*Span, error) {
	byID := make(map[string]*Span, len(spans))
	for i := range spans {
		s := spans[i]
		s.Children = nil
		byID[s.ID] = &s
	}

	var root *Span
	for _, s := range byID {
		if s.ParentID == "" {
			if root != nil {
				return nil, fmt.Errorf("spanagg: multiple root spans (%s and %s)", root.ID, s.ID)
			}
			root = s
			continue
		}
		parent, ok := byID[s.ParentID]
		if !ok {
			return nil, fmt.Errorf("spanagg: span %s references unknown parent %s", s.ID, s.ParentID)
		}
		parent.Children = append(parent.Children, s)
	}
	if root == nil {
		return nil, errors.New("spanagg: no root span found (every span has a parent)")
	}

	for _, s := range byID {
		sort.Slice(s.Children, func(i, j int) bool { return s.Children[i].ID < s.Children[j].ID })
	}
	return root, nil
}

// Metrics summarizes a span tree.
type Metrics struct {
	SpanCount       int
	TotalDurationMS int64
	ErrorCount      int
	MaxDepthSeen    int
}

// Aggregate recursively walks the span tree rooted at root, summing
// duration and error counts and tracking the deepest level actually
// visited, while enforcing maxDepth (root is depth 1). A malformed trace --
// an accidental parent/child cycle from a buggy client, or a
// pathologically long span chain -- is rejected the moment recursion
// reaches maxDepth+1, rather than recursing forever or overflowing the
// goroutine stack.
func Aggregate(root *Span, maxDepth int) (Metrics, error) {
	if maxDepth < 1 {
		return Metrics{}, fmt.Errorf("spanagg: maxDepth must be >= 1, got %d", maxDepth)
	}
	var m Metrics
	if err := aggregate(root, 1, maxDepth, &m); err != nil {
		return Metrics{}, err
	}
	return m, nil
}

func aggregate(span *Span, depth, maxDepth int, m *Metrics) error {
	if depth > maxDepth {
		return fmt.Errorf("%w: span %s at depth %d", ErrMaxDepthExceeded, span.ID, depth)
	}
	m.SpanCount++
	m.TotalDurationMS += span.DurationMS
	if span.IsError {
		m.ErrorCount++
	}
	if depth > m.MaxDepthSeen {
		m.MaxDepthSeen = depth
	}
	for _, child := range span.Children {
		if err := aggregate(child, depth+1, maxDepth, m); err != nil {
			return err
		}
	}
	return nil
}
```

### The runnable demo

The demo aggregates a small, real-looking trace, then manufactures a
cyclic pair of spans — simulating a client bug that re-parents two spans
onto each other — and shows `Aggregate` rejecting it cleanly.

Create `cmd/demo/main.go`:

```go
package main

import (
	"fmt"

	"example.com/spanagg"
)

func main() {
	spans := []spanagg.Span{
		{ID: "root", ParentID: "", Name: "http-request", DurationMS: 100},
		{ID: "db", ParentID: "root", Name: "db-query", DurationMS: 30},
		{ID: "cache", ParentID: "root", Name: "cache-lookup", DurationMS: 5},
		{ID: "sql", ParentID: "db", Name: "sql-exec", DurationMS: 20, IsError: true},
	}

	root, err := spanagg.BuildTree(spans)
	if err != nil {
		panic(err)
	}

	metrics, err := spanagg.Aggregate(root, 5)
	if err != nil {
		panic(err)
	}
	fmt.Printf("spans=%d total_ms=%d errors=%d max_depth=%d\n",
		metrics.SpanCount, metrics.TotalDurationMS, metrics.ErrorCount, metrics.MaxDepthSeen)

	// Simulate a malformed trace: a buggy client re-parented span "a" onto
	// span "b" and "b" onto "a", forming a cycle instead of a tree.
	a := &spanagg.Span{ID: "a", DurationMS: 1}
	b := &spanagg.Span{ID: "b", DurationMS: 1}
	a.Children = []*spanagg.Span{b}
	b.Children = []*spanagg.Span{a}

	_, err = spanagg.Aggregate(a, 10)
	fmt.Println("cyclic trace result:", err)
}
```

Run it:

```bash
go run ./cmd/demo
```

Expected output:

```
spans=4 total_ms=155 errors=1 max_depth=3
cyclic trace result: spanagg: span tree exceeds maximum depth: span a at depth 11
```

### Tests

`TestBuildTreeAndAggregate` is the happy path: four spans, three levels
deep, with known totals. `TestBuildTreeRejectsUnknownParent`,
`TestBuildTreeRejectsMultipleRoots`, and `TestBuildTreeRejectsNoRoot` cover
the malformed flat-list shapes `BuildTree` must reject before a tree is
even handed to `Aggregate`. `TestAggregateRejectsLongChainPastMaxDepth`
builds a 300-span linear chain and confirms the guard fires.
`TestAggregateRejectsCycle` is the test this exercise exists for: two
spans manually wired into a cycle must still make `Aggregate` return
promptly with `ErrMaxDepthExceeded`, proving the depth counter alone is
sufficient to stop a walk that would otherwise never terminate.

Create `spanagg_test.go`:

```go
package spanagg

import (
	"errors"
	"fmt"
	"testing"
)

func TestBuildTreeAndAggregate(t *testing.T) {
	t.Parallel()

	spans := []Span{
		{ID: "root", ParentID: "", DurationMS: 100},
		{ID: "db", ParentID: "root", DurationMS: 30},
		{ID: "cache", ParentID: "root", DurationMS: 5},
		{ID: "sql", ParentID: "db", DurationMS: 20, IsError: true},
	}

	root, err := BuildTree(spans)
	if err != nil {
		t.Fatalf("BuildTree() error = %v", err)
	}

	metrics, err := Aggregate(root, 5)
	if err != nil {
		t.Fatalf("Aggregate() error = %v", err)
	}
	want := Metrics{SpanCount: 4, TotalDurationMS: 155, ErrorCount: 1, MaxDepthSeen: 3}
	if metrics != want {
		t.Fatalf("Aggregate() = %+v, want %+v", metrics, want)
	}
}

func TestBuildTreeRejectsUnknownParent(t *testing.T) {
	t.Parallel()

	spans := []Span{
		{ID: "root", ParentID: ""},
		{ID: "orphan", ParentID: "missing"},
	}
	if _, err := BuildTree(spans); err == nil {
		t.Fatal("expected error for unknown parent reference")
	}
}

func TestBuildTreeRejectsMultipleRoots(t *testing.T) {
	t.Parallel()

	spans := []Span{
		{ID: "root1", ParentID: ""},
		{ID: "root2", ParentID: ""},
	}
	if _, err := BuildTree(spans); err == nil {
		t.Fatal("expected error for multiple root spans")
	}
}

func TestBuildTreeRejectsNoRoot(t *testing.T) {
	t.Parallel()

	spans := []Span{
		{ID: "a", ParentID: "b"},
		{ID: "b", ParentID: "a"},
	}
	if _, err := BuildTree(spans); err == nil {
		t.Fatal("expected error when no span has an empty ParentID")
	}
}

func TestAggregateRejectsLongChainPastMaxDepth(t *testing.T) {
	t.Parallel()

	// Build a 300-span linear chain: a malformed or pathological trace far
	// deeper than any real request should ever nest.
	var root *Span
	var tail *Span
	for i := range 300 {
		s := &Span{ID: fmt.Sprintf("s%d", i), DurationMS: 1}
		if root == nil {
			root = s
		} else {
			tail.Children = []*Span{s}
		}
		tail = s
	}

	_, err := Aggregate(root, 50)
	if !errors.Is(err, ErrMaxDepthExceeded) {
		t.Fatalf("Aggregate() error = %v, want %v", err, ErrMaxDepthExceeded)
	}
}

func TestAggregateRejectsCycle(t *testing.T) {
	t.Parallel()

	// A buggy client can re-parent a span into a cycle; Aggregate must
	// still terminate, via the depth guard, instead of recursing forever.
	a := &Span{ID: "a", DurationMS: 1}
	b := &Span{ID: "b", DurationMS: 1}
	a.Children = []*Span{b}
	b.Children = []*Span{a}

	_, err := Aggregate(a, 10)
	if !errors.Is(err, ErrMaxDepthExceeded) {
		t.Fatalf("Aggregate() error = %v, want %v", err, ErrMaxDepthExceeded)
	}
}

func TestAggregateRejectsInvalidMaxDepth(t *testing.T) {
	t.Parallel()

	root := &Span{ID: "root"}
	if _, err := Aggregate(root, 0); err == nil {
		t.Fatal("expected error for maxDepth < 1")
	}
}
```

## Review

`Aggregate` is correct when it sums exactly the spans on a well-formed
tree within the depth budget, and fails cleanly — never hanging, never
overflowing the stack — on anything malformed enough to nest past that
budget. `TestAggregateRejectsCycle` is the test that would fail (by
hanging or by a runtime stack-overflow crash) on a version of this
exercise that recurses on `Children` without any depth accounting at all:
it looks correct on every well-formed test trace and only breaks in
production, against exactly the input this exercise is about — a
malformed trace from a client you do not control. The mistake this
exercise targets is treating "the tree is built from `[]*Span` pointers I
control the shape of" as license to skip the depth guard; the pointers are
yours, but the data that shaped them came from the network.

## Resources

- [OpenTelemetry: Traces specification](https://opentelemetry.io/docs/specs/otel/trace/)
- [OpenTelemetry: Span data model](https://opentelemetry.io/docs/specs/otel/trace/api/#span)
- [Go Specification: Function declarations](https://go.dev/ref/spec#Function_declarations)

---

Back to [00-concepts.md](00-concepts.md) | Prev: [26-state-machine-reachability-analysis.md](26-state-machine-reachability-analysis.md) | Next: [28-merkle-tree-verification-memoized.md](28-merkle-tree-verification-memoized.md)
