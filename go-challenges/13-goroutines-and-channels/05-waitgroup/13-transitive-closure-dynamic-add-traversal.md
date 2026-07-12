# Exercise 13: Dynamic Add During A Concurrent Dependency Closure

**Level: Advanced**

A deploy planner must compute the transitive closure of a package's dependencies,
but each node's direct dependencies are discovered only when that node is visited —
there is no pre-built adjacency list to iterate. The naive breadth-first loop is
serial and slow on a wide graph; the naive parallel version either double-expands
nodes across cycles and diamonds or races its `WaitGroup`. This module builds a
concurrent closure where workers fan out and, on finding an unvisited child, call
`wg.Add` on the same `WaitGroup` and launch another goroutine mid-flight.

This module is self-contained: its own module, a `closure` package, a demo, and
tests. Nothing here imports another exercise.

## What you'll build

```text
closure/                     independent module: example.com/closure
  go.mod                     go 1.26
  closure.go                 Closure fans out, Adds mid-flight, bounds concurrency, joins errors
  cmd/demo/main.go           runnable demo: closure over a graph with a diamond and a cycle
  closure_test.go            each node expanded once; bounded concurrency; error joined; leak-free
```

- Files: `closure.go`, `cmd/demo/main.go`, `closure_test.go`.
- Implement: `Closure(ctx context.Context, roots []string, maxConcurrency int, neighbors Neighbors) ([]string, error)` and `type Neighbors func(ctx context.Context, node string) ([]string, error)`.
- Test: DAG, cycle, and diamond each expand every reachable node exactly once (expansion-count map all ones) and return a deterministic sorted set; a bounded in-flight gauge never exceeds `maxConcurrency`; a neighbor error is joined and the traversal still terminates; leak-freedom via `go.uber.org/goleak`.
- Verify: `go test -count=1 -race ./...`

Set up the module:

```bash
go mod edit -go=1.26
go get go.uber.org/goleak
go mod tidy
```

### Why Add can run concurrently with Wait here

The classic rule is "call `Add` before the `go` statement, in the launching
goroutine." This exercise deliberately explores the subtler, still-legal case: a
running goroutine calling `Add` on a `WaitGroup` that another goroutine is already
`Wait`-ing on. The `WaitGroup` documentation permits this under one precise
condition — the new `Add` with a positive delta must happen-before the counter could
reach zero. Concretely, the goroutine that discovers a child and calls `wg.Add(1)`
still holds its own positive counter: its `defer wg.Done()` has not run yet. So the
counter can never be observed at zero between the parent's `Done` and the child's
`Add`, because the child's `Add` is ordered before the parent's `Done`. That single
ordering is the invariant this module pins down.

The traversal protocol is:

1. De-duplicate the roots, mark each in the `visited` set, and `wg.Add(len(seeds))`
   before launching any goroutine or calling `Wait`. This seed `Add` happens-before
   `Wait`, so the counter reflects every seeded task from the start.
2. Each `visit` goroutine defers `wg.Done()` on its first line, then acquires a
   semaphore slot. The semaphore — a buffered `chan struct{}` of capacity
   `maxConcurrency` — bounds how many goroutines are simultaneously expanding, so a
   graph with thousands of nodes cannot spawn thousands of concurrent workers.
3. The goroutine calls `neighbors` for its node. For each returned child, it locks
   the mutex, checks `visited`, and if the child is new, marks it visited, calls
   `wg.Add(1)` (while still holding a positive counter), and launches `go visit`.
   The `visited` set under the mutex is what prevents re-expansion across cycles and
   diamonds — a second discovery of an already-seen node adds nothing.
4. The main goroutine calls `wg.Wait()`. It returns only after every `Add` — seed
   and mid-flight alike — has a matching `Done`. That `Wait` also establishes
   happens-before for every write to `visited`, so the final read of the set needs
   no further synchronization.

