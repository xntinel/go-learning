# Exercise 1: The Instrumented Leak Detector

The core of the whole lesson is a `Detector` that wraps the three cancellable
context constructors, records the call site of every outstanding context, and
auto-deregisters each one the instant it becomes `Done`. This module builds that
detector and, critically, tests the one thing that is silently easy to get wrong:
the `runtime.Caller` skip depth that decides whether a leak report names your
handler or names the detector's own source.

This module is fully self-contained: its own `go mod init`, all code inline, its
own demo and tests. Nothing here imports another exercise.

## What you'll build

```text
leakdetect/                        module example.com/leakdetect
  go.mod
  internal/leakdetect/
    leakdetect.go                  Detector: WithCancel/WithTimeout/WithDeadline, Check, ActiveContexts, TotalCreated
    leakdetect_test.go             skip-depth test (report names user code), lifecycle, Example
  cmd/
    demo/
      main.go                      creates + cancels contexts, prints counts
```

Files: `internal/leakdetect/leakdetect.go`, `internal/leakdetect/leakdetect_test.go`, `cmd/demo/main.go`.
Implement: `Detector` with `New(grace)`, `WithCancel/WithTimeout/WithDeadline`, `Check() []LeakReport`, `ActiveContexts() int`, `TotalCreated() int64`, capturing the caller via `runtime.Caller` and auto-deregistering via `context.AfterFunc`.
Test: a report from a leaked context names the user's file (not `leakdetect.go`), a cancelled context is cleared, `TotalCreated` counts across all three constructors, and an `Example`.
Verify: `go test -count=1 -race ./...`

### The skip depth is the whole game

The wrappers exist to add exactly one piece of information the standard
constructors do not have: *where was this context created?* That answer comes from
`runtime.Caller(skip)`, and the `skip` count is measured from the frame that calls
`runtime.Caller`. Frame counting is exact and unforgiving: every function the call
threads through before reaching `runtime.Caller` is one more frame to skip. In the
code below all three constructors share a `track` helper, so inside `callerInfo`
the frames stack up like this: 0 is `callerInfo` itself, 1 is `register` (which
called `callerInfo`), 2 is `track` (which called `register`), 3 is the `WithX`
wrapper (which called `track`), and 4 is the user's handler (which called `WithX`).
So to name user code the wrapper chain passes `register(4)`, and `register` threads
that straight through to `runtime.Caller`. Had the wrappers each called `register`
directly with no `track` frame, the answer would be `register(3)` — the shared
helper is precisely what makes it 4. A detector built one frame short
compiles cleanly and reports `leakdetect.go` on every single leak — a tool
that is worse than useless because it looks like it works. That is why this module
leads with `TestReportNamesUserCode`, which asserts the captured caller string
contains the *test* file's name. Inlining can shift frame counts between Go
versions, and this test is what catches such a shift.

### Auto-deregistration via AfterFunc, and why cancel also deregisters

Each wrapper registers a `record` and then calls `context.AfterFunc(ctx,
deregister)`. `AfterFunc` runs its callback in a fresh goroutine the moment `ctx`
becomes `Done`, which covers every removal path uniformly: the child's own cancel,
the parent being cancelled, or a timer firing. The returned cancel closure also
calls `deregister` directly so that `ActiveContexts` drops the instant you cancel,
without waiting for the callback goroutine to be scheduled. Both paths deleting the
same key is safe because `delete` on a missing key is a no-op — that idempotency is
exactly why the code does not need to keep the `stop` function `AfterFunc` returns.

Create `internal/leakdetect/leakdetect.go`. Note `register(3)` — the skip depth
that makes the report point at user code:

```go
// internal/leakdetect/leakdetect.go
package leakdetect

import (
	"context"
	"fmt"
	"path/filepath"
	"runtime"
	"sync"
	"sync/atomic"
	"time"
)

// record holds the metadata for one outstanding context.
type record struct {
	createdAt time.Time
	caller    string // "handler.go:42"
	funcName  string
}

// LeakReport describes one context still outstanding past the grace period.
type LeakReport struct {
	Caller   string
	FuncName string
	Age      time.Duration
}

func (r LeakReport) String() string {
	return fmt.Sprintf("context leak: created at %s (%s), age %v", r.Caller, r.FuncName, r.Age)
}

// Detector wraps the cancellable context constructors and tracks every context
// that has not yet become Done. It is safe for concurrent use.
type Detector struct {
	mu          sync.Mutex
	records     map[*record]struct{}
	gracePeriod time.Duration
	created     atomic.Int64
}

// New returns a Detector. A context still outstanding past gracePeriod is
// reported by Check; a shorter grace period reports leaks sooner but risks
// flagging a cancel that is about to run.
func New(gracePeriod time.Duration) *Detector {
	return &Detector{
		records:     make(map[*record]struct{}),
		gracePeriod: gracePeriod,
	}
}

// callerInfo returns "file.go:line" and the function name skip frames above the
// call to callerInfo itself.
func callerInfo(skip int) (location, funcName string) {
	pc, file, line, ok := runtime.Caller(skip)
	if !ok {
		return "unknown:0", "unknown"
	}
	name := "unknown"
	if fn := runtime.FuncForPC(pc); fn != nil {
		name = fn.Name()
	}
	return fmt.Sprintf("%s:%d", filepath.Base(file), line), name
}

func (d *Detector) register(skip int) *record {
	loc, fn := callerInfo(skip)
	r := &record{createdAt: time.Now(), caller: loc, funcName: fn}
	d.mu.Lock()
	d.records[r] = struct{}{}
	d.mu.Unlock()
	d.created.Add(1)
	return r
}

func (d *Detector) deregister(r *record) {
	d.mu.Lock()
	delete(d.records, r) // delete on a missing key is a no-op: idempotent
	d.mu.Unlock()
}

// track wires AfterFunc auto-deregistration and returns a cancel closure that
// also deregisters eagerly. The frames above callerInfo are: 0 callerInfo,
// 1 register, 2 track, 3 the WithX wrapper, 4 the user's handler; skip=4 names
// user code.
func (d *Detector) track(ctx context.Context, cancel context.CancelFunc) (context.Context, context.CancelFunc) {
	r := d.register(4)
	context.AfterFunc(ctx, func() { d.deregister(r) })
	return ctx, func() {
		cancel()
		d.deregister(r)
	}
}

// WithCancel wraps context.WithCancel.
func (d *Detector) WithCancel(parent context.Context) (context.Context, context.CancelFunc) {
	ctx, cancel := context.WithCancel(parent)
	return d.track(ctx, cancel)
}

// WithTimeout wraps context.WithTimeout.
func (d *Detector) WithTimeout(parent context.Context, timeout time.Duration) (context.Context, context.CancelFunc) {
	ctx, cancel := context.WithTimeout(parent, timeout)
	return d.track(ctx, cancel)
}

// WithDeadline wraps context.WithDeadline.
func (d *Detector) WithDeadline(parent context.Context, deadline time.Time) (context.Context, context.CancelFunc) {
	ctx, cancel := context.WithDeadline(parent, deadline)
	return d.track(ctx, cancel)
}

// Check returns one LeakReport for every context still outstanding past the
// grace period.
func (d *Detector) Check() []LeakReport {
	now := time.Now()
	d.mu.Lock()
	defer d.mu.Unlock()
	var out []LeakReport
	for r := range d.records {
		if age := now.Sub(r.createdAt); age >= d.gracePeriod {
			out = append(out, LeakReport{Caller: r.caller, FuncName: r.funcName, Age: age})
		}
	}
	return out
}

// ActiveContexts returns the number of currently outstanding contexts.
func (d *Detector) ActiveContexts() int {
	d.mu.Lock()
	defer d.mu.Unlock()
	return len(d.records)
}

// TotalCreated returns the number of contexts ever created through this detector.
func (d *Detector) TotalCreated() int64 {
	return d.created.Load()
}
```

The `track` helper is the reason the skip is `4` and not `3`: routing all three
wrappers through one place adds a frame, so the stack above `callerInfo` is 0
`callerInfo`, 1 `register`, 2 `track`, 3 the `WithX` wrapper, 4 the user's
handler. The test below pins this exact value, which is the only reliable way to
know it — inlining can move frames between compiler versions, so a leak tool that
does not test its own skip depth silently rots into naming its own source file.

### The runnable demo

Create `cmd/demo/main.go`:

```go
package main

import (
	"context"
	"fmt"
	"time"

	"example.com/leakdetect/internal/leakdetect"
)

func main() {
	d := leakdetect.New(10 * time.Millisecond)

	// A correctly-scoped context: cancel runs, node is cleared.
	ctx, cancel := d.WithCancel(context.Background())
	_ = ctx
	cancel()

	// A leaked context: cancel is captured but never called here.
	_, leaked := d.WithTimeout(context.Background(), time.Minute)

	fmt.Println("total created:", d.TotalCreated())
	time.Sleep(30 * time.Millisecond) // let the grace period pass
	fmt.Println("active contexts:", d.ActiveContexts())

	leaks := d.Check()
	fmt.Println("leaks detected:", len(leaks))

	leaked() // clean up before exit
	time.Sleep(20 * time.Millisecond)
	fmt.Println("active after cancel:", d.ActiveContexts())
}
```

