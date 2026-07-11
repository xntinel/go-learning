# 7. The iter Package — Concepts

The `iter` package is one of the smallest packages in the standard library and one of the most load-bearing. It defines exactly two named types and two functions, yet those four names are the vocabulary that the whole post-1.23 iterator ecosystem speaks. `slices.Values`, `maps.Keys`, `bufio.Scanner.Lines`, `strings.Lines`, your own data structure's `All` method — every one of them produces or consumes an `iter.Seq`. This file is the conceptual foundation for the exercises: read it once and you will understand what the four names mean, why pull iterators need an explicit `stop`, and how a push iterator and a pull iterator convert into each other so that operations like merge and zip become straightforward to write.

## What the iter Package Actually Defines

The entire public surface of the package is four declarations. The two types are simply named function types:

- `iter.Seq[V any]` is `func(yield func(V) bool)`.
- `iter.Seq2[K, V any]` is `func(yield func(K, V) bool)`.

The two functions are the pull adapters:

- `iter.Pull[V any](seq Seq[V]) (next func() (V, bool), stop func())`.
- `iter.Pull2[K, V any](seq Seq2[K, V]) (next func() (K, V, bool), stop func())`.

That is the whole package. Everything else in this lesson is built from those four names plus the standard `slices` and `maps` helpers that traffic in them. The value of the named types is not that they add behavior over the raw `func(func(V) bool)` literal — they are identical to the compiler — but that they document intent. A function returning `iter.Seq[int]` announces "this is a standard iterator, range over it"; a function returning the bare `func(func(int) bool)` makes a reader stop and decode the shape by hand. Use the named types in every public signature for the same reason you write `io.Reader` instead of an anonymous interface.

## The Push Model: Seq, Seq2, and the yield Protocol

An `iter.Seq[V]` is a *push* iterator. You do not ask it for the next value; you hand it a `yield` callback and it drives the loop itself, calling `yield(v)` once per element. This inversion is what lets `for v := range seq` work: the compiler synthesizes the `yield` function from your loop body, and the iterator pushes values into it.

The single subtle rule is the boolean that `yield` returns. `yield(v)` returns `true` to mean "keep going" and `false` to mean "the consumer is done, stop now." Every correct push iterator checks that return value and stops the moment it sees `false`:

```text
for n := 2; n < limit; n++ {
    if isPrime(n) && !yield(n) {
        return        // consumer broke out of its range loop; honor it
    }
}
```

The `false` return is how `break`, an early `return`, or a `panic` inside the range loop reaches the iterator. If your iterator ignores the result and keeps calling `yield` after it returned `false`, the runtime panics with "range function continued iteration after function for loop body returned false." Treating the `false` return as a hard stop is therefore not an optimization, it is a correctness requirement. `Seq2` is the same protocol with a two-argument `yield(k, v)`; it is what `maps.All`, `slices.All`, and any index-or-key iterator return.

## The Pull Model: Why Pull Exists and Why stop Is Mandatory

The push model is ideal for consumption with `range`, but it is the wrong shape for any algorithm that must advance two iterators in lockstep, peek one element ahead, or interleave inputs by comparing their heads. You cannot drive two push iterators against each other inside a single `range` — each one wants to own the loop. `iter.Pull` solves this by inverting the inversion. It takes a push `Seq[V]` and hands back two functions: `next()`, which returns the next `(value, ok)` pair on demand, and `stop()`, which ends the iteration early.

The mechanism behind `Pull` is a coroutine. The pulled `Seq` runs on a separate goroutine that is parked at each `yield` call; every `next()` unparks it just long enough to produce one value and re-park it. This is why `stop` is not optional. If you pull an iterator, take a few values, and walk away without calling `stop`, the underlying goroutine stays parked forever and any resources the source holds — an open file, a database cursor, a network connection — are never released. The discipline is one line and it is unconditional: `defer stop()` on the line immediately after the `iter.Pull` call, before the first `next()`. `stop` is also idempotent and safe to call more than once, which is exactly what makes the bare `defer` safe even on the success path where the iterator drained naturally.

