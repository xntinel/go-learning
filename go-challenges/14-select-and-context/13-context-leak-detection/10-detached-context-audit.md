# Exercise 10: Bounded Detached Work With WithoutCancel

Some work must outlive the request that triggered it: an audit-log entry, a
metrics flush, a "user deleted" webhook. If you derive it from `r.Context()`, the
client disconnecting mid-request cancels the write — data loss. The fix is
`context.WithoutCancel`, which detaches the child from the parent's cancellation.
But detaching also removes the parent's *deadline*, so a naive `WithoutCancel`
context is unbounded: the goroutine behind it can hang forever. This module builds
the trap and its fix — wrapping the detached context in a fresh `WithTimeout` — and
uses the detector to prove which one leaks.

This module is fully self-contained: its own `go mod init`, all code inline, its
own demo and tests. Nothing here imports another exercise.

## What you'll build

```text
detached/                          module example.com/detached
  go.mod
  detached.go                      Detector.Detached (trap) vs BoundedDetached (fix); detachedAudit
  cmd/
    demo/
      main.go                      audit survives request cancel; unbounded detached leaks
  detached_test.go                 WithoutCancel semantics, bounded=no leak, unbounded=leak, audit survives
```

Files: `detached.go`, `cmd/demo/main.go`, `detached_test.go`.
Implement: `Detached(parent)` (unbounded `WithoutCancel`, tracked) and `BoundedDetached(parent, timeout)` (`WithTimeout(WithoutCancel(parent), ...)`, tracked), plus a `detachedAudit` writer.
Test: parent cancellation does not close a detached context but its own timeout does; the bounded detached context leaves 0 leaks and the unbounded one leaves 1; an audit write completes despite the request being cancelled.
Verify: `go test -count=1 -race ./...`

### WithoutCancel detaches — and thereby unbounds

`context.WithoutCancel(parent)` returns a context that keeps the parent's *values*
but drops its *cancellation*: its `Done()` is `nil`, its `Err()` and `Cause()` are
`nil`, and it has no deadline. That is exactly right for fire-and-forget work that
must survive the request — cancelling `r.Context()` when the client hangs up will
not abort the audit write. But look at what you gave up: the request's deadline is
gone too. A goroutine that blocks on work derived from a bare `WithoutCancel`
context has *nothing* that will ever cancel it. If the downstream it calls hangs,
the goroutine hangs forever, and every such request adds one permanently-parked
goroutine and one permanently-pinned context node. Detaching without re-bounding
converts a request-scoped leak into an unbounded one.

The fix is one line: wrap the detached context in a fresh
`context.WithTimeout(context.WithoutCancel(parent), timeout)`. Now the detached
work still ignores the parent's cancellation, but it has its own independent
deadline, so it cannot hang past that budget. The detector makes the difference
observable: `Detached` (the trap) registers a context whose `Done()` is `nil`, so
its `AfterFunc` deregistration *never fires* and it stays a leak forever;
`BoundedDetached` (the fix) registers a context that becomes `Done` at its own
timeout, so `AfterFunc` clears it. One method leaks, the other does not, and the
only difference is the added timeout.

Create `detached.go`:

```go
package detached

import (
	"context"
	"fmt"
	"path/filepath"
	"runtime"
	"sync"
	"time"
)

type record struct {
	createdAt time.Time
	caller    string
}

// LeakReport describes a context still outstanding past the grace period.
type LeakReport struct {
	Caller string
	Age    time.Duration
}

func (r LeakReport) String() string {
	return fmt.Sprintf("context leak: created at %s, age %v", r.Caller, r.Age)
}

// Detector tracks contexts and clears them via AfterFunc when they become Done.
type Detector struct {
	mu          sync.Mutex
	active      map[*record]struct{}
	gracePeriod time.Duration
}

// New returns a Detector reporting contexts outstanding past gracePeriod.
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

func (d *Detector) deregister(r *record) {
	d.mu.Lock()
	delete(d.active, r)
	d.mu.Unlock()
}

// Detached is the TRAP: it detaches from the parent but adds no deadline, so its
// Done() is nil, AfterFunc never fires, and it leaks until the process dies.
func (d *Detector) Detached(parent context.Context) context.Context {
	ctx := context.WithoutCancel(parent)
	r := d.register(3) // 0 callerInfo, 1 register, 2 Detached, 3 user code
	context.AfterFunc(ctx, func() { d.deregister(r) })
	return ctx
}

// BoundedDetached is the FIX: detached from the parent AND bounded by its own
// timeout, so it is independently cancellable and AfterFunc clears it.
func (d *Detector) BoundedDetached(parent context.Context, timeout time.Duration) (context.Context, context.CancelFunc) {
	ctx, cancel := context.WithTimeout(context.WithoutCancel(parent), timeout)
	r := d.register(3)
	context.AfterFunc(ctx, func() { d.deregister(r) })
	return ctx, func() {
		cancel()
		d.deregister(r)
	}
}

// Check returns leaks: contexts still outstanding past the grace period.
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

// ActiveContexts returns the number of outstanding contexts.
func (d *Detector) ActiveContexts() int {
	d.mu.Lock()
	defer d.mu.Unlock()
	return len(d.active)
}

// detachedAudit simulates a slow audit-log write that honors its context. It
// completes after 30ms, or reports abort if its context is cancelled first.
func detachedAudit(ctx context.Context, done chan<- string) {
	select {
	case <-time.After(30 * time.Millisecond):
		done <- "written"
	case <-ctx.Done():
		done <- "aborted"
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

	"example.com/detached"
)

func main() {
	d := detached.New(20 * time.Millisecond)

	// A request whose client disconnects mid-flight.
	req, cancelReq := context.WithCancel(context.Background())
	ctx, cancel := d.BoundedDetached(req, 200*time.Millisecond)

	result := make(chan string, 1)
	go func() {
		result <- audit(ctx)
	}()

	cancelReq() // client disconnects; the audit must still complete
	fmt.Println("audit result:", <-result)
	cancel()

	// The trap: an unbounded detached context is never cleared.
	_ = d.Detached(context.Background())
	time.Sleep(40 * time.Millisecond)
	fmt.Println("leaks from unbounded detached:", len(d.Check()))
}

// audit mirrors detachedAudit for the demo binary.
func audit(ctx context.Context) string {
	select {
	case <-time.After(30 * time.Millisecond):
		return "written"
	case <-ctx.Done():
		return "aborted"
	}
}
```

