# Exercise 2: Filter and FlatMap

Map keeps the record count constant. The other two stateless primitives change it: Filter drops records that fail a predicate, and FlatMap expands one record into zero, one, or many. This exercise builds both on the same operator interface, so they slot into a pipeline interchangeably with Map.

This module is fully self-contained. It bundles its own copy of the operator core (`Record`, `ErrorAction`, `Metrics`, the config options, the `Operator` interface), defines `FilterOperator` and `FlatMapOperator`, and ships its own demo and tests. Nothing here imports any other exercise.

## What you'll build

```text
operator.go            Record, ErrorAction, ErrorHandler, Metrics, operatorConfig, Operator
filter.go              FilterFn, FilterOperator, NewFilter, Process
flatmap.go             FlatMapFn, FlatMapOperator, NewFlatMap, Process (per-element ctx check)
cmd/
  demo/
    main.go            filter then flat-map a few sentences into words
operators_test.go      predicate selection, one-to-many expansion, filter abort path
```

- Files: `operator.go`, `filter.go`, `flatmap.go`, `cmd/demo/main.go`, `operators_test.go`.
- Implement: `FilterOperator` (forward records whose predicate returns true) and `FlatMapOperator` (emit each element of the returned slice, checking the context between sends).
- Test: `operators_test.go` checks predicate-based selection over a table, one-to-zero-to-many expansion, and that a filter built with `AbortOnError` propagates a wrapped sentinel on its error channel.
- Verify: `go test -race ./...`

Set up the module:

```bash
go mod edit -go=1.26
```

### Why FlatMap checks the context between every element

Filter is the simplest of the three operators: it runs the predicate, and on `false` it drops the record (incrementing `Dropped`) and continues; on `true` it forwards. Its error handling mirrors Map exactly — Skip, Retry, Abort — because a predicate can fail (for example, parsing a field that turns out to be malformed), and that failure deserves the same policy choice as a transform failure.

FlatMap is where a subtle correctness issue appears. One input record can produce a large slice of output records, and each one is a separate blocking send. The naive loop `for _, r := range results { out <- r }` cannot be interrupted: if the context is cancelled while a single input record is in the middle of emitting ten thousand outputs, the operator keeps trying to send them all, and if the downstream consumer has already stopped reading, the goroutine parks on a send forever and leaks. The fix is to wrap every inner send in the same `select { case out <- r: case <-ctx.Done(): return }` used for the outer send. Cancellation is then honoured between individual elements, not only between input records, so a cancelled pipeline drains promptly no matter how large a single record's expansion is.

A second FlatMap detail: a transform that returns an empty or nil slice is valid and means "drop this input". The operator counts that as one `Dropped` record, so the metrics distinguish a record that produced no output from one that produced several. Returning `[]Record{r}` is the identity expansion, and returning a multi-element slice is the one-to-many case that makes FlatMap the right tool for splitting, exploding, and joining records.

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
	// Skip discards the failing record and continues processing.
	Skip ErrorAction = iota
	// Retry re-applies the transform once without backoff.
	Retry
	// Abort closes the error channel and stops the operator goroutine.
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

// operatorConfig holds the settings shared by all operator types.
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

