# Exercise 2: The Runnable Windowed Engine

The job-graph engine produces a topology but never moves a record. This module is the data plane: a real pipeline of goroutines that ingests records and watermarks, transforms them through map and filter, shuffles them by key across parallel window operators, fires tumbling windows when watermarks prove event time has advanced, and fans the results into a sink — an end-to-end windowed aggregation you can run, race-test, and replay deterministically.

This module is fully self-contained. It defines its own record/message types, the source, the operators, the windowed reduce, and the fan-in sink in package `windowengine`, plus its own demo and tests. Nothing here imports another exercise.

## What you'll build

```text
engine.go     Record, message (record|watermark tagged union), WindowResult,
              Source + SliceSource, Pipeline fluent builder
              (Map/Filter/KeyBy/Window/Reduce/Parallelism),
              Run: source -> map+filter -> shuffle(by key) ->
                   parallel windowed reduce -> fan-in sink
cmd/
  demo/
    main.go   a word-count over an event-time stream, parallelism 2
engine_test.go end-to-end counts, parallelism invariance, map+filter,
              max-reduce, cancellation, example
```

- Files: `engine.go`, `cmd/demo/main.go`, `engine_test.go`.
- Implement: the goroutine pipeline in `Run` — the source emitter, the map+filter stage, the key-hash shuffle that broadcasts watermarks, the per-partition windowed reduce that fires on watermarks, and the fan-in sink with a single-closer result channel.
- Test: end-to-end windowed counts; identical results at parallelism 1/2/4/8 (partition independence); map and filter applied before windowing; a max aggregation through the same reduce machinery; prompt return under a cancelled context; and an example verified by `go test`.
- Verify: `go test -race ./...`

Set up the module:

```bash
mkdir -p windowengine/cmd/demo && cd windowengine
go mod init example.com/windowengine
go mod edit -go=1.26
```

### How a topology actually runs

Every operator is a goroutine; every edge is a bounded channel carrying the same tagged union — a `message` that is either a record or a watermark. This single-channel-per-link choice is the same one the job-graph engine makes, and it is what lets the window operator treat "new data" and "time advanced" as two cases of one `range` loop instead of two channels it must select over.

`Run` wires five stages. The **source** emits records in event-time order and interleaves a watermark after each one carrying that record's event time, then a final watermark of `math.MaxInt64` to flush every still-open window at end of stream. The **map+filter** stage applies the map to each record, drops the ones the filter rejects, and forwards watermarks untouched — a watermark must never be filtered, or downstream windows would never fire. The **shuffle** is the heart of the keyed parallelism: a record goes to exactly one of the `N` partitions chosen by `hash(key) % N`, while a watermark is *broadcast* to all `N` partitions, because event time advances everywhere at once. Each **window operator** keeps a `map[windowStart]map[key]int64` accumulator; a record folds into its window-and-key slot via the reduce function, and a watermark fires every window whose end is at or before the watermark, emitting one result per key. The **sink** is the main goroutine, fanning in results from all partitions.

Two design points make this correct under `-race` and reproducible across runs. First, the result channel has exactly one closer: a goroutine that `wg.Wait()`s for all window operators and only then closes the channel. If each operator closed it, the second close — or a send racing a close — would panic; the single-closer rule is the canonical fan-in discipline. Second, because the shuffle sends a given key to exactly one partition, that key's entire history accumulates in one place, so the aggregate is independent of `N`; and because results arrive in goroutine-scheduling order, `Run` sorts the collected slice by `(windowStart, key)` before returning. The aggregate is computed in parallel but observed in a stable order.

The reduce step is a plain `func(a, b int64) int64`. The operator seeds a window-and-key slot with the first value it sees and folds every later value with the reduce function, so `a + b` yields counts or sums and `max(a, b)` yields a per-window high-water mark — the execution code never changes. Watermark-driven firing means the output depends only on event time, never on wall-clock timing, which is exactly why the tests can assert an exact result.

Create `engine.go`:

```go
// Package windowengine runs a complete streaming pipeline end to end: a source
// feeds records and watermarks through map and filter operators into a keyed,
// parallel tumbling-window aggregation, and a sink collects the fired window
// results. It is the runnable counterpart to the job-graph engine: where that
// module compiles a topology, this one executes one.
package windowengine

import (
	"context"
	"hash/fnv"
	"math"
	"sort"
	"sync"
)

// Record is one event: a key, an integer value to aggregate, and an event-time
// timestamp expressed as logical milliseconds since an arbitrary epoch.
type Record struct {
	Key       string
	Value     int64
	EventTime int64
}

// WindowResult is one fired aggregate: the window's start, the key, and the
// reduced value for that key within that window.
type WindowResult struct {
	WindowStart int64
	Key         string
	Value       int64
}

// msgKind discriminates a message payload.
type msgKind uint8

const (
	kindRecord msgKind = iota
	kindWatermark
)

// message is the tagged union carried by every inter-operator channel: either a
// data record or a watermark asserting that no record older than wm follows.
type message struct {
	kind msgKind
	rec  Record
	wm   int64
}

// Source feeds records and watermarks into the pipeline. Emit must close over
// the context and stop sending when ctx is cancelled.
type Source interface {
	Emit(ctx context.Context, out chan<- message)
}

// SliceSource emits a fixed slice of records in order, injecting a watermark
// equal to each record's event time, then a final flush watermark.
type SliceSource struct {
	Records []Record
}

// Emit sends each record followed by a watermark at that record's event time,
// then a final math.MaxInt64 watermark that closes every still-open window.
func (s SliceSource) Emit(ctx context.Context, out chan<- message) {
	for _, r := range s.Records {
		if !send(ctx, out, message{kind: kindRecord, rec: r}) {
			return
		}
		if !send(ctx, out, message{kind: kindWatermark, wm: r.EventTime}) {
			return
		}
	}
	send(ctx, out, message{kind: kindWatermark, wm: math.MaxInt64})
}

// send delivers m unless ctx is cancelled first. It returns false when the send
// was abandoned, so callers can stop promptly without leaking a goroutine.
func send(ctx context.Context, out chan<- message, m message) bool {
	select {
	case out <- m:
		return true
	case <-ctx.Done():
		return false
	}
}

// Pipeline is a fluent description of a windowed aggregation. Build it with the
// chained setters, then call Run to execute it.
type Pipeline struct {
	source      Source
	mapFn       func(Record) Record
	filterFn    func(Record) bool
	keyFn       func(Record) string
	windowSize  int64
	reduceFn    func(a, b int64) int64
	parallelism int
}

// New returns a Pipeline over src with identity map, pass-all filter, key-by-Key,
// a window of size 1, sum reduction, and parallelism 1. Override any of these
// with the setters before calling Run.
func New(src Source) *Pipeline {
	return &Pipeline{
		source:      src,
		mapFn:       func(r Record) Record { return r },
		filterFn:    func(Record) bool { return true },
		keyFn:       func(r Record) string { return r.Key },
		windowSize:  1,
		reduceFn:    func(a, b int64) int64 { return a + b },
		parallelism: 1,
	}
}

// Map sets the record transformation applied before windowing.
func (p *Pipeline) Map(fn func(Record) Record) *Pipeline { p.mapFn = fn; return p }

// Filter sets the predicate; records for which it returns false are dropped.
func (p *Pipeline) Filter(fn func(Record) bool) *Pipeline { p.filterFn = fn; return p }

// KeyBy sets the function that extracts the partition/grouping key.
func (p *Pipeline) KeyBy(fn func(Record) string) *Pipeline { p.keyFn = fn; return p }

// Window sets the tumbling-window size in the same units as Record.EventTime.
func (p *Pipeline) Window(size int64) *Pipeline { p.windowSize = size; return p }

// Reduce sets the binary aggregation folded over each window-and-key group.
func (p *Pipeline) Reduce(fn func(a, b int64) int64) *Pipeline { p.reduceFn = fn; return p }

// Parallelism sets the number of parallel window operators. Values below 1 are
// ignored.
func (p *Pipeline) Parallelism(n int) *Pipeline {
	if n >= 1 {
		p.parallelism = n
	}
	return p
}

// Run executes the pipeline to completion and returns the fired window results
// sorted by (WindowStart, Key). It is deterministic for a given input: results
// depend only on event time, not on goroutine scheduling or parallelism. Run
// returns promptly if ctx is cancelled, with whatever results were collected.
func (p *Pipeline) Run(ctx context.Context) []WindowResult {
	if p.windowSize < 1 {
		p.windowSize = 1
	}
	n := p.parallelism
	if n < 1 {
		n = 1
	}

	srcOut := make(chan message, 16)
	stageOut := make(chan message, 16)
	parts := make([]chan message, n)
	for i := range parts {
		parts[i] = make(chan message, 16)
	}
	results := make(chan WindowResult, 64)

	var stages sync.WaitGroup

	// Stage 1: source emitter.
	stages.Add(1)
	go func() {
		defer stages.Done()
		defer close(srcOut)
		p.source.Emit(ctx, srcOut)
	}()

	// Stage 2: map + filter. Records are transformed and possibly dropped;
	// watermarks pass through untouched so downstream windows still fire.
	stages.Add(1)
	go func() {
		defer stages.Done()
		defer close(stageOut)
		for m := range srcOut {
			if m.kind == kindRecord {
				r := p.mapFn(m.rec)
				if !p.filterFn(r) {
					continue
				}
				m.rec = r
			}
			if !send(ctx, stageOut, m) {
				return
			}
		}
	}()

	// Stage 3: shuffle. A record goes to one partition by key hash; a watermark
	// is broadcast to every partition because event time advances everywhere.
	stages.Add(1)
	go func() {
		defer stages.Done()
		defer func() {
			for _, c := range parts {
				close(c)
			}
		}()
		for m := range stageOut {
			if m.kind == kindWatermark {
				for _, c := range parts {
					if !send(ctx, c, m) {
						return
					}
				}
				continue
			}
			idx := int(hashKey(p.keyFn(m.rec)) % uint64(n))
			if !send(ctx, parts[idx], m) {
				return
			}
		}
	}()

	// Stage 4: parallel windowed reduce, one goroutine per partition.
	var ops sync.WaitGroup
	for i := 0; i < n; i++ {
		ops.Add(1)
		go func(in <-chan message) {
			defer ops.Done()
			p.runWindow(ctx, in, results)
		}(parts[i])
	}
	// Single closer for the fan-in channel: wait for every operator first.
	go func() {
		ops.Wait()
		close(results)
	}()

	// Stage 5: sink. The main goroutine fans in every fired result.
	var out []WindowResult
	for r := range results {
		out = append(out, r)
	}
	stages.Wait()

	sort.Slice(out, func(i, j int) bool {
		if out[i].WindowStart != out[j].WindowStart {
			return out[i].WindowStart < out[j].WindowStart
		}
		return out[i].Key < out[j].Key
	})
	return out
}

// runWindow accumulates records into per-window, per-key slots and fires each
// window when a watermark proves event time has passed its end boundary.
func (p *Pipeline) runWindow(ctx context.Context, in <-chan message, out chan<- WindowResult) {
	windows := make(map[int64]map[string]int64)

	fire := func(upTo int64) bool {
		var starts []int64
		for ws := range windows {
			if ws+p.windowSize <= upTo {
				starts = append(starts, ws)
			}
		}
		sort.Slice(starts, func(i, j int) bool { return starts[i] < starts[j] })
		for _, ws := range starts {
			bucket := windows[ws]
			keys := make([]string, 0, len(bucket))
			for k := range bucket {
				keys = append(keys, k)
			}
			sort.Strings(keys)
			for _, k := range keys {
				select {
				case out <- WindowResult{WindowStart: ws, Key: k, Value: bucket[k]}:
				case <-ctx.Done():
					return false
				}
			}
			delete(windows, ws)
		}
		return true
	}

	for m := range in {
		switch m.kind {
		case kindRecord:
			ws := windowStart(m.rec.EventTime, p.windowSize)
			bucket := windows[ws]
			if bucket == nil {
				bucket = make(map[string]int64)
				windows[ws] = bucket
			}
			if existing, ok := bucket[m.rec.Key]; ok {
				bucket[m.rec.Key] = p.reduceFn(existing, m.rec.Value)
			} else {
				bucket[m.rec.Key] = m.rec.Value
			}
		case kindWatermark:
			if !fire(m.wm) {
				return
			}
		}
	}
}

// windowStart rounds an event time down to the start of its tumbling window.
func windowStart(t, size int64) int64 {
	return (t / size) * size
}

// hashKey is a stable 64-bit hash used to assign a key to a shuffle partition.
func hashKey(s string) uint64 {
	h := fnv.New64a()
	_, _ = h.Write([]byte(s))
	return h.Sum64()
}
```

