# Exercise 9: Named Return Values for Deferred Error Cleanup

Named returns earn their keep in exactly one common situation: a deferred closure
must observe and possibly overwrite the returned error, so a failed `Close` or
flush surfaces even when the body succeeded. This exercise builds an `ExportReport`
that writes to an `io.WriteCloser` and correctly promotes a close failure, dodging
the `:=`-inside-defer shadowing trap.

This module is fully self-contained. It has its own `go mod init`, defines every
type it needs, and ships its own demo and tests. Nothing here imports any other
exercise.

## What you'll build

```text
exporter/                      independent module: example.com/exporter
  go.mod                       module example.com/exporter
  exporter.go                  ExportReport(w, rows) (n int, err error) with deferred close
  cmd/
    demo/
      main.go                  exports to a failing closer and prints the surfaced error
  exporter_test.go             close-fails, write-fails, both-fail (errors.Join) cases
```

- Files: `exporter.go`, `cmd/demo/main.go`, `exporter_test.go`.
- Implement: `ExportReport(w io.WriteCloser, rows []string) (n int, err error)` whose deferred closure sets `err` from a failed `Close` only when the body succeeded, without clobbering a successful result.
- Test: a fake closer whose `Close` fails after a good write surfaces the close error; a write failure keeps its error and still closes; both failing reports both via `errors.Join`.
- Verify: `go test -count=1 -race ./...`

Set up the module:

```bash
mkdir -p go-solutions/02-variables-types-and-constants/01-variable-declaration-and-short-assignment/09-named-returns-deferred-cleanup/cmd/demo
cd go-solutions/02-variables-types-and-constants/01-variable-declaration-and-short-assignment/09-named-returns-deferred-cleanup
```

### Why named returns here, and only here

A deferred closure runs after the `return` statement has set the result values but
before the function actually returns to the caller. To *change* the returned error
from inside a `defer`, the closure must be able to name and assign it — and only a
*named* return value is visible and assignable there. That is the entire
justification for named returns in this function: `func ExportReport(...) (n int, err error)`
lets a deferred `Close` promote its own failure into `err`. Without a named `err`,
the deferred closure would have no way to affect what the caller sees, and a failed
flush would vanish.

This is the narrow case. Outside deferred-cleanup error mutation, prefer explicit
returns; named returns spread through a long function with naked `return`s obscure
what is being returned. The point is not "name your returns" — it is "name them when
a defer must touch them".

### The two traps

First, the closure must not overwrite a successful result with a close error only
when there was no prior error, and must not lose the primary error when both fail.
The idiom is: if the body already failed, keep that error (optionally join the close
error); if the body succeeded but `Close` failed, promote the close error. Second —
the shadowing trap the concepts file warns about — the closure must assign the
*outer* `err` with `=`, never redeclare it with `:=`:

```go
// WRONG: cerr, err := ... would declare a new err local to the closure.
defer func() {
	if cerr := w.Close(); cerr != nil && err == nil {
		err = cerr // = assigns the named return; a := here would shadow it
	}
}()
```

If you wrote `err := w.Close()` inside the closure, you would create a fresh `err`
that the closure discards on exit, and the caller would still see the old value. The
`=` is load-bearing.

Create `exporter.go`:

```go
package exporter

import (
	"errors"
	"fmt"
	"io"
)

// ExportReport writes each row followed by a newline to w, then closes it. It uses
// named returns so the deferred Close can surface a flush failure: if the body
// succeeded, a Close error becomes err; if the body failed, both are reported.
func ExportReport(w io.WriteCloser, rows []string) (n int, err error) {
	defer func() {
		cerr := w.Close()
		if cerr == nil {
			return
		}
		if err == nil {
			err = fmt.Errorf("close: %w", cerr) // promote: body was fine, flush failed
		} else {
			err = errors.Join(err, fmt.Errorf("close: %w", cerr)) // keep both
		}
	}()

	for _, row := range rows {
		written, werr := fmt.Fprintln(w, row)
		n += written
		if werr != nil {
			return n, fmt.Errorf("write row: %w", werr)
		}
	}
	return n, nil
}
```

Note `written, werr :=` inside the loop is a genuinely local pair — there is no
outer `written`/`werr` to shadow, and `n` is accumulated with `+=` on the named
return. The only `=` on `err` happens in the deferred closure, exactly where the
promotion must land.

### The runnable demo

The demo exports to a closer whose `Close` fails after the body writes cleanly, so
you can watch `ExportReport` surface the close error that a non-named-return version
would drop.

Create `cmd/demo/main.go`:

```go
package main

import (
	"errors"
	"fmt"
	"io"
	"strings"

	"example.com/exporter"
)

// failCloser writes to an underlying builder but fails to close.
type failCloser struct {
	b        *strings.Builder
	closeErr error
}

func (f *failCloser) Write(p []byte) (int, error) { return f.b.Write(p) }
func (f *failCloser) Close() error                { return f.closeErr }

func main() {
	var sb strings.Builder
	fc := &failCloser{b: &sb, closeErr: errors.New("disk full on flush")}

	n, err := exporter.ExportReport(fc, []string{"order-1", "order-2"})
	fmt.Printf("wrote %d bytes\n", n)
	fmt.Printf("error surfaced: %v\n", err)
	fmt.Printf("body content: %q\n", sb.String())

	var _ io.WriteCloser = fc
}
```

Run it:

```bash
go run ./cmd/demo
```

Expected output:

```
wrote 16 bytes
error surfaced: close: disk full on flush
body content: "order-1\norder-2\n"
```

The body wrote all 16 bytes successfully, yet `ExportReport` still returns the close
error — the whole reason the named return exists. A version returning `n, nil`
explicitly would silently report success on a failed flush.

### Tests

Create `exporter_test.go`:

```go
package exporter

import (
	"errors"
	"fmt"
	"strings"
	"testing"
)

// fakeCloser records writes and returns configurable errors.
type fakeCloser struct {
	b        strings.Builder
	writeErr error
	closeErr error
	closed   bool
}

func (f *fakeCloser) Write(p []byte) (int, error) {
	if f.writeErr != nil {
		return 0, f.writeErr
	}
	return f.b.Write(p)
}

func (f *fakeCloser) Close() error {
	f.closed = true
	return f.closeErr
}

func TestExportSurfacesCloseError(t *testing.T) {
	t.Parallel()
	closeErr := errors.New("flush failed")
	fc := &fakeCloser{closeErr: closeErr}

	_, err := ExportReport(fc, []string{"a", "b"})
	if !errors.Is(err, closeErr) {
		t.Fatalf("err = %v, want it to wrap the close error", err)
	}
	if !fc.closed {
		t.Fatal("Close was never called")
	}
}

func TestExportSuccessReturnsNilAndCloses(t *testing.T) {
	t.Parallel()
	fc := &fakeCloser{}

	n, err := ExportReport(fc, []string{"a", "b"})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if n != 4 { // "a\n" + "b\n"
		t.Fatalf("n = %d, want 4", n)
	}
	if !fc.closed {
		t.Fatal("Close was never called on success")
	}
	if got := fc.b.String(); got != "a\nb\n" {
		t.Fatalf("body = %q, want \"a\\nb\\n\"", got)
	}
}

func TestExportWriteErrorStillCloses(t *testing.T) {
	t.Parallel()
	writeErr := errors.New("connection reset")
	fc := &fakeCloser{writeErr: writeErr}

	_, err := ExportReport(fc, []string{"a"})
	if !errors.Is(err, writeErr) {
		t.Fatalf("err = %v, want the write error", err)
	}
	if !fc.closed {
		t.Fatal("Close must run even after a write failure")
	}
}

func TestExportBothFailReportsBoth(t *testing.T) {
	t.Parallel()
	writeErr := errors.New("write boom")
	closeErr := errors.New("close boom")
	fc := &fakeCloser{writeErr: writeErr, closeErr: closeErr}

	_, err := ExportReport(fc, []string{"a"})
	if !errors.Is(err, writeErr) {
		t.Fatalf("err lost the write error: %v", err)
	}
	if !errors.Is(err, closeErr) {
		t.Fatalf("err lost the close error: %v", err)
	}
}

func ExampleExportReport() {
	fc := &fakeCloser{}
	n, err := ExportReport(fc, []string{"row"})
	fmt.Println(n, err)
	// Output: 4 <nil>
}
```

`TestExportBothFailReportsBoth`
proves the `errors.Join` branch: when the body and the close both fail, the returned
error matches both, so neither is dropped.

## Review

`ExportReport` is correct when the named `err` is the single value both the body and
the deferred `Close` write to, and the closure uses `=` (never `:=`) to promote a
close failure only when the body succeeded, or `errors.Join` when both fail. The
close-error test is the direct guard: a version that returns `n, nil` explicitly
would report success on a failed flush.

The mistakes to avoid: `cerr, err :=` inside the deferred closure (shadows the named
return, so the promotion is lost), overwriting a primary error with a close error
(losing the root cause), and reaching for named returns outside this
deferred-cleanup pattern. Run `go test -race` to confirm all four paths.

## Resources

- [Go Specification: Return statements (named result parameters)](https://go.dev/ref/spec#Return_statements)
- [Effective Go: Deferred functions and named returns](https://go.dev/doc/effective_go#defer)
- [errors.Join](https://pkg.go.dev/errors#Join)
- [io.WriteCloser](https://pkg.go.dev/io#WriteCloser)

---

Back to [00-concepts.md](00-concepts.md) | Prev: [08-if-switch-init-scope.md](08-if-switch-init-scope.md) | Next: [../02-zero-values-and-default-initialization/00-concepts.md](../02-zero-values-and-default-initialization/00-concepts.md)
