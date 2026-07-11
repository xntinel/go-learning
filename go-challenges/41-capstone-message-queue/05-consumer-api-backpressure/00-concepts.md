# 5. Consumer API with Backpressure - Concepts

The consumer side of a message queue is where backpressure lives. A producer can always be told "slow down" by an acknowledgement protocol, but a consumer has the harder job: it must pull work at exactly the rate it can process it, never buffering so much that it runs out of memory and never falling so far behind that the queue grows without bound. This file is the conceptual foundation for three independent, self-contained Go modules that each attack one facet of that problem: a prefetch consumer whose backpressure is a bounded channel, a credit-based consumer that advertises a fixed in-flight budget, and a rate-limited consumer that caps delivery with a token bucket and can be paused mid-stream. Read this once and you will have the vocabulary and the design reasoning needed for all three.

## Concepts

### Pull-Based Consumption and the Fetch Loop

A pull-based consumer controls its own intake rate: it asks for records explicitly rather than having them pushed at it. A push design lets the broker decide when to send, which is simpler until a slow consumer is overwhelmed by a fast topic and the only recovery is to drop messages or buffer them unboundedly on the consumer. Pulling moves the rate decision to the side that knows its own capacity.

The canonical Go shape is a background goroutine per partition that continuously fills a bounded channel:

```
Broker.Fetch -> fetchLoop goroutine -> chan *Record -> Poll -> caller
```

The goroutine calls `Broker.Fetch`, which blocks until records are available, then sends the records one at a time into the channel. The application's `Poll` drains from the channel. The two halves run at independent speeds, joined only by the channel between them, and that channel is where backpressure is enforced.

### Backpressure via Bounded Channels

When the prefetch channel is at capacity, the fetch goroutine's send blocks:

```go
select {
case buf.ch <- r:
	// record is now buffered; advance the fetch position
case <-c.ctx.Done():
	return
}
```

The goroutine stalls until `Poll` drains a slot. There is no explicit signal, no extra mutex, no rate computation: the channel capacity *is* the flow-control mechanism. A buffer of 1000 records lets the fetcher run ahead of a momentarily slow application without holding the entire partition in memory, and if the application stops consuming entirely the fetcher stops too, having buffered at most `cap(ch)` records. This is the single most important idea on the consumer side: a buffered channel is simultaneously a prefetch cache and a backpressure valve.

### Bounded Fetch Context and Pause Semantics

The fetch goroutine must periodically re-check two things that change underneath it: the current fetch position (which `Seek` can move) and the pause flag. If `Broker.Fetch` blocks indefinitely while the partition is empty, neither update ever reaches the goroutine and `Pause`/`Seek` appear to hang.

The fix is a bounded context on each fetch call:

```go
fetchCtx, fetchCancel := context.WithTimeout(c.ctx, 100*time.Millisecond)
records, err := c.broker.Fetch(fetchCtx, p, offset, batchSize)
fetchCancel()
if errors.Is(err, context.DeadlineExceeded) {
	continue // re-read pause flag and position at the top of the loop
}
```

After at most 100 ms an empty fetch returns `context.DeadlineExceeded`, the loop restarts, and the goroutine picks up any `Pause` or `Seek` that arrived while it was waiting. This bounds the latency for either to take effect. The tradeoff is a 100 ms worst-case delay before a newly written record is noticed on an otherwise-idle partition, which is why production clients expose this as a `fetch.max.wait.ms`-style knob.

### Epoch Safety on Seek

`Seek` changes the fetch position, but the goroutine may be mid-batch: it fetched records starting at the old position and is partway through sending them into the channel. Letting those stale records land in the buffer after a `Seek` would corrupt delivery order, returning records the application explicitly seeked away from.

The solution is an epoch counter per partition, an `atomic.Int64` bumped by `Seek`. The goroutine reads the epoch once at the top of each loop iteration, right after reading the position, and checks it again before every channel send:

```go
// In Seek:
buf.epoch.Add(1)

// In fetchLoop, before each send:
if buf.epoch.Load() != epochBefore {
	break // discard the rest of this stale batch
}
```

If `Seek` bumps the epoch between reading the position and buffering a record, the goroutine discards the whole batch and re-reads the now-updated position next iteration. This is a compare-and-discard pattern: it is lock-free, needs no coordination with the mutex that guards the position map, and is correct because the epoch only ever moves forward.

### Position vs Committed Offset

Two offsets move independently and conflating them is the classic consumer bug.

- **Position** is the next offset the fetch goroutine will request. It advances as records are *buffered*, running ahead of what the application has actually processed. It lives in memory.
- **Committed offset** is durably stored on the broker and survives a restart. On a fresh consumer the starting position is loaded from the committed offset.

The gap between them is the uncommitted window: records received by the application but not yet committed. A crash in that window re-delivers those records (at-least-once) unless the delivery mode commits before returning them (at-most-once). The rule that keeps this honest is that auto-commit must track the highest offset *returned by Poll*, never the fetch position, because the fetch position includes records still sitting in the buffer that the application has not seen. Commit the position and a restart silently skips every buffered-but-unprocessed record.

### Credit-Based Flow Control

The bounded channel limits how many records sit *in the buffer*, but it says nothing about how many records the application has pulled out and not yet finished processing. A second, complementary form of backpressure bounds exactly that: the in-flight window of delivered-but-unacknowledged records.

