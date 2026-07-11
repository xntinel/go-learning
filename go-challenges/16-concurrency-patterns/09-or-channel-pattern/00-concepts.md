# 9. Or-Channel Pattern — Concepts

The or-channel pattern answers one recurring question in concurrent Go: "I am holding several signals, and I want to act the moment the first of any of them fires." A signal here is a `<-chan struct{}` that a producer closes to broadcast an event, which is exactly how Go expresses cancellation and completion: a closed channel is permanently readable, so every goroutine waiting on it wakes at once. The pattern takes N such channels and folds them into a single `<-chan struct{}` that closes when any one of the inputs closes. That single channel is what you hand to a `select`, an `errgroup`, a `context`, or a long-running loop so it can stop at the first sign of any of its reasons to stop. This file is the conceptual ground floor: read it once and you will have everything needed to reason through the exercises, which build the pattern and then push it into two production shapes — combining a server request's cancellation sources, and racing N replicas for the fastest healthy answer.

## Concepts

### A Closed Channel Is A Latched Broadcast

The whole pattern rests on one property of channel close. A receive on an open channel blocks; a receive on a closed channel returns immediately, forever, for every receiver. Closing is therefore a one-shot, level-triggered, multi-listener broadcast: it cannot be undone, it never blocks, and a thousand goroutines selecting on the same closed channel all observe it. This is why Go uses `close` rather than a sent value to signal "done" — a sent value reaches exactly one receiver, but a close reaches all of them. The or-channel exploits this twice over: it listens on each input's close to learn that some reason to stop has occurred, and it closes its own output to broadcast that fact onward to whoever is waiting on the combined channel.

The corollary is the cardinal rule of close: a channel may be closed exactly once, and closing an already-closed channel panics. The entire correctness burden of the or-channel is making sure the output is closed exactly one time even when several inputs fire in the same instant.

### The Output Must Close Exactly Once

The natural implementation spawns one goroutine per input; each goroutine waits for its input to close and then closes the shared output. Under load this races: if two inputs close within the same scheduling slice, two goroutines reach `close(out)` and the second panics with "close of closed channel". There are two standard cures, and a robust implementation uses both.

The first cure is `sync.Once`. Wrapping the close in `once.Do(func() { close(out) })` makes the close idempotent: the first caller closes, every later caller's `Do` is a no-op. This is airtight regardless of how many goroutines race to fire it, which is why it is the default tool for "this must happen exactly once across goroutines".

The second cure is to give every goroutine a second exit. Each goroutine does not merely wait on its own input; it selects on its input and on the output channel. The first input to fire closes the output; every other goroutine's select then observes the now-closed output on its second arm and returns. Without that second arm, the losing goroutines block forever on inputs that may never fire, and they leak. The two cures are complementary: `sync.Once` prevents the double-close panic, and the `<-out` arm prevents the goroutine leak.

### Nil Channels Never Fire, So Skip Them

A receive on a nil channel blocks forever; it is the zero value of a channel and is never ready. If a nil slips into the input list and a goroutine is spawned to wait on it, that goroutine can only ever exit through its second arm (the closed output), and only if some other input fires first. If every input is nil, the goroutines wait forever and the output never closes. The clean response is to skip nil inputs before spawning anything: a nil input means "this reason can never occur", which is exactly "ignore it". Filtering nils up front also keeps the goroutine count honest — one live goroutine per channel that can actually fire.

### Two Shapes: Flat Fan-In And Recursive Merge

There are two well-known implementations, and the difference between them is frequently described wrongly, so it is worth being precise.

The flat form spawns one goroutine per input, each selecting on its input and on the shared output, with a `sync.Once` guarding the single close. For N inputs it uses N goroutines.

The recursive form merges channels pairwise in a balanced tree: split the inputs in half, build an or-channel for each half, then or those two halves together. The base cases are zero inputs (return an already-closed channel), one input (return it untouched), and two inputs (spawn a single goroutine that selects on both and closes its output via `defer`). A balanced binary tree with N leaves has N-1 internal nodes, and each internal node is one pairwise merge that spawns exactly one goroutine, so the recursive form uses N-1 goroutines. Both forms are therefore O(N) in goroutines; the recursive form does not reduce that to O(log N), and any claim that it does is false. What the recursion does change is two real things: the call-stack depth while building the tree is O(log N) rather than O(N), and every spawned goroutine selects over exactly two channels and closes its own private output, so no goroutine needs a shared `sync.Once` — each intermediate output has exactly one closer by construction. The flat form is simpler and is the right default; the recursive form is the one to reach for when you want each merge goroutine to own its output without shared synchronization.

