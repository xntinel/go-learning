# Exercise 3: Bounded Concurrent Fetch with errgroup: Result-Slot Capture

A batch fetcher runs one goroutine per key, bounded by `errgroup.SetLimit`, and
writes each result into its own preallocated slot. This is the shape of every
"fetch N records concurrently" endpoint. The loop-capture trap here is the result
slot: the index that selects `results[i]` must be a goroutine argument, never a
shared capture, or two goroutines write the same slot and the `-race` build
catches it.

## What you'll build

```text
bfetch/                      independent module: example.com/bfetch
  go.mod                     go 1.26; requires golang.org/x/sync
  fetch.go                   Fetcher; FetchAll with errgroup, SetLimit, per-slot writes
  cmd/
    demo/
      main.go                runnable demo: fetch keys, print ordered results
  fetch_test.go              slot-correctness, limit-ceiling gauge, first-error cancel
```

- Files: `fetch.go`, `cmd/demo/main.go`, `fetch_test.go`.
- Implement: `FetchAll(ctx, keys, fetch)` that spawns one `errgroup` goroutine per key with a concurrency limit, passing the index by argument so each result lands in its own slot; the first non-nil error cancels the derived context and is returned by `Wait`.
- Test: with a fake fetcher, assert every key maps to its correct slot; an atomic gauge proves concurrency never exceeds the ceiling; an injected error cancels the context and is returned; `-race` clean over 100 keys.
- Verify: `go test -count=1 -race ./...`

Set up the module (it needs the `golang.org/x/sync` dependency):

```bash
go mod edit -go=1.26
go get golang.org/x/sync/errgroup@latest
```

### Why the slot index must be an argument

`errgroup.WithContext(ctx)` returns a `*errgroup.Group` and a context that is
cancelled the moment the first goroutine returns a non-nil error (or when `Wait`
returns). `Group.SetLimit(n)` caps concurrency: `Group.Go` blocks until fewer
than `n` goroutines are running, so at most `n` fetches are in flight at once —
this is how you bound fan-out against a slice whose size you do not control.
`Group.Wait` returns the first non-nil error.

Each goroutine must write to `results[i]` where `i` selects that goroutine's own
slot. Distinct indices are distinct memory, so disjoint-slot writes need no
mutex. The bug this exercise guards against is passing the index by capture in a
way that lets two goroutines share it. On a `go 1.26` module the per-iteration
`range` variable already gives each iteration its own `i`, but the robust,
version-independent form — and the one that survives a refactor into a plain
`for i := 0; i < n; i++` counter loop — is to pass `i` as a parameter to the
closure. That is what `FetchAll` does. The fake fetcher in the test returns a
value derived from the key, so any cross-slot contamination shows up as a value
in the wrong index.

The concurrency ceiling is checked with an instrumented fetcher: each call bumps
an atomic gauge on entry and drops it on exit, recording the maximum. After the
batch, the test asserts the recorded maximum never exceeded the limit.

Create `fetch.go`:

```go
package bfetch

import (
	"context"

	"golang.org/x/sync/errgroup"
)

// FetchFunc loads the value for one key. It must respect ctx cancellation.
type FetchFunc[K any, V any] func(ctx context.Context, key K) (V, error)

// FetchAll fetches every key concurrently, bounded to limit in-flight requests,
// writing each result into its own slot. It returns the results in key order and
// the first error encountered; the first error cancels the shared context so the
// remaining fetches can abort early.
func FetchAll[K any, V any](ctx context.Context, keys []K, limit int, fetch FetchFunc[K, V]) ([]V, error) {
	results := make([]V, len(keys))
	g, ctx := errgroup.WithContext(ctx)
	if limit > 0 {
		g.SetLimit(limit)
	}
	for i, key := range keys {
		g.Go(func() error {
			v, err := fetch(ctx, key)
			if err != nil {
				return err
			}
			results[i] = v // i is this goroutine's own per-iteration slot
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

The demo fetches three keys with a limit of 2 and prints the results in order.
The fetch function just upper-cases the key so the output is deterministic.

Create `cmd/demo/main.go`:

```go
package main

import (
	"context"
	"fmt"
	"strings"

	"example.com/bfetch"
)

