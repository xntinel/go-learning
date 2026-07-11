# Panic vs Error: Boundaries, Recovery, and Invariants — Concepts

The choice between `panic` and `error` is not a matter of taste. For a senior
backend engineer it is two decisions at once: an API-contract decision (what does
a correct caller owe me, and what do I owe a correct caller?) and a blast-radius
decision (if this goes wrong, does one request fail, or does the whole process
die and take every in-flight request with it?). Get it wrong and a single
malformed request becomes a full outage; get it right and the same request
produces a clean 400 or 500 while the service keeps serving everyone else. This
file is the conceptual foundation for the nine independent exercises that follow.
Read it once and you have the model you need to reason through each of them.

## Concepts

### The dividing line: expected-but-unwanted vs impossible

`error` is for the expected-but-unwanted: bad user input, a missing file, a
timed-out RPC, a row that is not there. These are outcomes a correct, well-behaved
caller can and will hit at runtime with entirely valid usage. `panic` is for the
impossible: a violated invariant, an unreachable branch, a programmer error the
caller was contractually required to prevent. The single test that decides it:
*could a correct caller trigger this at runtime with valid usage?* If yes, it is
an error, and it must never be a panic. A parser meeting a bad byte is an error.
A parser reaching a `default` case its own exhaustive `switch` proves cannot
happen is a panic, because if it ever fires, the code — not the input — is wrong.

This is why panicking on user input is a design bug. It forces every caller to
wrap every call in `recover`, converts a routine 400 Bad Request into a process
crash, and couples callers to a fragile recovery contract instead of the simple,
composable `if err != nil`.

### A panic unwinds exactly one goroutine — the operational core

This is the single most important operational fact about panic in a concurrent
server, and the one most often gotten wrong. A `panic` unwinds only the stack of
the goroutine that raised it. `recover` catches a panic only while it propagates
up the *same* goroutine's stack. A parent goroutine cannot recover a child's
panic. An HTTP middleware's `recover` protects the handler goroutine — and
nothing it spawns. The instant any goroutine's panic reaches the top of its own
stack unrecovered, the Go runtime terminates the *entire process*. Every other
goroutine, every in-flight request, dies with it.

The consequence for design is absolute: recovery must live at *every goroutine
entry point*, not only at the request boundary. If your handler does
`go doWork()` and `doWork` panics, your beautiful recovery middleware never sees
it and the server crashes. Every `go` statement whose body can panic needs its
own `defer`/`recover`, or must be routed through a helper that supplies one.

### recover only works directly inside a deferred function

`recover()` returns the value passed to `panic` and stops the unwinding — but
*only* when called directly inside a deferred function that is running because of
the panic. Call `recover` on the normal path (not during unwinding) and it
returns `nil` and does nothing. Call it from a helper function that your deferred
function calls, rather than in the deferred function itself, and it also returns
`nil`: the spec requires the call to be direct. The canonical, and essentially
only correct, shape is:

```go
func guarded() (err error) {
	defer func() {
		if r := recover(); r != nil {
			err = fmt.Errorf("recovered: %v", r)
		}
	}()
	// ... work that may panic ...
	return nil
}
```

Two mechanics make this work. First, deferred functions are the *only* code that
runs during a panic unwind, so the `defer` is what gives `recover` a place to
live. Second, a deferred closure can read and rewrite the enclosing function's
*named* return values — here `err` — which is exactly how "recover to error"
turns a panic into a returned error. Without a named return there is no variable
for the deferred closure to assign, and the recovery cannot surface a value.
Deferred functions also run in LIFO order, which matters when a guard and a
cleanup are both deferred.

### Recover at a boundary, never mid-stack

Legitimate recovery boundaries are few and structural: an HTTP or gRPC handler
wrapper, a goroutine or worker entry point, a job runner, a plugin/dispatch
trampoline. What they share is that they sit at the edge of a unit of work whose
failure is isolatable — one request, one job, one plugin call. Recovering deep in
business logic is the opposite: it swallows the fault where you have no idea what
invariants were half-mutated before the panic, and lets the program stumble
onward in an undefined state. Recover at the seam where you can cleanly abandon
the whole unit of work and report it; never in the middle of the work itself.

