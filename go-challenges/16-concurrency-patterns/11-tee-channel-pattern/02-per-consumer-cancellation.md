# Exercise 2: Per-Consumer Cancellation

A tee often feeds two consumers with different lifetimes: one may finish, error out, or have its context cancelled while the other keeps going. This exercise builds a tee that gives each output its own done channel, so a consumer can detach without stalling the other, and a per-output buffer that smooths timing jitter. It is also where we state plainly what buffering does and does not buy you, correcting a tempting but false belief.

## What you'll build

```text
tee.go               Tee[T] with per-output done channels and a per-output buffer
cmd/
  demo/
    main.go          cancel one consumer; show the other still receives every value
tee_test.go          cancelled output never stalls the other; both-live delivery; no deadlock; race
```

- Files: `tee.go`, `cmd/demo/main.go`, `tee_test.go`.
- Implement: `Tee[T any](done1, done2 <-chan struct{}, buf int, in <-chan T) (<-chan T, <-chan T)`.
- Test: closing `done1` lets the second consumer receive every value, two live consumers both receive everything, a buffered run drains without deadlock, and a 1000-value run is clean under `-race`.
- Verify: `go test -race ./...`

Set up the module:

```bash
mkdir -p go-solutions/16-concurrency-patterns/11-tee-channel-pattern/02-per-consumer-cancellation/cmd/demo && cd go-solutions/16-concurrency-patterns/11-tee-channel-pattern/02-per-consumer-cancellation
```

### What a done channel per output actually isolates

The basic tee from exercise 1 has no escape hatch: if a consumer stops reading, the wrapper blocks forever on its next send to that output. The fix is to let each output carry its own cancellation signal and to make every send abandonable:

```go
select {
case out1 <- v:
case <-done1:
}
```

If `done1` has fired, the receive on the closed channel is always ready, so the select takes that branch instead of blocking on the send, and the wrapper moves on to `out2`. A consumer that closes its done channel and walks away costs the wrapper nothing: each subsequent value is offered to the departed output, instantly skipped, and delivered to the survivor. This is genuine independence — a cancelled consumer imposes no delay, no backpressure, nothing, on the other consumer. That is the property this exercise is about, and it is worth having cleanly.

### What buffering does not isolate

It is just as important to be precise about what this tee does **not** give you, because the natural next thought — "add a buffer and a slow consumer won't block the fast one" — is false for sustained load, and believing it leads to designs that wedge in production. The `buf` parameter sizes a buffer on each output. A buffer lets a consumer fall briefly behind: a value sent to a momentarily-busy consumer lands in its buffer and the wrapper proceeds without waiting for the consumer to catch up. This smooths jitter, a consumer that stutters for a tick and recovers never stalls the source.

But a buffer of any fixed depth does not decouple sustained rates. Consider two live consumers, one permanently slower than the source. Its buffer fills, and once full the next send blocks exactly as in the unbuffered case — the `select` has only the send and the never-firing `done` to choose from, so it waits for the slow consumer, and the source is throttled to that consumer's pace, which throttles the fast consumer too. The buffer bought slack equal to its depth and nothing more. Cancellation independence and rate independence are different things: skipping a *cancelled* output is free, but feeding a *slow but live* output still applies backpressure that the other consumer feels. The only way to let one live consumer run arbitrarily far behind another is to stop guaranteeing it every value, which means dropping — the subject of exercises 3 and 4. This tee deliberately does not drop; it is lossless to both live consumers, and lossless plus bounded buffers necessarily means the slower consumer governs the pace.

Create `tee.go`:

```go
package tee

// Tee returns two channels that each receive every value from in. Each output
// has its own done channel and an optional buffer of depth buf.
//
// Closing done1 detaches out1's consumer: the wrapper's send to out1 becomes a
// no-op (the closed-channel receive wins the select), so it never blocks waiting
// for a reader that has gone away, and out2 keeps receiving every value. The
// same holds for done2 and out2. Cancellation is therefore fully independent.
//
// Rate is not. While both consumers are live, the wrapper sends to each in turn
// with a blocking send, so a slow live consumer applies backpressure to the
// source and thereby to the other consumer. The buf parameter absorbs short
// bursts of jitter; it does not decouple sustained rates. To let one consumer
// outrun the other indefinitely you must drop values, which this tee never does.
func Tee[T any](done1, done2 <-chan struct{}, buf int, in <-chan T) (<-chan T, <-chan T) {
	out1 := make(chan T, buf)
	out2 := make(chan T, buf)
	go func() {
		defer close(out1)
		defer close(out2)
		for v := range in {
			select {
			case out1 <- v:
			case <-done1:
			}
			select {
			case out2 <- v:
			case <-done2:
			}
		}
	}()
	return out1, out2
}
```

### The runnable demo

The demo cancels consumer A before any value flows by closing `done1`, then leaves A unread — the realistic shape of a consumer that has detached. With `buf` set to zero, A's send has neither a reader nor buffer space, so the closed `done1` is the only ready case and the wrapper skips A every time, delivering all five values to B. The output is deterministic.

Create `cmd/demo/main.go`:

```go
package main

import (
	"fmt"

	"example.com/cancellable-tee"
)

func main() {
	src := make(chan int, 5)
	for i := 1; i <= 5; i++ {
		src <- i
	}
	close(src)

	done1 := make(chan struct{})
	done2 := make(chan struct{})
	a, b := tee.Tee(done1, done2, 0, src)

	// Consumer A detaches immediately and never reads from a.
	close(done1)
	_ = a

	var gotB []int
	for v := range b {
		gotB = append(gotB, v)
	}

	fmt.Printf("B received every value: %v\n", gotB)
	fmt.Println("A was cancelled and read nothing; the wrapper skipped it without stalling B")
}
```

