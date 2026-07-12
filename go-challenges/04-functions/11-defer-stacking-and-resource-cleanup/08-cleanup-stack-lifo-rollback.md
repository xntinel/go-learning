# Exercise 8: Multi-Step Setup With a LIFO Cleanup Stack (Saga-Style Rollback)

Some setups are too dynamic for a fixed list of `defer` statements: open a file,
then acquire a lease, then begin a transaction, and if any later step fails, undo
the earlier ones — newest first — but on full success keep everything. This module
builds a small cleanup stack that makes the LIFO principle explicit and reusable:
push a rollback closure after each successful step, run them in reverse on
failure, and disarm on success.

This module is fully self-contained: its own `go mod init`, its own demo, its own
tests. Nothing here imports any other exercise.

## What you'll build

```text
rollback/                   independent module: example.com/rollback
  go.mod
  rollback/rollback.go       Cleanup stack (Push/Run/Disarm); Provision orchestrator
  cmd/demo/main.go           three steps; third fails; watch reverse-order rollback
  rollback/rollback_test.go  mid-failure rolls back in reverse; success keeps all; rollback error; panic
```

- Files: `rollback/rollback.go`, `cmd/demo/main.go`, `rollback/rollback_test.go`.
- Implement: a `Cleanup` stack with `Push(func() error)`, `Run()` (reverse order, joining errors), and `Disarm()`; and `Provision(steps)` that runs steps in order, pushes each step's rollback, and on error or panic runs the stack in reverse — disarming on full success.
- Test: step 3 of 4 fails and steps 2 then 1 roll back in reverse while step 4 never runs; all succeed and nothing rolls back; a rollback that itself errors is surfaced via `errors.Join` while the remaining rollbacks still run; a panic mid-setup triggers full rollback then re-panic.
- Verify: `go test -count=1 -race ./...`

Set up the module:

```bash
mkdir -p go-solutions/04-functions/11-defer-stacking-and-resource-cleanup/08-cleanup-stack-lifo-rollback/rollback go-solutions/04-functions/11-defer-stacking-and-resource-cleanup/08-cleanup-stack-lifo-rollback/cmd/demo
cd go-solutions/04-functions/11-defer-stacking-and-resource-cleanup/08-cleanup-stack-lifo-rollback
```

### Why not just use defers

A chain of `defer` statements is the right tool when the set of things to undo is
known at compile time and you *always* want to undo them. Neither holds for a
saga-style setup. You do not know how far you will get — if step three fails you
must undo steps two and one but not a step-four rollback that was never armed —
and on success you must *not* undo anything, because the whole point is to return
live resources to the caller. A fixed `defer step2.rollback()` would also fire on
the success path and tear down what you meant to keep.

The cleanup stack is `defer` semantics you drive by hand. It is a slice of
rollback closures with three operations:

- `Push(fn)` appends a rollback after a step succeeds, so the stack always
  reflects exactly the steps completed so far.
- `Run()` executes the closures in reverse index order — LIFO, the same order a
  stack of defers would fire — so the most recently acquired resource is released
  first. It keeps going even if one rollback errors, joining all errors, because
  abandoning cleanup halfway is how you leak the rest.
- `Disarm()` empties the stack, so a subsequent `Run()` is a no-op. This is what
  you call (or, equivalently, the flag you set) on full success to keep the
  resources.

`Provision` ties it together with a deferred closure keyed on a `success` flag and
`recover()`. On the normal error path the flag is false, so the deferred closure
runs the stack. On a panic the closure runs the stack and re-raises. On success
the flag is true and the closure runs nothing — the resources survive. This is the
same named-return/deferred-closure discipline from the transaction helper, applied
to a dynamic list.

Create `rollback/rollback.go`:

