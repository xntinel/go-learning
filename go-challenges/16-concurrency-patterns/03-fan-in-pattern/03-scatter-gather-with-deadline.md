# Exercise 3: Scatter-Gather With a Deadline

Production search and aggregation almost never wait for the slowest backend. You scatter a query to many replicas or services, gather the answers that arrive before a deadline, and return that partial set — a fast, mostly-complete answer beats a slow, perfect one. This exercise builds that best-effort fan-in: a `Gather` that runs every search concurrently, collects results until a `context` deadline fires, counts the laggards as timed out, and abandons them without leaking a single goroutine.

This module is fully self-contained: its own `go mod init`, every symbol inline, its own demo and tests. It imports no other exercise.

## What you'll build

```text
scattergather.go     Result, Search, Gather (deadline-bounded fan-in)
cmd/
  demo/
    main.go          scatter to three backends, gather before a 300ms deadline
scattergather_test.go all-complete, abandon-slow, per-source error, no-leak late send
```

- Files: `scattergather.go`, `cmd/demo/main.go`, `scattergather_test.go`.
- Implement: `Gather(ctx context.Context, searches map[string]Search) ([]Result, int, []error)`, plus the `Result` type and the `Search` function type.
- Test: all searches complete before a generous deadline; slow searches are abandoned and counted as timed out under a short deadline; a per-source error is collected separately; an abandoned slow goroutine can still finish its send after `Gather` returns (no leak).
- Verify: `go test -race ./...`

### The gather loop, the deadline, and why the sink must be buffered

`Gather` scatters first: it launches one goroutine per search, each calling `search(ctx)` and sending its outcome — a result or an error — onto a shared channel. Then it gathers in a loop that runs at most once per search, each iteration selecting between receiving the next outcome and `ctx.Done()`. As long as outcomes keep arriving before the deadline, the loop accumulates them. The moment `ctx.Done()` fires, the loop stops and returns what it has, computing the number of unfinished sources as the total minus what was gathered minus what errored. The caller gets three things: the results that made it, a count of how many timed out, and the per-source errors.

The correctness trap is the abandoned producers. When the deadline fires, the slow searches are still running. Their goroutines will eventually return and try to send their outcome — but `Gather` has stopped receiving. If the outcome channel were unbuffered, every one of those sends would block forever and the goroutines would leak; under enough load that is a slow-motion crash. The fix is to buffer the channel to the number of searches, so every producer can always complete its send and exit, whether or not anyone is still listening. Buffering the sink to the producer count is the defining move of a leak-free best-effort fan-in: it decouples "the consumer stopped caring" from "the producer can finish." The searches themselves must also honor `ctx` — a backend that ignores cancellation keeps doing real work after it's been abandoned — but honoring the context governs the *work*; buffering the channel governs the *send*, and both are needed.

Create `scattergather.go`:

```go
package scattergather

import "context"

// Result is one backend's answer to a scattered query.
type Result struct {
	Source string
	Hits   []string
}

// Search queries one backend. It must respect ctx: when ctx is done it should
// stop and return promptly, so an abandoned search does no further work.
type Search func(ctx context.Context) (Result, error)

// Gather runs every search concurrently and collects the outcomes that arrive
// before ctx's deadline. Searches still running when ctx is done are abandoned;
// their goroutines finish and exit on their own because the outcome channel is
// buffered to the number of searches, so a late send never blocks. Gather
// returns the gathered results (in completion order), the number of sources that
// did not finish in time, and the per-source errors collected so far.
func Gather(ctx context.Context, searches map[string]Search) ([]Result, int, []error) {
	type outcome struct {
		res Result
		err error
	}
	// Buffered to len(searches): every producer can send and exit even after
	// Gather has returned, so no abandoned goroutine leaks.
	outcomes := make(chan outcome, len(searches))

	for name, search := range searches {
		go func() {
			res, err := search(ctx)
			res.Source = name
			outcomes <- outcome{res: res, err: err}
		}()
	}

	var gathered []Result
	var errs []error
	for range searches {
		select {
		case o := <-outcomes:
			if o.err != nil {
				errs = append(errs, o.err)
			} else {
				gathered = append(gathered, o.res)
			}
		case <-ctx.Done():
			timedOut := len(searches) - len(gathered) - len(errs)
			return gathered, timedOut, errs
		}
	}
	return gathered, 0, errs
}
```

