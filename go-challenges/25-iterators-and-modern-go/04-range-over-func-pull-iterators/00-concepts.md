# 4. Range Over Func — Pull Iterators — Concepts

Go's range-over-function iterators come in two shapes. The one you write most often is a *push* iterator: a function that takes a `yield` callback and drives the loop itself, calling `yield` once per element. A `for range` loop consumes it, and control lives inside the producer. The other shape is a *pull* iterator: the consumer asks for the next value by calling a function, one value at a time, and control lives on the consumer side. The standard library bridges the two with `iter.Pull` and `iter.Pull2`. This file is the conceptual foundation for the exercises, which build a bounded manual-pull helper, lockstep zip and merge, a sorted merge-join over key/value sequences with `iter.Pull2`, and a peekable one-token-lookahead view. Read it once and you will have the model you need to reason through every module.

## Concepts

### Push iteration: the default, and where it runs out

A push iterator has type `iter.Seq[V]`, which is `func(yield func(V) bool)`. The producer loops, hands each value to `yield`, and stops if `yield` returns `false` (the loop broke or returned early). The two-value form is `iter.Seq2[K, V]`, `func(yield func(K, V) bool)`. This is the model `for v := range seq` and `for k, v := range seq2` consume directly, and it is the right default: it is cheap, it is what every standard-library iterator returns, and the producer's own control flow (a `for` over a slice, a tree walk, a scanner) maps onto it naturally.

The push model has one structural limit: the producer owns the loop, so a single push iterator can only drive *its own* control flow. The moment you need to advance two sequences in a coordinated way — take one from A, then decide whether to take from A or B next — neither sequence's `yield`-driven loop can express the decision, because each loop only knows how to walk itself to the end. You need to step each sequence on demand. That is what pull iteration provides.

### `iter.Pull` and `iter.Pull2`: the exact signatures

`iter.Pull` turns a push iterator into a pull iterator:

```
func Pull[V any](seq Seq[V]) (next func() (V, bool), stop func())
```

It returns two functions. `next` returns the next value and a boolean: `(v, true)` while values remain, and `(zero, false)` once the sequence is exhausted (and on every call thereafter). `stop` ends the iteration early and releases the resources the pull machinery holds.

`iter.Pull2` is the same for two-value sequences:

```
func Pull2[K, V any](seq Seq2[K, V]) (next func() (K, V, bool), stop func())
```

Here `next` returns `(k, v, true)` or `(zeroK, zeroV, false)`. Everything else is identical. The exercises use `iter.Pull` for single-value sequences (zip, merge, peek) and `iter.Pull2` for the key/value merge-join.

### How the bridge works, and why `stop` exists

A push iterator wants to run a loop to completion; a pull consumer wants to extract values one at a time and possibly stop in the middle. Reconciling those two control flows requires running the producer and the consumer as coroutines. `iter.Pull` starts the push iterator on a separate goroutine, parked inside `yield`. Each call to `next` resumes that goroutine just long enough to produce one value at the next `yield`, then parks it again and returns the value to the caller. The producer's `for` loop never runs to the end on its own; it advances exactly one `yield` per `next`.

This is why `stop` is not optional. If the consumer stops pulling before the sequence is exhausted — an early `break`, an error, taking only the first three values — the producer goroutine is still parked inside `yield`, waiting to be resumed, and any `defer` it holds (a file close, a mutex unlock, a buffer release) has not run. `stop` resumes that goroutine one last time with `yield` returning `false`, so the producer's loop exits, its deferred cleanup runs, and the goroutine terminates. Skip `stop` and you leak the goroutine and everything it was deferring. The discipline is mechanical: write `defer stop()` on the line immediately after `iter.Pull`, before you touch `next`, so it fires no matter how the consumer leaves.

`stop` is safe to call more than once; later calls do nothing. Calling `next` after `stop` returns the zero value and `false`. The one rule the runtime enforces strictly: `next` and `stop` for a given pull iterator are not safe for concurrent use — call them from a single goroutine at a time, or the runtime panics.

### Where pull earns its cost: lockstep, lookahead, manual stepping

Pull iteration is not free. Every `next` is a goroutine switch plus synchronization, so pulling a sequence value-by-value is meaningfully more expensive than letting a `for range` push it. The rule of thumb: if a plain `for range` expresses what you want, use it. Reach for `iter.Pull` only when the control flow genuinely cannot be a single push loop. Three patterns qualify.

The first is lockstep coordination of two or more sequences. Zipping two sequences into pairs, or merging two sorted sequences into one sorted stream, requires holding a cursor on each input and choosing which to advance. With pull iterators each input has its own `next`, so the coordinating loop reads `(av, okA)` and `(bv, okB)` and decides. A push iterator cannot do this, because its loop only knows how to walk one sequence.

The second is lookahead. A peekable iterator lets the consumer inspect the next value without consuming it — the classic need in a parser or a run-length grouping pass. Lookahead means buffering exactly one pulled value: `Peek` pulls and caches, `Next` returns the cache and clears it. Push has no place to store a held-back value, so peek is a pull-only construct.

The third is manual, bounded stepping: take at most N values, or step under the consumer's explicit control rather than draining the whole sequence in one loop. `iter.Pull` makes "consume k of them, then maybe stop" a direct expression.

### A note on idiom: the standard library leans push

Almost every iterator the standard library hands you is a push iterator (`maps.Keys`, `slices.Values`, `strings.Lines`, and so on), because push is the shape that composes with `for range`. `iter.Pull` is the adapter you apply at the point where a specific algorithm needs pull control; the iterators you publish for others to consume should still be push iterators, so they drop into a `range` loop. Pull is an implementation technique, not an API style.

## Common Mistakes

### Forgetting to call `stop`

Pulling a sequence and leaving without calling `stop` — an early `break`, an error path, a `return` after the first few values — leaves the producer goroutine parked forever and its deferred cleanup unrun. That is a leaked goroutine and possibly a leaked file handle or held lock. The fix is unconditional: `defer stop()` on the line right after `iter.Pull`, so it runs on every exit. The leak is invisible in a quick test that happens to drain the sequence to exhaustion; it shows up under early termination, which is exactly the case pull exists to support.

### Reaching for pull when a `for range` would do

Converting a single sequence to pull style just to print or collect every value adds a goroutine and a synchronization per element for no benefit. If you are draining a sequence start to finish with no coordination, no lookahead, and no early bound, a plain `for range` is simpler and faster. Pull is for the cases push cannot express, not a general replacement.

### Assuming zip or merge pads the shorter sequence

A zip of unequal-length sequences stops when *either* side ends; it does not invent default values to match the longer one. A merge of two sorted sequences drains whichever side has values left after the other is exhausted. Getting these boundary rules wrong is the usual source of off-by-one bugs, which is why the exercises pin each with an explicit unequal-length and empty-side test.

### Calling `next` and `stop` concurrently

The pair returned by `iter.Pull` is single-goroutine state. Calling `next` from two goroutines, or `next` on one and `stop` on another at the same time, is a data race that the runtime detects and panics on. If a pulled sequence must be shared across goroutines, serialize access behind your own lock; do not hand `next` to several goroutines directly.

Next: [01-bridge-push-to-pull.md](01-bridge-push-to-pull.md)
