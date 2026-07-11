# 18. Bounded Worker Pool With Adaptive Sizing — Concepts

A fixed-size worker pool is a compromise frozen at compile time. Size it for the
peak and it burns goroutines and cache lines idling through the quiet hours; size
it for the average and it collapses into a backlog the moment traffic spikes. An
adaptive pool moves that decision to runtime: it watches a signal that reflects
load, and it grows the worker count when the signal says "behind" and shrinks it
when the signal says "idle", always within a hard `[Min, Max]` band. This chapter
builds three production-shaped versions of that idea — a queue-depth pool, an
autoscaler that also reacts to latency, and a backpressure-aware ingest that holds
a target number of in-flight operations — and it builds each one so that its
scaling decisions are deterministically testable under the race detector. Read
this file once and you will have the full model: the feedback loop, why the clamp
is mandatory, why shrinking is the part everyone gets wrong, and why a controller
that cannot be tested without `time.Sleep` is a controller you cannot trust.

## Concepts

### The Feedback Loop: Observe, Decide, Act

Every adaptive pool is the same control loop wearing different clothes. On a fixed
cadence it observes a signal (queue depth, latency, in-flight count), feeds that
observation to a decision rule that returns a new target worker count, and then
acts to make the live worker count match the target. The three jobs are worth
keeping mentally separate, because the decision rule is a pure function — given
the current target and a sample, it returns the next target with no goroutines,
no channels, and no clock — and a pure function is the thing you can test
exhaustively in a table. The mechanism around it (the goroutines, the ticker, the
spawn-and-retire plumbing) is where the concurrency bugs live, and isolating the
arithmetic from the plumbing is what lets you prove the arithmetic correct without
fighting a scheduler.

### The Clamp Is Not Optional

A bare feedback loop has two failure modes and both are fatal. If it can shrink
without a floor, a quiet period drives the worker count to zero, and a pool with
zero workers is a deadlock: the next job is enqueued and no one will ever pick it
up. If it can grow without a ceiling, a sustained spike drives the worker count
toward infinity, and a process with ten thousand goroutines all allocating per-job
buffers is an out-of-memory crash. `Min` and `Max` are therefore not tuning knobs
you may omit; they are the safety rails that make the loop legal. Every decision
rule in this chapter ends by clamping its result into `[Min, Max]`, and `Min` is a
strict floor of at least one.

### Shrinking Means Retiring a Goroutine, Not Decrementing a Counter

This is the single most common bug in a hand-rolled adaptive pool, and it is worth
stating bluntly because it is easy to write code that looks correct and is not. A
worker is a goroutine blocked in a `for range jobs` loop. "Shrinking the pool" is
frequently coded as `workers--` — a decrement of a bookkeeping integer. That does
nothing. The goroutine is still alive, still ranging over the job channel, still
consuming a stack and scheduler slot. The reported worker count drops, the actual
goroutine count does not, and the two silently diverge. Worse, the next time the
pool grows it spawns *new* goroutines on top of the ones it never retired, so the
real concurrency climbs past `Max` and never comes back down. The counter lies and
the ceiling leaks.

Real shrinking requires a retirement protocol: a way to tell one specific worker
to finish what it is doing and exit, so that its goroutine actually returns, its
`defer wg.Done()` fires, and its slot is reclaimed. The standard implementation is
a `retire` channel that workers select on alongside the job channel; sending one
token retires exactly one worker. Because Go's `select` picks a ready case at
random, a worker only takes the retirement token when it has no job to take —
which is exactly when you want to shrink — so a busy worker keeps working and an
idle one exits. The chapter's pools track the live goroutine count with an atomic
counter precisely so a test can assert that the count returns to `Min`, which is
the assertion that distinguishes real retirement from the counter lie.

### Choosing the Signal: Depth, Latency, and In-Flight

The signal is what the loop reacts to, and the right choice depends on what you
are protecting. Queue depth — how many jobs are waiting — is the simplest: a long
queue means workers cannot keep up, grow; an empty queue means they are idle,
shrink. It is cheap to read (`len(jobs)`) and directly proportional to backlog,
but it is a lagging indicator and it says nothing about how long each job takes.

Latency — how long a job spends being processed — catches a class of overload that
depth misses entirely. A pool can have a near-empty queue and still be in trouble
if each job has started taking ten times longer because a downstream dependency is
struggling; reacting to latency lets the pool add workers to ride out a slow
dependency before the queue ever builds. A robust autoscaler watches both: it
grows if the queue is deep *or* latency is above target, and it only shrinks when
the queue is empty *and* latency is healthy, so a momentary lull does not retire
workers the system is about to need again.

In-flight count — how many operations are executing concurrently right now — is the
signal of choice when the thing you are protecting is a downstream service rather
than your own CPU. By Little's law the average number of in-flight requests equals
the arrival rate times the average latency, so holding in-flight at a target is
equivalent to holding the offered concurrency at a level the downstream can absorb.
That is the backpressure problem, below.

### Hysteresis: Why the Loop Must React Slowly

