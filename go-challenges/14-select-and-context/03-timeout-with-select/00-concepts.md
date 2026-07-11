# Timeouts with select and time.Timer — Concepts

A bare channel receive is unbounded. `v := <-ch` blocks the goroutine until a
sender delivers, forever if none ever does. In a request handler, a worker, or a
call into a downstream service, that unbounded wait is the single most common
source of goroutine leaks and stuck requests in Go backends. A timeout is the
budget you put on that wait, and `select` against a timer channel is the
primitive that converts an unbounded receive into a bounded one. Everything
higher-level — `context.WithTimeout`, `http.Client.Timeout`, a database query's
context deadline — is built on exactly this mechanism. This lesson stays
deliberately on the pre-context primitives so that when you later reach for
`context`, you know precisely what it is doing underneath: which channel a
`select` actually chose, whether a fired timer left a stale tick, whether the
goroutine feeding an abandoned channel leaks, and how one SLA is split across
sub-steps.

Read this file once and you have the model behind all ten exercises that follow.
Each exercise is an independent, self-contained module that builds one realistic
backend artifact — a timeout library, a leak demonstrator, an idle-timeout
consumer, an SLA guard, a retry loop, a batch flusher, a liveness monitor, a
debouncer, a deadline splitter, and a graceful drain.

## The bounded receive

`select` waits on several communications at once and proceeds with whichever is
ready first. Put your data channel in one case and a timer channel in another,
and the receive gains a budget:

```go
select {
case v := <-ch:
	// value arrived within budget
case <-time.After(d):
	// budget elapsed first
}
```

`time.After(d)` returns a `<-chan time.Time` that receives a single value after
`d`. It is exactly `time.NewTimer(d).C`: a one-shot timer whose channel fires
once. The moment either case is ready, `select` takes it; if both are ready at
once, it picks one uniformly at random. That randomness is a correctness
constraint, not a detail: never write logic that assumes the timer case loses (or
wins) a tie against a ready data channel. Under load both can be ready in the same
instant, and the choice is a coin flip.

## time.After allocates; in a loop that is a problem

`time.After(d)` allocates a fresh `Timer` on every call, and that timer's runtime
entry lives until it fires. For a single one-shot receive this is fine. Inside a
hot loop it is not: each iteration allocates a new timer, and before Go 1.23 a
pending `time.After` could not be garbage-collected until it fired, so a loop
that allocated one per iteration pinned every un-fired timer in memory. Go 1.23
made unreferenced timers eligible for collection immediately, which removes the
leak, but the per-iteration allocation is still wasted work and GC pressure. The
fix for any loop-bound wait is to create one `time.NewTimer` outside the loop and
`Reset` it each iteration, or to use a `time.Ticker` when the cadence is fixed.

## Reusing one timer: Stop, drain, Reset — and what Go 1.23 changed

`time.NewTimer(d)` returns a `*time.Timer` with a `C` channel and `Stop`/`Reset`
methods. To reuse it across iterations you `Reset(d)` it. The historically tricky
part was that on a timer that had already fired, a value was sitting buffered in
`C`; calling `Reset` without first draining that value meant the next receive on
`C` returned the *stale* tick immediately instead of waiting the new duration.
The canonical guard was the Stop-drain-Reset dance:

```go
if !timer.Stop() {
	select {
	case <-timer.C:
	default:
	}
}
timer.Reset(d)
```

`Stop` returns `false` if the timer had already fired (or was already stopped);
the non-blocking `select` then drains any pending tick. Note the drain must be
non-blocking (`default` case): `Stop` does not close `C`, so a blind `<-timer.C`
after a `Stop` that already consumed the value would deadlock.

Go 1.23 changed the underlying semantics. Timer and ticker channels are now
**unbuffered (capacity 0)** rather than buffered (capacity 1), and the language
now guarantees that after any `Stop` or `Reset` call, no stale value prepared
before that call will be sent or received. In other words, on a module whose
`go.mod` declares `go 1.23.0` or later, the drain is no longer necessary — a
plain `timer.Stop(); timer.Reset(d)` is correct, and even `Reset` alone will not
deliver a stale tick. The old buffered behavior can be restored with
`GODEBUG=asynctimerchan=1`, and the old behavior also remains in effect when a
Go 1.23+ toolchain builds a module whose `go.mod` still names an older version.

The senior takeaway is to know which regime your code targets. On modern builds
the drain is redundant but harmless, so the Stop-drain-Reset form is safe
everywhere and is the right choice for code that must also compile under older
toolchains. Assuming the *new* auto-drain semantics while still targeting an old
`go.mod` version is the dangerous mistake: a stale buffered tick fires `Reset`
immediately and your idle timeout collapses to zero. The exercises here write the
portable Stop-drain-Reset form and call out where 1.23 makes it optional.

## Per-message budget versus overall deadline

These are different questions and they use the timer differently. A fresh timer
*inside* the loop body gives each receive its own budget: "how long do I wait for
the next value?" One timer created *outside* the loop is the total operation
deadline: "how long may the whole operation run, across all values?"

```go
// per-message: each iteration gets a fresh budget
for {
	select {
	case v := <-ch:
		process(v)
	case <-time.After(perMsg): // new budget every iteration
		return
	}
}

// overall: one timer bounds the entire loop
deadline := time.NewTimer(total)
defer deadline.Stop()
for {
	select {
	case v := <-ch:
		process(v)
	case <-deadline.C: // fires once, ends the whole loop
		return
	}
}
```

