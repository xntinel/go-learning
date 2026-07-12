# Exercise 26: Compensating Transaction Unwinding Stack on Failure

**Nivel: Avanzado** — validacion normal (tabla de casos, incluye concurrencia).

A distributed transaction that touches payment, inventory, and shipping
cannot rely on a database transaction to roll everything back — each leg is
a call to a different service with its own commit. The Saga pattern instead
runs a sequence of steps where every success pushes a *compensating* action
(refund the payment, release the reserved unit) onto a private stack; if a
later step fails, the already-committed steps are undone by popping and
running that stack in LIFO order — last committed, first undone. The stack
itself is a pair of closures over a mutex-guarded slice, safe to push onto
from concurrent goroutines even though this exercise's `Run` drives it
sequentially.

## What you'll build

```text
compensating-saga/          independent module: example.com/compensating-saga
  go.mod                     go 1.24
  comptx.go                  NewStack (push/unwindAll closures), Step, Run
  cmd/
    demo/
      main.go                 payment+inventory succeed, shipping fails, unwinds
  comptx_test.go              table test: success, LIFO unwind, compensation errors, concurrency
```

- Files: `comptx.go`, `cmd/demo/main.go`, `comptx_test.go`.
- Implement: `NewStack() (push func(func() error), unwindAll func() []error)` closing over a mutex-guarded `[]func() error`; `type Step func() (compensate func() error, err error)`; `Run(steps ...Step) (err error, compensationErrs []error)`.
- Test: all steps succeeding runs no compensation; a mid-sequence failure unwinds already-succeeded steps in reverse order; a failing compensation is collected instead of panicking or stopping the unwind; a concurrency test pushes onto one stack from many goroutines and unwinds exactly once under `-race`.
- Verify: `go test -count=1 -race ./...`

Set up the module:

```bash
go mod edit -go=1.24
```

### Why a stack of closures, not a slice of undo structs

`NewStack` returns two closures sharing one captured `stack []func() error`
and one `sync.Mutex`: `push` appends a compensating closure, `unwindAll` runs
every entry from the end backwards and clears the slice. Each compensating
action is itself a closure — `func() error { return refundAPI.Refund(id) }`
— so it carries whatever context (a payment ID, a reservation token) its
step captured, without a separate undo-struct type per step kind. `Run`
drives the whole saga: for each `Step`, on success it pushes the returned
compensation; on the first failure it calls `unwindAll` and returns both the
original error and any errors the compensations themselves produced,
because a refund that itself fails is operationally important and must not
be silently swallowed.

The mutex exists because `push` is meant to be safe even when steps run
concurrently (a saga is free to fan a batch of independent reservations out
to goroutines before checking results) — the stack must never corrupt under
concurrent pushes, and `unwindAll`'s read-modify-clear must be atomic with
respect to any push racing to land one more entry.

Create `comptx.go`:

```go
package comptx

import "sync"

// NewStack returns a push closure and an unwindAll closure sharing a private
// LIFO stack of compensating actions, guarded by a mutex so steps that push
// compensations from concurrent goroutines are safe. unwindAll runs every
// pushed compensation in reverse (last pushed, first run) order and clears
// the stack.
func NewStack() (push func(compensate func() error), unwindAll func() []error) {
	var mu sync.Mutex
	var stack []func() error

	push = func(compensate func() error) {
		mu.Lock()
		defer mu.Unlock()
		stack = append(stack, compensate)
	}

	unwindAll = func() []error {
		mu.Lock()
		defer mu.Unlock()
		var errs []error
		for i := len(stack) - 1; i >= 0; i-- {
			if err := stack[i](); err != nil {
				errs = append(errs, err)
			}
		}
		stack = nil
		return errs
	}

	return push, unwindAll
}

// Step performs one leg of a distributed transaction (payment, inventory,
// shipping...). On success it returns a compensate func that undoes the
// step; on failure it returns a non-nil err and compensate is ignored.
type Step func() (compensate func() error, err error)

// Run executes steps in order, pushing each success's compensation onto a
// private stack. On the first failure it unwinds every already-succeeded
// step's compensation in LIFO order (last committed, first undone) and
// returns the original step error alongside any errors the compensations
// themselves produced. On full success nothing is unwound.
func Run(steps ...Step) (err error, compensationErrs []error) {
	push, unwindAll := NewStack()
	for _, step := range steps {
		compensate, stepErr := step()
		if stepErr != nil {
			return stepErr, unwindAll()
		}
		if compensate != nil {
			push(compensate)
		}
	}
	return nil, nil
}
```

### The runnable demo

Payment and inventory succeed and push their compensations; shipping fails,
so `Run` unwinds inventory's release then payment's refund, in that order.

Create `cmd/demo/main.go`:

```go
package main

import (
	"errors"
	"fmt"

	"example.com/compensating-saga"
)

func main() {
	steps := []comptx.Step{
		func() (func() error, error) {
			fmt.Println("payment: charged $50")
			return func() error { fmt.Println("compensate: refunded $50"); return nil }, nil
		},
		func() (func() error, error) {
			fmt.Println("inventory: reserved 1 unit")
			return func() error { fmt.Println("compensate: released 1 unit"); return nil }, nil
		},
		func() (func() error, error) {
			fmt.Println("shipping: label request failed")
			return nil, errors.New("carrier API unavailable")
		},
	}

	err, compErrs := comptx.Run(steps...)
	fmt.Printf("transaction error: %v\n", err)
	fmt.Printf("compensation errors: %v\n", compErrs)
}
```

