# 9. Observer Pattern with Channels — Concepts

The observer pattern decouples the thing that emits events from the things that react to them. In classic object-oriented code an observer registers a callback and the subject invokes it synchronously, on the subject's own goroutine, one observer at a time. Go offers a more natural mechanism: a channel. The publisher sends an event onto a channel and a subscriber receives from it, so the two run on their own goroutines, at their own pace, with the channel's buffer as the shock absorber between them. This file is the conceptual foundation. Read it once and you will have what you need to reason through each exercise, which build the idea piece by piece as independent, self-contained Go modules: a complete event bus, a concurrency-safe subscribe/unsubscribe broadcaster, and a non-blocking fan-out that drops on a full buffer.

## Concepts

### Channels Are The Observer's Handle

A subscriber receives its own channel from `Subscribe`. The publisher does not know or care how many subscribers exist; it knows only the type of event it emits. The channel is the handle that decouples the publisher's pace from the subscriber's pace, and the channel's buffer is what lets the two run independently: a publisher that is briefly faster than a subscriber fills the buffer rather than blocking, and a subscriber that is briefly faster blocks on an empty channel rather than spinning. This is the same idea as a callback list, but the queue between subject and observer is now a first-class language object you can buffer, select on, time out, and close.

The publisher side is deliberately ignorant. It holds a map from event type to a slice of subscriber channels and, on publish, walks the slice for the matching type and hands each channel the event. Adding a subscriber appends a channel; removing one deletes it. No subscriber identity, no callback registry, no virtual dispatch — just channels in a slice.

### Buffered Channels Plus A Non-Blocking Send Protect The Publisher

If a subscriber is slow, a plain synchronous send `ch <- event` stalls the publisher until that one subscriber is ready, and every other subscriber's event waits behind it. That couples the publisher's throughput to its slowest observer, which is exactly the coupling the pattern exists to remove. The fix is a buffered channel plus a non-blocking send:

```
select {
case ch <- event:
default:
}
```

If the buffer has room the event is delivered; if it is full the `default` arm fires and the event is dropped for that subscriber only. The trade-off is explicit and is the whole point: this design chooses availability (the publisher never blocks) over delivery (a slow subscriber loses events). Production variants count the drops, log them, or spill to a secondary queue; the policy is a knob, but the mechanism — buffered channel, `select` with `default` — is the same. The fan-out exercise makes the drop count a first-class, testable number.

### Closing The Channel Signals End-Of-Stream

Unsubscription removes the subscriber from the publisher's slice and closes its channel. Closing is the language-level "no more values will ever arrive" signal, and the two receiving idioms both react to it cleanly. A subscriber looping `for event := range ch { ... }` exits the loop when the channel closes. A subscriber selecting on several channels writes `case ev, ok := <-ch:` and treats `ok == false` as "this stream ended." The alternative — sending a sentinel value such as an event whose type is `"EOF"` — forces every receiver to test the type on every event and silently breaks the day one receiver forgets the check. Closing is the correct primitive; sentinels are a bug waiting for a deadline.

There is one rule closing imposes, and it is the subtlest hazard in the whole pattern: it is a panic to send on a closed channel, and a non-blocking send does not save you. `select { case ch <- event: default: }` does not fall through to `default` when `ch` is closed — the send case is chosen and the send panics. So the moment a design both closes subscriber channels (to signal end-of-stream) and sends to them non-blockingly (to protect the publisher), it has a latent "send on closed channel" panic unless publishing and closing are made mutually exclusive. The robust answer used throughout these exercises is to perform the non-blocking send while holding the same lock that `cancel` and `Close` take to close a channel. Because the send has a `default` arm it never blocks, so holding the lock across the fan-out is bounded by the number of subscribers and cannot deadlock; and because closing needs that same lock, no channel can be closed in the window between selecting it and sending to it.

### Unsubscribe Must Be Idempotent And Closed Exactly Once

A channel may be closed exactly once; closing a closed channel panics. Two forces push toward a double close. First, callers reasonably expect `cancel()` to be safe to call more than once (a `defer cancel()` plus an explicit early `cancel()` is ordinary code). Second, a bus-wide `Close` also closes every subscriber channel, so an unsubscribe racing with a shutdown could try to close the same channel twice. Both are solved the same way: guard the close with a `sync.Once` so repeated cancels collapse to one, and have every close path first check, under the lock, that the channel is still registered before closing it. If `Close` already removed and closed it, a later `cancel` finds it absent and does nothing; if `cancel` removed it first, `Close` never sees it. The registry membership check under the lock is what makes "closed exactly once" hold even when the two paths race.

