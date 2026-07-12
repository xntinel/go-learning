# Exercise 4: MultiSource Fan-In

A pipeline usually ingests from several origins at once. `MultiSource` merges an arbitrary set of child sources into one record channel using the fan-in pattern, treating every child as an opaque `Source` regardless of whether it is a file, a socket, or an in-memory generator.

Every module in this lesson is fully self-contained: it begins with its own `go mod init`, bundles the shared `Record`, `Metrics`, and `Source` definitions it needs, and ships its own demo and tests. Nothing here imports any other exercise.

## What you'll build

```text
multi-source/
  go.mod
  source.go             Record, Metrics, Source, ErrSourceClosed
  slice_source.go       SliceSource: a deterministic in-memory child source
  multi_source.go       MultiSource: one forwarder per child, single closer
  multi_source_test.go  merge two children, close-on-drain, close-before-open
  cmd/demo/main.go      merge two slice sources and print the union
```

- Files: `source.go`, `slice_source.go`, `multi_source.go`, `multi_source_test.go`, `cmd/demo/main.go`.
- Implement: `MultiSource` over `...Source`, plus a bundled `SliceSource` to act as a deterministic child.
- Test: records from both children arrive; the merged channel closes once all children drain; `Close` before `Open` returns the sentinel.
- Verify: `go test -race ./...`

Set up the module:

```bash
mkdir -p go-solutions/43-capstone-stream-processing-engine/01-source-connectors/04-multi-source/cmd/demo && cd go-solutions/43-capstone-stream-processing-engine/01-source-connectors/04-multi-source
```

### A deterministic child to merge

To exercise `MultiSource` without the timing nondeterminism of real sockets, this module bundles `SliceSource`: the minimal possible source, emitting a fixed slice of values and then closing its channel. It is the "channel source" reduced to essentials, and it still obeys the full lifecycle contract — internal context, `WaitGroup`, single closer goroutine — so it is a faithful stand-in for any real child.

Create `source.go`:

```go
package multisource

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
var ErrSourceClosed = errors.New("multisource: source not open")

// Source is the common interface for all data origins.
type Source interface {
	Open(ctx context.Context) (<-chan Record, <-chan error)
	Close() error
	Metrics() Metrics
}
```

Create `slice_source.go`:

```go
package multisource

import (
	"context"
	"sync"
	"sync/atomic"
	"time"
)

// SliceSource is a minimal in-memory Source used as a deterministic child for
// MultiSource: it emits a fixed slice of values, then closes its channel. It is
// the "channel source" pattern reduced to its essentials, with the same
// lifecycle contract every real source obeys.
type SliceSource struct {
	name   string
	values [][]byte

	cancel  context.CancelFunc
	wg      sync.WaitGroup
	records chan Record
	errs    chan error
	emitted atomic.Int64
}

// NewSliceSource builds a source that emits each value in values as a Record.
func NewSliceSource(name string, values ...[]byte) *SliceSource {
	return &SliceSource{name: name, values: values}
}

func (ss *SliceSource) Open(ctx context.Context) (<-chan Record, <-chan error) {
	inner, cancel := context.WithCancel(ctx)
	ss.cancel = cancel
	records := make(chan Record, len(ss.values))
	errs := make(chan error, 1)
	ss.records = records
	ss.errs = errs

	ss.wg.Add(1)
	go func() {
		defer ss.wg.Done()
		for _, v := range ss.values {
			r := Record{Value: append([]byte(nil), v...), Timestamp: time.Now().UTC(), Source: ss.name}
			select {
			case records <- r:
				ss.emitted.Add(1)
			case <-inner.Done():
				return
			}
		}
	}()

	go func() {
		ss.wg.Wait()
		close(records)
		close(errs)
	}()

	return records, errs
}

func (ss *SliceSource) Close() error {
	if ss.cancel == nil {
		return ErrSourceClosed
	}
	ss.cancel()
	ss.wg.Wait()
	return nil
}

func (ss *SliceSource) Metrics() Metrics {
	return Metrics{RecordsEmitted: ss.emitted.Load()}
}

var _ Source = (*SliceSource)(nil)
```

