# Exercise 4: Turn a Worker Task Panic into a Returned Error

A worker pool, a job runner, or a plugin host runs code it does not fully trust. A
single `panic` in one task must not take down the pool. The fix is a wrapper that
recovers the panic and converts it into a returned error carrying the recovered
value and a captured stack — and that conversion is only possible because the
result is a named `err` the deferred closure can assign to.

This module is self-contained: its own `go mod init`, its own demo, its own tests.

## What you'll build

```text
safe/                       independent module: example.com/safe
  go.mod
  safe.go                   PanicError; SafeRun (recover -> named err)
  cmd/demo/
    main.go                 runnable demo: a pool where one task panics, pool survives
  safe_test.go              nil/pass-through/panic-string/panic-error cases, errors.As
```

- Files: `safe.go`, `cmd/demo/main.go`, `safe_test.go`.
- Implement: `SafeRun(task func() error) (err error)` that recovers a panic and stores it in the named `err` as a `*PanicError` carrying the recovered value and `debug.Stack()`.
- Test: task returns nil -> nil; task returns error -> passed through unchanged; task panics with a string and with an error -> non-nil `*PanicError` (via `errors.As`) exposing the value; the surrounding goroutine survives.
- Verify: `go test -count=1 -race ./...`

Set up the module:

```bash
mkdir -p ~/go-exercises/safe/cmd/demo
cd ~/go-exercises/safe
go mod init example.com/safe
```

### Recover into the named result

A panic value is only observable inside a deferred function via `recover()`, and
the only way to hand it back to the caller as a normal error is to assign it to a
named result. `SafeRun` is the whole pattern:

```go
func SafeRun(task func() error) (err error) {
	defer func() {
		if r := recover(); r != nil {
			err = &PanicError{Value: r, Stack: debug.Stack()}
		}
	}()
	return task()
}
```

If `task` returns normally, `return task()` sets `err` to whatever it returned, the
deferred `recover()` returns nil (no panic in flight), and that value passes
through untouched — a task that returns its own error is not disturbed. If `task`
panics, the panic propagates into the deferred closure, `recover()` returns the
panic value, and we overwrite the named `err` with a `*PanicError`. The caller of
`SafeRun` receives an ordinary error and never sees a panic.

Capturing `debug.Stack()` at recovery time is what makes the failure diagnosable —
without it you know a task panicked but not where. `PanicError` keeps both the raw
recovered value (so a caller can `errors.As` it back out and inspect it) and the
stack (for logging). Two disciplines matter in production: capture the stack *at
the recover site* (later is too late; the stack has unwound), and scope the recover
to exactly one task. A blanket recover that wraps unrelated code would swallow
nil-map dereferences and index-out-of-range bugs that should crash loudly in
development.

Create `safe.go`:

```go
package safe

import (
	"fmt"
	"runtime/debug"
)

// PanicError carries a value recovered from a panic together with the stack
// captured at recovery time.
type PanicError struct {
	Value any
	Stack []byte
}

// Error renders the recovered value. The stack is available on the field for
// logging but kept out of the one-line message.
func (e *PanicError) Error() string {
	return fmt.Sprintf("recovered panic: %v", e.Value)
}

// SafeRun runs task and converts a panic into a returned error, so one bad task
// cannot crash the pool. A task that returns an error normally is passed through
// unchanged. The recovered value can reach the caller only because err is a named
// result the deferred closure assigns to.
func SafeRun(task func() error) (err error) {
	defer func() {
		if r := recover(); r != nil {
			err = &PanicError{Value: r, Stack: debug.Stack()}
		}
	}()
	return task()
}
```

### The runnable demo

The demo models a small pool: it runs several tasks through `SafeRun`, one of which
panics, and prints the outcome of each. The point is that the loop keeps going —
the panic became an error, not a crash.

Create `cmd/demo/main.go`:

```go
package main

import (
	"errors"
	"fmt"

	"example.com/safe"
)

func main() {
	tasks := []func() error{
		func() error { return nil },
		func() error { return errors.New("db timeout") },
		func() error { panic("nil pointer in plugin") },
		func() error { return nil },
	}

	for i, task := range tasks {
		err := safe.SafeRun(task)
		switch {
		case err == nil:
			fmt.Printf("task %d: ok\n", i)
		default:
			fmt.Printf("task %d: %v\n", i, err)
		}
	}
	fmt.Println("pool survived all tasks")
}
```

