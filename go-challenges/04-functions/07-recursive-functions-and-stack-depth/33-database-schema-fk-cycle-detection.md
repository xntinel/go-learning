# Exercise 33: Detect Circular Foreign Key Dependencies in Schema

**Nivel: Avanzado** — validacion normal (tabla de casos, incluye borde o concurrencia).

A database schema's foreign keys form a directed graph: an edge from
table `A` to table `B` means "`A` has a foreign key referencing `B`, so
`B` must exist before `A` can." Working out a safe migration order —
create referenced tables before the tables that reference them — is a
topological sort, and detecting whether that order even exists is the
classic three-color depth-first search: a table is white before it is
visited, gray while it is on the current path, and black once every table
it depends on has been fully processed. Reaching a gray table a second
time, before it has turned black, means the current path has looped back
on itself — two or more tables that cannot be created in any order
without deferring a constraint. The one case that is *not* a problem, and
that a naive implementation gets wrong, is a table referencing itself:
`employees.manager_id → employees` is a completely ordinary self-reference,
and it must not be reported as a migration-blocking cycle.

This module is fully self-contained: its own `go mod init`, the detector
inline, its own demo and tests.

## What you'll build

```text
fkgraph/                      independent module: example.com/fkgraph
  go.mod                        go 1.24
  fkgraph.go                     DetectCycles (recursive, three-color DFS); TopoOrder (recursive post-order)
  fkgraph_test.go                empty on DAG, two-table cycle found, self-reference ignored, safe order matches dependency constraints, TopoOrder fails on cycle
  cmd/
    demo/
      main.go                     a schema with an accidental two-table cycle, then the same schema fixed
```

- Files: `fkgraph.go`, `cmd/demo/main.go`, `fkgraph_test.go`.
- Implement: `DetectCycles(schema map[string][]string) [][]string` using a recursive three-color DFS (`white`/`gray`/`black`), and `TopoOrder(schema map[string][]string) ([]string, error)` returning a safe creation order or `ErrCycleDetected`.
- Test: an acyclic schema reports no cycles; a genuine two-table cycle is found and reported; a self-referencing table is not treated as a cycle; `TopoOrder`'s result respects every non-self dependency edge; `TopoOrder` fails with `errors.Is(err, ErrCycleDetected)` on a cyclic schema.
- Verify: `go test -count=1 ./...`

Set up the module:

```bash
mkdir -p ~/go-exercises/fkgraph/cmd/demo
cd ~/go-exercises/fkgraph
go mod init example.com/fkgraph
go mod edit -go=1.24
```

### Three colors, one skip, and why the skip has to come first

`visit` marks a table gray on entry and black on exit, keeping every gray
table's name on an explicit `stack` slice for the duration. The dispatch
on a dependency's color is exactly the textbook three-color rule: white
means "not explored yet, recurse"; black means "already fully processed
via some other path, definitely no cycle through here, skip"; gray means
"this table is an ancestor of the current call on the very path we are
walking right now" — which is precisely what a cycle is, and
`extractCycle` reconstructs it by slicing `stack` from that ancestor's
position to the top and repeating it at the end to show the loop closing.

The one line that is easy to leave out, and that changes the answer for
an extremely common real schema, is `if dep == table { continue }` —
checked *before* the color switch, not folded into it. Without that
check, a self-referencing table hits the `gray` case on its own name
(it marked itself gray one line earlier) and is reported as a
single-table "cycle," which is not a real migration problem: a
`CREATE TABLE` statement that both defines a column and constrains it to
reference the same table it belongs to has no ordering requirement to
violate. Reflecting that in the algorithm — rather than filtering
single-table results back out afterward — is what keeps `DetectCycles`
correct without a second pass.

Create `fkgraph.go`:

