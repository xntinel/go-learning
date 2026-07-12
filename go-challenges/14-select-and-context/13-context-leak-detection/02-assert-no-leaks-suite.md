# Exercise 2: AssertNoLeaks Test Helper And Contract Suite

A detector is only trustworthy if its behavior is pinned by tests. This module
ships the `AssertNoLeaks` helper a team drops into any test, plus the full
behavioral contract the detector must satisfy: cancelled contexts are not leaked,
uncancelled ones are, parent cancellation clears a child, a timeout clears on
expiry, `ActiveContexts` is exact at every stage, and leak reports carry usable
caller data.

This module is fully self-contained: its own `go mod init`, all code inline, its
own demo and tests. Nothing here imports another exercise.

## What you'll build

```text
leakdetect/                        module example.com/leakdetect
  go.mod
  internal/leakdetect/
    leakdetect.go                  the detector (same core as Exercise 1)
    leakdetect_test.go             AssertNoLeaks + the full contract suite
  cmd/
    demo/
      main.go                      leak vs clean, then AssertNoLeaks-style check
```

Files: `internal/leakdetect/leakdetect.go`, `internal/leakdetect/leakdetect_test.go`, `cmd/demo/main.go`.
Implement: `AssertNoLeaks(t testing.TB, d *Detector)` that drains `AfterFunc` callbacks then fails once per surviving leak.
Test: cancelled-not-leaked, uncancelled-is-leaked (the canary), parent-cancel-clears-child, timeout-clears-on-expiry, `ActiveContexts` pinned at each stage, `TotalCreated` across all three constructors, report carries caller and age, `AssertNoLeaks` passes on clean code, and the deadline-expiry test.
Verify: `go test -count=1 -race ./...`

Set up the module:

```bash
mkdir -p go-solutions/14-select-and-context/13-context-leak-detection/02-assert-no-leaks-suite/internal/leakdetect
mkdir -p go-solutions/14-select-and-context/13-context-leak-detection/02-assert-no-leaks-suite/cmd/demo
cd go-solutions/14-select-and-context/13-context-leak-detection/02-assert-no-leaks-suite
```

### Why AssertNoLeaks sleeps before it checks

`AssertNoLeaks` is the ergonomic front door: any test can call it and get a
per-leak failure with a call site attached. Its one subtlety is timing. The
detector deregisters a context from two places — the eager cancel closure and the
`AfterFunc` callback that runs in its own goroutine — and the callback goroutine
may not have been scheduled yet at the moment a test calls `AssertNoLeaks`. So the
helper sleeps first, giving pending callbacks time to drain, and only then calls
`Check`. The sleep is generous relative to the detector's grace period so the
suite does not flake under `-race`, where goroutine scheduling is slower. This is
the concrete form of the "check after the grace period, not before" rule: a
correctly-cancelled context must never be falsely accused.

`TestUnCancelledContextIsLeaked` is the canary. A context whose cancel is never
called must appear in `Check()` after the grace period, with a non-empty caller.
If that test ever passes with zero leaks, the registry has stopped tracking and
the whole tool is blind. `TestParentCancellationClearsChild` proves the
`AfterFunc` path specifically: the child's own cancel is never invoked, yet
cancelling the *parent* removes the child, because the child becomes `Done` when
the parent does. `TestDeadlineContextClearsOnExpiry` proves the timer path: an
expired deadline context is cleared by `AfterFunc` with no cancel call at all.

Create `internal/leakdetect/leakdetect.go` (the same detector as Exercise 1):

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

type record struct {
	createdAt time.Time
	caller    string
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
// that has not yet become Done. Safe for concurrent use.
type Detector struct {
	mu          sync.Mutex
	records     map[*record]struct{}
	gracePeriod time.Duration
	created     atomic.Int64
}

// New returns a Detector reporting contexts outstanding past gracePeriod.
func New(gracePeriod time.Duration) *Detector {
	return &Detector{
		records:     make(map[*record]struct{}),
		gracePeriod: gracePeriod,
	}
}

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
	delete(d.records, r)
	d.mu.Unlock()
}

