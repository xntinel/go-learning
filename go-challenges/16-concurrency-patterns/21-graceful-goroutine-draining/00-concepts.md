# 21. Graceful Goroutine Draining — Concepts

A hard kill discards everything in flight: half-written files, uncommitted transactions, unacknowledged messages. Graceful draining instead moves a program through three ordered phases — stop accepting new work, wait for in-flight work to finish under a deadline, then force what is left — so that nothing acknowledged is lost and no goroutine outlives the process that owns it. This file is the conceptual foundation for the three exercises that follow: a reusable draining worker pool, a connection server that drains in-flight handlers on shutdown, and a background queue worker that empties itself on a SIGTERM-style signal. Read it once and you will have the reasoning needed to build each one as an independent, self-contained Go module.

## The Three Phases of a Graceful Stop

Every correct shutdown is the same three phases in the same order, whatever the workload.

Stop-accepting is the first phase and it must be cheap and instantaneous: the system refuses new work so the set of outstanding work stops growing. For a worker pool this means `Submit` returns an error; for a network server it means the listener stops accepting connections; for a queue consumer it means producers are told the door is closed. The defining property is that this phase does not interrupt anything already running — it only bounds the problem so the next phase can terminate.

Drain-in-flight is the second phase: wait for every unit of work that was already accepted to run to completion. This is a join — the parent blocks until the children finish — and the canonical join barrier in Go is `sync.WaitGroup`. The wait can take as long as the single longest job, which is why this phase is always wrapped in a deadline. A drain without a deadline is not graceful, it is a hang waiting to happen: one stuck handler and the process never exits, and an orchestrator that asked it to stop eventually sends an un-catchable `kill -9` and you lose the very durability the drain was supposed to protect.

Force-exit is the third phase and it only runs if the deadline fires before the drain finishes. Forcing means actively interrupting the work that did not finish in time — cancelling its context, closing its connection, aborting its loop — and then exiting. The contract of the force phase is that the caller is never blocked past the deadline: a graceful stop has an upper bound on how long it can take, by construction. The deadline is the budget; the force is what spends the remainder of it.

## WaitGroup as the Join Barrier

`sync.WaitGroup` counts outstanding goroutines. Each goroutine is registered with `wg.Add(1)` *before* it is launched, and signs off with `defer wg.Done()` as its first statement, so the decrement happens no matter how the goroutine returns. `wg.Wait()` blocks until the counter reaches zero.

The one rule that matters is where `Add` is called. It must run in the parent, before `go f()`, never inside the goroutine. If the goroutine itself calls `wg.Add(1)`, the scheduler may not have run it by the time the parent reaches `wg.Wait()`; the counter is still zero, `Wait` returns immediately, and the parent proceeds while children are still alive. That is the difference between a drain and a data race.

To put a deadline on the wait, `Wait` itself does not take a context, so the idiom is to run it in a throwaway goroutine that closes a channel when it returns, then `select` on that channel against `ctx.Done()`:

```go
done := make(chan struct{})
go func() { wg.Wait(); close(done) }()
select {
case <-done:
	return nil
case <-ctx.Done():
	return ctx.Err()
}
```

If the context fires first, the function returns `context.DeadlineExceeded` and the caller proceeds to force. The throwaway goroutine is not a leak: once the real work finishes, `Wait` returns, it closes `done`, and it exits — which is exactly why the work must also be made to finish on the force path.

## Why Closing the Jobs Channel Is the Wrong Stop Signal

The tempting design for a worker pool is to signal shutdown by closing the jobs channel: workers loop over `for job := range jobs`, and `close(jobs)` ends every range loop after the buffer drains. It reads beautifully and it has a fatal flaw in the presence of an external `Submit`.

`Submit` and `Shutdown` race. `Submit` does `jobs <- job`; `Shutdown` does `close(jobs)`. A send on a closed channel panics, and a panic on the send path takes down the whole process. Guarding the send with an atomic "closed" flag does not fix it: the flag is checked, then the send happens, and `Shutdown` can close the channel in the window between the two — the classic check-then-act race. Worse, a `Submit` that is *blocked* on a full channel is already past any flag check; when `Shutdown` closes the channel, that blocked send panics. There is no flag placement that closes this hole, because the hole is the close itself.

The rule is therefore: a channel must be closed by its sender, and only when no sender can still be running. When sends come from arbitrary external callers, that condition can never be guaranteed, so the jobs channel is never closed. The correct stop signal is a *separate* channel — a `quit` channel — that shutdown closes exactly once (guarded by `sync.Once`). Closing a channel is a broadcast: every goroutine selecting on it unblocks at once, which is precisely what a stop signal wants and what a single send (which wakes only one receiver) cannot give. Workers select between a real job and `quit`; `Submit` selects between sending a job and `quit`, returning an error the moment `quit` is closed. No send ever races a close, because the only channel that gets closed is one that nobody sends on.

When `quit` closes, each worker still has to consume whatever was already buffered before it exits — that is the drain. A worker that simply returns on `quit` would strand buffered jobs. So the worker, on seeing `quit`, switches to a non-blocking drain loop (`select` with a `default`) that pulls buffered jobs until the channel is empty, then exits. Because every buffered job is finite and some worker eventually pulls each one, nothing accepted is dropped.

## Detecting a Goroutine Leak

A drain that returns but leaves goroutines running is not graceful, it is a leak with good manners. The whole point of phases two and three is that *after* the stop completes, the goroutine population is back to where it started. That is a testable assertion.

