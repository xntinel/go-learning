# Exercise 7: Run Audit Flush and Lease Release After the Request Is Cancelled

Some work must run *because* a request was cancelled and therefore must survive
the very signal that ended the request: releasing a distributed lease, flushing a
buffered audit write, recording that the caller hung up. This exercise builds that
detached-cleanup path with `context.WithoutCancel` — which keeps the correlation
ID but drops cancellation — and `context.AfterFunc`, which fires the cleanup the
instant the parent is cancelled.

This module is fully self-contained: its own `go mod init`, all code inline, its
own demo and tests. Nothing here imports another exercise.

## What you'll build

```text
detach/                      independent module: example.com/detach
  go.mod                     go 1.24
  detach.go                  ctxKey + RequestID; Auditor; RegisterCleanup using
                             WithoutCancel + AfterFunc
  cmd/
    demo/
      main.go                cancel a request; watch the lease release + audit row land
  detach_test.go             cleanup runs after cancel, detached ctx has no deadline,
                             AfterFunc fire/stop semantics
```

Files: `detach.go`, `cmd/demo/main.go`, `detach_test.go`.
Implement: an `Auditor` sink; `RegisterCleanup(parent, audit, leaseID)` that
derives `context.WithoutCancel(parent)` and registers a `context.AfterFunc` which
releases the lease and writes an audit row carrying the correlation ID.
Test: the cleanup completes after the parent is cancelled with the ID intact; a
`WithoutCancel` child has a nil `Done` and nil `Err` after the parent cancels;
`AfterFunc`'s `stop()` returns false after the func ran and true if it prevented
the run.
Verify: `go test -count=1 -race ./...`

Set up the module:

```bash
mkdir -p ~/go-exercises/detach/cmd/demo
cd ~/go-exercises/detach
go mod init example.com/detach
go mod edit -go=1.24
```

### Why cleanup cannot run on the request context

The naive cleanup path runs the audit write and lease release on the same `ctx`
the request used. But that `ctx` is exactly the one that was just cancelled — the
client hung up, or the deadline fired — so the cleanup I/O is cancelled before it
can complete, the lease is never released, and the audit row is never written. The
harder the outage, the more these leaks matter: a lease that is never released
blocks the next worker; an audit row that is never written loses the record of
*why* a request failed.

`context.WithoutCancel(parent)` (Go 1.21) is the escape hatch. It returns a child
that shares the parent's *values* — so the correlation ID and tenant still travel
to the cleanup's log line — but drops the parent's cancellation and deadline
entirely. Its `Done()` channel is nil (it can never be cancelled by the parent),
and its `Err()` stays nil even after the parent is cancelled. In real code you
then give the detached context its *own* short timeout
(`context.WithTimeout(context.WithoutCancel(parent), 2*time.Second)`) so the
cleanup is bounded but independent of the request's lifetime.

`context.AfterFunc(parent, f)` is the trigger. It runs `f` in a fresh goroutine
the moment `parent` is cancelled, and returns a `stop func() bool`. The return
value of `stop()` is a small state machine worth memorizing: it returns true if it
de-registered `f` before `f` was scheduled (so `f` will never run), and false if
`f` had already been started or finished (so stopping came too late). That is why
`TestAfterFuncStopPreventsRun` asserts `true` and `TestAfterFuncFiresOnCancel`
asserts `false` — the two ends of the same contract.

The cleanup goroutine is joined deterministically through a `done` channel the
callback closes, so the tests never race the assertion against the background
goroutine.

Create `detach.go`:

```go
package detach

import (
	"context"
	"sync"
)

type ctxKey int

const requestIDKey ctxKey = 0

// WithRequestID attaches a correlation id.
func WithRequestID(ctx context.Context, id string) context.Context {
	return context.WithValue(ctx, requestIDKey, id)
}

// RequestID reads the correlation id, safely absent-tolerant.
func RequestID(ctx context.Context) (string, bool) {
	id, ok := ctx.Value(requestIDKey).(string)
	return id, ok
}

// Auditor is a concurrency-safe sink for audit rows.
type Auditor struct {
	mu   sync.Mutex
	rows []string
}

// NewAuditor returns an empty Auditor.
func NewAuditor() *Auditor { return &Auditor{} }

// Record appends one audit row.
func (a *Auditor) Record(row string) {
	a.mu.Lock()
	defer a.mu.Unlock()
	a.rows = append(a.rows, row)
}

// Rows returns a copy of the recorded rows.
func (a *Auditor) Rows() []string {
	a.mu.Lock()
	defer a.mu.Unlock()
	out := make([]string, len(a.rows))
	copy(out, a.rows)
	return out
}

// Cleanup bundles the AfterFunc stop handle with a Done channel that is closed
// once the detached cleanup has finished, so callers can join it deterministically.
type Cleanup struct {
	stop func() bool
	Done chan struct{}
}

// Stop cancels the pending cleanup. It returns true if the cleanup had not yet
// started (and will now never run), false if it had already fired.
func (c *Cleanup) Stop() bool { return c.stop() }

// RegisterCleanup arranges for a detached cleanup to run when parent is
// cancelled: it releases leaseID and writes an audit row carrying the parent's
// correlation id. The cleanup runs on a context derived with WithoutCancel, so
// it survives the very cancellation that triggers it.
func RegisterCleanup(parent context.Context, audit *Auditor, leaseID string) *Cleanup {
	detached := context.WithoutCancel(parent)
	done := make(chan struct{})
	stop := context.AfterFunc(parent, func() {
		defer close(done)
		// In production this ctx would carry its own timeout for the cleanup I/O:
		//   ctx, cancel := context.WithTimeout(detached, 2*time.Second)
		// Here the work is in-memory, so the detached context suffices.
		id, ok := RequestID(detached)
		if !ok {
			id = "unknown"
		}
		audit.Record("released lease=" + leaseID + " request_id=" + id)
	})
	return &Cleanup{stop: stop, Done: done}
}
```

