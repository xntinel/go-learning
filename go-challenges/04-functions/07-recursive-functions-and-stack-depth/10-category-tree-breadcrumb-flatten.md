# Exercise 10: Flatten a Category Tree into Breadcrumb Rows with a Depth Cap

**Nivel: Intermedio** — validacion rapida (un test corto).

A merchant catalog feed hands you a category hierarchy — Electronics > Phones >
Cases — and you need it flattened into one row per category, each carrying its
full breadcrumb path and depth. The feed comes from outside your team, so the
flattener refuses to descend past a caller-supplied depth cap instead of
trusting the feed to be shallow.

This module is fully self-contained: its own `go mod init`, the tree, the
flattener, and the tests inline.

## What you'll build

```text
categorybreadcrumb/        independent module: example.com/categorybreadcrumb
  go.mod                   go 1.24
  category.go              type Category; type Row; func Flatten
  category_test.go         small tree, nil root, exactly-at-cap, one-past-cap
```

Files: `category.go`, `category_test.go`.
Implement: `type Category struct { ID, Name string; Children []*Category }`,
`type Row struct { ID, Path string; Depth int }`, and
`func Flatten(root *Category, maxDepth int) ([]Row, error)`.
Test: a small tree with a known row count and breadcrumb path, a nil root
returning no rows, a chain exactly at `maxDepth` passing, and one node deeper
failing with `ErrMaxDepthExceeded` (checked via `errors.Is`).
Verify: `go test -count=1 ./...`

```bash
mkdir -p ~/go-exercises/categorybreadcrumb
cd ~/go-exercises/categorybreadcrumb
go mod init example.com/categorybreadcrumb
go mod edit -go=1.24
```

### Why the cap belongs in the recursion, not after it

The obvious-looking approach — flatten everything, then check whether any row's
depth exceeds the limit — still lets a maliciously deep feed build every row in
memory before you reject it. `Flatten` instead carries the depth down through
the recursive calls and returns `ErrMaxDepthExceeded` the instant a call would
go one level too deep, so an over-deep subtree is rejected before its rows are
ever constructed. The breadcrumb path itself is built the same way: each call
receives the parent's already-built path and appends its own name, so no call
needs to know its ancestors beyond what its caller handed it.

Create `category.go`:

```go
package category

import (
	"errors"
	"fmt"
)

// ErrMaxDepthExceeded is returned when the tree is deeper than the caller's
// allowed depth. Catalog feeds are imported from merchants, not authored by
// us, so a hard cap protects the flattener from a maliciously or accidentally
// deep category chain.
var ErrMaxDepthExceeded = errors.New("category: max depth exceeded")

// Category is one node of a product catalog hierarchy.
type Category struct {
	ID       string
	Name     string
	Children []*Category
}

// Row is one flattened line: the breadcrumb path down to this category and
// its depth (root is 0).
type Row struct {
	ID    string
	Path  string
	Depth int
}

// Flatten walks root and returns one Row per category, in a depth-first,
// parent-before-children order, refusing to descend past maxDepth.
func Flatten(root *Category, maxDepth int) ([]Row, error) {
	if root == nil {
		return nil, nil
	}
	return flatten(root, "", 0, maxDepth)
}

func flatten(n *Category, prefix string, depth int, maxDepth int) ([]Row, error) {
	if depth > maxDepth {
		return nil, fmt.Errorf("%s: %w", n.Name, ErrMaxDepthExceeded)
	}

	path := n.Name
	if prefix != "" {
		path = prefix + " > " + n.Name
	}

	rows := []Row{{ID: n.ID, Path: path, Depth: depth}}
	for _, c := range n.Children {
		childRows, err := flatten(c, path, depth+1, maxDepth)
		if err != nil {
			return nil, err
		}
		rows = append(rows, childRows...)
	}
	return rows, nil
}
```

### Tests

The table covers a small tree with a known row count, a nil root (no rows, no
error), a chain sitting exactly at `maxDepth` (passes), and one node past it
(fails). A second test checks the breadcrumb path text and depth on the
deepest row of a known tree.

Create `category_test.go`:

```go
package category

import (
	"errors"
	"fmt"
	"testing"
)

func chain(depth int) *Category {
	root := &Category{ID: "c0", Name: "L0"}
	cur := root
	for i := 1; i <= depth; i++ {
		child := &Category{ID: fmt.Sprintf("c%d", i), Name: fmt.Sprintf("L%d", i)}
		cur.Children = append(cur.Children, child)
		cur = child
	}
	return root
}

func TestFlatten(t *testing.T) {
	tests := []struct {
		name     string
		root     *Category
		maxDepth int
		wantLen  int
		wantErr  error
	}{
		{
			name: "small tree",
			root: &Category{
				ID: "1", Name: "Electronics",
				Children: []*Category{
					{ID: "2", Name: "Phones", Children: []*Category{
						{ID: "3", Name: "Cases"},
					}},
					{ID: "4", Name: "Laptops"},
				},
			},
			maxDepth: 5,
			wantLen:  4,
			wantErr:  nil,
		},
		{
			name:     "nil root",
			root:     nil,
			maxDepth: 5,
			wantLen:  0,
			wantErr:  nil,
		},
		{
			name:     "exactly at cap",
			root:     chain(3),
			maxDepth: 3,
			wantLen:  4,
			wantErr:  nil,
		},
		{
			name:     "one past cap",
			root:     chain(4),
			maxDepth: 3,
			wantLen:  0,
			wantErr:  ErrMaxDepthExceeded,
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			rows, err := Flatten(tc.root, tc.maxDepth)
			if tc.wantErr == nil && err != nil {
				t.Fatalf("Flatten() error = %v, want nil", err)
			}
			if tc.wantErr != nil {
				if !errors.Is(err, tc.wantErr) {
					t.Fatalf("Flatten() error = %v, want %v", err, tc.wantErr)
				}
				return
			}
			if len(rows) != tc.wantLen {
				t.Fatalf("Flatten() returned %d rows, want %d", len(rows), tc.wantLen)
			}
		})
	}
}

func TestFlattenBreadcrumbPath(t *testing.T) {
	root := &Category{
		ID: "1", Name: "Electronics",
		Children: []*Category{
			{ID: "2", Name: "Phones", Children: []*Category{
				{ID: "3", Name: "Cases"},
			}},
		},
	}

	rows, err := Flatten(root, 5)
	if err != nil {
		t.Fatalf("Flatten() error = %v", err)
	}

	want := "Electronics > Phones > Cases"
	got := rows[len(rows)-1].Path
	if got != want {
		t.Errorf("deepest row path = %q, want %q", got, want)
	}
	if rows[len(rows)-1].Depth != 2 {
		t.Errorf("deepest row depth = %d, want 2", rows[len(rows)-1].Depth)
	}
}
```

Run it: `go test -count=1 ./...`

## Review

`Flatten` is the same shape as the trusted filesystem walker earlier in this
lesson — recurse over children, base case is a leaf — but with the depth
threaded through every call so a hostile or malformed feed is rejected before
its rows are built, not after. Threading the breadcrumb path down rather than
reconstructing it from an ancestor list keeps each call ignorant of anything
above its immediate parent, which is what makes the recursion straightforward
to reason about.

## Resources

- [Go Specification: Struct types](https://go.dev/ref/spec#Struct_types)
- [errors.Is](https://pkg.go.dev/errors#Is)

---

Back to [00-concepts.md](00-concepts.md) | Prev: [09-no-tco-accumulator-vs-loop.md](09-no-tco-accumulator-vs-loop.md) | Next: [11-nested-config-redaction-depth-guard.md](11-nested-config-redaction-depth-guard.md)