The loop ranges over `searches` only to bound the receive count at one per source; the values are ignored. Each goroutine stamps `res.Source = name` from the per-iteration loop variable — under Go 1.22+ scoping `name` and `search` are fresh on every iteration, so the closure captures the right pair without the old `name := name` shadowing. If `ctx` is already done when `Gather` starts, the first `select` can take the `ctx.Done()` branch immediately and report every source as timed out, which is the correct best-effort answer for a query that arrived with no time budget.

### The runnable demo

The demo scatters to three backends: two return instantly, one blocks until the context deadline and is abandoned. With a 300ms deadline the two fast backends are always gathered and the slow one is always counted as timed out. Results are sorted by source before printing because completion order is not deterministic.

Create `cmd/demo/main.go`:

```go
package main

import (
	"context"
	"fmt"
	"sort"
	"time"

	"example.com/scattergather"
)

func instant(hits ...string) scattergather.Search {
	return func(ctx context.Context) (scattergather.Result, error) {
		return scattergather.Result{Hits: hits}, nil
	}
}

func slow() scattergather.Search {
	return func(ctx context.Context) (scattergather.Result, error) {
		<-ctx.Done()
		return scattergather.Result{}, ctx.Err()
	}
}

func main() {
	ctx, cancel := context.WithTimeout(context.Background(), 300*time.Millisecond)
	defer cancel()

	results, timedOut, errs := scattergather.Gather(ctx, map[string]scattergather.Search{
		"alpha": instant("a1", "a2"),
		"beta":  instant("b1"),
		"gamma": slow(),
	})

	sort.Slice(results, func(i, j int) bool { return results[i].Source < results[j].Source })
	for _, r := range results {
		fmt.Printf("%s: %v\n", r.Source, r.Hits)
	}
	fmt.Printf("timed out: %d\n", timedOut)
	fmt.Printf("errors: %d\n", len(errs))
}
```

Run it:

```bash
go run ./cmd/demo
```

Expected output:

```
alpha: [a1 a2]
beta: [b1]
timed out: 1
errors: 0
```

### Tests

The tests pin the four behaviors. `TestGatherCollectsAllBeforeDeadline` gives a generous deadline and three instant searches and asserts all three are gathered with nothing timed out. `TestGatherAbandonsSlowSources` mixes instant and block-until-deadline searches under a short deadline and asserts the fast ones are gathered and the slow ones counted as timed out. `TestGatherReportsPerSourceErrors` asserts a failing search lands in the error slice, not the results. `TestGatherLateSourceFinishesWithoutLeaking` proves an abandoned goroutine can still complete its send after `Gather` returns — the property the buffered channel guarantees.

Create `scattergather_test.go`:

```go
package scattergather

import (
	"context"
	"errors"
	"testing"
	"time"
)

func instant(hits ...string) Search {
	return func(ctx context.Context) (Result, error) {
		return Result{Hits: hits}, nil
	}
}

func blockUntilDone() Search {
	return func(ctx context.Context) (Result, error) {
		<-ctx.Done()
		return Result{}, ctx.Err()
	}
}

func failing(err error) Search {
	return func(ctx context.Context) (Result, error) {
		return Result{}, err
	}
}

func TestGatherCollectsAllBeforeDeadline(t *testing.T) {
	t.Parallel()

	ctx, cancel := context.WithTimeout(context.Background(), time.Second)
	defer cancel()

	got, timedOut, errs := Gather(ctx, map[string]Search{
		"a": instant("x"),
		"b": instant("y"),
		"c": instant("z"),
	})
	if len(got) != 3 {
		t.Fatalf("gathered %d, want 3", len(got))
	}
	if timedOut != 0 {
		t.Fatalf("timedOut %d, want 0", timedOut)
	}
	if len(errs) != 0 {
		t.Fatalf("errs %v, want none", errs)
	}
}

func TestGatherAbandonsSlowSources(t *testing.T) {
	t.Parallel()

	ctx, cancel := context.WithTimeout(context.Background(), 300*time.Millisecond)
	defer cancel()

	got, timedOut, errs := Gather(ctx, map[string]Search{
		"fast1": instant("a"),
		"fast2": instant("b"),
		"slow1": blockUntilDone(),
		"slow2": blockUntilDone(),
	})
	if len(got) != 2 {
		t.Fatalf("gathered %d fast results, want 2", len(got))
	}
	if timedOut != 2 {
		t.Fatalf("timedOut %d, want 2", timedOut)
	}
	if len(errs) != 0 {
		t.Fatalf("errs %v, want none", errs)
	}
}

func TestGatherReportsPerSourceErrors(t *testing.T) {
	t.Parallel()

	ctx, cancel := context.WithTimeout(context.Background(), time.Second)
	defer cancel()

	boom := errors.New("backend down")
	got, timedOut, errs := Gather(ctx, map[string]Search{
		"ok":  instant("a"),
		"bad": failing(boom),
	})
	if len(got) != 1 {
		t.Fatalf("gathered %d, want 1", len(got))
	}
	if timedOut != 0 {
		t.Fatalf("timedOut %d, want 0", timedOut)
	}
	if len(errs) != 1 || !errors.Is(errs[0], boom) {
		t.Fatalf("errs %v, want [%v]", errs, boom)
	}
}

func TestGatherLateSourceFinishesWithoutLeaking(t *testing.T) {
	t.Parallel()

	ctx, cancel := context.WithTimeout(context.Background(), 100*time.Millisecond)
	defer cancel()

	released := make(chan struct{})
	late := func(ctx context.Context) (Result, error) {
		<-ctx.Done()
		close(released)
		return Result{Hits: []string{"late"}}, nil
	}

	_, timedOut, _ := Gather(ctx, map[string]Search{
		"fast": instant("a"),
		"late": Search(late),
	})
	if timedOut != 1 {
		t.Fatalf("timedOut %d, want 1", timedOut)
	}

	// The abandoned goroutine must still be able to run to completion: it sends
	// into the buffered channel and exits. If the channel were unbuffered it
	// would block forever here.
	select {
	case <-released:
	case <-time.After(time.Second):
		t.Fatal("late source never completed; goroutine leaked or blocked on send")
	}
}
```

## Review

`Gather` is correct when two things hold together: the gather loop returns on `ctx.Done()` with an accurate timed-out count, and no abandoned producer can leak. The count is `len(searches) - len(gathered) - len(errs)` computed at the moment the deadline fires, which is exactly the sources that had not yet delivered an outcome. The no-leak property rests entirely on the buffered channel: `TestGatherLateSourceFinishesWithoutLeaking` forces a producer to send after `Gather` has returned and asserts it completes — flip the buffer to unbuffered and that test hangs, which is the leak made visible.

The traps are the ones best-effort aggregation invites. An unbuffered sink turns every laggard into a permanently blocked goroutine. A search that ignores `ctx` keeps consuming CPU and connections after it has been abandoned, so the cancellation has to reach the actual work, not just the send. Counting timeouts by subtraction only works if you compute it at the `ctx.Done()` branch, before any further receive — do it after and the arithmetic drifts. And running this under `-race` is not optional: the producers and the gather loop share the outcome channel, and the late-send path is precisely where a missing buffer or a data race would surface.

## Resources

- [`context`](https://pkg.go.dev/context) — `WithTimeout` and `Context.Done`, the deadline machinery the gather loop selects on.
- [Go Blog: Pipelines and cancellation](https://go.dev/blog/pipelines) — fan-in with explicit cancellation, the foundation this deadline-bounded gather builds on.
- [Go Blog: Go Concurrency Patterns: Context](https://go.dev/blog/context) — how a `context` propagates a deadline and cancellation across a tree of concurrent calls.

---

Back to [02-ordered-shard-merge.md](02-ordered-shard-merge.md) | Next: [Worker Pool Pattern](../04-worker-pool-pattern/04-worker-pool-pattern.md)