### Deliberate panics vs involuntary runtime faults

Not all recovered values are equal, and a mature boundary treats them
differently. A *deliberate* panic is a value you chose to panic with — a sentinel
you defined, or `http.ErrAbortHandler`. An *involuntary* runtime fault is the
runtime raising a `runtime.Error`: a nil pointer dereference, an index or slice
bound violation, a write to a nil map, a failed type assertion, an integer
divide-by-zero. The `runtime.Error` interface (it embeds `error` and adds a
`RuntimeError()` method) is precisely the marker for "this was a genuine bug, not
a condition anyone intended". A boundary may legitimately convert a deliberate
panic into an error and continue. It should *not* silently do the same to a
`runtime.Error`: that fault means the code is broken and probably left state
corrupted, so swallowing it hides the defect and risks serving wrong data. The
mature policy is to detect `runtime.Error` via `errors.As`, log it with its
stack, and re-panic (or crash) — fail loud on real bugs, degrade gracefully only
on conditions you anticipated.

### The Must convention is fail-fast-at-init

`regexp.MustCompile`, `template.Must`, and a generic `Must[T any](v T, err error) T`
all encode one idea: the input is a compile-time constant the developer controls,
so a failure means the *program is misbuilt* and must never start serving. Panic
here is correct because there is no valid caller who could hit it at runtime — the
only way to fail is to ship a broken constant, and crashing at package-init is
strictly better than starting up and serving broken responses. The same helper is
*wrong* the instant its input is dynamic. `Must(regexp.Compile(userPattern))`
turns a routine bad request into a process crash. Must is for trusted, constant,
init-time and test-setup input only; anything touching runtime or user data uses
the error-returning variant.

### defer is the only invariant-preserving path through a panic

Because deferred functions are the only code that runs during unwinding, every
piece of cleanup that must survive a panic has to be registered with `defer`:
database transaction rollback, `mutex.Unlock`, file and connection `Close`, tracing
span `End`. The classic disaster is a non-deferred unlock — `mu.Lock()` then work
then `mu.Unlock()` on the normal path only. A panic between the two skips the
unlock and permanently *poisons the mutex*: it is locked forever, and every future
goroutine that tries to acquire it blocks eternally. One panic in a critical
section becomes a total deadlock. `defer mu.Unlock()` immediately after `mu.Lock()`
makes that impossible. The same logic makes `defer tx.Rollback()`-style guards the
only correct way to keep a transaction from leaking a held connection when the body
panics.

### Re-panic preserves fail-fast while still running cleanup

The transaction guard needs two things that look contradictory: run the rollback
(so no connection leaks), and still crash on a genuine bug (so it is not silently
swallowed). Both are achievable because `defer` runs during the unwind. The
pattern is: in the deferred function, detect that a panic is in flight
(`recover()` returns non-nil), perform the cleanup (rollback), then `panic(r)`
again to resume the unwinding. The rollback happens; the panic still propagates to
the process boundary. This is the correct shape for any guard that must both
restore an invariant and preserve fail-fast semantics on a real fault.

### Capture the stack at the moment of recovery

A recovered panic where you logged only `fmt.Sprintf("%v", r)` has thrown away the
one thing an on-call engineer needs: *where* it happened. `runtime/debug.Stack()`
returns `[]byte`, the formatted stack trace of the *calling* goroutine, and it
must be called at the recovery site, inside the deferred function — once the stack
unwinds past the boundary the frames are gone. Capture it there and attach both
the recovered value and the stack to a structured error type (an `error` that
carries `Value any` and `Stack []byte`, implements `Unwrap` when the panic value
is itself an error, and is discoverable with `errors.As`). That turns a lost trace
into a first-class, wrappable, observable error your logging and error-tracking
pipeline can consume.

