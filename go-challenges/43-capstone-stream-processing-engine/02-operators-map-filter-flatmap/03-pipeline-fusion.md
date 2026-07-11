# Exercise 3: Pipeline Builder with Operator Fusion

Operators compose by hand by feeding one's output channel into the next's input, but a builder makes the topology declarative and opens the door to an optimization: fusing adjacent Map operators into one so the pipeline runs fewer goroutines and allocates fewer channels. This exercise builds the `Pipeline` builder, the single-pass fusion algorithm, and the composite operator that wires the fused steps together and merges their error channels.

This module is fully self-contained. It bundles the operator core plus Map, Filter, and FlatMap, then adds the pipeline. Nothing here imports any other exercise.

## What you'll build

```text
operator.go            Record, ErrorAction, Metrics, operatorConfig, Operator
map.go                 MapOperator + Fn (exported so fuse can read the function)
filter.go              FilterOperator
flatmap.go             FlatMapOperator
pipeline.go            Pipeline, fuse, Build, compositeOperator (fan-in error merge)
cmd/
  demo/
    main.go            Filter -> Map -> FlatMap end to end
operators_test.go      chaining, expansion, fusion merges/skips, error channel closes
```

- Files: `operator.go`, `map.go`, `filter.go`, `flatmap.go`, `pipeline.go`, `cmd/demo/main.go`, `operators_test.go`.
- Implement: `Pipeline` with `Map`/`Filter`/`FlatMap`/`Build`, the `fuse` pass that merges adjacent `*MapOperator` pairs, and `compositeOperator.Process`, which chains the steps and closes the merged error channel once every per-step fan-in goroutine has finished.
- Test: `operators_test.go` checks Map→Filter chaining, FlatMap expansion through the builder, that `fuse` merges two adjacent maps into one and leaves a filter-separated pair alone, and that the composite's merged error channel is closed when the stream drains.
- Verify: `go test -race ./...`

Set up the module:

```bash
mkdir -p pipeline-fusion/cmd/demo && cd pipeline-fusion
go mod init example.com/pipeline-fusion
go mod edit -go=1.26
```

### Why fusion is a single pass, and why the error channel needs a WaitGroup

The builder accumulates operators in a slice; `Build` runs `fuse` over them and returns a `compositeOperator` that wires the survivors in sequence. Fusion exists because each operator is a goroutine plus a channel, and two adjacent stateless maps do redundant work: the first map's goroutine receives a record, transforms it, and sends it across a channel only for the second map's goroutine to receive it, transform it, and send it again. Fusing them replaces the pair with one operator whose function is the composition `outer(inner(r))`, deleting one goroutine and one channel hop per fused pair. The composed function threads the error through correctly — if the inner transform fails it returns immediately and the outer never runs — so the fused operator's error semantics match running the two separately.

The fusion scan is deliberately a single left-to-right pass. When it finds a `*MapOperator` immediately followed by another `*MapOperator`, it emits one fused operator and advances the index by two; otherwise it emits the current operator and advances by one. Three adjacent maps therefore fuse the first two and leave the third, producing two operators rather than fully collapsing to one — a simple, predictable rule that never reorders operators. A Filter or FlatMap between two maps breaks adjacency, so neither map fuses; this matters because fusion is only valid across operators that are stateless and order-preserving, and the type check `ops[i].(*MapOperator)` is exactly the guard that keeps a stateful operator from ever being fused.

The composite's error handling is where the original single-file version left a deliberate gap. Each step returns its own error channel; the composite spawns one goroutine per step to fan those into a single merged channel. The merged channel must be closed when, and only when, every fan-in goroutine has finished — close it too early and a late error sends on a closed channel and panics; never close it and a caller ranging over it blocks forever after the stream ends. A `sync.WaitGroup` solves this exactly: increment it once per step, have each fan-in goroutine call `Done` when its source channel closes, and run one final goroutine that waits for the group and then closes the merged channel. The merged channel is buffered to one slot per step so the non-blocking sends inside the fan-in never drop an error under normal operation.

Create `operator.go`:

```go
// Package stream provides composable stream processing operators for
// building data transformation pipelines.
package stream

import (
	"context"
	"sync/atomic"
	"time"
)

// Record is the unit of data flowing through the pipeline.
type Record struct {
	Key       []byte
	Value     []byte
	Timestamp time.Time
	Metadata  map[string]string
}

// ErrorAction controls what an error handler does with a failed record.
type ErrorAction int

const (
	Skip ErrorAction = iota
	Retry
	Abort
)

// ErrorHandler decides what happens to a record whose transform failed.
type ErrorHandler func(err error, r Record) ErrorAction

// SkipOnError is the default: silently discard the record and continue.
func SkipOnError(_ error, _ Record) ErrorAction { return Skip }

// AbortOnError stops the pipeline on the first transform error.
func AbortOnError(_ error, _ Record) ErrorAction { return Abort }

// Metrics tracks per-operator counters using lock-free atomic integers.
type Metrics struct {
	In      atomic.Int64
	Out     atomic.Int64
	Dropped atomic.Int64
	Errors  atomic.Int64
}

type operatorConfig struct {
	buf   int
	onErr ErrorHandler
}

func defaultConfig() operatorConfig {
	return operatorConfig{buf: 16, onErr: SkipOnError}
}

// OperatorOption is a functional option for any operator constructor.
type OperatorOption func(*operatorConfig)

// WithBuffer sets the output channel buffer depth (default 16).
func WithBuffer(n int) OperatorOption {
	return func(c *operatorConfig) { c.buf = n }
}

// WithErrorHandler overrides the error handling strategy.
func WithErrorHandler(h ErrorHandler) OperatorOption {
	return func(c *operatorConfig) { c.onErr = h }
}

// Operator reads records, transforms them, and returns an output channel
// and an error channel it owns and closes.
type Operator interface {
	Process(ctx context.Context, in <-chan Record) (<-chan Record, <-chan error)
}
```

Create `map.go`:

```go
package stream

import (
	"context"
	"fmt"
)

// MapFn transforms a single record into a single record.
type MapFn func(Record) (Record, error)

// MapOperator applies fn to every input record, emitting the result.
type MapOperator struct {
	fn      MapFn
	cfg     operatorConfig
	metrics *Metrics
}

// NewMap returns a MapOperator that applies fn to every record.
func NewMap(fn MapFn, opts ...OperatorOption) *MapOperator {
	cfg := defaultConfig()
	for _, o := range opts {
		o(&cfg)
	}
	return &MapOperator{fn: fn, cfg: cfg, metrics: &Metrics{}}
}

// Fn returns the underlying transform, used by the pipeline for fusion.
func (m *MapOperator) Fn() MapFn { return m.fn }

// Metrics returns the operator's live counters.
func (m *MapOperator) Metrics() *Metrics { return m.metrics }

// Process implements Operator.
func (m *MapOperator) Process(ctx context.Context, in <-chan Record) (<-chan Record, <-chan error) {
	out := make(chan Record, m.cfg.buf)
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
				m.metrics.In.Add(1)
				res, err := m.fn(r)
				if err != nil {
					m.metrics.Errors.Add(1)
					switch m.cfg.onErr(err, r) {
					case Skip:
						m.metrics.Dropped.Add(1)
						continue
					case Retry:
						res, err = m.fn(r)
						if err != nil {
							m.metrics.Dropped.Add(1)
							continue
						}
					case Abort:
						select {
						case errc <- fmt.Errorf("map operator: %w", err):
						default:
						}
						return
					}
				}
				select {
				case out <- res:
					m.metrics.Out.Add(1)
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
```

Create `filter.go`:

```go
package stream

import (
	"context"
	"fmt"
)

// FilterFn returns true if the record should be forwarded downstream.
type FilterFn func(Record) (bool, error)

// FilterOperator passes only records for which fn returns true.
type FilterOperator struct {
	fn      FilterFn
	cfg     operatorConfig
	metrics *Metrics
}

// NewFilter returns a FilterOperator.
func NewFilter(fn FilterFn, opts ...OperatorOption) *FilterOperator {
	cfg := defaultConfig()
	for _, o := range opts {
		o(&cfg)
	}
	return &FilterOperator{fn: fn, cfg: cfg, metrics: &Metrics{}}
}

// Metrics returns the operator's live counters.
func (f *FilterOperator) Metrics() *Metrics { return f.metrics }

// Process implements Operator.
func (f *FilterOperator) Process(ctx context.Context, in <-chan Record) (<-chan Record, <-chan error) {
	out := make(chan Record, f.cfg.buf)
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
				f.metrics.In.Add(1)
				keep, err := f.fn(r)
				if err != nil {
					f.metrics.Errors.Add(1)
					switch f.cfg.onErr(err, r) {
					case Skip:
						f.metrics.Dropped.Add(1)
						continue
					case Retry:
						keep, err = f.fn(r)
						if err != nil {
							f.metrics.Dropped.Add(1)
							continue
						}
					case Abort:
						select {
						case errc <- fmt.Errorf("filter operator: %w", err):
						default:
						}
						return
					}
				}
				if !keep {
					f.metrics.Dropped.Add(1)
					continue
				}
				select {
				case out <- r:
					f.metrics.Out.Add(1)
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
```

Create `flatmap.go`:

```go
package stream

import (
	"context"
	"fmt"
)

// FlatMapFn transforms one input record into zero or more output records.
type FlatMapFn func(Record) ([]Record, error)

// FlatMapOperator expands each input record into zero or more records.
type FlatMapOperator struct {
	fn      FlatMapFn
	cfg     operatorConfig
	metrics *Metrics
}

// NewFlatMap returns a FlatMapOperator.
func NewFlatMap(fn FlatMapFn, opts ...OperatorOption) *FlatMapOperator {
	cfg := defaultConfig()
	for _, o := range opts {
		o(&cfg)
	}
	return &FlatMapOperator{fn: fn, cfg: cfg, metrics: &Metrics{}}
}

// Metrics returns the operator's live counters.
func (fm *FlatMapOperator) Metrics() *Metrics { return fm.metrics }

// Process implements Operator.
func (fm *FlatMapOperator) Process(ctx context.Context, in <-chan Record) (<-chan Record, <-chan error) {
	out := make(chan Record, fm.cfg.buf)
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
				fm.metrics.In.Add(1)
				results, err := fm.fn(r)
				if err != nil {
					fm.metrics.Errors.Add(1)
					switch fm.cfg.onErr(err, r) {
					case Skip:
						fm.metrics.Dropped.Add(1)
						continue
					case Retry:
						results, err = fm.fn(r)
						if err != nil {
							fm.metrics.Dropped.Add(1)
							continue
						}
					case Abort:
						select {
						case errc <- fmt.Errorf("flatmap operator: %w", err):
						default:
						}
						return
					}
				}
				if len(results) == 0 {
					fm.metrics.Dropped.Add(1)
				}
				for _, outRec := range results {
					select {
					case out <- outRec:
						fm.metrics.Out.Add(1)
					case <-ctx.Done():
						return
					}
				}
			case <-ctx.Done():
				return
			}
		}
	}()
	return out, errc
}
```

Create `pipeline.go`:

```go
package stream

import (
	"context"
	"sync"
)

// Pipeline is a builder that chains operators in a linear topology.
// Call Map, Filter, and FlatMap to add steps, then Build to get a
// composite Operator.  Adjacent MapOperators are fused automatically
// during Build to eliminate intermediate channels and goroutines.
type Pipeline struct {
	ops []Operator
}

// NewPipeline returns an empty pipeline builder.
func NewPipeline() *Pipeline { return &Pipeline{} }

// Map appends a MapOperator to the pipeline.
func (p *Pipeline) Map(fn MapFn, opts ...OperatorOption) *Pipeline {
	p.ops = append(p.ops, NewMap(fn, opts...))
	return p
}

// Filter appends a FilterOperator to the pipeline.
func (p *Pipeline) Filter(fn FilterFn, opts ...OperatorOption) *Pipeline {
	p.ops = append(p.ops, NewFilter(fn, opts...))
	return p
}

// FlatMap appends a FlatMapOperator to the pipeline.
func (p *Pipeline) FlatMap(fn FlatMapFn, opts ...OperatorOption) *Pipeline {
	p.ops = append(p.ops, NewFlatMap(fn, opts...))
	return p
}

// fuse detects adjacent *MapOperator pairs and merges them into one.
// The inner function (the first in the chain) runs before the outer.
// Non-map operators are left untouched; the algorithm makes a single
// left-to-right pass so three adjacent maps produce two maps, not one.
func fuse(ops []Operator) []Operator {
	if len(ops) < 2 {
		return ops
	}
	out := make([]Operator, 0, len(ops))
	i := 0
	for i < len(ops) {
		m1, ok1 := ops[i].(*MapOperator)
		if ok1 && i+1 < len(ops) {
			if m2, ok2 := ops[i+1].(*MapOperator); ok2 {
				inner := m1.Fn()
				outer := m2.Fn()
				fused := NewMap(func(r Record) (Record, error) {
					r2, err := inner(r)
					if err != nil {
						return Record{}, err
					}
					return outer(r2)
				})
				out = append(out, fused)
				i += 2
				continue
			}
		}
		out = append(out, ops[i])
		i++
	}
	return out
}

// Build returns an Operator that represents the full pipeline after
// fusion.  Calling Build does not modify the Pipeline; it may be called
// multiple times to build independent composite operators.
func (p *Pipeline) Build() Operator {
	return &compositeOperator{steps: fuse(p.ops)}
}

// compositeOperator wires the fused steps in sequence.
type compositeOperator struct {
	steps []Operator
}

// Process implements Operator.  Each step's output channel becomes the
// next step's input.  Per-step error channels are fan-in merged into a
// single channel that is closed once every fan-in goroutine has finished.
func (c *compositeOperator) Process(ctx context.Context, in <-chan Record) (<-chan Record, <-chan error) {
	mergedErrc := make(chan error, len(c.steps)+1)
	var wg sync.WaitGroup
	cur := in
	for _, op := range c.steps {
		var errc <-chan error
		cur, errc = op.Process(ctx, cur)
		wg.Add(1)
		go func(ec <-chan error) {
			defer wg.Done()
			for err := range ec {
				select {
				case mergedErrc <- err:
				default:
				}
			}
		}(errc)
	}
	go func() {
		wg.Wait()
		close(mergedErrc)
	}()
	return cur, mergedErrc
}
```

The final goroutine that calls `wg.Wait()` then `close(mergedErrc)` is the whole fix: the merged channel outlives every per-step fan-in goroutine and is closed exactly once, after the last one returns, so a caller can safely `for err := range mergedErrc` to completion.

### The runnable demo

The demo builds the full three-stage pipeline: filter sentences by length, upper-case them, then split them into words. The Filter and Map are not adjacent maps so nothing fuses here, but the same `Build` call would fuse two adjacent `Map` steps transparently.

Create `cmd/demo/main.go`:

```go
// Command demo shows the stream pipeline API end to end.
package main

import (
	"context"
	"fmt"
	"log"
	"strings"
	"time"

	stream "example.com/pipeline-fusion"
)

func main() {
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	sentences := []string{
		"the quick brown fox",
		"go",
		"jumps over the lazy dog",
		"stream processing is fun",
		"ok",
	}
	in := make(chan stream.Record, len(sentences))
	for _, s := range sentences {
		in <- stream.Record{Key: []byte("demo"), Value: []byte(s), Timestamp: time.Now()}
	}
	close(in)

	p := stream.NewPipeline().
		Filter(func(r stream.Record) (bool, error) {
			return len(r.Value) > 4, nil
		}).
		Map(func(r stream.Record) (stream.Record, error) {
			r.Value = []byte(strings.ToUpper(string(r.Value)))
			return r, nil
		}).
		FlatMap(func(r stream.Record) ([]stream.Record, error) {
			words := strings.Fields(string(r.Value))
			out := make([]stream.Record, len(words))
			for i, w := range words {
				out[i] = stream.Record{Key: r.Key, Value: []byte(w), Timestamp: r.Timestamp}
			}
			return out, nil
		}).
		Build()

	out, errc := p.Process(ctx, in)
	go func() {
		for err := range errc {
			log.Printf("pipeline error: %v", err)
		}
	}()

	count := 0
	for r := range out {
		fmt.Printf("word: %s\n", r.Value)
		count++
	}
	fmt.Printf("total words emitted: %d\n", count)
}
```

