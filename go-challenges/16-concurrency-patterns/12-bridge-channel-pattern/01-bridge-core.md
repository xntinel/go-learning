# Exercise 1: The Bridge Channel

The bridge is one small generic function, but every property the pattern promises lives inside it: it flattens `<-chan (<-chan T)` into `<-chan T`, it preserves order as a deterministic concatenation, it is cancellable through an explicit `done`, and it survives a nil inner channel. This exercise builds that function together with a tiny helper that turns a slice of channels into a stream of channels, then pins all four properties with a race-tested suite.

This module is fully self-contained. It begins with its own `go mod init`, defines every type it needs, and ships its own demo and tests. Nothing here imports any other exercise.

## What you'll build

```text
bridge.go            Bridge[T] (the flattener), ChanStream[T] (slice -> stream of channels)
cmd/
  demo/
    main.go          bridge three streams into one and print the flattened sequence
bridge_test.go       order, single empty stream, cancellation terminates, nil inner skipped, race
```

- Files: `bridge.go`, `cmd/demo/main.go`, `bridge_test.go`.
- Implement: `Bridge[T any](done <-chan struct{}, chanStream <-chan (<-chan T)) <-chan T` and `ChanStream[T any](streams ...<-chan T) <-chan (<-chan T)`.
- Test: `bridge_test.go` checks the flattened order is exact concatenation, that an empty stream of streams closes the output, that closing `done` terminates the bridge, that a nil inner channel is skipped, and that a high-volume run is race-clean.
- Verify: `go test -run 'TestBridge' -race ./...`

Set up the module:

```bash
mkdir -p go-solutions/16-concurrency-patterns/12-bridge-channel-pattern/01-bridge-core/cmd/demo && cd go-solutions/16-concurrency-patterns/12-bridge-channel-pattern/01-bridge-core
```

### Why two selects and an inner range, and why a nil guard

The bridge is a single goroutine that owns the output channel — it is the only sender on `out`, which is why it is also the only thing allowed to close it, and why `defer close(out)` is the first line inside the goroutine. Everything after that is a state machine with two blocking points, and each blocking point must be escapable through `done`.

The outer `select` waits for the next inner channel. It has three cases: `done` fired (return, and the deferred close fires), the outer channel closed (`ok` is false, return), or a new inner channel arrived. Once an inner channel is in hand the bridge enters the inner `for v := range stream` loop, which drains that channel to exhaustion — this inner loop is the flattening, and draining fully before returning to the outer `select` is what makes the output a deterministic concatenation rather than an interleave. Each forward inside the inner loop is itself a `select` on `done` and `out <- v`, because a consumer that closes `done` while the bridge is parked mid-forward must still unblock it; without that second `done` case the goroutine leaks exactly when the consumer gives up.

The `if stream == nil { continue }` guard is not defensive ceremony. A receive on a nil channel blocks forever, so `for v := range stream` on a nil `stream` would hang the bridge permanently and starve every later inner channel. The language does not skip a nil channel for you in a `range`; the guard is the only thing that turns "a nil arrived on the outer channel" into "move on to the next one" instead of "deadlock."

Create `bridge.go`:

```go
package bridge

// Bridge flattens a stream of channels into a single channel. Each value
// received on chanStream is itself a <-chan T; Bridge drains the current inner
// channel to exhaustion, then receives the next one, copying every value onto
// the returned output channel in deterministic concatenation order.
//
// The single owning goroutine is the only sender on out and therefore the only
// thing that closes it. Closing done terminates the bridge from either blocking
// point. A nil inner channel is skipped rather than ranged over, because a
// receive on a nil channel would block forever.
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

// ChanStream adapts a fixed slice of channels into the stream-of-channels shape
// Bridge consumes. The producer goroutine sends each channel in order, then
// closes the outer channel so the bridge sees a clean end. It selects on done so
// it cannot leak if the consumer cancels before every inner channel is taken.
func ChanStream[T any](done <-chan struct{}, streams ...<-chan T) <-chan (<-chan T) {
	out := make(chan (<-chan T))
	go func() {
		defer close(out)
		for _, s := range streams {
			select {
			case out <- s:
			case <-done:
				return
			}
		}
	}()
	return out
}
```

`ChanStream` exists only to make test and demo data ergonomic, but it is built to the same discipline as the bridge: it closes its outer channel after the last send so the bridge terminates cleanly, and it selects on `done` so a cancellation mid-registration cannot strand its goroutine. In the senior exercises the outer channel is fed by a live registrar instead of a slice, but the contract it must honor — send inner channels, then close — is identical.

### The runnable demo

The demo makes the concatenation visible: three streams with distinct value ranges, bridged into one, printed in arrival order. Because the bridge drains each inner channel fully before the next, the output is the three ranges back to back, never interleaved.

Create `cmd/demo/main.go`:

```go
package main

import (
	"fmt"

	"example.com/bridge"
)

// stream returns a channel that emits vals and closes, the shape of one inner
// channel on the bridge's outer channel.
func stream(vals ...int) <-chan int {
	c := make(chan int, len(vals))
	for _, v := range vals {
		c <- v
	}
	close(c)
	return c
}

func main() {
	done := make(chan struct{})
	defer close(done)

	a := stream(1, 2, 3)
	b := stream(10, 20)
	c := stream(100, 200, 300)

	for v := range bridge.Bridge(done, bridge.ChanStream(done, a, b, c)) {
		fmt.Println(v)
	}
}
```

Run it:

```bash
go run ./cmd/demo
```

Expected output:

```
1
2
3
10
20
100
200
300
```

### Tests

