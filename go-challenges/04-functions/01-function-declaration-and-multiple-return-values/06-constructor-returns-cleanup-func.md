# Exercise 6: A Constructor That Returns Its Own Cleanup Function

"Open returns a close" is one of the most reused shapes in Go: a constructor
acquires a resource and hands back a first-class closure the caller defers to
release it. This exercise builds `Open(cfg) (*Spooler, func() error, error)` over
a real temp-file resource, with the strict ordering contract that on failure it
returns `(nil, nil, err)` and leaks nothing.

This module is fully self-contained: its own `go mod init`, all code inline, its
own demo and tests.

## What you'll build

```text
spooler/                   independent module: example.com/spooler
  go.mod                   go 1.25
  spooler.go               Open(cfg) (*Spooler, func() error, error); Write; the cleanup closure
  cmd/
    demo/
      main.go              opens a spooler, writes, then runs cleanup via defer
  spooler_test.go          success returns non-nil cleanup; cleanup removes the file; failure leaks nothing; -race
```

- Files: `spooler.go`, `cmd/demo/main.go`, `spooler_test.go`.
- Implement: `Open(cfg Config) (*Spooler, func() error, error)` that creates a temp file; the returned closure closes and removes it, composing both errors with `errors.Join`; on any failure return `(nil, nil, err)` and release the partial file internally.
- Test: a successful `Open` returns a non-nil spooler, a non-nil cleanup, nil error; calling cleanup removes the file and is safe once; a forced-failure `Open` returns `(nil, nil, err)` and leaves no temp file; a cleanup error is surfaced.
- Verify: `go test -count=1 -race ./...`

Set up the module:

```bash
go mod edit -go=1.25
```

### The ordering contract

The signature `Open(cfg) (*Spooler, func() error, error)` promises the caller a
`cleanup` they will `defer`. The whole value of the pattern is that the caller
never has to know *how* to release the resource — it just defers the closure the
constructor built. The contract that makes this safe is strict:

- On success return `(res, cleanup, nil)`, where `cleanup` releases everything
  `res` acquired.
- On failure return `(nil, nil, err)` and release any partially-acquired resource
  *inside* `Open`, before returning.

The failure path is where this goes wrong. If `Open` returns a non-nil `cleanup`
alongside a non-nil `err`, the caller is stuck: the idiom is
`if err != nil { return err }`, which skips the defer and leaks the partial
resource; or if the caller defers before checking, it defers a closure over a
half-built resource and may double-free or panic. So `Open` must clean up its own
mess on the error path and hand back `nil, nil`. Here that means: if we created
the temp file but a later step failed, we close and remove the file ourselves
before returning the error.

The cleanup closure *captures* the resource (the `*os.File`) — that is why it is a
closure and not a method: it carries the exact file handle to release, and the
caller does not need a reference to it. It composes the two possible release
errors (close, then remove) with `errors.Join`, so neither is silently dropped.

Create `spooler.go`:

```go
package spooler

import (
	"errors"
	"fmt"
	"os"
)

type Config struct {
	// Dir is where the spool file is created. Empty means the OS temp dir.
	Dir string
	// Pattern is the CreateTemp name pattern; a "*" is replaced by a random string.
	Pattern string
	// ForceFail simulates a post-create initialization failure so tests can
	// exercise the (nil, nil, err) path and confirm nothing leaks.
	ForceFail bool
}

// Spooler buffers writes to a temporary file.
type Spooler struct {
	f *os.File
}

// Write appends p to the spool file.
func (s *Spooler) Write(p []byte) (int, error) {
	return s.f.Write(p)
}

// Path reports the spool file's path (for tests and the demo).
func (s *Spooler) Path() string {
	return s.f.Name()
}

// Open creates the spool file and returns the spooler plus a cleanup closure the
// caller defers. On any failure it returns (nil, nil, err) and removes the
// partially-created file itself, so the caller never defers a partial cleanup.
func Open(cfg Config) (*Spooler, func() error, error) {
	pattern := cfg.Pattern
	if pattern == "" {
		pattern = "spool-*.log"
	}
	f, err := os.CreateTemp(cfg.Dir, pattern)
	if err != nil {
		return nil, nil, fmt.Errorf("create spool file: %w", err)
	}

	// A later initialization step could fail here. If it does, release the file
	// we already created before returning the error.
	if cfg.ForceFail {
		// Best-effort internal cleanup of the partial resource.
		_ = f.Close()
		_ = os.Remove(f.Name())
		return nil, nil, fmt.Errorf("initialize spooler: %w", errors.New("forced failure"))
	}

	s := &Spooler{f: f}
	cleanup := func() error {
		// Compose both release errors so neither is silently dropped.
		return errors.Join(f.Close(), os.Remove(f.Name()))
	}
	return s, cleanup, nil
}
```

