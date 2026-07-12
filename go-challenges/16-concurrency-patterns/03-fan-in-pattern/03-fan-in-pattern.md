# 3. Fan-In Pattern: Merge Many Channels Into One

Fan-in is the dual of fan-out: a function reads from multiple input channels
and multiplexes them onto a single output channel. The Go blog's pipeline
article calls this "a way to combine results from multiple workers". The
canonical implementation uses `sync.WaitGroup` to delay closing the output
until every input is drained.

```text
fanin/
  go.mod
  internal/fanin/fanin.go
  internal/fanin/fanin_test.go
  cmd/fanindemo/main.go
```

The package exposes `Merge` (fan-in over `<-chan int`), `Generate` (a producer
used by tests), and `Square` (the upstream worker stage). The lesson's tests
exercise correctness, ordering-by-arrival, race-freedom under load, and the
zero-input case.

## Concepts

### Fan-In Is Multiplexing N Channels Into One

A fan-in function takes a slice (or variadic) of `<-chan T` and returns a
single `<-chan T`. Internally, one goroutine per input channel copies values
from that input to the shared output. The function returns immediately; the
goroutines live until every input is closed.

### The Close Must Wait For Every Writer

Sends on a closed channel panic. Closing too early kills any goroutine still
trying to send. The Go blog uses `sync.WaitGroup` for exactly this reason: each
input goroutine calls `wg.Done` when it finishes, and a separate goroutine
calls `wg.Wait` and then `close(out)`.

### `wg.Add` Must Run Before The Goroutines Start

A subtle bug: if `wg.Wait()` is called before `wg.Add`, `Wait` returns zero and
the close fires immediately. The fix is `wg.Add(len(cs))` synchronously,
followed by the `go output(c)` calls, followed by `go func() { wg.Wait();
close(out) }()`. The order is mandatory.

### Fan-In Loses Channel Order

Two upstream sources that emit `1, 2` and `10, 20` will produce output in
arrival order, which is non-deterministic across runs. Tests must sort or
deduplicate before comparing.

## Exercises

### Exercise 1: The Producer And The Upstream Stage

Create `internal/fanin/fanin.go`:

```go
package fanin

func Generate(nums ...int) <-chan int {
	out := make(chan int)
	go func() {
		defer close(out)
		for _, n := range nums {
			out <- n
		}
	}()
	return out
}

func Square(in <-chan int) <-chan int {
	out := make(chan int)
	go func() {
		defer close(out)
		for n := range in {
			out <- n * n
		}
	}()
	return out
}
```

Both are simple stages. `Square` is included so the package has a complete
producer-transformation-consumer surface; the test for fan-in does not depend
on `Square`, but the demo does.

### Exercise 2: The `Merge` Implementation

```go
package fanin

import "sync"

func Merge(cs ...<-chan int) <-chan int {
	out := make(chan int)
	var wg sync.WaitGroup

	output := func(c <-chan int) {
		defer wg.Done()
		for n := range c {
			out <- n
		}
	}

	wg.Add(len(cs))
	for _, c := range cs {
		go output(c)
	}

	go func() {
		wg.Wait()
		close(out)
	}()
	return out
}
```

This is the canonical recipe. Note that `wg.Add(len(cs))` runs before the
goroutines start. If `len(cs) == 0`, the closer still runs and closes `out`
immediately; `for v := range Merge()` returns no values.

### Exercise 3: Test The Contract

Create `internal/fanin/fanin_test.go`:

```go
package fanin

import (
	"sort"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

func TestMergeCombinesTwoSources(t *testing.T) {
	t.Parallel()

	a := Generate(1, 2, 3)
	b := Generate(10, 20, 30)

	out := Merge(a, b)

	var got []int
	for v := range out {
		got = append(got, v)
	}

	want := []int{1, 2, 3, 10, 20, 30}
	sort.Ints(got)
	if !equal(got, want) {
		t.Fatalf("got %v, want %v", got, want)
	}
}

func TestMergeEmptyInputsClosesImmediately(t *testing.T) {
	t.Parallel()

	out := Merge()
	select {
	case _, ok := <-out:
		if ok {
			t.Fatal("expected closed channel, got value")
		}
	case <-time.After(100 * time.Millisecond):
		t.Fatal("Merge() did not close")
	}
}

func TestMergeManyChannelsRaceFree(t *testing.T) {
	t.Parallel()

	const sources = 16
	const perSource = 500
	channels := make([]<-chan int, sources)
	for i := 0; i < sources; i++ {
		base := i * perSource
		nums := make([]int, perSource)
		for j := 0; j < perSource; j++ {
			nums[j] = base + j
		}
		channels[i] = Generate(nums...)
	}

	out := Merge(channels...)
	seen := make(map[int]bool)
	for v := range out {
		if seen[v] {
			t.Fatalf("duplicate value %d", v)
		}
		seen[v] = true
	}
	if len(seen) != sources*perSource {
		t.Fatalf("got %d unique, want %d", len(seen), sources*perSource)
	}
}

func TestMergeWithSlowProducerStillCompletes(t *testing.T) {
	t.Parallel()

	fast := Generate(1, 2, 3)

	slow := make(chan int)
	var counter atomic.Int64
	go func() {
		defer close(slow)
		for i := 0; i < 3; i++ {
			time.Sleep(5 * time.Millisecond)
			slow <- 100 + i
		}
	}()
	_ = counter.Load()

	out := Merge(fast, slow)
	var got []int
	for v := range out {
		got = append(got, v)
	}
	if len(got) != 6 {
		t.Fatalf("got %d values, want 6", len(got))
	}
	sort.Ints(got)
	want := []int{1, 2, 3, 100, 101, 102}
	if !equal(got, want) {
		t.Fatalf("got %v, want %v", got, want)
	}
}

func TestMergeCloseAfterAllWriters(t *testing.T) {
	t.Parallel()

	var wg sync.WaitGroup
	wg.Add(1)
	in := make(chan int)
	go func() {
		defer wg.Done()
		defer close(in)
		for i := 0; i < 50; i++ {
			in <- i
		}
	}()

	out := Merge(in)
	count := 0
	for range out {
		count++
	}
	wg.Wait()
	if count != 50 {
		t.Fatalf("count = %d, want 50", count)
	}
}

func equal(a, b []int) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if a[i] != b[i] {
			return false
		}
	}
	return true
}
```

`TestMergeWithSlowProducerStillCompletes` proves the pattern does not deadlock
when one source is slower than another: the closer waits on the slow producer
and the output channel closes only after both inputs have drained.

Your turn: add `TestMergeOfNilChannels` that asserts `Merge((<-chan int)(nil),
nil)` returns a channel that immediately closes (hint: a nil channel in a
`for ... range` loop terminates immediately, so the input goroutine calls
`wg.Done` right away).

### Exercise 4: Runnable Demo

Create `cmd/fanindemo/main.go`:

```go
package main

import (
	"fmt"

	"example.com/fanin/internal/fanin"
)

func main() {
	a := fanin.Generate(1, 2, 3)
	b := fanin.Generate(10, 20, 30)
	merged := fanin.Merge(a, b)
	out := fanin.Square(merged)
	for v := range out {
		fmt.Println(v)
	}
}
```

## Common Mistakes

### Closing The Output Channel From Inside A Worker

Wrong: each input goroutine calls `defer close(out)`.

What happens: the first goroutine to finish closes `out`; subsequent goroutines
panic on `out <- n`.

Fix: only the closer goroutine closes, after `wg.Wait`.

### Calling `wg.Wait` Before `wg.Add`

Wrong: starting the closer goroutine before `wg.Add`.

What happens: `Wait` returns zero, the closer closes the channel, and every
sender goroutine panics.

Fix: `wg.Add(len(cs))` runs synchronously, then the goroutines are spawned,
then the closer is spawned.

### Returning The Output Without A Closer

Wrong: returning `out` from `Merge` without `go func() { wg.Wait(); close(out) }()`.

What happens: `for v := range out` blocks forever after the last value.

Fix: the closer goroutine is the only path to `close(out)`.

### Assuming Order Preservation

Wrong: asserting `Merge(Generate(1,2), Generate(3,4))` emits `1, 2, 3, 4` in
that order.

What happens: goroutine scheduling decides the order; the assertion is flaky.

Fix: sort or compare as a set. Order is preserved only per-source; across
sources, it is not.

## Verification

From `~/go-exercises/fanin`:

```bash
test -z "$(gofmt -l .)"
go vet ./...
go build ./...
go test -count=1 -race ./...
```

All four must pass. The race detector is essential: without it, a missing
WaitGroup.Add before a goroutine spawn is a flaky timing bug, not a test
failure.

## Summary

- Fan-in merges N input channels into one output channel.
- One goroutine per input; a `sync.WaitGroup` tracks completion.
- A closer goroutine calls `close(out)` after `wg.Wait`.
- `wg.Add(len(cs))` runs before the goroutines spawn, never after.
- Order across sources is non-deterministic; sort or set-compare in tests.

## What's Next

Next: [Worker Pool Pattern](../04-worker-pool-pattern/04-worker-pool-pattern.md).

## Resources

- [Go Blog: Pipelines and cancellation - Fan-out, fan-in](https://go.dev/blog/pipelines)
- [Go talks: Concurrency Patterns (2012)](https://go.dev/talks/2012/concurrency.slide)
- [sync.WaitGroup](https://pkg.go.dev/sync#WaitGroup)