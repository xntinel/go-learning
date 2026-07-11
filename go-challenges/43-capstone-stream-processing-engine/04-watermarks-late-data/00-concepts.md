# 4. Watermarks and Late Data — Concepts

Distributed stream pipelines see events arrive out of order. A record with an event timestamp of 09:00:05 may arrive seconds after a record timestamped 09:00:10, because network hops, buffering, and clock skew are all sources of reordering. A windowing engine that closes windows based solely on observed timestamps will either wait forever (unsafe) or miss records that arrive late (incorrect). Watermarks solve this by letting the engine make a bounded, heuristic assertion: "no event with a timestamp earlier than W will arrive from this source." When the watermark passes a window's end time, the window can be closed and its result emitted. This file is the conceptual foundation for the whole chapter. Read it once and every exercise — per-source and global watermark tracking, periodic and punctuated watermark generators, the event-time window operator that fires from the watermark, the late-data handler with three routing policies, and the watermark-lag monitor — follows from it.

## Event Time Versus Processing Time

Every record in a stream has two clocks attached to it. The *event time* is when the event actually occurred, stamped by the producer (a sensor, a browser, an API server). The *processing time* is when the record arrives at the stream engine. In a healthy, low-latency pipeline the two clocks agree closely. In practice they diverge: network queues, Kafka consumer lag, micro-batches, and mobile clients that buffer events offline all introduce gaps.

A window that closes based on processing time is simple but wrong: two runs of the same pipeline over the same data produce different results depending on how fast the machine was and how traffic happened to be scheduled. A window that closes based on event time produces repeatable results, but needs a mechanism to decide when "enough" event-time has elapsed that it is safe to close. That mechanism is the watermark.

## Watermarks as Bounded Heuristics

A watermark W(t) at processing time t is the engine's assertion that all events with an event timestamp earlier than W(t) have already been observed. This is a *heuristic*, not a guarantee. The bound on out-of-orderness — the maximum amount by which a record is expected to be late relative to the highest timestamp already seen — is the parameter that controls the trade-off:

- A large bound: the watermark lags far behind the real maximum observed timestamp, windows close later, fewer late records, higher end-to-end latency.
- A small bound: the watermark tracks the maximum observed timestamp tightly, windows close sooner, more late records, lower latency.

The per-source watermark is therefore `WM_source = max_observed_event_time - out_of_orderness_bound`. The watermark is per-source because different Kafka partitions, HTTP sources, or file shards progress at different rates, and a single global timestamp would either be too conservative for the fast partitions or too aggressive for the slow ones.

A watermark must be monotonic: it never moves backwards. Because the bound is subtracted from the *maximum* observed timestamp, and the maximum only ever grows, the per-source watermark is naturally non-decreasing. An out-of-order record with a small timestamp does not lower the maximum, so it cannot lower the watermark.

## Watermark Generation: Periodic Versus Punctuated

There are two strategies for *when* a generator emits a new watermark, and a senior engineer is expected to know the difference because it determines the latency-versus-overhead trade-off of the whole pipeline.

A *periodic* generator emits a watermark on a fixed timer — every 200 ms, say — regardless of how many events arrived in between. The generator updates its running maximum on every event, but it only produces a watermark value when the timer fires. This decouples watermark advancement from event arrival: even a low-traffic source advances its watermark on schedule, and a high-traffic source does not flood the pipeline with one watermark per event. Periodic generation is the default in production engines.

A *punctuated* generator emits a watermark only in response to a special marker in the stream — a flush record, an end-of-batch sentinel, or a field on the event itself that says "this is a checkpoint boundary." It produces exactly one watermark per marker and nothing in between. Punctuated generation gives precise control when the source itself knows where safe boundaries lie (for example, a source that emits a marker after draining a file), at the cost of more watermarks when markers are dense.

