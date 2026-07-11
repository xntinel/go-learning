# Exercise 6: PanicError — Convert a Recovered Panic into an Observable Error

A recovered panic logged as `fmt.Sprintf("%v", r)` has lost the stack — the one
thing on-call needs. This module builds a `PanicError` type that carries the
recovered value and the captured stack, implements the `error` interface, and
`Unwrap`s to the underlying error when the panic value was itself an error — so
your observability pipeline gets a first-class, wrappable error instead of a lost
trace.

This module is fully self-contained: its own `go mod init`, all code inline, its
own demo and tests. Nothing here imports another exercise.

## What you'll build

```text
panicerr/                    independent module: example.com/panicerr
  go.mod                     go 1.26
  panicerr.go                PanicError{Value, Stack}; Error, Unwrap; Guard(fn) error
  cmd/
    demo/
      main.go                runnable demo: panic -> *PanicError with stack; error passes through
  panicerr_test.go           errors.As extracts PanicError; Stack non-empty; errors.Is via Unwrap; passthrough
```

Files: `panicerr.go`, `cmd/demo/main.go`, `panicerr_test.go`.
Implement: a `PanicError` type (`Value any`, `Stack []byte`) implementing `error` and `Unwrap`, and `Guard(fn func() error) error` that wraps any panic into a `*PanicError` (preserving an underlying error via `Unwrap`) while passing a normally-returned error through unchanged.
Test: `Guard` over a panicking fn returns a `*PanicError` (via `errors.As`) with a non-empty `Stack` and an `Error()` naming the value; when the panic value is itself an error, `errors.Is` finds it through `Unwrap`; a fn returning a normal error is passed through, not wrapped; a clean fn returns nil.
Verify: `go test -count=1 -race ./...`

Set up the module:

```bash
mkdir -p ~/go-exercises/panicerr/cmd/demo
cd ~/go-exercises/panicerr
go mod init example.com/panicerr
go mod edit -go=1.26
```

### A panic deserves to be a real error type

A boundary that recovers should not flatten the panic into a string. It should
produce a value your error handling already knows how to consume: an `error` that
composes with `errors.Is`, `errors.As`, and `%w` wrapping. `PanicError` is that
value. It stores the recovered `Value any` and the `Stack []byte` captured at the
recovery site (via `runtime/debug.Stack()`, which must be called inside the
deferred function while the frames still exist). `Error()` renders the value for
logs; `Unwrap()` returns the underlying error *when the panic value was itself an
error*, so `errors.Is(guardErr, someSentinel)` works transparently for a
`panic(io.EOF)` and returns `nil` (no unwrap) for a `panic("boom")`.

`Guard(fn func() error) error` is the boundary. Note the two paths it must keep
distinct. On a panic, the deferred closure builds a `*PanicError` and assigns the
named return. On a *normal* return — including one where `fn` returned an ordinary
error — `Guard` must pass that error through *unchanged*, not wrap it as a
`PanicError`. Only an actual panic becomes a `PanicError`; a returned error is not
a panic and stays exactly what it was. Because `err` is a named return,
`return fn()` sets it, and the deferred closure overwrites it only when `recover()`
sees a live panic.

Create `panicerr.go`:

```go
package panicerr

import (
	"fmt"
	"runtime/debug"
)

// PanicError is a recovered panic promoted to a first-class error: it carries the
// recovered value and the stack captured at the recovery site, and unwraps to the
// underlying error when the panic value was itself an error.
type PanicError struct {
	Value any
	Stack []byte
}

func (e *PanicError) Error() string {
	return fmt.Sprintf("recovered panic: %v", e.Value)
}

// Unwrap exposes the underlying error when the panic value was an error, so
// errors.Is/As see through the PanicError to it.
func (e *PanicError) Unwrap() error {
	if err, ok := e.Value.(error); ok {
		return err
	}
	return nil
}

// Guard runs fn behind a recovery boundary. A panic becomes a *PanicError
// carrying the value and stack; a normally-returned error (or nil) is passed
// through unchanged.
func Guard(fn func() error) (err error) {
	defer func() {
		if r := recover(); r != nil {
			err = &PanicError{Value: r, Stack: debug.Stack()}
		}
	}()
	return fn()
}
```

### The runnable demo

Create `cmd/demo/main.go`:

```go
package main

import (
	"errors"
	"fmt"
	"io"

	"example.com/panicerr"
)

func main() {
	// A panic with a plain string.
	err := panicerr.Guard(func() error { panic("nil session token") })
	var pe *panicerr.PanicError
	if errors.As(err, &pe) {
		fmt.Printf("got PanicError: %v (stack captured: %t)\n", pe.Value, len(pe.Stack) > 0)
	}

	// A panic whose value is itself an error: errors.Is sees through Unwrap.
	err = panicerr.Guard(func() error { panic(io.EOF) })
	fmt.Println("is io.EOF:", errors.Is(err, io.EOF))

	// A normally-returned error passes through unchanged.
	sentinel := errors.New("normal failure")
	err = panicerr.Guard(func() error { return sentinel })
	fmt.Println("passthrough equals sentinel:", err == sentinel)

	// A clean call returns nil.
	err = panicerr.Guard(func() error { return nil })
	fmt.Println("clean err:", err)
}
```

Run it:

```bash
go run ./cmd/demo
```

Expected output:

```
got PanicError: nil session token (stack captured: true)
is io.EOF: true
passthrough equals sentinel: true
clean err: <nil>
```

### Tests

Create `panicerr_test.go`:

```go
package panicerr

import (
	"errors"
	"fmt"
	"io"
	"strings"
	"testing"
)

func TestGuardWrapsPanic(t *testing.T) {
	t.Parallel()
	err := Guard(func() error { panic("boom") })

	var pe *PanicError
	if !errors.As(err, &pe) {
		t.Fatalf("err = %v, want *PanicError", err)
	}
	if len(pe.Stack) == 0 {
		t.Fatal("PanicError.Stack is empty; stack not captured at recovery site")
	}
	if !strings.Contains(pe.Error(), "boom") {
		t.Fatalf("Error() = %q, want it to name the value %q", pe.Error(), "boom")
	}
}

func TestGuardUnwrapsErrorPanic(t *testing.T) {
	t.Parallel()
	err := Guard(func() error { panic(io.EOF) })

	if !errors.Is(err, io.EOF) {
		t.Fatalf("errors.Is(err, io.EOF) = false; Unwrap not exposing the value")
	}
	var pe *PanicError
	if !errors.As(err, &pe) {
		t.Fatalf("err = %v, want *PanicError", err)
	}
}

func TestGuardPassesNormalErrorThrough(t *testing.T) {
	t.Parallel()
	sentinel := errors.New("normal")
	err := Guard(func() error { return sentinel })

	if err != sentinel {
		t.Fatalf("err = %v, want the sentinel passed through unchanged", err)
	}
	var pe *PanicError
	if errors.As(err, &pe) {
		t.Fatal("a normally-returned error must not be wrapped as *PanicError")
	}
}

func TestGuardCleanReturnsNil(t *testing.T) {
	t.Parallel()
	if err := Guard(func() error { return nil }); err != nil {
		t.Fatalf("clean Guard err = %v, want nil", err)
	}
}

func ExampleGuard() {
	err := Guard(func() error { panic("boom") })
	var pe *PanicError
	fmt.Println(errors.As(err, &pe), pe.Value)
	// Output: true boom
}
```

## Review

`PanicError` is correct when a recovered panic becomes a value your error tooling
already understands: `errors.As` extracts it, its `Stack` is populated (because
`debug.Stack()` runs inside the deferred function), and `Unwrap` exposes the
underlying error so `errors.Is(io.EOF)` works for a `panic(io.EOF)`. The subtle
guarantee is the passthrough: `Guard` wraps only actual panics, so a fn that
*returns* an error hands back that exact error, never a spurious `PanicError` — the
`TestGuardPassesNormalErrorThrough` case pins it. This is the type that makes a
recovery boundary observable instead of a black hole. Run `go test -race`.

## Resources

- [`errors.As` / `errors.Unwrap`](https://pkg.go.dev/errors#As) — extracting and unwrapping typed errors.
- [`runtime/debug.Stack`](https://pkg.go.dev/runtime/debug#Stack) — the stack captured for the `Stack` field.
- [Go Blog: Working with Errors in Go 1.13](https://go.dev/blog/go1.13-errors) — implementing `Unwrap` for `Is`/`As`.

---

Back to [00-concepts.md](00-concepts.md) | Next: [07-defer-rollback-transaction-on-panic.md](07-defer-rollback-transaction-on-panic.md)