// Operator reads records from an input channel, applies a transformation,
// and returns a new output channel and an error channel.  The operator
// owns and closes both returned channels.
type Operator interface {
	Process(ctx context.Context, in <-chan Record) (<-chan Record, <-chan error)
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
// Returning false silently drops the record; returning an error triggers
// the operator's ErrorHandler.
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
					action := f.cfg.onErr(err, r)
					switch action {
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
// Returning an empty (or nil) slice is valid: the input record is dropped.
type FlatMapFn func(Record) ([]Record, error)

// FlatMapOperator expands each input record into zero or more records.
// It iterates over the returned slice and sends each element individually,
// checking for context cancellation between sends to allow clean shutdown.
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
					action := fm.cfg.onErr(err, r)
					switch action {
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

`FlatMapOperator` checks context cancellation between individual element sends, not only between input records. Without that inner `select`, a burst of ten thousand output records from one input record cannot be interrupted mid-flight.

### The runnable demo

The demo wires Filter into FlatMap by hand — Filter's output channel is FlatMap's input channel — to show the two operators composing without the pipeline builder from the next exercise. It keeps sentences longer than four characters, then splits each survivor into its words.

Create `cmd/demo/main.go`:

```go
// Command demo chains a FilterOperator into a FlatMapOperator by feeding
// the filter's output channel directly into the flat-map's input.
package main

import (
	"context"
	"fmt"
	"strings"
	"time"

	stream "example.com/filter-and-flatmap"
)

func main() {
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	sentences := []string{"go", "the quick fox", "hi", "stream wins"}
	in := make(chan stream.Record, len(sentences))
	for _, s := range sentences {
		in <- stream.Record{Value: []byte(s)}
	}
	close(in)

	filter := stream.NewFilter(func(r stream.Record) (bool, error) {
		return len(r.Value) > 4, nil
	})
	flat := stream.NewFlatMap(func(r stream.Record) ([]stream.Record, error) {
		words := strings.Fields(string(r.Value))
		out := make([]stream.Record, len(words))
		for i, w := range words {
			out[i] = stream.Record{Value: []byte(w)}
		}
		return out, nil
	})

	kept, e1 := filter.Process(ctx, in)
	words, e2 := flat.Process(ctx, kept)
	go func() {
		for range e1 {
		}
	}()
	go func() {
		for range e2 {
		}
	}()

	count := 0
	for r := range words {
		fmt.Printf("word: %s\n", r.Value)
		count++
	}
	fmt.Printf("total words: %d\n", count)
}
```

Run it:

```bash
go run ./cmd/demo
```

Expected output:

```
word: the
word: quick
word: fox
word: stream
word: wins
total words: 5
```

### Tests

`TestFilterOperator` walks a table covering keep-some, keep-none, and keep-all predicates. `TestFlatMapOperator` checks the three expansion shapes: one-to-many (split by comma), one-to-zero (drop), and one-to-one (passthrough). `TestFilterOperatorAbortsOnError` builds a filter with `AbortOnError`, feeds it a record whose predicate returns an error, and asserts the error channel receives a non-nil error wrapping the original sentinel.

Create `operators_test.go`:

```go
package stream

import (
	"context"
	"errors"
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

// TestFilterOperator verifies predicate-based record selection.
func TestFilterOperator(t *testing.T) {
	t.Parallel()

	cases := []struct {
		name  string
		input []string
		pred  FilterFn
		want  []string
	}{
		{
			name:  "keep long values",
			input: []string{"hi", "hello", "go", "world"},
			pred:  func(r Record) (bool, error) { return len(r.Value) > 3, nil },
			want:  []string{"hello", "world"},
		},
		{
			name:  "keep none",
			input: []string{"a", "b"},
			pred:  func(r Record) (bool, error) { return false, nil },
			want:  nil,
		},
		{
			name:  "keep all",
			input: []string{"x", "y"},
			pred:  func(r Record) (bool, error) { return true, nil },
			want:  []string{"x", "y"},
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			records := make([]Record, len(tc.input))
			for i, v := range tc.input {
				records[i] = rec(v)
			}
			op := NewFilter(tc.pred)
			out, _ := op.Process(context.Background(), feed(records))
			got := collect(out)
			if len(got) != len(tc.want) {
				t.Fatalf("len(got)=%d, want %d; values: %v", len(got), len(tc.want), got)
			}
			for i, r := range got {
				if string(r.Value) != tc.want[i] {
					t.Errorf("record[%d]: got %q, want %q", i, r.Value, tc.want[i])
				}
			}
		})
	}
}

// TestFlatMapOperator verifies that a single record can expand to zero or many.
func TestFlatMapOperator(t *testing.T) {
	t.Parallel()

	cases := []struct {
		name    string
		input   []string
		fn      FlatMapFn
		wantLen int
	}{
		{
			name:  "split by comma",
			input: []string{"a,b,c", "x,y"},
			fn: func(r Record) ([]Record, error) {
				parts := strings.Split(string(r.Value), ",")
				out := make([]Record, len(parts))
				for i, p := range parts {
					out[i] = rec(p)
				}
				return out, nil
			},
			wantLen: 5,
		},
		{
			name:    "emit nothing",
			input:   []string{"drop"},
			fn:      func(r Record) ([]Record, error) { return nil, nil },
			wantLen: 0,
		},
		{
			name:    "single passthrough",
			input:   []string{"pass"},
			fn:      func(r Record) ([]Record, error) { return []Record{r}, nil },
			wantLen: 1,
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			records := make([]Record, len(tc.input))
			for i, v := range tc.input {
				records[i] = rec(v)
			}
			op := NewFlatMap(tc.fn)
			out, _ := op.Process(context.Background(), feed(records))
			got := collect(out)
			if len(got) != tc.wantLen {
				t.Fatalf("got %d records, want %d", len(got), tc.wantLen)
			}
		})
	}
}

// TestFilterOperatorAbortsOnError verifies that a filter built with
// AbortOnError propagates a wrapped sentinel when the predicate fails.
func TestFilterOperatorAbortsOnError(t *testing.T) {
	t.Parallel()

	errPred := errors.New("predicate failure")
	pred := func(r Record) (bool, error) {
		if string(r.Value) == "boom" {
			return false, errPred
		}
		return true, nil
	}

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	op := NewFilter(pred, WithErrorHandler(AbortOnError))
	out, errc := op.Process(ctx, feed([]Record{rec("boom"), rec("ok")}))
	for range out {
	}

	var gotErr error
	for err := range errc {
		gotErr = err
	}
	if gotErr == nil {
		t.Fatal("expected error on errc, got nil")
	}
	if !errors.Is(gotErr, errPred) {
		t.Errorf("got %v, want to wrap errPred", gotErr)
	}
}
```

## Review

Filter is correct when `false` and an error are handled distinctly: `false` is a normal drop that increments `Dropped`, while an error runs the configured policy and may increment `Errors`. The expansion semantics of FlatMap are the place to be careful — an empty slice is a valid "drop" and must be counted, and `len(results) == 0` is the only signal of it. The bug the `-race` flag catches here is the same as for Map: the metrics are shared across goroutines. The bug that only manifests under cancellation is the missing inner `select` in FlatMap; without it, a cancelled context cannot interrupt a large expansion, and `TestContextCancellationShutsDown` from the previous exercise would hang if FlatMap were wired the same way.

## Resources

- [Go blog: Pipelines and cancellation](https://go.dev/blog/pipelines) — channel ownership and the close-from-the-producer rule both operators follow.
- [strings.Fields](https://pkg.go.dev/strings#Fields) — the word splitter the FlatMap demo uses.
- [Effective Go: Channels](https://go.dev/doc/effective_go#channels) — the buffered-channel and select idioms underpinning backpressure.

---

Prev: [01-operator-core-and-map.md](01-operator-core-and-map.md) | Next: [03-pipeline-fusion.md](03-pipeline-fusion.md)