Both strategies share two rules. First, *suppress non-advancing watermarks*: if the newly computed watermark is not strictly greater than the last one emitted, emit nothing. Re-emitting the same watermark wastes downstream work and can confuse operators that treat each watermark as progress. Second, both compute the watermark the same way — running maximum minus the out-of-orderness bound — they differ only in the cadence of emission.

## The Global Watermark: Minimum Across Active Sources

A multi-source pipeline cannot close a window until *all* sources have passed the window boundary, because any source might still deliver a record inside the window. The global watermark is therefore the minimum across the per-source watermarks: `WM_global = min(WM_source_1, ..., WM_source_N)`.

This is the slowest-source-wins property: one slow or stalled source blocks event-time progress for the entire pipeline. Two mitigations apply.

First, **idle source exclusion**. If a source has emitted no records for longer than a configurable idle timeout, exclude it from the minimum. The source is assumed to have finished or stalled externally; the pipeline should not freeze because of it. Without this, a partition that produces nothing overnight would hold the global watermark at last night's value and no window would ever fire.

Second, **monotonicity enforcement**. The global watermark must never decrease. Once `WM_global = T`, the engine has already emitted results for windows ending before T; allowing it to go backwards would require retracting those results unconditionally. The implementation enforces `WM_global = max(WM_global_old, WM_global_new)`. This matters specifically when a new, slow source is added to a running pipeline: its low watermark would pull the raw minimum backwards, and the monotonicity clamp is what prevents that from corrupting already-emitted results.

The standard lock-free pattern for a monotonically advancing counter is a compare-and-swap (CAS) loop on an `atomic.Int64`: load the current value, return early if the proposed value is not larger, otherwise attempt to swap; a failed swap means another goroutine advanced it concurrently, so retry. Only the goroutine that wins the swap advances the watermark; the others observe the already-advanced value.

## Event-Time Windows and Watermark-Triggered Firing

A watermark is only useful because it drives *firing*. A tumbling event-time window operator assigns each record to a fixed-size window aligned to the epoch — a 10-second tumbling window puts an event at 09:00:07 into `[09:00:00, 09:00:10)` — and accumulates an aggregate (a sum, a count) in per-window state. The operator does not fire on a clock or on a record count; it fires when the *watermark* passes a window's end. The rule is: a window `[start, end)` may fire once `watermark >= end`, because at that point the watermark is the engine's promise that no further in-time records for that window will arrive.

Firing is the moment the window's state can be emitted and, after the allowed-lateness grace period, purged. The order of firing is deterministic: when the watermark jumps forward, every window whose end is now at or below it fires, in ascending window-start order, so two runs over the same input produce byte-identical output. A record whose window has already fired is *late*, and the operator must decide what to do with it.

## Late Data, Allowed Lateness, and Three Routing Policies

A record is *late* if its window's end time is at or before the current watermark when the record arrives — the watermark already promised that nothing earlier would show up, and this record breaks that promise. Late records cannot simply be ignored by default because they affect aggregate correctness. *Allowed lateness* is a bounded grace period after a window's end during which late records are still accepted into the (already-fired) window and cause a re-fire. After `window.End + allowedLateness`, the window state is purged and any further late record is genuinely too late.

Three policies handle late records:

**Discard** (`LateDiscard`): drop the record and increment a counter. Used when the window's correctness is not critical or a small error is acceptable in exchange for simplicity.

**Accept with re-fire** (`LateAccept`): fold the record into the already-fired window's accumulated state and re-emit the result. Two sub-modes differ in how downstream consumers stay correct. *Accumulating* re-emits the new total; downstream sees the result grow monotonically, which is fine for an idempotent overwrite but double-counts if a downstream `SUM` naively adds every emission. *Accumulating and retracting* first emits a *retraction* (a negation of the previously emitted result) and then the new total; a downstream aggregation subtracts the retraction and adds the update, leaving the final answer correct even when it sums across many window emissions. Use retracting whenever a downstream operator aggregates the window outputs.

