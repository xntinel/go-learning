# Exercise 1: Bounded Parallel Map

`Map` applies a function to every item concurrently, preserves result order, and
bounds how many calls run at once — the everyday fan-out a backend actually needs.
It is the smallest realistic showcase of `WaitGroup.Go`: one `wg.Go` per item with
no `Add`/`Done` to mismanage, a semaphore that supplies the bound a `WaitGroup`
does not, and a lock-free result collection that the memory model makes safe.

This module is fully self-contained. It begins with its own `go mod init`, defines
everything it needs, and ships its own demo and tests. Nothing here imports any
other exercise.

## What you'll build

```text
parallel.go          Map[T,R](items, limit, fn) ([]R, error); firstErr
cmd/
  demo/
    main.go          uppercase a slice of words through Map and print the result
parallel_test.go     order, bounded peak concurrency, first-error, empty, limit<1
example_test.go      Example with // Output: a runnable doc example
```

- Files: `parallel.go`, `cmd/demo/main.go`, `parallel_test.go`, `example_test.go`.
- Implement: the generic `Map[T, R any](items []T, limit int, fn func(T) (R, error)) ([]R, error)` and the unexported `firstErr`.
- Test: `parallel_test.go` asserts input-order results, peak concurrency never exceeds `limit`, the first error by index with the other results still filled, an empty input, and that `limit < 1` is clamped to 1.
- Verify: `go vet ./... && go test -race -count=1 ./...`

Set up the module:

```bash
mkdir -p go-solutions/48-modern-go-language-and-stdlib/05-sync-waitgroup-go/01-bounded-parallel-map/cmd/demo && cd go-solutions/48-modern-go-language-and-stdlib/05-sync-waitgroup-go/01-bounded-parallel-map
go mod edit -go=1.25
```

### Why a semaphore, and why no lock

Two facts about `WaitGroup.Go` shape every line of `Map`, and both are things `Go`
does *not* do for you.

The first is bounding. `wg.Go` launches its goroutine immediately; a `WaitGroup`
only counts tasks, it never throttles them. A naive `for _, item := range items {
wg.Go(...) }` therefore starts *all* of them at once, which is exactly wrong when
each call opens a database connection or allocates a buffer. The bound has to come
from somewhere else, and the idiomatic source is a counting semaphore built from a
buffered channel of capacity `limit`. Sending into the channel before each `wg.Go`
acquires a slot and blocks once `limit` tasks are in flight; the task releases its
slot with a `<-sem` in a `defer` when it finishes. The send happens in the loop
goroutine, *before* the launch, so the loop itself stalls until a slot frees up —
that back-pressure is what caps in-flight work at `limit` rather than letting the
loop race ahead and queue every task.

The second is that there is no shared mutable state to protect, so there is no
lock. Each task writes only its own `results[i]` and `errs[i]`; distinct indices
never alias the same memory, so two goroutines can never touch the same slot.
Pre-sizing both slices to `len(items)` before the loop is what makes that true —
the slices never grow, so the backing array never moves, and an index computed in
the loop stays valid in the goroutine. The reads afterward are safe for the
memory-model reason from the concepts file: every task's write happens before
`wg.Go`'s internal `Done`, which happens before `Wait` returns, so by the time
`firstErr` scans `errs` and the caller reads `results`, every write is visible.
Order falls out for free — `results[i]` is the result of `items[i]` regardless of
which task finished first, because the index, not the completion order, decides
the slot.

Note what `Map` deliberately does *not* do: it has no cancellation. A failing task
does not stop the others; every task runs to completion and `firstErr` reports the
lowest-indexed error at the end. If you need the first failure to abort the rest,
that is `errgroup`'s job, not this one's.

Create `parallel.go`:

```go
package parallel

import "sync"

// Map applies fn to each item concurrently, running at most limit calls at
// once, and returns the results in the original order. Every started task
// completes before Map returns. If any call fails, Map returns the first error
// by index.
func Map[T, R any](items []T, limit int, fn func(T) (R, error)) ([]R, error) {
	if limit < 1 {
		limit = 1
	}

	results := make([]R, len(items))
	errs := make([]error, len(items))
	sem := make(chan struct{}, limit)

	var wg sync.WaitGroup
	for i, item := range items {
		sem <- struct{}{} // acquire a slot; blocks once limit tasks are in flight
		wg.Go(func() {
			defer func() { <-sem }() // release the slot
			results[i], errs[i] = fn(item)
		})
	}
	wg.Wait()

	return results, firstErr(errs)
}

func firstErr(errs []error) error {
	for _, err := range errs {
		if err != nil {
			return err
		}
	}
	return nil
}
```

Each goroutine writes only its own `results[i]` and `errs[i]`, so distinct
indices never collide, and `Wait` synchronizes those writes before the reads in
`firstErr` and in the caller. There is no shared mutable state to lock. The
`limit < 1` clamp keeps a caller that passes `0` from constructing a
zero-capacity semaphore, which would deadlock on the first send.

### The runnable demo

The demo runs the everyday case — transform a slice in parallel and read the
results back in order.

Create `cmd/demo/main.go`:

```go
package main

import (
	"fmt"
	"strings"

	"example.com/parallel"
)

func main() {
	words := []string{"alpha", "beta", "gamma", "delta"}

	upper, err := parallel.Map(words, 2, func(w string) (string, error) {
		return strings.ToUpper(w), nil
	})
	if err != nil {
		fmt.Println("error:", err)
		return
	}
	fmt.Println(upper)
}
```

Run it:

```bash
go run ./cmd/demo
```

Expected output:

```
[ALPHA BETA GAMMA DELTA]
```

### Tests

