# Panic and Recover: Boundaries, Supervisors, and Failure Classification — Concepts

`panic`/`recover` is not error handling. It is the mechanism for crossing a
process-integrity boundary: the point where the program says "the assumptions I
was executing under no longer hold, unwind me to a place that can decide what
happens next." A senior backend engineer is not judged on knowing the syntax —
`defer func(){ recover() }()` fits on one line — but on knowing exactly three
things cold: where a recover boundary legitimately belongs, why a deferred
recover cannot catch a panic from a goroutine you launched, and how to classify
what you caught so that a real bug is not silently downgraded into a "handled"
success. Get any of those wrong and you ship a service that either crashes in
production despite "having recovery" or, worse, keeps running while quietly
returning corrupt results and telling your observability stack nothing happened.

This file is the conceptual spine. Read it once and you have everything needed to
reason through the independent modules that follow — a recovery middleware,
a goroutine supervisor, a streaming-abort sentinel, a value classifier, a batch
consumer, an internal-panic parser, the cross-goroutine trap, selective
re-panic, and panic-during-cleanup.

## The mechanism, precisely

`panic(v)` halts the current goroutine at the call site, then runs that
goroutine's deferred calls in last-in-first-out order as the stack unwinds. If a
deferred call invokes `recover()` and returns normally, unwinding stops at that
frame and the function containing the deferred call returns normally to its
caller. If no deferred `recover` intercepts the panic, unwinding reaches the top
of the goroutine, the runtime prints the panic value and a stack trace to
standard error, and the whole process terminates — every goroutine, not just the
one that panicked. There is no "uncaught panic in one goroutine, keep serving the
rest": an unrecovered panic anywhere is a full crash.

`recover()` returns a non-nil value only when it is called *directly* inside a
function that is itself running as a deferred call during an active panic. Called
anywhere else — at the bottom of a normal function body, or inside a helper that
the deferred function calls rather than in the deferred function itself — it
returns `nil` and does nothing. This is the single most common beginner bug and
it is invisible until a panic actually fires in production: the code "has a
recover" but the recover is one call level too deep, so the panic sails past it.
The recovered value is exactly the argument passed to `panic`, of any type. A
deferred function may re-`panic` a value to keep it propagating after inspecting
it; that is a first-class tool, not a hack.

The subtle guarantee that makes defensive cleanup possible: while a panic is
unwinding and running a deferred function `D`, a *normal* nested call that `D`
makes runs with its own frame. If that nested call defers its own recover and
returns without panicking, its recover sees `nil` — it does not steal the outer
panic. Only if the nested call itself panics does its recover fire, catching that
*inner* panic while the outer one stays paused. This is why a `safeClose` with
its own `recover` can swallow a `Close`-time panic without clobbering the primary
panic that is still in flight above it.

## recover has goroutine scope — internalize this or crash in production

A deferred `recover` can only catch panics unwinding through *its own* goroutine.
A panic in a goroutine you started with `go f()` is completely invisible to the
launcher's deferred recover, because that recover is on a different goroutine's
stack. The child goroutine's panic unwinds to the top of the child, finds no
recover, and terminates the entire process. This one fact is the most common way
a service that "has panic recovery" still crashes: the recovery middleware wraps
the request handler, the handler fires off a `go auditWrite(...)` or a cache-warm
goroutine, that goroutine panics, and the beautifully-built middleware never had a
chance — it is on the request goroutine, not the child. The rule is mechanical:
every goroutine you spawn needs its own recover installed inside it. A supervisor
helper (`SafeGo`, `goSafe`) that wraps the spawned function with a deferred
recover is not optional hygiene; it is the only thing standing between one
worker's bug and a whole-process crash.

## Where a recover boundary belongs — and where it is a bug

There are exactly three places a recover boundary is legitimate:

1. The top of an inbound request — an HTTP or gRPC handler. One bad request must
   not take down the server; recover, log with a stack, return a 500, keep
   serving.
