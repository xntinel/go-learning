# WaitGroup: Coordinating Fan-Out Work in Production Backends

A backend request rarely does one thing. It scatters: probe three dependencies for
a readiness check, enrich a batch of IDs from an upstream service, read from every
shard, prefetch several caches. The hard part is never launching the goroutines —
`go f()` is one keyword. The hard part is the *join*: knowing, precisely, that all
the work you started has actually finished, so you can read its results, close a
channel, or return a response. `sync.WaitGroup` is the primitive that answers that
one operational question — "is all the work I started done?" — and it does so with
a memory-model guarantee that makes the finished work safe to read.

This chapter treats WaitGroup not as a syntax feature but as a *join counter*, and
teaches you to pick the right join primitive under real constraints: ordered
results, bounded concurrency, first-error cancellation, panic isolation, and
shutdown deadlines. Along the way it contrasts raw `Add`/`Done`/`Wait` with the
Go 1.25 `wg.Go` method and with `golang.org/x/sync/errgroup`, so you know when each
is the correct tool on a real production path.

## Concepts

### WaitGroup is a join counter, not a mutex

A `WaitGroup` holds a single integer counter. `Add(n)` increments it by `n` (the
count of outstanding tasks), `Done` decrements it by one, and `Wait` blocks the
calling goroutine until the counter reaches zero. That is the entire model. It does
not protect shared memory — that is a mutex's job — and it does not carry values or
errors. It answers exactly one question: *have all the started tasks finished?*

What makes it more than a busy-wait is the memory guarantee. The Go memory model
specifies that everything a goroutine did *before* its `Done` call is
happens-before-ordered ahead of the return of a `Wait` that the `Done` unblocks. In
plain terms: once `Wait` returns, every write those goroutines performed is visible
to you. That is why you can fan out into a pre-sized results slice, `Wait`, and then
read the slice without any additional synchronization — the `Wait` itself
establishes the visibility.

### The Add-before-goroutine rule is about a race on the counter

The single most important rule: call `Add` in the launching goroutine, *before* the
`go` statement, not inside the new goroutine. If `Add(1)` runs inside the goroutine,
the scheduler may run `Wait` first, observe a counter that has not yet been
incremented, see zero, and return — before the goroutine is even counted. Your join
silently completes early and drops work. `Add` before `go` closes that window: the
counter reflects the task the instant you decide to launch it.

### defer Done on the first line covers every exit path

`defer wg.Done()` as the goroutine's first statement guarantees the counter is
decremented on *every* exit: normal return, an early `return` in an error branch,
and a panic unwinding the stack. If any path skips `Done`, the counter never reaches
zero and `Wait` blocks forever. This failure mode is a hang, not a crash — the
process sits there consuming a goroutine, which under load looks like a leak and a
stuck request. A missing `Done` is far more insidious than most bugs because nothing
errors; it just never finishes.

### A negative counter panics

More `Done` calls than `Add` drives the counter below zero and panics with
`sync: negative WaitGroup counter`. This is a real bug class, not a theoretical one:
it shows up when `Add` and `Done` are split across functions, when a `Done` is
written both in a `defer` and again explicitly, or when a computed `Add(delta)` uses
the wrong delta. Keep `Add` and `Done` symmetric and local, and the counter stays
honest.

### Go 1.25's wg.Go removes the boilerplate and the footgun

Go 1.25 added `wg.Go(f func())`. It does `Add(1)`, runs `f` in a new goroutine, and
calls `Done` when `f` returns — atomically, in the right order. That single method
eliminates the classic three-line dance and, more importantly, the Add-placement
footgun: there is no longer an `Add` that can drift into the wrong goroutine. For
fire-and-join goroutines in modern code, `wg.Go(func(){ ... })` is the preferred
spelling. One caveat carries over: `wg.Go` does *not* recover panics. An uncaught
panic inside `f` still crashes the process, exactly as it would with a hand-rolled
goroutine, so risky handler code still needs its own `recover`.

### Disjoint-index writes are race-free; shared append is not

When you fan out over a slice of inputs and each goroutine writes `results[i]` for
its own `i`, there is no data race: every goroutine touches a distinct memory
location, and the `Wait` publishes all of them. This is the idiom to reach for when
you also need to *preserve input order* — result `i` lands in slot `i`. By contrast,
`append`-ing to one shared slice from many goroutines is a data race: `append` reads
and mutates the slice header (length, capacity, backing array), and concurrent
appends corrupt it. The race detector catches this immediately. If you need appends,
guard them with a `sync.Mutex`; if you need order, prefer indexed writes.

### WaitGroup gives you the join but not errors or cancellation

WaitGroup tells you *when* work finished, not *whether it succeeded* and not *how to
stop it early*. Two very common needs — "return the first error and cancel the rest"
and "bound concurrency" — are not in its vocabulary. `errgroup.Group` layers them
on. `errgroup.WithContext` hands you a group and a derived context; the first `Go`
function to return a non-nil error cancels that context, so peers watching
`ctx.Done()` abort early, and `Wait` returns that first error. `SetLimit(n)` caps
how many goroutines run at once, applying backpressure at `Go`. The decision rule is
clean: use a plain WaitGroup when you want *collect-all* semantics (run everything,
gather every result and error); use errgroup when you want *fail-fast* (first error
wins and cancels the rest). When you want collect-all *and* bounded concurrency, you
can combine `SetLimit` with per-item error collection via `errors.Join`.

### Wait is not cancelable — race it against a deadline

