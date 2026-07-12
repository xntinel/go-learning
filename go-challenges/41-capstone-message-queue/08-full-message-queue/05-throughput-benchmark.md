# Exercise 5: Throughput Benchmark

A headline "messages per second" number is meaningless without the hardware, value size, partition count, and fsync policy behind it, and quoting one as a guarantee is dishonest. What is reproducible is the *count* of work a fixed load performs. This exercise builds a deterministic load generator that drives a known workload through the broker and asserts on counts — produced equals consumed equals the planned total — plus a standard Go `Benchmark` for measuring real timings on your own hardware.

This module is fully self-contained: its own `go mod init`, an inline value-only partition log and broker, its own demo and tests. Nothing here imports another exercise.

## What you'll build

```text
log.go            PartitionLog: minimal value-only append-only segment
broker.go         Broker: Open, CreateTopic, Produce, Fetch, Close
load.go           RunLoad: deterministic concurrent producer + consumer counts
cmd/
  demo/
    main.go       run a fixed workload, print counts (no timings)
load_test.go      assert counts are exact and deterministic; BenchmarkProduce
```

- Files: `log.go`, `broker.go`, `load.go`, `cmd/demo/main.go`, `load_test.go`.
- Implement: `RunLoad(b, topic, partitions, perPartition, valueSize)` returning a `LoadResult` of counts, plus `BenchmarkProduce`.
- Test: the counts are exactly `partitions * perPartition` produced and consumed, and `partitions * perPartition * valueSize` bytes, regardless of concurrency.
- Verify: `go test -race ./...` and (optionally) `go test -bench=. -benchtime=2s`.

### Why counts, not timings

The benchmark's job is to be a regression guard and a demonstration, not a marketing number. A regression guard must be deterministic: it has to produce the same answer on a fast laptop, a loaded CI runner, and a reviewer's machine, or it is useless as a check. Wall-clock throughput satisfies none of that — it depends on the disk, the scheduler, whether the race detector is on, and what else is running. The *count* of records moved does satisfy it: if the load plan says produce `partitions * perPartition` records and consume them back, then a correct broker reports exactly those counts every time, and a count that comes back wrong means a real bug (a dropped append, a partition that lost records, an off-by-one in the consumer loop).

So `RunLoad` fans out one producer goroutine per partition — exercising the per-partition locks concurrently, which is what the race detector inspects — and then consumes every partition and tallies what comes back. It returns a `LoadResult` of pure counts. The concurrency is real (the producers run in parallel and contend on nothing because each owns a distinct partition), but the result is deterministic because counts do not depend on timing. For anyone who genuinely wants nanoseconds-per-op, `BenchmarkProduce` is a standard `testing.B` loop that the Go tool times on the machine it runs on — the honest place for a timing number, reported with its own hardware context.

Create `log.go`:

```go
package broker

import (
	"encoding/binary"
	"errors"
	"fmt"
	"io"
	"os"
	"sort"
	"sync"
)

// header is offset(8) + valueLen(4); values carry no key in this minimal log.
const headerSize = 12

type indexEntry struct {
	offset  int64
	filePos int64
}

// PartitionLog is a minimal value-only append-only log for one partition.
type PartitionLog struct {
	mu      sync.Mutex
	file    *os.File
	index   []indexEntry
	nextOff int64
	size    int64
}

func newPartitionLog(dir string) (*PartitionLog, error) {
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return nil, fmt.Errorf("partition: mkdir %s: %w", dir, err)
	}
	f, err := os.OpenFile(dir+"/segment.log", os.O_CREATE|os.O_RDWR|os.O_APPEND, 0o644)
	if err != nil {
		return nil, fmt.Errorf("partition: open: %w", err)
	}
	pl := &PartitionLog{file: f}
	if err := pl.recover(); err != nil {
		f.Close()
		return nil, err
	}
	return pl, nil
}

func (pl *PartitionLog) append(value []byte) (int64, error) {
	pl.mu.Lock()
	defer pl.mu.Unlock()

	off := pl.nextOff
	buf := make([]byte, headerSize+len(value))
	binary.BigEndian.PutUint64(buf[0:8], uint64(off))
	binary.BigEndian.PutUint32(buf[8:12], uint32(len(value)))
	copy(buf[12:], value)

	filePos := pl.size
	n, err := pl.file.Write(buf)
	if err != nil {
		return 0, fmt.Errorf("partition: write offset %d: %w", off, err)
	}
	pl.index = append(pl.index, indexEntry{offset: off, filePos: filePos})
	pl.nextOff++
	pl.size += int64(n)
	return off, nil
}

func (pl *PartitionLog) readFrom(startOffset int64, maxMsgs int) ([][]byte, error) {
	pl.mu.Lock()
	defer pl.mu.Unlock()
	if len(pl.index) == 0 || startOffset >= pl.nextOff {
		return nil, nil
	}
	i := sort.Search(len(pl.index), func(k int) bool {
		return pl.index[k].offset >= startOffset
	})
	if i >= len(pl.index) {
		return nil, nil
	}
	pos := pl.index[i].filePos
	var out [][]byte
	for len(out) < maxMsgs {
		val, next, err := pl.decodeAt(pos)
		if err != nil {
			if errors.Is(err, io.EOF) || errors.Is(err, io.ErrUnexpectedEOF) {
				break
			}
			return nil, fmt.Errorf("partition: decode at %d: %w", pos, err)
		}
		out = append(out, val)
		pos = next
	}
	return out, nil
}

func (pl *PartitionLog) decodeAt(pos int64) ([]byte, int64, error) {
	var hdr [headerSize]byte
	if _, err := pl.file.ReadAt(hdr[:], pos); err != nil {
		return nil, 0, err
	}
	valLen := int(binary.BigEndian.Uint32(hdr[8:12]))
	pos += headerSize
	val := make([]byte, valLen)
	if valLen > 0 {
		if _, err := pl.file.ReadAt(val, pos); err != nil {
			return nil, 0, err
		}
	}
	return val, pos + int64(valLen), nil
}

func (pl *PartitionLog) recover() error {
	info, err := pl.file.Stat()
	if err != nil {
		return fmt.Errorf("partition: stat: %w", err)
	}
	pl.size = info.Size()
	pl.index = pl.index[:0]
	pl.nextOff = 0

	pos := int64(0)
	for pos < pl.size {
		_, next, err := pl.decodeAt(pos)
		if err != nil {
			if errors.Is(err, io.EOF) || errors.Is(err, io.ErrUnexpectedEOF) {
				if terr := pl.file.Truncate(pos); terr != nil {
					return fmt.Errorf("partition: truncate partial tail: %w", terr)
				}
				pl.size = pos
				break
			}
			return fmt.Errorf("partition: recover at %d: %w", pos, err)
		}
		pl.index = append(pl.index, indexEntry{offset: pl.nextOff, filePos: pos})
		pl.nextOff++
		pos = next
	}
	return nil
}

func (pl *PartitionLog) close() error {
	pl.mu.Lock()
	defer pl.mu.Unlock()
	return pl.file.Close()
}
```

Create `broker.go`:

```go
package broker

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"sync"
)

var (
	ErrTopicNotFound    = errors.New("broker: topic not found")
	ErrPartitionInvalid = errors.New("broker: invalid partition")
)

// Broker is a minimal file-backed broker used to drive a deterministic load.
type Broker struct {
	mu     sync.RWMutex
	dir    string
	topics map[string][]*PartitionLog
}

// Open creates a broker rooted at dir.
func Open(dir string) (*Broker, error) {
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return nil, fmt.Errorf("broker: mkdir %s: %w", dir, err)
	}
	return &Broker{dir: dir, topics: make(map[string][]*PartitionLog)}, nil
}

// CreateTopic creates a topic with numPartitions partitions. Idempotent.
func (b *Broker) CreateTopic(name string, numPartitions int) error {
	if numPartitions < 1 {
		return fmt.Errorf("broker: %w: partitions must be >= 1", ErrPartitionInvalid)
	}
	b.mu.Lock()
	defer b.mu.Unlock()
	if _, ok := b.topics[name]; ok {
		return nil
	}
	partitions := make([]*PartitionLog, numPartitions)
	for i := range partitions {
		pl, err := newPartitionLog(filepath.Join(b.dir, name, fmt.Sprintf("partition-%d", i)))
		if err != nil {
			return err
		}
		partitions[i] = pl
	}
	b.topics[name] = partitions
	return nil
}

func (b *Broker) partition(topic string, p int) (*PartitionLog, error) {
	b.mu.RLock()
	defer b.mu.RUnlock()
	parts, ok := b.topics[topic]
	if !ok {
		return nil, fmt.Errorf("%w: %s", ErrTopicNotFound, topic)
	}
	if p < 0 || p >= len(parts) {
		return nil, fmt.Errorf("%w: %s/%d", ErrPartitionInvalid, topic, p)
	}
	return parts[p], nil
}

// Produce appends value and returns its offset.
func (b *Broker) Produce(topic string, partition int, value []byte) (int64, error) {
	pl, err := b.partition(topic, partition)
	if err != nil {
		return 0, err
	}
	return pl.append(value)
}

// Fetch returns up to maxMsgs values from offset (non-blocking).
func (b *Broker) Fetch(topic string, partition int, offset int64, maxMsgs int) ([][]byte, error) {
	pl, err := b.partition(topic, partition)
	if err != nil {
		return nil, err
	}
	return pl.readFrom(offset, maxMsgs)
}

// Close closes all partition logs.
func (b *Broker) Close() error {
	b.mu.Lock()
	defer b.mu.Unlock()
	var first error
	for _, parts := range b.topics {
		for _, pl := range parts {
			if err := pl.close(); err != nil && first == nil {
				first = err
			}
		}
	}
	return first
}
```