### One forwarder per child, one closer for the merge

`MultiSource.Open` opens every child with the shared internal context and, for each child, spawns two forwarding goroutines: one drains the child's record channel and re-sends each record onto the merged channel (guarded by `inner.Done()`), the other drains the child's error channel onto the merged error channel with a non-blocking send. Both are tracked by the multiplexer's `WaitGroup`.

The elegance of fan-in is in the termination logic. Each child closes its own record channel when it finishes, so each forwarder's `for r := range ch` loop ends naturally; when the last forwarder returns, the `WaitGroup` hits zero and the single closer goroutine closes the merged channel. A consumer ranging over the merged channel therefore sees one clean end-of-stream once *all* children are done, with no coordination needed beyond the contract every child already honours. `Close` cancels the internal context and also calls `Close` on each child, so an explicit shutdown propagates downward; the children stop, their channels close, the forwarders drain and return, and `wg.Wait()` completes.

`Metrics` aggregates: the merged emitted count plus each child's bytes and errors, so one call summarizes the whole ingest tree.

Create `multi_source.go`:

```go
package multisource

import (
	"context"
	"sync"
	"sync/atomic"
)

// MultiSource fans in records from multiple child sources into a single output channel.
type MultiSource struct {
	children []Source

	cancel  context.CancelFunc
	wg      sync.WaitGroup
	records chan Record
	errs    chan error

	emitted atomic.Int64
}

// NewMultiSource creates a MultiSource from the given child sources.
func NewMultiSource(children ...Source) *MultiSource {
	return &MultiSource{children: children}
}

func (ms *MultiSource) Open(ctx context.Context) (<-chan Record, <-chan error) {
	inner, cancel := context.WithCancel(ctx)
	ms.cancel = cancel

	ms.records = make(chan Record, 256)
	ms.errs = make(chan error, 64)

	for _, child := range ms.children {
		recs, errs := child.Open(inner)

		ms.wg.Add(1)
		go func(ch <-chan Record) {
			defer ms.wg.Done()
			for r := range ch {
				select {
				case ms.records <- r:
					ms.emitted.Add(1)
				case <-inner.Done():
					return
				}
			}
		}(recs)

		ms.wg.Add(1)
		go func(ch <-chan error) {
			defer ms.wg.Done()
			for e := range ch {
				select {
				case ms.errs <- e:
				default:
				}
			}
		}(errs)
	}

	go func() {
		ms.wg.Wait()
		close(ms.records)
		close(ms.errs)
	}()

	return ms.records, ms.errs
}

func (ms *MultiSource) Close() error {
	if ms.cancel == nil {
		return ErrSourceClosed
	}
	ms.cancel()
	for _, child := range ms.children {
		child.Close() //nolint:errcheck
	}
	ms.wg.Wait()
	return nil
}

func (ms *MultiSource) Metrics() Metrics {
	var m Metrics
	m.RecordsEmitted = ms.emitted.Load()
	for _, child := range ms.children {
		cm := child.Metrics()
		m.BytesRead += cm.BytesRead
		m.ErrorsTotal += cm.ErrorsTotal
	}
	return m
}

var _ Source = (*MultiSource)(nil)
```

### The runnable demo

The demo merges two slice sources — an "orders" stream and a "clicks" stream — drains the merged channel to exhaustion, and prints the union. Fan-in interleaving is inherently nondeterministic, so the demo sorts the collected values before printing to produce a stable, reproducible output.

Create `cmd/demo/main.go`:

```go
package main

import (
	"context"
	"fmt"
	"sort"

	ms "example.com/multi-source"
)

func main() {
	c1 := ms.NewSliceSource("orders", []byte("order-1"), []byte("order-2"))
	c2 := ms.NewSliceSource("clicks", []byte("click-1"), []byte("click-2"), []byte("click-3"))
	m := ms.NewMultiSource(c1, c2)

	recs, _ := m.Open(context.Background())

	var values []string
	for r := range recs {
		values = append(values, fmt.Sprintf("%s:%s", r.Source, r.Value))
	}
	// Fan-in order is non-deterministic; sort for a stable demo output.
	sort.Strings(values)
	for _, v := range values {
		fmt.Println(v)
	}
	m.Close()
	fmt.Printf("merged total=%d\n", m.Metrics().RecordsEmitted)
}
```

