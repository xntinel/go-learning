# Exercise 7: Collect Results From N Parallel Calls a Goroutine Cannot Return

A goroutine cannot return a value, so the moment you fan out N parallel calls you
need a way to collect their results. The canonical answer is a buffered results
channel sized to the number of senders: each goroutine deposits its result without
ever blocking, and the caller drains after launching. This exercise builds a
scatter-gather across N read replicas or shards, aggregating the responses.

This module is fully self-contained: its own `go mod init`, its own demo, its own
tests. Nothing here imports another exercise.

## What you'll build

```text
scatter/                     independent module: example.com/scatter
  go.mod
  scatter.go                 Result; QueryReplicas([]int, call func(int) Result) []Result
  cmd/
    demo/
      main.go                query 4 replicas, aggregate the responses
  scatter_test.go            len matches, set matches regardless of order, under -race
```

Files: `scatter.go`, `cmd/demo/main.go`, `scatter_test.go`.
Implement: `QueryReplicas(ids []int, call func(int) Result) []Result` that launches one goroutine per id, sends each `Result` on a buffered channel of capacity `len(ids)`, then drains and returns all results.
Test: assert `len(results) == len(ids)` and the set of returned values matches expected regardless of completion order; run with `-count=10` for order independence.
Verify: `go test -race -count=10 ./...`

### Why the buffer must be sized to the senders

Each goroutine produces one `Result` and must get it out somehow. A channel is the
tool, but the buffer size is not a detail — it is a correctness property. Consider
the shape: you launch N goroutines that each `ch <- call(id)`, and *then* the
caller drains with `for range N { results = append(results, <-ch) }`.

If `ch` is unbuffered, a sender blocks until a receiver is ready. But the caller
does not start receiving until after the launch loop finishes — and with an
unbuffered channel, the launch loop itself would block on the first send that has
no waiting receiver, so the code stalls. Even a buffer smaller than N only
postpones the problem: once the buffer fills, the next sender blocks, and if the
caller is not yet draining, you deadlock. Sizing the buffer to `len(ids)` — one
slot per sender — guarantees every goroutine can deposit its result and exit
immediately, with no receiver required at send time. The caller then drains all N
slots at its leisure.

This decoupling is what makes the pattern robust: senders never block, so a slow
or absent collector cannot wedge a worker, and every goroutine has a guaranteed
exit. The results come back in completion order, not input order, so the collector
must not assume ordering; the tests assert on the *set* of results. (If you need
results in input order, write into `out[i]` by index instead, as in the batch
enrichment exercise — that is the other half of this toolkit.)

Create `scatter.go`:

```go
package scatter

// Result is one replica's response.
type Result struct {
	ReplicaID int
	Value     int
	Err       error
}

// QueryReplicas calls call(id) in parallel for every id, one goroutine each, and
// returns all results. Each goroutine deposits its Result on a channel buffered
// to len(ids), so no sender ever blocks and every goroutine is guaranteed to
// exit. Results are returned in completion order, not input order.
func QueryReplicas(ids []int, call func(int) Result) []Result {
	ch := make(chan Result, len(ids))
	for _, id := range ids {
		go func() {
			ch <- call(id)
		}()
	}

	results := make([]Result, 0, len(ids))
	for range ids {
		results = append(results, <-ch)
	}
	return results
}
```

### The runnable demo

The demo queries four replicas, each returning its id doubled, then sums the
values. Because the sum is order-independent, the output is deterministic even
though the results arrive in scheduler order.

Create `cmd/demo/main.go`:

```go
package main

import (
	"fmt"

	"example.com/scatter"
)

func main() {
	ids := []int{1, 2, 3, 4}

	results := scatter.QueryReplicas(ids, func(id int) scatter.Result {
		return scatter.Result{ReplicaID: id, Value: id * 2}
	})

	total := 0
	for _, r := range results {
		total += r.Value
	}
	fmt.Printf("collected %d results, sum=%d\n", len(results), total)
}
```

