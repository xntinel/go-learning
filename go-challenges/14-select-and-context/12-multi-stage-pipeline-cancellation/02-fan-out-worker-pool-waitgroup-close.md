# Exercise 2: Fan-Out Worker Pool — One Closer After All Workers Exit

When one stage is the bottleneck — say each item requires a call to a slow
enrichment service — you parallelize it by fanning out: N workers read one input
channel and write one shared output channel. The resource hazard is the close:
many goroutines write `out`, but exactly one may close it, and only after *every*
worker has exited. This module builds that fan-out with a `sync.WaitGroup` gate
and a dedicated closer goroutine, framed as N parallel enrichment workers whose
output order is not guaranteed.

This module is fully self-contained: its own `go mod init`, all code inline, its
own demo and tests. Nothing here imports another exercise.

## What you'll build

```text
fanout/                      module example.com/fanout
  go.mod
  fanout.go                  func FanOut(ctx, in, n, work) <-chan int
  cmd/
    demo/
      main.go                fans 0..11 across 3 workers, prints the sum
  fanout_test.go             sum invariant, worker count, single-close, cancel-mid-stream
```

Files: `fanout.go`, `cmd/demo/main.go`, `fanout_test.go`.
Implement: `FanOut(ctx, in, n, work) <-chan int` — N workers apply `work` to each
input and select-send to a shared output that exactly one closer closes after all
workers exit.
Test: sum invariant over unordered output, N workers actually run, output closes
exactly once (no panic under `-race`), and a cancel mid-stream drains cleanly.
Verify: `go test -count=1 -race ./...`

Set up the module:

```bash
mkdir -p ~/go-exercises/fanout/cmd/demo
cd ~/go-exercises/fanout
go mod init example.com/fanout
```

### The close ownership problem, and the WaitGroup gate

If each of N workers did `defer close(out)`, the first worker to finish would close
`out` while the other N-1 are still sending — an immediate "send on closed
channel" panic. The output channel is *shared*, so its close cannot be owned by
any single worker. The standard resolution is to make the close owned by a
separate goroutine that runs only after all writers are provably done:

```go
var wg sync.WaitGroup
wg.Add(n)
// ... launch n workers, each doing defer wg.Done() ...
go func() {
	wg.Wait()
	close(out)
}()
```

