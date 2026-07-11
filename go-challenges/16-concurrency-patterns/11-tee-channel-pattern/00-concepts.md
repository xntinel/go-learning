# 11. Tee Channel Pattern — Concepts

The tee pattern takes one inbound channel and hands every value it carries to two independent consumers. It is the channel-level analogue of the Unix `tee(1)` command: the stream flows through unchanged, but two readers see every element. In a backend it is how you feed one event stream to both the thing that acts on it and the thing that observes it — a processor and an audit log, a request handler and a tracer, a state machine and a metrics aggregator. This file is the conceptual foundation. Read it once and the four exercises, each a self-contained Go module, will build the pattern from the simplest correct form up to the asymmetric, drop-tolerant tees that real telemetry pipelines depend on.

## Tee duplicates, it does not filter

The defining property of a tee is that every value on the input is delivered to both outputs, unchanged, in order. A tee never drops a value because of its content, never transforms it, never routes it to one side based on a predicate. "Send each value to two places" is tee; "send each value to one of two places based on what it is" is a different pattern (call it `Split` or `Route`) and conflating the two is the most common conceptual error. If you find yourself wanting one consumer to see only some values, you do not want a tee with a filter bolted on, you want a router. Keep them distinct.

A direct corollary: closing the input must close both outputs. The wrapper goroutine ranges over the input; when the input closes, the range loop ends, and the wrapper closes both output channels so that each consumer's `for v := range out` loop terminates cleanly. Closing only one output is a classic bug — the other consumer's range loop blocks forever, and the goroutine that was supposed to drain it leaks. Both closes belong in `defer` statements at the top of the wrapper goroutine so that no early return can skip one.

## The coupling that every tee designer must understand

The hard part of a tee is not duplication, it is what happens when the two consumers run at different speeds. A synchronous channel send blocks until a receiver takes the value, so the moment a tee sends to a slow consumer, the wrapper itself is blocked, and while it is blocked it is not advancing the input. The source can therefore go no faster than the slower of the two consumers. This coupling is not a bug; it is backpressure, and backpressure is usually what you want. But it has two distinct components and they are fixed by different means.

The first is ordering coupling. If the wrapper always sends to `out1` before `out2`, then within a single value a fast `out2` consumer is made to wait for a slow `out1` consumer, even though `out2` was ready first. This artificial ordering is removed with a `select` that offers the value to both outputs at once and sends to whichever is ready first, disabling that case afterward by setting its channel variable to `nil` — a nil channel blocks forever in a select, so a satisfied output is simply never chosen again for that value. The Go Blog's pipelines article uses exactly this construction. It removes the arbitrary "out1 first" rule, but it does not let one consumer get ahead of the other: the wrapper still waits for both to take each value before advancing.

The second is rate coupling, and this is the one people get wrong. Adding a buffer to each output lets a consumer fall briefly behind without blocking the wrapper — the value lands in the buffer and the wrapper moves on. This smooths jitter: a consumer that pauses for one tick and then catches up never stalls the source. But a buffer of any fixed size does **not** decouple sustained rates. If one consumer is permanently slower than the source, its buffer fills, and once full the wrapper blocks on it exactly as before. Buffering buys you slack equal to the buffer depth; it does not buy you independence. The only way to let one consumer run indefinitely behind another is to stop guaranteeing it every value — that is, to drop. Any claim that "a buffered tee isolates a slow consumer from a fast one" is false for sustained load and is the precise trap this lesson exists to dispel.

## Cancellation is independent of rate

A separate axis is teardown. A consumer may need to stop reading early — it hit an error, its context was cancelled, the request it served completed. If the wrapper is blocked on a send to that consumer, it will block forever unless it is also watching a done signal. Giving each output its own done channel, and wrapping each send in `select { case out <- v: case <-done: }`, lets one consumer detach without disturbing the other: the wrapper simply stops offering values to the departed output and keeps feeding the survivor every value. This is cancellation independence, and it is genuinely independent — a cancelled consumer imposes nothing on the other. It is worth being precise that cancellation independence is not rate independence: skipping a *cancelled* output is free, but a *slow but live* output still applies backpressure. The two are solved by different mechanisms and must not be confused.

