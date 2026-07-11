# Exercise 3: Generic Function Decorators

A decorator does not need an interface or a struct; a function that takes a function and returns a function of the same signature is a decorator too. This exercise uses generics to write two reusable function decorators — `WithRetry` and `WithTimeout` — that wrap any `func(context.Context) (T, error)` for any `T`, and composes them so a retried operation is bounded by a per-attempt timeout.

This module is fully self-contained. It begins with its own `go mod init`, defines every type it needs, and ships its own demo and tests. Nothing here imports any other exercise.

## What you'll build

```text
decorator.go         Op[T], ErrTransient, IsRetryable,
                     WithRetry[T], WithTimeout[T]
cmd/
  demo/
    main.go          retry a flaky op to success; time out a slow op
decorator_test.go    retry success-after-transient, stop-on-permanent,
                     exhaustion wrap, timeout fires, fast pass-through,
                     retry-around-timeout composition
```

- Files: `decorator.go`, `cmd/demo/main.go`, `decorator_test.go`.
- Implement: `Op[T]` as `func(context.Context) (T, error)`, plus `WithRetry[T]` and `WithTimeout[T]` that each take an `Op[T]` and return an `Op[T]`.
- Test: retry succeeds after transient failures and stops on permanent ones, wraps the last error on exhaustion; timeout fires on a slow op and passes a fast result through; the composition retries each attempt under its own timeout.
- Verify: `go test -race ./...`

Set up the module:

```bash
mkdir -p func-decorators/cmd/demo && cd func-decorators
go mod init example.com/func-decorators
```

### One decorator for every return type

The function-decorator version of the pattern keeps the signature instead of the interface. Define `type Op[T any] func(context.Context) (T, error)` — a cancellable operation returning a value of any type — and a decorator is any `func(Op[T]) Op[T]`: it takes an operation and returns one of the same shape with behavior added around the call. Because the input and output types match, decorators compose by nesting exactly as the interface and HTTP versions did, and because `T` is a type parameter, one `WithRetry` works for `Op[int]`, `Op[string]`, `Op[*User]`, and everything else, with no `interface{}` and no per-type duplication. Generics are what make the function decorator general; before Go 1.18 you either wrote one wrapper per type or paid for runtime type assertions.

The zero value matters in generic code. Every early return that has no real value to give must return `var zero T` — the type parameter's zero value — because you cannot write a literal that is valid for all `T`. Both decorators declare a `zero` and return it on their error paths.

### Retry: classify, then loop

`WithRetry` loops up to `maxAttempts`, returning immediately on success and on any error the `retryable` predicate rejects, so permanent failures propagate on the first attempt instead of being hammered. It treats a nil predicate as `IsRetryable` and clamps `maxAttempts` to at least one, so a caller cannot accidentally configure an operation that never runs. Between attempts it checks `ctx.Err()`: if the context was cancelled or its deadline passed, it stops and returns the context error rather than spending the remaining budget on attempts that cannot succeed. When the budget is exhausted it wraps the last error with `%w`, so a caller can still recover the underlying cause with `errors.Is`.

The predicate is the seam. `IsRetryable` here treats `ErrTransient` and `context.DeadlineExceeded` as retryable — the latter is what makes the composition with `WithTimeout` work, because a single attempt that times out should trigger another attempt rather than fail the whole operation. In real code the predicate would classify network resets, 503s, and lock-contention errors; the sentinel keeps that classification visible and testable.

### Timeout: race the work against the clock, without leaking

`WithTimeout` derives a child context with `context.WithTimeout`, runs the operation in a goroutine, and selects between the context's `Done` channel and the operation's result. The detail that separates a correct implementation from a leaky one is the buffered channel. The result channel has capacity one, so when the timeout branch wins the select and the decorator returns, the goroutine running the operation can still send its eventual result into the buffer and exit, instead of blocking forever on a send no one will receive. An unbuffered channel here would leak one goroutine on every timeout. The wrapped operation must also select on `ctx.Done()` so the timeout actually stops the work; the decorator cancels the context, but only the operation can honor that cancellation. `defer cancel()` releases the timer in every exit path.

Create `decorator.go`:

```go
package fn

import (
	"context"
	"errors"
	"fmt"
	"time"
)

// Op is a cancellable operation that returns a value of any type T or an error.
// Decorators below take an Op and return an Op of the same type, so they compose.
type Op[T any] func(context.Context) (T, error)

// ErrTransient marks a failure worth retrying. Real code would classify network
// timeouts, 503s, and lock contention; here a sentinel keeps the seam visible.
var ErrTransient = errors.New("fn: transient error")

// IsRetryable is the default retry predicate: transient failures and deadline
// timeouts are worth another attempt; everything else is permanent.
func IsRetryable(err error) bool {
	return errors.Is(err, ErrTransient) || errors.Is(err, context.DeadlineExceeded)
}

// WithRetry returns an Op that calls op up to maxAttempts times, retrying only
// errors for which retryable returns true. It stops early if the context is
// cancelled, and wraps the last error with %w when attempts are exhausted.
func WithRetry[T any](op Op[T], maxAttempts int, retryable func(error) bool) Op[T] {
	if maxAttempts < 1 {
		maxAttempts = 1
	}
	if retryable == nil {
		retryable = IsRetryable
	}
	return func(ctx context.Context) (T, error) {
		var zero T
		var lastErr error
		for attempt := 1; attempt <= maxAttempts; attempt++ {
			v, err := op(ctx)
			if err == nil {
				return v, nil
			}
			if !retryable(err) {
				return zero, err
			}
			lastErr = err
			if ctx.Err() != nil {
				return zero, ctx.Err()
			}
		}
		return zero, fmt.Errorf("fn: exhausted %d attempts: %w", maxAttempts, lastErr)
	}
}

// WithTimeout returns an Op that bounds op to d. It runs op in a goroutine and
// races its result against the derived context's deadline. The result channel
// is buffered so the goroutine can always send and exit, even after a timeout
// has already returned, which is what prevents a goroutine leak.
func WithTimeout[T any](op Op[T], d time.Duration) Op[T] {
	return func(ctx context.Context) (T, error) {
		ctx, cancel := context.WithTimeout(ctx, d)
		defer cancel()

		type result struct {
			v   T
			err error
		}
		ch := make(chan result, 1)
		go func() {
			v, err := op(ctx)
			ch <- result{v, err}
		}()

		select {
		case <-ctx.Done():
			var zero T
			return zero, ctx.Err()
		case res := <-ch:
			return res.v, res.err
		}
	}
}
```

### The runnable demo

The demo shows each decorator alone. A flaky operation fails transiently twice and then returns 7; wrapping it in `WithRetry` with a budget of five drives it to success. A slow operation that respects cancellation is wrapped in a 20 ms `WithTimeout` and a 1 s body, so the timeout fires and the result is `context.DeadlineExceeded`.

Create `cmd/demo/main.go`:

```go
package main

import (
	"context"
	"errors"
	"fmt"
	"time"

	"example.com/func-decorators"
)

// flaky returns an Op that fails transiently the first `failures` times, then
// returns 7. The closure is called sequentially by WithRetry, so a plain int
// counter is safe here.
func flaky(failures int) fn.Op[int] {
	var n int
	return func(context.Context) (int, error) {
		if n < failures {
			n++
			return 0, fn.ErrTransient
		}
		return 7, nil
	}
}

func main() {
	ctx := context.Background()

	retried := fn.WithRetry(flaky(2), 5, fn.IsRetryable)
	v, err := retried(ctx)
	fmt.Printf("retry result: v=%d err=%v\n", v, err)

	slow := func(ctx context.Context) (string, error) {
		select {
		case <-time.After(time.Second):
			return "done", nil
		case <-ctx.Done():
			return "", ctx.Err()
		}
	}
	guarded := fn.WithTimeout(fn.Op[string](slow), 20*time.Millisecond)
	_, err = guarded(ctx)
	fmt.Printf("timeout fired: %v\n", errors.Is(err, context.DeadlineExceeded))
}
```

Run it:

```bash
go run ./cmd/demo
```

Expected output:

```
retry result: v=7 err=<nil>
timeout fired: true
```

### Tests

The retry tests pin the three behaviors: success after transient failures with the call count proving the loop ran the right number of times, an immediate stop on a permanent error, and a wrapped error on exhaustion. The timeout tests pin that a slow op yields `DeadlineExceeded` and a fast op passes its value through. The composition test is the payoff: `WithTimeout` inside `WithRetry` means each attempt gets its own deadline, so an operation that blocks past the timeout on its first attempt and returns fast on the second is retried to success — and the counter is an `atomic.Int64` because the first attempt's goroutine may still be unwinding when the next attempt starts.

Create `decorator_test.go`:

```go
package fn

import (
	"context"
	"errors"
	"sync/atomic"
	"testing"
	"time"
)

func TestWithRetry_SucceedsAfterTransient(t *testing.T) {
	t.Parallel()

	var calls int
	op := func(context.Context) (int, error) {
		calls++
		if calls < 3 {
			return 0, ErrTransient
		}
		return 42, nil
	}

	got, err := WithRetry(Op[int](op), 5, IsRetryable)(t.Context())
	if err != nil {
		t.Fatalf("WithRetry: %v", err)
	}
	if got != 42 {
		t.Errorf("value = %d, want 42", got)
	}
	if calls != 3 {
		t.Errorf("calls = %d, want 3", calls)
	}
}

func TestWithRetry_StopsOnPermanent(t *testing.T) {
	t.Parallel()

	permanent := errors.New("validation failed")
	var calls int
	op := func(context.Context) (int, error) {
		calls++
		return 0, permanent
	}

	_, err := WithRetry(Op[int](op), 5, IsRetryable)(t.Context())
	if !errors.Is(err, permanent) {
		t.Errorf("err = %v, want permanent", err)
	}
	if calls != 1 {
		t.Errorf("calls = %d, want 1 (no retry on permanent error)", calls)
	}
}

func TestWithRetry_ExhaustsAndWraps(t *testing.T) {
	t.Parallel()

	op := func(context.Context) (string, error) {
		return "", ErrTransient
	}

	_, err := WithRetry(Op[string](op), 3, IsRetryable)(t.Context())
	if !errors.Is(err, ErrTransient) {
		t.Errorf("err = %v, want wrap of ErrTransient", err)
	}
}

func TestWithTimeout_FiresOnSlowOp(t *testing.T) {
	t.Parallel()

	op := func(ctx context.Context) (int, error) {
		select {
		case <-time.After(time.Second):
			return 1, nil
		case <-ctx.Done():
			return 0, ctx.Err()
		}
	}

	_, err := WithTimeout(Op[int](op), 10*time.Millisecond)(t.Context())
	if !errors.Is(err, context.DeadlineExceeded) {
		t.Errorf("err = %v, want DeadlineExceeded", err)
	}
}

func TestWithTimeout_PassesFastResult(t *testing.T) {
	t.Parallel()

	op := func(context.Context) (string, error) {
		return "quick", nil
	}

	got, err := WithTimeout(Op[string](op), time.Second)(t.Context())
	if err != nil {
		t.Fatalf("WithTimeout: %v", err)
	}
	if got != "quick" {
		t.Errorf("value = %q, want quick", got)
	}
}

func TestComposed_RetryAroundTimeout(t *testing.T) {
	t.Parallel()

	var calls atomic.Int64
	op := func(ctx context.Context) (int, error) {
		// First attempt blocks past its timeout; later attempts return fast.
		if calls.Add(1) == 1 {
			select {
			case <-time.After(time.Second):
				return 0, nil
			case <-ctx.Done():
				return 0, ctx.Err()
			}
		}
		return 7, nil
	}

	guarded := WithTimeout(Op[int](op), 10*time.Millisecond)
	got, err := WithRetry(guarded, 3, IsRetryable)(t.Context())
	if err != nil {
		t.Fatalf("composed: %v", err)
	}
	if got != 7 {
		t.Errorf("value = %d, want 7", got)
	}
	if n := calls.Load(); n != 2 {
		t.Errorf("calls = %d, want 2 (timeout then success)", n)
	}
}
```

## Review

The decorators are correct when both keep the `Op[T]` signature exactly, so they compose in either order. Confirm every error path returns `var zero T` rather than a typed literal, because no literal is valid for all `T`. Confirm `WithRetry` retries only what its predicate accepts: `TestWithRetry_StopsOnPermanent` pins the call count at one for a non-retryable error, which a blanket `if err != nil { retry }` would fail. Confirm the exhaustion path wraps with `%w` so `errors.Is` still finds the cause. For `WithTimeout`, the load-bearing detail is the channel buffer of one: with an unbuffered channel, every timeout would leak the operation's goroutine, and while the leak does not fail a short test, it is the kind of defect `go test -race` and production memory graphs eventually surface.

The composition test encodes the reason these are worth writing as separate decorators. `WithTimeout` inside `WithRetry` gives each attempt a fresh deadline, so a transient slow response is retried rather than fatal; `WithRetry` inside `WithTimeout` would instead bound the entire retry loop with a single deadline. Both are legitimate, and which you want is a composition-order decision exactly like the HTTP chain's — the decorators do not change, only the nesting does. The `atomic.Int64` counter in that test is not incidental: because the timeout decorator returns as soon as the deadline fires, the first attempt's goroutine can still be running when the retry loop starts the second attempt, so the two attempts may touch the counter without a happens-before edge, and a plain `int` would be a data race the detector flags.

## Resources

- [Introduction to generics](https://go.dev/blog/intro-generics) — type parameters and type inference, what makes one decorator work for every `T`.
- [`context.WithTimeout`](https://pkg.go.dev/context#WithTimeout) — deriving a deadline-bounded child context and why `cancel` must always be called.
- [Working with Errors in Go 1.13](https://go.dev/blog/go1.13-errors) — `errors.Is` and `%w`, used by the retry predicate and the exhaustion wrap.
- [Go Concurrency Patterns: Pipelines and cancellation](https://go.dev/blog/pipelines) — buffered channels and avoiding goroutine leaks when a consumer stops early.

---

Back to [00-concepts.md](00-concepts.md) | Previous: [02-http-middleware-chain.md](02-http-middleware-chain.md) | Next: [04-production-api-gateway-stack.md](04-production-api-gateway-stack.md)