Run it:

```bash
go run ./cmd/demo
```

Expected output:

```
task 0: ok
task 1: db timeout
task 2: recovered panic: nil pointer in plugin
pool survived all tasks
```

### Tests

The tests cover the four shapes and prove the goroutine survives. `errors.As`
recovers the `*PanicError` to inspect the original value, which distinguishes a
converted panic from an ordinary returned error.

Create `safe_test.go`:

```go
package safe

import (
	"errors"
	"fmt"
	"sync"
	"testing"
)

func TestSafeRunNil(t *testing.T) {
	t.Parallel()

	if err := SafeRun(func() error { return nil }); err != nil {
		t.Fatalf("SafeRun(nil task) = %v, want nil", err)
	}
}

func TestSafeRunPassesErrorThrough(t *testing.T) {
	t.Parallel()

	sentinel := errors.New("db timeout")
	err := SafeRun(func() error { return sentinel })
	if !errors.Is(err, sentinel) {
		t.Fatalf("SafeRun err = %v, want the task's own error", err)
	}
	var pe *PanicError
	if errors.As(err, &pe) {
		t.Fatal("a normal error was wrapped as a PanicError")
	}
}

func TestSafeRunRecoversStringPanic(t *testing.T) {
	t.Parallel()

	err := SafeRun(func() error { panic("boom") })
	var pe *PanicError
	if !errors.As(err, &pe) {
		t.Fatalf("SafeRun err = %v, want *PanicError", err)
	}
	if pe.Value != "boom" {
		t.Fatalf("PanicError.Value = %v, want boom", pe.Value)
	}
	if len(pe.Stack) == 0 {
		t.Fatal("PanicError.Stack is empty; stack was not captured")
	}
}

func TestSafeRunRecoversErrorPanic(t *testing.T) {
	t.Parallel()

	inner := errors.New("panic payload")
	err := SafeRun(func() error { panic(inner) })
	var pe *PanicError
	if !errors.As(err, &pe) {
		t.Fatalf("SafeRun err = %v, want *PanicError", err)
	}
	if got, ok := pe.Value.(error); !ok || !errors.Is(got, inner) {
		t.Fatalf("PanicError.Value = %v, want the panicked error", pe.Value)
	}
}

func TestSafeRunGoroutineSurvives(t *testing.T) {
	t.Parallel()

	var wg sync.WaitGroup
	results := make([]error, 4)
	for i := range results {
		wg.Add(1)
		go func() {
			defer wg.Done()
			results[i] = SafeRun(func() error { panic(fmt.Sprintf("task %d", i)) })
		}()
	}
	wg.Wait()

	for i, err := range results {
		var pe *PanicError
		if !errors.As(err, &pe) {
			t.Fatalf("result %d = %v, want recovered *PanicError", i, err)
		}
	}
}

func ExampleSafeRun() {
	err := SafeRun(func() error { panic("boom") })
	fmt.Println(err)
	// Output: recovered panic: boom
}
```

## Review

The wrapper is correct when a normal return (nil or error) passes through
untouched and a panic becomes a `*PanicError` carrying both the recovered value and
a non-empty stack. The `-race` run over the goroutine fan-out proves the real
property: each task's panic is contained to its own `SafeRun` call and none of them
crashes the process. The mistakes to avoid are losing the stack (recover into a
bare `fmt.Errorf("%v", r)` and you cannot tell where it blew up) and recovering too
broadly (a recover around unrelated code silently swallows bugs that should fail
loudly in a test). Keep the recover scoped to the single task and capture the stack
at the recover site.

## Resources

- [Go Blog: Defer, Panic, and Recover](https://go.dev/blog/defer-panic-and-recover)
- [`runtime/debug.Stack`](https://pkg.go.dev/runtime/debug#Stack)
- [`recover` builtin](https://pkg.go.dev/builtin#recover)
- [`errors.As`](https://pkg.go.dev/errors#As)

---

Back to [00-concepts.md](00-concepts.md) | Previous: [03-tx-commit-rollback-boundary.md](03-tx-commit-rollback-boundary.md) | Next: [05-close-error-not-lost.md](05-close-error-not-lost.md)
