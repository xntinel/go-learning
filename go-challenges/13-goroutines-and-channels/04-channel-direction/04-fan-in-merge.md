# Exercise 4: Fan-In — Merge Many Shard Streams Into One

Aggregating results from N upstream shards into one stream is the fan-in pattern.
Its signature — `Merge(cs ...<-chan T) <-chan T` — is all direction: every input
is receive-only, the single output is receive-only. The hard part is closing the
merged output *exactly once*, after every input has drained.

This module is fully self-contained: its own `go mod init`, all code inline, its
own demo and tests.

## What you'll build

```text
fanin/                       independent module: example.com/fanin
  go.mod                     go 1.26
  fanin.go                   Merge[T any](cs ...<-chan T) <-chan T
  cmd/
    demo/
      main.go                runnable demo: merge three shard streams
  fanin_test.go              multiset union, output-closes, zero inputs
```

Files: `fanin.go`, `cmd/demo/main.go`, `fanin_test.go`.
Implement: generic `Merge(cs ...<-chan T) <-chan T` using one drain goroutine per input, a `sync.WaitGroup`, and a single closer goroutine.
Test: the merged output is the multiset union of all inputs (order nondeterministic), the output closes after all inputs close, zero inputs closes immediately.
Verify: `go test -count=1 -race ./...`

Set up the module:

```bash
mkdir -p ~/go-exercises/fanin/cmd/demo
cd ~/go-exercises/fanin
go mod init example.com/fanin
```

### Why the close needs a lone closer goroutine

Each input is a `<-chan T`; the output is a `chan T` internally, returned as
`<-chan T`. `Merge` launches one goroutine per input that drains that input with
`for v := range c` and forwards each value to the shared output. When an input
closes, its drain goroutine's `range` ends and the goroutine returns.

The subtlety is *when* to close the output. It must close exactly once, and only
after *every* input has drained — otherwise a consumer's `for range out` would
stop early and miss values. Two wrong constructions:

- Close the output from inside each drain goroutine: the first input to finish
  closes `out`, and every other drain goroutine then sends on a closed channel
  and panics. If two finish at once you also get a close-of-closed panic.
- Close after the *last* input in argument order: inputs finish in nondeterministic
  order, so "last in the list" is not "last to drain."

The correct construction is a `sync.WaitGroup` with one count per input; each
drain goroutine calls `wg.Done()` when its input closes; and a *single* closer
goroutine calls `wg.Wait()` and then `close(out)` once. Only that lone closer
ever closes the output, so the close is exactly-once by construction. Running the
tests under `-race` is what catches any accidental extra close.

With zero inputs, `wg.Wait()` returns immediately and the closer closes an empty
output right away — so a merge of nothing is a closed stream, which is the
correct degenerate case.

Create `fanin.go`:

```go
package fanin

import "sync"

// Merge fans in any number of receive-only input channels into a single
// receive-only output channel. It launches one drain goroutine per input and a
// single closer goroutine that closes the output exactly once, after every
// input has been fully drained.
func Merge[T any](cs ...<-chan T) <-chan T {
	out := make(chan T)
	var wg sync.WaitGroup
	wg.Add(len(cs))
	for _, c := range cs {
		go func(c <-chan T) {
			defer wg.Done()
			for v := range c {
				out <- v
			}
		}(c)
	}
	go func() {
		wg.Wait()
		close(out)
	}()
	return out
}
```

The `go func(c <-chan T)` takes `c` as a parameter. Under Go 1.22+ loop-variable
scoping the closure would also capture `c` correctly without the parameter, but
passing it makes the per-goroutine binding explicit and self-documenting.

### The runnable demo

The demo merges three shard streams. Because the interleaving of the three drain
goroutines is nondeterministic, the demo sorts the collected result before
printing so the output is stable.

Create `cmd/demo/main.go`:

```go
package main

import (
	"fmt"
	"slices"

	"example.com/fanin"
)

func shard(vals ...int) <-chan int {
	ch := make(chan int)
	go func() {
		defer close(ch)
		for _, v := range vals {
			ch <- v
		}
	}()
	return ch
}

func main() {
	merged := fanin.Merge(shard(1, 2), shard(3, 4), shard(5, 6))

	var got []int
	for v := range merged {
		got = append(got, v)
	}
	slices.Sort(got)
	fmt.Println(got)
}
```

Run it:

```bash
go run ./cmd/demo
```

Expected output:

```
[1 2 3 4 5 6]
```

### Tests

Order is nondeterministic, so the tests compare the sorted multiset union.
`TestMergeCollectsFromAllInputs` merges three inputs and asserts every value
arrives exactly once. `TestMergeClosesOutputAfterAllInputsClose` drains the merge
and then asserts a further receive reports closed. `TestMergeZeroInputsClosesImmediately`
asserts a merge of no channels is an immediately-closed stream. `-race` is
essential here to catch any double-close or send-on-closed.

Create `fanin_test.go`:

```go
package fanin

import (
	"slices"
	"testing"
	"time"
)

func shard(vals ...int) <-chan int {
	ch := make(chan int)
	go func() {
		defer close(ch)
		for _, v := range vals {
			ch <- v
		}
	}()
	return ch
}

func TestMergeCollectsFromAllInputs(t *testing.T) {
	t.Parallel()

	merged := Merge(shard(1, 2, 3), shard(4, 5), shard(6))
	var got []int
	for v := range merged {
		got = append(got, v)
	}
	slices.Sort(got)
	want := []int{1, 2, 3, 4, 5, 6}
	if !slices.Equal(got, want) {
		t.Fatalf("merged multiset = %v, want %v", got, want)
	}
}

func TestMergeClosesOutputAfterAllInputsClose(t *testing.T) {
	t.Parallel()

	merged := Merge(shard(1), shard(2))
	count := 0
	for range merged {
		count++
	}
	if count != 2 {
		t.Fatalf("received %d values, want 2", count)
	}
	select {
	case _, ok := <-merged:
		if ok {
			t.Fatal("output delivered a value after draining, want closed")
		}
	case <-time.After(time.Second):
		t.Fatal("merged output never closed")
	}
}

func TestMergeZeroInputsClosesImmediately(t *testing.T) {
	t.Parallel()

	merged := Merge[int]()
	select {
	case _, ok := <-merged:
		if ok {
			t.Fatal("zero-input merge delivered a value")
		}
	case <-time.After(time.Second):
		t.Fatal("zero-input merge never closed")
	}
}
```

## Review

The merge is correct when the output carries the exact multiset union of the
inputs and closes exactly once after all inputs drain. The zero-input and
close-after-drain tests pin the two degenerate ends. The whole design exists to
make the close exactly-once: the `sync.WaitGroup` plus lone closer is the only
construction that guarantees it, and `-race` is the tool that proves no drain
goroutine ever closed the output itself. Run `go test -race` — a stray close
shows up as a data race or a panic, not a wrong value.

## Resources

- [Go Blog: Pipelines and cancellation](https://go.dev/blog/pipelines) — the fan-in section this exercise is modeled on, including the WaitGroup-and-closer construction.
- [`sync.WaitGroup`](https://pkg.go.dev/sync#WaitGroup) — `Add`, `Done`, `Wait` semantics used to close the output exactly once.
- [Go spec: Close](https://go.dev/ref/spec#Close) — why closing an already-closed channel or sending on a closed one panics.

---

Prev: [03-batching-sink-writer.md](03-batching-sink-writer.md) | Back to [00-concepts.md](00-concepts.md) | Next: [05-tee-audit-split.md](05-tee-audit-split.md)