## The asymmetric tee: lossless work, best-effort telemetry

The senior pattern, and the reason a tee is so common in production, is the asymmetric one. You have a hot path that must see every value — the processor, the request handler — and you have an observer that would be nice to feed but must never be allowed to slow the hot path down — the audit log, the metrics sink, the tracer. Treating both outputs identically is the mistake. The right design gives them different congestion policies.

The hot path is lossless: the wrapper sends to it with a plain blocking send, so the source's pace is governed by the work that actually matters, and no value is ever lost. The observer is best-effort: the wrapper sends to it with a non-blocking send, `select { case obs <- v: default: drops++ }`, behind a bounded buffer. When the observer keeps up, it sees everything; when it falls behind, its buffer fills and further values are dropped and counted, and crucially the wrapper never blocks on it. Telemetry congestion can therefore never stall production work. This is "independent backpressure" in the precise sense that matters: each output's congestion is handled by its own policy, so neither sink's slowness is transmitted to the other. The lossless path applies backpressure to the source; the best-effort path applies backpressure to no one and pays for its protection in dropped samples, which it counts so the loss is observable rather than silent.

A bounded buffer plus drop-and-count is the whole trick. The two temptations to resist are making the observer lossless too — which couples it to the hot path and defeats the purpose — and making its buffer unbounded as a "fix" for drops, which converts a bounded, observable loss into an unbounded memory leak that eventually takes the process down under exactly the load spike you most needed it to survive. Drops are not a failure of the design; they are the design working. Count them and export the count.

## Sampling: deciding what to observe before you pay for it

Tracing pushes the asymmetric tee one step further. Even a best-effort copy of every request costs something to produce, and at high request rates you neither need nor want a trace of every call. Sampling makes the selection explicit. Head-based sampling decides as early as possible — at the tee, by sequence number or a random draw — whether a given request is even offered to the tracer, so the unsampled majority costs nothing beyond the modulo check. OpenTelemetry calls this head sampling and contrasts it with tail sampling, which buffers complete traces to decide after the fact. For a channel tee, head sampling is the natural fit: select one request in N, mirror only the selected ones to the tracer, and send even those with a non-blocking send so a stalled tracer drops its sample rather than blocking the request path. The serve path stays lossless and fast; the trace path is both sampled and drop-tolerant.

## Common Mistakes

### Closing one output but not the other

Closing only `out1` leaves `out2`'s consumer blocked in its range loop forever and leaks the goroutine that drains it. Put both `defer close(...)` lines at the top of the wrapper goroutine so no path can skip one. The order of the two closes does not matter functionally — both run when the goroutine returns — so do not reason about LIFO close order as if it changed behavior; it does not.

### Starting the wrapper before any receiver exists

With unbuffered outputs, the wrapper's first send blocks until a consumer reads, and if the consumers are scheduled after a blocking operation that itself waits on the wrapper, you deadlock. Either buffer the outputs, or make sure the consumer goroutines are launched before the wrapper produces its first value. A buffered, already-closed source channel sidesteps this in tests.

### Treating a tee as a filter

Expecting `Tee(src)` to hand one consumer a subset of values is a category error: a tee delivers every value to every output. If you want content-based selection, write a router with an explicit predicate. Do not bolt a filter onto a tee.

### Believing a buffer decouples sustained rates

A per-output buffer smooths momentary jitter; it does not let a permanently slow consumer fall arbitrarily behind. Once the buffer fills, the wrapper blocks on that output again, and the source is throttled to the slow consumer's pace. The only mechanism that decouples sustained rates is dropping. Stating otherwise is the canonical tee falsehood.

### Letting telemetry block the hot path

Sending to an audit log, metrics sink, or tracer with a blocking send couples production work to the slowest observer. Use a non-blocking send behind a bounded buffer and count the drops. Telemetry must degrade by losing samples, never by slowing the work it observes.

### "Fixing" drops with an unbounded buffer

Replacing a bounded best-effort buffer with an unbounded one to eliminate drops trades a visible, harmless loss for an invisible memory leak that crashes the process under load. Keep the buffer bounded, keep the drop counter, and treat a rising drop rate as the signal it is.

Next: [01-basic-tee.md](01-basic-tee.md)
