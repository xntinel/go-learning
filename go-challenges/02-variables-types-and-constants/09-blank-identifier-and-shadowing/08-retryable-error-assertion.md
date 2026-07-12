# Exercise 8: Retry Classifier: Type Assertion comma-ok Without Discarding ok

A retry decision function classifies an error as retryable by asserting it against
a capability interface `interface{ Retryable() bool }`. The single-value assertion
`t := err.(Retryabler)` panics on the first error that does not implement the
interface; the comma-ok form `t, ok := err.(Retryabler)` never panics. For wrapped
error chains, `errors.As` is the right tool — a bare assertion misses a capability
buried under a `%w` wrap.

This module is fully self-contained: its own `go mod init`, all code inline, its
own demo and tests. Nothing here imports another exercise.

## What you'll build

```text
retry/                          module: example.com/retry
  go.mod
  retry.go                      Retryabler, TransientError, FatalError, Classify, Do
  cmd/
    demo/
      main.go                   classify bare/wrapped/fatal/nil, run Do with backoff
  retry_test.go                 direct vs errors.As, non-retryable, nil, Do behavior
```

- Files: `retry.go`, `cmd/demo/main.go`, `retry_test.go`.
- Implement: `Classify(err)` using `errors.As` against a capability interface, and `Do(maxAttempts, op)` that retries only on retryable errors.
- Test: a bare retryable error (direct comma-ok matches), a wrapped retryable error (only `errors.As` finds it), a non-retryable error and `nil` (no panic), and `Do` retrying then stopping.
- Verify: `go test -count=1 -race ./...`

Set up the module:

```bash
mkdir -p go-solutions/02-variables-types-and-constants/09-blank-identifier-and-shadowing/08-retryable-error-assertion/cmd/demo
cd go-solutions/02-variables-types-and-constants/09-blank-identifier-and-shadowing/08-retryable-error-assertion
```

### Why comma-ok, and why errors.As beats a bare assertion

A type assertion has two forms and they behave very differently on a miss. The
single-value form panics:

```go
r := err.(Retryabler) // panics if err does not implement Retryabler
```

The comma-ok form never panics; it reports the miss in `ok`:

```go
r, ok := err.(Retryabler) // ok == false on a miss, no panic
```

A classifier runs on *every* error, most of which will not implement your
capability interface, so the single-value form is a guaranteed crash. Discarding
the `ok` (`r, _ := err.(Retryabler)`) is just as bad: on a miss `r` is `nil`, and
calling `r.Retryable()` panics with a nil dereference. Always keep the `ok` and
branch on it.

There is a second, subtler failure. A direct assertion only inspects the error
*value you hold*. Real backend errors are wrapped: `fmt.Errorf("fetch user: %w",
transient)`. The outer error does not implement `Retryabler` — the transient one
inside it does. A direct assertion returns `ok == false` and you wrongly classify
a retryable failure as fatal. `errors.As` walks the whole `%w` chain and binds the
first error that matches your target, so `Classify` uses it. This is the general
rule: prefer `errors.As` over a bare assertion whenever the error may be wrapped.

Create `retry.go`:

```go
package retry

import (
	"errors"
	"fmt"
)

// Retryabler is a capability interface: an error that can classify itself.
type Retryabler interface {
	Retryable() bool
}

// TransientError is a retryable failure (timeout, connection reset).
type TransientError struct {
	Msg string
}

func (e *TransientError) Error() string { return e.Msg }

func (e *TransientError) Retryable() bool { return true }

// FatalError is a permanent failure (not found, validation).
type FatalError struct {
	Msg string
}

func (e *FatalError) Error() string { return e.Msg }

func (e *FatalError) Retryable() bool { return false }

// assertDirect is the naive path: a comma-ok assertion on the error value itself.
// It never panics, but it misses a capability wrapped under %w. Kept to contrast
// with Classify in the tests.
func assertDirect(err error) (retryable bool, matched bool) {
	r, ok := err.(Retryabler)
	if !ok {
		return false, false
	}
	return r.Retryable(), true
}

// Classify reports whether err (or anything it wraps) is retryable. errors.As
// walks the %w chain; a nil or non-capability error classifies as not retryable.
func Classify(err error) bool {
	var r Retryabler
	if errors.As(err, &r) {
		return r.Retryable()
	}
	return false
}

// Do runs op up to maxAttempts times, retrying only while the error is retryable.
// err is declared once and reassigned with = across attempts, never shadowed.
func Do(maxAttempts int, op func() error) error {
	var err error
	for attempt := 1; attempt <= maxAttempts; attempt++ {
		err = op()
		if err == nil {
			return nil
		}
		if !Classify(err) {
			return fmt.Errorf("attempt %d: %w", attempt, err)
		}
	}
	return fmt.Errorf("giving up after %d attempts: %w", maxAttempts, err)
}
```

