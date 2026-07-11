# Exercise 17: Polling Retry Logic Spins on Unhandled Error Types Due to Missing Switch Cases

**Nivel: Intermedio** — validacion rapida (un test corto).

A readiness poll that classifies every probe failure into "retry" or "give
up now" only works as long as those two cases are truly exhaustive. The
moment a probe starts returning some third kind of error — a malformed
response, a type nobody wired a sentinel for — a `switch` with no `default`
case treats it as neither, silently retrying it exactly like a transient
failure until every attempt is burned, instead of surfacing the gap in the
classification immediately. This module is fully self-contained: its own
`go mod init`, all code inline, its own demo and tests.

## What you'll build

```text
pollretry/                  independent module: example.com/polling-retry-unhandled-error-case
  go.mod
  pollretry.go               ErrRetryable, ErrPermanentFailure, PollUntilReady
  cmd/
    demo/
      main.go                runnable demo: retryable-then-ready, then an unclassified failure
  pollretry_test.go           unclassified error stops after one attempt, plus the two classified paths
```

- Files: `pollretry.go`, `cmd/demo/main.go`, `pollretry_test.go`.
- Implement: `PollUntilReady(maxAttempts int, probe Prober) error` that classifies every probe error via `errors.Is` and has an explicit policy for anything that matches neither known sentinel.
- Test: a probe that returns an unclassified error and assert `probe` is called exactly once, not `maxAttempts` times; two more tests pin the retryable and permanent paths.
- Verify: `go test -count=1 ./...`.

```bash
mkdir -p ~/go-exercises/polling-retry-unhandled-error-case/cmd/demo
cd ~/go-exercises/polling-retry-unhandled-error-case
go mod init example.com/polling-retry-unhandled-error-case
```

### Why a switch without a default silently retries the unknown

`PollUntilReady`'s per-attempt classification is a tagless `switch` over
`errors.Is` checks — the idiomatic shape for range-like conditions in Go.
The bug is dropping the `default` arm on the assumption that every error
will be one of the two sentinels:

```go
switch {
case errors.Is(err, ErrRetryable):
	continue
case errors.Is(err, ErrPermanentFailure):
	return fmt.Errorf("permanent failure on attempt %d: %w", attempt, err)
}
// BUG: no default -- an unclassified err falls through to here and the
// for loop simply moves on to the next attempt, indistinguishable from
// a deliberate retry.
```

Go's `switch` never falls through to another case implicitly, but when
*no* case matches and there is no `default`, execution just continues
past the `switch` entirely — here, straight to the bottom of the loop
body, which is exactly what a `continue` under `ErrRetryable` does too.
That is the trap: the absence of a matching case behaves identically to
the explicit retry case, so an error nobody classified gets retried
`maxAttempts` times, wasting every attempt and delaying the real failure
message behind a generic "exceeded N attempts" — which then sends whoever
is debugging looking at timeouts and backoff instead of at the one probe
response that was never accounted for. The fix is a `default` that treats
an unclassified error as its own outcome — reported immediately, not
retried — so adding a new failure mode to the system without also adding
its sentinel and case produces a loud, specific error instead of a quiet
waste of the retry budget.

Create `pollretry.go`:

```go
package pollretry

import (
	"errors"
	"fmt"
)

// ErrRetryable marks a failure as transient: the caller should try again.
var ErrRetryable = errors.New("transient failure")

// ErrPermanentFailure marks a failure as non-recoverable: retrying will not
// help, so PollUntilReady should stop immediately.
var ErrPermanentFailure = errors.New("permanent failure")

// Prober reports on a single attempt's outcome. A nil error means ready.
type Prober func(attempt int) error

// PollUntilReady calls probe up to maxAttempts times. Every error is
// classified with errors.Is against the known sentinels: ErrRetryable tries
// again on the next iteration, ErrPermanentFailure stops immediately, and
// any error that matches neither is treated as a policy gap -- it is
// reported immediately rather than silently retried, because retrying an
// error nobody classified is retrying a failure mode nobody has reasoned
// about.
func PollUntilReady(maxAttempts int, probe Prober) error {
	for attempt := 1; attempt <= maxAttempts; attempt++ {
		err := probe(attempt)
		if err == nil {
			return nil
		}

		switch {
		case errors.Is(err, ErrRetryable):
			continue
		case errors.Is(err, ErrPermanentFailure):
			return fmt.Errorf("permanent failure on attempt %d: %w", attempt, err)
		default:
			return fmt.Errorf("unclassified polling error on attempt %d: %w", attempt, err)
		}
	}
	return fmt.Errorf("exceeded %d attempts without becoming ready", maxAttempts)
}
```