Run it:

```bash
go run ./cmd/demo
```

Expected output:

```
audit result: written
leaks from unbounded detached: 1
```

### Tests

Create `detached_test.go`:

```go
package detached

import (
	"context"
	"errors"
	"testing"
	"time"
)

func TestWithoutCancelDetachesButOwnTimeoutCloses(t *testing.T) {
	t.Parallel()

	d := New(time.Second)
	parent, cancelParent := context.WithCancel(context.Background())
	ctx, cancel := d.BoundedDetached(parent, 40*time.Millisecond)
	defer cancel()

	cancelParent() // parent cancellation must NOT reach the detached context

	select {
	case <-ctx.Done():
		t.Fatal("parent cancellation closed the detached context; WithoutCancel should prevent it")
	case <-time.After(10 * time.Millisecond):
		// good: still alive despite parent cancel
	}

	select {
	case <-ctx.Done(): // its own timeout DOES close it
	case <-time.After(200 * time.Millisecond):
		t.Fatal("detached context never closed on its own timeout")
	}
	if got := ctx.Err(); !errors.Is(got, context.DeadlineExceeded) {
		t.Errorf("ctx.Err() = %v, want DeadlineExceeded", got)
	}
}

func TestBoundedDetachedDoesNotLeak(t *testing.T) {
	t.Parallel()

	d := New(10 * time.Millisecond)
	parent, cancelParent := context.WithCancel(context.Background())
	_, cancel := d.BoundedDetached(parent, 20*time.Millisecond)
	defer cancel()

	cancelParent()
	time.Sleep(70 * time.Millisecond) // past the detached timeout + grace + drain
	if leaks := d.Check(); len(leaks) != 0 {
		t.Fatalf("bounded detached leaked: %v", leaks)
	}
}

func TestUnboundedDetachedLeaks(t *testing.T) {
	t.Parallel()

	d := New(10 * time.Millisecond)
	parent, cancelParent := context.WithCancel(context.Background())
	_ = d.Detached(parent) // no timeout: nothing will ever clear it

	cancelParent() // even cancelling the parent cannot clear a detached context
	time.Sleep(40 * time.Millisecond)
	if leaks := d.Check(); len(leaks) != 1 {
		t.Fatalf("unbounded detached should leak exactly 1: got %d", len(leaks))
	}
}

func TestAuditSurvivesRequestCancellation(t *testing.T) {
	t.Parallel()

	d := New(time.Second)
	req, cancelReq := context.WithCancel(context.Background()) // stands in for r.Context()
	ctx, cancel := d.BoundedDetached(req, 200*time.Millisecond)
	defer cancel()

	result := make(chan string, 1)
	go detachedAudit(ctx, result)

	cancelReq() // client disconnects immediately

	if got := <-result; got != "written" {
		t.Fatalf("audit result = %q, want %q (request cancel must not abort detached work)", got, "written")
	}
}
```

## Review

The module is correct when parent cancellation does not close the detached context
(the `WithoutCancel` guarantee) but its own timeout does, when the bounded detached
context leaves zero leaks and the unbounded one leaves exactly one, and when the
audit write completes even though the request was cancelled before it finished.
`TestUnboundedDetachedLeaks` is the point of the whole exercise: `Detached`
registers a context whose `Done()` is `nil`, so its `AfterFunc` never fires and the
detector reports a permanent leak — the runtime proof that detaching without
re-bounding is a bug. `TestAuditSurvivesRequestCancellation` is the reason to reach
for `WithoutCancel` at all. Run `-race`: the audit goroutine and the test's cancel
run concurrently against the same context and detector.

## Resources

- [context.WithoutCancel](https://pkg.go.dev/context#WithoutCancel) — the detach semantics: `Done()` nil, no deadline, values preserved.
- [context.WithTimeout](https://pkg.go.dev/context#WithTimeout) — the fresh deadline that re-bounds the detached work.
- [Go 1.21 release notes: context](https://go.dev/doc/go1.21#context) — where `WithoutCancel`, `AfterFunc`, and the cause APIs landed.

---

Back to [09-build-tag-shim.md](09-build-tag-shim.md) | Next: [../14-building-a-context-aware-service-framework/00-concepts.md](../14-building-a-context-aware-service-framework/00-concepts.md)
