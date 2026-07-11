# Goroutine Debugging Under Load — Concepts

It is 02:00 and a Go service is degrading in production: p99 latency is climbing,
the goroutine count on the dashboard is a slow upward ramp that never comes back
down, and one worker pool has stopped draining its queue. You cannot attach a
debugger to a live production process and single-step it, and adding `fmt.Println`
and redeploying is a thirty-minute round trip you do not have. The Go runtime
already carries the diagnostic instruments you need — goroutine stack dumps,
`net/http/pprof`, the goroutine/heap/block/mutex profiles, goroutine labels, and
the execution tracer — and a senior on-call engineer reaches for them in a fixed
order. This file is the model behind that toolkit. Read it once and each of the
independent exercises that follow builds one instrument you wire into a real
service and its test suite.

## Concepts

### A goroutine stack dump is the first move, not the last

Before you form a hypothesis, capture the shape of the system. `runtime.Stack(buf,
true)` writes the stack of *every* goroutine into `buf` and returns the number of
bytes written. For each goroutine the dump shows its ID, how long it has been in
its current state, the state itself, and the full call stack. The state is the
diagnostic gold: `chan receive`, `chan send`, `select`, `semacquire` (waiting on a
mutex or `WaitGroup`), `IO wait`, `sync.Cond.Wait`, or `running`. A thousand
goroutines all parked on `chan send` is a producer/consumer imbalance; a pile on
`semacquire` pointing at one function is lock contention; a steadily growing set on
`chan receive` inside your request handler is a leak. You read the parked-on state
first, and it tells you the class of bug before you have guessed anything.

Two properties make the dump trustworthy only if you respect them. First, you must
pass `all=true`. `runtime.Stack(buf, false)` captures *only the calling goroutine*
— exactly the one you did not need to see. Second, `runtime.Stack` stops the world
for the duration of the capture: every goroutine is paused while the runtime walks
their stacks. That is acceptable as a rare diagnostic, but it is why you never put
`runtime.Stack(_, true)` on a hot path. For continuous, tool-friendly, lower-
overhead access to the same information, use the goroutine *profile* from
`runtime/pprof` instead.

### Sizing the buffer, or losing the frames that matter

`runtime.Stack` writes into the buffer you give it and, if the dump does not fit,
it truncates *silently* and returns `len(buf)`. Under load with thousands of
goroutines a fixed 64 KiB buffer overflows, and the frames it drops are usually the
deepest ones — the function that is actually stuck. The correct idiom is to grow
the buffer until the call returns fewer bytes than the buffer holds:

```go
size := 1 << 16
for {
	buf := make([]byte, size)
	n := runtime.Stack(buf, true)
	if n < len(buf) {
		return string(buf[:n]) // the full dump fit
	}
	size *= 2 // it was truncated; double and retry
}
```

`n < len(buf)` is the only reliable signal that the dump was complete; `n ==
len(buf)` means "there may have been more."

### net/http/pprof and the debug surface you leak by accident

`net/http/pprof` is the production window into a running service: point `go tool
pprof` or `go tool trace` at it and you get CPU profiles, heap profiles, full
goroutine dumps, and execution traces from the live process. The trap is *how* it
installs itself. Importing the package for its side effects — `import _
"net/http/pprof"` — runs an `init` that registers all of those handlers on
`http.DefaultServeMux`. If your service serves its public listener from
`DefaultServeMux`, that single blank import silently exposes `/debug/pprof/`,
including `cmdline` and full stack dumps, to anyone who can reach the port. That is
an information-disclosure and denial-of-service hole (a CPU profile pins a core for
its duration).

The production rule is to register the handlers *explicitly* on a private mux
behind authentication, and never to serve `DefaultServeMux` on a public port. The
package exports exactly what you need for this: `pprof.Index`, `pprof.Cmdline`,
`pprof.Profile`, `pprof.Symbol`, `pprof.Trace`, and `pprof.Handler(name)` for the
named profiles (`goroutine`, `heap`, `block`, `mutex`). You wire those onto an
`http.ServeMux` of your own, wrap it in an auth middleware, and bind it to a
loopback or admin-only listener.

### Profile debug levels: pick the format for the consumer

