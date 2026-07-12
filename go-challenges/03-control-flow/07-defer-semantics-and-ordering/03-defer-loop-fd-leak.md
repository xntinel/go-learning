# Exercise 3: Config Loader — Fixing a defer-in-loop FD Leak

A config loader that reads and merges every file in a directory is a textbook
place to leak file descriptors: the naive version `defer f.Close()` *inside* the
loop, so every handle stays open until the whole load finishes and a large
directory exhausts the process `ulimit`. This exercise builds the correct version
— a per-file helper whose `defer Close()` fires at the end of each iteration — and
instruments open/close to prove at most one handle is open at a time.

This module is fully self-contained: its own `go mod init`, all code inline, its
own demo and tests.

## What you'll build

```text
cfgload/                     independent module: example.com/cfgload
  go.mod                     module example.com/cfgload
  cfgload.go                 Opener, Load (per-file helper), parse key=value
  cmd/
    demo/
      main.go                runnable demo: write temp files, load, print merged
  cfgload_test.go            counting opener proves max 1 handle open; merge check
```

- Files: `cfgload.go`, `cmd/demo/main.go`, `cfgload_test.go`.
- Implement: `Load(open Opener, dir string) (map[string]string, error)` that lists the directory with `os.ReadDir`, and for each file calls a helper `loadOne` that opens through the injected `Opener`, reads with `io.ReadAll`, parses `key=value` lines, and `defer`s `Close` in its own scope.
- Test: a counting `Opener` that tracks concurrently-open handles asserts the maximum is 1; a fixture directory asserts the merged config; last-file-wins on duplicate keys.
- Verify: `go test -count=1 -race ./...`

Set up the module:

```bash
mkdir -p go-solutions/03-control-flow/07-defer-semantics-and-ordering/03-defer-loop-fd-leak/cmd/demo
cd go-solutions/03-control-flow/07-defer-semantics-and-ordering/03-defer-loop-fd-leak
```

### Why the helper is the fix, not a style preference

Here is the bug, written the way it usually appears:

```go
// Wrong: every handle stays open until Load returns.
func Load(dir string) (map[string]string, error) {
	entries, _ := os.ReadDir(dir)
	merged := map[string]string{}
	for _, e := range entries {
		f, err := os.Open(filepath.Join(dir, e.Name()))
		if err != nil {
			return nil, err
		}
		defer f.Close() // BUG: fires at Load's return, not at end of iteration
		// ... parse f into merged ...
	}
	return merged, nil
}
```

`defer` fires at *function* return, and the function here is `Load`, not the loop
body. So a directory with 5,000 config files opens 5,000 handles and holds them
all until `Load` returns. Long before that, the process hits its open-file limit
and `os.Open` starts failing with "too many open files". The bug is invisible in a
unit test with three fixture files and catastrophic in production at scale, which
is exactly why it survives code review.

The fix is structural: extract a per-file helper. The helper's function boundary
becomes the per-iteration cleanup boundary, so its `defer Close()` runs each time
the helper returns — once per file — and the handle count never exceeds one.

Create `cfgload.go`:

```go
package cfgload

import (
	"fmt"
	"io"
	"os"
	"path/filepath"
	"sort"
	"strings"
)

// Opener abstracts os.Open so a test can inject a handle-counting fake. In
// production it is just os.Open wrapped to return an io.ReadCloser.
type Opener func(name string) (io.ReadCloser, error)

// OSOpener is the production Opener.
func OSOpener(name string) (io.ReadCloser, error) {
	return os.Open(name)
}

// Load reads every regular file in dir and merges their key=value contents.
// Later files win on duplicate keys (deterministic: entries are sorted).
func Load(open Opener, dir string) (map[string]string, error) {
	entries, err := os.ReadDir(dir)
	if err != nil {
		return nil, fmt.Errorf("read dir: %w", err)
	}

	names := make([]string, 0, len(entries))
	for _, e := range entries {
		if e.IsDir() {
			continue
		}
		names = append(names, e.Name())
	}
	sort.Strings(names)

	merged := make(map[string]string)
	for _, name := range names {
		// Each call to loadOne opens, parses, and closes within its own scope,
		// so the descriptor is released at the end of this iteration.
		if err := loadOne(open, filepath.Join(dir, name), merged); err != nil {
			return nil, err
		}
	}
	return merged, nil
}

// loadOne opens a single file, parses it into merged, and defers Close in its
// own function scope. This is the fix for the defer-in-loop leak.
func loadOne(open Opener, path string, merged map[string]string) error {
	f, err := open(path)
	if err != nil {
		return fmt.Errorf("open %s: %w", path, err)
	}
	defer f.Close()

	data, err := io.ReadAll(f)
	if err != nil {
		return fmt.Errorf("read %s: %w", path, err)
	}
	for _, line := range strings.Split(string(data), "\n") {
		line = strings.TrimSpace(line)
		if line == "" || strings.HasPrefix(line, "#") {
			continue
		}
		key, value, ok := strings.Cut(line, "=")
		if !ok {
			return fmt.Errorf("%s: malformed line %q", path, line)
		}
		merged[strings.TrimSpace(key)] = strings.TrimSpace(value)
	}
	return nil
}
```

### The runnable demo

The demo writes two config files into a temp directory, loads them, and prints the
merged result. The second file overrides `region`, so last-file-wins is visible.

Create `cmd/demo/main.go`:

```go
package main

import (
	"fmt"
	"os"
	"path/filepath"
	"sort"

	"example.com/cfgload"
)

func main() {
	dir, err := os.MkdirTemp("", "cfgload-demo")
	if err != nil {
		fmt.Println("mkdir:", err)
		return
	}
	defer os.RemoveAll(dir)

	os.WriteFile(filepath.Join(dir, "01-base.conf"),
		[]byte("region=us-east-1\nworkers=4\n"), 0o644)
	os.WriteFile(filepath.Join(dir, "02-override.conf"),
		[]byte("# prod override\nregion=eu-west-1\n"), 0o644)

	cfg, err := cfgload.Load(cfgload.OSOpener, dir)
	if err != nil {
		fmt.Println("load:", err)
		return
	}

	keys := make([]string, 0, len(cfg))
	for k := range cfg {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	for _, k := range keys {
		fmt.Printf("%s=%s\n", k, cfg[k])
	}
}
```

Run it:

```bash
go run ./cmd/demo
```

Expected output:

```
region=eu-west-1
workers=4
```

### Tests

`TestAtMostOneHandleOpen` is the point of the exercise: a counting `Opener` tracks
how many handles are open at any instant and asserts the maximum never exceeds
one, which is only true because `loadOne` closes each file before the next opens.
`TestMergeAndOverride` checks the merged config and last-file-wins. `TestMalformed`
checks the error path for a line with no `=`.

Create `cfgload_test.go`:

```go
package cfgload

import (
	"errors"
	"io"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
)

// countingOpener wraps OSOpener and tracks concurrently-open handles.
type countingOpener struct {
	mu      sync.Mutex
	open    int
	maxOpen int
}

func (c *countingOpener) Open(name string) (io.ReadCloser, error) {
	f, err := os.Open(name)
	if err != nil {
		return nil, err
	}
	c.mu.Lock()
	c.open++
	if c.open > c.maxOpen {
		c.maxOpen = c.open
	}
	c.mu.Unlock()
	return &countingFile{ReadCloser: f, c: c}, nil
}

type countingFile struct {
	io.ReadCloser
	c    *countingOpener
	once sync.Once
}

func (f *countingFile) Close() error {
	f.once.Do(func() {
		f.c.mu.Lock()
		f.c.open--
		f.c.mu.Unlock()
	})
	return f.ReadCloser.Close()
}

func writeFixtures(t *testing.T) string {
	t.Helper()
	dir := t.TempDir()
	files := map[string]string{
		"01-base.conf":     "region=us-east-1\nworkers=4\n",
		"02-cache.conf":    "# comment line\ncache_ttl=30s\n",
		"03-override.conf": "region=eu-west-1\n",
	}
	for name, body := range files {
		if err := os.WriteFile(filepath.Join(dir, name), []byte(body), 0o644); err != nil {
			t.Fatalf("write %s: %v", name, err)
		}
	}
	return dir
}

func TestAtMostOneHandleOpen(t *testing.T) {
	t.Parallel()

	dir := writeFixtures(t)
	co := &countingOpener{}

	if _, err := Load(co.Open, dir); err != nil {
		t.Fatalf("Load() = %v", err)
	}
	if co.maxOpen != 1 {
		t.Fatalf("max simultaneously-open handles = %d, want 1", co.maxOpen)
	}
	if co.open != 0 {
		t.Fatalf("handles still open after Load = %d, want 0", co.open)
	}
}

func TestMergeAndOverride(t *testing.T) {
	t.Parallel()

	dir := writeFixtures(t)
	cfg, err := Load(OSOpener, dir)
	if err != nil {
		t.Fatalf("Load() = %v", err)
	}

	want := map[string]string{
		"region":    "eu-west-1", // 03-override wins over 01-base
		"workers":   "4",
		"cache_ttl": "30s",
	}
	for k, v := range want {
		if cfg[k] != v {
			t.Errorf("cfg[%q] = %q, want %q", k, cfg[k], v)
		}
	}
}

func TestMalformedLineIsAnError(t *testing.T) {
	t.Parallel()

	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "bad.conf"),
		[]byte("this-line-has-no-equals\n"), 0o644); err != nil {
		t.Fatal(err)
	}

	_, err := Load(OSOpener, dir)
	if err == nil || !strings.Contains(err.Error(), "malformed line") {
		t.Fatalf("Load() error = %v, want malformed line", err)
	}
}

func TestMissingDir(t *testing.T) {
	t.Parallel()

	_, err := Load(OSOpener, filepath.Join(t.TempDir(), "does-not-exist"))
	if !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("Load() error = %v, want os.ErrNotExist", err)
	}
}
```

## Review

The loader is correct when the merged map reflects last-file-wins and — the part
that matters at scale — when at most one descriptor is open at any instant.
`TestAtMostOneHandleOpen` proves the latter by instrumenting the `Opener`; if you
revert `loadOne` back into an inline `defer f.Close()` in the loop, `maxOpen` jumps
to the number of files and the test fails. The trap to internalize is that `defer`
binds to the enclosing *function*, so "put a `defer Close` in the loop" is not a
fix — the fix is a function boundary per iteration. The injected `Opener` also
demonstrates a broader idiom: to test resource lifecycle deterministically, make
the resource acquisition an interface you can count against, rather than reaching
for real OS-level FD introspection.

## Resources

- [`os.Open` / `os.File.Close`](https://pkg.go.dev/os#Open) — the descriptor whose lifetime the helper bounds.
- [`os.ReadDir`](https://pkg.go.dev/os#ReadDir) — listing the directory once, sorted.
- [`io.ReadAll`](https://pkg.go.dev/io#ReadAll) — draining each file before its scope ends.
- [Effective Go: Defer](https://go.dev/doc/effective_go#defer) — why deferred closes accumulate to function return.

---

Back to [00-concepts.md](00-concepts.md) | Previous: [02-defer-lifo-resource-stack.md](02-defer-lifo-resource-stack.md) | Next: [04-named-return-rollback.md](04-named-return-rollback.md)
