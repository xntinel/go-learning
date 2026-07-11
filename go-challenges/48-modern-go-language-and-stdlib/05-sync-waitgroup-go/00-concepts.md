# 5. Spawning Goroutines with sync.WaitGroup.Go — Concepts

The `Add(1)` / `go func(){ defer wg.Done(); ... }()` dance is the most-copied
snippet in concurrent Go, and the easiest to get subtly wrong: forget `Done` and
`Wait` hangs forever; call `Done` twice and it panics; move `Add` inside the
goroutine and you race against `Wait`. Go 1.25 added `WaitGroup.Go`, which folds
the counter management and the goroutine launch into one call that cannot get the
bookkeeping wrong. This file is the conceptual foundation: read it once and you
will have what you need to reason through both exercises, which build a bounded
parallel `Map` and a nested-`Go` concurrent tree fold as independent,
self-contained Go modules.

## Concepts

### What WaitGroup.Go does

`wg.Go(f)` increments the counter, runs `f` in a new goroutine, and decrements the
counter when `f` returns — `Add(1)` and `defer Done()` in a single call. The
bookkeeping is no longer yours to misplace, which removes a whole class of bugs:
the missing `Done` that deadlocks `Wait`, the extra `Done` that panics, and the
`Add` placed inside the goroutine where it races the `Wait`. The same memory-model
guarantee holds: the return of `f` synchronizes before the return of any `Wait`
it unblocks, so values `f` writes before returning are visible after `Wait`.

### It does not recover panics

`wg.Go` is not a safety wrapper. Its documentation states "the function `f` must
not panic" — a panic in `f` propagates and crashes the process exactly as it would
in a bare goroutine, because no other goroutine can recover it. If the work can
fail, return an error from within `f` and collect it; do not rely on `Go` to
contain a panic.

### The waitgroup vet check

Go 1.25 also shipped a `waitgroup` analyzer in `go vet` that flags misuse such as
calling `wg.Add` inside the goroutine started for the group. The detail to keep
straight is when it fires: `go build` does not run `go vet`, and the default vet
subset that `go test` runs does not include this analyzer either, so the check
only catches the mistake when you (or CI) invoke `go vet` explicitly. That is why
every exercise here lists `go vet ./...` as a separate verification step rather
than assuming a plain build would surface the problem.

### WaitGroup bounds nothing by itself

A `WaitGroup` waits for tasks; it does not limit how many run at once. Launching
one goroutine per item with `wg.Go` will start all of them. To bound parallelism —
the realistic requirement when each task opens a connection or uses memory —
combine it with a counting semaphore (a buffered channel): acquire a slot before
`wg.Go`, release it when the task finishes. That is the pattern `Map` uses.

### Nested Go: fanning out a tree

A goroutine started by `wg.Go` may itself call `wg.Go` on the same group. This is
explicitly allowed as long as the group is non-empty at the time (it is: the
parent task is still counted), and it is exactly how you fan out a recursive
structure — a directory tree, a DOM, a dependency graph. Each node's task spawns
a task per child on the same `WaitGroup`, and the single `Wait` at the top
returns only after the entire tree has been processed.

There is one hazard to internalize, and it is a genuine deadlock, not a slowdown.
Do *not* combine nested `Go` with a bounded semaphore that a parent acquires and
then holds while it waits for its own children to run. That is the textbook
hold-and-wait condition: every slot in the semaphore ends up held by a parent
task that is blocked waiting for a child, and the children can never acquire a
slot to make progress because no parent will release one until its child
finishes — which it cannot start. The cycle never breaks and the program hangs.
The `SumTree` exercise fans out without a semaphore for exactly this reason: each
node's task spawns its children's tasks and returns immediately rather than
holding anything while it waits, so there is no resource for a parent to starve a
child of.

### The happens-before guarantee

The memory model gives `WaitGroup.Go` a precise guarantee: the return of each `f`
synchronizes before the return of the `Wait` it unblocks. That is what makes the
result-collection pattern safe without a lock — `Map`'s goroutines each write a
distinct `results[i]`, and because every write happens before `Wait` returns,
the reads after `Wait` see them all. The same reasoning lets `SumTree` read its
atomic total after `Wait` with every contribution already visible.

### When to reach for errgroup instead

`WaitGroup.Go` is the stdlib primitive with no error handling and, deliberately,
no cancellation: the `Map` built here runs every task to completion even after one
fails and simply reports the first error by index — there is no `context` to abort
the rest. For fan-out that needs first-error propagation *and* cancellation of the
still-running tasks, `golang.org/x/sync/errgroup` is usually the better tool: its
`Group.Go` collects an error and a context derived from the group cancels the rest
on the first failure. Reach for `WaitGroup.Go` when you own results and error
handling and do not need to cancel; reach for `errgroup` when first-error-cancels
is the contract.

## Common Mistakes

### Keeping the manual Add/Done with Go

Wrong: `wg.Add(1); wg.Go(func(){ defer wg.Done(); ... })`. `Go` already does the
`Add` and the `Done`, so this double-counts and `Wait` blocks forever.

Fix: call only `wg.Go(f)`; do not touch `Add`/`Done` for that task.

### Expecting Go to recover a panic

Wrong: assuming a panic inside `f` is contained. It crashes the program.

Fix: return errors from `f` and aggregate them; recover explicitly inside `f` only
if you have a real reason to.

### Thinking WaitGroup limits parallelism

Wrong: `for _, x := range items { wg.Go(work) }` to "throttle" work. This starts
every task at once.

Fix: gate launches with a buffered-channel semaphore, as `Map` does, or use a
worker pool.

### Reusing one WaitGroup across overlapping batches

Wrong: calling `wg.Go` for a new batch before the previous `Wait` returned.

Fix: a reused `WaitGroup` requires new `Go` calls to happen after all prior
`Wait` calls return; use a fresh `WaitGroup` per batch when in doubt.

Next: [01-bounded-parallel-map.md](01-bounded-parallel-map.md)
