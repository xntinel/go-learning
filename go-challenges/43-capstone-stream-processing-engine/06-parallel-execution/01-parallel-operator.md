# Exercise 1: Parallel Operator

This is the core of the chapter: a harness that wraps any `Operator` and runs N concurrent copies of it, routing incoming records with a pluggable `Partitioner` and merging every copy's output back onto one channel. It ships three partitioners (key, round-robin, broadcast), per-partition metrics, and the full fan-out/fan-in machinery.

This module is fully self-contained. It begins with its own `go mod init`, defines every type it needs, and ships its own demo and tests. Nothing here imports any other exercise.

## What you'll build

```text
parallel.go            Record, Operator, OperatorFactory, Partitioner,
                       KeyPartitioner, RoundRobinPartitioner,
                       BroadcastPartitioner (BroadcastAll), PartitionMetrics,
                       ParallelOperator, NewParallelOperator, Process
cmd/
  demo/
    main.go            200 records, 10 keys, 4 instances, per-partition metrics
parallel_test.go       partitioner stability/distribution, validation, end-to-end
                       routing, broadcast fan-out, cancellation drain
```

- Files: `parallel.go`, `cmd/demo/main.go`, `parallel_test.go`.
- Implement: the three `Partitioner` implementations, and `ParallelOperator` with `Process`, `Parallelism`, and `PartitionMetrics`, built by `NewParallelOperator`.
- Test: `parallel_test.go` pins hash stability and distribution, constructor validation, key routing, broadcast delivery to all partitions, round-robin ignoring keys, and a clean close on context cancellation.
- Verify: `go test -race ./...`

Set up the module:

```bash
mkdir -p parallel/cmd/demo && cd parallel
go mod init example.com/parallel
go mod edit -go=1.26
```

### The Operator contract and the partitioner interface

Everything rests on one interface. An `Operator` takes an input channel and returns an output channel and an error channel; `Process` spawns a goroutine, owns the two returned channels, and closes both when the input is exhausted or the context is cancelled. The parallel layer is itself an `Operator`, so a fanned-out operator composes into a pipeline exactly like a single one. An `OperatorFactory` builds a fresh instance per partition, which is what keeps each instance's state independent — sharing one operator across partitions would share its state and defeat the point.

The `Partitioner` maps a record to a partition index in `[0, N)`. `KeyPartitioner` uses CRC-32/IEEE and routes an empty key to partition 0; the modulo is taken on the `uint32` hash *before* the cast to `int`, which is the one detail that keeps the index non-negative. `RoundRobinPartitioner` uses an `atomic.Uint64` counter so concurrent fan-out callers need no mutex. `BroadcastPartitioner` returns the sentinel `BroadcastAll` (-1) rather than a real index; the fan-out loop checks for that sentinel before doing any modulo arithmetic.

Create `parallel.go`:

```go
// Package parallel provides a data-parallel execution layer for stream
// processing operators. It wraps any Operator with a harness that runs N
// concurrent instances and routes incoming records via a configurable
// partitioning strategy.
package parallel

import (
	"context"
	"errors"
	"fmt"
	"hash/crc32"
	"sync"
	"sync/atomic"
	"time"
)

// Record is the unit of data flowing through the pipeline.
// Key identifies the record for partitioning; Value carries the payload.
type Record struct {
	Key       []byte
	Value     []byte
	Timestamp time.Time
	Metadata  map[string]string
}

// Operator processes a stream of records. Process spawns a goroutine, owns
// its returned channels, and closes both when the input is exhausted or the
// context is cancelled. Consumers must drain both channels to avoid leaks.
type Operator interface {
	Process(ctx context.Context, in <-chan Record) (<-chan Record, <-chan error)
}

// OperatorFactory creates a fresh Operator instance. The parallel layer
// calls it once per partition to ensure each instance has independent state.
type OperatorFactory func() Operator

// BroadcastAll is returned by BroadcastPartitioner.Partition to signal that
// the record must be delivered to every partition. Fan-out code must check
// for this sentinel before applying modulo arithmetic.
const BroadcastAll = -1

// Partitioner assigns an incoming Record to a partition index in
// [0, numPartitions). BroadcastPartitioner returns BroadcastAll instead.
// Implementations must be safe for concurrent use from multiple goroutines.
type Partitioner interface {
	Partition(r Record, numPartitions int) int
}

// KeyPartitioner routes all records that share the same key to the same
// partition using CRC-32/IEEE. For a fixed numPartitions the mapping is
// stable: the same key always yields the same partition index. An empty key
// is always routed to partition 0.
type KeyPartitioner struct{}

// Partition implements Partitioner.
func (KeyPartitioner) Partition(r Record, numPartitions int) int {
	if numPartitions <= 0 || len(r.Key) == 0 {
		return 0
	}
	h := crc32.ChecksumIEEE(r.Key)
	return int(h % uint32(numPartitions))
}

// RoundRobinPartitioner distributes records evenly across partitions
// independent of record content. An atomic counter provides lock-free
// sequencing so concurrent fan-out goroutines do not need a mutex.
type RoundRobinPartitioner struct {
	counter atomic.Uint64
}

// Partition implements Partitioner.
func (rr *RoundRobinPartitioner) Partition(_ Record, numPartitions int) int {
	if numPartitions <= 0 {
		return 0
	}
	n := rr.counter.Add(1) - 1
	return int(n % uint64(numPartitions))
}

// BroadcastPartitioner signals that every record should be delivered to
// every partition. The fan-out loop sends one copy per partition channel.
type BroadcastPartitioner struct{}

// Partition implements Partitioner. It always returns BroadcastAll.
func (BroadcastPartitioner) Partition(_ Record, _ int) int {
	return BroadcastAll
}

// PartitionMetrics tracks per-partition activity counters.
// All fields are safe for concurrent reads and writes.
type PartitionMetrics struct {
	Processed atomic.Int64
	Errors    atomic.Int64
}

// Sentinel errors returned by NewParallelOperator.
var (
	ErrInvalidParallelism = errors.New("parallelism must be >= 1")
	ErrNilFactory         = errors.New("operator factory must not be nil")
	ErrNilPartitioner     = errors.New("partitioner must not be nil")
)

// ParallelOperator fans a single logical operator out to Parallelism()
// concurrent instances. Incoming records are routed by the Partitioner;
// all partition outputs are merged into one downstream channel.
type ParallelOperator struct {
	factory     OperatorFactory
	parallelism int
	partitioner Partitioner
	metrics     []*PartitionMetrics
}

// NewParallelOperator constructs a ParallelOperator. It returns an error if
// parallelism < 1, factory is nil, or partitioner is nil.
func NewParallelOperator(factory OperatorFactory, parallelism int, partitioner Partitioner) (*ParallelOperator, error) {
	if parallelism < 1 {
		return nil, fmt.Errorf("parallel: %w: got %d", ErrInvalidParallelism, parallelism)
	}
	if factory == nil {
		return nil, fmt.Errorf("parallel: %w", ErrNilFactory)
	}
	if partitioner == nil {
		return nil, fmt.Errorf("parallel: %w", ErrNilPartitioner)
	}
	m := make([]*PartitionMetrics, parallelism)
	for i := range m {
		m[i] = &PartitionMetrics{}
	}
	return &ParallelOperator{
		factory:     factory,
		parallelism: parallelism,
		partitioner: partitioner,
		metrics:     m,
	}, nil
}

// Parallelism returns the number of concurrent operator instances.
func (p *ParallelOperator) Parallelism() int { return p.parallelism }

// PartitionMetrics returns the per-partition counters slice. Its length
// equals Parallelism(). Counters may be read at any time.
func (p *ParallelOperator) PartitionMetrics() []*PartitionMetrics { return p.metrics }

// Process implements Operator. It creates p.parallelism operator instances,
// fans incoming records out to per-partition input channels according to the
// Partitioner, and merges all partition outputs into the returned channel.
// Both returned channels are closed when all partition goroutines finish.
func (p *ParallelOperator) Process(ctx context.Context, in <-chan Record) (<-chan Record, <-chan error) {
	const partBuf = 16

	// Per-partition input channels: the fan-out goroutine writes here.
	partIn := make([]chan Record, p.parallelism)
	for i := range partIn {
		partIn[i] = make(chan Record, partBuf)
	}

	// Start one operator instance per partition.
	partOut := make([]<-chan Record, p.parallelism)
	partErrc := make([]<-chan error, p.parallelism)
	for i := 0; i < p.parallelism; i++ {
		out, errc := p.factory().Process(ctx, partIn[i])
		partOut[i] = out
		partErrc[i] = errc
	}

	// Fan-out: route records from in to the appropriate partition channel.
	// Closes all partIn channels when in is drained or ctx is cancelled.
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
				target := p.partitioner.Partition(r, p.parallelism)
				if target == BroadcastAll {
					for i, ch := range partIn {
						select {
						case ch <- r:
							p.metrics[i].Processed.Add(1)
						case <-ctx.Done():
							return
						}
					}
				} else {
					select {
					case partIn[target] <- r:
						p.metrics[target].Processed.Add(1)
					case <-ctx.Done():
						return
					}
				}
			case <-ctx.Done():
				return
			}
		}
	}()

	// Fan-in: merge all partition outputs into one channel.
	merged := make(chan Record, partBuf*p.parallelism)
	mergedErr := make(chan error, p.parallelism)

	var wg sync.WaitGroup
	for i := 0; i < p.parallelism; i++ {
		wg.Add(1)
		out := partOut[i]
		errc := partErrc[i]
		go func() {
			defer wg.Done()
			for r := range out {
				select {
				case merged <- r:
				case <-ctx.Done():
					return
				}
			}
			for err := range errc {
				select {
				case mergedErr <- err:
				case <-ctx.Done():
					return
				}
			}
		}()
	}

	go func() {
		wg.Wait()
		close(merged)
		close(mergedErr)
	}()

	return merged, mergedErr
}
```