### http.ErrAbortHandler is a special sentinel panic

`http.ErrAbortHandler` is a sentinel `error` value the `net/http` server
recognizes: a handler may `panic(http.ErrAbortHandler)` to abort the response, and
the server aborts it *without* logging the usual panic stack trace. Client
disconnects and `httputil.ReverseProxy` use it. A recovery middleware that treats
every recovered value identically will therefore log a full 500-with-stack for
every routine client abort, flooding your error logs with noise that is not a bug.
A correct middleware special-cases it: on `errors.Is(r-as-error, http.ErrAbortHandler)`
it re-panics (or simply does not log a stack and does not force a 500 body),
preserving the server's intended quiet-abort behavior.

### Never use panic for control flow

Panic must not signal "done", "skip", or "not found". It is far slower than a
return, defeats the static reasoning that `if err != nil` gives readers and tools,
and couples callers to a fragile recover contract. Return a value or a sentinel
error. The only time panic-as-signal is defensible is a deeply recursive walk that
unwinds to a single well-defined boundary in the same package (the standard
library does this in a few parsers), and even there it is an internal
implementation detail, never part of the exported contract.

## Common Mistakes

### Panicking on expected failures

Wrong: a function that panics on invalid user input or a missing record, forcing
every caller to wrap the call in `recover`. Fix: return an error and let
`errors.Is`/`errors.As` classify it. The `Parse` in Exercise 1 does exactly this.

### Assuming a parent's recover protects a spawned goroutine

Wrong: believing an HTTP middleware's `recover`, or a parent goroutine's `recover`,
catches a panic in a goroutine it spawned. It does not — the child panic unwinds
its own stack and, unrecovered, crashes the whole process. Fix: wrap every
goroutine body in its own `defer`/`recover`, or route it through a helper that
does (Exercise 3).

### Calling recover outside a deferred function

Wrong: calling `recover()` on the normal path, or inside a helper the deferred
function calls, and expecting it to catch the panic. It returns `nil`. Fix: call
`recover` directly in the deferred closure.

### Recovering runtime faults indiscriminately

Wrong: recovering everything — including nil dereferences and index-out-of-range —
and continuing as if nothing happened, converting a hard bug into silent data
corruption. Fix: detect `runtime.Error` via `errors.As` and re-panic (or crash)
after logging (Exercise 5).

### Losing the stack trace

Wrong: logging only the recovered value with `fmt.Sprintf("%v", r)`. Fix: call
`runtime/debug.Stack()` at the recovery site and attach it to a structured error
(Exercise 6).

### Non-deferred cleanup

Wrong: calling `tx.Rollback()` / `mu.Unlock()` / `f.Close()` on the normal path
only, so a panic skips it and leaks the transaction or poisons the mutex. Fix:
`defer` the cleanup immediately after acquisition (Exercises 7 and 8).

### Must on runtime or user input

Wrong: using a Must-style constructor on dynamic input, turning a routine bad
request into a crash. Fix: Must is only for constant, trusted, init-time input;
use the error-returning variant for anything dynamic (Exercise 4).

### Swallowing a recovered panic and reporting success

Wrong: recovering a panic and returning a nil error (or an empty result), so the
caller believes the operation succeeded. Fix: convert the recovered value into a
non-nil error and propagate it.

### Not special-casing http.ErrAbortHandler

Wrong: a recovery middleware that logs every recovered value as a 500 with a
stack, so legitimate client-abort panics flood the logs. Fix: special-case
`http.ErrAbortHandler` (Exercise 2).

### Writing a 500 after a partial body was already sent

Wrong: writing the 500 status/body from the recovery middleware after the handler
already wrote a partial response, producing a corrupt or duplicated reply. Fix:
track whether the handler already wrote a header/body and only synthesize the 500
if nothing has been written yet (Exercise 2).

Next: [01-kv-config-parser-error-vs-must.md](01-kv-config-parser-error-vs-must.md)
