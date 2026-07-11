# Range Over Collections — Concepts

`range` looks like one keyword, but it is really five different loops wearing the
same clothes, plus a sixth shape (Go 1.23) that lets an ordinary function pretend
to be a collection. In production Go, the `range` site is where correctness,
determinism, and goroutine leaks intersect: a value-copy you forgot mutates
nothing, a map you logged in raw order breaks a reproducible build, a channel
nobody closes hangs a consumer forever, and a byte index you mistook for a rune
count corrupts a redaction. This file is the conceptual foundation for the ten
independent exercises that follow; read it once and you have the model you need to
reason through each of them.

## The five shapes of range, and choosing the right one

Go's `for ... range` clause dispatches on the type of the operand. There are five
built-in shapes, and picking the wrong one is a design smell, not a style nit.

Over a slice or array, `for i, v := range xs` binds `i` to the zero-based index
and `v` to a *copy* of the element. The copy is the single most consequential fact
about this shape: writing `v.Field = x` mutates the copy and is silently lost;
mutating the backing array requires indexing, `xs[i].Field = x`. The copy is also
a real cost when the element is a large struct in a hot path.

Over a map, `for k, v := range m` yields each key/value pair exactly once, but in
an order that Go deliberately randomizes per range statement. This is not an
accident to be worked around; it is a guardrail that stops you from writing code
that silently depends on iteration order. Any output that is logged, hashed,
serialized to the wire, or asserted in a test must have an order imposed on it
first.

Over a channel, `for v := range ch` receives values until the channel is closed,
then ends. It blocks on an empty-but-open channel and never terminates on its own;
termination is entirely the producer's responsibility. Ranging a channel that
nobody closes is a permanent goroutine leak.

Over an integer (Go 1.22+), `for i := range n` runs `n` iterations with `i` going
`0..n-1`, and `for range n` runs `n` iterations with no variable at all. This is
the idiomatic counted loop; it replaces `for i := 0; i < n; i++` when you simply
need a count, and it reads at a glance as "do this n times".

Over a string, `for i, r := range s` decodes UTF-8: `i` is the *byte* index where
the rune starts and `r` is the `rune` (a `int32` code point), which is a different
thing from ranging `[]byte(s)`, where you get raw bytes one at a time. `len(s)` is
a byte count, never a rune count. Confusing the two corrupts any multibyte text
you truncate, redact, or do column math on.

## range-over-func: turning a function into a collection (Go 1.23)

Go 1.23 made a function rangeable if it has one of the iterator types from package
`iter`:

```go
type Seq[V any] func(yield func(V) bool)
type Seq2[K, V any] func(yield func(K, V) bool)
```

When you write `for v := range seq`, the compiler calls `seq` with a `yield`
function it generates from your loop body. Each time the iterator calls
`yield(v)`, your loop body runs once. The crucial part is the boolean return:
`yield` returns `false` when the caller did `break`, `return`, `goto`, or
otherwise wants to stop. A correct iterator *must* check that return value and
stop producing — and, just as important, run its cleanup (close a file, cancel a
request, release a connection) on the way out. An iterator that ignores `yield`'s
return keeps fetching pages, keeps reading the network, keeps leaking, even though
the caller has already left the loop.

This is the mechanism that lets you expose a lazy, resource-backed stream —
cursor pagination, an NDJSON decoder, a database cursor — behind an ordinary
`for x := range client.All(ctx)`. The caller gets normal `for`/`break` ergonomics;
the iterator owns the paging, the buffering, and the teardown. It is the single
most senior-flavored addition to the loop vocabulary, because it turns "how do I
iterate this thing" from a bespoke `Next()/Err()` protocol into a language
feature.

### Push vs pull, and why you sometimes need iter.Pull

A range-over-func iterator is a *push* iterator: the iterator is in control and
pushes values into your loop body via `yield`. That is perfect for a single
`for` loop, but it composes poorly when you need to advance two sequences in
lockstep (a merge join) or interleave them. `iter.Pull(seq)` and
`iter.Pull2(seq2)` invert control, returning a `next()` function you call to get
the next value and a `stop()` function you must call to release the iterator's
resources:

```go
next, stop := iter.Pull(seq)
defer stop()
for {
	v, ok := next()
	if !ok {
		break
	}
	// use v
}
```

Forgetting `stop()` leaks the goroutine that `iter.Pull` uses to drive the push
iterator. The rule is symmetric with channels: whoever pulls owns stopping.

## The stdlib iterator adapters (slices, maps — all Go 1.23)

Packages `slices` and `maps` grew a family of adapters that produce and consume
these iterator types, letting you build small pipelines:

- `slices.Values(s) iter.Seq[E]` and `slices.All(s) iter.Seq2[int, E]` view a
  slice as an iterator — lazy, no allocation.
- `slices.Collect(seq) []E` and `maps.Collect(seq2) map[K]V` materialize an
  iterator into a fresh slice/map — these allocate.
- `slices.Sorted(seq) []E` and `slices.SortedFunc(seq, cmp)` collect and sort —
  allocate and sort.
- `slices.Chunk(s, n) iter.Seq[[]E]` yields consecutive sub-slices of length `n`
  (the last may be shorter); it panics if `n < 1`.
- `maps.Keys(m) iter.Seq[K]` and `maps.Values(m) iter.Seq[V]` iterate a map's
  keys/values — lazily and in randomized order, so `slices.Sorted(maps.Keys(m))`
  is the canonical "give me the keys in stable order" idiom.

