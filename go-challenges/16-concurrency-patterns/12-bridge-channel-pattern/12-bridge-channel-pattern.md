# 12. Bridge Channel Pattern: Flatten `<-chan <-chan T>` Into `<-chan T`

A bridge converts a channel of channels into a single channel. The input is
`<-chan <-chan T>`: each value on the inbound is itself a channel. The output
is `<-chan T`: every value emitted on every input channel, in arrival order.
The pattern lets callers iterate over a stream of streams without writing
nested `for` loops.

```text
bridge/
  go.mod
  internal/bridge/bridge.go
  internal/bridge/bridge_test.go
  cmd/bridgedemo/main.go
```

The package exposes `Bridge` and a small helper that builds a channel of
channels from a slice. The test exercises single-stream, multi-stream,
cancellation, and race-freedom.

## Concepts

### Bridge Flattens Stream Of Streams

`chanStream()` returns `<-chan (<-chan T)`. Each value on that channel is a
`<-chan T` that the consumer must read to exhaustion before moving to the
next. The bridge does this automatically: when the current inner channel
closes, the bridge receives the next outer value and starts reading from
that inner channel.

### The Outer Channel Owns Lifecycle

When the outer channel closes, the bridge has no more inner channels to
drain, so it closes the output. Cancellation requires a `done` channel
because reading from a closed inner channel is the only signal for "move on
to the next outer value".

### Generics Make The Bridge Type-Agnostic

`Bridge[T any](done <-chan struct{}, chanStream <-chan (<-chan T)) <-chan T`
is the natural shape. The same code works for `int`, `string`, or a custom
type without duplicating the loop.

### Without A Bridge The Caller Writes Nested Loops

The naive version:

```go
for inner := range outer {
    for v := range inner {
        handle(v)
    }
}
```

works, but every consumer has to repeat it. A bridge turns the nesting into
a single `for v := range Bridge(done, outer)` loop.

## Exercises

Set up the module:

```bash
mkdir -p ~/go-exercises/bridge/internal/bridge ~/go-exercises/bridge/cmd/bridgedemo
cd ~/go-exercises/bridge
go mod init example.com/bridge
```

### Exercise 1: The Bridge

Create `internal/bridge/bridge.go`:

```go
package bridge

func Bridge[T any](done <-chan struct{}, chanStream <-chan (<-chan T)) <-chan T {
	out := make(chan T)
	go func() {
		defer close(out)
		for {
			var stream <-chan T
			select {
			case <-done:
				return
			case s, ok := <-chanStream:
				if !ok {
					return
				}
				stream = s
			}
			if stream == nil {
				continue
			}
			for v := range stream {
				select {
				case <-done:
					return
				case out <- v:
				}
			}
		}
	}()
	return out
}
```

The outer loop receives the next inner channel from `chanStream`. The inner
loop drains that inner channel. When the inner channel closes, control
returns to the outer loop for the next inner channel. If `done` fires at
any point, the bridge exits.

### Exercise 2: Building A Channel Of Channels

A small helper makes the test data easy to construct:

```go
package bridge

func ChanStream[T any](streams ...<-chan T) <-chan (<-chan T) {
	out := make(chan (<-chan T))
	go func() {
		defer close(out)
		for _, s := range streams {
			out <- s
		}
	}()
	return out
}
```

The producer closes `out` after the last send. The bridge sees the close
and exits cleanly.

### Exercise 3: Test The Contract

Create `internal/bridge/bridge_test.go`:

```go
package bridge

import (
	"sort"
	"sync"
	"testing"
	"time"
)

func makeIntChan(vals ...int) <-chan int {
	c := make(chan int, len(vals))
	for _, v := range vals {
		c <- v
	}
	close(c)
	return c
}

func TestBridgeForwardsAllValuesAcrossStreams(t *testing.T) {
	t.Parallel()

	a := makeIntChan(1, 2, 3)
	b := makeIntChan(10, 20)
	c := makeIntChan(100, 200, 300)

	done := make(chan struct{})
	defer close(done)

	out := Bridge(done, ChanStream(a, b, c))

	var got []int
	for v := range out {
		got = append(got, v)
	}
	sort.Ints(got)
	want := []int{1, 2, 3, 10, 20, 100, 200, 300}
	if !equal(got, want) {
		t.Fatalf("got %v, want %v", got, want)
	}
}

func TestBridgeWithSingleEmptyStream(t *testing.T) {
	t.Parallel()

	done := make(chan struct{})
	defer close(done)

	out := Bridge(done, ChanStream[int]())
	select {
	case _, ok := <-out:
		if ok {
			t.Fatal("expected closed channel")
		}
	case <-time.After(50 * time.Millisecond):
		t.Fatal("Bridge did not close on empty stream")
	}
}

func TestBridgeStopsOnDone(t *testing.T) {
	t.Parallel()

	// Each inner channel delivers one value; we cancel after the first outer
	// stream finishes draining.
	a := makeIntChan(1, 2, 3)
	b := makeIntChan(10, 20, 30)
	c := makeIntChan(100, 200, 300)

	done := make(chan struct{})
	out := Bridge(done, ChanStream(a, b, c))

	var got []int
	for v := range out {
		got = append(got, v)
		if len(got) == 3 {
			close(done)
		}
	}
	if len(got) != 3 {
		t.Fatalf("got %d values, want 3", len(got))
	}
}

func TestBridgePreservesOrderWithinStream(t *testing.T) {
	t.Parallel()

	c := make(chan int, 5)
	c <- 5
	c <- 4
	c <- 3
	c <- 2
	c <- 1
	close(c)

	done := make(chan struct{})
	defer close(done)

	out := Bridge(done, ChanStream(c))
	got := []int{}
	for v := range out {
		got = append(got, v)
	}
	want := []int{5, 4, 3, 2, 1}
	if !equal(got, want) {
		t.Fatalf("got %v, want %v", got, want)
	}
}

func TestBridgeIsRaceFree(t *testing.T) {
	t.Parallel()

	const streams = 8
	const perStream = 100
	all := make([]<-chan int, streams)
	for i := 0; i < streams; i++ {
		base := i * perStream
		c := make(chan int, perStream)
		for j := 0; j < perStream; j++ {
			c <- base + j
		}
		close(c)
		all[i] = c
	}

	done := make(chan struct{})
	defer close(done)

	out := Bridge(done, ChanStream(all...))

	seen := make(map[int]bool, streams*perStream)
	var mu sync.Mutex
	for v := range out {
		mu.Lock()
		if seen[v] {
			t.Errorf("duplicate %d", v)
		}
		seen[v] = true
		mu.Unlock()
	}
	if len(seen) != streams*perStream {
		t.Fatalf("got %d unique, want %d", len(seen), streams*perStream)
	}
}

func TestBridgeWithStrings(t *testing.T) {
	t.Parallel()

	a := make(chan string, 2)
	a <- "alpha"
	a <- "beta"
	close(a)

	b := make(chan string, 2)
	b <- "gamma"
	b <- "delta"
	close(b)

	done := make(chan struct{})
	defer close(done)

	out := Bridge(done, ChanStream(a, b))
	var got []string
	for v := range out {
		got = append(got, v)
	}
	want := []string{"alpha", "beta", "gamma", "delta"}
	if !equalStrings(got, want) {
		t.Fatalf("got %v, want %v", got, want)
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

func equalStrings(a, b []string) bool {
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

Your turn: add `TestBridgeRespectsNilInnerChannel` that passes a nil channel
as one of the streams and asserts the bridge continues to deliver values
from the non-nil streams. Hint: a nil channel in `for v := range` terminates
immediately, which is exactly what you want here.

### Exercise 4: Runnable Demo

Create `cmd/bridgedemo/main.go`:

```go
package main

import (
	"fmt"

	"example.com/bridge/internal/bridge"
)

func stream(name string, vals ...int) <-chan int {
	c := make(chan int, len(vals))
	go func() {
		defer close(c)
		for _, v := range vals {
			c <- v
		}
	}()
	_ = name
	return c
}

func main() {
	done := make(chan struct{})
	defer close(done)

	a := stream("a", 1, 2, 3)
	b := stream("b", 10, 20)
	c := stream("c", 100, 200, 300)

	for v := range bridge.Bridge(done, bridge.ChanStream(a, b, c)) {
		fmt.Println(v)
	}
}
```

## Common Mistakes

### Forgetting The Outer Done Case

Wrong: outer select only handles `case s := <-chanStream`.

What happens: if `done` fires while waiting for the next inner channel, the
bridge blocks forever.

Fix: include `case <-done: return` in the outer select and the inner send
select.

### Reading The Inner Channel Without Selecting On Done

Wrong: `for v := range stream { out <- v }`.

What happens: if `done` fires, the wrapper is blocked on `out <- v` and the
goroutine leaks.

Fix: wrap the send in a `select` with `<-done`.

### Treating The Inner Channel As A Single Value

Wrong: `Bridge` reads one value from the inner channel and moves on.

What happens: only the first value of each inner channel is delivered.

Fix: range over the inner channel until it closes, then move to the next
outer value.

### Forgetting To Close The Output

Wrong: omitting `defer close(out)`.

What happens: the consumer's `for v := range out` hangs forever after the
last value.

Fix: `defer close(out)` covers every return path including the cancellation
branch.

## Verification

From `~/go-exercises/bridge`:

```bash
test -z "$(gofmt -l .)"
go vet ./...
go build ./...
go test -count=1 -race ./...
```

All four must pass. The race detector is essential: the bridge touches the
output channel from a single goroutine, but the consumer may close `done`
concurrently, and the race detector pins that handshake.

## Summary

- `Bridge` flattens `<-chan (<-chan T)` into `<-chan T`.
- The outer loop receives inner channels; the inner loop drains them.
- Both selects must include `<-done` so cancellation is effective.
- The output closes when the outer channel closes or `done` fires.
- Generics make one `Bridge` work for any element type.

## What's Next

Next: [Rate Limiter Token Bucket](../13-rate-limiter-token-bucket/13-rate-limiter-token-bucket.md).

## Resources

- [Go talks: Advanced Go Concurrency Patterns](https://go.dev/blog/advanced-go-concurrency-patterns)
- [Go Blog: Pipelines and cancellation](https://go.dev/blog/pipelines)
- [golang.org/x/sync/errgroup](https://pkg.go.dev/golang.org/x/sync/errgroup)