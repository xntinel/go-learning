# 16. Pub/Sub With Channels — Concepts

Publish/subscribe is the broadcast primitive of an event-driven system: a publisher emits an event and every interested subscriber receives a copy, with neither side holding a reference to the other. In Go the natural substrate is the channel, and the whole design problem reduces to three questions that this chapter answers in code. Who owns the set of subscribers, and how is that set mutated safely while events are in flight? What happens when one subscriber reads slower than the publisher writes — does the publisher block, does the event get dropped, or does a buffer absorb the difference? And how does the whole thing shut down so that not a single goroutine is left blocked on a channel that will never receive again? Read this file once and you have the conceptual frame for all three exercises, which build the same broadcast idea three different ways: a mutex-guarded broker, a handler-based domain event bus with an explicit slow-subscriber policy, and a single-goroutine fan-out broadcaster with context-driven shutdown.

## Concepts

### One Inbound, N Outbound

Every pub/sub implementation, however it is dressed up, is the same shape: one logical inbound stream of events and N outbound streams, one per subscriber. The publisher writes once; the machinery copies that write to each subscriber's outbound path. Each subscriber reads from its own path, so a slow reader on one path does not, by itself, corrupt or reorder another path's stream. The Go talk "Advanced Go Concurrency Patterns" uses exactly this one-inbound-N-outbound figure as the building block for event systems, and it is worth holding in mind because the three exercises differ only in what the "outbound path" physically is: a buffered channel the subscriber ranges over, a buffered channel drained by a dedicated goroutine that calls a handler, or a fresh channel handed back from a single owning goroutine.

### Who Owns the Subscriber Set

The subscriber set is mutable shared state — `Subscribe` adds, `Unsubscribe` removes, `Publish` reads — and the entire correctness of a pub/sub system turns on how that state is owned. There are three disciplined answers, and the chapter builds one of each.

The first is a mutex-guarded map. The broker holds a `map[id]*subscription` behind a `sync.RWMutex`: writers (subscribe, unsubscribe, close) take the write lock, and a publish takes the read lock to find its targets. This is the most direct translation of the data structure into code, and it is what most in-process brokers actually use. Its one sharp edge is the interaction between sending an event and closing a channel, covered next.

The second is one goroutine per subscription. Instead of the broker sending directly into a subscriber's channel, each subscription owns a small goroutine that pumps from a bounded inbound channel and invokes the subscriber's handler. The broker's job shrinks to delivering an event into the right inbound channels; the per-subscription goroutine isolates a slow handler so it cannot stall the registry. This is the push (callback) model, and it is how an in-process domain event bus typically delivers to handlers that do real work.

The third is a single owning goroutine, the actor model. No mutex at all: one goroutine owns the subscriber set outright, and `Subscribe`, `Unsubscribe`, and `Broadcast` are messages sent to it over channels. Because only that one goroutine ever touches the set or the output channels, there is no shared mutable state to race over and nothing to lock. The price is that the owning goroutine is a serialization point, so a slow send inside it stalls every subscriber — which is exactly the trade-off the fan-out exercise makes visible.

### The Send-on-Closed-Channel Hazard

This is the single most important correctness fact in channel-based pub/sub, and the bug it causes is a panic, not a quiet wrong answer. Sending on a closed channel panics; closing an already-closed channel panics; but receiving from a closed channel is always safe and yields the zero value with `ok == false`. A broadcaster therefore has a races-against-itself problem: `Publish` wants to send into a subscriber's channel at the same moment `Unsubscribe` or `Close` wants to close that same channel.

The tempting but broken design is to snapshot the target subscribers under a read lock, release the lock, and then send. It reads as an optimization — do not hold the lock during a potentially slow send — but it opens a window: between releasing the lock and performing the send, another goroutine can acquire the write lock, remove the subscriber, and close its channel. The subsequent send then panics on a closed channel. The window is small, which is worse, not better: it survives light testing and fails in production under load. The race detector catches it only if a test actually publishes and unsubscribes concurrently.

There are exactly two disciplined fixes. The first is to send and close under the same lock: perform the send while holding the read lock, and close only while holding the write lock. Because a read lock and a write lock are mutually exclusive, a send can never overlap a close, and the panic is structurally impossible. To keep the read lock's hold time bounded, the send must be non-blocking (a `select` with a `default`), which is what couples this fix to an explicit drop policy. The second fix is to never close the data channel from the broker at all: give each subscription a separate `quit` (or `done`) channel, signal shutdown by closing `quit`, and have every send be a `select { case ch <- e: case <-quit: }`. A closed `quit` releases a blocked sender without anyone ever sending on a closed data channel. The domain event bus uses this second discipline; the broker uses the first.

### Slow Subscribers and the Delivery Policy

A subscriber that reads more slowly than the publisher writes is not an error case — it is the normal case, and a pub/sub system is mostly a set of decisions about what to do about it. There are four standard policies and each is a different point on the same trade-off curve.

Block (backpressure): the publisher waits until the slow subscriber makes room. This never loses an event, but it couples the publisher's rate to the slowest subscriber's rate, which is often unacceptable — one stuck consumer freezes the entire bus. Bounded buffer: each subscriber gets a buffered channel of fixed depth; the publisher blocks only when that specific buffer is full. This decouples up to the buffer depth and converts the slow subscriber from an immediate stall into a delayed one. Drop-newest: when the buffer is full, the new event is discarded and counted; the publisher never blocks. This is the right choice when stale-but-flowing beats complete-but-stalled — metrics, telemetry, live dashboards. Drop-oldest: when the buffer is full, the oldest queued event is evicted to make room for the newest, keeping the freshest window of events; this is what a ring-buffer subscriber does.

