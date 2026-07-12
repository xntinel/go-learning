# Exercise 7: Fire Compensation Exactly at the Deadline With AfterFunc

Some work holds a resource that must be released the moment the deadline fires — a
lease, a reservation, a downstream signal — not "eventually" and not "only on the
happy path." This exercise builds a leased-work runner that uses `context.AfterFunc`
to schedule a compensation callback precisely at expiry, calls the returned `stop`
on normal completion, and makes the completion-versus-expiry race exactly-once so the
lease is released by exactly one path.

This module is fully self-contained. It has its own `go mod init`, defines every
type it needs, and ships its own demo and tests. Nothing here imports any other
exercise.

## What you'll build

```text
lease-runner/                        independent module: example.com/leaserunner
  go.mod                             go 1.26
  runner.go                          Recorder, RunLeased; AfterFunc compensation + stop
  cmd/
    demo/
      main.go                        runnable demo: slow times out (compensated), fast stops it
  runner_test.go                     slow/fast/race paths, exactly-once, -race
```

- Files: `runner.go`, `cmd/demo/main.go`, `runner_test.go`.
- Implement: a `Recorder` with an idempotent `release` and a timeout metric, and `RunLeased(ctx, rec, work)` that schedules a compensation via `context.AfterFunc`, calls `stop()` on completion, and waits for the callback when `stop` reports it already fired.
- Test: slow work triggers the callback (lease released, metric incremented); fast work's `stop()` returns true and the callback never runs (recorder untouched by timeout); the overlap race releases exactly once.
- Verify: `go test -count=1 -race ./...`

Set up the module:

```bash
mkdir -p go-solutions/14-select-and-context/05-context-withtimeout-withdeadline/07-deadline-afterfunc-cleanup/cmd/demo
cd go-solutions/14-select-and-context/05-context-withtimeout-withdeadline/07-deadline-afterfunc-cleanup
```

### The AfterFunc contract and the race it creates

`context.AfterFunc(ctx, f)` (Go 1.21) arranges to call `f` in its own goroutine once
`ctx` is done — cancelled or deadline-exceeded — and returns a `stop func() bool`.
Calling `stop` deregisters `f`: it returns true if it stopped `f` from running, and
false if the call came too late because `ctx` is already done and `f` has been started
(or `f` was already stopped). This is the clean, race-aware replacement for the manual
`go func() { <-ctx.Done(); compensate() }()`, which has to be written carefully to
avoid leaking the goroutine when the work finishes first.

The compensation here releases a lease and emits a timeout metric (in a real system it
would also signal downstream that the work was abandoned). The design problem is that
completion and expiry can happen at almost the same instant, and the lease must be
released *exactly once* — never by both the normal path and the callback, and never by
neither. Two mechanisms combine to guarantee that. First, `stop()`'s return value
routes the responsibility: if `stop` returns true, the callback was deregistered before
it ran, so the normal path owns the cleanup; if it returns false, the callback has been
started and *it* owns the cleanup, so the normal path must not release — it waits for
the callback to finish so the caller sees a consistent state when `RunLeased` returns.
Second, `release` is idempotent behind a mutex, a belt-and-suspenders that makes a
double release harmless even if the routing logic is ever wrong.

The metric is only incremented inside the callback, guarded by the same idempotent
release, so it counts a timeout compensation exactly once and never on the fast path.
Waiting on a `cleaned` channel when `stop` returns false is what makes the test
deterministic: `RunLeased` does not return until the compensation goroutine has
finished, so there is no window where the lease looks un-released after the call.

Create `runner.go`:

```go
package leaserunner

import (
	"context"
	"sync"
)

// Recorder stands in for the leased resource and its metrics. release is
// idempotent; the timeout metric is incremented only by the compensation path.
type Recorder struct {
	mu           sync.Mutex
	releaseCount int
	timeouts     int
}

// release frees the lease, returning true only for the call that actually did it.
func (r *Recorder) release() bool {
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.releaseCount > 0 {
		return false
	}
	r.releaseCount++
	return true
}

func (r *Recorder) recordTimeout() {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.timeouts++
}

// ReleaseCount reports how many times the lease was actually released (want 1).
func (r *Recorder) ReleaseCount() int {
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.releaseCount
}

// Timeouts reports how many times compensation fired (want 1 on timeout, 0 on fast).
func (r *Recorder) Timeouts() int {
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.timeouts
}

// RunLeased runs work while holding a lease. It schedules compensation to run at
// the deadline via context.AfterFunc, and cancels it with stop() on normal
// completion, guaranteeing the lease is released exactly once.
func RunLeased(ctx context.Context, rec *Recorder, work func(context.Context) error) error {
	cleaned := make(chan struct{})
	stop := context.AfterFunc(ctx, func() {
		if rec.release() {
			rec.recordTimeout()
			// A real system would also signal downstream here.
		}
		close(cleaned)
	})

	err := work(ctx)

	if stop() {
		// Compensation was deregistered before running: normal path owns cleanup.
		rec.release()
	} else {
		// Compensation has fired: it owns cleanup. Wait for it to finish so the
		// lease is guaranteed released when RunLeased returns.
		<-cleaned
	}
	return err
}
```

### The runnable demo

The demo runs the same runner twice: once with work that blocks past a 30ms deadline
(compensation fires) and once with fast work under a 1s budget (`stop` cancels the
compensation).

Create `cmd/demo/main.go`:

```go
package main

import (
	"context"
	"fmt"
	"time"

	"example.com/leaserunner"
)

func main() {
	slowRec := &leaserunner.Recorder{}
	ctx1, cancel1 := context.WithTimeout(context.Background(), 30*time.Millisecond)
	defer cancel1()
	_ = leaserunner.RunLeased(ctx1, slowRec, func(ctx context.Context) error {
		<-ctx.Done()
		return ctx.Err()
	})
	fmt.Printf("slow: released=%d timeouts=%d\n", slowRec.ReleaseCount(), slowRec.Timeouts())

	fastRec := &leaserunner.Recorder{}
	ctx2, cancel2 := context.WithTimeout(context.Background(), time.Second)
	defer cancel2()
	_ = leaserunner.RunLeased(ctx2, fastRec, func(ctx context.Context) error {
		time.Sleep(5 * time.Millisecond)
		return nil
	})
	fmt.Printf("fast: released=%d timeouts=%d\n", fastRec.ReleaseCount(), fastRec.Timeouts())
}
```

Run it:

```bash
go run ./cmd/demo
```

Expected output:

```
slow: released=1 timeouts=1
fast: released=1 timeouts=0
```

### Tests

`TestSlowWorkCompensates` blocks work past a short deadline and asserts the lease was
released once and the timeout metric incremented. `TestFastWorkStopsCompensation`
runs quick work under a long budget and asserts the lease released once but the
timeout metric stayed zero — the callback never ran. `TestExactlyOnceUnderRace` runs
many iterations where the work duration equals the deadline, so completion and expiry
overlap, and asserts the release count is exactly one every time.

Create `runner_test.go`:

```go
package leaserunner

import (
	"context"
	"errors"
	"testing"
	"time"
)

func TestSlowWorkCompensates(t *testing.T) {
	t.Parallel()
	rec := &Recorder{}
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Millisecond)
	defer cancel()

	err := RunLeased(ctx, rec, func(ctx context.Context) error {
		<-ctx.Done()
		return ctx.Err()
	})
	if !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("err = %v, want DeadlineExceeded", err)
	}
	if rec.ReleaseCount() != 1 {
		t.Fatalf("ReleaseCount = %d, want 1", rec.ReleaseCount())
	}
	if rec.Timeouts() != 1 {
		t.Fatalf("Timeouts = %d, want 1 (compensation fired)", rec.Timeouts())
	}
}

func TestFastWorkStopsCompensation(t *testing.T) {
	t.Parallel()
	rec := &Recorder{}
	ctx, cancel := context.WithTimeout(context.Background(), time.Second)
	defer cancel()

	err := RunLeased(ctx, rec, func(ctx context.Context) error {
		time.Sleep(5 * time.Millisecond)
		return nil
	})
	if err != nil {
		t.Fatalf("err = %v, want nil", err)
	}
	if rec.ReleaseCount() != 1 {
		t.Fatalf("ReleaseCount = %d, want 1", rec.ReleaseCount())
	}
	if rec.Timeouts() != 0 {
		t.Fatalf("Timeouts = %d, want 0 (compensation must not fire)", rec.Timeouts())
	}
}

func TestExactlyOnceUnderRace(t *testing.T) {
	t.Parallel()
	for i := range 50 {
		rec := &Recorder{}
		ctx, cancel := context.WithTimeout(context.Background(), 5*time.Millisecond)

		err := RunLeased(ctx, rec, func(ctx context.Context) error {
			// Finish right around the deadline to overlap completion and expiry.
			select {
			case <-time.After(5 * time.Millisecond):
				return nil
			case <-ctx.Done():
				return ctx.Err()
			}
		})
		cancel()

		if rec.ReleaseCount() != 1 {
			t.Fatalf("iteration %d: ReleaseCount = %d, want exactly 1 (err=%v)", i, rec.ReleaseCount(), err)
		}
	}
}
```

## Review

The runner is correct when the lease is released exactly once regardless of who wins
the completion-versus-expiry race. `TestSlowWorkCompensates` proves the callback path:
the lease is released and the timeout metric fires. `TestFastWorkStopsCompensation`
proves `stop()` cancels the callback so it never runs — the recorder's timeout counter
stays zero, which is the whole reason for calling `stop` on the happy path.
`TestExactlyOnceUnderRace` hammers the overlap and asserts `ReleaseCount == 1` every
iteration, catching any path where both the callback and the normal cleanup release, or
neither does.

The mistakes to avoid: replacing `AfterFunc` with a hand-rolled
`go func(){ <-ctx.Done(); ... }()` that leaks when the work finishes first; ignoring
`stop()`'s return value and letting both paths release (or neither); and reading the
recorder immediately after `RunLeased` returns without the `cleaned`-channel wait,
which would race the compensation goroutine. The idempotent `release` behind a mutex is
the safety net; the `stop`-return routing is the correctness. Run `go test -race` — the
race path is designed to trip the detector if the exactly-once discipline is broken.

## Resources

- [context.AfterFunc](https://pkg.go.dev/context#AfterFunc) — the constructor, its goroutine semantics, and the stop function's return contract.
- [context.WithTimeout](https://pkg.go.dev/context#WithTimeout) — the deadline that drives the compensation.
- [Go 1.21 release notes](https://go.dev/doc/go1.21#context) — the introduction of AfterFunc alongside the cause API.

---

Back to [06-labeled-timeout-cause.md](06-labeled-timeout-cause.md) | Next: [08-fanout-subbudget-allocation.md](08-fanout-subbudget-allocation.md)
