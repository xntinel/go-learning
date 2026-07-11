# Ranging Over Channels: Draining, Batching, and Fan-In in Backend Consumers

`for v := range ch` is the load-bearing loop of every Go backend that moves work
over channels: queue consumers, event ingesters, log and metric pipelines,
fan-in aggregators, batch flushers, and graceful-shutdown drains. The syntax is
trivial; the engineering is not. What separates a senior consumer from a toy is
the ownership and lifecycle discipline around the loop: who closes the channel
and when, why ranging a never-closed channel is a permanent goroutine leak, how
`range` differs from `for { v, ok := <-ch }` and from `for { select { ... } }`,
when you must abandon `range` because you need to multiplex cancellation, and how
to bound consumption by count, time, or context without dropping in-flight items.

This file is the conceptual foundation for the ten independent exercises that
follow. Each exercise is a real consumer artifact you would ship and operate:
draining a bounded queue on shutdown, merging worker outputs, flushing DB batches
on size-or-timer, throttling a firehose, aggregating partial-failure results,
deduplicating redelivered events, and bounding a scrape cycle by wall clock.

## Concepts

### What `range ch` actually does

`for v := range ch` receives successive values from `ch` until the channel is
**closed and drained**, then the loop ends. Two words in that sentence carry the
whole model. "Drained" means a buffered channel that has been closed still yields
every buffered value before the loop terminates: `close` does not mean "stop now",
it means "no more sends are coming". So closing a channel that holds three
buffered items and then ranging it produces exactly those three values and then
ends. A consumer that assumes `close` truncates the buffer will silently lose the
tail.

The loop is exactly sugar for the explicit receive form:

```go
for {
	v, ok := <-ch
	if !ok {
		break
	}
	// use v
}
```

The `ok` is false precisely when the channel is closed and its buffer is empty; at
that point `v` is the zero value. You drop to the explicit `ok`-form only when you
need to distinguish a legitimately-sent zero value from the closed signal, or when
you need to interleave other logic around each receive. For the common case,
`range` is shorter, clearer, and impossible to get wrong.

### The termination signal is close, and only close

`range` has no built-in cancellation. The only thing that ends the loop is the
channel being closed by whoever owns it. This is the single most important
operational fact about ranging: **ranging a channel that is never closed blocks
forever.** The goroutine parks in the receive, is never scheduled again, and is a
permanent leak. It shows up not as an error but as slowly growing memory, a
climbing goroutine count on your metrics, and a shutdown that hangs because that
goroutine never returns. Every `range ch` in a long-lived service must have a
provable answer to "who closes `ch`, and are they guaranteed to?"

### Ownership: the producer closes, never the consumer

The rule that prevents the nastiest bugs: the **producer (sender) closes the
channel; the consumer (receiver) never does.** Closing is a broadcast that
unblocks every ranger at once, so it is the sender's way of announcing "I am done
producing". If a receiver closes the channel to "signal done", it races the real
producer into a `send on closed channel` panic — an unrecoverable crash, not a
handled error. When a consumer needs to tell a producer to stop, it does so out of
band: cancel a `context`, or close a separate `done` channel that the producer
selects on. Encode the contract in the type: a `<-chan T` (receive-only)
parameter makes it a compile error for the consumer to send or close, so the
ownership rule is visible and enforced at the boundary.

### When you must abandon `range`: multiplexing a done signal

You cannot observe context cancellation from inside a `range` loop, because the
loop is blocked in a single receive with nowhere to watch a second channel. The
moment a consumer must react to `ctx.Done()` while it waits for the next value,
`range` is the wrong tool and you rewrite it as:

```go
for {
	select {
	case <-ctx.Done():
		return
	case v, ok := <-ch:
		if !ok {
			return
		}
		// use v
	}
}
```

This is the canonical "when NOT to use `range`" rule. The `select` lets the
goroutine wake on either the next value or cancellation, whichever comes first.
Note the cost: with both cases ready, `select` chooses pseudo-randomly, so a
cancelled context does not guarantee the loop stops before draining a value that
was already buffered — cancellation is prompt, not instantaneous. Consumers that
need "stop as soon as possible" and consumers that need "drain what is already
accepted" are different designs; know which one you are building.

### Bounding a consumer: count, time, cancellation

Bounding comes in three flavors that are frequently combined, and each demands a
different loop shape:

- **By count** — `break` after N items. This is the only bound that can stay a
  plain `range`, because you decide to stop from inside the loop body.
- **By time** — a `time.Ticker` for periodic flushes, or a `context` deadline for
  a hard wall-clock window. Both require `select`, because you are waiting on a
  timer channel concurrently with the data channel.
- **By cancellation** — `ctx.Done()`, which also requires `select`.

A subtle trap lives in the count bound: `break` stops *consuming* but does not
close or drain the channel. Abandoned items remain, and if nobody else drains
them, the producer blocks forever on its next send — backpressure silently turning
into a deadlock or leak. Breaking early is safe only when the producer is bounded,
buffered enough, or explicitly cancelled. Never treat `break` as a way to "throw
away the rest".

### Fan-in: many inputs, one output, exactly one close

Merging several producer channels into one lets a single consumer `range` the
combined stream. The pattern is one forwarding goroutine per input, each copying
its input into a shared output channel, plus a `sync.WaitGroup` and one closer
goroutine that calls `close(out)` **exactly once**, after all forwarders have
finished:

```go
func Merge[T any](chans ...<-chan T) <-chan T {
	out := make(chan T)
	var wg sync.WaitGroup
	for _, c := range chans {
		wg.Add(1)
		go func(c <-chan T) {
			defer wg.Done()
			for v := range c {
				out <- v
			}
		}(c)
	}
	go func() {
		wg.Wait()
		close(out)
	}()
	return out
}
```

