# Exercise 2: t.TempDir for a File-Backed Config/Cache Loader

Backend code writes files: a config loader persists a rendered configuration, a
cache warms an entry to disk, a migration runner reads a directory. Testing that
code needs scratch space that is unique per test and gone afterward, with no
`/tmp` litter and no manual cleanup that a `t.Fatal` would skip. `t.TempDir()` is
exactly that primitive, and this exercise wires a small file-backed loader through
it.

This module is fully self-contained: its own `go mod init`, its own demo, its own
tests. Nothing here imports any other exercise.

## What you'll build

```text
configcache/                 independent module: example.com/configcache
  go.mod                     go 1.24
  loader.go                  Loader (Save/Load/Path) over a base directory
  cmd/
    demo/
      main.go                runnable demo: save a rendered config, load it back
  loader_test.go             t.TempDir roundtrip, uniqueness, auto-removal proof
```

- Files: `loader.go`, `cmd/demo/main.go`, `loader_test.go`.
- Implement: a `Loader` bound to a base directory, with `Save(name, data)`, `Load(name)`, and `Path(name)` using `path/filepath.Join`.
- Test: a roundtrip through `t.TempDir()`, a uniqueness check across two `t.TempDir()` calls, and a proof that the tree is removed after the test.
- Verify: `go test -count=1 -race ./...`

Set up the module:

```bash
go mod edit -go=1.24
```

### Why t.TempDir instead of os.MkdirTemp + defer

The obvious way to get scratch space in a test is `dir, _ := os.MkdirTemp("",
"..."); defer os.RemoveAll(dir)`. It has two defects. First, the `defer` runs at
function return, so a `t.Fatal` above it skips the removal and leaves the tree on
disk — every failing run adds litter to `/tmp`. Second, if the test is parallel or
spawns parallel subtests, the `defer` timing is wrong for the same reason it is
wrong for any resource. `t.TempDir()` fixes both: it registers its removal as a
`t.Cleanup`, so the tree is removed at *test* end unconditionally — on success, on
`t.Fatal`, on panic — and each call returns a fresh unique directory, so parallel
tests never collide. It also honors `GOTMPDIR`, so CI can point scratch at a fast
or size-limited volume.

The loader itself is deliberately small: it joins names against a base directory
and reads and writes bytes. In production that base directory is a configured cache
path; in a test it is `t.TempDir()`. The loader does not know the difference, which
is the point — you test the real file I/O without a filesystem abstraction, because
the scratch directory is already hermetic.

Create `loader.go`:

```go
package configcache

import (
	"os"
	"path/filepath"
)

// Loader reads and writes small files under a base directory. In production the
// base is a configured cache path; in tests it is t.TempDir().
type Loader struct {
	dir string
}

// NewLoader binds a loader to a base directory.
func NewLoader(dir string) *Loader {
	return &Loader{dir: dir}
}

// Path returns the absolute path a name resolves to under the base directory.
func (l *Loader) Path(name string) string {
	return filepath.Join(l.dir, name)
}

// Save writes data to name under the base directory with owner-only permissions.
func (l *Loader) Save(name string, data []byte) error {
	return os.WriteFile(l.Path(name), data, 0o600)
}

// Load reads the bytes previously stored under name.
func (l *Loader) Load(name string) ([]byte, error) {
	return os.ReadFile(l.Path(name))
}
```

### The runnable demo

The demo runs outside a test, so it uses `os.MkdirTemp` and cleans up with a
`defer` — the pattern `t.TempDir()` replaces inside tests. It saves a rendered
config and loads it back.

Create `cmd/demo/main.go`:

```go
package main

import (
	"fmt"
	"os"

	"example.com/configcache"
)

func main() {
	dir, err := os.MkdirTemp("", "configcache-demo")
	if err != nil {
		panic(err)
	}
	defer os.RemoveAll(dir)

	l := configcache.NewLoader(dir)
	if err := l.Save("config.json", []byte(`{"port":8080}`)); err != nil {
		panic(err)
	}
	data, err := l.Load("config.json")
	if err != nil {
		panic(err)
	}
	fmt.Printf("loaded: %s\n", data)
}
```

