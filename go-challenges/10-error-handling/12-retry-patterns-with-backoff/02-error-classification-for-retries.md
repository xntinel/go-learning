# Exercise 2: Deciding What Is Retryable — net.Error, url.Error, HTTP Status, and errors.As

The single most important line in any retry policy is the one that decides *whether
to retry at all*. Get it wrong in the permissive direction and you amplify bugs and
4xx errors; get it wrong in the strict direction and you fail on blips you could
have ridden out. This module builds a production `IsRetryable` classifier for a
real HTTP/DB client that unwraps timeouts, maps status codes, and honors sentinels.

This module is fully self-contained: its own `go mod init`, all types inline, its
own demo and tests.

## What you'll build

```text
retryclass/                independent module: example.com/retryclass
  go.mod                   go 1.26
  classify.go              StatusError, RetryableStatus, ErrPermanent,
                           IsRetryable classifier over wrapped errors
  cmd/
    demo/
      main.go              runnable demo: classifies a mix of errors
  classify_test.go         table tests over timeouts, statuses, sentinels
```

Files: `classify.go`, `cmd/demo/main.go`, `classify_test.go`.
Implement: `RetryableStatus(code int) bool`, a `StatusError` type carrying an HTTP status, and `IsRetryable(err error) bool` composing `errors.As` (net timeouts, status) and `errors.Is` (context, sentinels).
Test: a wrapped `*net.OpError` timeout (retryable), `context.Canceled`/`DeadlineExceeded` (never), each HTTP status class, a permanent sentinel, and a timeout wrapped three levels deep still detected.
Verify: `go test -count=1 -race ./...`

```bash
mkdir -p go-solutions/10-error-handling/12-retry-patterns-with-backoff/02-error-classification-for-retries/cmd/demo
cd go-solutions/10-error-handling/12-retry-patterns-with-backoff/02-error-classification-for-retries
go mod edit -go=1.26
```

### Classification is unwrapping, not type-switching

Real errors arrive wrapped. A database driver returns a `*net.OpError` that a
connection pool wraps in its own error that a repository wraps with
`fmt.Errorf("query users: %w", err)`. A naive classifier that type-asserts the top
error (`err.(net.Error)`) sees only the outermost `*fmt.wrapError` and misses the
timeout entirely. The correct tool is `errors.As`, which walks the `Unwrap` chain
looking for the *first* error assignable to your target — so a timeout buried three
wraps deep is still found. `errors.Is` does the same walk for sentinel matching.

The classifier composes several independent checks, ordered so the cheap,
definitive ones come first:

1. **`context.Canceled` / `context.DeadlineExceeded` are never retryable.** These
   mean the *caller* gave up, not the *server* failed. Retrying them is pointless
   (the deadline is already blown) and wrong (a cancelled caller does not want more
   work done). Check these first with `errors.Is`, because a timed-out request
   often *also* wraps a `net.Error` timeout, and the caller's intent wins.

2. **A caller-defined permanent sentinel is never retryable.** `errors.Is(err,
   ErrPermanent)` lets application code explicitly mark an error as "do not retry"
   regardless of what it wraps — an escape hatch for domain rules the generic
   classifier cannot know.

3. **A `net.Error` with `Timeout() == true` is retryable.** `errors.As(err,
   &netErr)` unwraps to the `net.Error` interface; a dial/read timeout is the
   canonical transient failure. Note we ask `Timeout()`, never the deprecated
   `Temporary()`.

4. **An HTTP `StatusError` is classified by its code.** `RetryableStatus` returns
   true for 429, 500, 502, 503, 504 and false for everything else — crucially false
   for 400, 401, 403, 404, and 409, which are permanent client-side or conflict
   errors that will fail identically on retry.

If none of these fire, the default is *not retryable*. An unknown error is treated
as permanent, because retrying an error you do not understand is how you amplify a
bug you have not diagnosed. That default is a deliberate safety choice.

`RetryableStatus` uses the `net/http` status constants rather than magic numbers —
`http.StatusTooManyRequests`, `http.StatusBadGateway`, and friends — so the intent
reads directly.