```go
// Package fkgraph detects circular foreign key dependencies in a database
// schema and computes a safe table creation (migration) order when none
// exist. A schema is a directed graph -- an edge from table A to table B
// means "A has a foreign key referencing B, so B must exist first" -- and
// cycle detection over it is the classic three-color depth-first search:
// white (unvisited), gray (on the current DFS path), black (fully
// processed). A gray table reached again while still gray is a genuine
// cycle: two or more tables that cannot be created in any order without
// deferring a constraint. A table that merely references itself (a
// self-referencing foreign key, e.g. an employee's own manager) is not
// treated as a cycle: nothing prevents creating a single table whose own
// constraint points at itself.
package fkgraph

import (
	"errors"
	"fmt"
	"sort"
)

// ErrCycleDetected is returned when the schema graph contains a circular
// foreign key dependency across two or more distinct tables.
var ErrCycleDetected = errors.New("fkgraph: circular foreign key dependency detected")

const (
	white = 0
	gray  = 1
	black = 2
)

// DetectCycles returns every circular foreign key dependency in schema
// (table -> tables it references), each reported as the sequence of
// tables in the cycle starting and ending at the same table. A table that
// only references itself is not reported. Traversal order is
// deterministic (tables and their dependencies are visited in sorted
// order), so results are reproducible across runs.
func DetectCycles(schema map[string][]string) [][]string {
	d := &detector{schema: schema, color: make(map[string]int)}
	for _, table := range sortedKeys(schema) {
		if d.color[table] == white {
			d.visit(table)
		}
	}
	return d.cycles
}

type detector struct {
	schema map[string][]string
	color  map[string]int
	stack  []string
	cycles [][]string
}

func (d *detector) visit(table string) {
	d.color[table] = gray
	d.stack = append(d.stack, table)

	for _, dep := range sortedStrings(d.schema[table]) {
		if dep == table {
			continue // self-reference: not a cross-table ordering problem
		}
		switch d.color[dep] {
		case white:
			d.visit(dep)
		case gray:
			d.cycles = append(d.cycles, extractCycle(d.stack, dep))
		case black:
			// already fully processed via some other path; no cycle here
		}
	}

	d.color[table] = black
	d.stack = d.stack[:len(d.stack)-1]
}

// extractCycle returns the portion of stack from start's position to the
// top, with start repeated at the end to show the loop closing.
func extractCycle(stack []string, start string) []string {
	for i, t := range stack {
		if t == start {
			cycle := append([]string(nil), stack[i:]...)
			return append(cycle, start)
		}
	}
	return nil
}

// TopoOrder returns a safe table creation order for schema -- every
// table's dependencies appear before the table itself -- or
// ErrCycleDetected if the schema contains a circular dependency.
func TopoOrder(schema map[string][]string) ([]string, error) {
	if cycles := DetectCycles(schema); len(cycles) > 0 {
		return nil, fmt.Errorf("%w: %v", ErrCycleDetected, cycles[0])
	}

	visited := make(map[string]bool)
	var order []string
	var visit func(table string)
	visit = func(table string) {
		if visited[table] {
			return
		}
		visited[table] = true
		for _, dep := range sortedStrings(schema[table]) {
			if dep != table {
				visit(dep)
			}
		}
		order = append(order, table)
	}
	for _, table := range sortedKeys(schema) {
		visit(table)
	}
	return order, nil
}

func sortedKeys(schema map[string][]string) []string {
	keys := make([]string, 0, len(schema))
	for k := range schema {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	return keys
}

func sortedStrings(in []string) []string {
	out := append([]string(nil), in...)
	sort.Strings(out)
	return out
}
```

### The runnable demo

The demo starts from a schema with an accidental two-table cycle
(`customers` and `orders` reference each other) alongside a normal
self-reference (`employees`), shows the cycle detected and `TopoOrder`
failing, then removes the accidental back-reference and shows a valid
migration order.

Create `cmd/demo/main.go`:

```go
package main

import (
	"fmt"

	"example.com/fkgraph"
)

func main() {
	// "customers" and "orders" accidentally reference each other:
	// customers.last_order_id -> orders, orders.customer_id -> customers.
	// employees.manager_id -> employees is a normal self-reference.
	broken := map[string][]string{
		"employees":  {"employees"},
		"orders":     {"customers"},
		"customers":  {"orders"},
		"line_items": {"orders", "products"},
		"products":   {},
	}

	cycles := fkgraph.DetectCycles(broken)
	fmt.Println("cycles found:", cycles)

	_, err := fkgraph.TopoOrder(broken)
	fmt.Println("migration order:", err)

	// Remove the accidental back-reference: customers no longer points at
	// orders, so the schema is a clean DAG.
	fixed := map[string][]string{
		"employees":  {"employees"},
		"orders":     {"customers"},
		"customers":  {},
		"line_items": {"orders", "products"},
		"products":   {},
	}
	order, err := fkgraph.TopoOrder(fixed)
	if err != nil {
		panic(err)
	}
	fmt.Println("migration order:", order)
}
```

Run it:

```bash
go run ./cmd/demo
```

Expected output:

```
cycles found: [[customers orders customers]]
migration order: fkgraph: circular foreign key dependency detected: [customers orders customers]
migration order: [customers employees orders products line_items]
```

### Tests

`TestDetectCyclesEmptyOnDAG` and `TestDetectCyclesFindsTwoTableCycle`
check the basic detection contract, including the exact reported cycle
path. `TestDetectCyclesIgnoresSelfReference` is the edge case this
exercise is built around: a table referencing only itself must report
zero cycles. `TestTopoOrderSucceedsOnDAG` checks the general property a
valid order must have — every dependency precedes its dependent — rather
than one hardcoded slice, so it stays correct regardless of exactly how
ties are broken. `TestTopoOrderFailsOnCycle` confirms the error path.

