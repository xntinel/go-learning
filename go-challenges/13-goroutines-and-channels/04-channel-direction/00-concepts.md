# Channel Direction as an API Contract — Concepts

Channel direction (`chan<- T` and `<-chan T`) is one of Go's most under-used
compile-time tools. Most engineers first meet it as a syntax curiosity: the
arrow can go on either side of `chan`. That framing misses the point. Direction
is how a senior engineer encodes ownership and lifecycle contracts into a
concurrency API so that the compiler — not a code reviewer, not a runbook, not a
comment — enforces them at the package boundary. The load-bearing rule in every
production streaming subsystem is "the sender owns the channel and is the only
party allowed to close it." Direction types are what make that rule expressible
and unforgeable. Read this once and you have the model behind all nine exercises
that follow: a producer/transform/consumer ETL pipeline, a change-feed source, a
batching sink, a fan-in merge, a tee, an or-done cancellation wrapper, a pub/sub
broker, a graceful-shutdown handler, and a ticker-driven rate limiter — each the
directional shape of a real backend subsystem.

## Concepts

### Three distinct types, one underlying channel

`chan T`, `chan<- T`, and `<-chan T` are three different types even though they
all refer to the same kind of runtime object. A bidirectional `chan T` permits
send, receive, and close. A send-only `chan<- T` permits send and close but not
receive. A receive-only `<-chan T` permits receive only — it can neither send
nor close. The direction is part of the static type, so the restriction is
checked when the program is compiled, never at run time. There is no reflection
trick, no runtime flag: a receive from a `chan<- int` simply does not compile.

Read the arrow as the direction data flows relative to *you*, the holder. `out
chan<- int` means "values flow out of you into the channel" — you feed it.
`in <-chan int` means "values flow from the channel into you" — you drain it.

### The conversion is one-way and implicit — that is the whole trick

A bidirectional `chan T` is assignable to `chan<- T` or to `<-chan T`. This
happens implicitly at an assignment or, far more commonly, at a call boundary:
you hold a `chan int`, you pass it to `func Produce(out chan<- int)`, and the
compiler narrows it to send-only for the callee. The callee now physically
cannot receive from or — depending on direction — close the channel it was
handed.

The asymmetry is the entire point. The conversion only ever goes from
bidirectional to directional, and from a directional type to *nothing else*. You
cannot convert a `<-chan T` back to a `chan T`, and you cannot convert a
`<-chan T` to a `chan<- T`. Once a value has been narrowed, the restriction is
permanent for that reference. That one-way narrowing is what makes a directional
API contract impossible to forge: a caller handed a `<-chan Event` has no legal
expression that recovers the ability to close or send.

### Direction encodes ownership, and ownership is the real rule

The canonical Go concurrency rule is: *only the sender should close a channel,
and it should close exactly once.* Closing tells receivers "no more values are
coming"; sending on a closed channel panics, and closing an already-closed
channel panics. So "who may close" is not a style question — getting it wrong is
a crash. Direction types turn that social rule into a structural one. Hand every
consumer a `<-chan` and the language guarantees they cannot `close(ch)` — it is
a compile error, because closing a receive-only channel is forbidden. Hand every
producer a `chan<- ` and they can feed and, if they own it, close. The
compiler now enforces "the sender owns and closes" for you.

### close on a directional channel

Precisely: `close(ch)` where `ch` is `<-chan T` is a compile error — receivers
must not close. `close(ch)` where `ch` is `chan<- T` is legal, because the
sender/owner is the party allowed to close. This is exactly why the generator
pattern returns `<-chan`: the function keeps a bidirectional reference for
itself (so it can close on completion) and hands the caller a receive-only view
that the caller cannot corrupt.

### The four close-related panics you are designing around