The semaphore is released with `defer func(){ <-sem }()` registered only *after* a
successful acquire, so the cancelled-context escape path — which returns without ever
acquiring — does not under-fill the channel. Because children are launched with `go`
and never waited on synchronously, a `maxConcurrency` of 1 still makes progress: the
parent releases its slot on return and the queued children run one at a time.

Create `closure.go`:

```go
package closure

import (
	"context"
	"errors"
	"fmt"
	"slices"
	"strings"
	"sync"
)

// Neighbors returns the direct dependencies of a node. It is called exactly once
// per reachable node; discovery is lazy, so a node's children are only known once
// the node is visited.
type Neighbors func(ctx context.Context, node string) ([]string, error)

// Closure returns the set of nodes reachable from roots. It fans out concurrently,
// calling wg.Add from inside running goroutines as new nodes are discovered (safe
// only while the discoverer still holds a positive counter, so Add is ordered
// before Wait can observe zero), bounds concurrency with maxConcurrency, and
// returns the sorted reachable set plus errors.Join of any neighbor failures.
func Closure(ctx context.Context, roots []string, maxConcurrency int, neighbors Neighbors) ([]string, error) {
	if maxConcurrency < 1 {
		maxConcurrency = 1
	}

	var (
		mu      sync.Mutex
		visited = make(map[string]struct{})
		errs    []error
		wg      sync.WaitGroup
		sem     = make(chan struct{}, maxConcurrency) // caps in-flight expanders
	)

	var visit func(node string)
	visit = func(node string) {
		defer wg.Done() // one Done per Add, on every exit path

		// Acquire a slot. This is the only place a launched goroutine can block,
		// and it always yields to a cancelled context, so the counter drains.
		select {
		case sem <- struct{}{}:
		case <-ctx.Done():
			mu.Lock()
			errs = append(errs, fmt.Errorf("visit %q: %w", node, context.Cause(ctx)))
			mu.Unlock()
			return
		}
		defer func() { <-sem }()

		children, err := neighbors(ctx, node)
		if err != nil {
			mu.Lock()
			errs = append(errs, fmt.Errorf("neighbors %q: %w", node, err))
			mu.Unlock()
			return
		}

		for _, c := range children {
			mu.Lock()
			if _, seen := visited[c]; seen {
				mu.Unlock()
				continue
			}
			visited[c] = struct{}{}
			// This goroutine still holds a positive counter (its own Done has not
			// run), so this Add is happens-before-ordered ahead of any Wait
			// observing zero. That is what makes Add-concurrent-with-Wait legal.
			wg.Add(1)
			mu.Unlock()
			go visit(c)
		}
	}

	// Seed with the de-duplicated roots. Add(len(seeds)) happens-before Wait, so
	// the counter reflects every seeded task before we begin joining.
	var seeds []string
	for _, r := range roots {
		if _, seen := visited[r]; seen {
			continue
		}
		visited[r] = struct{}{}
		seeds = append(seeds, r)
	}
	wg.Add(len(seeds))
	for _, r := range seeds {
		go visit(r)
	}

	wg.Wait() // returns only after every Add has a matching Done

	// Wait established happens-before for every write to visited, so reading it
	// now needs no further synchronization.
	out := make([]string, 0, len(visited))
	for n := range visited {
		out = append(out, n)
	}
	slices.Sort(out)

	return out, joinErrs(errs)
}

func joinErrs(errs []error) error {
	if len(errs) == 0 {
		return nil
	}
	// Sort by message so the joined error is deterministic regardless of the order
	// in which concurrent goroutines recorded their failures.
	slices.SortFunc(errs, func(a, b error) int {
		return strings.Compare(a.Error(), b.Error())
	})
	return errors.Join(errs...)
}
```

### The runnable demo

The demo resolves a deploy graph that contains both a diamond (`api` reaches
`proto` through two paths) and a cycle (`log` and `metrics` point at each other).
Because `Closure` returns a sorted slice, the output is deterministic regardless of
the order in which goroutines discover nodes.