### The runnable demo

The demo runs two scenarios: transient failures followed by readiness, and
a single unclassified failure that stops the poll on its first attempt.

Create `cmd/demo/main.go`:

```go
package main

import (
	"errors"
	"fmt"

	"example.com/polling-retry-unhandled-error-case"
)

func main() {
	// Scenario 1: two transient failures, then ready.
	attempts := 0
	err := pollretry.PollUntilReady(5, func(attempt int) error {
		attempts++
		if attempt < 3 {
			return fmt.Errorf("attempt %d: %w", attempt, pollretry.ErrRetryable)
		}
		return nil
	})
	fmt.Println("scenario 1 error:", err, "attempts:", attempts)

	// Scenario 2: an unclassified error stops the poll immediately instead
	// of burning through every remaining attempt.
	attempts = 0
	err = pollretry.PollUntilReady(5, func(attempt int) error {
		attempts++
		return errors.New("dns lookup returned garbage")
	})
	fmt.Println("scenario 2 error:", err, "attempts:", attempts)
}
```

```bash
go run ./cmd/demo
```

Expected output:

```text
scenario 1 error: <nil> attempts: 3
scenario 2 error: unclassified polling error on attempt 1: dns lookup returned garbage attempts: 1
```

### Tests

Create `pollretry_test.go`:

```go
package pollretry

import (
	"errors"
	"testing"
)

func TestUnclassifiedErrorStopsAfterOneAttempt(t *testing.T) {
	weird := errors.New("dns lookup returned garbage")
	calls := 0

	err := PollUntilReady(5, func(attempt int) error {
		calls++
		return weird
	})

	if !errors.Is(err, weird) {
		t.Fatalf("err = %v, want it to wrap %q", err, weird)
	}
	if calls != 1 {
		t.Fatalf("probe was called %d times, want exactly 1 (unclassified errors must not be retried)", calls)
	}
}

func TestRetryableErrorsAreRetriedUntilReady(t *testing.T) {
	calls := 0
	err := PollUntilReady(5, func(attempt int) error {
		calls++
		if attempt < 3 {
			return ErrRetryable
		}
		return nil
	})
	if err != nil {
		t.Fatalf("err = %v, want nil", err)
	}
	if calls != 3 {
		t.Fatalf("probe was called %d times, want 3", calls)
	}
}

func TestPermanentErrorStopsImmediately(t *testing.T) {
	calls := 0
	err := PollUntilReady(5, func(attempt int) error {
		calls++
		return ErrPermanentFailure
	})
	if !errors.Is(err, ErrPermanentFailure) {
		t.Fatalf("err = %v, want it to wrap ErrPermanentFailure", err)
	}
	if calls != 1 {
		t.Fatalf("probe was called %d times, want exactly 1", calls)
	}
}
```

Run: `go test -count=1 ./...`.

## Review

`PollUntilReady` is correct when every error `probe` can possibly return is
accounted for by exactly one arm of the `switch` — retryable, permanent,
or explicitly unclassified — and none of those three outcomes is left to
fall through by omission. The mistake this design avoids is treating "no
case matched" and "no default provided" as harmless, when in a `for` loop
that shape is indistinguishable from a deliberate retry: the loop simply
moves to its next iteration either way. A `default` that returns an error
immediately turns a silent classification gap into a specific, actionable
failure the very first time it happens, instead of a wasted retry budget
and a generic timeout message.

## Resources

- [Go Specification: Switch statements](https://go.dev/ref/spec#Switch_statements) — a tagless `switch { case cond: ... }` evaluates each case in order; if none matches and there is no `default`, execution falls through to the statement after the switch.
- [errors.Is](https://pkg.go.dev/errors#Is) — classifying wrapped errors against sentinel values without string comparison.

---

Back to [00-concepts.md](00-concepts.md) | Prev: [16-connection-pool-defer-before-validation.md](16-connection-pool-defer-before-validation.md) | Next: [18-batch-aggregator-continue-blocks-shutdown.md](18-batch-aggregator-continue-blocks-shutdown.md)