```go
package rollback

import (
	"errors"
	"fmt"
)

// Cleanup is a LIFO stack of rollback closures. Push one after each step that
// succeeds; Run undoes them newest-first; Disarm keeps the resources on success.
type Cleanup struct {
	fns []func() error
}

// Push adds a rollback to the top of the stack.
func (c *Cleanup) Push(fn func() error) {
	if fn != nil {
		c.fns = append(c.fns, fn)
	}
}

// Run executes every rollback in reverse order, joining any errors. It runs all
// of them even if some fail, so no resource is abandoned.
func (c *Cleanup) Run() error {
	var errs []error
	for i := len(c.fns) - 1; i >= 0; i-- {
		if err := c.fns[i](); err != nil {
			errs = append(errs, err)
		}
	}
	return errors.Join(errs...)
}

// Disarm empties the stack so a later Run does nothing.
func (c *Cleanup) Disarm() { c.fns = nil }

// Step is one setup action. Setup does its work and returns a rollback closure
// (nil if there is nothing to undo) plus an error.
type Step struct {
	Name  string
	Setup func() (rollback func() error, err error)
}

// Provision runs steps in order. If any step fails or panics, it rolls back the
// completed steps in reverse (LIFO) and returns the error (or re-panics). On full
// success the rollbacks are disarmed so the resources survive.
func Provision(steps []Step) (err error) {
	var cu Cleanup
	success := false

	defer func() {
		if p := recover(); p != nil {
			_ = cu.Run()
			panic(p)
		}
		if !success {
			if rbErr := cu.Run(); rbErr != nil {
				err = errors.Join(err, rbErr)
			}
		}
	}()

	for _, s := range steps {
		rb, serr := s.Setup()
		if serr != nil {
			return fmt.Errorf("step %s: %w", s.Name, serr)
		}
		cu.Push(rb)
	}

	success = true
	return nil
}
```

### The runnable demo

Create `cmd/demo/main.go`:

```go
package main

import (
	"fmt"

	"example.com/rollback/rollback"
)

func main() {
	var trace []string

	mk := func(name string, fail bool) rollback.Step {
		return rollback.Step{
			Name: name,
			Setup: func() (func() error, error) {
				if fail {
					trace = append(trace, "setup-"+name+"-FAIL")
					return nil, fmt.Errorf("cannot %s", name)
				}
				trace = append(trace, "setup-"+name)
				return func() error {
					trace = append(trace, "rollback-"+name)
					return nil
				}, nil
			},
		}
	}

	err := rollback.Provision([]rollback.Step{
		mk("open-file", false),
		mk("acquire-lease", false),
		mk("begin-tx", true),
	})

	fmt.Println("err:", err)
	for _, e := range trace {
		fmt.Println(e)
	}
}
```

Run it:

```bash
go run ./cmd/demo
```

Expected output:

```
err: step begin-tx: cannot begin-tx
setup-open-file
setup-acquire-lease
setup-begin-tx-FAIL
rollback-acquire-lease
rollback-open-file
```

### Tests

The tests use fake steps that append to a shared `trace` slice (the calls are
sequential, so no lock is needed). They assert the reverse rollback order, the
zero-rollback success path, error joining when a rollback itself fails, and the
panic-then-rollback-then-re-panic path.

Create `rollback/rollback_test.go`:

