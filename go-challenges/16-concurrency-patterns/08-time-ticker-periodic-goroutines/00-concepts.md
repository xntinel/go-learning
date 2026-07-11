# 8. Periodic Goroutines with time.Ticker — Concepts

Almost every long-running Go service needs to do something on a schedule: refresh a cached config every thirty seconds, flush a metrics buffer every second, expire stale sessions every minute, send a heartbeat every five. The naive way to write that loop drifts, leaks goroutines, and cannot be stopped cleanly. The disciplined way is built on `time.Ticker`, a `select` that listens for both the tick and a shutdown signal, and a `sync.WaitGroup` so the owner knows the worker has actually exited before it frees the state the worker touches. This file is the conceptual foundation. Read it once and you will have everything you need to reason through the exercises, which build a periodic runner, a bounded variant, a production config refresher with jitter and an injectable clock, and a metrics-flush loop that batches and drains on shutdown — each as an independent, self-contained Go module.

## Concepts

### Why Not a Sleep Loop

The first instinct is `for { work(); time.Sleep(d) }`. It is wrong for two independent reasons, and both matter in production.

The first is drift. The period you actually get is `time.Taken(work) + d`, not `d`. If `work` takes 8 ms and `d` is 100 ms, you fire every 108 ms, and the error compounds: after an hour the loop has run hundreds of fewer times than "once every 100 ms" would suggest, and the firings have walked away from any wall-clock phase you cared about. A `time.Ticker` instead schedules ticks on a fixed cadence relative to its own start, so the time spent inside the handler does not push the next tick later; the ticker is always trying to fire at multiples of `d` from when it was created.

The second is cancellation. A goroutine parked in `time.Sleep` cannot be woken early. If the process is shutting down, the loop will not notice until the current sleep elapses, and there is no channel to select on, so there is no clean way to tell it to stop. A ticker exposes its ticks as a channel, and a channel is selectable, which is the whole point: a periodic worker must wait on "the next tick OR a stop signal", and only a channel-shaped tick lets you write that `select`.

### Ticker Drops Missed Ticks, It Does Not Queue Them

`time.NewTicker(d)` returns a `*time.Ticker` whose field `C` is a `<-chan time.Time`. The runtime sends the current time on `C` every `d`. The behaviour that distinguishes a `Ticker` from a `Timer` is what happens when the receiver is slow: if your handler takes longer than `d`, the runtime does not pile up a backlog of pending ticks to deliver later. It coalesces them. The channel never holds more than one outstanding tick, so a handler that runs long simply causes some ticks to be skipped, and when it finishes it sees the next tick on the normal cadence rather than a flurry of catch-up ticks.

This is exactly the semantics you want for "do this every N seconds, and if one run is slow, just do it again next time, do not try to make up for lost runs". A `Timer`, by contrast, fires once; you would have to `Reset` it each cycle, and a `Reset` after the timer has already fired is the supported idiom but it is single-shot scheduling, not a cadence. Reach for `Ticker` when you want a cadence and for `Timer` when you want one delayed action (or a per-cycle re-randomized delay, which is how jitter is built — see below).

### The Receiver Must Select on Done

The heart of the pattern is the receive loop:

```go
for {
	select {
	case t := <-ticker.C:
		handler(t)
	case <-done:
		return
	}
}
```

Both cases are mandatory. Without `case <-done`, there is no way to stop the goroutine: it will block on the next `<-ticker.C` forever, and the only thing that ever unblocks it is another tick, which keeps it alive rather than ending it. `done` is a channel the owner closes to mean "no more work, return now". Closing a channel makes every receive on it proceed immediately with the zero value, so closing `done` wakes the `select` even if a tick is not due for another `d`. That is why the stop signal is a channel and why you close it rather than send on it — closing broadcasts to every receiver at once and is idempotent in observation: every `<-done` after the close returns instantly.

### Stop Does Not Close the Ticker Channel

`(*time.Ticker).Stop()` turns the ticker off so no further ticks are sent. The single most common misconception is that `Stop` closes `ticker.C`. It does not. The standard library documents this explicitly: "Stop does not close the channel, to prevent a concurrent goroutine reading from the channel from seeing an erroneous tick." So you cannot detect a stopped ticker by checking for a closed channel — a `case t, ok := <-ticker.C` will never observe `ok == false` from `Stop`, because the channel is simply quiet, not closed. The corollary: never write `for t := range ticker.C { ... }` and expect `Stop` to end the loop. A `range` over a channel ends only when the channel is closed, and `Stop` never closes it, so that loop would block forever after `Stop`. Always use the explicit `select { case <-ticker.C: ...; case <-done: return }` form, and own your own `done` channel for the exit.

The mirror-image mistake is to close `ticker.C` yourself to "signal" the consumer. You must never do this. The channel is owned by the runtime, which sends on it; if you close it, the runtime's next send hits a closed channel and the program panics. The stop signal is always a separate `done chan struct{}` that you own and close.

### Stop Must Wait for the Goroutine to Exit

`Stop` on your own periodic type usually does three things, in this order: stop the ticker so no more ticks arrive, close `done` to wake the `select`, then wait on a `sync.WaitGroup` for the goroutine to actually return. The third step is the one people forget, and it is the difference between a clean shutdown and a use-after-free. If `Stop` returns before the goroutine has exited, the caller believes the worker is gone and proceeds to tear down state — close a database handle, free a buffer, exit the process — while the handler may still be mid-run touching exactly that state. Adding `p.wg.Wait()` at the end of `Stop` makes `Stop` a synchronization point: when it returns, the goroutine has provably finished its last handler call and executed its deferred `wg.Done`, so nothing the worker touched is in flight.

