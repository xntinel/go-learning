# Exercise 7: Distinguishing Context Cancellation from Domain Failures

A client that hangs up and a deadline that expires are signals about the *call*,
not failures of the *domain* — and if you let them fall into your default-500
branch you manufacture spurious server errors and invite retry storms. This
exercise builds a boundary `Classify` that checks `context.Canceled` and
`context.DeadlineExceeded` *first*, and uses `context.Cause` to recover the real
reason behind a `WithCancelCause` cancellation.

This module is fully self-contained: its own `go mod init`, its own demo, its own
tests. Nothing here imports any other exercise.

## What you'll build

```text
context-error-distinction/         module example.com/context-error-distinction
  go.mod
  boundary.go                      Outcome enum; Classify() checks ctx errors first; Reason() via context.Cause
  cmd/demo/main.go                 cancel, deadline, cause, and a domain error through Classify
  boundary_test.go                 canceled->client-gone; deadline->timeout; cause recovered; domain still maps
```

- Files: `boundary.go`, `cmd/demo/main.go`, `boundary_test.go`.
- Implement: an `Outcome` enum, a `Classify(err)` that tests context errors before any domain category, and a `Reason(ctx)` that returns `context.Cause(ctx)`.
- Test: a cancelled context maps to the client-gone outcome (not internal); an expired deadline maps to timeout; `WithCancelCause(myErr)` yields `context.Cause == myErr` while `ctx.Err() == context.Canceled`; a plain `ErrUserNotFound` still maps to the domain branch.
- Verify: `go test -count=1 -race ./...`

Set up the module:

```bash
mkdir -p ~/go-exercises/context-error-distinction/cmd/demo
cd ~/go-exercises/context-error-distinction
go mod init example.com/context-error-distinction
```

### Order matters: context first, domain second

`Classify` is a `switch` whose *order* is the whole design. `context.Canceled` and
`context.DeadlineExceeded` are checked before any domain category, because they mean
something categorically different. A cancelled context means the client is gone —
there is no one waiting for a response, so the right outcome is a no-op or a 499,
never a 500. An exceeded deadline means the work ran out of time — a 504. If either
fell through to the `default` branch it would become an "internal error", the
transport would emit a 500, and a well-behaved client that sees a 500 might *retry*
the request it just abandoned, multiplying load exactly when the system is already
slow. Checking the context signals first is what prevents that.

`errors.Is` is the right predicate because these errors travel wrapped: a repository
that annotates a failed query with `%w` still carries `context.DeadlineExceeded`
underneath, and `errors.Is(err, context.DeadlineExceeded)` finds it while `==`
would not. Only after the two context checks does `Classify` consult domain
categories and finally default to internal.

`Reason` shows the other half of context error handling. When you cancel with
`context.WithCancelCause(parent, cause)`, `ctx.Err()` still reports the generic
`context.Canceled` — that is deliberate, so code that only branches on `Err()`
keeps working — but `context.Cause(ctx)` returns the specific cause you attached.
This lets you distinguish "user pressed stop" from "circuit breaker opened" from
"upstream returned 429" while keeping the `Err()` contract stable. In a real service
the cause goes to the log; the outcome (client-gone) drives the response.

Create `boundary.go`:

```go
package boundary

import (
	"context"
	"errors"
	"fmt"
)

// Domain categories, translated by lower layers before reaching classify.
var (
	ErrDomain       = errors.New("domain error")
	ErrUserNotFound = fmt.Errorf("user: not found: %w", ErrDomain)
)

// Outcome is what the boundary decides to do with a failed request.
type Outcome int

const (
	OutcomeOK Outcome = iota
	OutcomeClientGone
	OutcomeTimeout
	OutcomeNotFound
	OutcomeInternal
)

func (o Outcome) String() string {
	switch o {
	case OutcomeOK:
		return "ok"
	case OutcomeClientGone:
		return "client-gone"
	case OutcomeTimeout:
		return "timeout"
	case OutcomeNotFound:
		return "not-found"
	default:
		return "internal"
	}
}

// Classify decides the outcome for err. Context signals are checked FIRST, so a
// client disconnect or a deadline never falls through to the domain/internal
// branch and becomes a spurious 500.
func Classify(err error) Outcome {
	switch {
	case err == nil:
		return OutcomeOK
	case errors.Is(err, context.Canceled):
		return OutcomeClientGone
	case errors.Is(err, context.DeadlineExceeded):
		return OutcomeTimeout
	case errors.Is(err, ErrUserNotFound):
		return OutcomeNotFound
	default:
		return OutcomeInternal
	}
}

// Reason recovers the real cause behind a cancellation. For a context cancelled
// with WithCancelCause it returns the attached cause; ctx.Err() still reports
// context.Canceled.
func Reason(ctx context.Context) error {
	return context.Cause(ctx)
}
```

### The runnable demo

The demo runs all four paths: a cancelled context, an expired deadline, a
cause-carrying cancellation, and a plain domain error. Note the third line — the
`ctx.Err()` stays `context canceled` while `Reason` recovers the real cause.