Run it:

```bash
go run ./cmd/demo
```

Expected output:

```
word: THE
word: QUICK
word: BROWN
word: FOX
word: JUMPS
word: OVER
word: THE
word: LAZY
word: DOG
word: STREAM
word: PROCESSING
word: IS
word: FUN
total words emitted: 13
```

### Tests

`TestPipelineChains` confirms Map→Filter composes (an upper-cased "go" becomes the two-character "GO" and is filtered out). `TestPipelineFlatMapExpands` checks expansion through the builder. `TestOperatorFusionMergesAdjacentMaps` proves two adjacent maps fuse into one operator that applies both functions. `TestFusionDoesNotMergeNonAdjacentMaps` proves a filter between two maps blocks fusion. `TestPipelineErrorChannelCloses` drains a pipeline and asserts the merged error channel is closed afterwards, the property the WaitGroup guarantees. `ExampleNewPipeline` documents the builder API with verified output.

Create `operators_test.go`:

```go
package stream

import (
	"context"
	"fmt"
	"strings"
	"testing"
	"time"
)

func feed(records []Record) <-chan Record {
	ch := make(chan Record, len(records)+1)
	for _, r := range records {
		ch <- r
	}
	close(ch)
	return ch
}

func collect(out <-chan Record) []Record {
	var results []Record
	for r := range out {
		results = append(results, r)
	}
	return results
}

func rec(value string) Record {
	return Record{Key: []byte("k"), Value: []byte(value), Timestamp: time.Now()}
}

// TestPipelineChains verifies that Map -> Filter composes correctly.
func TestPipelineChains(t *testing.T) {
	t.Parallel()

	upper := func(r Record) (Record, error) {
		r.Value = []byte(strings.ToUpper(string(r.Value)))
		return r, nil
	}
	longOnly := func(r Record) (bool, error) { return len(r.Value) > 3, nil }

	p := NewPipeline().Map(upper).Filter(longOnly).Build()
	in := feed([]Record{rec("hello world"), rec("go"), rec("stream")})
	out, _ := p.Process(context.Background(), in)
	got := collect(out)

	// "go" uppercased is "GO" (2 chars) -- filtered out.
	if len(got) != 2 {
		t.Fatalf("got %d records, want 2: %v", len(got), got)
	}
	if string(got[0].Value) != "HELLO WORLD" {
		t.Errorf("got[0] = %q, want %q", got[0].Value, "HELLO WORLD")
	}
	if string(got[1].Value) != "STREAM" {
		t.Errorf("got[1] = %q, want %q", got[1].Value, "STREAM")
	}
}

// TestPipelineFlatMapExpands verifies that FlatMap within a pipeline expands records.
func TestPipelineFlatMapExpands(t *testing.T) {
	t.Parallel()

	splitWords := func(r Record) ([]Record, error) {
		words := strings.Fields(string(r.Value))
		out := make([]Record, len(words))
		for i, w := range words {
			out[i] = rec(w)
		}
		return out, nil
	}

	p := NewPipeline().FlatMap(splitWords).Build()
	in := feed([]Record{rec("a b"), rec("c d e")})
	out, _ := p.Process(context.Background(), in)
	got := collect(out)

	if len(got) != 5 {
		t.Fatalf("got %d records, want 5", len(got))
	}
}

// TestOperatorFusionMergesAdjacentMaps verifies that fuse combines two
// adjacent MapOperators into one.
func TestOperatorFusionMergesAdjacentMaps(t *testing.T) {
	t.Parallel()

	addA := func(r Record) (Record, error) {
		r.Value = append(r.Value, 'A')
		return r, nil
	}
	addB := func(r Record) (Record, error) {
		r.Value = append(r.Value, 'B')
		return r, nil
	}

	ops := []Operator{NewMap(addA), NewMap(addB)}
	fused := fuse(ops)

	if len(fused) != 1 {
		t.Fatalf("fuse returned %d ops, want 1", len(fused))
	}

	out, _ := fused[0].Process(context.Background(), feed([]Record{rec("x")}))
	got := collect(out)

	if len(got) != 1 || string(got[0].Value) != "xAB" {
		t.Fatalf("fused output = %q, want %q", got[0].Value, "xAB")
	}
}

// TestFusionDoesNotMergeNonAdjacentMaps verifies that a filter between two
// maps prevents fusion.
func TestFusionDoesNotMergeNonAdjacentMaps(t *testing.T) {
	t.Parallel()

	ops := []Operator{
		NewMap(func(r Record) (Record, error) { return r, nil }),
		NewFilter(func(r Record) (bool, error) { return true, nil }),
		NewMap(func(r Record) (Record, error) { return r, nil }),
	}
	fused := fuse(ops)
	if len(fused) != 3 {
		t.Fatalf("fuse returned %d ops, want 3 (filter breaks adjacency)", len(fused))
	}
}

// TestPipelineErrorChannelCloses verifies the merged error channel is closed
// after the stream drains, so a caller ranging over it terminates.
func TestPipelineErrorChannelCloses(t *testing.T) {
	t.Parallel()

	p := NewPipeline().
		Map(func(r Record) (Record, error) { return r, nil }).
		Filter(func(r Record) (bool, error) { return true, nil }).
		Build()
	out, errc := p.Process(context.Background(), feed([]Record{rec("a"), rec("b")}))
	collect(out)

	done := make(chan struct{})
	go func() {
		for range errc {
		}
		close(done)
	}()
	select {
	case <-done:
		// merged error channel closed as expected
	case <-time.After(2 * time.Second):
		t.Fatal("merged error channel was not closed within 2s")
	}
}

// ExampleNewPipeline demonstrates the pipeline builder API.
func ExampleNewPipeline() {
	ctx := context.Background()

	in := make(chan Record, 3)
	in <- Record{Value: []byte("apple")}
	in <- Record{Value: []byte("go")}
	in <- Record{Value: []byte("banana")}
	close(in)

	p := NewPipeline().
		Filter(func(r Record) (bool, error) {
			return len(r.Value) > 3, nil
		}).
		Map(func(r Record) (Record, error) {
			r.Value = []byte(strings.ToUpper(string(r.Value)))
			return r, nil
		}).
		Build()

	out, errc := p.Process(ctx, in)
	go func() {
		for range errc {
		}
	}()
	for r := range out {
		fmt.Println(string(r.Value))
	}
	// Output:
	// APPLE
	// BANANA
}
```

