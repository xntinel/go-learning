# Select Statement Basics — Concepts

Before `context.Context` enters the picture (that is lessons 04 and later), every
fan-in, every hedged read, every stoppable worker, and every result collector a
backend engineer writes is built on one primitive: `select`. `context` itself is
a thin, ergonomic wrapper over the same machinery — a `context.Done()` case in a
`select` is exactly a receive on a done channel. So the fastest way to build real
intuition for cancellation is to build the patterns by hand first, with raw
channels and `select`, and see precisely where they leak, spin, or deadlock.

That is what this lesson does. Each module is a real backend artifact — a replica
multiplexer, a shard-event merge, a stoppable queue consumer, a scatter-gather
coordinator, a hedged read, a pipeline stage, a pub/sub dispatcher, a fairness
audit — and each ships a `*_test.go` that runs under `-race`, so the contract is
machine-checked rather than asserted in prose. Getting `select` semantics wrong
produces the two most expensive concurrency bugs in production Go: a busy-spin
that pegs a CPU core, and a goroutine leak that slowly eats memory until the
process is OOM-killed. Both are shown here as concrete, reproducible failures with
the fix beside them.

## Concepts

### A select waits on channel operations, not on values

A `select` statement is to channel operations what `switch` is to values, with one
decisive difference: the cases are **not** evaluated top-to-bottom. The runtime
looks at every case, finds the set that can proceed right now, and — this is the
part people get wrong — makes a *uniform pseudo-random fair choice* among them.
The Go spec is explicit: "If one or more of the communications can proceed, a
single one that can proceed is chosen via a uniform pseudo-random selection."

That randomness is not a quirk to work around; it is the mechanism that keeps one
channel from monopolizing a hot `select` over a long run. If you have a `select`
that is almost always ready on two channels, the pseudo-random choice is what
guarantees, statistically, that neither source starves. Module 09 measures this
empirically over 100k iterations and asserts the split lands near 50/50 — the
concrete evidence behind the rule "never assume listing order implies priority."

### Blocking without a default, and how the runtime fails fast

A `select` with no `default` branch blocks the calling goroutine until at least
one case can proceed. This is the normal, correct behavior — it is how a worker
parks cheaply until either a job or a stop signal arrives, consuming zero CPU
while blocked.

The failure mode is instructive. If *no* goroutine will ever send on or close any
watched channel, the whole program can make no progress. The runtime detects this
and panics with `fatal error: all goroutines are asleep - deadlock!`. That turns
"did I forget to start a producer?" into a fast-failing question: a `select` that
should have received but instead deadlocks tells you immediately that a producer
was never launched, or was launched but blocked elsewhere. In tests this is a
feature — a forgotten producer crashes loudly instead of hanging silently.

### A receive on a closed channel is always ready — and that pegs a core

This is the single most expensive `select` misunderstanding in production. A
receive on a closed channel does not block; it succeeds immediately, returning the
element type's zero value, with the comma-ok boolean reporting `false`. Inside a
`select`, a closed channel therefore looks **ready forever**.

So this loop, after the channel is closed, spins at 100% CPU:

```go
for {
	select {
	case v := <-ch: // ch is closed: this case is ready on every iteration
		use(v) // v is the zero value, forever
	}
}
```

The receive fires on every iteration, instantly, doing no useful work, pegging one
core. The idiomatic drain that does not spin is `for v := range ch`: a range over a
channel exits cleanly when the channel is drained and closed. When you genuinely
need a `select` (because there is a second case), detect the close with comma-ok
and stop feeding that case — either `break` out, or set the channel variable to
`nil` (next point). Module 07 builds a broken stage and its fix side by side and
asserts, with a bounded time budget, that the correct version actually terminates.

### A nil channel blocks forever — which is a feature

A send to, or receive from, a `nil` channel blocks forever. That sounds like a bug
generator, and it is when accidental, but it is also a precise tool: setting a
channel variable to `nil` **disables its case** in a `select`. When one source of a
multi-source loop is exhausted, you do not restructure the loop; you nil out that
channel and the `select` simply stops considering it, continuing to serve the
remaining cases. The trap is the mirror image — leaving a channel `nil` by
accident silently disables a case you needed, and the symptom (a case that never
fires) is baffling until you remember the rule.

### reflect.Select for a runtime-sized set of channels

A static `select` has a fixed number of cases written into the source. But real
multiplexers wait on a set of channels whose size is only known at runtime — one
per shard, one per replica, one per subscriber. You cannot write a fixed `select`
for that. The only tool is `reflect.Select`:

```go
func Select(cases []SelectCase) (chosen int, recv Value, recvOK bool)

type SelectCase struct {
	Dir  SelectDir // SelectRecv, SelectSend, or SelectDefault
	Chan Value     // the channel
	Send Value     // the value to send (send cases only)
}
```

`recvOK` mirrors comma-ok exactly: for a receive case it is `false` when the
channel was closed. The costs are real — each call allocates the `[]SelectCase`
slice and pays reflection overhead, and the number of cases is capped at 65536 —
so `reflect.Select` is the right tool *only* when the arity is genuinely dynamic.
Reaching for it to avoid writing a plain three-case static `select` trades away
compile-time type safety and speed for nothing. Module 01 (dynamic replica set)
and module 08 (dynamic subscriber set) use it because they must; every other
module uses a static `select` because it can.

