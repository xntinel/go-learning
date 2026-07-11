# Exercise 5: Retry Classifier: Deciding Retryable vs Terminal Errors

The single most dangerous decision a client makes is whether to retry. Retry a
terminal 4xx and you waste calls; retry a canceled context and you ignore your
caller; fail to retry a transient timeout and you drop a request that would have
succeeded. This module builds the classify-then-decide core: `classify(err) Decision`
plus a `doWithRetry` driver that early-returns on success and terminal errors and
backs off on retryable ones.

This module is fully self-contained: its own `go mod init`, all code inline, its own
demo and tests.

## What you'll build

```text
retryclass/                 independent module: example.com/retryclass
  go.mod                    go 1.26
  retry.go                  Decision, classify(err), doWithRetry(ctx, op)
  cmd/
    demo/
      main.go               classify a few errors; drive an op that fails then succeeds
  retry_test.go             table-driven classify; driver attempt-count assertions
```

- Files: `retry.go`, `cmd/demo/main.go`, `retry_test.go`.
- Implement: `classify(err error) Decision` using `errors.As` for `net.Error.Timeout()`, `errors.Is` for `context.DeadlineExceeded`/`Canceled`, and sentinels for retryable/terminal app errors; `doWithRetry(ctx, op)` that retries with backoff.
- Test: classify each error class; driver succeeds after N-1 retryable failures (assert attempt count), returns immediately on a terminal error, and stops when the context is canceled.
- Verify: `go test -count=1 -race ./...`

Set up the module:

```bash
mkdir -p ~/go-exercises/retryclass/cmd/demo
cd ~/go-exercises/retryclass
go mod init example.com/retryclass
```

## Classify by type and sentinel, never by string or Temporary()

`classify` returns one of three decisions: `Retry`, `Terminal`, or (for a `nil`
error) `Success`. The order of its checks encodes the policy, and each check uses the
right tool:

- A canceled or deadline-exceeded *context* is terminal. `errors.Is(err, context.Canceled)`
  and `errors.Is(err, context.DeadlineExceeded)` catch these even when wrapped. This
  check comes first: once your caller has given up, retrying is wrong regardless of
  what else the error looks like.
- A network `Timeout()` is retryable. `errors.As(err, &netErr)` recovers the
  `net.Error` interface, and `netErr.Timeout()` reports a transient timeout. Do not
  use the deprecated `Temporary()` — it is ill-defined and removed from good code.
- Application error classes are explicit sentinels: `ErrServerBusy` (a 5xx-shaped
  condition) is retryable; `ErrBadRequest` (a 4xx-shaped condition) is terminal.
- Anything unrecognized is terminal by default. Retrying an unknown error risks a
  storm against a dependency that is failing for a reason you do not understand;
  conservative is correct.

The driver `doWithRetry` is the guard-clause counterpart. Each attempt runs the op
in an init-statement `if`: `if err := op(ctx); err == nil { return nil }`. On success
it returns; on a terminal classification it returns the error immediately (no point
retrying); otherwise it backs off and loops, up to a max number of attempts. Between
attempts it waits on either the backoff timer or `ctx.Done()`, so a canceled context
stops the loop promptly rather than sleeping out the backoff.

Create `retry.go`:

```go
package retryclass

import (
	"context"
	"errors"
	"net"
	"time"
)

// Decision is the outcome of classifying an error.
type Decision int

const (
	Success Decision = iota
	Retry
	Terminal
)

func (d Decision) String() string {
	switch d {
	case Success:
		return "success"
	case Retry:
		return "retry"
	default:
		return "terminal"
	}
}

// Application error classes.
var (
	ErrServerBusy = errors.New("server busy") // 5xx-shaped: retryable
	ErrBadRequest = errors.New("bad request") // 4xx-shaped: terminal
)

// classify decides whether err warrants a retry.
func classify(err error) Decision {
	if err == nil {
		return Success
	}
	// A caller who cancelled or timed out must not be retried against.
	if errors.Is(err, context.Canceled) || errors.Is(err, context.DeadlineExceeded) {
		return Terminal
	}
	// A transient network timeout is retryable. Temporary() is deprecated; use
	// Timeout().
	var netErr net.Error
	if errors.As(err, &netErr) && netErr.Timeout() {
		return Retry
	}
	if errors.Is(err, ErrServerBusy) {
		return Retry
	}
	if errors.Is(err, ErrBadRequest) {
		return Terminal
	}
	return Terminal // conservative default: do not storm an unknown failure
}

// doWithRetry runs op until it succeeds, hits a terminal error, exhausts attempts,
// or the context is done. backoff is the base delay between attempts.
func doWithRetry(ctx context.Context, attempts int, backoff time.Duration, op func(context.Context) error) error {
	var last error
	for attempt := range attempts {
		if err := op(ctx); err == nil {
			return nil
		} else {
			last = err
			if classify(err) == Terminal {
				return err
			}
		}
		if attempt == attempts-1 {
			break // no wait after the final attempt
		}
		timer := time.NewTimer(backoff)
		select {
		case <-ctx.Done():
			timer.Stop()
			return ctx.Err()
		case <-timer.C:
		}
	}
	return last
}
```

### The runnable demo

The demo classifies a handful of errors and then drives an operation that fails
twice with `ErrServerBusy` before succeeding, printing the attempt count.

Create `cmd/demo/main.go`:

```go
package main

import (
	"context"
	"fmt"
	"time"

	"example.com/retryclass"
)

func main() {
	fmt.Println("busy   ->", retryclass.ClassifyForDemo(retryclass.ErrServerBusy))
	fmt.Println("bad    ->", retryclass.ClassifyForDemo(retryclass.ErrBadRequest))
	fmt.Println("cancel ->", retryclass.ClassifyForDemo(context.Canceled))

	attempts := 0
	op := func(context.Context) error {
		attempts++
		if attempts < 3 {
			return retryclass.ErrServerBusy
		}
		return nil
	}
	err := retryclass.DoWithRetryForDemo(context.Background(), 5, time.Millisecond, op)
	fmt.Printf("succeeded after %d attempts, err=%v\n", attempts, err)
}
```

`classify` and `doWithRetry` are unexported (they are internal policy), so the demo
reaches them through thin exported wrappers. Add these to `retry.go`:

Append to `retry.go`:

```go
// ClassifyForDemo exposes classify for the runnable demo.
func ClassifyForDemo(err error) Decision { return classify(err) }

// DoWithRetryForDemo exposes doWithRetry for the runnable demo.
func DoWithRetryForDemo(ctx context.Context, attempts int, backoff time.Duration, op func(context.Context) error) error {
	return doWithRetry(ctx, attempts, backoff, op)
}
```

Run it:

```bash
go run ./cmd/demo
```

Expected output:

```
busy   -> retry
bad    -> terminal
cancel -> terminal
succeeded after 3 attempts, err=<nil>
```

### Tests

The classify table feeds one error of each class, including a fake `net.Error` with
`Timeout() == true`, a wrapped `context.DeadlineExceeded`, and an unknown error. The
driver tests assert the attempt count: succeed after N-1 retryable failures, return
immediately on a terminal error (one attempt), and stop when the context cancels.

Create `retry_test.go`:

```go
package retryclass

import (
	"context"
	"errors"
	"fmt"
	"testing"
	"time"
)

// timeoutErr is a fake net.Error reporting a transient timeout.
type timeoutErr struct{}

func (timeoutErr) Error() string   { return "i/o timeout" }
func (timeoutErr) Timeout() bool   { return true }
func (timeoutErr) Temporary() bool { return false }

func TestClassify(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name string
		err  error
		want Decision
	}{
		{"nil is success", nil, Success},
		{"net timeout retries", timeoutErr{}, Retry},
		{"wrapped deadline terminal", fmt.Errorf("call: %w", context.DeadlineExceeded), Terminal},
		{"cancel terminal", context.Canceled, Terminal},
		{"server busy retries", fmt.Errorf("upstream: %w", ErrServerBusy), Retry},
		{"bad request terminal", ErrBadRequest, Terminal},
		{"unknown terminal", errors.New("mystery"), Terminal},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			if got := classify(tc.err); got != tc.want {
				t.Fatalf("classify(%v) = %v, want %v", tc.err, got, tc.want)
			}
		})
	}
}

func TestDoWithRetrySucceedsAfterFailures(t *testing.T) {
	t.Parallel()
	attempts := 0
	op := func(context.Context) error {
		attempts++
		if attempts < 3 {
			return ErrServerBusy
		}
		return nil
	}
	if err := doWithRetry(t.Context(), 5, time.Millisecond, op); err != nil {
		t.Fatalf("doWithRetry() = %v, want nil", err)
	}
	if attempts != 3 {
		t.Fatalf("attempts = %d, want 3", attempts)
	}
}

func TestDoWithRetryStopsOnTerminal(t *testing.T) {
	t.Parallel()
	attempts := 0
	op := func(context.Context) error {
		attempts++
		return ErrBadRequest
	}
	err := doWithRetry(t.Context(), 5, time.Millisecond, op)
	if !errors.Is(err, ErrBadRequest) {
		t.Fatalf("err = %v, want ErrBadRequest", err)
	}
	if attempts != 1 {
		t.Fatalf("attempts = %d, want 1 (terminal, no retry)", attempts)
	}
}

func TestDoWithRetryStopsOnCancel(t *testing.T) {
	t.Parallel()
	ctx, cancel := context.WithCancel(t.Context())
	attempts := 0
	op := func(context.Context) error {
		attempts++
		cancel() // caller gives up during the first attempt
		return ErrServerBusy
	}
	err := doWithRetry(ctx, 5, time.Second, op)
	if !errors.Is(err, context.Canceled) {
		t.Fatalf("err = %v, want context.Canceled", err)
	}
	if attempts != 1 {
		t.Fatalf("attempts = %d, want 1 (cancel stops the loop)", attempts)
	}
}
```

## Review

The classifier is correct when a canceled or deadline-exceeded context is terminal
regardless of what else wraps it, a `net.Error.Timeout()` is retryable, application
classes map through explicit sentinels, and anything unknown defaults to terminal.
The driver is correct when a terminal error costs exactly one attempt, a retryable
sequence stops the moment it succeeds, and a canceled context breaks the loop instead
of sleeping out the backoff. The mistakes to avoid are `strings.Contains` on the
message, `net.Error.Temporary()` (deprecated), retrying an unknown error by default,
and continuing to retry after the caller's context is done.

## Resources

- [net.Error interface (Timeout; Temporary deprecated)](https://pkg.go.dev/net#Error)
- [context.Canceled and DeadlineExceeded](https://pkg.go.dev/context#pkg-variables)
- [errors.As and errors.Is](https://pkg.go.dev/errors#As)
- [Google SRE: Addressing cascading failures (retries)](https://sre.google/sre-book/addressing-cascading-failures/)

---

Back to [00-concepts.md](00-concepts.md) | Previous: [04-cache-comma-ok-ttl.md](04-cache-comma-ok-ttl.md) | Next: [06-idempotency-guard.md](06-idempotency-guard.md)
