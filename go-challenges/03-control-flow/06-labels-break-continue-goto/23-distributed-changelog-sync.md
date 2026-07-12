# Exercise 23: Sync two databases via changelog, skip batches on divergence threshold

**Nivel: Avanzado** — validacion normal (tabla de casos, incluye borde o concurrencia).

A primary and a replica database are kept in sync by applying a changelog:
a stream of signed deltas describing how far the replica has drifted from
the primary for a given key range. Normally each batch nudges divergence
back toward zero, but a batch that would push divergence too far in either
direction is unsafe to apply — applying it commits the replica to a state
the operator has decided is unacceptable. This module is fully
self-contained: its own `go mod init`, all code inline, its own demo and
tests.

## What you'll build

```text
changelogsync/              independent module: example.com/changelogsync
  go.mod                     go 1.24
  changelogsync.go            Change, Batch, Sync
  cmd/
    demo/
      main.go                runnable demo: two applied batches, one rejected in between
  changelogsync_test.go       table test: no batches, all within threshold, skip preserves divergence, recovery after a skip, intermediate breach, negative-direction breach, plus a concurrent-callers race check
```

- Files: `changelogsync.go`, `cmd/demo/main.go`, `changelogsync_test.go`.
- Implement: `Sync(batches []Batch, threshold int) (applied, skipped []string, finalDivergence int)`, rejecting any batch whose running divergence would leave `[-threshold, threshold]` at any point while summing its changes.
- Test: no batches, every batch staying comfortably within threshold, a rejected batch leaving divergence untouched, a later batch recovering right after a skip, an intermediate breach inside a batch whose final sum would have been fine, and a breach in the negative direction. A concurrency test confirms `Sync` is safe to call from many goroutines against a shared, read-only batch slice.
- Verify: `go test -count=1 -race ./...`

Set up the module:

```bash
go mod edit -go=1.24
```

### Why the state has to survive the labeled continue

`Sync` carries one variable, `divergence`, across every iteration of the
outer batches loop — this is the accumulated drift the whole sync process
is tracking, and it must be exactly right after every batch, applied or
not. Deciding whether the CURRENT batch is safe means summing its changes
one at a time in a nested loop, checking the running total against the
threshold after every single change rather than only at the end: a batch
whose changes swing wildly but cancel out by the last one is still unsafe,
because a real replica applies changes in order and would sit in the
intermediate, out-of-band state for however long that batch takes to apply.
The moment any partial sum breaches the band, the whole batch must be
abandoned — a labeled `continue batches`, fired from inside the per-change
loop, discards every change already summed for this batch and moves to the
next one. Critically, `divergence` is only ever updated to `running` *after*
the per-change loop finishes without a labeled continue firing — so a
rejected batch leaves `divergence` exactly where it found it, letting a
later batch with a small or corrective delta succeed even though the one
before it failed.

Create `changelogsync.go`:

```go
package changelogsync

// Change is one row-level change with a signed magnitude: positive widens
// primary/replica divergence, negative narrows it (a corrective change).
type Change struct {
	Key   string
	Delta int
}

// Batch is one changelog batch, applied to the replica as a unit.
type Batch struct {
	ID      string
	Changes []Change
}

// Sync applies changelog batches in order, tracking a running divergence
// value between primary and replica. A batch is applied only if divergence
// never leaves the [-threshold, threshold] band at ANY point while its
// changes are summed one by one; the moment it would, the WHOLE BATCH is
// rejected — none of its changes are committed, divergence stays exactly
// where it was before the batch started, and a labeled continue on the
// BATCHES loop, fired from inside the per-change summing loop, moves
// straight to the next batch. Because a rejected batch leaves divergence
// untouched rather than making it worse, a LATER batch with a small or
// corrective delta can still succeed on its own — recovery happens in a
// later round, not by patching up the rejected one.
func Sync(batches []Batch, threshold int) (applied, skipped []string, finalDivergence int) {
	divergence := 0

batches:
	for _, b := range batches {
		running := divergence
		for _, c := range b.Changes {
			running += c.Delta
			if running > threshold || running < -threshold {
				skipped = append(skipped, b.ID)
				continue batches
			}
		}
		divergence = running
		applied = append(applied, b.ID)
	}

	return applied, skipped, divergence
}
```

### The runnable demo

Create `cmd/demo/main.go`:

```go
package main

import (
	"fmt"

	"example.com/changelogsync"
)

func main() {
	batches := []changelogsync.Batch{
		{ID: "b1", Changes: []changelogsync.Change{{Key: "k1", Delta: 2}, {Key: "k2", Delta: 1}}},
		{ID: "b2", Changes: []changelogsync.Change{{Key: "k3", Delta: 5}}}, // pushes divergence too far
		{ID: "b3", Changes: []changelogsync.Change{{Key: "k4", Delta: -1}}},
	}

	applied, skipped, divergence := changelogsync.Sync(batches, 3)
	fmt.Println("applied:", applied)
	fmt.Println("skipped:", skipped)
	fmt.Println("final divergence:", divergence)
}
```

Run it:

```bash
go run ./cmd/demo
```

Expected output:

```
applied: [b1 b3]
skipped: [b2]
final divergence: 2
```

`b1` brings divergence from 0 to 2, within the threshold of 3. `b2` would
push it to 7, well past the band, so it is rejected and divergence stays at
2. `b3`'s corrective `-1` delta brings divergence to 2 — well within the
band — succeeding even though the batch right before it failed.

### Tests

`TestSync` covers no batches at all, every batch staying comfortably inside
the threshold, a rejected batch leaving divergence exactly where it was, a
later batch recovering right after a skip, a breach in the middle of a
batch whose final sum would otherwise look fine, and a breach in the
negative direction. `TestSyncConcurrentCallers` runs `Sync` from fifty
goroutines against a shared batches slice to confirm the read-only,
locally-scoped state introduces no data race.