### Zero Inputs: Already Closed, By Convention

Calling the combiner with no inputs is a boundary worth pinning down, because the two reasonable answers point in opposite directions. One reading is "there are no reasons to stop, so never fire" — a channel that never closes. The other is "there are no reasons to wait for, so proceed immediately" — an already-closed channel. The classic or-channel takes the second reading: zero inputs returns a channel that is already closed, so a caller's `select` falls straight through. This is a deliberate, documented contract, not an accident, and the tests pin it. When you build a server combiner in the exercises you will see the opposite convention can also be correct for its domain (no cancellation sources can mean "this work is never cancelled"), which is the point: the zero case is a design decision you make on purpose and then test.

### Where The Pattern Lives In Real Systems

In production the inputs to an or-channel are rarely anonymous test channels; they are the named reasons a unit of work should stop. A request handler typically has at least three: the process is shutting down (a server-wide channel closed on SIGTERM), the request's own deadline elapsed (a per-request timer), and an upstream dependency went fatally unhealthy (a watcher's done channel). Folding those into one done channel lets the handler write a single `select` arm for "stop, for whatever reason", and — crucially — lets it later ask which reason fired, so it can return the right status code or error. That is the second exercise.

A different production shape inverts the question. Instead of "stop at the first reason to stop", it asks "finish at the first good answer": fire the same request at N replicas, take the first one that comes back healthy, and cancel the rest so they stop wasting work. This is the tail-tolerance technique Dean and Barroso describe in "The Tail at Scale" — racing redundant requests to dodge a slow or sick replica. It is an or-channel in spirit (the first event wins and cancels the others) with two senior twists: the winning event must be a healthy result, not merely the first response, so a replica that fails fast must not win; and cancellation of the losers must be wired through `context` so their in-flight work actually stops. That is the third exercise.

## Common Mistakes

### Closing The Output From Every Goroutine

Wrong: each goroutine runs a bare `close(out)` the moment its input fires. The second input to fire panics with "close of closed channel", and the panic crashes the program rather than producing a recoverable error.

Fix: make the close idempotent with `sync.Once`, and give every goroutine a `<-out` arm so the losers exit by observing the closed output instead of racing to close it again.

### Leaking Goroutines On Inputs That Never Fire

Wrong: a goroutine waits on a single input with no second exit. If that input never closes — because it was nil, or because some other input won the race — the goroutine blocks forever and leaks.

Fix: skip nil inputs before spawning, and have every spawned goroutine select on both its input and the shared output so the closed output is always a viable exit.

### Claiming The Recursive Form Uses Fewer Goroutines

Wrong: asserting that the recursive merge spawns O(log N) goroutines. It spawns N-1, which is O(N), the same order as the flat form. Only the call-stack depth during construction is O(log N).

Fix: choose the recursive form for the right reason — each merge goroutine owns its output and needs no shared `sync.Once` — not for an imaginary reduction in goroutine count. State the trade-off truthfully.

### Treating "First To Respond" As "First Healthy"

Wrong: in a race-the-replicas combiner, returning the first response that arrives. A replica that errors instantly then "wins" the race, and the caller gets the fast failure instead of the slightly slower success that a healthy replica would have produced.

Fix: only a result with a nil error counts as a win; errored responses are recorded and the race continues until a healthy result arrives or every replica has failed, at which point the aggregated error is returned.

### Forgetting To Cancel The Losers

Wrong: taking the first answer and returning without cancelling the other in-flight requests. Those goroutines keep running, holding connections and burning the very capacity the redundancy was meant to protect.

Fix: derive a cancellable `context` for the fan-out, and cancel it the instant a winner is chosen so every loser observes `ctx.Done()` and stops; use a buffered result channel so the losers can still send and exit without blocking on a receiver that has already left.

---

Next: [01-or-channel-core.md](01-or-channel-core.md)
</content>
