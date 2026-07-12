# Exercise 5: Decide Retry via errors.As Into an Interface Target

Whether to retry a failed operation depends on the error's *behavior*, not its
concrete type: some errors are transient and worth another attempt, others are
permanent and must fail fast. This exercise builds a retry gate that classifies by
extracting a behavioral interface — `interface{ Retryable() bool }` — with the
interface-target form of `errors.As`, retrying only when the recovered value says
it is retryable.

This module is fully self-contained: its own `go mod init`, demo, and tests.

## What you'll build

```text
retrygate/                      independent module: example.com/retrygate
  go.mod                        go 1.25
  retry.go                      Retryable interface, TransientError, Do(maxAttempts, op)
  retry_test.go                 transient retried to N, permanent stops at 1, plain err
  cmd/demo/main.go              runnable demo showing attempt counts
```

Files: `retry.go`, `retry_test.go`, `cmd/demo/main.go`.
Implement: `Do(maxAttempts, op)` that calls `op`, and on error uses `errors.As(err, &r)` with `r` an `interface{ Retryable() bool }`, retrying only while `r.Retryable()` is true.
Test: a transient error is retried up to N times; a permanent error (no method, or `Retryable() == false`) returns immediately with attempt count 1; a plain `errors.New` value is treated as non-retryable.
Verify: `go test -count=1 -race ./... && go vet ./...`

Set up the module:

```bash
mkdir -p go-solutions/10-error-handling/03-errors-is-and-errors-as/05-retry-classification-as-interface/cmd/demo
cd go-solutions/10-error-handling/03-errors-is-and-errors-as/05-retry-classification-as-interface
go mod edit -go=1.25
```

### Classify by behavior, not by type

A retry gate should not enumerate concrete error types. If it did, every new
error type that happens to be transient would need to be added to the gate's
allow-list. The robust design classifies by *behavior*: any error that declares
itself retryable, by implementing a `Retryable() bool` method, is eligible; every
other error is not. This is behavior-based classification, and `errors.As` has a
form built for it — the interface target.

`errors.As(err, target)` accepts, besides a pointer to a concrete error type, a
pointer to an *interface* type. When `target` is `*I` for some interface `I`, `As`
walks the tree and binds the first error whose dynamic type implements `I`. So
`var r Retryable; errors.As(err, &r)` searches the tree for anything implementing
`Retryable() bool` and, on success, gives you a value you can call `r.Retryable()`
on — without ever naming the concrete type. If nothing in the tree implements the
interface, `As` returns false and `r` stays nil, which the gate treats as
non-retryable. A plain `errors.New(...)` value has no such method, so it is
non-retryable by default — the safe default, since an unknown error is not
something to hammer.

One rule to respect: the target passed to `As` must be a non-nil pointer to a
concrete error type *or* to an interface type; anything else panics. The
interface form here (`&r` where `r` is `Retryable`) is exactly the allowed
interface case. The gate then loops: call `op`; on `nil`, return the attempt count
and success; on an error that is retryable, loop again until `maxAttempts`; on any
non-retryable error, return immediately. The attempt counter is the observable
contract — a permanent failure returns after exactly one attempt.

Create `retry.go`:

```go
package retrygate

import (
	"errors"
	"fmt"
)

// Retryable is a behavioral interface. Any error that implements it opts into
// retrying; the gate never names a concrete error type.
type Retryable interface {
	Retryable() bool
}

// TransientError marks a failure worth retrying. It wraps a cause and reports
// Retryable() true.
type TransientError struct {
	Cause error
}

func (e *TransientError) Error() string   { return "transient: " + e.Cause.Error() }
func (e *TransientError) Unwrap() error   { return e.Cause }
func (e *TransientError) Retryable() bool { return true }

// Do runs op up to maxAttempts times, retrying only while the returned error
// extracts into a Retryable whose Retryable() reports true. It returns the number
// of attempts made and the last error (nil on success).
func Do(maxAttempts int, op func() error) (attempts int, err error) {
	for attempts = 1; attempts <= maxAttempts; attempts++ {
		err = op()
		if err == nil {
			return attempts, nil
		}
		var r Retryable
		if !errors.As(err, &r) || !r.Retryable() {
			return attempts, err // non-retryable: fail fast
		}
	}
	return maxAttempts, err // exhausted retries
}

// Wrap is a small helper the demo/tests use to build a transient error.
func Wrap(cause error) error { return fmt.Errorf("op failed: %w", &TransientError{Cause: cause}) }
```

### The runnable demo

The demo runs three operations: one transient that always fails (exhausts
retries), one that fails twice then succeeds, and one permanent error (fails
immediately).

