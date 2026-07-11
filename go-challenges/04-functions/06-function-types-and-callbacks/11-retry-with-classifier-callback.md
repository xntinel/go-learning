# Exercise 11: Retry Loop Where Both the Operation and the Policy Are Callbacks

**Nivel: Intermedio** — validacion rapida (un test corto).

Every SDK's "with retries" wrapper is the same shape underneath: a loop that
calls your operation, plus a callback that decides whether the error it got
back is worth trying again. This module builds `Retry(maxAttempts, op,
retryable)` with named function types for both callbacks. No sleeps or
backoff timing — that is the production extension, noted at the end.

Self-contained: its own `go mod init`, all code inline, its own demo and test.

## What you'll build

```text
retry/                      independent module: example.com/retry-callback
  go.mod                     go 1.24
  retry.go                   type Op, type Classifier, func Retry
  cmd/
    demo/
      main.go                runnable demo: transient failures then success
  retry_test.go              table test: success, permanent error, exhaustion
```

Files: `retry.go`, `cmd/demo/main.go`, `retry_test.go`.
Implement: `type Op func() error`, `type Classifier func(error) bool`, and
`func Retry(maxAttempts int, op Op, retryable Classifier) error`.
Test: a table covering transient-then-success (3 calls), a permanent error
(1 call), and exhaustion (wrapped error still matches the sentinel via
`errors.Is`).
Verify: `go test -count=1 ./...`

```bash
mkdir -p ~/go-exercises/retry-callback/cmd/demo
cd ~/go-exercises/retry-callback
go mod init example.com/retry-callback
go mod edit -go=1.24
```

### Why named function types

`Retry` could take `op func() error, retryable func(error) bool` and compile
identically, but naming the types buys readability (`Op`/`Classifier` say
what each callback's job is) and interchangeability (a closure, a function,
or a method value like `client.Ping` all satisfy the named type with no
adapter). The split isolates concerns too: `Op` just tries once; `Classifier`
knows, for its own domain, which errors are transient (a connection reset, a
503) versus permanent (a validation failure, a 404) — so the same loop serves
any transport by swapping the `Classifier`.

Create `retry.go`:

```go
package retry

import "fmt"

// Op is the unit of work. A named type, not a bare func() error, so it reads
// in a signature and a caller can pass a closure, a top-level func, or a
// method value wherever an Op is expected.
type Op func() error

// Classifier decides whether an error returned by an Op is worth retrying.
// It knows nothing about the retry loop; it only knows transient from
// permanent for its own domain (HTTP status, driver error code, and so on).
type Classifier func(error) bool

// Retry runs op up to maxAttempts times. It stops immediately on success
// (nil), stops immediately if retryable reports the error as permanent, and
// otherwise keeps trying until maxAttempts is exhausted. On exhaustion it
// wraps the last error with the attempt count so the caller can tell "failed
// once" from "failed after every retry."
func Retry(maxAttempts int, op Op, retryable Classifier) error {
	var lastErr error
	for attempt := 1; attempt <= maxAttempts; attempt++ {
		lastErr = op()
		if lastErr == nil {
			return nil
		}
		if !retryable(lastErr) {
			return lastErr
		}
	}
	return fmt.Errorf("after %d attempts: %w", maxAttempts, lastErr)
}
```

### The runnable demo

Create `cmd/demo/main.go`:

```go
package main

import (
	"errors"
	"fmt"

	"example.com/retry-callback"
)

var errTransient = errors.New("connection reset")

func main() {
	attempts := 0
	op := func() error {
		attempts++
		if attempts < 3 {
			return errTransient
		}
		return nil
	}

	err := retry.Retry(5, op, func(e error) bool {
		return errors.Is(e, errTransient)
	})
	fmt.Printf("attempts=%d err=%v\n", attempts, err)
}
```

Run it (`go run ./cmd/demo`); expected output:

```
attempts=3 err=<nil>
```

### Tests

The table's `failUntil` drives one op per case: `2` fails twice then
succeeds, `0` always returns the permanent error, `99` always fails and
exhausts `maxAttempts`. `errors.Is` on that last case proves `%w` kept the
sentinel reachable through the attempt-count wrapping.

Create `retry_test.go`:

```go
package retry

import (
	"errors"
	"testing"
)

var (
	errTransient = errors.New("transient")
	errPermanent = errors.New("permanent")
)

func retryable(e error) bool { return errors.Is(e, errTransient) }

func TestRetry(t *testing.T) {
	tests := []struct {
		name        string
		failUntil   int
		maxAttempts int
		wantCalls   int
		wantErr     error
	}{
		{"transient twice then success", 2, 5, 3, nil},
		{"permanent error stops after one attempt", 0, 5, 1, errPermanent},
		{"exhaustion wraps the last error", 99, 4, 4, errTransient},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			calls := 0
			op := Op(func() error {
				calls++
				if tc.failUntil == 0 {
					return errPermanent
				}
				if calls <= tc.failUntil {
					return errTransient
				}
				return nil
			})

			err := Retry(tc.maxAttempts, op, retryable)
			if calls != tc.wantCalls {
				t.Errorf("op called %d times, want %d", calls, tc.wantCalls)
			}
			if tc.wantErr == nil && err != nil {
				t.Errorf("Retry() = %v, want nil", err)
			}
			if tc.wantErr != nil && !errors.Is(err, tc.wantErr) {
				t.Errorf("Retry() = %v, want to match %v", err, tc.wantErr)
			}
		})
	}
}
```

Run it: `go test -count=1 ./...`

## Review

Three axes, one per case: success stops the loop on the first nil;
`retryable` returning false ends it immediately on a permanent error;
exhaustion runs exactly `maxAttempts` times, with `%w` keeping the sentinel
reachable through `errors.Is`. Left out on purpose: backoff. A production
retry sleeps between attempts with a growing delay and watches a
`context.Context` so a cancelled caller does not wait out an uninterruptible
sleep — more machinery around the same `Op`/`Classifier` callbacks.

## Resources

- [Go Specification: Function types](https://go.dev/ref/spec#Function_types)
- [errors.Is](https://pkg.go.dev/errors#Is)

---

Back to [00-concepts.md](00-concepts.md) | Prev: [10-shutdown-hook-stack.md](10-shutdown-hook-stack.md) | Next: [12-pubsub-topic-subscribers.md](12-pubsub-topic-subscribers.md)
