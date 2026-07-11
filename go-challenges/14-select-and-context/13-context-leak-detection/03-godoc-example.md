# Exercise 3: Runnable Godoc Examples For The Detector

A godoc `Example` is documentation that cannot rot: `go test` runs it and diffs
its stdout against the `// Output:` comment, so a doc that drifts from the code
fails the build. This module writes the detector's public examples from an
*external* test package, which both renders them in godoc and proves the exported
API is usable from outside the package that defines it.

This module is fully self-contained: its own `go mod init`, all code inline, its
own demo and tests. Nothing here imports another exercise.

## What you'll build

```text
leakdetect/                        module example.com/leakdetect
  go.mod
  internal/leakdetect/
    leakdetect.go                  the detector (same core as Exercise 1)
  example_test.go                  package leakdetect_test: runnable // Output examples
  cmd/
    demo/
      main.go                      mirrors the example against the wall clock
```

Files: `internal/leakdetect/leakdetect.go`, `example_test.go`, `cmd/demo/main.go`.
Implement: nothing new in the detector; the deliverable is the examples.
Test: `ExampleDetector_WithCancel` asserts `leaks after cancel: 0`; `ExampleDetector_leak` asserts a dropped cancel reports `1`, both via `// Output:`.
Verify: `go test -count=1 -race ./...`

Set up the module:

```bash
mkdir -p ~/go-exercises/leakdetect/internal/leakdetect
mkdir -p ~/go-exercises/leakdetect/cmd/demo
cd ~/go-exercises/leakdetect
go mod init example.com/leakdetect
```

### Why the example lives in package leakdetect_test

A test file declared `package leakdetect_test` (note the `_test` suffix on the
package name, in the same directory) compiles as a *separate* package that can
only reach the detector's exported identifiers, exactly as a real caller would. If
the example compiles and its `// Output:` matches, the exported surface —
`New`, `WithCancel`, `Check` — is genuinely usable from the outside, not just from
privileged in-package tests. Here the example imports the package by its full
module path, `example.com/leakdetect/internal/leakdetect`, which also demonstrates
that `internal/` packages are importable from anywhere *within the same module*
(the `internal` visibility rule only blocks imports from other modules).

The two examples are a matched pair. `ExampleDetector_WithCancel` is the good
path: derive a context, always call `cancel`, wait past the grace period, and
assert zero leaks — the shape every handler should follow. `ExampleDetector_leak`
is the counterexample: drop the cancel on the floor and watch `Check` report one
leak. Because both are verified by `// Output:`, the lesson's central claim —
"cancel clears, dropping leaks" — is executable documentation, not prose.

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

func main() {
	d := leakdetect.New(10 * time.Millisecond)

	ctx, cancel := d.WithCancel(context.Background())
	_ = ctx
	cancel()

	time.Sleep(30 * time.Millisecond)
	fmt.Println("leaks after cancel:", len(d.Check()))
}
```

Run it:

```bash
go run ./cmd/demo
```

Expected output:

```
leaks after cancel: 0
```

### Tests

Create `example_test.go` at the module root:

```go
package leakdetect_test

import (
	"context"
	"fmt"
	"time"

	"example.com/leakdetect/internal/leakdetect"
)

func ExampleDetector_WithCancel() {
	d := leakdetect.New(10 * time.Millisecond)
	ctx, cancel := d.WithCancel(context.Background())
	_ = ctx
	cancel() // always call cancel

	time.Sleep(30 * time.Millisecond) // let the grace period pass
	fmt.Println("leaks after cancel:", len(d.Check()))
	// Output: leaks after cancel: 0
}

func ExampleDetector_leak() {
	d := leakdetect.New(10 * time.Millisecond)
	_, cancel := d.WithCancel(context.Background())
	defer cancel() // cleanup AFTER the check, to leave the leak visible

	time.Sleep(30 * time.Millisecond)
	fmt.Println("leaks after dropping cancel:", len(d.Check()))
	// Output: leaks after dropping cancel: 1
}
```

## Review

The examples are correct when `go test` runs them and their `// Output:` blocks
match. The value beyond documentation is structural: because `example_test.go` is
`package leakdetect_test`, a compile failure here means the exported API is not
usable from outside the package — a real regression that in-package tests would
hide. The paired examples encode the lesson in executable form: cancel yields
zero, dropping cancel yields one. If either example's output has to be edited to
make the test pass, the detector's behavior changed and the change was probably a
bug.

## Resources

- [Go testable examples](https://go.dev/blog/examples) — how `Example` functions and `// Output:` comments are compiled, run, and rendered in godoc.
- [testing package: Examples](https://pkg.go.dev/testing#hdr-Examples) — the naming rules (`ExampleType_method`) that attach an example to a symbol.
- [internal packages](https://go.dev/doc/go1.4#internalpackages) — why `internal/leakdetect` is importable within this module but not from others.

---

Back to [02-assert-no-leaks-suite.md](02-assert-no-leaks-suite.md) | Next: [04-demo-leak-cli.md](04-demo-leak-cli.md)
