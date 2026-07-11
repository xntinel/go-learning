# Exercise 6: Guarantee uniform error context on every exit path with named-return + defer

A transactional operation with several steps has many `return err` points, and
forgetting the wrap prefix on even one leaves a bare `EOF` in the logs with no
operation name. This exercise builds a `ProcessOrder` that uses the named-return +
defer idiom: a single deferred closure wraps the error with the operation name on
whichever path set it, and rolls the resource back — while the `if err != nil`
guard keeps the success path clean and does not fabricate a failure.

This module is fully self-contained: its own `go mod init`, all code inline, its
own demo and tests. Nothing here imports another exercise.

## What you'll build

```text
order/                         independent module: example.com/order
  go.mod                       go 1.24
  order.go                     step sentinels; Steps; Result; ProcessOrder with defer-wrap
  order_test.go                each step fails -> wrapped + Is finds sentinel; success -> nil, committed; nil-wrap guard
  cmd/
    demo/
      main.go                  a succeeding order and one that fails at charge
```

- Files: `order.go`, `cmd/demo/main.go`, `order_test.go`.
- Implement: `ProcessOrder(steps Steps) (res *Result, err error)` with one `defer` that, only `if err != nil`, rolls back and wraps `err` with `process order: %w`.
- Test: forcing failure at each step yields a message prefixed with the operation and `errors.Is` still finds the step sentinel; the success path returns `nil` with a committed result (proving the guard does not fabricate); `fmt.Errorf("op: %w", nil)` is non-nil (justifying the guard).
- Verify: `go test -count=1 -race ./...`

Set up the module:

```bash
mkdir -p ~/go-exercises/order/cmd/demo
cd ~/go-exercises/order
go mod init example.com/order
```

### One defer, every exit path

`ProcessOrder` names its return values `(res *Result, err error)` so the deferred
closure can see and rewrite `err` after any `return`. The steps are injected as a
`Steps` struct of `func() error` fields, so a test can force a failure at exactly
one step. The body is a sequence of early returns:

```go
if err = steps.ReserveStock(); err != nil {
	return res, err
}
```

Each returns a *bare* step error — no per-line prefix. The single `defer` supplies
the uniform context:

```go
defer func() {
	if err != nil {
		res.RolledBack = true
		err = fmt.Errorf("process order: %w", err)
	}
}()
```

Two things make this correct. First, `%w` preserves the step sentinel, so a caller
can still `errors.Is(err, ErrCharge)` through the `process order:` prefix. Second,
the `if err != nil` guard is load-bearing, not decoration: on the success path
`err` is `nil`, and `fmt.Errorf("process order: %w", nil)` would return a *non-nil*
error whose message contains `%!w(<nil>)`. Without the guard, every successful
order would come back as a fabricated failure. The final test in this module pins
exactly that fact about `fmt.Errorf` so the guard's necessity is documented in
code.

Create `order.go`:

```go
package order

import (
	"errors"
	"fmt"
)

// Step sentinels a caller can branch on through the operation-level wrap.
var (
	ErrReserveStock = errors.New("reserve stock failed")
	ErrCharge       = errors.New("charge failed")
	ErrCommit       = errors.New("commit failed")
)

// Steps injects the fallible operations so a test can force each failure.
type Steps struct {
	ReserveStock func() error
	Charge       func() error
	Commit       func() error
}

// Result reports what happened to the transaction-like resource.
type Result struct {
	Committed  bool
	RolledBack bool
}

// ProcessOrder runs the steps in order. A single deferred closure guarantees that
// every failing exit path is rolled back and wrapped with the operation name,
// while the success path returns a clean nil error.
func ProcessOrder(steps Steps) (res *Result, err error) {
	res = &Result{}
	defer func() {
		if err != nil {
			res.RolledBack = true
			err = fmt.Errorf("process order: %w", err)
		}
	}()

	if err = steps.ReserveStock(); err != nil {
		return res, err
	}
	if err = steps.Charge(); err != nil {
		return res, err
	}
	if err = steps.Commit(); err != nil {
		return res, err
	}

	res.Committed = true
	return res, nil
}
```

### The runnable demo

Create `cmd/demo/main.go`:

```go
package main

import (
	"errors"
	"fmt"

	"example.com/order"
)

func main() {
	ok := order.Steps{
		ReserveStock: func() error { return nil },
		Charge:       func() error { return nil },
		Commit:       func() error { return nil },
	}
	res, err := order.ProcessOrder(ok)
	fmt.Printf("success: committed=%v err=%v\n", res.Committed, err)

	failCharge := order.Steps{
		ReserveStock: func() error { return nil },
		Charge:       func() error { return order.ErrCharge },
		Commit:       func() error { return nil },
	}
	res, err = order.ProcessOrder(failCharge)
	fmt.Printf("charge fails: rolledBack=%v err=%v\n", res.RolledBack, err)
	fmt.Printf("  is ErrCharge=%v\n", errors.Is(err, order.ErrCharge))
}
```