### The Garbage-Collection Change in Go 1.23

A real and recent detail worth knowing on Go 1.26: as of Go 1.23 the runtime changed how unreferenced timers and tickers are collected. Before 1.23, a `Ticker` you dropped without calling `Stop` could not be garbage collected, which made a forgotten `Stop` a genuine memory leak. Since 1.23, a `Timer` or `Ticker` that becomes unreachable is collected even if it was never stopped, and the tick channel is now effectively unbuffered. This does not make `Stop` optional — `Stop` still matters because it halts the ticks promptly and lets a still-referenced ticker stop doing work — but it does mean a leaked-and-forgotten ticker no longer pins memory the way it once did. The old behaviour is available under `GODEBUG=asynctimerchan=1` for code that depended on the buffered channel. Treat `Stop` as mandatory for correctness (stopping work) rather than only for collection.

### Jitter: Why Identical Schedules Are Dangerous

A subtle production hazard appears when many instances of a service all refresh on the same fixed period. If a thousand pods each reload their config every thirty seconds and they were all started by the same deploy, their ticks are phase-aligned, and every thirty seconds the config server is hit by a synchronized thundering herd of a thousand requests followed by silence. The fix is jitter: perturb each instance's period by a small random amount so the fleet's requests spread into an approximately constant rate instead of synchronized spikes. The same reasoning that makes randomized backoff beat fixed backoff for retries makes a jittered refresh interval beat a fixed one for periodic background work.

You cannot get jitter from a plain fixed-period `Ticker`, because its cadence is constant by construction. The clean way to build a jittered cadence is a re-armed `Timer`: each cycle, compute `base + random(0, jitter)` and reset the timer to that, so every interval is independently randomized. For testing, the randomness must be injectable — a deterministic test passes a jitter function that returns a fixed value (or zero) so the schedule is reproducible, and the production code passes one backed by `math/rand`.

### Making Periodic Code Deterministically Testable: Inject the Clock

Code that calls `time.NewTicker` directly is hard to test, because the only way to advance it is to sleep in real wall-clock time, which makes tests slow and flaky. The senior technique is to depend on a small `Clock` interface rather than on the `time` package directly. The interface exposes the operations the code needs — typically a method that returns a ticker-like object with a tick channel and `Stop`, and possibly `Now` — and ships in two implementations: a real one that wraps `time.NewTicker`, and a fake one for tests whose ticks the test fires manually. With an injected fake clock, a test creates the worker, calls a method like `clock.Tick()` to deliver exactly one tick, synchronizes on a channel the handler signals, asserts the effect, and stops the worker — all without a single real sleep and with completely deterministic ordering. This is the pattern that lets the config refresher and the metrics flusher in the later exercises be tested under `-race` in microseconds rather than seconds, with no tolerance bands and no flakiness.

### Batching on Interval or on Buffer-Full, and Draining on Shutdown

A common shape for periodic work is not "do one thing per tick" but "accumulate items and flush them in batches". A metrics or log pipeline buffers incoming records and writes them out either when enough have accumulated to make a worthwhile batch or when a maximum latency has elapsed, whichever comes first. That is a `select` with three arms in one goroutine that exclusively owns the batch slice: an arm that receives a new item and appends it (flushing early if the batch just reached its size cap), an arm that receives a tick and flushes whatever has accumulated, and an arm that receives the stop signal. Having a single goroutine own the batch means no lock is needed around it — ownership is the synchronization. The shutdown arm carries one extra obligation that is easy to miss: before returning, it must drain any items still queued and perform a final flush, so that records submitted just before shutdown are not silently dropped. A flusher that exits the instant it sees `done`, abandoning a half-full batch, loses data on every clean shutdown.

## Common Mistakes

### Using a Sleep Loop Instead of a Ticker

Writing `for { work(); time.Sleep(d) }` drifts (the period becomes `work + d`) and cannot be cancelled early because a sleeping goroutine has no channel to select on. Use `time.NewTicker(d)` and a `select` over the tick channel and a `done` channel.

### Expecting Stop to Close or Range to End

Writing `for t := range ticker.C { handler(t) }` and assuming `Stop` ends the loop. `Stop` does not close `C`, so the `range` blocks forever after `Stop`. Use the explicit two-arm `select` and close your own `done` channel to exit.

### Checking ok on the Tick Channel

Writing `case t, ok := <-ticker.C: if !ok { return }` to detect a stopped ticker. Because `Stop` never closes `C`, `ok` is never `false` from a stop; the branch is dead code that hides the real exit mechanism, which is the `done` case. Drop the `ok` check and stop via `done`.

### Closing the Ticker Channel Yourself

Calling `close(ticker.C)` to signal the consumer panics the program: the runtime owns `C` and its next send hits a closed channel. Own a separate `done chan struct{}` and close that.

### Stop That Does Not Wait

Returning from `Stop` after closing `done` but before the goroutine has exited lets the caller tear down state the handler is still touching — a use-after-free. End `Stop` with `wg.Wait()` so it is a real synchronization point.

### Forgetting to Drain on Shutdown

A batch-and-flush loop that returns the instant it sees `done`, without draining the queue and flushing the partial batch, drops every item submitted just before shutdown. The shutdown arm must drain and do a final flush before returning.

---

Next: [01-periodic-runner.md](01-periodic-runner.md)
