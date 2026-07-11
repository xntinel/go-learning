# Exercise 9: Fan-Out — Collect All Worker Errors vs errgroup First-Error

The same concurrent fan-out has two opposite error policies, and choosing wrong is
a design bug. This exercise builds both: `CollectAll`, which runs every task and
aggregates every failure into an `errors.Join` (fail-complete), and `FirstError`,
which uses `errgroup` to return only the first failure and cancel the rest
(fail-fast). Seeing them side by side makes the trade-off concrete.

This module is fully self-contained: its own `go mod init`, all code inline, its
own demo and tests. It depends on `golang.org/x/sync/errgroup`.

## What you'll build

```text
fanout/                    independent module: example.com/fanout
  go.mod                   go 1.26; requires golang.org/x/sync
  fanout.go                CollectAll (mutex + Join); FirstError (errgroup)
  cmd/
    demo/
      main.go              runs both policies over the same tasks
  fanout_test.go           CollectAll has ALL sentinels; FirstError has one; -race
```

- Files: `fanout.go`, `cmd/demo/main.go`, `fanout_test.go`.
- Implement: `CollectAll(tasks []func() error) error` guarding a shared `[]error` with a `sync.Mutex` and returning `errors.Join`; `FirstError(ctx, tasks)` using `errgroup.WithContext`, returning the derived context and `Group.Wait`'s single error.
- Test: multiple concurrent failures — the `CollectAll` result is `errors.Is` every distinct sentinel; the `FirstError` result is `errors.Is` exactly the first and its derived context is Done. Run with `-race`.
- Verify: `go test -count=1 -race ./...`

Set up the module:

```bash
mkdir -p ~/go-exercises/fanout/cmd/demo
cd ~/go-exercises/fanout
go mod init example.com/fanout
go get golang.org/x/sync/errgroup
```

### The two policies, and why the accumulation must be locked

`CollectAll` is fail-complete: it launches one goroutine per task, and each
goroutine that fails records its error. Because several goroutines may fail at once,
the shared `[]error` is shared mutable state — appending to it without a
`sync.Mutex` is a data race that `-race` flags and that can corrupt the slice header
under load. So each goroutine locks, appends, unlocks. After the `WaitGroup`
drains, `errors.Join(errs...)` builds the census. This is the tool for a health
check that must report *every* unhealthy dependency.

`FirstError` is fail-fast: `errgroup.WithContext` gives a `Group` and a derived
`Context`. `Group.Wait()` returns only the *first* non-nil error any goroutine
returned, and the derived context is canceled the moment that first error appears —
which tears down the remaining goroutines that are watching `ctx.Done()`. This is
the tool for a request that is doomed the moment one dependency fails: get the
earliest error, stop wasting work on the rest.

The policies are duals. Same fan-out shape, opposite error semantics: a full
accounting of every failure, versus the earliest failure plus cancellation. The
mistake to avoid is reaching for `errgroup` when you needed the census — `Wait`
gives you one error and throws away the other four — or hand-rolling aggregation
when you actually wanted first-error-plus-cancellation.

Create `fanout.go`:

```go
package fanout

import (
	"context"
	"errors"
	"sync"

	"golang.org/x/sync/errgroup"
)

// CollectAll runs every task concurrently and returns errors.Join of every
// failure (fail-complete). The shared slice is guarded by a mutex because several
// goroutines may append at once. It returns nil when every task succeeds.
func CollectAll(tasks []func() error) error {
	var (
		mu   sync.Mutex
		errs []error
		wg   sync.WaitGroup
	)
	for _, task := range tasks {
		wg.Add(1)
		go func() {
			defer wg.Done()
			if err := task(); err != nil {
				mu.Lock()
				errs = append(errs, err)
				mu.Unlock()
			}
		}()
	}
	wg.Wait()
	return errors.Join(errs...)
}

// FirstError runs every task under an errgroup (fail-fast). Group.Wait returns
// only the first non-nil error, and the derived context is canceled on that first
// error, cancelling the remaining tasks. It returns the derived context so callers
// can observe the cancellation.
func FirstError(parent context.Context, tasks []func(context.Context) error) (context.Context, error) {
	g, ctx := errgroup.WithContext(parent)
	for _, task := range tasks {
		g.Go(func() error { return task(ctx) })
	}
	return ctx, g.Wait()
}
```

### The runnable demo

The demo runs three failing tasks under both policies. `CollectAll` shows every
sentinel present; `FirstError` shows only the fast one and a canceled context. The
output asserts presence with `errors.Is`, not raw aggregate text, because the
goroutine-scheduling order of the join is not deterministic.

Create `cmd/demo/main.go`:

```go
package main

import (
	"context"
	"errors"
	"fmt"

	"example.com/fanout"
)

var (
	errDB    = errors.New("db down")
	errCache = errors.New("cache down")
	errQueue = errors.New("queue down")
)

func main() {
	// Fail-complete: every failure is present in the aggregate.
	agg := fanout.CollectAll([]func() error{
		func() error { return errDB },
		func() error { return errCache },
		func() error { return errQueue },
	})
	fmt.Println("collect-all aggregates every failure:")
	fmt.Println("  errDB present:", errors.Is(agg, errDB))
	fmt.Println("  errCache present:", errors.Is(agg, errCache))
	fmt.Println("  errQueue present:", errors.Is(agg, errQueue))

	// Fail-fast: the fast task wins; the blockers exit on cancellation.
	ctx, err := fanout.FirstError(context.Background(), []func(context.Context) error{
		func(context.Context) error { return errDB },
		func(ctx context.Context) error { <-ctx.Done(); return ctx.Err() },
		func(ctx context.Context) error { <-ctx.Done(); return ctx.Err() },
	})
	fmt.Println("first-error returns just one:")
	fmt.Println("  errDB present:", errors.Is(err, errDB))
	fmt.Println("  context canceled:", ctx.Err() != nil)
}
```

Run it:

```bash
go run ./cmd/demo
```

Expected output:

```
collect-all aggregates every failure:
  errDB present: true
  errCache present: true
  errQueue present: true
first-error returns just one:
  errDB present: true
  context canceled: true
```

### Tests

`TestCollectAllHasEverySentinel` runs a set of distinct failing sentinels
concurrently and asserts the aggregate is `errors.Is` each — a full census. Running
under `-race` proves the mutex-guarded append is race-free. `TestFirstErrorReturnsOne`
makes one task fail immediately while the others block on `ctx.Done()`; this pins
the first error deterministically (the fast one always returns before the blockers,
which only return after cancellation), and asserts the derived context is canceled.
We assert on the *set* of sentinels, never on order.

Create `fanout_test.go`:

```go
package fanout

import (
	"context"
	"errors"
	"testing"
)

func TestCollectAllHasEverySentinel(t *testing.T) {
	t.Parallel()

	sentinels := []error{
		errors.New("s0"), errors.New("s1"), errors.New("s2"), errors.New("s3"),
	}
	tasks := make([]func() error, len(sentinels))
	for i, s := range sentinels {
		tasks[i] = func() error { return s }
	}

	agg := CollectAll(tasks)
	if agg == nil {
		t.Fatal("CollectAll = nil, want aggregate")
	}
	for i, s := range sentinels {
		if !errors.Is(agg, s) {
			t.Errorf("aggregate missing sentinel %d", i)
		}
	}
}

func TestCollectAllAllSuccess(t *testing.T) {
	t.Parallel()

	tasks := []func() error{
		func() error { return nil },
		func() error { return nil },
	}
	if err := CollectAll(tasks); err != nil {
		t.Fatalf("CollectAll = %v, want nil", err)
	}
}

func TestFirstErrorReturnsOne(t *testing.T) {
	t.Parallel()

	errFast := errors.New("fast")
	tasks := []func(context.Context) error{
		func(context.Context) error { return errFast },
		func(ctx context.Context) error { <-ctx.Done(); return ctx.Err() },
		func(ctx context.Context) error { <-ctx.Done(); return ctx.Err() },
	}

	ctx, err := FirstError(context.Background(), tasks)
	if !errors.Is(err, errFast) {
		t.Fatalf("FirstError = %v, want errFast", err)
	}
	if ctx.Err() == nil {
		t.Fatal("derived context not canceled after Wait")
	}
}
```

## Review

`CollectAll` is correct when the aggregate contains every failing sentinel and the
mutex makes the concurrent append race-free — which `-race` verifies; drop the lock
and the race detector fails the test. `FirstError` is correct when it returns the
single earliest error and its derived context ends up canceled. The determinism in
`TestFirstErrorReturnsOne` comes from design, not luck: the fast task returns before
the blockers, which cannot return until cancellation, so the first error is always
`errFast`. Choose `CollectAll` when you need a census of every failure and
`FirstError` when the earliest failure should abort the fan-out — they solve
different problems. Run with `-race`.

## Resources

- [golang.org/x/sync/errgroup](https://pkg.go.dev/golang.org/x/sync/errgroup) — `WithContext`, `Group.Go`, and `Group.Wait` returning the first error.
- [errors.Join](https://pkg.go.dev/errors#Join) — the fail-complete aggregation.
- [sync.Mutex](https://pkg.go.dev/sync#Mutex) — guarding the shared error slice.

---

Back to [00-concepts.md](00-concepts.md) | Prev: [08-unwrap-does-not-unwrap-join.md](08-unwrap-does-not-unwrap-join.md) | Next: [10-typed-extraction-astype.md](10-typed-extraction-astype.md)