A loop that adjusts on every job, or that grows the instant a single item appears
in the queue, thrashes: it adds a worker, the queue empties, it removes the
worker, the next item arrives, it adds it back, and the goroutine count oscillates
violently while doing no useful work. Two mechanisms damp this. The first is
cadence: the loop runs on a timer (every few hundred milliseconds, or every few
seconds in a real system), not on every event, so a transient blip is averaged
away before the next decision. The second is a watermark: the pool grows only when
the signal crosses a threshold that requires *sustained* load, not a single queued
item, so momentary jitter never trips it. Reacting slowly on purpose is what turns
a feedback loop into a stable controller instead of a relaxation oscillator.

### Backpressure: Holding a Target In-Flight

Backpressure is the discipline of letting a slow consumer slow down its producers
instead of drowning. A bounded queue is the crudest form: when it fills, `Submit`
blocks, and the producer is forced to wait. A more precise form is a dynamic
concurrency limit. The ingest in this chapter admits work through a resizable
semaphore: at most `limit` operations run at once, and a producer that arrives when
the limit is reached blocks until a permit frees. A controller then adjusts `limit`
every tick to hold the number of in-flight operations near a target — it shrinks
the limit when in-flight exceeds the target (tightening backpressure, slowing
producers) and grows it when in-flight is below the target and producers are
waiting (loosening backpressure, admitting more). The result is a system that
self-tunes its own concurrency to a setpoint, the same shape as TCP's congestion
window or a Netflix-style adaptive concurrency limiter, and the same shape the
Go standard library uses when it bounds parallelism with a buffered channel of
tokens.

### Why the Clock Must Be Injectable

A controller's whole job is to make decisions over time, which makes time the
hardest thing about testing it. A test written against the wall clock asserts "after
sleeping 300ms the pool should have grown", and that test is simultaneously slow,
flaky, and a liar: slow because it waits real milliseconds, flaky because a loaded
CI machine misses the timing, and a liar because when it fails you cannot tell
whether the logic is wrong or the scheduler was busy. The fix is to make the clock
a dependency. The pool takes a `Clock` interface — a `Now()` and a way to receive
ticks — and in production it is backed by `time`, while a test injects a fake clock
whose tick it fires by hand and whose `Now()` it advances by hand. Firing one tick
runs exactly one control cycle; advancing `Now()` by a fixed amount manufactures a
precise latency. The scaling decision becomes a deterministic function of inputs
the test controls completely, the test runs in microseconds, and it passes or fails
on the logic alone. This is non-negotiable for code that ships: a timing-dependent
test that "usually passes" is technical debt that fails at 3 a.m.

### macOS Notes

On a Mac the worker count you can usefully run is bounded by `runtime.GOMAXPROCS`,
which defaults to the number of logical CPUs (`sysctl -n hw.logicalcpu`); growing a
CPU-bound pool past that buys contention, not throughput, which is a good reason a
`Max` exists. The Go runtime's monotonic clock on darwin is backed by
`mach_absolute_time`, so `time.Now()` deltas used for latency are immune to wall-
clock adjustments — but the lesson never depends on that, because the clock is
injected and the tests measure latency against a fake clock they advance
themselves. Run every exercise with `go test -race`: the race detector is the only
tool that reliably catches the unsynchronized read of a shared worker count that an
adaptive pool makes so easy to introduce.

## Common Mistakes

### Decrementing a Counter Instead of Retiring a Worker

Wrong: implementing "shrink" as `p.workers--` while the worker goroutines keep
ranging over the job channel.

What happens: the reported count drops but no goroutine exits. The live goroutine
count stays high, and the next growth phase spawns more on top, so real
concurrency leaks past `Max` and never returns to `Min`. The pool's own
`WorkerCount()` becomes a fiction.

Fix: retire workers through a `retire` channel they select on; one token exits one
goroutine. Track the live count with an atomic and assert in a test that it
returns to `Min` after load subsides.

### Omitting the Clamp

Wrong: growing or shrinking by a step without bounding the result.

What happens: a quiet period shrinks the pool to zero workers and the next submit
deadlocks; a sustained spike grows it without limit and the process runs out of
memory.

Fix: clamp every decision into `[Min, Max]` with `Min >= 1`. The clamp is part of
the decision rule, not an afterthought.

### Adjusting on Every Event

Wrong: recomputing the worker count from inside `Submit`, once per job.

What happens: the goroutine count oscillates with every burst and lull; the pool
spends its time spawning and retiring instead of working.

Fix: drive adjustment from a timer at a fixed cadence, and grow only past a
watermark that requires sustained load, so transient jitter is averaged out.

### Testing Timing With time.Sleep

Wrong: asserting a scaling outcome after `time.Sleep(someInterval)`.

What happens: the test is slow, flaky under load, and uninformative on failure —
you cannot distinguish a logic bug from a missed deadline.

Fix: inject a `Clock`. Fire ticks by hand to run exactly one control cycle and
advance `Now()` by hand to manufacture latency, so the decision is a deterministic
function of controlled inputs.

### Forgetting to Drain Results

Wrong: a worker that sends every result on an unbuffered channel while the caller
never reads it.

What happens: the worker blocks on the send, stops taking jobs, and the pool
stalls; on shutdown the `wg.Wait()` that joins the workers deadlocks.

Fix: make draining the result channel part of the contract — every test and the
demo start a goroutine that ranges over results before submitting work.

---

Next: [01-adaptive-worker-pool.md](01-adaptive-worker-pool.md)
