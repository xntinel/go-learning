# 4. Worker Pool Pattern — Concepts

A worker pool runs a fixed number of goroutines that pull work from a shared
channel, process it, and report each outcome. It is the default shape for "do N
things concurrently" because it bounds the two resources that an unbounded
goroutine-per-job design blows up: the number of live goroutines and the memory
their stacks and in-flight state consume. The Go blog's bounded-parallelism
example is exactly this pattern — a fixed group of digester goroutines reading
from a `paths` channel — and every production job runner, email dispatcher, and
crawler frontier is a variation on it. This file is the conceptual foundation
for the three exercises that follow: a generic reusable pool, a job-processing
service with graceful shutdown and metrics, and a notification dispatcher with
retry and a dead-letter queue. Read it once and you will have the model you need
to reason through all three.

## Concepts

### Bounded Parallelism Is The Default, Not An Optimization

The naive way to process a queue is to spawn one goroutine per item: a loop that
does `go handle(item)` for every job. It works for ten items and detonates for
ten million. Each goroutine costs a few kilobytes of stack plus whatever the job
holds open — a database connection, an HTTP request, a buffer — and the runtime
scheduler degrades as the runnable set grows without bound. A pool of a fixed
size, say `runtime.GOMAXPROCS(0)` workers for CPU-bound work or a tuned constant
for I/O-bound work, serves the same throughput while keeping live goroutines and
memory flat regardless of queue depth. Bounding is not a tweak you add later; it
is the property that makes the pattern safe to point at an unbounded stream.

### The Pool Owns The Workers; The Caller Owns The Channels

A clean pool separates two responsibilities. The caller owns the input: it
produces jobs onto a channel and, crucially, closes that channel when there are
no more. The pool owns the workers: it fans out N goroutines that each loop with
`for j := range jobs`, and it owns a single closer goroutine that waits for all
workers to finish and then closes the output. The `for range` over a channel is
the mechanism that ties these together — every worker drains the same channel
until it is closed and empty, at which point each worker's loop ends naturally.
This is why the input channel *must* be closed by its producer: if it never
closes, every worker blocks forever in its range loop and the pool hangs. The
pool's closer is independent of the producer's close; conflating them is a
common source of deadlock.

### Merging Results Into One Channel With A Closer Goroutine

Each worker writes its results onto a shared output channel, so the results of N
workers merge into one stream the caller can range over. The hard part is
knowing when that stream is complete. The answer is the closer: a single
goroutine that calls `wg.Wait()` on a `sync.WaitGroup` the workers signal with
`defer wg.Done()`, and only then calls `close(out)`. The rule is absolute — only
that one goroutine ever closes the output. If a worker closes it with
`defer close(out)`, the first worker to finish closes a channel the others are
still sending on, and they panic with "send on closed channel". The
WaitGroup-plus-closer is the canonical fan-in, and it is the same machinery
whether the pool is generic, a job service, or a dispatcher.

### Generics Make One Pool Serve Every Job Shape

Since Go 1.18 the natural signature is `Pool[T, U any]` with a worker function
`func(T) U`. One implementation then squares integers, uppercases strings, or
parses URLs without change, because the dispatch loop, the WaitGroup, and the
closer are all independent of the element type. Generics here are not cleverness
for its own sake: they let the dangerous, easy-to-get-wrong concurrency core be
written, tested, and race-checked exactly once and reused everywhere, instead of
being re-implemented per job type where each copy is a fresh chance to leak a
goroutine or close a channel twice.

### Graceful Shutdown Drains In-Flight Work

A real service cannot just exit when asked to stop; it must finish the work it
already accepted. Graceful shutdown has a precise meaning: stop accepting new
jobs, let every job already queued or already running complete, then release
resources. The implementation is a three-step sequence — flip a flag so new
submissions are rejected, close the input channel so the workers' range loops end
once the queue is drained, then `wg.Wait()` for the workers and close the output.
The subtlety is the race between a `Submit` still in progress and the `close` of
the input channel: a `Submit` blocked on a full queue must not have its channel
closed underneath it, or it panics. The fix is a `sync.RWMutex` where `Submit`
holds a read lock across its check-and-send and shutdown takes the write lock
before closing. Because the write lock cannot be acquired while any read lock is
held, the close can never interleave with a send. This is the single most
important pattern in the two service exercises.

### Metrics Counters Need Atomics, Not Plain Increments

