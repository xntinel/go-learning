# Exercise 2: Spawn a Goroutine That Returns Its Result on a Channel

A goroutine cannot `return` a value to its caller — it has no caller in the ordinary
sense. The idiom for "run this concurrently and hand me one result" is to launch a
goroutine that delivers its answer on a channel. This exercise builds that shape the
safe way, using a capacity-1 result channel so the goroutine can finish and exit even
if the caller never reads the answer. The real-world shape is any async computation you
kick off and may or may not wait on — a speculative fetch, a background reduction, a
cancellable aggregation.

## What you'll build

```text
spawnresult/                       independent module: example.com/spawnresult
  go.mod
  spawn.go                         Spawn(done, work) <-chan int; cap-1 out, defer close
  cmd/
    demo/
      main.go                      runnable demo: spawn, drain work, receive the sum
  spawn_test.go                    full-drain, cancel, and ignored-result (no-leak) tests; -race
```

Files: `spawn.go`, `cmd/demo/main.go`, `spawn_test.go`.
Implement: `Spawn(done <-chan struct{}, work <-chan int) <-chan int` that folds `work` into a sum and delivers exactly one value on a cap-1 out channel, then closes out; returns the partial sum on `done`, the full sum on work-close.
Test: draining a closed work channel yields the full sum; closing `done` yields the partial; ignoring the result entirely still lets the goroutine finish (the cap-1 buffer).
Verify: `go test -count=1 -race ./...`

### Why the out channel has capacity 1

`Spawn` returns `<-chan int` and launches a goroutine that computes a single sum and
sends it on `out`. The critical design decision is `make(chan int, 1)`. With an
*unbuffered* out channel, the goroutine's `out <- sum` blocks until the caller receives;
if the caller decides it no longer needs the result — it cancelled, it errored, it timed
out — nobody ever receives, the send blocks forever, and the goroutine leaks. The cap-1
buffer breaks that coupling: the send always succeeds into the buffer, the goroutine runs
its `defer close(out)` and returns, and the buffered value is either read later or
garbage-collected along with the channel. The goroutine's lifetime is no longer hostage
to the caller's attention.

This is a deliberate, standard trade-off for the "one goroutine, one result" shape. It
costs one slot of buffering and buys guaranteed goroutine termination. `defer close(out)`
means the channel is closed on every return path, so a caller ranging or receiving after
the value can observe the close.

### The loop

Structurally identical to a cancellable worker, but delivering its answer on a channel
rather than returning it. It selects on `done` and on `work`; on either termination it
sends the current sum and returns. Because `out` is buffered to 1, that send never
blocks, on either path.

Create `spawn.go`:

```go
package spawnresult

// Spawn launches a goroutine that folds work into a sum and delivers exactly
// one result on the returned channel, which it then closes. If done is closed
// before work is drained, it delivers the partial sum. The result channel is
// buffered (cap 1) so the goroutine always finishes even if the caller never
// reads it. done is receive-only: Spawn's goroutine observes cancellation only.
func Spawn(done <-chan struct{}, work <-chan int) <-chan int {
	out := make(chan int, 1)
	go func() {
		defer close(out)
		var sum int
		for {
			select {
			case <-done:
				out <- sum
				return
			case v, ok := <-work:
				if !ok {
					out <- sum
					return
				}
				sum += v
			}
		}
	}()
	return out
}
```

### The runnable demo

Create `cmd/demo/main.go`:

```go
package main

import (
	"fmt"

	"example.com/spawnresult"
)

func main() {
	done := make(chan struct{})
	defer close(done)

	work := make(chan int, 3)
	work <- 10
	work <- 20
	work <- 30
	close(work)

	out := spawnresult.Spawn(done, work)
	sum := <-out
	fmt.Printf("spawned sum: %d\n", sum)
}
```

Run it:

```bash
go run ./cmd/demo
```

Expected output:

```
spawned sum: 60
```

