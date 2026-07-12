# Exercise 3: The Import Job That Ran Out of File Descriptors

A batch importer opened each file in a loop and wrote `defer f.Close()` inside the
loop body. In staging it worked; in production a ten-thousand-file batch died with
`too many open files`. You will reproduce the leak with a fake opener that counts
live handles, diagnose the `defer` accumulation, and fix it by scoping cleanup to
each iteration.

## What you'll build

```text
importer/                  module example.com/importer
  go.mod
  importer.go              Opener interface; ImportAll; per-iteration importOne helper
  cmd/demo/
    main.go                runnable demo: imports real temp files via os.Open
  importer_test.go         fake opener with a live-handle counter; leak + error-path tests
```

- Files: `importer.go`, `cmd/demo/main.go`, `importer_test.go`.
- Implement: `ImportAll(o Opener, paths, process)` that opens each path, hands the reader to `process`, and closes the handle before the next iteration — with the `defer` scoped to a per-iteration helper so it fires per file, and a close error aggregated via `errors.Join`.
- Test: a fake opener that tracks live and max-concurrent handles, asserting `maxOpen == 1`; a mid-batch open failure that must still close everything opened before it.
- Verify: `go test -count=1 -race ./...`.

Set up the module:

```bash
mkdir -p go-solutions/03-control-flow/10-control-flow-debugging-challenge/03-defer-close-in-loop-fd-leak/cmd/demo
cd go-solutions/03-control-flow/10-control-flow-debugging-challenge/03-defer-close-in-loop-fd-leak
```

### The artifact and the planted bug

The importer walks a slice of paths, opens each, processes it, and moves on. The
version that shipped put the cleanup where it reads naturally but runs wrongly:

```go
func ImportAll(o Opener, paths []string, process func(name string, r io.Reader) error) error {
	for _, name := range paths {
		f, err := o.Open(name)
		if err != nil {
			return fmt.Errorf("open %s: %w", name, err)
		}
		defer f.Close() // BUG: runs at ImportAll return, not at end of this iteration
		if err := process(name, f); err != nil {
			return fmt.Errorf("process %s: %w", name, err)
		}
	}
	return nil
}
```

`defer f.Close()` does not run when the iteration ends — it runs when `ImportAll`
*returns*. So every open handle stays open until the entire batch finishes: at
iteration `i` there are `i + 1` descriptors live, and a large enough batch hits
the process's descriptor limit and fails. The same shape leaks database rows and
held locks. It survives review because on a three-file test batch nothing runs
out.

The failing leak test reads:

```text
--- FAIL: TestClosesEachBeforeNext (0.00s)
    importer_test.go:63: maxOpen = 5, want 1 (handles leaked across iterations)
```

The fix moves the open/process/close of a single file into its own helper, so the
`defer` scopes to *that* function's return — once per file. The helper closes on
every exit path, including the `process` error path, and folds any close error
into the returned error with `errors.Join`.

Create `importer.go`:

```go
package importer

import (
	"errors"
	"fmt"
	"io"
)

// Opener abstracts opening a named resource so tests can inject a fake and
// production can pass an os-backed opener.
type Opener interface {
	Open(name string) (io.ReadCloser, error)
}

// ImportAll opens each path in order, hands the reader to process, and closes
// the handle before moving to the next path. The first open or process error
// aborts the batch; the handle for the current path is always closed.
func ImportAll(o Opener, paths []string, process func(name string, r io.Reader) error) error {
	for _, name := range paths {
		if err := importOne(o, name, process); err != nil {
			return err
		}
	}
	return nil
}

// importOne opens exactly one file and guarantees it is closed before returning,
// because the defer is scoped to this function, not to ImportAll. A close error
// is joined onto whatever the body returned.
func importOne(o Opener, name string, process func(name string, r io.Reader) error) (err error) {
	f, err := o.Open(name)
	if err != nil {
		return fmt.Errorf("open %s: %w", name, err)
	}
	defer func() {
		if cerr := f.Close(); cerr != nil {
			err = errors.Join(err, fmt.Errorf("close %s: %w", name, cerr))
		}
	}()
	if perr := process(name, f); perr != nil {
		return fmt.Errorf("process %s: %w", name, perr)
	}
	return nil
}
```

Now at most one handle is live at any instant: `importOne` opens, processes, and
its `defer` closes before control returns to the loop for the next path. The named
return `err` lets the deferred close append its own failure without discarding a
process error.

### The runnable demo

The demo writes three real temp files and imports them with an `os.Open`-backed
opener, so you can see the whole path work against the filesystem.

Create `cmd/demo/main.go`:

```go
package main

import (
	"fmt"
	"io"
	"os"
	"path/filepath"

	"example.com/importer"
)

type osOpener struct{}

func (osOpener) Open(name string) (io.ReadCloser, error) { return os.Open(name) }

func main() {
	dir, err := os.MkdirTemp("", "import")
	if err != nil {
		fmt.Println("mktemp:", err)
		return
	}
	defer os.RemoveAll(dir)

	var paths []string
	for i := range 3 {
		p := filepath.Join(dir, fmt.Sprintf("f%d.txt", i))
		if err := os.WriteFile(p, []byte("data"), 0o644); err != nil {
			fmt.Println("write:", err)
			return
		}
		paths = append(paths, p)
	}

	err = importer.ImportAll(osOpener{}, paths, func(name string, r io.Reader) error {
		b, _ := io.ReadAll(r)
		fmt.Printf("imported %s (%d bytes)\n", filepath.Base(name), len(b))
		return nil
	})
	fmt.Printf("done err=%v\n", err)
}
```

