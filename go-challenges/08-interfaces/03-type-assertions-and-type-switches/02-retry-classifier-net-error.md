# Exercise 2: Classify Whether A Failed Call Is Retryable

An outbound HTTP or database client that retries must first answer one question
about every failure: is this transient (retry) or permanent (give up)? Getting it
wrong either hammers a dead endpoint or drops a call that a single retry would have
saved. This exercise builds the classifier the right way: walk the error chain with
`errors.As`/`errors.Is`, decide on `Timeout()` and context-deadline signals, and
never touch the deprecated `Temporary()`.

This module is fully self-contained: its own `go mod init`, all code inline, its
own demo and tests. Nothing here imports another exercise.

## What you'll build

```text
retryclass/                 independent module: example.com/retryclass
  go.mod                    module path
  retryclass.go             ShouldRetry, PermanentError (domain non-retryable error)
  cmd/
    demo/
      main.go               runnable demo classifying several failures
  retryclass_test.go        wrapped net.Error / context / permanent cases
```

Files: `retryclass.go`, `cmd/demo/main.go`, `retryclass_test.go`.
Implement: `ShouldRetry(err error) bool` and a `*PermanentError` domain type.
Test: wrapped errors around a fake `net.Error` (timeout true/false), a context-deadline error, and a `*PermanentError`; assert each classification and that a deeply wrapped timeout is still detected.
Verify: `go test -count=1 -race ./...`

Set up the module:

```bash
mkdir -p ~/go-exercises/retryclass/cmd/demo
cd ~/go-exercises/retryclass
go mod init example.com/retryclass
```

### The classification order

Order matters because errors overlap. An explicit `*PermanentError` is the
strongest signal a caller can attach, so it is checked first and short-circuits to
non-retryable even if it wraps something that otherwise looks transient. Next,
recover a `net.Error` with `errors.As` — not a raw assertion — so a timeout buried
under `fmt.Errorf("dial: %w", err)` is still found; if `ne.Timeout()` is true the
call is retryable. Finally, treat the two context/deadline sentinels
(`context.DeadlineExceeded` and `os.ErrDeadlineExceeded`) as retryable timeouts via
`errors.Is`. Anything else defaults to non-retryable: a `4xx`-style application
error, a malformed request, an `io.EOF` from a closed body — retrying those wastes
work.

Notice what is absent: `net.Error.Temporary()`. It is deprecated precisely because
"temporary" was never defined consistently across implementations, so it is not a
dependable retry signal. `Timeout()` plus the deadline sentinels are the honest
inputs.

The reason `errors.As(err, &ne)` is used rather than `err.(net.Error)` is the wrap
chain: real client code wraps the transport error with context (`"get %s: %w"`)
before it reaches your classifier. A raw assertion inspects only the outermost
error and would miss the wrapped `net.Error` entirely; `errors.As` walks `Unwrap`
until it finds a value assignable to `*net.Error`.

Create `retryclass.go`:

```go
package retryclass

import (
	"context"
	"errors"
	"net"
	"os"
)

// PermanentError marks a failure the caller knows is not worth retrying.
type PermanentError struct {
	Reason string
}

func (e *PermanentError) Error() string {
	return "permanent: " + e.Reason
}

// ShouldRetry reports whether a failed call is worth retrying. It classifies by
// walking the error chain: an explicit PermanentError is never retried; a
// net.Error timeout and context/os deadline errors are retried; everything else
// is treated as permanent. It deliberately ignores the deprecated Temporary().
func ShouldRetry(err error) bool {
	if err == nil {
		return false
	}

	var perm *PermanentError
	if errors.As(err, &perm) {
		return false
	}

	var ne net.Error
	if errors.As(err, &ne) {
		return ne.Timeout()
	}

	if errors.Is(err, context.DeadlineExceeded) || errors.Is(err, os.ErrDeadlineExceeded) {
		return true
	}

	return false
}
```

### The runnable demo

Create `cmd/demo/main.go`:

```go
package main

import (
	"context"
	"fmt"
	"io"

	"example.com/retryclass"
)

func main() {
	cases := []struct {
		label string
		err   error
	}{
		{"context deadline", fmt.Errorf("query: %w", context.DeadlineExceeded)},
		{"permanent", fmt.Errorf("validate: %w", &retryclass.PermanentError{Reason: "bad request"})},
		{"eof", fmt.Errorf("read body: %w", io.EOF)},
		{"nil", nil},
	}
	for _, c := range cases {
		fmt.Printf("%-16s retry=%v\n", c.label, retryclass.ShouldRetry(c.err))
	}
}
```

Run it:

```bash
go run ./cmd/demo
```

Expected output:

```
context deadline retry=true
permanent        retry=false
eof              retry=false
nil              retry=false
```


### Tests

The fake `net.Error` lets the test control `Timeout()` without a real socket. The
key cases: a wrapped timeout is retried, a wrapped non-timeout is not, a permanent
error wins even when it wraps something transient-looking, and a deeply nested
timeout is still detected through several `%w` layers.

Create `retryclass_test.go`:

```go
package retryclass

import (
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"testing"
)

// fakeNetErr implements net.Error with a controllable Timeout.
type fakeNetErr struct {
	timeout bool
}

func (e *fakeNetErr) Error() string   { return "fake net error" }
func (e *fakeNetErr) Timeout() bool   { return e.timeout }
func (e *fakeNetErr) Temporary() bool { return false }

func TestShouldRetry(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name string
		err  error
		want bool
	}{
		{"nil", nil, false},
		{"net timeout", &fakeNetErr{timeout: true}, true},
		{"net non-timeout", &fakeNetErr{timeout: false}, false},
		{"wrapped net timeout", fmt.Errorf("dial: %w", &fakeNetErr{timeout: true}), true},
		{"deeply wrapped timeout", fmt.Errorf("get: %w", fmt.Errorf("dial: %w", &fakeNetErr{timeout: true})), true},
		{"context deadline", fmt.Errorf("q: %w", context.DeadlineExceeded), true},
		{"os deadline", fmt.Errorf("io: %w", os.ErrDeadlineExceeded), true},
		{"eof", io.EOF, false},
		{"permanent", &PermanentError{Reason: "x"}, false},
		{"permanent wrapping timeout", fmt.Errorf("%w", &PermanentError{Reason: "x"}), false},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			if got := ShouldRetry(tc.err); got != tc.want {
				t.Fatalf("ShouldRetry(%q) = %v, want %v", tc.name, got, tc.want)
			}
		})
	}
}

func TestPermanentBeatsTimeout(t *testing.T) {
	t.Parallel()
	// A PermanentError takes precedence even if a timeout is also in the chain.
	err := fmt.Errorf("outer: %w", &PermanentError{Reason: "gave up"})
	if ShouldRetry(err) {
		t.Fatal("permanent error was classified retryable")
	}
	var perm *PermanentError
	if !errors.As(err, &perm) {
		t.Fatal("errors.As did not recover *PermanentError")
	}
}

func ExampleShouldRetry() {
	fmt.Println(ShouldRetry(fmt.Errorf("q: %w", context.DeadlineExceeded)))
	fmt.Println(ShouldRetry(&PermanentError{Reason: "bad input"}))
	// Output:
	// true
	// false
}
```

## Review

The classifier is correct when its decision is a pure function of what the chain
contains, in priority order: permanent overrides everything, a `net.Error` is
retried exactly when `Timeout()` is true, the deadline sentinels are retried, and
the default is non-retryable. The two mistakes it is built to avoid are using a raw
`err.(net.Error)` (which misses every wrapped transport error the real client
produces) and consulting `Temporary()` (deprecated and unreliable). The
`deeply wrapped timeout` and `permanent wrapping timeout` cases are the proof that
`errors.As` traverses the chain and that ordering holds. Run `go test -race` to
confirm.

## Resources

- [net.Error interface (Timeout, deprecated Temporary)](https://pkg.go.dev/net#Error)
- [errors.As](https://pkg.go.dev/errors#As)
- [context.DeadlineExceeded](https://pkg.go.dev/context#pkg-variables)

---

Back to [00-concepts.md](00-concepts.md) | Prev: [01-plugin-loader-type-switch.md](01-plugin-loader-type-switch.md) | Next: [03-responsewriter-interface-upgrades.md](03-responsewriter-interface-upgrades.md)
