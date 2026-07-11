# Exercise 1: The Operator Interface and Map

Every operator in a stream engine reads from one channel, transforms, and writes to another, and they all share one interface so any operator composes with any other. This exercise builds that foundation — the `Record` unit, the `Operator` interface, the Skip/Retry/Abort error policy, lock-free metrics — and the first concrete operator, Map, which applies a function to every record.

This module is fully self-contained. It begins with its own `go mod init`, defines every type the rest of the chapter reuses, and ships its own demo and tests. Nothing here imports any other exercise.

## What you'll build

```text
operator.go            Record, ErrorAction, ErrorHandler, Metrics, operatorConfig, Operator
map.go                 MapFn, MapOperator, NewMap, Process (backpressure + error routing)
cmd/
  demo/
    main.go            map a few records, print results and live metric counters
operators_test.go      transform table, skip/retry/abort paths, metrics, ctx shutdown
```

- Files: `operator.go`, `map.go`, `cmd/demo/main.go`, `operators_test.go`.
- Implement: the `Operator` interface, the shared `operatorConfig` with `WithBuffer`/`WithErrorHandler` options, the `Metrics` counters, and `MapOperator` with `NewMap` and `Process`.
- Test: `operators_test.go` checks the transform on a table of inputs, the default skip-on-error path, the abort path with `errors.Is`, a one-shot retry, the metric counters, and clean shutdown on context cancellation.
- Verify: `go test -race ./...`

Set up the module:

```bash
mkdir -p operator-core-and-map/cmd/demo && cd operator-core-and-map
go mod init example.com/operator-core-and-map
go mod edit -go=1.26
```

This is a library; you verify it with `go test`, not by running a server.

### Why one interface, two channels, and three error actions

The whole chapter rests on one decision: every operator has the signature `Process(ctx, in) (out, errc)`. Returning a fresh output channel rather than writing into a caller-supplied one means the operator owns that channel and is the only thing that closes it, which removes the classic "who closes the channel" ambiguity — the producer always closes, never the consumer. Returning a separate error channel instead of folding errors into `Record` keeps the data type clean: a downstream Filter receives `Record` values it can transform directly, never a sum type it must unwrap first. The two channels are independent, so a caller can drain records on the main goroutine and errors on a background goroutine without either blocking the other.

The transform can fail, and what happens next is policy, not mechanism, so it is injected at construction time. `ErrorHandler` returns `Skip` (drop the record and keep going — the default, because one bad record should not kill a pipeline), `Retry` (re-apply the function once, for transient failures), or `Abort` (report the error downstream and stop). Separating the policy from the operator means the same `MapOperator` code serves a best-effort enrichment pipeline (`SkipOnError`) and a strict validation pipeline (`AbortOnError`) with no change to the operator itself, only a different option passed to `NewMap`.

Backpressure is the reason every send is a blocking `select` on `out` and `ctx.Done()`. The output channel has a small buffer (16 by default) to absorb bursts, but once it fills the send blocks, the operator stops reading `in`, and the stall propagates upstream to the source — no record is ever dropped to relieve pressure. The only thing that unblocks a full send other than the consumer reading is context cancellation, which is how the operator shuts down promptly instead of leaking a goroutine parked on a send nobody will ever receive.

`Fn()` is exported on `MapOperator` so the pipeline builder in a later exercise can recognise the concrete type and pull out its function for fusion. Without an exported accessor, fusion would need reflection or an unexported method that the builder could not reach from another package; here both live in the same package, but the export documents the intent.

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
// Key and Value carry the payload; Metadata holds optional annotations.
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

// ErrorHandler is called when an operator transform returns an error.
// It decides what happens to the failing record and may cancel the pipeline.
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
// A buffer of 16 smooths micro-bursts without defeating backpressure.
func WithBuffer(n int) OperatorOption {
	return func(c *operatorConfig) { c.buf = n }
}