Every `*pprof.Profile` (from `pprof.Lookup(name)`) writes with a `debug` level that
chooses the output format. `WriteTo(w, 0)` emits the binary protobuf format that
`go tool pprof` consumes. `WriteTo(w, 1)` emits a human-readable, *aggregated* text
form: goroutines grouped by identical stack, with counts. `WriteTo(w, 2)` (for the
goroutine profile) emits full per-goroutine stacks, the same shape as a panic dump,
one entry per goroutine. Use `0` when a tool will parse it, `1` for a quick
count-by-stack triage, `2` when you need to read individual stacks by eye.

### Goroutine labels turn an anonymous dump into an attributable one

A raw goroutine dump tells you *what* is stuck but not *whose* request it belongs
to. `runtime/pprof` labels fix that. `pprof.Do(ctx, pprof.Labels("tenant", t,
"request_id", id), fn)` sets those key/value labels on the current goroutine for
the duration of `fn`, and — this is the important part — any goroutine *started
inside* `fn` inherits them. So if your request middleware wraps the handler in
`pprof.Do`, every worker the handler spawns is tagged, and a goroutine profile
(debug=1) prints a `# labels: {...}` line for each stack. Now a stuck goroutine in
the dump names the tenant and request that owns it.

The boundaries matter: labels attach only within the labeled scope and to children
spawned there, never to goroutines that already existed or to work outside `Do`.
For a goroutine you start yourself outside a `Do` block, you attach labels manually
with `pprof.WithLabels(ctx, ...)` to build a labeled context and
`pprof.SetGoroutineLabels(ctx)` as the first line the goroutine runs. You read
labels back with `pprof.Label(ctx, key)` and enumerate them with
`pprof.ForLabels`.

### Block and mutex profiles: where time is lost waiting

A CPU profile shows where CPU cycles are *burned*. It says nothing about time lost
*waiting* — and a latency problem is very often waiting, not computing. Two
profiles answer the waiting question, and both are off by default because they add
per-event overhead:

- The **block profile** answers "where are goroutines blocking, and for how long?"
  — channel sends/receives, `select`, `WaitGroup` and `Cond` waits. Enable it with
  `runtime.SetBlockProfileRate(rate)`, where `rate` is roughly one sample per
  `rate` nanoseconds spent blocked (`1` samples every blocking event). Read it from
  `pprof.Lookup("block")`.
- The **mutex profile** answers "which lock is contended, and who was holding it?"
  Enable it with `runtime.SetMutexProfileFraction(n)`, which samples `1/n` of
  contention events (`1` samples all; it returns the previous fraction). Read it
  from `pprof.Lookup("mutex")`. The recorded stack is the *unlock* site — the
  holder that made others wait.

Both are diagnostic switches: turn one on to localize a *known* contention problem,
capture, then turn it back off (`SetBlockProfileRate(0)` /
`SetMutexProfileFraction(0)`). Leaving `1` enabled in production means paying
continuous sampling overhead forever.

### Goroutine leaks and why leak detection belongs in tests