`wg.Wait()` blocks until the counter hits zero and offers no timeout or context
parameter. During graceful shutdown you must bound how long you wait for in-flight
work to drain. The idiom: run `wg.Wait()` in a helper goroutine that closes a `done`
channel when it returns, then `select` between `done` and `ctx.Done()` (or a timer).
If `done` fires first, the drain completed cleanly; if the context fires first, you
return `context.DeadlineExceeded` and move on. Note this does not *stop* the
in-flight goroutines — they keep running — it stops *you* from waiting past the
deadline. Stopping the work itself requires a cancelable context threaded into it.

### Reuse is legal only after Wait has fully returned

A WaitGroup may be reused for a new round of work, but only once its counter is back
to zero *and* the previous `Wait` has returned. Starting a fresh `Add` cycle while a
`Wait` is still in progress is a race on the counter's reuse and is undefined. This
matters for batched or paged processing: you must fully drain page *k* (its `Wait`
returns) before you `Add` for page *k+1*. As long as each round completes before the
next begins, one WaitGroup serves an entire paged backfill.

### Bounding concurrency: semaphore channel vs SetLimit

Unbounded fan-out — one goroutine per item over a large input with no cap — is a
production risk: goroutine count and memory grow with the input, and a big batch can
exhaust the process. Two idioms bound it. A buffered `chan struct{}` used as a
semaphore, acquired *before* the `go` statement and released via `defer` inside the
goroutine, caps in-flight goroutines and applies backpressure on the producer loop
(the acquire blocks when the channel is full). `errgroup.SetLimit(n)` gives the same
bound with error handling built in — `Go` blocks when the limit is reached, and
`TryGo` returns false instead of blocking. Both cap goroutines and memory; the
choice is whether you also want errgroup's error propagation.

### Closing a shared results channel exactly once

In fan-in, several producer goroutines send into one shared output channel and a
consumer ranges over it. The consumer's `range` loop ends only when the channel is
closed — but closing it from a producer risks a send-on-closed-channel panic (a peer
may still be mid-send), and never closing it leaves the consumer ranging forever. The
canonical solution is a single dedicated waiter goroutine that does `wg.Wait();
close(out)`. It closes exactly once, only after every producer's `Done`, so no send
races the close and the consumer's loop terminates cleanly.

### Panic isolation with balanced counters relies on defer LIFO

If an individual handler can panic (bad input, nil dereference), that panic must not
crash the process *and* must not leak the WaitGroup counter. The pattern uses two
deferred functions, and their order matters because `defer` runs LIFO. Defer
`wg.Done()` *first* so it runs *last*; then defer a `recover()` that converts the
panic into a result error. Because the recover defer is registered second, it runs
first during unwinding — populating the result — and `Done` runs after it, keeping
the counter balanced. The goroutine survives, the panic becomes a `Result.Err`, and
`Wait` returns instead of hanging.

## Common Mistakes

### Calling Add inside the goroutine

Wrong: `go func() { wg.Add(1); defer wg.Done(); ... }()`. `Wait` can observe a
still-zero counter and return before this goroutine is counted, silently dropping
work.

Fix: call `wg.Add(1)` in the launching goroutine, immediately before `go`. Or use
`wg.Go`, which places the `Add` correctly for you.

### Forgetting Done, or putting it only on the happy path

Wrong: a `Done` written after the work, so an early `return` in an error branch or a
panic skips it. The counter never reaches zero and `Wait` hangs forever.

Fix: `defer wg.Done()` as the goroutine's first statement, so every exit path
decrements exactly once.

### Copying a WaitGroup by value

Wrong: passing `sync.WaitGroup` by value into a helper, or embedding it in a struct
that gets copied. `Add`/`Done`/`Wait` then operate on different copies with
independent counters, and the join is meaningless. `go vet` flags this.

Fix: pass `*sync.WaitGroup`, and never copy a struct that embeds one.

### Appending to a shared slice without a mutex

Wrong: many goroutines doing `results = append(results, r)` on one slice. `append`
mutates the shared header; this is a data race the `-race` detector catches.

Fix: write disjoint indices (`results[i] = r`), guard the append with a
`sync.Mutex`, or send results over a channel.

### Calling Done more times than Add

Wrong: a double `Done` (both a `defer` and an explicit call), or a `Done` with no
matching `Add`. The counter goes negative and panics with
`sync: negative WaitGroup counter`.

Fix: keep `Add` and `Done` symmetric — one `Done` per `Add(1)`, defer it exactly
once.

### Treating Wait as cancelable

Wrong: `wg.Wait()` on a shutdown path with no deadline, so a single stuck request
blocks shutdown forever.

Fix: run `wg.Wait()` in a goroutine that closes a `done` channel, then `select`
between `done` and `ctx.Done()`.

### Reusing a WaitGroup before Wait returns

Wrong: beginning a new `Add` round while a previous `Wait` is still blocked — a race
on the counter's reuse.

Fix: fully drain each round (let `Wait` return) before `Add`-ing the next.

### Closing the results channel from a producer

Wrong: a producer goroutine calling `close(out)` when it finishes — a peer still
sending panics with "send on closed channel".

Fix: close from a single waiter goroutine that runs `wg.Wait(); close(out)`.

### Assuming wg.Go recovers panics

Wrong: `wg.Go(func(){ riskyHandler() })` and assuming a panic is contained. An
uncaught panic still crashes the process.

Fix: wrap risky work in a `recover` inside the function you pass to `wg.Go`.

### Unbounded fan-out

Wrong: one goroutine per item over a huge input with no `SetLimit` or semaphore —
goroutine and memory blow-up under load.

Fix: bound with a buffered `chan struct{}` semaphore acquired before `go`, or use
`errgroup.SetLimit(n)`.

Next: [01-parallel-job-runner.md](01-parallel-job-runner.md)
