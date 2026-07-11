# Exercise 15: API Handler Timeout Race Due to Incomplete Context Check in Select

**Nivel: Intermedio** — validacion rapida (un test corto).

An API handler that waits for a downstream result usually has to give up for
two unrelated reasons: a server-imposed deadline expired, or the caller's own
request was cancelled (the client disconnected, an upstream context was
cancelled). Both are modeled as their own `context.Context`, and a `select`
that only watches one of the two `Done()` channels either hangs past a
cancelled request until the unrelated deadline eventually fires, or reports
the wrong reason for giving up. This module is fully self-contained: its own
`go mod init`, all code inline, its own demo and tests.

## What you'll build

```text
reqwait/                    independent module: example.com/request-timeout-select-race
  go.mod
  reqwait.go                ErrTimeout, ErrCancelled, WaitForResult
  cmd/
    demo/
      main.go                runnable demo: cancel-wins and deadline-wins scenarios
  reqwait_test.go            cancellation returns promptly, delivered result wins
```

- Files: `reqwait.go`, `cmd/demo/main.go`, `reqwait_test.go`.
- Implement: `WaitForResult(deadline, cancel context.Context, result <-chan int) (int, error)` that returns as soon as any one of the three inputs fires.
- Test: cancel the cancellation context immediately and assert the call returns within a short bound with `ErrCancelled`, not after blocking on an unrelated deadline; a second test asserts a delivered value wins over both.
- Verify: `go test -count=1 ./...`.

```bash
mkdir -p ~/go-exercises/request-timeout-select-race/cmd/demo
cd ~/go-exercises/request-timeout-select-race
go mod init example.com/request-timeout-select-race
```

### Why both Done() channels have to be in the same select

`WaitForResult` is handed two independent contexts on purpose: `deadline`
carries a server-side timeout the caller has no control over, and `cancel`
carries the caller's own reason to stop waiting. They are not redundant —
`context.WithTimeout` composed onto a parent does not know about a sibling
cancellation the caller derives separately, and merging them into one
context upstream would blur which one fired when a caller needs to log why
a request gave up. The bug this module guards against is a `select` that
lists `result` and only one of the two `Done()` cases:

```go
func WaitForResult(deadline, cancel context.Context, result <-chan int) (int, error) {
	select {
	case v := <-result:
		return v, nil
	case <-deadline.Done():
		return 0, ErrTimeout
	// BUG: no case <-cancel.Done() -- cancellation is invisible to this select
	}
}
```

With the `cancel` case missing, cancelling the request has no effect on the
`select` at all: the call keeps blocking until `result` is delivered or
`deadline` expires, whichever comes first, on a completely independent
schedule. If `deadline` is a long server timeout (or, worse, a
non-cancellable `context.Background()` standing in for "no deadline set"),
the goroutine blocks forever past the point the client already gave up —
exactly the goroutine leak a `select`-with-`ctx.Done()` pattern exists to
prevent. The fix is symmetric: every context a `select` is meant to respect
needs its own `case <-ctx.Done():` arm, because a `select` only reacts to
channels it explicitly lists, never to a context's cancellation as a side
effect of some other case.

Create `reqwait.go`:

```go
package reqwait

import (
	"context"
	"errors"
)

// ErrTimeout is returned when the timeout context expires before a result
// arrives.
var ErrTimeout = errors.New("request timed out")

// ErrCancelled is returned when the caller-controlled cancellation context
// is cancelled before a result arrives.
var ErrCancelled = errors.New("request cancelled")

// WaitForResult blocks until a result is delivered on result, the deadline
// context expires, or the cancel context is cancelled -- whichever happens
// first. Both contexts are watched independently because they model two
// different reasons a handler stops waiting: a server-imposed deadline and
// a client-imposed cancellation (e.g. the client disconnected).
func WaitForResult(deadline, cancel context.Context, result <-chan int) (int, error) {
	select {
	case v := <-result:
		return v, nil
	case <-deadline.Done():
		return 0, ErrTimeout
	case <-cancel.Done():
		return 0, ErrCancelled
	}
}
```

### The runnable demo

The demo runs two scenarios: a cancellation that fires before the deadline,
and a deadline that fires with no cancellation ever requested.

Create `cmd/demo/main.go`:

