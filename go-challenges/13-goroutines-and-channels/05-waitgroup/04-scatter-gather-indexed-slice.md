# Exercise 4: Order-Preserving Scatter-Gather Into a Pre-Sized Slice

A batch enrichment client takes a slice of IDs, fetches details for each from an
upstream service in parallel, and must return the results in the *same order* as the
input — result `i` corresponds to `ids[i]`. This module builds that client and shows
why writing each goroutine's result into its own slice index is data-race-free and
order-preserving, whereas appending to a shared slice is neither.

This module is fully self-contained: its own module, demo, and tests.

## What you'll build

```text
enrich/                    independent module: example.com/enrich
  go.mod                   go 1.25
  enrich.go                Fetcher; FetchAll returns results in input order
  cmd/
    demo/
      main.go              runnable demo: enrich 4 IDs, print in order
  enrich_test.go           order-preserving under shuffled input; empty/single
```

- Files: `enrich.go`, `cmd/demo/main.go`, `enrich_test.go`.
- Implement: `FetchAll(ctx, ids []int, fetch Fetcher) ([]Detail, error)` — one goroutine per ID writing `results[i]`, join with WaitGroup, no lock.
- Test: assert `results[i]` corresponds to `ids[i]` for a shuffled input under `-race`; cover empty and single-element inputs.
- Verify: `go test -count=1 -race ./...`

Set up the module:

```bash
mkdir -p go-solutions/13-goroutines-and-channels/05-waitgroup/04-scatter-gather-indexed-slice/cmd/demo
cd go-solutions/13-goroutines-and-channels/05-waitgroup/04-scatter-gather-indexed-slice
go mod edit -go=1.25
```

### Indexed writes preserve order and avoid the lock

The core insight: goroutine `i` writes only `results[i]`. Because every goroutine
touches a distinct element of the backing array — distinct memory addresses — there
is no data race, even with no mutex. The `wg.Wait()` establishes happens-before, so
after it returns the whole slice is safely readable. And because slot `i` always
holds the result for input `i`, the output order matches the input order for free.

Contrast this with `append`. Appending to a shared slice from many goroutines is a
data race (it mutates the shared length and backing pointer) *and* produces results
in completion order, not input order. To get both safety and order from append you
would need a mutex plus a post-hoc sort — strictly more work than pre-sizing the
slice and writing indices. When you know the output length up front and want input
order, indexed writes are the right tool.

