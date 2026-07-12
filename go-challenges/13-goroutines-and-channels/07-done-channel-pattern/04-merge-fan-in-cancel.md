# Exercise 4: Fan-In Several Streams and Abandon Them Early on Cancel

Fan-in merges several input channels into one output. It is the canonical pattern from the
Go pipelines blog, and it is where the leak-avoidance rule earns its place: each forwarder
goroutine must select its send against `done`, or a consumer that stops reading early leaks
every forwarder. The real scenario is merging results from several sharded database queries
into one stream where the caller may only need the first K rows and then walk away.

## What you'll build

```text
mergefanin/                        independent module: example.com/mergefanin
  go.mod
  merge.go                         Merge(done, cs ...<-chan int) <-chan int; per-input forwarder + WaitGroup close
  cmd/
    demo/
      main.go                      runnable demo: merge three streams, sum the union
  merge_test.go                    combines-all and abandon-early (no-leak) tests; -race
```

Files: `merge.go`, `cmd/demo/main.go`, `merge_test.go`.
Implement: `Merge(done <-chan struct{}, cs ...<-chan int) <-chan int` with one forwarder goroutine per input, each using `select { case out <- v: case <-done: return }`, and a `sync.WaitGroup` that closes `out` once all forwarders finish.
Test: merging three inputs yields the union of their values; reading a few values from an infinite merge and closing `done` lets every forwarder exit (proven by `out` closing).
Verify: `go test -count=1 -race ./...`

Set up the module:

```bash
mkdir -p go-solutions/13-goroutines-and-channels/07-done-channel-pattern/04-merge-fan-in-cancel/cmd/demo
cd go-solutions/13-goroutines-and-channels/07-done-channel-pattern/04-merge-fan-in-cancel
```

### The forwarder and the guarded send

`Merge` starts one goroutine per input channel. Each forwarder ranges over its input and
copies every value to the shared `out`. The load-bearing detail is the send:

```go
for v := range c {
	select {
	case out <- v:
	case <-done:
		return
	}
}
```

Not `out <- v`. Consider an infinite producer feeding one of the inputs and a consumer that
reads three values from the merged stream and then stops. With a bare send, the forwarder for
that input blocks forever on `out <- v` the moment the consumer stops receiving — the
goroutine is leaked, and so is the producer behind it. The `select` against `done` gives the
forwarder an exit: when the caller signals cancellation, the blocked send loses to `<-done`
and the forwarder returns. Every fan-in forwarder needs this guard; it is the difference
between a merge that cleans up on early cancellation and one that leaks a goroutine per input.

### Who closes out

There are N forwarders all writing to `out`, so no single forwarder may close it — that would
race the others' sends. A `sync.WaitGroup` counts the forwarders; each calls `wg.Done()` on
return; a separate closer goroutine runs `wg.Wait(); close(out)`. The merged channel is closed
exactly once, after the last forwarder has provably returned — whether they returned because
their inputs drained or because `done` fired. The consumer ranging over the merged channel
then always sees a clean close.

Create `merge.go`:

```go
package mergefanin

import "sync"

// Merge fans several input channels into one. Each value from every input is
// forwarded to the returned channel, which is closed once every input is
// drained or done is closed. A forwarder never blocks forever on the output:
// its send is selected against done, so a consumer that stops reading early
// does not leak the forwarders. done is receive-only here.
func Merge(done <-chan struct{}, cs ...<-chan int) <-chan int {
	out := make(chan int)
	var wg sync.WaitGroup
	wg.Add(len(cs))
	for _, c := range cs {
		go func(c <-chan int) {
			defer wg.Done()
			for v := range c {
				select {
				case out <- v:
				case <-done:
					return
				}
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

Note the explicit `func(c <-chan int)` parameter: even under Go 1.22+ loop-variable scoping,
passing `c` as an argument is the clearest way to bind each forwarder to its own input, and it
documents the intent.

### The runnable demo

Create `cmd/demo/main.go`:

```go
package main

import (
	"fmt"

	"example.com/mergefanin"
)