2. The top of every goroutine you spawn — because of goroutine scope above.
3. The exported edge of a package that *deliberately* uses `panic` internally to
   unwind (a recursive-descent parser or validator that bails out of deep
   recursion with a private panic and converts it back to a returned `error` at
   its public boundary).

Everywhere else, recover is a defect. A recover buried in business logic or in a
reusable library swallows the caller's contract violation, hides the bug that
caused it, and returns as if the function succeeded — with whatever partial,
possibly-corrupt state existed at panic time. Recovery is a boundary concern
owned by the *caller* of a stack, never by a library function deep inside it. A
library should let panics propagate so the boundary that owns the process can
decide.

## Classify what you caught — severity is not one bucket

A recovered value is not automatically "an error to log and move on." What you
caught determines what you must do, and there are three distinct severities:

- A **`runtime.Error`** (nil pointer dereference, index/slice out of range, nil
  map write, integer divide by zero, failed type assertion) is a code bug, not a
  handled condition. The runtime generated it because your program did something
  impossible. Recovering one and returning a clean response is how you blind your
  on-call: log it with a full stack, increment a bug metric, and very often
  re-panic so the crash and any crash-reporter still fire. `runtime.Error` is an
  interface — `error` plus a marker `RuntimeError()` method — so you detect it
  with `errors.As(err, &runtimeErr)`, not by comparing concrete types.

- An **application `panic(error)`** is a value you chose to panic with. It can be
  unwrapped (`errors.As`/`errors.Is`) and mapped to a domain response. This is
  the only severity where converting the panic into a normal returned error is
  correct — and even then, only at a legitimate boundary.

- A **`*runtime.PanicNilError`** is what `recover` returns when code called
  `panic(nil)`. Since Go 1.21, `panic(nil)` no longer makes `recover` return
  `nil`; it yields a `*runtime.PanicNilError` (which also satisfies
  `runtime.Error`). This finally makes `if r := recover(); r != nil` behave for
  that case. Treat it as a bug — someone panicked with no value. The legacy
  behavior is available only for migration via `GODEBUG=panicnil=1`.

Idiomatic Go panics with an `error`, and the value you build to carry a recovered
panic across a boundary should implement `Unwrap` so `errors.Is`/`errors.As` keep
working through it. A bare `panic("string")` is hostile: the caller cannot
`errors.As` or `Unwrap` it, only string-match, so panic with an error or a typed
sentinel instead.

## Sentinels the runtime and net/http define for you

`http.ErrAbortHandler` is a sentinel panic value: `panic(http.ErrAbortHandler)`
aborts the response to the client *and* tells `net/http` to suppress the
stack-trace log for that panic. It is how a streaming or SSE handler says "abort
this partially-written response deliberately" — a client disconnect or a
mid-stream giving-up, not a server error. Recovery middleware must special-case
it: detect it with `errors.Is(err, http.ErrAbortHandler)` (never `==`, so a
wrapped abort still matches), and re-panic it or return quietly instead of
emitting a 500 with an error-level stack log. Treating an abort like any other
panic pollutes your error rate and your logs with what was a deliberate,
expected control action.

## Capture the stack at the moment of recovery

`runtime/debug.Stack()` returns the calling goroutine's stack trace as `[]byte`;
`debug.PrintStack()` writes it to standard error. The trace is only meaningful at
the instant of recovery — call `debug.Stack()` as the first thing your deferred
recover does, before any other call runs in that deferred function, because
further calls overwrite the stack context and the recorded trace no longer points
at the panic site. When you re-panic to preserve a crash, capture the stack
*once* before re-raising so the crash-reporter and your own record agree.

## Observability is not optional

A silent recover — `defer func(){ recover() }()` with no log, metric, or report —
is a defect, full stop. The function returns as if it succeeded, carrying
whatever partial state existed when the panic fired, and the operator sees
nothing: no log line, no metric bump, no alert. Every recovery must be
observable. If you recover, you log (or emit a metric, or report to a
crash-reporter), and if the value is not something this boundary should own, you
re-panic so the process crash still happens. Swallowing an unclassified panic is
how an outage becomes a silent data-corruption incident.

