# Exercise 1: The Or-Done Wrapper

This exercise builds the wrapper itself: a function that takes a caller-owned
`done` signal and a source channel and returns a new channel that forwards
every value until the source closes or `done` fires, then closes. You build
both a typed generic form, which is what real code uses, and an `any`-typed
form that shows the mechanism without generics, and you pin the contract with
tests that exercise every exit path under the race detector.

This module is fully self-contained. It starts with its own `go mod init`,
defines everything it needs, and ships its own demo and tests. Nothing here
imports any other exercise.

## What you'll build

```text
ordone.go            OrDone (any), OrDoneT[T] (generic): the double-select forwarder
cmd/
  demo/
    main.go          forward a slow producer, cancel it mid-stream, count delivered
ordone_test.go       source-drains, done-mid-stream, done-already-closed,
                     done-before-any-value, race-under-load, drains-then-closes
```

- Files: `ordone.go`, `cmd/demo/main.go`, `ordone_test.go`.
- Implement: `OrDoneT[T any](done <-chan struct{}, in <-chan T) <-chan T` and the
  `any`-typed `OrDone(done <-chan struct{}, in <-chan any) <-chan any`.
- Test: `ordone_test.go` covers natural drain, cancel mid-stream, cancel before
  the first value, an already-closed `done`, and race-freedom under load.
- Verify: `go test -count=1 -race ./...`

Set up the module:

```bash
mkdir -p go-solutions/16-concurrency-patterns/10-or-done-channel-pattern/01-or-done-wrapper/cmd/demo && cd go-solutions/16-concurrency-patterns/10-or-done-channel-pattern/01-or-done-wrapper
```

### Why two selects, and why the output is always closed

The forwarder runs in one goroutine and must satisfy three properties at once:
it forwards every value the consumer is still willing to take, it stops promptly
when `done` fires, and it never leaves a goroutine blocked. The structure that
delivers all three is two nested selects sharing a single `defer close(out)`.

The outer select waits for whichever comes first: a done signal or a value from
the source. If `done` wins, the goroutine returns and the deferred close fires.
If a value arrives, the second component of the receive — the `ok` boolean —
distinguishes a real value from a closed-and-drained source; on `!ok` the
goroutine returns and again the close fires. So both natural termination and
early cancel funnel through the same `defer`, which is what lets a downstream
`for v := range out` end cleanly regardless of why the stream stopped.

The inner select is the send guard, and it is the difference between a correct
forwarder and a leak. Having received `v`, the goroutine owes it to `out`, but
the consumer may already be gone and `done` may already be closed. A bare
`out <- v` would block forever against an absent reader. Wrapping the send in
`select { case out <- v: case <-done: return }` means the goroutine always
makes progress: it either delivers `v` to a live consumer or drops it and exits
because the caller cancelled. Dropping the in-flight value on cancel is the
designed behavior — for a cancellable stream it is harmless, and the later
subscription exercise shows the loss-free discipline for paths where it is not.

The typed form `OrDoneT[T]` is what production code uses: it forwards `T`
directly with no boxing. The `any` form is identical in shape and exists to
show that the pattern predates and does not require generics; it pays an
`interface{}` boxing cost per value, which is why you reach for the typed form
unless you genuinely need a heterogeneous stream.

Create `ordone.go`:

```go
// Package ordone forwards values from a source channel to a new output channel
// until the source closes or a caller-owned done signal fires.
package ordone

// OrDoneT forwards every value from in to the returned channel until in is
// closed and drained or done fires, then closes the returned channel.
//
// The caller owns done and is the only party allowed to close it; OrDoneT only
// reads from it. The returned channel is owned by OrDoneT, which closes it on
// every exit path. If done fires while a value is in flight, that value may be
// dropped without delivery; use this only where dropping the tail on cancel is
// acceptable.
func OrDoneT[T any](done <-chan struct{}, in <-chan T) <-chan T {
	out := make(chan T)
	go func() {
		defer close(out)
		for {
			select {
			case <-done:
				return
			case v, ok := <-in:
				if !ok {
					return
				}
				select {
				case out <- v:
				case <-done:
					return
				}
			}
		}
	}()
	return out
}

// OrDone is the any-typed form of OrDoneT, kept for streams that genuinely carry
// heterogeneous values. It boxes each value into an interface and is otherwise
// identical to OrDoneT.
func OrDone(done <-chan struct{}, in <-chan any) <-chan any {
	out := make(chan any)
	go func() {
		defer close(out)
		for {
			select {
			case <-done:
				return
			case v, ok := <-in:
				if !ok {
					return
				}
				select {
				case out <- v:
				case <-done:
					return
				}
			}
		}
	}()
	return out
}
```