Create `cmd/demo/main.go`:

```go
package main

import (
	"context"
	"fmt"
	"strings"

	"example.com/closure"
)

func main() {
	// A deploy graph with a diamond (api -> auth, cache; both -> proto) and a
	// cycle (log -> metrics -> log). Discovery is lazy: neighbors is consulted
	// only when a node is visited, and each node exactly once.
	graph := map[string][]string{
		"api":     {"auth", "cache"},
		"auth":    {"proto", "log"},
		"cache":   {"proto"},
		"proto":   {},
		"log":     {"metrics"},
		"metrics": {"log"}, // cycle back into log
	}

	neighbors := func(_ context.Context, node string) ([]string, error) {
		return graph[node], nil
	}

	reached, err := closure.Closure(context.Background(), []string{"api"}, 4, neighbors)
	fmt.Println("reachable from api:", strings.Join(reached, " "))
	fmt.Println("count:", len(reached))
	fmt.Println("err:", err)
}
```

Run it:

```bash
go run ./cmd/demo
```

Expected output:

```
reachable from api: api auth cache log metrics proto
count: 6
err: <nil>
```

### Tests

`TestClosureExpandsEachNodeExactlyOnce` drives a DAG, a cycle, and a diamond as
subtests. Its `Neighbors` wrapper records how many times each node is expanded and
adds a small randomized delay to shuffle discovery order under `-race`; each subtest
asserts the sorted reachable set and that the expansion-count map is all ones —
proof that neither the cycle nor the diamond triggers a re-expansion.
`TestClosureBoundsConcurrency` fans one root out to 200 leaves and asserts that the
peak concurrent expansion count never exceeds `maxConcurrency`, tracked with an
atomic gauge inside `Neighbors`. `TestClosureJoinsNeighborErrorAndTerminates` makes
one node fail to resolve and asserts the failure is surfaced through `errors.Join`
(via `errors.Is`), that the traversal still terminates, and that the failed node is
still in the reachable set (it was discovered before its expansion failed).
`TestClosureCancelTerminates` cancels the context up front and asserts the call
returns a `context.Canceled` error rather than hanging. `TestClosureDeduplicatesRoots`
passes a duplicated root and asserts it is expanded only once. `TestMain` wraps the
whole suite in `goleak.VerifyTestMain`, so no goroutine may outlive any test — on the
success, error, or cancel path.

Create `closure_test.go`:

```go
package closure

import (
	"context"
	"errors"
	"fmt"
	"math/rand/v2"
	"slices"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"go.uber.org/goleak"
)

func TestMain(m *testing.M) {
	// No goroutine may outlive any test: success, error, and cancel paths all
	// must drain the WaitGroup and every launched visit goroutine.
	goleak.VerifyTestMain(m)
}

// countingNeighbors wraps a static graph, records how many times each node is
// expanded, and tracks the peak number of concurrent expansions.
type countingNeighbors struct {
	graph     map[string][]string
	mu        sync.Mutex
	counts    map[string]int
	inflight  atomic.Int64
	maxseen   atomic.Int64
	perCallMs int // upper bound of a randomized delay to shuffle discovery order
}

func newCounting(graph map[string][]string, perCallMs int) *countingNeighbors {
	return &countingNeighbors{
		graph:     graph,
		counts:    make(map[string]int),
		perCallMs: perCallMs,
	}
}

func (c *countingNeighbors) fn(_ context.Context, node string) ([]string, error) {
	n := c.inflight.Add(1)
	for {
		old := c.maxseen.Load()
		if n <= old || c.maxseen.CompareAndSwap(old, n) {
			break
		}
	}
	defer c.inflight.Add(-1)

	if c.perCallMs > 0 {
		time.Sleep(time.Duration(rand.IntN(c.perCallMs)+1) * time.Millisecond)
	}

	c.mu.Lock()
	c.counts[node]++
	c.mu.Unlock()

	return c.graph[node], nil
}

func TestClosureExpandsEachNodeExactlyOnce(t *testing.T) {
	t.Parallel()

	cases := []struct {
		name  string
		graph map[string][]string
		roots []string
		want  []string
	}{
		{
			name: "dag",
			graph: map[string][]string{
				"a": {"b", "c"},
				"b": {"d"},
				"c": {"d"},
				"d": {},
			},
			roots: []string{"a"},
			want:  []string{"a", "b", "c", "d"},
		},
		{
			name: "cycle",
			graph: map[string][]string{
				"x": {"y"},
				"y": {"z"},
				"z": {"x"}, // z -> x closes the cycle
			},
			roots: []string{"x"},
			want:  []string{"x", "y", "z"},
		},
		{
			name: "diamond",
			graph: map[string][]string{
				"top":    {"left", "right"},
				"left":   {"bottom"},
				"right":  {"bottom"},
				"bottom": {},
			},
			roots: []string{"top"},
			want:  []string{"bottom", "left", "right", "top"},
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			cn := newCounting(tc.graph, 2) // shuffle discovery to exercise -race
			got, err := Closure(context.Background(), tc.roots, 3, cn.fn)
			if err != nil {
				t.Fatalf("err = %v, want nil", err)
			}
			if !slices.Equal(got, tc.want) {
				t.Fatalf("reachable = %v, want %v", got, tc.want)
			}
			// Expansion-count map must be all ones: every reachable node expanded
			// exactly once, no re-expansion across cycles or diamonds.
			for _, n := range tc.want {
				if c := cn.counts[n]; c != 1 {
					t.Fatalf("node %q expanded %d times, want exactly 1", n, c)
				}
			}
			if len(cn.counts) != len(tc.want) {
				t.Fatalf("expanded %d distinct nodes, want %d", len(cn.counts), len(tc.want))
			}
		})
	}
}

func TestClosureBoundsConcurrency(t *testing.T) {
	t.Parallel()

	// A wide graph: one root fanning out to many leaves. Without a bound the peak
	// in-flight count would approach the leaf count.
	graph := map[string][]string{"root": {}}
	for i := range 200 {
		leaf := fmt.Sprintf("leaf-%d", i)
		graph["root"] = append(graph["root"], leaf)
		graph[leaf] = []string{}
	}

	const limit = 5
	cn := newCounting(graph, 1)
	got, err := Closure(context.Background(), []string{"root"}, limit, cn.fn)
	if err != nil {
		t.Fatalf("err = %v, want nil", err)
	}
	if len(got) != 201 {
		t.Fatalf("reachable count = %d, want 201", len(got))
	}
	if peak := cn.maxseen.Load(); peak > limit {
		t.Fatalf("peak in-flight = %d, exceeds maxConcurrency = %d", peak, limit)
	}
}

var errUpstream = errors.New("upstream unavailable")

func TestClosureJoinsNeighborErrorAndTerminates(t *testing.T) {
	t.Parallel()

	// Node "b" fails to resolve its neighbors. The traversal must still terminate
	// (Wait neither hangs nor returns early) and still report every node it did
	// reach, with the failure surfaced via errors.Join.
	neighbors := func(_ context.Context, node string) ([]string, error) {
		switch node {
		case "a":
			return []string{"b", "c"}, nil
		case "b":
			return nil, errUpstream
		case "c":
			return []string{"d"}, nil
		default:
			return nil, nil
		}
	}

	got, err := Closure(context.Background(), []string{"a"}, 4, neighbors)
	if err == nil {
		t.Fatal("err = nil, want the joined upstream failure")
	}
	if !errors.Is(err, errUpstream) {
		t.Fatalf("err = %v, want errors.Is(err, errUpstream)", err)
	}
	// b was discovered (added to the set) before its expansion failed, so it is
	// still part of the reachable set; only its children are missing.
	want := []string{"a", "b", "c", "d"}
	if !slices.Equal(got, want) {
		t.Fatalf("reachable = %v, want %v", got, want)
	}
}

func TestClosureCancelTerminates(t *testing.T) {
	t.Parallel()

	graph := map[string][]string{
		"a": {"b", "c"},
		"b": {"d"},
		"c": {"d"},
		"d": {},
	}
	cn := newCounting(graph, 0)

	ctx, cancel := context.WithCancel(context.Background())
	cancel() // cancel before starting

	got, err := Closure(ctx, []string{"a"}, 2, cn.fn)
	if err == nil {
		t.Fatal("err = nil, want a context cancellation error")
	}
	if !errors.Is(err, context.Canceled) {
		t.Fatalf("err = %v, want errors.Is(err, context.Canceled)", err)
	}
	// Whatever it returns must be a subset of the graph and sorted; the only hard
	// requirement is that the call returned at all (no hang) with no leaked
	// goroutine (TestMain's goleak proves the latter).
	if !slices.IsSorted(got) {
		t.Fatalf("reachable = %v, want sorted", got)
	}
}

func TestClosureDeduplicatesRoots(t *testing.T) {
	t.Parallel()

	graph := map[string][]string{
		"a": {"b"},
		"b": {},
	}
	cn := newCounting(graph, 0)
	got, err := Closure(context.Background(), []string{"a", "a", "b"}, 2, cn.fn)
	if err != nil {
		t.Fatalf("err = %v, want nil", err)
	}
	if !slices.Equal(got, []string{"a", "b"}) {
		t.Fatalf("reachable = %v, want [a b]", got)
	}
	if cn.counts["a"] != 1 {
		t.Fatalf("root a expanded %d times, want 1 despite duplicate roots", cn.counts["a"])
	}
}
```