`wg.Add(n)` is called *before* launching any worker (adding inside the workers
would race the closer's `Wait`). Each worker calls `wg.Done()` on exit. The closer
blocks on `wg.Wait()` until the counter hits zero — i.e. every worker has
returned — then and only then closes `out`. Because all writes precede all workers'
`Done` calls, and `close` happens-after `Wait` returns, there is never a send
racing the close.

Each worker still needs the select-guarded send: when `ctx` is cancelled, a worker
mid-send to an unread `out` must be able to return rather than block, or the
`WaitGroup` never reaches zero and the closer hangs forever. So every worker's
loop is `for v := range in { select { case out <- work(v): case <-ctx.Done():
return } }`. When the input channel closes normally, the `range` ends and the
worker returns; when `ctx` is cancelled, the select's `ctx.Done()` case returns.
Either way `wg.Done()` runs, the closer eventually unblocks, and `out` closes.

Create `fanout.go`:

```go
package fanout

import (
	"context"
	"sync"
)

// FanOut runs n workers that each read from in, apply work, and send to a single
// shared output channel. Output order is not guaranteed. Exactly one goroutine
// closes the output, after all workers have exited (normal drain or ctx cancel).
func FanOut(ctx context.Context, in <-chan int, n int, work func(int) int) <-chan int {
	out := make(chan int)
	var wg sync.WaitGroup
	wg.Add(n)
	for range n {
		go func() {
			defer wg.Done()
			for v := range in {
				select {
				case out <- work(v):
				case <-ctx.Done():
					return
				}
			}
		}()
	}
	go func() {
		wg.Wait()
		close(out)
	}()
	return out
}
```

### The runnable demo

The demo fans a source of 0..11 across three workers that triple each value and
sums the unordered output. The sum is order-independent, so it is stable even
though which worker handles which value is not.

Create `cmd/demo/main.go`:

```go
package main

import (
	"context"
	"fmt"

	"example.com/fanout"
)

func source(ctx context.Context, n int) <-chan int {
	out := make(chan int)
	go func() {
		defer close(out)
		for i := range n {
			select {
			case out <- i:
			case <-ctx.Done():
				return
			}
		}
	}()
	return out
}

func main() {
	ctx := context.Background()
	in := source(ctx, 12)
	out := fanout.FanOut(ctx, in, 3, func(v int) int { return v * 3 })

	sum := 0
	count := 0
	for v := range out {
		sum += v
		count++
	}
	fmt.Printf("workers=3 count=%d sum=%d\n", count, sum)
	fmt.Printf("expected sum=%d (3 * (0+1+...+11))\n", 3*66)
}
```

Run it:

```bash
go run ./cmd/demo
```

Expected output:

```
workers=3 count=12 sum=198
expected sum=198 (3 * (0+1+...+11))
```

### Tests

`TestFanOutPreservesAllValues` asserts the sum invariant: fan-out reorders but
never drops or duplicates, so the sum of the tripled output equals the sum of the
tripled input. `TestFanOutRunsWorkersInParallel` proves the pool is genuinely
concurrent: a barrier releases only once all `n` workers are simultaneously inside
`work`, so peak observed concurrency reaches `n`; a single-worker implementation
could never get `n` goroutines to the barrier and would deadlock the test.
`TestOutputClosesExactlyOnce` runs under `-race` and would panic if the closer
raced a worker's send. `TestCancelMidStreamDrainsCleanly` cancels after a few reads
and drains the rest with no deadlock.

Create `fanout_test.go`:

```go
package fanout

import (
	"context"
	"sync/atomic"
	"testing"
)

func source(ctx context.Context, start, count int) <-chan int {
	out := make(chan int)
	go func() {
		defer close(out)
		for i := start; i < start+count; i++ {
			select {
			case out <- i:
			case <-ctx.Done():
				return
			}
		}
	}()
	return out
}

func TestFanOutPreservesAllValues(t *testing.T) {
	t.Parallel()

	in := source(context.Background(), 0, 100)
	out := FanOut(context.Background(), in, 4, func(v int) int { return v * 3 })

	sum, count := 0, 0
	for v := range out {
		sum += v
		count++
	}
	if count != 100 {
		t.Fatalf("count = %d, want 100", count)
	}
	want := 0
	for i := range 100 {
		want += i * 3
	}
	if sum != want {
		t.Fatalf("sum = %d, want %d", sum, want)
	}
}

func TestFanOutRunsWorkersInParallel(t *testing.T) {
	t.Parallel()

	// A barrier that releases only once all n workers are simultaneously inside
	// work proves real parallelism: cur tracks how many workers are concurrently
	// in-flight, and peak records its high-water mark. The n-th worker to arrive
	// closes the gate, releasing everyone. A single-worker pool could never reach
	// arrived==n, so the gate would never close and the test would deadlock.
	const n = 4
	var arrived, cur, peak, invoked atomic.Int64
	gate := make(chan struct{})

	in := source(context.Background(), 0, 200)
	out := FanOut(context.Background(), in, n, func(v int) int {
		c := cur.Add(1)
		for {
			p := peak.Load()
			if c <= p || peak.CompareAndSwap(p, c) {
				break
			}
		}
		if arrived.Add(1) == n {
			close(gate) // the n-th worker to enter releases the whole pool
		}
		<-gate // block until n workers are concurrently in-flight
		cur.Add(-1)
		invoked.Add(1)
		return v
	})
	for range out {
	}
	if peak.Load() < n {
		t.Fatalf("peak concurrency = %d, want %d (workers did not run in parallel)", peak.Load(), n)
	}
	if invoked.Load() != 200 {
		t.Fatalf("work invoked %d times, want 200", invoked.Load())
	}
}

func TestOutputClosesExactlyOnce(t *testing.T) {
	t.Parallel()

	// If the closer raced a worker's send, -race and the runtime would flag it.
	for range 20 {
		in := source(context.Background(), 0, 30)
		out := FanOut(context.Background(), in, 5, func(v int) int { return v })
		for range out {
		}
	}
}

func TestCancelMidStreamDrainsCleanly(t *testing.T) {
	t.Parallel()

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	in := source(ctx, 0, 1000)
	out := FanOut(ctx, in, 3, func(v int) int { return v })

	read := 0
	for range out {
		read++
		if read == 5 {
			cancel()
		}
	}
	// Reaching here means every worker and the closer exited: no deadlock.
	if read < 5 {
		t.Fatalf("read = %d, want at least 5 before cancel", read)
	}
}
```

## Review

The fan-out is correct when the sum invariant holds (nothing dropped or
duplicated), the output channel is closed exactly once by the dedicated closer
after `wg.Wait()`, and a cancel mid-stream lets every worker return so the closer
unblocks. The two ways to break it are giving each worker `defer close(out)`
(panics the instant a second worker finishes) and dropping the select-guard on the
send (a cancelled-but-still-sending worker never calls `wg.Done()`, so the closer
hangs and `out` never closes). `TestOutputClosesExactlyOnce` under `-race` catches
the first; `TestCancelMidStreamDrainsCleanly` catches the second by deadlocking if
a worker leaks. Run `go test -race`.

## Resources

- [`sync.WaitGroup`](https://pkg.go.dev/sync#WaitGroup) — `Add`/`Done`/`Wait` and the happens-before guarantees the closer relies on.
- [Go Blog: Pipelines and cancellation](https://go.dev/blog/pipelines) — the "fan-out, fan-in" section and the single-closer pattern.
- [Go Memory Model](https://go.dev/ref/mem) — why `wg.Wait` returning happens-after every `wg.Done`, making the close safe.

---

Back to [01-stage-primitives-generate-transform-collect.md](01-stage-primitives-generate-transform-collect.md) | Next: [03-goroutine-leak-harness.md](03-goroutine-leak-harness.md)
