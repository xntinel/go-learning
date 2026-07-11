# 13. io/fs Virtual Filesystems

Build a package that reads documents from any `fs.FS`. The lesson focuses on testing filesystem code without touching the host filesystem and on writing APIs that work equally well with embedded files, `os.DirFS`, and `fstest.MapFS`.

## Concepts

### fs.FS Is A Read-Only Interface

`fs.FS` exposes `Open(name string) (fs.File, error)`. Higher-level helpers such as `fs.ReadFile`, `fs.WalkDir`, and `fs.ValidPath` build on that interface. The interface uses slash-separated paths, not OS-specific separators.

### Valid Paths Are Relative

An `fs.FS` path should be relative and slash-separated. `fs.ValidPath` rejects empty paths, paths with leading slashes, and paths with `..` components.

### fstest.MapFS Makes Tests Hermetic

`testing/fstest.MapFS` is an in-memory implementation of `fs.FS`. It lets tests cover missing files, invalid paths, and directory walking without creating real files.

## Exercises

Set up the module:

```bash
mkdir -p ~/go-exercises/docstore/cmd/demo
cd ~/go-exercises/docstore
go mod init example.com/docstore
```

### Exercise 1: Implement A Filesystem-Backed Store

Create `store.go`:

```go
package docstore

import (
	"fmt"
	"io/fs"
	"sort"
	"strings"
)

type Store struct {
	fsys fs.FS
}

func New(fsys fs.FS) (Store, error) {
	if fsys == nil {
		return Store{}, fmt.Errorf("new store: %w", ErrNilFS)
	}
	return Store{fsys: fsys}, nil
}

func (s Store) Read(name string) (string, error) {
	if !fs.ValidPath(name) {
		return "", fmt.Errorf("read document %q: %w", name, ErrInvalidPath)
	}
	data, err := fs.ReadFile(s.fsys, name)
	if err != nil {
		return "", fmt.Errorf("read document %q: %w", name, err)
	}
	return string(data), nil
}

func (s Store) ListMarkdown() ([]string, error) {
	var names []string
	err := fs.WalkDir(s.fsys, ".", func(name string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if !d.IsDir() && strings.HasSuffix(name, ".md") {
			names = append(names, name)
		}
		return nil
	})
	if err != nil {
		return nil, fmt.Errorf("list markdown: %w", err)
	}
	sort.Strings(names)
	return names, nil
}
```

Create `errors.go`:

```go
package docstore

import "errors"

var (
	ErrNilFS       = errors.New("filesystem must not be nil")
	ErrInvalidPath = errors.New("path must be a valid fs path")
)
```

### Exercise 2: Test With MapFS

Create `store_test.go`:

```go
package docstore

import (
	"errors"
	"fmt"
	"testing"
	"testing/fstest"
)

func TestReadUsesProvidedFilesystem(t *testing.T) {
	t.Parallel()

	store, err := New(fstest.MapFS{"docs/a.md": {Data: []byte("A")}})
	if err != nil {
		t.Fatal(err)
	}
	got, err := store.Read("docs/a.md")
	if err != nil {
		t.Fatal(err)
	}
	if got != "A" {
		t.Fatalf("doc = %q", got)
	}
}

func TestListMarkdownSortsResults(t *testing.T) {
	t.Parallel()

	store, err := New(fstest.MapFS{"b.md": {}, "a.txt": {}, "docs/a.md": {}})
	if err != nil {
		t.Fatal(err)
	}
	got, err := store.ListMarkdown()
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 2 || got[0] != "b.md" || got[1] != "docs/a.md" {
		t.Fatalf("names = %+v", got)
	}
}

func TestValidationErrors(t *testing.T) {
	t.Parallel()

	if _, err := New(nil); !errors.Is(err, ErrNilFS) {
		t.Fatalf("New(nil) err = %v", err)
	}
	store, _ := New(fstest.MapFS{})
	if _, err := store.Read("../secret"); !errors.Is(err, ErrInvalidPath) {
		t.Fatalf("Read invalid err = %v", err)
	}
}

func ExampleStore_Read() {
	store, _ := New(fstest.MapFS{"hello.txt": {Data: []byte("hello")}})
	text, _ := store.Read("hello.txt")
	fmt.Println(text)
	// Output: hello
}
```

### Exercise 3: Add A Demo Program

Create `cmd/demo/main.go`:

```go
package main

import (
	"fmt"
	"log"
	"testing/fstest"

	"example.com/docstore"
)

func main() {
	store, err := docstore.New(fstest.MapFS{"docs/readme.md": {Data: []byte("demo")}})
	if err != nil {
		log.Fatal(err)
	}
	names, err := store.ListMarkdown()
	if err != nil {
		log.Fatal(err)
	}
	fmt.Println(names[0])
}
```

## Common Mistakes

### Using filepath Paths With fs.FS

Wrong: pass paths with platform separators into `fs.ReadFile`.

Fix: use slash-separated `fs.FS` paths and validate them with `fs.ValidPath`.

### Testing Only os.DirFS

Wrong: require real files in every test.

Fix: use `fstest.MapFS` for fast hermetic tests, and reserve `os.DirFS` for integration boundaries.

### Returning Unsorted Walk Results

Wrong: expose traversal order as part of the API accidentally.

Fix: sort names before returning when order matters to callers.

## Verification

Run this from `~/go-exercises/docstore`:

```bash
test -z "$(gofmt -l .)"
go vet ./...
go build ./...
go test -count=1 -race ./...
go run ./cmd/demo
```

Add one more test for reading a missing valid path and assert that an error is returned.

## Summary

- `fs.FS` lets code read from embedded, disk, or in-memory filesystems.
- `fs.ValidPath` protects APIs that accept virtual filesystem paths.
- `fstest.MapFS` keeps tests hermetic.
- Sort derived lists when callers need deterministic behavior.

## What's Next

Next: [Pipe-Based I/O](../14-pipe-based-io/14-pipe-based-io.md).

## Resources

- [io/fs package](https://pkg.go.dev/io/fs)
- [testing/fstest package](https://pkg.go.dev/testing/fstest)
- [os.DirFS](https://pkg.go.dev/os#DirFS)