Create `cmd/demo/main.go`:

```go
package main

import (
	"context"
	"errors"
	"fmt"
	"time"

	"example.com/context-error-distinction"
)

func main() {
	// Client hung up.
	ctx1, cancel1 := context.WithCancel(context.Background())
	cancel1()
	fmt.Printf("canceled -> %s\n", boundary.Classify(ctx1.Err()))

	// Deadline passed.
	ctx2, cancel2 := context.WithTimeout(context.Background(), time.Nanosecond)
	defer cancel2()
	<-ctx2.Done()
	fmt.Printf("deadline -> %s\n", boundary.Classify(ctx2.Err()))

	// Cancellation with a cause.
	myErr := errors.New("circuit breaker open")
	ctx3, cancel3 := context.WithCancelCause(context.Background())
	cancel3(myErr)
	fmt.Printf("cause -> ctxErr=%v cause=%v classify=%s\n",
		ctx3.Err(), boundary.Reason(ctx3), boundary.Classify(ctx3.Err()))

	// A real domain error.
	fmt.Printf("domain -> %s\n", boundary.Classify(boundary.ErrUserNotFound))
}
```

Run it:

```bash
go run ./cmd/demo
```

Expected output:

```
canceled -> client-gone
deadline -> timeout
cause -> ctxErr=context canceled cause=circuit breaker open classify=client-gone
domain -> not-found
```

### Tests

The tests drive real contexts. The cancellation and deadline cases assert the
non-domain outcomes (the whole point — a cancel must not classify as internal); the
cause case asserts the `Err()`/`Cause()` split; and the domain case confirms a real
category still reaches the domain branch, so the context checks did not swallow it.

Create `boundary_test.go`:

```go
package boundary

import (
	"context"
	"errors"
	"fmt"
	"testing"
	"time"
)

func TestCancelledIsClientGone(t *testing.T) {
	t.Parallel()
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if got := Classify(ctx.Err()); got != OutcomeClientGone {
		t.Fatalf("Classify(canceled) = %s; want client-gone", got)
	}
}

func TestDeadlineIsTimeout(t *testing.T) {
	t.Parallel()
	ctx, cancel := context.WithTimeout(context.Background(), time.Nanosecond)
	defer cancel()
	<-ctx.Done()
	if got := Classify(ctx.Err()); got != OutcomeTimeout {
		t.Fatalf("Classify(deadline) = %s; want timeout", got)
	}
}

func TestWrappedContextErrorStillClassifies(t *testing.T) {
	t.Parallel()
	ctx, cancel := context.WithTimeout(context.Background(), time.Nanosecond)
	defer cancel()
	<-ctx.Done()
	wrapped := fmt.Errorf("repo: query failed: %w", ctx.Err())
	if got := Classify(wrapped); got != OutcomeTimeout {
		t.Fatalf("Classify(wrapped deadline) = %s; want timeout", got)
	}
}

func TestCauseRecoversReason(t *testing.T) {
	t.Parallel()
	myErr := errors.New("circuit breaker open")
	ctx, cancel := context.WithCancelCause(context.Background())
	cancel(myErr)

	if !errors.Is(ctx.Err(), context.Canceled) {
		t.Fatalf("ctx.Err() = %v; want context.Canceled", ctx.Err())
	}
	if got := Reason(ctx); !errors.Is(got, myErr) {
		t.Fatalf("Reason = %v; want %v", got, myErr)
	}
}

func TestDomainErrorStillMaps(t *testing.T) {
	t.Parallel()
	if got := Classify(ErrUserNotFound); got != OutcomeNotFound {
		t.Fatalf("Classify(ErrUserNotFound) = %s; want not-found", got)
	}
	if got := Classify(errors.New("kaboom")); got != OutcomeInternal {
		t.Fatalf("Classify(random) = %s; want internal", got)
	}
}
```

## Review

The classification is correct when the two context signals win over every domain
category: a cancelled context is client-gone, an expired deadline is timeout, and
neither is ever an internal error — check them *first* in the switch. Use
`errors.Is`, not `==`, so a wrapped `context.DeadlineExceeded` from deep in the
repository is still recognized at the edge. The `WithCancelCause` split is the piece
people miss: `ctx.Err()` stays `context.Canceled` for compatibility while
`context.Cause(ctx)` carries the real reason for your logs. Get the order wrong and
a client disconnect becomes a 500, the caller retries, and you have built a retry
storm out of a dropped connection.

## Resources

- [`context` package](https://pkg.go.dev/context) — `Canceled`, `DeadlineExceeded`, `Cause`, `WithCancelCause`.
- [`errors.Is`](https://pkg.go.dev/errors#Is) — recognizing a wrapped context error at the boundary.
- [Go 1.21 release notes: context](https://go.dev/doc/go1.21#context) — `WithCancelCause` and `context.Cause`.

---

Back to [06-transient-vs-permanent-retry.md](06-transient-vs-permanent-retry.md) | Next: [08-machine-readable-error-codes.md](08-machine-readable-error-codes.md)