### The runnable demo

The demo runs a word count over a seven-record event-time stream with a one-second (1000 ms) tumbling window at parallelism 2. Each record contributes a count of 1; the reduce sums them per word per window. The flush watermark closes the final window, and the sorted output is identical on every run.

Create `cmd/demo/main.go`:

```go
// cmd/demo runs an end-to-end windowed word count and prints the per-window,
// per-key counts in a stable order.
//
// Run with: go run ./cmd/demo
package main

import (
	"context"
	"fmt"

	"example.com/windowengine"
)

func main() {
	src := windowengine.SliceSource{Records: []windowengine.Record{
		{Key: "go", Value: 1, EventTime: 100},
		{Key: "rust", Value: 1, EventTime: 200},
		{Key: "go", Value: 1, EventTime: 300},
		{Key: "go", Value: 1, EventTime: 1100},
		{Key: "rust", Value: 1, EventTime: 1200},
		{Key: "python", Value: 1, EventTime: 1500},
		{Key: "go", Value: 1, EventTime: 2100},
	}}

	results := windowengine.New(src).
		Filter(func(r windowengine.Record) bool { return r.Key != "" }).
		KeyBy(func(r windowengine.Record) string { return r.Key }).
		Window(1000).
		Reduce(func(a, b int64) int64 { return a + b }).
		Parallelism(2).
		Run(context.Background())

	fmt.Println("windowed word counts:")
	for _, r := range results {
		fmt.Printf("  window=%d key=%s count=%d\n", r.WindowStart, r.Key, r.Value)
	}
}
```

