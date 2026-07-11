# Exercise 8: Prove Early Failure Leaks No Goroutines with goleak

A goroutine leak does not fail a test — the test passes, the leak accumulates, and
the process slowly bloats with parked goroutines until it falls over in production.
The only way to catch it in CI is to assert the absence of leaks explicitly.
`go.uber.org/goleak` does exactly that: `VerifyTestMain` checks the whole package
after all tests run, and `VerifyNone(t)` checks around a single test. This module
adds leak verification to a fail-fast fan-out and proves that when one job errors
early, the workers blocked on the context are cancelled and drained rather than
left hanging.

This module is fully self-contained: its own `go mod init`, all code inline, its
own demo and tests. Nothing here imports another exercise.

## What you'll build

```text
leakcheck/                   independent module: example.com/leakcheck
  go.mod                     go 1.26; requires golang.org/x/sync and go.uber.org/goleak
  leakcheck.go               FanOut (errgroup fail-fast; workers honor ctx)
  cmd/
    demo/
      main.go                runnable demo: early failure, count goroutines before/after
  leakcheck_test.go          TestMain with goleak.VerifyTestMain; per-test VerifyNone
```

Files: `leakcheck.go`, `cmd/demo/main.go`, `leakcheck_test.go`.
Implement: `FanOut(ctx, jobs)` using `errgroup.WithContext` where every worker honors `ctx.Done()`, so the first error cancels and drains the rest.
Test: `TestMain` calls `goleak.VerifyTestMain(m)`; a test runs `FanOut` with one immediately-failing job and others blocked on `ctx`, guarded by `defer goleak.VerifyNone(t)`, which passes only if the runner cancelled and drained.
Verify: `go test -count=1 -race ./...`

Set up the module:

```bash
mkdir -p ~/go-exercises/leakcheck/cmd/demo
cd ~/go-exercises/leakcheck
go mod init example.com/leakcheck
go mod edit -go=1.26
go get golang.org/x/sync/errgroup
go get go.uber.org/goleak
```

### What goleak checks, and what a leak looks like

`goleak.VerifyNone(t)` snapshots the set of running goroutines and fails the test
if any unexpected ones remain, ignoring the known runtime and testing-framework
goroutines. Placed as `defer goleak.VerifyNone(t)` at the top of a test, it runs
last and asserts that the code under test left nothing running.
`goleak.VerifyTestMain(m)` does the same once for the whole package after all tests
complete — a cheap safety net you add to every concurrency package's `TestMain`.
The two compose: keep `VerifyTestMain` for coverage and add `VerifyNone` to the
specific tests where a leak is most likely.

The leak this exercise targets is the classic fail-fast bug. A fan-out where one
job errors immediately should cancel the shared context so the other workers —
blocked on `ctx.Done()` — wake and return. If a worker instead blocks on something
the cancellation cannot reach (an unbuffered channel with no remaining reader, a
context it never consults), it lives forever after the run returns. `FanOut` is
written correctly: it uses `errgroup.WithContext`, whose derived context is
cancelled on the first non-nil return, and every worker's blocking wait is a
`select` on `ctx.Done()`. So when the failing job returns its error, the context
cancels, the blocked workers return `ctx.Err()`, `Wait` returns the first error,
and no goroutine survives. `goleak.VerifyNone` proves that last claim.

The contrast is worth stating precisely. A *leaky* variant would give a worker no
cancellation-aware exit — for example `<-make(chan struct{})`, a receive on a
channel nobody ever sends to or closes, which no context cancellation can unblock:

```text
// LEAKY — do not do this. This worker can never be cancelled:
job := func(ctx context.Context) error {
	<-make(chan struct{}) // blocks forever; ctx.Done() is ignored
	return nil
}
// FanOut would return on the first error, but this goroutine stays parked,
// and goleak.VerifyNone(t) would fail the test reporting the leaked stack
// sitting in runtime.gopark <- chan receive at this line.
```

Because that goroutine ignores `ctx`, `errgroup`'s cancellation never reaches it,
`Wait` returns while it is still parked, and goleak catches it. The fix is the one
`FanOut` already uses: every blocking wait selects on `ctx.Done()`.

Create `leakcheck.go`:

```go
package leakcheck

import (
	"context"

	"golang.org/x/sync/errgroup"
)

// Job is a named unit of work that MUST honor ctx so it can be cancelled when a
// sibling fails. A job that ignores ctx and blocks is the leak this package's
// tests are designed to catch.
type Job struct {
	Name string
	Run  func(ctx context.Context) error
}

// FanOut runs jobs with fail-fast semantics: the first error cancels the shared
// context, every ctx-aware worker returns, and Wait yields that first error. No
// goroutine survives the call, which goleak verifies in the tests.
func FanOut(ctx context.Context, jobs []Job) error {
	g, ctx := errgroup.WithContext(ctx)
	for _, j := range jobs {
		g.Go(func() error {
			return j.Run(ctx)
		})
	}
	return g.Wait()
}
```

