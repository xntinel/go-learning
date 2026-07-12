# Exercise 7: Job Queue with Bounded Prefetch

A producer that emits jobs as an `iter.Seq[Job]` is clean to range over, but a naive consumer stalls: it produces one job, processes it, then produces the next, never overlapping the two. The fix is bounded prefetch — let a background worker pull jobs ahead into a small buffer while the consumer works — and the trap is that the worker now owns a live `iter.Pull` that must be stopped if the consumer quits early, or its goroutine leaks. This exercise builds `Prefetch`, and its headline test runs under `-race` to prove no goroutine survives an early consumer exit.

This module is fully self-contained. It begins with its own `go mod init`, defines every type it needs, and ships its own demo and tests. Nothing here imports any other exercise.

## What you'll build

```text
prefetch.go          Job{ID,Payload}; Prefetch(jobs iter.Seq[Job], window int) iter.Seq[Job]
cmd/
  demo/
    main.go          drain a finite queue, then consume two jobs and bail out
prefetch_test.go     full-drain order, window<1 normalization, no-leak on early exit
```

- Files: `prefetch.go`, `cmd/demo/main.go`, `prefetch_test.go`.
- Implement: `Prefetch(jobs iter.Seq[Job], window int) iter.Seq[Job]` that pulls up to `window` jobs ahead of the consumer and re-exposes them as a push iterator.
- Test: drain a finite queue in order, confirm a non-positive window is normalized, and prove that breaking the consumer early tears the producer goroutine down (no leak) under `-race`.
- Verify: `go test -run 'TestPrefetch' -race ./...`

### Who owns the pull, and why it must be the worker

Prefetch overlaps production and consumption, so production has to run on its own goroutine: a background worker repeatedly pulls the next job and hands it to the consumer over a channel whose capacity, `window`, bounds how far ahead the worker may run. That channel is the prefetch buffer — fill it and the worker blocks until the consumer drains a slot, which is exactly the back-pressure that keeps prefetch bounded instead of eagerly draining an unbounded producer into memory.

The subtle part is ownership of the `iter.Pull` pair. `next` and `stop` must never be called concurrently with each other, so the worker — and only the worker — calls `iter.Pull` and `defer stop()`. If the main goroutine held `stop` and fired it while the worker was parked inside `next`, that is a concurrent `next`/`stop` and undefined behavior. By moving the pull entirely inside the worker goroutine, both `next` and `stop` live on one goroutine and can never overlap. The main goroutine never touches the pull; it only reads the channel and signals.

That signal is a `done` channel the returned `Seq` closes on every exit path via `defer close(done)`. Walk the early-exit case: the consumer ranges over `Prefetch`, processes two jobs, and `break`s. Breaking makes the inner `yield` return `false`, so the returned `Seq` returns, and its `defer close(done)` fires. The worker is parked either sending into the now-unread channel or pulling the next job; either way its `select` has a `<-done` arm, so it wakes, returns, and its deferred `stop()` runs — resuming the producer one last time with `yield` returning `false` so the producer's own cleanup runs and its coroutine exits. Drop the `defer close(done)` or the worker's `defer stop()` and that producer goroutine parks forever: against an unbounded producer that is a guaranteed, growing leak. The whole design is a generator (return a push `Seq`) wrapped around a bounded channel and a single owning worker, with `stop` reachable from every way the consumer can leave.

Create `prefetch.go`:

```go
// Create `prefetch.go`
package jobqueue

import "iter"

// Job is one unit of work flowing through the queue.
type Job struct {
	ID      int
	Payload string
}

// Prefetch wraps a producer push iterator so that up to window jobs are pulled
// ahead of the consumer by a background worker, overlapping production with
// consumption. The returned Seq is an ordinary push iterator: if the consumer
// breaks early, the worker is signalled, the producer's pull is stopped, and no
// goroutine is left behind. A window below 1 is treated as 1.
func Prefetch(jobs iter.Seq[Job], window int) iter.Seq[Job] {
	if window < 1 {
		window = 1
	}
	return func(yield func(Job) bool) {
		type item struct {
			job Job
			ok  bool
		}
		ch := make(chan item, window)
		done := make(chan struct{})

		// The worker owns the pull: it is the only goroutine that calls next or
		// stop, so the two can never run concurrently.
		go func() {
			next, stop := iter.Pull(jobs)
			defer stop()
			for {
				j, ok := next()
				select {
				case ch <- item{job: j, ok: ok}:
					if !ok {
						return
					}
				case <-done:
					return
				}
			}
		}()

		// Closing done on every exit path tears the worker (and its pull) down.
		defer close(done)

		for it := range ch {
			if !it.ok {
				return
			}
			if !yield(it.job) {
				return
			}
		}
	}
}
```

### The runnable demo

The demo drains a finite five-job queue with a window of two — the worker stays at most two jobs ahead — then consumes only the first two jobs of a thousand-job queue and breaks. The output is deterministic because a single worker writes the buffered channel in producer order and the single consumer reads it in that same order.

Create `cmd/demo/main.go`:

```go
// Create `cmd/demo/main.go`
package main

import (
	"fmt"
	"iter"

	"example.com/job-queue-prefetch"
)

func producer(n int) iter.Seq[jobqueue.Job] {
	return func(yield func(jobqueue.Job) bool) {
		for i := 1; i <= n; i++ {
			if !yield(jobqueue.Job{ID: i, Payload: fmt.Sprintf("task-%d", i)}) {
				return
			}
		}
	}
}

func main() {
	fmt.Println("drain all:")
	for j := range jobqueue.Prefetch(producer(5), 2) {
		fmt.Printf("ran %s\n", j.Payload)
	}

	fmt.Println("early exit after 2:")
	count := 0
	for j := range jobqueue.Prefetch(producer(1000), 3) {
		fmt.Printf("ran %s\n", j.Payload)
		count++
		if count == 2 {
			break
		}
	}
	fmt.Println("stopped early; producer goroutine torn down")
}
```