Mixing them silently produces the wrong behavior: put `time.After` outside the
loop expecting a per-message budget and it becomes a one-shot overall deadline
that fires once and never again; reset a timer inside the loop when you wanted a
hard total cap and the operation can run unbounded as long as values keep
trickling in. Decide which question you are answering before you place the timer.

## The leak the timeout hides

When the timeout case wins the race, the goroutine that was going to produce the
value is still running and about to send. Where does that send go? If the result
channel is *unbuffered* and the only reader (your `select`) has already moved on,
the send blocks forever and the producer goroutine leaks. Under sustained load
this is a slow, silent goroutine leak that eventually exhausts memory and
scheduler capacity. The fix is structural and cheap: give the result channel a
buffer of one, `make(chan T, 1)`, so the abandoned producer can complete its send
into the buffer and return even though nobody will ever read it.

```go
res := make(chan T, 1) // buffer of 1: the loser of the race can still send and exit
go func() { res <- compute() }()
select {
case v := <-res:
	return v, nil
case <-time.After(budget):
	return zero, ErrTimeout // producer still finishes into the buffer, then exits
}
```

This single-slot handoff is the difference between a timeout that bounds your
handler and a timeout that bounds your handler *while quietly leaking a goroutine
per breach*. Every "run it in a goroutine and race it against a timer" pattern in
this lesson uses a buffered result channel for exactly this reason.

## Sentinels, not strings

A budget breach is a first-class outcome that callers branch on, so represent it
as a sentinel error, `var ErrTimeout = errors.New("...")`, and compare with
`errors.Is`. Never match the error string: the message is a presentation detail
that changes, while the sentinel's identity is the contract. Wrap it with `%w`
when you add context (`fmt.Errorf("fetch user: %w", ErrTimeout)`) so `errors.Is`
still finds it through the wrapping.

## An overall deadline is a resource to spend

When one request carries a single deadline and runs several sequential sub-steps,
the deadline is a budget you divide among them. Compute each step's slice from
`time.Until(deadline)` — the remaining wall-clock — so a slow early step cannot
starve the later ones, and so the total stays bounded no matter how many steps
run. A step given a near-zero remaining budget times out immediately, which is
correct: the request already spent its time. This is the arithmetic
`context.WithDeadline` formalizes; doing it by hand once makes the context version
obvious.

## Timer versus Ticker

A `Timer` fires once: use it for an idle timeout, a one-shot deadline, a single
backoff sleep. A `Ticker` fires repeatedly on a fixed cadence: use it for a batch
flush interval or a heartbeat sample. Both hold a runtime entry and must be
stopped when done — `defer t.Stop()` at allocation. A `Ticker` in particular
keeps firing and leaks if you never stop it, because unlike a one-shot timer it
never becomes garbage on its own while it is still scheduled to tick. When you
need a repeating action that you also reset on external events (a heartbeat that
resets the deadline on each beat), a single `Timer` you `Reset` is usually
clearer than a `Ticker`, because you control exactly when the next fire is armed.

## Common Mistakes

### Confusing per-message and overall timeouts

Wrong: placing `time.After` outside the loop and expecting each received message
to get a fresh budget. Outside the loop it is evaluated once, so it becomes a
single overall deadline that fires one time. Conversely, resetting a timer inside
the loop when you wanted a hard total cap lets the operation run unbounded as long
as values keep arriving.

Fix: `time.After` (or a `Reset` timer) *inside* the loop for a per-message
budget; one timer created *outside* the loop for an overall deadline. Name the
question before placing the timer.

### Leaking the timed-out producer on an unbuffered channel

Wrong: `res := make(chan T)` (unbuffered), spawn a producer that sends on it,
then abandon it when the timeout wins. The producer blocks forever on its send
and leaks.

Fix: `res := make(chan T, 1)`. The buffer lets the loser of the race complete its
send and return.

### Allocating time.After in a hot loop

Wrong: `for { select { case v := <-ch: ...; case <-time.After(d): return } }`
allocates a fresh timer every iteration.

Fix: one `time.NewTimer` before the loop, `Reset` it each iteration; or a
`time.Ticker` when the cadence is fixed.

### Forgetting to Stop a Timer or Ticker

Wrong: allocating a timer or ticker and never stopping it. The runtime entry is
held (and a ticker keeps firing) until it would have fired.

Fix: `defer t.Stop()` immediately after allocation.

### Carrying the wrong drain assumption across Go versions

Wrong: assuming Go 1.23 auto-drain semantics while the module's `go.mod` still
names an older version — a stale buffered tick fires `Reset` immediately and the
timeout collapses to zero. Equally wrong is a blind blocking `<-t.C` drain after
`Stop` on a modern build, which can deadlock because the value was already
consumed and `Stop` does not close `C`.

Fix: know your `go.mod` version. Use the non-blocking Stop-drain-Reset form; it is
correct under both regimes.

### Comparing timeout errors by string

Wrong: `if err.Error() == "operation timed out"`. The message is a UI concern and
changes.

Fix: `var ErrTimeout = errors.New(...)` and `errors.Is(err, ErrTimeout)`, wrapping
with `%w` when adding context.

### Assuming select picks the timer case deterministically

Wrong: relying on the data case always winning (or the timer always winning) when
both are ready. `select` chooses uniformly at random among ready cases.

Fix: design so correctness never depends on which of two simultaneously-ready
cases wins.

Next: [01-timeout-primitives.md](01-timeout-primitives.md)
