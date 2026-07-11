# 8. Standard Library Iterators — Concepts

Go 1.23 turned the range loop into an extension point: a function with the
right shape can now be ranged over directly. Go 1.23 and 1.24 then shipped a
wave of standard-library functions built on that shape, so that the everyday
work of walking a slice, enumerating a map, splitting a string, sorting, and
collecting no longer needs hand-written loops. This file is the conceptual
foundation for the exercises. Read it once and you will understand the iterator
types every standard function speaks, which `slices`, `maps`, and `strings`
functions produce iterators and which consume them, and the one rule that
governs every map-based iterator: its order is unspecified, so determinism is
something you add on purpose. Each exercise then builds a small, self-contained
program against these functions as an independent Go module.

## Concepts

### The Two Iterator Types Everything Shares

A range-over-function iterator is just a function. There are exactly two shapes
the language understands. A single-value iterator has type
`iter.Seq[V] = func(yield func(V) bool)`, and a two-value iterator has type
`iter.Seq2[K, V] = func(yield func(K, V) bool)`. When you write
`for v := range seq`, the compiler calls `seq` and hands it a `yield` function;
each time the producer calls `yield(v)`, your loop body runs once. The `bool`
that `yield` returns is the back-channel: it is `false` when the loop body did a
`break`, `return`, or otherwise stopped early, and a well-behaved producer must
notice that and stop pushing values.

The reason this matters for the standard library is that every iterator-aware
function in `slices`, `maps`, and `strings` is typed in terms of these two
aliases and nothing else. `slices.Values` returns an `iter.Seq[E]`;
`maps.All` returns an `iter.Seq2[K, V]`; `strings.Lines` returns an
`iter.Seq[string]`. Because they all share the same two types, a value produced
by one package flows without adaptation into a consumer in another, and a custom
iterator you write yourself is a first-class peer of the standard ones. The
whole composability story rests on this single shared vocabulary.

### Producers, Consumers, and Adapters

It helps to sort the functions into three roles. Producers turn an existing
container or string into an iterator: `slices.Values` (each element),
`slices.All` (index and element), `slices.Backward` (index and element, last to
first), `maps.Keys`, `maps.Values`, `maps.All`, and the `strings` family
`strings.Lines`, `strings.SplitSeq`, `strings.SplitAfterSeq`,
`strings.FieldsSeq`, and `strings.FieldsFuncSeq`. Consumers run an iterator to
exhaustion and return a concrete value: `slices.Collect` materializes an
`iter.Seq[E]` into a `[]E`, `slices.Sorted` collects and sorts in ascending
order, `slices.SortedFunc` and `slices.SortedStableFunc` collect and sort by a
comparison function, and `maps.Collect` builds a map from an `iter.Seq2`.
Adapters sit in the middle: they take an iterator and return a new iterator,
which is where your own `Filter` and `Map` live, and where `slices.Chunk` sits
as a standard adapter that regroups a slice into a sequence of sub-slices.

The shape of a real program is usually producer to (zero or more) adapters to
consumer. `slices.Collect(strings.SplitSeq(line, ","))` is producer then
consumer with no adapter. `slices.Sorted(maps.Keys(m))` is producer then
consumer. A filtered, mapped, then collected slice is producer, two adapters,
consumer. Naming the three roles is what makes an unfamiliar pipeline readable
at a glance.

### Laziness: Nothing Runs Until You Range

A producer like `slices.Values(nums)` does no work when you call it. It returns
a function. The elements are only walked when something ranges over that
function, and a chain of adapters is fused into a single pass: filtering then
mapping then collecting does not build an intermediate slice between each stage,
it pulls one element all the way through the chain before pulling the next. This
is why an adapter is cheap to add and why a consumer is where the cost lives.
The practical consequence is that a pipeline allocates only at its consumer:
`slices.Collect` allocates the result slice; the `Filter` and `Map` stages in
front of it allocate nothing. It also means an early `break` in the consumer
stops the producer immediately, so `for x := range filtered { break }` reads at
most the elements needed to yield the first one.

### Map Iteration Order Is Unspecified, Always

Go deliberately randomizes map iteration order, and the map iterators inherit
that exactly. `maps.Keys(m)`, `maps.Values(m)`, and `maps.All(m)` yield in an
order that can differ on every run of the program; relying on it is a bug the
runtime works to expose. This is not a limitation to route around so much as the
single most important fact about these functions. Any time a map feeds output
that a human reads, a test asserts on, or another machine parses, you must
impose an order yourself.