Create `classify.go`:

```go
package retryclass

import (
	"context"
	"errors"
	"net"
	"net/http"
)

// ErrPermanent marks an error the caller never wants retried, regardless of what
// it wraps. Wrap it: fmt.Errorf("validate: %w", ErrPermanent).
var ErrPermanent = errors.New("permanent error")

// StatusError carries an HTTP status code through the error chain so the retry
// classifier can inspect it with errors.As.
type StatusError struct {
	Code int
}

func (e *StatusError) Error() string {
	return http.StatusText(e.Code)
}

// RetryableStatus reports whether an HTTP status code represents a transient
// server-side condition worth retrying. 4xx client errors and 409 conflicts are
// never retryable; they will fail identically on retry.
func RetryableStatus(code int) bool {
	switch code {
	case http.StatusTooManyRequests, // 429
		http.StatusInternalServerError, // 500
		http.StatusBadGateway,          // 502
		http.StatusServiceUnavailable,  // 503
		http.StatusGatewayTimeout:      // 504
		return true
	default:
		return false
	}
}

// IsRetryable classifies an error, unwrapping it with errors.As/errors.Is. The
// default for an unrecognized error is false: an error we do not understand is
// treated as permanent rather than amplified by retrying.
func IsRetryable(err error) bool {
	if err == nil {
		return false
	}
	// Caller intent wins: a cancelled or expired context is never retryable.
	if errors.Is(err, context.Canceled) || errors.Is(err, context.DeadlineExceeded) {
		return false
	}
	// Explicit application marker.
	if errors.Is(err, ErrPermanent) {
		return false
	}
	// Network timeout anywhere in the chain.
	var netErr net.Error
	if errors.As(err, &netErr) && netErr.Timeout() {
		return true
	}
	// HTTP status carried by a StatusError anywhere in the chain.
	var statusErr *StatusError
	if errors.As(err, &statusErr) {
		return RetryableStatus(statusErr.Code)
	}
	return false
}
```

### The runnable demo

The demo classifies one error of each important shape and prints the verdict, so
you can see the table of decisions at a glance.

Create `cmd/demo/main.go`:

```go
package main

import (
	"context"
	"fmt"
	"net"

	"example.com/retryclass"
)

func main() {
	timeout := &net.OpError{Op: "dial", Net: "tcp", Err: timeoutError{}}
	cases := []struct {
		name string
		err  error
	}{
		{"dial timeout", fmt.Errorf("connect db: %w", timeout)},
		{"429 too many requests", &retryclass.StatusError{Code: 429}},
		{"503 service unavailable", &retryclass.StatusError{Code: 503}},
		{"400 bad request", &retryclass.StatusError{Code: 400}},
		{"404 not found", &retryclass.StatusError{Code: 404}},
		{"context canceled", fmt.Errorf("op: %w", context.Canceled)},
		{"permanent sentinel", fmt.Errorf("bad input: %w", retryclass.ErrPermanent)},
	}
	for _, c := range cases {
		fmt.Printf("%-24s retryable=%v\n", c.name, retryclass.IsRetryable(c.err))
	}
}

// timeoutError is a minimal net.Error whose Timeout reports true.
type timeoutError struct{}

func (timeoutError) Error() string   { return "i/o timeout" }
func (timeoutError) Timeout() bool   { return true }
func (timeoutError) Temporary() bool { return false }
```

Run it:

```bash
go run ./cmd/demo
```

Expected output:

```
dial timeout             retryable=true
429 too many requests    retryable=true
503 service unavailable  retryable=true
400 bad request          retryable=false
404 not found            retryable=false
context canceled         retryable=false
permanent sentinel       retryable=false
```

### Tests

The table feeds one error of each shape and asserts the boolean. Two cases carry
the design's sharpest claims: a `*net.OpError` timeout wrapped *three levels deep*
must still be detected (proving `errors.As` walks the chain), and a 400 must never
be retryable even though it is an error (proving the classifier does not fall back
to "any error is retryable").

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