Create `cmd/demo/main.go`:

```go
package main

import (
	"errors"
	"fmt"

	"example.com/retrygate"
)

func main() {
	// Always transient: exhausts all attempts.
	n, err := retrygate.Do(3, func() error {
		return retrygate.Wrap(errors.New("connection reset"))
	})
	fmt.Printf("always-transient: attempts=%d err=%v\n", n, err != nil)

	// Transient twice, then succeeds.
	calls := 0
	n, err = retrygate.Do(5, func() error {
		calls++
		if calls < 3 {
			return retrygate.Wrap(errors.New("temporary"))
		}
		return nil
	})
	fmt.Printf("succeeds-on-3: attempts=%d err=%v\n", n, err)

	// Permanent: a plain error has no Retryable method, so no retry.
	n, err = retrygate.Do(3, func() error {
		return errors.New("invalid argument")
	})
	fmt.Printf("permanent: attempts=%d err=%v\n", n, err != nil)
}
```

Run it:

```bash
go run ./cmd/demo
```

Expected output:

```
always-transient: attempts=3 err=true
succeeds-on-3: attempts=3 err=<nil>
permanent: attempts=1 err=true
```

### Tests

The tests pin the attempt counts. `TestTransientRetried` counts calls and asserts
a transient error is attempted exactly `maxAttempts` times. `TestPermanentStops`
asserts a plain error stops after one attempt. `TestRetryableFalse` asserts an
error whose `Retryable()` returns false is also not retried. `TestSuccess`
asserts success returns the attempt on which it first passed.

Create `retry_test.go`:

```go
package retrygate

import (
	"errors"
	"fmt"
	"testing"
)

// falseRetryable implements Retryable but reports false.
type falseRetryable struct{}

func (falseRetryable) Error() string   { return "not worth retrying" }
func (falseRetryable) Retryable() bool { return false }

func TestTransientRetried(t *testing.T) {
	t.Parallel()
	calls := 0
	n, err := Do(4, func() error {
		calls++
		return Wrap(errors.New("temporary"))
	})
	if n != 4 || calls != 4 {
		t.Fatalf("attempts=%d calls=%d, want 4 and 4", n, calls)
	}
	if err == nil {
		t.Fatal("want the last transient error, got nil")
	}
}

func TestPermanentStops(t *testing.T) {
	t.Parallel()
	calls := 0
	n, err := Do(4, func() error {
		calls++
		return errors.New("invalid argument")
	})
	if n != 1 || calls != 1 {
		t.Fatalf("attempts=%d calls=%d, want 1 and 1 (no retry)", n, calls)
	}
	if err == nil {
		t.Fatal("want the permanent error, got nil")
	}
}

func TestRetryableFalse(t *testing.T) {
	t.Parallel()
	calls := 0
	n, _ := Do(4, func() error {
		calls++
		return falseRetryable{}
	})
	if n != 1 || calls != 1 {
		t.Fatalf("attempts=%d calls=%d, want 1 and 1 (Retryable()==false)", n, calls)
	}
}

func TestSuccess(t *testing.T) {
	t.Parallel()
	calls := 0
	n, err := Do(5, func() error {
		calls++
		if calls < 2 {
			return Wrap(errors.New("temporary"))
		}
		return nil
	})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if n != 2 {
		t.Fatalf("attempts=%d, want 2", n)
	}
}

func Example() {
	n, _ := Do(3, func() error { return errors.New("permanent") })
	fmt.Println(n)
	// Output: 1
}
```

## Review

The gate is correct when the attempt count matches the classification: a
retryable error is attempted `maxAttempts` times, and anything non-retryable —
including a plain `errors.New` value with no method — stops after exactly one. The
mechanism is the interface-target form of `errors.As`: `var r Retryable;
errors.As(err, &r)` binds the first tree element implementing `Retryable() bool`,
so the gate classifies by behavior and never enumerates concrete types. Two
mistakes to avoid: passing a non-pointer or a pointer to a non-interface,
non-error type to `As` (it panics), and treating an unclassifiable error as
retryable (it should be the safe non-retryable default). Run `go test -race`.

## Resources

- [errors.As](https://pkg.go.dev/errors#As) — the interface-target form for behavior-based extraction.
- [errors package](https://pkg.go.dev/errors) — traversal rules shared by `Is` and `As`.
- [Go Blog: Working with Errors in Go 1.13](https://go.dev/blog/go1.13-errors) — wrapping so behavior survives context.

---

Back to [00-concepts.md](00-concepts.md) | Previous: [04-validation-join-multierror.md](04-validation-join-multierror.md) | Next: [06-custom-as-method-adapter.md](06-custom-as-method-adapter.md)
