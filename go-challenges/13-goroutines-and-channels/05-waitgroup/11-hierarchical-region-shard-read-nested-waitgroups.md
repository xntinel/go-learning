# Exercise 11: Nested WaitGroups For A Multi-Region Sharded Read

**Level: Intermediate**

A read-path service often has to gather one key from every shard of every region and
merge the leaves into a single view before it can answer. The naive flat fan-out —
one goroutine per (region, shard) joined by a single WaitGroup — works, but it throws
away the tree structure the problem actually has, and it gives you nowhere to hang
per-region concerns (a per-region deadline, a per-region quorum, a per-region merge).
This module builds the honest shape: an outer WaitGroup fans out one goroutine per
region, and inside each region an inner WaitGroup fans out one goroutine per shard,
with the joins nested so the top-level answer is valid only once every leaf has landed.

This module is self-contained: its own module, a `shardread` package, a demo, and tests.
Nothing here imports another exercise.

## What you'll build

```text
shardread/                   independent module: example.com/shardread
  go.mod                     go 1.26
  shardread.go               ReadAll fans out over regions then shards into m[region][shard]
  cmd/demo/main.go           runnable demo: a 2x2 read with one failing shard
  shardread_test.go          exact-once fill, no writer after return, per-cell errors, topologies, Example
```

- Files: `shardread.go`, `cmd/demo/main.go`, `shardread_test.go`.
- Implement: `ReadAll(ctx context.Context, regions, shardsPerRegion int, read ShardReader) [][]ShardValue`, with `type ShardValue struct { Region, Shard int; Value string; Err error }` and `type ShardReader func(ctx context.Context, region, shard int) (string, error)`.
- Test: every cell filled exactly once in its own slot under `-race` with no lock; no goroutine still writing after `ReadAll` returns; per-cell errors captured without failing the scan; degenerate 0-region / 0-shard topologies; table-driven sizes; an `Example` pinning a 2x2 merge.
- Verify: `go test -count=1 -race ./...`

Set up the module:

```bash
go mod edit -go=1.26
```

### Nesting the joins so the outer Wait publishes every leaf

The structure is a two-level tree of joins. The outer WaitGroup counts regions; each
region goroutine owns an inner WaitGroup that counts its shards. The instructive
invariant is the ordering between the two levels:

1. The outer loop launches one goroutine per region and records it on the outer
   WaitGroup *before* the goroutine starts — the Add-before-`go` rule, which
   `outer.Go` enforces for you by placing the increment correctly.
2. Inside a region goroutine, an inner loop launches one goroutine per shard on a
   fresh inner WaitGroup. Each shard goroutine writes exactly one cell,
   `m[region][shard]`, and nothing else touches that cell.
3. The region goroutine calls `inner.Wait()` and only *then* returns. Because the
   region goroutine was launched with `outer.Go`, its outer `Done` fires when it
   returns — that is, strictly after its `inner.Wait()`.
4. The caller's `outer.Wait()` therefore happens-after every region's `inner.Wait()`,
   which happens-after every shard write. Transitively, `outer.Wait()` returning
   publishes every leaf in the matrix.

That transitive happens-before chain is the whole point. It is what lets the caller
read the entire `m[region][shard]` matrix afterward with no mutex and no channel: the
writes are disjoint (each goroutine owns one cell) and the nested joins publish them.
The failure mode this avoids is subtle. If a region goroutine returned *before* its
inner Wait — for example if you called `inner.Wait()` in a detached goroutine, or
dropped it entirely — the outer `Done` could fire while shard goroutines were still
writing. `outer.Wait()` would then return over a matrix that is still being mutated,
and the caller's read would race the lingering writers. Under `-race` that is a
reported data race; in production it is a torn read that intermittently drops a
shard's value. Keeping `inner.Wait()` on the region goroutine's own return path is
what forbids it.

Note the errors do not propagate up the tree: a shard read that fails records its
error in that cell's `ShardValue.Err` and the scan continues. This is collect-all
semantics, not fail-fast — the merged view wants every leaf's outcome, successful or
not, exactly as a cross-region quorum read reports which replicas answered.

Create `shardread.go`:

```go
package shardread

import (
	"context"
	"sync"
)

// ShardValue is one leaf of the read: the answer from a single (Region, Shard).
// Err is non-nil when that shard's read failed; a failed cell does not fail the
// rest of the scan.
type ShardValue struct {
	Region int
	Shard  int
	Value  string
	Err    error
}

// ShardReader fetches a key from one shard of one region. Implementations may
// block, delay, or fail independently per (region, shard).
type ShardReader func(ctx context.Context, region, shard int) (string, error)

// ReadAll fans out one goroutine per region (the outer WaitGroup) and, inside
// each region, one goroutine per shard (an inner WaitGroup). It returns a fully
// populated matrix indexed as m[region][shard].
//
// The load-bearing invariant is the nesting of the joins: each region goroutine
// calls its inner Wait before it returns, and because the outer join is driven by
// wg.Go (which calls Done only after the function returns), the outer Wait
// happens-after every inner Wait. That chain publishes every leaf write, so the
// caller may read the whole matrix without any additional lock: every cell is
// written by exactly one goroutine into its own m[region][shard] slot.
func ReadAll(ctx context.Context, regions, shardsPerRegion int, read ShardReader) [][]ShardValue {
	m := make([][]ShardValue, regions)
	for r := range regions {
		m[r] = make([]ShardValue, shardsPerRegion)
	}

	var outer sync.WaitGroup
	for r := range regions {
		outer.Go(func() {
			var inner sync.WaitGroup
			for s := range shardsPerRegion {
				inner.Go(func() {
					v, err := read(ctx, r, s)
					// Disjoint write: this goroutine owns cell (r, s) alone.
					m[r][s] = ShardValue{Region: r, Shard: s, Value: v, Err: err}
				})
			}
			// Inner Wait must precede the region goroutine's return, so the outer
			// Done (issued by outer.Go on return) happens-after every shard write.
			inner.Wait()
		})
	}
	outer.Wait()
	return m
}
```

### The runnable demo

The demo runs a 2x2 topology where one shard (region 1, shard 0) always fails. Print
order walks the returned slice in `[region][shard]` order, which is fixed regardless
of goroutine scheduling, so the output is deterministic.

Create `cmd/demo/main.go`:

```go
package main

import (
	"context"
	"fmt"

	"example.com/shardread"
)

func main() {
	// A reader that fails exactly one cell (region 1, shard 0) and otherwise
	// returns a stable value. Print order walks the returned slice, so it is
	// deterministic regardless of goroutine scheduling.
	read := func(ctx context.Context, region, shard int) (string, error) {
		if region == 1 && shard == 0 {
			return "", fmt.Errorf("shard offline")
		}
		return fmt.Sprintf("v-%d-%d", region, shard), nil
	}

	m := shardread.ReadAll(context.Background(), 2, 2, read)

	for r := range m {
		for s := range m[r] {
			cell := m[r][s]
			if cell.Err != nil {
				fmt.Printf("region=%d shard=%d ERR %v\n", cell.Region, cell.Shard, cell.Err)
				continue
			}
			fmt.Printf("region=%d shard=%d val=%s\n", cell.Region, cell.Shard, cell.Value)
		}
	}
}
```

Run it:

```bash
go run ./cmd/demo
```

Expected output:

```
region=0 shard=0 val=v-0-0
region=0 shard=1 val=v-0-1
region=1 shard=0 ERR shard offline
region=1 shard=1 val=v-1-1
```

### Tests

`TestReadAllFillsEveryCellExactlyOnce` runs a 6x5 topology with a jittered reader and
asserts every cell landed in its own slot (its `Region`/`Shard` match the index) with
the expected value and no error — the exact-once, disjoint-write property, proven
under `-race` with no lock. `TestReadAllNoWriterAfterReturn` re-reads the whole matrix
*after* `ReadAll` returns while the reader adds tiny randomized delays; if the outer
Wait did not happen-after every inner Wait, a leaf goroutine would still be writing and
the race detector would flag this read. `TestReadAllCapturesPerCellErrors` fails the
diagonal and asserts those cells carry the sentinel error via `errors.Is` while every
off-diagonal cell still succeeds — errors are per-cell, not fatal to the scan.
`TestReadAllTopologies` is table-driven across sizes including `0x0`, `0x4`, and `4x0`,
asserting the returned matrix is correctly shaped and degenerate empties come back
empty. `ExampleReadAll` pins the merged output for a fixed 2x2 topology.

Create `shardread_test.go`:

```go
package shardread

import (
	"context"
	"errors"
	"fmt"
	"math/rand/v2"
	"testing"
	"time"
)

func wantValue(r, s int) string { return fmt.Sprintf("v-%d-%d", r, s) }

// delayedReader jitters each read by a tiny randomized delay, so if the outer
// Wait did not happen-after every inner Wait, a leaf goroutine would still be
// writing after ReadAll returned — and the post-return re-read below would race.
func delayedReader(ctx context.Context, region, shard int) (string, error) {
	time.Sleep(time.Duration(rand.IntN(200)) * time.Microsecond)
	return wantValue(region, shard), nil
}

func TestReadAllFillsEveryCellExactlyOnce(t *testing.T) {
	t.Parallel()

	const regions, shards = 6, 5
	m := ReadAll(context.Background(), regions, shards, delayedReader)

	if len(m) != regions {
		t.Fatalf("rows = %d, want %d", len(m), regions)
	}
	for r := range m {
		if len(m[r]) != shards {
			t.Fatalf("row %d cols = %d, want %d", r, len(m[r]), shards)
		}
		for s := range m[r] {
			c := m[r][s]
			// Each cell must land in its own slot: Region/Shard identify the slot.
			if c.Region != r || c.Shard != s {
				t.Fatalf("cell[%d][%d] misfiled as (%d,%d)", r, s, c.Region, c.Shard)
			}
			if c.Err != nil {
				t.Fatalf("cell[%d][%d] Err = %v, want nil", r, s, c.Err)
			}
			if c.Value != wantValue(r, s) {
				t.Fatalf("cell[%d][%d] Value = %q, want %q", r, s, c.Value, wantValue(r, s))
			}
		}
	}
}

func TestReadAllNoWriterAfterReturn(t *testing.T) {
	t.Parallel()

	// A jittered reader plus a full re-read of the matrix after ReadAll returns.
	// Under -race, a lingering leaf writer would be flagged against this read.
	const regions, shards = 5, 4
	m := ReadAll(context.Background(), regions, shards, delayedReader)

	for r := range m {
		for s := range m[r] {
			if got := m[r][s].Value; got != wantValue(r, s) {
				t.Fatalf("re-read cell[%d][%d] = %q, want %q", r, s, got, wantValue(r, s))
			}
		}
	}
}

var errShard = errors.New("shard read failed")

func TestReadAllCapturesPerCellErrors(t *testing.T) {
	t.Parallel()

	const regions, shards = 3, 3
	// Fail the diagonal; every other cell must still succeed.
	read := func(ctx context.Context, region, shard int) (string, error) {
		if region == shard {
			return "", errShard
		}
		return wantValue(region, shard), nil
	}

	m := ReadAll(context.Background(), regions, shards, read)
	for r := range m {
		for s := range m[r] {
			c := m[r][s]
			if r == s {
				if !errors.Is(c.Err, errShard) {
					t.Fatalf("cell[%d][%d] Err = %v, want errShard", r, s, c.Err)
				}
				if c.Value != "" {
					t.Fatalf("cell[%d][%d] Value = %q, want empty on error", r, s, c.Value)
				}
				continue
			}
			if c.Err != nil {
				t.Fatalf("cell[%d][%d] Err = %v, want nil", r, s, c.Err)
			}
			if c.Value != wantValue(r, s) {
				t.Fatalf("cell[%d][%d] Value = %q, want %q", r, s, c.Value, wantValue(r, s))
			}
		}
	}
}

func TestReadAllTopologies(t *testing.T) {
	t.Parallel()

	cases := []struct {
		name           string
		regions, shard int
	}{
		{"empty", 0, 0},
		{"no-regions", 0, 4},
		{"no-shards", 4, 0},
		{"single", 1, 1},
		{"wide", 2, 8},
		{"tall", 8, 2},
		{"square", 5, 5},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			m := ReadAll(context.Background(), tc.regions, tc.shard, delayedReader)

			if len(m) != tc.regions {
				t.Fatalf("rows = %d, want %d", len(m), tc.regions)
			}
			for r := range m {
				if len(m[r]) != tc.shard {
					t.Fatalf("row %d cols = %d, want %d", r, len(m[r]), tc.shard)
				}
				for s := range m[r] {
					if m[r][s].Value != wantValue(r, s) {
						t.Fatalf("cell[%d][%d] = %q, want %q", r, s, m[r][s].Value, wantValue(r, s))
					}
				}
			}
		})
	}
}

func ExampleReadAll() {
	read := func(ctx context.Context, region, shard int) (string, error) {
		return fmt.Sprintf("v-%d-%d", region, shard), nil
	}
	m := ReadAll(context.Background(), 2, 2, read)
	for r := range m {
		for s := range m[r] {
			fmt.Printf("%d/%d=%s ", m[r][s].Region, m[r][s].Shard, m[r][s].Value)
		}
	}
	fmt.Println()
	// Output: 0/0=v-0-0 0/1=v-0-1 1/0=v-1-0 1/1=v-1-1
}
```

## Review

`ReadAll` is correct when the returned matrix has one `ShardValue` per (region, shard)
in its own slot, every leaf write is visible to the caller with no lock, per-cell
failures are captured in `Err` without aborting the scan, and degenerate topologies
come back correctly shaped. The guarantee comes from nesting the joins: each region
goroutine calls `inner.Wait()` before returning, and `outer.Go` fires that region's
outer `Done` only on return, so `outer.Wait()` happens-after every inner Wait and
transitively publishes every disjoint cell write. The jittered post-return re-read is
the proof under `-race` — if a leaf were still writing after `ReadAll` returned, the
detector would report the race against that read. The production bug this pattern
prevents is a top-level response assembled over a matrix that is still being filled:
drop or detach the inner Wait and the caller intermittently serves a view missing a
shard's value, the read-path equivalent of answering a quorum read before the quorum
has actually landed.

## Resources

- [The Go Memory Model](https://go.dev/ref/mem) -- the happens-before rules that make WaitGroup joins publish writes; the basis for the nested-join argument.
- [`sync.WaitGroup`](https://pkg.go.dev/sync#WaitGroup) -- the `Go`, `Add`, `Done`, and `Wait` methods, including the 1.25 `Go` helper used at both levels.
- [`go` statement and goroutines](https://go.dev/ref/spec#Go_statements) -- the language spec on launching concurrent execution, the leaves of this tree.

---

Back to [00-concepts.md](00-concepts.md) | Prev: [10-panic-safe-worker-wg.md](10-panic-safe-worker-wg.md) | Next: [12-outbox-relay-batch-dispatch-partition.md](12-outbox-relay-batch-dispatch-partition.md)
