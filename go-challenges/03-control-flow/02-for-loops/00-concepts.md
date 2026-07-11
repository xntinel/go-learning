# For Loops: The Three Forms in Production Control Flow

Go has exactly one loop keyword, `for`, and it wears four hats: the counted
loop, the condition-only loop, the bare infinite loop, and `range`. That economy
is not a language quiz — it is a design discipline. In backend code almost every
control-flow decision reduces to choosing the loop *shape* that matches the exit
condition, because the shape you pick is a claim about *how the loop terminates*.
A counted loop claims "this runs a bounded number of times." A condition-only
loop claims "this runs until a predicate flips." An infinite `for {}` claims
"this runs until an explicit signal tells it to stop." Choosing the wrong shape
is choosing the wrong termination proof, and a loop with a broken termination
proof is not a style problem — it is an availability incident waiting for a slow
dependency to trigger it.

This file is the conceptual spine for ten independent exercises. Each one takes a
real backend artifact — a rate limiter, a retry, a batch writer, a paginator, a
worker pool, a scheduler search, a service event loop, a readiness probe, a lazy
iterator, a log parser — and shows how the loop shape *is* the design. Read this
once and you have the model you need for all ten.

## The model: one keyword, three forms, one range

### Counted `for i := 0; i < n; i++` is a resource budget

The counted loop is the shape for *known, bounded* iteration. In production the
bound is almost never "a number for its own sake" — it is a budget: retry at most
N times, write at most `size` rows per statement, fetch at most `maxPages`. The
value of the counted form is that the bound is visible at the top of the loop, so
a reviewer can see the worst case without reading the body. The moment you write
a loop that *should* be bounded but is not (`for i := 0; ; i++`), you have thrown
away that guarantee and created a loop that runs forever against a misbehaving
dependency.

Go 1.22 added integer range: `for i := range n` iterates `i` from `0` to `n-1`.
Prefer it whenever the index is exactly `0..n-1`, because it communicates a fixed
count to both the reader and the compiler and removes the off-by-one surface of a
hand-written three-clause loop. Since Go 1.22 the loop variable is also a fresh
variable per iteration, so the old "capture the loop variable in a goroutine" bug
(`for _, v := range xs { go func() { use(v) }() }` all seeing the last `v`) is
gone — you can capture the loop variable in a closure or goroutine safely, and
the `v := v` shadowing line that used to be mandatory is now dead code.

### Condition-only `for cond` is a wait whose termination must be provable

`for check() != ok` is the wait/poll shape: run until a predicate flips. The
danger is that the predicate might *never* flip. A readiness poll against a
database that never comes up, a spin on a flag that another goroutine forgot to
set — a condition-only loop with no independent bound and no cancellation is a
hang. The production discipline is that every condition loop pairs its predicate
with two escape hatches: a bounded attempt/deadline budget so it cannot run
forever, and a `ctx.Done()` check so a caller can cancel it. "Provable
termination" means you can point at the line that guarantees the loop stops even
when the predicate never becomes true.

### Infinite `for {}` is a drain/event loop whose only exit is explicit

The bare `for {}` is correct exactly when the exit condition has nothing to do
with a counter or a simple predicate: a long-lived service loop, a pagination
drain, a channel consumer. Its body must contain the explicit `break` or `return`
that ends it, and in a backend that explicit exit is almost always one of two
things — a closed input channel, or a cancelled context. An infinite loop whose
only exit is "the work happens to run out" is fine for a paginator with a real
terminal cursor; an infinite loop with *no* reachable exit under a stuck upstream
is a hot loop that pins a core. Pair every infinite loop with both a structural
exit (cursor empty, channel closed) and a safety exit (`ctx.Done()`, a page cap).

### `for range ch` is the canonical bounded consumer

