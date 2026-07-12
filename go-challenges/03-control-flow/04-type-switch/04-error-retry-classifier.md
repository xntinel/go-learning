# Exercise 4: Classify Errors for Retry Without Losing the Wrapped Chain

Before a client retries a failed call it must classify the error: transient
(retry), permanent (do not retry), or fatal (stop the whole operation). This is
the exact place a bare type switch silently breaks, because production errors are
wrapped with `fmt.Errorf("...: %w", ...)` and a type switch does not unwrap. This
module builds the classifier the correct way and demonstrates the bug side by
side.

This module is fully self-contained: its own `go mod init`, all code inline, its
own demo and tests.

## What you'll build

```text
retryclass/                  independent module: example.com/retryclass
  go.mod                     go 1.26
  classify.go                Classify(err error) Decision (errors.As/Is); classifyNaive (buggy, for contrast)
  cmd/
    demo/
      main.go                classifies a wrapped net error and a deadline
  classify_test.go           wrapped *net.OpError, deadline, sentinel, nil; naive-vs-correct contrast
```

- Files: `classify.go`, `cmd/demo/main.go`, `classify_test.go`.
- Implement: `Classify(err error) Decision` using `errors.As` for concrete types
  (`*net.OpError`, `*os.SyscallError`) and interface probing (`net.Error.Timeout`),
  plus `errors.Is` for sentinels; and `classifyNaive` using a bare type switch to
  show the failure mode.
- Test: a `*net.OpError` wrapped three times is still retryable via `Classify` but
  missed by `classifyNaive`; `context.DeadlineExceeded` maps to a distinct
  decision; a plain sentinel maps to permanent; `nil` maps to a no-error decision.
- Verify: `go test -count=1 -race ./...`

## Why the bare type switch is the bug, and the correct ordering

A network call deep in a stack returns a `*net.OpError`. Every layer above wraps
it for context: `fmt.Errorf("fetch user: %w", err)`, then `fmt.Errorf("handler:
%w", err)`. The value that reaches your classifier has dynamic type
`*fmt.wrapError`. A bare `switch e := err.(type) { case *net.OpError: ... }`
inspects that outer dynamic type, does not match, and falls to `default`: the
retryable error is misclassified as permanent and the request is dropped. That is
`classifyNaive`, kept in this module purely so a test can prove it wrong.

`Classify` uses the unwrap-aware tools. `errors.As(err, &target)` walks the wrap
chain until it finds a value assignable to `target`; `errors.Is(err, sentinel)`
walks it comparing against a sentinel. The *ordering* of the checks is
load-bearing because several of these overlap:

1. `nil` first — no error is its own decision.
2. `errors.Is(err, context.DeadlineExceeded)` and `context.Canceled` *before* the
   `net.Error` timeout check. This matters: `context.DeadlineExceeded` itself
   implements `net.Error` with `Timeout()` returning true, so if you checked
   `net.Error` first, a deadline would be misread as a transient network timeout.
   Checking the sentinel first gives the deadline its own distinct decision.
3. `errors.Is(err, ErrPermanent)` — a domain sentinel meaning "do not retry".
4. `errors.As` to `net.Error` and probe `Timeout()`; a timeout is retryable.
5. `errors.As` to `*net.OpError` and `*os.SyscallError` — connection-level
   failures (refused, reset) are retryable.
6. `default` — unknown errors are treated as permanent, the safe choice.

Create `classify.go`:

```go
package retryclass

import (
	"context"
	"errors"
	"net"
	"os"
)

// Decision is the retry classification of an error.
type Decision int

const (
	NoError   Decision = iota // err == nil
	Retryable                 // transient; safe to retry
	Permanent                 // will not succeed on retry
	Fatal                     // stop the whole operation (deadline/cancel)
)

func (d Decision) String() string {
	switch d {
	case NoError:
		return "no-error"
	case Retryable:
		return "retryable"
	case Permanent:
		return "permanent"
	case Fatal:
		return "fatal"
	default:
		return "unknown"
	}
}

// ErrPermanent is a domain sentinel meaning the operation must not be retried.
var ErrPermanent = errors.New("permanent failure")

// Classify inspects the whole wrap chain via errors.As / errors.Is.
func Classify(err error) Decision {
	if err == nil {
		return NoError
	}
	// Deadlines and cancellations are distinct and checked before net.Error,
	// because context.DeadlineExceeded also satisfies net.Error with Timeout()==true.
	if errors.Is(err, context.DeadlineExceeded) || errors.Is(err, context.Canceled) {
		return Fatal
	}
	if errors.Is(err, ErrPermanent) {
		return Permanent
	}
	var netErr net.Error
	if errors.As(err, &netErr) && netErr.Timeout() {
		return Retryable
	}
	var opErr *net.OpError
	if errors.As(err, &opErr) {
		return Retryable
	}
	var sysErr *os.SyscallError
	if errors.As(err, &sysErr) {
		return Retryable
	}
	return Permanent
}

// classifyNaive is the WRONG approach kept for contrast: a bare type switch that
// does not unwrap, so any wrapped error falls to the default branch.
func classifyNaive(err error) Decision {
	if err == nil {
		return NoError
	}
	switch err.(type) {
	case *net.OpError, *os.SyscallError:
		return Retryable
	default:
		return Permanent
	}
}
```