// WithErrorHandler overrides the error handling strategy.
func WithErrorHandler(h ErrorHandler) OperatorOption {
	return func(c *operatorConfig) { c.onErr = h }
}

// Operator reads records from an input channel, applies a transformation,
// and returns a new output channel and an error channel.
//
// The operator owns both returned channels and closes them when the input
// is exhausted or the context is cancelled.  Consumers must drain both
// channels to avoid goroutine leaks.
type Operator interface {
	Process(ctx context.Context, in <-chan Record) (<-chan Record, <-chan error)
}
```

The `operatorConfig` struct is an unexported shared config so all operator constructors accept the same `...OperatorOption` variadic without repeating field declarations. Defaults are set once in `defaultConfig`; options override them in order.

Create `map.go`:

```go
package stream

import (
	"context"
	"fmt"
)

// MapFn transforms a single record into a single record.
// Return an error to trigger the operator's ErrorHandler.
type MapFn func(Record) (Record, error)

// MapOperator applies fn to every input record, emitting the result.
// On error the configured ErrorHandler decides whether to skip, retry,
// or abort.
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

// Fn returns the underlying transform function, used by the pipeline
// builder for operator fusion.
func (m *MapOperator) Fn() MapFn { return m.fn }

// Metrics returns the operator's live counters.
func (m *MapOperator) Metrics() *Metrics { return m.metrics }

// Process implements Operator.  It spawns a goroutine that reads from in,
// applies fn, and sends results to out.  Blocking sends provide
// backpressure: a slow downstream consumer slows this operator, which in
// turn slows its upstream.
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
					action := m.cfg.onErr(err, r)
					switch action {
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

The read loop's outer `select` lets `ctx.Done()` interrupt a wait on an empty input channel, and the inner `select` around the send lets it interrupt a wait on a full output channel. Both are needed: without the first, a cancelled pipeline whose source has stalled never shuts down; without the second, a cancelled pipeline whose consumer has stopped reading never shuts down.

### The runnable demo

The demo maps three records to upper-case, deliberately making the transform fail on one of them so the default skip path and the metric counters are both visible. It drains the error channel in the background even though no error reaches it, because draining both channels is the contract every consumer must honour.

Create `cmd/demo/main.go`:

```go
// Command demo shows the MapOperator transforming records and the live
// metric counters after the stream drains.
package main

import (
	"context"
	"errors"
	"fmt"
	"strings"

	stream "example.com/operator-core-and-map"
)

func main() {
	in := make(chan stream.Record, 3)
	for _, w := range []string{"alpha", "skip", "gamma"} {
		in <- stream.Record{Value: []byte(w)}
	}
	close(in)

	// Upper-case every record, but fail on the literal "skip" so the
	// default SkipOnError policy drops it.
	op := stream.NewMap(func(r stream.Record) (stream.Record, error) {
		if string(r.Value) == "skip" {
			return stream.Record{}, errors.New("unprocessable record")
		}
		r.Value = []byte(strings.ToUpper(string(r.Value)))
		return r, nil
	})

	out, errc := op.Process(context.Background(), in)
	go func() {
		for range errc {
		}
	}()
	for r := range out {
		fmt.Println(string(r.Value))
	}

	m := op.Metrics()
	fmt.Printf("in=%d out=%d dropped=%d errors=%d\n",
		m.In.Load(), m.Out.Load(), m.Dropped.Load(), m.Errors.Load())
}
```

Run it:

```bash
go run ./cmd/demo
```

Expected output:

```
ALPHA
GAMMA
in=3 out=2 dropped=1 errors=1
```

### Tests

The tests cover the four behaviours the operator promises. `TestMapOperatorTransforms` runs a table including the empty-input case. `TestMapOperatorSkipsErroredRecords` checks the default policy drops a failing record and bumps `Errors` and `Dropped`. `TestMapOperatorAbortsOnError` uses `errors.Is` to confirm the abort path wraps the original sentinel. `TestMapOperatorRetry` proves Retry calls the function exactly twice. `TestContextCancellationShutsDown` feeds an unbuffered, never-closing input and asserts the operator still stops within two seconds of cancellation. `TestMetricsCountCorrectly` checks all four counters together.

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

// feed writes records to a buffered channel and closes it.
func feed(records []Record) <-chan Record {
	ch := make(chan Record, len(records)+1)
	for _, r := range records {
		ch <- r
	}
	close(ch)
	return ch
}

// collect drains a channel to a slice.
func collect(out <-chan Record) []Record {
	var results []Record
	for r := range out {
		results = append(results, r)
	}
	return results
}

// rec is a shorthand for creating test records.
func rec(value string) Record {
	return Record{
		Key:       []byte("k"),
		Value:     []byte(value),
		Timestamp: time.Now(),
	}
}

// TestMapOperatorTransforms verifies that MapOperator applies fn to every record.
func TestMapOperatorTransforms(t *testing.T) {
	t.Parallel()

	cases := []struct {
		name  string
		input []string
		fn    MapFn
		want  []string
	}{
		{
			name:  "uppercase",
			input: []string{"hello", "world"},
			fn: func(r Record) (Record, error) {
				r.Value = []byte(strings.ToUpper(string(r.Value)))
				return r, nil
			},
			want: []string{"HELLO", "WORLD"},
		},
		{
			name:  "empty input",
			input: []string{},
			fn:    func(r Record) (Record, error) { return r, nil },
			want:  nil,
		},
		{
			name:  "identity",
			input: []string{"a", "b", "c"},
			fn:    func(r Record) (Record, error) { return r, nil },
			want:  []string{"a", "b", "c"},
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			records := make([]Record, len(tc.input))
			for i, v := range tc.input {
				records[i] = rec(v)
			}
			op := NewMap(tc.fn)
			out, _ := op.Process(context.Background(), feed(records))
			got := collect(out)
			if len(got) != len(tc.want) {
				t.Fatalf("len(got)=%d, want %d", len(got), len(tc.want))
			}
			for i, r := range got {
				if string(r.Value) != tc.want[i] {
					t.Errorf("record[%d]: got %q, want %q", i, r.Value, tc.want[i])
				}
			}
		})
	}
}

