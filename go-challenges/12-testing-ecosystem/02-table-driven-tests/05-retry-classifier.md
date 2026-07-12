# Exercise 5: Classifying Errors as Retryable or Terminal

Whether a client hammers a struggling downstream or gives up gracefully comes down
to one predicate: is this error worth retrying. It is the highest-stakes boolean in
a resilient backend, and it is exactly the kind of policy a table documents well.
This module builds `IsRetryable(err) bool`, exercising both `errors.Is` (for
sentinels like `context.Canceled`) and `errors.As` (to extract a `net.Error` or a
typed `*HTTPError` and read its fields).

This module is fully self-contained: its own `go mod init`, its own demo, its own
tests. Nothing here imports another exercise.

## What you'll build

```text
retryclass/               independent module: example.com/retryclass
  go.mod                  go 1.26
  retry.go                HTTPError type + IsRetryable(err) bool
  cmd/
    demo/
      main.go             classifies a few errors, prints retry/terminal
  retry_test.go           table over {name,err,wantRetry}, Is and As cases
```

- Files: `retry.go`, `cmd/demo/main.go`, `retry_test.go`.
- Implement: `IsRetryable(err) bool` — retryable on network timeouts (`net.Error.Timeout()`), HTTP 5xx (`*HTTPError`), and `io.ErrUnexpectedEOF`; terminal on `context.Canceled`, `context.DeadlineExceeded`, HTTP 4xx, and anything unknown.
- Test: a table of `{name, err, wantRetry}` that reaches sentinels with `errors.Is` and typed errors with `errors.As`, including a `context.Canceled` row that must be terminal despite looking transient.
- Verify: `go test -count=1 -race ./...`

### Why order matters, and why Is and As both appear

This classifier is the clearest case in the lesson of *assert the weakest
sufficient property*, and of ordering being part of correctness. Two of the checks
need `errors.As`, not `errors.Is`, because the decision depends on a *field*, not
an identity: a `net.Error` is retryable only when `Timeout()` is true, and an
`*HTTPError` is retryable only when its `StatusCode` is 5xx. You cannot answer
those with `errors.Is`; you must extract the concrete or interface value and
inspect it. The other checks — `context.Canceled`, `context.DeadlineExceeded`,
`io.ErrUnexpectedEOF` — are pure identity, so `errors.Is` is right.

The ordering trap is real and the table pins it. `context.DeadlineExceeded`
*implements `net.Error`* and its `Timeout()` returns true — so if the `net.Error`
check ran first, a blown deadline would be misclassified as a retryable timeout,
and the client would hammer a downstream after the caller already gave up. The fix
is to check the context sentinels *before* the `net.Error` branch, so a
caller-driven cancellation or deadline is terminal regardless of what interface it
happens to satisfy. The `canceled` and `deadline_exceeded` rows in the table are
what keep that ordering honest; reorder the branches and they fail.

The policy itself encodes hard-won operational judgment: a network *timeout* is
transient (the peer may recover, retry it), but *cancellation* is the caller's
decision (never retry it); a 5xx says the server failed and may succeed on retry,
but a 4xx says the request itself is wrong and retrying is pointless; a truncated
read (`io.ErrUnexpectedEOF`) is usually a transient connection drop mid-stream.

Create `retry.go`:

```go
package retryclass

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net"
)

// HTTPError carries a status code from a downstream HTTP call.
type HTTPError struct {
	StatusCode int
}

func (e *HTTPError) Error() string {
	return fmt.Sprintf("http status %d", e.StatusCode)
}

// IsRetryable reports whether err is worth retrying with backoff. Caller-driven
// cancellation and deadlines are terminal even though they satisfy net.Error, so
// they are checked before the network-timeout branch.
func IsRetryable(err error) bool {
	if err == nil {
		return false
	}

	// Terminal: the caller gave up. Checked first because DeadlineExceeded also
	// satisfies net.Error with Timeout() == true.
	if errors.Is(err, context.Canceled) || errors.Is(err, context.DeadlineExceeded) {
		return false
	}

	// Transient: a network timeout may succeed on retry.
	var netErr net.Error
	if errors.As(err, &netErr) && netErr.Timeout() {
		return true
	}

	// HTTP: 5xx is the server's fault and may recover; 4xx is the request's fault.
	var httpErr *HTTPError
	if errors.As(err, &httpErr) {
		return httpErr.StatusCode >= 500
	}

	// A stream truncated mid-read is usually a transient connection drop.
	if errors.Is(err, io.ErrUnexpectedEOF) {
		return true
	}

	return false
}
```