Create `fkgraph_test.go`:

```go
package fkgraph

import (
	"errors"
	"reflect"
	"testing"
)

func TestDetectCyclesEmptyOnDAG(t *testing.T) {
	t.Parallel()

	schema := map[string][]string{
		"orders":     {"customers"},
		"customers":  {},
		"line_items": {"orders", "products"},
		"products":   {},
	}
	got := DetectCycles(schema)
	if len(got) != 0 {
		t.Fatalf("DetectCycles() = %v, want none", got)
	}
}

func TestDetectCyclesFindsTwoTableCycle(t *testing.T) {
	t.Parallel()

	schema := map[string][]string{
		"orders":    {"customers"},
		"customers": {"orders"},
	}
	got := DetectCycles(schema)
	if len(got) != 1 {
		t.Fatalf("DetectCycles() = %v, want exactly one cycle", got)
	}
	want := []string{"customers", "orders", "customers"}
	if !reflect.DeepEqual(got[0], want) {
		t.Fatalf("DetectCycles()[0] = %v, want %v", got[0], want)
	}
}

func TestDetectCyclesIgnoresSelfReference(t *testing.T) {
	t.Parallel()

	// employees.manager_id -> employees is a normal, safe self-reference:
	// the table can always be created before its own constraint is
	// checked, so it must not be reported as a cycle.
	schema := map[string][]string{
		"employees": {"employees"},
	}
	got := DetectCycles(schema)
	if len(got) != 0 {
		t.Fatalf("DetectCycles() = %v, want none (self-reference is not a cycle)", got)
	}
}

func TestTopoOrderSucceedsOnDAG(t *testing.T) {
	t.Parallel()

	schema := map[string][]string{
		"employees":  {"employees"},
		"orders":     {"customers"},
		"customers":  {},
		"line_items": {"orders", "products"},
		"products":   {},
	}
	got, err := TopoOrder(schema)
	if err != nil {
		t.Fatalf("TopoOrder() error = %v", err)
	}

	pos := make(map[string]int, len(got))
	for i, table := range got {
		pos[table] = i
	}
	// Every table must come after everything it depends on.
	for table, deps := range schema {
		for _, dep := range deps {
			if dep == table {
				continue
			}
			if pos[dep] >= pos[table] {
				t.Errorf("%s (pos %d) does not precede %s (pos %d)", dep, pos[dep], table, pos[table])
			}
		}
	}
	if len(got) != len(schema) {
		t.Fatalf("len(TopoOrder()) = %d, want %d", len(got), len(schema))
	}
}

func TestTopoOrderFailsOnCycle(t *testing.T) {
	t.Parallel()

	schema := map[string][]string{
		"orders":    {"customers"},
		"customers": {"orders"},
	}
	_, err := TopoOrder(schema)
	if !errors.Is(err, ErrCycleDetected) {
		t.Fatalf("TopoOrder() error = %v, want %v", err, ErrCycleDetected)
	}
}
```

## Review

`DetectCycles` is correct when it reports every genuine cross-table cycle
and nothing else, and `TopoOrder` is correct when its result satisfies
"every dependency precedes its dependent" for every non-self edge in the
schema, and fails cleanly whenever no such order exists.
`TestDetectCyclesIgnoresSelfReference` is the test that would fail on the
tempting simplification of running an unmodified textbook three-color DFS
with no special case at all: it would report `employees` as its own
one-table cycle, which is technically what a gray-revisits-gray check
finds, but not what the exercise actually needs to flag — a real schema
with dozens of ordinary self-referencing tables would drown the genuine,
migration-blocking cycles in false positives. `TestTopoOrderSucceedsOnDAG`
is deliberately written against the *property* a valid order must have
rather than one specific expected slice, because a correct topological
sort is not unique — asserting an exact ordering would make the test
fail on an equally-correct implementation that simply broke ties in
sorted-key iteration differently.

## Resources

- [Wikipedia: Topological sorting](https://en.wikipedia.org/wiki/Topological_sorting)
- [Wikipedia: Depth-first search (three-color / vertex coloring)](https://en.wikipedia.org/wiki/Depth-first_search)
- [PostgreSQL: Foreign Keys documentation](https://www.postgresql.org/docs/current/tutorial-fk.html)

---

Back to [00-concepts.md](00-concepts.md) | Prev: [32-protobuf-message-nesting-validation.md](32-protobuf-message-nesting-validation.md) | Next: [34-filesystem-inode-hardlink-detection.md](34-filesystem-inode-hardlink-detection.md)
