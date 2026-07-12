# Exercise 10: Choosing the Primitive — Semaphore vs Worker Pool vs errgroup

Three tools bound a fan-out, and they are not interchangeable. A per-item
goroutine gated by a semaphore caps concurrency but still spawns one goroutine per
item. A fixed worker pool bounds the goroutine *count* and gives natural
back-pressure. `errgroup.SetLimit` bounds concurrency and wires cancellation and
error propagation for free. This capstone implements one `ProcessBatch` task all
three ways behind a single interface, proves all three honor the cap and
cancellation, and benchmarks them so the trade-offs are concrete.

This module is fully self-contained: its own `go mod init`, all code inline, its
own demo, tests, and benchmarks. It depends on `golang.org/x/sync`, fetched by the
gate.

## What you'll build

```text
batch/                      independent module: example.com/batch
  go.mod                    go 1.26; requires golang.org/x/sync
  batch.go                  type Processor; semaphore, worker-pool, errgroup impls
  cmd/
    demo/
      main.go               run all three over one input, show they agree
  batch_test.go             identical output + peak<=cap + cancellation; benchmarks
```

- Files: `batch.go`, `cmd/demo/main.go`, `batch_test.go`.
- Implement: a `Processor` interface with three implementations — per-item goroutine + semaphore, fixed worker pool draining a jobs channel, and `errgroup.SetLimit` — each capping concurrency at `limit` and honoring `ctx` cancellation.
- Test: one table-driven test runs all three against the same input, asserts identical output and that peak concurrency stays `<= limit`, and that each returns `context.Canceled` when the parent is cancelled; `Benchmark*` for each variant.
- Verify: `go test -count=1 -race ./...`

Set up the module:

```bash
go mod edit -go=1.26
go get golang.org/x/sync/errgroup
```

### The three shapes, and when each wins

All three take a `Transform` (per-item work that honors `ctx`) and a `limit`, and
fill an output slice indexed to the input so per-item writes never race. What
differs is the machinery.

The **semaphore** version spawns one goroutine per item and gates each on a
buffered channel of capacity `limit`. It is the simplest to write and fine when
the item count is modest, but note the cost: a million-item batch spawns a
million goroutines — a million stacks — even though only `limit` run the transform
at once. The semaphore bounds concurrency, not goroutine count.

The **worker pool** starts exactly `limit` goroutines that drain a jobs channel.
The goroutine count is constant regardless of batch size, so memory is bounded,
and because the jobs channel is unbuffered here, feeding it applies back-pressure:
a producer outrunning the workers blocks on the send. This is the shape to reach
for when the goroutine count itself is a resource, or when you want producers
throttled.

The **errgroup** version calls `SetLimit(limit)` and `Go` per item. `SetLimit` is
a semaphore, so like the first version it spawns per item — but it also captures
the first error and cancels the derived context so siblings stop early, which the
other two must wire by hand. When you need first-error-wins semantics with
cancellation, this is the least code and the fewest bugs.

All three wrap the incoming context with a cancel so that the first transform
error cancels the rest; all three collect the first error. The point of putting
them behind one interface is that the *caller* is identical — the choice is an
implementation trade-off, not an API change.

Create `batch.go`:

