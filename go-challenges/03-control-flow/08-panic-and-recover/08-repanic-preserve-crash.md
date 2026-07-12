# Exercise 8: Selective Recovery: Handle What You Can, Re-Panic the Rest

A recovery boundary that swallows *every* panic is how you blind your on-call: a
real bug — a nil deref, a broken invariant — gets downgraded into a clean error
response, the crash-reporter never fires, and the defect ships. The disciplined
boundary recovers to *inspect*, captures the stack once at the moment of recovery,
decides via a predicate whether the panic is something it should own, and
re-panics the original value for everything else so the process crash still
happens. This module builds `Guard`, that selective boundary.

This module is fully self-contained: its own module, demo, and tests.

## What you'll build

```text
guard/                     independent module: example.com/guard
  go.mod                   go 1.26
  guard.go                 Report, Guard(onPanic, fn), isRuntime
  cmd/
    demo/
      main.go              runnable demo: app panic returns; runtime bug re-panics
  guard_test.go            app panic converted; runtime error re-panicked once
```

Files: `guard.go`, `cmd/demo/main.go`, `guard_test.go`.
Implement: `Guard(onPanic func(Report), fn func()) error` that captures `debug.Stack()` once, reports via `onPanic`, converts an application `panic(error)` to a returned error, and re-panics anything that is not application-recoverable (runtime bugs, non-errors).
Test: a recoverable app panic is converted with no re-panic (an outer recover must not fire); a `runtime.Error` re-panics the same value and `onPanic` was called exactly once with a captured stack.
Verify: `go test -count=1 -race ./...`

Set up the module:

```bash
mkdir -p go-solutions/03-control-flow/08-panic-and-recover/08-repanic-preserve-crash/cmd/demo
cd go-solutions/03-control-flow/08-panic-and-recover/08-repanic-preserve-crash
```

### Recover to classify, then decide

`Guard` recovers in a deferred function and immediately calls `debug.Stack()` —
the very first thing, because the stack is only meaningful before any other call
runs in the deferred function. It stores that stack in a `Report` and hands it to
`onPanic`, so the recovery is observable no matter which branch it takes next
(even the re-panic path has already reported). Then it decides: if the recovered
value is an `error` and *not* a `runtime.Error`, it is an application panic the
boundary can own — `Guard` wraps it with `%w` and returns it as a normal error. If
the value is a `runtime.Error` (a real bug) or a non-error (a bare string), `Guard`
re-panics the original value.

The predicate is `isRuntime`, which uses `errors.As(err, &runtimeErr)` — the same
interface match from the classification module. Re-panicking with the *original*
value (`panic(rec)`, not a new error) matters: it preserves the value's identity so
an outer handler, the runtime's crash printer, and any crash-reporter all see
exactly what was thrown. The stack was captured once, before the re-panic, so the
recorded trace still points at the panic site and does not get shifted by the
re-raise.

This is the pattern behind a production boundary that both keeps serving on
recoverable conditions and *still crashes loudly* on genuine bugs. Swallowing an
unclassified panic is the anti-pattern; `Guard` is the fix.

Create `guard.go`:

```go
package guard

import (
	"errors"
	"fmt"
	"runtime"
	"runtime/debug"
)

// Report is what a boundary records at the moment of recovery.
type Report struct {
	Value any
	Stack []byte
}

// isRuntime reports whether err is a runtime-generated panic (a code bug).
func isRuntime(err error) bool {
	var re runtime.Error
	return errors.As(err, &re)
}

// Guard runs fn, converting a recoverable application panic(error) into a
// returned error while re-panicking anything it should not own (runtime bugs,
// non-error values) so the crash and crash-reporter still fire. The stack is
// captured exactly once, at recovery time, and reported via onPanic on every
// panic path.
func Guard(onPanic func(Report), fn func()) (err error) {
	defer func() {
		rec := recover()
		if rec == nil {
			return
		}
		stack := debug.Stack() // capture once, before any other call
		onPanic(Report{Value: rec, Stack: stack})

		if e, ok := rec.(error); ok && !isRuntime(e) {
			err = fmt.Errorf("recovered application panic: %w", e)
			return
		}
		panic(rec) // not ours to own: re-raise the original value
	}()
	fn()
	return nil
}
```

### The runnable demo

The demo runs `Guard` twice: once around an application `panic(error)`, which
returns cleanly, and once around a nil dereference, whose re-panic the demo catches
in its own outer recover so the process stays alive to print the outcome.

Create `cmd/demo/main.go`:

```go
package main

import (
	"errors"
	"fmt"

	"example.com/guard"
)

func main() {
	report := func(r guard.Report) {
		fmt.Printf("reported: %v\n", r.Value)
	}

	// Application panic: Guard owns it and returns an error.
	err := guard.Guard(report, func() {
		panic(errors.New("quota exceeded"))
	})
	fmt.Printf("app panic returned: %v\n", err)

	// Runtime bug: Guard re-panics; we catch it here to keep the demo running.
	func() {
		defer func() {
			if rec := recover(); rec != nil {
				fmt.Printf("runtime bug re-panicked to caller: %v\n", rec)
			}
		}()
		_ = guard.Guard(report, func() {
			var p *int
			_ = *p
		})
	}()
}
```

Run it:

```bash
go run ./cmd/demo
```

Expected output:

```
reported: quota exceeded
app panic returned: recovered application panic: quota exceeded
reported: runtime error: invalid memory address or nil pointer dereference
runtime bug re-panicked to caller: runtime error: invalid memory address or nil pointer dereference
```

### Tests

`TestAppPanicConvertedNoRepanic` runs `Guard` around an application error inside a
wrapper with its own outer recover, and asserts the outer recover did NOT fire
(`Guard` returned instead of re-panicking) and the returned error `errors.Is` the
sentinel. `TestRuntimeErrorRepanics` runs `Guard` around a nil deref and asserts
`Guard` re-panicked (the outer recover fired), the re-panicked value is the same
`runtime.Error`, and `onPanic` was invoked exactly once with a non-empty stack.

Create `guard_test.go`:

```go
package guard

import (
	"errors"
	"runtime"
	"testing"
)

func TestAppPanicConvertedNoRepanic(t *testing.T) {
	t.Parallel()

	sentinel := errors.New("app failure")
	reports := 0
	var outerRecovered any
	var returned error

	func() {
		defer func() { outerRecovered = recover() }()
		returned = Guard(func(Report) { reports++ }, func() {
			panic(sentinel)
		})
	}()

	if outerRecovered != nil {
		t.Fatalf("Guard re-panicked an application error: %v", outerRecovered)
	}
	if !errors.Is(returned, sentinel) {
		t.Fatalf("returned = %v, want it to wrap the sentinel", returned)
	}
	if reports != 1 {
		t.Fatalf("onPanic called %d times, want 1", reports)
	}
}

func TestRuntimeErrorRepanics(t *testing.T) {
	t.Parallel()

	var reports []Report
	var outerRecovered any

	func() {
		defer func() { outerRecovered = recover() }()
		_ = Guard(func(r Report) { reports = append(reports, r) }, func() {
			var p *int
			_ = *p // nil dereference: a runtime.Error
		})
		t.Error("Guard should have re-panicked, not returned")
	}()

	if outerRecovered == nil {
		t.Fatal("expected Guard to re-panic the runtime error")
	}
	outerErr, ok := outerRecovered.(error)
	if !ok {
		t.Fatalf("re-panicked value %T, want an error", outerRecovered)
	}
	var re runtime.Error
	if !errors.As(outerErr, &re) {
		t.Fatalf("re-panicked value %v, want a runtime.Error", outerErr)
	}
	if len(reports) != 1 {
		t.Fatalf("onPanic called %d times, want exactly 1 before re-raise", len(reports))
	}
	if len(reports[0].Stack) == 0 {
		t.Fatal("expected a non-empty captured stack")
	}
	// The re-panicked value is the same one that was reported.
	repErr, ok := reports[0].Value.(error)
	if !ok || repErr.Error() != outerErr.Error() {
		t.Fatalf("reported value %v differs from re-panicked value %v", reports[0].Value, outerErr)
	}
}
```

## Review

`Guard` is correct when an application `panic(error)` becomes a returned error with
no re-panic, and a `runtime.Error` (or any non-error) re-panics the original value
after reporting exactly once. Two details are load-bearing. First, capture
`debug.Stack()` as the first action in the deferred recover and report before
branching, so the trace points at the panic site and is recorded on every path,
including the re-raise. Second, re-panic the *original* value, not a rewrapped one,
so the outer handler and crash-reporter see the identical throw. The trap this
closes is the blanket `recover()` that owns everything: it turns bugs into 200s and
silences your alerting. Recover to classify; own only what you can; re-raise the
rest.

## Resources

- [Go Language Specification: Handling panics](https://go.dev/ref/spec#Handling_panics) — recover and re-panic semantics.
- [runtime.Error](https://pkg.go.dev/runtime#Error) — the interface identifying a code bug you should re-raise.
- [runtime/debug.Stack](https://pkg.go.dev/runtime/debug#Stack) — capturing the trace once, at recovery time.

---

Back to [00-concepts.md](00-concepts.md) | Next: [09-panic-during-defer-cleanup.md](09-panic-during-defer-cleanup.md)
