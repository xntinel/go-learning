# Production Tracing with runtime/trace FlightRecorder — Concepts

The execution trace is Go's richest diagnostic: it shows every goroutine
transition, GC pause, syscall, and scheduler decision, and with user annotations
it shows your application's own structure. The senior-backend problem is never
"how do I read a trace" — it is that the pathological event happens once a day at
3am, and by the time you attach `go tool trace` it is gone. You cannot leave
`trace.Start` running in production either: a continuous trace grows to gigabytes
within minutes and adds nontrivial runtime overhead. The interesting event is
rare, unpredictable, and expensive to catch with the obvious tool.

`runtime/trace.FlightRecorder` (Go 1.25, graduated from `golang.org/x/exp/trace`)
is the answer to exactly that problem. It keeps only a moving window of the most
recent trace data in an in-memory ring buffer that costs a bounded amount of RAM,
and it flushes that window to a writer only on demand — the moment a rare
condition fires. This is the aircraft-black-box model: always armed, bounded,
dumped on trigger. This file is the conceptual foundation for the three
independent exercises that follow: arming a recorder and snapshotting on demand,
wiring a real trigger with cooldown and dedup, and annotating the workload so the
captured window is actually diagnosable.

## Concepts

### What flight recording is, and why it exists

A regular trace consumer started with `trace.Start(w)` streams the *entire* trace
to `w` from the instant you start until you stop. That is the right tool for a
reproducible workload you can run under a trace. It is the wrong tool for a rare
production anomaly: you would have to leave it running continuously, paying the
size and overhead cost the whole time, just to have the buffer populated when the
event finally happens.

A flight recorder inverts this. It records continuously, but retains only the
tail — the last few seconds of trace data — in a fixed-size ring buffer. The cost
is bounded and constant. When your trigger fires (an SLO breach, a 5xx, a panic
being recovered, a watchdog), you snapshot that tail to disk. You get the seconds
*leading up to* the rare event, which is exactly the window you could never
capture by attaching a tool after the fact.

### The two config knobs and their precedence

`FlightRecorderConfig` has two fields, both hints rather than guarantees:

- `MinAge time.Duration` — a lower bound on how far back the window reaches. The
  recorder strives to keep at least this much history and to promptly discard
  events older than it. Zero means an implementation-defined default, on the
  order of seconds.
- `MaxBytes uint64` — an upper bound on the window size in bytes. Zero means an
  implementation-defined default.

The critical rule is precedence: **`MaxBytes` overrides `MinAge`**. Under a high
volume of trace events the window shrinks in wall-clock terms to stay under the
byte cap, so you may silently get *less* history than `MinAge` suggests. And
`MaxBytes` is explicitly documented as a hint: it does not bound the size of what
`WriteTo` writes, nor does it strictly bound memory overhead. Treat both as
directional guidance, and never build a system that assumes `MaxBytes` caps disk
or memory precisely.

### The lifecycle contract

The type has a small, strict lifecycle:

- `NewFlightRecorder(cfg)` builds an inactive recorder.
- `Start() error` activates it and begins recording. It returns an error if it
  cannot start, if it is already started, or if another recorder (or, in the
  single-recorder era, a conflicting consumer) is already active.
- `Enabled() bool` reports whether it is active — `Start` succeeded and `Stop`
  has not yet been called. It is safe to call concurrently and is the natural
  "is it armed?" check.
- `WriteTo(w io.Writer) (int64, error)` snapshots the current window into `w`.
  It errors if the write fails, if the recorder is inactive, or if another
  `WriteTo` is already in progress.
- `Stop()` ends recording and blocks until any in-flight `WriteTo` completes.

Two subtleties matter operationally. First, `WriteTo` permits only one snapshot
at a time; a concurrent second call returns an error rather than queueing.
Second, `Stop` is not instant — it waits for a concurrent snapshot to finish, so
it must be wired into graceful shutdown rather than assumed to return
immediately.

### Single-instance and coexistence rules

At most one `FlightRecorder` may be active process-wide at any instant. Design
your service to own exactly one — a singleton armed at boot — rather than
constructing one per component or per request; a second `Start` on a second
recorder fails. (This single-recorder restriction may be relaxed in a future
release, but write code that respects it today.)

Flight recording *is*, however, allowed to run concurrently with a regular trace
consumer started via `trace.Start`. You can be armed as a black box and also be
running an on-purpose full trace at the same time — the two do not conflict.

### Annotation is the other half of the feature

A raw trace window shows scheduler, GC, and syscall events, but not what your
application was *doing*. To make the 3am snapshot interpretable you annotate the
workload:

- `trace.NewTask(ctx, taskType)` groups all the work of one logical operation (a
  request) across the goroutines it spawns, and returns a derived context that
  carries the task. Its `Task.End()` marks completion.
- `trace.StartRegion(ctx, regionType).End()` and `trace.WithRegion(ctx, regionType, fn)`
  mark sub-stages with durations. Regions must nest within a goroutine, and
  `Region.End` must be called on the same goroutine that started it.
- `trace.Log(ctx, category, message)` and `trace.Logf(ctx, category, format, args...)`
  attach structured key-value breadcrumbs at decision points (cache hit/miss,
  retry counts).

Without these, the captured window is far harder to read; with them, the snapshot
is actionable rather than merely present. This is the difference between "we have
tracing" and "we can explain the one request that mattered."

### The IsEnabled gate and the cost model