```go
package batch

import (
	"context"
	"sync"

	"golang.org/x/sync/errgroup"
)

// Transform is per-item work. It must honor ctx cancellation.
type Transform func(ctx context.Context, n int) (int, error)

// Processor applies a Transform across a batch with bounded concurrency.
type Processor interface {
	Process(ctx context.Context, items []int) ([]int, error)
}

// --- Semaphore: one goroutine per item, gated by a buffered channel ---

type semaphoreProcessor struct {
	limit int
	fn    Transform
}

// NewSemaphoreProcessor bounds concurrency with a buffered-channel semaphore.
func NewSemaphoreProcessor(limit int, fn Transform) Processor {
	return semaphoreProcessor{limit: limit, fn: fn}
}

func (p semaphoreProcessor) Process(ctx context.Context, items []int) ([]int, error) {
	ctx, cancel := context.WithCancel(ctx)
	defer cancel()

	out := make([]int, len(items))
	sem := make(chan struct{}, p.limit)
	var wg sync.WaitGroup
	var mu sync.Mutex
	var firstErr error
	setErr := func(err error) {
		mu.Lock()
		if firstErr == nil {
			firstErr = err
		}
		mu.Unlock()
	}

loop:
	for i, it := range items {
		select {
		case sem <- struct{}{}:
		case <-ctx.Done():
			setErr(ctx.Err())
			break loop
		}
		wg.Add(1)
		go func() {
			defer wg.Done()
			defer func() { <-sem }()
			v, err := p.fn(ctx, it)
			if err != nil {
				setErr(err)
				cancel()
				return
			}
			out[i] = v
		}()
	}
	wg.Wait()

	mu.Lock()
	defer mu.Unlock()
	return out, firstErr
}

// --- Worker pool: a fixed number of goroutines draining a jobs channel ---

type poolProcessor struct {
	limit int
	fn    Transform
}

// NewPoolProcessor bounds the goroutine count with a fixed worker pool.
func NewPoolProcessor(limit int, fn Transform) Processor {
	return poolProcessor{limit: limit, fn: fn}
}

func (p poolProcessor) Process(ctx context.Context, items []int) ([]int, error) {
	ctx, cancel := context.WithCancel(ctx)
	defer cancel()

	out := make([]int, len(items))
	var mu sync.Mutex
	var firstErr error
	setErr := func(err error) {
		mu.Lock()
		if firstErr == nil {
			firstErr = err
		}
		mu.Unlock()
	}

	type job struct{ i, v int }
	jobs := make(chan job)
	var wg sync.WaitGroup
	for range p.limit {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for j := range jobs {
				v, err := p.fn(ctx, j.v)
				if err != nil {
					setErr(err)
					cancel()
					continue
				}
				out[j.i] = v
			}
		}()
	}

feed:
	for i, it := range items {
		select {
		case jobs <- job{i: i, v: it}:
		case <-ctx.Done():
			setErr(ctx.Err())
			break feed
		}
	}
	close(jobs)
	wg.Wait()

	mu.Lock()
	defer mu.Unlock()
	return out, firstErr
}

// --- errgroup: SetLimit as a semaphore, with error+cancellation for free ---

type errgroupProcessor struct {
	limit int
	fn    Transform
}

// NewErrgroupProcessor bounds concurrency with errgroup.SetLimit.
func NewErrgroupProcessor(limit int, fn Transform) Processor {
	return errgroupProcessor{limit: limit, fn: fn}
}

func (p errgroupProcessor) Process(ctx context.Context, items []int) ([]int, error) {
	g, ctx := errgroup.WithContext(ctx)
	g.SetLimit(p.limit)

	out := make([]int, len(items))
	for i, it := range items {
		if ctx.Err() != nil {
			break
		}
		g.Go(func() error {
			v, err := p.fn(ctx, it)
			if err != nil {
				return err
			}
			out[i] = v
			return nil
		})
	}
	err := g.Wait()
	return out, err
}
```

### The runnable demo

The demo runs all three processors over the same input with a doubling transform
and prints whether their outputs agree — a deterministic result even though the
work runs concurrently.

Create `cmd/demo/main.go`:

```go
package main

import (
	"context"
	"fmt"
	"slices"

	"example.com/batch"
)

func main() {
	items := make([]int, 20)
	for i := range items {
		items[i] = i
	}
	double := func(ctx context.Context, n int) (int, error) { return n * 2, nil }

	procs := []batch.Processor{
		batch.NewSemaphoreProcessor(4, double),
		batch.NewPoolProcessor(4, double),
		batch.NewErrgroupProcessor(4, double),
	}

	var want []int
	agree := true
	for i, p := range procs {
		got, err := p.Process(context.Background(), items)
		if err != nil {
			fmt.Printf("processor %d error: %v\n", i, err)
			return
		}
		if i == 0 {
			want = got
			continue
		}
		if !slices.Equal(got, want) {
			agree = false
		}
	}
	fmt.Printf("processors=%d agree=%t first=%v\n", len(procs), agree, want[:4])
}
```

Run it:

```bash
go run ./cmd/demo
```

Expected output:

```
processors=3 agree=true first=[0 2 4 6]
```

### Tests

`TestProcessorsAgree` runs all three against the same input with a tracking
transform and asserts identical output *and* that the observed peak concurrency
stayed `<= limit` — the trustworthy proof, under `-race`, that every variant
honored the cap. `TestProcessorsCancel` gives each a transform that blocks until
the context is cancelled, cancels the parent, and asserts each returns
`context.Canceled`. The `Benchmark*` functions run a trivial transform over a
fixed workload so you can compare goroutine-spawn cost.

Create `batch_test.go`:

```go
package batch

import (
	"context"
	"errors"
	"slices"
	"sync/atomic"
	"testing"
	"time"
)

func doublingWithPeak(live, peak *atomic.Int64) Transform {
	return func(ctx context.Context, n int) (int, error) {
		cur := live.Add(1)
		for {
			old := peak.Load()
			if cur <= old || peak.CompareAndSwap(old, cur) {
				break
			}
		}
		time.Sleep(time.Millisecond)
		live.Add(-1)
		return n * 2, nil
	}
}

func TestProcessorsAgree(t *testing.T) {
	t.Parallel()

	const limit = 4
	items := make([]int, 40)
	want := make([]int, len(items))
	for i := range items {
		items[i] = i
		want[i] = i * 2
	}

	cases := []struct {
		name string
		make func(fn Transform) Processor
	}{
		{"semaphore", func(fn Transform) Processor { return NewSemaphoreProcessor(limit, fn) }},
		{"pool", func(fn Transform) Processor { return NewPoolProcessor(limit, fn) }},
		{"errgroup", func(fn Transform) Processor { return NewErrgroupProcessor(limit, fn) }},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			var live, peak atomic.Int64
			p := tc.make(doublingWithPeak(&live, &peak))

			got, err := p.Process(context.Background(), items)
			if err != nil {
				t.Fatalf("Process err = %v, want nil", err)
			}
			if !slices.Equal(got, want) {
				t.Fatalf("output mismatch: got %v", got)
			}
			if pk := peak.Load(); pk > limit {
				t.Fatalf("peak concurrency = %d, want <= %d", pk, limit)
			}
		})
	}
}

func TestProcessorsCancel(t *testing.T) {
	t.Parallel()

	blockUntilCancel := func(ctx context.Context, n int) (int, error) {
		<-ctx.Done()
		return 0, ctx.Err()
	}
	items := make([]int, 20)
	for i := range items {
		items[i] = i
	}

	cases := []struct {
		name string
		proc Processor
	}{
		{"semaphore", NewSemaphoreProcessor(4, blockUntilCancel)},
		{"pool", NewPoolProcessor(4, blockUntilCancel)},
		{"errgroup", NewErrgroupProcessor(4, blockUntilCancel)},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			ctx, cancel := context.WithCancel(context.Background())
			go func() {
				time.Sleep(20 * time.Millisecond)
				cancel()
			}()
			_, err := tc.proc.Process(ctx, items)
			if !errors.Is(err, context.Canceled) {
				t.Fatalf("Process err = %v, want context.Canceled", err)
			}
		})
	}
}

func benchWorkload() []int {
	items := make([]int, 1000)
	for i := range items {
		items[i] = i
	}
	return items
}

func fastDouble(ctx context.Context, n int) (int, error) { return n * 2, nil }

func BenchmarkSemaphore(b *testing.B) {
	items := benchWorkload()
	p := NewSemaphoreProcessor(16, fastDouble)
	for b.Loop() {
		_, _ = p.Process(context.Background(), items)
	}
}

func BenchmarkPool(b *testing.B) {
	items := benchWorkload()
	p := NewPoolProcessor(16, fastDouble)
	for b.Loop() {
		_, _ = p.Process(context.Background(), items)
	}
}

func BenchmarkErrgroup(b *testing.B) {
	items := benchWorkload()
	p := NewErrgroupProcessor(16, fastDouble)
	for b.Loop() {
		_, _ = p.Process(context.Background(), items)
	}
}
```

## Review

The three implementations are correct when they produce identical output, keep
peak concurrency within the cap, and all return `context.Canceled` on a cancelled
parent — the table-driven test proves all three at once under `-race`. The
trade-off is the lesson: the semaphore and errgroup versions spawn a goroutine per
item (cheap per goroutine, but N of them for N items), while the worker pool holds
the goroutine count constant and back-pressures producers through the jobs
channel. errgroup additionally hands you first-error cancellation with no manual
wiring, which the other two reimplement by hand with a `context.WithCancel` and a
guarded `firstErr`. Reach for the pool when goroutine count or producer
back-pressure is the constraint, errgroup when error-and-cancel semantics
dominate, and the bare semaphore when the fan-out is small and you want the least
indirection. Run the benchmarks with `go test -bench . -benchmem` to see the
spawn-cost difference on your workload.

## Resources

- [golang.org/x/sync/errgroup](https://pkg.go.dev/golang.org/x/sync/errgroup) — `SetLimit`, `WithContext`, first-error-wins semantics.
- [Go Blog: Pipelines and cancellation](https://go.dev/blog/pipelines) — worker pools, fan-out/fan-in, and cancellation.
- [testing: B.Loop](https://pkg.go.dev/testing#B.Loop) — the modern benchmark loop used here.
- [Effective Go: Channels](https://go.dev/doc/effective_go#channels) — buffered channels as semaphores and the worker-pool pattern.

---

Back to [00-concepts.md](00-concepts.md) | Previous: [09-drain-on-shutdown-semaphore.md](09-drain-on-shutdown-semaphore.md) | Next: [11-keyed-semaphore-per-tenant-isolation.md](11-keyed-semaphore-per-tenant-isolation.md)
