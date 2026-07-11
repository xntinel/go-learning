# Exercise 6: Concurrent Fan-Out of Upstream Calls with errgroup and Function Literals

Fetching N independent results in parallel — batch key lookups across shards,
parallel calls to several upstreams — is the canonical `errgroup` job: you submit
one anonymous `func() error` per item, let the first error cancel the rest, and
bound how many run at once. This module builds that aggregator, writing each result
to its own index so the shared slice is never a race.

This module is fully self-contained. It uses `golang.org/x/sync/errgroup`; nothing
here imports another exercise.

## What you'll build

```text
aggregator/                   module example.com/aggregator
  go.mod                      requires golang.org/x/sync
  aggregator.go               FetchAll: errgroup fan-out, SetLimit, index writes
  aggregator_test.go          order, first-error-cancels-siblings, SetLimit bound
  cmd/demo/main.go            fan out three lookups
```

- Files: `aggregator.go`, `aggregator_test.go`, `cmd/demo/main.go`.
- Implement: `FetchAll(ctx, keys, limit, fetch)` submitting one `func() error` per key to an `errgroup.Group` created with `WithContext`, bounded by `SetLimit`, each literal writing only `results[i]`.
- Test: the happy path fills every slot in order; a failing literal makes `Wait` return that error and cancel the derived context so siblings observe `ctx.Done`; `SetLimit` bounds peak concurrency (verified against the unbounded case).
- Verify: `go test -count=1 -race ./...`

Set up the module:

```bash
mkdir -p ~/go-exercises/aggregator/cmd/demo
cd ~/go-exercises/aggregator
go mod init example.com/aggregator
go get golang.org/x/sync/errgroup
```

### One task literal per key, writing its own index

`errgroup.WithContext(ctx)` returns a group and a derived context. Each key is
submitted as a `func() error` literal via `g.Go`. Inside the literal, `fetch` is
called with the *group's* context — so when any task returns a non-nil error,
errgroup cancels that context and the remaining tasks that watch it can bail out
early. `g.Wait()` blocks until every submitted task finishes and returns the first
non-nil error.

The subtlety is the shared `results` slice. errgroup synchronizes only around
`Wait`; it does not serialize the tasks' writes. The rule that keeps this race-free
is index partitioning: each literal writes only `results[i]`, its own slot, so no
two goroutines touch the same memory. Because Go 1.22 gives each loop iteration a
fresh `i` and `key`, the literal captures them safely without an argument — but the
write must still target a distinct index. `SetLimit(n)` caps how many tasks run at
once, which matters when N is large and each call holds a scarce resource (an
upstream connection, a database handle).

Create `aggregator.go`:

```go
package aggregator

import (
	"context"
	"fmt"

	"golang.org/x/sync/errgroup"
)

// Fetcher retrieves the value for a key from an upstream or shard.
type Fetcher func(ctx context.Context, key string) (string, error)

// FetchAll fetches every key concurrently, at most limit at a time (limit <= 0
// means unbounded), writing each result to its own index. The first error
// cancels the derived context and is returned from Wait.
func FetchAll(ctx context.Context, keys []string, limit int, fetch Fetcher) ([]string, error) {
	results := make([]string, len(keys))
	g, ctx := errgroup.WithContext(ctx)
	if limit > 0 {
		g.SetLimit(limit)
	}
	for i, key := range keys {
		g.Go(func() error {
			v, err := fetch(ctx, key)
			if err != nil {
				return fmt.Errorf("key %q: %w", key, err)
			}
			results[i] = v
			return nil
		})
	}
	if err := g.Wait(); err != nil {
		return nil, err
	}
	return results, nil
}
```

### The runnable demo

The demo fans out three lookups through a fetcher that upper-cases the key, bounded
to two at a time.

Create `cmd/demo/main.go`:

```go
package main

import (
	"context"
	"fmt"
	"strings"

	"example.com/aggregator"
)

func main() {
	fetch := func(_ context.Context, key string) (string, error) {
		return strings.ToUpper(key), nil
	}
	res, err := aggregator.FetchAll(context.Background(), []string{"a", "b", "c"}, 2, fetch)
	fmt.Println(res, err)
}
```

Run it:

```bash
go run ./cmd/demo
```

Expected output:

```
[A B C] <nil>
```

### Tests

