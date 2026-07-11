# Exercise 2: Classify Retryable vs Terminal Errors via Interface-Typed AsType

An outbound client wrapping a flaky dependency must answer one question on every
failure: retry, or give up? The answer lives in the error's *behavior* — is it a
timeout, is it a cancellation, does the domain declare it transient — not in any
one concrete type. This exercise builds that classifier and leans on the fact
that `errors.AsType`'s type parameter can be an interface, so you match by
contract across packages you do not own.

This module is fully self-contained: its own module, the classifier, a demo, and
tests. Nothing here imports any other exercise.

## What you'll build

```text
retryclass/                independent module: example.com/retryclass
  go.mod                   go 1.26 (errors.AsType needs it)
  retryclass.go            Decision, Retryable interface, Classify(err) Decision
  cmd/
    demo/
      main.go              classifies timeouts, cancellation, upstream statuses
  retryclass_test.go       table tests + interface-by-contract test + Example
```

Files: `retryclass.go`, `cmd/demo/main.go`, `retryclass_test.go`.
Implement: a `Decision` enum, a `Retryable` interface that embeds `error`, and `Classify(err error) Decision` using `errors.Is` for sentinels and `errors.AsType[net.Error]` / `errors.AsType[Retryable]` for behavioral matches.
Test: a table over timeouts, non-timeouts, context sentinels, and domain verdicts; a test that pulls `net.Error` out of an `*net.OpError` by contract; an `Example`.
Verify: `go test -count=1 -race ./...`

Set up the module. `errors.AsType` requires Go 1.26:

```bash
mkdir -p ~/go-exercises/retryclass/cmd/demo
cd ~/go-exercises/retryclass
go mod init example.com/retryclass
go mod edit -go=1.26
```

### Matching by contract, not by concrete type

The reason to reach for the interface form of `AsType` here is coupling. A retry
classifier that hard-codes `*net.OpError`, `*os.SyscallError`, and every driver's
private error type is brittle: it breaks the moment a dependency wraps its errors
differently, and it forces you to import types you should not depend on. What you
actually care about is the *contract* `net.Error` — `Error() string` plus
`Timeout() bool`. `errors.AsType[net.Error](err)` walks the tree and returns the
first error whose dynamic type implements that interface, so a timeout buried
inside an `*net.OpError`, which itself implements `net.Error`, is found without
naming `OpError` at all. On a hit you immediately call `ne.Timeout()` on the
returned value — payoff (1) again, but now the "field" is a behavioral method.

This interface form is only available because `AsType`'s constraint is `E error`
and `net.Error` embeds `error`. The same reasoning lets you define a *domain*
contract:

```go
type Retryable interface {
	error
	Retryable() bool
}
```

`errors.AsType[Retryable](err)` finds any domain error that has opted into
declaring its own retry policy, again without the classifier knowing the concrete
type. This is the constraint boundary to internalize: if `Retryable` did not embed
`error`, it would not satisfy `E error`, and this call would not compile — you
would be forced back to `errors.As`, whose target may be any interface. Matching
by a non-error interface is exactly the job that stays with `As`; that trade-off
is the subject of Exercise 3.

### Order encodes policy

`Classify` is a sequence of guards, and their order is the policy. Cancellation is
checked first with `errors.Is(err, context.Canceled)`: an explicit "stop" from the
caller is terminal even if some inner layer looks transient, because retrying work
the caller abandoned is wasted and can be harmful. A per-attempt
`context.DeadlineExceeded` is treated as retryable at the call level — the single
attempt ran out of time, but the overall operation may still have budget. Only
then does the classifier consult behavior: a `net.Error` that reports a timeout is
retried, and finally a domain error's own `Retryable()` verdict is honored. Note
the division of labor: the two context checks are *sentinel identity*, so they use
`errors.Is`; the two behavioral checks are *type-with-a-method*, so they use
`errors.AsType`. Using `AsType` for the context sentinels would be a category
error, and the concepts file calls that out explicitly.

Create `retryclass.go`:

```go
// Package retryclass decides whether a failed outbound call should be retried
// or treated as terminal, by classifying the error tree with errors.AsType and
// errors.Is rather than by string-matching or concrete-type coupling.
package retryclass

import (
	"context"
	"errors"
	"net"
)

// Decision is the outcome of classifying an error.
type Decision int

const (
	// Terminal means the caller should give up: the error will not resolve on
	// its own, or the caller has cancelled.
	Terminal Decision = iota
	// Retry means another attempt (after backoff) may succeed.
	Retry
)

func (d Decision) String() string {
	if d == Retry {
		return "retry"
	}
	return "terminal"
}

// Retryable is a behavioral interface a domain error can implement to declare
// whether it is worth retrying. It embeds error so it satisfies the [E error]
// constraint of errors.AsType; a bare interface{ Retryable() bool } could not.
type Retryable interface {
	error
	Retryable() bool
}

// Classify inspects err's tree and returns Retry or Terminal. The order encodes
// policy: an explicit cancellation is terminal even if a lower layer looks
// transient; a network timeout is retryable; a domain error's own Retryable
// verdict is honored last.
func Classify(err error) Decision {
	if err == nil {
		return Terminal
	}

	// A cancelled context is an explicit "stop"; do not retry regardless of
	// what wrapped it. This is sentinel identity, so use errors.Is.
	if errors.Is(err, context.Canceled) {
		return Terminal
	}

	// A per-attempt deadline being exceeded is retryable at the call level.
	if errors.Is(err, context.DeadlineExceeded) {
		return Retry
	}

	// Match by contract, not by concrete type: any error in the tree that
	// implements net.Error and reports a timeout is worth another attempt.
	if ne, ok := errors.AsType[net.Error](err); ok && ne.Timeout() {
		return Retry
	}

	// Finally honor a domain error's own verdict via a custom interface E that
	// embeds error.
	if r, ok := errors.AsType[Retryable](err); ok {
		if r.Retryable() {
			return Retry
		}
		return Terminal
	}

	return Terminal
}
```

### The runnable demo

The demo classifies the failures a real client sees: a dial timeout (built as a
genuine `*net.OpError`, which satisfies `net.Error`), a context deadline, a caller
cancellation, an upstream 503 and 400 through a domain error that implements
`Retryable`, and a plain error with no signal.

Create `cmd/demo/main.go`:

```go
package main

import (
	"context"
	"errors"
	"fmt"
	"net"

	"example.com/retryclass"
)

// upstreamError is a domain error carrying an HTTP status from a downstream
// service; it declares its own retry policy through the Retryable interface.
type upstreamError struct {
	status int
}

func (e *upstreamError) Error() string { return fmt.Sprintf("upstream returned %d", e.status) }

func (e *upstreamError) Retryable() bool {
	return e.status == 429 || (e.status >= 500 && e.status <= 599)
}

func main() {
	// A real *net.OpError satisfies net.Error; here we build one that reports a
	// timeout, as a dial timeout would.
	netTimeout := &net.OpError{Op: "dial", Net: "tcp", Err: timeoutErr{}}

	cases := []struct {
		label string
		err   error
	}{
		{"dial timeout", fmt.Errorf("call payments: %w", netTimeout)},
		{"context deadline", fmt.Errorf("call payments: %w", context.DeadlineExceeded)},
		{"caller cancelled", fmt.Errorf("call payments: %w", context.Canceled)},
		{"upstream 503", fmt.Errorf("call payments: %w", &upstreamError{status: 503})},
		{"upstream 400", fmt.Errorf("call payments: %w", &upstreamError{status: 400})},
		{"plain error", errors.New("malformed response")},
	}

	for _, c := range cases {
		fmt.Printf("%-18s -> %s\n", c.label, retryclass.Classify(c.err))
	}
}

// timeoutErr is a minimal net.Error used as the cause of the demo's OpError.
type timeoutErr struct{}

func (timeoutErr) Error() string   { return "i/o timeout" }
func (timeoutErr) Timeout() bool   { return true }
func (timeoutErr) Temporary() bool { return false }
```

Run it:

```bash
go run ./cmd/demo
```

Expected output:

```
dial timeout       -> retry
context deadline   -> retry
caller cancelled   -> terminal
upstream 503       -> retry
upstream 400       -> terminal
plain error        -> terminal
```

### Tests

The table exercises every guard, including the two that are easy to get wrong: a
`net.Error` that is *not* a timeout (connection refused) must be terminal, and
`context.Canceled` must stay terminal even though it arrives wrapped. Two fake
`net.Error` implementations (`timeoutErr`, `refusedErr`) stand in for real network
errors, and `*net.OpError` is used directly to prove the contract match reaches a
timeout nested inside a standard-library wrapper.
`TestInterfaceMatchByContract` isolates the headline idiom: pull `net.Error` out
of a wrapped `*net.OpError` and read `Timeout()` off the returned value.

Create `retryclass_test.go`:

```go
package retryclass

import (
	"context"
	"errors"
	"fmt"
	"net"
	"testing"
)

// timeoutErr implements net.Error and reports a timeout.
type timeoutErr struct{}

func (timeoutErr) Error() string   { return "i/o timeout" }
func (timeoutErr) Timeout() bool   { return true }
func (timeoutErr) Temporary() bool { return false }

// refusedErr implements net.Error but is not a timeout (e.g. connection refused).
type refusedErr struct{}

func (refusedErr) Error() string   { return "connection refused" }
func (refusedErr) Timeout() bool   { return false }
func (refusedErr) Temporary() bool { return false }

// upstreamError declares its own retry policy through the Retryable interface.
type upstreamError struct {
	status int
}

func (e *upstreamError) Error() string { return fmt.Sprintf("upstream returned %d", e.status) }

func (e *upstreamError) Retryable() bool {
	return e.status == 429 || (e.status >= 500 && e.status <= 599)
}

func TestClassify(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name string
		err  error
		want Decision
	}{
		{"nil", nil, Terminal},
		{"net timeout wrapped", fmt.Errorf("dial: %w", timeoutErr{}), Retry},
		{"net timeout via OpError", &net.OpError{Op: "read", Net: "tcp", Err: timeoutErr{}}, Retry},
		{"net non-timeout is terminal", fmt.Errorf("dial: %w", refusedErr{}), Terminal},
		{"context deadline exceeded", fmt.Errorf("call: %w", context.DeadlineExceeded), Retry},
		{"context canceled beats timeout", fmt.Errorf("call: %w", context.Canceled), Terminal},
		{"retryable 503", fmt.Errorf("call: %w", &upstreamError{status: 503}), Retry},
		{"retryable 429", &upstreamError{status: 429}, Retry},
		{"non-retryable 400", fmt.Errorf("call: %w", &upstreamError{status: 400}), Terminal},
		{"unknown is terminal", errors.New("boom"), Terminal},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			if got := Classify(tc.err); got != tc.want {
				t.Errorf("Classify(%v) = %s, want %s", tc.err, got, tc.want)
			}
		})
	}
}

// TestInterfaceMatchByContract shows the high-value idiom: matching by the
// net.Error contract finds a timeout wrapped inside an *net.OpError without any
// dependency on a concrete net error type.
func TestInterfaceMatchByContract(t *testing.T) {
	t.Parallel()
	err := fmt.Errorf("outbound: %w", &net.OpError{Op: "dial", Net: "tcp", Err: timeoutErr{}})
	ne, ok := errors.AsType[net.Error](err)
	if !ok {
		t.Fatal("expected an error implementing net.Error in the tree")
	}
	if !ne.Timeout() {
		t.Fatal("expected Timeout() == true")
	}
}

func ExampleClassify() {
	err := fmt.Errorf("call billing: %w", context.Canceled)
	fmt.Println(Classify(err))
	// Output: terminal
}
```

## Review

The classifier is correct when it decides on behavior, not on string content, and
when the guard order matches the policy: cancellation before timeout, sentinel
checks with `errors.Is` and behavioral checks with `errors.AsType`. The trap
specific to the interface form is the `E error` constraint — if you try
`errors.AsType[interface{ Timeout() bool }]`, it will not compile, because that
interface does not embed `error`; the fix is to match `net.Error`, which does.
Watch the `net`-non-timeout case: `AsType[net.Error]` succeeds for a connection
refused too, so the `&& ne.Timeout()` guard is what keeps it terminal — drop it
and every network error becomes retryable. Confirm with `go test -race ./...`; the
table covers each branch and `TestInterfaceMatchByContract` proves the contract
match reaches into a standard-library wrapper.

## Resources

- [errors package (AsType, Is)](https://pkg.go.dev/errors) — AsType with an interface type argument and Is for sentinels.
- [net.Error interface](https://pkg.go.dev/net#Error) — the `Timeout() bool` contract this classifier matches on.
- [context package (Canceled, DeadlineExceeded)](https://pkg.go.dev/context#pkg-variables) — the sentinel errors checked with `errors.Is`.
- [net.OpError](https://pkg.go.dev/net#OpError) — a standard-library error that implements `net.Error` and wraps its cause.

---

Back to [00-concepts.md](00-concepts.md) | Previous: [01-http-error-mapper.md](01-http-error-mapper.md) | Next: [03-astype-vs-as-tradeoffs.md](03-astype-vs-as-tradeoffs.md)
