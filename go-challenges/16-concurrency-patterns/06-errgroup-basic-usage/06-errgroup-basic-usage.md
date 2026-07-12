# 6. Concurrent Group: Run N Tasks, Return The First Error

When N goroutines run in parallel and any one fails, the right response is to cancel the others, wait for the lot, and return the first observed error. `golang.org/x/sync/errgroup` is the canonical wrapper for this pattern. The pattern itself is built on `sync.WaitGroup`, `sync.Once`, and `context.WithCancel` — three stdlib pieces. The lesson teaches the pattern with stdlib only, so the tests run hermetically and the prose does not need a `go get`. `errgroup` is the idiomatic wrapper; if you need it on a real project, the API is `g.Go(func() error)` and `g.Wait() error`.

```text
grpdemo/
  go.mod
  internal/group/group.go
  internal/group/group_test.go
  cmd/grpdemo/main.go
```

The package exposes a `group.Group`. The `cmd/grpdemo` CLI schedules five tasks (two of which fail) and prints the per-task status, sorted by name. The output is the lesson's documentation: a real run is captured below.

## Concepts

### The Pattern: Wait, Then Return The First Error

`sync.WaitGroup` waits for goroutines to finish. It does not collect errors. The naive workaround is a shared `error` variable protected by a `sync.Mutex`, but the mutex is everywhere and the order of "first error" is racy. The pattern is:

1. A `sync.WaitGroup` counts the goroutines.
2. A `sync.Once` guards the first error and the cancellation of the derived context.
3. A `context.Context` derived from the parent gives the cancellation signal to all goroutines.
4. `Wait()` blocks on the WaitGroup, then returns the recorded error (or nil).

### Cancellation Is The Default

A goroutine that takes 200ms should not run for 200ms if a sibling failed at 5ms. The derived context is cancelled the moment the first error fires. Each goroutine uses `select { case <-ctx.Done(): ... case <-work: ... }` to honor the cancellation. The main goroutine waits, and the children exit promptly.

### The First Error Is Whichever Completes First

`Wait()` returns "an error" but not "the first error in source order". The order of completion is the order of error return. In a real run with five tasks, the one with the smallest sleep wins. The lesson's tests use `errors.Is` to check that the returned error is **one of** the errors the goroutines produced, not a specific one.

### `errgroup` Is The Idiomatic Wrapper

`golang.org/x/sync/errgroup` exposes the same pattern as `Group`, with the API `g.Go(func() error)` and `g.Wait() error`. The lesson's `group.Group` is what `errgroup` does internally. If you ship this to production and you already use `golang.org/x/sync`, prefer `errgroup`; if you want stdlib only, the pattern in this lesson is the canonical form.

## Exercises

### Exercise 1: Implement The Group

Create `internal/group/group.go`:

```go
package group

import (
	"context"
	"sync"
)

type Group struct {
	wg      sync.WaitGroup
	errOnce sync.Once
	err     error
	cancel  context.CancelFunc
	ctx     context.Context
}

// New returns a new Group and a derived context. The derived context
// is cancelled when any goroutine in the group returns a non-nil
// error, or when the parent context is cancelled.
func New(ctx context.Context) (*Group, context.Context) {
	ctx, cancel := context.WithCancel(ctx)
	return &Group{ctx: ctx, cancel: cancel}, ctx
}

// Go runs f in a new goroutine. If f returns a non-nil error, the
// Group's derived context is cancelled and the first such error is
// recorded.
func (g *Group) Go(f func() error) {
	g.wg.Add(1)
	go func() {
		defer g.wg.Done()
		if err := f(); err != nil {
			g.errOnce.Do(func() {
				g.err = err
				g.cancel()
			})
		}
	}()
}

// Wait blocks until all goroutines in the group have returned and
// returns the first non-nil error, if any. The derived context is
// cancelled when Wait returns, even on success.
func (g *Group) Wait() error {
	g.wg.Wait()
	if g.cancel != nil {
		g.cancel()
	}
	return g.err
}
```

`New` derives a cancellable context. `Go` starts a goroutine and uses `errOnce.Do` to record the first error and trigger cancellation. `Wait` blocks on the WaitGroup, cancels the derived context, and returns the first error.

