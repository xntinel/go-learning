# Exercise 3: Bounded Concurrent Enrichment With First-Error Cancellation

Enriching a batch of records with data from an upstream — hydrate each order with
its customer, each event with its geo — is fan-out where you usually want the
opposite of the previous exercise's per-item errors: if one fetch fails hard, stop
the rest and return that error. This exercise builds that with `errgroup.Group`,
using `SetLimit` to cap in-flight requests and `WithContext` so the first failing
fetch cancels the others.

This module is fully self-contained. It uses `golang.org/x/sync/errgroup`.

## What you'll build

```text
enrich/                    independent module: example.com/enrich
  go.mod                   go 1.25; require golang.org/x/sync
  enrich.go                Enrich(ctx, ids, limit, fetch) ([]string, error)
  cmd/
    demo/
      main.go              runnable demo: enrich 6 ids, limit 3
  enrich_test.go           concurrency-cap, first-error-cancels, all-success tests, -race
```

- Files: `enrich.go`, `cmd/demo/main.go`, `enrich_test.go`.
- Implement: an `Enrich` that fetches supplementary data for every id across at most `limit` concurrent requests, writing each result into a preallocated slice by index, cancelling on the first error.
- Test: at most `limit` fetches run at once, `Wait` returns the first error and remaining fetchers observe cancellation, and the all-success path fills every slot and returns `nil`.
- Verify: `go test -count=1 -race ./...`

Set up the module:

```bash
mkdir -p go-solutions/13-goroutines-and-channels/13-goroutine-pools/03-errgroup-bounded-enrichment/cmd/demo
cd go-solutions/13-goroutines-and-channels/13-goroutine-pools/03-errgroup-bounded-enrichment
go get golang.org/x/sync/errgroup
```

### Why errgroup here and not a hand-rolled pool

`errgroup.Group` packages three things a batch enrichment needs and a bare
`WaitGroup` does not. `SetLimit(n)` caps the number of concurrently running
functions: `g.Go` blocks when `n` are already active and only proceeds when one
finishes, so the group *is* the concurrency bound — no separate semaphore needed.
`WithContext` returns a derived context that the group cancels the moment any
`g.Go` function returns a non-nil error; every other function that watches that
context sees `ctx.Done()` fire and can stop early instead of finishing doomed
work. And `Wait` returns the *first* such error (and waits for all functions to
return before it does), so the caller gets one representative failure and a
guarantee that nothing is still running.

The important consequence of `WithContext` is that the context you pass *into* the
fetch is the group's cancellable context, not the original. When fetch #2 fails,
the group cancels that context; fetch #5, blocked on a slow upstream call that
respects context, gets `ctx.Err()` and returns immediately rather than waiting out
its own timeout. That is first-error cancellation doing real work: it stops the
service from spending on requests whose result is already going to be discarded.

### Writing results by index needs no lock

Each `g.Go` closure writes `out[i]`, a distinct preallocated slot, so there is no
shared-slice data race — no two goroutines ever touch the same index, and the
slice header itself is not mutated (its length and backing array are fixed before
the goroutines start). This is the same index-ownership trick as the previous
exercise, without the results channel: because the caller already knows the arity
(one result per id) and wants them in input order, writing directly into the
preallocated slice is simpler than collecting and reordering. The `-race` detector
will confirm there is no overlap.

Note the loop needs no `i := i` capture: since Go 1.22 each iteration has its own
`i` and `id`, so the closure captures the right values.

Create `enrich.go`:

```go
package enrich

import (
	"context"

	"golang.org/x/sync/errgroup"
)

// Enrich fetches supplementary data for every id, running at most limit fetches
// concurrently. It writes each result into a preallocated slice by index. The
// first fetch to fail cancels the context handed to the remaining fetches, and
// Enrich returns that error with a nil slice.
func Enrich(ctx context.Context, ids []string, limit int, fetch func(context.Context, string) (string, error)) ([]string, error) {
	out := make([]string, len(ids))
	g, ctx := errgroup.WithContext(ctx)
	g.SetLimit(limit)

	for i, id := range ids {
		g.Go(func() error {
			v, err := fetch(ctx, id)
			if err != nil {
				return err
			}
			out[i] = v
			return nil
		})
	}

	if err := g.Wait(); err != nil {
		return nil, err
	}
	return out, nil
}
```

### The runnable demo

The demo enriches six ids with a fetcher that just prefixes them, capped at three
concurrent fetches, and prints the results in order.

Create `cmd/demo/main.go`:

```go
package main

import (
	"context"
	"fmt"

	"example.com/enrich"
)

func main() {
	ids := []string{"a", "b", "c", "d", "e", "f"}

	out, err := enrich.Enrich(context.Background(), ids, 3,
		func(_ context.Context, id string) (string, error) {
			return "user:" + id, nil
		})
	if err != nil {
		fmt.Println("error:", err)
		return
	}
	for i, v := range out {
		fmt.Printf("%d %s\n", i, v)
	}
}
```

Run it:

```bash
go run ./cmd/demo
```

Expected output:

```
0 user:a
1 user:b
2 user:c
3 user:d
4 user:e
5 user:f
```

### Tests

`TestConcurrencyCapped` uses a fetcher that increments a running gauge, records
the peak with an atomic compare-and-swap, sleeps, then decrements, and asserts the
peak never exceeds `limit` even with many more ids than the limit.
`TestFirstErrorCancels` injects a fetcher that fails on one id and, on every other
id, blocks until the context is cancelled and then returns `ctx.Err()`; it asserts
`Enrich` returns the injected sentinel via `errors.Is` and that the other fetchers
observed cancellation. `TestAllSuccess` asserts the happy path fills every slot in
order and returns `nil`.

Create `enrich_test.go`:

```go
package enrich

import (
	"context"
	"errors"
	"fmt"
	"strconv"
	"sync/atomic"
	"testing"
	"time"
)

var errUpstream = errors.New("upstream failed")

func TestConcurrencyCapped(t *testing.T) {
	t.Parallel()

	const limit = 3
	ids := make([]string, 20)
	for i := range ids {
		ids[i] = strconv.Itoa(i)
	}

	var running, peak atomic.Int64
	_, err := Enrich(context.Background(), ids, limit,
		func(_ context.Context, id string) (string, error) {
			cur := running.Add(1)
			for {
				p := peak.Load()
				if cur <= p || peak.CompareAndSwap(p, cur) {
					break
				}
			}
			time.Sleep(5 * time.Millisecond)
			running.Add(-1)
			return id, nil
		})
	if err != nil {
		t.Fatalf("Enrich returned %v, want nil", err)
	}
	if got := peak.Load(); got > limit {
		t.Fatalf("peak concurrency = %d, want <= %d", got, limit)
	}
}

func TestFirstErrorCancels(t *testing.T) {
	t.Parallel()

	ids := []string{"0", "1", "2", "3", "4", "5"}
	var cancelled atomic.Int64

	_, err := Enrich(context.Background(), ids, 6,
		func(ctx context.Context, id string) (string, error) {
			if id == "2" {
				return "", errUpstream
			}
			// Others wait to observe cancellation triggered by id=="2".
			select {
			case <-ctx.Done():
				cancelled.Add(1)
				return "", ctx.Err()
			case <-time.After(time.Second):
				return id, nil
			}
		})

	if !errors.Is(err, errUpstream) {
		t.Fatalf("Enrich err = %v, want errUpstream", err)
	}
	if got := cancelled.Load(); got == 0 {
		t.Fatal("no fetcher observed cancellation; first error did not propagate")
	}
}

func TestAllSuccess(t *testing.T) {
	t.Parallel()

	ids := []string{"a", "b", "c"}
	out, err := Enrich(context.Background(), ids, 2,
		func(_ context.Context, id string) (string, error) { return "v:" + id, nil })
	if err != nil {
		t.Fatalf("Enrich returned %v, want nil", err)
	}
	want := []string{"v:a", "v:b", "v:c"}
	for i := range want {
		if out[i] != want[i] {
			t.Fatalf("out[%d] = %q, want %q", i, out[i], want[i])
		}
	}
}

func ExampleEnrich() {
	out, _ := Enrich(context.Background(), []string{"x", "y"}, 2,
		func(_ context.Context, id string) (string, error) {
			return fmt.Sprintf("id-%s", id), nil
		})
	fmt.Println(out)
	// Output: [id-x id-y]
}
```

## Review

`Enrich` is correct when the concurrency cap, the cancellation, and the
result-placement all hold. The cap comes entirely from `SetLimit`: with it,
`TestConcurrencyCapped` never sees more than `limit` fetchers running; drop the
`SetLimit` call and the same test would see up to 20 — that is the difference
between using `errgroup` as a bound and using it as a mere `WaitGroup`. The
cancellation comes from `WithContext`: `TestFirstErrorCancels` proves the first
error both surfaces from `Wait` (via `errors.Is`) and cancels the context the
other fetchers watch. Result placement is race-free because each goroutine owns a
distinct `out[i]`.

The mistakes to avoid: forgetting `SetLimit` (unbounded goroutines despite using
`errgroup`); passing the *outer* context into `fetch` instead of the group's
derived `ctx` (then cancellation never reaches the fetchers); and returning
`out` alongside a non-nil error (a failed batch has holes, so return `nil` on
error). Run `-race` to confirm the by-index writes never overlap.

## Resources

- [`golang.org/x/sync/errgroup`](https://pkg.go.dev/golang.org/x/sync/errgroup) — `Group`, `WithContext`, `SetLimit`, `Go`, `Wait`.
- [`context`](https://pkg.go.dev/context) — the cancellation propagated by `WithContext`.
- [Go Blog: Pipelines and cancellation](https://go.dev/blog/pipelines) — first-error cancellation and stopping doomed work early.

---

Back to [00-concepts.md](00-concepts.md) | Previous: [02-fan-out-fan-in-results.md](02-fan-out-fan-in-results.md) | Next: [04-weighted-semaphore-cost.md](04-weighted-semaphore-cost.md)