Ranging over a channel blocks until a value arrives or the channel is closed, and
terminates cleanly on close. This is the backbone of worker pools and pipelines:
`for job := range jobs` consumes until the producer closes `jobs`, then the
worker returns on its own. The ownership rule is non-negotiable: the *producer*
owns the channel and closes it; consumers never close a channel they read,
because a second close panics and a send on a closed channel panics. The clean
shutdown of a pool is "producer closes `jobs`, workers drain and return, a
`WaitGroup` waits for them, then the last goroutine closes `results`."

### `for { select { ... } }` is the shape of every service

The infinite `for` wrapped around a `select` is the canonical long-lived worker:
it multiplexes an input channel, a periodic ticker, and a cancellation signal.
Its correct exits are precisely `case <-ctx.Done()` and a closed input channel.
The subtlety that separates a correct service loop from a lossy one is the
*bounded drain on shutdown*: when the context is cancelled, in-flight or buffered
work should be flushed — up to a bound — before the loop returns `ctx.Err()`, so
a shutdown does not silently drop events. Unbounded drain is its own bug: a
cancelled loop that keeps draining a fast producer never actually shuts down.

### Range-over-func (Go 1.23): a package can expose its own `for`

Go 1.23 lets `for v := range seq` iterate over a function value whose type is
`iter.Seq[V]`, defined as `func(yield func(V) bool)`. The producer calls
`yield(v)` once per element; `yield` returning `false` means the consumer used
`break` (or `return`) and the producer must stop. This is how a data-access layer
hides pagination behind a plain loop: the caller writes
`for item := range db.Pages(ctx)` and each page is fetched lazily only as the
caller pulls. The producer's obligation is to *check yield's bool return* — if it
ignores it and keeps fetching after the consumer broke out, it does exactly the
over-fetching the abstraction was meant to prevent. (Calling `yield` again after
it returned `false` panics, so checking is not optional.)

## Labels: the one place they earn their keep

`break` exits the innermost `for`; `continue` skips to the next iteration of the
innermost `for`. A labeled `break L` / `continue L` targets an *outer* loop from
inside an inner one. This is the only clean way to leave or skip an outer loop
from within a nested search — for example, scanning a two-dimensional
availability grid and exiting *both* loops on the first fit. A plain `break` there
only exits the inner loop and the outer scan wastefully continues, which is a
real and common bug. Labels pay for themselves exactly in this nested-search case;
anywhere else, extracting the inner work into a helper function with an early
`return` is usually clearer than a label. Reach for a label when a helper would be
awkward (it would need to close over many locals) and the loop genuinely must
control an outer iteration.

## `continue` flattens the happy path

Inside a loop, deeply nested `if/else` around the "real" work is a smell. Invert
each rejection into an early-`continue` guard — skip blank lines, skip comments,
skip malformed rows — so the happy path stays at the loop body's top indentation
level. An ingestion loop that reads `if valid { if parseable { if nonEmpty {
process() } } }` should read as three `continue` guards followed by a flat
`process()`. This is the same "early return" discipline applied to a loop body.

## Determinism: never sleep to test a loop that depends on time

Every loop whose behavior depends on time — a token refill, an exponential
backoff, a poll interval — must take its time source as an injected dependency so
a test can advance it explicitly. A test that calls `time.Sleep` to "wait for the
refill" is slow and flaky: it couples the assertion to the wall clock and the
scheduler. Inject a `Clock func() time.Time` (or a fake ticker channel, or use a
`testing/synctest` bubble) that the test advances by exact amounts, and assert
the exact outcome. The corollary rule is *take the clock once, in the
constructor*: a `SetClock` setter that swaps the clock after construction is
hidden mutable state that races under concurrent requests. Construct-time
injection is deterministic and race-free; a setter is neither.

## Cancelable waits: `NewTimer`/`NewTicker` in a `select`, then `Stop`

A wait *inside a loop* must be cancelable. The correct shape is a
`time.NewTimer(d)` (or `NewTicker`) whose channel is one case of a `select` whose
other case is `ctx.Done()`, and you call `Stop()` on the timer on the
cancellation path. The tempting shortcut `case <-time.After(d):` inside a loop is
a leak: `time.After` allocates a fresh timer *every iteration*, none of them can
be stopped, and under a stuck upstream the loop accumulates live timers until the
context finally fires. `NewTimer` once per wait, `Stop()` on cancel, is the
non-leaking form.

