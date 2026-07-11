# Designing Iterator APIs — Concepts

An iterator API is a public contract. Once a package exposes a method that returns an `iter.Seq` or `iter.Seq2`, every caller's `for ... range` loop depends on its name, its shape, and its lifetime. The Go 1.23 iterator design (the `iter` package, range-over-func, and the new `slices`/`maps` helpers) settled a small vocabulary that the whole ecosystem now shares: `All`, `Values`, `Keys`, `Backward`, and the rule that an eager snapshot returns a slice rather than a sequence. This file is the conceptual foundation for the exercises, which build three independent collections that follow those conventions to the letter: an insertion-ordered list, an insertion-ordered map, and a fallible line reader that streams `(value, error)` pairs. Read it once and you will be able to reason through every naming and shape decision the exercises make.

## The two iterator types and what their shape means

There are exactly two function types to design against. `iter.Seq[V]` is `func(yield func(V) bool)`: a one-value-at-a-time sequence. `iter.Seq2[K, V]` is `func(yield func(K, V) bool)`: a two-value sequence, "most commonly key-value or index-value pairs" in the standard library's own words. Picking between them is the first API decision, and it is a semantic one, not a convenience one. `Seq2` is not "the version with more data"; it is the version whose first element is a stable, meaningful coordinate — an index into a list, a key in a map, a sequence number. If the second value carries no companion coordinate, `Seq` is the honest choice, and forcing an artificial index onto it only invites callers to write `for _, v := range` and discard half of every pair.

The `yield` function returns a `bool`, and that boolean is the entire early-termination protocol. Every iterator you write must check it: `if !yield(v) { return }`. A `break`, a `return`, or a `panic` inside the caller's loop body causes the runtime to make the next `yield` call return `false`, and an iterator that ignores the result keeps producing values into a loop that has already stopped — at best wasted work, at worst a deadlock against a closed channel or a write to a torn-down resource. The `return` after a false `yield` is not optional politeness; it is the contract.

## Naming the sequence, not the mechanism

The standard library fixed a vocabulary, and the value of a convention is entirely in following it. The `iter` package documentation and the "Range Over Function Types" blog post lay out the names:

- `All` returns every element. For an indexed collection it is `iter.Seq2[int, E]` (index, element), exactly like `slices.All`. For a keyed collection it is `iter.Seq2[K, V]` (key, value), exactly like `maps.All`. The first component is the coordinate; the second is the element.
- `Values` returns the elements alone as `iter.Seq[V]` — `slices.Values`, `maps.Values`.
- `Keys` returns the keys alone as `iter.Seq[K]` — `maps.Keys`.
- `Backward` returns the elements in reverse with their coordinates as `iter.Seq2[int, E]` — `slices.Backward`. It is `Seq2`, not `Seq`: reverse iteration still wants the index.

A method named `All` lets a caller write `for i, v := range list.All()` without reading the implementation or remembering a package-specific verb. The blog post is explicit that container types should provide an `All` method precisely so programmers never have to recall whether to range over a value directly or call something first — they can always call `All`. The anti-pattern is a single method named `Iter()` or `Each()`: the name describes the mechanism (iteration) instead of the sequence (everything), so the caller must read the body to learn whether it yields values, pairs, or keys, and in what order.

A subtle corollary: a method's name should match its shape across the whole package family. If `All` on your list is `Seq2[int, E]`, then `All` on your map should be `Seq2[K, V]` and never `Seq[V]`. Consistency with `slices` and `maps` is the point; a caller who has internalized the standard library should be able to predict your signatures.

## When to return a slice instead of a sequence

Not every "give me the elements" method should return an iterator. `iter.Seq` is lazy and allocation-light: it computes each value on demand and never materializes the whole collection, which is exactly right for a streaming pass, a `for range` that may break early, or a pipeline of `filter`/`map` stages. But laziness is a cost when the caller needs random access, a length, repeated passes, or a sorted order, because each of those forces a full materialization anyway — and an iterator that the caller immediately dumps into a slice is just a slower `[]T` with extra indirection.

The rule the standard library models: return `iter.Seq` when the work is naturally lazy and per-element; return `[]T` when the operation is inherently eager. Sorting is the canonical eager operation — `slices.Sorted(seq)` consumes an entire `iter.Seq` and returns a fully materialized, sorted `[]E`, because you cannot sort without seeing every element first. A `Sorted` method that returned an `iter.Seq` would be dishonest: it would have allocated and ordered the whole slice internally, then handed back a lazy wrapper that hides the fact that the work is already done and the memory already spent.

