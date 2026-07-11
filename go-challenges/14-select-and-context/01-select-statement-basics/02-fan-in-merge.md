# Exercise 2: Merge N Shard Event Streams Into One (Fan-In)

A change-data-capture consumer reads one event stream per database shard and has
to present a single, unified feed to the rest of the system. That is the fan-in
pattern: N source channels collapsed into one output channel that closes exactly
once, after every source has drained. This module builds the generic, modern
version of that merge.

This module is fully self-contained: its own `go mod init`, all code inline, its
own demo and tests. It imports no other exercise.

## What you'll build

```text
shardmerge/                     module example.com/shardmerge
  go.mod                        go 1.26
  shardmerge.go                 FanIn[T](sources ...<-chan T) <-chan T
  cmd/
    demo/
      main.go                   merge three shard feeds, print the unified stream
  shardmerge_test.go            multiset-equality merge, concurrent producers, Example
```

Files: `shardmerge.go`, `cmd/demo/main.go`, `shardmerge_test.go`.
Implement: `FanIn[T any](sources ...<-chan T) <-chan T` — one draining goroutine per source, single close after all drain.
Test: pre-filled+closed sources merge to the union (order-independent); concurrent producers merge to the exact count and the output closes.
Verify: `go test -count=1 -race ./...`

Set up the module:

```bash
mkdir -p ~/go-exercises/shardmerge/cmd/demo
cd ~/go-exercises/shardmerge
go mod init example.com/shardmerge
```

## The merge contract, and where it panics if you break it

The shape is fixed and every part of it is load-bearing:

- One goroutine per source, each draining its source with `for v := range src`.
  Ranging over a channel is the idiomatic drain: it pulls values until the source
  is closed and empty, then the loop exits on its own. No comma-ok, no manual
  close detection.
- A `sync.WaitGroup` to learn when *all* source goroutines have finished. In Go
  1.25 the clean spelling is `wg.Go(func(){ ... })`, which does the `Add(1)` and
  `defer Done()` for you and cannot be mis-paired — a dropped `Done` (which hangs
  the merge forever) is impossible.
- A single `close(out)` performed by *one dedicated goroutine* after `wg.Wait()`.

That last point is the one that panics if you get it wrong. If a source goroutine
closed `out` when it finished, a second source still running would call
`out <- v` on a closed channel and the program would panic with "send on closed
channel." The output must be closed exactly once, by nobody who is still sending.
The dedicated `wg.Wait(); close(out)` goroutine is the only safe place.

`out` is unbuffered: it is a rendezvous point. Each source goroutine blocks on
`out <- v` until the downstream consumer receives, which is exactly the
backpressure you want — a slow consumer naturally slows every producer instead of
letting an unbounded backlog build in memory.

Two modern notes. First, `for _, src := range sources` no longer needs the old
`src := src` shadow: since Go 1.22 each iteration binds a fresh `src`, so the
goroutine closes over the right channel automatically. Second, this is generic:
`FanIn[T any]` merges channels of any element type — shard events, log records,
metrics — with no `interface{}` and no per-type copy.

Create `shardmerge.go`:

```go
package shardmerge

import "sync"

// FanIn merges any number of source channels into a single output channel. One
// goroutine drains each source; the output is closed exactly once, by a
// dedicated goroutine, only after every source has drained. The output is
// unbuffered, so a slow consumer applies backpressure to all producers.
func FanIn[T any](sources ...<-chan T) <-chan T {
	out := make(chan T)

	var wg sync.WaitGroup
	for _, src := range sources {
		// Since Go 1.22, src is a fresh variable each iteration; no shadow needed.
		wg.Go(func() {
			for v := range src {
				out <- v
			}
		})
	}

	// Close out exactly once, from here, after all sources have drained. Closing
	// from a source goroutine would risk a send-on-closed panic in another.
	go func() {
		wg.Wait()
		close(out)
	}()

	return out
}
```

## The runnable demo

The demo builds three shard feeds, each pre-loaded and closed, and merges them.
Because the merged order is up to the scheduler and `select`'s fairness, the demo
sorts before printing so the output is stable to read.

Create `cmd/demo/main.go`:

```go
package main

import (
	"fmt"
	"sort"

	"example.com/shardmerge"
)

func shard(events ...string) <-chan string {
	ch := make(chan string, len(events))
	for _, e := range events {
		ch <- e
	}
	close(ch)
	return ch
}

func main() {
	merged := shardmerge.FanIn(
		shard("shard0:insert", "shard0:update"),
		shard("shard1:delete"),
		shard("shard2:insert", "shard2:insert", "shard2:update"),
	)

	var got []string
	for e := range merged {
		got = append(got, e)
	}
	sort.Strings(got) // merged order is nondeterministic; sort for a stable view

	fmt.Printf("merged %d events:\n", len(got))
	for _, e := range got {
		fmt.Println(e)
	}
}
```