Run it:

```bash
go run ./cmd/demo
```

Expected output:

```
total created: 2
active contexts: 1
leaks detected: 1
active after cancel: 0
```

### Tests

Create `internal/leakdetect/leakdetect_test.go`:

```go
package leakdetect

import (
	"context"
	"fmt"
	"strings"
	"testing"
	"time"
)

// TestReportNamesUserCode is the canary: a leak must point at THIS test file,
// not at leakdetect.go. If it fails, the runtime.Caller skip depth is wrong.
func TestReportNamesUserCode(t *testing.T) {
	t.Parallel()

	d := New(5 * time.Millisecond)
	_, cancel := d.WithCancel(context.Background())
	defer cancel() // clean up only after the assertion

	time.Sleep(20 * time.Millisecond)
	leaks := d.Check()
	if len(leaks) != 1 {
		t.Fatalf("Check() returned %d leaks, want 1", len(leaks))
	}
	if !strings.HasPrefix(leaks[0].Caller, "leakdetect_test.go:") {
		t.Fatalf("report names %q, want the user's test file (skip depth is wrong)", leaks[0].Caller)
	}
	if leaks[0].FuncName == "" {
		t.Error("FuncName is empty")
	}
	if leaks[0].Age < 5*time.Millisecond {
		t.Errorf("Age = %v, want >= 5ms", leaks[0].Age)
	}
}

func TestCancelledContextIsCleared(t *testing.T) {
	t.Parallel()

	d := New(5 * time.Millisecond)
	_, cancel := d.WithCancel(context.Background())
	cancel()

	time.Sleep(20 * time.Millisecond)
	if leaks := d.Check(); len(leaks) != 0 {
		t.Fatalf("cancelled context reported as leaked: %v", leaks)
	}
	if n := d.ActiveContexts(); n != 0 {
		t.Fatalf("ActiveContexts = %d after cancel, want 0", n)
	}
}

func TestTotalCreatedCountsAllConstructors(t *testing.T) {
	t.Parallel()

	d := New(time.Second)
	_, c1 := d.WithCancel(context.Background())
	_, c2 := d.WithTimeout(context.Background(), time.Second)
	_, c3 := d.WithDeadline(context.Background(), time.Now().Add(time.Second))
	c1()
	c2()
	c3()

	if got := d.TotalCreated(); got != 3 {
		t.Fatalf("TotalCreated = %d, want 3", got)
	}
}

func Example() {
	d := New(5 * time.Millisecond)
	ctx, cancel := d.WithCancel(context.Background())
	_ = ctx
	cancel()

	time.Sleep(20 * time.Millisecond)
	fmt.Println("leaks:", len(d.Check()))
	// Output: leaks: 0
}
```

## Review

The detector is correct when three properties hold. First, a report names user
code: `TestReportNamesUserCode` fails loudly if the skip depth drifts, which is the
single most common way this kind of tool rots. Second, a cancelled context leaves
no trace: both `Check` and `ActiveContexts` return to zero, proving the
`AfterFunc` and eager-`deregister` paths agree and that double-deletion is safe.
Third, `TotalCreated` counts every constructor. Run `go test -race`: the map is
mutated from the `AfterFunc` callback goroutine and from the cancel closure at
once, so a missing lock would surface here. The skip-depth comment in the code is
deliberate — because `track` adds a frame, verify the exact number against the
canary test rather than trusting the arithmetic.

## Resources

- [context package godoc](https://pkg.go.dev/context) — `WithCancel`, `WithTimeout`, `WithDeadline`, and the cancel-leak warning.
- [context.AfterFunc](https://pkg.go.dev/context#AfterFunc) — the deregistration primitive and its `stop` return.
- [runtime.Caller](https://pkg.go.dev/runtime#Caller) and [runtime.FuncForPC](https://pkg.go.dev/runtime#FuncForPC) — capturing the allocation site.

---

Back to [00-concepts.md](00-concepts.md) | Next: [02-assert-no-leaks-suite.md](02-assert-no-leaks-suite.md)