`atomic.Uint64` and `atomic.Int64` are the struct types (added in Go 1.19) with `Add`, `Load`, `Store`, and `CompareAndSwap` methods; they replace the older package-level `sync/atomic` functions and remove the manual alignment tricks the pointer-based API required.

`Process` is the heart of the harness. It allocates one bounded input channel per partition and starts one operator instance on each. The fan-out goroutine is the *sole writer* of every `partIn` channel and closes all of them in a `defer`, so each partition receives EOF regardless of how the goroutine exits — a clean input drain or a context cancellation. For a non-broadcast record it blocks only on the target partition (propagating backpressure to just that partition); for a broadcast record it sends one copy to each partition in turn. Fan-in runs one goroutine per partition that drains the partition's output channel and then its error channel — both, in that order, so a late error can never wedge an operator goroutine. A `WaitGroup` plus a closer goroutine close the merged channels exactly once, after the last partition finishes.

### The runnable demo

The demo builds a key-partitioned operator with four instances, pushes 200 records across ten keys through it, drains the merged output, and prints the per-partition metrics. Because the ten keys hash deterministically, the per-partition counts are the same on every run.

Create `cmd/demo/main.go`:

```go
package main

import (
	"context"
	"fmt"
	"log"
	"time"

	"example.com/parallel"
)

// noopOperator passes every record through unchanged.
type noopOperator struct{}

func (noopOperator) Process(ctx context.Context, in <-chan parallel.Record) (<-chan parallel.Record, <-chan error) {
	out := make(chan parallel.Record, 16)
	errc := make(chan error, 1)
	go func() {
		defer close(out)
		defer close(errc)
		for {
			select {
			case r, ok := <-in:
				if !ok {
					return
				}
				select {
				case out <- r:
				case <-ctx.Done():
					return
				}
			case <-ctx.Done():
				return
			}
		}
	}()
	return out, errc
}

func main() {
	// Build a key-partitioned parallel operator with 4 instances.
	op, err := parallel.NewParallelOperator(
		func() parallel.Operator { return noopOperator{} },
		4,
		parallel.KeyPartitioner{},
	)
	if err != nil {
		log.Fatalf("NewParallelOperator: %v", err)
	}

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	// Send 200 records with 10 distinct keys.
	in := make(chan parallel.Record, 200)
	for i := 0; i < 200; i++ {
		in <- parallel.Record{
			Key:       []byte(fmt.Sprintf("user-%d", i%10)),
			Value:     []byte(fmt.Sprintf("event-%d", i)),
			Timestamp: time.Now(),
		}
	}
	close(in)

	out, errc := op.Process(ctx, in)

	var total int
	for range out {
		total++
	}
	for err := range errc {
		log.Printf("pipeline error: %v", err)
	}

	fmt.Printf("sent 200 records through 4 parallel instances\n")
	fmt.Printf("received %d records from merged output\n", total)
	fmt.Println("per-partition metrics:")
	for i, m := range op.PartitionMetrics() {
		fmt.Printf("  partition %d: processed=%d\n", i, m.Processed.Load())
	}
}
```

Run it:

```bash
go run ./cmd/demo
```

Expected output:

```
sent 200 records through 4 parallel instances
received 200 records from merged output
per-partition metrics:
  partition 0: processed=60
  partition 1: processed=40
  partition 2: processed=60
  partition 3: processed=40
```

### Tests

