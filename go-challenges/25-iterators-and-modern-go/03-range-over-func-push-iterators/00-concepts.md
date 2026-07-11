# 3. Range Over Func -- Push Iterators -- Concepts

Since Go 1.23 the `for ... range` statement can range over a function value. That
single language change turns an ordinary function into an iterator: you write a
function that produces values and hands each one to a callback, and callers walk
those values with the same `for x := range thing` syntax they already use for
slices, maps, and channels. These functions are called push iterators, because
the iterator pushes each value into the loop body rather than the loop pulling
values out. This file is the conceptual foundation for the exercises, which build
push iterators over generated sequences, over key/value pairs, and over a
recursive tree, each as an independent, self-contained Go module.

## Concepts

### The Three Iterator Shapes the Compiler Accepts

The `for range` clause accepts a function value of exactly one of three shapes,
and the standard library gives the two useful ones names in the `iter` package:

- `func(yield func() bool)` -- a sequence of nothing but iterations (rare; used
  for "do this N times" producers).
- `iter.Seq[V]`, which is `func(yield func(V) bool)` -- a sequence of single
  values. This is what `for v := range seq` ranges over.
- `iter.Seq2[K, V]`, which is `func(yield func(K, V) bool)` -- a sequence of
  pairs. This is what `for k, v := range seq` ranges over.

The compiler rewrites `for v := range seq { body }` into a call `seq(yield)`,
where `yield` is a synthesized function whose body is the loop body. The
iterator is just a function; the magic is entirely in how the compiler builds
the `yield` argument and wires `break`, `return`, `continue`, and `panic` in the
loop body into that function's control flow. Nothing about the iterator type is
special to the runtime beyond this rewrite -- `iter.Seq[V]` is a plain generic
type alias for a function type, and you can store one in a variable, pass it
around, and call it directly with your own `yield`.

### Why "Push", and How It Differs From a Pull Iterator

The defining property of a push iterator is who is in control. The iterator
function owns the loop: it decides the order, it walks the data structure, and it
calls `yield` once per value. The consumer's loop body is a passive callback that
runs each time the iterator pushes. This is the natural shape for any producer
that already has a loop or a recursion -- walking a slice, traversing a tree,
reading lines from a file -- because the producer keeps its own control flow and
simply replaces "append to a result" with "call `yield`".

The alternative, a pull iterator, inverts that: the consumer calls a `Next`
function to pull the next value on demand, and the producer must suspend and
resume between calls. Pull iterators are what `iter.Pull` builds (the next
lesson), and they are the right tool when the consumer needs to advance two
sequences in lockstep or stop and resume across function boundaries. Push is the
default and the simpler of the two; reach for pull only when the push shape
genuinely does not fit.

### The Yield Protocol: the Bool Return Controls Termination

The single most important rule of push iterators lives in the bool that `yield`
returns. Each call `yield(v)` returns `true` if the consumer wants more values
and `false` if it does not. The iterator must inspect that result on every call
and stop the moment it sees `false`:

```go
for cur := head; cur != nil; cur = cur.Next {
	if !yield(cur.Value) {
		return
	}
}
```

`yield` returns `false` whenever the consumer's loop body exits early for any
reason: a `break`, a `return` out of the enclosing function, a `goto` or labeled
`continue` that leaves the loop, or a `panic` propagating through it. The
compiler funnels all of those into one signal -- the next `yield` call returns
`false` -- so the iterator does not need to know which one happened. It only
needs to honor the contract: see `false`, return promptly, run any cleanup
(`defer file.Close()`) on the way out.

Ignoring the result is the cardinal bug. If the iterator keeps calling `yield`
after it has already returned `false`, the program is incorrect, and the code the
compiler generates around the loop panics at run time rather than silently doing
the wrong thing. The same panic guards the other half of the contract: once the
iterator function itself returns, `yield` must never be called again (for example
from a goroutine the iterator forgot to join). Treat `if !yield(v) { return }`
as the non-negotiable skeleton of every push iterator.

### Threading the Stop Signal Through Recursion

A flat loop honors the protocol with a plain `return`, because `return` leaves
the whole iterator function. Recursion is where it gets subtle. When a tree
traversal is written as a recursive helper, a bare `return` inside the helper
unwinds exactly one stack frame -- it stops that node's work but the parent frame
happily continues to the right subtree, calling `yield` again after it already
returned `false`, which is the very panic the protocol forbids.

