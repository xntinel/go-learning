# Exercise 2: Decide Retry vs Fail By Inspecting Error Types

An outbound HTTP or DB client fails for many reasons, and only some of them are
worth retrying. A network timeout should be retried with backoff; a validation
error or a cancelled context should not. This exercise builds the classifier that
a retry loop consults, and it does so by inspecting error *types* through the wrap
chain — never by matching on message strings.

This module is fully self-contained: its own module, all code inline, its own
demo and tests.

## What you'll build

```text
retryclass/                  independent module: example.com/retryclass
  go.mod                     go 1.26
  classify.go                type Decision; Classify(error); TransientError; ValidationError; HTTPError
  cmd/
    demo/
      main.go                runnable demo: classify a handful of errors
  classify_test.go           table of constructed/wrapped errors -> expected Decision
```

- Files: `classify.go`, `cmd/demo/main.go`, `classify_test.go`.
- Implement: `Classify(err error) Decision` using `errors.Is`, `errors.As`, and a type switch to distinguish retriable from terminal errors.
- Test: a table of `fmt.Errorf`-wrapped errors (a fake `net.Error` timeout, `context.DeadlineExceeded`, `context.Canceled`, `*ValidationError`, `*TransientError`, `*HTTPError` with 4xx and 5xx) asserting each decision, including a wrapped-twice case.
- Verify: `go test -count=1 -race ./...`

Set up the module:

```bash
go mod edit -go=1.26
```

### Why type inspection, not string matching

A retry decision is a policy about the *kind* of failure, and the kind is
encoded in the error's concrete type, not in its human-readable message. If you
classify by `strings.Contains(err.Error(), "timeout")`, the classifier breaks the
first time a caller wraps the error (`fmt.Errorf("dial upstream: %w", err)` still
contains "timeout", but a differently-worded wrapper may not), or the first time
the standard library rewords a message across a release. Type inspection is
stable across both.

Two tools do the inspection. `errors.Is(err, target)` walks the `Unwrap` chain
comparing against a *sentinel value* — this is how you catch
`context.DeadlineExceeded` and `context.Canceled`, which are package-level error
values. `errors.As(err, &target)` walks the same chain looking for the first
error assignable to `target`'s type — this is how you catch a `net.Error`
(an interface: any concrete network error implementing `Timeout()`), a
`*TransientError`, or a `*ValidationError`, even when they are wrapped several
layers deep. The final `HTTPError` case then uses a plain type switch on the
status code to split 4xx (terminal) from 429/5xx (retriable), which is the shape
a type switch fits best: dispatch over a value you already hold concretely.

The decision enum has three values so the caller can act differently: `Retry`
immediately, `Backoff` after a delay, `Fail` permanently. A `net.Error` timeout
and a 5xx warrant backoff (the server is under load); a fresh `DeadlineExceeded`
or an explicit `TransientError` warrant a plain retry; everything terminal fails.

Create `classify.go`:

```go
// classify.go
package retryclass

import (
	"context"
	"errors"
	"fmt"
	"net"
)

// Decision is what a retry loop should do with a failed attempt.
type Decision int

const (
	Fail    Decision = iota // permanent; do not retry
	Retry                   // retry immediately
	Backoff                 // retry after a delay
)

func (d Decision) String() string {
	switch d {
	case Retry:
		return "Retry"
	case Backoff:
		return "Backoff"
	default:
		return "Fail"
	}
}

// TransientError marks a failure the caller has declared safe to retry.
type TransientError struct{ Op string }

func (e *TransientError) Error() string { return fmt.Sprintf("transient failure in %s", e.Op) }

// ValidationError marks input the server rejected; retrying cannot help.
type ValidationError struct{ Field string }

func (e *ValidationError) Error() string { return fmt.Sprintf("invalid field %q", e.Field) }

// HTTPError carries an upstream status code.
type HTTPError struct{ StatusCode int }

func (e *HTTPError) Error() string { return fmt.Sprintf("http status %d", e.StatusCode) }

// Classify decides how a retry loop should treat err. It inspects error types
// through the wrap chain rather than matching on message text.
func Classify(err error) Decision {
	if err == nil {
		return Fail
	}

	// Sentinel comparisons walk the chain.
	if errors.Is(err, context.Canceled) {
		return Fail // the caller gave up on purpose
	}
	if errors.Is(err, context.DeadlineExceeded) {
		return Retry
	}

	// Interface extraction: any wrapped net.Error reporting a timeout.
	var netErr net.Error
	if errors.As(err, &netErr) && netErr.Timeout() {
		return Backoff
	}

	// Concrete-type extraction through the chain.
	var transient *TransientError
	if errors.As(err, &transient) {
		return Retry
	}
	var validation *ValidationError
	if errors.As(err, &validation) {
		return Fail
	}

	// A held concrete error dispatched by a type switch on its content.
	var httpErr *HTTPError
	if errors.As(err, &httpErr) {
		switch {
		case httpErr.StatusCode == 429 || httpErr.StatusCode >= 500:
			return Backoff
		default:
			return Fail // 4xx other than 429 is a client error
		}
	}

	return Fail
}
```

