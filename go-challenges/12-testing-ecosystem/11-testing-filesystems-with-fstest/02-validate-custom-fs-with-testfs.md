# Exercise 2: Implement a custom fs.FS and prove correctness with fstest.TestFS

Sooner or later you write your own `fs.FS`: an adapter that mounts a subtree
under a virtual root, a union of two filesystems, a read-through cache. The
contract is subtle and easy to get wrong. This exercise builds a `prefixFS`
adapter and validates it with `fstest.TestFS`, the standard-library contract
checker — the tool that catches the bugs a hand-written FS ships with.

This module is fully self-contained: its own `go mod init`, all code inline, its
own demo and tests.

## What you'll build

```text
prefixfs/                    independent module: example.com/prefixfs
  go.mod                     go 1.26
  prefixfs.go                prefixFS type; New(fs.FS, prefix) fs.FS; Open with ValidPath guard
  cmd/
    demo/
      main.go                wrap a MapFS, read a file through the prefix, list a dir
  prefixfs_test.go           fstest.TestFS contract check + invalid-path rejection + Sub parity
```

- Files: `prefixfs.go`, `cmd/demo/main.go`, `prefixfs_test.go`.
- Implement: `New(inner fs.FS, prefix string) fs.FS` returning a `prefixFS`
  whose `Open(name)` rejects any `name` failing `fs.ValidPath` with a
  `*fs.PathError` wrapping `fs.ErrInvalid`, joins the prefix, and delegates.
- Test: drive `fstest.TestFS(wrapped, expected...)` over the wrapper and require
  `nil`; a negative test that `Open("../escape")` satisfies
  `errors.Is(err, fs.ErrInvalid)`; a parity test against `fs.Sub`.
- Verify: `go test -count=1 -race ./...`

### The contract is the hard part, not the Open

A `prefixFS` mounts an inner filesystem's subtree at the virtual root: reading
`css/app.css` on the wrapper reads `assets/css/app.css` on the inner FS. The
implementation is three lines of real work, but two of them encode contract
obligations that are easy to omit.

First, *validate the incoming path before touching the inner FS*. The `fs.FS`
contract says `Open` must reject any `name` for which `fs.ValidPath(name)` is
false, and it must do so with a `*fs.PathError` whose `Err` is `fs.ErrInvalid`.
If you skip this and blindly `path.Join(prefix, name)`, a caller passing
`../../etc/passwd` gets cleaned by `path.Join` into something that may escape
the prefix — the exact traversal the rooted contract is supposed to prevent. The
guard is the security boundary; it must come first.

Second, *join with `path.Join`, not string concatenation*. `path.Join(prefix,
".")` correctly yields `prefix` (opening the wrapper's root opens the inner
subtree root), and `path.Join` cleans the result so no `//` or trailing slash
sneaks through. Naive concatenation (`prefix + "/" + name`) breaks the root case
and produces invalid inner paths.

This is, in effect, a reimplementation of `fs.Sub` — which is exactly why the
test compares behavior against `fs.Sub` as a reference. The value of the
exercise is not the adapter; it is proving the adapter is correct with
`fstest.TestFS`, which walks the whole wrapped tree, opens every file, checks
`ReadDir` ordering, checks `Stat`/`Open` agreement, and checks that invalid
paths are rejected. A single call surfaces every contract violation.

Create `prefixfs.go`:

```go
package prefixfs

import (
	"io/fs"
	"path"
)

// prefixFS presents inner's subtree rooted at prefix as a virtual root:
// Open("x") on the wrapper resolves to Open(prefix/x) on inner.
type prefixFS struct {
	inner  fs.FS
	prefix string
}

// New wraps inner so that its prefix subtree appears at the root.
func New(inner fs.FS, prefix string) fs.FS {
	return prefixFS{inner: inner, prefix: prefix}
}

// Open validates name against the fs.FS contract, joins the prefix, and
// delegates to the inner filesystem.
func (p prefixFS) Open(name string) (fs.File, error) {
	if !fs.ValidPath(name) {
		return nil, &fs.PathError{Op: "open", Path: name, Err: fs.ErrInvalid}
	}
	return p.inner.Open(path.Join(p.prefix, name))
}
```

### The runnable demo

The demo wraps a `MapFS` whose files live under `assets/`, then reads and lists
them through the virtual root as if `assets/` were the top level.

Create `cmd/demo/main.go`:

```go
package main

import (
	"fmt"
	"io/fs"
	"testing/fstest"

	"example.com/prefixfs"
)

func main() {
	inner := fstest.MapFS{
		"assets/index.html":   {Data: []byte("<h1>home</h1>")},
		"assets/css/app.css":  {Data: []byte("body{}")},
		"assets/js/app.js":    {Data: []byte("console.log(1)")},
		"unrelated/notes.txt": {Data: []byte("ignored")},
	}
	wrapped := prefixfs.New(inner, "assets")

	data, _ := fs.ReadFile(wrapped, "css/app.css")
	fmt.Printf("css/app.css -> %s\n", data)

	entries, _ := fs.ReadDir(wrapped, ".")
	for _, e := range entries {
		fmt.Printf("root entry: %s (dir=%v)\n", e.Name(), e.IsDir())
	}
}
```

Run it:

```bash
go run ./cmd/demo
```

Expected output:

```
css/app.css -> body{}
root entry: css (dir=true)
root entry: index.html (dir=false)
root entry: js (dir=true)
```

### Tests

`TestContract` is the centerpiece: it builds a nested `MapFS`, wraps it, and
hands the wrapper plus the full expected path list to `fstest.TestFS`. If the
adapter mis-orders `ReadDir`, disagrees between `Stat` and `Open`, or fails to
reject an invalid path, `TestFS` returns a non-nil error and the test fails.
`TestRejectsTraversal` proves the `fs.ValidPath` guard fires:
`wrapped.Open("../escape")` must return an error satisfying
`errors.Is(err, fs.ErrInvalid)`. `TestParityWithSub` reads the same file through
both `prefixFS` and `fs.Sub` and asserts identical bytes, pinning the adapter to
the standard-library reference.

Create `prefixfs_test.go`:

```go
package prefixfs

import (
	"errors"
	"io/fs"
	"testing"
	"testing/fstest"
)

func nestedFS() fstest.MapFS {
	return fstest.MapFS{
		"assets/index.html":  {Data: []byte("<h1>home</h1>")},
		"assets/css/app.css": {Data: []byte("body{}")},
		"assets/js/app.js":   {Data: []byte("console.log(1)")},
		"other/ignore.txt":   {Data: []byte("ignored")},
	}
}

func TestContract(t *testing.T) {
	t.Parallel()

	wrapped := New(nestedFS(), "assets")
	if err := fstest.TestFS(wrapped, "index.html", "css/app.css", "js/app.js"); err != nil {
		t.Fatalf("prefixFS violates fs.FS contract: %v", err)
	}
}

func TestRejectsTraversal(t *testing.T) {
	t.Parallel()

	wrapped := New(nestedFS(), "assets")
	_, err := wrapped.Open("../escape")
	if !errors.Is(err, fs.ErrInvalid) {
		t.Fatalf("Open(../escape) err = %v, want errors.Is fs.ErrInvalid", err)
	}
}

func TestParityWithSub(t *testing.T) {
	t.Parallel()

	inner := nestedFS()
	wrapped := New(inner, "assets")
	sub, err := fs.Sub(inner, "assets")
	if err != nil {
		t.Fatal(err)
	}

	got, err := fs.ReadFile(wrapped, "css/app.css")
	if err != nil {
		t.Fatalf("prefixFS ReadFile: %v", err)
	}
	want, err := fs.ReadFile(sub, "css/app.css")
	if err != nil {
		t.Fatalf("Sub ReadFile: %v", err)
	}
	if string(got) != string(want) {
		t.Fatalf("prefixFS = %q, Sub = %q", got, want)
	}
}
```

## Review

The adapter is correct when it upholds two obligations the `fs.FS` contract
imposes: reject invalid paths with a `*fs.PathError` wrapping `fs.ErrInvalid`
*before* dispatch, and resolve the root case (`.`) to the inner subtree root via
`path.Join`. `fstest.TestFS` is what proves both, plus the ordering and
`Stat`/`Open` consistency you would otherwise have to hand-test. The mistake to
avoid is shipping a custom `fs.FS` without running `TestFS` against it — the
contract has enough corners that "it worked in my one manual test" is not
evidence. The parity test against `fs.Sub` is a second check that the adapter's
observable behavior matches the standard library's own subtree implementation.

## Resources

- [`fstest.TestFS`](https://pkg.go.dev/testing/fstest#TestFS) — walks and validates any `fs.FS` against the contract.
- [`fs.ValidPath`](https://pkg.go.dev/io/fs#ValidPath) — the exact path rules an `Open` must enforce.
- [`fs.Sub`](https://pkg.go.dev/io/fs#Sub) — the standard-library subtree adapter this exercise mirrors.

---

Back to [00-concepts.md](00-concepts.md) | Previous: [01-config-loader-over-fsfs.md](01-config-loader-over-fsfs.md) | Next: [03-migration-runner-walkdir.md](03-migration-runner-walkdir.md)