Directional types exist to eliminate a specific family of run-time crashes.
Receiving from a closed channel is *safe*: it yields the zero value immediately
with `ok == false` via `v, ok := <-ch`, which is how a `for range` over a
channel terminates. The dangerous operations are: sending on a closed channel
(panic), closing an already-closed channel (panic), and closing a nil channel
(panic). A receive-only type at the boundary makes the first two impossible for
that holder; you still have to guarantee the owner closes exactly once. That
"exactly once" is a real design obligation in fan-in, where multiple goroutines
feed one output.

### The generator / source pattern

A source function creates a channel, launches a goroutine that produces values
and then closes the channel on completion, and returns the *receive-only* end.
The returned `<-chan` is a self-contained stream: its whole lifecycle —
production and the single close — lives inside the function, and the caller can
only drain it. `os/signal`, `time.Tick`, and `context.Done` are all this shape.
The failure mode a source must avoid is leaking its producer goroutine: if the
consumer stops draining an unbuffered channel, the producer parks on its next
send forever. Every long-lived source therefore needs both a close on normal
completion and a cancellation path (see or-done).

### The sink pattern

The mirror image: a sink accepts a `<-chan` and drains it with `for v := range
in` until the channel closes. The receive-only parameter type guarantees the
sink can never accidentally send back upstream or close the producer's channel —
two mistakes that a bidirectional parameter would happily compile. A real sink
(bulk insert, log shipper) also batches: it accumulates values and flushes on a
size threshold or on channel close, treating close as the end-of-stream signal
that flushes the final partial batch.

### Fan-in and the exactly-once close

Merging N input streams into one output is the pattern where "close exactly
once" bites. The correct construction: one drain goroutine per input, each
forwarding its input's values to the shared output; a `sync.WaitGroup` that each
drain goroutine marks `Done` when its input closes; and a single closer goroutine
that calls `wg.Wait()` and then closes the output once. Closing the output from
inside any drain goroutine instead would close it as soon as the *first* input
drains — the other goroutines then send on a closed channel and panic, and if
two finish together you also get a close-of-closed panic. The WaitGroup plus
lone closer is the only construction that closes the merged output exactly once,
after every input has drained.

### Directional channels are all over the stdlib

The standard library uses direction as deliberate API documentation, and reading
those signatures teaches the intended ownership. `signal.Notify(c chan<-
os.Signal, sig ...os.Signal)` and `signal.Stop(c chan<- os.Signal)` take
send-only: the signal package is the producer, you supply the channel it feeds.
`context.Context.Done() <-chan struct{}` hands back receive-only: you may only
wait on cancellation, never trigger it by closing that channel. `time.Ticker.C`
is `<-chan Time`, and `time.After`/`time.Tick` return `<-chan Time`: the runtime
owns and feeds the tick stream. When you see these signatures, the direction is
telling you who closes and who feeds.

### Direction is an encapsulation boundary, not a field type

A recurring smell is storing a `chan<- T` as a struct field, hoping to "make the
field send-only." It backfires: a send-only field cannot be received from for
internal fan-out and cannot be closed on shutdown, so the type loses the two
operations it most needs. The idiomatic design stores a *bidirectional* `chan T`
internally — where the type has full control to receive, fan out, and close —
and exposes direction only through method signatures: `Subscribe() <-chan Event`
returns receive-only views, and `Publish(e Event)` is the only send path.
Direction belongs at the API surface (parameters and return types), not on the
private field.

### select composes with direction, and nil disables a case

In a `select`, a send-only channel may appear only in a send case and a
receive-only channel only in a receive case — the same static check, now
per-case. A separate but essential fact: a `nil` channel blocks forever, so a
`select` case on a nil channel is effectively disabled. Setting a channel
variable to `nil` after you have used it lets you turn a `select` case off. That
is the mechanism behind the tee's "send this value to each of the two outputs
exactly once": start with both output variables live, and after a successful
send to one, set that variable to `nil` so the next `select` iteration can only
choose the other.

### Context cancellation composes with direction