The tests pin the four contracts `Map` promises plus the `limit < 1` guard, and
each is written to assert its property without flaking. Order checks that squaring
`1..5` returns results in input order even though work runs concurrently.
Bounding tracks peak concurrency with `sync/atomic` and asserts it never exceeds
`limit` — an upper-bound assertion the semaphore guarantees, so it cannot flake on
a slow machine the way a lower-bound "at least N ran together" assertion would.
First-error returns the failing index's error by position while the non-failing
results stay filled. Empty input returns an empty slice and no error. The
`limit = 0` case asserts every item is still processed because the constructor
clamps the bound to at least 1. The whole file runs under `-race`, which is the
real check that the per-index writes never collide.

Create `parallel_test.go`:

```go
package parallel

import (
	"errors"
	"sync/atomic"
	"testing"
	"time"
)

func TestMapPreservesOrder(t *testing.T) {
	t.Parallel()

	out, err := Map([]int{1, 2, 3, 4, 5}, 2, func(n int) (int, error) {
		return n * n, nil
	})
	if err != nil {
		t.Fatalf("Map error = %v", err)
	}
	want := []int{1, 4, 9, 16, 25}
	for i, w := range want {
		if out[i] != w {
			t.Fatalf("out = %v, want %v", out, want)
		}
	}
}

func TestMapBoundsConcurrency(t *testing.T) {
	t.Parallel()

	const limit = 3
	var current, peak atomic.Int32

	items := make([]int, 30)
	_, err := Map(items, limit, func(int) (int, error) {
		n := current.Add(1)
		for {
			p := peak.Load()
			if n <= p || peak.CompareAndSwap(p, n) {
				break
			}
		}
		time.Sleep(time.Millisecond)
		current.Add(-1)
		return 0, nil
	})
	if err != nil {
		t.Fatalf("Map error = %v", err)
	}
	if got := peak.Load(); got > limit {
		t.Fatalf("peak concurrency = %d, want <= %d", got, limit)
	}
}

func TestMapReturnsFirstError(t *testing.T) {
	t.Parallel()

	errBoom := errors.New("boom")
	out, err := Map([]int{1, 2, 3, 4}, 2, func(n int) (int, error) {
		if n == 3 {
			return 0, errBoom
		}
		return n * 10, nil
	})
	if !errors.Is(err, errBoom) {
		t.Fatalf("err = %v, want errBoom", err)
	}
	// The non-failing tasks still produced their results.
	if out[0] != 10 || out[1] != 20 || out[3] != 40 {
		t.Fatalf("out = %v, want non-error indices filled", out)
	}
}

func TestMapEmpty(t *testing.T) {
	t.Parallel()

	out, err := Map(nil, 4, func(n int) (int, error) { return n, nil })
	if err != nil || len(out) != 0 {
		t.Fatalf("Map(nil) = %v, %v; want [], <nil>", out, err)
	}
}

func TestMapClampsLimit(t *testing.T) {
	t.Parallel()

	// limit = 0 must not deadlock on a zero-capacity semaphore; it is clamped
	// to 1 and every item is still processed in order.
	out, err := Map([]int{1, 2, 3}, 0, func(n int) (int, error) {
		return n + 100, nil
	})
	if err != nil {
		t.Fatalf("Map error = %v", err)
	}
	want := []int{101, 102, 103}
	for i, w := range want {
		if out[i] != w {
			t.Fatalf("out = %v, want %v", out, want)
		}
	}
}
```

A doc example doubles as a compile-checked, output-verified test. `go test` runs
it and compares stdout to the `// Output:` comment.

Create `example_test.go`:

```go
package parallel

import "fmt"

func Example() {
	out, err := Map([]int{1, 2, 3, 4}, 2, func(n int) (int, error) {
		return n * n, nil
	})
	fmt.Println(out, err)
	// Output: [1 4 9 16] <nil>
}
```

## Review

`Map` is correct when four properties hold together. Order: `results[i]` is always
the result of `items[i]`, because the index decides the slot, not the order tasks
finish — the order test confirms it. Bounding: the buffered-channel send before
each `wg.Go` is the only thing capping in-flight work, and the peak-concurrency
test asserts the bound is never exceeded; note it is deliberately an upper-bound
check, which the semaphore makes unflakeable, rather than a fragile "at least N
ran at once." Error reporting: every task runs to completion and `firstErr`
returns the lowest-indexed failure while the other slots stay filled. Safety: the
per-index writes plus `Wait`'s happens-before edge mean no lock is needed, and
`-race` is the real proof.

Common mistakes for this feature. The first is reaching for the `WaitGroup` to
throttle and discovering it does nothing — the bound must come from the semaphore,
and the slot must be acquired in the loop *before* `wg.Go`, not inside the task,
or every goroutine launches before any of them blocks. The second is forgetting
the `limit < 1` clamp: a `limit` of `0` builds a zero-capacity channel and the
very first `sem <- struct{}{}` blocks forever. The third is growing the result
slices inside the goroutines instead of pre-sizing them, which both reorders
results and races on the slice header; pre-allocating to `len(items)` and writing
by index is what keeps the writes independent.

## Resources

- [`sync.WaitGroup.Go`](https://pkg.go.dev/sync#WaitGroup.Go) — the Go 1.25 method this exercise is built on, including the "must not panic" contract.
- [Go 1.25 release notes: `sync`](https://go.dev/doc/go1.25#sync) — where `WaitGroup.Go` and the `go vet` waitgroup analyzer were introduced.
- [`golang.org/x/sync/errgroup`](https://pkg.go.dev/golang.org/x/sync/errgroup) — the alternative when you need first-error propagation and context cancellation, which `Map` intentionally omits.

---

Back to [00-concepts.md](00-concepts.md) | Next: [02-concurrent-tree-fold.md](02-concurrent-tree-fold.md)
