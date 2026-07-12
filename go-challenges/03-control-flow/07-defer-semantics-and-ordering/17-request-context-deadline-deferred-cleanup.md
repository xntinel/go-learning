# Exercise 17: Request Deadline — Deferred Cleanup on Context Timeout

**Nivel: Intermedio** — validacion rapida (un test corto).

A handler that starts background I/O and returns as soon as the request's
context deadline fires has a narrow but real bug if it closes its resources
without first making sure that background work has actually stopped: the
still-running goroutine can write to the same buffer the handler is
draining, a data race that only shows up under load. This module builds a
`Handle` function whose deferred cascade cancels the background operation,
waits for it to actually exit, and only then drains and closes the shared
resource — in that order, guaranteed by defer's LIFO rule. The module is
fully self-contained: its own `go mod init`, all code inline, its own demo
and tests.

## What you'll build

```text
reqdeadline/                independent module: example.com/request-context-deadline-deferred-cleanup
  go.mod                     go 1.24
  reqdeadline.go             Resource, Handle(ctx, res, work) error, StreamChunks helper
  cmd/
    demo/
      main.go                runnable demo: ample deadline vs. short deadline cut off mid-stream
  reqdeadline_test.go        completes-before-deadline case; deadline-exceeded-still-cleans-up case
```

- Files: `reqdeadline.go`, `cmd/demo/main.go`, `reqdeadline_test.go`.
- Implement: `Resource` (`Write`, `Close`, `Closed`, `Drained`), `Handle(ctx context.Context, res *Resource, work func(context.Context, *Resource)) error`, `StreamChunks`.
- Test: one case where work finishes before the deadline, one where the deadline cuts it off, asserting the error and that cleanup still ran correctly.
- Verify: `go test -count=1 -race ./...`

Set up the module:

```bash
go mod edit -go=1.24
```

### Why the wait step has to sit between cancel and close

`Handle` starts `work` in its own goroutine and races it against `ctx.Done()`
with a `select`. Whichever branch fires, `Handle` returns immediately — but
"returns" only means the `select` is done, not that the background goroutine
has stopped. If `Handle` closed and drained `res` right away, a background
goroutine that has not yet observed cancellation could still be mid-`Write`
to the exact same buffer `Close` is reading, a data race. The three defers
close that gap. `defer res.Close()` is registered first, so it runs last.
`defer func() { <-done }()` is registered second, so it runs in the middle:
it blocks until the background goroutine's `close(done)` proves it has
actually returned. `defer cancel()` is registered third, so it runs first,
telling `work` to stop via `opCtx`. The resulting execution order — cancel,
then wait, then close — is exactly what's needed: tell it to stop, confirm
it stopped, only then touch the shared buffer.

Create `reqdeadline.go`:

```go
package reqdeadline

import (
	"context"
	"time"
)

// Resource simulates a pooled I/O resource -- a buffered writer over a
// network connection, say -- that must always drain whatever was written
// to it and close, regardless of how the handler exits.
type Resource struct {
	buffered []byte
	drained  []byte
	closed   bool
}

// Write appends to the resource's internal buffer.
func (r *Resource) Write(p []byte) { r.buffered = append(r.buffered, p...) }

// Close drains whatever is left in the buffer into drained, then marks the
// resource closed. Safe to call only once nothing else can write.
func (r *Resource) Close() {
	r.drained = append(r.drained, r.buffered...)
	r.buffered = nil
	r.closed = true
}

// Closed reports whether Close has run.
func (r *Resource) Closed() bool { return r.closed }

// Drained returns whatever was drained by Close.
func (r *Resource) Drained() []byte { return r.drained }

// Handle processes one request bound to ctx. work runs in its own
// goroutine, writing to res, using opCtx (a child of ctx) to know when to
// stop. Handle returns as soon as work finishes or ctx is done, whichever
// comes first.
//
// Three defers are registered in acquisition order and run in reverse
// (LIFO) at return: cancel fires first, telling the background goroutine
// to stop writing; the wait-for-done fires second, blocking until that
// goroutine has actually returned; only then does res.Close() drain
// whatever was buffered and mark the resource closed. That order is what
// makes the deadline path safe to run concurrently with the background
// goroutine at all -- without the wait step, Close could run while a write
// is still in flight, a data race on res.buffered.
func Handle(ctx context.Context, res *Resource, work func(context.Context, *Resource)) error {
	opCtx, cancel := context.WithCancel(ctx)
	done := make(chan struct{})

	defer res.Close()
	defer func() { <-done }()
	defer cancel()

	go func() {
		work(opCtx, res)
		close(done)
	}()

	select {
	case <-done:
		return nil
	case <-ctx.Done():
		return ctx.Err()
	}
}

// StreamChunks returns a work function that writes each chunk with a delay
// between writes, checking ctx for cancellation before and during each
// wait so it stops promptly once the deadline fires or the caller cancels.
func StreamChunks(chunks []string, delay time.Duration) func(context.Context, *Resource) {
	return func(ctx context.Context, res *Resource) {
		for _, c := range chunks {
			select {
			case <-ctx.Done():
				return
			default:
			}
			res.Write([]byte(c))
			select {
			case <-ctx.Done():
				return
			case <-time.After(delay):
			}
		}
	}
}
```

