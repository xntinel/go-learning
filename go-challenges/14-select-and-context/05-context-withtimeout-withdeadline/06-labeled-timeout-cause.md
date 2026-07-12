# Exercise 6: Labeled Timeouts With WithTimeoutCause for Observability

When a request threads through several timed stages and one blows its budget,
`ctx.Err()` says `DeadlineExceeded` for every stage — it cannot tell you which one
was slow. This exercise builds a two-stage pipeline where each stage wraps its
sub-timeout with `context.WithTimeoutCause` and a distinct sentinel, so a timeout
keeps `ctx.Err() == DeadlineExceeded` for compatibility while `context.Cause(ctx)`
returns the labeled stage — the attribution your logs and metrics need.

This module is fully self-contained. It has its own `go mod init`, defines every
type it needs, and ships its own demo and tests. Nothing here imports any other
exercise.

## What you'll build

```text
staged-cause/                        independent module: example.com/stagedcause
  go.mod                             go 1.26
  pipeline.go                        Pipeline, Run; ErrAuthSLO, ErrFetchSLO; caused sub-timeouts
  cmd/
    demo/
      main.go                        runnable demo: Err vs Cause on each stage timeout
  pipeline_test.go                   per-stage cause attribution, cancel-cause, fast path, -race
```

- Files: `pipeline.go`, `cmd/demo/main.go`, `pipeline_test.go`.
- Implement: `Pipeline{Auth, Fetch, AuthTimeout, FetchTimeout}` and `Run(ctx) (string, error)` where each stage runs under `context.WithTimeoutCause(parent, timeout, sentinel)` and a timeout is attributed via `context.Cause`.
- Test: force each stage to exceed its sub-timeout and assert `ctx.Err() == context.DeadlineExceeded` while `context.Cause` `errors.Is` the correct stage sentinel; a `WithCancelCause` path surfaces its own cause; `Cause` on a live context is nil; the fast path returns the result.
- Verify: `go test -count=1 -race ./...`

### The cause API, and why Err stays DeadlineExceeded

Go 1.21 added `context.WithTimeoutCause(parent, d, cause)` and
`context.WithDeadlineCause(parent, t, cause)`. They behave exactly like their
uncaused counterparts with one addition: when the deadline fires, `context.Cause(ctx)`
returns the `cause` error you supplied instead of the generic `context.DeadlineExceeded`
that `Cause` would otherwise mirror from `Err`. The crucial invariant is that
`ctx.Err()` is *unchanged* — it is still `context.DeadlineExceeded`. This is a
deliberate compatibility guarantee: every existing `errors.Is(err, context.DeadlineExceeded)`
check keeps working, and retry logic that keys off `Err` is unaffected. The cause is
purely additive attribution, read through a separate function.

That separation is the entire design. `Err()` answers a *policy* question — timeout or
cancel, which determines retry behavior. `Cause()` answers an *attribution* question —
which stage, which SLO, which semantic reason. A pipeline gives each stage a sub-context
with its own labeled timeout, so when a stage stalls, the code catches the
`DeadlineExceeded` and reads `context.Cause(stageCtx)` to learn *which* stage it was.
It logs and increments a metric against that sentinel, and a dashboard can show "auth
stage timed out 40 times, fetch stage 3 times" instead of an undifferentiated pile of
`DeadlineExceeded`. Reading `Err()` where you needed `Cause()` silently discards the
label you went to the trouble of attaching.

`WithCancelCause` (Go 1.20) is the manual-cancel analog: `cancel(reason)` makes
`Cause` return `reason` while `Err` stays `context.Canceled`. And on a context that is
*not yet done*, `Cause` returns nil — there is no reason yet — which is worth
remembering so the fast path is not misread as "caused by nil."

Create `pipeline.go`:

```go
package stagedcause

import (
	"context"
	"errors"
	"fmt"
	"time"
)

// ErrAuthSLO and ErrFetchSLO are the per-stage causes attached to each stage's
// sub-timeout, so a blown budget is attributable to the exact stage.
var (
	ErrAuthSLO  = errors.New("auth stage exceeded SLO")
	ErrFetchSLO = errors.New("fetch stage exceeded SLO")
)

// Pipeline runs an auth stage then a fetch stage, each under its own labeled
// sub-timeout derived from the caller's context.
type Pipeline struct {
	Auth         func(ctx context.Context) error
	Fetch        func(ctx context.Context) (string, error)
	AuthTimeout  time.Duration
	FetchTimeout time.Duration
}

// Run executes the two stages in order. On a stage timeout it returns an error
// that wraps both the stage's labeled cause and context.DeadlineExceeded, so
// callers can attribute the stage (errors.Is the sentinel) and still recognize a
// deadline (errors.Is DeadlineExceeded).
func (p *Pipeline) Run(ctx context.Context) (string, error) {
	authCtx, cancel := context.WithTimeoutCause(ctx, p.AuthTimeout, ErrAuthSLO)
	defer cancel()
	if err := p.Auth(authCtx); err != nil {
		if errors.Is(err, context.DeadlineExceeded) {
			return "", fmt.Errorf("auth stage: %w: %w", context.Cause(authCtx), err)
		}
		return "", fmt.Errorf("auth stage: %w", err)
	}

	fetchCtx, cancel2 := context.WithTimeoutCause(ctx, p.FetchTimeout, ErrFetchSLO)
	defer cancel2()
	data, err := p.Fetch(fetchCtx)
	if err != nil {
		if errors.Is(err, context.DeadlineExceeded) {
			return "", fmt.Errorf("fetch stage: %w: %w", context.Cause(fetchCtx), err)
		}
		return "", fmt.Errorf("fetch stage: %w", err)
	}
	return data, nil
}
```

### The runnable demo

The demo runs the pipeline three ways: both stages fast (success), auth too slow, and
fetch too slow — printing for each timeout that `Err` is a deadline while `Cause`
names the stage.

Create `cmd/demo/main.go`:

```go
package main

import (
	"context"
	"errors"
	"fmt"
	"time"

	"example.com/stagedcause"
)

func slow(d time.Duration) func(context.Context) error {
	return func(ctx context.Context) error {
		select {
		case <-time.After(d):
			return nil
		case <-ctx.Done():
			return ctx.Err()
		}
	}
}

func main() {
	fast := &stagedcause.Pipeline{
		Auth:         func(ctx context.Context) error { return nil },
		Fetch:        func(ctx context.Context) (string, error) { return "profile", nil },
		AuthTimeout:  time.Second,
		FetchTimeout: time.Second,
	}
	data, err := fast.Run(context.Background())
	fmt.Printf("fast: data=%q err=%v\n", data, err)

	slowAuth := &stagedcause.Pipeline{
		Auth:         slow(500 * time.Millisecond),
		Fetch:        func(ctx context.Context) (string, error) { return "profile", nil },
		AuthTimeout:  20 * time.Millisecond,
		FetchTimeout: time.Second,
	}
	_, err = slowAuth.Run(context.Background())
	fmt.Printf("slow-auth: authSLO=%v deadline=%v\n",
		errors.Is(err, stagedcause.ErrAuthSLO), errors.Is(err, context.DeadlineExceeded))
}
```

Run it:

```bash
go run ./cmd/demo
```

Expected output:

```
fast: data="profile" err=<nil>
slow-auth: authSLO=true deadline=true
```

### Tests

`TestAuthTimeoutAttributed` and `TestFetchTimeoutAttributed` force each stage to
stall and assert the returned error `errors.Is` both the stage sentinel and
`context.DeadlineExceeded`. `TestRawCausePreservesErr` builds a caused sub-timeout
directly and asserts the core invariant: `ctx.Err() == context.DeadlineExceeded`
while `context.Cause(ctx)` `errors.Is` the sentinel. `TestCancelCauseSurfacesReason`
exercises `WithCancelCause`. `TestCauseNilOnLiveContext` asserts `Cause` is nil before
the deadline. `TestFastPathSucceeds` asserts the happy path.

Create `pipeline_test.go`:

```go
package stagedcause

import (
	"context"
	"errors"
	"testing"
	"time"
)

func slow(d time.Duration) func(context.Context) error {
	return func(ctx context.Context) error {
		select {
		case <-time.After(d):
			return nil
		case <-ctx.Done():
			return ctx.Err()
		}
	}
}

func TestAuthTimeoutAttributed(t *testing.T) {
	t.Parallel()
	p := &Pipeline{
		Auth:         slow(500 * time.Millisecond),
		Fetch:        func(ctx context.Context) (string, error) { return "data", nil },
		AuthTimeout:  20 * time.Millisecond,
		FetchTimeout: time.Second,
	}
	_, err := p.Run(context.Background())
	if !errors.Is(err, ErrAuthSLO) {
		t.Fatalf("err = %v, want attributed to ErrAuthSLO", err)
	}
	if !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("err = %v, want DeadlineExceeded in chain", err)
	}
	if errors.Is(err, ErrFetchSLO) {
		t.Fatalf("err = %v, must NOT be attributed to fetch", err)
	}
}

func TestFetchTimeoutAttributed(t *testing.T) {
	t.Parallel()
	p := &Pipeline{
		Auth: func(ctx context.Context) error { return nil },
		Fetch: func(ctx context.Context) (string, error) {
			select {
			case <-time.After(500 * time.Millisecond):
				return "data", nil
			case <-ctx.Done():
				return "", ctx.Err()
			}
		},
		AuthTimeout:  time.Second,
		FetchTimeout: 20 * time.Millisecond,
	}
	_, err := p.Run(context.Background())
	if !errors.Is(err, ErrFetchSLO) {
		t.Fatalf("err = %v, want attributed to ErrFetchSLO", err)
	}
	if !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("err = %v, want DeadlineExceeded in chain", err)
	}
}

func TestRawCausePreservesErr(t *testing.T) {
	t.Parallel()
	ctx, cancel := context.WithTimeoutCause(context.Background(), time.Millisecond, ErrAuthSLO)
	defer cancel()
	<-ctx.Done()

	if !errors.Is(ctx.Err(), context.DeadlineExceeded) {
		t.Fatalf("ctx.Err() = %v, want DeadlineExceeded (unchanged by cause)", ctx.Err())
	}
	if !errors.Is(context.Cause(ctx), ErrAuthSLO) {
		t.Fatalf("context.Cause(ctx) = %v, want ErrAuthSLO", context.Cause(ctx))
	}
}

func TestCancelCauseSurfacesReason(t *testing.T) {
	t.Parallel()
	errManual := errors.New("client disconnected")
	ctx, cancel := context.WithCancelCause(context.Background())
	cancel(errManual)
	<-ctx.Done()

	if !errors.Is(ctx.Err(), context.Canceled) {
		t.Fatalf("ctx.Err() = %v, want Canceled", ctx.Err())
	}
	if !errors.Is(context.Cause(ctx), errManual) {
		t.Fatalf("context.Cause(ctx) = %v, want errManual", context.Cause(ctx))
	}
}

func TestCauseNilOnLiveContext(t *testing.T) {
	t.Parallel()
	ctx, cancel := context.WithTimeoutCause(context.Background(), time.Hour, ErrAuthSLO)
	defer cancel()
	if context.Cause(ctx) != nil {
		t.Fatalf("context.Cause(live) = %v, want nil", context.Cause(ctx))
	}
}

func TestFastPathSucceeds(t *testing.T) {
	t.Parallel()
	p := &Pipeline{
		Auth:         func(ctx context.Context) error { return nil },
		Fetch:        func(ctx context.Context) (string, error) { return "profile", nil },
		AuthTimeout:  time.Second,
		FetchTimeout: time.Second,
	}
	data, err := p.Run(context.Background())
	if err != nil {
		t.Fatalf("err = %v, want nil", err)
	}
	if data != "profile" {
		t.Fatalf("data = %q, want %q", data, "profile")
	}
}
```

## Review

The pipeline is correct when a timeout is both recognizable as a deadline and
attributable to a stage. `TestRawCausePreservesErr` pins the core invariant that the
cause API does not disturb `Err()`: after a `WithTimeoutCause` expiry, `Err()` is
`DeadlineExceeded` and only `Cause()` returns the sentinel. The two stage tests prove
`Run` reads `Cause` from the *right* sub-context, so an auth timeout is never
misattributed to fetch. `TestCancelCauseSurfacesReason` shows the same split for a
manual cancel, and `TestCauseNilOnLiveContext` guards the "not done yet" case.

The mistakes to avoid: reading `ctx.Err()` when you wanted the attribution (you get
the generic deadline, not the stage); calling `Cause` on the *parent* context instead
of the stage's sub-context (the parent may not be the one that fired); and forgetting
`defer cancel()` on each caused sub-timeout, which `go vet` flags and which would leak
a timer per stage. Wrapping both the cause and `DeadlineExceeded` with `%w` is what
lets a single returned error answer both the policy and attribution questions. Run
`go test -race`.

## Resources

- [context.WithTimeoutCause](https://pkg.go.dev/context#WithTimeoutCause) — attaching a labeled cause to a timeout.
- [context.Cause](https://pkg.go.dev/context#Cause) — reading the cause, which is DeadlineExceeded/Canceled unless a cause was set.
- [context.WithCancelCause](https://pkg.go.dev/context#WithCancelCause) — the manual-cancel analog with its CancelCauseFunc.
- [Go 1.21 release notes](https://go.dev/doc/go1.21#context) — the introduction of the cause-carrying constructors.

---

Back to [05-budget-guard-skip-work.md](05-budget-guard-skip-work.md) | Next: [07-deadline-afterfunc-cleanup.md](07-deadline-afterfunc-cleanup.md)