The cost model is worth stating plainly. With a buffer of depth `B` and a producer that is faster than a consumer by `d` events per second, a block policy paces the producer down to the consumer's rate; a drop policy lets the producer run free and discards `d` events per second once the `B`-deep buffer saturates after `B/d` seconds. Real systems make this choice explicit and observable: NATS disconnects a "slow consumer" once its outbound buffer overflows; a Kafka consumer that falls behind accumulates measurable "consumer lag" rather than dropping, because Kafka's buffer is the durable log itself. The lesson is that there is no universally correct policy, only a correct policy for a given event's value — so the policy must be a named, configurable decision, not an accident of buffer sizing.

### Backpressure Versus Decoupling

The block and drop policies are the two ends of a single axis: how tightly is the publisher coupled to the subscribers? Full backpressure (block, unbuffered) maximizes coupling and guarantees delivery at the cost of shared fate — the system runs at the speed of its slowest member. Full decoupling (drop, or an unbounded buffer) minimizes coupling and protects the publisher's rate at the cost of either lost events (drop) or unbounded memory (an unbounded buffer, which is almost always a latent out-of-memory bug). A bounded buffer with an explicit overflow policy is the engineering middle: enough slack to absorb bursts, a hard ceiling on memory, and a defined, observable behavior when the ceiling is hit. When you design a bus, you are choosing a point on this axis for each class of event, and writing that choice down as the buffer depth plus the overflow policy.

### Clean Shutdown and Goroutine Leaks

A goroutine leak is a goroutine that can never make progress and will never return: it is blocked forever on a channel operation that will never complete. Pub/sub is a rich source of leaks because it is full of long-lived goroutines parked on channels. The two classic leaks are a consumer ranging over a channel that is never closed (the `for range ch` never ends), and a sender blocked on a send into a channel that has no remaining receiver. The race detector does not flag leaks directly, but a leaked goroutine usually shows up either as a test that hangs or as a `-race` run that reports a leak via a goroutine still touching shared state after the test ended; the surest discipline is to give every goroutine an explicit termination path and to join them.

Three mechanisms provide that termination path. Closing the data channel: once the broker closes a subscriber's channel, the subscriber's `for range` drains the buffered remainder and then exits cleanly — this is why `Close` must close every subscriber channel exactly once. A separate done/quit channel: a goroutine selecting on both its work channel and a `quit` channel returns the moment `quit` is closed, without the work channel ever being closed. And `context.Context` cancellation: a `ctx.Done()` case in a `select` is the idiomatic, composable way to thread a single cancellation signal through a whole tree of goroutines, which is how the fan-out broadcaster tears everything down from one `cancel()` call. In every case the shutdown owner must also wait for the goroutines to actually finish — a `sync.WaitGroup` or a per-goroutine `done` channel — before declaring the system closed, or the "shutdown" returns while leaked goroutines are still running. Verifying all of this is the job of `go test -race`, which on macOS (including Apple Silicon, `darwin/arm64`) instruments memory accesses to flag any unsynchronized access the moment two goroutines touch the same location without a happens-before edge between them. These patterns are operating-system independent — there is no fsync or platform syscall here — but every module in this chapter is built and checked with `-race` on macOS, and a passing `-race` run plus a clean shutdown that joins every goroutine is the bar each exercise must clear.

## Common Mistakes

### Sending After Releasing the Lock

Wrong: snapshot the target subscribers under the read lock, release the lock, then send into their channels. It looks like a way to avoid holding the lock during a slow send.

What happens: between the release and the send, another goroutine takes the write lock, unsubscribes a target, and closes its channel. The send then panics with "send on closed channel". The window is small enough to pass casual testing and fail under concurrent load.

Fix: either send while still holding the read lock (closes happen only under the write lock, so a send and a close are mutually exclusive) and make the send non-blocking so the lock is not held long; or never close the data channel from the broker and signal shutdown through a separate `quit` channel selected alongside every send.

### Treating Publish as Always Lossless

Wrong: assuming `Publish` either always blocks until delivered or always succeeds, without deciding which.

What happens: an undecided policy becomes an accidental one. An unbuffered or full channel with a blocking send silently turns one stuck subscriber into a frozen publisher; a non-blocking send with no accounting silently drops events with no record that it happened.

Fix: choose the slow-subscriber policy deliberately — block, bounded buffer, drop-newest, or drop-oldest — make it a named option, and when the policy is drop, count the drops so the loss is observable.

### Ranging Over a Channel That Is Never Closed

Wrong: hand a subscriber a channel to `for range` over, but on unsubscribe just remove it from the registry without closing the channel.

What happens: the subscriber's range loop blocks forever waiting for a value or a close that never comes. The goroutine leaks; over a long-lived process these accumulate until the program exhausts memory or scheduler capacity.

Fix: close the subscriber's channel exactly once when it is removed, or give the subscriber goroutine a `quit`/`ctx.Done()` case it can return on. Whichever you choose, make sure the shutdown path waits for the goroutine to actually exit.

### Double Close

Wrong: closing a subscriber channel in both `Unsubscribe` and `Close`, or calling `Close` twice, so a channel is closed a second time.

What happens: closing an already-closed channel panics, taking down the process.

Fix: remove a subscription from the registry before closing its channel so a later `Close` cannot find it again, and guard the broker's own teardown with a `sync.Once` so `Close` is idempotent.

---

Next: [01-channel-broker.md](01-channel-broker.md)
