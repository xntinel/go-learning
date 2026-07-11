# Exercise 9: Panic While Unwinding: Preserving the Original Error When Cleanup Also Panics

During a panic, deferred calls run as the stack unwinds — and if one of those
deferred calls (a `Close`, a `Rollback`, a `Flush`) panics too, the new panic
*replaces* the one in flight. "Last panic wins": your primary failure — the real
reason the operation died — is silently lost, and you are left debugging the
cleanup instead. This module builds the pattern that never loses the primary: run
cleanup defensively with its own recover, capture the primary panic first, and
merge both outcomes with `errors.Join`.

This module is fully self-contained: its own module, demo, and tests.

## What you'll build

```text
cleanup/                   independent module: example.com/cleanup
  go.mod                   go 1.26
  cleanup.go               Resource, safeClose, Do(r, op)
  cmd/
    demo/
      main.go              runnable demo: primary+clean, primary+close-panic, close-err
  cleanup_test.go          three cases; primary never clobbered by a close panic
```

Files: `cleanup.go`, `cmd/demo/main.go`, `cleanup_test.go`.
Implement: `Do(r *Resource, op func(*Resource) error) error` that guarantees `r.Close` runs, captures a primary panic from `op`, runs `Close` through a `safeClose` with its own recover, and merges both with `errors.Join`.
Test: primary panic + clean Close returns the primary; primary panic + Close that panics returns BOTH (`errors.Is` each); no primary + Close error surfaces just the cleanup error.
Verify: `go test -count=1 -race ./...`

Set up the module:

```bash
mkdir -p ~/go-exercises/cleanup/cmd/demo
cd ~/go-exercises/cleanup
go mod init example.com/cleanup
```

### Why the naive defer loses the primary error

Consider the obvious code: `defer r.Close()` inside a function that panics. As the
panic unwinds, `Close()` runs; if `Close` panics, that panic takes over and the
original is gone — the runtime only reports the last one. Even the non-panic
version is subtle: a `defer` that only returns `Close`'s error via a named return
will happily overwrite the primary error with the cleanup error.

`Do` fixes this with a specific structure. The deferred function runs cleanup
*first*, through `safeClose`, and only then calls `recover()` to pick up the
primary panic. `safeClose` has its own deferred `recover`: if `Close` panics,
`safeClose` catches it and returns it as an error (wrapped with `%w`), so a
Close-time panic becomes a value rather than a new in-flight panic. Crucially,
because `safeClose` is a normal nested call from within `Do`'s deferred function,
its inner recover does *not* steal `op`'s primary panic — that panic is paused
while deferred calls run, and only `safeClose`'s own panic (if any) is visible to
`safeClose`'s recover. After `safeClose` returns, `Do`'s `recover()` retrieves the
primary panic, and `Do` merges the primary with the cleanup error via
`errors.Join`.

`errors.Join` is the right tool: it drops `nil` members (so a clean Close adds
nothing) and builds a single error whose `Is`/`As` walks each member, so the caller
can `errors.Is` the primary failure *and* the cleanup failure out of one returned
value. The primary is never clobbered because it was captured before the merge, and
the cleanup outcome is never lost because it was captured before the recover. Both
survive.

Create `cleanup.go`:

```go
package cleanup

import (
	"errors"
	"fmt"
)

// Resource models a tx/file/conn whose Close may return an error or panic during
// unwinding. closePanic is used to simulate a cleanup that itself panics.
type Resource struct {
	name       string
	closeErr   error
	closePanic error
	closed     bool
}

// New builds a Resource. closeErr (if non-nil) is returned by Close; closePanic
// (if non-nil) is panicked by Close instead of returning.
func New(name string, closeErr, closePanic error) *Resource {
	return &Resource{name: name, closeErr: closeErr, closePanic: closePanic}
}

// Closed reports whether Close ran.
func (r *Resource) Closed() bool { return r.closed }

// Close releases the resource. It may return an error or panic.
func (r *Resource) Close() error {
	r.closed = true
	if r.closePanic != nil {
		panic(r.closePanic)
	}
	return r.closeErr
}

// safeClose runs Close with its own recover so a Close-time panic becomes a
// returned error instead of a new in-flight panic that clobbers the primary.
func safeClose(r *Resource) (err error) {
	defer func() {
		if rec := recover(); rec != nil {
			if e, ok := rec.(error); ok {
				err = fmt.Errorf("cleanup of %s panicked: %w", r.name, e)
				return
			}
			err = fmt.Errorf("cleanup of %s panicked: %v", r.name, rec)
		}
	}()
	return r.Close()
}

// panicToError converts a recovered primary panic into an error.
func panicToError(rec any) error {
	if e, ok := rec.(error); ok {
		return fmt.Errorf("operation panicked: %w", e)
	}
	return fmt.Errorf("operation panicked: %v", rec)
}

// Do runs op with r, guaranteeing r.Close runs and that neither a primary panic
// from op nor a failing/panicking Close is lost: both are merged with
// errors.Join, with the primary captured before cleanup can overwrite it.
func Do(r *Resource, op func(*Resource) error) (err error) {
	defer func() {
		closeErr := safeClose(r) // runs cleanup defensively, before recover
		if rec := recover(); rec != nil {
			err = errors.Join(panicToError(rec), closeErr)
			return
		}
		err = errors.Join(err, closeErr)
	}()
	return op(r)
}
```

