# 3. Windowing — Concepts

Unbounded streams have no natural boundaries, but every useful aggregation requires finite scope. Windowing imposes that structure: records are grouped into bounded buckets, accumulated, and emitted as a single result when the bucket closes. The complexity is not in counting records; it is in a design space with three orthogonal axes — window shape (tumbling, sliding, session, global), trigger policy (fire when?), and key isolation (aggregate per entity, not globally) — plus the optimizations and edge policies a production engine layers on top: pane sharing to make sliding windows cheap, eviction to bound an unbounded buffer, and processing-time timers to fire on the wall clock. This file is the conceptual foundation for every exercise in the lesson; read it once and you will have the vocabulary to reason through each module.

## Concepts

### The Window Shapes

A **tumbling window** partitions time into non-overlapping, fixed-size intervals. Every record belongs to exactly one window. A record at 10:03 with a 5-minute window lands in [10:00, 10:05). The boundary computation is a single truncation: `start = ts.Truncate(size)`. Because `time.Time.Truncate` rounds down to a multiple of the duration since the Go zero time (January 1, year 1, UTC), all correctly-formatted UTC times produce the expected floor.

A **sliding window** overlaps: each record belongs to at most `ceil(Size/Slide)` windows. A 5-minute window with a 2-minute slide emits a result every 2 minutes, each covering the prior 5 minutes. A record at 10:03 belongs to [10:00, 10:05) and [10:02, 10:07). The set of valid window starts is the set of multiples of `Slide` in the range `(ts-Size, ts]`. A naive assigner walks backward from `ts.Truncate(Slide)` in `Slide` steps, stopping when the distance from the start to `ts` would equal or exceed `Size`.

A **session window** is driven by user activity, not the clock. Records are grouped into sessions separated by an inactivity gap. If no record arrives for key K within `Gap` of the last record, the session closes. The tricky part is **merging**: when a new record bridges two previously separate sessions, they must collapse into one window with the combined boundaries. The merge algorithm sorts sessions by start time and sweeps left to right, extending the current window's end whenever the next window starts at or before the current end.

A **global window** assigns every record for a key to a single window that never closes on its own. By itself it would accumulate forever, so it is meaningless without a trigger that decides when to emit and, usually, an evictor that decides what to discard. The global window is how count-based windows are built: a trigger that fires every N records plus an evictor that keeps the most recent M elements produces a sliding count window with no reference to time at all.

### Event Time vs Processing Time

Every record has two timestamps: the **event time** (when the event happened, encoded in the record) and the **processing time** (when the engine receives it). These diverge when records arrive out of order or late.

A window based on event time groups records by when they happened. A window based on processing time groups them by when the engine saw them. The former is more correct for analytics; the latter is simpler and has lower latency. The tradeoff: event-time correctness requires the engine to wait for late data (handled by watermarks, the next lesson); processing time emits immediately at wall-clock boundaries and never waits, at the cost of putting the same logical event in different windows on every replay.

An event-time trigger fires based on the timestamps of records themselves: a record whose timestamp is at or past the window end signals that the window has closed. A processing-time trigger fires based on the wall clock — a `time.Ticker` or `time.AfterFunc` calls into the operator on each tick, and whatever has accumulated since the last tick becomes the window. Because the wall clock is involved, processing-time logic is the hardest to test deterministically; Go 1.25's `testing/synctest` package supplies a fake clock so a timer that nominally takes a minute fires instantly and exactly.

### Window Assigners and Triggers as Separate Axes

Assigners and triggers are independent: any assigner composes with any trigger. This separation is the key design insight from the Google Dataflow paper. The assigner answers "which windows does this record belong to?" The trigger answers "should this window emit now?" Combining them in a single operator gives tumbling + count-trigger, sliding + event-time-trigger, session + processing-time-trigger, or any other pairing, without re-implementing the accumulation machinery for each combination.

A trigger that decides on record count needs the current per-window count, so the operator passes it in rather than making the trigger keep its own counter. A trigger that decides on time receives a `time.Time` so that event-time and processing-time triggers implement the same method, one driven by record timestamps and the other by a flush call.

### Keyed Windows and State Isolation

In real pipelines, you aggregate per entity (per user, per sensor, per order). A key function extracts a string key from each record. The operator stores state in a `map[WindowKey]*state` where `WindowKey` is `{Key, Start}`. Two records with different keys never share window state, even when they land in the same time interval. This is what makes a windowed aggregation a `GROUP BY key, window` rather than a single global counter.

A stateless assigner (tumbling, sliding) holds no per-key data and is safe to share across operators and goroutines. A session assigner is stateful — it must remember each key's open sessions to merge new records into them — so it owns its own lock and must never be shared between two operators. The operator releases the assigner before acquiring its own lock so that the two locks are never held nested in opposite orders by different goroutines.