### The fan-in merge contract

Merging N source channels into one is the canonical concurrency pattern, and it
has an exact contract that, violated, panics or leaks:

- one goroutine per source, each draining its source with `for v := range src`;
- a `sync.WaitGroup` to detect when *every* source goroutine has finished;
- a *single* `close(out)`, performed by a separate goroutine, only after
  `wg.Wait()` returns.

The load-bearing rule is that the output channel is closed exactly once, and never
by a producer goroutine. If a producer closed `out`, a second producer still
running would send on a closed channel and panic. Closing from one dedicated
goroutine after `wg.Wait()` is the only safe shape. Module 02 builds this, and
`-race` is the real assertion — it catches an unsynchronized or double close that
a functional test might miss on a lucky scheduling.

### Buffer the result channel so losers can exit

In any first-of-N pattern — hedged replica reads, scatter-gather where the first
error wins — you launch several goroutines, take the first useful reply, and
*abandon* the rest. The abandoned goroutines are still trying to send their result.
If the result channel is unbuffered and nobody is receiving anymore, those
goroutines block on the send **forever**: a goroutine leak, which is also a memory
leak, because everything the goroutine references stays alive.

Two fixes, usually combined: buffer the result channel to `len(sources)` so every
producer can complete its send and return even if no one reads it; and close a
shared `stop` channel so in-flight producers notice they lost and abandon their
work early. Module 06 builds a hedged read and asserts, via `runtime.NumGoroutine`
before and after, that the loser goroutines actually exit and the count returns to
baseline.

### A single-case select is an anti-pattern

`select { case v := <-ch: use(v) }` is just `v := <-ch` with extra noise and a
false suggestion that something is being arbitrated. Reserve `select` for two or
more channel operations, or for one operation plus a `default`/timeout. If you
find a one-case `select`, delete the `select`.

### Modern idioms (Go 1.24-1.26)

The loop-variable-per-iteration change landed in Go 1.22, so the old
`src := src` shadow inside a `for _, src := range sources` loop is unnecessary —
each iteration already has its own `src`. Prefer `sync.WaitGroup.Go(f)` (added in
Go 1.25) over the manual `wg.Add(1); go func(){ defer wg.Done(); f() }()` triad;
it does the `Add`/`Done` bookkeeping for you and cannot be mis-paired. Use
`for i := range n` for count loops. Wire cancellation in tests with `t.Context()`
(Go 1.24), which is cancelled automatically when the test ends.

## Common Mistakes

### Assuming case order implies priority

Wrong mental model: a `select` evaluates cases top-to-bottom like a `switch`, so
the first-listed channel has priority. Right: the runtime makes a uniform
pseudo-random choice among all ready cases, and the choice is not reproducible
across runs. Fix: when you truly need priority, build it explicitly with a nested
`select` plus a `default` (lesson 08); never rely on listing order.

### Busy-spinning on a closed channel

Wrong: `for { select { case v := <-ch: use(v) } }` after `close(ch)`. The closed
receive is ready on every iteration and pegs a core at 100%. Fix: drain with
`for v := range ch`; or use comma-ok and `break` when `ok == false`; or set the
channel to `nil` to disable that case.

### Spawning the producer after the consumer starts selecting

Wrong: enter the `select` first, then start the goroutine that feeds one of its
cases. The producer's first send may not have happened yet, the `select` finds
nothing ready, and the consumer blocks intermittently — a scheduling race that is
hard to reproduce. Fix: start the producers (to their first send or close) in the
same function that constructs and returns the channels, before the consumer
selects.

### Leaking loser goroutines in first-of-N patterns

Wrong: after taking the first reply, the other goroutines stay blocked forever on
an unreceived send. Fix: buffer the result channel to `len(sources)`, and/or close
a shared `stop` channel so losers unblock and return.

### Closing the output channel from a producer goroutine in fan-in

Wrong: one producer calls `close(out)` when it finishes; another producer still
running then sends on the closed channel and panics. Fix: close `out` exactly once,
from a dedicated goroutine, only after `wg.Wait()` returns.

### Wrapping a single channel operation in a select

Wrong: `select { case v := <-ch: ... }` with no other case or default. Fix: write
`v := <-ch`. Reserve `select` for two or more operations.

### Using reflect.Select for a statically known set of channels

Wrong: reaching for `reflect.Select` because it "looks flexible," paying
allocation and reflection cost and losing compile-time type checking, when the
channel count is fixed and small. Fix: write a plain static `select`; use
`reflect.Select` only when the arity is genuinely runtime-dynamic.

### Forgetting the nil-channel rule in both directions

Wrong: leaving a channel `nil` by accident, silently disabling a case you needed
and producing a case that mysteriously never fires — or, conversely, not knowing
the trick and restructuring an entire loop when nil-ing one channel would have
disabled its exhausted case cleanly. Fix: treat `nil` as the deliberate off-switch
for a `select` case, and audit for accidental `nil` when a case never fires.

Next: [01-multiplex-first-ready.md](01-multiplex-first-ready.md)
