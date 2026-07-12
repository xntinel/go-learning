# Exercise 2: Convert the Recursive Walker to an Explicit-Stack, Depth-Bounded Walker

The same tree walk, mechanically converted from recursion to an explicit LIFO
stack, so you can carry a per-frame depth and refuse to descend past a limit. This
is the counter-skill to Exercise 1: when the tree is not yours to trust, you trade
the tidy recurrence for a stack you control and a hard bound you enforce.

This module is fully self-contained: its own `go mod init`, both the recursive
`Walk` (for a parity baseline) and the iterative `WalkBounded` inline, its own demo
and tests.

## What you'll build

```text
boundedwalk/               independent module: example.com/boundedwalk
  go.mod                   go 1.26
  walk/
    walk.go                Entry; Walk (recursive baseline); WalkBounded; ErrMaxDepthExceeded
    walk_test.go           parity, boundary (maxDepth vs maxDepth+1), determinism
  cmd/
    demo/
      main.go              bounded walk within budget; a too-deep tree rejected
```

- Files: `walk/walk.go`, `cmd/demo/main.go`, `walk/walk_test.go`.
- Implement: `WalkBounded(fsys fs.FS, root string, maxDepth int) ([]Entry, error)` using an explicit stack of `frame{path, depth}`, returning the sentinel `ErrMaxDepthExceeded` when a subtree exceeds `maxDepth`; keep a recursive `Walk` as the parity baseline.
- Test: parity with `Walk` on a trusted tree, a boundary tree at exactly `maxDepth` passing and `maxDepth+1` failing (checked with `errors.Is`), and deterministic ordering.
- Verify: `go test -count=1 -race ./...`

### The mechanical transformation

Recursion to iteration is a rote transformation, and doing it by hand once fixes
the pattern. The arguments of the recursive call — here the directory path and its
depth — become the fields of a `frame` struct. The recursive call becomes a *push*
onto an explicit stack; the function's entry becomes a *pop* in a loop that runs
until the stack is empty. That is the whole recipe.

The subtle part is preserving order and enforcing the bound. To reproduce the
recursive pre-order (a node, then its subtree, before its siblings), push a
directory's children in reverse-sorted order so they *pop* in sorted order, and
push a directory's own children immediately when it is popped — so its subtree is
processed before anything pushed earlier. That makes `WalkBounded` produce the
exact same order as the recursive `Walk`, which the parity test relies on.

The bound is the reason the conversion is worth doing. Each frame carries its
`depth`. The root is depth 0; its direct children are depth 1, and so on. When a
frame is popped whose depth exceeds `maxDepth`, the walk stops and returns
`ErrMaxDepthExceeded` wrapped with the offending path. Because the check happens
before the frame's children are read, an adversarial deep chain fails fast after
`maxDepth+1` pops — it never builds a stack proportional to the full (possibly
enormous) depth, and it never approaches the goroutine stack cap, because there is
no call recursion at all. This is precisely the defense a recursive walk cannot
offer: a recursive `Walk` over a million-deep chain approaches the 1 GiB stack cap
and aborts the process; `WalkBounded` rejects it with an ordinary error after
`maxDepth+1` iterations.

The classic bug in this conversion is carrying the path but not the depth — the
loop still runs, the stack still bounds memory somewhat, but the *depth limit you
added is never actually checked*. The frame struct must carry `depth`, and the loop
must test it.

Create `walk/walk.go`:

```go
package walk

import (
	"errors"
	"fmt"
	"io/fs"
	"path"
	"sort"
)

// Entry is one filesystem node discovered by a walk.
type Entry struct {
	Path  string
	IsDir bool
	Size  int64
}

// ErrMaxDepthExceeded is returned when a subtree is deeper than the allowed limit.
var ErrMaxDepthExceeded = errors.New("max depth exceeded")

// Walk is the recursive baseline (see Exercise 1). It is unbounded and is kept
// here only so tests can prove WalkBounded produces the same entries on a
// trusted tree.
func Walk(fsys fs.FS, root string) ([]Entry, error) {
	if fsys == nil {
		return nil, errors.New("nil fs")
	}
	if root == "" {
		root = "."
	}
	info, err := fs.Stat(fsys, root)
	if err != nil {
		return nil, err
	}
	var out []Entry
	if !info.IsDir() {
		out = append(out, Entry{Path: root, Size: info.Size()})
		return out, nil
	}
	var rec func(current string) error
	rec = func(current string) error {
		entries, err := fs.ReadDir(fsys, current)
		if err != nil {
			return err
		}
		sort.Slice(entries, func(i, j int) bool { return entries[i].Name() < entries[j].Name() })
		for _, e := range entries {
			child := path.Join(current, e.Name())
			ci, err := e.Info()
			if err != nil {
				return err
			}
			out = append(out, Entry{Path: child, IsDir: e.IsDir(), Size: ci.Size()})
			if e.IsDir() {
				if err := rec(child); err != nil {
					return err
				}
			}
		}
		return nil
	}
	out = append(out, Entry{Path: root, IsDir: true})
	if err := rec(root); err != nil {
		return nil, err
	}
	return out, nil
}

type frame struct {
	path  string
	isDir bool
	size  int64
	depth int
}

// WalkBounded walks fsys from root using an explicit LIFO stack instead of
// recursion, rejecting any subtree deeper than maxDepth with ErrMaxDepthExceeded.
// The root is depth 0; its direct children are depth 1. Output order matches Walk.
func WalkBounded(fsys fs.FS, root string, maxDepth int) ([]Entry, error) {
	if fsys == nil {
		return nil, errors.New("nil fs")
	}
	if root == "" {
		root = "."
	}
	info, err := fs.Stat(fsys, root)
	if err != nil {
		return nil, err
	}
	var out []Entry
	if !info.IsDir() {
		out = append(out, Entry{Path: root, Size: info.Size()})
		return out, nil
	}

	stack := []frame{{path: root, isDir: true, depth: 0}}
	for len(stack) > 0 {
		f := stack[len(stack)-1]
		stack = stack[:len(stack)-1]

		if f.depth > maxDepth {
			return nil, fmt.Errorf("%q at depth %d: %w", f.path, f.depth, ErrMaxDepthExceeded)
		}

		out = append(out, Entry{Path: f.path, IsDir: f.isDir, Size: f.size})

		if !f.isDir {
			continue
		}
		entries, err := fs.ReadDir(fsys, f.path)
		if err != nil {
			return nil, err
		}
		sort.Slice(entries, func(i, j int) bool { return entries[i].Name() < entries[j].Name() })
		// Push in reverse so children pop in sorted (pre-order) sequence.
		for i := len(entries) - 1; i >= 0; i-- {
			e := entries[i]
			ci, err := e.Info()
			if err != nil {
				return nil, err
			}
			stack = append(stack, frame{
				path:  path.Join(f.path, e.Name()),
				isDir: e.IsDir(),
				size:  ci.Size(),
				depth: f.depth + 1,
			})
		}
	}
	return out, nil
}
```

### The runnable demo

The demo walks a small tree within a generous budget, then shows the same walker
rejecting a chain nested past a tight limit.

Create `cmd/demo/main.go`:

```go
package main

import (
	"errors"
	"fmt"
	"testing/fstest"

	"example.com/boundedwalk/walk"
)

func main() {
	fsys := fstest.MapFS{
		"config.yaml":       &fstest.MapFile{Data: []byte("port: 8080")},
		"a/b/deep.txt":      &fstest.MapFile{Data: []byte("ok")},
		"a/sibling.txt":     &fstest.MapFile{Data: []byte("hi")},
		"a/b/c/toodeep.txt": &fstest.MapFile{Data: []byte("x")},
	}

	entries, err := walk.WalkBounded(fsys, ".", 10)
	if err != nil {
		panic(err)
	}
	fmt.Printf("within budget: %d entries\n", len(entries))

	_, err = walk.WalkBounded(fsys, ".", 2)
	fmt.Printf("tight budget err: %v\n", err)
	fmt.Printf("is ErrMaxDepthExceeded: %v\n", errors.Is(err, walk.ErrMaxDepthExceeded))
}
```

Run it:

```bash
go run ./cmd/demo
```

Expected output:

```
within budget: 8 entries
tight budget err: "a/b/c" at depth 3: max depth exceeded
is ErrMaxDepthExceeded: true
```

### Tests

`TestParityWithRecursiveWalk` builds a trusted tree and asserts `WalkBounded` with
a generous limit returns the exact same entries, in the same order, as the
recursive `Walk` — proving the conversion is faithful. `TestBoundaryAtMaxDepth`
builds a chain and checks that a limit equal to the leaf's depth passes while one
short of it returns `ErrMaxDepthExceeded`, verified with `errors.Is`.
`TestDeterministicOrder` pins the exact order. `TestVeryDeepChainIsRejectedFast`
builds a 5000-deep chain that a naive recursion budget would strain and confirms
the iterative walker rejects it with an ordinary error instead of a crash.

Create `walk/walk_test.go`:

```go
package walk

import (
	"errors"
	"fmt"
	"reflect"
	"testing"
	"testing/fstest"
)

// chainFS builds a single directory chain d0/d1/.../d(n-1)/leaf.txt.
func chainFS(n int) (fstest.MapFS, string) {
	dir := ""
	for i := range n {
		if i == 0 {
			dir = "d0"
		} else {
			dir = dir + "/d" + fmt.Sprint(i)
		}
	}
	leaf := dir + "/leaf.txt"
	return fstest.MapFS{leaf: &fstest.MapFile{Data: []byte("bottom")}}, leaf
}

func TestParityWithRecursiveWalk(t *testing.T) {
	t.Parallel()

	fsys := fstest.MapFS{
		"a.txt":          &fstest.MapFile{Data: []byte("hello")},
		"b.txt":          &fstest.MapFile{Data: []byte("world!")},
		"sub/c.txt":      &fstest.MapFile{Data: []byte("c")},
		"sub/inner/e.go": &fstest.MapFile{Data: []byte("package x")},
	}

	want, err := Walk(fsys, ".")
	if err != nil {
		t.Fatal(err)
	}
	got, err := WalkBounded(fsys, ".", 100)
	if err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("WalkBounded = %+v\nWalk = %+v", got, want)
	}
}

func TestBoundaryAtMaxDepth(t *testing.T) {
	t.Parallel()

	// Chain d0/d1/d2/leaf.txt: leaf.txt is at depth 4 (d0=1,d1=2,d2=3,leaf=4).
	fsys, _ := chainFS(3)

	if _, err := WalkBounded(fsys, ".", 4); err != nil {
		t.Fatalf("maxDepth=4 should pass, got %v", err)
	}
	_, err := WalkBounded(fsys, ".", 3)
	if !errors.Is(err, ErrMaxDepthExceeded) {
		t.Fatalf("maxDepth=3 err = %v, want ErrMaxDepthExceeded", err)
	}
}

func TestDeterministicOrder(t *testing.T) {
	t.Parallel()

	fsys := fstest.MapFS{
		"z.txt":     &fstest.MapFile{Data: []byte("z")},
		"a.txt":     &fstest.MapFile{Data: []byte("a")},
		"m/b.txt":   &fstest.MapFile{Data: []byte("b")},
		"m/a.txt":   &fstest.MapFile{Data: []byte("a")},
		"m/z/x.txt": &fstest.MapFile{Data: []byte("x")},
	}

	got, err := WalkBounded(fsys, ".", 100)
	if err != nil {
		t.Fatal(err)
	}
	paths := make([]string, len(got))
	for i, e := range got {
		paths[i] = e.Path
	}
	want := []string{".", "a.txt", "m", "m/a.txt", "m/b.txt", "m/z", "m/z/x.txt", "z.txt"}
	if !reflect.DeepEqual(paths, want) {
		t.Fatalf("order = %v\nwant  = %v", paths, want)
	}
}

func TestVeryDeepChainIsRejectedFast(t *testing.T) {
	t.Parallel()

	fsys, _ := chainFS(5000)
	_, err := WalkBounded(fsys, ".", 64)
	if !errors.Is(err, ErrMaxDepthExceeded) {
		t.Fatalf("deep chain err = %v, want ErrMaxDepthExceeded", err)
	}
}

func Example() {
	fsys := fstest.MapFS{"a/b/c.txt": &fstest.MapFile{Data: []byte("x")}}
	_, err := WalkBounded(fsys, ".", 2)
	fmt.Println(errors.Is(err, ErrMaxDepthExceeded))
	// Output: true
}
```

## Review

The conversion is faithful when `WalkBounded` on a trusted tree is
indistinguishable from `Walk` — `TestParityWithRecursiveWalk` asserts exact
equality of entries and order, which is the honest proof that the manual stack did
not silently change the traversal. The bound is real when the boundary test holds:
a leaf at depth `d` passes at `maxDepth == d` and fails at `maxDepth == d-1` with a
sentinel checked by `errors.Is`. The mistake this exercise exists to prevent is
carrying the path but not the depth in the frame — do that and the loop runs but
the limit is never enforced, which passes the parity test and silently fails the
security one. The very-deep-chain test is the payoff: 5000 levels that would make a
recursive walker climb toward the stack cap are rejected in `maxDepth+1`
iterations with an ordinary, recoverable error. Reach for this form whenever the
tree's depth is attacker-controlled.

## Resources

- [io/fs package (fs.FS, ReadDir, Stat)](https://pkg.go.dev/io/fs)
- [runtime/debug.SetMaxStack (goroutine stack cap)](https://pkg.go.dev/runtime/debug#SetMaxStack)
- [errors package (errors.Is, errors.New)](https://pkg.go.dev/errors)
- [testing/fstest package (MapFS)](https://pkg.go.dev/testing/fstest)

---

Back to [00-concepts.md](00-concepts.md) | Prev: [01-recursive-fs-tree-walker.md](01-recursive-fs-tree-walker.md) | Next: [03-untrusted-json-depth-guard.md](03-untrusted-json-depth-guard.md)