```go
package rollback

import (
	"errors"
	"fmt"
	"slices"
	"testing"
)

// recorder builds steps that log setup and rollback into a shared trace.
type recorder struct{ trace []string }

func (r *recorder) ok(name string) Step {
	return Step{
		Name: name,
		Setup: func() (func() error, error) {
			r.trace = append(r.trace, "setup-"+name)
			return func() error {
				r.trace = append(r.trace, "rollback-"+name)
				return nil
			}, nil
		},
	}
}

func (r *recorder) fail(name string, err error) Step {
	return Step{
		Name: name,
		Setup: func() (func() error, error) {
			r.trace = append(r.trace, "setup-"+name+"-FAIL")
			return nil, err
		},
	}
}

func TestProvisionRollsBackInReverse(t *testing.T) {
	t.Parallel()

	r := &recorder{}
	stepErr := errors.New("db down")
	err := Provision([]Step{
		r.ok("one"),
		r.ok("two"),
		r.fail("three", stepErr),
		r.ok("four"), // never reached
	})

	if !errors.Is(err, stepErr) {
		t.Fatalf("err = %v, want errors.Is %v", err, stepErr)
	}
	want := []string{
		"setup-one", "setup-two", "setup-three-FAIL",
		"rollback-two", "rollback-one",
	}
	if !slices.Equal(r.trace, want) {
		t.Fatalf("trace = %v, want %v", r.trace, want)
	}
}

func TestProvisionSuccessKeepsResources(t *testing.T) {
	t.Parallel()

	r := &recorder{}
	err := Provision([]Step{r.ok("one"), r.ok("two")})
	if err != nil {
		t.Fatalf("err = %v, want nil", err)
	}
	want := []string{"setup-one", "setup-two"}
	if !slices.Equal(r.trace, want) {
		t.Fatalf("trace = %v, want %v (no rollbacks)", r.trace, want)
	}
}

func TestProvisionSurfacesRollbackError(t *testing.T) {
	t.Parallel()

	rbErr := errors.New("rollback of two failed")
	stepErr := errors.New("boom")
	var trace []string

	badRollback := Step{
		Name: "two",
		Setup: func() (func() error, error) {
			trace = append(trace, "setup-two")
			return func() error {
				trace = append(trace, "rollback-two-ERR")
				return rbErr
			}, nil
		},
	}
	okStep := Step{
		Name: "one",
		Setup: func() (func() error, error) {
			trace = append(trace, "setup-one")
			return func() error {
				trace = append(trace, "rollback-one")
				return nil
			}, nil
		},
	}
	failStep := Step{
		Name: "three",
		Setup: func() (func() error, error) {
			trace = append(trace, "setup-three-FAIL")
			return nil, stepErr
		},
	}

	err := Provision([]Step{okStep, badRollback, failStep})

	if !errors.Is(err, stepErr) {
		t.Errorf("err = %v, want errors.Is %v (step)", err, stepErr)
	}
	if !errors.Is(err, rbErr) {
		t.Errorf("err = %v, want errors.Is %v (rollback)", err, rbErr)
	}
	// The failing rollback did not stop the remaining one.
	want := []string{"setup-one", "setup-two", "setup-three-FAIL", "rollback-two-ERR", "rollback-one"}
	if !slices.Equal(trace, want) {
		t.Fatalf("trace = %v, want %v", trace, want)
	}
}

func TestProvisionPanicRollsBackThenReRaises(t *testing.T) {
	t.Parallel()

	var trace []string
	okStep := Step{
		Name: "one",
		Setup: func() (func() error, error) {
			trace = append(trace, "setup-one")
			return func() error {
				trace = append(trace, "rollback-one")
				return nil
			}, nil
		},
	}
	panicStep := Step{
		Name: "two",
		Setup: func() (func() error, error) {
			panic("kaboom")
		},
	}

	defer func() {
		r := recover()
		if r == nil {
			t.Fatal("expected panic to propagate")
		}
		want := []string{"setup-one", "rollback-one"}
		if !slices.Equal(trace, want) {
			t.Fatalf("trace = %v, want %v", trace, want)
		}
	}()

	_ = Provision([]Step{okStep, panicStep})
}

func TestCleanupDisarm(t *testing.T) {
	t.Parallel()

	ran := 0
	var cu Cleanup
	cu.Push(func() error { ran++; return nil })
	cu.Disarm()
	if err := cu.Run(); err != nil {
		t.Fatalf("Run = %v", err)
	}
	if ran != 0 {
		t.Fatalf("ran = %d, want 0 after Disarm", ran)
	}
}

func Example() {
	var trace []string
	step := func(name string) Step {
		return Step{Name: name, Setup: func() (func() error, error) {
			return func() error { trace = append(trace, "undo-"+name); return nil }, nil
		}}
	}
	_ = Provision([]Step{step("a"), {Name: "b", Setup: func() (func() error, error) {
		return nil, fmt.Errorf("b failed")
	}}})
	fmt.Println(trace)
	// Output: [undo-a]
}
```

## Review

The orchestrator is correct when a mid-sequence failure rolls back exactly the
completed steps in reverse order and no further, when full success rolls back
nothing, when a rollback that itself errors is surfaced (via `errors.Join`) yet
does not stop the remaining rollbacks, and when a panic mid-setup triggers the
full rollback and then re-raises. The mistake this pattern exists to prevent is
using a fixed sequence of `defer` statements that also fire on the success path,
tearing down the very resources you meant to hand back; the cleanup stack is armed
per completed step and disarmed on success, so rollback is strictly conditional on
failure. Run `go test -race`.

## Resources

- [errors.Join](https://pkg.go.dev/errors#Join)
- [The Go Programming Language Specification: Defer statements](https://go.dev/ref/spec#Defer_statements)
- [slices.Equal](https://pkg.go.dev/slices#Equal)
- [Saga pattern (compensating transactions)](https://learn.microsoft.com/en-us/azure/architecture/patterns/saga)

---

Back to [00-concepts.md](00-concepts.md) | Previous: [07-panic-recover-http-middleware.md](07-panic-recover-http-middleware.md) | Next: [09-graceful-shutdown-layered-defer.md](09-graceful-shutdown-layered-defer.md)