A goroutine leak is unbounded growth in live goroutines under steady load. The
canonical causes are all cancellation failures: a worker whose context is never
cancelled, a send on a channel no one will ever receive from, a `range` over a
channel that is never closed. The signature on a dashboard is
`runtime.NumGoroutine()` (or the goroutine profile's `Count()`) trending upward
while load is flat.

You do not want to discover leaks in production; you want them to fail the build.
A leak guard snapshots the goroutine set before a unit runs, runs it, and at
cleanup polls — with backoff, after `runtime.GC()` — until the set returns to the
baseline. If it stays elevated, the unit leaked. The critical detail is the
*baseline diff*: the test runner has its own goroutines, so you never assert on a
raw count; you snapshot before, diff against that, and retry a few times because a
just-cancelled goroutine takes a moment to actually exit. This is exactly what the
widely-used `go.uber.org/goleak` does, and why it is wired into serious concurrent
codebases.

### A wedged worker pool, and the shape it makes in a dump

The classic production stall: a bounded worker pool where every worker blocks
forever on a send to a results channel because the reader has already exited (an
error path returned early, a client disconnected). In a dump this is a wall of
goroutines parked on `chan send` (for a bare send) or `select` (for a send inside a
select), all pointing at the same worker function. The fix is to make every send
cancellable — `select { case out <- v: case <-ctx.Done(): return }` — or to
guarantee a drain, so that no goroutine can block on a send that will never
complete.

### Graceful shutdown must be bounded, and dump on timeout

"Why won't my service exit?" is a shutdown that waits forever. The correct pattern
cancels the context, then waits on the pool with a *timeout*. When the timeout
fires, capture a full stack dump before exiting: the dump names precisely which
goroutines ignored cancellation, which is the only reliable way to debug a hung
shutdown after the fact. An unbounded `wg.Wait()` with no deadline and no dump
leaves you with a process stuck in `SIGTERM` and nothing to look at.

### Execution traces operate below profiles

Profiles are statistical summaries. An execution trace (`runtime/trace`) is the
event timeline: goroutine start/stop/block, GC start/stop and pauses, syscall
entry/exit, and per-processor utilization, all with timestamps. Reach for a trace
when latency is lost to *scheduling* — a goroutine that is runnable but not being
scheduled, a GC pause that lands on the critical path, a syscall that blocks a P —
rather than to CPU or a single lock. `trace.Start(w)` begins writing the binary
trace to `w` and returns an error if a trace is already running; `trace.Stop()`
ends it; the two must be paired (use `defer`). You annotate spans of interest with
`trace.NewTask` and `trace.WithRegion` so `go tool trace` can show your logical
operations against the scheduler timeline.

## Common Mistakes

### Calling runtime.Stack with all=false

Wrong: `runtime.Stack(buf, false)` captures only the calling goroutine, hiding
every worker — exactly the ones you needed. Fix: pass `all=true` for a service-wide
dump.

### Reading a silently truncated dump

Wrong: a fixed, too-small buffer (say 1 KiB or even 64 KiB under heavy load); the
dump is cut off and the deepest, most relevant frames vanish with no error. Fix:
grow the buffer until `runtime.Stack` returns strictly fewer bytes than
`len(buf)`.

### Leaking the pprof debug surface via a blank import

Wrong: `import _ "net/http/pprof"` on a service whose public listener uses
`http.DefaultServeMux`, exposing profiles and `cmdline` to anyone on the network.
Fix: register `pprof.Index`/`Handler(...)` explicitly on a private mux behind auth,
and never serve `DefaultServeMux` publicly.

### Expecting labels on goroutines that already existed

Wrong: assuming `pprof.Labels` retroactively tags running goroutines or work
outside `pprof.Do`. Labels attach only within the labeled scope and to children
spawned there. Fix: wrap the work in `pprof.Do`, or call
`pprof.SetGoroutineLabels` as the first thing a manually-started goroutine does.

### Reading an empty block/mutex profile and concluding "no contention"

Wrong: calling `pprof.Lookup("block")` or `"mutex"` without first enabling
sampling, seeing an empty profile, and declaring the system contention-free. Fix:
`SetBlockProfileRate` / `SetMutexProfileFraction` *before* the workload, then reset
to `0` after capture.

### Leaving profiling rates enabled in production

Wrong: shipping `SetBlockProfileRate(1)` or `SetMutexProfileFraction(1)` and paying
continuous sampling overhead. Fix: enable to diagnose, capture, then disable.

### Leak-testing on a raw goroutine count

Wrong: asserting `runtime.NumGoroutine()` equals a hard-coded number, so the test
runner's own goroutines produce flakes. Fix: snapshot a baseline, diff against it,
and poll with backoff after `runtime.GC()` before failing.

### A non-cancellable send that wedges the pool

Wrong: `out <- v` in a worker with no `ctx.Done()` branch, so a consumer that exits
early blocks the whole pool permanently. Fix: make every send a `select` that also
watches `ctx.Done()`, or guarantee a drain.

### Unbounded graceful shutdown

Wrong: `wg.Wait()` with no timeout and no diagnostic, so one stuck worker hangs
shutdown forever. Fix: bound the wait with `time.After` and dump stacks on timeout.

### Unpaired trace.Start/Stop

Wrong: calling `trace.Start` twice without `Stop` (the second returns an error you
ignore), or forgetting `Stop` so the trace file is incomplete and unparseable. Fix:
pair them with `defer trace.Stop()` and check the error from `Start`.

### Treating a CPU profile as a cure-all for a latency problem

Wrong: profiling CPU to explain latency that is actually *waiting* time — a CPU
profile shows where cycles burn, not where goroutines park. Fix: use the block or
mutex profile, or an execution trace, when the time is lost waiting.

Next: [01-dump-goroutine-stacks-under-load.md](01-dump-goroutine-stacks-under-load.md)