**Side output** (`LateSideOutput`): redirect records that arrive beyond the allowed-lateness deadline to a separate output channel, carrying the original record and its window metadata. Callers log, archive, or audit these records without disrupting the main result stream. A side output is the difference between "we silently lost data" and "we have an auditable record of every dropped event."

## Window State Lifetime and Memory

When a window fires, the engine must keep the window's accumulated state in memory until `window.End + allowedLateness` before it can safely purge it, because a late record within that grace period must be foldable into the existing state. This is the memory cost of allowed lateness: more lateness tolerance means more live window state held for longer. A cleanup pass (a timer or a periodic `Purge` call) removes window state once its deadline has passed. Until then, late records within the window's allowed lateness are accepted; after, they are dropped or side-output. Sizing allowed lateness is therefore a direct trade between completeness (catching stragglers) and memory.

## Watermark Lag as a Service-Level Indicator

The single most important health metric of an event-time pipeline is *watermark lag*: `processing_time - watermark`. In a healthy pipeline the lag is roughly the out-of-orderness bound plus a small processing delay, and it stays bounded. Two failure modes show up as distinct lag signatures. If the consumer is falling behind — the watermark keeps advancing but trails the wall clock by a growing margin — the pipeline is *lagging*; it is making progress but the backlog is increasing. If the watermark stops advancing entirely while wall-clock time keeps moving, the pipeline is *stalled*; a source has gone silent or an operator is wedged. Distinguishing "lagging" from "stalled" is what tells an on-call engineer whether to add capacity or to go find the stuck source. A lag monitor that classifies these states from an injected clock is deterministic and testable, which is exactly what you want from an alerting component.

## Common Mistakes

### Advancing the Global Watermark with a Plain Assignment Instead of a CAS Loop

Storing the new global value with a non-atomic `gt.globalMs = minMs` from a goroutine races every concurrent reader and writer: two callers can interleave so a higher value is overwritten by a lower one, breaking monotonicity. The `-race` flag catches it immediately. Use `atomic.Int64.CompareAndSwap` in a loop so only the winner advances and everyone else observes the advanced value.

### Letting a Source with No Events Drag the Global Watermark to Zero

Including a source whose `Watermark()` is the zero time in the minimum computation pulls the global watermark down to `time.Time{}`, so no window ever fires. Skip sources that have observed no events and skip idle sources; count how many sources actually contributed and return a sentinel error if none did.

### Forgetting Monotonicity After Adding a New Slow Source

Recomputing the raw minimum across all current sources on every call, without clamping to the previous global, makes the watermark jump backwards the moment a new slow source is registered on a running pipeline. Windows that already fired would have to be retracted unconditionally, which most downstreams cannot do. The CAS loop's "if the proposed value is not greater than the current, return the current unchanged" branch is the clamp that prevents this.

### Re-Emitting a Watermark That Did Not Advance

A periodic generator that emits its watermark on every timer tick, even when no new maximum arrived, floods downstream operators with redundant progress signals and can mask the difference between "advanced a little" and "did not advance at all." Suppress the emission unless the new watermark is strictly greater than the last emitted one.

### Firing a Window on a Record Count or a Processing-Time Timer

Firing when "enough" records have arrived, or on a wall-clock timer, reintroduces exactly the non-determinism that event-time windows exist to remove. Fire a window only when `watermark >= window.End`, and fire all eligible windows in ascending window-start order so the output is reproducible.

### Holding the Handler Lock While Sending on a Channel

Sending the re-fire result on the output channel while still holding the handler's mutex deadlocks if the channel is full and the consumer needs the same mutex to make progress. Compute the new aggregate and capture the values to emit under the lock, release the lock, then send.

### Treating Watermark Lag as a Single Threshold

Alerting only on "lag exceeds X" cannot tell a slow-but-advancing pipeline from a frozen one — both can show a large lag, but they need opposite responses. Track when the watermark last advanced as well as the current lag, so a frozen watermark is classified as stalled even if its absolute lag is the same as a merely lagging one.

Next: [01-watermark-tracking.md](01-watermark-tracking.md)