### The runnable demo

The demo runs a fan-out where one job fails immediately and two others block on the
context, then compares the goroutine count before and after. Because the workers
honor `ctx`, the count returns to its baseline — no leak.

Create `cmd/demo/main.go`:

```go
package main

import (
	"context"
	"errors"
	"fmt"
	"runtime"
	"time"

	"example.com/leakcheck"
)

func main() {
	before := runtime.NumGoroutine()

	err := leakcheck.FanOut(context.Background(), []leakcheck.Job{
		{Name: "fails", Run: func(ctx context.Context) error {
			return errors.New("upstream down")
		}},
		{Name: "blocks-1", Run: func(ctx context.Context) error {
			<-ctx.Done()
			return ctx.Err()
		}},
		{Name: "blocks-2", Run: func(ctx context.Context) error {
			<-ctx.Done()
			return ctx.Err()
		}},
	})

	// Give the runtime a moment to reap the returned goroutines before counting.
	time.Sleep(50 * time.Millisecond)
	after := runtime.NumGoroutine()

	fmt.Printf("FanOut error: %v\n", err)
	fmt.Printf("goroutines returned to baseline: %t\n", after <= before)
}
```

Run it:

```bash
go run ./cmd/demo
```

Expected output:

```
FanOut error: upstream down
goroutines returned to baseline: true
```

### Tests

`TestMain` installs `goleak.VerifyTestMain(m)` so the whole package is leak-checked
after every test. `TestEarlyFailureLeavesNoLeak` is the pointed test: one job fails
immediately, two block on `ctx.Done()`, and `defer goleak.VerifyNone(t)` asserts
that after `FanOut` returns, none of those blocked workers survive — which holds
only because the failing job cancelled the context and the blocked workers honored
it. `TestAllSucceedNoLeak` runs a clean fan-out under the same guard. Both use the
package `TestMain`, so they benefit from the double check.

Create `leakcheck_test.go`:

```go
package leakcheck

import (
	"context"
	"errors"
	"testing"

	"go.uber.org/goleak"
)

func TestMain(m *testing.M) {
	goleak.VerifyTestMain(m)
}

var errBoom = errors.New("boom")

func TestEarlyFailureLeavesNoLeak(t *testing.T) {
	defer goleak.VerifyNone(t)

	err := FanOut(context.Background(), []Job{
		{Name: "fails", Run: func(ctx context.Context) error { return errBoom }},
		{Name: "blocks-1", Run: func(ctx context.Context) error { <-ctx.Done(); return ctx.Err() }},
		{Name: "blocks-2", Run: func(ctx context.Context) error { <-ctx.Done(); return ctx.Err() }},
	})
	if !errors.Is(err, errBoom) {
		t.Fatalf("FanOut() error = %v, want errBoom", err)
	}
}

func TestAllSucceedNoLeak(t *testing.T) {
	defer goleak.VerifyNone(t)

	err := FanOut(context.Background(), []Job{
		{Name: "a", Run: func(ctx context.Context) error { return nil }},
		{Name: "b", Run: func(ctx context.Context) error { return nil }},
	})
	if err != nil {
		t.Fatalf("FanOut() error = %v, want nil", err)
	}
}
```

## Review

The runner is correct when an early failure leaves nothing running, and `goleak` is
what turns that into a checkable assertion: `VerifyNone(t)` fails the test if any
worker from the fan-out is still parked after `FanOut` returns, and it passes only
because `errgroup.WithContext` cancelled the context on the first error and every
worker honored `ctx.Done()`. The leak this catches is a worker whose blocking wait
cannot be reached by cancellation — a receive on a channel nobody closes, a context
it never consults. Note the two goleak entry points do different jobs:
`VerifyTestMain` is the cheap package-wide net in `TestMain`, `VerifyNone` is the
targeted per-test check where a leak is most likely; use both. Remember goleak
checks *after* the function returns, so a worker that is merely slow to return can
produce a flaky failure — every goroutine must have a prompt, cancellation-driven
exit. Run `go test -race` and `go vet ./...` to confirm.

## Resources

- [`go.uber.org/goleak`](https://pkg.go.dev/go.uber.org/goleak) — `VerifyNone`, `VerifyTestMain`, and `Find`.
- [`golang.org/x/sync/errgroup`](https://pkg.go.dev/golang.org/x/sync/errgroup) — the fail-fast group whose cancellation drains the workers.
- [Go Blog: Pipelines and cancellation](https://go.dev/blog/pipelines) — why every worker needs a cancellation-driven exit.

---

Back to [00-concepts.md](00-concepts.md) | Next: [09-safe-background-task-sink.md](09-safe-background-task-sink.md)