Run it:

```bash
go run ./cmd/demo
```

Expected output:

```
loaded: {"port":8080}
```

### The tests

`TestSaveLoadRoundTrip` proves the loader persists and reads back correctly over a
`t.TempDir()`. `TestTempDirUniquePerCall` proves two calls return different
directories, which is what makes parallel tests safe. `TestTempDirRemovedAfterTest`
is the auto-removal proof: it captures a subtest's `t.TempDir()` path, and after the
subtest returns — at which point the subtest's cleanups, including the temp-dir
removal, have run — it asserts `os.Stat` on that path reports "does not exist". That
is a deterministic, in-process proof that the tree is gone.

Create `loader_test.go`:

```go
package configcache

import (
	"errors"
	"fmt"
	"os"
	"testing"
)

func TestSaveLoadRoundTrip(t *testing.T) {
	t.Parallel()
	l := NewLoader(t.TempDir())

	want := []byte("rendered-config\nport=8080\n")
	if err := l.Save("app.conf", want); err != nil {
		t.Fatalf("Save: %v", err)
	}
	got, err := l.Load("app.conf")
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if string(got) != string(want) {
		t.Fatalf("Load = %q, want %q", got, want)
	}
}

func TestTempDirUniquePerCall(t *testing.T) {
	t.Parallel()
	a := t.TempDir()
	b := t.TempDir()
	if a == b {
		t.Fatalf("TempDir returned the same directory twice: %q", a)
	}
}

func TestTempDirRemovedAfterTest(t *testing.T) {
	t.Parallel()
	var captured string
	t.Run("writes to scratch", func(t *testing.T) {
		dir := t.TempDir()
		captured = dir
		l := NewLoader(dir)
		if err := l.Save("warm-cache.bin", []byte("payload")); err != nil {
			t.Fatalf("Save: %v", err)
		}
		if _, err := os.Stat(l.Path("warm-cache.bin")); err != nil {
			t.Fatalf("file should exist during the test: %v", err)
		}
	})
	// The subtest has returned, so its t.TempDir cleanup has removed the tree.
	if _, err := os.Stat(captured); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("temp dir %q should be removed after the subtest; stat err = %v", captured, err)
	}
}

func ExampleLoader() {
	dir, _ := os.MkdirTemp("", "configcache-example")
	defer os.RemoveAll(dir)

	l := NewLoader(dir)
	_ = l.Save("greeting.txt", []byte("warm-cache"))
	data, _ := l.Load("greeting.txt")
	fmt.Println(string(data))
	// Output: warm-cache
}
```

## Review

The loader is correct when a save followed by a load returns the same bytes, and
the hermeticity is correct when the scratch tree is gone after the test — which
`TestTempDirRemovedAfterTest` verifies against the real filesystem, not by
inspection. The mistake to avoid is reaching back for `os.MkdirTemp` plus `defer
os.RemoveAll` inside a test: it skips removal on `t.Fatal` and litters `/tmp`, and
its timing is wrong under parallelism. `t.TempDir()` removes the tree
unconditionally and hands each call a unique directory. Run `go test -race` to
confirm the parallel roundtrip and uniqueness tests never touch the same directory.

## Resources

- [`testing.T.TempDir`](https://pkg.go.dev/testing#T.TempDir) — unique per call, auto-removed, GOTMPDIR-honoring.
- [`os.WriteFile`](https://pkg.go.dev/os#WriteFile) and [`os.ReadFile`](https://pkg.go.dev/os#ReadFile) — the file I/O under test.
- [`path/filepath.Join`](https://pkg.go.dev/path/filepath#Join) — building paths under the base directory.

---

Back to [00-concepts.md](00-concepts.md) | Previous: [01-repo-fixture-cleanup.md](01-repo-fixture-cleanup.md) | Next: [03-cleanup-runs-on-failure.md](03-cleanup-runs-on-failure.md)
