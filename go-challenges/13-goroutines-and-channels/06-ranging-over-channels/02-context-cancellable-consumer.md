# Exercise 2: Cancellable Consumer: for-select Instead of range

A plain `range` cannot observe cancellation. When a consumer must stop the moment
a `context` is cancelled — a request handler abandoning work because the client
disconnected, a worker draining until SIGTERM — you drop `range` for
`for { select { ... } }` and multiplex the data channel against `ctx.Done()`. This
exercise builds that consumer and returns whatever it drained before stopping.

This module is fully self-contained: its own `go mod init`, all types inline, its
own demo and tests. Nothing here imports another exercise.

## What you'll build

```text
cancelconsumer/             independent module: example.com/cancelconsumer
  go.mod                    go 1.26
  consumer.go               type Job; Consume(ctx, in) []Job via for-select
  cmd/
    demo/
      main.go               drain until the producer closes, then cancel
  consumer_test.go          pre-cancel empty, partial drain, closed-channel drain-all
```

Files: `consumer.go`, `cmd/demo/main.go`, `consumer_test.go`.
Implement: `Consume(ctx context.Context, in <-chan Job) []Job` that loops on a `select` over `ctx.Done()` and `v, ok := <-in`, returning the jobs drained so far.
Test: a pre-cancelled context returns immediately with an empty slice; an open channel plus a later cancel returns a bounded partial drain and terminates; a closed channel returns all jobs via the `ok == false` branch; `-race` proves no leak.
Verify: `go test -count=1 -race ./...`

### Why range cannot do this, and what replaces it

`for j := range in` blocks in a single receive. There is nowhere in that
expression to also watch a cancellation channel, so a cancelled context is simply
invisible to it — the loop keeps waiting for the next value or blocks forever. The
fix is the canonical multiplexing loop:

```go
for {
	select {
	case <-ctx.Done():
		return out
	case j, ok := <-in:
		if !ok {
			return out
		}
		out = append(out, j)
	}
}
```

Two exit paths, and both matter. The `ctx.Done()` case is cancellation: the caller
signalled stop, so we return what we have. The `!ok` case is the normal end: the
producer closed the channel, so there is nothing left to drain. Returning `out` in
both means the consumer never loses the work it already accepted — a cancelled
consumer hands back a partial result, not an empty one.

One honest caveat about `select`: when both cases are ready — the context is
cancelled *and* a value is buffered and waiting — `select` picks pseudo-randomly.
So a cancelled consumer does not deterministically stop before draining a buffered
value; cancellation is prompt but not instantaneous. Any test around cancellation
must assert a *bound* ("at most the buffered count", "terminates") rather than an
exact count, because the exact count is a scheduler coin-flip. This is a real
property of Go's `select`, not a weakness in the test.

Create `consumer.go`:

```go
package cancelconsumer

import "context"

// Job is a unit of work.
type Job struct {
	ID int
}

// Consumer drains a channel while honoring context cancellation.
type Consumer struct{}

func New() *Consumer { return &Consumer{} }

// Consume drains in until either the producer closes it or ctx is cancelled,
// whichever comes first, returning every job accepted before that point. A plain
// range could not observe ctx, which is exactly why this uses for-select.
func (c *Consumer) Consume(ctx context.Context, in <-chan Job) []Job {
	var out []Job
	for {
		select {
		case <-ctx.Done():
			return out
		case j, ok := <-in:
			if !ok {
				return out
			}
			out = append(out, j)
		}
	}
}
```

### The runnable demo

The demo shows the normal (uncancelled) path: a producer fills and closes the
queue, and `Consume` drains it via the `!ok` branch. The context is there but
never fires, which is the common case — cancellation is the exception, not the
rule.

Create `cmd/demo/main.go`:

```go
package main

import (
	"context"
	"fmt"

	"example.com/cancelconsumer"
)

func main() {
	queue := make(chan cancelconsumer.Job, 3)
	for i := 1; i <= 3; i++ {
		queue <- cancelconsumer.Job{ID: i}
	}
	close(queue)

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	jobs := cancelconsumer.New().Consume(ctx, queue)
	fmt.Printf("drained %d jobs before stop\n", len(jobs))
}
```