```go
package main

import (
	"context"
	"fmt"
	"time"

	"example.com/request-timeout-select-race"
)

func main() {
	// Scenario 1: the client cancels before the deadline fires.
	deadline, stopDeadline := context.WithTimeout(context.Background(), 100*time.Millisecond)
	defer stopDeadline()
	cancel, stopCancel := context.WithCancel(context.Background())
	go func() {
		time.Sleep(10 * time.Millisecond)
		stopCancel()
	}()
	result := make(chan int)
	v, err := reqwait.WaitForResult(deadline, cancel, result)
	fmt.Println("scenario 1:", v, err)

	// Scenario 2: no cancellation is requested, the deadline fires first.
	deadline2, stopDeadline2 := context.WithTimeout(context.Background(), 20*time.Millisecond)
	defer stopDeadline2()
	cancel2, stopCancel2 := context.WithCancel(context.Background())
	defer stopCancel2()
	result2 := make(chan int)
	v2, err2 := reqwait.WaitForResult(deadline2, cancel2, result2)
	fmt.Println("scenario 2:", v2, err2)
}
```

```bash
go run ./cmd/demo
```

Expected output:

```text
scenario 1: 0 request cancelled
scenario 2: 0 request timed out
```

### Tests

`TestWaitForResultReturnsPromptlyOnCancel` fires the cancellation context
before calling `WaitForResult` and races it against a `time.After` bound in
a separate goroutine — proving the call returns quickly with `ErrCancelled`
rather than blocking on `deadline`, which in this test is
`context.Background()` and never fires on its own. A second test confirms a
delivered result still wins over both.

Create `reqwait_test.go`:

```go
package reqwait

import (
	"context"
	"errors"
	"testing"
	"time"
)

func TestWaitForResultReturnsPromptlyOnCancel(t *testing.T) {
	deadline := context.Background() // Done() is nil: never fires on its own
	cancel, stop := context.WithCancel(context.Background())
	stop() // cancel is already fired before the call

	result := make(chan int)
	done := make(chan struct{})
	var v int
	var err error
	go func() {
		v, err = WaitForResult(deadline, cancel, result)
		close(done)
	}()

	select {
	case <-done:
	case <-time.After(200 * time.Millisecond):
		t.Fatal("WaitForResult did not return promptly after cancellation")
	}

	if !errors.Is(err, ErrCancelled) {
		t.Fatalf("err = %v, want ErrCancelled", err)
	}
	if v != 0 {
		t.Fatalf("v = %d, want 0", v)
	}
}

func TestWaitForResultReturnsValueWhenDelivered(t *testing.T) {
	deadline := context.Background()
	cancel := context.Background()
	result := make(chan int, 1)
	result <- 42

	v, err := WaitForResult(deadline, cancel, result)
	if err != nil {
		t.Fatalf("err = %v, want nil", err)
	}
	if v != 42 {
		t.Fatalf("v = %d, want 42", v)
	}
}
```

Run: `go test -count=1 ./...`.

## Review

A `select` only reacts to the channels explicitly listed in its `case`
arms; a context being cancelled has zero effect on a `select` that never
lists that context's `Done()` channel. When a function accepts more than
one context for more than one reason to stop waiting, every single one of
them needs its own `case <-ctx.Done():` arm, or that context's cancellation
is silently invisible to the wait. The test that catches this bounds the
*time to return* after cancellation with a `time.After` guard in a
goroutine — a test that only checks the final error value without racing
it against a deadline would hang indefinitely on the buggy version instead
of failing fast.

## Resources

- [Go Specification: Select statements](https://go.dev/ref/spec#Select_statements) — a `select` blocks until one of its listed communications can proceed; it has no visibility into a channel it does not list.
- [context package](https://pkg.go.dev/context) — `Done()` returns a channel closed when the context is cancelled or its deadline expires; a nil `Done()` (from `context.Background()`) blocks forever in a `select`.
- [Go Blog: Context](https://go.dev/blog/context) — composing deadline and cancellation contexts across API boundaries.

---

Back to [00-concepts.md](00-concepts.md) | Prev: [14-shadowed-err-in-nested-if-swallows-failure.md](14-shadowed-err-in-nested-if-swallows-failure.md) | Next: [16-connection-pool-defer-before-validation.md](16-connection-pool-defer-before-validation.md)