### The runnable demo

The demo exercises three scenarios: an operation that panics while `Close` is
clean, an operation that panics while `Close` *also* panics (the footgun), and a
clean operation whose `Close` returns an error. It prints the merged error each
time, showing the primary is always present.

Create `cmd/demo/main.go`:

```go
package main

import (
	"errors"
	"fmt"

	"example.com/cleanup"
)

func main() {
	errPrimary := errors.New("write failed")
	errClosePanic := errors.New("connection reset on close")
	errCloseSoft := errors.New("flush returned error")

	// Primary panic, clean Close.
	e1 := cleanup.Do(cleanup.New("tx1", nil, nil), func(*cleanup.Resource) error {
		panic(errPrimary)
	})
	fmt.Printf("primary+cleanclose: %v | has primary: %v\n", e1, errors.Is(e1, errPrimary))

	// Primary panic AND Close panics: both must survive.
	e2 := cleanup.Do(cleanup.New("tx2", nil, errClosePanic), func(*cleanup.Resource) error {
		panic(errPrimary)
	})
	fmt.Printf("primary+closepanic: has primary: %v | has close: %v\n",
		errors.Is(e2, errPrimary), errors.Is(e2, errClosePanic))

	// No primary, Close returns an error.
	e3 := cleanup.Do(cleanup.New("tx3", errCloseSoft, nil), func(*cleanup.Resource) error {
		return nil
	})
	fmt.Printf("closeerr only: %v | has close: %v\n", e3, errors.Is(e3, errCloseSoft))
}
```

Run it:

```bash
go run ./cmd/demo
```

Expected output:

```
primary+cleanclose: operation panicked: write failed | has primary: true
primary+closepanic: has primary: true | has close: true
closeerr only: flush returned error | has close: true
```

### Tests

`TestPrimaryPanicCleanClose` (case A) asserts a primary panic with a clean Close
returns the primary and `Close` ran. `TestPrimaryPanicAndClosePanic` (case B) is
the footgun: both the primary and the Close panic must be reachable via
`errors.Is` from the single joined error. `TestCloseErrorOnly` (case C) asserts a
clean operation with a failing Close surfaces exactly the cleanup error.
`TestAllClean` confirms the happy path returns `nil`.

Create `cleanup_test.go`:

```go
package cleanup

import (
	"errors"
	"testing"
)

var (
	errPrimary    = errors.New("primary write failed")
	errClosePanic = errors.New("close reset the connection")
	errCloseSoft  = errors.New("close flush error")
)

func TestPrimaryPanicCleanClose(t *testing.T) {
	t.Parallel()

	r := New("a", nil, nil)
	err := Do(r, func(*Resource) error {
		panic(errPrimary)
	})

	if !errors.Is(err, errPrimary) {
		t.Fatalf("err = %v, want it to carry the primary", err)
	}
	if !r.Closed() {
		t.Fatal("Close must run even when the operation panics")
	}
}

func TestPrimaryPanicAndClosePanic(t *testing.T) {
	t.Parallel()

	r := New("b", nil, errClosePanic)
	err := Do(r, func(*Resource) error {
		panic(errPrimary)
	})

	if !errors.Is(err, errPrimary) {
		t.Fatalf("primary was clobbered by the close panic: %v", err)
	}
	if !errors.Is(err, errClosePanic) {
		t.Fatalf("close panic was lost: %v", err)
	}
}

func TestCloseErrorOnly(t *testing.T) {
	t.Parallel()

	r := New("c", errCloseSoft, nil)
	err := Do(r, func(*Resource) error {
		return nil
	})

	if !errors.Is(err, errCloseSoft) {
		t.Fatalf("err = %v, want the cleanup error", err)
	}
	if errors.Is(err, errPrimary) {
		t.Fatalf("err = %v, should not contain a primary", err)
	}
}

func TestAllClean(t *testing.T) {
	t.Parallel()

	r := New("d", nil, nil)
	err := Do(r, func(*Resource) error {
		return nil
	})

	if err != nil {
		t.Fatalf("err = %v, want nil", err)
	}
	if !r.Closed() {
		t.Fatal("Close should have run")
	}
}
```

## Review

`Do` is correct when the primary failure is present in the returned error no matter
what `Close` does, and a Close-time panic is captured rather than allowed to
replace the in-flight primary. The ordering in the deferred function is the whole
trick: call `safeClose` *before* `recover()`, because `safeClose`'s own recover only
sees a Close panic (a normal nested call cannot steal the paused primary panic), and
`Do`'s `recover()` then retrieves the primary. `errors.Join` merges them, dropping
`nil` cleanup outcomes and keeping both `Is`-reachable. The trap this closes is the
classic "last panic wins": a deferred `Close()` that panics during unwinding erases
the real error unless you capture it first and give cleanup its own recover.

## Resources

- [errors.Join](https://pkg.go.dev/errors#Join) — merging primary and cleanup errors so Is/As reaches each.
- [Go Language Specification: Handling panics](https://go.dev/ref/spec#Handling_panics) — a panic in a deferred call replaces the one being unwound.
- [Go Blog: Defer, Panic, and Recover](https://go.dev/blog/defer-panic-and-recover) — deferred execution order during unwinding.

---

Back to [00-concepts.md](00-concepts.md) | Next: [10-plugin-dispatch-boundary.md](10-plugin-dispatch-boundary.md)
