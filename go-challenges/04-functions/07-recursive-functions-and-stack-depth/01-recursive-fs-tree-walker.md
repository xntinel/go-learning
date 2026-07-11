# Exercise 1: Recursive Filesystem Tree Walker with Size and Depth Aggregation

A tree walker that visits every entry under a root and reports the total size and
the deepest path is the canonical place recursion earns its keep: the code mirrors
the recurrence, and the tree is data you own, so its depth is trusted. This module
builds that walker over an `fs.FS`, with a named inner closure that recurses on
directories, plus `TotalSize` and `DeepestPath` aggregators.

This module is fully self-contained: its own `go mod init`, all code inline, its
own demo and tests. Nothing here imports another exercise.

## What you'll build

```text
treewalk/                  independent module: example.com/treewalk
  go.mod                   go 1.26
  walk/
    walk.go                type Entry; Walk (recursive), TotalSize, DeepestPath
    walk_test.go           fstest.MapFS trees: visits, size, nil, depth, deep tree
  cmd/
    demo/
      main.go              walk an in-memory tree, print entries, size, deepest
```

- Files: `walk/walk.go`, `cmd/demo/main.go`, `walk/walk_test.go`.
- Implement: `Walk(fsys fs.FS, root string) ([]Entry, error)` using a named inner closure that recurses on directories, plus `TotalSize([]Entry) int64` and `DeepestPath([]Entry) (string, int)`.
- Test: synthetic `fstest.MapFS` trees asserting every entry is visited once, sizes sum, a nil filesystem errors, the deepest file and its depth are reported, a single-file root early-returns, and a 50-directory-deep tree still walks.
- Verify: `go test -count=1 -race ./...`

Set up the module:

```bash
mkdir -p ~/go-exercises/treewalk/walk ~/go-exercises/treewalk/cmd/demo
cd ~/go-exercises/treewalk
go mod init example.com/treewalk
```

### Why recursion is the right default here

The tree is an `fs.FS` you are handed — a directory layout, an embedded asset
tree, a synthetic test filesystem. Its depth is bounded by something you control,
so the hard stack cap is never in play, and the recurrence reads directly: to walk
a directory, list its entries, record each, and walk the ones that are themselves
directories. The base case is a file — it is recorded and the recursion does not
descend. A directory with no children is also a base case: `ReadDir` returns an
empty slice and the loop does nothing.

`Walk` uses a named function literal, `var walk func(string) error; walk = ...`,
for the recursive step. The name is required because an anonymous closure cannot
refer to itself: you must declare the variable first so the body can call `walk`
by name. The closure captures `out` and `fsys` from the enclosing `Walk`, so the
recursion accumulates into a single slice rather than merging returned slices at
every level — simpler and allocation-light.

Two design points matter for correctness. First, entries are sorted by name before
recording, so the output order is deterministic regardless of the filesystem's
native `ReadDir` order; tests can then assert an exact order. Second, `Walk`
checks `info.IsDir()` on the root: a single-file root is an early return that emits
just that file, never attempting `ReadDir` on a non-directory (which would fail
with `not a directory`).

`TotalSize` sums the `Size` of every entry; directories report size 0 in this
model, so the total is exactly the sum of file bytes. `DeepestPath` ignores
directories and returns the file with the most path separators, along with that
depth — a shallow scan over the already-flattened entry list, not a second walk.

Create `walk/walk.go`:

```go
package walk

import (
	"errors"
	"io/fs"
	"path"
	"sort"
)

// Entry is one filesystem node discovered by Walk.
type Entry struct {
	Path  string
	IsDir bool
	Size  int64
}

// Walk returns every entry under root in deterministic (name-sorted) order. It
// recurses on directories via a named inner closure; a file root early-returns.
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

	var walk func(current string) error
	walk = func(current string) error {
		entries, err := fs.ReadDir(fsys, current)
		if err != nil {
			return err
		}
		sort.Slice(entries, func(i, j int) bool { return entries[i].Name() < entries[j].Name() })
		for _, entry := range entries {
			child := path.Join(current, entry.Name())
			info, err := entry.Info()
			if err != nil {
				return err
			}
			out = append(out, Entry{Path: child, IsDir: entry.IsDir(), Size: info.Size()})
			if entry.IsDir() {
				if err := walk(child); err != nil {
					return err
				}
			}
		}
		return nil
	}

	out = append(out, Entry{Path: root, IsDir: true})
	if err := walk(root); err != nil {
		return nil, err
	}
	return out, nil
}

// TotalSize sums the byte size of every entry. Directories contribute 0.
func TotalSize(entries []Entry) int64 {
	var total int64
	for _, e := range entries {
		total += e.Size
	}
	return total
}

// DeepestPath returns the most deeply nested file and its depth (separator count).
func DeepestPath(entries []Entry) (string, int) {
	var deepest string
	var maxDepth int
	for _, e := range entries {
		if e.IsDir {
			continue
		}
		depth := 0
		for _, c := range e.Path {
			if c == '/' {
				depth++
			}
		}
		if depth > maxDepth {
			maxDepth = depth
			deepest = e.Path
		}
	}
	return deepest, maxDepth
}
```