There is a second, easy-to-miss reason `Sorted` is a package function rather than a method in the exercises. A method cannot add a type constraint to its receiver's type parameter: if `List[E any]` is generic over any element, a method `func (l *List[E]) Sorted() []E` has no way to require `E` to be `cmp.Ordered`, which sorting needs. The standard library answers this with free functions — `slices.Sorted[E cmp.Ordered]` is a function, not a method — and the list exercise follows the same shape: `Sorted` lives as `func Sorted[E cmp.Ordered](l *List[E]) []E`. The constraint that the operation needs but the type does not have belongs on a function, not a method.

## Single-use versus reusable iterators

Lifetime is part of the contract and must be documented. The `iter` package distinguishes two kinds. A reusable iterator can be ranged any number of times and yields the same sequence each pass — the list and map exercises return these, because each call to `All` or `Values` produces a fresh closure that re-reads a slice or map the collection still owns. A single-use iterator can be walked only once, because walking it consumes an underlying resource — a network connection, a `bufio.Scanner` over an open file, a channel. The fallible `Lines` iterator in the third exercise is single-use in spirit per range: each `for ... range Lines(fsys, path)` opens the file again, scans it once, and closes it, so a given range pass cannot be replayed without re-invoking `Lines`.

The practical obligation is to say which kind you are, in the doc comment, because the two demand opposite caller habits. A caller who assumes reuse will silently get an empty second pass from a single-use iterator; a caller who assumes single-use will defensively re-fetch a reusable one and pay for nothing. When in doubt, prefer reusable iterators returned from a method on a value that owns its data, and reserve single-use for iterators that genuinely wrap a consumable.

## Carrying errors through a sequence

A sequence that can fail mid-iteration cannot signal the failure with a return value — it has already returned the iterator — and it must not silently swallow the error and yield a truncated-but-innocent-looking sequence. The convention the standard library endorses is to use `iter.Seq2[V, error]`: each step yields `(value, nil)` on success, and on failure yields `(zero, err)` exactly once and then stops. The caller's loop becomes `for v, err := range seq { if err != nil { ... break } ... }`, which puts the error handling in the one place the data is consumed and lets the caller stop immediately on the first failure.

Three properties make this pattern safe. First, after yielding an error the iterator must `return` rather than continue, so an error is terminal for that pass — no value follows a non-nil error. Second, the error should be wrapped with `%w` against a sentinel (`fmt.Errorf("open %s: %w", path, err)`) so callers can use `errors.Is` to classify it without string matching. Third, an eager helper that drains the sequence — `Collect(seq) ([]V, error)` — should stop and return at the first error, mirroring the loop a careful caller would write by hand. The mistake to avoid is logging the error inside the iterator and yielding partial data as if it were complete: that turns a recoverable, classifiable failure into silent data loss at the call site.

## Common Mistakes

### Naming every iterator `Iter` or `Each`

Wrong: exposing a single `Iter()` (or `Each()`) method and making callers read the body to learn whether it yields values, index-value pairs, or keys. The name describes the mechanism, not the sequence.

Fix: name the sequence with the shared vocabulary — `All` for every element with its coordinate, `Values` for elements alone, `Keys` for keys, `Backward` for reverse — so a caller who knows `slices` and `maps` can predict the signature.

### Using `Seq2` with a meaningless first value

Wrong: returning `iter.Seq2[int, V]` from a method whose first component is just a running counter with no relationship to how the collection is addressed, forcing every caller to write `for _, v := range` and throw the index away.

Fix: return `iter.Seq[V]` when there is no stable coordinate to pair with the value. Reserve `Seq2` for genuine index-value (`slices.All`) or key-value (`maps.All`) pairs.

### Returning an iterator for inherently eager work

Wrong: a `Sorted` method that returns `iter.Seq[E]` after internally allocating and ordering the entire collection, hiding the fact that the eager work and the allocation already happened.

Fix: return `[]E` when the operation must see every element anyway — sorting, length, random access. Let lazy `iter.Seq` mean genuinely lazy, per-element work.

### Ignoring the `yield` return value

Wrong: looping `for _, v := range src { yield(v) }` without checking the boolean, so the iterator keeps producing after the caller has broken out of its loop.

Fix: write `if !yield(v) { return }` on every yield. The boolean is the early-termination signal; honoring it is the contract, not an optimization.

### Swallowing a mid-iteration error

Wrong: catching a read error inside the iterator, logging it, and continuing to yield, so the caller's loop ends normally and treats a truncated sequence as complete.

Fix: yield `(zero, err)` once via `iter.Seq2[V, error]`, wrap with `%w` against a sentinel, and `return` immediately so the error is terminal and classifiable with `errors.Is`.

---

Next: [01-collection-iterators.md](01-collection-iterators.md)