### The runnable demo

The demo starts a request with a correlation ID, registers the cleanup, cancels
the request as if the client hung up, waits for the detached cleanup to finish,
and prints the audit row — which exists precisely because the cleanup outlived the
cancellation.

Create `cmd/demo/main.go`:

```go
package main

import (
	"context"
	"fmt"

	"example.com/detach"
)

func main() {
	audit := detach.NewAuditor()

	ctx, cancel := context.WithCancel(detach.WithRequestID(context.Background(), "corr-777"))
	cl := detach.RegisterCleanup(ctx, audit, "lease-42")

	// Client hangs up: cancel the request.
	cancel()

	// Join the detached cleanup deterministically.
	<-cl.Done

	for _, row := range audit.Rows() {
		fmt.Println(row)
	}
}
```

Run it:

```bash
go run ./cmd/demo
```

Expected output:

```
released lease=lease-42 request_id=corr-777
```

### Tests

Create `detach_test.go`:

```go
package detach

import (
	"context"
	"fmt"
	"testing"
	"time"
)

func TestCleanupRunsAfterCancel(t *testing.T) {
	t.Parallel()

	audit := NewAuditor()
	ctx, cancel := context.WithCancel(WithRequestID(context.Background(), "corr-1"))
	cl := RegisterCleanup(ctx, audit, "lease-9")

	cancel()
	<-cl.Done // join the detached goroutine

	rows := audit.Rows()
	if len(rows) != 1 {
		t.Fatalf("got %d audit rows, want 1: %v", len(rows), rows)
	}
	want := "released lease=lease-9 request_id=corr-1"
	if rows[0] != want {
		t.Fatalf("audit row = %q, want %q", rows[0], want)
	}
}

func TestDetachedContextHasNoDeadline(t *testing.T) {
	t.Parallel()

	parent, cancel := context.WithTimeout(WithRequestID(context.Background(), "corr-2"), time.Millisecond)
	defer cancel()

	detached := context.WithoutCancel(parent)
	<-parent.Done() // parent's deadline fires

	if detached.Done() != nil {
		t.Fatal("WithoutCancel child should have a nil Done channel")
	}
	if detached.Err() != nil {
		t.Fatalf("detached.Err() = %v, want nil after parent cancel", detached.Err())
	}
	// Values still ride the detached context.
	if id, ok := RequestID(detached); !ok || id != "corr-2" {
		t.Fatalf("RequestID(detached) = (%q, %v), want (corr-2, true)", id, ok)
	}
	// A timeout derived on top of the detached context has its own Done.
	bounded, cancelB := context.WithTimeout(detached, time.Hour)
	defer cancelB()
	if bounded.Done() == nil {
		t.Fatal("timeout derived from detached context must have a non-nil Done")
	}
}

func TestAfterFuncFiresOnCancel(t *testing.T) {
	t.Parallel()

	ran := make(chan struct{})
	ctx, cancel := context.WithCancel(context.Background())
	stop := context.AfterFunc(ctx, func() { close(ran) })

	cancel()
	<-ran // the func ran

	if stop() {
		t.Fatal("stop() returned true after the func already ran; want false")
	}
}

func TestAfterFuncStopPreventsRun(t *testing.T) {
	t.Parallel()

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	ran := false
	stop := context.AfterFunc(ctx, func() { ran = true })

	if !stop() {
		t.Fatal("stop() before cancel returned false; want true (it prevented the run)")
	}
	cancel() // too late: the func was already de-registered

	// Give any (incorrectly) scheduled goroutine a chance to run.
	time.Sleep(20 * time.Millisecond)
	if ran {
		t.Fatal("func ran even though stop() reported it was prevented")
	}
}

func ExampleRegisterCleanup() {
	audit := NewAuditor()
	ctx, cancel := context.WithCancel(WithRequestID(context.Background(), "corr-777"))
	cl := RegisterCleanup(ctx, audit, "lease-42")

	cancel()  // client hangs up
	<-cl.Done // join the detached cleanup

	fmt.Println(audit.Rows()[0])
	// Output: released lease=lease-42 request_id=corr-777
}
```

## Review

The cleanup is correct when it completes *after* the request context is cancelled,
carrying the correlation ID that rode the detached context. `TestCleanupRunsAfterCancel`
is the whole point: cancel first, then observe the audit row — if the cleanup ran
on the request context instead of a `WithoutCancel` child, the row would be missing
or the write would be cancelled. `TestDetachedContextHasNoDeadline` proves the two
properties that make this safe: the detached child cannot be cancelled by the
parent (nil `Done`, nil `Err`) yet still carries values, and a fresh timeout layered
on top gives the cleanup its own bound. The `AfterFunc` `stop()` semantics are the
easiest thing to get backwards, so the two `AfterFunc` tests assert both ends. Run
`go test -race`; the `done`-channel join is what keeps the background goroutine from
racing the assertions.

## Resources

- [`context.WithoutCancel`](https://pkg.go.dev/context#WithoutCancel) — a child that keeps values but drops cancellation.
- [`context.AfterFunc`](https://pkg.go.dev/context#AfterFunc) — run a func when a context is cancelled, with a stop handle.
- [Go 1.21 release notes: context](https://go.dev/doc/go1.21#context) — where `WithoutCancel` and `AfterFunc` landed.

---

Prev: [06-request-scoped-values.md](06-request-scoped-values.md) | Back to [00-concepts.md](00-concepts.md) | Next: [08-deadline-budget-propagation.md](08-deadline-budget-propagation.md)
