# Exercise 5: Generator Source (Bounded vs Unbounded)

Not every source reads I/O. A generator produces records from a function — synthetic load, a test fixture, a counter — and it is the cleanest way to see the line between a bounded source that ends on its own and an unbounded one that runs until you stop it. The same lifecycle contract serves both; the only difference is whether the producing goroutine ever returns voluntarily.

Every module in this lesson is fully self-contained: it begins with its own `go mod init`, bundles the shared `Record`, `Metrics`, and `Source` definitions it needs, and ships its own demo and tests. Nothing here imports any other exercise.

## What you'll build

```text
generator-source/
  go.mod
  source.go                  Record, Metrics, Source, ErrSourceClosed
  generator_source.go        GenFunc, GeneratorSource: bounded + unbounded
  generator_source_test.go   bounded completes, unbounded runs until Close
  cmd/demo/main.go           a bounded generator that closes its own channel
```

- Files: `source.go`, `generator_source.go`, `generator_source_test.go`, `cmd/demo/main.go`.
- Implement: `GeneratorSource` driven by a `GenFunc func(seq int64) (Record, bool)`; `ok == false` ends a bounded source, `ok == true` forever makes it unbounded.
- Test: a bounded generator emits exactly N and closes its channel without a `Close` call; an unbounded one keeps emitting until `Close`.
- Verify: `go test -race ./...`

Set up the module:

```bash
mkdir -p generator-source/cmd/demo && cd generator-source
go mod init example.com/generator-source
```

### The shared vocabulary

`source.go` bundles the same `Record`, `Metrics`, and `Source` types. A generator needs no error channel for I/O failures, but it still returns one to satisfy the interface, kept small and always drained-or-buffered.

Create `source.go`:

```go
package generatorsource

import (
	"context"
	"errors"
	"time"
)

// Record is the atomic unit flowing through the pipeline.
type Record struct {
	Key       []byte
	Value     []byte
	Timestamp time.Time
	Source    string
	Metadata  map[string]string
}

// Metrics is a point-in-time snapshot of a source's counters.
type Metrics struct {
	RecordsEmitted int64
	BytesRead      int64
	ErrorsTotal    int64
	BacklogSize    int64
}

// ErrSourceClosed is returned by Close when the source was never opened.
var ErrSourceClosed = errors.New("generatorsource: source not open")

// Source is the common interface for all data origins.
type Source interface {
	Open(ctx context.Context) (<-chan Record, <-chan error)
	Close() error
	Metrics() Metrics
}
```

### Bounded and unbounded are one mechanism

The whole design rests on the `GenFunc` signature: `func(seq int64) (Record, bool)`. The source calls it with a strictly increasing sequence number; the boolean is the source's lifetime in disguise. A generator that returns `false` once `seq` reaches some limit is *bounded* — when the producing goroutine sees `ok == false` it simply `return`s, the `WaitGroup` drains, and the closer goroutine closes the record channel, so a downstream `range` terminates with no `Close` call ever made. A generator that returns `true` forever is *unbounded* — the goroutine never returns voluntarily, and the only thing that stops it is the `ctx.Done()` arm of its send `select`, reached when `Close` cancels the internal context.

This is the conceptual payoff: "bounded" and "unbounded" are not two code paths, they are two behaviours of the same loop, selected entirely by what the generator returns. The optional `interval` adds a `time.Ticker` so an unbounded generator can pace itself (a heartbeat, a synthetic load at N/sec) without a busy loop; with `interval == 0` it produces as fast as the consumer drains. Every send is the now-familiar context-guarded blocking send, so even a 0-interval unbounded source shuts down promptly on `Close`.

Create `generator_source.go`:

```go
package generatorsource

import (
	"context"
	"sync"
	"sync/atomic"
	"time"
)

// GenFunc produces the record for sequence number seq. It returns ok=false to
// signal that the generator is exhausted: a bounded source stops and closes its
// channel, an unbounded source returns ok=true forever.
type GenFunc func(seq int64) (Record, bool)

// GeneratorSource emits records produced by a GenFunc. With a GenFunc that
// eventually returns false it is a bounded source that completes on its own;
// with one that always returns true it is an unbounded source that runs until
// Close or context cancellation.
type GeneratorSource struct {
	gen        GenFunc
	interval   time.Duration
	bufferSize int

	cancel  context.CancelFunc
	wg      sync.WaitGroup
	records chan Record
	errs    chan error

	emitted atomic.Int64
	bytes   atomic.Int64
}

// NewGeneratorSource builds a source driven by gen. interval is the pause
// between records (0 for as-fast-as-possible); bufferSize is the output
// channel capacity.
func NewGeneratorSource(gen GenFunc, interval time.Duration, bufferSize int) *GeneratorSource {
	return &GeneratorSource{gen: gen, interval: interval, bufferSize: bufferSize}
}

func (gs *GeneratorSource) Open(ctx context.Context) (<-chan Record, <-chan error) {
	inner, cancel := context.WithCancel(ctx)
	gs.cancel = cancel
	gs.records = make(chan Record, gs.bufferSize)
	gs.errs = make(chan error, 4)

	gs.wg.Add(1)
	go gs.run(inner)

	go func() {
		gs.wg.Wait()
		close(gs.records)
		close(gs.errs)
	}()

	return gs.records, gs.errs
}

func (gs *GeneratorSource) run(ctx context.Context) {
	defer gs.wg.Done()

	var ticker *time.Ticker
	if gs.interval > 0 {
		ticker = time.NewTicker(gs.interval)
		defer ticker.Stop()
	}

	for seq := int64(0); ; seq++ {
		rec, ok := gs.gen(seq)
		if !ok {
			return // bounded source exhausted: channel closes cleanly
		}
		gs.bytes.Add(int64(len(rec.Value)))
		select {
		case gs.records <- rec:
			gs.emitted.Add(1)
		case <-ctx.Done():
			return
		}
		if ticker != nil {
			select {
			case <-ticker.C:
			case <-ctx.Done():
				return
			}
		}
	}
}

func (gs *GeneratorSource) Close() error {
	if gs.cancel == nil {
		return ErrSourceClosed
	}
	gs.cancel()
	gs.wg.Wait()
	return nil
}

func (gs *GeneratorSource) Metrics() Metrics {
	return Metrics{
		RecordsEmitted: gs.emitted.Load(),
		BytesRead:      gs.bytes.Load(),
	}
}

// compile-time check that GeneratorSource satisfies Source.
var _ Source = (*GeneratorSource)(nil)
```

### The runnable demo

The demo wires a bounded generator that yields three `event-N` records and then returns `ok == false`. Because the source is bounded, ranging over its channel terminates on its own — the demo never calls `Close` to stop the data, only observes the channel closing.

Create `cmd/demo/main.go`:

```go
package main

import (
	"context"
	"fmt"

	gen "example.com/generator-source"
)

func main() {
	// A bounded generator: three records, then ok=false closes the channel.
	count := 0
	g := func(seq int64) (gen.Record, bool) {
		if seq >= 3 {
			return gen.Record{}, false
		}
		count++
		return gen.Record{Value: []byte(fmt.Sprintf("event-%d", seq)), Source: "generator"}, true
	}

	gs := gen.NewGeneratorSource(g, 0, 8)
	recs, _ := gs.Open(context.Background())

	for r := range recs {
		fmt.Printf("generated: %s\n", r.Value)
	}
	fmt.Println("channel closed (bounded source exhausted)")
	fmt.Printf("emitted=%d\n", gs.Metrics().RecordsEmitted)
}
```

Run it:

```bash
go run ./cmd/demo
```

Expected output:

```
generated: event-0
generated: event-1
generated: event-2
channel closed (bounded source exhausted)
emitted=3
```

### Tests

`TestBoundedSourceCompletes` asserts a 5-record generator emits exactly five values in order and that the channel is already closed afterward (a second receive returns `ok == false`) — proving the source self-terminates. `TestUnboundedSourceRunsUntilClose` runs an always-`true` generator, reads a few records, then calls `Close` and drains, proving the source keeps producing until cancelled and then shuts down cleanly. `TestCloseBeforeOpen` asserts the sentinel.