func gen(nums ...int) <-chan int {
	c := make(chan int, len(nums))
	for _, n := range nums {
		c <- n
	}
	close(c)
	return c
}

func main() {
	done := make(chan struct{})
	defer close(done)

	merged := mergefanin.Merge(done, gen(1, 2, 3), gen(10, 20), gen(100))

	sum := 0
	for v := range merged {
		sum += v
	}
	fmt.Println("union sum:", sum)
}
```

Run it:

```bash
go run ./cmd/demo
```

Expected output:

```
union sum: 136
```

### Tests

`TestMergeCombinesAll` merges three finite inputs and asserts the merged stream is the union of
their values (collected into a multiset, since fan-in order across goroutines is
nondeterministic). `TestMergeStopsOnDone` is the leak test: it merges two *infinite* producers
(each a well-behaved upstream that watches `done` and closes its own channel on cancel), reads a few
values, stops reading, closes `done`, and then proves every forwarder exited by draining the merged
channel until it closes. At the moment `done` is closed the forwarders are blocked trying to send to
a consumer that has stopped receiving — only the guarded send rescues them. The merged channel closes
only after `wg.Wait()` unblocks, which happens only when every forwarder has returned; so a regression
to a bare `out <- v` send would leave a forwarder stuck and the channel would never close, hanging the
test. That hang *is* the detection of the leak.

Create `merge_test.go`:

```go
package mergefanin

import (
	"sort"
	"testing"
)

func gen(nums ...int) <-chan int {
	c := make(chan int, len(nums))
	for _, n := range nums {
		c <- n
	}
	close(c)
	return c
}

func TestMergeCombinesAll(t *testing.T) {
	t.Parallel()

	done := make(chan struct{})
	defer close(done)

	merged := Merge(done, gen(1, 2, 3), gen(10, 20), gen(100))

	var got []int
	for v := range merged {
		got = append(got, v)
	}
	sort.Ints(got)

	want := []int{1, 2, 3, 10, 20, 100}
	if len(got) != len(want) {
		t.Fatalf("got %d values %v, want %d %v", len(got), got, len(want), want)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Fatalf("got %v, want %v", got, want)
		}
	}
}

func TestMergeStopsOnDone(t *testing.T) {
	t.Parallel()

	done := make(chan struct{})

	// An infinite producer: it will block on its send forever unless the
	// forwarder guards the send against done.
	infinite := func() <-chan int {
		c := make(chan int)
		go func() {
			defer close(c) // a well-behaved upstream closes on cancel
			i := 0
			for {
				select {
				case c <- i:
					i++
				case <-done:
					return
				}
			}
		}()
		return c
	}

	merged := Merge(done, infinite(), infinite())

	// Consume a few values, then abandon the stream.
	for range 5 {
		<-merged
	}
	close(done)

	// Drain until closed. This terminates only if every forwarder returned,
	// which requires the guarded send. A bare send would leak and hang here.
	for range merged {
	}
}
```

## Review

The merge is correct when it forwards the union of all inputs and when closing `done` unwinds
every forwarder no matter what they were doing. The combines-all test uses a multiset assertion
because merge order is inherently nondeterministic; asserting a specific interleaving would be a
broken test. The stops-on-done test is the real proof: it can only terminate if the guarded send
let every forwarder exit, so a regression to `out <- v` turns it into a hang, which is exactly how
you want a leak to surface in CI. The WaitGroup-gated close is what makes closing `out` safe with
N writers — never close a fan-in output from a forwarder. Run `go test -race`; the WaitGroup
ordering means the close never races a send.

## Resources

- [Go Blog: Go Concurrency Patterns — Pipelines and cancellation](https://go.dev/blog/pipelines)
- [pkg.go.dev: sync.WaitGroup](https://pkg.go.dev/sync#WaitGroup)
- [Go Language Spec: Select statements](https://go.dev/ref/spec#Select_statements)

---

Back to [00-concepts.md](00-concepts.md) | Previous: [03-fanout-pool-shutdown.md](03-fanout-pool-shutdown.md) | Next: [05-pipeline-stage-cancellation.md](05-pipeline-stage-cancellation.md)
