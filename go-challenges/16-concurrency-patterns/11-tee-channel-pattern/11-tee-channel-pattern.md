# 11. Tee Channel Pattern: Split One Stream Into Two

The tee pattern takes one inbound channel and returns two outbound channels
that each receive every value the source emits. It is the same shape as the
Unix `tee(1)` command: data flows through unchanged, but two consumers see
every byte. The Go talks' "Advanced Go Concurrency Patterns" present it as
the building block for fanning a stream to multiple consumers.

```text
tee/
  go.mod
  internal/tee/tee.go
  internal/tee/tee_test.go
  cmd/teedemo/main.go
```

The package exposes `Tee` that returns two `<-chan T` channels, plus an
`OrDone` wrapper so the consumer can cancel without blocking the other
consumer. The test verifies that both channels receive every value, in
order, and that closing one consumer does not stop the other.

## Concepts

### Tee Splits Without Filtering

Every value emitted on `in` is delivered to both `out1` and `out2`. The
wrapper does not transform, drop, or reorder values. The pattern is
"duplicate the stream", not "split by predicate".

### Backpressure Is Per-Output

A tee must respect each consumer's pace. If consumer A is slow and consumer
B is fast, the tee cannot block on A's send or B starves. The standard fix
is to buffer each output by one and use `OrDone` on each consumer's read
side. Lesson 10's `OrDone` is the natural pair to `Tee`.

### Closing The Source Closes Both Outputs

The wrapper goroutine ranges over `in`. When `in` closes, the loop exits
and the wrapper closes both outputs. Each consumer's `for v := range c`
loop exits.

### A Slow Consumer Can Stall The Wrapper

Without per-consumer buffering, a slow consumer blocks the wrapper's send,
which in turn can stall the producer. The lesson's `Tee` uses unbuffered
outputs by default and documents the backpressure contract. The
`BufferedTee` variant adds a buffer per output.

## Exercises

Set up the module:

```bash
mkdir -p ~/go-exercises/tee/internal/tee ~/go-exercises/tee/cmd/teedemo
cd ~/go-exercises/tee
go mod init example.com/tee
```

### Exercise 1: The Basic Tee

Create `internal/tee/tee.go`:

```go
package tee

func Tee[T any](in <-chan T) (<-chan T, <-chan T) {
	out1 := make(chan T)
	out2 := make(chan T)
	go func() {
		defer close(out1)
		defer close(out2)
		for v := range in {
			out1 <- v
			out2 <- v
		}
	}()
	return out1, out2
}
```

Both sends are unguarded by `select`. If either consumer is slow, the
wrapper blocks on that send, and the fast consumer experiences backpressure
through the wrapper's slow send. This is the simplest correct form.

### Exercise 2: Buffered Tee

For consumers with mismatched rates, a buffered output per consumer isolates
the two:

```go
package tee

func BufferedTee[T any](in <-chan T, buf int) (<-chan T, <-chan T) {
	out1 := make(chan T, buf)
	out2 := make(chan T, buf)
	go func() {
		defer close(out1)
		defer close(out2)
		for v := range in {
			// A send to a slow consumer must not block the fast one. The
			// simplest correct form sends sequentially; with buf>=1 each
			// consumer can absorb one tick of jitter. With buf==0 the
			// wrapper still works as long as both consumers drain in step.
			out1 <- v
			out2 <- v
		}
	}()
	return out1, out2
}

// DoneTee is a tee where each consumer has its own done channel. A consumer
// that stops early does not stall the other; the wrapper just stops sending
// to the cancelled output.
func DoneTee[T any](done1, done2 <-chan struct{}, in <-chan T) (<-chan T, <-chan T) {
	out1 := make(chan T)
	out2 := make(chan T)
	go func() {
		defer close(out1)
		defer close(out2)
		for v := range in {
			// out1: send unless done1 has fired
			select {
			case out1 <- v:
			case <-done1:
			}
			// out2: send unless done2 has fired
			select {
			case out2 <- v:
			case <-done2:
			}
		}
	}()
	return out1, out2
}
```

A buffer of 1 per consumer is enough to smooth one-tick jitter; larger
buffers tolerate longer pauses at the cost of memory. The lesson uses
`buf=1` for the test because we only need to prove isolation, not deep
buffering.

### Exercise 3: Test The Contract

Create `internal/tee/tee_test.go`:

```go
package tee

import (
	"sync"
	"testing"
	"time"
)

func TestTeeDeliversEveryValueToBothConsumers(t *testing.T) {
	t.Parallel()

	src := make(chan int, 6)
	src <- 1
	src <- 2
	src <- 3
	src <- 4
	src <- 5
	src <- 6
	close(src)

	a, b := Tee(src)

	var gotA, gotB []int
	var wg sync.WaitGroup
	wg.Add(2)
	go func() {
		defer wg.Done()
		for v := range a {
			gotA = append(gotA, v)
		}
	}()
	go func() {
		defer wg.Done()
		for v := range b {
			gotB = append(gotB, v)
		}
	}()
	wg.Wait()

	want := []int{1, 2, 3, 4, 5, 6}
	if !equal(gotA, want) {
		t.Fatalf("A got %v, want %v", gotA, want)
	}
	if !equal(gotB, want) {
		t.Fatalf("B got %v, want %v", gotB, want)
	}
}

func TestTeeClosesBothOnSourceClose(t *testing.T) {
	t.Parallel()

	src := make(chan int)
	close(src)

	a, b := Tee(src)

	for range a {
		t.Fatal("A emitted unexpected value")
	}
	for range b {
		t.Fatal("B emitted unexpected value")
	}
}

func TestDoneTeeDoesNotBlockOnCancelledOutput(t *testing.T) {
	t.Parallel()

	src := make(chan int, 100)
	for i := 1; i <= 100; i++ {
		src <- i
	}
	close(src)

	done1 := make(chan struct{})
	done2 := make(chan struct{})
	a, b := DoneTee(done1, done2, src)

	close(done1)

	// B must receive every value: 100 values, source closes, b closes.
	var gotB []int
	for v := range b {
		gotB = append(gotB, v)
	}
	if len(gotB) != 100 {
		t.Fatalf("B got %d values, want 100", len(gotB))
	}

	// Drain whatever A still has; the wrapper should not block on out1.
	for range a {
	}
}

func TestBufferedTeeDrainsBufferedSource(t *testing.T) {
	t.Parallel()

	src := make(chan int, 5)
	for i := 1; i <= 5; i++ {
		src <- i
	}
	close(src)

	a, b := BufferedTee(src, 1)

	var wg sync.WaitGroup
	wg.Add(2)
	go func() {
		defer wg.Done()
		for range a {
		}
	}()
	go func() {
		defer wg.Done()
		for range b {
		}
	}()

	done := make(chan struct{})
	go func() {
		wg.Wait()
		close(done)
	}()
	select {
	case <-done:
	case <-time.After(100 * time.Millisecond):
		t.Fatal("BufferedTee deadlocked")
	}
}

func TestTeeWithStrings(t *testing.T) {
	t.Parallel()

	src := make(chan string, 3)
	src <- "alpha"
	src <- "beta"
	src <- "gamma"
	close(src)

	a, b := Tee(src)

	var gotA, gotB []string
	var wg sync.WaitGroup
	wg.Add(2)
	go func() {
		defer wg.Done()
		for v := range a {
			gotA = append(gotA, v)
		}
	}()
	go func() {
		defer wg.Done()
		for v := range b {
			gotB = append(gotB, v)
		}
	}()
	wg.Wait()

	want := []string{"alpha", "beta", "gamma"}
	if !equalStrings(gotA, want) {
		t.Fatalf("A got %v, want %v", gotA, want)
	}
	if !equalStrings(gotB, want) {
		t.Fatalf("B got %v, want %v", gotB, want)
	}
}

func TestTeeIsRaceFree(t *testing.T) {
	t.Parallel()

	const n = 1000
	src := make(chan int, n)
	for i := 0; i < n; i++ {
		src <- i
	}
	close(src)

	a, b := Tee(src)

	var wg sync.WaitGroup
	wg.Add(2)
	go func() {
		defer wg.Done()
		for range a {
		}
	}()
	go func() {
		defer wg.Done()
		for range b {
		}
	}()
	wg.Wait()
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

Your turn: add `TestTeeSourceLargerThanBufferDoesNotBlock` that pushes 100
values into a 5-buffered source and asserts the buffered tee drains without
deadlock.

### Exercise 4: Runnable Demo

Create `cmd/teedemo/main.go`:

```go
package main

import (
	"fmt"

	"example.com/tee/internal/tee"
)

func main() {
	src := make(chan int, 4)
	for i := 1; i <= 4; i++ {
		src <- i
	}
	close(src)

	a, b := tee.Tee(src)

	done := make(chan struct{})
	go func() {
		for v := range a {
			fmt.Printf("A: %d\n", v)
		}
		close(done)
	}()
	for v := range b {
		fmt.Printf("B: %d\n", v)
	}
	<-done
}
```

## Common Mistakes

### Closing One Output But Not The Other

Wrong: `defer close(out1)` only.

What happens: the wrapper exits and `out1` closes, but `out2` blocks
forever because the wrapper never reaches `close(out2)`.

Fix: close both, with `defer close(out1); defer close(out2)` in that order
(LIFO so `out2` closes first, then `out1`).

### Sending Without A Receiver

Wrong: starting the tee goroutine before the consumers are ready.

What happens: the first send blocks; the consumers wait for the tee to
deliver; deadlock.

Fix: either buffer the outputs, or make sure the consumers are scheduled
before the tee fires its first send.

### Treating Tee As Filter

Wrong: expecting `Tee(src)` to drop values for one consumer based on a
predicate.

What happens: both consumers receive every value, including ones you wanted
filtered.

Fix: write a separate `Split` function for predicate-based splitting.

### Using Tee When Fan-In Is Right

Wrong: combining two tee outputs back into one channel with `Merge`.

What happens: you have a duplicate stream that you then deduplicate
upstream.

Fix: if you only want each value once, use a single consumer. Tee is for
"two different things need to see the same stream".

## Verification

From `~/go-exercises/tee`:

```bash
test -z "$(gofmt -l .)"
go vet ./...
go build ./...
go test -count=1 -race ./...
```

All four must pass. `TestTeeIsRaceFree` proves the wrapper is safe under the
race detector with two concurrent consumers.

## Summary

- `Tee` duplicates one stream into two outbound channels.
- The wrapper closes both outputs when the source closes.
- `BufferedTee` adds a per-output buffer to isolate consumer rates.
- Tee is "every value to every consumer", not "split by predicate".
- Pair with `OrDone` (lesson 10) if a consumer needs cancellation.

## What's Next

Next: [Bridge Channel Pattern](../12-bridge-channel-pattern/12-bridge-channel-pattern.md).

## Resources

- [Go talks: Advanced Go Concurrency Patterns](https://go.dev/blog/advanced-go-concurrency-patterns)
- [Go Blog: Pipelines and cancellation](https://go.dev/blog/pipelines)
- [Unix tee(1)](https://man7.org/linux/man-pages/man1/tee.1.html)