Run it:

```bash
go run ./cmd/demo
```

Expected output:

```
payment: charged $50
inventory: reserved 1 unit
shipping: label request failed
compensate: released 1 unit
compensate: refunded $50
transaction error: carrier API unavailable
compensation errors: []
```

### Tests

Create `comptx_test.go`:

```go
package comptx

import (
	"errors"
	"fmt"
	"sync"
	"sync/atomic"
	"testing"
)

func TestRunAllStepsSucceedNoUnwind(t *testing.T) {
	var trace []string
	steps := []Step{
		func() (func() error, error) {
			trace = append(trace, "payment")
			return func() error { trace = append(trace, "refund"); return nil }, nil
		},
		func() (func() error, error) {
			trace = append(trace, "inventory")
			return func() error { trace = append(trace, "restock"); return nil }, nil
		},
	}

	err, compErrs := Run(steps...)
	if err != nil {
		t.Fatalf("Run() error = %v, want nil", err)
	}
	if compErrs != nil {
		t.Fatalf("compensationErrs = %v, want nil (nothing to unwind)", compErrs)
	}
	want := "[payment inventory]"
	if got := fmt.Sprint(trace); got != want {
		t.Fatalf("trace = %s, want %s (no compensation ran)", got, want)
	}
}

func TestRunFailureUnwindsInLIFOOrder(t *testing.T) {
	var trace []string
	failShipping := errors.New("shipping unavailable")
	steps := []Step{
		func() (func() error, error) {
			trace = append(trace, "payment")
			return func() error { trace = append(trace, "refund"); return nil }, nil
		},
		func() (func() error, error) {
			trace = append(trace, "inventory")
			return func() error { trace = append(trace, "restock"); return nil }, nil
		},
		func() (func() error, error) {
			trace = append(trace, "shipping")
			return nil, failShipping
		},
	}

	err, compErrs := Run(steps...)
	if !errors.Is(err, failShipping) {
		t.Fatalf("err = %v, want %v", err, failShipping)
	}
	if len(compErrs) != 0 {
		t.Fatalf("compensationErrs = %v, want empty (compensations succeeded)", compErrs)
	}
	want := "[payment inventory shipping restock refund]"
	if got := fmt.Sprint(trace); got != want {
		t.Fatalf("trace = %s, want %s (LIFO unwind after failure)", got, want)
	}
}

func TestRunCollectsCompensationErrors(t *testing.T) {
	stepErr := errors.New("boom")
	compErr := errors.New("refund failed")
	steps := []Step{
		func() (func() error, error) {
			return func() error { return compErr }, nil
		},
		func() (func() error, error) { return nil, stepErr },
	}

	err, compErrs := Run(steps...)
	if !errors.Is(err, stepErr) {
		t.Fatalf("err = %v, want %v", err, stepErr)
	}
	if len(compErrs) != 1 || !errors.Is(compErrs[0], compErr) {
		t.Fatalf("compensationErrs = %v, want [%v]", compErrs, compErr)
	}
}

func TestStackConcurrentPushThenUnwind(t *testing.T) {
	push, unwindAll := NewStack()
	const n = 100
	var executed atomic.Int64
	var wg sync.WaitGroup
	for range n {
		wg.Add(1)
		go func() {
			defer wg.Done()
			push(func() error { executed.Add(1); return nil })
		}()
	}
	wg.Wait()

	errs := unwindAll()
	if len(errs) != 0 {
		t.Fatalf("errs = %v, want empty", errs)
	}
	if got := executed.Load(); got != n {
		t.Fatalf("executed = %d, want %d", got, n)
	}
}
```

Verify: `go test -count=1 -race ./...`

## Review

The first two tests prove the core contract: full success leaves nothing to
undo, and a mid-sequence failure unwinds strictly in reverse commit order —
shipping's failure is followed by inventory's release, then payment's
refund, never the other way around. The third test proves a failing
compensation is reported, not swallowed or panicked on. The concurrency test
is the one that would fail — under `-race` or by a corrupted/short stack —
if `push` and `unwindAll` did not share one lock around the full
append/read-modify-clear: a hundred goroutines racing to push must all land
safely, and the subsequent unwind must run every single one exactly once.

## Resources

- [pkg.go.dev: sync.Mutex](https://pkg.go.dev/sync#Mutex) — guards the shared compensation stack's push and unwind.
- [AWS Prescriptive Guidance: Saga pattern](https://docs.aws.amazon.com/prescriptive-guidance/latest/cloud-design-patterns/saga.html) — the compensating-transaction pattern this exercise implements.
- [pkg.go.dev: errors.Is](https://pkg.go.dev/errors#Is) — how the tests identify the original step error versus compensation errors.

---

Back to [00-concepts.md](00-concepts.md) | Prev: [25-cardinality-limiter-unique-labels.md](25-cardinality-limiter-unique-labels.md) | Next: [27-encryption-key-versioning-wrapper.md](27-encryption-key-versioning-wrapper.md)
