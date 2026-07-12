# Exercise 3: Recover Panics Locally So One Bad Job Cannot Crash the Process

A single unrecovered panic in any goroutine takes down the entire process — every
in-flight request, every other worker, gone. And the `recover` you put in the
parent does nothing for a panic in a child. This module extracts the panic
firewall into a reusable `SafeRun`/`SafeGo` helper that converts a panicking job
into an error carrying the job name and a captured stack, and it pins the
easy-to-get-wrong contract that recovery must be *inside* the goroutine that
panics.

This module is fully self-contained: its own `go mod init`, all code inline, its
own demo and tests. Nothing here imports another exercise.

## What you'll build

```text
saferun/                     independent module: example.com/saferun
  go.mod                     go 1.26
  saferun.go                 PanicError; SafeRun (recover to error+stack), SafeGo (fan out safely)
  cmd/
    demo/
      main.go                runnable demo: a panicking job and a normal job both handled
  saferun_test.go            tests: panic -> named error, sibling still completes, stack captured
```

Files: `saferun.go`, `cmd/demo/main.go`, `saferun_test.go`.
Implement: `SafeRun(name, fn)` that runs `fn`, and on panic returns a `*PanicError` carrying the job name, the recovered value, and `debug.Stack()`; `SafeGo` that fans a set of named funcs out, recovering each locally.
Test: a panicking job yields a non-nil error whose message contains the job name; a normal sibling still completes; `errors.As` extracts the `*PanicError`; the captured stack is non-empty.
Verify: `go test -count=1 -race ./...`

Set up the module:

```bash
mkdir -p go-solutions/10-error-handling/09-error-handling-in-goroutines/03-per-goroutine-panic-recovery/cmd/demo
cd go-solutions/10-error-handling/09-error-handling-in-goroutines/03-per-goroutine-panic-recovery
go mod edit -go=1.26
```

### Why recovery must be local, and what a good recovery captures

Go's `recover` only sees a panic that is unwinding *its own goroutine's* stack. A
`defer func(){ recover() }()` in the parent goroutine is invisible to a panic in a
`go child()` — the child's panic unwinds the child's stack, finds no recover
there, reaches the top of the goroutine, and aborts the whole program. This is the
single most surprising thing about panics for engineers coming from
exception-based languages, where a `try` in the caller catches a throw in a callee
across thread boundaries. In Go it does not. So every goroutine that runs
arbitrary or fallible work needs its *own* deferred recover.

`SafeRun` is that recover, packaged. It runs `fn` inside a deferred recover and, on
panic, builds a `*PanicError` capturing three things: the job name (so on-call
knows *which* job blew up), the recovered value (the panic payload), and
`debug.Stack()` (the goroutine's stack at the moment of panic, so the failure is
diagnosable without a reproduction). Capturing the stack *at recover time* is the
point — once `recover` returns, the panicking stack is unwound and gone;
`runtime/debug.Stack()` called inside the deferred function is the last chance to
snapshot it. `PanicError` implements `Error()` and, because it is a concrete type,
`errors.As` can pull it back out of a joined aggregate for classification (a panic
is a different failure class from a returned error, and you may want to alert on it
differently).

Note the named return value `err`: the deferred closure assigns to `err`, and that
assignment is only visible to the caller because `err` is a *named* result. Drop
the name and the recovered error has nowhere to go. `SafeGo` then layers the
concurrent fan-out on top — each goroutine calls `SafeRun`, so one job panicking is
converted to an error and the siblings run to completion, exactly as a resilient
worker pool must.

Create `saferun.go`:

```go
package saferun

import (
	"context"
	"fmt"
	"runtime/debug"
	"sync"
)

// PanicError wraps a recovered panic with the job name and the stack captured at
// recover time, so a panic is a diagnosable, classifiable error rather than a
// process crash.
type PanicError struct {
	Job   string
	Value any
	Stack []byte
}

func (e *PanicError) Error() string {
	return fmt.Sprintf("job %q panicked: %v", e.Job, e.Value)
}

// SafeRun runs fn and converts a panic into a *PanicError. The recover lives
// inside this function, so it catches only a panic on this goroutine's stack.
func SafeRun(name string, fn func() error) (err error) {
	defer func() {
		if rec := recover(); rec != nil {
			err = &PanicError{Job: name, Value: rec, Stack: debug.Stack()}
		}
	}()
	return fn()
}

// Named pairs a name with its work, for SafeGo.
type Named struct {
	Name string
	Run  func(ctx context.Context) error
}

// SafeGo runs every job concurrently, recovering each panic locally so one bad
// job cannot crash the process or abort its siblings. It returns one error per
// failed job (panics included), keyed by name.
func SafeGo(ctx context.Context, jobs []Named) map[string]error {
	var (
		mu   sync.Mutex
		errs = make(map[string]error, len(jobs))
		wg   sync.WaitGroup
	)
	for _, j := range jobs {
		wg.Go(func() {
			err := SafeRun(j.Name, func() error { return j.Run(ctx) })
			if err != nil {
				mu.Lock()
				errs[j.Name] = err
				mu.Unlock()
			}
		})
	}
	wg.Wait()
	return errs
}
```

### The runnable demo

The demo runs three jobs concurrently: one succeeds, one returns a normal error,
one panics. All three are handled — the process does not crash — and the panic is
reported as a `*PanicError` naming the job. Because the stack is long and
environment-specific, the demo prints only its first line to keep output stable.

Create `cmd/demo/main.go`:

```go
package main

import (
	"context"
	"errors"
	"fmt"
	"sort"

	"example.com/saferun"
)

func main() {
	jobs := []saferun.Named{
		{Name: "ok", Run: func(ctx context.Context) error { return nil }},
		{Name: "returns-error", Run: func(ctx context.Context) error {
			return errors.New("disk full")
		}},
		{Name: "panics", Run: func(ctx context.Context) error {
			var m map[string]int
			m["x"] = 1 // nil map write: panic
			return nil
		}},
	}

	errs := saferun.SafeGo(context.Background(), jobs)

	names := make([]string, 0, len(errs))
	for name := range errs {
		names = append(names, name)
	}
	sort.Strings(names)

	for _, name := range names {
		err := errs[name]
		var pe *saferun.PanicError
		if errors.As(err, &pe) {
			fmt.Printf("%s: PANIC (%v); stack captured=%t\n", name, pe.Value, len(pe.Stack) > 0)
		} else {
			fmt.Printf("%s: error: %v\n", name, err)
		}
	}
	fmt.Println("process survived")
}
```

Run it:

```bash
go run ./cmd/demo
```

The `ok` job produced no error, so only the two failures print, sorted by name.
Expected output:

```
panics: PANIC (assignment to entry in nil map); stack captured=true
returns-error: error: disk full
process survived
```

### Tests

`TestSafeRunConvertsPanic` proves a panicking job yields a `*PanicError` (via
`errors.As`) whose message contains the job name and whose captured stack is
non-empty. `TestSafeGoSiblingSurvives` proves the resilience contract: with one
panicking job and one normal job, the normal job still completes and only the
panicking one shows up in the error map. `TestParentRecoverDoesNotCatchChild`
documents the core rule the whole exercise exists for — a `recover` in the parent
goroutine does *not* stop a child's panic, which is exactly why `SafeRun` puts the
recover inside the worker. That test drives the point positively: it shows that
routing the child through `SafeRun` (local recover) is what makes the panic
recoverable, and explains in a comment why a parent-level recover would not.

Create `saferun_test.go`:

```go
package saferun

import (
	"context"
	"errors"
	"strings"
	"testing"
)

func TestSafeRunConvertsPanic(t *testing.T) {
	t.Parallel()
	err := SafeRun("indexer", func() error { panic("boom") })

	var pe *PanicError
	if !errors.As(err, &pe) {
		t.Fatalf("SafeRun() = %v, want a *PanicError", err)
	}
	if !strings.Contains(pe.Error(), "indexer") {
		t.Fatalf("PanicError message = %q, want it to name the job", pe.Error())
	}
	if len(pe.Stack) == 0 {
		t.Fatal("PanicError.Stack is empty; the stack was not captured at recover time")
	}
}

func TestSafeRunPassesThroughNormalError(t *testing.T) {
	t.Parallel()
	sentinel := errors.New("normal")
	err := SafeRun("job", func() error { return sentinel })
	if !errors.Is(err, sentinel) {
		t.Fatalf("SafeRun() = %v, want the returned error unchanged", err)
	}
}

func TestSafeGoSiblingSurvives(t *testing.T) {
	t.Parallel()
	completed := make(chan struct{}, 1)
	jobs := []Named{
		{Name: "panics", Run: func(ctx context.Context) error { panic("kaboom") }},
		{Name: "normal", Run: func(ctx context.Context) error {
			completed <- struct{}{}
			return nil
		}},
	}

	errs := SafeGo(context.Background(), jobs)

	select {
	case <-completed:
	default:
		t.Fatal("the normal sibling did not complete; a panic aborted the group")
	}
	if len(errs) != 1 {
		t.Fatalf("len(errs) = %d, want 1 (only the panicking job)", len(errs))
	}
	if _, ok := errs["panics"]; !ok {
		t.Fatal("expected an error recorded for the panicking job")
	}
	if _, ok := errs["normal"]; ok {
		t.Fatal("the normal job should not have produced an error")
	}
}

// TestParentRecoverDoesNotCatchChild pins the contract that a recover is local to
// its own goroutine. Routing the child's work through SafeRun (whose recover runs
// on the child's stack) is what makes the panic recoverable. A recover placed in
// this parent goroutine instead would NOT catch it: the child's panic would
// unwind the child's stack, find no recover there, and abort the whole process,
// so the assertion below can only pass because SafeRun recovered locally.
func TestParentRecoverDoesNotCatchChild(t *testing.T) {
	t.Parallel()
	got := make(chan error, 1)
	go func() {
		// This recover is on the CHILD goroutine's stack (via SafeRun), which is
		// why it works. A defer recover() written here directly, wrapping the go
		// statement in the parent, would be useless.
		got <- SafeRun("child", func() error { panic("child panic") })
	}()

	err := <-got
	var pe *PanicError
	if !errors.As(err, &pe) {
		t.Fatalf("child panic = %v, want a recovered *PanicError", err)
	}
}
```

## Review

The helper is correct when a panic never escapes as a crash and always arrives as
a `*PanicError` naming its job with a captured stack — the `errors.As` and
non-empty-stack assertions pin both. The single most important design fact is
placement: the `recover` is inside `SafeRun`, which runs on the panicking
goroutine's stack, because a recover anywhere else cannot see the panic. The demo
and the sibling-survives test together prove the resilience payoff: one job's
nil-map write becomes one recorded error while every other job finishes and the
process prints `process survived`. The stack must be captured with
`runtime/debug.Stack()` *inside* the deferred recover — after `recover` returns,
the panicking frames are unwound and unrecoverable. Run `go test -race` and
`go vet ./...` to confirm.

## Resources

- [Go Blog: Defer, Panic, and Recover](https://go.dev/blog/defer-panic-and-recover) — the recover-to-error pattern and why recover is goroutine-local.
- [`runtime/debug.Stack`](https://pkg.go.dev/runtime/debug#Stack) — snapshotting the goroutine stack at recover time.
- [`errors.As`](https://pkg.go.dev/errors#As) — extracting the concrete `*PanicError` for classification.

---

Back to [00-concepts.md](00-concepts.md) | Next: [04-errgroup-fail-fast-fanout.md](04-errgroup-fail-fast-fanout.md)