Run it:

```bash
go run ./cmd/demo
```

Expected output:

```
success: committed=true err=<nil>
charge fails: rolledBack=true err=process order: charge failed
  is ErrCharge=true
```

### Tests

The table drives a failure at each step and asserts two things per case: the
message carries the `process order` prefix, and `errors.Is` still finds the step
sentinel through it. Separate tests pin the success path (nil error, committed, not
rolled back — the guard did not fabricate) and the raw `fmt.Errorf` nil-wrap
behavior that justifies the guard.

Create `order_test.go`:

```go
package order

import (
	"errors"
	"fmt"
	"strings"
	"testing"
)

func passing() Steps {
	return Steps{
		ReserveStock: func() error { return nil },
		Charge:       func() error { return nil },
		Commit:       func() error { return nil },
	}
}

func TestProcessOrderWrapsEachStep(t *testing.T) {
	t.Parallel()

	cases := []struct {
		name    string
		mutate  func(*Steps)
		wantErr error
	}{
		{"reserve", func(s *Steps) { s.ReserveStock = func() error { return ErrReserveStock } }, ErrReserveStock},
		{"charge", func(s *Steps) { s.Charge = func() error { return ErrCharge } }, ErrCharge},
		{"commit", func(s *Steps) { s.Commit = func() error { return ErrCommit } }, ErrCommit},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()

			steps := passing()
			tc.mutate(&steps)
			res, err := ProcessOrder(steps)

			if !errors.Is(err, tc.wantErr) {
				t.Fatalf("err = %v, want errors.Is %v through the wrap", err, tc.wantErr)
			}
			if !strings.HasPrefix(err.Error(), "process order: ") {
				t.Fatalf("err.Error() = %q, want the process order prefix", err.Error())
			}
			if !res.RolledBack {
				t.Error("resource should be rolled back on failure")
			}
			if res.Committed {
				t.Error("resource should not be committed on failure")
			}
		})
	}
}

func TestProcessOrderSuccess(t *testing.T) {
	t.Parallel()

	res, err := ProcessOrder(passing())
	if err != nil {
		t.Fatalf("err = %v, want nil (guard must not fabricate)", err)
	}
	if !res.Committed {
		t.Error("resource should be committed on success")
	}
	if res.RolledBack {
		t.Error("resource should not be rolled back on success")
	}
}

func TestWrapNilFabricatesNonNil(t *testing.T) {
	t.Parallel()

	// This is why the defer needs an `if err != nil` guard: wrapping nil yields
	// a non-nil error, not nil.
	err := fmt.Errorf("process order: %w", error(nil))
	if err == nil {
		t.Fatal("fmt.Errorf wrapping nil unexpectedly returned nil")
	}
	if !strings.Contains(err.Error(), "%!w(<nil>)") {
		t.Fatalf("err.Error() = %q, want the %%!w(<nil>) marker", err.Error())
	}
}
```

## Review

The operation is correct when every failing path returns an error prefixed with
`process order:` that still `errors.Is`-matches its step sentinel, and the success
path returns a genuinely `nil` error with a committed resource. The idiom's whole
value is that adding a fourth step later cannot introduce an unwrapped exit path —
the context lives in one place, not at every `return`. The guard is the part
juniors delete "because it looks redundant"; `TestWrapNilFabricatesNonNil` is the
concrete proof that removing it turns success into a `%!w(<nil>)` failure. Note
also that `res` is initialized before the defer is registered, so the closure can
safely set `RolledBack` even on the earliest failure.

## Resources

- [fmt.Errorf](https://pkg.go.dev/fmt#Errorf) — `%w` and the nil-wrap behavior.
- [Defer, Panic, and Recover](https://go.dev/blog/defer-panic-and-recover) — deferred closures and named returns.
- [errors package](https://pkg.go.dev/errors) — `Is` through the operation wrap.
- [Go Code Review Comments: error strings](https://go.dev/wiki/CodeReviewComments#error-strings) — message conventions.

---

Back to [00-concepts.md](00-concepts.md) | Prev: [05-validation-join-accumulate.md](05-validation-join-accumulate.md) | Next: [07-error-message-hygiene-contract.md](07-error-message-hygiene-contract.md)