Run it:

```bash
go run ./cmd/demo
```

Expected output:

```
clicks:click-1
clicks:click-2
clicks:click-3
orders:order-1
orders:order-2
merged total=5
```

### Tests

`TestMultiSourceMergesChildren` merges a 2-record child and a 3-record child and asserts all five arrive with the correct per-source counts. `TestMultiSourceClosesWhenChildrenDrain` proves the termination property: with a single one-record child, after the record is read the merged channel must close on its own, so a second receive reports `ok == false`. `TestMultiSourceCloseBeforeOpen` asserts the sentinel.

Create `multi_source_test.go`:

```go
package multisource

import (
	"context"
	"testing"
	"time"
)

func drain(ch <-chan Record, max int, timeout time.Duration) []Record {
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

func TestMultiSourceMergesChildren(t *testing.T) {
	t.Parallel()

	c1 := NewSliceSource("src-1", []byte("a1"), []byte("a2"))
	c2 := NewSliceSource("src-2", []byte("b1"), []byte("b2"), []byte("b3"))
	ms := NewMultiSource(c1, c2)

	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()

	recs, _ := ms.Open(ctx)
	got := drain(recs, 5, 2*time.Second)
	if len(got) != 5 {
		t.Fatalf("got %d records, want 5", len(got))
	}

	bySource := map[string]int{}
	for _, r := range got {
		bySource[r.Source]++
	}
	if bySource["src-1"] != 2 || bySource["src-2"] != 3 {
		t.Errorf("per-source counts = %v, want src-1:2 src-2:3", bySource)
	}

	ms.Close()
	if m := ms.Metrics(); m.RecordsEmitted != 5 {
		t.Errorf("RecordsEmitted = %d, want 5", m.RecordsEmitted)
	}
}

func TestMultiSourceClosesWhenChildrenDrain(t *testing.T) {
	t.Parallel()

	c1 := NewSliceSource("only", []byte("x"))
	ms := NewMultiSource(c1)

	recs, _ := ms.Open(context.Background())
	got := drain(recs, 10, time.Second)
	if len(got) != 1 {
		t.Fatalf("got %d, want 1", len(got))
	}
	// All children exhausted: the merged channel must close on its own.
	if _, ok := <-recs; ok {
		t.Error("merged channel still open after all children drained")
	}
	ms.Close()
}

func TestMultiSourceCloseBeforeOpen(t *testing.T) {
	t.Parallel()
	ms := NewMultiSource()
	if err := ms.Close(); err != ErrSourceClosed {
		t.Errorf("Close = %v, want %v", err, ErrSourceClosed)
	}
}
```

## Review

The multiplexer is correct when the merged channel closes exactly once, after every child has drained, and when an explicit `Close` propagates to all children. The key insight is that fan-in needs no extra bookkeeping: each child's channel closing ends its forwarder, and the shared `WaitGroup` plus a single closer turn "all forwarders done" into "merged channel closed." The common mistakes are closing the merged channel from a forwarder instead of from the one post-`Wait` closer, forgetting to guard the re-send with `ctx.Done()` (so a slow consumer wedges shutdown), and forgetting to `Close` the children in `MultiSource.Close` (leaking their goroutines). The close-on-drain test under `-race` confirms the termination handshake.

## Resources

- [Go blog: Pipelines and cancellation](https://go.dev/blog/pipelines) — the canonical fan-in pattern, one forwarding goroutine per input closed after a `WaitGroup`.
- [`sync.WaitGroup`](https://pkg.go.dev/sync#WaitGroup) — the `Add`/`Done`/`Wait` contract behind the single-closer termination.
- [Go Concurrency Patterns (Pike)](https://go.dev/blog/io2013-talk-concurrency) — multiplexing several channels into one, the talk fan-in comes from.

---

Back to [00-concepts.md](00-concepts.md) | Prev: [03-http-source.md](03-http-source.md) | Next: [05-generator-source.md](05-generator-source.md)
