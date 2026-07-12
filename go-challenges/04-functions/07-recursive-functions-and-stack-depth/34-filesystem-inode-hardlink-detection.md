# Exercise 34: Walk Filesystem Tracking Inodes for Hardlinks

**Nivel: Intermedio** — validacion rapida (un test corto).

Walking a filesystem tree to total up disk usage looks like the simplest
possible recursion: visit a directory, recurse into every child, sum every
file's size. Real filesystems break that simplicity in two specific ways.
A regular file can have more than one directory entry pointing at the same
underlying inode — a hardlink — and a walker that is not tracking inode
numbers counts that file's bytes once per entry, silently inflating a
total-size report. A directory can be bind-mounted onto one of its own
ancestors, so "recurse into every subdirectory" tries to recurse into a
directory that is, for traversal purposes, itself — with no base case a
naive recursive walk will ever reach on its own. Both problems have the
same fix: track which inode numbers you have already seen, at the right
scope, and consult that before you count a file's bytes or descend into a
directory again.

This module is fully self-contained: its own `go mod init`, the walker
inline, its own demo and tests.

## What you'll build

```text
fswalk/                       independent module: example.com/fswalk
  go.mod                         go 1.24
  fswalk.go                       type Node; type WalkResult; Walk (recursive, inode-tracking)
  fswalk_test.go                  every entry visited, hardlink counted once, circular mount stopped (shallow and deep), no false hardlinks
  cmd/
    demo/
      main.go                     a tree with a real hardlink and a circular bind mount, both handled safely
```

- Files: `fswalk.go`, `cmd/demo/main.go`, `fswalk_test.go`.
- Implement: `Node{Name string; Inode uint64; Type EntryType; Size int64; Children []*Node}` and `Walk(root *Node) WalkResult`, recursing through an unexported `walker.visit` that tracks file inodes (for hardlinks) and the chain of directory inodes currently on the path (for circular mounts).
- Test: every entry in a small tree is visited in deterministic order; a hardlinked file's size is counted once, not once per entry; a circular mount is recorded and not recursed into, both one level deep and several levels deep; a tree with no shared inodes reports no hardlinks.
- Verify: `go test -count=1 ./...`

Set up the module:

```bash
mkdir -p go-solutions/04-functions/07-recursive-functions-and-stack-depth/34-filesystem-inode-hardlink-detection/cmd/demo
cd go-solutions/04-functions/07-recursive-functions-and-stack-depth/34-filesystem-inode-hardlink-detection
go mod edit -go=1.24
```

### Two inode-tracking maps, scoped very differently

`walker` keeps two maps that look similar but serve opposite purposes.
`fileInodePaths` (and `fileInodeSize`) accumulate for the *entire* walk:
once a file inode's size has been counted, it stays counted, no matter how
many more directory entries later reference the same inode — that is
exactly what makes a hardlink's bytes get charged once to `TotalSize`
instead of once per entry. `dirAncestors`, in contrast, is scoped to the
*current path only*: a directory's inode is added right before recursing
into its children and removed right after — the same push-before-recurse,
pop-after-return discipline used for cycle detection anywhere else in this
chapter. That difference in scope is the whole design: a file inode seen
twice anywhere in the tree is a hardlink (interesting, but not dangerous —
files have no children to recurse into); a directory inode seen twice
*on the same path* is a cycle that would recurse forever if not stopped,
while the same directory inode reached again via a completely different,
non-overlapping path is not a problem at all (it is just another bind
mount pointing at the same real directory, visited independently). Using
one shared, never-shrinking map for both cases would be a correctness bug
in exactly that scenario: it would flag the second, unrelated visit to a
repeated directory inode as circular even though nothing loops.

`visit` always records a directory's own entry before checking whether it
is circular — the way `ls` would still list a mount point even though
descending into it is unsafe — and only skips the recursive step, not the
listing, once the ancestor check fires.

Create `fswalk.go`:

```go
// Package fswalk recursively walks a filesystem tree while tracking inode
// numbers, the way a real disk-usage tool must to be safe and accurate. A
// regular file can be hardlinked -- two different directory entries, same
// inode, same underlying bytes -- and counting its size once per entry
// silently inflates a total. A directory can be bind-mounted onto one of
// its own ancestors, turning "recurse into every subdirectory" into an
// infinite loop with no base case a naive walker would ever reach. Walk
// tracks file inodes to report hardlinks and count their bytes once, and
// tracks the chain of directory inodes on the current path to detect and
// stop at a circular mount instead of recursing forever.
package fswalk

import "sort"

// EntryType names whether a Node/Entry is a regular file or a directory.
type EntryType int

const (
	File EntryType = iota
	Dir
)

// Node is one node of a synthetic filesystem tree. Two Nodes sharing the
// same Inode represent the same underlying file (a hardlink) or, for
// directories, the same underlying directory reached by two paths (a bind
// mount).
type Node struct {
	Name     string
	Inode    uint64
	Type     EntryType
	Size     int64
	Children []*Node
}

// Entry is one node as reported in a walk's results.
type Entry struct {
	Path  string
	Inode uint64
	Type  EntryType
	Size  int64
}

// WalkResult summarizes a walk.
type WalkResult struct {
	Entries        []Entry
	Hardlinks      map[uint64][]string // file inode -> every path sharing it (only inodes seen more than once)
	TotalSize      int64               // unique file bytes: each file inode's size counted exactly once
	CircularMounts []string            // directory paths where the walk stopped because that inode is already an ancestor
}

// Walk recursively walks the tree rooted at root.
func Walk(root *Node) WalkResult {
	w := &walker{
		fileInodePaths: make(map[uint64][]string),
		fileInodeSize:  make(map[uint64]int64),
		dirAncestors:   make(map[uint64]bool),
	}
	w.visit(root, root.Name)

	var totalSize int64
	for _, size := range w.fileInodeSize {
		totalSize += size
	}

	hardlinks := make(map[uint64][]string)
	for inode, paths := range w.fileInodePaths {
		if len(paths) > 1 {
			sorted := append([]string(nil), paths...)
			sort.Strings(sorted)
			hardlinks[inode] = sorted
		}
	}
	sort.Strings(w.circular)

	return WalkResult{
		Entries:        w.entries,
		Hardlinks:      hardlinks,
		TotalSize:      totalSize,
		CircularMounts: w.circular,
	}
}

type walker struct {
	fileInodePaths map[uint64][]string
	fileInodeSize  map[uint64]int64
	dirAncestors   map[uint64]bool
	entries        []Entry
	circular       []string
}

// visit records node at path and, for directories, recurses into its
// children -- unless node's inode is already an ancestor on the current
// path, in which case it is recorded as a circular mount and the walk
// stops there instead of descending again.
func (w *walker) visit(node *Node, path string) {
	switch node.Type {
	case File:
		w.entries = append(w.entries, Entry{Path: path, Inode: node.Inode, Type: File, Size: node.Size})
		w.fileInodePaths[node.Inode] = append(w.fileInodePaths[node.Inode], path)
		if _, seen := w.fileInodeSize[node.Inode]; !seen {
			w.fileInodeSize[node.Inode] = node.Size
		}

	case Dir:
		w.entries = append(w.entries, Entry{Path: path, Inode: node.Inode, Type: Dir})
		if w.dirAncestors[node.Inode] {
			w.circular = append(w.circular, path)
			return // this inode is already an ancestor: do not recurse again
		}

		w.dirAncestors[node.Inode] = true
		for _, child := range sortedChildren(node) {
			w.visit(child, path+"/"+child.Name)
		}
		delete(w.dirAncestors, node.Inode)
	}
}

func sortedChildren(node *Node) []*Node {
	children := append([]*Node(nil), node.Children...)
	sort.Slice(children, func(i, j int) bool { return children[i].Name < children[j].Name })
	return children
}
```

### The runnable demo

The demo tree has a genuine hardlink (`readme.txt` and `readme_copy.txt`
share inode 100) and a circular bind mount (`mnt/loopback` shares its
inode with `root` itself), and shows both handled without either
inflating the size total or looping.

Create `cmd/demo/main.go`:

```go
package main

import (
	"fmt"

	"example.com/fswalk"
)

func main() {
	root := &fswalk.Node{Name: "root", Inode: 1, Type: fswalk.Dir}
	docs := &fswalk.Node{Name: "docs", Inode: 2, Type: fswalk.Dir, Children: []*fswalk.Node{
		{Name: "readme.txt", Inode: 100, Type: fswalk.File, Size: 500},
		{Name: "readme_copy.txt", Inode: 100, Type: fswalk.File, Size: 500}, // hardlink: same inode
	}}
	loopback := &fswalk.Node{Name: "loopback", Inode: 1, Type: fswalk.Dir} // same inode as root: circular bind mount
	mnt := &fswalk.Node{Name: "mnt", Inode: 3, Type: fswalk.Dir, Children: []*fswalk.Node{loopback}}
	root.Children = []*fswalk.Node{docs, mnt}

	result := fswalk.Walk(root)

	fmt.Println("entries:")
	for _, e := range result.Entries {
		kind := "file"
		if e.Type == fswalk.Dir {
			kind = "dir"
		}
		fmt.Printf("  %-4s %s\n", kind, e.Path)
	}
	fmt.Println("hardlinks:", result.Hardlinks)
	fmt.Println("total size:", result.TotalSize)
	fmt.Println("circular mounts:", result.CircularMounts)
}
```

Run it:

```bash
go run ./cmd/demo
```

Expected output:

```
entries:
  dir  root
  dir  root/docs
  file root/docs/readme.txt
  file root/docs/readme_copy.txt
  dir  root/mnt
  dir  root/mnt/loopback
hardlinks: map[100:[root/docs/readme.txt root/docs/readme_copy.txt]]
total size: 500
circular mounts: [root/mnt/loopback]
```

### Tests

`TestWalkVisitsEveryEntry` checks the basic deterministic traversal.
`TestWalkDetectsHardlinkAndCountsSizeOnce` is the demo's hardlink scenario
as an assertion: `TotalSize` must be 500, not 1000.
`TestWalkStopsAtCircularMountInsteadOfLooping` checks the one-level-deep
circular case, including that the mount point itself is still listed.
`TestWalkHandlesDeeplyNestedCircularMountWithoutHanging` repeats that with
a longer chain before the loop closes, to make sure the guard is not
accidentally special-cased for "parent equals child." `TestWalkNoHardlinksWhenEveryInodeIsUnique`
is the negative case for the hardlink map.

Create `fswalk_test.go`:

```go
package fswalk

import (
	"reflect"
	"testing"
)

func TestWalkVisitsEveryEntry(t *testing.T) {
	t.Parallel()

	root := &Node{Name: "root", Inode: 1, Type: Dir, Children: []*Node{
		{Name: "a.txt", Inode: 10, Type: File, Size: 5},
		{Name: "sub", Inode: 2, Type: Dir, Children: []*Node{
			{Name: "b.txt", Inode: 11, Type: File, Size: 7},
		}},
	}}

	result := Walk(root)
	var paths []string
	for _, e := range result.Entries {
		paths = append(paths, e.Path)
	}
	want := []string{"root", "root/a.txt", "root/sub", "root/sub/b.txt"}
	if !reflect.DeepEqual(paths, want) {
		t.Fatalf("paths = %v, want %v", paths, want)
	}
}

func TestWalkDetectsHardlinkAndCountsSizeOnce(t *testing.T) {
	t.Parallel()

	root := &Node{Name: "root", Inode: 1, Type: Dir, Children: []*Node{
		{Name: "a.txt", Inode: 100, Type: File, Size: 500},
		{Name: "b.txt", Inode: 100, Type: File, Size: 500}, // hardlink to the same inode
	}}

	result := Walk(root)
	if result.TotalSize != 500 {
		t.Fatalf("TotalSize = %d, want 500 (same inode counted once, not 1000)", result.TotalSize)
	}
	want := []string{"root/a.txt", "root/b.txt"}
	if !reflect.DeepEqual(result.Hardlinks[100], want) {
		t.Fatalf("Hardlinks[100] = %v, want %v", result.Hardlinks[100], want)
	}
}

func TestWalkStopsAtCircularMountInsteadOfLooping(t *testing.T) {
	t.Parallel()

	root := &Node{Name: "root", Inode: 1, Type: Dir}
	loopback := &Node{Name: "loopback", Inode: 1, Type: Dir} // same inode as root
	mnt := &Node{Name: "mnt", Inode: 2, Type: Dir, Children: []*Node{loopback}}
	root.Children = []*Node{mnt}

	result := Walk(root)

	if len(result.CircularMounts) != 1 || result.CircularMounts[0] != "root/mnt/loopback" {
		t.Fatalf("CircularMounts = %v, want [root/mnt/loopback]", result.CircularMounts)
	}
	// The circular entry itself is listed (a real `ls` would show it) but
	// must not have been recursed into a second time.
	var paths []string
	for _, e := range result.Entries {
		paths = append(paths, e.Path)
	}
	want := []string{"root", "root/mnt", "root/mnt/loopback"}
	if !reflect.DeepEqual(paths, want) {
		t.Fatalf("paths = %v, want %v", paths, want)
	}
}

func TestWalkHandlesDeeplyNestedCircularMountWithoutHanging(t *testing.T) {
	t.Parallel()

	// A longer chain before looping back: root -> a -> b -> c -> (loop to a).
	a := &Node{Name: "a", Inode: 2, Type: Dir}
	b := &Node{Name: "b", Inode: 3, Type: Dir}
	loopToA := &Node{Name: "loop", Inode: 2, Type: Dir} // same inode as "a"
	c := &Node{Name: "c", Inode: 4, Type: Dir, Children: []*Node{loopToA}}
	b.Children = []*Node{c}
	a.Children = []*Node{b}
	root := &Node{Name: "root", Inode: 1, Type: Dir, Children: []*Node{a}}

	result := Walk(root)
	want := []string{"root/a/b/c/loop"}
	if !reflect.DeepEqual(result.CircularMounts, want) {
		t.Fatalf("CircularMounts = %v, want %v", result.CircularMounts, want)
	}
}

func TestWalkNoHardlinksWhenEveryInodeIsUnique(t *testing.T) {
	t.Parallel()

	root := &Node{Name: "root", Inode: 1, Type: Dir, Children: []*Node{
		{Name: "a.txt", Inode: 10, Type: File, Size: 5},
		{Name: "b.txt", Inode: 11, Type: File, Size: 7},
	}}
	result := Walk(root)
	if len(result.Hardlinks) != 0 {
		t.Fatalf("Hardlinks = %v, want none", result.Hardlinks)
	}
	if result.TotalSize != 12 {
		t.Fatalf("TotalSize = %d, want 12", result.TotalSize)
	}
}
```

## Review

`Walk` is correct when `TotalSize` reflects unique bytes on disk rather
than bytes per directory entry, `Hardlinks` names exactly the inodes
reached by more than one path, and a circular mount is listed once,
flagged, and never expanded. `TestWalkDetectsHardlinkAndCountsSizeOnce` is
the test that would fail (with a doubled total) on a version of this
exercise that sums `node.Size` unconditionally on every file entry
instead of gating it behind "have I already counted this inode's bytes" —
a mistake that produces a plausible-looking, simply wrong number rather
than a crash, which is exactly why it needs its own assertion rather than
relying on "the program didn't panic." `TestWalkHandlesDeeplyNestedCircularMountWithoutHanging`
is the test that would fail (by hanging) on a version that only checks
`node.Inode == parent.Inode` — catching a directory that loops directly
back to its immediate parent but missing one that loops back to a
grandparent or further, which is exactly why the guard has to check
against the *whole* current-path ancestor set, not just one level up.

## Resources

- [Wikipedia: Inode](https://en.wikipedia.org/wiki/Inode)
- [Wikipedia: Hard link](https://en.wikipedia.org/wiki/Hard_link)
- [io/fs package (fs.FS, for a real filesystem's WalkDir counterpart)](https://pkg.go.dev/io/fs)

---

Back to [00-concepts.md](00-concepts.md) | Prev: [33-database-schema-fk-cycle-detection.md](33-database-schema-fk-cycle-detection.md) | Next: [../08-init-functions-and-package-initialization/00-concepts.md](../08-init-functions-and-package-initialization/00-concepts.md)
