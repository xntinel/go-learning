# 10. Or-Done Channel Pattern — Concepts

The or-done pattern is the small, reusable wrapper that turns a plain
`<-chan T` into a cancellable one. A naive `for v := range in` reads a source
channel until it closes and offers no way to stop early; if the source never
closes, the ranging goroutine blocks forever and leaks. The or-done wrapper
fixes this by forwarding values from the source to a fresh output channel until
either the source closes or a caller-owned `done` signal fires, at which point
it returns and closes the output. That single property — a guaranteed exit on
either of two conditions — is what makes long-lived channel pipelines safe to
cancel, and it is the building block the Go team's "Advanced Go Concurrency
Patterns" talk calls a context-aware pipeline stage. This file is the
conceptual foundation; read it once and you will have everything you need to
reason through the three exercises, which build the wrapper, then apply it to
two production scenarios where getting cancellation wrong leaks goroutines: an
HTTP streaming endpoint whose client disconnects, and a subscription consumer
that must drain in flight before it shuts down.

## Concepts

### The Wrapper Owns Its Output Goroutine, the Caller Owns Done

The or-done wrapper spawns exactly one goroutine. That goroutine loops over the
source channel `in`, watches the caller's `done` channel, and forwards each
received value to a fresh `out` channel that the wrapper returns immediately to
the caller. The division of ownership is the whole contract. The wrapper owns
`out` and the goroutine that feeds it: it created them, it is the only writer,
and it is responsible for closing `out` on every exit path. The caller owns
`done`: the caller created it and the caller is the only one allowed to close
it. The wrapper never closes `done` — it only reads from it — because two
parties closing the same channel is a panic, and because "ask the wrapper to
stop" is the caller's decision to express, not the wrapper's.

The goroutine has two reasons to exit, and both must close `out`. It exits when
the source `in` is closed and drained (the natural end of the stream), and it
exits when `done` fires (an early cancel). Whichever happens first, a single
`defer close(out)` covers it, which is what lets a downstream stage stay an
ordinary `for v := range out` and terminate cleanly no matter which exit was
taken.

### The Double Select Is Not Optional

The core of the wrapper is two nested `select` statements, and the inner one is
the part everyone forgets. The outer select waits for either a value from the
source or a done signal:

```go
select {
case <-done:
	return
case v, ok := <-in:
	if !ok {
		return // source closed and drained
	}
	// ... now forward v
}
```

Once a value `v` has been received, it must be sent on `out`. The temptation is
to write `out <- v` directly. That is a latent deadlock. Between the instant the
value was received and the instant the send completes, the consumer may have
gone away and `done` may have fired. An unconditional `out <- v` to a channel
nobody is reading blocks the goroutine forever — the exact leak the wrapper
exists to prevent. The send must therefore be guarded by its own select:

```go
select {
case out <- v:
case <-done:
	return
}
```

Now the goroutine can always make progress: it either hands the value to a live
consumer or abandons it because `done` fired. Two selects, both watching
`done`: one around the receive, one around the send.

### Composability and the ctx.Done() Bridge

Because the wrapper returns a `<-chan T` that closes on exit, wrapped stages
chain: the output of one or-done wrapper is a valid input to the next, and a
single `done` shared across the chain tears the whole pipeline down at once.
The `done` channel is deliberately typed `<-chan struct{}` — a pure signal that
carries no value and costs no allocation — which is exactly the type
`context.Context.Done()` returns. That is not a coincidence: when the caller is
already holding a `context.Context`, it passes `ctx.Done()` straight in as the
done channel and the wrapper becomes context-aware for free. The HTTP streaming
exercise relies on precisely this: an inbound request's `r.Context()` is
cancelled by the `net/http` server the moment the client disconnects, so
`OrDone(r.Context().Done(), source)` stops streaming the instant the browser
closes the tab.

### The In-Flight Value Can Be Dropped — and When That Matters

The inner select hides a behavior you must understand before trusting the
wrapper with every kind of stream. When `done` fires while a value is in flight
— received from `in` but not yet sent on `out` — the inner select may choose
the `<-done` case and `return`, and that value is gone: consumed from the
source, never delivered downstream. For a stream where dropping the last item
on cancel is harmless, this is exactly right. A client that just disconnected
from a live event feed does not care that one final event was discarded. But
for an at-least-once consumer — a message-queue subscription where every
message must be processed — silently dropping the in-flight value is data loss.
That is why the subscription exercise does not reach for the dropping wrapper on
its consume path. It uses the same or-done shutdown discipline (select on a stop
signal and on the source), but loss-free: it never receives a value it is not
prepared to process, and after the stop signal it drains the source to
completion before unsubscribing. The or-done family has two members, and a
senior engineer picks by whether a dropped tail value is acceptable.

