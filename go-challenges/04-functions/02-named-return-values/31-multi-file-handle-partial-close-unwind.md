# Exercise 31: Multi-File Handle Partial Acquisition Unwind

Opening several files as one logical operation — say, every shard of a
sharded log — means that if the third file fails to open, the first two
must not leak. This exercise builds an `OpenAll` that opens paths in order
into a named `files []*os.File` slice and, via a single deferred closure
keyed on the named `err`, closes every handle collected so far the moment
any `os.Open` call fails.

**Nivel: Avanzado** — validacion normal (exito, fallo a mitad de la lista, verificacion de que los handles quedaron cerrados).

## What you'll build

```text
multiopen/                  independent module: example.com/multiopen
  go.mod
  multiopen.go               OpenAll (named files+err, deferred unwind on error)
  cmd/demo/
    main.go                  runnable demo: all-present case and one-missing-path case
  multiopen_test.go           success returns every handle; failure closes the ones already opened
```

- Files: `multiopen.go`, `cmd/demo/main.go`, `multiopen_test.go`.
- Implement: `OpenAll(paths []string) (files []*os.File, err error)` that appends to `files` as it opens each path and, in a deferred closure, closes every entry in `files` whenever the named `err` is non-nil.
- Test: all paths present returns every handle open; a missing path partway through returns an error and the handles already opened are provably closed (a `Read` on them returns `os.ErrClosed`).
- Verify: `go test -count=1 ./...`

Set up the module:

```bash
mkdir -p go-solutions/04-functions/02-named-return-values/31-multi-file-handle-partial-close-unwind/cmd/demo
cd go-solutions/04-functions/02-named-return-values/31-multi-file-handle-partial-close-unwind
go mod edit -go=1.24
```

### One unwind, driven by what was actually collected

```go
defer func() {
    if err != nil {
        for _, f := range files {
            _ = f.Close()
        }
    }
}()

for _, p := range paths {
    f, openErr := os.Open(p)
    if openErr != nil {
        err = fmt.Errorf("open %q: %w", p, openErr)
        return
    }
    files = append(files, f)
}
```

`files` grows by exactly one entry per successful `os.Open`, so at the
moment the loop hits a failing path, `files` holds precisely the handles
that succeeded before it — no more, no less. Because `files` and `err` are
both named results, the deferred closure sees the same slice the loop built
and the same error the failing `os.Open` produced, and it needs no separate
bookkeeping to know how many handles to close: it just ranges over `files`.
The `if err != nil` guard is what keeps the success path untouched — when
every path opens cleanly, the closure runs but does nothing, and the caller
receives every handle still open.

Create `multiopen.go`:

```go
package multiopen

import (
	"fmt"
	"os"
)

// OpenAll opens every path in paths, in order. If any os.Open call fails, all
// files already opened for earlier paths are closed before OpenAll returns,
// so a caller that only checks err never leaks the handles that did succeed.
//
// files and err are named results: a single deferred closure runs after the
// failing return statement has copied its values into files and err, checks
// err, and — only then — unwinds by closing every handle collected so far.
// The success path leaves err nil, so the closure does nothing and the
// caller receives every opened file.
func OpenAll(paths []string) (files []*os.File, err error) {
	defer func() {
		if err != nil {
			for _, f := range files {
				_ = f.Close()
			}
		}
	}()

	for _, p := range paths {
		f, openErr := os.Open(p)
		if openErr != nil {
			err = fmt.Errorf("open %q: %w", p, openErr)
			return
		}
		files = append(files, f)
	}
	return
}
```

### The runnable demo

Create `cmd/demo/main.go`:

```go
package main

import (
	"fmt"
	"os"

	"example.com/multiopen"
)

func main() {
	// Relative paths keep the printed error message identical on every
	// run: os.Open reports exactly the string it was given, it does not
	// expand it to an absolute path.
	if err := os.MkdirAll("demodata", 0o755); err != nil {
		panic(err)
	}
	defer os.RemoveAll("demodata")
	os.WriteFile("demodata/a.txt", []byte("a"), 0o644)
	os.WriteFile("demodata/b.txt", []byte("b"), 0o644)

	files, err := multiopen.OpenAll([]string{"demodata/a.txt", "demodata/b.txt"})
	fmt.Printf("all present: opened=%d err=%v\n", len(files), err)
	for _, f := range files {
		f.Close()
	}

	files, err = multiopen.OpenAll([]string{"demodata/a.txt", "demodata/b.txt", "demodata/missing.txt"})
	fmt.Printf("one missing: opened=%d err=%v\n", len(files), err)
}
```

Run it:

```bash
go run ./cmd/demo
```

Expected output:

```
all present: opened=2 err=<nil>
one missing: opened=2 err=open "demodata/missing.txt": open demodata/missing.txt: no such file or directory
```

### Tests

Create `multiopen_test.go`:

```go
package multiopen

import (
	"errors"
	"os"
	"path/filepath"
	"testing"
)

func writeTempFile(t *testing.T, dir, name string) string {
	t.Helper()
	p := filepath.Join(dir, name)
	if err := os.WriteFile(p, []byte(name), 0o644); err != nil {
		t.Fatalf("WriteFile(%s): %v", p, err)
	}
	return p
}

func TestOpenAllSuccessReturnsEveryHandle(t *testing.T) {
	t.Parallel()

	dir := t.TempDir()
	a := writeTempFile(t, dir, "a.txt")
	b := writeTempFile(t, dir, "b.txt")

	files, err := OpenAll([]string{a, b})
	if err != nil {
		t.Fatalf("OpenAll: unexpected error: %v", err)
	}
	if len(files) != 2 {
		t.Fatalf("OpenAll returned %d files, want 2", len(files))
	}
	for _, f := range files {
		f.Close()
	}
}

func TestOpenAllUnwindsAlreadyOpenedFilesOnFailure(t *testing.T) {
	t.Parallel()

	dir := t.TempDir()
	a := writeTempFile(t, dir, "a.txt")
	b := writeTempFile(t, dir, "b.txt")
	missing := filepath.Join(dir, "does-not-exist.txt")

	files, err := OpenAll([]string{a, b, missing})
	if err == nil {
		t.Fatal("OpenAll: want error for missing path, got nil")
	}
	if len(files) != 2 {
		t.Fatalf("OpenAll returned %d files before failing, want 2", len(files))
	}

	// The two files opened before the failure must already be closed: a
	// read against a closed *os.File returns os.ErrClosed.
	buf := make([]byte, 1)
	for i, f := range files {
		_, readErr := f.Read(buf)
		if !errors.Is(readErr, os.ErrClosed) {
			t.Fatalf("files[%d].Read after unwind = %v, want os.ErrClosed", i, readErr)
		}
	}
}
```

## Review

`OpenAll` is correct when a fully successful call returns every handle open,
and a partial failure leaves nothing open behind it — no handle from the
successful prefix survives past the return. The test proves the latter
concretely rather than by inference: reading from an already-closed
`*os.File` returns `os.ErrClosed`, so `TestOpenAllUnwindsAlreadyOpenedFilesOnFailure`
can assert closure directly instead of trusting that `Close` was called. The
mistake to avoid is building `files` as a local slice and only assigning it
to the named result on success (`return localFiles, nil` at the bottom,
`return nil, err` on failure) — that discards the very handles the unwind
needs to close, turning "close what we opened" back into a leak on every
failure path.

## Resources

- [Go Spec: Defer statements](https://go.dev/ref/spec#Defer_statements)
- [`os.Open`](https://pkg.go.dev/os#Open)
- [`os.ErrClosed`](https://pkg.go.dev/os#pkg-variables)

---

Back to [00-concepts.md](00-concepts.md) | Prev: [30-range-loop-err-shadowing-defer-trap.md](30-range-loop-err-shadowing-defer-trap.md) | Next: [32-event-handler-post-process-on-success.md](32-event-handler-post-process-on-success.md)