The function threads a context so a slow upstream can be cancelled, and it captures
the first fetch error it sees. Because each goroutine writes its own error slot too,
we scan for the first non-nil error after the join and return it. (This is
collect-all with a single representative error; for true first-error *cancellation*,
Exercise 5's errgroup is the tool.)

Create `enrich.go`:

```go
package enrich

import (
	"context"
	"sync"
)

// Detail is the enriched record fetched for one ID.
type Detail struct {
	ID   int
	Name string
}

// Fetcher fetches the detail for a single ID.
type Fetcher func(ctx context.Context, id int) (Detail, error)

// FetchAll fetches every ID concurrently and returns results in the same order
// as ids. If any fetch fails, FetchAll returns the first error encountered (by
// index) along with the partial results.
func FetchAll(ctx context.Context, ids []int, fetch Fetcher) ([]Detail, error) {
	results := make([]Detail, len(ids))
	errs := make([]error, len(ids))

	var wg sync.WaitGroup
	for i, id := range ids {
		wg.Add(1)
		go func() {
			defer wg.Done()
			d, err := fetch(ctx, id)
			results[i] = d // disjoint index: no lock needed
			errs[i] = err
		}()
	}
	wg.Wait()

	for _, err := range errs {
		if err != nil {
			return results, err
		}
	}
	return results, nil
}
```

### The runnable demo

Create `cmd/demo/main.go`:

```go
package main

import (
	"context"
	"fmt"

	"example.com/enrich"
)

func main() {
	names := map[int]string{10: "alice", 20: "bob", 30: "carol", 40: "dave"}
	fetch := func(ctx context.Context, id int) (enrich.Detail, error) {
		return enrich.Detail{ID: id, Name: names[id]}, nil
	}

	ids := []int{10, 20, 30, 40}
	details, err := enrich.FetchAll(context.Background(), ids, fetch)
	if err != nil {
		fmt.Println("error:", err)
		return
	}
	for _, d := range details {
		fmt.Printf("%d=%s\n", d.ID, d.Name)
	}
}
```

Run it:

```bash
go run ./cmd/demo
```

Expected output:

```
10=alice
20=bob
30=carol
40=dave
```

### Tests

`TestFetchAllPreservesOrder` shuffles the input IDs, gives each fetch a randomized
tiny delay so completion order differs from input order, and asserts `details[i].ID
== ids[i]` for every position — the order guarantee, proved under `-race`.
`TestFetchAllError` injects a failing fetch and asserts the error propagates.
`TestFetchAllEmpty` and `TestFetchAllSingle` cover the boundaries. A documented
subtest explains (without running) why a shared append would break both order and
race-freedom.

Create `enrich_test.go`:

```go
package enrich

import (
	"context"
	"errors"
	"fmt"
	"math/rand"
	"testing"
	"time"
)

var errUpstream = errors.New("upstream unavailable")

func TestFetchAllPreservesOrder(t *testing.T) {
	t.Parallel()

	ids := []int{5, 1, 9, 3, 7, 2, 8, 4, 6, 0}
	rand.Shuffle(len(ids), func(i, j int) { ids[i], ids[j] = ids[j], ids[i] })

	fetch := func(ctx context.Context, id int) (Detail, error) {
		// Randomized delay so completion order != input order.
		time.Sleep(time.Duration(rand.Intn(3)) * time.Millisecond)
		return Detail{ID: id, Name: fmt.Sprintf("name-%d", id)}, nil
	}

	details, err := FetchAll(context.Background(), ids, fetch)
	if err != nil {
		t.Fatalf("FetchAll err = %v, want nil", err)
	}
	if len(details) != len(ids) {
		t.Fatalf("len = %d, want %d", len(details), len(ids))
	}
	for i := range ids {
		if details[i].ID != ids[i] {
			t.Fatalf("details[%d].ID = %d, want %d (order not preserved)", i, details[i].ID, ids[i])
		}
		if details[i].Name != fmt.Sprintf("name-%d", ids[i]) {
			t.Fatalf("details[%d].Name = %q, want name-%d", i, details[i].Name, ids[i])
		}
	}
}

func TestFetchAllError(t *testing.T) {
	t.Parallel()

	fetch := func(ctx context.Context, id int) (Detail, error) {
		if id == 2 {
			return Detail{}, errUpstream
		}
		return Detail{ID: id}, nil
	}
	_, err := FetchAll(context.Background(), []int{1, 2, 3}, fetch)
	if !errors.Is(err, errUpstream) {
		t.Fatalf("err = %v, want errUpstream", err)
	}
}

func TestFetchAllEmpty(t *testing.T) {
	t.Parallel()

	fetch := func(ctx context.Context, id int) (Detail, error) { return Detail{ID: id}, nil }
	details, err := FetchAll(context.Background(), nil, fetch)
	if err != nil {
		t.Fatalf("err = %v, want nil", err)
	}
	if len(details) != 0 {
		t.Fatalf("len = %d, want 0", len(details))
	}
}

func TestFetchAllSingle(t *testing.T) {
	t.Parallel()

	fetch := func(ctx context.Context, id int) (Detail, error) {
		return Detail{ID: id, Name: "solo"}, nil
	}
	details, err := FetchAll(context.Background(), []int{42}, fetch)
	if err != nil {
		t.Fatalf("err = %v, want nil", err)
	}
	if len(details) != 1 || details[0].ID != 42 || details[0].Name != "solo" {
		t.Fatalf("details = %+v, want [{42 solo}]", details)
	}
}

// Documented contrast (NOT run): a shared `append` from every goroutine would be
// a data race on the slice header AND would place results in completion order, so
// details[i] would no longer correspond to ids[i]. The indexed write above avoids
// both problems.

func ExampleFetchAll() {
	fetch := func(ctx context.Context, id int) (Detail, error) {
		return Detail{ID: id, Name: "x"}, nil
	}
	details, _ := FetchAll(context.Background(), []int{7}, fetch)
	fmt.Println(details[0].ID, details[0].Name)
	// Output: 7 x
}
```

## Review

The client is correct when `details[i]` always corresponds to `ids[i]` regardless of
which fetch finished first, the whole thing is race-clean under `-race` with no lock,
and an upstream failure surfaces via `errors.Is`. The order test earns its keep by
shuffling inputs and randomizing delays, so completion order genuinely differs from
input order — if the code accidentally used append, this test would fail on ordering
and `-race` would fail on the append.

The lesson is the disjoint-index idiom: pre-size the slice to the known output length
and let goroutine `i` own slot `i`. Reach for it whenever you fan out over a slice and
need results back in input order. When you instead need first-error cancellation, the
next module's errgroup is the right primitive.

## Resources

- [`sync.WaitGroup`](https://pkg.go.dev/sync#WaitGroup) — the join that publishes the indexed writes.
- [Go memory model](https://go.dev/ref/mem) — why disjoint-index writes plus `Wait` are race-free.
- [Data Race Detector](https://go.dev/doc/articles/race_detector) — what `-race` catches on a shared append.

---

Back to [00-concepts.md](00-concepts.md) | Previous: [03-healthcheck-aggregator-wg-go.md](03-healthcheck-aggregator-wg-go.md) | Next: [05-errgroup-first-error-cancel.md](05-errgroup-first-error-cancel.md)
