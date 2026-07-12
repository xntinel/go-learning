# Exercise 3: Fan-In: Merge Many Worker Output Channels into One

When you fan work out to N parallel workers, each with its own result channel, a
downstream consumer wants a single stream to `range`. Fan-in merges those N
channels into one. The whole art is closing the merged channel exactly once, after
every input has drained. This exercise builds a generic `Merge` and the
WaitGroup-gated closer that makes it correct.

This module is fully self-contained: its own `go mod init`, all code inline, its
own demo and tests. Nothing here imports another exercise.

## What you'll build

```text
faninmerge/                 independent module: example.com/faninmerge
  go.mod                    go 1.26
  merge.go                  Merge[T](chans ...<-chan T) <-chan T; Collect helper
  cmd/
    demo/
      main.go               three producers merged into one drained stream
  merge_test.go             union multiset, zero-channels, single-close under -race
```

Files: `merge.go`, `cmd/demo/main.go`, `merge_test.go`.
Implement: `Merge[T any](chans ...<-chan T) <-chan T` — one forwarding goroutine per input, one closer goroutine gated by a `sync.WaitGroup` that closes the output exactly once.
Test: merging three producers yields the union of their values (order-independent, compared as a count map); merging zero channels yields an immediately-closed channel; `-race` verifies the single-close discipline.
Verify: `go test -count=1 -race ./...`

Set up the module:

```bash
mkdir -p go-solutions/13-goroutines-and-channels/06-ranging-over-channels/03-fan-in-merge/cmd/demo
cd go-solutions/13-goroutines-and-channels/06-ranging-over-channels/03-fan-in-merge
```

### The single-close discipline

The naive fan-in — have each forwarding goroutine `close(out)` when its input ends
— panics the instant the second one runs: `close of closed channel`. The
symmetric mistake, closing `out` before the forwarders finish, drops values that
were still in flight. The correct structure separates the two concerns:

- One **forwarding goroutine per input**, each ranging its input and copying every
  value into the shared `out`. When its input closes, its `range` ends and the
  goroutine calls `wg.Done()` and exits. It never touches `close`.
- One **closer goroutine** that does `wg.Wait()` then `close(out)`. It runs
  exactly once and fires only after every forwarder has returned, so `out` is
  closed once, after the last value has been forwarded.

`Merge` returns `out` as a receive-only `<-chan T`, so the caller can only range
it, never send or close — the ownership contract again. The output is unbuffered
here; each forwarder blocks until the consumer takes the value, which is the
natural backpressure. Because the consumer ranges the merged channel to
completion, all forwarders drain and the closer fires; nothing leaks.

Create `merge.go`:

```go
package faninmerge

import "sync"

// Merge fans in any number of receive-only channels into a single channel. It
// starts one forwarding goroutine per input and one closer goroutine that closes
// the output exactly once, after every input has been fully drained.
func Merge[T any](chans ...<-chan T) <-chan T {
	out := make(chan T)
	var wg sync.WaitGroup

	forward := func(c <-chan T) {
		defer wg.Done()
		for v := range c { // ends when this input is closed and drained
			out <- v
		}
	}

	wg.Add(len(chans))
	for _, c := range chans {
		go forward(c)
	}

	// Exactly one goroutine closes out, and only after all forwarders finish.
	go func() {
		wg.Wait()
		close(out)
	}()

	return out
}

// Collect drains a channel into a slice. Handy for callers and tests.
func Collect[T any](ch <-chan T) []T {
	var out []T
	for v := range ch {
		out = append(out, v)
	}
	return out
}
```

### The runnable demo

The demo starts three producers, each emitting a handful of integers on its own
channel and closing it, then merges and drains them. Because merge order across
concurrent producers is non-deterministic, the demo sorts before printing so the
output is stable.

Create `cmd/demo/main.go`:

```go
package main

import (
	"fmt"
	"sort"

	"example.com/faninmerge"
)

func producer(vals ...int) <-chan int {
	ch := make(chan int, len(vals))
	for _, v := range vals {
		ch <- v
	}
	close(ch)
	return ch
}

func main() {
	merged := faninmerge.Merge(
		producer(1, 2, 3),
		producer(10, 20),
		producer(100),
	)
	got := faninmerge.Collect(merged)
	sort.Ints(got)
	fmt.Printf("merged %d values: %v\n", len(got), got)
}
```

Run it:

```bash
go run ./cmd/demo
```

Expected output:

```
merged 6 values: [1 2 3 10 20 100]
```

### Tests

`TestMergeUnion` merges three producers and compares the result as a count map, so
the assertion is order-independent — fan-in makes no ordering promise across
inputs. `TestMergeZeroChannels` merges nothing and asserts the returned channel is
already closed (a `range` over it ends immediately), which the closer goroutine
guarantees because `wg.Wait()` returns instantly when the count is zero.
`TestMergeManyConcurrent` merges many producers under `-race` to exercise the
single-close discipline against concurrent forwarders.

Create `merge_test.go`:

```go
package faninmerge

import (
	"fmt"
	"testing"
)

func producer(vals ...int) <-chan int {
	ch := make(chan int, len(vals))
	for _, v := range vals {
		ch <- v
	}
	close(ch)
	return ch
}

func counts(vals []int) map[int]int {
	m := make(map[int]int)
	for _, v := range vals {
		m[v]++
	}
	return m
}

func TestMergeUnion(t *testing.T) {
	t.Parallel()
	merged := Merge(
		producer(1, 2, 3),
		producer(2, 3, 4),
		producer(5),
	)
	got := counts(Collect(merged))
	want := map[int]int{1: 1, 2: 2, 3: 2, 4: 1, 5: 1}

	if len(got) != len(want) {
		t.Fatalf("distinct values = %d, want %d", len(got), len(want))
	}
	for k, w := range want {
		if got[k] != w {
			t.Fatalf("count[%d] = %d, want %d", k, got[k], w)
		}
	}
}

func TestMergeZeroChannels(t *testing.T) {
	t.Parallel()
	merged := Merge[int]()
	got := Collect(merged) // must not block; channel is closed immediately
	if len(got) != 0 {
		t.Fatalf("Merge() with no inputs = %v, want empty", got)
	}
}

func TestMergeManyConcurrent(t *testing.T) {
	t.Parallel()
	const producers, each = 8, 50
	chans := make([]<-chan int, producers)
	for p := range producers {
		vals := make([]int, each)
		for i := range each {
			vals[i] = p*each + i
		}
		chans[p] = producer(vals...)
	}
	got := Collect(Merge(chans...))
	if len(got) != producers*each {
		t.Fatalf("merged %d values, want %d", len(got), producers*each)
	}
}

func ExampleMerge() {
	merged := Merge(producer(42))
	fmt.Println(Collect(merged))
	// Output: [42]
}
```

## Review

The merge is correct when it forwards every value from every input, closes the
output exactly once, and never panics or hangs. The single-close discipline is the
crux: the closer goroutine gated on `wg.Wait()` is the *only* place `close(out)`
appears, which is why forwarders can finish in any order without a double-close.
The union test compares count maps precisely because fan-in makes no cross-input
ordering guarantee — asserting a specific order would flake. The zero-channel test
proves the closer fires immediately when there is nothing to wait for. Run under
`-race`; the whole point of the WaitGroup is to make the concurrent forwarders and
the single close data-race-free.

## Resources

- [Go Blog: Pipelines and cancellation](https://go.dev/blog/pipelines) — the canonical fan-in `Merge` with a WaitGroup closer.
- [pkg.go.dev: sync.WaitGroup](https://pkg.go.dev/sync#WaitGroup) — `Add`/`Done`/`Wait` and the "wait for all goroutines" pattern.

---

Back to [00-concepts.md](00-concepts.md) | Previous: [02-context-cancellable-consumer.md](02-context-cancellable-consumer.md) | Next: [04-batch-flusher.md](04-batch-flusher.md)
