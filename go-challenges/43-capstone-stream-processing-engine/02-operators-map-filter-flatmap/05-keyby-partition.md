# Exercise 5: Key Partitioning (keyBy)

To process a stream in parallel while keeping per-key ordering, you partition it: every record is routed to one of N sub-streams chosen by a hash of its key, so all records sharing a key always land in the same partition. Each partition is then an independent ordered substream a downstream worker can consume concurrently. This is Flink's `keyBy`, Kafka's partitioner, and a MapReduce shuffle — the same idea each time. This exercise builds the partitioning fan-out and confronts its central trade-off: head-of-line blocking.

This module is fully self-contained. It defines the generic `Partition` operator and the deterministic `PartitionIndex` helper, its own demo, and its own tests. Nothing here imports any other exercise.

## What you'll build

```text
partition.go           PartitionIndex (fnv32a % n), Partition (single router -> N channels)
cmd/
  demo/
    main.go            show key -> partition mapping and per-partition contents
partition_test.go      same key same partition, all elements delivered, ctx abort
```

- Files: `partition.go`, `cmd/demo/main.go`, `partition_test.go`.
- Implement: `PartitionIndex(key, n)` using a stable FNV-1a hash, and `Partition(ctx, in, n, buf, keyOf)` returning N output channels fed by a single router goroutine that closes them all when the input drains.
- Test: `partition_test.go` proves one key always lands in one partition, that every input element is delivered exactly once across all partitions, and that a cancelled context stops the router.
- Verify: `go test -race ./...`

Set up the module:

```bash
go mod edit -go=1.26
```

### Why determinism matters and what head-of-line blocking costs

Partitioning has one non-negotiable property: the same key must always map to the same partition. `PartitionIndex` hashes the key with FNV-1a and takes the result modulo N, which is a pure function of the key and N — feed it `"alice"` and it returns the same partition every run, every process, every machine. That determinism is what makes per-key ordering and keyed state work: if `"alice"` could land in partition 0 on one record and partition 2 on the next, her records would be reordered across partitions and any per-key accumulator downstream would be split in two. A non-cryptographic hash is the right tool here; the goal is uniform spreading across partitions, not collision resistance, and FNV-1a is fast and well-distributed for short string keys.

The fan-out itself is a single router goroutine that reads each record, computes its partition, and sends it to that partition's channel. Using one router rather than N is what guarantees a single, well-defined arrival order per key — the router observes the input stream in order, so records for one key are forwarded in that same order. The cost of a single router is head-of-line blocking: if one partition's downstream consumer stalls, the router blocks on the send to that partition's channel and, because it is the only router, every other partition starves even though their consumers are ready. Buffered partition channels soften this — the `buf` parameter lets a temporarily slow consumer fall behind by up to `buf` records before it stalls the router — but buffering only defers the problem; it does not remove it. The honest framing for a senior is that this design trades maximum throughput for strict per-key ordering and simplicity, and a system that needs both unbounded decoupling and ordering must add per-partition queues with their own backpressure, which is a larger machine than this operator.

Closing is the last detail. The router owns all N output channels and closes every one of them in a single `defer` when it returns — whether the input drained or the context was cancelled. Consumers ranging over a partition channel therefore always terminate. Forgetting to close even one partition would hang any worker ranging over it forever, so the `defer` closes the whole slice in a loop rather than closing channels individually at scattered return points.

Create `partition.go`:

```go
// Package stream provides a key-partitioning fan-out: records are routed to
// one of N ordered substreams by a stable hash of their key, so all records
// sharing a key land in the same partition and keep their relative order.
package stream

import (
	"context"
	"hash/fnv"
)

// PartitionIndex maps a key to a partition in [0, n) using a stable FNV-1a
// hash. It is a pure function of key and n: the same key always yields the
// same partition, which is what preserves per-key ordering and keyed state.
func PartitionIndex(key string, n int) int {
	h := fnv.New32a()
	_, _ = h.Write([]byte(key))
	return int(h.Sum32() % uint32(n))
}

// Partition fans the input stream out into n output channels, routing each
// element to partition PartitionIndex(keyOf(element), n). A single router
// goroutine reads the input in order, so records of one key are forwarded
// in arrival order. Partition channels are buffered by buf to let a slow
// consumer fall behind before it stalls the router (head-of-line blocking).
// The router closes every output channel when the input drains or ctx is
// cancelled.
func Partition[T any](ctx context.Context, in <-chan T, n int, buf int, keyOf func(T) string) []<-chan T {
	chans := make([]chan T, n)
	for i := range chans {
		chans[i] = make(chan T, buf)
	}
	go func() {
		defer func() {
			for _, c := range chans {
				close(c)
			}
		}()
		for {
			select {
			case v, ok := <-in:
				if !ok {
					return
				}
				idx := PartitionIndex(keyOf(v), n)
				select {
				case chans[idx] <- v:
				case <-ctx.Done():
					return
				}
			case <-ctx.Done():
				return
			}
		}
	}()
	outs := make([]<-chan T, n)
	for i, c := range chans {
		outs[i] = c
	}
	return outs
}
```

The function returns `[]<-chan T` (receive-only channels) even though it created `[]chan T` internally: the caller may only receive, and the router is the sole sender and closer. Handing out receive-only views is the type system enforcing the channel-ownership rule the design depends on.

### The runnable demo

The demo first prints the deterministic key-to-partition mapping for a handful of keys, then partitions a stream of user events across three partitions and drains every partition concurrently, printing each partition's contents. Because the router forwards in arrival order and `PartitionIndex` is deterministic, the per-partition contents are reproducible.

Create `cmd/demo/main.go`:

```go
// Command demo shows the deterministic key->partition mapping and the
// contents of each partition after a stream is fanned out.
package main

import (
	"context"
	"fmt"
	"sync"

	stream "example.com/keyby-partition"
)

func main() {
	const n = 3
	ctx := context.Background()

	// Deterministic mapping: the same key always maps to the same partition.
	for _, k := range []string{"alice", "dave", "eve", "grace"} {
		fmt.Printf("key %-5s -> partition %d\n", k, stream.PartitionIndex(k, n))
	}

	type event struct{ user string }
	users := []string{"alice", "eve", "dave", "alice", "grace", "eve", "alice"}
	in := make(chan event, len(users))
	for _, u := range users {
		in <- event{user: u}
	}
	close(in)

	parts := stream.Partition(ctx, in, n, 8, func(e event) string { return e.user })

	// Drain every partition concurrently into a slice, then print in order.
	collected := make([][]string, n)
	var wg sync.WaitGroup
	for i, p := range parts {
		wg.Add(1)
		go func(i int, p <-chan event) {
			defer wg.Done()
			for e := range p {
				collected[i] = append(collected[i], e.user)
			}
		}(i, p)
	}
	wg.Wait()

	for i, c := range collected {
		fmt.Printf("partition %d: %v\n", i, c)
	}
}
```

Run it:

```bash
go run ./cmd/demo
```

Expected output:

```
key alice -> partition 2
key dave  -> partition 1
key eve   -> partition 0
key grace -> partition 1
partition 0: [eve eve]
partition 1: [dave grace]
partition 2: [alice alice alice]
```

### Tests

`TestPartitionSameKeySamePartition` feeds many records of one key and asserts they all arrive in a single partition, the property the whole operator exists to guarantee. `TestPartitionDeliversEveryElement` feeds a mixed stream and asserts the total across all partitions equals the input count, so nothing is dropped or duplicated. `TestPartitionContextCancel` cancels before draining and asserts the router still closes every partition channel so no consumer hangs.

Create `partition_test.go`:

```go
package stream

import (
	"context"
	"sync"
	"testing"
	"time"
)

func drainAll[T any](parts []<-chan T) [][]T {
	collected := make([][]T, len(parts))
	var wg sync.WaitGroup
	for i, p := range parts {
		wg.Add(1)
		go func(i int, p <-chan T) {
			defer wg.Done()
			for v := range p {
				collected[i] = append(collected[i], v)
			}
		}(i, p)
	}
	wg.Wait()
	return collected
}

// TestPartitionSameKeySamePartition verifies one key lands in exactly one partition.
func TestPartitionSameKeySamePartition(t *testing.T) {
	t.Parallel()

	const n = 4
	in := make(chan string, 100)
	for i := 0; i < 100; i++ {
		in <- "same-key"
	}
	close(in)

	parts := Partition(context.Background(), in, n, 16, func(s string) string { return s })
	collected := drainAll(parts)

	nonEmpty := 0
	total := 0
	for _, c := range collected {
		if len(c) > 0 {
			nonEmpty++
		}
		total += len(c)
	}
	if nonEmpty != 1 {
		t.Errorf("one key spread across %d partitions, want 1", nonEmpty)
	}
	if total != 100 {
		t.Errorf("delivered %d records, want 100", total)
	}
	want := PartitionIndex("same-key", n)
	if len(collected[want]) != 100 {
		t.Errorf("partition %d got %d, want 100", want, len(collected[want]))
	}
}

// TestPartitionDeliversEveryElement verifies every input element is delivered exactly once.
func TestPartitionDeliversEveryElement(t *testing.T) {
	t.Parallel()

	const n = 3
	keys := []string{"a", "b", "c", "d", "e", "f", "g", "a", "b", "c"}
	in := make(chan string, len(keys))
	for _, k := range keys {
		in <- k
	}
	close(in)

	parts := Partition(context.Background(), in, n, 4, func(s string) string { return s })
	collected := drainAll(parts)

	total := 0
	for _, c := range collected {
		total += len(c)
	}
	if total != len(keys) {
		t.Errorf("delivered %d records, want %d", total, len(keys))
	}
}

// TestPartitionContextCancel verifies the router closes every partition on cancel.
func TestPartitionContextCancel(t *testing.T) {
	t.Parallel()

	ctx, cancel := context.WithCancel(context.Background())
	in := make(chan string) // never written, never closed
	parts := Partition(ctx, in, 3, 0, func(s string) string { return s })

	cancel()

	done := make(chan struct{})
	go func() {
		drainAll(parts) // returns only if every partition channel is closed
		close(done)
	}()
	select {
	case <-done:
		// all partitions closed after cancel
	case <-time.After(2 * time.Second):
		t.Fatal("router did not close partitions within 2s of cancel")
	}
}
```

## Review

The operator is correct when partitioning is deterministic and total: every key maps to one stable partition and every input is delivered exactly once. The bug that breaks per-key ordering is making the index depend on anything but the key — a counter, a timestamp, the time of day — so the test pins a single key to a single partition. The bug that hangs consumers is forgetting to close a partition channel on one of the router's exit paths, which the single `defer` over the whole slice prevents and `TestPartitionContextCancel` exercises. The trade-off to remember is head-of-line blocking: one router preserves order but lets a slow partition stall the rest, and the `buf` parameter only defers that stall, it does not remove it. Run with `-race`: the router writes the partition channels while N consumer goroutines read them.

## Resources

- [hash/fnv](https://pkg.go.dev/hash/fnv) — the FNV-1a hash used for stable, well-distributed partition assignment.
- [Apache Flink: keyBy and key groups](https://nightlies.apache.org/flink/flink-docs-stable/docs/dev/datastream/operators/overview/#keyby) — the production partitioning operator this mirrors.
- [Go blog: Pipelines and cancellation](https://go.dev/blog/pipelines) — the fan-out pattern and channel-ownership rules the router follows.

---

Prev: [04-stateful-scan.md](04-stateful-scan.md) | Next: [06-iter-seq-operators.md](06-iter-seq-operators.md)
