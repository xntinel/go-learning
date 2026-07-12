# Exercise 1: Multiplex a Runtime-Sized Set of Replica Channels (reflect.Select)

A read-replica coordinator fires the same query at every healthy replica and takes
whichever answers first. The number of replicas is not a compile-time constant —
it grows and shrinks as replicas join and drain — so you cannot write a fixed
`select`. This module builds `First`, a dynamic-arity multiplexer over a
runtime-sized set of receive channels plus a timeout budget, using
`reflect.Select`.

This module is fully self-contained: its own `go mod init`, all code inline, its
own demo and tests. It imports no other exercise.

## What you'll build

```text
replicamux/                     module example.com/replicamux
  go.mod                        go 1.26
  replicamux.go                 First(timeout, chans...) (index, value, ok) via reflect.Select
  cmd/
    demo/
      main.go                   fire 4 replicas, one fast; print who won
  replicamux_test.go            ready-case, timeout-budget, two-ready, ExampleFirst
```

Files: `replicamux.go`, `cmd/demo/main.go`, `replicamux_test.go`.
Implement: `First(timeout time.Duration, chans ...<-chan int) (int, int, bool)` over a dynamic set of channels plus a timer case.
Test: a buffered send makes one case ready; all-empty hits the timeout budget; two-ready returns fast; an `Example` pins the happy path.
Verify: `go test -count=1 -race ./...`

Set up the module:

```bash
mkdir -p go-solutions/14-select-and-context/01-select-statement-basics/01-multiplex-first-ready/cmd/demo
cd go-solutions/14-select-and-context/01-select-statement-basics/01-multiplex-first-ready
```

## Why reflect.Select here

A static `select` bakes its cases into the source. Here the caller passes a slice
whose length is only known at runtime, so there is no static case list to write.
`reflect.Select` is the sole tool for that: you build a `[]reflect.SelectCase`, one
receive case per replica channel, append one more receive case for the timer, and
hand the whole slice to `reflect.Select`. It blocks until one case can proceed,
makes the same uniform pseudo-random choice a normal `select` would, and returns
the index of the winner.

Three details carry the correctness:

- The timer case is always appended last, so `chosen == len(chans)` unambiguously
  means "the budget elapsed, nobody answered." Any smaller index is a real
  replica, and it is the caller's index into the original slice — that is how the
  coordinator knows *which* replica won.
- `recvOK` is `reflect.Select`'s comma-ok. For a replica whose channel was closed
  (a replica that drained and shut its feed), `recvOK` is `false`; we surface that
  as the `ok` return so the caller can tell a real value from a closed source.
- `time.NewTimer` plus `defer timer.Stop()` releases the timer's runtime resources
  the instant `First` returns, whether a replica won or the budget elapsed. A
  leaked, un-stopped timer is a slow drip of memory in a hot read path.

`reflect.Select` is not free — it allocates the case slice and pays reflection
overhead, and caps at 65536 cases. That is the correct trade only because the
arity is genuinely dynamic. For a fixed two- or three-way race you would write a
plain `select`; reaching for reflection there buys nothing and loses compile-time
type checking.

Create `replicamux.go`:

```go
package replicamux

import (
	"reflect"
	"time"
)

// First fires at a runtime-sized set of replica channels and returns the index
// of the first one to deliver a value, that value, and whether the receive
// corresponded to a real send (ok == false means the channel was closed). If no
// replica answers within timeout, First returns (-1, 0, false).
//
// The channel count is only known at runtime, so First multiplexes with
// reflect.Select rather than a static select.
func First(timeout time.Duration, chans ...<-chan int) (int, int, bool) {
	if len(chans) == 0 {
		return -1, 0, false
	}

	timer := time.NewTimer(timeout)
	defer timer.Stop()

	// One receive case per replica channel, plus a trailing receive case on the
	// timer. The timer is always last so chosen == len(chans) means "timed out".
	cases := make([]reflect.SelectCase, len(chans)+1)
	for i, ch := range chans {
		cases[i] = reflect.SelectCase{
			Dir:  reflect.SelectRecv,
			Chan: reflect.ValueOf(ch),
		}
	}
	cases[len(chans)] = reflect.SelectCase{
		Dir:  reflect.SelectRecv,
		Chan: reflect.ValueOf(timer.C),
	}

	chosen, recv, recvOK := reflect.Select(cases)
	if chosen == len(chans) {
		return -1, 0, false // budget elapsed
	}
	var val int
	if recvOK {
		val = int(recv.Int())
	}
	return chosen, val, recvOK
}
```

## The runnable demo

The demo models four replicas answering at different latencies. Replica 2 is
fastest, so `First` returns its index and value while the slower replicas are still
computing.

Create `cmd/demo/main.go`:

```go
package main

import (
	"fmt"
	"time"

	"example.com/replicamux"
)

func main() {
	// Four replicas; each answers after its own latency. Buffered so a slow
	// replica's eventual send never blocks after it lost the race.
	latencies := []time.Duration{
		40 * time.Millisecond,
		25 * time.Millisecond,
		5 * time.Millisecond, // fastest
		60 * time.Millisecond,
	}
	chans := make([]<-chan int, len(latencies))
	for i, d := range latencies {
		ch := make(chan int, 1)
		chans[i] = ch
		go func(out chan<- int, delay time.Duration, replica int) {
			time.Sleep(delay)
			out <- 1000 + replica // pretend row version answered by this replica
		}(ch, d, i)
	}

	idx, val, ok := replicamux.First(100*time.Millisecond, chans...)
	if !ok {
		fmt.Println("no replica answered within budget")
		return
	}
	fmt.Printf("replica %d answered first with %d\n", idx, val)
}
```

Run with `go run ./cmd/demo`.

Expected output:

```
replica 2 answered first with 1002
```

## Tests

`TestFirstReturnsValueOnReady` pre-loads a buffered channel so exactly one receive
case is ready the moment `First` builds its case slice; `First` must return that
index and value without spinning. `TestFirstTimesOutWhenNoneReady` gives only
empty channels and asserts the call returns `ok == false` and only *after* the
budget has elapsed (with a small slack for scheduler jitter), proving the timer
case is what unblocked it. `TestFirstPrefersReadyChannel` — the promoted "your
turn" from the original lesson — mixes two ready channels among several empty ones
and asserts `First` returns promptly with one of the ready indices, proving it
never waits on the empty channels. `ExampleFirst` pins the happy path under
`go test`.

Create `replicamux_test.go`:

```go
package replicamux

import (
	"fmt"
	"testing"
	"time"
)

func TestFirstReturnsValueOnReady(t *testing.T) {
	t.Parallel()

	ch := make(chan int, 1)
	ch <- 42

	idx, val, ok := First(50*time.Millisecond, ch)
	if !ok {
		t.Fatal("First: ok = false, want true")
	}
	if idx != 0 {
		t.Fatalf("First: idx = %d, want 0", idx)
	}
	if val != 42 {
		t.Fatalf("First: val = %d, want 42", val)
	}
}

func TestFirstTimesOutWhenNoneReady(t *testing.T) {
	t.Parallel()

	const budget = 30 * time.Millisecond
	a, b := make(chan int), make(chan int)

	start := time.Now()
	idx, _, ok := First(budget, a, b)
	elapsed := time.Since(start)

	if ok {
		t.Fatal("First: ok = true with all-empty channels, want false")
	}
	if idx != -1 {
		t.Fatalf("First: idx = %d on timeout, want -1", idx)
	}
	// The timer, not a stray ready case, must be what unblocked us.
	if elapsed < budget-5*time.Millisecond {
		t.Fatalf("First returned too early (%v < %v); timer did not gate it", elapsed, budget)
	}
}

func TestFirstPrefersReadyChannel(t *testing.T) {
	t.Parallel()

	empty1, empty2, empty3 := make(chan int), make(chan int), make(chan int)
	ready1 := make(chan int, 1)
	ready2 := make(chan int, 1)
	ready1 <- 100
	ready2 <- 200

	start := time.Now()
	idx, val, ok := First(time.Second, empty1, ready1, empty2, ready2, empty3)
	elapsed := time.Since(start)

	if !ok {
		t.Fatal("First: ok = false with two ready channels, want true")
	}
	if elapsed > 20*time.Millisecond {
		t.Fatalf("First blocked on empty channels: took %v", elapsed)
	}
	// The winner is one of the two ready cases (index 1 or 3), never an empty one.
	switch {
	case idx == 1 && val == 100:
	case idx == 3 && val == 200:
	default:
		t.Fatalf("First returned idx=%d val=%d; want (1,100) or (3,200)", idx, val)
	}
}

func ExampleFirst() {
	ch := make(chan int, 1)
	ch <- 7
	idx, v, ok := First(time.Second, ch)
	fmt.Println(idx, v, ok)
	// Output: 0 7 true
}
```

## Review

`First` is correct when the returned index is a valid index into the caller's
original slice (never the timer's synthetic index leaking out as a real replica),
when `ok` faithfully distinguishes a real value from a closed source, and when the
timeout branch is the only thing that returns `(-1, 0, false)`. The timing
assertion in `TestFirstTimesOutWhenNoneReady` is the guard against a subtle
regression where an off-by-one in the case slice lets an empty channel look ready.
Keep `defer timer.Stop()` — dropping it leaks a timer per call in a hot path. And
resist the urge to "simplify" this into a static `select`: the whole reason for the
reflection cost is that the replica count is dynamic. Run `go test -race` to
confirm the concurrent sends in the demo-shaped tests are clean.

## Resources

- [reflect.Select and reflect.SelectCase](https://pkg.go.dev/reflect#Select) — the dynamic-arity select, `recvOK`, and the 65536-case cap.
- [Go Specification: Select statements](https://go.dev/ref/spec#Select_statements) — the uniform pseudo-random choice that `reflect.Select` mirrors.
- [time.NewTimer and Timer.Stop](https://pkg.go.dev/time#NewTimer) — releasing timer resources with `defer timer.Stop()`.

---

Back to [00-concepts.md](00-concepts.md) | Next: [02-fan-in-merge.md](02-fan-in-merge.md)