Run it:

```bash
go run ./cmd/demo
```

Expected output:

```text
windowed word counts:
  window=0 key=go count=2
  window=0 key=rust count=1
  window=1000 key=go count=1
  window=1000 key=python count=1
  window=1000 key=rust count=1
  window=2000 key=go count=1
```

### Tests

The tests pin the engine's correctness properties: the exact end-to-end counts, that parallelism never changes the result (the shuffle invariant), that map and filter run before windowing, that an arbitrary reduce (max) flows through the same code, and that a cancelled context makes `Run` return promptly. Run them with `-race` to prove the fan-in shutdown and the shuffle are free of data races. The example function is checked against its `// Output:` comment by `go test`.

Create `engine_test.go`:

```go
package windowengine

import (
	"context"
	"fmt"
	"sort"
	"testing"
)

func wordStream() SliceSource {
	return SliceSource{Records: []Record{
		{Key: "go", Value: 1, EventTime: 100},
		{Key: "rust", Value: 1, EventTime: 200},
		{Key: "go", Value: 1, EventTime: 300},
		{Key: "go", Value: 1, EventTime: 1100},
		{Key: "rust", Value: 1, EventTime: 1200},
		{Key: "python", Value: 1, EventTime: 1500},
		{Key: "go", Value: 1, EventTime: 2100},
	}}
}

func runWordCount(parallelism int) []WindowResult {
	return New(wordStream()).
		KeyBy(func(r Record) string { return r.Key }).
		Window(1000).
		Reduce(func(a, b int64) int64 { return a + b }).
		Parallelism(parallelism).
		Run(context.Background())
}

func assertResults(t *testing.T, got, want []WindowResult) {
	t.Helper()
	sort.Slice(want, func(i, j int) bool {
		if want[i].WindowStart != want[j].WindowStart {
			return want[i].WindowStart < want[j].WindowStart
		}
		return want[i].Key < want[j].Key
	})
	if len(got) != len(want) {
		t.Fatalf("len = %d, want %d\n got=%v\nwant=%v", len(got), len(want), got, want)
	}
	for i := range got {
		if got[i] != want[i] {
			t.Fatalf("result[%d] = %+v, want %+v", i, got[i], want[i])
		}
	}
}

func TestEndToEndWindowedCounts(t *testing.T) {
	t.Parallel()
	got := runWordCount(1)
	want := []WindowResult{
		{WindowStart: 0, Key: "go", Value: 2},
		{WindowStart: 0, Key: "rust", Value: 1},
		{WindowStart: 1000, Key: "go", Value: 1},
		{WindowStart: 1000, Key: "python", Value: 1},
		{WindowStart: 1000, Key: "rust", Value: 1},
		{WindowStart: 2000, Key: "go", Value: 1},
	}
	assertResults(t, got, want)
}

// TestParallelismDoesNotChangeResults proves the shuffle invariant: a key always
// lands on one partition, so the aggregate is identical at any parallelism.
func TestParallelismDoesNotChangeResults(t *testing.T) {
	t.Parallel()
	base := runWordCount(1)
	for _, n := range []int{2, 3, 4, 8} {
		assertResults(t, runWordCount(n), base)
	}
}

// TestMapAndFilter checks that the map runs before windowing and the filter
// drops records before they reach the aggregate.
func TestMapAndFilter(t *testing.T) {
	t.Parallel()
	src := SliceSource{Records: []Record{
		{Key: "a", Value: 5, EventTime: 0},
		{Key: "b", Value: 5, EventTime: 100},
		{Key: "a", Value: 5, EventTime: 200},
	}}
	got := New(src).
		Map(func(r Record) Record { r.Value *= 2; return r }).
		Filter(func(r Record) bool { return r.Key == "a" }).
		KeyBy(func(r Record) string { return r.Key }).
		Window(1000).
		Reduce(func(a, b int64) int64 { return a + b }).
		Parallelism(2).
		Run(context.Background())
	// Two "a" records, each 5*2=10, summed in window [0,1000) => 20. "b" dropped.
	assertResults(t, got, []WindowResult{{WindowStart: 0, Key: "a", Value: 20}})
}

// TestReduceMax shows that an arbitrary binary reduce flows through the same
// windowing machinery.
func TestReduceMax(t *testing.T) {
	t.Parallel()
	src := SliceSource{Records: []Record{
		{Key: "k", Value: 3, EventTime: 10},
		{Key: "k", Value: 9, EventTime: 20},
		{Key: "k", Value: 1, EventTime: 30},
	}}
	got := New(src).
		KeyBy(func(r Record) string { return r.Key }).
		Window(1000).
		Reduce(func(a, b int64) int64 {
			if a > b {
				return a
			}
			return b
		}).
		Run(context.Background())
	assertResults(t, got, []WindowResult{{WindowStart: 0, Key: "k", Value: 9}})
}

// TestRunRespectsCancelledContext verifies Run returns promptly under a
// cancelled context without panicking or leaking a goroutine into a closed
// channel; the race detector validates the shutdown path.
func TestRunRespectsCancelledContext(t *testing.T) {
	t.Parallel()
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	recs := make([]Record, 1000)
	for i := range recs {
		recs[i] = Record{Key: "k", Value: 1, EventTime: int64(i)}
	}
	_ = New(SliceSource{Records: recs}).
		KeyBy(func(r Record) string { return r.Key }).
		Window(10).
		Parallelism(4).
		Run(ctx)
}

func ExamplePipeline_Run() {
	src := SliceSource{Records: []Record{
		{Key: "x", Value: 1, EventTime: 0},
		{Key: "x", Value: 1, EventTime: 100},
	}}
	res := New(src).Window(1000).Run(context.Background())
	fmt.Printf("%+v\n", res)
	// Output:
	// [{WindowStart:0 Key:x Value:2}]
}
```

