# Exercise 3: Keyed-State Operator

Stateless operators parallelize trivially, but a keyed stateful operator — one that keeps a running aggregate per key — is only correct if every record for a key reaches the same instance and is processed in order. This module builds a parallel keyed counter that routes by key, gives each worker its own per-key map, and preserves each key's order through the merge, with a test that asserts the per-key counts arrive as a strict 1, 2, 3, ... sequence.

This module is fully self-contained. It begins with its own `go mod init`, defines every type it needs, and ships its own demo and tests. Nothing here imports any other exercise.

## What you'll build

```text
keyed.go               Record, KeyedCounter, NewKeyedCounter, Parallelism,
                       partition (crc32 % N), Run (fan-out / per-key workers /
                       fan-in)
cmd/
  demo/
    main.go            three interleaved keys, per-key counts in arrival order
keyed_test.go          per-key order + exact count, key->worker affinity,
                       distribution across workers, single hot key exactness
```

- Files: `keyed.go`, `cmd/demo/main.go`, `keyed_test.go`.
- Implement: `KeyedCounter` with `Run`, routing records by key to one of N workers, each worker owning an independent `map[string]int64` and emitting the running count per record.
- Test: `keyed_test.go` asserts that for every key the emitted `Seq` and `Count` arrive in strict input order (per-key order preserved) and that the final count equals the number of records sent, even when keys are interleaved.
- Verify: `go test -race ./...`

Set up the module:

```bash
mkdir -p keyed/cmd/demo && cd keyed
go mod init example.com/keyed
go mod edit -go=1.26
```

### Why per-key order survives the parallel merge

A keyed aggregate is correct only if the operator sees a key's records in order and never splits a key across instances. Routing by key with `crc32(key) % N` guarantees the second condition: a key's entire history lands on exactly one worker. The first condition — order — follows from a chain of three single-writer guarantees. The fan-out goroutine sends a key's records to its worker in input order. The worker reads its input channel sequentially in a single goroutine, increments the key's count, and emits in that same order. One fan-in goroutine forwards that worker's output in order onto the merged channel. So although the merged stream interleaves *different* keys arbitrarily — that ordering is scheduler-dependent and not deterministic — the subsequence belonging to any one key is still exactly its input order. The test exploits this precisely: it groups the merged output by key and checks each group, never relying on cross-key order.

Because each worker owns its count map and no other goroutine touches it, there is no lock and no shared state — the map is reachable from one goroutine only. That is the cleanest way to make keyed state safe under `-race`: not by guarding shared state with a mutex, but by partitioning the state so it is never shared. The structure is the same fan-out / per-partition-worker / fan-in shape as Exercise 1, specialised so the per-partition instance carries state.

Create `keyed.go`:

```go
// Package keyed implements a data-parallel keyed operator that preserves the
// per-key order of its input. Records are partitioned by key, so every record
// for a given key reaches the same worker, and each worker processes its input
// channel sequentially. The merged output interleaves keys arbitrarily, but
// the subsequence of any single key stays in input order.
package keyed

import (
	"context"
	"hash/crc32"
	"sync"
)

// Record carries a key, a per-key sequence number assigned by the producer,
// and a Count filled in by the operator (the running per-key occurrence count).
type Record struct {
	Key   string
	Seq   int
	Count int64
}

// KeyedCounter is a parallel operator: it runs Parallelism worker goroutines,
// each owning an independent per-key count map. Because routing is by key, a
// key's entire history lands on one worker, so its running count is exact and
// its order is preserved.
type KeyedCounter struct {
	parallelism int
}

// NewKeyedCounter returns a KeyedCounter with the given parallelism (clamped to
// at least 1).
func NewKeyedCounter(parallelism int) *KeyedCounter {
	if parallelism < 1 {
		parallelism = 1
	}
	return &KeyedCounter{parallelism: parallelism}
}

// Parallelism reports the number of worker goroutines.
func (k *KeyedCounter) Parallelism() int { return k.parallelism }

// partition routes a key with crc32(key) % n. Empty keys go to partition 0.
func partition(key string, n int) int {
	if n <= 0 || key == "" {
		return 0
	}
	return int(crc32.ChecksumIEEE([]byte(key)) % uint32(n))
}

// Run starts the operator and returns its merged output channel. The output
// channel closes after in is drained (or ctx is cancelled) and every worker
// has finished. The caller must drain the output to avoid leaking goroutines.
func (k *KeyedCounter) Run(ctx context.Context, in <-chan Record) <-chan Record {
	n := k.parallelism

	partIn := make([]chan Record, n)
	partOut := make([]chan Record, n)
	for i := range partIn {
		partIn[i] = make(chan Record, 16)
		partOut[i] = make(chan Record, 16)
	}

	// One worker per partition. Each worker owns its count map exclusively, so
	// no lock is needed: the map is reachable from a single goroutine only.
	var workers sync.WaitGroup
	for i := 0; i < n; i++ {
		workers.Add(1)
		src := partIn[i]
		dst := partOut[i]
		go func() {
			defer workers.Done()
			defer close(dst)
			counts := make(map[string]int64)
			for {
				select {
				case r, ok := <-src:
					if !ok {
						return
					}
					counts[r.Key]++
					r.Count = counts[r.Key]
					select {
					case dst <- r:
					case <-ctx.Done():
						return
					}
				case <-ctx.Done():
					return
				}
			}
		}()
	}

	// Fan-out: route each record to its key's worker. The sole writer of every
	// partIn channel, it closes them all on exit so workers see EOF.
	go func() {
		defer func() {
			for _, ch := range partIn {
				close(ch)
			}
		}()
		for {
			select {
			case r, ok := <-in:
				if !ok {
					return
				}
				t := partition(r.Key, n)
				select {
				case partIn[t] <- r:
				case <-ctx.Done():
					return
				}
			case <-ctx.Done():
				return
			}
		}
	}()

	// Fan-in: merge worker outputs. Per-worker order is preserved by each
	// forwarder, and since a key lives on exactly one worker, that key's order
	// is preserved in the merged stream.
	merged := make(chan Record, 16*n)
	var fanin sync.WaitGroup
	for i := 0; i < n; i++ {
		fanin.Add(1)
		src := partOut[i]
		go func() {
			defer fanin.Done()
			for r := range src {
				select {
				case merged <- r:
				case <-ctx.Done():
					return
				}
			}
		}()
	}
	go func() {
		fanin.Wait()
		close(merged)
	}()

	return merged
}
```