// TestMapOperatorSkipsErroredRecords verifies the default SkipOnError behavior.
func TestMapOperatorSkipsErroredRecords(t *testing.T) {
	t.Parallel()

	errBad := errors.New("bad record")
	fn := func(r Record) (Record, error) {
		if string(r.Value) == "bad" {
			return Record{}, errBad
		}
		return r, nil
	}

	op := NewMap(fn) // default: SkipOnError
	out, _ := op.Process(context.Background(), feed([]Record{rec("good"), rec("bad"), rec("ok")}))
	got := collect(out)

	if len(got) != 2 {
		t.Fatalf("got %d records, want 2 (errored record skipped)", len(got))
	}
	if op.Metrics().Errors.Load() != 1 {
		t.Errorf("errors counter = %d, want 1", op.Metrics().Errors.Load())
	}
	if op.Metrics().Dropped.Load() != 1 {
		t.Errorf("dropped counter = %d, want 1", op.Metrics().Dropped.Load())
	}
}

// TestMapOperatorAbortsOnError verifies that AbortOnError propagates an error.
func TestMapOperatorAbortsOnError(t *testing.T) {
	t.Parallel()

	errBoom := errors.New("boom")
	fn := func(r Record) (Record, error) {
		if string(r.Value) == "boom" {
			return Record{}, errBoom
		}
		return r, nil
	}

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	op := NewMap(fn, WithErrorHandler(AbortOnError))
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
	if !errors.Is(gotErr, errBoom) {
		t.Errorf("got %v, want to wrap errBoom", gotErr)
	}
}