In a credit-based protocol the consumer advertises a fixed number of credits to the fetcher. A record may be delivered only by spending one credit, and a credit is returned only when the application acknowledges that record. The number of records in flight therefore never exceeds the advertised credit count, no matter how fast the upstream produces. This is the model AMQP and RabbitMQ call *prefetch count* (`basic.qos`), and the same idea the Reactive Streams specification formalizes as *demand*: a subscriber calls `request(n)` to grant the publisher permission to emit up to `n` more elements, and the publisher must never exceed outstanding demand.

The clean Go implementation is a counting semaphore built from a buffered channel of tokens. Pre-fill it with `MaxCredits` tokens; the fetcher receives a token before each delivery (blocking when none remain) and the application's `Ack` sends a token back. Because the channel holds at most `MaxCredits` tokens, the count of tokens-taken-but-not-returned, which is exactly the in-flight window, is bounded by the channel capacity for free. The difference from the prefetch channel is *what* is bounded: the prefetch channel bounds records waiting to be picked up; credits bound records picked up but not yet completed. Real systems use both.

### Token-Bucket Rate Limiting and Pause/Resume

Sometimes the constraint is not memory or concurrency but a hard *rate*: deliver at most R records per second to protect a downstream database, a third-party API quota, or a billing system. A token bucket is the standard primitive. The bucket holds up to `burst` tokens and regenerates `rate` tokens per second; each delivery spends one token, and a delivery that finds the bucket empty must wait for regeneration.

The bucket gives a precise upper bound that is worth stating exactly: the number of deliveries between any two instants t0 and t can never exceed `burst + rate*(t - t0)`. The `burst` term lets a bucket that has been idle absorb a short spike up to its capacity; the `rate*elapsed` term is the steady-state ceiling. That inequality *is* the rate limit, and because it is an invariant of the bucket rather than an emergent property of timing, it can be tested deterministically by driving the bucket with a fake clock and counting grants.

Computing tokens lazily from elapsed wall-clock time, rather than with a background refill goroutine, keeps the bucket simple and makes the clock injectable for tests: `tokens = min(burst, tokens + elapsed*rate)` on each call, where `elapsed` comes from a `now()` function that real code wires to `time.Now` and tests wire to a manual clock.

Pause/resume is an orthogonal control that often ships alongside the limiter. Pausing flips an atomic flag that the delivery loop checks each iteration; while set, the loop spends no tokens and delivers nothing, and the records already produced simply wait. It is the coarse, operator-facing override ("stop delivering to this partition now") layered on top of the fine-grained, automatic rate limit.

## Common Mistakes

### Blocking Indefinitely in Broker.Fetch

Wrong: the fetch loop calls `broker.Fetch(c.ctx, ...)` with no per-call timeout. If the partition has no records, the goroutine blocks until the consumer is closed, so `Pause` and `Seek` never take effect.

What happens: a `Seek` updates the position map, but the goroutine is stuck inside `Fetch` waiting at the old position and never re-reads the new one. Tests that seek after draining a partition hang.

Fix: wrap each fetch in `context.WithTimeout(c.ctx, fetchMaxWait)`. When the timeout fires the loop restarts and re-reads the position and the pause flag, so both take effect within `fetchMaxWait`.

### Advancing the Position Before the Channel Send

Wrong:

```go
c.positions[p] = r.Offset + 1 // advance first
buf.ch <- r                   // then send
```

What happens: if the send blocks because the buffer is full and the consumer is then closed, the record is never delivered yet the position has already moved past it. The record is silently lost.

Fix: advance the position only inside the successful send branch (`case buf.ch <- r:`). A record that never enters the buffer never advances the position.

### Auto-Committing the Fetch Position Instead of the Delivered Offset

Wrong: the auto-commit goroutine reads `c.positions[p]` and commits it.

What happens: the position tracks where the *fetcher* is, ahead of what the application processed. Committing it marks buffered-but-unseen records as done; a restart skips them.

Fix: track the highest `offset + 1` returned by `Poll` in a separate `pendingCommit` map and commit that.

### Acking More Than Once Per Credit

Wrong: in a credit-based consumer, calling `Ack` in a `defer` and again at the end of processing, or acking on an error path that also retries.

What happens: each spurious `Ack` returns an extra token, inflating the credit balance above `MaxCredits`. The in-flight bound is silently broken and the consumer can run unboundedly ahead.

Fix: ack exactly once per record returned by `Recv`. Treat the credit accounting as a strict one-token-out, one-token-back ledger.

### Refilling the Token Bucket With a Background Goroutine

Wrong: a ticker goroutine that adds tokens every interval.

What happens: the refill granularity is the tick, so a 10 ms ticker delivers tokens in bursts of `rate*0.01` rather than smoothly, the goroutine leaks if the bucket outlives its owner, and the clock is not injectable so the rate bound can only be tested with flaky real-time sleeps.

Fix: compute tokens lazily on each `Allow` call from the elapsed time since the last call, capped at `burst`, using an injectable `now()` function. No goroutine, smooth regeneration, deterministic tests.

---

Next: [01-consumer-prefetch-backpressure.md](01-consumer-prefetch-backpressure.md)