Create `changelogsync_test.go`:

```go
package changelogsync

import (
	"slices"
	"sync"
	"testing"
)

func TestSync(t *testing.T) {
	t.Parallel()

	tests := map[string]struct {
		batches        []Batch
		threshold      int
		wantApplied    []string
		wantSkipped    []string
		wantDivergence int
	}{
		"no batches": {
			batches:        nil,
			threshold:      5,
			wantApplied:    nil,
			wantSkipped:    nil,
			wantDivergence: 0,
		},
		"every batch stays within threshold": {
			batches: []Batch{
				{ID: "b1", Changes: []Change{{Key: "k1", Delta: 1}}},
				{ID: "b2", Changes: []Change{{Key: "k2", Delta: 1}}},
			},
			threshold:      5,
			wantApplied:    []string{"b1", "b2"},
			wantSkipped:    nil,
			wantDivergence: 2,
		},
		"a batch exceeding threshold is skipped and divergence is untouched": {
			batches: []Batch{
				{ID: "b1", Changes: []Change{{Key: "k1", Delta: 2}}},
				{ID: "b2", Changes: []Change{{Key: "k2", Delta: 10}}},
			},
			threshold:      3,
			wantApplied:    []string{"b1"},
			wantSkipped:    []string{"b2"},
			wantDivergence: 2,
		},
		"a corrective batch recovers even right after a skip": {
			batches: []Batch{
				{ID: "b1", Changes: []Change{{Key: "k1", Delta: 2}, {Key: "k2", Delta: 1}}},
				{ID: "b2", Changes: []Change{{Key: "k3", Delta: 5}}},
				{ID: "b3", Changes: []Change{{Key: "k4", Delta: -1}}},
			},
			threshold:      3,
			wantApplied:    []string{"b1", "b3"},
			wantSkipped:    []string{"b2"},
			wantDivergence: 2,
		},
		"an intermediate breach within a batch rejects it even if the final sum is fine": {
			batches: []Batch{
				{ID: "b1", Changes: []Change{{Key: "k1", Delta: 5}, {Key: "k2", Delta: -5}}},
			},
			threshold:      3,
			wantApplied:    nil,
			wantSkipped:    []string{"b1"},
			wantDivergence: 0,
		},
		"negative divergence past the threshold is also rejected": {
			batches: []Batch{
				{ID: "b1", Changes: []Change{{Key: "k1", Delta: -10}}},
			},
			threshold:      3,
			wantApplied:    nil,
			wantSkipped:    []string{"b1"},
			wantDivergence: 0,
		},
	}

	for name, tc := range tests {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			applied, skipped, divergence := Sync(tc.batches, tc.threshold)
			if !slices.Equal(applied, tc.wantApplied) {
				t.Fatalf("applied = %v, want %v", applied, tc.wantApplied)
			}
			if !slices.Equal(skipped, tc.wantSkipped) {
				t.Fatalf("skipped = %v, want %v", skipped, tc.wantSkipped)
			}
			if divergence != tc.wantDivergence {
				t.Fatalf("divergence = %d, want %d", divergence, tc.wantDivergence)
			}
		})
	}
}

// TestSyncConcurrentCallers checks that Sync, which only reads its input and
// keeps all state local, is safe to call concurrently -- run with -race to
// confirm no data race is introduced by sharing the same batches slice.
func TestSyncConcurrentCallers(t *testing.T) {
	batches := []Batch{
		{ID: "b1", Changes: []Change{{Key: "k1", Delta: 1}}},
		{ID: "b2", Changes: []Change{{Key: "k2", Delta: 1}}},
	}

	var wg sync.WaitGroup
	for range 50 {
		wg.Add(1)
		go func() {
			defer wg.Done()
			applied, skipped, divergence := Sync(batches, 5)
			if len(applied) != 2 || len(skipped) != 0 || divergence != 2 {
				t.Errorf("Sync = (%v, %v, %d), want (2 applied, 0 skipped, 2)", applied, skipped, divergence)
			}
		}()
	}
	wg.Wait()
}
```

Verify:

```bash
go test -count=1 -race ./...
```

## Review

The sync is correct when a rejected batch leaves `divergence` at exactly its
pre-batch value, never partially applied and never made worse — the
"corrective batch recovers even right after a skip" test is the one to
study, since `b3` only succeeds because `b2`'s rejection did not push
`divergence` any further from zero. The bug this exercise guards against is
updating `divergence` incrementally as each change is summed rather than
only after the whole batch is confirmed safe: that would let a rejected
batch's earlier, in-threshold changes leak into the tracked state even
though the batch as a whole was never applied. The "intermediate breach"
test pins the other half of the contract: checking only the batch's final
sum would wrongly accept a batch that spent part of its execution outside
the allowed band.

## Resources

- [Go Specification: Continue statements](https://go.dev/ref/spec#Continue_statements) — a labeled `continue` targets the named enclosing `for`.
- [Change Data Capture patterns (Debezium docs)](https://debezium.io/documentation/reference/stable/architecture.html) — the changelog-driven sync shape this exercise models.
- [The Go Memory Model](https://go.dev/ref/mem) — why a function with only local, read-only state needs no synchronization to be race-free.

---

Back to [00-concepts.md](00-concepts.md) | Prev: [22-connection-pool-lifecycle-rebalance.md](22-connection-pool-lifecycle-rebalance.md) | Next: [24-pipeline-stage-graceful-shutdown.md](24-pipeline-stage-graceful-shutdown.md)