### The runnable demo

Create `cmd/demo/main.go`:

```go
package main

import (
	"fmt"

	"example.com/retry"
)

func main() {
	transient := &retry.TransientError{Msg: "connection reset"}
	wrapped := fmt.Errorf("fetch user: %w", transient)
	fatal := &retry.FatalError{Msg: "not found"}

	fmt.Println("transient retryable:", retry.Classify(transient))
	fmt.Println("wrapped retryable:", retry.Classify(wrapped))
	fmt.Println("fatal retryable:", retry.Classify(fatal))
	fmt.Println("nil retryable:", retry.Classify(nil))

	attempts := 0
	err := retry.Do(3, func() error {
		attempts++
		if attempts < 3 {
			return transient
		}
		return nil
	})
	fmt.Printf("Do succeeded after %d attempts: err=%v\n", attempts, err)
}
```

Run it:

```bash
go run ./cmd/demo
```

Expected output:

```
transient retryable: true
wrapped retryable: true
fatal retryable: false
nil retryable: false
Do succeeded after 3 attempts: err=<nil>
```

### Tests

`TestWrappedNeedsErrorsAs` is the contrast: the direct assertion misses a wrapped
capability (`matched == false`) while `Classify` finds it. The nil and
non-retryable cases prove nothing panics.

Create `retry_test.go`:

```go
package retry

import (
	"errors"
	"fmt"
	"testing"
)

func TestClassifyBare(t *testing.T) {
	t.Parallel()

	if !Classify(&TransientError{Msg: "reset"}) {
		t.Fatal("transient error should be retryable")
	}
	if Classify(&FatalError{Msg: "gone"}) {
		t.Fatal("fatal error should not be retryable")
	}
}

func TestWrappedNeedsErrorsAs(t *testing.T) {
	t.Parallel()

	wrapped := fmt.Errorf("fetch user: %w", &TransientError{Msg: "reset"})

	if _, matched := assertDirect(wrapped); matched {
		t.Fatal("direct assertion should NOT match a wrapped capability")
	}
	if !Classify(wrapped) {
		t.Fatal("Classify must find the wrapped retryable error via errors.As")
	}
}

func TestClassifyNilAndPlain(t *testing.T) {
	t.Parallel()

	if Classify(nil) {
		t.Fatal("nil error must classify as not retryable")
	}
	if Classify(errors.New("plain")) {
		t.Fatal("a plain error must classify as not retryable")
	}
}

func TestDoRetriesThenSucceeds(t *testing.T) {
	t.Parallel()

	attempts := 0
	err := Do(5, func() error {
		attempts++
		if attempts < 3 {
			return &TransientError{Msg: "reset"}
		}
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	if attempts != 3 {
		t.Fatalf("attempts = %d, want 3", attempts)
	}
}

func TestDoStopsOnFatal(t *testing.T) {
	t.Parallel()

	fatal := &FatalError{Msg: "not found"}
	attempts := 0
	err := Do(5, func() error {
		attempts++
		return fatal
	})
	if !errors.Is(err, fatal) {
		t.Fatalf("error = %v, want wrap of fatal", err)
	}
	if attempts != 1 {
		t.Fatalf("attempts = %d, want 1 (no retry on fatal)", attempts)
	}
}

func ExampleClassify() {
	err := fmt.Errorf("db call: %w", &TransientError{Msg: "timeout"})
	fmt.Println(Classify(err))
	// Output: true
}
```

## Review

The classifier is correct when it never panics and finds a retryable capability
even under a wrap. `TestWrappedNeedsErrorsAs` is the load-bearing test: it proves
the direct comma-ok assertion misses a wrapped capability (so a bare assertion
would misclassify a retryable failure as fatal) while `Classify` finds it via
`errors.As`. `TestClassifyNilAndPlain` proves the non-matching paths return
cleanly with no panic — which the single-value assertion `err.(Retryabler)` would
not. `Do` declares `err` once and reassigns it with `=` across attempts, so no
nested scope shadows the error it ultimately returns.

## Resources

- [`errors.As`](https://pkg.go.dev/errors#As) — matching an error anywhere in the `%w` chain.
- [Go Specification: Type assertions](https://go.dev/ref/spec#Type_assertions) — single-value (panics) vs comma-ok.
- [Go Blog: Working with Errors](https://go.dev/blog/go1.13-errors) — `errors.As`, `errors.Is`, and `%w`.

---

Back to [00-concepts.md](00-concepts.md) | Next: [09-worker-pool-index-discard.md](09-worker-pool-index-discard.md)