// TestMapOperatorRetry verifies that Retry re-applies fn once.
func TestMapOperatorRetry(t *testing.T) {
	t.Parallel()

	attempts := 0
	fn := func(r Record) (Record, error) {
		attempts++
		if attempts == 1 {
			return Record{}, errors.New("first attempt fails")
		}
		r.Value = []byte("retried")
		return r, nil
	}

	op := NewMap(fn, WithErrorHandler(func(_ error, _ Record) ErrorAction { return Retry }))
	out, _ := op.Process(context.Background(), feed([]Record{rec("x")}))
	got := collect(out)

	if len(got) != 1 || string(got[0].Value) != "retried" {
		t.Fatalf("got %v, want [{retried}]", got)
	}
	if attempts != 2 {
		t.Errorf("fn called %d times, want 2", attempts)
	}
}

// TestContextCancellationShutsDown verifies that cancelling the context
// causes an operator to close its output channel within 2 seconds.
func TestContextCancellationShutsDown(t *testing.T) {
	t.Parallel()

	ctx, cancel := context.WithCancel(context.Background())

	// Unbuffered, never-closing input: only the context can stop the operator.
	in := make(chan Record)
	op := NewMap(func(r Record) (Record, error) { return r, nil })
	out, _ := op.Process(ctx, in)

	cancel()

	done := make(chan struct{})
	go func() {
		for range out {
		}
		close(done)
	}()

	select {
	case <-done:
		// operator shut down cleanly
	case <-time.After(2 * time.Second):
		t.Fatal("operator did not shut down within 2s after context cancellation")
	}
}

// TestMetricsCountCorrectly verifies that In, Out, Dropped, and Errors are accurate.
func TestMetricsCountCorrectly(t *testing.T) {
	t.Parallel()

	errBad := errors.New("bad")
	fn := func(r Record) (Record, error) {
		if string(r.Value) == "drop" {
			return Record{}, errBad
		}
		return r, nil
	}

	op := NewMap(fn) // SkipOnError: drop bad records
	in := feed([]Record{rec("ok"), rec("drop"), rec("also ok")})
	out, _ := op.Process(context.Background(), in)
	collect(out)

	m := op.Metrics()
	if m.In.Load() != 3 {
		t.Errorf("In = %d, want 3", m.In.Load())
	}
	if m.Out.Load() != 2 {
		t.Errorf("Out = %d, want 2", m.Out.Load())
	}
	if m.Dropped.Load() != 1 {
		t.Errorf("Dropped = %d, want 1", m.Dropped.Load())
	}
	if m.Errors.Load() != 1 {
		t.Errorf("Errors = %d, want 1", m.Errors.Load())
	}
}
```

## Review

The operator is correct when it closes its output channel on every exit path — input exhausted, abort, or cancellation — which is why `defer close(out)` sits at the top of the goroutine rather than at the bottom of the loop. The most common bug is dropping records under load with a non-blocking `default` send; the blocking `select` on `out` and `ctx.Done()` is what trades a dropped record for backpressure. The second is forgetting that the error channel must be drained: `TestMapOperatorAbortsOnError` drains `out` first and only then ranges over `errc`, which works because the abort path uses a non-blocking send to a capacity-1 buffer that never deadlocks. Run the suite with `-race`: the `Metrics` counters are read by the test goroutine while the operator goroutine writes them, and `atomic.Int64` is the only thing making that safe.

## Resources

- [Go blog: Pipelines and cancellation](https://go.dev/blog/pipelines) — the canonical reference for channel-based fan-in, fan-out, and cancellation patterns used throughout this chapter.
- [sync/atomic](https://pkg.go.dev/sync/atomic) — `atomic.Int64`, used for the lock-free metric counters.
- [errors](https://pkg.go.dev/errors) — `errors.Is` and `%w` wrapping, the basis of the abort error path.
- [Go spec: Receive operator](https://go.dev/ref/spec#Receive_operator) — the `v, ok := <-ch` form that detects a closed channel, used in the read loop.

---

Prev: [00-concepts.md](00-concepts.md) | Next: [02-filter-and-flatmap.md](02-filter-and-flatmap.md)