Run it:

```bash
go run ./cmd/demo
```

Expected output:

```
B received every value: [1 2 3 4 5]
A was cancelled and read nothing; the wrapper skipped it without stalling B
```

### Tests

`TestCancelledConsumerDoesNotStallOther` is the core property: with `done1` closed and A unread, B must still receive all 100 values and both outputs must close. `TestBothLiveReceiveEverything` confirms that when neither consumer cancels, both receive the full stream. `TestBufferedDrainsWithoutDeadlock` runs a buffered tee with both consumers draining and guards against a hang with a timeout. `TestTeeIsRaceFree` drives a thousand values through two live consumers under `-race`.

Create `tee_test.go`:

```go
package tee

import (
	"sync"
	"testing"
	"time"
)

func collect[T any](c <-chan T) []T {
	var out []T
	for v := range c {
		out = append(out, v)
	}
	return out
}

func TestCancelledConsumerDoesNotStallOther(t *testing.T) {
	t.Parallel()

	const n = 100
	src := make(chan int, n)
	for i := range n {
		src <- i
	}
	close(src)

	done1 := make(chan struct{})
	done2 := make(chan struct{})
	a, b := Tee(done1, done2, 0, src)

	// Consumer A detaches and never reads a.
	close(done1)

	got := collect(b)
	if len(got) != n {
		t.Fatalf("B got %d values, want %d", len(got), n)
	}

	// a was closed by the wrapper; draining it must return immediately.
	for range a {
		t.Fatal("a delivered a value to a cancelled consumer that was not reading")
	}
}

func TestBothLiveReceiveEverything(t *testing.T) {
	t.Parallel()

	const n = 50
	src := make(chan int, n)
	for i := range n {
		src <- i
	}
	close(src)

	done1 := make(chan struct{})
	done2 := make(chan struct{})
	a, b := Tee(done1, done2, 1, src)

	var gotA, gotB []int
	var wg sync.WaitGroup
	wg.Add(2)
	go func() { defer wg.Done(); gotA = collect(a) }()
	go func() { defer wg.Done(); gotB = collect(b) }()
	wg.Wait()

	if len(gotA) != n || len(gotB) != n {
		t.Fatalf("A got %d, B got %d, want both %d", len(gotA), len(gotB), n)
	}
	for i := range n {
		if gotA[i] != i || gotB[i] != i {
			t.Fatalf("order mismatch at %d: A=%d B=%d", i, gotA[i], gotB[i])
		}
	}
}

func TestBufferedDrainsWithoutDeadlock(t *testing.T) {
	t.Parallel()

	src := make(chan int, 5)
	for i := range 5 {
		src <- i
	}
	close(src)

	done1 := make(chan struct{})
	done2 := make(chan struct{})
	a, b := Tee(done1, done2, 1, src)

	var wg sync.WaitGroup
	wg.Add(2)
	go func() { defer wg.Done(); collect(a) }()
	go func() { defer wg.Done(); collect(b) }()

	done := make(chan struct{})
	go func() { wg.Wait(); close(done) }()

	select {
	case <-done:
	case <-time.After(time.Second):
		t.Fatal("buffered tee deadlocked")
	}
}

func TestTeeIsRaceFree(t *testing.T) {
	t.Parallel()

	const n = 1000
	src := make(chan int, n)
	for i := range n {
		src <- i
	}
	close(src)

	done1 := make(chan struct{})
	done2 := make(chan struct{})
	a, b := Tee(done1, done2, 4, src)

	var wg sync.WaitGroup
	wg.Add(2)
	go func() { defer wg.Done(); collect(a) }()
	go func() { defer wg.Done(); collect(b) }()
	wg.Wait()
}
```

## Review

This tee is correct when a cancelled consumer is skipped for free and a live consumer still gets every value: `TestCancelledConsumerDoesNotStallOther` closing `done1` and still seeing all 100 values on B is the proof of the first half, and the both-live test seeing the full ordered stream on both sides is the proof of the second. The trap this exercise exists to dispel is the belief that the `buf` parameter isolates a slow consumer from a fast one — it does not, and the prose says so because saying otherwise would be a lie the code cannot back up: once a live consumer's buffer fills, the blocking send throttles the source and the other consumer with it. Keep cancellation independence and rate independence separate in your head; this exercise delivers the former honestly and points you at exercises 3 and 4 for the latter, where dropping is what actually decouples rates. The buffered-drain test's timeout guard catches the classic deadlock of starting the wrapper with no receiver, and the race test confirms two live consumers share the stream without a data race.

## Resources

- [Go Blog: Go Concurrency Patterns: Pipelines and cancellation](https://go.dev/blog/pipelines) — the done-channel cancellation idiom and why every blocking send in a pipeline should select on done.
- [The Go Memory Model](https://go.dev/ref/mem) — the happens-before guarantees of channel send/receive and close that make the cancellation race-free.
- [Go spec: Select statements](https://go.dev/ref/spec#Select_statements) — the rule that a select with multiple ready communications chooses one, which is why a closed done channel reliably wins over a blocked send.

---

Back to [01-basic-tee.md](01-basic-tee.md) | Next: [03-event-stream-split.md](03-event-stream-split.md)
