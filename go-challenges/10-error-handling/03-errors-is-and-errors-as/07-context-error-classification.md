# Exercise 7: Classify context.Canceled vs context.DeadlineExceeded

When a request-scoped operation returns an error, a server must distinguish the
client giving up from the server exceeding its own time budget: the first is a
client-gone event that warrants no alert, the second is a timeout that usually
does. This exercise builds a pipeline that classifies a returned error with
`errors.Is` against `context.Canceled` and `context.DeadlineExceeded`, and shows
why comparing `ctx.Err()` with `==` is fragile once the error is wrapped.

This module is fully self-contained: its own `go mod init`, demo, and tests.

## What you'll build

```text
ctxclassify/                    independent module: example.com/ctxclassify
  go.mod                        go 1.25
  classify.go                   Disposition, Classify(err), Fetch(ctx) wrapping ctx.Err()
  classify_test.go              cancel -> Canceled; 1ns timeout -> DeadlineExceeded
  cmd/demo/main.go              runnable demo of both dispositions
```

Files: `classify.go`, `classify_test.go`, `cmd/demo/main.go`.
Implement: `Classify(err)` mapping `context.Canceled` to a client-gone disposition and `context.DeadlineExceeded` to a timeout disposition via `errors.Is`; `Fetch(ctx)` wrapping `ctx.Err()` with `%w`.
Test: cancel the context then run the op and assert `errors.Is(err, context.Canceled)` and the mapped disposition; use a 1ns timeout and assert `errors.Is(err, context.DeadlineExceeded)`.
Verify: `go test -count=1 -race ./... && go vet ./...`

Set up the module:

```bash
go mod edit -go=1.25
```

### Two sentinels, two operational meanings

`context` reports why a context ended through two distinct sentinels.
`context.Canceled` means someone called the `cancel` function — in a server, this
almost always means the client disconnected or gave up, so the work was abandoned
by the caller. `context.DeadlineExceeded` means the context's deadline passed
before the work finished — the server ran out of its own time budget. These are
different events with different responses: a client-gone should be logged quietly
and never paged (an HTTP 499-style disposition, no alert), while a timeout is a
server-side problem that maps to 504 and usually should alert. Collapsing them
into one "context error" bucket loses the signal that tells you whether your
service is slow or your clients are impatient.

The classification must use `errors.Is`, not `==` on `ctx.Err()`. In real code the
context error rarely reaches you bare: `net/http`, `database/sql`, and your own
layers wrap it with `fmt.Errorf(..., %w)` as it propagates. The instant that
happens, `err == context.DeadlineExceeded` is false even though the deadline is
exactly why the operation failed — but `errors.Is(err, context.DeadlineExceeded)`
still finds it, because `%w` kept the sentinel in the tree. This exercise makes the
wrapping explicit: `Fetch` returns `fmt.Errorf("fetch: %w", ctx.Err())`, so the
returned error is one wrap layer removed from the sentinel, and `Classify` must
walk through that wrap. String matching (`strings.Contains(err.Error(),
"deadline")`) is equally fragile: the message text is not part of the API and
changes across Go versions and wrappers.

`Fetch` models a request-scoped operation: it blocks until either its (fake) work
completes or the context ends, and if the context ends first it returns the
wrapped `ctx.Err()`. Both test scenarios drive `Fetch` — one with a cancelled
context, one with an already-expired deadline — and assert the sentinel survives
the wrap and maps to the right disposition.

Create `classify.go`:

```go
package ctxclassify

import (
	"context"
	"errors"
	"fmt"
)

type Disposition int

const (
	// DispOK is a successful completion.
	DispOK Disposition = iota
	// DispClientGone is a client cancellation: log quietly, do not alert.
	DispClientGone
	// DispTimeout is a server-side deadline: map to 504 and alert.
	DispTimeout
	// DispUnknown is any other failure.
	DispUnknown
)

func (d Disposition) String() string {
	switch d {
	case DispOK:
		return "ok"
	case DispClientGone:
		return "client-gone"
	case DispTimeout:
		return "timeout"
	default:
		return "unknown"
	}
}

// Classify maps a returned error to an operational disposition using errors.Is,
// so it works even when the context sentinel is wrapped.
func Classify(err error) Disposition {
	switch {
	case err == nil:
		return DispOK
	case errors.Is(err, context.Canceled):
		return DispClientGone
	case errors.Is(err, context.DeadlineExceeded):
		return DispTimeout
	default:
		return DispUnknown
	}
}

// Fetch models a request-scoped operation: it completes its (fake) work unless
// the context ends first, in which case it returns the wrapped ctx.Err().
func Fetch(ctx context.Context, work <-chan struct{}) error {
	select {
	case <-work:
		return nil
	case <-ctx.Done():
		// Wrap with %w so callers must use errors.Is, not ==.
		return fmt.Errorf("fetch: %w", ctx.Err())
	}
}
```