The output `Record` reuses the input's `Key` and `Seq` and fills in `Count`, so a test can verify two things at once from a single record: that `Seq` arrives in input order (order preserved) and that `Count` is the running occurrence number (state correct). The fan-out goroutine is again the sole writer of every worker's input channel and closes them all on exit; each worker closes its own output channel; the closer goroutine closes the merged channel after all workers finish.

### The runnable demo

The demo feeds three interleaved keys, four records each, through a four-worker counter and prints each key's counts in the order they arrived on the merged channel. Grouping by key and sorting the key names makes the output deterministic even though the cross-key interleaving is not.

Create `cmd/demo/main.go`:

```go
package main

import (
	"context"
	"fmt"
	"sort"

	"example.com/keyed"
)

func main() {
	kc := keyed.NewKeyedCounter(4)
	ctx := context.Background()

	in := make(chan keyed.Record, 64)
	out := kc.Run(ctx, in)

	// Three keys, interleaved, with increasing per-key sequence numbers.
	keys := []string{"alpha", "beta", "gamma"}
	const perKey = 4
	go func() {
		for i := 0; i < perKey; i++ {
			for _, k := range keys {
				in <- keyed.Record{Key: k, Seq: i}
			}
		}
		close(in)
	}()

	// Collect by key. The merged stream interleaves keys, but each key's
	// counts arrive strictly in order.
	got := make(map[string][]int64)
	for r := range out {
		got[r.Key] = append(got[r.Key], r.Count)
	}

	names := make([]string, 0, len(got))
	for k := range got {
		names = append(names, k)
	}
	sort.Strings(names)

	fmt.Printf("processed %d keys with parallelism %d\n", len(got), kc.Parallelism())
	for _, k := range names {
		fmt.Printf("  %-5s counts in arrival order: %v\n", k, got[k])
	}
}
```

Run it:

```bash
go run ./cmd/demo
```

Expected output:

```
processed 3 keys with parallelism 4
  alpha counts in arrival order: [1 2 3 4]
  beta  counts in arrival order: [1 2 3 4]
  gamma counts in arrival order: [1 2 3 4]
```

Each key's counts arrive as `1 2 3 4` in order — the per-key ordering guarantee, visible directly.

### Tests

`TestPerKeyOrderAndCount` is the core test: it interleaves twelve keys, fifty records each, then groups the merged output by key and asserts every group's `Seq` is `0..49` in order and its `Count` is `1..50` in order. `TestSameKeyAlwaysSameWorker` pins the routing affinity, `TestKeysAreDistributedAcrossWorkers` proves the load spreads, and `TestSingleKeyCountIsExact` drives a thousand records of one hot key and checks the final count is exact. All are deterministic.

Create `keyed_test.go`:

```go
package keyed

import (
	"context"
	"fmt"
	"sort"
	"testing"
	"time"
)

// drain collects every record from out, grouped by key in arrival order.
func drain(ctx context.Context, out <-chan Record) (map[string][]Record, error) {
	got := make(map[string][]Record)
	for {
		select {
		case r, ok := <-out:
			if !ok {
				return got, nil
			}
			got[r.Key] = append(got[r.Key], r)
		case <-ctx.Done():
			return got, ctx.Err()
		}
	}
}

func TestPerKeyOrderAndCount(t *testing.T) {
	t.Parallel()
	const numKeys = 12
	const perKey = 50

	kc := NewKeyedCounter(4)
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	in := make(chan Record, 64)
	out := kc.Run(ctx, in)

	go func() {
		// Interleave keys so records of one key are spread through the stream.
		for i := 0; i < perKey; i++ {
			for k := 0; k < numKeys; k++ {
				in <- Record{Key: fmt.Sprintf("key-%d", k), Seq: i}
			}
		}
		close(in)
	}()

	got, err := drain(ctx, out)
	if err != nil {
		t.Fatal(err)
	}

	if len(got) != numKeys {
		t.Fatalf("got %d distinct keys, want %d", len(got), numKeys)
	}
	for key, recs := range got {
		if len(recs) != perKey {
			t.Fatalf("key %s: %d records, want %d", key, len(recs), perKey)
		}
		// Per-key order preserved: Seq strictly increases 0..perKey-1 and the
		// running Count is exactly 1..perKey in the same order.
		for i, r := range recs {
			if r.Seq != i {
				t.Fatalf("key %s record %d: Seq = %d, want %d (order not preserved)", key, i, r.Seq, i)
			}
			if r.Count != int64(i+1) {
				t.Fatalf("key %s record %d: Count = %d, want %d", key, i, r.Count, i+1)
			}
		}
	}
}

func TestKeysAreDistributedAcrossWorkers(t *testing.T) {
	t.Parallel()
	// With many keys and crc32 routing, every worker should receive at least
	// one key. This is deterministic for a fixed key set and partition count.
	const n = 4
	used := make([]bool, n)
	for k := 0; k < 200; k++ {
		used[partition(fmt.Sprintf("key-%d", k), n)] = true
	}
	for i, u := range used {
		if !u {
			t.Fatalf("worker %d received no keys", i)
		}
	}
}

func TestSameKeyAlwaysSameWorker(t *testing.T) {
	t.Parallel()
	for k := 0; k < 100; k++ {
		key := fmt.Sprintf("key-%d", k)
		first := partition(key, 7)
		for i := 0; i < 50; i++ {
			if got := partition(key, 7); got != first {
				t.Fatalf("key %s routed to %d, want %d", key, got, first)
			}
		}
	}
}

func TestSingleKeyCountIsExact(t *testing.T) {
	t.Parallel()
	kc := NewKeyedCounter(4)
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	in := make(chan Record, 16)
	out := kc.Run(ctx, in)
	const total = 1000
	go func() {
		for i := 0; i < total; i++ {
			in <- Record{Key: "hot", Seq: i}
		}
		close(in)
	}()

	got, err := drain(ctx, out)
	if err != nil {
		t.Fatal(err)
	}
	recs := got["hot"]
	if len(recs) != total {
		t.Fatalf("got %d records, want %d", len(recs), total)
	}
	if last := recs[len(recs)-1].Count; last != total {
		t.Fatalf("final count = %d, want %d", last, total)
	}
}

func ExampleKeyedCounter_Run() {
	kc := NewKeyedCounter(2)
	ctx := context.Background()
	in := make(chan Record, 8)
	out := kc.Run(ctx, in)

	for i := 0; i < 3; i++ {
		in <- Record{Key: "a", Seq: i}
	}
	in <- Record{Key: "b", Seq: 0}
	close(in)

	got := make(map[string][]int64)
	for r := range out {
		got[r.Key] = append(got[r.Key], r.Count)
	}
	keys := make([]string, 0, len(got))
	for k := range got {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	for _, k := range keys {
		fmt.Printf("%s: %v\n", k, got[k])
	}
	// Output:
	// a: [1 2 3]
	// b: [1]
}
```

## Review

The operator is correct when each key's merged subsequence is in input order and its final count is exact, regardless of how keys interleave. The most common bug is sharing one count map across workers behind a mutex: it compiles and passes a non-race build but serialises every worker and, worse, invites a missed lock that `-race` then flags. Partitioning the state so each worker owns its map removes the lock entirely. The second bug is routing by anything other than the key — a round-robin partitioner here would scatter a key across workers and every count would be partial. The third is assuming the *merged* order is the input order: it is not, and the test is written to group by key precisely because cross-key order is non-deterministic. Run `go test -race` to confirm no worker's map is touched by another goroutine.

## Resources

- [Apache Flink: Working with State (keyed state)](https://nightlies.apache.org/flink/flink-docs-stable/docs/dev/datastream/fault-tolerance/state/) — the production model for state scoped to a key, which this exercise implements in miniature.
- [Go Blog: Pipelines and cancellation](https://go.dev/blog/pipelines) — the fan-out / fan-in pattern the operator is built on.
- [hash/crc32](https://pkg.go.dev/hash/crc32) — the key-to-worker routing hash.
- [The Go Memory Model](https://go.dev/ref/mem) — why state owned by a single goroutine needs no synchronisation.

---

Back to [00-concepts.md](00-concepts.md) | Previous: [02-affinity-partitioner.md](02-affinity-partitioner.md) | Next: [04-worker-pool-backpressure.md](04-worker-pool-backpressure.md)
