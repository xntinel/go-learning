# Exercise 3: Reconnect-with-backoff loop that exits cleanly on context cancel

A message broker or websocket client that loses its connection must redial with
exponential backoff — but it must also stop dialing the instant the process is
shutting down, not after the full backoff sleep. That means racing the backoff
timer against `ctx.Done()` in a `select`, and leaving the retry loop with a
labeled `break` on both the success path and the cancel path.

This module is fully self-contained: its own `go mod init`, all code inline, its
own demo and tests. Nothing here imports any other exercise.

## What you'll build

```text
reconnect/                 independent module: example.com/reconnect
  go.mod                   go 1.24
  reconnect.go             Conn, DialFunc, Config; Connect retries with backoff
  cmd/
    demo/
      main.go              runnable demo: succeeds on the 2nd attempt
  reconnect_test.go        succeeds on Nth attempt; cancel interrupts backoff
```

- Files: `reconnect.go`, `cmd/demo/main.go`, `reconnect_test.go`.
- Implement: `Connect(ctx, Config) (*Conn, int, error)` that retries `Dial` with exponential backoff capped at `Max`, racing the backoff timer against `ctx.Done()`, and leaving the loop with a labeled `break` on success or cancel.
- Test: a dial that succeeds on the 3rd attempt returns the conn with attempt count 3; a dial that always fails, cancelled mid-backoff, returns `ctx.Err()` promptly rather than waiting out the backoff. Run with `-race`.
- Verify: `go test -count=1 -race ./...`

Set up the module:

```bash
go mod edit -go=1.24
```

### Why the timer must race ctx.Done(), and why the break needs a label

The naive backoff loop sleeps: `time.Sleep(backoff)` between attempts. That is a
shutdown bug. If `backoff` has grown to thirty seconds and the process receives
`SIGTERM`, the loop is parked inside `Sleep` and cannot notice the cancellation
until the sleep finishes; shutdown stalls for up to thirty seconds. The fix is to
turn the sleep into a `select` that waits on *either* the backoff timer *or*
`ctx.Done()`, whichever fires first. Cancellation now interrupts the wait
immediately.

That `select` is also where the labeled break becomes load-bearing. On the cancel
branch you must leave the outer retry `for`, but a bare `break` inside the
`select` would leave only the `select` and the loop would redial. So the cancel
branch does `break connect`, naming the `for`. On that branch the connection is
still `nil`, and after the loop the function returns `ctx.Err()`. The success path
also `break connect`s, but with `conn` set. Racing the timer and using the label
are the two halves of a correct reconnect loop.

Since Go 1.23 the garbage collector reclaims a `time.Timer` that is no longer
referenced even without a `Stop`, so the unstopped timer on the loop path does not
leak. Still, on the cancel branch we call `timer.Stop()` as hygiene: it documents
intent and prevents a stray tick from firing into logic that has already decided
to quit.

Create `reconnect.go`:

```go
package reconnect

import (
	"context"
	"time"
)

// Conn is a placeholder for an established broker/websocket connection.
type Conn struct {
	ID int
}

// DialFunc attempts a single connection. attempt is 1-based. A nil error means
// the connection succeeded.
type DialFunc func(ctx context.Context, attempt int) (*Conn, error)

// Config parameterizes the reconnect loop.
type Config struct {
	Base time.Duration // initial backoff
	Max  time.Duration // backoff ceiling
	Dial DialFunc
}

// Connect retries cfg.Dial with exponential backoff (doubling from Base, capped
// at Max) until it succeeds or ctx is cancelled. It returns the connection and
// the 1-based attempt count on success, or (nil, attempt, ctx.Err()) if the
// context is cancelled first. The backoff wait races the timer against
// ctx.Done() so cancellation is prompt.
func Connect(ctx context.Context, cfg Config) (*Conn, int, error) {
	backoff := cfg.Base
	attempt := 0
	var conn *Conn

connect:
	for {
		attempt++
		if c, err := cfg.Dial(ctx, attempt); err == nil {
			conn = c
			break connect
		}

		timer := time.NewTimer(backoff)
		select {
		case <-ctx.Done():
			timer.Stop()
			break connect // leave the for, not just the select
		case <-timer.C:
		}

		backoff = min(backoff*2, cfg.Max)
	}

	if conn == nil {
		return nil, attempt, ctx.Err()
	}
	return conn, attempt, nil
}
```

### The runnable demo

Create `cmd/demo/main.go`:

```go
package main

import (
	"context"
	"errors"
	"fmt"
	"time"

	"example.com/reconnect"
)

func main() {
	// A dial that fails once, then succeeds on the second attempt.
	dial := func(ctx context.Context, attempt int) (*reconnect.Conn, error) {
		if attempt < 2 {
			return nil, errors.New("connection refused")
		}
		return &reconnect.Conn{ID: 42}, nil
	}

	cfg := reconnect.Config{Base: 5 * time.Millisecond, Max: 100 * time.Millisecond, Dial: dial}
	conn, attempt, err := reconnect.Connect(context.Background(), cfg)
	if err != nil {
		fmt.Println("connect failed:", err)
		return
	}
	fmt.Printf("connected id=%d on attempt %d\n", conn.ID, attempt)
}
```

Run it:

```bash
go run ./cmd/demo
```

Expected output:

```
connected id=42 on attempt 2
```

### Tests

`TestSucceedsOnThirdAttempt` uses a dial that fails twice and succeeds on the
third, with tiny backoffs, and asserts the returned attempt count is 3.
`TestCancelInterruptsBackoff` uses a dial that always fails and a large `Base`
(one hour) so the backoff sleep would block essentially forever; it cancels the
context a few milliseconds in and asserts `Connect` returns `context.Canceled`
promptly, after only one attempt. That is the proof that cancellation interrupts
the backoff rather than waiting it out.

Create `reconnect_test.go`:

```go
package reconnect

import (
	"context"
	"errors"
	"fmt"
	"testing"
	"time"
)

func TestSucceedsOnThirdAttempt(t *testing.T) {
	t.Parallel()

	attempts := 0
	dial := func(ctx context.Context, attempt int) (*Conn, error) {
		attempts = attempt
		if attempt < 3 {
			return nil, errors.New("refused")
		}
		return &Conn{ID: 7}, nil
	}

	cfg := Config{Base: time.Millisecond, Max: 10 * time.Millisecond, Dial: dial}
	conn, got, err := Connect(context.Background(), cfg)
	if err != nil {
		t.Fatalf("Connect err = %v, want nil", err)
	}
	if conn == nil || conn.ID != 7 {
		t.Fatalf("conn = %+v, want ID 7", conn)
	}
	if got != 3 || attempts != 3 {
		t.Fatalf("attempt count = %d (dial saw %d), want 3", got, attempts)
	}
}

func TestCancelInterruptsBackoff(t *testing.T) {
	t.Parallel()

	dial := func(ctx context.Context, attempt int) (*Conn, error) {
		return nil, errors.New("always down")
	}

	ctx, cancel := context.WithCancel(context.Background())
	go func() {
		time.Sleep(5 * time.Millisecond)
		cancel()
	}()

	// Base is huge: if cancellation did not interrupt the backoff, Connect would
	// block for an hour. It must return promptly instead.
	cfg := Config{Base: time.Hour, Max: time.Hour, Dial: dial}

	done := make(chan struct{})
	var got int
	var err error
	go func() {
		_, got, err = Connect(ctx, cfg)
		close(done)
	}()

	select {
	case <-done:
	case <-time.After(2 * time.Second):
		t.Fatal("Connect did not return; cancellation failed to interrupt backoff")
	}

	if !errors.Is(err, context.Canceled) {
		t.Fatalf("Connect err = %v, want context.Canceled", err)
	}
	if got != 1 {
		t.Fatalf("attempt count = %d, want 1 (cancelled during first backoff)", got)
	}
}

func ExampleConnect() {
	dial := func(ctx context.Context, attempt int) (*Conn, error) {
		return &Conn{ID: 1}, nil
	}
	conn, attempt, _ := Connect(context.Background(), Config{
		Base: time.Millisecond,
		Max:  time.Second,
		Dial: dial,
	})
	fmt.Printf("id=%d attempt=%d\n", conn.ID, attempt)
	// Output: id=1 attempt=1
}
```

## Review

The loop is correct when success returns the conn with the right attempt count and
cancellation returns `ctx.Err()` without waiting out the backoff. The two failure
modes to watch: sleeping instead of racing the timer (so `TestCancelInterruptsBackoff`
would time out at two seconds), and a bare `break` on the cancel branch that leaves
only the `select` (so the loop redials forever). The `-race` flag matters here
because the cancel test drives `Connect` from one goroutine while cancelling from
another; the shared state is only the context, which is safe for concurrent use.
Note `min` is the builtin (Go 1.21+), used to cap the backoff at `Max`.

## Resources

- [context package](https://pkg.go.dev/context) — `WithCancel`, `Done`, and `Err`.
- [time.NewTimer](https://pkg.go.dev/time#NewTimer) — the backoff timer and `Timer.Stop`.
- [Go 1.23 release notes: timers](https://go.dev/doc/go1.23#timer-changes) — unreferenced timers are now garbage collected.

---

Back to [00-concepts.md](00-concepts.md) | Prev: [02-event-loop-break-in-select.md](02-event-loop-break-in-select.md) | Next: [04-worker-fanin-graceful-drain.md](04-worker-fanin-graceful-drain.md)