## The runnable demo

The demo classifies a `*net.OpError` wrapped twice (as production code wraps it)
and a wrapped `context.DeadlineExceeded`, printing what each approach decides so
the difference is visible.

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

func main() {
	opErr := &net.OpError{Op: "dial", Net: "tcp", Err: errors.New("connection refused")}
	wrapped := fmt.Errorf("handler: %w", fmt.Errorf("fetch user: %w", opErr))
	fmt.Printf("wrapped net error -> %s\n", retryclass.Classify(wrapped))

	deadline := fmt.Errorf("call timed out: %w", context.DeadlineExceeded)
	fmt.Printf("wrapped deadline  -> %s\n", retryclass.Classify(deadline))

	fmt.Printf("plain permanent   -> %s\n", retryclass.Classify(retryclass.ErrPermanent))
	fmt.Printf("no error          -> %s\n", retryclass.Classify(nil))
}
```

Run it:

```bash
go run ./cmd/demo
```

Expected output:

```text
wrapped net error -> retryable
wrapped deadline  -> fatal
plain permanent   -> permanent
no error          -> no-error
```

## Tests

The central test wraps a `*net.OpError` three times and asserts `Classify`
returns `Retryable` while `classifyNaive` returns `Permanent` — both assertions,
so the lesson is explicit that the bare type switch is the bug. The deadline test
proves `context.DeadlineExceeded` maps to `Fatal` (distinct from a transient
timeout). The sentinel and nil tests cover the remaining decisions.

Create `classify_test.go`:

```go
package retryclass

import (
	"context"
	"errors"
	"fmt"
	"net"
	"testing"
)

func TestWrappedNetErrorRetryableButNaiveMisses(t *testing.T) {
	t.Parallel()
	base := &net.OpError{Op: "read", Net: "tcp", Err: errors.New("connection reset")}
	wrapped := fmt.Errorf("a: %w", fmt.Errorf("b: %w", fmt.Errorf("c: %w", base)))

	if got := Classify(wrapped); got != Retryable {
		t.Errorf("Classify(wrapped) = %s, want retryable", got)
	}
	if got := classifyNaive(wrapped); got != Permanent {
		t.Errorf("classifyNaive(wrapped) = %s, want permanent (proves the bare switch bug)", got)
	}
	// The naive switch only works on the unwrapped value:
	if got := classifyNaive(base); got != Retryable {
		t.Errorf("classifyNaive(base) = %s, want retryable", got)
	}
}

func TestDeadlineIsDistinct(t *testing.T) {
	t.Parallel()
	err := fmt.Errorf("request: %w", context.DeadlineExceeded)
	if got := Classify(err); got != Fatal {
		t.Errorf("Classify(deadline) = %s, want fatal", got)
	}
}

func TestSentinelAndNil(t *testing.T) {
	t.Parallel()
	if got := Classify(fmt.Errorf("save: %w", ErrPermanent)); got != Permanent {
		t.Errorf("Classify(ErrPermanent) = %s, want permanent", got)
	}
	if got := Classify(nil); got != NoError {
		t.Errorf("Classify(nil) = %s, want no-error", got)
	}
	if got := Classify(errors.New("who knows")); got != Permanent {
		t.Errorf("Classify(unknown) = %s, want permanent", got)
	}
}
```

## Review

The classifier is correct when a `*net.OpError` wrapped any number of times is
still `Retryable`, when a `context.DeadlineExceeded` is `Fatal` rather than being
mistaken for a transient timeout, and when unknown errors default to `Permanent`
(the safe choice — retrying an unclassifiable error risks an infinite loop). The
two mistakes this module exists to prevent are: reaching for a bare type switch
on the incoming error (it never unwraps, so every wrapped error misroutes — use
`errors.As`/`errors.Is`); and checking `net.Error` before the context sentinels,
which reclassifies a deadline as a retryable timeout because the deadline type
also satisfies `net.Error`. Order the checks most-specific first.

## Resources

- [errors.As (unwrap-aware type matching)](https://pkg.go.dev/errors#As)
- [Go Blog: Working with Errors in Go 1.13](https://go.dev/blog/go1.13-errors)
- [net.Error](https://pkg.go.dev/net#Error)
- [context: DeadlineExceeded satisfies net.Error](https://pkg.go.dev/context#pkg-variables)

---

Prev: [03-sql-scanner-type-switch.md](03-sql-scanner-type-switch.md) | Up: [00-concepts.md](00-concepts.md) | Next: [05-config-value-coercion.md](05-config-value-coercion.md)