`ctx.Done()` is a `<-chan struct{}`. Because it is just another receive-only
channel, you `select` against it alongside your data channel. That is how a
consumer abandons a stream: `select { case <-ctx.Done(): return; case v, ok :=
<-in: ... }`. When the context is cancelled, the consumer returns and its
goroutine exits instead of leaking, and any `out` channel it owned is closed by
its `defer`. Wrapping this once in an `OrDone(ctx, in) <-chan T` helper gives
every downstream consumer cancellation for free.

## Common Mistakes

### Receiving from a send-only channel or sending to a receive-only channel

Wrong: `v := <-out` where `out` is `chan<- int`, or `in <- v` where `in` is
`<-chan int`. Both are compile errors, not runtime bugs. The fix is to use the
correct end and let the boundary conversion happen at the call site: the caller
holds a bidirectional `chan int`, and passing it narrows it to the right
direction for each side.

### Closing a channel from the consumer side

Wrong: a consumer calls `close(ch)` on the channel it is draining. If the API
had handed back `<-chan`, this would not even compile. When the API leaks a
bidirectional channel, a consumer that closes it causes the producer's next send
to panic with "send on closed channel." Fix: the owner (the sender) closes;
consumers receive only. Hand consumers a `<-chan` so the compiler enforces it.

### Double close in fan-in

Wrong: closing the merged output from inside each input's drain goroutine. The
first input to drain closes the output, and every other drain goroutine then
sends on a closed channel and panics; two finishing together panic on
close-of-closed. Fix: a `sync.WaitGroup` plus a single closer goroutine that
closes once after `wg.Wait()`. Run `-race` — it is the fastest way to catch a
stray close.

### Leaking the producer goroutine

Wrong: a source returns a `<-chan` but never closes it, or blocks forever on a
full unbuffered send after the consumer has stopped draining. The producer
goroutine parks and is never collected. Fix: pair every source with a `defer
close(out)` on normal completion and a cancellation/stop path (a `<-chan
struct{}` stop signal or `ctx.Done()`) so the goroutine can exit when the
consumer leaves.

### Storing a chan<- as a struct field

Wrong: a broker with a `chan<- Event` field, expecting to fan out or close it
later. A send-only field cannot be received from or closed. Fix: store a
bidirectional `chan Event` internally and expose direction only through method
signatures — `Subscribe() <-chan Event`, `Publish` sends.

### A sequential tee that stalls on a slow consumer

Wrong: a tee that sends to output A, waits for it to be consumed, then sends to
output B. A slow or stalled A consumer now blocks B from ever seeing the value,
silently breaking the "both outputs get every value" invariant. Fix: use the
`nil`-channel/`select` trick to send to each output exactly once per value, in
whichever order each is ready, so neither consumer's pace gates the other's
correctness.

### Delivering a real OS signal in a unit test

Wrong: a shutdown-handler unit test that actually raises `SIGTERM` in the
process. It is flaky and environment-dependent, and can tear down the test
runner. Fix: test the waiter against an injected `<-chan os.Signal` fed a fake
`syscall.SIGTERM`; reserve a real, self-sent signal for an optional,
timeout-guarded integration test.

### An unbuffered signal channel with signal.Notify

Wrong: `c := make(chan os.Signal); signal.Notify(c, ...)`. The signal package
does a non-blocking send, so if no receiver is parked on `c` at the instant the
signal arrives, the signal is dropped. Direction does not save you here —
buffering does. Fix: `make(chan os.Signal, 1)`.

### Assuming close broadcasts a value

Wrong: treating a received zero value as "the real data" without checking
whether the channel closed. `close` signals end-of-stream, not a value; a
closed channel yields the zero value with `ok == false`. Fix: use the two-value
receive `v, ok := <-ch` or `for range` to distinguish a genuine zero value from
a closed channel.

Next: [01-streaming-etl-pipeline.md](01-streaming-etl-pipeline.md)