Workers run concurrently, so any counter they touch — jobs submitted, succeeded,
failed, retried, dead-lettered — is shared mutable state. A plain `n++` from two
goroutines is a data race: the race detector flags it and the count is wrong
under load. `sync/atomic` types (`atomic.Int64`) make each increment a single
indivisible operation, and a `Snapshot` method that `Load`s each field gives the
caller a consistent read without a lock. The discipline is to never read or write
a shared counter with a bare operator; always go through the atomic. Counters
also encode invariants worth asserting in tests: in a job service, submitted must
equal succeeded plus failed once the pool has drained; in a dispatcher, delivered
plus dead-lettered must equal enqueued.

### Retry With A Bounded Budget And A Dead-Letter Sink

Transient failures — a timed-out SMTP connection, a 503 from a downstream API —
should be retried, but not forever. The pattern is an attempt budget: try up to
`MaxAttempts` times, optionally sleeping a backoff between tries, and on success
return immediately. A message that exhausts its budget is not silently dropped;
it goes to a dead-letter sink, a separate channel and counter that captures
everything the pool could not deliver. The dead-letter count is the operational
signal that something downstream is broken, and the dead-letter messages are
what an operator replays after fixing it. Two failure modes bracket the correct
design: retrying forever (no cap) turns one bad message into a stuck worker, and
treating every error as permanent (no retry) turns a one-second blip into lost
mail. A bounded budget plus a dead-letter sink is the middle path.

### Backpressure And Joining: Why Bounded Queues And WaitGroups Together

A bounded input channel is also a backpressure mechanism: when the queue fills,
`Submit` blocks, which slows the producer to the rate the workers can sustain
instead of letting an unbounded backlog consume all memory. The matching
discipline on the output side is that the caller must drain the results (or
dead-letter) channel concurrently; if it stops reading, the workers block on
their sends and the shutdown's `wg.Wait()` hangs forever. Every goroutine the
pool starts must be joined — workers through the WaitGroup, any result-collector
goroutine through the channel close — or the program leaks goroutines, which the
race-enabled test run plus a leak check will expose.

## Common Mistakes

### One Goroutine Per Job

Wrong: `for _, job := range jobs { go handle(job) }` over an unbounded source.

What happens: a million jobs spawn a million goroutines; stacks and per-job
resources exhaust memory and the scheduler thrashes. The program OOMs or grinds
to a halt under load that a pool would absorb.

Fix: a fixed pool of workers reading from a shared channel. The worker count
bounds concurrency; the channel bounds the backlog.

### Closing The Output Channel From A Worker

Wrong: `defer close(out)` inside each worker goroutine.

What happens: the first worker to finish closes the channel while the others are
still sending, and they panic with "send on closed channel".

Fix: a single closer goroutine calls `close(out)` once, after `wg.Wait()`. Only
one goroutine ever closes a channel.

### Forgetting To Close The Input Channel

Wrong: the producer sends every job but never closes the input channel.

What happens: each worker's `for j := range jobs` blocks forever waiting for a
value that never comes; the pool never finishes and `wg.Wait()` hangs.

Fix: the producer closes the input channel after its last send. The pool's
closer is separate and must not be confused with it.

### Closing The Input Channel While A Submit Is Still Sending

Wrong: a shutdown path that calls `close(jobs)` without coordinating with a
`Submit` that may be blocked sending on a full queue.

What happens: a data race and a "send on closed channel" panic the moment the
close interleaves with the send.

Fix: guard `Submit` with a read lock and the shutdown's close with the write
lock, and reject new submissions once the closed flag is set. The write lock
cannot be taken while a send-in-progress holds the read lock, so the close is
always safe.

### Reading Metrics Counters Without Atomics

Wrong: `m.succeeded++` from multiple workers, or reading `m.succeeded` directly
while workers run.

What happens: a data race; the detector flags it and the totals are wrong under
contention.

Fix: `atomic.Int64` for every shared counter, incremented with `Add` and read
with `Load` through a `Snapshot` method.

### Not Draining The Result Or Dead-Letter Channel

Wrong: starting the pool, submitting work, and calling shutdown without a
goroutine consuming the output or dead-letter channel.

What happens: workers block on their sends once the channel buffer fills, so
`wg.Wait()` in shutdown never returns and the program deadlocks.

Fix: range over the output and dead-letter channels in a goroutine that runs
concurrently with submission and exits when those channels are closed.

### Retrying Forever Or Never

Wrong: a retry loop with no attempt cap, or a send path that gives up on the
first error.

What happens: an unbounded loop pins a worker on one permanently-failing message
and starves the rest; a no-retry path turns every transient blip into a lost
message.

Fix: a bounded attempt budget with optional backoff, and a dead-letter sink for
messages that exhaust the budget.

---

Next: [01-generic-worker-pool.md](01-generic-worker-pool.md)
