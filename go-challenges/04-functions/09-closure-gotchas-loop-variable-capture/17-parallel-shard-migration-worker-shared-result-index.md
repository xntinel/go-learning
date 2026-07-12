# Exercise 17: Parallel Shard Migration: Workers Capturing Loop Index for Shared Result Slice

**Nivel: Avanzado** — validacion normal (tabla de casos, incluye concurrencia).

A shard migration tool spawns one worker goroutine per shard and writes each
migration result into its own slot in a preallocated slice. The trap: if a
worker computes its slot from a single shared index variable instead of one
passed as its own parameter, every worker ends up targeting the SAME slot —
whichever shard the dispatch loop last got to — and every other shard's
migration result is silently lost.

## What you'll build

```text
shardmigrate/                independent module: example.com/shardmigrate
  go.mod                     go 1.24
  shardmigrate.go              Shard, Result, MigrateShards, MigrateShardsBuggy
  cmd/
    demo/
      main.go                runnable demo: migrate shards, print results and lost-slot count
  shardmigrate_test.go         slot-correctness, shared-index collapse, single-shard edge case
```

- Files: `shardmigrate.go`, `cmd/demo/main.go`, `shardmigrate_test.go`.
- Implement: `MigrateShards(shards, migrate)` spawning one goroutine per shard that writes into its own slot passed as a parameter; `MigrateShardsBuggy` spawning goroutines that all read a single shared `idx` variable instead.
- Test: assert every shard's result lands in its own slot for the correct version, `-race` clean over 50 shards; assert the buggy version collapses every result into the last slot and loses the rest, deterministically, using a barrier so the test never flakes.
- Verify: `go test -count=1 -race ./...`

Set up the module:

```bash
go mod edit -go=1.24
```

### Why the slot index must be a parameter, not a shared variable

Each worker goroutine must know which slot in `results` is its own. Distinct
indices are distinct memory, so disjoint-slot writes need no lock at all —
`MigrateShards` passes `i` as an argument to the goroutine closure, so
`results[i]` is always that goroutine's own slot regardless of scheduling.

`MigrateShardsBuggy` recreates the classic mistake deliberately: `idx` is
declared once, outside the loop, and every worker reads it instead of its own
index. A worker never runs before the dispatch loop finishes spawning the
rest, so this exercise makes the worst case *deterministic* instead of just
possible: every worker blocks on a `release` channel until the dispatch loop
has finished advancing `idx` to the last shard's index, then they all read
it. In real code without that barrier this is exactly as bad, just
timing-dependent instead of guaranteed — any worker that happens to run late
enough lands in the same wrong slot. The write to `results[idx]` is
mutex-protected so the concurrent writes themselves are not a data race; the
bug is entirely in *which* slot every worker targets. The result: every other
slot is left at its zero value, and the last slot holds whichever migration
happened to finish first under the lock.

Create `shardmigrate.go`:

```go
package shardmigrate

import "sync"

// Shard identifies one migration unit. IDs start at 1 so the zero value of
// Result is unambiguously "never written".
type Shard struct {
	ID int
}

// Result is what migrating one shard produces.
type Result struct {
	ShardID int
	Value   int
}

// MigrateFunc migrates one shard.
type MigrateFunc func(Shard) Result

// MigrateShards migrates every shard concurrently, one goroutine per shard,
// each writing into its OWN result slot passed as a parameter. Distinct
// indices are distinct memory, so disjoint-slot writes need no lock.
func MigrateShards(shards []Shard, migrate MigrateFunc) []Result {
	results := make([]Result, len(shards))
	var wg sync.WaitGroup
	for i, shard := range shards {
		wg.Add(1)
		go func(i int, s Shard) {
			defer wg.Done()
			results[i] = migrate(s)
		}(i, shard)
	}
	wg.Wait()
	return results
}

// MigrateShardsBuggy migrates every shard concurrently too, but every worker
// reads its result slot from a SINGLE shared `idx` variable declared outside
// the loop instead of its own index. A worker never runs before the loop
// that spawned it finishes dispatching the rest, so this exercise makes that
// worst case deterministic with a barrier: every worker blocks until the
// dispatch loop has finished advancing idx to the LAST shard's index, then
// they all read it. In real code without the barrier this is exactly as bad,
// just timing-dependent instead of guaranteed -- workers that happen to run
// late enough will land in the same wrong slot regardless. The result: every
// shard's migration overwrites the SAME final slot, and every other slot is
// silently lost.
func MigrateShardsBuggy(shards []Shard, migrate MigrateFunc) []Result {
	results := make([]Result, len(shards))
	var idx int // BUG: one shared index for every worker instead of its own
	var mu sync.Mutex
	release := make(chan struct{})
	var wg sync.WaitGroup
	for i, shard := range shards {
		idx = i
		wg.Add(1)
		go func(s Shard) {
			defer wg.Done()
			<-release // wait for the dispatch loop to finish advancing idx
			r := migrate(s)
			mu.Lock()
			results[idx] = r // BUG: idx now holds the LAST shard's index for every worker
			mu.Unlock()
		}(shard)
	}
	close(release) // only now do workers read idx; every read sees the final value
	wg.Wait()
	return results
}
```

### The runnable demo

The demo migrates four shards with both variants. Which shard's data wins the
race for the buggy version's shared slot is scheduler-dependent, so the demo
reports the deterministic part of the bug — how many slots were lost, not
whose value survived.

Create `cmd/demo/main.go`:

```go
package main

import (
	"fmt"

	"example.com/shardmigrate"
)

func migrate(s shardmigrate.Shard) shardmigrate.Result {
	return shardmigrate.Result{ShardID: s.ID, Value: s.ID * 100}
}

func shards(n int) []shardmigrate.Shard {
	out := make([]shardmigrate.Shard, n)
	for i := range out {
		out[i] = shardmigrate.Shard{ID: i + 1}
	}
	return out
}

func main() {
	correct := shardmigrate.MigrateShards(shards(4), migrate)
	fmt.Println("correct results:", correct)

	// Which shard's migration wins the race for the shared slot is
	// nondeterministic, so the demo reports the deterministic part of the
	// bug: how many slots were silently lost, not which value survived.
	buggy := shardmigrate.MigrateShardsBuggy(shards(4), migrate)
	lost := 0
	for _, r := range buggy {
		if r == (shardmigrate.Result{}) {
			lost++
		}
	}
	fmt.Printf("buggy   results: %d of %d slots lost\n", lost, len(buggy))
}
```

Run it:

```bash
go run ./cmd/demo
```

Expected output:

```
correct results: [{1 100} {2 200} {3 300} {4 400}]
buggy   results: 3 of 4 slots lost
```

### Tests

`TestMigrateShardsEachOwnSlot` migrates 50 shards and asserts `results[i]`
always derives from `shards[i]` under `-race`. `TestMigrateShardsBuggyLoses
AllButLastSlot` asserts every slot except the last stays at its zero value
and that the last slot holds *some* valid migration output — the winning
shard's identity is scheduler-dependent, so the test checks it matches one of
the inputs rather than a specific one. `TestMigrateShardsSingleShardEdge
Case` covers the boundary where the bug cannot manifest because there is no
other slot to leak into.

Create `shardmigrate_test.go`:

```go
package shardmigrate

import "testing"

func testShards(n int) []Shard {
	out := make([]Shard, n)
	for i := range out {
		out[i] = Shard{ID: i + 1}
	}
	return out
}

func testMigrate(s Shard) Result {
	return Result{ShardID: s.ID, Value: s.ID * 10}
}

func TestMigrateShardsEachOwnSlot(t *testing.T) {
	t.Parallel()

	shards := testShards(50)
	results := MigrateShards(shards, testMigrate)
	if len(results) != len(shards) {
		t.Fatalf("len(results) = %d, want %d", len(results), len(shards))
	}
	for i, s := range shards {
		want := Result{ShardID: s.ID, Value: s.ID * 10}
		if results[i] != want {
			t.Fatalf("results[%d] = %+v, want %+v (slot contamination)", i, results[i], want)
		}
	}
}

func TestMigrateShardsBuggyLosesAllButLastSlot(t *testing.T) {
	t.Parallel()

	shards := testShards(6)
	results := MigrateShardsBuggy(shards, testMigrate)
	if len(results) != len(shards) {
		t.Fatalf("len(results) = %d, want %d", len(results), len(shards))
	}

	for i := 0; i < len(results)-1; i++ {
		if results[i] != (Result{}) {
			t.Fatalf("results[%d] = %+v, want zero value (never written)", i, results[i])
		}
	}

	// Every worker reads the SAME shared idx, so they all target the last
	// slot -- but which worker's write wins that race is scheduler-dependent,
	// so only assert that some migration survived there, not whose.
	last := results[len(results)-1]
	if last == (Result{}) {
		t.Fatalf("results[%d] = zero value, want some surviving migration result", len(results)-1)
	}
	validShard := false
	for _, s := range shards {
		if last.ShardID == s.ID && last.Value == s.ID*10 {
			validShard = true
			break
		}
	}
	if !validShard {
		t.Fatalf("surviving result %+v does not match any shard's expected migration output", last)
	}
}

func TestMigrateShardsSingleShardEdgeCase(t *testing.T) {
	t.Parallel()

	shards := testShards(1)
	correct := MigrateShards(shards, testMigrate)
	buggy := MigrateShardsBuggy(shards, testMigrate)

	want := Result{ShardID: 1, Value: 10}
	if correct[0] != want {
		t.Fatalf("correct[0] = %+v, want %+v", correct[0], want)
	}
	if buggy[0] != want {
		t.Fatalf("buggy[0] = %+v, want %+v (single shard has no other slot to leak into)", buggy[0], want)
	}
}
```

## Review

The migration is correct when every shard's result lands in its own slot no
matter how many workers run concurrently — `TestMigrateShardsEachOwnSlot`
under `-race` over 50 shards is the guard: a shared index would either race
or land values in the wrong slots. The mechanism to keep straight is that a
goroutine's parameters are copied at the `go` statement, so passing `i`
freezes that goroutine's own value regardless of what the dispatch loop does
afterward; closing over a variable instead means every goroutine reads
whatever that variable holds when it happens to run. The barrier in the
buggy version does not change the bug, it only pins the outcome so the test
is deterministic instead of a flake. Run `go test -race`; the disjoint-slot
writes in the correct version and the mutex-guarded writes in the buggy
version must both be clean.

## Resources

- [Go spec: Go statements](https://go.dev/ref/spec#Go_statements) — function arguments are evaluated when the `go` statement executes, not when the goroutine runs.
- [`sync.WaitGroup`](https://pkg.go.dev/sync#WaitGroup) — coordinating completion of a fan-out of worker goroutines.
- [Go blog: Fixing for loops in Go 1.22](https://go.dev/blog/loopvar-preview) — why the slot index is safe per-iteration but still best passed explicitly.

---

Back to [00-concepts.md](00-concepts.md) | Prev: [16-webhook-batch-async-timeout-defer-accumulation.md](16-webhook-batch-async-timeout-defer-accumulation.md) | Next: [18-distributed-transaction-connection-pool-lifo-close.md](18-distributed-transaction-connection-pool-lifo-close.md)
