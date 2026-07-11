# Exercise 8: Context and Network: Distinguish Cancel, Deadline, and Timeout Through a Chain

"The request failed with a timeout" is three different failures that demand three
different responses: the caller *cancelled* (client went away — do not retry, log
quietly), the *deadline* passed (you ran out of time — 504, maybe retry), or the
*socket* timed out (network-level — 504, retry-friendly). All three survive
wrapping as distinguishable signals, and a correct boundary tells them apart. This
module builds a downstream-call wrapper and the classifier that keeps them
distinct.

This module is fully self-contained: its own `go mod init`, all code inline, its
own demo and tests. Nothing here imports another exercise.

## What you'll build

```text
downstream/                 independent module: example.com/downstream
  go.mod                    go 1.24
  downstream.go             Call wrapper (%w); Classify -> status via Is/As
  cmd/
    demo/
      main.go               runnable demo: one line per failure kind
  downstream_test.go        each root cause wrapped; correct branch fires, others do not
```

Files: `downstream.go`, `cmd/demo/main.go`, `downstream_test.go`.
Implement: `Call(ctx, do)` that wraps the downstream error with `%w`, and `Classify(err)` that returns a status by `errors.Is` against `context.Canceled`/`DeadlineExceeded` and `errors.As` to a `net.Error` whose `Timeout()` reports a socket timeout.
Test: inject each root cause wrapped under context; assert the correct branch fires and the wrong ones do not; for `net.Error`, use a fake and assert `errors.As` populates it and `Timeout()` is true; deadline and cancel must not be conflated.
Verify: `go test -count=1 -race ./...`

Set up the module:

```bash
mkdir -p ~/go-exercises/downstream/cmd/demo
cd ~/go-exercises/downstream
go mod init example.com/downstream
go mod edit -go=1.24
```

### Three signals, kept distinct through the chain

`context.Canceled` and `context.DeadlineExceeded` are two different package-level
sentinels. A `net.Error` with `Timeout() == true` is a third, structurally
different signal — an interface value carrying a method, not a sentinel. Wrapping
with `%w` preserves all three: an `errors.Is` finds the context sentinels no matter
how deep, and an `errors.As` extracts the `net.Error` to ask `Timeout()`. The bug
this exercise inoculates against is *conflation*: collapsing all three into a single
"timeout" branch, which loses the cancel-vs-deadline distinction that drives whether
you retry and which status you emit.

`Classify` checks in a deliberate order. Context sentinels first, because they are
the most specific statement of intent (the caller aborted, or the caller's deadline
hit) and they are cheap identity checks. Then `errors.As` to a `net.Error`, asking
`Timeout()` — a socket-level timeout that is not tied to the caller's context.
Anything else is a generic bad-gateway. Each branch returns a distinct status:
canceled maps to a 499-style "client closed request" (nginx's convention for a
client that hung up), deadline and socket-timeout both map to 504, and the default
is 502.

`Call` is the thin wrapper every downstream client has: it runs the operation and,
on error, wraps it with one context clause via `%w` so the caller can still classify
it. The point of the exercise is that this wrapping does not destroy any of the three
signals — they remain `errors.Is`/`errors.As`-reachable from the top.

Create `downstream.go`:

```go
package downstream

import (
	"context"
	"errors"
	"fmt"
	"net"
)

// Call runs a downstream operation and wraps any error with one context clause,
// preserving the underlying signal for classification.
func Call(ctx context.Context, do func() error) error {
	if err := do(); err != nil {
		return fmt.Errorf("downstream call: %w", err)
	}
	return nil
}

// Classify maps a (possibly wrapped) downstream error to an HTTP status and label,
// keeping cancel, deadline, and socket-timeout distinct.
func Classify(err error) (int, string) {
	if err == nil {
		return 200, "ok"
	}
	var ne net.Error
	switch {
	case errors.Is(err, context.Canceled):
		return 499, "client closed request"
	case errors.Is(err, context.DeadlineExceeded):
		return 504, "deadline exceeded"
	case errors.As(err, &ne) && ne.Timeout():
		return 504, "socket timeout"
	default:
		return 502, "bad gateway"
	}
}
```

### The runnable demo

The demo injects each of the four failure kinds through `Call` and prints the
classification. It defines a tiny fake `net.Error` for the socket-timeout case,
since a real one requires an actual network timeout.

Create `cmd/demo/main.go`:

```go
package main

import (
	"context"
	"errors"
	"fmt"

	"example.com/downstream"
)

// fakeTimeout implements net.Error with Timeout() == true.
type fakeTimeout struct{}

func (fakeTimeout) Error() string   { return "read tcp: i/o timeout" }
func (fakeTimeout) Timeout() bool   { return true }
func (fakeTimeout) Temporary() bool { return true }

func main() {
	cases := []struct {
		name string
		err  error
	}{
		{"canceled", context.Canceled},
		{"deadline", context.DeadlineExceeded},
		{"socket", fakeTimeout{}},
		{"other", errors.New("connection refused")},
	}

	for _, c := range cases {
		err := downstream.Call(context.Background(), func() error { return c.err })
		status, label := downstream.Classify(err)
		fmt.Printf("%-9s -> %d %s\n", c.name, status, label)
	}
}
```