func (d *Detector) track(ctx context.Context, cancel context.CancelFunc) (context.Context, context.CancelFunc) {
	r := d.register(4) // 0 callerInfo,1 register,2 track,3 wrapper,4 user code
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

// Check returns one LeakReport per context outstanding past the grace period.
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

	ctx, cancel := d.WithCancel(context.Background())
	_ = ctx
	cancel() // clean path

	_, leaked := d.WithCancel(context.Background()) // leaked path

	time.Sleep(30 * time.Millisecond)
	leaks := d.Check()
	fmt.Println("leaks detected:", len(leaks))
	for _, l := range leaks {
		fmt.Println("  -", l.Caller)
	}

	leaked()
	time.Sleep(20 * time.Millisecond)
	fmt.Println("active after cleanup:", d.ActiveContexts())
}
```

Run it:

```bash
go run ./cmd/demo
```

Expected output:

```
leaks detected: 1
  - main.go:18
active after cleanup: 0
```

### Tests

Create `internal/leakdetect/leakdetect_test.go`:

```go
package leakdetect

import (
	"context"
	"testing"
	"time"
)

// AssertNoLeaks drains AfterFunc callbacks, then fails once per surviving leak.
func AssertNoLeaks(t testing.TB, d *Detector) {
	t.Helper()
	time.Sleep(30 * time.Millisecond) // let AfterFunc callbacks run
	for _, leak := range d.Check() {
		t.Errorf("%s", leak)
	}
}

func TestCancelledContextIsNotLeaked(t *testing.T) {
	t.Parallel()

	d := New(10 * time.Millisecond)
	_, cancel := d.WithCancel(context.Background())
	cancel()

	time.Sleep(40 * time.Millisecond)
	if leaks := d.Check(); len(leaks) != 0 {
		t.Fatalf("got %d leaks, want 0: %v", len(leaks), leaks)
	}
}

// TestUnCancelledContextIsLeaked is the canary: if this passes with 0 leaks the
// registry is not tracking contexts at all.
func TestUnCancelledContextIsLeaked(t *testing.T) {
	t.Parallel()

	d := New(10 * time.Millisecond)
	_, cancel := d.WithCancel(context.Background())
	defer cancel() // clean up only after the assertion

	time.Sleep(40 * time.Millisecond)
	leaks := d.Check()
	if len(leaks) != 1 {
		t.Fatalf("got %d leaks, want 1", len(leaks))
	}
	if leaks[0].Caller == "" {
		t.Fatal("leak report has empty caller")
	}
}

func TestParentCancellationClearsChild(t *testing.T) {
	t.Parallel()

	d := New(10 * time.Millisecond)
	parent, parentCancel := context.WithCancel(context.Background())
	_, _ = d.WithCancel(parent) // child cancel never called

	parentCancel() // cancelling the parent must clear the child via AfterFunc

	time.Sleep(50 * time.Millisecond)
	if leaks := d.Check(); len(leaks) != 0 {
		t.Fatalf("parent cancel should clear child: got %d leaks", len(leaks))
	}
}

func TestTimeoutContextClearsOnExpiry(t *testing.T) {
	t.Parallel()

	d := New(10 * time.Millisecond)
	_, cancel := d.WithTimeout(context.Background(), 20*time.Millisecond)
	defer cancel()

	time.Sleep(70 * time.Millisecond) // let the timeout fire and AfterFunc run
	if leaks := d.Check(); len(leaks) != 0 {
		t.Fatalf("timed-out context should be cleared: got %d leaks", len(leaks))
	}
}

