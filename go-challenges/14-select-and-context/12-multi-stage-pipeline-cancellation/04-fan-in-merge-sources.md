# Exercise 4: Fan-In — Merge Multiple Upstream Sources Into One Stream

The inverse of fan-out is fan-in: many upstream channels, one downstream. A worker
that consumes several Kafka partitions, or several shard readers, needs to
multiplex them into a single processing stream. The close-ownership problem is the
same as fan-out's, inverted: many forwarders write the merged output, exactly one
goroutine may close it, and only after every forwarder has exited. This module
builds `Merge`, framed as multiplexing several partition readers into one stage.

This module is fully self-contained: its own `go mod init`, all code inline, its
own demo and tests. Nothing here imports another exercise.

## What you'll build

```text
fanin/                       module example.com/fanin
  go.mod
  fanin.go                   func Merge(ctx, ...<-chan int) <-chan int
  cmd/
    demo/
      main.go                merges 3 partition readers, prints the union set + count
  fanin_test.go              union invariant, single-close, cancel-drains-all-sources
```

Files: `fanin.go`, `cmd/demo/main.go`, `fanin_test.go`.
Implement: `Merge(ctx, ins ...<-chan int) <-chan int` — one forwarder goroutine per
input, each select-sending to a shared output, with one closer that waits on a
`WaitGroup` and closes exactly once.
Test: the merged union (order-free) equals the union of all sources with correct
total count, the output closes exactly once, and a cancel mid-read drains all
source forwarders with no leak.
Verify: `go test -count=1 -race ./...`

Set up the module:

```bash
mkdir -p go-solutions/14-select-and-context/12-multi-stage-pipeline-cancellation/04-fan-in-merge-sources/cmd/demo
cd go-solutions/14-select-and-context/12-multi-stage-pipeline-cancellation/04-fan-in-merge-sources
```

### One forwarder per input, one closer over the WaitGroup

`Merge` takes a variadic slice of input channels. For each input it launches one
*forwarder* goroutine that ranges the input and select-sends each value onto the
shared `out`, also watching `ctx.Done()`. A `sync.WaitGroup`, sized to the number
of inputs *before* any forwarder starts, counts the forwarders; a single closer
goroutine `Wait`s for all of them and then closes `out`.

This is exactly the fan-out close discipline applied to a different topology. The
forwarder's select-send is essential for the same reason: if the downstream stops
reading and one forwarder is mid-send, a bare send would block that forwarder
forever, its `wg.Done()` would never run, and the closer would hang — the merged
output would never close and the whole pipeline would stall. With the select-send,
a cancel unblocks every forwarder through its `ctx.Done()` case; each returns,
`wg` drains to zero, and the closer closes `out` cleanly.

A subtle point: when a forwarder's *input* channel closes normally (that partition
reached end of stream), its `range` ends and it returns even without a cancel. So
`Merge` handles both partial teardown (one source ends, the rest keep flowing) and
full cancel (all sources torn down at once) with the same code — the output closes
only once every source is exhausted or the context is cancelled.

Create `fanin.go`:

```go
package fanin

import (
	"context"
	"sync"
)

// Merge multiplexes any number of input channels into one output channel. Each
// input gets one forwarder goroutine; a single closer waits for all forwarders
// (via a WaitGroup) and then closes the output exactly once. A ctx cancel tears
// down every forwarder and ends the merged stream cleanly.
func Merge(ctx context.Context, ins ...<-chan int) <-chan int {
	out := make(chan int)
	var wg sync.WaitGroup
	wg.Add(len(ins))
	for _, in := range ins {
		go func(in <-chan int) {
			defer wg.Done()
			for v := range in {
				select {
				case out <- v:
				case <-ctx.Done():
					return
				}
			}
		}(in)
	}
	go func() {
		wg.Wait()
		close(out)
	}()
	return out
}
```

The forwarder takes `in` as a parameter rather than closing over the loop variable.
Under Go 1.22+ loop-variable semantics the closure-capture bug is gone, but passing
`in` explicitly keeps the forwarder's input unambiguous and is the idiom most teams
still write for a per-iteration goroutine.

### The runnable demo

The demo builds three "partition readers" — small source channels emitting disjoint
ID ranges — merges them, and collects the union. Because fan-in interleaves
non-deterministically, the demo sorts the collected IDs before printing so the
output is stable.

Create `cmd/demo/main.go`:

```go
package main

import (
	"context"
	"fmt"
	"sort"

	"example.com/fanin"
)

// partition emits the ids on a fresh channel, closing when done.
func partition(ids ...int) <-chan int {
	out := make(chan int)
	go func() {
		defer close(out)
		for _, id := range ids {
			out <- id
		}
	}()
	return out
}

func main() {
	ctx := context.Background()
	p0 := partition(0, 1, 2)
	p1 := partition(10, 11, 12)
	p2 := partition(20, 21, 22)

	merged := fanin.Merge(ctx, p0, p1, p2)

	var got []int
	for id := range merged {
		got = append(got, id)
	}
	sort.Ints(got)
	fmt.Printf("count=%d union=%v\n", len(got), got)
}
```

Run it:

```bash
go run ./cmd/demo
```

Expected output:

```
count=9 union=[0 1 2 10 11 12 20 21 22]
```

### Tests

`TestMergeUnion` asserts the merged output, collected into a set, equals the union
of all sources — fan-in reorders but never drops or duplicates. `TestSingleClose`
runs many merges under `-race` to catch a double close. `TestCancelDrainsAllSources`
cancels after a partial read and, using a goroutine-count baseline, proves every
forwarder exited.

Create `fanin_test.go`:

```go
package fanin

import (
	"context"
	"runtime"
	"testing"
	"time"
)

func partition(ids ...int) <-chan int {
	out := make(chan int)
	go func() {
		defer close(out)
		for _, id := range ids {
			out <- id
		}
	}()
	return out
}

func settles(before int, timeout time.Duration) bool {
	deadline := time.Now().Add(timeout)
	for {
		runtime.Gosched()
		if runtime.NumGoroutine() <= before+2 {
			return true
		}
		if time.Now().After(deadline) {
			return runtime.NumGoroutine() <= before+2
		}
		time.Sleep(time.Millisecond)
	}
}

func TestMergeUnion(t *testing.T) {
	t.Parallel()

	merged := Merge(context.Background(),
		partition(0, 1, 2),
		partition(10, 11, 12, 13),
		partition(20),
	)

	got := map[int]int{}
	for v := range merged {
		got[v]++
	}
	want := []int{0, 1, 2, 10, 11, 12, 13, 20}
	if len(got) != len(want) {
		t.Fatalf("distinct values = %d, want %d: %v", len(got), len(want), got)
	}
	for _, w := range want {
		if got[w] != 1 {
			t.Fatalf("value %d seen %d times, want 1", w, got[w])
		}
	}
}

func TestSingleClose(t *testing.T) {
	t.Parallel()

	for range 30 {
		merged := Merge(context.Background(),
			partition(1, 2, 3),
			partition(4, 5, 6),
		)
		for range merged {
		}
	}
}

func TestCancelDrainsAllSources(t *testing.T) {
	before := runtime.NumGoroutine()

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	// Sources that never end on their own, so only cancel can stop the forwarders.
	src := func() <-chan int {
		out := make(chan int)
		go func() {
			defer close(out)
			for i := 0; ; i++ {
				select {
				case out <- i:
				case <-ctx.Done():
					return
				}
			}
		}()
		return out
	}
	merged := Merge(ctx, src(), src(), src())

	read := 0
	for range merged {
		read++
		if read == 10 {
			cancel()
			break
		}
	}
	for range merged { // drain the rest so forwarders unblock
	}

	if !settles(before, 100*time.Millisecond) {
		t.Fatalf("forwarders leaked: before=%d after=%d", before, runtime.NumGoroutine())
	}
}
```

`TestCancelDrainsAllSources` does not call `t.Parallel()`: it reads the
process-global goroutine count, which a parallel sibling would perturb.

## Review

`Merge` is correct when the union invariant holds, the output closes exactly once,
and a cancel tears down every forwarder so the merged stream ends without a hung
goroutine. The failure modes mirror fan-out: giving each forwarder `defer
close(out)` double-closes the moment a second source ends, and a bare send leaves a
forwarder blocked forever on cancel so the closer never runs. `TestSingleClose`
under `-race` catches the double close; `TestCancelDrainsAllSources` catches the
leak via the goroutine baseline. Note that `Merge` closes `out` only after *all*
sources are exhausted or the context fires — a single finished partition does not
close the merged stream.

## Resources

- [Go Blog: Pipelines and cancellation](https://go.dev/blog/pipelines) — the `merge` function this module generalizes to variadic inputs.
- [`sync.WaitGroup`](https://pkg.go.dev/sync#WaitGroup) — the forwarder counter and the closer's `Wait`.
- [Go Concurrency Patterns (Pike, 2012)](https://go.dev/talks/2012/concurrency.slide) — fan-in and the single-closer idiom.

---

Back to [03-goroutine-leak-harness.md](03-goroutine-leak-harness.md) | Next: [05-bounded-errgroup-pipeline.md](05-bounded-errgroup-pipeline.md)
