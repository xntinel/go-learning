# Exercise 9: Bounded, Cancelable Pipeline with errgroup (error propagation + fan-out limit)

The raw-channel pipelines of Exercises 1 and 2 are where you start; this is where you
land. A production pipeline must do two things the naive version cannot: propagate the
first error out to the caller, and cancel the rest of the work when something fails so
you do not burn CPU on results you will discard. `golang.org/x/sync/errgroup` with
`WithContext` and `SetLimit(N)` gives you exactly that — first-error propagation,
context cancellation of the whole group, and a bounded fan-out — while buffered stage
channels still cap memory. This exercise is the errgroup evolution of the worker pool.

This module is self-contained; it depends only on `golang.org/x/sync/errgroup`.

## What you'll build

```text
errpipeline/                 module: example.com/errpipeline
  go.mod                     go 1.26, require golang.org/x/sync
  errpipeline.go             RunPipeline(ctx, inputs, limit, transform) ([]int, error)
  cmd/
    demo/
      main.go                run happy path and a failing path, print outcomes
  errpipeline_test.go        all-processed happy path, first-error propagation + early stop, SetLimit cap
```

- Files: `errpipeline.go`, `cmd/demo/main.go`, `errpipeline_test.go`.
- Implement: `RunPipeline(ctx, inputs []int, limit int, transform func(context.Context, int) (int, error)) ([]int, error)` using `errgroup.WithContext` + `SetLimit(limit)` and a buffered results channel.
- Test: happy path processes all inputs and returns `nil`; a transform error propagates out of `Wait`, cancels the context, and stops downstream work early; `SetLimit(N)` caps concurrent stage goroutines.
- Verify: `go test -count=1 -race ./...`

Set up the module:

```bash
mkdir -p ~/go-exercises/errpipeline/cmd/demo
cd ~/go-exercises/errpipeline
go mod init example.com/errpipeline
go mod edit -go=1.26
go get golang.org/x/sync/errgroup
```

### What errgroup adds over the raw pool

`errgroup.WithContext(ctx)` returns a `*Group` and a derived `ctx`. Three behaviors make
it the production choice. First, `Group.Go(f)` runs `f` in a goroutine, and `Group.Wait()`
returns the *first* non-nil error any `f` returned — the raw channel pool has nowhere to
put an error, so it drops them; here the caller gets it. Second, the derived context is
*cancelled* the moment the first `f` returns an error (or when `Wait` returns), so every
other worker can observe `ctx.Done()` and stop early instead of doing work whose result
will be thrown away. Third, `Group.SetLimit(N)` bounds the fan-out: `Group.Go` blocks
when N goroutines are already active and only proceeds once one finishes, so the loop that
launches work is itself throttled to at most N concurrent stage goroutines — the same
bound a `chan struct{}` semaphore gives you, but integrated with the error and
cancellation machinery.

The structure: `SetLimit(limit)`, then loop over inputs calling `g.Go`. Because `g.Go`
blocks at the limit, the loop is the bounded producer. Each worker first checks the
context (a cancelled context means an earlier worker already failed, so skip the work),
runs `transform`, returns any error (which cancels the group), and otherwise sends its
result to a *buffered* results channel. The channel is buffered to `len(inputs)` so a
worker never blocks handing back a result — that is what keeps the launching loop from
deadlocking against a full results buffer. After the loop, `g.Wait()` returns the first
error (or nil); then close and drain the results channel. On error the caller gets
`(nil, err)`; on success, the collected results.

Crucially, `transform` must respect the context — check `ctx.Err()` and return promptly
on cancellation — or the "stop early" guarantee is empty: errgroup cancels the context,
but only work that *watches* the context actually stops. This is the same producer-leak
lesson from the concepts section, now enforced by the group.

Create `errpipeline.go`:

```go
package errpipeline

import (
	"context"

	"golang.org/x/sync/errgroup"
)

// RunPipeline applies transform to every input with at most `limit` concurrent
// goroutines. The first transform error is returned to the caller and cancels the
// derived context so remaining workers stop early. Result order is not preserved.
func RunPipeline(
	ctx context.Context,
	inputs []int,
	limit int,
	transform func(context.Context, int) (int, error),
) ([]int, error) {
	g, ctx := errgroup.WithContext(ctx)
	if limit < 1 {
		limit = len(inputs)
	}
	if limit > 0 {
		g.SetLimit(limit)
	}

	results := make(chan int, len(inputs)) // buffered so a worker never blocks on handback

	for _, in := range inputs {
		g.Go(func() error {
			if err := ctx.Err(); err != nil {
				return err // an earlier worker already failed; stop early
			}
			out, err := transform(ctx, in)
			if err != nil {
				return err
			}
			results <- out
			return nil
		})
	}

	err := g.Wait()
	close(results)

	if err != nil {
		return nil, err
	}
	out := make([]int, 0, len(inputs))
	for r := range results {
		out = append(out, r)
	}
	return out, nil
}
```

### The runnable demo

The demo runs a happy path (square each input) and a failing path (fail on a sentinel
value), printing both outcomes.

Create `cmd/demo/main.go`:

```go
package main

import (
	"context"
	"errors"
	"fmt"
	"sort"

	"example.com/errpipeline"
)

func main() {
	square := func(_ context.Context, n int) (int, error) { return n * n, nil }
	out, err := errpipeline.RunPipeline(context.Background(), []int{1, 2, 3, 4, 5}, 2, square)
	sort.Ints(out)
	fmt.Printf("happy: %v err=%v\n", out, err)

	errBad := errors.New("bad record")
	failOn3 := func(_ context.Context, n int) (int, error) {
		if n == 3 {
			return 0, errBad
		}
		return n * n, nil
	}
	out, err = errpipeline.RunPipeline(context.Background(), []int{1, 2, 3, 4, 5}, 2, failOn3)
	fmt.Printf("failing: out=%v err=%v\n", out, err)
}
```

Run it:

```bash
go run ./cmd/demo
```

Expected output (the happy path returns nil error; the failing path returns an empty
slice and the sentinel error):

```
happy: [1 4 9 16 25] err=<nil>
failing: out=[] err=bad record
```

### Tests

`TestHappyPathProcessesAll` asserts the happy path returns every squared input and a nil
error. `TestFirstErrorPropagatesAndStopsEarly` fails the first launched input with `limit=1`
(strictly sequential launch order) and asserts `RunPipeline` returns that sentinel (via
`errors.Is`); early-stop is proven directly by an `atomic.Int64` that counts how many
inputs' `transform` was actually *invoked* — the failing item cancels the context, and
`RunPipeline`'s `ctx.Err()` guard then short-circuits every later item before its
transform runs, so `invoked` stays far below `len(inputs)`. Remove the guard (or break
cancellation propagation) and all ten transforms run, `invoked` reaches `len(inputs)`,
and the test fails.
`TestSetLimitCapsConcurrency` uses an `atomic.Int64` peak counter inside `transform` to
assert observed concurrency never exceeds the limit. All run under `-race`.

Create `errpipeline_test.go`:

```go
package errpipeline

import (
	"context"
	"errors"
	"fmt"
	"slices"
	"sync/atomic"
	"testing"
	"time"
)

func TestHappyPathProcessesAll(t *testing.T) {
	t.Parallel()

	square := func(_ context.Context, n int) (int, error) { return n * n, nil }
	out, err := RunPipeline(context.Background(), []int{1, 2, 3, 4}, 2, square)
	if err != nil {
		t.Fatalf("err = %v, want nil", err)
	}
	slices.Sort(out)
	if want := []int{1, 4, 9, 16}; !slices.Equal(out, want) {
		t.Fatalf("out = %v, want %v", out, want)
	}
}

func TestFirstErrorPropagatesAndStopsEarly(t *testing.T) {
	t.Parallel()

	errBad := errors.New("bad record")
	var invoked atomic.Int64 // counts how many inputs' transform actually ran
	transform := func(ctx context.Context, n int) (int, error) {
		invoked.Add(1)
		if n == 1 {
			return 0, errBad // fail the very first item, cancelling the derived context
		}
		return n * n, nil
	}

	inputs := []int{1, 2, 3, 4, 5, 6, 7, 8, 9, 10}
	// limit=1 forces strictly sequential launch order: item 1 fails and cancels the
	// group's context before any later item runs, so RunPipeline's own ctx.Err() guard
	// short-circuits every later item and never even invokes its transform.
	out, err := RunPipeline(context.Background(), inputs, 1, transform)

	if !errors.Is(err, errBad) {
		t.Fatalf("err = %v, want errBad", err)
	}
	if out != nil {
		t.Fatalf("out = %v, want nil on error", out)
	}
	// Direct evidence of early-stop: far fewer than all inputs had their transform
	// invoked. Without the ctx.Err() guard (or if cancellation did not propagate),
	// all 10 transforms would run and invoked would reach len(inputs); this fails then.
	if got := invoked.Load(); got >= int64(len(inputs)) {
		t.Fatalf("transform invoked %d times; early-stop did not skip downstream work (want < %d)", got, len(inputs))
	}
}

func ExampleRunPipeline() {
	double := func(_ context.Context, n int) (int, error) { return n * 2, nil }
	out, err := RunPipeline(context.Background(), []int{1, 2, 3, 4}, 2, double)
	slices.Sort(out) // results are unordered; sort for a deterministic print
	fmt.Println(out, err)
	// Output: [2 4 6 8] <nil>
}

func TestSetLimitCapsConcurrency(t *testing.T) {
	t.Parallel()

	const limit = 3
	var live, max atomic.Int64
	transform := func(_ context.Context, n int) (int, error) {
		cur := live.Add(1)
		for {
			m := max.Load()
			if cur <= m || max.CompareAndSwap(m, cur) {
				break
			}
		}
		time.Sleep(time.Millisecond)
		live.Add(-1)
		return n, nil
	}

	inputs := make([]int, 50)
	for i := range inputs {
		inputs[i] = i
	}
	if _, err := RunPipeline(context.Background(), inputs, limit, transform); err != nil {
		t.Fatalf("err = %v, want nil", err)
	}
	if got := max.Load(); got > limit {
		t.Fatalf("peak concurrency = %d, exceeds SetLimit(%d)", got, limit)
	}
}
```

## Review

The pipeline is production-grade when three properties hold: `Wait` returns the first
error (proven by `errors.Is` against the injected sentinel), the derived context cancels
so downstream work stops early (proven by counting `transform` invocations: after the
first item fails, `RunPipeline`'s `ctx.Err()` guard skips the rest, so far fewer than
`len(inputs)` transforms ever run), and
`SetLimit(N)` caps the fan-out (proven by the `atomic.Int64` peak never exceeding N). The
buffered results channel sized to `len(inputs)` is what keeps the launching loop from
deadlocking against a full buffer while `g.Go` throttles it. The subtle requirement is
that `transform` must watch `ctx` — errgroup cancels the context, but only work that
checks it actually stops; a `transform` that ignores cancellation makes the "stop early"
guarantee hollow. This is the version a senior ships: the raw-channel pipeline drops
errors and leaks producers on early exit; the errgroup pipeline handles both.

## Resources

- [pkg.go.dev: golang.org/x/sync/errgroup](https://pkg.go.dev/golang.org/x/sync/errgroup) — `WithContext`, `SetLimit`, `Go`, `Wait`.
- [The Go Blog: Go Concurrency Patterns — Pipelines and cancellation](https://go.dev/blog/pipelines) — the error-and-cancellation problems errgroup solves.
- [pkg.go.dev: context](https://pkg.go.dev/context) — cancellation propagation the derived context relies on.

---

Back to [00-concepts.md](00-concepts.md) | Prev: [08-backpressure-benchmark.md](08-backpressure-benchmark.md) | Next: [10-outbox-relay-unbuffered-confirm-handoff.md](10-outbox-relay-unbuffered-confirm-handoff.md)