### Pane Sharing: Making Sliding Windows Cheap

The naive sliding assigner does redundant work. A 60-minute window sliding every 1 minute puts each record in 60 overlapping windows, and a straightforward operator re-aggregates all 60 from scratch. The standard optimization is **pane sharing** (also called slicing): divide the timeline into non-overlapping panes of width `gcd(Size, Slide)` — when `Slide` divides `Size`, the pane is exactly `Slide`. Each record updates exactly one pane's partial aggregate. A window's result is then the combination of the `Size/pane` consecutive panes it covers, so adjacent overlapping windows share all but one pane.

This turns O(Size/Slide) work per record into O(1), and it is the reason sliding-window aggregation is feasible at scale. The catch is that the aggregate must be a combinable partial state — a commutative monoid such as sum, count, min, max, or (sum, count) for an average. A non-combinable aggregate such as "median" or "exact distinct count" cannot be paned and forces the engine back to per-window buffers.

### Eviction: Bounding an Unbounded Buffer

A trigger says when to emit; an **evictor** says what to remove. Time windows purge their whole state when they fire and never need an evictor, but a global window driven by a count trigger would grow without bound, so an evictor trims it. A count evictor keeps the most recent M elements and drops the rest; a time evictor drops elements older than the newest element minus a threshold. Eviction runs around the window function — typically before aggregation, so the function only sees the retained elements — and it is what lets a single never-closing global window stand in for an unbounded family of sliding count windows while using O(M) memory.

## Common Mistakes

### Computing Window Boundaries with Round Instead of Truncate

Wrong: computing `start = ts.Round(size)` to find a record's tumbling window.

What happens: `Round` snaps to the *nearest* multiple, so a record at 10:03 with a 5-minute size rounds up to 10:05 and lands in [10:05, 10:10) — the wrong window — while a record at 10:01 rounds down to 10:00. Two records three minutes apart can end up in different buckets, and a record can be assigned to a window whose start is after its own timestamp.

Fix: always `ts.Truncate(size)`. Truncate floors to a multiple of `size` since the Go zero time, which is exactly the window-start definition.

### Sharing One Stateful Session Assigner Across Operators

Wrong: constructing a single `*SessionWindowAssigner` and passing the same pointer to two operators, or to two goroutines that mutate it without synchronization.

What happens: the session assigner mutates its per-key session map on every assignment. Two operators sharing the pointer interleave each other's sessions; two unsynchronized goroutines race on the map and the race detector aborts the test.

Fix: a session assigner owns its own mutex for concurrent calls within one operator, but each operator must construct its own assigner instance. Stateless tumbling and sliding assigners may be shared; stateful ones may not.

### Expecting a Count Trigger to Fire on a Timer

Wrong: pairing a count-based trigger with a time-driven flush and waiting for output.

What happens: a count trigger only fires from record arrivals, so a flush call advances the clock but emits nothing. Records accumulate until the count threshold is reached, and a window that never reaches the threshold never emits at all.

Fix: match the trigger to the emission mechanism. Use an event-time or processing-time trigger when emission must be time-driven; use a count trigger (and pair it with an evictor on a global window) when emission must be count-driven. A global window with a count trigger and no evictor is the same mistake in slow motion: it emits but never bounds its memory.

### Re-Aggregating Every Sliding Window from Scratch

Wrong: implementing a sliding aggregation by, on each record, looping over all `ceil(Size/Slide)` windows the record belongs to and folding it into each one independently.

What happens: throughput collapses as the overlap ratio grows. A 24-hour window sliding every minute does 1440 folds per record and stores 1440 redundant accumulators, most of which hold nearly identical partial sums.

Fix: pane the timeline. Fold each record into one pane of width `gcd(Size, Slide)`, and assemble each window by combining its constituent panes. The work per record drops to O(1) and the partial aggregates are shared across every window that overlaps a pane.

### Testing Processing-Time Windows with Real Sleeps

Wrong: testing a wall-clock window operator by calling `time.Sleep(windowSize)` and asserting on what was emitted.

What happens: the test is slow (it really waits) and flaky (a scheduling hiccup near the boundary moves a record into the adjacent window). CI failures appear at random and resist reproduction.

Fix: run the operator inside `testing/synctest`. Inside the bubble the clock is fake: a goroutine that sleeps for the window size advances instantly, timers fire at exact virtual instants, and `synctest.Wait` blocks until every goroutine in the bubble is idle, so the assertion sees a fully-settled state with no real time elapsed.

Next: [01-window-operator.md](01-window-operator.md)