Run it:

```bash
go run ./cmd/demo
```

Expected output:

```
collected 4 results, sum=20
```

### Tests

`TestQueryReplicasCollectsAll` asserts the count equals `len(ids)` and the set of
returned `(ReplicaID, Value)` pairs matches the expected set — built as a map so
order is irrelevant. `TestQueryReplicasEmpty` pins the empty case (no goroutines,
no results). `TestQueryReplicasReturnsPromptly` is the buffer-sizing proof: with
the correctly buffered channel, `QueryReplicas` returns without any concurrent
receiver being needed during the launch, so the call simply completes; the test
runs it inside a bounded expectation and asserts it finished. Running `-count=10`
exercises many completion orders.

Create `scatter_test.go`:

```go
package scatter

import (
	"testing"
)

func TestQueryReplicasCollectsAll(t *testing.T) {
	t.Parallel()

	ids := []int{10, 20, 30, 40, 50}
	got := QueryReplicas(ids, func(id int) Result {
		return Result{ReplicaID: id, Value: id + 1}
	})

	if len(got) != len(ids) {
		t.Fatalf("len(results) = %d, want %d", len(got), len(ids))
	}

	want := map[int]int{} // replicaID -> value
	for _, id := range ids {
		want[id] = id + 1
	}
	seen := map[int]bool{}
	for _, r := range got {
		if r.ReplicaID == 0 {
			t.Fatalf("result has zero ReplicaID: %+v", r)
		}
		if seen[r.ReplicaID] {
			t.Fatalf("replica %d appeared twice", r.ReplicaID)
		}
		seen[r.ReplicaID] = true
		if want[r.ReplicaID] != r.Value {
			t.Fatalf("replica %d value = %d, want %d", r.ReplicaID, r.Value, want[r.ReplicaID])
		}
	}
	if len(seen) != len(ids) {
		t.Fatalf("distinct replicas = %d, want %d", len(seen), len(ids))
	}
}

func TestQueryReplicasEmpty(t *testing.T) {
	t.Parallel()

	got := QueryReplicas(nil, func(id int) Result { return Result{} })
	if len(got) != 0 {
		t.Fatalf("len(results) = %d, want 0", len(got))
	}
}

func TestQueryReplicasReturnsPromptly(t *testing.T) {
	t.Parallel()

	// A buffered results channel means the launch loop never blocks and the
	// call returns after draining. If the buffer were too small this would
	// deadlock and the test would time out.
	ids := []int{1, 2, 3, 4, 5, 6, 7, 8}
	got := QueryReplicas(ids, func(id int) Result {
		return Result{ReplicaID: id, Value: id}
	})
	if len(got) != len(ids) {
		t.Fatalf("len(results) = %d, want %d", len(got), len(ids))
	}
}
```

## Review

The scatter-gather is correct when it returns exactly one result per id and the
set of results matches the expected set, in any order — the completion order is
the scheduler's choice, so the tests build a map and never assert a sequence. The
load-bearing decision is the buffer size: `make(chan Result, len(ids))` gives every
sender a slot so none blocks and each goroutine has a guaranteed exit. An
unbuffered channel or an undersized buffer with the drain-after-launch structure
deadlocks. Run `-race -count=10` to exercise many interleavings; the channel
itself synchronizes the handoff, so there is no additional lock to get wrong.

## Resources

- [Effective Go: Channels](https://go.dev/doc/effective_go#channels)
- [Go Language Specification: Channel types](https://go.dev/ref/spec#Channel_types)
- [Go Language Specification: Send statements](https://go.dev/ref/spec#Send_statements)

---

Back to [00-concepts.md](00-concepts.md) | Prev: [06-safego-panic-recovery-supervisor.md](06-safego-panic-recovery-supervisor.md) | Next: [08-bounded-fanout-errgroup.md](08-bounded-fanout-errgroup.md)