### Each Subscriber Runs On Its Own Goroutine

The pattern is concurrent by design: the publisher does not run handlers, it hands off events, and each subscriber runs its handler on its own goroutine reading from its own channel. This is what lets three observers react to the same event in parallel instead of in series. The bus itself does not start those goroutines — that is the caller's responsibility, because the caller owns the handler logic and its lifetime — but every consumer of the bus starts one goroutine per subscription and ties its lifetime to the channel: the goroutine lives as long as the channel is open and exits when it closes. Forgetting to start the goroutine means events pile up in the buffer and then drop; forgetting to ever close the channel means the goroutine blocks forever and leaks.

### Graceful Shutdown Drains, It Does Not Guarantee

`Close` flips the bus to a closed state so no new subscribers can join and no new events are accepted, then closes every subscriber channel. Events already buffered in a subscriber's channel are still there and are still drained by that subscriber's `for range` loop before it sees the close and exits. What `Close` does not promise is that every event ever published reaches every subscriber: an event dropped earlier because a buffer was full is gone, and an event still in flight at the instant of close is delivered only if it was already buffered. The guarantee is narrower and honest: after `Close` the publisher stops, every subscriber channel closes, and every subscriber goroutine exits cleanly with no leak. Coordinate that exit with a `sync.WaitGroup` — `Add` before starting each goroutine, `Done` inside it, `Wait` after `Close` — so the program does not exit while a goroutine is still draining.

### Generics Make The Bus Reusable Without `any`

A bus typed as `chan Event` where `Event.Payload` is `any` works, but every subscriber pays for it with a type assertion `ev.Payload.(OrderPlaced)` that can panic at runtime. A generic `Broadcaster[T]` or `Fanout[T]` pushes the element type into the type system: the channel is `chan T`, the send is checked at compile time, and the receiver gets a `T` with no assertion. The event-bus exercise keeps the `any` payload because its job is to route many event types through one bus by string key; the broadcaster and fan-out exercises are single-type by design and use generics so the compiler, not a runtime assertion, enforces the element type.

## Common Mistakes

### Sending To A Channel That Cancel Or Close May Have Closed

Wrong: snapshot the subscriber slice under the lock, release the lock, then run `select { case ch <- event: default: }` over the snapshot. Between the unlock and the send, `cancel` or `Close` closes `ch`, and the non-blocking send panics with "send on closed channel" instead of taking the `default` arm.

Fix: perform the non-blocking fan-out while still holding the lock that `cancel` and `Close` use to close channels. The send has a `default` arm so it never blocks; holding the lock across a bounded, non-blocking loop cannot deadlock, and it makes "send" and "close" mutually exclusive so no channel is ever closed in the send window.

### Closing The Same Channel Twice

Wrong: `cancel()` closes the channel, and a later `Close()` (or a second `cancel()`) closes it again, panicking with "close of closed channel."

Fix: wrap the close in a `sync.Once` so repeated cancels collapse to one, and before closing always check, under the lock, that the channel is still registered. Whichever path removes it from the registry first owns the close; the other finds it gone and does nothing.

### Signaling End-Of-Stream With A Sentinel Value

Wrong: `ch <- Event{Type: "EOF"}` to tell a subscriber to stop.

Fix: `close(ch)`. A `for range` loop exits on close; a `select` reads `ok == false` and exits. Closing is the language primitive for "no more values"; a sentinel forces a type check on every event and breaks silently the day one check is missing.

### Starting Subscriber Goroutines Without Coordinating Shutdown

Wrong: `go func() { for ev := range ch { ... } }()` with no `WaitGroup`, then letting `main` return.

Fix: `wg.Add(1)` before each goroutine, `defer wg.Done()` inside it, and `wg.Wait()` after closing the bus, so the program does not exit while a goroutine is still draining buffered events.

### Publishing Synchronously From Inside A Handler

Wrong: a handler that calls `bus.Publish(...)` on the same goroutine while handling an event. With a lock held across the fan-out this deadlocks (the publish re-enters the lock it already holds); without one it can recurse or block on a full buffer.

Fix: hand the follow-up event to a fresh goroutine — `go bus.Publish(...)` — so the handler returns and the reactive chain runs on its own stack. Reactive engines that need long chains use an explicit event loop or a second bus rather than re-entrant publishing.

---

Next: [01-event-bus.md](01-event-bus.md)
