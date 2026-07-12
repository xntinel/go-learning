# Exercise 4: Classify Why Work Stopped — Deadline vs Client-Gone vs Load-Shed

When a request ends early, your metrics need to know *why*: a genuine SLO timeout,
a client that hung up, and an operator-triggered load-shed are three very
different signals that `ctx.Err()` collapses into one word. This exercise builds a
request coordinator on top of `context.WithCancelCause` and
`context.WithTimeoutCause` so that `context.Cause(ctx)` returns a domain-specific
reason, and maps each reason to the metric label and HTTP status a real handler
would emit.

This module is fully self-contained: its own `go mod init`, all code inline, its
own demo and tests. Nothing here imports another exercise.

## What you'll build

```text
causeclass/                  independent module: example.com/causeclass
  go.mod                     go 1.24
  causeclass.go              sentinels; WithSLOBudget (WithTimeoutCause); Classify; StatusFor
  cmd/
    demo/
      main.go                three scenarios, each printing label + status
  causeclass_test.go         one cause per test + a label->status table
```

Files: `causeclass.go`, `cmd/demo/main.go`, `causeclass_test.go`.
Implement: sentinels `ErrClientDisconnected`, `ErrLoadShed`, `ErrSLOExpired`; a
`WithSLOBudget` helper built on `context.WithTimeoutCause`; a `Classify(ctx)` that
reads `context.Cause`; a `StatusFor(label)` mapping.
Test: cancelling with each cause makes `Classify` return the right label while
`ctx.Err()` stays coarse; `Classify` is `"live"` before cancellation; a table maps
labels to statuses.
Verify: `go test -count=1 -race ./...`

Set up the module:

```bash
go mod edit -go=1.24
```

### Why `ctx.Err()` is not enough

`ctx.Err()` answers exactly one question — "canceled or deadline?" — and it
answers it with two values and no room for a third. That is fine for control flow
("should this goroutine stop?") but useless for observability. A request that ends
because the client TCP connection dropped, a request that ends because a load
shedder rejected it under overload, and a request that ends because it blew its
SLO budget all deserve different dashboards, different alerts, and often different
HTTP status codes. If your only signal is `ctx.Err() == context.Canceled`, the
first two are indistinguishable and the third is mislabeled the moment you cancel
a timeout early.

`context.Cause(ctx)` is the fix. Attach a domain error at the point you know the
reason, and read it back at the point you emit the metric:

- For an explicit cancel, `context.WithCancelCause` returns a
  `CancelCauseFunc` — `func(cause error)`. Calling `cancel(ErrClientDisconnected)`
  records the reason; `ctx.Err()` is still `context.Canceled`, but
  `context.Cause(ctx)` returns `ErrClientDisconnected`.
- For a timeout, `context.WithTimeoutCause(parent, d, ErrSLOExpired)` records the
  reason on the *timeout path only*. When the timer fires, `ctx.Err()` is
  `context.DeadlineExceeded` and `context.Cause(ctx)` returns `ErrSLOExpired`. The
  `CancelFunc` it returns does not set the cause — so if you cancel it manually
  before the deadline, `Cause` is the plain `context.Canceled`, and your classifier
  must handle that fall-through.

`Classify` centralizes this logic so no handler open-codes it, and `StatusFor`
turns a label into the status code the edge should return. The coordinator pattern
here — one place that derives the request context with a cause, one place that
classifies it — is what keeps the taxonomy consistent across every endpoint.

Create `causeclass.go`:

```go
package causeclass

import (
	"context"
	"errors"
	"net/http"
	"time"
)

// Domain reasons a request can end. These ride on the context as the "cause" so
// context.Cause can report them even though ctx.Err() stays coarse.
var (
	// ErrClientDisconnected means the caller went away before we finished.
	ErrClientDisconnected = errors.New("causeclass: client disconnected")
	// ErrLoadShed means an operator or load shedder aborted the request.
	ErrLoadShed = errors.New("causeclass: request shed under load")
	// ErrSLOExpired is attached to the SLO deadline; it surfaces via
	// context.Cause when the budget timer fires.
	ErrSLOExpired = errors.New("causeclass: SLO budget expired")
)

// WithSLOBudget derives a context that expires after budget, tagging the timeout
// path with ErrSLOExpired so context.Cause can report a deadline distinctly from
// an ordinary cancellation.
func WithSLOBudget(parent context.Context, budget time.Duration) (context.Context, context.CancelFunc) {
	return context.WithTimeoutCause(parent, budget, ErrSLOExpired)
}

// Classify maps a finished (or live) context to a metric label by reading its
// cause. It returns "live" before any cancellation.
func Classify(ctx context.Context) string {
	cause := context.Cause(ctx)
	switch {
	case cause == nil:
		return "live"
	case errors.Is(cause, ErrClientDisconnected):
		return "client_disconnected"
	case errors.Is(cause, ErrLoadShed):
		return "load_shed"
	case errors.Is(cause, ErrSLOExpired), errors.Is(cause, context.DeadlineExceeded):
		return "deadline"
	case errors.Is(cause, context.Canceled):
		return "canceled"
	default:
		return "unknown"
	}
}

// StatusFor maps a Classify label to the HTTP status an edge handler should emit.
func StatusFor(label string) int {
	switch label {
	case "deadline":
		return http.StatusGatewayTimeout // 504
	case "client_disconnected":
		return 499 // nginx's "client closed request"
	case "load_shed":
		return http.StatusServiceUnavailable // 503
	case "live":
		return http.StatusOK // 200
	default:
		return http.StatusInternalServerError // 500
	}
}
```

### The runnable demo

Create `cmd/demo/main.go`:

```go
package main

import (
	"context"
	"fmt"
	"time"

	"example.com/causeclass"
)

func main() {
	// 1. Client hangs up.
	ctx1, cancel1 := context.WithCancelCause(context.Background())
	cancel1(causeclass.ErrClientDisconnected)
	report("client hang-up", ctx1)

	// 2. Operator sheds the request under load.
	ctx2, cancel2 := context.WithCancelCause(context.Background())
	cancel2(causeclass.ErrLoadShed)
	report("load shed", ctx2)

	// 3. SLO budget expires.
	ctx3, cancel3 := causeclass.WithSLOBudget(context.Background(), 5*time.Millisecond)
	defer cancel3()
	<-ctx3.Done()
	report("SLO timeout", ctx3)
}

func report(scenario string, ctx context.Context) {
	label := causeclass.Classify(ctx)
	fmt.Printf("%-14s err=%-26v label=%-19s status=%d\n",
		scenario, ctx.Err(), label, causeclass.StatusFor(label))
}
```

Run it:

```bash
go run ./cmd/demo
```

Expected output:

```
client hang-up err=context canceled           label=client_disconnected status=499
load shed      err=context canceled           label=load_shed           status=503
SLO timeout    err=context deadline exceeded  label=deadline            status=504
```

Note the `err` column shows `context canceled` for both the hang-up and the shed —
`ctx.Err()` cannot tell them apart. The `label` column, driven by
`context.Cause`, can.

### Tests

Create `causeclass_test.go`:

```go
package causeclass

import (
	"context"
	"errors"
	"fmt"
	"testing"
	"time"
)

func TestCauseIsClientDisconnected(t *testing.T) {
	t.Parallel()

	ctx, cancel := context.WithCancelCause(context.Background())
	cancel(ErrClientDisconnected)

	if !errors.Is(context.Cause(ctx), ErrClientDisconnected) {
		t.Fatalf("Cause = %v, want ErrClientDisconnected", context.Cause(ctx))
	}
	if !errors.Is(ctx.Err(), context.Canceled) {
		t.Fatalf("Err = %v, want context.Canceled (coarse)", ctx.Err())
	}
	if got := Classify(ctx); got != "client_disconnected" {
		t.Fatalf("Classify = %q, want client_disconnected", got)
	}
}

func TestCauseIsLoadShed(t *testing.T) {
	t.Parallel()

	ctx, cancel := context.WithCancelCause(context.Background())
	cancel(ErrLoadShed)

	if !errors.Is(context.Cause(ctx), ErrLoadShed) {
		t.Fatalf("Cause = %v, want ErrLoadShed", context.Cause(ctx))
	}
	if got := Classify(ctx); got != "load_shed" {
		t.Fatalf("Classify = %q, want load_shed", got)
	}
}

func TestCauseIsDeadline(t *testing.T) {
	t.Parallel()

	ctx, cancel := WithSLOBudget(context.Background(), 5*time.Millisecond)
	defer cancel()
	<-ctx.Done()

	// The timeout path sets ErrSLOExpired as the cause...
	if !errors.Is(context.Cause(ctx), ErrSLOExpired) {
		t.Fatalf("Cause = %v, want ErrSLOExpired", context.Cause(ctx))
	}
	// ...while ctx.Err() stays the standard DeadlineExceeded.
	if !errors.Is(ctx.Err(), context.DeadlineExceeded) {
		t.Fatalf("Err = %v, want context.DeadlineExceeded", ctx.Err())
	}
	if got := Classify(ctx); got != "deadline" {
		t.Fatalf("Classify = %q, want deadline", got)
	}
}

func TestNoCauseWhenLive(t *testing.T) {
	t.Parallel()

	ctx, cancel := context.WithCancelCause(context.Background())
	defer cancel(nil)

	if context.Cause(ctx) != nil {
		t.Fatalf("Cause = %v, want nil before cancellation", context.Cause(ctx))
	}
	if got := Classify(ctx); got != "live" {
		t.Fatalf("Classify = %q, want live", got)
	}
}

func TestManualCancelOfBudgetFallsThrough(t *testing.T) {
	t.Parallel()

	// Cancelling a WithTimeoutCause context BEFORE its timer does NOT set the
	// custom cause; it is the plain context.Canceled.
	ctx, cancel := WithSLOBudget(context.Background(), time.Hour)
	cancel()

	if !errors.Is(context.Cause(ctx), context.Canceled) {
		t.Fatalf("Cause = %v, want context.Canceled from manual cancel", context.Cause(ctx))
	}
	if got := Classify(ctx); got != "canceled" {
		t.Fatalf("Classify = %q, want canceled", got)
	}
}

func TestStatusForLabels(t *testing.T) {
	t.Parallel()

	cases := []struct {
		label string
		want  int
	}{
		{"deadline", 504},
		{"client_disconnected", 499},
		{"load_shed", 503},
		{"live", 200},
		{"unknown", 500},
	}
	for _, tc := range cases {
		if got := StatusFor(tc.label); got != tc.want {
			t.Errorf("StatusFor(%q) = %d, want %d", tc.label, got, tc.want)
		}
	}
}

func ExampleClassify() {
	ctx, cancel := context.WithCancelCause(context.Background())
	cancel(ErrLoadShed)

	label := Classify(ctx)
	fmt.Println(label, StatusFor(label))
	// Output: load_shed 503
}
```

## Review

The coordinator is correct when the `label` and the `err` disagree in exactly the
way they should: `ctx.Err()` stays coarse (`Canceled`/`DeadlineExceeded`) while
`context.Cause` carries the domain reason. The trap the tests pin down is the
`WithTimeoutCause` asymmetry — the custom cause is set only when the timer fires,
so `TestManualCancelOfBudgetFallsThrough` proves the classifier still does the
right thing when someone cancels the budget early and the cause degrades to
`context.Canceled`. In production this classifier lives once, at the request
boundary, and every endpoint funnels through it, so the metric taxonomy stays
consistent instead of each handler inventing its own labels. Run `go test -race`;
the cause plumbing is goroutine-safe by construction, but the race detector
confirms no accidental sharing.

## Resources

- [`context.WithCancelCause` and `context.Cause`](https://pkg.go.dev/context#WithCancelCause) — attaching and reading a cause.
- [`context.WithTimeoutCause`](https://pkg.go.dev/context#WithTimeoutCause) — a cause on the deadline path.
- [Go 1.20 release notes: context](https://go.dev/doc/go1.20#context) — where cause support landed.

---

Prev: [03-wrapped-error-chain.md](03-wrapped-error-chain.md) | Back to [00-concepts.md](00-concepts.md) | Next: [05-errgroup-bounded-fanout.md](05-errgroup-bounded-fanout.md)