### The runnable demo

The demo builds an in-memory `fstest.MapFS` shaped like a small service repo,
walks it, and prints each entry, the total size, and the deepest path.

Create `cmd/demo/main.go`:

```go
package main

import (
	"fmt"
	"testing/fstest"

	"example.com/treewalk/walk"
)

func main() {
	fsys := fstest.MapFS{
		"config.yaml":      &fstest.MapFile{Data: []byte("port: 8080")},
		"handlers/auth.go": &fstest.MapFile{Data: []byte("package handlers")},
		"handlers/user.go": &fstest.MapFile{Data: []byte("package handlers")},
		"handlers/v2/x.go": &fstest.MapFile{Data: []byte("package v2")},
	}

	entries, err := walk.Walk(fsys, ".")
	if err != nil {
		panic(err)
	}
	for _, e := range entries {
		kind := "file"
		if e.IsDir {
			kind = "dir"
		}
		fmt.Printf("%-4s %5d  %s\n", kind, e.Size, e.Path)
	}
	fmt.Printf("total: %d bytes\n", walk.TotalSize(entries))
	deepest, depth := walk.DeepestPath(entries)
	fmt.Printf("deepest: %s (depth %d)\n", deepest, depth)
}
```

Run it:

```bash
go run ./cmd/demo
```

Expected output:

```
dir      0  .
file    10  config.yaml
dir      0  handlers
file    16  handlers/auth.go
file    16  handlers/user.go
dir      0  handlers/v2
file    10  handlers/v2/x.go
total: 52 bytes
deepest: handlers/v2/x.go (depth 2)
```

### Tests

The tests build synthetic `fstest.MapFS` trees and assert the walker's contract.
`TestWalkVisitsEveryEntry` sorts the reported paths and checks the exact set,
proving every entry is visited once. `TestWalkReportsTotalSize` checks the byte
sum. `TestWalkRejectsNilFS` checks the guard. `TestDeepestPathReturnsTheMostNestedFile`
pins the depth aggregator. `TestWalkSingleFile` proves the non-directory early
return. `TestWalkSurvivesDeepTree` builds a 50-directory chain and confirms the
recursion handles a non-trivial depth. The `Example` gives an `// Output:`-checked
smoke of the size helper.

Create `walk/walk_test.go`:

```go
package walk

import (
	"fmt"
	"reflect"
	"sort"
	"strings"
	"testing"
	"testing/fstest"
)

func TestWalkVisitsEveryEntry(t *testing.T) {
	t.Parallel()

	fsys := fstest.MapFS{
		"a.txt":          &fstest.MapFile{Data: []byte("hello")},
		"b.txt":          &fstest.MapFile{Data: []byte("world!")},
		"sub/c.txt":      &fstest.MapFile{Data: []byte("c")},
		"sub/d.txt":      &fstest.MapFile{Data: []byte("dd")},
		"sub/inner/e.go": &fstest.MapFile{Data: []byte("package x")},
	}

	got, err := Walk(fsys, ".")
	if err != nil {
		t.Fatal(err)
	}

	paths := make([]string, len(got))
	for i, e := range got {
		paths[i] = e.Path
	}
	sort.Strings(paths)

	want := []string{
		".",
		"a.txt",
		"b.txt",
		"sub",
		"sub/c.txt",
		"sub/d.txt",
		"sub/inner",
		"sub/inner/e.go",
	}
	if !reflect.DeepEqual(paths, want) {
		t.Fatalf("paths = %v, want %v", paths, want)
	}
}

func TestWalkReportsTotalSize(t *testing.T) {
	t.Parallel()

	fsys := fstest.MapFS{
		"a.txt":     &fstest.MapFile{Data: []byte("hello")},
		"b.txt":     &fstest.MapFile{Data: []byte("world!")},
		"sub/c.txt": &fstest.MapFile{Data: []byte("c")},
	}

	entries, err := Walk(fsys, ".")
	if err != nil {
		t.Fatal(err)
	}
	if got := TotalSize(entries); got != 12 {
		t.Fatalf("TotalSize = %d, want 12 (5+6+1)", got)
	}
}

func TestWalkRejectsNilFS(t *testing.T) {
	t.Parallel()

	if _, err := Walk(nil, "."); err == nil {
		t.Fatal("expected error for nil fs")
	}
}

func TestDeepestPathReturnsTheMostNestedFile(t *testing.T) {
	t.Parallel()

	fsys := fstest.MapFS{
		"a.txt":                &fstest.MapFile{Data: []byte("a")},
		"sub/b.txt":            &fstest.MapFile{Data: []byte("b")},
		"sub/inner/c.txt":      &fstest.MapFile{Data: []byte("c")},
		"sub/inner/deep/d.txt": &fstest.MapFile{Data: []byte("d")},
	}

	entries, err := Walk(fsys, ".")
	if err != nil {
		t.Fatal(err)
	}
	got, depth := DeepestPath(entries)
	if got != "sub/inner/deep/d.txt" {
		t.Fatalf("DeepestPath = %q, want sub/inner/deep/d.txt", got)
	}
	if depth != 3 {
		t.Fatalf("depth = %d, want 3", depth)
	}
}

func TestWalkSingleFile(t *testing.T) {
	t.Parallel()

	fsys := fstest.MapFS{
		"only.txt": &fstest.MapFile{Data: []byte("x")},
	}
	entries, err := Walk(fsys, "only.txt")
	if err != nil {
		t.Fatal(err)
	}
	if len(entries) != 1 || entries[0].Path != "only.txt" {
		t.Fatalf("entries = %+v", entries)
	}
}

func TestWalkSurvivesDeepTree(t *testing.T) {
	t.Parallel()

	fsys := fstest.MapFS{}
	dir := ""
	for i := range 50 {
		if i == 0 {
			dir = "d0"
		} else {
			dir = dir + "/d" + fmt.Sprint(i)
		}
	}
	fsys[dir+"/leaf.txt"] = &fstest.MapFile{Data: []byte("bottom")}

	entries, err := Walk(fsys, ".")
	if err != nil {
		t.Fatal(err)
	}

	var leaf string
	for _, e := range entries {
		if strings.HasSuffix(e.Path, "leaf.txt") {
			leaf = e.Path
		}
	}
	if leaf == "" {
		t.Fatal("deep leaf not visited")
	}
	if _, depth := DeepestPath(entries); depth != 50 {
		t.Fatalf("deepest depth = %d, want 50", depth)
	}
}

func Example() {
	fsys := fstest.MapFS{
		"a.txt":     &fstest.MapFile{Data: []byte("hello")},
		"sub/b.txt": &fstest.MapFile{Data: []byte("world!")},
	}
	entries, _ := Walk(fsys, ".")
	fmt.Println(TotalSize(entries))
	// Output: 11
}
```

## Review

The walker is correct when its output is a deterministic, complete flattening of
the tree: every file and directory appears exactly once, sorted by name at each
level, with the root first. `TestWalkVisitsEveryEntry` proves completeness against
a known set; `TestWalkSingleFile` proves the non-directory early return, which is
the guard that keeps `ReadDir` off a file. The two mistakes this exercise targets
are recursing without the leaf base case (a file must not trigger `ReadDir`) and
forgetting the `IsDir` check before descending. `TotalSize` and `DeepestPath` are
pure functions over the flattened list, so they are trivially testable without a
second traversal. This recursive walker is the right tool precisely because the
filesystem is data you own; the next exercise converts it to a bounded iterative
form for the case where the depth is not yours to trust.

## Resources

- [io/fs package (fs.FS, ReadDir, DirEntry, Stat)](https://pkg.go.dev/io/fs)
- [testing/fstest package (MapFS, MapFile)](https://pkg.go.dev/testing/fstest)
- [path package (path.Join)](https://pkg.go.dev/path)
- [Go Specification: Function declarations](https://go.dev/ref/spec#Function_declarations)

---

Back to [00-concepts.md](00-concepts.md) | Next: [02-depth-bounded-iterative-walker.md](02-depth-bounded-iterative-walker.md)
