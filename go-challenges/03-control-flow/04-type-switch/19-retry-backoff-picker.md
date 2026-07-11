# Exercise 19: Select Exponential, Linear, or Fixed Backoff by Error Class

**Nivel: Intermedio** — validacion rapida (un test corto).

A retry coordinator sits between a caller and a flaky dependency, deciding
after every failed attempt whether to retry and how long to wait first. That
decision depends entirely on what kind of failure occurred: a transient
network blip warrants exponential backoff so repeated failures do not
hammer a struggling dependency; a server that returned `429` with a
`Retry-After` header has already told the coordinator exactly how long to
wait, and guessing its own delay would ignore that signal; a validation
failure means the request itself is malformed and retrying it, at any
delay, would just fail the same way again.

## What you'll build

```text
retry-backoff-picker/       independent module: example.com/retry-backoff-picker
  go.mod                     go 1.24
  retrypick.go               Pick(err error, attempt int) Decision
  cmd/
    demo/
      main.go                classifies one of each error class
  retrypick_test.go           table test over every error class plus a wrapped error
```

- Files: `retrypick.go`, `cmd/demo/main.go`, `retrypick_test.go`.
- Implement: `Pick(err error, attempt int) Decision`, type-switching on
  `*TransientError`, `*RateLimitedError`, `*ValidationError`, and `nil`.
- Test: each error class's retry decision and backoff, the exponential
  backoff at several attempt counts including its cap, and a
  `*TransientError` wrapped by `fmt.Errorf("...: %w", ...)` proving a bare
  type switch does not unwrap it.

Set up the module:

```bash
mkdir -p ~/go-exercises/retry-backoff-picker/cmd/demo
cd ~/go-exercises/retry-backoff-picker
go mod init example.com/retry-backoff-picker
go mod edit -go=1.24
```

`Pick` is a bare type switch, not `errors.As`: it inspects `err`'s dynamic
type directly and does not walk a wrap chain. That is a deliberate, narrow
contract — the coordinator must be handed the error class produced at the
failure site, before any `fmt.Errorf("...: %w", ...)` wrapping is added
further up the call stack. The test proves this explicitly by wrapping a
`*TransientError` and showing it lands on the non-retryable default instead
of the exponential-backoff branch, which is exactly the trap the
`00-concepts.md` file for this lesson calls out: a bare type switch is not
`errors.As`, and code that assumes otherwise silently misroutes wrapped
errors. `*RateLimitedError` short-circuits the coordinator's own backoff
calculation entirely — the server already told it the exact wait, and
computing a different one would ignore that signal. The exponential
calculation itself is capped at `maxDelay` so a long-running retry loop
never computes an unbounded sleep duration from a large attempt count.

Create `retrypick.go`:

```go
package retrypick

import (
	"fmt"
	"time"
)

const (
	baseDelay = 100 * time.Millisecond
	maxDelay  = 30 * time.Second
)

// TransientError marks a failure the retry coordinator should retry with
// exponential backoff, such as a dial timeout or a reset connection.
type TransientError struct {
	Err error
}

func (e *TransientError) Error() string { return fmt.Sprintf("transient: %v", e.Err) }

// RateLimitedError marks a failure whose retry delay was told to us by the
// server, so the coordinator must honor that exact delay instead of
// computing its own.
type RateLimitedError struct {
	RetryAfter time.Duration
}

func (e *RateLimitedError) Error() string {
	return fmt.Sprintf("rate limited, retry after %s", e.RetryAfter)
}

// ValidationError marks a permanent failure: the request itself is
// malformed, and retrying it would just fail again the same way.
type ValidationError struct {
	Field string
}

func (e *ValidationError) Error() string { return fmt.Sprintf("invalid field %q", e.Field) }

// Decision is the retry coordinator's verdict for one failed attempt.
type Decision struct {
	Retry   bool
	Backoff time.Duration
}

// Pick classifies err by its concrete type and returns whether to retry and
// with what backoff. This is a bare type switch, not errors.As: it inspects
// the error's dynamic type directly and does not walk a wrap chain. Callers
// must hand Pick the error class produced at the failure site, before any
// fmt.Errorf("...: %w", ...) wrapping is added further up the call stack —
// otherwise a *TransientError wrapped by a caller silently falls to default
// here and is treated as non-retryable.
func Pick(err error, attempt int) Decision {
	switch e := err.(type) {
	case nil:
		return Decision{Retry: false}
	case *RateLimitedError:
		return Decision{Retry: true, Backoff: e.RetryAfter}
	case *TransientError:
		return Decision{Retry: true, Backoff: exponential(attempt)}
	case *ValidationError:
		return Decision{Retry: false}
	default:
		// An error class the coordinator does not recognize is treated as
		// non-retryable rather than guessed at, so a new error type must be
		// added here deliberately before it can be retried.
		return Decision{Retry: false}
	}
}

// exponential computes base * 2^attempt, capped at maxDelay so a long retry
// loop never sleeps for an unbounded amount of time.
func exponential(attempt int) time.Duration {
	if attempt < 0 {
		attempt = 0
	}
	delay := baseDelay
	for i := 0; i < attempt; i++ {
		delay *= 2
		if delay >= maxDelay {
			return maxDelay
		}
	}
	return delay
}
```