`runtime.NumGoroutine()` returns the current count. Capture a baseline before starting the subsystem, run the full lifecycle, and after shutdown assert the count has returned to the baseline. Because goroutine teardown is asynchronous — a goroutine can have returned from its function microseconds before the runtime stops counting it — the assertion must poll with a short sleep and a retry budget rather than checking once. The check is only meaningful when no other goroutines are being spawned concurrently, so leak tests must not run with `t.Parallel()`: a non-parallel test in Go runs while every parallel test in the package is paused, giving a stable population to measure against. Run every one of these subsystems under `go test -race`; the race detector is what proves the stop signal, the WaitGroup, and the shared counters are actually synchronized and not merely lucky.

## Signals: NotifyContext and the Drain-on-SIGTERM Pattern

A container runtime stops a process by sending `SIGTERM` and starting a grace-period timer (Kubernetes defaults to thirty seconds); if the process has not exited when the timer expires, it sends `SIGKILL`, which cannot be caught. The whole job of a graceful process is to catch `SIGTERM`, drain within the grace period, and exit on its own — so that the `SIGKILL` never has to be sent and no in-flight work is lost.

The modern idiom is `signal.NotifyContext`, which returns a context that is cancelled when one of the named signals arrives:

```go
ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGTERM)
defer stop()
```

This collapses signal handling into the same context-cancellation mechanism the rest of the drain already uses: the worker's run loop selects on `ctx.Done()`, and a `SIGTERM` cancels it exactly as a manual `cancel()` would in a test. The `stop` function returned alongside the context must be deferred — it unregisters the signal handler and restores the default disposition, which both prevents a handler leak and lets a second `SIGTERM` terminate a process that is wedged. Because a context cancel and a real signal drive the identical code path, the run loop can be unit-tested with a plain `context.WithCancel` and only the wiring exercised against an actual `syscall.SIGTERM`.

## Nothing-Lost: Ordering the Close Against the Send

"Drains its queue, persists nothing lost" is a stronger guarantee than "stops cleanly," and getting it right is a question of ordering a single flag-flip against the producers' sends. The danger is symmetric to the channel-close race: if a consumer decides "I am done, the queue is empty" at the same instant a producer slips one more item in, that item is accepted by the producer but never processed by the consumer — silently lost.

The fix is to make "stop accepting" and "enqueue" mutually exclusive over a single mutex. A producer takes the lock, checks a `closed` flag, and only then attempts to enqueue; the consumer, when it begins draining, takes the same lock to set `closed = true`. The mutex serializes the two: either the producer enqueued first (the item is in the buffer and the subsequent drain will process it) or the consumer set `closed` first (the producer sees it and is rejected, so the item was never accepted). There is no third outcome, so every item the producer was *told* was accepted is guaranteed to be processed before the consumer exits. Backpressure — a full buffer — is reported to the producer as a distinct error so it can retry rather than block, keeping the accept decision instantaneous and the lock hold time tiny.

## Common Mistakes

### Closing the Jobs Channel to Signal Shutdown

Wrong: signalling workers by `close(jobs)` while external callers still run `jobs <- job` in `Submit`.

What happens: a send on a closed channel panics and crashes the process. An atomic "closed" flag checked before the send does not help — the channel can be closed in the check-then-send window, and a `Submit` already blocked on a full channel is past the check entirely.

Fix: never close a channel that external callers send on. Close a separate `quit` channel exactly once with `sync.Once`; have both `Submit` and the workers `select` on it. Workers drain buffered jobs with a non-blocking `select`/`default` loop before exiting.

### A Drain With No Deadline

Wrong: `Shutdown` calls `wg.Wait()` directly and returns when it completes.

What happens: one stuck or slow job makes `Shutdown` block forever. The orchestrator's grace timer expires, it sends `SIGKILL`, and the in-flight durability the drain was meant to protect is destroyed anyway.

Fix: wrap `wg.Wait()` in a goroutine that closes a `done` channel, then `select` on `done` against `ctx.Done()`. Return `ctx.Err()` when the deadline fires, and use the force path to interrupt whatever did not finish.

### Forcing Work That Cannot Be Interrupted

Wrong: the force path closes connections or sets a flag, but the in-flight work is a bare `time.Sleep` (or any blocking call) that ignores it.

What happens: `Shutdown` returns "forced" but the goroutines keep running to their natural end, outliving the process boundary and leaking. The force was cosmetic.

Fix: make the work interruptible at the point it blocks — `select` on `time.After(d)` against a force channel, or pass a context the work checks. The force phase then actually unblocks the work, the WaitGroup reaches zero, and there is no leak.

### Calling wg.Add(1) Inside the Goroutine

Wrong: `go func() { wg.Add(1); defer wg.Done(); ... }()`.

What happens: if the goroutine has not been scheduled when the parent calls `wg.Wait()`, the counter is still zero, `Wait` returns early, and the parent proceeds while the goroutine is still running.

Fix: call `wg.Add(1)` in the parent, before `go f()`. The goroutine's `defer wg.Done()` balances it.

### Losing an Item to a Close-vs-Send Race

Wrong: the consumer decides the queue is empty and returns at the same moment a producer enqueues one more item under no shared lock.

What happens: the producer's send succeeds (it was told the item was accepted) but the consumer has already exited, so the item is never processed — a silent loss that a count check at the end will catch but a casual test will not.

Fix: guard the producer's "check closed, then enqueue" and the consumer's "set closed" with the same mutex so they are serialized. Report a full buffer as a distinct backpressure error rather than blocking under the lock.

---

Next: [01-draining-worker-pool.md](01-draining-worker-pool.md)
</content>
</invoke>
