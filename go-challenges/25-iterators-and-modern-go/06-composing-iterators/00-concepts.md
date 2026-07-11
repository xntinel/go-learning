# 6. Composing Iterators — Concepts

An iterator combinator is a function that takes one sequence and returns another. Go's `iter.Seq[V]` makes this possible because a sequence is just a function — `func(yield func(V) bool)` — so a combinator is a function that wraps another function, transforming each value as it flows through. The power of the design is that combinators compose: `Take(Map(Filter(src, even), square), 5)` reads left to right as a pipeline, every stage is lazy, and no stage ever builds an intermediate slice. The hard parts are subtle and all live in one place: the `bool` that `yield` returns. Honor it everywhere and your pipeline short-circuits the instant a consumer stops; ignore it in even one combinator and the whole pipeline keeps doing work nobody asked for, or worse, panics. This file is the conceptual foundation for the exercises, which build a small combinator library piece by piece as independent, self-contained Go modules.

## Concepts

### Combinators Preserve The Iterator Shape

`Filter` accepts an `iter.Seq[V]` and returns an `iter.Seq[V]`. `Map` accepts an `iter.Seq[A]` and returns an `iter.Seq[B]`. Because the output of one combinator is a valid input to the next, they nest without any glue code and without ever materializing the values in between. The returned `iter.Seq` is a closure that has captured its upstream sequence and its transform; it does no work at all until something ranges over it. This is the single most important property to internalize: constructing a pipeline is free, and the cost is paid only when a consumer pulls values through it.

A combinator that returns `iter.Seq[V]` is *intermediate*: it describes a new lazy sequence. A function that returns a plain value — a sum, a count, a slice — is *terminal*: it drives the pipeline to completion (or until it breaks) and produces a result. `Reduce` is terminal; `slices.Collect` from the standard library is terminal. You can chain any number of intermediate combinators, but a terminal operation ends the chain and consumes one pass over the sequence.

### Laziness Is The `yield` Return Value

A push iterator hands each value to a `yield` function. That function returns a `bool`: `true` means "keep going," `false` means "I am done, stop." The entire laziness contract reduces to one rule that every combinator must obey: when the downstream `yield` returns `false`, stop pulling from upstream and return immediately. The canonical loop body is

```go
for v := range seq {
	if !yield(transform(v)) {
		return
	}
}
```

The `return` is not optional cleanup; it is the mechanism by which a `break` deep in a consumer's `for range` loop propagates all the way back up the pipeline to the original source. When a consumer breaks, the runtime makes the consumer's `yield` return `false`. That `false` travels up through each combinator's `if !yield(...) { return }`, each `return` ending that combinator's loop, until the source's own `if !yield(...) { return }` fires and the source stops producing. Drop the check in one combinator and that combinator keeps draining its upstream after the consumer has already left — the definition of a leaky, non-lazy pipeline.

### Yielding After Stop Is A Runtime Panic

The contract has teeth. Once `yield` has returned `false`, calling it again is a programming error, and the Go runtime detects it: the range-over-func machinery panics with "range function continued iteration after function for loop body returned false." This is deliberate. It converts the silent bug of a combinator that ignores the stop signal into a loud crash. The defense is always the same — `return` the instant `yield` returns `false` — and it is why the `if !yield(...) { return }` idiom appears in every single combinator rather than a bare `yield(...)`.

### Take And The "One Extra Pull" Subtlety

`Take(seq, n)` should yield at most the first `n` values and, to be maximally lazy, pull no more than `n` values from upstream. The naive structure pulls one too many:

```go
for v := range seq {
	if count >= n {
		return // count checked AFTER the value was already pulled
	}
	count++
	yield(v)
}
```

The `for v := range seq` pulls the next value *before* the body can decide to stop, so this version pulls `n+1` values to yield `n`. The fix is to check the count right after a successful yield and return before the loop pulls again, with a guard for the `n == 0` case so it yields nothing:

```go
if n == 0 {
	return
}
count := 0
for v := range seq {
	if !yield(v) {
		return
	}
	count++
	if count == n {
		return
	}
}
```

That extra pull is harmless for a slice but matters when upstream is expensive (a network call) or infinite (a generator of all integers). Measuring the exact number of upstream pulls is the sharpest test of whether a combinator is truly lazy, which is why the exercises count pulls rather than just checking the output values.

### Driving Two Sequences At Once Needs `iter.Pull`

A single `for range` loop can drive exactly one push sequence. `Zip`, which advances two sequences in lockstep and yields pairs, cannot be written with two nested `for range` loops — they would run one to completion before the other started. The standard-library answer is `iter.Pull`, which converts a push sequence into a pull sequence: it returns a `next func() (V, bool)` you call on demand and a `stop func()` you must call to release it. With one sequence driven by `for range` and the other pulled by hand, `Zip` advances them together and stops as soon as either runs dry:

```go
next, stop := iter.Pull(b)
defer stop()
for av := range a {
	bv, ok := next()
	if !ok {
		return
	}
	if !yield(av, bv) {
		return
	}
}
```

`iter.Pull` spins up a goroutine to suspend the pushed sequence between calls, so `stop` is mandatory — `defer stop()` guarantees the goroutine is torn down whether `a` is exhausted, `b` runs out, or the consumer breaks. `Zip` returns an `iter.Seq2[A, B]` because each step produces two values; ranging over it binds two variables, `for a, b := range Zip(xs, ys)`.

### Flatten And Nested Stop Propagation

`Flatten` turns an `iter.Seq[iter.Seq[V]]` — a sequence of sequences — into a flat `iter.Seq[V]`. Its body is two nested `for range` loops, and the only subtlety is, again, the stop signal. A `return` inside the inner loop exits the whole combinator function, and the runtime correctly tears down both the inner and the outer range, so a consumer that breaks halfway through the second sub-sequence causes every later sub-sequence to never start. This is what "no work past an early break" means concretely: the laziness of `Flatten` is exactly that the third sub-sequence is never even constructed if the consumer stopped during the second.

## Common Mistakes

### Draining Upstream After Downstream Stops

Wrong: a combinator calls `yield(v)` and ignores the result, looping on. After the consumer breaks, this combinator keeps pulling from its upstream — and the next `yield` call panics with "continued iteration after ... returned false." Fix: write `if !yield(v) { return }`, never a bare `yield(v)`. The `return` both stops this combinator and propagates the stop upstream.

### Materializing Every Stage

Wrong: each combinator collects its input into a slice, transforms the slice, and returns a sequence over the result. This makes the pipeline eager — it does all the work up front — and allocates an intermediate slice per stage, defeating both the laziness and the zero-allocation benefits of `iter.Seq`. Fix: return a closure that transforms one value at a time as it flows through; allocate nothing but the closure itself.

### Treating `Reduce` Like A Reusable Iterator

Wrong: expecting `Reduce` (or `slices.Collect`, or any terminal) to return something you can range over again. A terminal operation consumes one pass over the sequence and returns a value. Fix: keep terminals at the end of a chain; if you need both a sum and the values, collect once into a slice and reuse the slice, not the sequence.

### Pulling Without Stopping

Wrong: calling `iter.Pull` and forgetting `stop()`. The pull machinery holds a suspended goroutine; never calling `stop` leaks it. Fix: `defer stop()` immediately after `iter.Pull`, so every exit path — exhaustion, the other sequence ending, or a consumer break — releases it.

### Ranging A Single-Use Sequence Twice

Wrong: assuming any `iter.Seq` can be re-ranged. A sequence backed by a slice or a counter can, but one backed by a network stream, a channel, or an `iter.Pull` cursor is consumed by its first traversal and yields nothing on a second. Fix: do not assume re-iterability; if a value is needed more than once, collect it into a slice first.

---

Next: [01-core-combinators.md](01-core-combinators.md)
