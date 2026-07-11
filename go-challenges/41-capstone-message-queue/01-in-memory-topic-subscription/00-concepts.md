# In-Memory Topic and Subscription System — Concepts

A message queue is, at its heart, a concurrency problem wearing the clothes of a data-structure problem. The data structure is almost embarrassingly simple: an append-only slice of messages with a per-subscriber cursor. Everything hard about a broker lives in the spaces between goroutines — a subscriber that blocks waiting for a message that has not been published yet, two publishers racing to claim the next offset, a delivery that must be retried because its consumer crashed before acknowledging it, a topic that is deleted while someone is still polling it, and a fast publisher that would otherwise drown a slow consumer. This file is the conceptual foundation for the whole lesson: read it once and you will have the model you need to reason through each exercise, which builds the broker piece by piece as independent, self-contained Go modules.

## Concepts

### The append-only offset log

A topic is not a queue in the traditional first-in-first-out, consume-and-discard sense. Messages are never removed when they are read. Each message is assigned a monotonically increasing 64-bit offset equal to its position in the underlying slice, and that offset is the message's permanent identity. Subscribers do not mutate the log at all; the only per-subscriber state is a cursor — an integer saying "I have read up to here." This is the design Apache Kafka popularized: the log is the single source of truth, and the consumer's offset is the one piece of mutable state that belongs to the consumer rather than to the broker.

Two consequences fall out of this design immediately. The first is that fan-out is nearly free: broadcasting one message to a thousand subscribers does not copy the message a thousand times, it simply means a thousand cursors will each, in their own time, step past the same shared slice element. The second is that retention, not consumption, is what bounds storage. Messages accumulate until something explicitly reclaims them — a retention policy, a compaction pass, or dropping the topic entirely. Reading a message costs nothing and frees nothing.

The offset must be assigned while the topic lock is held, in the same critical section that performs the append. If two publishers each computed `len(messages)` without the lock and then appended, they could compute the same length, assign the same offset, and corrupt the log. Assigning `offset = len(messages)` immediately before `append(messages, msg)`, both under the mutex, makes duplicate offsets impossible no matter how many goroutines publish concurrently.

### sync.Cond for a blocking poll

A subscriber calls `Poll(n, timeout)` and reasonably expects to block until at least one message is available or the timeout elapses — not to spin in a loop burning a CPU core, and not to return empty immediately. The correct primitive for "sleep until a shared condition becomes true" is `sync.Cond`, a condition variable bound to the topic's mutex.

The mechanics are worth internalizing because they are subtle. `cond.Wait()` does three things atomically: it releases the mutex, suspends the calling goroutine, and — when later woken — re-acquires the mutex before returning. The atomic release-and-suspend is the crucial part: it closes the window in which a publisher could append-and-signal in between a subscriber checking the condition and going to sleep. That window, if it existed, would be the classic "lost wakeup" bug, where the subscriber sleeps forever despite a message being available. Publishers call `cond.Broadcast()` after every successful append, which wakes every waiting poller. Each woken poller re-checks its condition in a loop, because a wakeup is only a hint that something *might* have changed, never a guarantee — `Wait` may also return spuriously, and `Broadcast` wakes everyone even though only some have new data. The iron rule is: always call `Wait` inside a `for` loop that re-tests the predicate, never inside a bare `if`.

The timeout is layered on top with `time.AfterFunc`. When a poller is about to wait, it arms a timer whose callback acquires the same mutex and broadcasts on the same cond. When the deadline fires, the callback wakes the waiter exactly as a publisher would; the waiter re-checks, sees the deadline has passed, and returns empty. There is no busy loop, no second channel, and no separate timeout goroutine that outlives the call — the timer is stopped as soon as `Wait` returns. This is the canonical way to turn an unbounded condition wait into a bounded one.

### At-least-once delivery and the visibility timeout

