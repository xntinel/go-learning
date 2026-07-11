# Exercise 1: Drain a Bounded Work Queue with for-range (and Break-Early)

The most common consumer in any Go backend is a worker that pulls jobs off a
channel and processes them until the producer is done. This exercise builds that
worker two ways: one that drains the queue fully with `for v := range ch`, and a
bounded variant that stops after N jobs while leaving the channel owned by the
producer.

This module is fully self-contained: its own `go mod init`, all types inline, its
own demo and tests. Nothing here imports another exercise.

## What you'll build

```text
drainqueue/                 independent module: example.com/drainqueue
  go.mod                    go 1.26
  consumer.go               type Job; type Consumer; DrainAll, DrainLimit
  cmd/
    demo/
      main.go               fill a queue, close it, drain it
  consumer_test.go          drain-all, empty-closed, limit-stops-early, 100-value contract
```

Files: `consumer.go`, `cmd/demo/main.go`, `consumer_test.go`.
Implement: `Consumer` with `DrainAll(in <-chan Job) []Job` (ranges to completion) and `DrainLimit(in <-chan Job, limit int) []Job` (breaks after `limit`, never closes the channel).
Test: buffered-and-closed channel drains in order; empty-closed returns empty; `DrainLimit` returns exactly N and leaves the rest receivable; a 100-value collect-all contract test.
Verify: `go test -count=1 -race ./...`

Set up the module:

```bash
mkdir -p ~/go-exercises/drainqueue/cmd/demo
cd ~/go-exercises/drainqueue
go mod init example.com/drainqueue
```

### Why the parameter is a receive-only channel

`DrainAll` takes `in <-chan Job`, not `chan Job`. That receive-only type is the
ownership contract made visible: inside the consumer it is a compile error to send
on `in` or to `close(in)`. The producer keeps the bidirectional handle and is the
only party that can close. This is not decoration — it is how you prevent the
`send on closed channel` and `close of closed channel` panics at the type level
rather than by convention.

`DrainAll` is the plain `for v := range in` loop. It receives every value the
producer sends and terminates the instant the channel is closed and its buffer is
empty. Because a closed-but-buffered channel still yields its buffered values, a
producer can fill the queue, `close` it, and walk away; `DrainAll` still returns
every item. That is the property the 100-value contract test pins.

`DrainLimit` shows the count bound: it ranges but `break`s once it has collected
`limit` items. Critically, it does **not** close `in`. Breaking out of a range
leaves the channel open and its remaining items intact, still owned by the
producer. That is correct here because the caller owns the producer and will drain
or discard the rest; it would be a leak if the producer were blocked waiting to
send into a channel nobody drains. The test proves the non-closing property by
receiving the leftover items after `DrainLimit` returns.

Create `consumer.go`:

```go
package drainqueue

// Job is a unit of work pulled off a queue.
type Job struct {
	ID      int
	Payload string
}

// Consumer drains jobs from a producer-owned channel.
type Consumer struct{}

func New() *Consumer { return &Consumer{} }

// DrainAll ranges the channel until the producer closes it, returning every job
// in the order received. It never closes in: the producer owns that.
func (c *Consumer) DrainAll(in <-chan Job) []Job {
	var out []Job
	for j := range in {
		out = append(out, j)
	}
	return out
}

// DrainLimit collects at most limit jobs, then breaks. It does not close in;
// the remaining jobs stay in the channel for the producer's caller to handle.
func (c *Consumer) DrainLimit(in <-chan Job, limit int) []Job {
	var out []Job
	for j := range in {
		out = append(out, j)
		if len(out) >= limit {
			break
		}
	}
	return out
}
```

### The runnable demo

The demo plays the producer: it fills a buffered queue with five jobs, closes it
to announce "no more work", and hands the receive-only end to the consumer, which
drains all five.

Create `cmd/demo/main.go`:

```go
package main

import (
	"fmt"

	"example.com/drainqueue"
)

func main() {
	queue := make(chan drainqueue.Job, 5)
	for i := 1; i <= 5; i++ {
		queue <- drainqueue.Job{ID: i, Payload: fmt.Sprintf("task-%d", i)}
	}
	close(queue) // producer announces: no more jobs

	c := drainqueue.New()
	jobs := c.DrainAll(queue)

	fmt.Printf("drained %d jobs\n", len(jobs))
	for _, j := range jobs {
		fmt.Printf("job %d: %s\n", j.ID, j.Payload)
	}
}
```