### The runnable demo

Create `cmd/demo/main.go`:

```go
package main

import (
	"errors"
	"fmt"
	"time"

	"example.com/retry-backoff-picker"
)

func main() {
	errs := []error{
		&retrypick.TransientError{Err: errors.New("connection reset")},
		&retrypick.RateLimitedError{RetryAfter: 5 * time.Second},
		&retrypick.ValidationError{Field: "email"},
	}
	for i, err := range errs {
		d := retrypick.Pick(err, i)
		fmt.Printf("%v -> retry=%v backoff=%s\n", err, d.Retry, d.Backoff)
	}
}
```

```bash
go run ./cmd/demo
```

Expected output:

```text
transient: connection reset -> retry=true backoff=100ms
rate limited, retry after 5s -> retry=true backoff=5s
invalid field "email" -> retry=false backoff=0s
```

### Tests

Create `retrypick_test.go`:

```go
package retrypick

import (
	"errors"
	"fmt"
	"testing"
	"time"
)

func TestPick(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name    string
		err     error
		attempt int
		want    Decision
	}{
		{"nil error does not retry", nil, 0, Decision{Retry: false}},
		{"rate limited honors server delay", &RateLimitedError{RetryAfter: 5 * time.Second}, 3, Decision{Retry: true, Backoff: 5 * time.Second}},
		{"transient error backs off exponentially", &TransientError{Err: errors.New("reset")}, 0, Decision{Retry: true, Backoff: 100 * time.Millisecond}},
		{"transient error at attempt 3", &TransientError{Err: errors.New("reset")}, 3, Decision{Retry: true, Backoff: 800 * time.Millisecond}},
		{"transient error backoff caps out", &TransientError{Err: errors.New("reset")}, 20, Decision{Retry: true, Backoff: maxDelay}},
		{"validation error never retries", &ValidationError{Field: "email"}, 0, Decision{Retry: false}},
		{"wrapped transient error is not unwrapped by a bare switch", fmt.Errorf("call failed: %w", &TransientError{Err: errors.New("reset")}), 0, Decision{Retry: false}},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			got := Pick(tt.err, tt.attempt)
			if got != tt.want {
				t.Fatalf("Pick(%v, %d) = %+v, want %+v", tt.err, tt.attempt, got, tt.want)
			}
		})
	}
}
```

Verify: `go test -count=1 ./...`

## Review

The exponential branch is correct because it derives its delay purely from
`attempt` with no shared mutable state, so the same attempt count always
produces the same backoff regardless of call order — a property the retry
coordinator depends on when the same error class is hit concurrently by
several in-flight requests. The wrapped-error test case is the one to
protect above the others: it is tempting to "fix" a wrapped-error bug by
adding `errors.As` calls inside each case, but that changes `Pick`'s
contract from "classify this concrete error" to "search a wrap chain for
this type," which is a different function with a different cost profile.
The discipline this exercise drills is the other side of that trade-off:
document, as the comment on `Pick` does, that callers must hand it
unwrapped errors — and let a wrapped error visibly fail closed (treated as
non-retryable) rather than silently misclassify.

## Resources

- [Go Specification: Type switches](https://go.dev/ref/spec#Type_switches)
- [errors.As and errors.Is](https://pkg.go.dev/errors)
- [Google SRE Workbook: Handling Overload (retries and backoff)](https://sre.google/sre-book/handling-overload/)

---

Back to [00-concepts.md](00-concepts.md) | Prev: [18-oauth-grant-handler.md](18-oauth-grant-handler.md) | Next: [20-tenant-quota-enforcer.md](20-tenant-quota-enforcer.md)