Run with `go run ./cmd/demo`.

Expected output:

```
merged 6 events:
shard0:insert
shard0:update
shard1:delete
shard2:insert
shard2:insert
shard2:update
```

## Tests

Because the merged order is nondeterministic, the tests assert on multisets, not
sequences. `TestFanInMergesAllSources` builds synchronous pre-filled-and-closed
sources and asserts the merged output, counted into a map, equals the union of the
inputs — every event present, none invented, duplicates preserved.
`TestFanInConcurrentProducers` spawns four concurrent producers of 25 sends each
and asserts the merged channel yields exactly 100 values and then *closes* (the
`for range` terminates), proving the single-close-after-`wg.Wait()` fires. Under
`-race`, this second test is the real proof: any unsynchronized or double close
would be reported.

Create `shardmerge_test.go`:

```go
package shardmerge

import (
	"fmt"
	"sort"
	"sync"
	"testing"
)

func closedSource(values ...string) <-chan string {
	c := make(chan string, len(values))
	for _, v := range values {
		c <- v
	}
	close(c)
	return c
}

func TestFanInMergesAllSources(t *testing.T) {
	t.Parallel()

	merged := FanIn(
		closedSource("alpha", "beta"),
		closedSource("gamma", "delta"),
		closedSource("epsilon", "alpha"), // duplicate "alpha" must survive
	)

	got := map[string]int{}
	for v := range merged {
		got[v]++
	}

	want := map[string]int{"alpha": 2, "beta": 1, "gamma": 1, "delta": 1, "epsilon": 1}
	if len(got) != len(want) {
		t.Fatalf("distinct values = %d, want %d (%v)", len(got), len(want), got)
	}
	for k, n := range want {
		if got[k] != n {
			t.Fatalf("count[%q] = %d, want %d", k, got[k], n)
		}
	}
}

func TestFanInConcurrentProducers(t *testing.T) {
	t.Parallel()

	const producers = 4
	const perProducer = 25

	sources := make([]<-chan string, producers)
	var wg sync.WaitGroup
	for i := range producers {
		c := make(chan string) // unbuffered: real concurrent sends, not a prefill
		sources[i] = c
		wg.Go(func() {
			defer close(c)
			for range perProducer {
				c <- fmt.Sprintf("p%d", i)
			}
		})
	}

	merged := FanIn(sources...)
	count := 0
	for range merged { // terminates only if FanIn closes out after all drain
		count++
	}
	wg.Wait()

	if count != producers*perProducer {
		t.Fatalf("merged %d values, want %d", count, producers*perProducer)
	}
}

func Example() {
	merged := FanIn(closedSource("b", "a"), closedSource("c"))
	var got []string
	for v := range merged {
		got = append(got, v)
	}
	sort.Strings(got)
	fmt.Println(got)
	// Output: [a b c]
}
```

## Review

The merge is correct when the output is a faithful multiset union of the inputs —
nothing dropped, nothing invented, duplicates preserved — and the output channel
closes exactly once after the last source drains, so a downstream `for range`
terminates. The two failure shapes to keep out are the double-close (a producer
closing `out`) and the lost-`Done` hang (manual `Add`/`Done` mis-paired);
`wg.Go` plus a single dedicated closer eliminates both by construction. The
`-race` run on `TestFanInConcurrentProducers` is the assertion that matters most —
functional tests can pass on a lucky schedule while a close race lurks. Do not
buffer `out` to "speed things up"; the unbuffered rendezvous is deliberate
backpressure.

## Resources

- [sync.WaitGroup.Go](https://pkg.go.dev/sync#WaitGroup.Go) — the Go 1.25 method that pairs Add/Done for you.
- [Go Blog: Pipelines and cancellation](https://go.dev/blog/pipelines) — the canonical fan-in/fan-out treatment this module distills.
- [The Go Memory Model](https://go.dev/ref/mem) — why the close must be ordered after every send via `wg.Wait()`.

---

Back to [00-concepts.md](00-concepts.md) | Previous: [01-multiplex-first-ready.md](01-multiplex-first-ready.md) | Next: [03-fan-in-demo-cli.md](03-fan-in-demo-cli.md)