// TestDeadlineContextClearsOnExpiry mirrors the timeout case for WithDeadline:
// no cancel is ever called, yet AfterFunc clears the expired deadline context.
func TestDeadlineContextClearsOnExpiry(t *testing.T) {
	t.Parallel()

	d := New(10 * time.Millisecond)
	_, cancel := d.WithDeadline(context.Background(), time.Now().Add(20*time.Millisecond))
	defer cancel()

	time.Sleep(70 * time.Millisecond)
	if leaks := d.Check(); len(leaks) != 0 {
		t.Fatalf("expired deadline context should be cleared: got %d leaks", len(leaks))
	}
}

func TestActiveContextsCount(t *testing.T) {
	t.Parallel()

	d := New(time.Second) // long grace: nothing ages out during the test
	if d.ActiveContexts() != 0 {
		t.Fatal("expected 0 active before any create")
	}

	_, c1 := d.WithCancel(context.Background())
	_, c2 := d.WithCancel(context.Background())
	if got := d.ActiveContexts(); got != 2 {
		t.Fatalf("ActiveContexts = %d, want 2", got)
	}

	c1()
	if got := d.ActiveContexts(); got != 1 {
		t.Fatalf("ActiveContexts after one cancel = %d, want 1", got)
	}

	c2()
	if got := d.ActiveContexts(); got != 0 {
		t.Fatalf("ActiveContexts after both cancel = %d, want 0", got)
	}
}

func TestTotalCreated(t *testing.T) {
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

func TestLeakReportContainsCallerInfo(t *testing.T) {
	t.Parallel()

	d := New(10 * time.Millisecond)
	_, cancel := d.WithCancel(context.Background())
	defer cancel()

	time.Sleep(40 * time.Millisecond)
	leaks := d.Check()
	if len(leaks) == 0 {
		t.Fatal("expected at least one leak report")
	}
	r := leaks[0]
	if r.Caller == "" {
		t.Error("Caller is empty")
	}
	if r.FuncName == "" {
		t.Error("FuncName is empty")
	}
	if r.Age < 10*time.Millisecond {
		t.Errorf("Age = %v, want >= 10ms", r.Age)
	}
}

func TestAssertNoLeaksPassesOnCleanCode(t *testing.T) {
	t.Parallel()

	d := New(10 * time.Millisecond)
	_, cancel := d.WithCancel(context.Background())
	cancel()
	AssertNoLeaks(t, d)
}
```

Note the two eager-cancel counts. `TestActiveContextsCount` reads
`ActiveContexts` with no sleep after `c1()`/`c2()` and still expects the exact
count, which works only because the cancel closure deregisters *eagerly* rather
than waiting for the `AfterFunc` goroutine. The parent-cancellation and expiry
tests, by contrast, have no eager path — nothing calls the child's cancel — so
they must sleep to let `AfterFunc` run.

## Review

This module is the contract. `TestUnCancelledContextIsLeaked` must stay a canary:
if it ever reports zero leaks, tracking is broken. The three "clears" tests
(cancel, parent-cancel, deadline-expiry) each exercise a distinct removal path, so
a regression in any one is isolated. `AssertNoLeaks` sleeping before `Check` is
not a hack around flakiness — it is the correct expression of the grace-period
rule, and shortening that sleep to "speed up the suite" is the fastest way to a
flaky false positive under `-race`. Run `go test -race`: the map is mutated
concurrently by cancel closures and `AfterFunc` goroutines across every parallel
test, so an unlocked access surfaces here.

## Resources

- [context.AfterFunc](https://pkg.go.dev/context#AfterFunc) — why the deregistration is asynchronous and must be drained before asserting.
- [testing.TB](https://pkg.go.dev/testing#TB) — the interface that lets `AssertNoLeaks` accept both `*testing.T` and `*testing.B`.
- [context.WithDeadline](https://pkg.go.dev/context#WithDeadline) — the timer-backed constructor exercised by the expiry test.

---

Back to [01-detector-core.md](01-detector-core.md) | Next: [03-godoc-example.md](03-godoc-example.md)