### Goroutine Leaks Are the Bug This Pattern Prevents

Every goroutine you start must have a guaranteed path to return. A goroutine
blocked forever on a channel send or receive is never garbage-collected: its
stack, and everything its closure captures, stays live for the life of the
process. This is the canonical Go concurrency bug, and it is invisible to the
compiler and to a passing unit test — the leaked goroutines simply accumulate
under load until the process is starved. Two leaks lurk around any channel
forwarder. The forwarder itself leaks if it blocks on `out <- v` with no
consumer (the inner select fixes this). The producer feeding the source leaks
if it blocks on a send into a source channel that the forwarder has stopped
reading — so the producer must also watch the same `done`/`ctx`, which is why
the exercises build the source generator with its own cancellation arm, not
just the wrapper.

The exercises assert the absence of leaks directly. Without an external
dependency, the technique is to snapshot `runtime.NumGoroutine()` after the
system is idle, exercise the cancel path, then poll until the count returns to
the baseline; if it never does, a goroutine leaked. The polling matters because
goroutines unwind asynchronously — cancellation signals a goroutine to exit but
does not block until it has — so a correct test waits for the count to settle
rather than reading it once. Production code reaches for `go.uber.org/goleak`,
which automates the same snapshot-and-compare in a `TestMain`; the exercises use
the standard library so each module stays dependency-free and gates in
isolation.

### Mac-Native Notes: Scheduling and the Race Detector

The Go runtime multiplexes goroutines onto OS threads, capping the number that
run Go code simultaneously at `GOMAXPROCS`, which defaults to
`runtime.NumCPU()` — on Apple Silicon that counts every logical core, including
the efficiency cores, so a forwarder and its producer genuinely run in parallel
on a Mac and any unsynchronized shared state is a real race, not a theoretical
one. The race detector is fully supported on `darwin/arm64` and `darwin/amd64`,
so `go test -race` is the right tool here and every exercise is written to pass
under it. Run each exercise's verification with `-race`; the detector is what
catches a forgotten guard on a shared `done` close or a send racing a close,
and it is cheap enough to leave on for the whole concurrency chapter.

## Common Mistakes

### Skipping the Inner Select Around the Send

Wrong: receiving a value in the outer select, then sending it with a bare
`out <- v`.

What happens: if `done` fires (or the consumer simply stops reading) between the
receive and the send, the send has no reader and blocks forever. The wrapper
goroutine leaks — the precise failure the pattern was supposed to eliminate.

Fix: guard the send with its own `select { case out <- v: case <-done: return }`
so the goroutine can always either deliver the value or abandon it and exit.

### Forgetting to Close the Output Channel

Wrong: forwarding values without a `defer close(out)`.

What happens: when the source drains or `done` fires, the wrapper goroutine
returns but never closes `out`. The downstream `for v := range out` blocks
forever on a channel that will never receive again or close — a hang that looks
like a deadlock far from its cause.

Fix: `defer close(out)` as the first statement of the goroutine. It fires on
every return path: source drained, done fired, or source closed before any
value arrived.

### Closing Done From Inside the Wrapper

Wrong: the wrapper closes its own `done` channel to "clean up."

What happens: the caller also closes `done` to cancel, and a second close of a
closed channel panics, crashing the program at an unpredictable time.

Fix: the wrapper only ever reads from `done`. Closing it is the caller's sole
responsibility; ownership of the signal stays with whoever created it.

### Assuming No Value Is Ever Lost

Wrong: using the dropping wrapper on a path where every value must be processed,
such as a durable message subscription.

What happens: when the stop signal fires while a value is in flight, the inner
select may discard it. The message was acknowledged off the source but never
handled — silent data loss that no test of the happy path will reveal.

Fix: on at-least-once paths, do not let the forwarder receive a value it might
drop. React to the stop signal, then drain the source to completion before
releasing it, as the subscription exercise does.

### Cancelling the Wrapper but Not the Producer

Wrong: wiring `done` into the wrapper but leaving the producer goroutine
blocked on an unconditional send into the source channel.

What happens: the wrapper exits on `done`, but the producer is still blocked
trying to push the next value into a source nobody reads. The producer leaks
even though the wrapper was "cancelled correctly."

Fix: give the producer the same `done`/`ctx` arm in its own select, so the
entire chain — producer, wrapper, consumer — tears down on one signal.

---

Next: [01-or-done-wrapper.md](01-or-done-wrapper.md)
</content>