## Review

The engine is correct when the aggregate matches by event time and is stable across parallelism. `TestEndToEndWindowedCounts` pins the exact per-window counts, including the final window that only the `math.MaxInt64` flush watermark can close. `TestParallelismDoesNotChangeResults` is the key invariant: if a refactor ever broadcasts records instead of routing them by key hash, or hashes inconsistently, the counts diverge at `N > 1` and this test catches it. The mistakes most likely to break the engine are filtering watermarks along with records (windows then never fire and the result is empty), closing the result channel from each operator instead of from one waiter (a `send on closed channel` panic, or a close/close panic), and reading results in arrival order instead of sorting (a flaky test that passes at parallelism 1 and fails at 2). Run the suite with `-race`: the cancellation test drives 1000 records through four partitions and then tears down, which is where a missing `ctx.Done()` arm in a send would surface as a leak or a race.

## Resources

- [Go blog: Pipelines and cancellation](https://go.dev/blog/pipelines) — the fan-out/fan-in pattern and the single-closer rule this engine follows.
- [The Dataflow Model (VLDB 2015)](https://research.google/pubs/pub43864/) — the event-time, windowing, and watermark model that underlies tumbling-window firing.
- [Apache Flink: timely stream processing](https://nightlies.apache.org/flink/flink-docs-stable/docs/concepts/time/) — event time vs. processing time and how watermarks trigger window emission in production.
- [pkg.go.dev/hash/fnv](https://pkg.go.dev/hash/fnv) — the FNV-1a hash used for stable key-to-partition assignment in the shuffle.

---

Back to [01-job-graph-engine.md](01-job-graph-engine.md) | Next: [Frame Parsing](../../44-capstone-http2-implementation/01-frame-parsing/00-concepts.md)