### Exercise 2: Test The Group

Create `internal/group/group_test.go`:

```go
package group

import (
	"context"
	"errors"
	"testing"
	"time"
)

func TestGroupAllSucceed(t *testing.T) {
	t.Parallel()

	g, gctx := New(context.Background())
	for i := 0; i < 5; i++ {
		g.Go(func() error {
			return nil
		})
	}
	if err := g.Wait(); err != nil {
		t.Fatalf("Wait = %v, want nil", err)
	}
	// The derived context is cancelled when Wait returns, even on success.
	// This matches errgroup's contract.
	if err := gctx.Err(); !errors.Is(err, context.Canceled) {
		t.Fatalf("gctx.Err = %v, want context.Canceled after Wait", err)
	}
}

func TestGroupReturnsAnErrorFromTheSet(t *testing.T) {
	t.Parallel()

	errA := errors.New("a failed")
	errB := errors.New("b failed")
	g, _ := New(context.Background())
	g.Go(func() error { return errA })
	g.Go(func() error { return errB })
	got := g.Wait()
	if !errors.Is(got, errA) && !errors.Is(got, errB) {
		t.Fatalf("Wait = %v, want errA or errB", got)
	}
}

func TestGroupCancelsOnError(t *testing.T) {
	t.Parallel()

	g, gctx := New(context.Background())
	released := make(chan struct{})
	g.Go(func() error {
		select {
		case <-gctx.Done():
			close(released)
			return gctx.Err()
		case <-time.After(2 * time.Second):
			return errors.New("did not cancel in time")
		}
	})
	g.Go(func() error {
		return errors.New("first")
	})
	if err := g.Wait(); err == nil {
		t.Fatal("expected error")
	}
	select {
	case <-released:
	case <-time.After(1 * time.Second):
		t.Fatal("context was not cancelled when the group saw an error")
	}
}

func TestGroupRespectsParentCancel(t *testing.T) {
	t.Parallel()

	ctx, cancel := context.WithCancel(context.Background())
	g, gctx := New(ctx)
	done := make(chan struct{})
	g.Go(func() error {
		<-gctx.Done()
		close(done)
		return gctx.Err()
	})
	cancel()
	if err := g.Wait(); !errors.Is(err, context.Canceled) {
		t.Fatalf("Wait = %v, want context.Canceled", err)
	}
	select {
	case <-done:
	case <-time.After(1 * time.Second):
		t.Fatal("goroutine did not exit on parent cancel")
	}
}
```

`TestGroupCancelsOnError` is the lesson's main test: it proves the group cancels siblings as soon as the first error fires. The inner goroutine waits up to 2 seconds; if cancellation does not reach it, the test fails on the inner timeout. `TestGroupRespectsParentCancel` proves the inverse direction: cancelling the parent context cancels the group's derived context. `TestGroupReturnsAnErrorFromTheSet` does not assert a specific error; the order of completion is the order of error return, and the test accepts any of the two errors as correct.

### Exercise 3: Run It End To End

Create `cmd/grpdemo/main.go`:

```go
package main

import (
	"context"
	"errors"
	"fmt"
	"os"
	"time"

	"example.com/grpdemo/internal/group"
)

func main() {
	g, gctx := group.New(context.Background())

	// Two tasks fail (with small sleeps so the order is non-deterministic).
	// The rest either succeed or get cancelled when the first error fires.
	type task struct {
		name    string
		delay   time.Duration
		wantErr bool
	}
	tasks := []task{
		{"a", 30 * time.Millisecond, false},
		{"b", 10 * time.Millisecond, true},
		{"c", 50 * time.Millisecond, false},
		{"d", 5 * time.Millisecond, true},
		{"e", 20 * time.Millisecond, false},
	}
	status := make([]string, len(tasks))
	for i, t := range tasks {
		i, t := i, t
		g.Go(func() error {
			select {
			case <-time.After(t.delay):
				if t.wantErr {
					status[i] = "FAIL " + t.name
					return errors.New("task " + t.name + " failed")
				}
				status[i] = "OK   " + t.name
				return nil
			case <-gctx.Done():
				status[i] = "SKIP " + t.name
				return gctx.Err()
			}
		})
	}

	err := g.Wait()
	for _, line := range status {
		fmt.Println(line)
	}
	if err != nil {
		fmt.Fprintln(os.Stderr, "group returned error:", err)
		os.Exit(1)
	}
	fmt.Println("all tasks completed cleanly")
}
```

