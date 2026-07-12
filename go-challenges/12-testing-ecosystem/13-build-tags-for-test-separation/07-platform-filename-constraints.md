# Exercise 7: OS-specific implementation and tests via filename constraints

Cross-platform adapters — a file lock, a terminal control, a path resolver — need
a different implementation per operating system, and the code that selects them
should never run at run time. Go does the selection at compile time through
filename constraints and `//go:build` lines. This module builds an advisory
file-lock adapter for a single-writer worker, with a Unix `flock(2)`
implementation and a Windows stub, and pins down exactly which of the two
mechanisms each side needs.

Self-contained module. On a Unix CI host the default `go test ./...` compiles and
runs the Unix path; `GOOS=windows go build` proves the Windows stub compiles
without a Windows host.

## What you'll build

```text
filelock/                  independent module: example.com/filelock
  go.mod
  lock.go                  OS-agnostic Lock (Open/Acquire/Release/Close) over *os.File
  lock_unix.go             //go:build unix: acquire/release via syscall.Flock
  lock_windows.go          filename-constrained Windows stub (no //go:build needed)
  lock_unix_test.go        //go:build unix: exclusive-lock behavior test
  cmd/
    demo/
      main.go              acquires and releases the lock on a temp file
```

- Files: `lock.go`, `lock_unix.go`, `lock_windows.go`, `lock_unix_test.go`, `cmd/demo/main.go`.
- Implement: a `Lock` whose OS-specific `acquire`/`release` come from a per-platform file, selected at compile time.
- Test: `lock_unix_test.go` proves a second, independent open cannot take the lock non-blockingly while the first holds it, then can after release.
- Verify: `go test -race ./...` on a Unix host; `GOOS=windows go build ./...` cross-compiles the stub.

Set up the module:

```bash
go mod edit -go=1.26
```

### Two selection mechanisms, and the trap between them

There are two ways to give a file an OS constraint, and this module deliberately
uses one of each so the difference is unmistakable:

- `lock_windows.go` uses an **implicit filename constraint**. Because `windows`
  is a real `GOOS` value, a file whose name ends in `_windows.go` is compiled
  only on Windows — no comment required. The `_GOOS.go`, `_GOARCH.go`, and
  `_GOOS_GOARCH.go` patterns all work this way, and the `_test` suffix combines
  with them.
- `lock_unix.go` uses an **explicit `//go:build unix` line**. Here is the trap
  that catches seniors: `unix` is a build *tag* (an aggregate of every Unix-like
  `GOOS`, since Go 1.19), but it is **not** a `GOOS` value, so the filename
  suffix `_unix.go` carries *no* implicit constraint at all. A file named
  `lock_unix.go` with no build line would compile on Windows too. To constrain to
  the Unix family you must write `//go:build unix` inside the file. The filename
  is then just a naming convention; the comment does the gating. The same applies
  to the test file, which is why `lock_unix_test.go` also carries the line.

The related trap the concepts file names is the *contradiction*: a file named
`foo_windows.go` that also carries `//go:build linux`. The implicit `windows`
from the name and the explicit `linux` from the comment are ANDed, so the file
compiles on no platform at all — a whole implementation silently vanishes. The
rule is to keep the filename and the `//go:build` line consistent, or use only
one of them. Here `lock_windows.go` has no `//go:build` line (avoiding any
disagreement), and `lock_unix.go` uses only the comment (since the filename gives
nothing).

### The adapter

`lock.go` is OS-agnostic: it opens the file and delegates the locking primitive
to `acquire`/`release`, which are declared once per platform. Advisory locking is
the right tool for a single-writer worker — a cron job, a migration runner, a log
compactor — that must guarantee only one instance mutates a resource. `flock(2)`
with `LOCK_NB` gives a non-blocking exclusive lock: a second instance learns
immediately that another holds it, rather than hanging.

Create `lock.go`:

```go
package filelock

import "os"

// Lock is an advisory, single-writer file lock for a worker that must be the
// only process writing a resource. The OS-specific acquire/release primitives
// live in lock_unix.go and lock_windows.go and are selected at compile time.
type Lock struct {
	f *os.File
}

// Open opens (creating if needed) the lock file at path.
func Open(path string) (*Lock, error) {
	f, err := os.OpenFile(path, os.O_CREATE|os.O_RDWR, 0o644)
	if err != nil {
		return nil, err
	}
	return &Lock{f: f}, nil
}

// Acquire takes the advisory lock without blocking, returning an error if it is
// already held by another open file description.
func (l *Lock) Acquire() error {
	return acquire(l.f)
}

// Release drops the advisory lock.
func (l *Lock) Release() error {
	return release(l.f)
}

// Close closes the underlying file (which also drops any held lock).
func (l *Lock) Close() error {
	return l.f.Close()
}
```

Create `lock_unix.go` — note the explicit `//go:build unix`, required because the
filename alone does not constrain:

```go
//go:build unix

package filelock

import (
	"os"
	"syscall"
)

// acquire takes a non-blocking exclusive advisory lock via flock(2).
func acquire(f *os.File) error {
	return syscall.Flock(int(f.Fd()), syscall.LOCK_EX|syscall.LOCK_NB)
}

// release drops the flock.
func release(f *os.File) error {
	return syscall.Flock(int(f.Fd()), syscall.LOCK_UN)
}
```

Create `lock_windows.go` — the `_windows.go` name is itself the constraint, so no
comment is needed:

```go
package filelock

import (
	"errors"
	"os"
)

// errUnsupported is a placeholder for this teaching stub. A production Windows
// adapter would call LockFileEx through golang.org/x/sys/windows.
var errUnsupported = errors.New("filelock: advisory locking not implemented on windows")

func acquire(f *os.File) error { return errUnsupported }

func release(f *os.File) error { return errUnsupported }
```

### The runnable demo

Create `cmd/demo/main.go`:

```go
package main

import (
	"fmt"
	"os"
	"path/filepath"

	"example.com/filelock"
)

func main() {
	path := filepath.Join(os.TempDir(), "filelock-demo.lock")
	l, err := filelock.Open(path)
	if err != nil {
		fmt.Println("open:", err)
		return
	}
	defer l.Close()

	if err := l.Acquire(); err != nil {
		fmt.Println("acquire:", err)
		return
	}
	fmt.Println("acquired advisory lock")

	if err := l.Release(); err != nil {
		fmt.Println("release:", err)
		return
	}
	fmt.Println("released advisory lock")
}
```

Run it on a Unix host:

```bash
go run ./cmd/demo
```

Expected output:

```text
acquired advisory lock
released advisory lock
```

### The Unix test

The test opens the same path twice (two independent open file descriptions) and
proves the exclusive, non-blocking semantics: while the first holds the lock the
second's `Acquire` fails, and after the first releases the second succeeds. It
carries `//go:build unix` because, again, the `_unix_test.go` filename does not
constrain on its own.

Create `lock_unix_test.go`:

```go
//go:build unix

package filelock

import (
	"path/filepath"
	"testing"
)

func TestAcquireIsExclusive(t *testing.T) {
	path := filepath.Join(t.TempDir(), "worker.lock")

	l1, err := Open(path)
	if err != nil {
		t.Fatalf("Open first: %v", err)
	}
	defer l1.Close()
	if err := l1.Acquire(); err != nil {
		t.Fatalf("first Acquire: %v", err)
	}

	l2, err := Open(path)
	if err != nil {
		t.Fatalf("Open second: %v", err)
	}
	defer l2.Close()
	if err := l2.Acquire(); err == nil {
		t.Fatal("second Acquire succeeded while first holds the lock; want failure")
	}

	if err := l1.Release(); err != nil {
		t.Fatalf("Release: %v", err)
	}
	if err := l2.Acquire(); err != nil {
		t.Fatalf("second Acquire after release: %v", err)
	}
}
```

### Cross-compiling the Windows path

You do not need a Windows machine to check the stub compiles — the toolchain
honors filename and `//go:build` constraints during cross-compilation:

```bash
GOOS=windows GOARCH=amd64 go build ./...
```

That build excludes `lock_unix.go` (its `unix` tag is false) and includes
`lock_windows.go`, so `acquire`/`release` resolve to the stub. If you instead
wrote `//go:build linux` inside `lock_windows.go`, this build would fail with
`undefined: acquire` — the contradiction trap, made visible.

## Review

The adapter is correct when exactly one of `lock_unix.go` and `lock_windows.go`
compiles per platform, so `acquire`/`release` are defined exactly once with no
redeclaration and no missing symbol. The Unix test proves the real `flock`
semantics: a second holder is refused non-blockingly and admitted only after the
first releases. The two lessons to carry forward are that a real `GOOS`/`GOARCH`
suffix constrains a file by itself while `unix` (a tag, not a `GOOS`) needs an
explicit `//go:build unix`, and that a filename constraint contradicting a
`//go:build` line ANDs to compile-nowhere. `GOOS=windows go build ./...` keeps the
Windows path honest from a Unix CI host, which is the only way a Unix-only
pipeline can stop the Windows adapter from rotting.

## Resources

- [go/build: Build Constraints (grammar and filename rules)](https://pkg.go.dev/go/build#hdr-Build_Constraints) — the `_GOOS`/`_GOARCH` filename rules and the `unix` tag.
- [syscall: Flock](https://pkg.go.dev/syscall#Flock) — the Unix advisory-lock primitive and its `LOCK_*` flags.
- [go command: Build constraints](https://pkg.go.dev/cmd/go#hdr-Build_constraints) — how cross-compilation honors `GOOS`/`GOARCH`.

---

Back to [00-concepts.md](00-concepts.md) | Previous: [06-driver-import-isolation.md](06-driver-import-isolation.md) | Next: [08-ignore-tag-seed-generator.md](08-ignore-tag-seed-generator.md)