The fix is to make the recursive helper return a bool that means "keep going",
and to propagate it at every call site:

```go
func (n *node[V]) push(yield func(V) bool) bool {
	if n == nil {
		return true
	}
	if !n.left.push(yield) {
		return false
	}
	if !yield(n.value) {
		return false
	}
	return n.right.push(yield)
}
```

Every recursive call is guarded by `if !...{ return false }`, so a single
`false` from `yield` deep in the left subtree races straight back up through
every parent frame and the traversal halts cleanly. This bool-threaded helper is
the canonical pattern for any recursive push iterator, and it is exactly what the
tree exercise builds.

### Validating Inputs Without Breaking the Loop

An iterator value is lazy: the function body does not run until `for range` calls
it. That creates a question about where to put input validation. If a constructor
can be given an invalid argument (a negative count, a nil root), you do not want
the error to surface in the middle of someone's loop, because a push iterator has
no clean channel to report an error mid-iteration -- it can only stop. The idiom
is to validate eagerly and return `(iter.Seq[V], error)` from the constructor:
the caller handles the error before the loop, and by the time `for range` runs
the iterator the inputs are already known good. Reserve the `iter.Seq2[K, error]`
shape (a value/error pair per element) for the genuinely different case where
each individual element can fail, such as a streaming decoder.

### Naming and Method Conventions

The standard library settled on conventions worth following so your iterators
read like the ones in `slices` and `maps`. A method that iterates every element
of a collection is named `All` and returns `iter.Seq[V]` (or `iter.Seq2[K, V]`
for indexed or keyed collections); `slices.All`, `maps.All`, and `maps.Keys` are
the models. A standalone constructor that generates a sequence is named for what
it produces. Iterators are cheap to return and compose well: because an
`iter.Seq[V]` is just a function, a filtering or mapping wrapper is another
function that calls the inner one with a wrapping `yield`, and the bool protocol
threads through the composition for free.

## Common Mistakes

### Ignoring the Bool That Yield Returns

Wrong: calling `yield(v)` for its side effect and continuing the loop
unconditionally, as if `yield` returned nothing.

What happens: when the consumer breaks out early, `yield` returns `false`, but
the iterator keeps producing and calls `yield` again. The compiler-generated loop
machinery detects the violated contract and panics at run time. Even without a
crash the iterator does pointless work after the consumer stopped caring.

Fix: write every yield as `if !yield(v) { return }`. The `return` is the entire
point of the bool; it is what makes `break` in the caller actually stop the
producer.

### A Bare Return Inside a Recursive Helper

Wrong: writing a recursive traversal whose helper returns nothing and uses a
plain `return` when `yield` reports `false`.

What happens: the `return` unwinds only the current frame. The parent frame
resumes and visits the next subtree, calling `yield` after it already returned
`false` -- the forbidden call -- which panics.

Fix: have the helper return `bool` ("keep going"), guard every recursive call
with `if !child.push(yield) { return false }`, and return the result of `yield`
so a single `false` propagates through every frame.

### Reporting Per-Constructor Errors From Inside the Loop

Wrong: letting a constructor accept an invalid argument and only discovering it
once `for range` starts pulling values, with no way to surface it.

What happens: a push iterator cannot return an error mid-iteration; it can only
stop, so the loop ends early and silently and the caller never learns why.

Fix: validate eager arguments in the constructor and return `(iter.Seq[V],
error)` so the caller checks the error before the loop. Reserve the
value/error-pair (`iter.Seq2[V, error]`) shape for sequences where each element
can independently fail.

### Returning a Channel and Goroutine for an In-Memory Sequence

Wrong: spawning a goroutine that sends values on a channel and handing the
channel back, then ranging the channel, for a plain in-memory walk.

What happens: every consumer that breaks early leaks the goroutine, which blocks
forever on a send no one will receive. The channel approach also costs a
goroutine and synchronization per sequence.

Fix: return `iter.Seq[V]`. The bool protocol handles early termination with no
goroutine, no channel, and no leak. Reach for channels only when the producer is
genuinely concurrent.

---

Next: [01-seq-and-the-yield-protocol.md](01-seq-and-the-yield-protocol.md)
