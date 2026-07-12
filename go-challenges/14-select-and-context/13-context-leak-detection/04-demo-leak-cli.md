# Exercise 4: A Demo CLI That Surfaces A Real Leak

A demo that a human eyeballs is not a test — it does not fail when the code
regresses. This module builds a small CLI that contrasts a correct code path
against a leaky one, and then guards the demo itself with a test on a `run()`
function, so "the leak is detected and named `leakyCode`" is asserted by
`go test`, not by a reader squinting at output.

This module is fully self-contained: its own `go mod init`, all code inline, its
own demo and tests. Nothing here imports another exercise.

## What you'll build

```text
leakdetect/                        module example.com/leakdetect
  go.mod
  internal/leakdetect/
    leakdetect.go                  the detector (same core as Exercise 1)
  cmd/
    demo/
      main.go                      goodCode vs leakyCode; run() returns a Result
      main_test.go                 asserts run() finds exactly 1 leak, named leakyCode
```

Files: `internal/leakdetect/leakdetect.go`, `cmd/demo/main.go`, `cmd/demo/main_test.go`.
Implement: `goodCode` (defer cancel), `leakyCode` (returns the cancel instead of deferring), and a `run(d)` that returns a `Result` with the counts and reports.
Test: `run` reports `TotalCreated == 2`, exactly one leak whose `FuncName` contains `leakyCode`, and `ActiveContexts == 0` after the returned cancel is called.
Verify: `go test -count=1 -race ./...`

Set up the module:

```bash
mkdir -p go-solutions/14-select-and-context/13-context-leak-detection/04-demo-leak-cli/internal/leakdetect
mkdir -p go-solutions/14-select-and-context/13-context-leak-detection/04-demo-leak-cli/cmd/demo
cd go-solutions/14-select-and-context/13-context-leak-detection/04-demo-leak-cli
```

### Why the logic lives in run(), not main()

`main()` is unreachable from a test: it takes no arguments, returns nothing, and
writes to stdout. So the demo's actual logic goes in `run(d *leakdetect.Detector)
Result`, a pure-ish function that returns a struct of the numbers a test wants to
assert, and `main()` shrinks to "build a detector, call `run`, print the fields".
This is the standard shape for a testable CLI: `main` does I/O and wiring, `run`
does work and returns data. The demo is now regression-guarded — if a future edit
breaks leak detection, `main_test.go` fails in CI instead of a human noticing the
output looks wrong.

The two code paths are the whole point. `goodCode` derives a context and
`defer cancel()`s it, so by the time it returns the node is already gone.
`leakyCode` derives a `WithTimeout` context and *returns* the cancel to its
caller — the classic real-world leak, where a helper hands back a `CancelFunc` and
the caller forgets it. Because the detector captured the call site with the right
skip depth, the leak report's `FuncName` names `leakyCode`, which is exactly what
an on-call engineer needs to jump straight to the offending function.

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

// goodCode derives a context and cancels it before returning: no leak.
func goodCode(d *leakdetect.Detector) {
	ctx, cancel := d.WithCancel(context.Background())
	defer cancel()
	_ = ctx
}

// leakyCode returns the cancel func instead of deferring it. The caller must
// remember to call it; if they drop it, the context leaks.
func leakyCode(d *leakdetect.Detector) context.CancelFunc {
	_, cancel := d.WithTimeout(context.Background(), time.Minute)
	return cancel
}

// Result carries what the demo observed, so a test can assert on it.
type Result struct {
	TotalCreated int64
	ActiveBefore int
	Leaks        []leakdetect.LeakReport
	ActiveAfter  int
}

func run(d *leakdetect.Detector) Result {
	goodCode(d)            // properly cancelled
	cancel := leakyCode(d) // cancel returned, not yet called

	total := d.TotalCreated()
	before := d.ActiveContexts()

	time.Sleep(40 * time.Millisecond) // let the grace period pass
	leaks := d.Check()

	cancel() // clean up the leaked context
	time.Sleep(20 * time.Millisecond)

	return Result{
		TotalCreated: total,
		ActiveBefore: before,
		Leaks:        leaks,
		ActiveAfter:  d.ActiveContexts(),
	}
}

func main() {
	d := leakdetect.New(20 * time.Millisecond)
	r := run(d)

	fmt.Println("total created:", r.TotalCreated)
	fmt.Println("active before grace:", r.ActiveBefore)
	fmt.Printf("leaks detected: %d\n", len(r.Leaks))
	for _, l := range r.Leaks {
		fmt.Println(" ", l)
	}
	fmt.Println("active after cancel:", r.ActiveAfter)
}
```

Run it:

```bash
go run ./cmd/demo
```

Expected output (the age varies slightly run to run):

```
total created: 2
active before grace: 1
leaks detected: 1
  context leak: created at main.go:21 (main.leakyCode), age 40.384834ms
active after cancel: 0
```

### Tests

Create `cmd/demo/main_test.go`:

```go
package main

import (
	"strings"
	"testing"
	"time"

	"example.com/leakdetect/internal/leakdetect"
)

func TestRunSurfacesTheLeak(t *testing.T) {
	d := leakdetect.New(20 * time.Millisecond)
	r := run(d)

	if r.TotalCreated != 2 {
		t.Errorf("TotalCreated = %d, want 2", r.TotalCreated)
	}
	if r.ActiveBefore != 1 {
		t.Errorf("ActiveBefore = %d, want 1 (only the leaked context)", r.ActiveBefore)
	}
	if len(r.Leaks) != 1 {
		t.Fatalf("Leaks = %d, want exactly 1", len(r.Leaks))
	}
	if !strings.Contains(r.Leaks[0].FuncName, "leakyCode") {
		t.Errorf("leak names %q, want it to contain leakyCode", r.Leaks[0].FuncName)
	}
	if r.ActiveAfter != 0 {
		t.Errorf("ActiveAfter = %d, want 0 after cancel", r.ActiveAfter)
	}
}
```

## Review

The demo is correct when `run` returns two created contexts, one active before the
grace period (the good one already cancelled), exactly one leak named `leakyCode`,
and zero active after cleanup. The design lesson is that pushing logic out of
`main` into a `run` that returns data is what makes a CLI testable at all — a demo
whose only output is `fmt.Println` can never fail a regression. `TestRunSurfacesTheLeak`
is the guard: if the skip depth breaks, the `FuncName` assertion fails; if
tracking breaks, the leak count fails. The `main.go:21` line in the expected output
is the `d.WithTimeout` call inside `leakyCode`, which is precisely the site an
operator wants to see.

## Resources

- [Testing a main package](https://pkg.go.dev/testing) — why extracting a `run` function from `main` makes the program testable.
- [runtime.FuncForPC](https://pkg.go.dev/runtime#FuncForPC) — how the leak report resolves a program counter to `main.leakyCode`.
- [Command Go: build and run](https://pkg.go.dev/cmd/go) — `go run ./cmd/demo` and package `main` conventions.

---

Back to [03-godoc-example.md](03-godoc-example.md) | Next: [05-goroutine-leak-guard.md](05-goroutine-leak-guard.md)