`TestFetchAllOrder` proves every slot is filled and results stay in key order
despite concurrent execution. `TestFirstErrorCancelsSiblings` makes one key fail
immediately and has the others block on `ctx.Done`; it asserts `FetchAll` returns
the error wrapping the sentinel and that the siblings observed cancellation.
`TestSetLimitBoundsConcurrency` measures peak concurrency with a limit and without,
proving the bound is real.

Create `aggregator_test.go`:

```go
package aggregator

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"sync/atomic"
	"testing"
	"time"
)

func TestFetchAllOrder(t *testing.T) {
	t.Parallel()
	keys := []string{"a", "b", "c", "d"}
	fetch := func(_ context.Context, key string) (string, error) {
		return strings.ToUpper(key), nil
	}
	got, err := FetchAll(context.Background(), keys, 2, fetch)
	if err != nil {
		t.Fatalf("FetchAll: %v", err)
	}
	want := []string{"A", "B", "C", "D"}
	for i := range want {
		if got[i] != want[i] {
			t.Fatalf("got %v, want %v", got, want)
		}
	}
}

func TestFirstErrorCancelsSiblings(t *testing.T) {
	t.Parallel()
	sentinel := errors.New("shard down")
	var canceled atomic.Int64
	fetch := func(ctx context.Context, key string) (string, error) {
		if key == "bad" {
			return "", sentinel
		}
		<-ctx.Done() // wait until the failing task cancels the group context
		canceled.Add(1)
		return "", ctx.Err()
	}

	_, err := FetchAll(context.Background(), []string{"a", "bad", "c"}, 0, fetch)
	if !errors.Is(err, sentinel) {
		t.Fatalf("err = %v, want wrapping sentinel", err)
	}
	if canceled.Load() == 0 {
		t.Fatal("sibling tasks did not observe context cancellation")
	}
}

func measurePeak(t *testing.T, limit, keys int) int64 {
	t.Helper()
	var concurrent, peak atomic.Int64
	fetch := func(_ context.Context, key string) (string, error) {
		cur := concurrent.Add(1)
		for {
			p := peak.Load()
			if cur <= p || peak.CompareAndSwap(p, cur) {
				break
			}
		}
		time.Sleep(2 * time.Millisecond)
		concurrent.Add(-1)
		return key, nil
	}
	ks := make([]string, keys)
	for i := range ks {
		ks[i] = fmt.Sprintf("k%d", i)
	}
	if _, err := FetchAll(context.Background(), ks, limit, fetch); err != nil {
		t.Fatalf("FetchAll: %v", err)
	}
	return peak.Load()
}

func TestSetLimitBoundsConcurrency(t *testing.T) {
	t.Parallel()
	if got := measurePeak(t, 2, 20); got > 2 {
		t.Fatalf("bounded peak = %d, want <= 2", got)
	}
	if got := measurePeak(t, 0, 20); got <= 2 {
		t.Fatalf("unbounded peak = %d, want > 2 (SetLimit not actually bounding?)", got)
	}
}

func ExampleFetchAll() {
	fetch := func(_ context.Context, key string) (string, error) {
		return strings.ToUpper(key), nil
	}
	res, err := FetchAll(context.Background(), []string{"x", "y"}, 2, fetch)
	fmt.Println(res, err)
	// Output: [X Y] <nil>
}
```

## Review

The aggregator is correct when three things hold under `-race`: every slot is
filled in key order, the first error both propagates out of `Wait` and cancels the
derived context so siblings can abandon their work, and `SetLimit` actually bounds
concurrency. The `-race` run is what certifies the index-partitioned writes — the
central discipline here. The mistake the concepts warn about is letting each task
`append` to a shared slice: errgroup does not synchronize task bodies, only `Wait`,
so an unpartitioned append is a race even though the group looks orderly. Keep each
literal writing to exactly its own index, and pass the group's context (not the
original) into `fetch` so cancellation reaches the tasks.

## Resources

- [golang.org/x/sync/errgroup](https://pkg.go.dev/golang.org/x/sync/errgroup)
- [errgroup.WithContext](https://pkg.go.dev/golang.org/x/sync/errgroup#WithContext)
- [(*errgroup.Group).SetLimit](https://pkg.go.dev/golang.org/x/sync/errgroup#Group.SetLimit)
- [Go blog: Pipelines and cancellation](https://go.dev/blog/pipelines)

---

Back to [00-concepts.md](00-concepts.md) | Previous: [05-http-middleware-function-literal.md](05-http-middleware-function-literal.md) | Next: [07-transaction-callback-runner.md](07-transaction-callback-runner.md)