## Review

Correct here means three things at once: every reachable node is expanded exactly
once (the expansion-count map is all ones, even through cycles and diamonds), the
returned set is deterministically sorted, and no goroutine outlives the return on any
path. The single invariant that guarantees the first is the `visited` set under the
mutex: a node is marked before its `go visit` launches, so a second discovery adds
nothing and no `Add` is issued for it. The invariant that guarantees the join is the
happens-before ordering of the counter: the seed `Add(len(seeds))` precedes `Wait`,
and every mid-flight `wg.Add(1)` runs while its discovering goroutine still holds a
positive counter — so its `Add` is ordered ahead of that goroutine's `Done`, and
`Wait` can never observe a false zero and return early. The tests prove it by
reaching their assertions at all (a leaked counter would hang `Wait` under
`goleak`), by pinning the all-ones expansion map under a race-shuffled discovery
order, and by asserting the peak in-flight gauge stays within `maxConcurrency`. The
production bug this pattern prevents is the concurrent crawler that either
double-fetches nodes on a diamond, races its `WaitGroup` into an early return that
drops half the graph, or fans out unbounded and exhausts the process on a large
dependency tree.

## Resources

- [`sync.WaitGroup`](https://pkg.go.dev/sync#WaitGroup) -- the `Add` documentation states the exact rule that a positive-delta `Add` must happen-before a `Wait` could observe zero, which is what makes mid-flight `Add` legal here.
- [The Go Memory Model](https://go.dev/ref/mem) -- the happens-before guarantees behind `Add`/`Done`/`Wait` and why reading `visited` after `Wait` needs no extra lock.
- [`errors.Join`](https://pkg.go.dev/errors#Join) -- aggregating independent neighbor failures into one error that still answers `errors.Is`.
- [`go.uber.org/goleak`](https://pkg.go.dev/go.uber.org/goleak) -- the leak detector used in `TestMain` to prove no goroutine outlives the traversal.

---

Back to [00-concepts.md](00-concepts.md) | Prev: [12-outbox-relay-batch-dispatch-partition.md](12-outbox-relay-batch-dispatch-partition.md) | Next: [14-request-hedging-join-for-leak-freedom.md](14-request-hedging-join-for-leak-freedom.md)