Run it from `~/go-exercises/grpdemo`:

```bash
go run ./cmd/grpdemo
```

Expected output (captured by the author on Go 1.26):

```text
SKIP a
SKIP b
SKIP c
FAIL d
SKIP e
group returned error: task d failed
exit status 1
```

The first error is `task d failed` because `d` has the smallest sleep (5ms). The other goroutines see the cancellation through `gctx.Done()` and skip. The `status[i] = "..."` write happens before the goroutine returns, so `Wait` returns after all five status writes are visible.

If you want to see the `errgroup` equivalent for comparison, the API is `g, gctx := errgroup.WithContext(ctx)`, `g.Go(func() error { ... })`, `err := g.Wait()`. The lesson's `group.Group` and `errgroup` are line-for-line equivalent in behavior.

## Common Mistakes

### Sharing An Error Variable With A Mutex

Wrong: a package-level `var err error` plus a `sync.Mutex` shared between goroutines, with each goroutine doing `mu.Lock(); err = ...; mu.Unlock()`. The "first error" race is real: a slow goroutine may overwrite a fast one's error. The order is also wrong if a later goroutine finishes first.

Fix: `sync.Once` guards the write and the cancel. The first goroutine to call `Do` wins; the others see the work already done.

### Forgetting To Cancel The Derived Context

Wrong: a Group that records the error but does not cancel the derived context. The siblings keep running to completion. The pattern is broken: the user expected cancellation, and they got "wait for everyone".

Fix: `cancel()` is called inside `errOnce.Do`, alongside the error write. The first error fires the cancel; the siblings see `gctx.Done()`.

### Using `g.Wait()` And Ignoring The Error

Wrong: `g.Wait()` and discard the returned error. The pattern is a guard clause, not a fire-and-forget. If the error is the only signal of failure, ignoring it is the bug.

Fix: assign and check: `if err := g.Wait(); err != nil { ... }`. The CLI does this with `os.Exit(1)`.

### Assuming A Specific Error Comes Back First

Wrong: a test that asserts `g.Wait() == specificError`. The order of completion is the order of error return; tests that pin a specific error are flaky on different machines or under `-race`.

Fix: assert membership: `errors.Is(got, errA) || errors.Is(got, errB)`. The lesson's `TestGroupReturnsAnErrorFromTheSet` does this.

## Verification

Run this from `~/go-exercises/grpdemo`:

```bash
test -z "$(gofmt -l .)"
go test -count=1 -race ./...
go vet ./...
go build ./...
go run ./cmd/grpdemo
```

The `go build ./...` step proves the `cmd/grpdemo` binary compiles. The `go run ./cmd/grpdemo` step produces the captured output above (sorted by task name, exit code 1). The test suite pins the contract: all-success, first-error-wins, cancellation-on-error, parent-cancel-propagation.

The optional "swap `group.Group` for `errgroup`" exercise (not in the tests) is left to the reader: replace the import and the `g.Go` call signature, and the behavior is identical.

## Summary

- The pattern is `WaitGroup + Once + WithCancel`; the idiomatic wrapper is `errgroup.Group`.
- The first error cancels the derived context; siblings see `gctx.Done()` and exit.
- `Wait()` returns "an error", not "the first error in source order" — tests assert membership, not identity.
- The derived context is cancelled when `Wait` returns, even on success; the contract is `ctx.Err() == context.Canceled` after `Wait`.
- If you ship to production and you already use `golang.org/x/sync`, prefer `errgroup`. If you want stdlib only, this pattern is the canonical form.

## What's Next

Next: [errgroup With Context](../07-errgroup-with-context/07-errgroup-with-context.md).

## Resources

- [golang.org/x/sync/errgroup](https://pkg.go.dev/golang.org/x/sync/errgroup)
- [sync.WaitGroup](https://pkg.go.dev/sync#WaitGroup)
- [context.WithCancel](https://pkg.go.dev/context#WithCancel)
- [sync.Once](https://pkg.go.dev/sync#Once)