The suite pins the four properties separately. `TestBridgeFlattensInConcatenationOrder` asserts the exact concatenated order with no sorting, because the order is deterministic and sorting would mask a flattening bug. `TestBridgeWithSingleEmptyStream` proves an empty stream of streams closes the output rather than hanging. `TestBridgeStopsOnDone` drives an *unbounded* generator and proves that closing `done` makes the output channel close — the cancellation property, verified by the fact that the `range` terminates at all instead of running forever. `TestBridgeSkipsNilInnerChannel` puts a nil channel between two real ones and asserts every real value still arrives, exercising the guard. `TestBridgeIsRaceFree` pushes many values through many streams under `-race`.

Create `bridge_test.go`:

```go
package bridge

import (
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

func TestBridgeFlattensInConcatenationOrder(t *testing.T) {
	t.Parallel()

	a := makeIntChan(1, 2, 3)
	b := makeIntChan(10, 20)
	c := makeIntChan(100, 200, 300)

	done := make(chan struct{})
	defer close(done)

	var got []int
	for v := range Bridge(done, ChanStream(done, a, b, c)) {
		got = append(got, v)
	}

	want := []int{1, 2, 3, 10, 20, 100, 200, 300}
	if !equal(got, want) {
		t.Fatalf("got %v, want %v", got, want)
	}
}

func TestBridgeWithSingleEmptyStream(t *testing.T) {
	t.Parallel()

	done := make(chan struct{})
	defer close(done)

	out := Bridge(done, ChanStream[int](done))
	select {
	case _, ok := <-out:
		if ok {
			t.Fatal("expected closed channel, got a value")
		}
	case <-time.After(time.Second):
		t.Fatal("Bridge did not close on an empty stream of streams")
	}
}

func TestBridgeStopsOnDone(t *testing.T) {
	t.Parallel()

	done := make(chan struct{})

	// An unbounded generator that itself honors done: if the bridge ignored
	// done, the range below would never terminate and the test would time out.
	ints := func(start int) <-chan int {
		c := make(chan int)
		go func() {
			defer close(c)
			for i := start; ; i++ {
				select {
				case c <- i:
				case <-done:
					return
				}
			}
		}()
		return c
	}

	out := Bridge(done, ChanStream(done, ints(0)))

	got := 0
	for v := range out {
		got++
		if v >= 4 {
			close(done)
		}
	}
	// Reaching here at all proves cancellation closed the output; if it did not,
	// the range above would block forever.
	if got < 5 {
		t.Fatalf("read %d values before cancel, want at least 5", got)
	}
}

func TestBridgeSkipsNilInnerChannel(t *testing.T) {
	t.Parallel()

	a := makeIntChan(1, 2)
	var nilCh <-chan int // nil: ranging it would block forever without the guard
	b := makeIntChan(3, 4)

	done := make(chan struct{})
	defer close(done)

	var got []int
	for v := range Bridge(done, ChanStream(done, a, nilCh, b)) {
		got = append(got, v)
	}

	want := []int{1, 2, 3, 4}
	if !equal(got, want) {
		t.Fatalf("got %v, want %v", got, want)
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

	var got []string
	for v := range Bridge(done, ChanStream(done, a, b)) {
		got = append(got, v)
	}

	want := []string{"alpha", "beta", "gamma", "delta"}
	if len(got) != len(want) {
		t.Fatalf("got %v, want %v", got, want)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Fatalf("got %v, want %v", got, want)
		}
	}
}

func TestBridgeIsRaceFree(t *testing.T) {
	t.Parallel()

	const streams = 8
	const perStream = 100

	all := make([]<-chan int, streams)
	for i := range streams {
		base := i * perStream
		c := make(chan int, perStream)
		for j := range perStream {
			c <- base + j
		}
		close(c)
		all[i] = c
	}

	done := make(chan struct{})
	defer close(done)

	seen := make(map[int]bool, streams*perStream)
	for v := range Bridge(done, ChanStream(done, all...)) {
		if seen[v] {
			t.Errorf("duplicate value %d", v)
		}
		seen[v] = true
	}
	if len(seen) != streams*perStream {
		t.Fatalf("got %d unique values, want %d", len(seen), streams*perStream)
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

## Review

The bridge is correct when all four properties hold at once: the flattened output is the exact concatenation of the inner channels in outer-channel order with no sorting needed, an empty stream of streams closes the output instead of hanging, closing `done` terminates the `range` (proving cancellation reaches both selects), and a nil inner channel is skipped rather than deadlocked on. The volume test passing under `-race` is what certifies the output handoff and the producer goroutines share nothing without a happens-before edge.

Common mistakes for this feature. Sorting the output before comparing it, as a first draft often does, hides the difference between a real flatten and a broken one — assert the concatenation order directly. Asserting an exact *count* of values delivered before a cancel is a flake, not a test: once `done` is closed, the bridge's forward `select` and the consumer's receive are both ready and the scheduler may hand over one more value before the bridge sees `done`, so the count is racy by construction; assert that the output *closes* instead. Dropping the `done` case from the inner forward leaks the goroutine exactly when the consumer cancels mid-stream. And assuming a nil inner channel is skipped for free turns one nil on the outer channel into a permanent deadlock; the explicit guard is mandatory.

## Resources

- [Go Concurrency Patterns: Pipelines and cancellation](https://go.dev/blog/pipelines) — the `done`-channel cancellation discipline this bridge threads through both of its selects.
- [Advanced Go Concurrency Patterns](https://go.dev/blog/advanced-go-concurrency-patterns) — the talk that popularized flattening a sequence of channels into one consumer stream.
- [The Go Programming Language Specification: Receive operator](https://go.dev/ref/spec#Receive_operator) — the rule that a receive on a nil channel blocks forever, which is exactly why the bridge needs its nil guard.

---

Back to [00-concepts.md](00-concepts.md) | Next: [02-connection-stream-bridge.md](02-connection-stream-bridge.md)