The suite pins each invariant. `TestKeyPartitionerStability` proves a key maps to the same partition on a thousand repeats; `TestKeyPartitionerDistribution` checks the spread is within a wide band. `TestRoundRobinDistribution` proves the exactly-even split, and `TestRoundRobinDoesNotRouteByKey` proves round-robin ignores key content by sending 100 identical-key records and asserting at least two partitions were used. `TestKeyPartitioningRoutesConsistently` and `TestBroadcastDeliversToAllPartitions` drive the full harness end to end, and `TestContextCancellationDrains` proves both channels close promptly after a cancel.

Create `parallel_test.go`:

```go
package parallel

import (
	"context"
	"errors"
	"fmt"
	"testing"
	"time"
)

// echoOperator echoes every received record to its output unchanged.
// It is used as a fixture in tests that need a real Operator.
type echoOperator struct{}

func (echoOperator) Process(ctx context.Context, in <-chan Record) (<-chan Record, <-chan error) {
	out := make(chan Record, 16)
	errc := make(chan error, 1)
	go func() {
		defer close(out)
		defer close(errc)
		for {
			select {
			case r, ok := <-in:
				if !ok {
					return
				}
				select {
				case out <- r:
				case <-ctx.Done():
					return
				}
			case <-ctx.Done():
				return
			}
		}
	}()
	return out, errc
}

func echoFactory() Operator { return echoOperator{} }

// collectAll drains out and errc concurrently and returns the collected
// records and the first error seen. It returns early with ctx.Err() if the
// context expires before both channels close.
func collectAll(ctx context.Context, out <-chan Record, errc <-chan error) ([]Record, error) {
	type result struct {
		records []Record
		err     error
	}
	ch := make(chan result, 1)
	go func() {
		var r result
		for out != nil || errc != nil {
			select {
			case rec, ok := <-out:
				if !ok {
					out = nil
				} else {
					r.records = append(r.records, rec)
				}
			case err, ok := <-errc:
				if !ok {
					errc = nil
				} else if r.err == nil {
					r.err = err
				}
			case <-ctx.Done():
				r.err = ctx.Err()
				ch <- r
				return
			}
		}
		ch <- r
	}()
	res := <-ch
	return res.records, res.err
}

// --- KeyPartitioner tests ---

func TestKeyPartitionerStability(t *testing.T) {
	t.Parallel()
	p := KeyPartitioner{}
	keys := [][]byte{
		[]byte("user-1"), []byte("order-99"), []byte("session-abc"),
	}
	for _, k := range keys {
		r := Record{Key: k}
		first := p.Partition(r, 8)
		for i := 0; i < 1000; i++ {
			if got := p.Partition(r, 8); got != first {
				t.Fatalf("KeyPartitioner: key %q: got %d, want %d on repeat %d",
					k, got, first, i)
			}
		}
	}
}

func TestKeyPartitionerEmptyKeyRoutesToZero(t *testing.T) {
	t.Parallel()
	p := KeyPartitioner{}
	if got := p.Partition(Record{}, 4); got != 0 {
		t.Fatalf("empty key: partition = %d, want 0", got)
	}
}

func TestKeyPartitionerDistribution(t *testing.T) {
	t.Parallel()
	const n = 100_000
	const partitions = 8
	p := KeyPartitioner{}
	counts := make([]int, partitions)
	for i := 0; i < n; i++ {
		key := []byte(fmt.Sprintf("key-%d", i))
		counts[p.Partition(Record{Key: key}, partitions)]++
	}
	// Each partition should receive between 8% and 18% (expected ~12.5%).
	for i, c := range counts {
		ratio := float64(c) / n
		if ratio < 0.08 || ratio > 0.18 {
			t.Errorf("partition %d: ratio %.3f outside [0.08, 0.18]", i, ratio)
		}
	}
}

// --- RoundRobinPartitioner tests ---

func TestRoundRobinDistribution(t *testing.T) {
	t.Parallel()
	const n = 10_000
	const partitions = 4
	p := &RoundRobinPartitioner{}
	counts := make([]int, partitions)
	for i := 0; i < n; i++ {
		counts[p.Partition(Record{}, partitions)]++
	}
	// Round-robin distributes exactly n/partitions records to each partition
	// when n is a multiple of partitions.
	expected := n / partitions
	for i, c := range counts {
		if c != expected {
			t.Errorf("partition %d: got %d records, want %d", i, c, expected)
		}
	}
}

func TestRoundRobinWrap(t *testing.T) {
	t.Parallel()
	p := &RoundRobinPartitioner{}
	for i := 0; i < 8; i++ {
		got := p.Partition(Record{}, 4)
		want := i % 4
		if got != want {
			t.Fatalf("call %d: partition = %d, want %d", i, got, want)
		}
	}
}

// --- BroadcastPartitioner tests ---

func TestBroadcastPartitionerReturnsSentinel(t *testing.T) {
	t.Parallel()
	p := BroadcastPartitioner{}
	for _, n := range []int{1, 2, 4, 100} {
		if got := p.Partition(Record{}, n); got != BroadcastAll {
			t.Fatalf("BroadcastPartitioner with %d partitions: got %d, want BroadcastAll (%d)",
				n, got, BroadcastAll)
		}
	}
}

// --- NewParallelOperator validation tests ---

func TestNewParallelOperatorValidation(t *testing.T) {
	t.Parallel()
	cases := []struct {
		name        string
		factory     OperatorFactory
		parallelism int
		partitioner Partitioner
		wantErr     error
	}{
		{"zero parallelism", echoFactory, 0, KeyPartitioner{}, ErrInvalidParallelism},
		{"negative parallelism", echoFactory, -1, KeyPartitioner{}, ErrInvalidParallelism},
		{"nil factory", nil, 4, KeyPartitioner{}, ErrNilFactory},
		{"nil partitioner", echoFactory, 4, nil, ErrNilPartitioner},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			_, err := NewParallelOperator(tc.factory, tc.parallelism, tc.partitioner)
			if !errors.Is(err, tc.wantErr) {
				t.Fatalf("err = %v, want %v", err, tc.wantErr)
			}
		})
	}
}

func TestNewParallelOperatorSucceeds(t *testing.T) {
	t.Parallel()
	op, err := NewParallelOperator(echoFactory, 4, KeyPartitioner{})
	if err != nil {
		t.Fatal(err)
	}
	if op.Parallelism() != 4 {
		t.Fatalf("Parallelism = %d, want 4", op.Parallelism())
	}
	if got := len(op.PartitionMetrics()); got != 4 {
		t.Fatalf("len(PartitionMetrics) = %d, want 4", got)
	}
}

// --- End-to-end routing tests ---

func TestKeyPartitioningRoutesConsistently(t *testing.T) {
	t.Parallel()
	op, err := NewParallelOperator(echoFactory, 4, KeyPartitioner{})
	if err != nil {
		t.Fatal(err)
	}

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	in := make(chan Record, 100)
	out, errc := op.Process(ctx, in)

	keys := [][]byte{[]byte("a"), []byte("b"), []byte("c"), []byte("d")}
	const perKey = 10
	for i := 0; i < perKey; i++ {
		for _, k := range keys {
			in <- Record{Key: k, Value: []byte(fmt.Sprintf("%d", i))}
		}
	}
	close(in)

	records, err := collectAll(ctx, out, errc)
	if err != nil {
		t.Fatal(err)
	}
	if got := len(records); got != len(keys)*perKey {
		t.Fatalf("got %d records, want %d", got, len(keys)*perKey)
	}

	// The sum of all partition metrics must equal total records sent
	// (each record is sent to exactly one partition with key partitioning).
	var total int64
	for _, m := range op.PartitionMetrics() {
		total += m.Processed.Load()
	}
	want := int64(len(keys) * perKey)
	if total != want {
		t.Fatalf("metrics total = %d, want %d", total, want)
	}
}

func TestBroadcastDeliversToAllPartitions(t *testing.T) {
	t.Parallel()
	const parallelism = 3
	op, err := NewParallelOperator(echoFactory, parallelism, BroadcastPartitioner{})
	if err != nil {
		t.Fatal(err)
	}

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	in := make(chan Record, 10)
	out, errc := op.Process(ctx, in)

	const n = 5
	for i := 0; i < n; i++ {
		in <- Record{Key: []byte(fmt.Sprintf("k%d", i))}
	}
	close(in)

	records, err := collectAll(ctx, out, errc)
	if err != nil {
		t.Fatal(err)
	}
	// Broadcast: each record goes to all partitions, so total output = n * parallelism.
	wantRecords := n * parallelism
	if got := len(records); got != wantRecords {
		t.Fatalf("broadcast: got %d records, want %d (%d records x %d partitions)",
			got, wantRecords, n, parallelism)
	}
	// Each partition metric must show exactly n records processed.
	for i, m := range op.PartitionMetrics() {
		if got := m.Processed.Load(); got != n {
			t.Errorf("partition %d: Processed = %d, want %d", i, got, n)
		}
	}
}

func TestContextCancellationDrains(t *testing.T) {
	t.Parallel()
	op, err := NewParallelOperator(echoFactory, 2, &RoundRobinPartitioner{})
	if err != nil {
		t.Fatal(err)
	}

	ctx, cancel := context.WithCancel(context.Background())
	in := make(chan Record) // unbuffered: fan-out goroutine will block immediately
	out, errc := op.Process(ctx, in)
	cancel()

	// After cancellation both output channels must close within a short timeout.
	timer := time.NewTimer(2 * time.Second)
	defer timer.Stop()
	outDone, errcDone := false, false
	for !outDone || !errcDone {
		select {
		case _, ok := <-out:
			if !ok {
				outDone = true
			}
		case _, ok := <-errc:
			if !ok {
				errcDone = true
			}
		case <-timer.C:
			t.Fatal("timed out: channels did not close after context cancellation")
		}
	}
}

// TestRoundRobinDoesNotRouteByKey sends 100 records that all carry the same
// key through a round-robin ParallelOperator with parallelism 4 and asserts
// that at least two partitions received records, proving round-robin ignores
// key content (unlike key partitioning, which would send all 100 to one).
func TestRoundRobinDoesNotRouteByKey(t *testing.T) {
	t.Parallel()
	const parallelism = 4
	op, _ := NewParallelOperator(echoFactory, parallelism, &RoundRobinPartitioner{})
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	in := make(chan Record, 100)
	out, errc := op.Process(ctx, in)
	for i := 0; i < 100; i++ {
		in <- Record{Key: []byte("same-key")}
	}
	close(in)
	if _, err := collectAll(ctx, out, errc); err != nil {
		t.Fatal(err)
	}
	var partitionsUsed int
	for _, m := range op.PartitionMetrics() {
		if m.Processed.Load() > 0 {
			partitionsUsed++
		}
	}
	if partitionsUsed < 2 {
		t.Fatalf("round-robin: only %d partitions used, want >= 2", partitionsUsed)
	}
}

// --- Example functions (auto-verified by go test) ---

func ExampleRoundRobinPartitioner_Partition() {
	p := &RoundRobinPartitioner{}
	r := Record{}
	fmt.Println(p.Partition(r, 4))
	fmt.Println(p.Partition(r, 4))
	fmt.Println(p.Partition(r, 4))
	fmt.Println(p.Partition(r, 4))
	fmt.Println(p.Partition(r, 4)) // wraps: 4 % 4 = 0
	// Output:
	// 0
	// 1
	// 2
	// 3
	// 0
}

func ExampleBroadcastPartitioner_Partition() {
	p := BroadcastPartitioner{}
	// BroadcastAll signals the fan-out loop to copy the record to every partition.
	fmt.Println(p.Partition(Record{}, 4) == BroadcastAll)
	fmt.Println(p.Partition(Record{}, 1) == BroadcastAll)
	// Output:
	// true
	// true
}
```

