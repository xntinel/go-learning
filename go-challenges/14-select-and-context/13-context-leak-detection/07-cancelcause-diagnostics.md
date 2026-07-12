# Exercise 7: Cause-Tagged Cancellation In Leak Reports

`ctx.Err()` tells you *that* a context ended; it never tells you *why*. In an
incident, "why" is the whole question — was this request cancelled because the
client hung up, because the service shed load, or because it blew its deadline?
This module extends the detector with cause-carrying wrappers and threads distinct
sentinel causes through a request pipeline, so the diagnostic report distinguishes
one cause from another where `ctx.Err()` would flatten them all to `Canceled`.

This module is fully self-contained: its own `go mod init`, all code inline, its
own demo and tests. Nothing here imports another exercise.

## What you'll build

```text
causedetect/                       module example.com/causedetect
  go.mod
  causedetect.go                   Detector with WithCancelCause/WithTimeoutCause; Ended() diagnostics; sentinels
  cmd/
    demo/
      main.go                      pipeline cancelled three ways, prints each cause
  causedetect_test.go              Cause vs Err, timeout-cause-on-expiry, plain-cancel-omits-cause, errors.Is
```

Files: `causedetect.go`, `cmd/demo/main.go`, `causedetect_test.go`.
Implement: `WithCancelCause(parent)` and `WithTimeoutCause(parent, timeout, cause)` wrappers that record `context.Cause` when the context ends, an `Ended()` diagnostic log, and sentinels `ErrClientDisconnected`, `ErrLoadShed`.
Test: `context.Cause` returns the exact sentinel while `ctx.Err()` stays `Canceled`; `WithTimeoutCause` yields its cause on expiry but the plain cancel does not; `errors.Is` matches the sentinel through a wrapped pipeline error.
Verify: `go test -count=1 -race ./...`

### Cause is the diagnosability upgrade over Err

`context.WithCancelCause(parent)` returns a `CancelCauseFunc` — a
`func(cause error)` — and `context.Cause(ctx)` returns exactly the error that was
passed. Crucially, `ctx.Err()` is unchanged: it still returns `context.Canceled`.
So the same cancellation now carries two pieces of information at two altitudes:
`Err()` for control flow (is it cancelled? yes) and `Cause()` for diagnostics (why?
the client disconnected). The detector captures the cause at the moment a context
becomes `Done` — inside the `AfterFunc` callback, where `context.Cause(ctx)` is
guaranteed to be set — and appends it to an `Ended` log. An operator reading that
log sees "cancelled: client disconnected" next to "cancelled: load shed" next to
"deadline exceeded", instead of three identical `Canceled` lines.

`WithTimeoutCause(parent, timeout, cause)` has one sharp edge worth memorizing,
straight from the godoc: it sets `cause` *only when the timeout expires*. The
plain `CancelFunc` it returns does **not** set the cause — an explicit cancel of a
`WithTimeoutCause` context yields `context.Canceled`, not your sentinel. The tests
pin both halves of that rule, because getting it wrong means a report that claims
"load shed" for a context that was actually cancelled for an unrelated reason.

Create `causedetect.go`:

```go
package causedetect

import (
	"context"
	"errors"
	"fmt"
	"path/filepath"
	"runtime"
	"sync"
	"time"
)

// Sentinel causes an operator can distinguish in a diagnostic log.
var (
	ErrClientDisconnected = errors.New("client disconnected")
	ErrLoadShed           = errors.New("load shed")
)

type record struct {
	createdAt time.Time
	caller    string
}

// Ended records why one context ended, for the diagnostic log.
type Ended struct {
	Caller string
	Cause  error
}

// LeakReport describes a context still active past the grace period.
type LeakReport struct {
	Caller string
	Age    time.Duration
}

// Detector tracks contexts and, when they end, records their cancellation cause.
// Safe for concurrent use.
type Detector struct {
	mu          sync.Mutex
	active      map[*record]struct{}
	ended       []Ended
	gracePeriod time.Duration
}

// New returns a Detector with the given grace period for leak reporting.
func New(gracePeriod time.Duration) *Detector {
	return &Detector{active: make(map[*record]struct{}), gracePeriod: gracePeriod}
}

func callerInfo(skip int) string {
	_, file, line, ok := runtime.Caller(skip)
	if !ok {
		return "unknown:0"
	}
	return fmt.Sprintf("%s:%d", filepath.Base(file), line)
}

func (d *Detector) register(skip int) *record {
	r := &record{createdAt: time.Now(), caller: callerInfo(skip)}
	d.mu.Lock()
	d.active[r] = struct{}{}
	d.mu.Unlock()
	return r
}

// end moves a record out of active and logs its cause. It is idempotent: the
// eager cancel path and the AfterFunc path both call it, but only the first wins.
func (d *Detector) end(r *record, cause error) {
	d.mu.Lock()
	defer d.mu.Unlock()
	if _, ok := d.active[r]; !ok {
		return
	}
	delete(d.active, r)
	d.ended = append(d.ended, Ended{Caller: r.caller, Cause: cause})
}

// WithCancelCause wraps context.WithCancelCause. The returned CancelCauseFunc
// takes the cause; context.Cause(ctx) will return it.
func (d *Detector) WithCancelCause(parent context.Context) (context.Context, context.CancelCauseFunc) {
	ctx, cancel := context.WithCancelCause(parent)
	r := d.register(3) // 0 callerInfo, 1 register, 2 wrapper, 3 user code
	context.AfterFunc(ctx, func() { d.end(r, context.Cause(ctx)) })
	return ctx, func(cause error) {
		cancel(cause)
		d.end(r, context.Cause(ctx))
	}
}

// WithTimeoutCause wraps context.WithTimeoutCause. The cause is applied ONLY on
// timeout expiry; the returned CancelFunc yields context.Canceled instead.
func (d *Detector) WithTimeoutCause(parent context.Context, timeout time.Duration, cause error) (context.Context, context.CancelFunc) {
	ctx, cancel := context.WithTimeoutCause(parent, timeout, cause)
	r := d.register(3)
	context.AfterFunc(ctx, func() { d.end(r, context.Cause(ctx)) })
	return ctx, func() {
		cancel()
		d.end(r, context.Cause(ctx))
	}
}

// Check returns leaks: contexts still active past the grace period.
func (d *Detector) Check() []LeakReport {
	now := time.Now()
	d.mu.Lock()
	defer d.mu.Unlock()
	var out []LeakReport
	for r := range d.active {
		if age := now.Sub(r.createdAt); age >= d.gracePeriod {
			out = append(out, LeakReport{Caller: r.caller, Age: age})
		}
	}
	return out
}

// Ended returns the diagnostic log of ended contexts and their causes.
func (d *Detector) Ended() []Ended {
	d.mu.Lock()
	defer d.mu.Unlock()
	out := make([]Ended, len(d.ended))
	copy(out, d.ended)
	return out
}
```

### The runnable demo

The demo runs one request three ways: cancelled by a client disconnect, shed under
load, and expired on a deadline; each prints its `context.Cause`.

Create `cmd/demo/main.go`:

```go
package main

import (
	"context"
	"fmt"
	"time"

	"example.com/causedetect"
)

func main() {
	d := causedetect.New(time.Second)

	// 1. Client disconnected.
	_, c1 := d.WithCancelCause(context.Background())
	c1(causedetect.ErrClientDisconnected)

	// 2. Load shed.
	_, c2 := d.WithCancelCause(context.Background())
	c2(causedetect.ErrLoadShed)

	// 3. Deadline exceeded: a short WithTimeoutCause with a nil cause reports the
	// natural context.DeadlineExceeded on expiry.
	ctx3, c3 := d.WithTimeoutCause(context.Background(), 10*time.Millisecond, nil)
	<-ctx3.Done() // let it expire
	c3()

	time.Sleep(20 * time.Millisecond) // let AfterFunc callbacks drain
	for _, e := range d.Ended() {
		fmt.Printf("ended at %s: cause=%v\n", e.Caller, e.Cause)
	}
}
```

Run it:

```bash
go run ./cmd/demo
```

Expected output (order may vary; each line shows a distinct cause):

```
ended at main.go:15: cause=client disconnected
ended at main.go:19: cause=load shed
ended at main.go:24: cause=context deadline exceeded
```

### Tests

Create `causedetect_test.go`:

```go
package causedetect

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"testing"
	"time"
)

func TestCauseCarriesSentinelWhileErrStaysCanceled(t *testing.T) {
	t.Parallel()

	d := New(time.Second)
	ctx, cancel := d.WithCancelCause(context.Background())
	cancel(ErrClientDisconnected)

	if got := ctx.Err(); !errors.Is(got, context.Canceled) {
		t.Errorf("ctx.Err() = %v, want context.Canceled", got)
	}
	if got := context.Cause(ctx); !errors.Is(got, ErrClientDisconnected) {
		t.Errorf("context.Cause() = %v, want ErrClientDisconnected", got)
	}
}

func TestTimeoutCauseAppliedOnExpiry(t *testing.T) {
	t.Parallel()

	d := New(time.Second)
	ctx, cancel := d.WithTimeoutCause(context.Background(), 15*time.Millisecond, ErrLoadShed)
	defer cancel()

	<-ctx.Done() // wait for the timeout to fire
	if got := ctx.Err(); !errors.Is(got, context.DeadlineExceeded) {
		t.Errorf("ctx.Err() = %v, want DeadlineExceeded", got)
	}
	if got := context.Cause(ctx); !errors.Is(got, ErrLoadShed) {
		t.Errorf("context.Cause() = %v, want ErrLoadShed on expiry", got)
	}
}

func TestPlainCancelDoesNotSetTimeoutCause(t *testing.T) {
	t.Parallel()

	d := New(time.Second)
	// A long timeout we cancel explicitly before it fires.
	ctx, cancel := d.WithTimeoutCause(context.Background(), time.Hour, ErrLoadShed)
	cancel() // plain cancel: cause is NOT the supplied sentinel

	if got := context.Cause(ctx); !errors.Is(got, context.Canceled) {
		t.Errorf("context.Cause() = %v, want context.Canceled (plain cancel omits the timeout cause)", got)
	}
	if errors.Is(context.Cause(ctx), ErrLoadShed) {
		t.Error("plain cancel wrongly reported the timeout cause ErrLoadShed")
	}
}

func TestErrorsIsMatchesThroughWrappedPipeline(t *testing.T) {
	t.Parallel()

	d := New(time.Second)
	ctx, cancel := d.WithCancelCause(context.Background())
	// A pipeline stage wraps the sentinel with context about the record it rejected.
	cancel(fmt.Errorf("stage validate rejected record 7: %w", ErrLoadShed))

	if got := context.Cause(ctx); !errors.Is(got, ErrLoadShed) {
		t.Errorf("errors.Is(Cause, ErrLoadShed) failed; Cause = %v", got)
	}
}

func TestEndedLogRecordsCauseAndCaller(t *testing.T) {
	t.Parallel()

	d := New(time.Second)
	_, cancel := d.WithCancelCause(context.Background())
	cancel(ErrClientDisconnected)

	time.Sleep(20 * time.Millisecond) // let AfterFunc drain
	ended := d.Ended()
	if len(ended) != 1 {
		t.Fatalf("Ended() = %d entries, want 1", len(ended))
	}
	if !errors.Is(ended[0].Cause, ErrClientDisconnected) {
		t.Errorf("logged cause = %v, want ErrClientDisconnected", ended[0].Cause)
	}
	if got := ended[0].Caller; !strings.HasPrefix(got, "causedetect_test.go:") {
		t.Errorf("logged caller = %q, want the user's test file", got)
	}
}
```

## Review

The module is correct when `context.Cause` returns the exact sentinel a stage
passed while `ctx.Err()` remains `context.Canceled` — that split is the entire
value of cause tagging. The two `WithTimeoutCause` tests are the trap the godoc
warns about: the cause appears on *expiry* but a plain cancel yields `Canceled`, so
a report must never assume the configured timeout cause was the real reason.
`errors.Is` matching through a `%w`-wrapped pipeline error is what lets a stage add
context ("rejected record 7") without hiding the sentinel the operator greps for.
The `Ended` log's idempotent `end` is exercised by both the eager-cancel and
`AfterFunc` paths at once; run `-race` to confirm the double call is safe.

## Resources

- [context.WithCancelCause and context.Cause](https://pkg.go.dev/context#WithCancelCause) — the cause-carrying cancel and how `Cause` differs from `Err`.
- [context.WithTimeoutCause](https://pkg.go.dev/context#WithTimeoutCause) — the "cause on expiry only; plain cancel omits it" rule.
- [errors.Is](https://pkg.go.dev/errors#Is) — matching a sentinel through a `%w`-wrapped chain.

---

Back to [06-httptest-handler-leak.md](06-httptest-handler-leak.md) | Next: [08-pprof-goroutine-triage.md](08-pprof-goroutine-triage.md)