Create `generator_source_test.go`:

```go
package generatorsource

import (
	"context"
	"fmt"
	"sync/atomic"
	"testing"
	"time"
)

func collect(ch <-chan Record, max int, timeout time.Duration) []Record {
	var out []Record
	deadline := time.After(timeout)
	for {
		select {
		case r, ok := <-ch:
			if !ok {
				return out
			}
			out = append(out, r)
			if len(out) >= max {
				return out
			}
		case <-deadline:
			return out
		}
	}
}

// TestBoundedSourceCompletes verifies a bounded generator emits exactly N
// records and then closes its channel without a Close call.
func TestBoundedSourceCompletes(t *testing.T) {
	t.Parallel()

	const n = 5
	gen := func(seq int64) (Record, bool) {
		if seq >= n {
			return Record{}, false
		}
		return Record{Value: []byte(fmt.Sprintf("event-%d", seq)), Source: "gen"}, true
	}
	gs := NewGeneratorSource(gen, 0, 8)

	recs, _ := gs.Open(context.Background())
	got := collect(recs, 100, time.Second)
	if len(got) != n {
		t.Fatalf("got %d records, want %d", len(got), n)
	}
	for i, r := range got {
		want := fmt.Sprintf("event-%d", i)
		if string(r.Value) != want {
			t.Errorf("record[%d] = %q, want %q", i, r.Value, want)
		}
	}
	// Channel must already be closed; a second receive returns ok=false.
	if _, ok := <-recs; ok {
		t.Error("channel still open after bounded source finished")
	}
	if m := gs.Metrics(); m.RecordsEmitted != n {
		t.Errorf("RecordsEmitted = %d, want %d", m.RecordsEmitted, n)
	}
}

// TestUnboundedSourceRunsUntilClose verifies an unbounded generator keeps
// emitting until Close cancels it.
func TestUnboundedSourceRunsUntilClose(t *testing.T) {
	t.Parallel()

	var produced atomic.Int64
	gen := func(seq int64) (Record, bool) {
		produced.Add(1)
		return Record{Value: []byte("tick")}, true
	}
	gs := NewGeneratorSource(gen, time.Millisecond, 4)

	recs, _ := gs.Open(context.Background())
	got := collect(recs, 3, time.Second)
	if len(got) < 3 {
		t.Fatalf("got %d records, want >= 3", len(got))
	}
	if err := gs.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}
	// After Close the channel drains and closes.
	for range recs {
	}
}

// TestCloseBeforeOpen verifies the sentinel error.
func TestCloseBeforeOpen(t *testing.T) {
	t.Parallel()
	gs := NewGeneratorSource(func(int64) (Record, bool) { return Record{}, false }, 0, 1)
	if err := gs.Close(); err != ErrSourceClosed {
		t.Errorf("Close before Open = %v, want %v", err, ErrSourceClosed)
	}
}
```

## Review

The source is correct when the generator's boolean alone decides termination: returning `false` closes the channel with no `Close` call, returning `true` forever runs until `Close` cancels the context. Confirm the producing goroutine `return`s immediately on `ok == false` (so the closer can close the channel) and that every send and every inter-record pause has a `ctx.Done()` arm (so an unbounded source shuts down promptly). The common mistakes are leaking an unbounded generator by forgetting the `ctx.Done()` arm on the send or the ticker wait, and conflating "no more data" with "error" — an exhausted bounded generator is a normal end of stream, not a failure. The two tests together, under `-race`, cover both lifetimes.

## Resources

- [`time.Ticker`](https://pkg.go.dev/time#Ticker) — the pacing primitive behind a self-rate-limiting unbounded generator, and why you must `Stop` it.
- [Go blog: Pipelines and cancellation](https://go.dev/blog/pipelines) — generator-style sources and context-driven shutdown.
- [Effective Go: Channels](https://go.dev/doc/effective_go#channels) — closing a channel to signal "no more values," the basis of the bounded-source end of stream.

---

Back to [00-concepts.md](00-concepts.md) | Prev: [04-multi-source.md](04-multi-source.md) | Next: [06-replayable-source.md](06-replayable-source.md)
