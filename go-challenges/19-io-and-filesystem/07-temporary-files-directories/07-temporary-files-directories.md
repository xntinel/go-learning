# 7. Temporary Files and Directories

Build a small package for staging JSON data in temporary files and committing it with an atomic rename. The lesson focuses on cleanup, permissions, and how to test file operations without leaking host state.

## Concepts

### Temp Paths Are For Isolation

`os.CreateTemp` and `os.MkdirTemp` create names that are hard to guess and unlikely to collide. Tests should prefer `t.TempDir`, which automatically removes the directory after the test.

### Close Before Rename Or Readback

A temporary file still has buffered kernel state and an open descriptor. Close it before reopening or renaming it, especially when code must work consistently across platforms.

### Cleanup Policy Belongs In The API

Some temporary files are internal scratch space and should always be deleted. Others are staged outputs that should survive after a successful commit. The API should make that distinction explicit.

## Exercises

### Exercise 1: Stage JSON In A Temporary File

Create `stage.go`:

```go
package tempstage

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
)

type StagedFile struct {
	path string
}

func StageJSON(dir string, value any) (StagedFile, error) {
	if dir == "" {
		return StagedFile{}, fmt.Errorf("stage json: %w", ErrEmptyDir)
	}
	f, err := os.CreateTemp(dir, "data-*.json")
	if err != nil {
		return StagedFile{}, fmt.Errorf("create temp file: %w", err)
	}
	path := f.Name()
	remove := true
	defer func() {
		if remove {
			_ = os.Remove(path)
		}
	}()

	enc := json.NewEncoder(f)
	enc.SetIndent("", "  ")
	if err := enc.Encode(value); err != nil {
		_ = f.Close()
		return StagedFile{}, fmt.Errorf("encode json: %w", err)
	}
	if err := f.Close(); err != nil {
		return StagedFile{}, fmt.Errorf("close temp file: %w", err)
	}
	remove = false
	return StagedFile{path: path}, nil
}

func (s StagedFile) Path() string {
	return s.path
}

func (s StagedFile) Read() ([]byte, error) {
	if s.path == "" {
		return nil, fmt.Errorf("read staged file: %w", ErrEmptyPath)
	}
	data, err := os.ReadFile(s.path)
	if err != nil {
		return nil, fmt.Errorf("read staged file: %w", err)
	}
	return data, nil
}

func (s StagedFile) Commit(target string) error {
	if s.path == "" || target == "" {
		return fmt.Errorf("commit staged file: %w", ErrEmptyPath)
	}
	if err := os.MkdirAll(filepath.Dir(target), 0o700); err != nil {
		return fmt.Errorf("create target dir: %w", err)
	}
	if err := os.Rename(s.path, target); err != nil {
		return fmt.Errorf("rename staged file: %w", err)
	}
	s.path = ""
	return nil
}
```

Create `errors.go`:

```go
package tempstage

import "errors"

var (
	ErrEmptyDir  = errors.New("directory must not be empty")
	ErrEmptyPath = errors.New("path must not be empty")
)
```

### Exercise 2: Test Staging And Commit

Create `stage_test.go`:

```go
package tempstage

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestStageJSONWritesReadableTempFile(t *testing.T) {
	t.Parallel()

	staged, err := StageJSON(t.TempDir(), map[string]string{"name": "Ada"})
	if err != nil {
		t.Fatal(err)
	}
	data, err := staged.Read()
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(data), `"name": "Ada"`) {
		t.Fatalf("data = %s", data)
	}
}

func TestCommitRenamesFileToTarget(t *testing.T) {
	t.Parallel()

	root := t.TempDir()
	staged, err := StageJSON(root, map[string]int{"count": 2})
	if err != nil {
		t.Fatal(err)
	}
	target := filepath.Join(root, "out", "data.json")
	if err := staged.Commit(target); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(target); err != nil {
		t.Fatal(err)
	}
}

func TestStageJSONRejectsEmptyDir(t *testing.T) {
	t.Parallel()

	_, err := StageJSON("", map[string]string{})
	if !errors.Is(err, ErrEmptyDir) {
		t.Fatalf("err = %v, want ErrEmptyDir", err)
	}
}

func TestReadRejectsEmptyPath(t *testing.T) {
	t.Parallel()

	_, err := (StagedFile{}).Read()
	if !errors.Is(err, ErrEmptyPath) {
		t.Fatalf("err = %v, want ErrEmptyPath", err)
	}
}

func ExampleStageJSON() {
	dir, _ := os.MkdirTemp("", "tempstage-example-")
	defer os.RemoveAll(dir)
	staged, _ := StageJSON(dir, map[string]string{"name": "Ada"})
	fmt.Println(filepath.Dir(staged.Path()) == dir)
	// Output: true
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

	"example.com/tempstage"
)

func main() {
	dir, err := os.MkdirTemp("", "tempstage-")
	if err != nil {
		log.Fatal(err)
	}
	defer os.RemoveAll(dir)

	staged, err := tempstage.StageJSON(dir, map[string]string{"name": "demo"})
	if err != nil {
		log.Fatal(err)
	}
	target := filepath.Join(dir, "committed.json")
	if err := staged.Commit(target); err != nil {
		log.Fatal(err)
	}
	fmt.Println(target)
}
```

## Common Mistakes

### Forgetting To Close The File

Wrong: encode JSON and immediately rename the still-open temp file.

Fix: close the file and check the close error before returning the staged path.

### Leaking Temp Files On Encode Errors

Wrong: return after a failed encode and leave the partial file behind.

Fix: use a deferred cleanup flag so failed staging removes the temp path.

### Testing In A Shared Directory

Wrong: write tests under `/tmp/my-app-test` and manually clean up.

Fix: use `t.TempDir`; it is isolated and automatically removed.

## Verification

Run this from `~/go-exercises/tempstage`:

```bash
test -z "$(gofmt -l .)"
go vet ./...
go build ./...
go test -count=1 -race ./...
go run ./cmd/demo
```

Add one more test that calls `Commit("")` and asserts `errors.Is(err, ErrEmptyPath)`.

## Summary

- `os.CreateTemp` and `os.MkdirTemp` create collision-resistant temporary paths.
- Close temporary files before reading, renaming, or reporting success.
- Use `t.TempDir` for hermetic tests.
- Decide which temporary paths are scratch files and which are staged outputs.

## What's Next

Next: [CSV Reading and Writing](../08-csv-reading-writing/08-csv-reading-writing.md).

## Resources

- [os.CreateTemp](https://pkg.go.dev/os#CreateTemp)
- [os.MkdirTemp](https://pkg.go.dev/os#MkdirTemp)
- [testing.T.TempDir](https://pkg.go.dev/testing#T.TempDir)
- [os.Rename](https://pkg.go.dev/os#Rename)