The discipline is non-negotiable: if each forwarder closed `out` you would panic
with `close of closed channel`; if you closed `out` before the forwarders drained
you would lose values. The lone closer, gated on `wg.Wait()`, is what makes the
single-close correct. Callers then just `for v := range Merge(a, b, c)`.

### Pipelines: each stage owns and closes its own output

Stages compose by chaining ranges. A stage ranges its input, transforms or filters
each value, sends to a fresh output channel, and — this is the load-bearing part —
closes that output when its input range ends, typically with `defer close(out)`:

```go
func Stage[T, U any](in <-chan T, fn func(T) (U, bool)) <-chan U {
	out := make(chan U)
	go func() {
		defer close(out)
		for v := range in {
			if u, keep := fn(v); keep {
				out <- u
			}
		}
	}()
	return out
}
```

Closing the source propagates through every stage: each stage's `range` ends when
its upstream closes, its `defer close` fires, and the next stage's `range` ends in
turn, until the final consumer's loop terminates cleanly. A single close at the
head shuts the whole pipeline down with no leaks. The rule "each stage closes its
own output" is what makes that cascade work — and it is why a stage must read from
`in` and write to a *separate* `out`, never range and send on the same channel.

### Batching: trade latency for throughput, always flush the tail

A batch flusher accumulates items and writes them in groups to cap write
amplification on a database. It flushes on two triggers: **size** (throughput —
write a full batch as soon as it fills) and a **timer** (latency ceiling — never
let an accepted item wait longer than the interval). The rule that is most often
forgotten: when the input closes, **flush the final partial batch**. The last few
items rarely add up to a full `maxSize`, and a loop that only flushes on the size
trigger strands them. And because the accumulator slice is reused, hand a
`slices.Clone` to the flusher, not the live slice — otherwise the next `append`
mutates a batch the flusher is still holding.

### Idempotency: consumers under at-least-once delivery

Every real message broker (SQS, Kafka with at-least-once semantics, a retrying
HTTP producer) can redeliver the same message. A naive `range` processes the
duplicate twice — double-charges a card, double-sends an email. A senior consumer
is **idempotent**: it dedups by an idempotency key while ranging, keeping a
`map[string]struct{}` set of keys it has already handled and skipping any repeat.
The set is the smallest correct guard; in production it is often backed by Redis or
a unique DB constraint, but the ranging structure is identical.

## Common Mistakes

### Producer never closes, so the range blocks forever

Wrong: the sender finishes producing but never calls `close(ch)`, so the
consumer's `for range` parks in the receive after the last value. A silent
goroutine leak that surfaces only as growing memory and a shutdown that never
completes.

Fix: the sender closes exactly once when it is done, commonly with
`defer close(out)` in the producing goroutine. Every `range` must have an
owner that is guaranteed to close.

### Consumer closes the channel to signal done

Wrong: the receiver calls `close(ch)` to tell the producer to stop, racing the
producer into a `send on closed channel` panic.

Fix: only the owning sender closes. To ask the producer to stop, cancel a
`context` or close a separate `done` channel the producer selects on.

### Closing a fan-in output more than once

Wrong: each per-input forwarding goroutine closes the shared output when it
finishes, so the second close panics with `close of closed channel`.

Fix: one closer goroutine gated by `wg.Wait()` closes the output exactly once,
after every forwarder has returned.

### Assuming close truncates the buffer

Wrong: expecting `range` to stop the instant `close(ch)` is called, then being
surprised that buffered items are still delivered.

Fix: remember `close` means "no more sends"; every already-buffered value drains
first, and only then does the loop end.

### Trying to cancel a plain range with a context

Wrong: passing a `context` to a function that does `for v := range ch` and
expecting cancellation to break the loop — it cannot, so people bolt on a data
race or a second goroutine to force it.

Fix: rewrite as `for { select { case <-ctx.Done(): return; case v, ok := <-ch:
if !ok { return } } }`. `range` and cancellation do not mix.

### Using break to bound a consumer, stranding the producer

Wrong: `break` after N items while the producer keeps sending, so the producer
blocks forever on its next send because nobody drains the remainder — an
early-exit that becomes a deadlock.

Fix: cancel the producer (context) so it stops sending, or drain the remainder,
or only break early when the channel is buffered enough to absorb what is left.

### Ranging and sending on the same channel

Wrong: a loop that ranges over a channel and sends back to the *same* channel,
so it feeds itself and never terminates.

Fix: a stage reads from a distinct `in` and writes to a separate `out`.

### Forgetting the final partial batch

Wrong: a batch flusher that only flushes when the batch reaches `maxSize`,
silently dropping the last fewer-than-maxSize items when the input closes.

Fix: after the loop ends, flush any non-empty accumulated batch.

### Reusing the batch slice without cloning

Wrong: sending the live accumulator slice to the flusher and then continuing to
`append` to it, so the flusher observes the slice mutate underneath it (an
aliasing bug).

Fix: `slices.Clone` the batch (or hand it off and re-`make` a fresh one) before
sending it downstream.

### Using time.Tick in a long-lived consumer

Wrong: `time.Tick(d)` inside a consumer that runs for the life of the process —
the underlying ticker can never be stopped and leaks.

Fix: `t := time.NewTicker(d); defer t.Stop()` so the ticker is reclaimed when the
consumer returns.

Next: [01-drain-queue-consumer.md](01-drain-queue-consumer.md)