### The runnable demo

The demo runs `Handle` twice: once with a deadline generous enough for all
chunks to stream through, once with a deadline short enough to cut the
stream off partway through.

Create `cmd/demo/main.go`:

```go
package main

import (
	"context"
	"fmt"
	"time"

	"example.com/request-context-deadline-deferred-cleanup"
)

func main() {
	fmt.Println("-- ample deadline, work completes --")
	ctx1, cancel1 := context.WithTimeout(context.Background(), 200*time.Millisecond)
	defer cancel1()
	res1 := &reqdeadline.Resource{}
	err1 := reqdeadline.Handle(ctx1, res1, reqdeadline.StreamChunks([]string{"a", "b", "c"}, 10*time.Millisecond))
	fmt.Println("error:", err1)
	fmt.Println("drained:", string(res1.Drained()))
	fmt.Println("closed:", res1.Closed())

	fmt.Println("-- short deadline, cut off mid-stream --")
	ctx2, cancel2 := context.WithTimeout(context.Background(), 15*time.Millisecond)
	defer cancel2()
	res2 := &reqdeadline.Resource{}
	err2 := reqdeadline.Handle(ctx2, res2, reqdeadline.StreamChunks([]string{"a", "b", "c", "d", "e"}, 10*time.Millisecond))
	fmt.Println("error:", err2)
	fmt.Println("closed:", res2.Closed())
}
```

Run it:

```bash
go run ./cmd/demo
```

Expected output:

```text
-- ample deadline, work completes --
error: <nil>
drained: abc
closed: true
-- short deadline, cut off mid-stream --
error: context deadline exceeded
closed: true
```

### Tests

`TestHandleCompletesBeforeDeadline` checks the normal path. Because how many
chunks a real clock lets through before an arbitrary short deadline fires is
inherently timing-dependent,
`TestHandleDeadlineExceededStillCleansUp` instead uses a `work` function
that blocks on its own context until told to stop and signals a `stopped`
channel right after — deterministic proof that the background goroutine
actually exited, and that the resource was still drained and closed even
though `Handle` returned via the deadline branch. Run with `-race` to
confirm the wait step actually prevents the write/close race it exists to
avoid.

Create `reqdeadline_test.go`:

```go
package reqdeadline

import (
	"context"
	"errors"
	"testing"
	"time"
)

func TestHandleCompletesBeforeDeadline(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 200*time.Millisecond)
	defer cancel()

	res := &Resource{}
	err := Handle(ctx, res, StreamChunks([]string{"a", "b"}, 5*time.Millisecond))

	if err != nil {
		t.Fatalf("err = %v, want nil", err)
	}
	if !res.Closed() {
		t.Fatal("resource should be closed after Handle returns")
	}
	if string(res.Drained()) != "ab" {
		t.Fatalf("drained = %q, want %q", res.Drained(), "ab")
	}
}

func TestHandleDeadlineExceededStillCleansUp(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Millisecond)
	defer cancel()

	// A work function that never finishes on its own -- it only stops when
	// its context is cancelled -- simulating an operation with no natural
	// end, like a long-poll or a stuck downstream call. It closes stopped
	// right after observing cancellation, so the test can prove the
	// background goroutine actually exited before asserting anything else.
	stopped := make(chan struct{})
	work := func(ctx context.Context, res *Resource) {
		res.Write([]byte("partial"))
		<-ctx.Done()
		close(stopped)
	}

	res := &Resource{}
	err := Handle(ctx, res, work)

	if !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("err = %v, want %v", err, context.DeadlineExceeded)
	}

	select {
	case <-stopped:
	case <-time.After(time.Second):
		t.Fatal("background work never observed cancellation")
	}

	if !res.Closed() {
		t.Fatal("resource should still be closed after a deadline cutoff")
	}
	if string(res.Drained()) != "partial" {
		t.Fatalf("drained = %q, want %q", res.Drained(), "partial")
	}
}
```

Verify: `go test -count=1 -race ./...`

## Review

The handler is correct when cleanup runs identically well on both exit
paths — work finishing on its own, and the deadline cutting it off — and
never touches the shared resource while the background goroutine might
still be writing to it. The three-defer cascade produces that guarantee
structurally: LIFO order guarantees cancel-then-wait-then-close, and no
caller of `Handle` has to remember to coordinate that sequence by hand. The
mistake this design avoids is calling `res.Close()` directly after the
`select`, without first confirming the background goroutine actually
stopped — code that passes every test run on a fast machine and then
data-races in production under load.

## Resources

- [context package](https://pkg.go.dev/context) — `WithTimeout`, `WithCancel`, and the cancellation-propagates-to-children contract this handler relies on.
- [Go Specification: Defer statements](https://go.dev/ref/spec#Defer_statements) — deferred functions execute in LIFO order at return.
- [Go Blog: Go Concurrency Patterns: Context](https://go.dev/blog/context) — the deadline-propagation pattern this handler builds on.

---

Back to [00-concepts.md](00-concepts.md) | Prev: [16-event-ledger-deferred-flush.md](16-event-ledger-deferred-flush.md) | Next: [18-batch-job-per-item-cleanup-helper.md](18-batch-job-per-item-cleanup-helper.md)