There are three delivery guarantees a broker can offer. At-most-once may drop a message but never delivers it twice. Exactly-once is the holy grail and, in a distributed setting, provably expensive. At-least-once — the guarantee this broker provides — may deliver a message more than once but will never silently drop it. For most systems this is the right default: duplicates are tolerable if consumers are idempotent, whereas silent loss usually is not.

The mechanism that delivers at-least-once is the visibility timeout, the same idea behind Amazon SQS. When a subscriber polls a message, the subscription does not advance past it and forget it; instead it records the delivery time and a state of "delivered, awaiting acknowledgment." If the subscriber finishes its work and calls `Ack(offset)` within the visibility window, the message transitions to "acknowledged" and is never redelivered. If the window elapses with no `Ack` — because the consumer crashed, hung, or simply was too slow — the broker concludes the delivery failed and makes the message visible again on the next `Poll`. `Nack(offset)` is the explicit, polite version of a crash: it tells the broker "I cannot process this right now, redeliver it immediately" by expiring the visibility window at once rather than waiting it out.

This is why expired redeliveries must take priority over fetching brand-new messages in a `Poll`. If a flood of new messages could always be served first, a single repeatedly-Nacked message could be starved indefinitely. Checking for expired in-flight messages before advancing the cursor guarantees a message that needs redelivery is never crowded out.

### Broadcast versus competing consumers

The same log supports two dispatch patterns that look opposite but share one implementation. In Broadcast mode, every subscription with a distinct name receives every message; each has its own independent cursor starting wherever it chose. This is publish/subscribe: N subscribers, N copies delivered, N times the work. It is how you fan an event out to several independent reactions — a logger, a metrics collector, and an email notifier all seeing every order.

In Competing Consumers mode, several callers share one subscription name and therefore one cursor. They form a consumer group, and each message is delivered to exactly one member of the group. This is work distribution: N workers, one copy delivered, the load split N ways. The implementation trick is that callers sharing a name share the same `*Subscription` value, and the topic mutex serializes their concurrent `Poll` calls — because advancing the shared cursor happens under the lock, whichever poller wins the lock claims the next batch and the others see an already-advanced cursor. No message is delivered twice within the group, and none is lost. This is exactly the semantics of a Kafka consumer group or a RabbitMQ work queue.

### Backpressure: what happens when the log fills

An unbounded in-memory log is a memory leak waiting for a fast publisher. A topic can therefore be configured with a maximum message count or a maximum total byte size. When a `Publish` would exceed the limit, the topic's policy decides the outcome. The reject policy returns an error (`ErrTopicFull`) immediately, pushing the decision back to the publisher — drop it, retry later, or fail the request. The block policy parks the publisher on a *second* condition variable until a consumer acknowledges a message and frees capacity, then lets it proceed. Two condition variables on the same mutex — one signaled by publishers for waiting pollers, one signaled by acknowledgments for waiting publishers — is a clean, deadlock-free way to express both directions of flow control.

Backpressure at the topic level treats all consumers as one. A subtler and very common problem is the *slow consumer* in a fan-out: one subscriber that cannot keep up while its siblings can. Blocking the publisher would penalize every consumer for the slowest one, so real brokers attach a per-subscriber overflow policy — drop the oldest queued message, drop the newest, or disconnect the laggard entirely — so that one slow reader degrades only its own stream. One of the exercises builds exactly this.

### Wildcard and pattern subscriptions

Flat topic names force a choice between too few topics (everything in one stream, consumers filtering by hand) and too many (a separate `Subscribe` call per concrete name). Hierarchical, dotted topic names with wildcard subscriptions resolve the tension. A publisher sends to a fully-specified subject like `orders.eu.created`; a subscriber expresses interest in a *pattern* like `orders.*.created` or `orders.#`. The broker matches each published subject against every registered pattern and fans the message out to all that match.