### The runnable demo

Create `cmd/demo/main.go`:

```go
package main

import (
	"fmt"
	"os"

	"example.com/spooler"
)

func main() {
	s, cleanup, err := spooler.Open(spooler.Config{})
	if err != nil {
		panic(err)
	}
	defer func() {
		if err := cleanup(); err != nil {
			fmt.Println("cleanup error:", err)
		}
	}()

	path := s.Path()
	if _, err := s.Write([]byte("job-1\n")); err != nil {
		panic(err)
	}
	fmt.Printf("spooled to a temp file, exists=%t\n", fileExists(path))

	// After main returns, the deferred cleanup removes the file. We print the
	// path's existence now, before the defer runs.
	_ = path
}

func fileExists(path string) bool {
	_, err := os.Stat(path)
	return err == nil
}
```

Run it:

```bash
go run ./cmd/demo
```

Expected output:

```
spooled to a temp file, exists=true
```

### Tests

Create `spooler_test.go`:

```go
package spooler

import (
	"os"
	"testing"
)

func TestOpenSuccess(t *testing.T) {
	t.Parallel()
	s, cleanup, err := Open(Config{Dir: t.TempDir()})
	if err != nil {
		t.Fatalf("Open: unexpected error %v", err)
	}
	if s == nil || cleanup == nil {
		t.Fatalf("Open success returned s=%v cleanup!=nil=%v; want both non-nil", s, cleanup != nil)
	}
	path := s.Path()
	if _, err := os.Stat(path); err != nil {
		t.Fatalf("spool file should exist after Open: %v", err)
	}
	if err := cleanup(); err != nil {
		t.Fatalf("cleanup: unexpected error %v", err)
	}
	if _, err := os.Stat(path); !os.IsNotExist(err) {
		t.Fatalf("spool file should be gone after cleanup, stat err = %v", err)
	}
}

func TestOpenFailureLeaksNothing(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()

	s, cleanup, err := Open(Config{Dir: dir, ForceFail: true})
	if err == nil {
		t.Fatal("Open with ForceFail: want error, got nil")
	}
	if s != nil || cleanup != nil {
		t.Fatalf("failure path must return (nil, nil, err), got s=%v cleanup!=nil=%v", s, cleanup != nil)
	}

	// The partial file must have been removed internally: the dir is empty.
	entries, err := os.ReadDir(dir)
	if err != nil {
		t.Fatalf("ReadDir: %v", err)
	}
	if len(entries) != 0 {
		t.Fatalf("Open leaked %d file(s) on the failure path", len(entries))
	}
}

func TestCleanupSurfacesError(t *testing.T) {
	t.Parallel()
	s, cleanup, err := Open(Config{Dir: t.TempDir()})
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	// Remove the file out from under the spooler, then cleanup's os.Remove fails
	// and errors.Join surfaces it rather than swallowing it.
	if err := os.Remove(s.Path()); err != nil {
		t.Fatalf("pre-remove: %v", err)
	}
	if err := cleanup(); err == nil {
		t.Fatal("cleanup should surface the failed os.Remove, got nil")
	}
}
```

## Review

The constructor is correct when the success path returns a usable spooler and a
non-nil cleanup that removes the file, and the failure path returns
`(nil, nil, err)` with no file left behind. `TestOpenFailureLeaksNothing` is the
load-bearing test: it reads the temp dir after a forced failure and asserts it is
empty, proving `Open` released the partial file itself instead of handing the
caller a cleanup it could not safely defer.

The mistakes are all about the failure path. Returning a non-nil cleanup with a
non-nil error forces the caller into a leak or a double-free — always return
`nil, nil` on error and clean up internally. And do not swallow the cleanup's own
error: `TestCleanupSurfacesError` proves the closure surfaces a failed
`os.Remove` via `errors.Join`, so a caller that logs `cleanup()`'s return sees a
real problem instead of a silent one.

## Resources

- [os.CreateTemp](https://pkg.go.dev/os#CreateTemp) — creating the temp file the cleanup closure removes.
- [errors.Join](https://pkg.go.dev/errors#Join) — composing the close and remove errors without dropping either.
- [testing.T.TempDir](https://pkg.go.dev/testing#T.TempDir) — a per-test directory auto-removed after the test.

---

Back to [00-concepts.md](00-concepts.md) | Previous: [05-comma-ok-type-assertion-payload.md](05-comma-ok-type-assertion-payload.md) | Next: [07-variadic-sql-in-clause-builder.md](07-variadic-sql-in-clause-builder.md)
