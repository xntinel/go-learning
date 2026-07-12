# Exercise 7: The Goroutine-Scope Trap: Why Your Recover Didn't Catch That Crash

The single most common way a service "with panic recovery" still crashes in
production: a handler wrapped in recovery middleware spawns a fire-and-forget
goroutine — a cache warm, an audit write — and that goroutine panics. The
handler's deferred recover is on the *request* goroutine; the panic is on the
*child* goroutine, and recover has goroutine scope, so it catches nothing and the
whole process dies. This module ships the broken pattern (as an illustration you
must not run) and the fix: `GoSafe`, which installs the recover *inside* each
spawned goroutine.

This module is fully self-contained: its own module, demo, and tests.

## What you'll build

```text
gosafe/                    independent module: example.com/gosafe
  go.mod                   go 1.26
  gosafe.go                PanicReport (Error+Unwrap), GoSafe(wg, report, fn)
  cmd/
    demo/
      main.go              runnable demo: spawn workers, one panics, all reported
  gosafe_test.go           spawned panic caught, stack captured, wg completes
```

Files: `gosafe.go`, `cmd/demo/main.go`, `gosafe_test.go`.
Implement: `GoSafe(wg *sync.WaitGroup, report func(error), fn func())` that spawns `fn` with a deferred recover inside the new goroutine, reporting a `*PanicReport` (with captured stack, `Unwrap` to the original error) and completing the `WaitGroup`.
Test: a spawned goroutine that panics is caught and reported with a non-empty stack, `errors.Is` reaches the original error, and the `WaitGroup` returns; the happy path reports nothing.
Verify: `go test -count=1 -race ./...`

Set up the module:

```bash
mkdir -p go-solutions/03-control-flow/08-panic-and-recover/07-cross-goroutine-recover-trap/cmd/demo
cd go-solutions/03-control-flow/08-panic-and-recover/07-cross-goroutine-recover-trap
```

### The scope rule, made unambiguous

Recover only catches panics unwinding through *its own* goroutine. Here is the
broken code that this rule punishes — it is shown as an illustration, not built or
run, because running it crashes the process:

```go
// BROKEN: the handler's recover lives on the request goroutine. The panic below
// is on a different goroutine, so recover never sees it and the process crashes.
func brokenWarm(w http.ResponseWriter, r *http.Request) {
	defer func() {
		if rec := recover(); rec != nil {
			// This will NEVER fire for the goroutine started below.
			http.Error(w, "recovered", http.StatusInternalServerError)
		}
	}()
	go func() {
		panic("cache warm failed") // crashes the WHOLE process
	}()
	w.WriteHeader(http.StatusAccepted)
}
```

Even a recovery middleware wrapping this handler does not help: the middleware's
recover is also on the request goroutine. The panic on the spawned goroutine
reaches the top of *that* goroutine's stack, finds no recover, and the runtime
terminates the process. No 500, no log line from your middleware — a hard crash.

The fix is mechanical and non-negotiable: every goroutine you spawn installs its
own recover as the first thing it does. `GoSafe` encapsulates that. It takes a
`*sync.WaitGroup` (so callers can wait for completion), a `report func(error)`
callback (so the recovery is observable — the lesson's rule that a silent recover
is a defect), and the `fn` to run. Inside the new goroutine it defers `wg.Done()`
first and the recover second, so the recover runs before `Done`. On a panic it
captures `debug.Stack()` immediately and builds a `*PanicReport` that `Unwrap`s to
the original error, then hands it to `report`. The panic never leaves the
goroutine; the process survives; the operator gets a report with a stack.

Create `gosafe.go`:

```go
package gosafe

import (
	"fmt"
	"runtime/debug"
	"sync"
)

// PanicReport carries a recovered goroutine panic to the reporter. It Unwraps to
// the original error when the panic value was an error.
type PanicReport struct {
	Value any
	Stack []byte
}

func (p *PanicReport) Error() string {
	return fmt.Sprintf("goroutine panic: %v", p.Value)
}

func (p *PanicReport) Unwrap() error {
	if err, ok := p.Value.(error); ok {
		return err
	}
	return nil
}

// GoSafe runs fn in a new goroutine with a recover installed INSIDE that
// goroutine, so a panic in fn is reported instead of crashing the process. The
// WaitGroup completes whether fn returns normally or panics.
func GoSafe(wg *sync.WaitGroup, report func(error), fn func()) {
	wg.Add(1)
	go func() {
		defer wg.Done()
		defer func() {
			if rec := recover(); rec != nil {
				report(&PanicReport{Value: rec, Stack: debug.Stack()})
			}
		}()
		fn()
	}()
}
```