### The runnable demo

Create `cmd/demo/main.go`. It uses a small `net.Error` fake so the demo needs no
real socket:

```go
// cmd/demo/main.go
package main

import (
	"context"
	"errors"
	"fmt"

	"example.com/retryclass"
)

// timeoutErr is a minimal net.Error reporting a timeout.
type timeoutErr struct{}

func (timeoutErr) Error() string   { return "i/o timeout" }
func (timeoutErr) Timeout() bool   { return true }
func (timeoutErr) Temporary() bool { return true }

func main() {
	cases := []error{
		fmt.Errorf("dial upstream: %w", timeoutErr{}),
		context.DeadlineExceeded,
		context.Canceled,
		fmt.Errorf("save order: %w", &retryclass.ValidationError{Field: "email"}),
		&retryclass.HTTPError{StatusCode: 503},
		errors.New("unknown failure"),
	}
	for _, err := range cases {
		fmt.Printf("%-8s <- %v\n", retryclass.Classify(err), err)
	}
}
```

Run it:

```bash
go run ./cmd/demo
```

Expected output:

```
Backoff  <- dial upstream: i/o timeout
Retry    <- context deadline exceeded
Fail     <- context canceled
Fail     <- save order: invalid field "email"
Backoff  <- http status 503
Fail     <- unknown failure
```

### Tests

The table constructs each error kind, wraps some of them, and asserts the
decision. The double-wrapped `TransientError` case proves `errors.As` unwraps
through more than one layer.

Create `classify_test.go`:

```go
// classify_test.go
package retryclass

import (
	"context"
	"errors"
	"fmt"
	"testing"
)

// fakeTimeout is a net.Error whose Timeout() is configurable.
type fakeTimeout struct{ timeout bool }

func (f fakeTimeout) Error() string   { return "fake network error" }
func (f fakeTimeout) Timeout() bool   { return f.timeout }
func (f fakeTimeout) Temporary() bool { return false }

func TestClassify(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name string
		err  error
		want Decision
	}{
		{"nil", nil, Fail},
		{"net timeout wrapped", fmt.Errorf("dial: %w", fakeTimeout{timeout: true}), Backoff},
		{"net non-timeout", fakeTimeout{timeout: false}, Fail},
		{"deadline exceeded", context.DeadlineExceeded, Retry},
		{"canceled", context.Canceled, Fail},
		{"transient once", &TransientError{Op: "publish"}, Retry},
		{"transient twice wrapped", fmt.Errorf("outer: %w", fmt.Errorf("inner: %w", &TransientError{Op: "publish"})), Retry},
		{"validation", fmt.Errorf("save: %w", &ValidationError{Field: "id"}), Fail},
		{"http 400", &HTTPError{StatusCode: 400}, Fail},
		{"http 429", &HTTPError{StatusCode: 429}, Backoff},
		{"http 503", &HTTPError{StatusCode: 503}, Backoff},
		{"plain error", errors.New("boom"), Fail},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			if got := Classify(tt.err); got != tt.want {
				t.Fatalf("Classify(%v) = %v, want %v", tt.err, got, tt.want)
			}
		})
	}
}

func ExampleClassify() {
	err := fmt.Errorf("call upstream: %w", context.DeadlineExceeded)
	fmt.Println(Classify(err))
	// Output: Retry
}
```

## Review

The classifier is correct when the decision depends only on error *identity and
type*, never on message text, and when it is stable under wrapping. The
`transient twice wrapped` case is the proof: two `fmt.Errorf("%w")` layers around
a `*TransientError` still classify as `Retry` because `errors.As` unwraps the
whole chain. Order matters in the function: sentinel `errors.Is` checks come
first, then `errors.As` for the `net.Error` interface and the concrete custom
types, and finally the `HTTPError` status switch — moving the `net.Error` check
after a broader case, or replacing any of it with `strings.Contains`, is the bug
this exercise exists to prevent. Note the `net.Error` fake implements
`Temporary()` as well as `Timeout()`: `net.Error` still declares both methods, so
a value missing `Temporary()` does not satisfy the interface.

## Resources

- [errors.As](https://pkg.go.dev/errors#As) — extracting a concrete or interface error type from a chain.
- [errors.Is](https://pkg.go.dev/errors#Is) — comparing against sentinel error values through the chain.
- [net.Error](https://pkg.go.dev/net#Error) — the `Timeout()`/`Temporary()` interface network errors implement.
- [context: Canceled and DeadlineExceeded](https://pkg.go.dev/context#pkg-variables) — the sentinel errors a cancelled or timed-out context returns.

---

Back to [00-concepts.md](00-concepts.md) | Next: [03-safe-numeric-narrowing.md](03-safe-numeric-narrowing.md)