### The runnable demo

The demo classifies one error of each kind, including a wrapped 503 and a wrapped
`context.Canceled`, and prints the verdict.

Create `cmd/demo/main.go`:

```go
package main

import (
	"context"
	"errors"
	"fmt"
	"io"

	"example.com/retryclass"
)

func main() {
	errs := []error{
		fmt.Errorf("call: %w", &retryclass.HTTPError{StatusCode: 503}),
		&retryclass.HTTPError{StatusCode: 404},
		context.Canceled,
		fmt.Errorf("read body: %w", io.ErrUnexpectedEOF),
		errors.New("malformed response"),
	}
	for _, err := range errs {
		verdict := "terminal"
		if retryclass.IsRetryable(err) {
			verdict = "retry"
		}
		fmt.Printf("%-8s <- %v\n", verdict, err)
	}
}
```

Run it:

```bash
go run ./cmd/demo
```

Expected output:

```
retry    <- call: http status 503
terminal <- http status 404
terminal <- context canceled
retry    <- read body: unexpected EOF
terminal <- malformed response
```

### The tests

The table mixes `errors.Is` targets (context sentinels, `io.ErrUnexpectedEOF`)
with `errors.As` targets (a fake `net.Error`, a `*HTTPError`). The `fakeNetErr`
helper implements the `net.Error` interface so the timeout branch can be tested
without a real socket. The `deadline_exceeded` row is the ordering guard: it
satisfies `net.Error` with `Timeout() == true` yet must classify terminal because
the context check runs first.

Create `retry_test.go`:

```go
package retryclass

import (
	"context"
	"errors"
	"fmt"
	"io"
	"testing"
)

// fakeNetErr implements net.Error so timeout classification can be tested
// without a real network connection.
type fakeNetErr struct {
	timeout bool
}

func (e fakeNetErr) Error() string   { return "fake network error" }
func (e fakeNetErr) Timeout() bool   { return e.timeout }
func (e fakeNetErr) Temporary() bool { return false }

func TestIsRetryable(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name      string
		err       error
		wantRetry bool
	}{
		{"nil", nil, false},
		{"net_timeout", fakeNetErr{timeout: true}, true},
		{"net_non_timeout", fakeNetErr{timeout: false}, false},
		{"http_503", &HTTPError{StatusCode: 503}, true},
		{"http_503_wrapped", fmt.Errorf("call: %w", &HTTPError{StatusCode: 500}), true},
		{"http_404", &HTTPError{StatusCode: 404}, false},
		{"unexpected_eof", fmt.Errorf("read: %w", io.ErrUnexpectedEOF), true},
		{"canceled", context.Canceled, false},
		{"deadline_exceeded", context.DeadlineExceeded, false},
		{"unknown", errors.New("boom"), false},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			if got := IsRetryable(tc.err); got != tc.wantRetry {
				t.Fatalf("IsRetryable(%v) = %v, want %v", tc.err, got, tc.wantRetry)
			}
		})
	}
}
```

## Review

The classifier is correct when the *field-dependent* decisions use `errors.As`
(the `net_timeout` vs `net_non_timeout` split and the `http_503` vs `http_404`
split both turn on a value, not an identity) and the *identity* decisions use
`errors.Is`. The single most important row is `deadline_exceeded`: it proves the
context check precedes the `net.Error` check, which is the difference between a
resilient client and one that retries requests the caller has already abandoned.
Reorder those two branches and that row goes red — the test is guarding the
ordering, not just the mapping.

Note that `fakeNetErr` implements `Temporary()` too, because the `net.Error`
interface still declares it (deprecated, but part of the type); a fake that omits
it does not satisfy the interface and `errors.As` would never match. Run
`go test -race` to confirm the parallel rows share nothing.

## Resources

- [errors.As](https://pkg.go.dev/errors#As) — extracting a concrete or interface error from the chain.
- [net.Error](https://pkg.go.dev/net#Error) — the `Timeout()`/`Temporary()` interface.
- [io.ErrUnexpectedEOF](https://pkg.go.dev/io#pkg-variables) and [context sentinels](https://pkg.go.dev/context#pkg-variables) — the identity errors used here.

---

Back to [00-concepts.md](00-concepts.md) | Next: [06-config-env-loader.md](06-config-env-loader.md)