## Review

The builder is correct when fusion preserves order and semantics: `outer(inner(r))` must apply the first map before the second, and an inner error must short-circuit before the outer runs. The classic single-pass mistake is collapsing three adjacent maps into one in a single sweep; the index-by-two advance deliberately produces two operators instead, which keeps the algorithm simple and its output predictable. The error-channel bug is the one this exercise fixes: closing `mergedErrc` from inside a fan-in goroutine races with the others and can close it while a sibling still holds an error to send, so the close is deferred to a single goroutine guarded by `sync.WaitGroup`. Run with `-race`; the fan-in goroutines and the metrics counters are all shared state.

## Resources

- [Apache Flink: task chaining and resource groups](https://nightlies.apache.org/flink/flink-docs-stable/docs/dev/datastream/operators/overview/#task-chaining-and-resource-groups) — the production design operator fusion mirrors.
- [sync.WaitGroup](https://pkg.go.dev/sync#WaitGroup) — the primitive that closes the merged error channel exactly once.
- [Go blog: Pipelines and cancellation](https://go.dev/blog/pipelines) — the fan-in merge pattern used by the composite operator.

---

Prev: [02-filter-and-flatmap.md](02-filter-and-flatmap.md) | Next: [04-stateful-scan.md](04-stateful-scan.md)