## Bounded budgets everywhere

The through-line of every loop in this lesson is a bound. A retry loop bounds
attempts. A pagination drain bounds pages. A readiness poll bounds attempts and
honors a deadline. A shutdown drain bounds the number of buffered events it
flushes. The general principle: any loop that iterates *in response to an external
system* must have an explicit upper bound plus a cancellation path, because the
external system can and eventually will misbehave, and an unbounded loop turns
that misbehavior into an outage of your service rather than a handled error.

## Common Mistakes

### Sleeping to test a time-dependent loop

Wrong: `l.Allow(); time.Sleep(time.Second); if !l.Allow() { ... }` to test that a
token refilled. The test is slow, and on a loaded CI box the sleep may not line up
with the refill, so it flakes.

Fix: inject a `Clock` and advance it in the test (`clock.Advance(time.Second)`),
or run under a `testing/synctest` bubble. The assertion becomes exact and the test
runs in microseconds.

### A `SetClock` setter instead of constructor injection

Wrong: exposing `func (l *Limiter) SetClock(c Clock)` so a test can swap the clock.
That is mutable shared state; two concurrent requests plus a test swap is a data
race.

Fix: take the clock once in `New(..., clock Clock)` and never mutate it. Tests pass
their fake clock at construction.

### `for i := 0; ; i++` for bounded work

Wrong: an infinite loop with an unused index for work that is actually bounded. No
upper bound is visible and the index is noise.

Fix: `for range n` (or `for n > 0 { n-- }`) states the bound, lets the compiler
own the counter, and removes the off-by-one surface.

### `time.After` inside a loop body

Wrong: `for { select { case <-time.After(d): ...; case <-ctx.Done(): return } }`.
Each iteration leaks a timer that cannot be stopped.

Fix: `t := time.NewTimer(d)` once per wait, `select` on `t.C` and `ctx.Done()`, and
`t.Stop()` on the cancel path.

### Plain `break` expecting to exit a nested search

Wrong: a `break` in the inner loop of a two-dimensional search, believing it ends
the whole search. It only ends the inner loop; the outer loop keeps scanning.

Fix: a labeled `break outer`, or extract the inner loop into a helper that returns
the first match.

### Retry or pagination loop with no cap

Wrong: retry "until it works" or page "until the cursor is empty" with no attempt
or page bound. A stuck upstream turns the loop into an infinite hot loop or an
unbounded memory grow.

Fix: cap attempts/pages, and add a `ctx.Done()` exit, so a misbehaving dependency
yields a handled error instead of an outage.

### Off-by-one in bucket or batch math

Wrong: `>` where `>=` was meant when consuming tokens, or slicing
`records[i : i+size]` past the end on the final short batch.

Fix: use `min(i+size, len(records))` to bound the final chunk and half-open slice
indices `[low:high)`; use `>= 1` (not `> 1`) when a single token must be spendable.

### Mixing state mutation and time math in one loop body

Wrong: a hand-rolled limiter whose loop body refills, decides, and waits all
tangled together — the classic source of double-spend and negative-token bugs.

Fix: structure the body as three ordered steps — refill, then decide, then wait —
so each concern is isolated and testable.

### Ignoring `yield`'s bool in a range-over-func producer

Wrong: an `iter.Seq` producer that calls `yield(v)` and keeps fetching pages
regardless of the return value, so a consumer `break` does not stop the fetching
(and a later `yield` after `false` panics).

Fix: `if !yield(v) { return }` — stop producing the instant the consumer breaks.

### Deep nesting instead of early-continue guards

Wrong: a loop body buried three `if`s deep, with the real work at the bottom.

Fix: invert each condition into an early `continue`, keeping the happy path flat at
the top indentation level.

Next: [01-token-bucket-rate-limiter.md](01-token-bucket-rate-limiter.md)
