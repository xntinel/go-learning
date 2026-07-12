# Exercise 5: Recover Boundary That Re-Panics on Genuine Runtime Bugs

A boundary that recovers *everything* and continues is a liability: it turns a nil
dereference or an index-out-of-range — a genuine bug that probably left state
corrupted — into a silent success, hiding the defect until it surfaces as bad data
downstream. A mature boundary distinguishes a deliberate panic (recover it into an
error) from an involuntary `runtime.Error` (log it and re-panic, because it is a
real bug). This module builds that discriminating boundary.

This module is fully self-contained: its own `go mod init`, all code inline, its
own demo and tests. Nothing here imports another exercise.

## What you'll build

```text
rethrow/                     independent module: example.com/rethrow
  go.mod                     go 1.26
  rethrow.go                 ErrDeliberate; Guard(logger, fn) error
  cmd/
    demo/
      main.go                runnable demo: deliberate -> error; runtime fault -> re-panic
  rethrow_test.go            deliberate -> error; nil deref/OOB re-panics as runtime.Error; clean -> nil
```

Files: `rethrow.go`, `cmd/demo/main.go`, `rethrow_test.go`.
Implement: `Guard(logger *slog.Logger, fn func()) error` that recovers, and via `errors.As` against `runtime.Error` returns a deliberate panic as an error but logs and re-panics a genuine runtime fault.
Test: a deliberate sentinel panic returns a non-nil error with no re-panic; a nil dereference and an index-out-of-range each re-panic (caught by the test's outer recover) with a value satisfying `errors.As(&runtime.Error)` and a stack logged first; a clean call returns nil.
Verify: `go test -count=1 -race ./...`

Set up the module:

```bash
mkdir -p go-solutions/10-error-handling/08-panic-vs-error/05-rethrow-runtime-errors-vs-deliberate-panics/cmd/demo
cd go-solutions/10-error-handling/08-panic-vs-error/05-rethrow-runtime-errors-vs-deliberate-panics
go mod edit -go=1.26
```

### runtime.Error is the marker of a real bug

The Go runtime raises a `runtime.Error` for every involuntary fault: a nil pointer
dereference, an index or slice-bounds violation, a write to a nil map, a failed
type assertion, an integer divide-by-zero. `runtime.Error` is an interface that
embeds `error` and adds a `RuntimeError()` marker method; it exists precisely so
code can ask "was this a real fault, or a value someone chose to panic with?".

`Guard` uses that distinction as policy. It recovers, then checks whether the
recovered value is an `error` that `errors.As` can unwrap into a `runtime.Error`:

- If yes, this is an *involuntary fault* — a bug. Swallowing it would let the
  program continue with whatever half-mutated state the fault left behind. So Guard
  logs it with its stack (for the crash report) and re-panics, preserving fail-fast
  semantics. The bug crashes loudly instead of corrupting data quietly.
- If no, this is a *deliberate panic* — a value the code chose (here the sentinel
  `ErrDeliberate`, or any non-runtime value). Guard converts it to a returned error
  and the caller continues normally.

Using `errors.As` rather than a bare type assertion is deliberate: a runtime fault
that was wrapped (`fmt.Errorf("...: %w", runtimeErr)` somewhere down the stack)
still gets recognized, because `errors.As` walks the chain. Re-panicking with the
*original* recovered value keeps the true stack and type intact for whatever outer
boundary (or the runtime's own crash printer) handles it.

Create `rethrow.go`:

```go
package rethrow

import (
	"errors"
	"fmt"
	"log/slog"
	"runtime"
	"runtime/debug"
)

// ErrDeliberate is an example of a value code may intentionally panic with as a
// control signal that Guard converts into a returned error.
var ErrDeliberate = errors.New("deliberate control signal")

// Guard runs fn behind a recovery boundary with a discriminating policy:
//   - a deliberate panic (any value that is not a runtime.Error) becomes a
//     returned error;
//   - an involuntary runtime.Error (nil deref, index out of range, nil-map write,
//     etc.) is a genuine bug: it is logged with its stack and RE-PANICKED, never
//     swallowed.
func Guard(logger *slog.Logger, fn func()) (err error) {
	defer func() {
		r := recover()
		if r == nil {
			return
		}
		if e, ok := r.(error); ok {
			var re runtime.Error
			if errors.As(e, &re) {
				logger.Error("runtime fault at boundary; re-panicking",
					slog.Any("fault", re),
					slog.String("stack", string(debug.Stack())),
				)
				panic(r) // do not hide a real bug
			}
			err = fmt.Errorf("guard recovered: %w", e)
			return
		}
		err = fmt.Errorf("guard recovered: %v", r)
	}()
	fn()
	return nil
}
```

### The runnable demo

The demo shows all three branches. The runtime-fault case is wrapped in a local
recover so the demo can print that it propagated rather than crashing.

Create `cmd/demo/main.go`:

```go
package main

import (
	"fmt"
	"io"
	"log/slog"

	"example.com/rethrow"
)

func main() {
	logger := slog.New(slog.NewTextHandler(io.Discard, nil))

	// Deliberate panic -> converted to an error.
	err := rethrow.Guard(logger, func() { panic(rethrow.ErrDeliberate) })
	fmt.Println("deliberate:", err)

	// Clean run -> nil error.
	err = rethrow.Guard(logger, func() {})
	fmt.Println("clean:", err)

	// Runtime fault -> re-panicked, not swallowed. Caught here to prove it escaped.
	func() {
		defer func() {
			if r := recover(); r != nil {
				fmt.Println("runtime fault re-panicked, not swallowed")
			}
		}()
		var p *int
		_ = rethrow.Guard(logger, func() { _ = *p }) // nil dereference
	}()
}
```

Run it:

```bash
go run ./cmd/demo
```

Expected output:

```
deliberate: guard recovered: deliberate control signal
clean: <nil>
runtime fault re-panicked, not swallowed
```

### Tests

`TestDeliberateBecomesError` proves a chosen sentinel is recovered into a
classifiable error (via `errors.Is`) with no re-panic. `TestRuntimeFaultRePanics`
runs a nil deref and an index-out-of-range through `Guard`, catches the re-panic in
an outer deferred recover, and asserts the escaped value satisfies
`errors.As(&runtime.Error)` and that a stack was logged first. `TestCleanReturnsNil`
proves the happy path.

Create `rethrow_test.go`:

```go
package rethrow

import (
	"bytes"
	"errors"
	"log/slog"
	"runtime"
	"strings"
	"testing"
)

func TestDeliberateBecomesError(t *testing.T) {
	t.Parallel()
	logger := slog.New(slog.NewTextHandler(bytes.NewBuffer(nil), nil))

	err := Guard(logger, func() { panic(ErrDeliberate) })
	if err == nil {
		t.Fatal("deliberate panic should return a non-nil error")
	}
	if !errors.Is(err, ErrDeliberate) {
		t.Fatalf("err = %v, want it to wrap ErrDeliberate", err)
	}
}

func TestRuntimeFaultRePanics(t *testing.T) {
	t.Parallel()
	faults := map[string]func(){
		"nil deref": func() {
			var p *int
			_ = *p
		},
		"index out of range": func() {
			var s []int
			_ = s[3]
		},
	}
	for name, fault := range faults {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			var buf bytes.Buffer
			logger := slog.New(slog.NewTextHandler(&buf, nil))

			defer func() {
				r := recover()
				if r == nil {
					t.Fatal("runtime fault was swallowed; expected re-panic")
				}
				e, ok := r.(error)
				if !ok {
					t.Fatalf("re-panicked value %v is not an error", r)
				}
				var re runtime.Error
				if !errors.As(e, &re) {
					t.Fatalf("re-panicked value %v is not a runtime.Error", r)
				}
				if !strings.Contains(buf.String(), "runtime fault") {
					t.Fatalf("stack/fault not logged before re-panic: %q", buf.String())
				}
			}()

			_ = Guard(logger, fault)
			t.Fatal("Guard returned instead of re-panicking on a runtime fault")
		})
	}
}

func TestCleanReturnsNil(t *testing.T) {
	t.Parallel()
	logger := slog.New(slog.NewTextHandler(bytes.NewBuffer(nil), nil))
	if err := Guard(logger, func() {}); err != nil {
		t.Fatalf("clean call err = %v, want nil", err)
	}
}
```

## Review

The boundary is correct when it treats the two kinds of panic differently: a
deliberate value becomes a returned error the caller can classify with `errors.Is`,
while a `runtime.Error` is logged with its stack and re-panicked so the bug is not
hidden. The `errors.As` check (not a bare `r.(runtime.Error)` assertion) is what
makes it robust to a wrapped fault deep in the stack. Re-panicking the *original*
value keeps the fault's true type and stack for the outer handler. The anti-pattern
this exercise guards against is the tempting "recover everything and move on", which
converts a hard crash into silent corruption — `TestRuntimeFaultRePanics` fails if
you ever swallow a `runtime.Error`. Run `go test -race`.

## Resources

- [`runtime.Error`](https://pkg.go.dev/runtime#Error) — the interface marking involuntary runtime faults.
- [`errors.As`](https://pkg.go.dev/errors#As) — walking the wrap chain to find a `runtime.Error`.
- [Go Blog: Working with Errors in Go 1.13](https://go.dev/blog/go1.13-errors) — `As`/`Is` semantics over wrapped errors.

---

Back to [00-concepts.md](00-concepts.md) | Next: [06-panic-boundary-structured-error-with-stack.md](06-panic-boundary-structured-error-with-stack.md)