### Tests

`TestSpawnReturnsSumOnClose` drives the normal path with a filled, closed work channel and
expects 60. `TestSpawnStopsOnDone` fills two buffered values, closes `done`, and expects
the partial sum — the `select` may fold zero, one, or both buffered values before it sees
`done`, so the assertion accepts any prefix sum, which is the honest contract of a random
`select`. `TestSpawnDoesNotLeakWhenCallerIgnoresResult` is the point of the cap-1 buffer:
it spawns, closes `done`, deliberately does not read the result at first, and then proves
the goroutine still finished by observing that `out` eventually closes (a closed channel
yields `ok == false` after its one buffered value). If the buffer were absent, that
goroutine would be blocked on the send and `out` would never close.

Create `spawn_test.go`:

```go
package spawnresult

import (
	"fmt"
	"testing"
)

func TestSpawnReturnsSumOnClose(t *testing.T) {
	t.Parallel()

	done := make(chan struct{})
	defer close(done)
	work := make(chan int, 3)
	work <- 10
	work <- 20
	work <- 30
	close(work)

	out := Spawn(done, work)
	if sum := <-out; sum != 60 {
		t.Fatalf("sum = %d, want 60", sum)
	}
}

func TestSpawnStopsOnDone(t *testing.T) {
	t.Parallel()

	done := make(chan struct{})
	work := make(chan int, 2)
	work <- 1
	work <- 2

	out := Spawn(done, work)
	close(done)

	// select is random: 0, 1, or 2 of the buffered values may be folded
	// before done is observed. The partial sum is one of these prefixes.
	sum := <-out
	switch sum {
	case 0, 1, 3:
	default:
		t.Fatalf("partial sum = %d, want one of {0,1,3}", sum)
	}
}

func TestSpawnDoesNotLeakWhenCallerIgnoresResult(t *testing.T) {
	t.Parallel()

	done := make(chan struct{})
	work := make(chan int) // no reader, no sends

	out := Spawn(done, work)
	close(done)

	// The caller "ignores" the result: it does not receive before cancelling.
	// Because out has cap 1, the goroutine's send succeeds without a reader,
	// it runs defer close(out) and exits. We prove it exited by draining out
	// until it is closed. If the buffer were missing, this would hang.
	<-out // the one buffered partial sum
	if _, ok := <-out; ok {
		t.Fatal("out delivered a second value; goroutine did not close it")
	}
}

func ExampleSpawn() {
	done := make(chan struct{})
	defer close(done)
	work := make(chan int, 2)
	work <- 7
	work <- 8
	close(work)

	out := Spawn(done, work)
	fmt.Println(<-out)
	// Output: 15
}
```

## Review

`Spawn` is correct when it delivers exactly one value and then closes the channel on every
path, and when it never blocks its own goroutine on the send. The cap-1 buffer is the whole
lesson: it decouples the goroutine's termination from the caller's willingness to receive,
which is what prevents the leak when a caller abandons the result. The ignored-result test
demonstrates this directly — the goroutine finishes and closes `out` without any reader
having taken the value first. Note the partial-sum test does not assert an exact number: a
`select` racing `done` against buffered work may fold any prefix, and asserting a single
value there would be a flaky test that misunderstands `select`. Run `go test -race` to
confirm there is no shared state to race — each `Spawn` owns its own `out` and `sum`.

## Resources

- [Go Blog: Go Concurrency Patterns (Rob Pike)](https://go.dev/talks/2012/concurrency.slide)
- [Go Language Spec: Close and receive from a closed channel](https://go.dev/ref/spec#Close)
- [Go Language Spec: Select statements](https://go.dev/ref/spec#Select_statements)

---

Back to [00-concepts.md](00-concepts.md) | Previous: [01-cancellable-worker.md](01-cancellable-worker.md) | Next: [03-fanout-pool-shutdown.md](03-fanout-pool-shutdown.md)