Run it:

```bash
go run ./cmd/demo
```

Expected output:

```
drained 3 jobs before stop
```

### Tests

`TestPreCancelledReturnsEmpty` cancels before consuming and asserts an empty
result and prompt return — the `ctx.Done()` branch wins immediately.
`TestClosedChannelDrainsAll` uses an uncancelled context and a closed buffered
channel to prove the `!ok` branch collects everything. `TestLaterCancelBounded`
feeds a fixed number of buffered items and a context that is then cancelled; it
asserts the consumer terminates and returns no more than what was buffered — the
honest bound, given `select` randomness.

Create `consumer_test.go`:

```go
package cancelconsumer

import (
	"context"
	"testing"
	"time"
)

func TestPreCancelledReturnsEmpty(t *testing.T) {
	t.Parallel()
	ctx, cancel := context.WithCancel(context.Background())
	cancel() // cancelled before we consume

	ch := make(chan Job) // never fed, never closed
	got := New().Consume(ctx, ch)
	if len(got) != 0 {
		t.Fatalf("Consume on pre-cancelled ctx = %d jobs, want 0", len(got))
	}
}

func TestClosedChannelDrainsAll(t *testing.T) {
	t.Parallel()
	ch := make(chan Job, 4)
	for i := range 4 {
		ch <- Job{ID: i}
	}
	close(ch)

	got := New().Consume(context.Background(), ch)
	if len(got) != 4 {
		t.Fatalf("Consume on closed channel = %d jobs, want 4", len(got))
	}
}

func TestLaterCancelBounded(t *testing.T) {
	t.Parallel()
	const buffered = 3
	ch := make(chan Job, buffered) // NOT closed
	for i := range buffered {
		ch <- Job{ID: i}
	}

	ctx, cancel := context.WithCancel(context.Background())
	// Cancel shortly; the consumer must terminate rather than block forever on
	// the never-closed channel once the buffer is drained.
	go func() {
		time.Sleep(20 * time.Millisecond)
		cancel()
	}()

	done := make(chan []Job, 1)
	go func() { done <- New().Consume(ctx, ch) }()

	select {
	case got := <-done:
		if len(got) > buffered {
			t.Fatalf("Consume returned %d jobs, want at most %d", len(got), buffered)
		}
	case <-time.After(time.Second):
		t.Fatal("Consume did not terminate after cancel: goroutine leak")
	}
}

func ExampleConsumer_Consume() {
	ch := make(chan Job, 2)
	ch <- Job{ID: 1}
	ch <- Job{ID: 2}
	close(ch)

	jobs := New().Consume(context.Background(), ch)
	println(len(jobs) == 2)
	// Output:
}
```

`TestLaterCancelBounded` is the leak proof: without the `ctx.Done()` case, a
`Consume` over a never-closed channel would block forever after draining the
buffer, the `done` channel would never receive, and the `time.After` guard would
fire the failure. Running the whole file under `-race` confirms the concurrent
cancel and consume share no unsynchronized state.

## Review

The consumer is correct when it exits on *either* signal and never loses accepted
work. The three tests pin the three behaviors: pre-cancel returns empty, close
drains all, and a mid-flight cancel terminates within a bound. Resist the urge to
assert an exact count in the cancel test — with a buffered value and a cancelled
context both ready, `select` chooses at random, so "exactly one drained" would
flake. Assert the bound and the termination, which are the real guarantees. The
`Example` uses an empty `// Output:` and `println` to stderr so it compiles and
runs as a smoke check without a brittle stdout assertion.

## Resources

- [Go spec: Select statements](https://go.dev/ref/spec#Select_statements) — pseudo-random choice among ready cases.
- [pkg.go.dev: context](https://pkg.go.dev/context) — `Context.Done`, `WithCancel`, and the cancellation contract.
- [Go Blog: Pipelines and cancellation](https://go.dev/blog/pipelines) — the explicit done/ctx multiplexing pattern.

---

Back to [00-concepts.md](00-concepts.md) | Previous: [01-drain-queue-consumer.md](01-drain-queue-consumer.md) | Next: [03-fan-in-merge.md](03-fan-in-merge.md)
