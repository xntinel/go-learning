# 5. Walking Directory Trees

Build a directory inventory package with `filepath.WalkDir`. The core problem is not recursion itself; it is preserving useful error context while keeping traversal deterministic enough to test.

## Concepts

### WalkDir Avoids Extra Stat Calls

`filepath.WalkDir` visits a root and every descendant using `fs.DirEntry`. A `DirEntry` often carries enough information to tell whether an entry is a directory without calling `Info`, so it is usually cheaper than the older `filepath.Walk` API.

### Traversal Errors Are Data

The walk function receives an `err` argument when the walker could not read an entry. Returning that error stops the walk; returning nil skips that failing entry. A library should make that policy explicit.

### File Extensions Need Normalization

`filepath.Ext("README")` returns an empty string. `filepath.Ext("main.go")` returns `.go`. A useful inventory should decide how to represent files without extensions rather than letting callers guess.

## Exercises

### Exercise 1: Implement The Inventory

Create `inventory.go`:

```go
package treeinventory

import (
	"fmt"
	"io/fs"
	"path/filepath"
	"sort"
)

const NoExtension = "[none]"

type Inventory struct {
	Files      int
	Dirs       int
	Bytes      int64
	Extensions map[string]int
}

type ExtensionCount struct {
	Extension string
	Count     int
}

func Scan(root string) (Inventory, error) {
	if root == "" {
		return Inventory{}, fmt.Errorf("scan tree: %w", ErrEmptyRoot)
	}

	inv := Inventory{Extensions: map[string]int{}}
	err := filepath.WalkDir(root, func(path string, d fs.DirEntry, err error) error {
		if err != nil {
			return fmt.Errorf("visit %s: %w", path, err)
		}
		if d.IsDir() {
			inv.Dirs++
			return nil
		}
		info, err := d.Info()
		if err != nil {
			return fmt.Errorf("stat %s: %w", path, err)
		}
		inv.Files++
		inv.Bytes += info.Size()
		ext := filepath.Ext(d.Name())
		if ext == "" {
			ext = NoExtension
		}
		inv.Extensions[ext]++
		return nil
	})
	if err != nil {
		return Inventory{}, fmt.Errorf("scan tree: %w", err)
	}
	return inv, nil
}

func (i Inventory) TopExtensions(limit int) []ExtensionCount {
	if limit < 1 {
		return nil
	}
	counts := make([]ExtensionCount, 0, len(i.Extensions))
	for ext, count := range i.Extensions {
		counts = append(counts, ExtensionCount{Extension: ext, Count: count})
	}
	sort.Slice(counts, func(a, b int) bool {
		if counts[a].Count == counts[b].Count {
			return counts[a].Extension < counts[b].Extension
		}
		return counts[a].Count > counts[b].Count
	})
	if len(counts) > limit {
		counts = counts[:limit]
	}
	return counts
}
```

Create `errors.go`:

```go
package treeinventory

import "errors"

var ErrEmptyRoot = errors.New("root path must not be empty")
```

### Exercise 2: Test The Scanner

Create `inventory_test.go`:

```go
package treeinventory

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"testing"
)

func TestScanCountsFilesDirsBytesAndExtensions(t *testing.T) {
	t.Parallel()

	root := t.TempDir()
	mustWrite(t, filepath.Join(root, "main.go"), "package main\n")
	mustWrite(t, filepath.Join(root, "README"), "docs")
	mustMkdir(t, filepath.Join(root, "nested"))
	mustWrite(t, filepath.Join(root, "nested", "data.txt"), "abc")

	inv, err := Scan(root)
	if err != nil {
		t.Fatal(err)
	}
	if inv.Files != 3 || inv.Dirs != 2 || inv.Bytes != 20 {
		t.Fatalf("inventory = %+v", inv)
	}
	if inv.Extensions[".go"] != 1 || inv.Extensions[".txt"] != 1 || inv.Extensions[NoExtension] != 1 {
		t.Fatalf("extensions = %+v", inv.Extensions)
	}
}

func TestTopExtensionsSortsByCountThenName(t *testing.T) {
	t.Parallel()

	inv := Inventory{Extensions: map[string]int{".txt": 2, ".go": 2, ".md": 1}}
	got := inv.TopExtensions(2)
	if len(got) != 2 || got[0].Extension != ".go" || got[1].Extension != ".txt" {
		t.Fatalf("top = %+v", got)
	}
}

func TestScanRejectsEmptyRoot(t *testing.T) {
	t.Parallel()

	_, err := Scan("")
	if !errors.Is(err, ErrEmptyRoot) {
		t.Fatalf("err = %v, want ErrEmptyRoot", err)
	}
}

func ExampleInventory_TopExtensions() {
	inv := Inventory{Extensions: map[string]int{".go": 3, ".md": 1}}
	fmt.Println(inv.TopExtensions(1)[0].Extension)
	// Output: .go
}

func mustWrite(t *testing.T, path, data string) {
	t.Helper()
	if err := os.WriteFile(path, []byte(data), 0o600); err != nil {
		t.Fatal(err)
	}
}

func mustMkdir(t *testing.T, path string) {
	t.Helper()
	if err := os.Mkdir(path, 0o700); err != nil {
		t.Fatal(err)
	}
}
```

### Exercise 3: Add A Demo Program

Create `cmd/demo/main.go`:

```go
package main

import (
	"fmt"
	"log"
	"os"
	"path/filepath"

	"example.com/treeinventory"
)

func main() {
	root, err := os.MkdirTemp("", "treeinventory-")
	if err != nil {
		log.Fatal(err)
	}
	defer os.RemoveAll(root)

	if err := os.WriteFile(filepath.Join(root, "demo.txt"), []byte("demo"), 0o600); err != nil {
		log.Fatal(err)
	}

	inv, err := treeinventory.Scan(root)
	if err != nil {
		log.Fatal(err)
	}
	fmt.Printf("files=%d dirs=%d bytes=%d\n", inv.Files, inv.Dirs, inv.Bytes)
}
```

## Common Mistakes

### Walking A Real Home Directory In Tests

Wrong: test the scanner against `$HOME` and hope the counts stay stable.

Fix: use `t.TempDir` and create the exact tree the test needs.

### Dropping The Path From Errors

Wrong: return `err` directly from the callback when `d.Info()` fails.

Fix: wrap it with the path, as `Scan` does with `fmt.Errorf("stat %s: %w", path, err)`.

### Sorting Only By Count

Wrong: sort extension counts by count and leave ties to map iteration order.

Fix: add a name tie-breaker so tests and demos are deterministic.

## Verification

Run this from `~/go-exercises/treeinventory`:

```bash
test -z "$(gofmt -l .)"
go vet ./...
go build ./...
go test -count=1 -race ./...
go run ./cmd/demo
```

Add one more test for `TopExtensions(0)` returning an empty slice.

## Summary

- `filepath.WalkDir` is the preferred filesystem tree traversal API for path-based walks.
- Tests should build temporary trees instead of relying on machine-specific directories.
- Traversal errors should include the path and wrap the original error.
- Stable sorting makes map-derived reports testable.

## What's Next

Next: [The embed Directive](../06-embed-directive/06-embed-directive.md).

## Resources

- [path/filepath WalkDir](https://pkg.go.dev/path/filepath#WalkDir)
- [io/fs package](https://pkg.go.dev/io/fs)
- [os package](https://pkg.go.dev/os)