## Review

The harness is correct when the three channel-lifecycle rules hold. First, the fan-out goroutine is the only writer of the `partIn` channels and closes them all on exit; if any other goroutine closes or writes them, a `close` of a channel with a concurrent send panics. Second, each fan-in goroutine drains both the record channel and the error channel — dropping the error drain leaks the operator goroutine when an error arrives after the records. Third, the merged channels are closed exactly once, by the lone closer goroutine after `wg.Wait()`. The most common correctness bug unrelated to channels is the signed-modulo trap in `KeyPartitioner`: take the modulo on the `uint32` before casting to `int`. Run the suite under `go test -race` to confirm there is no unsynchronised access and no leaked goroutine.

## Resources

- [Go Blog: Pipelines and cancellation](https://go.dev/blog/pipelines) — the canonical fan-out/fan-in goroutine pattern this exercise implements.
- [hash/crc32](https://pkg.go.dev/hash/crc32) — the CRC-32/IEEE checksum used by `KeyPartitioner`.
- [sync/atomic](https://pkg.go.dev/sync/atomic) — the `atomic.Uint64` and `atomic.Int64` struct types (Go 1.19+).
- [Apache Flink: Parallel Execution](https://nightlies.apache.org/flink/flink-docs-stable/docs/dev/datastream/execution/parallel/) — the production reference for the parallelism / subtask model.

---

Back to [00-concepts.md](00-concepts.md) | Next: [02-affinity-partitioner.md](02-affinity-partitioner.md)