Run it:

```bash
go run ./cmd/demo
```

Expected output:

```
canceled  -> 499 client closed request
deadline  -> 504 deadline exceeded
socket    -> 504 socket timeout
other     -> 502 bad gateway
```

### Tests

The table wraps each root cause through `Call` (so the classifier sees a wrapped
error, not the bare sentinel) and asserts both the status and that the wrong
branches do not fire — the `deadline` case explicitly asserts
`errors.Is(err, context.Canceled)` is false, and the `canceled` case asserts the
converse, so the two are provably not conflated. A dedicated test extracts the
`net.Error` with `errors.As` and asserts `Timeout()` is true, exercising the
capability path directly.

Create `downstream_test.go`:

```go
package downstream

import (
	"context"
	"errors"
	"fmt"
	"net"
	"testing"
)

// fakeTimeout implements net.Error with a true Timeout.
type fakeTimeout struct{}

func (fakeTimeout) Error() string   { return "read tcp: i/o timeout" }
func (fakeTimeout) Timeout() bool   { return true }
func (fakeTimeout) Temporary() bool { return true }

func TestClassify(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name       string
		cause      error
		wantStatus int
		wantLabel  string
	}{
		{"canceled", context.Canceled, 499, "client closed request"},
		{"deadline", context.DeadlineExceeded, 504, "deadline exceeded"},
		{"socket timeout", fakeTimeout{}, 504, "socket timeout"},
		{"generic", errors.New("connection refused"), 502, "bad gateway"},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			err := Call(context.Background(), func() error { return tt.cause })
			status, label := Classify(err)
			if status != tt.wantStatus || label != tt.wantLabel {
				t.Errorf("Classify = (%d, %q), want (%d, %q)", status, label, tt.wantStatus, tt.wantLabel)
			}
		})
	}
}

func TestDeadlineAndCancelNotConflated(t *testing.T) {
	t.Parallel()

	deadlineErr := Call(context.Background(), func() error { return context.DeadlineExceeded })
	if errors.Is(deadlineErr, context.Canceled) {
		t.Error("deadline error wrongly matches context.Canceled")
	}
	if !errors.Is(deadlineErr, context.DeadlineExceeded) {
		t.Error("deadline error should match context.DeadlineExceeded")
	}

	cancelErr := Call(context.Background(), func() error { return context.Canceled })
	if errors.Is(cancelErr, context.DeadlineExceeded) {
		t.Error("cancel error wrongly matches context.DeadlineExceeded")
	}
	if !errors.Is(cancelErr, context.Canceled) {
		t.Error("cancel error should match context.Canceled")
	}
}

func TestNetErrorExtraction(t *testing.T) {
	t.Parallel()
	err := Call(context.Background(), func() error { return fakeTimeout{} })

	var ne net.Error
	if !errors.As(err, &ne) {
		t.Fatal("errors.As should extract a net.Error from the wrapped chain")
	}
	if !ne.Timeout() {
		t.Error("extracted net.Error should report Timeout() == true")
	}
}

func ExampleClassify() {
	err := Call(context.Background(), func() error { return context.DeadlineExceeded })
	status, label := Classify(err)
	fmt.Println(status, label)
	// Output: 504 deadline exceeded
}
```

## Review

`Classify` is correct when the three signals stay distinct through the wrapper:
canceled to 499, deadline to 504, socket-timeout to 504-with-a-different-label, and
everything else to 502 — and, critically, when the deadline error does *not* match
`context.Canceled` and vice versa. That non-conflation is what
`TestDeadlineAndCancelNotConflated` pins; it is the difference between quietly
logging a client hang-up and alerting on a timeout. The `net.Error` path is a
capability check via `errors.As`, not a sentinel — a real socket timeout has no
package-level identity, so `Timeout()` is the only reliable discriminator. Keep the
context checks before the `net.Error` check so an error that is both is classified by
the more specific intent.

## Resources

- [context package](https://pkg.go.dev/context) — `Canceled` and `DeadlineExceeded` sentinels.
- [net.Error](https://pkg.go.dev/net#Error) — the `Timeout()`/`Temporary()` interface extracted with `errors.As`.
- [errors package](https://pkg.go.dev/errors) — `Is` for sentinels, `As` for the `net.Error` capability.

---

Back to [00-concepts.md](00-concepts.md) | Next: [09-safe-error-redaction-logging.md](09-safe-error-redaction-logging.md)