## Common Mistakes

### recover at the bottom of the function body, not in a defer

Wrong: calling `recover()` as a statement near the end of a function. Outside a
deferred call it returns `nil` and the panic keeps propagating right past it.

Fix: always call `recover()` inside a `defer func(){ ... }()`. That deferred
function is the only place it does anything.

### recover in a helper the deferred function calls

Wrong: the deferred function calls `handlePanic()`, and `handlePanic` calls
`recover()`. That is one call level too deep; `recover` must be invoked directly
by the deferred function, so it returns `nil` and the panic escapes.

Fix: call `recover()` in the deferred function itself and pass the value down to
a helper, rather than calling `recover()` from the helper.

### assuming a handler's recover protects goroutines it spawns

Wrong: a request handler with a deferred recover fires off `go doAsync()`; when
`doAsync` panics the process crashes, because recover has goroutine scope and the
handler's recover is on a different goroutine.

Fix: wrap every spawned goroutine's body with its own deferred recover (a
`SafeGo`/`goSafe` helper). The child must recover itself.

### silent recover that swallows a contract violation

Wrong: a bare `recover()` with no logging, returning a "successful" but corrupt
result while the operator sees nothing.

Fix: every recovery logs/metrics/reports. If you cannot classify and own the
value, re-panic it.

### recovering inside a reusable library or deep in business logic

Wrong: a helper that `recover`s "to be safe," hiding its caller's bug and
breaking the error contract.

Fix: libraries let panics propagate. Recovery is a boundary concern owned by the
caller, at the three legitimate boundaries only.

### turning http.ErrAbortHandler into a 500

Wrong: recovery middleware treats an `http.ErrAbortHandler` panic like any other,
writing a 500 and logging a stack at error level for what was a deliberate
stream abort.

Fix: detect it with `errors.Is(err, http.ErrAbortHandler)` and re-panic or return
quietly — no 500, at most a debug log.

### losing the primary error to a Close-time panic during unwinding

Wrong: a deferred `Close()` that panics while a primary panic is unwinding
replaces the in-flight panic ("last panic wins") and the original failure is
gone.

Fix: capture the primary value first, run cleanup defensively with its own
recover, and merge both with `errors.Join`.

### relying on `if r := recover(); r != nil` to skip panic(nil)

Wrong: assuming `panic(nil)` slips past `r != nil`. True before Go 1.21; since
1.21 `panic(nil)` yields a `*runtime.PanicNilError` and IS caught.

Fix: expect and classify `*runtime.PanicNilError` as a bug. Only
`GODEBUG=panicnil=1` restores the legacy behavior, and only for migration.

### panicking with a bare string

Wrong: `panic("bad state")`, which callers can only string-match.

Fix: `panic(fmt.Errorf(...))` or a typed sentinel, so a boundary can `errors.As`
and `Unwrap` it.

### writing a 500 after the handler already wrote headers

Wrong: recovering and calling `http.Error` after the handler already called
`WriteHeader`/wrote a body, producing a duplicate-`WriteHeader` warning and a
corrupt response.

Fix: wrap the `ResponseWriter` to track whether a header was written, and skip
the 500 write if it was.

### using panic/recover as ordinary control flow

Wrong: panicking for expected conditions — validation failures, not-found — and
recovering them as normal flow.

Fix: return errors for the expected. Reserve panic for the exceptional and the
impossible; the one legitimate internal use (a parser unwinding deep recursion)
stays entirely behind the package's own boundary.

### capturing debug.Stack() too late

Wrong: calling `debug.Stack()` after other calls have run inside the deferred
function, so the recorded trace no longer points at the panic site.

Fix: capture the stack as the first action in the deferred recover.

Next: [01-recovery-middleware.md](01-recovery-middleware.md)