Run it:

```bash
go run ./cmd/demo
```

Expected output:

```
drained 5 jobs
job 1: task-1
job 2: task-2
job 3: task-3
job 4: task-4
job 5: task-5
```

### Tests

`TestDrainAllInOrder` fills a buffered channel, closes it, and asserts the drained
slice equals the sent slice exactly — order and all. `TestDrainAllEmptyClosed`
proves an already-closed empty channel yields an empty (nil) result and does not
block. `TestDrainLimitStopsEarly` asserts `DrainLimit` returns exactly `limit`
items and, crucially, that the remaining items are still receivable afterward —
proving the consumer did not close the channel. `TestDrainAllCollectsEveryValue`
is the contract test: 100 values in, 100 values out, in order.

Create `consumer_test.go`:

```go
package drainqueue

import (
	"fmt"
	"slices"
	"testing"
)

func fill(jobs ...Job) chan Job {
	ch := make(chan Job, len(jobs))
	for _, j := range jobs {
		ch <- j
	}
	close(ch)
	return ch
}

func TestDrainAllInOrder(t *testing.T) {
	t.Parallel()
	want := []Job{{ID: 1, Payload: "a"}, {ID: 2, Payload: "b"}, {ID: 3, Payload: "c"}}
	ch := fill(want...)

	got := New().DrainAll(ch)
	if !slices.Equal(got, want) {
		t.Fatalf("DrainAll = %v, want %v", got, want)
	}
}

func TestDrainAllEmptyClosed(t *testing.T) {
	t.Parallel()
	ch := make(chan Job)
	close(ch)

	got := New().DrainAll(ch)
	if len(got) != 0 {
		t.Fatalf("DrainAll on empty-closed = %v, want empty", got)
	}
}

func TestDrainLimitStopsEarly(t *testing.T) {
	t.Parallel()
	// Buffered and NOT closed: DrainLimit must stop on the count bound alone.
	ch := make(chan Job, 5)
	for i := 1; i <= 5; i++ {
		ch <- Job{ID: i}
	}

	got := New().DrainLimit(ch, 3)
	if len(got) != 3 {
		t.Fatalf("DrainLimit len = %d, want 3", len(got))
	}
	// The channel was not closed by the consumer: the rest is still receivable.
	var rest []Job
	for len(ch) > 0 {
		rest = append(rest, <-ch)
	}
	if len(rest) != 2 {
		t.Fatalf("leftover = %d, want 2 (consumer must not close the channel)", len(rest))
	}
}

func TestDrainAllCollectsEveryValue(t *testing.T) {
	t.Parallel()
	const n = 100
	ch := make(chan Job, n)
	for i := range n {
		ch <- Job{ID: i}
	}
	close(ch)

	got := New().DrainAll(ch)
	if len(got) != n {
		t.Fatalf("DrainAll collected %d, want %d", len(got), n)
	}
	for i := range n {
		if got[i].ID != i {
			t.Fatalf("got[%d].ID = %d, want %d", i, got[i].ID, i)
		}
	}
}

func ExampleConsumer_DrainAll() {
	ch := fill(Job{ID: 7, Payload: "ship"}, Job{ID: 8, Payload: "log"})
	jobs := New().DrainAll(ch)
	fmt.Printf("%d jobs: %s, %s\n", len(jobs), jobs[0].Payload, jobs[1].Payload)
	// Output: 2 jobs: ship, log
}
```

## Review

The consumer is correct when `DrainAll` returns exactly the multiset the producer
sent, in order, and terminates only on close. The two failure modes this exercise
guards against are the classics: a producer that forgets to close (which would
hang `DrainAll` forever — every test here closes deliberately), and a consumer
that closes the channel it does not own. `DrainLimit`'s leftover test is the proof
of the second: after breaking early the remaining items must still be receivable,
which they can only be if the consumer left the channel open. Run
`go test -race` to confirm the buffered hand-off is clean.

## Resources

- [Go spec: For statements with range clause](https://go.dev/ref/spec#For_range) — the exact semantics of ranging a channel until close.
- [Effective Go: Channels](https://go.dev/doc/effective_go#channels) — close as "no more values", and the receive-only channel type.

---

Back to [00-concepts.md](00-concepts.md) | Next: [02-context-cancellable-consumer.md](02-context-cancellable-consumer.md)