The two wildcards, drawn from AMQP and MQTT topic conventions, have precise and different meanings. A single-segment wildcard (written `*`) matches exactly one segment: `orders.*` matches `orders.created` but not `orders.eu.created`. A multi-segment wildcard (written `#`) matches zero or more trailing segments: `orders.#` matches `orders`, `orders.created`, and `orders.eu.created` alike. Getting the "zero or more" boundary right — and confining `#` to a trailing position — is the whole substance of the matcher, and an exercise builds and exhaustively tests it.

## Common Mistakes

### Protecting subscription state with a second mutex

Wrong: adding a `sync.Mutex` to `Subscription` and locking it inside `Poll`, then acquiring `s.topic.mu` while still holding it.

What happens: lock ordering becomes inconsistent across goroutines. `Ack` takes `s.topic.mu`; a concurrent `Poll` takes `s.mu` then `s.topic.mu`; a third path that happens to take them in the opposite order completes an AB-BA deadlock. The race detector will not save you here — it finds data races, not lock-ordering deadlocks — so the program simply hangs, often only under load.

Fix: use only `s.topic.mu` for all subscription state. Every read or write of the cursor or the in-flight records already happens while the topic lock is held, so a second lock buys nothing but a deadlock risk. One lock, one ordering, no cycles.

### Reading a shutdown flag under a different lock than the one that writes it

Wrong: storing `closed` on the subscription, writing it under `s.mu`, and reading it inside `Poll` under `s.topic.mu`.

What happens: the write and the read are synchronized by *different* mutexes, so the Go memory model offers no happens-before relationship between them. It is a textbook data race; `go test -race` flags it and the tests fail intermittently.

Fix: let the topic own the shutdown signal. `DeleteTopic` sets `t.closed = true` and broadcasts, all under `t.mu`; `Poll` reads `t.closed` under that same `t.mu`. One mutex guards both sides of the flag, so the write strictly happens-before the read.

### Busy-polling instead of waiting on the condition

Wrong:

```go
for len(topic.messages) == int(s.currentOffset) {
	time.Sleep(time.Millisecond)
}
```

What happens: it burns a CPU on every idle subscriber, it reads `topic.messages` without the lock (a data race on the slice header), and the sleep granularity adds up to a millisecond of latency to every message even when the system is otherwise idle.

Fix: `s.topic.cond.Wait()` inside the poll loop, with publishers calling `Broadcast` after each append. The subscriber consumes no CPU while waiting and wakes the instant a message arrives.

### Forgetting to Broadcast after Publish

Wrong: appending to `t.messages`, assigning the offset, and returning — without calling `t.cond.Broadcast()`.

What happens: a subscriber already blocked inside `Wait` is never woken, so it sleeps until its timeout even though its message is sitting in the log. A broadcast test that publishes and then expects an immediate delivery hangs until the test runner kills it.

Fix: make `t.cond.Broadcast()` the last step of every successful `Publish`. The signal is what converts a passive log append into an active delivery.

### Not wrapping sentinel errors with %w

Wrong: `return fmt.Errorf("invalid offset: %d", offset)`.

What happens: `errors.Is(err, ErrInvalidOffset)` returns false because the sentinel is not in the error's chain. Callers and tests that branch on the error type silently take the wrong path.

Fix: `return fmt.Errorf("%w: %d", ErrInvalidOffset, offset)`. The `%w` verb links the sentinel into the chain so `errors.Is` finds it while the formatted context is preserved for humans.

### Placing a multi-segment wildcard anywhere but the end

Wrong: accepting a pattern like `orders.#.created` and trying to match it segment by segment as if `#` consumed exactly one word.

What happens: `#` means "zero or more trailing segments," so it has no well-defined meaning in the middle of a pattern — should `orders.#.created` match `orders.created` (zero words for `#`, but then `created` must align) or `orders.eu.created`? The ambiguity produces a matcher that disagrees with itself depending on input length.

Fix: define `#` as legal only as the final segment, where "match the rest of the subject, including nothing" is unambiguous. Reject or document any non-trailing `#`, and let `*` be the tool for matching a single interior segment.

---

Next: [01-broker-core.md](01-broker-core.md)