Knowing which adapters are lazy (`Values`, `All`, `Chunk`, `Keys`) and which
allocate (`Collect`, `Sorted`, `SortedFunc`) is the difference between a clean
pipeline and one that quietly allocates several intermediate slices you did not
intend. `maps.Keys` in particular returns a lazy `iter.Seq[K]`, *not* a slice —
you must `slices.Sorted` or `slices.Collect` it before you can index or re-range
it.

## Map mutation during range, and the concurrency line

Deleting from a map during its own `range` is defined and safe in Go: a key you
delete before the loop reaches it will not be produced. Adding a key during range
is where behavior is intentionally unspecified — the new key may or may not be
produced. So the cache-sweep pattern `for k := range m { if expired(k) { delete(m, k) } }`
is legal and idiomatic; the collect-keys-then-delete variant is equivalent and
sometimes clearer.

The hard line is concurrency. A single goroutine mutating a map it is ranging is
fine. Two goroutines where one ranges (or reads) while another writes is a data
race — the runtime's race detector will flag it, and an unsynchronized concurrent
map access can panic with "concurrent map iteration and map write" even without
`-race`. Guard shared maps with a `sync.Mutex`/`sync.RWMutex`, or use `sync.Map`
for the specialized read-mostly case.

## Channel range and the fan-in close discipline

Because `for v := range ch` ends only on close, a worker-pool fan-in has a
canonical shape: N workers write results into a shared `results` channel, and a
single separate goroutine waits for all workers to finish and then closes it:

```go
go func() {
	wg.Wait()
	close(results)
}()
for r := range results {
	// collect
}
```

The `wg.Wait(); close(results)` closer is what makes the collector's `range`
terminate. Close from the workers directly and you double-close (panic) or close
too early (send on closed channel, panic). The producer side owns the close; the
number of producers is exactly why you need the WaitGroup to serialize it.

## The Go 1.22 loop-variable change

Before Go 1.22, the loop variable in a `for`/`range` was a single variable reused
across iterations, so capturing it in a goroutine or closure aliased the last
value — the infamous `for _, v := range xs { go func() { use(v) }() }` bug. Go
1.22 changed the semantics so each iteration gets a fresh instance of the loop
variable, and the capture now does the obvious thing. You should still write for
clarity and not assume every reader knows which Go version is in play, but the old
`v := v` shadowing dance is no longer necessary and reads as noise in modern code.

## Common Mistakes

### Asserting on or logging raw map iteration order

Wrong: a test that asserts a map-range produced keys in insertion order, or a log
line that prints a `map[string]string` directly. Map order is randomized per range
statement, so this is flaky across runs and non-reproducible on the wire.

Fix: impose order. `slices.Sorted(maps.Keys(m))` for keys, or collect to a slice
and `sort.Slice`, before you log, hash, serialize, or assert.

### Mutating slice elements through the range value copy

Wrong: `for _, r := range orders { r.Processed = true }`. `r` is a copy; the write
is discarded and `orders` is unchanged.

Fix: index the backing array, `for i := range orders { orders[i].Processed = true }`.

### Ranging a channel the producer never closes

Wrong: a consumer's `for v := range ch` where no one ever closes `ch`. The
consumer blocks forever and leaks its goroutine.

Fix: the producer (or a `wg.Wait(); close(ch)` closer goroutine in a fan-in) owns
closing the channel exactly once, when there is nothing left to send.

### Ignoring yield's bool in a range-over-func iterator

Wrong: an `iter.Seq` implementation that calls `yield(v)` and ignores the return,
so a caller's `break` does not actually stop the iterator — it keeps fetching
pages or reading the network.

Fix: `if !yield(v) { return }` after every `yield`, and run cleanup on the way out
(a `defer` inside the iterator function).

### Forgetting stop() when pulling

Wrong: `next, _ := iter.Pull(seq)` and never calling `stop()`. The goroutine
driving the push iterator leaks.

Fix: `next, stop := iter.Pull(seq); defer stop()`.

### Treating a string range index as a rune count

Wrong: using the byte index `i` from `for i, r := range s` as a character count,
or slicing `s[:i]` on a multibyte string assuming one byte per character. It
truncates mid-rune and corrupts UTF-8.

Fix: use `utf8.RuneCountInString(s)` for a count; the byte index is a byte index,
correct for `s[i:]` slicing at rune boundaries but not a character position.

### Copying large structs every iteration in a hot path

Wrong: `for _, big := range xs` where `big` is a large struct and the loop is hot;
each iteration copies the whole struct.

Fix: index (`xs[i].Field`) or range a slice of pointers when you only read a few
fields of a big element.

### Passing 0 or a negative size to slices.Chunk

Wrong: `slices.Chunk(records, n)` with `n <= 0`, which panics. A batch size read
from config can be 0.

Fix: validate `n >= 1` before calling, and treat a non-positive batch size as a
configuration error at the boundary.

### Assuming maps.Keys returns a slice

Wrong: `keys := maps.Keys(m); keys[0]` — `maps.Keys` returns a lazy `iter.Seq[K]`,
not a slice, so it is not indexable.

Fix: `keys := slices.Sorted(maps.Keys(m))` (or `slices.Collect`) to get a slice
first.

Next: [01-metrics-aggregator-range-forms.md](01-metrics-aggregator-range-forms.md)