### The runnable demo

The demo spawns three background jobs through `GoSafe`; the second panics. It
collects every report and prints how many jobs panicked — the process finishes
normally, which is the proof the child panic was contained.

Create `cmd/demo/main.go`:

```go
package main

import (
	"errors"
	"fmt"
	"sync"

	"example.com/gosafe"
)

func main() {
	var wg sync.WaitGroup
	var mu sync.Mutex
	var reports []error

	report := func(err error) {
		mu.Lock()
		reports = append(reports, err)
		mu.Unlock()
	}

	for i := 1; i <= 3; i++ {
		gosafe.GoSafe(&wg, report, func() {
			if i == 2 {
				panic(errors.New("audit write failed"))
			}
		})
	}

	wg.Wait()

	fmt.Printf("jobs that panicked: %d\n", len(reports))
	for _, r := range reports {
		fmt.Printf("report: %v\n", r)
	}
	fmt.Println("process finished normally")
}
```

Run it:

```bash
go run ./cmd/demo
```

Expected output:

```
jobs that panicked: 1
report: goroutine panic: audit write failed
process finished normally
```

### Tests

`TestGoSafeCatchesSpawnedPanic` spawns a goroutine that panics with a sentinel
error, waits, and asserts the report arrived, that `errors.Is` reaches the
sentinel through `*PanicReport`, and that the captured stack is non-empty.
`TestGoSafeHappyPath` asserts a non-panicking `fn` produces no report and the
`WaitGroup` still completes. The prose documents that a recover in the *parent*
goroutine cannot observe a child panic; the tests exercise only the guarded path,
never a real crash.

Create `gosafe_test.go`:

```go
package gosafe

import (
	"errors"
	"sync"
	"testing"
)

func TestGoSafeCatchesSpawnedPanic(t *testing.T) {
	t.Parallel()

	sentinel := errors.New("audit write failed")
	reports := make(chan error, 1)
	var wg sync.WaitGroup

	GoSafe(&wg, func(err error) { reports <- err }, func() {
		panic(sentinel)
	})

	wg.Wait() // completes even though fn panicked

	select {
	case err := <-reports:
		if !errors.Is(err, sentinel) {
			t.Fatalf("errors.Is(%v, sentinel) = false, want true", err)
		}
		var pr *PanicReport
		if !errors.As(err, &pr) {
			t.Fatalf("report %T is not a *PanicReport", err)
		}
		if len(pr.Stack) == 0 {
			t.Fatal("expected a non-empty captured stack")
		}
	default:
		t.Fatal("no panic report was delivered")
	}
}

func TestGoSafeHappyPath(t *testing.T) {
	t.Parallel()

	reports := make(chan error, 1)
	var wg sync.WaitGroup

	GoSafe(&wg, func(err error) { reports <- err }, func() {
		// returns normally
	})

	wg.Wait()

	select {
	case err := <-reports:
		t.Fatalf("unexpected report on the happy path: %v", err)
	default:
	}
}
```

## Review

`GoSafe` is correct when a panic in the spawned `fn` is always reported and never
crashes the process, the `WaitGroup` completes on both the panic and the happy
path, and the report carries the original error (via `Unwrap`) plus a captured
stack. The rule to internalize is the one the broken example demonstrates: a
parent goroutine's recover cannot catch a child goroutine's panic, so every
spawned goroutine must recover itself. The trap this closes is invisible until it
fires in production — the code "has recovery," but the recovery is on the wrong
goroutine. Run the tests with `-race`; the reporter callback and any shared state
it touches must be concurrency-safe, which is why the demo guards its slice with a
mutex and the tests use a buffered channel.

## Resources

- [Go Blog: Defer, Panic, and Recover](https://go.dev/blog/defer-panic-and-recover) — recover's per-goroutine scope.
- [Go Language Specification: Go statements](https://go.dev/ref/spec#Go_statements) — a spawned goroutine runs independently of its launcher.
- [runtime/debug.Stack](https://pkg.go.dev/runtime/debug#Stack) — capturing the failing goroutine's stack.

---

Back to [00-concepts.md](00-concepts.md) | Next: [08-repanic-preserve-crash.md](08-repanic-preserve-crash.md)