Annotation calls are cheap but not free. `trace.IsEnabled()` reports whether
tracing is currently being collected; guarding an annotation with it makes the
annotation a true no-op when nothing is recording. The guard matters most for
`Logf`, whose variadic `args ...any` are boxed into an interface slice *at the
call site* before the function is even entered — that boxing allocates whether or
not the log is ultimately emitted. Writing `if trace.IsEnabled() { trace.Logf(...) }`
skips the boxing entirely when tracing is off, giving zero allocations on the hot
path. A constant-string `trace.Log(ctx, "cache", "hit")` has no arguments to box,
so guarding it adds little; guard the formatting and expensive-argument cases.

One thing to internalize: an armed FlightRecorder *counts as tracing being
enabled*. `IsEnabled()` is true whenever the recorder is running. So in a service
with the black box always armed, the guard's payoff is mostly when no trace at
all is active (tests, tooling-off builds); it is still correct and idiomatic to
keep, because it documents intent and protects the truly-off case.

### Trigger design is a production concern

The recorder is passive. It does nothing until you call `WriteTo`; deciding
*when* to snapshot is your job, and it is where the engineering lives. Real
triggers are SLO latency breaches, 5xx spikes, a panic/recover path, or a
watchdog goroutine. The dominant failure mode is the *trigger storm*: a bad
minute fires the condition thousands of times, and a naive trigger writes
thousands of snapshots. That (a) serializes on the single-in-flight-`WriteTo`
constraint, (b) fills the disk, and (c) turns a diagnostic into an outage
amplifier. The production-critical guard is a cooldown/dedup gate — an atomic
compare-and-swap on a "next allowed" timestamp so at most one snapshot is written
per cooldown window — plus unique dump filenames so snapshots never clobber one
another.

### Reading the output

`WriteTo` produces a standard execution-trace byte stream. You can view it
interactively with `go tool trace file`, or parse it programmatically with
`golang.org/x/exp/trace`'s `NewReader`/`ReadEvent` for automated
analysis and assertions. That reader package is still experimental and external
— fine for offline and test analysis, but do not depend on it in a production
code path. A non-empty trace always begins and ends with a `Sync` event; iterate
`ReadEvent` until `io.EOF`.

### Operational placement

Arm the recorder once at process boot, keep it running for the whole lifetime,
and wire `Stop()` into graceful shutdown. Expose two ways to snapshot: an
automatic trigger (request middleware) and a manual escape hatch (an
authenticated `/debug/flightrecorder/snapshot` endpoint) so an operator can grab
the window during a live incident. Treat the dump directory like any other
unbounded output — rotate or cap it so successful diagnostics do not themselves
fill the disk over weeks.

## Common Mistakes

### Leaving trace.Start running continuously in production

Wrong: starting a full trace at boot "so we never miss the event". It produces
gigabytes and adds real overhead. Fix: arm a bounded `FlightRecorder` and
snapshot on trigger — that is the exact problem the recorder solves.

### Creating more than one active recorder

Wrong: constructing a `FlightRecorder` per component or per request. At most one
may be active process-wide; the second `Start` fails. Fix: own a single recorder
in a service singleton.

### Assuming MaxBytes bounds the file or the memory

Wrong: sizing disk or RAM on the assumption that `MaxBytes` is a hard cap. The
docs call it a hint; `WriteTo` output and memory overhead can exceed it, and it
overrides `MinAge` so you can silently get less history than the age you
configured under load. Fix: treat both knobs as directional and measure real
snapshot sizes.

### Firing WriteTo on every triggering request

Wrong: snapshotting unconditionally whenever latency spikes, so a bad minute
writes thousands of files, fills the disk, and serializes on the single-in-flight
`WriteTo` constraint. Fix: gate the trigger with an atomic cooldown and write
unique filenames.

### Calling WriteTo concurrently or after Stop

Wrong: two goroutines snapshotting at once (the second errors), or calling
`WriteTo` after `Stop`/before `Start` and ignoring the returned error. Fix:
serialize snapshots or handle the in-progress error, and check `Enabled()` and
the error rather than assuming success.

### Logf on the hot path without a guard

Wrong: `trace.Logf(ctx, "cache", "hit=%v", hit)` unguarded, paying argument
boxing and formatting even when nothing is recording; or, conversely,
over-guarding a constant-string `trace.Log`. Fix: guard the variadic/formatting
and expensive-argument cases with `trace.IsEnabled()`; leave cheap constant logs
unguarded.

### Unbalanced tasks and regions

Wrong: forgetting to `End()` a region or task (or starting a task but passing the
wrong context down, so child regions are not associated with it). The window
becomes unbalanced and misleading. Fix: `defer task.End()` and
`defer trace.StartRegion(ctx, name).End()`, and always propagate the context
`NewTask` returns.

### Never wiring Stop into shutdown

Wrong: relying on process exit to stop the recorder, or expecting `Stop` to be
instant. It blocks until any concurrent `WriteTo` finishes. Fix: call `Stop` in
graceful shutdown so the last window is not lost and shutdown does not race a
snapshot.

### Depending on golang.org/x/exp/trace as if it were stable

Wrong: importing the experimental external reader in a production code path. Fix:
use it only for offline/test analysis, and in a curriculum keep it behind a build
tag so the default build stays stdlib-only.

Next: [01-armed-recorder-snapshot.md](01-armed-recorder-snapshot.md)