Run it:

```bash
go run ./cmd/demo
```

Expected output:

```
drain all:
ran task-1
ran task-2
ran task-3
ran task-4
ran task-5
early exit after 2:
ran task-1
ran task-2
stopped early; producer goroutine torn down
```

### Tests

`TestPrefetchDrainsAll` consumes a finite queue and asserts every job arrives in order, and reuses the same body with `window == 0` to prove a non-positive window is normalized rather than deadlocking. `TestPrefetchNoGoroutineLeak` is the proof under `-race`: it consumes an unbounded producer, breaks early, and uses a channel the producer closes in its `defer` to confirm the producer's cleanup ran (so `stop` was called); it repeats fifty times and checks the goroutine count returns to baseline, which it cannot do if each early exit stranded a worker.

Create `prefetch_test.go`:

```go
// Create `prefetch_test.go`
package jobqueue

import (
	"fmt"
	"iter"
	"runtime"
	"slices"
	"testing"
	"time"
)

func producerN(n int) iter.Seq[Job] {
	return func(yield func(Job) bool) {
		for i := 1; i <= n; i++ {
			if !yield(Job{ID: i, Payload: fmt.Sprintf("task-%d", i)}) {
				return
			}
		}
	}
}

func TestPrefetchDrainsAll(t *testing.T) {
	for _, window := range []int{1, 2, 5, 0} {
		var got []int
		for j := range Prefetch(producerN(5), window) {
			got = append(got, j.ID)
		}
		if !slices.Equal(got, []int{1, 2, 3, 4, 5}) {
			t.Fatalf("window %d drained %v, want [1 2 3 4 5]", window, got)
		}
	}
}

func TestPrefetchNoGoroutineLeak(t *testing.T) {
	base := runtime.NumGoroutine()

	for n := 0; n < 50; n++ {
		cleaned := make(chan struct{})
		// An unbounded producer: if its goroutine is not stopped on early exit,
		// it parks forever and cleaned is never closed.
		var producer iter.Seq[Job] = func(yield func(Job) bool) {
			defer close(cleaned)
			for i := 0; ; i++ {
				if !yield(Job{ID: i}) {
					return
				}
			}
		}

		got := 0
		for range Prefetch(producer, 4) {
			got++
			if got == 3 {
				break
			}
		}
		if got != 3 {
			t.Fatalf("iteration %d consumed %d, want 3", n, got)
		}

		select {
		case <-cleaned:
			// stop() resumed the producer to its deferred close: no leak.
		case <-time.After(2 * time.Second):
			t.Fatalf("iteration %d: producer goroutine leaked after early exit", n)
		}
	}

	// After fifty early exits, no worker or producer goroutine should remain.
	deadline := time.Now().Add(2 * time.Second)
	for {
		runtime.GC()
		have := runtime.NumGoroutine()
		if have <= base+2 {
			break
		}
		if time.Now().After(deadline) {
			t.Fatalf("goroutines leaked: have %d, want <= %d", have, base+2)
		}
		time.Sleep(10 * time.Millisecond)
	}
}
```

## Review

`Prefetch` is correct when the worker is the sole owner of the `iter.Pull` pair, the channel capacity is the prefetch window, and `done` is closed on every exit path so the worker — and through it `stop` — is always reached. The drain test with several window sizes, including `0`, proves two things: production and consumption interleave correctly regardless of how far ahead the buffer lets the worker run, and a non-positive window is normalized to `1` instead of creating an unbuffered channel that deadlocks the first send. Order is deterministic because one worker writes the channel and one consumer reads it.

The no-leak test is the reason the design exists. Against an unbounded producer, the only way the `cleaned` channel ever closes is if `stop` resumes the producer to its deferred `close` — so a missing `defer stop()` or a missing `defer close(done)` turns the two-second wait into a failure, fifty times over. The trailing goroutine-count check is the blunt confirmation: if each early exit stranded a worker, fifty iterations would leave fifty extra goroutines and the count would never settle back to baseline. Running it under `-race` additionally rules out any data race in the worker/channel handoff. The lesson that outlasts this exercise: the moment you move an iterator's production onto a background goroutine, you own a `stop` that must be reachable from every way the consumer can leave — close a `done` channel on every exit path and let the goroutine that owns the pull call `stop` on its way out.

## Resources

- [`iter.Pull`](https://pkg.go.dev/iter#Pull) — the adapter the worker owns, with the documented rule that `next` and `stop` must not run concurrently.
- [`iter.Seq`](https://pkg.go.dev/iter#Seq) — the push iterator type the producer supplies and `Prefetch` re-exposes.
- [Go memory model](https://go.dev/ref/mem) — defines the channel happens-before edges that make the worker handoff race-free.
- [Range Over Function Types](https://go.dev/blog/range-functions) — explains the coroutine behind `Pull` and why an unstopped pull leaks a goroutine.

---

Back to [00-concepts.md](00-concepts.md) | Previous: [06-stream-join-service.md](06-stream-join-service.md) | Next: [../08-standard-library-iterators/00-concepts.md](../08-standard-library-iterators/00-concepts.md)