// fakeTimeout is a net.Error reporting a timeout, for building test chains.
type fakeTimeout struct{}

func (fakeTimeout) Error() string   { return "i/o timeout" }
func (fakeTimeout) Timeout() bool   { return true }
func (fakeTimeout) Temporary() bool { return false }

func TestIsRetryable(t *testing.T) {
	t.Parallel()

	opTimeout := &net.OpError{Op: "read", Net: "tcp", Err: fakeTimeout{}}
	deepTimeout := fmt.Errorf("repo: %w", fmt.Errorf("pool: %w", fmt.Errorf("conn: %w", opTimeout)))

	cases := []struct {
		name string
		err  error
		want bool
	}{
		{"nil", nil, false},
		{"wrapped net timeout", fmt.Errorf("dial: %w", opTimeout), true},
		{"timeout three wraps deep", deepTimeout, true},
		{"429", &StatusError{Code: 429}, true},
		{"500", &StatusError{Code: 500}, true},
		{"502", &StatusError{Code: 502}, true},
		{"503", &StatusError{Code: 503}, true},
		{"504", &StatusError{Code: 504}, true},
		{"400 never retryable", &StatusError{Code: 400}, false},
		{"401 never retryable", &StatusError{Code: 401}, false},
		{"403 never retryable", &StatusError{Code: 403}, false},
		{"404 never retryable", &StatusError{Code: 404}, false},
		{"409 conflict never retryable", &StatusError{Code: 409}, false},
		{"context canceled", fmt.Errorf("op: %w", context.Canceled), false},
		{"deadline exceeded", fmt.Errorf("op: %w", context.DeadlineExceeded), false},
		{"permanent sentinel", fmt.Errorf("x: %w", ErrPermanent), false},
		{"unknown error defaults false", errors.New("mystery"), false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			if got := IsRetryable(tc.err); got != tc.want {
				t.Errorf("IsRetryable(%v) = %v, want %v", tc.err, got, tc.want)
			}
		})
	}
}

func TestRetryableStatus(t *testing.T) {
	t.Parallel()
	retryable := []int{429, 500, 502, 503, 504}
	permanent := []int{200, 201, 301, 400, 401, 403, 404, 409, 422}
	for _, c := range retryable {
		if !RetryableStatus(c) {
			t.Errorf("RetryableStatus(%d) = false, want true", c)
		}
	}
	for _, c := range permanent {
		if RetryableStatus(c) {
			t.Errorf("RetryableStatus(%d) = true, want false", c)
		}
	}
}

func ExampleIsRetryable() {
	fmt.Println(IsRetryable(&StatusError{Code: 503}))
	fmt.Println(IsRetryable(&StatusError{Code: 404}))
	fmt.Println(IsRetryable(fmt.Errorf("x: %w", context.Canceled)))
	// Output:
	// true
	// false
	// false
}
```

## Review

The classifier is correct when every decision is a deliberate rule and the default
is "do not retry". The two properties to confirm by hand: a timeout survives deep
wrapping because `errors.As` walks the chain (not a top-level type assertion), and a
context error outranks a coincident network timeout because caller intent is checked
first. The mistakes this design forecloses: using `net.Error.Temporary()` (we ask
only `Timeout()`), and treating "any non-nil error" as retryable (an unknown error
returns false). Run `go test -race`; the classifier is pure and read-only, so the
whole table can run in parallel.

## Resources

- [`errors#As`](https://pkg.go.dev/errors#As) — walks the `Unwrap` chain to find a target type.
- [`net#Error`](https://pkg.go.dev/net#Error) — `Timeout()` and the deprecated `Temporary()`.
- [`net/http` status constants](https://pkg.go.dev/net/http#pkg-constants) — `StatusTooManyRequests`, `StatusServiceUnavailable`, etc.
- [`context`](https://pkg.go.dev/context) — `Canceled` and `DeadlineExceeded` sentinels.

---

Back to [00-concepts.md](00-concepts.md) | Previous: [01-backoff-retry-client.md](01-backoff-retry-client.md) | Next: [03-jitter-strategies.md](03-jitter-strategies.md)