Create `load.go`:

```go
package broker

import (
	"bytes"
	"fmt"
	"sync"
	"sync/atomic"
)

// LoadResult holds the deterministic counts of one load run.
type LoadResult struct {
	Partitions       int
	MessagesProduced int64
	MessagesConsumed int64
	BytesProduced    int64
}

// RunLoad produces perPartition messages of valueSize bytes into each of
// partitions partitions concurrently, then consumes every partition and tallies
// the counts. The returned counts are deterministic; only timing varies.
func RunLoad(b *Broker, topic string, partitions, perPartition, valueSize int) (LoadResult, error) {
	if partitions < 1 || perPartition < 0 || valueSize < 0 {
		return LoadResult{}, fmt.Errorf("load: invalid plan partitions=%d perPartition=%d valueSize=%d", partitions, perPartition, valueSize)
	}
	if err := b.CreateTopic(topic, partitions); err != nil {
		return LoadResult{}, err
	}
	value := bytes.Repeat([]byte{'x'}, valueSize)

	var produced, bytesProduced atomic.Int64
	errs := make([]error, partitions)
	var wg sync.WaitGroup
	for p := 0; p < partitions; p++ {
		wg.Add(1)
		go func(p int) {
			defer wg.Done()
			for i := 0; i < perPartition; i++ {
				if _, err := b.Produce(topic, p, value); err != nil {
					errs[p] = err
					return
				}
				produced.Add(1)
				bytesProduced.Add(int64(valueSize))
			}
		}(p)
	}
	wg.Wait()
	for _, e := range errs {
		if e != nil {
			return LoadResult{}, e
		}
	}

	var consumed int64
	for p := 0; p < partitions; p++ {
		msgs, err := b.Fetch(topic, p, 0, perPartition+1)
		if err != nil {
			return LoadResult{}, err
		}
		consumed += int64(len(msgs))
	}

	return LoadResult{
		Partitions:       partitions,
		MessagesProduced: produced.Load(),
		MessagesConsumed: consumed,
		BytesProduced:    bytesProduced.Load(),
	}, nil
}
```

`RunLoad` is deliberately boring in its result and interesting in its concurrency. The producers run in parallel — one per partition — so the race detector exercises the per-partition `append` path under genuine concurrent pressure, which is the bug class (a data race on the index or the file size) this exercise is positioned to catch. But the counts it returns depend only on the plan: `partitions * perPartition` produced, the same consumed if no record was dropped, and `partitions * perPartition * valueSize` bytes. Any deviation is a correctness failure, not a timing artifact.

### The runnable demo

The demo runs a fixed plan and prints the counts. No timings, addresses, or timestamps appear, so the output is identical on every machine.

Create `cmd/demo/main.go`:

```go
package main

import (
	"fmt"
	"log"
	"os"

	"example.com/throughput"
)

func main() {
	dir, err := os.MkdirTemp("", "throughput-demo-*")
	if err != nil {
		log.Fatal(err)
	}
	defer os.RemoveAll(dir)

	b, err := broker.Open(dir)
	if err != nil {
		log.Fatal(err)
	}
	defer b.Close()

	const (
		partitions   = 4
		perPartition = 2500
		valueSize    = 64
	)
	res, err := broker.RunLoad(b, "load", partitions, perPartition, valueSize)
	if err != nil {
		log.Fatal(err)
	}

	fmt.Printf("partitions=%d\n", res.Partitions)
	fmt.Printf("produced=%d\n", res.MessagesProduced)
	fmt.Printf("consumed=%d\n", res.MessagesConsumed)
	fmt.Printf("bytes=%d\n", res.BytesProduced)
	if res.MessagesProduced == res.MessagesConsumed {
		fmt.Println("check: produced == consumed (no records lost)")
	}
}
```