func main() {
	ctx := context.Background()
	keys := []string{"alpha", "beta", "gamma"}

	fetch := func(_ context.Context, key string) (string, error) {
		return strings.ToUpper(key), nil
	}

	results, err := bfetch.FetchAll(ctx, keys, 2, fetch)
	if err != nil {
		fmt.Println("error:", err)
		return
	}
	for i, key := range keys {
		fmt.Printf("%s -> %s\n", key, results[i])
	}
}
```

Run it:

```bash
go run ./cmd/demo
```

Expected output:

```
alpha -> ALPHA
beta -> BETA
gamma -> GAMMA
```

### Tests

`TestEachKeyLandsInItsSlot` fetches 100 keys and asserts `results[i]` derives
from `keys[i]` — cross-slot contamination fails it. `TestRespectsLimit` uses an
atomic gauge to assert in-flight fetches never exceed the ceiling.
`TestFirstErrorCancels` injects an error on one key and asserts `FetchAll`
returns it and that the shared context was cancelled (a later-starting fetch sees
`ctx.Err()`).

Create `fetch_test.go`:

```go
package bfetch

import (
	"context"
	"errors"
	"fmt"
	"strconv"
	"sync/atomic"
	"testing"
)

func TestEachKeyLandsInItsSlot(t *testing.T) {
	t.Parallel()

	const n = 100
	keys := make([]int, n)
	for i := range keys {
		keys[i] = i
	}
	fetch := func(_ context.Context, key int) (string, error) {
		return "v" + strconv.Itoa(key), nil
	}

	results, err := FetchAll(context.Background(), keys, 8, fetch)
	if err != nil {
		t.Fatalf("FetchAll: %v", err)
	}
	for i := range keys {
		want := "v" + strconv.Itoa(keys[i])
		if results[i] != want {
			t.Fatalf("results[%d] = %q, want %q (slot contamination)", i, results[i], want)
		}
	}
}

func TestRespectsLimit(t *testing.T) {
	t.Parallel()

	const limit = 4
	var inFlight atomic.Int32
	var peak atomic.Int32

	fetch := func(_ context.Context, key int) (int, error) {
		cur := inFlight.Add(1)
		for {
			p := peak.Load()
			if cur <= p || peak.CompareAndSwap(p, cur) {
				break
			}
		}
		// Busy a moment so several goroutines overlap.
		sum := 0
		for j := range 1000 {
			sum += j
		}
		inFlight.Add(-1)
		return sum + key, nil
	}

	keys := make([]int, 200)
	for i := range keys {
		keys[i] = i
	}
	if _, err := FetchAll(context.Background(), keys, limit, fetch); err != nil {
		t.Fatalf("FetchAll: %v", err)
	}
	if got := peak.Load(); got > limit {
		t.Fatalf("peak in-flight = %d, want <= %d", got, limit)
	}
}

func TestFirstErrorCancels(t *testing.T) {
	t.Parallel()

	errBoom := errors.New("boom")
	var sawCancel atomic.Bool

	fetch := func(ctx context.Context, key int) (int, error) {
		if key == 0 {
			return 0, errBoom
		}
		// Later keys observe the cancellation triggered by key 0's failure.
		<-ctx.Done()
		sawCancel.Store(true)
		return 0, ctx.Err()
	}

	keys := []int{0, 1, 2, 3}
	_, err := FetchAll(context.Background(), keys, 2, fetch)
	if !errors.Is(err, errBoom) {
		t.Fatalf("FetchAll error = %v, want %v", err, errBoom)
	}
	if !sawCancel.Load() {
		t.Fatal("context was not cancelled on first error")
	}
}

func ExampleFetchAll() {
	fetch := func(_ context.Context, key int) (int, error) {
		return key * key, nil
	}
	results, _ := FetchAll(context.Background(), []int{2, 3, 4}, 2, fetch)
	fmt.Println(results)
	// Output: [4 9 16]
}
```

## Review

The fetcher is correct when `results[i]` always derives from `keys[i]`,
concurrency never exceeds `SetLimit`, and the first error both returns from
`Wait` and cancels the shared context. `TestEachKeyLandsInItsSlot` over 100 keys
under `-race` is the slot-capture guard: a shared index would either race or land
values in wrong slots. Keep two facts straight — the derived context from
`WithContext` is what lets peer goroutines abort on the first failure, and
`SetLimit` blocks `Go` rather than dropping work, so every key still runs unless
the context is cancelled first. Run `go test -race`; the disjoint-slot writes and
the atomic gauge must be clean.

## Resources

- [`golang.org/x/sync/errgroup`](https://pkg.go.dev/golang.org/x/sync/errgroup) — `WithContext`, `Group.Go`, `Group.SetLimit`, `Group.Wait`.
- [`context`](https://pkg.go.dev/context) — cancellation propagation via the derived context.
- [Go blog: Fixing for loops in Go 1.22](https://go.dev/blog/loopvar-preview) — why the slot index is safe per-iteration but still best passed explicitly.

---

Back to [00-concepts.md](00-concepts.md) | Next: [04-http-route-table-handlers.md](04-http-route-table-handlers.md)