### The runnable demo

The demo runs `Fetch` twice: once with a context it cancels immediately, once with
a context whose deadline has already passed, printing the classification of each.

Create `cmd/demo/main.go`:

```go
package main

import (
	"context"
	"fmt"
	"time"

	"example.com/ctxclassify"
)

func main() {
	// Client cancels.
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	err := ctxclassify.Fetch(ctx, make(chan struct{}))
	fmt.Printf("cancelled: %s\n", ctxclassify.Classify(err))

	// Deadline already passed.
	ctx2, cancel2 := context.WithTimeout(context.Background(), time.Nanosecond)
	defer cancel2()
	time.Sleep(time.Millisecond) // ensure the 1ns deadline has elapsed
	err = ctxclassify.Fetch(ctx2, make(chan struct{}))
	fmt.Printf("timed out: %s\n", ctxclassify.Classify(err))

	// Success.
	done := make(chan struct{})
	close(done)
	err = ctxclassify.Fetch(context.Background(), done)
	fmt.Printf("completed: %s\n", ctxclassify.Classify(err))
}
```

Run it:

```bash
go run ./cmd/demo
```

Expected output:

```
cancelled: client-gone
timed out: timeout
completed: ok
```

### Tests

`TestCancelledIsClientGone` cancels a context, runs `Fetch`, and asserts both the
`errors.Is(err, context.Canceled)` match through the wrap and the `DispClientGone`
disposition. `TestDeadlineIsTimeout` uses a 1ns timeout and asserts
`context.DeadlineExceeded` and `DispTimeout`. `TestWrappedStillMatches` proves `==`
would fail where `errors.Is` succeeds.

Create `classify_test.go`:

```go
package ctxclassify

import (
	"context"
	"errors"
	"testing"
	"time"
)

func TestCancelledIsClientGone(t *testing.T) {
	t.Parallel()
	ctx, cancel := context.WithCancel(t.Context())
	cancel()

	err := Fetch(ctx, make(chan struct{}))

	if !errors.Is(err, context.Canceled) {
		t.Fatalf("errors.Is(err, context.Canceled) = false; err=%v", err)
	}
	if got := Classify(err); got != DispClientGone {
		t.Fatalf("Classify = %s, want client-gone", got)
	}
}

func TestDeadlineIsTimeout(t *testing.T) {
	t.Parallel()
	ctx, cancel := context.WithTimeout(t.Context(), time.Nanosecond)
	defer cancel()
	time.Sleep(time.Millisecond) // let the 1ns deadline elapse

	err := Fetch(ctx, make(chan struct{}))

	if !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("errors.Is(err, context.DeadlineExceeded) = false; err=%v", err)
	}
	if got := Classify(err); got != DispTimeout {
		t.Fatalf("Classify = %s, want timeout", got)
	}
}

func TestWrappedStillMatches(t *testing.T) {
	t.Parallel()
	ctx, cancel := context.WithCancel(t.Context())
	cancel()
	err := Fetch(ctx, make(chan struct{}))

	// The wrapped error is NOT == the sentinel, but errors.Is finds it.
	if err == context.Canceled {
		t.Fatal("wrapped error should not be == the sentinel")
	}
	if !errors.Is(err, context.Canceled) {
		t.Fatal("errors.Is should still match through the wrap")
	}
}

func TestSuccessIsOK(t *testing.T) {
	t.Parallel()
	done := make(chan struct{})
	close(done)
	if got := Classify(Fetch(t.Context(), done)); got != DispOK {
		t.Fatalf("Classify = %s, want ok", got)
	}
}
```

## Review

The pipeline is correct when a cancelled context classifies as client-gone and an
expired deadline classifies as timeout, *through* the `%w` wrap that `Fetch`
applies. That wrap is deliberate: it forces `Classify` to use `errors.Is`, which
is the only comparison that survives real propagation where `net/http` and
`database/sql` wrap the context error before you see it. The mistakes to avoid are
`ctx.Err() == context.DeadlineExceeded` (breaks the moment anything wraps) and
matching on the error string (not part of the API, changes across versions). Keep
the two sentinels in separate dispositions — merging them loses the "slow server
vs impatient client" signal. Run `go test -race`.

## Resources

- [context package: Canceled and DeadlineExceeded](https://pkg.go.dev/context#pkg-variables) — the two distinct sentinels.
- [context.WithTimeout / WithCancel](https://pkg.go.dev/context#WithTimeout) — producing each condition.
- [errors.Is](https://pkg.go.dev/errors#Is) — matching a sentinel through wrapping.

---

Back to [00-concepts.md](00-concepts.md) | Previous: [06-custom-as-method-adapter.md](06-custom-as-method-adapter.md) | Next: [08-observability-extract-fields.md](08-observability-extract-fields.md)