Run it:

```bash
go run ./cmd/demo
```

Expected output:

```
partitions=4
produced=10000
consumed=10000
bytes=640000
check: produced == consumed (no records lost)
```

### Tests

The test asserts the exact counts for a plan, which is the deterministic contract, and includes a standard benchmark for anyone who wants real timings.

Create `load_test.go`:

```go
package broker

import (
	"testing"
)

func TestRunLoadCountsAreExact(t *testing.T) {
	t.Parallel()
	b, err := Open(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	defer b.Close()

	const (
		partitions   = 4
		perPartition = 2500
		valueSize    = 64
	)
	res, err := RunLoad(b, "load", partitions, perPartition, valueSize)
	if err != nil {
		t.Fatal(err)
	}

	wantMsgs := int64(partitions * perPartition)
	if res.MessagesProduced != wantMsgs {
		t.Fatalf("produced = %d, want %d", res.MessagesProduced, wantMsgs)
	}
	if res.MessagesConsumed != wantMsgs {
		t.Fatalf("consumed = %d, want %d", res.MessagesConsumed, wantMsgs)
	}
	if want := int64(partitions * perPartition * valueSize); res.BytesProduced != want {
		t.Fatalf("bytes = %d, want %d", res.BytesProduced, want)
	}
}

func TestRunLoadIsDeterministic(t *testing.T) {
	t.Parallel()
	run := func() LoadResult {
		b, err := Open(t.TempDir())
		if err != nil {
			t.Fatal(err)
		}
		defer b.Close()
		res, err := RunLoad(b, "load", 3, 1000, 32)
		if err != nil {
			t.Fatal(err)
		}
		return res
	}
	a := run()
	b := run()
	if a != b {
		t.Fatalf("non-deterministic counts: %+v vs %+v", a, b)
	}
}

func TestRunLoadEmptyPlan(t *testing.T) {
	t.Parallel()
	b, err := Open(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	defer b.Close()
	res, err := RunLoad(b, "empty", 2, 0, 16)
	if err != nil {
		t.Fatal(err)
	}
	if res.MessagesProduced != 0 || res.MessagesConsumed != 0 || res.BytesProduced != 0 {
		t.Fatalf("empty plan produced non-zero counts: %+v", res)
	}
}

func BenchmarkProduce(b *testing.B) {
	br, err := Open(b.TempDir())
	if err != nil {
		b.Fatal(err)
	}
	defer br.Close()
	if err := br.CreateTopic("bench", 1); err != nil {
		b.Fatal(err)
	}
	value := make([]byte, 128)
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		if _, err := br.Produce("bench", 0, value); err != nil {
			b.Fatal(err)
		}
	}
}
```

Run the benchmark (timings are yours, not a guarantee):

```bash
go test -bench=. -benchtime=2s
```

## Review

The load generator is correct when its counts are exact and reproducible. `TestRunLoadCountsAreExact` asserts produced and consumed both equal `partitions * perPartition` and bytes equal that times `valueSize` — the deterministic contract that makes this usable as a regression guard rather than a flaky timing check. `TestRunLoadIsDeterministic` runs the same plan twice and asserts the `LoadResult` values are identical, which is the property that lets the count assertion mean something on any machine. `TestRunLoadEmptyPlan` covers the boundary where `perPartition` is zero. The concurrency is the part the race detector earns its keep on: producers run one per partition, so `go test -race` is what proves the per-partition `append` has no data race on the index or file size. The discipline to carry forward is the separation of concerns — counts for correctness and CI, the `Benchmark` for timing on known hardware — and the refusal to bake a wall-clock number into a pass/fail check or into prose as if it were a guarantee.

## Resources

- [`testing.B` and writing benchmarks](https://pkg.go.dev/testing#hdr-Benchmarks) — the standard, honest place for a timing number, measured on the machine that runs it.
- [`sync/atomic`](https://pkg.go.dev/sync/atomic) — the lock-free counters that tally produced records and bytes without contending on a mutex.
- [Go blog: Profiling Go programs](https://go.dev/blog/pprof) — how to turn a benchmark into a real performance investigation when you do want timings.

---

Back to [04-end-to-end-replay.md](04-end-to-end-replay.md) | Next: [L4 TCP Proxy](../../42-capstone-service-mesh-data-plane/01-l4-tcp-proxy/00-concepts.md)