Run it:

```bash
go run ./cmd/demo
```

Expected output:

```text
imported f0.txt (4 bytes)
imported f1.txt (4 bytes)
imported f2.txt (4 bytes)
done err=<nil>
```

### Tests

The fake opener is the instrument: it increments a live counter on `Open`,
decrements it on `Close`, and remembers the maximum ever live. With the leak, the
counter climbs to `len(paths)`; fixed, it never exceeds `1`.
`TestClosesEachBeforeNext` asserts one handle open during each `process` call and
`maxOpen == 1` overall. `TestClosesOnMidBatchError` makes an open fail mid-batch
and asserts every handle opened before the failure was closed. (Standard `go vet`
does not flag defer-in-loop, so the counting test — not the toolchain — is what
guards this.)

Create `importer_test.go`:

```go
package importer

import (
	"errors"
	"fmt"
	"io"
	"testing"
)

// fdTracker is a fake Opener that counts live and peak-concurrent handles.
type fdTracker struct {
	open        int
	maxOpen     int
	totalClosed int
	failOn      string // path whose Open returns an error
}

func (t *fdTracker) Open(name string) (io.ReadCloser, error) {
	if name == t.failOn {
		return nil, errors.New("no such file")
	}
	t.open++
	if t.open > t.maxOpen {
		t.maxOpen = t.open
	}
	return &countingCloser{tracker: t}, nil
}

type countingCloser struct {
	tracker *fdTracker
	closed  bool
}

func (c *countingCloser) Read(p []byte) (int, error) { return 0, io.EOF }

func (c *countingCloser) Close() error {
	if c.closed {
		return errors.New("double close")
	}
	c.closed = true
	c.tracker.open--
	c.tracker.totalClosed++
	return nil
}

func TestClosesEachBeforeNext(t *testing.T) {
	t.Parallel()

	tr := &fdTracker{}
	paths := []string{"a", "b", "c", "d", "e"}
	err := ImportAll(tr, paths, func(name string, r io.Reader) error {
		if tr.open != 1 {
			return fmt.Errorf("during %s: %d handles open, want 1", name, tr.open)
		}
		return nil
	})
	if err != nil {
		t.Fatalf("ImportAll: %v", err)
	}
	if tr.maxOpen != 1 {
		t.Fatalf("maxOpen = %d, want 1 (handles leaked across iterations)", tr.maxOpen)
	}
	if tr.totalClosed != len(paths) {
		t.Fatalf("totalClosed = %d, want %d", tr.totalClosed, len(paths))
	}
}

func TestClosesOnMidBatchError(t *testing.T) {
	t.Parallel()

	tr := &fdTracker{failOn: "c"}
	paths := []string{"a", "b", "c", "d"}
	err := ImportAll(tr, paths, func(name string, r io.Reader) error { return nil })
	if err == nil {
		t.Fatal("expected an error when opening c fails")
	}
	if tr.open != 0 {
		t.Fatalf("open = %d after the error, want 0 (all opened handles closed)", tr.open)
	}
	if tr.totalClosed != 2 {
		t.Fatalf("totalClosed = %d, want 2 (a and b)", tr.totalClosed)
	}
}

func ExampleImportAll() {
	tr := &fdTracker{}
	_ = ImportAll(tr, []string{"a", "b", "c"}, func(name string, r io.Reader) error {
		return nil
	})
	fmt.Println(tr.totalClosed, tr.maxOpen)
	// Output: 3 1
}
```

## Review

The importer is correct when at most one handle is live at any instant and every
opened handle is closed exactly once, on every exit path. The peak-handle counter
is the proof: `defer f.Close()` inside a loop leaves `maxOpen` equal to the batch
size, while a per-iteration helper holds it at `1`. The error-path test matters
because the tempting "just close at the end" refactor forgets the case where an
open fails partway through — the handles already open must still be released.
Remember the rule that makes all of this predictable: `defer` fires at *function*
return, so cleanup that must happen per iteration belongs in a function that
returns per iteration.

## Resources

- [Go Blog: Defer, Panic, and Recover](https://go.dev/blog/defer-panic-and-recover) — when deferred calls run.
- [os.Open and (*os.File).Close](https://pkg.go.dev/os#Open) — the descriptor-backed reader the demo uses.
- [errors.Join](https://pkg.go.dev/errors#Join) — aggregating a close error onto a body error.

---

Back to [00-concepts.md](00-concepts.md) | Prev: [02-bounded-retry-off-by-one.md](02-bounded-retry-off-by-one.md) | Next: [04-status-class-switch-fallthrough.md](04-status-class-switch-fallthrough.md)
