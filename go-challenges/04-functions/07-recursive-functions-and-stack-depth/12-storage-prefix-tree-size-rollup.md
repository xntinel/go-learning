# Exercise 12: Roll Up Object Storage Usage over a Prefix Tree, Depth-Capped

**Nivel: Intermedio** — validacion rapida (un test corto).

Object keys in a bucket encode "directory" prefixes the same way a filesystem
path does — `logs/2024/01/01/access.log` — except nothing stops a key from
nesting arbitrarily deep. This module builds a usage-report rollup over an
in-memory prefix tree: total bytes and object count, computed bottom-up, with
a hard cap on how deep the recursion will follow a prefix.

This module is fully self-contained: its own `go mod init`, the tree and the
rollup inline.

## What you'll build

```text
storagerollup/               independent module: example.com/storagerollup
  go.mod                      go 1.24
  storagetree.go               type Node; func Rollup
  storagetree_test.go          small tree, single object, exactly-at-cap, one-past-cap
```

Files: `storagetree.go`, `storagetree_test.go`.
Implement: `type Node struct { Name string; Size int64; Children []*Node }`
and `func Rollup(n *Node, maxDepth int) (totalSize int64, objectCount int, err error)`.
Test: a small tree with a known total and object count, a single leaf object,
a chain exactly at `maxDepth` passing, and one node deeper failing with
`ErrMaxDepthExceeded`.
Verify: `go test -count=1 ./...`

```bash
mkdir -p ~/go-exercises/storagerollup
cd ~/go-exercises/storagerollup
go mod init example.com/storagerollup
go mod edit -go=1.24
```

### A leaf is defined by shape, not by a flag

`Node` has no `IsLeaf` field: a node with no children *is* a leaf, and its
`Size` is the only one that means anything (an internal node's `Size` field is
simply unused, since its total is computed from its children). This mirrors
how object storage actually works — a prefix is not a real directory, it is
just a common substring of keys, so "is this an object or a prefix" is a
question about whether anything nests under it, not a stored attribute.
`Rollup` recurses depth-first, summing children's totals on the way back up,
and refuses to go past `maxDepth` for the same reason the other bounded
exercises in this lesson do: nothing in a bucket's key structure guarantees a
shallow tree.

Create `storagetree.go`:

```go
package storagetree

import (
	"errors"
	"fmt"
)

// ErrMaxDepthExceeded is returned when a prefix nests deeper than maxDepth.
// Object keys in a bucket can encode arbitrarily deep "directory" prefixes
// (a/b/c/d/.../object), so a usage report that recurses over them needs a
// hard cap the same way any recursion over externally supplied structure does.
var ErrMaxDepthExceeded = errors.New("storagetree: max depth exceeded")

// Node is one entry in an in-memory object-storage prefix tree. A leaf (no
// Children) is an object with a known Size; an internal node is a prefix
// whose Size is ignored and computed from its children instead.
type Node struct {
	Name     string
	Size     int64
	Children []*Node
}

// Rollup computes the total byte size and object count for the subtree
// rooted at n, refusing to descend past maxDepth.
func Rollup(n *Node, maxDepth int) (totalSize int64, objectCount int, err error) {
	return rollup(n, 0, maxDepth)
}

func rollup(n *Node, depth int, maxDepth int) (int64, int, error) {
	if depth > maxDepth {
		return 0, 0, fmt.Errorf("%s: %w", n.Name, ErrMaxDepthExceeded)
	}

	if len(n.Children) == 0 {
		return n.Size, 1, nil
	}

	var total int64
	var objects int
	for _, c := range n.Children {
		st, so, err := rollup(c, depth+1, maxDepth)
		if err != nil {
			return 0, 0, err
		}
		total += st
		objects += so
	}
	return total, objects, nil
}
```

### Tests

The table covers a small two-level tree with a known total size and object
count, a single-object tree, a chain sitting exactly at `maxDepth`, and one
node past it. `chain` builds a linear prefix chain of the given depth with a
single leaf object of size 10 at the end.

Create `storagetree_test.go`:

```go
package storagetree

import (
	"errors"
	"testing"
)

func chain(depth int) *Node {
	root := &Node{Name: "p0"}
	cur := root
	for i := 1; i <= depth; i++ {
		child := &Node{Name: "p"}
		cur.Children = []*Node{child}
		cur = child
	}
	cur.Size = 10
	return root
}

func TestRollup(t *testing.T) {
	tests := []struct {
		name        string
		root        *Node
		maxDepth    int
		wantSize    int64
		wantObjects int
		wantErr     error
	}{
		{
			name: "small tree",
			root: &Node{
				Name: "bucket",
				Children: []*Node{
					{Name: "a.txt", Size: 100},
					{Name: "logs", Children: []*Node{
						{Name: "1.log", Size: 20},
						{Name: "2.log", Size: 30},
					}},
				},
			},
			maxDepth:    5,
			wantSize:    150,
			wantObjects: 3,
			wantErr:     nil,
		},
		{
			name:        "single object",
			root:        &Node{Name: "only.bin", Size: 42},
			maxDepth:    5,
			wantSize:    42,
			wantObjects: 1,
			wantErr:     nil,
		},
		{
			name:        "exactly at cap",
			root:        chain(3),
			maxDepth:    3,
			wantSize:    10,
			wantObjects: 1,
			wantErr:     nil,
		},
		{
			name:     "one past cap",
			root:     chain(4),
			maxDepth: 3,
			wantErr:  ErrMaxDepthExceeded,
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			size, objects, err := Rollup(tc.root, tc.maxDepth)
			if tc.wantErr != nil {
				if !errors.Is(err, tc.wantErr) {
					t.Fatalf("Rollup() error = %v, want %v", err, tc.wantErr)
				}
				return
			}
			if err != nil {
				t.Fatalf("Rollup() error = %v, want nil", err)
			}
			if size != tc.wantSize {
				t.Errorf("Rollup() size = %d, want %d", size, tc.wantSize)
			}
			if objects != tc.wantObjects {
				t.Errorf("Rollup() objects = %d, want %d", objects, tc.wantObjects)
			}
		})
	}
}
```

Run it: `go test -count=1 ./...`

## Review

The rollup is a textbook bottom-up fold: a leaf returns its own size and a
count of one, an internal node returns the sum of what its children reported.
The depth cap is orthogonal to that fold — it stops the recursion from
descending in the first place, before any summing happens for a subtree that
is too deep to trust. The two chain cases pin down the boundary precisely:
depth equal to the cap still computes a real total, one deeper returns
`ErrMaxDepthExceeded` and nothing else.

## Resources

- [Go Specification: Recursive types](https://go.dev/ref/spec#Type_declarations)
- [errors.Is](https://pkg.go.dev/errors#Is)

---

Back to [00-concepts.md](00-concepts.md) | Prev: [11-nested-config-redaction-depth-guard.md](11-nested-config-redaction-depth-guard.md) | Next: [13-comment-thread-render-with-truncation.md](13-comment-thread-render-with-truncation.md)