`OrDoneT` and `OrDone` are line-for-line identical except for the channel
element type. Read them as one function: the deferred close is the first thing
the goroutine arranges, the outer select races the source against `done`, the
`ok` check turns a closed source into a clean return, and the inner select makes
the send abandonable. There is no shared state between the goroutine and the
caller other than the two channels, so the only synchronization is the channel
operations themselves — which is what makes the wrapper race-free by
construction.

### The runnable demo

The demo makes the cancel path concrete. A producer sends integers on an
unbuffered channel every few milliseconds; a separate goroutine closes `done`
after a fixed delay; the main goroutine ranges over the wrapped channel and
counts what it receives. Because everything is timed, the demo prints a fixed,
explanatory summary rather than a value-by-value dump.

Create `cmd/demo/main.go`:

```go
package main

import (
	"fmt"
	"time"

	"example.com/ordone"
)

func main() {
	// Producer: emit 0..49 on an unbuffered channel, one every 4ms, and stop
	// if nobody is reading. The select on done is what keeps the producer from
	// leaking once the wrapper has been cancelled.
	done := make(chan struct{})
	src := make(chan int)
	go func() {
		defer close(src)
		for i := 0; i < 50; i++ {
			select {
			case src <- i:
			case <-done:
				return
			}
			time.Sleep(4 * time.Millisecond)
		}
	}()

	// Cancel after ~30ms: enough for a handful of values to flow.
	go func() {
		time.Sleep(30 * time.Millisecond)
		close(done)
	}()

	count := 0
	for range ordone.OrDoneT(done, src) {
		count++
	}

	fmt.Println("source did not finish (cancelled early):", count < 50)
	fmt.Println("delivered at least one value:", count > 0)
}
```

Run it:

```bash
go run ./cmd/demo
```

Expected output:

```
source did not finish (cancelled early): true
delivered at least one value: true
```

### Tests

The tests pin every exit path. `TestForwardsUntilSourceCloses` proves the happy
path: a buffered, pre-closed source is forwarded in full. `TestStopsOnDone`
closes `done` mid-stream and asserts the wrapper stops after at most one extra
in-flight value. `TestStopsImmediatelyIfDoneAlreadyClosed` and
`TestDoneFiresBeforeAnyValue` cover the two zero-value cancel cases — `done`
already closed before the wrapper runs, and `done` firing against a source that
never sends — and assert the output closes without hanging. `TestRaceFreeUnderLoad`
runs a thousand values through under `-race`. `TestUntypedForwardsAnyValues`
checks the `any` form, and the `Example` gives an `// Output`-verified
round-trip.

Create `ordone_test.go`:

```go
package ordone

import (
	"fmt"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

func TestForwardsUntilSourceCloses(t *testing.T) {
	t.Parallel()

	src := make(chan int, 5)
	for i := 1; i <= 5; i++ {
		src <- i
	}
	close(src)

	done := make(chan struct{})
	var got []int
	for v := range OrDoneT(done, src) {
		got = append(got, v)
	}
	if len(got) != 5 {
		t.Fatalf("got %v, want 1..5", got)
	}
}

func TestStopsOnDone(t *testing.T) {
	t.Parallel()

	src := make(chan int)
	done := make(chan struct{})
	go func() {
		for i := 0; ; i++ {
			select {
			case src <- i:
			case <-done:
				return
			}
		}
	}()

	out := OrDoneT(done, src)

	got := 0
	for range out {
		got++
		if got == 3 {
			close(done)
		}
		if got > 10 {
			t.Fatalf("received %d values after done fired", got)
		}
	}
	// At most one in-flight value can slip through after done is closed.
	if got < 3 || got > 4 {
		t.Fatalf("received %d values, want 3 or 4", got)
	}
}

func TestStopsImmediatelyIfDoneAlreadyClosed(t *testing.T) {
	t.Parallel()

	src := make(chan int)
	go func() {
		for i := 0; ; i++ {
			select {
			case src <- i:
			case <-time.After(time.Second):
				return
			}
		}
	}()

	done := make(chan struct{})
	close(done)
	out := OrDoneT(done, src)

	select {
	case _, ok := <-out:
		if ok {
			// A single in-flight value is acceptable, but the channel must
			// then close promptly.
			if _, ok2 := <-out; ok2 {
				t.Fatal("expected output to close after done was already closed")
			}
		}
	case <-time.After(time.Second):
		t.Fatal("output did not close when done was already closed")
	}
}

func TestDoneFiresBeforeAnyValue(t *testing.T) {
	t.Parallel()

	src := make(chan int) // never sends, never closes
	done := make(chan struct{})
	out := OrDoneT(done, src)

	close(done)

	select {
	case _, ok := <-out:
		if ok {
			t.Fatal("expected closed output after done fired before any value")
		}
	case <-time.After(time.Second):
		t.Fatal("output did not close when done fired before any value")
	}
}

func TestRaceFreeUnderLoad(t *testing.T) {
	t.Parallel()

	const n = 1000
	src := make(chan int, n)
	for i := 0; i < n; i++ {
		src <- i
	}
	close(src)

	done := make(chan struct{})
	out := OrDoneT(done, src)

	var received atomic.Int64
	var wg sync.WaitGroup
	wg.Add(1)
	go func() {
		defer wg.Done()
		for range out {
			received.Add(1)
		}
	}()
	wg.Wait()

	if received.Load() != int64(n) {
		t.Fatalf("received %d, want %d", received.Load(), n)
	}
}

func TestUntypedForwardsAnyValues(t *testing.T) {
	t.Parallel()

	src := make(chan any, 3)
	src <- "hello"
	src <- 42
	src <- 3.14
	close(src)

	done := make(chan struct{})
	var got []any
	for v := range OrDone(done, src) {
		got = append(got, v)
	}
	if len(got) != 3 {
		t.Fatalf("got %d values, want 3", len(got))
	}
}

func ExampleOrDoneT() {
	src := make(chan int, 3)
	src <- 1
	src <- 2
	src <- 3
	close(src)

	done := make(chan struct{})
	for v := range OrDoneT(done, src) {
		fmt.Println(v)
	}
	// Output:
	// 1
	// 2
	// 3
}
```

## Review

The wrapper is correct when both selects watch `done` and a single
`defer close(out)` covers every return. Confirm the inner select is present: a
bare `out <- v` is the one change that turns this from leak-free into a goroutine
leak, and the race detector plus `TestStopsOnDone` are what catch its absence —
without the guard, the wrapper goroutine blocks on the send the moment the
consumer stops reading. Confirm the `ok` check on the receive: dropping it makes
a closed source spin, reading the zero value forever instead of terminating.
Confirm the wrapper never closes `done`; it is a parameter the caller owns, and
a wrapper that closes it races a double-close panic with the caller.

The subtle behavior to internalize is the at-most-one-extra delivery in
`TestStopsOnDone`. After the consumer closes `done` at the third value, the
wrapper may already have received a fourth in its outer select and be inside the
inner select; that fourth value can still be delivered before the next loop
iteration sees `done`. The bound is exactly one: the loop cannot receive a fifth
without first passing through the outer select again, where the closed `done` is
ready. This is why an at-least-once consumer cannot use this wrapper on its
critical path — that one droppable in-flight value is a lost message — and it is
the motivation for the drain-safe discipline two exercises from now.

## Resources

- [Go Blog: Pipelines and cancellation](https://go.dev/blog/pipelines) — the
  canonical treatment of `done`-channel cancellation that this wrapper distills.
- [Go talks: Advanced Go Concurrency Patterns](https://go.dev/talks/2013/advconc.slide) —
  Sameer Ajmani's talk introducing context-aware pipeline stages.
- [`context.Context.Done`](https://pkg.go.dev/context#Context.Done) — the
  `<-chan struct{}` that this wrapper's `done` parameter is shaped to accept.

---

Back to [00-concepts.md](00-concepts.md) | Next: [02-streaming-endpoint-client-disconnect.md](02-streaming-endpoint-client-disconnect.md)
</content>