The canonical fix is a one-liner: `slices.Sorted(maps.Keys(m))` produces the
keys in ascending order, and you then read the map in that key order. The same
shape, `slices.SortedFunc(maps.Keys(m), cmp)`, gives you any other total order.
This composition, a map producer feeding a sorting consumer, is the standard
idiom for deterministic map output and recurs throughout the exercises. When the
values rather than the keys need ordering, `slices.Sorted(maps.Values(m))`
works the same way, though note that distinct keys can share a value, so sorting
values alone loses the association.

### The `strings` Iterators and the Newline Detail

The `strings` package gained iterator twins of its splitting functions so that
scanning text no longer forces an intermediate `[]string`. `strings.SplitSeq`
is the lazy form of `strings.Split`, `strings.FieldsSeq` of `strings.Fields`,
and `strings.Lines` walks a string one line at a time. They matter most for
large inputs, where the slice form would allocate one big array of substrings up
front while the iterator form yields one substring at a time and lets an early
`break` skip the rest.

One behavioral detail trips up everyone the first time: `strings.Lines` yields
each line including its trailing newline. Ranging over `"a\nb\n"` produces
`"a\n"` then `"b\n"`, not `"a"` then `"b"`. If the string does not end in a
newline, the final line is yielded without one, so `"a\nb"` produces `"a\n"`
then `"b"`. Code that compares a yielded line against a newline-free constant, or
that builds a key from it, must trim the newline first (`strings.TrimRight(line,
"\n")` or `strings.TrimSuffix`). `strings.FieldsSeq`, by contrast, splits around
runs of whitespace and yields no empty and no newline-bearing fields, which is
why word-counting is cleanest as `strings.Lines` to get lines, then
`strings.FieldsSeq` within each line.

### Why Prefer the Standard Functions Over Hand-Written Loops

The argument is not merely brevity. A hand-written sort-the-keys-then-loop is
four or five lines that restate intent the reader must decode;
`slices.Sorted(maps.Keys(m))` states it in one. More importantly, the standard
functions are correct by construction for the cases that hand loops get wrong:
`slices.SortedFunc` is documented and tested against the comparison contract,
`slices.Collect` grows the result slice with the same amortized strategy as
`append` without you writing the capacity hint, and the `strings` iterators
handle the empty-string and trailing-separator edge cases that a naive split
loop mishandles. The custom code you keep is then only the part that is genuinely
yours: the domain predicate in a `Filter`, the transform in a `Map`, the
comparison key. That is the right division of labor, and it is the throughline of
every exercise here.

## Common Mistakes

### Asserting on Raw Map Iterator Order

Wrong: comparing the direct output of `maps.Keys(m)` or a loop over `maps.All(m)`
against a fixed sequence in a test or in user-facing output.

What happens: the test passes on some runs and fails on others because the
runtime randomizes map order, and the failure looks nondeterministic and
maddening to debug.

Fix: pass the keys (or values) through `slices.Sorted` or `slices.SortedFunc`
before asserting or displaying. Determinism over a map is always something you
add explicitly; it is never inherited.

### Forgetting That `strings.Lines` Keeps the Newline

Wrong: `if line == "ERROR"` or `counts[line]++` directly on a value yielded by
`strings.Lines`, expecting the bare line text.

What happens: the comparison never matches and the map keys carry stray `\n`
bytes, because each yielded line still has its terminating newline (except a
final line with no newline in the source).

Fix: trim first with `strings.TrimRight(line, "\n")` or `strings.TrimSuffix`,
or use `strings.FieldsSeq` when you want whitespace-delimited tokens with no
newline.

### Reimplementing Collect, Sort, and Split by Hand

Wrong: writing a manual `out = append(out, v)` loop to drain an iterator, a
`sort.Slice` over collected keys, or a hand-rolled split loop with index
arithmetic.

What happens: the code works but is longer, restates intent the reader must
re-derive, and is more likely to mishandle an empty input or a trailing
separator than the standard function it replaces.

Fix: reach for `slices.Collect`, `slices.Sorted`/`slices.SortedFunc`, and
`strings.SplitSeq`/`strings.FieldsSeq`. Keep only the domain-specific predicate,
transform, or comparison as your own code.

### Aliasing the Sub-Slices from `slices.Chunk`

Wrong: storing the chunk yielded by `slices.Chunk` directly into a longer-lived
slice of slices, then continuing to use the original backing array.

What happens: each yielded chunk is a window into the input's backing array, not
a fresh copy. Retaining the window keeps the whole input alive and exposes the
stored chunk to later mutation of the source.

Fix: copy a chunk with `slices.Clone` before retaining it past the loop
iteration that produced it.

---

Next: [01-deterministic-map-iteration.md](01-deterministic-map-iteration.md)