Calling `stop()` does more than free the goroutine: it resumes the parked `Seq` one final time with `yield` returning `false`, so the source runs its own deferred cleanup (closing that file, releasing that cursor) as part of being stopped. A pull consumer that forgets `stop` therefore also skips the source's cleanup, which is the second, quieter half of the leak.

## Converting Push to Pull and Back Again

The most important compositional pattern in this lesson is the round trip: take one or more push `Seq` values, convert them to pull form to do bookkeeping that needs random lookahead, and wrap the whole thing back up as a single push `Seq` so the caller can `range` over the result like any other iterator. Merging two already-sorted sequences is the canonical example. The merge needs to look at the current head of each input and emit the smaller one, which is precisely the lookahead that the push model forbids and the pull model grants.

The shape is always the same. The outer function returns an `iter.Seq[V]` — a closure that takes `yield`. Inside that closure it calls `iter.Pull` on each input, `defer`s each `stop`, primes the heads with an initial `next()`, then loops: compare the heads, `yield` the winner, advance only that side with another `next()`. Because the body lives inside a returned `Seq`, the result is a first-class push iterator: lazy (nothing runs until someone ranges over it), composable (it can be the input to another merge), and correctly terminating (when the consumer breaks, `yield` returns `false`, the closure returns, and both deferred `stop` calls fire). This push → pull → push sandwich is how nearly every non-trivial iterator combinator — merge, zip, dedup, windowing — is built.

## The Standard Library Speaks Iterator

The reason these four names matter is that the standard library adopted them everywhere, so custom iterators and standard helpers compose without glue. The `slices` package provides both producers and consumers: `slices.Values(s)` turns a slice into an `iter.Seq[V]`, `slices.All(s)` into an `iter.Seq2[int, V]`, and `slices.Backward(s)` into a reverse `iter.Seq2[int, V]`; on the consuming side, `slices.Collect(seq)` drains an `iter.Seq` into a fresh slice and `slices.Sorted(seq)` drains and sorts it. The `maps` package mirrors this with `maps.Keys(m)`, `maps.Values(m)`, and `maps.All(m)` as producers. The idiomatic way to get a map's keys in sorted order is the composition `slices.Sorted(maps.Keys(m))`: `maps.Keys` produces an unordered `iter.Seq[K]`, and `slices.Sorted` collects and sorts it in one call. That one line replaces the old three-step ritual of allocating a slice, appending every key in a `for range`, and calling `sort.Strings`.

## Common Mistakes

The first mistake is exposing raw function types in public signatures. A field or return value typed `func(func(T) bool)` works but tells a reader nothing; `iter.Seq[T]` and `iter.Seq2[K, V]` make the intent and the `range`-ability obvious to both humans and tooling. Always name the type at API boundaries.

The second mistake is forgetting to `stop` a pulled iterator. Every `iter.Pull` and `iter.Pull2` owns a goroutine and, transitively, whatever the source holds open. The fix is mechanical: `defer stop()` on the line right after the pull, never inside a conditional, so that early returns, the natural end of the data, and panics all release it. Returning from a pull-using function on an error path without having deferred `stop` leaks the goroutine silently.

The third mistake is ignoring the boolean that `yield` returns inside a custom push iterator. Continuing to call `yield` after it has returned `false` is a runtime panic, not a no-op. Every loop in a `Seq` body must be written as "if `yield(v)` is false, return now," so that a consumer's `break` actually breaks.

The fourth mistake is assuming map iteration order is stable when you collect keys. Ranging a map directly gives a different order on every run. When order matters, route through an iterator and sort: `slices.Sorted(maps.Keys(m))` is the one-liner, and it is correct precisely because it does not depend on the map's internal traversal order.

---

Next: [01-seq-and-seq2-producers.md](01-seq-and-seq2-producers.md)
